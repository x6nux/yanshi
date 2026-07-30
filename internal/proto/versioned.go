package proto

import "encoding/json"

// AgentAPIVersionV1 is the resource contract version for the Yanshi Agent API.
// It is intentionally separate from the legacy ClientFrame/ServerFrame wire
// vocabulary so the TUI compatibility surface is not tied to the public Agent
// API lifecycle. Requests may omit `version` (treated as v1); every response
// includes it. JSON-RPC's `jsonrpc:"2.0"` is the transport version and does
// NOT replace this resource version.
const AgentAPIVersionV1 = "v1"

// VersionedFrame is a provisional version envelope for streaming events that
// carry a v1 resource alongside transport metadata (sequence, thread/turn
// correlation). It is an optional helper for callers that want a single struct
// shape across item and control notifications; the canonical v1 wire type for
// streamed items remains v1.Item.
type VersionedFrame struct {
	Version  string          `json:"version"`
	Sequence int64           `json:"sequence"`
	Type     string          `json:"type"`
	ThreadID string          `json:"threadId"`
	TurnID   string          `json:"turnId"`
	Payload  json.RawMessage `json:"payload,omitempty"`
}

// NewVersionedFrame constructs a VersionedFrame with the v1 version constant
// and the payload marshaled inline. Returns an error if the payload cannot be
// marshaled (the caller's responsibility to surface as an internal error).
func NewVersionedFrame(sequence int64, typ, threadID, turnID string, payload any) (VersionedFrame, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return VersionedFrame{}, err
	}
	return VersionedFrame{
		Version: AgentAPIVersionV1, Sequence: sequence, Type: typ,
		ThreadID: threadID, TurnID: turnID, Payload: data,
	}, nil
}
