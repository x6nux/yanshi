package guard

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/x6nux/yanshi/internal/execpolicy"
)

// TestCheckMCPTools_PromptWhenNoPatternMatch covers the MCP prompt path
// (guard.go:144): an mcp_ tool against a non-empty MCP.Allow whose patterns do
// not match. This is PROMPTABLE (an interactive user may approve a new MCP
// tool) — only an empty MCP.Allow is HardDeny.
func TestCheckMCPTools_PromptWhenNoPatternMatch(t *testing.T) {
	profile := PermissionProfile{
		Tools: ToolsPerm{Allow: []string{"*"}},
		MCP:   ToolsPerm{Allow: []string{"mcp_other_*"}},
	}
	d := New().Check(profile, Action{Tool: "mcp_github_create_issue"})
	assert.Equal(t, Prompt, d.Verdict, "unmatched MCP tool should be Prompt, got %#v", d)
	assert.True(t, d.Promptable, "unmatched MCP tool must remain Promptable")
	assert.Contains(t, d.Reason, "mcp_github_create_issue")
}

// TestCheckShell_ExecPolicyPrompt covers the execpolicy "prompt" outcome
// (guard.go:232-233): a Rule with Decision "prompt" surfaces as a Promptable
// Decision carrying the rule's ID/Justification.
func TestCheckShell_ExecPolicyPrompt(t *testing.T) {
	p := PermissionProfile{
		Tools: ToolsPerm{Allow: []string{"shell_run"}},
		Shell: ShellPerm{Rules: []execpolicy.Rule{
			{ID: "go-build", Program: "go", Prefix: []string{"build"}, Decision: "prompt", Justification: "build needs review"},
		}},
	}
	d := New().Check(p, Action{Tool: "shell_run", Shell: "go build ./..."})
	assert.Equal(t, Prompt, d.Verdict, "execpolicy prompt should map to Prompt")
	assert.True(t, d.Promptable)
	assert.Equal(t, "go-build", d.RuleID)
	assert.Equal(t, "build needs review", d.Justification)
}

// TestCheckNet_HardDenyWhenAllowedToolButNetClosed covers guard.go:265-267: a
// tool the profile permits, carrying a NetHost, against Net.Allow=false must
// HardDeny at the net dimension (not short-circuit at checkTools).
func TestCheckNet_HardDenyWhenAllowedToolButNetClosed(t *testing.T) {
	p := PermissionProfile{
		Tools: ToolsPerm{Allow: []string{"web_*"}},
		Net:   NetPerm{Allow: false},
	}
	d := New().Check(p, Action{Tool: "web_fetch", NetHost: "evil.com"})
	assert.Equal(t, HardDeny, d.Verdict)
	assert.False(t, d.Promptable)
	assert.Contains(t, d.Reason, "network access denied")
}

// TestCheckNet_AllowTrueNoHostRestriction covers guard.go:268-270: Net.Allow
// with an empty Hosts list permits any host (allow=true, no restriction).
func TestCheckNet_AllowTrueNoHostRestriction(t *testing.T) {
	p := PermissionProfile{
		Tools: ToolsPerm{Allow: []string{"web_*"}},
		Net:   NetPerm{Allow: true}, // no Hosts → unrestricted
	}
	d := New().Check(p, Action{Tool: "web_fetch", NetHost: "api.example.com"})
	assert.True(t, d.IsAllowed(), d.Reason)
	assert.Equal(t, Allow, d.Verdict)
}

// TestCycleMode_UnknownModeFallsBackToDefault covers mode.go:114: a value that
// is neither in cycleOrder nor ModePlan/"" falls back to cycleOrder[0].
func TestCycleMode_UnknownModeFallsBackToDefault(t *testing.T) {
	assert.Equal(t, ModeDefault, CycleMode("bogus-mode"))
}

// TestModeLabel_AllCases covers every ModeLabel arm (mode.go:133-142); only
// ModePlan was exercised before.
func TestModeLabel_AllCases(t *testing.T) {
	cases := map[PermissionMode]string{
		ModeDefault:    "default",
		ModeAllowEdits: "edits",
		ModeYOLO:       "YOLO",
		ModeAuto:       "auto",
		ModePlan:       "plan",
		"":             "default",
	}
	for mode, want := range cases {
		assert.Equal(t, want, ModeLabel(mode), "ModeLabel(%q)", mode)
	}
}

// TestRiskPrompt covers RiskPrompt (mode.go:151): it embeds the date, tool and
// args so the model can contextualize the risk rating.
func TestRiskPrompt(t *testing.T) {
	got := RiskPrompt("shell_run", "rm -rf /")
	assert.Contains(t, got, "shell_run")
	assert.Contains(t, got, "rm -rf /")
	assert.Contains(t, got, time.Now().Format("2006-01-02"))
	assert.Contains(t, got, "single integer 1-10")
}

// TestCheckModel_AllowsAnyModel covers profile.go:44-46: a profile with no
// model restrictions (empty Models) admits any model name.
func TestCheckModel_AllowsAnyModel(t *testing.T) {
	var p PermissionProfile // Subagent.Models is empty → AllowsAnyModel
	require.NoError(t, p.Subagent.CheckModel("literally-anything"))
}

// TestCheckReasoning_InvalidProfileCap covers profile.go:72-74: when the
// profile's MaxReasoning is not a recognized rank, ReasoningCap returns a value
// absent from reasoningRank and CheckReasoning rejects with a cap error.
func TestCheckReasoning_InvalidProfileCap(t *testing.T) {
	p := PermissionProfile{Subagent: SubagentPerm{MaxReasoning: "bogus"}}
	err := p.Subagent.CheckReasoning("low")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid profile reasoning cap")
}

// TestParseRiskScore_MultiDigitRunWithTrailingText covers a non-leading digit
// run (e.g. "Risk: 7.") — the trailing branch at mode.go:189-190 — keeping the
// score-extraction robust against model prose.
func TestParseRiskScore_MultiDigitRunWithTrailingText(t *testing.T) {
	n, ok := ParseRiskScore("I would rate this a 6 overall")
	require.True(t, ok)
	assert.Equal(t, 6, n)
}

// keep strings referenced for future expansion of these edge-case tests.
var _ = strings.Contains
