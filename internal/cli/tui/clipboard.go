package tui

// Tier G entry-A — clipboard image paste.
//
// Before this file `internal/clipimg` had ZERO importers: the capability to
// read an image off the OS clipboard existed but was unreachable from the UI,
// and `buildSendFrame` (images.go) had zero production callers. This file is
// the bridge: Ctrl+V grabs a clipboard image into model.pendingImages, and
// dispatchSend (queue.go) drains that set through buildSendFrame +
// tuiSession.SendFrame.
//
// The load-bearing rule here is SILENCE ON MISS. In a terminal Ctrl+V is also
// the ordinary "paste text" chord, so the overwhelmingly common case is a
// clipboard holding no image at all. Reporting that as an error would put a
// toast on the screen every time the user pastes a snippet. clipimg's backend
// contract already distinguishes "no image" (ok=false) from a real failure, so
// a miss is simply a no-op and the keystroke falls through to the textarea.

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	_ "image/gif"  // register GIF for DecodeConfig (dims only)
	_ "image/jpeg" // register JPEG for DecodeConfig (dims only)
	_ "image/png"  // register PNG for DecodeConfig (dims only)
	"time"

	"github.com/x6nux/yanshi/internal/clipimg"
	"github.com/x6nux/yanshi/internal/proto"
)

// clipImageFunc reads one image from the clipboard. ok=false means "no image
// present", which is a normal outcome and never an error. It is a function
// type (not an interface) so tests inject a plain closure — the repo's
// fake-over-mock convention, matching clipimg's own commandOutput seam.
type clipImageFunc func(ctx context.Context) (data []byte, format string, ok bool)

// clipGrabTimeout bounds the clipboard read. Every platform backend shells out
// to a helper process (pngpaste / xclip / PowerShell); a wedged helper must not
// freeze the TUI's event loop, so the grab is given a hard deadline.
const clipGrabTimeout = 3 * time.Second

// readClipboardImage is the production grabber: the real OS clipboard via
// internal/clipimg.
func readClipboardImage(ctx context.Context) ([]byte, string, bool) {
	return clipimg.New().Read(ctx)
}

// grabber returns the injected clipboard reader, defaulting to the real one.
func (m *model) grabber() clipImageFunc {
	if m.clipImage != nil {
		return m.clipImage
	}
	return readClipboardImage
}

// attachClipboardImage pulls an image off the clipboard into pendingImages.
// It reports whether an image was actually attached; false means the clipboard
// held no image (or empty bytes) and the caller must do nothing visible — see
// the silence rule in this file's header.
func (m *model) attachClipboardImage() bool {
	ctx, cancel := context.WithTimeout(context.Background(), clipGrabTimeout)
	defer cancel()
	data, format, ok := m.grabber()(ctx)
	if !ok || len(data) == 0 {
		return false
	}
	w, h := imageDims(data)
	m.pendingImages = append(m.pendingImages, proto.ImageAttach{
		ID:      fmt.Sprintf("img-%d", len(m.pendingImages)+1),
		Source:  "clipboard",
		Fmt:     format,
		W:       w,
		H:       h,
		DataB64: base64.StdEncoding.EncodeToString(data),
	})
	return true
}

// takePendingImages returns the pending attachments and clears them, so one
// paste rides exactly one turn instead of sticking to every later turn.
func (m *model) takePendingImages() []proto.ImageAttach {
	if len(m.pendingImages) == 0 {
		return nil
	}
	out := m.pendingImages
	m.pendingImages = nil
	return out
}

// imageDims decodes just the image header for display dimensions. Unknown or
// undecodable formats (e.g. webp, which stdlib does not register) yield 0x0 —
// harmless, since ImageAttach.W/H are omitempty hints for the placeholder.
func imageDims(data []byte) (int, int) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0
	}
	return cfg.Width, cfg.Height
}
