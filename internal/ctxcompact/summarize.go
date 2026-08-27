// internal/ctxcompact/summarize.go
package ctxcompact

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/cloudwego/eino/schema"
)

const (
	summaryRetryMax    = 3
	summaryRetryBaseMs = 1000
	carryAckContent    = "Understood — continuing with the prior summary as context."
)

// ErrNoWindowRoom reports that the carried summary plus the fixed framing
// (ack + instruction) already fills the model window, leaving no budget for
// even one message of the next chunk — so the carry loop cannot make progress
// and RunSummary stops instead of silently over-running the window.
//
// It is a sentinel rather than an anonymous fmt.Errorf because it is the ONE
// failure a caller may legitimately tolerate: a window small enough to hit it
// was never going to be compacted at all. Everything else RunSummary can
// return (a model error, a stream error, a retry exhaustion) is a real
// failure. Without a distinguishable sentinel, a test that wants to tolerate
// the first has to tolerate all of them — which is exactly how
// TestProperty_EachSummaryCallWithinWindow ended up passing against a
// RunSummary gutted to `return "", err`.
var ErrNoWindowRoom = errors.New("compaction: carry + framing leaves no room in the model window")

// RunSummary produces a text summary of msgs via m. If the estimated token count
// fits within ChunkThreshold*(ModelWindow−OutputReserve) it makes a single
// CACHE-ALIGNED call (msgs verbatim as prefix + a trailing instruction) so the
// summary request hits the prefix cache. Otherwise it runs a CARRY-style rolling
// summary where each chunk's budget is computed DYNAMICALLY from the current
// carry:
//
//	chunkBudget = ModelWindow − OutputReserve − carry − ack − instruction
//
// so chunk + carry(prefix) + ack + instruction ≤ ModelWindow − OutputReserve.
//
// That inequality holds for the framing, NOT unconditionally for the chunk:
// takeChunk may exceed its budget, and the excess is bounded by the largest
// INDIVISIBLE run in the history rather than by any multiple of the window.
// See takeChunk for the two ways a run becomes indivisible. Retries transient
// errors.
//
// When opts.Redactor is set, the messages are passed through it FIRST, so no
// registered secret reaches the summary model — and, more importantly, none can
// be folded into a summary that is then re-sent on every later turn. The
// redaction applies to a COPY; the caller's slice is untouched. See
// redactForSummary.
//
// The summary it asks for is the five-section structured document described by
// summaryInstruction. Two things follow from that and are worth stating here,
// because both are easy to break from this function:
//
//   - The INCREMENTAL-UPDATE instruction is selected per call, from whether the
//     request being built already contains a summary. On the carry path that is
//     true from chunk 2 onward (the carry IS the prior summary); on the single
//     path it is true when an earlier compaction's summary has aged out of the
//     pinned tail and back into the summarize set. Deciding it once up front
//     would get chunk 1 of the carry loop wrong in one direction or every later
//     chunk wrong in the other.
//   - The instruction's token cost is therefore NOT constant across chunks —
//     the update form is longer than the produce form. instructionTokens is
//     measured for the form actually about to be sent, so the chunk budget
//     shrinks to match rather than being computed against a cheaper message
//     than the one that gets dispatched.
func RunSummary(ctx context.Context, msgs []*schema.Message, opts RunOpts, m ModelSummarizer, onChunk func(string)) (string, error) {
	if len(msgs) == 0 {
		return "", nil
	}
	// C11: redact before ANY branch below can reach the model. Placing this at
	// the top of RunSummary rather than in Run means the direct RunSummary
	// callers (tests, and any future caller that summarizes without planning)
	// get the same protection — a redaction that only covers one entry point is
	// a redaction with a documented hole.
	msgs = redactForSummary(opts.Redactor, msgs)
	instruction := instructionMessage(opts.SummaryWordLimit, containsPriorSummary(msgs), opts.CoveredSeq, opts.ModelWindow)
	instructionTok := estimateMessageTokens(instruction)

	// Single cache-aligned path: messages + instruction fit the threshold budget.
	if sb := singleBudget(opts); sb > 0 && EstimateTokens(msgs)+instructionTok <= sb {
		req := append([]*schema.Message{}, msgs...)
		req = append(req, instruction)
		s, err := callWithRetry(ctx, m, req, onChunk)
		if err != nil {
			return "", err
		}
		return redactSummary(opts.Redactor, s), nil
	}

	// Carry-style chunked. carry grows each iteration, so chunkBudget must shrink
	// to match — chunkBudgetFor recomputes from the live carry each pass.
	carry := ""
	remaining := msgs
	for chunkIdx := 1; len(remaining) > 0; chunkIdx++ {
		// The carry IS a prior summary, so from chunk 2 onward the model is
		// asked to UPDATE it rather than write a fresh one. That is the whole
		// point of the carry loop: without it each chunk produces an
		// independent summary of its own slice and the last one silently wins.
		hasPrior := carry != "" || containsPriorSummary(remaining)
		chunkInstructionTok := instructionTok
		if hasPrior != containsPriorSummary(msgs) {
			chunkInstructionTok = estimateMessageTokens(
				instructionMessage(opts.SummaryWordLimit, hasPrior, opts.CoveredSeq, opts.ModelWindow))
		}
		chunkBudget := chunkBudgetFor(opts, carry, chunkInstructionTok)
		if chunkBudget <= 0 {
			// The carry alone (+framing) fills the window — no safe progress.
			// Surfaces as an error rather than silently over-running the window.
			return "", fmt.Errorf("%w: carry is %d tok, window is %d", ErrNoWindowRoom,
				estimateMessageTokens(&schema.Message{Role: schema.User, Content: SummarySentinel + carry}),
				opts.ModelWindow)
		}
		chunk, rest := takeChunk(remaining, chunkBudget)
		req := buildCarryRequest(carry, chunk, opts.SummaryWordLimit, hasPrior, opts.CoveredSeq, opts.ModelWindow)
		s, err := callWithRetry(ctx, m, req, onChunk)
		if err != nil {
			return "", fmt.Errorf("compaction chunk %d: %w", chunkIdx, err)
		}
		carry = s
		remaining = rest
	}
	return redactSummary(opts.Redactor, carry), nil
}

