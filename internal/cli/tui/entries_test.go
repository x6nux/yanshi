package tui

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newSpinner returns a ready-to-render spinner for toolEntry.render tests,
// matching the pattern used across the model_test.go suite (newModel + .spinner).
func newSpinner() spinner.Model {
	return newModel(&fakeSession{}, "/proj").spinner
}

// ---- B3: tool-call display by semantics (silent / tail / agent / normal) ----

// TestToolRenderSilentNoResultLine proves a Silent-class tool (fs_read) renders
// its header WITHOUT a ⎿ result line: these tools return large file dumps whose
// size is already conveyed by the header's size hint ("12.3 KB · 245 lines"),
// and a full result line would just push prior context off screen. The result
// is still available via ctrl+o expand.
func TestToolRenderSilentNoResultLine(t *testing.T) {
	e := &toolEntry{name: "fs_read", args: `{"path":"foo.go"}`, root: ".", status: "ok", result: "1\thello"}
	out := e.render(80, newSpinner())
	if strings.Contains(out, "⎿") {
		t.Fatalf("Silent 不应有结果行:\n%s", out)
	}
	if !strings.Contains(out, "Read") {
		t.Fatalf("应有 Read 头部")
	}
}

// TestToolRenderTailRunningLimited proves a Tail-class running tool (shell_run)
// caps its live tail output at ~10 lines so a chatty command cannot flood the
// transcript. Progress lines beyond the cap are dropped from the rendered tail.
func TestToolRenderTailRunningLimited(t *testing.T) {
	e := &toolEntry{
		name:     "shell_run",
		args:     `{"command":"ls"}`,
		status:   "running",
		progress: []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l"},
	}
	out := e.render(80, newSpinner())
	if strings.Count(out, "\n") > 14 {
		t.Fatalf("Tail running 应限高 ~10 行:\n%s", out)
	}
}

// TestToolRenderAgentNoStandaloneThinking proves an Agent-class tool
// (agent_start) never renders a standalone "Thinking…" block — the nested
// agent's reasoning is attributed to the tool's nestedThought (rendered
// indented inside the block), not emitted as a sibling thinkingEntry above it.
// Without this guard, a parent tool call sandwiched between its own reasoning
// and its child's would visually invert the parent → child relationship.
func TestToolRenderAgentNoStandaloneThinking(t *testing.T) {
	e := &toolEntry{name: "agent_start", args: `{"task":"x"}`, status: "ok"}
	out := e.render(80, newSpinner())
	if strings.Contains(out, "Thinking…") {
		t.Fatalf("Agent 不应显独立思考块:\n%s", out)
	}
}

// TestToolRenderNormalKeepsResultLine proves a Normal-class tool (memory_save)
// keeps the historic ⎿ one-line result behavior — the B3 refactor must not
// regress tools that don't fall into silent/tail/agent.
func TestToolRenderNormalKeepsResultLine(t *testing.T) {
	e := &toolEntry{name: "memory_save", args: `{"k":"v"}`, root: ".", status: "ok", result: "saved"}
	out := e.render(80, newSpinner())
	if !strings.Contains(out, "⎿") {
		t.Fatalf("Normal 应有结果行:\n%s", out)
	}
}

// TestToolRenderAgentShowsNestedThought proves a nested agent's buffered
// reasoning renders indented INSIDE the agent block (not as a standalone
// thinkingEntry sibling) when expanded. This is the display half of the nested
// buffer: applyEvent routes "thinking" deltas into toolEntry.nestedThought when
// a nested agent tool is running; renderAgent surfaces them in the expanded
// view. (The collapsed done view shows the one-line summary instead — see
// TestToolRenderAgentProgressDoneSummary.)
func TestToolRenderAgentShowsNestedThought(t *testing.T) {
	e := &toolEntry{
		name:          "agent_start",
		args:          `{"task":"x"}`,
		status:        "ok",
		nested:        true,
		nestedThought: "planning the work",
		expanded:      true,
	}
	out := e.render(80, newSpinner())
	if !strings.Contains(out, "planning the work") {
		t.Fatalf("Agent 嵌套思考应渲染（expanded）:\n%s", out)
	}
	if strings.Contains(out, "Thinking…") {
		t.Fatalf("Agent 不应显独立思考块")
	}
}

// ---- Sub-agent inline progress: 10 lines running / 3 lines done ----

