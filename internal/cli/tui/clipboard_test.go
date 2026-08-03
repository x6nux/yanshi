package tui

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// samplePNG encodes a real 2x3 PNG so the attach path exercises the genuine
// image header decode (dims) instead of a hand-waved byte blob.
func samplePNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 3))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

// fakeClipImage is the injectable clipboard-image grabber used by these tests
// (fake over mock, per repo convention): it returns a fixed payload and counts
// calls, with no OS clipboard and no subprocess involved.
type fakeClipImage struct {
	data  []byte
	fmtID string
	ok    bool
	calls int
}

func (f *fakeClipImage) read(context.Context) ([]byte, string, bool) {
	f.calls++
	return f.data, f.fmtID, f.ok
}

// TestCtrlVAttachesClipboardImage asserts entry-A: Ctrl+V pulls the clipboard
// image into the model's pending attachments.
func TestCtrlVAttachesClipboardImage(t *testing.T) {
	fake := &fakeClipImage{data: samplePNG(t), fmtID: "png", ok: true}
	m := wsModel(&recordingSession{})
	m.clipImage = fake.read

	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlV})
	m = mm.(model)

	if fake.calls != 1 {
		t.Fatalf("clipboard grabber calls = %d, want 1", fake.calls)
	}
	if len(m.pendingImages) != 1 {
		t.Fatalf("pendingImages = %#v, want 1 attachment", m.pendingImages)
	}
	got := m.pendingImages[0]
	if got.Fmt != "png" {
		t.Errorf("fmt = %q, want png", got.Fmt)
	}
	if got.DataB64 == "" {
		t.Error("dataB64 is empty: clipboard bytes were dropped")
	}
	if got.ID == "" {
		t.Error("id is empty: attachment needs a stable client-side id")
	}
	if got.W != 2 || got.H != 3 {
		t.Errorf("dims = %dx%d, want 2x3", got.W, got.H)
	}
}

// TestPendingImagesRideNextSendAndClear asserts the second half of entry-A: the
// next user turn carries the attachment on a user_message frame (through the
// previously-unused buildSendFrame seam) and the pending set is emptied so the
// image is not re-sent on every later turn.
func TestPendingImagesRideNextSendAndClear(t *testing.T) {
	fake := &fakeClipImage{data: samplePNG(t), fmtID: "png", ok: true}
	rec := &recordingSession{}
	m := wsModel(rec)
	m.clipImage = fake.read

	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlV})
	m = mm.(model)
	m.input.SetValue("what is this")
	mm2, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = mm2.(model)

	if len(rec.frames) != 1 {
		t.Fatalf("frames = %v, want one user_message frame", frameTypes(rec.frames))
	}
	fr := rec.frames[0]
	if fr.Type != "user_message" || fr.Text != "what is this" {
		t.Fatalf("frame = %+v", fr)
	}
	if len(fr.Images) != 1 || fr.Images[0].DataB64 == "" {
		t.Fatalf("frame images = %#v, want the pasted attachment", fr.Images)
	}
	if len(rec.sentText) != 0 {
		t.Errorf("text-only Send used despite pending images: %v", rec.sentText)
	}
	if len(m.pendingImages) != 0 {
		t.Errorf("pendingImages = %#v after send, want cleared", m.pendingImages)
	}

	// A follow-up turn must be text-only again (no sticky re-send).
	m.input.SetValue("and now")
	mm3, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = mm3.(model)
	if len(rec.sentText) != 1 || rec.sentText[0] != "and now" {
		t.Errorf("second turn sentText = %v, want the plain text path", rec.sentText)
	}
	if len(rec.frames) != 1 {
		t.Errorf("second turn re-sent images: %v", frameTypes(rec.frames))
	}
}

// TestCtrlVWithoutImageIsSilent pins the "Ctrl+V is also plain text paste"
// requirement: when the clipboard holds no image the keystroke must not attach
// anything and must not raise a toast (an error toast on every text paste would
// spam the transcript).
func TestCtrlVWithoutImageIsSilent(t *testing.T) {
	fake := &fakeClipImage{ok: false}
	m := wsModel(&recordingSession{})
	m.clipImage = fake.read

	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlV})
	m = mm.(model)

	if len(m.pendingImages) != 0 {
		t.Fatalf("pendingImages = %#v, want none", m.pendingImages)
	}
	for _, tst := range m.toasts.items {
		if tst.Level == "error" || tst.Level == "warn" {
			t.Errorf("no-image Ctrl+V raised a %s toast: %q", tst.Level, tst.Text)
		}
	}
	_ = cmd
}
