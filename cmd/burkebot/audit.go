package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Summary matches the summary.json written by burkebot-codex-run.
//
// Fields below the runner's set (JobRepoDir … PRRepo) are added by
// the dashboard's executePromptRun / executePublish after the run
// finishes. The runner is unaware of them; the dashboard merges
// them in via patchSummaryGitState so summary.json stays the single
// source of truth on the read side.
type Summary struct {
	RunID             string          `json:"run_id"`
	Source            string          `json:"source"`
	Label             string          `json:"label"`
	PromptID          string          `json:"prompt_id"`
	StartedAt         string          `json:"started_at"`
	EndedAt           string          `json:"ended_at"`
	DurationSeconds   int             `json:"duration_seconds"`
	Dangerous         bool            `json:"dangerous"`
	ExitCode          int             `json:"exit_code"`
	TokenUsage        json.RawMessage `json:"token_usage"`
	CodexSessionFiles []string        `json:"codex_session_files"`
	ResumeSessionID   string          `json:"resume_session_id,omitempty"`

	// Dashboard-managed git state (omitted for runs that don't go
	// through the dashboard publish flow, e.g., task API runs).
	JobRepoDir    string `json:"job_repo_dir,omitempty"`
	RootRunID     string `json:"root_run_id,omitempty"`
	BranchName    string `json:"branch_name,omitempty"`
	BaseCommitSHA string `json:"base_commit_sha,omitempty"`
	HeadCommitSHA string `json:"head_commit_sha,omitempty"`
	LastPushedSHA string `json:"last_pushed_sha,omitempty"`
	PRNumber      int    `json:"pr_number,omitempty"`
	PRRepo        string `json:"pr_repo,omitempty"`

	// RemoteURL is the canonical upstream URL (https://github.com/...
	// or git@git-server:...). Captured on the root run; carried into
	// follow-ups so they can re-point their cloned-from-prev-job-dir
	// origin remote at the actual upstream — burkebot-publish must
	// push to GitHub, not to the prior job repo.
	RemoteURL string `json:"remote_url,omitempty"`
}

// summaryGitState is the subset of Summary fields the dashboard
// writes back to summary.json. Separate type so the patch logic
// can't accidentally clobber runner-owned fields like ExitCode or
// TokenUsage with their zero values.
type summaryGitState struct {
	JobRepoDir    string
	RootRunID     string
	BranchName    string
	BaseCommitSHA string
	HeadCommitSHA string
	LastPushedSHA string
	PRNumber      int
	PRRepo        string
	RemoteURL     string
}

// patchSummaryGitState reads summary.json, merges the non-zero fields
// of patch into it, and writes it back atomically.
func patchSummaryGitState(path string, patch summaryGitState) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read summary: %w", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("parse summary: %w", err)
	}
	if raw == nil {
		raw = make(map[string]any)
	}
	if patch.JobRepoDir != "" {
		raw["job_repo_dir"] = patch.JobRepoDir
	}
	if patch.RootRunID != "" {
		raw["root_run_id"] = patch.RootRunID
	}
	if patch.BranchName != "" {
		raw["branch_name"] = patch.BranchName
	}
	if patch.BaseCommitSHA != "" {
		raw["base_commit_sha"] = patch.BaseCommitSHA
	}
	if patch.HeadCommitSHA != "" {
		raw["head_commit_sha"] = patch.HeadCommitSHA
	}
	if patch.LastPushedSHA != "" {
		raw["last_pushed_sha"] = patch.LastPushedSHA
	}
	if patch.PRNumber != 0 {
		raw["pr_number"] = patch.PRNumber
	}
	if patch.PRRepo != "" {
		raw["pr_repo"] = patch.PRRepo
	}
	if patch.RemoteURL != "" {
		raw["remote_url"] = patch.RemoteURL
	}
	encoded, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal summary: %w", err)
	}
	encoded = append(encoded, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, encoded, 0o640); err != nil {
		return fmt.Errorf("write tmp summary: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename summary: %w", err)
	}
	return nil
}

// AuditFile holds a filename and its size.
type AuditFile struct {
	Name string
	Size int64
}

// AuditBundle represents one run directory under the audit root.
type AuditBundle struct {
	RunID         string
	Summary       *Summary    // nil if summary.json missing/corrupt
	Files         []string    // filenames present in the directory
	FileInfo      []AuditFile // filenames with sizes
	PromptPreview string      // first line of prompt.txt (truncated)
	AgentPreview  string      // first line of last-message.txt (truncated)
}

// Command represents one line from commands.jsonl.
type Command struct {
	Timestamp string          `json:"timestamp"`
	Name      string          `json:"name"`
	CallID    string          `json:"call_id"`
	Arguments json.RawMessage `json:"arguments"`
}

// runBelongsToProject reports whether a RunID belongs to the given project.
// RunIDs have the form "YYYYMMDDTHHMMSSZ-SOURCE-LABEL" where LABEL is
// typically "REPONAME-pr-N". When multiple projects share an audit directory,
// we match on the project name appearing in the label portion.
//
// If projectName is empty, all runs match (single-project mode).
func runBelongsToProject(runID, projectName string) bool {
	if projectName == "" {
		return true
	}
	// The label starts after the 16-char timestamp.
	rest := runID
	if len(rest) > 16 {
		rest = rest[16:] // strip "YYYYMMDDTHHMMSSZ"
	}
	// rest is now like "-pr-returns-pr-5"
	return strings.Contains(rest, "-"+projectName+"-")
}

