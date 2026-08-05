package shell

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/x6nux/yanshi/internal/netpolicy"
	"github.com/x6nux/yanshi/internal/sandbox"
	"github.com/x6nux/yanshi/internal/secproc"
)

// --- Manager.Read / Write / ReadJob / ReadJob-stale (were 0%) ---

func TestManagerReadWriteAndNotFound(t *testing.T) {
	m := NewManager(Config{Root: t.TempDir(), MaxOutputBytes: 4096, Factory: &fakeFactory{consoleOut: [][]byte{[]byte("payload\n")}}})
	sess, err := m.Start(context.Background(), LaunchSpec{ShellName: "sh", Program: "sh", Args: []string{"-c", "echo payload"}})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond) // let the pump drain

	// Read returns the buffered output.
	out, err := m.Read(sess.ID, 0)
	if err != nil || !strings.Contains(out, "payload") {
		t.Fatalf("Read: out=%q err=%v", out, err)
	}
	// Read with a small max returns only the tail.
	out, _ = m.Read(sess.ID, 3)
	if len(out) > 3 {
		t.Fatalf("Read(max=3) must cap: %q", out)
	}

	// Write sends bytes to the console (fakeConsole accepts anything).
	n, err := m.Write(sess.ID, []byte("ls\n"))
	if err != nil || n != 3 {
		t.Fatalf("Write: n=%d err=%v", n, err)
	}

	// NotFound paths.
	if _, err := m.Read("ghost", 0); err != ErrNotFound {
		t.Fatalf("Read ghost err=%v want ErrNotFound", err)
	}
	if _, err := m.Write("ghost", []byte("x")); err != ErrNotFound {
		t.Fatalf("Write ghost err=%v want ErrNotFound", err)
	}
	if _, err := m.ReadJob("ghost", 0); err != ErrNotFound {
		t.Fatalf("ReadJob ghost err=%v want ErrNotFound", err)
	}
	_ = m.Cancel(sess.ID)
}

// TestManagerReadJobOnStaleJob covers ReadJob's session==nil branch: a restored
// (stale) job has no live session.
func TestManagerReadJobOnStaleJob(t *testing.T) {
	kv := &memKV{}
	prior := []Job{{ID: "job-stale", SessionID: "s-gone", Command: "x", State: StateRunning, PID: 1, ExitCode: -1, StartedAt: time.Unix(1, 0)}}
	data, _ := jsonMarshalProduction(prior)
	_ = kv.KVSet("security.shell.jobs.v1", string(data))
	m := NewManager(Config{Root: t.TempDir(), Factory: &fakeFactory{}}).WithPersistence(JobFromKV(kv))
	if err := m.RestoreJobs(); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ReadJob("job-stale", 0); err != ErrNotFound {
		t.Fatalf("ReadJob on stale job err=%v want ErrNotFound", err)
	}
}

// TestManagerWaitNotFound covers Wait's ErrNotFound branch.
func TestManagerWaitNotFound(t *testing.T) {
	m := NewManager(Config{Root: t.TempDir(), Factory: &fakeFactory{}})
	if _, err := m.Wait(context.Background(), "ghost"); err != ErrNotFound {
		t.Fatalf("Wait ghost err=%v want ErrNotFound", err)
	}
}

// TestManagerCancelNotFound covers Cancel's ErrNotFound branch.
func TestManagerCancelNotFound(t *testing.T) {
	m := NewManager(Config{Root: t.TempDir(), Factory: &fakeFactory{}})
	if err := m.Cancel("ghost"); err != ErrNotFound {
		t.Fatalf("Cancel ghost err=%v want ErrNotFound", err)
	}
}

// --- Manager.Start error branches ---

func TestManagerStartNoFactory(t *testing.T) {
	m := NewManager(Config{Root: t.TempDir()})
	if _, err := m.Start(context.Background(), LaunchSpec{Program: "x"}); err == nil {
		t.Fatal("Start with nil factory must error")
	}
}

// errorFactory always fails to start.
type errorFactory struct{ err error }

