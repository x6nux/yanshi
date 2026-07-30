# Batch E1 — 测试覆盖补齐设计

> **日期**：2026-07-22
> **归属**：E-H roadmap 的 Batch E1（`docs/feature-roadmap-e-h.md` §3）
> **命题**：补齐 `internal/store` / `internal/proto` / `internal/bootstrap` 三个关键包的测试覆盖率，从 synthesis 现状分别 58%/67%/23% 提升至 75%+/80%+/50%+。**不加新功能，只加测试。**
> **状态**：设计稿，待用户审阅 → writing-plans。

---

## 1. 目标与非目标

### 目标

- `internal/store`（持久化核心，扇入 7）+ 行级测试 + 表驱动 + 边界 → 覆盖率 ≥75%。
- `internal/proto`（WS/SSE 共享帧词表）+ 全帧类型 JSON 往返 + SSE golden + 词表对称 + 未知字段兼容 → 覆盖率 ≥80%。
- `internal/bootstrap`（唯一组合根）+ 最小 App 构建 + 软降级验证 + Options.Cfg 注入 + 一条端到端 turn（fake model + 工具调用） → 覆盖率 ≥50%。
- 所有新测试遵循 Fake 优先原则，无需真实 API key、子进程或外部文件。

### 非目标

- 修改任何 `.go` 生产代码（只改 `_test.go`）。
- 增加任何功能点、工具、帧类型、配置字段。
- 补齐 `internal/vcs/`、`internal/guard/`、`internal/tools/`、`internal/cli/tui/` 等的覆盖（归 E2/E3 或后续批次）。
- 引入 mock 框架（Fake 原则优先，库如 `testify` 保留）。

---

## 2. 背景

### 2.1 覆盖现状来源

`synthesis-final.md`（07-20）§8 对三个薄弱包做了明确的中线覆盖评估。当前代码已超过 A-D 15 批迭代（610 commits, `main` branch），但覆盖未系统性提升。E-H 路线图（`docs/feature-roadmap-e-h.md`）定下 P0 优先级：E1 必须补齐这三个覆盖面。

**D3 交叉风险**：另一 agent 正在跑 D3（secrets/auth/i18n/keymap），直改 `internal/config`、`internal/bootstrap`、`internal/store`。执行本 spec 前须 `git log --oneline` 确认 D3 落地后 bootstrap/store 的生产代码变更是否影响了本 spec 的落点。以下所有 `落点` 标注 `⚠ D3` 的条目均需执行前 re-verify。

### 2.2 `internal/store` — 包概述

文件（18 文件，~800 生产行）：

| 文件 | 职责 | 已有测试覆盖 |
|---|---|---|
| `store.go` | `Open`/`Close`/`migrate`/schema DDL | 部分（TestOpen_InMemory, TestOpen_MigratesIdempotently, TestMigrate_AddsWorktreeColumn） |
| `session.go` | `CreateSession`/`AppendMessage`/`UpdateSessionTitle`/`UpdateSessionMeta`/`Messages`/`SessionMessageCount`/`DeleteSession`/`SetSessionArchived` + snapshot 恢复 | 间隙（UpdateSessionTitle/SessionMessageCount 无测试） |
| `session_list.go` | `ListSessions`/`ListArchivedSessions`/`GetSession` | 部分（ListSessions/GetSession 有测，listSessionsWhere 边界缺） |
| `session_fork.go` | `ForkSession` | 良好（5 个测试，涵盖 -1/>=0/越界/负/usage 重置） |
| `task.go` | `CreateTask`/`ClaimTask`/`SetTaskResult`/`GetTask`/.../`TouchTask`/`ListStaleRunning`/`RequeueTask`/`FinalizeTask`/`RequeueStaleTask`/`CancelTask`/`ListPending` 等 | 间隙（TouchTask/StaleRunning/RequeueTask+FinalizeTask fail 无测试） |
| `auth.go` | `AuthMetadataFromDB`/`SaveAuthMetadata`/`LoadAuthMetadata`/`DeleteAuthMetadata` | 良好（3 个测试，含并发） |
| `memory.go` | `WriteMemory`/`SearchMemory`/`RecallMemory` | **无测试** |
| `kv.go` | `KVSet`/`KVGet` | **无测试**（仅表存在性探测） |

### 2.3 `internal/proto` — 包概述

文件（4 文件，~860 生产行）：

