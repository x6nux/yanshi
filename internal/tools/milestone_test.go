// internal/tools/milestone_test.go
package tools

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/store"
)

// TestMilestoneSet_ReturnsAnAddressThatResolves is the whole contract: the seq
// the tool reports back must be the seq the note actually landed at, because
// the model is told to cite it and history_read is what will be asked to
// resolve the citation.
func TestMilestoneSet_ReturnsAnAddressThatResolves(t *testing.T) {
	s := newHistoryStore(t)
	sid, err := s.CreateSession("milestones")
	require.NoError(t, err)
	ctx := historyCtx(t, sid)

	mt := NewMilestoneTools(s)
	ht := NewHistoryTools(s)

	out, err := runHistoryTool(t, ctx, mt.runSet,
		map[string]any{"text": "fixed the nil deref in lexShellLite; guard tests green"})
	require.NoError(t, err)

	seq := seqFromMilestoneReply(t, out)
	read, err := runHistoryTool(t, ctx, ht.runRead,
		map[string]any{"from_seq": seq, "to_seq": seq + 1})
	require.NoError(t, err)
	require.Contains(t, read, "fixed the nil deref in lexShellLite",
		"the seq the tool handed back does not return the milestone it wrote")
}

// TestMilestoneSet_RepeatedIdenticalTextGetsDistinctAddresses is the bug the
// dedup probe found, pinned.
//
// AppendMessages derives an idempotency key from the message content, which is
// right for the WS layer (it re-flushes the whole window before every
// eviction) and wrong here: recording the same label at two points is two real
// events. Without an explicit key the second call inserts nothing, the
// watermark does not move, and the seq handed back names whatever message now
// sits there — a different one, once anything was written in between. The
// model then cites a pointer to somebody else's text.
func TestMilestoneSet_RepeatedIdenticalTextGetsDistinctAddresses(t *testing.T) {
	s := newHistoryStore(t)
	sid, err := s.CreateSession("repeat")
	require.NoError(t, err)
	ctx := historyCtx(t, sid)
	mt := NewMilestoneTools(s)
	ht := NewHistoryTools(s)

	const label = "ran the test suite"
	var seqs []int
	for i := 0; i < 3; i++ {
		out, err := runHistoryTool(t, ctx, mt.runSet, map[string]any{"text": label})
		require.NoError(t, err)
		seqs = append(seqs, seqFromMilestoneReply(t, out))
		// Something else lands between the milestones, which is what turns a
		// stale watermark from "same answer" into "wrong answer".
		_, _, err = s.AppendMessages(sid, []store.Message{
			{Role: store.RoleAssistant, Content: "unrelated reply " + strconv.Itoa(i)},
		})
		require.NoError(t, err)
	}
	seen := map[int]bool{}
	for i, seq := range seqs {
		require.False(t, seen[seq], "milestone %d reported seq %d, already used by an earlier one", i, seq)
		seen[seq] = true

		read, err := runHistoryTool(t, ctx, ht.runRead,
			map[string]any{"from_seq": seq, "to_seq": seq + 1})
		require.NoError(t, err)
		require.Contains(t, read, label,
			"milestone %d's reported seq %d resolves to something else entirely:\n%s", i, seq, read)
		require.NotContains(t, read, "unrelated reply",
			"the reported seq points at an ordinary message, not the milestone")
	}
}

// TestMilestoneSet_Rejections — each is a label that would degrade the index
// rather than populate it.
func TestMilestoneSet_Rejections(t *testing.T) {
	s := newHistoryStore(t)
	sid, err := s.CreateSession("rejections")
	require.NoError(t, err)
	ctx := historyCtx(t, sid)
	mt := NewMilestoneTools(s)

	cases := []struct {
		name string
		text string
		want string
	}{
		{"empty", "", "required"},
		{"whitespace only", "   \n\t ", "required"},
		{"over the length cap", strings.Repeat("x", MaxMilestoneText+1), "keep it under"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := runHistoryTool(t, ctx, mt.runSet, map[string]any{"text": tc.text})
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
		})
	}
	// Nothing was written by any of them.
	msgs, err := s.MessagesPage(store.MessageRange{SessionID: sid, Limit: 50})
	require.NoError(t, err)
	require.Empty(t, msgs, "a rejected milestone was stored anyway: %+v", msgs)
}

