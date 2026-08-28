package ctxcompact

import (
	"context"
	"fmt"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fragmentsIn is the test-local locator. It used to be an exported
// ParseFragments; review showed the whole binary compiled with that function
// hollowed out, so it was deleted and its one real user — these tests — keeps a
// three-line version here instead of the package keeping public API for it.
func fragmentsIn(msgs []*schema.Message) []fragment {
	var out []fragment
	for _, m := range msgs {
		if f, ok := parseFragment(m); ok {
			out = append(out, f)
		}
	}
	return out
}

// chatter builds n filler assistant messages — history long enough that Plan
// has something to summarize once the tail pin is satisfied.
func chatter(n int) []*schema.Message {
	out := make([]*schema.Message, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, &schema.Message{Role: schema.Assistant, Content: fmt.Sprintf("assistant chatter %d", i)})
	}
	return out
}

// TestFragment_SummaryAndMapRemainDistinguishable is the guard on the one
// property a "unified fragment mechanism" is most likely to destroy.
//
// Plan short-circuits when history ENDS in a summary (bug⑦: no
// summary-of-summary), and IsSummaryMessage is the whole of that decision. If
// the eviction map were folded into the summary's marker rather than given its
// own kind, every history ending in a map would read as already-compacted and
// stop being compactable — silently, because "nothing to summarize" is also
// what a genuinely compacted history looks like.
//
// BOTH DIRECTIONS ARE ASSERTED. A test that only shows the map does not
// short-circuit passes just as well against a Plan that never short-circuits at
// all, which is the opposite failure and equally wrong.
func TestFragment_SummaryAndMapRemainDistinguishable(t *testing.T) {
	endsInMap := append(chatter(20),
		&schema.Message{Role: schema.User, Content: EvictionMapSentinel + "spans 1-40"})
	got := Plan(endsInMap, PlanOpts{KeepRecent: 2})
	assert.NotEmpty(t, got.SummarizeIndices,
		"history ending in an eviction map must still be compactable")

	endsInSummary := append(chatter(20),
		&schema.Message{Role: schema.User, Content: SummarySentinel + "we discussed X"})
	got = Plan(endsInSummary, PlanOpts{KeepRecent: 2})
	assert.Empty(t, got.SummarizeIndices,
		"history ending in a summary is already compacted — no summary-of-summary")
	assert.Len(t, got.PinnedIndices, len(endsInSummary),
		"the short-circuit pins everything")
}

// TestContextFragment_SummarySentinelUsesTheSameMechanism pins the two
// historical sentinels to the kind-derived marker.
//
// SummarySentinel is declared in sentinel.go as its own literal and is left
// that way deliberately: its semantics must not move by a character, and its
// doc comment is the argument for why the bracketed form is collision-proof.
// What this asserts is that the literal and the derived marker are the same
// string, which is what makes MarkFragment(KindSummary, …) produce a message
// the unchanged IsSummaryMessage recognises. Let them drift and the two
// spellings become two mechanisms again — the exact thing W-D-13 removes.
func TestContextFragment_SummarySentinelUsesTheSameMechanism(t *testing.T) {
	assert.Equal(t, SummarySentinel, fragmentMarker(KindSummary))
	assert.Equal(t, EvictionMapSentinel, fragmentMarker(KindEvictionMap))

	// The predicates that were NOT rewritten must accept what MarkFragment
	// builds; that acceptance is the whole claim of "same mechanism".
	assert.True(t, IsSummaryMessage(MarkFragment(KindSummary, "body")))
	assert.True(t, IsEvictionMapMessage(MarkFragment(KindEvictionMap, "body")))
	assert.False(t, IsSummaryMessage(MarkFragment(KindEvictionMap, "body")))
	assert.False(t, IsEvictionMapMessage(MarkFragment(KindSummary, "body")))
}

