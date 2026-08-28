// internal/tools/memory_autorecall_test.go
package tools

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/store"
)

func newRecallStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestAutoRecall_FiresOnARelevantMemory is C12's reason to exist: the model
// never calls memory_search, so a stored preference has to arrive without
// being asked for.
func TestAutoRecall_FiresOnARelevantMemory(t *testing.T) {
	s := newRecallStore(t)
	_, err := s.WriteMemory("pref", "always run gofmt before committing Go code")
	require.NoError(t, err)
	_, err = s.WriteMemory("note", "the deploy pipeline lives in .github/workflows")
	require.NoError(t, err)

	got := AutoRecall(context.Background(), s, "can you format this gofmt thing for me", store.MemoryFilter{})
	if got == "" {
		t.Fatal("a directly relevant memory was not recalled")
	}
	if !strings.Contains(got, "always run gofmt") {
		t.Errorf("the matching memory is missing from the block:\n%s", got)
	}
	if !strings.Contains(got, AutoRecallHeader) {
		t.Errorf("the block carries no header, so the model cannot tell recalled notes from the current request:\n%s", got)
	}
}

// TestAutoRecall_FindsChineseMemory goes through AutoRecall itself, not
// SearchMemoryRanked directly, because that is the gap the CJK fix had to
// close: ftsQuery renders the term as `"张伟"` (FTS5 phrase syntax), and
// hasCJK routes that string into the LIKE fallback. Before parseFTSTerms
// existed, the fallback searched for the literal string `"张伟"` — quote
// characters included — which no real memory content contains, so this path
// stayed dead in Chinese even after the fallback was added.
//
// ledger: A2/W-A-03#2 SearchMemory 与 memory_autorecall 走同一检索路径因而同时生效
func TestAutoRecall_FindsChineseMemory(t *testing.T) {
	s := newRecallStore(t)
	_, err := s.WriteMemory("note", "项目的截止日期是周二，负责人是张伟")
	require.NoError(t, err)

	got := AutoRecall(context.Background(), s, "张伟", store.MemoryFilter{})
	require.NotEmpty(t, got, "a Chinese memory containing the query term was not recalled")
	require.Contains(t, got, "张伟")
}

// TestAutoRecall_StaysSilentBelowTheThreshold is the half that makes the
// feature tolerable. Injecting a few weak matches on every turn costs tokens
// forever and trains the model to skim past the block — which also disarms the
// turn where the match was right.
func TestAutoRecall_StaysSilentBelowTheThreshold(t *testing.T) {
	s := newRecallStore(t)
	for _, m := range []string{
		"the deploy pipeline lives in .github/workflows",
		"prefer tabs over spaces in Makefiles",
		"the staging database is reset every Sunday",
	} {
		_, err := s.WriteMemory("note", m)
		require.NoError(t, err)
	}
	for _, q := range []string{
		"what is the weather like today",
		"hello",
		"",
		"   ",
		"?!.",
	} {
		if got := AutoRecall(context.Background(), s, q, store.MemoryFilter{}); got != "" {
			t.Errorf("query %q injected something with nothing relevant to inject:\n%s", q, got)
		}
	}

	// THE CASE ABOVE DOES NOT EXERCISE THE FILTER, which a mutation probe
	// established: those queries share no term with any memory, so FTS returns
	// nothing and replacing the relevance test with `if false` left them all
	// green. The filter only earns its place on a query that DOES match — one
	// distinctive word in common out of many — which is precisely the accidental
	// match it exists to suppress.
	q := "the staging deploy of the tabs prototype hit an unexpected " +
		"authentication timeout while provisioning replicas"
	require.NotEmpty(t, s2Hits(t, s, q), "the probe query no longer matches anything; "+
		"this case has stopped testing the relevance filter and needs a new query")
	if got := AutoRecall(context.Background(), s, q, store.MemoryFilter{}); got != "" {
		t.Errorf("a query sharing one word out of a dozen was injected anyway:\n%s", got)
	}
}

// s2Hits is the RAW FTS result for a query, ignoring the relevance filter. It
// exists so a "must stay silent" case can prove the silence comes from the
// filter and not from the query matching nothing — the difference between a
// test of the filter and a test of the fixture.
func s2Hits(t *testing.T, s *store.Store, q string) []store.MemoryHit {
	t.Helper()
	hits, err := s.SearchMemoryRanked(AutoRecallQuery(q), 20, store.MemoryFilter{})
	require.NoError(t, err)
	return hits
}

