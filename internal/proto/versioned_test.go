package proto

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestVersionedFrameCarriesV1AndCamelCase proves the provisional envelope
// stamps Version="v1" on every frame and exposes camelCase keys for thread/turn
// correlation. The envelope is a helper for transport layers that want a
// uniform frame shape around v1 resources.
func TestVersionedFrameCarriesV1AndCamelCase(t *testing.T) {
	fr, err := NewVersionedFrame(7, "item", "thread-1", "turn-1", map[string]any{"text": "hi"})
	if err != nil {
		t.Fatalf("NewVersionedFrame: %v", err)
	}
	if fr.Version != AgentAPIVersionV1 {
		t.Fatalf("version = %q, want %q", fr.Version, AgentAPIVersionV1)
	}
	data, err := json.Marshal(fr)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got := string(data)
	for _, want := range []string{`"version":"v1"`, `"threadId":"thread-1"`, `"turnId":"turn-1"`, `"sequence":7`} {
		if !strings.Contains(got, want) {
			t.Fatalf("frame JSON %q lacks %s", got, want)
		}
	}
}

// TestNewVersionedFrameRejectsBadPayload proves a marshal failure is surfaced
// rather than producing a half-built frame. The caller must propagate the
// error as an internal server error, not a v1 resource.
func TestNewVersionedFrameRejectsBadPayload(t *testing.T) {
	_, err := NewVersionedFrame(1, "x", "t", "u", make(chan int))
	if err == nil {
		t.Fatal("expected marshal error for unmarshallable payload")
	}
}

// TestVersionedFrame_VersionFieldRoundTrip proves the Version field survives
// marshal→unmarshal and reads back as the v1 constant, and that the
// correlation fields (Sequence/ThreadID/TurnID) round-trip.
func TestVersionedFrame_VersionFieldRoundTrip(t *testing.T) {
	in, err := NewVersionedFrame(42, "item", "th-1", "tn-1", map[string]any{"k": "v"})
	if err != nil {
		t.Fatalf("NewVersionedFrame: %v", err)
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got VersionedFrame
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	assert.Equal(t, AgentAPIVersionV1, got.Version, "Version must round-trip as v1")
	assert.Equal(t, in.Sequence, got.Sequence)
	assert.Equal(t, in.ThreadID, got.ThreadID)
	assert.Equal(t, in.TurnID, got.TurnID)
}
