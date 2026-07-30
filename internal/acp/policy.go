package acp

import (
	"fmt"
	"sync"

	"github.com/x6nux/yanshi/internal/guard"
)

// Policy gates inbound server->client requests (fs ops, terminal, permission
// prompts) that arrive during a session/prompt turn. Every method is
// fail-closed: if the guard denies the action the method returns a non-nil
// error, which the transport turns into a JSON-RPC error response so the
// agent sees the denial.
type Policy interface {
	// OnPermission decides a session/request_permission request.
	// Returns the outcome to send back to the agent.
	OnPermission(p RequestPermissionParams) PermissionOutcome
	// OnFSRead gates fs/read_text_file. nil = allow; non-nil = deny.
	OnFSRead(path string) error
	// OnFSWrite gates fs/write_text_file. nil = allow; non-nil = deny.
	OnFSWrite(path string) error
	// OnTerminal gates terminal/create. nil = allow; non-nil = deny.
	OnTerminal(cmd string) error
}

// GuardPolicy implements Policy over a guard.PermissionProfile.
// All checks are fail-closed: if the guard denies the action, an error is
// returned and the transport writes a JSON-RPC error response.
type GuardPolicy struct {
	Profile guard.PermissionProfile
	g       *guard.Guard

	// trackedCalls maps toolCallId → Update (Kind/Title) seen via
	// session/update tool_call notifications. Guarded by callMu.
	// OnPermission uses this to determine the action kind so it can
	// consult the guard before auto-allowing.
	callMu  sync.Mutex
	calls   map[string]Update
}

// NewGuardPolicy creates a GuardPolicy backed by the given permission profile.
func NewGuardPolicy(p guard.PermissionProfile) *GuardPolicy {
	return &GuardPolicy{Profile: p, g: guard.New(), calls: make(map[string]Update)}
}

// TrackToolCall records a tool call update so OnPermission can look up
// the call's Kind and consult the guard. This is called by the Client
// when it receives a session/update with SessionUpdate "tool_call" or
// "tool_call_update".
func (gp *GuardPolicy) TrackToolCall(toolCallID string, upd Update) {
	gp.callMu.Lock()
	defer gp.callMu.Unlock()
	gp.calls[toolCallID] = upd
}

// OnFSRead checks the path against the profile's FS.Read patterns.
func (gp *GuardPolicy) OnFSRead(path string) error {
	d := gp.g.Check(gp.Profile, guard.Action{
		Tool: "fs.read",
		FS:   guard.FSWant{Op: "read", Paths: []string{path}},
	})
	if !d.IsAllowed() {
		return fmt.Errorf("acp: fs read denied: %s", d.Reason)
	}
	return nil
}

// OnFSWrite checks the path against the profile's FS.Write patterns.
func (gp *GuardPolicy) OnFSWrite(path string) error {
	d := gp.g.Check(gp.Profile, guard.Action{
		Tool: "fs.write",
		FS:   guard.FSWant{Op: "write", Paths: []string{path}},
	})
	if !d.IsAllowed() {
		return fmt.Errorf("acp: fs write denied: %s", d.Reason)
	}
	return nil
}

// OnTerminal checks the command against the profile's shell policy.
func (gp *GuardPolicy) OnTerminal(cmd string) error {
	d := gp.g.Check(gp.Profile, guard.Action{
		Tool:  "terminal.run",
		Shell: cmd,
	})
	if !d.IsAllowed() {
		return fmt.Errorf("acp: terminal denied: %s", d.Reason)
	}
	return nil
}

// OnPermission inspects the permission request by looking up the tracked
// tool call's Kind, mapping it to a guard action, and checking the guard.
// If the guard allows the action, an allow option is selected. Otherwise
// the outcome is "cancelled" (fail-closed). Untracked tool calls are also
// cancelled.
func (gp *GuardPolicy) OnPermission(p RequestPermissionParams) PermissionOutcome {
	// Look up the tracked tool call to determine the action kind.
	gp.callMu.Lock()
	upd, ok := gp.calls[p.ToolCall.ToolCallID]
	gp.callMu.Unlock()

	if !ok {
		// Untracked tool call — fail-closed.
		return PermissionOutcome{Outcome: "cancelled"}
	}

	// Map the tool call Kind to a guard Action and run pre-checks for
	// FS operations (the guard's FS check needs a path, but OnPermission
	// doesn't have one; instead we check that the profile has at least
	// one pattern for the operation — if none, deny).
	var action guard.Action
	switch upd.Kind {
	case "read", "search":
		if len(gp.Profile.FS.Read) == 0 {
			return PermissionOutcome{Outcome: "cancelled"}
		}
		action = guard.Action{Tool: "fs.read"}
	case "edit", "delete", "move":
		if len(gp.Profile.FS.Write) == 0 {
			return PermissionOutcome{Outcome: "cancelled"}
		}
		action = guard.Action{Tool: "fs.write"}
	case "execute":
		action = guard.Action{Tool: "shell.run", Shell: upd.Title}
	case "fetch":
		if !gp.Profile.Net.Allow {
			return PermissionOutcome{Outcome: "cancelled"}
		}
		action = guard.Action{Tool: "web_fetch"}
	case "think", "switch_mode":
		// No side effect — allow without guard check.
		return selectAllow(p.Options)
	default:
		// Unknown kind — fail-closed.
		return PermissionOutcome{Outcome: "cancelled"}
	}

	d := gp.g.Check(gp.Profile, action)
	if !d.IsAllowed() {
		return PermissionOutcome{Outcome: "cancelled"}
	}
	return selectAllow(p.Options)
}

// selectAllow picks the first allow_once/allow_always option, or cancels
// if none is available.
func selectAllow(options []PermissionOption) PermissionOutcome {
	for _, opt := range options {
		if opt.Kind == "allow_once" || opt.Kind == "allow_always" {
			return PermissionOutcome{
				Outcome:  "selected",
				OptionID: opt.OptionID,
			}
		}
	}
	return PermissionOutcome{Outcome: "cancelled"}
}
