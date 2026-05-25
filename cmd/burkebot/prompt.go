package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

type promptRunnerConfig struct {
	Enabled      bool
	RepoRoot     string
	RunnerPath   string
	EnvdirBinary string
	EnvDir       string
	BotHome      string

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
	RepoWrite         bool
	WorkspaceWrite    bool
	GitHubCredentials bool
	Dangerous         bool
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
	if p.GitHubCredentials {
		parts = append(parts, "github")
	}
	if p.Dangerous {
		parts = append(parts, "dangerous")
	} else {
		parts = append(parts, "safe")
	}
	return strings.Join(parts, "-")
}

func resolvePromptPolicy(repoWrite, workspaceWrite, githubCredentials, dangerous bool) (promptPolicy, error) {
	if workspaceWrite && !repoWrite {
		return promptPolicy{}, errors.New("workspace writes require repo edits")
	}
	return promptPolicy{
		RepoWrite:         repoWrite,
		WorkspaceWrite:    workspaceWrite,
		GitHubCredentials: githubCredentials,
		Dangerous:         dangerous,
	}, nil
}

func buildPromptReadWritePaths(cfg promptRunnerConfig, proj Project, repoDir string, policy promptPolicy) []string {
	paths := []string{proj.AuditDir, cfg.BotHome}
	if policy.WorkspaceWrite {
		if cfg.RepoRoot != "" {
			paths = append(paths, cfg.RepoRoot)
		}
		if proj.StateFile != "" {
			paths = append(paths, filepath.Dir(proj.StateFile))
		}
	} else if policy.RepoWrite {
		paths = append(paths, repoDir)
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

// executePromptRun is the dashboard prompt UI's entry point. It validates
// inputs, builds a runnerInvocation describing the sandbox + runner argv
// for an ad-hoc submission (envdir-wrapped if GitHub creds are enabled,
// `--safe` unless the user opted out), and hands it to runRunner.
func executePromptRun(logger *slog.Logger, cfg promptRunnerConfig, proj Project, policy promptPolicy, prompt string) (runResult, error) {
	repoDir := proj.RepoDirectory(cfg.RepoRoot)
	if repoDir == "" {
		return runResult{}, errors.New("project has no repo directory")
	}
	if info, err := os.Stat(repoDir); err != nil || !info.IsDir() {
		return runResult{}, fmt.Errorf("repo directory %q is not available", repoDir)
	}
	if info, err := os.Stat(cfg.RunnerPath); err != nil || info.IsDir() {
		return runResult{}, fmt.Errorf("runner binary %q is not available", cfg.RunnerPath)
	}
	if policy.GitHubCredentials {
		if info, err := os.Stat(cfg.EnvdirBinary); err != nil || info.IsDir() {
			return runResult{}, fmt.Errorf("envdir binary %q is not available", cfg.EnvdirBinary)
		}
		if info, err := os.Stat(cfg.EnvDir); err != nil || !info.IsDir() {
			return runResult{}, fmt.Errorf("envdir directory %q is not available", cfg.EnvDir)
		}
	}

	runLabel := fmt.Sprintf("%s-%s", proj.Name, policy.labelSuffix())
	promptID := fmt.Sprintf("%s-%d", proj.Name, len(prompt))

	command := []string{}
	if policy.GitHubCredentials {
		command = append(command, cfg.EnvdirBinary, cfg.EnvDir)
	}
	command = append(command,
		cfg.RunnerPath,
		"--source", "adhoc",
		"--label", runLabel,
		"--prompt-id", promptID,
		"--repo-dir", repoDir,
	)
	if !policy.Dangerous {
		command = append(command, "--safe")
	}

	res, err := runRunner(logger, runnerInvocation{
		WorkingDirectory: repoDir,
		ReadWritePaths:   buildPromptReadWritePaths(cfg, proj, repoDir, policy),
		Command:          command,
		Prompt:           prompt,
	})
	if err != nil {
		return res, fmt.Errorf("prompt run failed: %w", err)
	}
	return res, nil
}

// executeFollowupRun resumes a previous Codex session with a follow-up
// prompt. sessionsDir is the codex-sessions directory from the original
// audit bundle. The runner is invoked with --resume-session pointing at
// the exported session files, and --job-dir for a clean environment.
func executeFollowupRun(logger *slog.Logger, cfg promptRunnerConfig, proj Project, policy promptPolicy, sessionsDir, prompt string) (runResult, error) {
	repoDir := proj.RepoDirectory(cfg.RepoRoot)
	if repoDir == "" {
		return runResult{}, errors.New("project has no repo directory")
	}
	if info, err := os.Stat(repoDir); err != nil || !info.IsDir() {
		return runResult{}, fmt.Errorf("repo directory %q is not available", repoDir)
	}
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

	command := []string{}
	if policy.GitHubCredentials {
		command = append(command, cfg.EnvdirBinary, cfg.EnvDir)
	}
	command = append(command,
		cfg.RunnerPath,
		"--source", "followup",
		"--label", runLabel,
		"--prompt-id", promptID,
		"--repo-dir", jobRepoDir,
		"--job-dir", jobDir,
		"--resume-session", sessionsDir,
	)
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
	if !s.prompt.Enabled {
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
		r.FormValue("github_credentials") != "",
		r.FormValue("dangerous") != "",
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
		r.FormValue("github_credentials") != "",
		r.FormValue("dangerous") != "",
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
		http.Redirect(w, r, "/projects/"+proj.Name+"/audit/"+result.RunID+"/", http.StatusSeeOther)
		return
	}
	if err != nil {
		redirectProjectMessage(w, r, proj, err.Error(), true)
		return
	}
	redirectProjectMessage(w, r, proj, "Prompt submitted", false)
}
