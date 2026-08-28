// internal/agent/upkeep/memory.go
//
// W-D-03: long-term memory is extracted from finished sessions by this worker,
// not only by a model that remembered to call memory_write.
//
// The gap this closes is concrete. A self-driving goal loop can run for hours,
// solve the problem and exit having produced no durable asset at all: memories
// existed only when the model chose to write one, and nothing in the loop
// prompts it to. W-A-05 wired up the consolidation entry point
// (tools.DistillMemories) but its only trigger was still the model itself.
//
// Two phases, and the split is not cosmetic:
//
//	Phase 1 EXTRACTS. It reads the session's projected window and asks the model
//	for durable facts, writing each as its own memory row scoped to the session.
//
//	Phase 2 CONSOLIDATES, by calling tools.DistillMemories — the SAME entry
//	point /distill and the post-turn hook use. A second consolidation path here
//	would be a second thing to keep correct, and the one that already exists is
//	the one with the "never lose a memory" transaction behind it.
//
// tools.MinDistillBatch is 6, so phase 2 is a no-op until a session has yielded
// that many notes. That is not a bug to work around: below six there is nothing
// to consolidate that would not be better left alone.
package upkeep

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/x6nux/yanshi/internal/store"
	"github.com/x6nux/yanshi/internal/tools"
)

// MemoryIdle is how long a session must be quiet before the worker treats it as
// finished.
//
// It is a constant rather than a config knob because there is no operator
// decision here to make: shorter risks extracting from a conversation the user
// stepped away from mid-thought, longer only delays a background job nobody is
// waiting on. It is also deliberately far shorter than storage.retention_days —
// a session's memories are worth having the same afternoon, its rows are not
// worth compressing for a month.
const MemoryIdle = 30 * time.Minute

// memoryLeaseTTL bounds how long one extraction may hold a session.
//
// Long enough for a slow provider (extractTimeout plus the distillation pass),
// short enough that a process killed mid-extraction frees the session the same
// hour rather than at the next restart.
const memoryLeaseTTL = 10 * time.Minute

// extractTimeout bounds the phase-1 model call. An unbounded Generate on a
// background loop leaks a goroutine and an in-flight request per tick, which is
// the W-A-06 failure shape; the sweep has nothing to gain from waiting longer.
const extractTimeout = 2 * time.Minute

// maxExtractedNotes caps how many memories one session may contribute.
// A model asked to summarise a long transcript will happily produce fifty
// lines, and fifty near-duplicates make retrieval worse rather than better.
const maxExtractedNotes = 12

// transcriptCharBudget bounds the rendered window handed to the extraction
// model, newest text last. A cold session can hold megabytes of tool output;
// without a bound this call would exceed the context window of the cheap model
// it is meant to run on.
const transcriptCharBudget = 24000

// memoryKind tags rows this worker produced, so an operator can tell a
// worker-extracted note from one the model chose to write.
const memoryKind = "session"

// ExtractPrompt asks for durable facts, one per line.
//
// Line-oriented rather than JSON for the reason DistillPrompt gives: a small
// model producing JSON fails in a way that loses the whole batch on one
// unescaped quote, while a line format fails per line — and a per-line failure
// is one dropped note, which this file is built to survive.
//
// The rules push against the failure mode that makes automatic memory useless,
// which is not "too few notes" but "notes about the conversation instead of
// about the world". "The user asked me to fix the build" is true, worthless,
// and exactly what an unprompted summariser produces.
//
// Exported because it is a PROMPT: moving behaviour into a string moves it out
// of the compiler's view, so deleting half of these rules changes what the
// worker records and compiles fine. Naming it lets a test assert the rules are
// still there, which is the same reason guard.AutoApprovalPrompt is exported.
const ExtractPrompt = `Below is a finished work session. Extract the DURABLE facts worth remembering for future, unrelated sessions.

Record only things that will still be true tomorrow:
- decisions and their reasons ("we use X because Y")
- discovered facts about the codebase, the environment, or the tools
- the user's stated preferences and constraints
- corrections: where something turned out not to work, and what did

Do NOT record:
- what happened in this session ("the user asked me to...", "I ran the tests")
- anything already obvious from the code itself
- speculation, or anything you are not certain the session established

Preserve verbatim: file paths, commands, names, versions, numbers.

Answer with one fact per line, in exactly this form:

NOTE the fact, as a standalone statement

Write at most %d lines. If the session established nothing durable, answer exactly:

NOTHING

Session:
`

