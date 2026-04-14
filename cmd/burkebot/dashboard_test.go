package main

import (
	"encoding/json"
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

func TestHandleProject(t *testing.T) {
	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")
	stateFile := filepath.Join(tmp, "state.json")
	os.WriteFile(stateFile, []byte(`{"42":"abc123"}`), 0o644)
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
	os.WriteFile(stateFile, []byte(`{"5":"sha5","9":"sha9"}`), 0o644)

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
	os.WriteFile(stateFile, []byte(`{"5":"sha5","9":"sha9"}`), 0o644)

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
	os.WriteFile(stateFile, []byte(`{"5":"sha5","9":"sha9"}`), 0o644)

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

	state := ProcessedPRState{"1": "aaa", "2": "bbb"}
	if err := saveProcessedPRs(path, state); err != nil {
		t.Fatal(err)
	}

	loaded, err := loadProcessedPRs(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded["1"] != "aaa" || loaded["2"] != "bbb" {
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
