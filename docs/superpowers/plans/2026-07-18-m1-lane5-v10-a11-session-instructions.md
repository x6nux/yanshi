# V10+A11: 会话生命周期 + 分层项目指令 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让会话可 rename / archive / unarchive / delete（当前只有 clear），并让项目指令按"目标文件所在目录到仓库根"的父→子层级合并 `AGENTS.md`（每层回退 `AGENT.md` → `CLAUDE.md`），子覆盖父、带硬大小上限并按文件注入。

**Architecture:** V10 在 store 的 `sessions` 表加 `archived` 列（迁移 + schema），`ListSessions` 默认过滤已归档、新增 `ListArchivedSessions`/`SetSessionArchived`，`DeleteSession` 改为事务清理 messages + session 行；rename 复用既有 `UpdateSessionTitle`。四条新 client 帧（`rename_session`/`archive_session`/`unarchive_session`/`delete_session`）+ 一条列表补充帧（`session_list_archived`）走 WS 主循环既有 control-frame 分发（与 `session_list`/`restore_session` 同构），服务端以单一新 `session_ack` ServerFrame 回执（携带 action/id/title）；SSE 无状态、不改。A11 新建独立小包 `internal/instruct`，提供纯函数 `LoadHierarchical(rootDir, targetDir)`（父→子合并 + 单项/总量双硬上限 + 截断标注）与 `NestedInstructions`（排除根层、用于 fs 工具按文件注入，避免与系统指令重复）；bootstrap 的 `loadProjectPrompt` 改为委托该包（获得 `AGENTS.md` 支持），`fs_read` 在结果前缀该文件目录链的嵌套指令。

**Tech Stack:** Go stdlib（`database/sql` 事务、`os`/`path/filepath` 文件遍历）；现有 `internal/store`、`internal/proto`、`internal/api/http`（ws.go）、`internal/cli`（backend.go/wsbackend.go）、`internal/cli/tui`（commands.go/model.go）、`internal/bootstrap`、`internal/tools`（fs.go）。无新外部依赖。

**关键设计决策（自上而下）：**
- **V10 仅走 WS。** session 管理帧（含既有 `session_list`/`restore_session`）本就只在 `ws.go` 分发，SSE 无状态不支持；新帧沿用此约定。
- **`session_ack` 是唯一新 ServerFrame。** action ∈ {renamed, archived, unarchived, deleted}，复用既有 `SessionID`/`Text` 字段 + 新增一个 `Action` 字段；不自动重推 `sessions` 列表（用户可用 `/sessions`/`/archived` 主动刷新，与 `/restore` 单次语义一致）。
- **归档是列表过滤、不是软删除。** 新增 `archived` bool 列；`ListSessions` WHERE archived=0，新增 `ListArchivedSessions` WHERE archived=1。`DeleteSession` 才是真删（事务）。
- **delete 删当前会话则重置 connSession。** 否则客户端会持有指向已删行的历史。
- **delete 确认在 TUI 客户端**（无状态 token）：`/delete <id>` 提示须 `/delete <id> yes` 才执行——与服务端无关、无需新协议帧、与既有 slash-command 模型一致。
- **A11 用独立 `internal/instruct` 包。** bootstrap 是组合根（知晓所有包），tools 不能反向依赖 bootstrap；instruct 只依赖 stdlib，被 bootstrap 与 tools 共用，符合六边形向内依赖。
- **A11 按文件注入只在 `fs_read`。** `fs_write`/`fs_edit` 返回 JSON 结果，前缀文本会破坏 JSON；读文件是建立上下文的主操作，注入于此最自然且不破坏既有结果契约。
- **per-level 回退顺序 `AGENTS.md → AGENT.md → CLAUDE.md`。** 新规范名 `AGENTS.md` 优先，同时向后兼容既有 `AGENT.md`（当前代码读的就是它）与 `CLAUDE.md`。
- **父在前、子在后（拼接）。** 子层指令更具体，放在后面让其"覆盖"父层（语义靠顺序，不做字段级合并）。

**不变性：** 既有 `load_prompt_test.go` 全部用例必须仍通过（`loadProjectPrompt` 仍读根层、仍 AGENT.md 优先于 CLAUDE.md）；既有 fs 工具测试目录无 `AGENTS.md`，`NestedInstructions` 返回空、结果字节不变；既有 `TestListSessions_OrderAndCount` 在新过滤下仍返回 2 条（默认 archived=0）。

---

## File Structure

- **Create** `internal/instruct/instruct.go` — `LoadHierarchical`、`NestedInstructions`、`loadChain`、常量 `maxItemBytes`/`maxTotalBytes`/`instructionFiles`。纯函数，仅依赖 stdlib。
- **Create** `internal/instruct/instruct_test.go` — 层级合并、回退、不同 target、单项/总量截断、NestedInstructions 排除根、空。
- **Modify** `internal/store/store.go` — schema `sessions` 表加 `archived` 列；migrate 加 `addColumnIfMissing("sessions","archived",...)`。
- **Modify** `internal/store/session_list.go` — `SessionSummary.Archived`；`ListSessions` 过滤 + 抽 `listSessionsWhere`；新增 `ListArchivedSessions`；`GetSession` SELECT 加 `archived`。
- **Modify** `internal/store/session.go` — `DeleteSession` 改事务；新增 `SetSessionArchived`。
- **Modify** `internal/store/session_lifecycle_test.go`（新建）— archive/unarchive/delete/list 过滤的事务级测试。
- **Modify** `internal/proto/frame.go` — `ClientFrame` 5 个构造函数；`ServerFrame.Action` 字段 + `session_ack` 类型 + `NewSessionAck`；更新两处类型注释表。
- **Modify** `internal/proto/frame_test.go`（追加）— 新帧/构造函数测试。
- **Modify** `internal/api/http/ws.go` — 5 个 dispatch case + `handleRenameSession`/`handleArchiveSession`/`handleUnarchiveSession`/`handleDeleteSession`/`handleArchivedSessionList`。
- **Modify** `internal/api/http/ws_session_test.go`（新建）— store 撑起的 rename/archive/unarchive/delete/list_archived 端到端。
- **Modify** `internal/cli/backend.go` — `StreamEvent.Action` 字段。
- **Modify** `internal/cli/wsbackend.go` — `toStreamEvent` 映射 `Action`；`isControlReply` 加 `session_ack`。
- **Modify** `internal/cli/wsbackend_test.go`（追加）— session_ack 映射 + control-reply 闭合。
- **Modify** `internal/cli/tui/commands.go` — `/rename` `/archive` `/unarchive` `/delete` `/archived` 命令 + `commandTable` 注册 + `ackEntry` + `formatSessionAck`。
- **Modify** `internal/cli/tui/model.go` — `applyEvent` 加 `case "session_ack"`。
- **Modify** `internal/cli/tui/commands_test.go`（追加）— 命令路由 + ack 渲染测试。
- **Modify** `internal/bootstrap/bootstrap.go` — `loadProjectPrompt` 委托 `instruct.LoadHierarchical(dir, dir)`。
- **Modify** `internal/bootstrap/load_prompt_test.go` — 加 AGENTS.md 优先用例。
- **Modify** `internal/tools/fs.go` — `fs_read` 结果前缀 `instruct.NestedInstructions`；抽 `withInstructions` 辅助。
- **Modify** `internal/tools/fs_test.go`（追加）— 嵌套 AGENTS.md 注入测试。

---

## Task 1: Store — archived 列 + archive/unarchive/delete API

**Files:**
- Modify: `internal/store/store.go`
- Modify: `internal/store/session_list.go`
- Modify: `internal/store/session.go`
- Test: `internal/store/session_lifecycle_test.go`（Create）

- [ ] **Step 1: 写失败测试**

创建 `internal/store/session_lifecycle_test.go`:
```go
package store

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSession_ArchiveHidesFromList(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	idActive, err := s.CreateSession("active")
	require.NoError(t, err)
	idArchived, err := s.CreateSession("to-be-archived")
	require.NoError(t, err)

	// Archive one session.
	require.NoError(t, s.SetSessionArchived(idArchived, true))

	active, err := s.ListSessions(0)
	require.NoError(t, err)
	require.Len(t, active, 1)
	assert.Equal(t, idActive, active[0].ID)
	assert.False(t, active[0].Archived, "active list must report Archived=false")

	archived, err := s.ListArchivedSessions(0)
	require.NoError(t, err)
	require.Len(t, archived, 1)
	assert.Equal(t, idArchived, archived[0].ID)
	assert.True(t, archived[0].Archived, "archived list must report Archived=true")
}

func TestSession_UnarchiveRestoresToList(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	id, err := s.CreateSession("x")
	require.NoError(t, err)
	require.NoError(t, s.SetSessionArchived(id, true))
	require.NoError(t, s.SetSessionArchived(id, false))

	active, err := s.ListSessions(0)
	require.NoError(t, err)
	require.Len(t, active, 1)
	assert.Equal(t, id, active[0].ID)
}

func TestSession_GetSessionReportsArchived(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	id, err := s.CreateSession("x")
	require.NoError(t, err)
	require.NoError(t, s.SetSessionArchived(id, true))

	ss, err := s.GetSession(id)
	require.NoError(t, err)
	require.NotNil(t, ss)
	assert.True(t, ss.Archived)
}

func TestSession_DeleteIsTransactional(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	id, err := s.CreateSession("doomed")
	require.NoError(t, err)
	require.NoError(t, s.AppendMessage(id, 0, "user", "hi"))
	require.NoError(t, s.AppendMessage(id, 1, "assistant", "hello"))

	require.NoError(t, s.DeleteSession(id))

	// Session row gone.
	ss, err := s.GetSession(id)
	require.NoError(t, err)
	assert.Nil(t, ss, "session row must be deleted")

	// Associated messages gone.
	msgs, err := s.Messages(id)
	require.NoError(t, err)
	assert.Empty(t, msgs, "messages must be cleaned up")

	// Archived list also empty.
	archived, err := s.ListArchivedSessions(0)
	require.NoError(t, err)
	assert.Empty(t, archived)
}
```

