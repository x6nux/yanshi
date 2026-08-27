package shell

import (
	"context"
	"io"
	"runtime"
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/secproc"
)

// childEnvViaProductionFactory launches /usr/bin/env through the PRODUCTION
// factory and returns what the child actually saw.
//
// Real spawn, real factory, on purpose. The credential scrub lives in the
// host-environment baseline the factory builds, and a test that constructs its
// own exec.Cmd would re-introduce exactly the fake-is-more-capable-than-real
// divergence that hid the missing-PATH bug for months (see
// TestDefaultSecureFactoryChildInheritsHostEnv). Only the child's own view of
// its environment proves a credential did not reach it.
func childEnvViaProductionFactory(t *testing.T, spec secproc.SecureProcessSpec) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("uses /usr/bin/env to dump the child environment")
	}
	f := DefaultSecureFactory{OS: OSProcessFactory{}, ProxyURL: "http://127.0.0.1:9999"}
	spec.Program = "/usr/bin/env"
	spec.Dir = t.TempDir()
	proc, err := f.Start(context.Background(), spec)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	out, _ := io.ReadAll(proc.Stdout)
	_ = proc.Wait()
	return string(out)
}

// TestChildDoesNotInheritHostCredentials is the end-to-end assertion for the
// credential scrub: a provider API key exported in yanshi's own process must
// not appear in the environment of a child launched on the model's behalf.
//
// This is the leak the guard's auto-approval prompt has always named — "env /
// printenv puts an API key into the transcript, which then goes to the
// provider on the next request" — and which, until the scrub existed, nothing
// in the code prevented. shell_run could read every key yanshi holds.
func TestChildDoesNotInheritHostCredentials(t *testing.T) {
	secrets := map[string]string{
		"OPENAI_API_KEY":        "sk-" + strings.Repeat("x", 40),
		"ANTHROPIC_API_KEY":     "sk-ant-" + strings.Repeat("y", 40),
		"AWS_SECRET_ACCESS_KEY": strings.Repeat("z", 40),
		"GH_TOKEN":              "ghp_" + strings.Repeat("A", 36),
		"YANSHI_TEST_DSN":       "postgres://appuser:hunter2@db.internal:5432/app",
	}
	for name, value := range secrets {
		t.Setenv(name, value)
	}
	t.Setenv("YANSHI_TEST_PLAIN", "not-a-credential")

	env := childEnvViaProductionFactory(t, secproc.SecureProcessSpec{Tool: "shell_run"})

	for name, value := range secrets {
		if strings.Contains(env, name+"=") {
			t.Errorf("child inherited %s; env was:\n%s", name, env)
		}
		// The value must not survive under ANY name either — a scrub that
		// dropped the name while copying the value elsewhere would pass the
		// check above and leak just as badly.
		if strings.Contains(env, value) {
			t.Errorf("the VALUE of %s reached the child under some other name", name)
		}
	}
	if !strings.Contains(env, "YANSHI_TEST_PLAIN=not-a-credential") {
		t.Errorf("an ordinary variable was scrubbed; env was:\n%s", env)
	}
}

// TestChildKeepsStructuralEnvAfterScrub is the other half: the scrub must not
// break the child. A child with no PATH cannot resolve its interpreter, and a
// child with no HOME makes gh write its state into the repository — both were
// real regressions on this exact code path, from a different cause.
func TestChildKeepsStructuralEnvAfterScrub(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-"+strings.Repeat("x", 40))
	env := childEnvViaProductionFactory(t, secproc.SecureProcessSpec{Tool: "shell_run"})

	for _, name := range []string{"PATH", "HOME"} {
		if !strings.Contains(env, name+"=") {
			t.Errorf("child has no %s after the credential scrub; env was:\n%s", name, env)
		}
	}
	// The scrub must not have cost the proxy injection either.
	if !strings.Contains(env, "HTTP_PROXY=http://127.0.0.1:9999") ||
		!strings.Contains(env, "http_proxy=http://127.0.0.1:9999") {
		t.Errorf("managed proxy variables missing after the scrub; env was:\n%s", env)
	}
}

// TestChildInheritsAllowedCredential proves the escape hatch reaches the real
// child. The scrub is only shippable if a legitimately-declared credential
// actually arrives: gh cannot authenticate without GH_TOKEN, and a hatch that
// is wired but non-functional would make every github_* tool report "not
// logged in" on a machine where `gh auth status` succeeds.
func TestChildInheritsAllowedCredential(t *testing.T) {
	t.Setenv("GH_TOKEN", "ghp_"+strings.Repeat("A", 36))
	t.Setenv("OPENAI_API_KEY", "sk-"+strings.Repeat("x", 40))

	env := childEnvViaProductionFactory(t, secproc.SecureProcessSpec{
		Tool:     "github_pr_view",
		AllowEnv: []string{"GH_TOKEN"},
	})

	if !strings.Contains(env, "GH_TOKEN=ghp_") {
		t.Errorf("the declared credential did not reach the child; env was:\n%s", env)
	}
	if strings.Contains(env, "OPENAI_API_KEY=") {
		t.Errorf("AllowEnv widened past what it named; env was:\n%s", env)
	}
}

// TestSecureLaunchFactoryScrubsCredentials covers the shell v2 path, which is
// a SEPARATE launcher from secproc: SecureLaunchFactory implements
// ProcessFactory, DefaultSecureFactory implements secproc.Factory, and one type
// cannot satisfy both. They share childLaunchPosture precisely so a security
// property cannot hold on one path and not the other — the two previously
// drifted on env construction, which is how the missing-PATH bug happened. A
// scrub that reached only the secproc path would leave shell v2 tools handing
// out every key yanshi holds.
func TestSecureLaunchFactoryScrubsCredentials(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /usr/bin/env to dump the child environment")
	}
	t.Setenv("OPENAI_API_KEY", "sk-"+strings.Repeat("x", 40))
	t.Setenv("GH_TOKEN", "ghp_"+strings.Repeat("A", 36))
	t.Setenv("YANSHI_TEST_PLAIN", "not-a-credential")

	f := NewSecureLaunchFactory(SecureLaunchFactory{ProxyURL: "http://127.0.0.1:9999"})
	proc, console, err := f.Start(context.Background(), LaunchSpec{
		Program:  "/usr/bin/env",
		Dir:      t.TempDir(),
		AllowEnv: []string{"GH_TOKEN"},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	out, _ := io.ReadAll(console)
	_ = proc.Wait()
	env := string(out)

	if strings.Contains(env, "OPENAI_API_KEY=") {
		t.Errorf("shell v2 child inherited a credential; env was:\n%s", env)
	}
	if !strings.Contains(env, "GH_TOKEN=ghp_") {
		t.Errorf("shell v2 AllowEnv did not admit the declared credential; env was:\n%s", env)
	}
	if !strings.Contains(env, "PATH=") || !strings.Contains(env, "YANSHI_TEST_PLAIN=") {
		t.Errorf("shell v2 child lost structural or ordinary env; env was:\n%s", env)
	}
}
