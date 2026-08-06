package http

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/tools"
)

// TestChatWS_ModeYOLO_NoPrompt proves set_mode("yolo") makes the permission
// callback auto-approve a static-denied tool call WITHOUT emitting a
// permission_request frame: the tool runs and the file is written.
func TestChatWS_ModeYOLO_NoPrompt(t *testing.T) {
	url, workdir := newPermWSServer(t)
	c := dial(t, url)
	defer c.Close()

	// Enter yolo mode first.
	require.NoError(t, c.WriteJSON(proto.NewSetMode("yolo", 0)))
	drainUntil(t, c, "status")

	require.NoError(t, c.WriteJSON(proto.NewUserMessage("write the file")))

	var sawPrompt, sawResult bool
	for {
		f := readFrame(t, c)
		if f.Type == "permission_request" {
			sawPrompt = true
		}
		if f.Type == "tool_result" {
			sawResult = true
		}
		if f.Type == "done" {
			break
		}
	}
	assert.False(t, sawPrompt, "yolo mode must NOT prompt — it auto-approves")
	assert.True(t, sawResult, "the tool must still run under yolo")
	assert.FileExists(t, filepath.Join(workdir, "out.txt"),
		"yolo must let fs_write run and create the file")
}

// TestChatWS_ModeAllowEdits_NoPromptForEditTool proves set_mode("allow-edits")
// auto-approves an edit tool (fs_write) without prompting.
func TestChatWS_ModeAllowEdits_NoPromptForEditTool(t *testing.T) {
	url, workdir := newPermWSServer(t)
	c := dial(t, url)
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewSetMode("allow-edits", 0)))
	drainUntil(t, c, "status")

	require.NoError(t, c.WriteJSON(proto.NewUserMessage("write the file")))

	var sawPrompt bool
	for {
		f := readFrame(t, c)
		if f.Type == "permission_request" {
			sawPrompt = true
		}
		if f.Type == "done" {
			break
		}
	}
	assert.False(t, sawPrompt, "allow-edits mode must NOT prompt for an edit tool")
	assert.FileExists(t, filepath.Join(workdir, "out.txt"),
		"allow-edits must let fs_write run")
}

// TestChatWS_StatusEchoesMode proves a status frame carries the permission mode
// back to the client after set_mode, so the TUI footer can mirror it.
func TestChatWS_StatusEchoesMode(t *testing.T) {
	url, _ := newPermWSServer(t)
	c := dial(t, url)
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewSetMode("auto", 6)))
	st := drainUntil(t, c, "status")
	assert.Equal(t, "auto", st.PermMode)
	assert.Equal(t, 6, st.AutoThreshold)
}

// TestResolvePermissionMode_ProfileHardDeny proves an overridable profile-policy
// deny (shell policy="deny", net.allow=false, empty allowlist) is resolved
// server-side for every mode EXCEPT auto: YOLO overrides to allow;
// default/allow-edits/plan deny SILENTLY (so policy="deny" never prompts). Auto
// no longer bypasses — it delegates to the AI risk assessment (here nil-model,
// so it returns not-resolved, i.e. it would prompt).
func TestResolvePermissionMode_ProfileHardDeny(t *testing.T) {
	cases := []struct {
		mode     guard.PermissionMode
		wantTool tools.PermissionDecision
		resolved bool
	}{
		{guard.ModeYOLO, tools.PermissionAllow, true},
		{guard.ModeAuto, tools.PermissionDeny, false}, // AI-judges (nil model -> prompt)
		{guard.ModeDefault, tools.PermissionDeny, true},
		{guard.ModeAllowEdits, tools.PermissionDeny, true},
		{guard.ModePlan, tools.PermissionDeny, true},
	}
	for _, c := range cases {
		t.Run(string(c.mode), func(t *testing.T) {
			cs := &connSession{perm: &permModeState{}}
			cs.perm.set(c.mode, 0)
			req := tools.PermissionRequest{Tool: "shell_run", ProfileHardDeny: true}
			d, resolved := resolvePermissionMode(context.Background(), cs, nil, req)
			assert.Equal(t, c.wantTool, d, "mode %s", c.mode)
			assert.Equal(t, c.resolved, resolved, "mode %s", c.mode)
		})
	}
}

