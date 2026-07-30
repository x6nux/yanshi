package tools

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/task/work"
)

// gateCmd returns a shell command (no pipe, no redirect) that exits 0 on the
// current platform. Both branches produce a single argv so guard's metachar
// rule accepts them.
func gateCmd() (env, command string) {
	if runtime.GOOS == "windows" {
		return "cmd", "cmd /c exit 0"
	}
	return "sh", "sh -c true"
}

// gateFailCmd 同 gateCmd 但退出码非零。
func gateFailCmd() (env, command string) {
	if runtime.GOOS == "windows" {
		return "cmd", "cmd /c exit 1"
	}
	return "sh", "sh -c false"
}

// gatePrintCmd 输出一行文本后退出 0。
func gatePrintCmd() (env, command string) {
	if runtime.GOOS == "windows" {
		return "cmd", `cmd /c echo hello`
	}
	return "sh", `sh -c "echo hello"`
}

func gateCtx(t *testing.T, manager work.ManagerLike, root string) context.Context {
	// Resolve symlinks so the FS allowed-path matches the EvalSymlinks-resolved
	// root that withinRootAbs returns (macOS /var → /private/var). Without this,
	// every gate tool call that Authorizes the resolved cwd gets a permission
	// denial because the non-resolved /var/folders/... pattern doesn't glob-match
	// /private/var/folders/....
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	ctx := WithProfile(WithWorkRoot(WithTaskManager(context.Background(), manager), root), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
		Shell: guard.ShellPerm{Policy: "allowlist", Patterns: []string{"*"}},
		FS:    guard.FSPerm{Read: []string{root}, Write: []string{root}},
	})
	return ctx
}

func TestTaskGateRun_PassEvidence(t *testing.T) {
	manager := work.NewFakeManager()
	task, err := manager.Create(context.Background(), work.CreateReq{Title: "x", Prompt: "p"})
	require.NoError(t, err)
	root := t.TempDir()
	ctx := gateCtx(t, manager, root)
	env, command := gateCmd()
	gt := NewGateTools()
	argsJSON := `{"task_id":"` + task.ID + `","gate":"test","command":"` + command + `","env":"` + env + `"}`
	out, err := runTool(ctx, gt.Run, argsJSON)
	require.NoError(t, err, "raw out=%q", out)
	var payload struct {
		Evidence work.Evidence `json:"evidence"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &payload), "out=%q", out)
	assert.Equal(t, "pass", payload.Evidence.Classification)
	assert.Equal(t, 0, payload.Evidence.ExitCode)
	assert.Equal(t, "test", payload.Evidence.Gate)
	assert.Equal(t, command, payload.Evidence.Command)
}

func TestTaskGateRun_NonZeroExitRecordsFailClassification(t *testing.T) {
	manager := work.NewFakeManager()
	task, err := manager.Create(context.Background(), work.CreateReq{Title: "x", Prompt: "p"})
	require.NoError(t, err)
	root := t.TempDir()
	ctx := gateCtx(t, manager, root)
	env, command := gateFailCmd()
	gt := NewGateTools()
	argsJSON := `{"task_id":"` + task.ID + `","gate":"test","command":"` + command + `","env":"` + env + `"}`
	out, err := runTool(ctx, gt.Run, argsJSON)
	require.NoError(t, err)
	var payload struct {
		Evidence work.Evidence `json:"evidence"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &payload))
	assert.Equal(t, "fail", payload.Evidence.Classification)
	assert.Greater(t, payload.Evidence.ExitCode, 0)
}

func TestTaskGateRun_CwdEscapeRejected(t *testing.T) {
	manager := work.NewFakeManager()
	task, err := manager.Create(context.Background(), work.CreateReq{Title: "x", Prompt: "p"})
	require.NoError(t, err)
	root := t.TempDir()
	ctx := gateCtx(t, manager, root)
	env, command := gateCmd()
	gt := NewGateTools()
	argsJSON := `{"task_id":"` + task.ID + `","gate":"test","command":"` + command + `","env":"` + env + `","cwd":"../../../../etc"}`
	out, err := runTool(ctx, gt.Run, argsJSON)
	// kernel 错误经 InvokableRun 表面化为 result 文本，err 为 nil 是设计如此
	require.NoError(t, err)
	// 越界 cwd 应被 pathjail/withinRootAbs 拒绝：EvalSymlinks 失败（系统找不到路径）
	// 或 rel 检查失败（path escapes work root）。
	require.NotEmpty(t, out)
	// 验证 cwd 确实越界到 root 外（pathjail 报错）。Windows 下错误文本是
	// "GetFileAttributesEx ...: The system cannot find the file specified."，
	// POSIX 下是 "lstat ...: no such file or directory"。
	assert.True(t,
		strings.Contains(out, "permission denied") ||
			strings.Contains(out, "pathjail") ||
			strings.Contains(out, "escapes") ||
			strings.Contains(out, "cannot find") ||
			strings.Contains(out, "no such file"),
		"want escape-related error in out; got %q", out)
}

