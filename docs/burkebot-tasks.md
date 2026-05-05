# Burkebot Tasks

How to define a task that external services can invoke through
[the task API](burkebot-task-api.md).

A task is a server-side bundle of:

- **a prompt template** — Go `text/template`, rendered per request with
  the input file paths
- **a JSON Schema** — describes the structure of the model's output;
  enforced both by `codex --output-schema` during the run and by
  burkebot when validating the runner's response
- **an input declaration** — what fields the API caller must supply,
  what filename each lands at on disk, and a per-input byte cap
- **a fixed runtime policy** — read-only sandbox, no GitHub creds, no
  workspace writes; not configurable from the wire

Callers see only the task name and the input field names. Everything
else is server-defined and immutable from the caller's side.

## File layout

A typical deployment puts task definitions under `/etc/burkebot/`:

```
/etc/burkebot/
  tasks.json
  tokens.json
  tasks/
    annotate-meeting/
      prompt.tmpl
      schema.json
```

The dashboard is started with:

```bash
burkebot dashboard \
  --tasks-file /etc/burkebot/tasks.json \
  --tokens-file /etc/burkebot/tokens.json \
  ...
```

## `tasks.json`

A list of task objects. Burkebot validates every entry at startup —
unparseable templates, missing schema files, or duplicate input names
fail loudly rather than waiting until first invocation.

```json
[
  {
    "name": "annotate-meeting",
    "project": "public-meetings",
    "prompt_template": "/etc/burkebot/tasks/annotate-meeting/prompt.tmpl",
    "output_schema":   "/etc/burkebot/tasks/annotate-meeting/schema.json",
    "inputs": [
      {"name": "agenda",     "filename": "agenda.txt",     "max_bytes": 1048576},
      {"name": "transcript", "filename": "transcript.txt", "max_bytes": 8388608}
    ],
    "timeout_seconds": 300
  }
]
```

| Field | Required | Description |
|-------|----------|-------------|
| `name` | yes | Slug used in URLs: `POST /api/tasks/<name>/runs`. Lowercase, dash-separated by convention. |
| `project` | yes | Must match a `Project.name` in `projects.json`. The audit bundle and project repo come from that project. |
| `prompt_template` | yes | Absolute path to a Go `text/template` file. Parsed at startup. |
| `output_schema` | yes | Absolute path to a JSON Schema (draft 2020-12). Compiled at startup. |
| `inputs` | yes | List of input declarations (see below). May be empty for prompts that take no caller-supplied data. |
| `timeout_seconds` | no | Soft timeout for the codex run. The systemd unit is not stopped — burkebot just stops waiting and returns `runner_failed`. `0` means "no soft cap" (rely on whatever the runner enforces). |

### `inputs`

Each input gets written to `<scratchDir>/<filename>` before the runner
fires; the scratch dir is the runner's working directory inside the
sandbox, so codex can read these files directly.

| Field | Required | Description |
|-------|----------|-------------|
| `name` | yes | The JSON key the caller uses in the request body. |
| `filename` | yes | The basename of the file the value is written to. The absolute path becomes available in the prompt template. |
| `max_bytes` | no | Hard cap on the value size. The server returns `413 input_too_large` *before* writing anything to disk. `0` means no cap — set a real number for any input that comes from a third party. |

Input order matters in error messages but not in the protocol;
declaring `agenda` before `transcript` is purely for the
`unknown_input` error's `detail` listing.

## Prompt templates

The template is rendered with a `map[string]any` whose keys are the
input names you declared, with values being the *absolute on-disk path*
to the input file. Templates can also reach into a synthetic `Inputs`
key for the same map — useful for ranging:

```go
{{/* annotate-meeting/prompt.tmpl */}}
Read the agenda items in {{ .agenda }} and the meeting transcript in
{{ .transcript }}.

For each agenda item, find the timestamp in the transcript where its
discussion begins. Write a JSON object matching the schema burkebot
gave you (agenda items only when actually discussed; ordered by
start_seconds ascending).
```

Two notes on the template DSL:

- **Inputs are paths, not values.** The template receives where the
  data lives, not the data itself. Codex reads the files inside the
  sandbox. This keeps prompts under the codex token-budget even for
  large transcripts.
- **`{{ .Inputs }}` is the same map**, available as `{{ range $name, $path := .Inputs }}...{{ end }}` for prompts that want to iterate.

Templates are parsed (not executed) at server startup; a syntax error
in `prompt.tmpl` fails `burkebot dashboard --tasks-file ...` with
the file path and parse error.

