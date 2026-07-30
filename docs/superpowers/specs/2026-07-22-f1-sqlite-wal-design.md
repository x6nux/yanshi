# Tier F1 — SQLite 并发（WAL + 连接池 + busy_timeout）设计

> **日期**：2026-07-22
> **归属**：E-H roadmap 的 Batch F1（`docs/feature-roadmap-e-h.md`）
> **命题**：把 yanshi 的 SQLite 存储从"单连接串行"升级为"WAL + 读连接池 + 写串行化 + busy_timeout"，消除 `database is locked`，让并发读不阻塞写、并发写不报错，并兜底跨进程访问。
> **范围**：只做存储层的并发与可靠性加固，不改 schema 语义、不改各业务表结构、不换驱动（仍是 `modernc.org/sqlite`）。
> **状态**：设计稿，待用户审阅 → writing-plans。

---

## 1. 目标与非目标

### 目标（WAL1 权威范围）

- **WAL 模式**：启动首连 `PRAGMA journal_mode=WAL` + `synchronous=NORMAL` + `busy_timeout=<ms>`，对旧库（rollback journal）自动升级。
- **连接池**：`database/sql` 的 `SetMaxOpenConns` 从 `1` 放开为读连接池；保证读多写不互斥。
- **写串行化**：保证进程内并发写（多 WS session、work Manager、artifact janitor、VCS commit）**不报** `database is locked`。
- **并发写测试**：多 goroutine 并发 `AppendMessage` / `UpdateSessionMeta` / `KVSet` 全部成功，无 `SQLITE_BUSY`。
- **WAL checkpoint 策略**：`wal_autocheckpoint` + 关停时主动 `wal_checkpoint(TRUNCATE)`，防 `-wal`/`-shm` 文件膨胀。
- **跨平台**：Windows（主开发平台）下 WAL 行为有测试与回归守护。
- **旧 DB 升级**：首次连接 rollback-journal 库自动转 WAL，不丢数据、幂等。
- **D3 落点 re-verify**：D3（secrets/auth/i18n/keymap）正在改 `internal/store`（redactor/auth_metadata），落地前须复核落点未被改写。

### 非目标（v1 不做）

- 换驱动（`modernc.org/sqlite` → `mattn/go-sqlite3` 或 `CGO`）。F1 保持 `CGO_ENABLED=0` 兼容。
- 把单文件 SQLite 换成 Postgres / 远程 DB。
- 拆分 `sessions.db` / `vcs.db` / `work.db` 多库（仍单库共享 `*sql.DB`）。
- 跨机/网络多进程并发写（仍单机；跨进程仅限本机 CLI 子命令）。
- 改 schema 迁移机制（仍 `addColumnIfMissing` + `CREATE TABLE IF NOT EXISTS`）。

---

## 2. 背景（代码实测，2026-07-22）

### 2.1 当前连接模型

`internal/store/store.go:41-54` 的 `Open`：

```go
func Open(path string) (*Store, error) {
    db, err := sql.Open("sqlite", path)
    ...
    db.SetMaxOpenConns(1)   // sqlite serializes writes; a single writer connection avoids lock errors.
    s := &Store{DB: db}
    if err := s.migrate(); err != nil { ... }
    return s, nil
}
```

- 驱动 `modernc.org/sqlite v1.53.0`（`go.mod:31`），纯 Go、`CGO_ENABLED=0`。**modernc 是 SQLite C 的忠实转译，WAL 模式完整支持**（含 `-wal`/`-shm` 与 checkpoint）。
- **没有任何 PRAGMA**：未设 `journal_mode`（默认 `rollback` / `delete`）、未设 `synchronous`（默认 `FULL`，每事务 fsync）、未设 `busy_timeout`（默认 `0`，立即 `SQLITE_BUSY`）。
- `SetMaxOpenConns(1)`：整个进程所有 DB 访问共享**一个物理连接**，Go 侧天然串行，所以进程内不会 `SQLITE_BUSY`——但代价是**所有读都被写阻塞**，并发读之间也串行。

### 2.2 一个 `*sql.DB` 被 4 个消费者共享

`bootstrap.go:276` 调 `store.Open(cfg.Storage.SQLitePath)` 得到 `st`，其 `st.DB` 被四处复用：