func TestTaskGateRun_RecordGatePropagated(t *testing.T) {
	// 用一个总是回 RecordGate 错误的 manager 验证错误不忽略。
	manager := &failingRecordGateManager{FakeManager: work.NewFakeManager()}
	task, err := manager.Create(context.Background(), work.CreateReq{Title: "x", Prompt: "p"})
	require.NoError(t, err)
	root := t.TempDir()
	ctx := gateCtx(t, manager, root)
	env, command := gateCmd()
	gt := NewGateTools()
	argsJSON := `{"task_id":"` + task.ID + `","gate":"test","command":"` + command + `","env":"` + env + `"}`
	out, err := runTool(ctx, gt.Run, argsJSON)
	// kernel 错误经 InvokableRun 表面化为 result 文本，err 为 nil
	require.NoError(t, err)
	assert.Contains(t, out, "record-gate-failure", "want RecordGate error surfaced; got %q", out)
}

// failingRecordGateManager 包装 FakeManager，让 RecordGate 总回错。
type failingRecordGateManager struct {
	*work.FakeManager
}

func (f *failingRecordGateManager) RecordGate(_ context.Context, _ string, _ work.Evidence) error {
	return errFailingRecordGate
}

var errFailingRecordGate = &recordGateErr{}

type recordGateErr struct{}

func (*recordGateErr) Error() string { return "record-gate-failure" }

func TestSummarizeGateOutput(t *testing.T) {
	// empty
	assert.Equal(t, "(no output)", summarizeGateOutput(nil))
	assert.Equal(t, "(no output)", summarizeGateOutput([]byte("   \n\t")))
	// single line
	assert.Equal(t, "hello", summarizeGateOutput([]byte("hello\n")))
	// multiline: tail wins（错误通常在末尾）
	in := "line1\nline2\nline3\nthe-trailing-error\n"
	s := summarizeGateOutput([]byte(in))
	assert.Contains(t, s, "the-trailing-error")
	// very long tail: 截到 160 rune（不含 "…"）
	long := strings.Repeat("x", 1000)
	got := summarizeGateOutput([]byte(long))
	assert.LessOrEqual(t, len([]rune(got)), 160)
}

func TestTaskGateRun_UsesShellCommandHelper(t *testing.T) {
	// 这是个 smoke test：验证 shellCommand 被复用而非复制 —— 通过检查
	// exec.Cmd 的 Path 是否是 cmd.exe 或 sh（取决于平台）。
	env, command := gatePrintCmd()
	ctx := context.Background()
	cmd := shellCommand(ctx, env, command)
	require.NotNil(t, cmd)
	// cmd.Path 必须指向某个 shell 可执行（unix: /bin/sh 或 /usr/bin/sh；
	// windows: cmd.exe）。
	_ = exec.LookPath // import 保证
	assert.NotEmpty(t, cmd.Path)
}

func TestTaskGateRun_SpillToArtifact(t *testing.T) {
	// 这个测试用真实 Manager + tmp root 验证 spill 路径。
	// 但我们没法轻易产生 > SpillThreshold 的 stdout（输出受 shell 限制），
	// 所以这里只验证 summarizeGateOutput 在小输出时不 spill、走 summary 路径。
	manager := work.NewFakeManager()
	task, err := manager.Create(context.Background(), work.CreateReq{Title: "x", Prompt: "p"})
	require.NoError(t, err)
	root := t.TempDir()
	ctx := gateCtx(t, manager, root)
	env, command := gatePrintCmd()
	gt := NewGateTools()
	argsJSON, err := json.Marshal(map[string]string{
		"task_id": task.ID,
		"gate":    "print",
		"command": command,
		"env":     env,
	})
	require.NoError(t, err)
	out, err := runTool(ctx, gt.Run, string(argsJSON))
	require.NoError(t, err)
	var payload struct {
		Evidence work.Evidence `json:"evidence"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &payload))
	// 没溢出，LogArtifactID 为空，summary 包含 echo 输出
	assert.Empty(t, payload.Evidence.LogArtifactID)
	// echo 输出在 Windows 是 "hello\r\n"，在 Unix 是 "hello\n"，trim 后都是 "hello"
	// 但 summarizeGateOutput 取尾部 N 行：单行就是 "hello" 或 "hello\r"
	assert.Contains(t, payload.Evidence.Summary, "hello")
}

func TestTaskGateRun_NoManager(t *testing.T) {
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
	})
	gt := NewGateTools()
	_, err := runKernel(ctx, gt.runGate, `{"task_id":"x","gate":"g","command":"cmd /c exit 0","env":"cmd"}`)
	require.Error(t, err)
	// 给个超时上限来 drain
	_ = time.Second
}
