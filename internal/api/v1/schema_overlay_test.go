package v1

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdkschema "github.com/x6nux/yanshi/sdk/schema"
)

// collectRefs walks a decoded JSON Schema and returns every "$ref" string in it.
func collectRefs(node any, out map[string]bool) {
	switch v := node.(type) {
	case map[string]any:
		for k, child := range v {
			if k == "$ref" {
				if s, ok := child.(string); ok {
					out[s] = true
				}
				continue
			}
			collectRefs(child, out)
		}
	case []any:
		for _, child := range v {
			collectRefs(child, out)
		}
	}
}

// TestV11OverlayIsReachable proves the v1.1 delta actually constrains
// something.
//
// v1.1 is an allOf/$ref layer over v1 carrying one addition: Item gains
// reasoningTokens with minimum 0. That delta was DEAD. v1's root anyOf refers
// to "#/$defs/Item", and a JSON Pointer resolves in the $id scope of the
// document that wrote it — v1's own — so it can never reach v1.1's local
// $defs.Item. Measured with Ajv before the fix: register v1, compile v1.1,
// feed an item with reasoningTokens: -5 → valid, errors null. The one thing
// the overlay existed to say was unsayable.
//
// The assertion is structural rather than a validation run because the fix is
// structural: a local $defs nothing in the document $refs is unreachable no
// matter which validator you point at it, and pulling a JSON Schema
// implementation into internal/api/v1 to observe that would be a dependency
// bought to re-derive what the document already states.
//
// The mirror assertion — that a $ref to it exists — would pass on a document
// whose $defs and $refs are two disjoint sets, so both directions are checked:
// every local $defs must be referenced, and every local #/$defs/ ref must
// resolve.
func TestV11OverlayIsReachable(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal(sdkschema.V11(), &doc); err != nil {
		t.Fatalf("unmarshal v1.1: %v", err)
	}
	defs, _ := doc["$defs"].(map[string]any)
	if len(defs) == 0 {
		t.Skip("v1.1 carries no local $defs; there is no overlay to keep alive")
	}
	refs := map[string]bool{}
	collectRefs(doc, refs)

	for name := range defs {
		if !refs["#/$defs/"+name] {
			t.Errorf("v1.1 declares $defs.%s but nothing in the document $refs it.\n"+
				"A JSON Pointer inside the v1 document resolves in v1's own $id scope, "+
				"so v1's #/$defs/%s reaches v1's definition and never this one — the "+
				"override is dead weight that reads like a constraint.", name, name)
		}
	}
	for ref := range refs {
		local, ok := strings.CutPrefix(ref, "#/$defs/")
		if !ok {
			continue
		}
		if _, exists := defs[local]; !exists {
			t.Errorf("v1.1 $refs %q but declares no such $defs entry", ref)
		}
	}
}

// TestV11IsTheFileOnDisk keeps the embedded overlay and the file the
// TypeScript tests load from disk as one artifact. sdk/ts/tests resolve
// ../../schema/v1.1/ at runtime; an embed that had drifted would let Go and
// Ajv disagree about what v1.1 says while both reported green.
func TestV11IsTheFileOnDisk(t *testing.T) {
	onDisk, err := os.ReadFile(filepath.Join("..", "..", "..", "sdk", "schema", "v1.1", "agent-api.schema.json"))
	if err != nil {
		t.Fatalf("read v1.1: %v", err)
	}
	if string(onDisk) != string(sdkschema.V11()) {
		t.Error("the embedded v1.1 schema and the file on disk are not the same bytes")
	}
}
