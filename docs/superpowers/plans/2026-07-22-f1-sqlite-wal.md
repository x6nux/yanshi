# Batch F1 — SQLite 并发（WAL + 连接池 + 写串行化）Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: 用 `superpowers:executing-plans` 逐 Task 执行。每个 Task 按 TDD 顺序：先写失败测试（RED），再实现（GREEN），再跑该 Task 指定的测试命令。不要并行修改同一文件。

**Goal:** 把 yanshi 的 SQLite 存储从"单连接串行"升级为"WAL + 读连接池 + 进程内写串行化 + busy_timeout 跨进程兜底"，消除 `database is locked`，让并发读不阻塞写、进程内并发写零 `SQLITE_BUSY`，并兜底 auth CLI 子进程的跨进程访问。

**Architecture:** 改动**集中在 `store.Open`**（它产出的单一 `*sql.DB` 被 store/auth/work/vcs 四个消费者复用）。三层叠加：(1) **PRAGMA**——`journal_mode=WAL`（持久、一次性 `db.Exec`）+ `synchronous=NORMAL`/`busy_timeout=<ms>`/`wal_autocheckpoint=<pages>`（per-connection，经 DSN `_pragma` 对池里每条新连接生效）；(2) **读连接池**——`SetMaxOpenConns` 从 `1` 放开为可配（默认 4），`:memory:` 强制 1；(3) **进程内写串行化**——路线 A：集中 `store.WriteTx(ctx, fn)` 原语（`writeMu` + 单事务），store 自身写方法、auth 适配器、work.Store、VCS 全部统一走它，跨进程冲突（auth CLI）由 `busy_timeout` 兜底。`Close` 加 `wal_checkpoint(TRUNCATE)` 防 `-wal` 膨胀。`queryScoped(tx)`/`queryRowScoped(tx)`/work `BeginTx` 的事务一致性语义**完全不变**。

**Tech Stack:** Go 1.26.4；`database/sql` + `modernc.org/sqlite v1.53.0`（已锁定，不升版，`CGO_ENABLED=0`）；context value 注入；Fake 优先（临时文件库即确定性，无需 mock）；Windows 主开发平台。

**Spec:** `docs/superpowers/specs/2026-07-22-f1-sqlite-wal-design.md`（权威）。

---

## 已决策约束（团队 lead 已锁定，直接执行，不再讨论）

1. **写串行化走路线 A**：集中 `store.WriteTx(ctx, fn func(*sql.Tx) error) error` 原语，改 auth/work 签名，VCS 经 `v.store.WriteTx`。理由：符合 CLAUDE.md"重复逻辑必须抽成公共函数"——`Begin/Lock/Commit` 模式只实现一次，所有消费者共用同一个 `writeMu` 实例（在 `store.Open` 创建，bootstrap 下发）。**不采纳**路线 B（各消费者自锁 + busy_timeout 兜底，易漏、写锁分散）。
2. **`foreign_keys=ON` 不纳入 F1**（out-of-scope，不扩大范围）。DSN 不含 `foreign_keys`；`DeleteSession` 的手动事务删 messages、`task_work` 的 `ON DELETE CASCADE`（仍不生效）均为既有行为，F1 不动。spec §5.4 整条降级。
3. **`MaxOpenConns` 默认 4、可配**（`wal_max_open_conns`；`1` = 旧行为排障；`:memory:` 一律强制 `1`）。不取 `runtime.NumCPU()`。
4. **跨进程测试用双 `Open`**（同进程内对同一文件库开两个 `store.Open`，各自独立 `writeMu`，足够覆盖 Go 侧 busy_timeout 行为）。`exec` 子进程"真·跨进程"版本作为可选加强，用 `//go:build` 门控，**非阻塞**。
5. **DSN `_pragma` 路线**（非自定义 Connector）。语法按 modernc.org/sqlite **v1.53.0 源码核定**（见 §"modernc DSN 核定"）：`<path>?_pragma=busy_timeout(<ms>)&_pragma=synchronous(NORMAL)&_pragma=wal_autocheckpoint(<pages>)`，`journal_mode=WAL` 经一次性 `db.Exec` 设置（持久进 DB 头）。退路（Connector）仅在实现期发现 DSN 路线因未知原因受阻时启用，记在风险章，不预先实现。
6. **核心不变量**：`queryScoped(tx)`/`queryRowScoped(tx)`/work 事务内读**仍显式走 `tx`**——放开池后，事务内通过池（`s.DB.Query`）读会读到不同快照，**禁止**在事务里用池读。新增写方法统一经 `WriteTx`（fn 内读天然在 `tx` 上）。

---

## modernc DSN 核定（v1.53.0 源码，实现 T2 时照此）

- **路径与 query 分离**（`conn.go:62-73` `newConn`）：`pos := strings.IndexRune(dsn, '?')`；命中且 `pos >= 1` 时 `query = dsn[pos+1:]`，并且 **`if !strings.HasPrefix(dsn, "file:")` 则 `dsn = dsn[:pos]`**——即对**非 `file:` 前缀**的 DSN，modernc 把 `?` 之前的原文当文件名、`?` 之后当 query。**结论：Windows 普通路径（含反斜杠/盘符）可直接拼 `?_pragma=...`，无需 `file:` URI、无需百分号编码。** 唯一限制：DB 路径本身不能含 `?`（yanshi 的路径不含）。
- **pragma 应用**（`sqlite.go:207-237` `applyQueryParams`）：对 `url.ParseQuery(query)` 结果，遍历 `q["_pragma"]`（可重复 key），排序时**`busy_timeout` 永远最先**、其余按字母序，逐条执行 `pragma <value>`（注意：是 `pragma ` + 整个 value，故 value 形如 `busy_timeout(5000)`）。`applyQueryParams` 在**每条新池连接**的 `newConn` 里调用 → per-connection 生效。
- **`_pragma` value 语法**：`<name>(<arg>)`，例如 `busy_timeout(5000)`、`synchronous(NORMAL)`、`wal_autocheckpoint(1000)`。多个 pragma 用重复的 `_pragma=` key + `&` 分隔。
- **`journal_mode=WAL` 不进 DSN**：它是持久 PRAGMA（写进 DB 文件头）。在 `applyConnectionPragmas` 里对首条连接 `db.Exec("PRAGMA journal_mode=WAL")` 一次即可（幂等：已是 WAL 时回显 `wal`）；后续池连接从文件头继承 WAL，不重复转换。须在 `migrate()` **之前**执行（迁移也写）。
- **`SQLITE_OPEN_URI` 已默认开启**（`conn.go:99`），但因我们用非 `file:` 路径，SQLite 把剥掉 query 后的串当普通文件名，无副作用。

**故 T2 的 `buildDSN` 确定为**（伪代码）：
```go
func buildDSN(path string, busyMs, autoCkpt int) string {
    return path + "?_pragma=busy_timeout(" + strconv.Itoa(busyMs) + ")" +
        "&_pragma=synchronous(NORMAL)" +
        "&_pragma=wal_autocheckpoint(" + strconv.Itoa(autoCkpt) + ")"
}
```

