package instruct

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestCov_RelOrSelf covers the success branches that hold on every platform:
// an equal dir yields ".", and a nested dir yields a relative path. The
// Windows-only Rel-error branch (different drive letters) lives in
// instruct_cov_windows_test.go behind a build tag.
func TestCov_RelOrSelf(t *testing.T) {
	assert.Equal(t, ".", relOrSelf("/root", "/root"))
	assert.Equal(t, "sub", relOrSelf("/root", "/root/sub"))
}
