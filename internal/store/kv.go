package store

import (
	"context"
	"database/sql"
)

// KVSet inserts or updates a key-value pair.
func (s *Store) KVSet(key, value string) error {
	return s.WriteTx(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`INSERT INTO kv (key, value) VALUES (?, ?)
			 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
			key, value,
		)
		return err
	})
}

// KVGet returns the value for key. ok is false if the key does not exist.
func (s *Store) KVGet(key string) (value string, ok bool, err error) {
	err = s.DB.QueryRow("SELECT value FROM kv WHERE key = ?", key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}
