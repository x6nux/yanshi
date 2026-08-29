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
	// Methods narrows a host verdict to individual HTTP methods. It is only
	// consultable on a request whose method is KNOWN, which for a subprocess
	// talking HTTPS means the proxy decrypted it — see CheckRequest.
	Methods []MethodRule
}

// MethodRule is one "within this host, these methods" entry. Host uses the
// same pattern syntax as Allow/Deny (exact, or ".example.com" for subdomains);
// Methods is compared case-insensitively and an empty list matches every
// method.
//
// It exists because host granularity cannot express the request an operator
// actually wants to allow. "Let the build read from registry.npmjs.org" and
// "let the build publish to registry.npmjs.org" are one host and two very
// different grants; before this the only way to permit the read was to permit
// the write with it.
type MethodRule struct {
	Host    string
	Methods []string
	Allow   bool
}

// matches reports whether this rule speaks to (host, method). host must
// already be normalized; method is folded here.
func (r MethodRule) matches(host, method string) bool {
	if !hostMatches(r.Host, host) {
		return false
	}
	if len(r.Methods) == 0 {
		return true
	}
	for _, m := range r.Methods {
		if strings.EqualFold(strings.TrimSpace(m), method) {
			return true
		}
	}
	return false
}

// CheckRequest is CheckHost plus the method dimension: the host verdict is
// computed first and, when it admits, the first matching MethodRule may
// override it in EITHER direction.
//
// Ordering is the load-bearing part and it is deliberately asymmetric. A host
// DENY is never reconsidered — the method table narrows a grant, it does not
// widen a refusal, so `deny: ["evil.test"]` cannot be undone by adding a GET
// rule for the same host. Within an admitted host the first matching rule wins
// (source order), which is what lets an operator write "allow GET, deny
// everything else" as two lines in the order they would say it out loud.
//
// method == "" means "not known at this layer" and returns the plain host
// verdict. That is the honest answer for a blind CONNECT tunnel: nobody read
// the method, so no rule about methods can have fired. Returning a method
// rule's verdict there would be a decision about a request this code never
// saw.
func (p Policy) CheckRequest(host, method string) Decision {
	d := p.CheckHost(host)
	if !d.Allowed || method == "" {
		return d
	}
	normalized := normalizeHost(host)
	for _, rule := range p.Methods {
		if !rule.matches(normalized, method) {
			continue
		}
		verb := "deny"
		if rule.Allow {
			verb = "allow"
		}
		return Decision{
			Allowed: rule.Allow,
			Rule:    "method-" + verb + ":" + rule.Host + " " + strings.ToUpper(method),
			Reason:  "method " + strings.ToUpper(method) + " " + verb + "ed by a method rule for " + rule.Host,
		}
	}
	return d
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

// GrantHost returns a copy of p in which host is admitted by CheckHost, plus
// whether such a copy exists at all.
//
// It is how a runtime approval (the net dimension of tools.request_permission)
// becomes something the request actually runs under. Returning a POLICY rather
// than a yes/no is the load-bearing part: web_fetch hands its policy to
// NewTransport, whose dialer re-runs CheckHost on every connection, so a tool
// that merely skipped its own check would be refused by the dial a few
// microseconds later — the same inert-grant failure one layer down, and just as
// silent.
//
// An explicit deny rule is NOT grantable and the second return is false. The
// allow list and the default express "not permitted yet"; a deny entry is the
// operator having named this host and said no, and a dialog able to undo that
// would make security.network.deny advisory. Callers ask BEFORE consuming an
// approval so a one-shot grant is not burned on a host it cannot admit.
//
// The IP-range half of the policy is untouched. CheckResolvedIPs still runs on
// the dial, so a granted host resolving to 169.254.169.254 is still refused:
// this widens the host rules by exactly one name and nothing else.
func (p Policy) GrantHost(host string) (Policy, bool) {
	host = normalizeHost(host)
	if host == "" {
		return p, false
	}
	for _, pattern := range p.Deny {
		if hostMatches(pattern, host) {
			return p, false
		}
	}
	out := p
	out.Allow = append(append([]string(nil), p.Allow...), host)
	return out, true
}

// NormalizeHost folds a host string to the form CheckHost compares against:
// lowercased, whitespace- and port-stripped, with a trailing dot removed.
//
// Exported because approval scopes for the net dimension are matched with
// reflect.DeepEqual, so the host recorded at grant time and the host derived
// from a URL at call time have to be normalized by the SAME function or the
// grant is inert. "API.Example.test:8443" and "api.example.test" are one host
// to this policy and must be one scope to the approval manager.
func NormalizeHost(raw string) string { return normalizeHost(raw) }

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
	return p.checkIPRanges(ips)
}

// checkIPRanges is the SSRF half of CheckResolvedIPs with the host rules left
// out.
//
// Split off for the dialer, which may be carrying a runtime grant that admits
// a host the rules do not. Running the bundled CheckResolvedIPs there would
// re-apply the host rules and refuse the connection a few microseconds after
// the operator approved it — the inert-grant failure Policy.GrantHost's doc
// describes, moved one layer down where nothing reports it.
//
// The IP half is deliberately NOT skippable. A grant widens the host rules by
// exactly one name; it does not make 169.254.169.254 reachable.
func (p Policy) checkIPRanges(ips []net.IP) Decision {
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
