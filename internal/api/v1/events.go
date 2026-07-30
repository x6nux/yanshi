package v1

import (
	"strconv"

	"github.com/x6nux/yanshi/internal/proto"
)

// ItemFromServerFrame is the SINGLE mapping from the legacy transport
// vocabulary (proto.ServerFrame) to the public v1 resource vocabulary (Item).
// Keeping it centralized prevents HTTP and JSON-RPC from disagreeing about the
// meaning of a streamed event: every transport that surfaces items to an
// external client goes through this function.
//
// Unknown frame types are mapped to "event.<legacyType>" rather than dropped,
// per the v1 contract: clients ignore unknown item types, but the server
// preserves them so the wire never silently loses events when a new frame is
// added before the v1 mapping learns about it.
func ItemFromServerFrame(f proto.ServerFrame, threadID, turnID string, sequence int64) Item {
	item := Item{
		Version: Version, ID: "item-" + formatSequence(sequence), Sequence: sequence,
		ThreadID: threadID, TurnID: turnID, Status: f.Status,
		Text:             f.Text,
		ToolName:         f.ToolName,
		ToolArgs:         f.ToolArgs,
		StructuredResult: f.StructuredResult,
	}
	switch f.Type {
	case "agent_chunk":
		item.Type = ItemMessageDelta
	case "thinking":
		item.Type = ItemReasoningDelta
	case "tool_call":
		item.Type = ItemToolCall
	case "tool_result":
		item.Type = ItemToolResult
	case "tool_chunk", "tool_progress":
		item.Type = ItemToolProgress
	case "structured_result":
		item.Type = ItemStructuredResult
	case "error":
		item.Type = ItemTurnError
		item.Error = f.Text
	case "done":
		item.Type = ItemTurnCompleted
	default:
		// Unknown legacy frame: preserve its type so the v1 client can at least
		// see that something was emitted. Never silently drop.
		item.Type = "event." + f.Type
	}
	return item
}

// formatSequence renders a sequence number as the decimal suffix of the item
// id. Negative values should never occur (sequence starts at 1 and increments);
// if one does, it is clamped to "0" so the id stays a valid identifier.
func formatSequence(sequence int64) string {
	if sequence < 0 {
		return "0"
	}
	return strconv.FormatInt(sequence, 10)
}