- [ ] **Step 2: 运行确认失败**

Run:
```sh
go test ./internal/store -run "TestSession_ArchiveHidesFromList|TestSession_UnarchiveRestoresToList|TestSession_GetSessionReportsArchived|TestSession_DeleteIsTransactional" -v
```
Expected: FAIL（`SetSessionArchived`/`ListArchivedSessions`/`SessionSummary.Archived` 未定义；`GetSession`/`ListSessions` 列数不含 archived）。

- [ ] **Step 3: 修改 `store.go` — schema + migration**

(a) 在 `const schema = ...` 的 `sessions` 表定义中，在 `reasoning_tokens INTEGER NOT NULL DEFAULT 0` 之后、闭括号之前加一列：
```sql
    archived         INTEGER NOT NULL DEFAULT 0
```
最终 `sessions` 表应为：
```sql
CREATE TABLE IF NOT EXISTS sessions (
    id               TEXT PRIMARY KEY,
    title            TEXT NOT NULL DEFAULT '',
    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL,
    model            TEXT NOT NULL DEFAULT '',
    thinking         TEXT NOT NULL DEFAULT '',
    tokens_in        INTEGER NOT NULL DEFAULT 0,
    tokens_out       INTEGER NOT NULL DEFAULT 0,
    turns            INTEGER NOT NULL DEFAULT 0,
    cached_tokens    INTEGER NOT NULL DEFAULT 0,
    reasoning_tokens INTEGER NOT NULL DEFAULT 0,
    archived         INTEGER NOT NULL DEFAULT 0
);
```

(b) 在 `migrate()` 末尾的 `return s.addColumnIfMissing("sessions", "reasoning_tokens", ...)` 之前插入两行（替换那行 return 为顺序追加 + 最终 return）：
```go
	// V10: archived marks a session hidden from the active list but not deleted
	// (soft-hide). Defaults to 0 (active) so pre-existing rows and pre-V10 SQLite
	// databases stay visible until the user explicitly archives.
	if err := s.addColumnIfMissing("sessions", "archived", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	return nil
```
（即把原先 `return s.addColumnIfMissing("sessions", "reasoning_tokens", ...)` 改为先 `if err := ... reasoning_tokens ...; err != nil { return err }`，再 `if err := ... archived ...; err != nil { return err }`，最后 `return nil`。）

- [ ] **Step 4: 修改 `session_list.go` — Archived 字段 + 过滤 + ListArchivedSessions**

整文件替换为：
```go
package store

import "database/sql"

// SessionSummary is a lightweight session row for list views.
type SessionSummary struct {
	ID        string
	Title     string
	CreatedAt int64
	UpdatedAt int64
	Model     string
	Thinking  string
	TokensIn  int
	TokensOut int
	// CachedTokens / ReasoningTokens (Task A6): cumulative prompt-cache hits
	// and reasoning-model spend. Zero for sessions recorded before this column
	// existed.
	CachedTokens    int
	ReasoningTokens int
	Turns           int
	// Archived (V10) is true for sessions hidden from the active list but not
	// deleted. ListSessions returns Archived=false rows; ListArchivedSessions
	// returns Archived=true rows.
	Archived bool
}

// listSessionsWhere runs the canonical session-list SELECT with an extra WHERE
// fragment (e.g. "WHERE archived = 0") and an optional LIMIT. The column list,
// scan order, and ORDER BY live here so ListSessions and ListArchivedSessions
// cannot drift apart.
func (s *Store) listSessionsWhere(where string, limit int) ([]SessionSummary, error) {
	q := "SELECT id, title, created_at, updated_at, model, thinking, tokens_in, tokens_out, turns, cached_tokens, reasoning_tokens, archived FROM sessions " +
		where + " ORDER BY updated_at DESC"
	args := []any{}
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.DB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionSummary
	for rows.Next() {
		var ss SessionSummary
		if err := rows.Scan(&ss.ID, &ss.Title, &ss.CreatedAt, &ss.UpdatedAt, &ss.Model, &ss.Thinking, &ss.TokensIn, &ss.TokensOut, &ss.Turns, &ss.CachedTokens, &ss.ReasoningTokens, &ss.Archived); err != nil {
			return nil, err
		}
		out = append(out, ss)
	}
	return out, rows.Err()
}

// ListSessions returns ACTIVE sessions (archived = 0) ordered by most-recently-
// updated first. A limit <= 0 returns all active rows. Archived sessions are
// invisible here — use ListArchivedSessions to enumerate them (for /unarchive).
func (s *Store) ListSessions(limit int) ([]SessionSummary, error) {
	return s.listSessionsWhere("WHERE archived = 0", limit)
}

// ListArchivedSessions returns ARCHIVED sessions (archived = 1) ordered by most-
// recently-updated first, so the user can discover IDs to unarchive. A limit <= 0
// returns all archived rows.
func (s *Store) ListArchivedSessions(limit int) ([]SessionSummary, error) {
	return s.listSessionsWhere("WHERE archived = 1", limit)
}

// GetSession returns the session with the given id (active OR archived), or
// (nil, nil) if not found.
func (s *Store) GetSession(id string) (*SessionSummary, error) {
	var ss SessionSummary
	err := s.DB.QueryRow(
		"SELECT id, title, created_at, updated_at, model, thinking, tokens_in, tokens_out, turns, cached_tokens, reasoning_tokens, archived FROM sessions WHERE id = ?",
		id,
	).Scan(&ss.ID, &ss.Title, &ss.CreatedAt, &ss.UpdatedAt, &ss.Model, &ss.Thinking, &ss.TokensIn, &ss.TokensOut, &ss.Turns, &ss.CachedTokens, &ss.ReasoningTokens, &ss.Archived)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &ss, nil
}
```

- [ ] **Step 5: 修改 `session.go` — SetSessionArchived + 事务化 DeleteSession**

(a) 替换既有 `DeleteSession`（当前是两条独立 `Exec`）为事务版本，并在其上方新增 `SetSessionArchived`：
```go
// SetSessionArchived (V10) hides a session from the active list (archived=true)
// or restores it (archived=false). The session row and its messages are NOT
// deleted — only the archived flag flips, so unarchive is lossless.
func (s *Store) SetSessionArchived(sessionID string, archived bool) error {
	v := 0
	if archived {
		v = 1
	}
	_, err := s.DB.Exec("UPDATE sessions SET archived = ? WHERE id = ?", v, sessionID)
	return err
}

// DeleteSession deletes a session and all its messages in a single transaction,
// so a crash between the two DELETEs cannot orphan messages under a gone session
// row (the FK on messages.session_id is unenforced in SQLite by default).
func (s *Store) DeleteSession(sessionID string) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() // safe no-op after Commit
	if _, err := tx.Exec("DELETE FROM messages WHERE session_id = ?", sessionID); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM sessions WHERE id = ?", sessionID); err != nil {
		return err
	}
	return tx.Commit()
}
```

- [ ] **Step 6: 运行确认通过**

Run:
```sh
go test ./internal/store -v
```
Expected: 全 PASS（含新 lifecycle 测试 + 既有 `TestListSessions_OrderAndCount` 仍 PASS——两条会话默认 archived=0 都在活跃列表 + `TestSession_CreateAndAppend` 等）。

- [ ] **Step 7: 提交**

```sh
git add internal/store/store.go internal/store/session_list.go internal/store/session.go internal/store/session_lifecycle_test.go
git commit -m "feat(store): archived flag + archive/unarchive + transactional delete (V10)"
```

---

## Task 2: Proto — rename/archive/unarchive/delete/list_archived client 帧 + session_ack server 帧

**Files:**
- Modify: `internal/proto/frame.go`
- Test: `internal/proto/frame_test.go`（追加；若不存在则新建 `package proto`）

- [ ] **Step 1: 写失败测试**

追加到 `internal/proto/frame_test.go`（若不存在则新建，package proto）:
```go
package proto

import (
	"encoding/json"
	"testing"
)

func TestNewRenameSession(t *testing.T) {
	f := NewRenameSession("s1", "new title")
	if f.Type != "rename_session" || f.ID != "s1" || f.Text != "new title" {
		t.Fatalf("got %+v", f)
	}
}

func TestNewArchiveUnarchiveDeleteSession(t *testing.T) {
	ar := NewArchiveSession("s1")
	if ar.Type != "archive_session" || ar.ID != "s1" {
		t.Fatalf("archive: %+v", ar)
	}
	un := NewUnarchiveSession("s1")
	if un.Type != "unarchive_session" || un.ID != "s1" {
		t.Fatalf("unarchive: %+v", un)
	}
	del := NewDeleteSession("s1")
	if del.Type != "delete_session" || del.ID != "s1" {
		t.Fatalf("delete: %+v", del)
	}
}

func TestNewSessionListArchived(t *testing.T) {
	f := NewSessionListArchived()
	if f.Type != "session_list_archived" {
		t.Fatalf("got %+v", f)
	}
}

func TestNewSessionAck(t *testing.T) {
	f := NewSessionAck("renamed", "s1", "new title")
	if f.Type != "session_ack" || f.Action != "renamed" || f.SessionID != "s1" || f.Text != "new title" {
		t.Fatalf("got %+v", f)
	}
	// action must serialize as a top-level JSON string field, not be dropped.
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if !containsSub(string(b), `"action":"renamed"`) {
		t.Fatalf("action field missing from wire form: %s", b)
	}
}

func containsSub(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
```

