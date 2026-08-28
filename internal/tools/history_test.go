package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/approval"
	"github.com/x6nux/yanshi/internal/store"
)

func newHistoryStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "h.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// historyCtx binds a session id the way production does: through the approval
// context, which is where the WS connection's session id enters the tool layer.
func historyCtx(t *testing.T, sessionID string) context.Context {
	t.Helper()
	return WithApprovalManager(context.Background(),
		&approval.Manager{}, sessionID)
}

func seedHistory(t *testing.T, s *store.Store) string {
	t.Helper()
	sid, err := s.CreateSession("history test")
	require.NoError(t, err)
	_, _, err = s.AppendMessages(sid, []store.Message{
		{Role: store.RoleUser, Content: "why does the guard test fail"},
		{Role: store.RoleToolCall, ToolCallID: "c1", ToolName: "shell_run",
			ToolArgs: `{"cmd":"go test ./internal/guard"}`},
		{Role: store.RoleToolResult, ToolCallID: "c1", ToolName: "shell_run",
			Content: "--- FAIL: TestClassify\n    nil pointer dereference in lexShellLite"},
		{Role: store.RoleAssistant, Content: "the lexer dereferences an empty segment"},
	})
	require.NoError(t, err)
	return sid
}

func runHistoryTool(t *testing.T, ctx context.Context, fn func(context.Context, string) (string, error), args any) (string, error) {
	t.Helper()
	b, err := json.Marshal(args)
	require.NoError(t, err)
	return fn(ctx, string(b))
}

// ---------------------------------------------------------------------------
// C2: history_search
// ---------------------------------------------------------------------------

// TestHistorySearch_FindsEvictedToolOutput is the point of C2: the model can
// retrieve the content of a tool result that is no longer in its window. If
// this only ever found prose, the durable log's tool rows would have a writer
// and no reader.
func TestHistorySearch_FindsEvictedToolOutput(t *testing.T) {
	s := newHistoryStore(t)
	sid := seedHistory(t, s)
	ht := NewHistoryTools(s)

	out, err := runHistoryTool(t, historyCtx(t, sid), ht.runSearch,
		map[string]any{"query": "dereference"})
	require.NoError(t, err)
	assert.Contains(t, out, "#2")
	assert.Contains(t, out, "shell_run")
	assert.Contains(t, strings.ToLower(out), "dereference")
}

func TestHistorySearch_TableDriven(t *testing.T) {
	s := newHistoryStore(t)
	sid := seedHistory(t, s)
	ht := NewHistoryTools(s)
	ctx := historyCtx(t, sid)

	cases := []struct {
		name        string
		args        map[string]any
		wantErr     bool
		wantContain []string
		wantAbsent  []string
	}{
		{
			name:        "prose term",
			args:        map[string]any{"query": "lexer"},
			wantContain: []string{"#3", "assistant"},
		},
		{
			name: "term inside tool arguments",
			args: map[string]any{"query": `"internal/guard"`},
			// tool_args is indexed, so a command is findable by the path it names.
			wantContain: []string{"#1", "shell_run"},
		},
		{
			name:        "no match says so rather than returning nothing",
			args:        map[string]any{"query": "zzzznotpresent"},
			wantContain: []string{"No matching messages"},
		},
		{
			name:    "empty query is refused",
			args:    map[string]any{"query": "   "},
			wantErr: true,
		},
		{
			name:    "missing query is refused",
			args:    map[string]any{},
			wantErr: true,
		},
		{
			name:        "limit bounds the result",
			args:        map[string]any{"query": "the OR guard OR nil", "limit": 1},
			wantContain: []string{"1 match(es)"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runHistoryTool(t, ctx, ht.runSearch, tc.args)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			for _, want := range tc.wantContain {
				assert.Contains(t, out, want)
			}
			for _, absent := range tc.wantAbsent {
				assert.NotContains(t, out, absent)
			}
		})
	}
}

// TestHistorySearch_MalformedFTSQueryIsRecoverable: an FTS5 syntax error is the
// model's mistake and is fixable by rephrasing, so it must come back as an
// instruction rather than an opaque SQL error.
func TestHistorySearch_MalformedFTSQueryIsRecoverable(t *testing.T) {
	s := newHistoryStore(t)
	sid := seedHistory(t, s)
	ht := NewHistoryTools(s)

	_, err := runHistoryTool(t, historyCtx(t, sid), ht.runSearch,
		map[string]any{"query": `unbalanced "quote`})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "FTS5 syntax")
}

