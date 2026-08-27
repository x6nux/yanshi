// internal/store/memory_distill_test.go
package store

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func seedMemories(t *testing.T, s *Store, bodies ...string) []string {
	t.Helper()
	ids := make([]string, 0, len(bodies))
	for _, b := range bodies {
		id, err := s.WriteMemory("note", b)
		require.NoError(t, err)
		ids = append(ids, id)
	}
	return ids
}

// TestApplyDistillation_NeverDeletesTheOriginals is C13's hard requirement.
// A merge is a rewrite of memory, and the only thing that makes a wrong one
// survivable is that the inputs are still there afterwards.
func TestApplyDistillation_NeverDeletesTheOriginals(t *testing.T) {
	s, _ := openTempStore(t)
	ids := seedMemories(t, s,
		"use pytest for the tests",
		"actually pytest with -x",
		"pytest runs from the repo root")

	newID, err := s.ApplyDistillation(MemoryDistillation{
		SourceIDs: ids,
		Content:   "run pytest -x from the repo root",
	})
	require.NoError(t, err)

	// Every original is still readable, byte for byte, under its own id.
	for i, id := range ids {
		got, err := s.MemoryByID(id)
		require.NoError(t, err, "original %d is gone; a bad distillation would be unrecoverable", i)
		require.Equal(t, newID, got.SupersededBy,
			"original %d does not point at what replaced it, so the merge cannot be traced", i)
	}
	// And the merged row names what it consumed.
	merged, err := s.MemoryByID(newID)
	require.NoError(t, err)
	require.ElementsMatch(t, ids, merged.DistilledFrom,
		"the merged row does not record its inputs; its provenance cannot be checked")
	require.Equal(t, DistilledKind, merged.Kind)
	require.NotZero(t, merged.DistilledAt)

	// Default retrieval sees only the merged row.
	cur, err := s.RecallMemory(50)
	require.NoError(t, err)
	require.Len(t, cur, 1, "the default query still returns superseded rows: %+v", cur)
	require.Equal(t, newID, cur[0].ID)

	// The audit path sees all four.
	all, err := s.RecallMemoryScoped(50, MemoryFilter{IncludeSuperseded: true})
	require.NoError(t, err)
	require.Len(t, all, 4, "IncludeSuperseded did not return the originals; the trail is unreadable")
}

// TestApplyDistillation_RefusalsLeaveEverythingCurrent is the failure
// direction, one row per way a distillation can be wrong. In every case the
// originals must remain visible and unsuperseded — losing a merge costs a
// model call, losing the memories costs the memories.
func TestApplyDistillation_RefusalsLeaveEverythingCurrent(t *testing.T) {
	base := []string{"note one about deploys", "note two about deploys", "note three"}

	cases := []struct {
		name string
		mut  func(ids []string) MemoryDistillation
		want string
	}{
		{
			"empty content",
			func(ids []string) MemoryDistillation {
				return MemoryDistillation{SourceIDs: ids, Content: "   "}
			},
			"empty",
		},
		{
			"single source",
			func(ids []string) MemoryDistillation {
				return MemoryDistillation{SourceIDs: ids[:1], Content: "merged"}
			},
			"at least 2",
		},
		{
			"no sources",
			func(ids []string) MemoryDistillation {
				return MemoryDistillation{Content: "merged"}
			},
			"at least 2",
		},
		{
			"unknown id",
			func(ids []string) MemoryDistillation {
				return MemoryDistillation{SourceIDs: []string{ids[0], "no-such-memory"}, Content: "merged"}
			},
			"does not exist",
		},
		{
			"duplicate id",
			func(ids []string) MemoryDistillation {
				return MemoryDistillation{SourceIDs: []string{ids[0], ids[0]}, Content: "merged"}
			},
			"listed twice",
		},
		{
			"empty id",
			func(ids []string) MemoryDistillation {
				return MemoryDistillation{SourceIDs: []string{ids[0], ""}, Content: "merged"}
			},
			"empty source id",
		},
		{
			"too many sources",
			func(ids []string) MemoryDistillation {
				many := make([]string, MaxDistillInputs+1)
				for i := range many {
					many[i] = ids[0]
				}
				return MemoryDistillation{SourceIDs: many, Content: "merged"}
			},
			"exceeds the limit",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := openTempStore(t)
			ids := seedMemories(t, s, base...)

			_, err := s.ApplyDistillation(tc.mut(ids))
			require.Error(t, err, "an invalid distillation was applied")
			require.Contains(t, err.Error(), tc.want)

			// Nothing written, nothing hidden.
			cur, rerr := s.RecallMemory(50)
			require.NoError(t, rerr)
			require.Len(t, cur, len(base),
				"a refused distillation changed the current memory set: %+v", cur)
			for _, m := range cur {
				require.Empty(t, m.SupersededBy,
					"a refused distillation superseded %q anyway", m.Content)
				require.Empty(t, m.DistilledFrom)
			}
		})
	}
}

