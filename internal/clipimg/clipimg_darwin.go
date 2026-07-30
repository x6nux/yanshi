//go:build darwin

package clipimg

import (
	"context"
	"os/exec"
)

type platformReader struct{}

// ReadImage uses pngpaste (if available) to read the clipboard image. Falls
// back to osascript on macOS. No image in clipboard → empty → ok=false.
func (platformReader) ReadImage(ctx context.Context) ([]byte, string, bool) {
	if p, err := exec.LookPath("pngpaste"); err == nil {
		out, err := commandOutput(ctx, p, "-")
		if err == nil && len(out) > 0 {
			return out, "png", true
		}
	}
	return nil, "", false
}
