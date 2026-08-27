package http

// ws_recall.go — the call site for C12's automatic memory recall.
//
// The retrieval itself lives in internal/tools (AutoRecall). This file is the
// half that was missing: nothing on the live turn path called it, so every
// memory in the store was reachable only by a model that already knew to run
// memory_search — precisely the situation C12 was written to end. A live run
// made the gap visible: AutoRecall returned a 406-character block when called
// directly against the session's own store, and the model's prompt for the very
// same question contained no trace of it.
//
// Keeping the call site in its own file rather than inline in ws.go is not
// cosmetic. runUserTurn is long and already carries several ordering
// constraints; a retrieval that must happen at one specific point between the
// skill prefix and the history append deserves a named function whose doc
// comment can say why that point and not another.

import (
	"context"

	"github.com/x6nux/yanshi/internal/store"
	"github.com/x6nux/yanshi/internal/tools"
)

// withRecalledMemories prepends any strongly-matching stored memories to query,
// or returns it unchanged.
//
// Unchanged is the common case and the intended one. Every guard here resolves
// toward "inject nothing": no store, a session that is not being recorded, no
// relevant memory. The asymmetry is deliberate and is argued in
// internal/tools/memory_autorecall.go — a missed injection costs one lookup the
// model might not make, while a spurious one is charged to every turn
// afterwards through the attention it trains away.
//
// # Scope
//
// The filter is left ZERO rather than narrowed to cs.sessionID, and that is a
// decision rather than an omission. Memories are written by memory_write with
// whatever dimensions its caller knows, and the value of a recalled memory is
// almost always cross-session: "the deployment runbook lives in X" is worth
// exactly as much in a conversation started tomorrow. Scoping the recall to the
// current session would make a memory recallable only inside the conversation
// that stored it, which is a cache, not a memory. C14's dimensions exist so a
// caller that genuinely wants isolation can ask for it; the default retrieval
// matches memory_search's default, so what the model can find by asking and
// what it is handed unasked are the same set.
//
// # Suppressed recording
//
// A connection with recording suppressed gets no recall. Such a session is
// explicitly off the record, and handing it content retrieved from the durable
// store would leak the persistent side of the system into a conversation that
// asked not to touch it.
func withRecalledMemories(s *Server, cs *connSession, query string) string {
	if s == nil || s.store == nil || cs == nil || cs.recordingSuppressed() {
		return query
	}
	block := tools.AutoRecall(context.Background(), s.store, query, store.MemoryFilter{})
	if block == "" {
		return query
	}
	return block + "\n\n" + query
}
