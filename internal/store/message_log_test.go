package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/secrets"
)

func openTempStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	s, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

func newSession(t *testing.T, s *Store) string {
	t.Helper()
	id, err := s.CreateSession("t")
	require.NoError(t, err)
	return id
}

// ---------------------------------------------------------------------------
// C1: the durable log stores tool_call / tool_result
// ---------------------------------------------------------------------------

// TestAppendMessages_PersistsEveryRole is the primary C1 assertion: the four
// roles all survive a round trip WITH their tool fields. Before C1 only the two
// prose roles were ever written and the tool columns did not exist, so an
// assertion that only checked user/assistant would have passed on the old code.
func TestAppendMessages_PersistsEveryRole(t *testing.T) {
	s, _ := openTempStore(t)
	sid := newSession(t, s)

	in := []Message{
		{Role: RoleUser, Content: "run the tests"},
		{Role: RoleToolCall, ToolCallID: "call-1", ToolName: "shell_run", ToolArgs: `{"cmd":"go test ./..."}`},
		{Role: RoleToolResult, ToolCallID: "call-1", ToolName: "shell_run", Content: "FAIL internal/guard 0.2s"},
		{Role: RoleAssistant, Content: "the guard test is red"},
	}
	inserted, next, err := s.AppendMessages(sid, in)
	require.NoError(t, err)
	assert.Equal(t, 4, inserted)
	assert.Equal(t, 4, next)

	got, err := s.Messages(sid)
	require.NoError(t, err)
	require.Len(t, got, 4)

	for i := range in {
		assert.Equal(t, i, got[i].Seq, "seq %d", i)
		assert.Equal(t, in[i].Role, got[i].Role)
		assert.Equal(t, in[i].Content, got[i].Content)
		assert.Equal(t, in[i].ToolCallID, got[i].ToolCallID, "tool_call_id %d", i)
		assert.Equal(t, in[i].ToolName, got[i].ToolName, "tool_name %d", i)
		assert.Equal(t, in[i].ToolArgs, got[i].ToolArgs, "tool_args %d", i)
		assert.NotEmpty(t, got[i].DedupKey, "row %d must carry a dedup key", i)
	}
}

// TestAppendMessages_Idempotent pins the invariant that makes flush-whole-window
// viable: re-appending an already-durable batch inserts nothing and does not
// renumber. Without it the WS layer would need a watermark across a slice that
// compaction rewrites underneath it.
func TestAppendMessages_Idempotent(t *testing.T) {
	s, _ := openTempStore(t)
	sid := newSession(t, s)

	batch := []Message{
		{Role: RoleUser, Content: "hello"},
		{Role: RoleToolCall, ToolCallID: "c1", ToolName: "fs_read", ToolArgs: `{"path":"a.go"}`},
	}
	n1, _, err := s.AppendMessages(sid, batch)
	require.NoError(t, err)
	require.Equal(t, 2, n1)

	n2, next, err := s.AppendMessages(sid, batch)
	require.NoError(t, err)
	assert.Zero(t, n2, "re-appending a durable batch must insert nothing")
	assert.Equal(t, 2, next)

	// Superset: the two old rows are skipped, only the new one lands.
	grown := append(append([]Message(nil), batch...),
		Message{Role: RoleAssistant, Content: "read it"})
	n3, next, err := s.AppendMessages(sid, grown)
	require.NoError(t, err)
	assert.Equal(t, 1, n3)
	assert.Equal(t, 3, next)

	got, err := s.Messages(sid)
	require.NoError(t, err)
	require.Len(t, got, 3)
	for i, m := range got {
		assert.Equal(t, i, m.Seq)
	}
}

// TestAppendMessages_IdenticalSiblingsAreDistinct: a turn that says "ok" twice
// must store two rows. A content-only fingerprint would collapse them, which is
// data loss dressed up as deduplication.
func TestAppendMessages_IdenticalSiblingsAreDistinct(t *testing.T) {
	s, _ := openTempStore(t)
	sid := newSession(t, s)

	n, _, err := s.AppendMessages(sid, []Message{
		{Role: RoleAssistant, Content: "ok"},
		{Role: RoleAssistant, Content: "ok"},
		{Role: RoleAssistant, Content: "ok"},
	})
	require.NoError(t, err)
	assert.Equal(t, 3, n)

	// And re-flushing the same three still inserts nothing.
	n2, _, err := s.AppendMessages(sid, []Message{
		{Role: RoleAssistant, Content: "ok"},
		{Role: RoleAssistant, Content: "ok"},
		{Role: RoleAssistant, Content: "ok"},
	})
	require.NoError(t, err)
	assert.Zero(t, n2)
}

