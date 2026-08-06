package shell

import (
	"context"
	"time"
)

// lastTouch records when a session was last read, written to, or waited on.
// Sessions that nothing has touched are what the reaper collects.
//
// Kept on liveSession rather than in a side map so it cannot go stale against
// a session that was removed.

// reapInterval is how often the reaper wakes. Small relative to any sensible
// idle timeout, large enough that an idle manager is not a busy loop.
const reapInterval = 15 * time.Second

// StartReaper runs the idle-session collector until ctx is done.
//
// Config.IdleTimeout existed before this and had exactly one consumer:
// Manager.Wait's `time.After` branch, which bounds how long a CALLER waits.
// That is a different thing from reclaiming a session — after that branch
// fires, the session is still in the map and its OS process is still running.
// So "sessions are reclaimed by policy" described a timeout that reclaimed
// nothing; a client that started sessions and stopped reading them leaked a
// process each time, for the life of the server.
//
// Cancelling a session also kills its process tree (see killtree_unix.go), so
// reaping is a real release rather than a bookkeeping delete.
//
// Returns immediately when no idle timeout is configured: a zero timeout means
// "no policy", and a reaper that invented one would kill sessions the operator
// expected to keep.
func (m *Manager) StartReaper(ctx context.Context) {
	if m == nil || m.cfg.IdleTimeout <= 0 {
		return
	}
	go func() {
		t := time.NewTicker(reapInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				m.ReapIdle(time.Now())
			}
		}
	}()
}

// ReapIdle cancels and removes every session idle for longer than
// Config.IdleTimeout, returning the ids it collected.
//
// Exported and taking an explicit `now` so tests can drive it without waiting
// for wall-clock time — the alternative is a test that sleeps for the timeout,
// which is both slow and flaky.
func (m *Manager) ReapIdle(now time.Time) []string {
	if m == nil || m.cfg.IdleTimeout <= 0 {
		return nil
	}
	m.mu.Lock()
	var stale []string
	for id, s := range m.sessions {
		if s == nil {
			continue
		}
		s.mu.Lock()
		idle := now.Sub(s.touched)
		// A finished session holds no process; leaving it in the map is what
		// lets a caller read its final output, and it is bounded by the same
		// timeout.
		s.mu.Unlock()
		if idle > m.cfg.IdleTimeout {
			stale = append(stale, id)
		}
	}
	m.mu.Unlock()

	for _, id := range stale {
		// Cancel outside the manager lock: it signals a process group and
		// waits on the pump, and holding mu across that would block every
		// other session's Read.
		_ = m.Cancel(id)
		m.mu.Lock()
		delete(m.sessions, id)
		m.mu.Unlock()
	}
	return stale
}

// touch records activity on a session. Called by every caller-facing operation
// so "idle" means "nobody is using it", not "it produced no output" — a
// long-running silent build is not idle.
func (s *liveSession) touch(now time.Time) {
	s.mu.Lock()
	s.touched = now
	s.mu.Unlock()
}
