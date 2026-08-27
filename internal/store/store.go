// Package store provides SQLite-backed persistence.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/x6nux/yanshi/internal/secrets"
	_ "modernc.org/sqlite"
)

// sqlOpener is a test-visible indirection over sql.Open that allows tests
// to inject connection failures. Production behavior is identical.
var sqlOpener = sql.Open

// Store wraps a SQLite connection with applied migrations.
type Store struct {
	DB *sql.DB

	// inMemory is true when the store was opened from ":memory:".
	// WAL PRAGMAs are skipped for in-memory databases (modernc/sqlite
	// on macOS/Windows may not handle journal_mode=WAL on :memory:
	// correctly, causing silent transaction failures).
	inMemory bool

	// writeMu serializes WAL writes inside a single process so concurrent
	// goroutines never hit SQLITE_BUSY. Consumers coordinate through WriteTx.
	// Cross-process conflicts (auth CLI subprocess) are handled by DSN busy_timeout.
	writeMu sync.Mutex
	// redactor is the process-wide secret redactor injected by bootstrap
	// after all provider secrets have been registered. CreateSession,
	// AppendMessage and UpdateSessionTitle all call redact() on the
	// user-supplied text so secret substrings never reach SQLite. nil means
	// no redaction (unit tests that don't care about D3 S10 still work).
	redactor *secrets.Redactor
}

// SetRedactor injects the process-wide redactor. Called by bootstrap after
// all provider secrets have been registered. Safe to call once at startup;
// concurrent reads after injection are guarded by the Redactor's internal
// RWMutex, so a subsequent SetRedactor is also safe but not expected.
func (s *Store) SetRedactor(r *secrets.Redactor) { s.redactor = r }

// redact returns a redacted copy of text. Returns text unchanged when no
// redactor has been injected, preserving the legacy behaviour expected by
// pre-D3 tests.
func (s *Store) redact(text string) string {
	if s.redactor == nil {
		return text
	}
	return s.redactor.Redact(text)
}

// Open opens (or creates) the SQLite database at path and runs migrations.
// Use ":memory:" for an ephemeral in-memory database (for tests).
func Open(path string) (*Store, error) {
	return OpenWith(path, DefaultOptions)
}

// Close closes the underlying SQLite database. It takes the write lock and
// runs PRAGMA wal_checkpoint(TRUNCATE) first. Checkpoint failure is non-fatal
// (Windows may hold lingering read connections); the database is closed
// regardless.
//
// The explicit checkpoint is DEFENSIVE, not the mechanism that bounds the WAL.
// Measured: replacing it with a no-op leaves the -wal file gone after Close all
// the same, because SQLite checkpoints and deletes the WAL itself when the last
// connection to a database closes cleanly. What this line buys is the case
// where that does not happen — an unclean shutdown leaves the WAL for the next
// opener, and TRUNCATE here makes the common path deterministic rather than
// dependent on how the process ended. It is kept for that reason, not because
// removing it was observed to break anything.
func (s *Store) Close() error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	// TRUNCATE the WAL on close so -wal doesn't balloon on long-running
	// instances. Failure is non-fatal — Windows may hold read connections.
	// Skip for in-memory databases (no WAL file to checkpoint).
	if !s.inMemory {
		_, _ = s.DB.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	}
	return s.DB.Close()
}

// OpenOptions configures the WAL connection pool. The zero value is replaced by
// DefaultOptions in OpenWith.
type OpenOptions struct {
	MaxOpenConns      int // read pool size (0 = default 4; :memory: forced 1)
	BusyTimeoutMs     int // busy_timeout in ms (0 = default 5000)
	WALAutoCheckpoint int // wal_autocheckpoint pages (0 = default 1000; negative = disable)
}

// DefaultOptions is the fallback for zero OpenOptions fields.
var DefaultOptions = OpenOptions{
	MaxOpenConns:      4,
	BusyTimeoutMs:     5000,
	WALAutoCheckpoint: 1000,
}

