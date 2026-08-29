package shell

import (
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/netpolicy"
)

// postureEnv runs the shared child-launch posture and returns the environment
// it would hand a child.
func postureEnv(p childLaunchPosture) []string { return p.env(nil, nil) }

// TestPosturePublishesEveryManagedProxyEndpoint pins that the three endpoints
// travel together. The SOCKS listener and the inspection root are both
// features whose only production producer is this posture: publish neither and
// the SOCKS5 handler has no clients and the generated CA is trusted by
// nothing.
func TestPosturePublishesEveryManagedProxyEndpoint(t *testing.T) {
	env := postureEnv(childLaunchPosture{
		Policy:   &netpolicy.Policy{},
		ProxyURL: "http://127.0.0.1:1234",
		SOCKSURL: "socks5h://127.0.0.1:1234",
		CAFile:   "/tmp/yanshi-ca.pem",
	})
	joined := strings.Join(env, "\n")
	for _, want := range []string{
		"HTTP_PROXY=http://127.0.0.1:1234",
		"http_proxy=http://127.0.0.1:1234",
		"ALL_PROXY=socks5h://127.0.0.1:1234",
		"all_proxy=socks5h://127.0.0.1:1234",
		"SSL_CERT_FILE=/tmp/yanshi-ca.pem",
		"NODE_EXTRA_CA_CERTS=/tmp/yanshi-ca.pem",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("child environment missing %q", want)
		}
	}
}

// TestPostureWithoutInspectionPublishesNoTrustVariables is the opt-in half of
// ADR-0023 measured at the place a child actually reads it.
func TestPostureWithoutInspectionPublishesNoTrustVariables(t *testing.T) {
	env := postureEnv(childLaunchPosture{
		Policy:   &netpolicy.Policy{},
		ProxyURL: "http://127.0.0.1:1234",
		SOCKSURL: "socks5h://127.0.0.1:1234",
	})
	joined := strings.Join(env, "\n")
	for _, key := range []string{"CURL_CA_BUNDLE", "REQUESTS_CA_BUNDLE", "NODE_EXTRA_CA_CERTS", "GIT_SSL_CAINFO"} {
		if strings.Contains(joined, key+"=") {
			t.Fatalf("%s published with inspection off", key)
		}
	}
}

// TestSnapshotIsScrubbedBeforeItReachesAChild is the security-critical
// ordering in W-B-21.
//
// An rc file exporting a provider key is one of the likeliest sources of a
// credential in a captured environment. The snapshot is layered in BEFORE
// ScrubCredentials runs, so it is subject to the same policy as yanshi's own
// environment — applying it afterwards would hand the child exactly the
// secrets the scrub exists to remove, on a path with no second allowlist.
func TestSnapshotIsScrubbedBeforeItReachesAChild(t *testing.T) {
	posture := childLaunchPosture{
		Policy:   &netpolicy.Policy{},
		ProxyURL: "http://127.0.0.1:1",
		Snapshot: Snapshot{Env: map[string]string{
			"ANTHROPIC_API_KEY":          "sk-ant-secret",
			"YANSHI_PROBE_TOOLCHAIN_DIR": "/opt/probe-toolchain",
		}},
	}
	joined := strings.Join(postureEnv(posture), "\n")
	if strings.Contains(joined, "sk-ant-secret") || strings.Contains(joined, "ANTHROPIC_API_KEY=") {
		t.Fatal("a credential from the captured shell environment reached the child")
	}
	if !strings.Contains(joined, "YANSHI_PROBE_TOOLCHAIN_DIR=/opt/probe-toolchain") {
		t.Fatal("the scrub took the whole snapshot instead of just the credential")
	}
}

// TestSnapshotDoesNotSmuggleAProxyVariable covers the other ordering hazard: a
// captured http_proxy must not survive, or an rc file would silently redirect
// every child's egress away from the managed proxy.
func TestSnapshotDoesNotSmuggleAProxyVariable(t *testing.T) {
	posture := childLaunchPosture{
		Policy:   &netpolicy.Policy{},
		ProxyURL: "http://127.0.0.1:1",
		Snapshot: Snapshot{Env: map[string]string{
			"http_proxy": "http://attacker.test:8080",
			"ALL_PROXY":  "socks5://attacker.test:1080",
		}},
	}
	joined := strings.Join(postureEnv(posture), "\n")
	if strings.Contains(joined, "attacker.test") {
		t.Fatalf("a captured proxy variable survived: %s", joined)
	}
}

// TestProxyCredentialsSurviveTheScrub pins the ordering that used to be
// documented on the now-deleted netpolicy.PrepareEnvWithPolicy (W-B fix-b57
// finding 7): childLaunchPosture.env runs netpolicy.ScrubCredentials BEFORE
// netpolicy.PrepareEnvFor appends the managed proxy variables. Running it the
// other way round would make the scrub's own output eligible for inspection,
// which is harmless until a proxy URL carries inline basic-auth credentials
// (http://user:pass@proxy) — a shape LooksLikeCredentialValue recognises, and
// exactly the value the child needs in order to reach the proxy at all.
//
// This invariant used to be pinned only in internal/netpolicy against
// PrepareEnvWithPolicy directly; that function had zero production callers
// (production always went through this posture) and was deleted along with
// its dedicated test. This is the replacement, against the composition that
// actually ships.
func TestProxyCredentialsSurviveTheScrub(t *testing.T) {
	const proxy = "http://proxyuser:proxypass@127.0.0.1:9000"
	posture := childLaunchPosture{Policy: &netpolicy.Policy{}, ProxyURL: proxy}
	joined := strings.Join(postureEnv(posture), "\n")
	for _, want := range []string{"HTTP_PROXY=" + proxy, "http_proxy=" + proxy} {
		if !strings.Contains(joined, want) {
			t.Fatalf("%s missing: the credential scrub must run before the proxy is published, not after: %s", want, joined)
		}
	}
}

// TestBothProductionFactoriesShareOnePosture is the drift guard the two
// factories' posture() methods exist for. shell v2 shipped without ProxyURL
// while the secproc path had it, so identical env SEMANTICS were applied to
// non-identical INPUTS and only one of the two launch paths was proxied.
func TestBothProductionFactoriesShareOnePosture(t *testing.T) {
	policy := &netpolicy.Policy{}
	snap := Snapshot{Env: map[string]string{"YANSHI_PROBE_TOOLCHAIN_DIR": "/opt/probe-toolchain"}}
	secproc := DefaultSecureFactory{
		Policy: policy, ProxyURL: "http://p", SOCKSURL: "socks5h://p",
		CAFile: "/ca.pem", Snapshot: snap,
	}.posture()
	v2 := SecureLaunchFactory{
		Policy: policy, ProxyURL: "http://p", SOCKSURL: "socks5h://p",
		CAFile: "/ca.pem", Snapshot: snap,
	}.posture()

	a, b := strings.Join(postureEnv(secproc), "\n"), strings.Join(postureEnv(v2), "\n")
	if a != b {
		t.Fatalf("the two production launch paths build different child environments:\n%s\n---\n%s", a, b)
	}
	if !strings.Contains(a, "YANSHI_PROBE_TOOLCHAIN_DIR=/opt/probe-toolchain") || !strings.Contains(a, "ALL_PROXY=socks5h://p") {
		t.Fatalf("posture dropped a field: %s", a)
	}
}
