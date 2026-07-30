package tools

import (
	"context"
	"time"

	"github.com/cloudwego/eino/schema"
)

// TimeTools exposes time_now.
type TimeTools struct {
	Now *GuardedTool
}

// NewTimeTools builds time tools.
func NewTimeTools() *TimeTools {
	t := &TimeTools{}
	t.Now = NewGuardedTool(
		"time_now", "Time", "Return the current time (ISO 8601 + unix epoch + UTC offset seconds).",
		5*time.Second,
		params(map[string]*schema.ParameterInfo{}),
		SyncStream(t.runNow),
	)
	return t
}

func (t *TimeTools) runNow(_ context.Context, _ string) (string, error) {
	now := time.Now()
	_, off := now.Zone()
	return toJSON(map[string]any{
		"iso8601":        now.Format(time.RFC3339),
		"unix":           now.Unix(),
		"offset_seconds": off,
	}), nil
}
