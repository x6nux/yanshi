package http

import (
	"encoding/base64"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/imagestore"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/tools"
)

// TestVisionTurnCostReachesTheStatusFrame is the last clause of G/VISION-TOOL.
//
// The ledger recorded this as "structurally true": images have no separate
// billing channel — the provider folds them into PromptTokens, and
// addProviderUsage takes that same usage — so nothing needed wiring. That
// reasoning is correct and is exactly the kind of claim the ledger exists to
// refuse: "should hold by construction" is not evidence, and every regression
// that would break it (dropping ResponseMeta on the image path, resetting
// billing per turn, a status frame built from a different field) leaves the
// reasoning intact while the number goes to zero.
//
// A cost of zero is also what an unconfigured price table produces, so the
// assertion has to be against the EXPECTED number, not merely "> 0".
//
// ledger: G/VISION-TOOL#4 费用纳入 /cost
func TestVisionTurnCostReachesTheStatusFrame(t *testing.T) {
	const (
		prompt     = 1000 // the image is folded in here by the provider
		completion = 20
		inPerM     = 3.0
		outPerM    = 15.0
	)

	fm := einollm.NewFakeModel(nil, nil)
	fm.Vision = true
	fm.Usage = &schema.TokenUsage{PromptTokens: prompt, CompletionTokens: completion}

	o, err := orchestrator.New(orchestrator.Config{
		Model:         fm,
		MultimodalMap: map[string]bool{"vision-model": true},
	})
	require.NoError(t, err)

	s := New(Config{
		Token: "t",
		PriceTab: map[string]einollm.ModelPricing{
			"vision-model": {InputPerM: inPerM, OutputPerM: outPerM},
		},
	})
	s.ChatWS(o, map[string]model.BaseChatModel{"vision-model": fm}, nil)

	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	c := dial(t, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/v1/chat/ws")
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewUserMessageWithImages("what is this?",
		[]proto.ImageAttach{{Source: "paste", Fmt: "png", W: 1, H: 1, DataB64: visionPNGB64(t)}})))

	var sawVisionReply bool
	for {
		f := readFrame(t, c)
		if f.Type == "agent_chunk" && strings.Contains(f.Text, "fake-vision(1 image)") {
			sawVisionReply = true
		}
		if f.Type == "done" {
			break
		}
	}
	// Without this the test could pass on a turn where the image never reached
	// the model at all — and a text-only turn also produces a cost.
	require.True(t, sawVisionReply, "the image never reached the model, so this is not a vision turn")

	// Ask for the status block the /cost command renders from.
	require.NoError(t, c.WriteJSON(proto.NewGetStatus()))
	var status *proto.ServerFrame
	for i := 0; i < 20 && status == nil; i++ {
		f := readFrame(t, c)
		if f.Type == "status" {
			status = &f
		}
	}
	require.NotNil(t, status, "no status frame came back")

	want := (float64(prompt)*inPerM + float64(completion)*outPerM) / 1_000_000
	assert.True(t, status.CostKnown, "the priced model reported an unknown cost")
	assert.InDelta(t, want, status.CostUSD, 1e-12,
		"a turn carrying an image billed %v, want %v — the image tokens are folded into "+
			"PromptTokens by the provider, so they must reach the same accumulator",
		status.CostUSD, want)
}

// TestAuxVisionModelTokensReachTheStatusFrame is the other half of the clause.
//
// The auxiliary model runs when the turn's model is not multimodal: the
// placeholder path stores the image, and image_describe calls a second model to
// look at it. Those tokens are spent on the user's behalf and cost money.
//
// They were collected — VisionUsageFunc fed a visionUsageAccumulator — and the
// accumulator had ZERO readers: a grep found the type, the App field, the
// wiring, and two tests asserting the accumulator adds up. Nothing read it. It
// is also process-wide while cost is per-session, so it could not have reached
// /cost even with a reader. The tokens now travel the same route sub-agent
// spend does: the turn's usage sink.
//
// ledger: G/VISION-TOOL#4 费用纳入 /cost
func TestAuxVisionModelTokensReachTheStatusFrame(t *testing.T) {
	const (
		auxPrompt     = 700
		auxCompletion = 30
		inPerM        = 3.0
		outPerM       = 15.0
	)

	// The aux model is what image_describe calls; it reports the usage.
	aux := einollm.NewFakeModel([]string{"a red pixel"}, nil)
	aux.Usage = &schema.TokenUsage{PromptTokens: auxPrompt, CompletionTokens: auxCompletion}

	// The turn's model asks for image_describe once, then answers. It reports
	// no usage of its own so the assertion isolates the auxiliary spend.
	main := einollm.NewFakeModelWithMessages([]*schema.Message{
		schema.AssistantMessage("", []schema.ToolCall{{
			ID: "c1", Type: "function",
			Function: schema.FunctionCall{Name: "image_describe", Arguments: `{"image_ref":"img-1"}`},
		}}),
		schema.AssistantMessage("it is a red pixel", nil),
	}, nil)

	store := imagestore.New(imagestore.Config{MaxItems: 20, MaxBytes: 100 << 20})
	id, err := store.Put(decodePNG(t, visionPNGB64(t)), "paste", "png")
	require.NoError(t, err)
	require.Equal(t, "img-1", id)

	root := t.TempDir()
	o, err := orchestrator.New(orchestrator.Config{
		Model:      main,
		ImageStore: store,
		Tools:      []orchestrator.BaseTool{tools.NewImageDescribeTool(aux, store, root, nil)},
		Profile: guard.PermissionProfile{
			Tools: guard.ToolsPerm{Allow: []string{"*"}},
			FS:    guard.FSPerm{Read: []string{"/**"}},
		},
	})
	require.NoError(t, err)

	s := New(Config{
		Token:    "t",
		PriceTab: map[string]einollm.ModelPricing{"main-model": {InputPerM: inPerM, OutputPerM: outPerM}},
	})
	s.ChatWS(o, map[string]model.BaseChatModel{"main-model": main}, nil)

	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	c := dial(t, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/v1/chat/ws")
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewUserMessage("what is [image:img-1]?")))
	for {
		if readFrame(t, c).Type == "done" {
			break
		}
	}

	require.NoError(t, c.WriteJSON(proto.NewGetStatus()))
	var status *proto.ServerFrame
	for i := 0; i < 20 && status == nil; i++ {
		f := readFrame(t, c)
		if f.Type == "status" {
			status = &f
		}
	}
	require.NotNil(t, status)

	want := (float64(auxPrompt)*inPerM + float64(auxCompletion)*outPerM) / 1_000_000
	assert.InDelta(t, want, status.CostUSD, 1e-12,
		"the auxiliary vision model's tokens did not reach the session cost: got %v, want %v",
		status.CostUSD, want)
}

// decodePNG turns the base64 fixture back into bytes for the store.
func decodePNG(t *testing.T, b64 string) []byte {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(b64)
	require.NoError(t, err)
	return raw
}
