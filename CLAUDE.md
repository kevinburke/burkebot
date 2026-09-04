# burkebot

burkebot watches PRs from upstream bots (Dependabot, etc.) and acts on them
via Codex. The repository ships a dashboard, a task-API server, and a few
runner binaries; the deployment (systemd units, sandboxing, vault, helper
shell scripts) lives in
`~/src/github.com/kevinburke/caracal-server/roles/burkebot/`.

## Execution paths

There are four ways a Codex run is kicked off. They share the same
audit-bundle layout but differ on repo-dir strategy and on who holds
GitHub credentials.

1. **Cron-driven prompt** (operator runs `burkebot-prompts/run-*.sh` on
   their laptop → SSH → `burkebot-prompt.sh.j2` on the host). Calls
   `burkebot-mirror-fetch` to ensure a bare mirror under
   `/srv/burkebot/mirrors/<repo>.git`, clones fresh into a per-run job
   dir. Agent runs sandboxed with NO `GH_TOKEN`. Prompt is expected to
   emit a JSON outcome (`outcome push_branch`, etc.). After the agent
   finishes, `burkebot-publish` (running outside the sandbox with
   `GH_TOKEN` from envdir) consumes the decision and pushes / opens the
   PR.

2. **Dashboard task API** (`POST /api/tasks/<task>/runs`). Caller drops
   input files into a fresh per-run job dir; the runner `cd`s in and runs
   Codex with `--job-dir`. `git init` only — no remote, no push.

3. **Dashboard ad-hoc prompt** (`POST /projects/<project>/prompt`).
   Mirrors the cron flow's shape: dashboard calls `burkebot-mirror-fetch`,
   clones into a fresh job dir under `/srv/burkebot-jobs/`, checks out
   `burkebot/<project>-<token>`, then invokes the runner. Agent has no
   `GH_TOKEN`. After the run, dashboard captures the base/head SHAs and
   patches `summary.json` with `JobRepoDir`, `BranchName`, `RemoteURL`,
   etc. The audit page exposes an "Open / Update PR" button that shells
   out to `burkebot-publish` (or auto-runs it if "Open PR when done" was
   ticked at submit).

4. **Dashboard follow-up** (`POST /projects/<project>/audit/<run>/followup`).
   Per-run job dir cloned **from the origin run's `JobRepoDir`** so the
   follow-up inherits all of the origin's commits and starts on the same
   branch. After the clone, `origin` is re-pointed at the canonical
   upstream (carried as `RemoteURL` in the origin's summary) so a later
   publish pushes to GitHub, not to the prior job repo. Codex
   `--resume-session` preserves conversational continuity on top.
   Publishing a follow-up uses `--force-with-lease` against the recorded
   `LastPushedSHA` to update the same PR branch.

   The follow-up form is hidden when the origin run lacks
   dashboard-managed git state (cron / task-API runs), or when the
   origin's job dir has been cleaned up (no current retention policy;
   job dirs accumulate under `/srv/burkebot-jobs/` until manually
   pruned).

## Authority boundary

- **Agent (Codex):** sandboxed (srt + bubblewrap), `GH_TOKEN` explicitly
  unset before exec, env-dir denyRead'd inside the sandbox. Network
  restricted to OpenAI endpoints. Sees only the per-run repo dir.
- **Orchestration scripts (`burkebot-publish`, `burkebot-prompt`):** run
  as the `burkebot` user OUTSIDE the sandbox with `GH_TOKEN` available
  via envdir. They do all GitHub push and PR creation.
- **Dashboard process:** runs as root, but no `GH_TOKEN` in its
  environment. When it needs to publish, it shells out to
  `burkebot-publish`, which loads its own creds.

Do NOT add `GH_TOKEN` to the dashboard service env, and do NOT add it to
any envdir wrapping a Codex invocation. The whole point of the
separation is that a compromised LLM can't exfiltrate the token.

## Audit bundle layout

Each run writes to `<AuditDir>/<RunID>/` where `RunID` is
`YYYYMMDDTHHMMSSZ-<source>-<label>` (parsed from the runner's
`Audit run:` / `Audit dir:` stdout lines in `runner.go:53-55`).

