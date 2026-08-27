// internal/api/http/ws_seam.go
//
// Seam-related WS handlers, split from ws.go to keep that file under the 1000
// pure-code-line cap (B2-RB1 CLAUDE.md). handleListSeams replies with the
// recent seams for the session + the current head (short display hash and full
// destructive binding); handleRestoreTurn validates the target binding, reverts
// main + truncates history + replies with the undo seam id.

package http

import (
	"fmt"
	"slices"

	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/store"
	"github.com/x6nux/yanshi/internal/vcs"
)

// applySessionRevertSnapshot replaces only the conversation-owned connSession
// fields from an exact durable snapshot. It intentionally leaves live transport
// state (perm, inTurn, startedAt, defaultModel) untouched.
//
// The message mapping is restoreMessages (ws_handlers.go), shared with the
// WS restore_session path rather than duplicated here. This used to carry
// its own copy — role split into a user/assistant binary with an
// Assistant-leaning default, dropping ToolCallID/ToolName/ToolArgs — which
// was the same defect restoreMessages exists to fix, reachable from three
// live frames (fork/reconnect via loadSession, restore_turn's two snapshot
// branches below) that a fix scoped to only the WS restore_session handler
// would have left broken.
func applySessionRevertSnapshot(cs *connSession, snap store.SessionRevertSnapshot) {
	cs.history = restoreMessages(snap.Messages)
	cs.sessionID = snap.Meta.ID
	cs.seq = len(snap.Messages)
	cs.model = snap.Meta.Model
	cs.thinking = snap.Meta.Thinking
	cs.tokensIn = snap.Meta.TokensIn
	cs.tokensOut = snap.Meta.TokensOut
	cs.cachedTokens = snap.Meta.CachedTokens
	cs.reasoningTokens = snap.Meta.ReasoningTokens
	cs.turns = snap.Meta.Turns
}

// handleListSeams replies with the recent seams for the session (filtered by
// cs.sessionID when set — D7: never pass "" here or other sessions' seams leak)
// + the current main_head (short for display, full for binding). When VCS is
// unconfigured the reply is an empty list (the TUI renders "(no seams)").
func handleListSeams(s *Server, conn *wsConn, cs *connSession) {
	if s.vcs == nil || s.repoID == "" {
		conn.write(proto.NewSeams(nil, "", ""))
		return
	}
	if cs.sessionID == "" {
		// D7: an unestablished WS session has no seam namespace. Return an empty
		// list without ever passing "" to ListSeams (which means all sessions).
		conn.write(proto.NewSeams(nil, s.shortHead(), s.fullHead()))
		return
	}
	// D7: always scope by this concrete session id; never call ListSeams with "".
	seams, err := s.vcs.ListSeams(s.repoID, cs.sessionID, 0)
	if err != nil {
		conn.write(proto.NewError("list_seams: " + err.Error()))
		return
	}
	items := make([]proto.SeamInfo, 0, len(seams))
	for _, sm := range seams {
		items = append(items, proto.SeamInfo{
			ID:          sm.ID,
			Kind:        string(sm.Kind),
			TurnSeq:     sm.TurnSeq,
			HistoryLen:  sm.HistoryLen,
			CommitShort: shortHash(sm.CommitID),
			Label:       sm.Label,
			CreatedAt:   sm.CreatedAt,
		})
	}
	conn.write(proto.NewSeams(items, s.shortHead(), s.fullHead()))
}