// TestAppendMessages_TableDriven covers the argument-validation and shape edges.
func TestAppendMessages_TableDriven(t *testing.T) {
	cases := []struct {
		name      string
		sessionID string // "" means "use the real session"
		msgs      []Message
		wantErr   bool
		wantRows  int
	}{
		{name: "empty session id", sessionID: "no-such-thing-\x00", msgs: []Message{{Role: RoleUser, Content: "x"}}, wantErr: true},
		{name: "empty batch", msgs: nil, wantRows: 0},
		{name: "one row", msgs: []Message{{Role: RoleUser, Content: "x"}}, wantRows: 1},
		{
			name: "tool call with empty content is still stored",
			msgs: []Message{{Role: RoleToolCall, ToolCallID: "c", ToolName: "t", ToolArgs: "{}"}},
			// Content is '' on purpose here; a tool_call's payload is its args.
			wantRows: 1,
		},
		{
			name: "distinct tool calls of the same tool are distinct rows",
			msgs: []Message{
				{Role: RoleToolCall, ToolCallID: "c1", ToolName: "fs_read", ToolArgs: `{"path":"a"}`},
				{Role: RoleToolCall, ToolCallID: "c2", ToolName: "fs_read", ToolArgs: `{"path":"b"}`},
			},
			wantRows: 2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := openTempStore(t)
			sid := newSession(t, s)
			target := sid
			if tc.name == "empty session id" {
				target = ""
			}
			n, _, err := s.AppendMessages(target, tc.msgs)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantRows, n)
		})
	}
}

// TestAppendMessages_AtomicOnFailure: the batch is all-or-nothing. The caller's
// whole "do not evict on write failure" logic rests on a non-nil error meaning
// NOTHING is durable — a half-written batch would leave it unable to tell what
// it is still responsible for.
func TestAppendMessages_AtomicOnFailure(t *testing.T) {
	s, _ := openTempStore(t)
	sid := newSession(t, s)
	require.NoError(t, func() error {
		_, _, e := s.AppendMessages(sid, []Message{{Role: RoleUser, Content: "first"}})
		return e
	}())

	// A row whose session_id violates the FK aborts the transaction; the good
	// rows in the same batch must not survive it.
	_, _, err := s.AppendMessages("no-such-session", []Message{
		{Role: RoleUser, Content: "doomed-a"},
		{Role: RoleUser, Content: "doomed-b"},
	})
	require.Error(t, err)

	got, err := s.Messages(sid)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "first", got[0].Content)

	var orphans int
	require.NoError(t, s.DB.QueryRow(
		"SELECT COUNT(*) FROM messages WHERE content LIKE 'doomed-%'").Scan(&orphans))
	assert.Zero(t, orphans, "a failed batch must leave no rows behind")
}

// TestAppendMessages_SeqContinuesPastLegacyRows: seq is assigned from the
// table's own watermark, so the durable log keeps growing monotonically even
// though the LIVE window shrinks on every compaction. A caller-side counter is
// what would drift here and start overwriting history.
func TestAppendMessages_SeqContinuesPastLegacyRows(t *testing.T) {
	s, _ := openTempStore(t)
	sid := newSession(t, s)
	require.NoError(t, s.AppendMessage(sid, 0, "user", "legacy-0"))
	require.NoError(t, s.AppendMessage(sid, 1, "assistant", "legacy-1"))

	_, next, err := s.AppendMessages(sid, []Message{{Role: RoleUser, Content: "new"}})
	require.NoError(t, err)
	assert.Equal(t, 3, next)

	got, err := s.Messages(sid)
	require.NoError(t, err)
	require.Len(t, got, 3)
	assert.Equal(t, 2, got[2].Seq)
	assert.Equal(t, "new", got[2].Content)
}

// TestAppendMessages_LegacyRowsDoNotCollide: pre-C1 rows all carry the empty
// string as their dedup_key, so a TOTAL unique index would make the second one
// an insert failure. The index is partial for exactly this reason, and an
// upgrade must not brick appends.
func TestAppendMessages_LegacyRowsDoNotCollide(t *testing.T) {
	s, _ := openTempStore(t)
	sid := newSession(t, s)
	for i := 0; i < 5; i++ {
		require.NoError(t, s.AppendMessage(sid, i, "user", fmt.Sprintf("legacy-%d", i)))
	}
	got, err := s.Messages(sid)
	require.NoError(t, err)
	assert.Len(t, got, 5)
	for _, m := range got {
		assert.Empty(t, m.DedupKey, "legacy path must not fabricate a dedup key")
	}
}

// TestAppendMessages_Redacts pins that the durable log goes through the same
// redactor as every other text column. Tool arguments carry credentials at
// least as often as prose does, so the new column must be covered too.
func TestAppendMessages_Redacts(t *testing.T) {
	s, _ := openTempStore(t)
	r := secrets.NewRedactor()
	r.Register("sk-supersecret")
	s.SetRedactor(r)
	sid := newSession(t, s)

	_, _, err := s.AppendMessages(sid, []Message{
		{Role: RoleUser, Content: "my key is sk-supersecret ok"},
		{Role: RoleToolCall, ToolCallID: "c", ToolName: "web_fetch",
			ToolArgs: `{"header":"Bearer sk-supersecret"}`},
	})
	require.NoError(t, err)

	got, err := s.Messages(sid)
	require.NoError(t, err)
	require.Len(t, got, 2)
	for _, m := range got {
		assert.NotContains(t, m.Content, "sk-supersecret")
		assert.NotContains(t, m.ToolArgs, "sk-supersecret")
	}
	assert.Contains(t, got[1].ToolArgs, "REDACTED")
}

