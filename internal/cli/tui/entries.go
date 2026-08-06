package tui

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/lipgloss"

	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/task/work"
)

// entry is one rendered transcript block. Tool calls are stateful *toolEntry
// pointers so a later tool_result can resolve the spinner to a check/cross in
// place (rather than appending a new block).
type entry interface {
	render(width int, sp spinner.Model) string
}

// userEntry is one rendered user message. It is a pointer (*userEntry) so
// toggleExpand (ctrl+o) can flip its expanded flag in place. Long PASTES
// (>240 chars or >4 lines, identified by pasted=true) collapse to a
// "[粘贴 #<id>]" placeholder in the transcript — the full text is still sent
// to the model unchanged; only the display is collapsed. A long TYPED message
// (pasted=false) renders inline in full, because a user who slowly typed it
// wants to see what they wrote like any other input. The pasted flag is set
// in submit() from m.inputPasted, which is in turn flipped by the bracketed-
// paste detection in Update (tea.KeyMsg.Paste) or the >50-runes heuristic.
type userEntry struct {
	text     string
	pasteID  string // short stable id (first 4 hex of sha256(text)) for the placeholder
	expanded bool   // true → render full text even when long
	pasted   bool   // true → arrived via bracketed paste / bulk-rune drop → eligible for collapse
}

// pasteThresholdChars / pasteThresholdLines gate the collapse. A message is
// "long" when it exceeds either.
const (
	pasteThresholdChars = 240
	pasteThresholdLines = 4
)

// bulkPasteRuneThreshold is the minimum number of runes in a single KeyRunes
// event that we treat as a paste — terminals/IMEs that don't emit the bracketed
// paste sequence still surface a bulk drop as one large KeyRunes, and the IME
// path (Chinese etc.) typically emits one rune per keystroke. The threshold is
// a heuristic, not a hard guarantee: a 51-rune IME composition would be marked
// pasted, but the worst case is one over-eager collapse (the user can ctrl+o
// expand it). Picked to be well above any realistic typed keystroke yet well
// below a real paste.
const bulkPasteRuneThreshold = 50

func (e *userEntry) isLong() bool {
	return len(e.text) > pasteThresholdChars || strings.Count(e.text, "\n")+1 > pasteThresholdLines
}

func (e *userEntry) render(width int, _ spinner.Model) string {
	var body string
	// Collapse only when the message was PASTED AND is long — a typed long
	// message (pasted=false) renders inline in full so the user sees their own
	// composition. See userEntry doc for the why.
	if e.pasted && e.isLong() && !e.expanded {
		body = pasteStyle.Render("[粘贴 #" + e.pasteID + "]")
	} else {
		body = strings.TrimRight(renderMarkdown(width, e.text), "\n")
	}
	return roleUser.Render("you:") + " " + body + "\n"
}

// pasteID returns a short stable id (first 4 hex chars of sha256(content)) used
// to label a collapsed long paste.
func pasteID(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])[:4]
}

// assistantEntry is one rendered assistant turn. thought (when non-nil) is the
// collapsed reasoning block that preceded the answer; it renders BETWEEN the
// "assistant:" label and the content (below the label, above the answer) — a
// pointer so toggleExpand can flip its expanded flag through the
// value-copied model.
type assistantEntry struct {
	text         string
	thought      *thinkingEntry
	continuation bool // true when this block follows an assistant block in the same turn
}

func (e assistantEntry) render(width int, sp spinner.Model) string {
	var b strings.Builder
	if !e.continuation {
		b.WriteString(roleAsst.Render("assistant:"))
	}
	if e.thought != nil {
		// The "✻ Thought for Xs" line goes directly under the label, above the
		// answer content.
		b.WriteString("\n" + strings.TrimRight(e.thought.render(width, sp), "\n"))
	}
	// Trim leading+trailing newlines so the content sits flush under the label
	// (glamour emits a leading newline that would otherwise leave a gap).
	b.WriteString("\n" + strings.Trim(renderMarkdown(width, e.text), "\n") + "\n")
	return b.String()
}

// summaryEntry is the turn-end "Done N tools uses X tokens Y" line. It renders
// in dim grey (doneSummaryStyle, matching the finalized "Thought for Xs" line)
// so it reads as a transcript annotation, not as the assistant's answer.
type summaryEntry struct{ text string }

func (e summaryEntry) render(_ int, _ spinner.Model) string {
	return doneSummaryStyle.Render(e.text) + "\n"
}

type errorEntry struct{ text string }

func (e errorEntry) render(_ int, _ spinner.Model) string {
	return errStyle.Render("✗ "+e.text) + "\n"
}

// seamRestorePromptEntry (B2-RB1) is the in-TUI confirmation prompt for
// /restore-turn <id>. The user confirms by re-typing the exact seam id + "yes".
type seamRestorePromptEntry struct {
	seamID string
}

func (e seamRestorePromptEntry) render(_ int, _ spinner.Model) string {
	return fmt.Sprintf("%s\n%s %s\n",
		toolMeta.Render("  confirm revert to "+e.seamID),
		toolMeta.Render("  type"),
		okStyle.Render("/restore-turn "+e.seamID+" yes"),
	)
}

