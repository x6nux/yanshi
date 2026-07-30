package auth

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestCov_ValidateEndpoint_LoopbackIPLiteral covers the net.ParseIP+IsLoopback
// branch: 127.0.0.2 is loopback (127/8) but not the literal "127.0.0.1"/"::1"/
// "localhost" handled earlier, so plain HTTP to it is allowed.
func TestCov_ValidateEndpoint_LoopbackIPLiteral(t *testing.T) {
	assert.NoError(t, validateEndpoint("http://127.0.0.2"))
}
