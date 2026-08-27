// internal/cli/doctorpolicy.go
//
// S3's diagnostic half: telling the operator that the file deciding what the
// agent may do is sitting inside the tree the agent can write to.
//
// It lives in its own file rather than in doctor.go for the reason c1wiring.go
// and profile.go give in bootstrap: doctor.go is long, and a check that
// reasons about a security posture reads better beside the argument for why
// the posture matters than appended to a list of unrelated probes.
//
// # Why this is a check and not an error
//
// Refusing to start when config.yaml is inside the work root would break every
// existing single-file deployment on upgrade, which is not a trade a security
// improvement gets to make silently. The narrowing mechanism in
// internal/config/policy.go is the fix; this check is how an operator learns
// the fix is available and that they are currently without it.
//
// # Why the profile's own globs are not consulted
//
// The risk is POSITIONAL. Whether today's profile happens to permit writing
// config.yaml is not the question: the agent that can write anywhere in the
// tree can write the profile that permits writing the profile. Reporting "your
// current fs.write does not cover it" would be a true statement about a
// property that stops being true one edit later.

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/x6nux/yanshi/internal/config"
)

// checkPolicyScope reports whether the effective permission profiles can be
// rewritten by the agent they govern.
//
// The three outcomes are:
//
//   - OK — a trusted policy file is active. The local config can only narrow
//     it, so writing to config.yaml gains the agent nothing.
//   - OK — the config file is outside the work root. Nothing the agent's FS
//     tools can reach decides its permissions.
//   - WARN — the config file is inside the work root and no trusted policy is
//     active. This is the self-escalation posture, and the message names the
//     specific remedy rather than the general problem, because a warning an
//     operator cannot act on is a warning they learn to skip.
//
// It is a warn and not a fail even under --release. The posture is the shipped
// default: failing it would make `yanshi doctor --release` red on a correctly
// configured stock installation, which would train operators to pass a flag
// that suppresses it.
func checkPolicyScope(cfg *config.Config, cfgErr error, configPath, root string) CheckResult {
	const name = "policy-scope"
	if cfgErr != nil {
		return skipped(name, cfgErr)
	}
	policyPath, configured := config.PolicyPath()

	if cfg.PolicyActive {
		msg := fmt.Sprintf("profiles governed by trusted policy %s; the working-directory config can only narrow them", policyPath)
		if len(cfg.PolicyNarrowed) > 0 {
			msg += fmt.Sprintf(" (narrowed: %s)", strings.Join(cfg.PolicyNarrowed, ", "))
		}
		return CheckResult{Name: name, Status: StatusOK, Message: msg}
	}

	if !config.ConfigInAgentWriteScope(configPath, root) {
		return CheckResult{Name: name, Status: StatusOK,
			Message: fmt.Sprintf("config %s is outside the agent work root %s", configPath, root)}
	}

	remedy := fmt.Sprintf("create %s with a `profiles:` block", policyPath)
	if !configured {
		remedy = fmt.Sprintf("set %s to a policy file outside the work root", config.PolicyEnvVar)
	}
	return CheckResult{Name: name, Status: StatusWarn,
		Message: fmt.Sprintf(
			"config %s is inside the agent work root, so an agent edit plus a restart could widen its own profile; %s",
			configPath, remedy)}
}

// checkPolicyFilePerms reports policy and config files that are writable by
// users other than the owner.
//
// A trusted policy file the whole machine can write is not trusted, and the
// failure is silent: yanshi reads it, narrows nothing it should not, and
// reports policy-scope as OK. That combination — a control that appears to be
// working while being bypassable — is the reason this is a separate check
// rather than a clause of the one above.
//
// POSIX only. Windows access control is ACL-based and a mode-bit test there
// reports whatever the Go runtime synthesises, which would be a check that
// prints a verdict it did not actually make.
func checkPolicyFilePerms(configPath string) CheckResult {
	const name = "policy-perms"
	if !posixModeBitsMeaningful() {
		return CheckResult{Name: name, Status: StatusOK,
			Message: "skipped: file mode bits are not the access-control mechanism on this platform"}
	}
	var loose []string
	for _, p := range config.PolicyProtectedPaths(configPath) {
		info, err := os.Stat(p)
		if err != nil || info.IsDir() {
			continue
		}
		// Group and other write bits. Read access is deliberately not checked:
		// these files hold no secrets, only authorization, and a world-readable
		// policy is not a weakness.
		if info.Mode().Perm()&0o022 != 0 {
			loose = append(loose, fmt.Sprintf("%s (%04o)", filepath.Base(p), info.Mode().Perm()))
		}
	}
	if len(loose) == 0 {
		return CheckResult{Name: name, Status: StatusOK, Message: "policy and config files are not group/world writable"}
	}
	return CheckResult{Name: name, Status: StatusWarn,
		Message: fmt.Sprintf("group/world-writable policy files: %s; chmod 600 them (a policy anyone can rewrite grants nothing)",
			strings.Join(loose, ", "))}
}
