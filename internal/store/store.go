// Package store provides SQLite-backed persistence.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/x6nux/yanshi/internal/secrets"
	sqlite "modernc.org/sqlite"
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

	// SelfHeal allows OpenWith to move an UNREADABLE database aside and start
	// an empty one rather than refusing to open. It defaults to FALSE, and the
	// default is the safety property: healing renames the user's data, which
	// only the process that owns the database has any business doing.
	//
	// Most openers are not owners. `yanshi doctor` is a READ-ONLY diagnostic —
	// with healing on it would quarantine the database it was asked to inspect
	// and then report StatusOK, so a user running "check whether anything is
	// wrong" would have their history moved away and be told everything is
	// fine. `yanshi vcs-mcp` is worse: bootstrap spawns one per ACP agent, so
	// several processes would race to quarantine the same file. The goal and
	// auth subcommands are likewise incidental readers. All of these go
	// through Open, which uses DefaultOptions, so they get the safe default
	// without opting into anything.
	//
	// bootstrap.Build FORWARDS this from bootstrap.Options.SelfHeal rather than
	// setting it — it has six callers and only two of them own the database.
	// The authoritative list of entries allowed to turn it on, with a reason
	// per entry, is bootstrap.selfHealAllowedSites, which an AST test keeps in
	// sync with the code.
	SelfHeal bool
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
//
// When opts.SelfHeal is set, an UNREADABLE database file self-heals instead of
// refusing to start: the file is moved aside and an empty one takes its place.
// yanshi ships as one local binary, so a corrupt yanshi.db otherwise means the
// TUI never comes up and the user has no second tool to repair it with — losing
// history is bad, losing the program that could have told you the history was
// lost is worse. The old file is renamed, never deleted, and the backup path is
// logged so the data is still there for anyone who wants to salvage it. See
// isCorruptDB for why the trigger is narrow, and OpenOptions.SelfHeal for why
// it is off unless the caller owns the database.
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
	s, err := openPrepared(path, maxOpen, busyMs, ckpt)
	if err == nil || !opts.SelfHeal || !isCorruptDB(err) {
		return s, err
	}
	return healCorrupt(path, maxOpen, busyMs, ckpt, err)
}

// healWaitTimeout bounds how long an opener waits for ANOTHER process to
// finish healing before giving up and reporting the corruption.
const healWaitTimeout = 5 * time.Second

// healRecheckAttempts and healRecheckBackoff bound the recheck-under-lock
// retry budget: how long a healer tolerates CONTENTION recheck failures before
// reporting the last one instead of quarantining. The contention this waits
// out is the sibling openers' migrate() storm on the freshly repaired
// database, which drains in single-digit milliseconds; 25 attempts at 20ms is
// ~500ms of slack against a CI runner under load, still well inside the
// healWaitTimeout a queued peer is itself prepared to spend. Attempts stop
// early the moment the recheck returns anything that is not a contention code
// — corruption-classified and every other non-transient failure takes the
// quarantine branch immediately, so the garbage-database case and the
// rebuild-failed case pay for none of this budget.
const (
	healRecheckAttempts = 25
	healRecheckBackoff  = 20 * time.Millisecond
)

// healLockTTL bounds how long a heal lock is honoured. Healing is one rename
// plus one round of CREATE TABLE — milliseconds — so a lock older than this
// belongs to a process that died holding it. Never healing again is a worse
// outcome than proceeding, so a lock that stale is taken over.
const healLockTTL = time.Minute

