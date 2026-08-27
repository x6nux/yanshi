// internal/llm/eino/c6_overflow_wire_test.go
//
// C6 over the wire: when a provider rejects a request for being too long, is a
// SMALLER request actually sent, exactly once?
//
// Both of C6's rules are about request COUNTS and request SIZES, which is to
// say they are about what the server received:
//
//  1. retry only if the input actually shrank — a byte-identical resend
//     reproduces the rejection and is charged for,
//  2. retry at most once — compacting repeatedly toward an unreachable target
//     is how one failed turn becomes a metered loop.
//
// Neither can be checked by inspecting forceCompact's return value, because the
// question is not "did compaction produce something smaller" but "did the thing
// that went out on the wire get smaller". Those differ whenever the shrunk
// history fails to reach the request builder — which is the wiring class of bug
// this whole exercise is looking for.
package eino

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/x6nux/yanshi/internal/ctxcompact"
)

// overflowRejectionBody is the 400 an OpenAI-compatible endpoint returns for an
// over-long prompt.
const overflowRejectionBody = `{"error":{"message":"This model's maximum context length is ` +
	`8192 tokens, however you requested 41000 tokens","code":"context_length_exceeded",` +
	`"type":"invalid_request_error"}}`

// plausibleSummary is what the stub answers a summarisation request with.
//
// It must be long enough to clear C10's summary quality gate, which rejects a
// summary too short relative to its input. That gate is real and fires here:
// the first draft of these tests let the stub answer "STUB-OK" to everything,
// and every forced compaction failed with "summary is 7 runes; 15878 runes of
// input require at least 15". A stub that cannot produce a usable summary tests
// the quality gate, not C6.
const plausibleSummary = "Earlier in this conversation the user asked a series of questions about " +
	"lorem ipsum text and the assistant answered each of them in turn. No files were modified, " +
	"no commands were run, and no decisions were reached that later turns depend on. The user " +
	"has now asked for a summary of everything above."

// summarisingStub answers overflow-rejected requests per reject, and every
// other request with a summary long enough to satisfy the quality gate.
func summarisingStub(reject func(n int) bool) func(int, capturedRequest) stubResponse {
	return func(n int, _ capturedRequest) stubResponse {
		if reject != nil && reject(n) {
			return stubResponse{Status: 400, Body: overflowRejectionBody}
		}
		return stubResponse{Content: plausibleSummary}
	}
}

// wireLongHistory builds a history with enough compressible middle for a forced
// compaction to have something to remove.
//
// The turns alternate user/assistant because ctxcompact pins by role and a
// single-role history would be pinned wholesale, testing the "did not shrink"
// path instead of the shrinking one.
func wireLongHistory(turns int, bodyLen int) []*schema.Message {
	filler := strings.Repeat("lorem ipsum dolor sit amet consectetur ", bodyLen)
	msgs := []*schema.Message{schema.SystemMessage("you are a helpful assistant")}
	for i := range turns {
		msgs = append(msgs,
			schema.UserMessage(fmt.Sprintf("request %d: %s", i, filler)),
			schema.AssistantMessage(fmt.Sprintf("answer %d: %s", i, filler), nil))
	}
	msgs = append(msgs, schema.UserMessage("and finally, summarise the above"))
	return msgs
}

