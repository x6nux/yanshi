// internal/ctxcompact/fold_test.go
package ctxcompact

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
)

// toolResult builds a tool result message of roughly the requested line count.
func toolResult(id string, lines int) *schema.Message {
	var b strings.Builder
	for i := 0; i < lines; i++ {
		fmt.Fprintf(&b, "internal/tools/file%d.go:%d: some matched line of output here\n", i, i*7)
	}
	return &schema.Message{Role: schema.Tool, ToolCallID: id, Content: b.String()}
}

// manyToolResults builds a history of n tool results, each `lines` long, with
// the assistant tool_call that produced each one — the shape a coding agent
// generates and the one C5 exists for.
func manyToolResults(n, lines int) []*schema.Message {
	msgs := []*schema.Message{{Role: schema.User, Content: "find every caller"}}
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("call_%d", i)
		msgs = append(msgs, &schema.Message{
			Role:      schema.Assistant,
			ToolCalls: []schema.ToolCall{{ID: id, Function: schema.FunctionCall{Name: "grep", Arguments: `{"q":"x"}`}}},
		})
		msgs = append(msgs, toolResult(id, lines))
	}
	return msgs
}

// plainToolResults is manyToolResults with PATH-FREE bodies.
//
// It exists because Plan pins any message mentioning a working-set path, and
// manyToolResults emits grep-style output naming a .go file on every line — so
// Plan pins the entire history and the summarize set comes back empty. That is
// correct behaviour and exactly what TestRunAppliesFoldingToPinnedResults
// wants, but it makes the summarizer unreachable, so a test that needs
// summarization to happen has to avoid tripping the rule.
func plainToolResults(n, lines int) []*schema.Message {
	msgs := []*schema.Message{{Role: schema.User, Content: "run the checks"}}
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("call_%d", i)
		msgs = append(msgs, &schema.Message{
			Role:      schema.Assistant,
			ToolCalls: []schema.ToolCall{{ID: id, Function: schema.FunctionCall{Name: "check", Arguments: `{"n":1}`}}},
		})
		var b strings.Builder
		for j := 0; j < lines; j++ {
			fmt.Fprintf(&b, "checked item %d of batch %d and it reported nothing unusual\n", j, i)
		}
		msgs = append(msgs, &schema.Message{Role: schema.Tool, ToolCallID: id, Content: b.String()})
	}
	return msgs
}

// TestFoldFiresOnAggregatePressureNotSingleSize is the bug C5 names.
//
// internal/tools caps ONE result at 64 KiB. That cap is per-message and cannot
// see the failure that actually fills a coding agent's window: a hundred
// results of ten kilobytes each, every one comfortably under the cap, together
// a megabyte. This test builds exactly that history and asserts that folding
// fires on it — and, in the same breath, that the same total spread over a
// budget large enough to hold it does NOT fold, because a trigger that always
// fires is not measuring pressure.
func TestFoldFiresOnAggregatePressureNotSingleSize(t *testing.T) {
	msgs := manyToolResults(60, 40)
	total := EstimateTokens(msgs)

	// No single result is anywhere near a size that would spill.
	for _, m := range msgs {
		if isToolResult(m) && len(m.Content) >= 64*1024 {
			t.Fatalf("fixture invalid: a single result is %d bytes, large enough to spill; "+
				"this test must exercise the aggregate case", len(m.Content))
		}
	}

	tight := total / 2 // pressure well over FoldPressureThreshold
	folded, stats := FoldToolResults(msgs, FoldOptions{Budget: tight})
	if stats.Truncated+stats.Digested == 0 {
		t.Fatalf("no result folded at budget %d for a %d-token history (pressure %.2f); "+
			"the aggregate case is exactly what the per-message spill cap cannot see",
			tight, total, ToolResultPressure(msgs, tight))
	}
	if stats.TokensAfter >= stats.TokensBefore {
		t.Errorf("folding did not shrink the history: %d → %d", stats.TokensBefore, stats.TokensAfter)
	}
	if len(folded) != len(msgs) {
		t.Errorf("folding changed the message count %d → %d; pairs would be severed", len(msgs), len(folded))
	}

	// And the negative: a roomy budget must leave everything alone.
	roomy := total * 4
	_, quiet := FoldToolResults(msgs, FoldOptions{Budget: roomy})
	if quiet.Truncated+quiet.Digested != 0 {
		t.Errorf("folded %d results at budget %d for a %d-token history (pressure %.2f); "+
			"folding an unpressured history costs fidelity for nothing",
			quiet.Truncated+quiet.Digested, roomy, total, ToolResultPressure(msgs, roomy))
	}
}

