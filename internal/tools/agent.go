package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// AgentTools provides tools for launching sub-agents and managing workflows.
// Sub-agents use the configured chat model to process tasks and return results.
// Workflow tools execute multiple sub-agents with DAG-based dependency ordering
// and CPU-core concurrency limiting.
//
// The Analysis tool uses a predefined "analysis" agent template and supports
// both single-agent (detailed prompt) and workflow (multi-step DAG) modes.
type AgentTools struct {
	StartAgent    *GuardedTool
	StartWorkflow *GuardedTool
	Analysis      *GuardedTool
	Summarize     *GuardedTool
	// Managed sub-agent lifecycle tools (B1).
	AgentSpawn     *GuardedTool
	AgentWait      *GuardedTool
	AgentResult    *GuardedTool
	AgentSendInput *GuardedTool
	AgentResume    *GuardedTool
	AgentAssign    *GuardedTool
	AgentCancel    *GuardedTool
	AgentList      *GuardedTool
	// Review is the chunked code-review pipeline (B3).
	Review    *GuardedTool
	chatModel model.BaseChatModel
}

// agentRegistryTimeout bounds the seven agent_* tools that only touch the
// in-memory subagent registry. They do no I/O, so any generous bound works;
// what matters is that it is not 0, which would expire the context before the
// tool body ran. agent_wait is the exception and uses NoTimeout -- it is
// SUPPOSED to block until the subagent finishes, and a deadline wrapped around
// that would cut off exactly the wait the caller asked for.
const agentRegistryTimeout = 30 * time.Second

