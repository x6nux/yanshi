package bootstrap_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/bootstrap"
)

// TestDefaultOrchestratorProfileIsStable proves the factory-default profile
// is reachable independently of config, which is what lets GOV5 compare the
// shipped allow list against the shipped tool registry.
func TestDefaultOrchestratorProfileIsStable(t *testing.T) {
	p := bootstrap.DefaultOrchestratorProfile()
	require.NotEmpty(t, p.Tools.Allow, "default profile must name concrete tools, not fail open")
	require.True(t, p.Net.Allow, "default profile allows net (see bootstrap.go comment)")
}

// TestAppExposesToolNames proves a built App reports the tool names actually
// registered with the orchestrator.
func TestAppExposesToolNames(t *testing.T) {
	app := buildMinimalApp(t) // helper from bootstrap_test.go:40
	require.NotEmpty(t, app.ToolNames, "App.ToolNames must list the registered tools")
	require.Contains(t, app.ToolNames, "fs_read", "fs_read is always registered")
}
