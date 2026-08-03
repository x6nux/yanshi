package tools

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/netpolicy"
)

// allowAllPolicy is the "let web_fetch reach any host" netpolicy used by
// tests that don't care about host restrictions. It mirrors the pre-Task-13
// `NetPerm{Allow: true, Hosts: ["*"]}` profile shape. AllowPrivate=true is
// needed because httptest servers listen on 127.0.0.1.
func allowAllPolicy() *netpolicy.Policy {
	return &netpolicy.Policy{Default: "allow", AllowPrivate: true}
}

// denyAllPolicy mirrors NetPerm{Allow: false} from the pre-Task-13 era. The
// default-deny posture plus empty Allow list fails every host.
func denyAllPolicy() *netpolicy.Policy {
	return &netpolicy.Policy{Default: "deny"}
}

// profileWithWebTool builds the minimal profile needed so GuardedTool's tool-
// name check passes for web_fetch. The actual host gating is now done via
// netpolicy.Policy bound by WithNetworkPolicy.
func profileWithWebTool() guard.PermissionProfile {
	return guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"web_*"}}}
}

func TestWebFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hello web"))
	}))
	defer srv.Close()

	ctx := WithProfile(context.Background(), profileWithWebTool())
	ctx = WithNetworkPolicy(ctx, allowAllPolicy())
	out, err := runTool(ctx, NewWebTools(1024*32, 0).Fetch, `{"url":"`+srv.URL+`"}`)
	require.NoError(t, err)
	assert.Contains(t, out, "hello web")
}

func TestWebFetch_DeniedByNetPolicy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hello web"))
	}))
	defer srv.Close()

	ctx := WithProfile(context.Background(), profileWithWebTool())
	ctx = WithNetworkPolicy(ctx, denyAllPolicy())
	out, err := runTool(ctx, NewWebTools(1024*32, 0).Fetch, `{"url":"`+srv.URL+`"}`)
	require.NoError(t, err, "permission denial must surface as a result, not a Go error")
	assert.Contains(t, out, "✗")
	assert.Contains(t, out, "permission denied")
}

// TestWebFetch_DeniesWithoutProfile verifies that runFetch fails closed when
// no network policy is bound to the context (standalone safety check).
func TestWebFetch_DeniesWithoutProfile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hello web"))
	}))
	defer srv.Close()

	// No policy in context — should be denied.
	out, err := runTool(context.Background(), NewWebTools(1024*32, 0).Fetch, `{"url":"`+srv.URL+`"}`)
	require.NoError(t, err, "permission denial must surface as a result, not a Go error")
	assert.Contains(t, out, "✗")
	assert.Contains(t, out, "permission denied")
}

// TestWebFetch_DeniesEmptyHost verifies that a URL with no host (e.g. a
// malformed URL) is denied before the net check, preventing host-allow-list
// bypass.
func TestWebFetch_DeniesEmptyHost(t *testing.T) {
	ctx := WithProfile(context.Background(), profileWithWebTool())
	ctx = WithNetworkPolicy(ctx, allowAllPolicy())
	// A URL with no host: "not a url" → url.Parse succeeds but Hostname() is "".
	out, err := runTool(ctx, NewWebTools(1024*32, 0).Fetch, `{"url":"not-a-url"}`)
	require.NoError(t, err, "permission denial must surface as a result, not a Go error")
	assert.Contains(t, out, "✗")
	assert.Contains(t, out, "empty host")
}

// TestWebFetch_RedirectEnforcesHostPolicy verifies that the CheckRedirect
// callback re-runs the netpolicy host check against each redirect target. A
// server that 302-redirects from an allowed host to a forbidden host must be
// denied.
func TestWebFetch_RedirectEnforcesHostPolicy(t *testing.T) {
	policy := &netpolicy.Policy{Default: "deny", Allow: []string{"allowed.example.com"}}

	// Simulate what CheckRedirect does for a redirect to a forbidden host.
	forbiddenHost := "169.254.169.254"
	dec := policy.CheckHost(forbiddenHost)
	assert.False(t, dec.Allowed, "redirect to %q must be denied by host policy", forbiddenHost)

	// And an allowed host passes.
	allowedHost := "allowed.example.com"
	dec = policy.CheckHost(allowedHost)
	assert.True(t, dec.Allowed, "redirect to %q must be allowed by host policy", allowedHost)
}

// TestWebFetch_RedirectAllowedHost verifies the full redirect flow works
// when the policy allows all hosts — preserving the current default behavior.
func TestWebFetch_RedirectAllowedHost(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("redirected OK"))
	}))
	defer target.Close()

	initial := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer initial.Close()

	ctx := WithProfile(context.Background(), profileWithWebTool())
	ctx = WithNetworkPolicy(ctx, allowAllPolicy())
	out, err := runTool(ctx, NewWebTools(1024*32, 0).Fetch, `{"url":"`+initial.URL+`"}`)
	require.NoError(t, err)
	assert.Contains(t, out, "redirected OK")
}

// TestWebFetch_RedirectDeniedByHostPolicy verifies the full end-to-end flow
// where a restrictive host policy denies the redirect target. The initial
// httptest server (127.0.0.1) is allowed, but the server 302-redirects to a
// forbidden host (169.254.169.254) which the CheckRedirect guard must block.
func TestWebFetch_RedirectDeniedByHostPolicy(t *testing.T) {
	badRedirectURL := "http://169.254.169.254/latest/meta-data/"

	initial := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, badRedirectURL, http.StatusFound)
	}))
	defer initial.Close()

	ctx := WithProfile(context.Background(), profileWithWebTool())
	ctx = WithNetworkPolicy(ctx, &netpolicy.Policy{Default: "deny", Allow: []string{"127.0.0.1"}, AllowPrivate: true})
	out, err := runTool(ctx, NewWebTools(1024*32, 0).Fetch, `{"url":"`+initial.URL+`"}`)
	require.NoError(t, err, "permission denial must surface as a result, not a Go error")
	assert.Contains(t, out, "✗")
	assert.Contains(t, out, "redirect denied")
}

// TestWebFetch_CheckRedirectEmptyHost verifies that a redirect target with
// an empty host (e.g. file:// URL) would be caught by the empty-host check.
func TestWebFetch_CheckRedirectEmptyHost(t *testing.T) {
	h := hostOnly("file:///etc/passwd")
	assert.Empty(t, h, "file:// URL should have empty host")
}

// Ensure fmt and url imports are used.
var _ = fmt.Sprintf
var _ = url.Parse

func TestWebFetchImageContentTypeReturnsStructuredRef(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte{0x89, 'P', 'N', 'G'})
	}))
	defer srv.Close()
	ctx := WithProfile(context.Background(), profileWithWebTool())
	ctx = WithNetworkPolicy(ctx, allowAllPolicy())
	out, err := runTool(ctx, NewWebTools(1024*32, 0).Fetch, `{"url":"`+srv.URL+`"}`)
	require.NoError(t, err)
	assert.Contains(t, strings.ToLower(out), "image")
	assert.Contains(t, out, srv.URL)
}
