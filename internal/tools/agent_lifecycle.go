package tools

import (
	"context"
	"encoding/json"
	"fmt"
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
		if a.Role == "" {
			a.Role = "general"
		}

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

type agentResultArgs struct{ AgentID string `json:"agent_id"` }
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
type agentCancelArgs struct{ AgentID string `json:"agent_id"` }
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
