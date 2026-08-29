package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/x6nux/yanshi/internal/agent/registry"
	"github.com/x6nux/yanshi/internal/guard"
)

type agentSpawnArgs struct {
	Prompt          string `json:"prompt"`
	Role            string `json:"role"`
	Tools           string `json:"tools"`
	Nickname        string `json:"nickname,omitempty"`
	ModelOverride   string `json:"model,omitempty"`
	ReasoningEffort string `json:"reasoning,omitempty"`
}

type agentSpawnResult struct {
	AgentID string `json:"agent_id"`
	Status  string `json:"status"`
}

func (t *AgentTools) streamAgentSpawn(ctx context.Context, argsJSON string) <-chan ToolChunk {
	ch := make(chan ToolChunk, 1)
	go func() {
		defer close(ch)
		var a agentSpawnArgs
		if err := ParseArgs(argsJSON, &a); err != nil {
			pushErrChunk(ch, err)
			return
		}
		mgr := ManagerFromContext(ctx)
		if mgr == nil {
			pushErrChunk(ch, fmt.Errorf("agent_spawn: manager not configured"))
			return
		}

		allowed, err := parseToolList(a.Tools)
		if err != nil {
			pushErrChunk(ch, err)
			return
		}
		role, effective, err := resolveSpawnRole(a.Role, allowed)
		if err != nil {
			pushErrChunk(ch, err)
			return
		}
		a.Role, allowed = role, effective

		profile, ok := ProfileFromContext(ctx)
		if !ok {
			pushErrChunk(ch, fmt.Errorf("agent_spawn: permission profile not bound"))
			return
		}
		if err := ValidateOverride(ctx, a.ModelOverride, a.ReasoningEffort, profile, AvailableModelsFromContext(ctx)); err != nil {
			pushErrChunk(ch, err)
			return
		}

		factory := ManagedRunnerFactoryFromContext(ctx)
		if factory == nil {
			pushErrChunk(ch, fmt.Errorf("agent_spawn: orchestrator runner factory not bound"))
			return
		}
		runner := factory(allowed, "")

		id, err := mgr.Spawn(ctx, registry.SpawnRequest{
			Role: a.Role, Nickname: a.Nickname, Prompt: a.Prompt,
			AllowedTools: allowed, ModelOverride: a.ModelOverride, ReasoningEffort: a.ReasoningEffort,
			Runner: runner, Emit: subagentEmitAdapter(ctx),
		})
		if err != nil {
			pushErrChunk(ch, err)
			return
		}

		snap, _ := mgr.Result(id)
		status := "running"
		if snap.Status != "" {
			status = string(snap.Status)
		}
		body, _ := json.Marshal(agentSpawnResult{AgentID: id, Status: status})
		ch <- ToolChunk{Result: string(body)}
	}()
	return ch
}

// resolveSpawnRole normalizes the requested role, validates it against the
// catalog in agentroles.go, and returns the canonical role name together with
// the effective tool allowlist for the sub-agent.
//
// A role must only ever NARROW the caller's tool surface, never widen it — a
// sub-agent that can reach past its parent would turn role selection into a
// privilege-escalation primitive. Hence the returned list is the intersection
// of the role's AllowedTools and the caller's requested tools.
//
// The two empty-set conventions are easy to get backwards, so they are spelled
// out here and pinned by tests:
//
//   - An empty RoleDef.AllowedTools means "this role adds no tool restriction"
//     (the caller's list passes through verbatim) — NOT "allow nothing".
//   - An empty caller list means "inherit the parent's full set" (that is what
//     parseToolList returns for a missing tools arg), so the role's own list is
//     used as-is.
//
// Both sides may hold glob patterns ("*" for general, "memory_*" for
// implementer), so membership is tested in both directions: an entry survives
// when the other side PROVABLY covers it (guard.GlobCovers). That is what lets
// a wildcard behave as the superset it is meant to be — and, since W-B-19, what
// stops one behaving as a superset it is not: a plain match test kept `fs_*`
// against a role allowing `fs_?`, because the pattern matches the string while
// granting strictly less than what the string denotes.
//
// Two cases are rejected outright rather than degraded:
//
//   - role "custom" with no caller tools. Its RoleDef carries no AllowedTools,
//     so it would silently mean "everything the caller can do" — the exact
//     opposite of a custom restricted role. The list must be explicit.
//   - a fully disjoint intersection. Downstream (selectSubAgentTools) reads an
//     empty allowlist as "inherit everything", so returning one here would hand
//     out more than either side allowed. Fail closed instead.
func resolveSpawnRole(role string, callerTools []string) (string, []string, error) {
	requested := role
	if strings.TrimSpace(requested) == "" {
		requested = "general" // omitted role keeps the historical default
	}
	def, ok := LookupRole(requested)
	if !ok {
		return "", nil, fmt.Errorf("agent_spawn: unknown role %q; valid roles: %s",
			role, strings.Join(AgentRoleNames(), ", "))
	}
	if def.Name == "custom" && len(callerTools) == 0 {
		return "", nil, fmt.Errorf(`agent_spawn: role "custom" requires an explicit tools list ` +
			`(e.g. tools: ["fs_read","fs_search"]); without one the subagent would inherit every ` +
			`tool the caller can use, which is the opposite of a custom restricted role`)
	}

	effective, err := narrowRoleTools(def, callerTools)
	if err != nil {
		return "", nil, err
	}
	return def.Name, effective, nil
}

