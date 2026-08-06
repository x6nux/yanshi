package tools

import (
	"context"
	"encoding/json"
	"errors"
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

// shellWaitArgs is shell_wait's input. TimeoutS bounds the wait itself; the
// session is untouched when it expires.
type shellWaitArgs struct {
	ID       string `json:"id"`
	TimeoutS int    `json:"timeout_s"`
}

type shellReadArgs struct {
	ID       string `json:"id"`
	MaxBytes int    `json:"max_bytes"`
	// FromStart selects the legacy tail form. Default (false) is incremental:
	// only what the session produced since the previous read.
	FromStart bool `json:"from_start"`
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

// shellV2JobIDNote documents the id namespace the four task_shell_* tools
// share: task_shell_start returns a JOB id ("job-<session id>"), and every
// other task_shell_* tool must resolve it through a *Job Manager method.
// Handing a job id to a session-scoped method (Write/Cancel) silently misses
// the sessions map and answers "session/job not found" — which is how
// task_shell_cancel spent its whole existence unable to cancel anything.
const shellV2JobIDNote = "id is the job id returned by task_shell_start"

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
			"id":         {Type: schema.String, Required: true},
			"max_bytes":  {Type: schema.Integer},
			"from_start": {Type: schema.Boolean, Desc: "Return the buffer tail instead of only what is new since the last read. Default false (incremental)."},
		}),
		SyncStream(v.read))
	v.Write = NewGuardedTool("shell_write_stdin", "Shell", "Write stdin to a shell session.", 30*time.Second,
		params(map[string]*schema.ParameterInfo{
			"id":   {Type: schema.String, Required: true},
			"data": {Type: schema.String, Required: true},
		}),
		SyncStream(v.write))
	v.Wait = NewGuardedTool("shell_wait", "Shell",
		"Wait for session completion. With timeout_s the wait YIELDS when it expires — "+
			"the session keeps running and can be waited on or read again.", 130*time.Second,
		params(map[string]*schema.ParameterInfo{
			"id":        {Type: schema.String, Required: true},
			"timeout_s": {Type: schema.Integer, Desc: "Give up waiting after this many seconds and return timed_out=true. The session is NOT cancelled. Omit to use the server's idle timeout."},
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
			"id":        {Type: schema.String, Required: true, Desc: shellV2JobIDNote},
			"max_bytes": {Type: schema.Integer},
		}),
		SyncStream(v.taskWait))
	v.TaskWrite = NewGuardedTool("task_shell_stdin", "Shell Job", "Write stdin to a background job.", 30*time.Second,
		params(map[string]*schema.ParameterInfo{
			"id":   {Type: schema.String, Required: true, Desc: shellV2JobIDNote},
			"data": {Type: schema.String, Required: true},
		}),
		SyncStream(v.taskWrite))
	v.TaskCancel = NewGuardedTool("task_shell_cancel", "Shell Job", "Cancel a background job.", 30*time.Second,
		params(map[string]*schema.ParameterInfo{
			"id": {Type: schema.String, Required: true, Desc: shellV2JobIDNote},
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
	// Incremental by default. The tail form returned overlapping data on every
	// poll, so a model watching a running command re-read the same output and
	// had no way to tell what was new — and output pushed out of the ring
	// buffer between two polls vanished with no marker at all.
	if a.FromStart {
		out, rerr := m.Read(a.ID, a.MaxBytes)
		if rerr != nil {
			return "", rerr
		}
		return fmt.Sprintf(`{"output":%q}`, out), nil
	}
	out, skipped, err := m.ReadNew(a.ID, a.MaxBytes)
	if err != nil {
		return "", err
	}
	if skipped > 0 {
		// Named rather than silent: losing output to the cap and producing
		// none at all are different situations for the caller.
		return fmt.Sprintf(`{"output":%q,"skipped_bytes":%d}`, out, skipped), nil
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
	var a shellWaitArgs
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
	sess, err := m.WaitFor(ctx, a.ID, time.Duration(a.TimeoutS)*time.Second)
	if errors.Is(err, shell.ErrWaitTimeout) {
		// A yield, not a failure: reported as a normal result so the model
		// can go do something else and come back. Returning an error here
		// would read as "the command failed" for a build that is fine.
		return fmt.Sprintf(`{"id":%q,"timed_out":true,"still_running":true}`, a.ID), nil
	}
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
	// WriteJob, not Write: a is the JOB id (what task_shell_start returned and
	// what this tool documents), and Write only ever looks at the session map.
	n, err := m.WriteJob(a.ID, []byte(a.Data))
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
	// CancelJob, not Cancel: see shellV2JobIDNote.
	if err := m.CancelJob(a.ID); err != nil {
		return "", err
	}
	return `{"canceled":true}`, nil
}