| 消费者 | 接入点 | 表 |
|---|---|---|
| `store.Store` 自身 | `bootstrap.go:686` `App.Store` | sessions/messages/kv/memories(fts)/tasks |
| auth 元数据适配器 | `bootstrap.go:324` `store.AuthMetadataFromDB(st.DB)` → `authSQLiteAdapter`（`internal/store/auth.go`） | auth_metadata |
| durable work store | `bootstrap.go:596` `work.FromDB(st.DB)` | task_work/checklists/gates/artifacts |
| VCS | `bootstrap.go:857` `VCSDBPath=cfg.Storage.SQLitePath` → `vcs.New(st,...)` 用 `v.store.DB` | vcs_*  |

**这意味着 F1 改 `Open` 里的 PRAGMA 与 `SetMaxOpenConns`，会同时影响这 4 个消费者**——这是设计落点必须把改动集中在 `store.Open` 的根本原因。

### 2.3 关键不变量：`queryScoped` / `BeginTx` 绕开单连接死锁

`internal/vcs/vcs.go:381-398` 有专门注释：

```go
// queryRowScoped runs QueryRow via tx when non-nil, else via the DB pool. This
// avoids the single-connection deadlock (SetMaxOpenConns(1)) that would occur if
// a writeCommitInTx path read through v.store.DB while the caller's tx holds the
// only connection.
```

`internal/task/work/store.go:4-6` 同理："所有写路径都通过单 BeginTx 包住，以配合 SetMaxOpenConns(1) 的串行写连接，避免在嵌套 Query 时死锁。"

**这是 F1 必须显式处理的核心约束**：当前代码靠"事务内读也走 `tx`（而非池）"来避开 `MaxOpenConns(1)` 下"事务占住唯一连接，再向池要连接读 → 死锁"的陷阱。改连接池后：

- 死锁本身消失（池有多连接，事务占一个、读拿另一个）。
- 但**事务一致性语义必须保留**：`queryScoped(tx,...)` 让读看到事务未提交的写；改成池后这条路径不变（仍传 `tx`），语义保持。`queryScoped(nil,...)`（非事务读）本来就读最近已提交快照，不变。
- **结论**：`queryScoped`/`queryRowScoped` 的现有签名与调用点无需改动，F1 不碰它们；但 spec 须在风险章标注"放开池后，任何'事务内通过池读'的新代码会读到不同快照——禁止在事务里用 `s.DB.Query` 而非 `tx.Query`"，并以 vet/约定守护。

### 2.4 跨进程访问：auth CLI 子命令（WAL 的直接受益者）

`cmd/yanshi/main.go:290`：

```go
authDB, err := store.Open(cfg.Storage.SQLitePath)   // 同一个 yanshi.db
```

这是**另一个 OS 进程**（`yanshi login` 等子命令）在 backend 服务可能正运行时打开同一个 `yanshi.db`。当前 rollback + `busy_timeout=0` + `MaxOpenConns(1)`：服务端持有锁时，CLI 读/写 `auth_metadata` 会直接 `database is locked`。**这正是 WAL + busy_timeout 要解决的头号真实场景**（D3 auth 落地后 `login`/`status`/`logout` 会更频繁触达此表）。

### 2.5 已有的写串行化雏形

`internal/store/auth.go:23-26` 的 `authSQLiteAdapter` 已经自带 `txMu sync.Mutex`（"belt-and-suspenders"）。F1 把这个意图提升为**全 DB 写串行化**统一方案。

### 2.6 D3 正在改 `internal/store`

D3 已落地 `Store.redactor` / `SetRedactor`（`store.go:13-37`）、`auth_metadata` 表与 `auth.go` 适配器。**F1 执行前必须 re-verify** 这些落点（见 §10）。

---

## 3. 架构：WAL + 读池 + 写串行化

### 3.1 总览

```
                       store.Open(path)
                            │
   ┌────────────────────────┼─────────────────────────┐
   │ 1. DSN 带 _pragma（每连接生效）                    │
   │    busy_timeout=<ms>  synchronous=NORMAL          │
   │    foreign_keys=ON  (补强, 见 §5.4)               │
   │ 2. SetMaxOpenConns(N)  ← 读连接池（N≈4..runtime）  │
   │ 3. Exec PRAGMA journal_mode=WAL  ← 持久化进 DB 头  │
   │ 4. wal_autocheckpoint=<pages>                     │
   │ 5. 写串行化原语 writeMu（进程内零 SQLITE_BUSY）     │
   └────────────────────────┬──────────────────────────┘
                            ▼
            单一共享 *sql.DB  （4 消费者: store/auth/work/vcs）
        ├─ 读: 直接 db.Query / db.QueryRow （并行, 不阻塞写）
        └─ 写: 经 writeMu 守护的 BeginTx→Exec→Commit （串行）
                            │
        WAL: 多读 + 单写并发; busy_timeout 兜底跨进程(auth CLI)
```

