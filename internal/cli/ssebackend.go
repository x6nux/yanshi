package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"

	"github.com/cloudwego/eino/schema"

	"github.com/x6nux/yanshi/internal/proto"
)

// ErrSSEControlUnsupported is returned by sseBackend.SendFrame. SSE is a
// stateless POST-per-turn transport: there is no persistent socket to write a
// mid-stream control frame to, so the Phase-10 slash commands (which send
// set_model / list_models / clear / get_status / compact / list_mcp /
// permission_response) are not available on SSE. Model and thinking effort are
// still selectable per-request via the request body (T30), but the interactive
// slash-command UX requires the WebSocket transport. The TUI almost always
// resolves to WS (the primary transport); SSE is only a fallback when the WS
// handshake fails.
var ErrSSEControlUnsupported = errors.New("control frames require the WebSocket transport (SSE is stateless)")

// sseBackend is the SSE transport — a full-capability alternative to wsBackend:
// multi-turn and tool-aware. Because SSE is unidirectional, the backend holds
// the conversation history CLIENT-side and sends the full messages[] with each
// POST; the server runs EventsWithHistory on it. Tool calls arrive as the same
// structured events as WS (event: tool_call / tool_result, data: ServerFrame),
// so the TUI's agent-interaction mode is identical on either transport.
type sseBackend struct {
	baseURL string
	client  *http.Client

	histMu  sync.Mutex
	history []schema.Message // client-held conversation history

	cancelMu      sync.Mutex
	cancelCurrent context.CancelFunc

	threadID string
	turns    uint64
}

func newSSEBackend(baseURL string) *sseBackend {
	return &sseBackend{baseURL: baseURL, client: &http.Client{}, threadID: newClientThreadID()}
}

func (b *sseBackend) Mode() string { return "sse" }

// Send appends the user turn to the client-held history, POSTs the full
// history, and streams structured SSE events. On turn completion the
// accumulated assistant text is appended to the history (multi-turn memory).
func (b *sseBackend) Send(ctx context.Context, text string) (<-chan StreamEvent, error) {
	b.histMu.Lock()
	b.history = append(b.history, schema.Message{Role: schema.User, Content: text})
	snapshot := make([]schema.Message, len(b.history))
	copy(snapshot, b.history)
	b.histMu.Unlock()

			b.turns++
		body, _ := json.Marshal(struct {
			Messages []schema.Message `json:"messages"`
			ThreadID string `json:"thread_id,omitempty"`
			TurnID   string `json:"turn_id,omitempty"`
		}{
			Messages: snapshot,
			ThreadID: b.threadID,
			TurnID:   b.threadID + ":" + fmt.Sprintf("%d", b.turns),
		})

	// Create the cancellable child ctx BEFORE building the request so the
	// request inherits it. Otherwise Cancel() only fires the goroutine's
	// select at the next SSE flush boundary — a stalled turn (server thinking,
	// no data) blocks in sc.Scan() and can't be cancelled.
	ctx, cancel := context.WithCancel(ctx)
	b.cancelMu.Lock()
	b.cancelCurrent = cancel
	b.cancelMu.Unlock()

	req, err := http.NewRequestWithContext(ctx, "POST", b.baseURL+"/api/v1/chat", bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := b.client.Do(req)
	if err != nil {
		cancel()
		return nil, err
	}

	ch := make(chan StreamEvent, 16)
	go func() {
		defer cancel()
		defer resp.Body.Close()
		defer close(ch)

		var assistantText string
		var eventType string
		var dataBuf bytes.Buffer
		flush := func() {
			if dataBuf.Len() == 0 {
				eventType = ""
				return
			}
			var f proto.ServerFrame
			if json.Unmarshal(dataBuf.Bytes(), &f) == nil {
				if f.Type == "" {
					f.Type = eventType
				}
				switch f.Type {
				case "history_replaced":
					// Task 35b: the server compacted the history we sent.
					// Adopt its compacted slice verbatim (it includes the user
					// turn that triggered the compaction) so the NEXT POST
					// carries the compacted history. Transport-internal: not
					// forwarded to the TUI (the compact_chunk deltas + status
					// already drive the compaction UI).
					b.histMu.Lock()
					b.history = append([]schema.Message(nil), f.Messages...)
					b.histMu.Unlock()
				case "agent_chunk":
					assistantText += f.Text
					ch <- toStreamEvent(f)
				case "structured_result":
					// A12-core: validated JSON for a schema-constrained turn.
					// Forwarded like any turn event via toStreamEvent; listed
					// explicitly so the SSE switch stays in lockstep with the
					// WS path (CLAUDE.md: new frame type -> both ws.go and
					// ssebackend.go).
					ch <- toStreamEvent(f)
				default:
					// thinking, compact_chunk, tool_call, tool_result, status,
					// error, done, models, mcp_list, permission_request — all
					// surface to the TUI via toStreamEvent unchanged.
					ch <- toStreamEvent(f)
				}
			}
			eventType = ""
			dataBuf.Reset()
		}

		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			switch {
			case len(line) >= 7 && line[:7] == "event: ":
				eventType = line[7:]
			case len(line) >= 6 && line[:6] == "data: ":
				dataBuf.WriteString(line[6:])
			case line == "":
				flush()
				select {
				case <-ctx.Done():
					return
				default:
				}
			}
		}
		flush()

		// Commit the assistant turn to client-held history (multi-turn).
		if assistantText != "" {
			b.histMu.Lock()
			b.history = append(b.history, schema.Message{Role: schema.Assistant, Content: assistantText})
			b.histMu.Unlock()
		}
	}()

	return ch, nil
}

func (b *sseBackend) Cancel() error {
	b.cancelMu.Lock()
	c := b.cancelCurrent
	b.cancelMu.Unlock()
	if c != nil {
		c()
	}
	return nil
}

// SendFrame is unsupported on SSE for the standard control frames: the
// transport is a stateless POST-per-turn request, so there is no persistent
// socket to carry a mid-stream control frame (set_model / list_models / clear
// / get_status / compact / list_mcp / permission_response). Model and thinking
// effort are selectable per-request via the POST body instead (T30).
//
// /features (features_list / features_set) is also WS-only, but it gets a
// distinct error message so a TUI that surfaces /features can tell the user
// "this needs a WS backend" rather than the generic control-unsupported text
// (constraint 7 in the C4 plan).
func (b *sseBackend) SendFrame(_ context.Context, f proto.ClientFrame) (<-chan StreamEvent, error) {
	switch f.Type {
	case "features_list", "features_set":
		return nil, errors.New("/features requires a WebSocket backend (SSE is stateless)")
	default:
		return nil, ErrSSEControlUnsupported
	}
}

func (b *sseBackend) Close() error { return nil }

// newClientThreadID generates a unique SSE thread identifier.
// Format: "sse-" + 6 bytes of crypto/rand as hex.
func newClientThreadID() string {
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand.Read almost never fails; fall back to a fixed length
		return "sse-000000000000"
	}
	return "sse-" + hex.EncodeToString(buf)
}
