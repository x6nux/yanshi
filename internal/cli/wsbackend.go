package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"

	"github.com/x6nux/yanshi/internal/proto"
)

// wsBackend is a persistent WebSocket client of /api/v1/chat/ws. It keeps one
// connection across Send calls so the server retains multi-turn history. A
// dedicated read goroutine pumps ServerFrames into the active Send's channel.
//
// Two cur-channel modes:
//   - turn mode (controlMode=false): Send wrote a user_message; the channel
//     stays open until the server sends done/error (the turn terminator).
//   - control mode (controlMode=true): SendFrame wrote a control REQUEST
//     (list_models / get_status / set_model / set_thinking / clear / compact /
//     list_mcp); the channel closes on the first control REPLY (models / status
//     / mcp_list), which is the single-frame response. compact_chunk deltas do
//     not close it (they stream before the final status).
//
// permission_response is special: it is a mid-turn reply written WITHOUT
// disturbing the active turn's cur channel (the turn is blocked inside an ask
// callback waiting for this very answer), so SendFrame returns nil for it.
type wsBackend struct {
	conn *websocket.Conn

	sendMu sync.Mutex // serialize Send/SendFrame writes (one frame at a time)
	cur    chan StreamEvent
	curMu  sync.Mutex

	controlMode bool // true while cur is a control-request reply channel

	cancelCurrent context.CancelFunc
	cancelMu      sync.Mutex

	done      chan struct{}
	closeOnce sync.Once
}

// newWSBackend dials the chat WebSocket. The URL scheme must be ws/wss.
func newWSBackend(ctx context.Context, url string) (*wsBackend, error) {
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, url, http.Header{})
	if err != nil {
		return nil, err
	}
	b := &wsBackend{conn: conn, done: make(chan struct{})}
	go b.readLoop()
	return b, nil
}

func (b *wsBackend) Mode() string { return "ws" }

// Send writes a user_message frame and returns a channel receiving the turn's
// events (closed on done/error). Only one turn at a time (sendMu).
func (b *wsBackend) Send(ctx context.Context, text string) (<-chan StreamEvent, error) {
	b.sendMu.Lock()
	defer b.sendMu.Unlock()

	ch := make(chan StreamEvent, 16)
	b.curMu.Lock()
	b.cur = ch
	b.controlMode = false // turn mode: close on done/error
	b.curMu.Unlock()

	// Wire ctx cancellation to a cancel frame + close the channel.
	innerCtx, cancel := context.WithCancel(ctx)
	b.cancelMu.Lock()
	b.cancelCurrent = cancel
	b.cancelMu.Unlock()

	data, err := json.Marshal(proto.NewUserMessage(text))
	if err != nil {
		cancel()
		return nil, err
	}
	if err := b.conn.WriteMessage(websocket.TextMessage, data); err != nil {
		cancel()
		return nil, err
	}

	go func() {
		select {
		case <-innerCtx.Done():
		case <-b.done:
			return
		}
		// Tell the server to cancel, then end the local channel. Hold sendMu
		// around the WriteMessage: gorilla/websocket forbids concurrent writers
		// and every other write site already holds it.
		b.sendMu.Lock()
		_ = b.conn.WriteMessage(websocket.TextMessage, mustMarshal(proto.NewCancel()))
		b.sendMu.Unlock()
		b.curMu.Lock()
		cur := b.cur
		b.cur = nil
		b.controlMode = false
		b.curMu.Unlock()
		safeClose(cur)
	}()

	return ch, nil
}