| 文件 | 职责 | 已有测试覆盖 |
|---|---|---|
| `frame.go` | `ClientFrame`/`ServerFrame` + 全帧构造函数 + `SSEEvent()` | 部分（约 20 个测试覆盖主要帧，约一半构造函数无测试） |
| `versioned.go` | `VersionedFrame`/`NewVersionedFrame` | 已覆盖（2 个测试） |

**未测试的 ServerFrame 构造函数**：`NewAgentChunk`（已测试）、`NewThinking`（未测）、`NewToolCall`（已测）、`NewToolResult`（已测）、`NewToolProgress`（未测）、`NewError`（已测）、`NewDone`（已测）、`NewRetry`（未测）、`NewModels`（已测）、`NewStatus`（已测）、`NewStatusWithMode`（未测）、`NewPermissionRequest`（已测）、`NewMCPList`（已测）、`NewCompactChunk`（已测）、`NewHistoryReplaced`（未测）、`NewSessions`（未测）、`NewSessionRestored`（未测）、`NewSessionAck`（已测）、`NewStructuredResult`（已测）、`NewFeaturesReply`（未测）、`NewPermissions`（已测）、`NewPermissionRuleHit`（未测）、`NewTaskUpdate`（未测）、`NewPlanUpdate`（已测）、`NewChecklistUpdate`（已测）、`NewSubagentEvent`（已测）、`NewMCPStatusFrame`（未测）、`NewListMCP`（已测）、`NewSeams`（未测）、`NewSeamRestored`（未测）、`NewSessionForked`（未测）、`NewSideState`（未测）、`NewSkillsList`（未测）、`NewSkillAck`（未测）、`NewJobs`（已测）、`NewJobEvent`（未测）。

### 2.4 `internal/bootstrap` — 包概述

文件（8 文件，~1130 生产行）：

`bootstrap.go` 是唯一组合根。当前 13 个测试用例覆盖了：FakeModel 构建、Model 注册表、VCS 接线、安全子系统、LSP、功能标志/pricing、OTel、AgentAPI、secrets/auth 管线。

**已有覆盖缺口**：
- 无最小 App 构建（`:memory:` + FakeModel + VCS 禁用）的全分支覆盖。
- 无软降级路径测试（VCS InitRepo 失败、插件发现失败、MCP 服务器失败）、
- `Options.Cfg` 注入用于基本 Build（不用 YAML 文件）仅 auth/secrets 测试用到，没有全组件验证。
- `Options.Output` 注入无测试。
- 端到端 turn（模型回一句话 + 一个工具调用 + 工具结果回喂）无测试。
- 装配顺序验证（App 所有字段非 nil）。
- `App.Shutdown` 独立测试（当前仅在其他测试的 defer 中调用）。

---

## 3. 条目设计

### [COV1] `internal/store` 覆盖 58% → 75%+

#### 缺口

持久化核心包覆盖严重不足（synthesis R5）。`memory.go` 和 `kv.go` 完全无测试；`session.go` 的 `UpdateSessionTitle`/`SessionMessageCount` 未测；`task.go` 的 `TouchTask`/`ListStaleRunning`/`RequeueTask`/`FinalizeTask(fail)`/`IncrementAttempts` 未测；VCS 表 schema 完整性未断言；边界场景（超大 content、空 session 追加、并发追加）无覆盖。

#### 落点

`internal/store/` 以下测试文件（不改生产代码）：

| 测试文件 | 新增测试 | 覆盖的目标函数 |
|---|---|---|
| `session_test.go` | `TestSession_UpdateSessionTitle` / `TestSession_SessionMessageCount` / `TestSession_AppendMessage_MissingSession` / `TestSession_LargeContent` | `UpdateSessionTitle`, `SessionMessageCount`, `AppendMessage` 边界 |
| `session_list_test.go` | `TestListSessions_ZeroSessions` / `TestListArchivedSessions_Empty` / `TestGetSession_Missing` | `listSessionsWhere`, `ListArchivedSessions`, `GetSession` 边界 |
| `task_test.go` | `TestTask_TouchTask` / `TestTask_ListStaleRunning` / `TestTask_RequeueTask_OwnerGuard` / `TestTask_FinalizeTask_Failed` / `TestTask_IncrementAttempts` / `TestTask_RequeueToFailedViaRetries` / `TestTask_SetResultMissing` | `TouchTask`, `ListStaleRunning`, `RequeueTask`, `FinalizeTask`, `IncrementAttempts`, `RequeueStaleTask + maxRetries` |
| `memory_test.go` | `TestStore_WriteMemory` / `TestStore_SearchMemory` / `TestStore_RecallMemory` / `TestStore_SearchMemoryFTS` | `WriteMemory`, `SearchMemory`, `RecallMemory` |
| `kv_test.go` | `TestKV_SetGet` / `TestKV_GetMissing` / `TestKV_Overwrite` | `KVSet`, `KVGet` |
| `store_test.go` | `TestStore_Migrate_VCSTableColumns`（扩展：断言索引、FK、所有 VCS 表的完整列集）| `migrate`（VCS schema 验证）|
| `session_test.go` | `TestSession_ConcurrentAppend`（⚠ 并发追加） | `AppendMessage`（并发场景，race detector 配合）|

