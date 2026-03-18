# Burkebot Dashboard

Web UI for browsing Codex audit trails and managing processed-PR state.

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
`burkebot.service`.

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--port` | 4872 | HTTP listen port |
| `--listen-addr` | 0.0.0.0 | HTTP listen address |
| `--audit-dir` | /var/log/burkebot/audit | Audit bundle root (single-project) |
| `--state-file` | /var/lib/burkebot/processed-prs.json | PR state file (single-project) |
| `--data-dir` | | Root dir for auto-discovery (multi-project) |
| `--projects` | | Path to projects.json (multi-project) |
| `--version` | | Print version and exit |

## Pages

- **`/`** — project list (multi-project) or redirect to single project
- **`/projects/{project}/`** — audit run list + processed-PR summary
- **`/projects/{project}/audit/{run_id}/`** — audit detail: summary, prompt, last
  message, commands, stderr, file list
- **`/projects/{project}/audit/{run_id}/{file}`** — raw file content (text/plain,
  capped at 5 MB)
- **`/projects/{project}/state`** — processed-PR state with Rerun / Rerun All buttons

## Actions

- **Rerun** removes one PR from processed state and starts the project's
  systemd service (`systemctl start --no-block`).
- **Rerun All** resets the state file to `{}` and starts the service.

The dashboard must run as root (or a user that can read the audit directory and
call `systemctl start`).

## Tests

```bash
GO111MODULE=on go test -trimpath ./cmd/burkebot/...
```
