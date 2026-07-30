package store

import (
	"context"
	"database/sql"
	"time"
)

// Memory is a stored memory record.
type Memory struct {
	ID        string
	Kind      string
	Content   string
	CreatedAt int64
}

// WriteMemory inserts a memory and returns its id.
func (s *Store) WriteMemory(kind, content string) (string, error) {
	id := newID()
	now := time.Now().Unix()
	err := s.WriteTx(context.Background(), func(tx *sql.Tx) error {
		_, e := tx.Exec(
			"INSERT INTO memories (id, kind, content, created_at) VALUES (?, ?, ?, ?)",
			id, kind, content, now,
		)
		return e
	})
	if err != nil {
		return "", err
	}
	return id, nil
}

// SearchMemory runs an FTS5 full-text query over memory contents, returning up
// to limit most-relevant matches (newest first).
func (s *Store) SearchMemory(query string, limit int) ([]Memory, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.DB.Query(
		`SELECT m.id, m.kind, m.content, m.created_at
		 FROM memories_fts f JOIN memories m ON m.rowid = f.rowid
		 WHERE memories_fts MATCH ?
		 ORDER BY m.created_at DESC LIMIT ?`,
		query, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMemories(rows)
}

// RecallMemory returns up to limit most-recent memories.
func (s *Store) RecallMemory(limit int) ([]Memory, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.DB.Query(
		"SELECT id, kind, content, created_at FROM memories ORDER BY created_at DESC LIMIT ?",
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMemories(rows)
}

func scanMemories(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}) ([]Memory, error) {
	var out []Memory
	for rows.Next() {
		var m Memory
		if err := rows.Scan(&m.ID, &m.Kind, &m.Content, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