// seamsEntry (B2-RB1) renders the recent reversible seams. Only pre-turn and
// pre-revert rows are shown (post-turn / post-revert are audit-only). When the
// list is empty the user gets an explicit "(no reversible seams yet)" line.
type seamsEntry struct {
	items []proto.SeamInfo
}

func (e seamsEntry) render(_ int, _ spinner.Model) string {
	var b strings.Builder
	if len(e.items) == 0 {
		b.WriteString(toolMeta.Render("  (no reversible seams yet)") + "\n")
		return b.String()
	}
	b.WriteString(okStyle.Render("  recent reversible seams:") + "\n")
	for _, s := range e.items {
		label := s.Label
		if label == "" {
			label = "(no label)"
		}
		b.WriteString(fmt.Sprintf("    %s  %s  %s\n",
			toolMeta.Render(s.ID),
			toolMeta.Render("("+s.CommitShort+")"),
			label,
		))
	}
	b.WriteString(toolMeta.Render("  usage: /restore-turn <id> yes") + "\n")
	return b.String()
}

// seamRestoredEntry (B2-RB1) renders the post-restore confirmation with the
// undo hint so the user can revert again to undo this revert.
type seamRestoredEntry struct {
	undoID  string
	summary string
}

func (e seamRestoredEntry) render(_ int, _ spinner.Model) string {
	var b strings.Builder
	b.WriteString(okStyle.Render("  ✓ restored") + " " + e.summary + "\n")
	if e.undoID != "" {
		b.WriteString(fmt.Sprintf("    %s  %s\n",
			toolMeta.Render("undo:"),
			okStyle.Render("/restore-turn "+e.undoID+" yes"),
		))
	}
	return b.String()
}

// permissionsEntry renders a /permissions list reply: a header followed by one
// line per rule (id, action, TTL, scope JSON). Empty list renders as "(none)"
// so the user sees an explicit confirmation rather than a blank block.
type permissionsEntry struct{ items []proto.PermissionInfo }

func (e permissionsEntry) render(_ int, _ spinner.Model) string {
	if len(e.items) == 0 {
		return doneSummaryStyle.Render("Permissions\n  (none)") + "\n"
	}
	var b strings.Builder
	b.WriteString("Permissions\n")
	for _, item := range e.items {
		fmt.Fprintf(&b, "  %s  %s  %s  %s\n", item.ID, item.Action, item.TTL, item.Scope)
	}
	return doneSummaryStyle.Render(strings.TrimSuffix(b.String(), "\n")) + "\n"
}

// toolEntry is a tool-call block: a header line
// (Name(summary) <spinner|✓|✗>) and, once resolved, an indented ⎿ result line
// that is truncated to one line unless expanded (toggled with ctrl+o). The
// friendly display name and compact arg summary are derived in render via
// toolDisplayName / toolArgSummary; root is the work root used to shorten path
// args.
//
// Rendering is dispatched by toolDisplayFor(e.name) — see render. The display
// class decides what (if anything) trails the header:
//   - toolDispSilent (fs_read/list/glob/search/mkdir): header only; the ⎿
//     result line is suppressed. The size hint conveys what was read (file
//     dumps would otherwise dominate the transcript).
//   - toolDispTail (shell_run): header + a tail window of the live output so
//     long-running commands can be watched without flooding the screen.
//   - toolDispAgent (agent_start/workflow_start/analysis/skill_use): header +
//     the nested agent's buffered reasoning (nestedThought); the agent's ReAct
//     loop streams "thinking" deltas back through applyEvent, which routes them
//     into nestedThought instead of opening a sibling thinkingEntry.
//   - toolDispDiff (fs_edit/fs_write): header + a line-level unified diff of
//     the change. Collapsed shows "+N -M 行" as a footprint; expanded renders
//     the full colored diff so the user can see exactly what the agent changed
//     without leaving the transcript.
//   - toolDispNormal (default): header + one-line ⎿ result (the historic
//     behavior, preserved verbatim).
//
// Errors short-circuit all of the above: renderError replaces the arg summary
// with "(Error|short msg)" and skips the ⎿ line.
type taskUpdateEntry struct {
	task work.WorkTask
}

func (e taskUpdateEntry) render(_ int, _ spinner.Model) string {
	return fmt.Sprintf("  task %s  %s  %d%%\n    %s\n", e.task.ID, e.task.Status, e.task.Checklist.CompletionPct(), e.task.Title)
}

type planUpdateEntry struct {
	taskID    string
	checklist work.Checklist
}

func (e planUpdateEntry) render(_ int, _ spinner.Model) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("  plan %s\n", e.taskID))
	for _, item := range e.checklist.Items {
		status := "[ ]"
		if item.Status == "done" {
			status = "[x]"
		} else if item.Status == "in_progress" {
			status = "[-]"
		}
		b.WriteString(fmt.Sprintf("    %s %s\n", status, item.Content))
	}
	return b.String()
}

type checklistUpdateEntry struct {
	taskID    string
	checklist work.Checklist
}

func (e checklistUpdateEntry) render(_ int, _ spinner.Model) string {
	return planUpdateEntry(e).render(0, spinner.Model{})
}