---

## 职责边界与依赖方向

- `internal/store`：持有 `writeMu` 与 `WriteTx` 原语；`Open`/`OpenWith` 设 PRAGMA + 池；`Close` 加 checkpoint。**仍是组合根最底层之一，不反向依赖 work/vcs。**
- `internal/store/auth.go`（`authSQLiteAdapter`，**同包**）：`AuthMetadataFromDB` 改收 `*Store`，写经 `s.writeMu`（单语句 Exec 直接守 `writeMu`；多语句走 `WriteTx`）。移除自带的 `txMu`。
- `internal/task/work`：定义本地 `WriteTxer` interface（仅依赖 `context` + `database/sql`，**不导入 store**）；`FromDB(db, writeTx WriteTxer)`；所有写方法经注入的 `writeTx.WriteTx(ctx, fn)`。`*store.Store` 结构化满足该 interface，bootstrap 注入——依赖方向**仍向内**（work 不反向依赖 store）。
- `internal/vcs`：已持有 `v.store *store.Store`（`vcs.go:46`，`vcs.New(s *store.Store, …)`）。**签名不变**；把 3 处 `v.store.DB.Begin()` 入口（`writeCommit` `vcs.go:230`、两处 merge `vcs.go:919`、`vcs.go:1283`）整体包进 `v.store.WriteTx(ctx, func(tx){ … })`；`writeCommitInTx(tx,…)`、`queryScoped(tx,…)`、`queryRowScoped(tx,…)`、`commitDeltaScoped(tx,…)` **签名与语义不变**。
- `internal/config`：`StorageConfig` 增 3 字段 + `applyDefaults`。
- `internal/bootstrap`：组合根——`store.OpenWith(path, opts)`、把 `st` 传给 `AuthMetadataFromDB`、把 `st`（满足 `work.WriteTxer`）传给 `work.FromDB`、VCS 已收 `st`。
- `internal/cli/doctor.go`：`checkDatabase` 增 `journal_mode` 与 `-wal`/`-shm` 大小报告。
- `cmd/yanshi/main.go`：auth CLI 子命令的 `store.Open` → `store.OpenWith`、`AuthMetadataFromDB(authDB.DB)` → `AuthMetadataFromDB(authDB)`（T4 一并改）。

---

## File Structure

| 文件 | 动作 | 职责 |
|---|---|---|
| `internal/config/config.go` | 改 | `StorageConfig` 增 `WALMaxOpenConns`/`BusyTimeoutMs`/`WALAutoCheckpoint`；`applyDefaults` 补默认（4/5000/1000） |
| `internal/config/config_test.go` | 改 | 新字段解析 + 默认值断言 |
| `config.example.yaml` | 改 | `storage:` 增三键示例 + 注释 |
| `internal/store/store.go` | 改 | `Store.writeMu`；`Open`/`OpenWith`/`buildDSN`/`applyConnectionPragmas`；`WriteTx`；`Close` 加 `wal_checkpoint(TRUNCATE)` |
| `internal/store/auth.go` | 改 | `AuthMetadataFromDB(s *Store)`；`txMu` → `s.writeMu`；写路径走 writeMu/WriteTx |
| `internal/store/session.go`、`session_fork.go`、`kv.go`、`memory.go`、`task.go` | 改 | store 写方法全部经 `WriteTx`（`AppendMessage` 升级为单事务原子：insert message + bump updated_at） |
| `internal/store/store_test.go` | 改 | `:memory:` 单连接断言 + PRAGMA 回显 |
| `internal/store/concurrency_test.go` | 新 | 16×50 并发零 BUSY、混合写、读写不阻塞、WriteTx 串行、内存库单连接守护 |
| `internal/store/wal_upgrade_test.go` | 新 | rollback→WAL 就地升级、幂等、零丢失 |
| `internal/store/wal_crossproc_test.go` | 新 | 双 `Open` 同库 busy_timeout（子进程版 build-tag 门控） |
| `internal/task/work/store.go` | 改 | `WriteTxer` interface；`FromDB(db, writeTx)`；写方法经 `writeTx.WriteTx` |
| `internal/task/work/*_test.go` | 改 | `FromDB` 调用点适配（传 unlocked WriteTxer 或真实 store） |
| `internal/vcs/vcs.go` | 改 | 3 处写入口包进 `v.store.WriteTx`；`queryScoped`/`writeCommitInTx` 不变 |
| `internal/bootstrap/bootstrap.go` | 改 | `OpenWith(opts)`、`AuthMetadataFromDB(st)`、`work.FromDB(st.DB, st)` |
| `internal/cli/doctor.go` | 改 | `checkDatabase` 报 `journal_mode` + `-wal`/`-shm` 大小 |
| `cmd/yanshi/main.go` | 改 | auth CLI `OpenWith` + `AuthMetadataFromDB(authDB)` |
| `.gitignore` | 不改 | 已含 `*.db-wal`/`*.db-shm`（行 18-19），仅 T13 复核 |

---

## 依赖图

```
T1 Config ──► T2 Open+PRAGMA+writeMu+WriteTx(池仍=1) ──┬──► T3 store 写方法经 WriteTx ──────────┐
        (config.go / config_test)   (store.go)         │                                        ├──► T8 放开池 + 并发测试(16×50等)
                                                       ├──► T4 auth 适配器(*Store) ──┐          │     (依赖 T3：store 写已串行)
                                                       ├──► T5 work WriteTxer ───────┤          ├──► T9 wal_upgrade 测试
                                                       ├──► T6 vcs v.store.WriteTx ──┴──► T7 bootstrap ──┐
                                                       ├──► T9  wal_upgrade 测试                        │
                                                       ├──► T10 crossproc 双Open 测试 (依赖 T4)         ├──► T13 终验
                                                       ├──► T11 Close wal_checkpoint(TRUNCATE)          │
                                                       └──► T12 doctor journal_mode/wal 大小            │
```

- **T1 → T2**：T2 读 config 三字段。
- **T2 → {T3,T4,T5,T6,T9,T11,T12}**：都依赖 `WriteTx`/PRAGMA 原语。T2 之后这些可**并行**（不同包/文件，注意 T3、T4 同在 `store` 包但不同文件，顺序提交即可）。
- **T3 → T8**：放开池前 store 写必须已走 `writeMu`，否则 16×50 并发 `AppendMessage` 会 BUSY。
- **T4 → T10**：跨进程测试覆盖 auth CLI 写 `auth_metadata` 场景。
- **{T4,T5,T6} → T7**：bootstrap 装配需新签名都就位。
- **全部 → T13**：终验（含 D3 re-verify、E1 衔接、Windows `-race`）。

---

## 约定（全 Task 适用）

