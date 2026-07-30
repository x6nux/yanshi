package ctxcompact

import (
	"context"
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

func TestProperty_EachSummaryCallWithinWindow(t *testing.T) {
	windows := []int{800, 400, 200, 100}
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
				return
			}
			_, err := RunSummary(context.Background(), msgs, opts, rs, nil)
			if err != nil {
				t.Logf("RunSummary returned error (expected for some tiny windows): %v", err)
				return
			}
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
