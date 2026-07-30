package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCov_Run_Stdout covers the no-args → stdout TypeScript path.
func TestCov_Run_Stdout(t *testing.T) {
	var stdout bytes.Buffer
	code := run(nil, &stdout, io.Discard)
	assert.Equal(t, 0, code)
	assert.Contains(t, stdout.String(), "export type ItemType")
}

// TestCov_Run_OutFile covers the -out file-write path.
func TestCov_Run_OutFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v1.ts")
	code := run([]string{"-out", path}, io.Discard, io.Discard)
	assert.Equal(t, 0, code)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(data), "export interface Thread")
}

// TestCov_Run_Markdown covers the -markdown dispatch path.
func TestCov_Run_Markdown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schema.md")
	code := run([]string{"-markdown", path}, io.Discard, io.Discard)
	assert.Equal(t, 0, code)
	_, err := os.Stat(path)
	assert.NoError(t, err, "markdown file created")
}

// TestCov_Run_MutuallyExclusive covers the -out + -markdown rejection.
func TestCov_Run_MutuallyExclusive(t *testing.T) {
	var stderr bytes.Buffer
	code := run([]string{"-out", "a", "-markdown", "b"}, io.Discard, &stderr)
	assert.Equal(t, 2, code)
	assert.Contains(t, stderr.String(), "mutually exclusive")
}

// TestCov_Run_BadFlag covers the flag-parse-failure path.
func TestCov_Run_BadFlag(t *testing.T) {
	code := run([]string{"-bogus"}, io.Discard, io.Discard)
	assert.Equal(t, 2, code)
}
