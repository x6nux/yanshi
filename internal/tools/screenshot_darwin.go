//go:build darwin

package tools

import "context"

func platformCapture(ctx context.Context) ([]byte, string, error) {
	out, err := captureCommand(ctx, "screencapture", "-x", "-").Output()
	if err != nil {
		return nil, "", err
	}
	return out, "png", nil
}
