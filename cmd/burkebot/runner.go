package main

import (
	"bytes"
	"log/slog"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// runnerInvocation describes one transient-systemd-unit invocation of
// burkebot-codex-run. Both the prompt UI (executePromptRun) and the
// task API (executeTaskRun) build one of these and hand it to runRunner;
// the differences (auth context, schema enforcement, envdir wrapping)
// live in the builders so this struct stays stable.
type runnerInvocation struct {
	// WorkingDirectory becomes the unit's WorkingDirectory= property.
	WorkingDirectory string

	// ReadWritePaths are the absolute paths the unit may write to.
	// Everything else under the sandbox is read-only (ProtectSystem=strict,
	// ProtectHome=read-only) or absent (PrivateTmp=yes).
	ReadWritePaths []string

	// Command is what runs inside the unit after the `systemd-run …`
	// flags. Typically the runner binary plus its flags, optionally
	// prefixed with envdir + an envdir directory.
	Command []string

	// Prompt is fed to the runner on stdin.
	Prompt string

	// TimeoutSeconds, if > 0, caps how long runRunner will wait. The
	// systemd unit is not stopped — only the local wait is abandoned;
	// `--collect` reaps the unit when it finally exits. Used to keep
	// stuck runs from pinning an HTTP connection indefinitely.
	TimeoutSeconds int
}

// runResult is the parsed output of one runner invocation. Output is
// the runner's combined stdout+stderr (kept for diagnostics); RunID is
// the audit-bundle run ID extracted from the runner's "Audit run: …"
// line.
type runResult struct {
	RunID  string
	Output string
}

var auditRunRE = regexp.MustCompile(`(?m)^Audit run: (\S+)$`)

// runRunner shells out to systemd-run with a hardened sandbox and the
// caller-supplied unit Command, then parses the audit run ID out of
// the runner's output. This is the only place burkebot calls
// systemd-run; all higher-level entry points (the dashboard prompt UI,
// the task API) funnel through here.
//
// Sandbox documentation:
//
//	https://www.freedesktop.org/software/systemd/man/latest/systemd-run.html
//	https://www.freedesktop.org/software/systemd/man/latest/systemd.exec.html
func runRunner(logger *slog.Logger, inv runnerInvocation) (runResult, error) {
	args := []string{
		"--quiet",
		"--wait",
		"--pipe",
		"--collect",
		"--service-type=exec",
		"--property=WorkingDirectory=" + inv.WorkingDirectory,
		"--property=NoNewPrivileges=yes",
		"--property=PrivateTmp=yes",
		"--property=ProtectSystem=strict",
		"--property=ProtectHome=read-only",
		"--property=ReadWritePaths=" + strings.Join(inv.ReadWritePaths, " "),
	}
	args = append(args, inv.Command...)

	cmd := exec.Command("systemd-run", args...)
	cmd.Stdin = strings.NewReader(inv.Prompt)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output

	if inv.TimeoutSeconds > 0 {
		timer := time.AfterFunc(time.Duration(inv.TimeoutSeconds)*time.Second, func() {
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
		})
		defer timer.Stop()
	}

	err := cmd.Run()
	result := runResult{
		RunID:  extractAuditRunID(output.String()),
		Output: output.String(),
	}
	if err != nil {
		// Log the full output here so callers don't have to remember
		// to do it. Wrapping the error is left to the caller — they
		// know whether this is a prompt UI submission, a task API
		// run, or something else.
		logger.Error("runner failed", "error", err, "output", result.Output)
	}
	return result, err
}

func extractAuditRunID(output string) string {
	match := auditRunRE.FindStringSubmatch(output)
	if len(match) != 2 {
		return ""
	}
	return match[1]
}
