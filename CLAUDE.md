# burkebot

burkebot watches PRs from upstream bots (Dependabot, etc.) and acts on them
via Codex. The repository ships a dashboard, a task-API server, and a few
runner binaries; the deployment (systemd units, sandboxing, vault, helper
shell scripts) lives in
`~/src/github.com/kevinburke/caracal-server/roles/burkebot/`.

## Execution paths

There are three distinct ways a Codex run is kicked off today. They share
the same audit-bundle layout but differ on repo-dir strategy and on who
holds GitHub credentials.

1. **Cron-driven prompt** (operator runs `burkebot-prompts/run-*.sh` on
   their laptop → SSH → `burkebot-prompt.sh.j2` on the host). Uses a bare
   mirror under `/srv/burkebot/<slug>.git`, fresh clone per run into a
   per-run job dir. Agent runs sandboxed with NO `GH_TOKEN`. Prompt is
   expected to emit a JSON outcome (`outcome push_branch`, etc.). After
   the agent finishes, `burkebot-publish` (running outside the sandbox
   with `GH_TOKEN` from envdir) consumes the decision and pushes / opens
   the PR.

2. **Dashboard task API** (`POST /api/tasks/<task>/runs`). Caller drops
   input files into a fresh per-run job dir; the runner `cd`s in and runs
   Codex with `--job-dir`. `git init` only — no remote, no push.

3. **Dashboard ad-hoc prompt** (`POST /projects/<project>/prompt`). Today:
   runs inside a *shared* checkout at `proj.RepoDirectory(cfg.RepoRoot)`,
   optionally envdir-wrapped to inject `GH_TOKEN` into the agent's env. No
   structured outcome; the agent itself shells out to git/gh if it wants
   to ship a PR. **In flight (see
   `/Users/kevin/.claude/plans/breezy-churning-feigenbaum.md`):** moving
   this to the same per-run-job-dir + clone-from-mirror model as the cron
   flow, with a dashboard-driven "Open / Update PR" step that invokes
   `burkebot-publish`. Once that's done, the agent never holds
   `GH_TOKEN` in any path.

   **Known stale assumption:** the project page today errors with
   `repo directory "/srv/burkebot/<slug>" is not available`. That path is
   a checkout that nothing creates — `/srv/burkebot/` only holds bare
   mirrors. Fixed by the per-run-job-dir migration.

4. **Dashboard follow-up** (`POST /projects/<project>/audit/<run>/followup`).
   Per-run job dir with `git init` (no inheritance from the origin run's
   git state). Uses Codex `--resume-session` for conversational
   continuity only. The plan above adds clone-from-origin-job-dir so
   follow-ups can iterate on the same branch and update the same PR.

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

- `summary.json` — `Summary` struct in `cmd/burkebot/audit.go`. Includes
  RunID, Source, Label, PromptID, timing, ExitCode, TokenUsage,
  CodexSessionFiles, ResumeSessionID. The plan adds JobRepoDir,
  RootRunID, BranchName, BaseCommitSHA, HeadCommitSHA, LastPushedSHA,
  PRNumber, PRRepo for the publish flow.
- `prompt.txt` — operator's prompt (shown as preview on project page).
- `last-message.txt` — agent's final message.
- `codex-events.jsonl` — newline-delimited JSON, each line
  `{type: "item.completed", item: {...}}` with item types
  `command_execution` (Command + AggregatedOutput + ExitCode + Status)
  or `agent_message` (Text). Parsed by `loadConversationTimeline` in
  `audit.go:210-263` to render the timeline on the audit detail page.
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
