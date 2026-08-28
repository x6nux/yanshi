# W-D 历史与记忆 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把活动窗口从「就地重写的有损单副本」换成「append-only 日志 + 可重放投影」，并在这套模型上补齐跨会话检索、跨会话记忆生成、冷会话归档等 15 条历史与记忆能力。

**Architecture:** 新增只 INSERT 的 `context_events` 表承载压缩标记，`messages` 表保持不变；`Store.ProjectWindow` 折叠事件流算出 `hidden_seq`，再做一次索引范围读重建窗口。`internal/ctxcompact` 的 `Plan`/`EnforceToolCallPairs`/`Assemble` 三步一行不改 —— 改的只是「切片从哪来」。其余 15 条大多是这套模型的推论。

**Tech Stack:** Go 1.26.4、SQLite（`internal/store`）、Eino ADK、`schema.Message`。

**Spec:** `docs/superpowers/specs/2026-08-27-capability-roadmap-design.md` §1.2 INF3、§3（W-D-01 … W-D-16）
**ADR:** `docs/adr/0015-append-only-context-log-with-replayable-projection.md`

## Global Constraints

以下每条都对**所有**任务生效。

1. **不得使用占位符、mock 代替真实实现**：没有 TODO、没有空函数壳、没有硬编码假数据。（CLAUDE.md 用户级规则）
2. **`context_events` 只接受 INSERT。** 不得出现 `UPDATE` 或 `DELETE`，唯一例外是 `DeleteSession` 级联清理整个会话。撤销靠追加事件表达。（ADR-0015 约束 1）
3. **旧会话逐字节等同现状。** 没有任何 `context_events` 行时 `ProjectWindow` 必须返回与 `Messages` 完全相同的切片，且要有常驻回归测试。（ADR-0015 约束 2）
4. **`internal/ctxcompact` 不得 import `internal/store`。** GOV1 会红。（ADR-0015 约束 3）
5. **不得宣称 `takeChunk` 超窗上界被解决。** 它与存储模型无关。（ADR-0015 约束 4）
6. **W-D 不进台账。** 不要碰 `docs/feature-status.yaml`、不要碰 `internal/archtest/status_test.go` 的 `ledgerSize`、不要写 `ledger:` 标记。走常规单测 + GOV1–GOV9。
7. **新增的导出符号必须有 doc 注释**（GOV3）；新增的 `Build*` 必须从 `bootstrap.Build` 可达（GOV4）；新增的 `With<X>(ctx,…) context.Context` 必须有生产调用点（GOV6）；新增给模型用的工具必须在组合根注册（否则 `toolreg.Check` 运行期拒掉）。
8. **单文件纯代码行 ≤ 5000**，但拆分判据是职责不是行数。
9. **注释是承重文档**：解释*为什么*，密度对齐所在包。中文注释文件写中文，英文注释文件写英文 —— 跟随所在文件现状。
10. **不要执行 git 提交以外的 VCS 操作**（不建分支、不推送）。每个任务结束提交，conventional commit。
11. **文档里的 `路径::符号` 引用必须真实存在**（GOV9 扫全部活 `.md`）。写计划外的新文档时注意。
12. **每个任务先跑 `go test ./internal/archtest`**，再跑受影响包的测试。
13. **数据库迁移的两条路径【实测】**，别自己发明第三条：
    - **新表** → 写进 `internal/store/store.go` 的 `schema` 常量，用 `CREATE TABLE IF NOT EXISTS`。`migrate()` 第一句就是 `s.DB.Exec(schema)`。
    - **给已有表加列** → 在 `migrate()` 里加一行 `s.addColumnIfMissing(table, col, decl)`。**必须带 `NOT NULL DEFAULT <零值>`**，否则存量行读出来是 NULL 会炸扫描。`memories` 表当前的 `session_id` / `agent_id` 就是这么加的（`schema` 里那段 DDL 只有 4 列，别被它误导）。
    - 加列后**同步更新对应的 `xxxColumns` 常量与所有 `scanXxx` 的扫描顺序** —— 这两处漂了会在运行时才炸。
14. **压缩算法用标准库 `compress/gzip`【实测】**：`go.mod` 里没有 `klauspost/compress`，不要为归档新增依赖。（阶梯：标准库够用就用标准库）

## 计划的可信度分级

本计划给出的代码分两档，实施者必须区分：

- **【实测】** 标记的 DDL、SQL、签名、现状引文，是写计划时跑过或读过源码确认的，照抄即可。
- **【设计】** 标记的是意图与验收，落地形状以你读到的代码为准。**与本计划不符时以代码为准**，并在报告里记一条「计划说 X，实际是 Y，我按 Y 做了」。

W-A 包的六次 Critical 里有四次源于计划凭空编造夹具形状。这条分级是那次的直接产物：不要为了照抄计划而把测试写成断言一个生产环境不存在的形状。

---

### Task 1: INF3 — append-only 事件表与可重放投影

**覆盖** W-D-01（`A13`）、W-D-05（`B38`，投影天然是有界范围读，不另做反向扫描）

**Files:**
- Create: `internal/store/context_events.go`
- Create: `internal/store/context_events_test.go`
- Modify: `internal/store/store.go`（schema DDL 追加建表）
- Modify: `internal/api/http/ws_compaction.go`（压缩后写标记；`loadSession` 改走投影）
- Test: `internal/api/http/ws_persist_test.go`（追加重连回归）

**Interfaces:**
- Produces（后续任务依赖）：
  - `store.ContextEvent` 结构体
  - `func (s *Store) AppendContextEvent(sessionID, kind string, hiddenSeq int) error`
  - `func (s *Store) ProjectWindow(sessionID string) ([]Message, error)`
  - `func (s *Store) HiddenSeq(sessionID string) (int, error)`
  - 常量 `store.ContextEventCompact = "compact"`、`store.ContextEventUndo = "undo"`

#### 背景：这个 bug 是实测出来的，不是推理出来的

写计划时跑过一个探针（已删），产线路径的实测结果：

| 阶段 | 窗口条数 |
|---|---|
| 压缩前 | 11 |
| 压缩后 | 4 |
| 重连后 | **11** |

重连后的窗口是「全部原文 + 那条摘要」，比压缩前更大。`ws_compaction.go::loadSession` 走 `store.Store.Messages`，那是无差别的 `SELECT … ORDER BY seq ASC`，投影层根本不存在。

**你的第一步是把这个探针重建成常驻回归测试**，先看它红。

- [ ] **Step 1: 写失败的回归测试（重连必须保持压缩）**

在 `internal/api/http/ws_persist_test.go` 追加。夹具形状照抄同文件里 `TestMaybeAutoCompact_EvictsWhenPersistSucceeds` 的 —— 那是【实测】跑通的形状，`evictableHistory(8)` + 一组 tool call，`CompactionConfig{Model:"fm", Threshold:0.05, ContextWindow:4000, KeepRecent:1}`，用 `newWSPair(t)` 拿 `wc`。

