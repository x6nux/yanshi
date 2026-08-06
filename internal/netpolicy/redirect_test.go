package netpolicy

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// TestRedirectToDeniedHostIsRecheckedNotFollowed pins the redirect half of
// S09's "DNS/重定向不能绕过".
//
// The bypass this rules out: an allowed host answers 302 pointing at a denied
// one. If the proxy treated a redirect as a continuation of the original
// (already-authorized) request, the policy would be evaluated once, for a host
// the operator approved, and the traffic would end up somewhere they did not.
//
// It cannot happen here, and the reason is structural rather than a check
// anyone wrote: the proxy never follows redirects. It hands the 302 back and
// the CLIENT issues a second request -- which arrives as a fresh proxied
// request and is policed from scratch. The test drives exactly that sequence,
// so the property stops being an inference about control flow and becomes an
// assertion: an ordinary redirect-following client, pointed at an allowed host
// that redirects to a denied one, ends up with a 403.
//
// Both synthetic names resolve through fakeResolver to loopback, and the URLs
// carry the test server's port, so the proxy's own dial reaches the upstream
// without the test having to install a custom dialer.
func TestRedirectToDeniedHostIsRecheckedNotFollowed(t *testing.T) {
	var port string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://denied.example:"+port+"/secret", http.StatusFound)
	}))
	defer upstream.Close()

	u, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	port = u.Port()

	p, err := NewProxy(Policy{
		Allow:        []string{"allowed.example"},
		Default:      "deny",
		AllowPrivate: true,
	}, fakeResolver{ips: []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	// A default client follows redirects, which is the point: the second hop
	// must be policed even though the caller only ever asked for the first.
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(p.URL())}}

	resp, err := client.Get("http://allowed.example:" + port + "/start")
	if err != nil {
		t.Fatalf("first hop must reach the proxy: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("redirect to a denied host must be refused on the second hop, got %d", resp.StatusCode)
	}
}