// extractMemories is the memory half of one sweep.
//
// Order matters: extract first, prune second, so a session's fresh notes are in
// the table before the quota decides what to evict. Pruning first would let a
// tick delete rows only to write new ones over the quota again.
func (w *Worker) extractMemories(ctx context.Context) {
	if w.cfg.Model != nil {
		cutoff := w.now().Add(-MemoryIdle).Unix()
		ids, err := w.db.IdleSessions(cutoff, w.cfg.SweepLimit, false)
		if err != nil {
			slog.Warn("upkeep: listing idle sessions failed", "error", err)
		}
		for _, sid := range ids {
			if ctx.Err() != nil {
				return
			}
			w.extractOne(ctx, sid)
		}
	}
	// The quota is independent of extraction: memory_write still fills this
	// table whether or not a model is configured here, and bounding it is the
	// point of the knob.
	if n, err := w.db.PruneUnusedMemories(w.cfg.MemoryQuota); err != nil {
		slog.Warn("upkeep: pruning memories failed", "error", err)
	} else if n > 0 {
		slog.Info("upkeep: pruned unused memories over quota", "deleted", n)
	}
}

// memoryLease names the lease guarding one session's extraction.
func memoryLease(sessionID string) string { return "memextract:" + sessionID }

// extractOne runs both phases for one session under a lease.
//
// THE LEASE IS WHY TWO PROCESSES DO NOT BOTH EXTRACT. yanshi routinely runs
// several processes against one database — a TUI that bootstrapped its own
// backend, a `serve`, a second window that lost the election — and each of them
// builds this worker. Without the claim they would each pay for the same model
// call and write the same notes twice, which is not merely wasteful: duplicate
// memories are exactly what the consolidation pass exists to remove.
//
// A SUCCESSFUL PASS RETIRES THE LEASE, permanently. The session will never be
// extracted again, even if it later resumes and goes quiet a second time. That
// is the deliberate choice: re-extracting a longer window that CONTAINS the
// text already extracted produces near-duplicates of every note, and a table
// full of those is worse than a table missing the tail of one conversation.
//
// A FAILED PASS DOES NOT RETIRE IT. The claim simply expires, and the next
// sweep after memoryLeaseTTL tries again — which also means a provider outage
// costs a delay rather than a permanently skipped session.
func (w *Worker) extractOne(ctx context.Context, sessionID string) {
	// Cheap pre-check so a store full of retired sessions does not pay a write
	// transaction per session per tick just to lose the claim.
	if until, ok, err := w.db.LeaseHeldUntil(memoryLease(sessionID)); err == nil && ok {
		if until > w.now().Unix() {
			return
		}
	}
	won, err := w.db.ClaimLease(memoryLease(sessionID), memoryLeaseTTL)
	if err != nil {
		slog.Warn("upkeep: claiming memory lease failed", "session", sessionID, "error", err)
		return
	}
	if !won {
		return
	}

	notes, err := w.extractNotes(ctx, sessionID)
	if err != nil {
		slog.Warn("upkeep: memory extraction failed", "session", sessionID, "error", err)
		return
	}
	dims := store.MemoryFilter{SessionID: sessionID}
	for _, n := range notes {
		// W-D-07: WriteMemoryFromSession, not WriteMemoryScoped — this worker is
		// the writer whose output is least traceable by hand. Nobody watched it
		// run, so "which conversation produced this note" is unanswerable without
		// the recorded position.
		if _, err := w.db.WriteMemoryFromSession(memoryKind, n, dims); err != nil {
			slog.Warn("upkeep: writing extracted memory failed", "session", sessionID, "error", err)
			return
		}
	}

	// Phase 2 through the shared entry point. Its own contract is that every
	// failure leaves the originals intact and current, so an error here costs a
	// consolidation, never a memory — which is why it does not abort the retire
	// below. The notes are already durable at this point.
	if _, err := tools.DistillMemories(ctx, w.db, w.cfg.Model, dims); err != nil {
		slog.Warn("upkeep: memory consolidation failed", "session", sessionID, "error", err)
	}

	if err := w.db.RetireLease(memoryLease(sessionID)); err != nil {
		slog.Warn("upkeep: retiring memory lease failed", "session", sessionID, "error", err)
		return
	}
	if len(notes) > 0 {
		slog.Info("upkeep: extracted long-term memories", "session", sessionID, "notes", len(notes))
	}
}

