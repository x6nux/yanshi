package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/proto"
)

// pendingPermission returns the FRONT of the permission queue.
func (m model) pendingPermission() *permissionEntry {
	if len(m.pendingPermissions) == 0 {
		return nil
	}
	return m.pendingPermissions[0]
}

func (m model) sendMode() (tea.Model, tea.Cmd) {
	m.autoResolvePendingByMode()
	threshold := m.autoThreshold
	if threshold == 0 && m.permMode == guard.ModeAuto {
		threshold = guard.DefaultAutoThreshold
	}
	savePermMode(m.permMode, threshold)
	_ = m.sess.SendFrame(proto.NewSetMode(string(m.permMode), threshold))
	m.reflow()
	return m, nil
}

func (m model) cycleMode() (tea.Model, tea.Cmd) {
	if m.yoloConfirm > 0 {
		m.yoloConfirm = 0
		// User pressed Shift+Tab at the YOLO gate -> skip yolo and
		// continue to the next mode (auto). Advance twice: current ->
		// yolo -> next.
		next := guard.CycleMode(m.permMode)
		next = guard.CycleMode(next)
		m.permMode = next
		if next == guard.ModeAuto && m.autoThreshold == 0 {
			m.autoThreshold = guard.DefaultAutoThreshold
		}
		return m.sendMode()
	}
	next := guard.CycleMode(m.permMode)
	if next == guard.ModeYOLO {
		m.yoloConfirm = 1
		return m, nil
	}
	m.permMode = next
	if next == guard.ModeAuto && m.autoThreshold == 0 {
		m.autoThreshold = guard.DefaultAutoThreshold
	}
	return m.sendMode()
}

// permModeText returns the plain-text representation of the current permission
// mode, without any styling (used by the Powerline footer renderer which
// applies its own background/foreground colours per segment).
func (m model) permModeText() string {
	switch m.permMode {
	case guard.ModeDefault, "":
		return "manual mode"
	case guard.ModeAllowEdits:
		return "edit mode"
	case guard.ModeAuto:
		t := m.autoThreshold
		if t == 0 {
			t = guard.DefaultAutoThreshold
		}
		return fmt.Sprintf("auto ≤%d", t)
	case guard.ModeYOLO:
		return "bypass permissions"
	default:
		return "manual mode"
	}
}

func modeAutoAllows(mode guard.PermissionMode, tool string) bool {
	switch mode {
	case guard.ModeYOLO:
		return true
	case guard.ModeAllowEdits:
		return guard.IsEditTool(tool)
	}
	return false
}

func (m *model) autoResolvePendingByMode() {
	if len(m.pendingPermissions) == 0 {
		return
	}
	kept := make([]*permissionEntry, 0, len(m.pendingPermissions))
	for _, pe := range m.pendingPermissions {
		// pe.mandatory covers BOTH server flags (approval_required and
		// force_prompt): mandatory-approval tools (e.g. GitHub mutations),
		// force-prompt tools (task_cancel) and RequireApproval's destructive
		// actions (revert_turn) all survive YOLO/Auto — the user MUST click
		// Allow explicitly each time. Answering "allow" here on their behalf
		// would fabricate an authorization gesture that never happened.
		if !pe.mandatory && modeAutoAllows(m.permMode, pe.tool) {
			_ = m.sess.SendFrame(proto.NewPermissionResponse(pe.id, "allow"))
		} else {
			kept = append(kept, pe)
		}
	}
	m.pendingPermissions = kept
	m.permSel = 0
}

// permOptions are the permission popup's choices in display order; permSel
// indexes this slice. "Allow" first (default) since most prompts approve a
// legitimate tool call; "Deny" last. Task 9 split the old "Always allow" into
// "Allow this session" (TTL=session, dropped at process exit) and "Persistent
// allow" (TTL=persistent, mirrored to KV and survives restarts) so the operator
// can pick the matching durability. "always_allow" stays accepted on the wire
// as an alias for "allow_session" for one release (legacy clients still send it).
var permOptions = []struct {
	label, decision string
}{
	{"Allow", "allow"},
	{"Allow this session", "allow_session"},
	{"Persistent allow", "allow_persistent"},
	{"Deny", "deny"},
}

// permissionOptions returns the popup choices for the current pending permission.
// Mandatory-approval tools get only Allow/Deny (no session/persistent allow);
// regular tools get the full set.
func permissionOptions(pe *permissionEntry) []struct{ label, decision string } {
	if pe != nil && pe.mandatory {
		return []struct{ label, decision string }{
			{"Allow", "allow"},
			{"Deny", "deny"},
		}
	}
	return permOptions
}

// permMove moves the permission popup's selection by delta (wrapping). No-op
// when no prompt is pending.
func (m *model) permMove(delta int) {
	pe := m.pendingPermission()
	if pe == nil {
		return
	}
	opts := permissionOptions(pe)
	n := len(opts)
	m.permSel = ((m.permSel+delta)%n + n) % n
}

// respondPermission resolves the pending permission prompt with a decision
// ("allow" | "always_allow" | "deny"), clears the popup, and sends the response
// frame. No-op when no prompt is pending. Mandatory-approval tools reject
// "always_allow" / "allow_session" / "allow_persistent" — only "allow" and
// "deny" are valid; an invalid choice is silently dropped (the popup stays).
func (m *model) respondPermission(decision string) {
	pe := m.pendingPermission()
	if pe == nil {
		return
	}
	if pe.mandatory && decision != "allow" && decision != "deny" {
		return
	}
	m.pendingPermissions = m.pendingPermissions[1:]
	m.permSel = 0
	// SendFrame for permission_response returns nil (no direct reply) — fire
	// and forget; the turn's tool will proceed/deny based on the decision.
	_ = m.sess.SendFrame(proto.NewPermissionResponse(pe.id, decision))
}

// permModeFile returns the path to the persisted permission mode JSON file.
func permModeFile() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "yanshi", "perm_mode.json")
}

type permModeSave struct {
	Mode      string `json:"mode"`
	Threshold int    `json:"threshold"`
}

// persistPermMode is disabled by the TUI test package so mode-cycling tests do
// not share the developer's real config file or influence one another.
var persistPermMode = true

func savePermMode(mode guard.PermissionMode, threshold int) {
	if !persistPermMode {
		return
	}
	path := permModeFile()
	if path == "" {
		return
	}
	os.MkdirAll(filepath.Dir(path), 0755)
	data, _ := json.Marshal(permModeSave{Mode: string(mode), Threshold: threshold})
	if data != nil {
		os.WriteFile(path, data, 0644)
	}
}

func loadSavedMode() guard.PermissionMode {
	if !persistPermMode {
		return guard.ModeDefault
	}
	path := permModeFile()
	if path == "" {
		return guard.ModeDefault
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return guard.ModeDefault
	}
	var s permModeSave
	if json.Unmarshal(data, &s) != nil {
		return guard.ModeDefault
	}
	pm, ok := guard.NormalizeMode(s.Mode)
	if !ok {
		return guard.ModeDefault
	}
	return pm
}

func loadSavedThreshold() int {
	if !persistPermMode {
		return 0
	}
	path := permModeFile()
	if path == "" {
		return 0
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	var s permModeSave
	if json.Unmarshal(data, &s) != nil {
		return 0
	}
	if s.Threshold < 1 || s.Threshold > 10 {
		return 0
	}
	return s.Threshold
}