var _ entry = taskUpdateEntry{}
var _ entry = planUpdateEntry{}
var _ entry = checklistUpdateEntry{}

type toolEntry struct {
	name, args  string
	root        string // work root, for shortening path args in the summary
	status      string // "running" | "ok" | "error"
	result      string
	progress    []string // live output lines (shell_run streaming), shown while running
	statusPanel string   // right-side Status from tool_chunk frames (e.g. "1 tools 10k 1m9s")
	// progressOverwrite marks the panel as overwrite-mode (workflow progress:
	// each chunk replaces the whole panel, running agents sorted to top). When
	// true, renderAgent shows a head window (running agents) instead of a tail
	// window (recent activity) so the live agents stay visible.
	progressOverwrite bool
	expanded          bool

	// nested flags an agent-class tool (toolDispAgent). While a nested tool is
	// running, applyEvent routes "thinking" deltas into nestedThought instead
	// of opening a standalone thinkingEntry above the call — the reasoning
	// reads as the agent's internal monologue, not the parent's.
	nested bool
	// nestedThought is the accumulated reasoning streamed by a nested agent's
	// ReAct loop. Rendered indented inside the tool block (not as a ⎿ result
	// line) so the parent → child relationship stays visually intact.
	nestedThought string
	// nestedText is the sub-agent's streamed model output (agent_chunk deltas),
	// accumulated as a continuous string so natural newlines (not per-delta)
	// split lines. Appending each chunk as its own progress line produced
	// "two-Chinese-chars per line" breakage because chunks are short text
	// deltas, not line units. Rendered alongside nestedProgress in renderAgent.
	nestedText string
	// nestedProgress is the sub-agent's observable activity log: each tool
	// call ("Agent(Read) ◌") and tool result ("  ⎿ <first line>") the child
	// emits while the nested tool is running.
	// applyEvent routes these here (rather than creating top-level entries)
	// when lastRunningNestedTool() returns this entry — the sub-agent's ReAct
	// loop reads as inline indented activity, not as independent transcript
	// blocks that would visually invert the parent→child relationship.
	// Rendered with a 10-lines-running tail (renderAgent); once the block
	// resolves the tail is replaced by the one-line agentSummary (tool uses ·
	// tokens · duration), so a chatty sub-agent doesn't flood the screen.
	// The full log remains available via ctrl+o expand.
	nestedProgress []string
	// nestedToolUses counts the sub-agent's own tool calls (each child
	// tool_call routed into nestedProgress increments this). Surfaced in the
	// done summary as "N tool uses" to convey how much work the sub-agent did.
	nestedToolUses int
	// nestedTokens is the sub-agent's total token spend, delivered by a
	// nested_usage frame after the sub-agent's stream drains (runSubAgentTurn
	// emits it just before returning). Surfaced in the done summary as
	// "Nk tokens". Stays 0 when the provider omits usage (FakeModel) — the
	// summary then omits the segment. For a workflow_start (which runs many
	// sub-agents), applyEvent ACCUMULATES each task's nested_usage into this
	// field (+=, not =) so the summary reflects the workflow's total spend
	// across all tasks rather than just the last task's.
	nestedTokens int
	// nestedAgentsDone / nestedAgentsTotal carry the workflow_start's sub-agent
	// completion progress, delivered by nested_progress frames the workflow tool
	// emits after each task's runSubAgent returns (one frame per completed task,
	// via the same SubAgentEmit channel nested_usage uses). Surfaced in the
	// workflow_start done summary as "N/M agents" — the workflow's shape (how
	// many sub-agents it fanned out to) is more informative than the children's
	// aggregate tool-call count. Only the workflow_start branch of agentSummary
	// reads these; agent_start / analysis ignore them and keep "N tool uses".
	// Both stay 0 when no SubAgentEmit is bound (headless CLI) — agentSummary
	// then omits the agents segment and falls back to a bare "Done".
	nestedAgentsDone  int
	nestedAgentsTotal int
	// startedAt/endedAt bracket the Analysis block's wall-clock lifetime:
	// startedAt is stamped when applyEvent creates the nested entry (the
	// parent's tool_call(running)), endedAt when the parent's tool_result
	// resolves it. Their difference is the done summary's "Mm Ss" duration.
	// Both stay zero on non-agent tool entries (unused — renderAgent is the
	// sole consumer).
	startedAt time.Time
	endedAt   time.Time
}

// render dispatches the block's rendering by display class. The error path is
// common to all NON-agent classes (errors surface inline as the header summary,
// never as a ⎿ line). Agent-class tools are the exception: even on error they
// route to renderAgent, which shows the done summary (tool uses · tokens ·
// duration) under the header — the sub-agent still ran, and the summary conveys
// how much work it did. The ✗ glyph in the header (toolGlyph) carries the
// error status; the full error text rides in e.result and shows via expand.
func (e *toolEntry) render(_ int, sp spinner.Model) string {
	if e.status == "error" && toolDisplayFor(e.name) != toolDispAgent {
		return e.renderError(sp)
	}
	switch toolDisplayFor(e.name) {
	case toolDispSilent:
		return e.renderSilent(sp)
	case toolDispTail:
		return e.renderTail(sp)
	case toolDispAgent:
		return e.renderAgent(sp)
	case toolDispDiff:
		return e.renderDiff(sp)
	default:
		return e.renderNormal(sp)
	}
}