// ---------------------------------------------------------------------------
// C1/C2: paging
// ---------------------------------------------------------------------------

func seedRange(t *testing.T, s *Store, sid string, n int) {
	t.Helper()
	msgs := make([]Message, 0, n)
	for i := 0; i < n; i++ {
		msgs = append(msgs, Message{Role: RoleUser, Content: fmt.Sprintf("m%03d", i)})
	}
	_, _, err := s.AppendMessages(sid, msgs)
	require.NoError(t, err)
}

func TestMessagesPage(t *testing.T) {
	s, _ := openTempStore(t)
	sid := newSession(t, s)
	seedRange(t, s, sid, 30)

	cases := []struct {
		name      string
		r         MessageRange
		wantFirst string
		wantLast  string
		wantLen   int
	}{
		{"default limit caps nothing at 30", MessageRange{SessionID: sid}, "m000", "m029", 30},
		{"explicit limit truncates from the front", MessageRange{SessionID: sid, Limit: 5}, "m000", "m004", 5},
		{"newest takes the tail", MessageRange{SessionID: sid, Limit: 5, Newest: true}, "m025", "m029", 5},
		{"from_seq", MessageRange{SessionID: sid, FromSeq: 25}, "m025", "m029", 5},
		{"to_seq is exclusive", MessageRange{SessionID: sid, FromSeq: 10, ToSeq: 13}, "m010", "m012", 3},
		{"range beyond the end is empty", MessageRange{SessionID: sid, FromSeq: 500}, "", "", 0},
		{"over-large limit is clamped, not rejected", MessageRange{SessionID: sid, Limit: 10_000}, "m000", "m029", 30},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.MessagesPage(tc.r)
			require.NoError(t, err)
			require.Len(t, got, tc.wantLen)
			if tc.wantLen == 0 {
				return
			}
			assert.Equal(t, tc.wantFirst, got[0].Content)
			assert.Equal(t, tc.wantLast, got[len(got)-1].Content)
			// Ascending seq regardless of which end was truncated.
			for i := 1; i < len(got); i++ {
				assert.Less(t, got[i-1].Seq, got[i].Seq)
			}
		})
	}
}

// TestMessagesPage_HardCap: MaxMessagePageSize is a cap, not a suggestion. A
// recall tool that could ask for the whole log would re-fill the window it
// exists to relieve.
func TestMessagesPage_HardCap(t *testing.T) {
	s, _ := openTempStore(t)
	sid := newSession(t, s)
	seedRange(t, s, sid, MaxMessagePageSize+25)

	got, err := s.MessagesPage(MessageRange{SessionID: sid, Limit: 100000})
	require.NoError(t, err)
	assert.Len(t, got, MaxMessagePageSize)
}

func TestMessagesPage_EmptySessionIsError(t *testing.T) {
	s, _ := openTempStore(t)
	_, err := s.MessagesPage(MessageRange{})
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// C2: full-text search
// ---------------------------------------------------------------------------

func TestSearchMessages(t *testing.T) {
	s, _ := openTempStore(t)
	sid := newSession(t, s)
	other := newSession(t, s)

	_, _, err := s.AppendMessages(sid, []Message{
		{Role: RoleUser, Content: "please investigate the flaky guard test"},
		{Role: RoleToolCall, ToolCallID: "c1", ToolName: "shell_run",
			ToolArgs: `{"cmd":"go test ./internal/guard"}`},
		{Role: RoleToolResult, ToolCallID: "c1", ToolName: "shell_run",
			Content: "--- FAIL: TestCheckDestructive (0.00s)\n    panic: nil map"},
		{Role: RoleAssistant, Content: "the nil map is in ClassifyDestruction"},
	})
	require.NoError(t, err)
	_, _, err = s.AppendMessages(other, []Message{
		{Role: RoleUser, Content: "a completely unrelated panic in another session"},
	})
	require.NoError(t, err)

	cases := []struct {
		name     string
		query    string
		wantSeqs []int
	}{
		{"prose term", "flaky", []int{0}},
		// The evicted tool RESULT is findable — this is the whole point of C1.
		{"term inside a tool result", "panic", []int{2}},
		// tool_args is indexed too, so a command is findable by the path it names.
		{"path inside tool args", `"internal/guard"`, []int{1}},
		{"phrase", `"nil map"`, []int{2, 3}},
		{"no match", "zzzznotpresent", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hits, err := s.SearchMessages(sid, tc.query, 0)
			require.NoError(t, err)
			var seqs []int
			for _, h := range hits {
				seqs = append(seqs, h.Seq)
				assert.Equal(t, sid, h.SessionID, "search must never cross sessions")
			}
			assert.ElementsMatch(t, tc.wantSeqs, seqs)
		})
	}
}

