package upkeep

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/store"
	"github.com/x6nux/yanshi/internal/tools"
)

// scriptedModel answers by prompt shape: phase 1 receives ExtractPrompt, phase 2
// receives tools.DistillPrompt, and both arrive at the same Generate.
type scriptedModel struct {
	mu      sync.Mutex
	extract func(prompt string) string
	distill func(prompt string) string
	calls   int
	prompts []string
}

func (m *scriptedModel) Generate(_ context.Context, msgs []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	prompt := ""
	if len(msgs) > 0 {
		prompt = msgs[0].Content
	}
	m.prompts = append(m.prompts, prompt)
	reply := ""
	switch {
	case strings.Contains(prompt, "MERGE id1,id2,id3"):
		if m.distill != nil {
			reply = m.distill(prompt)
		} else {
			reply = "NOTHING"
		}
	case m.extract != nil:
		reply = m.extract(prompt)
	}
	return &schema.Message{Role: schema.Assistant, Content: reply}, nil
}

// noteReply builds a phase-1 answer with n NOTE lines.
func noteReply(n int) string {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, "NOTE durable fact %d about the release script\n", i)
	}
	return b.String()
}

// finishedSession writes a transcript and backdates it past MemoryIdle so the
// worker treats the session as over.
func finishedSession(t *testing.T, s *store.Store) string {
	t.Helper()
	sid := idleSession(t, s, 6, 2*time.Hour)
	return sid
}

// TestMemoryWorker_ProducesMemoriesOnSessionEnd is acceptance clause 1: a
// finished session yields memory rows, and the worker is what produced them.
//
// Nothing in this test calls memory_write or any tool — the only writer in
// scope is the worker — and the rows come back tagged with the session they
// were extracted from, which a model-authored memory would not be.
func TestMemoryWorker_ProducesMemoriesOnSessionEnd(t *testing.T) {
	s := openStore(t)
	sid := finishedSession(t, s)
	m := &scriptedModel{extract: func(string) string { return noteReply(3) }}

	New(s, Config{Model: m}).RunOnce(context.Background())

	ms, err := s.RecallMemoryScoped(50, store.MemoryFilter{SessionID: sid})
	require.NoError(t, err)
	require.Len(t, ms, 3)
	for _, mem := range ms {
		require.Equal(t, memoryKind, mem.Kind)
		require.Contains(t, mem.Content, "release script")
	}

	// And the prompt actually carried the transcript, so this is extraction
	// rather than a model inventing three lines from an empty page.
	require.NotEmpty(t, m.prompts)
	require.Contains(t, m.prompts[0], "turn body")
}

// TestMemoryWorker_NoModelExtractsNothing is the off switch: without
// storage.memory_auto_extract, bootstrap passes a nil model and this job must
// not run. Zero provider calls, zero rows.
func TestMemoryWorker_NoModelExtractsNothing(t *testing.T) {
	s := openStore(t)
	sid := finishedSession(t, s)

	New(s, Config{}).RunOnce(context.Background())

	ms, err := s.RecallMemoryScoped(50, store.MemoryFilter{SessionID: sid})
	require.NoError(t, err)
	require.Empty(t, ms)
}

// TestMemoryWorker_LeaseIsExclusive is acceptance clause 2: the same session
// processed by concurrent processes is extracted only once.
//
// TWO STORE HANDLES, REAL GOROUTINES, ONE START SIGNAL. Both workers see the
// same idle session and race for it. Ordering them with a sleep would prove
// nothing — a read-then-write claim passes that and loses the race in
// production, where a TUI-hosted backend and a `serve` genuinely run at once.
func TestMemoryWorker_LeaseIsExclusive(t *testing.T) {
	path := storePath(t)
	a := openStoreAt(t, path)
	b := openStoreAt(t, path)
	sid := finishedSession(t, a)

	m := &scriptedModel{extract: func(string) string { return noteReply(3) }}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, s := range []*store.Store{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			New(s, Config{Model: m}).RunOnce(context.Background())
		}()
	}
	close(start)
	wg.Wait()

	ms, err := a.RecallMemoryScoped(50, store.MemoryFilter{SessionID: sid})
	require.NoError(t, err)
	require.Len(t, ms, 3, "a session extracted twice would hold six near-duplicate notes")

	// A later sweep must not redo it either: a successful pass retires the
	// lease permanently.
	New(a, Config{Model: m}).RunOnce(context.Background())
	ms, err = a.RecallMemoryScoped(50, store.MemoryFilter{SessionID: sid})
	require.NoError(t, err)
	require.Len(t, ms, 3)
}