// singleBudget is the threshold (ChunkThreshold*ModelWindow) for the single
// cache-aligned call. Returns 0 when compaction can't be budgeted (ModelWindow
// unset), which disables the single path.
//
// IT IS DELIBERATELY NOT budgetFor. The output reserve belongs to the TURN's
// budget — how much history the conversation may carry so the assistant can
// still reply — and this is the SUMMARY CALL's budget, a different request with
// a different output. Subtracting the turn's reserve here would charge the
// summary call for a completion it is not the one generating, and the two
// headrooms would compound: ChunkThreshold (0.9 by default) is already the
// summary call's own output headroom, so a 0.75 window reserve on top of it
// leaves 0.675 of the window for input and pushes histories onto the chunked
// path that fit the single cache-aligned call perfectly well — losing the
// prefix-cache hit that path exists for.
//
// See budgetFor for the budget the reserve DOES apply to.
func singleBudget(opts RunOpts) int {
	if opts.ModelWindow <= 0 {
		return 0
	}
	if opts.ChunkThreshold <= 0 {
		return opts.ModelWindow
	}
	return int(float64(opts.ModelWindow) * opts.ChunkThreshold)
}

// chunkBudgetFor returns the max tokens a chunk's MESSAGES may occupy so that
// chunk + carry(prefix) + ack + instruction ≤ ModelWindow. overhead shrinks the
// available budget; carry (which grows each chunk) is counted at its current
// size, not a fixed estimate — this is what keeps every call in-window.
//
// Like singleBudget, this is the SUMMARY CALL's budget and so does not subtract
// the turn's output reserve — see the note there.
func chunkBudgetFor(opts RunOpts, carry string, instructionTok int) int {
	if opts.ModelWindow <= 0 {
		return 0
	}
	overhead := instructionTok
	if carry != "" {
		overhead += estimateMessageTokens(&schema.Message{Role: schema.User, Content: SummarySentinel + carry})
		overhead += estimateMessageTokens(&schema.Message{Role: schema.Assistant, Content: carryAckContent})
	}
	return opts.ModelWindow - overhead
}

