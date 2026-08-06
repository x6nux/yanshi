package tools

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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/x6nux/yanshi/internal/guard"
)

// writePNG writes a 1x1 PNG at path and returns its raw bytes.
func writePNG(t *testing.T, path string) []byte {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	var buf bytes.Buffer
	img := image.NewNRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.NRGBA{0, 128, 255, 255})
	require.NoError(t, png.Encode(&buf, img))
	require.NoError(t, os.WriteFile(path, buf.Bytes(), 0o600))
	return buf.Bytes()
}

// pathrefCtx binds a permissive profile plus the work root, mirroring what the
// orchestrator's withTurnContext binds before a turn runs.
func pathrefCtx(root string) context.Context {
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
		FS:    guard.FSPerm{Read: []string{"**"}},
	})
	return WithWorkRoot(ctx, root)
}

// Assertion 1: an @path pointing at an image inside the work root becomes a
// real image attachment (bytes carried, format detected), and the "@" token is
// expanded out of the text.
//
// ledger: G/VISION-TOOL#1 五入口各自可产生图像附件
//
// ledger: G/VISION-TOOL#2 image_describe/id-ref+path-ref 走通
func TestResolveImagePathRefs_AttachesImageInsideRoot(t *testing.T) {
	root := t.TempDir()
	want := writePNG(t, filepath.Join(root, "screenshots", "a.png"))

	res := ResolveImagePathRefs(pathrefCtx(root), "看看 @screenshots/a.png 这张图")

	require.Len(t, res.Images, 1, "expected one attachment, rejected=%v", res.Rejected)
	assert.Empty(t, res.Rejected)
	assert.Equal(t, "png", res.Images[0].Fmt)
	assert.Equal(t, "screenshots/a.png", res.Images[0].Source)
	got, err := base64.StdEncoding.DecodeString(res.Images[0].DataB64)
	require.NoError(t, err)
	assert.Equal(t, want, got)
	assert.NotContains(t, res.Text, "@screenshots/a.png")
	assert.Contains(t, res.Text, "screenshots/a.png")
}

// Assertion 2: model-influenced text must not turn into an arbitrary file read.
// Every @path goes through the SAME door as the vision tool: the pathjail
// root-jail (withinRootAbs) and the FS guard.
//
// ledger: G/VISION-TOOL#3 超限/越权被拒
func TestResolveImagePathRefs_RejectsEscapingRef(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "work")
	require.NoError(t, os.MkdirAll(root, 0o750))

	t.Run("traversal to a non-image is never read", func(t *testing.T) {
		in := "read @../../etc/passwd please"
		res := ResolveImagePathRefs(pathrefCtx(root), in)
		assert.Empty(t, res.Images)
		assert.Equal(t, in, res.Text)
	})

	t.Run("traversal to a real image outside the root is rejected", func(t *testing.T) {
		writePNG(t, filepath.Join(parent, "secret.png"))
		in := "look at @../secret.png"
		res := ResolveImagePathRefs(pathrefCtx(root), in)
		assert.Empty(t, res.Images)
		require.Len(t, res.Rejected, 1)
		assert.Equal(t, "../secret.png", res.Rejected[0].Ref)
		assert.Contains(t, res.Rejected[0].Reason, "root")
		assert.Equal(t, in, res.Text, "a rejected ref stays verbatim in the text")
	})

	t.Run("an in-root image still needs FS guard approval", func(t *testing.T) {
		writePNG(t, filepath.Join(root, "b.png"))
		denied := WithWorkRoot(WithProfile(context.Background(), guard.PermissionProfile{
			Tools: guard.ToolsPerm{Allow: []string{"*"}},
			FS:    guard.FSPerm{Read: nil},
		}), root)
		res := ResolveImagePathRefs(denied, "see @b.png")
		assert.Empty(t, res.Images)
		require.Len(t, res.Rejected, 1)
	})
}

// Assertion 3: a non-image extension is left verbatim in the text — neither
// attached nor reported as an error. "@notes.md" is ordinary prose.
func TestResolveImagePathRefs_NonImageExtensionStaysText(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "notes.md"), []byte("hi"), 0o600))

	in := "看 @notes.md 和 @someone 的说明"
	res := ResolveImagePathRefs(pathrefCtx(root), in)

	assert.Equal(t, in, res.Text)
	assert.Empty(t, res.Images)
	assert.Empty(t, res.Rejected)
}
