package tui

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestPermissionDeadlineAndSecondsLeft covers the countdown arithmetic the
// popup renders.
//
// The "no deadline" case is the load-bearing one: an older backend (or a
// policy with no expiry) sends no countdown, and the popup must then show none
// rather than a fabricated "0s left" that tells the user their prompt is
// already dead. That is why secondsLeft returns -1 instead of 0 for "absent" —
// zero is a real, distinct state meaning "expiring now".
func TestPermissionDeadlineAndSecondsLeft(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	cases := []struct {
		name        string
		timeoutSecs int
		at          time.Time
		want        int
	}{
		{name: "no countdown advertised", timeoutSecs: 0, at: now, want: -1},
		{name: "negative is treated as absent", timeoutSecs: -5, at: now, want: -1},
		{name: "full budget at receipt", timeoutSecs: 60, at: now, want: 60},
		{name: "counts down", timeoutSecs: 60, at: now.Add(45 * time.Second), want: 15},
		{name: "clamps at zero on expiry", timeoutSecs: 60, at: now.Add(60 * time.Second), want: 0},
		{name: "never goes negative", timeoutSecs: 60, at: now.Add(5 * time.Minute), want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pe := &permissionEntry{expiresAt: permissionDeadline(now, tc.timeoutSecs)}
			assert.Equal(t, tc.want, pe.secondsLeft(tc.at))
		})
	}

	// A nil entry is the "no pending prompt" case and must not panic.
	var none *permissionEntry
	assert.Equal(t, -1, none.secondsLeft(now))
}

// TestPermissionPopupShowsTheCountdownOnlyWhenTheServerSentOne is the consumer
// check: the wire field reaching the model is worthless if nothing renders it,
// and this repo's dominant defect class is exactly that — a value carried all
// the way to a struct field with no reader.
func TestPermissionPopupShowsTheCountdownOnlyWhenTheServerSentOne(t *testing.T) {
	withDeadline := newTestModel(t)
	withDeadline.pendingPermissions = []*permissionEntry{{
		id: "1", tool: "fs_write", args: "{}", reason: "not allowed",
		expiresAt: time.Now().Add(42 * time.Second),
	}}
	assert.Contains(t, withDeadline.permissionPopup(), "s left",
		"an advertised deadline must be visible, or the prompt appears to die for no reason")

	without := newTestModel(t)
	without.pendingPermissions = []*permissionEntry{{
		id: "1", tool: "fs_write", args: "{}", reason: "not allowed",
	}}
	assert.NotContains(t, without.permissionPopup(), "s left",
		"a backend that sent no countdown must not produce an invented one")
}