```go
// TestReconnectPreservesCompaction: 压缩后重连必须拿回压缩后的窗口，而不是
// 全部原文。C1 保证了原文写得下来，但没有人读那份原文的投影 —— loadSession
// 裸读 Messages()，于是压缩在每次断线重连时被完整撤销，摘要钱白付，下一轮
// 再压一次再付一次。
func TestReconnectPreservesCompaction(t *testing.T) {
	st := persistStore(t)
	sid, err := st.CreateSession("s")
	require.NoError(t, err)
	fm := einollm.NewFakeModel([]string{"SUMMARY"}, nil)
	srv := &Server{
		store:      st,
		compaction: CompactionConfig{Model: "fm", Threshold: 0.05, ContextWindow: 4000, KeepRecent: 1},
	}
	cs := &connSession{perm: &permModeState{}, sessionID: sid}
	cs.history = append(evictableHistory(8),
		&schema.Message{Role: schema.Assistant,
			ToolCalls: []schema.ToolCall{toolCall("c1", "shell_run", `{"cmd":"go build ./..."}`)}},
		&schema.Message{Role: schema.Tool, ToolCallID: "c1", ToolName: "shell_run",
			Content: "build finished in 3s"})

	wc, client, cleanup := newWSPair(t)
	defer cleanup()
	_ = client
	maybeAutoCompact(context.Background(), srv,
		map[string]model.BaseChatModel{"fm": fm}, wc, cs)
	compacted := len(cs.history)
	require.Less(t, compacted, 11, "compaction must actually fire on this fixture")

	cs.persistMessages(srv) // turn end flushes the COMPACTED window

	fresh := &connSession{perm: &permModeState{}}
	require.NoError(t, fresh.loadSession(srv, sid))

	require.Len(t, fresh.history, compacted,
		"reconnect restored %d messages but the compacted window was %d: the "+
			"projection is missing, so every reconnect undoes compaction and the "+
			"summary is paid for again next turn", len(fresh.history), compacted)
	require.Contains(t, fresh.history[len(fresh.history)-1].Content, "SUMMARY",
		"the summary must survive the projection")
}
```

- [ ] **Step 2: 跑它，确认红**

```sh
go test ./internal/api/http -run TestReconnectPreservesCompaction -v
```

预期：`reconnect restored 11 messages but the compacted window was 4`。**如果它绿了，停下来报告** —— 说明代码已经变了，本任务的前提不成立。

- [ ] **Step 3: 建表**

【实测】DDL，追加进 `internal/store/store.go` 的 schema 常量（跟在 `idx_messages_session` 那条之后）：

```sql
-- INF3 (ADR-0015): compaction markers. The messages table stays byte-identical
-- and append-only; what changes is that the active window is now a PROJECTION
-- over it rather than a raw SELECT. A 'compact' event says "seq < hidden_seq no
-- longer enters the window"; an 'undo' event pops the most recent one. Nothing
-- is ever updated or deleted here — reverting is expressed by appending.
CREATE TABLE IF NOT EXISTS context_events (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id  TEXT    NOT NULL,
    kind        TEXT    NOT NULL,
    hidden_seq  INTEGER NOT NULL,
    created_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_context_events_session
    ON context_events(session_id, id);
```

先确认那个 schema 常量的确切名字与它是否走 `IF NOT EXISTS`（旧库要能无痛升级）。**如果 store 有显式迁移版本号机制，用它而不是裸 `IF NOT EXISTS`** —— 读 `internal/store/store.go` 判断。

- [ ] **Step 4: 写 `internal/store/context_events.go`**

【设计】三个导出函数 + 常量。要点：

- `HiddenSeq`：`SELECT kind, hidden_seq FROM context_events WHERE session_id=? ORDER BY id ASC`，遍历折叠 —— `compact` 压栈、`undo` 弹栈（空栈时 `undo` 静默忽略，不报错：撤到底就是原文）。返回栈顶，空栈返回 0。
- `ProjectWindow`：拿 `HiddenSeq`，再 `SELECT <messageColumns> FROM messages WHERE session_id=? AND seq>=? ORDER BY seq ASC`。**复用 `messageColumns` 常量与现有的行扫描辅助**，不要复制一份扫描代码（DRY 是仓库约定，重复会被评审拒）。
- `AppendContextEvent`：走 `s.WriteTx`，一句 INSERT。校验 `kind` 只接受两个常量之一，别的返回 error。

**承重的一点**：`hidden_seq == 0` 时 `ProjectWindow` 的 SQL 与 `Messages` 逐字节等价 —— 这就是 ADR-0015 约束 2 的实现方式，别引入任何额外过滤。

- [ ] **Step 5: 写 `internal/store/context_events_test.go`**

至少五条：

1. **无事件 ⇒ 投影 == Messages()**（约束 2 的常驻回归）。写 10 条消息，`ProjectWindow` 与 `Messages` 用 `require.Equal` 整体比较，不是比长度。
2. `compact(hidden_seq=5)` ⇒ 投影只剩 seq>=5 的行。
3. `compact(5)` 后 `undo` ⇒ 投影恢复为全部行，且与 `Messages()` 相等。
4. `compact(3) → compact(7) → undo` ⇒ 投影为 seq>=3（弹一层回到前一个 compact，不是回到 0）。
5. `AppendContextEvent` 拒绝未知 kind。

再加一条**机制断言**：用 `go/ast` 或直接 `grep` 都行，但更稳的是在 `internal/archtest` 之外、本包内断言 —— 扫 `internal/store/*.go` 非测试文件，`UPDATE context_events` 与 `DELETE FROM context_events` 必须零命中（`DeleteSession` 的级联除外，若你加了级联就在断言里显式豁免那一处并写明理由）。这是 ADR-0015 约束 1 的机器强制。

- [ ] **Step 6: 跑 store 测试**

```sh
go test ./internal/store -v -run 'ContextEvent|ProjectWindow'
```

- [ ] **Step 7: 接线 —— 压缩路径写标记**

`internal/api/http/ws_compaction.go`。`maybeAutoCompact` 与 `compactNow` 两处都要，**抽成一个共享辅助**（重复逻辑必须抽公共函数，这是仓库约定）：

```go
// markCompacted records the compaction boundary so the window survives a
// reconnect. It runs AFTER the post-compaction flush, because hidden_seq is
// derived from where that flush left the sequence: the compacted window
// occupies the last `kept` rows, so everything before them is superseded.
//
// A failed event write is treated exactly like a failed history flush — the
// caller must not report success. Compacting without recording the marker is
// strictly worse than not compacting: the next reconnect pulls back everything
// that was just evicted, i.e. today's bug, but now with a summary paid for.
func (cs *connSession) markCompacted(s *Server, newHist []*schema.Message) bool
```

【设计】算法：`cs.flushHistory(s)` 之后 `cs.seq` 是下一个可用 seq；`kept := len(storeMessagesFor(newHist))`；`hidden := cs.seq - kept`；`hidden < 0` 时钳到 0。然后 `AppendContextEvent(cs.sessionID, ContextEventCompact, hidden)`。