// TestFoldNeverTouchesTheRecentTail is the invariant that keeps a folding
// agent working.
//
// The model is mid-task on the newest results: it reads a file, decides what
// to change, and writes it. Folding the read it is about to act on makes it
// re-read the same file next iteration — which costs more window than folding
// saved, and loops.
func TestFoldNeverTouchesTheRecentTail(t *testing.T) {
	msgs := manyToolResults(30, 60)
	folded, stats := FoldToolResults(msgs, FoldOptions{Budget: EstimateTokens(msgs) / 4})
	if stats.Truncated+stats.Digested == 0 {
		t.Fatal("nothing folded; the invariant below would be vacuous")
	}

	// Walk backwards collecting tool results, mirroring FoldToolResults.
	seen := 0
	for i := len(folded) - 1; i >= 0 && seen < FoldKeepRecent; i-- {
		if !isToolResult(folded[i]) {
			continue
		}
		seen++
		if folded[i].Content != msgs[i].Content {
			t.Errorf("tool result %d from the end was folded; the model is working with it right now.\ngot: %q",
				seen, firstRunes(folded[i].Content, 100))
		}
	}
	if seen != FoldKeepRecent {
		t.Fatalf("only found %d recent tool results, want %d — the fixture cannot exercise the invariant", seen, FoldKeepRecent)
	}
	// The OLDEST must have been folded, or "keeps the recent ones" is
	// indistinguishable from "keeps all of them".
	oldest := -1
	for i := range msgs {
		if isToolResult(msgs[i]) {
			oldest = i
			break
		}
	}
	if folded[oldest].Content == msgs[oldest].Content {
		t.Error("the oldest tool result was not folded, so 'protect the recent tail' is not distinguishable from 'protect everything'")
	}
}

// TestFoldTiersEscalateWithPressure pins the three-tier ladder. Each step must
// be strictly smaller than the one before, or the tier is decorative.
func TestFoldTiersEscalateWithPressure(t *testing.T) {
	if got := PressureFold(0.1); got != FoldNone {
		t.Errorf("PressureFold(0.1) = %v, want %v", got, FoldNone)
	}
	if got := PressureFold(FoldPressureThreshold); got != FoldTruncated {
		t.Errorf("PressureFold(%v) = %v, want %v", FoldPressureThreshold, got, FoldTruncated)
	}
	if got := PressureFold(FoldDigestPressure); got != FoldDigest {
		t.Errorf("PressureFold(%v) = %v, want %v", FoldDigestPressure, got, FoldDigest)
	}

	msgs := manyToolResults(40, 50)
	total := EstimateTokens(msgs)
	// Budgets chosen so pressure lands in each band. Pressure is
	// toolTokens/budget, so a smaller budget is more pressure.
	sizes := map[string]int{}
	for _, tc := range []struct {
		name   string
		budget int
	}{
		{"none", total * 4},
		{"truncated", int(float64(total) / (FoldPressureThreshold + 0.1))},
		{"digest", int(float64(total) / (FoldDigestPressure + 0.15))},
	} {
		out, _ := FoldToolResults(msgs, FoldOptions{Budget: tc.budget})
		sizes[tc.name] = EstimateTokens(out)
		t.Logf("%-10s budget=%7d pressure=%.2f tokens=%d",
			tc.name, tc.budget, ToolResultPressure(msgs, tc.budget), sizes[tc.name])
	}
	if !(sizes["digest"] < sizes["truncated"] && sizes["truncated"] < sizes["none"]) {
		t.Errorf("tiers do not escalate: none=%d truncated=%d digest=%d",
			sizes["none"], sizes["truncated"], sizes["digest"])
	}
}

