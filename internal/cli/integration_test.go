package cli

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	apihttp "github.com/x6nux/yanshi/internal/api/http"
	"github.com/x6nux/yanshi/internal/guard"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/tools"
)

// TestIntegration_InProcessWSTwoTurns boots an in-process backend (fake model,
// isolated temp root + sqlite config), resolves the transport, and drives two
// turns over it. It proves the full composition for the bare `yanshi` path:
// a fresh root with no lockfile makes the session bootstrap and own a backend,
// the resolved transport is WebSocket (the primary; SSE is only a fallback), and
// two turns over the one persistent socket each complete with a done event.
//
// This drives the same Resolve + Backend().Send path as runHeadless (kept inline
// so the test can also assert Mode()=="ws" and IsOwner() on one bootstrap, rather
// than double-bootstrapping to recover the mode). The fake model that
// bootstrap.Build installs does not echo, so multi-turn memory — the second turn
// seeing the first — is asserted separately by TestChatWS_MultiTurnMemory and
// TestSSEBackend_MultiTurn; this test pins the in-process resolver + wsBackend
// boot path and two-turn streaming.
func TestIntegration_InProcessWSTwoTurns(t *testing.T) {
	root := t.TempDir()
	opts := Options{ConfigPath: writeTestConfig(t, root), FakeModel: true, Root: root}

	ctx := context.Background()
	sess := newSession(opts.Root, opts.ConfigPath, opts.FakeModel)
	sess.setForced(opts.Server, opts.InProcess)
	require.NoError(t, sess.Resolve(ctx))
	defer sess.Close()

	assert.True(t, sess.IsOwner(), "fresh root -> session bootstraps and owns the backend")
	require.NotNil(t, sess.Backend(), "Resolve must install a backend")
	assert.Equal(t, "ws", sess.Backend().Mode(), "in-process backend must resolve to WebSocket")

	dones := 0
	for _, q := range []string{"remember ACME", "what did i say"} {
		ch, err := sess.Backend().Send(ctx, q)
		require.NoError(t, err)
		var sawEvents bool
		for ev := range ch {
			sawEvents = true
			if ev.Kind == "done" {
				dones++
			}
			if ev.Kind == "error" && ev.Err != nil {
				t.Fatalf("turn for %q returned error: %v", q, ev.Err)
			}
		}
		assert.True(t, sawEvents, "turn for %q must stream at least one event", q)
	}
	assert.Equal(t, 2, dones, "each of two turns must end with exactly one done event")
}

// assistantWithUsageMsg builds an assistant message carrying token usage, so a
// FakeModel can exercise ClassifyEventsWithUsage through the real ADK runner
// (usage survives the pipeline — see TestChatWS_GetStatus_AccumulatesUsage in
// package http). Local copy of the http-package helper so this test stays in
// package cli.
func assistantWithUsageMsg(text string, prompt, completion int) *schema.Message {
	return &schema.Message{
		Role:    schema.Assistant,
		Content: text,
		ResponseMeta: &schema.ResponseMeta{
			Usage: &schema.TokenUsage{
				PromptTokens:     prompt,
				CompletionTokens: completion,
				TotalTokens:      prompt + completion,
			},
		},
	}
}

