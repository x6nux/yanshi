package sandbox

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

// This file holds the Landlock rule model, its computation from a tier, and
// the argv encoding used to hand it to the re-exec helper. Like sbpl.go and
// bwrapargs.go it is platform-neutral on purpose: everything here is a pure
// function, so it compiles and is tested on darwin and windows CI legs even
// though only a Linux kernel can enforce the result.
//
// # Why there is an encoding at all
//
// Landlock must be applied by the process that will be confined, AFTER fork
// and BEFORE execve. Go cannot run arbitrary code in that window: between
// fork and exec the child holds a copy of the parent's address space but only
// one thread, so the Go runtime is not in a usable state and anything that
// allocates, takes a lock, or touches the scheduler can deadlock. os/exec's
// SysProcAttr covers the handful of operations the runtime implements in
// assembly for exactly that window; landlock_add_rule loops over paths and
// opens file descriptors, and is not one of them.
//
// The way out is to make the confined process apply the rules to ITSELF while
// it is still a normal, fully-functional Go program:
//
//	parent: exec /proc/self/exe <helper-arg> <encoded rules> -- <prog> <args>
//	child:  decode rules -> prctl(NO_NEW_PRIVS) -> landlock_restrict_self()
//	        -> syscall.Exec(prog, args)
//
// The child is yanshi's own binary running a hidden subcommand. It confines
// itself and then REPLACES itself with the target via execve, so no extra
// process remains: unlike the bubblewrap backend, pid and exit-status
// semantics are identical to running the target directly, because after the
// exec it IS the target. Landlock restrictions survive execve by design, and
// no_new_privs makes them unremovable.
//
// # Why the rules travel in argv rather than in an environment variable
//
// argv and the environment are equally visible to other processes on the host
// (both are readable from /proc/PID/{cmdline,environ} by the same uid), so
// this is not a confidentiality decision -- the rules are not secret. It is a
// reliability one: the environment is routinely rewritten by the layers
// between here and the exec (shell wrappers, secproc's own env handling,
// direnv-style tooling), and a rules variable that gets dropped would produce
// a child that confines itself to NOTHING and execs the target anyway. argv
// cannot be silently pruned. The helper additionally refuses to run when the
// rules operand is missing or malformed rather than defaulting to permissive.

// landlockHelperArg is the hidden subcommand token the parent passes as
// argv[1] when re-execing itself to apply Landlock.
//
// The leading underscores are load-bearing in two independent ways. They keep
// the token out of cmd/gendocs' dispatch inventory, whose regex matches only
// `case "<lowercase>":`, so wiring this helper cannot break
// TestSubcommandListMatchesDispatch or silently enlarge the documented
// subcommand surface. And they mark it, to anyone reading a process listing,
// as an internal re-exec rather than a command an operator is meant to type.
const landlockHelperArg = "__landlock_exec"

// LandlockHelperArg is the argv[1] token that routes to the Landlock re-exec
// helper. It is exported so cmd/yanshi can dispatch on the constant rather
// than on a copied string literal; a copy would drift and the failure mode of
// drift here is that the helper is never reached, the probe fails, and the
// backend degrades for a reason nobody can find.
func LandlockHelperArg() string { return landlockHelperArg }

// LandlockRules is the complete confinement policy handed to one process
// through the re-exec helper. Paths are absolute and already symlink-resolved
// by the caller.
//
// The three path lists are not a hierarchy: WritePaths entries are granted read
// rights too, because a directory a process can write but not read is not a
// usable workspace. ReadPaths therefore only needs to name what is read-only.
//
// It keeps its name after growing the two seccomp fields because Landlock is
// what the helper is FOR — the syscall filter is a second layer the same helper
// installs in the same window, not a separate mechanism with a separate
// delivery path. Splitting the token in two would mean a second argv operand,
// a second grammar, and two ways for the parent and the child to disagree.
type LandlockRules struct {
	// ReadPaths get execute + read-file + read-dir.
	ReadPaths []string `json:"r,omitempty"`
	// WritePaths get every filesystem right the running kernel supports.
	WritePaths []string `json:"w,omitempty"`
	// DevWritePaths get write-file only, without the directory-mutation and
	// removal rights WritePaths carries. See BuildLandlockRules.
	DevWritePaths []string `json:"d,omitempty"`

	// Seccomp asks the helper to install the syscall filter before exec'ing the
	// target, and to REFUSE the exec if it cannot.
	//
	// The parent decides, because the parent is where the availability probe
	// ran and where the capability report was written. A child that decided for
	// itself could silently answer "seccomp is not available here" and exec
	// anyway, while the report the operator reads says the filter is in force.
	Seccomp bool `json:"s,omitempty"`

	// NetDeny selects the network half of that filter: socket(2) and
	// socketpair(2) are refused for every address family except AF_UNIX. It is
	// separate from Seccomp because the unconditional denials (ptrace,
	// process_vm_*, io_uring) apply whether or not egress is restricted.
	NetDeny bool `json:"n,omitempty"`
}

