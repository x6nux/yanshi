package orchestrator

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/adk"
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
//
// Both turns pin the SAME model under the SAME id, so runnerCacheKey is
// identical and the second turn is served by the runner the first one memoised.
// The clock is what moves. That combination is the point: it is the only way to
// observe that a cached runner re-renders at all, and it is the real defect —
// a server that stays up overnight.
func TestSystemPrompt_SendsOnlyChangedSections(t *testing.T) {
	t.Setenv("SHELL", "/bin/shell-at-construction")

	day := time.Date(2026, 8, 28, 23, 0, 0, 0, time.UTC)
	restore := sysPromptNow
	sysPromptNow = func() time.Time { return day }
	t.Cleanup(func() { sysPromptNow = restore })

	fm := einollm.NewFakeModel([]string{"ok"}, nil)
	fm.RecordMessages = true
	fm.Repeat = true
	o, err := New(Config{Model: fm, Instruction: "BASE"})
	require.NoError(t, err)

	runOneTurn(o, TurnOpts{Model: fm, ModelID: "alpha"})
	firstStatic, firstVolatile := splitPrompt(t, capturedSystemPrompt(t, fm))

	// Change the machine out from under the running orchestrator, and cross
	// midnight. A per-turn re-probe would pick the first up; a section rendered
	// once at New() cannot. The date must move either way.
	t.Setenv("SHELL", "/bin/shell-after-construction")
	sysPromptNow = func() time.Time { return day.Add(2 * time.Hour) }

	runOneTurn(o, TurnOpts{Model: fm, ModelID: "alpha"})
	secondStatic, secondVolatile := splitPrompt(t, capturedSystemPrompt(t, fm))

	assert.NotEqual(t, firstVolatile, secondVolatile,
		"the volatile section did not change between turns: the memoised runner is "+
			"serving a prompt baked when it was built")
	assert.Contains(t, secondVolatile, "Date: 2026-08-29")
	assert.Equal(t, firstStatic, secondStatic,
		"the static section changed between turns: a prefix cache is invalidated for nothing")
	assert.Contains(t, secondStatic, "/bin/shell-at-construction",
		"the static section must keep its New()-time snapshot")
	assert.NotContains(t, secondStatic, "/bin/shell-after-construction",
		"the static section was re-probed per turn: that is a dozen subprocesses per model call")
}

// TestSystemPrompt_ModelSwitchVisibleWithinTurn walks the three states a
// session moves through, asserting on the messages the MODEL received rather
// than on a field: the failure this guards against is precisely one where every
// field is updated and the model is still handed a prompt written for the model
// it was switched away from.
//
// Turn 1 is the COMMON case and the one that must stay silent. The user has not
// run /model, so connSession.selectModel returns nil and the turn executes on
// o.rawModel — einollm.ResilientModel over the config-order failover chain.
// ModelID is still populated (displayModel falls back to the first name in
// SORTED order), and naming that would tell the model it is a provider that is
// merely alphabetically first. (set_model only accepts names the registry has,
// so a mismatched cs.model is not how nil arises here; "never switched" is.)
//
// Turn 3 is the cache probe. It pins fmA under "alpha", which is the exact
// runnerCacheKey turn 1 created and memoised — and turn 1's instruction carried
// no Model line at all. A prompt baked into the runner would still have none.
func TestSystemPrompt_ModelSwitchVisibleWithinTurn(t *testing.T) {
	fmA := einollm.NewFakeModel([]string{"ok"}, nil)
	fmA.RecordMessages = true
	fmA.Repeat = true
	o, err := New(Config{Model: fmA, Instruction: "BASE"})
	require.NoError(t, err)

	// 1. No /model yet: running on the failover chain, so no model is named.
	runOneTurn(o, TurnOpts{ModelID: "alpha"})
	promptA := capturedSystemPrompt(t, fmA)
	assert.NotContains(t, promptA, "Model:",
		"an unpinned turn runs on the failover chain; naming the sorted-first model "+
			"tells the model it is something it is not")
	assert.Contains(t, promptA, "Date: ", "the volatile block must still be there")

	// 2. /model to a different provider instance.
	fmB := einollm.NewFakeModel([]string{"ok"}, nil)
	fmB.RecordMessages = true
	fmB.Repeat = true
	runOneTurn(o, TurnOpts{Model: fmB, ModelID: "beta"})
	promptB := capturedSystemPrompt(t, fmB)
	assert.Contains(t, promptB, "Model: beta")

	// 3. /model onto the key turn 1 already memoised.
	runOneTurn(o, TurnOpts{Model: fmA, ModelID: "alpha"})
	assert.Contains(t, capturedSystemPrompt(t, fmA), "Model: alpha",
		"the cached runner served turn 1's instruction: the switch never reached the model")
}

// TestRenderVolatileSections_OmitsModelWhenUnselected pins the rendering rule
// itself. Every turn gets the date; only a turn that pinned a model gets a name.
func TestRenderVolatileSections_OmitsModelWhenUnselected(t *testing.T) {
	at := time.Date(2026, 8, 28, 13, 45, 0, 0, time.UTC)

	assert.Equal(t, "Date: 2026-08-28", renderVolatileSections(at, ""))
	assert.Equal(t, "Date: 2026-08-28\nModel: gpt-5", renderVolatileSections(at, "gpt-5"))
}

// TestSystemPromptRefresherIsTheOnlyInstructionWriter machine-checks the claim
// orchestratorMiddlewares makes about its own ordering: the refresher's position
// is free because nothing else in the stack writes the instruction. That is an
// invariant no compiler enforces — a second BeforeAgent implementer would make
// the comment quietly false and the order suddenly significant.
//
// Behavioural rather than an AST scan: what matters is not who declares the
// method (every entry inherits a no-op one by embedding) but who CHANGES the
// instruction. Feeding each handler a known value and counting the ones that
// alter it measures exactly that, and doubles as a wiring check — a count of
// zero means the refresher fell out of the slice.
func TestSystemPromptRefresherIsTheOnlyInstructionWriter(t *testing.T) {
	writers := 0
	for _, h := range orchestratorMiddlewares() {
		_, got, err := h.BeforeAgent(context.Background(),
			&adk.ChatModelAgentContext{Instruction: "STATIC"})
		require.NoError(t, err)
		require.NotNil(t, got)
		if got.Instruction != "STATIC" {
			writers++
		}
	}
	require.Equal(t, 1, writers,
		"exactly one middleware may rewrite the instruction; 0 means the refresher is "+
			"unwired, more than 1 means the order this stack documents as free is not")
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