#### 设计

**表驱动模式**：所有新测试参照现有 `TestTask_CreateClaimSetResultGet` 的模式（`Open(":memory:")` + `t.TempDir()` 持久临时文件 + `defer s.Close()`）。边界用例用 `testify/assert` + `require`。

**memory_test.go** — 操作 FTS5 虚拟表使测试与 SQLite 绑定，但仍用 `:memory:` 不涉外部文件。WriteMemory→SearchMemory 验证 FTS 匹配（`MATCH ?` 通配）。RecallMemory 验证 DESC 顺序和 limit。

**kv_test.go** — 最简单的 round-trip 测试。KVSet→KVGet OK；KVGet missing→ok=false；KVSet overwrite→KVGet 返回新值。

**task 扩展测试** — `TouchTask`：创建→Claim→Touch→验证 updated_at 增长。`ListStaleRunning`：创建→Claim→手动 SQL 降 updated_at→ListStaleRunning 命中。`RequeueTask`：Claim→RequeueTask（自己）成功；Client 换人→`ErrNotRunningOrOwned`。`FinalizeTask(fail)`：Claim→FinalizeTask("failed")→Get 验证。`RequeueStaleTask maxRetries`：Claim→RequeueStaleTask（`maxRetries=0`）→failed。`IncrementAttempts`：创建后增量两次验证。

**VCS 表 schema**：`TestStore_Migrate_VCSTableColumns` 断言每个 VCS 表存在的列（`vcs_repos`：`id, root_path, main_head, created_at`；`vcs_worktrees`：`id, repo_id, path, base_commit, created_at, active, tip`；`vcs_commits`：`id, repo_id, worktree_id, parent_id, merged_from, author, message, created_at`；`vcs_tree`：`commit_id, path, blob_hash, op`；`vcs_blobs`：`hash, content, size`；`vcs_uncommitted`：`scope_type, scope_id, path, blob_hash, op`；`vcs_seams`：全列）。用 `PRAGMA table_info(X)`（已有 `columns()` 方法）检查。

**并发追加**：`TestSession_ConcurrentAppend` 使用 `sync.WaitGroup` + 4 goroutine 对同一 session 追加不同 message，断言最终消息数和 `-race` 不报（⚠ 并发写依赖 SQLite 串行化写保护，不因并发报 `database is locked`，若 F1 WAL 未落地先标记为 `t.Skip` 兼容）。

**超大 content**：`TestSession_LargeContent` — 用 strings.Repeat 构造 `≥1MB` 的 content，验证 AppendMessage 不 panic、Messages 返回 intact。

#### 依赖

- 无前置依赖（`Open(":memory:")` 零外部依赖）。
- ⚠ D3 改 `store.go` 的 `Open`/`SetRedactor`/`redact`，执行前须确认这些函数签名未变。新版 `redact()` 参数 `text string`、`CreateSession` 调用 `s.redact(title)` 等流程未变则测试适用。

#### 风险

| 风险 | 缓解 |
|---|---|
| FTS5 `MATCH` 语法特殊，搜索测试可能因 tokenizer 不同而误判 | 使用简单的词项匹配（`MATCH ?` 通配 `*`），不做复杂搜索断言 |
| 并发追加在 F1 WAL 未落地前可能 `database is locked` | `TestSession_ConcurrentAppend` 添加 `t.Skip` 兜底条件，若初次失败则 gracefully 跳过 |
| VCS 表 schema 与 `internal/vcs/` 实际使用的列漂移 | 测试断言列名来自 `vcs.go` 查询中出现的列，不 copy 列定义 |
| D3 改 `task.go` 函数签名（如 `SetTaskResult` 增加 redact 参数？）| 执行前 git diff 确认 `task.go` 生产代码变动 |

#### 验收