// TestSearchMessages_IsSessionScoped: another conversation's history is not
// searchable, even for a term that only it contains. Widening the scope would
// be a confidentiality change, not a convenience.
func TestSearchMessages_IsSessionScoped(t *testing.T) {
	s, _ := openTempStore(t)
	mine := newSession(t, s)
	theirs := newSession(t, s)
	_, _, err := s.AppendMessages(mine, []Message{{Role: RoleUser, Content: "ordinary text"}})
	require.NoError(t, err)
	_, _, err = s.AppendMessages(theirs, []Message{{Role: RoleUser, Content: "kryptonite"}})
	require.NoError(t, err)

	hits, err := s.SearchMessages(mine, "kryptonite", 0)
	require.NoError(t, err)
	assert.Empty(t, hits)

	hits, err = s.SearchMessages(theirs, "kryptonite", 0)
	require.NoError(t, err)
	assert.Len(t, hits, 1)
}

// TestSearchMessages_AcrossSessions: an empty sessionID used to be an error
// ("store: search messages: empty session id"). It now means "every
// session", which is what lets a question like "how did we fix that bug last
// week" be answered at all: the caller does not know, and should not have to
// know, which past session holds the fix.
//
// Three sessions are seeded on purpose, not two: a fixture where only one
// session has any hits cannot distinguish "scoping was silently dropped" from
// "the cross-session code path was never exercised" — the test needs to see
// rows from more than one session in a single result set to prove the search
// actually spans sessions rather than just tolerating an empty argument.
func TestSearchMessages_AcrossSessions(t *testing.T) {
	s, _ := openTempStore(t)
	a := newSession(t, s)
	b := newSession(t, s)
	c := newSession(t, s)

	_, _, err := s.AppendMessages(a, []Message{
		{Role: RoleUser, Content: "deploying the new release pipeline"},
		{Role: RoleAssistant, Content: "there was a flaky test in the guard package"},
	})
	require.NoError(t, err)
	_, _, err = s.AppendMessages(b, []Message{
		{Role: RoleUser, Content: "looking at a flaky scheduler retry"},
		{Role: RoleAssistant, Content: "root cause: ONLYINSESSIONB raced on the lock"},
	})
	require.NoError(t, err)
	_, _, err = s.AppendMessages(c, []Message{
		{Role: RoleUser, Content: "unrelated conversation about pizza toppings"},
	})
	require.NoError(t, err)

	// The old error is gone.
	hits, err := s.SearchMessages("", "flaky", 0)
	require.NoError(t, err)
	require.NotEmpty(t, hits, "cross-session search must find the term")

	seen := map[string]bool{}
	for _, h := range hits {
		require.NotEmpty(t, h.SessionID, "every hit must say which session it came from")
		seen[h.SessionID] = true
	}
	assert.True(t, seen[a], "session A's hit must be present")
	assert.True(t, seen[b], "session B's hit must be present")
	assert.False(t, seen[c], "session C never mentioned the term")
	assert.Greater(t, len(seen), 1,
		"a fixture with hits in only one session cannot prove the search is cross-session")

	// A term that exists only in session B must still be found with no
	// session filter applied.
	hits, err = s.SearchMessages("", "ONLYINSESSIONB", 0)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, b, hits[0].SessionID)

	// limit still applies to the COMBINED cross-session result set, not
	// per-session.
	hits, err = s.SearchMessages("", "flaky", 1)
	require.NoError(t, err)
	assert.Len(t, hits, 1, "limit must bound the merged result, not each session separately")
}

// TestSearchMessages_AcrossSessionsCJK: the cross-session path must go
// through the same W-A-03 CJK fallback as the single-session path
// (SearchMessages routes on hasCJK before it knows whether sessionID is
// empty). Getting this wrong is invisible in English: FTS5's default
// tokenizer would still match ASCII text, and only a Chinese query would
// silently come back empty — which is exactly the failure mode a
// NoError-only assertion would miss, so this test also pins the hit count and
// the actual content, not just the absence of an error.
func TestSearchMessages_AcrossSessionsCJK(t *testing.T) {
	s, _ := openTempStore(t)
	a := newSession(t, s)
	b := newSession(t, s)
	c := newSession(t, s)

	require.NoError(t, s.AppendMessage(a, 0, RoleUser, "项目的截止日期是周二，需要跟进"))
	require.NoError(t, s.AppendMessage(b, 0, RoleUser, "张伟说这个项目的截止日期可能推迟"))
	require.NoError(t, s.AppendMessage(c, 0, RoleUser, "今天天气很好，适合散步"))

	hits, err := s.SearchMessages("", "截止日期", 0)
	require.NoError(t, err)
	require.Len(t, hits, 2, "the term appears in exactly two sessions")

	bySession := map[string]MessageSearchHit{}
	for _, h := range hits {
		bySession[h.SessionID] = h
	}
	require.Contains(t, bySession, a)
	require.Contains(t, bySession, b)
	assert.Equal(t, "项目的截止日期是周二，需要跟进", bySession[a].Content)
	assert.Equal(t, "张伟说这个项目的截止日期可能推迟", bySession[b].Content)
	assert.Contains(t, bySession[a].Snippet, "截止日期")
	assert.Contains(t, bySession[b].Snippet, "截止日期")
}

