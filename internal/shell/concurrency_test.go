package shell

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// blockingFactory hands out processes that never exit on their own, so a test
// controls exactly how many slots are occupied.
type blockingFactory struct {
	mu    sync.Mutex
	procs []*blockingProcess
	fail  error
	// starts counts spawn ATTEMPTS that got past the concurrency gate, which
	// is what distinguishes "queued" from "rejected".
	starts int64
}

func (f *blockingFactory) Start(context.Context, LaunchSpec) (Process, Console, error) {
	atomic.AddInt64(&f.starts, 1)
	if f.fail != nil {
		return nil, nil, f.fail
	}
	proc := &blockingProcess{console: newBlockingConsole(), done: make(chan error, 1)}
	f.mu.Lock()
	f.procs = append(f.procs, proc)
	f.mu.Unlock()
	return proc, proc.console, nil
}

// blockingProcess is a process that runs until the test ends it, and that ends
// its console when it does.
//
// The coupling is the point rather than an artefact: killing a real process
// closes the pipe the pump is reading, which is what lets pump reach
// Process.Wait and release the concurrency slot. A fake that kills the process
// without ending the console models a shape the OS cannot produce, and it
// deadlocks Manager.Close against the pump it is blocking.
type blockingProcess struct {
	console *blockingConsole
	done    chan error
}

func (p *blockingProcess) Wait() error { return <-p.done }
func (p *blockingProcess) PID() int    { return 1 }
func (p *blockingProcess) Kill() error { p.exit(nil); return nil }
func (p *blockingProcess) Capabilities() ProcessCapabilities {
	return ProcessCapabilities{CanKillTree: true}
}

func (p *blockingProcess) exit(err error) {
	p.console.unblock()
	select {
	case p.done <- err:
	default:
	}
}

// blockingConsole holds pump's Read open until the session ends, so a test
// controls exactly how long a concurrency slot stays occupied.
//
// It cannot be fakeConsole: that one is unblocked by CLOSING its channel, and
// the only thing that closes a live session's console is pump itself — after
// Read returns. A console that never returns from Read therefore deadlocks
// Manager.Close, which waits on the pump it is blocking.
type blockingConsole struct {
	release chan struct{}
	once    sync.Once
}

func newBlockingConsole() *blockingConsole {
	return &blockingConsole{release: make(chan struct{})}
}

func (c *blockingConsole) Read([]byte) (int, error) {
	<-c.release
	return 0, io.EOF
}
func (c *blockingConsole) Write(p []byte) (int, error) { return len(p), nil }
func (c *blockingConsole) Close() error                { c.unblock(); return nil }
func (c *blockingConsole) Resize(uint16, uint16) error { return nil }
func (c *blockingConsole) PTY() bool                   { return false }
func (c *blockingConsole) unblock()                    { c.once.Do(func() { close(c.release) }) }

// finish ends the i-th started session the way the OS would.
func (f *blockingFactory) finish(t *testing.T, i int) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.procs) {
		t.Fatalf("no process %d started (have %d)", i, len(f.procs))
	}
	f.procs[i].exit(nil)
}

func (f *blockingFactory) startCount() int64 { return atomic.LoadInt64(&f.starts) }

