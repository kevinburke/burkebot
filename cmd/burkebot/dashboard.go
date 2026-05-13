package main

import (
	"bytes"
	"embed"
	"flag"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

//go:embed templates/*.html
var templateFS embed.FS

const maxRawFileSize = 5 * 1024 * 1024 // 5 MB

// Server provides the web UI for Burkebot audit and state management,
// plus the task-API endpoints under /api/.
type Server struct {
	projects  []Project
	tmpl      *template.Template
	build     buildInfo
	logger    *slog.Logger
	auth      *basicAuthConfig
	csrfKey   []byte
	prompt    promptRunnerConfig
	api       apiConfig
	runPrompt func(*slog.Logger, promptRunnerConfig, Project, promptPolicy, string) (runResult, error)
	runTask   func(*slog.Logger, taskRunRequest) (runResult, error)
}

func (s *Server) projectByName(name string) *Project {
	for i := range s.projects {
		if s.projects[i].Name == name {
			return &s.projects[i]
		}
	}
	return nil
}

func runDashboard(args []string) {
	flagSet := flag.NewFlagSet("dashboard", flag.ExitOnError)
	port := flagSet.Int("port", 4872, "HTTP listen port")
	listenAddr := flagSet.String("listen-addr", "0.0.0.0", "HTTP listen address")
	auditDir := flagSet.String("audit-dir", "/var/log/burkebot/audit", "Path to audit bundles (single-project mode)")
	stateFile := flagSet.String("state-file", "/var/lib/burkebot/processed-prs.json", "Path to processed-prs.json (single-project mode)")
	dataDir := flagSet.String("data-dir", "", "Root data directory containing per-project subdirs (multi-project mode)")
	projectsFile := flagSet.String("projects", "", "Path to projects.json config file")
	certFile := flagSet.String("cert-file", "", "Path to TLS certificate file (enables HTTPS)")
	keyFile := flagSet.String("key-file", "", "Path to TLS private key file")
	authUser := flagSet.String("auth-user", "", "HTTP basic auth username")
	authPasswordFile := flagSet.String("auth-password-file", "", "Path to the HTTP basic auth password file")
	enablePromptUI := flagSet.Bool("enable-prompt-ui", false, "Enable ad hoc prompt submission UI")
	repoRoot := flagSet.String("repo-root", "/srv/burkebot", "Default repo checkout root for prompt submissions")
	promptRunner := flagSet.String("prompt-runner", "/usr/local/bin/burkebot-codex-run", "Path to the burkebot prompt runner")
	envdirBinary := flagSet.String("envdir-binary", "/opt/burkebot/bin/envdir", "Path to envdir for GitHub-authenticated prompt runs")
	envDir := flagSet.String("env-dir", "/opt/burkebot/env", "Envdir directory used for GitHub-authenticated prompt runs")
	botHome := flagSet.String("bot-home", "/home/burkebot", "Home directory for the burkebot user")
	botUser := flagSet.String("bot-user", "burkebot", "Name of the unprivileged user the runner script drops to via runuser; per-task-API job dirs are chowned to this user")
	botGroup := flagSet.String("bot-group", "burkebot", "Group for the unprivileged user (matches --bot-user by default)")
	codexAuthDir := flagSet.String("codex-auth-dir", "/var/lib/burkebot/codex-auth", "Directory holding the persistent codex auth.json; mounted writable into task-API runs so the runner can refresh the token")
	tasksFile := flagSet.String("tasks-file", "", "Path to tasks.json (enables the /api task endpoints)")
	tokensFile := flagSet.String("tokens-file", "", "Path to tokens.json (required when --tasks-file is set)")
	showVersion := flagSet.Bool("version", false, "Print version and exit")
	flagSet.Parse(args)

	if *showVersion {
		fmt.Fprintf(os.Stderr, "burkebot version %s\n", Version)
		os.Exit(0)
	}

	logger := slog.Default()
	bi := readBuildInfo()
	var auth *basicAuthConfig
	if *authUser != "" || *authPasswordFile != "" {
		if *authUser == "" || *authPasswordFile == "" {
			logger.Error("both --auth-user and --auth-password-file are required together")
			os.Exit(1)
		}
		password, err := readSecretFile(*authPasswordFile)
		if err != nil {
			logger.Error("failed to read auth password", "error", err, "path", *authPasswordFile)
			os.Exit(1)
		}
		auth = &basicAuthConfig{
			Username: *authUser,
			Password: password,
		}
	}

	var csrfKey []byte
	if auth != nil {
		secret, err := randomToken(32)
		if err != nil {
			logger.Error("failed to initialize csrf key", "error", err)
			os.Exit(1)
		}
		csrfKey = []byte(secret)
	}
	if *enablePromptUI && auth == nil {
		logger.Error("prompt UI requires --auth-user and --auth-password-file")
		os.Exit(1)
	}

	// Resolve projects.
	var projects []Project
	var err error
	if *projectsFile != "" {
		projects, err = loadProjects(*projectsFile)
		if err != nil {
			logger.Error("failed to load projects config", "error", err, "path", *projectsFile)
			os.Exit(1)
		}
	} else if *dataDir != "" {
		projects, err = discoverProjects(*dataDir)
		if err != nil {
			logger.Error("failed to discover projects", "error", err, "data_dir", *dataDir)
			os.Exit(1)
		}
	}
	if len(projects) == 0 {
		// Single-project fallback.
		projects = []Project{{
			Name:      "default",
			AuditDir:  *auditDir,
			StateFile: *stateFile,
		}}
	}

	for _, p := range projects {
		logger.Info("loaded project", "name", p.Name, "audit_dir", p.AuditDir, "state_file", p.StateFile)
	}

	tmpl, err := buildTemplates(bi)
	if err != nil {
		logger.Error("failed to parse templates", "error", err)
		os.Exit(1)
	}

	// Task API: requires both files. Mismatched config is a startup
	// error rather than a half-enabled API that returns 401 forever.
	if (*tasksFile == "") != (*tokensFile == "") {
		logger.Error("--tasks-file and --tokens-file must be set together")
		os.Exit(1)
	}
	tasks, err := loadTasks(*tasksFile)
	if err != nil {
		logger.Error("failed to load tasks", "error", err, "path", *tasksFile)
		os.Exit(1)
	}
	tokens, err := loadTokens(*tokensFile)
	if err != nil {
		logger.Error("failed to load tokens", "error", err, "path", *tokensFile)
		os.Exit(1)
	}
	for _, t := range tasks {
		logger.Info("loaded task", "name", t.Name, "project", t.Project, "inputs", len(t.Inputs))
	}
	logger.Info("loaded tokens", "count", len(tokens))

	prompt := promptRunnerConfig{
		Enabled:      *enablePromptUI,
		RepoRoot:     *repoRoot,
		RunnerPath:   *promptRunner,
		EnvdirBinary: *envdirBinary,
		EnvDir:       *envDir,
		BotHome:      *botHome,
		BotUser:      *botUser,
		BotGroup:     *botGroup,
		CodexAuthDir: *codexAuthDir,
	}

	s := &Server{
		projects: projects,
		tmpl:     tmpl,
		build:    bi,
		logger:   logger,
		auth:     auth,
		csrfKey:  csrfKey,
		prompt:   prompt,
		api: apiConfig{
			Tasks:    tasks,
			Tokens:   tokens,
			Prompt:   prompt,
			Projects: projects,
		},
	}

	mux := s.registerRoutes()

	addr := net.JoinHostPort(*listenAddr, strconv.Itoa(*port))
	if *certFile != "" && *keyFile != "" {
		logger.Info("starting burkebot dashboard", "addr", "https://"+addr, "projects", len(projects))
		if err := http.ListenAndServeTLS(addr, *certFile, *keyFile, mux); err != nil {
			logger.Error("server error", "error", err)
			os.Exit(1)
		}
	} else {
		logger.Info("starting burkebot dashboard", "addr", "http://"+addr, "projects", len(projects))
		if err := http.ListenAndServe(addr, mux); err != nil {
			logger.Error("server error", "error", err)
			os.Exit(1)
		}
	}
}

// buildTemplates parses the embedded HTML templates with the standard funcmap.
func buildTemplates(bi buildInfo) (*template.Template, error) {
	funcMap := template.FuncMap{
		"timeAgo":        timeAgo,
		"formatDuration": formatDuration,
		"exitCodeClass": func(code int) string {
			if code == 0 {
				return "success"
			}
			return "failure"
		},
		"truncate": func(s string, n int) string {
			if len(s) <= n {
				return s
			}
			return s[:n] + "..."
		},
		"runTime":   runTime,
		"extractPR": extractPRNumber,
		"formatSize": func(size int64) string {
			switch {
			case size < 1024:
				return fmt.Sprintf("%d B", size)
			case size < 1024*1024:
				return fmt.Sprintf("%.1f KB", float64(size)/1024)
			default:
				return fmt.Sprintf("%.1f MB", float64(size)/(1024*1024))
			}
		},
		"now":           func() time.Time { return time.Now() },
		"serverVersion": func() string { return bi.Version },
		"gitCommit": func() string {
			if bi.Commit == "" {
				return "dev"
			}
			short := bi.Commit
			if len(short) > 8 {
				short = short[:8]
			}
			if bi.Modified {
				short += "-dirty"
			}
			return short
		},
		"buildDate": func() string {
			if bi.BuildDate == "" {
				return "unknown"
			}
			return bi.BuildDate
		},
		"renderDurationMs": func(start time.Time) string {
			return fmt.Sprintf("%.1f", float64(time.Since(start).Microseconds())/1000.0)
		},
	}

	tplFS, err := fs.Sub(templateFS, "templates")
	if err != nil {
		return nil, err
	}
	return template.New("").Funcs(funcMap).ParseFS(tplFS, "*.html")
}

// registerRoutes sets up the HTTP mux for the dashboard using classic
// path-prefix routing (compatible with all Go versions).
func (s *Server) registerRoutes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleRoot)
	return mux
}