**顺序是承重的**：先 `cs.history = newHist`，再 `flushHistory`（把摘要写进日志），再 `markCompacted`。写计划时实测过，摘要是 `Assemble` 放在**窗口末尾**的一条 user+sentinel 消息，所以它 flush 后拿到最大的 seq，落在保留区间内。**先自己验证这个顺序下 hidden 算出来是对的**，别照抄一个数。

`s.store == nil` 或 `cs.sessionID == ""` 或 `cs.recordingSuppressed()` 时直接返回 true（与 `flushHistory` 的短路条件一致）。

- [ ] **Step 8: 接线 —— `loadSession` 改走投影**

把 `msgs, err := s.store.Messages(sessionID)` 换成 `s.store.ProjectWindow(sessionID)`。**只改这一处** —— `Messages` 还有别的调用方（会话导出、fork、revert 快照），它们要的就是全部原文，不要一起改。改之前 `grep -rn "\.Messages(" internal --include='*.go' | grep -v _test.go` 确认每个调用方该走哪条。

- [ ] **Step 9: 跑 Step 1 的回归测试，确认绿**

```sh
go test ./internal/api/http -run TestReconnectPreservesCompaction -v
```

- [ ] **Step 10: 跑全量 + 门禁**

```sh
go test ./internal/store ./internal/api/http ./internal/ctxcompact
go test ./internal/archtest ./internal/bootstrap
go test ./...
```

**`go test ./...` 必须零 FAIL。** 投影改了会话恢复语义，会话导出 / fork / revert 的现有测试是这次最可能被打穿的地方 —— 如果有红的，先判断是「我改错了」还是「那条测试钉住的正是这个 bug」，再动手。

- [ ] **Step 11: Commit**

```sh
git add internal/store/context_events.go internal/store/context_events_test.go \
        internal/store/store.go internal/api/http/ws_compaction.go \
        internal/api/http/ws_persist_test.go
git commit -m "feat(store,api): rebuild the active window from an append-only event log

Compaction was undone by every reconnect: loadSession read messages with a
raw SELECT, so the pre-compaction originals and the summary both came back
and the window ended up LARGER than before compaction. Measured 11 -> 4 -> 11.

context_events records the compaction boundary; ProjectWindow folds the event
stream and reads the surviving range. Sessions with no events project
byte-identically to the old path.

Refs: ADR-0015, W-D-01, W-D-05"
```

---

### Task 2: 跨会话历史检索

**覆盖** W-D-02（`A16`）

**Files:**
- Modify: `internal/store/message_log.go`
- Test: `internal/store/message_log_test.go`

**Interfaces:**
- Consumes: Task 1 的 `ProjectWindow`（仅在你决定让检索跳过已隐藏区间时；默认**不跳过** —— 见下）
- Produces: `MessageSearchHit` 增加会话标识与时间戳字段（若尚无）

#### 现状【实测】

`internal/store/message_log.go:301`：

```go
func (s *Store) SearchMessages(sessionID, query string, limit int) ([]MessageSearchHit, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("store: search messages: empty session id")
	}
```

强制非空 sessionID ⇒ 无法回答「上周那个 bug 怎么修的」。W-A-03 已经修好的 CJK 路径在 `:308` 分流到 `searchMessagesCJK`，**跨会话必须同样走它**，否则中文查询在新路径上零命中。

- [ ] **Step 1: 写失败的测试**

两条：

```go
// TestSearchMessages_AcrossSessions: 空 sessionID 从"报错"改为"跨全部会话检索"。
func TestSearchMessages_AcrossSessions(t *testing.T)
// TestSearchMessages_AcrossSessionsCJK: 跨会话路径必须走 W-A-03 的 CJK 分流，
// 否则中文查询零命中 —— FTS5 的默认分词器不切 CJK。
func TestSearchMessages_AcrossSessionsCJK(t *testing.T)
```

夹具：建 **3 个** 会话各写几条消息，其中至少一条命中词只出现在会话 B。断言：结果非空、`limit` 生效、每条命中带得出它来自哪个会话、且**跨会话结果里确实出现了不止一个会话的行**（只有一个会话有结果的夹具证明不了「跨」）。

- [ ] **Step 2: 跑，确认红**（预期 `empty session id`）

- [ ] **Step 3: 实现**

【设计】把 `sessionID == ""` 从 error 改成「不加会话过滤」。两条 SQL 都要改：FTS 那条去掉 `AND m.session_id = ?`，`searchMessagesCJK` 里对应的 LIKE 那条同理。做法上**优先用条件拼接而不是复制一份 SQL** —— 本文件已有 `likeAnyTermClause` 这类拼接辅助，跟随它的风格。

排序：spec 要求「相关度 + 时近度」。FTS 路径当前是 `ORDER BY rank`；跨会话时同分的行按 `m.created_at DESC` 次级排序。**别引入一个加权公式** —— 没有可校准的依据，两级排序是能解释的最简形状。

上限：`clampLimit(limit)` 已存在，跨会话沿用。spec 说「上限可配」—— 先确认 `clampLimit` 的常量在哪、是否已经可配；**若已经可配就不要再加一层配置**（YAGNI）。若写死，把它提成 `internal/store` 的包级变量或 `OpenOptions` 字段，取当前值作默认。

**关于「搜不搜已隐藏区间」**：默认**搜**。跨会话历史检索的用途正是找回被压缩掉的原文 —— 按投影过滤会让这个特性自我否定。在函数 doc 注释里写明这一点，否则下一个人会以为是漏了。

- [ ] **Step 4: 跑绿；跑 `go test ./internal/store`**
- [ ] **Step 5: 检查是否有调用方依赖旧的 error 行为**

```sh
grep -rn "SearchMessages(" internal cmd --include='*.go' | grep -v _test.go
```

有工具层调用方的话，确认它传空串时的新行为是否合理（大概率更合理，但要看一眼）。

- [ ] **Step 6: Commit**

```sh
git commit -m "feat(store): search messages across all sessions when session id is empty

An empty session id returned an error, so 'how did we fix that bug last week'
had no query that could answer it. It now means 'all sessions', reusing the
CJK fallback from W-A-03 so Chinese queries do not silently return nothing.

Refs: W-D-02"
```

---

### Task 3: 会话列表游标分页 + 状态库损坏自愈

**覆盖** W-D-10（`B39`）、W-D-11（`B41`）—— 两条都在 `internal/store`、都不依赖 INF3、都是自包含的小改，合为一个任务。

**Files:**
- Modify: `internal/store/session_list.go`（分页）
- Modify: `internal/store/store.go`（`OpenWith` 的损坏自愈）
- Test: `internal/store/session_list_test.go`、`internal/store/store_test.go`

#### 3a. 游标分页

【实测】`internal/store/session_list.go` 的 `listSessionsWhere(where string, limit int)` 是所有列表路径的收口：

```go
q := "SELECT " + sessionColumns + " FROM sessions " + where + " ORDER BY updated_at DESC"
if limit > 0 { q += " LIMIT ?" }
```

所以：**排序键是 `updated_at DESC`**，当前**连 OFFSET 都没有**（只有 LIMIT）。游标就是 `(updated_at, id)`。`ListSessions` 与 `ListArchivedSessions` 都走这个函数，它的 doc 注释明说列名/扫描顺序/ORDER BY 集中在这里是为了让两条路径不漂 —— **你的分页也要走它，不要另起一条 SELECT**。

