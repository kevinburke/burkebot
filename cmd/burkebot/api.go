package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kevinburke/rest"
	"github.com/kevinburke/rest/resterror"
)

// apiConfig collects everything the /api/* handlers need at request
// time. It is a value, not a pointer, so handlers don't accidentally
// mutate shared state.
type apiConfig struct {
	Tasks    []Task
	Tokens   []Token
	Prompt   promptRunnerConfig
	Projects []Project
}

func (a apiConfig) enabled() bool {
	return len(a.Tasks) > 0 && len(a.Tokens) > 0 && a.Prompt.RunnerPath != ""
}

func (a apiConfig) taskByName(name string) *Task {
	for i := range a.Tasks {
		if a.Tasks[i].Name == name {
			return &a.Tasks[i]
		}
	}
	return nil
}

func (a apiConfig) projectByName(name string) *Project {
	for i := range a.Projects {
		if a.Projects[i].Name == name {
			return &a.Projects[i]
		}
	}
	return nil
}

// taskRunResponse is the success body returned by POST /api/tasks/<name>/runs.
type taskRunResponse struct {
	RunID  string `json:"run_id"`
	Output any    `json:"output"`
}

// writeRestError encodes err as JSON with err.Status as the HTTP status.
// Used for status codes that github.com/kevinburke/rest doesn't ship a
// helper for (currently 413 and 502).
func writeRestError(w http.ResponseWriter, err *resterror.Error) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(err.Status)
	_ = json.NewEncoder(w).Encode(err)
}

// handleAPI dispatches /api/* requests. Mounted under /api/ in
// registerRoutes; called from handleRoot when the path begins with /api.
func (s *Server) handleAPI(w http.ResponseWriter, r *http.Request) {
	if !s.api.enabled() {
		rest.NotFound(w, r)
		return
	}

	suffix := strings.TrimPrefix(r.URL.Path, "/api/")
	parts := strings.Split(strings.TrimRight(suffix, "/"), "/")

	switch {
	// POST /api/tasks/<name>/runs
	case len(parts) == 3 && parts[0] == "tasks" && parts[2] == "runs":
		if r.Method != http.MethodPost {
			rest.NotAllowed(w, r)
			return
		}
		s.handleTaskRun(w, r, parts[1])
	default:
		rest.NotFound(w, r)
	}
}