### 3.2 为什么是"读池 + 写串行化"而非"纯池 + busy_timeout"

WAL 仍是**单写**：两个 goroutine 同时 `BeginTx` 写，第二个拿不到写锁。靠 `busy_timeout` 重试虽能消化偶发冲突，但：

- work artifact janitor（`bootstrap.go:823`，6h 周期）+ 正在跑的 turn 写 + VCS commit 可能短时叠加；长事务下 `busy_timeout` 仍有超时风险。
- yanshi 的写本就是低并发、短事务——一个进程内写互斥锁能把"进程内 `SQLITE_BUSY`"降到**零**，`busy_timeout` 专职兜底**跨进程**（auth CLI）。

故主设计 = **池（读并行）+ `writeMu`（写串行）+ `busy_timeout`（跨进程兜底）**。`writeMu` 的落地位置见 §5.3 的 open question。

### 3.3 各消费者如何走写串行化

| 消费者 | 现状写路径 | F1 后 |
|---|---|---|
| `store.Store` | `s.DB.Exec` / `s.DB.Begin`（session.go 等） | 走 `writeMu` 守护的写辅助（§5.3） |
| `authSQLiteAdapter` | 自带 `txMu`（auth.go） | 复用统一 `writeMu`，或保留自锁（见 open q） |
| `work.Store` | `BeginTx(ctx,...)` 包所有写（store.go:4） | 经同一 `writeMu` |
| VCS | `v.store.DB.Begin` + `queryScoped` | 写经 `writeMu`；`queryScoped(tx)` 读语义不变 |

读路径（`Messages`、`SearchMemory`、`SessionList`、`RecallMemory`、`commitDelta` 等）**不持 `writeMu`**，直接走池，与写并发。

---

## 4. 配置

### 4.1 schema（改 `internal/config/config.go`）

`StorageConfig`（`config.go:298-300`）增三字段：

```go
type StorageConfig struct {
    SQLitePath string `yaml:"sqlite_path"`
    // WALMaxOpenConns 放开读连接池上限（F1）。0/省略 = 默认 4；1 = 旧行为（单连接，
    // 仅用于排障）。写仍由进程内 writeMu 串行，故此值只影响读并行度。
    WALMaxOpenConns int `yaml:"wal_max_open_conns"`
    // BusyTimeoutMs 是 SQLite busy_timeout（F1），跨进程锁冲突时的重试窗口。
    // 0/省略 = 默认 5000ms。仅本机 CLI 子命令与 backend 并存时可能触达。
    BusyTimeoutMs int `yaml:"busy_timeout_ms"`
    // WALAutoCheckpoint 是 wal_autocheckpoint 的页数阈值（F1）。
    // 0/省略 = 默认 1000（SQLite 默认）；负数 = 禁用被动 checkpoint（不推荐）。
    WALAutoCheckpoint int `yaml:"wal_auto_checkpoint"`
}
```

`applyDefaults`（`config.go:408`）补默认：`WALMaxOpenConns=4`、`BusyTimeoutMs=5000`、`WALAutoCheckpoint=1000`。不新增校验逻辑（零值合法）。`config_test.go` 增字段解析与默认用例。

### 4.2 示例（`config.example.yaml`）

```yaml
storage:
  sqlite_path: "yanshi.db"
  wal_max_open_conns: 4     # 读连接池上限；写仍串行。1 = 单连接旧行为（排障）
  busy_timeout_ms: 5000     # 跨进程锁冲突重试窗口（auth CLI 与 backend 并存时）
  wal_auto_checkpoint: 1000 # -wal 被动 checkpoint 页阈值；负数禁用（不推荐）
```

`config.yaml`（gitignored）由用户自行同步；不改 `config.example.yaml` 之外的被跟踪配置。

---

## 5. 落点与设计

### 5.1 PRAGMA 设置点（`store.Open`，`store.go:41`）

新增 `applyConnectionPragmas(db)`，在 `migrate()` **之前**执行（迁移也是写，需要 `busy_timeout` 生效）：