// narrowRoleTools is the intersection arm of resolveSpawnRole: role ∩ caller,
// or the reason it could not be taken.
//
// Split out from resolveSpawnRole so the algebra can be exercised against a
// RoleDef that is not in the shipped catalog. The catalog contains no pattern
// pair that produces an unrepresentable overlap, which is exactly why a test
// restricted to catalog roles would be pinning "today's catalog happens to be
// safe" rather than "the algebra is safe" — and the catalog is a list somebody
// will add to.
func narrowRoleTools(def RoleDef, callerTools []string) ([]string, error) {
	switch {
	case len(def.AllowedTools) == 0:
		return callerTools, nil
	case len(callerTools) == 0:
		return def.AllowedTools, nil
	}

	effective, unprovable := intersectToolSets(def.AllowedTools, callerTools)
	if len(effective) > 0 {
		return effective, nil
	}
	// W-B-19: "无法安全求交时显式报错而非静默取宽". Two ways to get here and
	// they need different sentences, because only one of them is the caller's
	// mistake:
	//
	//   - genuinely disjoint sets. Nothing either side allows is allowed by the
	//     other, and the answer is "pick different tools".
	//   - an intersection this code cannot REPRESENT. Both sides carry patterns,
	//     they overlap, and no single glob denotes the overlap — so the only
	//     expressible answers are wider or narrower than the truth. Silently
	//     taking the wider one is a privilege escalation dressed as a
	//     convenience; taking the narrower one silently would be a sub-agent
	//     that mysteriously cannot use tools it was granted. Saying so is the
	//     only answer that is neither.
	if len(unprovable) > 0 {
		return nil, fmt.Errorf("agent_spawn: role %q and the requested tools %v overlap "+
			"only through patterns whose intersection cannot be expressed as a tool list (%v); "+
			"name the tools explicitly instead of using a wildcard. Role tools: %v",
			def.Name, callerTools, unprovable, def.AllowedTools)
	}
	return nil, fmt.Errorf("agent_spawn: role %q allows none of the requested tools %v; role tools: %v",
		def.Name, callerTools, def.AllowedTools)
}

