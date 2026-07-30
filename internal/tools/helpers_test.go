package tools

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestParamsPreservesArrayItemsAndObjectProperties verifies that the updated
// params helper correctly handles nested schemas: Array items via ElemInfo and
// Object properties via SubParams.
func TestParamsPreservesArrayItemsAndObjectProperties(t *testing.T) {
	got := params(map[string]*schema.ParameterInfo{
		"domains": {Type: schema.Array, Required: true, ElemInfo: &schema.ParameterInfo{Type: schema.String, Desc: "hostname"}},
		"scope": {Type: schema.Object, SubParams: map[string]*schema.ParameterInfo{
			"kind": {Type: schema.String, Required: true, Enum: []string{"working_tree", "base_ref", "commit"}},
			"ref":  {Type: schema.String},
		}},
	})
	js, err := got.ToJSONSchema()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(js)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, want := range []string{
		`"domains"`, `"items":{"description":"hostname","type":"string"}`,
		`"scope"`, `"properties"`, `"kind"`, `"required":["kind"]`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("schema %s missing %s", text, want)
		}
	}
}

// TestToJSON_ErrorProducesValidJSON verifies that when json.Marshal fails
// (e.g. for an unmarshallable type like chan int), toJSON returns a properly
// marshaled JSON object with an "error" key — not a hand-built string that
// breaks on special characters.
func TestToJSON_ErrorProducesValidJSON(t *testing.T) {
	// chan int cannot be marshaled by encoding/json.
	result := toJSON(make(chan int))

	// The result must be valid JSON that unmarshals into a map with an "error" key.
	var m map[string]string
	err := json.Unmarshal([]byte(result), &m)
	require.NoError(t, err, "toJSON error output must be valid JSON, got: %s", result)
	assert.Contains(t, m["error"], "chan")
}

// TestToJSON_Success verifies normal marshaling still works.
func TestToJSON_Success(t *testing.T) {
	result := toJSON(map[string]string{"key": "value"})
	var m map[string]string
	err := json.Unmarshal([]byte(result), &m)
	require.NoError(t, err)
	assert.Equal(t, "value", m["key"])
}
