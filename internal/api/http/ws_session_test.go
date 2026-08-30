package http

import (
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/store"
)

// newSessionTestServer wires a store-backed WS server for session-lifecycle tests.
func newSessionTestServer(t *testing.T) (*store.Store, *Server) {
	t.Helper()
	st, err := store.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })
	o, err := orchestrator.New(orchestrator.Config{Model: einollm.NewFakeModel([]string{"x"}, nil)})
	require.NoError(t, err)
	s := New(Config{Token: "t", Store: st})
	s.ChatWS(o, nil, nil)
	return st, s
}

func dialWSURL(t *testing.T, ts *httptest.Server) string {
	t.Helper()
	return "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/v1/chat/ws"
}

func TestChatWS_RenameSession(t *testing.T) {
	st, s := newSessionTestServer(t)
	sid, err := st.CreateSession("old")
	require.NoError(t, err)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	c := dial(t, dialWSURL(t, ts))
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewRenameSession(sid, "new title")))
	f := readFrame(t, c)
	assert.Equal(t, "session_ack", f.Type)
	assert.Equal(t, "renamed", f.Action)
	assert.Equal(t, sid, f.SessionID)
	assert.Equal(t, "new title", f.Text)

	ss, err := st.GetSession(sid)
	require.NoError(t, err)
	require.NotNil(t, ss)
	assert.Equal(t, "new title", ss.Title)
}

func TestChatWS_ArchiveThenUnarchive(t *testing.T) {
	st, s := newSessionTestServer(t)
	sid, err := st.CreateSession("hide me")
	require.NoError(t, err)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	c := dial(t, dialWSURL(t, ts))
	defer c.Close()

	// Archive: ack + session vanishes from active list.
	require.NoError(t, c.WriteJSON(proto.NewArchiveSession(sid)))
	f := readFrame(t, c)
	assert.Equal(t, "archived", f.Action)
	active, _ := st.ListSessions(0)
	assert.Empty(t, active)
	archived, _ := st.ListArchivedSessions(0)
	require.Len(t, archived, 1)
	assert.Equal(t, sid, archived[0].ID)

	// Unarchive: ack + session returns to active list.
	require.NoError(t, c.WriteJSON(proto.NewUnarchiveSession(sid)))
	f = readFrame(t, c)
	assert.Equal(t, "unarchive", f.Action)
	active, _ = st.ListSessions(0)
	require.Len(t, active, 1)
	assert.Equal(t, sid, active[0].ID)
}

func TestChatWS_DeleteSession_RemovesMessages(t *testing.T) {
	st, s := newSessionTestServer(t)
	sid, err := st.CreateSession("doomed")
	require.NoError(t, err)
	require.NoError(t, st.AppendMessage(sid, 0, "user", "hi"))
	require.NoError(t, st.AppendMessage(sid, 1, "assistant", "yo"))

	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	c := dial(t, dialWSURL(t, ts))
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewDeleteSession(sid)))
	f := readFrame(t, c)
	assert.Equal(t, "deleted", f.Action)

	ss, err := st.GetSession(sid)
	require.NoError(t, err)
	assert.Nil(t, ss)
	msgs, err := st.Messages(sid)
	require.NoError(t, err)
	assert.Empty(t, msgs)
}

func TestChatWS_SessionListArchived(t *testing.T) {
	st, s := newSessionTestServer(t)
	activeID, _ := st.CreateSession("active")
	archivedID, _ := st.CreateSession("gone")
	require.NoError(t, st.SetSessionArchived(archivedID, true))

	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	c := dial(t, dialWSURL(t, ts))
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewSessionListArchived()))
	f := readFrame(t, c)
	require.Equal(t, "sessions", f.Type)
	require.Len(t, f.Sessions, 1, "only archived sessions returned")
	assert.Equal(t, archivedID, f.Sessions[0].ID)
	assert.NotEqual(t, activeID, f.Sessions[0].ID)

	// Active list still excludes the archived one.
	require.NoError(t, c.WriteJSON(proto.NewSessionList()))
	f = readFrame(t, c)
	require.Equal(t, "sessions", f.Type)
	require.Len(t, f.Sessions, 1)
	assert.Equal(t, activeID, f.Sessions[0].ID)
}