- [ ] **Step 1: 写失败的测试**

```go
// TestListSessions_CursorPaginationIsStable: 游标分页在翻页过程中有新会话
// 插入时不重不漏。OFFSET 分页做不到这一点 —— 新行插在前面会把第 2 页的
// 第一条挤成第 1 页的最后一条，用户看到重复。
func TestListSessions_CursorPaginationIsStable(t *testing.T)
```

夹具：建 5 个会话，取第 1 页（size=2），**然后再建 2 个新会话**，再用游标取第 2 页。断言：两页并集恰好是最初 5 个里的前 4 个，无重复、无遗漏。这个「翻页中途插入」的步骤是这条测试的全部意义，别省。

- [ ] **Step 2: 实现**

【设计】游标 = 排序键的值（大概率是 `created_at` 或 `updated_at`，读代码确认）+ `id` 做 tie-break。`WHERE (sort_key, id) < (?, ?)` 形式，SQLite 支持行值比较。返回下一页游标（不透明字符串即可，`base64(sortkey|id)`）。

**向后兼容**：现有签名不能破。加一个新函数或给现有的加可选参数结构体，不要改已有调用方的行为。改之前 `grep -rn "ListSessions(" internal cmd --include='*.go'`。

#### 3b. 损坏自愈

【实测】`internal/store/store.go:112` `func OpenWith(path string, opts OpenOptions) (*Store, error)`。

- [ ] **Step 3: 写失败的测试**

```go
// TestOpenWith_RecoversFromCorruptDatabase: 一个损坏的库文件必须让进程正常
// 启动而不是拒绝启动。yanshi 是单二进制本地部署,一个坏掉的 yanshi.db 当前
// 会让 TUI 完全起不来,而用户手上没有任何工具去修它。
func TestOpenWith_RecoversFromCorruptDatabase(t *testing.T)
```

夹具：写一个**真的**损坏的 SQLite 文件（写入随机字节，或写一个合法 header 后跟垃圾），`OpenWith` 它。断言：返回可用的 `*Store`（能 `CreateSession`）、原文件被移到带时间戳的备份路径、备份路径出现在日志里。

**别用 mock 制造损坏** —— 真写坏字节，否则测的是你的假设不是 SQLite 的行为。先手工跑一次确认你造的文件真的会让 `sql.Open`+ping 或第一条 DDL 失败；SQLite 对损坏很宽容，随机字节不一定触发。

- [ ] **Step 4: 实现**

【设计】`OpenWith` 里在建表之后加一次完整性探测（`PRAGMA integrity_check` 或直接看建表是否报 `database disk image is malformed`）。失败则：关句柄 → `os.Rename` 到 `<path>.corrupt-<unix>` → 重新 `Open` 一次全新库 → `slog.Warn` 记下备份路径。

**只在自愈成功后才返回 nil error。** 重建仍失败就返回原错误 —— 静默给出一个不能用的 Store 比拒绝启动更糟。

- [ ] **Step 5: 跑绿，`go test ./internal/store`，commit**

```sh
git commit -m "feat(store): cursor pagination for session lists, and self-healing on a corrupt database

Refs: W-D-10, W-D-11"
```

---

### Task 4: 跨会话记忆自动生成

**覆盖** W-D-03（`A15`）

**Files:**
- Create: `internal/store/memory_worker.go` 或 `internal/agent/memworker/`（位置见下）
- Modify: `internal/bootstrap/`（装配 worker）
- Test: 同包测试

**Interfaces:**
- Consumes: Task 1 的 `ProjectWindow` / `HiddenSeq`；W-A-05 已接线的 `internal/tools/memory_distill.go::DistillMemories`
- Produces: worker 的构造函数与 `Build*`

#### 现状【实测】

只有模型主动调 `memory_write` 才会产生记忆 ⇒ **自驱动 goalloop 跑完不留任何长期资产**。W-A-05 已经把蒸馏入口接上了（`DistillMemories`），但触发者仍然只有模型自己。

#### 【实测，写计划后复核补入】蒸馏入口的签名与一个夹具陷阱

```go
func DistillMemories(ctx context.Context, s *store.Store, m DistillModel,
	dims store.MemoryFilter) (DistillResult, error)

type DistillModel interface {
	Generate(ctx context.Context, msgs []*schema.Message, opts ...model.Option) (*schema.Message, error)
}

const MinDistillBatch = 6 // 候选少于 6 条时,DistillMemories 直接返回不做任何事
```

**`MinDistillBatch = 6` 是这个任务最可能踩的坑。** 一个只写了 2、3 条候选记忆的测试夹具会让 Phase2 静默空转，而测试仍然「通过」——因为它断言的是「没报错」。**Phase2 的测试夹具必须产出至少 6 条候选**，否则你验证的是短路分支不是蒸馏。

`DistillMemories` 直接吃 `*store.Store`（不是接口），所以你的 worker 也可以直接持有 store，不需要为它造一层抽象。

- [ ] **Step 1: 先读三处，再动手**

1. `internal/tools/memory_distill.go` —— 上面的签名已核，读它是为了看 `DistillResult.Skipped` 与 `dims` 怎么用。
2. `internal/store/memory.go` —— `WriteMemoryScoped` / `MemoryFilter` 的维度字段。
3. `internal/bootstrap/bootstrap.go` 里已有的后台组件是怎么起停的（找 `go func` / `context.WithCancel` / `App.Shutdown` 的现有模式），**跟随它**，别发明第二套生命周期。

- [ ] **Step 2: 写失败的测试**

四条，对应 spec 的四句验收：

```go
// 1. 会话结束后产生记忆行,且触发者是 worker 而不是模型工具调用。
func TestMemoryWorker_ProducesMemoriesOnSessionEnd(t *testing.T)
// 2. 同一会话被并发进程处理时只抽取一次(租约认领)。
func TestMemoryWorker_LeaseIsExclusive(t *testing.T)
// 3. 配额上限生效后,旧的未用记忆被剪裁。
func TestMemoryWorker_QuotaPrunesUnusedMemories(t *testing.T)
// 4. Phase2 复用 W-A-05 的蒸馏入口而不是新建一条路径。
func TestMemoryWorker_Phase2CallsDistillEntrypoint(t *testing.T)
```

第 2 条是这个任务里最容易写假的：**必须真起两个 goroutine 争同一个会话**，断言只有一个拿到租约、另一个拿到「已被认领」。用 sleep 制造的顺序不算并发测试。

第 4 条**不要用 spy 或接口断言**（那只能证明「有个东西被调了」）。断言可观察的产物：Phase2 跑完后记忆的内容/形状与直接调 `DistillMemories` 一致。

- [ ] **Step 3: 实现**

【设计】