// TestAutoRecall_IsBoundedInCountAndSize pins both ceilings on the one input
// designed to blow through them: many long, all-relevant memories.
func TestAutoRecall_IsBoundedInCountAndSize(t *testing.T) {
	s := newRecallStore(t)
	for i := 0; i < 30; i++ {
		_, err := s.WriteMemory("note",
			"kubernetes deployment rollout strategy note "+strings.Repeat("detail ", 400))
		require.NoError(t, err)
	}
	hits := AutoRecallHits(context.Background(), s, "kubernetes deployment rollout strategy", store.MemoryFilter{})
	if len(hits) > MaxAutoRecall {
		t.Errorf("injected %d memories, over the cap of %d", len(hits), MaxAutoRecall)
	}
	if len(hits) == 0 {
		t.Fatal("nothing matched a query built from the memories themselves")
	}
	block := RenderRecalledMemories(hits)
	// The header is fixed overhead; the bound is on the memory bodies.
	body := strings.TrimPrefix(block, AutoRecallHeader)
	if len(body) > AutoRecallCharBudget+len(hits)*120 {
		t.Errorf("the injected block is %d chars of memory content, well over the %d budget",
			len(body), AutoRecallCharBudget)
	}
	if !strings.Contains(block, "truncated") {
		t.Error("an over-long memory was injected whole, or truncated without saying so")
	}
}

// TestAutoRecall_BlockIsDatedAndFramedAsThePast is the framing requirement. A
// year-old preference rendered without a date reads as something the user just
// said, and the model acts on it as the current instruction.
func TestAutoRecall_BlockIsDatedAndFramedAsThePast(t *testing.T) {
	s := newRecallStore(t)
	_, err := s.WriteMemory("pref", "deploy with terraform apply, never the console")
	require.NoError(t, err)

	got := AutoRecall(context.Background(), s, "how do I deploy with terraform", store.MemoryFilter{})
	require.NotEmpty(t, got)
	for _, want := range []string{"EARLIER", "not part of the current request", "may be out of date", "memory_search"} {
		if !strings.Contains(got, want) {
			t.Errorf("the framing is missing %q, so the block reads as current fact:\n%s", want, got)
		}
	}
	// A date, in the form the renderer writes it.
	if !strings.Contains(got, "-") || !strings.Contains(got, "[pref]") {
		t.Errorf("the entry carries no kind or date:\n%s", got)
	}
}

// TestAutoRecall_HidesSupersededMemories is the C12/C13 join. A distillation
// merges several notes into one; if the automatic injection kept returning the
// originals, every consolidation would make the injected block LONGER and more
// contradictory, which is the opposite of what it is for.
func TestAutoRecall_HidesSupersededMemories(t *testing.T) {
	s := newRecallStore(t)
	var ids []string
	for _, body := range []string{
		"use pytest for the python tests",
		"actually use pytest with the -x flag",
		"pytest runs from the repo root",
	} {
		id, err := s.WriteMemory("note", body)
		require.NoError(t, err)
		ids = append(ids, id)
	}
	_, err := s.ApplyDistillation(store.MemoryDistillation{
		SourceIDs: ids,
		Content:   "run pytest -x from the repo root",
	})
	require.NoError(t, err)

	// The assertion is on the HITS, not on the rendered text, and that is a
	// correction the mutation probe forced. Disabling the hide-superseded
	// clause in store left the rendered-text version of this test GREEN: four
	// rows then matched, MaxAutoRecall kept three, and the one original the
	// test happened to name for its NotContains check fell outside the top
	// three by relevance. The test was passing on the ranking, not on the
	// property. Checking every returned hit's SupersededBy cannot pass that way.
	hits := AutoRecallHits(context.Background(), s, "how do I run the pytest suite", store.MemoryFilter{})
	require.NotEmpty(t, hits, "the consolidated memory is not findable")
	for _, h := range hits {
		require.Empty(t, h.SupersededBy,
			"a superseded original was injected (%q); every consolidation would then make "+
				"the block longer and more contradictory, which is the opposite of C13's purpose", h.Content)
	}
	var merged bool
	for _, h := range hits {
		if h.Content == "run pytest -x from the repo root" {
			merged = true
		}
	}
	require.True(t, merged, "the consolidated statement was not injected: %+v", hits)

	got := RenderRecalledMemories(hits)
	if !strings.Contains(got, "consolidated from 3 earlier notes") {
		t.Errorf("the block does not say the entry is a consolidation, so the model over-trusts its wording:\n%s", got)
	}
}