func TestChatWS_RenameSession_DisabledWhenNoStore(t *testing.T) {
	// No Store in Config -> recording disabled.
	o, _ := orchestrator.New(orchestrator.Config{Model: einollm.NewFakeModel([]string{"x"}, nil)})
	s := New(Config{Token: "t"})
	s.ChatWS(o, nil, nil)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	c := dial(t, dialWSURL(t, ts))
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewRenameSession("any", "title")))
	f := readFrame(t, c)
	assert.Equal(t, "error", f.Type)
}

// TestChatWS_ForkSession_AllMessages proves fork_session{seq:-1} creates a new
// session, copies all messages, and returns the new id via session_forked.
//
// GB1: a fresh WS connection has empty connSession.sessionID until a
// user_message / restore_session lands; handleForkSession would otherwise
// error out ("session recording is disabled"). Each fork test therefore sends
// restore_session first so the server-side connSession picks up the sid, then
// reads the session_restored reply, and only then sends fork_session.
func TestChatWS_ForkSession_AllMessages(t *testing.T) {
	st, s := newSessionTestServer(t)
	sid, err := st.CreateSession("orig")
	require.NoError(t, err)
	require.NoError(t, st.AppendMessage(sid, 0, "user", "hi"))
	require.NoError(t, st.AppendMessage(sid, 1, "assistant", "hello"))

	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	c := dial(t, dialWSURL(t, ts))
	defer c.Close()

	// GB1: restore_session first so cs.sessionID is set on the server side.
	require.NoError(t, c.WriteJSON(proto.NewRestoreSession(sid)))
	rf := readFrame(t, c)
	require.Equal(t, "session_restored", rf.Type)
	assert.Equal(t, sid, rf.SessionID)

	require.NoError(t, c.WriteJSON(proto.NewForkSession(-1)))
	f := readFrame(t, c)
	require.Equal(t, "session_forked", f.Type)
	forkID := f.SessionID
	assert.NotEmpty(t, forkID)
	assert.NotEqual(t, sid, forkID)

	// New session has 2 messages.
	msgs, err := st.Messages(forkID)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	assert.Equal(t, "hi", msgs[0].Content)

	// Original session unchanged.
	orig, _ := st.Messages(sid)
	assert.Len(t, orig, 2)

	// The fork ack also switches the SERVER connSession. A following turn must
	// append to forkID, not silently keep writing the source session.
	require.NoError(t, c.WriteJSON(proto.NewUserMessage("fork-only turn")))
	turnDone := false
	for i := 0; i < 100; i++ {
		turnFrame := readFrame(t, c)
		if turnFrame.Type == "done" || turnFrame.Type == "error" {
			turnDone = true
			break
		}
	}
	require.True(t, turnDone, "fork-only turn must reach done/error")
	forkAfter, err := st.Messages(forkID)
	require.NoError(t, err)
	assert.Greater(t, len(forkAfter), 2)
	origAfter, err := st.Messages(sid)
	require.NoError(t, err)
	assert.Len(t, origAfter, 2)
}

// TestChatWS_ForkSession_PartialBySeq proves seq>=0 truncates up to that seq
// inclusive.
func TestChatWS_ForkSession_PartialBySeq(t *testing.T) {
	st, s := newSessionTestServer(t)
	sid, _ := st.CreateSession("orig")
	st.AppendMessage(sid, 0, "user", "m0")
	st.AppendMessage(sid, 1, "assistant", "m1")
	st.AppendMessage(sid, 2, "user", "m2")

	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	c := dial(t, dialWSURL(t, ts))
	defer c.Close()

	// GB1: restore first.
	require.NoError(t, c.WriteJSON(proto.NewRestoreSession(sid)))
	rf := readFrame(t, c)
	require.Equal(t, "session_restored", rf.Type)

	require.NoError(t, c.WriteJSON(proto.NewForkSession(1)))
	f := readFrame(t, c)
	require.Equal(t, "session_forked", f.Type)
	msgs, _ := st.Messages(f.SessionID)
	require.Len(t, msgs, 2)
}

// TestChatWS_ForkSession_SeqOutOfBoundsRejected proves an out-of-range seq
// returns an error frame.
func TestChatWS_ForkSession_SeqOutOfBoundsRejected(t *testing.T) {
	st, s := newSessionTestServer(t)
	sid, _ := st.CreateSession("orig")
	st.AppendMessage(sid, 0, "user", "only")

	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	c := dial(t, dialWSURL(t, ts))
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewRestoreSession(sid)))
	rf := readFrame(t, c)
	require.Equal(t, "session_restored", rf.Type)

	require.NoError(t, c.WriteJSON(proto.NewForkSession(99)))
	f := readFrame(t, c)
	assert.Equal(t, "error", f.Type)
}