- **租约**：`memory_leases(session_id TEXT PRIMARY KEY, holder TEXT, expires_at INTEGER)`，认领 = `INSERT … ON CONFLICT DO UPDATE … WHERE expires_at < ?`，看 `RowsAffected`。这是纯 SQL 的原子认领，不需要额外锁。
- **Phase1**：`ProjectWindow(sessionID)` 拿窗口 → 抽候选。抽取本身用什么模型/规则，跟随 `DistillMemories` 已有的做法。
- **Phase2**：调 `DistillMemories`。
- **热度排序 / 未用剪裁 / 配额**：`memories` 表加 `last_used_at` 与 `use_count`（若无）。配额超了删 `use_count = 0` 且最老的。**配额零值 = 不限制**（与引入前一致）。
- **触发**：会话结束。找 WS 层的会话关闭点接一个回调，或 worker 定期扫「`updated_at` 早于 N 分钟且未认领」的会话。**后者更稳** —— 断线的会话没有干净的结束事件。

- [ ] **Step 4: 装配进 `bootstrap.Build`**

GOV4 会检查 `Build*` 可达。装配失败按 CLAUDE.md 的既定做法：**打 stderr 并以该子系统禁用的方式继续**，不要拒绝整个启动。

- [ ] **Step 5: 跑绿，`go test ./internal/archtest ./internal/bootstrap ./...`，commit**

```sh
git commit -m "feat(store,bootstrap): background worker that distills long-term memories from finished sessions

A self-driving goal loop left no durable asset behind: memories only existed
when the model chose to call memory_write. Phase 1 extracts candidates per
session under a SQL lease so two processes cannot double-extract; Phase 2
reuses the W-A-05 distill entrypoint.

Refs: W-D-03"
```

---

### Task 5: 冷会话压缩归档 + 保留期

**覆盖** W-D-04（`A17`）

**Files:**
- Create: `internal/store/archive.go`、`internal/store/archive_test.go`
- Modify: `internal/store/store.go`（归档表/列）、`internal/config/config.go`（保留期配置）、`internal/bootstrap/`（worker 装配）

**Interfaces:**
- Consumes: Task 1 的 `ProjectWindow`（读取侧要透明解压）

- [ ] **Step 1: 写失败的测试**（四条，逐句对应 spec 验收）

```go
// 冷分片被压缩后占用显著下降。
func TestArchive_ShrinksColdSessions(t *testing.T)
// 读取已压缩分片的结果与压缩前逐条一致。
func TestArchive_ReadsBackIdentical(t *testing.T)
// 保留期为零时不删除任何数据(与引入前一致)。
func TestArchive_ZeroRetentionDeletesNothing(t *testing.T)
// 归档进行中的读取不返回错误。
func TestArchive_ConcurrentReadDuringArchiveSucceeds(t *testing.T)
```

第 2 条用 `require.Equal` 比较**整个消息切片**，不是比长度或比第一条。
第 4 条要真并发：一个 goroutine 归档、一个循环读，读侧任何一次 error 都判失败。

- [ ] **Step 2: 实现**

【设计】

- 压缩用 **`compress/gzip`**（标准库）。【实测】`go.mod` 里没有 `klauspost/compress`，zstd 不可用，且不值得为归档新增一条依赖。在注释里写明这个选择。
- 存储形状：`messages` 表加 `archived_blob BLOB` 不合适（行级压缩省不了多少）。**按会话整体压**：新表 `archived_sessions(session_id TEXT PRIMARY KEY, blob BLOB, message_count INTEGER, archived_at INTEGER)`，归档时把该会话全部消息序列化+压缩写进去，然后删 `messages` 里的行。

  ⚠️ **这与 ADR-0015 约束 1 的关系必须想清楚再写**：约束 1 管的是 `context_events`，`messages` 不在其列。但 C1 的「持久化先行」精神要求归档必须是**先写 blob、验证读得回来、再删行**，且在同一个 `WriteTx` 里。把这条写进代码注释。
- 读取侧：`ProjectWindow` / `Messages` 在 `messages` 查不到行时回退查 `archived_sessions` 解压。**回退是透明的**，调用方无感。
- 保留期：`config` 加 `storage.retention_days`，**零值 = 永久保留**。归档 worker 只归档「`updated_at` 早于 retention 且不是当前活动会话」的。**保留期到期是归档不是删除** —— spec 说「保留期为零时不删除任何数据」，反过来说非零时会删；但删什么要谨慎，本任务只做归档，删除留给显式的 `DeleteSession`。在注释里写明这个边界。

- [ ] **Step 3: 跑绿；重跑 Task 1 的投影测试**（归档改了读路径，那条回归必须还绿）

- [ ] **Step 4: 重跑生成的文档**（改了 `config.Config` ⇒ CI 的 docs diff-gate 会红）

```sh
go run ./cmd/gendocs -config docs/user-guide/configuration.md
```

- [ ] **Step 5: commit**

```sh
git commit -m "feat(store,config): archive cold sessions and honour a retention period

yanshi.db grew without bound on a single-binary local deployment. Cold
sessions are now serialised and compressed into one blob per session; reads
fall back to the archive transparently. Zero retention keeps everything,
matching pre-change behaviour exactly.

Refs: W-D-04"
```

---

### Task 6: 记忆引用溯源 + 一键清空记忆

**覆盖** W-D-07（`B35`）、W-D-12（`B36`）—— 都在记忆子系统，合为一个任务。

**Files:**
- Modify: `internal/store/memory.go`、`internal/store/store.go`
- Modify: `internal/cli/tui/commands.go`（清空的斜杠命令）或 `internal/tools/`（工具形态）
- Test: `internal/store/memory_test.go`、TUI 命令测试

#### 6a. 溯源

- [ ] **Step 1: 写失败的测试**

```go
// TestMemory_TracesBackToSourceLog: 每条记忆可回溯到产生它的日志位置。
func TestMemory_TracesBackToSourceLog(t *testing.T)
// TestMemory_TraceResolvesAfterArchive: 溯源目标被归档后仍可解析
// —— 这是 spec 明写的第二句,而 Task 5 的归档正是会打穿它的东西。
func TestMemory_TraceResolvesAfterArchive(t *testing.T)
```

第 2 条**必须真的跑一次 Task 5 的归档**再解析，不是伪造一个「已归档」标志。

- [ ] **Step 2: 实现**

【设计】`memories` 表加 `source_session_id TEXT` + `source_seq INTEGER`（零值 = 无溯源，向后兼容旧行）。写入侧：Task 4 的 worker 与 `memory_write` 工具在有会话上下文时填上。解析侧：一个 `func (s *Store) MemorySource(memoryID string) ([]Message, error)`，走 `ProjectWindow` 的同一条归档回退路径 —— **复用，不要复制**。

#### 6b. 一键清空

- [ ] **Step 3: 写失败的测试**

```go
// 清空需二次确认;按维度(project/agent)可选清空;清空后自动召回不再命中。
func TestClearMemories_RequiresConfirmation(t *testing.T)
func TestClearMemories_ScopedByDimension(t *testing.T)
func TestClearMemories_AutoRecallMissesAfterClear(t *testing.T)
```

第 3 条是防「零读者」的：清空了但自动召回还从别处命中，等于没清。**必须真调自动召回路径**（W-A-03 修好的那条），不是查表看行数。