// NewAgentTools builds agent management tools.
// chatModel is used to run sub-agent queries. When nil, the tools return an error
// prompting the user to configure a model first.
func NewAgentTools(chatModel model.BaseChatModel) *AgentTools {
	t := &AgentTools{
		chatModel: chatModel,
	}
	t.StartAgent = NewGuardedTool(
		"agent_start", "Agent",
		"Start a sub-agent to execute a task. The sub-agent runs its own ReAct loop "+
			"with the LLM, inheriting:\n"+
			"  • Permission profile from the parent\n"+
			"  • System instruction (AGENT.md/CLAUDE.md) from the parent\n"+
			"  • Tool set — use \"tools\" to restrict, omit to inherit all\n"+
			"\nUse \"instruction\" to override the sub-agent's system prompt; "+
			"omit to inherit the parent's instruction (including project AGENT.md/CLAUDE.md).\n"+
			"\nDefaults:\n"+
			"  • Shell environment: same as parent (see shell_run tool description for defaults)\n"+
			"  • Max nesting depth: 3 levels (agent → sub-agent → sub-sub-agent)",
		600*time.Second,
		params(map[string]*schema.ParameterInfo{
			"prompt":      {Type: schema.String, Desc: "The task description or prompt for the sub-agent to process", Required: true},
			"tools":       {Type: schema.String, Desc: "JSON array of tool names the sub-agent may use, e.g. [\"fs_read\",\"fs_search\"]. Omit to inherit the parent's full tool set.", Required: false},
			"instruction": {Type: schema.String, Desc: "Optional system instruction override for the sub-agent. When omitted, the sub-agent inherits the parent's instruction (including project AGENT.md/CLAUDE.md).", Required: false},
		}),
		t.streamStartAgent,
	)
	t.StartWorkflow = NewGuardedTool(
		"workflow_start", "Workflow",
		"Launch a DAG-based workflow of multiple sub-agents. "+
			"Each sub-agent inherits the parent's system instruction (AGENT.md/CLAUDE.md) and permission profile.\n\n"+
			"Supports two modes:\n\n"+
			"1) FLAT mode (backwards-compatible): {\"tasks\":[\"prompt1\",\"prompt2\",...]} — all tasks run in parallel (concurrency = CPU cores).\n\n"+
			"2) DAG mode: {\"steps\":[{...}]} — define a directed acyclic graph where each step can depend on previous step outputs. "+
			"The workflow is executed level-by-level: all ready steps run concurrently (concurrency = CPU cores). "+
			"Step IDs can include numeric ranges for fan-out/fan-in, e.g. \"B1-8\" expands to B1,B2,...,B8. "+
			"Deps support the same range syntax. Prompts support template interpolation via {{stepID.output}} for previous step results, "+
			"and {{self.index}}, {{self.count}}, {{self.id}} for expanded steps. "+
			"Returns a map of step ID to result/error.\n\n"+
			"Defaults:\n"+
			"  • Shell environment: same as parent (see shell_run tool description for defaults)\n"+
			"  • Concurrency: number of CPU cores\n"+
			"  • Max nesting depth: 3 levels (workflow → sub-agent → sub-sub-agent)",
		1800*time.Second,
		params(map[string]*schema.ParameterInfo{
			"tasks":    {Type: schema.String, Desc: "[FLAT MODE] JSON array of task prompt strings, e.g. [\"task1\", \"task2\"]", Required: false},
			"workflow": {Type: schema.String, Desc: "[DAG MODE] JSON object with a \"steps\" array defining the workflow DAG", Required: false},
		}),
		t.streamStartWorkflow,
	)

	t.Analysis = NewGuardedTool(
		"analysis", "Analysis",
		"Analyze code/project structure, architecture, dependencies, and quality.\n\n"+
			"mode (required): 'workflow' or 'agent'\n"+
			"  • 'workflow': launches a multi-step DAG — multiple sub-agents analyze different aspects "+
			"(structure / architecture / dependencies / quality) concurrently, then synthesize a final report. "+
			"RECOMMENDED for thorough project or directory analysis.\n"+
			"  • 'agent': single sub-agent quick pass. Use for fast single-file or targeted analysis.\n\n"+
			"target: file or directory path to analyze.\n"+
			"workflow (optional): custom workflow JSON; omit to auto-generate from target directory.\n\n"+
			"Analysis, agent, and workflow tools are never passed to child agents.",
		600*time.Second,
		params(map[string]*schema.ParameterInfo{
			"target":   {Type: schema.String, Desc: "File or directory path to analyze", Required: true},
			"mode":     {Type: schema.String, Desc: "Required: 'workflow' (multi-agent fan-out + synthesis, recommended) or 'agent' (single quick pass)", Required: true},
			"workflow": {Type: schema.String, Desc: "Optional workflow JSON for mode='workflow'; when omitted, auto-generate from target", Required: false},
		}),
		t.streamAnalysis,
	)
	t.Summarize = NewGuardedTool(
		"summarize", "Summarize",
		"Read a file and produce a structured summary. Handles large files by paging "+
			"with fs_read. Use this to condense a long file (or a spilled tool output under "+
			".yanshi/tmp/spillover/) instead of reading it whole.\n"+
			"  • path — file to summarize (project file or spilled temp file).\n"+
			"  • focus (optional) — what to emphasize, e.g. \"error handling\".\n"+
			"  • max_lines (optional, default 50) — target summary length.",
		300*time.Second,
		params(map[string]*schema.ParameterInfo{
			"path":      {Type: schema.String, Desc: "file path to summarize", Required: true},
			"focus":     {Type: schema.String, Desc: "optional focus, e.g. \"error handling\""},
			"max_lines": {Type: schema.Integer, Desc: "target max summary length in lines (default 50)"},
		}),
		t.streamSummarize,
	)
	// Managed sub-agent lifecycle tools (B1).
	t.AgentSpawn = NewGuardedTool("agent_spawn", "Agent", "Spawn a managed subagent and return its id immediately.", agentRegistryTimeout,
		params(map[string]*schema.ParameterInfo{
			"prompt": {Type: schema.String, Required: true},
			// Roles are validated (unknown names are rejected), so the legal set
			// has to be visible to the model here — otherwise it guesses.
			"role":      {Type: schema.String, Desc: "one of: " + strings.Join(AgentRoleNames(), ", ") + " (default general; \"custom\" requires an explicit tools list)"},
			"tools":     {Type: schema.String, Desc: "optional JSON array of tool names; intersected with the role's own allowlist"},
			"nickname":  {Type: schema.String},
			"model":     {Type: schema.String},
			"reasoning": {Type: schema.String},
		}), t.streamAgentSpawn)

	t.AgentWait = NewGuardedTool("agent_wait", "Agent", "Wait for a managed subagent to reach a terminal state.", NoTimeout,
		params(map[string]*schema.ParameterInfo{
			"agent_id": {Type: schema.String, Required: true},
			"timeout":  {Type: schema.Integer},
		}), t.streamAgentWait)

	t.AgentResult = NewGuardedTool("agent_result", "Agent", "Return a snapshot of a managed subagent.", agentRegistryTimeout,
		params(map[string]*schema.ParameterInfo{
			"agent_id": {Type: schema.String, Required: true},
		}), t.streamAgentResult)

	t.AgentSendInput = NewGuardedTool("agent_send_input", "Agent", "Queue follow-up input for a managed subagent.", agentRegistryTimeout,
		params(map[string]*schema.ParameterInfo{
			"agent_id":  {Type: schema.String, Required: true},
			"text":      {Type: schema.String, Required: true},
			"interrupt": {Type: schema.Boolean},
		}), t.streamAgentSendInput)

	t.AgentResume = NewGuardedTool("agent_resume", "Agent", "Resume an interrupted or completed subagent.", agentRegistryTimeout,
		params(map[string]*schema.ParameterInfo{
			"agent_id": {Type: schema.String, Required: true},
			"prompt":   {Type: schema.String},
		}), t.streamAgentResume)

	t.AgentAssign = NewGuardedTool("agent_assign", "Agent", "Assign a goal to a managed subagent.", agentRegistryTimeout,
		params(map[string]*schema.ParameterInfo{
			"agent_id":   {Type: schema.String, Required: true},
			"assignment": {Type: schema.String, Required: true},
		}), t.streamAgentAssign)

	t.AgentCancel = NewGuardedTool("agent_cancel", "Agent", "Cancel a managed subagent.", agentRegistryTimeout,
		params(map[string]*schema.ParameterInfo{
			"agent_id": {Type: schema.String, Required: true},
		}), t.streamAgentCancel)

	t.AgentList = NewGuardedTool("agent_list", "Agent", "List managed subagents.", agentRegistryTimeout,
		params(map[string]*schema.ParameterInfo{
			"include_archived": {Type: schema.Boolean},
		}), t.streamAgentList)

	// B3: Review tool — chunked code-review pipeline via sub-agents.
	t.Review = NewGuardedTool("review", "Code review",
		"Review a pull-request diff in 48 KiB chunks via the review sub-agent, "+
			"deduplicate and sort findings, and persist large outputs as artifacts.",
		10*time.Minute,
		params(map[string]*schema.ParameterInfo{
			"diff": {Type: schema.String,
				Desc: "Unified diff text to review. Omit it and pass base instead to review the repository directly."},
			"base": {Type: schema.String, Enum: []string{"working_tree", "base_ref", "commit"},
				Desc: "What to review when diff is omitted"},
			"ref":     {Type: schema.String, Desc: "Ref for base_ref / commit"},
			"task_id": {Type: schema.String, Desc: "Task ID for artifact storage"},
			"repo":    {Type: schema.String, Desc: "GitHub repo (owner/name) for context"},
			"number":  {Type: schema.Integer, Desc: "PR number for context"},
		}),
		streamReviewTool)

	return t
}

