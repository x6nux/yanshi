package orchestrator

import (
	"context"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/task/work"
)

// ---------------------------------------------------------------------------
// workevents.go: workEventFrame
// ---------------------------------------------------------------------------

func TestWorkEventFrame_PlanUpdate(t *testing.T) {
	f := workEventFrame(work.Event{
		Kind:      work.EventPlanUpdate,
		TaskID:    "t1",
		Checklist: work.Checklist{Items: nil},
	})
	// PlanUpdate must produce an event with non-nil Checklist.
	assert.NotNil(t, f.Checklist)
	assert.Empty(t, f.Checklist.Items)
}

func TestWorkEventFrame_ChecklistUpdate(t *testing.T) {
	f := workEventFrame(work.Event{
		Kind:   work.EventChecklistUpdate,
		TaskID: "t1",
		Checklist: work.Checklist{Items: []work.ChecklistItem{
			{ID: 1, Content: "step1"},
		}},
	})
	require.NotNil(t, f.Checklist)
	assert.Equal(t, 1, len(f.Checklist.Items))
}

func TestWorkEventFrame_DefaultIsTaskUpdate(t *testing.T) {
	f := workEventFrame(work.Event{
		Kind: work.EventTaskUpdate,
		Task: &work.WorkTask{ID: "t1"},
	})
	// Default case → NewTaskUpdate with a non-nil task → Type "task_update".
	assert.Equal(t, "task_update", f.Type)
}

// ---------------------------------------------------------------------------
// multimodal.go: mimeForImage, firstNonEmptyStr
// ---------------------------------------------------------------------------

func TestMimeForImage(t *testing.T) {
	assert.Equal(t, "image/png", mimeForImage("png"))
	assert.Equal(t, "image/png", mimeForImage("PNG"))
	assert.Equal(t, "image/gif", mimeForImage("gif"))
	assert.Equal(t, "image/webp", mimeForImage("webp"))
	assert.Equal(t, "image/jpeg", mimeForImage("jpg"))
	assert.Equal(t, "image/jpeg", mimeForImage(""))
}

func TestFirstNonEmptyStr(t *testing.T) {
	assert.Equal(t, "a", firstNonEmptyStr("a", "b"))
	assert.Equal(t, "b", firstNonEmptyStr("", "b"))
	assert.Equal(t, "", firstNonEmptyStr("", ""))
}

// ---------------------------------------------------------------------------
// completion.go: parseJudgeVerdict, JudgeRetryNudge, WithTurnRecorder
// ---------------------------------------------------------------------------

func TestParseJudgeVerdict_PlainJSON(t *testing.T) {
	v, err := parseJudgeVerdict(`{"complete":true,"reason":"done"}`)
	require.NoError(t, err)
	assert.True(t, v.Complete)
	assert.Equal(t, "done", v.Reason)
}

func TestParseJudgeVerdict_MarkdownFence(t *testing.T) {
	v, err := parseJudgeVerdict("```json\n{\"complete\":false,\"reason\":\"needs more\"}\n```")
	require.NoError(t, err)
	assert.False(t, v.Complete)
	assert.Equal(t, "needs more", v.Reason)
}

func TestParseJudgeVerdict_ProseWrapped(t *testing.T) {
	v, err := parseJudgeVerdict("Here is my verdict: {\"complete\":true,\"reason\":\"looks good\"}")
	require.NoError(t, err)
	assert.True(t, v.Complete)
}

func TestParseJudgeVerdict_Invalid(t *testing.T) {
	_, err := parseJudgeVerdict("no JSON here")
	require.Error(t, err)
}

func TestJudgeRetryNudge_Empty(t *testing.T) {
	nudge := JudgeRetryNudge("")
	assert.Equal(t, "Continue and finish addressing the user's request.", nudge)
}

func TestJudgeRetryNudge_WithReason(t *testing.T) {
	nudge := JudgeRetryNudge("test coverage missing")
	assert.Contains(t, nudge, "test coverage missing")
}

func TestWithTurnRecorder_Nil(t *testing.T) {
	ctx := context.Background()
	got := WithTurnRecorder(ctx, nil)
	assert.Equal(t, ctx, got, "nil recorder should return ctx unchanged")
}

// ---------------------------------------------------------------------------
// orchestrator.go: Profile / ProfileForTest
// ---------------------------------------------------------------------------

func TestProfile(t *testing.T) {
	p := guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
	}
	o := &Orchestrator{profile: p}
	assert.Equal(t, p, o.Profile())
	assert.Equal(t, p, o.ProfileForTest())
}

// ---------------------------------------------------------------------------
// env.go: hostOS (platform-agnostic), detectShell, shellOptions
// ---------------------------------------------------------------------------

func TestHostOS_ReturnsPlatform(t *testing.T) {
	os := hostOS()
	// Must be non-empty and start with a known OS prefix.
	assert.NotEmpty(t, os)
	switch runtime.GOOS {
	case "windows":
		assert.Contains(t, os, "Windows")
	case "darwin":
		assert.Contains(t, os, "macOS")
	case "linux":
		assert.Contains(t, os, "Linux")
	}
}

func TestDetectShell_Windows(t *testing.T) {
	if runtime.GOOS == "windows" {
		sh := detectShell()
		// Should return non-empty on Windows.
		assert.NotEmpty(t, sh)
	}
}

func TestShellOptions(t *testing.T) {
	opts := shellOptions()
	assert.NotEmpty(t, opts)
	if runtime.GOOS == "windows" {
		assert.Contains(t, opts, "cmd")
	} else {
		assert.Contains(t, opts, "sh")
	}
}

// ---------------------------------------------------------------------------
// classify.go: extractErrorResult
// ---------------------------------------------------------------------------

func TestExtractErrorResult_NotJSON(t *testing.T) {
	got := extractErrorResult("some result text")
	assert.Equal(t, "", got)
}

func TestExtractErrorResult_NoErrorField(t *testing.T) {
	got := extractErrorResult(`{"result":"ok"}`)
	assert.Equal(t, "", got)
}

func TestExtractErrorResult_WithErrorField(t *testing.T) {
	got := extractErrorResult(`{"error":"something broke"}`)
	assert.Equal(t, "something broke", got)
}

// env.go's local `probe` — a copy of execprobe.Run with the timeout removed —
// is gone, and with it TestProbe_Nonexistent. The behaviour it covered lives at
// the one remaining implementation: execprobe.TestRun_NonexistentCommandReturnsEmpty.