// takeChunk returns the longest prefix of msgs whose token sum ≤ budget, plus
// the remainder. If the next message would sever a tool_call/tool_result pair
// (splitIsSafe false), it is included in the current chunk EVEN IF that pushes
// the chunk over budget — pair integrity outranks strict budget (a severed pair
// means API 400). NB: never returns an empty chunk when len(msgs) > 0.
//
// THE OVERSHOOT IS NOT BOUNDED BY ANY MULTIPLE OF THE WINDOW. Two independent
// mechanisms push a chunk past its budget, and only one of them is pairing:
//
//  1. `i > 0` in the test below means index 0 is never budget-checked. It
//     cannot be — a chunk that came back empty would leave `remaining`
//     unchanged and spin RunSummary's carry loop forever — so a single message
//     larger than the whole budget ships as its own oversized chunk with no
//     tool pair involved at all.
//  2. splitIsSafe scans the ENTIRE left side for a matching call, so in
//     `[call(id1..idN), r1..rN]` every interior cut point is unsafe and the
//     whole parallel group ships as one chunk. That chunk is "one call message
//     plus the sum of its results", which grows with the parallel tool count.
//     Orchestrator classify.go emits exactly this shape.
//
// The real ceiling is therefore `budget + <largest indivisible run>`, a
// property of the INPUT. It is documented rather than enforced: capping below
// the head run would require severing a pair (400) or truncating message
// content mid-chunk (silent information loss). See
// TestProperty_EachSummaryCallWithinWindow, which asserts that ceiling, and
// docs/compaction.md for the measurements.
func takeChunk(msgs []*schema.Message, budget int) (chunk, rest []*schema.Message) {
	tok := 0
	for i := 0; i < len(msgs); i++ {
		mt := estimateMessageTokens(msgs[i])
		if i > 0 && tok+mt > budget && splitIsSafe(msgs, i) {
			return msgs[:i], msgs[i:]
		}
		tok += mt
	}
	return msgs, nil
}

// buildCarryRequest assembles the model input for one carry chunk: prior summary
// as a sentinel-prefixed user turn (+ ack), the chunk's messages verbatim, then
// the instruction. chunk1 has no carry prefix so its prefix == original history
// opening (cache-aligned for that one block).
//
// hasPrior is passed in rather than derived from `carry != ""` because a prior
// summary can also arrive inside the CHUNK — an earlier compaction's output
// that has since aged out of the pinned tail. Deriving it here would miss that
// case on chunk 1, which is precisely the second-compaction path this whole
// structure exists to get right.
func buildCarryRequest(carry string, chunk []*schema.Message, wordLimit int, hasPrior bool, covered SeqRef, window int) []*schema.Message {
	var req []*schema.Message
	if carry != "" {
		req = append(req, &schema.Message{Role: schema.User, Content: SummarySentinel + carry})
		req = append(req, &schema.Message{Role: schema.Assistant, Content: carryAckContent})
	}
	req = append(req, chunk...)
	req = append(req, instructionMessage(wordLimit, hasPrior, covered, window))
	return req
}

// splitIsSafe reports whether msgs[:i] | msgs[i:] severs no tool pair: the
// message ending the left side must not have a tool_call whose result is on the
// right, and the message starting the right side must not be a tool_result whose
// call is on the left.
func splitIsSafe(msgs []*schema.Message, i int) bool {
	if i <= 0 || i >= len(msgs) {
		return true
	}
	left := msgs[i-1]
	if left != nil {
		for _, tc := range left.ToolCalls {
			if tc.ID == "" {
				continue
			}
			for j := i; j < len(msgs); j++ {
				if msgs[j] != nil && msgs[j].ToolCallID == tc.ID {
					return false // result is on the right — would sever
				}
			}
		}
	}
	right := msgs[i]
	if right != nil && right.ToolCallID != "" {
		for j := 0; j < i; j++ {
			if msgs[j] != nil {
				for _, tc := range msgs[j].ToolCalls {
					if tc.ID == right.ToolCallID {
						return false // call is on the left — would sever
					}
				}
			}
		}
	}
	return true
}