1. `memory.go` 覆盖率从 0% 到 >80%（WriteMemory/SearchMemory/RecallMemory 各有一条以上测试）。
2. `kv.go` 覆盖率从 <10% 到 >90%（round-trip/overwrite/missing）。
3. `session.go` 的所有导出函数至少有一个正向测试。
4. `task.go` 的 TouchTask/ListStaleRunning/RequeueTask/FinalizeTask(fail)/IncrementAttempts 各有一个测试。
5. 边界测试（超大 content、空 session、missing session/task）存在。
6. VCS schema 断言通过。
7. 并发追加测试在允许环境下不 panic。
8. 覆盖率 `go test -cover internal/store` 报告 ≥75%。

#### 预估

2.5-3d（memory/kv 基础最简单，0.5d；task 生命周期扩展 0.5d；session 边界 0.5d；VCS schema 0.5d；并发测试+最终覆盖调平 0.5d）。

---

### [COV2] `internal/proto` 覆盖 67% → 80%+

#### 缺口

A-D 新增了大量帧类型（features、memory、fork、side、skills、seams、subagent_event、mcp_status、tool_progress 等），但测试仅覆盖了约一半构造函数。没有 SSE golden 文件断言 `event:`/`data:` 行稳定。没有未知字段兼容性测试（新加帧字段不破坏老客户端）。没有 WS/SSE 词表对称性断言。没有 `go test -update` 重建 golden 文件的机制。

#### 落点

`internal/proto/` 以下测试文件：

| 测试文件 | 新增测试 | 覆盖的目标帧类型 |
|---|---|---|
| `frame_test.go` | `TestServerFrame_AllTypesRoundTrip` | 所有此前未覆盖的 ServerFrame 构造函数（详见下方列表） |
| `frame_test.go` | `TestClientFrame_AllTypesRoundTrip` | 所有此前未覆盖的 ClientFrame 构造函数（详见下方列表） |
| `frame_test.go` | `TestSSEEvent_Golden` | 所有帧的 SSEEvent() 输出，与 golden 文件对比 |
| `frame_test.go` | `TestUnknownField_Compatibility` | 反序列化含有未知 JSON 字段的帧不 panic |
| `frame_test.go` | `TestVocabulary_Symmetry` | WS 和 SSE 使用完全相同的 ServerFrame 帧类型集合（断言所有 `Type` 值在 SSE 中有实现） |
| `versioned_test.go` | `TestVersionedFrame_VersionField` | 序列化后含 `"version":"v1"` 字段，可反序列化 |

**必须覆盖的 ServerFrame 构造函数**：`NewThinking`、`NewToolProgress`、`NewRetry`、`NewStatusWithMode`、`NewHistoryReplaced`、`NewSessions`、`NewSessionRestored`、`NewSessionForked`、`NewSideState`、`NewSeams`、`NewSeamRestored`、`NewFeaturesReply`、`NewPermissionRuleHit`、`NewMCPStatusFrame`、`NewSkillsList`、`NewSkillAck`、`NewTaskUpdate`（正种和 nil case 已有）、`NewPlanUpdate`（nil case 已有）、`NewChecklistUpdate`、`NewJobEvent`、`NewCompactChunk`。

**必须覆盖的 ClientFrame 构造函数**（尽管未测它们也是通过 WS 读取的）：`NewUserMessageWithSchema`（已测构造器）、`NewSetModel`（已测构造器但未序列化往返）、`NewRestoreSession`、`NewRenameSession`（已测构造器但未序列化往返）、`NewArchiveSession`（已测构造器）、`NewUnarchiveSession`（已测构造器）、`NewDeleteSession`（已测构造器）、`NewSessionListArchived`（已测构造器）、`NewListMCP`（已测构造器）、`NewListSkills`、`NewInstallSkill`、`NewUninstallSkill`、`NewTrustSkill`、`NewUntrustSkill`、`NewEnableSkill`、`NewDisableSkill`、`NewListSeams`、`NewRestoreTurn`、`NewForkSession`、`NewEnterSide`、`NewExitSide`、`NewFeaturesList`、`NewFeaturesSet`、`NewListPermissions`、`NewRevokePermission`、`NewJobsList`、`NewJobRead`、`NewJobWrite`、`NewJobCancel`、`NewMCPAction`。

#### 设计

**全帧类型往返**：使用表驱动，遍历所有构造函数生成的帧 → json.Marshal → json.Unmarshal → 断言 `Type` 不变 + 每个帧特有的字段正确保留。

