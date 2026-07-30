# autocode Lightweight VCS (autoVCS) — Design Spec

**Date:** 2026-07-15
**Status:** Approved (brainstormed 2026-07-15; revised same day to add `main` trunk)
**Phase:** M8 (follows M7 tools/skills/workflows + the ACP diff-application fix)
**Module:** `github.com/x6nux/autocode`

---

## 1. Goal

Give autocode a **custom, autocode-owned, SQLite-backed lightweight VCS** ("autoVCS") with a **`main` trunk** that all changes land on:

1. A global **`main`** (canonical trunk + history). The repo root is the **main working copy**.
2. **Auto-record every edit** — both **chat/orchestrator edits** (fs tools, committed to main) **and** task-assigned agent edits in **worktrees**.
3. **Worktrees** created at **task-assignment** time, **shareable across multiple agents**, **branched from `main_head`**.
4. **`vcs_merge`** integrates a worktree back into `main` (fast-forward or tree-level 3-way merge; both-side conflict → refuse + optional force).
5. **Snapshots** are content-addressed (blobs dedup'd); commits carry agent attribution, timestamp, message, source worktree.
6. Exposes git-like operations — `commit`, `log`, `diff`, `restore`, `merge` — as **tools** (GuardedTools for local/orchestrator agents; an **MCP server** delivered to ACP agents via `session/new` `mcpServers`).
7. **Ignores dependency/build dirs** (`node_modules`, `.git`, `vendor`, …).

No real git dependency. A deliberate **Option B** (custom, lightweight) design.

## 2. Background (what exists)

- **Store** (`internal/store/store.go`): SQLite (`modernc.org/sqlite`, pure Go) with `kv`, `sessions`, `messages`, `memories`(+FTS5), `tasks`. Schema is a single `const schema` run in `migrate()` (CREATE TABLE IF NOT EXISTS — idempotent). **No edit/change/worktree tables.**
- **`tasks` table** (`internal/store/task.go`): columns id/type/input/status/assigned_to/result/parent_task/created_at/updated_at/deadline/attempts. Adding `worktree_id` needs a **guarded migration** (ALTER TABLE is not idempotent; check `pragma table_info(tasks)` before altering).
- **Editing surfaces (no isolation/history today):**
  - `fs_write`/`fs_edit` (`internal/tools/fs.go`) — direct `os.WriteFile`.
  - ACP `applyDiffContent` (`internal/acp/client.go`, added post-M7) — materializes agent diffs to the session Cwd.
- **Goal loop** (`internal/agent/goalloop/implementer.go`): `WorktreeHelper` uses real git worktrees; `ACPImplementer.worker.run` creates one per worker. autoVCS **supersedes** this for tracked edits (WorktreeHelper stays for git-based isolation).
- **Task broker** (`internal/task/broker.go`): `Claim(worker)` assigns a pending task — the hook for worktree creation on the Task-API path.
- **ACP session** (`internal/acp/`): `Spawn` → `NewSession(cwd, extraDirs)` sends `session/new` with `mcpServers: {}` (must be present; adapters reject without it). The MCP delivery (§11) populates it.
- **Ignore matching**: `guard.MatchGlob` (exported, `internal/guard/glob.go`) — reused for VCS ignore.

## 3. Scope

**In scope:**
- `internal/vcs/` package: `main` trunk + worktrees + auto-record + commit/log/diff/restore/**merge** + ignore.
- Auto-track hooks in `fs_write`/`fs_edit` (→ main) and ACP `applyDiffContent` (→ worktree).
- `vcs_*` GuardedTools (incl. `vcs_merge`).
- Worktree creation at task assignment (goal-loop worker + task broker Claim), incl. shared multi-agent worktrees; worktrees under autocode's data dir.
- MCP server exposing `vcs_*` to ACP agents.
- SQLite schema migration (incl. guarded `tasks.worktree_id`).

**Out of scope (deferred):**
- **Line-level merge / conflict resolution** — v1 merge is tree-level (whole-file granularity); both-side-modified = conflict (refuse/force). Line-level 3-way is future.
- **Network sync / remote repos** — local SQLite only.
- **Binary file line-diffs** — binaries are snapshotted as blobs; diffed as "binary changed."
- Replacing the goal-loop's git `WorktreeHelper` (stays; autoVCS is additive).

## 4. Architecture

```
                         ┌──────────────────────┐
                         │       main           │  canonical trunk (main_head)
                         │  repo root = main WC │  chat/orchestrator edits land here
                         │  auto-track → commit │
                         └──────────┬───────────┘
              branch from main_head │            ▲ merge (FF / tree 3-way)
                                    ▼            │
   ┌─────────────────────────────────────┐  ┌──────────────────────────────┐
   │        Worktree (per task/team)      │  │  vcs_merge(wt) → main        │
   │  ~/.autocode/worktrees/<id>/        │  │  • FF if main unmoved         │
   │  edits (fs tools / ACP diffs)        │──┤  • tree 3-way if main moved   │
   │  auto-track → commit on wt branch    │  │  • conflict → refuse(+force) │
   └─────────────────────────────────────┘  └──────────────────────────────┘
        Surfaces: vcs_commit / vcs_log / vcs_diff / vcs_restore / vcs_merge
         • GuardedTools (orchestrator / local agents)
         • MCP server → ACP session/new mcpServers
```

`main` is the integration point. Worktrees are isolated branches that merge back.

## 5. Data Model (SQLite — appended to `store.schema`; guarded migration for the tasks column)

```sql
CREATE TABLE IF NOT EXISTS vcs_repos (
    id         TEXT PRIMARY KEY,
    root_path  TEXT NOT NULL,          -- the repo root = main working copy
    main_head  TEXT NOT NULL DEFAULT '',-- commit id of main's tip ('' = no commits yet)
    created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS vcs_worktrees (
    id          TEXT PRIMARY KEY,
    repo_id     TEXT NOT NULL,
    path        TEXT NOT NULL,          -- absolute working dir (under data dir)
    base_commit TEXT NOT NULL,          -- main_head at branch time
    created_at  INTEGER NOT NULL,
    active      INTEGER NOT NULL DEFAULT 1,
    FOREIGN KEY (repo_id) REFERENCES vcs_repos(id)
);
CREATE INDEX IF NOT EXISTS idx_vcs_worktrees_repo ON vcs_worktrees(repo_id);

CREATE TABLE IF NOT EXISTS vcs_commits (
    id              TEXT PRIMARY KEY,   -- content hash
    repo_id         TEXT NOT NULL,
    worktree_id     TEXT NOT NULL DEFAULT '', -- '' = a main commit; else the worktree it was created in
    parent_id       TEXT NOT NULL DEFAULT '',
    merged_from     TEXT NOT NULL DEFAULT '', -- for merge commits: the worktree merged in
    author          TEXT NOT NULL,      -- agent id (or 'orchestrator' for chat)
    message         TEXT NOT NULL DEFAULT '',
    created_at      INTEGER NOT NULL,
    FOREIGN KEY (repo_id) REFERENCES vcs_repos(id)
);
CREATE INDEX IF NOT EXISTS idx_vcs_commits_parent ON vcs_commits(parent_id);
CREATE INDEX IF NOT EXISTS idx_vcs_commits_worktree ON vcs_commits(worktree_id, created_at);

CREATE TABLE IF NOT EXISTS vcs_tree (       -- per-commit full path→hash snapshot
    commit_id  TEXT NOT NULL,
    path       TEXT NOT NULL,
    blob_hash  TEXT NOT NULL,
    PRIMARY KEY (commit_id, path)
);

CREATE TABLE IF NOT EXISTS vcs_blobs (      -- content-addressed, dedup'd (sha256)
    hash    TEXT PRIMARY KEY,
    content BLOB NOT NULL,
    size    INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS vcs_uncommitted ( -- pending changeset, keyed by scope
    scope_type TEXT NOT NULL,               -- 'main' | 'worktree'
    scope_id   TEXT NOT NULL,               -- repo_id (main) or worktree_id
    path       TEXT NOT NULL,
    blob_hash  TEXT NOT NULL,
    op         TEXT NOT NULL,               -- added | modified | deleted
    PRIMARY KEY (scope_type, scope_id, path)
);
```

`tasks.worktree_id` is added by a **guarded migration** in `migrate()`:
```go
// addColumnIfMissing checks pragma table_info and ALTERs only if the column is absent.
```
(Idempotent across re-runs.)

**Snapshot model:** each commit stores a full `path→blob_hash` map in `vcs_tree`. `main_head` points at main's tip. A worktree's tip = its latest commit. Diff = compare two trees. Merge = 3-way over trees (§7). Restore = materialize a blob.

## 6. Ignore Mechanism

- **Built-in default** ignore set (dir names + globs):
  `node_modules`, `.git`, `.hg`, `.svn`, `vendor`, `dist`, `build`, `.next`, `.nuxt`, `target`, `__pycache__`, `.venv`, `venv`, `.idea`, `.vscode`, `*.log`, `*.pyc`, `.DS_Store`.
- **Configurable** via `config.yaml`:
  ```yaml
  vcs:
    ignore: ["coverage", ".cache"]
    worktree_dir: "~/.autocode/worktrees"   # default; expanded (~ → home)
  ```
- **Repo-local** `.autocodeignore` (gitignore-style, merged; one level of globs).
- **Matching:** reuse `guard.MatchGlob` against forward-slash relative paths; `WalkDir` returns `SkipDir` for ignored dirs. Consulted in `RecordEdit`, `Commit`/snapshot, and the initial repo scan.

## 7. Core Package `internal/vcs/`

```go
type VCS struct {
    store       *store.Store
    ignore      []string           // defaults + config + .autocodeignore, merged
    worktreeDir string             // where worktree working dirs live
}

func New(s *store.Store, worktreeDir string, ignore ...string) *VCS

// Repo + main
func (v *VCS) InitRepo(root string) (repoID string, err error)
//   scan root (respecting ignore) → initial commit on main; set main_head.

// Worktree lifecycle (branched from main_head)
func (v *VCS) AddWorktree(repoID string, agents []string) (*Worktree, error)
//   base_commit = current main_head; copy main's tip tree → <worktreeDir>/<id>/.
func (v *VCS) RemoveWorktree(wtID string) error

// Auto-track (called by fs tools + ACP diff hook). scope = main or a worktree.
func (v *VCS) RecordEditMain(repoID, agent, path string, content []byte) error
func (v *VCS) RecordEditWorktree(wtID, agent, path string, content []byte) error
//   - ignore-match → skip
//   - hash content → upsert vcs_blobs
//   - upsert vcs_uncommitted {scope, path, hash, op}

// Commits
func (v *VCS) CommitMain(repoID, agent, message string) (commitID string, err error)
func (v *VCS) CommitWorktree(wtID, agent, message string) (commitID string, err error)
//   new commit: tree = parent_tree ∪ uncommitted; clear that scope's uncommitted.

// Merge a worktree into main
func (v *VCS) MergeToMain(wtID, agent string, force bool) (mergeCommitID string, conflicts []string, err error)
//   base = wt.base_commit; ours = main_head tree; theirs = wt tip tree.
//   per path: one-side change → take it; both-side change → conflict.
//   conflicts && !force → return conflicts, no commit.
//   conflicts && force → theirs wins (worktree version). FF when main unmoved.

// Queries
func (v *VCS) LogMain(repoID string, limit int) ([]Commit, error)
func (v *VCS) LogWorktree(wtID string, limit int) ([]Commit, error)
func (v *VCS) Diff(repoID, refA, refB string) ([]FileDiff, error)   // "" = main_head / wt tip
func (v *VCS) Restore(scope, ref, path string) error                // materialize a blob to disk

type Worktree struct{ ID, RepoID, Path, BaseCommit string }
type Commit struct{ ID, Author, Message, MergedFrom string; CreatedAt int64; ParentID string; FilesChanged int }
type FileDiff struct{ Path, Op string; OldHash, NewHash string }
```

**Concurrency:** edits serialize through the store's single-writer connection. A shared worktree's commit captures the accumulated changeset attributed to the committing agent. `MergeToMain` is a single transaction (read main_head + wt tip → write merge commit + advance main_head).

## 8. Auto-track Integration

The active **scope** (main repo vs worktree) + **agent id** travel in the acting context (same mechanism as the permission profile).

- **`fs_write`/`fs_edit`**: after a successful write, call the matching `RecordEdit*`:
  - If a **worktree** is bound in context → `RecordEditWorktree(wtID, agent, absPath, content)`.
  - Else (chat/orchestrator, no task) → `RecordEditMain(repoID, agent, absPath, content)` — chat edits track to **main**.
  - Reuse the content just written. Ignore-match → skip. No repo initialized → no-op (tracking is opt-in via `vcs init`).
- **ACP `applyDiffContent`**: after writing the diff, `RecordEditWorktree(wtID, agent, resolved, content)` (the ACP session carries its worktree id — §9).
- **Deletions** (edit-to-empty / future `fs_delete`): record `op=deleted`.

The `vcs.VCS` instance is built in `bootstrap` and injected into the tools + ACP client.

## 9. Worktree Lifecycle (task-assignment + shared; branched from main)

- **Creation at task assignment:**
  - **Goal-loop** (`ACPImplementer.worker.run`): replace the `WorktreeHelper.Add` git call with `vcs.AddWorktree(repo, agents)` (branches from `main_head`). Bind the worker + its ACP session to that worktree.
  - **Task-broker** (`broker.Claim(worker)`): on claim, create/attach a worktree (set `tasks.worktree_id`). Shared worktrees: multiple tasks/workers share one `worktree_id` (a team plan assigns the same worktree to its workers).
- **ACP binding:** `acp.SpawnOptions` gains a `WorktreeID` field; `applyDiffContent` attributes edits to it.
- **Shared worktree:** `AddWorktree(repo, agents)` registers once; each agent's session/task references the same `worktree_id`; all their edits land in one changeset + history.
- **Merge:** on task/team completion, `vcs_merge` integrates the worktree into main (the team-feature T3 path calls it after integration). Worktrees retain their own history for audit even after merge.
- **Location:** worktree working dirs live under `worktreeDir` (default `~/.autocode/worktrees/<id>/`), NOT in the user's repo.

## 10. Tools `internal/tools/vcs.go` (GuardedTools)

All gated by the tools allow-list (no FS/shell dimension — they operate on the store + the bound working copy). The "active scope" (main/worktree) + agent come from context.

| Tool | Params | Behavior |
|------|--------|----------|
| `vcs_commit` | `message`(req) | snapshot the active scope's changeset (main for chat, worktree for task-agent) |
| `vcs_log` | `scope`(enum: `main`\|`worktree`, default active), `limit`(int, default 20) | history |
| `vcs_diff` | `ref_a`, `ref_b`(optional; default active-vs-HEAD) | file-level diff |
| `vcs_restore` | `ref`(req), `path`(req) | restore a file into the active working copy |
| `vcs_merge` | `worktree`(req), `force`(bool) | merge a worktree into main; returns conflicts if any (and !force) |

`InitRepo`/`AddWorktree` are **internal** (bootstrap + task-assignment), not agent tools.

## 11. ACP Delivery — MCP Server `internal/vcs/mcp/`

A minimal MCP server (JSON-RPC over stdio) exposing `vcs_commit`/`vcs_log`/`vcs_diff`/`vcs_restore`/`vcs_merge` to ACP agents.

- `internal/vcs/mcp/server.go`: implements MCP `initialize`, `tools/list`, `tools/call` over stdio JSON-RPC. Each tool maps to the `vcs.VCS` operation, scoped to the session's worktree (ACP agents operate in a worktree, so their `vcs_commit` lands on their branch; `vcs_merge` integrates to main).
- **Wiring:** ACP `Spawn`/`session/new` `mcpServers` includes the autoVCS MCP server (currently `{}`). The server runs as an in-process stdio pair. Agent attribution carries through.
- Auto-track remains always-on; the MCP server gives agents **explicit** commit/log/diff/merge control.

## 12. Security

- **Ignore** keeps `node_modules`/secrets-out-of-tracked-roots from ballooning storage; autocode does not inspect content for secrets (documented).
- **Scope enforcement:** `RecordEdit*` and tools only touch the bound scope's paths (resolve + prefix-check, like `skills.Registry.ReadFile`). A worktree agent can't write to main or another worktree via VCS ops.
- **Guard:** `vcs_*` tools go through `GuardedTool` (tools allow-list). MCP tools are scoped to the session's worktree.
- **Attribution integrity:** author = acting agent id from context; not forgeable via tool args.
- **Blob storage:** `vcs_blobs` holds tracked source; treat the SQLite file as sensitive (documented).

## 13. Testing Strategy

- **VCS core** (`internal/vcs/`, in-memory store): InitRepo (ignore excludes node_modules), AddWorktree (copies main tip; base=main_head), RecordEdit (main + worktree), Commit (snapshot, parent chain, attribution, scope isolation), LogMain/LogWorktree, Diff (add/modify/delete), Restore (round-trip), **MergeToMain** (fast-forward; tree 3-way one-side; both-side conflict → refuse; force → theirs wins; main_head advances).
- **Auto-track:** fs_write/fs_edit with worktree context → worktree changeset; without (chat) → main changeset; no repo init → no-op. ACP diff with session worktree → recorded, attributed.
- **Shared worktree:** two agents → one changeset; commit by B attributes to B, captures A's pending edits.
- **Merge integration:** two worktrees branch from main; one merges (FF), the second merges (3-way or conflict).
- **Tools:** each `vcs_*` round-trip (allow/deny by profile); `vcs_merge` returns conflicts.
- **MCP server:** `tools/list` returns 5 tools; `tools/call vcs_commit`/`vcs_merge` scoped to session worktree.
- All pure-Go, in-memory store, no network, no CGO.

## 14. Phasing (M8)

- **M8-T1 VCS core:** store schema (incl. guarded `tasks.worktree_id` migration) + `internal/vcs/` (InitRepo, AddWorktree, RecordEdit*, Commit*, Log*, Diff, Restore, **MergeToMain**, ignore, worktreeDir).
- **M8-T2 Auto-track integration:** hooks in `fs_write`/`fs_edit` (main + worktree) + ACP `applyDiffContent`; scope+agent in context; bootstrap VCS injection.
- **M8-T3 `vcs_*` GuardedTools:** commit/log/diff/restore/merge + config `vcs:` block (ignore + worktree_dir).
- **M8-T4 Worktree lifecycle:** goal-loop `worker.run` + task-broker `Claim` create/attach worktrees (branched from main_head); shared multi-agent worktrees; ACP `Spawn` WorktreeID binding; merge-on-completion for the team path.
- **M8-T5 MCP server:** `internal/vcs/mcp/` + ACP `session/new` `mcpServers` population.
- **M8-T6 E2E + docs:** chat edit → main commit; task-agent → worktree → merge to main; conflict path; README + authoring docs.

## 15. Risks & Open Questions

- **R1 — Snapshot storage growth:** full `path→hash` map per commit = O(files) rows; dedup'd blobs keep byte-storage flat. v1 acceptable; tree compaction deferred.
- **R2 — Shared-worktree semantics:** concurrent agents interleave; one's commit captures others' pending edits. Documented v1 behavior.
- **R3 — Merge = tree-level only:** both-side-modified file = conflict (refuse/force), no line-level resolution. Real 3-way line merge is future.
- **R4 — MCP server scope:** T5 is the largest task; if it slips, v1 ships T1–T4 (auto-track + GuardedTools + merge; ACP agents get auto-record) and defers explicit ACP tools. Decision: include per "1 + α".
- **OQ (resolved this revision):** chat edits now commit to **main** (single canonical history); worktrees under autocode's **data dir** (`vcs.worktree_dir`).

## 16. Acceptance Criteria

1. `internal/vcs/` implements InitRepo/AddWorktree/RecordEdit*/Commit*/Log*/Diff/Restore/**MergeToMain** + ignore, unit-tested.
2. Auto-track records fs edits to **main** (chat) or the **worktree** (task-agent), and ACP-diff edits to the worktree — attributed to the acting agent.
3. Worktrees branch from `main_head`, are created at task assignment, shareable among agents, and live under autocode's data dir.
4. `vcs_merge` integrates a worktree into `main` (fast-forward or tree 3-way); both-side conflict is refused (force overrides).
5. `node_modules`/`.git`/`vendor`/etc. are never tracked.
6. `vcs_commit`/`vcs_log`/`vcs_diff`/`vcs_restore`/`vcs_merge` GuardedTools work for local/orchestrator agents.
7. The MCP server exposes the same five tools to ACP agents via `session/new` `mcpServers`.
8. E2E: chat edit → main commit; task-agent edits in a worktree → `vcs_merge` → change visible on main's log; conflict path refuses cleanly.
9. `go build ./...`, `go vet ./...`, `go test ./...` all pass; no CGO; no network in tests.
