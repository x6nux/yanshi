# Self-Contained CLI + TUI — Design

> Status: DRAFT (brainstorming output, pending user review)
> Date: 2026-07-16
> Branch target: `feat/cli-tui` (see §13 Branching)

## 1. Goal

Make `autocode` a **self-contained, Claude-Code-style** CLI: a single binary that,
when run with no separate backend, takes on the backend's role itself, presents a
TUI, and auto-discovers / shares any already-running backend. Three asks:

1. Allow direct connection to a local instance with **no authentication**.
2. Claude-Code-style **TUI** interaction (replacing the line-based SSE REPL).
3. If no backend is running, **the CLI itself embeds and runs the backend**.

## 2. Background / current state

- `cmd/autocode` has subcommands `serve` (HTTP+Task servers via `bootstrap.Build`),
  `chat` (line-based SSE client; requires `-token`, POSTs `/api/v1/chat`), `goal`,
  `vcs-mcp`.
- `internal/api/http/server.go` `auth` middleware requires a Bearer token for every
  path except `/healthz`.
- `internal/bootstrap/bootstrap.go` `Build(opts)` wires store → model → tools →
  skills → orchestrator → HTTP server; returns `*App{Orch, Store, VCS, …}`.
- `config.example.yaml`: `server.http_addr` defaults to `127.0.0.1:8080`
  (loopback-only already); `storage.sqlite_path` defaults to `"autocode.db"`
  (relative → per-project, in cwd). User-level state lives under `~/.autocode/`.
- The SQLite store persists conversation history, so a fresh process reading the
  same db recovers prior turns. `SetMaxOpenConns(1)` + `InitRepo` reuse-by-root-path
  already make multi-process access safe.

## 3. Architecture overview

The TUI is **always a thin localhost HTTP/SSE client** (one code path). The only
question at launch is *where the backend lives*: a separately-run `serve`, or an
**in-process backend** this CLI spun up. Either way the TUI talks to
`http://127.0.0.1:<port>/api/v1/chat`.

```
                 ┌─────────────────────────────┐
                 │   TUI  (bubbletea + glamour) │
                 └──────────────┬──────────────┘
                                │ ChatBackend (SSE over /api/v1/chat)
                                ▼
            ┌──────────────────────────────────────────┐
            │  Session resolver (discover / bootstrap)  │
            └──────────┬───────────────────┬───────────┘
                       ▼                   ▼
        ┌──────────────────────┐  ┌──────────────────────┐
        │ remote: found a live │  │ in-process: none found│
        │ serve via lockfile   │  │ → bootstrap.Build,    │
        │ → connect (no token) │  │   HTTP 127.0.0.1:0,   │
        │                      │  │   write lockfile      │
        └──────────────────────┘  └──────────────────────┘
```

Design choice **D2 (unified local HTTP)** over D1 (TUI calls the orchestrator
directly when in-process): gives multi-window sharing and keeps a single TUI code
path. SSE over localhost is effectively instant for a single-user CLI.

## 4. Modes & discovery

Default action of `autocode` (bare invocation) → resolve a backend, then launch TUI:

1. Compute project `root` = absolute cwd.
2. Read lockfile at `lockfile.Path(root)` (see §5).
3. **Live?** PID alive (`os.FindProcess` + liveness probe) **and**
   `GET http://<addr>/healthz` responds within ~300ms → **remote mode**: connect,
   no token (loopback bypass, §7).
4. **Stale** (PID dead or healthz fails) → remove the lockfile → fall through.
5. **Absent** → **in-process mode**: `bootstrap.Build`, bind HTTP to `127.0.0.1:0`
   (OS-chosen port), read back the actual listener address, **write the lockfile**,
   connect the TUI to it. The owning CLI owns the backend's lifetime: on exit,
   stop the server and remove the lockfile.

Forced overrides:
- `-server URL` → skip discovery, connect remote (token required unless loopback).
- `-inprocess` → skip discovery, always bootstrap in-process.

## 5. Lockfile

**Location** (option B, OS-standard cache dir, keyed by project root):

```
os.UserCacheDir() + "/autocode/run/" + sanitize(absRoot) + ".lock"
```

- `os.UserCacheDir()` → Windows `%LocalAppData%`, macOS `~/Library/Caches`,
  Linux `$XDG_CACHE_HOME` else `~/.cache`. Base dir is canonical/fixed per OS.
- `sanitize(absRoot)` replaces path separators and the Windows drive colon with `_`
  (e.g. `D:\code\autocode` → `D__code_autocode`).

