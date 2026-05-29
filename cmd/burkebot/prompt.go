package main

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type promptRunnerConfig struct {
	Enabled    bool
	RepoRoot   string
	RunnerPath string

	// MirrorFetchPath points at /usr/local/bin/burkebot-mirror-fetch,
	// the shared helper that ensures a bare mirror exists under
	// /srv/burkebot/<repo>.git and is up to date. Used by both the
	// ad-hoc and follow-up paths to seed per-run job dirs.
	MirrorFetchPath string

	// PublishPath points at /usr/local/bin/burkebot-publish, the
	// orchestration script that holds GH_TOKEN and does git push +
	// gh pr create/edit. The dashboard never holds GH_TOKEN itself.
	PublishPath string

	BotHome string

	// BotUser/BotGroup name the unprivileged user the runner script
	// drops to via `runuser`. The dashboard chowns per-task-API job
	// dirs to this identity so the runner can write inside them.
	BotUser  string
	BotGroup string

	// JobsDir is the parent directory for per-task-API job
	// directories. Must NOT be under /home or /srv/burkebot — both
	// are denyRead'd inside the srt sandbox the runner wraps codex
	// in, and that deny wins over the per-job allowWrite entries.
	// The canonical layout puts this at /srv/burkebot-jobs (sibling
	// of /srv/burkebot), which is the same place burkebot-run uses
	// for its dependabot-PR jobs.
	JobsDir string

	// CodexAuthDir holds the persistent codex device-auth token
	// (auth.json). The runner script seeds each per-job CODEX_HOME
	// from here and copies the refreshed auth.json back after the
	// run. The dashboard adds it to the task-runner systemd unit's
	// ReadWritePaths so the copy-back succeeds.
	CodexAuthDir string
}

type promptPolicy struct {
	RepoWrite      bool
	WorkspaceWrite bool
	Dangerous      bool

	// OpenPR, if true, auto-invokes burkebot-publish after a
	// successful run to push the agent's commits to a new branch
	// and open a PR. Defaults to false — most ad-hoc runs are
	// exploratory and should not ship a PR by accident.
	OpenPR bool
}

func (p promptPolicy) labelSuffix() string {
	parts := []string{"web"}
	if p.RepoWrite {
		parts = append(parts, "repo-write")
	} else {
		parts = append(parts, "read-only")
	}
	if p.WorkspaceWrite {
		parts = append(parts, "workspace")
	}
	if p.Dangerous {
		parts = append(parts, "dangerous")
	} else {
		parts = append(parts, "safe")
	}
	if p.OpenPR {
		parts = append(parts, "openpr")
	}
	return strings.Join(parts, "-")
}

func resolvePromptPolicy(repoWrite, workspaceWrite, dangerous, openPR bool) (promptPolicy, error) {
	if workspaceWrite && !repoWrite {
		return promptPolicy{}, errors.New("workspace writes require repo edits")
	}
	if openPR && !repoWrite {
		return promptPolicy{}, errors.New("opening a PR requires repo edits")
	}
	return promptPolicy{
		RepoWrite:      repoWrite,
		WorkspaceWrite: workspaceWrite,
		Dangerous:      dangerous,
		OpenPR:         openPR,
	}, nil
}

// buildPromptReadWritePaths returns the absolute paths the runner's
// systemd unit may write to.
//
// For per-run job dirs we always grant write access to the job dir
// itself (which contains the repo checkout, HOME, TMP, and cache);
// workspace-write extends that to the project's state dir.
func buildPromptReadWritePaths(cfg promptRunnerConfig, proj Project, jobDir string, policy promptPolicy) []string {
	paths := []string{proj.AuditDir, cfg.BotHome, jobDir, cfg.CodexAuthDir}
	if policy.WorkspaceWrite {
		if proj.StateFile != "" {
			paths = append(paths, filepath.Dir(proj.StateFile))
		}
	}
	return uniqueNonEmptyPaths(paths)
}

func uniqueNonEmptyPaths(paths []string) []string {
	seen := make(map[string]struct{}, len(paths))
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		out = append(out, path)
	}
	return out
}

// mirrorInfo holds the metadata burkebot-mirror-fetch prints on stdout.
type mirrorInfo struct {
	MirrorPath    string
	RemoteURL     string
	DefaultBranch string
	OnGitServer   bool
}

