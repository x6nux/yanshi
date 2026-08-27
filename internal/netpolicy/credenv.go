package netpolicy

import (
	"log/slog"
	"os"

	"github.com/x6nux/yanshi/internal/secrets"
)

// CredentialPolicy is the per-launch declaration of which credential-bearing
// environment variables a child process legitimately needs.
//
// The zero value strips everything secrets.ScrubEnv recognises. That default is
// the entire point: the guard's auto-approval prompt already names "env /
// printenv puts an API key into the transcript" as a risk category, but before
// this type existed the code side stripped exactly four proxy variables and
// handed every provider key, cloud credential and VCS token straight to the
// child. A defence written only into a prompt is not a defence.
//
// AllowEnv names variables by NAME. It cannot express "allow anything that
// looks like a GitHub token", because that is precisely the predicate an
// attacker-supplied value would satisfy.
type CredentialPolicy struct {
	// AllowEnv lists variable names that survive the credential scrub.
	// Case-insensitive; '-' folds to '_'. Empty means strip everything.
	AllowEnv []string
}

// scrubPolicy converts to the secrets-package shape. Keeping the two types
// distinct means callers of netpolicy do not have to import secrets to declare
// a launch policy, while the detection rules stay in the one package that
// already owns "what counts as a secret in yanshi's runtime".
func (c CredentialPolicy) scrubPolicy() secrets.EnvScrubPolicy {
	return secrets.EnvScrubPolicy{Allow: c.AllowEnv}
}

// PrepareEnvWithPolicy is PrepareEnv plus credential stripping.
//
// Order matters and is not arbitrary: the credential scrub runs BEFORE the
// managed proxy variables are appended. Running it after would make the scrub's
// own output eligible for inspection — harmless today, since no proxy name
// matches a credential rule, but it would silently become a bug the first time
// a proxy URL carried inline basic-auth credentials (http://user:pass@proxy),
// which is a shape LooksLikeCredentialValue recognises and which the child
// genuinely needs in order to reach the proxy at all.
//
// Dropped names are logged, never values. A credential that vanishes silently
// produces the worst failure this feature can cause: `gh` reporting "not logged
// in" on a machine where the operator demonstrably is, with nothing in any log
// to search for. The log line is what turns that into a two-minute diagnosis.
func PrepareEnvWithPolicy(in []string, proxyURL string, policy CredentialPolicy) []string {
	res := secrets.ScrubEnv(in, policy.scrubPolicy())
	if len(res.DroppedNames) > 0 {
		slog.Info("netpolicy credential scrub",
			"dropped_count", len(res.DroppedNames),
			"dropped_names", res.DroppedNames,
			"allowed", policy.AllowEnv)
	}
	return PrepareEnv(res.Env, proxyURL)
}

// ManagedEnvWithPolicy is ManagedEnv plus credential stripping: it starts from
// os.Environ(), removes every credential not named in policy.AllowEnv, then
// publishes the managed proxy variables.
//
// This is the function child-process launchers should call. ManagedEnv remains
// for the callers that have not yet been given a policy to declare; see its doc
// for why it is not simply an alias with an empty policy.
func ManagedEnvWithPolicy(proxyURL string, policy CredentialPolicy) []string {
	return PrepareEnvWithPolicy(os.Environ(), proxyURL, policy)
}

// ScrubCredentials removes credential-bearing entries from env without touching
// proxy variables, for callers that build their child environment themselves
// and only want the credential half of the posture.
//
// Returns the surviving environment and the sorted names that were removed, so
// the caller can report the drop in whatever channel it already has (a tool
// result, an audit record) rather than only in this process's slog.
func ScrubCredentials(env []string, policy CredentialPolicy) (kept []string, dropped []string) {
	res := secrets.ScrubEnv(env, policy.scrubPolicy())
	return res.Env, res.DroppedNames
}
