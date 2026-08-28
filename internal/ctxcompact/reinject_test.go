package ctxcompact

import (
	"fmt"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// W-D-14 — the INITIAL CONTEXT across the two compaction paths.
//
// The acceptance this came from was written against codex, where the initial
// context (user instructions + environment) is a set of user-role rollout items
// that compaction drops, hence codex's "re-insert before the last user
// message". yanshi's initial context is not shaped like that, and the two
// paths do not even receive the same slice — which is the whole reason they
// must be verified separately rather than through one shared helper:
//
//   - MID-TURN (einollm.CompactingModel wraps the model, so it sees adk's
//     model input). adk's defaultGenModelInput PREPENDS the agent instruction
//     as a schema.System message, so msgs[0] is the initial context and is
//     inside the slice Plan/Assemble rewrite. MEASURED before the fix: nothing
//     in Plan pinned it, so the assembled history went to the provider with no
//     system message at all — the operator prompt, the tool guidance, the skill
//     meta-prompt and the environment block all gone for the rest of the turn.
//     It survived only by accident, when the prompt happened to contain a word
//     in shouldPin's error-marker list.
//
//   - PRE-TURN (the WS handler calls MaybeCompact on cs.history). cs.history
//     is only ever appended with user/assistant/tool messages, and
//     storeMessagesFor skips the system message on the way to the store because
//     it is regenerated from server state every turn. There is nothing to lose
//     and therefore nothing to re-inject: adk rebuilds the instruction on the
//     next call whatever compaction did.
//
// So the mid-turn fix is "keep it", not "re-insert it", and keeping it in place
// is strictly better than re-inserting a copy: this package has no source for
// the instruction text other than the copy already in the slice, and the pinned
// prefix is what a provider's prefix cache keys on. The pre-turn half needs no
// code — but it does need the guard below, because the obvious reading of
// "re-inject the initial context" produces an Assemble that SYNTHESISES a
// system message, which on the pre-turn path would arrive alongside the one adk
// prepends and recreate the double-system conflict (bug④) the summary-as-user
// decision exists to avoid.

// TestReinject_MidTurnKeepsInitialContextAtTheHead covers the mid-turn shape:
// the slice starts with the agent instruction, and compaction must hand it back.
func TestReinject_MidTurnKeepsInitialContextAtTheHead(t *testing.T) {
	instruction := "You are yanshi. Prefer fs_read over shell_run for reading files."
	// The shape adk hands CompactingModel.Generate: system first, then history.
	msgs := append([]*schema.Message{{Role: schema.System, Content: instruction}}, chatter(40)...)

	plan := Plan(msgs, PlanOpts{KeepRecent: 2})
	require.NotEmpty(t, plan.SummarizeIndices, "the fixture must actually be compactable")
	assert.Contains(t, plan.PinnedIndices, 0, "the initial context is pinned verbatim")

	out := Assemble(msgs, plan, "we discussed X")
	require.NotEmpty(t, out)
	assert.Equal(t, schema.System, out[0].Role, "and it comes back FIRST")
	assert.Equal(t, instruction, out[0].Content, "verbatim — not re-rendered, not summarized")
	assert.Same(t, msgs[0], out[0],
		"the same message pointer, so the prefix stays byte-stable for the provider's cache")

	// Exactly one: pinning must not become duplicating.
	systems := 0
	for _, m := range out {
		if m.Role == schema.System {
			systems++
		}
	}
	assert.Equal(t, 1, systems)

	// It must not be handed to the summarizer either. Summarizing the operator
	// prompt spends the summary budget describing configuration as if it were
	// conversation, and the result then competes with the real instruction.
	assert.NotContains(t, plan.SummarizeIndices, 0)
}

// TestReinject_MidTurnKeepsInitialContextEvenWhenPlanPinsNothingElse guards the
// route the accidental survival used to come through. A system prompt with no
// error word, no diff marker and no working-set path is the case the previous
// code dropped; a fixture whose prompt happens to say "failed" would pass
// against the unfixed code and prove nothing.
func TestReinject_MidTurnKeepsInitialContextEvenWhenPlanPinsNothingElse(t *testing.T) {
	msgs := append([]*schema.Message{{Role: schema.System, Content: "Be concise."}}, chatter(40)...)
	plan := Plan(msgs, PlanOpts{KeepRecent: 0})
	assert.Equal(t, []int{0}, plan.PinnedIndices,
		"with no tail to keep, the initial context is the ONLY thing pinned")
	assert.Equal(t, schema.System, Assemble(msgs, plan, "s")[0].Role)
}

// TestReinject_PreTurnCarriesNoInitialContextAndInventsNone covers the pre-turn
// shape: cs.history holds no system message, and compaction must not grow one.
//
// This is the "cleared, and re-injected next turn" half. The clearing is what
// the WS handler already does by replacing cs.history with the compacted
// window; the re-injection is adk rebuilding the instruction on the next call.
// What this package owes that arrangement is to stay out of it — an Assemble
// that synthesised a system message here would put a second one in front of the
// model on every subsequent turn.
func TestReinject_PreTurnCarriesNoInitialContextAndInventsNone(t *testing.T) {
	// The shape cs.history has: user/assistant only, never a system message.
	msgs := append(chatter(40), &schema.Message{Role: schema.User, Content: "what next?"})

	plan := Plan(msgs, PlanOpts{KeepRecent: 2})
	require.NotEmpty(t, plan.SummarizeIndices)

	out := AssembleWithMap(msgs, plan, "we discussed X", "spans 1-20")
	for i, m := range out {
		assert.NotEqual(t, schema.System, m.Role,
			"compaction must not invent an initial context at index %d", i)
	}
	// The summary is carried as a USER message for the same reason: it is what
	// keeps the compacted window from colliding with the instruction adk
	// prepends on the next turn.
	assert.Equal(t, schema.User, out[len(out)-1].Role)
	assert.True(t, IsSummaryMessage(out[len(out)-1]))
}

// TestReinject_RepeatedCompactionDoesNotAccumulate is the multi-round guard.
//
// Every defect this file and fragment_test.go fix is an ACCUMULATION defect,
// and a single-round test cannot see one: round 1 of the broken code produced a
// history that looked fine. The measured shape before the fix was linear growth
// — 1 eviction map after one round, 5 after five, 513 runes of map text growing
// to 3035 — because each round pinned the previous round's fragment and
// appended a fresh one.
//
// So the property is BOUNDEDNESS across rounds, asserted at a round count where
// an accumulating implementation cannot still be hiding: at most one fragment
// per kind, and exactly one system message, no matter how many times the same
// window is compacted.
func TestReinject_RepeatedCompactionDoesNotAccumulate(t *testing.T) {
	instruction := "You are yanshi."
	hist := append([]*schema.Message{{Role: schema.System, Content: instruction}}, chatter(30)...)

	const rounds = 5
	for round := 1; round <= rounds; round++ {
		plan := Plan(hist, PlanOpts{KeepRecent: 2})
		require.NotEmpty(t, plan.SummarizeIndices,
			"round %d: the window must still be compactable — an accumulating "+
				"implementation eventually pins everything", round)

		hist = AssembleWithMap(hist, plan, fmt.Sprintf("summary after round %d", round),
			fmt.Sprintf("spans covered through round %d", round))

		frags := map[FragmentKind]int{}
		systems := 0
		for _, m := range hist {
			if f, ok := parseFragment(m); ok {
				frags[f.Kind]++
			}
			if m.Role == schema.System {
				systems++
			}
		}
		assert.Equal(t, 1, frags[KindSummary], "round %d: exactly one summary", round)
		assert.Equal(t, 1, frags[KindEvictionMap], "round %d: exactly one eviction map", round)
		assert.Equal(t, 1, systems, "round %d: exactly one system message", round)

		// The freshest fragments win: round N's text, not round 1's.
		assert.Equal(t, fmt.Sprintf("summary after round %d", round),
			hist[len(hist)-1].Content[len(SummarySentinel):])

		// And the initial context is still the original, still first.
		assert.Equal(t, schema.System, hist[0].Role, "round %d", round)
		assert.Equal(t, instruction, hist[0].Content, "round %d: verbatim", round)

		// Feed a couple more turns in, as a live session would.
		hist = append(hist, chatter(4)...)
	}
}

// TestReinject_SystemPromptDominatedHistoryStillCompacts covers the cost the
// initial-context pin introduces, which the fix's own report flagged and no
// test measured.
//
// Pinning the system message spends tokens compaction used to reclaim. Those
// tokens were an illusion — adk re-prepends the instruction on the very next
// call, so the old behaviour corrupted one call without durably saving anything
// — but the change does move which histories shrink. The pathological case is a
// history whose system prompt dominates it, and yanshi's really can: the static
// half carries the operator instruction, the skill meta-prompt, the memory
// block and a probed environment listing.
//
// The assertion is that such a history still COMPACTS (the conversation half is
// still summarized and the result is still smaller), not that it shrinks by any
// particular ratio — the ratio is a property of the fixture, not of the code.
func TestReinject_SystemPromptDominatedHistoryStillCompacts(t *testing.T) {
	huge := strings.Repeat("operator instruction, skills, memory, environment. ", 400)
	hist := append([]*schema.Message{{Role: schema.System, Content: huge}}, chatter(30)...)

	before := EstimateTokens(hist)
	plan := Plan(hist, PlanOpts{KeepRecent: 2})
	require.NotEmpty(t, plan.SummarizeIndices, "the conversation half is still summarizable")

	out := Assemble(hist, plan, "short summary")
	after := EstimateTokens(out)

	assert.Less(t, after, before,
		"a system-prompt-heavy history still shrinks; the instruction is a floor, not a blocker")
	assert.Equal(t, schema.System, out[0].Role)
	assert.Equal(t, huge, out[0].Content, "and the floor is the instruction itself, kept verbatim")

	// The floor is real and worth naming: what survives is at least the prompt.
	assert.GreaterOrEqual(t, after, EstimateTokens([]*schema.Message{hist[0]}),
		"the pinned instruction sets the lower bound on what compaction can reach")
}