// TestToolRenderAgentProgressRunningLimited proves a Running Agent-class tool
// caps its nestedProgress tail at ~10 lines (mirroring shell_run's renderTail),
// so a chatty sub-agent (many tool calls + output) cannot flood the transcript.
// The cap is on BODY lines (excluding the header + trailing blank).
func TestToolRenderAgentProgressRunningLimited(t *testing.T) {
	prog := make([]string, 20)
	for i := range prog {
		prog[i] = "line " + string(rune('a'+i))
	}
	e := &toolEntry{
		name:           "analysis",
		args:           `{"prompt":"x"}`,
		status:         "running",
		nested:         true,
		nestedProgress: prog,
	}
	out := e.render(80, newSpinner())
	// Body lives between the header line and the trailing "\n\n". Count lines
	// that come from nestedProgress (must be ≤10); "line k" is the 11th entry
	// (index 10) and should be the FIRST shown when the tail is 10-wide.
	if strings.Contains(out, "line a") {
		t.Fatalf("running 应只显最后 10 行，不应包含 line a:\n%s", out)
	}
	if !strings.Contains(out, "line k") {
		t.Fatalf("running 应显最后 10 行（含 line k）:\n%s", out)
	}
}

// TestToolRenderAgentProgressDoneSummary proves a DONE (ok/error) Agent-class
// tool replaces the old 3-line tail with a one-line "Done (…)" summary, so the
// resolved block stays compact. The nestedProgress tail lines must NOT appear
// in the collapsed done view (they remain available via ctrl+o expand — see
// TestToolRenderAgentProgressExpanded). The summary reflects nestedToolUses,
// nestedTokens, and the startedAt→endedAt duration.
func TestToolRenderAgentProgressDoneSummary(t *testing.T) {
	prog := make([]string, 10)
	for i := range prog {
		prog[i] = "line " + string(rune('a'+i))
	}
	start := time.Now().Add(-17*time.Minute - 55*time.Second)
	e := &toolEntry{
		name:           "analysis",
		args:           `{"prompt":"x"}`,
		status:         "ok",
		nested:         true,
		nestedProgress: prog,
		nestedToolUses: 38,
		nestedTokens:   86700,
		startedAt:      start,
		endedAt:        time.Now(),
	}
	out := e.render(80, newSpinner())
	// C3 format: nested tool lines (Agent(…)) ARE shown in collapsed done view
	// alongside the aggregate summary — the user spec requires per-tool lines
	// to always be visible. The full 10-line body is trimmed to a window.
	if !strings.Contains(out, "Done (") {
		t.Fatalf("done 应显摘要行:\n%s", out)
	}
	if !strings.Contains(out, "38 tool uses") {
		t.Fatalf("摘要应含 tool uses:\n%s", out)
	}
	if !strings.Contains(out, "86.7k tokens") {
		t.Fatalf("摘要应含 tokens:\n%s", out)
	}
	if !strings.Contains(out, "17m 55s") {
		t.Fatalf("摘要应含时长:\n%s", out)
	}
}

// TestToolRenderAgentProgressDoneSummaryOmitsZeroSegs proves the done summary
// OMITS segments whose data is zero/missing rather than showing placeholders:
// a sub-agent that reported no usage (FakeModel) omits the tokens segment, and
// one that called no tools omits the tool-uses segment. The remaining segments
// still render so the summary reads naturally.
func TestToolRenderAgentProgressDoneSummaryOmitsZeroSegs(t *testing.T) {
	e := &toolEntry{
		name:           "analysis",
		args:           `{"prompt":"x"}`,
		status:         "ok",
		nested:         true,
		nestedProgress: []string{"thinking…"},
		nestedToolUses: 3,
		// nestedTokens 0 (provider omitted usage) → tokens segment omitted.
		startedAt: time.Now().Add(-5 * time.Second),
		endedAt:   time.Now(),
	}
	out := e.render(80, newSpinner())
	if !strings.Contains(out, "3 tool uses") {
		t.Fatalf("应显 tool uses:\n%s", out)
	}
	if !strings.Contains(out, "5s") {
		t.Fatalf("应显时长:\n%s", out)
	}
	// No "tokens" segment.
	if strings.Contains(out, "tokens") {
		t.Fatalf("nestedTokens 为 0 时应省略 tokens 段:\n%s", out)
	}
}

