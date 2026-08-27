// internal/ctxcompact/fold.go
package ctxcompact

import (
	"fmt"
	"strings"

	"github.com/cloudwego/eino/schema"
)

// FoldTier is how aggressively one tool result was rewritten.
type FoldTier int

const (
	// FoldNone leaves the result verbatim.
	FoldNone FoldTier = iota
	// FoldTruncated keeps a head and a tail with the middle elided.
	FoldTruncated
	// FoldDigest replaces the body with a single descriptive line.
	FoldDigest
)

// String names the tier for logs and test failures.
func (t FoldTier) String() string {
	switch t {
	case FoldTruncated:
		return "truncated"
	case FoldDigest:
		return "digest"
	}
	return "none"
}

// Fold policy constants. See FoldToolResults for the argument that these are
// the right shape, and PressureFold for how the tier is chosen.
const (
	// FoldKeepRecent is the number of trailing tool results that are NEVER
	// folded, at any pressure.
	//
	// The model is mid-task on these. A ReAct loop reads a file, decides what
	// to change, and writes it; folding the read it is about to act on turns a
	// working agent into one that re-reads the same file every iteration —
	// which costs more window than folding saved, and loops. QwenPaw arrived
	// at 5 for the same reason (its history_keep_recent_messages, 4-6 by
	// effort level) and 5 is what this uses: enough to cover a read/edit/verify
	// triple plus the call that prompted it.
	FoldKeepRecent = 5

	// FoldPressureThreshold is the fraction of the input budget that tool
	// results must occupy before ANY folding happens.
	//
	// Below this the window is not the constraint and folding only costs
	// fidelity. 0.5 is where tool output stops being part of the conversation
	// and becomes the conversation: past half the budget spent on results, the
	// user turns, the system prompt and the assistant's own reasoning are
	// sharing the other half.
	FoldPressureThreshold = 0.5

	// FoldDigestPressure is the fraction at which folding escalates from
	// truncation to one-line digests. Past this the window is nearly gone and
	// keeping a head and tail of each of a hundred results is still too much.
	FoldDigestPressure = 0.75

	// foldHeadLines and foldTailLines bound the truncated tier. Head-and-tail
	// rather than head-only because the ends of a tool result are where the
	// answer usually is: a shell run's exit status and last error, a test
	// run's summary line, a JSON body's closing fields.
	foldHeadLines = 12
	foldTailLines = 8

	// foldMinRunes is the size below which a result is never folded. Folding a
	// short result costs more in marker text than it saves, and the marker is
	// itself confusing on a two-line output.
	foldMinRunes = 400
)

// FoldStats reports what a fold pass did. It exists so the caller can log the
// effect — "compacted 40 tool results, 180K→22K tokens" — rather than
// reporting that compaction ran.
type FoldStats struct {
	// Considered is the number of tool results examined.
	Considered int
	// Folded counts results actually rewritten, by tier.
	Truncated, Digested int
	// TokensBefore and TokensAfter are estimates over the WHOLE message
	// slice, not just the tool results, so the numbers are comparable with
	// Result.TokensBefore/After.
	TokensBefore, TokensAfter int
}

// FoldOptions configures a fold pass.
type FoldOptions struct {
	// Budget is the token budget the history must fit. Pressure is measured
	// against it. Zero disables folding entirely — with no budget there is no
	// pressure to relieve, and folding unconditionally would degrade every
	// history including the ones that fit comfortably.
	Budget int
	// KeepRecent overrides FoldKeepRecent. Negative is clamped to 0; note that
	// 0 means "fold even the newest result", which is a real setting for a
	// caller doing a last-ditch compaction, not a mistake.
	KeepRecent int
	// used marks KeepRecent as explicitly set, so 0 is distinguishable from
	// unset. See foldKeepRecent.
	KeepRecentSet bool
}

// foldKeepRecent resolves the effective keep-recent count.
func (o FoldOptions) foldKeepRecent() int {
	if !o.KeepRecentSet {
		return FoldKeepRecent
	}
	if o.KeepRecent < 0 {
		return 0
	}
	return o.KeepRecent
}

