package http

import (
	"context"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	"github.com/x6nux/yanshi/internal/features"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/store"
)

// newDistillTestRegistry builds a features.Registry with every DefaultSpecs
// entry registered (mirroring bootstrap's registration loop), so tests can
// flip memory_distill_after_turn without duplicating bootstrap's spec list.
func newDistillTestRegistry(t *testing.T) *features.Registry {
	t.Helper()
	reg := features.NewRegistry(false)
	for _, spec := range features.DefaultSpecs() {
		reg.Register(spec)
	}
	return reg
}

// TestDistillFrameRoundTrips proves ledger clause 2: a distill_memories
// frame is processed BY THE SERVER over a real WebSocket connection and
// answered with a memories_distilled reply carrying the actual pass counts.
// The brief's own example only constructed the proto frames directly and
// never dialed a connection, which would pass even if the dispatch case or
// handleDistillMemories were entirely missing -- see task-5-report.txt.
//
// ledger: A2/W-A-05#2 蒸馏请求帧被服务端处理并回复结果帧
func TestDistillFrameRoundTrips(t *testing.T) {
	st, err := store.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })

	for i := 0; i < 6; i++ {
		_, err := st.WriteMemoryScoped("note", "candidate memory", store.MemoryFilter{})
		require.NoError(t, err)
	}

	distillModel := einollm.NewFakeModel([]string{"NOTHING"}, nil)
	o, err := orchestrator.New(orchestrator.Config{Model: einollm.NewFakeModel([]string{"hi"}, nil)})
	require.NoError(t, err)
	s := New(Config{Token: "t", Store: st, DistillModel: distillModel})
	s.ChatWS(o, nil, nil)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	c := dial(t, dialWSURL(t, ts))
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewDistillMemories()))
	f := readFrame(t, c)
	require.Equal(t, "memories_distilled", f.Type)
	assert.Contains(t, f.Text, "considered 6")
	assert.Contains(t, f.Text, "merged 0")
}

// TestDistillMemories_DisabledWithoutModel proves distill_memories replies
// with an error frame (not a panic, not a hang) when no DistillModel is
// configured -- the state every deployment is in until bootstrap wires one.
func TestDistillMemories_DisabledWithoutModel(t *testing.T) {
	st, err := store.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })

	o, err := orchestrator.New(orchestrator.Config{Model: einollm.NewFakeModel([]string{"hi"}, nil)})
	require.NoError(t, err)
	s := New(Config{Token: "t", Store: st})
	s.ChatWS(o, nil, nil)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	c := dial(t, dialWSURL(t, ts))
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewDistillMemories()))
	f := readFrame(t, c)
	assert.Equal(t, "error", f.Type)
}

// TestDistillFailureDoesNotAbortTurn proves ledger clause 3 (A2/W-A-05#3):
// with the post-turn pass enabled and a distillation model that always
// errors, an ordinary user_message turn still reaches "done".
//
// The 6 candidate memories are written scoped to a session created and
// restored BEFORE the turn (GB1 pattern, see ws_session_test.go), not to an
// empty MemoryFilter{}. A plain user_message on a fresh connection makes
// ensureSession auto-create a session with a server-generated ID; writing
// the candidates unscoped would silently leave DistillCandidates matching
// zero rows against that ID, which drops the row count below
// tools.MinDistillBatch and short-circuits DistillMemories BEFORE it ever
// calls the model -- proving nothing about the failure path this test
// exists to cover. restore_session first pins cs.sessionID to the same ID
// the memories were written under, so the post-turn pass actually reaches
// the failing model.Generate call.
//
// ledger: A2/W-A-05#3 蒸馏失败不影响所在 turn 的正常结束
func TestDistillFailureDoesNotAbortTurn(t *testing.T) {
	st, err := store.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })

	sid, err := st.CreateSession("distill-turn")
	require.NoError(t, err)
	for i := 0; i < 6; i++ {
		_, err := st.WriteMemoryScoped("note", "candidate memory", store.MemoryFilter{SessionID: sid})
		require.NoError(t, err)
	}

	reg := newDistillTestRegistry(t)
	require.NoError(t, reg.Set("memory_distill_after_turn", true))

	failingDistillModel := einollm.NewFakeModel(nil, fmt.Errorf("distill model unavailable"))
	o, err := orchestrator.New(orchestrator.Config{Model: einollm.NewFakeModel([]string{"turn reply"}, nil)})
	require.NoError(t, err)
	s := New(Config{Token: "t", Store: st, DistillModel: failingDistillModel, FeaturesReg: reg})
	s.ChatWS(o, nil, nil)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	c := dial(t, dialWSURL(t, ts))
	defer c.Close()

	// GB1: restore_session first so cs.sessionID == sid on the server side,
	// matching the scope the 6 candidates above were written under.
	require.NoError(t, c.WriteJSON(proto.NewRestoreSession(sid)))
	rf := readFrame(t, c)
	require.Equal(t, "session_restored", rf.Type)

	require.NoError(t, c.WriteJSON(proto.NewUserMessage("hello")))
	turnDone := false
	for i := 0; i < 100; i++ {
		f := readFrame(t, c)
		if f.Type == "done" {
			turnDone = true
			break
		}
		require.NotEqual(t, "error", f.Type, "turn must not surface the background distillation failure as a turn error")
	}
	require.True(t, turnDone, "turn must reach done despite a failing post-turn distillation pass")
}

// TestRunDistillPass_SwallowsModelError proves the mechanism behind clause 3
// directly: runDistillPass never returns the underlying model error, and a
// failed pass leaves the candidate memories exactly as they were (none
// marked superseded).
func TestRunDistillPass_SwallowsModelError(t *testing.T) {
	st, err := store.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })
	ids := make([]string, 0, 6)
	for i := 0; i < 6; i++ {
		id, err := st.WriteMemoryScoped("note", "candidate memory", store.MemoryFilter{})
		require.NoError(t, err)
		ids = append(ids, id)
	}

	failingModel := einollm.NewFakeModel(nil, fmt.Errorf("boom"))
	res, err := runDistillPass(context.Background(), st, failingModel, store.MemoryFilter{})
	require.NoError(t, err, "runDistillPass must swallow the model error, not propagate it")
	assert.Zero(t, res, "a failed pass reports a zero result, not a partial one")

	for _, id := range ids {
		m, err := st.MemoryByID(id)
		require.NoError(t, err)
		assert.Empty(t, m.SupersededBy)
	}
}
