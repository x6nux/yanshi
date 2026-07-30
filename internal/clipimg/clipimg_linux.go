//go:build linux

package clipimg

import (
	"context"
	"os/exec"
)

type platformReader struct{}

// ReadImage tries wl-paste (Wayland) first, then xclip (X11). Neither present
// → ok=false. Only returns png format (most DEs support image/png clipboard).
func (platformReader) ReadImage(ctx context.Context) ([]byte, string, bool) {
	if p, err := exec.LookPath("wl-paste"); err == nil {
		out, err := commandOutput(ctx, p, "-t", "image/png")
		if err == nil && len(out) > 0 {
			return out, "png", true
		}
	}
	if p, err := exec.LookPath("xclip"); err == nil {
		out, err := commandOutput(ctx, p, "-selection", "clipboard", "-t", "image/png", "-o")
		if err == nil && len(out) > 0 {
			return out, "png", true
		}
	}
	return nil, "", false
}
