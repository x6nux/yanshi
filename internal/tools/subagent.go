package tools

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/x6nux/yanshi/internal/agent/registry"
	"github.com/x6nux/yanshi/internal/proto"
)

// SubAgentRunner runs a sub-agent ReAct loop with the given prompt and a subset
// of the parent's tools, inheriting the parent's permission profile (read from
// ctx by the nested tools). It is bound into the turn context BY the
// orchestrator (which owns the model + tool set), so the AgentTools layer can
// spawn real, tooled sub-agents without importing the orchestrator package
// (tools → orchestrator would be an import cycle, since orchestrator imports
// tools). allowedTools is the subset of tool names the sub-agent may use (nil/empty
// = inherit the parent's full set). instructionOverride, when non-empty, replaces
// the inherited system instruction for this sub-agent. When no runner is bound,
// runSubAgent falls back to a bare model Generate (no tools) — the legacy behavior.
type SubAgentRunner func(ctx context.Context, prompt string, allowedTools []string, instructionOverride string) (string, error)

type subAgentRunnerKey struct{}

// WithSubAgentRunner binds r into ctx so AgentTools.runSubAgent can find it.
func WithSubAgentRunner(ctx context.Context, r SubAgentRunner) context.Context {
	if r == nil {
		return ctx
	}
	return context.WithValue(ctx, subAgentRunnerKey{}, r)
}

// SubAgentRunnerFromContext returns the bound runner, or nil (→ bare-LLM
// fallback).
func SubAgentRunnerFromContext(ctx context.Context) SubAgentRunner {
	r, _ := ctx.Value(subAgentRunnerKey{}).(SubAgentRunner)
	return r
}

// WithLeafSubAgentTools marks a nested call created by a workflow. The
// orchestrator uses this marker to remove tools that can create another
// orchestrator/workflow, preventing workflow steps from recursively spawning
// workflows while retaining the normal inherited tool set for leaf work.
func WithLeafSubAgentTools(ctx context.Context) context.Context {
	return context.WithValue(ctx, leafSubAgentToolsKey{}, true)
}

// LeafSubAgentTools reports whether workflow-created sub-agents must receive
// only non-orchestration tools.
func LeafSubAgentTools(ctx context.Context) bool {
	v, _ := ctx.Value(leafSubAgentToolsKey{}).(bool)
	return v
}

type leafSubAgentToolsKey struct{}

// SubAgentEmit forwards a sub-agent's streamed ServerFrame (thinking,
// tool_call, tool_result, agent_chunk, …) to the parent transport so the user
// can watch the sub-agent's ReAct loop live instead of seeing only its final
// result. It is bound into the turn context BY the WS handler (the same
// ctx-callback pattern as WithToolProgress / WithCompactCallback), using
// proto.ServerFrame because that is the frame type conn.write already accepts —
// no conversion is needed at the bind site. When no emit is bound (the CLI
// Query path, unit tests, or the bare-LLM fallback), runSubAgentTurn degrades
// to the legacy behavior: events are consumed for the final answer only and
// nothing is forwarded. The tools package depends on proto (not the reverse),
// so this import does not form a cycle.
type SubAgentEmit func(frame proto.ServerFrame)

type subAgentEmitKey struct{}

// WithSubAgentEmit binds emit into ctx so the orchestrator's runSubAgentTurn
// can forward each sub-agent event frame to the parent transport. nil emit is a
// no-op (returns ctx unchanged), which preserves the legacy "black-box"
// behavior for callers that don't care about streaming sub-agent events.
func WithSubAgentEmit(ctx context.Context, emit SubAgentEmit) context.Context {
	if emit == nil {
		return ctx
	}
	return context.WithValue(ctx, subAgentEmitKey{}, emit)
}

// SubAgentEmitFrom returns the bound emit callback, or nil when none is bound
// (→ runSubAgentTurn skips forwarding and just returns the final answer).
func SubAgentEmitFrom(ctx context.Context) SubAgentEmit {
	emit, _ := ctx.Value(subAgentEmitKey{}).(SubAgentEmit)
	return emit
}

// subAgent depth limiting: an agent_start inside a sub-agent inside a sub-agent
// MaxSubAgentDepth is the vertical nesting limit. Concurrency (horizontal
// limit, configurable via subagents.limit / registry.Manager limit) is a
// separate orthogonal dimension. They interact at the Spawn site but neither
// subsumes the other: see registry/manager.go Spawn for the priority order.
// The cap is conservative (3) — deeper nesting is almost always a model
// mistake, not a real need.
const MaxSubAgentDepth = 3

