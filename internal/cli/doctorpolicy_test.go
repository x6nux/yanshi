package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/x6nux/yanshi/internal/config"
)

// isolatePolicyEnv points $YANSHI_POLICY at an existing but empty policy file
// so the test is independent of whether the developer running it happens to
// have ~/.yanshi/policy.yaml. An empty policy parses to zero profiles and is
// therefore inert; pointing at a nonexistent path would instead be an error,
// which is a different case with its own test in internal/config.
func isolatePolicyEnv(t *testing.T) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "empty-policy.yaml")
	require.NoError(t, os.WriteFile(p, []byte("profiles: {}\n"), 0o600))
	t.Setenv(config.PolicyEnvVar, p)
}

// TestCheckPolicyScope_WarnsWhenConfigIsAgentWritable is the reason this check
// exists: the shipped default puts config.yaml in the working directory, the
// agent can write there, and one restart later the profile is whatever the
// agent wrote.
func TestCheckPolicyScope_WarnsWhenConfigIsAgentWritable(t *testing.T) {
	isolatePolicyEnv(t)
	root := t.TempDir()
	cfgPath := filepath.Join(root, "config.yaml")

	got := checkPolicyScope(&config.Config{}, nil, cfgPath, root)
	assert.Equal(t, StatusWarn, got.Status)
	assert.Contains(t, got.Message, "widen its own profile")
	// The message must name a remedy. A warning an operator cannot act on is
	// one they learn to skip, which costs more than it reports. With
	// $YANSHI_POLICY already pointing somewhere, the actionable remedy is that
	// path — telling them to set a variable they have set would be noise.
	policyPath, _ := config.PolicyPath()
	assert.Contains(t, got.Message, policyPath)
}

// TestCheckPolicyScope_RemedyNamesTheEnvVarWhenUnset is the other branch of the
// remedy text. It matters because the two audiences are different: somebody
// with no policy at all needs to be told the mechanism exists, while somebody
// who already set the variable needs the path they should populate.
func TestCheckPolicyScope_RemedyNamesTheEnvVarWhenUnset(t *testing.T) {
	t.Setenv(config.PolicyEnvVar, "")
	root := t.TempDir()
	got := checkPolicyScope(&config.Config{}, nil, filepath.Join(root, "config.yaml"), root)
	require.Equal(t, StatusWarn, got.Status)
	if _, configured := config.PolicyPath(); !configured {
		// No home directory resolvable; the env var is then the only remedy.
		assert.Contains(t, got.Message, config.PolicyEnvVar)
		return
	}
	// A home directory exists, so the default ~/.yanshi/policy.yaml is the
	// remedy the operator should act on.
	policyPath, _ := config.PolicyPath()
	assert.Contains(t, got.Message, policyPath)
}

// TestCheckPolicyScope_OKWhenPolicyActive.
func TestCheckPolicyScope_OKWhenPolicyActive(t *testing.T) {
	isolatePolicyEnv(t)
	root := t.TempDir()
	cfg := &config.Config{PolicyActive: true, PolicyNarrowed: []string{"orchestrator"}}

	got := checkPolicyScope(cfg, nil, filepath.Join(root, "config.yaml"), root)
	assert.Equal(t, StatusOK, got.Status)
	assert.Contains(t, got.Message, "only narrow")
	assert.Contains(t, got.Message, "orchestrator",
		"a profile that was clamped must be named, so a narrowing that did not "+
			"take effect is visible at boot rather than inferred from a denial later")
}

// TestCheckPolicyScope_OKWhenConfigOutsideWorkRoot.
func TestCheckPolicyScope_OKWhenConfigOutsideWorkRoot(t *testing.T) {
	isolatePolicyEnv(t)
	got := checkPolicyScope(&config.Config{}, nil,
		filepath.Join(t.TempDir(), "config.yaml"), t.TempDir())
	assert.Equal(t, StatusOK, got.Status)
	assert.Contains(t, got.Message, "outside")
}

// TestCheckPolicyScope_SkippedOnConfigError pins that this check follows the
// same degradation rule as every other: a config-load failure downgrades it to
// a skipped warn rather than aborting the run.
func TestCheckPolicyScope_SkippedOnConfigError(t *testing.T) {
	got := checkPolicyScope(nil, assert.AnError, "config.yaml", t.TempDir())
	assert.Equal(t, StatusWarn, got.Status)
	assert.Contains(t, got.Message, "skipped")
}

