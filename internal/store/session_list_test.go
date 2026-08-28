package store

import (
	"encoding/base64"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListSessions_OrderAndCount(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	id1, err := s.CreateSession("first")
	require.NoError(t, err)

	time.Sleep(time.Second) // ensure updated_at differs

	id2, err := s.CreateSession("second")
	require.NoError(t, err)

	// Touch the first session so it becomes most-recently-updated. The sleep
	// is what makes "most-recently-updated" true rather than merely intended:
	// updated_at is second-granular, so appending in the same second as id2's
	// creation leaves the two TIED, and the assertions below would then be
	// reading whatever order the tie-break happens to produce instead of the
	// recency this test is named for.
	time.Sleep(time.Second)
	require.NoError(t, s.AppendMessage(id1, 0, "user", "hello"))

	list, err := s.ListSessions(0)
	require.NoError(t, err)
	require.Len(t, list, 2)

	// Most-recently-updated first → id1 was touched last.
	assert.Equal(t, id1, list[0].ID)
	assert.Equal(t, "first", list[0].Title)
	assert.Equal(t, id2, list[1].ID)

	// Limit works.
	one, err := s.ListSessions(1)
	require.NoError(t, err)
	require.Len(t, one, 1)
	assert.Equal(t, id1, one[0].ID)
}

// TestListSessions_ZeroSessions proves a fresh store lists nothing.
func TestListSessions_ZeroSessions(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	list, err := s.ListSessions(0)
	require.NoError(t, err)
	assert.Empty(t, list)
}

// TestListSessions_CursorPaginationIsStable proves the cursor keeps a paging
// walk free of duplicates and gaps while the list is being written to.
//
// The fixture creates five sessions, reads page 1, then creates two MORE before
// asking for page 2. That middle step is the entire point: the list is ordered
// updated_at DESC, so the new rows land at the front, and an OFFSET-based page
// 2 would skip past them and re-serve rows the caller already has. The union of
// the two pages must be exactly the first four of the original five, in order,
// with the two newcomers nowhere in sight — they sort ahead of the cursor, i.e.
// on a page the walk has already gone past.
//
// The five fixtures are created in a single second on purpose, so every one of
// them shares an updated_at. That makes the id tie-break in listSessionsWhere
// load-bearing: with only `updated_at DESC` the tie group has no defined order,
// so the cursor's id selects an arbitrary subset of it and page 2 stops being
// the next two rows. The intruders, by contrast, are created a full second
// later so they sort strictly ahead of the whole group — within one second the
// order is by random id, and an "intruder" could then legitimately land behind
// the cursor, which is ordinary keyset behaviour rather than the front-insertion
// this test is about.
func TestListSessions_CursorPaginationIsStable(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	var original []string
	for i := 0; i < 5; i++ {
		id, err := s.CreateSession("session")
		require.NoError(t, err)
		original = append(original, id)
	}

	// The authoritative order, taken from the unpaged read so the test cannot
	// disagree with ListSessions about what "page 1 then page 2" should be.
	full, err := s.ListSessions(0)
	require.NoError(t, err)
	require.Len(t, full, 5)

	page1, err := s.ListSessionsPage("", 2)
	require.NoError(t, err)
	require.Len(t, page1.Sessions, 2)
	require.NotEmpty(t, page1.NextCursor, "five rows, page size two: there is a page 2")

	// Mutate the list mid-walk: two brand-new sessions jump to the front.
	time.Sleep(1100 * time.Millisecond) // updated_at is second-granular
	var intruders []string
	for i := 0; i < 2; i++ {
		id, err := s.CreateSession("inserted-mid-walk")
		require.NoError(t, err)
		intruders = append(intruders, id)
	}

	page2, err := s.ListSessionsPage(page1.NextCursor, 2)
	require.NoError(t, err)
	require.Len(t, page2.Sessions, 2)

	var walked []string
	for _, ss := range append(append([]SessionSummary{}, page1.Sessions...), page2.Sessions...) {
		walked = append(walked, ss.ID)
	}

	want := []string{full[0].ID, full[1].ID, full[2].ID, full[3].ID}
	assert.Equal(t, want, walked, "the walk is the first four rows of the pre-insert order")

	// No duplicates, and no intruder smuggled in.
	seen := map[string]bool{}
	for _, id := range walked {
		assert.False(t, seen[id], "session %s served twice", id)
		seen[id] = true
	}
	for _, id := range intruders {
		assert.NotContains(t, seen, id, "a session created mid-walk must not appear behind the cursor")
	}
	assert.Subset(t, original, walked, "every walked row is one of the originals")

	// Walking to the end terminates, and NextCursor empties exactly once the
	// last row has been delivered rather than one page later.
	var all []string
	cursor := ""
	for range 20 {
		p, err := s.ListSessionsPage(cursor, 2)
		require.NoError(t, err)
		for _, ss := range p.Sessions {
			all = append(all, ss.ID)
		}
		cursor = p.NextCursor
		if cursor == "" {
			break
		}
	}
	assert.Empty(t, cursor, "the walk terminated")
	assert.Len(t, all, 7, "all five originals plus the two inserted mid-walk")
}

// TestListSessionsPage_RejectsMalformedCursor proves a corrupt token is an
// error rather than a silent restart at page 1, which would look to the caller
// like progress while re-serving the newest sessions forever.
func TestListSessionsPage_RejectsMalformedCursor(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	_, err = s.CreateSession("only")
	require.NoError(t, err)

	for name, tok := range map[string]string{
		"not base64":     "!!!not-base64!!!",
		"no separator":   base64.RawURLEncoding.EncodeToString([]byte("1700000000")),
		"empty id":       base64.RawURLEncoding.EncodeToString([]byte("1700000000|")),
		"bad timestamp":  base64.RawURLEncoding.EncodeToString([]byte("yesterday|abc")),
		"truncated pair": base64.RawURLEncoding.EncodeToString([]byte("|")),
	} {
		t.Run(name, func(t *testing.T) {
			page, err := s.ListSessionsPage(tok, 2)
			assert.Error(t, err)
			assert.Empty(t, page.Sessions, "a rejected cursor returns no rows, not page 1")
		})
	}
}

// TestGetSession_Missing proves GetSession on an absent id returns (nil, nil)
// with no error (session_list.go:102 — the missing case simply returns nil, nil).
func TestGetSession_Missing(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	got, err := s.GetSession("nope")
	assert.NoError(t, err) // actual code returns (nil, nil) on missing
	assert.Nil(t, got)
}

// TestListArchivedSessions_RoundTrip proves archiving hides from ListSessions
// and surfaces in ListArchivedSessions; unarchive reverses it.
func TestListArchivedSessions_RoundTrip(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	id, err := s.CreateSession("to-archive")
	require.NoError(t, err)

	// Initially: active list has it, archived list empty.
	active, err := s.ListSessions(0)
	require.NoError(t, err)
	require.Len(t, active, 1)
	archived, err := s.ListArchivedSessions(0)
	require.NoError(t, err)
	assert.Empty(t, archived)

	// Archive: moves between lists.
	require.NoError(t, s.SetSessionArchived(id, true))
	active, err = s.ListSessions(0)
	require.NoError(t, err)
	assert.Empty(t, active)
	archived, err = s.ListArchivedSessions(0)
	require.NoError(t, err)
	require.Len(t, archived, 1)
	assert.Equal(t, id, archived[0].ID)

	// Unarchive: reverses.
	require.NoError(t, s.SetSessionArchived(id, false))
	active, err = s.ListSessions(0)
	require.NoError(t, err)
	require.Len(t, active, 1)
	archived, err = s.ListArchivedSessions(0)
	require.NoError(t, err)
	assert.Empty(t, archived)
}
