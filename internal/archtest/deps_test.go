// Package archtest — architecture governance tests for GOV1 dependency layering.
//
// This file enforces hexagonal architecture rules: port allowlists (R2), W2
// tracking (R3), single bootstrap composition root (R4), and service-layer
// isolation (R5). All rules operate on the import graph produced by
// buildImportGraph (helpers_test.go) and use the same module identity helpers.
package archtest

import (
	"fmt"
	"sort"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Module identity
// ---------------------------------------------------------------------------

const modPrefix = "github.com/x6nux/yanshi"

// ip returns the full Go import path for a module-relative path such as
// "internal/guard".
func ip(rel string) string {
	return modPrefix + "/" + rel
}

// ---------------------------------------------------------------------------
// R2: Port allowlists (target state)
//
// Each port package's allowlist defines which internal dependencies are
// permitted. An empty set means no internal dependencies are allowed.
// Tracked exceptions (portExceptions) document known violations that must
// be removed when the corresponding refactoring milestone is completed.
// ---------------------------------------------------------------------------

// portAllowlists maps each port package (full import path) to the set of
// internal dependencies it is permitted to import.
var portAllowlists = map[string]map[string]bool{
	ip("internal/guard"): {ip("internal/execpolicy"): true},
	ip("internal/store"): {},
	ip("internal/proto"): {
		// internal/pathjail was listed here and internal/proto has never
		// imported it. GOV1's allowlist has no dead-entry detection — unlike
		// GOV2/3/4/5/6, whose exemption tables all fail when an entry stops
		// being needed — so this sat as a permanent pre-authorisation: the day
		// proto DID reach for pathjail, the gate would have waved it through.
		// W8's AttachRef deliberately carries a path and no resolution logic,
		// which is why it still does not need it. Handed to governance:
		// portAllowlists should get the same dead-entry check.
		ip("internal/task/work"): true,
	},
	ip("internal/config"): {},
	ip("internal/vcs"): {
		ip("internal/auth"):       true,
		ip("internal/execpolicy"): true,
		ip("internal/guard"):      true,
		ip("internal/secrets"):    true,
		ip("internal/store"):      true,
	},
}

// portExceptions maps (portPkg → forbiddenDep → remediation reason) for
// known, time-boxed allowlist violations. Each entry must be removed when
// the listed remediation is complete.
//
// Same semantics as the other exemption tables: entries may only be REMOVED,
// and a dead entry — the port no longer imports the dep, so the exemption
// excuses nothing — fails TestR2_PortAllowlist. Without that check a finished
// remediation leaves a standing permit behind, and the next accidental import
// of that dep is waved through by an exemption whose stated reason has already
// been discharged.
var portExceptions = map[string]map[string]string{
	ip("internal/store"): {
		ip("internal/auth"):    "store persists auth_metadata; pending type migration",
		ip("internal/secrets"): "store persists encrypted secrets; pending extraction to secrets-owned storage",
	},
	ip("internal/config"): {
		ip("internal/guard"): "W2: config.go uses guard.PermissionProfile as value type in map; P3 fix (move type + bootstrap conversion)",
	},
}

// ---------------------------------------------------------------------------
// R4: Server core set and fan-out exemption
// ---------------------------------------------------------------------------

// serverCoreSet lists the minimum core packages that must be reachable from
// the bootstrap composition root via transitive internal dependencies.
var serverCoreSet = []string{
	ip("internal/store"),
	ip("internal/vcs"),
	ip("internal/guard"),
	ip("internal/proto"),
	ip("internal/config"),
	ip("internal/secrets"),
	ip("internal/execpolicy"),
	ip("internal/pathjail"),
	ip("internal/approval"),
	ip("internal/auth"),
	ip("internal/tools"),
	ip("internal/agent/orchestrator"),
	ip("internal/api/http"),
	ip("internal/ctxcompact"),
	ip("internal/skills"),
	ip("internal/mcp"),
}

// fanOutExempt lists packages exempt from the 25-fan-out limit in R4(b).
// These are naturally high-fan-out packages (composition root, tool hub).
//
// Unlike portExceptions / lineExceptions / assemblyExceptions this is NOT a
// debt table and deliberately carries no dead-entry check. Those tables record
// a violation someone intends to repair, so an entry that stops applying is
// finished work and must go. This one records a permanent architectural role:
// bootstrap is the composition root and tools is the tool hub, and both are
// SUPPOSED to be hubs. Deleting an entry because the package momentarily
// dipped under 25 dependencies would make the gate accuse the composition root
// of being a second composition root as soon as it grew back.
var fanOutExempt = map[string]bool{
	ip("internal/bootstrap"): true,
	ip("internal/tools"):     true,
}

// ---------------------------------------------------------------------------
// R5: Service layer prefixes that ports must not import
// ---------------------------------------------------------------------------

// serviceLayerPrefixes lists the relative import path prefixes that identify
// service / application-layer packages. Port packages must not directly import
// any package whose full path matches or starts with one of these prefixes.
var serviceLayerPrefixes = []string{
	"internal/tools",
	"internal/agent",
	"internal/api",
	"internal/bootstrap",
	"internal/llm",
	"internal/ctxcompact",
	"internal/mcp",
	"internal/lsp",
	"internal/shell",
	"internal/sandbox",
}

// isServiceLayer reports whether pkg (a full Go import path) belongs to a
// service-layer package.
func isServiceLayer(pkg string) bool {
	for _, prefix := range serviceLayerPrefixes {
		full := ip(prefix)
		if pkg == full || strings.HasPrefix(pkg, full+"/") {
			return true
		}
	}
	return false
}

// =========================================================================
// GOV1 Tests
// =========================================================================

// TestR1_NoImportCycle verifies that the import graph builds successfully.
// The Go toolchain rejects cyclic imports at build / list time, so a
// successful buildImportGraph call is sufficient to assert no cycles exist.
func TestR1_NoImportCycle(t *testing.T) {
	graph := buildImportGraph(t)
	if len(graph) == 0 {
		t.Fatal("buildImportGraph returned empty graph")
	}
	t.Logf("import graph contains %d internal packages", len(graph))
}

// ---------------------------------------------------------------------------
// R2: Port allowlist gate
// ---------------------------------------------------------------------------

// TestR2_PortAllowlist verifies that every direct internal dependency of
// each port package is either in its allowlist or in the tracked exceptions
// map (portExceptions).
func TestR2_PortAllowlist(t *testing.T) {
	graph := buildImportGraph(t)
	mp := modulePath(t)

	violations := checkPortAllowlist(graph, portAllowlists, portExceptions, mp)
	for _, v := range violations {
		t.Error(v)
	}

	dead := deadPortExceptions(graph, portExceptions, mp)
	for _, d := range dead {
		t.Error(d)
	}

	if len(violations)+len(dead) > 0 {
		t.Fatalf("port allowlist problems: %d violation(s), %d dead exception(s) (see above)",
			len(violations), len(dead))
	}
}

// portImports reports whether portPkg directly imports dep in the graph.
func portImports(graph map[string][]string, portPkg, dep string) bool {
	for _, d := range graph[portPkg] {
		if d == dep {
			return true
		}
	}
	return false
}

// deadPortExceptions returns one message per portExceptions entry whose
// dependency no longer exists — the remediation landed and the permit outlived
// it.
func deadPortExceptions(
	graph map[string][]string,
	exceptions map[string]map[string]string,
	mp string,
) []string {
	var dead []string
	for portPkg, deps := range exceptions {
		for dep, reason := range deps {
			if portImports(graph, portPkg, dep) {
				continue
			}
			dead = append(dead, fmt.Sprintf("GOV1: %s no longer imports %s, but "+
				"portExceptions still permits it (%q) — DELETE the entry (no dead "+
				"exemptions); leaving it standing re-authorises the dependency the "+
				"remediation just removed", short(portPkg, mp), short(dep, mp), reason))
		}
	}
	sort.Strings(dead)
	return dead
}

// checkPortAllowlist examines each port package in allowlists against the
// import graph and returns human-readable violation messages for every
// dependency that is neither in the port's allowlist nor in the tracked
// exceptions map.
func checkPortAllowlist(
	graph map[string][]string,
	allowlists map[string]map[string]bool,
	exceptions map[string]map[string]string,
	mp string,
) []string {
	var violations []string

	// Sort keys for deterministic output.
	portPkgs := make([]string, 0, len(allowlists))
	for p := range allowlists {
		portPkgs = append(portPkgs, p)
	}
	sort.Strings(portPkgs)

	for _, portPkg := range portPkgs {
		allowlist := allowlists[portPkg]
		deps := graph[portPkg]

		exc, _ := exceptions[portPkg]
		if exc == nil {
			exc = map[string]string{}
		}

		for _, dep := range deps {
			if allowlist[dep] {
				continue
			}
			if _, ok := exc[dep]; ok {
				continue
			}
			violations = append(violations,
				fmt.Sprintf("%s imports forbidden internal package %s",
					short(portPkg, mp), short(dep, mp)))
		}
	}
	return violations
}

// ---------------------------------------------------------------------------
// R3: W2 config → guard tracking
// ---------------------------------------------------------------------------

// TestR3_W2ConfigMustNotDependOnGuard enforces three invariants around the
// known W2 config→guard dependency:
//
//  1. If config depends on guard, the dependency MUST be tracked in
//     portExceptions. A reminder is logged for the tracked exception.
//  2. If config does NOT depend on guard and an exception exists, the
//     exception MUST be removed (no dead entries).
//  3. If config does NOT depend on guard and no exception exists, GOV1
//     compliance is achieved.
//
// Invariant 2 is the named-pair case of the general dead-exemption check in
// TestR2_PortAllowlist (deadPortExceptions); both share portImports so the two
// can never disagree about what "still depends on" means. This test survives
// as a distinct assertion because config→guard carries a specific remediation
// (P3/P4) that deserves its own message.
func TestR3_W2ConfigMustNotDependOnGuard(t *testing.T) {
	graph := buildImportGraph(t)

	configPkg := ip("internal/config")
	guardPkg := ip("internal/guard")

	dependsOnGuard := portImports(graph, configPkg, guardPkg)

	exc := portExceptions[configPkg]
	_, hasException := exc[guardPkg]

	switch {
	case dependsOnGuard && hasException:
		t.Logf("REMINDER: config→guard is tracked in portExceptions: %s", exc[guardPkg])
		t.Log("This W2 dependency must be resolved by the P3/P4 milestone " +
			"(move PermissionProfile type out of guard, add bootstrap conversion).")

	case dependsOnGuard && !hasException:
		t.Error("config depends on guard but the dependency is NOT tracked in portExceptions. " +
			"Either remove the dependency or add an exception with a removal plan.")

	case !dependsOnGuard && hasException:
		t.Errorf("config no longer depends on guard, but portExceptions still tracks the dependency. "+
			"Remove the %q entry from portExceptions.", short(guardPkg, modulePath(t)))

	case !dependsOnGuard && !hasException:
		t.Log("config does not depend on guard — compliant with GOV1.")
	}
}

// ---------------------------------------------------------------------------
// R4: Single server composition root
// ---------------------------------------------------------------------------

// TestR4_SingleServerCompositionRoot enforces two rules:
//
//	(a) Every package in serverCoreSet is reachable from bootstrap via
//	    transitive internal dependencies (the composition root must wire
//	    the entire server).
//	(b) No internal package (except bootstrap and tools) has more than 25
//	    direct internal dependencies (fan-out), preventing a second
//	    composition root from emerging.
func TestR4_SingleServerCompositionRoot(t *testing.T) {
	graph := buildImportGraph(t)
	mp := modulePath(t)
	bootstrapPkg := ip("internal/bootstrap")

	// (a) Bootstrap closure must contain every serverCoreSet package.
	closure := closureInternalDeps(graph, bootstrapPkg)
	var missingCore []string
	for _, pkg := range serverCoreSet {
		if _, ok := closure[pkg]; !ok {
			missingCore = append(missingCore, short(pkg, mp))
		}
	}
	if len(missingCore) > 0 {
		t.Errorf("R4(a): bootstrap closure is missing %d server core package(s):\n\t%s",
			len(missingCore), strings.Join(missingCore, "\n\t"))
	}

	// (b) Fan-out limit.
	const fanOutLimit = 25
	var highFanOut []string
	for pkg, deps := range graph {
		if !strings.HasPrefix(pkg, mp+"/internal/") {
			continue
		}
		if fanOutExempt[pkg] {
			continue
		}
		if n := len(deps); n > fanOutLimit {
			highFanOut = append(highFanOut,
				fmt.Sprintf("%s (%d direct internal deps)", short(pkg, mp), n))
		}
	}
	if len(highFanOut) > 0 {
		t.Errorf("R4(b): %d package(s) exceed fan-out limit of %d (potential second composition root):\n\t%s",
			len(highFanOut), fanOutLimit, strings.Join(highFanOut, "\n\t"))
	}
}

// ---------------------------------------------------------------------------
// R5: Ports must not depend on service layer
// ---------------------------------------------------------------------------

// TestR5_PortsMustNotDependOnServiceLayer verifies that no port package
// imports any service-layer package (tools, agent, api, bootstrap, llm,
// ctxcompact, mcp, lsp, shell, sandbox).
func TestR5_PortsMustNotDependOnServiceLayer(t *testing.T) {
	graph := buildImportGraph(t)
	mp := modulePath(t)

	portPkgs := make([]string, 0, len(portAllowlists))
	for pkg := range portAllowlists {
		portPkgs = append(portPkgs, pkg)
	}
	sort.Strings(portPkgs)

	var violations []string
	for _, portPkg := range portPkgs {
		deps := graph[portPkg]
		for _, dep := range deps {
			if isServiceLayer(dep) {
				violations = append(violations,
					fmt.Sprintf("%s imports service-layer package %s",
						short(portPkg, mp), short(dep, mp)))
			}
		}
	}
	for _, v := range violations {
		t.Error(v)
	}
	if len(violations) > 0 {
		t.Fatalf("R5 violations: %d (see above)", len(violations))
	}
}

// ---------------------------------------------------------------------------
// Synthetic test: prove the gate catches new violations
// ---------------------------------------------------------------------------

// TestR2_DetectsNewViolationInSyntheticGraph proves that the port-allowlist
// gate (checkPortAllowlist) detects a new, untracked violation. It
// constructs a synthetic import graph where config (from a separate module)
// imports tools and checks that the violation is reported when the allowlist
// and exceptions are both empty.
func TestR2_DetectsNewViolationInSyntheticGraph(t *testing.T) {
	synMod := "example.com/synth"
	configPkg := synMod + "/internal/config"
	toolsPkg := synMod + "/internal/tools"

	graph := map[string][]string{
		configPkg: {toolsPkg},
		toolsPkg:  {},
	}

	allowlists := map[string]map[string]bool{
		configPkg: {},
	}
	exceptions := map[string]map[string]string{
		configPkg: {},
	}

	violations := checkPortAllowlist(graph, allowlists, exceptions, synMod)
	if len(violations) == 0 {
		t.Fatal("expected violation for config→tools, but got none — " +
			"checkPortAllowlist did not catch the untracked dependency")
	}

	// Verify the violation message mentions both packages.
	found := false
	for _, v := range violations {
		if strings.Contains(v, "config") && strings.Contains(v, "tools") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected config→tools violation in messages, got: %v", violations)
	}
	t.Logf("synthetic gate correctly detected violation (expected): %v", violations)
}

// ---------------------------------------------------------------------------
// R6: Who may depend on the orchestrator
// ---------------------------------------------------------------------------

// orchestratorConsumers is the closed set of packages allowed to import
// internal/agent/orchestrator, with the reason each one does.
//
// The direction is the point. portAllowlists answers "what may this package
// depend on"; this answers the reverse — "who may depend on THIS package" —
// and no existing table states that. R5's serviceLayerPrefixes is the nearest
// neighbour and says something different again (ports must not import the
// service layer).
//
// The orchestrator owns the ReAct loop, tool dispatch, sub-agent delegation
// and per-turn context injection. Every front door that reaches it directly
// re-implements the turn setup rather than going through a seam — which is
// exactly the drift the two S3 entries below record. The consumer set is
// small enough today to pin, so a new front end or channel wiring itself
// straight into the orchestrator turns red at the moment it is written
// instead of after it has grown its own copy of the turn plumbing.
//
// internal/agent/spawn is NOT here, and the dead-entry check is what said so:
// grep finds it importing the orchestrator, `go list` does not, because every
// file in that package starts with //go:build ignore. It is not unwired — it
// does not compile into the binary. Whether the package should be revived or
// deleted is W10's call; what W9 removed is the "spawn_agent" entry it left
// behind in the sub-agent deny set.
//
// This is an architectural allowlist, not a debt table: entries name a
// legitimate role. It still carries a dead-entry check, because a package that
// stopped importing the orchestrator has genuinely left the set, and package
// imports do not fluctuate the way a fan-out count does.
var orchestratorConsumers = map[string]string{
	ip("internal/bootstrap"): "the composition root; it builds the orchestrator",
	ip("internal/api/http"): "the WS/SSE chat transports. ws.go builds its own " +
		"TurnOpts and calls EventsWithHistoryOpts — converging that with the v1 " +
		"service's copy is S3 work, deliberately NOT done in W9.",
	ip("internal/api/v1"): "the versioned Agent API service, shared by HTTP and " +
		"JSON-RPC. It builds its own TurnOpts; same S3 convergence as api/http.",
}

// TestR6_OrchestratorConsumersAreClosed pins the reverse dependency edge.
func TestR6_OrchestratorConsumersAreClosed(t *testing.T) {
	graph := buildImportGraph(t)
	target := ip("internal/agent/orchestrator")

	seen := map[string]bool{}
	for pkg, deps := range graph {
		for _, dep := range deps {
			if dep != target {
				continue
			}
			seen[pkg] = true
			if _, ok := orchestratorConsumers[pkg]; !ok {
				t.Errorf("%s imports the orchestrator directly and is not in "+
					"orchestratorConsumers.\n"+
					"  Reaching the ReAct loop from a new package means re-implementing "+
					"turn setup (TurnOpts, context injection, sub-agent binding) rather "+
					"than going through a seam. If this really is a new front door, add "+
					"it with the reason; otherwise depend on the v1 service.", pkg)
			}
		}
	}
	for pkg := range orchestratorConsumers {
		if !seen[pkg] {
			t.Errorf("orchestratorConsumers lists %s, which no longer imports the "+
				"orchestrator — delete the row. A stale entry pre-authorises the "+
				"dependency to come back unnoticed.", pkg)
		}
	}
}