// renderToolHeader renders the header line shared by every display class:
// "  Name(summary) <glyph>". The summary is omitted when the args have no
// useful key (toolArgSummary returns ""). The glyph and name carry the status
// color via toolGlyph (running → blue, ok → green, error → red — though the
// error path uses renderError, not this helper). Read/List and other silent
// tools render header-only — no result content or size hint is appended.
func (e *toolEntry) renderToolHeader(sp spinner.Model) string {
	glyph, glyphStyle := toolGlyph(e.status, sp)
	name := glyphStyle.Render(toolDisplayName(e.name))
	summary := ""
	if s := toolArgSummary(e.name, e.args, e.root); s != "" {
		summary = toolMeta.Render(s)
	}
	right := ""
	if e.statusPanel != "" {
		right = " " + toolMeta.Render(e.statusPanel)
	}
	return "  " + name + summary + right + " " + glyph
}

// renderError renders a failed tool inline: Name(Error|short msg) ✗. The short
// (first line, truncated) error replaces the arg summary in the header, and the
// indented ⎿ result line is skipped — the failure is already surfaced inline.
// e.result holds the extracted error message (set by the tool_result frame's
// status="error" path). Name + glyph are red. Common to every display class.
func (e *toolEntry) renderError(sp spinner.Model) string {
	glyph, _ := toolGlyph(e.status, sp)
	name := errStyle.Render(toolDisplayName(e.name))
	short := truncate(firstLine(e.result), 50)
	return "  " + name + errStyle.Render("(Error|"+short+")") + " " + glyph + "\n\n"
}

// renderSilent renders a "quiet" tool (fs_read/fs_list/fs_glob/fs_search):
// header only, NO ⎿ result line. These tools return large file dumps
// whose size is already conveyed by the header's size hint ("12.3 KB · 245
// lines"); a result line would just push prior context off screen. The full
// content is still available via ctrl+o expand.
// renderSilent renders a read/classify tool (fs_read/fs_list/fs_glob/
// fs_search) as header-only — "  Read(foo.go) ✓". The result content (file
// body, directory listing, search hits) is never shown inline: these tools'
// output is for the model, not the transcript. ctrl+o does not expand them
// (toggleExpand skips silent tools) — there is nothing to reveal.
func (e *toolEntry) renderSilent(sp spinner.Model) string {
	return e.renderToolHeader(sp) + "\n"
}

// renderTail renders a streaming-output tool (shell_run): the header plus a tail
// window of the live output. While running, the last ~10 progress lines are
// shown so the user can watch a build/test run progress; once finished, only
// the command output is shown (NOT the raw JSON wrapper with exit code /
// duration_ms — those are metadata for the LLM, not the human). When the
// output is empty the result line is suppressed entirely: a successful silent
// command (e.g. "go build") reads as just "Bash(…) ✓" with no clutter. The
// full output is available via ctrl+o expand.
func (e *toolEntry) renderTail(sp spinner.Model) string {
	out := e.renderToolHeader(sp) + "\n"
	if e.status == "running" {
		if body := renderToolOutput(tailLines(strings.Join(e.progress, "\n"), 10)); body != "" {
			out += body + "\n\n"
		}
		return out
	}
	// Finished (ok or error): parse the JSON result and show only the output.
	// The JSON structure is {"output":"...","exit":N,"duration_ms":N}.
	body := toolResultOutput(e.result)
	if e.expanded && body != "" {
		out += renderToolOutput(body) + "\n\n"
	} else if body != "" {
		if t := tailLines(body, 3); t != "" {
			out += renderToolOutput(t) + "\n\n"
		}
	}
	return out
}

