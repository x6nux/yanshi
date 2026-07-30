package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/proto"
)

// TestCov_ParseCommand_BareSlash covers the empty-after-trim branch (bare "/"
// or whitespace-only).
func TestCov_ParseCommand_BareSlash(t *testing.T) {
	name, args := parseCommand("/")
	assert.Equal(t, "", name)
	assert.Nil(t, args)
}

// TestCov_UpdatePalette_MCPServersAndClamp covers the MCP-items append loop and
// the out-of-range paletteSel clamp.
func TestCov_UpdatePalette_MCPServersAndClamp(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m.paletteMCPServers = []proto.MCPServerStatus{{
		Name:   "srv",
		Status: "ready",
		Tools:  []proto.MCPToolBrief{{Name: "mcp_srv_read", Description: "read"}},
	}}
	m.paletteSel = 99 // out of range → clamped
	m.input.SetValue("/")
	m.updatePalette()
	assert.Greater(t, len(m.paletteItems), 0, "MCP group + tool items appended")
	assert.Equal(t, 0, m.paletteSel, "out-of-range sel clamped to 0")
}

// TestCov_PaletteBlock_MCPToolEmptyHelp covers the cmdMCPTool branch with an
// empty help string → "MCP tool" fallback.
func TestCov_PaletteBlock_MCPToolEmptyHelp(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m.paletteItems = []command{{name: "mcp_srv_x", kind: cmdMCPTool}} // help ""
	m.paletteSel = 0
	assert.Contains(t, m.paletteBlock(), "MCP tool")
}

// TestCov_PaletteMoveComplete_Empty covers the empty-palette no-op guards.
func TestCov_PaletteMoveComplete_Empty(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m.paletteMove(1)    // len 0 → return
	m.paletteComplete() // len 0 → return
	assert.Empty(t, m.paletteItems)
}

// TestCov_CmdPlan_PrePlanEmpty covers the prePlanMode=="" → Default branch.
func TestCov_CmdPlan_PrePlanEmpty(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m.permMode = "" // prePlanMode becomes "" → defaulted inside
	mm, _ := cmdPlan(m, nil)
	got := mm.(model)
	assert.Equal(t, guard.ModePlan, got.permMode)
	assert.Equal(t, guard.ModeDefault, got.prePlanMode)
}

// TestCov_CmdPlanOff_PrePlanEmpty covers the restore-to-Default branch when no
// prior mode was saved.
func TestCov_CmdPlanOff_PrePlanEmpty(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	mm, _ := cmdPlanOff(m, nil)
	got := mm.(model)
	assert.Equal(t, guard.ModeDefault, got.permMode)
}

// TestCov_CmdMode_NoArgsEmptyMode covers the no-args picker-open path with an
// empty permMode → cur defaults.
func TestCov_CmdMode_NoArgsEmptyMode(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m.permMode = ""
	mm, _ := cmdMode(m, nil)
	got := mm.(model)
	assert.Equal(t, "mode", got.pickerKind)
}

// TestCov_NewCmdHelpEntry_NilBundle covers the nil-bundle → defaultBundle fallback.
func TestCov_NewCmdHelpEntry_NilBundle(t *testing.T) {
	e := newCmdHelpEntry(nil, []command{{name: "help", help: "list commands"}})
	assert.Contains(t, e.render(80, newSpinner()), "help")
}

// TestCov_FeaturesRender_Enabled covers the enabled-state branch of a features
// table row.
func TestCov_FeaturesRender_Enabled(t *testing.T) {
	e := featuresEntry{rows: []proto.FeatureRow{
		{Key: "feat", Stage: "beta", Enabled: true, Owner: "team"},
	}}
	out := e.render(80, newSpinner())
	assert.Contains(t, out, "enabled")
}
