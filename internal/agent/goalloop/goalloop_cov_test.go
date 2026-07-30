package goalloop

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/guard"
)

// errJudge is a Judge stub that always errors (for the Run judge-error path).
type errJudge struct{ err error }

func (j errJudge) Judge(_ context.Context, _ []EvalVerdict) (Decision, error) {
	return Decision{}, j.err
}

// errConfirmer is a Confirmer stub that always errors.
type errConfirmer struct{ err error }

func (c errConfirmer) Confirm(_ context.Context, _ string) (bool, error) { return false, c.err }

// goalClassifier always classifies as IntentGoalImpl.
type goalClassifier struct{}

func (goalClassifier) Classify(_ context.Context, _ string) (Intent, error) {
	return IntentGoalImpl, nil
}

// TestCov_Run_PlannerError covers the planner-error abort branch.
func TestCov_Run_PlannerError(t *testing.T) {
	loop := New(Config{
		Planner:     FakePlanner{Err: errors.New("plan boom")},
		Implementer: &FakeImplementer{Result: "x"},
		Evaluators:  []Evaluator{&CounterEvaluator{passAt: 1}},
		Judge:       AggregateJudge{},
		Budget:      Budget{MaxTokens: 1_000_000, MaxIterations: 5},
	})
	_, err := loop.Run(context.Background(), Goal{Text: "g"}, nil)
	require.Error(t, err)
}

// TestCov_Run_EvaluatorError covers the evaluator-error → error-verdict path
// (the loop tolerates one evaluator failing and synthesises a fail verdict).
func TestCov_Run_EvaluatorError(t *testing.T) {
	loop := New(Config{
		Planner:     FakePlanner{Steps: []string{"s"}, Tests: []AcceptanceTest{{Name: "t", Command: "true"}}},
		Implementer: &FakeImplementer{Result: "x"},
		Evaluators:  []Evaluator{&FakeEvaluator{Err: errors.New("eval boom")}},
		Judge:       AggregateJudge{},
		Budget:      Budget{MaxTokens: 1_000_000, MaxIterations: 1},
	})
	dec, err := loop.Run(context.Background(), Goal{Text: "g"}, nil)
	require.NoError(t, err)
	assert.False(t, dec.Complete, "error verdict → not complete")
}

// TestCov_Run_JudgeError covers the judge-error abort branch.
func TestCov_Run_JudgeError(t *testing.T) {
	loop := New(Config{
		Planner:     FakePlanner{Steps: []string{"s"}, Tests: []AcceptanceTest{{Name: "t", Command: "true"}}},
		Implementer: &FakeImplementer{Result: "x"},
		Evaluators:  []Evaluator{&FakeEvaluator{Verdict: EvalVerdict{Pass: true}}},
		Judge:       errJudge{err: errors.New("judge boom")},
		Budget:      Budget{MaxTokens: 1_000_000, MaxIterations: 5},
	})
	_, err := loop.Run(context.Background(), Goal{Text: "g"}, nil)
	require.Error(t, err)
}

// TestCov_Trigger_ConfirmYes covers the OSConfirmer "y"/"yes" → true path.
func TestCov_Trigger_ConfirmYes(t *testing.T) {
	c := OSConfirmer{in: strings.NewReader("yes\n")}
	got, err := c.Confirm(context.Background(), "ok?")
	require.NoError(t, err)
	assert.True(t, got)

	c2 := OSConfirmer{in: strings.NewReader("y\n")}
	got2, _ := c2.Confirm(context.Background(), "ok?")
	assert.True(t, got2)
}

// TestCov_Trigger_DecideConfirmError covers the Decide branch where Confirm
// errors (enter=false, reason "auto+confirm").
func TestCov_Trigger_DecideConfirmError(t *testing.T) {
	tr := NewTrigger(goalClassifier{}, errConfirmer{err: errors.New("no tty")})
	got, reason := tr.Decide(context.Background(), "anything")
	assert.False(t, got)
	assert.Equal(t, "auto+confirm", reason)
}

// TestCov_ResolveProfile_Explicit covers the profileSet=true branch (caller
// supplied an explicit profile, used verbatim).
func TestCov_ResolveProfile_Explicit(t *testing.T) {
	in := guard.PermissionProfile{}
	assert.Equal(t, in, resolveProfile(in, true, "/wt"), "explicit profile returned unchanged")
}

// TestCov_Trigger_ConfirmNilReader covers the r==nil branch at trigger.go:90-92
// by temporarily replacing os.Stdin with a pipe.
func TestCov_Trigger_ConfirmNilReader(t *testing.T) {
	oldStdin := os.Stdin
	t.Cleanup(func() { os.Stdin = oldStdin })
	r, w, err := os.Pipe()
	if err != nil {
		t.Skipf("pipe: %v", err)
	}
	_, _ = w.Write([]byte("y\n"))
	_ = w.Close()
	os.Stdin = r

	c := OSConfirmer{} // in is nil → falls through to os.Stdin
	got, err := c.Confirm(context.Background(), "ok?")
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if !got {
		t.Fatal("expected true from 'y'")
	}
}