- [ ] **Step 2: 运行确认失败**

Run:
```sh
go test ./internal/proto -run "TestNewRenameSession|TestNewArchiveUnarchiveDeleteSession|TestNewSessionListArchived|TestNewSessionAck" -v
```
Expected: FAIL（构造函数/`Action` 字段未定义）。

- [ ] **Step 3: 修改 `frame.go` — ClientFrame 构造函数**

在 `NewRestoreSession` 函数（约 81-83 行）之后追加：
```go
// NewRenameSession renames a stored session's title. id is the session id; title
// is the new title (server trims/clamps). Reply: session_ack{action:"renamed"}.
func NewRenameSession(id, title string) ClientFrame {
	return ClientFrame{Type: "rename_session", ID: id, Text: title}
}

// NewArchiveSession hides a stored session from the active list (archived=true)
// without deleting it. Reply: session_ack{action:"archived"}.
func NewArchiveSession(id string) ClientFrame {
	return ClientFrame{Type: "archive_session", ID: id}
}

// NewUnarchiveSession restores an archived session to the active list. Reply:
// session_ack{action:"unarchive"}.
func NewUnarchiveSession(id string) ClientFrame {
	return ClientFrame{Type: "unarchive_session", ID: id}
}

// NewDeleteSession permanently deletes a stored session and its messages. The
// TUI gates this behind a confirmation token (/delete <id> yes) — the server
// executes unconditionally. Reply: session_ack{action:"deleted"}.
func NewDeleteSession(id string) ClientFrame {
	return ClientFrame{Type: "delete_session", ID: id}
}

// NewSessionListArchived requests the ARCHIVED sessions (reply: sessions). Used
// by /archived so the user can discover IDs to unarchive. Mirrors NewSessionList
// (which returns active sessions).
func NewSessionListArchived() ClientFrame { return ClientFrame{Type: "session_list_archived"} }
```

- [ ] **Step 4: 修改 `frame.go` — ClientFrame 类型注释**

在 `ClientFrame` 顶部的类型注释表（约 14-27 行）加几行（紧跟 `restore_session` 之后、注释闭合之前）:
```
//	rename_session       rename a stored session's title; ID is the session id, Text the new title
//	archive_session      hide a stored session from the active list; ID is the session id
//	unarchive_session    restore an archived session to the active list; ID is the session id
//	delete_session       permanently delete a stored session + its messages; ID is the session id
//	session_list_archived request the ARCHIVED sessions (reply: sessions)
```

- [ ] **Step 5: 修改 `frame.go` — ServerFrame.Action 字段 + 构造函数**

(a) 在 `ServerFrame` 结构体里，`Total int ...` 字段（约 190 行）之后追加：
```go
	// Action (V10) carries the mutation kind on a session_ack frame:
	// "renamed" | "archived" | "unarchive" | "deleted". Pair with SessionID and,
	// for rename, Text (the post-rename title).
	Action string `json:"action,omitempty"` // session_ack
```

(b) 在 `NewSessionRestored`（约 307-318 行）之后追加构造函数：
```go
// NewSessionAck builds a session_ack frame acknowledging a session mutation
// (rename/archive/unarchive/delete). action is "renamed"|"archived"|
// "unarchive"|"deleted"; id is the session id; title is the post-rename title
// (pass "" for non-rename actions). Emitted as a single-frame control reply
// (like sessions), so isControlReply closes the client's reply channel on it.
func NewSessionAck(action, id, title string) ServerFrame {
	return ServerFrame{Type: "session_ack", Action: action, SessionID: id, Text: title}
}
```

(c) 在 `ServerFrame` 顶部类型注释表（约 105-124 行）的 `session_restored` 行之后加一行：
```
//	session_ack         Action, SessionID, Text (ack for rename/archive/unarchive/delete)
```

- [ ] **Step 6: 运行确认通过**

Run:
```sh
go test ./internal/proto -v
```
Expected: 全 PASS（含新测试 + 既有 proto 测试）。

- [ ] **Step 7: 提交**

```sh
git add internal/proto/frame.go internal/proto/frame_test.go
git commit -m "feat(proto): session rename/archive/delete client frames + session_ack (V10)"
```

---

## Task 3: WS handlers — rename/archive/unarchive/delete/list_archived 分发

**Files:**
- Modify: `internal/api/http/ws.go`
- Test: `internal/api/http/ws_session_test.go`（Create）

> 既有 dispatch（`case "session_list"` / `case "restore_session"`，约 610-613 行）是模板。新 handler 与之同构：校验 store + id → 调 store 方法 → 回 `session_ack`（不发 done，与 `session_list` 一致——control 帧由 `isControlReply` 闭合客户端通道）。

- [ ] **Step 1: 写失败测试**

创建 `internal/api/http/ws_session_test.go`:
```go
package http

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/autocode/internal/agent/orchestrator"
	einollm "github.com/x6nux/autocode/internal/llm/eino"
	"github.com/x6nux/autocode/internal/proto"
	"github.com/x6nux/autocode/internal/store"
)

// newSessionTestServer wires a store-backed WS server for session-lifecycle tests.
func newSessionTestServer(t *testing.T) (*store.Store, *Server) {
	t.Helper()
	st, err := store.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })
	o, err := orchestrator.New(orchestrator.Config{Model: einollm.NewFakeModel([]string{"x"}, nil)})
	require.NoError(t, err)
	s := New(Config{Token: "t", Store: st})
	s.ChatWS(o, nil, nil)
	return st, s
}

func dialWSURL(t *testing.T, ts *httptest.Server) string {
	t.Helper()
	return "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/v1/chat/ws"
}

func TestChatWS_RenameSession(t *testing.T) {
	st, s := newSessionTestServer(t)
	sid, err := st.CreateSession("old")
	require.NoError(t, err)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	c := dial(t, dialWSURL(t, ts))
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewRenameSession(sid, "new title")))
	f := readFrame(t, c)
	assert.Equal(t, "session_ack", f.Type)
	assert.Equal(t, "renamed", f.Action)
	assert.Equal(t, sid, f.SessionID)
	assert.Equal(t, "new title", f.Text)

	ss, err := st.GetSession(sid)
	require.NoError(t, err)
	require.NotNil(t, ss)
	assert.Equal(t, "new title", ss.Title)
}

func TestChatWS_ArchiveThenUnarchive(t *testing.T) {
	st, s := newSessionTestServer(t)
	sid, err := st.CreateSession("hide me")
	require.NoError(t, err)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	c := dial(t, dialWSURL(t, ts))
	defer c.Close()

	// Archive: ack + session vanishes from active list.
	require.NoError(t, c.WriteJSON(proto.NewArchiveSession(sid)))
	f := readFrame(t, c)
	assert.Equal(t, "archived", f.Action)
	active, _ := st.ListSessions(0)
	assert.Empty(t, active)
	archived, _ := st.ListArchivedSessions(0)
	require.Len(t, archived, 1)
	assert.Equal(t, sid, archived[0].ID)

	// Unarchive: ack + session returns to active list.
	require.NoError(t, c.WriteJSON(proto.NewUnarchiveSession(sid)))
	f = readFrame(t, c)
	assert.Equal(t, "unarchive", f.Action)
	active, _ = st.ListSessions(0)
	require.Len(t, active, 1)
	assert.Equal(t, sid, active[0].ID)
}

func TestChatWS_DeleteSession_RemovesMessages(t *testing.T) {
	st, s := newSessionTestServer(t)
	sid, err := st.CreateSession("doomed")
	require.NoError(t, err)
	require.NoError(t, st.AppendMessage(sid, 0, "user", "hi"))
	require.NoError(t, st.AppendMessage(sid, 1, "assistant", "yo"))

	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	c := dial(t, dialWSURL(t, ts))
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewDeleteSession(sid)))
	f := readFrame(t, c)
	assert.Equal(t, "deleted", f.Action)

	ss, err := st.GetSession(sid)
	require.NoError(t, err)
	assert.Nil(t, ss)
	msgs, err := st.Messages(sid)
	require.NoError(t, err)
	assert.Empty(t, msgs)
}

func TestChatWS_SessionListArchived(t *testing.T) {
	st, s := newSessionTestServer(t)
	activeID, _ := st.CreateSession("active")
	archivedID, _ := st.CreateSession("gone")
	require.NoError(t, st.SetSessionArchived(archivedID, true))

	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	c := dial(t, dialWSURL(t, ts))
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewSessionListArchived()))
	f := readFrame(t, c)
	require.Equal(t, "sessions", f.Type)
	require.Len(t, f.Sessions, 1, "only archived sessions returned")
	assert.Equal(t, archivedID, f.Sessions[0].ID)
	assert.NotEqual(t, activeID, f.Sessions[0].ID)

	// Active list still excludes the archived one.
	require.NoError(t, c.WriteJSON(proto.NewSessionList()))
	f = readFrame(t, c)
	require.Equal(t, "sessions", f.Type)
	require.Len(t, f.Sessions, 1)
	assert.Equal(t, activeID, f.Sessions[0].ID)
}

func TestChatWS_RenameSession_DisabledWhenNoStore(t *testing.T) {
	// No Store in Config -> recording disabled.
	o, _ := orchestrator.New(orchestrator.Config{Model: einollm.NewFakeModel([]string{"x"}, nil)})
	s := New(Config{Token: "t"})
	s.ChatWS(o, nil, nil)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	c := dial(t, dialWSURL(t, ts))
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewRenameSession("any", "title")))
	f := readFrame(t, c)
	assert.Equal(t, "error", f.Type)
}
```

