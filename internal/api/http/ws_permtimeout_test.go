package http

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/tools"
)

// TestPermissionTimeoutPolicyResolve pins the defaulting rule: the zero value
// is a usable policy, and only unset (non-positive) fields are defaulted.
//
// It matters because the policy is threaded from Config through New into every
// connection. A resolve that dropped an explicitly configured value would make
// the countdown on the wire disagree with the wait that actually runs, and the
// user would watch a timer that means nothing.
func TestPermissionTimeoutPolicyResolve(t *testing.T) {
	cases := []struct {
		name           string
		in             PermissionTimeoutPolicy
		wantTimeout    time.Duration
		wantUnattended int
	}{
		{
			name:           "zero value takes both defaults",
			in:             PermissionTimeoutPolicy{},
			wantTimeout:    DefaultPermissionTimeout,
			wantUnattended: DefaultPermissionUnattendedAfter,
		},
		{
			name:           "explicit values survive",
			in:             PermissionTimeoutPolicy{Timeout: 5 * time.Second, UnattendedAfter: 2},
			wantTimeout:    5 * time.Second,
			wantUnattended: 2,
		},
		{
			name:           "negative timeout falls back, count kept",
			in:             PermissionTimeoutPolicy{Timeout: -1, UnattendedAfter: 7},
			wantTimeout:    DefaultPermissionTimeout,
			wantUnattended: 7,
		},
		{
			name:           "zero count falls back, timeout kept",
			in:             PermissionTimeoutPolicy{Timeout: 250 * time.Millisecond},
			wantTimeout:    250 * time.Millisecond,
			wantUnattended: DefaultPermissionUnattendedAfter,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.in.resolve()
			assert.Equal(t, tc.wantTimeout, got.Timeout)
			assert.Equal(t, tc.wantUnattended, got.UnattendedAfter)
		})
	}
}

// TestAwaitDecisionTimeoutIsAlwaysDeny is the non-negotiable half of S5: a
// prompt nobody answers must resolve to DENY.
//
// Stated as a table over every non-answer shape (expiry, cancelled turn,
// latched connection) because the failure being guarded is not "the timeout
// value is wrong" but "one of these branches returns Allow". A single-case test
// would leave the other two branches free to drift, and the drift would be an
// authorization bypass that no other test in this package observes: the guard
// layer takes whatever this callback returns.
func TestAwaitDecisionTimeoutIsAlwaysDeny(t *testing.T) {
	cases := []struct {
		name        string
		latch       bool
		cancelCtx   bool
		deliver     *tools.PermissionDecision
		wantOutcome permOutcome
	}{
		{name: "expiry denies", wantOutcome: permExpired},
		{name: "turn cancel denies", cancelCtx: true, wantOutcome: permAborted},
		{name: "latched connection denies without waiting", latch: true, wantOutcome: permRefusedUnattended},
	}
	allow := tools.PermissionAllow
	cases = append(cases, struct {
		name        string
		latch       bool
		cancelCtx   bool
		deliver     *tools.PermissionDecision
		wantOutcome permOutcome
	}{name: "an actual answer is honoured", deliver: &allow, wantOutcome: permAnswered})

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := newUnattendedState(PermissionTimeoutPolicy{
				Timeout: 30 * time.Millisecond, UnattendedAfter: 1,
			})
			if tc.latch {
				require.True(t, u.noteExpiry(), "threshold 1 must latch on the first expiry")
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancelCtx {
				cancel()
			}
			ch := make(chan tools.PermissionDecision, 1)
			if tc.deliver != nil {
				ch <- *tc.deliver
			}

			decision, outcome := u.awaitDecision(ctx, ch, time.Now().Add(30*time.Millisecond))

			assert.Equal(t, tc.wantOutcome, outcome)
			if tc.wantOutcome == permAnswered {
				assert.Equal(t, tools.PermissionAllow, decision)
				return
			}
			assert.Equal(t, tools.PermissionDeny, decision,
				"a prompt that was not answered must never resolve to allow")
		})
	}
}

// TestUnattendedLatchesAfterConsecutiveExpiriesAndResetsOnInteraction pins the
// degradation state machine.
//
// The two halves are inseparable and that is the point of testing them in one
// sequence: latching without a reset would make a single distracted minute
// permanently auto-deny the rest of the session, and resetting without latching
// leaves the unattended run paying a full timeout per prompt forever. Either
// half alone passes its own test.
func TestUnattendedLatchesAfterConsecutiveExpiriesAndResetsOnInteraction(t *testing.T) {
	u := newUnattendedState(PermissionTimeoutPolicy{Timeout: time.Second, UnattendedAfter: 3})

	assert.False(t, u.noteExpiry(), "first expiry must not latch")
	assert.False(t, u.isLatched())
	assert.False(t, u.noteExpiry(), "second expiry must not latch")
	assert.False(t, u.isLatched())
	assert.True(t, u.noteExpiry(), "third consecutive expiry latches")
	assert.True(t, u.isLatched())

	// A latch already set does not re-announce.
	assert.False(t, u.noteExpiry(), "an already-latched state must not report a new transition")
	assert.True(t, u.isLatched())

	// Any interaction clears it, AND clears the running count — the next latch
	// must require a fresh full run of expiries, not one more.
	u.noteInteraction()
	assert.False(t, u.isLatched())
	assert.False(t, u.noteExpiry(), "the counter must restart from zero after an interaction")
	assert.False(t, u.isLatched())
}

