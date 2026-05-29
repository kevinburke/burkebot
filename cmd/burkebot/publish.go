package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// publishForm carries the operator-supplied PR title/body and draft
// flag from the audit-page "Open/Update PR" form.
type publishForm struct {
	Title string
	Body  string
	Draft bool
}

// publishDecision is the JSON body burkebot-publish reads off disk.
// Mirrors the fields documented in burkebot-publish.sh.j2's usage.
type publishDecision struct {
	Outcome           string `json:"outcome"`
	BranchName        string `json:"branch_name"`
	BaseSHA           string `json:"base_sha,omitempty"`
	ExpectedRemoteSHA string `json:"expected_remote_sha,omitempty"`
	PRTitle           string `json:"pr_title,omitempty"`
	PRBody            string `json:"pr_body,omitempty"`
	PRNumber          int    `json:"pr_number,omitempty"`
	Draft             bool   `json:"draft,omitempty"`
}

// publishResult is what executePublish returns on success.
type publishResult struct {
	PRNumber   int
	Output     string
	LastPushed string
}

var publishPRNumberRE = regexp.MustCompile(`(?m)^PR_NUMBER=(\d+)\s*$`)

// executePublish builds a decision JSON from the run's summary and the
// operator's form, invokes /usr/local/bin/burkebot-publish (which
// holds GH_TOKEN; the dashboard never does), parses the PR number it
// prints, and patches summary.json with the new PRNumber +
// LastPushedSHA.
//
// Force-with-lease is used whenever the summary already records a
// LastPushedSHA — i.e., this isn't the first push of the branch.
func executePublish(logger *slog.Logger, cfg promptRunnerConfig, proj Project, summary *Summary, form publishForm) (publishResult, error) {
	if summary == nil {
		return publishResult{}, errors.New("summary missing; cannot publish")
	}
	if cfg.PublishPath == "" {
		return publishResult{}, errors.New("PublishPath not configured (pass --publish-path to the dashboard)")
	}
	if info, err := os.Stat(cfg.PublishPath); err != nil || info.IsDir() {
		return publishResult{}, fmt.Errorf("publish helper %q is not available", cfg.PublishPath)
	}
	if summary.BranchName == "" {
		return publishResult{}, errors.New("summary has no branch_name; was this run an ad-hoc dashboard run?")
	}
	if summary.JobRepoDir == "" {
		return publishResult{}, errors.New("summary has no job_repo_dir; cannot find run's repo checkout")
	}
	if info, err := os.Stat(summary.JobRepoDir); err != nil || !info.IsDir() {
		return publishResult{}, fmt.Errorf("job repo dir %q is no longer available (job dirs may have been cleaned up)", summary.JobRepoDir)
	}
	if summary.HeadCommitSHA == "" {
		return publishResult{}, errors.New("summary has no head_commit_sha; nothing to push")
	}
	if summary.BaseCommitSHA != "" && summary.HeadCommitSHA == summary.BaseCommitSHA {
		return publishResult{}, errors.New("agent made no commits on top of the base commit; nothing to push")
	}
	repo := summary.PRRepo
	if repo == "" {
		repo = proj.Repo
	}
	if repo == "" {
		return publishResult{}, errors.New("no PR repo slug recorded (set repo: in projects.json)")
	}

	decision := publishDecision{
		Outcome:           "push_branch",
		BranchName:        summary.BranchName,
		BaseSHA:           summary.BaseCommitSHA,
		ExpectedRemoteSHA: summary.LastPushedSHA,
		PRTitle:           strings.TrimSpace(form.Title),
		PRBody:            form.Body,
		PRNumber:          summary.PRNumber,
		Draft:             form.Draft,
	}
	decisionBytes, err := json.MarshalIndent(decision, "", "  ")
	if err != nil {
		return publishResult{}, fmt.Errorf("encoding decision: %w", err)
	}

	// Decision file goes in the audit dir so it's archived alongside
	// the run for forensics. burkebot-publish reads it as the
	// burkebot user; the audit dir is group-readable by burkebot.
	auditDir := filepath.Join(proj.AuditDir, summary.RunID)
	decisionPath := filepath.Join(auditDir, "publish-decision.json")
	if err := os.WriteFile(decisionPath, decisionBytes, 0o640); err != nil {
		return publishResult{}, fmt.Errorf("writing decision file: %w", err)
	}

	args := []string{
		"--repo", repo,
		"--decision-file", decisionPath,
		"--job-repo", summary.JobRepoDir,
		"--open-pr",
	}
	if summary.LastPushedSHA != "" {
		args = append(args, "--force-with-lease")
	}

	cmd := exec.Command(cfg.PublishPath, args...)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	runErr := cmd.Run()
	out := output.String()

	// Always archive the output for diagnostics.
	logPath := filepath.Join(auditDir, "publish.log")
	if writeErr := os.WriteFile(logPath, []byte(out), 0o640); writeErr != nil {
		logger.Warn("could not write publish.log", "error", writeErr, "path", logPath)
	}

	if runErr != nil {
		logger.Error("publish failed", "error", runErr, "repo", repo, "branch", summary.BranchName, "output", out)
		return publishResult{Output: out}, fmt.Errorf("burkebot-publish failed: %w", runErr)
	}

	prNumber := 0
	if match := publishPRNumberRE.FindStringSubmatch(out); len(match) == 2 {
		if n, err := strconv.Atoi(match[1]); err == nil {
			prNumber = n
		}
	}

	patch := summaryGitState{
		LastPushedSHA: summary.HeadCommitSHA,
		PRNumber:      prNumber,
	}
	if err := patchSummaryGitState(filepath.Join(auditDir, "summary.json"), patch); err != nil {
		logger.Warn("could not patch summary.json with publish state", "error", err, "audit_dir", auditDir)
	}

	return publishResult{PRNumber: prNumber, Output: out, LastPushed: summary.HeadCommitSHA}, nil
}

