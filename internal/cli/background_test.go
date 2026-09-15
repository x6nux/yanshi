package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/x6nux/yanshi/internal/lockfile"
)

// fakeProcess is a BackgroundProcess a test controls: Wait blocks until the
// test releases it or returns an error immediately, which is how the
// "died before ready" branch is reached without a real child.
type fakeProcess struct {
	pid  int
	once sync.Once
	done chan struct{}
	err  error
}

func newFakeProcess(pid int, exitsImmediately bool, err error) *fakeProcess {
	p := &fakeProcess{pid: pid, done: make(chan struct{}), err: err}
	if exitsImmediately {
		close(p.done)
	}
	return p
}

func (p *fakeProcess) Pid() int { return p.pid }
func (p *fakeProcess) Wait() error {
	<-p.done
	return p.err
}
func (p *fakeProcess) release() { p.once.Do(func() { close(p.done) }) }

// spawnRecorder captures what StartBackground asked the OS to run.
type spawnRecorder struct {
	mu    sync.Mutex
	calls int
	exe   string
	args  []string
	log   string
	root  string
	proc  BackgroundProcess
	err   error
}

func (s *spawnRecorder) spawn(exe string, args []string, logPath, root string, _ io.Reader) (BackgroundProcess, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.exe, s.args, s.log, s.root = exe, args, logPath, root
	return s.proc, s.err
}

// TestStartBackgroundReportsAnExistingDaemon pins rule one: starting twice must
// not produce two backends on one store. The spawner must not be called at all.
func TestStartBackgroundReportsAnExistingDaemon(t *testing.T) {
	rec := &spawnRecorder{}
	res, err := StartBackground(context.Background(), BackgroundOptions{
		Root:  t.TempDir(),
		Spawn: rec.spawn,
		StatusFn: func(context.Context, string) DaemonStatus {
			return DaemonStatus{Found: true, Alive: true, Ready: true, PID: 4242, Addr: "127.0.0.1:5555"}
		},
	})
	if err != nil {
		t.Fatalf("StartBackground: %v", err)
	}
	if !res.AlreadyRunning || res.Started {
		t.Fatalf("res = %#v, want already-running", res)
	}
	if res.PID != 4242 || res.Addr != "127.0.0.1:5555" {
		t.Fatalf("res = %#v, want the EXISTING daemon's pid/addr", res)
	}
	if rec.calls != 0 {
		t.Fatalf("spawned a second daemon (%d calls)", rec.calls)
	}
}

// TestStartBackgroundWaitsForReadyNotAlive pins rule two: the call returns
// only once the daemon ANSWERS readiness, and the pid/addr it reports come from
// the lockfile (the daemon that is serving) rather than from the spawner.
func TestStartBackgroundWaitsForReadyNotAlive(t *testing.T) {
	rec := &spawnRecorder{proc: newFakeProcess(999, false, nil)}
	probes := 0
	res, err := StartBackground(context.Background(), BackgroundOptions{
		Root:       t.TempDir(),
		ConfigPath: "cfg.yaml",
		ExtraArgs:  []string{"-fake-model"},
		Spawn:      rec.spawn,
		PollEvery:  time.Millisecond,
		Wait:       2 * time.Second,
		StatusFn: func(context.Context, string) DaemonStatus {
			probes++
			switch {
			case probes == 1:
				// Pre-spawn check: nobody owns the project yet.
				return DaemonStatus{}
			case probes < 4:
				// Spawned, alive, still assembling.
				return DaemonStatus{Found: true, Alive: true, Ready: false, PID: 999}
			default:
				return DaemonStatus{Found: true, Alive: true, Ready: true, PID: 999, Addr: "127.0.0.1:6001"}
			}
		},
	})
	if err != nil {
		t.Fatalf("StartBackground: %v", err)
	}
	if !res.Started || res.AlreadyRunning {
		t.Fatalf("res = %#v, want started", res)
	}
	if res.PID != 999 || res.Addr != "127.0.0.1:6001" || res.LogPath == "" {
		t.Fatalf("res = %#v", res)
	}
	if probes < 4 {
		t.Fatalf("returned before readiness was observed (probes=%d)", probes)
	}
	if rec.calls != 1 {
		t.Fatalf("spawn calls = %d, want 1", rec.calls)
	}
	if got := strings.Join(rec.args, " "); got != "serve -config cfg.yaml -fake-model" {
		t.Fatalf("child args = %q", got)
	}
	if rec.root == "" || rec.log == "" {
		t.Fatalf("spawn got root=%q log=%q", rec.root, rec.log)
	}
	rec.proc.(*fakeProcess).release()
}

