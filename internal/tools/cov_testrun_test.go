package tools

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// ---------------------------------------------------------------------------
// parseNPMOutput — various JSON shapes
// ---------------------------------------------------------------------------

func TestParseNPMOutput_NoJSON(t *testing.T) {
	r := parseNPMOutput("no json here")
	assert.Equal(t, "error", r.Status)
	assert.Contains(t, r.Summary, "no JSON")
}

func TestParseNPMOutput_InvalidJSON(t *testing.T) {
	r := parseNPMOutput("{bad json}")
	assert.Equal(t, "error", r.Status)
}

func TestParseNPMOutput_ValidWithFailures(t *testing.T) {
	raw := `{
		"stats": {"passes": 3, "failures": 1, "pending": 2},
		"failures": [{"name": "test fail"}]
	}`
	r := parseNPMOutput(raw)
	assert.Equal(t, 3, r.Passed)
	assert.Equal(t, 1, r.Failed)
	assert.Equal(t, 2, r.Skipped)
	assert.Equal(t, "fail", r.Status)
	assert.Len(t, r.Failures, 1)
	assert.Equal(t, "test fail", r.Failures[0].Test)
}

func TestParseNPMOutput_AllPass(t *testing.T) {
	raw := `{
		"stats": {"passes": 5, "failures": 0, "pending": 0},
		"failures": []
	}`
	r := parseNPMOutput(raw)
	assert.Equal(t, "pass", r.Status)
	assert.Equal(t, 5, r.Passed)
}

func TestParseCargoOutput_WithFailures(t *testing.T) {
	raw := `test result: 5 passed; 2 failed; 1 ignored
failures:
    test_a
    test_b
---- stdout ----`
	r := parseCargoOutput(raw)
	assert.Equal(t, 5, r.Passed)
	assert.Equal(t, 2, r.Failed)
	assert.Equal(t, "fail", r.Status)
	assert.Len(t, r.Failures, 2)
}

func TestParseGoJSON_InvalidLines(t *testing.T) {
	// Mix of valid and invalid JSON lines
	raw := `not json line
{"Action":"pass","Package":"pkg","Test":"TestFoo"}
another bad line
{"Action":"fail","Package":"pkg","Test":"TestBar"}`
	r := parseGoJSON(raw)
	assert.Equal(t, 1, r.Passed)
	assert.Equal(t, 1, r.Failed)
	assert.Equal(t, "fail", r.Status)
}

// ---------------------------------------------------------------------------
// detectRunner / detectMarkerFiles
// ---------------------------------------------------------------------------

func TestDetectRunner_All(t *testing.T) {
	assert.Equal(t, "go", detectRunner(map[string]bool{"go.mod": true}))
	assert.Equal(t, "cargo", detectRunner(map[string]bool{"Cargo.toml": true}))
	assert.Equal(t, "npm", detectRunner(map[string]bool{"package.json": true}))
	assert.Equal(t, "go", detectRunner(map[string]bool{"go.mod": true, "Cargo.toml": true}))
	assert.Equal(t, "", detectRunner(map[string]bool{}))
}

func TestTestSpec_UnknownFramework(t *testing.T) {
	spec, err := testSpec("unknown", runTestsArgs{}, "/root")
	assert.NoError(t, err)
	assert.Equal(t, "", spec.Program)
}