// handleRoot is the single entry point that dispatches to sub-handlers based
// on the URL path. We parse the path manually instead of relying on Go 1.22+
// enhanced routing patterns, which are not working reliably with Go tip.
func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	// /api/ uses bearer-token auth, not the dashboard's basic auth.
	// Route it before s.authenticate so an API caller doesn't see a
	// basic-auth challenge they can't satisfy.
	if strings.HasPrefix(r.URL.Path, "/api/") {
		s.handleAPI(w, r)
		return
	}

	if !s.authenticate(w, r) {
		return
	}

	path := strings.TrimRight(r.URL.Path, "/")

	// GET /
	if path == "" {
		if r.Method != http.MethodGet {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleIndex(w, r)
		return
	}

	// All other routes start with /projects/<project>/...
	// Split: ["", "projects", project, ...]
	parts := strings.Split(path, "/")
	if len(parts) < 3 || parts[1] != "projects" {
		http.NotFound(w, r)
		return
	}

	projectName := parts[2]
	proj := s.projectByName(projectName)
	if proj == nil {
		http.NotFound(w, r)
		return
	}

	rest := parts[3:] // path segments after /p/<project>

	switch {
	// GET /p/<project>
	case len(rest) == 0:
		if r.Method != http.MethodGet {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleProject(w, r, proj)

	// GET /p/<project>/state
	case len(rest) == 1 && rest[0] == "state":
		if r.Method != http.MethodGet {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleState(w, r, proj)

	// POST /projects/<project>/prompt
	case len(rest) == 1 && rest[0] == "prompt":
		if r.Method != http.MethodPost {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handlePrompt(w, r, proj)

	// POST /p/<project>/state/rerun
	case len(rest) == 2 && rest[0] == "state" && rest[1] == "rerun":
		if r.Method != http.MethodPost {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleRerun(w, r, proj)

	// POST /p/<project>/state/rerun-all
	case len(rest) == 2 && rest[0] == "state" && rest[1] == "rerun-all":
		if r.Method != http.MethodPost {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleRerunAll(w, r, proj)

	// GET /p/<project>/audit/<run_id>
	case len(rest) == 2 && rest[0] == "audit":
		if r.Method != http.MethodGet {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleAuditDetail(w, r, proj, rest[1])

	// GET /p/<project>/audit/<run_id>/<file>
	case len(rest) == 3 && rest[0] == "audit":
		if r.Method != http.MethodGet {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleAuditFile(w, r, proj, rest[1], rest[2])

	default:
		http.NotFound(w, r)
	}
}

// render executes a template to a buffer, then writes to w.
func (s *Server) render(w http.ResponseWriter, name string, data any) {
	buf := new(bytes.Buffer)
	if err := s.tmpl.ExecuteTemplate(buf, name, data); err != nil {
		s.logger.Error("template error", "template", name, "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(buf.Bytes())
}

type indexData struct {
	pageContext
	Projects []Project
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	// If there's only one project, redirect straight to it.
	if len(s.projects) == 1 {
		http.Redirect(w, r, "/projects/"+s.projects[0].Name+"/", http.StatusFound)
		return
	}
	s.render(w, "index.html", indexData{
		pageContext: newPageContext(),
		Projects:    s.projects,
	})
}

type projectData struct {
	pageContext
	Project         Project
	Projects        []Project
	Runs            []AuditBundle
	State           ProcessedPRState
	Message         string
	IsError         bool
	PromptEnabled   bool
	PromptCSRFToken string
}

func (s *Server) handleProject(w http.ResponseWriter, r *http.Request, proj *Project) {
	runs, err := loadAuditRuns(proj.AuditDir, proj.Name)
	if err != nil {
		s.logger.Error("failed to load audit runs", "error", err, "project", proj.Name)
		http.Error(w, "Failed to load audit runs", http.StatusInternalServerError)
		return
	}
	if len(runs) > 100 {
		runs = runs[:100]
	}

	state, err := loadProcessedPRs(proj.StateFile)
	if err != nil {
		s.logger.Error("failed to load state", "error", err, "project", proj.Name)
		state = make(ProcessedPRState)
	}

	s.render(w, "project.html", projectData{
		pageContext:     newPageContext(),
		Project:         *proj,
		Projects:        s.projects,
		Runs:            runs,
		State:           state,
		Message:         r.URL.Query().Get("message"),
		IsError:         r.URL.Query().Get("error") == "1",
		PromptEnabled:   s.prompt.Enabled,
		PromptCSRFToken: s.csrfToken(w, r, "/projects/"+proj.Name+"/prompt"),
	})
}

type auditDetailData struct {
	pageContext
	Project        Project
	Projects       []Project
	Bundle         AuditBundle
	Prompt         string
	LastMessage    string
	Commands       []Command
	CodexCommands  []CodexCommand
	AgentMessage   string
	Stderr         string
	RerunCSRFToken string
}

func (s *Server) handleAuditDetail(w http.ResponseWriter, r *http.Request, proj *Project, runID string) {
	if !isValidRunID(runID) {
		http.NotFound(w, r)
		return
	}

	bundle, err := loadBundle(proj.AuditDir, runID)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	data := auditDetailData{
		pageContext:    newPageContext(),
		Project:        *proj,
		Projects:       s.projects,
		Bundle:         *bundle,
		RerunCSRFToken: s.csrfToken(w, r, "/projects/"+proj.Name+"/state/rerun"),
	}

	dir := filepath.Join(proj.AuditDir, runID)
	// Read the runner's output. burkebot-codex-run owns audit files as
	// root with group-readable modes; whether the dashboard can read them
	// depends on the deployed audit directory group.
	data.Prompt = readFileString(filepath.Join(dir, "prompt.txt"))
	data.LastMessage = readFileString(filepath.Join(dir, "last-message.txt"))
	data.Stderr = readFileString(filepath.Join(dir, "codex-stderr.log"))

	commands, _ := loadCommands(filepath.Join(dir, "commands.jsonl"))
	data.Commands = commands

	codexCmds, agentMsg, _ := loadCodexEvents(filepath.Join(dir, "codex-events.jsonl"))
	data.CodexCommands = codexCmds
	data.AgentMessage = agentMsg

	s.render(w, "audit.html", data)
}

func (s *Server) handleAuditFile(w http.ResponseWriter, r *http.Request, proj *Project, runID, file string) {
	if !isValidRunID(runID) || !isValidFilename(file) {
		http.NotFound(w, r)
		return
	}

	path := filepath.Join(proj.AuditDir, runID, file)

	absPath, err := filepath.Abs(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	absAudit, err := filepath.Abs(proj.AuditDir)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if !strings.HasPrefix(absPath, absAudit+string(filepath.Separator)) {
		http.NotFound(w, r)
		return
	}

	f, err := os.Open(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if info.Size() > maxRawFileSize {
		w.Header().Set("X-Truncated", "true")
		io.Copy(w, io.LimitReader(f, maxRawFileSize))
		fmt.Fprintf(w, "\n\n--- truncated at %d bytes (file is %d bytes) ---\n", maxRawFileSize, info.Size())
		return
	}
	io.Copy(w, f)
}

type stateData struct {
	pageContext
	Project           Project
	Projects          []Project
	State             ProcessedPRState
	Message           string
	IsError           bool
	RerunCSRFToken    string
	RerunAllCSRFToken string
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request, proj *Project) {
	state, err := loadProcessedPRs(proj.StateFile)
	if err != nil {
		s.logger.Error("failed to load state", "error", err, "project", proj.Name)
		http.Error(w, "Failed to load state", http.StatusInternalServerError)
		return
	}

	s.render(w, "state.html", stateData{
		pageContext:       newPageContext(),
		Project:           *proj,
		Projects:          s.projects,
		State:             state,
		Message:           r.URL.Query().Get("message"),
		IsError:           r.URL.Query().Get("error") == "1",
		RerunCSRFToken:    s.csrfToken(w, r, "/projects/"+proj.Name+"/state/rerun"),
		RerunAllCSRFToken: s.csrfToken(w, r, "/projects/"+proj.Name+"/state/rerun-all"),
	})
}

func (s *Server) handleRerun(w http.ResponseWriter, r *http.Request, proj *Project) {
	base := "/projects/" + proj.Name + "/state"
	if !s.verifyCSRF(w, r, "/projects/"+proj.Name+"/state/rerun") {
		return
	}
	pr := strings.TrimSpace(r.FormValue("pr"))
	if pr == "" {
		http.Redirect(w, r, base+"?message=Missing+PR+number&error=1", http.StatusSeeOther)
		return
	}

	state, err := loadProcessedPRs(proj.StateFile)
	if err != nil {
		s.logger.Error("failed to load state", "error", err, "project", proj.Name)
		http.Redirect(w, r, base+"?message=Failed+to+load+state&error=1", http.StatusSeeOther)
		return
	}

	msg := "Started+" + proj.ServiceName()
	if _, ok := state[pr]; ok {
		delete(state, pr)
		if err := saveProcessedPRs(proj.StateFile, state); err != nil {
			s.logger.Error("failed to save state", "error", err, "project", proj.Name)
			http.Redirect(w, r, base+"?message=Failed+to+save+state&error=1", http.StatusSeeOther)
			return
		}
		msg = "Removed+PR+" + pr + "+and+started+" + proj.ServiceName()
	}

	if err := startBurkebotService(s.logger, proj.ServiceName()); err != nil {
		http.Redirect(w, r, base+"?message=Service+start+failed&error=1", http.StatusSeeOther)
		return
	}

	http.Redirect(w, r, base+"?message="+msg, http.StatusSeeOther)
}

func (s *Server) handleRerunAll(w http.ResponseWriter, r *http.Request, proj *Project) {
	base := "/projects/" + proj.Name + "/state"
	if !s.verifyCSRF(w, r, "/projects/"+proj.Name+"/state/rerun-all") {
		return
	}
	if err := saveProcessedPRs(proj.StateFile, make(ProcessedPRState)); err != nil {
		s.logger.Error("failed to save state", "error", err, "project", proj.Name)
		http.Redirect(w, r, base+"?message=Failed+to+reset+state&error=1", http.StatusSeeOther)
		return
	}

	if err := startBurkebotService(s.logger, proj.ServiceName()); err != nil {
		http.Redirect(w, r, base+"?message=State+reset+but+service+start+failed&error=1", http.StatusSeeOther)
		return
	}

	http.Redirect(w, r, base+"?message=Reset+all+state+and+started+"+proj.ServiceName(), http.StatusSeeOther)
}

// readFileString reads a file into a string, returning "" on error.
func readFileString(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

// isValidRunID checks that a run ID contains no path separators or ".." sequences.
func isValidRunID(id string) bool {
	if id == "" || id == "." || id == ".." {
		return false
	}
	return !strings.Contains(id, "/") && !strings.Contains(id, "\\") && !strings.Contains(id, "..")
}

// isValidFilename checks that a filename has no path separators or ".." sequences.
func isValidFilename(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	return !strings.Contains(name, "/") && !strings.Contains(name, "\\") && !strings.Contains(name, "..")
}

// extractPRNumber returns the trailing PR number from a RunID like
// "20260317T070738Z-pr-go-html-boilerplate-pr-10". Returns "" if no
// trailing -<digits> is found.
func extractPRNumber(runID string) string {
	i := strings.LastIndex(runID, "-")
	if i < 0 || i == len(runID)-1 {
		return ""
	}
	suffix := runID[i+1:]
	for _, c := range suffix {
		if c < '0' || c > '9' {
			return ""
		}
	}
	return suffix
}

// parseRunIDTime extracts the UTC timestamp prefix from a RunID like
// "20260317T070738Z-pr-go-html-boilerplate-pr-11".
func parseRunIDTime(runID string) (time.Time, bool) {
	// The timestamp prefix is always 16 chars: "20060102T150405Z"
	if len(runID) < 16 {
		return time.Time{}, false
	}
	t, err := time.Parse("20060102T150405Z", runID[:16])
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// runTime formats a RunID's embedded timestamp as a local date/time string
// with a relative-time suffix, e.g. "Mar 17, 2026 3:07 AM (2 hours ago)".
func runTime(runID string) string {
	t, ok := parseRunIDTime(runID)
	if !ok {
		return runID
	}
	local := t.Local()
	abs := local.Format("Jan 2, 2006 3:04 PM")
	d := time.Since(t)
	var rel string
	switch {
	case d < time.Minute:
		rel = "just now"
	case d < time.Hour:
		m := int(d.Minutes())
		if m == 1 {
			rel = "1 minute ago"
		} else {
			rel = fmt.Sprintf("%d minutes ago", m)
		}
	case d < 24*time.Hour:
		h := int(d.Hours())
		if h == 1 {
			rel = "1 hour ago"
		} else {
			rel = fmt.Sprintf("%d hours ago", h)
		}
	case d < 7*24*time.Hour:
		days := int(d.Hours()) / 24
		if days == 1 {
			rel = "1 day ago"
		} else {
			rel = fmt.Sprintf("%d days ago", days)
		}
	default:
		rel = ""
	}
	if rel != "" {
		return abs + " (" + rel + ")"
	}
	return abs
}

// timeAgo returns a human-readable relative time string.
func timeAgo(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		// Try the format used in summary.json.
		t, err = time.Parse("2006-01-02T15:04:05Z", s)
		if err != nil {
			return s
		}
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		m := int(d.Minutes())
		if m == 1 {
			return "1 minute ago"
		}
		return fmt.Sprintf("%d minutes ago", m)
	case d < 24*time.Hour:
		h := int(d.Hours())
		if h == 1 {
			return "1 hour ago"
		}
		return fmt.Sprintf("%d hours ago", h)
	case d < 7*24*time.Hour:
		days := int(d.Hours()) / 24
		if days == 1 {
			return "1 day ago"
		}
		return fmt.Sprintf("%d days ago", days)
	default:
		return t.Format("Jan 2, 2006")
	}
}

// formatDuration formats seconds into a human-readable duration.
func formatDuration(seconds int) string {
	if seconds < 60 {
		return fmt.Sprintf("%ds", seconds)
	}
	m := seconds / 60
	s := seconds % 60
	if s == 0 {
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%dm %ds", m, s)
}
