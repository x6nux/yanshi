//go:build windows

package instruct

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestCov_RelOrSelf_DifferentVolume covers the filepath.Rel-error branch that
// is exclusive to Windows: when dir is on a different drive than root, Rel
// cannot produce a relative path, so relOrSelf returns the absolute dir
// unchanged. Posix has no drive letters, so this case cannot occur there.
func TestCov_RelOrSelf_DifferentVolume(t *testing.T) {
	assert.Equal(t, "D:\\other", relOrSelf("C:\\root", "D:\\other"))
}
