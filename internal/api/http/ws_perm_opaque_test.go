package http

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/tools"
)

// ws_perm_opaque_test.go holds the mode-gate half of ADR-0020.
//
// guard grades a command whose payload nobody read as DestructionOpaque, a
// Prompt. That tier was absent from resolvePermissionMode's destructive switch,
// so yolo auto-approved the whole of it — "I could not read this command"
// followed by "approved automatically", which denies the tier its reason for
// existing. The family it covers is not exotic: `pkexec rm -rf /` (the shape
// W-B-B2-3 had just closed) and `GIT_SSH_COMMAND='rm -rf /' git fetch` both
// live here.
//
// The verdicts are asserted with their GRADE checked first. Without that, a
// command promoted to DestructionCatastrophic somewhere in guard would satisfy
// "yolo does not auto-approve it" for a reason that has nothing to do with this
// file, and the branch below could be deleted with the test still green.

// opaqueCommands are commands guard grades DestructionOpaque under the probe
// workdir. Each one is checked to still BE opaque before its mode verdict is
// asserted.
var opaqueCommands = []string{
	`python3 -c "print(1)"`,
	`pkexec rm -rf /`,
	`GIT_SSH_COMMAND='rm -rf /' git fetch origin`,
	`nu --commands "rm -rf /"`,
	`ssh -o ProxyCommand='rm -rf /' host`,
}

const opaqueWorkdir = "/work/project"

// TestYoloAsksAboutAPayloadNobodyRead pins that yolo no longer auto-resolves
// DestructionOpaque: it returns (deny, NOT resolved), which is "hand it back to
// the callback for an explicit answer" — the same route ForcePrompt takes — and
// not (deny, resolved), which would be a refusal yolo could not appeal.
func TestYoloAsksAboutAPayloadNobodyRead(t *testing.T) {
	for _, cmd := range opaqueCommands {
		cmd := cmd
		t.Run(cmd, func(t *testing.T) {
			require.Equal(t, guard.DestructionOpaque,
				guard.ClassifyDestruction(cmd, opaqueWorkdir),
				"precondition: this row must still be the OPAQUE tier, otherwise the "+
					"verdict below says nothing about resolvePermissionMode")

			cs := &connSession{perm: &permModeState{}}
			cs.perm.set(guard.ModeYOLO)
			d, resolved := resolvePermissionMode(context.Background(), cs, nil,
				tools.PermissionRequest{Tool: "shell_run", Shell: cmd, Workdir: opaqueWorkdir})

			assert.False(t, resolved,
				"yolo must not auto-resolve a command whose payload nobody read — "+
					"see ADR-0020 and the DestructionOpaque case in resolvePermissionMode")
			assert.Equal(t, tools.PermissionDeny, d)
		})
	}
}

// TestYoloStillAutoApprovesAReadableCommand is the reverse sample. Without it
// the assertion above is satisfied by a yolo that resolves nothing at all,
// which would be a different (and much larger) change than the one made.
func TestYoloStillAutoApprovesAReadableCommand(t *testing.T) {
	for _, cmd := range []string{`echo hi`, `rm -rf ./build`, `go test ./...`} {
		require.NotEqual(t, guard.DestructionOpaque,
			guard.ClassifyDestruction(cmd, opaqueWorkdir), "precondition for %q", cmd)
		cs := &connSession{perm: &permModeState{}}
		cs.perm.set(guard.ModeYOLO)
		d, resolved := resolvePermissionMode(context.Background(), cs, nil,
			tools.PermissionRequest{Tool: "shell_run", Shell: cmd, Workdir: opaqueWorkdir})
		assert.True(t, resolved, "yolo still auto-approves %q", cmd)
		assert.Equal(t, tools.PermissionAllow, d)
	}
}

// TestOpaqueIsFailClosedWithNoCallback is the SSE half. That transport binds a
// static profile and no permission callback, so nothing here can ask anybody:
// Authorize's rule 5 turns the Prompt into a DenyErr. The tier is therefore
// strictly stronger on SSE than on WS, and this pins it rather than leaving it
// to be re-derived from the callback table.
func TestOpaqueIsFailClosedWithNoCallback(t *testing.T) {
	prof := guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
		FS:    guard.FSPerm{Read: []string{"**"}, Write: []string{"**"}},
		Shell: guard.ShellPerm{Policy: "denylist"},
		Net:   guard.NetPerm{Allow: true},
	}
	ctx := tools.WithProfile(context.Background(), prof)
	for _, cmd := range opaqueCommands {
		require.Equal(t, guard.DestructionOpaque,
			guard.ClassifyDestruction(cmd, opaqueWorkdir), "precondition for %q", cmd)
		err := tools.Authorize(ctx,
			guard.Action{Tool: "shell_run", Shell: cmd, Workdir: opaqueWorkdir}, "")
		assert.Error(t, err, "no callback must fail closed for %q", cmd)
	}
}
