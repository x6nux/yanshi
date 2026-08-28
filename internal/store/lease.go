package store

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"time"
)

// leasePrefix namespaces lease rows inside the kv table.
const leasePrefix = "lease:"

// LeaseRetired is the expiry written by RetireLease: far enough out that no
// clock this program will run under reaches it, so a retired lease is never
// re-claimable. A sentinel rather than a second column because the whole point
// of putting leases in kv is that they need no schema of their own.
const LeaseRetired int64 = 1<<62 - 1

// ClaimLease takes a named lease for ttl and reports whether this caller won it.
//
// IT REUSES THE kv TABLE ON PURPOSE. A dedicated leases table would carry a
// holder column, an index and a migration to express what one row of
// "name → expiry" already says, and kv is already the place this repo keeps
// small durable scalars (goalrun records live there too). Nothing here needs to
// know WHO holds the lease — the only question asked is "may I start", and the
// answer is the same whether the incumbent is another goroutine or another
// process.
//
// THE CLAIM IS ONE STATEMENT, WHICH IS WHY IT IS SAFE ACROSS PROCESSES. The
// UPSERT's WHERE clause is evaluated by SQLite while it holds the write lock,
// so two processes issuing it concurrently produce exactly one RowsAffected=1.
// A read-then-write version would be atomic within one process (WriteTx holds
// writeMu) and racy across two, which is the shape that matters: yanshi
// routinely runs a TUI, a `serve` and a spawned vcs-mcp against one database.
//
// A lease is never explicitly released. Failing while holding one is the common
// case — a model call timed out, the process was killed — and an expiry is the
// only release that survives that. Callers that finish successfully and never
// want to run again call RetireLease.
func (s *Store) ClaimLease(name string, ttl time.Duration) (bool, error) {
	if name == "" {
		return false, fmt.Errorf("store: claim lease: empty name")
	}
	if ttl <= 0 {
		return false, fmt.Errorf("store: claim lease %q: non-positive ttl %v", name, ttl)
	}
	now := time.Now().Unix()
	var won bool
	err := s.WriteTx(context.Background(), func(tx *sql.Tx) error {
		res, e := tx.Exec(
			`INSERT INTO kv (key, value) VALUES (?, ?)
			 ON CONFLICT(key) DO UPDATE SET value = excluded.value
			   WHERE CAST(kv.value AS INTEGER) < ?`,
			leasePrefix+name, strconv.FormatInt(now+int64(ttl/time.Second), 10), now,
		)
		if e != nil {
			return e
		}
		n, e := res.RowsAffected()
		if e != nil {
			return e
		}
		won = n > 0
		return nil
	})
	return won, err
}

// RetireLease marks a lease permanently held, so ClaimLease never grants it
// again. It is how "this unit of work is done, do not repeat it" is recorded
// without a second table.
func (s *Store) RetireLease(name string) error {
	if name == "" {
		return fmt.Errorf("store: retire lease: empty name")
	}
	return s.KVSet(leasePrefix+name, strconv.FormatInt(LeaseRetired, 10))
}

// LeaseHeldUntil returns the expiry stored for a lease, or ok=false when no
// claim was ever made. Callers use it to skip work whose lease is retired
// without paying for a claim attempt that would fail anyway.
func (s *Store) LeaseHeldUntil(name string) (int64, bool, error) {
	raw, ok, err := s.KVGet(leasePrefix + name)
	if err != nil || !ok {
		return 0, false, err
	}
	// A value that is not a number is treated as "expired long ago" rather than
	// as an error: the row is ours to overwrite, and refusing to run because a
	// scalar got corrupted would wedge the work permanently for no gain.
	until, convErr := strconv.ParseInt(raw, 10, 64)
	if convErr != nil {
		return 0, true, nil
	}
	return until, true, nil
}
