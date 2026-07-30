package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestCov_TitleCaseSegments_EmptySeg covers the empty-segment skip (e.g. a
// double underscore yields an empty segment between two real ones).
func TestCov_TitleCaseSegments_EmptySeg(t *testing.T) {
	assert.Equal(t, "AB", titleCaseSegments("a__b"))
	assert.Equal(t, "A", titleCaseSegments("_a")) // leading empty segment skipped
}

// TestCov_ToolArgSummary_Branches covers the invalid-JSON raw fallback, the
// null→empty-map "no key" path (also exercising pickArgKey's shell_run→firstKey
// fallback and firstKey's empty-map return), and the nil-value "no scalar" path.
func TestCov_ToolArgSummary_Branches(t *testing.T) {
	// Invalid JSON → render the raw text so something useful shows.
	assert.Equal(t, "(not json)", toolArgSummary("x", "not json", "."))

	// "null" unmarshals to a nil map → no command/cmd → firstKey(empty) → "".
	assert.Equal(t, "", toolArgSummary("shell_run", "null", "."))

	// nil value → scalarToString("") → no summary.
	assert.Equal(t, "", toolArgSummary("fs_read", `{"path":null}`, "."))
}
