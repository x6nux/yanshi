package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/shell"
)

// shellStartArgs is the args shape for shell_start and task_shell_start. PTY
// is honored only when the platform's StartPTYProcess is implemented (Phase 0
// returns ErrPTYUnavailable for every platform).
type shellStartArgs struct {
	Command string `json:"command"`
	Workdir string `json:"workdir"`
	PTY     bool   `json:"pty"`
}

type shellIDArgs struct {
	ID string `json:"id"`
}

type shellWriteArgs struct {
	ID   string `json:"id"`
	Data string `json:"data"`
}

type shellReadArgs struct {
	ID       string `json:"id"`
	MaxBytes int    `json:"max_bytes"`
}

// ShellV2Tools is the v2 shell surface: a persistent-session tool family
// (Start/Read/Write/Wait/Cancel) plus a background-job family
// (TaskStart/TaskWait/TaskWrite/TaskCancel). Every tool Authorizes under its
// real name (e.g. shell_write_stdin, NOT a shell string) so the guard profile
// gates each action independently.
type ShellV2Tools struct {
	Start, Read, Write, Wait, Cancel           *GuardedTool
	TaskStart, TaskWait, TaskWrite, TaskCancel *GuardedTool

	// root is the work root: the destructive-deletion baseline handed to the
	// guard and the fallback launch directory for workdir-less calls.
	root string
}

// NewShellV2Tools builds the nine-tool shell v2 surface, anchored at the work
// root. Tools share nothing else at construction time; per-call state (session
// id, job id) is in args.
//
// root is load-bearing for safety, not cosmetics: it is the baseline the guard's
// destructive-deletion dimension classifies deletions against, and the directory
// a session runs in when the caller omits "workdir" (mirroring legacy shell_run).
func NewShellV2Tools(root string) *ShellV2Tools {
	v := &ShellV2Tools{root: root}
	v.Start = NewGuardedTool("shell_start", "Shell", "Start a persistent shell session.", 30*time.Second,
		params(map[string]*schema.ParameterInfo{
			"command": {Type: schema.String, Required: true},
			"workdir": {Type: schema.String},
			"pty":     {Type: schema.Boolean},
		}),
		SyncStream(v.start))
	v.Read = NewGuardedTool("shell_read", "Shell", "Read incremental output.", 30*time.Second,
		params(map[string]*schema.ParameterInfo{
			"id":        {Type: schema.String, Required: true},
			"max_bytes": {Type: schema.Integer},
		}),
		SyncStream(v.read))
	v.Write = NewGuardedTool("shell_write_stdin", "Shell", "Write stdin to a shell session.", 30*time.Second,
		params(map[string]*schema.ParameterInfo{
			"id":   {Type: schema.String, Required: true},
			"data": {Type: schema.String, Required: true},
		}),
		SyncStream(v.write))
	v.Wait = NewGuardedTool("shell_wait", "Shell", "Wait for session completion.", 130*time.Second,
		params(map[string]*schema.ParameterInfo{
			"id": {Type: schema.String, Required: true},
		}),
		SyncStream(v.wait))
	v.Cancel = NewGuardedTool("shell_cancel", "Shell", "Cancel a shell session.", 30*time.Second,
		params(map[string]*schema.ParameterInfo{
			"id": {Type: schema.String, Required: true},
		}),
		SyncStream(v.cancel))
	v.TaskStart = NewGuardedTool("task_shell_start", "Shell Job", "Start a background shell job.", 30*time.Second,
		params(map[string]*schema.ParameterInfo{
			"command": {Type: schema.String, Required: true},
			"workdir": {Type: schema.String},
		}),
		SyncStream(v.taskStart))
	v.TaskWait = NewGuardedTool("task_shell_wait", "Shell Job", "Read background job state.", 30*time.Second,
		params(map[string]*schema.ParameterInfo{
			"id":        {Type: schema.String, Required: true},
			"max_bytes": {Type: schema.Integer},
		}),
		SyncStream(v.taskWait))
	v.TaskWrite = NewGuardedTool("task_shell_stdin", "Shell Job", "Write stdin to a background job.", 30*time.Second,
		params(map[string]*schema.ParameterInfo{
			"id":   {Type: schema.String, Required: true},
			"data": {Type: schema.String, Required: true},
		}),
		SyncStream(v.taskWrite))
	v.TaskCancel = NewGuardedTool("task_shell_cancel", "Shell Job", "Cancel a background job.", 30*time.Second,
		params(map[string]*schema.ParameterInfo{
			"id": {Type: schema.String, Required: true},
		}),
		SyncStream(v.taskCancel))
	return v
}

// manager returns the *shell.Manager bound in ctx. Fails closed when no manager
// is bound — the v2 surface is inert without one.
func (v *ShellV2Tools) manager(ctx context.Context) (*shell.Manager, error) {
	m, ok := ShellManagerFromContext(ctx)
	if !ok {
		return nil, fmt.Errorf("shell: runtime unavailable")
	}
	return m, nil
}

// effectiveWorkdir resolves the directory a launch actually runs in: the
// caller's "workdir" arg when given, otherwise the work root (legacy shell_run
// parity — an omitted workdir must NOT inherit the server process's cwd).
//
// The same value is handed to guard.Action.Workdir, because the destructive
// deletion dimension classifies "inside vs outside the working directory"
// against it: with an empty Workdir the guard fails safe and treats EVERY
// absolute path as out-of-scope (prompting on each one) while losing the
// "deleting the work root itself" judgement entirely. Keeping the guard's
// baseline identical to the launch directory is what makes that classification
// mean what it says.
func (v *ShellV2Tools) effectiveWorkdir(argWorkdir string) string {
	if argWorkdir != "" {
		return argWorkdir
	}
	return v.root
}

