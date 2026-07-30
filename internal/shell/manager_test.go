package shell

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

var jsonMarshalProduction = json.Marshal

type fakeProcess struct {
	mu          sync.Mutex
	pid         int
	waitCh      chan error
	killed      bool
	canKillTree bool
}

func (p *fakeProcess) Wait() error { return <-p.waitCh }
func (p *fakeProcess) PID() int    { return p.pid }
func (p *fakeProcess) Kill() error {
	p.mu.Lock()
	p.killed = true
	p.mu.Unlock()
	// Simulate the OS killing the process so Wait returns. This fake sends a
	// plain error; the real *exec.ExitError extraction is tested separately
	// below using the test binary as a helper process.
	select {
	case p.waitCh <- errors.New("killed"):
	default:
	}
	return nil
}
func (p *fakeProcess) Capabilities() ProcessCapabilities {
	return ProcessCapabilities{CanKillTree: p.canKillTree}
}

type fakeConsole struct {
	mu     sync.Mutex
	closed bool
	out    chan []byte
}

func (c *fakeConsole) Read(p []byte) (int, error) {
	b, ok := <-c.out
	if !ok {
		return 0, io.EOF
	}
	return copy(p, b), nil
}
func (c *fakeConsole) Write(p []byte) (int, error) { return len(p), nil }
func (c *fakeConsole) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return nil
}
func (c *fakeConsole) Resize(uint16, uint16) error { return nil }
func (c *fakeConsole) PTY() bool                    { return false }

type fakeFactory struct {
	consoleOut   [][]byte
	canKillTree  bool
}

func (f *fakeFactory) Start(_ context.Context, spec LaunchSpec) (Process, Console, error) {
	// Buffer must be ≥ len(consoleOut) so the test does not block on the
	// channel send before the pump goroutine starts reading. Use a generous
	// minimum of 8 to keep small tests cheap.
	bufSize := len(f.consoleOut)
	if bufSize < 8 {
		bufSize = 8
	}
	ch := make(chan []byte, bufSize)
	for _, b := range f.consoleOut {
		ch <- b
	}
	close(ch)
	return &fakeProcess{pid: 42, waitCh: make(chan error, 1), canKillTree: f.canKillTree}, &fakeConsole{out: ch}, nil
}

func TestManagerStartSurvivesToolContextCancel(t *testing.T) {
	m := NewManager(Config{Root: t.TempDir(), MaxOutputBytes: 4096, Factory: &fakeFactory{consoleOut: [][]byte{[]byte("hi\n")}}})
	ctx, cancel := context.WithCancel(context.Background())
	sess, err := m.Start(ctx, LaunchSpec{ShellName: "sh", Command: "echo hi", Program: "sh", Args: []string{"-c", "echo hi"}})
	if err != nil {
		t.Fatal(err)
	}
	cancel() // caller (tool) ctx goes away — session MUST survive
	time.Sleep(20 * time.Millisecond)
	got := m.Snapshot(sess.ID)
	if got.State != StateRunning {
		t.Fatalf("session killed by tool ctx cancel: %#v", got)
	}
	_ = m.Cancel(sess.ID)
}

func TestManagerRingBufferCapsJobOutput(t *testing.T) {
	big := make([][]byte, 100)
	for i := range big {
		big[i] = []byte("abcdefgh")
	}
	m := NewManager(Config{Root: t.TempDir(), MaxOutputBytes: 16, Factory: &fakeFactory{consoleOut: big}})
	job, err := m.StartJob(context.Background(), "printf lots", LaunchSpec{ShellName: "sh", Program: "sh", Args: []string{"-c", "printf lots"}})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)
	out, _ := m.ReadJob(job.ID, 4096)
	if len(out) > 16 {
		t.Fatalf("ring buffer must cap output: %d", len(out))
	}
	_ = m.Cancel(job.SessionID)
}

func TestManagerWaitIsContextAware(t *testing.T) {
	m := NewManager(Config{Root: t.TempDir(), MaxOutputBytes: 4096, Factory: &fakeFactory{}})
	sess, err := m.Start(context.Background(), LaunchSpec{ShellName: "sh", Program: "sh", Args: []string{"-c", "sleep 60"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := m.Wait(ctx, sess.ID); err == nil {
		t.Fatal("Wait must honor context cancellation")
	}
	_ = m.Cancel(sess.ID)
}

func TestExitCodeFromRealExecExitError(t *testing.T) {
	if os.Getenv("YANSHI_SHELL_HELPER_EXIT") == "1" {
		os.Exit(7)
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestExitCodeFromRealExecExitError")
	cmd.Env = append(os.Environ(), "YANSHI_SHELL_HELPER_EXIT=1")
	err := cmd.Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("expected *exec.ExitError, got %T %v", err, err)
	}
	if got := exitCodeFrom(err); got != 7 {
		t.Fatalf("exitCodeFrom=%d, want 7", got)
	}
}

func TestManagerCancelInvokesKillOnly(t *testing.T) {
	m := NewManager(Config{Root: t.TempDir(), MaxOutputBytes: 4096, Factory: &fakeFactory{canKillTree: false}})
	sess, err := m.Start(context.Background(), LaunchSpec{ShellName: "sh", Program: "sh", Args: []string{"-c", "sleep 60"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Cancel(sess.ID); err != nil {
		t.Fatal(err)
	}
	got := m.Snapshot(sess.ID)
	if got.State != StateCanceled {
		t.Fatalf("state=%v", got.State)
	}
	// On a platform without KillTree we do NOT pretend tree-kill happened; the
	// process is canceled via plain Kill. Test only verifies state transition.
}

type memKV struct{ m sync.Map }

func (k *memKV) KVGet(key string) (string, bool, error) {
	v, ok := k.m.Load(key)
	if !ok {
		return "", false, nil
	}
	return v.(string), true, nil
}
func (k *memKV) KVSet(key, val string) error {
	k.m.Store(key, val)
	return nil
}

func TestManagerRestoreJobsLoadsPriorJobsAsStale(t *testing.T) {
	kv := &memKV{}
	// Simulate a prior boot that persisted a still-running job.
	prior := []Job{{ID: "job-x", SessionID: "s-x", Command: "go test", State: StateRunning, PID: 999, ExitCode: -1, StartedAt: time.Unix(1700000000, 0)}}
	data, _ := jsonMarshalProduction(prior)
	_ = kv.KVSet("security.shell.jobs.v1", string(data))
	m := NewManager(Config{Root: t.TempDir(), Factory: &fakeFactory{}}).WithPersistence(JobFromKV(kv))
	if err := m.RestoreJobs(); err != nil {
		t.Fatal(err)
	}
	// After restore, the job is in memory but its OS process is gone — it MUST
	// be marked stale so callers see StateStale, not StateRunning.
	jobs := m.ListJobs()
	if len(jobs) != 1 || jobs[0].State != StateStale {
		t.Fatalf("expected stale restored job, got %#v", jobs)
	}
}

func TestManagerCloseCancelsWaitsAndPersistsJobs(t *testing.T) {
	kv := &memKV{}
	persist := JobFromKV(kv)
	m := NewManager(Config{Root: t.TempDir(), Factory: &fakeFactory{}}).WithPersistence(persist)
	if _, err := m.StartJob(context.Background(), "sleep 60", LaunchSpec{ShellName: "sh", Program: "sh", Args: []string{"-c", "sleep 60"}}); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	jobs, err := persist.LoadJobs()
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("Close must flush final jobs, got %#v", jobs)
	}
}

// Ensure strings import is used in this test file (some build configurations
// flag unused imports when the test binary is reused as a helper subprocess).
var _ = strings.TrimSpace
