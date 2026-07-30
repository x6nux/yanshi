package store

import (
	"database/sql"
	"errors"
	"time"

	"github.com/x6nux/yanshi/internal/auth"
)

// authSQLiteAdapter is the outbound SQLite adapter for auth.MetadataStore.
// It persists only lifecycle metadata; its schema intentionally has no
// secret columns (access_token / refresh_token / device_code / user_code /
// client_secret). Keeping the type private prevents callers from depending
// on SQLite specifics — the public surface is the auth.MetadataStore
// interface returned by AuthMetadataFromDB.
//
// writeMu is the *Store's shared writeMu, serializing WAL writes across all
// consumers (auth, store, VCS) so no consumer can hit SQLITE_BUSY inside the
// same process. Read methods (LoadAuthMetadata) do NOT hold writeMu.
type authSQLiteAdapter struct {
	s *Store
}

// AuthMetadataFromDB follows the composition-root adapter style: bootstrap
// supplies the already-migrated Store and receives the inward auth port.
// Returning the interface (not the concrete struct) keeps bootstrap from
// accidentally reaching past the port. The returned adapter shares the
// process-wide writeMu so auth writes do not compete with store/VCS writes.
func AuthMetadataFromDB(s *Store) auth.MetadataStore {
	return &authSQLiteAdapter{s: s}
}

// SaveAuthMetadata upserts exactly one row. expires_at is stored as Unix
// seconds; the zero time maps to 0 so legacy rows without an expiry read
// back as "never expires" rather than 1970. updated_at is wall-clock now.
// Uses the shared writeMu to serialize with other store/VCS writes.
func (a *authSQLiteAdapter) SaveAuthMetadata(
	provider, account string,
	meta auth.AuthMetadata,
) error {
	a.s.writeMu.Lock()
	defer a.s.writeMu.Unlock()
	expiresAt := int64(0)
	if !meta.ExpiresAt.IsZero() {
		expiresAt = meta.ExpiresAt.Unix()
	}
	_, err := a.s.DB.Exec(
		`INSERT INTO auth_metadata
            (provider, account, source, expires_at, updated_at)
         VALUES (?, ?, ?, ?, ?)
         ON CONFLICT(provider, account) DO UPDATE SET
            source = excluded.source,
            expires_at = excluded.expires_at,
            updated_at = excluded.updated_at`,
		provider,
		account,
		meta.Source,
		expiresAt,
		time.Now().Unix(),
	)
	return err
}

// LoadAuthMetadata returns the stored row for (provider, account). A missing
// row yields auth.ErrAuthMetadataNotFound so Status can fall back to
// Source="secret" without mistaking the no-record case for a backend error.
func (a *authSQLiteAdapter) LoadAuthMetadata(
	provider, account string,
) (auth.AuthMetadata, error) {
	var source string
	var expiresAt int64
	err := a.s.DB.QueryRow(
		`SELECT source, expires_at FROM auth_metadata
         WHERE provider = ? AND account = ?`,
		provider, account,
	).Scan(&source, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return auth.AuthMetadata{}, auth.ErrAuthMetadataNotFound
	}
	if err != nil {
		return auth.AuthMetadata{}, err
	}
	meta := auth.AuthMetadata{Source: source}
	if expiresAt != 0 {
		meta.ExpiresAt = time.Unix(expiresAt, 0).UTC()
	}
	return meta, nil
}

// DeleteAuthMetadata removes one row. A no-op delete returns
// auth.ErrAuthMetadataNotFound so the compensating Logout transaction can
// distinguish "metadata already gone" from "backend errored". The affected-
// row count is the source of truth — SQLite returns nil even when zero rows
// matched.
func (a *authSQLiteAdapter) DeleteAuthMetadata(provider, account string) error {
	a.s.writeMu.Lock()
	defer a.s.writeMu.Unlock()
	result, err := a.s.DB.Exec(
		`DELETE FROM auth_metadata WHERE provider = ? AND account = ?`,
		provider, account,
	)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return auth.ErrAuthMetadataNotFound
	}
	return nil
}
