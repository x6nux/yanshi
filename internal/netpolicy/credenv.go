package netpolicy

import (
	"log/slog"
	"os"
	"strings"

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

// ScrubbedEnviron is this process's environment with every credential removed
// except the names in allow.
//
// It exists because a spawn site that writes `cmd.Env = os.Environ()` — or that
// simply omits cmd.Env, which means the same thing — hands the child every
// provider API key, cloud credential and VCS token the operator exported. Four
// production sites did exactly that: stdio MCP servers, language servers, the
// `gh` invocations behind `yanshi pr`, and the skills installer's git clone.
// None of them goes through secproc or shell.Factory, so none of them inherited
// the scrub those two apply.
//
// It publishes NO proxy variables, which is what makes it different from
// ManagedEnvWithPolicy and is deliberate: the callers above are not the
// untrusted-program launch path, and pointing a language server at the managed
// proxy would change its egress behaviour as a side effect of a credential fix.
// This function does one thing.
//
// The allowlist is by NAME and variadic so the common case reads as
// ScrubbedEnviron() with nothing to explain. A caller passing names is making an
// authorization statement about a specific program — `gh` cannot authenticate
// without GH_TOKEN no matter how restrictive the posture — and those lists
// belong in a named variable next to the caller, not inline.
//
// On top of the caller's list, the operator's ChildEnvAllowlistEnv is merged in.
// See that constant for why the escape hatch is here and not per call site.
func ScrubbedEnviron(allow ...string) []string {
	allow = append(append([]string(nil), allow...), OperatorAllowedChildEnv()...)
	kept, dropped := ScrubCredentials(os.Environ(), CredentialPolicy{AllowEnv: allow})
	kept = withoutName(kept, ChildEnvAllowlistEnv)
	if len(dropped) > 0 {
		slog.Debug("netpolicy inherited-environment scrub",
			"dropped_count", len(dropped),
			"dropped_names", dropped,
			"allowed", allow)
	}
	return kept
}

// ChildEnvAllowlistEnv is the operator's escape hatch from the credential
// scrub: a comma-separated list of variable NAMES that survive it, for every
// helper program yanshi starts through ScrubbedEnviron.
//
// # Why one knob rather than one per caller
//
// Stripping credentials is right; having no way to put back the two the child
// genuinely needs is the defect, and it appeared three times in a row. `gh`
// lost GH_HOST and the enterprise tokens, so `yanshi pr` stopped working
// against GitHub Enterprise. Language servers lost NETRC (gopls fetching a
// private module), npm_config_* (how pyright and typescript-language-server are
// usually installed) and SSH_AUTH_SOCK, with no Env field on LanguageServer and
// no lsp config section — the doc's suggested hatch, "name it in
// DefaultLanguages", is a table with nowhere to name it, i.e. "recompile".
// task_gate_run's command likewise loses npm_config_* the moment a gate runs
// `npm test`.
//
// Each of those has an obvious local fix, and three local fixes is three places
// to look, three things to document and three ways to be inconsistent. One
// operator-level list is the smaller thing: it is set in the same shell that
// exported the variables it names, it is by NAME so it can never be a pattern an
// attacker-supplied value satisfies, and it appears in the scrub's log line so a
// widening is visible rather than assumed.
//
// # What it deliberately does NOT widen
//
// Only ScrubbedEnviron, which is the path for programs YANSHI chose to run: MCP
// and language servers, gh, git, the skills clone, the screenshot and clipboard
// helpers, the version probes, and the gate command. PrepareEnvWithPolicy and
// ManagedEnvWithPolicy — the untrusted-program launch path behind shell_run and
// the ACP agents — are untouched, because there the allowlist is part of the
// profile's posture and a process-wide variable that quietly widened it would be
// a security control an operator could disable without editing the security
// config.
//
// The variable is removed from the child's own environment. It describes the
// host's posture and nothing downstream has any use for it; leaving it in would
// tell every child which credentials are worth asking for.
//
// Not a security hole for the same reason harden.DisableEnv is not: an attacker
// who can set this process's environment has already won. The names are by
// definition ones the operator exported on purpose.
//
// # But do not name a provider key or a cloud credential here
//
// That analogy covers WHO can set this variable. It does not cover the
// CONSEQUENCE, and the two differ: YANSHI_NO_HARDEN turns off measures that
// keep other processes out of THIS process, while a name in this list is handed
// to children — one of which, the gate command behind task_gate_run, runs a
// command string the MODEL wrote and feeds the output back to the model as
// evidence. `printenv OPENAI_API_KEY` in a gate command is a two-hop
// exfiltration of a key the operator exported for an entirely different reason.
// "The operator exported this variable on purpose" does not imply "the operator
// wants the model to read it".
//
// This is not enforced, and refusing a category here would be worse than
// useless: every name that can be put back is by definition a name the scrub
// matched, so "reject credential-looking names" rejects the entire purpose of
// the hatch (NETRC, SSH_AUTH_SOCK and npm_config_* are all matches). Separating
// "provider key" from "credential the child needs" would take a second
// blacklist of vendor prefixes, which fails open on the next vendor while
// blocking a legitimate use the operator can already see is legitimate. The
// boundary is stated instead, here and in docs/user-guide/configuration.md.
const ChildEnvAllowlistEnv = "YANSHI_ALLOW_CHILD_ENV"

// OperatorAllowedChildEnv parses ChildEnvAllowlistEnv into variable names.
//
// Empty entries are dropped so "A,,B" and a trailing comma are not names, which
// matters because the empty name would otherwise be compared against every
// variable by the folding matcher.
func OperatorAllowedChildEnv() []string {
	raw := os.Getenv(ChildEnvAllowlistEnv)
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var out []string
	for _, name := range strings.Split(raw, ",") {
		if name = strings.TrimSpace(name); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// withoutName drops the NAME=value entry for name, comparing case-insensitively
// because Windows does.
func withoutName(env []string, name string) []string {
	out := env[:0]
	for _, entry := range env {
		if key, _, ok := strings.Cut(entry, "="); ok && strings.EqualFold(key, name) {
			continue
		}
		out = append(out, entry)
	}
	return out
}

// GitHubCLICredentialEnv is the exact credential set the `gh` CLI is allowed to
// inherit, wherever it is spawned from.
//
// It lives here rather than beside either caller because there are two — the
// model-facing github_* tools and the operator-facing `yanshi pr` — and a
// second copy would drift into two different answers to "what may gh read".
// Widening it is an authorization change.
//
// GH_CONFIG_DIR is in the list even though it holds a PATH rather than a
// secret: gh's other authentication route is the token in its config
// directory, which it locates through this variable when the operator has
// relocated it. The name ends in a word the scrub treats as credential
// material, so without naming it here gh would silently look in the wrong
// place.
//
// GH_HOST is in the list for EXACTLY the same reason and was missed: the scrub
// strips GH_ and GITHUB_ as whole prefixes, so an unnamed GH_HOST is dropped and
// gh falls back to github.com. That is worse than the GH_CONFIG_DIR failure it
// mirrors — a GitHub Enterprise operator does not get "not logged in", they get
// a request aimed at the public site. The two enterprise token names are the
// credentials that go with it; `yanshi pr` is a new caller in this batch, so
// before it there was no scrub on that path at all and this list going in
// narrower than gh's own documented set was a regression rather than a
// tightening.
var GitHubCLICredentialEnv = []string{
	"GH_TOKEN", "GITHUB_TOKEN", "GH_CONFIG_DIR",
	"GH_HOST", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN",
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
