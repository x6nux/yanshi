package tools

import (
	"context"
	"os/exec"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/x6nux/yanshi/internal/imagestore"
	"github.com/x6nux/yanshi/internal/netpolicy"
)

// CaptureFunc captures the primary screen and returns (bytes, fmt, err).
// Implementations are platform-specific (build-tagged); tests inject a fake.
type CaptureFunc func(ctx context.Context) ([]byte, string, error)

// screenshotTool holds state for the screenshot capture.
type screenshotTool struct {
	store   *imagestore.Store
	capture CaptureFunc
}

// NewScreenshotTool builds the production screenshot tool with the platform
// capture adapter for the current OS.
func NewScreenshotTool(store *imagestore.Store) Tool {
	return newScreenshotTool(store, platformCapture)
}

// NewScreenshotToolWithCapture is the test seam.
func NewScreenshotToolWithCapture(store *imagestore.Store, capture CaptureFunc) Tool {
	return newScreenshotTool(store, capture)
}

func newScreenshotTool(store *imagestore.Store, capture CaptureFunc) Tool {
	s := &screenshotTool{store: store, capture: capture}
	return NewApprovalGuardedTool(
		"screenshot", "Screenshot", "Capture the primary screen and return an image reference (approval required).",
		15*time.Second,
		params(map[string]*schema.ParameterInfo{}),
		SyncStream(s.run),
	)
}

func (s *screenshotTool) run(ctx context.Context, _ string) (string, error) {
	bytes, fmtName, err := s.capture(ctx)
	if err != nil {
		return errorResult("截图失败：" + err.Error()), nil
	}
	id, err := s.store.Put(bytes, "screenshot", fmtName)
	if err != nil {
		return errorResult(err.Error()), nil
	}
	return s.store.Placeholder(id), nil
}

// captureCommand builds the exec.Cmd a platform screenshot backend runs.
//
// It exists so the credential scrub is applied once rather than in each of the
// three platform files, where the fourth one to be written would inevitably
// omit it. The screenshot backends are PATH-resolved third-party binaries
// (screencapture, grim, gnome-screenshot, powershell) started on behalf of a
// model tool call; before this they inherited yanshi's whole environment,
// including every provider API key the operator exported.
func captureCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = netpolicy.ScrubbedEnviron()
	return cmd
}