- [ ] **Step 2: 运行确认失败**

Run:
```sh
go test ./internal/api/http -run "TestChatWS_RenameSession|TestChatWS_ArchiveThenUnarchive|TestChatWS_DeleteSession_RemovesMessages|TestChatWS_SessionListArchived|TestChatWS_RenameSession_DisabledWhenNoStore" -v
```
Expected: FAIL（dispatch case 不识别新帧类型 → 服务端不回 session_ack，`readFrame` 超时或读到无关帧）。

- [ ] **Step 3: 修改 `ws.go` — dispatch case**

在 `case "restore_session": handleRestoreSession(...)`（约 612-613 行）之后、`case "user_message":`（约 614 行）之前插入：
```go
				case "rename_session":
					handleRenameSession(s, conn, &cs, cf.ID, cf.Text)
				case "archive_session":
					handleArchiveSession(s, conn, &cs, cf.ID)
				case "unarchive_session":
					handleUnarchiveSession(s, conn, cf.ID)
				case "delete_session":
					handleDeleteSession(s, conn, &cs, cf.ID)
				case "session_list_archived":
					handleArchivedSessionList(s, conn)
```

- [ ] **Step 4: 修改 `ws.go` — handler 函数**

在 `handleRestoreSession`（约 973-1022 行）之后追加五个 handler。先确认 `ws.go` 已 import `"strings"`（文件末尾 `sortedModelNames` 用 `sort.Strings`，但 `strings` 本体需单独确认——若 import 块无 `"strings"` 则加）：
```go
// handleRenameSession renames a stored session. Reply: session_ack{renamed} or
// error when recording is disabled / id empty / title blank. Title is server-
// trimmed and clamped to 200 runes to bound list rendering.
func handleRenameSession(s *Server, conn *wsConn, cs *connSession, sessionID, title string) {
	if s.store == nil || sessionID == "" {
		conn.write(proto.NewError("session recording is disabled"))
		return
	}
	title = strings.TrimSpace(title)
	if title == "" {
		conn.write(proto.NewError("rename: title must be non-empty"))
		return
	}
	if r := []rune(title); len(r) > 200 {
		title = string(r[:200])
	}
	if err := s.store.UpdateSessionTitle(sessionID, title); err != nil {
		conn.write(proto.NewError("rename: " + err.Error()))
		return
	}
	conn.write(proto.NewSessionAck("renamed", sessionID, title))
}

// handleArchiveSession flips a session's archived flag to true (hides it from
// the active list without deleting). Reply: session_ack{archived}.
func handleArchiveSession(s *Server, conn *wsConn, cs *connSession, sessionID string) {
	if s.store == nil || sessionID == "" {
		conn.write(proto.NewError("session recording is disabled"))
		return
	}
	if err := s.store.SetSessionArchived(sessionID, true); err != nil {
		conn.write(proto.NewError("archive: " + err.Error()))
		return
	}
	conn.write(proto.NewSessionAck("archived", sessionID, ""))
}

// handleUnarchiveSession restores an archived session to the active list.
// Reply: session_ack{unarchive}.
func handleUnarchiveSession(s *Server, conn *wsConn, sessionID string) {
	if s.store == nil || sessionID == "" {
		conn.write(proto.NewError("session recording is disabled"))
		return
	}
	if err := s.store.SetSessionArchived(sessionID, false); err != nil {
		conn.write(proto.NewError("unarchive: " + err.Error()))
		return
	}
	conn.write(proto.NewSessionAck("unarchive", sessionID, ""))
}

// handleDeleteSession permanently deletes a stored session and its messages
// (transactional). If the deleted session is the connSession's live recording,
// reset history/counters so the client is not left holding a dangling id. The
// TUI confirms before sending; the server executes unconditionally. Reply:
// session_ack{deleted}.
func handleDeleteSession(s *Server, conn *wsConn, cs *connSession, sessionID string) {
	if s.store == nil || sessionID == "" {
		conn.write(proto.NewError("session recording is disabled"))
		return
	}
	if err := s.store.DeleteSession(sessionID); err != nil {
		conn.write(proto.NewError("delete: " + err.Error()))
		return
	}
	if cs.sessionID == sessionID {
		cs.history = nil
		cs.tokensIn, cs.tokensOut, cs.turns = 0, 0, 0
		cs.cachedTokens, cs.reasoningTokens = 0, 0
		cs.sessionID = ""
		cs.seq = 0
	}
	conn.write(proto.NewSessionAck("deleted", sessionID, ""))
}

// handleArchivedSessionList replies with the ARCHIVED sessions (sessions frame),
// so the TUI's /archived can list ids to unarchive. Mirrors handleSessionList
// (active) — only the store query differs.
func handleArchivedSessionList(s *Server, conn *wsConn) {
	if s.store == nil {
		conn.write(proto.NewSessions(nil))
		return
	}
	sessions, err := s.store.ListArchivedSessions(0)
	if err != nil {
		conn.write(proto.NewSessions(nil))
		return
	}
	info := make([]proto.SessionInfo, 0, len(sessions))
	for _, ss := range sessions {
		count, _ := s.store.SessionMessageCount(ss.ID)
		info = append(info, proto.SessionInfo{
			ID:              ss.ID,
			Title:           ss.Title,
			CreatedAt:       ss.CreatedAt,
			UpdatedAt:       ss.UpdatedAt,
			MsgCount:        count,
			Model:           ss.Model,
			Thinking:        ss.Thinking,
			TokensIn:        ss.TokensIn,
			TokensOut:       ss.TokensOut,
			CachedTokens:    ss.CachedTokens,
			ReasoningTokens: ss.ReasoningTokens,
			Turns:           ss.Turns,
		})
	}
	conn.write(proto.NewSessions(info))
}
```

> 注：`handleSessionList`（既有，约 940 行）的字段映射照抄到 `handleArchivedSessionList`。`ws.go` 当前 import 块（3-24 行）**未**导入 `"strings"`，而 `handleRenameSession` 用了 `strings.TrimSpace`——必须在该 import 块（`"sort"` 之前或之后）加一行 `"strings"`，否则 `go build` 报 undefined: strings。

- [ ] **Step 5: 运行确认通过**

Run:
```sh
go test ./internal/api/http -run "TestChatWS_RenameSession|TestChatWS_ArchiveThenUnarchive|TestChatWS_DeleteSession_RemovesMessages|TestChatWS_SessionListArchived|TestChatWS_RenameSession_DisabledWhenNoStore" -v
go test ./internal/api/http -v
```
Expected: 新测试 PASS；既有 WS 测试全 PASS（回归）。

- [ ] **Step 6: 提交**

```sh
git add internal/api/http/ws.go internal/api/http/ws_session_test.go
git commit -m "feat(http): WS rename/archive/unarchive/delete + archived list (V10)"
```

---

## Task 4: CLI backend — StreamEvent.Action + session_ack 映射 + control-reply 闭合

**Files:**
- Modify: `internal/cli/backend.go`
- Modify: `internal/cli/wsbackend.go`
- Test: `internal/cli/wsbackend_test.go`（追加；若不存在则新建 `package cli`）

- [ ] **Step 1: 写失败测试**

追加到 `internal/cli/wsbackend_test.go`（若不存在则新建 `package cli` 测试文件）:
```go
package cli

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/x6nux/autocode/internal/proto"
)

func TestToStreamEvent_SessionAck(t *testing.T) {
	ev := toStreamEvent(proto.NewSessionAck("renamed", "s1", "new title"))
	assert.Equal(t, "session_ack", ev.Kind)
	assert.Equal(t, "renamed", ev.Action)
	assert.Equal(t, "s1", ev.SessionID)
	assert.Equal(t, "new title", ev.Text)
}

func TestIsControlReply_SessionAck(t *testing.T) {
	assert.True(t, isControlReply("session_ack"),
		"session_ack must close the control-mode reply channel (single-frame reply)")
	// Regression: existing control replies stay recognized.
	assert.True(t, isControlReply("sessions"))
	assert.True(t, isControlReply("status"))
	assert.False(t, isControlReply("agent_chunk"))
}
```

- [ ] **Step 2: 运行确认失败**

Run:
```sh
go test ./internal/cli -run "TestToStreamEvent_SessionAck|TestIsControlReply_SessionAck" -v
```
Expected: FAIL（`StreamEvent.Action` 未定义；`isControlReply` 不含 session_ack）。

- [ ] **Step 3a: `backend.go` 加 Action 字段**

在 `StreamEvent` 结构体里 `SessionID` 字段（约 64 行）之后追加：
```go
	// Action (V10) carries the mutation kind on a session_ack frame
	// ("renamed"|"archived"|"unarchive"|"deleted"). Consumed by the TUI to render
	// a one-line ack block after /rename /archive /unarchive /delete.
	Action string `json:"action,omitempty"`
```

