package tui

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// notifyLongTaskThreshold is how long a turn must run before its completion
// is worth a desktop notification. A quick round-trip finishing while the
// user is still looking at the terminal doesn't need one; a multi-minute
// tool-heavy turn the user has tabbed away from does. Not user-configurable
// (unlike the on/off switch itself) — this is a UX judgment call, not a
// policy decision, and one knob per feature is the point where "avoid an
// obnoxious default" stops needing more surface area.
const notifyLongTaskThreshold = 10 * time.Second

// notifyCmd returns the tea.Cmd that fires W-E-04's "turn finished" desktop
// notification, or nil when any of three gates fails:
//
//  1. notifyEnabled is false (config tui.notify, default OFF — see
//     notifyEnabled's doc comment on the model struct).
//  2. The turn ran shorter than notifyLongTaskThreshold — see that
//     constant's doc comment.
//
// Past those two gates, titleEnabled (== cap.AltScreen, reused rather than
// duplicated — see notifyEnabled's doc comment) picks the escape tier: a
// capable terminal gets the hand-rolled OSC 9 notification (tea.Notify);
// TERM=dumb gets a plain BEL (tea.Bell) instead of the OSC 9 bytes, which on
// a terminal E1's capability gate does not trust could print as garbage
// rather than degrade silently the way CSI ...t sequences do (see
// standardRenderer.bell's doc comment). This mirrors windowTitleCmd's
// gate-at-the-TUI-layer shape rather than pushing the decision into the fork.
func (m model) notifyCmd() tea.Cmd {
	if !m.notifyEnabled {
		return nil
	}
	if time.Since(m.turnStart) < notifyLongTaskThreshold {
		return nil
	}
	if !m.titleEnabled {
		return tea.Bell()
	}
	vars := map[string]string{"project": m.workDir}
	return tea.Notify(m.bundle.GetF("tui.notify.done", vars))
}