// fetchMirror invokes /usr/local/bin/burkebot-mirror-fetch and parses
// its KEY=value output.
func fetchMirror(logger *slog.Logger, helperPath, repo string) (mirrorInfo, error) {
	cmd := exec.Command(helperPath, "--repo", repo)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		logger.Error("mirror-fetch failed", "error", err, "repo", repo, "stderr", stderr.String())
		return mirrorInfo{}, fmt.Errorf("mirror-fetch: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}
	info := mirrorInfo{}
	for line := range strings.SplitSeq(stdout.String(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch key {
		case "MIRROR_PATH":
			info.MirrorPath = val
		case "REMOTE_URL":
			info.RemoteURL = val
		case "DEFAULT_BRANCH":
			info.DefaultBranch = val
		case "ON_GIT_SERVER":
			info.OnGitServer = val == "1"
		}
	}
	if info.MirrorPath == "" || info.RemoteURL == "" || info.DefaultBranch == "" {
		return mirrorInfo{}, fmt.Errorf("mirror-fetch returned incomplete metadata: %q", stdout.String())
	}
	return info, nil
}

// branchNameForRun returns the canonical branch name for a run chain
// rooted at branchToken. Stable across follow-ups (PR 3) so they push
// to the same branch.
func branchNameForRun(projectName, branchToken string) string {
	return fmt.Sprintf("burkebot/%s-%s", projectName, branchToken)
}

// gitRevParseHead runs `git -C dir rev-parse HEAD` as the bot user.
// Returns the commit SHA on success.
func gitRevParseHead(logger *slog.Logger, botUser, dir string) (string, error) {
	args := []string{"-u", botUser, "--", "git", "-C", dir, "rev-parse", "HEAD"}
	cmd := exec.Command("runuser", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		logger.Error("git rev-parse HEAD failed", "error", err, "dir", dir, "stderr", stderr.String())
		return "", fmt.Errorf("git rev-parse HEAD: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// prepareAdhocJobRepo seeds an empty per-run repo dir with a clone of
// the project's local bare mirror, fetches the latest origin, resets
// to it, and creates a fresh branch the agent will commit on.
//
// Returns the base SHA the run starts from.
func prepareAdhocJobRepo(logger *slog.Logger, cfg promptRunnerConfig, repo, jobRepoDir, branchName string) (mirrorInfo, string, error) {
	info, err := fetchMirror(logger, cfg.MirrorFetchPath, repo)
	if err != nil {
		return mirrorInfo{}, "", err
	}

	// makeJobDir already ran `git init` in jobRepoDir; replace it with
	// the mirror clone so origin points at a real upstream.
	if err := os.RemoveAll(jobRepoDir); err != nil {
		return mirrorInfo{}, "", fmt.Errorf("clearing jobRepoDir: %w", err)
	}
	runuser := func(args ...string) error {
		fullArgs := append([]string{"-u", cfg.BotUser, "--"}, args...)
		cmd := exec.Command("runuser", fullArgs...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
		}
		return nil
	}

	if err := runuser("git", "clone", "--no-local", info.MirrorPath, jobRepoDir); err != nil {
		return mirrorInfo{}, "", err
	}
	if err := runuser("git", "-C", jobRepoDir, "remote", "set-url", "origin", info.RemoteURL); err != nil {
		return mirrorInfo{}, "", err
	}
	if err := runuser("git", "-C", jobRepoDir, "fetch", "origin", info.DefaultBranch); err != nil {
		return mirrorInfo{}, "", err
	}
	if err := runuser("git", "-C", jobRepoDir, "reset", "--hard", "origin/"+info.DefaultBranch); err != nil {
		return mirrorInfo{}, "", err
	}
	if err := runuser("git", "-C", jobRepoDir, "checkout", "-b", branchName); err != nil {
		return mirrorInfo{}, "", err
	}

	baseSHA, err := gitRevParseHead(logger, cfg.BotUser, jobRepoDir)
	if err != nil {
		return mirrorInfo{}, "", err
	}
	return info, baseSHA, nil
}

// executePromptRun is the dashboard prompt UI's entry point.
//
// It creates a per-run job dir, clones the project's local bare
// mirror into it, checks out a fresh branch, then invokes the
// codex runner against that job dir. The agent never sees GH_TOKEN —
// pushing and PR creation happen later through burkebot-publish.
func executePromptRun(logger *slog.Logger, cfg promptRunnerConfig, proj Project, policy promptPolicy, prompt string) (runResult, error) {
	if proj.Repo == "" {
		return runResult{}, errors.New("project has no upstream repo configured (set repo: in projects.json)")
	}
	if info, err := os.Stat(cfg.RunnerPath); err != nil || info.IsDir() {
		return runResult{}, fmt.Errorf("runner binary %q is not available", cfg.RunnerPath)
	}
	if cfg.MirrorFetchPath == "" {
		return runResult{}, errors.New("MirrorFetchPath not configured (pass --mirror-fetch-path to the dashboard)")
	}
	if info, err := os.Stat(cfg.MirrorFetchPath); err != nil || info.IsDir() {
		return runResult{}, fmt.Errorf("mirror-fetch helper %q is not available", cfg.MirrorFetchPath)
	}
	if cfg.JobsDir == "" {
		return runResult{}, errors.New("JobsDir not configured (pass --jobs-dir to the dashboard)")
	}
	if cfg.CodexAuthDir == "" {
		return runResult{}, errors.New("CodexAuthDir not configured (pass --codex-auth-dir to the dashboard)")
	}

	jobDir, jobRepoDir, err := makeJobDir(cfg.JobsDir, cfg.BotUser, cfg.BotGroup, "adhoc")
	if err != nil {
		return runResult{}, fmt.Errorf("creating job dir: %w", err)
	}

	branchToken, err := randomToken(4)
	if err != nil {
		return runResult{}, fmt.Errorf("generating branch token: %w", err)
	}
	branchName := branchNameForRun(proj.Name, branchToken)

	_, baseSHA, err := prepareAdhocJobRepo(logger, cfg, proj.Repo, jobRepoDir, branchName)
	if err != nil {
		return runResult{}, fmt.Errorf("preparing job repo: %w", err)
	}

	runLabel := fmt.Sprintf("%s-%s", proj.Name, policy.labelSuffix())
	promptID := fmt.Sprintf("%s-%d", proj.Name, len(prompt))

	command := []string{
		cfg.RunnerPath,
		"--source", "adhoc",
		"--label", runLabel,
		"--prompt-id", promptID,
		"--repo-dir", jobRepoDir,
		"--job-dir", jobDir,
	}
	if !policy.Dangerous {
		command = append(command, "--safe")
	}

	res, err := runRunner(logger, runnerInvocation{
		WorkingDirectory: jobDir,
		ReadWritePaths:   buildPromptReadWritePaths(cfg, proj, jobDir, policy),
		Command:          command,
		Prompt:           prompt,
	})
	if err != nil {
		return res, fmt.Errorf("prompt run failed: %w", err)
	}

	if res.RunID == "" || res.AuditDir == "" {
		// Runner produced no audit bundle to patch; nothing to record.
		return res, nil
	}

	headSHA, headErr := gitRevParseHead(logger, cfg.BotUser, jobRepoDir)
	if headErr != nil {
		// Non-fatal: we still want the run to be visible. Log and
		// leave headSHA empty so the publish button stays hidden.
		logger.Warn("could not read HEAD after run", "error", headErr, "job_repo", jobRepoDir)
	}

	patch := summaryGitState{
		JobRepoDir:    jobRepoDir,
		RootRunID:     res.RunID,
		BranchName:    branchName,
		BaseCommitSHA: baseSHA,
		HeadCommitSHA: headSHA,
		PRRepo:        proj.Repo,
	}
	if err := patchSummaryGitState(filepath.Join(res.AuditDir, "summary.json"), patch); err != nil {
		logger.Warn("could not patch summary.json with git state", "error", err, "audit_dir", res.AuditDir)
	}
	return res, nil
}

// executeFollowupRun resumes a previous Codex session with a follow-up
// prompt. sessionsDir is the codex-sessions directory from the original
// audit bundle. The runner is invoked with --resume-session pointing at
// the exported session files, and --job-dir for a clean environment.
//
// PR 2 leaves the follow-up's repo as an empty git-init (same as before
// this change). PR 3 will clone the origin run's job repo so the
// follow-up can iterate on the same branch.
func executeFollowupRun(logger *slog.Logger, cfg promptRunnerConfig, proj Project, policy promptPolicy, sessionsDir, prompt string) (runResult, error) {
	if info, err := os.Stat(cfg.RunnerPath); err != nil || info.IsDir() {
		return runResult{}, fmt.Errorf("runner binary %q is not available", cfg.RunnerPath)
	}
	if info, err := os.Stat(sessionsDir); err != nil || !info.IsDir() {
		return runResult{}, fmt.Errorf("sessions directory %q is not available", sessionsDir)
	}
	if cfg.JobsDir == "" {
		return runResult{}, errors.New("JobsDir not configured (pass --jobs-dir to the dashboard)")
	}
	if cfg.CodexAuthDir == "" {
		return runResult{}, errors.New("CodexAuthDir not configured (pass --codex-auth-dir to the dashboard)")
	}

	jobDir, jobRepoDir, err := makeJobDir(cfg.JobsDir, cfg.BotUser, cfg.BotGroup, "followup")
	if err != nil {
		return runResult{}, fmt.Errorf("creating job dir: %w", err)
	}

	runLabel := fmt.Sprintf("%s-%s-followup", proj.Name, policy.labelSuffix())
	promptID := fmt.Sprintf("%s-followup-%d", proj.Name, len(prompt))

	command := []string{
		cfg.RunnerPath,
		"--source", "followup",
		"--label", runLabel,
		"--prompt-id", promptID,
		"--repo-dir", jobRepoDir,
		"--job-dir", jobDir,
		"--resume-session", sessionsDir,
	}
	if !policy.Dangerous {
		command = append(command, "--safe")
	}

	res, err := runRunner(logger, runnerInvocation{
		WorkingDirectory: jobDir,
		ReadWritePaths:   uniqueNonEmptyPaths([]string{jobDir, proj.AuditDir, cfg.CodexAuthDir}),
		Command:          command,
		Prompt:           prompt,
	})
	if err != nil {
		return res, fmt.Errorf("followup run failed: %w", err)
	}
	return res, nil
}

func redirectProjectMessage(w http.ResponseWriter, r *http.Request, proj *Project, message string, isError bool) {
	values := url.Values{}
	values.Set("message", message)
	if isError {
		values.Set("error", "1")
	}
	target := "/projects/" + proj.Name + "/"
	if encoded := values.Encode(); encoded != "" {
		target += "?" + encoded
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func redirectAuditMessage(w http.ResponseWriter, r *http.Request, proj *Project, runID, message string, isError bool) {
	values := url.Values{}
	values.Set("message", message)
	if isError {
		values.Set("error", "1")
	}
	target := "/projects/" + proj.Name + "/audit/" + runID + "/"
	if encoded := values.Encode(); encoded != "" {
		target += "?" + encoded
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func (s *Server) handleFollowup(w http.ResponseWriter, r *http.Request, proj *Project, runID string) {
	if s.prompt.RunnerPath == "" {
		http.NotFound(w, r)
		return
	}
	if !isValidRunID(runID) {
		http.NotFound(w, r)
		return
	}
	followupPath := "/projects/" + proj.Name + "/audit/" + runID + "/followup"
	if !s.verifyCSRF(w, r, followupPath) {
		return
	}

	sessionsDir := codexSessionsDir(proj.AuditDir, runID)
	if sessionsDir == "" {
		redirectAuditMessage(w, r, proj, runID, "No session files available for follow-up", true)
		return
	}

	promptText := strings.TrimSpace(r.FormValue("prompt"))
	if promptText == "" {
		redirectAuditMessage(w, r, proj, runID, "Follow-up prompt cannot be empty", true)
		return
	}

	policy, err := resolvePromptPolicy(
		r.FormValue("repo_write") != "",
		r.FormValue("workspace_write") != "",
		r.FormValue("dangerous") != "",
		false, // follow-ups don't auto-open PRs in PR 2; PR 3 wires this up
	)
	if err != nil {
		redirectAuditMessage(w, r, proj, runID, err.Error(), true)
		return
	}

	runner := s.runFollowup
	if runner == nil {
		runner = executeFollowupRun
	}

	result, err := runner(s.logger, s.prompt, *proj, policy, sessionsDir, promptText)
	if result.RunID != "" {
		http.Redirect(w, r, "/projects/"+proj.Name+"/audit/"+result.RunID+"/", http.StatusSeeOther)
		return
	}
	if err != nil {
		redirectAuditMessage(w, r, proj, runID, err.Error(), true)
		return
	}
	redirectAuditMessage(w, r, proj, runID, "Follow-up submitted", false)
}

func (s *Server) handlePrompt(w http.ResponseWriter, r *http.Request, proj *Project) {
	if !s.prompt.Enabled {
		http.NotFound(w, r)
		return
	}
	if !s.verifyCSRF(w, r, "/projects/"+proj.Name+"/prompt") {
		return
	}

	promptText := strings.TrimSpace(r.FormValue("prompt"))
	if promptText == "" {
		redirectProjectMessage(w, r, proj, "Prompt cannot be empty", true)
		return
	}

	policy, err := resolvePromptPolicy(
		r.FormValue("repo_write") != "",
		r.FormValue("workspace_write") != "",
		r.FormValue("dangerous") != "",
		r.FormValue("open_pr") != "",
	)
	if err != nil {
		redirectProjectMessage(w, r, proj, err.Error(), true)
		return
	}

	runner := s.runPrompt
	if runner == nil {
		runner = executePromptRun
	}

	result, err := runner(s.logger, s.prompt, *proj, policy, promptText)
	if result.RunID != "" {
		// If the operator opted in to auto-publish, kick that off
		// before redirecting. Failure to publish is recorded on the
		// audit page so the operator can retry via the button.
		if policy.OpenPR && err == nil {
			s.autoPublish(*proj, result)
		}
		http.Redirect(w, r, "/projects/"+proj.Name+"/audit/"+result.RunID+"/", http.StatusSeeOther)
		return
	}
	if err != nil {
		redirectProjectMessage(w, r, proj, err.Error(), true)
		return
	}
	redirectProjectMessage(w, r, proj, "Prompt submitted", false)
}