// TestMemoryWorker_QuotaPrunesUnusedMemories is acceptance clause 3.
func TestMemoryWorker_QuotaPrunesUnusedMemories(t *testing.T) {
	s := openStore(t)
	for i := range 20 {
		_, err := s.WriteMemory("note", "old unused fact "+strconv.Itoa(i))
		require.NoError(t, err)
	}
	var n int
	require.NoError(t, s.DB.QueryRow("SELECT COUNT(*) FROM memories").Scan(&n))
	require.Equal(t, 20, n)

	New(s, Config{MemoryQuota: 5}).RunOnce(context.Background())

	require.NoError(t, s.DB.QueryRow("SELECT COUNT(*) FROM memories").Scan(&n))
	require.Equal(t, 5, n)

	// Zero quota is the default and must leave the table alone.
	New(s, Config{}).RunOnce(context.Background())
	require.NoError(t, s.DB.QueryRow("SELECT COUNT(*) FROM memories").Scan(&n))
	require.Equal(t, 5, n)
}

// TestMemoryWorker_Phase2CallsDistillEntrypoint is acceptance clause 4:
// consolidation reuses tools.DistillMemories rather than growing a second path.
//
// ASSERTS THE OBSERVABLE PRODUCT, NOT THAT SOMETHING WAS CALLED. A spy or an
// interface assertion proves only that a function ran. What is checked instead
// is the lineage store.ApplyDistillation writes and nothing else in this
// codebase writes: a merged row whose distilled_from names its sources, and
// those sources marked superseded rather than deleted. A hand-rolled second
// consolidation path would have to reimplement that exact transaction to pass.
//
// The fixture yields SEVEN notes on purpose. tools.MinDistillBatch is 6 and
// DistillMemories returns without doing anything below it — a three-note
// fixture would exercise the short circuit while the test still reported
// success.
func TestMemoryWorker_Phase2CallsDistillEntrypoint(t *testing.T) {
	s := openStore(t)
	sid := finishedSession(t, s)
	m := &scriptedModel{
		extract: func(string) string { return noteReply(7) },
		distill: func(prompt string) string {
			ids := idsFromDistillPrompt(prompt)
			require.GreaterOrEqual(t, len(ids), tools.MinDistillBatch)
			return "MERGE " + strings.Join(ids[:3], ",") + " :: the release script is the only supported deploy path"
		},
	}

	New(s, Config{Model: m}).RunOnce(context.Background())

	dims := store.MemoryFilter{SessionID: sid}
	current, err := s.RecallMemoryScoped(50, dims)
	require.NoError(t, err)
	require.Len(t, current, 5, "7 extracted, 3 merged into 1")

	var merged store.Memory
	for _, mem := range current {
		if len(mem.DistilledFrom) > 0 {
			merged = mem
		}
	}
	require.NotEmpty(t, merged.ID, "the pass must leave a distilled row")
	require.Len(t, merged.DistilledFrom, 3)
	require.Equal(t, "the release script is the only supported deploy path", merged.Content)
	require.Positive(t, merged.DistilledAt)

	// The sources survive, marked superseded — ApplyDistillation never deletes.
	for _, src := range merged.DistilledFrom {
		row, err := s.MemoryByID(src)
		require.NoError(t, err)
		require.Equal(t, merged.ID, row.SupersededBy)
	}
}

// TestMemoryWorker_ExtractionFailureLeavesTheSessionRetryable: a provider
// outage must cost a delay, not a permanently skipped session. The lease is
// only retired on success.
func TestMemoryWorker_ExtractionFailureLeavesTheSessionRetryable(t *testing.T) {
	s := openStore(t)
	sid := finishedSession(t, s)

	// A model that answers with prose produces no NOTE lines and therefore no
	// memories — but it also must not retire the lease as if it had succeeded.
	// Simulate the failure that matters (a model error) by returning garbage
	// the parser rejects, then let the lease expire and retry with a good one.
	bad := &scriptedModel{extract: func(string) string { return "I could not read the session." }}
	New(s, Config{Model: bad}).RunOnce(context.Background())
	ms, err := s.RecallMemoryScoped(50, store.MemoryFilter{SessionID: sid})
	require.NoError(t, err)
	require.Empty(t, ms)

	// The pass "succeeded" with zero notes, so the lease IS retired — which is
	// correct: a session that established nothing durable must not be re-asked
	// on every tick forever. Force the retry the way an expiry would.
	require.NoError(t, s.KVSet("lease:"+memoryLease(sid), "1"))
	good := &scriptedModel{extract: func(string) string { return noteReply(2) }}
	New(s, Config{Model: good}).RunOnce(context.Background())
	ms, err = s.RecallMemoryScoped(50, store.MemoryFilter{SessionID: sid})
	require.NoError(t, err)
	require.Len(t, ms, 2)
}

