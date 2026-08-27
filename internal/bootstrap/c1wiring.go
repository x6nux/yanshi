package bootstrap

import (
	"context"
	"fmt"
	"os"

	"github.com/cloudwego/eino/components/model"

	"github.com/x6nux/yanshi/internal/agent/automation"
	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	"github.com/x6nux/yanshi/internal/agent/registry"
	"github.com/x6nux/yanshi/internal/config"
	"github.com/x6nux/yanshi/internal/store"
	"github.com/x6nux/yanshi/internal/task/work"
)

// wireC1 assembles the C1 capability — the eight automation_* tools,
// agent_batch, and (when a cheap provider is available) rlm_query — and
// returns the tools to register, the scheduler goroutine that App.Shutdown
// must Wait() on, and the automation Manager that `yanshi schedule` operates
// through.
//
// This lives in its own file rather than in bootstrap.go because C1 assembly
// is a self-contained responsibility with its own soft-degrade policy, not
// because of a line count — the GOV2 ceiling is 5000 now and bootstrap.go is
// nowhere near it. The original reason recorded here (bootstrap.go being within
// a few lines of a 1000-line cap) stopped being true when the cap was raised,
// and a stale justification invites the next person to undo a split that is
// still correct on its merits.
//
// Division of responsibility for failures, mirroring the VCS/LSP/plugin paths:
//
//   - Warnings are degradations BuildC1 already decided to survive (today: a
//     missing batch.rlm_model, which drops rlm_query alone). They are printed
//     here because there is no decision left for the caller to make.
//   - A returned error is a failure nobody has handled yet. The caller owns
//     that policy — Build prints it and continues with C1 entirely disabled,
//     rather than refusing to boot over a scheduler that most deployments
//     never touch.
//
// fakeModel must be nil on the production path: SelectRLMModel reads a non-nil
// fake as permission to skip the cheap-provider requirement.
func wireC1(
	parent context.Context,
	cfg config.Config,
	db *store.Store,
	workMgr work.ManagerLike,
	broker BrokerSubmitter,
	registryMgr *registry.Manager,
	models map[string]model.BaseChatModel,
	fakeModel model.BaseChatModel,
) ([]orchestrator.BaseTool, *automation.Scheduler, *automation.Manager, error) {
	adapter := NewA2Adapter(workMgr, broker, db)
	comps, err := BuildC1(parent, cfg, db, adapter, registryMgr, models, fakeModel)
	if err != nil {
		return nil, nil, nil, err
	}
	for _, w := range comps.Warnings {
		fmt.Fprintf(os.Stderr, "yanshi: %s\n", w)
	}

	var out []orchestrator.BaseTool
	if comps.Automation != nil && comps.Automation.Tools != nil {
		for _, t := range comps.Automation.Tools.Tools() {
			out = append(out, t)
		}
	}
	if comps.Batch != nil {
		for _, t := range comps.Batch.Tools() {
			out = append(out, t)
		}
	}
	// RLM is optional by design (see C1Components): a deployment without a
	// cheap provider keeps automation and agent_batch and simply has no
	// rlm_query.
	if comps.RLM != nil {
		for _, t := range comps.RLM.Tools() {
			out = append(out, t)
		}
	}

	var scheduler *automation.Scheduler
	var manager *automation.Manager
	if comps.Automation != nil {
		scheduler = comps.Automation.Scheduler
		// The manager is returned alongside the scheduler because `yanshi
		// schedule` needs the SAME instance: Manager serialises its
		// read-modify-write with an in-process mutex, so a second reader
		// talking to the database directly would race the scheduler's own
		// tick. Until this was returned, the ops endpoint had no way to reach
		// it and every `yanshi schedule` call answered "this daemon was
		// assembled without the automation scheduler" — a true sentence about
		// a wiring gap, which reads exactly like a legitimate build variant.
		manager = comps.Automation.Manager
	}
	return out, scheduler, manager, nil
}