// intersectToolSets returns the entries allowed by both sides, plus the entries
// it DROPPED because it could not prove they were covered.
//
// Membership is glob-aware in both directions, because either side may hold a
// pattern such as "*" or "memory_*" that stands for a set of concrete tools.
// The test is guard.GlobCovers, not a plain match, and W-B-19 is the difference:
// "does a pattern on the other side match this string" is not "is everything
// this string denotes allowed by the other side". `fs_?` matches the STRING
// `fs_*`, so the old test kept `fs_*` — a set strictly larger than the role
// permitted — and did it silently.
//
// The second return is not decoration. Dropping an entry narrows, which is the
// safe direction and needs no announcement while something survives; but when
// NOTHING survives, "the sets are disjoint" and "the overlap exists and cannot
// be written down" are different facts about the caller's request, and only the
// second is fixable by rewording it. resolveSpawnRole uses it to say which.
//
// Both input slices are assumed non-empty; the empty-set conventions are
// handled by resolveSpawnRole.
func intersectToolSets(roleTools, callerTools []string) (kept, unprovable []string) {
	out := make([]string, 0, len(callerTools))
	seen := make(map[string]bool, len(callerTools))
	keepFn := func(name string) {
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	// An unrepresentable overlap needs a wildcard on BOTH sides.
	//
	// Only a wildcard can be one at all — a literal the other side does not
	// match is plain disjointness, and calling that "cannot be expressed" sends
	// the caller looking for a spelling that does not exist. But that test alone
	// is not enough, and the shipped catalog proved it: `implementer` carries
	// `memory_*`, so ANY literal request it did not cover came back as
	// "overlap only through patterns whose intersection cannot be expressed
	// ([memory_*]); name the tools explicitly instead of using a wildcard" — to
	// a caller who wrote one literal and no wildcard at all, naming a pattern
	// they never typed.
	//
	// It is sound to require both sides. A wildcard on one side meets literals
	// on the other, and each of those literals either matches (kept above) or is
	// not in the set at all; so the intersection is exactly the kept ones and
	// there is nothing left over to be inexpressible.
	//
	// Still conservative in the other direction: `fs_*` against `net_*` is
	// reported as unrepresentable although it is empty. Deciding glob
	// disjointness in general is a machine this hint does not justify, and the
	// error is the harmless one — the request is refused either way, and the
	// wording only affects what the caller tries next.
	bothSidesGlob := slices.ContainsFunc(roleTools, guard.HasGlobMeta) &&
		slices.ContainsFunc(callerTools, guard.HasGlobMeta)
	dropped := map[string]bool{}
	dropFn := func(name string) {
		if bothSidesGlob && guard.HasGlobMeta(name) && !dropped[name] {
			dropped[name] = true
			unprovable = append(unprovable, name)
		}
	}
	for _, c := range callerTools {
		if guard.GlobCovers(roleTools, c) {
			keepFn(c)
		} else {
			dropFn(c)
		}
	}
	for _, r := range roleTools {
		if guard.GlobCovers(callerTools, r) {
			keepFn(r)
		} else {
			dropFn(r)
		}
	}
	return out, unprovable
}

type agentWaitArgs struct {
	AgentID string `json:"agent_id"`
	Timeout int    `json:"timeout,omitempty"`
}

func (t *AgentTools) streamAgentWait(ctx context.Context, argsJSON string) <-chan ToolChunk {
	ch := make(chan ToolChunk, 1)
	go func() {
		defer close(ch)
		var a agentWaitArgs
		if err := ParseArgs(argsJSON, &a); err != nil {
			pushErrChunk(ch, err)
			return
		}
		mgr := ManagerFromContext(ctx)
		if mgr == nil {
			pushErrChunk(ch, fmt.Errorf("agent_wait: manager not configured"))
			return
		}

		waitCtx := ctx
		if a.Timeout > 0 {
			var cancel context.CancelFunc
			waitCtx, cancel = context.WithTimeout(ctx, time.Duration(a.Timeout)*time.Second)
			defer cancel()
		}
		rec, werr := mgr.Wait(waitCtx, a.AgentID, registry.WaitOpts{})
		if werr != nil && rec.ID == "" {
			pushErrChunk(ch, werr)
			return
		}
		out, _ := json.Marshal(rec)
		ch <- ToolChunk{Result: string(out)}
	}()
	return ch
}

type agentResultArgs struct {
	AgentID string `json:"agent_id"`
}
type agentSendInputArgs struct {
	AgentID   string `json:"agent_id"`
	Text      string `json:"text"`
	Interrupt bool   `json:"interrupt,omitempty"`
}
type agentResumeArgs struct {
	AgentID string `json:"agent_id"`
	Prompt  string `json:"prompt,omitempty"`
}
type agentAssignArgs struct {
	AgentID    string `json:"agent_id"`
	Assignment string `json:"assignment"`
}
type agentCancelArgs struct {
	AgentID string `json:"agent_id"`
}
type agentListArgs struct {
	IncludeArchived bool `json:"include_archived,omitempty"`
}

func (t *AgentTools) streamAgentResult(ctx context.Context, argsJSON string) <-chan ToolChunk {
	return simpleStream(argsJSON, func(a agentResultArgs) (string, error) {
		mgr := ManagerFromContext(ctx)
		if mgr == nil {
			return "", fmt.Errorf("agent_result: manager not configured")
		}
		rec, ok := mgr.Result(a.AgentID)
		if !ok {
			return "", fmt.Errorf("agent_result: %q not found", a.AgentID)
		}
		out, _ := json.Marshal(rec)
		return string(out), nil
	})
}

func (t *AgentTools) streamAgentSendInput(ctx context.Context, argsJSON string) <-chan ToolChunk {
	return simpleStream(argsJSON, func(a agentSendInputArgs) (string, error) {
		mgr := ManagerFromContext(ctx)
		if mgr == nil {
			return "", fmt.Errorf("agent_send_input: manager not configured")
		}
		if err := mgr.SendInput(a.AgentID, a.Text, a.Interrupt); err != nil {
			return "", err
		}
		return `{"ok":true}`, nil
	})
}

func (t *AgentTools) streamAgentResume(ctx context.Context, argsJSON string) <-chan ToolChunk {
	return simpleStream(argsJSON, func(a agentResumeArgs) (string, error) {
		mgr := ManagerFromContext(ctx)
		if mgr == nil {
			return "", fmt.Errorf("agent_resume: manager not configured")
		}
		rec, ok := mgr.Result(a.AgentID)
		if !ok {
			return "", registry.ErrNotFound
		}
		profile, pok := ProfileFromContext(ctx)
		if !pok {
			return "", fmt.Errorf("agent_resume: permission profile not bound")
		}
		if err := EnsureOverrideForResume(ctx, rec.ModelOverride, rec.ReasoningEffort, profile, AvailableModelsFromContext(ctx)); err != nil {
			return "", err
		}
		factory := ManagedRunnerFactoryFromContext(ctx)
		if factory == nil {
			return "", fmt.Errorf("agent_resume: orchestrator runner factory not bound")
		}
		runner := factory(rec.AllowedTools, rec.Instruction)
		if _, err := mgr.Resume(context.Background(), a.AgentID, registry.ResumeRequest{Runner: runner, Emit: subagentEmitAdapter(ctx)}); err != nil {
			return "", err
		}
		snap, _ := mgr.Result(a.AgentID)
		out, _ := json.Marshal(snap)
		return string(out), nil
	})
}

func (t *AgentTools) streamAgentAssign(ctx context.Context, argsJSON string) <-chan ToolChunk {
	return simpleStream(argsJSON, func(a agentAssignArgs) (string, error) {
		mgr := ManagerFromContext(ctx)
		if mgr == nil {
			return "", fmt.Errorf("agent_assign: manager not configured")
		}
		if err := mgr.Assign(a.AgentID, a.Assignment); err != nil {
			return "", err
		}
		return `{"ok":true}`, nil
	})
}

func (t *AgentTools) streamAgentCancel(ctx context.Context, argsJSON string) <-chan ToolChunk {
	return simpleStream(argsJSON, func(a agentCancelArgs) (string, error) {
		mgr := ManagerFromContext(ctx)
		if mgr == nil {
			return "", fmt.Errorf("agent_cancel: manager not configured")
		}
		if err := mgr.Cancel(a.AgentID); err != nil {
			return "", err
		}
		return `{"ok":true}`, nil
	})
}

func (t *AgentTools) streamAgentList(ctx context.Context, argsJSON string) <-chan ToolChunk {
	return simpleStream(argsJSON, func(a agentListArgs) (string, error) {
		mgr := ManagerFromContext(ctx)
		if mgr == nil {
			return "", fmt.Errorf("agent_list: manager not configured")
		}
		out, _ := json.Marshal(mgr.List(a.IncludeArchived))
		return string(out), nil
	})
}

func simpleStream[Req any](argsJSON string, fn func(Req) (string, error)) <-chan ToolChunk {
	ch := make(chan ToolChunk, 1)
	go func() {
		defer close(ch)
		var req Req
		if err := ParseArgs(argsJSON, &req); err != nil {
			pushErrChunk(ch, err)
			return
		}
		out, err := fn(req)
		if err != nil {
			pushErrChunk(ch, err)
			return
		}
		ch <- ToolChunk{Result: out}
	}()
	return ch
}
