// internal/tools/memory_distill_test.go
package tools

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/store"
)

// distillFake is a scripted consolidation model. reply is templated by the
// caller against the ids the store actually generated, which is the only way
// to script a merge — the ids are random.
type distillFake struct {
	reply func(prompt string) string
	err   error
	calls int
	seen  string
}

func (f *distillFake) Generate(_ context.Context, msgs []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	f.calls++
	if len(msgs) > 0 {
		f.seen = msgs[0].Content
	}
	if f.err != nil {
		return nil, f.err
	}
	return &schema.Message{Role: schema.Assistant, Content: f.reply(f.seen)}, nil
}

// seedDistillable writes n memories and returns their ids in write order.
func seedDistillable(t *testing.T, s *store.Store, n int) []string {
	t.Helper()
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		id, err := s.WriteMemory("note", "memory number "+strconv.Itoa(i))
		require.NoError(t, err)
		ids = append(ids, id)
	}
	return ids
}

// idsFromPrompt pulls the candidate ids back out of the rendered prompt. Doing
// it this way rather than reusing the ids the test wrote is what proves the
// prompt actually SHOWS the model the ids it is asked to answer with — a
// renderer that omitted them would leave the model unable to produce a valid
// merge, and a test holding its own copy would never notice.
func idsFromPrompt(prompt string) []string {
	var out []string
	for _, line := range strings.Split(prompt, "\n") {
		if !strings.HasPrefix(line, "[") {
			continue
		}
		if end := strings.Index(line, "]"); end > 1 {
			out = append(out, line[1:end])
		}
	}
	return out
}

// TestDistillMemories_MergesWhatTheModelProposes is the happy path, end to end
// through the real store.
func TestDistillMemories_MergesWhatTheModelProposes(t *testing.T) {
	s := newRecallStore(t)
	seedDistillable(t, s, 8)

	fake := &distillFake{reply: func(prompt string) string {
		ids := idsFromPrompt(prompt)
		return "MERGE " + strings.Join(ids[:3], ",") + " :: the first three memories, consolidated\n" +
			"MERGE " + strings.Join(ids[3:5], ",") + " :: the next two, consolidated"
	}}

	res, err := DistillMemories(context.Background(), s, fake, store.MemoryFilter{})
	require.NoError(t, err)
	require.Equal(t, 8, res.Considered)
	require.Equal(t, 2, res.Groups)
	require.Equal(t, 5, res.Merged)
	require.Empty(t, res.Skipped)

	cur, err := s.RecallMemory(50)
	require.NoError(t, err)
	// 8 originals - 5 superseded + 2 merged = 5 current.
	require.Len(t, cur, 5)

	all, err := s.RecallMemoryScoped(50, store.MemoryFilter{IncludeSuperseded: true})
	require.NoError(t, err)
	require.Len(t, all, 10, "originals were deleted rather than superseded")
}

// TestDistillMemories_ModelFailureDeletesNothing is C13's hard requirement at
// this layer. Every way the model call can go wrong must end with the memory
// table exactly as it was.
func TestDistillMemories_ModelFailureDeletesNothing(t *testing.T) {
	cases := []struct {
		name    string
		fake    *distillFake
		wantErr bool
	}{
		{"api error", &distillFake{err: errors.New("502 bad gateway")}, true},
		{"prose answer", &distillFake{reply: func(string) string {
			return "I looked at your memories and they all seem fine to me."
		}}, false},
		{"explicit NOTHING", &distillFake{reply: func(string) string { return "NOTHING" }}, false},
		{"empty answer", &distillFake{reply: func(string) string { return "" }}, false},
		{"hallucinated ids", &distillFake{reply: func(string) string {
			return "MERGE aaaa,bbbb :: merged something that does not exist"
		}}, false},
		{"malformed lines", &distillFake{reply: func(string) string {
			return "MERGE\nMERGE ::\n:: nothing\nMERGE x"
		}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newRecallStore(t)
			ids := seedDistillable(t, s, 8)

			_, err := DistillMemories(context.Background(), s, tc.fake, store.MemoryFilter{})
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			cur, rerr := s.RecallMemory(50)
			require.NoError(t, rerr)
			require.Len(t, cur, len(ids),
				"a failed distillation changed the memory set: %+v", cur)
			for _, m := range cur {
				require.Empty(t, m.SupersededBy, "%q was superseded by a failed pass", m.Content)
			}
		})
	}
}

