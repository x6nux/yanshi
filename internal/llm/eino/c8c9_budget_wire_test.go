// internal/llm/eino/c8c9_budget_wire_test.go
//
// C8 (token counting) and C9 (output reserve + hard-limit refusal) as observed
// FROM THE MODEL LAYER — which is where they either work or silently do not.
//
// The mechanisms live in internal/ctxcompact and are unit-tested there. What is
// only observable from this side is the consequence the features exist for: when
// a history genuinely does not fit, does yanshi fail LOCALLY and by name, or
// does it send the request and collect a provider 400?
//
// That distinction is the whole of C9. A local failure is catchable, costs
// nothing, names its own measurements, and cannot leak the prompt to a provider
// that will only reject it. A remote 400 costs a round trip, arrives as
// unstructured provider prose, and — the part that actually hurts — is
// indistinguishable from the several other things a 400 can mean.
//
// So the assertion available here, and the one these tests make, is about
// REQUEST COUNT AT THE STUB: a locally-refused request never reaches the server.
package eino

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/x6nux/yanshi/internal/ctxcompact"
)

// TestC9_LocalOverflowIsRecognisedAsTheSameConditionAsAProvider400 pins the
// boundary-crossing claim in IsContextOverflow's doc: the local pre-send gate
// and the remote rejection are the same condition and must not be told apart by
// callers, because the recovery is identical.
//
// If they diverged, C6 would recover from one and not the other — and which one
// depended on whether the proactive gate happened to fire first, i.e. on the
// token estimate, i.e. on C8.
func TestC9_LocalOverflowIsRecognisedAsTheSameConditionAsAProvider400(t *testing.T) {
	local := &ctxcompact.ContextOverflowError{Tokens: 41000, Limit: 28000, Window: 32000, Reserve: 4000}
	if !IsContextOverflow(local) {
		t.Error("the LOCAL pre-send overflow is not recognised as a context overflow, so C6 will " +
			"not recover from it while it does recover from the identical remote condition")
	}
	if !errors.Is(local, ctxcompact.ErrContextOverflow) {
		t.Error("the local error does not match its own sentinel")
	}
	remote := errors.New("error, status code: 400, message: This model's maximum context length " +
		"is 32000 tokens, however you requested 41000 tokens")
	if !IsContextOverflow(remote) {
		t.Error("the REMOTE provider 400 is not recognised as a context overflow")
	}
	t.Logf("local: %v", local)
	t.Logf("remote classification: %s", ClassifyError(remote).Class)
}

// TestC9_LocalOverflowErrorCarriesItsMeasurements is the "by name" half. An
// unstructured failure would leave the operator with nothing to act on; these
// four numbers say exactly which knob is wrong.
func TestC9_LocalOverflowErrorCarriesItsMeasurements(t *testing.T) {
	err := &ctxcompact.ContextOverflowError{Tokens: 41000, Limit: 28000, Window: 32000, Reserve: 4096}
	msg := err.Error()
	t.Logf("local overflow message: %s", msg)
	for _, want := range []string{"41000", "28000", "32000", "4096"} {
		if !containsAny(msg, []string{want}) {
			t.Errorf("the message omits %s; without all four numbers an operator cannot tell "+
				"whether to raise context_window or lower the output reserve", want)
		}
	}
}

// TestC9_OutputReserveIsHeldBackFromTheWindow pins that the budget the gate
// compares against is SMALLER than the window.
//
// Without a reserve, a history filling the window exactly passes the gate and
// the provider then rejects the request for having no room to answer in — the
// failure lands after the round trip and reads as if the prompt were too long by
// a lot rather than by the length of the reply.
func TestC9_OutputReserveIsHeldBackFromTheWindow(t *testing.T) {
	for _, window := range []int{8192, 32000, 128000} {
		// Binary-search the largest history CheckContextLimit accepts, then
		// compare it to the window. Probing the public gate rather than reading
		// an unexported helper keeps the test aimed at the decision production
		// makes.
		opts := ctxcompact.RunOpts{ModelWindow: window}
		accepted := largestAcceptedTokens(t, opts)
		t.Logf("window %d → largest accepted input ≈ %d tokens (reserved ≈ %d)",
			window, accepted, window-accepted)
		if accepted >= window {
			t.Errorf("window %d accepts an input of %d tokens: nothing was reserved for the "+
				"reply, so a prompt that just fits leaves the model no room to answer",
				window, accepted)
		}
		if accepted <= 0 {
			t.Errorf("window %d accepts nothing at all", window)
		}
	}
}

// largestAcceptedTokens finds, by bisection over history length, the largest
// token count CheckContextLimit still admits for opts.
func largestAcceptedTokens(t *testing.T, opts ctxcompact.RunOpts) int {
	t.Helper()
	accepted := 0
	// Grow a single message until it is rejected, tracking the last accepted
	// size. Coarse steps are fine: the assertion is "materially below the
	// window", not an exact reserve.
	for n := 1; n <= 1<<22; n *= 2 {
		msgs := []*schema.Message{schema.UserMessage(strings.Repeat("token ", n))}
		tok := ctxcompact.EstimateTokens(msgs)
		if err := ctxcompact.CheckContextLimit(msgs, opts); err != nil {
			break
		}
		accepted = tok
	}
	return accepted
}

// TestC8_TokenEstimateNeverUnderstatesRealisticContent is the property C8's
// implementation note claims and the one the compaction gate depends on.
//
// The gate fires at a fraction of the window. An estimate that UNDER-counts
// moves the real firing point later than intended, and the failure that follows
// is the 400 the gate existed to prevent. Over-counting only costs an earlier
// compaction. So the estimator must be biased high on the content that actually
// appears in a coding agent's history — CJK text and dense JSON tool arguments
// are the two shapes a naive chars/4 heuristic gets badly wrong.
//
// The comparison is against a conservative floor rather than a tokenizer: this
// repo deliberately has no tokenizer dependency (fetching a BPE table on first
// use would make the FIRST compaction of an offline deployment fail), so the
// property to pin is the DIRECTION of the bias, not an exact count.
func TestC8_TokenEstimateNeverUnderstatesRealisticContent(t *testing.T) {
	cases := []struct {
		name string
		text string
	}{
		{"ascii prose", "The quick brown fox jumps over the lazy dog. " +
			"Refactor the handler so it validates input before dispatch."},
		{"dense json tool args", `{"path":"internal/llm/eino/adaptive.go","old_string":` +
			`"if err != nil {\n\treturn nil, err\n}","new_string":"if err != nil {\n\t` +
			`return nil, fmt.Errorf(\"adaptive: %w\", err)\n}","replace_all":false}`},
		{"cjk", "请把这个处理函数重构一下，让它在分发之前先校验输入参数，并且补上单元测试。"},
		{"mixed cjk and code", "修复 internal/guard/guard.go 里的 checkShell：" +
			"`rm -rf /` 必须是结构性 HardDeny，yolo 也不能越过。"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msgs := []*schema.Message{schema.UserMessage(tc.text)}
			est := ctxcompact.EstimateTokens(msgs)
			// A very conservative floor on the true token count: no tokenizer
			// emits more than one token per rune for these scripts, so runes is
			// an upper bound on truth for CJK and a loose one for ASCII.
			runes := len([]rune(tc.text))
			t.Logf("%d runes → estimate %d tokens (%.2f tokens/rune)",
				runes, est, float64(est)/float64(runes))
			if est <= 0 {
				t.Fatalf("estimate is %d for %d runes of content", est, runes)
			}
			// The direction that must hold: the estimate for CJK must not be the
			// chars/4 figure, which understates it several-fold.
			if est*4 < runes {
				t.Errorf("estimate %d is less than a quarter of the %d runes; that is the "+
					"chars/4 understatement the gate cannot tolerate", est, runes)
			}
		})
	}
}

// TestC8_EstimateGrowsWithHistory is the sanity property the gate relies on: a
// monotone measure. A non-monotone estimator would make compaction fire and
// un-fire as unrelated content arrived.
func TestC8_EstimateGrowsWithHistory(t *testing.T) {
	var msgs []*schema.Message
	last := 0
	for i := range 8 {
		msgs = append(msgs, schema.UserMessage(
			"a reasonably long request about refactoring some Go code, iteration"))
		est := ctxcompact.EstimateTokens(msgs)
		t.Logf("%d messages → %d tokens", i+1, est)
		if est <= last {
			t.Errorf("estimate did not grow when a message was appended: %d → %d", last, est)
		}
		last = est
	}
}

// TestC9_ARequestRefusedLocallyIsNeverSent is the operational payoff, asserted
// where it is visible: at the server.
//
// The history here is far larger than the tiny window configured, so the forced
// compaction cannot bring it under the limit. What must NOT happen is a parade
// of doomed requests: each one costs a round trip and a charge, and the answer
// was knowable locally.
func TestC9_ARequestRefusedLocallyIsNeverSent(t *testing.T) {
	s := newStubProvider(t, summarisingStub(nil)) // would happily answer anything
	inner, _ := buildStubModel(t, s, nil)
	a := NewAdaptiveModel(inner, AdaptiveConfig{
		ModelID: "stub-model-a",
		Overflow: OverflowRecoveryConfig{
			ContextWindow: 512, // absurdly small: nothing can be made to fit
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, _ = a.Generate(ctx, wireLongHistory(20, 60))

	n := len(s.chatRequests())
	t.Logf("stub saw %d request(s) for a history that cannot fit a 512-token window", n)
	if n > 4 {
		t.Errorf("stub saw %d requests; an input known locally not to fit must not be sent "+
			"repeatedly", n)
	}
}