// SendFrame writes a Phase-10 control frame. Request frames (list_models /
// get_status / set_model / set_thinking / clear / compact / list_mcp) set up a
// reply channel just like Send and return it; the channel closes when the
// single-frame reply (models / status / mcp_list) arrives. permission_response
// is a mid-turn reply: it is written without touching b.cur (the active turn
// owns it and is blocked waiting for this answer) and returns nil.
func (b *wsBackend) SendFrame(ctx context.Context, f proto.ClientFrame) (<-chan StreamEvent, error) {
	if f.Type == "permission_response" || f.Type == "set_mode" {
		// Mid-turn-safe frames: do NOT replace cur (the turn's channel owns it).
		// permission_response is a mid-turn reply with no ack; set_mode is a mode
		// switch the user can fire DURING a turn (e.g. from a permission popup),
		// and replacing cur would orphan the turn's remaining events. Both write
		// the frame and return nil; any status reply set_mode triggers flows on
		// the existing turn channel (or is dropped when idle — the TUI already set
		// the mode locally, so it does not need the echo).
		data, err := json.Marshal(f)
		if err != nil {
			return nil, err
		}
		b.sendMu.Lock()
		defer b.sendMu.Unlock()
		if err := b.conn.WriteMessage(websocket.TextMessage, data); err != nil {
			return nil, err
		}
		return nil, nil
	}

	b.sendMu.Lock()
	defer b.sendMu.Unlock()

	ch := make(chan StreamEvent, 16)
	b.curMu.Lock()
	b.cur = ch
	b.controlMode = true // control mode: close on the reply frame
	b.curMu.Unlock()

	innerCtx, cancel := context.WithCancel(ctx)
	b.cancelMu.Lock()
	b.cancelCurrent = cancel
	b.cancelMu.Unlock()

	data, err := json.Marshal(f)
	if err != nil {
		cancel()
		b.curMu.Lock()
		b.cur = nil
		b.controlMode = false
		b.curMu.Unlock()
		return nil, err
	}
	if err := b.conn.WriteMessage(websocket.TextMessage, data); err != nil {
		cancel()
		b.curMu.Lock()
		b.cur = nil
		b.controlMode = false
		b.curMu.Unlock()
		return nil, err
	}

	go func() {
		select {
		case <-innerCtx.Done():
		case <-b.done:
			return
		}
		// Hold sendMu around the WriteMessage: gorilla/websocket forbids
		// concurrent writers.
		b.sendMu.Lock()
		_ = b.conn.WriteMessage(websocket.TextMessage, mustMarshal(proto.NewCancel()))
		b.sendMu.Unlock()
		b.curMu.Lock()
		cur := b.cur
		b.cur = nil
		b.controlMode = false
		b.curMu.Unlock()
		safeClose(cur)
	}()

	return ch, nil
}

// readLoop pumps frames from the socket into the active channel until closed.
func (b *wsBackend) readLoop() {
	for {
		_, data, err := b.conn.ReadMessage()
		if err != nil {
			b.curMu.Lock()
			cur := b.cur
			b.cur = nil
			b.controlMode = false
			b.curMu.Unlock()
			if cur != nil {
				safeSend(cur, StreamEvent{Kind: "error", Err: err})
				safeClose(cur)
			}
			return
		}
		var f proto.ServerFrame
		if err := json.Unmarshal(data, &f); err != nil {
			continue
		}
		ev := toStreamEvent(f)
		b.curMu.Lock()
		cur := b.cur
		ctrl := b.controlMode
		b.curMu.Unlock()
		if cur != nil {
			safeSend(cur, ev)
		}
		// Turn terminator: done/error close cur (turn mode). Control reply:
		// models/status/mcp_list close cur (control mode). compact_chunk does
		// NOT close (it streams before the final status in Task 35b).
		if ev.Kind == "done" || ev.Kind == "error" {
			b.curMu.Lock()
			b.cur = nil
			b.controlMode = false
			b.curMu.Unlock()
			safeClose(cur)
		} else if ctrl && isControlReply(ev.Kind) {
			b.curMu.Lock()
			b.cur = nil
			b.controlMode = false
			b.curMu.Unlock()
			safeClose(cur)
		}
	}
}

// isControlReply reports whether a server frame type is the single-frame reply
// to a control request (and thus closes the control-mode cur channel).
func isControlReply(kind string) bool {
	switch kind {
	case "models", "status", "mcp_list", "mcp_status", "sessions", "session_restored", "session_ack",
		"permissions", "permission_rule_hit",
		"jobs", "job_event",
		"seams", "seam_restored", // B2-RB1 必修项 I
		"session_forked", "side_state", "skills_list", "skill_ack",
		"features": // OBS3: /features reply is a single-frame control reply
		return true
	}
	return false
}

