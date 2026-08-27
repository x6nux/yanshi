package cli

// liverun_c12_test.go — does a stored memory actually come back on the next
// turn, in a running system?
//
// The unit tests for internal/tools.AutoRecall establish that, given a store
// and a question, it returns the right block. That is a statement about a
// function. The capability C12 claims is different and strictly larger: that a
// memory written in one turn is present in the prompt of a later one WITHOUT
// the model asking. The gap between those two is a call site, and a call site
// that does not exist produces exactly the same green unit test.
//
// So this drives the assembled machine — real store, real orchestrator, real
// WebSocket turns — with a FakeModel in Echo mode, which returns the
// concatenation of everything it was sent. The echo is the prompt: if the
// recalled block is not in it, the memory did not reach the model, whatever the
// retrieval function would have returned if somebody had called it.

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/require"
	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	apihttp "github.com/x6nux/yanshi/internal/api/http"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/store"
	"github.com/x6nux/yanshi/internal/tools"
)

// echoTurn runs one turn against a server whose model echoes its input, and
// returns everything the model saw.
type echoRig struct {
	store *store.Store
	send  func(t *testing.T, text string) string
}

// newEchoRig assembles a store, an orchestrator over an echoing FakeModel, and
// a real WebSocket server in front of them.
func newEchoRig(t *testing.T) *echoRig {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/mem.db")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	fake := einollm.NewFakeModelWithMessages([]*schema.Message{{Role: schema.Assistant, Content: "ok"}}, nil)
	fake.Echo = true

	orch, err := orchestrator.New(orchestrator.Config{Model: fake, WorkRoot: t.TempDir()})
	require.NoError(t, err)

	// Config.Store is what makes the server record — and, with the C12 call
	// site, what makes it recall. A rig that omits it reproduces neither.
	srv := apihttp.New(apihttp.Config{Store: st})
	srv.ChatWS(orch, nil, nil)
	srv.Sessions(st)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	ctx := context.Background()
	be, err := newWSBackend(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/v1/chat/ws")
	require.NoError(t, err)
	t.Cleanup(func() { _ = be.Close() })

	return &echoRig{
		store: st,
		send: func(t *testing.T, text string) string {
			t.Helper()
			ch, err := be.Send(ctx, text)
			require.NoError(t, err)
			var sb strings.Builder
			for ev := range ch {
				if ev.Kind == "error" && ev.Err != nil {
					t.Fatalf("turn %q failed: %v", text, ev.Err)
				}
				sb.WriteString(ev.Text)
			}
			return sb.String()
		},
	}
}

// TestLiveRun_C12StoredMemoryReachesTheModelWithoutBeingAsked writes a memory
// the way memory_write does, then asks a question about it in a later turn and
// inspects what the model received.
//
// The failing direction is what makes this worth running: if nothing in the
// live request path calls the auto-recall, the echo contains the question and
// not the memory, and the test says so — which no test of AutoRecall itself
// can do.
func TestLiveRun_C12StoredMemoryReachesTheModelWithoutBeingAsked(t *testing.T) {
	rig := newEchoRig(t)

	const stored = "The deployment runbook lives in ops/deploy-runbook.md and requires the ACME vpn."
	id, err := rig.store.WriteMemory("note", stored)
	require.NoError(t, err)
	t.Logf("stored memory %s: %q", id, stored)

	// Sanity: the retrieval itself works against this store, so a failure below
	// is about the wiring and not about the query.
	block := tools.AutoRecall(context.Background(), rig.store,
		"where is the deployment runbook", store.MemoryFilter{})
	t.Logf("AutoRecall() called directly returns %d chars", len(block))
	require.NotEmpty(t, block,
		"the retrieval cannot find its own memory; fix that before asking about wiring")

	echo := rig.send(t, "where is the deployment runbook")
	t.Logf("model received (%d chars): %.400s", len(echo), echo)

	if !strings.Contains(echo, "deploy-runbook.md") {
		t.Errorf("the stored memory never reached the model.\n"+
			"AutoRecall returns it when called directly, so the retrieval works — "+
			"what is missing is a caller on the live turn path. A memory that is only "+
			"retrievable by a model that already knows to ask is the exact situation "+
			"C12 exists to fix.\nmodel input was: %.600s", echo)
	}
}

// TestLiveRun_C12IrrelevantMemoriesAreNotInjected is the other half, and the
// one that keeps the fix honest. Injecting every memory on every turn would
// pass the test above while making the feature a per-turn tax that trains the
// model to skim the injected block — which disarms the turns where the recall
// was right.
func TestLiveRun_C12IrrelevantMemoriesAreNotInjected(t *testing.T) {
	rig := newEchoRig(t)

	const unrelated = "The user prefers tabs over spaces in Makefiles."
	_, err := rig.store.WriteMemory("preference", unrelated)
	require.NoError(t, err)
	const alsoUnrelated = "Postgres credentials rotate on the first of the month."
	_, err = rig.store.WriteMemory("note", alsoUnrelated)
	require.NoError(t, err)

	echo := rig.send(t, "what is the airspeed velocity of an unladen swallow")
	t.Logf("model received (%d chars): %.400s", len(echo), echo)

	for _, memory := range []string{"tabs over spaces", "Postgres credentials"} {
		if strings.Contains(echo, memory) {
			t.Errorf("an unrelated memory (%q) was injected into a turn it has nothing to do with; "+
				"a block that appears every turn is a block the model learns to ignore", memory)
		}
	}
}
