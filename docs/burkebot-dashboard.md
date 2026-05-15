# Burkebot Dashboard

Web UI for browsing Codex audit trails, managing processed-PR state, and
submitting audited ad hoc prompts. Also hosts the [task API](burkebot-task-api.md)
for external services to invoke server-defined codex tasks; see
[burkebot-tasks.md](burkebot-tasks.md) for how to author a task.

## Building

```bash
mkdir -p tmp
GOOS=linux GOARCH=amd64 go build -trimpath -o tmp/burkebot ./cmd/burkebot
```

Or from the `caracal-server` Ansible repo:

```bash
make build-burkebot
```

The binary has zero external dependencies (stdlib only).

**Note:** With Go 1.27-devel, you may need `GO111MODULE=on` for the Go 1.22+
enhanced routing to work.

## Running

### Single-project mode

```bash
burkebot dashboard \
  --audit-dir /var/log/burkebot/audit \
  --state-file /var/lib/burkebot/processed-prs.json \
  --port 4872
```

These are the defaults, so `burkebot dashboard` with no flags works on the
Burkebot VM.

### Multi-project mode (auto-discover)

```bash
burkebot dashboard --data-dir /var/lib/burkebot
```

Discovers projects from subdirectories that contain an `audit/` subdir:

```
/var/lib/burkebot/
  returns/
    audit/
    processed-prs.json
  finance/
    audit/
    processed-prs.json
```

### Multi-project mode (explicit config)

```bash
burkebot dashboard --projects /etc/burkebot/projects.json
```

Where `projects.json` looks like:

```json
[
  {
    "name": "returns",
    "repo": "kevinburke/returns",
    "audit_dir": "/var/log/burkebot/returns/audit",
    "state_file": "/var/lib/burkebot/returns/processed-prs.json",
    "service": "burkebot-returns.service"
  },
  {
    "name": "finance",
    "repo": "kevinburke/finance-automations",
    "audit_dir": "/var/log/burkebot/finance/audit",
    "state_file": "/var/lib/burkebot/finance/processed-prs.json",
    "service": "burkebot-finance.service"
  }
]
```

All fields except `name` and `audit_dir` are optional. `service` defaults to
`burkebot.service`. `repo_dir` is optional; if omitted, the prompt UI derives
it from `repo` and `--repo-root`.

## Auth

If you want any write actions exposed in the dashboard, configure HTTP basic
auth:

```bash
burkebot dashboard \
  --projects /etc/burkebot/projects.json \
  --auth-user kevin \
  --auth-password-file /opt/burkebot/dashboard-password
```

When auth is enabled, all POST actions also require a CSRF token issued by the
server.

## Prompt UI

Enable the project-scoped prompt form with:

```bash
burkebot dashboard \
  --projects /etc/burkebot/projects.json \
  --auth-user kevin \
  --auth-password-file /opt/burkebot/dashboard-password \
  --enable-prompt-ui
```

The prompt form is project-scoped and writes every run into the normal audit
bundle flow. The UI exposes four server-enforced capability toggles:

- `Allow repo edits`
- `Allow wider Burkebot writes`
- `Expose GitHub credentials`
- `Allow dangerous Codex mode`

These toggles do not pass arbitrary CLI flags through from the browser. The
server maps them onto a fixed `systemd-run` sandbox policy and `burkebot-codex-run`
invocation.

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--port` | 4872 | HTTP listen port |
| `--listen-addr` | 0.0.0.0 | HTTP listen address |
| `--audit-dir` | /var/log/burkebot/audit | Audit bundle root (single-project) |
| `--state-file` | /var/lib/burkebot/processed-prs.json | PR state file (single-project) |
| `--data-dir` | | Root dir for auto-discovery (multi-project) |
| `--projects` | | Path to projects.json (multi-project) |
| `--auth-user` | | HTTP basic auth username |
| `--auth-password-file` | | File containing the HTTP basic auth password |
| `--enable-prompt-ui` | false | Enable the ad hoc prompt form |
| `--repo-root` | /srv/burkebot | Default repo root used when `repo_dir` is omitted |
| `--prompt-runner` | /usr/local/bin/burkebot-codex-run | Path to the prompt runner wrapper |
| `--envdir-binary` | /opt/burkebot/bin/envdir | Path to envdir for GitHub-authenticated runs |
| `--env-dir` | /opt/burkebot/env | Envdir directory for GitHub-authenticated runs |
| `--bot-home` | /home/burkebot | Burkebot home dir to keep writable during prompt runs |
| `--tasks-file` | | Path to tasks.json — enables the [task API](burkebot-task-api.md) |
| `--tokens-file` | | Path to tokens.json (required when `--tasks-file` is set) |
| `--version` | | Print version and exit |

## Pages

- **`/`** — project list (multi-project) or redirect to single project
- **`/projects/{project}/`** — audit run list + processed-PR summary
- **`/projects/{project}/tasks`** — configured task API tasks for the project,
  grouped with recent API runs and links to their audit details
- **`/projects/{project}/audit/{run_id}/`** — audit detail: summary, prompt, last
  message, commands, stderr, file list
- **`/projects/{project}/audit/{run_id}/{file}`** — raw file content (text/plain,
  capped at 5 MB)
- **`/projects/{project}/state`** — processed-PR state with Rerun / Rerun All buttons
- **`POST /projects/{project}/prompt`** — submit one audited ad hoc prompt for a
  specific project checkout

## Actions

- **Rerun** removes one PR from processed state and starts the project's
  systemd service (`systemctl start --no-block`).
- **Rerun All** resets the state file to `{}` and starts the service.
- **Run Prompt** executes `burkebot-codex-run` against that project's checkout
  and redirects to the resulting audit bundle.

The dashboard must run as root, or as a user that can traverse and read the
audit directory through the runner's group-readable audit bundle permissions.
Rerun actions also require permission to call `systemctl start`.

## Tests

```bash
GO111MODULE=on go test -trimpath ./cmd/burkebot/...
```
