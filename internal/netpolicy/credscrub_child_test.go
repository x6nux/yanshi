package netpolicy_test

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/netpolicy"
)

// credscrub_child_test.go verifies the credential scrub the only way that
// actually settles the question: by STARTING A REAL CHILD PROCESS and reading
// back the environment that process itself observes.
//
// Every other test in this package asserts on the []string that ScrubEnv
// returns. That is a necessary check and it is not sufficient, because the
// thing being defended is not a slice — it is what `printenv` prints inside
// shell_run, which then lands in the model's transcript and travels to the
// provider on the next request. Between the slice and that outcome sit
// exec.Cmd.Env semantics (last duplicate wins), the platform's own environment
// injection, and the possibility that a caller layers the host environment back
// on top. A slice-level assertion cannot see any of those.
//
// The child is this test binary re-executed with a marker variable, rather than
// /usr/bin/env: `env` does not exist on Windows, and the CI matrix runs there.

// envDumpMarker switches the re-executed test binary into "print my
// environment and exit" mode.
//
// The name is deliberately boring. A marker called TEST_TOKEN or CHILD_KEY
// would be stripped by the very scrub under test, the child would never enter
// dump mode, and the test would report a pass because it found no secrets in an
// empty output — the exact shape of a vacuous green.
const envDumpMarker = "YANSHI_TEST_ENV_DUMP"

// TestMain implements the child half. When the marker is present the process
// prints its own environment and exits; otherwise it runs the normal suite.
func TestMain(m *testing.M) {
	if os.Getenv(envDumpMarker) == "1" {
		for _, e := range os.Environ() {
			os.Stdout.WriteString(e + "\n")
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// probeCredentials are planted in the parent environment before the scrub. The
// VALUES are distinctive strings that cannot occur incidentally, so a match in
// the child's output is proof of a leak and not of a coincidence.
//
// The set spans both detection directions ScrubEnv documents, because a
// regression in either one is invisible to a probe that only exercises the
// other:
//   - names that are obviously credentials (OPENAI_API_KEY, GH_TOKEN)
//   - an innocent NAME carrying a secret VALUE (DATABASE_URL with an inline
//     password), which name-matching alone would pass straight through
var probeCredentials = [][2]string{
	{"OPENAI_API_KEY", "sk-yanshiprobe-openai-0000000000000000"},
	{"ANTHROPIC_API_KEY", "sk-ant-yanshiprobe-0000000000000000"},
	{"GH_TOKEN", "ghp_yanshiprobeAAAAAAAAAAAAAAAAAAAAAAAA"},
	{"AWS_SECRET_ACCESS_KEY", "yanshiprobeAWSsecret0000000000000000000"},
	{"DATABASE_URL", "postgres://user:yanshiprobepassword@db/app"},
}

// structuralVars must survive. This half is not decoration: a scrub that
// stripped PATH would pass every leak assertion above while making the child
// unable to find its own interpreter. "Contained" and "broken" are different
// outcomes and only this half distinguishes them.
var structuralVars = []string{"PATH", "HOME"}

// dumpChildEnv re-executes the test binary with the given environment and
// returns what the child process reports as its own environment.
func dumpChildEnv(t *testing.T, env []string) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("cannot locate test binary: %v", err)
	}
	cmd := exec.Command(self)
	cmd.Env = append(append([]string{}, env...), envDumpMarker+"=1")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("child process failed: %v", err)
	}
	return string(out)
}

// plantProbeCredentials sets the probe variables in this process's environment
// (t.Setenv restores them) and returns the resulting environment.
func plantProbeCredentials(t *testing.T) {
	t.Helper()
	for _, kv := range probeCredentials {
		t.Setenv(kv[0], kv[1])
	}
	// A non-credential planted alongside them, to prove the scrub removes the
	// credentials specifically rather than truncating the environment.
	t.Setenv("YANSHI_TEST_ORDINARY", "yanshiprobe-ordinary-value")
}

// TestScrubbedChildProcessCannotSeeCredentials is the real-subprocess proof:
// credentials present in the parent are absent from the child, while the
// variables the child needs in order to run at all are still there.
func TestScrubbedChildProcessCannotSeeCredentials(t *testing.T) {
	plantProbeCredentials(t)

	got := dumpChildEnv(t, netpolicy.ManagedEnvWithPolicy("", netpolicy.CredentialPolicy{}))

	for _, kv := range probeCredentials {
		if strings.Contains(got, kv[1]) {
			t.Errorf("child process can read %s — the credential value reached the child environment", kv[0])
		}
	}
	for _, name := range structuralVars {
		if !strings.Contains(got, name+"=") {
			t.Errorf("child process has no %s: the scrub removed a structural variable, which breaks the child rather than containing it", name)
		}
	}
	if !strings.Contains(got, "yanshiprobe-ordinary-value") {
		t.Error("ordinary non-credential variable was dropped; the scrub is truncating the environment rather than filtering it")
	}
}

