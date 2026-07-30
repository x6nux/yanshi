package version_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/version"
)

// TestCov_Parse_EmptyBuildMetadata covers the "empty build metadata" branch (93-95):
// "1.2.3+" has a trailing '+' with nothing after it.
func TestCov_Parse_EmptyBuildMetadata(t *testing.T) {
	_, err := version.Parse("1.2.3+")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty build metadata")
}

// TestCov_ParseNum_EmptyComponent covers the "empty component" branch (128-130)
// in parseNum: "1..3" splits into ["1","","3"] so the middle part is empty.
func TestCov_ParseNum_EmptyComponent(t *testing.T) {
	_, err := version.Parse("1..3")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty component")
}

// TestCov_Parse_MinorFail covers the (114) err-from-parseNum block on the minor
// component: "1.X.3" passes major but fails minor with non-numeric.
func TestCov_Parse_MinorFail(t *testing.T) {
	_, err := version.Parse("1.X.3")
	require.Error(t, err)
}

// TestCov_Parse_PatchFail covers the (117) err-from-parseNum block on the patch
// component: "1.2.X" passes major+minor but fails patch with non-numeric.
func TestCov_Parse_PatchFail(t *testing.T) {
	_, err := version.Parse("1.2.X")
	require.Error(t, err)
}
