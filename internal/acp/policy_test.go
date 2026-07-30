package acp

import (
	"testing"

	"github.com/x6nux/yanshi/internal/guard"
)

// TestPolicy_OnPermission_GuardDenies verifies that OnPermission consults
// the guard and cancels when the tool call kind maps to a denied action.
func TestPolicy_OnPermission_GuardDenies(t *testing.T) {
	t.Parallel()

	// Profile with empty FS.Write — all writes denied.
	gp := NewGuardPolicy(guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
		FS:    guard.FSPerm{}, // no read or write patterns
	})

	// Track a tool call with Kind="edit" (maps to fs.write, which is denied).
	gp.TrackToolCall("tc_edit", Update{Kind: "edit"})

	outcome := gp.OnPermission(RequestPermissionParams{
		ToolCall: ToolCallRef{ToolCallID: "tc_edit"},
		Options: []PermissionOption{
			{OptionID: "opt_allow", Name: "Allow", Kind: "allow_once"},
			{OptionID: "opt_deny", Name: "Deny", Kind: "deny"},
		},
	})

	assertEqual(t, "cancelled", outcome.Outcome, "edit should be denied when FS.Write is empty")
	assertEqual(t, "", outcome.OptionID, "no option should be selected when denied")
}

// TestPolicy_OnPermission_GuardAllows verifies that OnPermission selects an
// allow option when the guard permits the action.
func TestPolicy_OnPermission_GuardAllows(t *testing.T) {
	t.Parallel()

	// Profile that allows reads from /tmp/**.
	gp := NewGuardPolicy(guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
		FS:    guard.FSPerm{Read: []string{"/tmp/**"}},
	})

	// Track a tool call with Kind="read" (maps to fs.read, which is allowed
	// because FS.Read has at least one pattern).
	gp.TrackToolCall("tc_read", Update{Kind: "read"})

	outcome := gp.OnPermission(RequestPermissionParams{
		ToolCall: ToolCallRef{ToolCallID: "tc_read"},
		Options: []PermissionOption{
			{OptionID: "opt_allow", Name: "Allow", Kind: "allow_once"},
			{OptionID: "opt_deny", Name: "Deny", Kind: "deny"},
		},
	})

	assertEqual(t, "selected", outcome.Outcome, "read should be allowed when FS.Read has patterns")
	assertEqual(t, "opt_allow", outcome.OptionID, "should select the allow option")
}

// TestPolicy_OnPermission_UntrackedFailClosed verifies that an untracked
// tool call is cancelled (fail-closed).
func TestPolicy_OnPermission_UntrackedFailClosed(t *testing.T) {
	t.Parallel()

	gp := NewGuardPolicy(guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
	})

	// No tool call tracked for "tc_unknown".
	outcome := gp.OnPermission(RequestPermissionParams{
		ToolCall: ToolCallRef{ToolCallID: "tc_unknown"},
		Options: []PermissionOption{
			{OptionID: "opt_allow", Name: "Allow", Kind: "allow_once"},
		},
	})

	assertEqual(t, "cancelled", outcome.Outcome, "untracked tool call should be cancelled")
}

// TestPolicy_OnPermission_ThinkAutoAllows verifies that "think" and
// "switch_mode" kinds are auto-allowed without a guard check.
func TestPolicy_OnPermission_ThinkAutoAllows(t *testing.T) {
	t.Parallel()

	// Even with a restrictive profile, think/switch_mode should be allowed.
	gp := NewGuardPolicy(guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
		Shell: guard.ShellPerm{Policy: "deny"},
		FS:    guard.FSPerm{},
		Net:   guard.NetPerm{Allow: false},
	})

	gp.TrackToolCall("tc_think", Update{Kind: "think"})
	outcome := gp.OnPermission(RequestPermissionParams{
		ToolCall: ToolCallRef{ToolCallID: "tc_think"},
		Options: []PermissionOption{
			{OptionID: "opt_allow", Name: "Allow", Kind: "allow_once"},
		},
	})
	assertEqual(t, "selected", outcome.Outcome, "think should be auto-allowed")
	assertEqual(t, "opt_allow", outcome.OptionID, "should select the allow option")

	gp.TrackToolCall("tc_switch", Update{Kind: "switch_mode"})
	outcome = gp.OnPermission(RequestPermissionParams{
		ToolCall: ToolCallRef{ToolCallID: "tc_switch"},
		Options: []PermissionOption{
			{OptionID: "opt_allow", Name: "Allow", Kind: "allow_once"},
		},
	})
	assertEqual(t, "selected", outcome.Outcome, "switch_mode should be auto-allowed")
}

// TestPolicy_OnPermission_UnknownKindDenied verifies that an unknown Kind
// is denied (fail-closed).
func TestPolicy_OnPermission_UnknownKindDenied(t *testing.T) {
	t.Parallel()

	gp := NewGuardPolicy(guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
	})

	gp.TrackToolCall("tc_unknown_kind", Update{Kind: "bogus_kind"})
	outcome := gp.OnPermission(RequestPermissionParams{
		ToolCall: ToolCallRef{ToolCallID: "tc_unknown_kind"},
		Options: []PermissionOption{
			{OptionID: "opt_allow", Name: "Allow", Kind: "allow_once"},
		},
	})
	assertEqual(t, "cancelled", outcome.Outcome, "unknown kind should be denied")
}

// assertEqual is a minimal helper to avoid pulling in testify in this file.
func assertEqual(t *testing.T, expected, actual any, msg string) {
	t.Helper()
	if expected != actual {
		t.Errorf("%s: expected %v, got %v", msg, expected, actual)
	}
}

// TestPolicy_OnPermission_FetchRespectsNetOff verifies that a "fetch" tool
// call is cancelled when Net.Allow is false, even if Tools.Allow includes "*".
// When Net.Allow is true, it should be allowed.
func TestPolicy_OnPermission_FetchRespectsNetOff(t *testing.T) {
	t.Parallel()

	// Net off — fetch must be cancelled even though Tools.Allow=["*"].
	gpOff := NewGuardPolicy(guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
		Net:   guard.NetPerm{Allow: false},
	})
	gpOff.TrackToolCall("tc_fetch_off", Update{Kind: "fetch"})
	outcome := gpOff.OnPermission(RequestPermissionParams{
		ToolCall: ToolCallRef{ToolCallID: "tc_fetch_off"},
		Options: []PermissionOption{
			{OptionID: "opt_allow", Name: "Allow", Kind: "allow_once"},
		},
	})
	assertEqual(t, "cancelled", outcome.Outcome, "fetch should be cancelled when Net.Allow=false")

	// Net on — fetch should be allowed.
	gpOn := NewGuardPolicy(guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
		Net:   guard.NetPerm{Allow: true},
	})
	gpOn.TrackToolCall("tc_fetch_on", Update{Kind: "fetch"})
	outcome = gpOn.OnPermission(RequestPermissionParams{
		ToolCall: ToolCallRef{ToolCallID: "tc_fetch_on"},
		Options: []PermissionOption{
			{OptionID: "opt_allow", Name: "Allow", Kind: "allow_once"},
		},
	})
	assertEqual(t, "selected", outcome.Outcome, "fetch should be allowed when Net.Allow=true")
	assertEqual(t, "opt_allow", outcome.OptionID, "should select the allow option")
}
