package v1

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestResumeDoesNotAdvertiseItemsItCannotDeliver pins the removal of
// ThreadSnapshot.Items and ThreadResumeResponse.Items.
//
// Both carried `items,omitempty`, both were forwarded by BOTH transports (the
// HTTP resume handler and the JSON-RPC dispatch), both were declared in the
// schema and in the TypeScript and Python clients — and Service.snapshot never
// set either one. Every client that resumed a thread got a field that is
// always absent, from an API whose own type comment called it "best-effort".
//
// Filling it was the other option and it is worse: what the store holds is
// MESSAGES, while an Item is an event with a turnId, a per-thread sequence and
// a type drawn from a streaming vocabulary (message.delta, tool.call, …). None
// of those exist for a message read back from the store, so filling the field
// means minting a turnId, a sequence and a type that describe nothing. A wire
// field populated with invented values is worse than an absent one: the client
// cannot tell it apart from the real thing.
//
// So the contract now says what the service does. Resume returns the thread;
// item history is what the stream is for, and v1's capabilities already state
// there is no cross-process replay.
func TestResumeDoesNotAdvertiseItemsItCannotDeliver(t *testing.T) {
	// Reflection, not a marshalled sample: `items,omitempty` omits an empty
	// slice, so a JSON round-trip of the zero value looks identical whether
	// the field exists or not. That is precisely how the field stayed in the
	// contract, always absent, without any test noticing.
	for _, typ := range []any{ThreadSnapshot{}, ThreadResumeResponse{}} {
		rt := reflect.TypeOf(typ)
		if _, ok := rt.FieldByName("Items"); ok {
			t.Errorf("%s still declares Items; the service never sets it", rt.Name())
		}
	}

	// The schema must agree, or a client generated from it re-introduces the
	// field the Go type no longer has.
	var doc struct {
		Defs map[string]struct {
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(SchemaBytes(), &doc); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}
	for _, name := range []string{"ThreadResumeResponse"} {
		if _, ok := doc.Defs[name].Properties["items"]; ok {
			t.Errorf("the schema still declares %s.items", name)
		}
	}
}