// TestResolvePermissionMode_DestructiveGate proves the profile-independent
// destructive-deletion gate: catastrophic mass deletion is blocked in EVERY
// mode (fail-safe; guard blocks it structurally first), out-of-workdir deletion
// is blocked under YOLO, and in-workdir deletion is allowed under YOLO. Auto
// delegates out-of-scope to its AI assessment (nil model here -> prompt).
func TestResolvePermissionMode_DestructiveGate(t *testing.T) {
	cases := []struct {
		name     string
		mode     guard.PermissionMode
		shell    string
		want     tools.PermissionDecision
		resolved bool
	}{
		{"catastrophic/yolo", guard.ModeYOLO, "rm -rf /", tools.PermissionDeny, true},
		{"catastrophic/auto", guard.ModeAuto, "rm -rf /", tools.PermissionDeny, true},
		{"catastrophic/default", guard.ModeDefault, "rm -rf /", tools.PermissionDeny, true},
		{"out-of-scope/yolo", guard.ModeYOLO, "rm /etc/passwd", tools.PermissionDeny, true},
		{"out-of-scope/auto", guard.ModeAuto, "rm /etc/passwd", tools.PermissionDeny, false},
		{"out-of-scope/default", guard.ModeDefault, "rm /etc/passwd", tools.PermissionDeny, false},
		{"in-scope/yolo", guard.ModeYOLO, "rm -rf build", tools.PermissionAllow, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cs := &connSession{perm: &permModeState{}}
			cs.perm.set(c.mode, 0)
			req := tools.PermissionRequest{Tool: "shell_run", Shell: c.shell, Workdir: "/proj"}
			d, resolved := resolvePermissionMode(context.Background(), cs, nil, req)
			assert.Equal(t, c.want, d)
			assert.Equal(t, c.resolved, resolved)
		})
	}
}

// TestChatWS_ModePlan_ProducesReadOnlyTurn proves set_mode("plan") makes the
// NEXT turn genuinely read-only, not merely differently-worded: the WS handler
// must carry the live mode into orchestrator.TurnOpts.PlanMode, which is the
// single entry point for the whole plan implementation (tools.WithPlanMode,
// runnerFor's plan-keyed runner, and filterPlanTools).
//
// The observable that separates "plan wired" from "plan is just a label": with
// PlanMode set, filterPlanTools drops fs_write from the registered tool set, so
// the scripted model's call reaches UnknownToolsHandler instead of the tool —
// and an unknown tool produces NO tool_result frame at all (only registered
// tools emit a ToolOutput event the WS loop translates). Without the field the
// tool stays registered, runs, and is merely denied at the permission callback,
// which emits a "permission denied" tool_result. Asserting on the tool_result
// therefore pins the tool-set filtering, not just the absent file: the
// permission callback is a SECOND line of defense that already denies writes in
// plan mode and would otherwise mask the missing TurnOpts field entirely.
//
// ledger: A2/G05#1 plan 模式禁编辑类工具
func TestChatWS_ModePlan_ProducesReadOnlyTurn(t *testing.T) {
	url, workdir := newPermWSServer(t)
	c := dial(t, url)
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewSetMode("plan", 0)))
	drainUntil(t, c, "status")

	require.NoError(t, c.WriteJSON(proto.NewUserMessage("write the file")))

	var sawCall, sawErr bool
	var results []string
	for {
		f := readFrame(t, c)
		switch f.Type {
		case "tool_call":
			sawCall = sawCall || f.ToolName == "fs_write"
		case "tool_result":
			results = append(results, f.ToolName+": "+f.Text)
		case "error":
			sawErr = true
		}
		if f.Type == "done" {
			break
		}
	}

	// The model still emits the call — so an absent result means the tool was
	// filtered out of the plan runner, not that the turn died early.
	assert.True(t, sawCall, "the scripted model must still attempt fs_write")
	assert.False(t, sawErr, "a plan turn must complete normally, not error out")
	for _, r := range results {
		assert.NotContains(t, r, "permission denied",
			"fs_write reached the permission layer, so it was still registered: "+
				"TurnOpts.PlanMode was not set and the turn is not read-only")
	}
	assert.Empty(t, results,
		"plan mode must run the plan runner: fs_write is filtered out of the tool set, "+
			"so it produces no tool result at all; got %v", results)
	assert.NoFileExists(t, filepath.Join(workdir, "out.txt"),
		"plan mode is read-only: fs_write must never reach the disk")
}
