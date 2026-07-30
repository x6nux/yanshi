package http

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestCov_CompileSchema_UnresolvableRef covers the jsonschema.Compile error
// branch: a $ref to a missing definition fails to compile.
func TestCov_CompileSchema_UnresolvableRef(t *testing.T) {
	_, err := compileSchema(json.RawMessage(`{"$ref":"#/definitions/missing"}`))
	assert.Error(t, err)
}