// loadAuditRuns reads all audit bundle directories, sorted newest-first by
// directory name (which starts with a UTC timestamp). When projectName is
// non-empty, only runs belonging to that project are returned.
func loadAuditRuns(auditDir, projectName string) ([]AuditBundle, error) {
	entries, err := os.ReadDir(auditDir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading audit dir: %w", err)
	}

	var bundles []AuditBundle
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if !runBelongsToProject(e.Name(), projectName) {
			continue
		}
		b, err := loadBundle(auditDir, e.Name())
		if err != nil {
			// Skip corrupt bundles but keep going.
			continue
		}
		bundles = append(bundles, *b)
	}

	// Sort newest first by RunID (starts with YYYYMMDDTHHMMSSZ).
	sort.Slice(bundles, func(i, j int) bool {
		return bundles[i].RunID > bundles[j].RunID
	})
	return bundles, nil
}

// loadBundle loads one audit bundle from disk.
func loadBundle(auditDir, runID string) (*AuditBundle, error) {
	dir := filepath.Join(auditDir, runID)
	info, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", dir)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []string
	var fileInfo []AuditFile
	for _, e := range entries {
		files = append(files, e.Name())
		var size int64
		if info, err := e.Info(); err == nil {
			size = info.Size()
		}
		fileInfo = append(fileInfo, AuditFile{Name: e.Name(), Size: size})
	}

	b := &AuditBundle{
		RunID:         runID,
		Files:         files,
		FileInfo:      fileInfo,
		PromptPreview: readFilePreview(filepath.Join(dir, "prompt.txt"), 200),
		AgentPreview:  readFilePreview(filepath.Join(dir, "last-message.txt"), 200),
	}

	summaryPath := filepath.Join(dir, "summary.json")
	data, err := os.ReadFile(summaryPath)
	if err == nil {
		var s Summary
		if json.Unmarshal(data, &s) == nil {
			b.Summary = &s
		}
	}

	return b, nil
}

// readFilePreview reads the first line of a file, truncated to maxLen
// bytes. Returns "" if the file doesn't exist or is empty.
func readFilePreview(path string, maxLen int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	if !scanner.Scan() {
		return ""
	}
	line := scanner.Text()
	line = strings.TrimSpace(line)
	if len(line) > maxLen {
		line = line[:maxLen] + "..."
	}
	return line
}

// CodexEvent represents one line from codex-events.jsonl.
type CodexEvent struct {
	Type string          `json:"type"`
	Item json.RawMessage `json:"item"`
}

// CodexItem is the parsed item field from a codex event.
type CodexItem struct {
	ID               string          `json:"id"`
	Type             string          `json:"type"`
	Command          string          `json:"command"`
	AggregatedOutput string          `json:"aggregated_output"`
	ExitCode         *int            `json:"exit_code"`
	Status           string          `json:"status"`
	Text             string          `json:"text"`
	Items            json.RawMessage `json:"items"`
}

// CodexCommand is a completed command extracted from codex-events.jsonl.
type CodexCommand struct {
	Command  string
	Output   string
	ExitCode int
	Status   string
}

// ConversationStep is one step in the agent's conversation: either an
// agent message or a command execution, in the order they occurred.
type ConversationStep struct {
	Type     string // "agent_message" or "command"
	Text     string // agent_message text
	Command  string // command_execution command
	Output   string // command_execution output
	ExitCode int    // command_execution exit code
	Status   string // command_execution status
}

// loadConversationTimeline reads codex-events.jsonl and returns an
// ordered slice of conversation steps (agent messages interleaved with
// command executions) preserving the sequence in which they occurred.
func loadConversationTimeline(path string) ([]ConversationStep, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var steps []ConversationStep
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev CodexEvent
		if json.Unmarshal(line, &ev) != nil {
			continue
		}
		if ev.Type != "item.completed" || ev.Item == nil {
			continue
		}
		var item CodexItem
		if json.Unmarshal(ev.Item, &item) != nil {
			continue
		}
		switch item.Type {
		case "command_execution":
			exitCode := 0
			if item.ExitCode != nil {
				exitCode = *item.ExitCode
			}
			steps = append(steps, ConversationStep{
				Type:     "command",
				Command:  item.Command,
				Output:   item.AggregatedOutput,
				ExitCode: exitCode,
				Status:   item.Status,
			})
		case "agent_message":
			steps = append(steps, ConversationStep{
				Type: "agent_message",
				Text: item.Text,
			})
		}
	}
	return steps, scanner.Err()
}

// loadCodexEvents reads codex-events.jsonl and extracts completed commands
// and the final agent message.
func loadCodexEvents(path string) (commands []CodexCommand, agentMessage string, err error) {
	steps, err := loadConversationTimeline(path)
	if err != nil {
		return nil, "", err
	}
	for _, step := range steps {
		switch step.Type {
		case "command":
			commands = append(commands, CodexCommand{
				Command:  step.Command,
				Output:   step.Output,
				ExitCode: step.ExitCode,
				Status:   step.Status,
			})
		case "agent_message":
			agentMessage = step.Text
		}
	}
	return commands, agentMessage, nil
}

// codexSessionsDir returns the path to the codex-sessions directory
// within an audit bundle, or "" if it does not exist.
func codexSessionsDir(auditDir, runID string) string {
	dir := filepath.Join(auditDir, runID, "codex-sessions")
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return ""
	}
	return dir
}

// loadCommands reads commands.jsonl from an audit bundle.
func loadCommands(path string) ([]Command, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var commands []Command
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var c Command
		if json.Unmarshal(line, &c) == nil {
			commands = append(commands, c)
		}
	}
	return commands, scanner.Err()
}
