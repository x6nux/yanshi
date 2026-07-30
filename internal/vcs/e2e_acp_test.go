//go:build e2e_real

// Flagged real-CLI E2E for the ACP<->VCS integration (task V23). NOT part of the
// default suite — run with:
//
//	YANSHI_E2E=1 go test -tags=e2e_real -run TestE2EACP -timeout 300s ./internal/vcs/
//
// It spawns a real coding agent (codex preferred, claudecode fallback) via ACP
// inside an autoVCS worktree, prompts it to create a file, and asserts the edit
// was auto-tracked into the worktree changeset, then committed and fast-forward
// merged to main so the file lands on main's tree. Requires the agent CLIs +
// auth. The build tag keeps this out of the default build/vet/test run.
package vcs

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/x6nux/yanshi/internal/acp"
	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/store"
)

// workProfile builds a permissive guard profile scoped to the worktree's working
// dir: fs read/write under wtPath, fs.*/shell.* tools, a shell allowlist, and net
// off. It mirrors internal/acp/e2e_real_test.go's workProfile (copied here because
// that helper lives in package acp).
func workProfile(t *testing.T, wtPath string) guard.PermissionProfile {
	t.Helper()
	wt := strings.ReplaceAll(filepath.Join(wtPath, "**"), string(filepath.Separator), "/")
	return guard.PermissionProfile{
		FS:    guard.FSPerm{Read: []string{wt}, Write: []string{wt}},
		Tools: guard.ToolsPerm{Allow: []string{"fs.*", "shell.*"}},
		Shell: guard.ShellPerm{Policy: "allowlist", Patterns: []string{"go *", "git *", "npm *", "python *", "echo *", "cat *", "ls *"}},
		Net:   guard.NetPerm{Allow: false},
	}
}

// TestE2EACP_AgentEditsTrackedAndMerged proves the full ACP->VCS auto-track flow
// against a real agent CLI: an agent spawned in an autoVCS worktree creates a
// file, the diff-application mechanism records the edit into the worktree
// changeset via the Recorder callback, CommitWorktree + MergeToMain land it on
// main, and the blob content is readable from main's tree.
func TestE2EACP_AgentEditsTrackedAndMerged(t *testing.T) {
	if os.Getenv("YANSHI_E2E") == "" {
		t.Skip("set YANSHI_E2E=1 to run real-CLI E2E")
	}
	// Pick an agent that's installed (codex preferred; fall back to claudecode).
	agent := "codex"
	if _, err := exec.LookPath("codex"); err != nil {
		if _, err := exec.LookPath("claude"); err == nil {
			agent = "claudecode"
		} else {
			t.Skip("no codex/claudecode CLI installed")
		}
	}

	// 1. temp repo root + InitRepo + AddWorktree (branched from main_head).
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "seed.txt"), []byte("seed"), 0o644))
	st, err := store.Open(filepath.Join(root, "yanshi.db"))
	require.NoError(t, err)
	defer st.Close()
	v := New(st, filepath.Join(root, "wts"))
	repoID, err := v.InitRepo(root)
	require.NoError(t, err)
	wt, err := v.AddWorktree(repoID, []string{agent})
	require.NoError(t, err)
	t.Logf("worktree: %s (agent=%s)", wt.Path, agent)

	// 2. spawn the ACP agent in the WORKTREE dir with VCS tracking bound. The
	//    Recorder callback bridges agent diffs -> RecordEditWorktree (resolved
	//    against wt.Path per the V18 path-resolution fix).
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	spawned, err := acp.Spawn(ctx, acp.SpawnOptions{
		Agent:      agent,
		Cwd:        wt.Path,
		Policy:     acp.NewGuardPolicy(workProfile(t, wt.Path)),
		WorktreeID: wt.ID,
		Recorder: func(wtID, a, absPath string, content []byte) error {
			ag := a
			if ag == "" {
				ag = agent
			}
			return v.RecordEditWorktree(wtID, ag, absPath, content)
		},
	})
	if err != nil {
		t.Fatalf("spawn %q: %v", agent, err)
	}
	t.Logf("spawned %q ok (session=%s)", agent, spawned.SessionID)
	defer func() {
		spawned.Client.Close()
		spawned.Cmd.Process.Kill()
		spawned.Cmd.Wait()
	}()

	// 3. prompt the agent to create a file in the worktree (its Cwd = wt.Path).
	prompt := "Create a file named acpmark.txt in the current directory " +
		"containing exactly (one line, no quotes): marked-by-acp. " +
		"Use your file-write tool. Then stop."
	stop, err := spawned.Client.Prompt(ctx, spawned.SessionID, prompt, nil)
	require.NoError(t, err, "prompt failed")
	t.Logf("stopReason=%s", stop)

	// 4. assert auto-track recorded it to the worktree changeset. If pending is
	//    empty the agent didn't deliver its file-write as an ACP diff (a real
	//    adapter signal, not a test bug).
	pending := v.Uncommitted("worktree", wt.ID)
	bh, ok := pending["acpmark.txt"]
	if !ok {
		t.Fatalf("acpmark.txt not auto-tracked into worktree changeset; pending=%v", pending)
	}
	t.Logf("auto-tracked acpmark.txt (blob=%s); pending=%v", bh, pending)

	// 5. commit the worktree + merge to main (fast-forward expected: main hasn't
	//    moved since the branch, so there can be no conflict).
	cid, err := v.CommitWorktree(wt.ID, agent, "acp agent created acpmark.txt")
	require.NoError(t, err, "commit worktree")
	require.NotEmpty(t, cid)
	mergeCid, conflicts, err := v.MergeToMain(wt.ID, agent, false)
	require.NoError(t, err, "merge to main")
	require.Empty(t, conflicts, "expected a clean FF merge (main unmoved)")

	// 6. the file is now on main's tree with the agent-written content.
	mainTree := v.commitTree(mergeCid)
	gotH, ok := mainTree["acpmark.txt"]
	require.True(t, ok, "acpmark.txt on main after merge")
	content, err := v.getBlob(gotH)
	require.NoError(t, err)
	require.True(t, strings.Contains(string(content), "marked-by-acp"),
		"acpmark.txt content on main = %q", string(content))
	t.Logf("SUCCESS: agent edit auto-tracked + committed + merged to main (commit %s)", mergeCid)
}
