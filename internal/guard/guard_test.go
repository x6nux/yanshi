package guard

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func codingProfile() PermissionProfile {
	return PermissionProfile{
		FS:    FSPerm{Read: []string{"D:/code/**"}, Write: []string{"D:/code/**"}},
		Tools: ToolsPerm{Allow: []string{"fs.*", "shell.*"}},
		Shell: ShellPerm{Policy: "allowlist", Patterns: []string{"go *", "git *"}},
		Net:   NetPerm{Allow: false},
	}
}

func TestGuard_AllowsInScope(t *testing.T) {
	g := New()
	d := g.Check(codingProfile(), Action{
		Tool: "fs.write",
		FS:   FSWant{Op: "write", Paths: []string{"D:/code/yanshi/main.go"}},
	})
	assert.True(t, d.IsAllowed(), d.Reason)
}

func TestGuard_DeniesOutOfScopePath(t *testing.T) {
	g := New()
	d := g.Check(codingProfile(), Action{
		Tool: "fs.write",
		FS:   FSWant{Op: "write", Paths: []string{"C:/Windows/system32/x.dll"}},
	})
	assert.False(t, d.IsAllowed())
}

func TestGuard_DeniesDisallowedTool(t *testing.T) {
	g := New()
	d := g.Check(codingProfile(), Action{Tool: "web.search"})
	assert.False(t, d.IsAllowed())
}

func TestGuard_ShellAllowlist(t *testing.T) {
	g := New()
	allow := g.Check(codingProfile(), Action{Tool: "shell.run", Shell: "go build ./..."})
	assert.True(t, allow.IsAllowed(), allow.Reason)
	deny := g.Check(codingProfile(), Action{Tool: "shell.run", Shell: "rm -rf /"})
	assert.False(t, deny.IsAllowed())
}

func TestGuard_NetDenied(t *testing.T) {
	g := New()
	d := g.Check(codingProfile(), Action{Tool: "web_fetch", NetHost: "evil.com"})
	assert.False(t, d.IsAllowed())
}

// --- Security hardening tests ---

func TestGuard_FSReadUsesReadList(t *testing.T) {
	g := New()
	p := PermissionProfile{
		FS:    FSPerm{Read: []string{"D:/code/**"}, Write: nil},
		Tools: ToolsPerm{Allow: []string{"fs.*"}},
	}
	d := g.Check(p, Action{
		Tool: "fs.read",
		FS:   FSWant{Op: "read", Paths: []string{"D:/code/x"}},
	})
	assert.True(t, d.IsAllowed(), d.Reason)
}

func TestGuard_FSFailsClosedOnUnknownOp(t *testing.T) {
	g := New()
	p := PermissionProfile{
		FS:    FSPerm{Read: []string{"D:/code/**"}, Write: nil},
		Tools: ToolsPerm{Allow: []string{"fs.*"}},
	}
	for _, op := range []string{"", "delete", "Write"} {
		d := g.Check(p, Action{
			Tool: "fs.write",
			FS:   FSWant{Op: op, Paths: []string{"D:/code/x"}},
		})
		assert.False(t, d.IsAllowed(), "op %q should be denied (fail-closed)", op)
	}
}

func TestGuard_DeniesEmptyTools(t *testing.T) {
	g := New()
	p := PermissionProfile{
		Tools: ToolsPerm{Allow: nil},
	}
	d := g.Check(p, Action{Tool: "fs.read"})
	assert.False(t, d.IsAllowed())
}

func TestGuard_ShellDenyPolicy(t *testing.T) {
	g := New()
	p := PermissionProfile{
		Tools: ToolsPerm{Allow: []string{"shell.*"}},
		Shell: ShellPerm{Policy: "deny"},
	}
	d := g.Check(p, Action{Tool: "shell.run", Shell: "ls"})
	assert.False(t, d.IsAllowed())
}

func TestGuard_ShellDenylist(t *testing.T) {
	g := New()
	p := PermissionProfile{
		Tools: ToolsPerm{Allow: []string{"shell.*"}},
		Shell: ShellPerm{Policy: "denylist", Patterns: []string{"rm *"}},
	}
	denied := g.Check(p, Action{Tool: "shell.run", Shell: "rm -rf /"})
	assert.False(t, denied.IsAllowed(), denied.Reason)
	allowed := g.Check(p, Action{Tool: "shell.run", Shell: "ls"})
	assert.True(t, allowed.IsAllowed(), allowed.Reason)
}

