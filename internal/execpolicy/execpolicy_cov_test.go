package execpolicy

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestCov_Lex_TrailingEscape covers the trailing-backslash-inside-dquote error.
func TestCov_Lex_TrailingEscape(t *testing.T) {
	_, err := Lex("\"\\") // dquote then a lone backslash at end
	assert.Error(t, err)
}

// TestCov_Lex_ForbiddenExpansion covers the expansion/glob reject inside a
// quoted word (e.g. "$x").
func TestCov_Lex_ForbiddenExpansion(t *testing.T) {
	_, err := Lex("\"$x\"") // '$' inside double quotes
	assert.Error(t, err)
}

// TestCov_ForbiddenExpansionAt_OutOfRange covers the i>=len guard.
func TestCov_ForbiddenExpansionAt_OutOfRange(t *testing.T) {
	assert.False(t, forbiddenExpansionAt("abc", 3))
	assert.False(t, forbiddenExpansionAt("", 0))
}

// TestCov_IsDigits_NonDigit covers the non-digit rejection branch.
func TestCov_IsDigits_NonDigit(t *testing.T) {
	assert.False(t, isDigits("12a"))
	assert.False(t, isDigits(""))
	assert.True(t, isDigits("007"))
}

// TestCov_Parse_LexErrorProp covers Parse propagating a Lex error.
func TestCov_Parse_LexErrorProp(t *testing.T) {
	_, err := Parse("\"$x\"")
	assert.Error(t, err)
}

// TestCov_Parse_PipeNoProgram covers the flushSegment error on a leading pipe.
func TestCov_Parse_PipeNoProgram(t *testing.T) {
	_, err := Parse("| foo")
	assert.Error(t, err)
}

// TestCov_Parse_AndIfNoProgram covers the flushSegment error on a leading &&.
func TestCov_Parse_AndIfNoProgram(t *testing.T) {
	_, err := Parse("&& foo")
	assert.Error(t, err)
}

// TestCov_HasPrefix_Mismatch covers the element-mismatch branch.
func TestCov_HasPrefix_Mismatch(t *testing.T) {
	assert.False(t, hasPrefix([]string{"a", "b"}, []string{"a", "x"}))
	assert.False(t, hasPrefix([]string{"a"}, []string{"a", "b"}), "prefix longer than args")
	assert.True(t, hasPrefix([]string{"a", "b"}, []string{"a"}))
}
