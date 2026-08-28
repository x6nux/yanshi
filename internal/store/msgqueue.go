// internal/store/msgqueue.go
//
// W-D-08: a durable queue of user messages per session.
//
// Before this, the only way to say something to a session was to be connected
// to it. A message typed while the backend was down, or aimed at a session
// nobody currently has open, had nowhere to go — so "queue this up for the run
// I started this morning" was not expressible at all, and neither was any
// scripted producer that outlives one connection.
//
// WHY NOT REUSE THE TASK BROKER. It is the closest existing thing and it was
// checked first: task.Broker.Claim calls ListPending(1) and claims whatever it
// finds WITHOUT filtering on type, so a chat message parked in `tasks` would be
// picked up by the next cmd/agent-worker and run as a work item. The lifecycles
// are opposite too — a task is claimed by a worker, heartbeated, retried and
// can fail; a queued message is delivered once, to the one session that owns
// it, with no worker and no ownership. This note exists to stop a third queue
// from being built by someone who reaches the same fork.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// EnqueueMessage appends a user message to a session's queue and returns the
// number of messages now waiting.
//
// THE SESSION MUST EXIST. This is the one place that can catch a mistyped id,
// and the alternative — accepting anything — means the message waits forever in
// a queue nobody will ever drain, with no error anywhere to explain it. A
// foreign key would enforce the same thing with a message the operator cannot
// act on, and this table deliberately carries none for the same reason
// context_events does not.
//
// The content is redacted like every other user text that reaches SQLite.
func (s *Store) EnqueueMessage(sessionID, content string) (int, error) {
	if sessionID == "" {
		return 0, fmt.Errorf("store: enqueue message: empty session id")
	}
	if content == "" {
		return 0, fmt.Errorf("store: enqueue message: empty content")
	}
	var pending int
	err := s.WriteTx(context.Background(), func(tx *sql.Tx) error {
		var exists int
		if e := tx.QueryRow(
			"SELECT COUNT(*) FROM sessions WHERE id = ?", sessionID,
		).Scan(&exists); e != nil {
			return e
		}
		if exists == 0 {
			return fmt.Errorf("store: enqueue message: no session %q", sessionID)
		}
		if _, e := tx.Exec(
			"INSERT INTO queued_messages (session_id, content, created_at) VALUES (?, ?, ?)",
			sessionID, s.redact(content), time.Now().Unix(),
		); e != nil {
			return e
		}
		return tx.QueryRow(
			"SELECT COUNT(*) FROM queued_messages WHERE session_id = ? AND consumed_at = 0",
			sessionID,
		).Scan(&pending)
	})
	if err != nil {
		return 0, err
	}
	return pending, nil
}

// PendingQueuedMessages returns the messages waiting for a session, oldest
// first, WITHOUT consuming them.
//
// Separate from ConsumeQueuedMessages so inspecting a queue is not the same act
// as draining it — an operator asking "what is waiting" must not empty it by
// asking.
func (s *Store) PendingQueuedMessages(sessionID string) ([]string, error) {
	rows, err := s.DB.Query(
		`SELECT content FROM queued_messages
		  WHERE session_id = ? AND consumed_at = 0 ORDER BY id ASC`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ConsumeQueuedMessages returns a session's waiting messages, oldest first, and
// marks them consumed in the SAME transaction.
//
// One transaction is what makes it safe for two processes to call this at once:
// the second sees an already-empty queue rather than a second copy of the same
// messages. Splitting the read and the mark would deliver every queued message
// twice whenever a second window resumed the same session.
//
// DELIVERY IS AT-MOST-ONCE, deliberately. The rows are marked the moment they
// are handed over, so a caller that crashes between receiving them and using
// them loses them. The alternative — mark after use — needs the caller to
// report success per message, and a queue that redelivers on every crash would
// resend a message that had already reached the model. For user input, saying
// something twice is worse than not saying it again.
//
// The ids are ordered by id, not created_at: created_at has one-second
// resolution, so two messages enqueued in the same second would come back in
// whatever order SQLite chose.
func (s *Store) ConsumeQueuedMessages(sessionID string) ([]string, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("store: consume queued messages: empty session id")
	}
	var out []string
	err := s.WriteTx(context.Background(), func(tx *sql.Tx) error {
		rows, e := tx.Query(
			`SELECT id, content FROM queued_messages
			  WHERE session_id = ? AND consumed_at = 0 ORDER BY id ASC`, sessionID)
		if e != nil {
			return e
		}
		var ids []any
		var msgs []string
		for rows.Next() {
			var id int64
			var content string
			if e := rows.Scan(&id, &content); e != nil {
				rows.Close()
				return e
			}
			ids = append(ids, id)
			msgs = append(msgs, content)
		}
		if e := rows.Err(); e != nil {
			rows.Close()
			return e
		}
		rows.Close()
		if len(ids) == 0 {
			return nil
		}
		args := append([]any{time.Now().Unix()}, ids...)
		if _, e := tx.Exec(
			"UPDATE queued_messages SET consumed_at = ? WHERE id IN ("+
				placeholders(len(ids))+")", args...); e != nil {
			return e
		}
		out = msgs
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
