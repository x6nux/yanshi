package log

import (
	"log/slog"
	"sync"
	"time"
)

// sampler drops repeated INFO/DEBUG records so one chatty call site cannot
// bury everything else, WITHOUT ever dropping a WARN or ERROR.
//
// Two decisions worth stating, because the obvious implementations are both
// wrong for logs:
//
//   - Keyed by MESSAGE, not global. A global "keep 1 in N" penalises rare
//     messages exactly as much as chatty ones -- so the single line that
//     explains an incident is as likely to be discarded as the thousandth
//     copy of a health-check. Rate limiting per message text means a call
//     site can only crowd out ITSELF.
//   - Level-gated, not level-weighted. WARN and ERROR bypass the sampler
//     entirely rather than getting a higher keep-probability. "Sampling does
//     not drop the important ones" is only true if it is impossible, not
//     unlikely: a 1-in-1000 chance of losing the one ERROR in an incident is
//     the case where sampling costs the most.
//
// The counter is a burst-then-throttle: the first burst copies of a message in
// each window pass, the rest are counted and dropped, and the next window
// starts fresh with a suppressed=N attribute on the first survivor so the gap
// is visible rather than silent.
type sampler struct {
	burst  int
	window time.Duration

	mu    sync.Mutex
	state map[string]*sampleState
	now   func() time.Time // seam: tests drive time rather than sleeping
}

type sampleState struct {
	windowStart time.Time
	seen        int
	suppressed  int
}

func newSampler(burst int, window time.Duration) *sampler {
	if burst <= 0 {
		burst = 1
	}
	return &sampler{burst: burst, window: window, state: map[string]*sampleState{}, now: time.Now}
}

// allow reports whether this record should be emitted, and how many copies
// were suppressed since the last emitted one (0 unless this is the first
// survivor of a new window).
func (s *sampler) allow(level slog.Level, msg string) (bool, int) {
	// The bypass is the whole contract. Anything at WARN or above is never
	// counted, never rate limited, and never contributes to another message's
	// budget.
	if level >= slog.LevelWarn {
		return true, 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	st, ok := s.state[msg]
	if !ok {
		st = &sampleState{windowStart: now}
		s.state[msg] = st
	}
	if now.Sub(st.windowStart) >= s.window {
		suppressed := st.suppressed
		st.windowStart = now
		st.seen = 1
		st.suppressed = 0
		return true, suppressed
	}
	st.seen++
	if st.seen <= s.burst {
		return true, 0
	}
	st.suppressed++
	return false, 0
}
