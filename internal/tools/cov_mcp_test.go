package tools

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// buildMCPParams — various input schema shapes
// ---------------------------------------------------------------------------

func TestBuildMCPParams_InvalidJSON(t *testing.T) {
	p := buildMCPParams(`not-json`)
	assert.Nil(t, p)
}

func TestBuildMCPParams_Empty(t *testing.T) {
	p := buildMCPParams(`{}`)
	// Empty object → no properties → params() returns nil
	assert.Nil(t, p)
}

func TestBuildMCPParams_WithProperties(t *testing.T) {
	raw := `{
		"properties": {
			"name": {"type": "string", "description": "a name"},
			"count": {"type": "integer"}
		}
	}`
	p := buildMCPParams(raw)
	require.NotNil(t, p)
}

func TestBuildMCPParams_NoProperties(t *testing.T) {
	raw := `{"type": "object"}`
	// No "properties" key → empty map → params() returns nil
	p := buildMCPParams(raw)
	assert.Nil(t, p)
}

func TestBuildMCPParams_NestedObjects(t *testing.T) {
	raw := `{
		"properties": {
			"config": {
				"type": "object",
				"properties": {
					"nested": {"type": "string"}
				}
			},
			"items": {
				"type": "array",
				"items": {"type": "string"}
			}
		}
	}`
	p := buildMCPParams(raw)
	require.NotNil(t, p)
}

func TestBuildMCPParams_PropertyWithMissingType(t *testing.T) {
	raw := `{
		"properties": {
			"weird": {"description": "no type"}
		}
	}`
	p := buildMCPParams(raw)
	require.NotNil(t, p)
}

func TestBuildMCPParams_PropertyNotObject(t *testing.T) {
	raw := `{
		"properties": {
			"bad": "not-an-object"
		}
	}`
	p := buildMCPParams(raw)
	require.NotNil(t, p)
	// Should not crash on non-map property values
}