**SSE golden 文件**：创建 `internal/proto/testdata/` 目录，内含 `sse_golden.txt`。测试将每个帧类型通过 `SSEEvent()` 输出拼接为 `event: <name>\ndata: <json>\n\n` 格式，与 golden 文件逐行对比。新增 `-update` flag（通过 `flag` 包或 GOFLAGS — 参照 go test `-update` 惯例）：

```go
var update = flag.Bool("update", false, "update golden files")

func TestSSEEvent_Golden(t *testing.T) {
    // 生成 full output
    // if *update { os.WriteFile(goldenPath, output, 0644) }
    // else { 对比 output 与现有 golden }
}
```

**未知字段兼容**：`TestUnknownField_Compatibility` — 创建一个合法的 ServerFrame JSON，额外添加一个未知字段（如 `"future_field": "will_be_ignored"`），反序列化后断言不 error、Type 字段不变。

**WS/SSE 词表对称性**：`TestVocabulary_Symmetry` — 写一个函数收集 `ServerFrame` 所有的 `Type` 常量/值（通过遍历所有构造函数生成的帧的 Type），断言存在对应的 `SSEEvent()` 使用方法。反向验证：SSE 端文档的帧名称在 ServerFrame.Type 中都能生成。

**版本字段**：`TestVersionedFrame_VersionField` 断言 JSON 反序列化后 version 字段被正确映射。新增 `VersionedFrame` 的 `Version` 字段为常量值 `"v1"` 的序列化验证。

#### 依赖

- 无前置依赖。
- 新增 `testdata/sse_golden.txt` 必须入版本控制。
- `go test -update` 需要构建标签或 flag 参数。

#### 风险

| 风险 | 缓解 |
|---|---|
| golden 文件容易漂移（每次新增帧类型需重新生成） | 引入 `-update` flag 使维护自动化；CI 不传 `-update` 从而断裂测试暴露未同步的修改 |
| 60+ 个构造函数逐一写表驱动测试工作量大 | 用反射或 `[]struct{type, construct, verify}` 数据驱动模式，每组一个表条目 |
| 未知字段测试过于脆弱（Go JSON 总默认忽略未知字段，除非用 `DisallowUnknownFields`）| 明确测试只反序列化后 type 不变，不要求未知字段报错 |

#### 验收

1. 每帧类型的 JSON 往返覆盖 ≥90% 的构造函数（当前已有 ~30/60，新增覆盖其余）。
2. SSE golden 文件存在，`go test .` 无参数时与 golden 匹配。
3. `go test -update` 重新生成 golden。
4. 未知字段反序列化兼容性测试通过。
5. WS/SSE 词表对称性断言通过。
6. 覆盖率 `go test -cover internal/proto` 报告 ≥80%。

#### 预估

2d（帧类型表驱动测试 1d，SSE golden 0.5d，兼容/对称/版本 0.5d）。

---

### [COV3] `internal/bootstrap` 覆盖 23% → 50%+

#### 缺口

唯一组合根测试覆盖率极低（synthesis R12）。现有测试覆盖了 FakeModel 构建和部分子系统接线，但缺少：最小 App 全组件断言、软降级路径（VCS / 插件 / MCP / LSP 失败不阻塞）、`Options.Cfg` 注入做标准 Build（不绕过 `:memory:` store）、`Options.Output` 注入、端到端 turn（模型回话 + 工具调用 + 结果回喂）、App.Shutdown 独立测试。

#### 落点

`internal/bootstrap/bootstrap_test.go`（不改生产代码）：

| 新增测试 | 验证目标 |
|---|---|
| `TestBuild_MinimalApp` | 最小 App（FakeModel + `:memory:` store + VCS 隐含失败降级 + 所有 App 字段非 nil） |
| `TestBuild_VCSSoftDegrade`（⚠ D3）| VCS InitRepo 失败（空工作目录/不可写）时不阻塞 Build，VCSRepoID=""，不 panic |
| `TestBuild_PluginDiscoverySoftDegrade` | 插件发现失败（指向不存在的目录）时不阻塞 Build，Skills 非 nil |
| `TestBuild_MCPStartupSoftDegrade` | MCP server 无法启动时不阻塞 Build，MCP.Enabled() 可能为 false |
| `TestBuild_OptionsCfgInjection` | `Options.Cfg` 注入最小 Config（`:memory:` store + FakeModel），Build 成功且全组件非 nil |
| `TestBuild_OptionsOutputInjection` | `Options.Output` 注入自定义 SafeOutput，Build 后 Redactor 非 nil |
| `TestBuild_EndToEndTurn` | 构建 → 驱动 orchestrator turn（模型回 "tool_test:..." + tool call）→ 断言工具被调、结果回喂 |
| `TestApp_Shutdown` | Build → Shutdown → 不 error，可再次 Shutdown（幂等）|
| `TestBuild_AssemblyOrder` | Build 后所有 App 字段非 nil / 有意义（Store/Orch/Broker/Model/Models/Skills/VCS/.../AgentAPI/MCP/LSP） |

