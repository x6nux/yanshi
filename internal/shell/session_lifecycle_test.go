package shell

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestReadNewIsIncrementalNotOverlapping covers the resumable-read clause.
//
// shell_read took only id and max_bytes, and Manager.Read returned the TAIL of
// the ring buffer. Two polls of a running command therefore returned
// overlapping data: the caller re-read what it had already seen and had no way
// to tell new output from old. The breakdown named the trap — a single read
// asserting Contains(marker) passes on exactly that implementation.
//
// ledger: A1/T07/T08#2 可续读/stdin
func TestReadNewIsIncrementalNotOverlapping(t *testing.T) {
	rb := newRingBuffer(1 << 20)

	if _, err := rb.Write([]byte("first ")); err != nil {
		t.Fatal(err)
	}
	got1, skipped := rb.ReadNew(0)
	if skipped != 0 {
		t.Errorf("nothing was dropped yet, but skipped=%d", skipped)
	}
	if got1 != "first " {
		t.Fatalf("first read = %q, want %q", got1, "first ")
	}

	if _, err := rb.Write([]byte("second")); err != nil {
		t.Fatal(err)
	}
	got2, _ := rb.ReadNew(0)
	if got2 != "second" {
		t.Errorf("second read = %q, want only the new bytes", got2)
	}
	if strings.Contains(got2, "first") {
		t.Error("the second read repeated data the first already returned")
	}

	// Nothing new: an empty answer, not the tail again.
	got3, _ := rb.ReadNew(0)
	if got3 != "" {
		t.Errorf("a read with no new output returned %q", got3)
	}

	// The tail form still exists for callers that ask for it, and still
	// overlaps — that is its contract, not a bug.
	if tail := rb.Read(0); !strings.Contains(tail, "first") {
		t.Errorf("Read(0) no longer returns the whole buffer: %q", tail)
	}
}

// TestReadNewReportsBytesLostToTheCap is the honesty half.
//
// Output produced and pushed out of the ring buffer between two polls is gone.
// Returning the survivors silently makes a truncated stream look complete; the
// caller needs to know a gap exists to decide whether the answer is usable.
//
// ledger: A1/T07/T08#3 yield/timeout/输出上限/显式关闭
func TestReadNewReportsBytesLostToTheCap(t *testing.T) {
	rb := newRingBuffer(10)

	if _, err := rb.Write([]byte("0123456789")); err != nil {
		t.Fatal(err)
	}
	// Overflow: the head is trimmed before anybody read it.
	if _, err := rb.Write([]byte("ABCDEFGHIJ")); err != nil {
		t.Fatal(err)
	}

	got, skipped := rb.ReadNew(0)
	if skipped != 10 {
		t.Errorf("skipped=%d, want 10: the caller cannot tell a gap from a quiet period", skipped)
	}
	if got != "ABCDEFGHIJ" {
		t.Errorf("got %q, want the surviving tail", got)
	}
	// The cap is a cap: the buffer never grows past it.
	if n := len(rb.Read(0)); n > 10 {
		t.Errorf("buffer holds %d bytes with a cap of 10", n)
	}
}