**Format** (JSON):

```json
{
  "pid": 12345,
  "addr": "127.0.0.1:54321",
  "auth": "none",
  "root": "D:\\code\\autocode",
  "started_at": "2026-07-16T12:00:00Z",
  "version": 1
}
```

**Lifecycle:**
- Written by whoever boots a backend that should be discoverable: `serve`, and the
  in-process owner CLI.
- Removed on graceful shutdown of that owner.
- Treated as **advisory**: a reader always confirms liveness (PID + healthz) and
  reclaims/removes stale entries before falling back.

## 6. Self-healing & owner election (multi-window robustness)

With true in-process, the backend dies with its owning process — so owner exit
*would* disconnect other windows. We make this **self-healing** so it is not fatal:

- Every client wraps its SSE stream in a **reconnect loop**. On stream error/EOF it
  re-runs discovery (§4).
- The first client to re-discover and find **no live backend** bootstraps
  in-process and becomes the new owner; the rest find that owner's fresh live
  lockfile and reconnect.
- **Owner-election race** (two clients bootstrap simultaneously) is resolved by
  **atomic lockfile creation** (`O_CREATE|O_EXCL`): only one `Create` succeeds; the
  loser stands its backend down and connects to the winner. No duplicate backends.
- **Conversation continuity across reconnect** relies on the shared SQLite store:
  a freshly-bootstrapped owner loads prior turns from the same db. The in-flight
  query at the moment of owner death is lost; the TUI shows the partial output and
  a "backend changed; re-send if incomplete" note. (Owner death mid-stream is rare;
  the common case is owner exit while others are idle/between turns.)

User-visible effect on owner exit: a brief "reconnecting…" blip, then automatic
recovery. No orphan processes (every backend lives inside some CLI process).

## 7. No-auth for local connections

Modify `internal/api/http/server.go` `auth` middleware: if `r.RemoteAddr` host is
loopback (`127.0.0.1` or `::1`), skip the token check. `/healthz` already bypassed.

Consequences:
- Local TUI ↔ local backend: no token needed (loopback).
- `autocode chat -server http://127.0.0.1:8080`: no `-token` needed.
- Non-loopback clients still require the Bearer token (unchanged).

## 8. TUI (bubbletea + lipgloss + glamour)

**Stack:** `github.com/charmbracelet/bubbletea`, `.../bubbles` (textarea),
`.../lipgloss` (layout/style), `.../glamour` (markdown render).

**Layout** (full-screen, alt-screen):
```
┌─ autocode · in-process · :54321 · deepseek-v4-flash ────┐
│ <scrollable transcript>                                 │
│   user:      ...                                        │
│   assistant: ...markdown rendered by glamour...         │
│   🔧 fs_search "MergeToMain"  ✓                         │
│                                                         │
├─────────────────────────────────────────────────────────┤
│ > _                                  (Enter=nl, ⌃↵=send) │
└─────────────────────────────────────────────────────────┘
```

- **Transcript**: accumulates agent message chunks (rendered through glamour),
  interleaved with tool-call one-liners (`🔧 <name> <args> <status>`), expandable
  on demand. Scrollable (PgUp/PgDn, mouse wheel).
- **Input**: multi-line textarea. Submit binding finalized for terminal
  compatibility during implementation (plan: `Enter`=newline, `Ctrl+Enter`=send
  via kitty/CSI-u where supported, with an `Esc`+`Enter` fallback).
- **Streaming**: SSE events are translated to `tea.Msg`s and applied to the model
  incrementally; the in-progress assistant turn renders as it arrives.
- **Slash commands**: `/skill …`, `/dev …`, `/help`, `/clear` typed in the input
  and sent as the query (the backend already routes these).
- **Status bar**: connection mode (`in-process` / `connected :<port>`), model name.
- **Keybindings**: `Ctrl-C` cancels the current stream (graceful); a second
  `Ctrl-C` (or `Ctrl-D` on empty input) quits with graceful teardown.

## 9. CLI surface

| Command | Behavior |
|---|---|
| `autocode` (bare) | **Default → TUI**; discover / in-process fallback. |
| `autocode chat` | Same TUI (explicit alias). |
| `autocode chat --no-tui` | Legacy line-based SSE REPL (kept for scripting/fallback). |
| `autocode chat -server URL [-token T]` | Force remote; skip discovery. |
| `autocode chat -inprocess` | Force in-process; skip discovery. |
| `autocode serve` | Daemon — unchanged; writes lockfile. |
| `autocode goal`, `vcs-mcp` | Unchanged. |

