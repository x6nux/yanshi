package bootstrap

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/config"
	"github.com/x6nux/yanshi/internal/mcp"
)

// TestMCPOAuthReachesRuntimeFromYAML proves an operator-written oauth block
// arrives intact at mcp.ServerConfig.
//
// internal/mcp.ServerConfig has carried an OAuth field since T10 and nothing
// projected onto it: the authorization_code grant was reachable from Go and
// from no YAML any operator could write, so `yanshi auth mcp-login` refused
// every server with "has no oauth block" no matter what the config said. Unit
// tests inside internal/mcp all construct ServerConfig directly, so every one
// of them passed against that state.
//
// The test therefore starts at the YAML rather than at the struct — a test
// that built a config.MCPOAuthConfig in Go would re-create the same blind spot
// one layer up, since the missing piece was the yaml tag, not the projection.
func TestMCPOAuthReachesRuntimeFromYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(
		"server:\n  http_addr: 127.0.0.1:0\n"+
			"storage:\n  sqlite_path: \":memory:\"\n"+
			"mcp:\n"+
			"  servers:\n"+
			"    remote:\n"+
			"      enabled: false\n"+
			"      transport: http\n"+
			"      url: https://mcp.example.com\n"+
			"      oauth:\n"+
			"        grant: authorization_code\n"+
			"        token_url: https://auth.example.com/token\n"+
			"        authorization_url: https://auth.example.com/authorize\n"+
			"        client_id: cid-123\n"+
			"        client_secret: shhh\n"+
			"        scopes: [read, write]\n"), 0o644))

	cfg, err := config.Load(path)
	require.NoError(t, err)

	sc := cfg.MCP.Servers["remote"]
	require.NotNil(t, sc, "server block did not load")
	require.NotNil(t, sc.OAuth,
		"the oauth block did not deserialize: config.MCPServerConfig is missing the "+
			"yaml tag, so no operator-written oauth can ever reach the runtime")

	got := mcpOAuthFromConfig(sc.OAuth)
	require.NotNil(t, got)
	require.Equal(t, "https://auth.example.com/token", got.TokenURL)
	require.Equal(t, "https://auth.example.com/authorize", got.AuthorizationURL)
	require.Equal(t, "cid-123", got.ClientID)
	require.Equal(t, "shhh", got.ClientSecret)
	require.Equal(t, []string{"read", "write"}, got.Scopes)

	// The grant must survive verbatim: NormalizeGrant owns the ""-means-
	// client_credentials aliasing, and normalising early here would silently
	// change what a pre-T10 config means.
	require.Equal(t, mcp.GrantAuthorizationCode, got.Grant)
	grant, valid := mcp.NormalizeGrant(got.Grant)
	require.True(t, valid)
	require.Equal(t, mcp.GrantAuthorizationCode, grant)

	// A server with no oauth block stays nil rather than becoming a zero-valued
	// block, which TokenStoreFor would otherwise treat as "configured".
	require.Nil(t, mcpOAuthFromConfig(nil))
}

// TestBuildMCPManagerProjectsOAuth pins the projection at the composition root
// itself, so removing the OAuth line from buildMCPManager reddens even though
// mcpOAuthFromConfig keeps its own unit test passing.
func TestBuildMCPManagerProjectsOAuth(t *testing.T) {
	cfg := &config.Config{MCP: config.MCPConfig{
		Servers: map[string]*config.MCPServerConfig{
			"remote": {
				// Disabled: StartAll must not try to reach the network here.
				Enabled: false, Transport: "http", URL: "https://mcp.example.com",
				OAuth: &config.MCPOAuthConfig{
					Grant: mcp.GrantAuthorizationCode, TokenURL: "https://auth.example.com/token",
					AuthorizationURL: "https://auth.example.com/authorize", ClientID: "cid-123",
				},
			},
		},
	}}
	mgr := buildMCPManager(cfg, nil)
	require.NotNil(t, mgr)
	defer mgr.Shutdown()

	authURL, clientID, _, err := mgr.OAuthEndpoints("remote")
	require.NoError(t, err,
		"buildMCPManager dropped the oauth block, so the running agent sees no grant")
	require.Equal(t, "https://auth.example.com/authorize", authURL)
	require.Equal(t, "cid-123", clientID)
}
