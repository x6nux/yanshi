package tools

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/x6nux/yanshi/internal/proto"
)

// maxImageAttachBytes caps a SINGLE image attachment, and maxTurnImages caps how
// many attachments the tools of one turn may contribute in total.
//
// Both are context-window defences, not style limits. The URL fed to web_fetch
// and the path fed to fs_read are model output, so "fetch this 400 MB PNG" is a
// reachable instruction — and unlike a text body (which truncates harmlessly)
// half an image is worthless, so the only safe answer to an oversize image is to
// refuse it and say so. A hostile server can also answer any authorized request
// with a giant image/* body; the byte cap is what stops that response from
// becoming the whole context window.
//
// The byte cap is deliberately THE SAME constant the @path entry (pathref.go)
// enforces: fs_read, web_fetch and "@a.png" are three doors into one feature, and
// three separately-tuned caps would mean an image rejected at one door is
// accepted at another. It also matches imagestore's per-image default so an
// attachment accepted here is not silently dropped downstream.
const (
	maxImageAttachBytes = 10 << 20 // 10 MiB
	maxTurnImages       = 8
)

// TurnImages is the turn-scoped sink that carries image attachments from a tool
// back to the model.
//
// # Why a context sidecar and not a return value
//
// A tool result is a string. fs_read cannot "return a proto.ImageAttach" — the
// ADK writes whatever the tool returns into a text message, and stuffing base64
// bytes into that string would be worse than useless (it burns the context
// window and no provider would read it as an image). So the bytes need a channel
// that runs alongside the string result: the tool puts them here, and the
// orchestrator's image-attaching middleware drains the sink before the next model
// call and turns them into native image parts. The tool result string stays a
// short human-readable receipt ("[image attached: ...]") that tells the model the
// picture is present.
//
// # Why the sink knows whether the model is multimodal
//
// The decision "can this turn carry an image part at all" is a property of the
// turn's model, which the tool has no way to look up. Binding it onto the sink
// makes the fallback a single check at the call site: with no sink bound (a
// headless tool call) or a non-multimodal model, the tools return their original
// hint text instead of collecting anything. Pushing binary into a text message
// when the model cannot decode it is the one outcome that must never happen.
//
// A TurnImages is safe for concurrent use: the ADK may run several tool calls of
// one iteration in parallel, and every one of them may attach.
type TurnImages struct {
	mu         sync.Mutex
	multimodal bool
	images     []proto.ImageAttach
	// added counts attachments accepted over the WHOLE turn, not just the ones
	// currently pending. Drain must not refill the budget: otherwise a loop of
	// "fs_read an image, drain, fs_read another" would let one turn attach an
	// unbounded number of images, which is exactly what maxTurnImages exists to
	// prevent.
	added int
}

// NewTurnImages builds a sink for one turn. multimodal must be true only when
// the turn's model can accept native image parts; a false sink collects nothing
// and makes every image-producing tool fall back to its text hint.
func NewTurnImages(multimodal bool) *TurnImages {
	return &TurnImages{multimodal: multimodal}
}

// Multimodal reports whether this turn can carry image attachments. It is
// nil-safe: a nil sink (no turn bound, e.g. a headless tool invocation) reports
// false, which is the fail-safe answer — text hint, never binary.
func (t *TurnImages) Multimodal() bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.multimodal
}