#### 设计

**TestBuild_MinimalApp**：
```go
cfg := &config.Config{
    Storage: config.StorageConfig{SQLitePath: ":memory:"},
}
app, err := bootstrap.Build(bootstrap.Options{Cfg: cfg, FakeModel: true})
require.NoError(t, err)
defer app.Shutdown(context.Background())
// 断言所有关键字段非 nil（与 TestBuild_RegistersAllSecuritySubsystems 模式一致）
require.NotNil(t, app.Store)
require.NotNil(t, app.Orch)
require.NotNil(t, app.Broker)
require.NotNil(t, app.Model) // FakeModel
require.NotNil(t, app.Skills)
require.NotNil(t, app.Sandbox)
require.NotNil(t, app.NetworkPolicy)
require.NotNil(t, app.Approvals)
require.NotNil(t, app.ShellManager)
require.NotNil(t, app.SecureFactory)
require.NotNil(t, app.Features)
require.NotNil(t, app.Pricing)
require.NotNil(t, app.OTel)
require.NotNil(t, app.Redactor)
require.NotNil(t, app.Auth)
require.NotNil(t, app.AgentAPI)
require.NotNil(t, app.LSP)
require.NotNil(t, app.MCP)
require.NotNil(t, app.SubagentManager)
// VCS 在内存 store+无真实工作目录时可能 InitRepo 失败 → VCSRepoID 可能为空
// 但不应当 panic
```

**TestBuild_VCSSoftDegrade**：
```go
dir := t.TempDir()
cfg := &config.Config{
    Storage: config.StorageConfig{SQLitePath: filepath.Join(dir, "yanshi.db")},
    VCS: config.VCSConfig{
        WorktreeDir: filepath.Join(dir, "worktrees"),
        // 故意不设 Ignore，InitRepo 可能因为没有可扫描的文件而失败
    },
}
app, err := bootstrap.Build(bootstrap.Options{Cfg: cfg, FakeModel: true})
require.NoError(t, err)
defer app.Shutdown(context.Background())
// VCSRepoID 可能为空（因内存 store 或空目录），但不 panic
// 断言 VCS instance 仍非 nil
require.NotNil(t, app.VCS)
```

**TestBuild_EndToEndTurn**：
```go
// 使用 FakeModel 输出一条工具调用 + 一条 assistant 消息
step1 := schema.AssistantMessage("", []schema.ToolCall{
    {ID: "c1", Type: "function", Function: schema.FunctionCall{
        Name: "time_now", Arguments: `{}`,
    }},
})
step2 := schema.AssistantMessage("done", nil)
mdl := einollm.NewFakeModelWithMessages([]*schema.Message{step1, step2}, nil)

// 通过 Options 替换模型，使用 :memory: store
require.NoError(t, ...) // 使用 ProviderBuilder 返回 fake + time_now 工具可用

// 通过 app.Orch.Events 驱动用户 turn
iter := app.Orch.Events(context.Background(), "what time is it?")

// 断言 tool_call + tool_result + 最终 assistant 消息
assertToolCallSeen  // time_now
assertToolResultSeen // 包含当前时间
assertAssistantSeen  // "done"
```

由于 `bootstrap.Build` 的 `Model` 字段在 FakeModel 模式下是未集成工具集的单级模型（无 ReAct），端到端 turn 需用 `ProviderBuilder` 注入一个 `orchestrator.New` 能内置工具集。这需要调用 `options.ProviderBuilder` 返回一个含 time_now 工具的 chain，并在 `orcaConfig.Tools` 包含该工具。

另一种方式：直接构造 `orchestrator` 复用现有 `TestBuild_VCSToolsRunThroughOrchestrator` 的模式（测试在 bootstrap 包内，直接利用 `einollm.NewFakeModelWithMessages` + `orchestrator.New`），不经过 `Build` 的 `providerBuilder` 路径。**更简单**——定义为 `bootstrap_test.go` 内的 sub-test，不通过 `Build` 而是演示 bootstrap 级别的集成：