// TestChatWS_ForkSession_RejectsNegativeOtherThanMinusOne proves seq=-2 and
// smaller negatives are also rejected by the server (GB5 consistency).
func TestChatWS_ForkSession_RejectsNegativeOtherThanMinusOne(t *testing.T) {
	st, s := newSessionTestServer(t)
	sid, _ := st.CreateSession("orig")
	st.AppendMessage(sid, 0, "user", "only")

	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	c := dial(t, dialWSURL(t, ts))
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewRestoreSession(sid)))
	rf := readFrame(t, c)
	require.Equal(t, "session_restored", rf.Type)

	require.NoError(t, c.WriteJSON(proto.NewForkSession(-2)))
	f := readFrame(t, c)
	assert.Equal(t, "error", f.Type)
}

// seedRollbackSession creates a session with two user turns (seq 0 and 2,
// each followed by an assistant reply) — the fixture every
// resolveRollbackSeq/rollback-fork test below shares.
func seedRollbackSession(t *testing.T, st *store.Store) string {
	t.Helper()
	sid, err := st.CreateSession("orig")
	require.NoError(t, err)
	require.NoError(t, st.AppendMessage(sid, 0, "user", "first"))
	require.NoError(t, st.AppendMessage(sid, 1, "assistant", "reply1"))
	require.NoError(t, st.AppendMessage(sid, 2, "user", "second"))
	require.NoError(t, st.AppendMessage(sid, 3, "assistant", "reply2"))
	return sid
}

// TestResolveRollbackSeq_TargetsMostRecentUserTurn proves turnsBack=1 walks
// from the END of the transcript: it targets the LAST user message ("second",
// seq=2), not the first, resolving to seq-1=1 (fork ends right before it).
func TestResolveRollbackSeq_TargetsMostRecentUserTurn(t *testing.T) {
	st, _ := newSessionTestServer(t)
	sid := seedRollbackSession(t, st)

	seq, err := resolveRollbackSeq(st, sid, 1)
	require.NoError(t, err)
	assert.Equal(t, 1, seq)
}

// TestResolveRollbackSeq_FirstTurnYieldsEmptyForkSentinel proves the W-E-11
// edge case: rolling back to before the session's very first message (seq=0)
// cannot resolve to seq-1=-1 (store.ForkSession's "copy everything"
// sentinel) — it must resolve to -2, ForkSession's dedicated empty-fork
// sentinel, or a rollback to the first turn would silently duplicate the
// whole session instead of producing an empty one.
func TestResolveRollbackSeq_FirstTurnYieldsEmptyForkSentinel(t *testing.T) {
	st, _ := newSessionTestServer(t)
	sid := seedRollbackSession(t, st)

	seq, err := resolveRollbackSeq(st, sid, 2)
	require.NoError(t, err)
	assert.Equal(t, -2, seq)
}

// TestResolveRollbackSeq_ExceedsAvailableTurnsErrors proves the fail-closed
// stance (mirroring GB5's out-of-range rejection for plain seq forks):
// asking to roll back further than the session has user turns is an error,
// not a silent clamp to the empty-fork sentinel.
func TestResolveRollbackSeq_ExceedsAvailableTurnsErrors(t *testing.T) {
	st, _ := newSessionTestServer(t)
	sid := seedRollbackSession(t, st)

	_, err := resolveRollbackSeq(st, sid, 3)
	require.Error(t, err)
}

// TestChatWS_ForkSession_RollbackByTurnsBack proves the end-to-end wire path:
// fork_session{turns_back:1} reaches resolveRollbackSeq and lands on the same
// seq a manually-computed fork_session{seq:1} would.
func TestChatWS_ForkSession_RollbackByTurnsBack(t *testing.T) {
	st, s := newSessionTestServer(t)
	sid := seedRollbackSession(t, st)

	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	c := dial(t, dialWSURL(t, ts))
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewRestoreSession(sid)))
	rf := readFrame(t, c)
	require.Equal(t, "session_restored", rf.Type)

	require.NoError(t, c.WriteJSON(proto.NewForkSessionRollback(1)))
	f := readFrame(t, c)
	require.Equal(t, "session_forked", f.Type)
	msgs, err := st.Messages(f.SessionID)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	assert.Equal(t, "first", msgs[0].Content)
	assert.Equal(t, "reply1", msgs[1].Content)
}

