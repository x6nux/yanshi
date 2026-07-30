package keymap

// VimMode is the current modal state.
type VimMode int

// Vim modal states.
const (
	VimModeNormal VimMode = iota
	VimModeInsert
	VimModeVisual
)

// VimMachine is the modal state machine. D3 Visual mode tracks mode and maps
// j/k to viewport navigation only; it does not claim text-selection
// extension. The state lives on the TUI model so a model.Reset() between
// sessions restores the default Insert mode.
type VimMachine struct {
	mode VimMode
}

// NewVimMachine returns a machine starting in Insert mode (matching the
// textarea's default editable state — users see a cursor they can type
// into immediately, and Escape drops them into Normal).
func NewVimMachine() *VimMachine { return &VimMachine{mode: VimModeInsert} }

// Mode returns the current modal state.
func (v *VimMachine) Mode() VimMode { return v.mode }

// SetMode is used by tests and by the TUI when restoring mode after a
// transient state (e.g. exiting command palette). Production callers should
// prefer HandleKey so transitions stay consistent.
func (v *VimMachine) SetMode(m VimMode) { v.mode = m }

// VimResult separates semantic dispatch from input ownership. Consumed=true
// with ActionNone is load-bearing for i/a/o/v/Esc: the transition key must
// not leak into the textarea (typing "i" in Normal mode should switch to
// Insert, not insert an "i" character).
type VimResult struct {
	Action   Action
	Consumed bool
}

// HandleKey consults the current modal state plus any globally-configured
// action. A non-zero configured action always wins (so ctrl+c cancels in
// every mode); otherwise the modal table dispatches j/k/esc/i/a/o/v.
func (v *VimMachine) HandleKey(key string, configured Action) VimResult {
	// Explicit configured actions remain global in every mode.
	if configured != ActionNone {
		return VimResult{Action: configured, Consumed: true}
	}
	switch v.mode {
	case VimModeInsert:
		if key == "esc" {
			v.mode = VimModeNormal
			return VimResult{Consumed: true}
		}
		return VimResult{}
	case VimModeNormal:
		switch key {
		case "i", "a", "o":
			v.mode = VimModeInsert
			return VimResult{Consumed: true}
		case "v":
			v.mode = VimModeVisual
			return VimResult{Consumed: true}
		case "j":
			return VimResult{Action: ActionScrollDown, Consumed: true}
		case "k":
			return VimResult{Action: ActionScrollUp, Consumed: true}
		default:
			return VimResult{Consumed: true}
		}
	case VimModeVisual:
		switch key {
		case "esc":
			v.mode = VimModeNormal
			return VimResult{Consumed: true}
		case "j":
			return VimResult{Action: ActionScrollDown, Consumed: true}
		case "k":
			return VimResult{Action: ActionScrollUp, Consumed: true}
		default:
			return VimResult{Consumed: true}
		}
	default:
		return VimResult{}
	}
}

// effectiveVimMode implements the *bool tri-state merge (structural fix #9):
//   - nil (unset): use prefsDefault
//   - false (explicit): force off
//   - true (explicit): force on
//
// Exposed for tests; production TUI calls this through
// (EffectivePreferences).Vim which already collapsed the tri-state during
// mergeTUIPrefs.
func effectiveVimMode(flag *bool, prefsDefault bool) bool {
	if flag == nil {
		return prefsDefault
	}
	return *flag
}
