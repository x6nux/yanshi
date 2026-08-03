package orchestrator

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/require"
	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/imagestore"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/tools"
)

// encodePNGBase64ForE2E generates a minimal valid PNG as base64.
func encodePNGBase64ForE2E(t *testing.T) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	for y := 0; y < 2; y++ {
		for x := 0; x < 2; x++ {
			img.Set(x, y, color.RGBA{R: 255, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

// TestE2E_MultimodalMainDirectlyUnderstandsImage proves acceptance criterion 1:
// when the main model is multimodal, the image goes directly into the message
// as a native image part (no store, no placeholder).
func TestE2E_MultimodalMainDirectlyUnderstandsImage(t *testing.T) {
	mm := einollm.NewFakeModel([]string{"it is a red square"}, nil)
	store := imagestore.New(imagestore.Config{MaxItems: 20, MaxBytes: 100 << 20})
	o, err := New(Config{
		Model:         mm,
		MultimodalMap: map[string]bool{"mm": true},
		ImageStore:    store,
	})
	require.NoError(t, err)

	msgs := o.ApplyImages([]*schema.Message{}, "mm", []proto.ImageAttach{
		{Fmt: "png", DataB64: encodePNGBase64ForE2E(t)},
	})
	require.NotEmpty(t, msgs[0].UserInputMultiContent, "multimodal model must embed image as native part")
	// Verify the model actually sees the image when it runs.
	mm.RecordImages = true
	mm.Generate(nil, msgs)
	require.Equal(t, 1, mm.LastImageCount, "model must see exactly 1 image part")
}

// TestE2E_NonMultimodalMainUsesPlaceholderAndStore proves acceptance criterion 2:
// when the main model is non-multimodal, the image goes into the store and a
// placeholder appears in the user message (image_describe fetches by id later).
func TestE2E_NonMultimodalMainUsesPlaceholderAndStore(t *testing.T) {
	store := imagestore.New(imagestore.Config{MaxItems: 20, MaxBytes: 100 << 20})
	o, err := New(Config{
		Model:         einollm.NewFakeModel([]string{"ok"}, nil),
		MultimodalMap: map[string]bool{"text": false},
		ImageStore:    store,
	})
	require.NoError(t, err)

	msgs := o.ApplyImages([]*schema.Message{}, "text", []proto.ImageAttach{
		{Fmt: "png", DataB64: encodePNGBase64ForE2E(t)},
	})
	require.Contains(t, msgs[0].Content, "[image:img-1", "placeholder must reference the stored image id")
	_, ok := store.Get("img-1")
	require.True(t, ok, "image must be in the store for image_describe")
}

// TestE2E_PathRefTurnWiring proves Tier G entry B is actually wired into the
// turn path: a "@path" reference in the trailing user message reaches the model
// as an image part, an escaping reference does not, and neither rewrites the
// caller's history slice (the WS turn loop reuses it across retries).
func TestE2E_PathRefTurnWiring(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "work")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "shots"), 0o750))
	raw, err := base64.StdEncoding.DecodeString(encodePNGBase64ForE2E(t))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, "shots", "a.png"), raw, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(parent, "secret.png"), raw, 0o600))

	newOrch := func() (*Orchestrator, *einollm.FakeModel) {
		fm := einollm.NewFakeModel([]string{"unused"}, nil)
		fm.Vision, fm.RecordImages = true, true
		o, err := New(Config{
			Model:         fm,
			MultimodalMap: map[string]bool{"mm": true},
			ImageStore:    imagestore.New(imagestore.Config{MaxItems: 20, MaxBytes: 100 << 20}),
			WorkRoot:      root,
			Profile: guard.PermissionProfile{
				Tools: guard.ToolsPerm{Allow: []string{"*"}},
				FS:    guard.FSPerm{Read: []string{"**"}},
			},
		})
		require.NoError(t, err)
		return o, fm
	}

	t.Run("in-root ref reaches the model as an image part", func(t *testing.T) {
		o, _ := newOrch()
		msgs := []*schema.Message{{Role: schema.User, Content: "看 @shots/a.png"}}
		out := drainAgentChunks(t, o.EventsWithHistoryOpts(context.Background(), msgs, TurnOpts{ModelID: "mm"}))
		require.Contains(t, out, "fake-vision(1 image)")
		require.Equal(t, "看 @shots/a.png", msgs[0].Content, "caller history must not be rewritten in place")
	})

	t.Run("escaping ref reaches the model as text only", func(t *testing.T) {
		o, _ := newOrch()
		msgs := []*schema.Message{{Role: schema.User, Content: "看 @../secret.png"}}
		out := drainAgentChunks(t, o.EventsWithHistoryOpts(context.Background(), msgs, TurnOpts{ModelID: "mm"}))
		require.Contains(t, out, "fake-vision(0 images)")
	})
}