// TestMilestoneSet_FlattensMultilineText — the eviction map is a line-oriented
// directory, so a label with a newline in it would render as two entries, the
// second attached to no span.
func TestMilestoneSet_FlattensMultilineText(t *testing.T) {
	s := newHistoryStore(t)
	sid, err := s.CreateSession("flatten")
	require.NoError(t, err)

	out, err := runHistoryTool(t, historyCtx(t, sid), NewMilestoneTools(s).runSet,
		map[string]any{"text": "first part\nsecond part\n\tthird"})
	require.NoError(t, err)
	seq := seqFromMilestoneReply(t, out)

	msgs, err := s.MessagesPage(store.MessageRange{SessionID: sid, FromSeq: seq, ToSeq: seq + 1})
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	require.NotContains(t, msgs[0].Content, "\n")
	require.Equal(t, "first part second part third", msgs[0].Content)
}

// TestMilestoneSet_RequiresASession is the same rule history_search follows:
// with no conversation bound there is nothing to annotate, and inventing a
// session would write the note somewhere nobody reads.
func TestMilestoneSet_RequiresASession(t *testing.T) {
	s := newHistoryStore(t)
	_, err := runHistoryTool(t, historyCtx(t, ""), NewMilestoneTools(s).runSet,
		map[string]any{"text": "a label with nowhere to go"})
	require.Error(t, err)
}

// TestMilestoneSet_IsFindableByHistorySearch — the reason the note is a
// durable message rather than a side table. FTS indexing, seq addressing, fork
// and delete semantics all come for free; a side table would reimplement them
// and get some of them wrong.
func TestMilestoneSet_IsFindableByHistorySearch(t *testing.T) {
	s := newHistoryStore(t)
	sid, err := s.CreateSession("searchable")
	require.NoError(t, err)
	ctx := historyCtx(t, sid)

	_, err = runHistoryTool(t, ctx, NewMilestoneTools(s).runSet,
		map[string]any{"text": "switched the parser to a recursive descent implementation"})
	require.NoError(t, err)

	out, err := runHistoryTool(t, ctx, NewHistoryTools(s).runSearch,
		map[string]any{"query": "recursive descent"})
	require.NoError(t, err)
	// The FTS snippet wraps matched terms in «», so the phrase is not
	// contiguous in the output; assert on the surrounding words instead.
	require.Contains(t, out, "switched the parser to a",
		"a milestone is not reachable by history_search, so the label is write-only")
	require.Contains(t, out, "implementation")
	require.Contains(t, out, RoleMilestone,
		"the milestone is not distinguishable from ordinary prose in search results")
}

// TestMilestoneTools_NameIsRegisterable pins the tool name, which appears in
// the composition root's registration and in the profile allow list. A rename
// here without those makes the tool fail-closed at runtime (S8) and reddens
// GOV5.
func TestMilestoneTools_NameIsRegisterable(t *testing.T) {
	got := NewMilestoneTools(newHistoryStore(t)).Tools()
	require.Len(t, got, 1)
	info, err := got[0].Info(context.Background())
	require.NoError(t, err)
	require.Equal(t, "milestone_set", info.Name)
}

// seqFromMilestoneReply extracts the seq out of the tool's reply text, the way
// a model reading that reply would.
func seqFromMilestoneReply(t *testing.T, reply string) int {
	t.Helper()
	const marker = "at seq "
	i := strings.Index(reply, marker)
	require.GreaterOrEqual(t, i, 0, "the reply does not report a seq: %q", reply)
	rest := reply[i+len(marker):]
	end := strings.IndexAny(rest, ". ")
	require.Greater(t, end, 0, "the reply's seq is unparseable: %q", reply)
	n, err := strconv.Atoi(rest[:end])
	require.NoError(t, err, "the reply's seq is not a number: %q", reply)
	return n
}
