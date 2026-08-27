package mcp

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

// PKCE holds one authorization request's proof key (RFC 7636).
//
// PKCE is not optional here even though this is a confidential-client shape
// with a client_secret available. The redirect leg runs over a LOOPBACK HTTP
// listener, and every other process on the machine can bind a port and race for
// the callback; the authorization code alone is therefore not a secret. The
// verifier never leaves this process, so a stolen code cannot be exchanged.
type PKCE struct {
	// Verifier is the high-entropy secret kept in memory and sent only on the
	// token request.
	Verifier string
	// Challenge is S256(Verifier), sent on the authorization request.
	Challenge string
}

// pkceVerifierBytes is the raw entropy behind a verifier. RFC 7636 §4.1 allows
// 43–128 characters after base64url encoding; 32 bytes lands at 43, the
// specification's own minimum, and is the size every major provider tests
// against.
const pkceVerifierBytes = 32

// NewPKCE mints a fresh verifier/challenge pair.
//
// Only S256 is produced. The spec also defines the "plain" method, in which the
// challenge IS the verifier — which provides no protection at all against an
// attacker who can see the authorization request, i.e. the exact threat PKCE
// exists for. A server that rejects S256 is a server this flow declines to
// speak to rather than silently downgrade for.
func NewPKCE() (PKCE, error) {
	raw := make([]byte, pkceVerifierBytes)
	if _, err := rand.Read(raw); err != nil {
		return PKCE{}, fmt.Errorf("mcp oauth: generate pkce verifier: %w", err)
	}
	verifier := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(verifier))
	return PKCE{
		Verifier:  verifier,
		Challenge: base64.RawURLEncoding.EncodeToString(sum[:]),
	}, nil
}

// pkceStateBytes is the entropy behind the CSRF state parameter. 16 bytes is
// the conventional size; the value only has to be unguessable for the lifetime
// of one browser redirect.
const pkceStateBytes = 16

// NewOAuthState mints the CSRF state parameter.
//
// The callback handler compares it against what came back and refuses a
// mismatch. Without it, any page the user visits during the flow can navigate
// their browser to the loopback callback with an attacker-obtained code, and
// this process would exchange it and store the attacker's token as the user's.
func NewOAuthState() (string, error) {
	raw := make([]byte, pkceStateBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("mcp oauth: generate state: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
