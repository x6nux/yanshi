package eino

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/cloudwego/eino/schema"
)

// TestQuirkFromError is the M5 detection table. Each row is a provider message
// yanshi has to act on (or deliberately not act on) and the quirk it implies.
func TestQuirkFromError(t *testing.T) {
	cases := []struct {
		name  string
		err   error
		want  Quirk
		wantK bool
	}{
		{
			name:  "deepseek missing reasoning_content",
			err:   errors.New(`error, status code: 400, message: assistant message must contain reasoning_content`),
			want:  QuirkNeedsReasoningContent,
			wantK: true,
		},
		{
			name:  "phrase form of the same rejection",
			err:   errors.New(`HTTP 400: reasoning content is required for thinking models`),
			want:  QuirkNeedsReasoningContent,
			wantK: true,
		},
		{
			name:  "gateway rejects $ref in a tool schema",
			err:   errors.New(`status code: 400, message: tools[3].function.parameters: $ref is not supported`),
			want:  QuirkRejectsToolSchemaRefs,
			wantK: true,
		},
		{
			name:  "vllm rejects an unknown schema keyword",
			err:   errors.New(`HTTP 400: unsupported schema keyword: prefixItems`),
			want:  QuirkRejectsToolSchemaRefs,
			wantK: true,
		},
		{
			name:  "chat template refuses the system role",
			err:   errors.New(`status code: 400, message: role 'system' is not supported by this model`),
			want:  QuirkRejectsSystemRole,
			wantK: true,
		},
		{
			name:  "double-quoted variant of the system-role refusal",
			err:   errors.New(`HTTP 400: role "system" is not allowed here`),
			want:  QuirkRejectsSystemRole,
			wantK: true,
		},
		{
			// The whole reason detection is gated on ClassClientError: a 5xx
			// whose body quotes the rejected request must not teach us
			// anything, or one outage permanently changes how a model is
			// addressed.
			name:  "a 500 quoting reasoning_content teaches nothing",
			err:   errors.New(`status code: 500, message: upstream error: reasoning_content missing`),
			wantK: false,
		},
		{
			name:  "a 429 quoting a schema teaches nothing",
			err:   errors.New(`status code: 429, message: too many requests while validating $ref`),
			wantK: false,
		},
		{
			name:  "an ordinary 400 with no marker matches nothing",
			err:   errors.New(`status code: 400, message: invalid_request_error: missing field 'messages'`),
			wantK: false,
		},
		{
			// A context overflow classifies as ClassContextOverflow, not
			// ClassClientError, so it can never be mistaken for a quirk — C6
			// owns it.
			name:  "context overflow is not a quirk",
			err:   errors.New(`status code: 400, message: maximum context length is 8192 tokens`),
			wantK: false,
		},
		{name: "nil error", err: nil, wantK: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := QuirkFromError(tc.err)
			if ok != tc.wantK {
				t.Fatalf("QuirkFromError ok = %v, want %v (got quirk %q)", ok, tc.wantK, got)
			}
			if ok && got != tc.want {
				t.Errorf("QuirkFromError = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestQuirkStoreLearnIsIdempotentAndObservable pins the two properties the log
// line depends on: a repeat Learn reports false (so one WARN, not one per
// turn), and the quirk is readable afterwards.
func TestQuirkStoreLearnIsIdempotentAndObservable(t *testing.T) {
	s := NewQuirkStore()
	if s.Has("m", QuirkNeedsReasoningContent) {
		t.Fatal("fresh store already has a quirk")
	}
	if !s.Learn("m", QuirkNeedsReasoningContent, "400: reasoning_content") {
		t.Fatal("first Learn returned false")
	}
	if s.Learn("m", QuirkNeedsReasoningContent, "400: reasoning_content") {
		t.Error("second Learn returned true; the WARN would repeat every turn")
	}
	if !s.Has("m", QuirkNeedsReasoningContent) {
		t.Error("Has = false after Learn")
	}
	if s.Has("other", QuirkNeedsReasoningContent) {
		t.Error("the quirk leaked to a different model")
	}
	if got := s.List("m"); len(got) != 1 || got[0] != QuirkNeedsReasoningContent {
		t.Errorf("List = %v, want one entry", got)
	}
	s.Forget("m")
	if s.Has("m", QuirkNeedsReasoningContent) {
		t.Error("Forget did not clear the model")
	}
}

// TestQuirkStoreNilIsSafe pins that a nil store is a working no-op, so callers
// need no nil checks on the hot path.
func TestQuirkStoreNilIsSafe(t *testing.T) {
	var s *QuirkStore
	if s.Has("m", QuirkNeedsReasoningContent) {
		t.Error("nil store reported a quirk")
	}
	if s.List("m") != nil {
		t.Error("nil store returned a list")
	}
	if s.Learn("m", QuirkNeedsReasoningContent, "x") {
		t.Error("nil store claimed to learn")
	}
	s.Forget("m")
}

// TestQuirkStoreConcurrent drives Learn/Has from many goroutines; run with
// -race this is the only assertion that the mutex is doing its job.
func TestQuirkStoreConcurrent(t *testing.T) {
	s := NewQuirkStore()
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			model := fmt.Sprintf("m%d", i%4)
			s.Learn(model, AllQuirks[i%len(AllQuirks)], "e")
			s.Has(model, QuirkNeedsReasoningContent)
			s.List(model)
		}(i)
	}
	wg.Wait()
}

// TestEveryQuirkHasMarkersAndAnEffect pins the completeness invariant: a quirk
// added to AllQuirks without a detector or without an operator-facing effect
// string would be undetectable or unexplainable, and neither failure has any
// other symptom.
func TestEveryQuirkHasMarkersAndAnEffect(t *testing.T) {
	for _, q := range AllQuirks {
		if len(quirkMarkers[q]) == 0 {
			t.Errorf("quirk %q has no detection markers; it can never be learned", q)
		}
		if quirkEffect(q) == "unknown" {
			t.Errorf("quirk %q has no effect description; the WARN line would not say what changed", q)
		}
	}
	if len(quirkMarkers) != len(AllQuirks) {
		t.Errorf("quirkMarkers has %d entries but AllQuirks has %d; a marker set is unreachable",
			len(quirkMarkers), len(AllQuirks))
	}
}

// TestApplyReasoningContent pins the repair and, critically, that it does NOT
// write through the caller's message pointers: the same slice is the ADK's live
// history, and an in-place placeholder would outlive the model that needed it.
func TestApplyReasoningContent(t *testing.T) {
	orig := []*schema.Message{
		schema.SystemMessage("sys"),
		schema.UserMessage("hi"),
		schema.AssistantMessage("hello", nil),
		{Role: schema.Assistant, Content: "thought", ReasoningContent: "already here"},
	}
	out, changed := applyReasoningContent(orig)
	if !changed {
		t.Fatal("changed = false; the bare assistant message needed a placeholder")
	}
	if out[2].ReasoningContent != reasoningPlaceholder {
		t.Errorf("assistant reasoning = %q, want the placeholder", out[2].ReasoningContent)
	}
	if orig[2].ReasoningContent != "" {
		t.Error("the caller's message was mutated in place")
	}
	if out[3].ReasoningContent != "already here" {
		t.Error("an existing reasoning_content was overwritten")
	}
	if out[0] != orig[0] || out[1] != orig[1] {
		t.Error("untouched messages were copied needlessly")
	}
	// Idempotent: a second pass has nothing to do.
	if _, again := applyReasoningContent(out); again {
		t.Error("applyReasoningContent is not idempotent")
	}
}

// TestApplyNoSystemRole pins the merge: system messages become ONE leading user
// message, because the templates that reject the system role also reject two
// adjacent user turns.
func TestApplyNoSystemRole(t *testing.T) {
	in := []*schema.Message{
		schema.SystemMessage("rules"),
		schema.SystemMessage("more rules"),
		schema.UserMessage("hi"),
	}
	out, changed := applyNoSystemRole(in)
	if !changed {
		t.Fatal("changed = false")
	}
	if len(out) != 2 {
		t.Fatalf("len = %d, want 2 (merged system + user)", len(out))
	}
	if out[0].Role != schema.User {
		t.Errorf("out[0].Role = %v, want user", out[0].Role)
	}
	if out[0].Content != "rules\n\nmore rules" {
		t.Errorf("out[0].Content = %q, want the joined system text", out[0].Content)
	}
	if out[1] != in[2] {
		t.Error("the user message was not preserved")
	}
	// No system messages → unchanged, and the same slice back.
	none := []*schema.Message{schema.UserMessage("hi")}
	got, changed := applyNoSystemRole(none)
	if changed {
		t.Error("changed = true with no system messages")
	}
	if len(got) != 1 {
		t.Errorf("len = %d, want 1", len(got))
	}
}

// TestTruncateEvidence pins the log-line bound: provider 400 bodies routinely
// embed the whole rejected request, and an unbounded evidence field would put
// the conversation into the log at WARN.
func TestTruncateEvidence(t *testing.T) {
	long := make([]byte, maxEvidenceBytes*2)
	for i := range long {
		long[i] = 'x'
	}
	got := truncateEvidence(string(long))
	if len([]byte(got)) <= maxEvidenceBytes {
		t.Fatalf("truncated to %d bytes; expected the cap plus an ellipsis", len(got))
	}
	if len([]rune(got)) != maxEvidenceBytes+1 {
		t.Errorf("got %d runes, want %d + ellipsis", len([]rune(got)), maxEvidenceBytes)
	}
	if got := truncateEvidence("a\nb  c"); got != "a b c" {
		t.Errorf("newlines not collapsed: %q", got)
	}
}