```go
func Open(path string) (*Store, error) {
    db, err := sql.Open("sqlite", buildDSN(path, pragmas))  // DSN 带 _pragma
    ...
    db.SetMaxOpenConns(maxOpen)            // 读池（§5.2）
    db.SetConnMaxIdleTime(...)             // 可选：回收空闲读连接
    s := &Store{DB: db, writeMu: &sync.Mutex{}}
    if err := s.applyConnectionPragmas(); err != nil { ... }  // journal_mode=WAL 等
    if err := s.migrate(); err != nil { ... }
    return s, nil
}
```

PRAGMA 分两类（**这是 modernc 连接池下最容易踩的坑，必须区分**）：

| PRAGMA | 作用域 | 设置方式 | 说明 |
|---|---|---|---|
| `journal_mode=WAL` | **持久**（写进 DB 文件头，一次即可） | `db.Exec` 一次 | 升级旧库；再次设置是幂等查询 |
| `synchronous=NORMAL` | **per-connection** | DSN `_pragma` | WAL 下 NORMAL 足够安全且快；FULL 每事务 fsync 太慢 |
| `busy_timeout=<ms>` | **per-connection** | DSN `_pragma` | 必须每连接都设，否则池里新连接 timeout=0 |
| `foreign_keys=ON` | **per-connection** | DSN `_pragma` | 补强：`messages.session_id` 等 FK 当前未强制（`DeleteSession` 注释 store.go:111 提到"FK 默认未启用"） |
| `wal_autocheckpoint=<pages>` | **per-connection**（但全局生效） | DSN `_pragma` 或 `db.Exec` | 控制被动 checkpoint 频率 |

> **modernc DSN `_pragma` 语法须按锁定版本核对**：modernc.org/sqlite 支持 `?_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)` 形式（每新建池连接都应用）。**实现时以 v1.53.0 文档/源码为准核对确切语法**（括号 vs `=`、多 pragma 分隔符）。若 DSN 路线受阻，退路是 `sql.OpenDB` + 自定义 `driver.Connector`，在 `Connect` 里对每个新连接 `Exec` 这些 pragma（与 mattn 的 `sql.Register("sqlite3", &sqlite3.SQLiteDriver{ConnectHook})` 等价）。两条路线都在 spec 风险章标注。

### 5.2 连接池配置（`store.Open`）

```go
maxOpen := cfg.WALMaxOpenConns
if maxOpen <= 0 { maxOpen = 4 }
if maxOpen == 1 {
    // 旧行为：单连接。保留用于排障/回归对照。
} else {
    db.SetMaxOpenConns(maxOpen)
    db.SetConnMaxIdleTime(5 * time.Minute)
    // 不设 SetConnMaxLifetime（modernc 连接重建成本低；避免长连接被回收打断）
}
```

- **`:memory:` 例外**：内存库的多连接看到**不同**数据库（每连接独立内存页）。`Open(":memory:")` 用于测试时**必须强制 `SetMaxOpenConns(1)`**，否则迁移在一个连接建表、测试在另一个连接查不到。这是 F1 必加的守护（见 §6 测试）。
- 读并行收益场景：WS `/sessions` 列表读（`session_list.go`）与另一 session 的 `AppendMessage`（`ws.go:571`）并发；work task 列表读与 artifact janitor 写并发（`bootstrap.go:823`）。

### 5.3 写串行化原语（核心 open question）

进程内写互斥，保证零 `SQLITE_BUSY`。两条候选落地路线（**需人决策**，见 §16）：

**路线 A（推荐）：`store` 暴露写辅助，各消费者统一走它**

在 `store` 包加（依赖方向不破：`store` 不反向依赖 work/vcs）：

```go
// WriteTx 在进程内写锁 writeMu 下执行 fn，保证 WAL 单写不产生 SQLITE_BUSY。
// 跨进程冲突（auth CLI）由 busy_timeout 兜底。tx 由本函数 Begin/Commit。
func (s *Store) WriteTx(ctx context.Context, fn func(*sql.Tx) error) error {
    s.writeMu.Lock()
    defer s.writeMu.Unlock()
    tx, err := s.DB.BeginTx(ctx, nil)
    if err != nil { return err }
    if err := fn(tx); err != nil { _ = tx.Rollback(); return err }
    return tx.Commit()
}
```

