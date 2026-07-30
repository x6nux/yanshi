package tools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/x6nux/yanshi/internal/guard"
)

func TestTime_Now(t *testing.T) {
	tt := NewTimeTools()
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"time_*"}},
	})
	out, err := runTool(ctx, tt.Now, `{}`)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &m))
	assert.NotEmpty(t, m["iso8601"])
	assert.NotEmpty(t, m["unix"])
	assert.Contains(t, m["iso8601"], "T") // RFC3339 has a T separator
}

func TestTime_Now_OffsetSeconds(t *testing.T) {
	tt := NewTimeTools()
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"time_*"}},
	})
	out, err := runTool(ctx, tt.Now, `{}`)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &m))
	off, ok := m["offset_seconds"]
	require.True(t, ok, "offset_seconds must be present")
	_, isNum := off.(float64)
	assert.True(t, isNum, "offset_seconds must be a number")
}

func TestTime_Now_DeniedNoProfile(t *testing.T) {
	tt := NewTimeTools()
	out, err := runTool(context.Background(), tt.Now, `{}`)
	require.NoError(t, err, "permission denial must surface as a result, not a Go error")
	assert.Contains(t, out, "✗")
	assert.Contains(t, out, "permission denied")
}
