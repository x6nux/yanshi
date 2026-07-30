package tui

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// debounceMsg is fired ~16ms after the first schedule() call so the deferred
// reflow runs once and observes every keystroke that accumulated during that
// window. The tick is armed at most once at a time — further schedule() calls
// made while it is in flight are coalesced into the same pending tick.
type debounceMsg struct{}

// debouncer coalesces high-frequency reflow requests (one per KeyMsg at the
// bottom of Update) into a single ~16ms-deferred reflow.
//
// Why this exists: the bottom-of-Update path runs on every input mutation
// (typing, pasting, deleting). Without coalescing, each rune of a 1000-rune
// paste drives a full m.reflow() — three blockHeight measurements over the
// footer / status line / input block plus a viewport.SetContent over the whole
// rendered transcript — which is the dominant CPU cost during input and the
// root cause of paste/delete jitter (T9/T12). Deferring the reflow by ~16ms
// (one frame at 60Hz) folds an entire burst of keystrokes into a single
// measurement pass, so pasting 1000 runes reflows only ~1 time per frame
// instead of 1000 times.
//
// pending == true means a tick is already in flight and further schedule()
// calls are no-ops until it lands and consume() is called.
type debouncer struct{ pending bool }

// schedule arms a ~16ms tick if (and only if) none is already in flight, then
// returns the tick Cmd for the caller to batch into its Update return. A nil
// return means a tick is already pending: the caller has nothing to do — the
// in-flight tick will fire and reflow will pick up the latest input state.
//
// The 16ms window is chosen to be one frame at 60Hz: short enough that the
// user perceives typing as instantaneous, long enough to coalesce a full
// keystroke burst (terminals deliver paste runes in a handful of event-loop
// ticks, all within one frame).
func (d *debouncer) schedule() tea.Cmd {
	if d.pending {
		return nil
	}
	d.pending = true
	return tea.Tick(16*time.Millisecond, func(time.Time) tea.Msg { return debounceMsg{} })
}

// consume clears the in-flight flag so the next schedule() arms a fresh tick.
// Called from Update's debounceMsg handler before reflowing.
func (d *debouncer) consume() { d.pending = false }
