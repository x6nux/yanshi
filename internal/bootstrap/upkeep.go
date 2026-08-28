package bootstrap

import (
	"context"

	"github.com/x6nux/yanshi/internal/agent/upkeep"
	"github.com/x6nux/yanshi/internal/config"
	"github.com/x6nux/yanshi/internal/store"
)

// BuildUpkeep assembles the W-D background maintenance worker and starts its
// loop over parent.
//
// It returns a Worker rather than an error because there is nothing here that
// can fail a boot: every job it runs is opportunistic, and a nil store yields a
// nil Worker whose Start/Wait are no-ops. That matches the soft-degrade rule
// the rest of Build follows — a subsystem that cannot come up disables itself
// and says so, it does not refuse the process.
//
// THE WORKER IS ALWAYS STARTED, EVEN WITH NOTHING ENABLED. The off switches
// live inside the jobs (retention 0 compresses nothing), not here, so that a
// running-but-idle loop is the same code path as a working one and an operator
// turning retention on does not need a restart to get a different wiring.
func BuildUpkeep(parent context.Context, cfg config.Config, db *store.Store) *upkeep.Worker {
	w := upkeep.New(db, upkeep.Config{
		RetentionDays: cfg.Storage.RetentionDays,
	})
	go w.Start(parent)
	return w
}
