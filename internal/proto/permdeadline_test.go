package proto

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWithPermDeadlineRoundsUpSoASubSecondBudgetIsNeverInvisible pins the one
// non-obvious rule in the builder.
//
// PermTimeoutSecs is `omitempty`, so a value of 0 is DROPPED from the JSON —
// meaning a frame from a server with a 300ms budget would be byte-identical to
// one from a server with no expiry policy at all, while a clock was in fact
// running against the user. Truncation produced exactly that, and it is a lie
// about whether the prompt will die rather than a rounding imprecision.
//
// Rounding up over-states a sub-second budget as 1s. That is the correct trade:
// the absolute deadline is carried unrounded alongside it for any caller that
// needs precision, and a client that shows "1s" on a 300ms prompt is merely
// imprecise, whereas one that shows nothing is wrong.
func TestWithPermDeadlineRoundsUpSoASubSecondBudgetIsNeverInvisible(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	cases := []struct {
		name     string
		timeout  time.Duration
		wantSecs int
	}{
		{name: "sub-second budgets round up to 1", timeout: 300 * time.Millisecond, wantSecs: 1},
		{name: "one nanosecond still rounds up", timeout: time.Nanosecond, wantSecs: 1},
		{name: "whole seconds are exact", timeout: 60 * time.Second, wantSecs: 60},
		{name: "partial seconds round up", timeout: 1500 * time.Millisecond, wantSecs: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := NewPermissionRequest("id", "fs_write", "{}", "why", false, false).
				WithPermDeadline(tc.timeout, now.Add(tc.timeout))
			assert.Equal(t, tc.wantSecs, f.PermTimeoutSecs)

			// The whole point is that it survives omitempty.
			data, err := json.Marshal(f)
			require.NoError(t, err)
			var got ServerFrame
			require.NoError(t, json.Unmarshal(data, &got))
			assert.Equal(t, tc.wantSecs, got.PermTimeoutSecs,
				"the countdown must survive the wire, or the client cannot render it")
			assert.Equal(t, now.Add(tc.timeout).Unix(), got.PermDeadlineUnix)
		})
	}
}

// TestWithPermDeadlineIsANoOpWithoutAPolicy: a caller with no expiry must not
// have to branch, and the frame it produces must stay byte-identical to the
// pre-S5 wire form so an older client sees exactly what it saw before.
func TestWithPermDeadlineIsANoOpWithoutAPolicy(t *testing.T) {
	base := NewPermissionRequest("id", "fs_write", "{}", "why", false, false)
	for _, tc := range []struct {
		name     string
		timeout  time.Duration
		deadline time.Time
	}{
		{name: "zero timeout", timeout: 0, deadline: time.Unix(1, 0)},
		{name: "negative timeout", timeout: -time.Second, deadline: time.Unix(1, 0)},
		{name: "zero deadline", timeout: time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := base.WithPermDeadline(tc.timeout, tc.deadline)
			assert.Zero(t, got.PermTimeoutSecs)
			assert.Zero(t, got.PermDeadlineUnix)
			assert.Equal(t, base, got, "no policy must leave the frame untouched")
		})
	}
}

// TestWithPermDeadlineDoesNotMutateTheReceiver: the builder returns a copy, so
// a caller that stamps a deadline onto a shared template frame cannot leak the
// deadline into every other request built from it.
func TestWithPermDeadlineDoesNotMutateTheReceiver(t *testing.T) {
	base := NewPermissionRequest("id", "fs_write", "{}", "why", false, false)
	_ = base.WithPermDeadline(time.Minute, time.Unix(1_700_000_000, 0))
	assert.Zero(t, base.PermTimeoutSecs)
	assert.Zero(t, base.PermDeadlineUnix)
}
