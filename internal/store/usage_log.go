// internal/store/usage_log.go
//
// M10: the per-call token-usage ledger.
//
// The `sessions` table already carries token counters, but they are a RUNNING
// TOTAL per session, overwritten on every turn. That answers "what has this
// conversation cost" and destroys everything else in the same write: after a
// goal loop runs overnight across three models, "which model burned what, on
// which day" is unanswerable — not because the numbers were never observed, but
// because each one was folded into a sum the instant it arrived.
//
// This table is append-only and keeps every call, so the aggregate can be
// recomputed for any grouping later instead of being fixed at write time.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// UsageEvent is one persisted model call.
//
// Cached is stored alongside — not subtracted from — PromptTokens, because the
// two are billed at different rates and the provider reports them that way. A
// consumer wanting the billable prompt computes Prompt-Cached; one wanting
// cache effectiveness needs both, and a schema that stored only the difference
// could not answer the second question at all.
type UsageEvent struct {
	// ID is the autoincrement row id.
	ID int64
	// TS is the Unix second the call completed.
	TS int64
	// Provider is the adapter kind ("openai", "anthropic", …), possibly "".
	Provider string
	// Model is the registry key the call ran on. Required.
	Model string
	// SessionID attributes the call to a conversation, or "" when none was in
	// scope (a goal-loop judge call, a compaction summary).
	SessionID string
	// PromptTokens / CompletionTokens are the provider's own counts.
	PromptTokens     int
	CompletionTokens int
	// CachedTokens is the prompt-cache hit portion of PromptTokens.
	CachedTokens int
	// ReasoningTokens is the thinking portion of the completion.
	ReasoningTokens int
	// CacheHit records whether the provider reported any cached tokens. Stored
	// as its own column rather than derived from CachedTokens>0 at query time
	// so a "how often did caching work" query is an index scan and not a
	// full-table expression.
	CacheHit bool
}

