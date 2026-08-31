package store

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestForkSession_AllMessages(t *testing.T) {
	st := openTestStore(t)
	src, err := st.CreateSession("orig")
	require.NoError(t, err)
	require.NoError(t, st.AppendMessage(src, 0, "user", "hi"))
	require.NoError(t, st.AppendMessage(src, 1, "assistant", "hello"))
	require.NoError(t, st.AppendMessage(src, 2, "user", "how are you"))

	forkID, err := st.ForkSession(src, -1) // -1 = all
	require.NoError(t, err)
	require.NotEqual(t, src, forkID)
	require.NotEmpty(t, forkID)

	msgs, err := st.Messages(forkID)
	require.NoError(t, err)
	require.Len(t, msgs, 3)
	assert.Equal(t, "hi", msgs[0].Content)
	assert.Equal(t, "how are you", msgs[2].Content)

	// Source session is untouched.
	origMsgs, _ := st.Messages(src)
	assert.Len(t, origMsgs, 3, "source session row count must not change")
}

func TestForkSession_PartialBySeq(t *testing.T) {
	st := openTestStore(t)
	src, _ := st.CreateSession("orig")
	st.AppendMessage(src, 0, "user", "m0")
	st.AppendMessage(src, 1, "assistant", "m1")
	st.AppendMessage(src, 2, "user", "m2")
	st.AppendMessage(src, 3, "assistant", "m3")

	// fromSeq=2 → copy messages[0..2] inclusive.
	forkID, err := st.ForkSession(src, 2)
	require.NoError(t, err)
	msgs, _ := st.Messages(forkID)
	require.Len(t, msgs, 3)
	assert.Equal(t, "m0", msgs[0].Content)
	assert.Equal(t, "m2", msgs[2].Content)
}

func TestForkSession_SeqOutOfBoundsRejected(t *testing.T) {
	st := openTestStore(t)
	src, _ := st.CreateSession("orig")
	st.AppendMessage(src, 0, "user", "only")

	_, err := st.ForkSession(src, 5) // out of bounds
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "out of range") || strings.Contains(err.Error(), "bounds"),
		"err should mention out-of-range, got: %v", err)
}

// GB5: only -1 ("all") and -2 ("empty", W-E-11) carry meaning; anything below
// -2 is an illegal input and must not be silently treated as either.
func TestForkSession_NegativeBelowMinusTwoRejected(t *testing.T) {
	st := openTestStore(t)
	src, _ := st.CreateSession("orig")
	require.NoError(t, st.AppendMessage(src, 0, "user", "only"))

	before, err := st.ListSessions(0)
	require.NoError(t, err)
	_, err = st.ForkSession(src, -3)
	require.Error(t, err)
	after, err := st.ListSessions(0)
	require.NoError(t, err)
	assert.Len(t, after, len(before), "illegal negative must not create a fork row")
}

// TestForkSession_MinusTwoForksEmptySession proves the W-E-11 sentinel: -2
// creates a new session with zero messages (not "all", -1's meaning), while
// still inheriting title/model/thinking and resetting usage per GB6 — the
// same session-row semantics as every other fork, just with an empty
// transcript. This is the case "roll back to before my very first message"
// needs and that -1 cannot express (see ForkSession's doc comment).
func TestForkSession_MinusTwoForksEmptySession(t *testing.T) {
	st := openTestStore(t)
	src, _ := st.CreateSession("orig")
	require.NoError(t, st.AppendMessage(src, 0, "user", "only"))
	require.NoError(t, st.UpdateSessionMeta(src, "model-x", "high", 101, 202, 3, 44, 55, BillingMeta{}))

	forkID, err := st.ForkSession(src, -2)
	require.NoError(t, err)
	require.NotEqual(t, src, forkID)

	msgs, err := st.Messages(forkID)
	require.NoError(t, err)
	assert.Empty(t, msgs, "-2 must copy zero messages")

	fork, err := st.GetSession(forkID)
	require.NoError(t, err)
	require.NotNil(t, fork)
	assert.Equal(t, "model-x", fork.Model, "model must still be inherited on an empty fork")
	assert.Equal(t, "high", fork.Thinking)
	assert.Zero(t, fork.TokensIn)

	// Source session is untouched.
	origMsgs, _ := st.Messages(src)
	assert.Len(t, origMsgs, 1, "source session row count must not change")
}

// GB6: the message prefix is copied, but cumulative usage/turns cannot be carried
// over. The schema has no per-message usage, so all forks reset from zero;
// model/thinking are still inherited.
func TestForkSession_ResetsUsageMetadata(t *testing.T) {
	st := openTestStore(t)
	src, _ := st.CreateSession("orig")
	require.NoError(t, st.AppendMessage(src, 0, "user", "m0"))
	require.NoError(t, st.AppendMessage(src, 1, "assistant", "m1"))
	require.NoError(t, st.UpdateSessionMeta(src, "model-x", "high", 101, 202, 3, 44, 55, BillingMeta{}))

	forkID, err := st.ForkSession(src, 0) // partial fork
	require.NoError(t, err)
	fork, err := st.GetSession(forkID)
	require.NoError(t, err)
	require.NotNil(t, fork)
	assert.Equal(t, "model-x", fork.Model)
	assert.Equal(t, "high", fork.Thinking)
	assert.Zero(t, fork.TokensIn)
	assert.Zero(t, fork.TokensOut)
	assert.Zero(t, fork.Turns)
	assert.Zero(t, fork.CachedTokens)
	assert.Zero(t, fork.ReasoningTokens)

	// Source metadata remains untouched.
	orig, err := st.GetSession(src)
	require.NoError(t, err)
	assert.Equal(t, 101, orig.TokensIn)
	assert.Equal(t, 202, orig.TokensOut)
	assert.Equal(t, 3, orig.Turns)
	assert.Equal(t, 44, orig.CachedTokens)
	assert.Equal(t, 55, orig.ReasoningTokens)
}

func TestForkSession_SourceMissing(t *testing.T) {
	st := openTestStore(t)
	_, err := st.ForkSession("nonexistent", -1)
	require.Error(t, err)
}

// openTestStore returns a fresh in-memory store for this test file.
func openTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })
	return st
}