// TestDistillMemories_NoOpsBelowTheBatchFloor: a fresh session has a handful
// of memories, and a pass over them would merge things that merely happen to
// be adjacent. It must also not spend a model call finding that out.
func TestDistillMemories_NoOpsBelowTheBatchFloor(t *testing.T) {
	s := newRecallStore(t)
	seedDistillable(t, s, MinDistillBatch-1)
	fake := &distillFake{reply: func(string) string { return "NOTHING" }}

	res, err := DistillMemories(context.Background(), s, fake, store.MemoryFilter{})
	require.NoError(t, err)
	require.Zero(t, res.Groups)
	require.Zero(t, res.Considered)
	require.Zero(t, fake.calls, "a model call was spent on a batch too small to consolidate")
}

// TestDistillMemories_NilCollaboratorsAreANoOp — the pass is opportunistic
// maintenance, so an unconfigured model or store must be silence, not an error
// the caller has to special-case.
func TestDistillMemories_NilCollaboratorsAreANoOp(t *testing.T) {
	s := newRecallStore(t)
	seedDistillable(t, s, 10)

	res, err := DistillMemories(context.Background(), s, nil, store.MemoryFilter{})
	require.NoError(t, err)
	require.Zero(t, res.Groups)

	res, err = DistillMemories(context.Background(), nil,
		&distillFake{reply: func(string) string { return "NOTHING" }}, store.MemoryFilter{})
	require.NoError(t, err)
	require.Zero(t, res.Groups)
}

// TestDistillMemories_PromptShowsTheIdsAndTheOldestFirst is the two things the
// model needs to answer usefully: the ids it must copy, and the batch ordered
// so the endangered memories are the ones under consideration.
func TestDistillMemories_PromptShowsTheIdsAndTheOldestFirst(t *testing.T) {
	s := newRecallStore(t)
	ids := seedDistillable(t, s, 8)
	fake := &distillFake{reply: func(string) string { return "NOTHING" }}

	_, err := DistillMemories(context.Background(), s, fake, store.MemoryFilter{})
	require.NoError(t, err)
	require.Equal(t, 1, fake.calls)

	shown := idsFromPrompt(fake.seen)
	require.Equal(t, ids, shown,
		"the prompt does not list the candidate ids in write order; the model cannot "+
			"produce a valid MERGE line without them")
	require.Contains(t, fake.seen, "MERGE id1,id2,id3 ::",
		"the prompt never states the answer format")
	require.Contains(t, fake.seen, "NOTHING",
		"the prompt gives the model no way to decline, so it will invent a merge")
}

// TestParseDistillPlan_Table is the parser's contract: verify or refuse, and
// say why. Every rejection reason is a way a model answer can be wrong in a
// manner Go CAN check, which is the half the prompt cannot enforce.
func TestParseDistillPlan_Table(t *testing.T) {
	known := map[string]store.Memory{
		"a": {ID: "a"}, "b": {ID: "b"}, "c": {ID: "c"}, "d": {ID: "d"},
	}
	cases := []struct {
		name        string
		answer      string
		wantGroups  int
		wantSkipped int
		why         string
	}{
		{"one merge", "MERGE a,b :: both of them", 1, 0, ""},
		{"two merges", "MERGE a,b :: ab\nMERGE c,d :: cd", 2, 0, ""},
		{"nothing", "NOTHING", 0, 0, ""},
		{
			"unknown id rejects the whole line", "MERGE a,zzz :: ab", 0, 1,
			"merging the subset that exists applies a judgement the model did not make",
		},
		{
			"id claimed twice", "MERGE a,b :: ab\nMERGE b,c :: bc", 1, 1,
			"the second claim contradicts the first and there is no way to tell which was meant",
		},
		{
			"single-id merge", "MERGE a :: just a", 0, 1,
			"a merge of one is a rewrite, and this is not the API for rewriting a memory",
		},
		{
			"empty merged text", "MERGE a,b ::   ", 0, 1,
			"replacing two memories with nothing is deletion wearing a merge's clothes",
		},
		{
			"prose around the answer",
			"Sure! Here are the merges:\nMERGE a,b :: ab\nLet me know if you want more.",
			1, 0, "a model that wraps its answer must not lose the answer",
		},
		{
			"lowercase verb", "merge a,b :: ab", 1, 0,
			"case is not a judgement the model should be punished for",
		},
		{"empty answer", "", 0, 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			groups, skipped := ParseDistillPlan(tc.answer, known)
			require.Len(t, groups, tc.wantGroups, "groups (%s)", tc.why)
			require.Len(t, skipped, tc.wantSkipped, "skipped: %v (%s)", skipped, tc.why)
			for _, g := range groups {
				require.GreaterOrEqual(t, len(g.SourceIDs), 2)
				require.NotEmpty(t, strings.TrimSpace(g.Content))
			}
		})
	}
}

