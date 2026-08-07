package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/approval"
	"github.com/x6nux/yanshi/internal/guard"
)

// TestScriptPathFromCommand covers which commands are "running a script" and
// which merely mention a filename. The negatives matter more than the
// positives: a false positive here attaches a content hash to something that
// is not the code being run, so the approval would be pinned to the wrong
// bytes.
func TestScriptPathFromCommand(t *testing.T) {
	cases := []struct {
		cmd  string
		want string
	}{
		// Interpreter + operand.
		{"sh install.sh", "install.sh"},
		{"bash /tmp/setup.sh", "/tmp/setup.sh"},
		{"python3 setup.py", "setup.py"},
		{"node build.js", "build.js"},
		{"ruby rake.rb", "rake.rb"},
		{"/usr/bin/python3 setup.py", "setup.py"},
		{"PYTHON3.EXE setup.py", "setup.py"},
		{"bash -x install.sh", "install.sh"},
		{"sh -- install.sh", "install.sh"},
		{`sh "my script.sh"`, "my script.sh"},

		// The program IS the script.
		{"./install.sh", "./install.sh"},
		{"/tmp/setup.sh", "/tmp/setup.sh"},
		{"../tools/gen.sh", "../tools/gen.sh"},

		// Interpreter with no script operand: -c/-m take code, not a file.
		{"python -c 'print(1)'", ""},
		{"python -m http.server", ""},
		{"node --eval 'console.log(1)'", ""},
		{"bash", ""},
		{"sh --", ""},

		// Not a script execution at all. `ls install.sh` names the file but
		// does not run it; hashing it would pin an approval to a file the
		// command only read.
		{"ls install.sh", ""},
		{"cat install.sh", ""},
		{"go build ./...", ""},
		{"make", ""},
		{"git status", ""},
		{"", ""},

		// powershell -File names the script in the flag's VALUE.
		{"powershell -File deploy.ps1", "deploy.ps1"},
	}
	for _, c := range cases {
		if got := scriptPathFromCommand(c.cmd); got != c.want {
			t.Errorf("scriptPathFromCommand(%q) = %q, want %q", c.cmd, got, c.want)
		}
	}
}

// TestHashScriptForCommand_ChangesWithContent is the property the whole
// feature rests on: the hash follows the file's BYTES, not its name. If it
// did not, an approval recorded for install.sh would keep admitting install.sh
// after someone rewrote it.
func TestHashScriptForCommand_ChangesWithContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "install.sh")
	require.NoError(t, os.WriteFile(path, []byte("echo hello\n"), 0o755))

	first := hashScriptForCommand("sh install.sh", dir)
	require.NotEmpty(t, first, "a readable script must hash")
	assert.True(t, strings.HasPrefix(first, "sha256:"), "hash must be labelled: %q", first)

	// Same bytes, second read: stable.
	assert.Equal(t, first, hashScriptForCommand("sh install.sh", dir),
		"hashing the same file twice must agree")

	// One byte different: different hash, so the old approval stops matching.
	require.NoError(t, os.WriteFile(path, []byte("echo hello!\n"), 0o755))
	second := hashScriptForCommand("sh install.sh", dir)
	require.NotEmpty(t, second)
	assert.NotEqual(t, first, second, "editing the script must change the hash")

	// Absolute path reaches the same file as the workdir-relative one.
	assert.Equal(t, second, hashScriptForCommand("sh "+path, ""),
		"absolute and workdir-relative paths must agree on the same file")
}

// TestHashScriptForCommand_UnreadableYieldsNoHash covers the fail-safe
// direction. Every one of these must produce "" so CommandRunsAScript is
// false and no approval rule is ever recorded — an unreadable script is
// re-asked, never remembered.
func TestHashScriptForCommand_UnreadableYieldsNoHash(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "adirectory"), 0o755))

	for _, cmd := range []string{
		"sh does-not-exist.sh",  // missing
		"sh adirectory",         // a directory, not a file
		"sh /nope/nothing/x.sh", // missing absolute
		"go build ./...",        // not a script execution
		"python -c 'print(1)'",  // no file operand
	} {
		assert.Empty(t, hashScriptForCommand(cmd, dir), "cmd %q must not hash", cmd)
		assert.False(t, CommandRunsAScript(cmd, dir), "cmd %q must not count as a script run", cmd)
	}

	// Oversized files are refused rather than read: a 4 MiB+ "script" is not
	// something a human reviewed, and hashing it would stall the turn.
	big := filepath.Join(dir, "big.sh")
	require.NoError(t, os.WriteFile(big, make([]byte, maxHashedScriptBytes+1), 0o755))
	assert.Empty(t, hashScriptForCommand("sh big.sh", dir), "oversized script must not hash")
}

