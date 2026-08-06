package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestGoalTiersT3AndT4ProduceARealDecision is the clause the ledger recorded as
// partially covered.
//
// TestRunGoalRealPathLoopSetup drives `-tier t3` and then discards the exit
// code (`_ = code`), with a comment saying "any non-timeout result means the
// setup path ran cleanly". It can therefore only fail by hanging for 20
// seconds: the loop can return anything at all — success, failure, a panic
// recovered into an error — and the test passes. That is a coverage tick, not
// evidence that a tier "produces a real result".
//
// The self-contained path (--fake-model: FakePlanner + FakeImplementer that
// fails once + a counter evaluator that passes on the second pass) is what
// makes T3/T4 assertable without an external agent CLI, and it exercises the
// same Loop.Run that the real path does — only the components differ.
//
// ledger: M1/G03#1 每个 tier（T0–T4）都产生真实结果
func TestGoalTiersT3AndT4ProduceARealDecision(t *testing.T) {
	for _, tier := range []string{"t3", "t4"} {
		t.Run(tier, func(t *testing.T) {
			cfgPath := writeServeConfig(t)

			type outcome struct {
				out  string
				code int
			}
			done := make(chan outcome, 1)
			go func() {
				var code int
				out := captureStdout(t, func() {
					code = runGoal([]string{
						"-config", cfgPath, "-fake-model",
						"-tier", tier, "-max-iters", "4", "-goal", "make it work",
					})
				})
				done <- outcome{out, code}
			}()

			select {
			case got := <-done:
				assert.Equal(t, exitOK, got.code,
					"tier %s did not reach a complete decision; output:\n%s", tier, got.out)
				assert.Contains(t, got.out, "decision: complete=true",
					"tier %s printed no completed decision:\n%s", tier, got.out)
				// FakeImplementer fails once and the evaluator passes on the
				// second pass, so a run that completed in ONE iteration would
				// mean the loop is not iterating at all.
				assert.Contains(t, got.out, "[iter 2]",
					"tier %s finished without a second iteration, so the "+
						"plan-implement-evaluate-judge cycle did not actually cycle:\n%s",
					tier, got.out)
			case <-time.After(30 * time.Second):
				t.Fatalf("runGoal %s did not terminate", tier)
			}
		})
	}
}

// TestGoalT3WithoutAnAgentCLIFailsPredictably replaces the discarded exit code
// on the real path.
//
// The real T3 path needs an external agent CLI, which CI does not have, so the
// run must fail — but it must fail as a reported error rather than by hanging
// or by claiming success. Asserting the exit code is what distinguishes those;
// `_ = code` could not.
//
// ledger: M1/G03#1 每个 tier（T0–T4）都产生真实结果
func TestGoalT3WithoutAnAgentCLIFailsPredictably(t *testing.T) {
	cfgPath := writeServeConfig(t)
	done := make(chan int, 1)
	go func() {
		done <- runGoal([]string{
			"-config", cfgPath, "-tier", "t3", "-max-iters", "1", "-goal", "loop goal",
		})
	}()
	select {
	case code := <-done:
		assert.Equal(t, exitErr, code,
			"the real T3 path reported success without an agent CLI available; either it "+
				"never tried to implement anything, or it is calling a completed run what "+
				"is actually an unmet goal")
	case <-time.After(30 * time.Second):
		t.Fatal("runGoal t3 real path did not terminate")
	}
}