// summaryRetryBackoffs returns the exact sleep sequence callWithRetry walks:
// one entry per RETRY, so summaryRetryMax-1 entries, doubling from
// summaryRetryBaseMs.
//
// It exists so the sequence is a value that can be asserted instead of a
// formula that has to be re-derived by hand. summaryRetryMax=3 means 3
// ATTEMPTS and 2 retries — waiting 1s then 2s, a 3s worst case. It does NOT
// mean three backoffs: the loop's last attempt is not followed by a sleep, so
// the doubling stops at 2s and a third step is unreachable on every path.
// docs/compaction.md advertised "1s/2s/4s" for months, which doubles the
// worst-case wait an operator would budget for.
func summaryRetryBackoffs() []time.Duration {
	d := make([]time.Duration, 0, summaryRetryMax-1)
	for i := 0; i < summaryRetryMax-1; i++ {
		d = append(d, summaryRetryBaseMs*(1<<i)*time.Millisecond)
	}
	return d
}

// summaryAfter is the timer seam callWithRetry waits on between attempts. It
// is a var solely so a test can record the durations the loop ACTUALLY sleeps
// and reconcile them against summaryRetryBackoffs.
//
// Without that reconciliation summaryRetryBackoffs is only pinned as a pure
// function: inline the doubling back into the loop (deleting the `backoffs :=`
// line to dodge the unused-variable error) and the function becomes an orphan
// no test and no compiler complains about — while docs/compaction.md keeps
// pointing operators at a value that no longer drives anything. The seam turns
// "the docs quote this" into "the loop obeys this".
var summaryAfter = time.After

// callWithRetry invokes m.Stream (preferring streaming so onChunk gets deltas)
// and falls back to Generate. Retries only transient errors up to summaryRetryMax
// with exponential backoff. Permanent errors surface immediately.
func callWithRetry(ctx context.Context, m ModelSummarizer, msgs []*schema.Message, onChunk func(string)) (string, error) {
	var lastErr error
	backoffs := summaryRetryBackoffs()
	for attempt := 0; attempt < summaryRetryMax; attempt++ {
		if attempt > 0 {
			select {
			case <-summaryAfter(backoffs[attempt-1]):
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		s, err := streamOnce(ctx, m, msgs, onChunk)
		if err == nil {
			return s, nil
		}
		if !isTransient(err) {
			return "", err
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("compaction summary failed")
	}
	return "", lastErr
}

func streamOnce(ctx context.Context, m ModelSummarizer, msgs []*schema.Message, onChunk func(string)) (string, error) {
	sr, err := m.Stream(ctx, msgs)
	if err != nil {
		// Fall back to Generate when Stream returns an error (e.g. a provider
		// that can't stream a summary call). A mid-stream Recv failure does NOT
		// fall back — it surfaces to callWithRetry, which retries the whole call.
		if msg, gerr := m.Generate(ctx, msgs); gerr == nil && msg != nil {
			if onChunk != nil && msg.Content != "" {
				onChunk(msg.Content)
			}
			return msg.Content, nil
		}
		return "", err
	}
	defer sr.Close()
	var sb strings.Builder
	for {
		msg, recvErr := sr.Recv()
		if errors.Is(recvErr, io.EOF) {
			break
		}
		if recvErr != nil {
			return "", recvErr
		}
		if msg == nil || msg.Content == "" {
			continue
		}
		sb.WriteString(msg.Content)
		if onChunk != nil {
			onChunk(msg.Content)
		}
	}
	return sb.String(), nil
}

// isTransient classifies an error as retryable. This is the SECOND line of
// defense: in production the summarizer model is usually a ResilientChatModel
// that has ALREADY filtered 4xx and exhausted its own transient retries before
// the error reaches here. These keywords are a conservative superset for the
// cases that slip through (or when a bare model is passed, e.g. in tests). 4xx
// short-circuiting intentionally lives in internal/llm/eino/resilient.go and is
// NOT duplicated here — importing it would point ctxcompact's dependency arrow
// outward, violating the hexagonal layout in CLAUDE.md.
func isTransient(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false // user cancel / timeout-abort is not a retry case here
	}
	msg := strings.ToLower(err.Error())
	for _, m := range []string{"timeout", "timed out", "connection reset", "eof", "broken pipe", "temporary", "429", "503", "502", "retry"} {
		if strings.Contains(msg, m) {
			return true
		}
	}
	return false
}