// TestStartBackgroundSurfacesAnEarlyExitWithTheLog pins the failure path an
// operator actually hits: the child dies during bootstrap, and the only useful
// information is in the log file. The error must carry it.
func TestStartBackgroundSurfacesAnEarlyExitWithTheLog(t *testing.T) {
	// HOME is redirected because backgroundLogPath derives from
	// lockfile.Path, which resolves through the REAL user cache directory: the
	// fixture log below was being written into the operator's shared run dir
	// and never removed (measured: it showed up next to the live daemon's own
	// lockfile). t.Setenv forbids t.Parallel, which this test does not use.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	t.Setenv("LocalAppData", filepath.Join(home, "AppData", "Local"))
	dir := t.TempDir()
	// Pre-create the log where the daemon would write it, so the assertion is
	// about TAILING the log rather than about the spawner's plumbing.
	lockPath, err := lockfile.Path(dir)
	if err != nil {
		t.Fatalf("lockfile.Path: %v", err)
	}
	logPath, err := backgroundLogPath(dir)
	if err != nil {
		t.Fatalf("backgroundLogPath: %v", err)
	}
	if filepath.Dir(logPath) != filepath.Dir(lockPath) {
		t.Fatalf("log %s is not next to lockfile %s", logPath, lockPath)
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatalf("mkdir log dir: %v", err)
	}
	if err := os.WriteFile(logPath, []byte("line one\nboom: bad config\n"), 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}
	rec := &spawnRecorder{proc: newFakeProcess(1234, true, errors.New("exit status 1"))}
	_, err = StartBackground(context.Background(), BackgroundOptions{
		Root: dir, Spawn: rec.spawn, PollEvery: time.Millisecond, Wait: time.Second,
		StatusFn: func(context.Context, string) DaemonStatus { return DaemonStatus{} },
	})
	if err == nil {
		t.Fatal("a daemon that exited before readiness must be an error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "exited before it was ready") {
		t.Errorf("err = %q, want it to say the daemon exited", msg)
	}
	if !strings.Contains(msg, "boom: bad config") {
		t.Errorf("err = %q, want the tail of the log", msg)
	}
}

// TestStartBackgroundTimesOutWhenNeverReady proves the wait is bounded and the
// message says what to check, rather than hanging a script forever.
func TestStartBackgroundTimesOutWhenNeverReady(t *testing.T) {
	dir := t.TempDir()
	rec := &spawnRecorder{proc: newFakeProcess(7, false, nil)}
	_, err := StartBackground(context.Background(), BackgroundOptions{
		Root: dir, Spawn: rec.spawn, PollEvery: time.Millisecond, Wait: 15 * time.Millisecond,
		StatusFn: func(context.Context, string) DaemonStatus {
			return DaemonStatus{Found: true, Alive: true, Ready: false, PID: 7}
		},
	})
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if !strings.Contains(err.Error(), "did not become ready") {
		t.Errorf("err = %q", err.Error())
	}
	rec.proc.(*fakeProcess).release()
}

// TestBackgroundLogPathIsPerProject proves two roots cannot share a log even
// when lockfile.sanitize collapses their names to the same key.
func TestBackgroundLogPathIsPerProject(t *testing.T) {
	a, err := backgroundLogPath("/tmp/proj/a-b")
	if err != nil {
		t.Fatalf("backgroundLogPath: %v", err)
	}
	b, err := backgroundLogPath("/tmp/proj/a_b")
	if err != nil {
		t.Fatalf("backgroundLogPath: %v", err)
	}
	if a == b {
		t.Fatalf("two roots share a log path: %s", a)
	}
}

// TestStartBackgroundAwaitsAnUnreadyOwner pins the second half of rule one: a
// daemon that owns the project but has not finished assembling is waited for,
// and NOT duplicated. Reporting it as "started" would hand the caller a backend
// that refuses connections; spawning a second one would put two writers on the
// same store.
func TestStartBackgroundAwaitsAnUnreadyOwner(t *testing.T) {
	rec := &spawnRecorder{}
	probes := 0
	res, err := StartBackground(context.Background(), BackgroundOptions{
		Root:      t.TempDir(),
		Spawn:     rec.spawn,
		PollEvery: time.Millisecond,
		Wait:      2 * time.Second,
		StatusFn: func(context.Context, string) DaemonStatus {
			probes++
			if probes < 3 {
				return DaemonStatus{Found: true, Alive: true, Ready: false, PID: 5150}
			}
			return DaemonStatus{Found: true, Alive: true, Ready: true, PID: 5150, Addr: "127.0.0.1:6002"}
		},
	})
	if err != nil {
		t.Fatalf("StartBackground: %v", err)
	}
	if !res.AlreadyRunning || res.Started {
		t.Fatalf("res = %#v, want already-running", res)
	}
	if res.PID != 5150 || res.Addr != "127.0.0.1:6002" {
		t.Fatalf("res = %#v", res)
	}
	if rec.calls != 0 {
		t.Fatalf("spawned a second daemon (%d calls) while one was assembling", rec.calls)
	}
}
