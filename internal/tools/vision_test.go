package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/imagestore"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
)

// testPNGBytes encodes a solid-color PNG for test image construction.
func testPNGBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: 255, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

type visionUsage struct {
	called                    bool
	prompt, completion, total int
}

func (v *visionUsage) record(p, c, t int) { v.called = true; v.prompt, v.completion, v.total = p, c, t }

func newVisionStore(t *testing.T) (*imagestore.Store, string) {
	t.Helper()
	s := imagestore.New(imagestore.Config{MaxItems: 20, MaxBytes: 100 << 20})
	id, err := s.Put(testPNGBytes(t, 4, 4), "paste", "png")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	return s, id
}

func visionProfileContext() context.Context {
	return WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"image_describe"}},
	})
}

func TestImageDescribeByIDReturnsAuxDescription(t *testing.T) {
	store, id := newVisionStore(t)
	aux := einollm.NewFakeModel(nil, nil)
	aux.Vision = true
	var recorded visionUsage
	tool := NewImageDescribeTool(aux, store, "", recorded.record)
	args, _ := json.Marshal(map[string]string{"image_ref": id, "question": "what is this?"})
	ch := tool.Stream(visionProfileContext(), string(args))
	var result string
	for c := range ch {
		if c.Err != nil {
			t.Fatalf("chunk err: %v", c.Err)
		}
		result = c.Result
	}
	if !strings.Contains(result, "fake-vision") {
		t.Fatalf("result = %q", result)
	}
	// FakeModel doesn't set ResponseMeta.Usage, so usage callback may not be called.
	// Real-model integration tests verify usage recording separately.
}

func TestImageDescribeDefaultQuestion(t *testing.T) {
	store, id := newVisionStore(t)
	aux := einollm.NewFakeModel(nil, nil)
	aux.Vision = true
	aux.RecordMessages = true
	tool := NewImageDescribeTool(aux, store, "", nil)
	args, _ := json.Marshal(map[string]string{"image_ref": id})
	ch := tool.Stream(visionProfileContext(), string(args))
	for range ch {
	}
	last := aux.ReceivedMessages[len(aux.ReceivedMessages)-1]
	var lastText string
	for _, part := range last.UserInputMultiContent {
		if part.Type == schema.ChatMessagePartTypeText && part.Text != "" {
			lastText = part.Text
		}
	}
	if !strings.Contains(lastText, "请描述这张图片") {
		t.Fatalf("default question not applied; last msg = %#v", last)
	}
}

func TestImageDescribeNoAuxReturnsConfigError(t *testing.T) {
	store, id := newVisionStore(t)
	tool := NewImageDescribeTool(nil, store, "", nil) // no aux
	args, _ := json.Marshal(map[string]string{"image_ref": id})
	ch := tool.Stream(visionProfileContext(), string(args))
	var result string
	for c := range ch {
		result = c.Result
	}
	if !strings.Contains(result, "multimodal") || !strings.Contains(result, "provider") {
		t.Fatalf("missing-aux error must explain the config gap: %q", result)
	}
}

func TestImageDescribeBadIDReturnsErrorResult(t *testing.T) {
	store, _ := newVisionStore(t)
	aux := einollm.NewFakeModel(nil, nil)
	aux.Vision = true
	tool := NewImageDescribeTool(aux, store, "", nil)
	args, _ := json.Marshal(map[string]string{"image_ref": "img-nope"})
	ch := tool.Stream(visionProfileContext(), string(args))
	var result string
	for c := range ch {
		result = c.Result
	}
	if !strings.Contains(result, "✗") || !strings.Contains(strings.ToLower(result), "not found") {
		t.Fatalf("bad id must return ✗ not-found result: %q", result)
	}
}

func TestImageDescribePathRefDeniedByGuard(t *testing.T) {
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"image_describe"}},
		FS:    guard.FSPerm{Read: []string{}}, // empty = deny all reads
	})
	store := imagestore.New(imagestore.Config{MaxItems: 20, MaxBytes: 100 << 20})
	aux := einollm.NewFakeModel(nil, nil)
	aux.Vision = true
	tool := NewImageDescribeTool(aux, store, t.TempDir(), nil)
	args, _ := json.Marshal(map[string]string{"image_ref": "shot.png"})
	ch := tool.Stream(ctx, string(args))
	var result string
	for c := range ch {
		result = c.Result
	}
	if !strings.Contains(strings.ToLower(result), "deny") && !strings.Contains(strings.ToLower(result), "✗") {
		t.Fatalf("path ref must be denied by guard FS check (empty read whitelist); got %q", result)
	}
}