// TestContextFragment_IsLocatableStrippableDedupable covers the three
// properties W-D-13 asks of a fragment, on the API that delivers them.
func TestContextFragment_IsLocatableStrippableDedupable(t *testing.T) {
	msgs := []*schema.Message{
		{Role: schema.User, Content: "real user text"},
		MarkFragment(KindEvictionMap, "spans 1-10"),
		{Role: schema.Assistant, Content: "reply"},
		MarkFragment(KindSummary, "we discussed X"),
	}

	// LOCATABLE: kind and body come back, in history order.
	frags := fragmentsIn(msgs)
	require.Len(t, frags, 2)
	assert.Equal(t, fragment{Kind: KindEvictionMap, Body: "spans 1-10"}, frags[0])
	assert.Equal(t, fragment{Kind: KindSummary, Body: "we discussed X"}, frags[1])

	// STRIPPABLE: by kind, leaving conversation and other kinds untouched.
	stripped := StripFragments(msgs, KindSummary)
	require.Len(t, stripped, 3)
	assert.Equal(t, "real user text", stripped[0].Content)
	assert.True(t, IsEvictionMapMessage(stripped[1]), "the other kind survives")
	assert.Empty(t, fragmentsIn(StripFragments(msgs, KindSummary, KindEvictionMap)),
		"stripping every kind leaves no fragment")

	// Stripping nothing is not stripping everything — a variadic call with an
	// empty kind list must be a no-op, not a purge.
	assert.Len(t, StripFragments(msgs), len(msgs))

	// DEDUPABLE: a history carrying two fragments of the same kind collapses to
	// the freshest one. Round-tripping through MarkFragment/ParseFragments is
	// what makes the duplicate detectable in the first place.
	dupes := []*schema.Message{
		MarkFragment(KindSummary, "stale"),
		{Role: schema.Assistant, Content: "reply"},
		MarkFragment(KindSummary, "stale"),
	}
	assert.Len(t, fragmentsIn(dupes), 2, "both duplicates are located")
	assert.Empty(t, fragmentsIn(StripFragments(dupes, KindSummary)))

	// A plain user message that merely QUOTES a marker mid-body is not a
	// fragment: the marker is a prefix, not a substring.
	assert.Empty(t, fragmentsIn([]*schema.Message{
		{Role: schema.User, Content: "the summary marker is " + SummarySentinel},
	}))
	// Nor is a fragment-shaped message in the wrong role — the role is half of
	// the form every predicate checks.
	assert.Empty(t, fragmentsIn([]*schema.Message{
		{Role: schema.Assistant, Content: SummarySentinel + "body"},
	}))
	assert.Empty(t, fragmentsIn([]*schema.Message{nil}))
	// An unknown kind wearing the bracket form is not one of ours.
	assert.Empty(t, fragmentsIn([]*schema.Message{
		{Role: schema.User, Content: "[yanshi:not-a-kind]\nbody"},
	}))
}

// TestContextFragment_AssembleKeepsOneFragmentPerKind is the dedup property
// where it is actually consumed, and it exists because both duplications were
// MEASURED on the shipped code before the fix, not imagined:
//
//   - A stale eviction map is pinned by Plan (isUserOriginal sees a user-role
//     message with no ToolCallID and calls it user intent), and AssembleWithMap
//     then appends the fresh render — leaving the model two directories of
//     evicted spans, the older one first and strictly contained in the newer,
//     since the map is cumulative.
//   - A stale summary lands inside the tail window after a couple more turns,
//     gets pinned by the tail rule, and the fresh summary is appended after it.
//     Two messages then each claim to be "the" summary of the conversation.
//
// The rule is FRESHEST WINS, PER KIND — not "one fragment total". The two kinds
// answer different questions (what is true now, versus what is no longer
// visible) and both belong in the assembled history.
func TestContextFragment_AssembleKeepsOneFragmentPerKind(t *testing.T) {
	msgs := []*schema.Message{
		MarkFragment(KindEvictionMap, "stale map"),
		MarkFragment(KindSummary, "stale summary"),
		{Role: schema.User, Content: "real user text"},
	}
	plan := &PlanResult{PinnedIndices: []int{0, 1, 2}, SummarizeIndices: nil}

	out := AssembleWithMap(msgs, plan, "fresh summary", "fresh map")
	frags := fragmentsIn(out)
	require.Len(t, frags, 2, "one per kind, not four")
	assert.Equal(t, KindEvictionMap, frags[0].Kind)
	assert.Equal(t, "fresh map", frags[0].Body)
	assert.Equal(t, KindSummary, frags[1].Kind)
	assert.Equal(t, "fresh summary", frags[1].Body)
	assert.Equal(t, "real user text", out[0].Content, "conversation is untouched")

	// ORDER AT THE TAIL is map then summary, and AssembleWithMap's doc argues
	// why; deduping must not quietly reorder it.
	assert.True(t, IsEvictionMapMessage(out[len(out)-2]))
	assert.True(t, IsSummaryMessage(out[len(out)-1]))
}