// renderAgent renders an agent-class tool (agent_start/workflow_start/analysis/
// skill_use): the header plus either a scrolling tail of the nested agent's
// activity (while running) or a one-line done summary (once resolved). The
// nested agent's own ReAct loop streams events back through applyEvent; rather
// than emitting them as standalone blocks above the tool call (which would
// visually invert the parent → child relationship), we attribute them to the
// tool block and indent them so they read as the agent's internal activity.
//
// The tail-vs-summary split replaces the old "10 running / 3 done" tail rule:
// while running, the last 10 nestedProgress lines scroll (a window on a chatty
// sub-agent); once done, a compact summary replaces the 3-line tail — the
// summary conveys the sub-agent's effort far more compactly than three output
// lines, and is what the user actually wants once the work is finished. The
// summary's first segment is tool-specific: "N tool uses" for agent_start /
// analysis (the child's tool-call count), "N/M agents" for workflow_start (the
// workflow's sub-agent completion progress). See agentSummary for the branch.
// The full log is available via ctrl+o expand (full body + summary).
//
// The collapsed view is a TOOL-CALL LOG: it shows only the sub-agent's
// observable activity (nestedProgress per-tool lines + the workflow progress
// panel). The sub-agent's streamed text answer (nestedText) and reasoning
// (nestedThought, a last-resort fallback for a pure-reasoning child) are
// suppressed by default — a chatty analysis agent would otherwise dump its
// whole markdown answer inline and flood the transcript. They remain reachable
// via ctrl+o expand, which renders the full body alongside the per-tool lines
// and the summary.
func (e *toolEntry) renderAgent(sp spinner.Model) string {
	out := e.renderToolHeader(sp) + "\n"
	// On error, surface the failure reason (held in e.result) as its own line
	// under the header. Previously (pre-summary) agent-class errors went through
	// renderError, which inlined the message in the header summary; routing
	// them through renderAgent for the done-summary would otherwise hide it.
	// The ✗ glyph in the header carries the status; this line carries the why.
	// First line only, truncated — the full text is available via ctrl+o expand.
	if e.status == "error" && strings.TrimSpace(e.result) != "" {
		out += "  " + errStyle.Render("Error: "+truncate(firstLine(e.result), 100)) + "\n"
	}
	// colorProgressLines tints workflow panel lines (tool_chunk Text): ✓ →
	// green, ✗ → red, other non-empty lines get a spinner prefix. A no-op when
	// no panel is present (len(e.progress) == 0). Extracted as a closure so both
	// the collapsed activity window and the expanded full body share one pass.
	colorProgressLines := func(s string) string {
		if len(e.progress) == 0 {
			return s
		}
		spin := sp.View()
		lines := strings.Split(s, "\n")
		for i, l := range lines {
			t := strings.TrimSpace(l)
			switch {
			case strings.HasPrefix(t, "✓ "):
				lines[i] = okStyle.Render(l)
			case strings.HasPrefix(t, "✗ "):
				lines[i] = errStyle.Render(l)
			case t != "":
				lines[i] = spin + " " + l
			}
		}
		return strings.Join(lines, "\n")
	}

	// activity = the sub-agent's observable TOOL activity only. In single-agent
	// mode that is the per-tool log (nestedProgress: "Agent(Read) ◌" …) plus any
	// progress. In WORKFLOW mode (progressOverwrite) the per-agent panel
	// (progress, e.g. "Agent-A1(List) 1 tools …") already conveys each
	// sub-agent's activity WITH attribution, so the unattributed raw tool-call
	// lines are suppressed here — otherwise every child sub-agent's calls flood
	// the panel as anonymous "Agent(List) ◌" noise.
	activity := ""
	if !e.progressOverwrite {
		activity = strings.Join(e.nestedProgress, "\n")
	}
	if len(e.progress) > 0 {
		if strings.TrimSpace(activity) != "" {
			activity += "\n"
		}
		activity += strings.Join(e.progress, "")
	}

	// body extends activity with the sub-agent's streamed text (nestedText) and,
	// as a last-resort fallback for a pure-reasoning child, its reasoning
	// (nestedThought). body is rendered ONLY in expanded mode — the prose and
	// reasoning stay out of the default transcript but remain reachable via
	// ctrl+o expand.
	body := activity
	if strings.TrimSpace(e.nestedText) != "" {
		if strings.TrimSpace(body) != "" {
			body += "\n"
		}
		body += e.nestedText
	}
	if strings.TrimSpace(body) == "" {
		body = strings.TrimSpace(e.nestedThought)
	}
	body = colorProgressLines(body)

	if e.status == "running" {
		// Expanded: the full body (activity + prose + reasoning), no window cap
		// — the sub-agent hasn't finished, so no done summary yet (tool-use /
		// token / duration totals would be premature).
		if e.expanded {
			if body == "" {
				return out
			}
			return out + renderToolOutput(body) + "\n\n"
		}
		// Collapsed: a 10-line window of TOOL ACTIVITY only — never the
		// sub-agent's prose. Overwrite-mode panels (workflow) keep running
		// agents at the TOP, so a head window shows them; append-mode logs
		// (single agent) keep recent activity at the BOTTOM, so a tail window
		// shows it. Empty activity (a pure-reasoning child) renders just its
		// header; the spinner + activity line convey "running".
		if strings.TrimSpace(activity) == "" {
			return out
		}
		colored := colorProgressLines(activity)
		var t string
		if e.progressOverwrite {
			t = headLines(colored, 10)
		} else {
			t = tailLines(colored, 10)
		}
		if t != "" {
			return out + renderToolOutput(t) + "\n\n"
		}
		return out
	}

	// Done (ok or error): in single-agent mode show the per-tool lines
	// (nestedProgress: "Agent(Read) …") THEN the aggregate summary line — the
	// summary conveys the totals, the per-tool lines convey what ran. In workflow
	// mode (progressOverwrite) the per-tool lines are suppressed and only the
	// summary shows; the panel already conveyed the per-agent shape while
	// running. Expanded mode additionally shows the full body (reasoning +
	// output chunks) for either mode.
	summary := "  " + toolMeta.Render(e.agentSummary()) + "\n"
	// Per-tool lines (nestedProgress: "Agent(ToolName) …") are shown collapsed
	// only in single-agent mode; a workflow (progressOverwrite) summarizes via
	// the done summary instead — the panel carried the per-agent shape while
	// running.
	var nestedLines string
	if !e.progressOverwrite {
		if np := strings.Join(e.nestedProgress, "\n"); strings.TrimSpace(np) != "" {
			nestedLines = renderToolOutput(np) + "\n"
		}
	}
	if e.expanded && body != "" {
		return out + nestedLines + renderToolOutput(body) + "\n" + summary + "\n"
	}
	if nestedLines != "" {
		return out + nestedLines + summary + "\n"
	}
	return out + summary + "\n"
}