- [ ] **Step 3b: `wsbackend.go` 的 `toStreamEvent` 映射 Action**

在 `toStreamEvent` 返回结构体里，`SessionID: f.SessionID,`（约 287 行）之后加一行：
```go
		Action:        f.Action,
```

- [ ] **Step 3c: `wsbackend.go` 的 `isControlReply` 加 session_ack**

在 `isControlReply` 的 switch（约 240-246 行）的 case 列表加 `"session_ack"`：
```go
func isControlReply(kind string) bool {
	switch kind {
	case "models", "status", "mcp_list", "sessions", "session_restored", "session_ack":
		return true
	}
	return false
}
```

- [ ] **Step 4: 运行确认通过**

Run:
```sh
go test ./internal/cli -run "TestToStreamEvent_SessionAck|TestIsControlReply_SessionAck" -v
go test ./internal/cli -v
```
Expected: 新测试 PASS；既有 cli 测试全 PASS（回归——session_ack 闭合不影响其它 control 帧）。

- [ ] **Step 5: 提交**

```sh
git add internal/cli/backend.go internal/cli/wsbackend.go internal/cli/wsbackend_test.go
git commit -m "feat(cli): surface session_ack in StreamEvent (V10)"
```

---

## Task 5: TUI — /rename /archive /unarchive /delete /archived 命令 + ack 渲染

**Files:**
- Modify: `internal/cli/tui/commands.go`
- Modify: `internal/cli/tui/model.go`
- Test: `internal/cli/tui/commands_test.go`（追加）

> 命令模型照抄既有 `cmdRestore`（`/restore <id>`）与 `cmdSessions`：append 一个 entry、`sendControlFrame(proto.NewXxx(...))`。delete 加客户端确认 token（`/delete <id> yes`）——无状态、不引入 model 字段。ack 渲染在 `applyEvent` 加一个 `case "session_ack"`。

- [ ] **Step 1: 写失败测试**

追加到 `internal/cli/tui/commands_test.go`:
```go
func TestCmdRename_SendsRenameFrame(t *testing.T) {
	rs := &recordingSession{}
	m := newModel(rs, "/proj")
	mm, _ := cmdRename(m, []string{"s1", "new", "title"})
	m = mm.(model)
	require.Len(t, rs.frames, 1)
	assert.Equal(t, "rename_session", rs.frames[0].Type)
	assert.Equal(t, "s1", rs.frames[0].ID)
	assert.Equal(t, "new title", rs.frames[0].Text)
}

func TestCmdRename_MissingArgs(t *testing.T) {
	rs := &recordingSession{}
	m := newModel(rs, "/proj")
	mm, _ := cmdRename(m, []string{"s1"})
	m = mm.(model)
	assert.Empty(t, rs.frames, "no frame when title missing")
	// An error entry is rendered locally.
	_, isErr := m.entries[len(m.entries)-1].(errorEntry)
	assert.True(t, isErr, "missing-arg should render an error entry")
}

func TestCmdDelete_RequiresConfirmToken(t *testing.T) {
	rs := &recordingSession{}
	m := newModel(rs, "/proj")
	// No "yes" token -> confirmation prompt, NO frame sent.
	mm, _ := cmdDelete(m, []string{"s1"})
	m = mm.(model)
	assert.Empty(t, rs.frames)
	assert.Contains(t, renderLast(m), "yes")

	// With "yes" -> frame sent.
	mm, _ = cmdDelete(m, []string{"s1", "yes"})
	m = mm.(model)
	require.Len(t, rs.frames, 1)
	assert.Equal(t, "delete_session", rs.frames[0].Type)
	assert.Equal(t, "s1", rs.frames[0].ID)
}

func TestCmdArchive_CmdUnarchive_CmdArchived(t *testing.T) {
	rs := &recordingSession{}
	m := newModel(rs, "/proj")

	mm, _ := cmdArchive(m, []string{"s1"})
	m = mm.(model)
	mm, _ = cmdUnarchive(m, []string{"s1"})
	m = mm.(model)
	mm, _ = cmdArchived(m, nil)
	m = mm.(model)

	require.Len(t, rs.frames, 3)
	assert.Equal(t, "archive_session", rs.frames[0].Type)
	assert.Equal(t, "unarchive_session", rs.frames[1].Type)
	assert.Equal(t, "session_list_archived", rs.frames[2].Type)
}

func TestApplyEvent_SessionAck_RendersAck(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m = m.applyEvent(cli.StreamEvent{Kind: "session_ack", Action: "renamed", SessionID: "s1", Text: "hello"})
	last := renderLast(m)
	assert.Contains(t, last, "renamed")
	assert.Contains(t, last, "hello")

	m = m.applyEvent(cli.StreamEvent{Kind: "session_ack", Action: "deleted", SessionID: "s1"})
	assert.Contains(t, renderLast(m), "deleted")
}

// renderLast returns the rendered text of the last entry (helper for command tests).
func renderLast(m model) string {
	if len(m.entries) == 0 {
		return ""
	}
	return stripANSI(m.entries[len(m.entries)-1].render(120, m.spinner))
}
```

> 注：`stripANSI` 已存在于 view_test.go（同包），测试可直接调用。若 `commands_test.go` 未 import `cli`/`assert`/`require`，照抄同包既有测试的 import 块补齐。

- [ ] **Step 2: 运行确认失败**

Run:
```sh
go test ./internal/cli/tui -run "TestCmdRename_|TestCmdDelete_|TestCmdArchive_|TestApplyEvent_SessionAck" -v
```
Expected: FAIL（命令函数/`ackEntry`/`applyEvent` case 未定义）。

- [ ] **Step 3: 修改 `commands.go` — commandTable 注册**

在 `commandTable`（约 29-41 行）里，`{name: "restore", ...}` 之后追加五行：
```go
		{name: "rename", help: "rename a session: /rename <id> <title>", run: cmdRename},
		{name: "archive", help: "hide a session: /archive <id>", run: cmdArchive},
		{name: "unarchive", help: "restore a session: /unarchive <id>", run: cmdUnarchive},
		{name: "archived", help: "list archived sessions", run: cmdArchived},
		{name: "delete", help: "delete a session: /delete <id> yes", run: cmdDelete},
```

- [ ] **Step 4: 修改 `commands.go` — 命令 handler 实现**

在 `cmdRestore`（约 410-420 行）之后追加：
```go
// cmdRename: /rename <id> <title...>. args[0] is the session id; the rest (joined
// by space) is the new title. Missing id or title renders a local error and sends
// nothing. Reply (session_ack) renders via applyEvent.
func cmdRename(m model, args []string) (tea.Model, tea.Cmd) {
	if len(args) < 2 {
		m.entries = append(m.entries, errorEntry{text: "usage: /rename <id> <title>"})
		m.refresh()
		m.viewport.GotoBottom()
		return m, nil
	}
	id := args[0]
	title := strings.Join(args[1:], " ")
	return m.sendControlFrame(proto.NewRenameSession(id, title))
}

// cmdArchive: /archive <id>. Hides the session from the active list.
func cmdArchive(m model, args []string) (tea.Model, tea.Cmd) {
	if len(args) < 1 {
		m.entries = append(m.entries, errorEntry{text: "usage: /archive <id>"})
		m.refresh()
		m.viewport.GotoBottom()
		return m, nil
	}
	return m.sendControlFrame(proto.NewArchiveSession(args[0]))
}

// cmdUnarchive: /unarchive <id>. Restores an archived session. Find ids via
// /archived.
func cmdUnarchive(m model, args []string) (tea.Model, tea.Cmd) {
	if len(args) < 1 {
		m.entries = append(m.entries, errorEntry{text: "usage: /unarchive <id>"})
		m.refresh()
		m.viewport.GotoBottom()
		return m, nil
	}
	return m.sendControlFrame(proto.NewUnarchiveSession(args[0]))
}

// cmdArchived: /archived. Lists archived sessions (reply: sessions frame, reused
// render path). Mirrors cmdSessions.
func cmdArchived(m model, _ []string) (tea.Model, tea.Cmd) {
	m.entries = append(m.entries, &sessionsEntry{})
	m.refresh()
	m.viewport.GotoBottom()
	return m.sendControlFrame(proto.NewSessionListArchived())
}

// cmdDelete: /delete <id> [yes]. Deletion is irreversible, so the client requires
// an explicit "yes" token as the second arg before sending the frame — this is a
// stateless, protocol-free confirmation (no new frame, no model state). Without
// "yes" a confirmation prompt is rendered and nothing is sent.
func cmdDelete(m model, args []string) (tea.Model, tea.Cmd) {
	if len(args) < 1 {
		m.entries = append(m.entries, errorEntry{text: "usage: /delete <id> yes"})
		m.refresh()
		m.viewport.GotoBottom()
		return m, nil
	}
	id := args[0]
	if len(args) < 2 || args[1] != "yes" {
		m.entries = append(m.entries, errorEntry{
			text: "⚠ delete is irreversible. To confirm, run: /delete " + id + " yes",
		})
		m.refresh()
		m.viewport.GotoBottom()
		return m, nil
	}
	return m.sendControlFrame(proto.NewDeleteSession(id))
}
```

- [ ] **Step 5: 修改 `commands.go` — ackEntry + formatSessionAck**

