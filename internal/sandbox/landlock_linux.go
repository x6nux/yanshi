//go:build linux

package sandbox

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// This file is the Landlock syscall layer: ABI negotiation, ruleset creation,
// rule installation, and the self-restriction the re-exec helper performs on
// itself. The portable half -- what the rules ARE and how they travel in argv
// -- lives in landlockrules.go.
//
// # ABI negotiation is mandatory, not an optimisation
//
// landlock_create_ruleset rejects with EINVAL any handled_access_fs bit the
// running kernel does not know. The access bits grew across releases:
//
//	ABI 1 (5.13)  base filesystem rights
//	ABI 2 (5.19)  + REFER (cross-directory rename/link)
//	ABI 3 (6.2)   + TRUNCATE
//	ABI 4 (6.7)   + network TCP bind/connect  (not used here)
//	ABI 5 (6.10)  + IOCTL_DEV
//
// So a binary that unconditionally requested the ABI 5 bit set would fail
// outright on every kernel older than 6.10 -- that is, on most production
// Linux -- and the failure is EINVAL from ruleset creation, which reads as
// "Landlock is broken" rather than "your kernel is older than this bitmask".
// landlockABI queries the supported version and accessFSFor masks the request
// down to it. The cost is stated plainly in the capability Reason: on an
// older kernel some rights are simply not restrictable, and the sandbox is
// correspondingly weaker.

// landlockABIQuery is the flag that turns landlock_create_ruleset into a
// version query: with a NULL attr, zero size and this flag it returns the
// highest ABI version the kernel supports instead of creating anything.
const landlockABIQuery = unix.LANDLOCK_CREATE_RULESET_VERSION

// accessFSFor returns the set of filesystem access rights to handle, masked to
// what ABI version abi actually defines.
//
// "Handled" is the Landlock term for "restricted by this ruleset": any right
// in this mask that a path is not explicitly granted becomes denied. So this
// mask is the ceiling of what the sandbox can enforce, and every bit omitted
// because the kernel is too old is a right that stays UNRESTRICTED. That is
// why landlockBackend reports the negotiated version in its Reason.
func accessFSFor(abi int) uint64 {
	access := uint64(
		unix.LANDLOCK_ACCESS_FS_EXECUTE |
			unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
			unix.LANDLOCK_ACCESS_FS_READ_FILE |
			unix.LANDLOCK_ACCESS_FS_READ_DIR |
			unix.LANDLOCK_ACCESS_FS_REMOVE_DIR |
			unix.LANDLOCK_ACCESS_FS_REMOVE_FILE |
			unix.LANDLOCK_ACCESS_FS_MAKE_CHAR |
			unix.LANDLOCK_ACCESS_FS_MAKE_DIR |
			unix.LANDLOCK_ACCESS_FS_MAKE_REG |
			unix.LANDLOCK_ACCESS_FS_MAKE_SOCK |
			unix.LANDLOCK_ACCESS_FS_MAKE_FIFO |
			unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK |
			unix.LANDLOCK_ACCESS_FS_MAKE_SYM,
	)
	if abi >= 2 {
		access |= unix.LANDLOCK_ACCESS_FS_REFER
	}
	if abi >= 3 {
		access |= unix.LANDLOCK_ACCESS_FS_TRUNCATE
	}
	if abi >= 5 {
		access |= unix.LANDLOCK_ACCESS_FS_IOCTL_DEV
	}
	return access
}

// landlockReadAccess is the subset granted to read-only paths: traverse the
// directory, read files, execute programs. It is intersected with the handled
// mask before use, so it is safe to name bits unconditionally here.
const landlockReadAccess = uint64(
	unix.LANDLOCK_ACCESS_FS_EXECUTE |
		unix.LANDLOCK_ACCESS_FS_READ_FILE |
		unix.LANDLOCK_ACCESS_FS_READ_DIR,
)

// landlockDevAccess is the subset granted to the character devices: write the
// file, and read it. Notably NOT the MAKE_* or REMOVE_* rights -- a process
// that can write /dev/null has no business creating device nodes next to it.
const landlockDevAccess = uint64(
	unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
		unix.LANDLOCK_ACCESS_FS_READ_FILE,
)

// landlockABI returns the Landlock ABI version the running kernel supports, or
// an error when Landlock is unavailable.
//
// The error cases are distinguished because they call for different operator
// action: ENOSYS means the kernel predates 5.13 or was built without
// CONFIG_SECURITY_LANDLOCK; EOPNOTSUPP means it is compiled in but not enabled
// in the active LSM stack (lsm= boot parameter), which is a one-line fix the
// operator would never find from a generic "unavailable".
func landlockABI() (int, error) {
	v, _, errno := unix.Syscall(
		unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, uintptr(landlockABIQuery))
	if errno != 0 {
		switch errno {
		case unix.ENOSYS:
			return 0, errors.New("kernel does not implement landlock (needs >= 5.13 with CONFIG_SECURITY_LANDLOCK)")
		case unix.EOPNOTSUPP:
			return 0, errors.New("landlock is compiled in but disabled; add it to the lsm= boot parameter")
		default:
			return 0, fmt.Errorf("landlock version query failed: %w", errno)
		}
	}
	if v < 1 {
		return 0, fmt.Errorf("landlock reported an unusable ABI version %d", int(v))
	}
	return int(v), nil
}

