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
	"regexp"
	"strings"
)

var auditRunRE = regexp.MustCompile(`(?m)^Audit run: (\S+)$`)

type promptRunnerConfig struct {
	Enabled      bool
	RepoRoot     string
	RunnerPath   string
	EnvdirBinary string
	EnvDir       string
	BotHome      string
}

type promptPolicy struct {
	RepoWrite         bool
	WorkspaceWrite    bool
	GitHubCredentials bool
	Dangerous         bool
}

type promptRunResult struct {
	RunID  string
	Output string
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

func executePromptRun(logger *slog.Logger, cfg promptRunnerConfig, proj Project, policy promptPolicy, prompt string) (promptRunResult, error) {
	repoDir := proj.RepoDirectory(cfg.RepoRoot)
	if repoDir == "" {
		return promptRunResult{}, errors.New("project has no repo directory")
	}
	if info, err := os.Stat(repoDir); err != nil || !info.IsDir() {
		return promptRunResult{}, fmt.Errorf("repo directory %q is not available", repoDir)
	}
	if info, err := os.Stat(cfg.RunnerPath); err != nil || info.IsDir() {
		return promptRunResult{}, fmt.Errorf("runner binary %q is not available", cfg.RunnerPath)
	}
	if policy.GitHubCredentials {
		if info, err := os.Stat(cfg.EnvdirBinary); err != nil || info.IsDir() {
			return promptRunResult{}, fmt.Errorf("envdir binary %q is not available", cfg.EnvdirBinary)
		}
		if info, err := os.Stat(cfg.EnvDir); err != nil || !info.IsDir() {
			return promptRunResult{}, fmt.Errorf("envdir directory %q is not available", cfg.EnvDir)
		}
	}

	runLabel := fmt.Sprintf("%s-%s", proj.Name, policy.labelSuffix())
	promptID := fmt.Sprintf("%s-%d", proj.Name, len(prompt))

	args := []string{
		"--quiet",
		"--wait",
		"--pipe",
		"--collect",
		"--service-type=exec",
		"--property=WorkingDirectory=" + repoDir,
		"--property=NoNewPrivileges=yes",
		"--property=PrivateTmp=yes",
		"--property=ProtectSystem=strict",
		"--property=ProtectHome=read-only",
		"--property=ReadWritePaths=" + strings.Join(buildPromptReadWritePaths(cfg, proj, repoDir, policy), " "),
	}

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

	args = append(args, command...)

	cmd := exec.Command("systemd-run", args...)
	cmd.Stdin = strings.NewReader(prompt)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output

	err := cmd.Run()
	result := promptRunResult{
		RunID:  extractAuditRunID(output.String()),
		Output: output.String(),
	}
	if err != nil {
		logger.Error("prompt run failed", "project", proj.Name, "error", err, "output", result.Output)
		return result, fmt.Errorf("prompt run failed: %w", err)
	}
	return result, nil
}

func extractAuditRunID(output string) string {
	match := auditRunRE.FindStringSubmatch(output)
	if len(match) != 2 {
		return ""
	}
	return match[1]
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
