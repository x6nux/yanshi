package secproc

import (
	"log/slog"

	"github.com/x6nux/yanshi/internal/netpolicy"
)

// CredentialPolicy is the launch policy derived from the spec's AllowEnv
// declaration. Factories use it to scrub the host environment baseline they
// build for the child; Launch uses it to scrub the caller-supplied spec.Env.
//
// Both halves are needed because the two environments have different origins.
// Almost no secproc caller populates Env, so the credentials a child would see
// come from the Factory's os.Environ() baseline — that is the leak this feature
// closes. But a caller that DOES populate Env (git_status passes
// GIT_CONFIG_GLOBAL, a future caller could pass anything) must not be able to
// route a credential around the scrub simply by handing it in explicitly.
func (s SecureProcessSpec) CredentialPolicy() netpolicy.CredentialPolicy {
	return netpolicy.CredentialPolicy{AllowEnv: s.AllowEnv}
}

// scrubSpecEnv strips credential entries from spec.Env under the spec's own
// allowlist, returning the updated spec and the names dropped.
//
// A spec with no Env is left exactly as it was, including its nil-ness: the
// Factory distinguishes "caller supplied nothing" from "caller supplied an
// empty environment" when it decides whether to layer entries over the host
// baseline, and replacing nil with an empty non-nil slice would quietly change
// that decision for every call site.
func scrubSpecEnv(spec SecureProcessSpec) (SecureProcessSpec, []string) {
	if len(spec.Env) == 0 {
		return spec, nil
	}
	kept, dropped := netpolicy.ScrubCredentials(spec.Env, spec.CredentialPolicy())
	spec.Env = kept
	return spec, dropped
}

// auditCredentialScrub records which variable names were withheld from a child.
//
// Names only, never values — recording the value would defeat the entire
// purpose and would land the credential in the very transcript the guard's
// auto-approval prompt warns about. The record exists because a silently
// missing credential is this feature's worst failure mode: `gh` answering "not
// logged in" on a machine where the operator plainly is, with nothing anywhere
// to explain it.
func auditCredentialScrub(spec SecureProcessSpec, dropped []string) {
	if len(dropped) == 0 {
		return
	}
	slog.Info("secproc credential scrub",
		"tool", spec.Tool,
		"program", spec.Program,
		"dropped_names", dropped,
		"allowed", spec.AllowEnv)
}
