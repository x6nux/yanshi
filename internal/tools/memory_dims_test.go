package tools

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/approval"
	"github.com/x6nux/yanshi/internal/store"
	"github.com/x6nux/yanshi/internal/vcs"
)

func newMemoryStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "m.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// dimCtx binds the two context values memoryDims reads, using the same
// injectors production uses. A VCSScope with a nil VCS still carries the acting
// agent, which is the field memoryDims wants.
func dimCtx(sessionID, agentID string) context.Context {
	ctx := context.Background()
	if sessionID != "" {
		ctx = WithApprovalManager(ctx, &approval.Manager{}, sessionID)
	}
	if agentID != "" {
		ctx = WithVCS(ctx, VCSScope{Agent: agentID})
	}
	return ctx
}

func callMemTool(t *testing.T, ctx context.Context,
	fn func(context.Context, string) (string, error), args any) (string, error) {
	t.Helper()
	b, err := json.Marshal(args)
	require.NoError(t, err)
	return fn(ctx, string(b))
}

// ---------------------------------------------------------------------------
// C14: writes capture the dimensions
// ---------------------------------------------------------------------------

// TestMemoryWrite_TagsDimensionsFromContext is the write-side half of C14. If
// the dimensions are not captured here they can never be filtered on later,
// however good the query side becomes — which was the pre-C14 state.
func TestMemoryWrite_TagsDimensionsFromContext(t *testing.T) {
	s := newMemoryStore(t)
	mt := NewMemoryTools(s)

	_, err := callMemTool(t, dimCtx("sess-42", "reviewer"), mt.runWrite,
		map[string]any{"content": "the branch is feat/x", "kind": "fact"})
	require.NoError(t, err)

	got, err := s.RecallMemory(10)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "sess-42", got[0].SessionID)
	assert.Equal(t, "reviewer", got[0].AgentID)
	assert.Equal(t, "fact", got[0].Kind)
}

// TestMemoryWrite_RecordsWhatItKnows: a context with only one dimension records
// only that one. Fabricating the other would make the trail lie; recording the
// empty string keeps the row visible to an unscoped query, which is the honest
// reading of "we do not know".
func TestMemoryWrite_RecordsWhatItKnows(t *testing.T) {
	cases := []struct {
		name              string
		session, agent    string
		wantSess, wantAgt string
	}{
		{"both", "s", "a", "s", "a"},
		{"session only", "s", "", "s", ""},
		{"agent only", "", "a", "", "a"},
		{"neither", "", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newMemoryStore(t)
			mt := NewMemoryTools(s)
			_, err := callMemTool(t, dimCtx(tc.session, tc.agent), mt.runWrite,
				map[string]any{"content": "x"})
			require.NoError(t, err)
			got, err := s.RecallMemory(10)
			require.NoError(t, err)
			require.Len(t, got, 1)
			assert.Equal(t, tc.wantSess, got[0].SessionID)
			assert.Equal(t, tc.wantAgt, got[0].AgentID)
		})
	}
}

// TestMemoryDims_ThreadLinkFallback: with no approval manager the WS turn still
// binds the session id as the thread id. Without the fallback every memory
// written on an approvals-less deployment would be dimensionless.
func TestMemoryDims_ThreadLinkFallback(t *testing.T) {
	ctx := WithThreadLink(context.Background(), "thread-session", "turn")
	assert.Equal(t, "thread-session", memoryDims(ctx).SessionID)
}

// TestMemoryDims_AgentComesFromVCSScope pins where the acting agent is read
// from: the VCS scope already carries it for commit attribution, so C14 reuses
// that rather than adding a second, drift-prone source of the same fact.
func TestMemoryDims_AgentComesFromVCSScope(t *testing.T) {
	ctx := WithVCS(context.Background(), VCSScope{Agent: "worker-3", VCS: (*vcs.VCS)(nil)})
	assert.Equal(t, "worker-3", memoryDims(ctx).AgentID)
}

// ---------------------------------------------------------------------------
// C14: scoped retrieval
// ---------------------------------------------------------------------------

// seedScoped writes one memory per dimension combination through the STORE, so
// the query-side tests do not depend on the write-side path being correct.
func seedScoped(t *testing.T, s *store.Store) {
	t.Helper()
	for _, r := range []struct {
		content string
		dims    store.MemoryFilter
	}{
		{"alpha mine", store.MemoryFilter{SessionID: "s1", AgentID: "a1"}},
		{"alpha other agent", store.MemoryFilter{SessionID: "s1", AgentID: "a2"}},
		{"alpha other session", store.MemoryFilter{SessionID: "s2", AgentID: "a1"}},
		{"alpha legacy", store.MemoryFilter{}},
	} {
		_, err := s.WriteMemoryScoped("note", r.content, r.dims)
		require.NoError(t, err)
	}
}

