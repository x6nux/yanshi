package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/x6nux/yanshi/internal/lockfile"
)

// BackgroundProcess is the handle StartBackground needs from a started child:
// its pid (for diagnostics) and a way to learn that it died before it was
// ready. It is an interface rather than *os.Process because the ONLY fake that
// can be written for *os.Process is one whose Wait() returns an error
// immediately, which would make every test take the "died early" branch and
// leave the timeout path untested. spawnDetached returns the real one.
type BackgroundProcess interface {
	Pid() int
	Wait() error
}

// BackgroundSpawner starts the daemon process detached from this one's terminal
// and writes its stdout/stderr to logPath. It returns once the child has been
// CREATED, not once it is ready. stdin is nil in production, which means the
// child gets /dev/null.
type BackgroundSpawner func(exe string, args []string, logPath, root string, stdin io.Reader) (BackgroundProcess, error)

// BackgroundOptions configures a detached daemon start.
type BackgroundOptions struct {
	// ConfigPath is the config the child boots with ("" = the child's default).
	ConfigPath string
	// ExtraArgs are passed through to the child verbatim, AFTER the serve
	// subcommand — this is how -fake-model and -addr reach it.
	ExtraArgs []string
	// Root is the project root the daemon claims; "" = the process cwd.
	Root string
	// Wait bounds how long StartBackground waits for the child to become
	// READY (not merely alive). Zero takes DefaultBackgroundWait.
	Wait time.Duration
	// Spawn starts the detached child. Nil takes spawnDetached. Injected so a
	// test can drive the bookkeeping (already-running detection, readiness
	// polling, log tailing) without forking the test binary.
	Spawn BackgroundSpawner
	// StatusFn reads the daemon status for readiness polling. Nil takes
	// RunDaemonStatus.
	StatusFn func(ctx context.Context, root string) DaemonStatus
	// PollEvery overrides the readiness poll interval (tests). Zero = 200ms.
	PollEvery time.Duration
}

// BackgroundResult reports what `yanshi -b` did.
type BackgroundResult struct {
	// Started is true when THIS call started the daemon; false when one was
	// already running for the project (which is not an error — starting a
	// daemon twice would be, and the lockfile is what prevents it).
	Started bool `json:"started"`
	// AlreadyRunning is Started's inverse, present so the JSON reads without a
	// double negative.
	AlreadyRunning bool   `json:"alreadyRunning"`
	PID            int    `json:"pid,omitempty"`
	Addr           string `json:"addr,omitempty"`
	LogPath        string `json:"log,omitempty"`
	Root           string `json:"root,omitempty"`
}

// DefaultBackgroundWait bounds the readiness wait. It is generous because the
// child runs the full bootstrap (store migrations, proxy start, plugin
// discovery) before it listens, and a short timeout would report failure for a
// daemon that was merely slow — the error text says which it was.
const DefaultBackgroundWait = 30 * time.Second

// StartBackground starts `yanshi serve` as a detached daemon for one project.
//
// Two rules shape it:
//
//  1. Starting is IDEMPOTENT. If a live daemon already owns the project
//     lockfile, that is reported and nothing is spawned. Spawning anyway is
//     the double-backend case the lockfile exists to prevent: two writers on
//     one SQLite store, with the first still owning daemon/schedule control.
//  2. Success means READY, not merely alive. bootstrap assembles the store,
//     the sandbox and the proxy before it listens, and "process exists" is
//     exactly the state in which a client that believed it would fail to
//     connect. PID and address are read back from the lockfile the child
//     itself wrote, so they describe the daemon that is serving rather than
//     the argv this process built.
func StartBackground(ctx context.Context, opts BackgroundOptions) (BackgroundResult, error) {
	root := opts.Root
	if root == "" {
		wd, err := os.Getwd()
		if err != nil {
			return BackgroundResult{}, fmt.Errorf("background: %w", err)
		}
		root = wd
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		absRoot = root
	}
	statusFn := opts.StatusFn
	if statusFn == nil {
		statusFn = RunDaemonStatus
	}

	// Rule 1: whoever owns the project now is the daemon this call reports —
	// but only once it is READY. An owner that is alive and still assembling is
	// neither a success nor a reason to spawn: it is the thing to wait for.
	//
	// Both halves are load-bearing. Reporting an unready owner as "started"
	// would hand a script a backend that refuses connections; spawning a second
	// process because the first is not ready yet would put two writers on one
	// SQLite store, which is the state the lockfile exists to prevent.
	existing := statusFn(ctx, absRoot)
	if existing.Found && existing.Alive {
		if existing.Ready {
			return BackgroundResult{
				AlreadyRunning: true, PID: existing.PID, Addr: existing.Addr, Root: absRoot,
			}, nil
		}
		res, werr := waitForReady(ctx, absRoot, existing.PID, "", nil, opts, statusFn)
		if werr != nil {
			return BackgroundResult{}, werr
		}
		res.AlreadyRunning = true
		return res, nil
	}

	logPath, err := backgroundLogPath(absRoot)
	if err != nil {
		return BackgroundResult{}, err
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return BackgroundResult{}, fmt.Errorf("background: log dir: %w", err)
	}

	exe, err := os.Executable()
	if err != nil {
		return BackgroundResult{}, fmt.Errorf("background: locate executable: %w", err)
	}
	spawn := opts.Spawn
	if spawn == nil {
		spawn = spawnDetached
	}
	proc, err := spawn(exe, buildBackgroundArgs(opts), logPath, absRoot, nil)
	if err != nil {
		return BackgroundResult{}, fmt.Errorf("background: start %s: %w", exe, err)
	}

	// Watch the child for the whole wait: a daemon that dies during bootstrap
	// must fail the command with its log, not sit until the timeout and then
	// report a slow start.
	exited := make(chan error, 1)
	go func() { exited <- proc.Wait() }()

	res, err := waitForReady(ctx, absRoot, proc.Pid(), logPath, exited, opts, statusFn)
	if err != nil {
		return BackgroundResult{}, err
	}
	res.Started = true
	res.LogPath = logPath
	return res, nil
}