// OpenWith opens the database at path with the given pool and WAL pragma options.
//
// An empty path is an error, not a default. buildDSN appends the _pragma query
// string unconditionally, so path == "" produces the DSN "?_pragma=..." and
// modernc creates a database file *named that literal string* in the current
// working directory — silently, with no error to notice. storage.sqlite_path
// has no config default, so an omitted key reaches every caller here.
func OpenWith(path string, opts OpenOptions) (*Store, error) {
	if path == "" {
		return nil, errors.New("store: empty database path")
	}
	maxOpen := opts.MaxOpenConns
	if maxOpen <= 0 {
		maxOpen = DefaultOptions.MaxOpenConns
	}
	busyMs := opts.BusyTimeoutMs
	if busyMs <= 0 {
		busyMs = DefaultOptions.BusyTimeoutMs
	}
	ckpt := opts.WALAutoCheckpoint
	if ckpt == 0 {
		ckpt = DefaultOptions.WALAutoCheckpoint
	}
	dsn := buildDSN(path, busyMs, ckpt)
	db, err := sqlOpener("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// :memory: forces single connection — multiple connections see independent dbs.
	if path == ":memory:" {
		maxOpen = 1
	}
	db.SetMaxOpenConns(maxOpen)
	if maxOpen > 1 {
		db.SetConnMaxIdleTime(5 * time.Minute)
	}
	s := &Store{DB: db, inMemory: path == ":memory:"}
	if err := s.applyConnectionPragmas(); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// buildDSN appends the _pragma query string for per-connection PRAGMAs (modernc
// DSN format, v1.53.0+). path must not contain '?'.
//
// foreign_keys(ON) is here rather than in applyConnectionPragmas because SQLite
// makes it a PER-CONNECTION setting that defaults to OFF: a single Exec would
// arm one pooled connection and leave the other three unenforced, which is
// worse than off (referential bugs would surface nondeterministically depending
// on which connection served the write). It is not merely defensive — turning
// it on surfaced a real ordering bug in vcs.initNewRepoLocked, which wrote the
// initial commit before the vcs_repos row it points at.
//
// :memory: databases skip the DSN pragmas because modernc may not recognize
// :memory:?_pragma=... as in-memory on all platforms (macOS/Windows would
// create a file-backed database instead), and the single-connection forced by
// :memory: makes the WAL/timeout pragmas redundant anyway. foreign_keys is the
// exception — it changes behaviour rather than performance, so
// applyConnectionPragmas re-arms it with an Exec for that one case.
func buildDSN(path string, busyMs, autoCkpt int) string {
	if path == ":memory:" {
		return path
	}
	return path + "?_pragma=busy_timeout(" + strconv.Itoa(busyMs) + ")" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=wal_autocheckpoint(" + strconv.Itoa(autoCkpt) + ")" +
		"&_pragma=foreign_keys(ON)"
}

// applyConnectionPragmas sets the persistent journal_mode=WAL. This runs once
// per Store (on the first connection) and is idempotent. The per-connection
// PRAGMAs (synchronous, busy_timeout, wal_autocheckpoint, foreign_keys) are
// handled by DSN _pragma, not here — with the single :memory: exception below,
// which the DSN cannot reach.
func (s *Store) applyConnectionPragmas() error {
	if s.inMemory {
		// WAL is not meaningful for :memory:, but foreign_keys is: buildDSN
		// returns ":memory:" unchanged, so the DSN _pragma list never reaches
		// an in-memory database. :memory: forces MaxOpenConns=1, so a plain
		// Exec covers every connection there. Without this an in-memory test
		// would run with FK enforcement OFF while production has it ON — the
		// referential bugs this pragma exists to catch would be invisible to
		// exactly the cheapest tests, which is most of this repo's suite.
		_, err := s.DB.Exec("PRAGMA foreign_keys=ON")
		return err
	}
	if _, err := s.DB.Exec("PRAGMA journal_mode=WAL"); err != nil {
		return fmt.Errorf("store: set WAL: %w", err)
	}
	return nil
}

// WriteTx serializes WAL writes inside one process. It locks writeMu, begins a
// transaction, calls fn, and commits (or rolls back on fn error). fn must NOT
// call any other Store write method (WriteTx is NOT reentrant — nesting would
// deadlock on writeMu).
//
// It is NOT what keeps callers from seeing SQLITE_BUSY — that is busy_timeout,
// set per connection via the DSN. Measured with 16 goroutines × 50 writes:
// removing writeMu alone yields zero BUSY; removing writeMu AND setting
// busy_timeout(0) yields 717 of 800. The predecessor comment claimed the
// opposite causation, which matters because it makes writeMu look like the
// correctness guarantee and busy_timeout look like tuning. What writeMu
// actually buys is that contending goroutines wait on a Go mutex instead of
// spinning inside SQLite's lock retry loop.
func (s *Store) WriteTx(ctx context.Context, fn func(*sql.Tx) error) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (s *Store) migrate() error {
	if _, err := s.DB.Exec(schema); err != nil {
		return err
	}
	if err := s.addColumnIfMissing("tasks", "worktree_id", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.addColumnIfMissing("vcs_worktrees", "tip", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	// vcs_tree.op marks each row as an 'add'/'mod'/'del' delta vs the commit's
	// parent (delta storage). Existing pre-delta rows default to 'add' — correct
	// for a v1 feature with no production full-snapshot data to migrate.
	if err := s.addColumnIfMissing("vcs_tree", "op", "TEXT NOT NULL DEFAULT 'add'"); err != nil {
		return err
	}
	if err := s.addColumnIfMissing("sessions", "model", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.addColumnIfMissing("sessions", "thinking", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.addColumnIfMissing("sessions", "tokens_in", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.addColumnIfMissing("sessions", "tokens_out", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.addColumnIfMissing("sessions", "turns", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	// Task A6: cached_tokens / reasoning_tokens break out the API's prompt-cache
	// hits and reasoning-model spend. Defaults to 0 so old rows — and old SQLite
	// databases created before this migration — surface no savings until the
	// next turn writes a real value.
	if err := s.addColumnIfMissing("sessions", "cached_tokens", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.addColumnIfMissing("sessions", "reasoning_tokens", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	// V10: archived marks a session hidden from the active list but not deleted
	// (soft-hide). Defaults to 0 (active) so pre-existing rows and pre-V10 SQLite
	// databases stay visible until the user explicitly archives.
	if err := s.addColumnIfMissing("sessions", "archived", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	// C4 COST1: billed_* tokens and cost_usd/cost_known store the per-session
	// cumulative billable ledger (separate from the overwrite-semantics
	// tokens_in/tokens_out columns). Defaults are 0 / cost_known=0 so old rows
	// and pre-C4 SQLite databases render "$0.0000 (unknown)" until a real
	// billing event overwrites them. cost_known=0 means "at least one provider
	// usage in this session referenced a model absent from the pricing table";
	// renderers must show "N/A" not "$0".
	if err := s.addColumnIfMissing("sessions", "billed_input_tokens", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.addColumnIfMissing("sessions", "billed_cached_tokens", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.addColumnIfMissing("sessions", "billed_output_tokens", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.addColumnIfMissing("sessions", "cost_usd", "REAL NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.addColumnIfMissing("sessions", "cost_known", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	// C1: the durable conversation log. See migrateMessageLog for why these
	// four columns, the partial unique index and the FTS index cannot live in
	// the `schema` string like everything else.
	if err := s.migrateMessageLog(); err != nil {
		return err
	}
	// C14: memories gain retrieval dimensions. Pre-C14 rows keep '' for both,
	// which is exactly the value a dimension-less search matches, so old
	// memories stay visible to an unfiltered query and invisible to a filtered
	// one — the honest reading of "we do not know which agent wrote this".
	if err := s.addColumnIfMissing("memories", "session_id", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.addColumnIfMissing("memories", "agent_id", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if _, err := s.DB.Exec(
		"CREATE INDEX IF NOT EXISTS idx_memories_dims ON memories(session_id, agent_id, created_at DESC)",
	); err != nil {
		return err
	}
	// C13: memory consolidation lineage. A distillation writes a merged row and
	// marks its inputs superseded — it never deletes — so these three columns
	// are what make the merge auditable and reversible. Pre-C13 rows default to
	// '' / 0, i.e. "not distilled, not superseded", which is the truth about
	// them and leaves every existing memory current.
	if err := s.addColumnIfMissing("memories", "distilled_from", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.addColumnIfMissing("memories", "superseded_by", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.addColumnIfMissing("memories", "distilled_at", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	// Every default read now carries `superseded_by = ''`, which the dimension
	// index above does not cover.
	if _, err := s.DB.Exec(
		"CREATE INDEX IF NOT EXISTS idx_memories_current ON memories(superseded_by, created_at DESC)",
	); err != nil {
		return err
	}
	return nil
}

// messageLogColumns are the C1 additions to `messages`, in the order they are
// applied. Declared as data rather than four statements so migrateMessageLog
// and its test cannot disagree about the set.
var messageLogColumns = []struct{ Col, Decl string }{
	{"tool_call_id", "TEXT NOT NULL DEFAULT ''"},
	{"tool_name", "TEXT NOT NULL DEFAULT ''"},
	{"tool_args", "TEXT NOT NULL DEFAULT ''"},
	{"dedup_key", "TEXT NOT NULL DEFAULT ''"},
}

// migrateMessageLog brings a pre-C1 `messages` table up to the durable-log
// shape: four columns, a partial unique index on the dedup key, and an FTS5
// index over content + tool_args.
//
// It is a Go function rather than more lines in the `schema` string because
// ORDER MATTERS ACROSS OBJECT KINDS and `schema` is one Exec of CREATE-IF-NOT-
// EXISTS statements that all run BEFORE addColumnIfMissing. On an existing
// database the FTS virtual table and its triggers reference tool_args, which
// does not exist yet at that point — and SQLite accepts both anyway (measured:
// creating a content= virtual table and a trigger over a missing column both
// return no error), so the failure would not surface until the first INSERT.
//
// The unique index is PARTIAL:
//
//	WHERE dedup_key <> ''
//
// because every row written by the legacy AppendMessage path carries the empty
// string, including every row in databases that predate the column. A total
// unique index would reject the second such row in any session — i.e. it would
// turn an upgrade into an outage on the next message.
//
// The FTS rebuild is conditional on the index not already existing. `rebuild`
// re-reads the whole table, which is right once on upgrade (so pre-existing
// messages become searchable) and pure cost on every subsequent open.
func (s *Store) migrateMessageLog() error {
	for _, c := range messageLogColumns {
		if err := s.addColumnIfMissing("messages", c.Col, c.Decl); err != nil {
			return fmt.Errorf("store: migrate messages.%s: %w", c.Col, err)
		}
	}
	if _, err := s.DB.Exec(
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_messages_dedup
		   ON messages(session_id, dedup_key) WHERE dedup_key <> ''`,
	); err != nil {
		return fmt.Errorf("store: migrate messages dedup index: %w", err)
	}

	var ftsExists int
	if err := s.DB.QueryRow(
		"SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='messages_fts'",
	).Scan(&ftsExists); err != nil {
		return fmt.Errorf("store: probe messages_fts: %w", err)
	}
	stmts := []string{
		`CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
		     content, tool_args,
		     content='messages', content_rowid='rowid',
		     tokenize='porter unicode61')`,
		`CREATE TRIGGER IF NOT EXISTS messages_ai AFTER INSERT ON messages BEGIN
		     INSERT INTO messages_fts(rowid, content, tool_args)
		     VALUES (new.rowid, new.content, new.tool_args);
		 END`,
		`CREATE TRIGGER IF NOT EXISTS messages_ad AFTER DELETE ON messages BEGIN
		     INSERT INTO messages_fts(messages_fts, rowid, content, tool_args)
		     VALUES('delete', old.rowid, old.content, old.tool_args);
		 END`,
		`CREATE TRIGGER IF NOT EXISTS messages_au AFTER UPDATE ON messages BEGIN
		     INSERT INTO messages_fts(messages_fts, rowid, content, tool_args)
		     VALUES('delete', old.rowid, old.content, old.tool_args);
		     INSERT INTO messages_fts(rowid, content, tool_args)
		     VALUES (new.rowid, new.content, new.tool_args);
		 END`,
	}
	for _, q := range stmts {
		if _, err := s.DB.Exec(q); err != nil {
			return fmt.Errorf("store: migrate messages fts: %w", err)
		}
	}
	if ftsExists == 0 {
		if _, err := s.DB.Exec("INSERT INTO messages_fts(messages_fts) VALUES('rebuild')"); err != nil {
			return fmt.Errorf("store: rebuild messages fts: %w", err)
		}
	}
	return nil
}

func (s *Store) addColumnIfMissing(table, col, decl string) error {
	cols, err := s.columns(table)
	if err != nil {
		return err
	}
	for _, c := range cols {
		if c == col {
			return nil
		}
	}
	_, err = s.DB.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, col, decl))
	return err
}

func (s *Store) columns(table string) ([]string, error) {
	rows, err := s.DB.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

const schema = `
CREATE TABLE IF NOT EXISTS kv (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS sessions (
    id                   TEXT PRIMARY KEY,
    title                TEXT NOT NULL DEFAULT '',
    created_at           INTEGER NOT NULL,
    updated_at           INTEGER NOT NULL,
    model                TEXT NOT NULL DEFAULT '',
    thinking             TEXT NOT NULL DEFAULT '',
    tokens_in            INTEGER NOT NULL DEFAULT 0,
    tokens_out           INTEGER NOT NULL DEFAULT 0,
    turns                INTEGER NOT NULL DEFAULT 0,
    cached_tokens        INTEGER NOT NULL DEFAULT 0,
    reasoning_tokens     INTEGER NOT NULL DEFAULT 0,
    archived             INTEGER NOT NULL DEFAULT 0,
    billed_input_tokens  INTEGER NOT NULL DEFAULT 0,
    billed_cached_tokens INTEGER NOT NULL DEFAULT 0,
    billed_output_tokens INTEGER NOT NULL DEFAULT 0,
    cost_usd             REAL    NOT NULL DEFAULT 0,
    cost_known           INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS messages (
    id           TEXT PRIMARY KEY,
    session_id   TEXT NOT NULL,
    seq          INTEGER NOT NULL,
    role         TEXT NOT NULL,
    content      TEXT NOT NULL,
    tool_call_id TEXT NOT NULL DEFAULT '',
    tool_name    TEXT NOT NULL DEFAULT '',
    tool_args    TEXT NOT NULL DEFAULT '',
    dedup_key    TEXT NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL,
    FOREIGN KEY (session_id) REFERENCES sessions(id)
);
CREATE INDEX IF NOT EXISTS idx_messages_session ON messages(session_id, seq);

-- S6: permission decisions. The records auditPermission already built existed
-- only as stderr log lines, so "who approved that rm last night" was
-- unanswerable the moment the terminal scrolled. cmd_digest is a REDACTED
-- summary, never the raw command — see store.AppendPermissionAudit.
CREATE TABLE IF NOT EXISTS permission_audit (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    ts          INTEGER NOT NULL,
    session_id  TEXT NOT NULL DEFAULT '',
    agent_id    TEXT NOT NULL DEFAULT '',
    tool        TEXT NOT NULL,
    decision    TEXT NOT NULL,
    source      TEXT NOT NULL DEFAULT '',
    reason_code TEXT NOT NULL DEFAULT '',
    cmd_digest  TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_permission_audit_ts ON permission_audit(ts DESC);
CREATE INDEX IF NOT EXISTS idx_permission_audit_session ON permission_audit(session_id, ts DESC);

-- M10: per-call token usage. The sessions table's token columns are a running
-- total overwritten every turn, so "which model burned what on which day" was
-- destroyed by the same write that recorded it. This ledger is append-only, so
-- any grouping can be recomputed later instead of being fixed at write time.
-- See store.AppendUsage / store.AggregateUsage.
CREATE TABLE IF NOT EXISTS usage_log (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    ts                INTEGER NOT NULL,
    provider          TEXT NOT NULL DEFAULT '',
    model             TEXT NOT NULL,
    session_id        TEXT NOT NULL DEFAULT '',
    prompt_tokens     INTEGER NOT NULL DEFAULT 0,
    completion_tokens INTEGER NOT NULL DEFAULT 0,
    cached_tokens     INTEGER NOT NULL DEFAULT 0,
    reasoning_tokens  INTEGER NOT NULL DEFAULT 0,
    cache_hit         INTEGER NOT NULL DEFAULT 0
);
-- The (ts, model) index serves the aggregate query, which always scans a time
-- range and groups by model; the session index serves per-conversation lookup.
CREATE INDEX IF NOT EXISTS idx_usage_log_ts_model ON usage_log(ts DESC, model);
CREATE INDEX IF NOT EXISTS idx_usage_log_session ON usage_log(session_id, ts DESC);

CREATE TABLE IF NOT EXISTS auth_metadata (
    provider   TEXT NOT NULL,
    account    TEXT NOT NULL,
    source     TEXT NOT NULL,
    expires_at INTEGER NOT NULL DEFAULT 0,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (provider, account)
);

CREATE TABLE IF NOT EXISTS memories (
    id         TEXT PRIMARY KEY,
    kind       TEXT NOT NULL DEFAULT 'note',
    content    TEXT NOT NULL,
    created_at INTEGER NOT NULL
);

CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts USING fts5(
    content,
    content='memories',
    content_rowid='rowid'
);
-- keep fts in sync with the base table
CREATE TRIGGER IF NOT EXISTS memories_ai AFTER INSERT ON memories BEGIN
    INSERT INTO memories_fts(rowid, content) VALUES (new.rowid, new.content);
END;
CREATE TRIGGER IF NOT EXISTS memories_ad AFTER DELETE ON memories BEGIN
    INSERT INTO memories_fts(memories_fts, rowid, content) VALUES('delete', old.rowid, old.content);
END;
CREATE TRIGGER IF NOT EXISTS memories_au AFTER UPDATE ON memories BEGIN
    INSERT INTO memories_fts(memories_fts, rowid, content) VALUES('delete', old.rowid, old.content);
    INSERT INTO memories_fts(rowid, content) VALUES (new.rowid, new.content);
END;

CREATE TABLE IF NOT EXISTS tasks (
    id           TEXT PRIMARY KEY,
    type         TEXT NOT NULL,
    input        TEXT NOT NULL,
    status       TEXT NOT NULL,
    assigned_to  TEXT NOT NULL DEFAULT '',
    result       TEXT NOT NULL DEFAULT '',
    parent_task  TEXT NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL,
    deadline     INTEGER NOT NULL DEFAULT 0,
    attempts     INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_tasks_status ON tasks(status);

CREATE TABLE IF NOT EXISTS vcs_repos (
    id         TEXT PRIMARY KEY,
    root_path  TEXT NOT NULL,
    main_head  TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS vcs_worktrees (
    id          TEXT PRIMARY KEY,
    repo_id     TEXT NOT NULL,
    path        TEXT NOT NULL,
    base_commit TEXT NOT NULL,
    created_at  INTEGER NOT NULL,
    active      INTEGER NOT NULL DEFAULT 1,
    tip         TEXT NOT NULL DEFAULT '',
    FOREIGN KEY (repo_id) REFERENCES vcs_repos(id)
);
CREATE INDEX IF NOT EXISTS idx_vcs_worktrees_repo ON vcs_worktrees(repo_id);
CREATE TABLE IF NOT EXISTS vcs_commits (
    id           TEXT PRIMARY KEY,
    repo_id      TEXT NOT NULL,
    worktree_id  TEXT NOT NULL DEFAULT '',
    parent_id    TEXT NOT NULL DEFAULT '',
    merged_from  TEXT NOT NULL DEFAULT '',
    author       TEXT NOT NULL,
    message      TEXT NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL,
    FOREIGN KEY (repo_id) REFERENCES vcs_repos(id)
);
CREATE INDEX IF NOT EXISTS idx_vcs_commits_parent ON vcs_commits(parent_id);
CREATE INDEX IF NOT EXISTS idx_vcs_commits_worktree ON vcs_commits(worktree_id, created_at);
CREATE TABLE IF NOT EXISTS vcs_tree (
    commit_id  TEXT NOT NULL,
    path       TEXT NOT NULL,
    blob_hash  TEXT NOT NULL,
    op         TEXT NOT NULL DEFAULT 'add',
    PRIMARY KEY (commit_id, path)
);
CREATE TABLE IF NOT EXISTS vcs_blobs (
    hash    TEXT PRIMARY KEY,
    content BLOB NOT NULL,
    size    INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS vcs_uncommitted (
    scope_type TEXT NOT NULL,
    scope_id   TEXT NOT NULL,
    path       TEXT NOT NULL,
    blob_hash  TEXT NOT NULL,
    op         TEXT NOT NULL,
    PRIMARY KEY (scope_type, scope_id, path)
);
-- vcs_seams 存"逐轮 seam 快照"——每条行只是对 vcs_commits.id 的命名指针
-- (commit 本身已内容寻址 + 增量 delta 存储,所以 seam 开销 = 1 行)。
-- kind ∈ {"pre-turn","post-turn","pre-revert","post-revert"};
-- turn_seq = seal 时刻的 cs.turns(pre-turn 在 ++ 前;post-turn 在 ++ 后);
-- history_len = 目标 seam 的 len(cs.history),用于回滚时截断;
-- prev_turn_seq / prev_history_len 仅对 pre-revert(undo) seam 有意义:
-- 它们记录执行回滚前的 cs.turns / len(cs.history)。history_snapshot 只在
-- pre-revert seam 上保存同一边界的 durable session snapshot(JSON BLOB),使再次
-- 回滚 undo seam 能恢复被第一次截断删除的消息,而不只是恢复整数计数(D2)。
-- 其他 seam 的 history_snapshot 保持空 blob。
-- session_id 让 seam 在多连接共享 repo 时按 session 分组(必修项 J)。
-- seq 是单调自增主键,是 seam 列表的 SOLE 排序键(必修项 H)——
-- created_at 仅作展示(秒级精度不足以稳定排序同秒插入)。
CREATE TABLE IF NOT EXISTS vcs_seams (
    seq              INTEGER PRIMARY KEY AUTOINCREMENT,
    id               TEXT NOT NULL UNIQUE,
    repo_id          TEXT NOT NULL,
    session_id       TEXT NOT NULL DEFAULT '',
    commit_id        TEXT NOT NULL,
    turn_seq         INTEGER NOT NULL DEFAULT 0,
    history_len      INTEGER NOT NULL DEFAULT 0,
    prev_turn_seq    INTEGER NOT NULL DEFAULT 0,
    prev_history_len INTEGER NOT NULL DEFAULT 0,
    history_snapshot BLOB NOT NULL DEFAULT X'',
    kind             TEXT NOT NULL,
    label            TEXT NOT NULL DEFAULT '',
    created_at       INTEGER NOT NULL,
    FOREIGN KEY (repo_id) REFERENCES vcs_repos(id),
    FOREIGN KEY (commit_id) REFERENCES vcs_commits(id)
);
CREATE INDEX IF NOT EXISTS idx_vcs_seams_repo_seq ON vcs_seams(repo_id, seq DESC);
CREATE INDEX IF NOT EXISTS idx_vcs_seams_session ON vcs_seams(repo_id, session_id, seq DESC);
`
