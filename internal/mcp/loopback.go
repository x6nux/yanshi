package mcp

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// LoopbackCallback is the one-shot HTTP listener that receives the OAuth
// redirect (RFC 8252 §7.3: native apps use a loopback redirect URI).
//
// A loopback listener is used rather than a custom URI scheme because a scheme
// registration is an OS-level install step yanshi does not perform, and rather
// than an out-of-band paste flow because those are deprecated and increasingly
// refused by providers. Port 0 is requested so no fixed port has to be free.
type LoopbackCallback struct {
	ln     net.Listener
	srv    *http.Server
	result chan callbackResult
	state  string
}

// callbackResult is what the redirect delivered.
type callbackResult struct {
	code string
	err  error
}

// StartLoopbackCallback binds 127.0.0.1 on an ephemeral port and serves exactly
// one redirect.
//
// state is the CSRF token minted for this authorization request; a callback
// carrying a different one is refused without being exchanged. That refusal is
// the whole reason the parameter exists: any page the user happens to visit
// during the flow can navigate their browser to this loopback URL with an
// attacker-supplied code, and exchanging it would store the attacker's identity
// as the user's.
func StartLoopbackCallback(state string) (*LoopbackCallback, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("mcp oauth: bind loopback callback: %w", err)
	}
	c := &LoopbackCallback{ln: ln, result: make(chan callbackResult, 1), state: state}
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", c.handle)
	c.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = c.srv.Serve(ln) }()
	return c, nil
}

// RedirectURI is the exact string to send as redirect_uri. It must be reused
// byte-identically on the token exchange; providers compare it literally.
func (c *LoopbackCallback) RedirectURI() string {
	return "http://" + c.ln.Addr().String() + "/callback"
}

// handle processes the single redirect. It answers the browser with a plain
// page either way, because a user staring at a connection error has no way to
// tell a rejected callback from a crashed CLI.
func (c *LoopbackCallback) handle(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	res := c.classify(q)
	select {
	case c.result <- res:
	default:
		// A second callback for a flow already resolved. Dropped rather than
		// blocking the handler forever on an unbuffered send.
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if res.err != nil {
		w.WriteHeader(http.StatusBadRequest)
		// The browser gets a fixed string. The query it carries is
		// attacker-influenceable and rendering it back is a reflected-content
		// hazard for no diagnostic gain — the CLI prints the real reason.
		fmt.Fprintln(w, "Authorization failed. Return to your terminal for details.")
		return
	}
	fmt.Fprintln(w, "Authorization complete. You can close this tab and return to your terminal.")
}

// classify turns the redirect query into a result, enforcing the state check
// before anything else is read.
func (c *LoopbackCallback) classify(q url.Values) callbackResult {
	if got := q.Get("state"); got != c.state {
		return callbackResult{err: fmt.Errorf(
			"mcp oauth: callback state mismatch; the redirect did not come from the request this CLI started")}
	}
	if errCode := q.Get("error"); errCode != "" {
		return callbackResult{err: fmt.Errorf("mcp oauth: authorization denied (%s)", errCode)}
	}
	code := q.Get("code")
	if code == "" {
		return callbackResult{err: fmt.Errorf("mcp oauth: callback carried no authorization code")}
	}
	return callbackResult{code: code}
}

// Wait blocks until the redirect arrives or ctx expires, then returns the
// authorization code.
func (c *LoopbackCallback) Wait(ctx context.Context) (string, error) {
	select {
	case res := <-c.result:
		return res.code, res.err
	case <-ctx.Done():
		return "", fmt.Errorf("mcp oauth: timed out waiting for the browser redirect: %w", ctx.Err())
	}
}

// Close shuts the listener down. Safe to call more than once.
func (c *LoopbackCallback) Close() error { return c.srv.Close() }

// AuthorizationURL builds the authorization request URL.
//
// response_type=code and code_challenge_method=S256 are fixed rather than
// configurable: the implicit flow returns the token in a URL fragment (it lands
// in browser history and referrer headers) and the "plain" challenge method
// offers no protection whatsoever. Neither is a knob an operator should be able
// to turn.
func AuthorizationURL(authURL, clientID, redirectURI, state, challenge string, scopes []string) (string, error) {
	u, err := url.Parse(authURL)
	if err != nil {
		return "", fmt.Errorf("mcp oauth: parse authorization_url: %w", err)
	}
	if u.Scheme != "https" && u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost" {
		// A cleartext authorization endpoint puts the redirect (and therefore
		// the code) on the wire. Loopback is exempt because that is how a local
		// test server is reached and it never leaves the machine.
		return "", fmt.Errorf("mcp oauth: authorization_url must be https (got %q)", u.Scheme)
	}
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", clientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("state", state)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	if len(scopes) > 0 {
		q.Set("scope", strings.Join(scopes, " "))
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}
