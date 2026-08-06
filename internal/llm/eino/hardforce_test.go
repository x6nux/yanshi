package eino

import (
	"context"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// armedCooldownModel returns a CompactingModel that has REALLY compacted once,
// with a cooldown long enough that it is still active.
//
// Arming it by running a compaction rather than by assigning to the private
// fields is the point. The fields the test would write are the ones
// maybeCompact is supposed to write, so a maybeCompact that recorded nothing —
// or recorded the wrong number — would be invisible to any test that sets them
// by hand. It also means the cooldown under test is the production one.
func armedCooldownModel(t *testing.T, window int) (*CompactingModel, *recordingModel) {
	t.Helper()
	inner := &recordingModel{summary: "summarized", reply: "done", streamOK: true}
	cm := &CompactingModel{
		Inner:             inner,
		Threshold:         0.5,
		ContextWindow:     window,
		KeepRecent:        2,
		CooldownTokens:    100,
		CooldownDuration:  time.Hour,
		HardForceFraction: 0.95,
	}

	// ~228 tokens against a 0.5 threshold: over the gate, and with no prior
	// compaction there is no cooldown yet.
	first := []*schema.Message{
		bigMessage(30), bigMessage(30), bigMessage(30),
		bigMessage(30), bigMessage(30), bigMessage(30),
	}
	_, did := cm.maybeCompact(context.Background(), first)
	require.True(t, did, "the arming compaction did not happen")
	require.Equal(t, 1, inner.calls)

	// maybeCompact must have recorded the cooldown state itself.
	cm.cmMu.Lock()
	lastT, lastAt, flag := cm.lastCompactTokens, cm.lastCompactAt, cm.didCompact
	cm.cmMu.Unlock()
	require.True(t, flag, "maybeCompact did not record that a compaction happened")
	require.Positive(t, lastT,
		"maybeCompact recorded lastCompactTokens=0, so the token dimension of the "+
			"cooldown measures growth from nothing and never defers")
	require.False(t, lastAt.IsZero(), "maybeCompact did not stamp lastCompactAt")

	inner.calls = 0
	return cm, inner
}

// TestCooldownIsArmedByARealCompaction is the cooldown clause without the hand
// -written state.
//
// TestCompactingModel_CooldownDefersReCompact assigns cm.lastCompactAt after
// its first compaction, so "maybeCompact records the cooldown state correctly"
// — the step the whole mechanism rests on — was never asserted anywhere. This
// arms the cooldown by compacting and then checks it defers.
//
// ledger: F2/CCL1#1 同 turn 不重复压缩
func TestCooldownIsArmedByARealCompaction(t *testing.T) {
	cm, inner := armedCooldownModel(t, 400)

	// The same history again: growth is ~0, well under CooldownTokens, and the
	// hour-long duration has certainly not elapsed.
	same := []*schema.Message{
		bigMessage(30), bigMessage(30), bigMessage(30),
		bigMessage(30), bigMessage(30), bigMessage(30),
	}
	out, did := cm.maybeCompact(context.Background(), same)
	assert.False(t, did, "compacted again inside the cooldown it had just armed")
	assert.Zero(t, inner.calls, "a second summarize call was made during the cooldown")
	assert.Len(t, out, len(same), "a deferred compaction must return the history untouched")
}

// TestHardForceFiresInsideAnArmedCooldown is the clause the ledger recorded as
// covered and was not.
//
// TestCompactingModel_HardForceOverridesCooldown (since deleted) set
// CooldownTokens: 99999 to simulate a cooldown, but with no prior compaction
// inCooldown returns false immediately — "no prior compact → no cooldown" — so
// the cooldown was never armed and the ordinary threshold gate let the call
// through before hard-force was ever consulted. Measured: deleting the entire
// HardForceFraction branch from shouldCompact left internal/llm/eino,
// internal/bootstrap and internal/agent/orchestrator all green. Nothing in the
// repository held that branch down.
//
// Two conditions have to hold at once for the branch to be the reason anything
// happens: the cooldown must really be active, AND the ordinary threshold gate
// must not already be the thing letting it through. The second is why the
// negative twin below matters more than the positive case.
//
// ledger: F2/CCL1#2 逼近上限仍触发
func TestHardForceFiresInsideAnArmedCooldown(t *testing.T) {
	const window = 400

	t.Run("at the hard-force fraction it compacts anyway", func(t *testing.T) {
		cm, inner := armedCooldownModel(t, window)

		// 0.95 × 400 = 380. Four messages of ~108 tokens ≈ 432, past it.
		huge := []*schema.Message{
			bigMessage(100), bigMessage(100), bigMessage(100), bigMessage(100),
		}
		require.True(t, cm.inCooldown(4*108),
			"the cooldown is not active, so this would prove nothing about hard-force")

		_, did := cm.maybeCompact(context.Background(), huge)
		assert.True(t, did,
			"the history reached the hard-force fraction and compaction was still deferred; "+
				"the next inner model call goes over the context window")
		assert.Equal(t, 1, inner.calls)
	})

	t.Run("below the fraction the cooldown still wins", func(t *testing.T) {
		cm, inner := armedCooldownModel(t, window)

		// Between the threshold (0.5×400 = 200) and hard-force (380). Without
		// this twin, a hard-force branch that fired unconditionally — or one
		// deleted so the cooldown never applied — would pass the case above.
		mid := []*schema.Message{
			bigMessage(60), bigMessage(60), bigMessage(60), bigMessage(60),
		} // ≈ 4 × 68 = 272
		tokens := 4 * 68
		require.Less(t, tokens, int(0.95*float64(window)),
			"the fixture is already at hard-force, so this is not the negative case")
		require.GreaterOrEqual(t, tokens, int(0.5*float64(window)),
			"the fixture is under the ordinary threshold, so the cooldown is not what defers it")

		_, did := cm.maybeCompact(context.Background(), mid)
		assert.False(t, did,
			"compacted below the hard-force fraction while in cooldown: hard-force is firing "+
				"for everything, which makes the cooldown a no-op")
		assert.Zero(t, inner.calls)
	})
}

// TestKeepRecentBridgeHalvesTheMessageCount is the keepRecent clause as a
// behavioural claim.
//
// KeepRecent counts MESSAGES on CompactingModel and PAIRS in
// ctxcompact.PlanOpts, and maybeCompact divides by two to bridge them. The
// original acceptance asked for that to be "documented clearly", which no test
// can carry — a doc comment is not an assertion. The acceptance now asks for
// the two semantics to agree, which is checkable: a drift in either direction
// changes how much tail survives a compaction, silently and in production only.
//
// ledger: F2/CCL1#3 keepRecent 两处语义一致（消息数 ↔ 对数）
func TestKeepRecentBridgeHalvesTheMessageCount(t *testing.T) {
	for _, keepRecent := range []int{2, 4, 6, 7} {
		inner := &recordingModel{summary: "s", reply: "done", streamOK: true}
		cm := &CompactingModel{
			Inner: inner, Threshold: 0.5, ContextWindow: 400,
			KeepRecent: keepRecent,
		}
		got := cm.planKeepRecent()
		assert.Equalf(t, keepRecent/2, got,
			"CompactingModel.KeepRecent=%d (messages) bridges to PlanOpts.KeepRecent=%d "+
				"(pairs); the two units disagree, so a compaction keeps a different amount "+
				"of tail than the configuration says", keepRecent, got)
	}
}
