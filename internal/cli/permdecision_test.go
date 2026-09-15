package cli

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/proto"
)

// permBackend scripts a turn that BLOCKS on a permission request, which is what
// a real server does when a tool cannot be resolved by itself. fakeBackend only
// emits agent_chunk/done, so it cannot express the case this file is about.
type permBackend struct {
	req StreamEvent

	mu     sync.Mutex
	frames []proto.ClientFrame
}

func (b *permBackend) Send(ctx context.Context, text string) (<-chan StreamEvent, error) {
	return b.SendTurn(ctx, proto.NewUserMessage(text))
}

func (b *permBackend) SendTurn(_ context.Context, f proto.ClientFrame) (<-chan StreamEvent, error) {
	b.mu.Lock()
	b.frames = append(b.frames, f)
	b.mu.Unlock()
	ch := make(chan StreamEvent, 3)
	go func() {
		defer close(ch)
		ch <- b.req
		ch <- StreamEvent{Kind: "agent_chunk", Text: "ok"}
		ch <- StreamEvent{Kind: "done"}
	}()
	return ch, nil
}

func (b *permBackend) SendFrame(_ context.Context, f proto.ClientFrame) (<-chan StreamEvent, error) {
	b.mu.Lock()
	b.frames = append(b.frames, f)
	b.mu.Unlock()
	return nil, nil
}

func (b *permBackend) framesOfType(t string) []proto.ClientFrame {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []proto.ClientFrame
	for _, f := range b.frames {
		if f.Type == t {
			out = append(out, f)
		}
	}
	return out
}

func (b *permBackend) Cancel() error { return nil }
func (b *permBackend) Close() error  { return nil }
func (b *permBackend) Mode() string  { return "perm" }

// TestHeadlessDeniesPermissionRequestsInsteadOfHanging pins the behaviour that
// makes an unattended run survivable: a request the server could not resolve by
// itself is answered immediately, so the model is told "denied" and continues.
//
// Before this, execWithBackend ignored permission_request entirely: the server
// held the turn open until its 60s deadline, then failed the whole run with exit
// 1 and no answer — measured against a real provider (the model asked to write a
// file, was denied, asked for permission, and the run died instead of continuing).
func TestHeadlessDeniesPermissionRequestsInsteadOfHanging(t *testing.T) {
	b := &permBackend{req: StreamEvent{
		Kind:        "permission_request",
		ID:          "7",
		Reason:      "no paths permitted for op \"write\"",
		ForcePrompt: true,
	}}
	var out, errBuf strings.Builder
	res, err := execWithBackend(context.Background(), b, ExecOptions{
		Prompt: "write a file",
		Output: ExecOutputText,
		Stdout: &out,
		Stderr: &errBuf,
	})
	require.NoError(t, err)
	assert.Equal(t, "ok", out.String())

	answers := b.framesOfType("permission_response")
	require.Len(t, answers, 1, "one request must produce exactly one answer")
	assert.Equal(t, "7", answers[0].ID, "the answer must echo the request id")
	assert.Equal(t, "deny", answers[0].Decision)
	_ = res
}

// TestHeadlessSendsTheRequestedModeBeforeTheTurn pins that -mode reaches the
// server as a control frame rather than being a client-side no-op: yolo/auto do
// their work in the server's resolver.
func TestHeadlessSendsTheRequestedModeBeforeTheTurn(t *testing.T) {
	b := &permBackend{req: StreamEvent{Kind: "agent_chunk", Text: "unused"}}
	_, err := execWithBackend(context.Background(), b, ExecOptions{
		Prompt: "hi", Output: ExecOutputText, Mode: "yolo",
		Stdout: io.Discard, Stderr: io.Discard,
	})
	require.NoError(t, err)
	modes := b.framesOfType("set_mode")
	require.Len(t, modes, 1)
	assert.Equal(t, "yolo", modes[0].Mode)
	// And the mode frame must precede the turn, or the first turn would run under
	// the previous mode.
	b.mu.Lock()
	defer b.mu.Unlock()
	var order []string
	for _, f := range b.frames {
		order = append(order, f.Type)
	}
	assert.Equal(t, []string{"set_mode", "user_message"}, order)
}

// TestHeadlessPermissionDecisionIsFailClosed documents WHY the answer is always
// deny: every request that reaches a client is one written for a human.
func TestHeadlessPermissionDecisionIsFailClosed(t *testing.T) {
	for _, ev := range []StreamEvent{
		{Kind: "permission_request", ID: "1"},
		{Kind: "permission_request", ID: "2", ForcePrompt: true},
		{Kind: "permission_request", ID: "3", ApprovalRequired: true},
	} {
		assert.Equal(t, "deny", HeadlessPermissionDecision(ev, ApprovalNever))
	}
}

// TestHeadlessApprovalPolicies pins what each policy is willing to answer.
//
// The distinction the test protects: "required" exists because irreversible
// external effects are otherwise unreachable from EVERY unattended surface, and
// it must still refuse force-prompt tools — those are the ones a prior approval
// is forbidden to answer, and a policy that covered them would delete the
// reason the category exists.
func TestHeadlessApprovalPolicies(t *testing.T) {
	plain := StreamEvent{Kind: "permission_request", ID: "1"}
	required := StreamEvent{Kind: "permission_request", ID: "2", ApprovalRequired: true}
	forced := StreamEvent{Kind: "permission_request", ID: "3", ForcePrompt: true}

	cases := []struct {
		policy ApprovalPolicy
		ev     StreamEvent
		want   string
	}{
		{ApprovalNever, plain, "deny"},
		{ApprovalNever, required, "deny"},
		{ApprovalNever, forced, "deny"},
		{ApprovalRequired, plain, "deny"},
		{ApprovalRequired, required, "allow"},
		// ForcePrompt wins over ApprovalRequired: a tool that is BOTH must still
		// be denied, because force-prompt means "never answer this from a rule".
		{ApprovalRequired, StreamEvent{Kind: "permission_request", ID: "4", ApprovalRequired: true, ForcePrompt: true}, "deny"},
		{ApprovalAll, plain, "allow"},
		{ApprovalAll, required, "allow"},
		{ApprovalAll, forced, "allow"},
	}
	for _, c := range cases {
		if got := HeadlessPermissionDecision(c.ev, c.policy); got != c.want {
			t.Errorf("policy=%s event=%+v: got %q, want %q", c.policy, c.ev, got, c.want)
		}
	}
	// A policy must never mint a persistent rule: "always_allow" would outlive
	// the run that asked for unattended operation.
	for _, p := range []ApprovalPolicy{ApprovalNever, ApprovalRequired, ApprovalAll} {
		if got := HeadlessPermissionDecision(plain, p); got == "always_allow" {
			t.Errorf("policy %s returned always_allow", p)
		}
	}
}

// TestParseApprovalPolicyRejectsTypos proves a typo cannot silently pick a
// policy: one direction would make a workflow mysteriously stop, the other
// would make an unattended agent approve things nobody intended.
func TestParseApprovalPolicyRejectsTypos(t *testing.T) {
	for _, ok := range []string{"never", "required", "all"} {
		if _, valid := ParseApprovalPolicy(ok); !valid {
			t.Errorf("ParseApprovalPolicy(%q) rejected a valid policy", ok)
		}
	}
	for _, bad := range []string{"", "always", "Required", "yes"} {
		if _, valid := ParseApprovalPolicy(bad); valid {
			t.Errorf("ParseApprovalPolicy(%q) accepted an invalid policy", bad)
		}
	}
}