- [ ] **Step 4: 实现**

【设计】`func (s *Store) ClearMemories(dims MemoryFilter) (int, error)`，复用现有 `MemoryFilter` 的维度拼接（`internal/store/memory.go` 里已有 `sb.WriteString(" AND " + alias + "agent_id = ?")` 这类）。**空 filter = 清全部**，所以二次确认是必须的。

载体：优先做成**斜杠命令**（`/memory-clear`）而不是模型可调的工具 —— 清空记忆是用户意图，不该由模型自己决定。若做成斜杠命令，**必须注册进 `internal/cli/tui/commands.go` 的 `commandTable`**，否则 `TestPhantomSlashCommandsNotAdvertised` 会在你写文档时抓到幻影。

二次确认走 TUI 的现有确认机制 —— 先找一个已有的确认流照做，别新发明。

- [ ] **Step 5: 跑绿；`go test ./internal/archtest`（幻影命令门禁）；commit**

```sh
git commit -m "feat(store,tui): memory provenance, and a confirmed one-shot memory wipe

Refs: W-D-07, W-D-12"
```

---

### Task 7: 持久化用户消息队列

**覆盖** W-D-08（`B75`）

**Files:**
- Create: `internal/store/msgqueue.go`、测试
- Create: `cmd/yanshi/` 下的新子命令
- Modify: `internal/api/http/ws.go`（会话恢复时消费队列）

- [ ] **Step 1: 写失败的测试**（三条，逐句对应 spec 验收）

```go
// 可向运行中或离线会话排消息。
func TestMessageQueue_EnqueueToOfflineSession(t *testing.T)
// 跨进程可见(两个 Store 句柄指向同一个 db 文件)。
func TestMessageQueue_VisibleAcrossProcesses(t *testing.T)
// 会话恢复后队列消息被消费。
func TestMessageQueue_ConsumedOnSessionResume(t *testing.T)
```

第 2 条**必须开两个 `store.Open` 指向同一路径**，不是同一个句柄用两次 —— 那测的是 map 不是数据库。

- [ ] **Step 2: 先看能不能复用，再决定建不建表**

【实测】`internal/store/task.go` 已经有一套 task broker 的排队机制（`cmd/agent-worker` 消费它）。**先读它**，判断「向会话排消息」能不能表达成它的一种 task，而不是第二套队列。

能复用就复用并在报告里说明；确实语义不同（task 是给 worker 执行的工作项，本任务的队列是给会话消费的用户输入）就建新表，同样在注释里写明为什么不能复用——**这句注释是给下一个人看的，它防的是第三套队列**。

- [ ] **Step 3: 实现（若确认要建表）**

【设计】`queued_messages(id INTEGER PK AUTOINCREMENT, session_id TEXT, content TEXT, created_at INTEGER, consumed_at INTEGER DEFAULT 0)`。入队纯 INSERT；消费时 `UPDATE … SET consumed_at=?`（这张表**不受 ADR-0015 约束 1 管辖** —— 那条只管 `context_events`；在注释里写明这个边界，免得下一个人以为违规）。

子命令：`yanshi enqueue <session-id> <message>`。**新子命令改了 `-h` 文本 ⇒ 必须重跑 `go run ./cmd/gendocs -help-all docs/user-guide/tui.md docs/user-guide/entrypoints.md`**，否则 CI 的 docs diff-gate 会红。

消费点：`loadSession` 之后、第一条 user_message 之前。

- [ ] **Step 3: 跑绿；重跑 gendocs；commit**

```sh
git commit -m "feat(store,cli): durable message queue for running and offline sessions

Refs: W-D-08"
```

---

### Task 8: 输入历史的字节上限与多进程安全

**覆盖** W-D-09（`B37`）

> **本任务的范围被写计划后的复核推翻过一次，请读这段。**
>
> 计划初稿说「输入历史是纯内存，要落盘」，并让实施者把它搬进 SQLite。**那是错的。**【实测】`internal/cli/tui/history.go` 已经有完整的 JSONL 持久化：`LoadHistory` / `History.Add` / `History.Save`，`historyPath()` 返回 `os.UserConfigDir()/yanshi/history.jsonl`（**已经是全局的**，不按项目分），500 条上限、重复项移到尾部、tmp+随机后缀+原子 rename、读取时坏行跳过自愈。
>
> **不要把它重写进 SQLite。** 那是把一个能工作的东西返工一遍，只为满足一句写错的计划。
>
> 剩下的两个缺口是真的，本任务只做这两个：

**Files:**
- Modify: `internal/cli/tui/history.go`
- Test: `internal/cli/tui/history_test.go`

#### 缺口 1：上限是条数不是字节

`defaultHistoryCap = 500` 是**条数**。一条 100 KB 的粘贴 prompt ×500 = 50 MB 的 history.jsonl，每次 TUI 启动全量读进内存。

#### 缺口 2：多进程同时 Save 会丢历史

`History.Save` 是「全量重写 + rename」。rename 是原子的，所以文件不会**损坏** —— 但两个 yanshi 进程（多窗口是本仓的常规用法，见 CLAUDE.md 的后端发现一节）同时 Save 时，后写的整份覆盖先写的，另一个窗口这一整场会话的输入历史**静默消失**。

- [ ] **Step 1: 先自己确认上面两条**

读 `internal/cli/tui/history.go` 全文。**如果你读到的与上面不符，以代码为准并在报告里记一条。**

- [ ] **Step 2: 写失败的测试**

```go
// TestHistory_TrimsAtByteLimit: 上限必须同时是条数和字节。单条超大 prompt
// 会让 500 条上限失去意义 —— 每次启动都要把几十 MB 读进内存。
func TestHistory_TrimsAtByteLimit(t *testing.T)

// TestHistory_ConcurrentSavesDoNotLoseEntries: 两个进程各自 Add 后 Save,
// 后写的不得整份覆盖先写的。多窗口是本仓的常规用法,今天第二个窗口一保存,
// 第一个窗口这一整场的输入历史就没了。
func TestHistory_ConcurrentSavesDoNotLoseEntries(t *testing.T)
```

第 2 条的夹具：**两个独立的 `*History` 实例指向同一路径**（这才是两个进程；同一实例并发只测到了 `h.mu`）。各自 `LoadHistory` → 各自 `Add` 不同内容 → 各自 `Save` → 第三次 `LoadHistory` 读回来，断言**两边的条目都在**。

- [ ] **Step 3: 跑，确认红**

- [ ] **Step 4: 实现**

【设计】

- **字节上限**：加 `defaultHistoryBytes`（取一个能解释的值，比如 2 MiB，并在注释里写明理由）。裁剪在 `Add` 里，条数与字节两个上限都从最老端裁。**单条就超上限时**要有明确行为 —— 截断还是拒绝存，选一个并写进注释，别让它变成无限循环。
- **多进程**：`Save` 改成「读回磁盘上的当前内容 → 与内存条目按时间戳归并去重 → 再写」。归并的排序键用 `historyItem.TS`，去重沿用现有的「重复项移到尾部」语义。
  - 这仍不是完全的并发安全（两个进程在读与写之间交错仍可能丢一条），但把「丢一整场」降到「极窄窗口里丢一条」。**在注释里如实写明这个上限**，别宣称解决了它。
  - 不要为此引入文件锁：跨平台文件锁在本仓要新写一套（`internal/lockfile` 那套是给后端选举用的，语义不同），代价远大于它买到的东西。

