# 逐轮回滚 UI (B2-RB1) Implementation Plan — 重写版

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 每个 user turn 前后在 autoVCS main scope 打一条轻量 seam(只记 `commit_id` 引用,不复制完整树,不动用户自己的 `.git`);WS `/restore-turn` 能把 main 工作副本、VCS head 与 durable conversation history 回到指定 seam，agent `revert_turn` 提供明确标注为 VCS-only 的文件/head 回滚;每次回滚都返回 undo seam id;与 goalloop/ACP worktree 边界明确(只管 main 工作副本)。

**Architecture:** 不重写 VCS。① 新增一张轻量表 `vcs_seams`(单调 `seq` 排序、`session_id` 隔离、`history_len` 记录历史边界)—— seam 行只是对 `vcs_commits.id` 的命名指针(commit 本身已内容寻址 + 增量 delta 存储)。② `internal/vcs/seam.go` 承载 `SealMainTurnSeam` / `ListSeams` / `FindSeam` / `RepoMainHead`;`internal/vcs/revert.go` 承载 `MaterializeMain` / `ResetMainHead` / `RevertToSeam`,保持 `vcs.go` 不超 1000 纯代码行(当前已 1027 行,所有新方法必须拆到新文件)。③ **per-repo** 写串行化(`repoLocks map[string]*sync.Mutex` + `repoLocksMu sync.Mutex` 索引互斥),覆盖**所有**公共写路径(`InitRepo`/`AddWorktree`/`RemoveWorktree`/`RecordEditMain`/`RecordEditWorktree`/`CommitMain`/`CommitWorktree`/`MergeToMain`/`Restore`/`SealMainTurnSeam`/`RevertToSeam`/新增 `MaterializeMain`/`ResetMainHead`),避免 WS/SSE/orchestrator/task broker 共享一个 VCS 实例时的竞态;不同 repo 互不阻塞。`Restore` 原先走裸 `os.WriteFile` 绕过 pending 追踪,改为经由 `recordEdit` 写 `vcs_uncommitted`(否则 `vcs_restore` 工具破坏 pending 一致性)。④ turn 边界接线在 WS 与 SSE 两条传输上对称执行(pre/post seam),通过 `Server.sealTurnBoundary` 统一入口。⑤ WS 把 `user_message` case 抽成 `runUserTurn` 闭包,用 `defer`(release + post-turn seam)保证 post-turn seam 在正常 / 错误 / cancel / disconnect / panic 所有路径都执行;**保留** schema-retry 内层 for-loop 的 `continue`(只把外层 select 重派用的 `continue` 改成 `return`);reader goroutine 新增 `list_seams` / `restore_turn` bypass,通过 `atomic.Bool` 的 `inTurn` 标志在 turn 进行中即时拒绝(reader 不持任何锁调用 `conn.write`)。⑥ `revert_turn` agent 工具调用新的 `tools.RequireApproval`(强制提示,`PermissionRequest.Force=true` 使 WS callback 跳过 yolo/auto/allow-edits 模式解析,无 callback / timeout / cancel / deny 全部 fail-closed);`/restore-turn` TUI 路径在 `yes` token 之外再携带 `confirmed_head`(**完整** commit id)绑定,服务端再次校验且**要求非空**。⑦ SSE 无交互 callback → `revert_turn` 工具在 SSE turn 中执行时强制 fail-closed,但 pre/post seam lifecycle 仍然执行。⑧ 回滚返回的 `seam_restored.ID` 是指向回滚前 head 的 undo seam;undo seam 的 `prev_history_len`/`prev_turn_seq` 捕获回滚前边界,`history_snapshot` 保存同一时刻的 exact durable messages/meta(JSON BLOB)。三者与 head/undo seam 在同一 VCS SQLite tx 中写入,因此再次 restore undo seam 能恢复文件、head 和第一次回滚删除的会话历史(D2)。⑨ Server 结构体加 `vcs *vcs.VCS` + `repoID string` 字段,由 bootstrap 经 `apihttp.Config` 装配;`revert_turn` 加入 `VCSTools.Tools()` 后自动注册。⑩ **scope cut**(见 Limitations):agent `revert_turn` 工具仅回滚 main 文件 + head,不截断会话历史(D4);`MaterializeMain` 用快照-回滚而非真正的原子目录交换(D1)。

**Tech Stack:** Go 1.26.4 · 标准库 `database/sql`、`os`、`path/filepath`、`strconv`、`sync`、`sync/atomic`、`time` · 不引第三方 · 复用 `internal/vcs`、`internal/store`、`internal/proto`、`internal/api/http`、`internal/cli/tui` 现有结构与助手。

**Spec:** `docs/feature-roadmap-codex-deepseek.md` §7 [RB1]。参考 `reference/deepseek-tui/crates/tui/src/snapshot/repo.rs`(side-git 模型)、`reference/deepseek-tui/crates/tui/src/commands/restore.rs`、`reference/deepseek-tui/crates/tui/src/tools/revert_turn.rs`。

---

## 与 [LSP1] 的边界

B2 批次的另一项 [LSP1] 已有独立计划 `docs/superpowers/plans/2026-07-21-lsp-diagnostics.md`。本计划**只做 [RB1]**,不碰 LSP 相关包。两个特性相互独立,可并行或任意顺序落地。

---

## A–K 必修项覆盖矩阵

| 必修项 | 覆盖 Task | 关键落点 |
|---|---|---|
| A: 回滚可逆(undo seam) | Task 1 + Task 5 + Task 9 | `prev_turn_seq`/`prev_history_len` 捕获回滚前会话边界;`RevertToSeam` 返回 `SeamPreRevert`(指向 previousHead);WS 再次 restore undo seam 同时恢复历史;往返测试 |
| B: per-repo 串行化 | Task 2 | `repoLocks map[string]*sync.Mutex` + 索引锁;覆盖 `InitRepo`/`AddWorktree`/`RemoveWorktree`/`RecordEdit*`/`Commit*`/`MergeToMain`/`Restore`/`SealMainTurnSeam`/`RevertToSeam`/`MaterializeMain`/`ResetMainHead`;不同 repo 不互相阻塞;`go test -race` |
| C: WS 同步主循环 + defer | Task 9 | `atomic.Bool`;`runUserTurn` 顶部按 LIFO 安排 post seam → release → `inTurn=false`;仅外层 re-loop 的 `continue` 改 `return`,保留 schema-retry `continue` |
| D: SSE 对称 | Task 10 | 抽 `handleSSEInternal`;真实 Fake orchestrator 测试;pre/post seam 都用 `defer`(panic/early-return safe);SSE 无 control frame,不添加 `seam_restored` dead case |
| E: destructive approval | Task 6 + Task 9 + Task 11 | `PermissionRequest.Force`;WS callback 遇 `Force` 跳过模式自动放行,每次都 prompt;`revert_turn` fail-closed;`/restore-turn` 要求完整 non-empty `confirmed_head` |
| F: post-turn seam 全路径 | Task 9 + Task 10 | WS `runUserTurn` defer + SSE handler defer 覆盖 normal/model-error/tool-error/cancel/disconnect/panic;针对性测试 |
| G: 原子性 + 完整性 | Task 2 + Task 4 + Task 5 + Task 9 | strict blob 读取;Materialize 失败从 pre-revert 文件快照全量恢复,不推进 head;Windows delete-then-rename;`Restore` 经 recordEdit;DB truncate→memory truncate→VCS revert fail-closed;commit 归属 + path containment 校验 |
| H: 稳定排序 | Task 1 + Task 3 | `seq INTEGER PRIMARY KEY AUTOINCREMENT`;`ORDER BY seq DESC`;`created_at` 仅作展示;同秒插入测试 |
| I: 协议/TUI 缺口 | Task 6 + Task 12 | 真实 `command{name,help,run}` / `cmdRestoreTurn(model,args)(tea.Model,tea.Cmd)` / `m.sess.Mode()` / lowercase `render`;`isControlReply` 加 `seams`/`seam_restored`;`applyEvent` error 清 pending |
| J: turn_id 跨 session | Task 1 + Task 3 + Task 6 + Task 9 | seam 表 `session_id`;WS `ListSeams` 必须 exact `cs.sessionID`(空 session→空表),restore 再校验 `seam.SessionID==cs.sessionID`;selector 只用 exact `seam_id` |
| K: TDD + decision-complete | 全部 | 每个 Task 先 RED 再 PASS;测试放新文件(`seam_migration_test.go` 等);无 stub / 无"executor 决定";集成测试用 `RecordEditMain` 而非裸 `os.WriteFile`;不宽容 `ErrNoChanges` |

---

## File Structure

| 文件 | 职责 | 新建/改 |
|---|---|---|
| `internal/store/store.go` | schema 常量追加 `vcs_seams` 表 + 索引 | 改 |
| `internal/store/session.go` | `SessionRevertSnapshot` + JSON encode/decode + transactional snapshot/truncate/restore APIs | 改 |
| `internal/store/seam_migration_test.go` | seam 表 schema(含 undo pre-state + `history_snapshot` 列)锁定测试 | 新建 |
| `internal/store/seam_truncate_test.go` | durable snapshot JSON、truncate、compensation/undo expansion 测试 | 新建 |
| `internal/vcs/vcs.go` | 加 per-repo lock registry;全部公共写方法按 repo 加锁;`Restore` 改经 `recordEdit` 追踪 pending | 改 |
| `internal/vcs/seam.go` | `SealMainTurnSeam`/`FindSeam`/`ListSeams`/`RepoMainHead`/`Seam`/`SeamKind` | 新建 |
| `internal/vcs/revert.go` | lock-free private core + public locked `MaterializeMain`/`ResetMainHead`/`RevertToSeam`;失败全量文件回滚 | 新建 |
| `internal/vcs/seam_test.go` | seam 单元测试 | 新建 |
| `internal/vcs/seam_race_test.go` | repo mutex 并发测试(`go test -race`) | 新建 |
| `internal/vcs/revert_test.go` | 回滚 + 可逆性 + 原子性测试 | 新建 |
| `internal/proto/frame.go` | 加 `SeamInfo`/`NewListSeams`/`NewRestoreTurn`/`NewSeams`/`NewSeamRestored`;`ClientFrame.ConfirmedHead`;`ServerFrame.Seams`/display `CommitShort`/full `Head` | 改 |
| `internal/proto/seam_frame_test.go` | seam 帧往返测试 | 新建 |
| `internal/tools/permctx.go` | `PermissionRequest` 加 `Force bool`;加 `RequireApproval`(强制提示,不被 yolo/auto/allow-edits/static allow/always_allow 绕过) | 改 |
| `internal/tools/vcsctx.go` | 明确不加 history callback:agent `revert_turn` scope cut 为 VCS-only | 仅阅读 |
| `internal/tools/require_approval_test.go` | `RequireApproval` fail-closed + Force/YOLO/non-sticky 测试 | 新建 |
| `internal/tools/vcs.go` | 加 `Revert *GuardedTool`(名 `revert_turn`)+ `Tools()` 注册 + `runRevert` | 改 |
| `internal/tools/vcs_revert_test.go` | revert_turn 工具测试(profile + worktree 拒绝 + approval) | 新建 |
| `internal/api/http/server.go` | `Server` 加 `vcs *vcs.VCS` / `repoID string`;`Config` 加 `VCS`/`RepoID`;`New` 装配 | 改 |
| `internal/api/http/server_seam_test.go` | Server VCS 装配测试 | 新建 |
| `internal/api/http/ws.go` | `connSession` 加 `inTurn atomic.Bool`;抽 `runUserTurn`;pre/post seam;reader bypass;`list_seams`/`restore_turn` handler | 改 |
| `internal/api/http/ws_seam_test.go` | seam lifecycle + restore + cancel + model-error + disconnect 全路径测试 | 新建 |
| `internal/api/http/chat.go` | 抽 `handleSSEInternal`;SSE turn pre/post seam 都用 defer;`revert_turn` 经 `RequireApproval` 在 SSE fail-closed | 改 |
| `internal/api/http/sse_seam_test.go` | 真实 Fake orchestrator 驱动 SSE seam lifecycle 测试 | 新建 |
| `internal/bootstrap/bootstrap.go` | 把 `vcsInstance`/`vcsRepoID` 传入 `apihttp.Config{VCS:, RepoID:}` | 改 |
| `internal/cli/backend.go` | `StreamEvent` 加 `Seams`/`CommitShort`/full `Head`/`UndoSeamID` 字段 | 改 |
| `internal/cli/wsbackend.go` | `isControlReply` 加 `seams`/`seam_restored`;`toStreamEvent` 映射 seam 字段 | 改 |
| `internal/cli/wsbackend_seam_test.go` | wsbackend seam round-trip 测试 | 新建 |
| `internal/cli/tui/model.go` | `model` 加 `pendingSeamRestore`/`lastKnownHead`;`applyEvent` 加 `seams`/`seam_restored`/`error` 清 pending | 改 |
| `internal/cli/tui/commands.go` | 加 `/restore-turn` 到 `commandTable` + `cmdRestoreTurn` | 改 |
| `internal/cli/tui/entries.go` | `seamsEntry`/`seamRestorePromptEntry`/`seamRestoredEntry` 渲染(加 `proto` import) | 改 |
| `internal/cli/tui/restore_turn_test.go` | `/restore-turn` + applyEvent seam 测试 | 新建 |
| `internal/api/http/ws_rollback_integration_test.go` | 端到端回滚集成测试 | 新建 |

**依赖方向:** `vcs/seam.go` / `vcs/revert.go` 是 `vcs` 包内新文件,依赖 `store`;`tools/vcs.go` 依赖 `vcs`;`proto` 独立;`api/http` 依赖 `proto` + `vcs`;`bootstrap` 装配 `apihttp` + `vcs`;`cli/tui` 依赖 `proto`。全部沿现有六边形向内流动。

---
## Task 1: store schema + durable revert snapshot codec

**Files:**
- Modify: `internal/store/store.go`(schema 常量末尾)
- Modify: `internal/store/session.go`(先定义 `SessionRevertSnapshot` 与 JSON codec，供 Task 5 顺序编译)
- Test: `internal/store/seam_migration_test.go`(新建)

> schema 改动必须先于 vcs 代码落地。把 `vcs_seams` 放在 store.go 的 `schema` 常量里(与其他 vcs_* 表一致),避免运行期 CREATE TABLE 竞态。`seq INTEGER PRIMARY KEY AUTOINCREMENT` 保证同秒插入的稳定排序;`session_id` 让 seam 跨连接可分组;`history_len` 记录 seal 时刻的会话长度,用于回滚时截断。
>
> **顺序依赖(D2):** Task 5 的 `vcs.RevertToSeam` 签名及实现会直接引用 `store.SessionRevertSnapshot` / `store.EncodeSessionRevertSnapshot`。因此类型与纯 JSON codec 必须在本 Task 先落地；Task 8 只添加依赖数据库 transaction 的 snapshot/truncate/restore methods。这样按 Task 1→13 顺序执行时，每个 GREEN checkpoint 都能独立编译。

- [ ] **Step 1: 写失败测试** — `internal/store/seam_migration_test.go`(新建,独立文件避免与 `store_test.go` 重复 `package`/`import`)

```go
// internal/store/seam_migration_test.go
package store

import (
	"encoding/json"
	"testing"
)

// TestVCSSeamsSchema_LocksColumnsAndIndexes validates the vcs_seams table shape.
// It prevents accidental schema drift: a drop of any column or index would
// change the PRAGMA / sqlite_master output and fail this test.
func TestVCSSeamsSchema_LocksColumnsAndIndexes(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	wantCols := map[string]bool{
		"seq": true, "id": true, "repo_id": true, "session_id": true,
		"commit_id": true, "turn_seq": true, "history_len": true,
		"prev_turn_seq": true, "prev_history_len": true,
		"history_snapshot": true,
		"kind": true, "label": true, "created_at": true,
	}
	rows, err := s.DB.Query("PRAGMA table_info(vcs_seams)")
	if err != nil {
		t.Fatalf("vcs_seams 不存在或不可读: %v", err)
	}
	gotCols := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt interface{}
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan: %v", err)
		}
		gotCols[name] = true
		if name == "seq" && (typ != "INTEGER" || pk != 1) {
			t.Errorf("seq 列必须是 INTEGER PRIMARY KEY,实际 typ=%s pk=%d", typ, pk)
		}
	}
	rows.Close()
	for c := range wantCols {
		if !gotCols[c] {
			t.Errorf("vcs_seams 缺少列 %q", c)
		}
	}

	idxRows, err := s.DB.Query("SELECT name FROM sqlite_master WHERE type='index' AND tbl_name='vcs_seams'")
	if err != nil {
		t.Fatalf("查询 vcs_seams 索引失败: %v", err)
	}
	gotIdx := map[string]bool{}
	for idxRows.Next() {
		var n string
		_ = idxRows.Scan(&n)
		gotIdx[n] = true
	}
	idxRows.Close()
	for _, want := range []string{"idx_vcs_seams_repo_seq", "idx_vcs_seams_session"} {
		if !gotIdx[want] {
			t.Errorf("vcs_seams 缺少索引 %q", want)
		}
	}
}

// TestOpen_VCSeamsIdempotent verifies the vcs_seams DDL is idempotent.
func TestOpen_VCSeamsIdempotent(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if err := s.migrate(); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	s.Close()
}
// TestSessionRevertSnapshotCodec_RoundTripsExactState locks the D2 payload
// contract before vcs.RevertToSeam starts depending on it in Task 5.
func TestSessionRevertSnapshotCodec_RoundTripsExactState(t *testing.T) {
	snap := SessionRevertSnapshot{
		Meta: SessionSummary{ID: "session-1", Turns: 2},
		Messages: []Message{
			{ID: "m1", SessionID: "session-1", Seq: 0, Role: "user", Content: "hello", CreatedAt: 11},
			{ID: "m2", SessionID: "session-1", Seq: 1, Role: "assistant", Content: "world", CreatedAt: 12},
		},
	}
	blob, err := EncodeSessionRevertSnapshot(snap)
	if err != nil {
		t.Fatalf("EncodeSessionRevertSnapshot: %v", err)
	}
	if !json.Valid(blob) {
		t.Fatal("encoded snapshot is not valid JSON")
	}
	got, err := DecodeSessionRevertSnapshot(blob)
	if err != nil {
		t.Fatalf("DecodeSessionRevertSnapshot: %v", err)
	}
	if got.Meta.ID != snap.Meta.ID || got.Meta.Turns != snap.Meta.Turns ||
		len(got.Messages) != 2 || got.Messages[0] != snap.Messages[0] ||
		got.Messages[1] != snap.Messages[1] {
		t.Fatalf("round-trip mismatch: got=%+v want=%+v", got, snap)
	}
}

// TestSessionRevertSnapshotCodec_RejectsEmptyPayload locks fail-closed decode.
func TestSessionRevertSnapshotCodec_RejectsEmptyPayload(t *testing.T) {
	if _, err := DecodeSessionRevertSnapshot(nil); err == nil {
		t.Fatal("empty undo snapshot must be rejected")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/store/ -run 'TestVCSSeamsSchema|TestSessionRevertSnapshotCodec' -v`
Expected: 编译失败，`SessionRevertSnapshot` / `EncodeSessionRevertSnapshot` / `DecodeSessionRevertSnapshot` 未定义；在 codec 符号补齐后，schema 断言仍因 `vcs_seams` 表不存在而 FAIL。

- [ ] **Step 3: 添加 schema 与先行 snapshot codec**

3a. 在 schema 常量(`internal/store/store.go` 的 `vcs_uncommitted` 表定义后、闭合反引号前)追加:

```go
// vcs_seams 存"逐轮 seam 快照"——每条行只是对 vcs_commits.id 的命名指针
// (commit 本身已内容寻址 + 增量 delta 存储,所以 seam 开销 = 1 行)。
// kind ∈ {"pre-turn","post-turn","pre-revert","post-revert"};
// turn_seq = seal 时刻的 cs.turns(pre-turn 在 ++ 前;post-turn 在 ++ 后);
// history_len = 目标 seam 的 len(cs.history),用于回滚时截断;
// prev_turn_seq / prev_history_len 仅对 pre-revert(undo) seam 有意义:
// 它们记录执行回滚前的 cs.turns / len(cs.history)。history_snapshot 只在
// pre-revert seam 上保存同一边界的 durable session snapshot(JSON BLOB),使再次
// 回滚 undo seam 能恢复被第一次截断删除的消息,而不只是恢复整数计数(D2)。
// 其他 seam 的 history_snapshot 保持空 blob。
// session_id 让 seam 在多连接共享 repo 时按 session 分组(必修项 J)。
// seq 是单调自增主键,是 seam 列表的 SOLE 排序键(必修项 H)——
// created_at 仅作展示(秒级精度不足以稳定排序同秒插入)。
CREATE TABLE IF NOT EXISTS vcs_seams (
    seq          INTEGER PRIMARY KEY AUTOINCREMENT,
    id           TEXT NOT NULL UNIQUE,
    repo_id      TEXT NOT NULL,
    session_id   TEXT NOT NULL DEFAULT '',
    commit_id    TEXT NOT NULL,
    turn_seq         INTEGER NOT NULL DEFAULT 0,
    history_len      INTEGER NOT NULL DEFAULT 0,
    prev_turn_seq    INTEGER NOT NULL DEFAULT 0,
    prev_history_len INTEGER NOT NULL DEFAULT 0,
    history_snapshot BLOB NOT NULL DEFAULT X'',
    kind             TEXT NOT NULL,
    label        TEXT NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL,
    FOREIGN KEY (repo_id) REFERENCES vcs_repos(id),
    FOREIGN KEY (commit_id) REFERENCES vcs_commits(id)
);
CREATE INDEX IF NOT EXISTS idx_vcs_seams_repo_seq ON vcs_seams(repo_id, seq DESC);
CREATE INDEX IF NOT EXISTS idx_vcs_seams_session ON vcs_seams(repo_id, session_id, seq DESC);
```

3b. 在 `internal/store/session.go` 的现有 import block 加 `"encoding/json"` 与 `"fmt"`(若 `fmt` 已存在则只补 `encoding/json`)，并在 `Message` / `SessionSummary` 类型之后添加。该 codec 不访问数据库，因此可以先于 Task 8 的 transactional APIs 独立落地:

```go
// SessionRevertSnapshot is the exact durable session state before a rollback
// transition. Besides in-process compensation, it is JSON-encoded into the
// returned pre-revert seam so undo remains possible after reconnect (D2).
type SessionRevertSnapshot struct {
	Meta     SessionSummary
	Messages []Message
}

// EncodeSessionRevertSnapshot serializes the exact durable state saved on an
// undo seam. An empty session id is never a valid destructive undo payload.
func EncodeSessionRevertSnapshot(snap SessionRevertSnapshot) ([]byte, error) {
	if snap.Meta.ID == "" {
		return nil, fmt.Errorf("store: empty session revert snapshot")
	}
	blob, err := json.Marshal(snap)
	if err != nil {
		return nil, fmt.Errorf("store: encode session revert snapshot: %w", err)
	}
	return blob, nil
}

// DecodeSessionRevertSnapshot decodes a durable undo payload fail-closed.
func DecodeSessionRevertSnapshot(blob []byte) (SessionRevertSnapshot, error) {
	if len(blob) == 0 {
		return SessionRevertSnapshot{}, fmt.Errorf("store: seam has no session revert snapshot")
	}
	var snap SessionRevertSnapshot
	if err := json.Unmarshal(blob, &snap); err != nil {
		return SessionRevertSnapshot{}, fmt.Errorf("store: decode session revert snapshot: %w", err)
	}
	if snap.Meta.ID == "" {
		return SessionRevertSnapshot{}, fmt.Errorf(
			"store: decoded session revert snapshot has empty session id")
	}
	return snap, nil
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/store/ -v`
Expected: PASS(含 `TestVCSSeamsSchema_*`、`TestSessionRevertSnapshotCodec_*` 与全部已有测试)

- [ ] **Step 5: 提交**

```bash
git add internal/store/store.go internal/store/session.go internal/store/seam_migration_test.go
git commit -m "feat(store): add seam schema and durable revert snapshot codec"
```

---

## Task 2: VCS — repo 级写串行化(`sync.Mutex` + race 测试)

**Files:**
- Modify: `internal/vcs/vcs.go`(`VCS` struct + 公共写方法)
- Test: `internal/vcs/seam_race_test.go`(新建)

> 必修项 B。当前 `VCS` 只有 `treeCacheMu sync.RWMutex`(只保护 tree cache),没有 repo 级写锁。同一个 `vcsInstance` 被 WS/SSE/orchestrator/task broker/子代理多源共享,`SetMaxOpenConns(1)` 只串行化 DB 调用,不能保护"DB tx + 文件系统写"的复合操作(例如 `RevertToSeam` 在 `MaterializeMain` 写盘后、`UPDATE vcs_repos` 前的窗口)。
>
> 解法:加 `repoLocksMu sync.Mutex` + `repoLocks map[string]*sync.Mutex`,以 repoID 为 key 串行化同 repo 的写入,不同 repo 可并发。所有**公共**写路径都必须覆盖:`InitRepo` / `AddWorktree` / `RemoveWorktree` / `RecordEditMain` / `RecordEditWorktree` / `CommitMain` / `CommitWorktree` / `MergeToMain` / `Restore` / `SealMainTurnSeam` / `RevertToSeam` / `MaterializeMain` / `ResetMainHead`。内部嵌套调用使用 `*Locked` 私有版本(已持对应 repo 锁、不再加锁),避免 `RevertToSeam → materializeMainLocked` / `resetMainHeadLocked` 自锁死。`InitRepo` 尚无 repoID,先把 root canonicalize 成数据库 identity,以 `"init:"+canonicalRoot` 关闭相对路径、junction/symlink 与 Windows 大小写 alias 的双初始化；若 canonical root 已有 repo row,按固定的 init-key → repoID 顺序再取同一 repo lane并在锁内重新查询。没有任何 repoID writer 会反向获取 init-key,因此该顺序无锁环。现有 `v.ignore` 是跨 repo 共享 slice,另用 `ignoreMu` 做极短的 copy-on-read/append 保护；它不替代 repo lane,也不在 DB/FS 操作期间持有。

- [ ] **Step 1: 写失败测试** — `internal/vcs/seam_race_test.go`(新建)

```go
// internal/vcs/seam_race_test.go
package vcs

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/x6nux/yanshi/internal/store"
)

// newSeamRaceRepo builds a VCS + repo + initial commit for race tests.
func newSeamRaceRepo(t *testing.T) (*VCS, string, string) {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	st, err := store.Open(filepath.Join(base, "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	v := New(st, filepath.Join(base, "worktrees"))
	repoID, err := v.InitRepo(root)
	if err != nil {
		t.Fatalf("InitRepo: %v", err)
	}
	seedPath := filepath.Join(root, "counter.txt")
	if err := os.WriteFile(seedPath, []byte("0"), 0o644); err != nil {
		t.Fatalf("seed WriteFile: %v", err)
	}
	if err := v.RecordEditMain(repoID, "test", seedPath, []byte("0")); err != nil {
		t.Fatalf("seed RecordEditMain: %v", err)
	}
	if _, err := v.CommitMain(repoID, "test", "seed"); err != nil {
		t.Fatalf("seed CommitMain: %v", err)
	}
	return v, repoID, root
}

// publicRepoWriters is the lock-coverage contract. Tasks 3/4/5 append their new
// writers before their GREEN run: SealMainTurnSeam, MaterializeMain,
// ResetMainHead, RevertToSeam.
var publicRepoWriters = []string{
	"InitRepo", "AddWorktree", "RemoveWorktree", "Restore",
	"RecordEditMain", "RecordEditWorktree",
	"CommitMain", "CommitWorktree", "MergeToMain",
}

// TestPublicRepoWritersAcquireRepoLane parses production files and requires every
// public writer wrapper to call lockRepo. This deterministically catches an
// omitted lane even when a timing-dependent race does not reproduce.
func TestPublicRepoWritersAcquireRepoLane(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller")
	}
	dir := filepath.Dir(thisFile)
	pkgs, err := parser.ParseDir(token.NewFileSet(), dir, func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("ParseDir: %v", err)
	}
	for _, name := range publicRepoWriters {
		found, locks := false, false
		for _, pkg := range pkgs {
			for _, file := range pkg.Files {
				for _, decl := range file.Decls {
					fn, ok := decl.(*ast.FuncDecl)
					if !ok || fn.Recv == nil || fn.Name.Name != name {
						continue
					}
					found = true
					ast.Inspect(fn.Body, func(n ast.Node) bool {
						call, ok := n.(*ast.CallExpr)
						if !ok {
							return true
						}
						sel, ok := call.Fun.(*ast.SelectorExpr)
						if ok && sel.Sel.Name == "lockRepo" {
							locks = true
						}
						return true
					})
				}
			}
		}
		if !found {
			t.Errorf("public writer %s not found", name)
		} else if !locks {
			t.Errorf("public writer %s does not acquire lockRepo", name)
		}
	}
}

// TestRepoMu_ConcurrentRecordEditMain drives the same public writer from many
// goroutines. Each path is unique, so no goroutine's later CommitMain can consume
// another goroutine's pending set; one final commit verifies all edits survived.
func TestRepoMu_ConcurrentRecordEditMain(t *testing.T) {
	v, repoID, root := newSeamRaceRepo(t)
	const goroutines = 16
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()
			path := filepath.Join(root, fmt.Sprintf("counter-%02d.txt", i))
			content := []byte(fmt.Sprintf("g%d", i))
			if err := v.RecordEditMain(repoID, "race", path, content); err != nil {
				t.Errorf("RecordEditMain[%d]: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	if _, err := v.CommitMain(repoID, "race", "all concurrent edits"); err != nil {
		t.Fatalf("final CommitMain: %v", err)
	}
}

// TestRepoMu_DifferentReposProgressIndependently holds repo A's lane directly
// and proves a public writer for repo B can still complete. A single global
// mutex would time out here (CB1).
func TestRepoMu_DifferentReposProgressIndependently(t *testing.T) {
	v, repoA, rootA := newSeamRaceRepo(t)
	rootB := filepath.Join(filepath.Dir(rootA), "repo-b")
	if err := os.MkdirAll(rootB, 0o755); err != nil {
		t.Fatalf("MkdirAll repo-b: %v", err)
	}
	repoB, err := v.InitRepo(rootB)
	if err != nil {
		t.Fatalf("InitRepo repo-b: %v", err)
	}

	unlockA := v.lockRepo(repoA)
	done := make(chan error, 1)
	go func() {
		done <- v.RecordEditMain(repoB, "race",
			filepath.Join(rootB, "independent.txt"), []byte("b"))
	}()
	select {
	case err := <-done:
		unlockA()
		if err != nil {
			t.Fatalf("repo B writer: %v", err)
		}
	case <-time.After(2 * time.Second):
		unlockA()
		t.Fatal("repo B writer blocked behind unrelated repo A lane")
	}
}

// TestInitRepo_UsesCanonicalRootAsStoredIdentity locks the alias contract:
// canonicalization is not merely a mutex key; the canonical root is also the
// root_path queried and persisted, so aliases cannot create distinct repo rows.
func TestInitRepo_UsesCanonicalRootAsStoredIdentity(t *testing.T) {
	v, repoID, root := newSeamRaceRepo(t)
	alias := filepath.Join(root, ".")
	canonical, err := canonicalRepoRoot(alias)
	if err != nil {
		t.Fatalf("canonicalRepoRoot: %v", err)
	}
	gotID, err := v.InitRepo(alias)
	if err != nil {
		t.Fatalf("InitRepo alias: %v", err)
	}
	if gotID != repoID {
		t.Fatalf("InitRepo(alias) id = %s, want existing %s", gotID, repoID)
	}
	var stored string
	if err := v.store.DB.QueryRow(
		"SELECT root_path FROM vcs_repos WHERE id=?", repoID,
	).Scan(&stored); err != nil {
		t.Fatalf("query root_path: %v", err)
	}
	if stored != canonical {
		t.Fatalf("stored root_path = %q, want canonical %q", stored, canonical)
	}
}

// TestRepoMu_ConcurrentInitDifferentRepos drives separate init lanes while both
// roots contribute .yanshiignore patterns. With -race, this also locks the
// shared ignore slice's copy-on-read/append synchronization.
func TestRepoMu_ConcurrentInitDifferentRepos(t *testing.T) {
	base := t.TempDir()
	st, err := store.Open(filepath.Join(base, "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	v := New(st, filepath.Join(base, "worktrees"))

	roots := []string{filepath.Join(base, "repo-a"), filepath.Join(base, "repo-b")}
	for i, root := range roots {
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatalf("MkdirAll[%d]: %v", i, err)
		}
		if err := os.WriteFile(filepath.Join(root, ".yanshiignore"),
			[]byte(fmt.Sprintf("private-%d\n", i)), 0o644); err != nil {
			t.Fatalf("WriteFile .yanshiignore[%d]: %v", i, err)
		}
	}
	var wg sync.WaitGroup
	errs := make(chan error, len(roots))
	for _, root := range roots {
		root := root
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := v.InitRepo(root)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent InitRepo: %v", err)
		}
	}
}

// TestRestore_TracksMainWrite proves Restore's os.WriteFile and recordEdit are
// one serialized operation: restored bytes become a pending main edit (CB1).
func TestRestore_TracksMainWrite(t *testing.T) {
	v, repoID, root := newSeamRaceRepo(t)
	path := filepath.Join(root, "counter.txt")
	oldHead, err := v.RepoMainHead(repoID)
	if err != nil {
		t.Fatalf("RepoMainHead: %v", err)
	}
	if err := os.WriteFile(path, []byte("new"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := v.RecordEditMain(repoID, "test", path, []byte("new")); err != nil {
		t.Fatalf("RecordEditMain: %v", err)
	}
	if _, err := v.CommitMain(repoID, "test", "new version"); err != nil {
		t.Fatalf("CommitMain: %v", err)
	}

	if err := v.Restore(oldHead, "counter.txt", root); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "0" {
		t.Fatalf("restored bytes = %q, err=%v; want 0", got, err)
	}
	pending := v.Uncommitted("main", repoID)
	if pending["counter.txt"] != hashContent([]byte("0")) {
		t.Fatalf("pending restore hash = %q, want restored blob hash", pending["counter.txt"])
	}
}

// TestRestore_TracksWorktreeWrite proves destDir resolution selects the active
// worktree scope rather than accidentally recording the edit on main.
func TestRestore_TracksWorktreeWrite(t *testing.T) {
	v, repoID, _ := newSeamRaceRepo(t)
	head, err := v.RepoMainHead(repoID)
	if err != nil {
		t.Fatalf("RepoMainHead: %v", err)
	}
	wt, err := v.AddWorktree(repoID, []string{"agent-a"})
	if err != nil {
		t.Fatalf("AddWorktree: %v", err)
	}
	path := filepath.Join(wt.Path, "counter.txt")
	if err := os.WriteFile(path, []byte("dirty"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := v.Restore(head, "counter.txt", wt.Path); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got := v.Uncommitted("worktree", wt.ID)["counter.txt"];
		got != hashContent([]byte("0")) {
		t.Fatalf("worktree pending hash = %q, want restored blob hash", got)
	}
	if _, ok := v.Uncommitted("main", repoID)["counter.txt"]; ok {
		t.Fatal("worktree restore must not create a main pending edit")
	}
}

// Task 3 fills the seam half of this staged race test; Task 5 adds revert.
func TestRepoMu_ConcurrentMixedWrites(t *testing.T) {
	t.Skip("activated with concrete seam writer in Task 3")
}
```

