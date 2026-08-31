// internal/cli/tui/commands_prefs.go
//
// The four preference slash commands: /keymap, /vim, /contrast and /locale.
//
// They live in their own file rather than in commands.go for a mechanical
// reason: commands.go is the largest non-test file in internal/cli/tui and
// sits inside GOV2's "approaching" band, so four handlers with argument
// parsing, diagnostic rendering and persistence error handling would push it
// over the 1000 pure-code-line cap. commands.go gains only the four table
// rows, matching how commands_skills.go / commands_logs.go / commands_stats.go
// were split off before.

package tui

import (
	"fmt"
	"os"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/x6nux/yanshi/internal/i18n"
	"github.com/x6nux/yanshi/internal/keymap"
)

// savePrefs persists the USER layer and reports failure in the transcript.
//
// Only the user layer is written. A value that arrived from project config or
// the environment must not be copied into prefs.json: doing so would pin it
// against the project's own future changes, and the user never asked for that.
//
// A write failure is surfaced, not swallowed. The preference still takes
// effect for this session, so silently failing would produce the worst
// version of the bug: it works until you restart.
func (m model) savePrefs() model {
	if m.prefsPath == "" {
		return m
	}
	if err := persistPreferences(m.prefsPath, m.prefs); err != nil {
		m.entries = append(m.entries, errorEntry{
			text: m.bundle.GetF("tui.command.preference.persist_failed",
				map[string]string{"error": err.Error()}),
		})
	}
	return m
}

// remerge recomputes the effective cascade after the user layer changed, so a
// command's effect is visible immediately rather than only after a restart.
// The precedence is unchanged: env and flags still beat a fresh user value,
// which is correct — an operator who exported YANSHI_VIM=0 for this shell
// meant it.
func (m model) remerge() model {
	env, _ := PreferencesFromEnv(os.Getenv)
	m.effective = mergeTUIPrefs(Preferences{}, env, m.prefs, m.project)
	m.theme = themeForPrefs(m.effective)
	// A session /theme wins over the cascade until the session ends. Without
	// this, running any preference command after /theme silently reverted the
	// colours, with nothing on screen connecting the two.
	//
	// /contrast is the one exception: it is an accessibility switch, and
	// turning it on has to beat a theme choice or it does not work.
	if m.themeOverride != "" && !m.effective.HighContrast {
		m.theme = m.themeOverride
	}
	m.keys = buildKeymap(m.effective, m.project)
	// Vim mode is a live state machine, not a flag someone reads later: it
	// owns keystrokes. Constructed on transition to on, dropped on transition
	// to off so the normal editing path is byte-identical when it is off.
	switch {
	case m.effective.Vim && m.vim == nil:
		m.vim = keymap.NewVimMachine()
	case !m.effective.Vim:
		m.vim = nil
	}
	return m
}

func cmdVim(m model, args []string) (tea.Model, tea.Cmd) {
	on, ok := parseOnOff(args)
	if !ok {
		m.entries = append(m.entries, errorEntry{text: m.bundle.Get("tui.command.vim.usage")})
		m.refresh()
		return m, nil
	}
	m.prefs.Vim = &on
	m = m.savePrefs().remerge()
	key := "tui.command.vim.disabled"
	if m.effective.Vim {
		key = "tui.command.vim.enabled"
	}
	m.entries = append(m.entries, ackEntry{text: m.bundle.Get(key)})
	m.refresh()
	return m, nil
}

func cmdContrast(m model, args []string) (tea.Model, tea.Cmd) {
	on, ok := parseOnOff(args)
	if !ok {
		m.entries = append(m.entries, errorEntry{text: m.bundle.Get("tui.command.contrast.usage")})
		m.refresh()
		return m, nil
	}
	m.prefs.HighContrast = &on
	m = m.savePrefs().remerge()
	key := "contrast.disabled"
	if m.effective.HighContrast {
		key = "contrast.enabled"
	}
	m.entries = append(m.entries, ackEntry{text: m.bundle.Get(key)})
	m.refresh()
	return m, nil
}

// cmdLocale switches the UI language.
//
// It persists bundle.Persistent(), never bundle.Effective(). "auto" means
// "re-resolve from LC_ALL/LANG at every startup"; writing the resolved value
// would freeze whatever this machine happened to answer today, turning a
// follow-the-system setting into a hardcoded one with nothing on screen to say
// so. See i18n.Bundle.Persistent.
func cmdLocale(m model, args []string) (tea.Model, tea.Cmd) {
	if len(args) == 0 {
		m.entries = append(m.entries, ackEntry{
			text: m.bundle.GetF("tui.command.locale.current", map[string]string{
				"declared":  m.effective.UILocale,
				"effective": m.bundle.Effective(),
			}),
		})
		m.refresh()
		return m, nil
	}
	want := strings.TrimSpace(args[0])
	bundle, err := i18n.NewBundle(want)
	if err != nil {
		m.entries = append(m.entries, errorEntry{text: m.bundle.Get("tui.command.locale.usage")})
		m.refresh()
		return m, nil
	}
	m.bundle = bundle
	m.prefs.UILocale = bundle.Persistent()
	m = m.savePrefs().remerge()
	m.input.Placeholder = m.bundle.Get("tui.input.placeholder")
	m.entries = append(m.entries, ackEntry{
		text: m.bundle.GetF("tui.command.locale.changed", map[string]string{
			"declared":  bundle.Persistent(),
			"effective": bundle.Effective(),
		}),
	})
	m.refresh()
	return m, nil
}

