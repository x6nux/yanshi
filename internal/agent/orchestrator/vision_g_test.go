package orchestrator

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/imagestore"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/tools"
)

// tinyPNGBase64 is a 1x1 PNG. Real bytes rather than a made-up string because
// the placeholder path decodes and stores them.
const tinyPNGBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

func visionTurnImages() []proto.ImageAttach {
	return []proto.ImageAttach{{DataB64: tinyPNGBase64, Fmt: "png", Source: "attach"}}
}

// TestNoVisionPathIsAnErrorNotAPlaceholder covers the third clause.
//
// The clause reads "无辅助：error 而非静默" — with no auxiliary model, an error
// rather than silence — and until now the second half was what happened.
// ApplyImages never looked at whether an aux model existed: it inserted
// [image:img-N|...] regardless, and a placeholder is only a REFERENCE. The
// thing that resolves it is image_describe, which without an aux model answers
// with a configuration error. So the user attached an image, saw no error, and
// got an answer written as though the image had been read.
//
// The only existing test near this is tools::TestImageDescribeNoAuxReturnsConfigError,
// which covers the tool. Nothing covered the turn, which is where the clause
// puts the requirement.
//
// ledger: G/VISION#3 无辅助：error 而非静默
func TestNoVisionPathIsAnErrorNotAPlaceholder(t *testing.T) {
	store := imagestore.New(imagestore.Config{MaxItems: 20, MaxBytes: 100 << 20})
	mdl := einollm.NewFakeModel([]string{"answered"}, nil)

	o, err := New(Config{
		Model: mdl,
		// The turn's model is not in the map, so it is not multimodal.
		MultimodalMap: map[string]bool{"gpt-4o": true},
		ImageStore:    store,
		// The condition under test.
		VisionAuxAvailable: false,
	})
	require.NoError(t, err)

	history := []*schema.Message{schema.UserMessage("what is in this picture?")}
	iter := o.EventsWithHistoryOpts(context.Background(), history,
		TurnOpts{ModelID: "text-only-model", Images: visionTurnImages()})

	var gotErr error
	for {
		ev, ok := iter.Next()
		if !ok {
			break
		}
		if ev.Err != nil {
			gotErr = ev.Err
		}
	}

	require.Error(t, gotErr,
		"the turn completed silently with no vision path; the user gets an answer "+
			"written as though the image had been read")
	assert.True(t, errors.Is(gotErr, ErrNoVisionPath), "got %v", gotErr)
	// The message has to tell the operator what to change. "unsupported" would
	// satisfy the clause's letter and leave them nowhere.
	for _, want := range []string{"multimodal", "/model"} {
		assert.Contains(t, gotErr.Error(), want,
			"the error does not say how to fix it: %v", gotErr)
	}

	// And the model must not have been called: reporting the error while still
	// running the turn would produce the confident answer anyway.
	assert.Zero(t, mdl.GenerateCalls+mdl.StreamCalls,
		"the turn ran despite having no way to see the image")
}

// TestPlaceholderResolvesThroughImageDescribe covers the seam in clause 2.
//
// Two halves were tested separately: that the placeholder path puts an image
// into the store, and that image_describe returns a description for an id. What
// was never tested is that the id in the placeholder is one image_describe can
// resolve ON THE SAME STORE — which is the entire meaning of "走通". A
// placeholder naming an id the tool cannot find fails both halves' tests and
// neither of them notices.
//
// ledger: G/VISION#2 主非多模态+有辅助：占位+image_describe 走通
func TestPlaceholderResolvesThroughImageDescribe(t *testing.T) {
	store := imagestore.New(imagestore.Config{MaxItems: 20, MaxBytes: 100 << 20})

	o, err := New(Config{
		Model:              einollm.NewFakeModel([]string{"ok"}, nil),
		MultimodalMap:      map[string]bool{"gpt-4o": true},
		ImageStore:         store,
		VisionAuxAvailable: true,
	})
	require.NoError(t, err)

	history := []*schema.Message{schema.UserMessage("describe it")}
	out, err := o.applyImages(history, "text-only-model", visionTurnImages())
	require.NoError(t, err)
	require.NotEmpty(t, out)

	last := out[len(out)-1]
	require.Equal(t, schema.User, last.Role)

	// Pull the id out of the placeholder exactly as a model would have to.
	id := placeholderID(t, last.Content)
	require.NotEmpty(t, id, "no [image:...] placeholder in %q", last.Content)

	// The seam: the same store must resolve it.
	aux := einollm.NewFakeModel([]string{"a one-pixel image"}, nil)
	toolRoot := t.TempDir()
	tool := tools.NewImageDescribeTool(aux, store, toolRoot, nil)

	// Deliberately passed with a trailing space: a model copying the ref out of
	// "[image:img-1 | attach | 1x1 png]" naturally takes the space with it, and
	// an untrimmed lookup answers "not found in store" for an image that is
	// right there. See the TrimSpace in imageDescribeState.run.
	res, err := tool.InvokableRun(
		tools.WithProfile(context.Background(), guard.PermissionProfile{
			Tools: guard.ToolsPerm{Allow: []string{"*"}},
			// image_describe authorises a read even for the id form, so the
			// profile needs a read path or the call never reaches the store.
			// Wide open because the subject here is whether the store resolves
			// the placeholder id, not path policy — and on macOS t.TempDir
			// hands back /var/... while the guard sees the /private/var/...
			// it resolves to, so a narrow pattern would fail for a reason that
			// has nothing to do with this clause.
			FS: guard.FSPerm{Read: []string{"/**", toolRoot, toolRoot + "/**"}},
		}),
		`{"image_ref":"`+id+` "}`)
	require.NoError(t, err)
	assert.NotContains(t, res, "not found",
		"the placeholder names an image id that image_describe cannot resolve on the "+
			"same store, so the placeholder path leads nowhere: %s", res)
	assert.Contains(t, res, "one-pixel",
		"image_describe did not return the auxiliary model's description: %s", res)
}

// placeholderID extracts img-N from the first [image:img-N|...] token.
func placeholderID(t *testing.T, content string) string {
	t.Helper()
	i := strings.Index(content, "[image:")
	if i < 0 {
		return ""
	}
	rest := content[i+len("[image:"):]
	j := strings.IndexAny(rest, "|]")
	if j < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:j])
}
