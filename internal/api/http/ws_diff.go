// internal/api/http/ws_diff.go
//
// handleWorkspaceDiff is W-E-13's /diff backend: it replies with the current
// workspace's pending (uncommitted) changeset so the TUI can render it without
// switching windows. Split into its own file rather than folded into
// ws_seam.go because it is a different concern (main-scope working-tree state,
// not turn-rollback seams) even though the two handlers share the same
// VCS-unconfigured guard shape.

package http

import "github.com/x6nux/yanshi/internal/proto"

// handleWorkspaceDiff replies with the pending edits on the "main" scope —
// the scope chat/orchestrator edits track to (autoVCS scope, see CLAUDE.md).
// Unlike handleListSeams this is NOT session-scoped: the workspace's pending
// changeset is a single main-scope changeset shared by the whole repo, not
// something split per WS connection, so there is no cs.sessionID guard here.
//
// When VCS is unconfigured this replies with an error frame rather than an
// empty WorkspaceDiff (RE-6): an empty list is indistinguishable on the wire
// from "VCS is enabled and nothing is pending", which told a user on a
// VCS-less server that /diff had "nothing to show" instead of that the
// feature isn't available at all — see handleListSeams for the same fix
// applied to /seams' analogous nil-VCS branch.
func handleWorkspaceDiff(s *Server, conn *wsConn) {
	if s.vcs == nil || s.repoID == "" {
		conn.write(proto.NewError("workspace_diff: vcs is not enabled for this repo"))
		return
	}
	files, err := s.vcs.UncommittedDiff("main", s.repoID)
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