// handleTaskRun authenticates the caller, materializes the request
// body's inputs into a per-run scratch directory, fires the runner
// synchronously, validates the output against the task schema, and
// returns the parsed JSON to the caller.
func (s *Server) handleTaskRun(w http.ResponseWriter, r *http.Request, taskName string) {
	task := s.api.taskByName(taskName)
	if task == nil {
		// Generic 404: callers cannot tell the difference between an
		// unknown task and a known task they aren't allowed to invoke.
		rest.NotFound(w, r)
		return
	}

	bearer := extractBearer(r.Header.Get("Authorization"))
	tok := authorizeToken(s.api.Tokens, bearer, taskName)
	if tok == nil {
		rest.NotFound(w, r)
		return
	}

	proj := s.api.projectByName(task.Project)
	if proj == nil {
		rest.ServerError(w, r, fmt.Errorf("task %q points at unknown project %q", task.Name, task.Project))
		return
	}

	body, err := readJSONBody(r)
	if err != nil {
		rest.BadRequest(w, r, &resterror.Error{
			ID:     "invalid_body",
			Title:  "Invalid request body",
			Detail: err.Error(),
		})
		return
	}

	// Validate inputs: every declared input must be present, and we
	// reject any extras up-front (no silent drop).
	declared := make(map[string]TaskInput, len(task.Inputs))
	for _, in := range task.Inputs {
		declared[in.Name] = in
	}
	for k := range body {
		if _, ok := declared[k]; !ok {
			rest.BadRequest(w, r, &resterror.Error{
				ID:     "unknown_input",
				Title:  fmt.Sprintf("unknown input %q for task %q", k, task.Name),
				Detail: fmt.Sprintf("declared inputs are: %s", strings.Join(taskInputNames(task), ", ")),
			})
			return
		}
	}
	for _, in := range task.Inputs {
		raw, ok := body[in.Name]
		if !ok {
			rest.BadRequest(w, r, &resterror.Error{
				ID:    "missing_input",
				Title: fmt.Sprintf("missing input %q", in.Name),
			})
			return
		}
		if in.MaxBytes > 0 && len(raw) > in.MaxBytes {
			writeRestError(w, &resterror.Error{
				Status: http.StatusRequestEntityTooLarge,
				ID:     "input_too_large",
				Title:  fmt.Sprintf("input %q exceeds max_bytes %d", in.Name, in.MaxBytes),
			})
			return
		}
	}

	scratchDir, err := makeScratchDir(s.api.Prompt.BotHome, task.Name)
	if err != nil {
		rest.ServerError(w, r, fmt.Errorf("scratch dir for %q: %w", task.Name, err))
		return
	}

	inputPaths := make(map[string]string, len(task.Inputs))
	for _, in := range task.Inputs {
		path := filepath.Join(scratchDir, in.Filename)
		if err := os.WriteFile(path, body[in.Name], 0o644); err != nil {
			rest.ServerError(w, r, fmt.Errorf("writing input %q for %q: %w", in.Name, task.Name, err))
			return
		}
		inputPaths[in.Name] = path
	}

	prompt, err := task.renderPrompt(inputPaths)
	if err != nil {
		rest.ServerError(w, r, fmt.Errorf("rendering prompt for %q: %w", task.Name, err))
		return
	}

	runner := s.runTask
	if runner == nil {
		runner = executeTaskRun
	}
	result, err := runner(s.logger, s.api.Prompt, *task, *tok, scratchDir, prompt)
	if err != nil {
		s.logger.Error("task run", "task", task.Name, "token", tok.Name, "error", err, "run_id", result.RunID)
		writeRestError(w, &resterror.Error{
			Status: http.StatusBadGateway,
			ID:     "runner_failed",
			Title:  "Task runner failed",
			Detail: err.Error(),
		})
		return
	}

	// Read the runner's output. burkebot-codex-run writes the model's
	// last message to last-message.txt under the audit bundle.
	// Whether the dashboard process can read it depends on how the
	// runner role lays down permissions; today they're root:burkebot
	// 0640 (set by roles/burkebot in caracal-server) so any process
	// in the burkebot group can read it.
	lastMsgPath := filepath.Join(proj.AuditDir, result.RunID, "last-message.txt")
	rawOutput, err := os.ReadFile(lastMsgPath)
	if err != nil {
		s.logger.Error("reading runner output", "task", task.Name, "run_id", result.RunID, "error", err)
		writeRestError(w, &resterror.Error{
			Status: http.StatusBadGateway,
			ID:     "runner_no_output",
			Title:  "Runner did not produce an output file",
		})
		return
	}

	parsed, err := task.validateOutput(rawOutput)
	if err != nil {
		s.logger.Error("validating output", "task", task.Name, "run_id", result.RunID, "error", err)
		writeRestError(w, &resterror.Error{
			Status: http.StatusBadGateway,
			ID:     "invalid_output",
			Title:  "Runner output failed schema validation",
			Detail: err.Error(),
		})
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(taskRunResponse{
		RunID:  result.RunID,
		Output: parsed,
	})
}

// taskInputNames returns the declared input names for a task, in
// declaration order, suitable for printing in an error detail.
func taskInputNames(t *Task) []string {
	out := make([]string, len(t.Inputs))
	for i, in := range t.Inputs {
		out[i] = in.Name
	}
	return out
}

// executeTaskRun is the production runner used by handleTaskRun. It
// validates the task's filesystem prerequisites, builds a runnerInvocation
// for an --output-schema-enforced run inside the per-run scratch dir,
// and hands it to runRunner.
//
// Tasks always run --safe (read-only sandbox); there is no caller-
// supplied flag that can widen this.
func executeTaskRun(logger *slog.Logger, cfg promptRunnerConfig, task Task, tok Token, scratchDir, prompt string) (runResult, error) {
	if info, err := os.Stat(cfg.RunnerPath); err != nil || info.IsDir() {
		return runResult{}, fmt.Errorf("runner binary %q is not available", cfg.RunnerPath)
	}
	if info, err := os.Stat(scratchDir); err != nil || !info.IsDir() {
		return runResult{}, fmt.Errorf("scratch dir %q is not available", scratchDir)
	}
	if info, err := os.Stat(task.OutputSchemaPath); err != nil || info.IsDir() {
		return runResult{}, fmt.Errorf("schema %q is not available", task.OutputSchemaPath)
	}

	res, err := runRunner(logger, runnerInvocation{
		WorkingDirectory: scratchDir,
		ReadWritePaths:   uniqueNonEmptyPaths([]string{cfg.BotHome, scratchDir}),
		Command: []string{
			cfg.RunnerPath,
			"--source", "api",
			"--label", fmt.Sprintf("%s-%s", task.Name, tok.Name),
			"--prompt-id", fmt.Sprintf("%s-%d", task.Name, len(prompt)),
			"--repo-dir", scratchDir,
			"--output-schema", task.OutputSchemaPath,
			"--safe",
		},
		Prompt:         prompt,
		TimeoutSeconds: task.TimeoutSeconds,
	})
	if err != nil {
		return res, fmt.Errorf("task %q run failed: %w", task.Name, err)
	}
	return res, nil
}

// makeScratchDir creates a per-run scratch directory under botHome.
// The dashboard server typically runs as root and the runner runs as
// the burkebot user; world-readable files in this directory are visible
// to both.
func makeScratchDir(botHome, taskName string) (string, error) {
	if botHome == "" {
		return "", errors.New("BotHome not configured")
	}
	root := filepath.Join(botHome, "api-runs")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	id, err := randomToken(8)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, fmt.Sprintf("%s-%s-%s", time.Now().UTC().Format("20060102T150405Z"), taskName, id))
	if err := os.Mkdir(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// readJSONBody decodes the request body as a JSON object whose values
// are strings — i.e. each declared input is one string field. Returns
// the values as []byte (for direct WriteFile) keyed by input name.
func readJSONBody(r *http.Request) (map[string][]byte, error) {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields() // (no top-level keys are unknown — we validate against task.Inputs separately)
	var raw map[string]string
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("body must be a JSON object of string values: %w", err)
	}
	out := make(map[string][]byte, len(raw))
	for k, v := range raw {
		out[k] = []byte(v)
	}
	return out, nil
}