type subAgentDepthKey struct{}

// SubAgentDepth returns the current nesting depth (0 = top-level turn).
func SubAgentDepth(ctx context.Context) int {
	d, _ := ctx.Value(subAgentDepthKey{}).(int)
	return d
}

// WithSubAgentDepth returns ctx annotated with the given nesting depth.
func WithSubAgentDepth(ctx context.Context, depth int) context.Context {
	return context.WithValue(ctx, subAgentDepthKey{}, depth)
}

// ---------------------------------------------------------------------------
// Progress event types
// ---------------------------------------------------------------------------

// SubAgentEventKind enumerates sub-agent progress event kinds.
type SubAgentEventKind int

// Sub-agent progress event kinds.
const (
	SubAgentToolStart SubAgentEventKind = iota
	SubAgentToolEnd
	SubAgentTokens
)

// SubAgentEvent is one progress event reported by a running sub-agent.
type SubAgentEvent struct {
	Kind        SubAgentEventKind
	ToolDisplay string
	ToolArgs    string
	Tokens      int
}

type subAgentProgressKey struct{}

// WithSubAgentProgress stores a progress callback in context.
func WithSubAgentProgress(ctx context.Context, cb func(SubAgentEvent)) context.Context {
	if cb == nil {
		return ctx
	}
	return context.WithValue(ctx, subAgentProgressKey{}, cb)
}

// SubAgentProgressFromContext retrieves the sub-agent progress callback from
func SubAgentProgressFromContext(ctx context.Context) func(SubAgentEvent) {
	cb, _ := ctx.Value(subAgentProgressKey{}).(func(SubAgentEvent))
	return cb
}

// WorkflowProgress carries hooks the workflow engine calls as it runs steps.
type WorkflowProgress struct {
	SetTotal func(total int)
	StepCB   func(stepID string) func(SubAgentEvent)
	StepDone func(stepID string, err error)
}

type workflowProgressKey struct{}

// WithWorkflowProgress stores workflow progress hooks in context.
func WithWorkflowProgress(ctx context.Context, wp *WorkflowProgress) context.Context {
	if wp == nil {
		return ctx
	}
	return context.WithValue(ctx, workflowProgressKey{}, wp)
}

// WorkflowProgressFromContext retrieves the workflow progress hooks from
func WorkflowProgressFromContext(ctx context.Context) *WorkflowProgress {
	wp, _ := ctx.Value(workflowProgressKey{}).(*WorkflowProgress)
	return wp
}

// ---------------------------------------------------------------------------
// Managed sub-agent adapter (B1)
// ---------------------------------------------------------------------------

// ManagedSubAgentSpec is the parameter set for synchronously running a managed
// sub-agent (shared by agent_start, workflow, analysis).
type ManagedSubAgentSpec struct {
	ParentID        string
	Role            string
	Custom          *registry.CustomRole
	Nickname        string
	Prompt          string
	AllowedTools    []string
	Instruction     string
	ModelOverride   string
	ReasoningEffort string
	Runner          registry.Runner
}

// ManagedSubAgentResult carries the outcome of a managed sub-agent run.
type ManagedSubAgentResult struct {
	Text     string
	AgentID  string
	Usage    registry.Usage
	Terminal registry.Status
}

// ---------------------------------------------------------------------------
// Manager context key
// ---------------------------------------------------------------------------
type managerKey struct{}

// WithManager stores the registry.Manager in context for sub-agent spawning.
func WithManager(ctx context.Context, m *registry.Manager) context.Context {
	return context.WithValue(ctx, managerKey{}, m)
}

// ManagerFromContext retrieves the registry.Manager from context.
func ManagerFromContext(ctx context.Context) *registry.Manager {
	if v, ok := ctx.Value(managerKey{}).(*registry.Manager); ok {
		return v
	}
	return nil
}

// ---------------------------------------------------------------------------
// ManagedRunnerFactory
// ---------------------------------------------------------------------------

// ManagedRunnerFactory constructs a registry.Runner that re-binds the turn
// context (profile / workroot / VCS / depth / emit) on the Manager's
// cancellation-only child context.
type ManagedRunnerFactory func(allowed []string, instruction string) registry.Runner

type managedRunnerFactoryKey struct{}

