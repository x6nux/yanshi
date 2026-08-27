package store

import (
	"testing"
	"time"
)

// day returns the Unix second for a UTC calendar date at the given hour.
func day(t *testing.T, y int, m time.Month, d, hour int) int64 {
	t.Helper()
	return time.Date(y, m, d, hour, 0, 0, 0, time.UTC).Unix()
}

// TestAppendUsageRequiresModel pins the one required field: a row that cannot
// name the model it billed would silently corrupt every per-model aggregate.
func TestAppendUsageRequiresModel(t *testing.T) {
	s := openTestStore(t)
	if err := s.AppendUsage(UsageEvent{PromptTokens: 10}); err == nil {
		t.Fatal("AppendUsage accepted an event with no model")
	}
}

// TestAppendUsageFillsTimestamp pins that a caller who omits TS still gets a
// dated row, so no event can land outside every time bucket.
func TestAppendUsageFillsTimestamp(t *testing.T) {
	s := openTestStore(t)
	before := time.Now().Unix()
	if err := s.AppendUsage(UsageEvent{Model: "m", PromptTokens: 1}); err != nil {
		t.Fatalf("AppendUsage: %v", err)
	}
	got, err := s.QueryUsage(UsageQuery{})
	if err != nil {
		t.Fatalf("QueryUsage: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1", len(got))
	}
	if got[0].TS < before {
		t.Errorf("TS = %d, want at least %d", got[0].TS, before)
	}
}

// TestUsageRoundTrip pins every column through a write and a read, including
// the cache_hit boolean that is stored as an integer.
func TestUsageRoundTrip(t *testing.T) {
	s := openTestStore(t)
	want := UsageEvent{
		TS:               day(t, 2026, time.August, 8, 12),
		Provider:         "anthropic",
		Model:            "claude-sonnet-5",
		SessionID:        "sess-1",
		PromptTokens:     1000,
		CompletionTokens: 200,
		CachedTokens:     400,
		ReasoningTokens:  50,
		CacheHit:         true,
	}
	if err := s.AppendUsage(want); err != nil {
		t.Fatalf("AppendUsage: %v", err)
	}
	got, err := s.QueryUsage(UsageQuery{})
	if err != nil {
		t.Fatalf("QueryUsage: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1", len(got))
	}
	g := got[0]
	if g.ID == 0 {
		t.Error("ID was not assigned")
	}
	g.ID = 0
	if g != want {
		t.Errorf("round trip mismatch:\n got %+v\nwant %+v", g, want)
	}
}

// seedUsage writes a fixed multi-day, multi-model fixture.
func seedUsage(t *testing.T, s *Store) {
	t.Helper()
	events := []UsageEvent{
		{TS: day(t, 2026, time.August, 6, 9), Provider: "openai", Model: "gpt-4o",
			SessionID: "s1", PromptTokens: 100, CompletionTokens: 10},
		{TS: day(t, 2026, time.August, 6, 21), Provider: "openai", Model: "gpt-4o",
			SessionID: "s1", PromptTokens: 200, CompletionTokens: 20, CachedTokens: 50, CacheHit: true},
		{TS: day(t, 2026, time.August, 6, 22), Provider: "anthropic", Model: "claude-sonnet-5",
			SessionID: "s2", PromptTokens: 300, CompletionTokens: 30, ReasoningTokens: 7},
		{TS: day(t, 2026, time.August, 7, 3), Provider: "openai", Model: "gpt-4o",
			SessionID: "s2", PromptTokens: 400, CompletionTokens: 40, CachedTokens: 100, CacheHit: true},
	}
	for _, e := range events {
		if err := s.AppendUsage(e); err != nil {
			t.Fatalf("AppendUsage: %v", err)
		}
	}
}

// TestQueryUsageFilters is the filter table.
func TestQueryUsageFilters(t *testing.T) {
	s := openTestStore(t)
	seedUsage(t, s)

	cases := []struct {
		name string
		q    UsageQuery
		want int
	}{
		{name: "no filter returns everything", q: UsageQuery{}, want: 4},
		{name: "by model", q: UsageQuery{Model: "gpt-4o"}, want: 3},
		{name: "by provider", q: UsageQuery{Provider: "anthropic"}, want: 1},
		{name: "by session", q: UsageQuery{SessionID: "s2"}, want: 2},
		{
			name: "since is inclusive",
			q:    UsageQuery{Since: day(t, 2026, time.August, 6, 21)},
			want: 3,
		},
		{
			name: "until is exclusive",
			q:    UsageQuery{Until: day(t, 2026, time.August, 6, 21)},
			want: 1,
		},
		{
			name: "a half-open day window",
			q: UsageQuery{
				Since: day(t, 2026, time.August, 6, 0),
				Until: day(t, 2026, time.August, 7, 0),
			},
			want: 3,
		},
		{name: "limit", q: UsageQuery{Limit: 2}, want: 2},
		{name: "an unmatched model", q: UsageQuery{Model: "nope"}, want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.QueryUsage(tc.q)
			if err != nil {
				t.Fatalf("QueryUsage: %v", err)
			}
			if len(got) != tc.want {
				t.Errorf("got %d rows, want %d", len(got), tc.want)
			}
		})
	}
}