// TestToolRenderAgentProgressDoneSummaryAllZero proves the degenerate case where
// no segment has data renders a bare "Done" (no empty parentheses in the
// summary — the header's own "(args)" parens are unrelated).
func TestToolRenderAgentProgressDoneSummaryAllZero(t *testing.T) {
	e := &toolEntry{
		name:   "analysis",
		args:   `{"prompt":"x"}`,
		status: "ok",
		nested: true,
	}
	out := e.render(80, newSpinner())
	if !strings.Contains(out, "Done") {
		t.Fatalf("至少应显 Done:\n%s", out)
	}
	if strings.Contains(out, "Done (") {
		t.Fatalf("全 0 时摘要不应有括号段:\n%s", out)
	}
}

// TestToolRenderAgentErrorShowsMessageAndSummary proves an errored Agent-class
// tool surfaces BOTH the failure reason (Error: <msg>) AND the done summary —
// routing errors through renderAgent (for the summary) must not hide why the
// sub-agent failed. The ✗ glyph in the header carries the status; the Error:
// line carries the reason; the summary carries the work done before the failure.
func TestToolRenderAgentErrorShowsMessageAndSummary(t *testing.T) {
	e := &toolEntry{
		name:           "analysis",
		args:           `{"prompt":"x"}`,
		status:         "error",
		nested:         true,
		result:         "The agent reached its maximum step limit.",
		nestedToolUses: 5,
		nestedTokens:   12000,
		startedAt:      time.Now().Add(-3 * time.Minute),
		endedAt:        time.Now(),
	}
	out := e.render(80, newSpinner())
	if !strings.Contains(out, "Error:") {
		t.Fatalf("error 应显 Error: 行:\n%s", out)
	}
	if !strings.Contains(out, "maximum step limit") {
		t.Fatalf("error 应显具体错误信息:\n%s", out)
	}
	if !strings.Contains(out, "5 tool uses") {
		t.Fatalf("error 也应显摘要（工作量）:\n%s", out)
	}
	if !strings.Contains(out, "12k tokens") {
		t.Fatalf("error 也应显 tokens:\n%s", out)
	}
}

// TestToolRenderAgentProgressExpanded shows ctrl+o expand renders the FULL
// nestedProgress (no tail cap) PLUS the done summary, so the user can inspect
// every sub-agent step while still seeing the totals.
func TestToolRenderAgentProgressExpanded(t *testing.T) {
	prog := []string{"line a", "line b", "line c", "line d", "line e"}
	e := &toolEntry{
		name:           "analysis",
		args:           `{"prompt":"x"}`,
		status:         "ok",
		nested:         true,
		nestedProgress: prog,
		nestedToolUses: 2,
		nestedTokens:   3000,
		expanded:       true,
	}
	out := e.render(80, newSpinner())
	if !strings.Contains(out, "line a") || !strings.Contains(out, "line e") {
		t.Fatalf("expanded 应显全部:\n%s", out)
	}
	if !strings.Contains(out, "2 tool uses") {
		t.Fatalf("expanded 也应显摘要:\n%s", out)
	}
}

// TestToolRenderAgentProgressPreferredOverThought proves that when BOTH
// nestedProgress and nestedThought are present, the observable activity
// (nestedProgress) wins for the EXPANDED body — it is more informative than the
// child's reasoning. nestedThought is the fallback only when nestedProgress is
// empty. (The collapsed done view shows the summary line for either; this test
// uses expanded mode to inspect the body choice directly.)
func TestToolRenderAgentProgressPreferredOverThought(t *testing.T) {
	e := &toolEntry{
		name:           "analysis",
		args:           `{"prompt":"x"}`,
		status:         "ok",
		nested:         true,
		nestedProgress: []string{"Agent(Read) ◌"},
		nestedThought:  "pondering",
		expanded:       true,
	}
	out := e.render(80, newSpinner())
	if !strings.Contains(out, "Agent(Read)") {
		t.Fatalf("应优先 nestedProgress:\n%s", out)
	}
	if strings.Contains(out, "pondering") {
		t.Fatalf("nestedProgress 非空时不应显 nestedThought:\n%s", out)
	}

	// Fallback: empty nestedProgress → nestedThought surfaces as the body.
	e2 := &toolEntry{
		name:          "analysis",
		args:          `{"prompt":"x"}`,
		status:        "ok",
		nested:        true,
		nestedThought: "pondering",
		expanded:      true,
	}
	out2 := e2.render(80, newSpinner())
	if !strings.Contains(out2, "pondering") {
		t.Fatalf("nestedProgress 为空时应用 nestedThought 回退:\n%s", out2)
	}
}

// ---- Workflow agents progress (N/M) in done summary ----