// TestChatWS_ForkSession_RollbackBeforeFirstMessage proves the empty-fork
// edge case survives the full wire round trip: turns_back covering the
// session's very first turn produces a session_forked reply for a session
// with ZERO messages, not an error and not a full-history duplicate.
func TestChatWS_ForkSession_RollbackBeforeFirstMessage(t *testing.T) {
	st, s := newSessionTestServer(t)
	sid := seedRollbackSession(t, st)

	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	c := dial(t, dialWSURL(t, ts))
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewRestoreSession(sid)))
	rf := readFrame(t, c)
	require.Equal(t, "session_restored", rf.Type)

	require.NoError(t, c.WriteJSON(proto.NewForkSessionRollback(2)))
	f := readFrame(t, c)
	require.Equal(t, "session_forked", f.Type)
	msgs, err := st.Messages(f.SessionID)
	require.NoError(t, err)
	assert.Empty(t, msgs)
}

// TestChatWS_ForkSession_RollbackExceedingTurnsErrors mirrors
// TestChatWS_ForkSession_SeqOutOfBoundsRejected for the turns_back path.
func TestChatWS_ForkSession_RollbackExceedingTurnsErrors(t *testing.T) {
	st, s := newSessionTestServer(t)
	sid := seedRollbackSession(t, st)

	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	c := dial(t, dialWSURL(t, ts))
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewRestoreSession(sid)))
	rf := readFrame(t, c)
	require.Equal(t, "session_restored", rf.Type)

	require.NoError(t, c.WriteJSON(proto.NewForkSessionRollback(5)))
	f := readFrame(t, c)
	assert.Equal(t, "error", f.Type)
}

// TestChatWS_ForkSession_DisabledWhenNoStore proves store=nil returns an error.
// No session here, so no restore_session precondition (GB1 does not apply).
func TestChatWS_ForkSession_DisabledWhenNoStore(t *testing.T) {
	o, _ := orchestrator.New(orchestrator.Config{Model: einollm.NewFakeModel([]string{"x"}, nil)})
	srv := New(Config{Token: "t"})
	srv.ChatWS(o, nil, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	c := dial(t, dialWSURL(t, ts))
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewForkSession(-1)))
	f := readFrame(t, c)
	assert.Equal(t, "error", f.Type)
}

// TestChatWS_SideConversation_DoesNotWriteDB proves that during a side
// conversation:
//   - after enter_side, user_message does NOT create a new session
//   - user_message does NOT append messages to any session
//   - after exit_side, user_message resumes normal recording on the original
//     session
//
// This is V11's core promise (review #2): side never writes DB.
func TestChatWS_SideConversation_DoesNotWriteDB(t *testing.T) {
	st, s := newSessionTestServer(t)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	c := dial(t, dialWSURL(t, ts))
	defer c.Close()

	// 1) Main user_message — creates a session.
	require.NoError(t, c.WriteJSON(proto.NewUserMessage("main turn")))
	drainUntilDone(t, c)

	active, _ := st.ListSessions(0)
	require.Len(t, active, 1, "main turn should create exactly 1 session")
	mainSid := active[0].ID
	mainMsgs, _ := st.Messages(mainSid)
	require.True(t, len(mainMsgs) >= 1, "main turn should persist at least 1 message")

	// 2) enter_side.
	require.NoError(t, c.WriteJSON(proto.NewEnterSide()))
	fr := readFrame(t, c)
	require.Equal(t, "side_state", fr.Type)
	assert.Equal(t, 1, fr.SideDepth, "after enter_side depth=1")

	// 3) user_message inside the side — should NOT create a session nor append
	// messages.
	require.NoError(t, c.WriteJSON(proto.NewUserMessage("side turn")))
	drainUntilDone(t, c)

	activeAfter, _ := st.ListSessions(0)
	assert.Len(t, activeAfter, 1, "side turn must not create a new session")
	mainMsgsAfter, _ := st.Messages(mainSid)
	assert.Equal(t, len(mainMsgs), len(mainMsgsAfter), "side turn must not append messages to main thread")

	// 4) exit_side.
	require.NoError(t, c.WriteJSON(proto.NewExitSide()))
	fr = readFrame(t, c)
	require.Equal(t, "side_state", fr.Type)
	assert.Equal(t, 0, fr.SideDepth, "after exit_side depth=0")

	// 5) Main user_message resumes recording.
	require.NoError(t, c.WriteJSON(proto.NewUserMessage("main turn 2")))
	drainUntilDone(t, c)
	mainMsgsFinal, _ := st.Messages(mainSid)
	assert.True(t, len(mainMsgsFinal) > len(mainMsgsAfter), "after exit_side main thread must keep appending messages")
}