// TestDistillMemories_ReportsWhyAGroupWasSkipped: a pass that merged two
// groups and refused a third is the normal shape, and losing the reason for
// the third would make a systematically bad prompt look like a quiet one.
func TestDistillMemories_ReportsWhyAGroupWasSkipped(t *testing.T) {
	s := newRecallStore(t)
	seedDistillable(t, s, 8)
	fake := &distillFake{reply: func(prompt string) string {
		ids := idsFromPrompt(prompt)
		return "MERGE " + strings.Join(ids[:2], ",") + " :: a good merge\n" +
			"MERGE " + ids[2] + ",not-a-real-id :: a bad one"
	}}
	res, err := DistillMemories(context.Background(), s, fake, store.MemoryFilter{})
	require.NoError(t, err)
	require.Equal(t, 1, res.Groups)
	require.Len(t, res.Skipped, 1)
	require.Contains(t, res.Skipped[0], "unknown memory id")
}

// TestDistillMemories_ScopesTheMergedRow — a session-scoped pass must consume
// and produce inside that scope, or consolidation makes a session's memories
// unfindable under that session.
func TestDistillMemories_ScopesTheMergedRow(t *testing.T) {
	s := newRecallStore(t)
	dims := store.MemoryFilter{SessionID: "sess-x"}
	for i := 0; i < 8; i++ {
		_, err := s.WriteMemoryScoped("note", "scoped memory "+strconv.Itoa(i), dims)
		require.NoError(t, err)
	}
	// One memory in another session, which must not be touched.
	otherID, err := s.WriteMemoryScoped("note", "someone else's memory",
		store.MemoryFilter{SessionID: "sess-y"})
	require.NoError(t, err)

	fake := &distillFake{reply: func(prompt string) string {
		ids := idsFromPrompt(prompt)
		return "MERGE " + strings.Join(ids[:3], ",") + " :: three scoped memories"
	}}
	res, err := DistillMemories(context.Background(), s, fake, dims)
	require.NoError(t, err)
	require.Equal(t, 8, res.Considered, "the other session's memory entered the batch")
	require.Equal(t, 1, res.Groups)

	other, err := s.MemoryByID(otherID)
	require.NoError(t, err)
	require.Empty(t, other.SupersededBy, "another session's memory was superseded")

	scoped, err := s.RecallMemoryScoped(50, dims)
	require.NoError(t, err)
	var found bool
	for _, m := range scoped {
		if m.Content == "three scoped memories" {
			found = true
		}
	}
	require.True(t, found, "the merged row is not findable under the scope it was distilled from")
}

// TestDistillPrompt_StatesTheRulesTheParserCannotCheck. The judgement calls
// live only in the prompt: nothing in Go can verify that two notes are about
// the same subject, so deleting a clause here breaks the feature without
// failing a compile or any other test.
func TestDistillPrompt_StatesTheRulesTheParserCannotCheck(t *testing.T) {
	for _, want := range []string{
		"SAME subject",  // the merge criterion itself
		"CURRENT",       // keep the later state, not the history of the change
		"Preserve",      // identifiers must survive verbatim
		"Do NOT invent", // no fabrication
		"Leave a note alone",
		"NOTHING", // the decline path
	} {
		require.Contains(t, DistillPrompt, want,
			"the prompt no longer states %q; the parser cannot enforce it, so nothing else will", want)
	}
}
