# autoVCS

Yanshi ships a lightweight, SQLite-backed version-control system (autoVCS)
that tracks every edit agents make and exposes git-like commit/log/diff/restore/
merge operations. It lives in `internal/vcs`, stores blobs/commits/trees in the
same SQLite database as the rest of Yanshi, and needs no cooperation from the
agent beyond editing files through Yanshi's tools.

## The main / worktree model

- **`main`** is the canonical trunk. The repository root (Yanshi's working
  directory) is `main`'s working copy, and `main_head` points at its newest
  commit.
- **Worktrees** branch from `main_head` at creation time (typically when a task
  is assigned). A worktree's working dir is materialized under Yanshi's data
  dir — `~/.yanshi/worktrees/<id>/` by default (configurable via
  `vcs.worktree_dir`) — with the same repo-relative layout as `main`.
- Worktrees are **shareable**: multiple agents/sessions can reference the same
  worktree id (e.g. a task claimed by one worker, inspected by another).
- A worktree **merges back** into `main` via a tree-level 3-way merge
  (`base` = the worktree's `base_commit`, `ours` = `main_head`, `theirs` = the
  worktree tip).

## Auto-tracking (no agent cooperation needed)

Every edit funnels through Yanshi's tools, which record the new file content
into the appropriate pending changeset:

- **Chat / orchestrator edits** (`fs_write`, `fs_edit` from chat) → `main`.
- **Task-agent edits** (goal-loop worker `fs_*` calls) → that task's worktree.
- **ACP agent diffs** (external clients like claudecode/codex) → the session's
  worktree, bridged from the ACP diff callback.

Edits outside the repo root or matching an ignore pattern are silently skipped.
Nothing is committed automatically; an edit only becomes history when an agent
calls `vcs_commit`.

## Ignore

A path is ignored if any of:

1. It matches a **built-in default** — `node_modules`, `.git`, `.hg`, `.svn`,
   `vendor`, `dist`, `build`, `.next`, `.nuxt`, `target`, `__pycache__`, `.venv`,
   `venv`, `.idea`, `.vscode`, `*.log`, `*.pyc`, `.DS_Store`, `*.db`,
   `yanshi.db`.
2. It matches an entry in the config **`vcs.ignore`** list (merged with the
   defaults).
3. It matches a line in a repo-root **`.yanshiignore`** file (one glob per
   line, `#` comments allowed).

`*.db` / `yanshi.db` are ignored so Yanshi's own SQLite store is never
scanned into the initial commit.

## Tools

Five operations, exposed two ways:

- **GuardedTools** (`vcs_*`) for local agents (orchestrator, task workers) —
  registered in `internal/tools/vcs.go` and routed by the active VCS scope.
- **MCP server** for ACP agents, delivered to the client via `session/new`
  `mcpServers` and runnable standalone as `yanshi vcs-mcp`
  (`internal/vcs/mcp`).

| Tool | Action |
| --- | --- |
| `vcs_commit` | Snapshot the active scope's pending edits as a commit (author = acting agent). |
| `vcs_log`   | List commits for the active scope (newest-first). |
| `vcs_diff`  | File-level changes between two commits (defaults to active head vs its parent). |
| `vcs_restore` | Write a file's content from a commit back into the working copy. |
| `vcs_merge` | Merge a worktree into `main` (tree-level 3-way); returns conflicts; `force` overrides. |

Each tool routes to `main` or the active worktree based on the scope's
`WorktreeID`.

## Merge policy

`vcs_merge` integrates a worktree into `main`:

- **Fast-forward** when `main_head` == the worktree's `base_commit` (main hasn't
  moved since the branch) — the merged tree is exactly `theirs`, no conflict
  possible.
- **Tree-level 3-way** otherwise: per path, if only one side changed, that side
  wins; if both sides changed the same path differently, it is a conflict.
- **Both-side conflict → refused** when `force` is false: no commit is created,
  the tool returns the conflict path list.
- **`force` overrides**: the worktree version wins on conflicted paths and the
  merge proceeds.

Line-level merge is future work; the unit of conflict today is a whole file.

## How it flows (task-agent)

1. A task is assigned; the broker claims a worktree branched from `main_head`,
   and the worker agent is spawned with that worktree as its scope/working dir.
2. The agent edits files via `fs_*` tools; each edit auto-tracks into the
   worktree's pending changeset.
3. The agent calls `vcs_commit { message }` to snapshot its edits as a commit on
   the worktree tip.
4. The agent (or an integrator) calls `vcs_merge { worktree }`. On success the
   changes land on `main`; on conflict it retries or passes `force: true`.