在 entry 类型区（`restoreEntry` 之后，约 560 行附近）追加：
```go
// ackEntry renders a one-line session-mutation acknowledgement (session_ack).
// Rendered green for renamed/archived/unarchive, red for deleted.
type ackEntry struct{ text string }

func (e ackEntry) render(_ int, _ spinner.Model) string {
	return "  " + e.text + "\n\n"
}

// formatSessionAck renders the human line for a session_ack event.
func formatSessionAck(action, id, title string) string {
	short := id
	if len(short) > 8 {
		short = short[:8]
	}
	switch action {
	case "renamed":
		return okStyle.Render("✓ session "+short+" renamed to "+strconv.Quote(title))
	case "archived":
		return okStyle.Render("✓ session " + short + " archived (use /unarchive to restore)")
	case "unarchive":
		return okStyle.Render("✓ session " + short + " restored to active list")
	case "deleted":
		return warnStyle.Render("✗ session " + short + " deleted")
	default:
		return toolMeta.Render("session " + short + " " + action)
	}
}
```
> 注：`strconv` 已在 commands.go import（既有 `cmdMode` 用）。`okStyle`/`warnStyle`/`toolMeta` 均为本包既有样式变量（`sessionsEntry`/`restoreEntry` 已用）。

- [ ] **Step 6: 修改 `model.go` — applyEvent 加 session_ack case**

在 `applyEvent` 的 `case "session_restored":`（约 878 行）之后、`case "status":`（约 908 行）之前插入：
```go
	case "session_ack":
		// Reply to /rename /archive /unarchive /delete: render a one-line ack.
		m.flushAssistant()
		m.entries = append(m.entries, ackEntry{
			text: formatSessionAck(ev.Action, ev.SessionID, ev.Text),
		})
```

- [ ] **Step 7: 运行确认通过**

Run:
```sh
go test ./internal/cli/tui -run "TestCmdRename_|TestCmdDelete_|TestCmdArchive_|TestApplyEvent_SessionAck" -v
go test ./internal/cli/tui -v
```
Expected: 新测试 PASS；既有 TUI 测试全 PASS（回归）。

- [ ] **Step 8: 提交**

```sh
git add internal/cli/tui/commands.go internal/cli/tui/model.go internal/cli/tui/commands_test.go
git commit -m "feat(tui): /rename /archive /unarchive /delete /archived commands (V10)"
```

---

## Task 6: internal/instruct — 分层 AGENTS.md 合并（纯函数 + 截断）

**Files:**
- Create: `internal/instruct/instruct.go`
- Test: `internal/instruct/instruct_test.go`

> 纯函数包，仅依赖 stdlib。`LoadHierarchical(rootDir, targetDir)` 收集 rootDir→targetDir 每层首个存在的 `AGENTS.md`/`AGENT.md`/`CLAUDE.md`，父→子拼接，单项与总量双硬上限 + 截断标注。`NestedInstructions` 同理但排除 rootDir 层（供 fs 工具按文件注入，避免与系统指令重复）。

- [ ] **Step 1: 写失败测试**

创建 `internal/instruct/instruct_test.go`:
```go
package instruct

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func write(t *testing.T, path, body string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
}

func TestLoadHierarchical_ParentChildMergeOrder(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "AGENTS.md"), "ROOT")
	write(t, filepath.Join(root, "sub", "AGENTS.md"), "SUB")

	got := LoadHierarchical(root, filepath.Join(root, "sub"))
	assert.Contains(t, got, "ROOT")
	assert.Contains(t, got, "SUB")
	// Parent first, child after (child overrides by appearing later).
	assert.True(t, strings.Index(got, "ROOT") < strings.Index(got, "SUB"),
		"parent must precede child: %q", got)
}

func TestLoadHierarchical_PerLevelFallback(t *testing.T) {
	root := t.TempDir()
	// Root has only CLAUDE.md; sub has AGENTS.md; sub2 has AGENT.md.
	write(t, filepath.Join(root, "CLAUDE.md"), "ROOT-CLAUDE")
	write(t, filepath.Join(root, "sub", "AGENTS.md"), "SUB-AGENTS")
	write(t, filepath.Join(root, "sub2", "AGENT.md"), "SUB2-AGENT")

	got := LoadHierarchical(root, filepath.Join(root, "sub"))
	assert.Contains(t, got, "ROOT-CLAUDE", "root level falls back to CLAUDE.md")
	assert.Contains(t, got, "SUB-AGENTS", "AGENTS.md wins at sub")

	got2 := LoadHierarchical(root, filepath.Join(root, "sub2"))
	assert.Contains(t, got2, "SUB2-AGENT", "AGENT.md fallback at sub2")
	assert.False(t, strings.Contains(got2, "SUB-AGENTS"), "sibling dirs must NOT bleed in")
}

func TestLoadHierarchical_AgentsMdPreferredOverAgentMd(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "AGENTS.md"), "PLURAL")
	write(t, filepath.Join(dir, "AGENT.md"), "SINGULAR")
	got := LoadHierarchical(dir, dir)
	assert.Contains(t, got, "PLURAL")
	assert.False(t, strings.Contains(got, "SINGULAR"), "AGENTS.md must win over AGENT.md")
}

func TestLoadHierarchical_DifferentTargetsDifferentChains(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "AGENTS.md"), "ROOT")
	write(t, filepath.Join(root, "a", "AGENTS.md"), "A")
	write(t, filepath.Join(root, "b", "AGENTS.md"), "B")

	gotA := LoadHierarchical(root, filepath.Join(root, "a"))
	gotB := LoadHierarchical(root, filepath.Join(root, "b"))
	assert.Contains(t, gotA, "A")
	assert.False(t, strings.Contains(gotA, "B"), "chain for a/ must not include b/")
	assert.Contains(t, gotB, "B")
	assert.False(t, strings.Contains(gotB, "A"), "chain for b/ must not include a/")
}

func TestLoadHierarchical_EmptyWhenNoFile(t *testing.T) {
	root := t.TempDir()
	assert.Empty(t, LoadHierarchical(root, root))
	assert.Empty(t, LoadHierarchical(root, filepath.Join(root, "sub")))
}

func TestLoadHierarchical_ItemCapTruncates(t *testing.T) {
	root := t.TempDir()
	big := strings.Repeat("X", maxItemBytes+500)
	write(t, filepath.Join(root, "AGENTS.md"), big)
	got := LoadHierarchical(root, root)
	assert.True(t, len(got) < len(big), "single item must be truncated to <= cap+marker")
	assert.Contains(t, got, "truncated")
}

func TestLoadHierarchical_TotalCapTruncates(t *testing.T) {
	root := t.TempDir()
	// Several levels each near the item cap so the running total exceeds the
	// total cap partway through.
	chunk := strings.Repeat("Y", maxItemBytes)
	for _, sub := range []string{"a", "b", "c", "d", "e"} {
		write(t, filepath.Join(root, sub, "AGENTS.md"), chunk)
	}
	got := LoadHierarchical(root, filepath.Join(root, "e"))
	assert.LessOrEqual(t, len(got), maxTotalBytes+200, "total must be capped near maxTotalBytes")
	assert.Contains(t, got, "truncated")
}

func TestNestedInstructions_ExcludesRoot(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "AGENTS.md"), "ROOT")
	write(t, filepath.Join(root, "sub", "AGENTS.md"), "SUB")

	got := NestedInstructions(root, filepath.Join(root, "sub"))
	assert.Contains(t, got, "SUB")
	assert.False(t, strings.Contains(got, "ROOT"),
		"NestedInstructions must exclude root level (already in system prompt)")
}

func TestNestedInstructions_EmptyForRootLevelFile(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "AGENTS.md"), "ROOT")
	// Target dir == root: no nested levels -> empty.
	assert.Empty(t, NestedInstructions(root, root))
}

func TestLoadHierarchical_TargetOutsideRootFallsBackToRoot(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "AGENTS.md"), "ROOT")
	outside := t.TempDir()
	write(t, filepath.Join(outside, "AGENTS.md"), "OUTSIDE")
	// Target dir outside root must NOT pull in outside content; falls back to root.
	got := LoadHierarchical(root, outside)
	assert.Contains(t, got, "ROOT")
	assert.False(t, strings.Contains(got, "OUTSIDE"))
}
```

- [ ] **Step 2: 运行确认失败**

Run:
```sh
go test ./internal/instruct -v
```
Expected: FAIL（包/函数未定义，编译错误）。

- [ ] **Step 3: 实现 `instruct.go`**

