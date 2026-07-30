//go:build darwin

package tools

import (
	"context"
	"os/exec"
)

func platformCapture(ctx context.Context) ([]byte, string, error) {
	out, err := exec.CommandContext(ctx, "screencapture", "-x", "-").Output()
	if err != nil {
		return nil, "", err
	}
	return out, "png", nil
}