- `store.Store` 的写方法（`AppendMessage`/`CreateSession`/`UpdateSessionMeta`/`KVSet`/`WriteMemory`/`DeleteSession`/`ForkSession`/revert 系列）改走 `WriteTx` 或在其现有 `Begin` 外套 `writeMu`。
- `authSQLiteAdapter`（`auth.go`）把自带 `txMu` 换成注入的 `writeMu`（`AuthMetadataFromDB` 增 `writeMu` 参数，或改签名从 `*store.Store` 构造）。
- `work.Store`（`work.FromDB`）同样接收 `writeMu`，把"BeginTx 包所有写"的外层套上 `writeMu.Lock`。
- VCS 的 `writeCommitInTx` 路径在 `Begin` 外套 `writeMu`；`queryScoped(tx)` 不变。

**路线 B（最小侵入）：保留各消费者自锁，仅靠 DSN `busy_timeout` 兜底进程内偶发冲突**

- 只改 `Open` 的 PRAGMA + 池；`authSQLiteAdapter.txMu` 保留；work/vcs/store 各自加（或维持）写锁。
- 代价：写锁分散，易漏（新增写方法忘记加锁就可能在池下撞 `SQLITE_BUSY`），与 CLAUDE.md"重复逻辑必须抽成公共函数"相悖。

spec 默认推荐路线 A。落点汇总：`store.go`（`Open`/`Store`/新 `WriteTx`/`buildDSN`/`applyConnectionPragmas`）、`auth.go`（`AuthMetadataFromDB` 签名）、`work/store.go`（`FromDB` 签名 + 写路径）、`vcs.go`（`writeCommitInTx` 套 `writeMu`）、`bootstrap.go`（装配传递 `writeMu`）。

### 5.4 `foreign_keys=ON`（顺带补强，低风险）

`DeleteSession`（`session.go:111-126`）注释明说"FK 默认未启用，所以手动事务删 messages"。`task_work` 的 `ON DELETE CASCADE`（`work/store.go:54`）同样依赖 FK 开启才生效。F1 顺势在每连接开 `foreign_keys=ON`，让这些声明真正起作用。**风险**：开启后 `RestoreSessionAfterFailedRevert`（`session.go:312`）等批量插消息若 session 行不存在会因 FK 失败——需在测试章加回归（预期这些路径已先保证 parent 存在，但须验证）。若评审认为风险过高，此条可降级为 out-of-scope，仅保留 WAL/busy_timeout/池。

### 5.5 WAL checkpoint 策略

- **被动**：`wal_autocheckpoint=1000`（默认），由 SQLite 在写累积满 1000 页时自动 `PASSIVE` checkpoint。F1 不改默认（`config.example.yaml` 标注可调）。
- **主动 TRUNCATE（防膨胀）**：在 `Store.Close()`（`store.go:56`）前 `PRAGMA wal_checkpoint(TRUNCATE)`，把 `-wal` 截断到 0 字节，避免长跑后 `-wal` 高水位不降。
  - 注意：`TRUNCATE` 需要拿到写锁；`Close` 时持 `writeMu` 即可。
  - 关停失败不阻塞关闭（记日志、吞错）。
- **不做**定时主动 `FULL`/`RESTART` checkpoint（被动 + 关停 TRUNCATE 对 yanshi 写量足够；定时任务增加复杂度，留 out-of-scope）。
- `doctor`（`internal/cli/doctor.go:210` 读 `SQLitePath`）增一项：报告 `journal_mode` / `-wal`/`-shm` 文件大小，辅助运维诊断膨胀。

---

## 6. 并发写测试设计（新增 `internal/store/concurrency_test.go`）

测试用临时文件库（非 `:memory:`，因要验证 WAL 文件与多连接），`t.TempDir()` + `filepath.Join(...,"yanshi.db")`。