// TestAutoRecallQuery_Table covers the query builder, whose job is to turn a
// sentence into something FTS5 will accept and that can still miss.
func TestAutoRecallQuery_Table(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want func(string) bool
		why  string
	}{
		{
			"drops stop words", "can you help me with the deploy",
			func(q string) bool { return strings.Contains(q, `"deploy"`) && !strings.Contains(q, `"the"`) },
			"a query of stop words matches everything weakly, which is exactly what the threshold then has to reject one by one",
		},
		{
			"ors the terms", "guard permission profile",
			func(q string) bool { return strings.Count(q, " OR ") == 2 },
			"AND over a sentence finds nothing, and 'nothing' is indistinguishable from 'you have no memories'",
		},
		{
			"quotes every term", `NOT * "unbalanced`,
			func(q string) bool { return q == "" || !strings.Contains(q, `NOT *`) },
			"a bare FTS5 operator in user text would be executed as syntax, or crash the query",
		},
		{
			"keeps cjk singles", "压缩",
			func(q string) bool { return strings.Contains(q, `"压缩"`) },
			"a han character is a whole word; the latin minimum length would drop it",
		},
		{
			"bounds the term count", strings.Repeat("alpha beta gamma delta epsilon zeta ", 50),
			func(q string) bool { return strings.Count(q, " OR ")+1 <= maxAutoRecallTerms },
			"a pasted stack trace would otherwise build a thousand-term query",
		},
		{
			"nothing usable", "a to of in", func(q string) bool { return q == "" }, "",
		},
		{
			"empty", "", func(q string) bool { return q == "" }, "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := AutoRecallQuery(tc.in)
			if !tc.want(got) {
				t.Errorf("AutoRecallQuery(%q) = %q (%s)", tc.in, got, tc.why)
			}
		})
	}
}

// TestAutoRecall_DegradesQuietly: this is an unrequested enrichment, so every
// failure has to end with the turn proceeding without it.
func TestAutoRecall_DegradesQuietly(t *testing.T) {
	if got := AutoRecall(context.Background(), nil, "anything at all", store.MemoryFilter{}); got != "" {
		t.Errorf("a nil store produced a block: %q", got)
	}
	s := newRecallStore(t)
	require.NoError(t, s.Close())
	// A closed database must not panic or fail the turn.
	if got := AutoRecall(context.Background(), s, "kubernetes deployment", store.MemoryFilter{}); got != "" {
		t.Errorf("a closed store produced a block: %q", got)
	}
}

// TestRequiredOverlap_ScalesWithTheQuery pins the ratio, which is the shape
// that survives both failure modes the design walked through: an absolute
// score floor (kills every recall on a small memory table, because bm25 is
// corpus-relative) and an absolute term count (kills the correct one-word
// match of a three-word question).
func TestRequiredOverlap_ScalesWithTheQuery(t *testing.T) {
	cases := []struct{ terms, want int }{
		{0, 0},  // nothing asked, nothing can qualify
		{1, 1},  // a one-word question needs its one word
		{2, 1},  //
		{3, 1},  // "format this gofmt" — one shared term is a third of the question
		{6, 2},  //
		{9, 3},  //
		{12, 4}, // twelve words sharing one is coincidence, not a match
	}
	for _, tc := range cases {
		if got := RequiredOverlap(tc.terms); got != tc.want {
			t.Errorf("RequiredOverlap(%d) = %d, want %d", tc.terms, got, tc.want)
		}
	}
}

// TestRelevant_Table exercises the predicate directly, including the two
// shapes the ratio exists to separate.
func TestRelevant_Table(t *testing.T) {
	cases := []struct {
		name    string
		terms   []string
		content string
		want    bool
		why     string
	}{
		{
			"one of three is enough",
			[]string{"format", "gofmt", "thing"},
			"always run gofmt before committing Go code", true,
			"a fixed floor of two would silence the best match in the store",
		},
		{
			"one of twelve is not",
			[]string{"kubernetes", "ingress", "certificate", "renewal", "staging", "cluster",
				"annotation", "controller", "namespace", "secret", "rotation", "deploy"},
			"the deploy pipeline lives in .github/workflows", false,
			"a single shared word out of twelve is how an unrelated note gets injected",
		},
		{
			"four of twelve is",
			[]string{"kubernetes", "ingress", "certificate", "renewal", "staging", "cluster",
				"annotation", "controller", "namespace", "secret", "rotation", "deploy"},
			"kubernetes ingress certificate renewal runs weekly", true, "",
		},
		{"empty query", nil, "anything", false, "nothing asked cannot be answered"},
		{
			"case insensitive", []string{"gofmt"}, "Always Run GOFMT", true,
			"terms are lower-cased on extraction; content must be too",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Relevant(tc.terms, tc.content); got != tc.want {
				t.Errorf("Relevant(%d terms, %q) = %v, want %v (%s)",
					len(tc.terms), tc.content, got, tc.want, tc.why)
			}
		})
	}
}

