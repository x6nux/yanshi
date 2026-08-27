package acp

import (
	"path/filepath"
	"strings"

	"github.com/x6nux/yanshi/internal/guard"
)

// VCSMCPConfig describes the yanshi-vcs MCP stdio server that is delivered to a
// spawned ACP agent so it receives the vcs_* tools.
//
// It lives here rather than at each call site because there are now two of
// them — the goal loop's worker and the model-facing acp_delegate tool — and
// the map is a WIRE SHAPE, not a convenience: the environment variable names
// are read back by `yanshi vcs-mcp` in another process. A second hand-written
// copy would go stale silently, with the only symptom being an agent whose
// vcs_* tools bind to the wrong worktree.
type VCSMCPConfig struct {
	// Binary is the yanshi executable the ACP adapter spawns. Callers pass
	// os.Args[0]; it is a field rather than a package-level read so a test can
	// assert the descriptor without depending on the test binary's own name.
	Binary string
	// DBPath is the SQLite store path (cfg.Storage.SQLitePath).
	DBPath string
	// RepoID is the autoVCS repo the worktree belongs to.
	RepoID string
	// WorktreeID binds every vcs_* call to one worktree's changeset.
	WorktreeID string
	// Agent is the acting agent id, used for commit authorship.
	Agent string
	// WorktreeDir is the expanded worktree working dir (cfg.vcs.worktree_dir).
	WorktreeDir string
}

// VCSMCPCommand renders the config as the mcpServers entry Spawn sends in
// session/new (see SpawnOptions.MCPCommand for the shape adapters expect).
func (c VCSMCPConfig) VCSMCPCommand() map[string]any {
	return map[string]any{
		"command": c.Binary,
		"args":    []string{"vcs-mcp"},
		"env": map[string]string{
			"YANSHI_DB_PATH":      c.DBPath,
			"YANSHI_REPO_ID":      c.RepoID,
			"YANSHI_WT_ID":        c.WorktreeID,
			"YANSHI_AGENT":        c.Agent,
			"YANSHI_WORKTREE_DIR": c.WorktreeDir,
		},
	}
}

// PathToGlob converts a directory path to a glob matching everything under it,
// using forward slashes because that is what the guard's matcher expects on
// every platform.
func PathToGlob(dir string) string {
	return strings.ReplaceAll(filepath.Join(dir, "**"), "\\", "/")
}

// WorktreeScopedProfile returns the fail-closed profile handed to an ACP agent
// running inside an isolated worktree: it may read and write only within that
// worktree, use the fs/shell tool families, run the common dev commands, and
// has no network access.
//
// The tool names are DOTTED (fs.read, shell.run) rather than the underscore
// names used by yanshi's own registry. That is not a typo and not a thing to
// unify: these are the pseudo-actions GuardPolicy synthesises from the ACP
// protocol's tool-call KINDS (read/edit/execute/fetch), which is a different
// vocabulary from the one the model calls locally. Handing an ACP agent the
// caller's own Tools.Allow would deny every operation, because nothing in it
// ever matches "fs.read".
//
// Exported so both delegation paths — the goal loop's worker and the
// model-facing acp_delegate tool — get the same posture. A second copy would
// be a second security policy, and only one of them would get reviewed.
func WorktreeScopedProfile(worktree string) guard.PermissionProfile {
	wtGlob := PathToGlob(worktree)
	return guard.PermissionProfile{
		FS:    guard.FSPerm{Read: []string{wtGlob}, Write: []string{wtGlob}},
		Tools: guard.ToolsPerm{Allow: []string{"fs.*", "shell.*"}},
		Shell: guard.ShellPerm{
			Policy:   "allowlist",
			Patterns: []string{"go *", "git *", "npm *", "python *"},
		},
		Net: guard.NetPerm{Allow: false},
	}
}
