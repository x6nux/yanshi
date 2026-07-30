package tools

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPredefinedAgents_List(t *testing.T) {
	agents := ListPredefinedAgents()
	require.NotEmpty(t, agents, "should have at least one predefined agent")
	assert.Contains(t, agents, "analysis")
}

func TestPredefinedAgents_Get(t *testing.T) {
	def, ok := GetPredefinedAgent("analysis")
	require.True(t, ok)
	assert.Equal(t, "analysis", def.Name)
	assert.NotEmpty(t, def.Description)
	assert.NotEmpty(t, def.PromptTmpl)
}

func TestPredefinedAgents_GetNotFound(t *testing.T) {
	_, ok := GetPredefinedAgent("nonexistent")
	assert.False(t, ok)
}

func TestPredefinedAgents_WorkflowStructure(t *testing.T) {
	def, ok := GetPredefinedAgent("analysis")
	require.True(t, ok)
	require.NotNil(t, def.Workflow)
	require.GreaterOrEqual(t, len(def.Workflow.Steps), 4)

	// Check specific step IDs
	stepIDs := make([]string, len(def.Workflow.Steps))
	for i, s := range def.Workflow.Steps {
		stepIDs[i] = s.ID
	}
	assert.Contains(t, stepIDs, "A1")
	assert.Contains(t, stepIDs, "B1")
	assert.Contains(t, stepIDs, "B2")
	assert.Contains(t, stepIDs, "B3")
	assert.Contains(t, stepIDs, "C1")
}

func TestPredefinedAgents_PromptContainsTarget(t *testing.T) {
	def, ok := GetPredefinedAgent("analysis")
	require.True(t, ok)

	assert.Contains(t, def.PromptTmpl, "{{target}}")

	// Find the C1 step prompt for cross-checking.
	var c1Prompt string
	for _, s := range def.Workflow.Steps {
		if s.ID == "C1" {
			c1Prompt = s.Prompt
		}
	}

	// A1 (top-level) and B1/B2/B3 (fan-out) all need {{target}}.
	// C1 (synthesis) gets target context through {{A1.output}} etc.
	for _, s := range def.Workflow.Steps {
		if s.ID == "C1" {
			continue
		}
		assert.Contains(t, s.Prompt, "{{target}}",
			"step %s should have {{target}} placeholder", s.ID)
	}
	assert.NotContains(t, c1Prompt, "{{target}}", "C1 should not need direct {{target}}")
	assert.Contains(t, c1Prompt, "{{A1.output}}", "C1 should reference A1")
	assert.Contains(t, c1Prompt, "{{B1.output}}", "C1 should reference B1")
}
