package mcp

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/x6nux/yanshi/internal/secrets"
)

// secretsTokenStore persists MCP OAuth tokens in the credential backend.
//
// Why the credential backend and not config.yaml: a refresh token is a
// long-lived bearer credential for the user's account at the MCP provider, and
// config.yaml is a hand-edited file that gets copied into dotfile repositories
// and pasted into issue reports. The whole reason authorization_code exists in
// this codebase is that the alternative was writing a long-lived token into
// that file by hand.
type secretsTokenStore struct {
	mgr *secrets.Manager
}

// mcpTokenService is the credential-backend "service" namespace MCP tokens live
// under. The account within it is the MCP server name, so one login per server
// is representable and `yanshi auth mcp-login` can replace one without touching
// the others.
const mcpTokenService = "yanshi-mcp-oauth"

// NewSecretsTokenStore wraps a secrets.Manager as a TokenStore.
//
// It returns an error for a Manager with no backend rather than a store that
// silently drops writes. A dropped write means the user completes a browser
// login, sees "authenticated", and is logged out again at the next process
// start with nothing anywhere explaining why.
func NewSecretsTokenStore(mgr *secrets.Manager) (TokenStore, error) {
	if mgr == nil || mgr.Store() == nil {
		return nil, fmt.Errorf("mcp oauth: no secrets backend is configured, so MCP tokens cannot be stored; " +
			"set secrets.backend in config.yaml")
	}
	return &secretsTokenStore{mgr: mgr}, nil
}

// LoadTokens reads the stored tokens for server.
//
// A missing entry is (false, nil) — the normal "not logged in yet" state. A
// stored value that will not parse IS an error: treating it as absent would
// send the operator through a browser flow whose result lands in the same
// unparseable slot.
func (s *secretsTokenStore) LoadTokens(server string) (StoredTokens, bool, error) {
	raw, err := s.mgr.Store().Get(mcpTokenService, server)
	if errors.Is(err, secrets.ErrSecretNotFound) {
		return StoredTokens{}, false, nil
	}
	if err != nil {
		return StoredTokens{}, false, fmt.Errorf("mcp oauth: read stored tokens: %w", err)
	}
	if raw == "" {
		return StoredTokens{}, false, nil
	}
	var tok StoredTokens
	if err := json.Unmarshal([]byte(raw), &tok); err != nil {
		return StoredTokens{}, false, fmt.Errorf(
			"mcp oauth: stored tokens for %q are corrupt; run `yanshi auth mcp-logout %s` and log in again", server, server)
	}
	return tok, true, nil
}

// SaveTokens replaces the stored tokens for server and registers both with the
// redactor, so a token that later appears in a log line, a WS frame or a SQLite
// write is masked. Registering at the WRITE is what makes the coverage
// automatic: every token this process holds passed through here.
func (s *secretsTokenStore) SaveTokens(server string, tok StoredTokens) error {
	data, err := json.Marshal(tok)
	if err != nil {
		return fmt.Errorf("mcp oauth: encode tokens: %w", err)
	}
	if r := s.mgr.Redactor(); r != nil {
		r.Register(tok.AccessToken)
		r.Register(tok.RefreshToken)
	}
	if err := s.mgr.Store().Set(mcpTokenService, server, string(data)); err != nil {
		return fmt.Errorf("mcp oauth: persist tokens: %w", err)
	}
	return nil
}

// DeleteTokens removes the stored login for server, backing `yanshi auth
// mcp-logout`. It is on the concrete type rather than the TokenStore interface
// because the refresh path has no business deleting a credential — a transient
// network failure that logged the user out would be a spectacularly bad bug.
func (s *secretsTokenStore) DeleteTokens(server string) error {
	return s.mgr.Store().Delete(mcpTokenService, server)
}

// TokenDeleter is the logout half of a TokenStore. It is a separate interface,
// asserted for by the CLI, so the ordinary refresh path physically cannot
// reach a delete.
type TokenDeleter interface {
	DeleteTokens(server string) error
}