// streamReviewTool is the StreamFunc wrapper around streamReview. It parses the
// args, calls streamReview, and pushes the result as a single ToolChunk.
func streamReviewTool(ctx context.Context, argsJSON string) <-chan ToolChunk {
	ch := make(chan ToolChunk, 1)
	go func() {
		defer close(ch)
		var in reviewInput
		if err := ParseArgs(argsJSON, &in); err != nil {
			pushErrChunk(ch, err)
			return
		}
		out, err := streamReview(ctx, in)
		if err != nil {
			pushErrChunk(ch, err)
			return
		}
		ch <- ToolChunk{Text: out, Result: out}
	}()
	return ch
}

// Tools returns all 12 agent tools (4 legacy + 8 managed lifecycle).
func (t *AgentTools) Tools() []*GuardedTool {
	return []*GuardedTool{
		t.StartAgent, t.StartWorkflow, t.Analysis, t.Summarize,
		t.AgentSpawn, t.AgentWait, t.AgentResult, t.AgentSendInput,
		t.AgentResume, t.AgentAssign, t.AgentCancel, t.AgentList,
	}
}

// ---------------------------------------------------------------------------
// agent_start
// ---------------------------------------------------------------------------

type agentStartArgs struct {
	Prompt      string `json:"prompt"`
	Tools       string `json:"tools"`       // JSON array of tool names, e.g. ["fs_read","fs_search"]; "" = inherit all
	Instruction string `json:"instruction"` // optional system prompt override; "" = inherit parent's
}

