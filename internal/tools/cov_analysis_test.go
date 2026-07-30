package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// fillWorkflowTarget — valid JSON workflow, string-encoded, empty steps,
// invalid JSON
// ---------------------------------------------------------------------------

func TestFillWorkflowTarget_ValidJSON(t *testing.T) {
	raw := json.RawMessage(`{"steps":[{"id":"A1","prompt":"Analyze {{target}}","deps":[]}]}`)
	out, err := fillWorkflowTarget(raw, "/my/dir")
	require.NoError(t, err)

	var wf WorkflowDef
	require.NoError(t, json.Unmarshal(out, &wf))
	require.Len(t, wf.Steps, 1)
	assert.Equal(t, "Analyze /my/dir", wf.Steps[0].Prompt)
}

func TestFillWorkflowTarget_StringEncoded(t *testing.T) {
	// Workflow as a double-encoded JSON string
	encoded, _ := json.Marshal(`{"steps":[{"id":"A1","prompt":"Look at {{target}}"}]}`)
	raw := json.RawMessage(encoded)
	out, err := fillWorkflowTarget(raw, "/some/path")
	require.NoError(t, err)

	var wf WorkflowDef
	require.NoError(t, json.Unmarshal(out, &wf))
	require.Len(t, wf.Steps, 1)
	assert.Equal(t, "Look at /some/path", wf.Steps[0].Prompt)
}

func TestFillWorkflowTarget_EmptySteps(t *testing.T) {
	raw := json.RawMessage(`{"steps":[]}`)
	_, err := fillWorkflowTarget(raw, "/x")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at least one step")
}

func TestFillWorkflowTarget_InvalidJSON(t *testing.T) {
	raw := json.RawMessage(`not-json`)
	_, err := fillWorkflowTarget(raw, "/x")
	require.Error(t, err)
}

func TestFillWorkflowTarget_InvalidStringContent(t *testing.T) {
	// String that is not valid JSON workflow
	encoded, _ := json.Marshal(`just a string`)
	raw := json.RawMessage(encoded)
	_, err := fillWorkflowTarget(raw, "/x")
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// generateAnalysisWorkflow — directory with entries, empty dir, nonexistent
// dir, dir with only hidden/dot files
// ---------------------------------------------------------------------------

func TestGenerateAnalysisWorkflow_DirWithEntries(t *testing.T) {
	tmp := t.TempDir()
	// Create some files
	require.NoError(t, os.WriteFile(filepath.Join(tmp, "a.go"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(tmp, "b.go"), []byte("x"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(tmp, "sub"), 0o755))

	wf, err := generateAnalysisWorkflow(tmp, PredefinedAgentDef{
		Name:        "analysis",
		PromptTmpl:  "Analyze {{target}}",
		Description: "test",
	})
	require.NoError(t, err)
	assert.NotEmpty(t, wf.Steps)
	// First step should be A1 (inventory)
	assert.Equal(t, "A1", wf.Steps[0].ID)
	// Last step should be C1 (synthesis)
	assert.Equal(t, "C1", wf.Steps[len(wf.Steps)-1].ID)
}

func TestGenerateAnalysisWorkflow_NonexistentDir(t *testing.T) {
	// Nonexistent dir with a Workflow fallback in def
	def := PredefinedAgentDef{
		Name:       "analysis",
		PromptTmpl: "Analyze {{target}}",
		Workflow: &WorkflowDef{
			Steps: []WorkflowStepDef{
				{ID: "A1", Prompt: "Analyze {{target}}"},
			},
		},
	}
	wf, err := generateAnalysisWorkflow("/nonexistent/path/xyz", def)
	require.NoError(t, err)
	require.Len(t, wf.Steps, 1)
	assert.Equal(t, "A1", wf.Steps[0].ID)
	assert.Contains(t, wf.Steps[0].Prompt, "/nonexistent/path/xyz")
}

func TestGenerateAnalysisWorkflow_EmptyDir(t *testing.T) {
	tmp := t.TempDir()
	// Empty dir, no workflow fallback → error
	_, err := generateAnalysisWorkflow(tmp, PredefinedAgentDef{
		Name:       "analysis",
		PromptTmpl: "Analyze {{target}}",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty or unreadable")
}

func TestGenerateAnalysisWorkflow_EmptyDirWithFallback(t *testing.T) {
	tmp := t.TempDir()
	def := PredefinedAgentDef{
		Name:       "analysis",
		PromptTmpl: "x",
		Workflow: &WorkflowDef{
			Steps: []WorkflowStepDef{
				{ID: "S1", Prompt: "Step {{target}}"},
				{ID: "S2", Prompt: "Step 2", Deps: []string{"S1"}},
			},
		},
	}
	wf, err := generateAnalysisWorkflow(tmp, def)
	require.NoError(t, err)
	require.Len(t, wf.Steps, 2)
	assert.Equal(t, "S1", wf.Steps[0].ID)
	assert.Equal(t, "S2", wf.Steps[1].ID)
	assert.Contains(t, wf.Steps[0].Prompt, tmp)
}

func TestGenerateAnalysisWorkflow_OnlyHiddenFiles(t *testing.T) {
	tmp := t.TempDir()
	// Create only hidden/dot files and excluded dirs
	require.NoError(t, os.WriteFile(filepath.Join(tmp, ".hidden"), []byte("x"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(tmp, ".git"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(tmp, "node_modules"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(tmp, "vendor"), 0o755))

	def := PredefinedAgentDef{
		Name:       "analysis",
		PromptTmpl: "x",
		Workflow: &WorkflowDef{
			Steps: []WorkflowStepDef{{ID: "A1", Prompt: "fallback"}},
		},
	}
	wf, err := generateAnalysisWorkflow(tmp, def)
	require.NoError(t, err)
	// Since all entries are hidden/excluded, should fall back
	assert.NotEmpty(t, wf.Steps)
}

func TestGenerateAnalysisWorkflow_MaxEntries(t *testing.T) {
	tmp := t.TempDir()
	// Create 30 entries to trigger the maxEntries cap of 24
	for i := 0; i < 30; i++ {
		require.NoError(t, os.WriteFile(filepath.Join(tmp, "file%d.go"), []byte("x"), 0o644))
	}
	wf, err := generateAnalysisWorkflow(tmp, PredefinedAgentDef{
		Name:       "analysis",
		PromptTmpl: "x",
	})
	require.NoError(t, err)
	// Should have A1 + up to 24 B-steps + C1 = max 26 steps
	assert.LessOrEqual(t, len(wf.Steps), 26)
	assert.Greater(t, len(wf.Steps), 2)
}
