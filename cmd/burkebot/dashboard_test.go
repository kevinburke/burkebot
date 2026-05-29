package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTestServer creates a Server with the given projects and parsed templates.
func newTestServer(t *testing.T, projects []Project) *Server {
	t.Helper()
	s := &Server{
		projects: projects,
		build:    buildInfo{Version: "test"},
		logger:   testLogger(t),
	}
	tmpl, err := buildTemplates(s.build)
	if err != nil {
		t.Fatal(err)
	}
	s.tmpl = tmpl
	return s
}

// writeTestAudit creates a minimal audit bundle in a temp directory.
func writeTestAudit(t *testing.T, auditDir, runID string) {
	t.Helper()
	dir := filepath.Join(auditDir, runID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	summary := Summary{
		RunID:           runID,
		Source:          "pr",
		Label:           "pr-42",
		StartedAt:       "2026-03-15T12:00:00Z",
		EndedAt:         "2026-03-15T12:02:35Z",
		DurationSeconds: 155,
		Dangerous:       true,
		ExitCode:        0,
		TokenUsage:      json.RawMessage(`{}`),
	}
	data, _ := json.Marshal(summary)
	os.WriteFile(filepath.Join(dir, "summary.json"), data, 0o644)
	os.WriteFile(filepath.Join(dir, "prompt.txt"), []byte("test prompt"), 0o644)
	os.WriteFile(filepath.Join(dir, "last-message.txt"), []byte("test message"), 0o644)
	os.WriteFile(filepath.Join(dir, "codex-stderr.log"), []byte("test stderr"), 0o644)
	os.WriteFile(filepath.Join(dir, "commands.jsonl"), []byte(`{"timestamp":"t","name":"shell","call_id":"c1","arguments":{"cmd":"go test"}}`+"\n"), 0o644)
}

func writeTestAuditSummary(t *testing.T, auditDir, runID string, summary Summary) {
	t.Helper()
	dir := filepath.Join(auditDir, runID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if summary.RunID == "" {
		summary.RunID = runID
	}
	data, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "summary.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "last-message.txt"), []byte("task output"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestHandleIndexSingleProjectRedirects(t *testing.T) {
	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")
	os.MkdirAll(auditDir, 0o755)

	s := newTestServer(t, []Project{{
		Name:      "default",
		AuditDir:  auditDir,
		StateFile: filepath.Join(tmp, "state.json"),
	}})

	mux := s.registerRoutes()
	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d", w.Code)
	}
	loc := w.Header().Get("Location")
	if loc != "/projects/default/" {
		t.Fatalf("expected redirect to /projects/default/, got %q", loc)
	}
}

func TestHandleIndexMultiProjectShowsList(t *testing.T) {
	tmp := t.TempDir()
	os.MkdirAll(filepath.Join(tmp, "a", "audit"), 0o755)
	os.MkdirAll(filepath.Join(tmp, "b", "audit"), 0o755)

	s := newTestServer(t, []Project{
		{Name: "alpha", AuditDir: filepath.Join(tmp, "a", "audit"), StateFile: filepath.Join(tmp, "a", "state.json")},
		{Name: "beta", AuditDir: filepath.Join(tmp, "b", "audit"), StateFile: filepath.Join(tmp, "b", "state.json")},
	})

	mux := s.registerRoutes()
	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "alpha") || !strings.Contains(body, "beta") {
		t.Fatalf("expected project names in body, got %s", body)
	}
}

func TestHandleIndexShowsTasksLink(t *testing.T) {
	tmp := t.TempDir()
	os.MkdirAll(filepath.Join(tmp, "a", "audit"), 0o755)
	os.MkdirAll(filepath.Join(tmp, "b", "audit"), 0o755)

	s := newTestServer(t, []Project{
		{Name: "alpha", AuditDir: filepath.Join(tmp, "a", "audit"), StateFile: filepath.Join(tmp, "a", "state.json")},
		{Name: "beta", AuditDir: filepath.Join(tmp, "b", "audit"), StateFile: filepath.Join(tmp, "b", "state.json")},
	})
	s.api.Tasks = []Task{{Name: "annotate", Project: "alpha"}}

	mux := s.registerRoutes()
	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `href="/tasks"`) {
		t.Fatalf("expected tasks link in body, got %s", body)
	}
}

