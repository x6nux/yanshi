package store

import (
	"database/sql"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
)

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
// fragment (e.g. "WHERE archived = 0"), an optional keyset cursor, and an
// optional LIMIT. The column list, scan order, and ORDER BY live here so
// ListSessions, ListArchivedSessions and ListSessionsPage cannot drift apart —
// a paged read that ordered rows differently from the unpaged one would page
// through a sequence no caller ever sees.
//
// The ORDER BY carries `id DESC` as a tie-break, and that is load-bearing
// rather than cosmetic: updated_at is stored as time.Now().Unix(), so any two
// sessions created or touched within the same second collide, and a cursor
// over a non-total order cannot express "strictly after this row". Drop the
// tie-break and the ORDER BY stops agreeing with the row-value predicate
// below, which still compares ids — measured on a five-session walk, that
// serves one session twice and never returns two others at all.
// TestListSessions_CursorPaginationIsStable creates its five fixtures inside
// one second precisely so it goes red if this is dropped.
func (s *Store) listSessionsWhere(where string, limit int, after *sessionCursor) ([]SessionSummary, error) {
	q := "SELECT " + sessionColumns + " FROM sessions " + where
	args := []any{}
	if after != nil {
		// Row-value comparison (SQLite 3.15+) states "strictly past this
		// position in the ORDER BY" as one predicate. Both callers pass a
		// non-empty WHERE fragment, so AND is always the correct joiner.
		q += " AND (updated_at, id) < (?, ?)"
		args = append(args, after.UpdatedAt, after.ID)
	}
	q += " ORDER BY updated_at DESC, id DESC"
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
	return s.listSessionsWhere("WHERE archived = 0", limit, nil)
}

// ListArchivedSessions returns ARCHIVED sessions (archived = 1) ordered by most-
// recently-updated first, so the user can discover IDs to unarchive. A limit <= 0
// returns all archived rows.
func (s *Store) ListArchivedSessions(limit int) ([]SessionSummary, error) {
	return s.listSessionsWhere("WHERE archived = 1", limit, nil)
}

// SessionPage is one page of ListSessionsPage plus the token that reaches the
// next one.
type SessionPage struct {
	Sessions []SessionSummary
	// NextCursor is empty exactly when this page is the last one, so a caller
	// loops until it comes back empty rather than until a page comes back
	// short. It is opaque: its encoding is this package's business and callers
	// must only ever hand it back unmodified.
	NextCursor string
}

// sessionCursor is a decoded position in the session list: the sort key of the
// last row already delivered. Both components are needed because updated_at
// alone is not unique — see listSessionsWhere for why that matters.
type sessionCursor struct {
	UpdatedAt int64
	ID        string
}

// encode renders the cursor as the opaque token handed to callers. The
// separator is safe unqualified because session ids come from newID(), which
// emits hex.
func (c sessionCursor) encode() string {
	return base64.RawURLEncoding.EncodeToString(
		[]byte(strconv.FormatInt(c.UpdatedAt, 10) + "|" + c.ID))
}

// decodeSessionCursor reverses encode. A malformed token is an error rather
// than a silent fall back to the first page: a client that corrupts its cursor
// would otherwise restart the list without ever being told, and would read the
// newest sessions again believing it was making progress.
func decodeSessionCursor(tok string) (*sessionCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil {
		return nil, fmt.Errorf("store: bad session cursor: %w", err)
	}
	ts, id, ok := strings.Cut(string(raw), "|")
	if !ok || id == "" {
		return nil, fmt.Errorf("store: bad session cursor: malformed payload")
	}
	n, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("store: bad session cursor: %w", err)
	}
	return &sessionCursor{UpdatedAt: n, ID: id}, nil
}

// ListSessionsPage returns one page of ACTIVE sessions in the same order as
// ListSessions, starting just past cursor (empty cursor = the first page).
//
// This is keyset paging, not OFFSET paging, and the difference is visible to
// users rather than theoretical. The list is ordered by updated_at DESC, so
// every message appended during a paging walk moves a session to the front and
// shifts every OFFSET by one — the row that was about to start page 2 slides
// back onto page 1 and the reader sees it twice, while some other row is
// skipped entirely. A cursor names a position in the ordering instead of
// counting rows before it, so concurrent inserts and updates can only add rows
// the walk has already passed.
//
// limit shares clampLimit with the message log: an unspecified page becomes
// DefaultMessagePageSize and an over-large one is capped at MaxMessagePageSize.
// The bounds are the same numbers for the same reason, so they are not
// duplicated under session-flavoured names.
func (s *Store) ListSessionsPage(cursor string, limit int) (SessionPage, error) {
	var after *sessionCursor
	if cursor != "" {
		c, err := decodeSessionCursor(cursor)
		if err != nil {
			return SessionPage{}, err
		}
		after = c
	}
	n := clampLimit(limit)
	// Over-fetch one row: it answers "is there more" without a second query,
	// and without the alternative of always emitting a cursor, which hands the
	// caller one guaranteed empty page at the end of every walk.
	rows, err := s.listSessionsWhere("WHERE archived = 0", n+1, after)
	if err != nil {
		return SessionPage{}, err
	}
	p := SessionPage{Sessions: rows}
	if len(rows) > n {
		p.Sessions = rows[:n]
		last := p.Sessions[n-1]
		p.NextCursor = sessionCursor{UpdatedAt: last.UpdatedAt, ID: last.ID}.encode()
	}
	return p, nil
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
