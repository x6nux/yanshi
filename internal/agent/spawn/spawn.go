//go:build ignore

// Package spawn provides the spawn_agent tool that lets the orchestrator create
// sub-agents in the same process to execute tasks concurrently.
package spawn

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"

	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/tools"
	"github.com/x6nux/yanshi/internal/vcs"
)

// SpawnAgentTool exposes the spawn_agent GuardedTool. It lets the orchestrator
// create a sub-agent (a fresh ChatModelAgent in the same process) to execute a
// given goal and return the result. Multiple sub-agents can run concurrently.
type SpawnAgentTool struct {
	model     model.BaseChatModel  // default LLM for sub-agents
	tools     []tool.BaseTool      // base tools (spawn_agent auto-filtered)
	profile   guard.PermissionProfile
	models    map[string]model.BaseChatModel // optional named models
	vcs       *vcs.VCS                       // optional VCS instance
	vcsRepoID string                         // VCS repo id (empty = unavailable)

	agentIDSeq int64
	agentIDMu  sync.Mutex
}

// New creates a SpawnAgentTool.
//
//   - chatModel is the default LLM for sub-agents.
//   - baseTools is the full tool set; spawn_agent is filtered out automatically.
//   - prof is the permission profile for sub-agents.
//   - namedModels is an optional name→model map for model selection (may be nil).
//   - v is an optional VCS instance for worktree-based isolation.
//   - repoID is the VCS repo id (required when v is non-nil).
func New(
	chatModel model.BaseChatModel,
	baseTools []tool.BaseTool,
	prof guard.PermissionProfile,
	namedModels map[string]model.BaseChatModel,
	v *vcs.VCS,
	repoID string,
) *SpawnAgentTool {
	// Filter out every orchestration tool to prevent nested agent/workflow
	// expansion. Child tool sets may only shrink; leaf work remains available.
	filtered := make([]tool.BaseTool, 0, len(baseTools))
	for _, t := range baseTools {
		info, err := t.Info(context.Background())
		if err != nil || info == nil {
			continue
		}
		switch info.Name {
		case "spawn_agent", "agent_start", "workflow_start", "analysis":
			continue
		default:
			filtered = append(filtered, t)
		}
	}

	return &SpawnAgentTool{
		model:     chatModel,
		tools:     filtered,
		profile:   prof,
		models:    namedModels,
		vcs:       v,
		vcsRepoID: repoID,
	}
}

// GuardedTool returns the spawn_agent tool ready to be registered with the
// orchestrator's tool set.
func (s *SpawnAgentTool) GuardedTool() *tools.GuardedTool {
	return tools.NewGuardedTool(
		"spawn_agent",
		"Spawn a sub-agent to execute a specific task and return its result. "+
			"The sub-agent runs in the same process with its own ReAct loop, "+
			"shares the parent's tools and model, and returns the final output. "+
			"Multiple sub-agents can run concurrently.",
		params(map[string]*schema.ParameterInfo{
			"goal": {
				Type:     schema.String,
				Desc:     "The task description for the sub-agent to execute",
				Required: true,
			},
			"timeout_seconds": {
				Type: schema.Integer,
				Desc: "Maximum execution time in seconds (default 300, max 3600)",
			},
			"model_name": {
				Type: schema.String,
				Desc: "Optional model provider name (e.g. 'gpt-4o', 'claude-sonnet-4-20250514'). " +
					"Empty uses the parent agent's default model.",
			},
		}),
		s.runSpawn,
	)
}

// spawnArgs is the JSON argument shape for spawn_agent.
type spawnArgs struct {
	Goal           string `json:"goal"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	ModelName      string `json:"model_name"`
}

// runSpawn is the tool body: it creates a sub-agent, runs the goal, and returns
// the result as a JSON string.
func (s *SpawnAgentTool) runSpawn(ctx context.Context, argsJSON string) (string, error) {
	var a spawnArgs
	if err := tools.ParseArgs(argsJSON, &a); err != nil {
		return "", fmt.Errorf("spawn_agent: parse args: %w", err)
	}
	if a.Goal == "" {
		return "", fmt.Errorf("spawn_agent: goal is required")
	}

	// Resolve timeout (default 300s, max 3600s).
	timeout := 300
	if a.TimeoutSeconds > 0 {
		if a.TimeoutSeconds > 3600 {
			timeout = 3600
		} else {
			timeout = a.TimeoutSeconds
		}
	}

	// Resolve model.
	chatModel := s.model
	if a.ModelName != "" && s.models != nil {
		if named, ok := s.models[a.ModelName]; ok {
			chatModel = named
		}
	}

	// Generate a unique agent id.
	agentID := s.nextAgentID()

	// Build a VCS scope for the sub-agent. If a VCS is available, create a
	// worktree for isolation; otherwise inherit the parent's scope.
	subScope := tools.VCSScope{}
	if s.vcs != nil && s.vcsRepoID != "" {
		wt, err := s.vcs.AddWorktree(s.vcsRepoID, []string{agentID})
		if err == nil {
			subScope = tools.VCSScope{
				VCS:        s.vcs,
				RepoID:     s.vcsRepoID,
				WorktreeID: wt.ID,
				Agent:      agentID,
			}
			defer s.vcs.RemoveWorktree(wt.ID)
		}
	}
	if subScope.VCS == nil {
		if parentScope, ok := tools.VCSScopeFromContext(ctx); ok {
			subScope = tools.VCSScope{
				VCS:        parentScope.VCS,
				RepoID:     parentScope.RepoID,
				WorktreeID: "",
				Agent:      agentID,
			}
		}
	}

	// Build the sub-agent orchestrator.
	subOrch, err := orchestrator.New(orchestrator.Config{
		Model:   chatModel,
		Tools:   s.tools,
		Profile: s.profile,
		Instruction: "You are a sub-agent. Execute the given goal and report the result. " +
			"The user will provide a specific task — complete it thoroughly.",
		MaxIters: 20,
		VCSScope: subScope,
	})
	if err != nil {
		return "", fmt.Errorf("spawn_agent: create sub-agent: %w", err)
	}

	// Run with timeout.
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	result, err := subOrch.Query(runCtx, a.Goal)
	if err != nil {
		return tools.ToJSON(map[string]string{
			"error": fmt.Sprintf("spawn_agent: sub-agent failed: %v", err),
			"agent": agentID,
		}), nil
	}

	return tools.ToJSON(map[string]string{
		"result": result,
		"agent":  agentID,
	}), nil
}

// nextAgentID returns a unique agent identifier.
func (s *SpawnAgentTool) nextAgentID() string {
	s.agentIDMu.Lock()
	s.agentIDSeq++
	seq := s.agentIDSeq
	s.agentIDMu.Unlock()
	return fmt.Sprintf("sub-%d-%s", seq, uuid.NewString()[:8])
}

// params builds a ParamsOneOf from a map of ParameterInfo (mirrors tools.params
// but needed here to avoid importing helpers that aren't exported).
func params(m map[string]*schema.ParameterInfo) *schema.ParamsOneOf {
	if len(m) == 0 {
		return nil
	}
	return schema.NewParamsOneOfByParams(func() map[string]*schema.ParameterInfo {
		return m
	})
}
