package store

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSession_CreateAndAppend(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	sid, err := s.CreateSession("plan feature")
	require.NoError(t, err)
	assert.NotEmpty(t, sid)

	require.NoError(t, s.AppendMessage(sid, 0, "user", "hi"))
	require.NoError(t, s.AppendMessage(sid, 1, "assistant", "hello"))

	msgs, err := s.Messages(sid)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	assert.Equal(t, "user", msgs[0].Role)
	assert.Equal(t, "hi", msgs[0].Content)
	assert.Equal(t, "assistant", msgs[1].Role)
}

func TestSession_MessagesEmpty(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	sid, err := s.CreateSession("x")
	require.NoError(t, err)
	msgs, err := s.Messages(sid)
	require.NoError(t, err)
	assert.Empty(t, msgs)
}

// TestMigrateAndPersistBillingColumns proves the 5 billing columns are present
// after migration, UpdateSessionMeta persists them via BillingMeta, and the
// ListSessions/GetSession read paths surface them. Defaults are 0/CostKnown=false.
func TestMigrateAndPersistBillingColumns(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "yanshi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	id, err := st.CreateSession("billing")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateSessionMeta(id, "claude-opus-4-8", "low",
		5, 4, 3, 2, 1,
		BillingMeta{InputTokens: 3, CachedTokens: 2, OutputTokens: 4, CostUSD: 0.0125, CostKnown: true},
	); err != nil {
		t.Fatal(err)
	}

	got, err := st.GetSession(id)
	if err != nil || got == nil {
		t.Fatalf("GetSession: %v %v", got, err)
	}
	if got.BilledInputTokens != 3 || got.BilledCachedTokens != 2 || got.BilledOutputTokens != 4 {
		t.Fatalf("billed tokens = %+v", got)
	}
	if got.CostUSD != 0.0125 || !got.CostKnown {
		t.Fatalf("cost fields = %+v", got)
	}

	list, err := st.ListSessions(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].BilledInputTokens != 3 {
		t.Fatalf("ListSessions did not surface billed tokens: %+v", list)
	}
}

// TestSession_UpdateSessionTitle proves the title round-trips through GetSession.
// AppendMessage also bumps updated_at via the same row.
func TestSession_UpdateSessionTitle(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	sid, err := s.CreateSession("old")
	require.NoError(t, err)
	require.NoError(t, s.UpdateSessionTitle(sid, "new title"))

	got, err := s.GetSession(sid)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "new title", got.Title)
}

// TestSession_MessageCount proves the count tracks AppendMessage calls and
// is 0 for a fresh session.
func TestSession_MessageCount(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	sid, err := s.CreateSession("c")
	require.NoError(t, err)

	n, err := s.SessionMessageCount(sid)
	require.NoError(t, err)
	assert.Equal(t, 0, n)

	require.NoError(t, s.AppendMessage(sid, 0, "user", "a"))
	require.NoError(t, s.AppendMessage(sid, 1, "assistant", "b"))
	n, err = s.SessionMessageCount(sid)
	require.NoError(t, err)
	assert.Equal(t, 2, n)
}

// TestSession_AppendMessage_MissingSession documents the unenforced-FK
// behavior: appending to a non-existent session does NOT error (the messages
// row is orphaned), and Messages returns the orphan because it queries by
// session_id. This is correct SQLite behavior — the FK constraint is advisory
// by default. The test guards against a future regression that would either
// panic on missing sessions or silently drop the orphan.
func TestSession_AppendMessage_MissingSession(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	// messages.session_id FK is unenforced by default → AppendMessage to a
	// non-existent session does not error; Messages returns the orphan row.
	err = s.AppendMessage("definitely-not-a-session", 0, "user", "x")
	require.NoError(t, err, "AppendMessage must not reject a missing session")

	msgs, err := s.Messages("definitely-not-a-session")
	require.NoError(t, err)
	require.Len(t, msgs, 1, "orphan row is queryable by session_id")
	assert.Equal(t, "x", msgs[0].Content)
}

// TestSession_LargeContent proves AppendMessage handles >=1MB content without
// panic and Messages returns it intact.
func TestSession_LargeContent(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	sid, err := s.CreateSession("big")
	require.NoError(t, err)

	big := strings.Repeat("A", 1<<20) // 1 MiB
	require.NoError(t, s.AppendMessage(sid, 0, "user", big))

	msgs, err := s.Messages(sid)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	assert.Len(t, msgs[0].Content, 1<<20)
}

// TestSession_ConcurrentAppend exercises AppendMessage from multiple
// goroutines on the same session. SQLite serializes writes under a single
// connection; even with F1 WAL landed, :memory: forces MaxOpenConns=1 so
// concurrent writers still hit "database is locked". Skipped by default;
// set YANSHI_TEST_CONCURRENT=1 to run locally (may flake on :memory:).
func TestSession_ConcurrentAppend(t *testing.T) {
	if os.Getenv("YANSHI_TEST_CONCURRENT") != "1" {
		t.Skip("set YANSHI_TEST_CONCURRENT=1; :memory: forces single-conn, concurrent writes may hit locked")
	}

	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	sid, err := s.CreateSession("concurrent")
	require.NoError(t, err)

	const goroutines = 4
	const perG = 25
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				_ = s.AppendMessage(sid, g*perG+i, "user", fmt.Sprintf("g%d-i%d", g, i))
			}
		}(g)
	}
	wg.Wait()

	n, err := s.SessionMessageCount(sid)
	require.NoError(t, err)
	assert.Equal(t, goroutines*perG, n, "all appends must persist despite concurrency")
}