// AppendUsage persists one usage event.
//
// Model is required — a row that cannot name the model it billed is not an
// accounting record, and a NULL-model row would silently corrupt every
// per-model aggregate that follows.
//
// Deliberately NOT fatal to the caller and deliberately outside any caller
// transaction: this runs after a model call has already succeeded, and a full
// disk must not be able to retroactively fail a turn that produced an answer.
// Callers log and continue.
func (s *Store) AppendUsage(ev UsageEvent) error {
	if ev.Model == "" {
		return fmt.Errorf("store: usage event: empty model")
	}
	ts := ev.TS
	if ts == 0 {
		ts = time.Now().Unix()
	}
	hit := 0
	if ev.CacheHit {
		hit = 1
	}
	return s.WriteTx(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`INSERT INTO usage_log
			   (ts, provider, model, session_id, prompt_tokens, completion_tokens,
			    cached_tokens, reasoning_tokens, cache_hit)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			ts, ev.Provider, ev.Model, ev.SessionID,
			ev.PromptTokens, ev.CompletionTokens, ev.CachedTokens, ev.ReasoningTokens, hit,
		)
		return err
	})
}

// UsageQuery filters the usage ledger. The zero value returns the most recent
// DefaultUsagePageSize events across every model and session.
//
// Since is inclusive and Until exclusive (both Unix seconds); zero means
// unbounded on that end. The half-open interval is what makes adjacent day
// buckets tile without double-counting the row that lands exactly on midnight.
type UsageQuery struct {
	Model     string
	Provider  string
	SessionID string
	Since     int64
	Until     int64
	Limit     int
}

// DefaultUsagePageSize bounds an unspecified UsageQuery.Limit.
const DefaultUsagePageSize = 200

// MaxUsagePageSize bounds an over-specified UsageQuery.Limit.
const MaxUsagePageSize = 5000

// clampLimit applies the page-size bounds.
func (q UsageQuery) clampLimit() int {
	limit := q.Limit
	if limit <= 0 {
		limit = DefaultUsagePageSize
	}
	if limit > MaxUsagePageSize {
		limit = MaxUsagePageSize
	}
	return limit
}

// where builds the shared filter clause and its arguments. Extracted because
// QueryUsage and AggregateUsage must filter identically — a divergence would
// make the list and the totals on the same screen disagree, which is the single
// most confusing thing an accounting UI can do.
func (q UsageQuery) where() (string, []any) {
	var sb strings.Builder
	var args []any
	eq := func(col, val string) {
		if val != "" {
			sb.WriteString(" AND " + col + " = ?")
			args = append(args, val)
		}
	}
	eq("model", q.Model)
	eq("provider", q.Provider)
	eq("session_id", q.SessionID)
	if q.Since > 0 {
		sb.WriteString(" AND ts >= ?")
		args = append(args, q.Since)
	}
	if q.Until > 0 {
		sb.WriteString(" AND ts < ?")
		args = append(args, q.Until)
	}
	return sb.String(), args
}

// QueryUsage returns matching events, newest first.
func (s *Store) QueryUsage(q UsageQuery) ([]UsageEvent, error) {
	clause, args := q.where()
	args = append(args, q.clampLimit())
	rows, err := s.DB.Query(
		`SELECT id, ts, provider, model, session_id, prompt_tokens, completion_tokens,
		        cached_tokens, reasoning_tokens, cache_hit
		 FROM usage_log WHERE 1=1`+clause+` ORDER BY ts DESC, id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UsageEvent
	for rows.Next() {
		var e UsageEvent
		var hit int
		if err := rows.Scan(&e.ID, &e.TS, &e.Provider, &e.Model, &e.SessionID,
			&e.PromptTokens, &e.CompletionTokens, &e.CachedTokens, &e.ReasoningTokens, &hit); err != nil {
			return nil, err
		}
		e.CacheHit = hit != 0
		out = append(out, e)
	}
	return out, rows.Err()
}

// UsageBucket is one row of a time-series aggregate.
type UsageBucket struct {
	// Day is the UTC calendar day as "YYYY-MM-DD".
	//
	// UTC and not local time, because the ledger is queried by operators and by
	// the goal loop's reporting from machines that need not share a timezone
	// with the one that wrote the rows — and a bucket boundary that moves with
	// the reader makes two reports of the same night disagree.
	Day string
	// Model is the model the bucket aggregates. Empty when grouping by day
	// alone.
	Model string
	// Provider is the provider serving Model, or "" when a model was served by
	// more than one within the bucket.
	Provider string
	// Calls is how many events fell in the bucket.
	Calls int
	// PromptTokens / CompletionTokens / CachedTokens / ReasoningTokens are the
	// bucket sums.
	PromptTokens     int
	CompletionTokens int
	CachedTokens     int
	ReasoningTokens  int
	// CacheHits is how many of Calls reported cached tokens.
	CacheHits int
}

// AggregateUsage returns per-day, per-model totals matching q, oldest day
// first.
//
// The grouping is fixed rather than parameterised: day-by-model is the shape
// every question that motivated M10 takes ("which day did the goal loop burn
// the budget", "which model is the expensive one"), and a generic GROUP BY
// builder would put column names from the caller into SQL text for no gain.
//
// Provider is reported per bucket via MIN(), which is exact when a model is
// served by one provider (the normal case) and arbitrary-but-stable when it is
// not; the column is a convenience label, and the authoritative provider filter
// is UsageQuery.Provider.
func (s *Store) AggregateUsage(q UsageQuery) ([]UsageBucket, error) {
	clause, args := q.where()
	rows, err := s.DB.Query(
		`SELECT strftime('%Y-%m-%d', ts, 'unixepoch') AS day,
		        model,
		        MIN(provider),
		        COUNT(*),
		        COALESCE(SUM(prompt_tokens), 0),
		        COALESCE(SUM(completion_tokens), 0),
		        COALESCE(SUM(cached_tokens), 0),
		        COALESCE(SUM(reasoning_tokens), 0),
		        COALESCE(SUM(cache_hit), 0)
		 FROM usage_log WHERE 1=1`+clause+`
		 GROUP BY day, model
		 ORDER BY day ASC, model ASC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UsageBucket
	for rows.Next() {
		var b UsageBucket
		if err := rows.Scan(&b.Day, &b.Model, &b.Provider, &b.Calls,
			&b.PromptTokens, &b.CompletionTokens, &b.CachedTokens, &b.ReasoningTokens,
			&b.CacheHits); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
