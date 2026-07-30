package cli

import (
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
)

// alwaysErrReader always fails to read.
type alwaysErrReader struct{}

func (alwaysErrReader) Read([]byte) (int, error) { return 0, errors.New("read boom") }

// chunkThenErrReader returns first on the first Read, then errors.
type chunkThenErrReader struct {
	first string
	done  bool
}

func (r *chunkThenErrReader) Read(p []byte) (int, error) {
	if !r.done {
		r.done = true
		return copy(p, []byte(r.first)), nil
	}
	return 0, errors.New("read boom")
}

// TestCov_ReadHeadlessInputs_TextReadError covers the text-mode io.ReadAll
// error branch.
func TestCov_ReadHeadlessInputs_TextReadError(t *testing.T) {
	_, err := ReadHeadlessInputs(alwaysErrReader{}, HeadlessInputText)
	assert.Error(t, err)
}

// TestCov_ReadHeadlessInputs_LinesScanError covers the lines-mode scanner.Err
// branch (one good line, then the reader errors).
func TestCov_ReadHeadlessInputs_LinesScanError(t *testing.T) {
	_, err := ReadHeadlessInputs(&chunkThenErrReader{first: "line\n"}, HeadlessInputLines)
	assert.Error(t, err)
}

// TestCov_ReadHeadlessInputs_JSONLScanError covers the jsonl-mode scanner.Err
// branch (one valid JSON line, then the reader errors).
func TestCov_ReadHeadlessInputs_JSONLScanError(t *testing.T) {
	_, err := ReadHeadlessInputs(io.Reader(&chunkThenErrReader{first: `{"prompt":"x"}` + "\n"}), HeadlessInputJSONL)
	assert.Error(t, err)
}