// TestE2E_ToolProducedImageReachesTheModel proves Tier G entry C's third leg:
// an image a TOOL produced mid-turn (the bytes cannot ride the tool's string
// result) reaches the model as a native image part, and reaches it exactly once
// even though the ReAct loop calls the model again on every iteration.
func TestE2E_ToolProducedImageReachesTheModel(t *testing.T) {
	mm := einollm.NewFakeModel([]string{"ok"}, nil)
	o, err := New(Config{Model: mm, MultimodalMap: map[string]bool{"mm": true}})
	require.NoError(t, err)

	ctx := o.withTurnContext(context.Background(), TurnOpts{ModelID: "mm"})
	sink := tools.TurnImagesFromContext(ctx)
	require.NotNil(t, sink, "withTurnContext must bind the turn's image sink")
	require.True(t, sink.Multimodal(), "a multimodal ModelID must produce a collecting sink")

	// What fs_read/web_fetch do from inside the ReAct loop.
	require.NoError(t, sink.Add(proto.ImageAttach{
		Source: "shot.png", Fmt: "png", DataB64: encodePNGBase64ForE2E(t),
	}))

	att := newImageAttacher()
	state := &adk.ChatModelAgentState{Messages: []*schema.Message{
		schema.UserMessage("look at it"),
		{Role: schema.Tool, Content: "[image attached: shot.png (png, 90 B)]"},
	}}
	_, state, err = att.BeforeModelRewriteState(ctx, state, nil)
	require.NoError(t, err)

	mm.RecordImages = true
	mm.Generate(nil, state.Messages)
	require.Equal(t, 1, mm.LastImageCount, "the tool-produced image must reach the model")

	// The ADK persists the returned state and calls the model again next
	// iteration; a second drain must not append a duplicate copy.
	before := len(state.Messages)
	_, state, err = att.BeforeModelRewriteState(ctx, state, nil)
	require.NoError(t, err)
	require.Len(t, state.Messages, before, "a drained sink must not re-attach the same image")

	mm.LastImageCount = 0
	mm.Generate(nil, state.Messages)
	require.Equal(t, 1, mm.LastImageCount, "still exactly one image after the second iteration")
}

// TestE2E_NonMultimodalTurnBindsARefusingSink is the fallback contract the tools
// rely on: with a non-multimodal turn model there is nowhere for an image part
// to go, so the sink refuses to collect and fs_read/web_fetch return hint text
// instead of pushing binary into a text message.
func TestE2E_NonMultimodalTurnBindsARefusingSink(t *testing.T) {
	o, err := New(Config{
		Model:         einollm.NewFakeModel([]string{"ok"}, nil),
		MultimodalMap: map[string]bool{"text": false},
	})
	require.NoError(t, err)

	for _, modelID := range []string{"text", "" /* no model selected */} {
		sink := tools.TurnImagesFromContext(o.withTurnContext(context.Background(), TurnOpts{ModelID: modelID}))
		require.NotNil(t, sink)
		require.False(t, sink.Multimodal(), "modelID %q must not collect attachments", modelID)
		require.Error(t, sink.Add(proto.ImageAttach{Fmt: "png"}))
		require.Empty(t, sink.Drain())
	}
}
