# Burkebot Task API

JSON HTTP API that lets external services invoke server-defined codex
tasks through burkebot. Burkebot owns the codex install, the sandbox
config, and the audit bundle; callers hand burkebot inputs and get back
schema-validated structured output.

The API only knows about *named tasks* whose prompts, output schemas,
and runtime policies are defined on the server in `tasks.json`. Callers
cannot supply prompts, widen the sandbox, or change the schema — see
[burkebot-tasks.md](burkebot-tasks.md) for how tasks are authored.

## Enabling

The API is off unless the dashboard is started with both files:

```bash
burkebot dashboard \
  --projects /opt/burkebot/projects.json \
  --tasks-file /opt/burkebot/tasks.json \
  --tokens-file /opt/burkebot/tokens.json
```

Setting only one of `--tasks-file` / `--tokens-file` is a startup
error. With neither set, every `/api/*` request returns `404`.

## Base URL

All endpoints sit under `/api/` on the dashboard's listen address. They
are routed *before* the dashboard's HTTP basic auth, so an API caller
never sees a basic-auth challenge.

## Authentication

Every request must carry a bearer token:

```
Authorization: Bearer <token>
```

Tokens are sha256-hashed in `tokens.json` and scoped to a whitelist of
task names. There is no way to widen a token's scope from the wire — a
caller can only invoke tasks that already appear in the token's
`allowed_tasks` list.

The server returns `404 Not Found` for both "unknown task" and "known
task, this token is not allowed to invoke it" so callers cannot probe
the task list with an invalid token.

## Endpoints

### `POST /api/tasks/<name>/runs`

Run the named task synchronously and return its parsed output.

**Request body:** JSON object with one string field per input the task
declares. Any missing input is `400 missing_input`; any extra field is
`400 unknown_input`.

```bash
curl -sS -X POST \
  -H "Authorization: Bearer $BURKEBOT_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"agenda":"1. Roll Call\n","transcript":"[00:00:01] hi\n"}' \
  https://burkebot.example/api/tasks/annotate-meeting/runs
```

**Success response (200):**

```json
{
  "run_id": "20260506T091245Z-annotate-meeting-public-meetings",
  "output": {
    "meeting_summary": "...",
    "items": [
      { "number": "1", "title": "Roll Call", "start_timestamp": "00:00:05", "start_seconds": 5 }
    ]
  }
}
```

`output` is the parsed JSON the runner produced, after the server has
re-validated it against the task's JSON Schema. The server runs the
codex unit synchronously; the connection blocks for the full duration
(can be many minutes for long inputs). Use a generous client-side read
timeout.

`run_id` is the audit-bundle directory name under the project's
`audit_dir`; you can pull commands, stderr, and the raw last message
from the dashboard at
`/projects/<project>/audit/<run_id>/`.

If the connection drops mid-run, the audit bundle still contains the
result on the server, but there is currently no recovery endpoint —
the caller should retry. (The retry will start a fresh run; the prior
run finishes and is reaped.)

## Error responses

All errors are JSON objects in the [RFC 7807](https://tools.ietf.org/html/rfc7807)
problem-details shape, matching `kevinburke/rest/resterror.Error`:

```json
{
  "title": "missing input \"transcript\"",
  "id": "missing_input",
  "detail": "...",
  "status": 400
}
```

Branch on `id`, not on `title` or `detail`. Stable IDs:

| Status | `id` | When |
|--------|------|------|
| 400 | `invalid_body` | Body is not a JSON object of string values |
| 400 | `unknown_input` | Body has a field the task did not declare |
| 400 | `missing_input` | Body is missing a declared input |
| 401 | (n/a — see below) | The API does not return 401; auth failure is `404` |
| 404 | `not_found` | Unknown task, unauthorized token, or task API not enabled |
| 405 | `method_not_allowed` | e.g. GET on the runs endpoint |
| 413 | `input_too_large` | Input value exceeds the task's `max_bytes` |
| 500 | `server_error` | Misconfigured task, scratch-dir failure, etc. |
| 502 | `runner_failed` | Codex runner exited non-zero or hit the task's timeout |
| 502 | `runner_no_output` | Runner exited 0 but did not produce a `last-message.txt` |
| 502 | `invalid_output` | Runner output failed JSON Schema validation |

`502` errors mean burkebot was reachable but the upstream codex run
failed. Retry is usually appropriate for `runner_failed` and
`runner_no_output`; `invalid_output` more often means a model regression
or a schema mismatch and should be investigated rather than retried.

## Idempotency and retries

The current API has no idempotency keys. Each `POST` starts a fresh
codex run and burns a fresh audit bundle. Be aware that:

- A retry after a network drop will run the model twice on the server.
- There is no cap on concurrent submissions from a single token; the
  server starts every request immediately.

If you need at-most-once semantics, add an idempotency layer in front
of burkebot or queue serially on the caller side.

## Auditing what was run

Every API run lands in the project's audit bundle alongside dashboard
prompt runs. Bundles are tagged with `--source api` so they are
distinguishable from `--source adhoc` prompt-UI runs.

```bash
ls /var/log/burkebot/audit/ | grep annotate-meeting
```

Each bundle contains `prompt.txt`, `codex-events.jsonl`, `commands.jsonl`,
`last-message.txt`, and `summary.json` — see the dashboard's audit
detail page for an indexed view. The dashboard also lists configured
tasks and their recent API runs at `/tasks`.
