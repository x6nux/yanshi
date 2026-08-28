package orchestrator

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	einollm "github.com/x6nux/yanshi/internal/llm/eino"
)

// capturedSystemPrompt returns the content of the system message the fake model
// last received, failing the test when there is none.
//
// Reading the prompt off the MODEL's input, rather than off o.instruction, is
// the whole point: the instruction field is what the runner was CONFIGURED
// with, and everything this file tests is about what happens between that field
// and the message the provider actually sees.
func capturedSystemPrompt(t *testing.T, fm *einollm.FakeModel) string {
	t.Helper()
	for _, m := range fm.ReceivedMessages {
		if m != nil && m.Role == schema.System {
			return m.Content
		}
	}
	t.Fatalf("no system message reached the model; got %d messages", len(fm.ReceivedMessages))
	return ""
}

// runOneTurn drives a full turn through the ordinary per-turn entry point and
// drains it, so BeforeAgent runs exactly the way a WS turn makes it run.
func runOneTurn(o *Orchestrator, opts TurnOpts) {
	drainAll(o.EventsWithHistoryOpts(context.Background(),
		[]*schema.Message{schema.UserMessage("hi")}, opts))
}

// splitPrompt cuts a captured system prompt into its static half and its
// volatile half at the per-turn section header.
func splitPrompt(t *testing.T, prompt string) (static, volatile string) {
	t.Helper()
	static, volatile, found := strings.Cut(prompt, volatileSectionHeader)
	require.True(t, found,
		"system prompt carries no %q block, so nothing is re-rendered per turn:\n%s",
		strings.TrimSpace(volatileSectionHeader), prompt)
	return static, volatile
}

// TestSystemPrompt_SendsOnlyChangedSections pins what "only the changed
// sections" can mean in this repository.
//
// It cannot mean an incremental transmission: adk renders the instruction into
// one whole schema.SystemMessage per call and both transports rebuild the
// request from scratch, so there is no receiver holding a document to patch
// (see sysprompt.go). What it does mean is that only the VOLATILE section is
// recomputed between turns while the static half is reused byte-for-byte —
// which is also the only property a provider's prefix cache can exploit.
//
// The three assertions are chosen to be jointly tight:
//
//   - the volatile halves of two turns must DIFFER, ruling out a prompt that is
//     simply frozen at construction (the pre-change behaviour, which would
//     satisfy the byte-identity check on its own);
//   - the static halves must be byte-IDENTICAL, which is the "only the changed
//     section moved" claim;
//   - and the static half must still describe the environment as it was at
//     New() even after that environment changes underneath the process, which
//     is what fails if someone later moves buildEnvInfo's dozen-odd probe
//     subprocesses into the per-turn path. Byte-identity alone would not catch
//     that: re-probing an unchanged machine yields the same bytes.
func TestSystemPrompt_SendsOnlyChangedSections(t *testing.T) {
	t.Setenv("SHELL", "/bin/shell-at-construction")

	fm := einollm.NewFakeModel([]string{"ok"}, nil)
	fm.RecordMessages = true
	fm.Repeat = true
	o, err := New(Config{Model: fm, Instruction: "BASE"})
	require.NoError(t, err)

	runOneTurn(o, TurnOpts{ModelID: "alpha"})
	firstStatic, firstVolatile := splitPrompt(t, capturedSystemPrompt(t, fm))

	// Change the machine out from under the running orchestrator. A per-turn
	// re-probe would pick this up; a section rendered once at New() cannot.
	t.Setenv("SHELL", "/bin/shell-after-construction")

	runOneTurn(o, TurnOpts{ModelID: "beta"})
	secondStatic, secondVolatile := splitPrompt(t, capturedSystemPrompt(t, fm))

	assert.NotEqual(t, firstVolatile, secondVolatile,
		"the volatile section did not change between turns: the prompt is baked, not re-rendered")
	assert.Equal(t, firstStatic, secondStatic,
		"the static section changed between turns: a prefix cache is invalidated for nothing")
	assert.Contains(t, secondStatic, "/bin/shell-at-construction",
		"the static section must keep its New()-time snapshot")
	assert.NotContains(t, secondStatic, "/bin/shell-after-construction",
		"the static section was re-probed per turn: that is a dozen subprocesses per model call")
}