// waitForReady polls until the daemon answers readiness, the deadline passes,
// or ctx is cancelled. logPath is "" when this call did not spawn the daemon
// (an existing owner is being awaited), which only changes the wording.
//
// The child-exit watch is intentionally NOT here: whether a process is ours to
// reap is a property of the caller, and the spawned path already has a
// goroutine on it.
func waitForReady(
	ctx context.Context, absRoot string, pid int, logPath string, exited <-chan error,
	opts BackgroundOptions, statusFn func(context.Context, string) DaemonStatus,
) (BackgroundResult, error) {
	wait := opts.Wait
	if wait <= 0 {
		wait = DefaultBackgroundWait
	}
	poll := opts.PollEvery
	if poll <= 0 {
		poll = 200 * time.Millisecond
	}
	deadline := time.Now().Add(wait)
	for {
		if st := statusFn(ctx, absRoot); st.Alive && st.Ready {
			return BackgroundResult{PID: st.PID, Addr: st.Addr, Root: absRoot}, nil
		}
		select {
		case werr := <-exited:
			return BackgroundResult{}, fmt.Errorf(
				"background: daemon exited before it was ready: %v\nlast log lines:\n%s",
				werr, tailFile(logPath, 20))
		case <-time.After(poll):
		case <-ctx.Done():
			return BackgroundResult{}, ctx.Err()
		}
		if time.Now().After(deadline) {
			if logPath == "" {
				return BackgroundResult{}, fmt.Errorf(
					"background: the daemon already owning %s (pid %d) did not become ready within %s; "+
						"check `yanshi daemon status` (a live-but-unready daemon is wedged, not starting)",
					absRoot, pid, wait)
			}
			return BackgroundResult{}, fmt.Errorf(
				"background: daemon (pid %d) did not become ready within %s; it may still be starting. "+
					"Check `yanshi daemon status` and the log at %s\nlast log lines:\n%s",
				pid, wait, logPath, tailFile(logPath, 20))
		}
	}
}

// buildBackgroundArgs is the child's argv: the serve subcommand, the config it
// must boot with, then whatever the caller passed through.
func buildBackgroundArgs(opts BackgroundOptions) []string {
	args := []string{"serve"}
	if opts.ConfigPath != "" {
		args = append(args, "-config", opts.ConfigPath)
	}
	return append(args, opts.ExtraArgs...)
}

// backgroundLogPath is where the daemon's output goes. It sits next to the
// project lockfile, so "where is this daemon's log" has one answer an operator
// can find from the root they started it in, and it is derived from the
// lockfile's own name so the two cannot drift apart.
func backgroundLogPath(absRoot string) (string, error) {
	lfPath, err := lockfile.Path(absRoot)
	if err != nil {
		return "", fmt.Errorf("background: %w", err)
	}
	base := strings.TrimSuffix(filepath.Base(lfPath), ".lock")
	// A short hash of the root keeps the name stable AND distinct when two
	// roots sanitize to the same key (lockfile.sanitize collapses punctuation,
	// so two different projects can share a base name).
	sum := sha256.Sum256([]byte(absRoot))
	return filepath.Join(filepath.Dir(lfPath), base+"-"+hex.EncodeToString(sum[:4])+".log"), nil
}

// tailFile returns the last n lines of path, or a note saying why it could not.
// A missing or empty log is normal on a fast failure — the child can die before
// writing — so it must not turn a useful error into a confusing one.
func tailFile(path string, n int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("(no log at %s: %v)", path, err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	if len(lines) == 1 && lines[0] == "" {
		return "(log is empty)"
	}
	return strings.Join(lines, "\n")
}

// RenderBackgroundResult writes the human-readable or JSON form of a start.
func RenderBackgroundResult(w io.Writer, r BackgroundResult, asJSON bool) {
	if asJSON {
		line, err := json.Marshal(r)
		if err != nil {
			fmt.Fprintf(w, "{\"started\":false,\"error\":%q}\n", err.Error())
			return
		}
		fmt.Fprintln(w, string(line))
		return
	}
	switch {
	case r.AlreadyRunning:
		fmt.Fprintf(w, "yanshi: daemon already running pid=%d addr=%s root=%s\n", r.PID, r.Addr, r.Root)
	case r.Started:
		fmt.Fprintf(w, "yanshi: daemon started pid=%d addr=%s\n", r.PID, r.Addr)
		fmt.Fprintf(w, "yanshi: log -> %s\n", r.LogPath)
		fmt.Fprintln(w, "yanshi: stop with `yanshi daemon stop`")
	}
}
