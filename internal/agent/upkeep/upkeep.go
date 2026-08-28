// Package upkeep runs periodic background maintenance over sessions that have
// gone quiet.
//
// It exists because everything yanshi does to a session happens while somebody
// is attached to it. A conversation that ends — the user closes the window, the
// goal loop finishes, a socket drops — leaves its rows exactly as they were:
// uncompressed forever, and, before W-D-03, having produced no durable asset
// beyond the transcript itself. Both of those jobs need the same trigger ("this
// session has not moved in a while") and the same lifecycle (one ticker, joined
// on shutdown), so they share one worker rather than two.
//
// THE TRIGGER IS A SWEEP, NOT AN EVENT. A session-close callback would be more
// immediate and would miss every session that ended by disconnecting, which is
// most of them — a dropped WebSocket produces no clean end. Idle time is the
// only signal that covers both.
//
// The lifecycle deliberately copies automation.Scheduler: Start blocks until
// the context is cancelled, Wait proves it returned. bootstrap.App.Shutdown
// must Wait before closing the store, or a tick in flight writes into a closed
// database.
package upkeep

import (
	"context"
	"log/slog"
	"time"

	"github.com/x6nux/yanshi/internal/store"
	"github.com/x6nux/yanshi/internal/tools"
)

// Config is the worker's tuning. The zero value runs a worker that does
// nothing, which is what an operator who configured none of this gets.
type Config struct {
	// Interval is how often the sweep runs. Zero selects DefaultInterval.
	Interval time.Duration

	// RetentionDays is the idle threshold, in days, after which a session's
	// messages are compressed into cold storage. ZERO DISABLES COMPRESSION
	// ENTIRELY — see config.StorageConfig.RetentionDays for why that is the
	// only safe default.
	RetentionDays int

	// Model is the consolidation model used for cross-session memory
	// extraction (W-D-03). NIL DISABLES EXTRACTION ENTIRELY, and that is the
	// default an operator gets: this job spends a provider call per finished
	// session, which is not a cost to start incurring because somebody
	// upgraded.
	Model tools.DistillModel

	// MemoryQuota caps how many memory rows the store may hold. Zero means
	// unlimited, which is the pre-W-D-03 behaviour. It applies whether or not
	// Model is set, because memory_write fills the same table.
	MemoryQuota int

	// SweepLimit caps how many sessions one tick may touch, so a first sweep
	// over a year-old database does not hold the write lock for minutes. Zero
	// selects DefaultSweepLimit.
	SweepLimit int
}

// DefaultInterval is the sweep period when Config.Interval is zero. Sessions
// are compressed after days of idleness, so there is nothing to gain from
// looking more often than this, and a shorter period only costs write-lock
// contention with live conversations.
const DefaultInterval = 10 * time.Minute

// DefaultSweepLimit bounds one tick's work when Config.SweepLimit is zero.
const DefaultSweepLimit = 50

// Worker is the maintenance loop. Construct with New, run with Start, join with
// Wait.
type Worker struct {
	db   *store.Store
	cfg  Config
	done chan struct{}
	// now is time.Now in production; tests replace it to place a session on
	// either side of the retention cutoff without sleeping for a day.
	now func() time.Time
}

// New builds a Worker over db. A nil db yields a nil Worker, so a caller whose
// store failed to open degrades to "no upkeep" instead of panicking on the
// first tick.
func New(db *store.Store, cfg Config) *Worker {
	if db == nil {
		return nil
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.SweepLimit <= 0 {
		cfg.SweepLimit = DefaultSweepLimit
	}
	return &Worker{db: db, cfg: cfg, done: make(chan struct{}), now: time.Now}
}

// Start runs until ctx is cancelled, then closes the channel Wait blocks on.
// A nil Worker returns immediately, so the caller need not branch.
//
// The first sweep happens on the first TICK, not at construction: a fresh
// process is the worst moment to take the write lock for a full-database scan,
// and nothing here is urgent by definition — every candidate has been idle for
// days already.
func (w *Worker) Start(ctx context.Context) {
	if w == nil {
		return
	}
	defer close(w.done)
	ticker := time.NewTicker(w.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.RunOnce(ctx)
		}
	}
}

// Wait blocks until Start has returned. Idempotent; safe on a nil Worker.
func (w *Worker) Wait() {
	if w == nil {
		return
	}
	<-w.done
}

// RunOnce performs one sweep. Exported so a test drives a deterministic pass
// without waiting on a ticker, and so an operator-facing command could later
// force one.
//
// Errors are logged, never returned: this loop must survive a single bad
// session, and there is no caller in a position to do anything about one.
func (w *Worker) RunOnce(ctx context.Context) {
	if w == nil || ctx.Err() != nil {
		return
	}
	w.compressCold()
	w.extractMemories(ctx)
}

// compressCold packs sessions idle past the retention threshold.
func (w *Worker) compressCold() {
	if w.cfg.RetentionDays <= 0 {
		return
	}
	cutoff := w.now().AddDate(0, 0, -w.cfg.RetentionDays).Unix()
	packed, err := w.db.CompressColdSessions(cutoff, w.cfg.SweepLimit)
	if err != nil {
		slog.Warn("upkeep: compressing cold sessions failed",
			"error", err, "packed", packed)
	}
	if packed > 0 {
		slog.Info("upkeep: compressed cold sessions", "sessions", packed)
	}
}