1. **`TestConcurrentAppend_NoBusy`**：N=16 goroutine 各 `AppendMessage` M=50 条到**同一 session**，全部 `require.NoError`；结尾 `SessionMessageCount == N*M`，且 `seq` 无重复（探测是否丢了并发写）。这是 WAL1 的核心验收。
2. **`TestConcurrentMixedWrite_NoBusy`**：goroutine 混合 `CreateSession` / `AppendMessage` / `UpdateSessionMeta` / `KVSet` / `WriteMemory` / `DeleteSession`，断言无 `SQLITE_BUSY`（grep 错误串）。
3. **`TestConcurrentReadWrite_NoBlocking`**：一边持续 `AppendMessage` 写，一边 `Messages`/`SessionList`/`SearchMemory` 读，读必须在 `busy_timeout` 内返回且不报错（WAL 读不阻塞写）。
4. **`TestWriteTxSerializes`**：两个 `WriteTx` 嵌套/并发，验证 `writeMu` 串行（用 channel 同步观测：第二个 tx 的 fn 必在第一个 Commit 后才开始）。
5. **`TestInMemoryForcedSingleConn`**：`Open(":memory:")` 后 `db.Stats().MaxOpenConnections == 1`，且多 goroutine 读写不"看不到对方的表"（守护 §5.2 的内存库例外）。
6. **`TestCrossProcessBusyTimeout`**（`//go:build !race` 友好）：同一文件库开**两个 `store.Open`**（模拟 backend + auth CLI 两进程），一个长写事务期间另一个写 `auth_metadata`，在 `busy_timeout` 窗口内成功而非立即 `SQLITE_BUSY`。用 `exec` 子进程做"真·跨进程"版本作为可选加强（参照 `e2e_real` build-tag 门控）。

测试一律用现有 fake（临时文件库即确定性，无需 mock），遵循 CLAUDE.md"Fake 优先"。

---

## 7. 旧 DB 升级（自动）

- `PRAGMA journal_mode=WAL` 在 rollback-journal 库上执行即**就地转换**（SQLite 重写文件头），幂等（已是 WAL 则返回 `wal` 不变）。F1 的 `applyConnectionPragmas` 里直接 `Exec` 即可，**无需版本探测**。
- 升级测试（`internal/store/wal_upgrade_test.go`）：
  1. 用**旧 Open**（或手动 `sql.Open` 不带 pragma）建库、写若干 sessions/messages、关闭，确认 `journal_mode=delete`。
  2. 用**新 Open** 重新打开，断言 `journal_mode=wal`、`synchronous=normal`、`busy_timeout>0`。
  3. 断言数据零丢失（sessions/messages 行数与内容一致）。
  4. 再次关闭/打开，幂等（仍 wal，无重复转换副作用）。
- `-wal`/`-shm` 残留文件：旧库升级后首次写产生 `-wal`/`-shm`，属正常；关停 `TRUNCATE` 后 `-wal` 归零。`*.db` 已被 VCS ignore（`vcs.go:39` `"*.db","yanshi.db"`）——确认 `-wal`/`-shm` 也被忽略（见 §11 文件表 / 风险）。

---

## 8. Windows 行为（主开发平台）

modernc.org/sqlite 在 Windows 下 WAL 可用（纯 Go，无 mmap 系统调用差异问题；`-shm` 用普通文件 + 文件锁）。已知 Windows 陷阱与守护：

- **`-wal`/`-shm` 文件占用导致无法删除/重命名库文件**：Windows 下若连接未关闭，文件句柄占住，`os.Remove(db)` 失败。守护：测试 `t.Cleanup` 显式 `store.Close()`；`doctor` 检查库前提示后端已停。
- **checkpoint `TRUNCATE` 在 Windows 下偶发无法立即收缩**（若有读连接活跃）。守护：`Close` 里 `TRUNCATE` 失败仅告警不致命；`doctor` 报告 `-wal` 实际大小。
- **跨平台测试**：F1 的并发/升级测试在 Windows CI（本项目主平台）必须绿；Unix 下跑同测试套（`go test` 无 build-tag 区分，自然跨平台）。`TestCrossProcessBusyTimeout` 的子进程加强版用 build-tag 门控，避免 Unix CI 缺依赖时失败。

---

## 9. 文件结构

| 文件 | 职责 | 新/改 |
|---|---|---|
| `internal/store/store.go` | `Open` 加 PRAGMA/池/writeMu；新 `WriteTx`/`buildDSN`/`applyConnectionPragmas`；`Close` 加 `wal_checkpoint(TRUNCATE)`；`Store.writeMu` 字段 | 改 |
| `internal/store/concurrency_test.go` | 并发写/读不报 `SQLITE_BUSY`、`WriteTx` 串行、内存库单连接守护 | 新 |
| `internal/store/wal_upgrade_test.go` | rollback→WAL 就地升级、幂等、零丢失 | 新 |
| `internal/store/wal_crossproc_test.go` | 两 `Open` 同库的 busy_timeout 行为（子进程版 build-tag 门控） | 新 |
| `internal/store/auth.go` | `AuthMetadataFromDB` 接 `writeMu`（路线 A）；`txMu` 复用或移除 | 改 |
| `internal/store/store_test.go` | `:memory:` 单连接断言更新 | 改 |
| `internal/config/config.go` | `StorageConfig` 三字段 + `applyDefaults` | 改 |
| `internal/config/config_test.go` | 新字段解析与默认 | 改 |
| `config.example.yaml` | `wal_max_open_conns`/`busy_timeout_ms`/`wal_auto_checkpoint` 示例 | 改 |
| `internal/task/work/store.go` | `FromDB` 接 `writeMu`；写路径套 `writeMu` | 改 |
| `internal/vcs/vcs.go` | `writeCommitInTx` 等写路径套 `writeMu`；`queryScoped` 语义不变 | 改 |
| `internal/bootstrap/bootstrap.go` | 装配传递 `writeMu` 给 auth/work/vcs（路线 A） | 改 |
| `internal/cli/doctor.go` | 新检查项：`journal_mode`、`-wal`/`-shm` 大小 | 改 |
| `.gitignore` | 确认 `*.db-wal` / `*.db-shm` 已忽略（或补 `*.db-*`） | 改（若缺） |