// TestSearchMessages_Rejects: an empty QUERY is still an error (there is
// nothing to search for). An empty SESSION id is no longer one of these
// cases — see TestSearchMessages_AcrossSessions — so this test only pins the
// query-side validation now.
func TestSearchMessages_Rejects(t *testing.T) {
	s, _ := openTempStore(t)
	sid := newSession(t, s)
	_, err := s.SearchMessages(sid, "   ", 0)
	assert.Error(t, err, "empty query must not match everything")
}

// TestSearchMessages_SnippetMarksTheMatch: the snippet is what a recall tool
// shows instead of a multi-megabyte body, so it has to actually contain the hit.
func TestSearchMessages_SnippetMarksTheMatch(t *testing.T) {
	s, _ := openTempStore(t)
	sid := newSession(t, s)
	long := strings.Repeat("filler line\n", 400) + "the DISTINCTIVEWORD is here\n" +
		strings.Repeat("more filler\n", 400)
	_, _, err := s.AppendMessages(sid, []Message{
		{Role: RoleToolResult, ToolCallID: "c", ToolName: "shell_run", Content: long},
	})
	require.NoError(t, err)

	hits, err := s.SearchMessages(sid, "DISTINCTIVEWORD", 0)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Contains(t, strings.ToLower(hits[0].Snippet), "distinctiveword")
	assert.Less(t, len(hits[0].Snippet), len(hits[0].Content),
		"the snippet exists so the whole body need not be returned")
}

// TestSearchMessages_SurvivesDelete: the FTS triggers keep the index in step
// with the table. A stale index would return hits whose rows no longer exist,
// and the JOIN would silently drop them — i.e. a search that finds nothing for
// no visible reason.
func TestSearchMessages_SurvivesDelete(t *testing.T) {
	s, _ := openTempStore(t)
	sid := newSession(t, s)
	_, _, err := s.AppendMessages(sid, []Message{{Role: RoleUser, Content: "ephemeral marker"}})
	require.NoError(t, err)
	hits, err := s.SearchMessages(sid, "ephemeral", 0)
	require.NoError(t, err)
	require.Len(t, hits, 1)

	require.NoError(t, s.DeleteSession(sid))
	hits, err = s.SearchMessages(sid, "ephemeral", 0)
	require.NoError(t, err)
	assert.Empty(t, hits)
}

// ---------------------------------------------------------------------------
// Migration: old databases keep working AND become searchable
// ---------------------------------------------------------------------------

// legacySchema is the pre-C1 shape of the two tables this migration touches,
// verbatim from the schema constant before the durable log existed. Kept as a
// literal so the test builds a genuinely old database rather than asking the
// current code to describe what "old" was.
const legacySchema = `
CREATE TABLE sessions (
    id                   TEXT PRIMARY KEY,
    title                TEXT NOT NULL DEFAULT '',
    created_at           INTEGER NOT NULL,
    updated_at           INTEGER NOT NULL
);
CREATE TABLE messages (
    id         TEXT PRIMARY KEY,
    session_id TEXT NOT NULL,
    seq        INTEGER NOT NULL,
    role       TEXT NOT NULL,
    content    TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    FOREIGN KEY (session_id) REFERENCES sessions(id)
);
CREATE TABLE memories (
    id         TEXT PRIMARY KEY,
    kind       TEXT NOT NULL DEFAULT 'note',
    content    TEXT NOT NULL,
    created_at INTEGER NOT NULL
);
`

