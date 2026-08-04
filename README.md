# Yanshi

> **Yanshi — the self-driven coding agent.** 偃师 —— 自驱的编码 agent

Named after 偃师 (Yǎnshī), the legendary artisan who built an autonomous automaton in 《列子·汤问》. Not affiliated with [chaitin/yanshi](https://github.com/chaitin/yanshi).

Yanshi is a Go agent server (module `github.com/x6nux/yanshi`) that wires an
LLM orchestrator to guard-wrapped tools, a long-lived memory store, a
standard-compatible skill system, and a self-driven goal loop. The `yanshi`
binary is a self-contained CLI: a bare invocation launches a terminal UI (TUI)
that discovers a running backend for the current project or embeds one in-process.
`serve` starts a shared daemon; `goal` runs the self-driven loop.

## Why Yanshi?

偃师出自《列子·汤问》—— 上古工匠，造出能歌舞的"倡者"（自动人偶）献给周穆王；剖开只见皮革木胶颜料，内别无他物。世界最早的"自动机械"传说之一。

Yanshi 造的是会自己动的编码 agent：自驱动 goal loop、ReAct 编排、子代理委派、ACP 拉起外部 agent。名字就是产品语义。

## Prerequisites

- **Go 1.26.4+** (`go.mod`) — needed to build the binary.
- **git** — optional for the binary as a whole, but **five features shell out
  to it**, each carrying its own version floor. Yanshi never bundles git; where
  it is absent those features fail and the rest of the binary keeps working.

  | Feature | What it runs | Floor |
  | --- | --- | --- |
  | `git_diff` tool, `base_ref` and `commit` scopes | `git diff` / `git show` with `--end-of-options` | **2.24** (2019-11) |
  | `git_status` tool | `git status --porcelain=v2` | **2.11** (2016-11) |
  | `goal` loop, when autoVCS is unconfigured | `git worktree add` / `list` / `remove` | **2.17** (2018-04) |
  | `yanshi pr <number>` given a bare number | `git remote get-url origin` | **2.7** (2016-01) |
  | `/skill install github:<owner>/<repo>` | `git clone --depth 1` | any |

  The first row is the only floor that is a *hard* one in the sense of failing
  loudly on purpose. Those two scopes pass `--end-of-options` so that a ref
  beginning with `-` can never be read as a flag; older git does not recognise
  the marker and **rejects the command outright**, so on git < 2.24 the two
  scopes fail on every call rather than silently losing the protection.
  `git_diff`'s `working_tree` scope emits no marker and works with any git.

  Two more places invoke git but degrade instead of failing: the `diagnostics`
  tool's repository probe and the orchestrator's environment probe both simply
  report git as unavailable.

  **autoVCS is not on this list** — it is a SQLite store of its own, not a git
  wrapper, and it needs no git binary. The `goal` loop's git worktrees are the
  *fallback* taken only when autoVCS is unconfigured (`App.VCSRepoID` empty);
  with autoVCS active the loop branches and merges inside SQLite instead.

## Quick start

```sh
# 1. Build the CLI.
go build -o yanshi ./cmd/yanshi

# 2. Create configuration from the tracked example (config.yaml is gitignored).
cp config.example.yaml config.yaml
#    Edit config.yaml: set `token`, fill in provider api_key values, etc.

# 3. Launch the self-contained TUI (discovers or embeds the backend).
./yanshi
#    No API keys yet? Boot a deterministic fake model:
./yanshi --fake-model

# 4. Prefer a shared daemon? Start the server (SIGINT/SIGTERM to stop), then
#    other `yanshi` invocations in the same project discover it.
./yanshi serve -config config.yaml
```

The TUI is multi-turn and tool-aware: type a message and press Ctrl+J (Ctrl+Enter)
to send; assistant markdown streams in and tool calls render as `⏺ name …` blocks.
Ctrl+C cancels the in-flight turn; Ctrl+C again quits. Lines beginning with
`/skill <name>` are handled as a chat command (see **Skills** below). A legacy
line-based REPL is available via `yanshi chat --no-tui` (SSE, single-turn).
Self-driven `/dev` mode is reached via the `goal` subcommand, not chat.

**Verify the build.** The alt-screen TUI cannot be pipe-driven, but a scripted
boot check confirms the binary starts cleanly:

```sh
go build -o yanshi ./cmd/yanshi
./yanshi -h                       # prints usage and exits 0 (no TUI)
timeout 5 ./yanshi --fake-model -inprocess   # boots the TUI; timeout kills it
```

With a real `config.yaml` (providers + keys), exercise the feature-parity set by
hand in the TUI:

- [ ] `/model` lists the configured models; `/model <name>` switches (the header
  updates and the next turn runs on the new model).
- [ ] `/think medium` sets reasoning effort (visible in `/config` and the header).
- [ ] `/cost` and `/config` render a status block with token usage / turn count.
- [ ] Send a message that needs a guarded tool (e.g. ask the agent to write a
      file outside granted paths): a `y/n/a` permission prompt appears; `y`
      runs the tool, `n` denies it, `a` suppresses future prompts for that action.
- [ ] `/compact` summarizes older turns (WS only); `/clear` resets history.
- [ ] `/mcp` lists configured MCP servers; `/help` lists every command.

## CLI

`yanshi` is a single binary with a TUI-first default. The TUI is always a thin
local client: at launch a **session resolver** discovers a backend for the current
project (a per-project lockfile under the OS cache dir plus a `/healthz` probe)
or bootstraps one **in-process** (`127.0.0.1:0`) and claims the lockfile. The TUI
then connects over **WebSocket** (`/api/v1/chat/ws`, multi-turn, tool-aware); if
the WS handshake fails it falls back to **SSE** (`/api/v1/chat`, equally
multi-turn and tool-aware). Loopback needs no token.

```
yanshi                                 # self-contained TUI (default)
yanshi --fake-model                    # ...with a deterministic fake model
yanshi chat                            # same TUI
yanshi chat --no-tui                   # legacy line REPL (SSE, single-turn)
yanshi chat -server http://host:port   # force a remote backend
yanshi chat -inprocess                 # force an in-process backend
yanshi serve [-addr ADDR]              # shared daemon other invocations discover
yanshi goal -goal "..." -tier auto     # self-driven goal loop
```

- **Bare `yanshi`** — self-contained TUI. Discovers a running backend for the
  current project or embeds one in-process. WebSocket is the primary transport;
  SSE is the fallback. Flags: `-config`, `-fake-model`, `-server`, `-inprocess`.
- **`serve`** — start the HTTP server as a shared daemon. Other `yanshi`
  invocations in the same project discover it via the lockfile and join over WS.
- **`chat`** — same TUI as the bare invocation. `--no-tui` drops to the legacy
  line-based REPL (SSE, single-turn). `-server`/`-inprocess` force a backend.
- **Multi-window self-heal** — on owner exit, disconnected clients re-discover
  and the first to find no live backend bootstraps a new one (atomic lockfile
  election with PID-liveness reclaim).

### Slash commands & interactive features

The TUI exposes a Claude-Code-style command set. Type `/` to begin a command
(handled locally — never sent as a user message); `/help` lists them all.

| Command | Action |
| --- | --- |
| `/help` | list the available commands. |
| `/model [name]` | with no arg, list the configured models; with a name, switch the session model mid-conversation (takes effect on the next turn). |
| `/think low\|medium\|high\|off` | set the reasoning effort passed to the model for subsequent turns (`off` clears it). |
| `/config` | render a status block showing the active model, thinking effort, and accumulated token usage. |
| `/cost` | render a status block with token usage for the session so far. |
| `/clear` | reset the conversation history and zero the usage counters. |
| `/compact` | compact the context now (WebSocket only): summarize older turns into one message, streaming the summary live, and keep the recent exchange verbatim. |
| `/mcp` | list the configured MCP servers (renders "(none configured)" when empty). |

**Interactive tool permissions.** When the static permission profile would deny
a tool call (e.g. an `fs_write` outside granted write paths, or a `shell_run`
not on the allowlist), the TUI prompts interactively over the WebSocket: the
turn pauses, a prompt renders, and a single keypress resolves it — `y` allow
(this call only), `n` deny (the tool returns its denial), `a` always-allow
(records the exact action in the per-session allowlist so subsequent identical
calls skip the prompt). The session header always shows the active model,
thinking effort, and a live token/turn tally.

**Auto context-compaction.** When the estimated conversation tokens reach
`threshold * context_window`, the older turns are summarized into a single
message by a remote model before the next turn runs — streamed live as a
`compacting…` block, resolving to `compacted (X → Y tokens)`. Configure it under
`compaction:` in `config.yaml` (see `config.example.yaml`): `threshold`
(default `0.8`, `<= 0` disables), `keep_recent` (trailing user/assistant pairs
kept verbatim, default `4`), `context_window` (token budget, default `128000`),
and `model` (optional dedicated fast remote model for summarization; empty uses
the active session model). Auto-compaction is WebSocket-only on the streaming
path; the manual `/compact` command is also WS-only.

**Transports.** WebSocket (`/api/v1/chat/ws`) is the primary transport —
server-held history on one persistent socket, bidirectional (cancel, control
frames, interactive permissions). SSE (`/api/v1/chat`) is the fallback, equally
multi-turn and tool-aware (client-held history replayed each request); the WS
handshake fails over to it automatically. Interactive permissions and streaming
compaction are WS-only; SSE stays on the static permission profile.

## Tools

Every tool call is mediated by the guard (`internal/guard`), which checks the
acting agent's permission profile (tool allowlist, filesystem read/write globs,
shell policy, and net hosts) before dispatch.