// TestNonConsecutiveExpiriesNeverLatch is the property the word "consecutive"
// is carrying. A user who answers every other prompt is present, however slow;
// counting their expiries cumulatively would eventually latch on them and
// start auto-denying a session someone is actively watching.
func TestNonConsecutiveExpiriesNeverLatch(t *testing.T) {
	u := newUnattendedState(PermissionTimeoutPolicy{Timeout: time.Second, UnattendedAfter: 2})
	for i := 0; i < 10; i++ {
		assert.False(t, u.noteExpiry(), "expiry %d", i)
		u.noteInteraction()
	}
	assert.False(t, u.isLatched(), "alternating expiry/answer must never latch")
}

// TestAwaitDecisionAnswerUnlatches proves the answer path resets the latch
// itself, rather than relying on the reader goroutine's per-frame reset.
//
// The two resets exist for different traffic and neither subsumes the other:
// the reader sees every frame but runs in another goroutine, and a
// connection whose only traffic is permission answers must still unlatch. If
// only the reader reset existed, this test would fail — which is the point.
func TestAwaitDecisionAnswerUnlatches(t *testing.T) {
	u := newUnattendedState(PermissionTimeoutPolicy{Timeout: time.Second, UnattendedAfter: 2})
	require.False(t, u.noteExpiry())

	ch := make(chan tools.PermissionDecision, 1)
	ch <- tools.PermissionDeny
	_, outcome := u.awaitDecision(context.Background(), ch, time.Now().Add(time.Second))
	require.Equal(t, permAnswered, outcome)

	// The pending expiry count must be gone: one more expiry would otherwise
	// hit the threshold of 2 and latch a session that just answered.
	assert.False(t, u.noteExpiry(), "an answer must clear the consecutive-expiry count")
	assert.False(t, u.isLatched())
}

// TestPermDenyNoticeExplainsTheDenialAndUsesTheLivePolicy checks the two things
// the notice must do: distinguish the two auto-deny shapes, and quote the
// numbers this server actually runs on.
//
// The second is not cosmetic. A notice hardcoded to the package defaults on a
// server configured with a 10s timeout tells the operator to look for a 60s
// pause that never happened, which is a worse diagnostic than silence.
func TestPermDenyNoticeExplainsTheDenialAndUsesTheLivePolicy(t *testing.T) {
	policy := PermissionTimeoutPolicy{Timeout: 12 * time.Second, UnattendedAfter: 4}

	expired := permDenyNotice(permExpired, "shell_run", policy)
	assert.Contains(t, expired, "shell_run")
	assert.Contains(t, expired, "12s", "the notice must quote the configured timeout")
	assert.Contains(t, strings.ToLower(expired), "denied")

	unattendedNotice := permDenyNotice(permRefusedUnattended, "fs_write", policy)
	assert.Contains(t, unattendedNotice, "fs_write")
	assert.Contains(t, unattendedNotice, "4", "the notice must quote the configured threshold")
	assert.Contains(t, strings.ToLower(unattendedNotice), "unattended")

	assert.NotEqual(t, expired, unattendedNotice,
		"the two auto-deny shapes must be distinguishable in the transcript")

	// Silent paths: an answered prompt needs no explanation, and an aborted
	// turn is already reporting its own cancel.
	assert.Empty(t, permDenyNotice(permAnswered, "fs_write", policy))
	assert.Empty(t, permDenyNotice(permAborted, "fs_write", policy))
}

// TestPermDenyNoticeUsesDefaultsForAZeroPolicy guards the seam between the
// notice and the Config default: a Server built with no explicit policy stores
// the resolved one, but a caller that passes the zero value here must still
// read real numbers rather than an empty budget and a zero threshold.
func TestPermDenyNoticeUsesDefaultsForAZeroPolicy(t *testing.T) {
	zero := PermissionTimeoutPolicy{}
	assert.Contains(t, permDenyNotice(permExpired, "fs_write", zero),
		DefaultPermissionTimeout.String())
	assert.Equal(t,
		permDenyNotice(permRefusedUnattended, "fs_write", PermissionTimeoutPolicy{}.resolve()),
		permDenyNotice(permRefusedUnattended, "fs_write", zero),
		"an unresolved policy must render exactly as the resolved defaults do")
}