// applyLandlock installs rules on the CALLING process and is irreversible.
//
// The sequence and why each step is where it is:
//
//  1. Negotiate the ABI and build the handled mask. Requesting an unknown bit
//     is EINVAL, so this must precede ruleset creation.
//
//  2. Create the ruleset. The returned fd is the ruleset under construction;
//     it is closed before returning because restrict_self copies the ruleset
//     into the task's credentials and the fd has no further use -- leaking it
//     would hand the descriptor to the exec'd target for no reason.
//
//  3. Add one PATH_BENEATH rule per path. The parent fd is opened O_PATH so
//     that opening it is not itself a read (O_PATH gets a descriptor that
//     refers to the file without accessing its contents), which matters
//     because a directory may be listed in a policy the process is not
//     otherwise entitled to read.
//
//  4. prctl(PR_SET_NO_NEW_PRIVS). MANDATORY, and Landlock refuses
//     restrict_self with EPERM without it. The reason is the whole security
//     argument for letting an unprivileged process sandbox itself: without
//     no_new_privs a confined process could exec a setuid binary and have the
//     kernel grant it privileges the sandbox was relying on it not having.
//     no_new_privs also survives execve, so the target program inherits it.
//
//  5. restrict_self. From this instruction on, the calling thread and every
//     descendant -- across execve, forever -- is subject to the ruleset. There
//     is no unrestrict operation by design.
//
// A failure at any step returns an error and applies NOTHING enforceable: a
// created-but-unrestricted ruleset is inert, and the caller must treat the
// error as fatal rather than exec the target unconfined. The helper does.
func applyLandlock(rules LandlockRules) error {
	abi, err := landlockABI()
	if err != nil {
		return err
	}
	handled := accessFSFor(abi)

	attr := unix.LandlockRulesetAttr{Access_fs: handled}
	fd, _, errno := unix.Syscall(
		unix.SYS_LANDLOCK_CREATE_RULESET,
		uintptr(unsafe.Pointer(&attr)),
		unsafe.Sizeof(attr),
		0,
	)
	if errno != 0 {
		return fmt.Errorf("landlock_create_ruleset: %w", errno)
	}
	rulesetFD := int(fd)
	defer unix.Close(rulesetFD)

	grants := []struct {
		paths  []string
		access uint64
	}{
		{rules.ReadPaths, landlockReadAccess & handled},
		{rules.WritePaths, handled},
		{rules.DevWritePaths, landlockDevAccess & handled},
	}
	for _, g := range grants {
		for _, p := range g.paths {
			if err := addLandlockPath(rulesetFD, p, g.access); err != nil {
				return err
			}
		}
	}

	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("prctl(PR_SET_NO_NEW_PRIVS): %w", err)
	}
	if _, _, errno := unix.Syscall(
		unix.SYS_LANDLOCK_RESTRICT_SELF, uintptr(rulesetFD), 0, 0,
	); errno != 0 {
		return fmt.Errorf("landlock_restrict_self: %w", errno)
	}
	return nil
}

// addLandlockPath installs one PATH_BENEATH rule granting access under path.
//
// The O_PATH|O_CLOEXEC open is deliberate on both flags. O_PATH avoids
// actually accessing the object, which is what allows a directory to be named
// in a policy without the policy-installing process needing read rights on it.
// O_CLOEXEC ensures the descriptor does not survive into the exec'd target --
// without it every policy path would leak as an open fd into the sandboxed
// program, which is both a descriptor leak and a way for the target to reach a
// directory by fd that its own ruleset was meant to gate.
//
// A missing path is reported as an error rather than skipped. Skipping is the
// caller's job and BuildLandlockRules already does it; swallowing ENOENT here
// too would mean a genuinely broken policy -- a workspace root that vanished
// between rule computation and exec -- silently produces a process with no
// write access and no explanation.
func addLandlockPath(rulesetFD int, path string, access uint64) error {
	if access == 0 {
		return nil
	}
	pathFD, err := unix.Open(path, unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("landlock: open %q: %w", path, err)
	}
	defer unix.Close(pathFD)

	rule := unix.LandlockPathBeneathAttr{
		Allowed_access: access,
		Parent_fd:      int32(pathFD),
	}
	if _, _, errno := unix.Syscall6(
		unix.SYS_LANDLOCK_ADD_RULE,
		uintptr(rulesetFD),
		uintptr(unix.LANDLOCK_RULE_PATH_BENEATH),
		uintptr(unsafe.Pointer(&rule)),
		0, 0, 0,
	); errno != 0 {
		return fmt.Errorf("landlock_add_rule %q: %w", path, errno)
	}
	return nil
}

// RunLandlockHelper is the hidden subcommand entry point. It applies the
// encoded ruleset to ITSELF and then execve()s the target, replacing this
// process. It returns only on failure.
//
// # The fail-closed contract
//
// Every error path here returns without exec'ing. That is the single most
// important property in this file: an error path that fell through to
// syscall.Exec would run the target program UNCONFINED while the parent's
// capability report says os-isolated, which is precisely the over-claim the
// sandbox layer exists to prevent. There is deliberately no "best effort"
// branch and no recovery -- a policy that cannot be applied means the command
// does not run.
//
// # Why syscall.Exec and not exec.Command
//
// syscall.Exec is execve: this process is REPLACED. No fork, no extra pid, no
// wrapper left in the tree. The pid the parent is waiting on becomes the
// target program, so exit status, signal delivery and process-group semantics
// are exactly those of running the target directly -- which is the property
// the bubblewrap backend cannot offer. Landlock restrictions and no_new_privs
// both survive execve by design, so the confinement carries across.
//
// The environment is passed through untouched via os.Environ(): this helper is
// a transparent shim and the caller already decided what the target's
// environment should be.
func RunLandlockHelper(argv []string) error {
	token, program, args, err := SplitLandlockHelperArgs(argv)
	if err != nil {
		return err
	}
	rules, err := DecodeLandlockRules(token)
	if err != nil {
		return err
	}
	if err := applyLandlock(rules); err != nil {
		return fmt.Errorf("sandbox: refusing to exec %q unconfined: %w", program, err)
	}
	return syscall.Exec(program, args, os.Environ())
}
