package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/x6nux/yanshi/internal/mcp"
)

// MCP OAuth login (T10), the interactive half of internal/mcp's
// authorization_code grant.
//
// It is a verb under `yanshi auth` rather than a new top-level subcommand
// because it is the same thing `auth device` is — establish a credential a
// human has to approve in a browser — and the two would otherwise diverge in
// their flag names, their exit codes and their refusal messages.

// MCPLoginTimeout bounds the wait for the browser redirect.
//
// Generous because the user may have to log in, pick an account and clear an
// MFA prompt; bounded because a CLI that waits forever on a tab the user closed
// is a process the operator has to hunt down and kill.
const MCPLoginTimeout = 5 * time.Minute

// MCPLoginOptions configures RunMCPLogin.
type MCPLoginOptions struct {
	// Server is the MCP server name from config.mcp.servers.
	Server string
	// Manager supplies the endpoints, the client id and the token store. It is
	// the RUNNING configuration rather than a re-read of the file, so a login
	// cannot succeed against endpoints the manager is not using.
	Manager *mcp.Manager
	// Out receives the URL and progress. Required in practice: the URL is the
	// only way the user reaches the browser.
	Out io.Writer
	// Timeout overrides MCPLoginTimeout. Zero uses the default.
	Timeout time.Duration
}

// RunMCPLogin performs the authorization_code + PKCE flow for one MCP server
// and stores the resulting tokens.
//
// The URL is PRINTED rather than opened. Shelling out to `open`/`xdg-open`
// works on a developer laptop and fails silently on the headless box where
// yanshi most often runs as a daemon — and a flow that has silently opened
// nothing looks identical to one waiting for a slow browser. Printing always
// works, and a user on a laptop can click it.
func RunMCPLogin(ctx context.Context, opts MCPLoginOptions) error {
	if opts.Manager == nil {
		return errors.New("mcp login: no MCP manager is available")
	}
	out := opts.Out
	if out == nil {
		out = io.Discard
	}
	server := strings.TrimSpace(opts.Server)
	if server == "" {
		return fmt.Errorf("mcp login: a server name is required%s", mcpLoginCandidates(opts.Manager))
	}
	// The GRANT is checked before the endpoints, so a client-credentials
	// server is told it has no interactive login rather than being told its
	// authorization_url is missing — which is true but sends the operator to
	// add a URL for a flow that server does not use.
	source, err := opts.Manager.TokenStoreFor(server)
	if err != nil {
		return err
	}
	authURL, clientID, scopes, err := opts.Manager.OAuthEndpoints(server)
	if err != nil {
		return err
	}

	pkce, err := mcp.NewPKCE()
	if err != nil {
		return err
	}
	state, err := mcp.NewOAuthState()
	if err != nil {
		return err
	}
	cb, err := mcp.StartLoopbackCallback(state)
	if err != nil {
		return err
	}
	defer cb.Close()

	// The redirect URI is built from the callback's actual bound port, and the
	// same string is sent on both legs. Providers compare it literally, and a
	// mismatch surfaces as an opaque invalid_grant.
	redirectURI := cb.RedirectURI()
	browserURL, err := mcp.AuthorizationURL(authURL, clientID, redirectURI, state, pkce.Challenge, scopes)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Open this URL to authorize yanshi for MCP server %q:\n\n  %s\n\nwaiting for the redirect...\n",
		server, browserURL)

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = MCPLoginTimeout
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	code, err := cb.Wait(waitCtx)
	if err != nil {
		return err
	}

	tok, err := source.ExchangeAuthorizationCode(ctx, code, redirectURI, pkce.Verifier)
	if err != nil {
		return err
	}
	if err := source.SaveTokens(tok); err != nil {
		return err
	}
	fmt.Fprintf(out, "authorized %s\n", server)
	return nil
}

// mcpLoginCandidates renders the valid server names as a clause, so a bare
// invocation names the choices instead of only saying one is required.
func mcpLoginCandidates(m *mcp.Manager) string {
	names := m.AuthCodeServers()
	if len(names) == 0 {
		return "; no configured MCP server uses the authorization_code grant"
	}
	return "; servers using authorization_code: " + strings.Join(names, ", ")
}

// RunMCPLogout deletes the stored tokens for one MCP server.
//
// It type-asserts for the deleter rather than putting Delete on TokenStore,
// which keeps the refresh path physically unable to reach a delete — a
// transient network failure that logged the user out would be a spectacularly
// bad bug, and an interface method is an invitation to write it.
func RunMCPLogout(server string, store mcp.TokenStore, out io.Writer) error {
	if out == nil {
		out = io.Discard
	}
	if strings.TrimSpace(server) == "" {
		return errors.New("mcp logout: a server name is required")
	}
	deleter, ok := store.(mcp.TokenDeleter)
	if !ok {
		return errors.New("mcp logout: the configured credential store cannot delete entries")
	}
	if err := deleter.DeleteTokens(server); err != nil {
		return fmt.Errorf("mcp logout: %w", err)
	}
	fmt.Fprintf(out, "removed the stored MCP login for %s\n", server)
	return nil
}