// TestScopeFromAction_CarriesScriptHash proves the hash reaches the approval
// scope, which is the only place it can affect matching. The two scopes below
// differ ONLY in the script's contents, so a scope that dropped the hash would
// make them equal — and equal scopes mean the old approval admits the new
// script.
func TestScopeFromAction_CarriesScriptHash(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "install.sh")
	require.NoError(t, os.WriteFile(path, []byte("echo one\n"), 0o755))

	act := guard.Action{Tool: "shell_run", Shell: "sh install.sh", Workdir: dir}
	before, err := scopeFromAction(act)
	require.NoError(t, err)
	require.NotEmpty(t, before.ScriptHash, "a script execution must carry a hash")

	require.NoError(t, os.WriteFile(path, []byte("echo two\n"), 0o755))
	after, err := scopeFromAction(act)
	require.NoError(t, err)

	assert.Equal(t, before.Program, after.Program, "same program")
	assert.Equal(t, before.Prefix, after.Prefix, "same arguments")
	assert.NotEqual(t, before, after,
		"scopes must differ after the script changed; otherwise the old approval still matches")
	assert.NotEqual(t, before.ScriptHash, after.ScriptHash)
}

// TestScopeFromAction_NonScriptHasNoHash proves ordinary commands are
// unaffected. Their scope must stay exactly what it was, or every existing
// approval rule recorded before this change would stop matching.
func TestScopeFromAction_NonScriptHasNoHash(t *testing.T) {
	for _, shell := range []string{"go build ./...", "git status", "ls -la"} {
		scope, err := scopeFromAction(guard.Action{
			Tool: "shell_run", Shell: shell, Workdir: t.TempDir(),
		})
		require.NoError(t, err)
		assert.Empty(t, scope.ScriptHash, "cmd %q must not carry a hash", shell)
	}
}

// TestAuthorize_ScriptApprovalSurvivesUntilTheScriptChanges is the end-to-end
// property the feature exists for, driven through the real Authorize path and
// a real approval.Manager.
//
// Three phases, and the third is the one that matters: an approval recorded
// for a script must STOP applying the moment the script's bytes change. A
// cache keyed on the path alone would pass phases 1 and 2 and silently fail
// phase 3 — which is exactly the shape of a permanent backdoor, since anyone
// able to rewrite install.sh would inherit its approval.
func TestAuthorize_ScriptApprovalSurvivesUntilTheScriptChanges(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "install.sh")
	require.NoError(t, os.WriteFile(script, []byte("echo v1\n"), 0o755))

	prof := guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"shell_run"}},
		Shell: guard.ShellPerm{Policy: "allowlist"}, // empty list -> static deny -> callback
	}
	mgr, err := approval.New(&fakeApprovalKV{}, "proc-1", nil)
	require.NoError(t, err)

	var asks int
	ctx := WithProfile(context.Background(), prof)
	ctx = WithApprovalManager(ctx, mgr, "session-a")
	ctx = WithPermissionCallback(ctx, func(PermissionRequest) PermissionDecision {
		asks++
		return PermissionAllowPersistent // what auto mode returns for a cleared script
	})
	action := guard.Action{Tool: "shell_run", Shell: "sh install.sh", Workdir: dir}

	// Phase 1: first run asks and records.
	require.NoError(t, Authorize(ctx, action, `{}`))
	assert.Equal(t, 1, asks, "the first run must ask")

	// Phase 2: unchanged script re-runs without asking again.
	require.NoError(t, Authorize(ctx, action, `{}`))
	require.NoError(t, Authorize(ctx, action, `{}`))
	assert.Equal(t, 1, asks, "an unchanged script must not be re-asked")

	// Phase 3: one byte changes and the approval no longer applies.
	require.NoError(t, os.WriteFile(script, []byte("echo v2\n"), 0o755))
	require.NoError(t, Authorize(ctx, action, `{}`))
	assert.Equal(t, 2, asks, "a rewritten script must be asked about again")

	// And the new contents get their own approval, independent of the old one.
	require.NoError(t, Authorize(ctx, action, `{}`))
	assert.Equal(t, 2, asks, "the new contents are remembered in their own right")

	// Restoring the original bytes hits the ORIGINAL rule, which is still
	// there — proving the rules are keyed by content rather than replaced.
	require.NoError(t, os.WriteFile(script, []byte("echo v1\n"), 0o755))
	require.NoError(t, Authorize(ctx, action, `{}`))
	assert.Equal(t, 2, asks, "the original approval still matches the original bytes")
}

// TestAuthorize_UnreadableScriptIsNeverRemembered proves the fail-safe path
// end to end: when the script cannot be hashed there is nothing to pin an
// approval to, so every run must ask. A missing file that got remembered
// would mean the approval applied to whatever appears at that path later.
func TestAuthorize_UnreadableScriptIsNeverRemembered(t *testing.T) {
	dir := t.TempDir()
	prof := guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"shell_run"}},
		Shell: guard.ShellPerm{Policy: "allowlist"},
	}
	mgr, err := approval.New(&fakeApprovalKV{}, "proc-1", nil)
	require.NoError(t, err)

	var asks int
	ctx := WithProfile(context.Background(), prof)
	ctx = WithApprovalManager(ctx, mgr, "session-a")
	ctx = WithPermissionCallback(ctx, func(PermissionRequest) PermissionDecision {
		asks++
		return PermissionAllow // auto mode's answer when there is no script to pin
	})
	action := guard.Action{Tool: "shell_run", Shell: "sh missing.sh", Workdir: dir}

	require.NoError(t, Authorize(ctx, action, `{}`))
	require.NoError(t, Authorize(ctx, action, `{}`))
	assert.Equal(t, 2, asks, "a script that cannot be hashed must be asked about every time")
}