上述 import block 已完整列出 AST lock-coverage test、并发 test 与 Restore tracking test 所需 imports。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/vcs/ -run 'TestPublicRepoWritersAcquireRepoLane|TestInitRepo_UsesCanonicalRootAsStoredIdentity' -v`
Expected: 编译失败 — `lockRepo` / `canonicalRepoRoot` 未定义；即使先只补符号，AST contract 仍会因至少 `InitRepo` 等现有 public writer 未调用 `lockRepo` 而 FAIL。这是 deterministic RED,不依赖 race timing。

Run: `go test -race ./internal/vcs/ -run 'TestRepoMu_(ConcurrentRecordEditMain|DifferentReposProgressIndependently|ConcurrentInitDifferentRepos)' -v`
Expected: 旧实现编译失败(`lockRepo` 未定义)；只补一个全局锁会使 different-repos test 超时；只补 per-repo lane 而不保护共享 ignore slice会由 `-race` 报告并发读写。

Run: `go test ./internal/vcs/ -run 'TestRestore_Tracks(Main|Worktree)Write' -v`
Expected: FAIL — restored bytes写盘成功,但 `vcs_uncommitted` 没有对应 restored hash(旧 `Restore` 只调用裸 `os.WriteFile`)。

- [ ] **Step 3: 修改 `internal/vcs/vcs.go`**

3a. 在 `VCS` struct 加 per-repo lock 索引(放在 `treeCacheMu sync.RWMutex` 后),并加唯一取锁 helper:

```go
type VCS struct {
	store       *store.Store
	ignore      []string
	// ignoreMu protects the shared ignore slice independently of repo lanes.
	// isIgnored copies under RLock, then matches after unlock; loadDotIgnore parses
	// first and appends under Lock, so unrelated repo scans never hold this lock
	// during filesystem or database work.
	ignoreMu    sync.RWMutex
	worktreeDir string
	treeCache   map[string]map[string]string
	treeCacheMu sync.RWMutex

	// repoLocksMu protects ONLY the repoLocks map. Never hold it while waiting
	// on a per-repo mutex: take/create the pointer, release the index mutex, then
	// lock the repo mutex. This serializes DB+FS composites within one repo while
	// allowing independent repositories to progress concurrently (CB1).
	repoLocksMu sync.Mutex
	repoLocks   map[string]*sync.Mutex
}

// lockRepo locks the write lane for key and returns its unlock function.
// repoID is the normal key; InitRepo uses "init:"+canonicalRoot before an id
// exists. Lock entries intentionally live for the VCS lifetime (repo count is
// bounded by the process's opened projects; deleting them would create an ABA
// race with goroutines that already retained the mutex pointer).
func (v *VCS) lockRepo(key string) func() {
	v.repoLocksMu.Lock()
	if v.repoLocks == nil {
		v.repoLocks = make(map[string]*sync.Mutex)
	}
	mu := v.repoLocks[key]
	if mu == nil {
		mu = &sync.Mutex{}
		v.repoLocks[key] = mu
	}
	v.repoLocksMu.Unlock()
	mu.Lock()
	return mu.Unlock
}

// isIgnored snapshots the shared slice under RLock and performs all glob work
// after releasing it, so ignoreMu is never nested with a repo lane for long.
func (v *VCS) isIgnored(rel string) bool {
	v.ignoreMu.RLock()
	patterns := append([]string(nil), v.ignore...)
	v.ignoreMu.RUnlock()

	rel = filepath.ToSlash(rel)
	for _, pat := range patterns {
		if ok, err := guard.MatchGlob(pat, rel); err == nil && ok {
			return true
		}
	}
	for _, seg := range strings.Split(rel, "/") {
		for _, pat := range patterns {
			if !strings.ContainsAny(pat, "*?[") && seg == pat {
				return true
			}
		}
	}
	return false
}

// loadDotIgnore parses without a lock, then appends as one protected mutation.
func (v *VCS) loadDotIgnore(repoRoot string) {
	data, err := os.ReadFile(filepath.Join(repoRoot, ".yanshiignore"))
	if err != nil {
		return
	}
	var additions []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			additions = append(additions, line)
		}
	}
	if len(additions) == 0 {
		return
	}
	v.ignoreMu.Lock()
	v.ignore = append(v.ignore, additions...)
	v.ignoreMu.Unlock()
}
```

这里是现有 `isIgnored` 与 `loadDotIgnore` 的完整 replacement；不得保留未加锁的旧定义。`New` 在对象发布前构造初始 `ignore`，无需取 `ignoreMu`。

3b. 用下面的完整 replacement 改造 `RecordEditMain` / `RecordEditWorktree` / `CommitMain` / `CommitWorktree` / `MergeToMain`。worktree 路径统一执行只读 lookup → 获取该 repo lane → locked re-lookup + `RepoID` 比对，关闭 lookup→lock TOCTOU；`commitScope` 保持私有且不自行加锁：

```go
// RecordEditMain records an edit on the main scope (serialized per repo).
func (v *VCS) RecordEditMain(repoID, agent, absPath string, content []byte) error {
	unlock := v.lockRepo(repoID)
	defer unlock()
	return v.recordEditMainLocked(repoID, agent, absPath, content)
}

func (v *VCS) recordEditMainLocked(repoID, agent, absPath string, content []byte) error {
	r, err := v.getRepo(repoID)
	if err != nil {
		return err
	}
	return v.recordEdit("main", repoID, r.RootPath, absPath, content)
}

func (v *VCS) RecordEditWorktree(wtID, agent, absPath string, content []byte) error {
	wt, err := v.getWorktree(wtID)
	if err != nil {
		return err
	}
	unlock := v.lockRepo(wt.RepoID)
	defer unlock()
	return v.recordEditWorktreeLocked(
		wt.RepoID, wtID, agent, absPath, content)
}

func (v *VCS) recordEditWorktreeLocked(
	repoID, wtID, agent, absPath string, content []byte,
) error {
	wt, err := v.getWorktree(wtID)
	if err != nil {
		return err
	}
	if wt.RepoID != repoID {
		return fmt.Errorf("vcs: worktree %s changed repository", wtID)
	}
	return v.recordEdit("worktree", wtID, wt.Path, absPath, content)
}

// CommitMain folds main's pending changeset into a new commit (serialized).
func (v *VCS) CommitMain(repoID, author, message string) (string, error) {
	unlock := v.lockRepo(repoID)
	defer unlock()
	return v.commitScope("main", repoID, repoID, "", author, message)
}

func (v *VCS) CommitWorktree(wtID, author, message string) (string, error) {
	wt, err := v.getWorktree(wtID)
	if err != nil {
		return "", err
	}
	unlock := v.lockRepo(wt.RepoID)
	defer unlock()
	return v.commitWorktreeLocked(wt.RepoID, wtID, author, message)
}

func (v *VCS) commitWorktreeLocked(
	repoID, wtID, author, message string,
) (string, error) {
	wt, err := v.getWorktree(wtID)
	if err != nil {
		return "", err
	}
	if wt.RepoID != repoID {
		return "", fmt.Errorf("vcs: worktree %s changed repository", wtID)
	}
	return v.commitScope("worktree", wtID, repoID, wtID, author, message)
}

func (v *VCS) MergeToMain(
	wtID, author string, force bool,
) (string, []string, error) {
	wt, err := v.getWorktree(wtID)
	if err != nil {
		return "", nil, err
	}
	unlock := v.lockRepo(wt.RepoID)
	defer unlock()
	return v.mergeToMainLocked(wt.RepoID, wtID, author, force)
}

func (v *VCS) mergeToMainLocked(
	repoID, wtID, author string, force bool,
) (string, []string, error) {
	wt, err := v.getWorktree(wtID)
	if err != nil {
		return "", nil, err
	}
	if wt.RepoID != repoID {
		return "", nil, fmt.Errorf("vcs: worktree %s changed repository", wtID)
	}
	r, err := v.getRepo(repoID)
	if err != nil {
		return "", nil, err
	}
	base := v.commitTree(wt.BaseCommit)
	ours := v.commitTree(r.MainHead)
	theirs := v.commitTree(v.worktreeTip(wtID))

	merged := map[string]string{}
	paths := map[string]struct{}{}
	for _, tree := range []map[string]string{base, ours, theirs} {
		for path := range tree {
			paths[path] = struct{}{}
		}
	}
	var conflicts []string
	for path := range paths {
		baseHash := base[path]
		oursHash := ours[path]
		theirsHash := theirs[path]
		switch {
		case oursHash == theirsHash:
			if oursHash != "" {
				merged[path] = oursHash
			}
		case oursHash == baseHash:
			if theirsHash != "" {
				merged[path] = theirsHash
			}
		case theirsHash == baseHash:
			if oursHash != "" {
				merged[path] = oursHash
			}
		default:
			conflicts = append(conflicts, path)
			if force {
				if theirsHash != "" {
					merged[path] = theirsHash
				}
			} else if oursHash != "" {
				merged[path] = oursHash
			}
		}
	}
	sort.Strings(conflicts)
	if len(conflicts) > 0 && !force {
		return "", conflicts, ErrConflicts
	}

	tx, err := v.store.DB.Begin()
	if err != nil {
		return "", conflicts, err
	}
	defer tx.Rollback()
	cid, err := v.writeCommitInTx(
		tx, r.ID, "", r.MainHead, wtID, author,
		"merge worktree "+wtID, merged,
	)
	if err != nil {
		return "", conflicts, err
	}
	if _, err := tx.Exec(
		"UPDATE vcs_repos SET main_head=? WHERE id=?", cid, r.ID,
	); err != nil {
		return "", conflicts, err
	}
	if err := tx.Commit(); err != nil {
		return "", conflicts, err
	}
	return cid, conflicts, nil
}
```

`agent` 参数继续只承担 public API 对称性；attribution 仍在 commit 时应用。`commitScope` 的现有完整实现不改，但从此只有已持 repo lane 的 `CommitMain`、`commitWorktreeLocked` 调用它。上述五个 public writer 与三个 worktree locked core 是本 Step 的完整最终代码。

3c. **CB1 覆盖剩余公共写路径**：`InitRepo` / `AddWorktree` / `RemoveWorktree` / `Restore` 也必须取 per-repo 锁。下列代码完整给出 canonical identity、固定 lock ordering、全部 public wrapper 与 locked core；删除旧同名定义，不保留第二套未加锁入口：

```go
// canonicalRepoRoot returns the ONLY root identity used for both locking and
// vcs_repos.root_path. Abs closes relative aliases; EvalSymlinks closes
// junction/symlink aliases; Windows folding closes case aliases on its
// case-insensitive filesystem.
func canonicalRepoRoot(rootPath string) (string, error) {
	abs, err := filepath.Abs(rootPath)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("vcs: canonicalize repo root: %w", err)
	}
	real = filepath.Clean(real)
	if runtime.GOOS == "windows" {
		real = strings.ToLower(real)
	}
	return real, nil
}

func initRepoLockKey(canonicalRoot string) string {
	return "init:" + canonicalRoot
}

// InitRepo first serializes discovery/creation by canonical root. For an
// existing row it then acquires that repo's normal lane in the only permitted
// order (init-key -> repoID), re-queries while locked, and refreshes ignore
// rules. No repoID writer ever acquires an init-key, so no reverse edge exists.
func (v *VCS) InitRepo(rootPath string) (string, error) {
	canonicalRoot, err := canonicalRepoRoot(rootPath)
	if err != nil {
		return "", err
	}
	unlockInit := v.lockRepo(initRepoLockKey(canonicalRoot))
	defer unlockInit()

	var existingID string
	err = v.store.DB.QueryRow(
		"SELECT id FROM vcs_repos WHERE root_path = ?", canonicalRoot,
	).Scan(&existingID)
	switch {
	case err == nil:
		unlockRepo := v.lockRepo(existingID)
		defer unlockRepo()
		var lockedID string
		if err := v.store.DB.QueryRow(
			"SELECT id FROM vcs_repos WHERE root_path = ?", canonicalRoot,
		).Scan(&lockedID); err != nil {
			return "", err
		}
		if lockedID != existingID {
			return "", fmt.Errorf(
				"vcs: repo identity changed for root %s", canonicalRoot)
		}
		v.loadDotIgnore(canonicalRoot)
		return lockedID, nil
	case !errors.Is(err, sql.ErrNoRows):
		return "", err
	default:
		return v.initNewRepoLocked(canonicalRoot)
	}
}

// initNewRepoLocked creates a previously absent canonical root while its
// init-key lane is held. The row is inserted last, so no repoID writer can
// discover the repo until its initial commit exists.
func (v *VCS) initNewRepoLocked(canonicalRoot string) (string, error) {
	v.loadDotIgnore(canonicalRoot)
	id := newVCSID()
	tree := map[string]string{}
	_ = filepath.WalkDir(canonicalRoot, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		rel, err := filepath.Rel(canonicalRoot, p)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			if v.isIgnored(rel) {
				return filepath.SkipDir
			}
			return nil
		}
		if v.isIgnored(rel) {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		tree[rel] = v.putBlob(data)
		return nil
	})
	commitID, err := v.writeCommit(
		id, "", "", "", "orchestrator", "vcs init", tree,
	)
	if err != nil {
		return "", err
	}
	if _, err := v.store.DB.Exec(
		"INSERT INTO vcs_repos (id, root_path, main_head, created_at) VALUES (?, ?, ?, ?)",
		id, canonicalRoot, commitID, time.Now().Unix(),
	); err != nil {
		return "", err
	}
	return id, nil
}

// AddWorktree creates a worktree branch + row (serialized on the repo's lane).
func (v *VCS) AddWorktree(repoID string, agents []string) (Worktree, error) {
	unlock := v.lockRepo(repoID)
	defer unlock()
	return v.addWorktreeLocked(repoID, agents)
}

func (v *VCS) addWorktreeLocked(repoID string, agents []string) (Worktree, error) {
	r, err := v.getRepo(repoID)
	if err != nil {
		return Worktree{}, err
	}
	id := newVCSID()
	wtPath := filepath.Join(v.worktreeDir, id)
	if err := os.MkdirAll(wtPath, 0o755); err != nil {
		return Worktree{}, err
	}
	for path, h := range v.commitTree(r.MainHead) {
		content, err := v.getBlob(h)
		if err != nil {
			continue
		}
		dest := filepath.Join(wtPath, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return Worktree{}, err
		}
		if err := os.WriteFile(dest, content, 0o644); err != nil {
			return Worktree{}, err
		}
	}
	wt := Worktree{
		ID: id, RepoID: repoID, Path: wtPath,
		BaseCommit: r.MainHead, CreatedAt: time.Now().Unix(),
	}
	if _, err := v.store.DB.Exec(
		"INSERT INTO vcs_worktrees (id, repo_id, path, base_commit, created_at, active) VALUES (?, ?, ?, ?, ?, 1)",
		wt.ID, wt.RepoID, wt.Path, wt.BaseCommit, wt.CreatedAt,
	); err != nil {
		return Worktree{}, err
	}
	_ = agents
	return wt, nil
}

// RemoveWorktree preserves the existing idempotent missing-row behavior. For
// an existing row, lookup selects the lane and the locked core re-reads it to
// close lookup->lock TOCTOU before filesystem/DB mutation.
func (v *VCS) RemoveWorktree(wtID string) error {
	wt, err := v.getWorktree(wtID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	unlock := v.lockRepo(wt.RepoID)
	defer unlock()
	return v.removeWorktreeLocked(wt.RepoID, wtID)
}

func (v *VCS) removeWorktreeLocked(repoID, wtID string) error {
	wt, err := v.getWorktree(wtID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if wt.RepoID != repoID {
		return fmt.Errorf("vcs: worktree %s changed repository", wtID)
	}
	if wt.Path != "" && v.worktreeDir != "" {
		rel, relErr := filepath.Rel(v.worktreeDir, wt.Path)
		if relErr == nil && rel != "." &&
			!strings.HasPrefix(filepath.ToSlash(rel), "..") {
			_ = os.RemoveAll(wt.Path)
		}
	}
	_, err = v.store.DB.Exec(
		"UPDATE vcs_worktrees SET active=0 WHERE id=?", wtID,
	)
	return err
}

// Restore writes the historical blob into an ACTIVE working copy and records
// the same bytes in that scope's pending changeset before releasing the repo
// lane (CB1). The public signature is unchanged; commit ownership identifies
// repoID and destDir must exactly match that repo's root or an active worktree.
func (v *VCS) Restore(commitID, path, destDir string) error {
	repoID, err := v.commitRepoID(commitID)
	if err != nil {
		return err
	}
	unlock := v.lockRepo(repoID)
	defer unlock()
	return v.restoreLocked(repoID, commitID, path, destDir)
}

func (v *VCS) commitRepoID(commitID string) (string, error) {
	var repoID string
	if err := v.store.DB.QueryRow(
		"SELECT repo_id FROM vcs_commits WHERE id=?", commitID,
	).Scan(&repoID); err != nil {
		return "", fmt.Errorf("vcs: resolve commit %s: %w", commitID, err)
	}
	return repoID, nil
}

func (v *VCS) restoreLocked(repoID, commitID, path, destDir string) error {
	// Re-read ownership while holding the repo lane: closes lookup→lock TOCTOU.
	lockedRepoID, err := v.commitRepoID(commitID)
	if err != nil {
		return err
	}
	if lockedRepoID != repoID {
		return fmt.Errorf("vcs: commit %s changed repository", commitID)
	}
	scopeType, scopeID, scopeRoot, err := v.restoreScopeLocked(repoID, destDir)
	if err != nil {
		return err
	}

	cleanRel := filepath.Clean(filepath.FromSlash(path))
	relSlash := filepath.ToSlash(cleanRel)
	if cleanRel == "." || filepath.IsAbs(cleanRel) || relSlash == ".." ||
		strings.HasPrefix(relSlash, "../") {
		return fmt.Errorf("vcs: unsafe restore path %q", path)
	}
	tree := v.commitTree(commitID)
	h, ok := tree[relSlash]
	if !ok {
		return fmt.Errorf("vcs: %s not in commit %s", relSlash, commitID)
	}
	content, err := v.getBlob(h)
	if err != nil {
		return fmt.Errorf("vcs: read restore blob %s: %w", h, err)
	}
	dest := filepath.Join(scopeRoot, cleanRel)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}

	// Snapshot the destination so a recordEdit failure cannot leave an untracked
	// filesystem mutation. Blob insertion on a failed upsert is harmless dedup data.
	old, readErr := os.ReadFile(dest)
	existed := readErr == nil
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}
	if err := os.WriteFile(dest, content, 0o644); err != nil {
		return err
	}
	if err := v.recordEdit(scopeType, scopeID, scopeRoot, dest, content); err != nil {
		var rollbackErr error
		if existed {
			rollbackErr = os.WriteFile(dest, old, 0o644)
		} else {
			rollbackErr = os.Remove(dest)
			if errors.Is(rollbackErr, os.ErrNotExist) {
				rollbackErr = nil
			}
		}
		return errors.Join(fmt.Errorf("vcs: track restored file: %w", err), rollbackErr)
	}
	return nil
}

func (v *VCS) restoreScopeLocked(repoID, destDir string) (string, string, string, error) {
	destAbs, err := filepath.Abs(destDir)
	if err != nil {
		return "", "", "", err
	}
	destAbs = filepath.Clean(destAbs)
	r, err := v.getRepo(repoID)
	if err != nil {
		return "", "", "", err
	}
	rootAbs, err := filepath.Abs(r.RootPath)
	if err != nil {
		return "", "", "", err
	}
	rootAbs = filepath.Clean(rootAbs)
	if destAbs == rootAbs {
		return "main", repoID, rootAbs, nil
	}
	rows, err := v.store.DB.Query(
		"SELECT id, path FROM vcs_worktrees WHERE repo_id=? AND active=1", repoID)
	if err != nil {
		return "", "", "", err
	}
	defer rows.Close()
	for rows.Next() {
		var wtID, wtPath string
		if err := rows.Scan(&wtID, &wtPath); err != nil {
			return "", "", "", err
		}
		wtAbs, err := filepath.Abs(wtPath)
		if err == nil && destAbs == filepath.Clean(wtAbs) {
			return "worktree", wtID, destAbs, nil
		}
	}
	if err := rows.Err(); err != nil {
		return "", "", "", err
	}
	return "", "", "", fmt.Errorf(
		"vcs: restore destination %s is not an active working copy for repo %s",
		destAbs, repoID)
}
```

> 在 `internal/vcs/vcs.go` 现有 import block 加 `"runtime"`(供 Windows canonical DB/root identity);其他依赖 `filepath`/`strings`/`sync`/`guard` 已存在。上面给出的 `InitRepo` / `initNewRepoLocked`、`AddWorktree` / `addWorktreeLocked`、`RemoveWorktree` / `removeWorktreeLocked`、`isIgnored` / `loadDotIgnore` 均为完整 replacement；删除同名旧定义。`Restore` 使用上面的完整 replacement,不能保留原来的裸 `os.WriteFile`。public 签名保持为 `AddWorktree(repoID string, agents []string) (Worktree, error)`、`RemoveWorktree(wtID string) error`、`Restore(commitID, path, destDir string) error`。
>
> `MaterializeMain` 与 `ResetMainHead`(Task 4)也必须提供 per-repo locked **公共外壳**;`RevertToSeam` 已持锁时只调用 `materializeMainLocked` / `resetMainHeadLocked`,避免自锁。`SealMainTurnSeam`(Task 3)/`RevertToSeam`(Task 5)各自取锁,其内部 `*Locked` core 不重复取锁。

- [ ] **Step 4: 跑 race 测试**

Run: `go test -race ./internal/vcs/ -run 'TestPublicRepoWritersAcquireRepoLane|TestInitRepo_UsesCanonicalRootAsStoredIdentity|TestRepoMu_(ConcurrentRecordEditMain|DifferentReposProgressIndependently|ConcurrentInitDifferentRepos)|TestRestore_Tracks(Main|Worktree)Write' -v`
Expected: PASS；AST contract 覆盖全部当前 public writer；canonical root 同时作为 init-key 与 `root_path` identity；同 repo 写序列化而不同 repo writer 可在另一 lane 被持有时完成；共享 ignore slice 无 data race；Restore 写盘与 pending tracking 同处 repo lane且 main/worktree scope 均正确。

- [ ] **Step 5: 提交**

```bash
git add internal/vcs/vcs.go internal/vcs/seam_race_test.go
git commit -m "feat(vcs): add per-repo write locks to serialize all write paths (B2-RB1 B)"
```

---
## Task 3: VCS — Seam / SeamKind / SealMainTurnSeam / FindSeam / ListSeams / RepoMainHead

**Files:**
- Create: `internal/vcs/seam.go`
- Modify: `internal/vcs/seam_race_test.go`（在 Task 2 的 writer contract 追加 `SealMainTurnSeam`）
- Test: `internal/vcs/seam_test.go`(新建)

> 必修项 A/J/H。seam 是 commit 引用 + 元数据,不是完整快照。`SealMainTurnSeam` 在 per-repo 锁内完成"flush pending → read head → insert seam row"三步原子序列(对调用方而言);`FindSeam`/`ListSeams` 是只读 DB 查询,不需要 per-repo 锁;`RepoMainHead` 是 `getRepo` 的导出版,给 WS/SSE handler 用。

- [ ] **Step 1: 写失败测试** — `internal/vcs/seam_test.go`(新建)

```go
// internal/vcs/seam_test.go
package vcs

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/x6nux/yanshi/internal/store"
)

