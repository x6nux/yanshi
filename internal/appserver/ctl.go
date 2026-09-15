package appserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/x6nux/yanshi/internal/ctl"
)

// This file is the OPERATOR half of the JSON-RPC surface: the methods that
// describe and change the project rather than run a conversation. They exist
// because the same operations were reachable only through the TUI's WebSocket
// control frames (or through hand-written CLI subcommands with their own copy
// of the logic), so a local tool driving yanshi over the app-server protocol —
// `yanshi app` or the IPC socket — could start a turn but could not list a
// session, read a background job, or see a feature flag.
//
// Every method here delegates to internal/ctl. That is the point: one
// implementation, three surfaces (TUI, CLI, JSON-RPC), so a new capability
// cannot exist on one surface and be missing on another.

// WithCtl attaches the operator control plane. A server without one answers
// these methods with a clear "this daemon has no control plane" error rather
// than "method not found" — the difference between a wiring bug and a client
// using the wrong protocol.
func (s *Server) WithCtl(c *ctl.Service) *Server {
	s.ctl = c
	return s
}

// ctlError maps a control-plane error onto the protocol's error space.
//
// The three cases are distinct on purpose:
//   - no control plane / state lives in another process -> internal error with
//     the ctl message, which tells the operator to start a daemon
//   - not found -> invalid params, matching how v1 reports an unknown thread
//   - anything else -> invalid params with the store's own message, because a
//     store failure here is almost always the caller's input (an unknown id)
//     and burying it in -32603 would hide that.
func ctlError(method string, err error) *RPCError {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ctl.ErrNeedsDaemon):
		return &RPCError{Code: codeInternalError, Message: method + ": " + err.Error()}
	case errors.Is(err, ctl.ErrNotFound):
		return &RPCError{Code: codeInvalidParams, Message: err.Error()}
	default:
		return &RPCError{Code: codeInvalidParams, Message: method + ": " + err.Error()}
	}
}

// ctlMethods are the method names this file implements, in the order
// capabilities advertises them.
func ctlMethods() []string {
	return []string{
		"session/list", "session/show", "session/messages", "session/rename", "session/fork",
		"session/archive", "session/delete",
		"usage/get",
		"features/list", "features/set",
		"skills/list", "skills/show", "skills/enable", "skills/disable",
		"skills/trust", "skills/untrust",
		"approvals/list", "approvals/revoke",
		"jobs/list", "jobs/read", "jobs/write", "jobs/cancel",
		"mcp/list", "mcp/enable", "mcp/disable",
		"vcs/log", "vcs/diff",
		"models/list",
	}
}

