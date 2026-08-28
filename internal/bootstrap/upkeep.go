package bootstrap

import (
	"context"

	"github.com/x6nux/yanshi/internal/agent/upkeep"
	"github.com/x6nux/yanshi/internal/config"
	"github.com/x6nux/yanshi/internal/store"
	"github.com/x6nux/yanshi/internal/tools"
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
// live inside the jobs (retention 0 compresses nothing, a nil model extracts
// nothing), not here, so that a running-but-idle loop is the same code path as
// a working one.
//
// distillModel is the same model the WS distill handler uses. It is passed
// through only when storage.memory_auto_extract is set: the job spends one
// provider call per finished session, and an upgrade must not silently start
// billing an operator who never asked for it. Passing nil is what
// upkeep.Config.Model documents as "extraction disabled".
func BuildUpkeep(
	parent context.Context, cfg config.Config, db *store.Store, distillModel tools.DistillModel,
) *upkeep.Worker {
	var extractModel tools.DistillModel
	if cfg.Storage.MemoryAutoExtract {
		extractModel = distillModel
	}
	w := upkeep.New(db, upkeep.Config{
		RetentionDays: cfg.Storage.RetentionDays,
		Model:         extractModel,
		MemoryQuota:   cfg.Storage.MemoryQuota,
	})
	go w.Start(parent)
	return w
}
