package store

// liverun_c1314_test.go — memory consolidation and retrieval dimensions,
// checked against a real database rather than against the return value of the
// function that just wrote to it.
//
// The questions here are all "what does the table hold afterwards":
//
//   - C13: did a distillation actually merge the duplicates, and — the part
//     that matters far more — are the originals still there when the merge
//     FAILS? A consolidation that loses memories on its error path is worse
//     than no consolidation, and its success path looks identical.
//   - C14: can a session's memories be retrieved separately from another
//     session's, and does an unscoped search still see both? The dimension is
//     only useful if BOTH directions hold; a filter that hides everything is as
//     broken as one that hides nothing.
//
// Every assertion re-queries the store. Nothing trusts a returned struct as a
// description of what was persisted.

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// liveStore opens a real on-disk database, since these tests are about what
// survives a write.
func liveStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "memories.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// contents returns the sorted contents of a memory slice, for set comparisons.
func contents(ms []Memory) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Content)
	}
	sort.Strings(out)
	return out
}

// TestLiveRun_C13DistillationMergesAndHidesButKeepsTheOriginals runs a real
// consolidation and then interrogates the table three ways: what a normal
// retrieval sees, what an audit sees, and whether the lineage points at real
// rows.
func TestLiveRun_C13DistillationMergesAndHidesButKeepsTheOriginals(t *testing.T) {
	s := liveStore(t)

	originals := []string{
		"user prefers tabs in Makefiles",
		"user asked to keep using tabs in Makefiles",
		"reminder: Makefiles need tabs, not spaces",
	}
	var ids []string
	for _, c := range originals {
		id, err := s.WriteMemory("note", c)
		require.NoError(t, err)
		ids = append(ids, id)
	}
	before, err := s.RecallMemory(50)
	require.NoError(t, err)
	t.Logf("before: %d visible memories", len(before))
	require.Len(t, before, 3)

	merged := "The user wants tabs (not spaces) in Makefiles."
	newID, err := s.ApplyDistillation(MemoryDistillation{SourceIDs: ids, Content: merged})
	require.NoError(t, err)
	t.Logf("distilled %v -> %s", ids, newID)

	// (1) Default retrieval reads as if the rows were merged.
	after, err := s.RecallMemory(50)
	require.NoError(t, err)
	t.Logf("after: %d visible memories: %v", len(after), contents(after))
	if len(after) != 1 {
		t.Fatalf("default retrieval returns %d rows, want just the merged one: %v",
			len(after), contents(after))
	}
	if after[0].Content != merged {
		t.Errorf("the visible row is %q, want the merged text %q", after[0].Content, merged)
	}

	// (2) The originals are still on disk, byte for byte. This is the promise
	// that makes a bad merge recoverable rather than amnesia.
	audit, err := s.RecallMemoryScoped(50, MemoryFilter{IncludeSuperseded: true})
	require.NoError(t, err)
	t.Logf("audit view: %d rows", len(audit))
	got := contents(audit)
	want := append(append([]string{}, originals...), merged)
	sort.Strings(want)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("audit view lost a memory.\n got: %v\nwant: %v", got, want)
	}

	// (3) The lineage is real: every id the merged row cites resolves, and each
	// original points back at the merged row.
	m, err := s.MemoryByID(newID)
	require.NoError(t, err)
	t.Logf("merged row cites %v", m.DistilledFrom)
	if len(m.DistilledFrom) != len(ids) {
		t.Errorf("merged row cites %d sources, want %d", len(m.DistilledFrom), len(ids))
	}
	for _, src := range m.DistilledFrom {
		orig, err := s.MemoryByID(src)
		if err != nil {
			t.Errorf("merged row cites %s, which does not resolve: %v", src, err)
			continue
		}
		if orig.SupersededBy != newID {
			t.Errorf("original %s says superseded_by=%q, want %s", src, orig.SupersededBy, newID)
		}
	}
}

// TestLiveRun_C13FailedDistillationLeavesEveryOriginalCurrent is the safety
// case, and the reason this file exists at all.
//
// Each refusal below is checked by re-reading the table: the sources must still
// be VISIBLE to a default retrieval, not merely present. A half-applied merge
// that marks rows superseded and then fails to insert the replacement makes
// those memories invisible forever, which is indistinguishable from deletion
// for every consumer.
func TestLiveRun_C13FailedDistillationLeavesEveryOriginalCurrent(t *testing.T) {
	s := liveStore(t)
	var ids []string
	for i := 0; i < 3; i++ {
		id, err := s.WriteMemory("note", fmt.Sprintf("durable preference number %d", i))
		require.NoError(t, err)
		ids = append(ids, id)
	}

	cases := []struct {
		name string
		d    MemoryDistillation
	}{
		{"empty merged content", MemoryDistillation{SourceIDs: ids, Content: "   "}},
		{"single source is a rewrite, not a merge",
			MemoryDistillation{SourceIDs: ids[:1], Content: "rewritten"}},
		{"a source that does not exist",
			MemoryDistillation{SourceIDs: []string{ids[0], "no-such-memory-id"}, Content: "merged"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.ApplyDistillation(tc.d)
			t.Logf("refused with: %v", err)
			if err == nil {
				t.Fatalf("the store accepted a distillation it must refuse")
			}
			visible, rerr := s.RecallMemory(50)
			require.NoError(t, rerr)
			if len(visible) != len(ids) {
				t.Errorf("after a refused distillation only %d of %d memories are still visible: %v",
					len(visible), len(ids), contents(visible))
			}
			for _, id := range ids {
				m, merr := s.MemoryByID(id)
				require.NoError(t, merr)
				if m.SupersededBy != "" {
					t.Errorf("memory %s was marked superseded by a distillation that FAILED "+
						"(superseded_by=%q); it is now invisible to every default read",
						id, m.SupersededBy)
				}
			}
		})
	}

	// A second pass over already-merged rows must be refused too, or its
	// lineage would cite hidden rows.
	newID, err := s.ApplyDistillation(MemoryDistillation{SourceIDs: ids, Content: "merged once"})
	require.NoError(t, err)
	_, err = s.ApplyDistillation(MemoryDistillation{SourceIDs: ids, Content: "merged twice"})
	t.Logf("re-distilling superseded rows: %v", err)
	if err == nil {
		t.Errorf("a second distillation over already-superseded rows was accepted; " +
			"its lineage would point at rows no default query returns")
	}
	m, err := s.MemoryByID(newID)
	require.NoError(t, err)
	if m.SupersededBy != "" {
		t.Errorf("the first merge was superseded by a refused second pass")
	}
}

