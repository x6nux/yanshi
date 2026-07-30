// task_gate_run: run a command, capture evidence, record into WorkTask.
//
// 这是 durable task 工具族里唯一会执行外部命令的工具。它的设计约束：
//   - cwd 必须先经 withinRootAbs（pathjail canonical kernel）jail 到 work root 内；
//   - 命令经 Authorize（带 Shell + FS read 维度）—— guard 的 metachar 规则会
//     拒绝 `|`、`>`、`$()` 等，所以 gate 只能跑单一 argv；
//   - 命令失败是结构化 Evidence{classification:"fail"}，不是 Go error；
//   - 启动/持久化失败才是 Go error（与已决策约束 §9 一致）；
//   - 输出超过 SpillThreshold 时落到 artifact（WriteArtifact），否则 inline summary；
//   - RecordGate 错误必须向上传播，禁止 `_ =`。
package tools

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cloudwego/eino/schema"
	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/task/work"
)

// GateTools 暴露 task_gate_run 工具。
type GateTools struct {
	Run *GuardedTool
}

// NewGateTools 构造 task_gate_run。默认 120s timeout；可在 args 里覆盖。
func NewGateTools() *GateTools {
	gt := &GateTools{}
	gt.Run = NewGuardedTool(
		"task_gate_run", "Task Gate Run",
		"Run a single command (no pipe/redirect), classify exit code, record Evidence.",
		120*time.Second,
		params(map[string]*schema.ParameterInfo{
			"task_id": {Type: schema.String, Desc: "task id", Required: true},
			"gate":    {Type: schema.String, Desc: "gate name (e.g. test/lint/build)", Required: true},
			"command": {Type: schema.String, Desc: "single command (no pipe/redirect)", Required: true},
			"cwd":     {Type: schema.String, Desc: "working directory (defaults to work root)"},
			"timeout": {Type: schema.Integer, Desc: "timeout in seconds (default 120)"},
			"env":     {Type: schema.String, Desc: "shell env: cmd / sh / powershell / bash / zsh"},
		}),
		SyncStream(gt.runGate),
	)
	return gt
}

// Tools returns the gate tool slice for convenience.
func (g *GateTools) Tools() []*GuardedTool { return []*GuardedTool{g.Run} }

type gateArgs struct {
	TaskID  string `json:"task_id"`
	Gate    string `json:"gate"`
	Command string `json:"command"`
	Cwd     string `json:"cwd"`
	Timeout int    `json:"timeout"`
	Env     string `json:"env"`
}

// runGate 是 task_gate_run 的 kernel。
func (g *GateTools) runGate(ctx context.Context, argsJSON string) (string, error) {
	var args gateArgs
	if err := ParseArgs(argsJSON, &args); err != nil {
		return "", err
	}
	root := WorkRootFromContext(ctx)
	if root == "" {
		root = "."
	}
	cwd := args.Cwd
	if cwd == "" {
		cwd = root
	} else if !filepath.IsAbs(cwd) {
		cwd = filepath.Join(root, cwd)
	}
	// withinRootAbs 委托 pathjail canonical kernel：EvalSymlinks + volume + case。
	cwdResolved, err := withinRootAbs(root, cwd)
	if err != nil {
		return "", err
	}
	// Authorize：把 shell 命令与 cwd 读路径一起交给 guard。
	// metachar（|、>、$()、换行...）由 guard 的 shell 规则拒绝，确保 gate 只接受
	// 单一 argv；force-prompt 工具的检查也在这里发生（task_gate_run 不在名单里）。
	if err := Authorize(ctx, guard.Action{
		Tool:  "task_gate_run",
		Shell: args.Command,
		FS:    guard.FSWant{Op: "read", Paths: []string{cwdResolved}},
	}, argsJSON); err != nil {
		return "", err
	}
	manager, err := requireTaskManager(ctx)
	if err != nil {
		return "", err
	}
	timeout := 120 * time.Second
	if args.Timeout > 0 {
		timeout = time.Duration(args.Timeout) * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := shellCommand(runCtx, args.Env, args.Command)
	command.Dir = cwdResolved
	started := time.Now()
	output, runErr := command.CombinedOutput()
	exitCode := 0
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
	}
	evidence := work.Evidence{
		ID:             work.NewID("ev"),
		Gate:           args.Gate,
		Command:        args.Command,
		Cwd:            cwdResolved,
		ExitCode:       exitCode,
		DurationMs:     time.Since(started).Milliseconds(),
		Classification: work.ClassificationFromExitCode(exitCode),
		RecordedAt:     time.Now().Unix(),
	}
	if len(output) > SpillThreshold {
		artifact, err := manager.WriteArtifact(ctx, args.TaskID, "gate-log", output, root)
		if err != nil {
			return "", err
		}
		evidence.LogArtifactID = artifact.ID
		evidence.Summary = artifact.Summary
	} else {
		evidence.Summary = summarizeGateOutput(output)
	}
	// 与已决策约束 §9 一致：RecordGate 失败必须返回，禁止 `_ =`。
	if err := manager.RecordGate(ctx, args.TaskID, evidence); err != nil {
		return "", err
	}
	task, err := manager.Read(ctx, args.TaskID)
	if err != nil {
		return "", err
	}
	EmitWorkEvent(ctx, work.Event{Kind: work.EventTaskUpdate, Task: task, TaskID: task.ID})
	return toJSON(struct {
		Evidence work.Evidence `json:"evidence"`
	}{Evidence: evidence}), nil
}

// summarizeGateOutput 把命令输出折叠成短 summary：取尾部 ~512 字节内的完整行 trim，
// 空输出 → "(no output)"，非空截到 160 rune。与 summarizeArtifact 不同：
// gate 关心命令末尾输出（错误通常在尾部）。
func summarizeGateOutput(output []byte) string {
	if len(output) == 0 {
		return "(no output)"
	}
	// 取尾部 ~512 字节；先用 bytes 区段定位最后一个 \n 之前的部分，确保不截半行。
	const tailWindow = 512
	start := len(output) - tailWindow
	if start < 0 {
		start = 0
	}
	tail := output[start:]
	// 找到第一个 \n 之后的完整行（避免半行）
	if i := bytesIndex(tail, '\n'); i >= 0 && start+i+1 < len(output) {
		tail = output[start+i+1:]
	}
	trimmed := strings.TrimSpace(string(tail))
	if trimmed == "" {
		return "(no output)"
	}
	if utf8.RuneCountInString(trimmed) <= 160 {
		return trimmed
	}
	rs := []rune(trimmed)
	return string(rs[:160])
}

// bytesIndex 是 bytes.IndexByte 的本地别名，避免在本文件再 import bytes。
func bytesIndex(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}