// newParityServer boots an in-process WS backend exercising the feature-parity
// features: two distinguishable fake models in the registry (alpha the default,
// beta the switch target), a tool that triggers a permission prompt (fs_write
// allowed by name but granted no write paths, so it static-denies and escalates
// to the interactive callback), and a default model scripted to both accrue
// usage and call that tool. It returns the ws URL the client dials and the
// fs_write workdir (so the test can observe the allow side-effect).
//
// This composes the REAL in-process transport on both sides: the production WS
// handler (apihttp.Server.ChatWS) and the production wsBackend client. Only the
// model/tool fixtures are synthetic — everything between them is the live code
// path the TUI uses.
func newParityServer(t *testing.T) (wsURL, workdir string) {
	t.Helper()
	workdir = t.TempDir()

	// alpha is the default model AND models["alpha"]. Scripted to: (1) reply
	// with usage for the usage-check turn, (2) call fs_write for the permission
	// turn, (3) emit a final message after the tool runs. beta is the switch
	// target, distinguishable by its reply text.
	alpha := einollm.NewFakeModelWithMessages([]*schema.Message{
		assistantWithUsageMsg("usage-reply", 10, 5),
		schema.AssistantMessage("", []schema.ToolCall{
			{ID: "c1", Type: "function", Function: schema.FunctionCall{
				Name:      "fs_write",
				Arguments: `{"path":"out.txt","content":"hello"}`,
			}},
		}),
		schema.AssistantMessage("written", nil),
	}, nil)
	beta := einollm.NewFakeModel([]string{"from-beta"}, nil)
	models := map[string]model.BaseChatModel{"alpha": alpha, "beta": beta}

	fs := tools.NewFSTools(workdir)
	o, err := orchestrator.New(orchestrator.Config{
		Model: alpha, // default; also reachable as models["alpha"]
		Tools: []orchestrator.BaseTool{fs.Write},
		// Tool name allowed; Write allowlist grants a path that EXCLUDES the
		// test's "out.txt" so fs_write returns Prompt (interactive path).
		// After Task 1, an empty Write list would HardDeny and skip the
		// callback entirely.
		Profile: guard.PermissionProfile{
			Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
			FS:    guard.FSPerm{Write: []string{filepath.Join(workdir, "safe/**")}},
		},
	})
	require.NoError(t, err)

	s := apihttp.New(apihttp.Config{Token: "t"}) // compaction Threshold 0 -> disabled
	s.ChatWS(o, models, nil)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	wsURL = "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/v1/chat/ws"
	return wsURL, workdir
}

// findEvent returns the first event with the given Kind in evs, or false.
func findEvent(evs []StreamEvent, kind string) (StreamEvent, bool) {
	for _, ev := range evs {
		if ev.Kind == kind {
			return ev, true
		}
	}
	return StreamEvent{}, false
}