// TestFoldedResultsCarryARecoveryPointer is what separates folding from
// deletion.
//
// A folded result the model cannot get back is information destroyed, and the
// model has no way to know it is missing something. Every folded body must
// name where the full text still lives.
func TestFoldedResultsCarryARecoveryPointer(t *testing.T) {
	tests := []struct {
		name string
		msg  *schema.Message
		want string
		why  string
	}{
		{
			"spilled_result_points_at_the_file",
			&schema.Message{Role: schema.Tool, ToolCallID: "call_1",
				Content: "[spilled: 90000 lines / 12.0 MB → .yanshi/tmp/spillover/shell_run-123-abcd.txt]\n" +
					strings.Repeat("line of spilled preview output here\n", 300)},
			".yanshi/tmp/spillover/shell_run-123-abcd.txt",
			"the path is already in the body and fs_read resolves it to the exact bytes",
		},
		{
			"unspilled_result_points_at_the_call",
			toolResult("call_42", 300),
			"call_42",
			"the message is persisted under this id, so history_search finds it",
		},
		{
			"no_handle_says_so",
			&schema.Message{Role: schema.Tool, ToolCallID: "x", Content: strings.Repeat("plain output\n", 300)},
			"history_search",
			"an id is a real handle even without a spill path",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, tier := range []FoldTier{FoldTruncated, FoldDigest} {
				got, applied := foldOne(tt.msg, tier)
				if applied == FoldNone {
					t.Fatalf("tier %v: nothing folded, so the pointer is untested", tier)
				}
				if !strings.Contains(got.Content, tt.want) {
					t.Errorf("tier %v: folded body has no recovery pointer to %q (%s):\n%s",
						tier, tt.want, tt.why, firstRunes(got.Content, 300))
				}
			}
		})
	}

	// And the honest-failure case: no id and no spill path must produce a
	// marker that ADMITS it, not one that invents a handle.
	orphan := &schema.Message{Role: schema.Tool, ToolCallID: "id", Content: strings.Repeat("x y z\n", 300)}
	marker := foldMarker(orphan.Content, &schema.Message{Role: schema.Tool})
	if !strings.Contains(marker, "not retained") {
		t.Errorf("with no resolvable handle the marker should say so, got %q. "+
			"A pointer that looks recoverable and is not costs a tool call and teaches the model the pointers are noise", marker)
	}
}

// TestSpillMarkerMatchesToolsPreview pins the soft coupling between this
// package and internal/tools.
//
// ctxcompact is a leaf and must not import tools (GOV1), so the spillover
// marker is matched textually. That is a real coupling with no compiler behind
// it: if tools changes its preview header, spillPathIn silently stops finding
// paths and every folded spillover result falls back to the weaker
// tool-call-id pointer. The fallback is safe, which is exactly why the drift
// would never be noticed.
//
// The literal below is the format internal/tools/spillover.go::spillPreview
// produces. Changing it there without changing it here reddens this test.
func TestSpillMarkerMatchesToolsPreview(t *testing.T) {
	// Mirrors: fmt.Fprintf(&b, "[spilled: %d lines / %s → %s]\n%s", ...)
	const toolsPreviewHeader = "[spilled: 1234 lines / 2.0 MB → .yanshi/tmp/spillover/shell_run-1-ab.txt]"
	got := spillPathIn(toolsPreviewHeader + "\nrest of the preview")
	if got != ".yanshi/tmp/spillover/shell_run-1-ab.txt" {
		t.Fatalf("spillPathIn did not parse the tools preview header: got %q.\n"+
			"internal/tools/spillover.go::spillPreview and ctxcompact's spillPathIn have drifted; "+
			"every folded spillover result now falls back to the weaker tool-call-id pointer, silently", got)
	}
	if !strings.HasPrefix(toolsPreviewHeader, spillMarkerPrefix) {
		t.Errorf("spillMarkerPrefix %q no longer opens the tools preview", spillMarkerPrefix)
	}
	// Negatives: a body with no marker, and a malformed one, must both yield
	// "" rather than a garbage path that fs_read would fail on.
	for _, bad := range []string{"", "no marker here", "[spilled: broken", "[spilled: no arrow]"} {
		if p := spillPathIn(bad); p != "" {
			t.Errorf("spillPathIn(%q) = %q, want \"\"", bad, p)
		}
	}
}