// bindSubAgentProgress wires a SubAgentProgress callback into ctx that, on each
// sub-agent event, pushes the recomputed Status (stats summary) and Text (activity
// line) to ch. stepPrefix labels activity lines ("" for single-agent tools; a
// workflow step passes "B1: " etc.). Returns the derived ctx plus a no-op finalize
// (placeholder for future completion-state rendering). Caller defers finalize().
func bindSubAgentProgress(ctx context.Context, ch chan<- ToolChunk, stepPrefix string) (context.Context, func()) {
	start := time.Now()
	var toolCalls int
	var tokens int
	pushStatus := func() {
		ch <- ToolChunk{Status: fmt.Sprintf("%d tools %s %s", toolCalls, formatTokens(tokens), formatDur(time.Since(start)))}
	}
	cb := func(ev SubAgentEvent) {
		switch ev.Kind {
		case SubAgentToolStart:
			toolCalls++
			ch <- ToolChunk{Text: stepPrefix + ev.ToolDisplay + "(" + ev.ToolArgs + ")\n"}
			pushStatus()
		case SubAgentTokens:
			tokens += ev.Tokens
			pushStatus()
		}
	}
	return WithSubAgentProgress(ctx, cb), func() {}
}

// naturalIDLess compares step IDs by (alpha prefix, numeric suffix).
// "B2" < "B15" (numeric, not lexicographic). "A1" < "B1" (alpha).
func naturalIDLess(a, b string) bool {
	di, dj := digitsStart(a), digitsStart(b)
	pa, pb := a[:di], b[:dj]
	if pa != pb {
		return pa < pb
	}
	na, _ := strconv.Atoi(a[di:])
	nb, _ := strconv.Atoi(b[dj:])
	return na < nb
}

func digitsStart(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			return i
		}
	}
	return len(s)
}

// formatDur renders d as "3s" / "1m9s" / "2m" for Status panels.
func formatDur(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	m := int(d.Minutes())
	s := int(d.Seconds()) - m*60
	return fmt.Sprintf("%dm%ds", m, s)
}

// formatTokens renders a raw token count with K/M/B suffixes:
// 500 → "500", 1200 → "1.2K", 56000 → "56K", 4065000 → "4.1M".
func formatTokens(n int) string {
	const k, m, b = 1000, 1000000, 1000000000
	var val float64
	var suffix string
	switch {
	case n >= b:
		val, suffix = float64(n)/float64(b), "B"
	case n >= m:
		val, suffix = float64(n)/float64(m), "M"
	case n >= k:
		val, suffix = float64(n)/float64(k), "K"
	default:
		return strconv.Itoa(n)
	}
	s := strconv.FormatFloat(val, 'f', 1, 64)
	s = strings.TrimSuffix(s, ".0")
	return s + suffix
}

