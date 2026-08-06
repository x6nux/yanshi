package v1

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// sdkSchemaPath locates the SDK's copy from the repo root, which is three
// levels up from internal/api/v1.
func sdkSchemaPath(t *testing.T, rel string) string {
	t.Helper()
	return filepath.Join("..", "..", "..", "sdk", "schema", rel)
}

func defsOf(t *testing.T, raw []byte) []string {
	t.Helper()
	var doc struct {
		ID   string         `json:"$id"`
		Defs map[string]any `json:"$defs"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out := make([]string, 0, len(doc.Defs))
	for k := range doc.Defs {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestRuntimeSchemaIsTheSDKSchema pins the single source of truth for the v1
// wire contract.
//
// Before this there were THREE documents calling themselves the v1 schema:
// this package's Go literal (3 $defs, $id .../agent-api-v1.json), the SDK's
// sdk/schema/v1/agent-api.schema.json (21 $defs, a DIFFERENT $id) and the v1.1
// overlay. The only endpoint the product actually serves —
// GET /api/v1/schema/agent-v1.json — served the POOREST of the three, while
// every SDK client validated against the richest. A client that fetched the
// schema to learn the contract got a document that did not describe the
// contract its own SDK enforced, and the two even claimed different identities.
//
// Asserting equality of the $defs SET rather than of the bytes is deliberate:
// the point is that they are one document, and byte equality is what
// TestSchemaBytesAreStableForContractReview already covers.
//
// ledger: D1/V14#2 单一 schema 真相源且 parity 守门
//
// ledger: H2/APIREF1#3 与 schema 一致
func TestRuntimeSchemaIsTheSDKSchema(t *testing.T) {
	onDisk, err := os.ReadFile(sdkSchemaPath(t, filepath.Join("v1", "agent-api.schema.json")))
	if err != nil {
		t.Fatalf("read sdk schema: %v", err)
	}
	want := defsOf(t, onDisk)
	got := defsOf(t, SchemaBytes())

	if len(got) != len(want) {
		t.Fatalf("the served schema has %d $defs, the SDK schema has %d:\n served: %v\n sdk:    %v",
			len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("$defs differ at %d: served %q, sdk %q", i, got[i], want[i])
		}
	}

	var a, b struct {
		ID string `json:"$id"`
	}
	_ = json.Unmarshal(onDisk, &a)
	_ = json.Unmarshal(SchemaBytes(), &b)
	if a.ID != b.ID {
		t.Errorf("two identities for one contract: served $id %q, sdk $id %q", b.ID, a.ID)
	}
}