func TestMemoryScopeArgument(t *testing.T) {
	cases := []struct {
		name     string
		scope    any // omitted when nil
		wantErr  bool
		wantHits int
	}{
		// Omitting scope must behave exactly as before C14: cross-dimension.
		// Anything else silently hides memories that used to be findable.
		{"omitted defaults to all", nil, false, 4},
		{"explicit all", "all", false, 4},
		{"session", "session", false, 2},
		{"agent", "agent", false, 2},
		{"case and whitespace tolerated", "  SESSION ", false, 2},
		// An unknown scope is an ERROR, not a silent widening: a model asking for
		// one conversation and quietly getting all of them has been answered
		// wrongly with no way to tell.
		{"unknown scope is refused", "this-session", true, 0},
	}
	for _, tc := range cases {
		for _, tool := range []string{"search", "recall"} {
			t.Run(tc.name+"/"+tool, func(t *testing.T) {
				s := newMemoryStore(t)
				seedScoped(t, s)
				mt := NewMemoryTools(s)
				ctx := dimCtx("s1", "a1")

				args := map[string]any{"query": "alpha"}
				if tc.scope != nil {
					args["scope"] = tc.scope
				}
				fn := mt.runSearch
				if tool == "recall" {
					fn = mt.runRecall
				}
				out, err := callMemTool(t, ctx, fn, args)
				if tc.wantErr {
					require.Error(t, err)
					return
				}
				require.NoError(t, err)
				// Every seeded memory starts with "alpha "; count the renders.
				assert.Equal(t, tc.wantHits, countOccurrences(out, "alpha"))
			})
		}
	}
}

// TestMemoryScope_UnavailableDimensionIsAnError: asking to restrict to "my
// session" when there is no session must fail rather than fall back to an empty
// SessionID — which would match the rows written with an empty dimension by
// callers that had no session, i.e. the exact opposite of "restrict to mine".
func TestMemoryScope_UnavailableDimensionIsAnError(t *testing.T) {
	s := newMemoryStore(t)
	seedScoped(t, s)
	mt := NewMemoryTools(s)

	_, err := callMemTool(t, context.Background(), mt.runRecall, map[string]any{"scope": "session"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no recorded conversation")

	_, err = callMemTool(t, context.Background(), mt.runRecall, map[string]any{"scope": "agent"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no acting agent")

	// And the unscoped default still works in the same context.
	out, err := callMemTool(t, context.Background(), mt.runRecall, map[string]any{})
	require.NoError(t, err)
	assert.Equal(t, 4, countOccurrences(out, "alpha"))
}

func TestMemoryFilterFor(t *testing.T) {
	ctx := dimCtx("sess", "agent")
	cases := []struct {
		scope   string
		want    store.MemoryFilter
		wantErr bool
	}{
		{"", store.MemoryFilter{}, false},
		{MemoryScopeAll, store.MemoryFilter{}, false},
		{MemoryScopeSession, store.MemoryFilter{SessionID: "sess"}, false},
		{MemoryScopeAgent, store.MemoryFilter{AgentID: "agent"}, false},
		{"bogus", store.MemoryFilter{}, true},
	}
	for _, tc := range cases {
		t.Run("scope="+tc.scope, func(t *testing.T) {
			got, err := memoryFilterFor(ctx, tc.scope)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func countOccurrences(haystack, needle string) int {
	return strings.Count(haystack, needle)
}

// TestMemoryWrite_RecordsProvenance is W-D-07 asserted at the memory_write call
// site, for the reason its upkeep twin gives: the store API's own tests stay
// green when a caller quietly drops back to the un-provenanced writer.
//
// The session gets a real log first. A memory whose source resolves to zero
// messages would satisfy "an origin was recorded" while answering nothing.
func TestMemoryWrite_RecordsProvenance(t *testing.T) {
	s := newMemoryStore(t)
	sid, err := s.CreateSession("live")
	require.NoError(t, err)
	for i := 0; i < 3; i++ {
		require.NoError(t, s.AppendMessage(sid, i, "user", "turn "+strings.Repeat("x", i+1)))
	}
	mt := NewMemoryTools(s)

	_, err = callMemTool(t, dimCtx(sid, "reviewer"), mt.runWrite,
		map[string]any{"content": "the branch is feat/x", "kind": "fact"})
	require.NoError(t, err)

	got, err := s.RecallMemory(10)
	require.NoError(t, err)
	require.Len(t, got, 1)
	src, err := s.MemorySource(got[0].ID)
	require.NoError(t, err, "memory_write recorded no source log position")
	require.Len(t, src, 3, "the recorded position must resolve to this session's log")
	assert.Equal(t, sid, src[0].SessionID)
}

// TestMemoryWrite_NoSessionRecordsNoProvenance is the other half: the SSE path
// and bare sub-agents have no session, and inventing one would be worse than
// recording none. ErrNoMemorySource is the honest answer, not an empty slice
// that reads like "the source is gone".
func TestMemoryWrite_NoSessionRecordsNoProvenance(t *testing.T) {
	s := newMemoryStore(t)
	mt := NewMemoryTools(s)

	_, err := callMemTool(t, dimCtx("", "reviewer"), mt.runWrite,
		map[string]any{"content": "no session here"})
	require.NoError(t, err)

	got, err := s.RecallMemory(10)
	require.NoError(t, err)
	require.Len(t, got, 1)
	_, err = s.MemorySource(got[0].ID)
	assert.ErrorIs(t, err, store.ErrNoMemorySource)
}
