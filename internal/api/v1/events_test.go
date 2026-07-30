package v1

import (
	"encoding/json"
	"testing"

	"github.com/x6nux/yanshi/internal/proto"
)

// TestItemFromServerFrame proves the legacy -> v1 mapping preserves the stable
// item types clients depend on, plus the sequence and version envelope. Every
// legacy frame type maps to exactly one Item.Type constant.
func TestItemFromServerFrame(t *testing.T) {
	cases := []struct {
		frame proto.ServerFrame
		kind  string
		text  string
	}{
		{proto.NewAgentChunk("hello"), ItemMessageDelta, "hello"},
		{proto.NewThinking("think"), ItemReasoningDelta, "think"},
		{proto.NewToolCall("fs_read", `{"path":"a.go"}`, "running"), ItemToolCall, ""},
		{proto.NewToolResult("fs_read", "ok", "ok"), ItemToolResult, "ok"},
		{proto.NewError("boom"), ItemTurnError, "boom"},
		{proto.NewDone(), ItemTurnCompleted, ""},
	}
	for _, tc := range cases {
		got := ItemFromServerFrame(tc.frame, "thread-1", "turn-1", 9)
		if got.Version != Version || got.Type != tc.kind || got.Text != tc.text || got.Sequence != 9 {
			t.Fatalf("frame %#v -> %#v", tc.frame, got)
		}
		if got.ThreadID != "thread-1" || got.TurnID != "turn-1" || got.ID != "item-9" {
			t.Fatalf("correlation/id fields = %#v", got)
		}
	}
}

// TestItemFromServerFramePreservesStructuredResult proves the structured_result
// frame (A12 schema-constrained output) round-trips its raw JSON payload into
// the v1 item's StructuredResult field without re-encoding or escaping.
func TestItemFromServerFramePreservesStructuredResult(t *testing.T) {
	data := json.RawMessage(`{"ok":true}`)
	got := ItemFromServerFrame(proto.NewStructuredResult(data), "t", "u", 1)
	if got.Type != ItemStructuredResult || string(got.StructuredResult) != string(data) {
		t.Fatalf("structured item = %#v", got)
	}
}

// TestItemFromServerFrameMapsUnknownLegacyTypeAsEventPrefix proves a frame type
// the v1 mapping does not recognize is preserved as "event.<legacyType>" so
// the wire never silently loses events. The client treats it as an unknown
// item type and ignores it.
func TestItemFromServerFrameMapsUnknownLegacyTypeAsEventPrefix(t *testing.T) {
	unknown := proto.ServerFrame{Type: "future_telemetry", Text: "x"}
	got := ItemFromServerFrame(unknown, "t", "u", 5)
	if got.Type != "event.future_telemetry" {
		t.Fatalf("unknown frame type mapping = %q", got.Type)
	}
}

// TestItemFromServerFrameMapsToolProgressAliases proves both tool_chunk and
// tool_progress legacy types collapse to the single v1 ItemToolProgress type.
// Either alias surfaces the same wire contract to clients.
func TestItemFromServerFrameMapsToolProgressAliases(t *testing.T) {
	for _, typ := range []string{"tool_chunk", "tool_progress"} {
		f := proto.ServerFrame{Type: typ, ToolName: "shell_run", Text: "line"}
		got := ItemFromServerFrame(f, "t", "u", 1)
		if got.Type != ItemToolProgress {
			t.Fatalf("alias %q mapped to %q, want %s", typ, got.Type, ItemToolProgress)
		}
	}
}
