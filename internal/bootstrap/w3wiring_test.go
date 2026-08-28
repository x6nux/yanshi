package bootstrap

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/tools"
)

// w3ConfigFile writes a minimal config that also names a real user skills
// directory, so skill_write's registration precondition holds.
//
// The directory matters: BuildSkillWriteTool returns nil without one, and a
// test that silently exercised the nil path would assert nothing about the
// tool it claims to cover.
func w3ConfigFile(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	skillsDir := filepath.Join(dir, "skills")
	require.NoError(t, os.MkdirAll(skillsDir, 0o755))
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(
		"server:\n  http_addr: 127.0.0.1:0\n"+
			"storage:\n  sqlite_path: \":memory:\"\n"+
			"skills:\n  user_dir: "+skillsDir+"\n"), 0o644))
	return path
}

// TestW3ToolsAreRegisteredAndAuthorized asserts that the Wave 3 tools are both
// present in the REAL assembled registry and permitted by the REAL default
// profile.
//
// Registration and authorization are checked together on purpose, because the
// two failure modes are opposite and each one hides from the other's gate:
//
//   - Registered but not allowed: the model can see the tool and every call is
//     refused. GOV5 is silent — it checks that allowed names are registered,
//     not the converse.
//   - Allowed but not registered: a phantom name, refused at runtime by
//     toolreg.Check (S8). GOV5 catches this one, but only for the boot shape it
//     happens to build.
//
// Every tool named here shipped complete and fully unit-tested with ZERO
// composition-root callers, which no test inside the owning package can
// detect. This test is the one that can.
//
// acp_delegate is deliberately asserted as registered-but-NOT-allowed: it runs
// a third-party binary that executes code of its own choosing, and a glob miss
// yields Prompt (ask the user on WS, fail closed on SSE) rather than a silent
// grant. Pinning that here keeps a later "tidy up the allow list" edit from
// quietly upgrading the highest-capability tool in the registry.
func TestW3ToolsAreRegisteredAndAuthorized(t *testing.T) {
	app, err := Build(Options{ConfigPath: w3ConfigFile(t), FakeModel: true})
	require.NoError(t, err)
	defer app.Shutdown(context.Background())

	registered := make(map[string]bool, len(app.ToolNames))
	for _, n := range app.ToolNames {
		registered[n] = true
	}

	profile := extendProfileWithConditionalTools(DefaultOrchestratorProfile(), app.ToolNames)
	g := guard.New()

	for _, tc := range []struct {
		name        string
		wantAllowed bool
		why         string
	}{
		{"background_list", true, "reads this process's own offload registry"},
		{"background_result", true, "reads one offloaded run; the id is useless without it"},
		{"background_cancel", true, "only takes capability away"},
		{"milestone_set", true, "labels the model's own work for compaction"},
		{"skill_write", true, "conditional: registered because user_dir is set"},
		{"acp_delegate", false, "third-party code execution must stay a prompt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.True(t, registered[tc.name],
				"%s is not in the assembled registry: it was built but never wired into "+
					"allTools, so toolreg.Check (S8) refuses every call at runtime (%s)", tc.name, tc.why)

			d := g.Check(profile, guard.Action{Tool: tc.name})
			if tc.wantAllowed {
				require.Equal(t, guard.Allow, d.Verdict,
					"%s is registered but the default profile does not allow it, so the "+
						"model sees a tool whose every call is refused (%s)", tc.name, tc.why)
				return
			}
			require.NotEqual(t, guard.Allow, d.Verdict,
				"%s must NOT be allowed by default (%s)", tc.name, tc.why)
			require.True(t, d.Promptable,
				"%s should be a promptable miss, not a structural HardDeny", tc.name)
		})
	}

	require.NotNil(t, app.Background,
		"App.Background is nil: the background_* tools read the manager from the turn "+
			"context, so without it on orchestrator.Config every offload query fails")
}

// ledger: A2/W-A-02#4 真实装配出的 App 其 orchestrator 已绑定 Redactor
func TestW3RedactorReachesToolResults(t *testing.T) {
	app, err := Build(Options{ConfigPath: w3ConfigFile(t), FakeModel: true})
	require.NoError(t, err)
	defer app.Shutdown(context.Background())

	require.NotNil(t, app.Redactor,
		"the process-wide redactor must exist for W-A-02 to have anything to bind")

	ctx := app.Orch.BindExecutionContextForTest(context.Background(), "")
	_, ok := tools.RedactorFromContext(ctx)
	require.True(t, ok,
		"bindExecutionContext did not bind the redactor: every tool result still reaches the provider unredacted")
}
