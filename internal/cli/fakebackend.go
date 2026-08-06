package cli

import (
	"context"
	"sync"

	"github.com/x6nux/yanshi/internal/proto"
)

// fakeBackend emits a scripted sequence of agent_chunk events followed by done.
// It also records every ClientFrame passed to SendFrame so tests can assert on
// the control frames a command produced (e.g. "/clear" sent a clear frame).
type fakeBackend struct {
	chunks []string

	mu     sync.Mutex
	frames []proto.ClientFrame
}

func newFakeBackend(chunks []string) *fakeBackend { return &fakeBackend{chunks: chunks} }

func (b *fakeBackend) Send(ctx context.Context, text string) (<-chan StreamEvent, error) {
	return b.SendTurn(ctx, proto.NewUserMessage(text))
}

// SendTurn records the frame before replaying the scripted chunks, so a test
// can assert what the TUI actually put on the wire — attachments included.
// Send used to discard its argument entirely, which made "did the turn carry
// the attachment?" unanswerable from the fake.
func (b *fakeBackend) SendTurn(_ context.Context, f proto.ClientFrame) (<-chan StreamEvent, error) {
	b.mu.Lock()
	b.frames = append(b.frames, f)
	b.mu.Unlock()
	ch := make(chan StreamEvent, len(b.chunks)+1)
	go func() {
		defer close(ch)
		for _, c := range b.chunks {
			ch <- StreamEvent{Kind: "agent_chunk", Text: c}
		}
		ch <- StreamEvent{Kind: "done"}
	}()
	return ch, nil
}

// SendFrame records f for later test inspection (Frames) and returns a nil
// channel: the fake backend cannot synthesize a real server reply. Tests that
// need to drive applyEvent of a reply frame feed it directly via applyEvent.
func (b *fakeBackend) SendFrame(_ context.Context, f proto.ClientFrame) (<-chan StreamEvent, error) {
	b.mu.Lock()
	b.frames = append(b.frames, f)
	b.mu.Unlock()
	return nil, nil
}

// Frames returns a copy of the ClientFrames recorded by SendFrame (test helper).
func (b *fakeBackend) Frames() []proto.ClientFrame {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]proto.ClientFrame, len(b.frames))
	copy(out, b.frames)
	return out
}

func (b *fakeBackend) Cancel() error { return nil }
func (b *fakeBackend) Close() error  { return nil }
func (b *fakeBackend) Mode() string  { return "fake" }