// TestContextFragment_AssembleKeepsAMapItIsNotReplacing is the other half of
// "freshest wins", and the half a strip-everything implementation gets wrong.
//
// The mid-turn path runs with RunOpts.EvictionMap nil — its messages are still
// in flight and have no persisted seq numbers, so there are no citable
// addresses to record. It therefore produces an empty mapText and appends no
// map. If Assemble stripped the map kind anyway, a mid-turn compaction would
// DESTROY the structured directory a pre-turn compaction built, with nothing
// put in its place: the addresses would survive only as whatever prose the
// summarizer made of them.
func TestContextFragment_AssembleKeepsAMapItIsNotReplacing(t *testing.T) {
	msgs := []*schema.Message{
		MarkFragment(KindEvictionMap, "spans 1-40"),
		{Role: schema.User, Content: "real user text"},
	}
	plan := &PlanResult{PinnedIndices: []int{0, 1}}

	out := AssembleWithMap(msgs, plan, "fresh summary", "")
	frags := fragmentsIn(out)
	require.Len(t, frags, 2)
	assert.Equal(t, fragment{Kind: KindEvictionMap, Body: "spans 1-40"}, frags[0],
		"the map survives a compaction that produced no replacement")
	assert.Same(t, msgs[0], out[0], "and survives in place, same pointer")
	assert.Equal(t, KindSummary, frags[1].Kind)
}

// TestContextFragment_StaleSummaryIsNeverPinned is what makes the strip in
// AssembleWithMap LOSSLESS rather than merely tidy.
//
// isUserOriginal already refuses to treat a summary as user intent, but that is
// only one of three routes into the pinned set. The tail rule pins the last
// 2*KeepRecent messages regardless of role, so a summary two turns old is
// pinned by position; shouldPin would pin one whose text contains "failed" or a
// diff marker, which a summary of a debugging session reliably does. A summary
// that reaches the pinned set is one Assemble is about to delete without the
// summarizer ever having read it — the content would be gone, not compressed.
//
// Unpinning it puts it in the summarize set instead, so the fresh summary is
// built from a superset of what the stale one covered. That is also the only
// form of summary-of-summary this package permits: bug⑦'s short-circuit stops
// the degenerate case (history that ENDS in a summary, nothing new to fold in).
func TestContextFragment_StaleSummaryIsNeverPinned(t *testing.T) {
	// Route 1: the tail rule. The summary sits inside the last 2*KeepRecent.
	tail := append(chatter(20),
		MarkFragment(KindSummary, "stale summary"),
		&schema.Message{Role: schema.Assistant, Content: "after 1"},
		&schema.Message{Role: schema.Assistant, Content: "after 2"},
	)
	staleAt := len(tail) - 3
	got := Plan(tail, PlanOpts{KeepRecent: 2})
	assert.NotContains(t, got.PinnedIndices, staleAt, "tail rule must not pin a stale summary")
	assert.Contains(t, got.SummarizeIndices, staleAt, "it is summarized instead, so nothing is lost")

	// Route 2: shouldPin's error marker, which a summary of a debugging session
	// carries as a matter of course.
	marked := append(chatter(20),
		MarkFragment(KindSummary, "the build failed and we fixed it"),
		&schema.Message{Role: schema.Assistant, Content: "after"},
	)
	got = Plan(marked, PlanOpts{KeepRecent: 1})
	assert.NotContains(t, got.PinnedIndices, len(marked)-2)

	// The eviction map is NOT unpinned: it has no replacement guaranteed (the
	// mid-turn path produces none), and summarizing a directory of addresses
	// into prose loses the addresses.
	withMap := append(chatter(20), MarkFragment(KindEvictionMap, "spans 1-40"),
		&schema.Message{Role: schema.Assistant, Content: "after"})
	got = Plan(withMap, PlanOpts{KeepRecent: 1})
	assert.Contains(t, got.PinnedIndices, len(withMap)-2, "the map stays pinned")
}

