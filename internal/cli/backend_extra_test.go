package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/proto"
)

// TestFakeBackend_CancelCloseMode covers the trivial ChatBackend methods on
// fakeBackend that were previously never called (0% coverage): Cancel, Close,
// and Mode all return their fixed values.
func TestFakeBackend_CancelCloseMode(t *testing.T) {
	b := newFakeBackend(nil)
	require.NoError(t, b.Cancel())
	require.NoError(t, b.Close())
	assert.Equal(t, "fake", b.Mode())
	assert.Empty(t, b.Frames(), "no frames recorded without a SendFrame call")
}

// TestFakeBackend_SendFrameRecordsAndDrains proves SendFrame records the frame
// (test-inspectable via Frames) and returns a nil channel (no synthetic reply).
func TestFakeBackend_SendFrameRecordsAndDrains(t *testing.T) {
	b := newFakeBackend(nil)
	ch, err := b.SendFrame(context.Background(), proto.NewGetStatus())
	require.NoError(t, err)
	assert.Nil(t, ch, "fake backend cannot synthesize a real reply")
	require.Len(t, b.Frames(), 1)
	assert.Equal(t, "get_status", b.Frames()[0].Type)
}

// TestFakeBackend_SendStreamsChunksDone proves Send streams the scripted chunks
// followed by done.
func TestFakeBackend_SendStreamsChunksDone(t *testing.T) {
	b := newFakeBackend([]string{"a", "b"})
	ch, err := b.Send(context.Background(), "hi")
	require.NoError(t, err)
	var text string
	var sawDone bool
	for ev := range ch {
		if ev.Kind == "agent_chunk" {
			text += ev.Text
		}
		if ev.Kind == "done" {
			sawDone = true
		}
	}
	assert.Equal(t, "ab", text)
	assert.True(t, sawDone)
}

// TestSSEBackend_SendFrame_FeaturesDistinctError proves the features_list /
// features_set frames get the distinct "/features requires a WebSocket backend"
// error (constraint 7 in the C4 plan), not the generic control-unsupported text.
func TestSSEBackend_SendFrame_FeaturesDistinctError(t *testing.T) {
	b := newSSEBackend("http://unused")
	defer b.Close()
	for _, fr := range []proto.ClientFrame{
		proto.NewFeaturesList(), proto.NewFeaturesSet("flag", true),
	} {
		_, err := b.SendFrame(context.Background(), fr)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "/features requires a WebSocket backend")
	}
	// The generic default case keeps the standard message.
	_, err := b.SendFrame(context.Background(), proto.NewClear())
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrSSEControlUnsupported)
}

// TestSSEBackend_CancelNoopBeforeSend proves Cancel before any Send (no active
// cancelCurrent) is a harmless no-op.
func TestSSEBackend_CancelNoopBeforeSend(t *testing.T) {
	b := newSSEBackend("http://unused")
	defer b.Close()
	require.NoError(t, b.Cancel(), "Cancel with no in-flight turn is a no-op")
}

// TestNewClientThreadID_HasPrefix proves the happy path returns an "sse-" prefixed
// hex id (the crypto/rand fallback path is not reachable in normal operation).
func TestNewClientThreadID_HasPrefix(t *testing.T) {
	id := newClientThreadID()
	assert.True(t, strings.HasPrefix(id, "sse-"), "id = %q", id)
	// 6 bytes hex = 12 chars + "sse-" prefix = 16 total.
	assert.Len(t, id, 16)
	// Uniqueness across calls.
	assert.NotEqual(t, id, newClientThreadID())
}

// TestWSBackend_SendFrame_SetModeReturnsNil proves set_mode is a mid-turn-safe
// frame: it is written without replacing the active turn channel and returns nil
// (no reply channel), mirroring permission_response.
func TestWSBackend_SendFrame_SetModeReturnsNil(t *testing.T) {
	b, err := newWSBackend(context.Background(), newWSServer(t))
	require.NoError(t, err)
	defer b.Close()

	ch, err := b.SendFrame(context.Background(), proto.NewSetMode("default", 0))
	require.NoError(t, err)
	assert.Nil(t, ch, "set_mode carries no direct reply channel")
}
