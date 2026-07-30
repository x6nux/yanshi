package shell

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"sync"
	"time"
)

// Config tunes the Manager. Factory is REQUIRED (nil returns errors from
// Start); the OS-specific default lives in process.go (Task 18).
type Config struct {
	Root           string
	MaxOutputBytes int
	IdleTimeout    time.Duration
	Factory        ProcessFactory
	Logger         *log.Logger // optional; defaults to log.Default()
}

// Manager owns the set of live shell sessions and the persisted job table.
// Sessions outlive the tool context that started them (the lifecycle context
// is `context.WithoutCancel(ctx)`), so a shell_start call can return while
// the spawned process keeps running. The Manager is goroutine-safe.
type Manager struct {
	cfg      Config
	mu       sync.Mutex
	sessions map[string]*liveSession
	jobs     map[string]*liveJob
	persist  PersistStore
}

// liveSession is the Manager's internal per-session state. The mu protects
// meta + finished; the other fields are write-once at construction (or
// written only by the pump goroutine after close(done) — wait/done/cancel are
// safe to read concurrently because they are never reassigned).
type liveSession struct {
	mu        sync.Mutex
	meta      Session
	console   Console
	process   Process
	lifecycle context.Context
	cancel    context.CancelFunc
	output    *ringBuffer
	done      chan struct{} // closed once Process.Wait returns
	finished  bool
}

// liveJob is the Manager's internal per-job state. session is set at
// StartJob time and never mutated afterwards; meta is mutable under mu.
type liveJob struct {
	mu      sync.Mutex
	meta    Job
	session *liveSession
}

// NewManager builds a Manager with sensible defaults (1 MiB ring buffer when
// unset). Callers MUST supply Config.Factory; passing nil makes Start return
// an explicit error rather than silently doing nothing.
func NewManager(cfg Config) *Manager {
	if cfg.MaxOutputBytes == 0 {
		cfg.MaxOutputBytes = 1 << 20
	}
	return &Manager{
		cfg:      cfg,
		sessions: make(map[string]*liveSession),
		jobs:     make(map[string]*liveJob),
	}
}

// WithPersistence attaches a PersistStore so the Manager can flush jobs at
// shutdown and reload them at boot. Returns m for chaining.
func (m *Manager) WithPersistence(store PersistStore) *Manager {
	m.persist = store
	return m
}

// Start launches a persistent shell session. The session's lifecycle is
// decoupled from ctx via context.WithoutCancel: when the tool ctx that started
// the session is canceled (e.g. shell_start returns), the session keeps
// running. Use Wait/Cancel to observe or terminate it.
func (m *Manager) Start(ctx context.Context, spec LaunchSpec) (*Session, error) {
	if m.cfg.Factory == nil {
		return nil, fmt.Errorf("shell: no process factory configured")
	}
	lifecycle, cancel := context.WithCancel(context.WithoutCancel(ctx))
	proc, console, err := m.cfg.Factory.Start(lifecycle, spec)
	if err != nil {
		cancel()
		return nil, err
	}
	id := fmt.Sprintf("session-%d-%d", time.Now().UnixNano(), proc.PID())
	sess := &liveSession{
		meta: Session{
			ID:        id,
			PID:       proc.PID(),
			Command:   spec.Command,
			State:     StateRunning,
			ExitCode:  -1,
			PTY:       console.PTY(),
			StartedAt: time.Now().UTC(),
		},
		console:   console,
		process:   proc,
		lifecycle: lifecycle,
		cancel:    cancel,
		output:    newRingBuffer(m.cfg.MaxOutputBytes),
		done:      make(chan struct{}),
	}
	// Bind the Session's MarshalJSON lock to liveSession.mu so json.Marshal of
	// a Session (e.g. /jobs, /sessions rendering) is serialized against pump()
	// writing State/ExitCode/EndedAt under the same mutex. Without this,
	// meta.mu is nil and MarshalJSON skips the lock — a DATA RACE between
	// reflect-driven field reads and pump's writes.
	sess.meta.mu = &sess.mu
	m.mu.Lock()
	m.sessions[id] = sess
	m.mu.Unlock()
	go sess.pump()
	return &sess.meta, nil
}