// TestCheckPolicyFilePerms_FlagsWorldWritablePolicy. A policy file the machine
// can rewrite is not trusted, and the failure is silent — yanshi reads it and
// reports policy-scope as OK.
func TestCheckPolicyFilePerms_FlagsWorldWritablePolicy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits are not the access-control mechanism on Windows")
	}
	dir := t.TempDir()
	policy := filepath.Join(dir, "policy.yaml")
	require.NoError(t, os.WriteFile(policy, []byte("profiles: {}\n"), 0o666))
	// WriteFile's mode is masked by the process umask, which is 022 on a normal
	// developer machine — 0o666 lands as 0644 and the check correctly reports
	// OK, so the assertion below would fail locally while passing in CI (umask
	// 0). Chmod is not masked; it is what actually sets the world-write bit.
	require.NoError(t, os.Chmod(policy, 0o666))
	t.Setenv(config.PolicyEnvVar, policy)

	got := checkPolicyFilePerms(filepath.Join(dir, "config.yaml"))
	assert.Equal(t, StatusWarn, got.Status)
	assert.Contains(t, got.Message, "policy.yaml")
}

// TestCheckPolicyFilePerms_OKWhenOwnerOnly.
func TestCheckPolicyFilePerms_OKWhenOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits are not the access-control mechanism on Windows")
	}
	dir := t.TempDir()
	policy := filepath.Join(dir, "policy.yaml")
	require.NoError(t, os.WriteFile(policy, []byte("profiles: {}\n"), 0o600))
	t.Setenv(config.PolicyEnvVar, policy)

	cfgPath := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte("{}\n"), 0o600))

	got := checkPolicyFilePerms(cfgPath)
	assert.Equal(t, StatusOK, got.Status)
}

// TestCheckAuthCommandScope_OKWhenNoProviderConfiguresAuth is the common case:
// nothing to protect, so nothing to warn about.
func TestCheckAuthCommandScope_OKWhenNoProviderConfiguresAuth(t *testing.T) {
	cfg := &config.Config{}
	got := checkAuthCommandScope(cfg, nil)
	assert.Equal(t, StatusOK, got.Status)
	assert.Contains(t, got.Message, "no provider configures")
}

// TestCheckAuthCommandScope_WarnsWhenUngoverned is B-2's reason to exist: a
// provider configures auth.command from the working-directory config and no
// trusted policy file governs it, so an fs_write plus a restart could point
// that command at anything.
func TestCheckAuthCommandScope_WarnsWhenUngoverned(t *testing.T) {
	cfg := &config.Config{}
	cfg.LLM.Providers = []config.ProviderConfig{
		{Name: "primary", Auth: &config.ProviderAuthConfig{Command: []string{"/usr/bin/issuer"}}},
	}
	got := checkAuthCommandScope(cfg, nil)
	assert.Equal(t, StatusWarn, got.Status)
	assert.Contains(t, got.Message, "primary")
	assert.Contains(t, got.Message, "provider_auth",
		"the message must name the remedy, not just the problem")
}

// TestCheckAuthCommandScope_OKWhenPolicyFileGovernsIt pins the OK branch, and
// specifically that it keys off PolicyFileActive rather than PolicyActive — a
// policy document that pins ONLY security.provider_auth, naming no `profiles:`
// block, is a legitimate thing to write (ApplyPolicy applies it regardless of
// whether any profile was named) and must not be reported as unprotected.
func TestCheckAuthCommandScope_OKWhenPolicyFileGovernsIt(t *testing.T) {
	cfg := &config.Config{PolicyFileActive: true}
	cfg.LLM.Providers = []config.ProviderConfig{
		{Name: "primary", Auth: &config.ProviderAuthConfig{Command: []string{"/usr/bin/issuer"}}},
	}
	got := checkAuthCommandScope(cfg, nil)
	assert.Equal(t, StatusOK, got.Status)
	assert.Contains(t, got.Message, "primary")
}

// TestCheckAuthCommandScope_SkippedOnConfigError follows the same degradation
// rule as every other check in this file.
func TestCheckAuthCommandScope_SkippedOnConfigError(t *testing.T) {
	got := checkAuthCommandScope(nil, assert.AnError)
	assert.Equal(t, StatusWarn, got.Status)
	assert.Contains(t, got.Message, "skipped")
}

// TestRunDoctor_IncludesPolicyChecks pins that the checks are actually WIRED
// into the report. Both functions could be perfect and unreachable, which is
// the failure mode the unit tests above cannot see.
func TestRunDoctor_IncludesPolicyChecks(t *testing.T) {
	isolatePolicyEnv(t)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte("server:\n  http_addr: 127.0.0.1:0\n"), 0o600))

	rep := RunDoctor(t.Context(), DoctorOptions{ConfigPath: cfgPath, Root: dir})

	names := map[string]bool{}
	for _, c := range rep.Checks {
		names[c.Name] = true
	}
	assert.True(t, names["policy-scope"], "policy-scope must appear in the doctor report")
	assert.True(t, names["policy-perms"], "policy-perms must appear in the doctor report")
	assert.True(t, names["auth-command-scope"], "auth-command-scope must appear in the doctor report")
}