// ---------------------------------------------------------------------------
// history_search: all_sessions
// ---------------------------------------------------------------------------

// TestHistorySearch_AllSessionsFindsOtherSessions is the actual point of
// task-2's redo: store.SearchMessages("", ...) already searched every
// session, but the only production caller (this tool) had no way to ask for
// that — the store-layer fix had no reachable path. This drives the fix
// through the tool, the way the model actually calls it, and requires a real
// hit from a session OTHER than the current one, not just a lack of error.
func TestHistorySearch_AllSessionsFindsOtherSessions(t *testing.T) {
	s := newHistoryStore(t)
	mine := seedHistory(t, s) // contains no mention of the term below

	elsewhere, err := s.CreateSession("last week")
	require.NoError(t, err)
	_, _, err = s.AppendMessages(elsewhere, []store.Message{
		{Role: store.RoleAssistant, Content: "the fix was ONLYINANOTHERSESSION, a retry with backoff"},
	})
	require.NoError(t, err)

	// A third session that must NOT show up, so a fixture with hits in only
	// two sessions can't be mistaken for "every session matched".
	unrelated, err := s.CreateSession("unrelated")
	require.NoError(t, err)
	_, _, err = s.AppendMessages(unrelated, []store.Message{
		{Role: store.RoleUser, Content: "completely different conversation"},
	})
	require.NoError(t, err)

	ht := NewHistoryTools(s)
	out, err := runHistoryTool(t, historyCtx(t, mine), ht.runSearch,
		map[string]any{"query": "ONLYINANOTHERSESSION", "all_sessions": true})
	require.NoError(t, err)
	assert.Contains(t, out, "ONLYINANOTHERSESSION", "the current session never said this")
	assert.Contains(t, out, "[session "+elsewhere+"]",
		"a cross-session hit must name the session it came from")
	assert.NotContains(t, out, unrelated)
}

// TestHistorySearch_AllSessionsDefaultsToFalse pins that omitting all_sessions
// (the zero value) leaves history_search exactly as scoped as before this
// change: only the current session's rows are visible, and a term that exists
// only in another session is reported as a miss, not silently dropped from a
// wider result set. This is the regression the coordinator's fix-round exists
// to prevent: an opt-in whose default quietly widens anyway.
func TestHistorySearch_AllSessionsDefaultsToFalse(t *testing.T) {
	s := newHistoryStore(t)
	mine := seedHistory(t, s)

	elsewhere, err := s.CreateSession("last week")
	require.NoError(t, err)
	_, _, err = s.AppendMessages(elsewhere, []store.Message{
		{Role: store.RoleAssistant, Content: "ONLYINANOTHERSESSION was the fix"},
	})
	require.NoError(t, err)

	ht := NewHistoryTools(s)
	out, err := runHistoryTool(t, historyCtx(t, mine), ht.runSearch,
		map[string]any{"query": "ONLYINANOTHERSESSION"})
	require.NoError(t, err)
	assert.Equal(t, "No matching messages in this conversation's history.", out,
		"default scope must be byte-identical to pre-fix behaviour")
	assert.NotContains(t, out, "ONLYINANOTHERSESSION")
}

// TestHistorySearch_AllSessionsWorksWithNoCurrentSession pins the choice made
// for the case historySearchArgs.AllSessions's doc comment does not itself
// resolve: what happens when all_sessions=true and ctx carries no current
// session at all (no approval manager, no thread link). The chosen behaviour
// is "search everything anyway" — cross-session search has no notion of a
// current session to require, and forcing historySessionID to succeed first
// would make the feature fail in precisely the case it was built for ("what
// did we do in some earlier session", asked from a context that is not
// itself one of them). Contrast with TestHistoryTools_RequireASession, which
// pins the opposite (hard error) for the SCOPED, all_sessions=false path.
func TestHistorySearch_AllSessionsWorksWithNoCurrentSession(t *testing.T) {
	s := newHistoryStore(t)
	_, err := s.CreateSession("old one")
	require.NoError(t, err)
	sid2, err := s.CreateSession("old two")
	require.NoError(t, err)
	_, _, err = s.AppendMessages(sid2, []store.Message{
		{Role: store.RoleUser, Content: "a note from a prior session"},
	})
	require.NoError(t, err)

	ht := NewHistoryTools(s)
	out, err := runHistoryTool(t, context.Background(), ht.runSearch,
		map[string]any{"query": "prior", "all_sessions": true})
	require.NoError(t, err, "all_sessions search must not require a current session")
	// The FTS5 snippet wraps the matched term in «» markers, so assert on the
	// surrounding text rather than the literal seeded sentence.
	assert.Contains(t, out, "a note from a")
	assert.Contains(t, out, "session")
	assert.Contains(t, out, "prior")
}

