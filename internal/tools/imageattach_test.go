package tools

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/x6nux/yanshi/internal/guard"
)

// imageFSCtx binds the profile fs_read needs plus the work root, mirroring what
// the orchestrator's withTurnContext binds before a turn runs.
func imageFSCtx(root string) context.Context {
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
		FS:    guard.FSPerm{Read: []string{root + "/**"}},
	})
	return WithWorkRoot(ctx, root)
}

// imageWebCtx binds the profile + network policy web_fetch needs. The net guard
// has ALREADY authorized the request by the time the image branch runs; these
// tests only exercise how an authorized response is presented.
func imageWebCtx() context.Context {
	ctx := WithProfile(context.Background(), profileWithWebTool())
	return WithNetworkPolicy(ctx, allowAllPolicy())
}

// Assertion 1 (Tier G entry C, fs_read): reading a .png yields a REAL
// attachment — the actual bytes reach the turn's image sink — not a sentence
// telling the model to go call another tool.
func TestFSRead_ImageProducesRealAttachment(t *testing.T) {
	dir := t.TempDir()
	want := writePNG(t, filepath.Join(dir, "shot.png"))
	fs := NewFSTools(dir)

	sink := NewTurnImages(true)
	out, err := runTool(WithTurnImages(imageFSCtx(dir), sink), fs.Read, `{"path":"shot.png"}`)
	require.NoError(t, err)

	imgs := sink.Drain()
	require.Len(t, imgs, 1, "fs_read on an image must produce an attachment, got result: %s", out)
	assert.Equal(t, "png", imgs[0].Fmt)
	assert.Equal(t, "shot.png", imgs[0].Source)
	got, decErr := base64.StdEncoding.DecodeString(imgs[0].DataB64)
	require.NoError(t, decErr)
	assert.Equal(t, want, got, "the attachment must carry the file's real bytes")
	assert.Contains(t, out, "image attached")
	assert.NotContains(t, out, "call image_describe with this path")
}

// The multimodal fallback: with no multimodal model on this turn there is no
// place for an image part to go, so fs_read must return the original hint text
// rather than pushing binary into a text message.
func TestFSRead_ImageFallsBackWhenModelIsNotMultimodal(t *testing.T) {
	dir := t.TempDir()
	writePNG(t, filepath.Join(dir, "shot.png"))
	fs := NewFSTools(dir)

	sink := NewTurnImages(false)
	out, err := runTool(WithTurnImages(imageFSCtx(dir), sink), fs.Read, `{"path":"shot.png"}`)
	require.NoError(t, err)
	assert.Empty(t, sink.Drain(), "a non-multimodal turn must not collect attachments")
	assert.Contains(t, out, "call image_describe with this path")

	// No sink bound at all (headless tool call) behaves the same way.
	out, err = runTool(imageFSCtx(dir), fs.Read, `{"path":"shot.png"}`)
	require.NoError(t, err)
	assert.Contains(t, out, "call image_describe with this path")
}

// Assertion 2 (Tier G entry C, web_fetch): an image/* response becomes a real
// attachment too. The net guard already decided the request was allowed; this
// only changes how the AUTHORIZED response is presented to the model.
func TestWebFetch_ImageResponseProducesRealAttachment(t *testing.T) {
	png := writePNG(t, filepath.Join(t.TempDir(), "src.png"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(png)
	}))
	defer srv.Close()

	sink := NewTurnImages(true)
	out, err := runTool(WithTurnImages(imageWebCtx(), sink), NewWebTools(1024*32, 0).Fetch, `{"url":"`+srv.URL+`"}`)
	require.NoError(t, err)

	imgs := sink.Drain()
	require.Len(t, imgs, 1, "web_fetch on an image/* response must produce an attachment, got result: %s", out)
	assert.Equal(t, "png", imgs[0].Fmt)
	got, decErr := base64.StdEncoding.DecodeString(imgs[0].DataB64)
	require.NoError(t, decErr)
	assert.Equal(t, png, got)
	assert.Contains(t, out, "image attached")

	t.Run("non-image content type is untouched", func(t *testing.T) {
		text := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<p>hello web</p>"))
		}))
		defer text.Close()
		s := NewTurnImages(true)
		body, ferr := runTool(WithTurnImages(imageWebCtx(), s), NewWebTools(1024*32, 0).Fetch, `{"url":"`+text.URL+`"}`)
		require.NoError(t, ferr)
		assert.Empty(t, s.Drain())
		assert.Contains(t, body, "hello web")
	})
}

// Assertion 3: an oversize image is REJECTED. A hostile (or merely careless)
// server must not be able to blow up the context window by answering a
// web_fetch with a gigantic image/* body, and the same cap applies to fs_read.
func TestImageAttach_RejectsOversizeImage(t *testing.T) {
	t.Run("web_fetch", func(t *testing.T) {
		huge := make([]byte, maxImageAttachBytes+1)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(huge)
		}))
		defer srv.Close()

		sink := NewTurnImages(true)
		out, err := runTool(WithTurnImages(imageWebCtx(), sink), NewWebTools(1024*32, 0).Fetch, `{"url":"`+srv.URL+`"}`)
		require.NoError(t, err)
		assert.Empty(t, sink.Drain(), "an oversize image must never reach the model's context")
		assert.Contains(t, out, "image not attached")
	})

	t.Run("fs_read", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "huge.png"), make([]byte, maxImageAttachBytes+1), 0o600))
		fs := NewFSTools(dir)

		sink := NewTurnImages(true)
		out, err := runTool(WithTurnImages(imageFSCtx(dir), sink), fs.Read, `{"path":"huge.png"}`)
		require.NoError(t, err)
		assert.Empty(t, sink.Drain(), "an oversize image must never reach the model's context")
		assert.Contains(t, out, "image not attached")
	})

	t.Run("both entries share one limit constant", func(t *testing.T) {
		// The cap fs_read enforces IS the cap web_fetch enforces IS the cap the
		// @path entry enforces — one constant, not three drifting copies.
		assert.Equal(t, maxImageAttachBytes, 10<<20)
	})
}
