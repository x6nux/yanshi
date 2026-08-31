// internal/cli/tui/onboarding.go
//
// W-E-16: first-run wizard. Two steps:
//
//	step 0 — welcome: "enter to configure, esc/s to skip"
//	step 1 — permission-mode pick (reuses the same option list as /mode)
//
// The wizard is armed in newModelWithPrefs only when no cascade layer has
// recorded OnboardingDone. BOTH finishing and skipping write the tombstone,
// which is the entire "skip and never ask again" contract.

package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/x6nux/yanshi/internal/guard"
)

// onboardingModes returns the permission-mode rows for the wizard's pick
// step, marked with the current effective mode. Deliberately the same list
// guard.Modes() serves /mode with, so the wizard cannot advertise a mode the
// product cannot set.
func (m model) onboardingModes() []pickerItem {
	cur := m.permMode
	if cur == "" {
		cur = guard.ModeDefault
	}
	items := make([]pickerItem, 0, len(guard.Modes()))
	for _, mode := range guard.Modes() {
		items = append(items, pickerItem{
			name:    string(mode),
			current: mode == cur,
		})
	}
	return items
}

// onboardingCursor is the selection index for the wizard's step-1 picker.
// Stored on the state struct so the model value-copy semantics carry it.
func (o *onboardingState) cursor(items []pickerItem, key tea.KeyMsg) int {
	n := len(items)
	if n == 0 {
		return 0
	}
	switch key.Type {
	case tea.KeyDown:
		return (o.cursorIdx + 1) % n
	case tea.KeyUp:
		return (o.cursorIdx - 1 + n) % n
	}
	if key.Type == tea.KeyRunes && len(key.Runes) == 1 {
		switch string(key.Runes) {
		case "j":
			return (o.cursorIdx + 1) % n
		case "k":
			return (o.cursorIdx - 1 + n) % n
		}
	}
	return o.cursorIdx
}

// onboardingKey handles a keypress while the wizard is showing. It returns
// the updated model and consumes the key unconditionally (true at the call
// site).
func (m model) onboardingKey(msg tea.KeyMsg) (model, tea.Cmd) {
	if m.onboarding == nil {
		return m, nil
	}
	switch m.onboarding.step {
	case 0:
		switch msg.Type {
		case tea.KeyEnter:
			// Continue into the permission-mode step.
			m.onboarding.step = 1
			m.onboarding.cursorIdx = 0
			m.reflow()
			return m, nil
		case tea.KeyEscape:
			return m.onboardingFinish(false)
		case tea.KeyRunes:
			if len(msg.Runes) == 1 && (msg.Runes[0] == 's' || msg.Runes[0] == 'S') {
				return m.onboardingFinish(false)
			}
		}
		// Any other key at step 0: consumed, no state change.
		return m, nil
	case 1:
		items := m.onboardingModes()
		switch msg.Type {
		case tea.KeyEscape:
			// Esc at the pick step also counts as skip: backing out of the
			// wizard without choosing must not leave the tombstone unset, or
			// the wizard would reappear on every startup and become the very
			// "nag" the skip feature exists to prevent.
			return m.onboardingFinish(false)
		case tea.KeyUp, tea.KeyDown:
			m.onboarding.cursorIdx = m.onboarding.cursor(items, msg)
			m.reflow()
			return m, nil
		case tea.KeyRunes:
			if len(msg.Runes) == 1 {
				switch string(msg.Runes) {
				case "j", "k":
					m.onboarding.cursorIdx = m.onboarding.cursor(items, msg)
					m.reflow()
					return m, nil
				}
			}
			return m, nil
		case tea.KeyEnter:
			if len(items) == 0 {
				return m.onboardingFinish(false)
			}
			sel := items[m.onboarding.cursorIdx]
			pm, ok := guard.NormalizeMode(sel.name)
			if !ok {
				// Should be unreachable (the list came from guard.Modes),
				// but fail toward skip rather than panic.
				return m.onboardingFinish(false)
			}
			mm, cmd := m.applyOnboardingMode(pm)
			return mm, cmd
		}
		return m, nil
	}
	return m, nil
}

// applyOnboardingMode sets the chosen permission mode (through the same
// sendMode path /mode uses) and writes the tombstone. sendMode returns a nil
// Cmd (it sends the frame synchronously and reflows), so nothing is batched
// here; the assignment keeps that explicit rather than silently dropping a
// future non-nil Cmd.
func (m model) applyOnboardingMode(pm guard.PermissionMode) (model, tea.Cmd) {
	m.permMode = pm
	mm, cmd := m.sendMode()
	finished, fcmd := mm.(model).onboardingFinish(true)
	if cmd != nil {
		return finished, tea.Batch(cmd, fcmd)
	}
	return finished, fcmd
}

// onboardingFinish closes the wizard and persists the OnboardingDone
// tombstone. applied distinguishes "finished" from "skipped" only for the
// transcript message; both write the tombstone, which is the contract.
func (m model) onboardingFinish(applied bool) (model, tea.Cmd) {
	m.onboarding = nil
	yes := true
	m.prefs.OnboardingDone = &yes
	m = m.savePrefs().remerge()
	if applied {
		m.entries = append(m.entries, ackEntry{text: okStyle.Render("✓ onboarding complete")})
	} else {
		m.entries = append(m.entries, ackEntry{
			text: "onboarding skipped — run /mode and /model any time; this wizard will not appear again",
		})
	}
	m.reflow()
	return m, nil
}

// onboardingPopup renders the wizard. Returns "" when the wizard is closed.
func (m model) onboardingPopup() string {
	if m.onboarding == nil {
		return ""
	}
	var b strings.Builder
	if m.onboarding.step == 0 {
		b.WriteString("  " + toolName.Render("welcome to yanshi") + "\n")
		b.WriteString("  " + toolMeta.Render("configure your permission mode (you can change it any time with /mode)") + "\n")
		b.WriteString("  " + toolMeta.Render("enter: configure now · esc / s: skip (never ask again)") + "\n")
		return b.String()
	}
	b.WriteString("  " + toolName.Render("choose a permission mode") + "\n")
	items := m.onboardingModes()
	for i, item := range items {
		line := "  " + item.name
		if item.current {
			line = "  " + okStyle.Render("●") + line[2:]
		}
		if i == m.onboarding.cursorIdx {
			b.WriteString(selPaletteStyle.Render("▶ " + strings.TrimSpace(line)))
		} else {
			b.WriteString(paletteStyle.Render("  " + strings.TrimSpace(line[2:])))
		}
		b.WriteString("\n")
	}
	b.WriteString("  " + toolMeta.Render("↑↓ jk navigate · enter select · esc skip") + "\n")
	return b.String()
}