创建 `internal/instruct/instruct.go`:
```go
// Package instruct loads hierarchical project instruction files (AGENTS.md).
//
// The orchestrator's system prompt is static, baked once at bootstrap from the
// repository-root instruction file. instruct.LoadHierarchical additionally
// merges the per-directory AGENTS.md chain for a given target path so that
// file-touching tools (fs_read) can surface the instructions relevant to the
// specific directory the agent is operating in — parent directives first, child
// (more specific) directives last so the child overrides the parent by order.
//
// At each directory level the first present file is read, trying the modern
// AGENTS.md, then the legacy AGENT.md, then CLAUDE.md. Both per-item and total
// sizes are hard-capped with a truncation marker so a runaway instruction file
// cannot blow out the context window.
package instruct

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// maxItemBytes caps a single instruction file's contribution. A larger file is
// truncated to this many bytes plus a marker, so one giant AGENTS.md cannot
// monopolize the budget.
const maxItemBytes = 32 << 10 // 32 KiB

// maxTotalBytes caps the merged instruction text across ALL levels. Once the
// running total reaches this, the current segment is truncated and remaining
// levels are dropped, so a deep directory chain cannot exceed the budget.
const maxTotalBytes = 64 << 10 // 64 KiB

// instructionFiles is the per-level fallback order. AGENTS.md is the modern
// convention; AGENT.md is the legacy name this codebase historically read;
// CLAUDE.md is the Claude-Code-compatible fallback.
var instructionFiles = []string{"AGENTS.md", "AGENT.md", "CLAUDE.md"}

// LoadHierarchical returns merged project instructions for the directory chain
// from rootDir down to targetDir (both directories). At each level the first
// present file in instructionFiles is read; levels are concatenated parent→child
// so more-specific (child) instructions appear last. Each item AND the total are
// hard-capped (see maxItemBytes / maxTotalBytes) with a truncation marker.
//
// Returns "" when no instruction file exists at any level in the chain. If
// targetDir is outside rootDir, only the rootDir level is read (the walk never
// escapes the project root).
func LoadHierarchical(rootDir, targetDir string) string {
	return loadChain(rootDir, targetDir, true)
}

// NestedInstructions is LoadHierarchical EXCLUDING the rootDir level. It returns
// only instructions from directories strictly below rootDir, so injecting it
// into a per-file tool result does not duplicate the root-level instructions
// already baked into the orchestrator's system prompt by bootstrap. Returns ""
// for a target directly in rootDir or when no nested instruction file exists.
func NestedInstructions(rootDir, targetDir string) string {
	return loadChain(rootDir, targetDir, false)
}

// loadChain is the shared walker. includeRoot=false skips the rootDir level
// (NestedInstructions). The chain is built rootDir→targetDir (parent first); if
// targetDir is not under rootDir, the chain collapses to rootDir only.
func loadChain(rootDir, targetDir string, includeRoot bool) string {
	rootClean := filepath.Clean(rootDir)
	targetClean := filepath.Clean(targetDir)
	if !isWithin(targetClean, rootClean) {
		targetClean = rootClean
	}

	// Build the directory chain from targetClean up to rootClean, then reverse
	// to parent→child order. Prepending while walking up yields root-first.
	var chain []string
	d := targetClean
	for {
		if d == rootClean {
			chain = append([]string{rootClean}, chain...)
			break
		}
		chain = append([]string{d}, chain...)
		parent := filepath.Dir(d)
		if parent == d {
			// Reached the filesystem root without hitting rootClean (should not
			// happen after the isWithin guard, but defend against edge cases).
			break
		}
		d = parent
	}

	var parts []string
	total := 0
	for _, dir := range chain {
		if !includeRoot && dir == rootClean {
			continue
		}
		body, ok := readInstruction(dir)
		if !ok {
			continue
		}
		body = capItem(body)
		segment := fmt.Sprintf("## Instructions (%s)\n%s", relOrSelf(rootClean, dir), body)
		if total+len(segment) > maxTotalBytes {
			remaining := maxTotalBytes - total
			if remaining <= 0 {
				break
			}
			segment = segment[:remaining] + "\n[... truncated: total instruction cap reached ...]"
			parts = append(parts, segment)
			break
		}
		parts = append(parts, segment)
		total += len(segment)
	}
	return strings.Join(parts, "\n\n")
}

// readInstruction reads the first present instruction file in dir (per the
// fallback order). Returns (content, true) when one exists, else ("", false).
// A read error for a present file is silently ignored so a permissions issue on
// one file does not abort the whole chain.
func readInstruction(dir string) (string, bool) {
	for _, name := range instructionFiles {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err == nil {
			return string(data), true
		}
	}
	return "", false
}

// capItem truncates a single instruction file's bytes to maxItemBytes with a
// marker. No-op when already within the cap.
func capItem(s string) string {
	if len(s) <= maxItemBytes {
		return s
	}
	return s[:maxItemBytes] + "\n[... truncated: single instruction file cap reached ...]"
}

// relOrSelf returns dir relative to root ("." when they are equal), falling back
// to the absolute dir on error. Used only for the human-readable per-level
// header.
func relOrSelf(root, dir string) string {
	if dir == root {
		return "."
	}
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return dir
	}
	return rel
}

// isWithin reports whether child is equal to root or a descendant of it. Both
// inputs must be cleaned.
func isWithin(child, root string) bool {
	if child == root {
		return true
	}
	return strings.HasPrefix(child, root+string(filepath.Separator))
}
```

- [ ] **Step 4: 运行确认通过**

Run:
```sh
go test ./internal/instruct -v
```
Expected: 全 PASS。

- [ ] **Step 5: 提交**

```sh
git add internal/instruct/instruct.go internal/instruct/instruct_test.go
git commit -m "feat(instruct): hierarchical AGENTS.md loader with size caps (A11)"
```

---

## Task 7: Bootstrap — loadProjectPrompt 委托 instruct（+ AGENTS.md 支持）

**Files:**
- Modify: `internal/bootstrap/bootstrap.go`
- Modify: `internal/bootstrap/load_prompt_test.go`

> `loadProjectPrompt(dir)` 当前手写读 `["AGENT.md","CLAUDE.md"]`（约 353-364 行）。改为委托 `instruct.LoadHierarchical(dir, dir)`：根层单层，但获得 `AGENTS.md` 优先 + 统一截断。既有 5 个 `load_prompt_test.go` 用例必须仍通过（语义不变），并新增一个 AGENTS.md 优先用例。

- [ ] **Step 1: 写失败测试（新增用例）**

在 `internal/bootstrap/load_prompt_test.go` 顶部追加（保留既有用例不变）:
```go
func TestLoadProjectPrompt_PrefersAGENTSmd(t *testing.T) {
	dir := t.TempDir()
	// All three present — AGENTS.md must win.
	os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("# AGENTS.md"), 0o644)
	os.WriteFile(filepath.Join(dir, "AGENT.md"), []byte("# AGENT.md"), 0o644)
	os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("# CLAUDE.md"), 0o644)

	got := loadProjectPrompt(dir)
	if !strings.Contains(got, "# AGENTS.md") || strings.Contains(got, "# AGENT.md") {
		t.Errorf("loadProjectPrompt must prefer AGENTS.md over AGENT.md/CLAUDE.md; got %q", got)
	}
}
```
并在该文件的 import 块加 `"strings"`（若未导入）。

- [ ] **Step 2: 运行确认失败**

Run:
```sh
go test ./internal/bootstrap -run "TestLoadProjectPrompt_PrefersAGENTSmd" -v
```
Expected: FAIL（当前 `loadProjectPrompt` 只试 AGENT.md/CLAUDE.md，读不到 AGENTS.md→读到 AGENT.md → 断言失败）。

- [ ] **Step 3: 修改 `bootstrap.go`**

(a) 在 import 块加 `"github.com/x6nux/autocode/internal/instruct"`（若无）。

(b) 替换既有 `loadProjectPrompt` 函数体（约 348-364 行）为委托：
```go
// loadProjectPrompt returns the root-level project instructions, preferring
// AGENTS.md then AGENT.md then CLAUDE.md. It delegates to instruct.LoadHierarchical
// so the root level uses the SAME fallback order and hard size cap as the nested
// per-file injection (A11); at the root level the chain is a single directory, so
// this is equivalent to "read the first present root instruction file". An empty
// string is returned when none exists, letting the orchestrator use its built-in
// default instruction.
func loadProjectPrompt(dir string) string {
	return instruct.LoadHierarchical(dir, dir)
}
```

- [ ] **Step 4: 运行确认通过（含既有用例回归）**

Run:
```sh
go test ./internal/bootstrap -v
```
Expected: 全 PASS（新增 `PrefersAGENTSmd` + 既有 `PrefersAGENTmd`/`FallsBackToCLAUDEmd`/`EmptyWhenNoFile`/`IgnoresSubdirs`/`EmptyDir` 仍 PASS——`LoadHierarchical(dir,dir)` 单层、AGENT.md 仍优先于 CLAUDE.md、子目录不被读）。

- [ ] **Step 5: 提交**

```sh
git add internal/bootstrap/bootstrap.go internal/bootstrap/load_prompt_test.go
git commit -m "feat(bootstrap): loadProjectPrompt delegates to instruct (AGENTS.md support, A11)"
```

---

## Task 8: FS tools — fs_read 按文件注入嵌套 AGENTS.md 链

**Files:**
- Modify: `internal/tools/fs.go`
- Test: `internal/tools/fs_test.go`（追加）

> 只在 `fs_read` 注入（`fs_write`/`fs_edit` 返回 JSON，前缀会破坏 JSON）。注入内容 = `instruct.NestedInstructions(f.root, dir(paths[0]))`（排除根层，避免与系统指令重复）；空则不注入（既有测试目录无 AGENTS.md，结果字节不变——回归保护）。

- [ ] **Step 1: 写失败测试**