---

## 10. 测试策略（Fake 优先，全确定性）

- **驱动/PRAGMA 单元**：`applyConnectionPragmas` 后查 `PRAGMA journal_mode`/`synchronous`/`busy_timeout`/`foreign_keys` 回显正确。
- **并发写**：§6 的 6 个测试，`-race` 下跑（`go test -race ./internal/store`）。
- **读不阻塞写**：§6 #3，断言 WAL 并发语义。
- **写串行化**：§6 #4，`WriteTx` 互斥可观测。
- **内存库守护**：§6 #5，防多连接内存库回归。
- **跨进程 busy_timeout**：§6 #6，两 `Open` 同库；子进程版 build-tag 门控。
- **旧库升级**：§7，rollback→WAL 零丢失幂等。
- **消费者回归**：`work`/`vcs`/`auth` 现有测试全绿（验证 `writeMu` 接入未改语义）；`bootstrap_test.go`（`store.Open` 装配点）绿。
- **Windows 平台**：并发与升级测试在 Windows CI 必绿（主平台）。

---

## 11. 风险与缓解

| 风险 | 缓解 |
|---|---|
| 放开池后，**事务内通过池读**（`s.DB.Query` 而非 `tx.Query`）读到不同快照，破坏 `queryScoped` 不变量 | `queryScoped`/`queryRowScoped` 签名不变（仍显式传 `tx`）；spec/注释明令"事务内读必须走 `tx`"；新增写方法统一经 `WriteTx`（读天然在 `tx` 上） |
| modernc DSN `_pragma` 语法/版本差异导致 per-connection pragma 没生效（最隐蔽：`busy_timeout` 新连接=0） | 实现时核对 v1.53.0 源码；加测试**对池里多个连接**回显 `busy_timeout`/`synchronous`；退路=`sql.OpenDB`+自定义 Connector 在 Connect 里 Exec |
| `foreign_keys=ON` 让原本"静默成功"的孤儿插入失败（`RestoreSessionAfterFailedRevert` 等） | 开启前跑全量 store 测试；FK 作为可选增强（§5.4），评审可降级 out-of-scope |
| WAL `-wal`/`-shm` 文件未被 gitignore，污染工作树 | 确认/补 `.gitignore` `*.db-*`；VCS ignore 已含 `*.db`，确认 `-wal`/`-shm` 也命中 |
| Windows 下 `-wal`/`-shm` 句柄占用，库文件无法删/`doctor` 误报 | 测试 `t.Cleanup` 显式 `Close`；`doctor` 检测前提示停后端；`Close` 的 TRUNCATE 失败仅告警 |
| `writeMu` 落地路线分歧（集中 vs 分散）影响 work/vcs/auth 三包签名 | spec 推荐 `store.WriteTx` 集中原语；路线决策前置到人（§16）；无论哪条，消费者回归测试兜底 |
| D3 同时改 `internal/store`（redactor/auth_metadata）落点冲突 | 执行前 re-verify（§10 D3）；spec 任务依赖 D3 store 最终态；`store.go` 的 `Open`/`Store` 结构以 D3 合并后为准 |
| `:memory:` 多连接看到不同库（迁移/测试串味） | `Open(":memory:")` 强制 `SetMaxOpenConns(1)`；加守护测试（§6 #5） |
| checkpoint `TRUNCATE` 在活跃读连接下不收缩，`-wal` 仍膨胀 | 关停 + 被动 autocheckpoint 双保险；`doctor` 报告 `-wal` 大小供诊断；极端膨胀留 out-of-scope 的定时 FULL checkpoint |