// agentSummary renders the one-line done summary shown when a nested agent tool
// resolves (ok or error). The first segment depends on the tool:
//   - workflow_start: "N/M agents" — the workflow fanned out to M sub-agents and
//     completed N of them. This conveys the workflow's shape far better than the
//     children's aggregate tool-call count (which a workflow_summary would bury
//     under fan-out). Sourced from nested_progress frames.
//   - agent_start / analysis / skill_use: "N tool uses" — the sub-agent's own
//     tool-call count, sourced from nestedToolUses (22f00d8).
//
// The tokens ("Nk tokens") and duration ("Mm Ss") segments are shared. Each
// segment is omitted when its data is zero/missing so a partial summary still
// reads naturally: tokens stay 0 when the provider omits usage (FakeModel), and
// the agents segment stays absent when no nested_progress arrived (headless CLI
// with no SubAgentEmit bound). Returns just the inner text (no styling); the
// caller wraps it in toolMeta. "Done" alone (no segments) covers the degenerate
// all-zero case.
func (e *toolEntry) agentSummary() string {
	var segs []string
	if e.name == "workflow_start" {
		// Workflow: report sub-agent completion progress instead of tool uses.
		// total>0 gates the segment — without a nested_progress frame (no
		// transport bound) the numerator is meaningless, so fall back to
		// tokens/duration only.
		if e.nestedAgentsTotal > 0 {
			segs = append(segs, fmt.Sprintf("%d/%d agents", e.nestedAgentsDone, e.nestedAgentsTotal))
		}
	} else if e.nestedToolUses > 0 {
		segs = append(segs, pluralCount(e.nestedToolUses, "tool use"))
	}
	if e.nestedTokens > 0 {
		segs = append(segs, formatTokens(e.nestedTokens)+" tokens")
	}
	if !e.startedAt.IsZero() && !e.endedAt.IsZero() {
		segs = append(segs, formatDurationWords(e.endedAt.Sub(e.startedAt)))
	}
	if len(segs) == 0 {
		return "Done"
	}
	return "Done (" + strings.Join(segs, " · ") + ")"
}

// renderNormal is the default display: header plus a one-line ⎿ result (or the
// full result when expanded). This is the historic toolEntry.render behavior,
// preserved verbatim for any tool that doesn't fall into silent/tail/agent.
func (e *toolEntry) renderNormal(sp spinner.Model) string {
	out := e.renderToolHeader(sp) + "\n"
	if e.status != "running" {
		if e.expanded && e.result != "" {
			out += resultStyle.Render("⎿ "+e.result) + "\n\n"
		} else {
			oneLine := strings.ReplaceAll(strings.TrimSpace(e.result), "\n", " ")
			hint := ""
			if e.result != "" {
				hint = "  " + warnStyle.Render("(ctrl+o expand)")
			}
			out += resultStyle.Render("⎿ "+truncate(oneLine, 100)) + hint + "\n\n"
		}
	}
	return out
}

// renderDiff renders an edit-class tool (fs_edit/fs_write) as a line-level
// unified diff under the header, so the user can see exactly what the agent
// changed without leaving the transcript. The collapse/expand trade-off is the
// same as for renderSilent: large edits would otherwise dominate the screen.
//
//   - fs_edit: parse old_string/new_string from args, compute the LCS diff,
//     and either render the full colored diff (expanded) or a compact
//     "+N -M 行 (ctrl+o expand)" hint (collapsed). Falls back to renderNormal
//     when args are missing the expected fields so a malformed frame still
//     surfaces something useful instead of an empty block.
//   - fs_write: brand-new files have no "old" to diff against in the tool args
//     (the previous content, if any, lives in VCS — not reachable from
//     render). We render a "wrote N lines" footprint; expanded shows a
//     first-line preview of the content. This keeps the block informative
//     without fabricating a diff against missing data.
//
// Errors short-circuit to renderError via render() before we get here.
func (e *toolEntry) renderDiff(sp spinner.Model) string {
	out := e.renderToolHeader(sp) + "\n"
	if e.status == "running" {
		return out + "\n"
	}
	switch e.name {
	case "fs_edit":
		oldS, newS, ok := parseEditStrings(e.args)
		if !ok {
			// Missing/invalid old_string|new_string — fall back so the block
			// still surfaces the result rather than rendering empty.
			return e.renderNormal(sp)
		}
		diff := unifiedDiff(oldS, newS)
		if diff == "" {
			return out + "\n"
		}
		if e.expanded {
			out += renderColoredDiff(diff) + "\n\n"
		} else {
			add, del := countDiffAddDel(diff)
			hint := fmt.Sprintf("+%d -%d 行", add, del) + "  " + warnStyle.Render("(ctrl+o expand)")
			out += "  " + toolMeta.Render(hint) + "\n\n"
		}
		return out
	case "fs_write":
		content, _ := parseWriteContent(e.args)
		lines := lineCount(content)
		footprint := "wrote " + pluralCount(lines, "line")
		if e.expanded && content != "" {
			preview := truncate(firstLine(content), 80)
			footprint += "  " + warnStyle.Render("(ctrl+o expand)")
			out += "  " + toolMeta.Render(footprint) + "\n"
			out += "  " + diffCtxStyle.Render(preview) + "\n\n"
		} else {
			out += "  " + toolMeta.Render(footprint) + "\n\n"
		}
		return out
	}
	return out
}