// TestUnscrubbedChildProcessSeesCredentials is the CONTROL, and without it the
// test above proves nothing. If the probe values never reached a child under
// any configuration — a typo in a name, a t.Setenv that did not take, a child
// that inherits nothing — the leak assertions would pass vacuously.
//
// This launches the same child with the UNSCRUBBED parent environment and
// requires every probe value to be visible. That is the pre-fix behaviour the
// scrub exists to change, demonstrated rather than asserted.
func TestUnscrubbedChildProcessSeesCredentials(t *testing.T) {
	plantProbeCredentials(t)

	got := dumpChildEnv(t, os.Environ())

	for _, kv := range probeCredentials {
		if !strings.Contains(got, kv[1]) {
			t.Errorf("control failed: %s did not reach an UNSCRUBBED child, so the scrubbed-child assertions would pass vacuously", kv[0])
		}
	}
}

// TestAllowEnvReachesTheChildProcess proves the escape hatch is real at the
// process boundary, not just in the returned slice.
//
// It matters because the hatch's whole purpose is a program that cannot work
// without one specific credential (`gh` without GH_TOKEN reports "not logged
// in" on a machine where the operator plainly is). The assertion is two-sided
// in one run: the named variable arrives AND the unnamed ones still do not, so
// an allowlist that accidentally disabled the scrub would fail here.
func TestAllowEnvReachesTheChildProcess(t *testing.T) {
	plantProbeCredentials(t)

	got := dumpChildEnv(t, netpolicy.ManagedEnvWithPolicy("", netpolicy.CredentialPolicy{AllowEnv: []string{"GH_TOKEN"}}))

	if !strings.Contains(got, "ghp_yanshiprobeAAAAAAAAAAAAAAAAAAAAAAAA") {
		t.Error("GH_TOKEN was allowlisted but did not reach the child; the escape hatch does not work at the process boundary")
	}
	for _, kv := range probeCredentials {
		if kv[0] == "GH_TOKEN" {
			continue
		}
		if strings.Contains(got, kv[1]) {
			t.Errorf("allowlisting GH_TOKEN also let %s through: the allowlist widened the scrub instead of naming one exception", kv[0])
		}
	}
}

// TestScrubbedEnvironKeepsAChildRunnableWithoutCredentials covers the helper the
// four non-secproc spawn sites use.
//
// It is a separate test from the ManagedEnvWithPolicy one above because the two
// answer different questions and only one of them is about a proxy:
// ScrubbedEnviron publishes NO proxy variables, and a caller that reached for
// ManagedEnvWithPolicy instead would silently change a language server's egress
// behaviour as a side effect of a credential fix. Asserting the absence is what
// stops a future simplification from folding the two together.
func TestScrubbedEnvironKeepsAChildRunnableWithoutCredentials(t *testing.T) {
	plantProbeCredentials(t)

	got := dumpChildEnv(t, netpolicy.ScrubbedEnviron())

	for _, kv := range probeCredentials {
		if strings.Contains(got, kv[1]) {
			t.Errorf("child can read %s: an MCP server, a language server, gh or git "+
				"would receive it", kv[0])
		}
	}
	for _, name := range structuralVars {
		if !strings.Contains(got, name+"=") {
			t.Errorf("child has no %s: the scrub broke the child rather than containing it", name)
		}
	}
	if !strings.Contains(got, "yanshiprobe-ordinary-value") {
		t.Error("an ordinary variable was dropped; the scrub is truncating rather than filtering")
	}
	for _, proxyVar := range []string{"HTTP_PROXY=", "http_proxy=", "NO_PROXY="} {
		if strings.Contains(got, proxyVar) {
			t.Errorf("ScrubbedEnviron published %s; it must not touch the child's egress "+
				"configuration — that is ManagedEnvWithPolicy's job", proxyVar)
		}
	}
}

// TestScrubbedEnvironAllowlistIsByNameOnly pins the escape hatch the `gh`
// callers depend on, in both directions.
//
// One-sided is not enough: an allowlist that accidentally disabled the scrub
// would satisfy "GH_TOKEN arrives" perfectly.
func TestScrubbedEnvironAllowlistIsByNameOnly(t *testing.T) {
	plantProbeCredentials(t)

	got := dumpChildEnv(t, netpolicy.ScrubbedEnviron(netpolicy.GitHubCLICredentialEnv...))

	if !strings.Contains(got, "ghp_yanshiprobeAAAAAAAAAAAAAAAAAAAAAAAA") {
		t.Error("GH_TOKEN is in GitHubCLICredentialEnv but did not reach the child; " +
			"`yanshi pr` would report gh as not logged in on a machine where it is")
	}
	for _, kv := range probeCredentials {
		if kv[0] == "GH_TOKEN" {
			continue
		}
		if strings.Contains(got, kv[1]) {
			t.Errorf("the gh allowlist also let %s through", kv[0])
		}
	}
}