// landlockDevWritePaths are the character devices a confined process must be
// able to write to at EVERY tier, including read-only.
//
// This list is the Landlock counterpart of the readOnlyDevices carve-out in
// sbpl.go and of bubblewrap's synthetic --dev, and it exists for a concrete
// reason rather than symmetry: a strictly read-only Landlock ruleset denies
// LANDLOCK_ACCESS_FS_WRITE_FILE everywhere, and `cmd >/dev/null` is a write.
// So without this, the most common shell idiom in existence fails inside a
// "read-only" sandbox, and it fails as EACCES on /dev/null -- an error message
// that sends whoever reads it looking in entirely the wrong place.
//
// /dev/tty is included so an interactive child can still reach its terminal;
// /dev/stdout and friends are NOT listed because they are symlinks into
// /proc/self/fd and Landlock evaluates the resolved target, which is the
// already-open descriptor the parent handed down and which needs no rule.
var landlockDevWritePaths = []string{
	"/dev/null",
	"/dev/zero",
	"/dev/full",
	"/dev/random",
	"/dev/urandom",
	"/dev/tty",
}

// BuildLandlockRules computes the filesystem policy for one invocation.
//
// The tiers:
//
//   - ReadOnly: read+execute on "/", write on the device nodes only.
//   - WorkspaceWrite: the above, plus full rights on WorkspaceRoot and on each
//     scratch path.
//   - FullAccess: full rights on "/". This is VACUOUS -- see
//     LandlockRestrictsNothing -- and is represented explicitly rather than by
//     returning an empty ruleset, because an empty ruleset and an
//     everything-allowed ruleset are opposites: an empty one denies all.
//
// exists filters paths that are not present. Unlike bubblewrap, where a
// missing bind source fails the entire launch, landlock_add_rule on a missing
// path returns ENOENT and the helper could in principle continue. It does not:
// filtering here keeps the helper's error handling strict (any add_rule
// failure is fatal there), so a typo in a scratch path cannot be swallowed as
// "one rule quietly did not apply" and leave a workspace unwritable for
// reasons invisible in the argv.
func BuildLandlockRules(in BwrapInput, exists func(string) bool) LandlockRules {
	if exists == nil {
		exists = func(string) bool { return true }
	}
	rules := LandlockRules{}

	if in.Tier == FullAccess {
		rules.WritePaths = []string{"/"}
		return rules
	}

	rules.ReadPaths = []string{"/"}
	for _, d := range landlockDevWritePaths {
		if exists(d) {
			rules.DevWritePaths = append(rules.DevWritePaths, d)
		}
	}

	if in.Tier != WorkspaceWrite {
		return rules
	}

	seen := map[string]bool{}
	for _, p := range append([]string{in.WorkspaceRoot}, in.ScratchPaths...) {
		clean, ok := sanitizeBindPath(p)
		if !ok || seen[clean] || !exists(clean) {
			continue
		}
		seen[clean] = true
		rules.WritePaths = append(rules.WritePaths, clean)
	}
	return rules
}

// LandlockRestrictsNothing reports whether the ruleset leaves the filesystem
// entirely unrestricted, so the capability Reason can disclose it.
//
// Two shapes qualify, and as with BwrapRestrictsNothing the second is why this
// is a function and not a tier comparison:
//
//  1. A WritePaths entry of "/". Every path on the host is then fully
//     writable and the ruleset denies nothing at all.
//
//  2. No paths of any kind. That is NOT vacuous in the permissive direction --
//     an empty Landlock ruleset denies everything -- so it is excluded here
//     deliberately and callers must not treat "nothing to report" as "nothing
//     enforced". It is called out because the two empty-looking states are
//     opposites and conflating them is how a maximally-restrictive sandbox
//     gets reported as an absent one.
func LandlockRestrictsNothing(r LandlockRules) bool {
	for _, p := range r.WritePaths {
		if p == "/" {
			return true
		}
	}
	return false
}