// ToolResultPressure returns the fraction of budget currently occupied by tool
// results in msgs. It returns 0 for a non-positive budget ("unbudgeted", which
// every function in this package reads as "no limit").
//
// IT MEASURES TOOL RESULTS ONLY, deliberately. The question folding answers is
// not "is the history large" — Plan and RunSummary already handle that — but
// "is the history large BECAUSE OF material that can be recovered on demand".
// A history that is 90% full of user turns and assistant reasoning is equally
// close to the wall and folding can do nothing about it; measuring total size
// would fire the fold pass on exactly those histories and rewrite the handful
// of tool results they contain for no benefit.
func ToolResultPressure(msgs []*schema.Message, budget int) float64 {
	if budget <= 0 {
		return 0
	}
	tok := 0
	for _, m := range msgs {
		if isToolResult(m) {
			tok += estimateMessageTokens(m)
		}
	}
	return float64(tok) / float64(budget)
}

// PressureFold returns the tier to apply at a given pressure.
//
// The escalation is a step function rather than a continuous one because the
// tiers are qualitatively different acts — keep some of the text, or keep none
// of it — and interpolating between them has no meaning.
func PressureFold(pressure float64) FoldTier {
	switch {
	case pressure >= FoldDigestPressure:
		return FoldDigest
	case pressure >= FoldPressureThreshold:
		return FoldTruncated
	}
	return FoldNone
}

// FoldToolResults rewrites older tool results in msgs down to a tier chosen by
// the CURRENT PRESSURE, and returns the new slice plus what it did.
//
// WHY PRESSURE AND NOT PER-MESSAGE SIZE. The tools package already caps a
// SINGLE result at 64 KiB (its spillover). That cap is per-message and
// therefore blind to the failure that actually fills a coding agent's window:
// a hundred results of ten kilobytes each. Every one is comfortably under the
// cap, none of them spills, and together they are a megabyte. The trigger has
// to be the aggregate, which is what this measures.
//
// WHY IT LIVES HERE AND NOT IN internal/tools. Two reasons, and the second is
// the load-bearing one:
//
//   - Spillover is an OUTPUT-TIME decision about one result, made when the
//     tool returns and with no view of the conversation. Folding is a
//     WINDOW-TIME decision about the whole history, made when the history is
//     about to be sent and needing to see all of it at once. Different inputs,
//     different moments.
//   - The dependency arrow. ctxcompact is a leaf: it imports no other internal
//     package. Putting the pressure calculation in tools and having ctxcompact
//     call it would point ctxcompact outward at the tool hub, which GOV1
//     rejects — and rightly, since compaction has no business knowing what a
//     tool is. The reverse (tools importing ctxcompact) would put a compaction
//     policy inside every tool call.
//
// Nothing is lost that cannot be recovered: every folded message keeps a
// RECOVERY POINTER naming where the full text still lives. See foldMarker.
//
// The input slice is not modified — folded messages are copies, and unfolded
// ones keep their original pointer, so an unpressured history allocates
// nothing beyond the outer slice.
func FoldToolResults(msgs []*schema.Message, opts FoldOptions) ([]*schema.Message, FoldStats) {
	stats := FoldStats{TokensBefore: EstimateTokens(msgs)}
	stats.TokensAfter = stats.TokensBefore
	if len(msgs) == 0 || opts.Budget <= 0 {
		return msgs, stats
	}
	tier := PressureFold(ToolResultPressure(msgs, opts.Budget))
	if tier == FoldNone {
		return msgs, stats
	}

	// Walk backwards so "the most recent N tool results" is counted in the
	// order the model sees them, not the order they were produced in some
	// other slice.
	keep := opts.foldKeepRecent()
	out := make([]*schema.Message, len(msgs))
	copy(out, msgs)
	seen := 0
	for i := len(out) - 1; i >= 0; i-- {
		m := out[i]
		if !isToolResult(m) {
			continue
		}
		stats.Considered++
		if seen < keep {
			seen++
			continue // the model is working with these right now
		}
		seen++
		folded, applied := foldOne(m, tier)
		if applied == FoldNone {
			continue
		}
		out[i] = folded
		switch applied {
		case FoldTruncated:
			stats.Truncated++
		case FoldDigest:
			stats.Digested++
		}
	}
	stats.TokensAfter = EstimateTokens(out)
	return out, stats
}

// foldOne rewrites a single tool result to at most the given tier, returning
// the new message and the tier actually applied.
//
// The tier is a CEILING, not a command: a result already shorter than
// foldMinRunes is left alone whatever the pressure, and one whose truncated
// form would not be shorter than the original is left alone too. Folding that
// does not shrink anything still costs the marker text and still destroys the
// content, which is the worst of both.
func foldOne(m *schema.Message, tier FoldTier) (*schema.Message, FoldTier) {
	body := m.Content
	if len([]rune(body)) < foldMinRunes {
		return m, FoldNone
	}
	var replacement string
	switch tier {
	case FoldDigest:
		replacement = digestBody(body, m)
	case FoldTruncated:
		replacement = truncateBody(body, m)
	default:
		return m, FoldNone
	}
	if len(replacement) >= len(body) {
		return m, FoldNone
	}
	cp := *m
	cp.Content = replacement
	return &cp, tier
}