// cmdKeymap handles /keymap [name|reset|diagnostics|bind|list].
//
// The diagnostics subcommand is not a convenience: internal/cli's
// checkKeymapConfig tells the operator verbatim to run "/keymap diagnostics"
// when their bindings fail validation, so for as long as this did not exist,
// the single piece of remedial advice the product offered was itself a dead
// end.
//
// W-E-15: "bind <action>" enters the capture wizard. "list" shows all active
// bindings. Both require no server round-trip — pure preference state.
func cmdKeymap(m model, args []string) (tea.Model, tea.Cmd) {
	if len(args) == 0 {
		m.entries = append(m.entries, ackEntry{
			text: m.bundle.GetF("tui.command.keymap.current",
				map[string]string{"name": m.effective.KeymapName}),
		})
		m.refresh()
		return m, nil
	}
	switch strings.ToLower(strings.TrimSpace(args[0])) {
	case "diagnostics":
		m.entries = append(m.entries, ackEntry{text: renderKeymapDiagnostics(m)})
	case "reset":
		// The tombstone. A stored true keeps built-in defaults even while
		// project config still carries tui.bindings — without it, "reset"
		// would last exactly until the next startup re-applied the project's
		// bindings, which is not a reset.
		yes := true
		m.prefs.KeymapReset = &yes
		m.prefs.KeymapName = ""
		m = m.savePrefs().remerge()
		m.entries = append(m.entries, ackEntry{text: m.bundle.Get("keymap.diagnostics.reset")})
	case "list":
		// W-E-15: show all currently active bindings.
		m.entries = append(m.entries, ackEntry{text: renderKeymapList(m)})
	case "bind":
		// W-E-15: enter capture mode for the named action. The next
		// keypress (other than Esc to cancel) becomes the binding.
		if len(args) < 2 {
			m.entries = append(m.entries, errorEntry{
				text: "usage: /keymap bind <action>  (actions: " + keymapActionNames() + ")",
			})
			m.refresh()
			return m, nil
		}
		action := strings.TrimSpace(args[1])
		if !isKnownKeymapAction(action) {
			m.entries = append(m.entries, errorEntry{
				text: "unknown action \"" + action + "\" (known: " + keymapActionNames() + ")",
			})
			m.refresh()
			return m, nil
		}
		m.keymapCapture = action
		m.entries = append(m.entries, ackEntry{
			text: "press the key to bind to \"" + action + "\" (Esc to cancel)",
		})
		m.refresh()
		return m, nil
	default:
		name := strings.TrimSpace(args[0])
		if name != "default" {
			m.entries = append(m.entries, errorEntry{
				text: m.bundle.GetF("keymap.diagnostics.unsupported_keymap",
					map[string]string{"name": name}),
			})
			m.refresh()
			return m, nil
		}
		m.prefs.KeymapName = name
		no := false
		m.prefs.KeymapReset = &no
		m = m.savePrefs().remerge()
		m.entries = append(m.entries, ackEntry{
			text: m.bundle.GetF("tui.command.keymap.current", map[string]string{"name": name}),
		})
	}
	m.refresh()
	return m, nil
}