- **WriteTx 不可重入**：`writeMu` 是 `sync.Mutex`。store/work/vcs 的写方法**不得互相调用**；需要组合多步时，在**最外层**用一次 `WriteTx`，把多步放进 `fn`。VCS 的 `writeCommitInTx(tx,…)`（收 `tx`、不自建 WriteTx）天然可被外层 WriteTx 的 `fn` 调用。
- **每 Task 单独提交**，提交信息前缀 `feat(f1):` / `test(f1):` / `refactor(f1):`。
- **测试用临时文件库**（`t.TempDir()` + `filepath.Join(...,"yanshi.db")`），非 `:memory:`（除专门测内存库守护的 T8 #5）。Fake 优先，不引 mock。
- **`-race` 友好**：并发测试 `go test -race ./internal/store` 必须绿。

---

## Task 1: config 三字段 + 默认值 + 示例

**Files:**
- 改：`internal/config/config.go`（`StorageConfig` §298-300、`applyDefaults` §408）
- 改：`internal/config/config_test.go`
- 改：`config.example.yaml`

- [ ] **Step 1（RED）：写失败测试**

`config_test.go` 增：解析含三键的 YAML 得到用户值；省略三键时 `applyDefaults` 后为 `4/5000/1000`；`wal_max_open_conns: 1` 透传为 `1`（排障路径）；`wal_auto_checkpoint: -1`（禁用）透传。

```go
func TestStorageDefaults_WAL(t *testing.T) {
	cfg, err := config.LoadBytes([]byte("storage:\n  sqlite_path: yanshi.db\n"))
	require.NoError(t, err)
	assert.Equal(t, 4, cfg.Storage.WALMaxOpenConns)
	assert.Equal(t, 5000, cfg.Storage.BusyTimeoutMs)
	assert.Equal(t, 1000, cfg.Storage.WALAutoCheckpoint)
}
```

- [ ] **Step 2（GREEN）：实现**

```go
type StorageConfig struct {
	SQLitePath string `yaml:"sqlite_path"`
	// WALMaxOpenConns 放开读连接池上限（F1）。0/省略=4；1=旧行为（单连接，排障）。
	// 写仍由进程内 writeMu 串行，故此值只影响读并行度。
	WALMaxOpenConns int `yaml:"wal_max_open_conns"`
	// BusyTimeoutMs 是 SQLite busy_timeout（F1），跨进程锁冲突重试窗口。0/省略=5000。
	BusyTimeoutMs int `yaml:"busy_timeout_ms"`
	// WALAutoCheckpoint 是 wal_autocheckpoint 页阈值（F1）。0/省略=1000；负数=禁用被动 checkpoint（不推荐）。
	WALAutoCheckpoint int `yaml:"wal_auto_checkpoint"`
}
```

`applyDefaults` 末尾补：`if c.Storage.WALMaxOpenConns == 0 { c.Storage.WALMaxOpenConns = 4 }`；`BusyTimeoutMs`→5000；`WALAutoCheckpoint`→1000。（零值合法，不加校验。）

`config.example.yaml` 的 `storage:` 块加三键 + 行内注释（照 spec §4.2）。

- [ ] **Run:** `go test ./internal/config -run TestStorage -count=1`
- [ ] **Commit:** `feat(f1): add WAL/busy_timeout/autockpt storage config fields`

---

## Task 2: store.Open PRAGMA + writeMu + WriteTx 原语（池仍 = 1）

> 本 Task 引入全部原语但**保留 `SetMaxOpenConns` 旧行为**（文件库走默认 4 由 T8 才放开，见下），确保中间提交零 `SQLITE_BUSY` 风险。实际做法：`Open/OpenWith` 已按 opts 设池；但 T2 的测试只跑单线程 PRAGMA 回显，T8 才加并发测试并确认池行为。**为避免 T2→T8 之间任何并发测试撞 BUSY，T2 引入 `writeMu`+`WriteTx` 原语即可，store 写方法在 T3 才切换。**

> 实施注意：`Open(path)` 保持旧签名 = `OpenWith(path, DefaultOptions)`；`OpenWith(path, OpenOptions{MaxOpenConns,BusyTimeoutMs,WALAutoCheckpoint})`。`DefaultOptions = {4,5000,1000}`。

**Files:**
- 改：`internal/store/store.go`（`Store` 结构体、`Open`、新增 `OpenWith`/`buildDSN`/`applyConnectionPragmas`/`WriteTx`、`Close` 先不动，T11 改）
- 改：`internal/store/store_test.go`

- [ ] **Step 1（RED）：写失败测试**

```go
func TestOpen_AppliesWALPragmas(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "yanshi.db")
	st, err := Open(path)
	require.NoError(t, err)
	defer st.Close()
	// 持久 PRAGMA
	assertPragma(t, st.DB, "journal_mode", "wal")
	// per-connection（首连）
	assertPragma(t, st.DB, "synchronous", "normal")
	assertPragmaSqrt(t, st.DB, "busy_timeout", func(v int) bool { return v > 0 })
}

// 风险章重点：池里每条连接都要有 busy_timeout/synchronous，否则新连接 timeout=0。
func TestOpen_PragmasOnEveryPoolConn(t *testing.T) {
	dir := t.TempDir()
	st, _ := Open(filepath.Join(dir, "yanshi.db"))
	defer st.Close()
	for i := 0; i < 4; i++ { // 强制开 4 条连接
		c, err := st.DB.Conn(context.Background())
		require.NoError(t, err)
		var bt int
		require.NoError(t, c.QueryRowContext(context.Background(), "PRAGMA busy_timeout").Scan(&bt))
		require.Greater(t, bt, 0, "pool conn %d busy_timeout must be set", i)
		_ = c.Close()
	}
}

func TestOpen_MemoryForcesSingleConn(t *testing.T) {
	st, err := Open(":memory:")
	require.NoError(t, err)
	defer st.Close()
	assert.Equal(t, 1, st.DB.Stats().MaxOpenConnections)
}

func TestWriteTx_CommitsAndRollsBack(t *testing.T) {
	st, _ := Open(":memory:")
	defer st.Close()
	require.NoError(t, st.WriteTx(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec("INSERT INTO kv(key,value) VALUES('k','v')")
		return err
	}))
	var v string
	_ = st.DB.QueryRow("SELECT value FROM kv WHERE key='k'").Scan(&v)
	assert.Equal(t, "v", v)
	// fn 返回 error → rollback
	err := st.WriteTx(context.Background(), func(tx *sql.Tx) error {
		return fmt.Errorf("boom")
	})
	assert.Error(t, err)
}
```

- [ ] **Step 2（GREEN）：实现**

`Store` 增字段（D3 已有 `redactor`，保留）：
```go
type Store struct {
	DB *sql.DB
	writeMu sync.Mutex          // F1: 进程内写串行化，保证 WAL 单写零 SQLITE_BUSY
	redactor *secrets.Redactor  // D3
}
```