// TestLiveRun_C14SessionAndAgentDimensionsSeparateRetrieval writes memories
// under three different scopes and then checks every retrieval direction.
//
// Both directions are asserted on purpose. A filter that returns nothing passes
// "session A does not see session B" trivially, and a filter that is ignored
// passes "an unscoped search sees everything" just as trivially; only running
// all four queries distinguishes a working dimension from either failure.
func TestLiveRun_C14SessionAndAgentDimensionsSeparateRetrieval(t *testing.T) {
	s := liveStore(t)

	write := func(content string, dims MemoryFilter) {
		t.Helper()
		_, err := s.WriteMemoryScoped("note", content, dims)
		require.NoError(t, err)
	}
	write("alpha session note about widgets", MemoryFilter{SessionID: "sess-alpha"})
	write("beta session note about widgets", MemoryFilter{SessionID: "sess-beta"})
	write("subagent note about widgets", MemoryFilter{AgentID: "agent-reviewer"})
	write("unscoped note about widgets", MemoryFilter{})

	check := func(name string, dims MemoryFilter, want []string) {
		t.Helper()
		hits, err := s.SearchMemoryScoped("widgets", 50, dims)
		require.NoError(t, err)
		got := contents(hits)
		sort.Strings(want)
		t.Logf("%s -> %v", name, got)
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("%s returned\n got: %v\nwant: %v", name, got, want)
		}
	}

	check("unscoped search", MemoryFilter{}, []string{
		"alpha session note about widgets",
		"beta session note about widgets",
		"subagent note about widgets",
		"unscoped note about widgets",
	})
	check("session alpha", MemoryFilter{SessionID: "sess-alpha"},
		[]string{"alpha session note about widgets"})
	check("session beta", MemoryFilter{SessionID: "sess-beta"},
		[]string{"beta session note about widgets"})
	check("agent reviewer", MemoryFilter{AgentID: "agent-reviewer"},
		[]string{"subagent note about widgets"})
	check("a session that wrote nothing", MemoryFilter{SessionID: "sess-unknown"}, nil)

	// Recall (newest-first, no query) must honour the same dimensions, or the
	// two read paths disagree about what a scope contains.
	recalled, err := s.RecallMemoryScoped(50, MemoryFilter{SessionID: "sess-alpha"})
	require.NoError(t, err)
	t.Logf("recall scoped to sess-alpha -> %v", contents(recalled))
	if len(recalled) != 1 || recalled[0].Content != "alpha session note about widgets" {
		t.Errorf("scoped recall returned %v, want only alpha's note", contents(recalled))
	}

	// And the dimensions must survive a distillation: a merged row that loses
	// its scope becomes invisible to the very session whose notes it merges.
	for i := 0; i < 2; i++ {
		write(fmt.Sprintf("alpha extra note %d about widgets", i), MemoryFilter{SessionID: "sess-alpha"})
	}
	alpha, err := s.DistillCandidates(MaxDistillInputs, MemoryFilter{SessionID: "sess-alpha"})
	require.NoError(t, err)
	require.Len(t, alpha, 3, "the candidate query must also be scoped")
	var alphaIDs []string
	for _, m := range alpha {
		alphaIDs = append(alphaIDs, m.ID)
	}
	mergedID, err := s.ApplyDistillation(MemoryDistillation{
		SourceIDs: alphaIDs,
		Content:   "alpha's consolidated widget notes",
		Dims:      MemoryFilter{SessionID: "sess-alpha"},
	})
	require.NoError(t, err)

	merged, err := s.MemoryByID(mergedID)
	require.NoError(t, err)
	if merged.SessionID != "sess-alpha" {
		t.Errorf("the merged row is tagged session %q, want sess-alpha; it is now unfindable "+
			"from the session whose memories it consolidates", merged.SessionID)
	}
	scoped, err := s.RecallMemoryScoped(50, MemoryFilter{SessionID: "sess-alpha"})
	require.NoError(t, err)
	t.Logf("sess-alpha after distillation -> %v", contents(scoped))
	if len(scoped) != 1 || scoped[0].Content != "alpha's consolidated widget notes" {
		t.Errorf("after a scoped distillation sess-alpha sees %v, want only the merged row",
			contents(scoped))
	}
	// Beta must be untouched by alpha's consolidation.
	beta, err := s.RecallMemoryScoped(50, MemoryFilter{SessionID: "sess-beta"})
	require.NoError(t, err)
	if len(beta) != 1 || beta[0].Content != "beta session note about widgets" {
		t.Errorf("alpha's distillation changed sess-beta: %v", contents(beta))
	}
}