// TestToolRenderWorkflowDoneSummaryAgents proves a DONE workflow_start block
// renders "N/M agents" as the FIRST summary segment (not "N tool uses"),
// sourced from nestedAgentsDone/Total (delivered by nested_progress frames).
// This is the display half of the workflow progress feature: the workflow tool
// emits nested_progress after each task; applyEvent attributes it here; the
// summary then conveys the workflow's fan-out shape. Tokens + duration still
// follow, matching the agent_start/analysis format.
func TestToolRenderWorkflowDoneSummaryAgents(t *testing.T) {
	start := time.Now().Add(-12*time.Minute - 30*time.Second)
	e := &toolEntry{
		name:              "workflow_start",
		args:              `{"tasks":["a","b","c","d","e"]}`,
		status:            "ok",
		nested:            true,
		nestedAgentsDone:  5,
		nestedAgentsTotal: 5,
		nestedTokens:      42000,
		startedAt:         start,
		endedAt:           time.Now(),
	}
	out := e.render(80, newSpinner())
	if !strings.Contains(out, "Done (") {
		t.Fatalf("done 应显摘要行:\n%s", out)
	}
	if !strings.Contains(out, "5/5 agents") {
		t.Fatalf("workflow 摘要应含 '5/5 agents':\n%s", out)
	}
	if !strings.Contains(out, "42k tokens") {
		t.Fatalf("workflow 摘要应含累计 tokens:\n%s", out)
	}
	if !strings.Contains(out, "12m 30s") {
		t.Fatalf("workflow 摘要应含时长:\n%s", out)
	}
	// Must NOT show "tool uses" — that's the agent_start/analysis format.
	if strings.Contains(out, "tool uses") {
		t.Fatalf("workflow_start 不应显 'tool uses'（那是 agent_start/analysis 格式）:\n%s", out)
	}
}

// TestToolRenderWorkflowDoneSummaryPartialProgress proves the agents segment
// shows the LAST reported progress (cumulative done/total), so a workflow that
// lost a late nested_progress frame (WS backpressure) still reports an honest
// partial count like "4/5 agents" rather than hiding the segment.
func TestToolRenderWorkflowDoneSummaryPartialProgress(t *testing.T) {
	e := &toolEntry{
		name:              "workflow_start",
		args:              `{"tasks":["a","b","c","d","e"]}`,
		status:            "ok",
		nested:            true,
		nestedAgentsDone:  4,
		nestedAgentsTotal: 5,
	}
	out := e.render(80, newSpinner())
	if !strings.Contains(out, "4/5 agents") {
		t.Fatalf("应显部分进度 '4/5 agents':\n%s", out)
	}
}

// TestToolRenderWorkflowDoneSummaryOmitsAgentsWhenNoProgress proves that when
// no nested_progress frame arrived (no SubAgentEmit bound — headless CLI), the
// workflow_start summary OMITS the agents segment entirely rather than showing
// a misleading "0/0 agents". Tokens/duration (when present) still render.
func TestToolRenderWorkflowDoneSummaryOmitsAgentsWhenNoProgress(t *testing.T) {
	e := &toolEntry{
		name:         "workflow_start",
		args:         `{"tasks":["a","b"]}`,
		status:       "ok",
		nested:       true,
		nestedTokens: 1500,
		startedAt:    time.Now().Add(-5 * time.Second),
		endedAt:      time.Now(),
	}
	out := e.render(80, newSpinner())
	if strings.Contains(out, "agents") {
		t.Fatalf("无 nested_progress 时应省略 agents 段:\n%s", out)
	}
	if !strings.Contains(out, "1.5k tokens") {
		t.Fatalf("tokens 段仍应显示:\n%s", out)
	}
	if strings.Contains(out, "tool uses") {
		t.Fatalf("workflow_start 绝不应显 'tool uses':\n%s", out)
	}
}

// TestToolRenderAgentStartDoneSummaryStillToolUses proves agent_start (and by
// extension analysis) KEEPS the "N tool uses" format after the workflow_start
// branch was added — the branching must not leak. The agents fields are
// irrelevant for agent_start (it never emits nested_progress).
func TestToolRenderAgentStartDoneSummaryStillToolUses(t *testing.T) {
	e := &toolEntry{
		name:           "agent_start",
		args:           `{"prompt":"x"}`,
		status:         "ok",
		nested:         true,
		nestedToolUses: 7,
		nestedTokens:   9000,
		startedAt:      time.Now().Add(-2 * time.Minute),
		endedAt:        time.Now(),
	}
	out := e.render(80, newSpinner())
	if !strings.Contains(out, "7 tool uses") {
		t.Fatalf("agent_start 应保持 'tool uses' 格式:\n%s", out)
	}
	if strings.Contains(out, "agents") {
		t.Fatalf("agent_start 不应显 'agents' 段:\n%s", out)
	}
}

