package netpolicy

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestCheckRequestMethodRules(t *testing.T) {
	policy := Policy{
		Default: "deny",
		Allow:   []string{"api.test", ".cdn.test"},
		Deny:    []string{"evil.test"},
		Methods: []MethodRule{
			{Host: "api.test", Methods: []string{"get", "HEAD"}, Allow: true},
			{Host: "api.test", Allow: false}, // no methods = every remaining verb
			{Host: "evil.test", Methods: []string{"GET"}, Allow: true},
		},
	}
	cases := []struct {
		name    string
		host    string
		method  string
		allowed bool
	}{
		{"allowed host, allowed method", "api.test", "GET", true},
		{"method match is case-insensitive both ways", "api.test", "head", true},
		{"allowed host, denied method", "api.test", "POST", false},
		{"catch-all rule with no methods", "api.test", "DELETE", false},
		{"no rule for this host keeps the host verdict", "img.cdn.test", "POST", true},
		{"unknown host is still the default", "other.test", "GET", false},
		{"a method rule cannot widen a host deny", "evil.test", "GET", false},
		{"empty method returns the host verdict", "api.test", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := policy.CheckRequest(tc.host, tc.method); got.Allowed != tc.allowed {
				t.Fatalf("CheckRequest(%q, %q).Allowed = %v, want %v (rule %q, reason %q)",
					tc.host, tc.method, got.Allowed, tc.allowed, got.Rule, got.Reason)
			}
		})
	}
}

// TestCheckRequestWithNoMethodTableMatchesCheckHost pins that adding the
// method dimension changed nothing for the configurations that do not use it.
func TestCheckRequestWithNoMethodTableMatchesCheckHost(t *testing.T) {
	policy := Policy{Default: "deny", Allow: []string{"ok.test"}, Deny: []string{"no.test"}}
	for _, host := range []string{"ok.test", "no.test", "other.test", ""} {
		for _, method := range []string{"", "GET", "POST", "DELETE"} {
			want := policy.CheckHost(host)
			got := policy.CheckRequest(host, method)
			if got != want {
				t.Fatalf("CheckRequest(%q, %q) = %+v, CheckHost = %+v", host, method, got, want)
			}
		}
	}
}

// TestAGrantDoesNotOpenThePrivateAddressRanges is the boundary on the runtime
// approval. The host rules were widened by exactly one name; the SSRF guard is
// untouched, so an approved host resolving to the cloud metadata service is
// still refused.
//
// It is the reason PolicyDialer skips CheckHost and not CheckResolvedIPs when
// Granted answers true, and it is the test that goes red if someone
// "simplifies" that back into one call.
//
// "the guard refused" and "the dial itself failed" both surface to the client
// as a 502 with the underlying error as the body, so asserting on the status
// code alone (or on the "netpolicy:" prefix, which every netpolicy dial error
// carries) cannot tell the two apart — a mutant that lets a granted host skip
// checkIPRanges still gets a 502, just from the real dial to 169.254.169.254
// failing instead. The assertion below reads the checkIPRanges Reason text
// itself out of the body.
//
// A short client timeout keeps the result from depending on how long a real
// dial to 169.254.169.254 takes on the runner: on the unmutated tree
// checkIPRanges answers before any socket opens (no I/O, effectively 0s), so
// the timeout never matters. If the guard is skipped, the real dial either
// fails fast, hangs past the timeout, or — on a host where that address is a
// live metadata endpoint — actually succeeds; all three still fail this test
// (wrong body, client error, or 200 respectively) instead of racing the
// network for up to the transport's own 10s dial timeout.
func TestAGrantDoesNotOpenThePrivateAddressRanges(t *testing.T) {
	p, err := NewProxy(Policy{Default: "deny"}, fakeResolver{
		ips: []net.IPAddr{{IP: net.IPv4(169, 254, 169, 254)}},
	})
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}
	defer p.Close()
	p.SetApprover(&recordingApprover{verdict: true})

	client := proxyClient(p)
	client.Timeout = 500 * time.Millisecond
	resp, err := client.Get("http://metadata.test/latest/meta-data/")
	if err != nil {
		t.Fatalf("checkIPRanges did not answer before the dial (got a client-side error instead of an immediate 502): %v", err)
	}
	defer resp.Body.Close()
	if !p.isGranted("metadata.test") {
		t.Fatal("the approval was not recorded, so this test proves nothing about grants")
	}
	// The host is approved, so the handler lets it through; the DIAL is what
	// refuses, which surfaces as 502 rather than 403.
	if resp.StatusCode == http.StatusOK {
		t.Fatal("an approved host reached a link-local address")
	}
	body, _ := io.ReadAll(resp.Body)
	const wantReason = "resolved address 169.254.169.254 is private/local"
	if !strings.Contains(string(body), wantReason) {
		t.Fatalf("body = %q, want it to contain the checkIPRanges reason %q (a dial that simply fails against the real address also produces a 502 with a \"netpolicy:\" prefix, so that prefix alone cannot prove the guard fired)", body, wantReason)
	}
}

// TestGrantedHostStillDialsThroughThePolicyDialer is the positive control for
// the test above: with AllowPrivate set, the same grant DOES reach loopback,
// so the refusal above is the IP check and not a grant that never worked.
func TestGrantedHostStillDialsThroughThePolicyDialer(t *testing.T) {
	origin := newUpstream(t)
	p := newTestProxy(t, Policy{Default: "deny"})
	p.SetApprover(&recordingApprover{verdict: true})

	resp, err := proxyClient(p).Get("http://granted.test:" + origin.port() + "/x")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if n := origin.count(); n != 1 {
		t.Fatalf("origin saw %d requests, want 1", n)
	}
}

// TestPolicyDialerWithoutGrantsIsUnchanged pins that web_fetch's transport,
// which passes no Granted hook, still refuses on the host rules alone.
func TestPolicyDialerWithoutGrantsIsUnchanged(t *testing.T) {
	dialer := &PolicyDialer{
		Policy:   &Policy{Default: "deny", AllowPrivate: true},
		Resolver: loopbackResolver{},
	}
	if _, err := dialer.DialContext(context.Background(), "tcp", "denied.test:80"); err == nil {
		t.Fatal("a dialer with no grant hook admitted a denied host")
	}
}
