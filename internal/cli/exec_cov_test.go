package cli

import (
	"context"
	"errors"
	"fmt"
	"github.com/x6nux/yanshi/internal/proto"
	"strings"
	"testing"
)

// errSendBackend is a ChatBackend whose Send always returns an error.
type errSendBackend struct{ fakeExecBackend }

// SendTurn delegates to Send: these fakes exercise the text path only.
// It is on the interface rather than an optional capability so a backend
// that forgets it fails to compile — an optional one would silently drop
// every attachment.
func (e *errSendBackend) SendTurn(ctx context.Context, fr proto.ClientFrame) (<-chan StreamEvent, error) {
	return e.Send(ctx, fr.Text)
}

func (e *errSendBackend) Send(_ context.Context, _ string) (<-chan StreamEvent, error) {
	return nil, fmt.Errorf("send boom")
}

// TestExecWithBackend_SendError covers the b.Send error branch at exec.go:110-112.
func TestExecWithBackend_SendError(t *testing.T) {
	b := &errSendBackend{fakeExecBackend: fakeExecBackend{mode: "ws"}}
	_, err := execWithBackend(context.Background(), b, ExecOptions{
		Prompt: "hi",
	})
	if err == nil || !strings.Contains(err.Error(), "send") {
		t.Fatalf("expected send error, got: %v", err)
	}
}

// TestExec_SessionResolveError covers the sess.Resolve error branch at
// exec.go:51-53 by passing options with a non-existent config path.
func TestExec_SessionResolveError(t *testing.T) {
	_, err := Exec(context.Background(), ExecOptions{
		Options: Options{
			Root:       t.TempDir(),
			ConfigPath: "/nonexistent/path/config.yaml",
		},
		Prompt: "hi",
		Output: ExecOutputText,
	})
	if err == nil {
		t.Fatal("expected resolve error")
	}
}

// TestExecWithBackend_ResumeCtxError covers the ctx.Err() after resume at
// exec.go:103-105 by using a pre-cancelled context.
func TestExecWithBackend_ResumeCtxError(t *testing.T) {
	b := &fakeExecBackend{mode: "ws", sendText: "reply", statusID: "sess-r"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := execWithBackend(ctx, b, ExecOptions{
		Prompt: "hi",
		Resume: "sess-r",
	})
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("expected Canceled, got: %v", err)
	}
}
