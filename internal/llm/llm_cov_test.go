package llm

import (
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCov_RetryableError_Unwrap covers the Unwrap method so errors.Is/As can
// reach the wrapped error.
func TestCov_RetryableError_Unwrap(t *testing.T) {
	err := Retryable(io.EOF)
	assert.True(t, errors.Is(err, io.EOF), "Unwrap exposes the wrapped error to errors.Is")
	re, ok := err.(*RetryableError)
	require.True(t, ok)
	assert.Equal(t, io.EOF, re.Unwrap())
}