// TestContextFragment_StaleSummaryAloneIsNotWorthAModelCall guards the branch
// unpinStaleSummaries nearly destroyed.
//
// Run has a FREE exit: an empty summarize set means there is nothing to fold,
// so it folds tool results and returns without calling a model at all. Unpinning
// stale summaries can manufacture work for that branch — if a summary is the
// ONLY thing left unpinned, Run pays for a summary call whose entire input is a
// previous summary, produces nothing smaller, and the caller discards it on
// TokensAfter >= TokensBefore.
//
// Mid-turn that is not a one-off. CompactingModel.maybeCompact arms its cooldown
// only on success, so a discarded compaction leaves the gate wide open and the
// next ReAct iteration repeats it — one wasted summary call PER ITERATION for
// the rest of the turn.
//
// The assertions are the call count and the token delta, not "no error": the
// broken version returned no error, which is exactly why it was invisible.
func TestContextFragment_StaleSummaryAloneIsNotWorthAModelCall(t *testing.T) {
	// Every message is user-original (isUserOriginal pins them all), so once the
	// stale summary is unpinned it is the only member of the summarize set.
	msgs := make([]*schema.Message, 0, 12)
	for i := 0; i < 10; i++ {
		msgs = append(msgs, &schema.Message{Role: schema.User, Content: fmt.Sprintf("user says %d", i)})
	}
	msgs = append(msgs,
		MarkFragment(KindSummary, "an earlier summary of an earlier conversation"),
		&schema.Message{Role: schema.User, Content: "and one more"},
	)

	plan := Plan(msgs, PlanOpts{KeepRecent: 2})
	assert.Empty(t, plan.SummarizeIndices,
		"a lone stale summary is not content to fold — leave Run its free exit")
	// Same shape check the tool-pair route uses. Both routes into this defect
	// were disguised by a FALLING token count, so the durable property is what
	// goes into the summary call, not what comes out of it.
	assertSummarizeSetIsNotJustStaleFragments(t, msgs, plan)

	rec := &recordingSummarizer{Return: "fresh summary"}
	res, err := Run(context.Background(), msgs, PlanOpts{KeepRecent: 2},
		RunOpts{ModelWindow: 8000}, rec, nil)
	require.NoError(t, err)
	assert.Empty(t, rec.GenerateCalls, "no summary model call was paid for")
	assert.Empty(t, rec.StreamCalls)
	assert.LessOrEqual(t, res.TokensAfter, res.TokensBefore,
		"and the history did not GROW — the discarded path spent tokens to get bigger")

	// The stale summary is still there: nothing was dropped in exchange.
	assert.Len(t, fragmentsIn(res.Messages), 1)
}

