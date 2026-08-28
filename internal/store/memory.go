package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// Memory is a stored memory record.
//
// SessionID / AgentID (C14) are the retrieval dimensions. They hold the empty
// string for memories written before the columns existed and for any writer
// that does not know its own scope; MemoryFilter treats an empty dimension as
// "do not filter", so those rows remain visible to an unscoped search and
// invisible to a scoped one.
//
// DistilledFrom / SupersededBy / DistilledAt (C13) are the consolidation
// lineage. A distillation NEVER deletes: it writes one new row whose
// DistilledFrom lists the ids it replaces, and stamps those rows'
// SupersededBy with the new id. Retrieval hides superseded rows by default,
// so the table reads as if they were merged, while the bytes are still there
// for an audit or an undo. See MemoryDistillation.
type Memory struct {
	ID        string
	Kind      string
	Content   string
	SessionID string
	AgentID   string
	CreatedAt int64

	// DistilledFrom holds the ids this row was consolidated from, oldest
	// first. Empty for an ordinary memory.
	DistilledFrom []string
	// SupersededBy is the id of the distilled row that replaced this one, or
	// '' while the row is current.
	SupersededBy string
	// DistilledAt is the unix time the row was produced by a distillation, or
	// 0 for an ordinary memory.
	DistilledAt int64
}

// MemoryFilter narrows a memory query to one session and/or one agent.
//
// The zero value means "search across every dimension", which is the
// backwards-compatible behaviour: sub-agents and the goalloop share one table,
// and a default that suddenly scoped every existing query would make previously
// findable memories disappear without a single error. Narrowing is opt-in.
type MemoryFilter struct {
	SessionID string
	AgentID   string

	// IncludeSuperseded returns rows a distillation has replaced.
	//
	// It defaults to FALSE — superseded rows are hidden — because a
	// consolidation that leaves both the four originals and their merged
	// replacement in every result has made retrieval worse, not better, which
	// is the opposite of why C13 exists. It is a flag rather than a hard rule
	// because the originals are kept precisely so somebody can look at them:
	// an audit of what a distillation did, or a recovery from a bad one, needs
	// exactly this switch. Before any distillation has run no row is
	// superseded, so the default changes nothing for an existing database.
	IncludeSuperseded bool
}

// where renders the filter as an SQL fragment plus its arguments, using the
// given column alias prefix (e.g. "m." when the query joins). Empty dimension
// fields contribute nothing, so the zero filter yields only the
// hide-superseded clause.
func (f MemoryFilter) where(alias string) (string, []any) {
	var sb strings.Builder
	var args []any
	if f.SessionID != "" {
		sb.WriteString(" AND " + alias + "session_id = ?")
		args = append(args, f.SessionID)
	}
	if f.AgentID != "" {
		sb.WriteString(" AND " + alias + "agent_id = ?")
		args = append(args, f.AgentID)
	}
	if !f.IncludeSuperseded {
		sb.WriteString(" AND " + alias + "superseded_by = ''")
	}
	return sb.String(), args
}

// memoryColumns is the canonical SELECT list; every reader shares it so a new
// dimension cannot be added to one query and forgotten in another.
const memoryColumns = "id, kind, content, session_id, agent_id, created_at, " +
	"distilled_from, superseded_by, distilled_at"

// WriteMemory inserts a memory with no retrieval dimensions and returns its id.
// Equivalent to WriteMemoryScoped with a zero MemoryFilter; kept because the
// dimensions are genuinely unknown on some paths and inventing one would be
// worse than recording none.
func (s *Store) WriteMemory(kind, content string) (string, error) {
	return s.WriteMemoryScoped(kind, content, MemoryFilter{})
}

// WriteMemoryScoped inserts a memory tagged with its retrieval dimensions and
// returns its id. Empty dimension fields are stored as the empty string — the
// same value pre-C14 rows carry — so an unscoped search still finds the row.
func (s *Store) WriteMemoryScoped(kind, content string, dims MemoryFilter) (string, error) {
	return s.writeMemory(kind, content, dims, "", 0)
}