// renderColoredDiff renders a unified diff (as produced by unifiedDiff) with
// each line colored by its sigil and indented by 4 spaces so the diff visually
// nests under the tool-call header like the ⎿ result line of renderNormal.
// The bare sigil byte is preserved inside the colored output so a reader can
// still distinguish + / - / " at a glance.
func renderColoredDiff(diff string) string {
	const pad = "    "
	var b strings.Builder
	for i, ln := range strings.Split(diff, "\n") {
		if i > 0 {
			b.WriteByte('\n')
		}
		if ln == "" {
			b.WriteString(pad)
			continue
		}
		switch ln[0] {
		case '-':
			b.WriteString(pad + diffDelStyle.Render(ln))
		case '+':
			b.WriteString(pad + diffAddStyle.Render(ln))
		default:
			b.WriteString(pad + diffCtxStyle.Render(ln))
		}
	}
	return b.String()
}

// lineCount counts the lines in s using the convention that a trailing "\n"
// does not introduce an extra empty line. "" → 0; "a" → 1; "a\n" → 1; "a\nb"
// → 2. Used by renderDiff to size the fs_write footprint.
func lineCount(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

// parseEditStrings extracts the old_string/new_string pair from an fs_edit
// tool-call's args JSON. Returns ok=false when the args are not valid JSON or
// either field is absent — renderDiff falls back to renderNormal in that case
// so a malformed frame still surfaces something useful.
func parseEditStrings(argsJSON string) (oldS, newS string, ok bool) {
	argsJSON = strings.TrimSpace(argsJSON)
	if argsJSON == "" {
		return "", "", false
	}
	var v struct {
		OldString string `json:"old_string"`
		NewString string `json:"new_string"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &v); err != nil {
		return "", "", false
	}
	if v.OldString == "" && v.NewString == "" {
		return "", "", false
	}
	return v.OldString, v.NewString, true
}

// parseWriteContent extracts the content field from an fs_write tool-call's
// args JSON. Returns ok=false when args are not valid JSON or content is
// absent; renderDiff then renders just the "wrote 0 lines" footprint.
func parseWriteContent(argsJSON string) (content string, ok bool) {
	argsJSON = strings.TrimSpace(argsJSON)
	if argsJSON == "" {
		return "", false
	}
	var v struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &v); err != nil {
		return "", false
	}
	return v.Content, v.Content != ""
}

// toolGlyph returns the status glyph and the color style shared by the tool
// name and glyph, keyed by status: running → blue (spinner + blue name), ok →
// green (✓ + green name), error → red (✗ + red name). Coloring the name by
// status makes a tool block's outcome readable at a glance alongside the glyph.
func toolGlyph(status string, sp spinner.Model) (glyph string, style lipgloss.Style) {
	switch status {
	case "ok":
		return okStyle.Render("✓"), okStyle
	case "error":
		return errStyle.Render("✗"), errStyle
	default: // running
		return sp.View(), runningNameStyle
	}
}

// firstLine returns the first line of s (trimmed), or all of s if it has no
// newline. Used to keep an error message compact in the tool-call header.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// runningNameStyle colors a RUNNING tool block's name blue (ok/error reuse
// okStyle/errStyle — green/red — via toolGlyph). The palette otherwise lives in
// styles.go; this one is local because it is only consumed by toolGlyph.

// toolDisplay classifies how a tool's result is rendered.
type toolDisplay int

const (
	toolDispNormal toolDisplay = iota
	toolDispSilent
	toolDispTail
	toolDispAgent
	toolDispDiff
)

func toolDisplayFor(name string) toolDisplay {
	switch name {
	case "fs_read", "fs_list", "fs_glob", "fs_search":
		return toolDispSilent
	case "shell_run":
		return toolDispTail
	case "agent_start", "workflow_start", "analysis":
		return toolDispAgent
	case "fs_edit", "fs_write":
		return toolDispDiff
	}
	return toolDispNormal
}

func tailLines(s string, n int) string {
	s = strings.TrimRight(s, "\n")
	if s == "" || n <= 0 {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}

// headLines returns the first n lines of s (after trimming trailing newlines).
// Used for overwrite-mode panels (workflow progress) whose most relevant rows —
// the still-running agents — are sorted to the TOP, so a tail window would
// hide them behind completed agents.
func headLines(s string, n int) string {
	s = strings.TrimRight(s, "\n")
	if s == "" || n <= 0 {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[:n], "\n")
}

func renderToolOutput(s string) string {
	return resultStyle.Render(strings.TrimRight(s, "\n"))
}

// toolResultOutput extracts the human-readable output from a shell_run JSON
// result. The JSON has the shape {"output":"...","exit":N,"duration_ms":N}.
// Returns just the output string (the exit code and duration are LLM metadata,
// not shown to the TUI user). When the JSON is not parseable it falls back to
// the raw result string so nothing is silently lost.
func toolResultOutput(result string) string {
	if result == "" {
		return ""
	}
	var v struct {
		Output string `json:"output"`
	}
	if json.Unmarshal([]byte(result), &v) == nil {
		return v.Output
	}
	return result
}

var runningNameStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("39")).Bold(true) // blue

// isAsciiDigits reports whether s is non-empty and all ASCII digits (a cheap
// check that the token before a tab is an fs_read line number, not content).
func isAsciiDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// formatBytes renders a byte count compactly: < 1 KiB as "N B", otherwise
// "N.N KB"/"N.N MB" (1024-based, matching the "12.3 KB" example). Used for the
// fs_read/fs_list size hints.
func formatBytes(n int) string {
	const kiB = 1024
	if n < kiB {
		return fmt.Sprintf("%d B", n)
	}
	if n < kiB*1024 {
		return fmt.Sprintf("%.1f KB", float64(n)/kiB)
	}
	return fmt.Sprintf("%.1f MB", float64(n)/float64(kiB*1024))
}

// pluralCount renders "<n> <singular>" or "<n> <plural>" (lines/entries).
func pluralCount(n int, singular string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", singular)
	}
	return fmt.Sprintf("%d %ss", n, singular)
}

// skillsEntry renders the list of installed skills (reply to /skills). CB2: the
// entry interface requires render(width int, sp spinner.Model) string; both
// parameters are intentionally unused here but the signature must still match.
type skillsEntry struct {
	skills []proto.SkillInfo
}

func (e skillsEntry) render(_ int, _ spinner.Model) string {
	if len(e.skills) == 0 {
		return "  no skills installed\n\n"
	}
	var b strings.Builder
	b.WriteString("  skills:\n")
	for _, sk := range e.skills {
		enabled := "enabled"
		if !sk.Enabled {
			enabled = "disabled"
		}
		trusted := "trusted"
		if !sk.Trusted {
			trusted = "untrusted"
		}
		source := sk.Source
		if source == "" {
			source = "unknown"
		}
		fmt.Fprintf(&b, "  - %s (%s) [%s, %s]\n", sk.Name, source, enabled, trusted)
		// E03: name collisions. Load resolves them first-seen-wins and used to
		// drop the loser silently, so a project skill hidden behind a
		// user-level one of the same name was invisible — the name resolved to
		// something the user did not write and nothing said so. The DIRECTORY
		// is printed, not just the source label: "which file is being ignored"
		// is the question, and a label does not answer it when several roots
		// share one.
		for _, sh := range sk.Shadowed {
			src := sh.Source
			if src == "" {
				src = "unknown"
			}
			fmt.Fprintf(&b, "      shadowed: %s copy at %s is ignored\n", src, sh.Dir)
		}
	}
	b.WriteString("\n")
	return b.String()
}

// checklistEntry renders a plan checklist as the model updates it.
//
// Both plan_update and checklist_update land here: the server sends the whole
// list either way, and the only difference the operator cares about is whether
// the plan was rewritten or extended, which the header states.
type checklistEntry struct {
	taskID   string
	list     work.Checklist
	replaced bool // plan_update (replace) vs checklist_update (patch)
}

// checklistGlyphs maps each status to a marker that survives having styling
// stripped — a non-TTY, a screenshot and a pasted transcript all lose colour,
// and "which step is running" is the one thing this block exists to show.
var checklistGlyphs = map[work.ChecklistItemStatus]string{
	work.ChecklistPending:    "[ ]",
	work.ChecklistInProgress: "[~]",
	work.ChecklistDone:       "[x]",
}

func (e checklistEntry) render(_ int, _ spinner.Model) string {
	title := "plan updated"
	if !e.replaced {
		title = "checklist updated"
	}
	var b strings.Builder
	b.WriteString("  " + toolName.Render(title))
	if n := len(e.list.Items); n > 0 {
		b.WriteString(" " + toolMeta.Render(fmt.Sprintf("(%d%%, %d steps)",
			e.list.CompletionPct(), n)))
	}
	b.WriteString("\n")
	if len(e.list.Items) == 0 {
		b.WriteString("    " + warnStyle.Render("(empty)") + "\n\n")
		return b.String()
	}
	for _, it := range e.list.Items {
		glyph, ok := checklistGlyphs[it.Status]
		if !ok {
			// An unknown status must not render as done. Showing the raw value
			// is how the operator finds out the vocabulary drifted.
			glyph = "[" + string(it.Status) + "]"
		}
		line := fmt.Sprintf("    %s %s", glyph, it.Content)
		if it.Status == work.ChecklistInProgress {
			line = toolName.Render(line)
		}
		b.WriteString(line + "\n")
	}
	b.WriteString("\n")
	return b.String()
}