The groups below are the **core tools an operator meets first**, not the full
registry — a built server registers several times this many (sub-agent and
workflow delegation, task/checklist/todo bookkeeping, shell sessions,
automations, VCS, code review, diagnostics, GitHub, artifacts, and more). This
list is prose and is deliberately not kept in sync one-for-one; for the
authoritative set, read the registration calls under `internal/tools` (a built
`App` reports them in `ToolNames`). The groups below:

**Memory**
- `memory_search` — full-text search over stored memories.
- `memory_recall` — return the most-recent memories.
- `memory_write` — store a memory for later recall.

**Web**
- `web_fetch` — HTTP GET a URL and return the response body as text.
- `web_search` — search the web and return a list of result titles and URLs.

**Filesystem** (scoped to a work root)
- `fs_read` — read a file, optionally from a line offset with a line limit.
- `fs_write` — create or overwrite a file with the given content.
- `fs_edit` — replace an exact, unique string in a file (or every match).
- `fs_list` — list directory entries with sizes and `is_dir`.
- `fs_glob` — find files whose path matches a glob (supports `**`).
- `fs_search` — search file contents with a regexp (skips `.git`, `node_modules`, `vendor`, binary files).

**Shell**
- `shell_run` — run a single shell command; returns combined output, exit code, and duration. Shell metacharacters (`&&`, `||`, `;`, `|`, backticks, `$()`, `>`, `<`, newlines) are rejected — issue sequential commands instead.

