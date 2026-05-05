package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeTaskAPIServer wires up a Server with the API enabled, a
// mock runner that pretends codex produced canned output, and a
// project pointing at a temp audit dir. Returns the assembled Server
// and the bearer token configured on the test token.
func fakeTaskAPIServer(t *testing.T, schemaJSON, modelOutput string) (*Server, string) {
	t.Helper()

	dir := t.TempDir()
	auditDir := filepath.Join(dir, "audit")
	botHome := filepath.Join(dir, "bot-home")
	for _, p := range []string{auditDir, botHome} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	schemaPath := filepath.Join(dir, "schema.json")
	if err := os.WriteFile(schemaPath, []byte(schemaJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	tmplPath := filepath.Join(dir, "prompt.tmpl")
	if err := os.WriteFile(tmplPath, []byte("agenda={{.agenda}} transcript={{.transcript}}"), 0o644); err != nil {
		t.Fatal(err)
	}

	tasksJSON, err := json.Marshal([]Task{{
		Name:               "annotate",
		Project:            "p1",
		PromptTemplatePath: tmplPath,
		OutputSchemaPath:   schemaPath,
		Inputs: []TaskInput{
			{Name: "agenda", Filename: "agenda.txt", MaxBytes: 100},
			{Name: "transcript", Filename: "transcript.txt", MaxBytes: 1000},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	tasksPath := filepath.Join(dir, "tasks.json")
	if err := os.WriteFile(tasksPath, tasksJSON, 0o644); err != nil {
		t.Fatal(err)
	}

	tasks, err := loadTasks(tasksPath)
	if err != nil {
		t.Fatal(err)
	}

	tokens := []Token{{
		Name:         "tester",
		SHA256Hex:    sha256Hex("test-token"),
		AllowedTasks: []string{"annotate"},
	}}

	prompt := promptRunnerConfig{
		RunnerPath: "/bin/true", // nonempty so apiConfig.enabled() is true
		BotHome:    botHome,
	}

	s := &Server{
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		projects: []Project{{Name: "p1", AuditDir: auditDir}},
		prompt:   prompt,
		api: apiConfig{
			Tasks:    tasks,
			Tokens:   tokens,
			Prompt:   prompt,
			Projects: []Project{{Name: "p1", AuditDir: auditDir}},
		},
		runTask: func(_ *slog.Logger, _ promptRunnerConfig, _ Task, _ Token, _ string, _ string) (runResult, error) {
			runID := "20260504T120000Z-test"
			runDir := filepath.Join(auditDir, runID)
			if err := os.MkdirAll(runDir, 0o755); err != nil {
				return runResult{}, err
			}
			if err := os.WriteFile(filepath.Join(runDir, "last-message.txt"), []byte(modelOutput), 0o640); err != nil {
				return runResult{}, err
			}
			return runResult{RunID: runID}, nil
		},
	}
	return s, "test-token"
}

const annotationSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "required": ["meeting_summary", "items"],
  "properties": {
    "meeting_summary": {"type": "string"},
    "items": {"type": "array"}
  }
}`

func postJSON(s *Server, path, token string, body any) *httptest.ResponseRecorder {
	buf, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(buf))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	s.handleAPI(w, r)
	return w
}

func TestTaskAPI_Success(t *testing.T) {
	output := `{"meeting_summary":"hi","items":[]}`
	s, tok := fakeTaskAPIServer(t, annotationSchema, output)

	w := postJSON(s, "/api/tasks/annotate/runs", tok, map[string]string{
		"agenda":     "1. Roll Call",
		"transcript": "[00:00:01] hi",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp taskRunResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.RunID == "" {
		t.Error("empty run_id")
	}
	if resp.Output == nil {
		t.Error("nil output")
	}
}

func TestTaskAPI_AuthFailures(t *testing.T) {
	s, tok := fakeTaskAPIServer(t, annotationSchema, `{"meeting_summary":"x","items":[]}`)
	body := map[string]string{"agenda": "x", "transcript": "y"}

	cases := []struct {
		name string
		path string
		tok  string
	}{
		{"no token", "/api/tasks/annotate/runs", ""},
		{"wrong token", "/api/tasks/annotate/runs", "wrong"},
		// task exists but unknown name; should return same shape as
		// "task exists, token not authorized" — we don't differentiate.
		{"unknown task", "/api/tasks/nope/runs", tok},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := postJSON(s, c.path, c.tok, body)
			if w.Code != http.StatusNotFound {
				t.Fatalf("expected 404, got %d body=%s", w.Code, w.Body.String())
			}
		})
	}
}

func TestTaskAPI_InputValidation(t *testing.T) {
	s, tok := fakeTaskAPIServer(t, annotationSchema, `{"meeting_summary":"x","items":[]}`)

	cases := []struct {
		name     string
		body     map[string]string
		wantCode int
		wantSub  string
	}{
		{"missing input", map[string]string{"agenda": "x"}, http.StatusBadRequest, "missing input"},
		{"unknown input", map[string]string{"agenda": "x", "transcript": "y", "extra": "z"}, http.StatusBadRequest, "unknown input"},
		{"oversize", map[string]string{"agenda": strings.Repeat("a", 200), "transcript": "y"}, http.StatusRequestEntityTooLarge, "max_bytes"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := postJSON(s, "/api/tasks/annotate/runs", tok, c.body)
			if w.Code != c.wantCode {
				t.Fatalf("got %d want %d body=%s", w.Code, c.wantCode, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), c.wantSub) {
				t.Errorf("body %q does not contain %q", w.Body.String(), c.wantSub)
			}
		})
	}
}

func TestTaskAPI_SchemaValidation(t *testing.T) {
	// Model output drops the required "items" field — server must
	// catch that before returning to the caller.
	s, tok := fakeTaskAPIServer(t, annotationSchema, `{"meeting_summary":"x"}`)

	w := postJSON(s, "/api/tasks/annotate/runs", tok, map[string]string{
		"agenda":     "x",
		"transcript": "y",
	})
	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "schema") {
		t.Errorf("expected schema-validation error, got %s", w.Body.String())
	}
}
