package instruct

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestCov_RelOrSelf_DifferentVolume covers the filepath.Rel-error branch: when
// dir is on a different Windows drive than root, Rel cannot make a relative path
// and the absolute dir is returned unchanged.
func TestCov_RelOrSelf_DifferentVolume(t *testing.T) {
	got := relOrSelf("C:\\root", "D:\\other")
	assert.Equal(t, "D:\\other", got)
}
