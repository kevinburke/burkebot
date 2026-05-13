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
// the runner's combined stdout+stderr (kept for diagnostics); RunID and
// AuditDir are extracted from the runner's "Audit run: …" / "Audit
// dir: …" lines so callers can log them as their own structured fields
// rather than mining them out of an escaped Output string.
type runResult struct {
	RunID    string
	AuditDir string
	Output   string
}

var (
	auditRunRE = regexp.MustCompile(`(?m)^Audit run: (\S+)$`)
	auditDirRE = regexp.MustCompile(`(?m)^Audit dir: (\S+)$`)
)

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
		RunID:    extractAuditField(auditRunRE, output.String()),
		AuditDir: extractAuditField(auditDirRE, output.String()),
		Output:   output.String(),
	}
	if err != nil {
		// run_id and audit_dir get their own slog fields so they can
		// be double-clicked / copied directly out of a terminal. The
		// raw Output string keeps embedded \n's, which conflate the
		// audit dir with whatever the runner printed next when
		// selected by word boundary.
		logger.Error("runner failed",
			"error", err,
			"run_id", result.RunID,
			"audit_dir", result.AuditDir,
			"output", result.Output,
		)
	}
	return result, err
}

func extractAuditField(re *regexp.Regexp, output string) string {
	match := re.FindStringSubmatch(output)
	if len(match) != 2 {
		return ""
	}
	return match[1]
}
