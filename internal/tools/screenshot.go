package tools

import (
	"context"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/x6nux/yanshi/internal/imagestore"
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