## Output schemas

Plain JSON Schema (draft 2020-12). Two best practices:

1. **`additionalProperties: false`** — without this, codex can invent
   keys that you'll silently parse into nothing. The reflector in
   `public-meetings`'s `schemas/annotation.json` does this by default.
2. **Mark required fields** — anything you'll deference in the parsed
   output should be in `required`. The schema is enforced twice (codex
   `--output-schema` during the run, burkebot's validator before
   responding); marking required fields catches model regressions
   before the response leaves the host.

Schema file changes take effect on dashboard restart. There is no
hot-reload.

## `tokens.json`

```json
[
  {
    "name": "public-meetings",
    "sha256_hex": "9c1185a5c5e9fc54612808977ee8f548b2258d31ddadef708ddc2c6a55edcae3",
    "allowed_tasks": ["annotate-meeting"]
  }
]
```

| Field | Required | Description |
|-------|----------|-------------|
| `name` | yes | Human label that appears in audit bundle filenames and logs. Never sent on the wire. |
| `sha256_hex` | yes | Lowercase hex sha256 of the cleartext token. 64 hex chars. |
| `allowed_tasks` | yes | Whitelist of task names this token may invoke. Empty means "no tasks" — there is no wildcard. |

### Provisioning a token

```bash
# Generate the cleartext token (saved to the caller's secret store).
token=$(openssl rand -hex 32)
echo "$token"

# Compute the hash for tokens.json.
echo -n "$token" | sha256sum | awk '{print $1}'
```

The cleartext should never live on the burkebot host. Add only the
hash to `tokens.json` (typically rendered from an Ansible vault
variable, e.g. `vault_burkebot_api_tokens`).

### Rotation

Adding a new entry with the same `name` and a new hash + restarting the
dashboard rotates the token. There is no online rotation; restart is
the rotation barrier.

## Adding a new task — full walkthrough

Goal: add a `summarize-pr` task that takes a PR diff and a PR title and
returns a one-paragraph summary.

1. **Author the schema.**

   `/etc/burkebot/tasks/summarize-pr/schema.json`:

   ```json
   {
     "$schema": "https://json-schema.org/draft/2020-12/schema",
     "type": "object",
     "additionalProperties": false,
     "required": ["summary"],
     "properties": {
       "summary": {"type": "string", "minLength": 20}
     }
   }
   ```

2. **Author the prompt template.**

   `/etc/burkebot/tasks/summarize-pr/prompt.tmpl`:

   ```
   Read the PR title in {{ .title }} and the diff in {{ .diff }}.
   Write a one-paragraph summary of what the PR changes and why.
   Output JSON conforming to the schema burkebot gave you.
   ```

3. **Add the task entry** to `/etc/burkebot/tasks.json`.

4. **Provision a token** for the caller (or extend an existing token's
   `allowed_tasks` to include `summarize-pr`).

5. **Restart the dashboard** so it picks up the new task and token
   files.

6. **Smoke-test:**

   ```bash
   curl -sS -X POST \
     -H "Authorization: Bearer $BURKEBOT_TOKEN" \
     -H 'Content-Type: application/json' \
     -d '{"title":"Add foo","diff":"@@ ...\n+func Foo() {}\n"}' \
     https://burkebot.example/api/tasks/summarize-pr/runs
   ```

7. **Inspect the audit bundle** at
   `/var/log/burkebot/audit/<run-id>/` to confirm the prompt and the
   commands codex executed.

## Security model

What a compromised API token can do:

- Invoke any task on its `allowed_tasks` list, with attacker-controlled
  inputs (subject to per-input `max_bytes` caps).
- Read whatever the task's prompt tells codex to read inside the
  sandbox (`scratchDir` and `bot-home`, read-only elsewhere).
- Spend tokens on the OpenAI account codex authenticates against.

What it cannot do:

- Invoke a task that is not on its `allowed_tasks` list.
- Widen the sandbox (no `--workspace_write`, no GitHub creds, no
  `--dangerous`). These are not request-body fields; they are not
  reachable from the API.
- Run arbitrary commands. Codex can run shell commands inside the
  sandbox, but the sandbox is read-only outside the scratch dir.
- Reach the dashboard's basic-auth-protected pages. The `/api/`
  router does not share auth state with the dashboard UI.

If a token leaks, rotate it (replace `sha256_hex` and restart) and
audit the bundles tagged with that token's `name` for any unexpected
runs.
