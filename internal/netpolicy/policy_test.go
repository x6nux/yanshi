package netpolicy

import (
	"net"
	"testing"
)

func TestDefaultEmptyIsDenyAndDenyWins(t *testing.T) {
	p := Policy{Allow: []string{"api.example.com"}, Deny: []string{"api.example.com"}}
	if d := p.CheckHost("api.example.com"); d.Allowed {
		t.Fatalf("deny must win: %#v", d)
	}
	if d := (Policy{}).CheckHost("example.com"); d.Allowed {
		t.Fatalf("empty default must deny: %#v", d)
	}
}

// TestSubdomainPatternDoesNotMatchApex —— 见台账。
//
// ledger: A1/S09#2 host/port 规则生效
func TestSubdomainPatternDoesNotMatchApex(t *testing.T) {
	p := Policy{Default: "deny", Allow: []string{".example.com"}}
	if p.CheckHost("example.com").Allowed {
		t.Fatal("scoped subdomain pattern must not match apex")
	}
	if !p.CheckHost("api.example.com").Allowed {
		t.Fatal("subdomain should match")
	}
}

// TestResolvedIPsRejectPrivateLoopbackLinkLocal —— 见台账。
//
// ledger: A1/S09#3 DNS/重定向不能绕过
func TestResolvedIPsRejectPrivateLoopbackLinkLocal(t *testing.T) {
	p := Policy{Default: "deny", Allow: []string{"allowed.example"}}
	for _, ip := range []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("10.0.0.1"), net.ParseIP("169.254.1.1"), net.ParseIP("::1")} {
		if d := p.CheckResolvedIPs("allowed.example", []net.IP{ip}); d.Allowed {
			t.Fatalf("private/local %s allowed: %#v", ip, d)
		}
	}
}

func TestCheckHost_EmptyHostIsDenied(t *testing.T) {
	d := Policy{}.CheckHost("")
	if d.Allowed || d.Reason != "empty host" {
		t.Fatalf("empty host must be denied: %#v", d)
	}
}

// TestCheckHost_StripsPort —— 见台账。
//
// ledger: A1/S09#2 host/port 规则生效
func TestCheckHost_StripsPort(t *testing.T) {
	p := Policy{Default: "allow", Deny: []string{"malicious.example"}}
	// Host with port must still match against the host-only deny rule.
	if d := p.CheckHost("malicious.example:8080"); d.Allowed {
		t.Fatalf("port-stripped host must still match deny rule: %#v", d)
	}
}

func TestCheckHost_EmptyDenyPatternIsInert(t *testing.T) {
	// An empty pattern in the deny list must not match anything.
	p := Policy{Default: "allow", Deny: []string{""}}
	if d := p.CheckHost("anything.example"); !d.Allowed {
		t.Fatalf("empty deny pattern should not block: %#v", d)
	}
}

func TestCheckResolvedIPs_EmptyIPsIsDenied(t *testing.T) {
	p := Policy{Default: "allow", Allow: []string{"host.example"}}
	d := p.CheckResolvedIPs("host.example", nil)
	if d.Allowed || d.Reason != "DNS returned no addresses" {
		t.Fatalf("empty IPs must be denied: %#v", d)
	}
}

func TestCheckResolvedIPs_NilIPIsDenied(t *testing.T) {
	p := Policy{Default: "allow", Allow: []string{"host.example"}}
	d := p.CheckResolvedIPs("host.example", []net.IP{nil})
	if d.Allowed || d.Reason != "DNS returned invalid address" {
		t.Fatalf("nil IP must be denied: %#v", d)
	}
}

func TestCheckResolvedIPs_AllowPrivatePassesAllIPs(t *testing.T) {
	p := Policy{Default: "allow", Allow: []string{"host.example"}, AllowPrivate: true}
	d := p.CheckResolvedIPs("host.example", []net.IP{net.ParseIP("127.0.0.1")})
	if !d.Allowed {
		t.Fatalf("private IP must be allowed when AllowPrivate=true: %#v", d)
	}
}

func TestCheckResolvedIPs_CheckHostRejectionShortCircuits(t *testing.T) {
	// When CheckHost rejects the host, CheckResolvedIPs must return early
	// without even examining the IPs.
	p := Policy{Default: "deny"}
	d := p.CheckResolvedIPs("unknown.example", []net.IP{net.ParseIP("1.2.3.4")})
	if d.Allowed {
		t.Fatalf("host rejected by CheckHost must deny: %#v", d)
	}
}