// healCorrupt moves the unreadable database at path aside and opens a fresh one
// in its place. openErr is the corruption that triggered healing and is what
// comes back if healing does not work out.
//
// This function only ACQUIRES the lock; the repair itself is healUnderLock, so
// that every statement which touches the database sits inside the critical
// section. A caller that loses the race waits here and then runs the same
// repair, which by then finds a healthy database and simply adopts it.
//
// Healing is EXCLUSIVE ACROSS PROCESSES, and that is a data-safety property,
// not tidiness. Several yanshi processes routinely hold one project database at
// once: the TUI's own election in cli.bootstrapOwner Builds BEFORE it claims the
// lockfile, so two windows starting together both build, and `yanshi serve` can
// be running beside either. Measured without the lock: the second healer renames
// the database the first one had just repaired, the first keeps writing to an
// orphaned inode, and the project is left with three files and an empty store.
// Quarantining is destructive precisely because it is a rename, so it has to
// happen at most once — hence the lock, and hence the recheck under it.
func healCorrupt(path string, maxOpen, busyMs, autoCkpt int, openErr error) (*Store, error) {
	deadline := time.Now().Add(healWaitTimeout)
	for {
		unlock, lockErr := acquireHealLock(path)
		if lockErr == nil {
			healed, err := healUnderLock(path, maxOpen, busyMs, autoCkpt, openErr)
			unlock()
			return healed, err
		}
		if !errors.Is(lockErr, fs.ErrExist) {
			// The lock could not be CREATED — a read-only directory, EROFS, a
			// full disk. Healing needs to rename and recreate inside this same
			// directory, so it cannot possibly succeed; waiting would only add
			// healWaitTimeout to a startup that is going to fail anyway
			// (measured: 5.02s of it) before reporting the same error.
			return nil, openErr
		}
		if time.Now().After(deadline) {
			// A peer has held the lock for the whole timeout. Degrade to the
			// pre-healing behaviour — report the corruption — rather than hang.
			return nil, openErr
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// healUnderLock performs the repair with the heal lock held. Everything that
// TOUCHES the database lives here, and that containment is the invariant the
// whole design rests on: openPrepared is not a passive look, because SQLite
// creates the file when it is missing and takes an exclusive lock on the WAL.
//
// Two distinct races were measured before the rule was made absolute. A waiter
// polling openPrepared during the winner's rename→create gap builds its OWN
// database at the path the winner is about to write, and the winner's PRAGMA
// journal_mode=WAL then fails with SQLITE_BUSY (13 runs in 60) — so the user is
// told the database is corrupt just after it was successfully repaired (0.5%
// with two windows). And once several waiters were released together they ran
// migrate() concurrently on the same fresh file, where addColumnIfMissing is a
// check-then-ALTER that is not atomic across connections: "duplicate column
// name: agent_id", "disk I/O error", "unable to open database file", 3 failures
// in 200. Serialising every open behind the lock removes both.
func healUnderLock(path string, maxOpen, busyMs, autoCkpt int, openErr error) (*Store, error) {
	// Recheck under the lock. The holder we queued behind may have finished
	// between our failed open and our acquiring the lock, in which case the
	// file at path is now a healthy database and renaming it away would undo
	// their repair.
	//
	// The recheck's ERROR CLASS decides, not its mere existence, and the
	// decision is three-way:
	//
	//   - nil → the holder we queued behind already repaired the file; adopt it.
	//   - isTransientOpenErr (SQLITE_BUSY, SQLITE_IOERR_DELETE_NOENT — the two
	//     codes measured coming out of concurrent first opens, and the same set
	//     applyConnectionPragmas retries internally) → the sibling storm is
	//     still draining; retry on the expectation that it drains in
	//     milliseconds, and report the error as-is if the budget runs out.
	//     These codes say "somebody else is using it", which is no evidence
	//     about the file, and quarantining on that evidence renames a HEALTHY
	//     database: measured on the six-way open this package's concurrent
	//     healer test runs, that destroyed the repaired database, turned the
	//     -wal/-shm pair over under live connections, and ended in SIGBUS
	//     inside walIndexAppend — the data loss this whole apparatus exists to
	//     prevent, committed by the repairer.
	//   - anything else — corruption-classified (the file is still garbage, the
	//     case the first holder runs into) and every non-contention failure
	//     alike — takes the pre-existing branch below: the quarantine, and the
	//     rebuild whose failure reports the ORIGINAL corruption error wrapped.
	//     internal/store::TestOpenWith_ReturnsTheOriginalErrorWhenTheRebuildFails
	//     pins that reporting contract; routing a non-transient recheck error
	//     into the retry instead would strand the caller with an error that
	//     says nothing about the corruption that started the heal.
	var transientErr error
	for attempt := 0; ; attempt++ {
		healthy, err := openPrepared(path, maxOpen, busyMs, autoCkpt)
		if err == nil {
			return healthy, nil
		}
		if !isTransientOpenErr(err) {
			break
		}
		transientErr = err
		if attempt >= healRecheckAttempts-1 {
			break
		}
		time.Sleep(healRecheckBackoff)
	}
	if transientErr != nil {
		return nil, fmt.Errorf("store: %s was still contended (%v) after %d rechecks; not quarantining a database nothing has proven unreadable",
			path, transientErr, healRecheckAttempts)
	}

	// A failed quarantine is NOT fatal on its own, and the early return that
	// used to be here was wrong: if the file has already been moved (a racing
	// healer got there first) the rename fails with ENOENT while the reopen
	// below SUCCEEDS, because SQLite simply creates a new database at the now
	// empty path. Bailing out on mvErr turned that recoverable state into a
	// refusal to start. Only the reopen decides.
	//
	// With the recheck above in place this is defence in depth, not a live
	// path, and no test distinguishes it — measured, restoring the early return
	// leaves the suite green. It stays because "the rename failed" is not
	// evidence about whether the database can be opened, and only the thing
	// that answers that question should be allowed to fail the boot.
	backup, mvErr := quarantineCorrupt(path)
	healed, healErr := openPrepared(path, maxOpen, busyMs, autoCkpt)
	if healErr != nil {
		// Recovery failed too, so hand back nothing: a Store that cannot be
		// written to would push the same error out to whichever feature
		// happens to touch storage first, far from the reason.
		//
		// BOTH errors go in the message. The original stays wrapped, because
		// it is what is wrong with the database and callers test for it with
		// isCorruptDB; the rebuild error is included as text because
		// "unreadable" and "unreadable AND the rebuild hit a locked WAL" are
		// very different situations for whoever is reading the log, and
		// reporting only the first made the second indistinguishable from a
		// database nobody had touched.
		return nil, fmt.Errorf("store: %s was unreadable (%w) and could not be rebuilt: %v",
			path, openErr, healErr)
	}
	if mvErr != nil {
		slog.Warn("store: database was unreadable and could not be moved aside; started an empty one",
			"path", path, "err", openErr, "move_err", mvErr)
		return healed, nil
	}
	slog.Warn("store: database was unreadable; moved it aside and started an empty one",
		"path", path, "backup", backup, "err", openErr)
	return healed, nil
}

// acquireHealLock takes a cross-process exclusive lock on healing path, using
// O_EXCL create as the mutex — the portable one, since this repo targets
// Windows too.
//
// A nil error means the lock was won and unlock must be called. fs.ErrExist
// means somebody else holds it and waiting is worthwhile; ANY OTHER error means
// the lock could not be created at all and healing here is impossible, which
// the caller must not confuse with contention. Returning a bare false for both
// made a read-only directory wait out the full healWaitTimeout before failing.
//
// LIMIT: O_EXCL create is atomic on local filesystems, which is what yanshi
// targets (one binary, one project directory). On NFSv2 it is not, and on some
// network filesystems it is only advisory — a sqlite_path on a network share
// can therefore still admit two healers. Nothing here detects that.
func acquireHealLock(path string) (unlock func(), err error) {
	lock := path + ".healing"
	// Two attempts, not a retry loop: the second exists only to take over a
	// lock abandoned by a dead process, and looping past that would let two
	// processes steal from each other indefinitely.
	for range 2 {
		f, openErr := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if openErr == nil {
			_ = f.Close()
			return func() { _ = os.Remove(lock) }, nil
		}
		if !errors.Is(openErr, fs.ErrExist) {
			return nil, openErr
		}
		fi, statErr := os.Stat(lock)
		if statErr != nil || time.Since(fi.ModTime()) <= healLockTTL {
			return nil, openErr
		}
		// Abandoned by a process that died holding it. Between this Remove and
		// the next O_EXCL create another reclaimer can slip in; it loses the
		// create and is told fs.ErrExist, which is the correct answer for it.
		_ = os.Remove(lock)
	}
	return nil, fs.ErrExist
}

// openPrepared opens path, sizes the pool, applies the pragmas and runs the
// migrations. It closes the handle and returns a nil Store on every failure,
// which is what lets OpenWith rename the file afterwards: Windows refuses to
// rename a file that is still open.
func openPrepared(path string, maxOpen, busyMs, autoCkpt int) (*Store, error) {
	db, err := sqlOpener("sqlite", buildDSN(path, busyMs, autoCkpt))
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

// SQLite result codes meaning "this file is not a usable database".
const (
	sqliteCorrupt = 11 // SQLITE_CORRUPT — disk image is malformed
	sqliteNotADB  = 26 // SQLITE_NOTADB — header is not SQLite's
)

// isCorruptDB reports whether err says the file itself is unreadable, as
// opposed to something about this attempt to use it having gone wrong.
//
// The predicate is this narrow ON PURPOSE, because the caller's response is to
// rename the user's data away. A read-only file, a full disk, a permissions
// problem or an outright bug in migrate() all produce errors that a rebuild
// would appear to "fix" — by discarding a database that was fine. Widening
// this turns every transient storage fault into data loss.
// internal/store::TestOpenWith_DoesNotQuarantineARecoverableFailure is the one
// test that goes red if it widens — verified by widening it and reading the
// whole result list, not by picking a plausible name. It breaks migrate() on a
// perfectly good database with a healing opener and requires OpenWith to leave
// the file alone.
//
// This comment named TestOpenWith_MigrateFailsOnReadOnlyDB for three rounds and
// was wrong every time: that test opens with store.Open, so healing is off and
// the predicate is never consulted — it passes with the predicate fully open.
// Naming the wrong guard is not a harmless slip, because it is the only thing
// telling the next editor that widening this has a cost, and GOV9 cannot catch
// it (the symbol it names does exist).
//
// Detection REACTS to the failed open rather than probing with PRAGMA
// integrity_check. integrity_check is strictly more sensitive — measured, a
// real database whose tail has been overwritten with garbage opens, migrates
// and serves reads without complaint, and integrity_check would flag it — but
// it reads every page on every startup, and throwing away a database that
// still answers every query is a worse outcome than leaving it alone.
func isCorruptDB(err error) bool {
	var se *sqlite.Error
	if !errors.As(err, &se) {
		return false
	}
	return se.Code() == sqliteCorrupt || se.Code() == sqliteNotADB
}

// quarantineCorrupt renames the unreadable database out of the way and returns
// the backup path.
//
// The WAL and SHM sidecars move with it, but do NOT read that as "the backup is
// therefore a complete database". On every path that actually reaches here the
// backup is the main file alone, because SQLite has already destroyed the
// sidecars — measured across all three corruption shapes that trigger healing
// (garbage from byte zero, a bogus header page size, a smashed schema page):
// the open that fails reads -wal, rejects it, and deletes both before this
// function runs. The converse closes the other half: a database with a VALID
// un-checkpointed WAL opens successfully and recovers its 3 MB of pending
// writes, so it never reaches healing at all. There is no reachable case in
// which a user's newest writes survive in a sidecar to be preserved here.
//
// So the loop is DEAD CODE on this platform, kept deliberately and cheaply. It
// is retained because the measurement is platform-specific — this repo is
// developed on Windows, where deleting a file with an open handle behaves
// differently, so a sidecar may well outlive the failed open there — and
// because leaving a stale -wal beside the replacement database is the one
// outcome worth four lines to rule out. It is NOT needed to protect the new
// database: measured, a foreign -wal beside a freshly created database is
// ignored (the salt does not match) and nothing from the old one leaks in.
// TestQuarantineCorrupt_TakesTheSidecarsWithIt calls this function directly for
// that reason; it is not evidence of a production path.
//
// The timestamp is nanosecond, not second: two heals of the same path inside
// one second would otherwise silently overwrite the first backup — the one
// case where this function destroys the data it exists to preserve.
func quarantineCorrupt(path string) (string, error) {
	backup := path + ".corrupt-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	if err := os.Rename(path, backup); err != nil {
		return "", err
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Rename(path+suffix, backup+suffix); err != nil {
			_ = os.Remove(path + suffix)
		}
	}
	return backup, nil
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
	// Switching journal mode needs a brief EXCLUSIVE lock, and busy_timeout does
	// not cover it — SQLite returns SQLITE_BUSY straight away rather than
	// waiting. Two processes opening one project at the same time therefore had
	// a real chance of one failing with "database is locked" before it had done
	// anything at all: measured, 8 simultaneous first opens failed on roughly
	// every attempt. That is the ordinary case, not an exotic one, since the
	// TUI's lockfile election is decided only after both have built their store.
	//
	// Retrying is the whole fix, because the contention is momentary and the
	// winner leaves the database in the very mode the loser wanted. Once any
	// process has set WAL the pragma stops needing the exclusive lock, so this
	// converges immediately rather than spinning for the full budget.
	var err error
	for range 100 {
		if _, err = s.DB.Exec("PRAGMA journal_mode=WAL"); err == nil {
			return nil
		}
		if !isTransientOpenErr(err) {
			return fmt.Errorf("store: set WAL: %w", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("store: set WAL: %w", err)
}

// SQLite result codes that mean "another process got there first, try again".
// Neither says anything about the data, which is why they are deliberately NOT
// in isCorruptDB's set — healing on one would quarantine a database whose only
// problem is that somebody else is using it.
const (
	// sqliteBusy is SQLITE_BUSY: a conflicting lock is held.
	sqliteBusy = 5
	// sqliteIOErrDeleteNoent is SQLITE_IOERR_DELETE_NOENT, the extended code
	// for "tried to delete a file that is no longer there". A concurrent opener
	// removing the same journal or WAL sidecar produces it, and it is benign.
	sqliteIOErrDeleteNoent = 5898
	// sqliteReadOnly is SQLITE_READONLY. In the concurrent-heal storm a failed
	// opener's handle can end up pointed at the just-renamed-away inode (or at
	// a file the repairer is mid-rewrite on), and windows surfaces that as
	// READONLY rather than BUSY — measured in the six-way healer test
	// (run 33541845801). Like BUSY it says "somebody else is mid-move", which
	// is no evidence about the file's own health.
	sqliteReadOnly = 8
)

// isTransientOpenErr reports whether err is one of the contention codes above.
//
// This list is narrow on purpose and is NOT a general "retry storage errors"
// policy: it covers exactly the two codes measured coming out of concurrent
// first opens. Widening it would start retrying real I/O failures, which is how
// a broken disk turns into a hang instead of an error.
func isTransientOpenErr(err error) bool {
	var se *sqlite.Error
	if !errors.As(err, &se) {
		return false
	}
	return se.Code() == sqliteBusy || se.Code() == sqliteIOErrDeleteNoent ||
		se.Code() == sqliteReadOnly
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
	// INF3: pinned_seqs arrived after context_events shipped, so CREATE TABLE IF
	// NOT EXISTS skips it on every database that already has the table — which is
	// every database the previous round touched. Without this line the column
	// exists only for fresh installs, and an upgrade fails on the first read.
	if err := s.addColumnIfMissing("context_events", "pinned_seqs", "TEXT NOT NULL DEFAULT ''"); err != nil {
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
	// W-D-03: use_count makes "unused" answerable, which is what the memory
	// quota prunes by. Pre-W-D-03 rows default to 0 — literally true, since
	// nothing was counting — so the first prune after an upgrade treats the
	// whole existing table as unused and falls back to oldest-first. That is
	// the honest reading and it is why the quota defaults to unlimited: an
	// operator has to turn it on, and by the time they do the counter has been
	// running.
	//
	// DELIBERATELY NOT IN memoryColumns OR THE Memory STRUCT. Only SQL reads it
	// (the prune's ORDER BY) and only SQL writes it (markMemoriesUsed), so
	// exposing it as a Go field would add a value nothing consumes and a scan
	// position every reader has to keep in step.
	if err := s.addColumnIfMissing("memories", "use_count", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	// W-D-07: provenance. source_session_id names the session whose log produced
	// the row; source_seq is where in that log the derivation started.
	//
	// THE PAIR IS NOT REDUNDANT WITH session_id, EVEN THOUGH EVERY CURRENT
	// WRITER SETS THEM TO THE SAME VALUE. session_id is a RETRIEVAL DIMENSION —
	// a caller may leave it empty on purpose (WriteMemory does), and a future
	// one may rescope a row. Provenance is a fact about how the row came to
	// exist and must not move when a scope does. The split is also what makes
	// the upgrade honest: pre-W-D-07 rows carry a session_id but nobody recorded
	// their origin, and defaulting source_session_id to '' says exactly that,
	// whereas reusing session_id would have every existing memory claim a
	// position (seq 0) it was never derived from.
	//
	// '' IS THE "NO PROVENANCE" MARKER, NOT source_seq = 0. Seq 0 is a real row,
	// so a window legitimately starts there; see MemorySource.
	//
	// Kept OUT of memoryColumns and the Memory struct for use_count's reason:
	// MemorySource is the only reader and it selects the pair by name, so
	// carrying them through every scan would add two fields nothing consumes
	// and two scan positions three readers have to keep in step.
	if err := s.addColumnIfMissing("memories", "source_session_id", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.addColumnIfMissing("memories", "source_seq", "INTEGER NOT NULL DEFAULT 0"); err != nil {
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
	if err != nil && strings.Contains(err.Error(), "duplicate column name") {
		// The check above and this ALTER are not one atomic step, and nothing
		// serialises two PROCESSES opening the same database: both read the
		// column as missing, both add it, and the loser gets this error. That
		// is not a failure — the postcondition this function promises (the
		// column exists) is exactly what the winner just established.
		//
		// It is reachable without any of the self-heal machinery: two yanshi
		// processes starting on one project at the same time is the ordinary
		// case (the TUI's lockfile election is decided AFTER both have built).
		// Measured before this check, a 6-way concurrent open failed roughly
		// 2% of runs with "duplicate column name: worktree_id / session_id /
		// use_count" — whichever migration the two happened to collide on.
		return nil
	}
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

-- INF3 (ADR-0015): compaction markers. The messages table stays byte-identical
-- and append-only; what changes is that the active window is now a PROJECTION
-- over it rather than a raw SELECT. A 'compact' event says "the kept tail starts
-- at hidden_seq, plus these scattered survivors below it"; an 'undo' event pops
-- the most recent one. Nothing is ever updated or deleted here — reverting is
-- expressed by appending.
--
-- pinned_seqs is not an optimisation. ctxcompact.Plan pins messages ANYWHERE in
-- the history, so a compacted window is a set with holes and no single watermark
-- describes it; see store.contextBoundary for the full reversal.
--
-- No foreign key and no DeleteSession cascade, deliberately. Session ids are
-- random and never reused, so events left behind by a deleted session are
-- unreachable rather than stale, and paying a few orphan rows buys the stronger
-- property that this table has exactly one verb. store.AppendContextEvent's doc
-- states the rule; internal/store's own test enforces it by scanning the
-- package for UPDATE/DELETE against this table.
CREATE TABLE IF NOT EXISTS context_events (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id  TEXT    NOT NULL,
    kind        TEXT    NOT NULL,
    hidden_seq  INTEGER NOT NULL,
    pinned_seqs TEXT    NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_context_events_session
    ON context_events(session_id, id);

-- W-D-04: cold storage. One gzip blob per session, holding every row that used
-- to sit in the messages table. Per-session rather than per-row because a single
-- chat message does not compress — the win comes from the shared vocabulary
-- across a whole transcript.
--
-- DO NOT CONFUSE THIS WITH sessions.archived (V10). That flag hides a session
-- from the active list and changes nothing about storage; this table IS the
-- storage. A session can be in either state independently of the other, which is
-- why the names here avoid the word "archived" entirely.
--
-- max_seq is the highest seq inside the blob. It is here rather than derived so
-- ProjectWindow's stale-boundary backstop can keep working on a compressed
-- session without inflating the blob to answer one integer.
CREATE TABLE IF NOT EXISTS cold_sessions (
    session_id TEXT PRIMARY KEY,
    blob       BLOB    NOT NULL,
    max_seq    INTEGER NOT NULL
);

-- W-D-06: a named moment three dimensions can be rolled back to.
--
-- The three columns groups are the three dimensions and they are stored very
-- differently on purpose, because the three underlying stores have different
-- properties:
--
--   session: hidden_seq + pinned_seqs, i.e. a COPY OF THE CONTEXT BOUNDARY.
--     messages is append-only and context_events is append-only, so nothing
--     about a session's past can be lost; a checkpoint therefore only has to
--     remember where the window stood, and restoring is one more append
--     (ADR-0015's "checkpoints degrade to appending one event").
--
--   memory: a memories BLOB, a real snapshot. Memories are UPDATEd (use_count,
--     superseded_by) and DELETEd (the quota prune, /memory-clear), so there is
--     no append-only history to project a past state out of. gzip JSON, the
--     same encoder cold_sessions uses.
--
--   files: file_commit, just an id. internal/vcs already stores every version
--     of every file and can preview, freeze and restore them; duplicating any
--     of that here would be a second copy of the working tree.
CREATE TABLE IF NOT EXISTS checkpoints (
    id          TEXT    PRIMARY KEY,
    label       TEXT    NOT NULL DEFAULT '',
    session_id  TEXT    NOT NULL DEFAULT '',
    hidden_seq  INTEGER NOT NULL DEFAULT 0,
    pinned_seqs TEXT    NOT NULL DEFAULT '',
    memories     BLOB    NOT NULL,
    memory_count INTEGER NOT NULL DEFAULT 0,
    file_commit  TEXT    NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_checkpoints_created
    ON checkpoints(created_at DESC, id);

-- W-D-08: user messages queued for a session that may or may not be connected.
--
-- NOT EXPRESSED AS A tasks ROW, and the reason is concrete rather than
-- stylistic: task.Broker.Claim calls store.ListPending(1) and claims whatever
-- comes back WITHOUT filtering by type, so a queued chat message parked in the
-- tasks table would be picked up by the next cmd/agent-worker and executed as a
-- work item. The two queues also have opposite lifecycles — a task is claimed,
-- heartbeated, retried and can fail; a queued message is delivered once to the
-- session that owns it and has no worker, no ownership and no retry.
--
-- ADR-0015 constraint 1 DOES NOT APPLY HERE. That rule is about
-- context_events, which must stay INSERT-only because it is the compaction log.
-- This table is a work queue: marking a row consumed is the whole point, and
-- expressing consumption by appending would mean re-deriving delivery state on
-- every read.
--
-- consumed_at rather than DELETE so "what was queued and when did it land" is
-- still answerable after delivery; nothing here is large enough to be worth
-- reclaiming.
CREATE TABLE IF NOT EXISTS queued_messages (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id  TEXT    NOT NULL,
    content     TEXT    NOT NULL,
    created_at  INTEGER NOT NULL,
    consumed_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_queued_messages_session
    ON queued_messages(session_id, id);

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
