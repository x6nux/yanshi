package mcp

import (
	"strings"
	"testing"
)

// TestStdioServerEnvStripsInheritedCredentials pins what a stdio MCP server
// sees.
//
// An MCP server is a binary named by a config file and started before any of
// the guard's MCP dimension applies to it. It used to receive a raw
// os.Environ(), which on a developer's machine means every provider API key,
// cloud credential and VCS token they have exported.
func TestStdioServerEnvStripsInheritedCredentials(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-mcpprobe-0000000000000000")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "mcpprobeAWSsecret000000000000000")
	t.Setenv("YANSHI_MCP_ORDINARY", "mcpprobe-ordinary")

	got := strings.Join(stdioServerEnv(&ServerConfig{}), "\n")

	for _, leaked := range []string{"sk-mcpprobe-0000000000000000", "mcpprobeAWSsecret000000000000000"} {
		if strings.Contains(got, leaked) {
			t.Errorf("a stdio MCP server would receive %q", leaked)
		}
	}
	if !strings.Contains(got, "mcpprobe-ordinary") {
		t.Error("an ordinary variable was dropped: the scrub is truncating rather than filtering")
	}
	if !strings.Contains(got, "PATH=") {
		t.Error("PATH was removed, which breaks the server rather than containing it")
	}
}

// TestStdioServerEnvLetsConfiguredEnvWin pins the escape hatch and its
// direction.
//
// The layering order is the whole design: an operator who writes a token into
// mcp.servers.<name>.env has made a decision with a name attached, and it must
// survive. An inherited one has not, and must not. Asserting only the first
// half would pass for an implementation that skipped the scrub entirely.
func TestStdioServerEnvLetsConfiguredEnvWin(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghp_inheritedMustNotSurvive000000000000")

	got := stdioServerEnv(&ServerConfig{Env: map[string]string{
		"GITHUB_TOKEN": "ghp_declaredMustSurvive00000000000000",
	}})
	joined := strings.Join(got, "\n")

	if strings.Contains(joined, "ghp_inheritedMustNotSurvive000000000000") {
		t.Error("the inherited token survived; the scrub did not run before the config layer")
	}
	if !strings.Contains(joined, "ghp_declaredMustSurvive00000000000000") {
		t.Error("the operator's declared token did not reach the server")
	}
	// exec.Cmd keeps the LAST duplicate, so the declared entry must come after
	// anything the scrub left. Order, not mere presence, is what decides.
	inherited, declared := -1, -1
	for i, e := range got {
		if strings.HasPrefix(e, "GITHUB_TOKEN=") {
			if strings.Contains(e, "inherited") {
				inherited = i
			} else {
				declared = i
			}
		}
	}
	if declared < 0 {
		t.Fatal("no declared GITHUB_TOKEN entry at all")
	}
	if inherited >= 0 && inherited > declared {
		t.Error("the inherited entry comes after the declared one, so exec would prefer it")
	}
}