- [ ] **Step 5: 跑绿；`go test ./internal/cli/tui`；commit**

```sh
git commit -m "fix(tui): bound input history by bytes, and stop one window from erasing another's

The 500-entry cap was entry-count only, so a handful of large pasted prompts
put tens of megabytes on the startup read path. And Save rewrote the whole
file, so with two windows open the second save silently dropped everything
the first had recorded. Save now merges with what is on disk before writing.

Refs: W-D-09"
```

---

### Task 9: 检查点 / 快照系统

**覆盖** W-D-06（`B40`）

**Files:**
- Create: `internal/store/checkpoint.go` + 测试
- Modify: `internal/vcs/`（文件维度的快照复用现有能力）

- [ ] **Step 1: 写失败的测试**（四条，逐句对应 spec 验收）

```go
// 可选择性恢复「会话/记忆/文件」之一。
func TestCheckpoint_RestoresSelectedDimensionOnly(t *testing.T)
// 恢复前自动快照。
func TestCheckpoint_SnapshotsBeforeRestore(t *testing.T)
// 恢复期暂停写者。
func TestCheckpoint_PausesWritersDuringRestore(t *testing.T)
// dry-run 先出计划。
func TestCheckpoint_DryRunProducesPlanWithoutMutating(t *testing.T)
```

第 4 条断言 dry-run 后**数据库内容逐字节未变**，不只是「返回了一个计划」。

- [ ] **Step 2: 实现**

【设计】

- 会话维度：`context_events` 天然是快照序列 —— 检查点 = 记下当前最大 event id + 最大 seq，恢复 = append 一条把窗口拉回去的事件。**这是 ADR-0015 说的「检查点退化成多 append 一条」**，别另建快照表。
- 记忆维度：需要真快照（记忆表可变）。复用 Task 5 的序列化+压缩。
- 文件维度：**复用 `internal/vcs`**，不要重实现。W-A-08 刚修过 `MergeToMain` 的物化，读一眼那里的模式。
- 暂停写者：store 已有 `writeMu`，恢复期持写锁即可 —— 不要发明第二套锁。

【实测，写计划后复核补入】**本仓已有大量快照/恢复机制，本任务的主要工作是编排它们而不是造新的。** 动手前逐个看一眼：

- `internal/store/session.go` —— `SessionRevertSnapshot`、`snapshotSessionTx`、`TruncateSessionForRevert`、`RestoreSessionAfterFailedRevert`。**会话维度的快照原语已经齐了。**
- `internal/vcs/` —— `timeline.go`、`seam.go`、`preview.go`、`freeze.go`、`restore.go`、`revert.go`、`gc.go`。文件维度的快照、预览、冻结、恢复都在这里。`preview.go` 很可能就是你要的 dry-run。

**如果你发现某个维度已经完整实现，就在报告里说明并只补缺的那部分。** 「检查点系统」这个名字听起来像要造一套新东西，但按上面的清单，它更可能是一层统一入口 + 缺失的记忆维度。造第二套快照是本任务最大的风险。

- [ ] **Step 3: 跑绿；`go test ./internal/vcs ./internal/store`；commit**

---

### Task 10: 上下文片段可识别标记 + 初始上下文重注入策略

**覆盖** W-D-13（`B33`）、W-D-14（`B31`）—— 14 依赖 13，同包，合为一个任务。

**Files:**
- Modify: `internal/ctxcompact/`（片段标记）
- Modify: `internal/agent/orchestrator/`（重注入时机）

#### 现状【实测】—— 以及一条会被「统一」二字破坏的承重约束

已经有两个 sentinel，形态是统一的（`[yanshi:<kind>]\n` 前缀 + user 角色）：

```go
const EvictionMapSentinel = "[yanshi:evicted-context-map]\n"
// SummarySentinel 同形，在 summarize.go / assemble.go 各处使用
```

`internal/ctxcompact/assemble.go` 上那段 doc 注释明写了它们**为什么必须分得开**：

> It has its own sentinel rather than sharing SummarySentinel because a consumer already depends on telling the two apart: Plan short-circuits when history ENDS in a summary, and IsSummaryMessage is what decides that. A map wearing the summary's marker would be read as a summary, and any history ending in one would stop being compactable.

**所以「统一机制」不等于「统一 sentinel」。** 把两者合并成同一个标记会让 `Plan` 的短路判据失效 —— 以 map 结尾的历史会被误判为「已经压过了」，从此不再可压缩。

W-D-13 要做的是把这个**已经存在的形态**提炼成可复用的三件套（`MarkFragment` / `ParseFragments` / `StripFragments` + `Kind` 字段），让现有两个 sentinel 成为它的两个 kind，**判别函数 `IsSummaryMessage` / `IsEvictionMapMessage` 的语义一字不变**。

- [ ] **Step 0: 写一条守住这条约束的测试**

```go
// TestFragment_SummaryAndMapRemainDistinguishable: 统一片段机制不得让
// Plan 的短路判据失效。以 eviction map 结尾的历史必须仍然可压缩。
func TestFragment_SummaryAndMapRemainDistinguishable(t *testing.T)
```

断言：构造一个以 eviction map 结尾的历史，`Plan` 不短路（即仍然给出非空 `SummarizeIndices`）；再构造一个以 summary 结尾的，`Plan` 短路。**两个方向都要**，只测一边证明不了「分得开」。

- [ ] **Step 1: 写失败的测试**

```go
// 每条注入片段带 kind 与起止标记;可定位、可剥离、可去重。
func TestContextFragment_IsLocatableStrippableDedupable(t *testing.T)
// 现有 SummarySentinel 归入同一机制(而不是并存两套)。
func TestContextFragment_SummarySentinelUsesTheSameMechanism(t *testing.T)
// mid-turn 插在最后一条 user 前。
func TestReinject_MidTurnGoesBeforeLastUser(t *testing.T)
// pre-turn 清空下轮重注。
func TestReinject_PreTurnClearsAndReinjects(t *testing.T)
```

- [ ] **Step 2: 实现**

【设计】一个 `Fragment{Kind, Body}` 与 `MarkFragment/ParseFragments/StripFragments`。标记形态跟随现有 sentinel 的风格（`[yanshi:…]` 前缀，实测过的形状）。**去重按 (Kind, Body) 哈希**。

重注入两条路径**分别可验证**是 spec 明写的验收 —— 不要用一个共享 helper 把两条路径压成一条然后只测一次。

- [ ] **Step 3: 跑绿；`go test ./internal/ctxcompact ./internal/agent/orchestrator`；commit**

---

### Task 11: 逐 turn 增量重渲染系统提示

**覆盖** W-D-15（`B34`）

**Files:**
- Modify: `internal/agent/orchestrator/orchestrator.go`（系统提示构造）