// TestManagerQueuesOverTheConcurrencyCap is W-B-22's acceptance. The third
// start must WAIT, not fail: exceeding a resource ceiling is not a security
// event, and a refusal is a task the model has to notice and retry.
func TestManagerQueuesOverTheConcurrencyCap(t *testing.T) {
	factory := &blockingFactory{}
	m := NewManager(Config{Root: t.TempDir(), MaxConcurrent: 2, Factory: factory})
	t.Cleanup(func() { _ = m.Close() })

	spec := LaunchSpec{Program: "sh", Args: []string{"-c", "sleep"}}
	for i := 0; i < 2; i++ {
		if _, err := m.Start(context.Background(), spec); err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
	}

	third := make(chan error, 1)
	go func() {
		_, err := m.Start(context.Background(), spec)
		third <- err
	}()

	// The queued start must not have reached the factory yet.
	select {
	case err := <-third:
		t.Fatalf("the third start returned %v instead of queueing", err)
	case <-time.After(150 * time.Millisecond):
	}
	if n := factory.startCount(); n != 2 {
		t.Fatalf("factory saw %d spawns while the cap was full, want 2", n)
	}

	// Freeing one slot must let it through.
	factory.finish(t, 0)
	select {
	case err := <-third:
		if err != nil {
			t.Fatalf("the queued start failed after a slot freed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the queued start never proceeded after a session exited")
	}
	if n := factory.startCount(); n != 3 {
		t.Fatalf("factory saw %d spawns, want 3", n)
	}
}

// TestQueuedStartIsBoundedByTheCallersDeadline pins the other half of the
// queueing decision: the wait uses the CALLER's context, so a queue that never
// drains surfaces as that tool call's timeout rather than as a hang.
//
// The session lifecycle is context.WithoutCancel by design, so waiting on THAT
// context instead would make an over-cap start unbounded — this test is what
// goes red if the two are ever confused.
func TestQueuedStartIsBoundedByTheCallersDeadline(t *testing.T) {
	factory := &blockingFactory{}
	m := NewManager(Config{Root: t.TempDir(), MaxConcurrent: 1, Factory: factory})
	t.Cleanup(func() { _ = m.Close() })

	if _, err := m.Start(context.Background(), LaunchSpec{Program: "sh"}); err != nil {
		t.Fatalf("first start: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := m.Start(ctx, LaunchSpec{Program: "sh"})
	if err == nil {
		t.Fatal("a queued start succeeded past a full cap")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want it to wrap context.DeadlineExceeded", err)
	}
	if !strings.Contains(err.Error(), "concurrency") {
		t.Fatalf("error does not say why it waited: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Fatalf("returned after %v; it did not wait, it refused", elapsed)
	}
	if n := factory.startCount(); n != 1 {
		t.Fatalf("factory saw %d spawns, want 1", n)
	}
}

// TestConcurrencySlotIsReleasedWhenTheSpawnFails guards the leak: a factory
// error after the slot was taken must give it back, or N failed starts
// permanently shrink the cap to zero.
func TestConcurrencySlotIsReleasedWhenTheSpawnFails(t *testing.T) {
	factory := &blockingFactory{fail: errors.New("boom")}
	m := NewManager(Config{Root: t.TempDir(), MaxConcurrent: 1, Factory: factory})
	t.Cleanup(func() { _ = m.Close() })

	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err := m.Start(ctx, LaunchSpec{Program: "sh"})
		cancel()
		if err == nil || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("start %d: err = %v, want the factory's error (a leaked slot shows up "+
				"here as a deadline instead)", i, err)
		}
	}
	if n := factory.startCount(); n != 3 {
		t.Fatalf("factory saw %d spawns, want 3 — a slot was leaked", n)
	}
}

// TestZeroMaxConcurrentIsUncapped pins the default. A Manager built without
// thinking about this field must behave exactly as it did before the field
// existed.
func TestZeroMaxConcurrentIsUncapped(t *testing.T) {
	factory := &blockingFactory{}
	m := NewManager(Config{Root: t.TempDir(), Factory: factory})
	t.Cleanup(func() { _ = m.Close() })
	if m.slots != nil {
		t.Fatal("MaxConcurrent=0 built a gate")
	}
	for i := 0; i < 8; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, err := m.Start(ctx, LaunchSpec{Program: "sh"})
		cancel()
		if err != nil {
			t.Fatalf("start %d under an uncapped manager: %v", i, err)
		}
	}
}

// TestCanceledSessionReturnsItsSlot covers the path that is not a clean exit.
// Cancel is one of several ways a process ends and pump is where they all
// converge, which is why the release lives there and not in Cancel.
func TestCanceledSessionReturnsItsSlot(t *testing.T) {
	factory := &blockingFactory{}
	m := NewManager(Config{Root: t.TempDir(), MaxConcurrent: 1, Factory: factory})
	t.Cleanup(func() { _ = m.Close() })

	sess, err := m.Start(context.Background(), LaunchSpec{Program: "sh"})
	if err != nil {
		t.Fatalf("first start: %v", err)
	}
	if err := m.Cancel(sess.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := m.Start(ctx, LaunchSpec{Program: "sh"}); err != nil {
		t.Fatalf("a canceled session did not return its slot: %v", err)
	}
}
