// Package netpolicy is the single host-policy source yanshi consults for
// outbound network access. Both the interactive guard layer (Task 6) and the
// loopback HTTP proxy (Task 12) build a Policy from the same
// config.Security.Network block so an operator's allow/deny intent is applied
// uniformly regardless of which path a request takes.
//
// Resolution semantics:
//   - Deny wins: any matching Deny pattern short-circuits, even if Allow
//     would also match.
//   - Default deny: an empty/unknown Default value (including the zero
//     Policy) rejects. Operators opt into permissive posture by setting
//     Default="allow" — never by leaving fields blank.
//   - Resolved-IP re-check: after DNS resolves the host, every address is
//     rejected when it is loopback / RFC1918 / link-local / unspecified
//     unless AllowPrivate=true. This is the SSRF guard that makes
//     "allow *.github.com" safe even when DNS is poisoned.
package netpolicy

import (
	"fmt"
	"net"
	"strings"
)

// Decision is the binary outcome for a host or resolved-IP check plus the
// explanation the audit/UI surfaces. Rule identifies which policy entry
// fired (e.g. "allow:.example.com", "default:deny", "ip-range-deny") so the
// operator can see exactly which line admitted or blocked a call.
type Decision struct {
	Allowed bool
	Rule    string
	Reason  string
}

// Policy is the operator-configured host allow/deny table. All fields are
// optional; the zero value fails closed (Default="" → deny).
type Policy struct {
	Default      string
	Allow        []string
	Deny         []string
	AllowPrivate bool
}

// CheckHost evaluates raw against the policy. Patterns are case-insensitive
// and dot-suffix scoped (".example.com" matches "api.example.com" but not
// "example.com" itself, so an operator can grant a subdomain without granting
// the apex). Deny wins; Default is "allow" only on exact case-insensitive
// match — every other value (including empty) is deny.
func (p Policy) CheckHost(raw string) Decision {
	host := normalizeHost(raw)
	if host == "" {
		return Decision{Reason: "empty host"}
	}
	for _, pattern := range p.Deny {
		if hostMatches(pattern, host) {
			return Decision{Rule: "deny:" + pattern, Reason: "host denied by deny rule"}
		}
	}
	for _, pattern := range p.Allow {
		if hostMatches(pattern, host) {
			return Decision{Allowed: true, Rule: "allow:" + pattern, Reason: "host allowed"}
		}
	}
	// Empty/unknown default is fail-closed.
	if strings.EqualFold(p.Default, "allow") {
		return Decision{Allowed: true, Rule: "default:allow", Reason: "host allowed by default"}
	}
	return Decision{Rule: "default:deny", Reason: "host denied by default"}
}

// CheckResolvedIPs runs CheckHost first (the cheap rule check) and then, when
// the host is admitted, walks every resolved address to ensure none is
// loopback / private / link-local / unspecified. This is the SSRF guard: even
// if "allowed.example" resolves to 169.254.169.254 (a DNS-poisoning attempt
// to reach the cloud metadata service), the request is rejected unless
// AllowPrivate=true. Empty IP list is deny (DNS returned nothing useful).
func (p Policy) CheckResolvedIPs(host string, ips []net.IP) Decision {
	if d := p.CheckHost(host); !d.Allowed {
		return d
	}
	if len(ips) == 0 {
		return Decision{Reason: "DNS returned no addresses"}
	}
	for _, ip := range ips {
		if ip == nil {
			return Decision{Reason: "DNS returned invalid address"}
		}
		if !p.AllowPrivate && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified()) {
			return Decision{Rule: "ip-range-deny", Reason: fmt.Sprintf("resolved address %s is private/local", ip)}
		}
	}
	return Decision{Allowed: true, Rule: "resolved-ip-check", Reason: "all resolved addresses allowed"}
}

// normalizeHost lowercases, strips whitespace and a trailing dot, and drops a
// :port suffix if present. Used for both the input host and the policy
// patterns so the comparison is apples-to-apples.
func normalizeHost(raw string) string {
	host := strings.TrimSpace(strings.ToLower(raw))
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.TrimSuffix(host, ".")
}

// hostMatches reports whether host matches pattern. A pattern starting with
// "." is subdomain-scoped (".example.com" matches "api.example.com" and
// "sub.api.example.com" but NOT "example.com" itself); any other pattern is
// exact equality after normalization.
func hostMatches(pattern, host string) bool {
	pattern = normalizeHost(pattern)
	if pattern == "" {
		return false
	}
	if strings.HasPrefix(pattern, ".") {
		suffix := strings.TrimPrefix(pattern, ".")
		return host != suffix && strings.HasSuffix(host, "."+suffix)
	}
	return host == pattern
}