#### 承重的坑（spec §3 明写）

系统提示当前在构造期一次拼死。而 `/model` 切换走 `runners sync.Map`，**以 `model.BaseChatModel` 指针为键缓存 runner**。改重渲染时机会碰到这个缓存 —— 必须确认新 runner 拿到的是**新提示**而不是缓存里的旧值。

- [ ] **Step 1: 写失败的测试**

```go
// 只发变化段(RFC 7386 merge patch)。
func TestSystemPrompt_SendsOnlyChangedSections(t *testing.T)
// /model 切换在当前 turn 内对模型可见 —— 这一条是 runners 缓存的正面探针。
func TestSystemPrompt_ModelSwitchVisibleWithinTurn(t *testing.T)
```

第 2 条**必须真的切一次 model 再看模型收到的 system 内容**（用 `FakeModel` 捕获入参），不是断言某个字段被更新了。

- [ ] **Step 2: 实现**

【设计】把系统提示拆成命名段，每 turn 重算并与上 turn 做 RFC 7386 merge patch 比较，只把变化段重发。段的边界跟随现有提示的自然分节。

**缓存交互**：读 `runnerFor` 与 `runners sync.Map` 的现有代码，判断是「提示变了要驱逐缓存」还是「提示不进 runner 而是每 turn 传」。**后者更简单也更对** —— 若可行，选它并在注释里写明为什么没动缓存。

【实测，写计划后复核补入】两处与 spec 的描述不同，以这里为准：

1. **缓存键不是「model 指针」而是 `runnerCacheKey`（per-model + per-mode）** —— `internal/agent/orchestrator/orchestrator.go` 里 `runners sync.Map` 的注释自己写着 "memoizes per-model+per-mode Runners, keyed by runnerCacheKey"。CLAUDE.md 那句「以 `model.BaseChatModel` 指针为键」已经漂了。
2. **驱逐机制已经存在**：同文件有 `FlushRunners`（`o.runners.Range` + `Delete`）。所以「驱逐缓存」这条路是现成的，不需要你新造。**先看它现有的调用方**（`grep -rn "FlushRunners(" internal --include='*.go'`）—— 如果 `/model` 切换已经在调它，那第二条验收可能已经部分成立，你要断言的是**系统提示**这一层而不是 runner 这一层。

- [ ] **Step 3: 跑绿；commit**

---

### Task 12: 目标跨会话恢复

**覆盖** W-D-16（`B74`）

**Files:**
- Modify: `internal/agent/goalloop/loop.go`、`types.go`
- Create: `internal/store/goal_state.go` + 测试

#### 现状【实测】

`internal/agent/goalloop/loop.go:112` `for iter := 1; iter <= l.cfg.Budget.MaxIterations; iter++`，`types.go:59` `MaxIterations int`。预算与 objective 全在内存 ⇒ **进程一重启，目标从头再跑一遍，预算重置**。

- [ ] **Step 1: 写失败的测试**

```go
// objective 与双预算存 SQLite;进程重启后 goal loop 从中断处续跑;预算不被重置。
func TestGoalLoop_ResumesAfterRestart(t *testing.T)
```

夹具：跑 N 轮 → 模拟重启（新建 `Loop` + 新 `Store` 句柄指向同一 db）→ 断言续跑的起点是 N+1、剩余预算是 `MaxIterations - N`。**必须真开第二个 Store 句柄**。

- [ ] **Step 2: 实现**

【设计】`goal_state(goal_id TEXT PK, objective TEXT, iteration INTEGER, budget_json TEXT, updated_at INTEGER)`。每轮结束 UPSERT。启动时按 goal_id 查，有则续。

【实测，写计划后复核补入】「双预算」核实为真：

```go
type Budget struct {
	MaxIterations int // maximum number of plan-implement-evaluate-judge cycles
	MaxTokens     int // token budget across all LLM calls
}
```

**两个都要存、都要不被重置**，`MaxTokens` 尤其容易漏 —— 迭代数肉眼可见，token 花掉多少不看数据库就不知道。测试要对**两个**字段都断言。

【实测】**`internal/agent/goalloop` 当前完全不 import `internal/store`。** 所以直接加一个 store 依赖是新增一条跨层依赖，GOV1 的 `portAllowlists` 会管它。**做法：定义一个窄接口（存/取 goal 状态）放在 `goalloop` 包内，由 `bootstrap` 注入 store 的实现。** 这与本仓既有的六边形布局一致，也避开了 GOV1。动手前读 `internal/archtest/deps_test.go` 的 `portAllowlists` 确认 `goalloop` 是不是 port 包。

- [ ] **Step 3: 跑绿；`go test ./internal/archtest ./internal/agent/goalloop`；commit**

---

## 交付顺序

Task 1 必须先做（9 条依赖它）。其余顺序：2 → 3 → 5 → 4 → 6 → 7 → 8 → 9 → 10 → 11 → 12。

Task 5（归档）排在 Task 4（记忆 worker）前面，因为 Task 6 的溯源验收明写「归档后仍可解析」，需要归档已经存在才能真测。

## 自检（写完计划后跑的）

- **spec 覆盖**：W-D-01…16 共 16 条，映射到 12 个任务 —— 01+05→T1、02→T2、10+11→T3、03→T4、04→T5、07+12→T6、08→T7、09→T8、06→T9、13+14→T10、15→T11、16→T12。无遗漏。
- **占位符扫描**：无 TBD/TODO；每个「实现」步都给了具体的表结构或算法，没有「适当处理错误」这类空话。
- **类型一致性**：Task 1 的 `ProjectWindow` / `HiddenSeq` / `AppendContextEvent` 在 T5（归档回退）、T6（溯源）、T9（检查点）被引用，签名一致。
- **落点复核（写完计划后补跑的一轮，结果已回填进各任务）**：
  - T8 **整条被推翻并重写** —— 输入历史早就落盘了，原计划让人把能工作的东西返工进 SQLite。真实缺口是字节上限与多进程覆盖。
  - T11 两处事实以复核为准：缓存键是 `runnerCacheKey`（per-model+per-mode）不是 model 指针，且 `FlushRunners` 驱逐机制已存在。
  - T12 「双预算」核实为真（`MaxIterations` + `MaxTokens`），且 `goalloop` 当前不 import `store` ⇒ 走窄接口注入。
  - T4 补入 `MinDistillBatch = 6` 的夹具陷阱：候选不足 6 条时蒸馏静默空转。
  - T7 补入「先看 `internal/store/task.go` 能不能复用」。
  - T9 补入既有快照机制清单：会话维度原语已齐，`internal/vcs` 有 7 个相关文件，本任务多半是编排而非新造。
- **剩余的精度上限**：T2/T3/T5/T6/T10 只做了签名级与现状引文核对，没跑探针。这些任务的第一步都写了「先读 X」，且【实测】/【设计】分级要求实施者以代码为准 —— 这是本计划已知的、有意接受的上限。上面那轮复核在 6 个任务里推翻了 1 个、修正了 5 个，说明这个上限是真实存在的，实施者请当真。
