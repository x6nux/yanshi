package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCov_Markers covers the beginMarker/endMarker helpers.
func TestCov_Markers(t *testing.T) {
	assert.Contains(t, beginMarker("x"), "BEGIN GENERATED: x")
	assert.Contains(t, endMarker("x"), "END GENERATED: x")
}

// TestCov_RefName covers both the "/-present" and bare-string branches.
func TestCov_RefName(t *testing.T) {
	assert.Equal(t, "Turn", refName("#/$defs/Turn"))
	assert.Equal(t, "nodash", refName("nodash"))
}

// TestCov_PropertyType_Ref covers the $ref → refName branch (no const/type).
func TestCov_PropertyType_Ref(t *testing.T) {
	assert.Equal(t, "Turn", propertyType(map[string]any{"$ref": "#/$defs/Turn"}))
}

// TestCov_OrderedBlockIDs_Leftover covers the leftover (unexpected id) branch.
func TestCov_OrderedBlockIDs_Leftover(t *testing.T) {
	blocks := map[string]string{
		"api-schema-full":    "full",
		"api-defs:Thread":    "thread",
		"api-defs:zzz-extra": "extra", // not in defOrder → leftover
	}
	ids := orderedBlockIDs(blocks)
	assert.Equal(t, "api-schema-full", ids[0])
	assert.Equal(t, "api-defs:zzz-extra", ids[len(ids)-1], "leftover appended last (sorted)")
}

// TestCov_RunMarkdown_Bootstrap covers the create-new-file branch.
func TestCov_RunMarkdown_Bootstrap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schema.md")
	require.NoError(t, runMarkdown(path))
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	s := string(data)
	assert.Contains(t, s, "v1 JSON Schema")
	assert.Contains(t, s, "BEGIN GENERATED: api-schema-full")
}

// TestCov_RunMarkdown_Rewrite covers the rewrite-existing-marker branch.
func TestCov_RunMarkdown_Rewrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schema.md")
	seed := beginMarker("api-schema-full") + "\nold stale content\n" + endMarker("api-schema-full") + "\n"
	require.NoError(t, os.WriteFile(path, []byte(seed), 0o644))
	require.NoError(t, runMarkdown(path))
	data, _ := os.ReadFile(path)
	s := string(data)
	assert.Contains(t, s, "BEGIN GENERATED: api-schema-full", "marker preserved")
	assert.Contains(t, s, "{", "block rewritten with the real JSON schema")
	assert.NotContains(t, s, "stale content", "old block content replaced")
}

// TestCov_RunMarkdown_NoMarkerSkip covers the "file carries no markers → leave
// unchanged" branch.
func TestCov_RunMarkdown_NoMarkerSkip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schema.md")
	seed := "# plain doc\nno generated markers here\n"
	require.NoError(t, os.WriteFile(path, []byte(seed), 0o644))
	require.NoError(t, runMarkdown(path))
	data, _ := os.ReadFile(path)
	assert.Equal(t, seed, string(data), "file without markers left unchanged")
}