// ---- B9 / T11: only collapse PASTED long messages (typed long → full text) ----

// TestLongTypedNotCollapsed proves a LONG message that was TYPED (pasted=false)
// renders the full text rather than the "[粘贴 #id]" placeholder. The collapse
// is meant for bracketed pastes (whose large size would dominate the
// transcript); a user who slowly types a >240-char message wants to see it
// inline like any other input.
func TestLongTypedNotCollapsed(t *testing.T) {
	long := strings.Repeat("a", 300) // > pasteThresholdChars
	e := &userEntry{text: long, pasteID: pasteID(long), pasted: false}
	out := e.render(80, spinner.Model{})
	if strings.Contains(out, "粘贴") {
		t.Fatalf("手动输入的长文本不应折叠:\n%s", out)
	}
	if !strings.Contains(out, "aaa") {
		t.Fatalf("应显完整文本:\n%s", out)
	}
}

// TestLongPastedCollapsed proves a LONG message that was PASTED (pasted=true)
// still collapses to "[粘贴 #id]" — the original behavior, now gated on the
// pasted flag.
func TestLongPastedCollapsed(t *testing.T) {
	long := strings.Repeat("a", 300)
	e := &userEntry{text: long, pasteID: pasteID(long), pasted: true}
	out := e.render(80, spinner.Model{})
	if !strings.Contains(out, "粘贴") {
		t.Fatalf("粘贴的长文本应折叠:\n%s", out)
	}
}

// ---- RE-J (fix-e1 review of W-E-01): shell_run's JSON-enveloped result ----

// TestToolResultOutput_StripsANSIAfterJSONDecode proves toolResultOutput
// strips ESC bytes from the decoded "output" field. This is the half of
// RE-J applyEvent's ingestion-time strip (model.go) cannot reach: a raw ESC
// byte cannot appear literally inside valid JSON text — encoding/json always
// escapes it — so the literal byte only exists again once json.Unmarshal
// decodes the field, which happens inside this function, after the
// ingestion-time strip already ran on the (still harmless) JSON text.
func TestToolResultOutput_StripsANSIAfterJSONDecode(t *testing.T) {
	raw, err := json.Marshal(map[string]any{
		"output":      "before\x1b[31mRED\x1b[0mafter",
		"exit":        0,
		"duration_ms": 5,
	})
	require.NoError(t, err)
	// Sanity: confirm the premise this test exists to cover — the wire form
	// carries no literal ESC byte at all (encoding/json escaped it), so a
	// strip applied to the wire text itself (as applyEvent's ingestion-time
	// strip does) would find nothing to remove here.
	require.NotContains(t, string(raw), "\x1b", "json.Marshal must not emit a literal ESC byte")

	// Confirm the literal ESC byte genuinely reappears the moment the JSON is
	// decoded — this is the step toolResultOutput's own strip has to guard,
	// independent of applyEvent's ingestion-time strip.
	var v struct {
		Output string `json:"output"`
	}
	require.NoError(t, json.Unmarshal(raw, &v))
	require.Contains(t, v.Output, "\x1b", "decoding must recover the literal ESC byte — otherwise this test is not exercising the bug RE-J found")

	got := toolResultOutput(string(raw))
	assert.NotContains(t, got, "\x1b", "toolResultOutput must strip ANSI after decoding the JSON envelope")
	assert.Equal(t, "beforeREDafter", got)
}

// TestToolResultOutput_UnparseableFallbackAlreadyStripped proves the
// not-JSON fallback path (malformed/plain result) does not need its own
// strip: it returns its input unchanged, and by the time anything reaches
// toolEntry.result that input has already been through applyEvent's
// ingestion-time stripANSI (model.go) — this test documents that invariant
// by feeding toolResultOutput text that already has no ESC byte and
// confirming it survives unparsed, rather than being (harmlessly but
// misleadingly) re-stripped a second time.
func TestToolResultOutput_UnparseableFallbackAlreadyStripped(t *testing.T) {
	got := toolResultOutput("not json, just plain text")
	assert.Equal(t, "not json, just plain text", got)
}