// EncodeLandlockRules renders rules as a single argv-safe token.
//
// base64 of JSON, rather than a delimiter-joined path list, because paths may
// legally contain every byte except NUL and '/' -- including ':', '|', newline
// and the quote characters any hand-rolled separator scheme would pick. A
// workspace named "a:b" must not be able to smuggle an extra rule into the
// child's policy by being split on the separator, and RawURLEncoding produces
// a token with no shell-significant characters at all, so the encoding is also
// safe if a future caller routes this through something that does interpret
// its argv.
func EncodeLandlockRules(r LandlockRules) (string, error) {
	raw, err := json.Marshal(r)
	if err != nil {
		return "", fmt.Errorf("sandbox: encode landlock rules: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// DecodeLandlockRules parses a token produced by EncodeLandlockRules and
// validates every path in it.
//
// Validation is not belt-and-braces, it is the helper's only defence. The
// helper runs as a normal subcommand of the yanshi binary and will grant
// itself exactly the rights this token names, so a malformed or hostile token
// must produce an ERROR and never a partially-applied or empty policy. Each
// path must be absolute and already clean; a relative path would be resolved
// against the helper's working directory, and an uncleaned one ("/ws/../etc")
// would grant rights somewhere other than where it reads as granting them.
//
// Rejecting rather than cleaning is deliberate: cleaning here would mean the
// policy the parent computed and the policy the child applied could differ
// while both look correct in isolation, which is precisely the class of
// mismatch that makes a sandbox bug unattributable.
func DecodeLandlockRules(token string) (LandlockRules, error) {
	var r LandlockRules
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(token))
	if err != nil {
		return LandlockRules{}, fmt.Errorf("sandbox: landlock rules token is not valid base64: %w", err)
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		// LandlockRules{}, not r. encoding/json populates the destination as it
		// streams, so a token that is well-formed up to an unknown field leaves
		// r holding every rule parsed before the error -- a PARTIAL policy
		// returned alongside a non-nil error. Every caller here checks the
		// error, but handing back a half-built ruleset is the precise shape of
		// the accident this decoder exists to prevent, and one future caller
		// that logs-and-continues would confine a process to a policy nobody
		// computed. Caught by TestDecodeLandlockRulesRejectsMalformed, which
		// asserts a rejected token yields no rules rather than only asserting
		// that it errors.
		return LandlockRules{}, fmt.Errorf("sandbox: landlock rules token is not valid JSON: %w", err)
	}
	for _, group := range [][]string{r.ReadPaths, r.WritePaths, r.DevWritePaths} {
		for _, p := range group {
			if err := validateLandlockPath(p); err != nil {
				return LandlockRules{}, err
			}
		}
	}
	return r, nil
}

// validateLandlockPath enforces the absolute-and-already-clean invariant.
func validateLandlockPath(p string) error {
	if p == "" {
		return fmt.Errorf("sandbox: landlock rules contain an empty path")
	}
	if !strings.HasPrefix(p, "/") {
		return fmt.Errorf("sandbox: landlock rule path %q is not absolute", p)
	}
	if filepath.Clean(p) != p {
		return fmt.Errorf("sandbox: landlock rule path %q is not in cleaned form", p)
	}
	return nil
}

// SplitLandlockHelperArgs parses the helper's argv into the rules token and
// the target command, and is the single definition of that argv grammar:
//
//	<exe> __landlock_exec <token> -- <program> [args...]
//
// argv is the FULL vector including argv[0]. The `--` is required rather than
// inferred from position so that a target program whose path begins with a
// dash, or a token that happens to look like a path, cannot shift the
// boundary. Everything after the first `--` is the target, verbatim, including
// any further `--`.
//
// It lives here rather than in the helper's own file so the grammar is
// testable on any platform, and so the parent that BUILDS this argv and the
// child that PARSES it are checked against one shared definition -- the parent
// constructs it in landlockCommand and any divergence between the two would
// show up as a probe failure with no indication of which side was wrong.
func SplitLandlockHelperArgs(argv []string) (token string, program string, args []string, err error) {
	if len(argv) < 2 || argv[1] != landlockHelperArg {
		return "", "", nil, fmt.Errorf("sandbox: argv is not a %s invocation", landlockHelperArg)
	}
	if len(argv) < 3 {
		return "", "", nil, fmt.Errorf("sandbox: %s requires a rules token", landlockHelperArg)
	}
	token = argv[2]
	sep := -1
	for i := 3; i < len(argv); i++ {
		if argv[i] == "--" {
			sep = i
			break
		}
	}
	if sep < 0 {
		return "", "", nil, fmt.Errorf("sandbox: %s requires a -- separator before the target program", landlockHelperArg)
	}
	rest := argv[sep+1:]
	if len(rest) == 0 {
		return "", "", nil, fmt.Errorf("sandbox: %s requires a target program after --", landlockHelperArg)
	}
	return token, rest[0], rest, nil
}