// toStreamEvent maps a ServerFrame to a StreamEvent, including the Phase-10
// control-reply fields. models.Names and mcp_list.Servers both surface as Items
// (the Kind disambiguates); they are never set together.
func toStreamEvent(f proto.ServerFrame) StreamEvent {
	items := f.Names
	if items == nil {
		items = f.Servers
	}
	// Convert Messages to stubs for the StreamEvent.
	msgs := make([]MessageStub, 0, len(f.Messages))
	for _, m := range f.Messages {
		msgs = append(msgs, MessageStub{Role: string(m.Role), Content: m.Content})
	}
	ev := StreamEvent{
		Kind:             f.Type,
		Text:             f.Text,
		ToolName:         f.ToolName,
		ToolArgs:         f.ToolArgs,
		ToolStatus:       f.Status,
		Overwrite:        f.Overwrite,
		ID:               f.ID,
		Reason:           f.Reason,
		ApprovalRequired: f.ApprovalRequired,
		Model:            f.Model,
		Thinking:         f.Thinking,
		PermMode:         f.PermMode,
		AutoThreshold:    f.AutoThreshold,
		TokensIn:         f.TokensIn,
		TokensOut:        f.TokensOut,
		CachedTokens:     f.CachedTokens,
		ReasoningTokens:  f.ReasoningTokens,
		Turns:            f.Turns,
		ContextWindow:    f.ContextWindow,
		Items:            items,
		MCPServers:       f.MCPServers,
		Compacted:        f.Compacted,
		TokensBefore:     f.TokensBefore,
		TokensAfter:      f.TokensAfter,
		RetryAttempt:     f.RetryAttempt,
		RetryMax:         f.RetryMax,
		RetryDelayMs:     f.RetryDelayMs,
		Sessions:         f.Sessions,
		SessionID:        f.SessionID,
		MemoryPath:       f.MemoryPath,
		LogPath:          f.LogPath,
		SideDepth:        f.SideDepth,
		Skills:           f.Skills,
		Skill:            f.Skill,
		Action:           f.Action,
		Messages:         msgs,
		StructuredResult: f.StructuredResult,
		CostUSD:          f.CostUSD,
		CostKnown:        f.CostKnown,
		Features:         f.Features,
		Permissions:      f.Permissions,
		Jobs:             f.Jobs,
		Task:             f.Task,
		TaskID:           f.TaskID,
		Checklist:        f.Checklist,
		// B2-RB1: seam fields. seams/head are populated for the seams reply;
		// seam_restored maps ID → UndoSeamID (the load-bearing field for
		// "undo this revert" UX).
		Seams:       f.Seams,
		CommitShort: f.CommitShort,
		Head:        f.Head,
	}
	if f.Type == "seam_restored" {
		ev.UndoSeamID = f.ID
	}
	return ev
}

func (b *wsBackend) Cancel() error {
	b.cancelMu.Lock()
	c := b.cancelCurrent
	b.cancelMu.Unlock()
	if c != nil {
		c()
	}
	return nil
}

func (b *wsBackend) Close() error {
	var err error
	b.closeOnce.Do(func() {
		err = b.conn.Close()
		close(b.done) // unblock any watcher goroutines blocked in their select
	})
	return err
}

func safeClose(ch chan StreamEvent) {
	if ch != nil {
		// recover in case a late writer races; closing twice panics.
		defer func() { _ = recover() }()
		close(ch)
	}
}

// safeSend delivers ev to ch, recovering if ch was closed concurrently by the
// cancel watcher (a blocked send on a full buffer otherwise panics when the
// watcher closes cur after releasing curMu). The dropped frame is harmless
// (the turn is being cancelled / the backend is closing), so we prefer this
// over holding curMu across the send, which would stall cancel on a full buffer.
func safeSend(ch chan StreamEvent, ev StreamEvent) {
	defer func() { _ = recover() }()
	ch <- ev
}

func mustMarshal(f proto.ClientFrame) []byte {
	b, _ := json.Marshal(f)
	return b
}