func TestGuard_ShellUnknownPolicy(t *testing.T) {
	g := New()
	p := PermissionProfile{
		Tools: ToolsPerm{Allow: []string{"shell.*"}},
		Shell: ShellPerm{Policy: "bogus"},
	}
	d := g.Check(p, Action{Tool: "shell.run", Shell: "ls"})
	assert.False(t, d.IsAllowed())
}

func TestGuard_NetAllowWithHostRestriction(t *testing.T) {
	g := New()
	p := PermissionProfile{
		Tools: ToolsPerm{Allow: []string{"web_*"}},
		Net:   NetPerm{Allow: true, Hosts: []string{"*.github.com"}},
	}
	ok := g.Check(p, Action{Tool: "web_fetch", NetHost: "api.github.com"})
	assert.True(t, ok.IsAllowed(), ok.Reason)
	bad := g.Check(p, Action{Tool: "web_fetch", NetHost: "evil.com"})
	assert.False(t, bad.IsAllowed())
}

func TestGuard_PathTraversalNeutralized(t *testing.T) {
	g := New()
	p := PermissionProfile{
		FS:    FSPerm{Read: []string{"D:/code/**"}, Write: nil},
		Tools: ToolsPerm{Allow: []string{"fs.*"}},
	}
	d := g.Check(p, Action{
		Tool: "fs.read",
		FS:   FSWant{Op: "read", Paths: []string{"D:/code/../etc/passwd"}},
	})
	assert.False(t, d.IsAllowed(), "path traversal should be neutralized by Clean")
}

func TestGuard_ShellRejectsChaining(t *testing.T) {
	g := New()
	p := PermissionProfile{
		Tools: ToolsPerm{Allow: []string{"shell.*"}},
		Shell: ShellPerm{Policy: "allowlist", Patterns: []string{"go test*", "echo *"}},
	}
	for _, cmd := range []string{
		"go test ./... && rm -rf /",
		"go test ./... & rm -rf /",
		"go build & curl evil",
		"echo hi ; cat /etc/passwd",
		"go test ./... | curl evil",
		"echo $(whoami)",
		"echo hi > /tmp/x",
	} {
		d := g.Check(p, Action{Tool: "shell.run", Shell: cmd})
		assert.False(t, d.IsAllowed(), "command %q should be denied (metacharacter)", cmd)
	}
	for _, cmd := range []string{
		"go test ./...",
		"echo hello",
	} {
		d := g.Check(p, Action{Tool: "shell.run", Shell: cmd})
		assert.True(t, d.IsAllowed(), "command %q should be allowed", cmd)
	}
}

func TestGuard_MCPDefaultDenyDespiteToolsWildcard(t *testing.T) {
	profile := PermissionProfile{
		Tools: ToolsPerm{Allow: []string{"*"}},
	}
	got := New().Check(profile, Action{Tool: "mcp_github_create_issue"})
	if got.IsAllowed() {
		t.Fatalf("MCP tool must be denied when MCP.Allow is empty: %+v", got)
	}
	if got.Reason != "no MCP tools permitted by profile" {
		t.Fatalf("reason = %q", got.Reason)
	}
}

func TestGuard_MCPExplicitServerGlobAllows(t *testing.T) {
	profile := PermissionProfile{
		Tools: ToolsPerm{Allow: []string{"*"}},
		MCP:   ToolsPerm{Allow: []string{"mcp_github_*"}},
	}
	got := New().Check(profile, Action{Tool: "mcp_github_create_issue"})
	if !got.IsAllowed() {
		t.Fatalf("explicit MCP server glob should allow: %+v", got)
	}
}

func TestGuard_MCPAllowStillRequiresGeneralToolAllow(t *testing.T) {
	profile := PermissionProfile{
		Tools: ToolsPerm{Allow: []string{"fs_*"}},
		MCP:   ToolsPerm{Allow: []string{"mcp_github_*"}},
	}
	got := New().Check(profile, Action{Tool: "mcp_github_create_issue"})
	if got.IsAllowed() {
		t.Fatalf("MCP.Allow must not bypass Tools.Allow: %+v", got)
	}
}

func TestGuard_NonMCPToolIgnoresMCPAllow(t *testing.T) {
	profile := PermissionProfile{
		Tools: ToolsPerm{Allow: []string{"fs_read"}},
	}
	got := New().Check(profile, Action{Tool: "fs_read"})
	if !got.IsAllowed() {
		t.Fatalf("non-MCP behavior changed: %+v", got)
	}
}