// TestFoldLeavesNonToolMessagesAlone. User intent and assistant reasoning are
// not recoverable from anywhere — there is no spill file and no re-runnable
// call — so folding them would be pure loss. Only tool results, whose source
// is a repeatable call or a file on disk, are eligible.
func TestFoldLeavesNonToolMessagesAlone(t *testing.T) {
	long := strings.Repeat("a sentence of user requirements that must not be folded away. ", 60)
	msgs := append(manyToolResults(30, 60),
		&schema.Message{Role: schema.User, Content: long},
		&schema.Message{Role: schema.Assistant, Content: long},
		// A Tool-role message with no call id is not a pairable tool result;
		// EnforceToolCallPairs cannot reason about it, so folding must not
		// either.
		&schema.Message{Role: schema.Tool, Content: long},
	)
	folded, stats := FoldToolResults(msgs, FoldOptions{Budget: EstimateTokens(msgs) / 4})
	if stats.Truncated+stats.Digested == 0 {
		t.Fatal("nothing folded; the invariant would be vacuous")
	}
	for i, m := range msgs {
		if isToolResult(m) {
			continue
		}
		if folded[i].Content != m.Content {
			t.Errorf("message %d (role %s, no tool id) was folded; it is not recoverable from anywhere",
				i, m.Role)
		}
	}
}

// TestFoldDoesNotMutateTheInput. Run hands its caller the folded slice while
// the caller may still hold the original — MaybeCompact falls back to it on
// several paths — so folding in place would corrupt the fallback.
func TestFoldDoesNotMutateTheInput(t *testing.T) {
	msgs := manyToolResults(30, 60)
	originals := make([]string, len(msgs))
	for i, m := range msgs {
		originals[i] = m.Content
	}
	folded, stats := FoldToolResults(msgs, FoldOptions{Budget: EstimateTokens(msgs) / 4})
	if stats.Truncated+stats.Digested == 0 {
		t.Fatal("nothing folded; the invariant would be vacuous")
	}
	for i, m := range msgs {
		if m.Content != originals[i] {
			t.Fatalf("input message %d was mutated in place; a caller falling back to the original history now gets the folded one", i)
		}
	}
	if &folded[0] == &msgs[0] {
		t.Error("the returned slice aliases the input backing array")
	}
}

// TestFoldSkipsShortAndUnprofitableResults. Folding a short result costs more
// in marker text than it saves and destroys content for a negative return.
func TestFoldSkipsShortAndUnprofitableResults(t *testing.T) {
	short := &schema.Message{Role: schema.Tool, ToolCallID: "c", Content: "ok\nexit 0\n"}
	for _, tier := range []FoldTier{FoldTruncated, FoldDigest} {
		got, applied := foldOne(short, tier)
		if applied != FoldNone {
			t.Errorf("tier %v folded a %d-rune result into %q", tier, len([]rune(short.Content)), got.Content)
		}
		if got != short {
			t.Errorf("tier %v allocated a copy for a message it did not change", tier)
		}
	}
	// A long single-line body has no useful head/tail split, so the truncated
	// tier must fall through to the digest rather than returning something
	// bigger than it started with.
	oneLine := &schema.Message{Role: schema.Tool, ToolCallID: "c",
		Content: strings.Repeat(`{"k":"v"},`, 400)}
	got, applied := foldOne(oneLine, FoldTruncated)
	if applied == FoldNone {
		t.Fatal("a 4000-byte single-line body was not folded at all")
	}
	if len(got.Content) >= len(oneLine.Content) {
		t.Errorf("folding a single-line body made it larger: %d → %d", len(oneLine.Content), len(got.Content))
	}
}

