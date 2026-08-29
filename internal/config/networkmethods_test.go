package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestNetworkMethodRulesLoad is the happy path: the table survives
// deserialization with its order and its verdicts intact. Order is part of the
// contract — the first matching rule wins — so a loader that reordered or
// deduplicated would change what the policy means.
func TestNetworkMethodRulesLoad(t *testing.T) {
	cfg, err := LoadBytes([]byte(
		"security:\n" +
			"  network:\n" +
			"    default: deny\n" +
			"    allow: [\"registry.npmjs.org\"]\n" +
			"    inspect_https: true\n" +
			"    methods:\n" +
			"      - host: registry.npmjs.org\n" +
			"        methods: [GET, HEAD]\n" +
			"        action: allow\n" +
			"      - host: registry.npmjs.org\n" +
			"        action: deny\n"))
	require.NoError(t, err)
	require.True(t, cfg.Security.Network.InspectHTTPS)
	require.Len(t, cfg.Security.Network.Methods, 2)
	require.Equal(t, []string{"GET", "HEAD"}, cfg.Security.Network.Methods[0].Methods)
	require.Equal(t, "allow", cfg.Security.Network.Methods[0].Action)
	require.Empty(t, cfg.Security.Network.Methods[1].Methods)
	require.Equal(t, "deny", cfg.Security.Network.Methods[1].Action)
}

// TestNetworkMethodRuleRejectsAMissingAction is the fail-closed half of the
// validation. There is no default verdict: a typo ("alow") would otherwise
// become a deny under any zero-value reading, which is the opposite of what an
// operator writing an allow rule meant — and they would discover it from
// traffic, months later.
func TestNetworkMethodRuleRejectsAMissingAction(t *testing.T) {
	for _, action := range []string{"", "alow", "permit", "ALLOW-ish"} {
		_, err := LoadBytes([]byte(
			"security:\n  network:\n    methods:\n" +
				"      - host: api.test\n        action: \"" + action + "\"\n"))
		require.Error(t, err, "action %q was accepted", action)
		require.Contains(t, err.Error(), "action must be")
	}
}

// TestNetworkMethodRuleAcceptsEitherCase pins that the validation is about the
// VALUE and not its spelling, since the policy builder folds case too.
func TestNetworkMethodRuleAcceptsEitherCase(t *testing.T) {
	for _, action := range []string{"allow", "ALLOW", " Deny "} {
		_, err := LoadBytes([]byte(
			"security:\n  network:\n    methods:\n" +
				"      - host: api.test\n        action: \"" + action + "\"\n"))
		require.NoError(t, err, "action %q was rejected", action)
	}
}

// TestNetworkMethodRuleRejectsAMissingHost: a rule with no subject names
// nothing and can never fire.
func TestNetworkMethodRuleRejectsAMissingHost(t *testing.T) {
	_, err := LoadBytes([]byte(
		"security:\n  network:\n    methods:\n      - methods: [GET]\n        action: allow\n"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "host is required")
}

// TestShellRuntimeDefaultsAreUnchanged pins that the two new shell fields
// default to the previous behaviour: no cap, no capture, no login shell run at
// boot.
func TestShellRuntimeDefaultsAreUnchanged(t *testing.T) {
	cfg, err := LoadBytes([]byte("{}"))
	require.NoError(t, err)
	require.Zero(t, cfg.Security.Shell.MaxConcurrent)
	require.False(t, cfg.Security.Shell.CaptureProfile)
	require.Empty(t, cfg.Security.Shell.ProfileShell)
	require.False(t, cfg.Security.Network.InspectHTTPS)
	require.Empty(t, cfg.Security.Network.Methods)
}
