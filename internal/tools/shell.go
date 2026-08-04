package tools

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/secproc"
	"github.com/x6nux/yanshi/internal/shell"
)

// safeShellCommands is the built-in default allowlist of read-only commands that
// are auto-allowed (no permission prompt). They have no side effects (don't write,
// delete, or modify). Path-taking commands in this set (ls, cat, find, grep, …)
// are still jailed: "../" traversal is rejected (see run).
var safeShellCommands = map[string]bool{
	"ls": true, "cat": true, "head": true, "tail": true, "pwd": true,
	"grep": true, "egrep": true, "fgrep": true, "find": true, "echo": true,
	"wc": true, "which": true, "whereis": true, "file": true, "stat": true,
	"du": true, "df": true, "date": true, "uname": true, "whoami": true,
	"hostname": true, "id": true, "env": true, "printenv": true, "tree": true,
	"diff": true, "sort": true, "uniq": true, "cut": true, "tr": true,
	"dir": true, "type": true, "getfacl": true, "realpath": true,
}

// shellMetachars that would allow chaining to unsafe commands / redirection.
var shellMetachars = []string{"&&", "||", "|", ";", ">", "<", "`", "$("}

func hasShellMetachar(cmd string) bool {
	for _, m := range shellMetachars {
		if strings.Contains(cmd, m) {
			return true
		}
	}
	return false
}

// firstWord returns the command name (first whitespace-delimited token),
// trimmed of any leading path prefix (e.g. "/usr/bin/ls" → "ls") and a Windows
// ".exe" suffix.
func firstWord(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return ""
	}
	name := strings.Fields(cmd)[0]
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	return strings.TrimSuffix(name, ".exe")
}

// ShellTools exposes shell_run.
type ShellTools struct {
	root string
	Run  *GuardedTool
}

// NewShellTools builds shell tools with commands rooted at root.
func NewShellTools(root string) *ShellTools {
	if root == "" {
		root = "."
	}
	s := &ShellTools{root: root}
	s.Run = NewGuardedTool(
		"shell_run", "Bash",
		"Run a single shell command. Returns combined stdout+stderr and exit code.\n"+
			"\n⚠️  Restrictions:\n"+
			"  • No shell metacharacters allowed (&& || | ; > < ` $())\n"+
			"  • No \"../\" path traversal\n"+
			"\n═══ Default shell (env=\"auto\") ═══\n"+
			"  Windows → cmd /c\n"+
			"  Linux/macOS → sh -c\n"+
			"\n═══ Available env values ═══\n"+
			"  \"auto\"       — auto-detect (default)\n"+
			"  \"cmd\"        — cmd /c (Windows only)\n"+
			"  \"powershell\" — powershell -Command (Windows only)\n"+
			"  \"bash\"       — bash -c (Unix); Git Bash on Windows\n"+
			"  \"zsh\"        — zsh -c\n"+
			"  \"sh\"         — sh -c\n"+
			"\n═══ Windows Git Bash ⚠️ ═══\n"+
			"MSYS2's bash does NOT auto-append \".exe\" when resolving commands. "+
			"If you get \"command not found\", retry with .exe suffix, "+
			"e.g. \"go.exe version\" instead of \"go version\". "+
			"Use env=\"cmd\" (default) to avoid this issue entirely.",
		120*time.Second,
		params(map[string]*schema.ParameterInfo{
			"command": {Type: schema.String, Desc: "command line (metacharacters are rejected)", Required: true},
			"workdir": {Type: schema.String, Desc: "working directory (default work root)"},
			"timeout": {Type: schema.Integer, Desc: "timeout in seconds (default 120)"},
			"env":     {Type: schema.String, Desc: "Shell environment. Default (auto): Windows→cmd, Unix→sh. Options: auto, cmd, powershell, bash, zsh, sh. See full description for details."},
		}),
		s.stream,
	)
	return s
}