// setupSeamTestRepo mirrors newSeamRaceRepo but lives here so both files compile
// independently. Returns a VCS with a seeded repo + initial commit.
func setupSeamTestRepo(t *testing.T) (*VCS, string, string) {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	st, err := store.Open(filepath.Join(base, "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	v := New(st, filepath.Join(base, "worktrees"))
	repoID, err := v.InitRepo(root)
	if err != nil {
		t.Fatalf("InitRepo: %v", err)
	}
	seedPath := filepath.Join(root, "a.txt")
	if err := os.WriteFile(seedPath, []byte("v0"), 0o644); err != nil {
		t.Fatalf("seed WriteFile: %v", err)
	}
	if err := v.RecordEditMain(repoID, "test", seedPath, []byte("v0")); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := v.CommitMain(repoID, "test", "seed"); err != nil {
		t.Fatalf("seed commit: %v", err)
	}
	return v, repoID, root
}

// mustMainHead reads the repo's current main_head; helper for tests.
func mustMainHead(t *testing.T, v *VCS, repoID string) string {
	t.Helper()
	h, err := v.RepoMainHead(repoID)
	if err != nil {
		t.Fatalf("RepoMainHead: %v", err)
	}
	return h
}

func TestSealMainTurnSeam_RecordsSeamAtCurrentHead(t *testing.T) {
	v, repoID, root := setupSeamTestRepo(t)
	head0 := mustMainHead(t, v, repoID)

	// Pending edit must be folded into a new commit, then sealed.
	if err := v.RecordEditMain(repoID, "u", filepath.Join(root, "a.txt"), []byte("v1")); err != nil {
		t.Fatalf("RecordEditMain: %v", err)
	}
	seamID, err := v.SealMainTurnSeam(repoID, "s1", 1, 2, SeamPostTurn, "post-turn:1")
	if err != nil {
		t.Fatalf("SealMainTurnSeam: %v", err)
	}
	if seamID == "" {
		t.Fatal("seamID 空串")
	}
	head1 := mustMainHead(t, v, repoID)
	if head1 == head0 {
		t.Fatal("pending edit 未被 SealMainTurnSeam fold 成新 commit")
	}
	seam, err := v.FindSeam(seamID)
	if err != nil {
		t.Fatalf("FindSeam: %v", err)
	}
	if seam.CommitID != head1 {
		t.Errorf("seam.CommitID = %s, want %s", seam.CommitID, head1)
	}
	if seam.SessionID != "s1" {
		t.Errorf("seam.SessionID = %q, want %q", seam.SessionID, "s1")
	}
	if seam.TurnSeq != 1 || seam.HistoryLen != 2 {
		t.Errorf("seam.TurnSeq=%d HistoryLen=%d, want 1/2", seam.TurnSeq, seam.HistoryLen)
	}
	if seam.Kind != SeamPostTurn {
		t.Errorf("seam.Kind = %q, want %q", seam.Kind, SeamPostTurn)
	}
}

func TestSealMainTurnSeam_NoPendingUsesCurrentHead(t *testing.T) {
	v, repoID, _ := setupSeamTestRepo(t)
	head := mustMainHead(t, v, repoID)
	seamID, err := v.SealMainTurnSeam(repoID, "s1", 1, 2, SeamPreTurn, "pre-turn:1")
	if err != nil {
		t.Fatalf("SealMainTurnSeam: %v", err)
	}
	seam, _ := v.FindSeam(seamID)
	if seam.CommitID != head {
		t.Errorf("no-pending seam.CommitID = %s, want %s", seam.CommitID, head)
	}
}

// TestListSeams_OrderedBySeqDesc inserts 3 seams in the same second and asserts
// they come back latest-first. 必修项 H: seq must be the SOLE sort key.
func TestListSeams_OrderedBySeqDesc(t *testing.T) {
	v, repoID, _ := setupSeamTestRepo(t)
	ids := []string{}
	for i := 0; i < 3; i++ {
		id, err := v.SealMainTurnSeam(repoID, "s1", i, i, SeamPreTurn, "p"+strconv.Itoa(i))
		if err != nil {
			t.Fatalf("SealMainTurnSeam[%d]: %v", i, err)
		}
		ids = append(ids, id)
	}
	seams, err := v.ListSeams(repoID, "s1", 0)
	if err != nil {
		t.Fatalf("ListSeams: %v", err)
	}
	if len(seams) != 3 {
		t.Fatalf("ListSeams returned %d seams, want 3", len(seams))
	}
	// Latest-first: ids[2], ids[1], ids[0]
	for i, wantReversed := range []string{ids[2], ids[1], ids[0]} {
		if seams[i].ID != wantReversed {
			t.Errorf("seams[%d].ID = %s, want %s", i, seams[i].ID, wantReversed)
		}
	}
}

// TestListSeams_FiltersBySession — 必修项 J: seams from other sessions must
// NOT leak.
func TestListSeams_FiltersBySession(t *testing.T) {
	v, repoID, _ := setupSeamTestRepo(t)
	_, _ = v.SealMainTurnSeam(repoID, "session-A", 1, 1, SeamPreTurn, "a1")
	_, _ = v.SealMainTurnSeam(repoID, "session-B", 1, 1, SeamPreTurn, "b1")
	_, _ = v.SealMainTurnSeam(repoID, "session-A", 2, 2, SeamPreTurn, "a2")
	a, _ := v.ListSeams(repoID, "session-A", 0)
	if len(a) != 2 {
		t.Errorf("session-A: got %d seams, want 2", len(a))
	}
	b, _ := v.ListSeams(repoID, "session-B", 0)
	if len(b) != 1 {
		t.Errorf("session-B: got %d seams, want 1", len(b))
	}
}

func TestRepoMainHead_Exported(t *testing.T) {
	v, repoID, _ := setupSeamTestRepo(t)
	head, err := v.RepoMainHead(repoID)
	if err != nil {
		t.Fatalf("RepoMainHead: %v", err)
	}
	if head == "" {
		t.Fatal("RepoMainHead 返回空 head")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/vcs/ -run 'TestSealMainTurnSeam|TestListSeams|TestRepoMainHead' -v`
Expected: 编译失败(`SealMainTurnSeam`/`FindSeam`/`ListSeams`/`RepoMainHead`/`Seam`/`SeamPreTurn`/`SeamPostTurn` 未定义)

- [ ] **Step 3: 创建 `internal/vcs/seam.go`**

```go
// internal/vcs/seam.go
//
// seam 是"逐轮回滚 UI"(B2-RB1)的核心抽象:一条命名指针指向 vcs_commits.id,
// 加上 turn_seq / history_len / kind / label 元数据。seam 行开销 = 1 行 SQLite,
// commit 本身已内容寻址 + 增量 delta 存储(writeCommitInTx),所以快照成本极低。
//
// 生命周期:
//   pre-turn   — user_message 进入主循环、user msg 已 append 到 cs.history、
//                模型尚未运行时打。CommitID = 当前 main_head(若有 pending
//                则先 fold 成新 commit)。HistoryLen = len(cs.history)(含 user msg)。
//   post-turn  — turn 结束(无论正常 / model error / tool error / cancel / disconnect)
//                时打。CommitID = turn 后 main_head。HistoryLen = len(cs.history)。
//   pre-revert — RevertToSeam 执行前打(UNDO seam),CommitID = previousHead。
//                返回给调用方用于"撤销本次回滚"。
//   post-revert — RevertToSeam 执行后打(AUDIT seam),CommitID = targetCommit。
//                仅作审计,不用于再次回滚。
//
// 排序:vcs_seams.seq 是单调自增主键(必修项 H),是 ListSeams 的 SOLE 排序键。
// created_at 仅作展示(秒级精度不足以稳定排序同秒插入)。
//
// 并发:SealMainTurnSeam 持有 per-repo 锁(必修项 B),串行化"flush pending → read
// head → insert seam"复合操作;FindSeam / ListSeams 是只读 DB 查询,不加 per-repo 锁。

package vcs

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// SeamKind 标识 seam 在 turn / revert 生命周期中的位置。
type SeamKind string

const (
	SeamPreTurn    SeamKind = "pre-turn"
	SeamPostTurn   SeamKind = "post-turn"
	SeamPreRevert  SeamKind = "pre-revert"  // UNDO seam: 指向 previousHead
	SeamPostRevert SeamKind = "post-revert" // AUDIT seam: 指向 target commit
)

// Seam 是 vcs_seams 一行的轻量视图。
type Seam struct {
	ID             string
	RepoID         string
	SessionID      string
	CommitID       string
	TurnSeq        int
	HistoryLen     int
	// PrevTurnSeq / PrevHistoryLen are set only on SeamPreRevert (undo)
	// seams. They capture the conversation boundary BEFORE the revert, so
	// restoring the undo seam can restore the longer pre-revert history (D2).
	PrevTurnSeq    int
	PrevHistoryLen int
	// HistorySnapshot is an opaque JSON-encoded store.SessionRevertSnapshot.
	// It is non-empty only on SeamPreRevert and is never sent over proto.
	HistorySnapshot []byte
	Kind            SeamKind
	Label          string
	Seq            int64 // monotonic insertion order
	CreatedAt      int64
}

// RepoMainHead returns the current main_head commit id of repoID, or "" + error
// when the repo cannot be loaded. Exported mirror of getRepo.MainHead for
// callers (ws.go handler, sse chat handler) that need to bind confirmations
// to the exact head at request time (必修项 E: target binding).
func (v *VCS) RepoMainHead(repoID string) (string, error) {
	r, err := v.getRepo(repoID)
	if err != nil {
		return "", err
	}
	return r.MainHead, nil
}

// SealMainTurnSeam atomically (under the per-repo lock): if main has pending edits, folds
// them into a new commit; then reads the resulting main_head; then inserts a
// seam row referencing that head with the given kind/label. Returns the new
// seam id.
//
// sessionID scopes the seam to a logical chat session (WS connection); turnSeq
// is cs.turns at seal time (pre-turn: before increment; post-turn: after);
// historyLen is len(cs.history) at seal time (revert uses it to truncate).
//
// Empty repoID is a silent no-op (returns "", nil) — the caller
// (Server.sealTurnBoundary) uses this to skip when VCS is unconfigured.
func (v *VCS) SealMainTurnSeam(repoID, sessionID string, turnSeq, historyLen int, kind SeamKind, label string) (string, error) {
	if repoID == "" {
		return "", nil
	}
	unlock := v.lockRepo(repoID)
	defer unlock()
	return v.sealMainTurnSeamLocked(repoID, sessionID, turnSeq, historyLen, kind, label)
}

// sealMainTurnSeamLocked is the repo-lock-held core. Callers that ALREADY hold
// the repo lock call this to avoid re-locking. The mutex protects: (a) pending-changeset
// query, (b) commitScope's tx, (c) main_head read, (d) seam INSERT — a concurrent
// CommitMain between any of these would produce a seam referencing a stale head.
func (v *VCS) sealMainTurnSeamLocked(repoID, sessionID string, turnSeq, historyLen int, kind SeamKind, label string) (string, error) {
	r, err := v.getRepo(repoID)
	if err != nil {
		return "", err
	}
	head := r.MainHead

	// Fold pending edits so the seam references a STABLE head. ErrNoChanges is
	// benign — nothing to fold. commitScope is the same path CommitMain takes;
	// safe to call here because we hold the repo lock and commitScope does NOT re-lock.
	if len(v.Uncommitted("main", repoID)) > 0 {
		newHead, cErr := v.commitScope("main", repoID, repoID, "", "orchestrator", label)
		if cErr == nil {
			head = newHead
		} else if !errors.Is(cErr, ErrNoChanges) {
			return "", fmt.Errorf("vcs: seal %s: commit: %w", kind, cErr)
		}
	}

	seamID := newVCSID()
	now := time.Now().Unix()
	if _, err := v.store.DB.Exec(
		`INSERT INTO vcs_seams (id, repo_id, session_id, commit_id, turn_seq, history_len, prev_turn_seq, prev_history_len, history_snapshot, kind, label, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		seamID, repoID, sessionID, head, turnSeq, historyLen, 0, 0, []byte{}, string(kind), label, now,
	); err != nil {
		return "", fmt.Errorf("vcs: seal %s: insert: %w", kind, err)
	}
	return seamID, nil
}

// FindSeam loads a single seam by id. Returns (Seam{}, sql.ErrNoRows) when absent.
// Read-only — does NOT take the per-repo lock (DB reads are serialized by SetMaxOpenConns(1)).
func (v *VCS) FindSeam(seamID string) (Seam, error) {
	var s Seam
	var kind string
	err := v.store.DB.QueryRow(
		`SELECT id, repo_id, session_id, commit_id, turn_seq, history_len,
		        prev_turn_seq, prev_history_len, history_snapshot, kind, label, seq, created_at
		 FROM vcs_seams WHERE id = ?`,
		seamID,
	).Scan(&s.ID, &s.RepoID, &s.SessionID, &s.CommitID, &s.TurnSeq, &s.HistoryLen,
		&s.PrevTurnSeq, &s.PrevHistoryLen, &s.HistorySnapshot, &kind, &s.Label, &s.Seq, &s.CreatedAt)
	if err != nil {
		return Seam{}, err
	}
	s.Kind = SeamKind(kind)
	return s, nil
}

// ListSeams returns seams for repoID newest-first (ORDER BY seq DESC). When
// sessionID != "" the result is filtered to that session (必修项 J). limit <= 0
// means a default cap of 50. Read-only — does NOT take the per-repo lock.
func (v *VCS) ListSeams(repoID, sessionID string, limit int) ([]Seam, error) {
	if limit <= 0 {
		limit = 50
	}
	var (
		rows *sql.Rows
		err  error
	)
	if sessionID != "" {
		rows, err = v.store.DB.Query(
			`SELECT id, repo_id, session_id, commit_id, turn_seq, history_len,
			        prev_turn_seq, prev_history_len, history_snapshot, kind, label, seq, created_at
			 FROM vcs_seams WHERE repo_id = ? AND session_id = ?
			 ORDER BY seq DESC LIMIT ?`,
			repoID, sessionID, limit,
		)
	} else {
		rows, err = v.store.DB.Query(
			`SELECT id, repo_id, session_id, commit_id, turn_seq, history_len,
			        prev_turn_seq, prev_history_len, history_snapshot, kind, label, seq, created_at
			 FROM vcs_seams WHERE repo_id = ?
			 ORDER BY seq DESC LIMIT ?`,
			repoID, limit,
		)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Seam
	for rows.Next() {
		var s Seam
		var kind string
		if err := rows.Scan(&s.ID, &s.RepoID, &s.SessionID, &s.CommitID, &s.TurnSeq, &s.HistoryLen,
			&s.PrevTurnSeq, &s.PrevHistoryLen, &s.HistorySnapshot, &kind, &s.Label, &s.Seq, &s.CreatedAt); err != nil {
			return nil, err
		}
		s.Kind = SeamKind(kind)
		out = append(out, s)
	}
	return out, rows.Err()
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/vcs/ -run 'TestSealMainTurnSeam|TestListSeams|TestRepoMainHead' -v`
Expected: PASS

- [ ] **Step 5: 扩展 public-writer lock contract 并跑 race 测试**

先在 `internal/vcs/seam_race_test.go` 的 `publicRepoWriters` 末尾追加 `"SealMainTurnSeam"`。这使 Task 2 的 AST contract 同步覆盖本 Task 新增的 public writer,而不是只靠 timing race。

把 staged `TestRepoMu_ConcurrentMixedWrites` 完整替换为:

```go
func TestRepoMu_ConcurrentMixedWrites(t *testing.T) {
	v, repoID, root := newSeamRaceRepo(t)
	preID, err := v.SealMainTurnSeam(repoID, "race-session", 0, 0, SeamPreTurn, "pre-race")
	if err != nil {
		t.Fatalf("SealMainTurnSeam: %v", err)
	}
	const goroutines = 8
	var wg sync.WaitGroup
	errs := make(chan error, goroutines*2)
	wg.Add(goroutines * 2)
	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()
			path := filepath.Join(root, fmt.Sprintf("mixed-%02d.txt", i))
			if err := v.RecordEditMain(repoID, "race", path,
				[]byte(fmt.Sprintf("m%d", i))); err != nil {
				errs <- fmt.Errorf("RecordEditMain[%d]: %w", i, err)
			}
		}(i)
		go func(i int) {
			defer wg.Done()
			if _, err := v.SealMainTurnSeam(repoID, "race-session", i+1, i+1,
				SeamPostTurn, fmt.Sprintf("post-%d", i)); err != nil {
				errs <- fmt.Errorf("SealMainTurnSeam[%d]: %w", i, err)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	// Task 5 replaces this staged use with a RevertToSeam call.
	_ = preID
}
```

Run: `go test -race ./internal/vcs/ -run TestRepoMu -v`
Expected: PASS

- [ ] **Step 6: 提交**

```bash
git add internal/vcs/seam.go internal/vcs/seam_test.go internal/vcs/seam_race_test.go
git commit -m "feat(vcs): add Seam + SealMainTurnSeam/FindSeam/ListSeams/RepoMainHead (B2-RB1)"
```

---
## Task 4: VCS — MaterializeMain + ResetMainHead(strict,fail-fast,path-safety)

**Files:**
- Create: `internal/vcs/revert.go`(本任务只加 `MaterializeMain` + `ResetMainHead` + `validateRelPath`,Task 5 加 `RevertToSeam`)
- Modify: `internal/vcs/seam_race_test.go`（在 writer contract 追加 `MaterializeMain` / `ResetMainHead`）
- Test: `internal/vcs/revert_test.go`(新建)

> 必修项 G + D1。Task 2 已把单文件 `Restore` 纳入 tracked repo lane;本任务处理整树回滚。新增 `MaterializeMain`:
>
> - public `MaterializeMain` / `ResetMainHead` 都按 repoID 取锁(CB1);`RevertToSeam` 已持锁时调用 private `materializeMainLocked` / tx 内 SQL,不递归取锁。
> - 校验 target commit 属于 repoID 且 worktree_id=''(main commit)。
> - phase 1:校验所有 blob 与 path;再快照 `prevTree ∪ targetTree` 涉及的全部 tracked 文件(存在性、bytes、mode),任何校验/快照失败都不动盘。
> - phase 2:删除 prevTree 有、targetTree 无的文件;phase 3:写 targetTree。每次 mutation 失败都调用 snapshot rollback,把**所有已触碰文件**恢复到操作前状态;rollback 自身失败通过 `errors.Join` 暴露,绝不假装成功。
> - 单文件替换在 POSIX 使用 same-directory temp+rename;Windows 因 rename 不能覆盖已有目标,使用 delete-existing-then-rename。delete 与 rename 间不是 crash-atomic,但任一进程内 error 会由全量 snapshot rollback 补偿;此 scope cut 必须记录在 `Limitations`。
> - 只有 `MaterializeMain` 返回 nil 后,Task 5 才允许推进 head。`ResetMainHead` 仅更新 `vcs_repos.main_head`;Task 5 的 head+undo/audit seam 仍在一个 caller-owned SQLite tx 中直接执行。

- [ ] **Step 1: 写失败测试** — `internal/vcs/revert_test.go`(新建)

```go
// internal/vcs/revert_test.go
package vcs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestMaterializeMain_RestoresFileContents(t *testing.T) {
	v, repoID, root := setupSeamTestRepo(t)
	aPath := filepath.Join(root, "a.txt")
	// Advance: v0 -> v1 -> v2.
	for _, ver := range []string{"v1", "v2"} {
		if err := os.WriteFile(aPath, []byte(ver), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", ver, err)
		}
		if err := v.RecordEditMain(repoID, "u", aPath, []byte(ver)); err != nil {
			t.Fatalf("RecordEditMain %s: %v", ver, err)
		}
		if _, err := v.CommitMain(repoID, "u", ver); err != nil {
			t.Fatalf("CommitMain %s: %v", ver, err)
		}
	}
	log, err := v.LogMain(repoID, 3)
	if err != nil {
		t.Fatalf("LogMain: %v", err)
	}
	if len(log) < 3 {
		t.Fatalf("expected >=3 commits, got %d", len(log))
	}
	v0ID := log[2].ID // newest-first: [v2, v1, v0]
	if err := v.MaterializeMain(repoID, v0ID); err != nil {
		t.Fatalf("MaterializeMain: %v", err)
	}
	got, err := os.ReadFile(aPath)
	if err != nil {
		t.Fatalf("read a.txt: %v", err)
	}
	if string(got) != "v0" {
		t.Errorf("after materialize v0: a.txt = %q, want %q", got, "v0")
	}
}

func TestMaterializeMain_DeletesFilesAbsentInTarget(t *testing.T) {
	v, repoID, root := setupSeamTestRepo(t)
	extra := filepath.Join(root, "extra.txt")
	if err := os.WriteFile(extra, []byte("present"), 0o644); err != nil {
		t.Fatalf("WriteFile extra: %v", err)
	}
	if err := v.RecordEditMain(repoID, "u", extra, []byte("present")); err != nil {
		t.Fatalf("RecordEditMain: %v", err)
	}
	if _, err := v.CommitMain(repoID, "u", "add extra"); err != nil {
		t.Fatalf("CommitMain: %v", err)
	}
	log, err := v.LogMain(repoID, 3)
	if err != nil {
		t.Fatalf("LogMain: %v", err)
	}
	priorID := log[1].ID // [with-extra, prior]
	if err := v.MaterializeMain(repoID, priorID); err != nil {
		t.Fatalf("MaterializeMain: %v", err)
	}
	if _, err := os.Stat(extra); !os.IsNotExist(err) {
		t.Errorf("extra.txt should be removed; stat err = %v", err)
	}
}

// TestMaterializeMain_RejectsWrongRepo — 必修项 G: commit 归属校验。
func TestMaterializeMain_RejectsWrongRepo(t *testing.T) {
	v, repoID, _ := setupSeamTestRepo(t)
	root2 := t.TempDir()
	repoID2, err := v.InitRepo(root2)
	if err != nil {
		t.Fatalf("InitRepo root2: %v", err)
	}
	p2 := filepath.Join(root2, "b.txt")
	_ = v.RecordEditMain(repoID2, "u", p2, []byte("B"))
	head2, err := v.CommitMain(repoID2, "u", "b")
	if err != nil {
		t.Fatalf("commit on repo2: %v", err)
	}
	err = v.MaterializeMain(repoID, head2)
	if err == nil {
		t.Fatal("MaterializeMain 应当拒绝 cross-repo commit")
	}
}

// TestMaterializeMain_RejectsWorktreeCommit — only main commits are valid
// rollback targets.
func TestMaterializeMain_RejectsWorktreeCommit(t *testing.T) {
	v, repoID, _ := setupSeamTestRepo(t)
	wt, err := v.AddWorktree(repoID, []string{"test"})
	if err != nil {
		t.Fatalf("AddWorktree: %v", err)
	}
	wtFile := filepath.Join(wt.Path, "wt.txt")
	if err := v.RecordEditWorktree(wt.ID, "u", wtFile, []byte("WT")); err != nil {
		t.Fatalf("RecordEditWorktree: %v", err)
	}
	wtHead, err := v.CommitWorktree(wt.ID, "u", "wt commit")
	if err != nil {
		t.Fatalf("CommitWorktree: %v", err)
	}
	err = v.MaterializeMain(repoID, wtHead)
	if err == nil {
		t.Fatal("MaterializeMain 应当拒绝 worktree commit")
	}
}

// TestMaterializeMain_FailFastOnMissingBlob injects a vcs_tree row whose blob
// is missing, asserts MaterializeMain errors WITHOUT touching the working copy.
func TestMaterializeMain_FailFastOnMissingBlob(t *testing.T) {
	v, repoID, root := setupSeamTestRepo(t)
	aPath := filepath.Join(root, "a.txt")
	_ = v.RecordEditMain(repoID, "u", aPath, []byte("v1"))
	head, _ := v.CommitMain(repoID, "u", "v1")
	tree := v.commitTree(head)
	for _, h := range tree {
		if _, err := v.store.DB.Exec("DELETE FROM vcs_blobs WHERE hash = ?", h); err != nil {
			t.Fatalf("delete blob: %v", err)
		}
	}
	before, _ := os.ReadFile(aPath)
	err := v.MaterializeMain(repoID, head)
	if err == nil {
		t.Fatal("MaterializeMain 应当对 missing blob 报错")
	}
	after, _ := os.ReadFile(aPath)
	if string(before) != string(after) {
		t.Errorf("working copy 被改动: before=%q after=%q", before, after)
	}
}

func TestMaterializeMain_RollsBackAllTouchedFilesOnInjectedFailure(t *testing.T) {
	v, repoID, root := setupSeamTestRepo(t)
	targetHead := mustMainHead(t, v, repoID) // seed tree: a.txt=v0, no extra.txt
	aPath := filepath.Join(root, "a.txt")
	extraPath := filepath.Join(root, "extra.txt")

	// Build and materialize the pre-operation state: a.txt=v2 + extra.txt=keep.
	if err := v.RecordEditMain(repoID, "u", aPath, []byte("v2")); err != nil {
		t.Fatalf("record a: %v", err)
	}
	if err := v.RecordEditMain(repoID, "u", extraPath, []byte("keep")); err != nil {
		t.Fatalf("record extra: %v", err)
	}
	currentHead, err := v.CommitMain(repoID, "u", "current")
	if err != nil {
		t.Fatalf("commit current: %v", err)
	}
	if err := os.WriteFile(aPath, []byte("v2"), 0o644); err != nil {
		t.Fatalf("write a: %v", err)
	}
	if err := os.WriteFile(extraPath, []byte("keep"), 0o644); err != nil {
		t.Fatalf("write extra: %v", err)
	}

	calls := 0
	injected := errors.New("injected second mutation failure")
	err = v.materializeMainLockedWithHook(repoID, targetHead,
		func(stage, path string) error {
			calls++
			if calls == 2 {
				return injected
			}
			return nil
		})
	if !errors.Is(err, injected) {
		t.Fatalf("MaterializeMain error = %v, want injected", err)
	}
	if got, readErr := os.ReadFile(aPath); readErr != nil || string(got) != "v2" {
		t.Fatalf("a.txt after compensation = %q, err=%v; want v2", got, readErr)
	}
	if got, readErr := os.ReadFile(extraPath); readErr != nil || string(got) != "keep" {
		t.Fatalf("extra.txt after compensation = %q, err=%v; want keep", got, readErr)
	}
	if got := mustMainHead(t, v, repoID); got != currentHead {
		t.Fatalf("head advanced on failed materialize: got %s want %s", got, currentHead)
	}
}

func TestReplaceFile_ReplacesExistingDestination(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "existing.txt")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := replaceFile(path, []byte("new"), 0o644); err != nil {
		t.Fatalf("replaceFile: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "new" {
		t.Fatalf("got %q, err=%v; want new", got, err)
	}
}

func TestResetMainHead_UpdatesRepo(t *testing.T) {
	v, repoID, _ := setupSeamTestRepo(t)
	log, _ := v.LogMain(repoID, 5)
	if len(log) < 1 {
		t.Fatal("need >=1 commit")
	}
	newHead := log[0].ParentID
	if newHead == "" {
		t.Skip("root commit has no parent")
	}
	if err := v.ResetMainHead(repoID, newHead); err != nil {
		t.Fatalf("ResetMainHead: %v", err)
	}
	got, _ := v.RepoMainHead(repoID)
	if got != newHead {
		t.Errorf("after reset: RepoMainHead = %s, want %s", got, newHead)
	}
}

func TestValidateRelPath_DotGitRejected(t *testing.T) {
	bad := []string{".git/HEAD", "foo/.git/config", "../escape", "/abs/path", "sub/../.."}
	for _, p := range bad {
		if err := validateRelPath(p); err == nil {
			t.Errorf("validateRelPath(%q) 应当报错", p)
		}
	}
	good := []string{"a.txt", "sub/dir/f.txt", "foo/bar.go"}
	for _, p := range good {
		if err := validateRelPath(p); err != nil {
			t.Errorf("validateRelPath(%q) 不应报错: %v", p, err)
		}
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/vcs/ -run 'TestMaterializeMain|TestReplaceFile|TestResetMainHead|TestValidateRelPath' -v`
Expected: 编译失败(`MaterializeMain`/`materializeMainLockedWithHook`/`replaceFile`/`ResetMainHead`/`validateRelPath` 未定义);旧实现也无法通过注入失败后的全量文件恢复断言。

- [ ] **Step 3: 创建 `internal/vcs/revert.go`(Task 4 部分)**

```go
// internal/vcs/revert.go
//
// revert.go 实现 B2-RB1 的回滚核心:MaterializeMain(把 main 工作副本物化到
// 指定 commit 的树)、ResetMainHead(更新 vcs_repos.main_head)、RevertToSeam
// (Task 5 加)。所有方法 strict、fail-fast,任何 blob/IO 失败都立即 return error,
// 不静默 continue(必修项 G)。
//
// Concurrency: public MaterializeMain/ResetMainHead acquire the per-repo lane.
// RevertToSeam already owns that lane and calls private locked cores.

package vcs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// validateRelPath enforces the path-safety rules a tree path must satisfy
// before MaterializeMain will touch the working copy (必修项 G):
//   - relative (no leading slash)
//   - no ".." segment (no parent-dir escape)
//   - no ".git" segment (autoVCS never tracks .git, but defense in depth)
//   - non-empty
func validateRelPath(rel string) error {
	rel = filepath.ToSlash(rel)
	if rel == "" || rel == "." {
		return fmt.Errorf("vcs: empty path")
	}
	if strings.HasPrefix(rel, "/") {
		return fmt.Errorf("vcs: absolute path %q", rel)
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == ".." {
			return fmt.Errorf("vcs: parent escape in %q", rel)
		}
		if seg == ".git" {
			return fmt.Errorf("vcs: .git segment in %q", rel)
		}
	}
	return nil
}

// MaterializeMain is the public serialized entry point (CB1).
func (v *VCS) MaterializeMain(repoID, commitID string) error {
	unlock := v.lockRepo(repoID)
	defer unlock()
	return v.materializeMainLocked(repoID, commitID)
}

func (v *VCS) materializeMainLocked(repoID, commitID string) error {
	return v.materializeMainLockedWithHook(repoID, commitID, nil)
}

type materializeHook func(stage, path string) error

type workingFileSnapshot struct {
	exists bool
	data   []byte
	mode   os.FileMode
}

// materializeMainLockedWithHook is the deterministic core. hook is nil in
// production; same-package tests inject an error between mutations to prove
// snapshot compensation restores every touched path.
func (v *VCS) materializeMainLockedWithHook(
	repoID, commitID string, hook materializeHook,
) error {
	c, err := v.getCommit(commitID)
	if err != nil {
		return fmt.Errorf("vcs: materialize: load commit %s: %w", commitID, err)
	}
	if c.RepoID != repoID {
		return fmt.Errorf("vcs: materialize: commit %s belongs to repo %s, not %s",
			commitID, c.RepoID, repoID)
	}
	if c.WorktreeID != "" {
		return fmt.Errorf("vcs: materialize: commit %s is worktree commit %s",
			commitID, c.WorktreeID)
	}
	r, err := v.getRepo(repoID)
	if err != nil {
		return fmt.Errorf("vcs: materialize: load repo: %w", err)
	}

	targetTree := v.commitTree(commitID)
	prevTree := v.commitTree(r.MainHead)
	paths := sortedTreeUnion(prevTree, targetTree)
	for _, path := range paths {
		if err := validateRelPath(path); err != nil {
			return err
		}
	}

	// Resolve every target blob before the first filesystem mutation.
	targetBytes := make(map[string][]byte, len(targetTree))
	for path, hash := range targetTree {
		content, err := v.getBlob(hash)
		if err != nil {
			return fmt.Errorf("vcs: materialize: blob %s for %s: %w", hash, path, err)
		}
		targetBytes[path] = content
	}

	// D1: snapshot all tracked paths that this operation may delete/replace.
	snapshot, err := snapshotWorkingFiles(r.RootPath, paths)
	if err != nil {
		return fmt.Errorf("vcs: materialize: snapshot: %w", err)
	}
	compensate := func(cause error) error {
		if rollbackErr := restoreWorkingFiles(r.RootPath, paths, snapshot); rollbackErr != nil {
			return errors.Join(cause,
				fmt.Errorf("vcs: materialize: compensation failed: %w", rollbackErr))
		}
		return cause
	}

	// Delete prev-only paths first, in deterministic order.
	for _, path := range paths {
		if _, wasPresent := prevTree[path]; !wasPresent {
			continue
		}
		if _, remains := targetTree[path]; remains {
			continue
		}
		if hook != nil {
			if err := hook("remove", path); err != nil {
				return compensate(err)
			}
		}
		abs := filepath.Join(r.RootPath, filepath.FromSlash(path))
		if err := os.Remove(abs); err != nil && !errors.Is(err, os.ErrNotExist) {
			return compensate(fmt.Errorf("vcs: materialize: remove %s: %w", path, err))
		}
	}

	// Replace target paths. sortedTreeUnion keeps order deterministic for tests.
	for _, path := range paths {
		content, ok := targetBytes[path]
		if !ok {
			continue
		}
		if hook != nil {
			if err := hook("replace", path); err != nil {
				return compensate(err)
			}
		}
		abs := filepath.Join(r.RootPath, filepath.FromSlash(path))
		mode := os.FileMode(0o644)
		if prior := snapshot[path]; prior.exists {
			mode = prior.mode
		}
		if err := replaceFile(abs, content, mode); err != nil {
			return compensate(fmt.Errorf("vcs: materialize: replace %s: %w", path, err))
		}
	}
	return nil
}

func sortedTreeUnion(a, b map[string]string) []string {
	set := make(map[string]struct{}, len(a)+len(b))
	for path := range a {
		set[path] = struct{}{}
	}
	for path := range b {
		set[path] = struct{}{}
	}
	paths := make([]string, 0, len(set))
	for path := range set {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

func snapshotWorkingFiles(root string, paths []string) (map[string]workingFileSnapshot, error) {
	out := make(map[string]workingFileSnapshot, len(paths))
	for _, path := range paths {
		abs := filepath.Join(root, filepath.FromSlash(path))
		info, err := os.Stat(abs)
		if errors.Is(err, os.ErrNotExist) {
			out[path] = workingFileSnapshot{}
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("stat %s: %w", path, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("vcs: materialize: %s is not a regular file", path)
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		out[path] = workingFileSnapshot{exists: true, data: data, mode: info.Mode().Perm()}
	}
	return out, nil
}

func restoreWorkingFiles(
	root string, paths []string, snapshot map[string]workingFileSnapshot,
) error {
	var errs []error
	for _, path := range paths {
		abs := filepath.Join(root, filepath.FromSlash(path))
		prior := snapshot[path]
		if prior.exists {
			if err := replaceFile(abs, prior.data, prior.mode); err != nil {
				errs = append(errs, fmt.Errorf("restore %s: %w", path, err))
			}
			continue
		}
		if err := os.Remove(abs); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("remove new %s: %w", path, err))
		}
	}
	return errors.Join(errs...)
}

// replaceFile uses same-directory temp+rename. POSIX rename replaces atomically;
// Windows requires delete-existing-then-rename. The caller's snapshot
// compensation repairs the delete→rename gap on any returned process error.
func replaceFile(path string, data []byte, mode os.FileMode) (err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".yanshi-tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		if err != nil {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		return err
	}
	if err = tmp.Chmod(mode); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		if removeErr := os.Remove(path); removeErr != nil &&
			!errors.Is(removeErr, os.ErrNotExist) {
			return removeErr
		}
	}
	if err = os.Rename(tmpName, path); err != nil {
		return err
	}
	return nil
}

// ResetMainHead is also a public repo-serialized write path (CB1).
func (v *VCS) ResetMainHead(repoID, commitID string) error {
	unlock := v.lockRepo(repoID)
	defer unlock()
	return v.resetMainHeadLocked(repoID, commitID)
}

func (v *VCS) resetMainHeadLocked(repoID, commitID string) error {
	c, err := v.getCommit(commitID)
	if err != nil {
		return fmt.Errorf("vcs: reset head: load commit: %w", err)
	}
	if c.RepoID != repoID || c.WorktreeID != "" {
		return fmt.Errorf("vcs: reset head: commit %s is not main commit of repo %s",
			commitID, repoID)
	}
	res, err := v.store.DB.Exec(
		"UPDATE vcs_repos SET main_head=? WHERE id=?", commitID, repoID)
	if err != nil {
		return fmt.Errorf("vcs: reset head: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("vcs: reset head rows: %w", err)
	}
	if n != 1 {
		return fmt.Errorf("vcs: reset head: repo %s not found", repoID)
	}
	return nil
}

// (RevertToSeam is added in Task 5.)
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/vcs/ -run 'TestMaterializeMain|TestReplaceFile|TestResetMainHead|TestValidateRelPath' -v`
Expected: PASS,包括第二次 mutation 注入失败后所有 touched files 与 head 完整恢复;Windows 上已有目标也能 delete-then-rename 替换。

- [ ] **Step 5: 扩展并验证 public-writer lock contract**

在 `internal/vcs/seam_race_test.go` 的 `publicRepoWriters` 末尾追加 `"MaterializeMain", "ResetMainHead"`,然后运行:

Run: `go test ./internal/vcs/ -run TestPublicRepoWritersAcquireRepoLane -v`
Expected: PASS；两个新 public writer 都直接获取 `lockRepo`,私有 `*Locked` core 不重复加锁。

- [ ] **Step 6: 提交**

```bash
git add internal/vcs/revert.go internal/vcs/revert_test.go internal/vcs/seam_race_test.go
git commit -m "feat(vcs): add MaterializeMain + ResetMainHead (strict, fail-fast, path-safe) (B2-RB1 G)"
```

---

## Task 5: VCS — RevertToSeam(undo seam + 原子 tx + 可逆性测试)

**Files:**
- Modify: `internal/vcs/revert.go`(加 `RevertToSeam`)
- Modify: `internal/vcs/seam_race_test.go`（在 writer contract 与 mixed-write race 覆盖中追加 `RevertToSeam`）
- Test: `internal/vcs/revert_test.go`(追加 `TestRevertToSeam_*` 子测试,同文件,不重复 package/import)

> 必修项 A + G。`RevertToSeam` 是回滚入口,语义:
>
> 1. 持 per-repo 锁。
> 2. 读 seam 行(`FindSeam`),校验 seam.RepoID == repoID。
> 3. 捕获 `previousHead = RepoMainHead(repoID)`(在 mutate 前)。
> 4. 校验 target commit(seam.CommitID)属于 repoID 且是 main commit。
> 5. 在 mutate 前额外快照 `previousHead ∪ targetCommit` 的全部 touched paths,再调用已持锁版本 `materializeMainLocked(repoID, targetCommit)`;禁止调用 public `MaterializeMain` 造成同 repo 自锁。
> 6. 开 SQLite tx:`UPDATE vcs_repos SET main_head=targetCommit` + 插入 undo seam(`kind=SeamPreRevert`,`commit_id=previousHead`)+ 插入 audit seam(`kind=SeamPostRevert`,`commit_id=targetCommit`)。任一 Begin/Exec/Commit 失败,defer 用 pre-op snapshot 恢复文件后返回 error;补偿失败用 `errors.Join` 一并暴露,不得留下“disk=target/head=previous”却声称可重试的半状态。
> 7. tx 成功才解除补偿并返回 undo seam id。
>
> 关键不变式:**返回的 id 指向 previousHead**。对它再次 `RevertToSeam` 即把 main 回到本次回滚之前的状态。Task 5 Step 1 的 `TestRevertToSeam_RoundTripIsReversible` 直接验证此性质。

- [ ] **Step 1: 写失败测试** — 追加到 `internal/vcs/revert_test.go`(同文件,不重复 package/import)

```go
// 追加到 internal/vcs/revert_test.go(不加 package / import 行)

// TestRevertToSeam_RevertsFilesAndHead verifies the basic revert: after two
// turns, reverting to the pre-turn-1 seam puts a.txt back to v0 AND advances
// main_head to the v0 commit.
func TestRevertToSeam_RevertsFilesAndHead(t *testing.T) {
	v, repoID, root := setupSeamTestRepo(t)
	aPath := filepath.Join(root, "a.txt")

	// Seal a pre-turn-1 seam BEFORE advancing. The seam captures the v0 head.
	pre1ID, err := v.SealMainTurnSeam(repoID, "s1", 0, 0, SeamPreTurn, "pre-turn:1")
	if err != nil {
		t.Fatalf("SealMainTurnSeam pre1: %v", err)
	}
	pre1, _ := v.FindSeam(pre1ID)
	v0Commit := pre1.CommitID

	// Two turns advance to v2.
	for _, ver := range []string{"v1", "v2"} {
		if err := os.WriteFile(aPath, []byte(ver), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", ver, err)
		}
		if err := v.RecordEditMain(repoID, "u", aPath, []byte(ver)); err != nil {
			t.Fatalf("RecordEditMain %s: %v", ver, err)
		}
		if _, err := v.CommitMain(repoID, "u", ver); err != nil {
			t.Fatalf("CommitMain %s: %v", ver, err)
		}
	}
	headV2 := mustMainHead(t, v, repoID)

	// Revert to pre1.
	undoID, err := v.RevertToSeam(repoID, pre1ID, "revert to pre1", 0, 0, nil)
	if err != nil {
		t.Fatalf("RevertToSeam: %v", err)
	}
	if undoID == "" {
		t.Fatal("undo seam id 空串")
	}
	got, _ := os.ReadFile(aPath)
	if string(got) != "v0" {
		t.Errorf("after revert: a.txt = %q, want %q", got, "v0")
	}
	headAfter := mustMainHead(t, v, repoID)
	if headAfter != v0Commit {
		t.Errorf("after revert: head = %s, want %s(v0Commit)", headAfter, v0Commit)
	}

	// The undo seam MUST point at the pre-revert head (v2) — that is what makes
	// the revert reversible. Reverting the undo seam puts a.txt back to v2.
	undoSeam, _ := v.FindSeam(undoID)
	if undoSeam.CommitID != headV2 {
		t.Errorf("undo seam.CommitID = %s, want %s(previousHead v2)", undoSeam.CommitID, headV2)
	}
	if undoSeam.Kind != SeamPreRevert {
		t.Errorf("undo seam.Kind = %q, want %q", undoSeam.Kind, SeamPreRevert)
	}
}

// TestRevertToSeam_RoundTripIsReversible — 必修项 A 核心证据。RevertToSeam(r1)
// must restore the pre-revert state. The test fails if RevertToSeam returns a
// post-revert seam (pointing at the target) instead of an undo seam (pointing
// at previousHead): r1's commit would be the target, so reverting r1 would be
// a no-op, leaving a.txt at v0 instead of going back to v2.
func TestRevertToSeam_RoundTripIsReversible(t *testing.T) {
	v, repoID, root := setupSeamTestRepo(t)
	aPath := filepath.Join(root, "a.txt")
	pre1ID, _ := v.SealMainTurnSeam(repoID, "s1", 0, 0, SeamPreTurn, "pre-turn:1")
	for _, ver := range []string{"v1", "v2"} {
		if err := os.WriteFile(aPath, []byte(ver), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", ver, err)
		}
		if err := v.RecordEditMain(repoID, "u", aPath, []byte(ver)); err != nil {
			t.Fatalf("RecordEditMain %s: %v", ver, err)
		}
		if _, err := v.CommitMain(repoID, "u", ver); err != nil {
			t.Fatalf("CommitMain %s: %v", ver, err)
		}
	}

	// First revert: v2 -> v0.
	r1, err := v.RevertToSeam(repoID, pre1ID, "first revert", 0, 0, nil)
	if err != nil {
		t.Fatalf("first RevertToSeam: %v", err)
	}
	if got, _ := os.ReadFile(aPath); string(got) != "v0" {
		t.Fatalf("after first revert: a.txt = %q, want v0", got)
	}

	// Undo the revert by reverting r1 (the undo seam pointing at v2 head).
	_, err = v.RevertToSeam(repoID, r1, "undo first revert", 0, 0, nil)
	if err != nil {
		t.Fatalf("undo RevertToSeam(r1): %v", err)
	}
	got, _ := os.ReadFile(aPath)
	if string(got) != "v2" {
		t.Errorf("after RevertToSeam(r1): a.txt = %q, want %q(undo)", got, "v2")
	}
}

// TestRevertToSeam_RejectsWrongRepo — 必修项 G.
func TestRevertToSeam_RejectsWrongRepo(t *testing.T) {
	v, repoID, _ := setupSeamTestRepo(t)
	preID, _ := v.SealMainTurnSeam(repoID, "s1", 0, 0, SeamPreTurn, "pre")
	root2 := t.TempDir()
	repoID2, _ := v.InitRepo(root2)
	_, err := v.RevertToSeam(repoID2, preID, "cross-repo", 0, 0, nil)
	if err == nil {
		t.Fatal("RevertToSeam 应当拒绝 cross-repo seam")
	}
}

// TestRevertToSeam_FailFastOnMissingBlob — 必修项 G: materialize failure
// MUST NOT advance main_head.
func TestRevertToSeam_FailFastOnMissingBlob(t *testing.T) {
	v, repoID, root := setupSeamTestRepo(t)
	aPath := filepath.Join(root, "a.txt")
	preID, _ := v.SealMainTurnSeam(repoID, "s1", 0, 0, SeamPreTurn, "pre")
	_ = v.RecordEditMain(repoID, "u", aPath, []byte("v1"))
	_, _ = v.CommitMain(repoID, "u", "v1")
	pre, _ := v.FindSeam(preID)
	// Corrupt: delete blobs referenced by pre.CommitID.
	for _, h := range v.commitTree(pre.CommitID) {
		_, _ = v.store.DB.Exec("DELETE FROM vcs_blobs WHERE hash = ?", h)
	}
	headBefore := mustMainHead(t, v, repoID)
	_, err := v.RevertToSeam(repoID, preID, "should fail", 0, 0, nil)
	if err == nil {
		t.Fatal("RevertToSeam 应当对 missing blob 报错")
	}
	headAfter := mustMainHead(t, v, repoID)
	if headAfter != headBefore {
		t.Errorf("失败后 head 被切换: before=%s after=%s", headBefore, headAfter)
	}
}

// TestRevertToSeam_AtomicallyInsertsUndoAndAuditSeam — 必修项 G: the undo +
// audit seams + head reset MUST land in one tx.
func TestRevertToSeam_AtomicallyInsertsUndoAndAuditSeam(t *testing.T) {
	v, repoID, _ := setupSeamTestRepo(t)
	preID, _ := v.SealMainTurnSeam(repoID, "s1", 0, 0, SeamPreTurn, "pre")
	undoID, err := v.RevertToSeam(repoID, preID, "audit check", 0, 0, nil)
	if err != nil {
		t.Fatalf("RevertToSeam: %v", err)
	}
	seams, _ := v.ListSeams(repoID, "", 0) // VCS-only call stores undo/audit outside WS sessions.
	// Expected: preID, undoID, plus one SeamPostRevert audit seam.
	kinds := map[string]int{}
	for _, s := range seams {
		kinds[string(s.Kind)]++
		if s.ID == undoID && s.Kind != SeamPreRevert {
			t.Errorf("undo seam kind = %q, want %q", s.Kind, SeamPreRevert)
		}
	}
	if kinds["pre-revert"] == 0 {
		t.Errorf("未找到 pre-revert(undo)seam;kinds=%v", kinds)
	}
	if kinds["post-revert"] == 0 {
		t.Errorf("未找到 post-revert(audit)seam;kinds=%v", kinds)
	}
}

// TestRevertToSeam_TxFailureRestoresPreOperationFiles proves that a DB failure
// after successful materialization does not leave disk at target with old head.
func TestRevertToSeam_TxFailureRestoresPreOperationFiles(t *testing.T) {
	v, repoID, root := setupSeamTestRepo(t)
	aPath := filepath.Join(root, "a.txt")
	preID, err := v.SealMainTurnSeam(repoID, "s1", 0, 0,
		SeamPreTurn, "pre-turn:1")
	if err != nil {
		t.Fatalf("seal target: %v", err)
	}
	if err := v.RecordEditMain(repoID, "u", aPath, []byte("v1")); err != nil {
		t.Fatalf("record v1: %v", err)
	}
	if err := os.WriteFile(aPath, []byte("v1"), 0o644); err != nil {
		t.Fatalf("write v1: %v", err)
	}
	currentHead, err := v.CommitMain(repoID, "u", "v1")
	if err != nil {
		t.Fatalf("commit v1: %v", err)
	}
	if _, err := v.store.DB.Exec(`
		CREATE TRIGGER fail_pre_revert_insert
		BEFORE INSERT ON vcs_seams
		WHEN NEW.kind = 'pre-revert'
		BEGIN SELECT RAISE(ABORT, 'injected revert tx failure'); END;
	`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	if _, err := v.RevertToSeam(repoID, preID, "inject", 0, 0, nil); err == nil {
		t.Fatal("RevertToSeam should fail when undo seam INSERT is aborted")
	}
	got, readErr := os.ReadFile(aPath)
	if readErr != nil || string(got) != "v1" {
		t.Fatalf("disk after tx failure = %q, err=%v; want pre-op v1", got, readErr)
	}
	if gotHead := mustMainHead(t, v, repoID); gotHead != currentHead {
		t.Fatalf("head after tx failure = %s, want %s", gotHead, currentHead)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/vcs/ -run TestRevertToSeam -v`
Expected: 编译失败(`RevertToSeam` 未定义)

- [ ] **Step 3: 在 `internal/vcs/revert.go` 追加 `RevertToSeam`**

```go
// 追加到 internal/vcs/revert.go(不加 package/import)

// RevertToSeam reverts repoID's main scope to the state captured by seamID.
// Returns the undo seam id (pointing at the PRE-revert main_head) — call
// RevertToSeam again with that id to UNDO this revert. The undo + audit seams
// and the main_head reset land in ONE SQLite transaction (必修项 G). Before
// materialization this method snapshots every touched working-copy path; if
// Begin/Exec/Commit fails after disk mutation, a deferred compensation restores
// the exact pre-operation bytes/existence/modes (D1).
//
// prevHistoryLen / prevTurnSeq record the caller's conversation boundary
// (len(history), turns) AT REVERT TIME. historySnap is the exact durable state
// at that same boundary. All three are stored on the undo seam in the same tx
// as main_head, so reverting the undo seam can restore deleted messages after a
// reconnect, not merely recover counters (D2). The WS handler passes real values
// and a non-nil snapshot; the agent revert_turn tool passes 0,0,nil because it
// does not own WS conversation history.
//
// Must be called WITHOUT the repo lock held; the method acquires it for the full
// materialize + tx sequence so a concurrent CommitMain cannot race the head.
func (v *VCS) RevertToSeam(
	repoID, seamID, label string,
	prevHistoryLen, prevTurnSeq int,
	historySnap *store.SessionRevertSnapshot,
) (undoSeamID string, err error) {
	unlock := v.lockRepo(repoID)
	defer unlock()

	// (1) Load + validate seam.
	seam, err := v.FindSeam(seamID)
	if err != nil {
		return "", fmt.Errorf("vcs: revert: load seam: %w", err)
	}
	if seam.RepoID != repoID {
		return "", fmt.Errorf("vcs: revert: seam %s belongs to repo %s, not %s",
			seamID, seam.RepoID, repoID)
	}

	// D2: the WS caller supplies the exact pre-revert durable session snapshot.
	// Encode before touching disk. Agent VCS-only reverts pass nil and store an
	// empty blob. They are assigned session_id="" below so their undo/audit seams
	// never enter a WS session list or imply conversation-history undo (D4).
	historyJSON := []byte{}
	if historySnap != nil {
		if historySnap.Meta.ID != seam.SessionID {
			return "", fmt.Errorf("vcs: revert: history snapshot session %s does not match seam session %s",
				historySnap.Meta.ID, seam.SessionID)
		}
		historyJSON, err = store.EncodeSessionRevertSnapshot(*historySnap)
		if err != nil {
			return "", fmt.Errorf("vcs: revert: encode history snapshot: %w", err)
		}
	}

	// (2) Capture previousHead BEFORE any mutation.
	r, err := v.getRepo(repoID)
	if err != nil {
		return "", fmt.Errorf("vcs: revert: load repo: %w", err)
	}
	previousHead := r.MainHead

	// (3) Validate target commit (seam.CommitID) belongs to repo + is main.
	targetCommit := seam.CommitID
	c, err := v.getCommit(targetCommit)
	if err != nil {
		return "", fmt.Errorf("vcs: revert: target commit: %w", err)
	}
	if c.RepoID != repoID {
		return "", fmt.Errorf("vcs: revert: commit %s not in repo %s", targetCommit, repoID)
	}
	if c.WorktreeID != "" {
		return "", fmt.Errorf("vcs: revert: commit %s is worktree, not main", targetCommit)
	}

	// (4) Snapshot exact pre-op working-copy state for every path either tree may
	// touch. Validate before Join/read so a malicious tree path cannot escape root.
	rollbackPaths := sortedTreeUnion(v.commitTree(previousHead), v.commitTree(targetCommit))
	for _, path := range rollbackPaths {
		if err := validateRelPath(path); err != nil {
			return "", err
		}
	}
	diskBefore, err := snapshotWorkingFiles(r.RootPath, rollbackPaths)
	if err != nil {
		return "", fmt.Errorf("vcs: revert: snapshot: %w", err)
	}

	// (5) Already holding repo lane: call private core, never public wrapper.
	if err := v.materializeMainLocked(repoID, targetCommit); err != nil {
		return "", fmt.Errorf("vcs: revert: materialize: %w", err)
	}
	keepMaterialized := false
	defer func() {
		if keepMaterialized {
			return
		}
		if rollbackErr := restoreWorkingFiles(r.RootPath, rollbackPaths, diskBefore); rollbackErr != nil {
			err = errors.Join(err,
				fmt.Errorf("vcs: revert: restore pre-operation files: %w", rollbackErr))
		}
	}()

	// (6) Atomic tx: reset head + insert undo + audit seams.
	tx, err := v.store.DB.Begin()
	if err != nil {
		return "", fmt.Errorf("vcs: revert: begin tx: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(
		"UPDATE vcs_repos SET main_head = ? WHERE id = ?", targetCommit, repoID,
	); err != nil {
		return "", fmt.Errorf("vcs: revert: reset head: %w", err)
	}

	undoID := newVCSID()
	auditID := newVCSID()
	now := time.Now().Unix()
	sessionID := seam.SessionID
	if historySnap == nil {
		// D4: an agent/tool revert is VCS-only. Keep its undo/audit seams outside
		// every WS session namespace; the returned exact ID still supports a later
		// VCS-only undo through the tool.
		sessionID = ""
	}
	turnSeq := seam.TurnSeq

	// UNDO seam: points at PREVIOUS head. Returning this id is how the caller
	// can undo this revert (re-revert to previousHead). turn_seq/history_len copy
	// the TARGET seam's boundary (the state being reverted TO); prev_turn_seq /
	// prev_history_len record the PRE-revert boundary (D2) so reverting THIS undo
	// seam restores the longer history.
	if _, err := tx.Exec(
		`INSERT INTO vcs_seams (id, repo_id, session_id, commit_id, turn_seq, history_len, prev_turn_seq, prev_history_len, history_snapshot, kind, label, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		undoID, repoID, sessionID, previousHead, turnSeq, seam.HistoryLen,
		prevTurnSeq, prevHistoryLen, historyJSON,
		string(SeamPreRevert), label, now,
	); err != nil {
		return "", fmt.Errorf("vcs: revert: insert undo seam: %w", err)
	}

	// AUDIT seam: points at TARGET commit. For audit trail only; never the
	// return value of RevertToSeam (reverting it would be a no-op). prev_* are 0
	// (audit seams are never reverted to).
	if _, err := tx.Exec(
		`INSERT INTO vcs_seams (id, repo_id, session_id, commit_id, turn_seq, history_len, prev_turn_seq, prev_history_len, history_snapshot, kind, label, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		auditID, repoID, sessionID, targetCommit, turnSeq, seam.HistoryLen,
		0, 0, []byte{},
		string(SeamPostRevert), label, now,
	); err != nil {
		return "", fmt.Errorf("vcs: revert: insert audit seam: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("vcs: revert: commit tx: %w", err)
	}
	keepMaterialized = true // DB head + seams now match disk; disable compensation.
	return undoID, nil
}
```

> 注:`revert.go` 的 Task 4 import block 已含 `errors`;本 Step 再加入 `"time"` 与 `"github.com/x6nux/yanshi/internal/store"`(undo seam 的 durable history snapshot)。最终 import 为 `errors`, `fmt`, `os`, `path/filepath`, `runtime`, `sort`, `strings`, `time`,以及 `internal/store`。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/vcs/ -run TestRevertToSeam -v`
Expected: PASS(含 `TestRevertToSeam_RoundTripIsReversible`)

- [ ] **Step 5: 扩展 lock contract，并在 race 测试中加入 RevertToSeam**

先在 `internal/vcs/seam_race_test.go` 的 `publicRepoWriters` 末尾追加 `"RevertToSeam"`;Task 2 的 AST contract 至此覆盖 CB1 的最终完整 writer 清单。然后修改 `TestRepoMu_ConcurrentMixedWrites` 末尾,把 `_ = preID` 换成:

```go
	if _, err := v.RevertToSeam(repoID, preID, "race undo", 0, 0, nil); err != nil {
		t.Errorf("RevertToSeam after concurrent writes: %v", err)
	}
```

Run: `go test -race ./internal/vcs/ -v`
Expected: 全部 PASS

- [ ] **Step 6: 提交**

```bash
git add internal/vcs/revert.go internal/vcs/revert_test.go internal/vcs/seam_race_test.go
git commit -m "feat(vcs): add RevertToSeam with undo seam + atomic tx (B2-RB1 A/G)"
```

---
## Task 6: proto — SeamInfo / list_seams / restore_turn / seams / seam_restored

**Files:**
- Modify: `internal/proto/frame.go`
- Test: `internal/proto/seam_frame_test.go`(新建)

> 必修项 I + J + D6。协议层加 4 个新帧(`list_seams` / `restore_turn` 客户端→服务端,`seams` / `seam_restored` 服务端→客户端),selector 只用 exact `seam_id`(删除 by-N 与 `turn_id` selector)。`ClientFrame.ConfirmedHead` 携带 list 时刻的**完整** main head id,服务端要求 non-empty 并比较完整 id(防止 list 与 restore 之间 head 被另一个 turn 改变;禁止 short-hash collision)。`ServerFrame.CommitShort` 只展示,`ServerFrame.Head` 才由客户端缓存为下一次 restore 的 `ConfirmedHead`。

- [ ] **Step 1: 写失败测试** — `internal/proto/seam_frame_test.go`(新建)

```go
// internal/proto/seam_frame_test.go
package proto

import (
	"encoding/json"
	"testing"
)

// TestNewListSeams_FrameShape verifies the list_seams request frame.
func TestNewListSeams_FrameShape(t *testing.T) {
	f := NewListSeams()
	if f.Type != "list_seams" {
		t.Errorf("Type = %q, want %q", f.Type, "list_seams")
	}
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Round-trip through ClientFrame to confirm it decodes cleanly.
	var cf ClientFrame
	if err := json.Unmarshal(b, &cf); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cf.Type != "list_seams" {
		t.Errorf("round-trip Type = %q", cf.Type)
	}
}

// TestNewRestoreTurn_FrameWithConfirmedHead verifies the restore_turn frame
// carries the seam_id and the confirmed_head binding (D6: FULL commit id).
func TestNewRestoreTurn_FrameWithConfirmedHead(t *testing.T) {
	f := NewRestoreTurn("abc123", "fullcommitid0123456789abcdef")
	if f.Type != "restore_turn" {
		t.Errorf("Type = %q", f.Type)
	}
	if f.ID != "abc123" {
		t.Errorf("ID = %q, want %q", f.ID, "abc123")
	}
	if f.ConfirmedHead != "fullcommitid0123456789abcdef" {
		t.Errorf("ConfirmedHead = %q, want full id", f.ConfirmedHead)
	}
	b, _ := json.Marshal(f)
	var cf ClientFrame
	_ = json.Unmarshal(b, &cf)
	if cf.ID != "abc123" || cf.ConfirmedHead != "fullcommitid0123456789abcdef" {
		t.Errorf("round-trip lost binding: ID=%q ConfirmedHead=%q", cf.ID, cf.ConfirmedHead)
	}
}

// TestNewSeams_FrameShape verifies the seams reply carries the list + the
// server's current head (short for display, full for binding — D6).
func TestNewSeams_FrameShape(t *testing.T) {
	items := []SeamInfo{
		{ID: "s1", Kind: "pre-turn", TurnSeq: 1, HistoryLen: 2, CommitShort: "abc12345", Label: "pre-turn:1"},
		{ID: "s2", Kind: "post-turn", TurnSeq: 1, HistoryLen: 3, CommitShort: "def67890", Label: "post-turn:1"},
	}
	f := NewSeams(items, "abc12345", "fullheadabcdef0123456789")
	if f.Type != "seams" {
		t.Errorf("Type = %q", f.Type)
	}
	if len(f.Seams) != 2 {
		t.Errorf("len(Seams) = %d, want 2", len(f.Seams))
	}
	if f.CommitShort != "abc12345" {
		t.Errorf("CommitShort = %q, want %q", f.CommitShort, "abc12345")
	}
	if f.Head != "fullheadabcdef0123456789" {
		t.Errorf("Head = %q, want full id", f.Head)
	}
	b, _ := json.Marshal(f)
	var sf ServerFrame
	_ = json.Unmarshal(b, &sf)
	if sf.Type != "seams" || len(sf.Seams) != 2 || sf.CommitShort != "abc12345" || sf.Head != "fullheadabcdef0123456789" {
		t.Errorf("round-trip lost: %+v", sf)
	}
}

// TestNewSeamRestored_FrameShape verifies the seam_restored reply's ID is the
// undo seam id and the frame carries the post-revert head (short + full — D6).
func TestNewSeamRestored_FrameShape(t *testing.T) {
	f := NewSeamRestored("undo-id", "abc12345", "fullpostreverthead0123456", "restored to pre-turn:1")
	if f.Type != "seam_restored" {
		t.Errorf("Type = %q", f.Type)
	}
	if f.ID != "undo-id" {
		t.Errorf("ID = %q, want undo-id", f.ID)
	}
	if f.CommitShort != "abc12345" {
		t.Errorf("CommitShort = %q", f.CommitShort)
	}
	if f.Head != "fullpostreverthead0123456" {
		t.Errorf("Head = %q, want full id", f.Head)
	}
	if f.Text != "restored to pre-turn:1" {
		t.Errorf("Text = %q", f.Text)
	}
}

// TestSeamInfo_OmitsEmptyFields verifies the SeamInfo JSON omits unset fields
// (so a future field addition does not bloat the wire format).
func TestSeamInfo_OmitsEmptyFields(t *testing.T) {
	s := SeamInfo{ID: "x", Kind: "pre-turn"}
	b, _ := json.Marshal(s)
	// CommitShort / Label should be omitted (omitempty).
	var m map[string]interface{}
	_ = json.Unmarshal(b, &m)
	if _, ok := m["commit_short"]; ok {
		t.Errorf("commit_short 应当 omitempty")
	}
	if _, ok := m["label"]; ok {
		t.Errorf("label 应当 omitempty")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/proto/ -run 'TestNewListSeams|TestNewRestoreTurn|TestNewSeams|TestNewSeamRestored|TestSeamInfo' -v`
Expected: 编译失败(`NewListSeams`/`NewRestoreTurn`/`NewSeams`/`NewSeamRestored`/`SeamInfo` 未定义,`ClientFrame.ConfirmedHead`/`ServerFrame.Seams`/`ServerFrame.CommitShort` 未定义)

- [ ] **Step 3: 修改 `internal/proto/frame.go`**

3a. 在 `ClientFrame` struct 的 `OutputSchema json.RawMessage` 字段后插入 `ConfirmedHead`，并在类型 doc comment 的 frame 词表加入 `list_seams` / `restore_turn`。不要重写或删除现有字段及 `OutputSchema` 的承重注释；精确插入内容是：

```go
	// ConfirmedHead carries the FULL main_head id the client observed when listing
	// seams. restore_turn re-sends it so the server can reject a stale target.
	// Empty is invalid and is rejected fail-closed (D6).
	ConfirmedHead string `json:"confirmed_head,omitempty"`
```

3b. 在 `ServerFrame` struct 加 `Seams`、`CommitShort` 和 `Head` 字段:

```go
// In ServerFrame struct (append after StructuredResult):
	// Seams is the list of recent seams for the current session, reply to
	// list_seams (必修项 I). Each entry has its own CommitShort for display.
	Seams []SeamInfo `json:"seams,omitempty"`
	// CommitShort carries the short (first-8-hex) hash of the current main_head
	// on status / seams / seam_restored frames, for DISPLAY only.
	CommitShort string `json:"commit_short,omitempty"`
	// Head carries the FULL main_head commit id on seams / seam_restored / status
	// frames. The TUI caches it and re-sends it as the restore_turn frame's
	// ConfirmedHead; the server compares the FULL id (not the short hash) to bind
	// the restore target (必修项 E / D6: short-hash collision is a real risk across
	// long histories, so the binding must use the untruncated id). Empty when VCS
	// is unconfigured.
	Head string `json:"head,omitempty"`
```

3c. 加 `SeamInfo` 类型 + 4 个构造函数。放在 `SessionInfo` 类型定义后:

```go
// SeamInfo carries one seam row for the seams list response (type "seams").
// ID is the exact seam id (必修项 J: selector 只用 seam_id,没有 by-N / turn_id)。
// CommitShort is the first 8 hex of the referenced commit, for display only.
type SeamInfo struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	TurnSeq     int    `json:"turn_seq,omitempty"`
	HistoryLen  int    `json:"history_len,omitempty"`
	CommitShort string `json:"commit_short,omitempty"`
	Label       string `json:"label,omitempty"`
	CreatedAt   int64  `json:"created_at,omitempty"`
}
```

并在文件末尾(`NewStructuredResult` 后)加构造函数:

```go
// NewListSeams requests the recent seams for the current session (reply: seams).
// selector is exact seam_id (必修项 J: no N/turn_id selector).
func NewListSeams() ClientFrame { return ClientFrame{Type: "list_seams"} }

// NewRestoreTurn requests reverting main to the state captured by seamID.
// confirmedHead is the FULL main_head commit id the client observed at list
// time (D6: full id, not the short hash); the server rejects when the head has
// since changed (必修项 E). Empty is rejected by the server (D6: require
// non-empty).
func NewRestoreTurn(seamID, confirmedHead string) ClientFrame {
	return ClientFrame{Type: "restore_turn", ID: seamID, ConfirmedHead: confirmedHead}
}

// NewSeams replies with the recent seams + the current main_head. commitShort
// is the display short hash; head is the FULL commit id the client binds into
// the next restore_turn's ConfirmedHead (D6).
func NewSeams(items []SeamInfo, commitShort, head string) ServerFrame {
	return ServerFrame{Type: "seams", Seams: items, CommitShort: commitShort, Head: head}
}

// NewSeamRestored replies with the undo seam id (pointing at the PRE-revert
// main_head) and the post-revert head. commitShort is for display; head is the
// FULL post-revert commit id for the next binding (D6). text carries a
// human-readable summary for the TUI entry.
func NewSeamRestored(undoSeamID, commitShort, head, text string) ServerFrame {
	return ServerFrame{Type: "seam_restored", ID: undoSeamID, CommitShort: commitShort, Head: head, Text: text}
}
```

3d. 在 `ClientFrame` 类型 doc comment 的 frame-type 列表中加入:

```
//	list_seams          request the recent main seams for this session (reply: seams)
//	restore_turn        revert main to exact seam ID; ConfirmedHead = non-empty FULL list-time head (reply: seam_restored)
```

在 `ServerFrame` 类型 doc comment 中加入:

```
//	seams               Seams + CommitShort(display) + Head(full binding; reply to list_seams)
//	seam_restored       ID = undo seam id; CommitShort = display hash; Head = full post-revert binding; Text = summary
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/proto/ -v`
Expected: PASS(含全部 seam 帧测试 + 已有测试)

- [ ] **Step 5: 提交**

```bash
git add internal/proto/frame.go internal/proto/seam_frame_test.go
git commit -m "feat(proto): add SeamInfo + list_seams/restore_turn/seams/seam_restored frames (B2-RB1 I/J)"
```

---

## Task 7: Server wiring — VCS + RepoID + sealTurnBoundary + bootstrap

**Files:**
- Modify: `internal/api/http/server.go`
- Modify: `internal/bootstrap/bootstrap.go`
- Test: `internal/api/http/server_seam_test.go`(新建)

> 必修项 K(server_test.go RED/PASS)。Server 结构体加 `vcs` + `repoID`,`Config` 加 `VCS` + `RepoID`,`New` 装配。bootstrap 把 `vcsInstance` / `vcsRepoID` 传入。新加 `Server.sealTurnBoundary` / `Server.shortHead` helper 供 WS/SSE handler 调用。

- [ ] **Step 1: 写失败测试** — `internal/api/http/server_seam_test.go`(新建)

```go
// internal/api/http/server_seam_test.go
package http

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/x6nux/yanshi/internal/store"
	"github.com/x6nux/yanshi/internal/vcs"
)

// TestServer_StoresVCSAndRepoID verifies that New wires the VCS + repoID
// supplied via Config, so downstream handlers (ChatWS / Chat) can use them.
func TestServer_StoresVCSAndRepoID(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	st, err := store.Open(filepath.Join(base, "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	v := vcs.New(st, filepath.Join(base, "worktrees"))
	repoID, err := v.InitRepo(root)
	if err != nil {
		t.Fatalf("InitRepo: %v", err)
	}
	srv := New(Config{
		Store:  st,
		VCS:    v,
		RepoID: repoID,
	})
	if srv.vcs != v {
		t.Errorf("srv.vcs not wired")
	}
	if srv.repoID != repoID {
		t.Errorf("srv.repoID = %q, want %q", srv.repoID, repoID)
	}
}

// TestServer_SealTurnBoundary_NoopsWithoutVCS verifies the silent no-op path
// when VCS is unconfigured (the common test path for handlers built without
// a real VCS).
func TestServer_SealTurnBoundary_NoopsWithoutVCS(t *testing.T) {
	srv := New(Config{Store: nil, VCS: nil, RepoID: ""})
	// Must not panic / log.
	srv.sealTurnBoundary("s1", 0, 0, "pre-turn", "no-vcs")
}

// TestServer_ShortHead_NoVCS verifies shortHead returns "" when VCS is nil.
func TestServer_ShortHead_NoVCS(t *testing.T) {
	srv := New(Config{})
	if got := srv.shortHead(); got != "" {
		t.Errorf("shortHead() = %q, want empty", got)
	}
}

// TestServer_ShortHead_WithVCS verifies shortHead returns a non-empty short
// hash when VCS is configured.
func TestServer_ShortHead_WithVCS(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(base, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	v := vcs.New(st, filepath.Join(base, "worktrees"))
	repoID, err := v.InitRepo(root)
	if err != nil {
		t.Fatal(err)
	}
	srv := New(Config{Store: st, VCS: v, RepoID: repoID})
	if got := srv.shortHead(); len(got) < 8 {
		t.Errorf("shortHead() = %q (len %d), want >=8 chars", got, len(got))
	}
	if got, err := v.RepoMainHead(repoID); err != nil || srv.fullHead() != got {
		t.Errorf("fullHead() = %q, RepoMainHead=%q err=%v", srv.fullHead(), got, err)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/api/http/ -run 'TestServer_StoresVCSAndRepoID|TestServer_SealTurnBoundary|TestServer_ShortHead' -v`
Expected: 编译失败(`srv.vcs` / `srv.repoID` / `Config.VCS` / `Config.RepoID` / `srv.sealTurnBoundary` / `srv.shortHead` / `srv.fullHead` 未定义)

- [ ] **Step 3: 修改 `internal/api/http/server.go`**

3a. 在 `Config` struct 的 `Store *store.Store` 字段后插入:

```go
	// VCS + RepoID wire the autoVCS instance + repo id to the WS/SSE handlers
	// for seam lifecycle (B2-RB1). nil/"" disables seam features silently.
	VCS    *vcs.VCS
	RepoID string
```

3b. 在 `Server` struct 的 `store *store.Store` 字段后插入:

```go
	// vcs + repoID enable the seam lifecycle (B2-RB1). Nil/"" = disabled.
	vcs    *vcs.VCS
	repoID string
```

3c. 在 `New` 的 `s := &Server{...}` literal 中，紧接 `store: cfg.Store,` 插入:

```go
		vcs:    cfg.VCS,
		repoID: cfg.RepoID,
```

其余 route registration保持当前位置；不复制或重排 constructor。

3d. 加 `sealTurnBoundary` + `shortHead` helper(放文件末尾或 `compactionModel` 附近):

```go
// sealTurnBoundary flushes pending main-scope edits and inserts a seam row of
// the given kind. No-op when the server has no VCS / repo configured. Non-fatal:
// errors are logged to stderr but never break the turn (必修项 F: post-turn seam
// must fire on every path, so the helper itself cannot return an error that
// callers might skip).
//
// kind must be one of the vcs.SeamKind constants (passed as string to avoid the
// direct dep on vcs from this doc comment — callers pass vcs.SeamPreTurn etc.).
func (s *Server) sealTurnBoundary(sessionID string, turnSeq, historyLen int, kind, label string) {
	if s.vcs == nil || s.repoID == "" {
		return
	}
	if _, err := s.vcs.SealMainTurnSeam(s.repoID, sessionID, turnSeq, historyLen, vcs.SeamKind(kind), label); err != nil {
		fmt.Fprintf(os.Stderr, "yanshi: %s seam (%s) failed: %v\n", kind, label, err)
	}
}

// shortHead returns the first 8 hex of the current main_head, or "" when VCS
// is unconfigured / repo has no head. Used to populate ServerFrame.CommitShort
// for DISPLAY (必修项 E).
func (s *Server) shortHead() string {
	if s.vcs == nil || s.repoID == "" {
		return ""
	}
	head, err := s.vcs.RepoMainHead(s.repoID)
	if err != nil || head == "" {
		return ""
	}
	if len(head) > 8 {
		return head[:8]
	}
	return head
}

// fullHead returns the FULL current main_head commit id, or "" when VCS is
// unconfigured / repo has no head. Used to populate ServerFrame.Head for target
// BINDING — the restore_turn handler compares the client's ConfirmedHead against
// this FULL id (D6: short-hash comparison risks collision across long
// histories). Read-only; does NOT take the repo lock (DB reads are serialized
// by SetMaxOpenConns(1)).
func (s *Server) fullHead() string {
	if s.vcs == nil || s.repoID == "" {
		return ""
	}
	head, err := s.vcs.RepoMainHead(s.repoID)
	if err != nil {
		return ""
	}
	return head
}
```

3e. server.go 顶部加 import(若未存在)；`sealTurnBoundary` 同时使用 `fmt.Fprintf` 与 `os.Stderr`，因此两个标准库 import 都不可省略：

```go
"fmt"
"os"
"github.com/x6nux/yanshi/internal/vcs"
```

- [ ] **Step 4: 修改 `internal/bootstrap/bootstrap.go`**

找到 `bootstrap.Build` 中唯一的 `apihttp.New(apihttp.Config{...})` literal(已读取确认存在)，在其顶层 `Store: st,` 后插入以下两个确切字段；不要改动嵌套 `Compaction` literal:

```go
		VCS:    vcsInstance,
		RepoID: vcsRepoID,
```

> 注:`vcsInstance` 与 `vcsRepoID` 已在 `bootstrap.Build` 中存在(已读确认)。VCS 工具通过现有的 `for _, t := range tools.NewVCSTools().Tools()` 循环自动注册,Task 11 把 `t.Revert` 加进 `Tools()` 后无需再改 bootstrap。

- [ ] **Step 5: 跑测试确认通过**

Run: `go test ./internal/api/http/ -run 'TestServer_StoresVCSAndRepoID|TestServer_SealTurnBoundary|TestServer_ShortHead' -v`
Expected: PASS

Run: `go test ./internal/bootstrap/ -v`
Expected: PASS(bootstrap 装配测试无破坏)

- [ ] **Step 6: 提交**

```bash
git add internal/api/http/server.go internal/api/http/server_seam_test.go internal/bootstrap/bootstrap.go
git commit -m "feat(api/http): wire VCS+RepoID + sealTurnBoundary/shortHead helpers (B2-RB1)"
```

---
## Task 8: Store — transactional session truncation + durable undo snapshot

**Files:**
- Modify: `internal/store/session.go`(在 Task 1 已有 snapshot type/codec 上加 `SnapshotSessionForRevert` / `TruncateSessionForRevert` / `RestoreSessionAfterFailedRevert`)
- Test: `internal/store/seam_truncate_test.go`(新建)

> 必修项 K + D2 + D5。WS 回滚必须先把 persisted messages 与 session.turns 在**一个 SQLite tx**中截断,成功后才动内存/VCS。方法同时返回截断前完整 snapshot:VCS 把其 JSON blob 与 pre-revert undo seam 放在同一 SQLite tx 中,所以断线重连后仍能恢复被删除的历史。若随后 VCS 回滚失败,handler 用 snapshot 以第二个 tx 精确补偿。截断 tx 的任一读/DELETE/UPDATE/Commit 失败均返回 error且 tx rollback,上层按 fatal 处理,绝不能 best-effort 忽略。

- [ ] **Step 1: 写失败测试** — `internal/store/seam_truncate_test.go`(新建)

```go
// internal/store/seam_truncate_test.go
package store

import (
	"encoding/json"
	"fmt"
	"testing"
)

func seedRevertSession(t *testing.T) (*Store, string) {
	t.Helper()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	sid, err := s.CreateSession("truncate test")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	for i := 0; i < 5; i++ {
		if err := s.AppendMessage(sid, i, "user", fmt.Sprintf("msg-%d", i)); err != nil {
			t.Fatalf("AppendMessage[%d]: %v", i, err)
		}
	}
	if err := s.UpdateSessionMeta(sid, "fake", "off", 10, 20, 4, 2, 3); err != nil {
		t.Fatalf("UpdateSessionMeta: %v", err)
	}
	return s, sid
}

func TestTruncateSessionForRevert_IsAtomicAndReturnsSnapshot(t *testing.T) {
	s, sid := seedRevertSession(t)
	snap, err := s.TruncateSessionForRevert(sid, 3, 1)
	if err != nil {
		t.Fatalf("TruncateSessionForRevert: %v", err)
	}
	if len(snap.Messages) != 5 || snap.Meta.ID != sid || snap.Meta.Turns != 4 {
		t.Fatalf("snapshot = %+v / %d messages; want original turns=4/messages=5",
			snap.Meta, len(snap.Messages))
	}
	msgs, err := s.Messages(sid)
	if err != nil || len(msgs) != 3 {
		t.Fatalf("persisted messages after truncate = %d, err=%v; want 3", len(msgs), err)
	}
	meta, err := s.GetSession(sid)
	if err != nil || meta == nil || meta.Turns != 1 {
		t.Fatalf("meta after truncate = %+v, err=%v; want turns=1", meta, err)
	}
}

func TestSessionRevertSnapshot_JSONRoundTripForUndo(t *testing.T) {
	s, sid := seedRevertSession(t)
	snap, err := s.SnapshotSessionForRevert(sid)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := EncodeSessionRevertSnapshot(snap)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(blob) {
		t.Fatal("encoded undo snapshot is not valid JSON")
	}
	got, err := DecodeSessionRevertSnapshot(blob)
	if err != nil {
		t.Fatal(err)
	}
	if got.Meta.ID != sid || got.Meta.Turns != 4 || len(got.Messages) != 5 {
		t.Fatalf("decoded snapshot = %+v/%d messages", got.Meta, len(got.Messages))
	}
}

func TestRestoreSessionAfterFailedRevert_RestoresMessagesAndMeta(t *testing.T) {
	s, sid := seedRevertSession(t)
	snap, err := s.TruncateSessionForRevert(sid, 2, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RestoreSessionAfterFailedRevert(snap); err != nil {
		t.Fatalf("RestoreSessionAfterFailedRevert: %v", err)
	}
	msgs, err := s.Messages(sid)
	if err != nil || len(msgs) != 5 {
		t.Fatalf("restored messages = %d, err=%v; want 5", len(msgs), err)
	}
	for i, msg := range msgs {
		if msg.ID != snap.Messages[i].ID || msg.Seq != i ||
			msg.Content != fmt.Sprintf("msg-%d", i) {
			t.Fatalf("message[%d] not exactly restored: %+v", i, msg)
		}
	}
	meta, err := s.GetSession(sid)
	if err != nil || meta == nil || meta.Turns != 4 || meta.UpdatedAt != snap.Meta.UpdatedAt {
		t.Fatalf("restored meta = %+v, err=%v; want snapshot %+v", meta, err, snap.Meta)
	}
}

func TestTruncateSessionForRevert_DeleteFailureRollsBackMetaToo(t *testing.T) {
	s, sid := seedRevertSession(t)
	// Deterministic failure injection inside the truncation tx.
	trigger := fmt.Sprintf(`
		CREATE TRIGGER fail_history_truncate
		BEFORE DELETE ON messages
		WHEN OLD.session_id = '%s' AND OLD.seq >= 3
		BEGIN SELECT RAISE(ABORT, 'injected truncate failure'); END;
	`, sid)
	if _, err := s.DB.Exec(trigger); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	if _, err := s.TruncateSessionForRevert(sid, 3, 1); err == nil {
		t.Fatal("expected injected truncation failure")
	}
	msgs, err := s.Messages(sid)
	if err != nil || len(msgs) != 5 {
		t.Fatalf("messages changed after failed tx: %d, err=%v", len(msgs), err)
	}
	meta, err := s.GetSession(sid)
	if err != nil || meta == nil || meta.Turns != 4 {
		t.Fatalf("meta changed after failed tx: %+v, err=%v", meta, err)
	}
}

func TestTruncateSessionForRevert_RejectsMissingSession(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.TruncateSessionForRevert("missing", 0, 0); err == nil {
		t.Fatal("missing session must be an error, not a silent no-op")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/store/ -run 'TestTruncateSessionForRevert|TestRestoreSessionAfterFailedRevert|TestSessionRevertSnapshot' -v`
Expected: 编译失败(`SnapshotSessionForRevert` / `TruncateSessionForRevert` / `RestoreSessionAfterFailedRevert` transactional APIs 未定义；`SessionRevertSnapshot` 与 codec 已由 Task 1 定义，不能在本 Task 重复声明)。

- [ ] **Step 3: 在 `internal/store/session.go` 加 transactional APIs**

在现有 import block 加 `"database/sql"`；`"encoding/json"` 与 `"fmt"` 已由 Task 1 为 codec 加入。然后紧接 Task 1 的 `DecodeSessionRevertSnapshot` 后添加以下代码，**不要重复定义 snapshot type 或 codec**:

```go
// snapshotSessionTx captures one exact session row and all of its messages.
// Both read-only snapshot and truncate reuse it; do not duplicate this query.
func snapshotSessionTx(tx *sql.Tx, sessionID string) (SessionRevertSnapshot, error) {
	var snap SessionRevertSnapshot
	m := &snap.Meta
	if err := tx.QueryRow(
		`SELECT id, title, created_at, updated_at, model, thinking,
		        tokens_in, tokens_out, turns, cached_tokens,
		        reasoning_tokens, archived
		 FROM sessions WHERE id=?`, sessionID,
	).Scan(&m.ID, &m.Title, &m.CreatedAt, &m.UpdatedAt, &m.Model,
		&m.Thinking, &m.TokensIn, &m.TokensOut, &m.Turns,
		&m.CachedTokens, &m.ReasoningTokens, &m.Archived); err != nil {
		return SessionRevertSnapshot{}, fmt.Errorf("store: snapshot session: %w", err)
	}
	rows, err := tx.Query(
		`SELECT id, session_id, seq, role, content, created_at
		 FROM messages WHERE session_id=? ORDER BY seq ASC`, sessionID)
	if err != nil {
		return SessionRevertSnapshot{}, fmt.Errorf("store: snapshot messages: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var msg Message
		if err := rows.Scan(&msg.ID, &msg.SessionID, &msg.Seq, &msg.Role,
			&msg.Content, &msg.CreatedAt); err != nil {
			return SessionRevertSnapshot{}, fmt.Errorf("store: scan snapshot message: %w", err)
		}
		snap.Messages = append(snap.Messages, msg)
	}
	if err := rows.Err(); err != nil {
		return SessionRevertSnapshot{}, fmt.Errorf("store: iterate snapshot messages: %w", err)
	}
	return snap, nil
}

// SnapshotSessionForRevert returns an exact durable snapshot without mutation.
// The undo path uses it as compensation state before expanding older history.
func (s *Store) SnapshotSessionForRevert(sessionID string) (SessionRevertSnapshot, error) {
	if sessionID == "" {
		return SessionRevertSnapshot{}, fmt.Errorf("store: empty session id")
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return SessionRevertSnapshot{}, err
	}
	defer tx.Rollback()
	snap, err := snapshotSessionTx(tx, sessionID)
	if err != nil {
		return SessionRevertSnapshot{}, err
	}
	if err := tx.Commit(); err != nil {
		return SessionRevertSnapshot{}, fmt.Errorf("store: commit session snapshot read: %w", err)
	}
	return snap, nil
}

// TruncateSessionForRevert atomically snapshots a session, deletes messages
// with seq >= fromSeq, and updates turns. Any failure rolls back both changes.
func (s *Store) TruncateSessionForRevert(
	sessionID string, fromSeq, turns int,
) (SessionRevertSnapshot, error) {
	if sessionID == "" || fromSeq < 0 || turns < 0 {
		return SessionRevertSnapshot{}, fmt.Errorf(
			"store: invalid revert boundary session=%q seq=%d turns=%d",
			sessionID, fromSeq, turns)
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return SessionRevertSnapshot{}, err
	}
	defer tx.Rollback()

	snap, err := snapshotSessionTx(tx, sessionID)
	if err != nil {
		return SessionRevertSnapshot{}, err
	}
	if _, err := tx.Exec(
		"DELETE FROM messages WHERE session_id=? AND seq>=?", sessionID, fromSeq,
	); err != nil {
		return SessionRevertSnapshot{}, fmt.Errorf("store: truncate messages: %w", err)
	}
	res, err := tx.Exec(
		"UPDATE sessions SET turns=?, updated_at=? WHERE id=?",
		turns, time.Now().Unix(), sessionID)
	if err != nil {
		return SessionRevertSnapshot{}, fmt.Errorf("store: update truncated meta: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return SessionRevertSnapshot{}, fmt.Errorf("store: update truncated meta rows: %w", err)
	}
	if n != 1 {
		return SessionRevertSnapshot{}, fmt.Errorf(
			"store: update truncated meta affected %d rows", n)
	}
	if err := tx.Commit(); err != nil {
		return SessionRevertSnapshot{}, fmt.Errorf("store: commit history truncation: %w", err)
	}
	return snap, nil
}

// RestoreSessionAfterFailedRevert atomically replaces the session row metadata
// and message set with an exact snapshot. The handler uses it both to compensate
// a failed VCS phase and to expand durable history when restoring a pre-revert
// undo seam. Any failure is fatal and surfaced to the client.
func (s *Store) RestoreSessionAfterFailedRevert(snap SessionRevertSnapshot) error {
	if snap.Meta.ID == "" {
		return fmt.Errorf("store: empty session compensation snapshot")
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM messages WHERE session_id=?", snap.Meta.ID); err != nil {
		return fmt.Errorf("store: clear truncated messages: %w", err)
	}
	for _, msg := range snap.Messages {
		if msg.SessionID != snap.Meta.ID {
			return fmt.Errorf("store: snapshot message %s belongs to %s", msg.ID, msg.SessionID)
		}
		if _, err := tx.Exec(
			`INSERT INTO messages (id, session_id, seq, role, content, created_at)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			msg.ID, msg.SessionID, msg.Seq, msg.Role, msg.Content, msg.CreatedAt,
		); err != nil {
			return fmt.Errorf("store: restore message %s: %w", msg.ID, err)
		}
	}
	m := snap.Meta
	res, err := tx.Exec(
		`UPDATE sessions SET title=?, created_at=?, updated_at=?, model=?, thinking=?,
		 tokens_in=?, tokens_out=?, turns=?, cached_tokens=?, reasoning_tokens=?, archived=?
		 WHERE id=?`,
		m.Title, m.CreatedAt, m.UpdatedAt, m.Model, m.Thinking, m.TokensIn,
		m.TokensOut, m.Turns, m.CachedTokens, m.ReasoningTokens, m.Archived, m.ID)
	if err != nil {
		return fmt.Errorf("store: restore session meta: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: restore session meta rows: %w", err)
	}
	if n != 1 {
		return fmt.Errorf("store: restore session meta affected %d rows", n)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit session compensation: %w", err)
	}
	return nil
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/store/ -run 'TestTruncateSessionForRevert|TestRestoreSessionAfterFailedRevert|TestSessionRevertSnapshot' -v`
Expected: PASS,包括 DELETE trigger 注入失败后 messages/meta 均未变化、snapshot JSON round-trip,以及补偿/undo expansion 精确恢复。

- [ ] **Step 5: 提交**

```bash
git add internal/store/session.go internal/store/seam_truncate_test.go
git commit -m "feat(store): add atomic session truncation and revert compensation"
```

---

## Task 9: WS — runUserTurn + reader bypass + pre/post seam + restore_turn handler

**Files:**
- Modify: `internal/api/http/ws.go`(重构 `case "user_message"` + 加 `case "list_seams"` / `case "restore_turn"` + reader bypass)
- Create: `internal/api/http/ws_seam.go`（seam 相关 handler 拆出）
- Test: `internal/api/http/ws_seam_test.go`(新建)

> 必修项 C + F + G + I。这是 RB1 最大的一个 Task。关键变更:
>
> 1. `connSession` 加 `inTurn atomic.Bool` 值字段(不能用裸 `bool`或另一个 mutex,reader goroutine 与 main loop 并发读写);加 `setInTurn` / `isInTurn` 方法。
> 2. reader goroutine 加 `list_seams` / `restore_turn` bypass:`if cs.isInTurn() { conn.write(errorFrame); continue }`,否则 fall through 到 frames channel。
> 3. 抽 `runUserTurn` 闭包(在 ChatWS 内):入口先 `cs.setInTurn(true)` 并 defer reset,紧接着创建 turn context 并按注册顺序 `defer release()`、`defer postTurnSeam()`；Go LIFO 因而保证退出顺序严格为 post-turn seam → release → `inTurn=false`。仅把目标是重进外层 dispatch loop 的 `continue` 改成 `return`;保留 schema-retry attempt loop 的内层 `continue`。
> 4. pre-turn seam 在 `cs.history = append(... user_msg)` 之后、runner/compaction 之前调用；turn context 已在 closure 入口建立,确保连 `resolveQuery` early return 也走完整 finalizer 链。
> 5. 加 `case "list_seams"`(调 `handleListSeams`)与 `case "restore_turn"`(调 `handleRestoreTurn`)。
> 6. `handleListSeams` / `handleRestoreTurn` 拆到新文件 `ws_seam.go`(避免 `ws.go` 突破 1000 纯代码行上限)。
> 7. `handleRestoreTurn`:full `confirmed_head` 校验;普通 seam 先 durable truncate,pre-revert undo seam 先从 `history_snapshot` 恢复 durable history;随后更新 memory并调用 `vcs.RevertToSeam`。每次新 undo seam 原子保存本次操作前 snapshot;VCS 失败则精确补偿 durable+memory;成功回复 `NewSeamRestored`。
>
> 测试覆盖正常 turn 前后 seam、cancel-mid-turn 仍有 post seam、model error 仍有 post seam、disconnect 仍有 post seam、`list_seams` while in-turn 被拒、`restore_turn` happy path + history 截断。

- [ ] **Step 1: 写失败测试** — `internal/api/http/ws_seam_test.go`(新建)

```go
// internal/api/http/ws_seam_test.go
package http

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	"github.com/x6nux/yanshi/internal/guard"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/store"
	"github.com/x6nux/yanshi/internal/tools"
	"github.com/x6nux/yanshi/internal/vcs"
)

// newFakeOrchestrator builds a real orchestrator over a deterministic FakeModel,
// matching the pattern in ws_test.go (NOT a nil orchestrator — chat.go panics on
// nil). Used by setupSeamServer and the in-turn rejection test.
func newFakeOrchestrator(t *testing.T) *orchestrator.Orchestrator {
	t.Helper()
	o, err := orchestrator.New(orchestrator.Config{
		Model: einollm.NewFakeModel([]string{"ok"}, nil),
	})
	require.NoError(t, err)
	return o
}

// newBlockingOrchestrator uses the repository's real BlockingModel test fake.
// Started removes timing sleeps; Block controls deterministic completion.
func newBlockingOrchestrator(t *testing.T) (
	*orchestrator.Orchestrator, *einollm.BlockingModel,
) {
	t.Helper()
	blocking := einollm.NewBlockingModel("ok")
	o, err := orchestrator.New(orchestrator.Config{Model: blocking})
	require.NoError(t, err)
	return o, blocking
}

// setupSeamServer boots a real httptest server with a real VCS + repo + Fake
// orchestrator, wired exactly the way bootstrap wires a production server
// (New → ChatWS → httptest.NewServer(Handler())). Returns the dialable WS URL
// plus the VCS/repo for direct seam inspection. This is the complete CB6
// server bootstrap used by every test in this file.
func setupSeamServer(t *testing.T) (baseURL string, v *vcs.VCS, repoID string) {
	t.Helper()
	baseURL, v, repoID, _ = setupSeamServerFull(t)
	return baseURL, v, repoID
}

func setupSeamServerFull(t *testing.T) (
	baseURL string, v *vcs.VCS, repoID string, st *store.Store,
) {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	require.NoError(t, os.MkdirAll(root, 0o755))
	var err error
	st, err = store.Open(filepath.Join(base, "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })
	v = vcs.New(st, filepath.Join(base, "worktrees"))
	repoID, err = v.InitRepo(root)
	require.NoError(t, err)
	o := newFakeOrchestrator(t)
	s := New(Config{Store: st, VCS: v, RepoID: repoID})
	s.ChatWS(o, nil, nil)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return "ws" + ts.URL[len("http"):] + "/api/v1/chat/ws", v, repoID, st
}

// setupSeamServerWithOrchestrator installs a caller-supplied orchestrator and
// returns the real VCS/store so cancel and disconnect tests can inspect the
// concrete session's seams without an all-session query.
func setupSeamServerWithOrchestrator(t *testing.T, o *orchestrator.Orchestrator) (
	string, *vcs.VCS, string, *store.Store,
) {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	require.NoError(t, os.MkdirAll(root, 0o755))
	st, err := store.Open(filepath.Join(base, "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })
	v := vcs.New(st, filepath.Join(base, "worktrees"))
	repoID, err := v.InitRepo(root)
	require.NoError(t, err)
	s := New(Config{Store: st, VCS: v, RepoID: repoID})
	s.ChatWS(o, nil, nil)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return "ws" + ts.URL[len("http"):] + "/api/v1/chat/ws", v, repoID, st
}

// drainTurn reads frames until a "done" frame arrives (one full turn).
func drainTurn(t *testing.T, c *websocket.Conn) {
	t.Helper()
	for {
		var sf proto.ServerFrame
		require.NoError(t, c.ReadJSON(&sf))
		if sf.Type == "done" {
			return
		}
	}
}

// waitForSessionSeams obtains the concrete durable session id and polls the
// session-scoped list. Tests never use ListSeams(repoID, "", ...), because an
// empty filter would hide D7 regressions by admitting other sessions' seams.
func waitForSessionSeams(t *testing.T, st *store.Store, v *vcs.VCS,
	repoID string, want int,
) (string, []vcs.Seam) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		sessions, err := st.ListSessions(0)
		require.NoError(t, err)
		if len(sessions) == 1 {
			seams, err := v.ListSeams(repoID, sessions[0].ID, 0)
			require.NoError(t, err)
			if len(seams) >= want {
				return sessions[0].ID, seams
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for session-scoped seams")
	return "", nil
}

// TestWS_PreAndPostTurnSeamsCreated verifies that a single user_message turn
// creates exactly two seams (pre-turn + post-turn) for that session.
func TestWS_PreAndPostTurnSeamsCreated(t *testing.T) {
	url, v, repoID, st := setupSeamServerFull(t)
	c := dial(t, url)
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewUserMessage("hello")))
	drainTurn(t, c)

	// Resolve the exact durable session id, then assert that session's two seams.
	// This directly exercises D7 rather than using an empty all-session filter.
	_, seams := waitForSessionSeams(t, st, v, repoID, 2)
	if len(seams) < 2 {
		t.Errorf("expected >=2 session seams after first turn, got %d", len(seams))
	}
}

// TestWS_ListSeamsWhileInTurnRejected verifies the reader-goroutine bypass:
// a list_seams frame arriving during a turn is rejected immediately (not
// queued for post-turn processing). Uses a BlockingModel to keep the turn
// running until we release it.
func TestWS_ListSeamsWhileInTurnRejected(t *testing.T) {
	o, blocking := newBlockingOrchestrator(t)
	defer close(blocking.Block)
	url, _, _, _ := setupSeamServerWithOrchestrator(t, o)
	c := dial(t, url)
	defer c.Close()

	// Send user_message and wait for the real fake's Started signal — no sleep race.
	require.NoError(t, c.WriteJSON(proto.NewUserMessage("thinking...")))
	select {
	case <-blocking.Started:
	case <-time.After(2 * time.Second):
		t.Fatal("blocking model did not start")
	}
	require.NoError(t, c.WriteJSON(proto.NewListSeams()))

	gotReject := false
	for !gotReject {
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		var sf proto.ServerFrame
		if err := c.ReadJSON(&sf); err != nil {
			t.Fatalf("read: %v", err)
		}
		if sf.Type == "error" && contains(sf.Text, "while a turn is running") {
			gotReject = true
		}
		if sf.Type == "done" {
			t.Fatal("got done before the reject")
		}
	}
	// Defer closes blocking.Block after assertions; disconnect cancellation also
	// exercises runUserTurn's release/post-seam cleanup chain.
}

// TestWS_PostTurnSeamFiresOnCancel verifies the post-turn seam defer fires
// even when the user cancels mid-turn (必修项 F).
func TestWS_PostTurnSeamFiresOnCancel(t *testing.T) {
	o, blocking := newBlockingOrchestrator(t)
	t.Cleanup(func() { close(blocking.Block) })
	url, v, repoID, st := setupSeamServerWithOrchestrator(t, o)
	c := dial(t, url)
	defer c.Close()
	require.NoError(t, c.WriteJSON(proto.NewUserMessage("long")))
	select {
	case <-blocking.Started:
	case <-time.After(2 * time.Second):
		t.Fatal("blocking model did not start")
	}
	require.NoError(t, c.WriteJSON(proto.NewCancel()))
	drainTurn(t, c)
	_, seams := waitForSessionSeams(t, st, v, repoID, 2)
	if len(seams) < 2 {
		t.Errorf("post-turn seam should still fire on cancel; got %d seams", len(seams))
	}
}

// TestWS_PostTurnSeamFiresOnDisconnect proves the same defer chain runs when
// the transport disappears mid-model call. BlockingModel makes the in-flight
// state deterministic; closing the socket is the only event that ends the turn.
func TestWS_PostTurnSeamFiresOnDisconnect(t *testing.T) {
	o, blocking := newBlockingOrchestrator(t)
	t.Cleanup(func() { close(blocking.Block) })
	url, v, repoID, st := setupSeamServerWithOrchestrator(t, o)
	c := dial(t, url)
	require.NoError(t, c.WriteJSON(proto.NewUserMessage("disconnect")))
	select {
	case <-blocking.Started:
	case <-time.After(2 * time.Second):
		t.Fatal("blocking model did not start")
	}
	require.NoError(t, c.Close())
	_, seams := waitForSessionSeams(t, st, v, repoID, 2)
	if len(seams) < 2 {
		t.Errorf("post-turn seam should still fire on disconnect; got %d seams", len(seams))
	}
}

// TestWS_PostTurnSeamFiresOnModelError verifies the post-turn seam defer fires
// when the turn emits an error frame (必修项 F).
func TestWS_PostTurnSeamFiresOnModelError(t *testing.T) {
	// Build an orchestrator whose FakeModel always errors.
	o, err := orchestrator.New(orchestrator.Config{
		Model: einollm.NewFakeModel(nil, context.DeadlineExceeded),
	})
	require.NoError(t, err)
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	require.NoError(t, os.MkdirAll(root, 0o755))
	st, err := store.Open(filepath.Join(base, "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })
	v := vcs.New(st, filepath.Join(base, "worktrees"))
	repoID, err := v.InitRepo(root)
	require.NoError(t, err)
	s := New(Config{Store: st, VCS: v, RepoID: repoID})
	s.ChatWS(o, nil, nil)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	url := "ws" + ts.URL[len("http"):] + "/api/v1/chat/ws"
	c := dial(t, url)
	defer c.Close()
	require.NoError(t, c.WriteJSON(proto.NewUserMessage("trigger error")))
	drainTurn(t, c)
	_, seams := waitForSessionSeams(t, st, v, repoID, 2)
	if len(seams) < 2 {
		t.Errorf("post-turn seam should fire on model error; got %d seams", len(seams))
	}
}

// TestWS_RestoreTurn_HappyPath verifies the full restore_turn round-trip:
// list → confirm(full head) → revert; the undo seam is pre-revert.
func TestWS_RestoreTurn_HappyPath(t *testing.T) {
	url, v, repoID := setupSeamServer(t)
	c := dial(t, url)
	defer c.Close()
	require.NoError(t, c.WriteJSON(proto.NewUserMessage("first")))
	drainTurn(t, c)
	// List seams (handleListSeams always passes cs.sessionID — no cross-session
	// leak, 必修项 J/D7).
	require.NoError(t, c.WriteJSON(proto.NewListSeams()))
	var seamsFrame proto.ServerFrame
	for seamsFrame.Type != "seams" {
		require.NoError(t, c.ReadJSON(&seamsFrame))
	}
	if len(seamsFrame.Seams) == 0 {
		t.Fatal("no seams returned")
	}
	// confirmedHead is the FULL commit id (D6), not the display short hash.
	confirmedHead := seamsFrame.Head
	if confirmedHead == "" {
		t.Fatal("seams frame missing full Head binding")
	}
	pre1 := seamsFrame.Seams[len(seamsFrame.Seams)-1] // oldest = pre-turn:1

	require.NoError(t, c.WriteJSON(proto.NewRestoreTurn(pre1.ID, confirmedHead)))
	var restored proto.ServerFrame
	for restored.Type != "seam_restored" {
		require.NoError(t, c.ReadJSON(&restored))
		if restored.Type == "error" {
			t.Fatalf("restore_turn error: %s", restored.Text)
		}
	}
	if restored.ID == "" {
		t.Error("undo seam id (seam_restored.ID) is empty")
	}
	undoSeam, err := v.FindSeam(restored.ID)
	require.NoError(t, err)
	if undoSeam.Kind != vcs.SeamPreRevert {
		t.Errorf("undo seam.Kind = %q, want %q", undoSeam.Kind, vcs.SeamPreRevert)
	}
}

// TestWS_RestoreTurn_EmptyOrMismatchedHeadRejected verifies the server REQUIRES
// a non-empty confirmed_head that matches the current full main_head (必修项 D6).
func TestWS_RestoreTurn_EmptyOrMismatchedHeadRejected(t *testing.T) {
	url, _, _ := setupSeamServer(t)
	c := dial(t, url)
	defer c.Close()
	require.NoError(t, c.WriteJSON(proto.NewUserMessage("seed")))
	drainTurn(t, c)
	// Empty head → error.
	require.NoError(t, c.WriteJSON(proto.NewRestoreTurn("some-seam", "")))
	var sf proto.ServerFrame
	require.NoError(t, c.ReadJSON(&sf))
	if sf.Type != "error" {
		t.Errorf("empty confirmed_head: got %q, want error", sf.Type)
	}
	// Mismatched head → error.
	require.NoError(t, c.WriteJSON(proto.NewRestoreTurn("some-seam", "deadbeef-not-the-real-head")))
	require.NoError(t, c.ReadJSON(&sf))
	if sf.Type != "error" {
		t.Errorf("mismatched confirmed_head: got %q, want error", sf.Type)
	}
}

// TestWS_RestoreTurn_PersistFailureIsFatalBeforeVCS proves D5 ordering: an
// injected DELETE failure leaves durable history, memory-visible head, and VCS
// unchanged, and no seam_restored success frame is emitted.
func TestWS_RestoreTurn_PersistFailureIsFatalBeforeVCS(t *testing.T) {
	url, v, repoID, st := setupSeamServerFull(t)
	c := dial(t, url)
	defer c.Close()
	require.NoError(t, c.WriteJSON(proto.NewUserMessage("first")))
	drainTurn(t, c)
	require.NoError(t, c.WriteJSON(proto.NewListSeams()))
	var listed proto.ServerFrame
	for listed.Type != "seams" {
		require.NoError(t, c.ReadJSON(&listed))
	}
	require.NotEmpty(t, listed.Seams)
	target := listed.Seams[len(listed.Seams)-1]
	headBefore, err := v.RepoMainHead(repoID)
	require.NoError(t, err)
	sessions, err := st.ListSessions(0)
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	sid := sessions[0].ID
	messagesBefore, err := st.Messages(sid)
	require.NoError(t, err)
	trigger := fmt.Sprintf(`
		CREATE TRIGGER fail_ws_history_truncate
		BEFORE DELETE ON messages
		WHEN OLD.session_id = '%s'
		BEGIN SELECT RAISE(ABORT, 'injected ws truncate failure'); END;
	`, sid)
	_, err = st.DB.Exec(trigger)
	require.NoError(t, err)

	require.NoError(t, c.WriteJSON(proto.NewRestoreTurn(target.ID, listed.Head)))
	sawFatal, sawRestored := false, false
	for {
		var sf proto.ServerFrame
		require.NoError(t, c.ReadJSON(&sf))
		if sf.Type == "error" && contains(sf.Text, "FATAL: durable history truncation failed") {
			sawFatal = true
		}
		if sf.Type == "seam_restored" {
			sawRestored = true
		}
		if sf.Type == "done" {
			break
		}
	}
	if !sawFatal || sawRestored {
		t.Fatalf("fatal=%v seam_restored=%v; want true/false", sawFatal, sawRestored)
	}
	gotHead, err := v.RepoMainHead(repoID)
	require.NoError(t, err)
	if gotHead != headBefore {
		t.Fatalf("VCS head changed despite durable failure: got %s want %s", gotHead, headBefore)
	}
	messagesAfter, err := st.Messages(sid)
	require.NoError(t, err)
	if len(messagesAfter) != len(messagesBefore) {
		t.Fatalf("durable history changed despite failed tx: before=%d after=%d",
			len(messagesBefore), len(messagesAfter))
	}
}

// TestWS_RestoreTurn_CrossSessionRejected proves seam ids are not repo-global
// capabilities: c2 cannot restore a seam sealed by c1 even with the current head.
func TestWS_RestoreTurn_CrossSessionRejected(t *testing.T) {
	url, _, _ := setupSeamServer(t)
	c1 := dial(t, url)
	defer c1.Close()
	c2 := dial(t, url)
	defer c2.Close()

	require.NoError(t, c1.WriteJSON(proto.NewUserMessage("c1")))
	drainTurn(t, c1)
	require.NoError(t, c1.WriteJSON(proto.NewListSeams()))
	var c1List proto.ServerFrame
	for c1List.Type != "seams" {
		require.NoError(t, c1.ReadJSON(&c1List))
	}
	require.NotEmpty(t, c1List.Seams)
	foreignID := c1List.Seams[0].ID

	require.NoError(t, c2.WriteJSON(proto.NewUserMessage("c2")))
	drainTurn(t, c2)
	require.NoError(t, c2.WriteJSON(proto.NewListSeams()))
	var c2List proto.ServerFrame
	for c2List.Type != "seams" {
		require.NoError(t, c2.ReadJSON(&c2List))
	}
	require.NoError(t, c2.WriteJSON(proto.NewRestoreTurn(foreignID, c2List.Head)))
	var sf proto.ServerFrame
	require.NoError(t, c2.ReadJSON(&sf))
	if sf.Type != "error" || !contains(sf.Text, "does not belong to this session") {
		t.Fatalf("cross-session restore = type %q text %q; want session error", sf.Type, sf.Text)
	}
}

// TestResolvePermissionRequest_ForceNeverAutoResolves proves D3 at the exact
// WS mode gate: a Force request is unresolved under every auto-allowing mode,
// so the callback must fall through to permission_request + explicit response.
func TestResolvePermissionRequest_ForceNeverAutoResolves(t *testing.T) {
	for _, mode := range []guard.PermissionMode{
		guard.ModeYOLO,
		guard.ModeAllowEdits,
		guard.ModeAuto,
	} {
		t.Run(string(mode), func(t *testing.T) {
			cs := connSession{perm: &permModeState{}}
			cs.perm.set(mode, 10) // auto threshold would otherwise allow any scored request
			decision, resolved := resolvePermissionRequest(
				context.Background(), cs, nil,
				tools.PermissionRequest{Tool: "fs_write", Force: true},
			)
			if resolved {
				t.Fatalf("Force request auto-resolved in mode %q with decision %v", mode, decision)
			}
		})
	}
}

// contains is a tiny substring helper to avoid pulling strings into the test.
func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// drainJSON exists for future tests that decode via a stream reader.
func drainJSON(t *testing.T, dec *json.Decoder) {
	t.Helper()
	for {
		var sf proto.ServerFrame
		if err := dec.Decode(&sf); err != nil {
			return
		}
		if sf.Type == "done" {
			return
		}
	}
}
```

> 注:测试使用真实 `New(Config{...})` → `ChatWS(o,nil,nil)` → `httptest.NewServer(s.Handler())`(CB6)。普通路径用 `orchestrator.New` + `einollm.NewFakeModel`;in-turn 路径使用仓库真实 `einollm.NewBlockingModel("ok")` 及其 `Started`/`Block` channels(签名已按 `internal/llm/eino/blocking.go` 核实),不传 nil orchestrator,也不引用不存在的 overload。`dial` 复用 `ws_test.go` 现有 helper。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/api/http/ -run 'TestWS_PreAndPostTurnSeamsCreated|TestWS_ListSeamsWhileInTurnRejected|TestWS_PostTurnSeamFires|TestWS_RestoreTurn|TestResolvePermissionRequest_Force' -v`
Expected: 编译失败(`cs.inTurn` / seam handlers / transactional store APIs / `resolvePermissionRequest` 未定义);旧 handler也无法通过 durable DELETE failure 保持 VCS head不变及 cross-session reject断言。

- [ ] **Step 3: 修改 `internal/api/http/ws.go`**

3a. 在 `connSession` struct 加 `inTurn`(放在 `seq int` 后)。**用 `atomic.Bool`,不要用 mutex+bool**(CB3:reader goroutine 与主循环并发读,且 reader 在持有锁时不能调用 `conn.write`——那会与 `wsConn.write` 的互斥形成锁序问题):

```go
	// inTurn is true while a user_message turn is running on the main loop,
	// false otherwise. The reader goroutine Load()s it to reject list_seams /
	// restore_turn frames INLINE (without this gate they would queue behind the
	// running turn and only execute post-turn). atomic.Bool (NOT mutex+bool):
	// the reader must consult it WITHOUT holding a lock, because the reject
	// path calls conn.write (itself mutex-guarded).
	inTurn atomic.Bool
```

把上述字段插在 `connSession.seq int` 后；不要重写该 struct 的其他字段。随后在 struct 定义后添加:

```go
func (cs *connSession) setInTurn(v bool) { cs.inTurn.Store(v) }

// isInTurn is called by the reader goroutine (lock-free).
func (cs *connSession) isInTurn() bool { return cs.inTurn.Load() }
```

> 注:`sync/atomic` 已在 ws.go import 中(见 ws.go:11)。删除 v1 计划里的 `inTurnMu sync.Mutex` + `inTurn bool`——它会让 reader 在持锁状态下调用 `conn.write`,与 `wsConn` 写互斥形成锁序循环。

3b. 在 reader goroutine(`if cf.Type == "set_mode" { cs.applySetMode(cf) }` 后)加 bypass:

```go
				// list_seams / restore_turn are rejected INLINE when a turn is
				// running — the main loop is blocked, so queueing them through
				// `frames` would delay the response until post-turn (confusing
				// UX). The reject error frame is written from the reader
				// goroutine directly (conn.write is mutex-guarded). When idle
				// they fall through to `frames` and are handled by the main
				// loop's new list_seams / restore_turn cases.
				if cf.Type == "list_seams" || cf.Type == "restore_turn" {
					if cs.isInTurn() {
						conn.write(proto.NewError(
							"cannot " + cf.Type + " while a turn is running"))
						continue
					}
				}
```

3c. 把 `case "user_message":` 的整个 body(从 `lastUserText := cf.Text` 到本 case 结束)抽出为闭包 `runUserTurn`，并把闭包放在 reader goroutine 启动之后、外层 `for { select { ... } }` 之前。**CB2 禁止盲目 continue→return**：只把用于重新进入外层 for/select 的 `continue` 改成 closure `return`；保留 schema-retry 内层 `for attempt` 的 `continue`(ws.go 当前 schema validation 分支)以及 reader-bypass `continue`。closure 入口建立 turn context；按 `defer release()` 后 `defer postTurnSeam()` 的注册顺序，利用 Go LIFO 保证 post-turn seam → release → inTurn reset。下面给出 permission callback、tool-chunk/retry callbacks、attempt loop、usage、history/session persistence、status/result/done 在内的完整最终 closure：

```go
			// runUserTurn executes the full user_message lifecycle (resolveQuery
			// → maybeAutoCompact → ADK runner → post-turn persistence). It is
			// extracted from the case body so function-level defers cover EVERY
			// exit path (normal / model error / tool error / cancel / disconnect /
			// panic). The closure captures existing outer state only.
			runUserTurn := func(cf proto.ClientFrame) {
				// Register reset first: Go LIFO makes this the final cleanup, so
				// reader bypass remains closed through seam sealing and ctx release.
				cs.setInTurn(true)
				defer cs.setInTurn(false)

				// CB2: establish the turn ctx at closure entry. Register release
				// BEFORE the post-turn seam defer; LIFO then executes the seam first.
				// This also covers resolveQuery and every other early return.
				turnCtx, release := makeTurnCtx()
				defer release()
				defer func() {
					s.sealTurnBoundary(cs.sessionID, cs.turns, len(cs.history),
						string(vcs.SeamPostTurn), "post-turn:"+strconv.Itoa(cs.turns))
				}()

				lastUserText := cf.Text
				query, errMsg := resolveQuery(reg, cf.Text)
				if errMsg != "" {
					conn.write(proto.NewError(errMsg))
					conn.write(proto.NewDone())
					return
				}
				cs.history = append(cs.history, &schema.Message{Role: schema.User, Content: query})
				cs.ensureSession(s, lastUserText)

				// PRE-TURN SEAM: capture the state right after the user msg is
				// appended (revert preserves the question and re-runs the model).
				s.sealTurnBoundary(cs.sessionID, cs.turns, len(cs.history),
					string(vcs.SeamPreTurn), "pre-turn:"+strconv.Itoa(cs.turns+1))

				maybeAutoCompact(turnCtx, s, models, conn, &cs)

				// Interactive permissions (WS-only): install a callback the
				// GuardedTool layer consults when the static profile would
				// deny a tool call. It mints a request id, writes a
				// permission_request frame, then BLOCKS for the user's reply
				// (delivered by the reader goroutine). While blocked, the
				// turn runner is paused — the reader goroutine is separate,
				// so it can still read permission_response. Default-deny on
				// turn cancel / disconnect / 60s timeout. When the static
				// profile already allows the action the callback is never
				// invoked, so this is a no-op for ordinary tool calls. SSE
				// installs no callback and stays on the static profile.
				turnCtx = tools.WithErrCounter(turnCtx)
				turnCtx = tools.WithPermissionCallback(turnCtx, func(req tools.PermissionRequest) tools.PermissionDecision {
					// Mode resolution: before prompting, check whether the
					// session's permission mode auto-resolves this call.
					// allow-edits auto-approves edit tools; yolo approves
					// everything; auto asks an AI to rate the risk 1-10 and
					// approves when at/under the threshold. When resolved,
					// NO permission_request frame is written — the turn flows
					// on without user input. default (and auto over-threshold)
					// fall through to the interactive prompt below.
					if d, resolved := resolvePermissionRequest(turnCtx, cs, models, req); resolved {
						return d
					}
					id := pt.newID()
					ch := make(chan tools.PermissionDecision, 1)
					pt.register(id, ch)
					defer pt.take(id) // remove the entry on every return path
					conn.write(proto.NewPermissionRequest(id, req.Tool, req.Args, req.Reason))
					select {
					case d := <-ch:
						return d
					case <-turnCtx.Done():
						return tools.PermissionDeny
					case <-time.After(60 * time.Second):
						return tools.PermissionDeny
					}
				})

				// Mid-turn compaction progress (Task 35c): the CompactingModel
				// wrapper around the orchestrator's model consults this callback
				// when it summarizes the history BETWEEN ReAct iterations. Each
				// summary text delta is forwarded to the client as a
				// compact_chunk frame so the TUI's compacting block renders live
				// (the same frame type the pre-turn maybeAutoCompact emits). A
				// no-op when the turn never crosses the threshold.
				turnCtx = einollm.WithCompactCallback(turnCtx, func(chunk string) {
					conn.write(proto.NewCompactChunk(chunk))
				})

				// Tool-chunk streaming: each tool's Stream chunks (Text/Status/
				// Overwrite/Err) are forwarded as tool_chunk frames to the TUI for
				// live fixed-template rendering. guard.go's ToolChunkCallback tees
				// every chunk from the tool's Stream to this callback (TUI) in
				// addition to the InvokableRun channel (model collects Result) —
				// resolving the ADK InvokableRun-vs-TUI-Stream tension without
				// rewriting the ADK tool-call loop.
				// One ordered, bounded writer per turn. Tool Stream callbacks only
				// enqueue frames (non-blocking); a single consumer calls conn.write,
				// preserving Text append order while ensuring a slow WS client can
				// never stall the tool goroutine. On overload, live TUI chunks may
				// be dropped; Result still reaches the model through InvokableRun.
				toolChunkFrames := make(chan proto.ServerFrame, 256)
				go func() {
					for {
						select {
						case <-turnCtx.Done():
							return
						case frame := <-toolChunkFrames:
							conn.write(frame)
						}
					}
				}()
				turnCtx = tools.WithToolChunkCallback(turnCtx, func(display string, c tools.ToolChunk) {
					frame := proto.ServerFrame{
						Type:      "tool_chunk",
						ToolName:  display,
						Text:      c.Text,
						Status:    c.Status,
						Overwrite: c.Overwrite,
					}
					select {
					case toolChunkFrames <- frame:
					default: // queue full: drop live UI update, never block the tool
					}
				})

				// Retry progress: the ResilientChatModel consults this
				// callback before each retry sleep (transient error or a
				// mid-stream "unexpected EOF"). Forwarding a retry frame
				// lets the TUI render "↻ retry N/M…" in its activity line,
				// so a retried gateway drop is visible instead of silent.
				// retryReset arms the overwrite-discard of the partial assistant
				// text when a mid-stream retry happens (see the emit closure
				// below). Declared before the retry callback so the callback
				// can capture it.
				var retryReset atomic.Bool
				turnCtx = einollm.WithRetryCallback(turnCtx, func(attempt, max int, err error, delay time.Duration) {
					// Overwrite: a retry supersedes the partial output already
					// streamed this turn. Arm the reset flag (consumed by the
					// emit closure below) so the accumulated assistant text is
					// discarded before the regenerated stream is re-fed, keeping
					// the saved history clean (partial + full → full).
					retryReset.Store(true)
					conn.write(proto.NewRetry(attempt, max, int(delay.Milliseconds()), err.Error()))
				})

				opts := orchestrator.TurnOpts{
					Model:          cs.selectModel(models),
					ThinkingEffort: cs.thinking,
					OutputSchema:   cf.OutputSchema,
				}

				// A12-core: per-turn structured output. When the client declares
				// an output schema (NewUserMessageWithSchema), the loop below
				// validates the model's final assistantText against it and
				// retries with a reminder on failure, mirroring the task_end
				// mandatory-completion path. hasSchema=false keeps the text path
				// byte-identical to pre-A12.
				hasSchema := len(cf.OutputSchema) > 0
				var structuredResult json.RawMessage // set on schema success; emitted before done

				// Bind a per-turn message recorder into turnCtx so the
				// messageRecorder middleware captures the ADK state's messages
				// on each model call. JudgeCompletion reads the final capture
				// after the turn to judge completion against the full
				// conversation (replaying it with the judge question appended,
				// so the provider's KV cache covers the prefix). Binding through
				// context (not a handler field) means recording works for the
				// default runner AND each per-model memoized runner (which
				// installs its own recorder but shares the same turn ctx); each
				// turn has its own recorder, so concurrent WS turns never alias.
				turnCtx = orchestrator.WithNewTurnRecorder(turnCtx)

				// onUsage fires after each model response during the turn
				// (between tool calls, and as the model generates) with the
				// latest per-turn usage. The API reports CUMULATIVE counts per
				// call: prompt includes the full context for that call, so the
				// latest prompt value IS the current context size (overwrite, not
				// a running sum — summing is the doubling bug). Completion is
				// per-call output, so it accumulates across turns for /cost.
				// Cached/Reasoning (Task A6) follow the same overwrite rule:
				// each call's latest value reflects the current context's hit
				// rate and reasoning spend.
				onUsage := func(u orchestrator.TurnUsage) {
					st := cs.statusFrame(s)
					st.TokensIn = u.PromptTokens                     // current context fill
					st.TokensOut = cs.tokensOut + u.CompletionTokens // prior turns + this turn
					st.CachedTokens = u.CachedTokens                 // latest cumulative cache hits
					st.ReasoningTokens = u.ReasoningTokens           // latest cumulative reasoning spend
					conn.write(st)
				}

				var usage orchestrator.TurnUsage
				var judgeCompletionTokens int
				var judgeUsage orchestrator.TurnUsage
				var assistantText string

				// Stop-judge auto-continue. The model ends a turn by stopping
				// naturally; after each attempt JudgeCompletion asks the main
				// model whether the turn fully addressed the user's request.
				// When the judge says incomplete, the loop retries up to
				// maxIncompleteRetries times so the model gets another chance,
				// with the judge's reason injected as a reminder.
				//
				// Break conditions (checked in order after each attempt):
				//   - judge says complete → completed, break.
				//   - error frame emitted (hadError) → real failure (model
				//     error, max-iters), don't retry — retrying would reproduce
				//     the same failure.
				//   - turnCtx.Err() → user cancelled (A3 userCancelCtx), don't
				//     retry. turnCtx derives from userCancelCtx, so a cancel
				//     frame / disconnect surfaces here.
				//   - cap exhausted → accept the last attempt's output and emit
				//     an error marker so the user knows the turn ended abnormally.
				//
				// SIDE-EFFECT CAVEAT: retrying re-runs the turn, which may
				// re-execute side-effecting tools (shell_run, fs_write) the
				// model already called on a prior attempt. This is the inherent
				// trade-off of the stop judge (completeness over idempotency).
				// No de-duplication is performed (out of scope). The frames from
				// earlier attempts have already been forwarded to the client, so
				// the user may see duplicate streamed output across retries. The
				// same caveat applies to schema-validation retries (A12-core): a
				// retried turn re-runs the model and any tools it calls.
				const maxIncompleteRetries = 3
				// retryCap covers whichever path is active: stop-judge
				// completion (maxIncompleteRetries) or schema validation
				// (maxSchemaRetries). Taking the max keeps a single loop bound
				// for both paths; when neither is active the loop still runs a
				// single attempt (both break conditions trip immediately).
				retryCap := maxIncompleteRetries
				if hasSchema && maxSchemaRetries > retryCap {
					retryCap = maxSchemaRetries
				}
				// prevAssistantText captures the previous attempt's final
				// assistant text so the retry can extend cs.history with it
				// plus a reminder (see below). Only read when attempt > 0;
				// the first iteration uses cs.history as-is. Set at the end
				// of each iteration ONLY when the loop is about to retry
				// (no break), so breaks don't leak partial output into a
				// subsequent iteration's reminder.
				var prevAssistantText string
				// reminder is set on the retry path that caused the loop to
				// continue: schema retries carry the validation error (so the
				// model knows what to fix); stop-judge retries carry the judge's
				// reason. Reset to "" each iteration is NOT needed — it is only
				// set immediately before `continue`, so the next iteration sees
				// the right reminder; on the first attempt it stays "".
				var reminder string
				for attempt := 0; attempt <= retryCap; attempt++ {
					// Reset per-attempt state so earlier attempts' partial output
					// is discarded — the FINAL attempt's output is what the user
					// keeps in history. usage holds the last attempt's totals
					// (overwritten, not summed), so cs.tokensOut accumulates only
					// the accepted attempt's completion tokens.
					assistantText = ""
					usage = orchestrator.TurnUsage{}
					retryReset.Store(false)

					// Build the history for this attempt. Attempt 0 uses
					// cs.history unchanged. attempt > 0 extends a COPY of
					// cs.history with the previous attempt's assistant output
					// (as an assistant message) plus a user reminder. The
					// reminder differs by path: schema retries carry the
					// validation error (so the model knows what to fix),
					// task_end retries use the default nudge telling the model
					// to call task_end. Without this extension every retry
					// would re-send the SAME history, the model would reproduce
					// the same behavior, and the retry would be useless.
					//
					// The copy is mandatory: Go append may share cs.history's
					// backing array, and mutating it would leak the reminder
					// into the persistent session history (the post-loop
					// cs.history append would then double-count turns and
					// corrupt multi-turn memory).
					//
					// prevAssistantText may be empty (e.g. a turn that only
					// streamed tool_call frames with no assistant text); in
					// that case the assistant echo is skipped but the reminder
					// is still added so the model is guided toward completion.
					history := cs.history
					if attempt > 0 {
						extra := make([]*schema.Message, 0, 2)
						if prevAssistantText != "" {
							extra = append(extra, schema.AssistantMessage(prevAssistantText, nil))
						}
						msg := reminder
						if msg == "" {
							msg = "Continue and finish addressing the user's request."
						}
						extra = append(extra, schema.UserMessage(msg))
						history = make([]*schema.Message, 0, len(cs.history)+len(extra))
						history = append(history, cs.history...)
						history = append(history, extra...)
					}

					iter := o.EventsWithHistoryOpts(turnCtx, history, opts)
					var hadError bool
					orchestrator.ClassifyEventsWithUsage(iter, &usage, func(f proto.ServerFrame) {
						// A real failure (model error, MaxIterations) surfaces as
						// an error frame — don't retry after it.
						if f.Type == "error" {
							hadError = true
						}
						// A retry happened since the last frame → drop the partial
						// assistant text so the regenerated output replaces it
						// (overwrite, not append). CompareAndSwap resets the arm so
						// only the first frame after a retry triggers the discard.
						if retryReset.CompareAndSwap(true, false) {
							assistantText = ""
						}
						if f.Type == "agent_chunk" {
							assistantText += f.Text
						}
						if f.Type == "tool_call" {
							cs.toolCalls++
						}
						conn.write(f)
					}, onUsage)

					// Hard failures break regardless of mode: a model error or
					// a user cancel must not trigger a schema/stop-judge retry.
					// The error frame has already been emitted upstream; the
					// post-loop path still persists state and emits done.
					if hadError {
						break
					}
					if turnCtx.Err() != nil {
						break // user cancelled (A3 userCancelCtx) — don't retry
					}

					if hasSchema {
						// A12-core structured-output path: validate the final
						// assistant text against the declared schema. Success
						// sets structuredResult and breaks; failure either
						// retries with a reminder (carrying the validation
						// error) or, at the cap, emits an error frame.
						validated, verr := ValidateStructuredOutput(assistantText, cf.OutputSchema)
						if verr == nil {
							structuredResult = validated
							break // schema satisfied — done
						}
						if attempt == retryCap {
							conn.write(proto.NewError("output did not match the required schema after " +
								strconv.Itoa(attempt+1) + " attempt(s): " + verr.Error()))
							break
						}
						prevAssistantText = assistantText
						reminder = schemaRetryReminder(assistantText, verr)
						continue
					}

					// Stop-judge path: ask the model to judge whether the turn
					// is complete against the full conversation the recorder
					// captured. complete → accept; incomplete → retry with the
					// judge's reason as the reminder.
					complete, reason, ju := o.JudgeCompletion(turnCtx)
					judgeCompletionTokens += ju.CompletionTokens
					judgeUsage = ju
					if complete {
						break // judge accepted the turn as complete
					}
					if attempt == retryCap {
						// Cap exhausted: the judge kept flagging the turn as
						// incomplete. Accept the last attempt's output and warn
						// the user.
						conn.write(proto.NewError("turn ended without the completion judge accepting it; output may be incomplete"))
						break
					}
					// About to retry: capture this attempt's assistant text and
					// set the reminder to the judge's reason so the next attempt
					// addresses it.
					prevAssistantText = assistantText
					reminder = orchestrator.JudgeRetryNudge(reason)
				}

				// Prompt tokens overwrite (the API reports the cumulative context
				// per call, so the latest value is the current context size — the
				// ctx bar must reflect fill, not a forever-growing sum, which is
				// the doubling bug). Completion tokens accumulate across turns
				// (each call's output is separate), giving a real /cost total.
				// Cached/Reasoning (Task A6) follow prompt's overwrite rule: the
				// API reports the cumulative totals per call, so the latest value
				// is the current context's hit rate and reasoning spend — summing
				// would double-count on every model call.
				cs.tokensIn = usage.PromptTokens
				cs.tokensOut += usage.CompletionTokens + judgeCompletionTokens
				cs.cachedTokens = usage.CachedTokens
				cs.reasoningTokens = usage.ReasoningTokens
				// The judge call is the turn's last model invocation on a real
				// model; reflect its (cache-heavy) usage as the current context
				// so /cost and the ctx bar account for the judge probe. Skipped
				// when the judge usage is zero (FakeModel's judge probe, or a
				// provider that omits usage on non-streaming Generate) so it
				// doesn't clobber the turn's real context with zeros.
				if judgeUsage.PromptTokens > 0 {
					cs.tokensIn = judgeUsage.PromptTokens
					cs.cachedTokens = judgeUsage.CachedTokens
					cs.reasoningTokens = judgeUsage.ReasoningTokens
				}
				cs.turns++

				if assistantText != "" {
					cs.history = append(cs.history, &schema.Message{Role: schema.Assistant, Content: assistantText})
				}
				// Persist the turn to the DB (best-effort).
				cs.persistMessages(s, lastUserText, assistantText)
				// Persist session meta (model, thinking, token counters).
				if s.store != nil && cs.sessionID != "" {
					_ = s.store.UpdateSessionMeta(cs.sessionID, cs.model, cs.thinking, cs.tokensIn, cs.tokensOut, cs.turns, cs.cachedTokens, cs.reasoningTokens)
				}
				// Emit updated status (usage + turns + context window) so the
				// TUI's ctx bar reflects real token usage after every turn.
				conn.write(cs.statusFrame(s))
				// A12-core: emit the validated structured result before done so
				// a schema-constrained consumer (exec --output-schema, API
				// client, later the TUI) can take the parsed JSON without
				// re-parsing the stream. Skipped on text-mode turns
				// (structuredResult stays nil — hasSchema=false never sets it).
				if structuredResult != nil {
					conn.write(proto.NewStructuredResult(structuredResult))
				}
				// Turn-end summary "Done N tools uses X tokens Y" rides on the
				// done frame's Text so it neither disturbs the agent_chunk
				// stream nor pollutes cs.history (the assistantText append
				// above already captured the model's real answer). The TUI
				// renders it as the turn's closing line.
				conn.write(proto.ServerFrame{Type: "done", Text: turnEndSummary(cs)})
			}

			case "user_message":
				runUserTurn(cf)
			case "list_seams":
				handleListSeams(s, conn, &cs)
			case "restore_turn":
				handleRestoreTurn(s, conn, &cs, cf.ID, cf.ConfirmedHead)
```

> 注:抽取时逐个审计原 case body 的 `continue` 目标,禁止 block-wide find/replace。仅原本跳回 WS 外层 frame-dispatch loop 的 `continue` 改为 closure `return`;schema validation/retry 的 attempt-loop `continue` 原样保留。原 happy-path 的显式 `release()` 删除,由唯一的 `defer release()` 负责,避免 double release;reader goroutine 的 bypass `continue` 不在 closure 内且保持不变。

3d. `ws.go` 顶部加 imports：

```go
"github.com/x6nux/yanshi/internal/store"
"github.com/x6nux/yanshi/internal/vcs"
```

`store` 供 3g 的 `store.SessionRevertSnapshot` 使用；`vcs` 供 pre/post seam kind 使用。两者都不是可选 import。

3e. 用以下完整函数替换 `connSession.statusFrame`(只新增 display `CommitShort` 与 full `Head`，D6 binding):

```go
func (cs *connSession) statusFrame(s *Server) proto.ServerFrame {
	mode, auto := cs.perm.get()
	st := proto.NewStatusWithMode(
		cs.displayModel(), cs.thinking, cs.tokensIn, cs.tokensOut, cs.turns,
		contextWindowFor(cs.model, s.compaction), string(mode), auto,
	)
	st.CachedTokens = cs.cachedTokens
	st.ReasoningTokens = cs.reasoningTokens
	st.SessionID = cs.sessionID
	st.CommitShort = s.shortHead() // display-only first 8 chars
	st.Head = s.fullHead()         // D6 full id for next restore_turn
	return st
}
```

3f. **D3 — forced approval 不得被 yolo/auto/allow-edits 绕过**。先在 `ChatWS` 外、`resolvePermissionMode` 附近添加 package-level mode gate；`Force=true` 固定返回 unresolved，非 Force 才委托现有 `resolvePermissionMode`：

```go
// First add the Force field to PermissionRequest in permctx.go:
// type PermissionRequest struct {
//     Tool    string
//     Args    string
//     Reason  string
//     Force   bool   // D3: forced approval (must prompt even in yolo/auto)
// }

// resolvePermissionRequest applies auto-mode resolution only to ordinary
// requests. A forced destructive request must stay unresolved so the caller
// emits permission_request and waits for an explicit one-shot decision.
func resolvePermissionRequest(ctx context.Context, cs connSession,
	models map[string]model.BaseChatModel, req tools.PermissionRequest,
) (tools.PermissionDecision, bool) {
	if req.Force {
		return tools.PermissionDeny, false
	}
	return resolvePermissionMode(ctx, cs, models, req)
}
```

然后在 3c 的 `runUserTurn` closure 内，用下列完整 callback 替换现有 `tools.WithPermissionCallback` 赋值。此代码依赖 closure 局部变量 `turnCtx`、`cs`、`models`、`pt`、`conn`，不得放到 package scope：

```go
turnCtx = tools.WithPermissionCallback(turnCtx, func(req tools.PermissionRequest) tools.PermissionDecision {
	// D3: destructive RequireApproval sets req.Force. The gate never auto-resolves
	// a forced request under yolo / allow-edits / auto, so it always reaches the
	// normal permission_request path below.
	if d, resolved := resolvePermissionRequest(turnCtx, cs, models, req); resolved {
		return d
	}
	id := pt.newID()
	ch := make(chan tools.PermissionDecision, 1)
	pt.register(id, ch)
	defer pt.take(id)
	conn.write(proto.NewPermissionRequest(id, req.Tool, req.Args, req.Reason))
	select {
	case d := <-ch:
		return d
	case <-turnCtx.Done():
		return tools.PermissionDeny
	case <-time.After(60 * time.Second):
		return tools.PermissionDeny
	}
})
```

3g. **复用 durable snapshot → `connSession` 映射**：本 Task Step 4 的 `ws_seam.go` 定义 `applySessionRevertSnapshot`；把 `ws.go` 现有 `loadSession` 中从 `// Load messages...` 到各 meta 字段赋值的重复映射替换为：

```go
	msgs, err := s.store.Messages(sessionID)
	if err != nil {
		return err
	}
	applySessionRevertSnapshot(cs, store.SessionRevertSnapshot{
		Meta:     *ss,
		Messages: msgs,
	})
	return nil
```

这样 reconnect 加载与 undo snapshot 恢复使用同一角色/metadata 映射,不复制逻辑。

- [ ] **Step 4: 创建 `internal/api/http/ws_seam.go`(seam handler 拆分)**

```go
// internal/api/http/ws_seam.go
//
// Seam-related WS handlers, split from ws.go to keep that file under the 1000
// pure-code-line cap (B2-RB1 CLAUDE.md). handleListSeams replies with the
// recent seams for the session + the current head (short display hash and full
// destructive binding); handleRestoreTurn
// validates the target binding, reverts main + truncates history + replies with
// the undo seam id.

package http

import (
	"fmt"
	"slices"

	"github.com/cloudwego/eino/schema"

	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/store"
	"github.com/x6nux/yanshi/internal/vcs"
)

// applySessionRevertSnapshot replaces only the conversation-owned connSession
// fields from an exact durable snapshot. It intentionally leaves live transport
// state (perm, inTurn, startedAt, defaultModel) untouched.
func applySessionRevertSnapshot(cs *connSession, snap store.SessionRevertSnapshot) {
	hist := make([]*schema.Message, 0, len(snap.Messages))
	for _, m := range snap.Messages {
		role := schema.Assistant
		if m.Role == "user" {
			role = schema.User
		}
		hist = append(hist, &schema.Message{Role: role, Content: m.Content})
	}
	cs.history = hist
	cs.sessionID = snap.Meta.ID
	cs.seq = len(snap.Messages)
	cs.model = snap.Meta.Model
	cs.thinking = snap.Meta.Thinking
	cs.tokensIn = snap.Meta.TokensIn
	cs.tokensOut = snap.Meta.TokensOut
	cs.cachedTokens = snap.Meta.CachedTokens
	cs.reasoningTokens = snap.Meta.ReasoningTokens
	cs.turns = snap.Meta.Turns
}

// handleListSeams replies with the recent seams for the session (filtered by
// cs.sessionID when set — D7: never pass "" here or other sessions' seams leak)
// + the current main_head (short for display, full for binding). When VCS is
// unconfigured the reply is an empty list (the TUI renders "(no seams)").
func handleListSeams(s *Server, conn *wsConn, cs *connSession) {
	if s.vcs == nil || s.repoID == "" {
		conn.write(proto.NewSeams(nil, "", ""))
		return
	}
	if cs.sessionID == "" {
		// D7: an unestablished WS session has no seam namespace. Return an empty
		// list without ever passing "" to ListSeams (which means all sessions).
		conn.write(proto.NewSeams(nil, s.shortHead(), s.fullHead()))
		return
	}
	// D7: always scope by this concrete session id; never call ListSeams with "".
	seams, err := s.vcs.ListSeams(s.repoID, cs.sessionID, 0)
	if err != nil {
		conn.write(proto.NewError("list_seams: " + err.Error()))
		return
	}
	items := make([]proto.SeamInfo, 0, len(seams))
	for _, sm := range seams {
		items = append(items, proto.SeamInfo{
			ID:          sm.ID,
			Kind:        string(sm.Kind),
			TurnSeq:     sm.TurnSeq,
			HistoryLen:  sm.HistoryLen,
			CommitShort: shortHash(sm.CommitID),
			Label:       sm.Label,
			CreatedAt:   sm.CreatedAt,
		})
	}
	conn.write(proto.NewSeams(items, s.shortHead(), s.fullHead()))
}

// handleRestoreTurn validates the target binding, reverts main to the seam's
// commit, truncates the in-memory + persisted history, and replies with the
// undo seam id (so the user can revert again to undo this revert).
//
// Fail-closed ordering (D5): validate everything; transition durable session
// state first (truncate for an ordinary seam, restore the undo snapshot for a
// pre-revert seam); update memory; only then call VCS revert. The exact durable
// state before this operation is passed into RevertToSeam and stored on the new
// undo seam in the same tx as main_head (D2). A durable transition failure is
// FATAL and VCS/memory remain untouched. If VCS fails, restore both conversation
// layers from that exact snapshot before replying; compensation failure is FATAL.
//
// confirmedHead (D6): the FULL main_head commit id the client bound at list
// time. The server REQUIRES it non-empty and compares the FULL id — short-hash
// comparison risks collision across long histories.
func handleRestoreTurn(s *Server, conn *wsConn, cs *connSession, seamID, confirmedHead string) {
	if s.vcs == nil || s.repoID == "" || seamID == "" {
		conn.write(proto.NewError("restore_turn: VCS disabled or missing seam_id"))
		conn.write(proto.NewDone())
		return
	}

	// D6: require non-empty confirmedHead AND full-id match.
	if confirmedHead == "" {
		conn.write(proto.NewError("restore_turn: missing confirmed_head (re-list to bind the current head)"))
		conn.write(proto.NewDone())
		return
	}
	if actual := s.fullHead(); actual != confirmedHead {
		conn.write(proto.NewError(
			fmt.Sprintf("restore_turn: head changed since listing (was %s, now %s); re-list and re-confirm",
				shortHash(confirmedHead), shortHash(actual))))
		conn.write(proto.NewDone())
		return
	}

	// Load the seam to capture its history boundary + session BEFORE reverting.
	seam, err := s.vcs.FindSeam(seamID)
	if err != nil {
		conn.write(proto.NewError("restore_turn: seam not found: " + err.Error()))
		conn.write(proto.NewDone())
		return
	}
	// D7: exact match, including rejecting an empty current session.
	if cs.sessionID == "" || seam.SessionID != cs.sessionID {
		conn.write(proto.NewError("restore_turn: seam does not belong to this session"))
		conn.write(proto.NewDone())
		return
	}

	// Conversation rollback requires the durable store. In the current server a
	// non-empty sessionID is created only when Store is configured, but keep this
	// explicit and fail closed rather than silently doing a VCS-only WS restore.
	if s.store == nil {
		conn.write(proto.NewError("restore_turn: FATAL: durable session store is unavailable"))
		conn.write(proto.NewDone())
		return
	}

	oldHistoryLen, oldTurns := len(cs.history), cs.turns
	var durableBefore store.SessionRevertSnapshot

	if seam.Kind == vcs.SeamPreRevert {
		// D2 undo expansion: the first revert stored its exact pre-revert messages
		// on this seam. Decode + validate the payload before mutating either layer.
		target, decodeErr := store.DecodeSessionRevertSnapshot(seam.HistorySnapshot)
		if decodeErr != nil || target.Meta.ID != cs.sessionID ||
			len(target.Messages) != seam.PrevHistoryLen ||
			target.Meta.Turns != seam.PrevTurnSeq {
			conn.write(proto.NewError(fmt.Sprintf(
				"restore_turn: FATAL: invalid undo history snapshot: err=%v session=%q len=%d/%d turns=%d/%d",
				decodeErr, target.Meta.ID, len(target.Messages), seam.PrevHistoryLen,
				target.Meta.Turns, seam.PrevTurnSeq)))
			conn.write(proto.NewDone())
			return
		}
		var err error
		durableBefore, err = s.store.SnapshotSessionForRevert(cs.sessionID)
		if err == nil {
			err = s.store.RestoreSessionAfterFailedRevert(target)
		}
		if err != nil {
			conn.write(proto.NewError(
				"restore_turn: FATAL: durable undo history restore failed: " + err.Error()))
			conn.write(proto.NewDone())
			return
		}
		applySessionRevertSnapshot(cs, target)
	} else {
		// Ordinary seam: target must be a prefix of the live history. Zero is valid.
		truncLen, truncTurns := seam.HistoryLen, seam.TurnSeq
		if truncLen < 0 || truncLen > len(cs.history) || truncTurns < 0 {
			conn.write(proto.NewError(fmt.Sprintf(
				"restore_turn: invalid history boundary len=%d/%d turns=%d",
				truncLen, len(cs.history), truncTurns)))
			conn.write(proto.NewDone())
			return
		}
		var err error
		durableBefore, err = s.store.TruncateSessionForRevert(
			cs.sessionID, truncLen, truncTurns)
		if err != nil {
			conn.write(proto.NewError(
				"restore_turn: FATAL: durable history truncation failed: " + err.Error()))
			conn.write(proto.NewDone())
			return
		}
		cs.history = slices.Clone(cs.history[:truncLen])
		cs.seq = truncLen
		cs.turns = truncTurns
	}

	// D5 phase 3: VCS last. Task 5 compensates disk for its own internal errors.
	// On success it stores durableBefore on the returned undo seam atomically with
	// main_head; on failure, restore the exact pre-operation conversation state.
	undoID, err := s.vcs.RevertToSeam(s.repoID, seamID,
		"restore by "+cs.sessionID, oldHistoryLen, oldTurns, &durableBefore)
	if err != nil {
		restoreErr := s.store.RestoreSessionAfterFailedRevert(durableBefore)
		applySessionRevertSnapshot(cs, durableBefore)
		if restoreErr != nil {
			conn.write(proto.NewError(fmt.Sprintf(
				"restore_turn: FATAL: VCS revert failed (%v) and durable history compensation failed (%v)",
				err, restoreErr)))
			conn.write(proto.NewDone())
			return
		}
		conn.write(proto.NewError("restore_turn: " + err.Error()))
		conn.write(proto.NewDone())
		return
	}

	conn.write(proto.NewSeamRestored(undoID, s.shortHead(), s.fullHead(),
		fmt.Sprintf("reverted to %s (%s); undo seam %s",
			seam.Label, shortHash(seam.CommitID), shortHash(undoID))))
	conn.write(cs.statusFrame(s))
	conn.write(proto.NewDone())
}

// shortHash returns the first 8 hex of a commit id (same truncation as
// Server.shortHead but for arbitrary ids).
func shortHash(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
```

> 注:`ws_seam.go` 的完整 import 如代码所示:`fmt`, `slices`, `github.com/cloudwego/eino/schema`, `internal/proto`, `internal/store`, `internal/vcs`;这里必须复用 `ws.go` 已用的 Eino schema package，不存在 `internal/llm/schema`。不再需要旧 stderr 路径使用的 `os`。

- [ ] **Step 5: 跑测试确认通过**

Run: `go test ./internal/api/http/ -run 'TestWS_PreAndPostTurnSeamsCreated|TestWS_ListSeamsWhileInTurnRejected|TestWS_PostTurnSeamFires|TestWS_RestoreTurn|TestResolvePermissionRequest_Force' -v`
Expected: PASS,含 durable truncation failure fatal-before-VCS、cross-session reject、full-head binding与 happy path。

- [ ] **Step 6: 跑全部 WS 测试确认无回归**

Run: `go test ./internal/api/http/ -v`
Expected: 全部 PASS

- [ ] **Step 7: 提交**

```bash
git add internal/api/http/ws.go internal/api/http/ws_seam.go internal/api/http/ws_seam_test.go
git commit -m "feat(api/http): WS runUserTurn + pre/post seam + reader bypass + restore handler (B2-RB1 C/F/G/I)"
```

---
## Task 10: SSE — turn seam lifecycle + fail-closed revert + ssebackend mapping

**Files:**
- Modify: `internal/api/http/chat.go`
- Verify only: `internal/cli/ssebackend.go`（D7：确认不新增永远收不到的 `seam_restored` case，不产生 diff）
- Modify: `internal/cli/wsbackend.go`(`isControlReply` + `toStreamEvent`)
- Modify: `internal/cli/backend.go`(`StreamEvent` 加 seam 字段)
- Test: `internal/api/http/sse_seam_test.go`(新建)
- Test: `internal/cli/wsbackend_seam_test.go`(新建)

> 必修项 D + I。SSE 与 WS 必须在 seam lifecycle 上对称:
>
> - SSE turn 的 pre-turn / post-turn seam 必须执行(用 `Server.sealTurnBoundary`,且像 WS 一样用 `defer` 保证 panic / early-return 也 fire —— CB5)。session_id 用空串(SSE 是 stateless POST-per-turn,没有持久 session)。
> - SSE 是无交互 callback 的传输 → `revert_turn` agent 工具经 `tools.RequireApproval` 在 SSE 中必须 fail-closed(Task 11 实现 `RequireApproval`)。因此 **SSE 永远不会收到 `seam_restored` 帧**(revert 在审批阶段就被拒),`ssebackend.flush()` **不添加** `seam_restored` case(D7:这是 dead code,添加它会让 reviewer 误以为 SSE 支持回滚)。default 分支已足够。
> - `wsbackend.isControlReply` 必须加 `seams`/`seam_restored`(否则 control reply channel 永远不关闭,`/restore-turn` 永远 hang);`toStreamEvent` 必须映射 seam 字段(否则 TUI 拿不到数据)。
> - `StreamEvent` 加 `Seams`/`CommitShort`/`Head`/`UndoSeamID` 字段供 TUI 消费。

- [ ] **Step 1: 写全部 RED 测试** — `internal/api/http/sse_seam_test.go` + `internal/cli/wsbackend_seam_test.go`(均新建)

```go
// internal/api/http/sse_seam_test.go
package http

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/store"
	"github.com/x6nux/yanshi/internal/vcs"
)

// TestSSE_PreAndPostTurnSeamsCreated verifies that a single SSE chat turn
// creates both pre-turn and post-turn seams (transport parity with WS, 必修项 D).
func TestSSE_PreAndPostTurnSeamsCreated(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	st, err := store.Open(filepath.Join(base, "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	v := vcs.New(st, filepath.Join(base, "worktrees"))
	repoID, err := v.InitRepo(root)
	if err != nil {
		t.Fatalf("InitRepo: %v", err)
	}
	srv := New(Config{Store: st, VCS: v, RepoID: repoID})
	// CB5: real deterministic Fake orchestrator — never pass nil (the handler
	// dereferences it in the runner path). Helper is defined in Task 9's
	// ws_seam_test.go and available to all tests in package http.
	o := newFakeOrchestrator(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/chat",
		strings.NewReader(`{"message":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	// CB5: call the extracted core directly — no mux re-registration.
	srv.handleSSEInternal(rec, req, o, nil, nil)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	seams, err := v.ListSeams(repoID, "", 0)
	if err != nil {
		t.Fatalf("ListSeams: %v", err)
	}
	if len(seams) < 2 {
		t.Errorf("SSE turn should create >=2 seams, got %d", len(seams))
	}
}
```

> 注:`newFakeOrchestrator(t)` 由 Task 9 的 `ws_seam_test.go` 定义,使用真实 `orchestrator.New` + `einollm.NewFakeModel`;不新增第二份 helper。`handleSSEInternal` 是 production core(不是 test-only),由 `Chat` 注册的 closure 与测试共同调用。
>
> 同一个 RED checkpoint 还必须先创建 WS client mapping 测试；这样 `backend.go` / `wsbackend.go` 的实现不会先于测试落地：

```go
// internal/cli/wsbackend_seam_test.go
package cli

import (
	"testing"

	"github.com/x6nux/yanshi/internal/proto"
)

// TestIsControlReply_SeamFrames verifies isControlReply closes the control
// channel on seam replies (otherwise SendFrame would hang forever waiting for
// a done that never comes — these are single-frame replies).
func TestIsControlReply_SeamFrames(t *testing.T) {
	for _, kind := range []string{"seams", "seam_restored"} {
		if !isControlReply(kind) {
			t.Errorf("isControlReply(%q) = false, want true", kind)
		}
	}
}

// TestToStreamEvent_SeamsFrame verifies the seams reply's Seams slice and
// CommitShort/Head propagate through toStreamEvent (D6).
func TestToStreamEvent_SeamsFrame(t *testing.T) {
	f := proto.NewSeams(
		[]proto.SeamInfo{{ID: "s1", Kind: "pre-turn", CommitShort: "abc12345"}},
		"abc12345",
		"fullheadabcdef0123456789",
	)
	ev := toStreamEvent(f)
	if ev.Kind != "seams" {
		t.Errorf("ev.Kind = %q, want %q", ev.Kind, "seams")
	}
	if len(ev.Seams) != 1 || ev.Seams[0].ID != "s1" {
		t.Errorf("ev.Seams = %+v, want one entry s1", ev.Seams)
	}
	if ev.CommitShort != "abc12345" {
		t.Errorf("ev.CommitShort = %q, want %q", ev.CommitShort, "abc12345")
	}
	if ev.Head != "fullheadabcdef0123456789" {
		t.Errorf("ev.Head = %q, want full id", ev.Head)
	}
}

// TestToStreamEvent_SeamRestoredFrame verifies the seam_restored reply's ID is
// mapped to UndoSeamID (the load-bearing field for "undo this revert" UX).
func TestToStreamEvent_SeamRestoredFrame(t *testing.T) {
	f := proto.NewSeamRestored("undo-xyz", "deadbeef", "fullpostreverthead01", "summary")
	ev := toStreamEvent(f)
	if ev.UndoSeamID != "undo-xyz" {
		t.Errorf("ev.UndoSeamID = %q, want %q", ev.UndoSeamID, "undo-xyz")
	}
	if ev.CommitShort != "deadbeef" {
		t.Errorf("ev.CommitShort = %q, want %q", ev.CommitShort, "deadbeef")
	}
}
```

- [ ] **Step 2: 运行全部 RED 测试并确认预期失败**

```bash
go test ./internal/api/http/ -run TestSSE_PreAndPostTurnSeamsCreated -v
go test ./internal/cli/ -run 'TestIsControlReply_SeamFrames|TestToStreamEvent_Seams' -v
```

预期:第一条测试编译失败,因为 `(*Server).handleSSEInternal` 尚未定义;第二条测试编译失败,因为 `StreamEvent.Seams` / `CommitShort` / `Head` / `UndoSeamID` 和 seam frame mapping 尚未定义。两条失败都来自各自尚缺的 production 合同,而不是 fixture 或环境错误。

- [ ] **Step 3: 修改 `internal/api/http/chat.go`**

3a. 用下面的完整 production method 承接现有 SSE handler，并让 `Chat` 只注册一次 route 后转发参数（CB5：测试直接调用 method，不重新注册 mux route）。代码块完整列出 request decode、resolveQuery、compaction、runner、schema retry、usage/status/done 以及 seam lifecycle：

```go
func (s *Server) Chat(o *orchestrator.Orchestrator,
	models map[string]model.BaseChatModel, reg *skills.Registry) {
	s.HandleFunc("POST /api/v1/chat", func(w http.ResponseWriter, r *http.Request) {
		s.handleSSEInternal(w, r, o, models, reg)
	})
}

// handleSSEInternal owns one SSE request lifecycle. It is a production seam
// (also invoked directly by deterministic tests), not a test-only helper.
func (s *Server) handleSSEInternal(w http.ResponseWriter, r *http.Request,
	o *orchestrator.Orchestrator, models map[string]model.BaseChatModel,
	reg *skills.Registry) {
	var req struct {
		Message  string           `json:"message"`
		Messages []schema.Message `json:"messages"`
		Model    string           `json:"model,omitempty"`
		Thinking string           `json:"thinking,omitempty"`
		// OutputSchema carries an optional JSON Schema for this turn (A12-core).
		// When non-empty the handler validates the model's final assistant text
		// against it and retries with a reminder on failure (up to
		// maxSchemaRetries); on success it emits a structured_result event
		// before status/done. Empty/absent ⇒ text mode, byte-identical to
		// pre-A12 (the entire schema retry loop is skipped).
		OutputSchema json.RawMessage `json:"output_schema,omitempty"`
	}
	if err := json.NewDecoder(limitBody(w, r)).Decode(&req); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}

	history := req.Messages
	if len(history) == 0 && req.Message != "" {
		history = []schema.Message{{Role: schema.User, Content: req.Message}}
	}
	if len(history) == 0 {
		writeSSEError(w, "empty request")
		return
	}

	// Apply /skill prefix to the last user turn (the new message).
	if last := &history[len(history)-1]; last.Role == schema.User {
		q, errMsg := resolveQuery(reg, last.Content)
		if errMsg != "" {
			writeSSEError(w, errMsg)
			return
		}
		last.Content = q
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	fl, _ := w.(http.Flusher)

	msgs := make([]*schema.Message, len(history))
	for i := range history {
		msgs[i] = &history[i]
	}

	// A valid SSE request is one logical turn. SSE keeps history client-side,
	// so session_id is intentionally empty; the message count is still sealed
	// for audit symmetry with WS. Register post immediately after pre so every
	// later runner/compaction/schema exit, including panic, executes it.
	s.sealTurnBoundary("", 1, len(msgs),
		string(vcs.SeamPreTurn), "sse:pre-turn:1")
	defer s.sealTurnBoundary("", 1, len(msgs),
		string(vcs.SeamPostTurn), "sse:post-turn:1")

	// Auto context-compaction (Task 35b): if the received history exceeds
	// the threshold, summarize the older turns on a remote model, STREAMING
	// each summary delta as a compact_chunk SSE event, then emit
	// history_replaced (the compacted slice, so sseBackend adopts it before
	// its next request) and status{compacted} before the turn frames.
	// Disabled (threshold <= 0) or under-threshold histories are a no-op.
	// SSE holds history client-side, so publishing the compacted slice is
	// what keeps the next POST consistent with the server's view.
	kr := keepRecentOrDefault(s.compaction.KeepRecent)
	cw := contextWindowFor(req.Model, s.compaction)
	sumModel := compactionModel(s.compaction, models, req.Model)
	var newMsgs []*schema.Message
	var tb, ta int
	compacted := false
	if sumModel != nil {
		newMsgs, tb, ta, compacted = ctxcompact.MaybeCompact(r.Context(), msgs,
			s.compaction.Threshold, cw, kr, sumModel,
			func(chunk string) { writeSSEFrame(w, fl, proto.NewCompactChunk(chunk)) })
	}
	if compacted {
		msgs = newMsgs
		compactedHistory := make([]schema.Message, len(msgs))
		for i, m := range msgs {
			compactedHistory[i] = *m
		}
		writeSSEFrame(w, fl, proto.NewHistoryReplaced(compactedHistory))
		st := proto.NewStatus(req.Model, req.Thinking, 0, 0, 0, contextWindowFor(req.Model, s.compaction))
		st.Compacted, st.TokensBefore, st.TokensAfter = true, tb, ta
		writeSSEFrame(w, fl, st)
	}

	// Per-request model + thinking. An unknown/empty model name falls back
	// to the orchestrator default (models[name] is nil for a nil map or an
	// absent name); an unrecognized thinking effort is a no-op downstream.
	opts := orchestrator.TurnOpts{
		ThinkingEffort: req.Thinking,
		OutputSchema:   req.OutputSchema,
	}
	if req.Model != "" && models[req.Model] != nil {
		opts.Model = models[req.Model]
	}

	// A12-core: per-turn structured output. When the POST body declares an
	// output_schema the loop below validates the model's final assistantText
	// against it and retries with a reminder on failure, mirroring the WS
	// handler's schema path. hasSchema=false keeps the text path
	// byte-identical to pre-A12 (retryCap=0 ⇒ single attempt, original flow).
	hasSchema := len(req.OutputSchema) > 0
	retryCap := 0
	if hasSchema {
		retryCap = maxSchemaRetries
	}
	var usage orchestrator.TurnUsage
	var structuredResult json.RawMessage
	var prevAssistantText string
	var lastVErr error
	for attempt := 0; attempt <= retryCap; attempt++ {
		// Build the history for this attempt. Attempt 0 uses msgs unchanged.
		// attempt > 0 extends a COPY of msgs with the previous attempt's
		// assistant output plus a user reminder carrying the validation
		// error — without this extension every retry would re-send the SAME
		// history, the model would reproduce the same invalid output, and
		// the retry would be useless. The copy is mandatory: appending to
		// msgs directly would alias its backing array and leak the reminder
		// into subsequent attempts' baseline.
		runMsgs := msgs
		if attempt > 0 {
			extra := make([]*schema.Message, 0, 2)
			if prevAssistantText != "" {
				extra = append(extra, schema.AssistantMessage(prevAssistantText, nil))
			}
			extra = append(extra, schema.UserMessage(schemaRetryReminder(prevAssistantText, lastVErr)))
			runMsgs = make([]*schema.Message, 0, len(msgs)+len(extra))
			runMsgs = append(runMsgs, msgs...)
			runMsgs = append(runMsgs, extra...)
		}

		// Reset per-attempt state so earlier attempts' partial output is
		// discarded — the FINAL attempt's usage is what sseStatus reports.
		usage = orchestrator.TurnUsage{}
		var assistantText string
		// SSE is stateless and unidirectional, so it does NOT install a
		// permission callback: tool calls denied by the static profile are
		// denied (no interactive prompt). Interactive permissions are
		// WS-only (see ws.go). The orchestrator injects the static profile
		// here. tc is recreated per attempt so the err-counter is fresh.
		tc := tools.WithErrCounter(r.Context())
		iter := o.EventsWithHistoryOpts(tc, runMsgs, opts)
		var hadError bool
		orchestrator.ClassifyEventsWithUsage(iter, &usage, func(f proto.ServerFrame) {
			if f.Type == "error" {
				hadError = true
			}
			if f.Type == "agent_chunk" {
				assistantText += f.Text
			}
			writeSSEFrame(w, fl, f)
		})
		// Hard failures break regardless of mode: a model error or a user
		// cancel must not trigger a schema retry. The error frame has
		// already been emitted above; the post-loop path still emits status
		// + done.
		if hadError || r.Context().Err() != nil {
			break
		}
		if !hasSchema {
			break // text mode: single attempt, original behavior
		}
		// Schema path: validate the final assistant text. Success sets
		// structuredResult and breaks; failure either retries with a
		// reminder (carrying the validation error) or, at the cap, emits an
		// error frame and breaks.
		validated, verr := ValidateStructuredOutput(assistantText, req.OutputSchema)
		if verr == nil {
			structuredResult = validated
			break
		}
		lastVErr = verr
		if attempt == retryCap {
			writeSSEFrame(w, fl, proto.NewError("output did not match the required schema after "+
				strconv.Itoa(attempt+1)+" attempt(s): "+verr.Error()))
			break
		}
		prevAssistantText = assistantText
	}

	// A12-core: emit the validated structured result before status/done so
	// a schema-constrained consumer (exec --output-schema, API client, later
	// the TUI) can take the parsed JSON without re-parsing the stream.
	// Skipped on text-mode turns (structuredResult stays nil).
	if structuredResult != nil {
		writeSSEFrame(w, fl, proto.NewStructuredResult(structuredResult))
	}
	// Emit a status frame with the selection + usage before terminating so
	// the client can update its model indicator and /cost from either
	// transport. turns is always 1 for a stateless SSE request. Cached /
	// Reasoning (Task A6) are populated post-construction so NewStatus's
	// signature stays unchanged; the SSE and WS paths emit the same fields.
	sseStatus := proto.NewStatus(req.Model, req.Thinking, usage.PromptTokens, usage.CompletionTokens, 1, contextWindowFor(req.Model, s.compaction))
	sseStatus.CachedTokens = usage.CachedTokens
	sseStatus.ReasoningTokens = usage.ReasoningTokens
	writeSSEFrame(w, fl, sseStatus)
	writeSSEFrame(w, fl, proto.NewDone())
}
```

3b. 上面的完整 method 已把 lifecycle 插在 `msgs` 构造之后、compaction 之前；最终代码中必须只出现这一对调用：

```go
	s.sealTurnBoundary("", 1, len(msgs),
		string(vcs.SeamPreTurn), "sse:pre-turn:1")
	defer s.sealTurnBoundary("", 1, len(msgs),
		string(vcs.SeamPostTurn), "sse:post-turn:1")
```

`defer` 的参数在注册时求值，因此 post seam 使用进入 compaction/runner 前的同一 request message count；从注册点之后的 model error、request cancel、schema cap、正常返回与 panic 都走 post seam。不得在 attempt loop 后再添加第二个普通 post 调用。

3c. chat.go 顶部加 import(若未存在):

```go
"github.com/x6nux/yanshi/internal/vcs"
```

- [ ] **Step 4: 修改 `internal/cli/backend.go` — `StreamEvent` 加 seam 字段**

在 `StreamEvent.StructuredResult` 字段后插入:

```go
	// Seam fields (B2-RB1): populated by toStreamEvent for seams /
	// seam_restored control replies.
	Seams       []proto.SeamInfo
	CommitShort string // display-only
	Head        string // D6 full main_head binding
	UndoSeamID  string // seam_restored ID
```

- [ ] **Step 5: 修改 `internal/cli/wsbackend.go`**

5a. `isControlReply` 加 seam 帧类型:

```go
func isControlReply(kind string) bool {
	switch kind {
	case "models", "status", "mcp_list", "sessions", "session_restored", "session_ack",
		"seams", "seam_restored": // NEW (B2-RB1 I)
		return true
	}
	return false
}
```

5b. 用以下完整实现替换 `toStreamEvent`:

```go
func toStreamEvent(f proto.ServerFrame) StreamEvent {
	items := f.Names
	if items == nil {
		items = f.Servers
	}
	msgs := make([]MessageStub, 0, len(f.Messages))
	for _, m := range f.Messages {
		msgs = append(msgs, MessageStub{Role: string(m.Role), Content: m.Content})
	}
	ev := StreamEvent{
		Kind: f.Type, Text: f.Text, ToolName: f.ToolName, ToolArgs: f.ToolArgs,
		ToolStatus: f.Status, Overwrite: f.Overwrite, ID: f.ID,
		Reason: f.Reason, Model: f.Model, Thinking: f.Thinking,
		PermMode: f.PermMode, AutoThreshold: f.AutoThreshold,
		TokensIn: f.TokensIn, TokensOut: f.TokensOut,
		CachedTokens: f.CachedTokens, ReasoningTokens: f.ReasoningTokens,
		Turns: f.Turns, ContextWindow: f.ContextWindow, Items: items,
		Compacted: f.Compacted, TokensBefore: f.TokensBefore,
		TokensAfter: f.TokensAfter, RetryAttempt: f.RetryAttempt,
		RetryMax: f.RetryMax, RetryDelayMs: f.RetryDelayMs,
		Sessions: f.Sessions, SessionID: f.SessionID, Action: f.Action,
		Messages: msgs, StructuredResult: f.StructuredResult,
		Seams: f.Seams, CommitShort: f.CommitShort, Head: f.Head,
	}
	if f.Type == "seam_restored" {
		ev.UndoSeamID = f.ID
	}
	return ev
}
```

- [ ] **Step 6: `internal/cli/ssebackend.go` — D7: 不添加 `seam_restored` case**

SSE 上 `revert_turn` agent 工具经 `RequireApproval` fail-closed(无交互 callback),所以 `seam_restored` 帧**永远不会出现在 SSE 流里**。`flush()` 的 switch 保持原样(`history_replaced` / `agent_chunk` / `structured_result` / `default`),**不**新增 `case "seam_restored"`(D7:这是 dead code;添加它会误导 reviewer 以为 SSE 支持回滚,且 CLAUDE.md 的"新帧同时改 ws.go 与 ssebackend.go"指的是会发生 on-SSE 的帧,而 seam_restored 不在此列)。

本 Step 的确定结果是 `internal/cli/ssebackend.go` **不进入本次 diff**：`seam_restored` constructor 由 WS production path 引用，不存在 unused 问题。Step 8 的 `git add` 也不包含该文件；review 时若 diff 出现对 `ssebackend.go` 的 `seam_restored` 分支，必须删除后才能进入 GREEN。

- [ ] **Step 7: 运行全部测试并确认通过**

```bash
go test ./internal/api/http/ -run TestSSE_PreAndPostTurnSeamsCreated -v
go test ./internal/cli/ -run 'TestIsControlReply_SeamFrames|TestToStreamEvent_Seams' -v
```

预期:两条命令均 PASS。

- [ ] **Step 8: 提交**

```bash
git add internal/api/http/chat.go internal/api/http/sse_seam_test.go internal/cli/backend.go internal/cli/wsbackend.go internal/cli/wsbackend_seam_test.go
git commit -m "feat(api/cli): add SSE seam lifecycle and WS client seam mapping (B2-RB1 D/I)"
```

---

## Task 11: tools — `RequireApproval` + `revert_turn` agent tool

**Files:**
- Modify: `internal/tools/permctx.go`(给 `PermissionRequest` 加 `Force` 并实现 `RequireApproval`)
- Modify: `internal/tools/vcs.go`(加 `Revert *GuardedTool` + `runRevert` + 注册到 `Tools()`)
- Test: `internal/tools/require_approval_test.go`(新建)
- Test: `internal/tools/vcs_revert_test.go`(新建)

> 必修项 E + K。`revert_turn` 是 destructive 工具,即使静态 profile 允许也必须强制 `PermissionRequest`(因为"用户通过 yolo / allow-edits / always_allow 进入会话"不等于"用户允许这次具体的回滚")。`tools.RequireApproval` 是新增的强制提示 API:
>
> - 不查 session allowlist(`always_allow` 不能永久跳过)。
> - 不查静态 profile(静态允许也强制提示)。
> - 无 callback(SSE / 静态路径)→ 直接 `*DenyErr{Reason: "destructive action requires interactive approval"}`。
> - callback 返回 `PermissionDeny` 或未知值 → `*DenyErr`。
> - callback 返回 `PermissionAllow` → nil(放行)。
> - callback 返回 `PermissionAlwaysAllow` → 仍 nil(放行本次,但不写入 allowlist,下次仍提示)。
>
> `revert_turn` 工具调用 `RequireApproval`,然后调用公开 `RevertToSeam`；VCS 自己在该公开入口获取 repo 锁。Worktree scope 必须拒绝(worktree commit 不是 main commit,不是 RB1 的回滚入口)。Profile 测试必须显式 `Allow: []string{"revert_turn"}`(`vcs_*` glob 不匹配)。

- [ ] **Step 1: 写失败测试** — `internal/tools/require_approval_test.go`(新建)

```go
// internal/tools/require_approval_test.go
package tools

import (
	"context"
	"errors"
	"testing"

	"github.com/x6nux/yanshi/internal/guard"
)

// TestRequireApproval_NoCallbackDenies verifies the fail-closed path: with no
// callback bound (the SSE / static path), RequireApproval returns *DenyErr
// regardless of the static profile.
func TestRequireApproval_NoCallbackDenies(t *testing.T) {
	prof := testProfileAllowAll() // static profile that allows everything
	ctx := WithProfile(context.Background(), prof)
	// No WithPermissionCallback — simulates SSE.
	err := RequireApproval(ctx, PermissionRequest{Tool: "revert_turn", Reason: "destructive"})
	if err == nil {
		t.Fatal("RequireApproval 应当无 callback 时 deny")
	}
	var de *DenyErr
	if !errors.As(err, &de) {
		t.Errorf("返回类型 %T,想要 *DenyErr", err)
	}
}

// TestRequireApproval_CallbackAllowPasses verifies the happy path: a callback
// that returns PermissionAllow lets the action through.
func TestRequireApproval_CallbackAllowPasses(t *testing.T) {
	prof := testProfileAllowAll()
	ctx := WithProfile(context.Background(), prof)
	ctx = WithPermissionCallback(ctx, func(req PermissionRequest) PermissionDecision {
		if req.Tool != "revert_turn" {
			t.Errorf("callback got Tool=%q, want revert_turn", req.Tool)
		}
		return PermissionAllow
	})
	if err := RequireApproval(ctx, PermissionRequest{Tool: "revert_turn"}); err != nil {
		t.Errorf("allow: got %v, want nil", err)
	}
}

// TestRequireApproval_CallbackReceivesForce proves RequireApproval itself marks
// every destructive request as Force, even when the caller omitted the field.
// This is the contract the WS permission callback uses to bypass yolo / auto /
// allow-edits auto-resolution.
func TestRequireApproval_CallbackReceivesForce(t *testing.T) {
	prof := testProfileAllowAll()
	ctx := WithProfile(context.Background(), prof)
	ctx = WithPermissionCallback(ctx, func(req PermissionRequest) PermissionDecision {
		if !req.Force {
			t.Error("RequireApproval callback received Force=false, want true")
		}
		return PermissionAllow
	})
	if err := RequireApproval(ctx, PermissionRequest{Tool: "revert_turn"}); err != nil {
		t.Fatalf("RequireApproval: %v", err)
	}
}

// TestRequireApproval_CallbackDenyFails verifies Deny → *DenyErr.
func TestRequireApproval_CallbackDenyFails(t *testing.T) {
	prof := testProfileAllowAll()
	ctx := WithProfile(context.Background(), prof)
	ctx = WithPermissionCallback(ctx, func(req PermissionRequest) PermissionDecision {
		return PermissionDeny
	})
	err := RequireApproval(ctx, PermissionRequest{Tool: "revert_turn"})
	if err == nil {
		t.Fatal("RequireApproval 应当 deny")
	}
}

// TestRequireApproval_AlwaysAllowDoesNotStick verifies that AlwaysAllow for a
// forced prompt does NOT record into the session allowlist — the next call
// must STILL prompt. 必修项 E: "不能被 prior always_allow 永久跳过".
func TestRequireApproval_AlwaysAllowDoesNotStick(t *testing.T) {
	prof := testProfileAllowAll()
	ctx := WithProfile(context.Background(), prof)
	calls := 0
	cb := func(req PermissionRequest) PermissionDecision {
		calls++
		return PermissionAlwaysAllow
	}
	ctx = WithPermissionCallback(ctx, cb)
	// First call: AlwaysAllow — passes.
	_ = RequireApproval(ctx, PermissionRequest{Tool: "revert_turn"})
	// Second call: callback must STILL fire (not skipped via allowlist).
	_ = RequireApproval(ctx, PermissionRequest{Tool: "revert_turn"})
	if calls != 2 {
		t.Errorf("AlwaysAllow 应当不持久化;calls=%d, want 2", calls)
	}
}

// testProfileAllowAll returns a permissive profile value for RequireApproval tests
// (the point is that RequireApproval MUST prompt even when the profile allows).
// Uses the repository's real guard.PermissionProfile and guard.ToolsPerm
// contracts (CB7).
func testProfileAllowAll() guard.PermissionProfile {
	return guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
	}
}
```

> 注:这里直接构造真实值类型 `guard.PermissionProfile` + `guard.ToolsPerm{Allow: []string{"*"}}`(CB7)，与 `WithProfile(ctx, p guard.PermissionProfile)` 的真实签名一致。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/tools/ -run TestRequireApproval -v`
Expected: 编译失败(`RequireApproval` 未定义)

- [ ] **Step 3: 在 `internal/tools/permctx.go` 加 `Force` 字段 + `RequireApproval`**

3a. 给现有的 `PermissionRequest` struct 加 `Force bool` 字段(D3):

```go
// In permctx.go, append to the existing PermissionRequest struct:
type PermissionRequest struct {
	Tool   string
	Args   string
	Reason string
	// Force marks a destructive action that must ALWAYS prompt, even under
	// yolo / allow-edits / auto interactive mode (D3). The WS-installed permission
	// callback reads this: when true it skips mode resolution and emits the
	// interactive prompt unconditionally.
	Force bool
}
```

3b. 加 `RequireApproval`:

```go
// RequireApproval forces a permission prompt for destructive actions
// (revert_turn, future: shell_run with rm, etc.) REGARDLESS of the static
// profile or session allowlist. 必修项 E.
//
// Semantics:
//   - No profile bound              -> DenyErr (fail-closed, same as Authorize).
//   - No callback bound (SSE/static) -> DenyErr (destructive actions cannot
//     be approved without an interactive prompt).
//   - Callback returns PermissionAllow       -> nil (this one call proceeds).
//   - Callback returns PermissionAlwaysAllow -> nil (this one call proceeds;
//     the session allowlist is NOT updated, so the NEXT call still prompts —
//     destructive actions must NEVER be sticky-approved).
//   - Callback returns PermissionDeny / unknown -> DenyErr.
//   - Callback blocked by cancel / timeout -> the callback itself returns
//     PermissionDeny (the WS handler installs a callback that does this), so
//     RequireApproval surfaces DenyErr — fail-closed.
//
// argsJSON / reason are carried in the PermissionRequest for the callback's
// display (the WS handler builds the TUI prompt from them).
func RequireApproval(ctx context.Context, req PermissionRequest) error {
	if _, ok := ProfileFromContext(ctx); !ok {
		return &DenyErr{Reason: "no permission profile in context"}
	}
	ask, hasCallback := permissionCallback(ctx)
	if !hasCallback {
		return &DenyErr{Reason: "destructive action requires interactive approval (no callback bound)"}
	}
	// D3: mark the request as Force so the WS-installed callback knows to SKIP
	// interactive-mode auto-resolution (yolo / allow-edits / auto). The callback
	// reads req.Force and, when true, always emits an interactive prompt instead
	// of resolving the mode first.
	req.Force = true
	switch ask(req) {
	case PermissionAllow, PermissionAlwaysAllow:
		return nil
	default:
		return &DenyErr{Reason: req.Reason}
	}
}
```

> 注:不调用 `allowlistFrom(ctx).record(...)` —— 这是"非 sticky"语义的核心。每次 revert_turn 调用都强制提示。
>
> **D3 — WS callback 必须尊重 `req.Force`**(Task 9 已定义精确 gate):callback 先调用 `resolvePermissionRequest`;该 helper 对 `Force=true` 返回 `(PermissionDeny, false)`，使 caller 跳过 `resolvePermissionMode` 的 yolo/auto/allow-edits 自动放行并进入现有 `permission_request` 交互等待分支。不得用临时改写 mode 或依赖静态 profile。Task 9 的 `TestResolvePermissionRequest_ForceNeverAutoResolves` 遍历三种自动模式；本 Task 的 `TestRequireApproval_CallbackReceivesForce` 锁定上游一定设置 `Force=true`。

- [ ] **Step 4: 写 revert_turn 工具测试** — `internal/tools/vcs_revert_test.go`(新建)

```go
// internal/tools/vcs_revert_test.go
package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/vcs"
)

// newRevertTestRepo wraps newVCSTestRepo (existing helper in fs_test.go) and
// returns the seeded VCS / repoID / root path.
func newRevertTestRepo(t *testing.T) (*vcs.VCS, string, string) {
	t.Helper()
	v, repoID, root := newVCSTestRepo(t)
	return v, repoID, root
}

// allowRevertProfile returns a static profile whose Tools.Allow contains
// "revert_turn" explicitly (vcs_* glob does NOT match — 必修项 K). Uses the REAL
// guard types guard.PermissionProfile / guard.ToolsPerm (CB7).
func allowRevertProfile() guard.PermissionProfile {
	return guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"vcs_*", "revert_turn", "fs_*"}},
	}
}

// TestRevertTool_RegisteredInTools confirms NewVCSTools().Tools() includes
// revert_turn (otherwise bootstrap's auto-registration would miss it).
func TestRevertTool_RegisteredInTools(t *testing.T) {
	tt := NewVCSTools()
	found := false
	for _, tool := range tt.Tools() {
		if tool.Name() == "revert_turn" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("revert_turn not registered in VCSTools.Tools()")
	}
	if tt.Revert == nil {
		t.Error("VCSTools.Revert field is nil")
	}
}

// TestRevertTool_NoApprovalDeniesMainScope verifies the fail-closed path: with
// no interactive callback bound (SSE-style), runRevert returns a *DenyErr as
// its tool result (not a Go error — GuardedTool converts to result).
func TestRevertTool_NoApprovalDeniesMainScope(t *testing.T) {
	v, repoID, root := newRevertTestRepo(t)
	// Seed a pre-turn seam.
	aPath := filepath.Join(root, "a.go")
	require.NoError(t, os.WriteFile(aPath, []byte("v1"), 0o644))
	if err := v.RecordEditMain(repoID, "u", aPath, []byte("v1")); err != nil {
		t.Fatal(err)
	}
	if _, err := v.CommitMain(repoID, "u", "v1"); err != nil {
		t.Fatal(err)
	}
	preID, err := v.SealMainTurnSeam(repoID, "s1", 0, 0, vcs.SeamPreTurn, "pre")
	if err != nil {
		t.Fatal(err)
	}
	require.NoError(t, os.WriteFile(aPath, []byte("v2"), 0o644))
	if err := v.RecordEditMain(repoID, "u", aPath, []byte("v2")); err != nil {
		t.Fatal(err)
	}
	if _, err := v.CommitMain(repoID, "u", "v2"); err != nil {
		t.Fatal(err)
	}

	tt := NewVCSTools()
	ctx := WithProfile(context.Background(), allowRevertProfile())
	ctx = WithVCS(ctx, VCSScope{VCS: v, RepoID: repoID, Agent: "test"})
	args, _ := json.Marshal(map[string]string{"seam_id": preID})
	result, err := runTool(ctx, tt.Revert, string(args))
	if err != nil {
		t.Fatalf("runTool err: %v", err)
	}
	// Without callback the result text must contain "destructive" / "denied".
	if !strings.Contains(result, "destructive") && !strings.Contains(result, "denied") {
		t.Errorf("expected deny message, got %q", result)
	}
	// File must NOT be reverted.
	got, _ := os.ReadFile(aPath)
	if string(got) != "v2" {
		t.Errorf("without approval a.go was modified: %q", got)
	}
}

// TestRevertTool_ApprovalRevertsMainScope verifies the happy path: with a
// callback returning PermissionAllow, runRevert invokes RevertToSeam and the
// file is reverted.
func TestRevertTool_ApprovalRevertsMainScope(t *testing.T) {
	v, repoID, root := newRevertTestRepo(t)
	aPath := filepath.Join(root, "a.go")
	require.NoError(t, os.WriteFile(aPath, []byte("v1"), 0o644))
	if err := v.RecordEditMain(repoID, "u", aPath, []byte("v1")); err != nil {
		t.Fatal(err)
	}
	if _, err := v.CommitMain(repoID, "u", "v1"); err != nil {
		t.Fatal(err)
	}
	preID, err := v.SealMainTurnSeam(repoID, "s1", 0, 0, vcs.SeamPreTurn, "pre")
	if err != nil {
		t.Fatal(err)
	}
	require.NoError(t, os.WriteFile(aPath, []byte("v2"), 0o644))
	if err := v.RecordEditMain(repoID, "u", aPath, []byte("v2")); err != nil {
		t.Fatal(err)
	}
	if _, err := v.CommitMain(repoID, "u", "v2"); err != nil {
		t.Fatal(err)
	}

	tt := NewVCSTools()
	ctx := WithProfile(context.Background(), allowRevertProfile())
	ctx = WithVCS(ctx, VCSScope{VCS: v, RepoID: repoID, Agent: "test"})
	ctx = WithPermissionCallback(ctx, func(req PermissionRequest) PermissionDecision {
		if req.Tool != "revert_turn" {
			t.Errorf("callback Tool=%q, want revert_turn", req.Tool)
		}
		return PermissionAllow
	})
	args, _ := json.Marshal(map[string]string{"seam_id": preID})
	result, err := runTool(ctx, tt.Revert, string(args))
	if err != nil {
		t.Fatalf("runTool err: %v", err)
	}
	// The result JSON contains the undo seam id.
	if !strings.Contains(result, "undo") {
		t.Errorf("result missing undo field: %q", result)
	}
	got, _ := os.ReadFile(aPath)
	if string(got) != "v1" {
		t.Errorf("after approval a.go = %q, want v1(reverted)", got)
	}
}

// TestRevertTool_RejectsWorktreeScope verifies that when VCSScope.WorktreeID
// is set, runRevert returns a denial (worktree commits are not RB1 rollback
// targets).
func TestRevertTool_RejectsWorktreeScope(t *testing.T) {
	v, repoID, root := newRevertTestRepo(t)
	wt, err := v.AddWorktree(repoID, []string{"test"})
	if err != nil {
		t.Fatalf("AddWorktree: %v", err)
	}
	tt := NewVCSTools()
	ctx := WithProfile(context.Background(), allowRevertProfile())
	ctx = WithVCS(ctx, VCSScope{VCS: v, RepoID: repoID, WorktreeID: wt.ID, Agent: "test"})
	ctx = WithPermissionCallback(ctx, func(req PermissionRequest) PermissionDecision {
		return PermissionAllow // even with allow, worktree must reject
	})
	args, _ := json.Marshal(map[string]string{"seam_id": "any"})
	result, _ := runTool(ctx, tt.Revert, string(args))
	if !strings.Contains(result, "worktree") && !strings.Contains(result, "main only") {
		t.Errorf("expected worktree reject message, got %q", result)
	}
}

// TestRevertTool_DenyDoesNotModify verifies that a callback Deny leaves the
// working copy untouched.
func TestRevertTool_DenyDoesNotModify(t *testing.T) {
	v, repoID, root := newRevertTestRepo(t)
	aPath := filepath.Join(root, "a.go")
	require.NoError(t, os.WriteFile(aPath, []byte("v1"), 0o644))
	if err := v.RecordEditMain(repoID, "u", aPath, []byte("v1")); err != nil {
		t.Fatal(err)
	}
	if _, err := v.CommitMain(repoID, "u", "v1"); err != nil {
		t.Fatal(err)
	}
	preID, err := v.SealMainTurnSeam(repoID, "s1", 0, 0, vcs.SeamPreTurn, "pre")
	if err != nil {
		t.Fatal(err)
	}
	require.NoError(t, os.WriteFile(aPath, []byte("v2"), 0o644))
	if err := v.RecordEditMain(repoID, "u", aPath, []byte("v2")); err != nil {
		t.Fatal(err)
	}
	if _, err := v.CommitMain(repoID, "u", "v2"); err != nil {
		t.Fatal(err)
	}
	tt := NewVCSTools()
	ctx := WithProfile(context.Background(), allowRevertProfile())
	ctx = WithVCS(ctx, VCSScope{VCS: v, RepoID: repoID, Agent: "test"})
	ctx = WithPermissionCallback(ctx, func(req PermissionRequest) PermissionDecision {
		return PermissionDeny
	})
	args, _ := json.Marshal(map[string]string{"seam_id": preID})
	_, _ = runTool(ctx, tt.Revert, string(args))
	got, _ := os.ReadFile(aPath)
	if string(got) != "v2" {
		t.Errorf("Deny should leave a.go as v2, got %q", got)
	}
}
```

- [ ] **Step 5: 跑测试确认失败**

Run: `go test ./internal/tools/ -run 'TestRevertTool|TestRequireApproval' -v`
Expected: 编译失败(`tt.Revert` 字段与 `runRevert` 尚未定义；`RequireApproval` 已由 Step 3 实现)

- [ ] **Step 6: 修改 `internal/tools/vcs.go`**

6a. `VCSTools` 加 `Revert` 字段:

```go
type VCSTools struct {
	Commit  *GuardedTool
	Log     *GuardedTool
	Diff    *GuardedTool
	Restore *GuardedTool
	Merge   *GuardedTool
	Revert  *GuardedTool // NEW (B2-RB1)
}
```

6b. `NewVCSTools` 构造 `Revert`:

```go
	t.Revert = NewGuardedTool(
		"revert_turn", "Revert Turn",
		"Revert the main working copy and VCS head to a prior turn seam. This agent tool is VCS-only; use WS /restore-turn to restore conversation history too. Destructive: always prompts even in yolo/allow-edits mode.",
		60*time.Second,
		params(map[string]*schema.ParameterInfo{
			"seam_id": {Type: schema.String, Desc: "seam id (from vcs_log or /restore-turn)", Required: true},
		}),
		SyncStream(t.runRevert),
	)
```

6c. `Tools()` 加 `Revert`:

```go
func (t *VCSTools) Tools() []*GuardedTool {
	return []*GuardedTool{t.Commit, t.Log, t.Diff, t.Restore, t.Merge, t.Revert}
}
```

6d. 加 `runRevert` 方法:

```go
type vcsRevertArgs struct {
	SeamID string `json:"seam_id"`
}

// runRevert is the revert_turn tool core. It:
//   1. Resolves the VCS scope + rejects worktree scope (RB1 is main-only).
//   2. Calls tools.RequireApproval — forced prompt, fail-closed on no-callback
//      (SSE) / Deny / timeout. 必修项 E.
//   3. Invokes VCS.RevertToSeam, which materializes the target tree + inserts
//      an undo seam (pointing at the PRE-revert head) atomically.
//   4. Returns the undo seam id as JSON (so the model / TUI can offer "undo").
//
// Worktree scope is rejected at the tool layer (not inside VCS) so the model
// gets a clear result message; VCS.MaterializeMain would also reject (the
// seam's commit is a main commit, not the worktree's head), but the tool-level
// check produces a friendlier message.
func (t *VCSTools) runRevert(ctx context.Context, argsJSON string) (string, error) {
	var a vcsRevertArgs
	if err := ParseArgs(argsJSON, &a); err != nil {
		return "", err
	}
	sc, err := vcsScopeFromCtx(ctx)
	if err != nil {
		return "", err
	}
	if sc.WorktreeID != "" {
		return toJSON(map[string]string{
			"error": "revert_turn operates on main only; worktree commits are not rollback targets",
		}), nil
	}
	// Forced destructive approval. 必修项 E + D3: Force=true tells the WS
	// permission callback to SKIP interactive-mode auto-resolution (yolo /
	// allow-edits / auto would otherwise silently allow this destructive op).
	if err := RequireApproval(ctx, PermissionRequest{
		Tool:   "revert_turn",
		Args:   argsJSON,
		Reason: "revert main working copy + VCS head to a prior turn (agent path does not restore chat history)",
		Force:  true,
	}); err != nil {
		return toJSON(map[string]string{
			"error":  "revert_turn denied: " + err.Error(),
			"denied": "true",
		}), nil
	}
	// Agent path passes 0,0,nil: the agent tool does NOT own WS conversation
	// history, so it cannot truncate/restore it and stores no history_snapshot on
	// the returned seam (D4 scope cut: agent revert_turn is VCS-only).
	undoID, err := sc.VCS.RevertToSeam(sc.RepoID, a.SeamID, "agent:"+sc.Agent, 0, 0, nil)
	if err != nil {
		return "", err
	}
	return toJSON(map[string]string{
		"undo_seam_id": undoID,
		"hint":         "call revert_turn again with seam_id=" + undoID + " to undo this revert",
	}), nil
}
```

> 注:`toJSON` 已在 vcs.go 中存在(其他 run* 方法使用)。`RequireApproval` / `PermissionRequest` 在同包 `permctx.go` 中。`sync` 包的 `SyncStream` 已被同文件其他工具使用。

- [ ] **Step 7: 跑测试确认通过**

Run: `go test ./internal/tools/ -run 'TestRevertTool|TestRequireApproval' -v`
Expected: PASS

- [ ] **Step 8: 提交**

```bash
git add internal/tools/permctx.go internal/tools/require_approval_test.go internal/tools/vcs.go internal/tools/vcs_revert_test.go
git commit -m "feat(tools): add RequireApproval + revert_turn agent tool (forced prompt, fail-closed) (B2-RB1 E/K)"
```

---
## Task 12: TUI — `/restore-turn` 命令 + applyEvent seam 映射 + 错误清 pending

**Files:**
- Modify: `internal/cli/tui/model.go`(`model` 加字段 + `applyEvent` 加 case)
- Modify: `internal/cli/tui/commands.go`(`commandTable` + `cmdRestoreTurn`)
- Modify: `internal/cli/tui/entries.go`（`seamsEntry` / `seamRestorePromptEntry` / `seamRestoredEntry` + 加 `proto` import）
- Test: `internal/cli/tui/restore_turn_test.go`(新建)

> 必修项 I + J。TUI 新增 `/restore-turn` 命令(三态:无参→列表、`<id>`→显示提示、`<id> yes`→发送 `restore_turn` 帧),`applyEvent` 处理 `seams` / `seam_restored` / `error` 分支(后者清 `pendingSeamRestore`)。UI 展示 `pre-turn` 与可撤销前一次回滚的 `pre-revert` seam;隐藏纯 audit/redo 的 `post-turn` / `post-revert`。严格使用真实 `command{name,help,run}`、`entry.render(width,spinner)`、`m.sess.Mode()` 与 full `StreamEvent.Head`。

- [ ] **Step 1: 写失败测试** — `internal/cli/tui/restore_turn_test.go`(新建)

```go
// internal/cli/tui/restore_turn_test.go
package tui

import (
	"testing"

	"github.com/x6nux/yanshi/internal/cli"
	"github.com/x6nux/yanshi/internal/proto"
)

// TestRestoreTurnCommand_NoArgs_ListsSeams verifies the no-arg form sends a
// list_seams control frame (via sendControlFrame). CB4: cmdRestoreTurn is a
// package-level func(m model, args []string) (tea.Model, tea.Cmd).
func TestRestoreTurnCommand_NoArgs_ListsSeams(t *testing.T) {
	rs := &recordingSession{}
	m := newModel(rs, "/proj")
	cmdRestoreTurn(m, []string{})
	if len(rs.frames) != 1 {
		t.Fatalf("expected 1 frame sent, got %d", len(rs.frames))
	}
	if rs.frames[0].Type != "list_seams" {
		t.Errorf("frame.Type = %q, want list_seams", rs.frames[0].Type)
	}
}

// TestRestoreTurnCommand_WithIDAndYes_SendsRestoreFrame verifies the
// "<id> yes" form sends a restore_turn frame with the seam id + the cached
// confirmed_head binding (D6: full id).
func TestRestoreTurnCommand_WithIDAndYes_SendsRestoreFrame(t *testing.T) {
	rs := &recordingSession{}
	m := newModel(rs, "/proj")
	m.lastKnownHead = "fullheadabcdef0123456789"
	cmdRestoreTurn(m, []string{"seam-1", "yes"})
	if len(rs.frames) != 1 {
		t.Fatalf("expected 1 frame, got %d", len(rs.frames))
	}
	f := rs.frames[0]
	if f.Type != "restore_turn" {
		t.Errorf("Type = %q, want restore_turn", f.Type)
	}
	if f.ID != "seam-1" {
		t.Errorf("ID = %q, want seam-1", f.ID)
	}
	if f.ConfirmedHead != "fullheadabcdef0123456789" {
		t.Errorf("ConfirmedHead = %q, want full id", f.ConfirmedHead)
	}
}

// TestRestoreTurnCommand_WithIDOnly_ShowsPrompt verifies the "<id>" form
// (without yes) shows an in-TUI confirmation prompt and does NOT send a frame.
func TestRestoreTurnCommand_WithIDOnly_ShowsPrompt(t *testing.T) {
	rs := &recordingSession{}
	m := newModel(rs, "/proj")
	out, _ := cmdRestoreTurn(m, []string{"seam-1"})
	got, ok := out.(model)
	if !ok {
		t.Fatalf("cmdRestoreTurn returned %T, want model", out)
	}
	if len(rs.frames) != 0 {
		t.Errorf("no frame should be sent on single-arg form; got %d", len(rs.frames))
	}
	foundPrompt := false
	for _, entry := range got.entries {
		if _, ok := entry.(seamRestorePromptEntry); ok {
			foundPrompt = true
			break
		}
	}
	if !foundPrompt {
		t.Fatal("returned model must append seamRestorePromptEntry")
	}
}

// TestApplyEvent_SeamsPopulatesEntry verifies the seams reply fills the
// pending entry + caches the full head for the next restore (D6: Head, not
// CommitShort).
func TestApplyEvent_SeamsPopulatesEntry(t *testing.T) {
	rs := &recordingSession{}
	m := newModel(rs, "/proj")
	seamsEv := cli.StreamEvent{
		Kind: "seams",
		Seams: []proto.SeamInfo{
			{ID: "s1", Kind: "pre-turn", CommitShort: "abc12345", Label: "pre-turn:1"},
		},
		CommitShort: "abc12345",
		Head:        "fullheadabcdef0123456789",
	}
	m = m.applyEvent(seamsEv)
	if m.lastKnownHead != "fullheadabcdef0123456789" {
		t.Errorf("lastKnownHead = %q, want full id", m.lastKnownHead)
	}
	// An entry should have been appended.
	foundSeamsEntry := false
	for _, e := range m.entries {
		if _, ok := e.(seamsEntry); ok {
			foundSeamsEntry = true
			break
		}
	}
	if !foundSeamsEntry {
		t.Error("seamsEntry not appended to entries")
	}
}

// TestApplyEvent_SeamRestoredClearsPending verifies seam_restored resolves the
// pending restore entry.
func TestApplyEvent_SeamRestoredClearsPending(t *testing.T) {
	rs := &recordingSession{}
	m := newModel(rs, "/proj")
	m.pendingSeamRestore = &pendingSeamRestoreState{seamID: "s1"}
	ev := cli.StreamEvent{
		Kind: "seam_restored", UndoSeamID: "undo-x", Text: "ok",
		Head: "full-post-revert-head-0123456789",
	}
	m = m.applyEvent(ev)
	if m.pendingSeamRestore != nil {
		t.Error("pendingSeamRestore should be cleared after seam_restored")
	}
	if m.lastKnownHead != ev.Head {
		t.Errorf("lastKnownHead = %q, want seam_restored full Head %q", m.lastKnownHead, ev.Head)
	}
}

// TestApplyEvent_ErrorClearsPendingSeamRestore verifies the error branch clears
// the pending pointer (必修项 I: 不留下永久 reverting... pending 状态).
func TestApplyEvent_ErrorClearsPendingSeamRestore(t *testing.T) {
	rs := &recordingSession{}
	m := newModel(rs, "/proj")
	m.pendingSeamRestore = &pendingSeamRestoreState{seamID: "s1"}
	ev := cli.StreamEvent{Kind: "error", Text: "head changed"}
	m = m.applyEvent(ev)
	if m.pendingSeamRestore != nil {
		t.Error("pendingSeamRestore should be cleared on error event")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/cli/tui/ -run 'TestRestoreTurnCommand|TestApplyEvent_Seams|TestApplyEvent_SeamRestored|TestApplyEvent_ErrorClearsPendingSeamRestore' -v`
Expected: 编译失败(`cmdRestoreTurn` / `pendingSeamRestoreState` / `lastKnownHead` / `seamsEntry` / `seamRestorePromptEntry` / `seamRestoredEntry` 未定义)。

- [ ] **Step 3: 修改 `internal/cli/tui/model.go`**

3a. 在 package scope、紧邻真实 `type model struct {` 之前新增状态类型：

```go
type pendingSeamRestoreState struct {
	seamID string
}
```

3b. 在真实 `type model struct { ... }` **内部**，把现有字段：

```go
	pendingStatsEntry *statsEntry
```

精确替换为以下连续三个 struct fields（不能把后两个字段贴到 package scope）：

```go
	pendingStatsEntry   *statsEntry
	pendingSeamRestore *pendingSeamRestoreState
	lastKnownHead      string // FULL ServerFrame.Head, never CommitShort
```

3c. 在 `applyEvent` 的 `switch ev.Kind`(现有 `case "error":` 分支)前面加 `seams` / `seam_restored` 分支,并修改 `error` 分支清 pending:

```go
	case "seams":
		// D6: cache the FULL security binding, not display-only CommitShort.
		m.lastKnownHead = ev.Head
		preTurn := make([]proto.SeamInfo, 0, len(ev.Seams))
		for _, s := range ev.Seams {
			if s.Kind == "pre-turn" || s.Kind == "pre-revert" {
				// pre-revert is the returned undo seam; expose it so the user can
				// undo a revert and recover PrevHistoryLen/PrevTurnSeq (D2).
				preTurn = append(preTurn, s)
			}
		}
		m.entries = append(m.entries, seamsEntry{items: preTurn})
	case "seam_restored":
		// D6: this control reply closes the request channel, so do not rely on a
		// later status frame to deliver the new binding. Cache its FULL Head now.
		if ev.Head != "" {
			m.lastKnownHead = ev.Head
		}
		// Resolve the pending restore entry with the undo seam id (so the UI
		// can show "reverted; undo with /restore-turn <undo-id> yes").
		undoID := ev.UndoSeamID
		text := ev.Text
		if m.pendingSeamRestore != nil {
			m.entries = append(m.entries, seamRestoredEntry{undoID: undoID, summary: text})
			m.pendingSeamRestore = nil
		} else {
			// Late seam_restored (rare); append as a standalone entry.
			m.entries = append(m.entries, seamRestoredEntry{undoID: undoID, summary: text})
		}
	case "error":
		m.flushAssistant()
		// Clear in-flight restore state on every server error; render the
		// existing errorEntry exactly once.
		if m.pendingSeamRestore != nil {
			m.pendingSeamRestore = nil
		}
		m.entries = append(m.entries, errorEntry{text: ev.Text})
```

3d. 用下面的完整分支替换 `applyEvent` 中现有 `case "status":`；保留真实的 `m.applyStatus(ev)` 调用，并在其后缓存 full head：

```go
	case "status":
		// Existing status behavior remains centralized in applyStatus.
		m.applyStatus(ev)
		if ev.Head != "" {
			m.lastKnownHead = ev.Head // D6 full binding, never CommitShort
		}
```

这样 `status` 的 model/thinking/usage/compaction 更新不被复制或丢失，同时 destructive confirmation 的下一次绑定立即刷新。

- [ ] **Step 4: 修改 `internal/cli/tui/commands.go`**

4a. `commandTable` 加 `restore-turn`(在 `delete` 附近)。**CB4: 真实 `command` struct 字段是 `name` / `help` / `run`**(不是 `cmd` / `handler` / `desc`):

```go
	{name: "restore-turn", help: "list main seams or revert to a prior turn", run: cmdRestoreTurn},
```

4b. 加 `cmdRestoreTurn` 函数(放 `cmdDelete` 附近)。**CB4：真实签名是 `func(m model, args []string) (tea.Model, tea.Cmd)`；通过 `m.sess.Mode()` 判断传输；`sendControlFrame` 自己返回 `(tea.Model, tea.Cmd)`，不要包 `[]tea.Cmd{}`**：

```go
// cmdRestoreTurn implements the /restore-turn slash command:
//   /restore-turn              — list recent pre-turn seams
//   /restore-turn <id>         — show a confirmation prompt
//   /restore-turn <id> yes     — send restore_turn (target-bound by m.lastKnownHead)
// Numeric N (by-offset) is NOT supported (必修项 J: selector is exact seam_id).
func cmdRestoreTurn(m model, args []string) (tea.Model, tea.Cmd) {
	// CB4: transport mode comes from the real tuiSession interface.
	if m.sess.Mode() == "sse" {
		m.entries = append(m.entries, errorEntry{
			text: "/restore-turn requires the WebSocket transport (SSE is stateless)",
		})
		m.refresh()
		return m, nil
	}
	switch len(args) {
	case 0:
		return m.sendControlFrame(proto.NewListSeams())
	case 1:
		m.entries = append(m.entries, seamRestorePromptEntry{seamID: args[0]})
		m.refresh()
		m.viewport.GotoBottom()
		return m, nil
	default:
		if len(args) != 2 || args[1] != "yes" {
			m.entries = append(m.entries, errorEntry{
				text: "usage: /restore-turn [<id> [yes]]",
			})
			m.refresh()
			return m, nil
		}
		if m.lastKnownHead == "" {
			m.entries = append(m.entries, errorEntry{
				text: "restore-turn has no full head binding; run /restore-turn first",
			})
			m.refresh()
			return m, nil
		}
		m.pendingSeamRestore = &pendingSeamRestoreState{seamID: args[0]}
		return m.sendControlFrame(proto.NewRestoreTurn(args[0], m.lastKnownHead))
	}
}
```

> 注:`cmdRestoreTurn` 是包级函数(与 `cmdDelete` 同模式),不是 `model` 的方法。测试里直接调用 `cmdRestoreTurn(m, args)`(不是 `m.cmdRestoreTurn(args)`)。

- [ ] **Step 5: 修改 `internal/cli/tui/entries.go`**

5a. 顶部加 import:

```go
"github.com/x6nux/yanshi/internal/proto"
```

5b. 加 entry 类型 + `render` 实现。**CB4: 真实 `entry` interface 是 `render(width int, sp spinner.Model) string`(小写、返回 string、不收 `io.Writer`/`*styles`);本代码使用现有包级样式 `toolMeta` / `okStyle`(不是 `st.toolMeta`)**:

```go
// seamRestorePromptEntry is a transcript entry (not mutable pending state).
// The user confirms with the exact seam id; numeric offsets are never accepted.
type seamRestorePromptEntry struct {
	seamID string
}

func (e seamRestorePromptEntry) render(_ int, _ spinner.Model) string {
	return fmt.Sprintf("%s\n%s %s\n",
		toolMeta.Render("  confirm revert to "+e.seamID),
		toolMeta.Render("  type"),
		okStyle.Render("/restore-turn "+e.seamID+" yes"),
	)
}

// seamsEntry renders recent reversible seams. pre-turn rows go backward; a
// pre-revert row is the durable undo capability returned by a prior restore.
type seamsEntry struct {
	items []proto.SeamInfo
}

func (e seamsEntry) render(_ int, _ spinner.Model) string {
	var b strings.Builder
	if len(e.items) == 0 {
		b.WriteString(toolMeta.Render("  (no reversible seams yet)") + "\n")
		return b.String()
	}
	b.WriteString(okStyle.Render("  recent reversible seams:") + "\n")
	for _, s := range e.items {
		label := s.Label
		if label == "" {
			label = "(no label)"
		}
		b.WriteString(fmt.Sprintf("    %s  %s  %s\n",
			toolMeta.Render(s.ID),
			toolMeta.Render("("+s.CommitShort+")"),
			label,
		))
	}
	b.WriteString(toolMeta.Render("  usage: /restore-turn <id> yes") + "\n")
	return b.String()
}

// seamRestoredEntry renders the post-restore confirmation with the undo hint.
type seamRestoredEntry struct {
	undoID  string
	summary string
}

func (e seamRestoredEntry) render(_ int, _ spinner.Model) string {
	var b strings.Builder
	b.WriteString(okStyle.Render("  ✓ restored") + " " + e.summary + "\n")
	if e.undoID != "" {
		b.WriteString(fmt.Sprintf("    %s  %s\n",
			toolMeta.Render("undo:"),
			okStyle.Render("/restore-turn "+e.undoID+" yes"),
		))
	}
	return b.String()
}
```

> 注:`entries.go` 顶部已 import `fmt` / `strings` / `github.com/charmbracelet/bubbles/spinner`;本 Task **只新增** import `"github.com/x6nux/yanshi/internal/proto"`。本代码实际使用的现有包级样式仅为 `toolMeta` / `okStyle`;不要新增或引用未使用的 `errStyle`。

- [ ] **Step 6: 跑测试确认通过**

Run: `go test ./internal/cli/tui/ -run 'TestRestoreTurnCommand|TestApplyEvent_Seams|TestApplyEvent_SeamRestored|TestApplyEvent_ErrorClearsPendingSeamRestore' -v`
Expected: PASS

- [ ] **Step 7: 跑全部 TUI 测试确认无回归**

Run: `go test ./internal/cli/tui/ -v`
Expected: PASS

- [ ] **Step 8: 提交**

```bash
git add internal/cli/tui/model.go internal/cli/tui/commands.go internal/cli/tui/entries.go internal/cli/tui/restore_turn_test.go
git commit -m "feat(tui): add /restore-turn command + seams/seam_restored event mapping + error clear pending (B2-RB1 I/J)"
```

---

## Task 13: 端到端集成测试(确定性,真实 VCS + 真实 WS wiring)

**Files:**
- Test: `internal/api/http/ws_rollback_integration_test.go`(新建)

> 必修项 K + CB6 + D2 + D5 + D6。测试使用真实 `New(Config{...})` → `ChatWS(o,nil,nil)` → `httptest.NewServer(s.Handler())`,真实 `vcs.VCS` 与 deterministic `einollm.FakeModel`。它验证两轮 seam、full-head binding、文件/head/pending 回滚、durable history 截断,以及再次 restore pre-revert undo seam 后文件与被删除的消息/turns 都恢复。

- [ ] **Step 1: RED — 写完整集成行为测试，先引用尚未定义的 harness**

创建 `internal/api/http/ws_rollback_integration_test.go`:

```go
package http

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/store"
	"github.com/x6nux/yanshi/internal/vcs"
)

// TestRB1_EndToEnd_Deterministic covers file/head rollback, durable truncation,
// full-head confirmation, and D2's history-aware undo seam round trip.
func TestRB1_EndToEnd_Deterministic(t *testing.T) {
	h := newRollbackIntegrationHarness(t) // GREEN step supplies real WS wiring

	// Seed is v0. Turn 1 seals pre/post seams at v0, then external deterministic
	// edit advances main to v1. Turn 2 seals at v1, then advance to v2.
	h.turn("first turn")
	h.advance("v1")
	h.turn("second turn")
	h.advance("v2")

	listed := h.listSeams()
	require.NotEmpty(t, listed.Head, "D6: list must bind the full main_head")
	v2Head, err := h.v.RepoMainHead(h.repoID)
	require.NoError(t, err)
	require.Equal(t, v2Head, listed.Head, "Head must be full id, not CommitShort")

	var pre1 proto.SeamInfo
	for i := len(listed.Seams) - 1; i >= 0; i-- { // newest-first list
		if listed.Seams[i].Kind == string(vcs.SeamPreTurn) {
			pre1 = listed.Seams[i]
			break
		}
	}
	require.NotEmpty(t, pre1.ID, "oldest pre-turn seam")
	target, err := h.v.FindSeam(pre1.ID)
	require.NoError(t, err)

	sessions, err := h.st.ListSessions(0)
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	sid := sessions[0].ID
	beforeMessages, err := h.st.Messages(sid)
	require.NoError(t, err)
	beforeMeta, err := h.st.GetSession(sid)
	require.NoError(t, err)
	require.NotNil(t, beforeMeta)
	require.Greater(t, len(beforeMessages), target.HistoryLen)

	// First restore: use the FULL list binding; the reply also carries the new
	// full head for the next destructive confirmation.
	restored := h.restore(pre1.ID, listed.Head)
	require.NotEmpty(t, restored.ID, "returned pre-revert undo seam id")
	require.NotEmpty(t, restored.Head, "seam_restored must return full current head")

	got, err := os.ReadFile(h.file)
	require.NoError(t, err)
	require.Equal(t, "v0", string(got))
	headAfter, err := h.v.RepoMainHead(h.repoID)
	require.NoError(t, err)
	require.Equal(t, target.CommitID, headAfter)
	require.Equal(t, headAfter, restored.Head)
	require.Empty(t, h.v.Uncommitted("main", h.repoID))

	truncated, err := h.st.Messages(sid)
	require.NoError(t, err)
	require.Len(t, truncated, target.HistoryLen)
	truncatedMeta, err := h.st.GetSession(sid)
	require.NoError(t, err)
	require.Equal(t, target.TurnSeq, truncatedMeta.Turns)

	// D2: the returned undo seam preserves both integer boundaries and the exact
	// durable snapshot that was deleted by the first restore.
	undo, err := h.v.FindSeam(restored.ID)
	require.NoError(t, err)
	require.Equal(t, vcs.SeamPreRevert, undo.Kind)
	require.Equal(t, len(beforeMessages), undo.PrevHistoryLen)
	require.Equal(t, beforeMeta.Turns, undo.PrevTurnSeq)
	undoSnap, err := store.DecodeSessionRevertSnapshot(undo.HistorySnapshot)
	require.NoError(t, err)
	require.Equal(t, beforeMessages, undoSnap.Messages)
	require.Equal(t, beforeMeta.Turns, undoSnap.Meta.Turns)

	// Restore the undo seam using the first reply's FULL head. This must expand
	// history from the durable blob (simple slicing cannot recover deleted rows).
	undone := h.restore(restored.ID, restored.Head)
	require.NotEmpty(t, undone.ID)
	require.Equal(t, v2Head, undone.Head)
	got, err = os.ReadFile(h.file)
	require.NoError(t, err)
	require.Equal(t, "v2", string(got))
	finalHead, err := h.v.RepoMainHead(h.repoID)
	require.NoError(t, err)
	require.Equal(t, v2Head, finalHead)

	afterUndoMessages, err := h.st.Messages(sid)
	require.NoError(t, err)
	require.Equal(t, beforeMessages, afterUndoMessages,
		"undo must restore exact ids/seq/content, not only message count")
	afterUndoMeta, err := h.st.GetSession(sid)
	require.NoError(t, err)
	require.Equal(t, beforeMeta.Turns, afterUndoMeta.Turns)
}
```

- [ ] **Step 2: 运行 RED，确认因 harness 缺失而失败**

Run: `go test ./internal/api/http/ -run TestRB1_EndToEnd_Deterministic -v -count=1`
Expected: 编译失败，`undefined: newRollbackIntegrationHarness`。这证明集成行为测试已被编译选中；不是通过 `t.Skip` 绕过。

- [ ] **Step 3: GREEN — 在同一 `_test.go` 文件补齐真实 harness**

把 import block 扩成以下完整集合:

```go
import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/store"
	"github.com/x6nux/yanshi/internal/vcs"
)
```

在测试后追加:

```go
type rollbackIntegrationHarness struct {
	t      *testing.T
	c      *websocket.Conn
	v      *vcs.VCS
	st     *store.Store
	repoID string
	file   string
}

func newRollbackIntegrationHarness(t *testing.T) *rollbackIntegrationHarness {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	require.NoError(t, os.MkdirAll(root, 0o755))

	// Keep SQLite and generated worktrees OUTSIDE repo root; InitRepo must not
	// accidentally snapshot its own live DB/WAL files.
	st, err := store.Open(filepath.Join(base, "state.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })
	v := vcs.New(st, filepath.Join(base, "worktrees"))
	repoID, err := v.InitRepo(root)
	require.NoError(t, err)

	file := filepath.Join(root, "a.txt")
	require.NoError(t, os.WriteFile(file, []byte("v0"), 0o644))
	require.NoError(t, v.RecordEditMain(repoID, "test", file, []byte("v0")))
	_, err = v.CommitMain(repoID, "test", "seed v0")
	require.NoError(t, err)

	o, err := orchestrator.New(orchestrator.Config{
		Model: einollm.NewFakeModel([]string{"ok", "ok"}, nil),
	})
	require.NoError(t, err)
	s := New(Config{Store: st, VCS: v, RepoID: repoID})
	s.ChatWS(o, nil, nil)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	url := "ws" + ts.URL[len("http"):] + "/api/v1/chat/ws"
	c := dial(t, url)
	t.Cleanup(func() { _ = c.Close() })

	return &rollbackIntegrationHarness{
		t: t, c: c, v: v, st: st, repoID: repoID, file: file,
	}
}

func (h *rollbackIntegrationHarness) turn(text string) {
	h.t.Helper()
	require.NoError(h.t, h.c.WriteJSON(proto.NewUserMessage(text)))
	for {
		var f proto.ServerFrame
		require.NoError(h.t, h.c.ReadJSON(&f))
		if f.Type == "error" {
			h.t.Fatalf("turn error: %s", f.Text)
		}
		if f.Type == "done" {
			return
		}
	}
}

func (h *rollbackIntegrationHarness) advance(content string) {
	h.t.Helper()
	require.NoError(h.t, os.WriteFile(h.file, []byte(content), 0o644))
	require.NoError(h.t,
		h.v.RecordEditMain(h.repoID, "test", h.file, []byte(content)))
	_, err := h.v.CommitMain(h.repoID, "test", content)
	require.NoError(h.t, err)
}

func (h *rollbackIntegrationHarness) listSeams() proto.ServerFrame {
	h.t.Helper()
	require.NoError(h.t, h.c.WriteJSON(proto.NewListSeams()))
	for {
		var f proto.ServerFrame
		require.NoError(h.t, h.c.ReadJSON(&f))
		if f.Type == "error" {
			h.t.Fatalf("list_seams error: %s", f.Text)
		}
		if f.Type == "seams" {
			return f
		}
	}
}

func (h *rollbackIntegrationHarness) restore(seamID, fullHead string) proto.ServerFrame {
	h.t.Helper()
	require.NotEmpty(h.t, fullHead, "D6 confirmed_head")
	require.NoError(h.t, h.c.WriteJSON(proto.NewRestoreTurn(seamID, fullHead)))
	var restored proto.ServerFrame
	for {
		var f proto.ServerFrame
		require.NoError(h.t, h.c.ReadJSON(&f))
		if f.Type == "error" {
			h.t.Fatalf("restore_turn error: %s", f.Text)
		}
		if f.Type == "seam_restored" {
			restored = f
		}
		if f.Type == "done" {
			require.Equal(h.t, "seam_restored", restored.Type)
			return restored
		}
	}
}
```

- [ ] **Step 4: 运行 GREEN，确认集成测试通过**

Run: `go test ./internal/api/http/ -run TestRB1_EndToEnd_Deterministic -v -count=1 -timeout 60s`
Expected: PASS；无 `t.Skip`,且两次 restore 都使用 `ServerFrame.Head`(full id),从不把 `CommitShort` 当绑定值。

- [ ] **Step 5: 全套 race 测试**

Run: `go test -race ./internal/vcs/... ./internal/store/... ./internal/api/http/... ./internal/tools/... ./internal/cli/... -timeout 300s`
Expected: PASS

- [ ] **Step 6: 全套测试(确认无回归)**

Run: `go test ./... -timeout 300s`
Expected: PASS(允许的 skip:带 `//go:build e2e_real` 的测试、`YANSHI_E2E=1` 门禁的测试、provider 不可用时 t.Skip 的 eino 测试)

- [ ] **Step 7: 提交**

```bash
git add internal/api/http/ws_rollback_integration_test.go
git commit -m "test(api/http): cover deterministic turn rollback and history-aware undo"
```

---

## Limitations

1. **D1 — 文件物化不是 process-crash-atomic 的整目录交换。** `MaterializeMain` 对 `previousTree ∪ targetTree` 的每个 touched path 先保存存在性、bytes 与 mode；进程内任一 read/write/delete/rename/DB 错误都会恢复全部 touched paths且不推进 `main_head`。POSIX 使用 same-directory temp + rename；Windows 使用 delete-existing-then-rename并由 snapshot compensation覆盖进程内失败。但进程若恰在多个文件 mutation 之间崩溃，工作副本仍可能停在部分物化状态；本批次不声称提供原子目录交换或 crash recovery journal。
2. **D4 — agent `revert_turn` 是 VCS-only。** 工具只回滚 autoVCS main 工作副本与 `main_head`，不负责 WS `connSession` 或 durable conversation history。只有 WS `/restore-turn` control path 执行 history transition。agent 路径向 `RevertToSeam(..., 0, 0, nil)` 传 nil snapshot；由此产生的 undo/audit seam 使用空 `session_id`，不会进入任何 WS session list，也不暗示可恢复对话。
3. **D5 — conversation 与 VCS 是带补偿的跨阶段流程，不是单一 crash-atomic transaction。** WS 顺序是 validate → durable session SQLite transaction → in-memory transition → VCS materialize + VCS SQLite transaction；VCS 失败时用 exact `SessionRevertSnapshot` 补偿 durable 与 memory，进程内错误 fail-closed。durable session transaction 与 VCS transaction仍是分开的阶段，并与文件系统 mutation 组合；它们不是一个覆盖数据库/文件系统的单一 crash-atomic transaction。若进程在 durable commit 后、VCS 完成或 compensation 前崩溃，可能需要后续 recovery/reconciliation；本批次不实现该 journal。

---

## Self-Review（计划作者静态核对；不运行 build/test）

### CB1–CB7

- **CB1 — PASS，Task 2/3/4/5。** `VCS` 使用 `repoLocksMu sync.Mutex` + `repoLocks map[string]*sync.Mutex`；索引锁只保护 map，释放后才等待 repo mutex。`InitRepo` 先生成 canonical DB/root identity，固定按 init-key → repoID 取锁并在 repo lane 内重新查询；worktree writers用 lookup → repo lane → locked re-lookup；`ignoreMu` 保护跨 repo 共享 ignore slice。AST contract最终包含 `InitRepo`、`AddWorktree`、`RemoveWorktree`、`Restore`、`RecordEditMain`、`RecordEditWorktree`、`CommitMain`、`CommitWorktree`、`MergeToMain`、`SealMainTurnSeam`、`MaterializeMain`、`ResetMainHead`、`RevertToSeam`；`TestRepoMu_DifferentReposProgressIndependently` 锁定不同 repo 不共用全局 lane；`Restore` 写盘后在同一 repo lane调用 `recordEdit`。
- **CB2 — PASS，Task 9 Step 3。** `runUserTurn` 的注册顺序是 `cs.setInTurn(true); defer cs.setInTurn(false); turnCtx, release := makeTurnCtx(); defer release(); defer postTurnSeam()`，实际 LIFO 为 post seam → release → `inTurn=false`。只有原来重进外层 frame dispatch 的路径改成 closure `return`；schema retry 的 attempt-loop `continue` 与 reader bypass `continue` 保留原语义；原显式 `release()` 删除，避免 early-return leak 与 double release。
- **CB3 — PASS，Task 9 Step 3。** `connSession.inTurn` 是值字段 `atomic.Bool`；main loop只 `Store`，reader goroutine只 `Load`。reader在调用现有 mutex-guarded `conn.write` 前不持任何新 mutex，因此没有 inTurn-lock ↔ write-lock 顺序环。
- **CB4 — PASS，Task 12。** `/restore-turn` 使用真实 `command{name, help, run}`；`cmdRestoreTurn(m model, args []string) (tea.Model, tea.Cmd)`；传输判断为 `m.sess.Mode()`；`sendControlFrame` 的 `(tea.Model, tea.Cmd)` 直接返回。`seamsEntry`、`seamRestorePromptEntry`、`seamRestoredEntry` 实现真实小写 `render(width int, sp spinner.Model) string`；测试使用 `cli.StreamEvent` 与 `proto.SeamInfo`。
- **CB5 — PASS，Task 10。** `Chat` 只注册一次 route，并转发到 production method `handleSSEInternal(w, r, o, models, reg)`；测试直接调用该 method，使用 `orchestrator.New` + `einollm.NewFakeModel`，不传 nil orchestrator，不重复注册 mux。pre seam 后立即注册 post seam defer，覆盖 error/cancel/panic/early return。
- **CB6 — PASS，Task 9/13。** 所有 WS 测试统一使用 `New(Config{...})` → `ChatWS(o, nil, nil)` → `httptest.NewServer(s.Handler())`。普通测试使用 `einollm.NewFakeModel`；cancel/disconnect/in-turn 使用真实 `einollm.NewBlockingModel("ok")` 的 `Started`/`Block` channels，不依赖固定 sleep判断 turn 状态。
- **CB7 — PASS，Task 11。** test profile 的真实合同是值类型 `guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"*"}}}`，并按 `WithProfile(ctx, p guard.PermissionProfile)` 传值；没有虚构 guard type 或 constructor。

### D1–D7

- **D1 — PASS with documented scope cut，Task 4/5 + Limitations。** touched paths严格等于 `previousTree ∪ targetTree`；mutation 前保存存在性/bytes/mode；missing blob、unsafe relative path或任一 mutation失败均恢复全部 touched paths且不推进 head。POSIX 与 Windows replacement 分支有明确测试；不声称 process-crash-atomic 目录交换。
- **D2 — PASS，Task 1/5/8/9/13。** `vcs_seams` 保存 `prev_turn_seq`、`prev_history_len`、`history_snapshot BLOB`。最终签名为 `RevertToSeam(repoID, seamID, label string, prevHistoryLen, prevTurnSeq int, historySnap *store.SessionRevertSnapshot) (undoSeamID string, err error)`；JSON snapshot与 previous head/undo seam/main head写入同一 VCS tx。WS restore pre-revert seam时 decode exact snapshot并校验 session/len/turns；端到端测试验证第一次删除的 durable messages可在再次 restore undo seam 后恢复。
- **D3 — PASS，Task 9/11。** `PermissionRequest.Force bool` 是显式协议；`RequireApproval` 强制设置 Force且不写 sticky allowlist。`resolvePermissionRequest` 对 Force返回 unresolved，绕过 yolo/auto/allow-edits的自动决策，进入一次性交互 prompt；no callback、timeout、cancel、deny全部 fail-closed。
- **D4 — PASS with documented scope cut，Task 5/11 + Limitations。** agent tool描述与实现均为 main files/head VCS-only；其 `RevertToSeam(..., 0, 0, nil)` 结果使用空 session namespace。WS conversation rollback只存在于 `/restore-turn` handler，不把不可达的 history callback塞进 `VCSScope`。
- **D5 — PASS with documented crash window，Task 8/9 + Limitations。** store APIs为 `SnapshotSessionForRevert(sessionID string) (store.SessionRevertSnapshot, error)`、`TruncateSessionForRevert(sessionID string, fromSeq, turns int) (store.SessionRevertSnapshot, error)`、`RestoreSessionAfterFailedRevert(snap store.SessionRevertSnapshot) error`。truncate DELETE + meta UPDATE在一个 tx；任一错误 fatal且 memory/VCS未动。VCS phase失败时 durable + memory均由 exact snapshot补偿；补偿错误显式回给客户端。跨阶段 crash window如实记录。
- **D6 — PASS，Task 6/7/9/12/13。** destructive binding只使用完整 commit ID：`NewSeams(items, commitShort, head)` 与 `NewSeamRestored(undoID, commitShort, head, text)` 同时携带 display `CommitShort` 与 full `Head`；`NewRestoreTurn(seamID, confirmedHead)` 回传 full id。服务端拒绝空 `confirmed_head`，并将其与完整 `RepoMainHead` 比较；TUI在 `seams` 和 `seam_restored` 两类 control reply上立即更新 `lastKnownHead`。
- **D7 — PASS，Task 9/10。** WS list只调用 `ListSeams(repoID, cs.sessionID, 0)`；空 `cs.sessionID` 返回空列表。restore额外要求 `seam.SessionID == cs.sessionID` 且当前 session非空。SSE lifecycle seam明确使用空 session；SSE backend不添加永远收不到的 `seam_restored` control-reply case。

### 最终合同与 TDD 顺序

- `SealMainTurnSeam(repoID, sessionID string, turnSeq, historyLen int, kind SeamKind, label string) (string, error)`、`MaterializeMain(repoID, commitID string) error`、`ResetMainHead(repoID, commitID string) error`、上述 `RevertToSeam` 签名在所有 Task 中一致。
- proto constructors最终固定为 `NewSeams(items, commitShort, head)`、`NewSeamRestored(undoID, commitShort, head, text)`、`NewListSeams()`、`NewRestoreTurn(seamID, confirmedHead)`；`CommitShort` 仅展示，`Head` 才是 binding。
- 每个 Task 保留 RED test → 运行并确认预期失败 → GREEN最小实现 → 运行并确认通过 → commit；按 Task 1→13顺序，每个 GREEN checkpoint使用的类型/函数已在本 Task或更早 Task定义。Task 1先定义 `SessionRevertSnapshot` codec，Task 5不会前向依赖Task 8。
- 决策点为 **0**：D1/D4/D5 scope cut已在 `Limitations` 固化，不留给实施阶段选择。计划作者本轮只修改本 Markdown；未修改 `.go`、未运行 build/test/vet、未执行 git commit。

---
