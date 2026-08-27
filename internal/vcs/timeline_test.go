package vcs

import (
	"fmt"
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/store"
)

// seedConversation writes a session's user/assistant turns into the durable log
// exactly the way the WS layer does, so the ordinal join under test is the same
// join production performs.
func seedConversation(t *testing.T, v *VCS, sessionID string, questions []string) {
	t.Helper()
	if _, err := v.store.DB.Exec(
		"INSERT INTO sessions (id, title, created_at, updated_at) VALUES (?, ?, 0, 0)",
		sessionID, "timeline test"); err != nil {
		t.Fatalf("create session: %v", err)
	}
	for i, q := range questions {
		msgs := []store.Message{
			{Role: store.RoleUser, Content: q},
			// A turn's tool traffic sits between the question and the answer.
			// Including it is the point: the join must count USER messages, not
			// messages, or every ordinal after the first turn is wrong.
			{Role: store.RoleToolCall, ToolName: "fs_write", ToolArgs: `{"path":"x"}`},
			{Role: store.RoleToolResult, ToolCallID: "c", Content: "ok"},
			{Role: store.RoleAssistant, Content: fmt.Sprintf("answer %d", i)},
		}
		if _, _, err := v.store.AppendMessages(sessionID, msgs); err != nil {
			t.Fatalf("append turn %d: %v", i, err)
		}
	}
}

// TestTimeline_LabelsEachSeamWithItsOwnQuestion is V3's acceptance test: seam N
// must carry the question the user asked in turn N, for BOTH seam kinds.
//
// The pre-turn/post-turn pair is what makes this non-trivial: they are sealed
// with different values of the same counter, and a naive implementation labels
// one of the two with the neighbouring turn's question — wrong in a way that
// reads as right.
func TestTimeline_LabelsEachSeamWithItsOwnQuestion(t *testing.T) {
	v, repoID, root := setupSeamTestRepo(t)
	const sessionID = "sess-1"
	questions := []string{
		"rename the config loader",
		"now add a test for it",
		"undo the rename, keep the test",
	}
	seedConversation(t, v, sessionID, questions)

	type want struct {
		kind     SeamKind
		question string
	}
	var wants []want
	for turn, q := range questions {
		commitWith(t, v, repoID, root, "turn work",
			map[string]string{fmt.Sprintf("f%d.txt", turn): fmt.Sprintf("v%d\n", turn)})
		// Pre-turn is sealed with cs.turns BEFORE the increment...
		if _, err := v.SealMainTurnSeam(repoID, sessionID, turn, 0, SeamPreTurn, "pre"); err != nil {
			t.Fatalf("seal pre %d: %v", turn, err)
		}
		commitWith(t, v, repoID, root, "turn result",
			map[string]string{fmt.Sprintf("f%d.txt", turn): fmt.Sprintf("v%d-done\n", turn)})
		// ...and post-turn with the value AFTER it.
		if _, err := v.SealMainTurnSeam(repoID, sessionID, turn+1, 0, SeamPostTurn, "post"); err != nil {
			t.Fatalf("seal post %d: %v", turn, err)
		}
		wants = append(wants, want{SeamPreTurn, q}, want{SeamPostTurn, q})
	}

	entries, err := v.Timeline(repoID, TimelineOptions{SessionID: sessionID, Limit: 50})
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	if len(entries) != len(wants) {
		t.Fatalf("got %d entries, want %d", len(entries), len(wants))
	}
	// Timeline is newest-first; wants is oldest-first.
	for i, w := range wants {
		got := entries[len(entries)-1-i]
		if got.Kind != w.kind {
			t.Errorf("entry %d kind = %s, want %s", i, got.Kind, w.kind)
		}
		if got.Question != w.question {
			t.Errorf("entry %d (%s) question = %q, want %q",
				i, got.Kind, got.Question, w.question)
		}
	}
}

// TestTimeline_HeadAndFileCounts pins the two remaining columns: which entry is
// where the working copy stands, and how big each step was.
func TestTimeline_HeadAndFileCounts(t *testing.T) {
	v, repoID, root := setupSeamTestRepo(t)
	commitWith(t, v, repoID, root, "one file", map[string]string{"a.txt": "1\n"})
	if _, err := v.SealMainTurnSeam(repoID, "", 0, 0, SeamPostTurn, "small"); err != nil {
		t.Fatal(err)
	}
	commitWith(t, v, repoID, root, "three files", map[string]string{
		"b.txt": "1\n", "c.txt": "1\n", "d.txt": "1\n",
	})
	if _, err := v.SealMainTurnSeam(repoID, "", 1, 0, SeamPostTurn, "big"); err != nil {
		t.Fatal(err)
	}

	entries, err := v.Timeline(repoID, TimelineOptions{})
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	if !entries[0].IsHead {
		t.Error("the newest seam points at main_head and must be marked IsHead")
	}
	if entries[1].IsHead {
		t.Error("an older seam must not be marked IsHead")
	}
	if entries[0].FilesChanged != 3 {
		t.Errorf("newest FilesChanged = %d, want 3", entries[0].FilesChanged)
	}
	if entries[1].FilesChanged != 1 {
		t.Errorf("older FilesChanged = %d, want 1", entries[1].FilesChanged)
	}
}

