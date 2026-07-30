package cli

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/proto"
)

func TestFakeBackend_StreamsScriptedEvents(t *testing.T) {
	b := newFakeBackend([]string{"chunk1", "chunk2"})
	ch, err := b.Send(context.Background(), "hi")
	require.NoError(t, err)

	var texts []string
	var sawDone bool
	for ev := range ch {
		switch ev.Kind {
		case "agent_chunk":
			texts = append(texts, ev.Text)
		case "done":
			sawDone = true
		}
	}
	assert.Equal(t, []string{"chunk1", "chunk2"}, texts)
	assert.True(t, sawDone)
	assert.Equal(t, "fake", b.Mode())
}

// TestFakeBackend_SendFrame_Records verifies the fake records control frames so
// tests can assert on which frame a command produced (and returns nil — no
// synthetic reply; tests drive applyEvent of replies directly).
func TestFakeBackend_SendFrame_Records(t *testing.T) {
	b := newFakeBackend(nil)

	ch, err := b.SendFrame(context.Background(), proto.NewClear())
	require.NoError(t, err)
	assert.Nil(t, ch, "fake SendFrame returns no reply channel")

	ch, err = b.SendFrame(context.Background(), proto.NewSetModel("claude-opus-4"))
	require.NoError(t, err)
	assert.Nil(t, ch)

	got := b.Frames()
	require.Len(t, got, 2)
	assert.Equal(t, "clear", got[0].Type)
	assert.Equal(t, proto.NewSetModel("claude-opus-4"), got[1])
}

// TestToStreamEventMapsCostAndFeatures proves the WS backend's toStreamEvent
// maps the COST1 / OBS3 frame fields into the StreamEvent shape the TUI reads.
// This is the wire-format-to-TUI seam: missing it would leave /cost and
// /features silently always-zero in the TUI even when the server is sending
// real numbers.
func TestToStreamEventMapsCostAndFeatures(t *testing.T) {
	frame := proto.ServerFrame{
		Type:      "status",
		CostUSD:   0.42,
		CostKnown: true,
		Features:  []proto.FeatureRow{{Key: "x", Stage: "beta", Enabled: true, Owner: "C4"}},
	}
	_ = proto.NewFeaturesSet("k", false) // ensure constructor compiles
	ev := toStreamEvent(frame)
	if ev.CostUSD != 0.42 || !ev.CostKnown || len(ev.Features) != 1 {
		t.Fatalf("event = %+v", ev)
	}
}

// TestIsControlReplyIncludesFeatures proves the "features" reply closes the
// control-reply channel so SendFrame callers waiting on /features unblock when
// the table arrives (instead of hanging until the WS read deadline).
func TestIsControlReplyIncludesFeatures(t *testing.T) {
	if !isControlReply("features") {
		t.Fatal(`"features" must close the control reply channel`)
	}
}