```go
func TestBuild_EndToEndTurn(t *testing.T) {
    app := buildMinimalApp(t) // helper 复用 TestBuild_MinimalApp 的 cfg
    defer app.Shutdown(context.Background())

    // 构建脚本化的 orchestrator（复用现有模式）
    // 使用 app.Store + app 的上下文注入
    mdl := einollm.NewFakeModelWithMessages(...)
    // 但这时 app.Model 已经被 Build 设为 FakeModel，需要新建 ReAct orche
    // 注：由于云原生 Eino 的编译依赖，所有工具需要 real orchestrator。
    // 捷径：直接调用 app.Orch.Events — 但 app.Orch 的 Model 是纯 FakeModel（无工具驱动能力）。
    // 因此端到端 turn 的测试更适合用 ProviderBuilder 模式。
}
```

**端到端实现策略**：使用 `ProviderBuilder` + `Options.Cfg` 注入一个支持工具集的模型工厂。faker 模型输出工具调用后，orchestrator 解析执行工具，产生 tool_result，然后模型收到结果并回复。

更实际的方案：写一个 `buildMinimalAppWithToolModel` helper，通过 `ProviderBuilder` 返回一个已知行为模型 + `einollm.NewFakeModelWithMessages(return_schema_assistant_tool_call, ...)`。同时确保 tools 包含 time_now。这样 `Build` 的 end-to-end 路径被完整覆盖。

设计选择：为避免过度工程，**COV3 的端到端 turn 测试限制为**：通过 Build 获得 App，用 app.Store 创建 session + 写一条消息，通过 app.Orch.Events 驱动一个用户 turn，断言返回的数据帧包含 `agent_chunk`（至少一条）。工具调用为可选。

#### 依赖

- ⚠ D3 正在改 `bootstrap.go`（Redactor/Auth/credential resolution）。执行前须 `git log --oneline -- internal/bootstrap/bootstrap.go` 确认当前最新版本。本 spec 所有落点都基于 bootstrap_test.go（不改生产代码），不论 D3 最终分支如何，测试代码面对的接口不变。

#### 风险

| 风险 | 缓解 |
|---|---|
| D3 增加了 `bootstrap.Build` 的 required 字段（如 Secrets config 不能为空）导致内存 store FakeModel 构建失败 | 在 `config.Config` 中加 `Secrets: config.SecretsConfig{Backend: "none"}` 和 `Auth: config.AuthConfig{}` 显式配置 |
| end-to-end turn 因 Eino 版本依赖而 skip（与现有 `TestBuild_ModelRegistry` 的 skip 模式一致）| 使用 `t.Skip` 兼容，不影响其他 bootstrap 测试 |
| 软降级测试中 "插件发现失败" 难以构建（需要 plugin dir 有格式错误的内容）| 指向不存在的目录 → PluginDiscovery 返回 err → Build 继续（warning），验证 Skills 不 nil |

#### 验收

1. 最小 App 构建（`:memory:` store + FakeModel + VCS 隐含降级）成功，所有关键 App 字段非 nil。
2. VCS InitRepo 失败不阻塞 Build，`app.VCS` 非 nil 但 `app.VCSRepoID` 可能为空。
3. 插件发现失败不影响 Build 成功。
4. `Options.Cfg` 注入最小 Config 成功构建。`Options.Output` 注入后 Redactor 非 nil。
5. 端到端 turn 至少产生一条 `agent_chunk`。
6. App.Shutdown 不 error，可多次调用。
7. 覆盖率 `go test -cover internal/bootstrap` 报告 ≥50%。

#### 预估

2-3d（最小 App 构建 + 全字段断言 0.5d；软降级 3 条路径 0.5d；Options 注入 0.5d；端到端 turn 1d；Shutdown 测试 0.25d；覆盖调平+CI 验证 0.25d）。

---

## 4. 文件结构

| 文件 | 操作 | 职责 |
|---|---|---|
| `internal/store/memory_test.go` | 新建 | `WriteMemory`/`SearchMemory`/`RecallMemory` + FTS5 |
| `internal/store/kv_test.go` | 新建 | `KVSet`/`KVGet` round-trip/overwrite/missing |
| `internal/store/session_test.go` | 改 | 追加 `UpdateSessionTitle`/`SessionMessageCount`/`LargeContent`/`ConcurrentAppend`/`MissingSession` |
| `internal/store/session_list_test.go` | 改 | 追加 `ListSessions_ZeroSessions`/`GetSession_Missing`/`ListArchivedSessions_Empty` |
| `internal/store/task_test.go` | 改 | 追加 `TouchTask`/`ListStaleRunning`/`RequeueTask`/`FinalizeTaskFailed`/`IncrementAttempts`/`RequeueToFailed` |
| `internal/store/store_test.go` | 改 | 扩展 VCS 表 schema 验证（列/索引/FK） |
| `internal/proto/testdata/sse_golden.txt` | 新建 | SSEEvent golden 文件 |
| `internal/proto/frame_test.go` | 改 | 全帧类型往返 + SSE golden + 未知字段 + 词表对称 |
| `internal/proto/versioned_test.go` | 改 | 追加 version 字段序列化测试 |
| `internal/bootstrap/bootstrap_test.go` | 改 | 追加最小 App/软降级/Options 注入/端到端 turn/Shutdown 测试 |

