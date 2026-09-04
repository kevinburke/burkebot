package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
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

	jobDir, repoDir, err := makeJobDir(s.api.Prompt.JobsDir, s.api.Prompt.BotUser, s.api.Prompt.BotGroup, task.Name)
	if err != nil {
		rest.ServerError(w, r, fmt.Errorf("job dir for %q: %w", task.Name, err))
		return
	}

	inputPaths := make(map[string]string, len(task.Inputs))
	for _, in := range task.Inputs {
		path := filepath.Join(repoDir, in.Filename)
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
	result, err := runner(s.logger, taskRunRequest{
		Cfg:     s.api.Prompt,
		Task:    *task,
		Token:   *tok,
		Project: *proj,
		JobDir:  jobDir,
		RepoDir: repoDir,
		Prompt:  prompt,
	})
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

	// Before trusting anything the model said, check whether it was
	// able to do anything at all. Codex reports a failure to spawn its
	// tooling as an `error` item in the event stream and then finishes
	// the turn normally, exiting 0 -- the model answers from the prompt
	// alone and says so politely in prose that still validates against
	// the task schema. That is not a hypothetical: it is what published
	// five meeting pages whose summary was an apology for not being
	// able to read their own agenda, and what left them there for four
	// days because every layer below this one reported success.
	//
	// The runner script performs the same check and exits non-zero, so
	// in production this rarely fires. It is duplicated here on purpose:
	// the runner lives in a different repository and is the thing being
	// guarded against, and this handler is the one place that turns a
	// codex run into an HTTP 200 that another service will act on.
	runDir := filepath.Join(proj.AuditDir, result.RunID)
	eventsPath := filepath.Join(runDir, "codex-events.jsonl")
	if _, err := os.Stat(eventsPath); err != nil {
		s.logger.Error("run produced no event stream", "task", task.Name, "run_id", result.RunID, "error", err)
		writeRestError(w, &resterror.Error{
			Status: http.StatusBadGateway,
			ID:     "runner_no_events",
			Title:  "Runner produced no codex event stream",
			Detail: "cannot verify the run succeeded without codex-events.jsonl",
		})
		return
	}
	codexErrors, err := loadCodexErrors(eventsPath)
	if err != nil {
		s.logger.Error("reading codex events", "task", task.Name, "run_id", result.RunID, "error", err)
		writeRestError(w, &resterror.Error{
			Status: http.StatusBadGateway,
			ID:     "runner_events_unreadable",
			Title:  "Could not read the codex event stream",
			Detail: err.Error(),
		})
		return
	}
	if len(codexErrors) > 0 {
		s.logger.Error("codex reported errors during the run",
			"task", task.Name,
			"run_id", result.RunID,
			"errors", strings.Join(codexErrors, "; "),
		)
		writeRestError(w, &resterror.Error{
			Status: http.StatusBadGateway,
			ID:     "codex_run_error",
			Title:  "Codex reported errors during the run",
			Detail: strings.Join(codexErrors, "; "),
		})
		return
	}

	// Read the runner's output. burkebot-codex-run writes the model's
	// last message to last-message.txt under the audit bundle.
	// Whether the dashboard process can read it depends on how the
	// runner role lays down permissions; today they're root:burkebot
	// 0640 (set by roles/burkebot in caracal-server) so any process
	// in the burkebot group can read it.
	lastMsgPath := filepath.Join(runDir, "last-message.txt")
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

// taskRunRequest bundles the per-call inputs handleTaskRun hands to
// executeTaskRun (or to a test seam). Grouping them into a struct
// keeps the function signature stable as the task-API plumbing grows
// — the prior positional form had crept up to eight parameters.
type taskRunRequest struct {
	Cfg     promptRunnerConfig
	Task    Task
	Token   Token
	Project Project

	// JobDir / RepoDir are the layout produced by makeJobDir: JobDir
	// is the runner script's --job-dir (contains home/, tmp/, cache/,
	// codex/, repo/); RepoDir == JobDir/repo and is the agent's --cd
	// target, holding the input files and a `git init`-ed .git so
	// codex's trusted-directory check passes.
	JobDir  string
	RepoDir string

	// Prompt is the rendered template fed to the runner on stdin.
	Prompt string
}

// executeTaskRun is the production runner used by handleTaskRun. It
// validates the request's filesystem prerequisites, builds a
// runnerInvocation for an --output-schema-enforced run inside the
// per-run job dir, and hands it to runRunner.
//
// The runner script's --job-dir branch uses req.JobDir to set up a
// clean CODEX_HOME under JobDir/codex/, seeded from
// req.Cfg.CodexAuthDir/auth.json so codex has valid OpenAI auth.
// After the run, a (possibly refreshed) auth.json is copied back to
// CodexAuthDir atomically.
//
// req.Project.AuditDir (typically /var/log/burkebot/audit) and
// req.Cfg.CodexAuthDir (typically /var/lib/burkebot/codex-auth) are
// added to ReadWritePaths because the runner script writes per-run
// audit bundles under the former and refreshes auth.json in the
// latter. This matches the regular burkebot-run service's setup.
//
// Tasks always run --safe (read-only sandbox); there is no caller-
// supplied flag that can widen this.
func executeTaskRun(logger *slog.Logger, req taskRunRequest) (runResult, error) {
	if info, err := os.Stat(req.Cfg.RunnerPath); err != nil || info.IsDir() {
		return runResult{}, fmt.Errorf("runner binary %q is not available", req.Cfg.RunnerPath)
	}
	if info, err := os.Stat(req.JobDir); err != nil || !info.IsDir() {
		return runResult{}, fmt.Errorf("job dir %q is not available", req.JobDir)
	}
	if info, err := os.Stat(req.RepoDir); err != nil || !info.IsDir() {
		return runResult{}, fmt.Errorf("repo dir %q is not available", req.RepoDir)
	}
	if info, err := os.Stat(req.Project.AuditDir); err != nil || !info.IsDir() {
		return runResult{}, fmt.Errorf("audit dir %q is not available", req.Project.AuditDir)
	}
	if req.Cfg.CodexAuthDir == "" {
		return runResult{}, errors.New("CodexAuthDir not configured (pass --codex-auth-dir to the dashboard)")
	}
	if info, err := os.Stat(req.Cfg.CodexAuthDir); err != nil || !info.IsDir() {
		return runResult{}, fmt.Errorf("codex auth dir %q is not available", req.Cfg.CodexAuthDir)
	}
	if info, err := os.Stat(req.Task.OutputSchemaPath); err != nil || info.IsDir() {
		return runResult{}, fmt.Errorf("schema %q is not available", req.Task.OutputSchemaPath)
	}

	res, err := runRunner(logger, runnerInvocation{
		WorkingDirectory: req.JobDir,
		ReadWritePaths:   uniqueNonEmptyPaths([]string{req.JobDir, req.Project.AuditDir, req.Cfg.CodexAuthDir}),
		Command: []string{
			req.Cfg.RunnerPath,
			"--source", "api",
			"--label", fmt.Sprintf("%s-%s", req.Task.Name, req.Token.Name),
			"--prompt-id", fmt.Sprintf("%s-%d", req.Task.Name, len(req.Prompt)),
			"--repo-dir", req.RepoDir,
			"--job-dir", req.JobDir,
			"--output-schema", req.Task.OutputSchemaPath,
			"--safe",
		},
		Prompt:         req.Prompt,
		TimeoutSeconds: req.Task.TimeoutSeconds,
	})
	if err != nil {
		return res, fmt.Errorf("task %q run failed: %w", req.Task.Name, err)
	}
	return res, nil
}

// makeJobDir creates a per-run job directory under jobsDir with the
// layout the runner script expects when invoked with --job-dir:
//
//	<jobsDir>/<run-id>/
//	  home/   — HOME for the agent process (writable)
//	  tmp/    — TMPDIR
//	  cache/  — Go module/build caches (mostly unused for the
//	            annotation task, but burkebot-codex-run sets
//	            GOCACHE/GOMODCACHE here unconditionally)
//	  repo/   — the agent's --cd target. Input files are written
//	            here; git-init'd so codex's trusted-directory
//	            check passes.
//
// jobsDir must NOT be a subdirectory of /home or /srv/burkebot —
// both are denyRead'd inside the srt sandbox the runner wraps codex
// in, and that blanket deny wins over the per-job allowWrite
// entries. The canonical layout points jobsDir at /srv/burkebot-jobs
// (sibling of /srv/burkebot), which matches burkebot-run's pattern.
//
// The "repo" name matches burkebot-run's per-job convention; for the
// task API it's a misnomer (no remote, no commits, no actual git
// workflow) — keeping it aligned is a deliberate near-term choice,
// since aliasing it would mean rewriting the runner script too. If
// future tasks involve multi-repo or no-repo workflows we'll
// reconsider.
//
// Returns (jobDir, repoDir, error). The runner takes jobDir via
// --job-dir; the agent --cd's to repoDir via the runner's --repo-dir
// flag.
//
// The dashboard runs as root; everything is chowned to the burkebot
// user since the runner runs the agent as burkebot via runuser and
// needs to read/write these dirs.
func makeJobDir(jobsDir, botUser, botGroup, taskName string) (jobDir, repoDir string, err error) {
	if jobsDir == "" {
		return "", "", errors.New("JobsDir not configured (pass --jobs-dir to the dashboard)")
	}
	if err := os.MkdirAll(jobsDir, 0o755); err != nil {
		return "", "", err
	}
	id, err := randomToken(8)
	if err != nil {
		return "", "", err
	}
	jobDir = filepath.Join(jobsDir, fmt.Sprintf("%s-%s-%s", time.Now().UTC().Format("20060102T150405Z"), taskName, id))
	repoDir = filepath.Join(jobDir, "repo")
	for _, d := range []string{jobDir, filepath.Join(jobDir, "home"), filepath.Join(jobDir, "tmp"), filepath.Join(jobDir, "cache"), repoDir} {
		if err := os.Mkdir(d, 0o700); err != nil {
			return "", "", fmt.Errorf("mkdir %q: %w", d, err)
		}
	}
	// chown -R to the burkebot user so the agent (running as burkebot
	// via runuser inside the sandbox) can write to its HOME/TMPDIR/etc.
	if botUser != "" {
		if err := chownRecursive(jobDir, botUser, botGroup); err != nil {
			return "", "", err
		}
	}
	// git init the repo subdir so codex's trusted-directory check
	// passes when it --cd's there.
	cmd := exec.Command("git", "init", "--quiet", repoDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", "", fmt.Errorf("git init %q: %w (%s)", repoDir, err, strings.TrimSpace(string(out)))
	}
	return jobDir, repoDir, nil
}

// chownRecursive chowns path and every entry under it to user:group.
// Uses /usr/bin/chown so name→uid resolution happens once at the
// process boundary; pure Go would need user.Lookup plus a filepath.Walk.
func chownRecursive(path, user, group string) error {
	owner := user
	if group != "" {
		owner = user + ":" + group
	}
	cmd := exec.Command("chown", "-R", owner, path)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("chown %q to %s: %w (%s)", path, owner, err, strings.TrimSpace(string(out)))
	}
	return nil
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
