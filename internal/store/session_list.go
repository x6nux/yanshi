package store

import "database/sql"

// SessionSummary is a lightweight session row for list views.
type SessionSummary struct {
	ID        string
	Title     string
	CreatedAt int64
	UpdatedAt int64
	Model     string
	Thinking  string
	TokensIn  int
	TokensOut int
	// CachedTokens / ReasoningTokens (Task A6): cumulative prompt-cache hits
	// and reasoning-model spend. Zero for sessions recorded before this column
	// existed.
	CachedTokens    int
	ReasoningTokens int
	Turns           int
	// Archived (V10) is true for sessions hidden from the active list but not
	// deleted. ListSessions returns Archived=false rows; ListArchivedSessions
	// returns Archived=true rows.
	Archived bool
	// Billed* and CostUSD/CostKnown (C4 COST1) carry the per-session cumulative
	// ledger produced by einollm.Ledger. Defaults are 0 / CostKnown=false so
	// pre-C4 rows render as "N/A" (treated as unknown). CostKnown=true means
	// every provider usage in the session referenced a known pricing entry.
	BilledInputTokens  int
	BilledCachedTokens int
	BilledOutputTokens int
	CostUSD            float64
	CostKnown          bool
}

// sessionColumns is the canonical column list for session SELECTs. List and
// Get paths share it via scanSession so they cannot drift apart.
const sessionColumns = "id, title, created_at, updated_at, model, thinking, tokens_in, tokens_out, turns, cached_tokens, reasoning_tokens, archived, billed_input_tokens, billed_cached_tokens, billed_output_tokens, cost_usd, cost_known"

// scanSession scans one session row into ss. It normalizes the integer-stored
// boolean columns (archived, cost_known) into the struct fields. A nil-backed
// scanner (sql.Rows or sql.Row) is accepted so list and Get paths reuse this.
func scanSession(scanner interface{ Scan(dest ...any) error }, ss *SessionSummary) error {
	var archived int
	var known int
	err := scanner.Scan(
		&ss.ID, &ss.Title, &ss.CreatedAt, &ss.UpdatedAt, &ss.Model, &ss.Thinking,
		&ss.TokensIn, &ss.TokensOut, &ss.Turns, &ss.CachedTokens, &ss.ReasoningTokens,
		&archived,
		&ss.BilledInputTokens, &ss.BilledCachedTokens, &ss.BilledOutputTokens,
		&ss.CostUSD, &known,
	)
	ss.Archived = archived != 0
	ss.CostKnown = known != 0
	return err
}

// listSessionsWhere runs the canonical session-list SELECT with an extra WHERE
// fragment (e.g. "WHERE archived = 0") and an optional LIMIT. The column list,
// scan order, and ORDER BY live here so ListSessions and ListArchivedSessions
// cannot drift apart.
func (s *Store) listSessionsWhere(where string, limit int) ([]SessionSummary, error) {
	q := "SELECT " + sessionColumns + " FROM sessions " +
		where + " ORDER BY updated_at DESC"
	args := []any{}
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.DB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionSummary
	for rows.Next() {
		var ss SessionSummary
		if err := scanSession(rows, &ss); err != nil {
			return nil, err
		}
		out = append(out, ss)
	}
	return out, rows.Err()
}

// ListSessions returns ACTIVE sessions (archived = 0) ordered by most-recently-
// updated first. A limit <= 0 returns all active rows. Archived sessions are
// invisible here — use ListArchivedSessions to enumerate them (for /unarchive).
func (s *Store) ListSessions(limit int) ([]SessionSummary, error) {
	return s.listSessionsWhere("WHERE archived = 0", limit)
}

// ListArchivedSessions returns ARCHIVED sessions (archived = 1) ordered by most-
// recently-updated first, so the user can discover IDs to unarchive. A limit <= 0
// returns all archived rows.
func (s *Store) ListArchivedSessions(limit int) ([]SessionSummary, error) {
	return s.listSessionsWhere("WHERE archived = 1", limit)
}

// GetSession returns the session with the given id (active OR archived), or
// (nil, nil) if not found.
func (s *Store) GetSession(id string) (*SessionSummary, error) {
	var ss SessionSummary
	err := scanSession(s.DB.QueryRow("SELECT "+sessionColumns+" FROM sessions WHERE id = ?", id), &ss)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &ss, nil
}
