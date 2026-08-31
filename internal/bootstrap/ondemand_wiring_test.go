package bootstrap_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/bootstrap"
)

// buildAppWithOnDemand builds a real App (FakeModel:true) with the given
// tools.on_demand YAML fragment appended.
func buildAppWithOnDemand(t *testing.T, onDemandYAML string) *bootstrap.App {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	dbPath := toYAMLPath(filepath.Join(dir, "test.db"))
	yamlBytes := []byte(`
llm:
  providers:
    - name: literal
      kind: openai
      model: gpt-fake
      api_key: sk-fake
storage:
  sqlite_path: "` + dbPath + `"
` + onDemandYAML)
	require.NoError(t, os.WriteFile(cfgPath, yamlBytes, 0o644))
	app, err := bootstrap.Build(bootstrap.Options{ConfigPath: cfgPath, FakeModel: true})
	require.NoError(t, err)
	t.Cleanup(func() { app.Shutdown(context.Background()) })
	return app
}

// TestWFS11EscapeHatchRegistrationFollowsTheSwitch pins the composition-root
// half of on-demand loading: the escape-hatch tools are registered (and the
// factory default profile allows them) ONLY when tools.on_demand.enabled is
// set — off by default, both directions of the conditional.
func TestWFS11EscapeHatchRegistrationFollowsTheSwitch(t *testing.T) {
	t.Run("off by default: not registered", func(t *testing.T) {
		app := buildAppWithOnDemand(t, "")
		require.NotContains(t, app.ToolNames, "tools_list")
		require.NotContains(t, app.ToolNames, "tools_load")
	})

	t.Run("on: registered and allowed by the default profile", func(t *testing.T) {
		app := buildAppWithOnDemand(t, `
tools:
  on_demand:
    enabled: true
    max_visible: 8
`)
		require.Contains(t, app.ToolNames, "tools_list")
		require.Contains(t, app.ToolNames, "tools_load")
		// The EFFECTIVE profile (conditional names already extended in), read
		// off the really-built orchestrator — the same source GOV5's spirit
		// trusts rather than re-deriving the extension here.
		allow := app.Orch.ProfileForTest().Tools.Allow
		require.True(t, slices.Contains(allow, "tools_list") && slices.Contains(allow, "tools_load"),
			"the factory default profile must grant the hatch exactly when it is registered: %v", allow)
	})
}