// dispatchCtl handles the operator methods. The bool reports whether the method
// belongs to this file at all, so server.go's switch stays flat.
func (s *Server) dispatchCtl(ctx context.Context, req RPCRequest) (any, *RPCError, bool) {
	switch req.Method {
	case "session/list":
		var p struct {
			Limit    int  `json:"limit"`
			Archived bool `json:"archived"`
		}
		if err := decodeParams(req.Params, &p); err != nil {
			return nil, &RPCError{Code: codeInvalidParams, Message: err.Error()}, true
		}
		rows, err := s.ctlService().Sessions(p.Limit, p.Archived)
		if rpcErr := ctlError(req.Method, err); rpcErr != nil {
			return nil, rpcErr, true
		}
		return map[string]any{"sessions": rows}, nil, true

	case "session/show":
		var p struct {
			ID string `json:"id"`
		}
		if err := decodeParams(req.Params, &p); err != nil {
			return nil, &RPCError{Code: codeInvalidParams, Message: err.Error()}, true
		}
		view, err := s.ctlService().Session(p.ID)
		if rpcErr := ctlError(req.Method, err); rpcErr != nil {
			return nil, rpcErr, true
		}
		return map[string]any{"session": view}, nil, true

	case "session/messages":
		var p struct {
			ID   string `json:"id"`
			Tail int    `json:"tail"`
		}
		if err := decodeParams(req.Params, &p); err != nil {
			return nil, &RPCError{Code: codeInvalidParams, Message: err.Error()}, true
		}
		msgs, err := s.ctlService().SessionMessages(p.ID, p.Tail)
		if rpcErr := ctlError(req.Method, err); rpcErr != nil {
			return nil, rpcErr, true
		}
		return map[string]any{"messages": msgs}, nil, true

	case "session/fork":
		var p struct {
			ID      string `json:"id"`
			UpToSeq *int   `json:"upToSeq"`
		}
		if err := decodeParams(req.Params, &p); err != nil {
			return nil, &RPCError{Code: codeInvalidParams, Message: err.Error()}, true
		}
		// A pointer so "the whole session" (-1) is expressible while a MISSING
		// field is not silently read as 0, which would fork an empty session.
		upTo := -1
		if p.UpToSeq != nil {
			upTo = *p.UpToSeq
		}
		newID, err := s.ctlService().ForkSession(p.ID, upTo)
		if rpcErr := ctlError(req.Method, err); rpcErr != nil {
			return nil, rpcErr, true
		}
		return map[string]any{"ok": true, "id": p.ID, "forkedId": newID}, nil, true

	case "session/rename":
		var p struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		}
		if err := decodeParams(req.Params, &p); err != nil {
			return nil, &RPCError{Code: codeInvalidParams, Message: err.Error()}, true
		}
		if rpcErr := ctlError(req.Method, s.ctlService().RenameSession(p.ID, p.Title)); rpcErr != nil {
			return nil, rpcErr, true
		}
		return map[string]any{"ok": true, "id": p.ID}, nil, true

	case "session/archive":
		var p struct {
			ID       string `json:"id"`
			Archived *bool  `json:"archived"`
		}
		if err := decodeParams(req.Params, &p); err != nil {
			return nil, &RPCError{Code: codeInvalidParams, Message: err.Error()}, true
		}
		// A pointer so "archive" and "unarchive" are ONE method: a missing
		// field is a caller bug, not a silent un-archive.
		if p.Archived == nil {
			return nil, &RPCError{Code: codeInvalidParams, Message: "session/archive needs archived: true|false"}, true
		}
		if rpcErr := ctlError(req.Method, s.ctlService().SetSessionArchived(p.ID, *p.Archived)); rpcErr != nil {
			return nil, rpcErr, true
		}
		return map[string]any{"ok": true, "id": p.ID, "archived": *p.Archived}, nil, true

	case "session/delete":
		var p struct {
			ID      string `json:"id"`
			Confirm string `json:"confirm"`
		}
		if err := decodeParams(req.Params, &p); err != nil {
			return nil, &RPCError{Code: codeInvalidParams, Message: err.Error()}, true
		}
		// Same discipline as the CLI's `session delete <id> yes` and the TUI's
		// /delete. A machine surface gets the same guard as the human one: an
		// id is easy to compute wrong, and this is the one irreversible call in
		// the file.
		if p.Confirm != "yes" {
			return nil, &RPCError{
				Code:    codeInvalidParams,
				Message: "session/delete requires confirm: \"yes\"",
			}, true
		}
		if rpcErr := ctlError(req.Method, s.ctlService().DeleteSession(p.ID)); rpcErr != nil {
			return nil, rpcErr, true
		}
		return map[string]any{"ok": true, "id": p.ID}, nil, true

	case "usage/get":
		var p struct {
			ID    string `json:"id"`
			Limit int    `json:"limit"`
		}
		if err := decodeParams(req.Params, &p); err != nil {
			return nil, &RPCError{Code: codeInvalidParams, Message: err.Error()}, true
		}
		usage, err := s.ctlService().Usage(p.ID, p.Limit)
		if rpcErr := ctlError(req.Method, err); rpcErr != nil {
			return nil, rpcErr, true
		}
		return usage, nil, true

	case "features/list":
		rows, err := s.ctlService().Features()
		if rpcErr := ctlError(req.Method, err); rpcErr != nil {
			return nil, rpcErr, true
		}
		return map[string]any{"features": rows}, nil, true

	case "features/set":
		var p struct {
			Key     string `json:"key"`
			Enabled bool   `json:"enabled"`
		}
		if err := decodeParams(req.Params, &p); err != nil {
			return nil, &RPCError{Code: codeInvalidParams, Message: err.Error()}, true
		}
		if rpcErr := ctlError(req.Method, s.ctlService().SetFeature(p.Key, p.Enabled)); rpcErr != nil {
			return nil, rpcErr, true
		}
		return map[string]any{"ok": true, "key": p.Key, "enabled": p.Enabled}, nil, true

	case "skills/list":
		rows, err := s.ctlService().Skills()
		if rpcErr := ctlError(req.Method, err); rpcErr != nil {
			return nil, rpcErr, true
		}
		return map[string]any{"skills": rows}, nil, true

	case "skills/show":
		var p struct {
			Name string `json:"name"`
		}
		if err := decodeParams(req.Params, &p); err != nil {
			return nil, &RPCError{Code: codeInvalidParams, Message: err.Error()}, true
		}
		view, body, err := s.ctlService().Skill(p.Name)
		if rpcErr := ctlError(req.Method, err); rpcErr != nil {
			return nil, rpcErr, true
		}
		return map[string]any{"skill": view, "body": body}, nil, true

	case "skills/enable", "skills/disable":
		var p struct {
			Name string `json:"name"`
		}
		if err := decodeParams(req.Params, &p); err != nil {
			return nil, &RPCError{Code: codeInvalidParams, Message: err.Error()}, true
		}
		err := s.ctlService().SetSkillEnabled(p.Name, req.Method == "skills/enable")
		if rpcErr := ctlError(req.Method, err); rpcErr != nil {
			return nil, rpcErr, true
		}
		return map[string]any{"ok": true, "name": p.Name, "enabled": req.Method == "skills/enable"}, nil, true

	case "skills/trust", "skills/untrust":
		var p struct {
			Name string `json:"name"`
		}
		if err := decodeParams(req.Params, &p); err != nil {
			return nil, &RPCError{Code: codeInvalidParams, Message: err.Error()}, true
		}
		err := s.ctlService().SetSkillTrusted(p.Name, req.Method == "skills/trust")
		if rpcErr := ctlError(req.Method, err); rpcErr != nil {
			return nil, rpcErr, true
		}
		return map[string]any{"ok": true, "name": p.Name, "trusted": req.Method == "skills/trust"}, nil, true

	case "approvals/list":
		var p struct {
			SessionID string `json:"sessionId"`
		}
		if err := decodeParams(req.Params, &p); err != nil {
			return nil, &RPCError{Code: codeInvalidParams, Message: err.Error()}, true
		}
		rows, err := s.ctlService().Approvals(p.SessionID)
		if rpcErr := ctlError(req.Method, err); rpcErr != nil {
			return nil, rpcErr, true
		}
		return map[string]any{"rules": rows}, nil, true

	case "approvals/revoke":
		var p struct {
			SessionID string `json:"sessionId"`
			ID        string `json:"id"`
		}
		if err := decodeParams(req.Params, &p); err != nil {
			return nil, &RPCError{Code: codeInvalidParams, Message: err.Error()}, true
		}
		if rpcErr := ctlError(req.Method, s.ctlService().RevokeApproval(p.SessionID, p.ID)); rpcErr != nil {
			return nil, rpcErr, true
		}
		return map[string]any{"ok": true, "id": p.ID}, nil, true

	case "jobs/list":
		rows, err := s.ctlService().Jobs()
		if rpcErr := ctlError(req.Method, err); rpcErr != nil {
			return nil, rpcErr, true
		}
		return map[string]any{"jobs": rows}, nil, true

	case "jobs/read":
		var p struct {
			ID  string `json:"id"`
			Max int    `json:"max"`
		}
		if err := decodeParams(req.Params, &p); err != nil {
			return nil, &RPCError{Code: codeInvalidParams, Message: err.Error()}, true
		}
		out, err := s.ctlService().ReadJob(p.ID, p.Max)
		if rpcErr := ctlError(req.Method, err); rpcErr != nil {
			return nil, rpcErr, true
		}
		return map[string]any{"id": p.ID, "output": out}, nil, true

	case "jobs/write":
		var p struct {
			ID   string `json:"id"`
			Data string `json:"data"`
		}
		if err := decodeParams(req.Params, &p); err != nil {
			return nil, &RPCError{Code: codeInvalidParams, Message: err.Error()}, true
		}
		n, err := s.ctlService().WriteJob(p.ID, p.Data)
		if rpcErr := ctlError(req.Method, err); rpcErr != nil {
			return nil, rpcErr, true
		}
		return map[string]any{"ok": true, "id": p.ID, "bytes": n}, nil, true

	case "jobs/cancel":
		var p struct {
			ID string `json:"id"`
		}
		if err := decodeParams(req.Params, &p); err != nil {
			return nil, &RPCError{Code: codeInvalidParams, Message: err.Error()}, true
		}
		if rpcErr := ctlError(req.Method, s.ctlService().CancelJob(p.ID)); rpcErr != nil {
			return nil, rpcErr, true
		}
		return map[string]any{"ok": true, "id": p.ID}, nil, true

	case "mcp/list":
		rows, err := s.ctlService().MCP(ctx)
		if rpcErr := ctlError(req.Method, err); rpcErr != nil {
			return nil, rpcErr, true
		}
		return map[string]any{"servers": rows}, nil, true

	case "mcp/enable", "mcp/disable":
		var p struct {
			Name string `json:"name"`
		}
		if err := decodeParams(req.Params, &p); err != nil {
			return nil, &RPCError{Code: codeInvalidParams, Message: err.Error()}, true
		}
		err := s.ctlService().SetMCPEnabled(ctx, p.Name, req.Method == "mcp/enable")
		if rpcErr := ctlError(req.Method, err); rpcErr != nil {
			return nil, rpcErr, true
		}
		return map[string]any{"ok": true, "name": p.Name, "enabled": req.Method == "mcp/enable"}, nil, true

	case "vcs/log":
		var p struct {
			Limit int `json:"limit"`
		}
		if err := decodeParams(req.Params, &p); err != nil {
			return nil, &RPCError{Code: codeInvalidParams, Message: err.Error()}, true
		}
		rows, err := s.ctlService().VCSLog(p.Limit)
		if rpcErr := ctlError(req.Method, err); rpcErr != nil {
			return nil, rpcErr, true
		}
		return map[string]any{"commits": rows}, nil, true

	case "vcs/diff":
		var p struct {
			From string `json:"from"`
			To   string `json:"to"`
		}
		if err := decodeParams(req.Params, &p); err != nil {
			return nil, &RPCError{Code: codeInvalidParams, Message: err.Error()}, true
		}
		rows, err := s.ctlService().VCSDiff(p.From, p.To)
		if rpcErr := ctlError(req.Method, err); rpcErr != nil {
			return nil, rpcErr, true
		}
		return map[string]any{"files": rows}, nil, true

	case "models/list":
		return map[string]any{"models": s.ctlService().Models()}, nil, true
	}
	return nil, nil, false
}

// ctlService returns the control plane or a stand-in that reports the missing
// wiring, so a daemon assembled without one still answers these methods with a
// diagnosable error.
func (s *Server) ctlService() *ctl.Service {
	if s.ctl == nil {
		return ctl.New(ctl.Deps{})
	}
	return s.ctl
}

var _ = json.Marshal
var _ = fmt.Sprintf
