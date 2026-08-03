package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/x6nux/yanshi/internal/agent/registry"
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
// when a pattern on the other side matches it. That is what lets a wildcard
// behave as the superset it is meant to be.
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

	switch {
	case len(def.AllowedTools) == 0:
		return def.Name, callerTools, nil
	case len(callerTools) == 0:
		return def.Name, def.AllowedTools, nil
	}

	effective := intersectToolSets(def.AllowedTools, callerTools)
	if len(effective) == 0 {
		return "", nil, fmt.Errorf("agent_spawn: role %q allows none of the requested tools %v; role tools: %v",
			def.Name, callerTools, def.AllowedTools)
	}
	return def.Name, effective, nil
}

// intersectToolSets returns the entries allowed by both sides. Membership is
// glob-aware in both directions (anyGlobMatch), because either side may hold a
// pattern such as "*" or "memory_*" that stands for a set of concrete tools.
// Both input slices are assumed non-empty; the empty-set conventions are
// handled by resolveSpawnRole.
func intersectToolSets(roleTools, callerTools []string) []string {
	out := make([]string, 0, len(callerTools))
	seen := make(map[string]bool, len(callerTools))
	keep := func(name string) {
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	for _, c := range callerTools {
		if anyGlobMatch(roleTools, c) {
			keep(c)
		}
	}
	for _, r := range roleTools {
		if anyGlobMatch(callerTools, r) {
			keep(r)
		}
	}
	return out
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