**Git** (read-only; see [Prerequisites](#prerequisites) for the per-feature version floors)
- `git_status` — structured working-tree status. Reads `--porcelain=v2`, so it
  needs git 2.11+.
- `git_diff` — per-file structured diff. Scopes: `working_tree` (any git),
  `base_ref` and `commit` (git 2.24+).

**Time**
- `time_now` — current time as ISO 8601, unix epoch, and UTC offset seconds.

**Skills**
- `skill_use` — load a skill's instructions by name so the model follows them for the turn.

## Skills

Yanshi ships a [standard-compatible](https://agentskills.io/) skill system
(`internal/skills`). A skill is a directory containing a `SKILL.md` whose YAML
frontmatter (`name`, `description`) is loaded at startup and whose markdown body
is read on demand when the skill is invoked. Skill bodies are trusted
instructions; scripts referenced by a skill run through `shell_run` and inherit
the agent's shell profile and guard.

**Where skills live** (first-seen-wins across roots):
- **Builtin** — `skills/<name>/SKILL.md`, shipped with the repo (configurable via `skills.builtin_dir`).
- **User** — `~/.yanshi/skills/<name>/SKILL.md` (configurable via `skills.user_dir`).
- **Plugin** — `~/.yanshi/plugins/<plugin>/skills/<name>/SKILL.md`, discovered when the plugin has a `.yanshi-plugin/plugin.json` (configurable via `skills.plugin_dir`).

**How to invoke:**
- Let the model pick: at startup the orchestrator's system prompt lists every registered skill's name and description. The model calls `skill_use(name)` when one applies.
- Force a skill from chat: prefix a message with `/skill <name> [task]`. The skill body is loaded and injected as guidance for that turn; an unknown skill returns an SSE error listing what is available.

See `docs/skills-authoring.md` for the SKILL.md format, validation rules, and a minimal example.

### Builtin dev-workflow skills (T0–T4)

Five tiered skills cover development work from a one-line fix to an open-ended
project. Each describes a workflow and an escalation path to the next tier.

| Tier | Skill | Use when |
| --- | --- | --- |
| T0 | `dev-quick-fix` | single-file, obvious change (typo, config tweak, rename) |
| T1 | `dev-standard-feature` | one focused feature/bugfix, TDD, 1–3 files |
| T2 | `dev-designed-feature` | needs a design first (multi-file, new API, unclear scope) |
| T3 | `dev-team-feature` | multi-component, parallelizable, needs Lead + Workers + Integrator |
| T4 | `dev-autonomous-project` | open-ended; self-drive plan-implement-evaluate-judge to a terminal condition |

**Routing via `/dev`:** Self-driven mode is reached through the `yanshi goal`
subcommand: `yanshi goal -tier auto|t0..t4 -goal "..."`. When `auto`, the
`RuleTierer` picks a tier from the goal text (keyword heuristics, highest tier
wins); `t0`..`t4` force a specific tier. A chat-level `/dev` command is planned
but not yet wired: the `Trigger.DecideTier` logic in `internal/agent/goalloop`
already understands the `/dev` prefix, but the chat handler does not route it
today. See `internal/agent/goalloop` for the Trigger, Tierer, and goal loop.

## VCS (autoVCS)

Yanshi includes a lightweight, SQLite-backed version-control system
(`internal/vcs`) that automatically tracks every edit agents make and exposes
git-like history on top of the working directory. It needs no cooperation from
agents beyond editing files through Yanshi's tools — edits are captured as
they happen and committed on demand.

**Main / worktree model.** `main` is the canonical trunk; the repo root is its
working copy. Worktrees branch from `main_head` (created at task assignment,
live under `~/.yanshi/worktrees/`, and are shareable across agents) and merge
back into `main` via a tree-level 3-way merge. Chat/orchestrator edits track to
`main`; task-agent and ACP-agent edits track to the active worktree —
automatically.

**Tools** (GuardedTools for local agents; also an MCP server delivered to ACP
agents via `session/new` and runnable as `yanshi vcs-mcp`):

- `vcs_commit` — snapshot pending edits as a commit (author = acting agent).
- `vcs_log` — list commits for the active scope (newest-first).
- `vcs_diff` — file-level changes between two commits.
- `vcs_restore` — restore a file from a commit into the working copy.
- `vcs_merge` — merge a worktree into `main` (fast-forward or tree-level 3-way;
  conflicts are returned, `force` lets the worktree win).

**Ignore** merges built-in defaults (`node_modules`, `.git`, `vendor`, `*.log`,
`*.db`/`yanshi.db`, ...) with the config `vcs.ignore` list and any repo-root
`.yanshiignore`. See `docs/vcs.md` for the full model, merge policy, and a
task-agent flow walkthrough.

## Configuration

Configuration is YAML loaded by `internal/config` (`${VAR}` env vars are
expanded before unmarshalling). `config.yaml` is gitignored; start from the
tracked `config.example.yaml`. Top-level keys: `server`, `storage`, `token`,
`llm`, `agents`, `profiles` (per-agent permission profiles), and `skills`
(builtin/user/plugin directories). See `config.example.yaml` for a full
illustrative config including a `coding` profile that grants the filesystem,
shell, and network tools.