// autoPublish runs executePublish with operator-form-empty defaults
// after a successful run that opted in to "Open PR when done". Logs
// failure rather than returning it — the operator will see the
// outcome on the audit page.
func (s *Server) autoPublish(proj Project, res runResult) {
	bundle, err := loadBundle(proj.AuditDir, res.RunID)
	if err != nil || bundle == nil || bundle.Summary == nil {
		s.logger.Warn("auto-publish skipped: summary not loadable", "error", err, "run_id", res.RunID)
		return
	}
	publisher := s.runPublish
	if publisher == nil {
		publisher = executePublish
	}
	form := publishForm{Draft: true}
	if _, err := publisher(s.logger, s.prompt, proj, bundle.Summary, form); err != nil {
		s.logger.Error("auto-publish failed", "error", err, "run_id", res.RunID)
	}
}

// handlePublish is the POST handler for the audit-page "Open / Update PR"
// button.
func (s *Server) handlePublish(w http.ResponseWriter, r *http.Request, proj *Project, runID string) {
	if s.prompt.PublishPath == "" {
		http.NotFound(w, r)
		return
	}
	if !isValidRunID(runID) {
		http.NotFound(w, r)
		return
	}
	publishPath := "/projects/" + proj.Name + "/audit/" + runID + "/publish"
	if !s.verifyCSRF(w, r, publishPath) {
		return
	}

	bundle, err := loadBundle(proj.AuditDir, runID)
	if err != nil || bundle == nil || bundle.Summary == nil {
		redirectAuditMessage(w, r, proj, runID, "Audit bundle not found or summary missing", true)
		return
	}

	form := publishForm{
		Title: r.FormValue("pr_title"),
		Body:  r.FormValue("pr_body"),
		Draft: r.FormValue("draft") != "",
	}

	publisher := s.runPublish
	if publisher == nil {
		publisher = executePublish
	}
	result, err := publisher(s.logger, s.prompt, *proj, bundle.Summary, form)
	if err != nil {
		redirectAuditMessage(w, r, proj, runID, "Publish failed: "+err.Error(), true)
		return
	}

	msg := fmt.Sprintf("Pushed branch %s", bundle.Summary.BranchName)
	if result.PRNumber > 0 {
		msg = fmt.Sprintf("PR #%d updated", result.PRNumber)
		if bundle.Summary.PRNumber == 0 {
			msg = fmt.Sprintf("PR #%d opened", result.PRNumber)
		}
	}
	redirectAuditMessage(w, r, proj, runID, msg, false)
}
