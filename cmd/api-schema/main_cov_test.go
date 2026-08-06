package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestCov_Run_RequiresMarkdown covers the no-args path.
//
// It used to print a hardcoded TypeScript literal to stdout and this test
// asserted the literal contained "export type ItemType" — an assertion about a
// string constant in the same package, which held no matter what the wire
// contract said. The TS half is gone; the command now has exactly one job.
func TestCov_Run_RequiresMarkdown(t *testing.T) {
	var stderr bytes.Buffer
	code := run(nil, io.Discard, &stderr)
	assert.Equal(t, 2, code)
	assert.Contains(t, stderr.String(), "-markdown")
}

// TestCov_Run_Markdown covers the -markdown dispatch path.
func TestCov_Run_Markdown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schema.md")
	code := run([]string{"-markdown", path}, io.Discard, io.Discard)
	assert.Equal(t, 0, code)
	_, err := os.Stat(path)
	assert.NoError(t, err, "markdown file created")
}

// TestCov_Run_BadFlag covers the flag-parse-failure path.
func TestCov_Run_BadFlag(t *testing.T) {
	code := run([]string{"-bogus"}, io.Discard, io.Discard)
	assert.Equal(t, 2, code)
}
