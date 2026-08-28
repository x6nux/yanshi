// internal/store/coldstore.go
//
// W-D-04: cold sessions live as one compressed blob instead of N rows.
//
// yanshi.db grows without bound on a single-binary local deployment: every
// tool result the ReAct loop ever produced is in `messages` forever, and C1
// made that MORE true by design (the durable log exists precisely so
// compaction cannot destroy anything). Nothing here contradicts that — a cold
// session is still readable byte for byte, it just stops occupying one row per
// message.
//
// gzip, from the standard library. go.mod carries no klauspost/compress, and a
// dependency is not worth the few points zstd would add on text that is already
// mostly ASCII prose.
//
// PERSISTENCE FIRST, SAME AS C1. CompressSession writes the blob, reads it back
// and compares it to what it just serialised, and only then deletes the rows —
// all inside one WriteTx, so a failure anywhere leaves the rows exactly where
// they were. The rule is C1's: never remove the original until the replacement
// is provably readable.
//
// THE KNOWN CEILING: deleting the rows fires the messages_ad trigger, so a
// compressed session leaves the FTS index. history_search stops finding it.
// That is inherent to moving rows out of an external-content FTS table (the
// index stores terms, the join fetches the text from `messages`, and an entry
// whose base row is gone drops out of the join anyway), and it is the price the
// feature was asked for. Sessions only go cold after storage.retention_days of
// no activity, so the text that leaves the index is the text nobody has touched
// in that long.
package store

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"slices"
)

// coldBlobLimit bounds how much decompressed JSON one session may expand to.
// Without it a corrupted or hostile blob turns a projection into an
// out-of-memory, and the read path here is reached by every restore.
const coldBlobLimit = 256 << 20