// handleRestoreTurn validates the target binding, reverts main to the seam's
// commit, truncates the in-memory + persisted history, and replies with the
// undo seam id (so the user can revert again to undo this revert).
//
// Fail-closed ordering (D5): validate everything; transition durable session
// state first (truncate for an ordinary seam, restore the undo snapshot for a
// pre-revert seam); update memory; only then call VCS revert. The exact durable
// state before this operation is passed into RevertToSeam and stored on the new
// undo seam in the same tx as main_head (D2). A durable transition failure is
// FATAL and VCS/memory remain untouched. If VCS fails, restore both conversation
// layers from that exact snapshot before replying; compensation failure is FATAL.
//
// confirmedHead (D6): the FULL main_head commit id the client bound at list
// time. The server REQUIRES it non-empty and compares the FULL id — short-hash
// comparison risks collision across long histories.
func handleRestoreTurn(s *Server, conn *wsConn, cs *connSession, seamID, confirmedHead string) {
	if s.vcs == nil || s.repoID == "" || seamID == "" {
		conn.write(proto.NewError("restore_turn: VCS disabled or missing seam_id"))
		conn.write(proto.NewDone())
		return
	}

	// D6: require non-empty confirmedHead AND full-id match.
	if confirmedHead == "" {
		conn.write(proto.NewError("restore_turn: missing confirmed_head (re-list to bind the current head)"))
		conn.write(proto.NewDone())
		return
	}
	if actual := s.fullHead(); actual != confirmedHead {
		conn.write(proto.NewError(
			fmt.Sprintf("restore_turn: head changed since listing (was %s, now %s); re-list and re-confirm",
				shortHash(confirmedHead), shortHash(actual))))
		conn.write(proto.NewDone())
		return
	}

	// Load the seam to capture its history boundary + session BEFORE reverting.
	seam, err := s.vcs.FindSeam(seamID)
	if err != nil {
		conn.write(proto.NewError("restore_turn: seam not found: " + err.Error()))
		conn.write(proto.NewDone())
		return
	}
	// D7: exact match, including rejecting an empty current session.
	if cs.sessionID == "" || seam.SessionID != cs.sessionID {
		conn.write(proto.NewError("restore_turn: seam does not belong to this session"))
		conn.write(proto.NewDone())
		return
	}

	// Conversation rollback requires the durable store. In the current server a
	// non-empty sessionID is created only when Store is configured, but keep this
	// explicit and fail closed rather than silently doing a VCS-only WS restore.
	if s.store == nil {
		conn.write(proto.NewError("restore_turn: FATAL: durable session store is unavailable"))
		conn.write(proto.NewDone())
		return
	}

	oldHistoryLen, oldTurns := len(cs.history), cs.turns
	var durableBefore store.SessionRevertSnapshot

	if seam.Kind == vcs.SeamPreRevert {
		// D2 undo expansion: the first revert stored its exact pre-revert messages
		// on this seam. Decode + validate the payload before mutating either layer.
		target, decodeErr := store.DecodeSessionRevertSnapshot(seam.HistorySnapshot)
		if decodeErr != nil || target.Meta.ID != cs.sessionID ||
			len(target.Messages) != seam.PrevHistoryLen ||
			target.Meta.Turns != seam.PrevTurnSeq {
			conn.write(proto.NewError(fmt.Sprintf(
				"restore_turn: FATAL: invalid undo history snapshot: err=%v session=%q len=%d/%d turns=%d/%d",
				decodeErr, target.Meta.ID, len(target.Messages), seam.PrevHistoryLen,
				target.Meta.Turns, seam.PrevTurnSeq)))
			conn.write(proto.NewDone())
			return
		}
		var err error
		durableBefore, err = s.store.SnapshotSessionForRevert(cs.sessionID)
		if err == nil {
			err = s.store.RestoreSessionAfterFailedRevert(target)
		}
		if err != nil {
			conn.write(proto.NewError(
				"restore_turn: FATAL: durable undo history restore failed: " + err.Error()))
			conn.write(proto.NewDone())
			return
		}
		applySessionRevertSnapshot(cs, target)
	} else {
		// Ordinary seam: target must be a prefix of the live history. Zero is valid.
		truncLen, truncTurns := seam.HistoryLen, seam.TurnSeq
		if truncLen < 0 || truncLen > len(cs.history) || truncTurns < 0 {
			conn.write(proto.NewError(fmt.Sprintf(
				"restore_turn: invalid history boundary len=%d/%d turns=%d",
				truncLen, len(cs.history), truncTurns)))
			conn.write(proto.NewDone())
			return
		}
		var err error
		durableBefore, err = s.store.TruncateSessionForRevert(
			cs.sessionID, truncLen, truncTurns)
		if err != nil {
			conn.write(proto.NewError(
				"restore_turn: FATAL: durable history truncation failed: " + err.Error()))
			conn.write(proto.NewDone())
			return
		}
		cs.history = slices.Clone(cs.history[:truncLen])
		cs.seq = truncLen
		cs.turns = truncTurns
	}

	// D5 phase 3: VCS last. Task 5 compensates disk for its own internal errors.
	// On success it stores durableBefore on the returned undo seam atomically with
	// main_head; on failure, restore the exact pre-operation conversation state.
	undoID, err := s.vcs.RevertToSeam(s.repoID, seamID,
		"restore by "+cs.sessionID, oldHistoryLen, oldTurns, &durableBefore)
	if err != nil {
		restoreErr := s.store.RestoreSessionAfterFailedRevert(durableBefore)
		applySessionRevertSnapshot(cs, durableBefore)
		if restoreErr != nil {
			conn.write(proto.NewError(fmt.Sprintf(
				"restore_turn: FATAL: VCS revert failed (%v) and durable history compensation failed (%v)",
				err, restoreErr)))
			conn.write(proto.NewDone())
			return
		}
		conn.write(proto.NewError("restore_turn: " + err.Error()))
		conn.write(proto.NewDone())
		return
	}

	conn.write(proto.NewSeamRestored(undoID, s.shortHead(), s.fullHead(),
		fmt.Sprintf("reverted to %s (%s); undo seam %s",
			seam.Label, shortHash(seam.CommitID), shortHash(undoID))))
	conn.write(cs.statusFrame(s))
	conn.write(proto.NewDone())
}

// shortHash returns the first 8 hex of a commit id (same truncation as
// Server.shortHead but for arbitrary ids).
func shortHash(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