func TestHandleProject(t *testing.T) {
	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")
	stateFile := filepath.Join(tmp, "state.json")
	os.WriteFile(stateFile, []byte(`{"42":{"head_sha":"abc123","base_sha":"def456","recreate_requested":false}}`), 0o644)
	writeTestAudit(t, auditDir, "20260315T120000Z-pr-returns-pr-42")

	s := newTestServer(t, []Project{{
		Name: "returns", AuditDir: auditDir, StateFile: stateFile,
	}})

	mux := s.registerRoutes()
	req := httptest.NewRequest("GET", "/projects/returns/", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "20260315T120000Z-pr-returns-pr-42") {
		t.Fatalf("expected run ID in body")
	}
	if !strings.Contains(body, "#42") {
		t.Fatalf("expected PR state in body")
	}
	if !strings.Contains(body, "test prompt") {
		t.Fatalf("expected prompt preview in body")
	}
	if !strings.Contains(body, "test message") {
		t.Fatalf("expected agent preview in body")
	}
}

func TestHandleTasks(t *testing.T) {
	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")
	stateFile := filepath.Join(tmp, "state.json")
	if err := os.WriteFile(stateFile, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	writeTestAuditSummary(t, auditDir, "20260315T120000Z-api-annotate-public", Summary{
		Source:          "api",
		Label:           "annotate-public",
		DurationSeconds: 42,
		ExitCode:        0,
	})
	writeTestAuditSummary(t, auditDir, "20260315T130000Z-pr-r-pr-99", Summary{
		Source:          "pr",
		Label:           "annotate-public",
		DurationSeconds: 12,
		ExitCode:        0,
	})

	s := newTestServer(t, []Project{{
		Name: "r", AuditDir: auditDir, StateFile: stateFile,
	}})
	s.api.Tasks = []Task{{
		Name:             "annotate",
		Project:          "r",
		OutputSchemaPath: "schema.json",
		Inputs:           []TaskInput{{Name: "agenda", Filename: "agenda.txt"}},
	}}

	mux := s.registerRoutes()
	req := httptest.NewRequest("GET", "/projects/r/tasks", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{"annotate", "agenda", "20260315T120000Z-api-annotate-public", "public", "exit 0"} {
		if !strings.Contains(body, want) {
			t.Errorf("expected %q in body", want)
		}
	}
	if strings.Contains(body, "20260315T130000Z-pr-r-pr-99") {
		t.Fatal("non-API run appeared on task page")
	}
}

func TestHandleTasksMatchesLegacyTaskSummaries(t *testing.T) {
	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")
	stateFile := filepath.Join(tmp, "state.json")
	if err := os.WriteFile(stateFile, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	writeTestAuditSummary(t, auditDir, "20260315T120000Z-annotate-meetings-public", Summary{
		Label:           "public",
		PromptID:        "annotate-meetings-1234",
		DurationSeconds: 42,
		ExitCode:        0,
	})
	writeTestAuditSummary(t, auditDir, "20260315T130000Z-task-annotate-meetings-internal", Summary{
		Source:          "task",
		Label:           "not-the-task-name",
		DurationSeconds: 24,
		ExitCode:        1,
	})
	writeTestAuditSummary(t, auditDir, "20260315T140000Z-pr-r-pr-99", Summary{
		Source:   "pr",
		Label:    "annotate-meetings-public",
		PromptID: "annotate-meetings-9999",
		ExitCode: 0,
	})

	s := newTestServer(t, []Project{{
		Name: "r", AuditDir: auditDir, StateFile: stateFile,
	}})
	s.api.Tasks = []Task{{
		Name:             "annotate-meetings",
		Project:          "r",
		OutputSchemaPath: "schema.json",
	}}

	mux := s.registerRoutes()
	req := httptest.NewRequest("GET", "/projects/r/tasks", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"20260315T120000Z-annotate-meetings-public",
		"20260315T130000Z-task-annotate-meetings-internal",
		"exit 1",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected %q in body", want)
		}
	}
	if strings.Contains(body, "20260315T140000Z-pr-r-pr-99") {
		t.Fatal("PR run appeared on task page")
	}
}

func TestHandleAllTasks(t *testing.T) {
	tmp := t.TempDir()
	alphaAudit := filepath.Join(tmp, "alpha", "audit")
	betaAudit := filepath.Join(tmp, "beta", "audit")
	if err := os.MkdirAll(alphaAudit, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(betaAudit, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestAuditSummary(t, alphaAudit, "20260315T120000Z-api-annotate-public", Summary{
		Source:          "api",
		Label:           "annotate-public",
		DurationSeconds: 42,
		ExitCode:        0,
	})
	writeTestAuditSummary(t, betaAudit, "20260315T130000Z-api-summarize-internal", Summary{
		Source:          "api",
		Label:           "summarize-internal",
		DurationSeconds: 24,
		ExitCode:        1,
	})

	s := newTestServer(t, []Project{
		{Name: "alpha", AuditDir: alphaAudit, StateFile: filepath.Join(tmp, "alpha", "state.json")},
		{Name: "beta", AuditDir: betaAudit, StateFile: filepath.Join(tmp, "beta", "state.json")},
	})
	s.api.Tasks = []Task{
		{Name: "annotate", Project: "alpha", OutputSchemaPath: "annotation.schema.json"},
		{Name: "summarize", Project: "beta", OutputSchemaPath: "summary.schema.json"},
	}

	mux := s.registerRoutes()
	req := httptest.NewRequest("GET", "/tasks", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"annotate",
		"project alpha",
		`href="/projects/alpha/audit/20260315T120000Z-api-annotate-public/"`,
		"summarize",
		"project beta",
		`href="/projects/beta/audit/20260315T130000Z-api-summarize-internal/"`,
		"exit 1",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected %q in body", want)
		}
	}
}

func TestHandleProjectNotFound(t *testing.T) {
	s := newTestServer(t, []Project{{Name: "x", AuditDir: t.TempDir(), StateFile: "/dev/null"}})
	mux := s.registerRoutes()
	req := httptest.NewRequest("GET", "/projects/nonexistent/", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestHandleAuditDetail(t *testing.T) {
	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")
	runID := "20260315T120000Z-pr-returns-pr-42"
	writeTestAudit(t, auditDir, runID)

	s := newTestServer(t, []Project{{Name: "r", AuditDir: auditDir, StateFile: filepath.Join(tmp, "s.json")}})
	mux := s.registerRoutes()
	req := httptest.NewRequest("GET", "/projects/r/audit/"+runID, nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{"test prompt", "test message", "shell", runID} {
		if !strings.Contains(body, want) {
			t.Errorf("expected %q in body", want)
		}
	}
}

func TestHandleAuditDetailTimeline(t *testing.T) {
	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")
	runID := "20260315T120000Z-pr-returns-pr-42"
	writeTestAudit(t, auditDir, runID)

	events := `{"type":"item.completed","item":{"type":"agent_message","text":"I will check the tests."}}
{"type":"item.completed","item":{"type":"command_execution","command":"go test ./...","aggregated_output":"ok\n","exit_code":0,"status":"completed"}}
{"type":"item.completed","item":{"type":"agent_message","text":"All tests pass."}}
`
	dir := filepath.Join(auditDir, runID)
	os.WriteFile(filepath.Join(dir, "codex-events.jsonl"), []byte(events), 0o644)

	s := newTestServer(t, []Project{{Name: "r", AuditDir: auditDir, StateFile: filepath.Join(tmp, "s.json")}})
	mux := s.registerRoutes()
	req := httptest.NewRequest("GET", "/projects/r/audit/"+runID, nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"Conversation (3 steps)",
		"I will check the tests.",
		"go test ./...",
		"All tests pass.",
		"Agent",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected %q in body", want)
		}
	}
}

func TestLoadConversationTimeline(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "codex-events.jsonl")

	events := `{"type":"item.completed","item":{"type":"agent_message","text":"Let me inspect the code."}}
{"type":"item.completed","item":{"type":"command_execution","command":"ls -la","aggregated_output":"total 0\n","exit_code":0,"status":"completed"}}
{"type":"item.completed","item":{"type":"command_execution","command":"cat bad.go","aggregated_output":"","exit_code":1,"status":"completed"}}
{"type":"item.completed","item":{"type":"agent_message","text":"The file is missing."}}
`
	os.WriteFile(path, []byte(events), 0o644)

	steps, err := loadConversationTimeline(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 4 {
		t.Fatalf("expected 4 steps, got %d", len(steps))
	}
	if steps[0].Type != "agent_message" || steps[0].Text != "Let me inspect the code." {
		t.Errorf("step 0: %+v", steps[0])
	}
	if steps[1].Type != "command" || steps[1].Command != "ls -la" || steps[1].ExitCode != 0 {
		t.Errorf("step 1: %+v", steps[1])
	}
	if steps[2].Type != "command" || steps[2].ExitCode != 1 {
		t.Errorf("step 2: %+v", steps[2])
	}
	if steps[3].Type != "agent_message" || steps[3].Text != "The file is missing." {
		t.Errorf("step 3: %+v", steps[3])
	}
}

func TestLoadConversationTimelineMissingFile(t *testing.T) {
	steps, err := loadConversationTimeline(filepath.Join(t.TempDir(), "missing.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 0 {
		t.Fatalf("expected 0 steps, got %d", len(steps))
	}
}

func TestLoadCodexEventsViaTimeline(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "codex-events.jsonl")

	events := `{"type":"item.completed","item":{"type":"agent_message","text":"first message"}}
{"type":"item.completed","item":{"type":"command_execution","command":"echo hi","aggregated_output":"hi\n","exit_code":0,"status":"completed"}}
{"type":"item.completed","item":{"type":"agent_message","text":"final message"}}
`
	os.WriteFile(path, []byte(events), 0o644)

	commands, agentMsg, err := loadCodexEvents(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(commands) != 1 {
		t.Fatalf("expected 1 command, got %d", len(commands))
	}
	if commands[0].Command != "echo hi" {
		t.Errorf("expected 'echo hi', got %q", commands[0].Command)
	}
	if agentMsg != "final message" {
		t.Errorf("expected 'final message', got %q", agentMsg)
	}
}

func TestHandleAuditDetailFollowupForm(t *testing.T) {
	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")
	runID := "20260315T120000Z-adhoc-r-web-repo-write-safe"
	// Follow-up now requires dashboard-managed git state on the
	// origin run and the origin's job dir still on disk.
	jobRepoDir := filepath.Join(tmp, "jobs", "origin", "repo")
	if err := os.MkdirAll(jobRepoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestAuditSummary(t, auditDir, runID, Summary{
		Source:        "adhoc",
		JobRepoDir:    jobRepoDir,
		BranchName:    "burkebot/r-abcd",
		BaseCommitSHA: "aaaa",
		HeadCommitSHA: "bbbb",
		RemoteURL:     "https://github.com/kevinburke/returns.git",
		PRRepo:        "kevinburke/returns",
	})

	sessionsDir := filepath.Join(auditDir, runID, "codex-sessions")
	os.MkdirAll(sessionsDir, 0o755)
	os.WriteFile(filepath.Join(sessionsDir, "session.jsonl"), []byte(`{"type":"session_meta","payload":{"id":"test-uuid"}}`+"\n"), 0o644)

	s := newTestServer(t, []Project{{Name: "r", AuditDir: auditDir, StateFile: filepath.Join(tmp, "s.json")}})
	s.prompt = promptRunnerConfig{RunnerPath: "/usr/local/bin/burkebot-codex-run"}

	mux := s.registerRoutes()
	req := httptest.NewRequest("GET", "/projects/r/audit/"+runID, nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Send Follow-up") {
		t.Error("expected follow-up form in body")
	}
	if !strings.Contains(body, "/followup") {
		t.Error("expected followup action URL in body")
	}
}

// Follow-up form should be hidden when the origin run lacks
// dashboard-managed git state (e.g., a cron-driven run with sessions
// but no JobRepoDir). In that case the form would only error on submit.
func TestHandleAuditDetailNoFollowupWithoutGitState(t *testing.T) {
	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")
	runID := "20260315T120000Z-pr-returns-pr-42"
	writeTestAudit(t, auditDir, runID) // no JobRepoDir in default summary

	sessionsDir := filepath.Join(auditDir, runID, "codex-sessions")
	os.MkdirAll(sessionsDir, 0o755)
	os.WriteFile(filepath.Join(sessionsDir, "session.jsonl"), []byte(`{}`), 0o644)

	s := newTestServer(t, []Project{{Name: "r", AuditDir: auditDir, StateFile: filepath.Join(tmp, "s.json")}})
	s.prompt = promptRunnerConfig{RunnerPath: "/usr/local/bin/burkebot-codex-run"}

	mux := s.registerRoutes()
	req := httptest.NewRequest("GET", "/projects/r/audit/"+runID, nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if strings.Contains(w.Body.String(), "Send Follow-up") {
		t.Error("expected follow-up form hidden when origin lacks git state")
	}
}

func TestHandleAuditDetailNoFollowupWithoutSessions(t *testing.T) {
	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")
	runID := "20260315T120000Z-pr-returns-pr-42"
	writeTestAudit(t, auditDir, runID)

	s := newTestServer(t, []Project{{Name: "r", AuditDir: auditDir, StateFile: filepath.Join(tmp, "s.json")}})
	s.prompt = promptRunnerConfig{RunnerPath: "/usr/local/bin/burkebot-codex-run"}

	mux := s.registerRoutes()
	req := httptest.NewRequest("GET", "/projects/r/audit/"+runID, nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "Send Follow-up") {
		t.Error("follow-up form should not appear without session files")
	}
}

func TestHandleFollowup(t *testing.T) {
	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")
	runID := "20260315T120000Z-pr-returns-pr-42"
	writeTestAudit(t, auditDir, runID)

	sessionsDir := filepath.Join(auditDir, runID, "codex-sessions")
	os.MkdirAll(sessionsDir, 0o755)
	os.WriteFile(filepath.Join(sessionsDir, "session.jsonl"), []byte(`{"type":"session_meta","payload":{"id":"test-uuid"}}`+"\n"), 0o644)

	newRunID := "20260315T130000Z-followup-r-web-read-only-safe-followup"
	s := newTestServer(t, []Project{{Name: "r", AuditDir: auditDir, StateFile: filepath.Join(tmp, "s.json")}})
	s.prompt = promptRunnerConfig{RunnerPath: "/usr/local/bin/burkebot-codex-run"}
	s.runFollowup = func(logger *slog.Logger, cfg promptRunnerConfig, proj Project, policy promptPolicy, origin *Summary, sessions, prompt string) (runResult, error) {
		if sessions != sessionsDir {
			t.Errorf("expected sessions dir %q, got %q", sessionsDir, sessions)
		}
		if prompt != "please continue" {
			t.Errorf("expected prompt 'please continue', got %q", prompt)
		}
		if origin == nil || origin.RunID != runID {
			t.Errorf("expected origin summary for %q, got %+v", runID, origin)
		}
		return runResult{RunID: newRunID}, nil
	}

	mux := s.registerRoutes()
	form := url.Values{"prompt": {"please continue"}}
	req := httptest.NewRequest("POST", "/projects/r/audit/"+runID+"/followup", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, newRunID) {
		t.Fatalf("expected redirect to new run, got %q", loc)
	}
}

func TestHandleFollowupEmptyPrompt(t *testing.T) {
	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")
	runID := "20260315T120000Z-pr-returns-pr-42"
	writeTestAudit(t, auditDir, runID)

	sessionsDir := filepath.Join(auditDir, runID, "codex-sessions")
	os.MkdirAll(sessionsDir, 0o755)
	os.WriteFile(filepath.Join(sessionsDir, "session.jsonl"), []byte(`{"type":"session_meta","payload":{"id":"test-uuid"}}`+"\n"), 0o644)

	s := newTestServer(t, []Project{{Name: "r", AuditDir: auditDir, StateFile: filepath.Join(tmp, "s.json")}})
	s.prompt = promptRunnerConfig{RunnerPath: "/usr/local/bin/burkebot-codex-run"}

	mux := s.registerRoutes()
	form := url.Values{"prompt": {""}}
	req := httptest.NewRequest("POST", "/projects/r/audit/"+runID+"/followup", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "error=1") {
		t.Fatalf("expected error redirect for empty prompt, got %q", loc)
	}
}

func TestHandleFollowupNoSessions(t *testing.T) {
	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")
	runID := "20260315T120000Z-pr-returns-pr-42"
	writeTestAudit(t, auditDir, runID)

	s := newTestServer(t, []Project{{Name: "r", AuditDir: auditDir, StateFile: filepath.Join(tmp, "s.json")}})
	s.prompt = promptRunnerConfig{RunnerPath: "/usr/local/bin/burkebot-codex-run"}

	mux := s.registerRoutes()
	form := url.Values{"prompt": {"hello"}}
	req := httptest.NewRequest("POST", "/projects/r/audit/"+runID+"/followup", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "error=1") || !strings.Contains(loc, "No+session") {
		t.Fatalf("expected 'no session' error redirect, got %q", loc)
	}
}

func TestHandleAuditFile(t *testing.T) {
	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")
	runID := "20260315T120000Z-pr-returns-pr-42"
	writeTestAudit(t, auditDir, runID)

	s := newTestServer(t, []Project{{Name: "r", AuditDir: auditDir, StateFile: filepath.Join(tmp, "s.json")}})
	mux := s.registerRoutes()
	req := httptest.NewRequest("GET", "/projects/r/audit/"+runID+"/prompt.txt", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("expected text/plain, got %q", ct)
	}
	if body := w.Body.String(); body != "test prompt" {
		t.Fatalf("expected 'test prompt', got %q", body)
	}
}

func TestHandleAuditFilePathTraversal(t *testing.T) {
	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")
	writeTestAudit(t, auditDir, "20260315T120000Z-pr-returns-pr-42")
	// Write a file outside the audit dir to ensure traversal doesn't reach it.
	os.WriteFile(filepath.Join(tmp, "secret.txt"), []byte("secret"), 0o644)

	s := newTestServer(t, []Project{{Name: "r", AuditDir: auditDir, StateFile: filepath.Join(tmp, "s.json")}})
	mux := s.registerRoutes()

	tests := []string{
		"/projects/r/audit/../../secret.txt",
		"/projects/r/audit/20260315T120000Z-pr-returns-pr-42/../../secret.txt",
	}
	for _, path := range tests {
		req := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code == http.StatusOK && strings.Contains(w.Body.String(), "secret") {
			t.Errorf("path traversal succeeded for %s", path)
		}
	}
}

func TestHandleState(t *testing.T) {
	tmp := t.TempDir()
	stateFile := filepath.Join(tmp, "state.json")
	os.WriteFile(stateFile, []byte(`{"5":{"head_sha":"sha5","base_sha":"base5","recreate_requested":false},"9":{"head_sha":"sha9","base_sha":"base9","recreate_requested":false}}`), 0o644)

	s := newTestServer(t, []Project{{Name: "r", AuditDir: filepath.Join(tmp, "audit"), StateFile: stateFile}})
	os.MkdirAll(filepath.Join(tmp, "audit"), 0o755)
	mux := s.registerRoutes()
	req := httptest.NewRequest("GET", "/projects/r/state", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "#5") || !strings.Contains(body, "#9") {
		t.Fatalf("expected PR numbers in body")
	}
}

func TestHandleStateFlash(t *testing.T) {
	tmp := t.TempDir()
	stateFile := filepath.Join(tmp, "state.json")
	os.WriteFile(stateFile, []byte(`{}`), 0o644)

	s := newTestServer(t, []Project{{Name: "r", AuditDir: filepath.Join(tmp, "audit"), StateFile: stateFile}})
	os.MkdirAll(filepath.Join(tmp, "audit"), 0o755)
	mux := s.registerRoutes()
	req := httptest.NewRequest("GET", "/projects/r/state?message=hello&error=1", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "hello") {
		t.Fatalf("expected flash message in body")
	}
	if !strings.Contains(body, "flash-error") {
		t.Fatalf("expected flash-error class in body")
	}
}

func TestHandleRerun(t *testing.T) {
	tmp := t.TempDir()
	stateFile := filepath.Join(tmp, "state.json")
	os.WriteFile(stateFile, []byte(`{"5":{"head_sha":"sha5","base_sha":"base5","recreate_requested":false},"9":{"head_sha":"sha9","base_sha":"base9","recreate_requested":false}}`), 0o644)

	s := newTestServer(t, []Project{{Name: "r", AuditDir: filepath.Join(tmp, "audit"), StateFile: stateFile}})
	os.MkdirAll(filepath.Join(tmp, "audit"), 0o755)
	mux := s.registerRoutes()

	form := url.Values{"pr": {"5"}}
	req := httptest.NewRequest("POST", "/projects/r/state/rerun", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", w.Code)
	}

	// Verify PR 5 was removed from state.
	state, err := loadProcessedPRs(stateFile)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := state["5"]; ok {
		t.Fatal("PR 5 should have been removed from state")
	}
	if _, ok := state["9"]; !ok {
		t.Fatal("PR 9 should still be in state")
	}
}

func TestHandleRerunMissingPR(t *testing.T) {
	tmp := t.TempDir()
	stateFile := filepath.Join(tmp, "state.json")
	os.WriteFile(stateFile, []byte(`{}`), 0o644)

	s := newTestServer(t, []Project{{Name: "r", AuditDir: filepath.Join(tmp, "audit"), StateFile: stateFile}})
	os.MkdirAll(filepath.Join(tmp, "audit"), 0o755)
	mux := s.registerRoutes()

	req := httptest.NewRequest("POST", "/projects/r/state/rerun", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "error=1") {
		t.Fatalf("expected error redirect, got %q", loc)
	}
}

func TestHandleRerunPRNotInState(t *testing.T) {
	tmp := t.TempDir()
	stateFile := filepath.Join(tmp, "state.json")
	os.WriteFile(stateFile, []byte(`{}`), 0o644)

	s := newTestServer(t, []Project{{Name: "r", AuditDir: filepath.Join(tmp, "audit"), StateFile: stateFile}})
	os.MkdirAll(filepath.Join(tmp, "audit"), 0o755)
	mux := s.registerRoutes()

	form := url.Values{"pr": {"2"}}
	req := httptest.NewRequest("POST", "/projects/r/state/rerun", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", w.Code)
	}

	// State should still be empty (PR was not in state, nothing to remove).
	state, err := loadProcessedPRs(stateFile)
	if err != nil {
		t.Fatal(err)
	}
	if len(state) != 0 {
		t.Fatalf("expected empty state, got %v", state)
	}
}

func TestHandleRerunAll(t *testing.T) {
	tmp := t.TempDir()
	stateFile := filepath.Join(tmp, "state.json")
	os.WriteFile(stateFile, []byte(`{"5":{"head_sha":"sha5","base_sha":"base5","recreate_requested":false},"9":{"head_sha":"sha9","base_sha":"base9","recreate_requested":false}}`), 0o644)

	s := newTestServer(t, []Project{{Name: "r", AuditDir: filepath.Join(tmp, "audit"), StateFile: stateFile}})
	os.MkdirAll(filepath.Join(tmp, "audit"), 0o755)
	mux := s.registerRoutes()

	req := httptest.NewRequest("POST", "/projects/r/state/rerun-all", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", w.Code)
	}

	state, err := loadProcessedPRs(stateFile)
	if err != nil {
		t.Fatal(err)
	}
	if len(state) != 0 {
		t.Fatalf("expected empty state, got %v", state)
	}
}

func TestLoadAuditRuns(t *testing.T) {
	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")
	writeTestAudit(t, auditDir, "20260315T120000Z-pr-returns-pr-42")
	writeTestAudit(t, auditDir, "20260314T100000Z-adhoc-test")

	runs, err := loadAuditRuns(auditDir, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Fatalf("expected 2 runs, got %d", len(runs))
	}
	// Should be sorted newest first.
	if runs[0].RunID != "20260315T120000Z-pr-returns-pr-42" {
		t.Fatalf("expected newest first, got %s", runs[0].RunID)
	}
}

func TestLoadAuditRunsMissingDir(t *testing.T) {
	runs, err := loadAuditRuns(filepath.Join(t.TempDir(), "nonexistent"), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Fatalf("expected 0 runs, got %d", len(runs))
	}
}

func TestBuildTaskDashboardTasksPrefersLongestTaskName(t *testing.T) {
	tasks := []Task{
		{Name: "annotate", Project: "r"},
		{Name: "annotate-meeting", Project: "r"},
	}
	runs := []AuditBundle{{
		RunID: "20260315T120000Z-api-r-annotate-meeting-alice",
		Summary: &Summary{
			Source: "api",
			Label:  "annotate-meeting-alice",
		},
	}}

	got := buildTaskDashboardTasks(Project{Name: "r"}, tasks, runs)
	if len(got[0].Runs) != 0 {
		t.Fatalf("shorter task matched run: %#v", got[0].Runs)
	}
	if len(got[1].Runs) != 1 {
		t.Fatalf("expected one run for annotate-meeting, got %d", len(got[1].Runs))
	}
	if got[1].Runs[0].TokenName != "alice" {
		t.Fatalf("expected token alice, got %q", got[1].Runs[0].TokenName)
	}
	if got[1].TotalRuns != 1 {
		t.Fatalf("expected one total run for annotate-meeting, got %d", got[1].TotalRuns)
	}
}

func TestPaginateTaskDashboardRuns(t *testing.T) {
	matches := make([]taskDashboardRunMatch, defaultTaskDashboardRunsPerPage+10)
	for i := range matches {
		matches[i] = taskDashboardRunMatch{
			ProjectName: "r",
			TaskName:    "annotate",
			Bundle: AuditBundle{
				RunID: fmt.Sprintf("20260315T1200%02dZ-api-r-annotate-alice", i),
			},
			TokenName: "alice",
		}
	}

	req := httptest.NewRequest("GET", "/projects/r/tasks", nil)
	page, perPage := taskDashboardPageParams(req)
	got, pagination := paginateTaskDashboardRuns(req, matches, page, perPage)
	if len(got) != defaultTaskDashboardRunsPerPage {
		t.Fatalf("expected %d runs, got %d", defaultTaskDashboardRunsPerPage, len(got))
	}
	if pagination.Start != 1 || pagination.End != defaultTaskDashboardRunsPerPage {
		t.Fatalf("unexpected first page range: %d-%d", pagination.Start, pagination.End)
	}
	if !pagination.HasNext || pagination.NextURL != "/projects/r/tasks?page=2" {
		t.Fatalf("unexpected next page: has_next=%v url=%q", pagination.HasNext, pagination.NextURL)
	}

	req = httptest.NewRequest("GET", "/projects/r/tasks?page=2", nil)
	page, perPage = taskDashboardPageParams(req)
	got, pagination = paginateTaskDashboardRuns(req, matches, page, perPage)
	if len(got) != 10 {
		t.Fatalf("expected 10 runs on second page, got %d", len(got))
	}
	if pagination.Start != defaultTaskDashboardRunsPerPage+1 || pagination.End != len(matches) {
		t.Fatalf("unexpected second page range: %d-%d", pagination.Start, pagination.End)
	}
	if !pagination.HasPrev || pagination.PrevURL != "/projects/r/tasks?page=1" {
		t.Fatalf("unexpected previous page: has_prev=%v url=%q", pagination.HasPrev, pagination.PrevURL)
	}
	if pagination.HasNext {
		t.Fatal("second page should not have next page")
	}
}

func TestTaskDashboardPageParamsClampPerPage(t *testing.T) {
	req := httptest.NewRequest("GET", "/tasks?page=3&per_page=9999", nil)
	page, perPage := taskDashboardPageParams(req)
	if page != 3 {
		t.Fatalf("expected page 3, got %d", page)
	}
	if perPage != maxTaskDashboardRunsPerPage {
		t.Fatalf("expected per_page clamp to %d, got %d", maxTaskDashboardRunsPerPage, perPage)
	}
}

func TestLoadProcessedPRsMissingFile(t *testing.T) {
	state, err := loadProcessedPRs(filepath.Join(t.TempDir(), "nonexistent.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(state) != 0 {
		t.Fatalf("expected empty state, got %v", state)
	}
}

func TestSaveProcessedPRsAtomic(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "state.json")

	state := ProcessedPRState{
		"1": {HeadSHA: "aaa", BaseSHA: "aaabase"},
		"2": {HeadSHA: "bbb", BaseSHA: "bbbbase", RecreateRequested: true},
	}
	if err := saveProcessedPRs(path, state); err != nil {
		t.Fatal(err)
	}

	loaded, err := loadProcessedPRs(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded["1"].HeadSHA != "aaa" || loaded["2"].HeadSHA != "bbb" || !loaded["2"].RecreateRequested {
		t.Fatalf("round-trip failed: %v", loaded)
	}
}

func TestDiscoverProjects(t *testing.T) {
	tmp := t.TempDir()
	os.MkdirAll(filepath.Join(tmp, "returns", "audit"), 0o755)
	os.MkdirAll(filepath.Join(tmp, "finance", "audit"), 0o755)
	os.MkdirAll(filepath.Join(tmp, "notaproject"), 0o755) // no audit/ subdir

	projects, err := discoverProjects(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 2 {
		t.Fatalf("expected 2 projects, got %d", len(projects))
	}
	names := map[string]bool{}
	for _, p := range projects {
		names[p.Name] = true
	}
	if !names["returns"] || !names["finance"] {
		t.Fatalf("expected returns and finance, got %v", names)
	}
}

func TestTimeAgo(t *testing.T) {
	tests := []struct {
		input string
		want  string // just check it doesn't return the raw input (i.e. it parsed)
	}{
		{"2026-03-15T12:00:00Z", ""},
		{"not-a-date", "not-a-date"},
	}
	for _, tt := range tests {
		got := timeAgo(tt.input)
		if tt.want != "" && got != tt.want {
			t.Errorf("timeAgo(%q) = %q, want %q", tt.input, got, tt.want)
		}
		if tt.want == "" && got == tt.input {
			t.Errorf("timeAgo(%q) returned raw input, expected formatted", tt.input)
		}
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		secs int
		want string
	}{
		{0, "0s"},
		{30, "30s"},
		{60, "1m"},
		{155, "2m 35s"},
	}
	for _, tt := range tests {
		got := formatDuration(tt.secs)
		if got != tt.want {
			t.Errorf("formatDuration(%d) = %q, want %q", tt.secs, got, tt.want)
		}
	}
}

func TestIsValidRunID(t *testing.T) {
	tests := []struct {
		id   string
		want bool
	}{
		{"20260315T120000Z-pr-returns-pr-42", true},
		{"", false},
		{".", false},
		{"..", false},
		{"../etc", false},
		{"foo/bar", false},
	}
	for _, tt := range tests {
		if got := isValidRunID(tt.id); got != tt.want {
			t.Errorf("isValidRunID(%q) = %v, want %v", tt.id, got, tt.want)
		}
	}
}

func TestIsValidFilename(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"prompt.txt", true},
		{"summary.json", true},
		{"", false},
		{"..", false},
		{"../foo", false},
		{"a/b", false},
	}
	for _, tt := range tests {
		if got := isValidFilename(tt.name); got != tt.want {
			t.Errorf("isValidFilename(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestMethodNotAllowed(t *testing.T) {
	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")
	os.MkdirAll(auditDir, 0o755)

	s := newTestServer(t, []Project{{Name: "r", AuditDir: auditDir, StateFile: filepath.Join(tmp, "s.json")}})
	mux := s.registerRoutes()

	// POST to a GET-only route
	req := httptest.NewRequest("POST", "/projects/r/", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", w.Code)
	}

	// GET to a POST-only route
	req = httptest.NewRequest("GET", "/projects/r/state/rerun", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", w.Code)
	}
}