Files:

- `summary.json` — `Summary` struct in `cmd/burkebot/audit.go`. Runner
  writes RunID, Source, Label, PromptID, timing, ExitCode, TokenUsage,
  CodexSessionFiles, ResumeSessionID. Dashboard patches in (via
  `patchSummaryGitState`, atomic) JobRepoDir, RootRunID, BranchName,
  BaseCommitSHA, HeadCommitSHA, LastPushedSHA, PRNumber, PRRepo, and
  RemoteURL for the publish flow.
- `prompt.txt` — operator's prompt (shown as preview on project page).
- `last-message.txt` — agent's final message.
- `codex-events.jsonl` — newline-delimited JSON, each line
  `{type: "item.completed", item: {...}}` with item types
  `command_execution` (Command + AggregatedOutput + ExitCode + Status),
  `agent_message` (Text), or `error` (Message). Parsed by
  `loadConversationTimeline` to render the timeline on the audit detail
  page.

  **`exit_code` in summary.json does not tell you whether a run
  worked.** Codex emits an `error` item for a tooling or environment
  failure, carries on, and exits 0: the model answers from the prompt
  alone and says so in prose that still validates against the task
  schema. From 2026-08-29 to 2026-09-04 every run on this host was in
  that state (a codex upgrade began routing the model's `exec` tool
  through a `codex-code-mode-host` binary that was not installed), and
  it surfaced only as published meeting summaries apologizing for not
  being able to read their own agenda. `loadCodexErrors` is the check
  that catches it; `handleTaskRun` returns 502 rather than 200 when it
  finds anything, and the runner script in caracal-server
  (`roles/burkebot/templates/burkebot-codex-run.sh.j2`) makes the same
  check and exits non-zero so the cron and dashboard paths fail too.
- `commands.jsonl` — separate record of agent tool calls.
- `codex-stderr.log` — runner's stderr.
- `codex-sessions/` — exported Codex session files for resumption (used
  by follow-up's `--resume-session`).

## Sandbox notes

The runner (`burkebot-codex-run.sh.j2`) launches Codex inside `srt`
(sandbox-runtime), which wraps bubblewrap. Filesystem allowRead/Write,
denyRead/Write, and network egress rules come from
`burkebot-srt-settings.jq.j2`. Key constraints:

- `denyRead`: `/home`, `/srv/burkebot`, `/opt/burkebot/env`,
  `/var/lib/burkebot`, `/root`.
- `denyWrite`: `/opt/burkebot/env` (remounted RO).
- `allowRead/Write`: per-job dir only (`$job_dir/repo`, `$job_dir/home`,
  `/tmp`, `/cache`, `/codex`).
- Network: `api.openai.com`, `chatgpt.com`, `auth.openai.com` only.
- `burkebot-sandbox-preflight.sh.j2` asserts at boot that the sandbox
  masks `GH_TOKEN` and the env dir. If you add new credentials, extend
  preflight so a regression fails loudly.

## Build / dev

- `safego build -trimpath -o tmp/ ./cmd/burkebot` (don't leave binaries
  in the repo root).
- `safego vet ./... && safego fix ./... && safego fmt ./... && safegoimports -w .`
  before returning control.
- `go test ./...` for unit tests. The dashboard has test seams
  (`Server.runPrompt`, `runFollowup`, `runTask` function fields) for
  unit-testing handlers without actually invoking the runner.

## Deployment

Everything is Ansible. The role at
`~/src/github.com/kevinburke/caracal-server/roles/burkebot/` deploys
binaries, systemd units, the envdir token files, and the srt sandbox
config. Vault is whole-file
(`inventory/group_vars/burkebot/vault.yml`); see
`~/src/github.com/kevinburke/caracal-server/.claude/skills/ansible-vault/SKILL.md`
for the workflow.

After deploying, SSH to `burkebot` and run
`/usr/local/bin/burkebot-sandbox-preflight.sh` to confirm the sandbox is
still tight.
