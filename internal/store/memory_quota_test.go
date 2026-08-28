package store

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestPruneUnusedMemories_ZeroQuotaDeletesNothing is the off switch. This is
// the only function in the memory subsystem that destroys a row, so the default
// must be inert.
func TestPruneUnusedMemories_ZeroQuotaDeletesNothing(t *testing.T) {
	s := openTestStore(t)
	for i := range 10 {
		_, err := s.WriteMemory("note", "fact "+strconv.Itoa(i))
		require.NoError(t, err)
	}
	n, err := s.PruneUnusedMemories(0)
	require.NoError(t, err)
	require.Zero(t, n)
	require.Equal(t, 10, countMemories(t, s))

	// Counted with SQL, not with RecallMemory: a recall is a USE, and using
	// every row would make the prune below find nothing to trim — which would
	// turn the second half of this test into a tautology.
	//
	// The same call with a real quota does prune, so the assertion above is
	// about the switch and not about a prune that never works.
	n, err = s.PruneUnusedMemories(4)
	require.NoError(t, err)
	require.Equal(t, 6, n)
	require.Equal(t, 4, countMemories(t, s))
}

// countMemories counts rows without touching the use counter.
func countMemories(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	require.NoError(t, s.DB.QueryRow("SELECT COUNT(*) FROM memories").Scan(&n))
	return n
}

// TestPruneUnusedMemories_KeepsWhatWasRetrieved is the acceptance clause "when
// the quota bites, the OLD UNUSED memories are trimmed".
//
// The retrieved memory here is the OLDEST one, so a prune that ordered purely
// by age would delete exactly the row this test protects.
func TestPruneUnusedMemories_KeepsWhatWasRetrieved(t *testing.T) {
	s := openTestStore(t)
	oldest, err := s.WriteMemory("note", "deploys go through the release script")
	require.NoError(t, err)
	for i := range 9 {
		_, err := s.WriteMemory("note", "filler fact "+strconv.Itoa(i))
		require.NoError(t, err)
	}

	// Retrieve it the way production does — through the search path, not by
	// poking the column — so this also proves the counter is wired to a real
	// read rather than only to the test.
	hits, err := s.SearchMemory("release", 5)
	require.NoError(t, err)
	require.NotEmpty(t, hits)
	used, err := s.MemoryUseCount(oldest)
	require.NoError(t, err)
	require.Positive(t, used, "a search must count as a use")

	n, err := s.PruneUnusedMemories(3)
	require.NoError(t, err)
	require.Equal(t, 7, n)

	_, err = s.MemoryByID(oldest)
	require.NoError(t, err, "the oldest memory was retrieved and must survive the quota")
}

// TestPruneUnusedMemories_NeverEvictsUsedRowsToHitTheNumber: if everything over
// quota has been used, the table is allowed to exceed the quota. Evicting a
// proven-useful note to reach a number is worse than having no quota.
func TestPruneUnusedMemories_NeverEvictsUsedRowsToHitTheNumber(t *testing.T) {
	s := openTestStore(t)
	for i := range 5 {
		_, err := s.WriteMemory("note", "release fact "+strconv.Itoa(i))
		require.NoError(t, err)
	}
	hits, err := s.SearchMemory("release", 10)
	require.NoError(t, err)
	require.Len(t, hits, 5)

	n, err := s.PruneUnusedMemories(1)
	require.NoError(t, err)
	require.Zero(t, n)
	require.Equal(t, 5, countMemories(t, s))
}

// TestMarkMemoriesUsed_CountsRecallAndCJKSearchToo.
//
// Both are separate exits from the read path, and both were bypassed by the
// first version of this wiring. A Chinese-only deployment (this repo's own
// working language) would have had every memory look unused and be pruned
// first; memory_recall's results likewise.
func TestMarkMemoriesUsed_CountsRecallAndCJKSearchToo(t *testing.T) {
	s := openTestStore(t)
	id, err := s.WriteMemory("note", "部署走 release 脚本，不要手工 push")
	require.NoError(t, err)

	_, err = s.RecallMemory(10)
	require.NoError(t, err)
	afterRecall, err := s.MemoryUseCount(id)
	require.NoError(t, err)
	require.Equal(t, 1, afterRecall, "a recall must count as a use")

	hits, err := s.SearchMemory(`"部署"`, 10)
	require.NoError(t, err)
	require.NotEmpty(t, hits, "the CJK fallback must find it")
	afterSearch, err := s.MemoryUseCount(id)
	require.NoError(t, err)
	require.Equal(t, 2, afterSearch, "the CJK search path must count too")
}