// TestApplyDistillation_RejectsAlreadySupersededSources: two passes racing over
// the same rows must not both win, or the second one's lineage points at rows
// the default query hides — a trail that exists and leads nowhere.
func TestApplyDistillation_RejectsAlreadySupersededSources(t *testing.T) {
	s, _ := openTempStore(t)
	ids := seedMemories(t, s, "a", "b", "c")

	first, err := s.ApplyDistillation(MemoryDistillation{SourceIDs: ids[:2], Content: "a+b"})
	require.NoError(t, err)

	_, err = s.ApplyDistillation(MemoryDistillation{SourceIDs: []string{ids[0], ids[2]}, Content: "a+c"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "already superseded")

	// The loser changed nothing: c is still current and unsuperseded.
	third, err := s.MemoryByID(ids[2])
	require.NoError(t, err)
	require.Empty(t, third.SupersededBy)
	// And the winner is intact.
	won, err := s.MemoryByID(first)
	require.NoError(t, err)
	require.Len(t, won.DistilledFrom, 2)
}

// TestDistillCandidates_ReturnsTheOldestFirst is the ordering that makes the
// whole feature point the right way. Retrieval takes the NEWEST, so the oldest
// rows are the ones about to fall off the end of every query — and those are
// the durable preferences worth keeping. A consolidation pass over the newest
// rows would leave the endangered ones exactly where they were.
//
// The assertion is on INSERTION ORDER, not on created_at, and that distinction
// is what the test caught: created_at has one-second resolution, so every row
// written in one turn shares a timestamp and a created_at-only assertion is
// vacuous. The first version of this test tie-broke on id and failed one run
// in several — newID is random, so ordering by it shuffles a same-second burst
// and "the oldest memories" becomes "some memories". rowid is SQLite's
// insertion counter and recovers the real order.
func TestDistillCandidates_ReturnsTheOldestFirst(t *testing.T) {
	s, _ := openTempStore(t)
	var want []string
	for _, b := range []string{"oldest", "second", "third", "newest"} {
		id, err := s.WriteMemory("note", b)
		require.NoError(t, err)
		want = append(want, id)
	}
	got, err := s.DistillCandidates(0, MemoryFilter{})
	require.NoError(t, err)
	require.Len(t, got, len(want))
	for i := 1; i < len(got); i++ {
		require.LessOrEqual(t, got[i-1].CreatedAt, got[i].CreatedAt)
	}
	for i, id := range want {
		require.Equal(t, id, got[i].ID,
			"candidate %d is not the %d-th memory written; within one second the ordering "+
				"must still be insertion order or the pass consolidates an arbitrary subset", i, i)
	}
}

// TestDistillCandidates_ClampsToWhatCanBeApplied: a caller must not be able to
// assemble a batch that ApplyDistillation would then refuse for being too big.
func TestDistillCandidates_ClampsToWhatCanBeApplied(t *testing.T) {
	s, _ := openTempStore(t)
	for i := 0; i < MaxDistillInputs*2; i++ {
		_, err := s.WriteMemory("note", "memory "+strings.Repeat("x", i%5))
		require.NoError(t, err)
	}
	for _, limit := range []int{0, -1, 5, MaxDistillInputs, MaxDistillInputs + 100} {
		got, err := s.DistillCandidates(limit, MemoryFilter{})
		require.NoError(t, err)
		require.LessOrEqual(t, len(got), MaxDistillInputs,
			"limit %d produced a batch ApplyDistillation would refuse", limit)
	}
}

// TestDistillCandidates_SkipsSupersededRows — a second pass must not be handed
// rows the first one already consumed, which would fail on the
// already-superseded check and waste the pass.
func TestDistillCandidates_SkipsSupersededRows(t *testing.T) {
	s, _ := openTempStore(t)
	ids := seedMemories(t, s, "a", "b", "c", "d")
	_, err := s.ApplyDistillation(MemoryDistillation{SourceIDs: ids[:3], Content: "abc"})
	require.NoError(t, err)

	got, err := s.DistillCandidates(0, MemoryFilter{})
	require.NoError(t, err)
	require.Len(t, got, 2, "superseded rows came back as candidates: %+v", got)
	for _, m := range got {
		require.Empty(t, m.SupersededBy)
	}
}

// TestCountMemories_CountsWhatRetrievalWouldSee — it is the trigger for a
// distillation pass, so counting hidden rows would fire passes forever on a
// table that has already been consolidated.
func TestCountMemories_CountsWhatRetrievalWouldSee(t *testing.T) {
	s, _ := openTempStore(t)
	ids := seedMemories(t, s, "a", "b", "c", "d", "e")

	n, err := s.CountMemories(MemoryFilter{})
	require.NoError(t, err)
	require.Equal(t, 5, n)

	_, err = s.ApplyDistillation(MemoryDistillation{SourceIDs: ids[:4], Content: "abcd"})
	require.NoError(t, err)

	n, err = s.CountMemories(MemoryFilter{})
	require.NoError(t, err)
	require.Equal(t, 2, n, "the count still includes superseded rows; a consolidated table "+
		"would keep triggering passes that have nothing left to merge")
}

// TestApplyDistillation_TagsTheMergedRowWithItsDimensions: a session-scoped
// pass must write the result back into the same scope, or consolidating a
// session's memories makes them unfindable under that session.
func TestApplyDistillation_TagsTheMergedRowWithItsDimensions(t *testing.T) {
	s, _ := openTempStore(t)
	dims := MemoryFilter{SessionID: "sess-1", AgentID: "agent-a"}
	var ids []string
	for _, b := range []string{"one", "two"} {
		id, err := s.WriteMemoryScoped("note", b, dims)
		require.NoError(t, err)
		ids = append(ids, id)
	}
	newID, err := s.ApplyDistillation(MemoryDistillation{
		SourceIDs: ids, Content: "one and two", Dims: dims,
	})
	require.NoError(t, err)

	scoped, err := s.RecallMemoryScoped(50, dims)
	require.NoError(t, err)
	require.Len(t, scoped, 1)
	require.Equal(t, newID, scoped[0].ID,
		"the merged row is not findable under the scope its inputs came from")
}

// TestMemoryByID_ReadsSupersededRows is what makes the audit trail usable. A
// lookup that also hid superseded rows would leave the data kept and nothing
// able to see it — the failure shape C13's whole design is aimed at.
func TestMemoryByID_ReadsSupersededRows(t *testing.T) {
	s, _ := openTempStore(t)
	ids := seedMemories(t, s, "first", "second")
	_, err := s.ApplyDistillation(MemoryDistillation{SourceIDs: ids, Content: "both"})
	require.NoError(t, err)

	got, err := s.MemoryByID(ids[0])
	require.NoError(t, err)
	require.Equal(t, "first", got.Content)

	_, err = s.MemoryByID("nope")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")
}

// TestSearchMemoryRanked_OrdersByRelevance covers the ordering C12 depends on:
// a caller taking the top K discards everything below it unseen, so ranking by
// date would discard the best match for being old.
func TestSearchMemoryRanked_OrdersByRelevance(t *testing.T) {
	s, _ := openTempStore(t)
	// The best match is written FIRST and then aged, so recency ordering would
	// rank it LAST. Ageing it explicitly is not decoration: created_at has
	// one-second resolution, so rows written in one test all share a timestamp,
	// a recency ORDER BY becomes a tie, and SQLite is free to return them in
	// whatever order it likes — which was relevance order. A mutation probe
	// that replaced the ranking with `ORDER BY created_at DESC` passed this
	// test unchanged until the timestamps were forced apart.
	best, err := s.WriteMemory("note", "kubernetes ingress certificate renewal procedure")
	require.NoError(t, err)
	_, err = s.DB.Exec("UPDATE memories SET created_at = created_at - 1000 WHERE id = ?", best)
	require.NoError(t, err)
	for i := 0; i < 5; i++ {
		_, err := s.WriteMemory("note", "kubernetes note "+strings.Repeat("filler ", 40))
		require.NoError(t, err)
	}
	hits, err := s.SearchMemoryRanked(`"kubernetes" OR "ingress" OR "certificate" OR "renewal"`, 10, MemoryFilter{})
	require.NoError(t, err)
	require.NotEmpty(t, hits)
	require.Contains(t, hits[0].Content, "ingress certificate renewal",
		"the best match is not first; a top-K caller would discard it")
	for i := 1; i < len(hits); i++ {
		require.LessOrEqual(t, hits[i-1].Score, hits[i].Score,
			"scores are not ascending (bm25: more negative is better)")
	}
}

// TestSearchMemoryScoped_StillReturnsPlainMemories — the ranked query is the
// new implementation underneath the old signature, so the old one must keep
// returning what it always did.
func TestSearchMemoryScoped_StillReturnsPlainMemories(t *testing.T) {
	s, _ := openTempStore(t)
	seedMemories(t, s, "alpha beta gamma", "delta epsilon")

	got, err := s.SearchMemory("alpha", 0)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "alpha beta gamma", got[0].Content)
}