`OpenOptions` 与 `DefaultOptions`；`buildDSN`（照 §"modernc DSN 核定"）；`applyConnectionPragmas`：
```go
func (s *Store) applyConnectionPragmas() error {
	// journal_mode=WAL：持久，一次性 Exec（幂等）。须在 migrate() 前。
	if _, err := s.DB.Exec("PRAGMA journal_mode=WAL"); err != nil {
		return fmt.Errorf("store: set WAL: %w", err)
	}
	return nil
}
```
（`synchronous`/`busy_timeout`/`wal_autocheckpoint` 已由 DSN `_pragma` 对每条池连接生效，不在此重复 Exec。）

`OpenWith`：
```go
func OpenWith(path string, opts OpenOptions) (*Store, error) {
	maxOpen := opts.MaxOpenConns
	if maxOpen <= 0 { maxOpen = DefaultOptions.MaxOpenConns }
	busyMs := opts.BusyTimeoutMs;  if busyMs <= 0  { busyMs = DefaultOptions.BusyTimeoutMs }
	ckpt := opts.WALAutoCheckpoint; if ckpt == 0   { ckpt = DefaultOptions.WALAutoCheckpoint }
	dsn := buildDSN(path, busyMs, ckpt)
	db, err := sql.Open("sqlite", dsn)
	if err != nil { return nil, err }
	// :memory: 多连接看到不同库 → 强制单连接（迁移/测试串味的唯一守护）。
	if path == ":memory:" {
		maxOpen = 1
	}
	db.SetMaxOpenConns(maxOpen)
	if maxOpen > 1 {
		db.SetConnMaxIdleTime(5 * time.Minute) // 回收空闲读连接；不设 ConnMaxLifetime
	}
	s := &Store{DB: db}
	if err := s.applyConnectionPragmas(); err != nil { db.Close(); return nil, err }
	if err := s.migrate(); err != nil { db.Close(); return nil, err }
	return s, nil
}
func Open(path string) (*Store, error) { return OpenWith(path, DefaultOptions) }
```

`WriteTx`（**核心原语，全消费者共用**）：
```go
// WriteTx 在进程内写锁 writeMu 下执行 fn，保证 WAL 单写不产生 SQLITE_BUSY。
// 跨进程冲突（auth CLI 子进程）由 DSN busy_timeout 兜底。tx 由本函数 Begin/Commit。
// 不可重入：fn 内不得再次调用本 Store 的写方法（会死锁）。
func (s *Store) WriteTx(ctx context.Context, fn func(*sql.Tx) error) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil { return err }
	if err := fn(tx); err != nil { _ = tx.Rollback(); return err }
	return tx.Commit()
}
```

- [ ] **Run:** `go test ./internal/store -run 'TestOpen_|TestWriteTx_' -count=1`
- [ ] **Commit:** `feat(f1): WAL pragmas, connection pool, WriteTx primitive`
- [ ] **D3 re-verify：** 执行前确认 `store.go` 的 `Store` 结构体已是 D3 最终态（`redactor`/`SetRedactor` 在位）。当前 main 已含 D3 redactor；若 D3 还有未合并的 `store.go`/`auth.go` 改动，本批分支须 rebase 到 D3 最终态再动笔。

---

## Task 3: store 写方法全部经 WriteTx

> 在 T2 池已可 >1 的情况下，store 自身写必须先串行，否则 T8 的 16×50 并发 `AppendMessage` 会 BUSY。本 Task 在**单连接语义下也正确**（WriteTx 是 belt-and-suspenders），故中间提交 GREEN。

**Files:**
- 改：`internal/store/session.go`（`CreateSession`/`AppendMessage`/`UpdateSessionTitle`/`UpdateSessionMeta`/`SetSessionArchived`/`DeleteSession`/revert 系列 `session.go:248,273,316`）
- 改：`internal/store/session_fork.go`（`ForkSession` `session_fork.go:41`）
- 改：`internal/store/kv.go`（`KVSet` `kv.go:7`）
- 改：`internal/store/memory.go`（`WriteMemory` `memory.go:19`）
- 改：`internal/store/task.go`（各 `s.DB.Exec` 写 `task.go:32,48,70,90,128,180,202,221,245,275`）

- [ ] **Step 1（RED）：写失败测试**

`AppendMessage` 升级为单事务原子（insert message + bump `updated_at` 在同一 tx）。测试断言：追加后 `sessions.updated_at` 被刷新；并发的 2 个 `AppendMessage` 都成功（此处置小规模，大规模 16×50 在 T8）。

```go
func TestAppendMessage_BumpsUpdatedAtAtomically(t *testing.T) {
	st, _ := Open(":memory:"); defer st.Close()
	sid, _ := st.CreateSession("t")
	before := updated(t, st, sid)
	time.Sleep(time.Millisecond) // 确保 now 不同
	require.NoError(t, st.AppendMessage(sid, 1, "user", "hi"))
	assert.Greater(t, updated(t, st, sid), before)
}
```

- [ ] **Step 2（GREEN）：实现**

单语句写（`CreateSession`/`UpdateSessionTitle`/`UpdateSessionMeta`/`SetSessionArchived`/`KVSet`/`WriteMemory`/`task.go` 各）：
```go
func (s *Store) CreateSession(title string) (string, error) {
	id, now := newID(), time.Now().Unix()
	err := s.WriteTx(context.Background(), func(tx *sql.Tx) error {
		_, e := tx.Exec("INSERT INTO sessions(id,title,created_at,updated_at) VALUES(?,?,?,?)",
			id, s.redact(title), now, now)
		return e
	})
	if err != nil { return "", err }
	return id, nil
}
```

`AppendMessage`（多语句 → 单事务原子）：
```go
func (s *Store) AppendMessage(sessionID string, seq int, role, content string) error {
	id, now := newID(), time.Now().Unix()
	return s.WriteTx(context.Background(), func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO messages(id,session_id,seq,role,content,created_at) VALUES(?,?,?,?,?,?)`,
			id, sessionID, seq, role, s.redact(content), now); err != nil { return err }
		_, err := tx.Exec("UPDATE sessions SET updated_at=? WHERE id=?", now, sessionID)
		return err
	})
}
```

`DeleteSession`/revert/`ForkSession`：把现有 `tx, err := s.DB.Begin(); defer tx.Rollback(); …; return tx.Commit()` 整体搬进 `s.WriteTx(ctx, func(tx *sql.Tx) error { … })`，删掉手写 Begin/Commit/Rollback（WriteTx 接管）。注意这些函数内已用 `tx.Exec`/`tx.QueryRow`——保持不变，只是外层换成 WriteTx 的 `fn`。

- [ ] **Run:** `go test ./internal/store -count=1`（全 store 包测试须绿，验证 WriteTx 接入未改语义）
- [ ] **Commit:** `refactor(f1): route store writes through WriteTx`

---

## Task 4: auth 适配器走 writeMu（`AuthMetadataFromDB(s *Store)`）

**Files:**
- 改：`internal/store/auth.go`
- 改：`cmd/yanshi/main.go`（`main.go:290,296` auth CLI）
- 改：`internal/bootstrap/bootstrap.go`（`bootstrap.go:324`）——若 T7 统一改，此处先只改签名调用点

- [ ] **Step 1（RED）：写失败测试**

复用 D3 既有 auth 并发测试语义：多 goroutine 并发 `SaveAuthMetadata`/`DeleteAuthMetadata` 全成功、无 BUSY；单进程内 auth 写与 store 写共用同一 `writeMu`。

```go
func TestAuthMetadata_ConcurrentSave_NoBusy(t *testing.T) {
	st, _ := Open(":memory:"); defer st.Close()
	a := AuthMetadataFromDB(st)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done()
			_ = a.SaveAuthMetadata("p", fmt.Sprint(i), auth.AuthMetadata{Source: "secret"})
		}(i)
	}
	wg.Wait()
}
```

- [ ] **Step 2（GREEN）：实现**

```go
type authSQLiteAdapter struct{ s *Store }   // 移除 db *sql.DB + txMu sync.Mutex

