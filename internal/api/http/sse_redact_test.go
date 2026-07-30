package http

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/secrets"
)

// TestSSE_RedactsAllFrameTypes validates the serialized boundary shared by
// every ServerFrame passed through writeSSEFrame. The test deliberately calls
// the boundary helper directly; it does not pretend to execute each chat.go
// handler branch. Because all production call sites must adopt the new helper
// signature for chat.go to compile, one boundary implementation protects them
// uniformly. The secret contains JSON-meaningful bytes (quote + backslash), so
// RedactJSON must redact both raw and escaped spellings.
func TestSSE_RedactsAllFrameTypes(t *testing.T) {
	r := secrets.NewRedactor()
	secret := `sk-sse-"quoted"-\slash`
	r.Register(secret)
	structured, err := json.Marshal(map[string]string{"value": secret})
	if err != nil {
		t.Fatal(err)
	}

	cases := []proto.ServerFrame{
		proto.NewError("failed with key " + secret),
		proto.NewDone(),
		proto.NewCompactChunk("chunk contains " + secret),
		proto.NewHistoryReplaced(nil),
		proto.NewStatus("m", "", 0, 0, 0, 0),
		proto.NewStructuredResult(structured),
	}
	for i, f := range cases {
		rec := httptest.NewRecorder()
		writeSSEFrame(rec, nil, f, r)
		body := rec.Body.String()
		if strings.Contains(body, secret) || strings.Contains(body, `sk-sse-\"quoted\"-\\slash`) {
			t.Fatalf("case %d (%s) leaked secret: %q", i, f.Type, body)
		}
	}

	// writeSSEError is the pre-stream path used by both current call sites.
	rec := httptest.NewRecorder()
	writeSSEError(rec, "pre-stream error with "+secret, r)
	if strings.Contains(rec.Body.String(), secret) ||
		strings.Contains(rec.Body.String(), `sk-sse-\"quoted\"-\\slash`) {
		t.Fatalf("writeSSEError leaked: %q", rec.Body.String())
	}
}
