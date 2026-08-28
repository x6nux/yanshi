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
			"  • Chaining with && || | ; is allowed, but EVERY segment is checked "+
			"separately and the strictest verdict decides the whole command — one "+
			"disallowed segment refuses all of them, and nothing runs.\n"+
			"  • Redirection is allowed; the target path is checked against the "+
			"filesystem permissions like any other write (or read, for <).\n"+
			"  • Not allowed at all: command substitution $(...) and backticks, "+
			"process substitution <(...), subshells ( ), here-documents <<, "+
			"background &, and newlines.\n"+
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
			"command": {Type: schema.String, Desc: "command line; && || | ; and redirection are allowed and judged segment by segment, but command/process substitution, subshells, here-docs, background & and newlines are rejected", Required: true},
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
		// NOTE: shell_run does NOT call Authorize here. It used to, and then
		// handed the spec to the factory directly; W-B-02 routed the spawn
		// through secproc.Launch, which Authorizes the same guard.Action as its
		// first fail-closed step. Keeping both would ask the operator twice for
		// one command. The two fields that made the local call richer than
		// Launch's travel down in the spec instead: Workdir (destructive
		// dimension boundary) and ArgsJSON (what the dialog displays).
		//
		// safeShellCommands is unaffected either way. It survives as a UI
		// display hint and has not been on the security path since Task 18 —
		// the guard's structural HardDeny on unreadable shell structure /
		// unknown policy / execpolicy parse-error, and the overridable HardDeny
		// on shell-policy=deny and on a denylist match (both of which YOLO/Auto
		// may still bypass via the callback), MUST NOT be affected by knowing
		// the safe-list.
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
		// W-B-02: ONE launch path, and it is the central one.
		//
		// This used to read "prefer the factory when bound, otherwise fall back
		// to a direct exec pipe". That fallback was not a degraded mode, it was
		// a second implementation of the whole spawn: no credential scrub, no
		// sandbox seam, no managed proxy env, and no fail-closed check that the
		// launcher wiring was present at all. Which one ran was decided by
		// whether some ancestor had remembered to bind a factory — and
		// bindExecutionContext's nil gate meant "forgot to wire it" and
		// "deliberately unsandboxed" were the same observable state.
		//
		// It is deleted rather than patched. Routing it through Authorize too
		// would have left two spawn implementations to keep in step, and the
		// pair had already drifted once (see childLaunchPosture's header for
		// the env divergence that shipped).
		//
		// The factory is now bound unconditionally by
		// orchestrator.bindExecutionContext, so the "no factory" case is a
		// wiring bug and secproc.Launch reports it as one instead of silently
		// spawning outside the firewall.
		prog, args, _ := shell.ShellArgv(a.Env, a.Command)
		factoryStart := time.Now()
		started, err := LaunchSecureProcess(ctx, secproc.SecureProcessSpec{
			Tool: "shell_run", Shell: a.Command, Program: prog, Args: args, Dir: wd,
			// s.root, NOT wd: wd may come from the model's own "workdir"
			// argument, and letting it move the destructive dimension's
			// boundary would make `{"workdir":"/"}` turn every deletion into an
			// in-scope one.
			Workdir: s.root, ArgsJSON: argsJSON,
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
		// Reap on EVERY exit path, not just the successful one. The
		// success path calls waitExitCode below; the streaming-error path
		// (which is how cancellation and per-call timeouts leave this
		// function — Esc in the TUI, `timeout_s` expiry) used to return
		// straight to the caller, so the fix above only ever covered the
		// commands that ran to completion.
		//
		// Wait is not just bookkeeping here: for the production factory it
		// is (*exec.Cmd).Wait, which is what closes the child's pipes.
		// Skipping it left both the unreaped child AND the goroutines
		// pumping those pipes parked forever.
		//
		// A guarded defer rather than an unconditional one, because the
		// success path needs Wait's exit STATUS and Wait must not be
		// called twice. No synchronization is needed on `reaped`: both the
		// write below and the deferred read happen on this goroutine.
		// Registered after `defer close(ch)`, so LIFO reaps the child
		// before the channel closes.
		reaped := false
		reap := func() {
			if !reaped {
				reaped = true
				_ = started.Wait()
			}
		}
		defer reap()
		// shell_run is a DISPLAY consumer: the model must see stderr as
		// well as stdout — a compiler error or a "permission denied" is
		// the whole answer for most commands. The factory hands the two
		// streams back separately (so the capture path's parsers get an
		// unpolluted stdout), so re-merge them here rather than dropping
		// stderr on the floor.
		//
		// "What a terminal would show" is the intent, not the guarantee.
		// MergeOutput races two copiers, so relative order between the two
		// streams is approximate (~11% of lines land ahead of an earlier one
		// in a stress measurement). Lines are never split or spliced. Order
		// matters less than presence here — the exit footer, not the
		// interleaving, is what tells the model whether the command failed —
		// but do not describe this stream as faithful. The deleted pipe path
		// used a single fd and did preserve order exactly; that is the one
		// thing lost with it, and it is why this paragraph exists.
		//
		// Drain to EOF BEFORE Wait: (*exec.Cmd).Wait closes the child's
		// pipes, so reading after it races into "file already closed" and
		// silently truncates the output.
		if started.Stdout != nil || started.Stderr != nil {
			merged := started.MergedOutput()
			err := streamFromReader(ctx, ch, merged)
			_ = merged.Close()
			if err != nil {
				pushErrChunk(ch, err)
				return
			}
		}
		reaped = true
		exitCode, err := waitExitCode(started.Wait)
		if err != nil {
			pushErrChunk(ch, fmt.Errorf("shell: %w", err))
			return
		}
		// The footer is not decoration: without it a non-zero exit is
		// indistinguishable from success for the model, because stderr and
		// stdout are merged into the same untagged line stream.
		footer := fmt.Sprintf("── exit %d · %s ──\n", exitCode, formatDur(time.Since(factoryStart)))
		select {
		case ch <- ToolChunk{Text: footer, Result: footer}:
		case <-ctx.Done():
		}
		return
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
// Task 15: the resolution logic lives in internal/shell.ShellArgv so
// SecureProcessFactory (Task 14) and shell v2 (Task 20) share one
// interpreter-argv builder. This wrapper adds the platform-default fallback
// (cmd on Windows, sh elsewhere) for an env ShellArgv rejects.
//
// Its ONE remaining caller is task_gate_run (gate.go). shell_run used to share
// it through the direct-pipe fallback W-B-02 deleted; gate keeps it because it
// runs an operator-declared gate command with its own argv contract
// (ADR-0012), not a model-authored one.
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

// streamFromReader is shell_run's output pump (Task 21): it scans r
// line-by-line into Text chunks, feeds lines into both Text and Result, and
// emits a per-second Status tick. ctx.Done stops the scan early (tool context
// cancellation).
//
// r is an io.ReadCloser rather than an io.Reader because the cancellation path
// REQUIRES closing it, and a signature that accepted a bare Reader let callers
// hand over something this function cannot terminate. On ctx.Done the scanner
// goroutine is typically parked inside sc.Scan → r.Read, which no amount of
// context cancellation reaches; closing r is what unblocks it.
//
// Returning before that goroutine exits is a DATA RACE, not merely untidy: the
// caller's `defer close(ch)` fires the moment this returns, while the scanner
// may still be inside `ch <- ToolChunk{…}`. Its select has ctx.Done() as the
// other arm, but a select with two ready cases picks either one, and even the
// ctx.Done arm carries no happens-before edge to the caller's close. An earlier
// comment here asserted "scanner already stopped via select before caller
// closes ch" — that was an assumption, and `go test -race` on a cancelled
// factory-path shell_run reports closechan/chansend on exactly this pair.
// Closing r and then awaiting scanDone is what makes the assertion true.
func streamFromReader(ctx context.Context, ch chan<- ToolChunk, r io.ReadCloser) error {
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
			// Close first (unblocks a scanner parked in Read), then wait for
			// the goroutine to be gone before returning to a caller whose
			// deferred close(ch) is the other half of the race.
			_ = r.Close()
			<-scanDone
			return ctx.Err()
		case <-scanDone:
			return sc.Err()
		case <-ticker.C:
		}
	}
}
