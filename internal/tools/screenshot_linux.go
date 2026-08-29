//go:build linux

package tools

import (
	"context"
	"fmt"
	"os/exec"
)

func platformCapture(ctx context.Context) ([]byte, string, error) {
	if p, err := exec.LookPath("grim"); err == nil {
		out, err := captureCommand(ctx, p, "-").Output()
		if err == nil {
			return out, "png", nil
		}
	}
	if p, err := exec.LookPath("gnome-screenshot"); err == nil {
		out, err := captureCommand(ctx, p, "-f", "-").Output()
		if err == nil {
			return out, "png", nil
		}
	}
	return nil, "", fmt.Errorf("no supported screen capture tool (install grim or gnome-screenshot)")
}
