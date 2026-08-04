package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// ForkSession creates a new session by copying messages[0..fromSeq] (inclusive)
// from srcID into a new session row with a fresh id. The original session is
// not modified (rows are not shared, so COW is implicit).
//
// fromSeq semantics (unified across store / WS handler / TUI help):
//
//	-1      = copy ALL messages.
//	>=0     = copy messages with seq <= fromSeq (inclusive upper bound).
//	<-1     = invalid; return an error and create no fork (GB5).
//
// For fromSeq >=0, only values greater than the source's maximum seq are out
// of range; an upper bound that falls in a seq gap still copies every row with
// seq <= fromSeq. An empty source can only be forked with -1 (all/zero rows).
//
// GB6: the current schema has no per-message usage, so we cannot compute a
// faithful prefix sum. To avoid crediting the fork with the source's full
// cumulative totals, fork rows reset TokensIn/TokensOut/Turns/CachedTokens/
// ReasoningTokens to 0 and let subsequent turns re-accumulate from scratch.
// Model/Thinking are preserved so /model and /think reflect the active
// provider config.
//
// The local variable name `forkID` is deliberately NOT `newID` — that would
// shadow the package-level newID() function and break compilation.
func (s *Store) ForkSession(srcID string, fromSeq int) (string, error) {
	// GB5: reject anything other than -1 (all) or >=0 (inclusive upper bound).
	if fromSeq < -1 {
		return "", fmt.Errorf("ForkSession: invalid fromSeq %d (want -1 for all, or >=0 for inclusive upper bound)", fromSeq)
	}
	var forkID string
	err := s.WriteTx(context.Background(), func(tx *sql.Tx) error {
		// Load the source session row so we can clone its title/model/thinking.
		var (
			srcTitle    string
			srcModel    string
			srcThinking string
		)
		if err := tx.QueryRow(
			"SELECT title, model, thinking FROM sessions WHERE id = ?",
			srcID,
		).Scan(&srcTitle, &srcModel, &srcThinking); err != nil {
			return fmt.Errorf("ForkSession: load source session: %w", err)
		}

		// Load the source messages in the same tx (consistent snapshot).
		rows, err := tx.Query(
			"SELECT seq, role, content FROM messages WHERE session_id = ? ORDER BY seq ASC",
			srcID,
		)
		if err != nil {
			return fmt.Errorf("ForkSession: load messages: %w", err)
		}
		type srcMsg struct {
			Seq     int
			Role    string
			Content string
		}
		var allMsgs []srcMsg
		for rows.Next() {
			var m srcMsg
			if err := rows.Scan(&m.Seq, &m.Role, &m.Content); err != nil {
				rows.Close()
				return fmt.Errorf("ForkSession: scan message: %w", err)
			}
			allMsgs = append(allMsgs, m)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("ForkSession: rows iter: %w", err)
		}

		// Determine the slice to copy based on fromSeq.
		var toCopy []srcMsg
		switch {
		case fromSeq == -1:
			toCopy = allMsgs
		default: // fromSeq >= 0
			maxSeq := -1
			for _, m := range allMsgs {
				if m.Seq > maxSeq {
					maxSeq = m.Seq
				}
			}
			if fromSeq > maxSeq {
				return fmt.Errorf("ForkSession: fromSeq %d out of range (max seq in source = %d)", fromSeq, maxSeq)
			}
			for _, m := range allMsgs {
				if m.Seq <= fromSeq {
					toCopy = append(toCopy, m)
				}
			}
		}

		forkID = newID() // NOT shadowed: this is a fresh local name.
		now := time.Now().Unix()
		title := srcTitle
		if title == "" {
			title = "fork"
		}
		// GB6: model/thinking inherited; usage/turns/cached/reasoning reset to 0.
		if _, err := tx.Exec(
			"INSERT INTO sessions (id, title, created_at, updated_at, model, thinking, tokens_in, tokens_out, turns, cached_tokens, reasoning_tokens, archived) VALUES (?, ?, ?, ?, ?, ?, 0, 0, 0, 0, 0, 0)",
			forkID, title, now, now, srcModel, srcThinking,
		); err != nil {
			return fmt.Errorf("ForkSession: insert session: %w", err)
		}
		for _, m := range toCopy {
			msgID := newID()
			if _, err := tx.Exec(
				`INSERT INTO messages (id, session_id, seq, role, content, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
				msgID, forkID, m.Seq, m.Role, m.Content, now,
			); err != nil {
				return fmt.Errorf("ForkSession: insert message seq=%d: %w", m.Seq, err)
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return forkID, nil
}