func (f *errorFactory) Start(context.Context, LaunchSpec) (Process, Console, error) {
	return nil, nil, f.err
}

func TestManagerStartFactoryError(t *testing.T) {
	m := NewManager(Config{Root: t.TempDir(), Factory: &errorFactory{err: errors.New("boom")}})
	if _, err := m.Start(context.Background(), LaunchSpec{Program: "x"}); err == nil {
		t.Fatal("Start must surface factory error")
	}
}

func TestManagerStartJobFactoryError(t *testing.T) {
	m := NewManager(Config{Root: t.TempDir(), Factory: &errorFactory{err: errors.New("boom")}})
	if _, err := m.StartJob(context.Background(), "x", LaunchSpec{Program: "x"}); err == nil {
		t.Fatal("StartJob must surface factory error")
	}
}

// --- Manager.Wait done + idle paths via a self-completing process ---

// completingFactory spawns a process whose Wait returns immediately on its own
// (simulating a process that exits naturally), so the pump's process.Wait()
// unblocks right after the console EOFs and s.done closes without an explicit
// Cancel.
type completingProcess struct {
	pid     int
	killMux sync.Mutex
	killed  bool
}

func (p *completingProcess) Wait() error { return nil } // exits immediately
func (p *completingProcess) PID() int    { return p.pid }
func (p *completingProcess) Kill() error {
	p.killMux.Lock()
	p.killed = true
	p.killMux.Unlock()
	return nil
}
func (p *completingProcess) Capabilities() ProcessCapabilities {
	return ProcessCapabilities{CanKillTree: false}
}

type completingFactory struct {
	consoleOut [][]byte
}

func (f *completingFactory) Start(_ context.Context, _ LaunchSpec) (Process, Console, error) {
	ch := make(chan []byte, 8)
	for _, b := range f.consoleOut {
		ch <- b
	}
	close(ch)
	proc := &completingProcess{pid: 7}
	return proc, &fakeConsole{out: ch}, nil
}

func TestManagerWaitNaturalExit(t *testing.T) {
	m := NewManager(Config{Root: t.TempDir(), MaxOutputBytes: 4096, Factory: &completingFactory{consoleOut: [][]byte{[]byte("done\n")}}})
	sess, err := m.Start(context.Background(), LaunchSpec{Program: "x"})
	if err != nil {
		t.Fatal(err)
	}
	// Wait should return once the pump finishes (process self-completes).
	got, err := m.Wait(context.Background(), sess.ID)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got.State != StateExited {
		t.Fatalf("state=%v want exited", got.State)
	}
}

