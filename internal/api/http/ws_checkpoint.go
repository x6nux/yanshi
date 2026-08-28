// internal/api/http/ws_checkpoint.go
//
// W-D-06's carrier: the checkpoint frame, and the four actions behind
// /checkpoint.
//
// THE DIMENSION SPLIT IS THE WHOLE HANDLER. The store serves the session and
// memory dimensions and internal/vcs serves the files one, because the working
// copy is filesystem state under a repo lane and a freeze and has no business
// inside a database transaction. This file is where the two halves meet, since
// it is one of the few packages that already holds both.
package http

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/store"
)

// handleCheckpoint answers a checkpoint frame.
//
// An unknown action or dimension is an error, never a fallback: every action
// here except list either takes a snapshot or destroys state, so guessing which
// one the client meant is not a recoverable mistake.
func handleCheckpoint(s *Server, conn *wsConn, cs *connSession, cf proto.ClientFrame) {
	if s.store == nil {
		conn.write(proto.NewError("checkpoints are disabled: no store"))
		return
	}
	switch cf.Name {
	case proto.CheckpointList:
		text, err := renderCheckpoints(s.store)
		writeCheckpointReply(conn, text, err)
	case proto.CheckpointCreate:
		cp, err := s.createCheckpoint(cf.Text, cs.sessionID)
		if err != nil {
			conn.write(proto.NewError("checkpoint create: " + err.Error()))
			return
		}
		conn.write(proto.NewCheckpointResult(fmt.Sprintf(
			"checkpoint %s taken (session %s, memories %d, files %s)",
			cp.ID, orNone(cp.SessionID), cp.Memories, orNone(cp.FileCommit))))
	case proto.CheckpointPlan:
		text, err := s.planCheckpoint(cf.ID, cf.Dim)
		writeCheckpointReply(conn, text, err)
	case proto.CheckpointRestore:
		text, err := s.restoreCheckpoint(cf.ID, cf.Dim)
		writeCheckpointReply(conn, text, err)
	default:
		conn.write(proto.NewError("checkpoint: unknown action " + strconv.Quote(cf.Name)))
	}
}

// writeCheckpointReply is the one place a checkpoint outcome turns into a
// frame, so an error can never be rendered as a result.
func writeCheckpointReply(conn *wsConn, text string, err error) {
	if err != nil {
		conn.write(proto.NewError("checkpoint: " + err.Error()))
		return
	}
	conn.write(proto.NewCheckpointResult(text))
}

// orNone renders an absent dimension as a word rather than an empty gap, so a
// checkpoint with no session reads as such instead of looking truncated.
func orNone(v string) string {
	if v == "" {
		return "none"
	}
	return v
}

// createCheckpoint routes through the vcs when there is one, because only it can
// read the current head — and reading the head at checkpoint time is the point:
// a caller that fetched it earlier would record a moment that never existed.
func (s *Server) createCheckpoint(label, sessionID string) (store.Checkpoint, error) {
	if s.vcs != nil && s.repoID != "" {
		return s.vcs.CreateCheckpoint(label, sessionID, s.repoID)
	}
	return s.store.CreateCheckpoint(label, sessionID, "")
}

// planCheckpoint renders a dry run for one dimension.
func (s *Server) planCheckpoint(id, dim string) (string, error) {
	if store.CheckpointDimension(dim) == store.CheckpointFiles {
		if s.vcs == nil || s.repoID == "" {
			return "", fmt.Errorf("the files dimension needs a repository; none is configured")
		}
		plan, err := s.vcs.PlanCheckpointFiles(s.repoID, id)
		if err != nil {
			return "", err
		}
		create, overwrite, del := plan.Counts()
		return fmt.Sprintf(
			"files: %d created, %d overwritten, %d deleted, %d already match (target %s)\n"+
				"nothing has been written; run the restore to apply it",
			create, overwrite, del, plan.Unchanged, plan.TargetCommit), nil
	}
	plan, err := s.store.PlanCheckpointRestore(id, store.CheckpointDimension(dim))
	if err != nil {
		return "", err
	}
	return plan.Summary + "\nnothing has been written; run the restore to apply it", nil
}

// restoreCheckpoint rolls one dimension back and reports the automatic
// snapshot's id, which is what makes the restore undoable.
//
// THE FILE PATH PLANS AND APPLIES BACK TO BACK, and what the confirm token
// buys here is narrower than in an interactive preview: it does not bind the
// USER's confirmation to a plan they read — that binding is the "yes" token the
// TUI requires before this frame is ever sent — it binds the APPLY to the plan
// the server itself just computed. ApplyRestore re-plans under the repo lane and
// rejects a token that no longer matches, so a write that lands in between is
// still named rather than silently overwritten.
func (s *Server) restoreCheckpoint(id, dim string) (string, error) {
	if store.CheckpointDimension(dim) == store.CheckpointFiles {
		if s.vcs == nil || s.repoID == "" {
			return "", fmt.Errorf("the files dimension needs a repository; none is configured")
		}
		plan, err := s.vcs.PlanCheckpointFiles(s.repoID, id)
		if err != nil {
			return "", err
		}
		undo, applied, err := s.vcs.RestoreCheckpointFiles(s.repoID, id, plan.ConfirmToken)
		if err != nil {
			return "", err
		}
		create, overwrite, del := applied.Counts()
		return fmt.Sprintf(
			"files restored to %s: %d created, %d overwritten, %d deleted\nundo with: /checkpoint restore %s files yes",
			applied.TargetCommit, create, overwrite, del, undo.ID), nil
	}
	undo, err := s.store.RestoreCheckpoint(id, store.CheckpointDimension(dim))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s restored from %s\nundo with: /checkpoint restore %s %s yes",
		dim, id, undo.ID, dim), nil
}

// renderCheckpoints lists the recent checkpoints, newest first.
func renderCheckpoints(st *store.Store) (string, error) {
	cps, err := st.Checkpoints(20)
	if err != nil {
		return "", err
	}
	if len(cps) == 0 {
		return "no checkpoints yet — take one with /checkpoint create [label]", nil
	}
	var b strings.Builder
	for _, cp := range cps {
		fmt.Fprintf(&b, "%s  %s  session=%s memories=%d files=%s  %s\n",
			cp.ID, time.Unix(cp.CreatedAt, 0).Format("2006-01-02 15:04"),
			orNone(cp.SessionID), cp.Memories, orNone(cp.FileCommit), cp.Label)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}