// Add records one attachment. It returns an error (never panics on a nil sink)
// when the turn cannot take the image: no sink bound, a non-multimodal model, or
// the per-turn count budget already spent. The error text is written straight
// into the tool result, so it is phrased for the model to read.
func (t *TurnImages) Add(img proto.ImageAttach) error {
	if t == nil {
		return fmt.Errorf("no image sink bound to this turn")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.multimodal {
		return fmt.Errorf("this turn's model cannot accept images")
	}
	if t.added >= maxTurnImages {
		return fmt.Errorf("this turn already attached the maximum of %d images", maxTurnImages)
	}
	t.added++
	t.images = append(t.images, img)
	return nil
}

// Drain returns the attachments collected since the last Drain and empties the
// pending list. It is nil-safe and returns nil when nothing is pending, so the
// caller can treat "no sink" and "no images" identically.
//
// Draining (rather than reading) is what makes the attachment reach the model
// exactly once: the middleware appends the drained images to the ADK state, the
// ADK persists that state, and a second append on the next iteration would show
// the model the same picture twice.
func (t *TurnImages) Drain() []proto.ImageAttach {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := t.images
	t.images = nil
	return out
}

// turnImagesKey is the context key for the per-turn image sink. An unexported
// struct type prevents collisions with other context values.
type turnImagesKey struct{}

// WithTurnImages binds a per-turn image sink onto ctx. The orchestrator's
// withTurnContext calls this once per turn with a sink built from the turn
// model's multimodal capability; tools read it back with TurnImagesFromContext.
// A nil sink is bound as-is and behaves like no sink at all.
func WithTurnImages(ctx context.Context, sink *TurnImages) context.Context {
	return context.WithValue(ctx, turnImagesKey{}, sink)
}

// TurnImagesFromContext returns the turn's image sink, or nil when none is
// bound. Every method on *TurnImages is nil-safe, so callers do not need to
// check the result before using it.
func TurnImagesFromContext(ctx context.Context) *TurnImages {
	sink, _ := ctx.Value(turnImagesKey{}).(*TurnImages)
	return sink
}

// imageAttachEnabled reports whether the current turn can turn an image into a
// real attachment. It is the single gate both fs_read and web_fetch consult
// before taking their attachment branch.
func imageAttachEnabled(ctx context.Context) bool {
	return TurnImagesFromContext(ctx).Multimodal()
}

// attachFileImage is fs_read's Tier G entry C branch: it reads absPath (already
// jailed and FS-guarded by the caller) and hands the bytes to the turn's sink,
// returning the tool-result text. ref is the path as the model wrote it, which is
// what the model should see echoed back.
func attachFileImage(ctx context.Context, absPath, ref string) string {
	fi, err := os.Stat(absPath)
	if err != nil {
		return imageNotAttached(ref, "cannot stat the file: "+err.Error())
	}
	// The size check runs off the stat, before the read: reading first would
	// mean allocating the very buffer the cap exists to prevent.
	if fi.Size() > maxImageAttachBytes {
		return imageNotAttached(ref, oversizeReason(fi.Size()))
	}
	data, err := os.ReadFile(absPath)
	if err != nil {
		return imageNotAttached(ref, "cannot read the file: "+err.Error())
	}
	return attachImageBytes(ctx, ref, detectFmt(ref), data)
}

// attachResponseImage is web_fetch's Tier G entry C branch. It returns
// (text, true) when the response was handled as an image — including the
// rejection case, which the model still needs to be told about — and
// ("", false) when contentType names an image format that cannot be sent as a
// native part, so the caller falls back to its hint text.
//
// The net guard has ALREADY authorized this request; this only changes how an
// authorized response is presented.
func attachResponseImage(ctx context.Context, url, contentType string, body io.Reader) (string, bool) {
	format, ok := imageFmtFromContentType(contentType)
	if !ok {
		return "", false
	}
	// Read one byte past the cap: enough to KNOW the body is oversize without
	// ever holding the whole hostile response in memory.
	data, err := io.ReadAll(io.LimitReader(body, maxImageAttachBytes+1))
	if err != nil {
		return imageNotAttached(url, "cannot read the response body: "+err.Error()), true
	}
	if len(data) > maxImageAttachBytes {
		return imageNotAttached(url, oversizeReason(int64(len(data)))), true
	}
	return attachImageBytes(ctx, url, format, data), true
}

// attachImageBytes puts one image on the turn's sink and renders the tool result.
func attachImageBytes(ctx context.Context, source, format string, data []byte) string {
	err := TurnImagesFromContext(ctx).Add(proto.ImageAttach{
		Source:  source,
		Fmt:     format,
		DataB64: base64.StdEncoding.EncodeToString(data),
		// W/H are left zero deliberately: they are display dims for the
		// non-multimodal placeholder path, which this branch never takes.
	})
	if err != nil {
		return imageNotAttached(source, err.Error())
	}
	return fmt.Sprintf("[image attached: %s (%s, %s) -- the image itself is part of this turn; look at it directly]",
		source, format, humanBytes(len(data)))
}

// imageNotAttached renders the rejection result. It always names the reason:
// a bare failure would leave the model guessing whether the image was empty,
// forbidden or simply too large, and the difference decides what it does next.
func imageNotAttached(source, reason string) string {
	return fmt.Sprintf("[image not attached: %s -- %s]", source, reason)
}

// oversizeReason phrases the size rejection with both numbers, so the model can
// tell "slightly over" from "absurd" and pick between resizing and giving up.
func oversizeReason(size int64) string {
	return fmt.Sprintf("%s exceeds the %s per-image attachment limit; resize it or use image_describe instead",
		humanBytes(int(size)), humanBytes(maxImageAttachBytes))
}

// imageFmtFromContentType maps a response Content-Type onto one of the Tier G
// image formats, reporting false for anything else.
//
// The allowlist is deliberate rather than a "default to jpeg" fallback: an
// image/* type we do not recognise (image/svg+xml, image/tiff, image/heic) is
// NOT decodable by the providers as any of these four, and mislabelling it would
// produce a provider error in the middle of a turn instead of a clean hint the
// model can act on.
func imageFmtFromContentType(contentType string) (string, bool) {
	mime := strings.ToLower(strings.TrimSpace(contentType))
	if i := strings.IndexByte(mime, ';'); i >= 0 { // drop "; charset=..."
		mime = strings.TrimSpace(mime[:i])
	}
	switch mime {
	case "image/png":
		return "png", true
	case "image/jpeg", "image/jpg":
		return "jpeg", true
	case "image/gif":
		return "gif", true
	case "image/webp":
		return "webp", true
	}
	return "", false
}
