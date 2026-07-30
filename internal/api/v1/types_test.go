package v1

import (
	"encoding/json"
	"strings"

	"github.com/x6nux/yanshi/internal/proto"
	"testing"
)

// TestItemJSONUsesCamelCaseAndVersion proves every v1 resource uses camelCase
// wire keys and carries the provisional version envelope. The "version" field
// is always present; "threadId"/"turnId" never serialize as snake_case. This
// is the load-bearing wire contract for all HTTP and JSON-RPC responses.
func TestItemJSONUsesCamelCaseAndVersion(t *testing.T) {
	data, err := json.Marshal(Item{
		Version: "v1", ID: "item-1", ThreadID: "thread-1", TurnID: "turn-1",
		Sequence: 7, Type: ItemMessageDelta, Text: "hello",
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got := string(data)
	for _, want := range []string{`"version":"v1"`, `"threadId":"thread-1"`, `"turnId":"turn-1"`, `"sequence":7`} {
		if !strings.Contains(got, want) {
			t.Fatalf("JSON %q lacks %s", got, want)
		}
	}
	if strings.Contains(got, "thread_id") || strings.Contains(got, "turn_id") {
		t.Fatalf("wire JSON must be camelCase: %s", got)
	}
}

// TestUnknownFieldsAreIgnored proves v1 decoders do NOT call
// DisallowUnknownFields. A future field ("futureField") must not break a v1
// client; the service simply ignores it. This keeps the wire contract forward-
// compatible.
func TestUnknownFieldsAreIgnored(t *testing.T) {
	var p TurnStartParams
	if err := json.Unmarshal([]byte(`{"version":"v1","threadId":"t1","input":"hi","futureField":42}`), &p); err != nil {
		t.Fatalf("unknown future field should be ignored: %v", err)
	}
	if p.ThreadID != "t1" || p.Input != "hi" {
		t.Fatalf("params = %#v", p)
	}
}


func TestTurnStartParamsImagesIsCamelCaseAndOmittable(t *testing.T) {
	with, _ := json.Marshal(TurnStartParams{ThreadID: "t", Input: "hi", Images: []proto.ImageAttach{
		{ID: "img-1", Fmt: "png", DataB64: "AA"},
	}})
	if !strings.Contains(string(with), `"images":[`) || !strings.Contains(string(with), `"dataB64":"AA"`) {
		t.Fatalf("images must serialize camelCase: %s", with)
	}
	without, _ := json.Marshal(TurnStartParams{ThreadID: "t", Input: "hi"})
	if strings.Contains(string(without), "images") {
		t.Fatalf("text-only params must omit images (additive): %s", without)
	}
}
