package tools

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

func TestStreamFromReader(t *testing.T) {
	ch := make(chan ToolChunk, 32)
	reader := strings.NewReader("line1\nline2\nline3\n")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- streamFromReader(ctx, ch, reader)
		close(ch)
	}()

	var lines []string
	for c := range ch {
		if c.Result != "" {
			lines = append(lines, c.Result)
		}
	}
	err := <-errCh
	if err != nil {
		t.Fatalf("streamFromReader returned error: %v", err)
	}
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d: %v", len(lines), lines)
	}
}

func TestStreamFromReaderContextCancel(t *testing.T) {
	ch := make(chan ToolChunk, 32)
	// Use a pipe that never writes so the reader blocks.
	r, _ := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately.

	err := streamFromReader(ctx, ch, r)
	if err == nil || !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("expected context canceled error, got: %v", err)
	}
}

func TestStreamFromReaderEmpty(t *testing.T) {
	ch := make(chan ToolChunk, 32)
	reader := strings.NewReader("")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- streamFromReader(ctx, ch, reader)
		close(ch)
	}()

	<-ch // Drain until closed.
	err := <-errCh
	if err != nil {
		t.Fatalf("streamFromReader returned error: %v", err)
	}
}

func TestAbsEdgeCases(t *testing.T) {
	ft := NewFSTools(t.TempDir())
	t.Run("empty path", func(t *testing.T) {
		_, err := ft.abs("")
		if err == nil {
			t.Fatal("expected error for empty path")
		}
	})
}

func TestDenyReason(t *testing.T) {
	t.Run("deny err", func(t *testing.T) {
		d := &DenyErr{Reason: "not allowed"}
		r := denyReason(d)
		if r != "not allowed" {
			t.Fatalf("expected 'not allowed', got %q", r)
		}
	})
	t.Run("regular error", func(t *testing.T) {
		r := denyReason(io.EOF)
		if r != io.EOF.Error() {
			t.Fatalf("expected EOF, got %q", r)
		}
	})
}

func TestFriendlyErr(t *testing.T) {
	t.Run("deny err", func(t *testing.T) {
		err := &DenyErr{Reason: "not allowed"}
		msg := friendlyErr(err)
		if msg != "permission denied: not allowed" {
			t.Fatalf("expected 'permission denied: not allowed', got %q", msg)
		}
	})
	t.Run("regular error", func(t *testing.T) {
		msg := friendlyErr(io.EOF)
		if msg != io.EOF.Error() {
			t.Fatalf("expected EOF, got %q", msg)
		}
	})
}

func TestSubAgentEmitAdapter(t *testing.T) {
	t.Run("no emit returns nil", func(t *testing.T) {
		adapter := subagentEmitAdapter(context.Background())
		if adapter != nil {
			t.Fatal("expected nil adapter when no emit bound")
		}
	})
}