追加到 `internal/tools/fs_test.go`（照抄既有 `TestFS_Read` 的装配：`NewFSTools(root)` + `WithProfile` + `runTool(ctx, fs.Read, argsJSON)`）:
```go
func TestFS_Read_InjectsNestedAgentsMd(t *testing.T) {
	root := t.TempDir()
	// Root AGENTS.md is EXCLUDED (already in system prompt); nested one is injected.
	require.NoError(t, os.MkdirAll(filepath.Join(root, "pkg"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("ROOT-INSTR"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "pkg", "AGENTS.md"), []byte("PKG-INSTR"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "pkg", "main.go"), []byte("package main"), 0o644))

	f := NewFSTools(root)
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
		FS:    guard.FSPerm{Read: []string{root + "/**"}},
	})
	out, err := runTool(ctx, f.Read, `{"path":"pkg/main.go"}`)
	require.NoError(t, err)
	assert.Contains(t, out, "package main", "file content still present")
	assert.Contains(t, out, "PKG-INSTR", "nested AGENTS.md injected")
	assert.NotContains(t, out, "ROOT-INSTR", "root AGENTS.md must NOT be duplicated")
}

func TestFS_Read_NoInjectionWhenNoNestedInstructions(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "main.go"), []byte("package main"), 0o644))
	// Root-level AGENTS.md exists but NestedInstructions excludes root -> no injection.
	require.NoError(t, os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("ROOT-INSTR"), 0o644))

	f := NewFSTools(root)
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
		FS:    guard.FSPerm{Read: []string{root + "/**"}},
	})
	out, err := runTool(ctx, f.Read, `{"path":"main.go"}`)
	require.NoError(t, err)
	assert.NotContains(t, out, "ROOT-INSTR", "root-only must not inject")
	assert.Contains(t, out, "package main")
}
```
> 装配照抄同文件既有 `TestFS_Read`：`runTool(ctx, fs.Read, argsJSON-string)` 是本包既有 helper（返回 `(string, error)`）；`guard.PermissionProfile`/`guard.ToolsPerm{Allow}`/`guard.FSPerm{Read}` 是既有字段名（见 fs_test.go:27-29）；`context`/`os`/`path/filepath`/`guard`/`assert`/`require` 均为该文件已 import。无需新增辅助函数。

- [ ] **Step 2: 运行确认失败**

Run:
```sh
go test ./internal/tools -run "TestFS_Read_InjectsNestedAgentsMd|TestFS_Read_NoInjectionWhenNoNestedInstructions" -v
```
Expected: FAIL（注入用例断言 `PKG-INSTR`，当前 `fs_read` 不注入）。

- [ ] **Step 3: 修改 `fs.go`**

(a) 在 import 块加 `"github.com/x6nux/autocode/internal/instruct"`（若无）。`os`/`path/filepath` 已导入。

(b) 在 `fs.go` 加辅助方法（紧邻 `runRead` 之前或 `abs` 之后）:
```go
// withInstructions prepends any nested AGENTS.md instructions for absPath's
// directory to result, so the agent sees the directory-specific project rules
// when it reads a file. Root-level instructions are excluded (NestedInstructions)
// because they are already baked into the orchestrator's system prompt — this
// avoids duplication. Returns result unchanged when no nested instruction file
// exists (the common case), keeping fs_read output byte-identical to pre-A11 for
// directories without AGENTS.md.
func (f *FSTools) withInstructions(absPath, result string) string {
	hint := instruct.NestedInstructions(f.root, filepath.Dir(absPath))
	if hint == "" {
		return result
	}
	return "# Project instructions (AGENTS.md chain for this directory)\n" + hint +
		"\n# --- file content ---\n" + result
}
```

(c) 在 `runRead` 的 `return result, nil`（约 294 行）之前注入。把
```go
	result := strings.Join(out, "\n")
	if truncated {
		result += fmt.Sprintf(
			"\n[... truncated: showing first %d bytes of %d ...]",
			fsReadMaxBytes, origLen,
		)
	}
	return result, nil
```
改为：
```go
	result := strings.Join(out, "\n")
	if truncated {
		result += fmt.Sprintf(
			"\n[... truncated: showing first %d bytes of %d ...]",
			fsReadMaxBytes, origLen,
		)
	}
	return f.withInstructions(paths[0], result), nil
```

> 不改 `runWrite`/`runEdit`（JSON 结果完整性）。不改 `runList`/`runGlob`/`runSearch`（非"读单个目标文件"语义；注入会让列表/glob 结果膨胀）。

- [ ] **Step 4: 运行确认通过（含既有 fs 回归）**

Run:
```sh
go test ./internal/tools -run "TestFS_Read_" -v
go test ./internal/tools -v
```
Expected: 新注入测试 PASS；既有 fs_read 测试全 PASS（测试目录无嵌套 AGENTS.md → 不注入 → 字节不变）。

- [ ] **Step 5: 提交**

```sh
git add internal/tools/fs.go internal/tools/fs_test.go
git commit -m "feat(tools): fs_read injects nested AGENTS.md instructions (A11)"
```

---

## Task 9: 全量回归 + vet + 构建

**Files:**
- 无新增；运行全量测试 + go vet + build。

- [ ] **Step 1: 全量测试**

Run:
```sh
go test ./...
```
Expected: 全 PASS（允许 CLAUDE.md 记载的预期 `t.Skip`：`e2e_real` 门禁、部分 eino/bootstrap 测试在 openai provider 不可用时 skip——这些是预期行为不是失败）。

- [ ] **Step 2: vet**

Run:
```sh
go vet ./...
```
Expected: 无输出。

- [ ] **Step 3: 构建**

Run:
```sh
go build -o autocode ./cmd/autocode
```
Expected: 成功（`autocode.exe` 可能出现在工作树，勿提交）。

- [ ] **Step 4: 冒烟（可选，fake model）**

Run:
```sh
./autocode -h
```
Expected: 打印用法并退出 0（确认 bootstrap 接 `instruct` 没破坏启动）。

- [ ] **Step 5: 提交（若有未提交的小修）**

```sh
git add -A
git commit -m "test: V10+A11 regression green" || echo "nothing to commit"
```

---

## Self-Review

1. **Spec 覆盖**
   - **V10 rename 后列表可检索**：Task 1 `UpdateSessionTitle`（既有）+ Task 3 `handleRenameSession` 写回 + Task 3 `TestChatWS_RenameSession` 验证 store 落库；`/sessions` 重读即见新标题 ✅。
   - **V10 archive 后不在活跃列表**：Task 1 `SetSessionArchived(true)` + `ListSessions WHERE archived=0` + Task 3 `TestChatWS_ArchiveThenUnarchive` 验证 `ListSessions` 空、`ListArchivedSessions` 1 条 ✅。
   - **V10 unarchive 恢复**：Task 1 `SetSessionArchived(false)` + Task 3 同测试验证回到活跃列表 ✅。
   - **V10 delete 有确认且清理消息/元数据**：Task 1 事务化 `DeleteSession`（messages + session）+ Task 5 `/delete <id> yes` 客户端 token 确认 + Task 3 `TestChatWS_DeleteSession_RemovesMessages` 验证消息清空、行删除 ✅。
   - **A11 父→子 AGENTS.md 合并**：Task 6 `LoadHierarchical` 父前子后 + `TestLoadHierarchical_ParentChildMergeOrder` ✅。
   - **A11 不同目标文件得到正确指令链**：Task 6 `TestLoadHierarchical_DifferentTargetsDifferentChains`（函数级）+ Task 8 `TestFS_Read_InjectsNestedAgentsMd`（运行时按文件注入）✅。
   - **A11 注入有硬大小上限**：Task 6 单项 `maxItemBytes` + 总量 `maxTotalBytes` + 截断标注 + `TestLoadHierarchical_ItemCapTruncates`/`TotalCapTruncates` ✅。

2. **Placeholder 扫描**：Task 8 的 fs_test 装配照抄同文件既有 `TestFS_Read`（`runTool`/`guard.PermissionProfile`/`guard.FSPerm{Read}`），字段名与 helper 均经源码核对，无凭空假设。无 TBD/TODO/"implement later"。所有步骤含完整代码。

3. **类型一致性**：`SetSessionArchived`/`ListArchivedSessions`/`SessionSummary.Archived`（Task 1）→ `handleArchiveSession`/`handleArchivedSessionList`（Task 3）→ `NewArchiveSession`/`NewSessionListArchived`/`NewSessionAck`/`Action`（Task 2）→ `StreamEvent.Action`/`isControlReply`（Task 4）→ `cmdArchive`/`ackEntry`/`formatSessionAck`/`applyEvent case "session_ack"`（Task 5）命名贯穿一致 ✅。action 字符串 `"renamed"|"archived"|"unarchive"|"deleted"` 在 proto/handler/TUI 三处一致（注意 unarchive 动作名是 `"unarchive"`——`formatSessionAck` 的 switch case 与 `NewSessionAck` 调用处一致）✅。A11 侧 `LoadHierarchical`/`NestedInstructions`/`maxItemBytes`/`maxTotalBytes` 在 Task 6/7/8 一致 ✅。

4. **已知限制（非 placeholder，是 v1 边界）**：V10 仅走 WS（SSE 无状态，session 管理帧与既有 `session_list`/`restore_session` 一致地 WS-only）；delete 确认是无状态 token（非交互式两段式 popup），刻意避免改 model 的主键开关；rename 不自动重推 sessions 列表（用户主动 `/sessions`）。A11 只在 `fs_read` 注入（保护 write/edit 的 JSON 结果完整性）；per-level 回退顺序 `AGENTS.md→AGENT.md→CLAUDE.md` 同时兼容新规范名与历史名。

## 执行交接

Plan complete and saved to `docs/superpowers/plans/2026-07-18-m1-lane5-v10-a11-session-instructions.md`. 两种执行方式：

1. **Subagent-Driven（推荐）** — 每个任务派一个新 subagent，任务间 review。V10（Task 1-5）与 A11（Task 6-8）两条线相互独立，可并行推进；Task 9 收尾合并回归。
2. **Inline Execution** — 本会话内按 executing-plans 批次执行 + checkpoint。
