package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/x6nux/yanshi/internal/guard"
)

func TestGuardedTool_AllowsAndRuns(t *testing.T) {
	prof := guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"echo*"}}}
	ctx := WithProfile(context.Background(), prof)
	gt := NewGuardedTool(
		"echo", "Echo", "echoes input", 10*time.Second,
		schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"msg": {Type: schema.String, Required: true, Desc: "message"},
		}),
		SyncStream(func(ctx context.Context, args string) (string, error) { return "echo:" + args, nil }),
	)
	info, err := gt.Info(ctx)
	require.NoError(t, err)
	assert.Equal(t, "echo", info.Name)
	out, err := gt.InvokableRun(ctx, `{"msg":"hi"}`)
	require.NoError(t, err)
	assert.Contains(t, out, "hi")
}

func TestGuardedTool_DeniesOutOfProfile(t *testing.T) {
	prof := guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"other.*"}}}
	ctx := WithProfile(context.Background(), prof)
	gt := NewGuardedTool("echo", "Echo", "d", 10*time.Second, nil, SyncStream(func(ctx context.Context, args string) (string, error) {
		return "ran", nil
	}))
	out, err := gt.InvokableRun(ctx, "{}")
	require.NoError(t, err, "permission denial must surface as a result, not a Go error")
	assert.Contains(t, out, "permission denied")
}

func TestGuardedTool_SpillsOversizedResult(t *testing.T) {
	root := t.TempDir()
	big := NewGuardedTool("big", "Big", "returns a lot", 10*time.Second, nil,
		SyncStream(func(ctx context.Context, argsJSON string) (string, error) {
			return strings.Repeat("z", SpillThreshold+1), nil
		}))
	ctx := WithWorkRoot(WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
	}), root)

	out, err := big.InvokableRun(ctx, `{}`)
	require.NoError(t, err)
	assert.Contains(t, out, "[spilled:")
	assert.Contains(t, out, ".yanshi/tmp/spillover/big-")

	entries, err := os.ReadDir(filepath.Join(root, spillDir))
	require.NoError(t, err)
	require.Len(t, entries, 1, "oversized result must spill to one temp file")
}

func TestGuardedTool_SmallResultUnchanged(t *testing.T) {
	small := NewGuardedTool("small", "Small", "returns a little", 10*time.Second, nil,
		SyncStream(func(ctx context.Context, argsJSON string) (string, error) {
			return "ok", nil
		}))
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
	})
	out, err := small.InvokableRun(ctx, `{}`)
	require.NoError(t, err)
	assert.Equal(t, "ok", out, "small result must pass through unchanged")
}

// TestSpillRoundTrip_FsReadReadsSpilledFile proves the core design contract: a
// GuardedTool's oversized result spills to a temp file under the work root, and
// that temp file is readable by fs_read (so the model can page through it). This
// stitches together the spill layer, the WorkRoot ctx, and fs_read end-to-end.
func TestSpillRoundTrip_FsReadReadsSpilledFile(t *testing.T) {
	root := t.TempDir()

	// Oversized payload: a unique marker on line 1, then enough filler to exceed
	// SpillThreshold. The marker at the start lets fs_read read it back via a
	// tiny window (offset=1,end=1) without tripping fs_read's own >SpillThreshold
	// self-guard.
	const marker = "ROUNDTRIP_MARKER_7"
	filler := strings.Repeat("z", 700) + "\n" // ~701 B/line
	var b strings.Builder
	b.WriteString(marker + "\n")
	for b.Len() <= SpillThreshold {
		b.WriteString(filler)
	}
	payload := b.String()
	require.Greater(t, len(payload), SpillThreshold)

	big := NewGuardedTool("big", "Big", "returns a lot", 10*time.Second, nil,
		SyncStream(func(ctx context.Context, argsJSON string) (string, error) {
			return payload, nil
		}))
	ctx := WithWorkRoot(WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
		FS:    guard.FSPerm{Read: []string{root, root + "/**"}},
	}), root)

	// 1) The oversized result spills; the preview surfaces a relative temp path.
	preview, err := big.InvokableRun(ctx, `{}`)
	require.NoError(t, err)
	assert.Contains(t, preview, "[spilled:")

	// 2) Extract the relative path from the preview header:
	//    "[spilled: N lines / SIZE → <relpath>]\n..."
	idx := strings.Index(preview, "→ ")
	require.NotEqual(t, -1, idx, "preview must contain the spill path arrow")
	rest := preview[idx+len("→ "):]
	endIdx := strings.IndexByte(rest, ']')
	require.NotEqual(t, -1, endIdx, "preview header must close with ]")
	relPath := rest[:endIdx]
	assert.Contains(t, relPath, ".yanshi/tmp/spillover/big-", "path is the spill file under the work root")

	// 3) fs_read reads the spilled file back via a narrow window — round trip.
	fs := NewFSTools(root)
	out, err := runTool(ctx, fs.Read, fmt.Sprintf(`{"path":%q,"offset":1,"end":1}`, relPath))
	require.NoError(t, err)
	assert.Contains(t, out, marker, "fs_read must read back the spilled content (round trip)")
	assert.True(t, strings.HasPrefix(out, "1\t"+marker), "line 1 is the marker, got %q", out)
}