// TestHistoryTools_RequireASession: with no session bound there is nothing to
// search, and saying so is the only honest answer. Silently returning "no
// results" would read as "your history is empty".
func TestHistoryTools_RequireASession(t *testing.T) {
	s := newHistoryStore(t)
	ht := NewHistoryTools(s)

	_, err := runHistoryTool(t, context.Background(), ht.runSearch, map[string]any{"query": "x"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no conversation")

	_, err = runHistoryTool(t, context.Background(), ht.runRead, map[string]any{})
	require.Error(t, err)
}

// TestHistoryTools_SessionComesFromContextNotArgs is a SECURITY assertion. A
// session_id parameter would let a model that saw another conversation's id in
// a log line read that conversation's whole history, evicted tool output
// included. The tools must ignore any such argument.
func TestHistoryTools_SessionComesFromContextNotArgs(t *testing.T) {
	s := newHistoryStore(t)
	mine := seedHistory(t, s)

	other, err := s.CreateSession("someone else")
	require.NoError(t, err)
	_, _, err = s.AppendMessages(other, []store.Message{
		{Role: store.RoleUser, Content: "confidential kryptonite plans"},
	})
	require.NoError(t, err)

	ht := NewHistoryTools(s)
	ctx := historyCtx(t, mine)

	// Passing the other session's id every way a caller could try.
	for _, args := range []map[string]any{
		{"query": "kryptonite", "session_id": other},
		{"query": "kryptonite", "session": other},
		{"query": "kryptonite", "sessionID": other},
	} {
		out, err := runHistoryTool(t, ctx, ht.runSearch, args)
		require.NoError(t, err)
		assert.NotContains(t, out, "kryptonite",
			"a session argument must never widen the scope: %v", args)
	}

	out, err := runHistoryTool(t, ctx, ht.runRead, map[string]any{"session_id": other})
	require.NoError(t, err)
	assert.NotContains(t, out, "kryptonite")
}

// TestHistorySessionID_FallsBackToThreadLink: the approval manager is absent
// when approvals are unconfigured, but the WS turn still sets ThreadID to the
// session id. Without the fallback these tools would be dead on that config.
func TestHistorySessionID_FallsBackToThreadLink(t *testing.T) {
	ctx := WithThreadLink(context.Background(), "thread-as-session", "turn-1")
	got, err := historySessionID(ctx)
	require.NoError(t, err)
	assert.Equal(t, "thread-as-session", got)
}

// TestHistorySessionID_ApprovalWins pins the precedence, so a stale thread link
// cannot redirect a read at a different conversation.
func TestHistorySessionID_ApprovalWins(t *testing.T) {
	ctx := WithApprovalManager(context.Background(), &approval.Manager{}, "approval-session")
	ctx = WithThreadLink(ctx, "thread-session", "turn-1")
	got, err := historySessionID(ctx)
	require.NoError(t, err)
	assert.Equal(t, "approval-session", got)
}

// ---------------------------------------------------------------------------
// C2: history_read
// ---------------------------------------------------------------------------

func TestHistoryRead_TableDriven(t *testing.T) {
	s := newHistoryStore(t)
	sid := seedHistory(t, s)
	ht := NewHistoryTools(s)
	ctx := historyCtx(t, sid)

	cases := []struct {
		name        string
		args        map[string]any
		wantErr     bool
		wantContain []string
		wantAbsent  []string
	}{
		{
			name:        "whole log by default",
			args:        map[string]any{},
			wantContain: []string{"#0", "#1", "#2", "#3"},
		},
		{
			name:        "from_seq",
			args:        map[string]any{"from_seq": 2},
			wantContain: []string{"#2", "#3"},
			wantAbsent:  []string{"#0", "#1"},
		},
		{
			name:        "to_seq is exclusive",
			args:        map[string]any{"from_seq": 0, "to_seq": 2},
			wantContain: []string{"#0", "#1"},
			wantAbsent:  []string{"#2", "#3"},
		},
		{
			name:        "limit takes the front of the range",
			args:        map[string]any{"limit": 2},
			wantContain: []string{"#0", "#1"},
			wantAbsent:  []string{"#3"},
		},
		{
			name:        "newest takes the tail",
			args:        map[string]any{"limit": 2, "newest": true},
			wantContain: []string{"#2", "#3"},
			wantAbsent:  []string{"#0"},
		},
		{
			name:        "empty range says so",
			args:        map[string]any{"from_seq": 900},
			wantContain: []string{"No messages in that range"},
		},
		{
			name:    "negative from_seq is refused",
			args:    map[string]any{"from_seq": -1},
			wantErr: true,
		},
		{
			name:    "inverted range is refused rather than silently empty",
			args:    map[string]any{"from_seq": 5, "to_seq": 2},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runHistoryTool(t, ctx, ht.runRead, tc.args)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			for _, want := range tc.wantContain {
				assert.Contains(t, out, want)
			}
			for _, absent := range tc.wantAbsent {
				assert.NotContains(t, out, absent)
			}
		})
	}
}

// TestHistoryRead_ShowsToolArgumentsForACall: a tool_call row's payload is its
// arguments; rendering only Content would show an empty body for every call.
func TestHistoryRead_ShowsToolArgumentsForACall(t *testing.T) {
	s := newHistoryStore(t)
	sid := seedHistory(t, s)
	ht := NewHistoryTools(s)

	out, err := runHistoryTool(t, historyCtx(t, sid), ht.runRead,
		map[string]any{"from_seq": 1, "to_seq": 2})
	require.NoError(t, err)
	assert.Contains(t, out, "go test ./internal/guard")
}

// TestHistoryRead_TruncatesHugeBodies: a recall tool that can return a
// megabyte-sized evicted result would re-fill the window it exists to relieve.
// The cap is the feature, not a nicety.
func TestHistoryRead_TruncatesHugeBodies(t *testing.T) {
	s := newHistoryStore(t)
	sid, err := s.CreateSession("big")
	require.NoError(t, err)
	huge := strings.Repeat("verbose build output line\n", 20000)
	_, _, err = s.AppendMessages(sid, []store.Message{
		{Role: store.RoleToolResult, ToolCallID: "c", ToolName: "shell_run", Content: huge},
	})
	require.NoError(t, err)

	ht := NewHistoryTools(s)
	out, err := runHistoryTool(t, historyCtx(t, sid), ht.runRead, map[string]any{})
	require.NoError(t, err)
	assert.Less(t, len(out), len(huge)/2, "a huge body must not be returned whole")
	assert.Contains(t, out, "truncated")
}

// TestHistoryRead_TotalBudgetIsEnforcedAndAnnounced: the per-message cap alone
// still lets many messages back in at once. When the total cap stops the render
// it must SAY so and name the resume point — a silent cut reads as "the history
// ends here", which the model would act on.
func TestHistoryRead_TotalBudgetIsEnforcedAndAnnounced(t *testing.T) {
	s := newHistoryStore(t)
	sid, err := s.CreateSession("many")
	require.NoError(t, err)
	var msgs []store.Message
	for i := 0; i < 40; i++ {
		msgs = append(msgs, store.Message{
			Role:    store.RoleToolResult,
			Content: fmt.Sprintf("msg-%02d ", i) + strings.Repeat("z", 1500),
		})
	}
	_, _, err = s.AppendMessages(sid, msgs)
	require.NoError(t, err)

	ht := NewHistoryTools(s)
	out, err := runHistoryTool(t, historyCtx(t, sid), ht.runRead, map[string]any{"limit": 40})
	require.NoError(t, err)
	assert.Contains(t, out, "not shown")
	assert.Contains(t, out, "from_seq=")
	assert.Less(t, len(out), historyTotalBudget*2)
}

// TestHistoryTools_AreRegisterableNames pins the tool names, which appear in
// the composition root's profile allow list. A rename here without one there is
// a phantom name (GOV5).
func TestHistoryTools_AreRegisterableNames(t *testing.T) {
	ht := NewHistoryTools(newHistoryStore(t))
	var names []string
	for _, tool := range ht.Tools() {
		info, err := tool.Info(context.Background())
		require.NoError(t, err)
		names = append(names, info.Name)
	}
	assert.ElementsMatch(t, []string{"history_search", "history_read"}, names)
}