// WithManagedRunnerFactory stores a ManagedRunnerFactory in context.
func WithManagedRunnerFactory(ctx context.Context, f ManagedRunnerFactory) context.Context {
	return context.WithValue(ctx, managedRunnerFactoryKey{}, f)
}

// ManagedRunnerFactoryFromContext retrieves the ManagedRunnerFactory from
func ManagedRunnerFactoryFromContext(ctx context.Context) ManagedRunnerFactory {
	if v, ok := ctx.Value(managedRunnerFactoryKey{}).(ManagedRunnerFactory); ok {
		return v
	}
	return nil
}

// ---------------------------------------------------------------------------
// UsageSink
// ---------------------------------------------------------------------------

// UsageSink receives token usage callbacks from nested model calls.
type UsageSink func(registry.Usage)

type usageSinkKey struct{}

// WithUsageSink stores a token-usage callback in context.
func WithUsageSink(ctx context.Context, sink UsageSink) context.Context {
	return context.WithValue(ctx, usageSinkKey{}, sink)
}

// UsageSinkFrom retrieves the token-usage callback from context.
func UsageSinkFrom(ctx context.Context) UsageSink {
	if v, ok := ctx.Value(usageSinkKey{}).(UsageSink); ok {
		return v
	}
	return nil
}

// ---------------------------------------------------------------------------
// ManagedSubAgentRun
// ---------------------------------------------------------------------------

// ManagedSubAgentRun spawns a managed sub-agent, blocks on slot availability
// (retries on *SpawnErrCap), and waits for a terminal state.
func ManagedSubAgentRun(ctx context.Context, spec ManagedSubAgentSpec) (ManagedSubAgentResult, error) {
	mgr := ManagerFromContext(ctx)
	if mgr == nil {
		return ManagedSubAgentResult{}, fmt.Errorf("managed subagent run: no manager on context")
	}
	if spec.Runner == nil {
		return ManagedSubAgentResult{}, fmt.Errorf("managed subagent run: runner is required")
	}
	role := spec.Role
	if role == "" {
		role = inferRoleFromTools(spec.AllowedTools)
	}
	id, err := spawnWithRetry(ctx, mgr, registry.SpawnRequest{
		AgentType:       "subagent",
		ParentID:        spec.ParentID,
		Role:            role,
		Custom:          spec.Custom,
		Nickname:        spec.Nickname,
		Prompt:          spec.Prompt,
		AllowedTools:    spec.AllowedTools,
		Instruction:     spec.Instruction,
		ModelOverride:   spec.ModelOverride,
		ReasoningEffort: spec.ReasoningEffort,
		Runner:          spec.Runner,
		Emit:            subagentEmitAdapter(ctx),
	})
	if err != nil {
		return ManagedSubAgentResult{}, err
	}
	// Park the CALLER while it waits on its child. Without this a parent holds
	// its slot for the whole of the child's run, so at cap N a fleet of N
	// delegating parents occupies every slot waiting for children that can
	// never be spawned — a livelock, measured during the W3 prototype at the
	// default cap of 10. Parking is a no-op at depth 0, where the caller is an
	// orchestrator turn holding no slot at all.
	unpark := mgr.Park(registry.CurrentAgentID(ctx))
	final, werr := mgr.Wait(ctx, id, registry.WaitOpts{})
	unpark()
	res := ManagedSubAgentResult{
		Text: final.Result, AgentID: final.ID, Usage: final.Usage, Terminal: final.Status,
	}
	if werr != nil && final.ID == "" {
		return res, werr
	}
	return res, nil
}

// spawnWithRetry retries Spawn on *SpawnErrCap with exponential backoff,
// blocking until a slot frees or context is cancelled.
func spawnWithRetry(ctx context.Context, mgr *registry.Manager, req registry.SpawnRequest) (string, error) {
	backoff := 5 * time.Millisecond
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		id, err := mgr.Spawn(ctx, req)
		if err == nil {
			return id, nil
		}
		var capped *registry.SpawnErrCap
		if !errors.As(err, &capped) {
			return "", err
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 200*time.Millisecond {
			backoff *= 2
		}
	}
}

// subagentEmitAdapter adapts a registry.Event to a proto.ServerFrame and
// forwards it to the transport via SubAgentEmit from context.
func subagentEmitAdapter(ctx context.Context) registry.EventSink {
	emit := SubAgentEmitFrom(ctx)
	if emit == nil {
		return nil
	}
	return func(ev registry.Event) { emit(protoFromRegistryEvent(ev)) }
}