// CompressSession moves a session's messages into cold storage and returns how
// many rows it packed. A session that is already compressed, or that has no
// rows, is a no-op reporting 0.
//
// Exported although CompressColdSessions is its only caller in this package:
// compressing ONE named session is the operation every test of this file and of
// internal/tools drives, and routing them through the sweep instead would make
// each of them set up an idle-timestamp fixture to reach the code under test.
//
// IT DOES NOT TOUCH context_events, AND THAT IS THE POINT. The event log is the
// other half of the projection: delete it and the compaction markers go with
// it, so the next reconnect rebuilds the window from the full transcript and
// pays for a summary it already bought. ADR-0015's first constraint grants
// exactly one deletion exemption (DeleteSession) and compression is not a
// deletion — the conversation is still all there.
//
// It also does NOT call undoBoundariesAtOrAfterTx, even though that function's
// doc names cold-session archival as a caller-to-be. That instruction assumes
// the rows become unreadable; here they do not. messagesInWindow falls back to
// this blob and applies the same boundary, so a boundary that was valid before
// compression is still valid after it, and popping it would silently hand the
// model back a transcript compaction had already summarised.
// TestColdStore_ProjectWindowStillHonoursTheBoundary is what goes red if either
// half of that stops being true.
func (s *Store) CompressSession(sessionID string) (int, error) {
	if sessionID == "" {
		return 0, fmt.Errorf("store: compress session: empty session id")
	}
	var packed int
	err := s.WriteTx(context.Background(), func(tx *sql.Tx) error {
		var exists int
		if err := tx.QueryRow(
			"SELECT COUNT(*) FROM cold_sessions WHERE session_id = ?", sessionID,
		).Scan(&exists); err != nil {
			return fmt.Errorf("store: probe cold session: %w", err)
		}
		if exists > 0 {
			return nil
		}
		msgs, err := messagesTx(tx, sessionID)
		if err != nil {
			return err
		}
		if len(msgs) == 0 {
			return nil
		}
		blob, err := encodeColdBlob(msgs)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(
			"INSERT INTO cold_sessions (session_id, blob, max_seq) VALUES (?, ?, ?)",
			sessionID, blob, msgs[len(msgs)-1].Seq,
		); err != nil {
			return fmt.Errorf("store: write cold session: %w", err)
		}

		// Read back through the same path a projection will use, from the row
		// as SQLite stored it rather than from the slice still in hand. A blob
		// that round-trips in memory but not through the column is exactly the
		// failure this check exists to catch, and after the DELETE below there
		// is nothing left to catch it with.
		//
		// NO TEST COVERS THIS BRANCH, measured: stubbing the comparison out
		// leaves the suite green, because gzip+JSON over []Message has no
		// reachable failure today and faking one would need a seam whose only
		// consumer is the seam. It stays for the same reason C1 refuses to
		// evict what did not persist — the check is one comparison, and the
		// thing it guards is the only irreversible step in this file. The
		// atomicity claim above IS load-bearing and is real regardless: every
		// statement here runs in one WriteTx, so any error rolls the DELETE
		// back with everything else.
		var stored []byte
		if err := tx.QueryRow(
			"SELECT blob FROM cold_sessions WHERE session_id = ?", sessionID,
		).Scan(&stored); err != nil {
			return fmt.Errorf("store: verify cold session: %w", err)
		}
		back, err := decodeColdBlob(stored)
		if err != nil {
			return fmt.Errorf("store: verify cold session: %w", err)
		}
		if !slices.Equal(back, msgs) {
			return fmt.Errorf("store: cold session %s did not round-trip (%d rows in, %d out)",
				sessionID, len(msgs), len(back))
		}
		if _, err := tx.Exec("DELETE FROM messages WHERE session_id = ?", sessionID); err != nil {
			return fmt.Errorf("store: drop compressed messages: %w", err)
		}
		packed = len(msgs)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return packed, nil
}

// thawColdSessionTx puts a compressed session's rows back before anything is
// appended to it, and is a no-op for a session that was never compressed.
//
// IT IS WHAT KEEPS "READABLE BYTE FOR BYTE" TRUE ACROSS A WRITE. Compression
// leaves `messages` empty for the session, and two things then conspire:
// messagesInWindow only consults the blob when the live query returns ZERO
// rows, and AppendMessages takes its watermark from MAX(seq) over that same
// empty table. So the first row written to a compressed session both restarts
// seq at 0 — colliding with every archived row and stranding the boundary above
// the new log — and makes the table non-empty, which switches the cold fallback
// off for good. Measured before this existed: a 10-message session compressed,
// then sent one more message, read back as ONE message, permanently.
//
// Thawing rather than teaching every reader to union the blob in: a session
// being written to is not cold, and putting the rows back restores the FTS
// index (history_search finds it again), makes it eligible for a later
// compression sweep instead of being excluded forever by IdleSessions'
// skipCompressed, and leaves exactly one shape of truth in `messages`.
//
// The rows go back with their original ids, seqs and dedup keys, so the log is
// byte-identical to what CompressSession packed. It runs inside the caller's
// write transaction, so a failure anywhere leaves the blob untouched.
func thawColdSessionTx(tx *sql.Tx, sessionID string) error {
	var blob []byte
	switch err := tx.QueryRow(
		"SELECT blob FROM cold_sessions WHERE session_id = ?", sessionID,
	).Scan(&blob); {
	case err == sql.ErrNoRows:
		return nil
	case err != nil:
		return fmt.Errorf("store: probe cold session: %w", err)
	}
	msgs, err := decodeColdBlob(blob)
	if err != nil {
		return fmt.Errorf("store: thaw cold session %s: %w", sessionID, err)
	}
	for _, m := range msgs {
		if _, err := tx.Exec(
			`INSERT INTO messages
			   (id, session_id, seq, role, content, tool_call_id, tool_name, tool_args, dedup_key, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			m.ID, sessionID, m.Seq, m.Role, m.Content,
			m.ToolCallID, m.ToolName, m.ToolArgs, m.DedupKey, m.CreatedAt,
		); err != nil {
			return fmt.Errorf("store: thaw message %s: %w", m.ID, err)
		}
	}
	if _, err := tx.Exec("DELETE FROM cold_sessions WHERE session_id = ?", sessionID); err != nil {
		return fmt.Errorf("store: drop thawed cold session: %w", err)
	}
	return nil
}

// messagesTx reads a session's whole log inside a caller's transaction, so the
// rows that get packed are the rows that get deleted.
func messagesTx(tx *sql.Tx, sessionID string) ([]Message, error) {
	rows, err := tx.Query(
		"SELECT "+messageColumns+" FROM messages WHERE session_id = ? ORDER BY seq ASC",
		sessionID)
	if err != nil {
		return nil, fmt.Errorf("store: read messages for compression: %w", err)
	}
	defer rows.Close()
	return scanMessages(rows)
}

// encodeColdBlob renders a session as gzipped JSON.
func encodeColdBlob(msgs []Message) ([]byte, error) {
	blob, err := encodeGzipJSON(msgs)
	if err != nil {
		return nil, fmt.Errorf("store: encode cold session: %w", err)
	}
	return blob, nil
}

// decodeColdBlob is the inverse, bounded by coldBlobLimit.
func decodeColdBlob(blob []byte) ([]Message, error) {
	msgs, err := decodeGzipJSON[Message](blob)
	if err != nil {
		return nil, fmt.Errorf("store: decode cold session: %w", err)
	}
	return msgs, nil
}

// encodeGzipJSON renders any row slice as gzipped JSON.
//
// Generic because W-D-06 snapshots the `memories` table with the same encoder
// this file already uses for `messages` — one compression format, one bound,
// one place where a decode limit could be got wrong. gzip and not a new
// dependency, for the reason in this file's header.
func encodeGzipJSON[T any](rows []T) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if err := json.NewEncoder(zw).Encode(rows); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// decodeGzipJSON is encodeGzipJSON's inverse, bounded by coldBlobLimit so a
// corrupt or hostile blob cannot turn a read into an out-of-memory.
func decodeGzipJSON[T any](blob []byte) ([]T, error) {
	zr, err := gzip.NewReader(bytes.NewReader(blob))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	var rows []T
	if err := json.NewDecoder(io.LimitReader(zr, coldBlobLimit)).Decode(&rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// coldMessages returns a compressed session's rows, or ok=false when the
// session has none. Errors are reported rather than swallowed: a blob that
// cannot be read is the one case where returning "no messages" would look
// exactly like an empty session and hand the model a blank conversation.
func (s *Store) coldMessages(sessionID string) (msgs []Message, ok bool, err error) {
	var blob []byte
	switch err := s.DB.QueryRow(
		"SELECT blob FROM cold_sessions WHERE session_id = ?", sessionID,
	).Scan(&blob); {
	case err == sql.ErrNoRows:
		return nil, false, nil
	case err != nil:
		return nil, false, err
	}
	out, err := decodeColdBlob(blob)
	if err != nil {
		return nil, false, err
	}
	return out, true, nil
}

// coldMaxSeq returns the highest seq held in cold storage for a session.
// ok=false means the session is not compressed.
func (s *Store) coldMaxSeq(sessionID string) (int, bool, error) {
	var maxSeq int
	switch err := s.DB.QueryRow(
		"SELECT max_seq FROM cold_sessions WHERE session_id = ?", sessionID,
	).Scan(&maxSeq); {
	case err == sql.ErrNoRows:
		return 0, false, nil
	case err != nil:
		return 0, false, err
	}
	return maxSeq, true, nil
}

// filterWindow applies a context boundary to an already-materialised slice.
//
// THIS IS RULING P-1 AND IT IS NOT OPTIONAL. The live path expresses the
// boundary as a WHERE clause; the cold path has no rows to put a WHERE clause
// on, so without this the fallback would return the FULL transcript for a
// session that had been compacted — every original the summary replaced,
// straight back into the window, which is the exact bug ADR-0015 exists to
// stop, re-entering through the storage door.
//
// fromSeq <= 0 means "no boundary", and the slice is returned untouched so a
// never-compacted session reads identically either side of compression
// (ADR-0015's second constraint).
func filterWindow(msgs []Message, fromSeq int, pinned []int) []Message {
	if fromSeq <= 0 {
		return msgs
	}
	pins := make(map[int]bool, len(pinned))
	for _, p := range pinned {
		pins[p] = true
	}
	out := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		if m.Seq >= fromSeq || pins[m.Seq] {
			out = append(out, m)
		}
	}
	return out
}

// IdleSessions returns up to limit session ids whose last activity predates
// `before`, oldest first.
//
// skipCompressed drops sessions already in cold storage. The compression sweep
// passes true, so a store where everything old is already packed stops handing
// the worker the same ids every tick; the memory sweep passes false, because a
// compressed session still projects a readable window and excluding it would
// permanently skip every session that went cold before extraction was enabled.
//
// One function with a flag rather than two near-identical SELECTs: the ordering
// and the limit clamp are the parts that must not drift between the two sweeps.
func (s *Store) IdleSessions(before int64, limit int, skipCompressed bool) ([]string, error) {
	if limit <= 0 {
		limit = 50
	}
	q := "SELECT id FROM sessions WHERE updated_at < ?"
	if skipCompressed {
		q += " AND id NOT IN (SELECT session_id FROM cold_sessions)"
	}
	q += " ORDER BY updated_at ASC LIMIT ?"
	rows, err := s.DB.Query(q, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// CompressColdSessions compresses every session idle since `before` and returns
// how many it packed.
//
// A `before` of zero or less compresses NOTHING and reports no error.
//
// THAT GUARD IS DEFENCE IN DEPTH, NOT THE OFF SWITCH, and the difference was
// measured rather than assumed: deleting it leaves the whole suite green,
// because session timestamps are positive unix seconds and a cutoff of zero
// selects no candidate anyway. The switch operators actually turn is
// upkeep.Worker.compressCold, which never calls this function at all when
// storage.retention_days is 0 — that one IS guarded, by
// upkeep.TestWorker_RetentionDaysDrivesCompression and
// bootstrap.TestUpkeep_ZeroRetentionLeavesTheStoreAlone, both of which go red
// when it is removed. This line stays because "compress everything older than
// the epoch" is not a request any caller means, and answering it explicitly is
// cheaper than reasoning about timestamp signs at the call site.
//
// One failed session does not abort the sweep. Compression is opportunistic
// maintenance, and a single unreadable session must not stop the other
// thousand from being packed; the error is returned after the loop so the
// caller can log it.
func (s *Store) CompressColdSessions(before int64, limit int) (int, error) {
	if before <= 0 {
		return 0, nil
	}
	ids, err := s.IdleSessions(before, limit, true)
	if err != nil {
		return 0, err
	}
	var packed int
	var firstErr error
	for _, id := range ids {
		n, err := s.CompressSession(id)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if n > 0 {
			packed++
		}
	}
	return packed, firstErr
}
