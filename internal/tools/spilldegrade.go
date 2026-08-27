// Immediate, per-round degradation of tool results (T4).
//
// spillover.go caps ONE result at SpillThreshold (64 KiB) at the moment the
// tool returns. That cap answers "is this single result absurd", and nothing
// else: sixty results of forty kilobytes each are all comfortably under it, so
// the window fills with material the model finished reading several iterations
// ago while every individual result looks fine.
//
// This file is the other half — the DEGRADE tier. It shrinks a result that is
// no longer the one being worked on down to DegradeMaxBytes, writing the full
// text to the same spillover directory first so the shrink is recoverable. It
// is a port of QwenPaw's ToolResultPruningMiddleware (agents/middlewares.py):
// there, recent_n results keep the large cap and older ones are cut to
// old_max_bytes with the full output saved under tool_results_dir.
//
// # How this differs from ctxcompact's fold (C5), which it does NOT replace
//
// Both shrink historical tool results, and running both is intended:
//
//   - ctxcompact.FoldToolResults is PRESSURE-DRIVEN and WINDOW-TIME. It fires
//     only past FoldPressureThreshold, and it never writes to disk: its
//     recovery pointer is whatever handle it can already find in the message
//     (a spillover path in the body, else the tool-call id).
//   - This is UNCONDITIONAL and PRODUCTION-TIME. Every result over the degrade
//     threshold gets a disk copy the moment it stops being recent, whether or
//     not the window is under pressure.
//
// The second point is what makes the pair worth having: fold's best pointer is
// a spillover path, and a result between DegradeMaxBytes and SpillThreshold
// never had one, so folding it used to fall back to the weaker tool-call-id
// form. Degrading first means the strong pointer always exists by the time
// fold looks for it.
//
// # The marker format is load-bearing
//
// The preview this produces reuses spillPreview, so the header is byte-for-byte
// the one ctxcompact.spillPathIn parses ("[spilled: N lines / SIZE → PATH]").
// That coupling is soft in the safe direction (a drift degrades the pointer, it
// does not corrupt it) and is pinned from the other side by
// internal/ctxcompact::TestSpillMarkerMatchesToolsPreview. Pinned from this
// side by TestDegradeUsesTheFoldRecognisedMarker.
package tools

import (
	"context"
	"strings"
)

// DegradeMaxBytes is the ceiling a NO-LONGER-RECENT tool result is cut to.
//
// 3 KiB is QwenPaw's old_max_bytes (3000) rounded to a power-of-two boundary:
// enough for a head-and-tail that still shows a command's exit status and its
// last error line, small enough that a hundred of them cost roughly the same
// as two undegraded ones. It is deliberately a long way below SpillThreshold —
// the two tiers answer different questions and a degrade tier close to the
// spill cap would save nothing.
const DegradeMaxBytes = 3 * 1024

// DegradeKeepRecent is how many trailing tool results are never degraded.
//
// QwenPaw uses recent_n = 2. The model is mid-task on these: a ReAct loop
// reads, decides, and writes, and cutting the read it is about to act on makes
// it read again — which costs more than the cut saved. Two covers the
// call-and-its-predecessor that a single decision usually spans.
//
// ctxcompact.FoldKeepRecent is 5 and that is not a contradiction: fold runs
// only under window pressure and is the last line before eviction, so it keeps
// a wider margin. This runs every iteration, so its margin is the working set
// of one decision.
const DegradeKeepRecent = 2

// degradeMinBytes is the size below which degrading is refused outright.
// Below it the preview scaffolding (header, elision marker, guidance line)
// approaches the size of the body, so the rewrite costs window instead of
// saving it, and destroys the content as well.
const degradeMinBytes = DegradeMaxBytes / 2

// AlreadyDegraded reports whether body already carries a spillover preview
// header, i.e. this text IS the shrunken form of something larger.
//
// Callers use it to stay idempotent. Degrading an already-degraded result
// would write a second disk copy of a preview (not of the original), and the
// new pointer would resolve to strictly less than the old one — a recovery
// pointer that gets worse each time it is followed is worse than none.
func AlreadyDegraded(body string) bool {
	return strings.Contains(body, spillHeaderPrefix)
}

// spillHeaderPrefix opens the preview spillPreview writes. It is named here
// (rather than inlined) because two consumers now depend on the exact bytes:
// AlreadyDegraded, and ctxcompact.spillPathIn across the package boundary.
const spillHeaderPrefix = "[spilled: "

// DegradeToolResult shrinks one historical tool result to at most
// DegradeMaxBytes, writing the full text to <workRoot>/.yanshi/tmp/spillover/
// first so the shrink stays recoverable.
//
// It returns (newBody, true) when it rewrote the body, and (body, false) when
// it declined. It declines — rather than doing a best-effort cut — in every
// case where the rewrite would not be a clear improvement:
//
//   - the body is already at or under degradeMinBytes (nothing worth saving);
//   - the body is already a spillover preview (see AlreadyDegraded);
//   - the disk write failed, so no recovery pointer can be produced. This is
//     the important one: cutting text whose original is nowhere is DELETION,
//     and a full window is a smaller problem than a silently truncated test
//     log the model is about to draw a conclusion from.
//
// toolName only labels the temp file. workRoot comes from ctx (WithWorkRoot);
// an unbound root falls back to "." exactly as spillIfTooLong does.
func DegradeToolResult(ctx context.Context, toolName, body string) (string, bool) {
	if len(body) <= degradeMinBytes || AlreadyDegraded(body) {
		return body, false
	}
	rel, ok := spillFullText(ctx, toolName, body)
	if !ok {
		return body, false
	}
	out := degradePreview(body, rel)
	if len(out) >= len(body) {
		// Pathological input (very few, very long lines) where the preview did
		// not actually shrink anything. The disk copy is harmless and the
		// janitor sweeps it; keeping the original text is the right answer.
		return body, false
	}
	return out, true
}

// degradePreview builds the shrunken body. It reuses spillPreview so the header
// is identical to the one the 64 KiB spill path emits — see this file's header
// for why that identity is load-bearing — then enforces the tighter
// DegradeMaxBytes ceiling that spillPreview's own 24 KiB budgets do not.
//
// The final cut is byte-wise on a rune boundary rather than line-wise: the
// budget is already small, and a preview whose tail was dropped to hit the
// budget still ends with the elision marker that says so.
func degradePreview(body, rel string) string {
	preview := spillPreview(body, rel)
	if len(preview) <= DegradeMaxBytes {
		return preview
	}
	cut := truncateRunes(preview, DegradeMaxBytes-len(degradeCutMarker))
	return cut + degradeCutMarker
}

// degradeCutMarker terminates a preview that had to be cut a second time to
// fit DegradeMaxBytes. It names the budget so the reason is visible in the
// transcript rather than looking like a truncated stream.
const degradeCutMarker = "\n[... degraded to the per-result budget; full text at the path above ...]"

// truncateRunes cuts s to at most n bytes without splitting a UTF-8 rune.
// A non-positive n yields "".
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	// Walk back from the byte cut until the boundary starts a rune. At most
	// three steps: a UTF-8 sequence is four bytes at the widest.
	for n > 0 && !utf8Start(s[n]) {
		n--
	}
	return s[:n]
}

// utf8Start reports whether b begins a UTF-8 sequence (i.e. is not a 10xxxxxx
// continuation byte).
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }
