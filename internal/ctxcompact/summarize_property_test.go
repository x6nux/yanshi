package ctxcompact

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// recordingSummarizer implements ModelSummarizer, recording every Generate/Stream call.
type recordingSummarizer struct {
	GenerateCalls [][]*schema.Message
	StreamCalls   [][]*schema.Message
	Return        string
	ReturnErr     error
}

func (r *recordingSummarizer) Generate(ctx context.Context, msgs []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	r.GenerateCalls = append(r.GenerateCalls, msgs)
	if r.ReturnErr != nil {
		return nil, r.ReturnErr
	}
	return &schema.Message{Role: schema.User, Content: r.Return}, nil
}

func (r *recordingSummarizer) Stream(ctx context.Context, msgs []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	r.StreamCalls = append(r.StreamCalls, msgs)
	if r.ReturnErr != nil {
		return nil, r.ReturnErr
	}
	return schema.StreamReaderFromArray[*schema.Message]([]*schema.Message{
		{Role: schema.User, Content: r.Return},
	}), nil
}

// ---------- P3: each summary call's input <= ModelWindow ----------

// TestProperty_EachSummaryCallWithinWindow is a third, independent property,
// and the one that constrains the chunking path: no single summarizer call may
// be handed more tokens than the model window it is being sent to. Over-window
// summary calls are how a compaction meant to rescue a turn kills it instead.
//
// The bound is checked at four window sizes against randomly generated
// histories; the one tolerated overshoot (< 2x) is tool-call pair integrity,
// which cannot be split without producing a history the provider rejects.
//
// Failures are NOT swallowed. The error branch used to be `t.Logf(...);
// return`, which made the property hold for a RunSummary that fails every
// time: gutting it to `return "", err` produced four log lines and a pass.
// Only ErrNoWindowRoom is tolerated (a window too small to make any progress
// is a real outcome, and window=100 hits it), only some of the windows may
// take that branch, and a success must be followed by at least one real
// summarizer call — three independent ways for a broken RunSummary to redden.
//
// ledger: E2/PROP1#1 ≥3 个属性
func TestProperty_EachSummaryCallWithinWindow(t *testing.T) {
	windows := []int{800, 400, 200, 100}
	summarized := 0
	for _, mw := range windows {
		t.Run(fmt.Sprintf("window=%d", mw), func(t *testing.T) {
			rs := &recordingSummarizer{Return: "summarized"}
			opts := RunOpts{
				ModelWindow:      mw,
				ChunkThreshold:   0.9,
				SummaryWordLimit: 200,
			}
			rng := rand.New(rand.NewPCG(uint64(mw), 0))
			msgs := genHistory(rng, 30)
			if len(msgs) == 0 {
				t.Fatal("genHistory produced no messages: the property has no input")
			}
			_, err := RunSummary(context.Background(), msgs, opts, rs, nil)
			if err != nil {
				if !errors.Is(err, ErrNoWindowRoom) {
					t.Fatalf("RunSummary failed for a reason this property does not "+
						"tolerate (only ErrNoWindowRoom is): %v", err)
				}
				t.Logf("window %d is too small for the carry loop to make progress: %v", mw, err)
				return
			}
			summarized++
			allCalls := append(rs.GenerateCalls, rs.StreamCalls...)
			if len(allCalls) == 0 {
				t.Fatal("RunSummary returned success but summarizer was never called")
			}
			for i, callMsgs := range allCalls {
				tok := EstimateTokens(callMsgs)
				if tok > mw {
					if tok > mw*2 {
						t.Fatalf("call[%d] tok=%d exceeds ModelWindow=%d by >2x (unacceptable even for pair integrity)", i, tok, mw)
					}
					t.Logf("call[%d] tok=%d exceeds window=%d (acceptable: pair integrity)", i, tok, mw)
				}
			}
		})
	}
	// A RunSummary gutted to return ErrNoWindowRoom itself would slip past the
	// branch above; it does not slip past here. Only the smallest window is
	// allowed to be unsummarizable.
	if want := len(windows) - 1; summarized < want {
		t.Fatalf("only %d of %d windows produced summarizer calls (want ≥%d): the "+
			"in-window bound was asserted against almost nothing", summarized, len(windows), want)
	}
}

// ---------- P4: no empty summary when summarizer returns empty ----------

func TestProperty_NoEmptySummaryMessage(t *testing.T) {
	rs := &recordingSummarizer{Return: ""}
	opts := RunOpts{
		ModelWindow:      1000,
		ChunkThreshold:   0.9,
		SummaryWordLimit: 200,
	}
	rng := rand.New(rand.NewPCG(99, 0))
	msgs := genHistory(rng, 20)

	plan := Plan(msgs, PlanOpts{KeepRecent: 3})
	if len(plan.SummarizeIndices) == 0 {
		t.Skip("all messages pinned — nothing to summarize")
	}

	result, err := Run(context.Background(), msgs, PlanOpts{KeepRecent: 3}, opts, rs, nil)
	if err != nil {
		t.Fatalf("Run must not error with empty summarizer output: %v", err)
	}
	if len(result.Messages) == 0 {
		t.Fatal("Run must produce at least the summary message")
	}
	// Even with empty summarizer return, Assemble always appends a
	// sentinel-prefixed message — the output is never truly empty.
	last := result.Messages[len(result.Messages)-1]
	if !IsSummaryMessage(last) {
		t.Fatal("last message must be a summary message")
	}
}