func (v *ShellV2Tools) start(ctx context.Context, raw string) (string, error) {
	var a shellStartArgs
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		return "", err
	}
	wd := v.effectiveWorkdir(a.Workdir)
	if err := Authorize(ctx, guard.Action{Tool: "shell_start", Shell: a.Command, Workdir: wd}, raw); err != nil {
		return "", err
	}
	m, err := v.manager(ctx)
	if err != nil {
		return "", err
	}
	prog, args, err := shell.ShellArgv("", a.Command)
	if err != nil {
		return "", err
	}
	sess, err := m.Start(ctx, shell.LaunchSpec{
		ShellName: "",
		Command:   a.Command,
		Program:   prog,
		Args:      args,
		Dir:       wd,
		PTY:       a.PTY,
	})
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(sess)
	return string(data), err
}

func (v *ShellV2Tools) read(ctx context.Context, raw string) (string, error) {
	var a shellReadArgs
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		return "", err
	}
	if err := Authorize(ctx, guard.Action{Tool: "shell_read"}, raw); err != nil {
		return "", err
	}
	m, err := v.manager(ctx)
	if err != nil {
		return "", err
	}
	out, err := m.Read(a.ID, a.MaxBytes)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(`{"output":%q}`, out), nil
}

func (v *ShellV2Tools) write(ctx context.Context, raw string) (string, error) {
	var a shellWriteArgs
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		return "", err
	}
	if err := Authorize(ctx, guard.Action{Tool: "shell_write_stdin"}, raw); err != nil {
		return "", err
	}
	m, err := v.manager(ctx)
	if err != nil {
		return "", err
	}
	n, err := m.Write(a.ID, []byte(a.Data))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(`{"written":%d}`, n), nil
}

func (v *ShellV2Tools) wait(ctx context.Context, raw string) (string, error) {
	var a shellIDArgs
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		return "", err
	}
	if err := Authorize(ctx, guard.Action{Tool: "shell_wait"}, raw); err != nil {
		return "", err
	}
	m, err := v.manager(ctx)
	if err != nil {
		return "", err
	}
	// Pass ctx so the caller's deadline cancels Wait (Task 17 contract).
	sess, err := m.Wait(ctx, a.ID)
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(sess)
	return string(data), err
}

func (v *ShellV2Tools) cancel(ctx context.Context, raw string) (string, error) {
	var a shellIDArgs
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		return "", err
	}
	if err := Authorize(ctx, guard.Action{Tool: "shell_cancel"}, raw); err != nil {
		return "", err
	}
	m, err := v.manager(ctx)
	if err != nil {
		return "", err
	}
	if err := m.Cancel(a.ID); err != nil {
		return "", err
	}
	return `{"canceled":true}`, nil
}

func (v *ShellV2Tools) taskStart(ctx context.Context, raw string) (string, error) {
	var a shellStartArgs
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		return "", err
	}
	wd := v.effectiveWorkdir(a.Workdir)
	if err := Authorize(ctx, guard.Action{Tool: "task_shell_start", Shell: a.Command, Workdir: wd}, raw); err != nil {
		return "", err
	}
	m, err := v.manager(ctx)
	if err != nil {
		return "", err
	}
	prog, args, err := shell.ShellArgv("", a.Command)
	if err != nil {
		return "", err
	}
	job, err := m.StartJob(ctx, a.Command, shell.LaunchSpec{
		Command: a.Command,
		Program: prog,
		Args:    args,
		Dir:     wd,
	})
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(job)
	return string(data), err
}

func (v *ShellV2Tools) taskWait(ctx context.Context, raw string) (string, error) {
	var a shellReadArgs
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		return "", err
	}
	if err := Authorize(ctx, guard.Action{Tool: "task_shell_wait"}, raw); err != nil {
		return "", err
	}
	m, err := v.manager(ctx)
	if err != nil {
		return "", err
	}
	out, readErr := m.ReadJob(a.ID, a.MaxBytes)
	data, err := json.Marshal(struct {
		ID     string `json:"id"`
		Output string `json:"output"`
	}{ID: a.ID, Output: out})
	if err != nil {
		return "", err
	}
	if readErr != nil {
		// Propagate read errors so the model can distinguish "empty output"
		// from "job not found" / "read failed" (review #3, symmetric with read).
		return string(data), readErr
	}
	return string(data), nil
}

func (v *ShellV2Tools) taskWrite(ctx context.Context, raw string) (string, error) {
	var a shellWriteArgs
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		return "", err
	}
	if err := Authorize(ctx, guard.Action{Tool: "task_shell_stdin"}, raw); err != nil {
		return "", err
	}
	m, err := v.manager(ctx)
	if err != nil {
		return "", err
	}
	n, err := m.Write(a.ID, []byte(a.Data))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(`{"written":%d}`, n), nil
}

func (v *ShellV2Tools) taskCancel(ctx context.Context, raw string) (string, error) {
	var a shellIDArgs
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		return "", err
	}
	if err := Authorize(ctx, guard.Action{Tool: "task_shell_cancel"}, raw); err != nil {
		return "", err
	}
	m, err := v.manager(ctx)
	if err != nil {
		return "", err
	}
	if err := m.Cancel(a.ID); err != nil {
		return "", err
	}
	return `{"canceled":true}`, nil
}
