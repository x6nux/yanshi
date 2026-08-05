package tools

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/agent/registry"
)

// managedRoute matches the orchestrator branch that sends a delegated turn
// through the Manager instead of running it inline.
var managedRoute = regexp.MustCompile(`ManagedSubAgentRun\(ctx, tools\.ManagedSubAgentSpec\{`)

// TestDelegatedTurnsGoThroughTheManager pins the routing half of W3 Task 3.
//
// Running a sub-agent inline means it takes no slot, so the concurrency cap
// describes a population nobody belongs to: MaxConcurrent could be 1 and a
// hundred delegated turns would still run at once. The parking machinery in
// registry.Park is only worth anything once this routing exists — and this
// routing is only safe once parking does, since a parent blocked in Wait
// while holding its slot is exactly the livelock parking prevents.
//
// Asserted at the source. Driving the real branch needs an Orchestrator, a
// model and a live Manager, and orchestrator cannot import tools' test
// helpers without a cycle. What must not silently disappear is the branch.
func TestDelegatedTurnsGoThroughTheManager(t *testing.T) {
	src, err := os.ReadFile("../agent/orchestrator/orchestrator.go")
	if err != nil {
		t.Fatalf("read orchestrator.go: %v", err)
	}
	body := string(src)
	if !managedRoute.MatchString(body) {
		t.Fatal("runSubAgentTurn no longer routes through ManagedSubAgentRun: " +
			"delegated turns take no concurrency slot, so the cap stops meaning anything")
	}
	// The routing must stay conditional on a Manager being present, or every
	// non-orchestrator caller (and most tests) breaks.
	if !strings.Contains(body, "if mgr := tools.ManagerFromContext(ctx); mgr != nil {") {
		t.Error("the managed route is no longer guarded by a Manager check")
	}
	// The spec's fields matter as much as the branch. ParentID is what makes
	// the delegated agent a child rather than a sibling; drop it and the whole
	// thread tree flattens, silently — every agent looks top-level and the
	// depth accounting the cap relies on stops describing anything.
	// Measured W3 review round 8: blanking it reddened nothing.
	if !strings.Contains(body, "ParentID:     registry.CurrentAgentID(ctx),") {
		t.Error("the managed route no longer passes ParentID: delegated agents " +
			"become top-level and the thread tree flattens")
	}
	if !strings.Contains(body, "Runner:       factory(allowed, instructionOverride),") {
		t.Error("the managed route no longer builds its Runner from the bound factory")
	}
	// Instruction carries the caller's system-prompt override. Drop it and the
	// sub-agent silently runs on the default instruction instead: the analysis
	// tool's specialised prompt, for one, simply stops applying, and the turn
	// still succeeds and still returns text. Measured W3 review round 15.
	if !strings.Contains(body, "Instruction:  instructionOverride,") {
		t.Error("the managed route no longer passes the instruction override: " +
			"sub-agents fall back to the default prompt with nothing reporting it")
	}
}

// TestManagedSubAgentRunParksItsCaller pins the other half of the pair: the
// routing above is a livelock without it.
func TestManagedSubAgentRunParksItsCaller(t *testing.T) {
	src, err := os.ReadFile("subagent.go")
	if err != nil {
		t.Fatalf("read subagent.go: %v", err)
	}
	body := string(src)
	park := strings.Index(body, "unpark := mgr.Park(registry.CurrentAgentID(ctx))")
	wait := strings.Index(body, "final, werr := mgr.Wait(ctx, id, registry.WaitOpts{})")
	unpark := strings.Index(body, "unpark()")
	if park < 0 || wait < 0 || unpark < 0 {
		t.Fatal("ManagedSubAgentRun no longer parks its caller around the child wait: " +
			"a fleet of delegating parents will hold every slot waiting for children " +
			"that can never be spawned")
	}
	if !(park < wait && wait < unpark) {
		t.Errorf("park/wait/unpark are out of order (%d/%d/%d): parking must bracket the wait", park, wait, unpark)
	}
	// The spawn spec's Emit is what carries the child's events up to the
	// parent. Drop it and the delegation still succeeds and still returns its
	// text — the parent just goes blind for the whole run, with no error to
	// say so. Measured W3 review round 11: nulling it reddened nothing.
	if !strings.Contains(body, "Emit:            subagentEmitAdapter(ctx),") {
		t.Error("ManagedSubAgentRun no longer wires the child's event emitter: " +
			"the parent sees nothing the child does, and nothing reports that")
	}
}

// TestManagedRunnerFactoryRoundTrip keeps the context carrier the routing
// depends on honest.
func TestManagedRunnerFactoryRoundTrip(t *testing.T) {
	if ManagedRunnerFactoryFromContext(context.Background()) != nil {
		t.Fatal("a bare context must carry no factory")
	}
	var gotAllowed []string
	ctx := WithManagedRunnerFactory(context.Background(),
		ManagedRunnerFactory(func(allowed []string, _ string) registry.Runner {
			gotAllowed = allowed
			return nil
		}))
	f := ManagedRunnerFactoryFromContext(ctx)
	if f == nil {
		t.Fatal("factory did not survive the context round trip")
	}
	f([]string{"fs_read"}, "instr")
	if len(gotAllowed) != 1 || gotAllowed[0] != "fs_read" {
		t.Errorf("factory received %v; want the allowed list verbatim", gotAllowed)
	}
}
