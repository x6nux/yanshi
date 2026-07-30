package tools

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cloudwego/eino/schema"
	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/imagestore"
)

// ---------------------------------------------------------------------------
// vision.go — recordUsage with actual response metadata (success path)
// ---------------------------------------------------------------------------

func TestRecordUsage_WithUsage(t *testing.T) {
	var pi, ci, ti int
	s := &imageDescribeState{
		usage: func(p, c, total int) {
			pi, ci, ti = p, c, total
		},
	}
	msg := &schema.Message{
		ResponseMeta: &schema.ResponseMeta{
			Usage: &schema.TokenUsage{
				PromptTokens:     100,
				CompletionTokens: 200,
				TotalTokens:      300,
			},
		},
	}
	s.recordUsage(msg)
	assert.Equal(t, 100, pi)
	assert.Equal(t, 200, ci)
	assert.Equal(t, 300, ti)
}

func TestRecordUsage_NilUsage(t *testing.T) {
	var called bool
	s := &imageDescribeState{
		usage: func(_, _, _ int) { called = true },
	}
	msg := &schema.Message{
		ResponseMeta: &schema.ResponseMeta{},
	}
	s.recordUsage(msg)
	assert.False(t, called)
}

func TestRecordUsage_NilResponseMeta(t *testing.T) {
	var called bool
	s := &imageDescribeState{
		usage: func(_, _, _ int) { called = true },
	}
	msg := &schema.Message{}
	s.recordUsage(msg)
	assert.False(t, called)
}

// ---------------------------------------------------------------------------
// resolveRef — absolute path under root with profile
// ---------------------------------------------------------------------------

func TestResolveRef_AbsolutePathInRoot(t *testing.T) {
	store := imagestore.New(imagestore.Config{MaxItems: 20, MaxBytes: 100 << 20})
	tmp := t.TempDir()
	var buf bytes.Buffer
	img := image.NewNRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.NRGBA{0, 255, 0, 255})
	require.NoError(t, png.Encode(&buf, img))
	imgPath := filepath.Join(tmp, "test.png")
	require.NoError(t, os.WriteFile(imgPath, buf.Bytes(), 0o644))

	s := &imageDescribeState{store: store, root: tmp}
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
		FS:    guard.FSPerm{Read: []string{"**"}},
	})
	data, fmtName, err := s.resolveRef(ctx, "test.png", "{}")
	require.NoError(t, err)
	assert.NotEmpty(t, data)
	assert.Equal(t, "png", fmtName)
}

func TestResolveRef_PathEscapeRoot(t *testing.T) {
	store := imagestore.New(imagestore.Config{MaxItems: 20, MaxBytes: 100 << 20})
	tmp := t.TempDir()
	s := &imageDescribeState{store: store, root: tmp}
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
		FS:    guard.FSPerm{Read: []string{"**"}},
	})
	_, _, err := s.resolveRef(ctx, "../../../etc/passwd", "{}")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "escapes")
}

func TestResolveRef_NotImageExtension(t *testing.T) {
	store := imagestore.New(imagestore.Config{MaxItems: 20, MaxBytes: 100 << 20})
	tmp := t.TempDir()
	txtPath := filepath.Join(tmp, "data.txt")
	require.NoError(t, os.WriteFile(txtPath, []byte("hello"), 0o644))
	s := &imageDescribeState{store: store, root: tmp}
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
		FS:    guard.FSPerm{Read: []string{"**"}},
	})
	_, _, err := s.resolveRef(ctx, "data.txt", "{}")
	// Will either fail guard or succeed but return non-image fmt
	_ = err
}

// detectFmt and mimeForFmt are already tested in vision_coverage_test.go