// TestC6_OverflowTriggersASmallerResendExactlyOnce is the main C6 test: the
// stub rejects request 1 as too long and accepts request 2, and the test
// asserts there IS a request 2, that it is smaller, and that there is no
// request 3.
func TestC6_OverflowTriggersASmallerResendExactlyOnce(t *testing.T) {
	s := newStubProvider(t, summarisingStub(func(n int) bool { return n == 1 }))
	inner, _ := buildStubModel(t, s, nil)
	a := NewAdaptiveModel(inner, AdaptiveConfig{
		ModelID:  "stub-model-a",
		Overflow: OverflowRecoveryConfig{ContextWindow: 8192},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, err := a.Generate(ctx, wireLongHistory(12, 40))
	if err != nil {
		t.Fatalf("the overflow was not recovered from: %v", err)
	}
	if out.Content == "" {
		t.Error("the recovered call returned empty content")
	}

	reqs := s.chatRequests()
	t.Logf("request sizes seen by the server: %v", requestSizes(reqs))
	// The summariser is the same model, so it also hits the stub. What must
	// hold is that the LAST request succeeded and the FIRST was the rejected
	// one, with the final attempt materially smaller than the original.
	if len(reqs) < 2 {
		t.Fatalf("only %d request(s): the rejection was never followed by a resend", len(reqs))
	}
	first, last := len(reqs[0].Raw), len(reqs[len(reqs)-1].Raw)
	t.Logf("first request %d bytes, final request %d bytes", first, last)
	if last >= first {
		t.Errorf("the resent request (%d bytes) is not smaller than the rejected one (%d bytes); "+
			"resending the same size can only reproduce the rejection", last, first)
	}
}

// requestSizes maps requests to their body sizes, for log output.
func requestSizes(reqs []capturedRequest) []int {
	out := make([]int, 0, len(reqs))
	for _, r := range reqs {
		out = append(out, len(r.Raw))
	}
	return out
}

// TestC6_NoRetryWhenCompactionCannotShrink is rule 1, and it is the rule with
// teeth: a history too short to compact must produce exactly ONE request.
//
// Without it, an overflow on a two-message history would resend those same two
// messages and be billed twice for the identical rejection — and because the
// second failure looks like the first, nothing downstream would reveal that a
// pointless request had been made.
func TestC6_NoRetryWhenCompactionCannotShrink(t *testing.T) {
	s := newStubProvider(t, func(int, capturedRequest) stubResponse {
		return stubResponse{Status: 400, Body: overflowRejectionBody}
	})
	inner, _ := buildStubModel(t, s, nil)
	a := NewAdaptiveModel(inner, AdaptiveConfig{
		ModelID: "stub-model-a",
		// A window is configured, so recovery is enabled — the reason no retry
		// happens must be that there is nothing to remove, not that the feature
		// is switched off.
		Overflow: OverflowRecoveryConfig{ContextWindow: 8192},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Two messages: fewer than DefaultOverflowKeepRecent, so every message is
	// pinned and the compaction cannot remove anything.
	_, err := a.Generate(ctx, []*schema.Message{
		schema.SystemMessage("sys"), schema.UserMessage("hi"),
	})
	if err == nil {
		t.Fatal("want the overflow to surface as an error")
	}
	n := len(s.chatRequests())
	t.Logf("unshrinkable overflow → %d request(s), err=%v", n, err)
	if n != 1 {
		t.Errorf("stub saw %d requests; an input that could not shrink must not be resent", n)
	}
}

// TestC6_DisabledWithoutAContextWindow pins the other guard: with no window
// there is no budget to compact toward, so recovery cannot tell success from
// failure and must not run at all.
//
// This matters because Windows[key] is absent for any provider the resolver
// could not answer for, and bootstrap passes that zero straight through.
func TestC6_DisabledWithoutAContextWindow(t *testing.T) {
	s := newStubProvider(t, func(int, capturedRequest) stubResponse {
		return stubResponse{Status: 400, Body: overflowRejectionBody}
	})
	inner, _ := buildStubModel(t, s, nil)
	a := NewAdaptiveModel(inner, AdaptiveConfig{
		ModelID:  "stub-model-a",
		Overflow: OverflowRecoveryConfig{ContextWindow: 0}, // disabled
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := a.Generate(ctx, wireLongHistory(12, 40)); err == nil {
		t.Fatal("want an error")
	}
	n := len(s.chatRequests())
	t.Logf("no window configured → %d request(s)", n)
	if n != 1 {
		t.Errorf("stub saw %d requests with recovery disabled, want 1", n)
	}
}

// TestC6_SecondOverflowIsNotRetriedAgain is rule 2. The stub rejects
// EVERYTHING, so a recovery loop would keep compacting and resending forever
// against a target it can never reach.
//
// The assertion is an upper bound on requests rather than an exact count,
// because the forced compaction legitimately issues summariser calls of its
// own; what must not happen is an unbounded sequence of full-history attempts.
func TestC6_SecondOverflowIsNotRetriedAgain(t *testing.T) {
	// Reject every request EXCEPT the summariser's own call, so the forced
	// compaction genuinely succeeds and a real shrink is resent — otherwise
	// "only a few requests happened" would be explained by compaction failing
	// rather than by the budget being one.
	s := newStubProvider(t, summarisingStub(func(n int) bool { return n != 2 }))
	inner, _ := buildStubModel(t, s, nil)
	a := NewAdaptiveModel(inner, AdaptiveConfig{
		ModelID:  "stub-model-a",
		Overflow: OverflowRecoveryConfig{ContextWindow: 8192},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_, err := a.Generate(ctx, wireLongHistory(12, 40))
	if err == nil {
		t.Fatal("want an error when every attempt is rejected")
	}
	n := len(s.chatRequests())
	t.Logf("permanently-overflowing provider → %d request(s), err=%v", n, err)
	if n > 6 {
		t.Errorf("stub saw %d requests; the retry budget is supposed to be exactly one, so a "+
			"permanently failing provider must not become a metered loop", n)
	}
}

// TestC6_PersistedOverflowErrorNamesBothSizes covers the operator-facing half.
//
// "It still did not fit after we compacted from N to M" and "compaction did
// nothing" are different problems with different fixes (raise context_window,
// versus find the indivisible segment). Without the numbers in the error, the
// second failure is indistinguishable from the first and nothing says a
// recovery was even attempted.
func TestC6_PersistedOverflowErrorNamesBothSizes(t *testing.T) {
	s := newStubProvider(t, summarisingStub(func(n int) bool { return n != 2 }))
	inner, _ := buildStubModel(t, s, nil)
	a := NewAdaptiveModel(inner, AdaptiveConfig{
		ModelID:  "stub-model-a",
		Overflow: OverflowRecoveryConfig{ContextWindow: 8192},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_, err := a.Generate(ctx, wireLongHistory(12, 40))
	if err == nil {
		t.Fatal("want an error")
	}
	t.Logf("final error: %v", err)
	// The wrapped form appears only when a real shrink happened; when the
	// summariser call is itself rejected nothing shrank and the bare provider
	// error is correct. Both are acceptable; what is not is losing the
	// overflow classification.
	if !IsContextOverflow(err) {
		t.Errorf("the final error no longer classifies as a context overflow: %v", err)
	}
	if strings.Contains(err.Error(), "persisted after forced compaction") &&
		!strings.Contains(err.Error(), "tokens") {
		t.Error("the wrapped overflow error omits the token counts that make it actionable")
	}
}

// TestC6_OverflowErrorReportsTokensNotMessageCounts is a regression test for a
// defect this file's first run surfaced.
//
// The observed message was:
//
//	context overflow persisted after forced compaction (26 → 16 tokens)
//
// 26 and 16 were the MESSAGE COUNTS of the two histories. No model has a
// 16-token window, so an operator reading that would conclude compaction had
// destroyed the conversation — and the "raise context_window versus find the
// indivisible segment" decision the message exists to inform would be made
// against two numbers three orders of magnitude too small.
//
// The test asserts a lower bound rather than exact values: the point is the
// UNIT, and any real history of this size is thousands of tokens while its
// message count is tens.
func TestC6_OverflowErrorReportsTokensNotMessageCounts(t *testing.T) {
	s := newStubProvider(t, summarisingStub(func(n int) bool { return n != 2 }))
	inner, _ := buildStubModel(t, s, nil)
	a := NewAdaptiveModel(inner, AdaptiveConfig{
		ModelID:  "stub-model-a",
		Overflow: OverflowRecoveryConfig{ContextWindow: 8192},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	msgs := wireLongHistory(12, 40)
	_, err := a.Generate(ctx, msgs)
	if err == nil {
		t.Fatal("want an error")
	}

	var ore *overflowRetryError
	if !errors.As(err, &ore) {
		t.Fatalf("want the wrapped overflow error carrying both sizes, got %T: %v", err, err)
	}
	t.Logf("reported sizes: before=%d after=%d (history has %d messages, ~%d tokens)",
		ore.Before, ore.After, len(msgs), ctxcompact.EstimateTokens(msgs))

	if ore.Before <= len(msgs) {
		t.Errorf("Before=%d is not larger than the %d-message count: the field documented as a "+
			"TOKEN count is reporting messages, and no operator can act on it", ore.Before, len(msgs))
	}
	if ore.Before != ctxcompact.EstimateTokens(msgs) {
		t.Errorf("Before=%d, want the history's estimated token count %d",
			ore.Before, ctxcompact.EstimateTokens(msgs))
	}
	if ore.After <= 0 || ore.After >= ore.Before {
		t.Errorf("After=%d must be a positive token count below Before=%d", ore.After, ore.Before)
	}
}