// TestFoldKeepRecentZeroIsDistinguishableFromUnset. A last-ditch caller may
// legitimately want to fold everything including the newest result; a caller
// that simply did not set the field must get the protective default. A plain
// int cannot tell those apart, which is what KeepRecentSet is for.
func TestFoldKeepRecentZeroIsDistinguishableFromUnset(t *testing.T) {
	if got := (FoldOptions{}).foldKeepRecent(); got != FoldKeepRecent {
		t.Errorf("unset KeepRecent = %d, want the protective default %d", got, FoldKeepRecent)
	}
	if got := (FoldOptions{KeepRecent: 0, KeepRecentSet: true}).foldKeepRecent(); got != 0 {
		t.Errorf("explicit KeepRecent=0 = %d, want 0", got)
	}
	if got := (FoldOptions{KeepRecent: -3, KeepRecentSet: true}).foldKeepRecent(); got != 0 {
		t.Errorf("negative KeepRecent = %d, want 0", got)
	}

	msgs := manyToolResults(20, 60)
	budget := EstimateTokens(msgs) / 4
	_, def := FoldToolResults(msgs, FoldOptions{Budget: budget})
	_, all := FoldToolResults(msgs, FoldOptions{Budget: budget, KeepRecentSet: true})
	if all.Truncated+all.Digested <= def.Truncated+def.Digested {
		t.Errorf("KeepRecent=0 folded %d results, not more than the default's %d; the setting is inert",
			all.Truncated+all.Digested, def.Truncated+def.Digested)
	}
}

// TestFoldIsANoOpWithoutABudget. Zero budget means "unbudgeted" everywhere
// else in this package, and folding must read it the same way — otherwise a
// caller that never configured a window silently gets its tool output
// destroyed.
func TestFoldIsANoOpWithoutABudget(t *testing.T) {
	msgs := manyToolResults(30, 60)
	for _, budget := range []int{0, -1} {
		folded, stats := FoldToolResults(msgs, FoldOptions{Budget: budget})
		if stats.Truncated+stats.Digested != 0 {
			t.Errorf("budget=%d folded %d results; with no budget there is no pressure to relieve",
				budget, stats.Truncated+stats.Digested)
		}
		if ToolResultPressure(msgs, budget) != 0 {
			t.Errorf("ToolResultPressure with budget=%d is non-zero", budget)
		}
		for i := range msgs {
			if folded[i] != msgs[i] {
				t.Fatalf("budget=%d rewrote message %d", budget, i)
			}
		}
	}
	// Empty input must not panic or invent stats.
	if _, stats := FoldToolResults(nil, FoldOptions{Budget: 1000}); stats.Considered != 0 {
		t.Errorf("nil history reported %d considered", stats.Considered)
	}
}

// TestPressureMeasuresToolResultsOnly. The question folding answers is not "is
// the history large" — Plan and RunSummary already handle that — but "is it
// large BECAUSE OF material that can be recovered on demand". A history full
// of user turns is equally close to the wall and folding can do nothing about
// it; measuring total size would fire the pass on exactly those histories.
func TestPressureMeasuresToolResultsOnly(t *testing.T) {
	prose := strings.Repeat("the user explained the requirements at length. ", 400)
	userHeavy := []*schema.Message{
		{Role: schema.User, Content: prose},
		{Role: schema.Assistant, Content: prose},
	}
	budget := EstimateTokens(userHeavy) / 2 // the history is over budget
	if p := ToolResultPressure(userHeavy, budget); p != 0 {
		t.Errorf("pressure = %.2f for a history with no tool results at all; folding would fire and achieve nothing", p)
	}
	_, stats := FoldToolResults(userHeavy, FoldOptions{Budget: budget})
	if stats.Truncated+stats.Digested != 0 {
		t.Errorf("folded %d messages in a history with no tool results", stats.Truncated+stats.Digested)
	}
}

// TestFoldTierString covers the log rendering, which is the only way an
// operator sees which tier fired.
func TestFoldTierString(t *testing.T) {
	for tier, want := range map[FoldTier]string{
		FoldNone: "none", FoldTruncated: "truncated", FoldDigest: "digest",
	} {
		if got := tier.String(); got != want {
			t.Errorf("FoldTier(%d).String() = %q, want %q", tier, got, want)
		}
	}
}