## 10. ChatBackend interface

A thin abstraction so the TUI is testable with a fake, and so remote/in-process
share one implementation (both are HTTP/SSE):

```go
// internal/cli/backend.go
type StreamEvent struct {
    Kind       string // "agent_chunk" | "tool_call" | "tool_result" | "error" | "done"
    Text       string // agent_chunk
    ToolName   string // tool_call/tool_result
    ToolArgs   string // tool_call
    ToolStatus string // "running" | "ok" | "error"
    Err        error  // error
}

type ChatBackend interface {
    Stream(ctx context.Context, query string) (<-chan StreamEvent, error)
    Addr() string  // for the status bar
    Close() error
}
```

`httpBackend` implements it by POSTing `/api/v1/chat` and parsing the SSE stream.
A `fakeBackend` is used in tests (deterministic event sequences).

## 11. Scope

**v1 (MVP):**
- Lockfile discovery + self-healing in-process owner election (atomic lockfile).
- Loopback no-auth in the server.
- TUI: streaming transcript (glamour), multi-line input, tool-call one-liners,
  slash commands, status bar, Ctrl-C cancel / quit, graceful teardown.

**Later (explicitly out of v1):**
- Persistent TUI chat history beyond the store (scrollback restore across runs).
- Command palette, file-diff viewer, theming.
- Zero-blip detached-`serve`-spawn alternative to self-heal (§14).
- Graceful ownership handoff (no blip at all).

## 12. File structure

- `internal/lockfile/lockfile.go` (+`_test.go`) — `Lockfile{PID,Addr,Auth,Root,StartedAt,Version}`;
  `Path(root)`, `Write/Read/Alive/Remove`, atomic `Acquire`.
- `internal/cli/cli.go` — `Run(ctx, opts)`: resolve backend → launch TUI → teardown.
- `internal/cli/session.go` — discover/bootstrap + reconnect loop + owner election.
- `internal/cli/backend.go` (+`_test.go`) — `ChatBackend`, `httpBackend`, `fakeBackend`.
- `internal/cli/tui/model.go` — `tea.Model` (Init/Update/View).
- `internal/cli/tui/input.go` — textarea wrapper + submit handling.
- `internal/cli/tui/transcript.go` — render messages + tool calls.
- `internal/cli/tui/styles.go` — lipgloss styles + glamour renderer.
- `internal/cli/tui/model_test.go` — Update-logic tests.
- `internal/bootstrap/bootstrap.go` — add `App.Start(ctx) (addr string, shutdown func(), error)`
  for ephemeral-port in-process binding; expose graceful shutdown.
- `internal/api/http/server.go` (+ test) — loopback bypass in `auth`.
- `cmd/autocode/main.go` — bare invocation + `chat` → `cli.Run`; keep `--no-tui` legacy REPL.

## 13. Branching

Proposed: new branch `feat/cli-tui` off the **current** `feat/m8-autovcs` HEAD
(`1e66ec4`), so the CLI work is tracked separately but builds on the current
state (M8's bootstrap wiring). If/when M8 merges to main first, `feat/cli-tui`
rebases onto main. *Confirm branch strategy before implementation.*

## 14. Testing strategy

- **lockfile**: Path resolution per-OS (fake home via env), Write/Read round-trip,
  Alive (live vs dead PID), stale removal, atomic Acquire contention (two writers,
  one wins).
- **discovery/session**: given a fake live vs stale vs absent lockfile → correct
  mode; reconnect loop re-elects owner after simulated drop.
- **backend**: `httpBackend` against an `httptest` SSE server → events decoded.
- **server auth**: loopback bypasses token; non-loopback still requires it.
- **TUI**: `Update` logic (input accumulation, transcript growth, Ctrl-C cancel);
  view rendering snapshot where deterministic.
- **integration**: in-process `bootstrap.Build` + headless TUI driver sends a
  query, asserts streamed events (fake model for determinism) + one real-model
  smoke test.

## 15. Risks / non-goals / open details

- **Submit keybinding portability**: `Ctrl+Enter` needs kitty/CSI-u; finalize a
  documented fallback during implementation.
- **In-flight query loss on owner death**: accepted for v1 (§6); zero-blip model
  deferred (§11).
- **Conversation identity across windows**: whether multiple windows share one
  conversation or each has its own is an implementation detail of the existing
  `/api/v1/chat` session model — pin down in the plan.
- **Backward compat**: existing `serve` / `goal` / `vcs-mcp` behavior unchanged;
  `chat -server -token` still works for non-loopback.
