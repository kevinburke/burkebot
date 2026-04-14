package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

func TestDashboardRequiresBasicAuthWhenConfigured(t *testing.T) {
	s := newTestServer(t, []Project{{Name: "r", AuditDir: t.TempDir(), StateFile: filepath.Join(t.TempDir(), "state.json")}})
	s.auth = &basicAuthConfig{Username: "kevin", Password: "secret"}

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	s.registerRoutes().ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
	if got := w.Header().Get("WWW-Authenticate"); !strings.Contains(got, "Basic") {
		t.Fatalf("expected WWW-Authenticate header, got %q", got)
	}
}

func TestHandlePromptRequiresCSRF(t *testing.T) {
	tmp := t.TempDir()
	s := newTestServer(t, []Project{{
		Name: "r", Repo: "kevinburke/returns", AuditDir: filepath.Join(tmp, "audit"), StateFile: filepath.Join(tmp, "state.json"),
	}})
	s.csrfKey = []byte("csrf-secret")
	s.prompt = promptRunnerConfig{Enabled: true, RepoRoot: tmp}

	form := url.Values{"prompt": {"inspect the repo"}}
	req := httptest.NewRequest("POST", "/projects/r/prompt", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.registerRoutes().ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestHandlePromptRunsAndRedirectsToAudit(t *testing.T) {
	tmp := t.TempDir()
	s := newTestServer(t, []Project{{
		Name: "r", Repo: "kevinburke/returns", AuditDir: filepath.Join(tmp, "audit"), StateFile: filepath.Join(tmp, "state.json"),
	}})
	s.csrfKey = []byte("csrf-secret")
	s.prompt = promptRunnerConfig{Enabled: true, RepoRoot: tmp}

	var gotPolicy promptPolicy
	var gotPrompt string
	s.runPrompt = func(_ *slog.Logger, _ promptRunnerConfig, proj Project, policy promptPolicy, prompt string) (promptRunResult, error) {
		if proj.Name != "r" {
			t.Fatalf("unexpected project %q", proj.Name)
		}
		gotPolicy = policy
		gotPrompt = prompt
		return promptRunResult{RunID: "20260323T120000Z-adhoc-r-web-repo-write-github-safe"}, nil
	}

	secret := "csrf-cookie"
	token := s.signCSRF(secret, "/projects/r/prompt")
	form := url.Values{
		"csrf_token":         {token},
		"prompt":             {"fix the failing test"},
		"repo_write":         {"1"},
		"github_credentials": {"1"},
	}
	req := httptest.NewRequest("POST", "/projects/r/prompt", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: secret})
	w := httptest.NewRecorder()
	s.registerRoutes().ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/projects/r/audit/20260323T120000Z-adhoc-r-web-repo-write-github-safe/" {
		t.Fatalf("unexpected redirect location %q", loc)
	}
	if gotPrompt != "fix the failing test" {
		t.Fatalf("unexpected prompt %q", gotPrompt)
	}
	if !gotPolicy.RepoWrite || !gotPolicy.GitHubCredentials || gotPolicy.WorkspaceWrite || gotPolicy.Dangerous {
		t.Fatalf("unexpected policy %+v", gotPolicy)
	}
}

func TestHandlePromptRejectsInvalidPermissionCombo(t *testing.T) {
	tmp := t.TempDir()
	s := newTestServer(t, []Project{{
		Name: "r", Repo: "kevinburke/returns", AuditDir: filepath.Join(tmp, "audit"), StateFile: filepath.Join(tmp, "state.json"),
	}})
	s.csrfKey = []byte("csrf-secret")
	s.prompt = promptRunnerConfig{Enabled: true, RepoRoot: tmp}

	secret := "csrf-cookie"
	token := s.signCSRF(secret, "/projects/r/prompt")
	form := url.Values{
		"csrf_token":      {token},
		"prompt":          {"do a thing"},
		"workspace_write": {"1"},
	}
	req := httptest.NewRequest("POST", "/projects/r/prompt", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: secret})
	w := httptest.NewRecorder()
	s.registerRoutes().ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", w.Code)
	}
	if loc := w.Header().Get("Location"); !strings.Contains(loc, "workspace+writes+require+repo+edits") {
		t.Fatalf("unexpected redirect location %q", loc)
	}
}