// truncateBody keeps a head and a tail with an elision marker between them.
func truncateBody(body string, m *schema.Message) string {
	lines := strings.Split(body, "\n")
	if len(lines) <= foldHeadLines+foldTailLines {
		// Few lines but long ones (a single-line JSON blob, a minified file).
		// Fall through to the digest form: there is no useful head/tail split
		// to make, and leaving it verbatim would ignore the pressure that got
		// us here.
		return digestBody(body, m)
	}
	head := strings.Join(lines[:foldHeadLines], "\n")
	tail := strings.Join(lines[len(lines)-foldTailLines:], "\n")
	omitted := len(lines) - foldHeadLines - foldTailLines
	return fmt.Sprintf("%s\n[... %d lines folded out of context; %s ...]\n%s",
		head, omitted, foldMarker(body, m), tail)
}

// digestBody replaces the whole body with one descriptive line.
func digestBody(body string, m *schema.Message) string {
	lines := strings.Count(body, "\n") + 1
	first := strings.TrimSpace(firstNonEmptyLine(body))
	digest := fmt.Sprintf("[folded tool result: %d lines, %d bytes; %s]",
		lines, len(body), foldMarker(body, m))
	if first != "" {
		digest += "\n" + firstRunes(first, 120)
	}
	return digest
}

// foldMarker builds the RECOVERY POINTER: the instruction a model follows to
// get the folded text back.
//
// WITHOUT THIS, FOLDING IS JUST DELETION. The three sources are tried in order
// of how directly they resolve:
//
//  1. A SPILLOVER PATH already in the body. When a result was large enough to
//     spill, internal/tools wrote it to a file and put the relative path in
//     the preview. That path is still valid and fs_read reads it — so the
//     pointer costs nothing to produce and resolves to the exact bytes.
//  2. The TOOL CALL ID. Persisted alongside the message, so history_search can
//     find the message the result belongs to.
//  3. Nothing resolvable, in which case the marker says so rather than
//     inventing a handle. A pointer that looks recoverable and is not is worse
//     than an honest "not recoverable": it costs a tool call and teaches the
//     model that the pointers are noise.
func foldMarker(body string, m *schema.Message) string {
	if path := spillPathIn(body); path != "" {
		return "full text: fs_read(\"" + path + "\")"
	}
	if m.ToolCallID != "" {
		return "recover via history_search for tool_call " + m.ToolCallID
	}
	return "full text not retained"
}

// spillMarkerPrefix is the opening of the preview internal/tools writes in
// place of an oversized result. It is matched TEXTUALLY rather than by
// importing the tools package: ctxcompact is a leaf (see FoldToolResults), and
// the alternative to reading the marker is discarding a pointer that is
// already sitting in the message.
//
// If tools changes the marker this stops finding paths and foldMarker falls
// back to the tool-call id — a worse pointer, not a wrong one. The coupling is
// therefore soft in the safe direction, and
// TestSpillMarkerMatchesToolsPreview pins the two forms against each other so
// the drift is noticed rather than merely survived.
const spillMarkerPrefix = "[spilled: "

// spillPathIn extracts the spillover file path from a tools-generated preview
// header, which has the form "[spilled: <n> lines / <size> → <path>]".
func spillPathIn(body string) string {
	i := strings.Index(body, spillMarkerPrefix)
	if i < 0 {
		return ""
	}
	rest := body[i+len(spillMarkerPrefix):]
	end := strings.IndexByte(rest, ']')
	if end < 0 {
		return ""
	}
	header := rest[:end]
	arrow := strings.LastIndex(header, "→")
	if arrow < 0 {
		return ""
	}
	return strings.TrimSpace(header[arrow+len("→"):])
}

// firstNonEmptyLine returns the first line of s with content, used as the
// digest's one-line preview.
func firstNonEmptyLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			return line
		}
	}
	return ""
}

// isToolResult reports whether m is a tool result message. eino encodes these
// as Role==Tool with a ToolCallID; the id is required because
// EnforceToolCallPairs pairs on it, and a Tool-role message without one is not
// something folding should touch.
func isToolResult(m *schema.Message) bool {
	return m != nil && m.Role == schema.Tool && m.ToolCallID != ""
}