// TestRunAppliesFoldingToPinnedResults is the WIRING test, and it exists
// because a probe found the hole: every other test in this file calls
// FoldToolResults directly, so unwiring it from Run left the whole file green
// while folding never ran in production. Written-but-unread is the dominant
// defect shape in this repo, and folding is unusually exposed to it — a
// history that was going to be summarized anyway shrinks either way, so the
// callers' TokensAfter < TokensBefore check cannot tell whether folding
// contributed.
//
// The fixture is deliberately the ALL-PINNED shape: every tool result is
// recent enough that Plan pins it, so summarization has nothing to work with
// and folding is the only lever. That is both the branch most likely to be
// missed when wiring and the one where folding matters most.
func TestRunAppliesFoldingToPinnedResults(t *testing.T) {
	msgs := manyToolResults(40, 60)
	total := EstimateTokens(msgs)
	// KeepRecent large enough that Plan pins everything: nothing to summarize,
	// so any shrinkage is folding's doing and nothing else's.
	planOpts := PlanOpts{KeepRecent: len(msgs)}
	runOpts := RunOpts{ModelWindow: total, ChunkThreshold: 0.9}

	rs := &recordingSummarizer{Return: "unused"}
	res, err := Run(context.Background(), msgs, planOpts, runOpts, rs, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if calls := len(rs.GenerateCalls) + len(rs.StreamCalls); calls != 0 {
		t.Fatalf("the fixture summarized after all (%d calls); it cannot isolate folding", calls)
	}
	if res.Fold.Truncated+res.Fold.Digested == 0 {
		t.Fatalf("Run reported no folding on an all-pinned, over-budget history "+
			"(%d tokens, budget %d, pressure %.2f). FoldToolResults is not reachable from Run",
			total, budgetFor(runOpts), ToolResultPressure(msgs, budgetFor(runOpts)))
	}
	if res.TokensAfter >= res.TokensBefore {
		t.Errorf("Run did not shrink an all-pinned history: %d → %d", res.TokensBefore, res.TokensAfter)
	}
	// The stats must describe the messages actually returned, not a fold pass
	// whose output was thrown away.
	if got := EstimateTokens(res.Messages); got != res.TokensAfter {
		t.Errorf("TokensAfter=%d but the returned messages estimate at %d", res.TokensAfter, got)
	}
}

// TestRunFoldsAfterSummarizing pins the ORDER, which is the other half of the
// wiring and is invisible from outside without this check.
//
// Folding before the summary would hand the summarizer truncated inputs and
// bake that loss into the one artefact with unbounded lifetime — the summary
// is re-sent on every later turn, so a detail lost there is lost permanently,
// whereas a folded pinned message still has its recovery pointer. The
// assertion is that whatever the summarizer was shown contains no fold marker.
//
// THE FIXTURE CANNOT MENTION FILE PATHS, and finding that out is why this test
// reads the way it does. manyToolResults emits grep-style output, so every
// result names a .go file, Plan's working-set rule pins all of them, and the
// summarize set comes back empty — the summarizer is never called and the
// order is unobservable. plainToolResults produces path-free output so Plan
// actually splits the history.
func TestRunFoldsAfterSummarizing(t *testing.T) {
	msgs := plainToolResults(40, 60)
	total := EstimateTokens(msgs)
	rs := &recordingSummarizer{Return: strings.Repeat("real summary content. ", 40)}
	res, err := Run(context.Background(), msgs, PlanOpts{KeepRecent: 2},
		RunOpts{ModelWindow: total / 2, ChunkThreshold: 0.9, DisableQualityGate: true}, rs, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	seen := append(append([][]*schema.Message{}, rs.GenerateCalls...), rs.StreamCalls...)
	if len(seen) == 0 {
		t.Fatal("the summarizer was never called; the order cannot be observed")
	}
	for i, call := range seen {
		for j, m := range call {
			if strings.Contains(m.Content, "folded out of context") ||
				strings.Contains(m.Content, "[folded tool result:") {
				t.Fatalf("summarizer call %d message %d was already folded:\n%s\n"+
					"Folding before summarizing bakes the loss into the summary, which is re-sent forever",
					i, j, firstRunes(m.Content, 200))
			}
		}
	}
	if res == nil {
		t.Fatal("Run returned a nil result")
	}
}
