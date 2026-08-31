// internal/api/http/ws_diff.go
//
// handleWorkspaceDiff is W-E-13's /diff backend: it replies with the current
// workspace's pending (uncommitted) changeset so the TUI can render it without
// switching windows. Split into its own file rather than folded into
// ws_seam.go because it is a different concern (main-scope working-tree state,
// not turn-rollback seams) even though the two handlers share the same
// VCS-unconfigured guard shape.

package http

import (
	"database/sql"
	"errors"

	"github.com/x6nux/yanshi/internal/proto"
)

// handleWorkspaceDiff replies with what THIS SESSION has changed on the
// "main" scope so far — the scope chat/orchestrator edits track to (autoVCS
// scope, see CLAUDE.md).
//
// RE-1: this used to diff vcs_uncommitted ("main", s.repoID) directly and
// was NOT session-scoped. That table is folded into a commit and emptied by
// SealMainTurnSeam both immediately before and immediately after every
// turn (sealTurnBoundary), so it is structurally empty at essentially every
// moment a user could actually type /diff — the feature was non-functional
// in production even though its old unit test passed (it seeded a row via
// RecordEditMain directly, bypassing turn boundaries entirely). Now it
// diffs the session's baseline (SessionBaseline: the commit id on this
// session's first pre-turn seam) against the CURRENT main_head via
// CommitRangeDiff — both are durable commits, unaffected by pending-row
// folding, so this reflects reality at any moment between turns, which is
// the only moment a user can actually send this frame.
//
// When cs.sessionID == "" (no user_message has been sent yet on this
// connection, so no seam namespace exists) this replies with an empty diff,
// mirroring handleListSeams' identical cs.sessionID=="" branch — there is
// nothing to diff yet, which is different from VCS being unconfigured.
//
// When VCS is unconfigured this replies with an error frame rather than an
// empty WorkspaceDiff (RE-6): an empty list is indistinguishable on the wire
// from "VCS is enabled and nothing is pending", which told a user on a
// VCS-less server that /diff had "nothing to show" instead of that the
// feature isn't available at all — see handleListSeams for the same fix
// applied to /seams' analogous nil-VCS branch.
func handleWorkspaceDiff(s *Server, conn *wsConn, cs *connSession) {
	if s.vcs == nil || s.repoID == "" {
		conn.write(proto.NewError("workspace_diff: vcs is not enabled for this repo"))
		return
	}
	if cs.sessionID == "" {
		conn.write(proto.NewWorkspaceDiff(nil))
		return
	}
	baseline, err := s.vcs.SessionBaseline(s.repoID, cs.sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		// No turn has completed for this session yet, so there is no seam to
		// anchor a baseline against — nothing to diff yet, not a failure.
		conn.write(proto.NewWorkspaceDiff(nil))
		return
	}
	if err != nil {
		conn.write(proto.NewError("workspace_diff: " + err.Error()))
		return
	}
	head, err := s.vcs.RepoMainHead(s.repoID)
	if err != nil {
		conn.write(proto.NewError("workspace_diff: " + err.Error()))
		return
	}
	files, err := s.vcs.CommitRangeDiff(s.repoID, baseline, head)
	if err != nil {
		conn.write(proto.NewError("workspace_diff: " + err.Error()))
		return
	}
	items := make([]proto.WorkspaceDiffFile, 0, len(files))
	for _, f := range files {
		items = append(items, proto.WorkspaceDiffFile{
			Path:    f.Path,
			Op:      f.Op,
			OldText: f.OldText,
			NewText: f.NewText,
		})
	}
	conn.write(proto.NewWorkspaceDiff(items))
}