// AuthMetadataFromDB 改收 *Store：复用进程级 writeMu，与其他消费者共用同一写串行。
func AuthMetadataFromDB(s *Store) auth.MetadataStore { return &authSQLiteAdapter{s: s} }
```

单语句写（`SaveAuthMetadata`/`DeleteAuthMetadata`）守 `writeMu`：
```go
func (a *authSQLiteAdapter) SaveAuthMetadata(provider, account string, meta auth.AuthMetadata) error {
	a.s.writeMu.Lock(); defer a.s.writeMu.Unlock()
	expiresAt := int64(0)
	if !meta.ExpiresAt.IsZero() { expiresAt = meta.ExpiresAt.Unix() }
	_, err := a.s.DB.Exec(`INSERT INTO auth_metadata … ON CONFLICT …`, …)
	return err
}
```
`LoadAuthMetadata` 是读，**不持** `writeMu`，直接 `a.s.DB.QueryRow`。

调用点：`bootstrap.go:324` `store.AuthMetadataFromDB(st.DB)` → `store.AuthMetadataFromDB(st)`；`cmd/yanshi/main.go:296` `store.AuthMetadataFromDB(authDB.DB)` → `store.AuthMetadataFromDB(authDB)`。auth CLI 的 `store.Open(cfg.Storage.SQLitePath)`（`main.go:290`）→ `store.OpenWith(cfg.Storage.SQLitePath, optsFrom(cfg.Storage))`（拿跨进程 busy_timeout）。

- [ ] **Run:** `go test ./internal/store -run AuthMetadata -count=1`；`go build ./cmd/yanshi`
- [ ] **Commit:** `refactor(f1): auth adapter shares process writeMu`
- [ ] **D3 re-verify：** `auth.go` 的 adapter 形态以 D3 最终态为准（D3 已建 `authSQLiteAdapter`+`txMu`）；本 Task 仅把 `txMu` 换成共享 `writeMu`，不改 schema/语义。

---

## Task 5: work.Store 注入 WriteTxer

**Files:**
- 改：`internal/task/work/store.go`（`FromDB`、所有写方法）
- 改：`internal/task/work/*_test.go`（`FromDB` 调用点）

- [ ] **Step 1（RED）：写失败测试**

新增/调整：`FromDB` 须收 `WriteTxer`；传 nil 时回退到 unlocked（兼容老测试）；多 goroutine 并发 `Transition`/`AppendTimeline` 无 BUSY。

```go
// unlockedWriteTxer 供不需要跨消费者串行的测试用（行为等同旧 BeginTx）。
type unlockedWriteTxer struct{ db *sql.DB }
func (u unlockedWriteTxer) WriteTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := u.db.BeginTx(ctx, nil); if err != nil { return err }
	if err := fn(tx); err != nil { _ = tx.Rollback(); return err }
	return tx.Commit()
}
```

- [ ] **Step 2（GREEN）：实现**

```go
// WriteTxer 在进程内写锁 + 单事务内执行 fn。*store.Store 结构化满足它；
// bootstrap 注入 store，使 work 写与 store/auth/vcs 共用同一 writeMu。
// work 不导入 internal/store（依赖方向向内），仅依赖此 interface。
type WriteTxer interface { WriteTx(ctx context.Context, fn func(*sql.Tx) error) error }

type Store struct {
	db      *sql.DB
	writeTx WriteTxer // 可为 nil（老测试 / 排障）→ 回退 unlocked
}
func FromDB(db *sql.DB, writeTx WriteTxer) (*Store, error) {
	s := &Store{db: db, writeTx: writeTx}
	if err := s.migrate(); err != nil { return nil, err }
	return s, nil
}
// wt：nil-safe 包装
func (s *Store) wt() WriteTxer {
	if s.writeTx != nil { return s.writeTx }
	return unlockedWriteTxer{db: s.db}
}
```

每个写方法（`Create`/`Transition`/`SetChecklist`/`AddChecklistItem`/`RecordGate`/`AppendTimeline`/`PutArtifact`/`PatchChecklistItem`/`AttachBrokerTask`/`DeleteArtifactsBefore`）把 `tx, err := s.db.BeginTx(ctx,nil); defer tx.Rollback(); …; return tx.Commit()` 改写为：
```go
func (s *Store) Create(ctx context.Context, w *WorkTask) error {
	return s.wt().WriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO task_work …`, …); err != nil { return fmt.Errorf("work: insert task: %w", err) }
		for _, item := range w.Checklist.Items { … tx.ExecContext … }
		for _, e := range w.Timeline { … tx.ExecContext … }
		return nil
	})
}
```
返回值的（`AddChecklistItem (int, error)`）：在 `fn` 内写闭包变量、fn 返回后回读。

更新 work 测试调用点：`work.FromDB(db)` → `work.FromDB(db, nil)`（nil 走 unlocked，保留旧行为）。

- [ ] **Run:** `go test ./internal/task/work -count=1`
- [ ] **Commit:** `refactor(f1): work store routes writes through injected WriteTxer`

---

## Task 6: VCS 写路径走 `v.store.WriteTx`

> VCS 已持有 `v.store *store.Store`，**无需改 VCS 公开签名**。把 3 处写入口整体包进 `v.store.WriteTx`。`writeCommitInTx(tx,…)`/`queryScoped(tx,…)`/`queryRowScoped(tx,…)`/`commitDeltaScoped(tx,…)` 完全不变。

**Files:**
- 改：`internal/vcs/vcs.go`（`writeCommit` `:229-239`、merge `:917-…`、merge `:1281-…`，及裸写 `:141,631,701,751`）

- [ ] **Step 1（RED）：写失败测试**

复用既有 `seam_race_test.go` 的并发提交场景，断言无 BUSY；或新增 `TestVCS_ConcurrentCommits_NoBusy`（N goroutine 并发 `Commit`/`MergeToMain` 到同一 repo，全成功）。

- [ ] **Step 2（GREEN）：实现**

`writeCommit`（`:229`）：
```go
func (v *VCS) writeCommit(repoID, worktreeID, parentID, mergedFrom, author, message string, tree map[string]string) (string, error) {
	var id string
	err := v.store.WriteTx(context.Background(), func(tx *sql.Tx) error {
		var e error
		id, e = v.writeCommitInTx(tx, repoID, worktreeID, parentID, mergedFrom, author, message, tree)
		return e
	})
	return id, err
}
```

两处 merge（`:919`、`:1283`）：把 `tx, err := v.store.DB.Begin(); defer tx.Rollback(); cid, err := v.writeCommitInTx(tx,…); …; return cid, tx.Commit()` 整体搬进 `v.store.WriteTx(ctx, func(tx *sql.Tx) error{ … })`，函数内 `queryScoped(tx,…)`/`commitDeltaScoped(tx,…)` 调用不变。

裸写（`vcs.go:141` `v.store.DB.Exec` 写 `vcs_blobs` 等、`:631`/`:701`/`:751` worktree/repo 元数据写）：逐一守 `v.store.WriteTx`（单语句 Exec 包成 fn）。读（`v.store.DB.QueryRow`/`Query`，如 `:152,165,185,564,572`）**不动**。

**注意 WriteTx 不可重入**：VCS 公开写方法（`Commit`/`MergeToMain`/`InitRepo`/seal 等）在最外层包一次 WriteTx；其调用的 `writeCommitInTx`/`queryScoped` 收 `tx`、不再 WriteTx，不会嵌套。

- [ ] **Run:** `go test ./internal/vcs -count=1`（含 `-race`：`go test -race ./internal/vcs -run Seam -count=1`）
- [ ] **Commit:** `refactor(f1): vcs writes route through store.WriteTx`

---

## Task 7: bootstrap 装配（OpenWith + 注入 writeMu 能力）

**Files:**
- 改：`internal/bootstrap/bootstrap.go`（`:276` Open、`:324` AuthMetadataFromDB、`:596` work.FromDB、`:857` VCS 段）

- [ ] **Step 1（RED）：写失败测试**

`bootstrap_test.go`/`c1_test.go` 断言 `App.Store` 经 `OpenWith` 打开（`journal_mode=wal`）、`authMgr` 的 metadata store 用同一 `*Store`、work store 注入了 `WriteTxer`（不为 nil）。若现有 bootstrap 测试不便内省，至少断言 `Build` 成功且 `App.Store.DB` 查 `PRAGMA journal_mode` 为 `wal`。

- [ ] **Step 2（GREEN）：实现**

```go
st, err := store.OpenWith(cfg.Storage.SQLitePath, store.OpenOptions{
	MaxOpenConns:     cfg.Storage.WALMaxOpenConns,
	BusyTimeoutMs:    cfg.Storage.BusyTimeoutMs,
	WALAutoCheckpoint: cfg.Storage.WALAutoCheckpoint,
})
…
authMgr.SetMetadataStore(store.AuthMetadataFromDB(st))           // 共享 writeMu
…
workStore, err := work.FromDB(st.DB, st)                          // st 满足 work.WriteTxer
```
VCS 段（`:857`）`vcs.New(st, …)` 不变（已收 `*Store`）。`closeStoreOnError` 改调 `st.Close()`（T11 后含 checkpoint）。

- [ ] **Run:** `go test ./internal/bootstrap -count=1`
- [ ] **Commit:** `feat(f1): wire WAL pool and shared writeMu in bootstrap`

---

## Task 8: 放开连接池 + 并发测试门禁（16×50 零 BUSY + 读写不阻塞）

> 这是 F1 的**核心验收 commit**。T2 已让 `OpenWith` 按 opts 设池（默认 4），但此前无并发测试；本 Task 加入并发测试，正式 gate 池 + writeMu 的组合。`TestConcurrentReadWrite_NoBlocking` 是真正的 RED→GREEN（单连接下写阻塞读、超时；池+WAL 下读在 busy_timeout 内返回）。

**Files:**
- 新：`internal/store/concurrency_test.go`

- [ ] **Step 1（RED）：写测试**

```go
// #1 WAL1 核心验收：16 goroutine × 50 条并发 AppendMessage 到同一 session，
// 全部 NoError；行数精确 == 16*50；seq 无重复（探测丢并发写）；无 SQLITE_BUSY。
func TestConcurrentAppend_NoBusy(t *testing.T) {
	st, _ := Open(filepath.Join(t.TempDir(), "yanshi.db")); defer st.Close()
	sid, _ := st.CreateSession("c")
	const N, M = 16, 50
	var wg sync.WaitGroup
	start := make(chan struct{})
	for g := 0; g < N; g++ {
		wg.Add(1)
		go func(g int) { defer wg.Done()
			<-start
			for i := 0; i < M; i++ {
				if err := st.AppendMessage(sid, g*M+i, "user", fmt.Sprintf("g%d-i%d", g, i)); err != nil {
					t.Errorf("append: %v", err); return
				}
			}
		}(g)
	}
	close(start); wg.Wait()
	count, _ := st.SessionMessageCount(sid)
	assert.Equal(t, N*M, count, "no messages lost under concurrent write")
	// 探测 SQLITE_BUSY 串：此处用 require.NoError 已覆盖；另可 t.Logf 无 error。
}

// #2 混合并发写（Create/Append/UpdateMeta/KVSet/WriteMemory/Delete），无 BUSY。
func TestConcurrentMixedWrite_NoBusy(t *testing.T) { … }

// #3 读写不阻塞：一边持续 AppendMessage 写，一边 Messages/SessionList/SearchMemory 读，
// 读在 busy_timeout 内返回且不报错（WAL 读不阻塞写）。单连接下此测试会超时 → RED。
func TestConcurrentReadWrite_NoBlocking(t *testing.T) {
	… // 读侧用 ctx.WithTimeout(busy) 断言读完成前不被写阻塞太久
}

// #4 WriteTx 串行：两个 WriteTx 并发，第二个 fn 必在第一个 Commit 后才开始（channel 观测）。
func TestWriteTxSerializes(t *testing.T) { … }

// #5 :memory: 单连接守护：Open(":memory:") 后 MaxOpenConnections==1，
// 且多 goroutine 读写"看得到对方的表"（迁移不串味）。
func TestInMemoryForcedSingleConn(t *testing.T) { … }
```

- [ ] **Step 2（GREEN）：确认**

T2–T7 已就位（池默认 4 + writeMu 全消费者接入）。测试应直接绿。若 `TestConcurrentAppend_NoBusy` 出 BUSY → 说明某消费者写未走 WriteTx，回查 T3/T5/T6。

- [ ] **Run:** `go test -race ./internal/store -run 'TestConcurrent|TestWriteTx|TestInMemory' -count=1`
- [ ] **Commit:** `test(f1): concurrent 16x50 zero-BUSY, read-does-not-block, writeTx serializes`

---

## Task 9: WAL 升级测试（rollback→WAL 就地、幂等、零丢失）

**Files:**
- 新：`internal/store/wal_upgrade_test.go`

- [ ] **Step 1（RED）：写测试**

1. 用**裸 modernc**（`sql.Open("sqlite", path)`，不带 `_pragma`、不设 WAL）建库、写若干 sessions/messages、关闭；断言 `PRAGMA journal_mode` ∈ {`delete`,`rollback`}。
2. 用**新 `Open`** 重开 → 断言 `journal_mode=wal`、`synchronous=normal`、`busy_timeout>0`。
3. 断言数据零丢失（sessions/messages 行数与内容一致）。
4. 再关闭/打开 → 幂等（仍 `wal`，无副作用）。

- [ ] **Step 2（GREEN）：确认**（实现已在 T2；本测试是验收）

- [ ] **Run:** `go test ./internal/store -run TestWALUpgrade -count=1`
- [ ] **Commit:** `test(f1): rollback-to-WAL in-place upgrade, idempotent, zero-loss`

---

## Task 10: 跨进程 busy_timeout（双 Open）

**Files:**
- 新：`internal/store/wal_crossproc_test.go`（双 Open 版必做；`exec` 子进程版 `//go:build f1_xproc` 门控，可选）

- [ ] **Step 1（RED）：写测试**

```go
// 同一文件库开两个 store.Open（模拟 backend + auth CLI 两进程，各自独立 writeMu）。
// 一个长写事务期间，另一个写 auth_metadata，在 busy_timeout 窗口内成功而非立即 BUSY。
func TestCrossProcessBusyTimeout_DualOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "yanshi.db")
	a, _ := Open(path); defer a.Close()
	b, _ := Open(path); defer b.Close()
	// a 持写锁：在其 writeMu 下开一个未提交的写 tx，阻塞 b。
	a.writeMu.Lock()
	done := make(chan error, 1)
	go func() {
		done <- b.AuthMetadataFromDBForTest().SaveAuthMetadata("p", "x", auth.AuthMetadata{Source: "secret"})
	}()
	time.Sleep(50 * time.Millisecond) // 让 b 开始等
	a.writeMu.Unlock()                // 释放 → b 的写应在 busy_timeout 内成功
	select {
	case err := <-done: assert.NoError(t, err)
	case <-time.After(6 * time.Second): t.Fatal("second writer timed out — busy_timeout not effective")
	}
}
```
（测试需要一个拿到 `authSQLiteAdapter` 的途径——或直接对 `b` 用 `KVSet`/`AppendMessage` 测跨 Open 写，避免暴露私有 adapter。实现期取最简：用 `b.KVSet` 代替 auth 写。）

`exec` 子进程版：用 `//go:build f1_xproc` 门控，`go test -tags f1_xproc`，拉起 `yanshi` 子进程写 auth_metadata——**非阻塞**，T13 前可不做。

- [ ] **Step 2（GREEN）：确认**（依赖 T2 DSN busy_timeout + T4 auth 写路径）

- [ ] **Run:** `go test ./internal/store -run TestCrossProcess -count=1`
- [ ] **Commit:** `test(f1): cross-process busy_timeout via dual-Open`

---

## Task 11: Store.Close 加 `wal_checkpoint(TRUNCATE)`

**Files:**
- 改：`internal/store/store.go`（`Close` `:56`）

- [ ] **Step 1（RED）：写测试**

```go
func TestClose_CheckpointsWAL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "yanshi.db")
	st, _ := Open(path)
	sid, _ := st.CreateSession("t")
	for i := 0; i < 50; i++ { _ = st.AppendMessage(sid, i, "user", strings.Repeat("x", 1000)) }
	require.NoError(t, st.Close())
	// 关停后 -wal 应被 TRUNCATE 到 ~0（无活跃读连接时）。
	fi, err := os.Stat(path + "-wal")
	if err == nil { assert.Less(t, fi.Size(), int64(1<<16), "wal not truncated on close") }
}
```

- [ ] **Step 2（GREEN）：实现**

```go
func (s *Store) Close() error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	// 关停主动 TRUNCATE：把 -wal 收缩到 ~0，防长跑后高水位不降。
	// 需写锁；TRUNCATE 失败仅告警（Windows 下活跃读连接偶发不收缩），不阻塞 Close。
	if _, err := s.DB.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		// 不返回 err：checkpoint 失败不应让 Close 失败（见 spec §5.5/§8）。
	}
	return s.DB.Close()
}
```

- [ ] **Run:** `go test ./internal/store -run TestClose -count=1`
- [ ] **Commit:** `feat(f1): wal_checkpoint(TRUNCATE) on Store.Close`

---

## Task 12: doctor 报告 journal_mode + -wal/-shm 大小

**Files:**
- 改：`internal/cli/doctor.go`（`checkDatabase` `:206-221`）

- [ ] **Step 1（RED）：写测试**

`doctor` 测试（参照既有 `doctor_test.go` 风格）断言：`checkDatabase` 返回的 Message 含 `journal_mode=wal` 与 `-wal`/`-shm` 文件大小（无文件时报 0/absent）。

- [ ] **Step 2（GREEN）：实现**

`checkDatabase`：`store.Open`/`OpenWith` 后查 `PRAGMA journal_mode`；`os.Stat(path+"-wal")`/`os.Stat(path+"-shm")` 取大小；拼进 `CheckResult.Message`（如 `"%s opened (wal, -wal=12KB -shm=32KB)"`）。`_ = st.Close()`（含 T11 checkpoint）。**提示后端已停**的检查保持既有（spec §8）。

- [ ] **Run:** `go test ./internal/cli -run TestDoctor -count=1`
- [ ] **Commit:** `feat(f1): doctor reports journal_mode and wal/shm sizes`

---

## Task 13: 终验（Windows -race 全绿 + D3 re-verify + E1 衔接 + .gitignore 复核）

**Files:**（无新代码；验证 + 必要微调）

- [ ] **Step 1：全量验证命令**

```sh
go build -o yanshi.exe ./cmd/yanshi
go vet ./...
go test -race ./internal/store ./internal/vcs ./internal/task/work ./internal/bootstrap ./internal/config -count=1
go test ./...                                 # 全量（缓存生效），确认无回归
go run ./cmd/testchanged -count=1            # 仅变更包（含 auth CLI 间接）
timeout 5 ./yanshi.exe --fake-model -inprocess # 启动自检（alt-screen 不可管道驱动）
```

- [ ] **Step 2：D3 re-verify（门禁）**

复核 `internal/store/store.go` 与 `internal/store/auth.go` **已是 D3 最终态**：`Store.redactor`/`SetRedactor`/`auth_metadata` 表/`authSQLiteAdapter` 在位且未被 F1 改写语义。当前 main 的 `store.go` 已含 D3 redactor；若执行期间 D3 又改了 `store.go`/`auth.go`，rebase 后重跑 T2/T3/T4。**F1 不引入 `foreign_keys=ON`，不删 `DeleteSession` 手动事务。**

- [ ] **Step 3：E1 ConcurrentAppend 衔接**

E1（`docs/superpowers/specs/2026-07-22-e1-test-coverage-design.md`）在 `internal/store/session_test.go` 落了 `TestSession_ConcurrentAppend`（4 goroutine 并发追加，`-race`），并因"F1 WAL 未落地"加了 `t.Skip` 兜底（E1 spec 行 113、127）。**F1 合并后**：移除该 `t.Skip` 守卫（或确认其在 WAL 下直接绿），让 E1 的轻量覆盖与 F1 的 `TestConcurrentAppend_NoBusy`（16×50）并存——前者覆盖 `AppendMessage` 代码路径，后者是 WAL+writeMu 机制的权威压测。若 E1 尚未合并，本步记为"待 E1 合并后执行"。

- [ ] **Step 4：.gitignore 复核**

确认 `.gitignore` 行 16-19 已含 `*.db`/`*.db-journal`/`*.db-wal`/`*.db-shm`（已确认，**无需改动**）；VCS ignore `internal/vcs/vcs.go:39` 的 `"*.db","yanshi.db"` 覆盖 `-wal`/`-shm`（命中 `*.db` 前缀需复核——若 VCS ignore 不命中 `-wal`，补 `*.db-wal`/`*.db-shm` 到 vcs ignore 列表）。

- [ ] **Run:** 上述全量命令
- [ ] **Commit:** `test(f1): final verification, re-verify D3, coordinate E1`

---

## 与 E1（ConcurrentAppend）的衔接

- E1 的 `TestSession_ConcurrentAppend`（`internal/store/session_test.go`，4 goroutine，`-race`）与 F1 的 `TestConcurrentAppend_NoBusy`（16×50）是**互补关系**：E1 覆盖 `AppendMessage` 的代码路径与边界（空 session、超大 content）；F1 覆盖 WAL + writeMu 的并发机制本身。
- E1 spec 明确：并发追加"F1 WAL 未落地前可能 `database is locked`"，故加 `t.Skip` 兜底。**F1 是该 Skip 的解除条件**——F1 合并后 E1 的 Skip 应移除。执行顺序建议：**F1 先于 E1 合并**（或同批），E1 合并时其 `ConcurrentAppend` 无 Skip 直接绿。
- 依赖方向：F1 不依赖 E1；E1 的并发测试**软依赖** F1（无 F1 则 Skip）。

---

## 风险（F1 专属，补充 spec §11）

| 风险 | 缓解（落点） |
|---|---|
| 放开池后，**事务内通过池读**（`s.DB.Query` 而非 `tx.Query`）读到不同快照，破坏 `queryScoped` 不变量 | `queryScoped`/`queryRowScoped` 签名不变；新增写统一经 `WriteTx`（fn 内读在 `tx` 上）；T6 不碰 VCS 读路径；注释明令禁止事务内池读 |
| modernc 新连接 `busy_timeout`/`synchronous` 未生效（最隐蔽：新连接 timeout=0） | T2 `TestOpen_PragmasOnEveryPoolConn` 对池里每条连接回显 `busy_timeout>0`；DSN 语法经 v1.53.0 源码核定（§"modernc DSN 核定"）；退路 Connector 仅在 DSN 受阻时启用 |
| `WriteTx` 不可重入 → store/work/vcs 写方法互相调用死锁 | 约定章 + 各 Task 注释：写方法不互调；组合多步在**最外层**一次 WriteTx；VCS `writeCommitInTx(tx)` 收 tx 不自建 WriteTx |
| `AppendMessage` 升级单事务后行为变化（原子性增强，非破坏） | T3 测试断言 `updated_at` 被刷新；全 store 包测试绿 |
| Windows 下 `-wal`/`-shm` 句柄占用，库文件无法删 / `doctor` 误报 | 测试 `t.Cleanup` 显式 `Close`；`doctor` 检测前提示停后端；`Close` 的 TRUNCATE 失败仅告警（T11） |
| D3 与 F1 同改 `internal/store` 落点冲突 | T2/T4/T13 三处 D3 re-verify 门禁；F1 分支 rebase 到 D3 最终态 |
| `:memory:` 多连接串味 | T2 `Open(":memory:")` 强制 `MaxOpenConns(1)`；T8 `TestInMemoryForcedSingleConn` 守护 |
| auth CLI（独立进程）的 `store.Open` 未带 busy_timeout → 跨进程仍 BUSY | T4 把 `cmd/yanshi/main.go:290` 的 `Open` 改 `OpenWith(opts)`，DSN busy_timeout 对子进程连接生效 |

---

## 验收标准映射（spec §12）

| spec 验收 | 落点 Task / 测试 |
|---|---|
| 1. `journal_mode=wal`/`synchronous=normal`/`busy_timeout>0`（每条池连接） | T2 `TestOpen_AppliesWALPragmas` + `TestOpen_PragmasOnEveryPoolConn` |
| 2. `MaxOpenConns` 按 `wal_max_open_conns`（默认 4）；`:memory:` 强制 1 | T2 `TestOpen_MemoryForcesSingleConn`；T1 config 默认 |
| 3. 16×50 并发 `AppendMessage` 零 BUSY、行数精确 | T8 `TestConcurrentAppend_NoBusy` |
| 4. 并发读不阻塞写（busy_timeout 内返回） | T8 `TestConcurrentReadWrite_NoBlocking` |
| 5. 双 `Open` 同库写冲突在 busy_timeout 内成功 | T10 `TestCrossProcessBusyTimeout_DualOpen` |
| 6. rollback→WAL 自动升级、零丢失、幂等 | T9 `TestWALUpgrade*` |
| 7. `Close` 执行 `wal_checkpoint(TRUNCATE)`，`-wal` 收缩 | T11 `TestClose_CheckpointsWAL` |
| 8. work/vcs/auth/bootstrap 现有测试全绿 | T3/T4/T5/T6/T7 + T13 全量 |
| 9. Windows CI 并发/升级测试全绿 | T13 `-race` 全量 |
| 10. doctor 报 `journal_mode` + `-wal`/`-shm` 大小 | T12 |

---

## 预估

- T1–T2（config + 原语）：≈1d
- T3–T6（四消费者接入 WriteTx）：≈1.5d
- T7（bootstrap 装配）：≈0.5d
- T8–T10（并发/升级/跨进程测试）：≈2d
- T11–T12（checkpoint + doctor）：≈0.5d
- T13（终验 + D3/E1 衔接）：≈0.5d
- **合计 ≈ 6d**（与 spec §15 的 6-8d 一致）
