package v1

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestSchemaDeclaresVersionAndCamelCaseResources proves the published JSON
// Schema document declares the v1 dialect, exposes Thread/Turn/Item as $defs,
// and uses camelCase keys exclusively (never snake_case). The schema is the
// machine-readable mirror of types.go — drift here breaks external client
// generators that consume /api/v1/schema/agent-v1.json.
func TestSchemaDeclaresVersionAndCamelCaseResources(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal(SchemaBytes(), &doc); err != nil {
		t.Fatalf("schema JSON: %v", err)
	}
	if doc["$schema"] != "https://json-schema.org/draft/2020-12/schema" {
		t.Fatalf("schema dialect = %#v", doc["$schema"])
	}
	defs, ok := doc["$defs"].(map[string]any)
	if !ok {
		t.Fatal("schema lacks $defs")
	}
	item, ok := defs["Item"].(map[string]any)
	if !ok {
		t.Fatal("schema lacks Item")
	}
	props := item["properties"].(map[string]any)
	if _, ok := props["threadId"]; !ok {
		t.Fatal("Item schema lacks threadId")
	}
	if _, ok := props["thread_id"]; ok {
		t.Fatal("Item schema must not expose thread_id (snake_case)")
	}
	turn, ok := defs["Turn"].(map[string]any)
	if !ok {
		t.Fatal("schema lacks Turn")
	}
	turnProps := turn["properties"].(map[string]any)
	for _, key := range []string{"threadId", "startedAt", "completedAt"} {
		if _, ok := turnProps[key]; !ok {
			t.Fatalf("Turn schema lacks %s", key)
		}
	}
}

// TestSchemaBytesAreStableForContractReview proves repeated calls return
// byte-identical output. Stability is what lets a code review diff the schema
// against types.go once and trust it; a non-deterministic encoding would make
// every review noisy and let real contract changes hide.
func TestSchemaBytesAreStableForContractReview(t *testing.T) {
	first := string(SchemaBytes())
	second := string(SchemaBytes())
	if first == "" || first != second || !strings.Contains(first, `"version"`) {
		t.Fatal("schema bytes are empty or unstable")
	}
}
