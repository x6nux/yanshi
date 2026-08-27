package mcp

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestLoopbackCallbackDeliversTheCode is the happy path, and it also pins that
// the browser gets a readable page: a user staring at a connection error cannot
// tell a rejected callback from a crashed CLI.
func TestLoopbackCallbackDeliversTheCode(t *testing.T) {
	cb, err := StartLoopbackCallback("st4te")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer cb.Close()

	if !strings.HasPrefix(cb.RedirectURI(), "http://127.0.0.1:") {
		t.Errorf("redirect URI %q is not loopback", cb.RedirectURI())
	}
	resp, err := http.Get(cb.RedirectURI() + "?code=abc123&state=st4te")
	if err != nil {
		t.Fatalf("callback GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("browser got %d for a valid callback", resp.StatusCode)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	code, err := cb.Wait(ctx)
	if err != nil || code != "abc123" {
		t.Fatalf("Wait = %q, %v", code, err)
	}
}

// TestLoopbackCallbackRejections covers every way a callback must be refused.
// The state check is the important one: without it, any page the user visits
// during the flow can drive their browser to this URL with an attacker's code,
// and this process would store the attacker's identity as the user's.
func TestLoopbackCallbackRejections(t *testing.T) {
	cases := []struct {
		name  string
		query url.Values
		want  string
	}{
		{"state mismatch", url.Values{"code": {"c"}, "state": {"wrong"}}, "state mismatch"},
		{"state absent", url.Values{"code": {"c"}}, "state mismatch"},
		{"provider denied", url.Values{"state": {"st4te"}, "error": {"access_denied"}}, "access_denied"},
		{"no code at all", url.Values{"state": {"st4te"}}, "no authorization code"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cb, err := StartLoopbackCallback("st4te")
			if err != nil {
				t.Fatalf("start: %v", err)
			}
			defer cb.Close()

			resp, err := http.Get(cb.RedirectURI() + "?" + tc.query.Encode())
			if err != nil {
				t.Fatalf("callback GET: %v", err)
			}
			resp.Body.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			code, err := cb.Wait(ctx)
			if err == nil {
				t.Fatalf("rejected callback returned code %q with no error", code)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want one naming %q", err, tc.want)
			}
		})
	}
}

// TestLoopbackCallbackDoesNotReflectTheQuery: the query is attacker-influenced
// and rendering it back into the page is a reflected-content hazard for no
// diagnostic gain — the CLI prints the real reason.
func TestLoopbackCallbackDoesNotReflectTheQuery(t *testing.T) {
	cb, err := StartLoopbackCallback("st4te")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer cb.Close()

	marker := "MARKER-payload-9f2b"
	resp, err := http.Get(cb.RedirectURI() + "?state=wrong&error=" + marker)
	if err != nil {
		t.Fatalf("callback GET: %v", err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 2048)
	n, _ := resp.Body.Read(buf)
	if strings.Contains(string(buf[:n]), marker) {
		t.Fatalf("the response page echoed the query back: %q", string(buf[:n]))
	}
}

// TestLoopbackCallbackWaitRespectsContext: a user who never completes the
// browser flow must not wedge the CLI forever.
func TestLoopbackCallbackWaitRespectsContext(t *testing.T) {
	cb, err := StartLoopbackCallback("st4te")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer cb.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := cb.Wait(ctx); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want a timeout", err)
	}
}

// TestAuthorizationURLPinsTheSafeParameters. response_type and
// code_challenge_method are fixed on purpose: the implicit flow puts the token
// in a URL fragment (browser history, referrer headers) and "plain" protects
// nothing. Neither is a knob an operator should be able to turn.
func TestAuthorizationURLPinsTheSafeParameters(t *testing.T) {
	got, err := AuthorizationURL("https://idp.example/authorize", "cid",
		"http://127.0.0.1:9/callback", "st4te", "chal", []string{"read", "offline_access"})
	if err != nil {
		t.Fatalf("AuthorizationURL: %v", err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("result is not a URL: %v", err)
	}
	q := u.Query()
	for k, want := range map[string]string{
		"response_type":         "code",
		"code_challenge_method": "S256",
		"client_id":             "cid",
		"redirect_uri":          "http://127.0.0.1:9/callback",
		"state":                 "st4te",
		"code_challenge":        "chal",
		"scope":                 "read offline_access",
	} {
		if q.Get(k) != want {
			t.Errorf("query[%s] = %q, want %q", k, q.Get(k), want)
		}
	}
}

// TestAuthorizationURLPreservesExistingQuery: some providers require an
// `audience` or `resource` parameter baked into the configured URL. Clobbering
// it produces an opaque token that the MCP server then rejects.
func TestAuthorizationURLPreservesExistingQuery(t *testing.T) {
	got, err := AuthorizationURL("https://idp.example/authorize?audience=api%3A%2F%2Fmcp",
		"cid", "http://127.0.0.1:9/callback", "s", "c", nil)
	if err != nil {
		t.Fatalf("AuthorizationURL: %v", err)
	}
	u, _ := url.Parse(got)
	if u.Query().Get("audience") != "api://mcp" {
		t.Fatalf("the configured audience parameter was dropped: %q", got)
	}
}

// TestAuthorizationURLRefusesCleartext: a cleartext authorize endpoint puts the
// redirect — and therefore the code — on the wire. Loopback is exempt because
// that is how a local test IdP is reached and it never leaves the machine.
func TestAuthorizationURLRefusesCleartext(t *testing.T) {
	cases := []struct {
		url    string
		refuse bool
	}{
		{"https://idp.example/authorize", false},
		{"http://127.0.0.1:8080/authorize", false},
		{"http://localhost:8080/authorize", false},
		{"http://idp.example/authorize", true},
		{"ftp://idp.example/authorize", true},
	}
	for _, tc := range cases {
		t.Run(tc.url, func(t *testing.T) {
			_, err := AuthorizationURL(tc.url, "c", "http://127.0.0.1:9/cb", "s", "ch", nil)
			if (err != nil) != tc.refuse {
				t.Fatalf("AuthorizationURL(%q) err = %v, refuse=%v", tc.url, err, tc.refuse)
			}
		})
	}
}