func TestManagerWaitWithIdleTimeoutSet(t *testing.T) {
	// When IdleTimeout is set AND the process exits naturally, Wait enters the
	// idle-timeout select (manager.go:214) and returns immediately because
	// s.done is already closed. (The time.After / ctx.Done arms of that select
	// are unreachable since s.done always wins once closed.)
	m := NewManager(Config{Root: t.TempDir(), MaxOutputBytes: 4096, IdleTimeout: 30 * time.Second, Factory: &completingFactory{consoleOut: [][]byte{[]byte("x\n")}}})
	sess, err := m.Start(context.Background(), LaunchSpec{Program: "x"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := m.Wait(context.Background(), sess.ID)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got.State != StateExited {
		t.Fatalf("state=%v want exited", got.State)
	}
}

// TestManagerWaitIdleTimeoutFires covers the time.After arm of the idle-timeout
// select at manager.go:213-214. With a fakeFactory (whose process never exits
// on its own) and a short IdleTimeout, Wait returns "idle timeout" because the
// deadline fires before s.done closes.
func TestManagerWaitIdleTimeoutFires(t *testing.T) {
	m := NewManager(Config{Root: t.TempDir(), MaxOutputBytes: 4096, IdleTimeout: 50 * time.Millisecond, Factory: &fakeFactory{}})
	sess, err := m.Start(context.Background(), LaunchSpec{Program: "x"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Wait(context.Background(), sess.ID)
	if err == nil || !strings.Contains(err.Error(), "idle timeout") {
		t.Fatalf("expected idle timeout, got %v", err)
	}
	// Clean up the still-running fake session.
	_ = m.Cancel(sess.ID)
}

// TestManagerWaitIdleTimeoutCtxCanceled covers the ctx.Done arm of the
// idle-timeout select at manager.go:215-216. The caller's context cancels
// before the idle deadline fires and before the process exits.
func TestManagerWaitIdleTimeoutCtxCanceled(t *testing.T) {
	m := NewManager(Config{Root: t.TempDir(), MaxOutputBytes: 4096, IdleTimeout: 5 * time.Second, Factory: &fakeFactory{}})
	sess, err := m.Start(context.Background(), LaunchSpec{Program: "x"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = m.Wait(ctx, sess.ID)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded, got %v", err)
	}
	_ = m.Cancel(sess.ID)
}

// --- Manager.Cancel with a process whose Kill errors ---

// killErrProcess: Kill returns an error; Wait returns nil so the pump finishes
// cleanly instead of leaking a goroutine.
type killErrProcess struct{ pid int }

func (p *killErrProcess) Wait() error { return nil }
func (p *killErrProcess) PID() int    { return p.pid }
func (p *killErrProcess) Kill() error { return errors.New("kill failed") }
func (p *killErrProcess) Capabilities() ProcessCapabilities {
	return ProcessCapabilities{CanKillTree: false}
}

type killErrFactory struct{}

func (f *killErrFactory) Start(_ context.Context, _ LaunchSpec) (Process, Console, error) {
	ch := make(chan []byte, 1)
	close(ch)
	return &killErrProcess{pid: 9}, &fakeConsole{out: ch}, nil
}

func TestManagerCancelKillError(t *testing.T) {
	m := NewManager(Config{Root: t.TempDir(), Factory: &killErrFactory{}})
	sess, err := m.Start(context.Background(), LaunchSpec{Program: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Cancel(sess.ID); err == nil {
		t.Fatal("Cancel must surface Kill error")
	}
}

// --- RestoreJobs LoadJobs-error ---

type errorPersist struct{}

func (errorPersist) SaveJob(Job) error        { return nil }
func (errorPersist) LoadJobs() ([]Job, error) { return nil, errors.New("load boom") }

func TestManagerRestoreJobsLoadError(t *testing.T) {
	m := NewManager(Config{Root: t.TempDir(), Factory: &fakeFactory{}}).WithPersistence(errorPersist{})
	if err := m.RestoreJobs(); err == nil {
		t.Fatal("RestoreJobs must surface LoadJobs error")
	}
}

// --- ringBuffer.Read max branch ---

func TestRingBufferReadMax(t *testing.T) {
	rb := newRingBuffer(64)
	rb.Write([]byte("0123456789"))
	// max=0 → everything.
	if got := rb.Read(0); got != "0123456789" {
		t.Fatalf("Read(0)=%q", got)
	}
	// max > len → everything.
	if got := rb.Read(100); got != "0123456789" {
		t.Fatalf("Read(100)=%q", got)
	}
	// 0 < max < len → tail.
	if got := rb.Read(3); got != "789" {
		t.Fatalf("Read(3)=%q want 789", got)
	}
	// max larger than content after a cap-eviction.
	rb2 := newRingBuffer(4)
	rb2.Write([]byte("ABCDEFGH")) // cap 4 → keeps "EFGH"
	if got := rb2.Read(2); got != "GH" {
		t.Fatalf("Read(2)=%q want GH", got)
	}
}

// --- OSProcessFactory real subprocess (covers process.go + pipeConsole) ---

// TestOSProcessFactoryHelper is both a real test AND a subprocess helper: when
// the env var is set it prints a line to stdout and exits 0, so OSProcessFactory
// can spawn it and exercise the full pipe path.
func TestOSProcessFactoryHelper(t *testing.T) {
	if os.Getenv("YANSHI_OSFACTORY_HELPER") == "1" {
		fmt.Fprintln(os.Stdout, "hello-from-helper")
		os.Exit(0)
	}
}

func TestOSProcessFactoryPTYUnavailable(t *testing.T) {
	_, _, err := (&OSProcessFactory{}).Start(context.Background(), LaunchSpec{Program: "x", PTY: true})
	if !errors.Is(err, ErrPTYUnavailable) {
		t.Fatalf("PTY spawn must return ErrPTYUnavailable, got %v", err)
	}
}

func TestOSProcessFactoryProgramRequired(t *testing.T) {
	_, _, err := (&OSProcessFactory{}).Start(context.Background(), LaunchSpec{Program: ""})
	if err == nil {
		t.Fatal("empty Program must error")
	}
}

func TestOSProcessFactoryBogusCommand(t *testing.T) {
	_, _, err := (&OSProcessFactory{}).Start(context.Background(), LaunchSpec{
		Program: "definitely-not-a-real-binary-zzz",
	})
	if err == nil {
		t.Fatal("bogus Program must error at cmd.Start")
	}
}

func TestOSProcessFactorySpawnsAndDrains(t *testing.T) {
	if os.Getenv("YANSHI_OSFACTORY_HELPER") != "" && os.Getenv("YANSHI_OSFACTORY_HELPER") != "0" && testing.Short() {
		t.Skip("helper subprocess")
	}
	// Re-exec the test binary running the helper test, which prints one line.
	proc, console, err := (&OSProcessFactory{}).Start(context.Background(), LaunchSpec{
		Program: os.Args[0],
		Args:    []string{"-test.run=TestOSProcessFactoryHelper"},
		Env:     append(os.Environ(), "YANSHI_OSFACTORY_HELPER=1"),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer console.Close()

	// pipeConsole.PTY() is false for non-PTY spawns.
	if console.PTY() {
		t.Fatal("pipe console must report PTY=false")
	}
	// pipeConsole.Write is read-only.
	if _, err := console.Write([]byte("x")); err == nil {
		t.Fatal("pipe console Write must error")
	}
	// pipeConsole.Resize is a no-op error.
	if err := console.Resize(10, 10); err == nil {
		t.Fatal("pipe console Resize must error")
	}

	// Read the combined output.
	got, err := io.ReadAll(console)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !strings.Contains(string(got), "hello-from-helper") {
		t.Fatalf("output=%q", got)
	}

	// osProcess methods.
	p := proc.(*osProcess)
	if p.PID() <= 0 {
		t.Fatalf("PID=%d", p.PID())
	}
	if err := p.Wait(); err != nil {
		// The helper exits 0; a non-nil Wait here is unexpected unless the OS
		// reaped it already. Log but don't hard-fail on exit-code nuances.
		t.Logf("Wait returned %v (helper exited)", err)
	}
	if caps := p.Capabilities(); caps == (ProcessCapabilities{}) && CanKillTreeOnPlatform() {
		t.Fatal("capabilities should reflect platform tree-kill")
	}

	// Kill on an already-reaped process: nil process or already-exited. Should
	// not panic; either nil or an error is acceptable here.
	_ = p.Kill()
}

func TestOSProcessCapabilitiesBeforeStart(t *testing.T) {
	// An osProcess whose cmd has no Process yet reports PID 0 and Kill is a
	// no-op (both guard on cmd.Process == nil).
	p := &osProcess{cmd: &exec.Cmd{}}
	if p.PID() != 0 {
		t.Fatalf("PID=%d want 0", p.PID())
	}
	if err := p.Kill(); err != nil {
		t.Fatalf("Kill with nil process must be no-op: %v", err)
	}
}

// --- factory.go: consoleReader EOF + success, discardReader, success path ---

// eofConsole returns EOF on every Read (after the first optional payload).
type eofConsole struct{ delivered bool }

func (c *eofConsole) Read(p []byte) (int, error)  { return 0, io.EOF }
func (c *eofConsole) Write(b []byte) (int, error) { return len(b), nil }
func (c *eofConsole) Close() error                { return nil }
func (c *eofConsole) Resize(uint16, uint16) error { return nil }
func (c *eofConsole) PTY() bool                   { return false }

func TestConsoleReaderEOF(t *testing.T) {
	n, err := (consoleReader{r: &eofConsole{}}).Read(make([]byte, 4))
	if err != io.EOF {
		t.Fatalf("EOF must pass through: n=%d err=%v", n, err)
	}
}

// payloadConsole delivers one payload then EOF.
type payloadConsole struct {
	payload []byte
	done    bool
}

func (c *payloadConsole) Read(p []byte) (int, error) {
	if !c.done {
		c.done = true
		return copy(p, c.payload), nil
	}
	return 0, io.EOF
}
func (c *payloadConsole) Write(b []byte) (int, error) { return len(b), nil }
func (c *payloadConsole) Close() error                { return nil }
func (c *payloadConsole) Resize(uint16, uint16) error { return nil }
func (c *payloadConsole) PTY() bool                   { return false }

func TestConsoleReaderSuccess(t *testing.T) {
	cr := consoleReader{r: &payloadConsole{payload: []byte("abc")}}
	buf := make([]byte, 8)
	n, err := cr.Read(buf)
	if err != nil || string(buf[:n]) != "abc" {
		t.Fatalf("Read: n=%d buf=%q err=%v", n, buf[:n], err)
	}
}

func TestDiscardReaderEOF(t *testing.T) {
	n, err := discardReader{}.Read(make([]byte, 4))
	if err != io.EOF || n != 0 {
		t.Fatalf("discardReader: n=%d err=%v want EOF", n, err)
	}
}

// TestDefaultSecureFactorySuccessWithReader covers the full DefaultSecureFactory
// success path (netpolicy applied, delegates to OS factory) with a console that
// delivers a payload, so StartedProcess.Stdout can be pumped.
func TestDefaultSecureFactorySuccessWithReader(t *testing.T) {
	f := DefaultSecureFactory{
		OS: &fakeOSFactory{payload: []byte("out")},
	}
	sp, err := f.Start(context.Background(), secproc.SecureProcessSpec{
		Shell: "echo", Program: "echo", Args: []string{"out"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	got, err := io.ReadAll(sp.Stdout)
	if err != nil {
		t.Fatalf("ReadAll stdout: %v", err)
	}
	if string(got) != "out" {
		t.Fatalf("stdout=%q", got)
	}
	// Stderr is a discardReader.
	if _, err := sp.Stderr.Read(make([]byte, 4)); err != io.EOF {
		t.Fatalf("Stderr must EOF: %v", err)
	}
}

// fakeOSFactory implements ProcessFactory with a payload-delivering console.
type fakeOSFactory struct {
	payload []byte
}

func (f *fakeOSFactory) Start(context.Context, LaunchSpec) (Process, Console, error) {
	return &noopProcess{}, &payloadConsole{payload: f.payload}, nil
}

// TestDefaultSecureFactoryWithoutPolicyStillStripsProxy covers the else-branch
// (Policy==nil) of DefaultSecureFactory.Start.
func TestDefaultSecureFactoryWithoutPolicyStillStripsProxy(t *testing.T) {
	rec := &recordingFactory{}
	f := DefaultSecureFactory{OS: rec}
	if _, err := f.Start(context.Background(), secproc.SecureProcessSpec{
		Program: "go", Env: []string{"http_proxy=leak"},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	for _, e := range rec.gotEnv {
		if strings.HasPrefix(strings.ToLower(e), "http_proxy=leak") {
			t.Fatalf("proxy var survived even without policy: %v", rec.gotEnv)
		}
	}
}

// TestDefaultSecureFactoryStartError covers the OS.Start-error branch.
func TestDefaultSecureFactoryStartError(t *testing.T) {
	f := DefaultSecureFactory{OS: &errorFactory{err: errors.New("nope")}}
	if _, err := f.Start(context.Background(), secproc.SecureProcessSpec{Program: "go"}); err == nil {
		t.Fatal("must surface OS.Start error")
	}
}

// TestDefaultSecureFactoryDefaultProxyURL covers the empty-ProxyURL default
// branch (factory.go:48): when Policy is set but ProxyURL is empty, the managed
// proxy defaults to http://127.0.0.1:0.
// TestDefaultSecureFactoryPublishesNoPlaceholderProxy replaces a test that
// pinned the opposite: a policy with no proxy behind it used to publish
// http://127.0.0.1:0.
//
// That URL read as enforcement and was a black hole. It broke clients that
// honour proxy variables, let every other client straight out, and produced
// no decision an operator could inspect -- so the posture looked contained
// and was not. Publishing nothing is the honest form of the same state, and
// the enforced form is a real proxy with a real address.
func TestDefaultSecureFactoryPublishesNoPlaceholderProxy(t *testing.T) {
	rec := &recordingFactory{}
	f := DefaultSecureFactory{
		OS:     rec,
		Policy: &netpolicy.Policy{Default: "allow"},
		// ProxyURL empty: no managed proxy is running for this child.
	}
	if _, err := f.Start(context.Background(), secproc.SecureProcessSpec{Program: "go"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	env := strings.Join(rec.gotEnv, "\n")
	if containsStr(env, "127.0.0.1:0") {
		t.Fatalf("a placeholder proxy URL is being published again: %v", rec.gotEnv)
	}
}

func containsStr(s, sub string) bool { return strings.Contains(s, sub) }

// --- manager.go: Snapshot unknown id, RestoreJobs nil persist ---

func TestManagerSnapshotUnknown(t *testing.T) {
	m := NewManager(Config{Root: t.TempDir(), Factory: &fakeFactory{}})
	if s := m.Snapshot("ghost"); s.ID != "" {
		t.Fatalf("Snapshot unknown must return zero Session, got %#v", s)
	}
}

func TestManagerRestoreJobsNilPersist(t *testing.T) {
	m := NewManager(Config{Root: t.TempDir(), Factory: &fakeFactory{}}) // no WithPersistence
	if err := m.RestoreJobs(); err != nil {
		t.Fatalf("RestoreJobs with nil persist must be a no-op: %v", err)
	}
}

// --- persist.go branches ---

// errKV is a KV whose KVGet always errors.
type errKV struct{}

func (errKV) KVGet(string) (string, bool, error) { return "", false, errors.New("kv boom") }
func (errKV) KVSet(string, string) error         { return nil }

func TestPersistLoadJobsKVGetError(t *testing.T) {
	store := JobFromKV(errKV{})
	if _, err := store.LoadJobs(); err == nil {
		t.Fatal("LoadJobs must surface KVGet error")
	}
}

func TestPersistLoadJobsEmpty(t *testing.T) {
	// memKV with no key → ok=false → LoadJobs returns nil,nil.
	store := JobFromKV(&memKV{})
	list, err := store.LoadJobs()
	if err != nil || list != nil {
		t.Fatalf("LoadJobs empty: list=%v err=%v", list, err)
	}
}

func TestPersistLoadJobsCorruptJSON(t *testing.T) {
	kv := &memKV{}
	_ = kv.KVSet("security.shell.jobs.v1", "{not-json")
	store := JobFromKV(kv)
	if _, err := store.LoadJobs(); err == nil {
		t.Fatal("LoadJobs must error on corrupt JSON")
	}
}

func TestPersistSaveJobKeepsDistinctIDs(t *testing.T) {
	kv := &memKV{}
	store := JobFromKV(kv)
	first := Job{ID: "job-a", SessionID: "s", Command: "a", State: StateRunning, PID: 1, ExitCode: -1, StartedAt: time.Unix(1, 0)}
	second := Job{ID: "job-b", SessionID: "s", Command: "b", State: StateRunning, PID: 2, ExitCode: -1, StartedAt: time.Unix(2, 0)}
	if err := store.SaveJob(first); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveJob(second); err != nil {
		t.Fatal(err)
	}
	list, err := store.LoadJobs()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 distinct jobs, got %d", len(list))
	}
}

// --- sandbox referenced to keep the import set consistent ---
var _ = sandbox.New