// TestContextFragment_ToolPairRepairDoesNotStrandAStaleSummary is the second
// route into the defect TestContextFragment_StaleSummaryAloneIsNotWorthAModelCall
// was written to close. That test covers a history where every message is
// user-original; this one covers a history where the summarize set looks
// non-empty when hasUnpinnedContent runs and is empty by the time Plan returns.
//
// The mechanism is an ORDERING bug, not a logic bug. hasUnpinnedContent asks
// "is there real conversation left to fold this stale summary into", and the
// only honest answer comes from the FINAL pin set. Run it before
// EnforceToolCallPairs and it sees an intermediate one:
//
//	msgs[2] is a tool result whose tool_call (msgs[1]) is pinned by the error
//	marker, but which nothing pins on its own. At that moment it reads as
//	unpinned conversation, so the guard says "yes, there is content" and unpins
//	the stale summary. EnforceToolCallPairs then pins msgs[2] back — correctly,
//	since severing a tool_call from its result is a 400 from the provider — and
//	the stale summary is left alone in the summarize set.
//
// THIS ONE IS NASTIER THAN THE ORIGINAL. The original produced a summary that
// was no smaller, so maybeCompact's `TokensAfter >= TokensBefore` gate threw it
// away. Here the numbers fall (133 -> 119 as measured), so the mid-turn path
// scores it a SUCCESS, arms the cooldown, and installs a summary-of-a-summary —
// exactly the semantics bug⑦'s short-circuit exists to prevent, reached from
// the one angle bug⑦ cannot see, since it only inspects the LAST message.
//
// Fixture is the reviewer's, reproduced verbatim so the two agree on the shape.
func TestContextFragment_ToolPairRepairDoesNotStrandAStaleSummary(t *testing.T) {
	msgs := []*schema.Message{
		{Role: schema.User, Content: "please fix the bug"}, // rule 2
		{ // rule 4: the error marker pins it, and it carries the tool_call
			Role:      schema.Assistant,
			Content:   "retrying after error: connection refused",
			ToolCalls: []schema.ToolCall{{ID: "tc1", Function: schema.FunctionCall{Name: "shell_run"}}},
		},
		{Role: schema.Tool, Content: "ok", ToolCallID: "tc1"}, // nothing pins this on its own
		{Role: schema.User, Content: "thanks, continuing"},    // rule 2
		MarkFragment(KindSummary, "an earlier summary of an earlier conversation"),
		{Role: schema.User, Content: "one more thing"}, // tail + rule 2
	}

	got := Plan(msgs, PlanOpts{KeepRecent: 1})

	// The tool result must end up pinned — that is EnforceToolCallPairs doing
	// its job, and it is what invalidates the pre-pairing view of the pin set.
	assert.Contains(t, got.PinnedIndices, 2, "premise: pair repair pins the tool result back")

	// THE PROPERTY: the summarize set must never be nothing but a stale summary.
	// Asserted as a general shape rather than as "SummarizeIndices is empty", so
	// a third route into the same state is caught too.
	assertSummarizeSetIsNotJustStaleFragments(t, msgs, got)
}

// assertSummarizeSetIsNotJustStaleFragments fails when everything Plan chose to
// summarize is a compaction artefact.
//
// It exists because BOTH routes into this defect were disguised the same way:
// the token count went DOWN, so every caller-side sanity check scored the
// result a successful compaction. "No error" and "it got smaller" are both
// satisfied by summarizing a summary. The only thing that distinguishes the
// broken state is what went INTO the summary call, so that is what this checks.
func assertSummarizeSetIsNotJustStaleFragments(t *testing.T, msgs []*schema.Message, plan *PlanResult) {
	t.Helper()
	if len(plan.SummarizeIndices) == 0 {
		return // nothing to summarize is Run's free exit — the safe outcome
	}
	for _, i := range plan.SummarizeIndices {
		if i < 0 || i >= len(msgs) {
			continue
		}
		if _, isFragment := parseFragment(msgs[i]); !isFragment {
			return // real conversation is present; the summary has something to fold in
		}
	}
	t.Fatalf("the summarize set is nothing but compaction artefacts (indices %v): "+
		"this pays a model call to summarize a summary, and the token count still "+
		"falls, so no caller-side gate catches it", plan.SummarizeIndices)
}
