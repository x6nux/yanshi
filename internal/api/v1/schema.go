package v1

import "encoding/json"

// schemaDocument is the in-memory JSON Schema document for the v1 Agent API.
// It declares Thread/Turn/Item resources with camelCase property names and the
// provisional version envelope. The schema is intentionally a Go literal (not
// a parsed embedded asset) so a code review can diff it against the wire types
// in types.go side by side. SchemaBytes re-encodes it on every call so callers
// cannot mutate the global document and accidentally make HTTP and JSON-RPC
// schemas diverge.
//
// Task 9 expands this document (params/response shapes, item type enum) and
// adds compatibility-matrix tests that assert every camelCase key.
var schemaDocument = map[string]any{
	"$schema": "https://json-schema.org/draft/2020-12/schema",
	"$id":     "https://yanshi.dev/schema/agent-api-v1.json",
	"title":   "Yanshi Agent API v1",
	"type":    "object",
	"$defs": map[string]any{
		"Thread": map[string]any{
			"type":     "object",
			"required": []string{"version", "id", "status", "createdAt", "updatedAt"},
			"properties": map[string]any{
				"version":   map[string]any{"const": "v1"},
				"id":        map[string]any{"type": "string"},
				"status":    map[string]any{"type": "string"},
				"title":     map[string]any{"type": "string"},
				"createdAt": map[string]any{"type": "integer"},
				"updatedAt": map[string]any{"type": "integer"},
				"model":     map[string]any{"type": "string"},
				"thinking":  map[string]any{"type": "string"},
				"turns":     map[string]any{"type": "array", "items": map[string]any{"$ref": "#/$defs/Turn"}},
			},
		},
		"Turn": map[string]any{
			"type":     "object",
			"required": []string{"version", "id", "threadId", "status", "input", "startedAt"},
			"properties": map[string]any{
				"version":      map[string]any{"const": "v1"},
				"id":           map[string]any{"type": "string"},
				"threadId":     map[string]any{"type": "string"},
				"status":       map[string]any{"type": "string"},
				"input":        map[string]any{"type": "string"},
				"startedAt":    map[string]any{"type": "integer"},
				"completedAt":  map[string]any{"type": "integer"},
			},
		},
		"Item": map[string]any{
			"type":     "object",
			"required": []string{"version", "id", "sequence", "threadId", "turnId", "type"},
			"properties": map[string]any{
				"version":          map[string]any{"const": "v1"},
				"id":               map[string]any{"type": "string"},
				"sequence":         map[string]any{"type": "integer", "minimum": 1},
				"threadId":         map[string]any{"type": "string"},
				"turnId":           map[string]any{"type": "string"},
				"type":             map[string]any{"type": "string"},
				"text":             map[string]any{"type": "string"},
				"toolName":         map[string]any{"type": "string"},
				"toolArgs":         map[string]any{"type": "string"},
				"status":           map[string]any{"type": "string"},
				"error":            map[string]any{"type": "string"},
				"structuredResult": map[string]any{},
			},
		},
	},
}

// SchemaBytes returns a fresh JSON encoding of the v1 schema document so
// callers cannot mutate the global schemaDocument and accidentally make HTTP
// and JSON-RPC schemas diverge. The same bytes back GET /api/v1/schema/agent-v1.json
// and any future tooling that generates client types from the schema.
func SchemaBytes() []byte {
	data, err := json.Marshal(schemaDocument)
	if err != nil {
		// Marshal of a hand-built map[string]any cannot fail in practice; if it
		// ever does, return a minimal valid schema so the endpoint never serves
		// invalid JSON.
		return []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"https://yanshi.dev/schema/agent-api-v1.json"}`)
	}
	return data
}