// TestQueryUsageIsNewestFirst pins the ordering: the question that brings
// anyone to a usage log is almost always about the recent past.
func TestQueryUsageIsNewestFirst(t *testing.T) {
	s := openTestStore(t)
	seedUsage(t, s)
	got, err := s.QueryUsage(UsageQuery{})
	if err != nil {
		t.Fatalf("QueryUsage: %v", err)
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].TS < got[i].TS {
			t.Fatalf("row %d (ts %d) precedes a newer row (ts %d)", i-1, got[i-1].TS, got[i].TS)
		}
	}
}

// TestUsageLimitBounds pins both page-size clamps.
func TestUsageLimitBounds(t *testing.T) {
	cases := []struct {
		in, want int
	}{
		{0, DefaultUsagePageSize},
		{-1, DefaultUsagePageSize},
		{50, 50},
		{MaxUsagePageSize + 1, MaxUsagePageSize},
	}
	for _, tc := range cases {
		if got := (UsageQuery{Limit: tc.in}).clampLimit(); got != tc.want {
			t.Errorf("clampLimit(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestAggregateUsageByDayAndModel is the M10 aggregation assertion: the exact
// question the feature exists to answer.
func TestAggregateUsageByDayAndModel(t *testing.T) {
	s := openTestStore(t)
	seedUsage(t, s)

	got, err := s.AggregateUsage(UsageQuery{})
	if err != nil {
		t.Fatalf("AggregateUsage: %v", err)
	}
	want := []UsageBucket{
		{
			Day: "2026-08-06", Model: "claude-sonnet-5", Provider: "anthropic", Calls: 1,
			PromptTokens: 300, CompletionTokens: 30, ReasoningTokens: 7,
		},
		{
			Day: "2026-08-06", Model: "gpt-4o", Provider: "openai", Calls: 2,
			PromptTokens: 300, CompletionTokens: 30, CachedTokens: 50, CacheHits: 1,
		},
		{
			Day: "2026-08-07", Model: "gpt-4o", Provider: "openai", Calls: 1,
			PromptTokens: 400, CompletionTokens: 40, CachedTokens: 100, CacheHits: 1,
		},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d buckets, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("bucket %d:\n got %+v\nwant %+v", i, got[i], want[i])
		}
	}
}

// TestAggregateUsageRespectsFilters pins that the aggregate and the list filter
// identically — a divergence would make the totals and the rows on the same
// screen disagree.
func TestAggregateUsageRespectsFilters(t *testing.T) {
	s := openTestStore(t)
	seedUsage(t, s)

	q := UsageQuery{Model: "gpt-4o", Since: day(t, 2026, time.August, 6, 12)}
	rows, err := s.QueryUsage(q)
	if err != nil {
		t.Fatalf("QueryUsage: %v", err)
	}
	buckets, err := s.AggregateUsage(q)
	if err != nil {
		t.Fatalf("AggregateUsage: %v", err)
	}
	totalCalls, totalPrompt := 0, 0
	for _, b := range buckets {
		totalCalls += b.Calls
		totalPrompt += b.PromptTokens
	}
	if totalCalls != len(rows) {
		t.Errorf("aggregate counted %d calls, the list returned %d rows", totalCalls, len(rows))
	}
	rowPrompt := 0
	for _, r := range rows {
		rowPrompt += r.PromptTokens
	}
	if totalPrompt != rowPrompt {
		t.Errorf("aggregate prompt tokens %d, list sum %d", totalPrompt, rowPrompt)
	}
}

// TestAggregateUsageEmpty pins that an empty ledger aggregates to nothing
// rather than erroring.
func TestAggregateUsageEmpty(t *testing.T) {
	s := openTestStore(t)
	got, err := s.AggregateUsage(UsageQuery{})
	if err != nil {
		t.Fatalf("AggregateUsage: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d buckets from an empty ledger", len(got))
	}
}
