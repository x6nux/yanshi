package v1

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/proto"
)

// visionPNGB64 is a minimal 1x1 red PNG, base64-encoded — the payload a real
// client would attach.
func visionPNGB64(t *testing.T) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

// TestServiceTurnCarriesImagesToTheOrchestrator proves the v1 turn path forwards
// TurnStartParams.Images. Before this task runTurn read only p.Input, so the
// Images field — present in the wire contract since Tier G — was accepted by
// every v1 client and then silently dropped.
//
// The observation is the assistant text itself: the FakeModel runs in Vision
// mode, so its reply reports how many image parts actually reached it. Reading
// it off the item stream (rather than the fake's recorded state) keeps the
// assertion race-free. The registry holds exactly one model and the request
// leaves Model empty, so a "1 image" reply also proves the ModelID fallback:
// without a model id the orchestrator would treat the turn as non-multimodal and
// substitute a text placeholder, yielding zero image parts.
func TestServiceTurnCarriesImagesToTheOrchestrator(t *testing.T) {
	fm := einollm.NewFakeModel(nil, nil)
	fm.Vision = true
	o, err := orchestrator.New(orchestrator.Config{
		Model:         fm,
		MultimodalMap: map[string]bool{"vision-model": true},
	})
	require.NoError(t, err)
	svc, err := NewService(Config{
		Orchestrator: o,
		Models:       map[string]model.BaseChatModel{"vision-model": fm},
	})
	require.NoError(t, err)

	thread, err := svc.Start(context.Background(), ThreadStartParams{Title: "vision"})
	require.NoError(t, err)
	_, items, err := svc.StartTurn(context.Background(), TurnStartParams{
		ThreadID: thread.ID,
		Input:    "what is this?",
		Images:   []proto.ImageAttach{{Source: "attach", Fmt: "png", W: 1, H: 1, DataB64: visionPNGB64(t)}},
	})
	require.NoError(t, err)

	var reply strings.Builder
	for item := range items {
		reply.WriteString(item.Text)
	}
	assert.Contains(t, reply.String(), "fake-vision(1 image)",
		"the v1 turn must carry p.Images into TurnOpts — got %q", reply.String())
}