// extractNotes runs phase 1 and returns the durable facts the model found.
//
// It reads through ProjectWindow rather than Messages: the window is what the
// model actually worked from, so the originals a compaction already summarised
// are not re-read here — the summary that replaced them IS in the window and
// carries the same content at a fraction of the tokens.
func (w *Worker) extractNotes(ctx context.Context, sessionID string) ([]string, error) {
	msgs, err := w.db.ProjectWindow(sessionID)
	if err != nil {
		return nil, fmt.Errorf("read window: %w", err)
	}
	transcript := renderTranscript(msgs)
	if strings.TrimSpace(transcript) == "" {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, extractTimeout)
	defer cancel()
	reply, err := w.cfg.Model.Generate(ctx, []*schema.Message{{
		Role:    schema.User,
		Content: fmt.Sprintf(ExtractPrompt, maxExtractedNotes) + transcript,
	}})
	if err != nil {
		return nil, fmt.Errorf("model call: %w", err)
	}
	if reply == nil {
		return nil, fmt.Errorf("model returned no message")
	}
	return ParseExtractedNotes(reply.Content), nil
}

// ParseExtractedNotes pulls the NOTE lines out of a phase-1 answer.
//
// Prose the model wrapped around its answer is skipped rather than treated as a
// note, on the same principle ParseDistillPlan applies: a line that does not
// match the requested form is not describing what it claims to describe, and
// storing it would put "Here are the durable facts:" into long-term memory
// forever.
//
// Exported so the parser is testable without a store or a model — it is the one
// piece here that is pure.
func ParseExtractedNotes(answer string) []string {
	var out []string
	seen := make(map[string]bool)
	for _, line := range strings.Split(answer, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.EqualFold(trimmed, "NOTHING") {
			continue
		}
		if len(trimmed) < 5 || !strings.EqualFold(trimmed[:4], "NOTE") {
			continue
		}
		body := strings.TrimSpace(trimmed[4:])
		if body == "" || seen[body] {
			continue
		}
		seen[body] = true
		out = append(out, body)
		if len(out) == maxExtractedNotes {
			break
		}
	}
	return out
}

// renderTranscript flattens a window into the text the extraction model reads,
// keeping the NEWEST messages when the budget runs out.
//
// Newest-last-and-newest-kept, because a session's conclusions are at its end:
// truncating from the tail would hand the model the exploration and hide the
// answer, which is the one part worth remembering.
func renderTranscript(msgs []store.Message) string {
	lines := make([]string, 0, len(msgs))
	budget := transcriptCharBudget
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		body := strings.TrimSpace(m.Content)
		if m.Role == store.RoleToolCall {
			body = m.ToolName + " " + strings.TrimSpace(m.ToolArgs)
		}
		if body == "" {
			continue
		}
		line := m.Role + ": " + body
		if len(line) > budget {
			if budget < 200 {
				break
			}
			line = line[:budget] + "…"
		}
		budget -= len(line)
		lines = append(lines, line)
	}
	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}
	return strings.Join(lines, "\n")
}