// ---------------------------------------------------------------------------
// Role inference
// ---------------------------------------------------------------------------
var readOnlyToolSet = map[string]bool{
	"fs_read": true, "fs_glob": true, "fs_search": true, "shell_run": true, "time_now": true,
}

func inferRoleFromTools(allowed []string) string {
	if len(allowed) == 0 {
		return "general"
	}
	for _, t := range allowed {
		if t == "*" || !readOnlyToolSet[t] {
			return "general"
		}
	}
	return "explore"
}

// ---------------------------------------------------------------------------
// Working-set hint from EVIDENCE section
// ---------------------------------------------------------------------------
var knownResultSections = []string{"SUMMARY", "CHANGES", "EVIDENCE", "RISKS", "BLOCKERS"}

// ParentWorkingSetHint extracts the EVIDENCE section from a sub-agent's
// five-section result and appends it as a hint.
func ParentWorkingSetHint(result string) string {
	evidence := extractResultSection(result, "EVIDENCE")
	if strings.TrimSpace(evidence) == "" {
		return result
	}
	return result + "\n\n[parent working-set hint — EVIDENCE]\n" + evidence
}

func extractResultSection(result, section string) string {
	lines := strings.Split(result, "\n")
	var out []string
	started := false
	for _, ln := range lines {
		if name := matchResultSection(strings.TrimSpace(ln)); name != "" {
			if name == section {
				started = true
				continue
			}
			if started {
				break
			}
			continue
		}
		if started && strings.TrimSpace(ln) != "" {
			out = append(out, ln)
		}
	}
	return strings.Join(out, "\n")
}

func matchResultSection(trimmed string) string {
	for _, n := range knownResultSections {
		if trimmed == n+":" || trimmed == n {
			return n
		}
	}
	return ""
}

// protoFromRegistryEvent maps a registry.Event to a proto.ServerFrame for
// transport relay.
func protoFromRegistryEvent(ev registry.Event) proto.ServerFrame {
	return proto.NewSubagentEvent(ev.AgentID, ev.Role, string(ev.Type), string(ev.Status), ev.Text)
}

// keep unused-import satisfaction
var _ = filepath.Base

// ResultSections is a sub-agent result parsed into the five contract sections.
//
// Missing is the list of sections the contract requires that the result did not
// carry, in contract order. It is what makes the parse RESULT usable rather
// than a best-effort map: a caller reading Evidence cannot otherwise tell an
// empty section from an absent one, and the difference decides whether to ask
// the sub-agent again.
type ResultSections struct {
	Summary  string
	Changes  string
	Evidence string
	Risks    string
	Blockers string
	Missing  []string
}

// Get returns a section by its contract name, or "" for an unknown name.
func (r ResultSections) Get(name string) string {
	switch name {
	case "SUMMARY":
		return r.Summary
	case "CHANGES":
		return r.Changes
	case "EVIDENCE":
		return r.Evidence
	case "RISKS":
		return r.Risks
	case "BLOCKERS":
		return r.Blockers
	}
	return ""
}

// Complete reports whether every contract section was present.
func (r ResultSections) Complete() bool { return len(r.Missing) == 0 }

// ParseResultSections splits a sub-agent result into the five contract
// sections.
//
// Only EVIDENCE was ever extracted — knownResultSections had exactly one
// consumer, and the other four sections had none at all. So "the five-section
// output is parseable" was true of the vocabulary and of nothing else: a
// sub-agent could emit four of them, or emit them out of order, and no code
// path would notice.
//
// Order is deliberately NOT required. The contract lists the sections in an
// order, but a model that answers with BLOCKERS first has still answered; a
// parser that rejected that would turn a cosmetic deviation into a failed turn.
// What IS reported is absence, through Missing.
func ParseResultSections(result string) ResultSections {
	var out ResultSections
	for _, name := range knownResultSections {
		body := strings.TrimSpace(extractResultSection(result, name))
		switch name {
		case "SUMMARY":
			out.Summary = body
		case "CHANGES":
			out.Changes = body
		case "EVIDENCE":
			out.Evidence = body
		case "RISKS":
			out.Risks = body
		case "BLOCKERS":
			out.Blockers = body
		}
		if body == "" {
			out.Missing = append(out.Missing, name)
		}
	}
	return out
}
