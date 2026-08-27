//go:build linux

package sandbox

import "fmt"

// This file is the Linux backend selector. Two enforcement mechanisms are
// tried in order and the first one that PROVES it works is used; when neither
// does, the sandbox degrades honestly rather than claiming isolation it does
// not have.
//
// # Why an ordered chain and not a single backend
//
// The two mechanisms are not substitutes with different syntax -- they cover
// genuinely different amounts of the threat model, and which is available is a
// property of the host that yanshi cannot control:
//
//	bubblewrap   filesystem + pid + network + ipc + uts namespaces.
//	             Requires unprivileged user namespaces, which a large number
//	             of hardened and containerised hosts disable.
//	landlock     filesystem ONLY. Requires kernel >= 5.13 with the LSM
//	             enabled, needs no privileges and no user namespaces, and so
//	             works on many of the hosts where bubblewrap cannot.
//
// bubblewrap is preferred because it is strictly stronger. Landlock is a real
// fallback rather than a consolation prize: filesystem confinement is the
// majority of what this layer is asked for, and having it is meaningfully
// different from having nothing. But the difference is NOT cosmetic, and the
// capability Reason spells it out -- see the landlock branch below.

// newPlatformSandbox builds the Linux backend, preferring bubblewrap and
// falling back to Landlock, degrading honestly when neither enforces.
//
// Each constructor runs its own real enforcement probe and returns a reason
// string when it declines, so the degraded report names BOTH failures rather
// than a generic "no sandbox available". That matters operationally: "bwrap
// not on PATH" and "bwrap present but user namespaces disabled" call for
// completely different fixes, and an operator who sees only the second-level
// message would install a package that is already installed.
func newPlatformSandbox(cfg Config) Sandbox {
	bwrapSandbox, bwrapReason := newBubblewrap(cfg)
	if bwrapSandbox != nil {
		return bwrapSandbox
	}

	landlockSandbox, landlockReason := newLandlock(cfg)
	if landlockSandbox != nil {
		return landlockSandbox
	}

	return &degraded{report: CapabilityReport{
		Platform:    "linux",
		Requested:   cfg.Tier,
		Effective:   DegradedHostGuard,
		Backend:     "linux-no-backend",
		Reason:      fmt.Sprintf("bubblewrap unavailable: %s; landlock unavailable: %s", bwrapReason, landlockReason),
		Enforced:    false,
		CanKillTree: false,
	}}
}