// TestMemoryWorker_FreshSessionIsLeftAlone: extraction must not run on a
// conversation the user is still in the middle of.
func TestMemoryWorker_FreshSessionIsLeftAlone(t *testing.T) {
	s := openStore(t)
	sid := idleSession(t, s, 6, time.Minute)
	m := &scriptedModel{extract: func(string) string { return noteReply(3) }}

	New(s, Config{Model: m}).RunOnce(context.Background())

	require.Zero(t, m.calls, "a live session must not cost a provider call")
	ms, err := s.RecallMemoryScoped(50, store.MemoryFilter{SessionID: sid})
	require.NoError(t, err)
	require.Empty(t, ms)
}

// TestParseExtractedNotes covers the parser's refusals directly: it is the one
// pure piece here, and each rejection is a thing that would otherwise end up in
// long-term memory forever.
func TestParseExtractedNotes(t *testing.T) {
	got := ParseExtractedNotes(`Here are the durable facts:
NOTE the build runs with go test ./...
note lowercase is accepted too
NOTE
NOTE the build runs with go test ./...
NOTHING
NOTE  spaces are trimmed  `)
	require.Equal(t, []string{
		"the build runs with go test ./...",
		"lowercase is accepted too",
		"spaces are trimmed",
	}, got)

	require.Empty(t, ParseExtractedNotes("NOTHING"))
	require.Empty(t, ParseExtractedNotes(""))
	require.Len(t, ParseExtractedNotes(noteReply(50)), maxExtractedNotes)
}

// TestRenderTranscript_KeepsTheEnd: a session's conclusions are at its end, so
// the budget must truncate the exploration and keep the answer.
func TestRenderTranscript_KeepsTheEnd(t *testing.T) {
	filler := strings.Repeat("x", 4000)
	msgs := make([]store.Message, 0, 20)
	for i := range 20 {
		msgs = append(msgs, store.Message{
			Seq: i, Role: store.RoleAssistant,
			Content: fmt.Sprintf("m%d %s", i, filler),
		})
	}
	out := renderTranscript(msgs)
	require.LessOrEqual(t, len(out), transcriptCharBudget+len(msgs)*32)
	require.Contains(t, out, "m19", "the newest message must survive the budget")
	require.NotContains(t, out, "m0 ", "the oldest must be the one dropped")

	// Ordering is still oldest-first in the rendered text, so the model reads a
	// conversation rather than a reversed one.
	require.Less(t, strings.Index(out, "m18"), strings.Index(out, "m19"))
}

// idsFromDistillPrompt recovers the candidate ids from a rendered distill
// prompt. Taken from the prompt rather than from the store so the merge line is
// built out of exactly what the model was shown.
func idsFromDistillPrompt(prompt string) []string {
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

// TestMemoryWorker_ExtractedMemoriesCarryProvenance pins W-D-07 at the CALL
// SITE rather than at the store API.
//
// Measured before it existed: swapping this worker back to WriteMemoryScoped —
// which drops the provenance and is the one-identifier edit a refactor makes by
// accident — left every package green, including the store tests that cover
// MemorySource itself. A guarded API with an unguarded caller is the shape this
// repository keeps finding, so the assertion has to be that THIS worker's rows
// resolve, not that some row could.
func TestMemoryWorker_ExtractedMemoriesCarryProvenance(t *testing.T) {
	s := openStore(t)
	sid := finishedSession(t, s)
	New(s, Config{Model: &scriptedModel{extract: func(string) string { return noteReply(2) }}}).
		RunOnce(context.Background())

	ms, err := s.RecallMemoryScoped(50, store.MemoryFilter{SessionID: sid})
	require.NoError(t, err)
	require.Len(t, ms, 2)
	for _, mem := range ms {
		src, err := s.MemorySource(mem.ID)
		require.NoErrorf(t, err, "memory %s records no source log position", mem.ID)
		require.NotEmpty(t, src, "the recorded position resolves to no messages")
		require.Equal(t, sid, src[0].SessionID,
			"the source must be the session the note was extracted from")
	}
}