// WriteMemoryFromSession is WriteMemoryScoped plus provenance (W-D-07): the row
// records WHICH log slice it was derived from, so it can be traced back later.
//
// The source is derived here rather than taken as a parameter because every
// writer would compute the same thing and one of them would eventually compute
// it differently. It is dims.SessionID's CURRENT CONTEXT WINDOW START — the
// first row of the slice the writer was looking at, which is what ProjectWindow
// hands both production writers (the upkeep extraction worker reads the window
// directly; memory_write is called by a model whose prompt IS that window).
//
// A memory written with no session (the SSE path, a bare sub-agent) records no
// provenance and behaves exactly like WriteMemoryScoped — inventing a source
// would be worse than recording none, for the reason memoryDims gives about
// inventing a dimension.
//
// A FAILURE TO READ THE BOUNDARY IS NOT A FAILURE TO WRITE THE MEMORY. The
// memory is the asset; provenance is metadata about it. Losing the note because
// the event log could not be read would trade the thing being recorded for the
// record of where it came from.
func (s *Store) WriteMemoryFromSession(kind, content string, dims MemoryFilter) (string, error) {
	if dims.SessionID == "" {
		return s.writeMemory(kind, content, dims, "", 0)
	}
	b, err := s.boundary(dims.SessionID)
	if err != nil {
		slog.Warn("store: could not record memory provenance; writing the memory without it",
			"session", dims.SessionID, "error", err)
		return s.writeMemory(kind, content, dims, "", 0)
	}
	return s.writeMemory(kind, content, dims, dims.SessionID, b.HiddenSeq)
}