// TestSystemPrompt_ModelSwitchVisibleWithinTurn is the positive probe on the
// runners cache.
//
// It asserts on the messages the MODEL received, not on a field or on a
// FlushRunners call count: the failure this guards against is precisely one
// where every field is updated and the model is still handed a prompt rendered
// for the model it was switched away from.
//
// Turn 3 is the part that matters. It leaves TurnOpts.Model nil, so
// EventsWithHistoryOpts falls back to o.rawModel and runnerFor returns the
// runner it built and memoised on turn 1 — the SAME runnerCacheKey, whose agent
// was constructed with turn 1's instruction. That combination is not contrived:
// connSession.selectModel returns nil whenever the selected name is absent from
// the registry (every --fake-model run, and any registry miss), while
// displayModel still reports the name the user picked, so a real /model switch
// routinely varies ModelID with the model object held fixed.
func TestSystemPrompt_ModelSwitchVisibleWithinTurn(t *testing.T) {
	fmA := einollm.NewFakeModel([]string{"ok"}, nil)
	fmA.RecordMessages = true
	fmA.Repeat = true
	o, err := New(Config{Model: fmA, Instruction: "BASE"})
	require.NoError(t, err)

	runOneTurn(o, TurnOpts{ModelID: "alpha"})
	assert.Contains(t, capturedSystemPrompt(t, fmA), "Model: alpha")

	// /model switch to a different provider instance.
	fmB := einollm.NewFakeModel([]string{"ok"}, nil)
	fmB.RecordMessages = true
	fmB.Repeat = true
	runOneTurn(o, TurnOpts{Model: fmB, ModelID: "beta"})
	promptB := capturedSystemPrompt(t, fmB)
	assert.Contains(t, promptB, "Model: beta")
	assert.NotContains(t, promptB, "Model: alpha",
		"the new model was told it is the old one")

	// /model switch that lands back on the memoised runner from turn 1.
	runOneTurn(o, TurnOpts{ModelID: "gamma"})
	promptC := capturedSystemPrompt(t, fmA)
	assert.Contains(t, promptC, "Model: gamma",
		"the cached runner served turn 1's instruction: the switch never reached the model")
	assert.NotContains(t, promptC, "Model: alpha")
}

// TestRenderVolatileSections_OmitsModelWhenUnselected pins the entry points
// that have no model name to report (Query, Events, sub-agent turns). They must
// still get the date, and must not get a Model line naming something the turn
// is not running against.
func TestRenderVolatileSections_OmitsModelWhenUnselected(t *testing.T) {
	at := time.Date(2026, 8, 28, 13, 45, 0, 0, time.UTC)

	assert.Equal(t, "Date: 2026-08-28", renderVolatileSections(at, ""))
	assert.Equal(t, "Date: 2026-08-28\nModel: gpt-5", renderVolatileSections(at, "gpt-5"))
}

// TestSystemPrompt_DateIsNotFrozenAtConstruction closes the other half of the
// section split. The date used to be the first line of buildEnvInfo, rendered
// once in New(), so a server still up the next morning kept asserting the day
// it booted on — the model has no other clock, so it had no way to notice.
//
// buildEnvInfo must therefore NOT carry a date any more, and the volatile block
// must.
func TestSystemPrompt_DateIsNotFrozenAtConstruction(t *testing.T) {
	assert.NotContains(t, buildEnvInfo(), "Date:",
		"the date is back in the static snapshot, where it freezes at process start")

	fm := einollm.NewFakeModel([]string{"ok"}, nil)
	fm.RecordMessages = true
	o, err := New(Config{Model: fm, Instruction: "BASE"})
	require.NoError(t, err)

	runOneTurn(o, TurnOpts{ModelID: "alpha"})
	_, volatile := splitPrompt(t, capturedSystemPrompt(t, fm))
	assert.Contains(t, volatile, "Date: "+time.Now().Format("2006-01-02"))
}