// TestAutoRecall_RelevanceDoesNotDependOnTableSize is the corpus-independence
// claim stated as an experiment, because it is what the abandoned score
// threshold got wrong: the same question and the same matching note must
// recall on a two-row table and on a thousand-row one.
func TestAutoRecall_RelevanceDoesNotDependOnTableSize(t *testing.T) {
	const question = "how should I run the terraform apply for staging"
	const answer = "terraform apply for staging needs the -var-file=staging.tfvars flag"

	for _, filler := range []int{0, 50, 1000} {
		t.Run(fmt.Sprintf("%d_other_memories", filler), func(t *testing.T) {
			s := newRecallStore(t)
			_, err := s.WriteMemory("pref", answer)
			require.NoError(t, err)
			for i := 0; i < filler; i++ {
				_, err := s.WriteMemory("note", "unrelated observation number "+strconv.Itoa(i))
				require.NoError(t, err)
			}
			got := AutoRecall(context.Background(), s, question, store.MemoryFilter{})
			if !strings.Contains(got, "-var-file=staging.tfvars") {
				t.Errorf("with %d other memories the correct note was not recalled; "+
					"relevance is depending on the corpus size:\n%s", filler, got)
			}
		})
	}
}

// TestRenderRecalledMemories_OldestFirst — the block reads as a timeline, so
// the newest note (the one most likely to supersede the others) sits closest
// to the live request.
func TestRenderRecalledMemories_OldestFirst(t *testing.T) {
	hits := []store.MemoryHit{
		{Memory: store.Memory{Kind: "note", Content: "newest", CreatedAt: 300}},
		{Memory: store.Memory{Kind: "note", Content: "oldest", CreatedAt: 100}},
		{Memory: store.Memory{Kind: "note", Content: "middle", CreatedAt: 200}},
	}
	got := RenderRecalledMemories(hits)
	oi, mi, ni := strings.Index(got, "oldest"), strings.Index(got, "middle"), strings.Index(got, "newest")
	if !(oi < mi && mi < ni) {
		t.Errorf("entries are not oldest-first (oldest@%d middle@%d newest@%d):\n%s", oi, mi, ni, got)
	}
	if got := RenderRecalledMemories(nil); got != "" {
		t.Errorf("no hits rendered %q; a header with no entries claims a recall that did not happen", got)
	}
}

// TestClearMemories_AutoRecallMissesAfterClear is W-D-12's third clause and the
// only one that can tell a real wipe from a bookkeeping one.
//
// It drives the AUTOMATIC retrieval path rather than counting rows, because the
// failure it exists to catch is a memory that is gone from the table a
// /memory-clear looked at and still arriving through the one path that fires
// without being asked: the FTS shadow table is a separate physical table kept
// in sync only by triggers, so "deleted" and "unfindable" are two claims, not
// one.
func TestClearMemories_AutoRecallMissesAfterClear(t *testing.T) {
	s := newRecallStore(t)
	const doomed = "always run gofmt before committing Go code"
	const survivor = "the gofmt hook lives in .githooks/pre-commit"
	_, err := s.WriteMemoryScoped("pref", doomed, store.MemoryFilter{SessionID: "s1"})
	require.NoError(t, err)
	_, err = s.WriteMemoryScoped("note", survivor, store.MemoryFilter{SessionID: "s2"})
	require.NoError(t, err)

	const ask = "can you run the gofmt thing for me"
	before := AutoRecall(context.Background(), s, ask, store.MemoryFilter{})
	require.Contains(t, before, doomed,
		"positive control: the memory must be recallable before the clear")

	n, err := s.ClearMemories(store.MemoryFilter{SessionID: "s1"})
	require.NoError(t, err)
	require.Equal(t, 1, n)

	// Scoped clear: s1 is gone, s2 still answers. Asserting only the empty case
	// would pass just as well if the clear had wiped everything.
	got := AutoRecall(context.Background(), s, ask, store.MemoryFilter{})
	require.NotContains(t, got, doomed, "a cleared memory came back through auto-recall")
	require.Contains(t, got, survivor, "clearing one session must not silence the others")

	n, err = s.ClearMemories(store.MemoryFilter{})
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Empty(t, AutoRecall(context.Background(), s, ask, store.MemoryFilter{}),
		"after clearing everything the automatic path must return nothing")
}