// StartJob wraps Start to also register a Job under the Manager's job table
// (which /jobs renders against). The job is persisted if WithPersistence was
// called. Returns the Job by value so callers can read ID/SessionID without
// holding a Manager lock.
func (m *Manager) StartJob(ctx context.Context, command string, spec LaunchSpec) (*Job, error) {
	sess, err := m.Start(ctx, spec)
	if err != nil {
		return nil, err
	}
	job := &Job{
		ID:        fmt.Sprintf("job-%s", sess.ID),
		SessionID: sess.ID,
		Command:   command,
		State:     StateRunning,
		ExitCode:  -1,
		PID:       sess.PID,
		StartedAt: sess.StartedAt,
	}
	m.mu.Lock()
	m.jobs[job.ID] = &liveJob{meta: *job, session: m.sessions[sess.ID]}
	m.mu.Unlock()
	if m.persist != nil {
		_ = m.persist.SaveJob(*job)
	}
	return job, nil
}

// Snapshot returns a copy of the session's current metadata. Returns the zero
// Session when id is unknown (callers can check ID == "" if they care).
func (m *Manager) Snapshot(id string) Session {
	m.mu.Lock()
	s := m.sessions[id]
	m.mu.Unlock()
	if s == nil {
		return Session{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.meta
}

// Read returns up to max bytes of the session's accumulated output. max <= 0
// means "as much as the ring buffer currently holds".
func (m *Manager) Read(id string, max int) (string, error) {
	m.mu.Lock()
	s := m.sessions[id]
	m.mu.Unlock()
	if s == nil {
		return "", ErrNotFound
	}
	return s.output.Read(max), nil
}

// ReadJob mirrors Read but takes a job id (resolves to its owning session).
func (m *Manager) ReadJob(id string, max int) (string, error) {
	m.mu.Lock()
	j := m.jobs[id]
	m.mu.Unlock()
	if j == nil {
		return "", ErrNotFound
	}
	if j.session == nil {
		return "", ErrNotFound
	}
	return j.session.output.Read(max), nil
}

// Write sends bytes to the session's stdin (PTY/pipe depending on Console).
func (m *Manager) Write(id string, data []byte) (int, error) {
	m.mu.Lock()
	s := m.sessions[id]
	m.mu.Unlock()
	if s == nil {
		return 0, ErrNotFound
	}
	return s.console.Write(data)
}

// Wait blocks until the session exits or ctx is canceled. The ctx-aware path
// is the real fix for review C8: Wait no longer holds the session hostage when
// the caller's deadline passes. Idle timeout (Config.IdleTimeout) is integrated
// into the main select so it fires while the process is still running — the
// previous gating behind s.done made it dead code (s.done was already closed).
func (m *Manager) Wait(ctx context.Context, id string) (*Session, error) {
	m.mu.Lock()
	s := m.sessions[id]
	m.mu.Unlock()
	if s == nil {
		return nil, ErrNotFound
	}
	if m.cfg.IdleTimeout > 0 {
		select {
		case <-s.done:
		case <-time.After(m.cfg.IdleTimeout):
			return nil, fmt.Errorf("shell: idle timeout")
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	} else {
		select {
		case <-s.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return &s.meta, nil
}

// Cancel terminates the session. Honors the process's KillTree capability:
// when CanKillTree=false we kill only the leaf and emit a log line so the
// operator knows children may have survived — never pretend tree-kill
// happened. Sets meta.State to StateCanceled and stamps EndedAt.
func (m *Manager) Cancel(id string) error {
	m.mu.Lock()
	s := m.sessions[id]
	m.mu.Unlock()
	if s == nil {
		return ErrNotFound
	}
	caps := s.process.Capabilities()
	if err := s.process.Kill(); err != nil {
		return err
	}
	if !caps.CanKillTree {
		lg := m.cfg.Logger
		if lg == nil {
			lg = log.Default()
		}
		lg.Printf("shell: process %d canceled without tree kill (platform CanKillTree=false)", s.process.PID())
	}
	s.cancel() // also cancel the lifecycle so pump unblocks
	s.mu.Lock()
	s.meta.State = StateCanceled
	now := time.Now().UTC()
	s.meta.EndedAt = &now
	s.mu.Unlock()
	return nil
}

// ListJobs returns a snapshot of the job table. Order is undefined; callers
// that need a stable order must sort the result.
func (m *Manager) ListJobs() []Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Job, 0, len(m.jobs))
	for _, j := range m.jobs {
		j.mu.Lock()
		out = append(out, j.meta)
		j.mu.Unlock()
	}
	return out
}

// RestoreJobs loads any persisted jobs and marks them StateStale. Called once
// at bootstrap; reflects the fact that the underlying OS process from a prior
// boot is gone.
func (m *Manager) RestoreJobs() error {
	if m.persist == nil {
		return nil
	}
	list, err := m.persist.LoadJobs()
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, job := range list {
		stale := job
		stale.State = StateStale
		m.jobs[stale.ID] = &liveJob{meta: stale, session: nil}
	}
	return nil
}

// Close tears the manager down at shutdown (called once from App.Shutdown,
// Task 21, before the store closes). For each live session it cancels the
// lifecycle context — because the session's *exec.Cmd was built with
// CommandContext(lifecycle, ...), cancellation kills the OS process, the
// stdout/stderr pipe then EOFs, the pump's Read loop breaks, Process.Wait
// returns, and s.done closes. We block on <-s.done so the pump goroutine and
// console Close (deferred in pump) finish before Close returns. Finally the
// current job list is flushed to the persist store so the next boot's
// RestoreJobs can mark them stale. Idempotent and best-effort.
func (m *Manager) Close() error {
	m.mu.Lock()
	sessions := make([]*liveSession, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.mu.Unlock()
	for _, s := range sessions {
		s.cancel()           // cancel the independent lifecycle context
		_ = s.process.Kill() // unblock factories that do not bind Process to ctx
		<-s.done             // wait for pump goroutine (also closes the console)
	}
	if m.persist != nil {
		for _, job := range m.ListJobs() {
			_ = m.persist.SaveJob(job)
		}
	}
	return nil
}

// ErrNotFound is returned by Read/Write/Wait/Cancel when the supplied id is
// not in the Manager's table.
var ErrNotFound = errors.New("shell: session/job not found")

// pump drains console output into the ring buffer until the console signals
// EOF, then waits for the process and records the exit code. Pumping always
// runs (even when nobody calls Read) so the buffer captures the full tail —
// this is the real fix for the "ring buffer never drains" review bug.
func (s *liveSession) pump() {
	defer close(s.done)
	defer s.console.Close()
	buf := make([]byte, 4096)
	for {
		n, err := s.console.Read(buf)
		if n > 0 {
			s.output.Write(buf[:n])
		}
		if err != nil {
			break
		}
	}
	werr := s.process.Wait()
	s.mu.Lock()
	now := time.Now().UTC()
	s.meta.EndedAt = &now
	if s.meta.State != StateCanceled {
		s.meta.State = StateExited
	}
	s.meta.ExitCode = exitCodeFrom(werr)
	s.finished = true
	s.mu.Unlock()
}

// exitCodeFrom extracts the exit code from the value returned by
// (*exec.Cmd).Wait. nil → clean exit (0); *exec.ExitError → ee.ExitCode();
// any other error (context cancellation, pipe failure, etc.) → -1 sentinel
// so the JSON wire form distinguishes "we don't know" from "zero exit".
func exitCodeFrom(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// ringBuffer is a size-capped byte queue. Writes that would overflow the cap
// drop the OLDEST bytes (so the tail — what a reader most wants — is always
// preserved). Read(max) returns the last min(max, len) bytes.
type ringBuffer struct {
	mu  sync.Mutex
	cap int
	buf bytes.Buffer
}

func newRingBuffer(c int) *ringBuffer { return &ringBuffer{cap: c} }

func (r *ringBuffer) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf.Write(p)
	if r.cap > 0 && r.buf.Len() > r.cap {
		overflow := r.buf.Len() - r.cap
		r.buf.Next(overflow)
	}
	return len(p), nil
}

func (r *ringBuffer) Read(max int) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if max <= 0 || max > r.buf.Len() {
		return r.buf.String()
	}
	data := r.buf.Bytes()
	start := len(data) - max
	if start < 0 {
		start = 0
	}
	return string(data[start:])
}