// commitKeymapCapture processes a keypress while the /keymap bind wizard is
// active. It validates the key, checks for conflicts against the combined
// project + user bindings, and atomically writes the new binding to prefs.
//
// Conflict detection: if the key is already assigned to another action, the
// write is blocked — only the conflict message is shown, no partial write.
// The test for this is TestKeymapCaptureConflictBlocksWrite (keymap_wizard_test.go).
func (m model) commitKeymapCapture(msg tea.KeyMsg) (model, tea.Cmd) {
	action := m.keymapCapture
	m.keymapCapture = ""

	normalized, ok := keymap.NormalizeKey(msg)
	if !ok {
		m.entries = append(m.entries, errorEntry{
			text: "key cannot be bound (paste, multi-rune, or unsupported key type)",
		})
		m.reflow()
		return m, nil
	}

	// Conflict check: build the effective binding map and see if the key is
	// already assigned to a different action.
	effective := buildKeymap(m.effective, m.project)
	if existing := effective.Lookup(msg); existing != keymap.ActionNone && string(existing) != action {
		m.entries = append(m.entries, errorEntry{
			text: "conflict: " + normalized + " is already bound to \"" + string(existing) +
				"\" — unbind it first with /keymap bind " + string(existing),
		})
		m.reflow()
		// Conflict detected: write blocked. Test: TestKeymapCaptureConflictBlocksWrite
		return m, nil
	}

	// Write the binding atomically via the existing persistPreferences machinery.
	if m.prefs.KeymapBindings == nil {
		m.prefs.KeymapBindings = make(map[string]string)
	}
	m.prefs.KeymapBindings[normalized] = action
	m = m.savePrefs().remerge()
	m.entries = append(m.entries, ackEntry{
		text: "bound: " + normalized + " → " + action,
	})
	m.reflow()
	return m, nil
}
func renderKeymapList(m model) string {
	if len(m.effective.KeymapBindings) == 0 {
		return "no custom user-level bindings (use /keymap bind <action> to add one)"
	}
	keys := make([]string, 0, len(m.effective.KeymapBindings))
	for k := range m.effective.KeymapBindings {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	sb.WriteString("user key bindings:\n")
	for _, k := range keys {
		sb.WriteString("  " + k + " → " + m.effective.KeymapBindings[k] + "\n")
	}
	return sb.String()
}

// isKnownKeymapAction reports whether action is a rebindable action name.
func isKnownKeymapAction(action string) bool {
	for _, a := range keymapActions() {
		if string(a) == action {
			return true
		}
	}
	return false
}

// keymapActionNames returns a comma-separated list of rebindable action names
// for error messages.
func keymapActionNames() string {
	as := keymapActions()
	names := make([]string, len(as))
	for i, a := range as {
		names[i] = string(a)
	}
	return strings.Join(names, ", ")
}

// keymapActions returns the rebindable action set. ActionNone is excluded
// because it is not a real action but the zero value, and ActionSend /
// ActionNewline are excluded — those are handled by the textarea's own Enter
// logic and the custom bubbletea fork, and rebinding them without the same
// wiring would confuse users.
func keymapActions() []keymap.Action {
	return []keymap.Action{
		keymap.ActionCancel, keymap.ActionScrollUp, keymap.ActionScrollDown,
		keymap.ActionClear, keymap.ActionHelp, keymap.ActionQuit,
		keymap.ActionCommandMode,
	}
}

// renderKeymapDiagnostics turns Map.Diagnostics into the localized report.
// All four diagnostic kinds have catalog entries; an unrecognized kind is
// printed raw rather than dropped, because a silently missing diagnostic is
// the failure mode this whole command exists to prevent.
func renderKeymapDiagnostics(m model) string {
	if m.keys == nil {
		return m.bundle.Get("tui.command.keymap.none")
	}
	ds := m.keys.Diagnostics()
	if len(ds) == 0 {
		return m.bundle.Get("tui.command.keymap.none")
	}
	lines := make([]string, 0, len(ds))
	for _, d := range ds {
		lines = append(lines, formatKeymapDiagnostic(m.bundle, d))
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func formatKeymapDiagnostic(b *i18n.Bundle, d keymap.Diagnostic) string {
	// Diagnostic carries the raw spellings and the actions that collided; the
	// catalog strings name them {a}/{b} for a conflict and {action} for an
	// unknown action, so both are flattened here rather than in the catalog.
	vars := map[string]string{
		"key":    d.Key,
		"name":   d.Key,
		"action": joinActions(d.Actions),
		"a":      actionAt(d.Actions, 0),
		"b":      actionAt(d.Actions, 1),
	}
	switch d.Kind {
	case "conflict":
		return b.GetF("keymap.diagnostics.conflict", vars)
	case "invalid_key":
		return b.GetF("keymap.diagnostics.invalid_key", vars)
	case "unknown_action":
		return b.GetF("keymap.diagnostics.unknown_action", vars)
	case "normalized_duplicate":
		return b.GetF("keymap.diagnostics.normalized_duplicate", vars)
	default:
		return fmt.Sprintf("%s: %s", d.Kind, d.Key)
	}
}

// joinActions renders every action a diagnostic mentions.
func joinActions(as []keymap.Action) string {
	out := make([]string, len(as))
	for i, a := range as {
		out[i] = string(a)
	}
	return strings.Join(out, ", ")
}

// actionAt returns the i-th action or "" — a conflict names two, the other
// kinds name one, and reaching past the end must not panic on a diagnostic
// shape this function does not yet know about.
func actionAt(as []keymap.Action, i int) string {
	if i >= len(as) {
		return ""
	}
	return string(as[i])
}

// parseOnOff accepts the on/off vocabulary these toggles share. Anything else
// returns ok=false so the caller prints usage instead of guessing — a toggle
// that silently treats "maybe" as "off" is worse than one that refuses.
func parseOnOff(args []string) (bool, bool) {
	if len(args) == 0 {
		return false, false
	}
	switch strings.ToLower(strings.TrimSpace(args[0])) {
	case "on", "true", "yes", "1", "enable", "enabled":
		return true, true
	case "off", "false", "no", "0", "disable", "disabled":
		return false, true
	}
	return false, false
}