// drainUntilDone reads frames from c until a "done" frame arrives. Used by the
// side-conversation test to advance past turn frames.
func drainUntilDone(t *testing.T, c *websocket.Conn) {
	t.Helper()
	for i := 0; i < 100; i++ { // safety bound
		f := readFrame(t, c)
		if f.Type == "done" || f.Type == "error" {
			return
		}
	}
	t.Fatal("drainUntilDone: exhausted bound without done/error")
}

// TestChatWS_RenameSession_EmptyTitle proves rename with an empty title
// returns an error frame.
func TestChatWS_RenameSession_EmptyTitle(t *testing.T) {
	st, s := newSessionTestServer(t)
	sid, err := st.CreateSession("test")
	require.NoError(t, err)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	c := dial(t, dialWSURL(t, ts))
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewRenameSession(sid, "  ")))
	f := readFrame(t, c)
	assert.Equal(t, "error", f.Type)
	assert.Contains(t, f.Text, "non-empty")
}

// TestChatWS_RenameSession_LongTitle proves rename with a very long title
// truncates it to 200 runes.
func TestChatWS_RenameSession_LongTitle(t *testing.T) {
	st, s := newSessionTestServer(t)
	sid, err := st.CreateSession("test")
	require.NoError(t, err)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	c := dial(t, dialWSURL(t, ts))
	defer c.Close()

	long := strings.Repeat("x", 300)
	require.NoError(t, c.WriteJSON(proto.NewRenameSession(sid, long)))
	f := readFrame(t, c)
	assert.Equal(t, "session_ack", f.Type)
	assert.Equal(t, "renamed", f.Action)
	assert.Len(t, []rune(f.Text), 200, "title must be truncated to 200 runes")
}

// TestChatWS_DeleteSession_DeletesCurrentSession proves deleting the current
// session resets connSession state.
func TestChatWS_DeleteSession_DeletesCurrentSession(t *testing.T) {
	st, s := newSessionTestServer(t)
	sid, err := st.CreateSession("doomed")
	require.NoError(t, err)
	require.NoError(t, st.AppendMessage(sid, 0, "user", "hi"))

	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	c := dial(t, dialWSURL(t, ts))
	defer c.Close()

	// Restore the session first so cs.sessionID is set.
	require.NoError(t, c.WriteJSON(proto.NewRestoreSession(sid)))
	rf := readFrame(t, c)
	require.Equal(t, "session_restored", rf.Type)

	// Now delete it.
	require.NoError(t, c.WriteJSON(proto.NewDeleteSession(sid)))
	f := readFrame(t, c)
	assert.Equal(t, "deleted", f.Action)

	// Verify it's gone.
	ss, err := st.GetSession(sid)
	require.NoError(t, err)
	assert.Nil(t, ss)
}

// TestChatWS_SessionHandler_StoreNil tests store-nil error paths for session
// management handlers. Each handler should return an error frame when store
// is unavailable.
func TestChatWS_SessionHandler_StoreNil(t *testing.T) {
	o, err := orchestrator.New(orchestrator.Config{Model: einollm.NewFakeModel([]string{"x"}, nil)})
	require.NoError(t, err)
	srv := New(Config{Token: "t"})
	srv.ChatWS(o, nil, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	t.Run("session_list returns empty", func(t *testing.T) {
		c := dial(t, dialWSURL(t, ts))
		defer c.Close()
		require.NoError(t, c.WriteJSON(proto.NewSessionList()))
		f := readFrame(t, c)
		assert.Equal(t, "sessions", f.Type)
		assert.Empty(t, f.Sessions)
	})

	t.Run("archive errors", func(t *testing.T) {
		c := dial(t, dialWSURL(t, ts))
		defer c.Close()
		require.NoError(t, c.WriteJSON(proto.NewArchiveSession("any")))
		f := readFrame(t, c)
		assert.Equal(t, "error", f.Type)
	})

	t.Run("unarchive errors", func(t *testing.T) {
		c := dial(t, dialWSURL(t, ts))
		defer c.Close()
		require.NoError(t, c.WriteJSON(proto.NewUnarchiveSession("any")))
		f := readFrame(t, c)
		assert.Equal(t, "error", f.Type)
	})

	t.Run("delete errors", func(t *testing.T) {
		c := dial(t, dialWSURL(t, ts))
		defer c.Close()
		require.NoError(t, c.WriteJSON(proto.NewDeleteSession("any")))
		f := readFrame(t, c)
		assert.Equal(t, "error", f.Type)
	})

	t.Run("archived list returns empty", func(t *testing.T) {
		c := dial(t, dialWSURL(t, ts))
		defer c.Close()
		require.NoError(t, c.WriteJSON(proto.NewSessionListArchived()))
		f := readFrame(t, c)
		assert.Equal(t, "sessions", f.Type)
		assert.Empty(t, f.Sessions)
	})
}

// TestChatWS_ArchiveSession_EmptyID proves archive with an empty session id
// returns an error frame.
func TestChatWS_ArchiveSession_EmptyID(t *testing.T) {
	_, s := newSessionTestServer(t)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	c := dial(t, dialWSURL(t, ts))
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewArchiveSession("")))
	f := readFrame(t, c)
	assert.Equal(t, "error", f.Type)
}

