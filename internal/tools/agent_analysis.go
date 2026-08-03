// Package tools provides agent management, workflow, and analysis tools.
//
// This file contains the Analysis tool and supporting workflow generation
// functions moved from agent.go for the E3 architecture governance split:
//   - analysisArgs struct
//   - streamAnalysis method (dispatches single-agent vs workflow mode)
//   - runAnalysisWorkflow method
//   - fillWorkflowTarget helper
//   - generateAnalysisWorkflow function

package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type analysisArgs struct {
	Target   string          `json:"target"`
	Mode     string          `json:"mode"`
	Workflow json.RawMessage `json:"workflow"`
}

func (t *AgentTools) streamAnalysis(ctx context.Context, argsJSON string) <-chan ToolChunk {
	ch := make(chan ToolChunk, 16)
	go func() {
		defer close(ch)
		var a analysisArgs
		if err := ParseArgs(argsJSON, &a); err != nil {
			pushErrChunk(ch, err)
			return
		}
		if a.Target == "" {
			pushErrChunk(ch, fmt.Errorf("target must not be empty"))
			return
		}
		if t.chatModel == nil {
			pushErrChunk(ch, fmt.Errorf("no chat model configured; cannot run analysis"))
			return
		}
		mode := strings.ToLower(strings.TrimSpace(a.Mode))
		if mode == "" {
			pushErrChunk(ch, fmt.Errorf("mode is required; use 'agent' or 'workflow'"))
			return
		}
		agentDef, ok := GetPredefinedAgent("analysis")
		if !ok {
			pushErrChunk(ch, fmt.Errorf("internal error: analysis predefined agent not found"))
			return
		}
		switch mode {
		case "agent":
			prompt := FillPrompt(agentDef.PromptTmpl, map[string]string{"target": a.Target})
			sctx, finalize := bindSubAgentProgress(ctx, ch, "")
			result, err := t.runSubAgent(WithLeafSubAgentTools(sctx), prompt, nil, "")
			finalize()
			if err != nil {
				pushErrChunk(ch, err)
				return
			}
			// Terminal path (same rationale as agent_start): the analysis
			// result is handed straight to the parent agent, so EVIDENCE is
			// re-surfaced as a parent-facing working-set hint.
			ch <- ToolChunk{Result: ParentWorkingSetHint(result)}
		case "workflow":
			wp, stopTicker := makeWorkflowProgress(ch)
			defer stopTicker()
			sctx := WithWorkflowProgress(ctx, wp)
			result, err := t.runAnalysisWorkflow(sctx, agentDef, a.Target, a.Workflow)
			if err != nil {
				pushErrChunk(ch, err)
				return
			}
			ch <- ToolChunk{Result: result}
		default:
			pushErrChunk(ch, fmt.Errorf("mode must be 'agent' or 'workflow' (got %q)", mode))
		}
	}()
	return ch
}

// runAnalysisWorkflow runs the analysis as a multi-step DAG workflow.
// It clones the predefined workflow definition, fills the {{target}}
// placeholder in every step prompt, and executes it.
func (t *AgentTools) runAnalysisWorkflow(ctx context.Context, def PredefinedAgentDef, target string, workflowJSON json.RawMessage) (string, error) {
	if len(bytes.TrimSpace(workflowJSON)) > 0 {
		filled, err := fillWorkflowTarget(workflowJSON, target)
		if err != nil {
			return errorResult("workflow must be valid JSON: " + err.Error()), nil
		}
		return t.runDAGWorkflow(ctx, filled)
	}

	wf, err := generateAnalysisWorkflow(target, def)
	if err != nil {
		return errorResult("analysis workflow generation: " + err.Error()), nil
	}
	wfJSON, err := json.Marshal(wf)
	if err != nil {
		return "", fmt.Errorf("analysis workflow: marshal: %w", err)
	}
	return t.runDAGWorkflow(ctx, wfJSON)
}

// fillWorkflowTarget applies the analysis target to caller-provided workflow
// prompts while preserving the caller's step IDs and dependency graph.
func fillWorkflowTarget(raw json.RawMessage, target string) (json.RawMessage, error) {
	var wf WorkflowDef
	if err := json.Unmarshal(raw, &wf); err != nil {
		var encoded string
		if stringErr := json.Unmarshal(raw, &encoded); stringErr != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(encoded), &wf); err != nil {
			return nil, err
		}
	}
	if len(wf.Steps) == 0 {
		return nil, fmt.Errorf("workflow must contain at least one step")
	}
	for i := range wf.Steps {
		wf.Steps[i].Prompt = FillPrompt(wf.Steps[i].Prompt, map[string]string{"target": target})
	}
	return json.Marshal(wf)
}

// generateAnalysisWorkflow builds a bounded analysis DAG from the target's
// immediate directory entries. Each entry gets an independent inspection step
// followed by a synthesis step. If the target cannot be inspected, the
// predefined workflow remains a deterministic fallback.
func generateAnalysisWorkflow(target string, def PredefinedAgentDef) (*WorkflowDef, error) {
	entries, err := os.ReadDir(target)
	if err != nil || len(entries) == 0 {
		if def.Workflow == nil {
			return nil, fmt.Errorf("target directory is empty or unreadable")
		}
		steps := make([]WorkflowStepDef, len(def.Workflow.Steps))
		for i, s := range def.Workflow.Steps {
			steps[i] = WorkflowStepDef{ID: s.ID, Prompt: FillPrompt(s.Prompt, map[string]string{"target": target}), Deps: append([]string(nil), s.Deps...)}
		}
		return &WorkflowDef{Steps: steps}, nil
	}

	const maxEntries = 24
	steps := []WorkflowStepDef{{
		ID:     "A1",
		Prompt: fmt.Sprintf("Build a concise inventory of %s before analyzing its entries.", target),
	}}
	deps := []string{"A1"}
	for i, entry := range entries {
		if i >= maxEntries {
			break
		}
		name := entry.Name()
		if name == ".git" || name == "node_modules" || name == "vendor" || strings.HasPrefix(name, ".") {
			continue
		}
		id := fmt.Sprintf("B%d", len(steps))
		path := filepath.Join(target, name)
		steps = append(steps, WorkflowStepDef{
			ID:     id,
			Prompt: fmt.Sprintf("Analyze entry %s (under %s): structure, responsibilities, dependencies, risks, and useful improvement opportunities. Use the inventory from {{A1.output}}.", path, target),
			Deps:   []string{"A1"},
		})
		deps = append(deps, id)
	}
	steps = append(steps, WorkflowStepDef{
		ID:     "C1",
		Prompt: fmt.Sprintf("Synthesize the project analysis for %s from {{A1.output}} and all entry reports. Produce one structured, actionable report.", target),
		Deps:   deps,
	})
	return &WorkflowDef{Steps: steps}, nil
}
