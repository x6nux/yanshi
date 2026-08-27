package secproc_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/secproc"
)

// errDenied stands in for a guard refusal in the authorize-ordering test.
var errDenied = errors.New("denied by test authorizer")

// credCtx returns a context carrying the spy factory, with a permissive
// authorizer installed for the duration of the test. Launch runs Authorize
// before it scrubs, so a test that only wants to observe the scrub still has
// to get past the firewall.
func credCtx(t *testing.T, spy *spyFactory) context.Context {
	t.Helper()
	saved := secproc.SwapAuthorizer(func(context.Context, guard.Action, string) error { return nil })
	t.Cleanup(func() { secproc.SwapAuthorizer(saved) })
	return secproc.WithFactory(context.Background(), spy)
}

// specEnvNames maps the env the factory received, so assertions read as names.
func specEnvNames(env []string) map[string]bool {
	out := make(map[string]bool, len(env))
	for _, entry := range env {
		if i := strings.IndexByte(entry, '='); i >= 0 {
			out[entry[:i]] = true
		}
	}
	return out
}

// TestLaunchScrubsSpecEnv covers the half of the defence that lives in Launch:
// a caller cannot route a credential past the scrub by putting it in spec.Env
// instead of relying on the host environment. The Factory scrubs the host
// baseline; this scrubs what the caller handed in.
func TestLaunchScrubsSpecEnv(t *testing.T) {
	cases := []struct {
		name        string
		allowEnv    []string
		wantPresent []string
		wantAbsent  []string
		why         string
	}{
		{
			name:        "default strips every credential",
			allowEnv:    nil,
			wantPresent: []string{"GIT_CONFIG_NOSYSTEM", "XDG_CONFIG_HOME"},
			wantAbsent:  []string{"OPENAI_API_KEY", "GH_TOKEN", "DATABASE_URL"},
			why:         "the zero AllowEnv is the secure default",
		},
		{
			name:        "AllowEnv admits exactly what it names",
			allowEnv:    []string{"GH_TOKEN"},
			wantPresent: []string{"GIT_CONFIG_NOSYSTEM", "XDG_CONFIG_HOME", "GH_TOKEN"},
			wantAbsent:  []string{"OPENAI_API_KEY", "DATABASE_URL"},
			why:         "the gh escape hatch is per-spawn and by name",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spy := &spyFactory{}
			ctx := credCtx(t, spy)
			_, err := secproc.Launch(ctx, secproc.SecureProcessSpec{
				Tool:     "shell_run",
				Program:  "gh",
				AllowEnv: tc.allowEnv,
				Env: []string{
					"GIT_CONFIG_NOSYSTEM=1",
					"XDG_CONFIG_HOME=/work/.yanshi/tmp/gitxdg",
					"OPENAI_API_KEY=sk-" + strings.Repeat("x", 40),
					"GH_TOKEN=ghp_" + strings.Repeat("A", 36),
					"DATABASE_URL=postgres://appuser:hunter2@db.internal:5432/app",
				},
			})
			if err != nil {
				t.Fatalf("Launch: %v", err)
			}
			got := specEnvNames(spy.last.Env)
			for _, name := range tc.wantPresent {
				if !got[name] {
					t.Errorf("%s missing from the factory spec but must survive (%s)", name, tc.why)
				}
			}
			for _, name := range tc.wantAbsent {
				if got[name] {
					t.Errorf("%s reached the factory but must be scrubbed (%s)", name, tc.why)
				}
			}
		})
	}
}

// TestLaunchPreservesNilEnv pins the nil-vs-empty distinction. The Factory
// checks len(spec.Env) to decide whether to layer caller entries over the host
// baseline; replacing a nil Env with an empty non-nil slice would be invisible
// here and would change that decision for every call site that passes none —
// which is nearly all of them.
func TestLaunchPreservesNilEnv(t *testing.T) {
	spy := &spyFactory{}
	ctx := credCtx(t, spy)
	if _, err := secproc.Launch(ctx, secproc.SecureProcessSpec{Tool: "shell_run", Program: "go"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if spy.last.Env != nil {
		t.Fatalf("a nil Env became %#v; the factory reads len(Env) to decide whether to layer", spy.last.Env)
	}
}

// TestLaunchDoesNotDropNonCredentialEnv is the "child must still run" half.
// A scrub that took PATH or HOME would leave the child unable to resolve its
// own interpreter, and the operator would see "command not found" with nothing
// pointing at this code.
func TestLaunchDoesNotDropNonCredentialEnv(t *testing.T) {
	spy := &spyFactory{}
	ctx := credCtx(t, spy)
	env := []string{
		"PATH=/usr/bin:/bin",
		"HOME=/home/operator",
		"LANG=en_US.UTF-8",
		"TERM=xterm-256color",
		"GOMODCACHE=/home/operator/go/pkg/mod",
	}
	if _, err := secproc.Launch(ctx, secproc.SecureProcessSpec{
		Tool: "run_tests", Program: "go", Env: env,
	}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if len(spy.last.Env) != len(env) {
		t.Fatalf("structural variables were dropped: got %v, want %v", spy.last.Env, env)
	}
}

// TestSpecCredentialPolicyCarriesAllowEnv pins the conversion the Factory
// relies on to scrub its own host-environment baseline. The Factory reads the
// policy off the spec, so a conversion that dropped AllowEnv would silently
// re-strip a credential the caller legitimately declared — gh would go back to
// reporting "not logged in".
func TestSpecCredentialPolicyCarriesAllowEnv(t *testing.T) {
	spec := secproc.SecureProcessSpec{AllowEnv: []string{"GH_TOKEN", "GITHUB_TOKEN"}}
	got := spec.CredentialPolicy()
	if strings.Join(got.AllowEnv, ",") != "GH_TOKEN,GITHUB_TOKEN" {
		t.Fatalf("CredentialPolicy().AllowEnv = %v, want the spec's AllowEnv", got.AllowEnv)
	}
	if empty := (secproc.SecureProcessSpec{}).CredentialPolicy(); len(empty.AllowEnv) != 0 {
		t.Fatalf("the zero spec must yield an empty policy, got %v", empty.AllowEnv)
	}
}

// TestLaunchScrubRunsAfterAuthorize pins the pipeline order. A denied spawn
// must never reach the scrub, because reaching it would mean the Factory ran:
// the scrub is the last step before Factory.Start, so observing it at all on a
// denied action proves the firewall was bypassed.
func TestLaunchScrubRunsAfterAuthorize(t *testing.T) {
	spy := &spyFactory{}
	saved := secproc.SwapAuthorizer(func(context.Context, guard.Action, string) error {
		return errDenied
	})
	defer secproc.SwapAuthorizer(saved)
	ctx := secproc.WithFactory(context.Background(), spy)
	if _, err := secproc.Launch(ctx, secproc.SecureProcessSpec{
		Tool: "shell_run", Env: []string{"GH_TOKEN=ghp_" + strings.Repeat("A", 36)},
	}); err == nil {
		t.Fatal("a denied action must not launch")
	}
	if spy.calls != 0 {
		t.Fatalf("the factory ran %d time(s) for a denied action", spy.calls)
	}
}