// stream is shell_run's StreamFunc: it pipes the command's combined stdout/stderr
// line-by-line into Text (TUI scrolling window) + Result (model), pushes a
// per-second Status ("运行中·Xs"), and finishes with an exit-code footer. Errors
// at any stage become a single ✗ chunk. This replaces the old synchronous run +
// lineProgressWriter pair (Task 2.2).
func (s *ShellTools) stream(ctx context.Context, argsJSON string) <-chan ToolChunk {
	ch := make(chan ToolChunk, 32)
	go func() {
		defer close(ch)
		var a shellRunArgs
		if err := ParseArgs(argsJSON, &a); err != nil {
			pushErrChunk(ch, err)
			return
		}
		// Every shell command goes through Authorize, including those whose
		// first word appears in safeShellCommands. The map remains for UI
		// display hints (Task 18's TUI may surface a "known safe command"
		// affordance), but it no longer affects the security path — the guard's
		// structural HardDeny on shell metachar / unknown policy / denylist, and
		// the overridable HardDeny on shell-policy=deny (which YOLO/Auto may
		// still bypass via the callback), MUST NOT be affected by knowing the
		// safe-list. Workdir = s.root feeds the destructive-deletion dimension
		// (catastrophic block / out-of-workdir escalation) for yolo/auto.
		if err := Authorize(ctx, guard.Action{Tool: "shell_run", Shell: a.Command, Workdir: s.root}, argsJSON); err != nil {
			pushErrChunk(ch, err)
			return
		}
		if strings.Contains(a.Command, "../") || strings.Contains(a.Command, `..\`) {
			pushErrChunk(ch, fmt.Errorf("'..' path traversal is not allowed; use paths relative to the work root"))
			return
		}
		// Honor a caller-specified per-call timeout (shell_run's "timeout" arg) when
		// it is shorter than the tool's DefaultTimeout already on ctx — preserves
		// the per-call cap callers rely on. ctx here is the Stream deadline context.
		if a.Timeout > 0 {
			cctx, cancel := context.WithTimeout(ctx, time.Duration(a.Timeout)*time.Second)
			defer cancel()
			ctx = cctx
		}
		wd := a.Workdir
		if wd == "" {
			wd = s.root
		}
		// (Task 21) Prefer SecureProcessFactory when bound; otherwise fall back
		// to the direct exec path so SSE / unit tests without a factory still
		// work. The fallback still goes through Authorize above; it only skips
		// the central launcher seam (e.g. when no manager is configured).
		if f, ok := SecureProcessFactoryFromContext(ctx); ok {
			prog, args, _ := shell.ShellArgv(a.Env, a.Command)
			factoryStart := time.Now()
			started, err := f.Start(ctx, secproc.SecureProcessSpec{
				Tool: "shell_run", Shell: a.Command, Program: prog, Args: args, Dir: wd,
			})
			if err != nil {
				pushErrChunk(ch, err)
				return
			}
			// Fail closed on a Factory that forgot the reaper, exactly as
			// RunSecureCapture does — this inlined path used to skip that check
			// AND never call Wait at all, leaking one unreaped child per
			// shell_run.
			if started.Wait == nil {
				pushErrChunk(ch, fmt.Errorf("shell: Factory returned a process with no reaper (fail-closed)"))
				return
			}
			// Drain stdout to EOF BEFORE Wait: (*exec.Cmd).Wait closes the
			// stdout pipe, so reading after it races into "file already closed"
			// and silently truncates the output.
			if started.Stdout != nil {
				if err := streamFromReader(ctx, ch, started.Stdout); err != nil {
					pushErrChunk(ch, err)
					return
				}
			}
			exitCode, err := waitExitCode(started.Wait)
			if err != nil {
				pushErrChunk(ch, fmt.Errorf("shell: %w", err))
				return
			}
			// The footer is not decoration: without it a non-zero exit is
			// indistinguishable from success for the model, because stderr and
			// stdout are merged into the same untagged line stream. The legacy
			// pipe path below always emitted it; this path must match.
			footer := fmt.Sprintf("── exit %d · %s ──\n", exitCode, formatDur(time.Since(factoryStart)))
			select {
			case ch <- ToolChunk{Text: footer, Result: footer}:
			case <-ctx.Done():
			}
			return
		}
		cmd := shellCommand(ctx, a.Env, a.Command)
		cmd.Dir = wd
		cmd.WaitDelay = 5 * time.Second
		pr, pw := io.Pipe() // merge stdout+stderr into one readable stream
		cmd.Stdout = pw
		cmd.Stderr = pw
		start := time.Now()
		if err := cmd.Start(); err != nil {
			pw.Close()
			pushErrChunk(ch, fmt.Errorf("shell start: %w", err))
			return
		}
		// Wait in a goroutine and close pw when the command exits so the scanner
		// sees EOF. waitCh carries the wait error (nil on clean exit).
		waitCh := make(chan error, 1)
		go func() {
			waitCh <- cmd.Wait()
			pw.Close()
		}()
		// Status ticker: per-second "运行中·Xs" on the right-side panel (overwrite).
		// Non-blocking send so a status tick can never stall the stdout pump.
		// tickStopped is closed when the ticker goroutine actually exits, so the
		// main goroutine can wait for it before returning (and thus before
		// defer close(ch) runs) — without this, close(ch) races with a final
		// in-flight ch<- from the ticker (DATA RACE: closechan vs chansend).
		tickDone := make(chan struct{})
		tickStopped := make(chan struct{})
		go func() {
			t := time.NewTicker(time.Second)
			defer t.Stop()
			defer close(tickStopped)
			for {
				select {
				case <-tickDone:
					return
				case <-t.C:
					select {
					case ch <- ToolChunk{Status: "运行中·" + formatDur(time.Since(start))}:
					default:
					}
				}
			}
		}()
		// Ensure the ticker has fully stopped before this goroutine returns
		// (and defer close(ch) fires) on EVERY exit path — the scanner's
		// ctx.Done return, the pushErrChunk returns, and the normal footer
		// path. Registered after defer close(ch) so LIFO runs it FIRST.
		defer func() {
			close(tickDone)
			<-tickStopped
		}()
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text() + "\n"
			select {
			case ch <- ToolChunk{Text: line, Result: line}:
			case <-ctx.Done():
				return
			}
		}
		waitErr := <-waitCh
		// tickDone is closed + tickStopped awaited by the deferred func above;
		// no close(tickDone) here (would double-close).
		if ctx.Err() != nil {
			pushErrChunk(ch, fmt.Errorf("shell: %w", ctx.Err()))
			return
		}
		exitCode, waitFail := exitCodeFromWaitErr(waitErr)
		if waitFail != nil {
			pushErrChunk(ch, fmt.Errorf("shell: %w", waitFail))
			return
		}
		footer := fmt.Sprintf("── exit %d · %s ──\n", exitCode, formatDur(time.Since(start)))
		ch <- ToolChunk{Text: footer, Result: footer}
	}()
	return ch
}

type shellRunArgs struct {
	Command string `json:"command"`
	Workdir string `json:"workdir"`
	Timeout int    `json:"timeout"`
	Env     string `json:"env"` // shell environment: ""|auto|cmd|powershell|bash|zsh|sh
}

// shellCommand returns an exec.Cmd configured for the requested shell env.
// Task 15: the resolution logic now lives in internal/shell.ShellArgv so
// SecureProcessFactory (Task 14) and the future shell v2 (Task 20) share one
// interpreter-argv builder with legacy shell_run. This wrapper preserves the
// pre-Task-15 fallback shape (cmd on Windows, sh elsewhere) so the existing
// shell_run path stays byte-identical on the happy path; the only behavior
// change is that an unrecognized env ("fish", etc.) now degrades to the
// platform default rather than silently dispatching to a different shell.
//
// Supported env values (mirrors shell.ShellArgv):
//
//	"" / "auto"  — auto-detected: cmd /c (Windows), sh -c (Unix)
//	"cmd"        — cmd /c (Windows only; falls back to auto on Unix)
//	"powershell" — powershell -Command
//	"bash"       — bash -c
//	"zsh"        — zsh -c
//	"sh"         — sh -c
func shellCommand(ctx context.Context, env, command string) *exec.Cmd {
	prog, args, err := shell.ShellArgv(env, command)
	if err != nil {
		// Preserve legacy fallback: on Windows default to cmd /c, elsewhere sh -c.
		// Unknown env (e.g. "fish") still fails closed at Authorize; we only land
		// here if the env was recognized by the guard layer but ShellArgv rejected
		// it (programmatic error) — degrade to platform default rather than panic.
		if runtime.GOOS == "windows" {
			prog, args = "cmd", []string{"/c", command}
		} else {
			prog, args = "sh", []string{"-c", command}
		}
	}
	return exec.CommandContext(ctx, prog, args...)
}

// streamFromReader is the shared scanner helper (Task 21) used by BOTH
// the SecureProcessFactory path and the legacy pipe path in shell_run's
// stream(). It scans r line-by-line into Text chunks, feeds lines into
// both Text and Result, and emits a per-second Status tick. ctx.Done stops
// the scan early (tool context cancellation).
//
// The ticker is identical to the legacy path's, so both code paths produce
// the same TUI experience ("running·Xs").
func streamFromReader(ctx context.Context, ch chan<- ToolChunk, r io.Reader) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	scanDone := make(chan struct{})
	go func() {
		defer close(scanDone)
		for sc.Scan() {
			line := sc.Text() + "\n"
			select {
			case ch <- ToolChunk{Text: line, Result: line}:
			case <-ctx.Done():
				return
			}
		}
	}()

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// scanner already stopped via select before caller closes ch
			return ctx.Err()
		case <-scanDone:
			return sc.Err()
		case <-ticker.C:
		}
	}
}