// writeLegacyDB creates a pre-C1 database file with real data in it and returns
// its path.
func writeLegacyDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	_, err = db.Exec(legacySchema)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO sessions (id, title, created_at, updated_at) VALUES ('old-s', 'legacy chat', 100, 100)`)
	require.NoError(t, err)
	for i, c := range []string{"legacy user question about goroutines", "legacy assistant answer"} {
		role := "user"
		if i == 1 {
			role = "assistant"
		}
		_, err = db.Exec(
			`INSERT INTO messages (id, session_id, seq, role, content, created_at) VALUES (?, 'old-s', ?, ?, ?, 100)`,
			fmt.Sprintf("old-m%d", i), i, role, c)
		require.NoError(t, err)
	}
	_, err = db.Exec(`INSERT INTO memories (id, kind, content, created_at) VALUES ('old-mem', 'note', 'legacy memory', 100)`)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	return path
}

// TestMigration_LegacyDatabaseStillReadable is the upgrade-safety assertion the
// whole migration hinges on: opening a database that predates every C1/C14
// column must preserve its rows, not just avoid crashing.
func TestMigration_LegacyDatabaseStillReadable(t *testing.T) {
	path := writeLegacyDB(t)
	s, err := Open(path)
	require.NoError(t, err, "opening a pre-C1 database must succeed")
	defer s.Close()

	msgs, err := s.Messages("old-s")
	require.NoError(t, err)
	require.Len(t, msgs, 2, "legacy messages must survive the migration")
	assert.Equal(t, "legacy user question about goroutines", msgs[0].Content)
	assert.Equal(t, "legacy assistant answer", msgs[1].Content)
	for _, m := range msgs {
		assert.Empty(t, m.ToolCallID)
		assert.Empty(t, m.ToolName)
		assert.Empty(t, m.ToolArgs)
		assert.Empty(t, m.DedupKey)
	}

	mems, err := s.RecallMemory(10)
	require.NoError(t, err)
	require.Len(t, mems, 1)
	assert.Equal(t, "legacy memory", mems[0].Content)
	assert.Empty(t, mems[0].SessionID, "pre-C14 rows carry no dimensions")
	assert.Empty(t, mems[0].AgentID)
}

// TestMigration_LegacyRowsBecomeSearchable: the FTS rebuild runs once on
// upgrade. Without it the index would only ever contain messages written AFTER
// the upgrade, so a user's existing history would be permanently unsearchable
// while every test using a fresh database passed.
func TestMigration_LegacyRowsBecomeSearchable(t *testing.T) {
	path := writeLegacyDB(t)
	s, err := Open(path)
	require.NoError(t, err)
	defer s.Close()

	hits, err := s.SearchMessages("old-s", "goroutines", 0)
	require.NoError(t, err)
	require.Len(t, hits, 1, "pre-existing messages must be indexed by the upgrade")
	assert.Equal(t, 0, hits[0].Seq)
}

// TestMigration_IsIdempotent: opening the same database repeatedly must not
// fail, duplicate index entries, or re-run the rebuild into double hits.
func TestMigration_IsIdempotent(t *testing.T) {
	path := writeLegacyDB(t)
	for i := 0; i < 3; i++ {
		s, err := Open(path)
		require.NoError(t, err, "open #%d", i)
		hits, err := s.SearchMessages("old-s", "goroutines", 0)
		require.NoError(t, err)
		assert.Len(t, hits, 1, "open #%d must not duplicate index entries", i)
		require.NoError(t, s.Close())
	}
}

// TestMigration_AppendsWorkOnUpgradedDatabase closes the loop: after an upgrade,
// the NEW write path works against rows written by the OLD one, and seq
// continues from the legacy watermark rather than colliding with it.
func TestMigration_AppendsWorkOnUpgradedDatabase(t *testing.T) {
	path := writeLegacyDB(t)
	s, err := Open(path)
	require.NoError(t, err)
	defer s.Close()

	n, next, err := s.AppendMessages("old-s", []Message{
		{Role: RoleToolCall, ToolCallID: "c1", ToolName: "fs_read", ToolArgs: `{"path":"main.go"}`},
		{Role: RoleToolResult, ToolCallID: "c1", ToolName: "fs_read", Content: "package main"},
	})
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	assert.Equal(t, 4, next)

	msgs, err := s.Messages("old-s")
	require.NoError(t, err)
	require.Len(t, msgs, 4)
	assert.Equal(t, []int{0, 1, 2, 3}, []int{msgs[0].Seq, msgs[1].Seq, msgs[2].Seq, msgs[3].Seq})
	assert.Equal(t, RoleToolCall, msgs[2].Role)
	assert.Equal(t, "fs_read", msgs[2].ToolName)
}

// TestMigration_MessageLogColumnsAllApplied pins the column set against the
// live table, so a column added to messageLogColumns without a matching schema
// entry (or vice versa) is caught rather than surfacing as a scan error later.
func TestMigration_MessageLogColumnsAllApplied(t *testing.T) {
	path := writeLegacyDB(t)
	s, err := Open(path)
	require.NoError(t, err)
	defer s.Close()

	cols, err := s.columns("messages")
	require.NoError(t, err)
	have := map[string]bool{}
	for _, c := range cols {
		have[c] = true
	}
	for _, c := range messageLogColumns {
		assert.True(t, have[c.Col], "migration must add messages.%s", c.Col)
	}
}

// ---------------------------------------------------------------------------
// Round-trip through the snapshot / fork paths
// ---------------------------------------------------------------------------

// TestSnapshotRestore_PreservesToolFields: undo must not quietly strip the tool
// rows it restores. The snapshot path had its own hand-written column list, so
// adding columns to `messages` alone would have left it copying role+content
// and dropping everything C1 added.
func TestSnapshotRestore_PreservesToolFields(t *testing.T) {
	s, _ := openTempStore(t)
	sid := newSession(t, s)
	_, _, err := s.AppendMessages(sid, []Message{
		{Role: RoleUser, Content: "go"},
		{Role: RoleToolCall, ToolCallID: "c9", ToolName: "shell_run", ToolArgs: `{"cmd":"ls"}`},
		{Role: RoleToolResult, ToolCallID: "c9", ToolName: "shell_run", Content: "a.go b.go"},
	})
	require.NoError(t, err)

	snap, err := s.SnapshotSessionForRevert(sid)
	require.NoError(t, err)
	require.Len(t, snap.Messages, 3)
	assert.Equal(t, "c9", snap.Messages[1].ToolCallID)
	assert.Equal(t, `{"cmd":"ls"}`, snap.Messages[1].ToolArgs)

	_, err = s.TruncateSessionForRevert(sid, 0, 0)
	require.NoError(t, err)
	require.NoError(t, s.RestoreSessionAfterFailedRevert(snap))

	got, err := s.Messages(sid)
	require.NoError(t, err)
	require.Len(t, got, 3)
	assert.Equal(t, "c9", got[1].ToolCallID)
	assert.Equal(t, "shell_run", got[1].ToolName)
	assert.Equal(t, `{"cmd":"ls"}`, got[1].ToolArgs)
	assert.Equal(t, RoleToolResult, got[2].Role)
	assert.Equal(t, "a.go b.go", got[2].Content)
}

// TestForkSession_PreservesToolFields: same hazard on the fork path, which also
// had its own column list. A fork that drops tool rows' payloads loses exactly
// the data C1 exists to keep.
func TestForkSession_PreservesToolFields(t *testing.T) {
	s, _ := openTempStore(t)
	sid := newSession(t, s)
	_, _, err := s.AppendMessages(sid, []Message{
		{Role: RoleUser, Content: "go"},
		{Role: RoleToolCall, ToolCallID: "cf", ToolName: "fs_write", ToolArgs: `{"path":"x"}`},
		{Role: RoleToolResult, ToolCallID: "cf", ToolName: "fs_write", Content: "wrote 12 bytes"},
	})
	require.NoError(t, err)

	forkID, err := s.ForkSession(sid, -1)
	require.NoError(t, err)
	got, err := s.Messages(forkID)
	require.NoError(t, err)
	require.Len(t, got, 3)
	assert.Equal(t, "cf", got[1].ToolCallID)
	assert.Equal(t, "fs_write", got[1].ToolName)
	assert.Equal(t, `{"path":"x"}`, got[1].ToolArgs)
	assert.Equal(t, "wrote 12 bytes", got[2].Content)
	// The fork's rows DO inherit dedup keys, and the rule was reversed
	// deliberately (INF3 / ADR-0015). The keys say "this message is already
	// durable in this session", which is exactly true of a copied row — and
	// leaving them empty made the partial unique index skip them, so the fork's
	// first whole-window flush re-inserted its entire history. Measured with an
	// inherited compaction boundary: 12 rows and a 4-message window became 16
	// rows and 8 messages, every message twice, on the first turn after /fork.
	// The unique index is (session_id, dedup_key), so copying across sessions
	// cannot collide.
	for _, m := range got {
		assert.NotEmpty(t, m.DedupKey, "fork rows must inherit the source's dedup keys")
	}
}

// TestForkThenAppend_DoesNotSkipRows: a fork must still be able to record a new
// message that happens to be byte-identical to one it inherited.
//
// Inheriting the keys (see above) is what makes this worth pinning, because the
// naive reading is that the inherited row now suppresses the new one. It does
// not, and the reason is the shape the two production callers actually use:
//
//   - flushHistory re-flushes the WHOLE window, so AssignDedupKeys sees both
//     copies in one batch and gives the second one ordinal 1 — a different key.
//   - tools.milestone supplies its own nonce'd key, so it never derives one
//     from content at all.
//
// A caller that appends ONLY the new duplicate, with no key and without its
// predecessor in the batch, cannot be distinguished from a re-flush and is
// skipped. That is the pre-existing meaning of a content-derived key, not a
// fork-specific hazard: the same call on the SOURCE session is skipped too.
func TestForkThenAppend_DoesNotSkipRows(t *testing.T) {
	s, _ := openTempStore(t)
	sid := newSession(t, s)
	_, _, err := s.AppendMessages(sid, []Message{{Role: RoleUser, Content: "same text"}})
	require.NoError(t, err)

	forkID, err := s.ForkSession(sid, -1)
	require.NoError(t, err)

	// The whole-window shape, i.e. what flushHistory does.
	n, _, err := s.AppendMessages(forkID, []Message{
		{Role: RoleUser, Content: "same text"},
		{Role: RoleUser, Content: "same text"},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, n, "an inherited row must not suppress a genuinely new one")

	got, err := s.Messages(forkID)
	require.NoError(t, err)
	assert.Len(t, got, 2, "the inherited row is kept once and the new one is added")

	// The explicit-key shape, i.e. what tools.milestone does.
	n, _, err = s.AppendMessages(forkID, []Message{
		{Role: RoleUser, Content: "same text", DedupKey: "explicit:1"},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, n, "an explicit key is never suppressed by an inherited row")
}

// TestForkThenFlushDoesNotDuplicateTheLog is the regression the rule reversal
// exists for: re-flushing an unchanged inherited window must write nothing.
func TestForkThenFlushDoesNotDuplicateTheLog(t *testing.T) {
	s, _ := openTempStore(t)
	sid := newSession(t, s)
	window := []Message{
		{Role: RoleUser, Content: "do the thing"},
		{Role: RoleAssistant, Content: "on it"},
		{Role: RoleToolCall, ToolCallID: "c1", ToolName: "shell_run", ToolArgs: `{"cmd":"ls"}`},
		{Role: RoleToolResult, ToolCallID: "c1", ToolName: "shell_run", Content: "a.go"},
	}
	_, _, err := s.AppendMessages(sid, window)
	require.NoError(t, err)

	forkID, err := s.ForkSession(sid, -1)
	require.NoError(t, err)
	inserted, _, err := s.AppendMessages(forkID, window)
	require.NoError(t, err)
	assert.Zero(t, inserted, "the inherited window is already durable in the fork")

	got, err := s.Messages(forkID)
	require.NoError(t, err)
	assert.Len(t, got, len(window), "a re-flush must not double the fork's log")
}

// ---------------------------------------------------------------------------
// AssignDedupKeys
// ---------------------------------------------------------------------------

func TestAssignDedupKeys(t *testing.T) {
	cases := []struct {
		name     string
		in       []Message
		wantSame bool // true = all keys equal, false = all keys distinct
	}{
		{
			name:     "distinct content yields distinct keys",
			in:       []Message{{Role: RoleUser, Content: "a"}, {Role: RoleUser, Content: "b"}},
			wantSame: false,
		},
		{
			name:     "identical siblings yield distinct keys",
			in:       []Message{{Role: RoleUser, Content: "a"}, {Role: RoleUser, Content: "a"}},
			wantSame: false,
		},
		{
			name: "role participates in the key",
			in: []Message{
				{Role: RoleUser, Content: "same"},
				{Role: RoleAssistant, Content: "same"},
			},
			wantSame: false,
		},
		{
			name: "tool args participate in the key",
			in: []Message{
				{Role: RoleToolCall, ToolName: "t", ToolArgs: `{"a":1}`},
				{Role: RoleToolCall, ToolName: "t", ToolArgs: `{"a":2}`},
			},
			wantSame: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msgs := append([]Message(nil), tc.in...)
			AssignDedupKeys(msgs)
			seen := map[string]bool{}
			for _, m := range msgs {
				require.NotEmpty(t, m.DedupKey)
				seen[m.DedupKey] = true
			}
			assert.Len(t, seen, len(msgs))
		})
	}
}

// TestAssignDedupKeys_Deterministic: the same window must produce the same keys
// on every flush, or idempotence is a coin flip.
func TestAssignDedupKeys_Deterministic(t *testing.T) {
	build := func() []Message {
		return []Message{
			{Role: RoleUser, Content: "q"},
			{Role: RoleToolCall, ToolCallID: "c", ToolName: "t", ToolArgs: "{}"},
			{Role: RoleAssistant, Content: "a"},
			{Role: RoleAssistant, Content: "a"},
		}
	}
	a, b := build(), build()
	AssignDedupKeys(a)
	AssignDedupKeys(b)
	for i := range a {
		assert.Equal(t, a[i].DedupKey, b[i].DedupKey, "row %d", i)
	}
}

// TestAssignDedupKeys_PreservesExplicitKeys: a caller that supplies its own key
// (a replay, a migration) keeps it.
func TestAssignDedupKeys_PreservesExplicitKeys(t *testing.T) {
	msgs := []Message{{Role: RoleUser, Content: "x", DedupKey: "mine"}, {Role: RoleUser, Content: "y"}}
	AssignDedupKeys(msgs)
	assert.Equal(t, "mine", msgs[0].DedupKey)
	assert.NotEmpty(t, msgs[1].DedupKey)
}

// TestAssignDedupKeys_HugeBodiesStayDistinct: the fingerprint truncates its
// input at maxDedupSourceBytes, so two messages sharing a long prefix must
// still be told apart by the length component.
func TestAssignDedupKeys_HugeBodiesStayDistinct(t *testing.T) {
	prefix := strings.Repeat("x", maxDedupSourceBytes+100)
	msgs := []Message{
		{Role: RoleToolResult, Content: prefix + "ENDING-A"},
		{Role: RoleToolResult, Content: prefix + "A-DIFFERENT-ENDING"},
	}
	AssignDedupKeys(msgs)
	assert.NotEqual(t, msgs[0].DedupKey, msgs[1].DedupKey)
}

func TestPrefixed(t *testing.T) {
	assert.Equal(t, "m.a, m.b, m.c", prefixed("a, b, c", "m."))
	assert.Equal(t, "a", prefixed("a", ""))
}