// TestChatWS_UnarchiveSession_EmptyID proves unarchive with an empty session
// id returns an error frame.
func TestChatWS_UnarchiveSession_EmptyID(t *testing.T) {
	_, s := newSessionTestServer(t)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	c := dial(t, dialWSURL(t, ts))
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewUnarchiveSession("")))
	f := readFrame(t, c)
	assert.Equal(t, "error", f.Type)
}

// TestChatWS_DeleteSession_EmptyID proves delete with an empty session id
// returns an error frame.
func TestChatWS_DeleteSession_EmptyID(t *testing.T) {
	_, s := newSessionTestServer(t)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	c := dial(t, dialWSURL(t, ts))
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewDeleteSession("")))
	f := readFrame(t, c)
	assert.Equal(t, "error", f.Type)
}

// TestChatWS_RenameSession_EmptyID proves rename with an empty session id
// returns an error frame.
func TestChatWS_RenameSession_EmptyID(t *testing.T) {
	o, err := orchestrator.New(orchestrator.Config{Model: einollm.NewFakeModel([]string{"x"}, nil)})
	require.NoError(t, err)
	srv := New(Config{Token: "t"})
	srv.ChatWS(o, nil, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	c := dial(t, dialWSURL(t, ts))
	defer c.Close()

	// No store so handleRenameSession checks store nil path.
	require.NoError(t, c.WriteJSON(proto.NewRenameSession("", "title")))
	f := readFrame(t, c)
	assert.Equal(t, "error", f.Type)
}

// TestChatWS_SessionListIsBoundedAndSaysSo is the production consumer W-D-10's
// keyset pager did not have.
//
// store.ListSessionsPage was exported, tested and called by nothing: gutting it
// to a panic left `go build ./...` at exit 0. The read it replaces was
// store.ListSessions(0) — every active session, no bound — and sessionInfos
// then spends one SessionMessageCount query PER ROW, so opening /sessions on a
// long-lived project was an unbounded fan-out behind an unbounded frame.
//
// Both halves are asserted, because the bound alone would be the worse bug: a
// prefix presented as the whole list is how a session that is perfectly safe on
// disk gets reported as lost.
func TestChatWS_SessionListIsBoundedAndSaysSo(t *testing.T) {
	st, s := newSessionTestServer(t)
	const extra = 5
	for i := range store.MaxMessagePageSize + extra {
		_, err := st.CreateSession("session " + strconv.Itoa(i))
		require.NoError(t, err)
	}

	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	c := dial(t, dialWSURL(t, ts))
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewSessionList()))
	f := readFrame(t, c)
	require.Equal(t, "sessions", f.Type)
	require.Len(t, f.Sessions, store.MaxMessagePageSize,
		"the reply must be one clamped page, not every row in the table")
	require.Contains(t, f.Text, "not listed",
		"a truncated list that does not say so reads as a lost session")

	// Under the bound the reply is exactly what it always was: the whole list,
	// and no note. A warning printed every time stops being read.
	st2, s2 := newSessionTestServer(t)
	only, err := st2.CreateSession("the only one")
	require.NoError(t, err)
	ts2 := httptest.NewServer(s2.Handler())
	defer ts2.Close()
	c2 := dial(t, dialWSURL(t, ts2))
	defer c2.Close()
	require.NoError(t, c2.WriteJSON(proto.NewSessionList()))
	f = readFrame(t, c2)
	require.Equal(t, "sessions", f.Type)
	require.Len(t, f.Sessions, 1)
	assert.Equal(t, only, f.Sessions[0].ID)
	assert.Empty(t, f.Text, "nothing was left out, so there is nothing to say")
}