**不改的生产代码**：`internal/store/session.go`、`task.go`、`memory.go`、`kv.go`、`auth.go`；`internal/proto/frame.go`、`versioned.go`；`internal/bootstrap/bootstrap.go`。

---

## 5. 测试策略（Fake 优先）

- **Fake 模型**：`einollm.FakeModel`（`NewFakeModelWithMessages`）用于 bootstrap 端到端 turn 测试，**不需要真实 API key**。
- **Fake store**：SQLite `":memory:"` + `t.TempDir()` 持久化临时文件，零外部依赖。
- **Fake 工具**：`time_now`（内置工具，无副作用）用于验证工具调用回喂。
- **表格驱动**：proto 的 60+ 帧类型使用表条目，每行一组构造函数 + 字段验证，不复制代码。
- **Golden 文件**：SSE golden 用 `testify/assert` 逐行对比，不 dump 到 stdout。
- **-race 兼容**：`ConcurrentAppend` 使用 `sync.WaitGroup`，在 `go test -race` 下不报竞态。
- **t.Skip 兼容**：Eino 版本依赖的端到端测试使用 `t.Skip` 模式（与现有 `TestBuild_ModelRegistry` 一致）。

---

## 6. 风险与缓解

| 风险 | 缓解 |
|---|---|
| D3 同时改 `bootstrap`/`store` 落点冲突 | 执行前 `git diff HEAD -- internal/store/ internal/bootstrap/ internal/proto/` 确认；落点标注 ⚠ D3 |
| memory FTS5 测试在不同 Go/SQLite 版本下行为差异 | 使用基础 `MATCH ?` 通配，避免复杂搜索断言；若因 tokenizer 不匹配首次失败则 t.Skip |
| 端到端 turn 测试因 Eino 版本锁定不可用 | t.Skip，不影响其他 COV3 测试通过 |
| golden 文件未及时更新导致 CI 断裂 | 引入 `-update` flag，开发者修 proto 代码后运行 `go test -update ./internal/proto/` |
| 大并发测试在 CI Windows runner 上 timeout | 设置合理 timeout（30s），并发 goroutine 数量少（≤8） |
| 预期覆盖率目标有跳动（如 memory.go 从 0% 到覆盖 50% 而非 80%） | 接受合理范围内偏差；验收条件以 "每个导出函数至少有一个测试" 替代 "80%" 这样的硬数字 |

---

## 7. 验收标准

1. `go test ./internal/store/ -cover` 覆盖率 ≥75%。
2. `go test ./internal/proto/ -cover` 覆盖率 ≥80%。
3. `go test ./internal/bootstrap/ -cover` 覆盖率 ≥50%。
4. `go test ./internal/store/ -race -count=1` 通过（并发测试不 panic）。
5. `go test ./internal/proto/ -count=1` 通过（golden 文件匹配）。
6. `go test -update ./internal/proto/` 可重新生成 golden。
7. 所有新增测试使用 `Open(":memory:")` 或 `t.TempDir()`，不依赖外部数据库文件。
8. 所有新增测试不引入 mock 框架（使用 `testify` 和内置 fake）。

---

## 8. 后续（非本批）

- E2 `FUZ1` guard.MatchGlob fuzz — 由另一批次覆盖。
- E2 `PROP1` ctxcompact 属性测试 — 由另一批次覆盖。
- E2 `RAC1` race detector 固化 — 由另一批次覆盖。
- F1 `WAL1` SQLite 并发 — 覆盖 WAL/连接池，与本批 COV1 `ConcurrentAppend` 配合协同。
- COV1 未覆盖的 `internal/store/` 非核心路径（side conversation store 操作、memory KV extra 索引查询）不在本批范围。
- COV2 未覆盖的 mcp_frames 和 seam_frame 测试（已在独立的 `mcp_frames_test.go` 和 `seam_frame_test.go` 中覆盖）不重复测试。