// TestTimeline_RevertSeamsAreOptional pins the filter: rollback audit seams
// describe a rollback, not a question, so they are excluded unless asked for —
// and the limit must still be honoured once they are filtered out.
func TestTimeline_RevertSeamsAreOptional(t *testing.T) {
	v, repoID, root := setupSeamTestRepo(t)
	commitWith(t, v, repoID, root, "work", map[string]string{"f.txt": "v1\n"})
	seamID, err := v.SealMainTurnSeam(repoID, "", 0, 0, SeamPreTurn, "pre")
	if err != nil {
		t.Fatal(err)
	}
	commitWith(t, v, repoID, root, "more", map[string]string{"f.txt": "v2\n"})
	if _, err := v.RevertToSeam(repoID, seamID, "test", 0, 0, nil); err != nil {
		t.Fatalf("RevertToSeam: %v", err)
	}

	without, err := v.Timeline(repoID, TimelineOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range without {
		if isRevertSeam(e.Kind) {
			t.Errorf("revert seam %s leaked into the default timeline", e.Kind)
		}
	}
	with, err := v.Timeline(repoID, TimelineOptions{IncludeRevertSeams: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(with) <= len(without) {
		t.Fatalf("IncludeRevertSeams added nothing: %d vs %d", len(with), len(without))
	}
	// The over-fetch exists so filtering cannot silently shorten a limited
	// result. Ask for exactly one and require exactly one.
	one, err := v.Timeline(repoID, TimelineOptions{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(one) != 1 {
		t.Errorf("Limit=1 returned %d entries; the revert-seam filter ate the budget", len(one))
	}
}

// TestTimeline_DegradesHonestlyWithoutASession pins the refusal to invent: a
// VCS-only seam has no conversation, so its question is empty and it is still
// listed.
func TestTimeline_DegradesHonestlyWithoutASession(t *testing.T) {
	v, repoID, root := setupSeamTestRepo(t)
	commitWith(t, v, repoID, root, "agent work", map[string]string{"f.txt": "v1\n"})
	if _, err := v.SealMainTurnSeam(repoID, "", 0, 0, SeamPostTurn, "agent"); err != nil {
		t.Fatal(err)
	}
	entries, err := v.Timeline(repoID, TimelineOptions{})
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].Question != "" {
		t.Errorf("a seam with no session must have no question, got %q", entries[0].Question)
	}
	if entries[0].SeamID == "" || entries[0].CommitID == "" {
		t.Error("an unlabelled seam must still be navigable")
	}
}

// TestTimeline_TurnBeyondTheDurableLogIsUnlabelled covers the other degradation
// path: a seam whose turn number has no corresponding user message (the turn
// predates the durable log, or a revert truncated it away).
func TestTimeline_TurnBeyondTheDurableLogIsUnlabelled(t *testing.T) {
	v, repoID, root := setupSeamTestRepo(t)
	const sessionID = "short"
	seedConversation(t, v, sessionID, []string{"only question"})
	commitWith(t, v, repoID, root, "work", map[string]string{"f.txt": "v1\n"})
	// turnSeq 7 has no seventh user message.
	if _, err := v.SealMainTurnSeam(repoID, sessionID, 7, 0, SeamPostTurn, "far"); err != nil {
		t.Fatal(err)
	}
	entries, err := v.Timeline(repoID, TimelineOptions{SessionID: sessionID})
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	if len(entries) != 1 || entries[0].Question != "" {
		t.Fatalf("entries = %+v; a turn past the log must be unlabelled, not mislabelled", entries)
	}
}

// TestPreviewLine is the unit table for the question preview.
func TestPreviewLine(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		max       int
		want      string
		truncated bool
	}{
		{name: "short single line", in: "hello", max: 10, want: "hello"},
		{name: "first line only", in: "headline\nrest of the prompt", max: 50, want: "headline"},
		{name: "CRLF", in: "headline\r\nrest", max: 50, want: "headline"},
		{name: "collapses whitespace", in: "  a   b \t c  ", max: 50, want: "a b c"},
		{name: "truncates", in: "abcdefghij", max: 4, want: "abcd", truncated: true},
		{name: "trims the cut edge", in: "ab cdefgh", max: 3, want: "ab", truncated: true},
		{
			// A byte cut would split the 3-byte rune and emit U+FFFD in every
			// UI that renders the result.
			name: "cuts on runes not bytes", in: "重构配置加载器并补测试",
			max: 4, want: "重构配置", truncated: true,
		},
		{name: "empty", in: "", max: 10, want: ""},
		{name: "whitespace only", in: "   \n  ", max: 10, want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, truncated := previewLine(tc.in, tc.max)
			if got != tc.want || truncated != tc.truncated {
				t.Errorf("got (%q, %v), want (%q, %v)", got, truncated, tc.want, tc.truncated)
			}
			if n := len([]rune(got)); n > tc.max {
				t.Errorf("result is %d runes, over the %d cap", n, tc.max)
			}
		})
	}
}

// TestTurnOrdinal pins the normalisation across seam kinds.
func TestTurnOrdinal(t *testing.T) {
	tests := []struct {
		kind SeamKind
		seq  int
		want int
	}{
		{kind: SeamPreTurn, seq: 0, want: 1},
		{kind: SeamPreTurn, seq: 4, want: 5},
		{kind: SeamPostTurn, seq: 1, want: 1},
		{kind: SeamPostTurn, seq: 5, want: 5},
		{kind: SeamPreRevert, seq: 3, want: 3},
		{kind: SeamPostRevert, seq: 3, want: 3},
	}
	for _, tc := range tests {
		if got := turnOrdinal(Seam{Kind: tc.kind, TurnSeq: tc.seq}); got != tc.want {
			t.Errorf("%s seq=%d: got %d, want %d", tc.kind, tc.seq, got, tc.want)
		}
	}
}

// TestTimeline_PagesPastTheFirstMessagePage proves the paging loop actually
// advances: a session with more rows than one page must still resolve a
// question that lives beyond it.
//
// store.MaxMessagePageSize caps a single query, so a naive single-call
// implementation silently loses every turn past that boundary — and does so
// only in long sessions, which is exactly where a timeline matters most.
func TestTimeline_PagesPastTheFirstMessagePage(t *testing.T) {
	v, repoID, root := setupSeamTestRepo(t)
	const sessionID = "long"
	// Four rows per turn, so this crosses MaxMessagePageSize comfortably.
	turns := store.MaxMessagePageSize/2 + 5
	questions := make([]string, turns)
	for i := range questions {
		questions[i] = fmt.Sprintf("question number %d", i)
	}
	seedConversation(t, v, sessionID, questions)

	commitWith(t, v, repoID, root, "work", map[string]string{"f.txt": "v1\n"})
	last := turns
	if _, err := v.SealMainTurnSeam(repoID, sessionID, last, 0, SeamPostTurn, "last"); err != nil {
		t.Fatal(err)
	}
	entries, err := v.Timeline(repoID, TimelineOptions{SessionID: sessionID, Limit: 1})
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	want := questions[last-1]
	if entries[0].Question != want {
		t.Errorf("question = %q, want %q (the paging loop stopped early)",
			entries[0].Question, want)
	}
}

// TestTimeline_TruncationIsReported pins the flag a renderer needs in order to
// append an ellipsis without guessing.
func TestTimeline_TruncationIsReported(t *testing.T) {
	v, repoID, root := setupSeamTestRepo(t)
	const sessionID = "verbose"
	long := strings.Repeat("a very long prompt ", 40)
	seedConversation(t, v, sessionID, []string{long})
	commitWith(t, v, repoID, root, "work", map[string]string{"f.txt": "v1\n"})
	if _, err := v.SealMainTurnSeam(repoID, sessionID, 1, 0, SeamPostTurn, "p"); err != nil {
		t.Fatal(err)
	}
	entries, err := v.Timeline(repoID, TimelineOptions{
		SessionID: sessionID, QuestionPreviewChars: 20,
	})
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries", len(entries))
	}
	if !entries[0].QuestionTruncated {
		t.Error("a cut question must report QuestionTruncated")
	}
	if n := len([]rune(entries[0].Question)); n > 20 {
		t.Errorf("question is %d runes, over the 20 cap", n)
	}
}

// TestTimeline_SessionScopeFilters pins the SessionID option: two sessions in
// one repo must not read each other's questions.
func TestTimeline_SessionScopeFilters(t *testing.T) {
	v, repoID, root := setupSeamTestRepo(t)
	seedConversation(t, v, "s-a", []string{"question A"})
	seedConversation(t, v, "s-b", []string{"question B"})
	commitWith(t, v, repoID, root, "a", map[string]string{"a.txt": "1\n"})
	if _, err := v.SealMainTurnSeam(repoID, "s-a", 1, 0, SeamPostTurn, "a"); err != nil {
		t.Fatal(err)
	}
	commitWith(t, v, repoID, root, "b", map[string]string{"b.txt": "1\n"})
	if _, err := v.SealMainTurnSeam(repoID, "s-b", 1, 0, SeamPostTurn, "b"); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ session, want string }{
		{session: "s-a", want: "question A"},
		{session: "s-b", want: "question B"},
	} {
		entries, err := v.Timeline(repoID, TimelineOptions{SessionID: tc.session})
		if err != nil {
			t.Fatalf("Timeline %s: %v", tc.session, err)
		}
		if len(entries) != 1 || entries[0].Question != tc.want {
			t.Errorf("session %s: entries = %+v, want one labelled %q",
				tc.session, entries, tc.want)
		}
	}

	// Unscoped lists both, each still labelled from its OWN session — the
	// property that makes the repo-wide agent-facing listing correct.
	all, err := v.Timeline(repoID, TimelineOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, e := range all {
		got[e.Question] = true
	}
	if !got["question A"] || !got["question B"] {
		t.Errorf("unscoped timeline questions = %v, want both sessions labelled", got)
	}
}