// writeMemory is the single INSERT behind both writers, so a new column cannot
// be added to one and forgotten in the other.
func (s *Store) writeMemory(
	kind, content string, dims MemoryFilter, srcSession string, srcSeq int,
) (string, error) {
	id := newID()
	now := time.Now().Unix()
	err := s.WriteTx(context.Background(), func(tx *sql.Tx) error {
		_, e := tx.Exec(
			`INSERT INTO memories
			   (id, kind, content, session_id, agent_id, created_at, source_session_id, source_seq)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			id, kind, content, dims.SessionID, dims.AgentID, now, srcSession, srcSeq,
		)
		return e
	})
	if err != nil {
		return "", err
	}
	return id, nil
}

// ErrNoMemorySource reports that a memory carries no recorded provenance.
//
// A distinct error rather than an empty slice because the two answers demand
// different things of a caller: "nobody wrote down where this came from" (every
// pre-W-D-07 row, and every memory written without a session) is permanent,
// while an empty result from a resolved source means the rows themselves are
// gone and something removed history it should not have.
var ErrNoMemorySource = errors.New("store: memory has no recorded source")

// MemorySource returns the log slice a memory was derived from (W-D-07), or
// ErrNoMemorySource when none was recorded.
//
// IT RESOLVES THROUGH messagesInWindow, which is ProjectWindow's own reader, so
// provenance survives everything that reader already survives — in particular
// W-D-04 compression, where the rows have been deleted and live inside a gzip
// blob. That is the second half of the acceptance and it is bought by reusing
// the path rather than writing a second one: a private SELECT over `messages`
// would return nothing for an archived session and look like "the source is
// gone" instead of "the source moved".
//
// The slice is the log FROM the recorded position, not a fixed excerpt, so it
// grows as the session continues. That is deliberate: the durable fact is the
// POSITION, and rendering it as "the conversation from here on" is the only
// reading that stays true without a second column to pin the far end.
func (s *Store) MemorySource(memoryID string) ([]Message, error) {
	var sess string
	var seq int
	err := s.DB.QueryRow(
		"SELECT source_session_id, source_seq FROM memories WHERE id = ?", memoryID,
	).Scan(&sess, &seq)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("store: memory %q not found", memoryID)
	}
	if err != nil {
		return nil, err
	}
	if sess == "" {
		return nil, ErrNoMemorySource
	}
	return s.messagesInWindow(sess, seq, nil)
}

// ClearMemories deletes every memory matching dims and returns how many it
// removed (W-D-12). A ZERO FILTER CLEARS THE WHOLE TABLE — which is the point,
// and why the callers of this function all sit behind an explicit confirmation.
//
// IT IGNORES IncludeSuperseded AND ALWAYS CLEARS SUPERSEDED ROWS TOO. The
// default filter hides them from retrieval, so honouring it here would leave
// every consolidated original in the table after a "clear": invisible to
// search, still on disk, and still returned by the audit switch that exists to
// read them. "Cleared" has to mean gone.
//
// Deleting rather than superseding, unlike ApplyDistillation: a wipe that left
// the rows readable through the audit switch would be a rename.
//
// IT IS NOT AN ERASURE, AND THE DOC HERE USED TO SAY IT WAS. The earlier
// wording — "the user asking for it is asking for the bytes to stop existing" —
// was falsified by the very next change in its own work package: W-D-06 gzips
// the WHOLE memories table into `checkpoints.memories` on every /checkpoint
// create and on every /checkpoint restore, so a cleared memory is still on disk
// in those blobs and comes back verbatim from a plain
// `/checkpoint restore <id> memory yes`. Measured on a memory whose content was
// an API key.
//
// PURGING THOSE BLOBS WAS TRIED AND REJECTED, for two reasons that both point
// the same way. It would delete the capability store.restoreMemoriesTx exists
// for and names in its own doc — "undoing an accidental wipe" is one of the two
// reasons anybody restores this dimension, and a clear that shredded the
// snapshots would leave a mistyped /memory-clear unrecoverable. And it would
// still not deliver erasure: the text a memory was distilled FROM is in
// `messages`, which no clear has ever touched.
//
// So the honest contract is the narrow one: the rows leave `memories` and stop
// being retrievable. Callers that need the user to know the difference say so —
// see the reply handleClearMemories writes when checkpoints exist. Erasure of
// secret text is secrets redaction's job, on the way in.
func (s *Store) ClearMemories(dims MemoryFilter) (int, error) {
	dims.IncludeSuperseded = true
	cond, args := dims.where("")
	var deleted int
	err := s.WriteTx(context.Background(), func(tx *sql.Tx) error {
		res, e := tx.Exec("DELETE FROM memories WHERE 1=1"+cond, args...)
		if e != nil {
			return fmt.Errorf("store: clear memories: %w", e)
		}
		n, e := res.RowsAffected()
		if e != nil {
			return e
		}
		deleted = int(n)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return deleted, nil
}

// SearchMemory runs an FTS5 full-text query over memory contents, returning up
// to limit most-relevant matches (newest first), across all dimensions.
func (s *Store) SearchMemory(query string, limit int) ([]Memory, error) {
	return s.SearchMemoryScoped(query, limit, MemoryFilter{})
}

// SearchMemoryScoped is SearchMemory narrowed to a session and/or agent. A zero
// filter is identical to SearchMemory.
func (s *Store) SearchMemoryScoped(query string, limit int, dims MemoryFilter) ([]Memory, error) {
	hits, err := s.SearchMemoryRanked(query, limit, dims)
	if err != nil {
		return nil, err
	}
	out := make([]Memory, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.Memory)
	}
	return out, nil
}

// MemoryHit is a memory plus the FTS5 relevance score of the query that found
// it.
//
// The score exists to ORDER hits, and the caller taking a top-K needs that
// ordering: everything below K is discarded unseen, so ranking by date would
// discard the best match for being old.
//
// IT IS NOT A USABLE ABSOLUTE THRESHOLD, and that was measured rather than
// assumed. bm25's IDF term is corpus-relative, so on a small memory table
// every score — including a perfect single-term match — sits a whisker below
// zero: a two-row table returned -1e-06 for the obviously correct hit, while
// a large table returns scores in the single digits for the same quality of
// match. Any constant drawn across that is a fit to one table size, and the
// direction it fails in is the bad one (a new install has few memories, scores
// near zero, and an absolute floor suppresses every correct recall on exactly
// the tables where each memory matters most). A caller deciding whether a hit
// is worth acting on needs a corpus-independent criterion; see
// tools.AutoRecallHits.
type MemoryHit struct {
	Memory
	// Score is SQLite's bm25() value: MORE NEGATIVE IS A BETTER MATCH. The
	// sign is not normalised away, because normalising would invent a scale
	// this package cannot justify.
	Score float64
}

// SearchMemoryRanked is SearchMemoryScoped with the relevance score attached,
// ordered BY RELEVANCE rather than by recency.
//
// The ordering differs from SearchMemoryScoped on purpose. A human (or a model)
// reading a memory_search result is scanning a short list where recency is a
// useful tiebreak; a caller taking the top K for automatic injection needs the
// K most relevant, because everything below K is discarded unseen and ordering
// by date would discard the best match for being old — which is precisely the
// failure C13 exists to correct on the write side.
// W-D-03: this is also where a retrieval is COUNTED, and the dispatch below is
// arranged so both the FTS and the CJK branch pass through the single exit that
// does it. The obvious shape — an early `return s.searchMemoryCJK(...)` — was
// the first version and it left every Chinese search uncounted, i.e. every
// memory in this repo's own working language would have looked unused to the
// quota and been pruned first.
func (s *Store) SearchMemoryRanked(query string, limit int, dims MemoryFilter) ([]MemoryHit, error) {
	hits, err := s.searchMemoryRanked(query, limit, dims)
	if err != nil {
		return nil, err
	}
	s.markMemoriesUsed(memoryIDs(hits))
	return hits, nil
}

// searchMemoryRanked is SearchMemoryRanked without the use accounting, so the
// two retrieval backends have one place to return through.
func (s *Store) searchMemoryRanked(query string, limit int, dims MemoryFilter) ([]MemoryHit, error) {
	if limit <= 0 {
		limit = 10
	}
	if hasCJK(query) {
		return s.searchMemoryCJK(query, limit, dims)
	}
	cond, args := dims.where("m.")
	q := `SELECT ` + prefixed(memoryColumns, "m.") + `, bm25(memories_fts)
	      FROM memories_fts f JOIN memories m ON m.rowid = f.rowid
	      WHERE memories_fts MATCH ?` + cond + `
	      ORDER BY bm25(memories_fts) ASC, m.created_at DESC LIMIT ?`
	all := append([]any{query}, args...)
	all = append(all, limit)
	rows, err := s.DB.Query(q, all...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MemoryHit
	for rows.Next() {
		var h MemoryHit
		var from string
		if err := rows.Scan(&h.ID, &h.Kind, &h.Content, &h.SessionID, &h.AgentID,
			&h.CreatedAt, &from, &h.SupersededBy, &h.DistilledAt, &h.Score); err != nil {
			return nil, err
		}
		h.DistilledFrom = splitIDs(from)
		out = append(out, h)
	}
	return out, rows.Err()
}

// searchMemoryCJK is the CJK fallback path for SearchMemoryRanked, same cause
// and same fix as searchMessagesCJK — memories_fts does not even carry a
// tokenize= clause, so it falls back to the default unicode61 and equally
// fails to segment Chinese words.
//
// Score is always 0: bm25 does not exist on this path, and MemoryHit.Score's
// doc comment already states it exists only to ORDER and is not a usable
// absolute threshold. Fabricating a score would let a caller further up the
// stack mistake it for a real relevance judgement. Ordering falls back to
// created_at DESC instead.
//
// query is FTS5 MATCH syntax, not a literal LIKE pattern. This matters
// concretely: memory_autorecall's ftsQuery renders queries as `"term1" OR
// "term2"`, and that string reaching hasCJK routes it straight into this
// function. Matching it as one literal substring (the original version of
// this fallback did) requires a row to contain the quote characters and the
// word OR, which no real memory ever does — memory_autorecall was silently
// dead in Chinese even after this fallback existed. parseFTSTerms recovers
// the OR'd terms and likeAnyTermClause matches a row if any of them is
// present, which is the LIKE-side equivalent of MATCH's OR.
func (s *Store) searchMemoryCJK(query string, limit int, dims MemoryFilter) ([]MemoryHit, error) {
	if limit <= 0 {
		limit = 10
	}
	if limit > maxCJKFallbackRows {
		limit = maxCJKFallbackRows
	}
	terms := parseFTSTerms(query)
	clause, likeArgs := likeAnyTermClause([]string{"m.content"}, terms)
	cond, condArgs := dims.where("m.")
	q := `SELECT ` + prefixed(memoryColumns, "m.") + `
	      FROM memories m
	      WHERE (` + clause + `)` + cond + `
	      ORDER BY m.created_at DESC LIMIT ?`
	all := append([]any{}, likeArgs...)
	all = append(all, condArgs...)
	all = append(all, limit)
	rows, err := s.DB.Query(q, all...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MemoryHit
	for rows.Next() {
		var h MemoryHit
		var from string
		if err := rows.Scan(&h.ID, &h.Kind, &h.Content, &h.SessionID, &h.AgentID,
			&h.CreatedAt, &from, &h.SupersededBy, &h.DistilledAt); err != nil {
			return nil, err
		}
		h.DistilledFrom = splitIDs(from)
		out = append(out, h)
	}
	return out, rows.Err()
}

// RecallMemory returns up to limit most-recent memories across all dimensions.
func (s *Store) RecallMemory(limit int) ([]Memory, error) {
	return s.RecallMemoryScoped(limit, MemoryFilter{})
}

// RecallMemoryScoped is RecallMemory narrowed to a session and/or agent. A zero
// filter is identical to RecallMemory.
func (s *Store) RecallMemoryScoped(limit int, dims MemoryFilter) ([]Memory, error) {
	if limit <= 0 {
		limit = 10
	}
	cond, args := dims.where("")
	q := "SELECT " + memoryColumns + " FROM memories WHERE 1=1" + cond +
		" ORDER BY created_at DESC LIMIT ?"
	all := append([]any{}, args...)
	all = append(all, limit)
	rows, err := s.DB.Query(q, all...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out, err := scanMemories(rows)
	if err != nil {
		return nil, err
	}
	// W-D-03: a recall is a use, exactly like a search. Counting only searches
	// would make memory_recall's results look untouched and prune them first.
	ids := make([]string, 0, len(out))
	for _, m := range out {
		ids = append(ids, m.ID)
	}
	s.markMemoriesUsed(ids)
	return out, nil
}

func scanMemories(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}) ([]Memory, error) {
	var out []Memory
	for rows.Next() {
		var m Memory
		var from string
		if err := rows.Scan(&m.ID, &m.Kind, &m.Content, &m.SessionID, &m.AgentID,
			&m.CreatedAt, &from, &m.SupersededBy, &m.DistilledAt); err != nil {
			return nil, err
		}
		m.DistilledFrom = splitIDs(from)
		out = append(out, m)
	}
	return out, rows.Err()
}

// idSeparator joins the distilled-from id list into one column.
//
// A delimited string rather than a join table: the list is read as a unit,
// never queried by member, and never longer than MaxDistillInputs — a table
// would add a migration, a second write inside the transaction and a join to
// every read, all to model a field nothing filters on.
const idSeparator = ","

// joinIDs renders an id list for storage. Empty in, empty out — so an
// ordinary memory stores the empty string rather than a delimiter-only string
// that splitIDs would have to special-case.
func joinIDs(ids []string) string { return strings.Join(ids, idSeparator) }

// splitIDs parses a stored id list, returning nil for the empty column.
func splitIDs(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, idSeparator)
}