---

## 12. 验收标准

1. `store.Open` 后查 `PRAGMA journal_mode` 返回 `wal`；`synchronous=normal`；`busy_timeout>0`；每条池连接均如此（非仅首连）。
2. `SetMaxOpenConns` 按 `wal_max_open_conns` 生效（默认 4）；`Open(":memory:")` 强制 1。
3. 16 goroutine × 50 条并发 `AppendMessage` 同 session 全部成功，`SQLITE_BUSY` 零出现，行数精确。
4. 并发读（`Messages`/`SessionList`/`SearchMemory`）与写并发时，读在 `busy_timeout` 内返回且不报错。
5. 两 `Open` 同库（模拟跨进程 auth CLI）的写冲突在 `busy_timeout` 窗口内成功，而非立即 `SQLITE_BUSY`。
6. rollback-journal 旧库经新 `Open` 自动转 WAL，数据零丢失、幂等。
7. `Store.Close` 执行 `wal_checkpoint(TRUNCATE)`；`-wal` 文件收缩至 ~0（无活跃读连接时）。
8. `work`/`vcs`/`auth`/`bootstrap` 现有测试全绿（`writeMu` 接入未改语义）。
9. Windows CI 下并发/升级测试全绿。
10. `doctor` 报告 `journal_mode` 与 `-wal`/`-shm` 大小。

---

## 13. 后续（非本批 / out-of-scope）

- 定时主动 `FULL`/`RESTART` checkpoint（应对极端写量下的 `-wal` 膨胀）。
- 拆 `sessions.db` / `vcs.db` / `work.db` 多库（降低单库写争用，但跨库事务复杂）。
- 换 `mattn/go-sqlite3`（CGO）以拿到更细的 hook 与 mmap 控制——与 PKG1 `CGO_ENABLED=0` 打包冲突，不做。
- 跨机/网络多进程并发（远端 DB）。
- `foreign_keys=ON` 的全面数据完整性审计（若 §5.4 升级，则留专项）。

---

## 14. 依赖

- **D3（soft dependency）**：D3 已改 `internal/store`（`Store.redactor`/`SetRedactor`、`auth_metadata`、`auth.go`）。F1 的 `Open`/`Store` 结构改动须基于 D3 合并后的 `store.go`/`auth.go`。**执行前 re-verify**（§10 风险表）。若 D3 尚未合并，F1 落地分支须 rebase 到 D3 最终态。
- **roadmap 内**：无其他强前置；F1 是存储地基加固，所有上层（A2 durable task、B2 seam、C4 cost）都从其并发可靠性受益。
- **外部**：`modernc.org/sqlite v1.53.0`（已锁定，不升版）。

---

## 15. 预估

- 设计已出（本 spec）。
- 实现：`store.go` Open/WriteTx/PRAGMA + config 三字段 + auth/work/vcs 写路径接 `writeMu` + bootstrap 装配 ≈ 3-4d。
- 测试：并发/升级/跨进程/内存库守护 + 消费者回归 + doctor ≈ 2-3d。
- Windows 验证 + 跨平台跑 ≈ 1d。
- **合计 ≈ 6-8d（约 1.5 周）**。

---

## 16. 需人决策的 open question

1. **写串行化落地路线**（§5.3）：路线 A（`store.WriteTx` 集中原语，推荐，改 auth/work/vcs 签名）vs 路线 B（各消费者自锁 + busy_timeout 兜底，最小侵入但易漏）。→ **需决策**。
2. **`foreign_keys=ON` 是否纳入 F1**（§5.4）：顺带补强但可能触发历史数据的孤儿插入失败；纳入 vs 降级 out-of-scope。→ **需决策**。
3. **`WALMaxOpenConns` 默认值**：4（保守）vs `runtime.NumCPU()`（激进，读并行更高但连接开销大）。→ 倾向 4，待确认。
4. **跨进程测试形态**（§6 #6）：双 `Open`（同进程两连接，够覆盖 Go 侧）vs `exec` 子进程（真·跨进程，build-tag 门控，成本高）。→ 倾向前者为主、后者可选。
5. **DSN `_pragma` vs 自定义 Connector**（§5.1）：实现时按 v1.53.0 核对；若 DSN 路线不稳，是否接受 Connector 退路（多约 30 行）。→ 倾向 DSN，Connector 作退路。
