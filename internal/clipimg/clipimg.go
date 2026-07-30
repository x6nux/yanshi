// Package clipimg reads an image from the OS clipboard. It is the Tier G entry-A
// backend: the TUI binds Ctrl+V, calls Read, and only when ok=true treats the
// payload as an image attachment (otherwise the keystroke falls through to text
// paste). The default Reader uses a subprocess per platform (CGO_ENABLED=0
// compatible); a cgo-backed implementation may be added behind a build tag later.
package clipimg

import (
	"context"
	"os/exec"
)

// commandOutput runs a named subprocess and captures its stdout. It is a
// package-level seam so tests can inject a fake runner without touching the
// real OS clipboard; the default delegates to exec.CommandContext(...).Output()
// exactly as each platform implementation always did. Swapping it is
// behavior-preserving for every production caller.
var commandOutput = func(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

// Reader reads one image from the clipboard.
type Reader struct {
	backend backend
}

// backend is the platform seam. Returns (bytes, fmt, ok); ok=false means the
// clipboard holds no image (text or empty), which must NOT be an error.
type backend interface {
	ReadImage(ctx context.Context) ([]byte, string, bool)
}

// New returns the default platform Reader.
func New() *Reader { return &Reader{backend: platformReader{}} }

// NewWithBackend is the test seam.
func NewWithBackend(b backend) *Reader { return &Reader{backend: b} }

// Read returns the clipboard image, or ok=false when none is present.
func (r *Reader) Read(ctx context.Context) ([]byte, string, bool) {
	return r.backend.ReadImage(ctx)
}