// TestIntegration_Parity exercises the Phase-10 feature-parity features
// end-to-end over the real in-process WS transport (production handler +
// production wsBackend client): model listing/switch, thinking effort, usage
// accounting, and interactive tool permissions. Booted against an isolated temp
// workdir with two fake models and a permission-triggering tool, it proves the
// full client-side StreamEvent flow — control replies (models/status), turn
// accrual, and the permission_request -> permission_response -> tool_result
// round-trip — that the TUI depends on.
func TestIntegration_Parity(t *testing.T) {
	wsURL, workdir := newParityServer(t)
	// context.Background() (never cancelled) matches every wsBackend test in
	// the package: Send/SendFrame each spawn a cancellation goroutine that only
	// fires when this ctx is cancelled, so a cancellable ctx would make them all
	// fire at test return and race on conn.WriteMessage. The permission turn
	// below drains with an explicit timeout guard so the test can't hang.
	b, err := newWSBackend(context.Background(), wsURL)
	require.NoError(t, err)
	defer b.Close()
	assert.Equal(t, "ws", b.Mode(), "in-process transport must resolve to WebSocket")

	// --- Linear control frames + usage turn (driven via the frame seam). ---
	evs, err := runHeadlessWithFrames(context.Background(), b, []proto.ClientFrame{
		proto.NewListModels(),          // -> models
		proto.NewSetThinking("medium"), // -> status (thinking)
		proto.NewUserMessage("usage"),  // turn on alpha (usage-bearing reply)
		proto.NewGetStatus(),           // -> status (usage + turns)
	})
	require.NoError(t, err)

	// list_models -> models frame with the configured names.
	modelsEv, ok := findEvent(evs, "models")
	require.True(t, ok, "expected a models event from list_models")
	assert.ElementsMatch(t, []string{"alpha", "beta"}, modelsEv.Items,
		"models frame must list the configured provider names")

	// set_thinking medium -> a status frame reflecting it.
	thinkEv, ok := findEvent(evs, "status")
	require.True(t, ok, "expected a status frame after set_thinking")
	assert.Equal(t, "medium", thinkEv.Thinking, "status must reflect set_thinking")

	// A turn accrues usage: get_status (the LAST status) must show turns > 0
	// and, because alpha's reply carries TokenUsage, non-zero input tokens.
	// (Find the final status — the get_status reply — which is the last one.)
	var statusEv StreamEvent
	for _, ev := range evs {
		if ev.Kind == "status" {
			statusEv = ev // keep the last status (get_status reply)
		}
	}
	assert.Equal(t, "medium", statusEv.Thinking, "usage status still reflects thinking")
	assert.GreaterOrEqual(t, statusEv.Turns, 1, "at least one turn must have run")
	assert.Greater(t, statusEv.TokensIn, 0,
		"usage-bearing fake model must accrue input tokens (got %d)", statusEv.TokensIn)

	// --- Permission turn (driven inline: read permission_request, respond). ---
	// fs_write static-denies -> the WS handler's ask callback emits
	// permission_request mid-turn; we answer allow, and the tool must run.
	permCh, err := b.Send(context.Background(), "write the file")
	require.NoError(t, err)

	var (
		sawToolCall bool
		permReq     StreamEvent
		gotPermReq  bool
		sawResult   bool
		resultEv    StreamEvent
	)
	// Drain with a timeout guard: the turn blocks on the permission reply, so a
	// plain range could hang forever if the prompt never arrives.
	deadline := time.After(5 * time.Second)
loop:
	for {
		select {
		case ev, ok := <-permCh:
			if !ok {
				break loop
			}
			switch ev.Kind {
			case "tool_call":
				sawToolCall = true
				assert.Equal(t, "fs_write", ev.ToolName)
			case "permission_request":
				permReq = ev
				gotPermReq = true
				assert.Equal(t, "fs_write", permReq.ToolName)
				assert.NotEmpty(t, permReq.ID, "permission_request must carry an id")
				assert.NotEmpty(t, permReq.Reason, "reason must explain the static denial")
				// Answer allow WITHOUT disturbing the turn's channel (mid-turn reply).
				_, perr := b.SendFrame(context.Background(), proto.NewPermissionResponse(permReq.ID, "allow"))
				require.NoError(t, perr)
			case "tool_result":
				sawResult = true
				resultEv = ev
			case "error":
				t.Fatalf("permission turn errored: %v", ev.Err)
			}
		case <-deadline:
			t.Fatal("permission turn timed out waiting for tool_result/done")
		}
	}
	require.True(t, sawToolCall, "turn must emit a tool_call for fs_write")
	require.True(t, gotPermReq, "fs_write must trigger a permission_request")
	require.True(t, sawResult, "allow must let fs_write run and produce a tool_result")
	assert.Equal(t, "ok", resultEv.ToolStatus, "tool_result status must be ok after allow")
	assert.FileExists(t, filepath.Join(workdir, "out.txt"),
		"allow must let fs_write create the file")

	// --- Model switch: set_model beta -> subsequent turn runs on beta. ---
	switchEvs, err := runHeadlessWithFrames(context.Background(), b, []proto.ClientFrame{
		proto.NewSetModel("beta"),         // -> status (model=beta)
		proto.NewUserMessage("beta turn"), // turn on beta
	})
	require.NoError(t, err)

	switchStatus, ok := findEvent(switchEvs, "status")
	require.True(t, ok, "expected a status frame after set_model")
	assert.Equal(t, "beta", switchStatus.Model, "get_status must reflect the selected model")

	var betaReply strings.Builder
	for _, ev := range switchEvs {
		if ev.Kind == "agent_chunk" {
			betaReply.WriteString(ev.Text)
		}
	}
	assert.Contains(t, betaReply.String(), "from-beta",
		"set_model beta must route the subsequent turn to beta (distinguishable reply)")
}