// TestReapIdleReclaimsSessionsAndTheirProcesses covers the reclamation clause.
//
// Config.IdleTimeout had exactly one consumer: the time.After branch inside
// Manager.Wait, which bounds how long a CALLER waits and leaves the session in
// the map with its process running. So "sessions are reclaimed by policy"
// described a timeout that reclaimed nothing, and a client that started
// sessions and stopped reading them leaked a process each time.
//
// The assertion is on both halves — gone from the map AND the process actually
// dead — because a bookkeeping delete that orphaned the process would satisfy
// the first alone, which is the worse outcome of the two.
//
// now is passed in rather than slept for: waiting out a real idle timeout makes
// the test slow and flaky without making it stronger.
//
// ledger: A1/T07/T08#5 session 关闭按策略回收
func TestReapIdleReclaimsSessionsAndTheirProcesses(t *testing.T) {
	m := NewManager(Config{
		Factory:     OSProcessFactory{},
		IdleTimeout: time.Minute,
	})

	sess, err := m.Start(context.Background(), LaunchSpec{
		Program: "sh", Args: []string{"-c", "sleep 300"}, Dir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	pid := sess.PID
	if pid == 0 {
		t.Fatal("the session has no pid, so there is nothing to reclaim")
	}
	if !processAlive(pid) {
		t.Fatal("the process was never running")
	}

	// Not yet idle.
	if reaped := m.ReapIdle(time.Now()); len(reaped) != 0 {
		t.Fatalf("a freshly started session was reaped: %v", reaped)
	}
	if _, err := m.Read(sess.ID, 0); err != nil {
		t.Fatalf("the session should still be readable: %v", err)
	}

	// Idle past the timeout.
	reaped := m.ReapIdle(time.Now().Add(2 * time.Minute))
	if len(reaped) != 1 || reaped[0] != sess.ID {
		t.Fatalf("reaped %v, want [%s]", reaped, sess.ID)
	}
	if _, err := m.Read(sess.ID, 0); err == nil {
		t.Error("the session is still in the map after being reaped")
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = syscallKill(pid)
	t.Errorf("the session was removed from the map but its process (pid %d) is still "+
		"running: a bookkeeping delete that orphans the process is worse than no reaper",
		pid)
}

// TestReapIdleDoesNothingWithoutAPolicy guards the default.
//
// A zero IdleTimeout means "no policy". A reaper that invented one would kill
// sessions the operator expected to keep, which is a far worse failure than the
// leak it fixes.
func TestReapIdleDoesNothingWithoutAPolicy(t *testing.T) {
	m := NewManager(Config{Factory: OSProcessFactory{}}) // no IdleTimeout
	sess, err := m.Start(context.Background(), LaunchSpec{
		Program: "sh", Args: []string{"-c", "sleep 300"}, Dir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Cancel(sess.ID) })

	if reaped := m.ReapIdle(time.Now().Add(24 * time.Hour)); len(reaped) != 0 {
		t.Errorf("sessions were reaped with no idle policy configured: %v", reaped)
	}
	if _, err := m.Read(sess.ID, 0); err != nil {
		t.Errorf("the session was removed with no policy configured: %v", err)
	}
}

// TestActivityKeepsASessionAlive pins what "idle" means.
//
// A long silent build produces no output for minutes and is not idle: the
// caller is waiting on it. Idleness has to be measured from caller activity,
// not from the output stream, or the reaper kills exactly the sessions worth
// keeping.
//
// ledger: A1/T07/T08#5 session 关闭按策略回收
func TestActivityKeepsASessionAlive(t *testing.T) {
	m := NewManager(Config{Factory: OSProcessFactory{}, IdleTimeout: time.Minute})
	sess, err := m.Start(context.Background(), LaunchSpec{
		Program: "sh", Args: []string{"-c", "sleep 300"}, Dir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Cancel(sess.ID) })

	// A read at base+2min refreshes the clock; the reap at base+2min30s is
	// half the timeout after the last touch.
	//
	// One base for both instants, deliberately: calling time.Now() twice puts
	// microseconds between them, and with an exactly-one-minute gap that is
	// enough to cross the boundary and make this test flaky for a reason that
	// has nothing to do with the reaper.
	base := time.Now()
	m.mu.Lock()
	live := m.sessions[sess.ID]
	m.mu.Unlock()
	live.touch(base.Add(2 * time.Minute))

	if reaped := m.ReapIdle(base.Add(2*time.Minute + 30*time.Second)); len(reaped) != 0 {
		t.Errorf("a session touched 30s ago was reaped with a one-minute timeout: %v", reaped)
	}
	// And it IS reaped once genuinely idle, or the assertion above would hold
	// for a reaper that never collects anything.
	if reaped := m.ReapIdle(base.Add(4 * time.Minute)); len(reaped) != 1 {
		t.Errorf("a session idle for two minutes was not reaped: %v", reaped)
	}
}

// TestWaitForYieldsWithoutKillingTheSession covers the yield clause.
//
// shell_wait had no timeout parameter, so a model waiting on a long build had
// two options: block until the tool's own 130s deadline killed the CALL, or not
// wait at all. Neither lets it check in and go do something else, which is what
// yield means.
//
// The critical half is that timing out leaves the session alone. A wait that
// reclaimed on expiry would make "is it done yet?" destructive — and the
// caller, seeing a timeout, would have no way to know it had just killed the
// thing it was waiting for.
//
// ledger: A1/T07/T08#3 yield/timeout/输出上限/显式关闭
func TestWaitForYieldsWithoutKillingTheSession(t *testing.T) {
	m := NewManager(Config{Factory: OSProcessFactory{}})
	sess, err := m.Start(context.Background(), LaunchSpec{
		Program: "sh", Args: []string{"-c", "sleep 300"}, Dir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Cancel(sess.ID) })
	pid := sess.PID

	start := time.Now()
	_, err = m.WaitFor(context.Background(), sess.ID, 150*time.Millisecond)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a 300-second sleep finished within 150ms")
	}
	if !errors.Is(err, ErrWaitTimeout) {
		t.Fatalf("got %v, want ErrWaitTimeout: a caller cannot distinguish a yield from a "+
			"real failure without a distinct error", err)
	}
	if elapsed > 5*time.Second {
		t.Errorf("the wait took %v for a 150ms bound", elapsed)
	}

	// The session must be untouched.
	if !processAlive(pid) {
		t.Error("the timed-out wait killed the process: checking on a build must not end it")
	}
	if _, rerr := m.Read(sess.ID, 0); rerr != nil {
		t.Errorf("the session was removed by a timed-out wait: %v", rerr)
	}

	// And a second wait still works — the yield left it waitable.
	if _, err := m.WaitFor(context.Background(), sess.ID, 50*time.Millisecond); !errors.Is(err, ErrWaitTimeout) {
		t.Errorf("the session is no longer waitable after a yield: %v", err)
	}
}

// TestWaitForReturnsTheSessionWhenItFinishes is the positive control.
//
// Without it, "wait times out" would hold for a WaitFor that never returns
// successfully at all.
//
// ledger: A1/T07/T08#3 yield/timeout/输出上限/显式关闭
func TestWaitForReturnsTheSessionWhenItFinishes(t *testing.T) {
	m := NewManager(Config{Factory: OSProcessFactory{}})
	sess, err := m.Start(context.Background(), LaunchSpec{
		Program: "sh", Args: []string{"-c", "exit 3"}, Dir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	done, err := m.WaitFor(context.Background(), sess.ID, 30*time.Second)
	if err != nil {
		t.Fatalf("WaitFor: %v", err)
	}
	if done.ExitCode != 3 {
		t.Errorf("exit code %d, want 3 — the wait returned before the process did", done.ExitCode)
	}
}

// TestStdinRoundTripsThroughTheSession covers the stdin half of the clause.
//
// shell_write_stdin existed and had no test that anything WRITTEN came back
// out. A write that returns a byte count and goes nowhere is indistinguishable
// from a working one at the call site; the only way to tell is to make the
// child echo it.
//
// `cat` is the whole fixture: it copies stdin to stdout and nothing else, so a
// marker appearing in the output can only have come through the pipe under
// test.
//
// ledger: A1/T07/T08#2 可续读/stdin
func TestStdinRoundTripsThroughTheSession(t *testing.T) {
	m := NewManager(Config{Factory: OSProcessFactory{}})
	sess, err := m.Start(context.Background(), LaunchSpec{
		Program: "cat", Dir: t.TempDir(),
	})
	if err != nil {
		t.Skipf("cat is unavailable: %v", err)
	}
	t.Cleanup(func() { _ = m.Cancel(sess.ID) })

	const marker = "STDIN_ROUNDTRIP_MARKER\n"
	n, err := m.Write(sess.ID, []byte(marker))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != len(marker) {
		t.Errorf("wrote %d of %d bytes", n, len(marker))
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		out, _ := m.Read(sess.ID, 0)
		if strings.Contains(out, "STDIN_ROUNDTRIP_MARKER") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	out, _ := m.Read(sess.ID, 0)
	t.Errorf("what was written to stdin never came back out; the session output is %q", out)
}