func (t *AgentTools) streamStartAgent(ctx context.Context, argsJSON string) <-chan ToolChunk {
	ch := make(chan ToolChunk, 16)
	go func() {
		defer close(ch)
		var a agentStartArgs
		if err := ParseArgs(argsJSON, &a); err != nil {
			pushErrChunk(ch, err)
			return
		}
		if a.Prompt == "" {
			pushErrChunk(ch, fmt.Errorf("prompt must not be empty"))
			return
		}
		if t.chatModel == nil {
			pushErrChunk(ch, fmt.Errorf("no chat model configured; cannot start sub-agent"))
			return
		}
		allowed, perr := parseToolList(a.Tools)
		if perr != nil {
			pushErrChunk(ch, perr)
			return
		}
		sctx, finalize := bindSubAgentProgress(ctx, ch, "")
		result, err := t.runSubAgent(WithLeafSubAgentTools(sctx), a.Prompt, allowed, a.Instruction)
		finalize()
		if err != nil {
			pushErrChunk(ch, err)
			return
		}
		// agent_start is a TERMINAL path: this result goes straight back to the
		// parent agent, so the EVIDENCE section is re-surfaced as an explicit
		// parent-facing hint (ledger B1/M04b: "the parent can consume EVIDENCE").
		// The DAG and workflow call sites deliberately do NOT do this — see the
		// comments there.
		result = ParentWorkingSetHint(result)
		ch <- ToolChunk{Result: result} // final conclusion -> model only (context hygiene)
	}()
	return ch
}

// parseToolList parses a "tools" arg that may be a JSON array of strings OR a
// JSON string wrapping such an array (models sometimes double-encode it).
// Returns nil (inherit all) for empty input.
func parseToolList(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var arr []string
	if err := json.Unmarshal([]byte(raw), &arr); err == nil {
		return arr, nil
	}
	var inner string
	if err := json.Unmarshal([]byte(raw), &inner); err == nil {
		if err := json.Unmarshal([]byte(inner), &arr); err == nil {
			return arr, nil
		}
	}
	return nil, fmt.Errorf("tools must be a JSON array of tool names, e.g. [\"fs_read\"]")
}

// ---------------------------------------------------------------------------
// sub-agent execution
// ---------------------------------------------------------------------------

// runSubAgent executes a single sub-agent task. When a SubAgentRunner is bound
// in ctx (the orchestrator installs one), the sub-agent is a REAL ReAct agent
// with the parent's permission profile, the allowedTools subset, and inherits
// the parent's instruction (unless instructionOverride is non-empty). With no
// runner bound (e.g. in a unit test or a model-only setup), it falls back to a
// bare model Generate (no tools), the legacy behavior. allowedTools is the
// subset of tool names the sub-agent may use; nil/empty inherits the parent's
// full tool set. instructionOverride, when non-empty, replaces the inherited
// system instruction for this sub-agent.
func (t *AgentTools) runSubAgent(ctx context.Context, prompt string, allowedTools []string, instructionOverride string) (string, error) {
	if runner := SubAgentRunnerFromContext(ctx); runner != nil {
		return runner(ctx, prompt, allowedTools, instructionOverride)
	}
	msgs := []*schema.Message{
		schema.UserMessage(prompt),
	}

	resp, err := t.chatModel.Generate(ctx, msgs)
	if err != nil {
		return "", fmt.Errorf("sub-agent: generate: %w", err)
	}
	if resp == nil {
		return "", fmt.Errorf("sub-agent: empty response from model")
	}
	return resp.Content, nil
}
