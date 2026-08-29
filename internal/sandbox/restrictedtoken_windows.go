//go:build windows

package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// This file is the Win32 half of the Windows restricted-token backend: it
// creates the tokens, writes the workspace grant, runs the enforcement
// self-check, and hands the verdict to restrictedtoken.go.
//
// # What it delivers of W-B-25, and what it does not
//
// It delivers CreateRestrictedToken and the capability-SID ACL. The other two
// controls that item names are absent for reasons that are structural rather
// than unfinished, and windowsStructuralGapNote states both to the operator:
// a separate desktop needs an lpDesktop that Go's syscall.SysProcAttr does not
// expose, and WFP filters need administrative rights yanshi does not run with.
//
// # Why there is no deny-read half (W-B-26)
//
// Because a WRITE_RESTRICTED token cannot carry one, and the reference
// implementation says so in its own source. Restricting SIDs are a list
// separate from the token's groups, and the second access-check pass that
// consults them runs for WRITE access only. A deny-read ACE naming
// sandboxCapabilitySID therefore matches nothing: the read check is a single
// pass over the token's user and groups, and the capability SID is in neither.
//
// codex-rs/windows-sandbox-rs/src/lib.rs refuses the same combination
// explicitly — "WRITE_RESTRICTED tokens consult restricting SIDs only for
// writes, so this backend cannot make capability-SID deny-read ACLs
// authoritative" — and routes deny-read to an elevated backend that creates a
// real local user account (see its hide_users.rs) precisely to obtain a
// principal that IS in the token's group list. That needs administrator.
//
// Writing the ACEs anyway would be worse than omitting them: it would mutate
// the DACLs of the operator's credential directories while denying nobody, and
// the report would tell them their credentials were protected. So this backend
// claims the write boundary it has and says nothing about reads.

// Flags for CreateRestrictedToken. Not declared by x/sys/windows.
//
// WRITE_RESTRICTED is the one that shapes everything else here: with it, the
// restricting SIDs are consulted for write access only, so the child keeps the
// read rights it needs to load a DLL and run a compiler while its writes are
// confined to the objects whose DACL names the capability SID. Without it the
// restricting SIDs apply to reads too, and a child that cannot read
// kernel32.dll does not start — which would need grants across the whole system
// directory, i.e. administrator.
//
// DISABLE_MAX_PRIVILEGE drops every privilege except SeChangeNotify. A sandbox
// whose child kept SeBackupPrivilege could read past any DACL, which would make
// the workspace boundary decorative.
const (
	tokenDisableMaxPrivilege = 0x1
	tokenWriteRestricted     = 0x8
)

var (
	modadvapi32               = windows.NewLazySystemDLL("advapi32.dll")
	procCreateRestrictedToken = modadvapi32.NewProc("CreateRestrictedToken")
)

// sandboxReadOnlyCapabilitySID is the principal a read-only child runs under.
//
// It is granted on NOTHING, which is what makes tier=read-only mean "no writes
// anywhere" rather than "no writes outside the workspace". A second SID is
// needed rather than reusing sandboxCapabilitySID because the workspace grant
// is an ACE on disk: a read-only child holding the same SID would inherit that
// grant and be able to write the workspace, silently collapsing two tiers into
// one.
//
// Same authority and the same reasoning as sandboxCapabilitySID: S-1-15-3 can
// never be an account, so a fixed constant there cannot resolve to a principal
// that can log in.
const sandboxReadOnlyCapabilitySID = "S-1-15-3-1024-2657981914-3390417052-" +
	"1284763186-2073994558-3821206471-1520788935-2296437002-1735021160"

// sandboxProbeCapabilitySID is used only by the construction-time self-check.
//
// The probe must observe a denial, so it needs a granted directory and an
// ungranted one. It cannot use the real capability SID for that: the scratch
// paths granted to it include the system temp directory, which is where the
// probe's own scratch pair would live, so "outside" would already be writable
// and the denial assertion would be vacuous. A third SID granted on exactly one
// throwaway directory makes the two sides genuinely different.
const sandboxProbeCapabilitySID = "S-1-15-3-1024-1997364028-2438205905-" +
	"1120643971-3496612577-2054993540-4152730236-1793065224-2611548417"

// createRestrictedToken builds a WRITE_RESTRICTED primary token whose only
// restricting SID is sid.
//
// # Why the resulting token can be used without SeAssignPrimaryToken
//
// CreateProcessAsUser normally demands SE_ASSIGNPRIMARYTOKEN_NAME and
// SE_INCREASE_QUOTA_NAME, which an unelevated process does not hold. The
// documented exception is exactly this case: a token that is a RESTRICTED
// VERSION OF THE CALLER'S OWN primary token needs neither. That is the whole
// reason this backend can exist unelevated, and it is why the base token is
// opened from the current process rather than logged on separately.
func createRestrictedToken(sidStr string) (windows.Token, error) {
	sid, err := windows.StringToSid(sidStr)
	if err != nil {
		return 0, fmt.Errorf("the sandbox capability SID %q is not a valid SID: %w", sidStr, err)
	}
	var base windows.Token
	// TOKEN_ADJUST_DEFAULT is required because CreateRestrictedToken derives the
	// new token's default DACL from this one; TOKEN_ASSIGN_PRIMARY is what makes
	// the result usable as a process token rather than only for impersonation.
	access := uint32(windows.TOKEN_DUPLICATE | windows.TOKEN_ASSIGN_PRIMARY |
		windows.TOKEN_QUERY | windows.TOKEN_ADJUST_DEFAULT)
	if err := windows.OpenProcessToken(windows.CurrentProcess(), access, &base); err != nil {
		return 0, fmt.Errorf("OpenProcessToken failed: %w", err)
	}
	defer base.Close()

	restrict := []windows.SIDAndAttributes{{Sid: sid, Attributes: 0}}
	var out windows.Token
	// A lazy proc rather than a typed wrapper: x/sys/windows does not declare
	// CreateRestrictedToken. The argument list is nine words with four unused
	// pairs (no SIDs to disable, no privileges to delete beyond what
	// DISABLE_MAX_PRIVILEGE already removes).
	//
	// Both unsafe.Pointer conversions are written INLINE in the call's argument
	// list. That is not a style choice: it is the one form the unsafe.Pointer
	// rules guarantee keeps the referents alive and unmoved for the duration of
	// the call, and hoisting either into a local uintptr would make the pointer
	// collectable while the kernel is reading through it.
	r, _, e := procCreateRestrictedToken.Call(
		uintptr(base),
		uintptr(tokenWriteRestricted|tokenDisableMaxPrivilege),
		0, 0, // DisableSidCount, SidsToDisable
		0, 0, // DeletePrivilegeCount, PrivilegesToDelete
		uintptr(len(restrict)),
		uintptr(unsafe.Pointer(&restrict[0])),
		uintptr(unsafe.Pointer(&out)),
	)
	if r == 0 {
		return 0, fmt.Errorf("CreateRestrictedToken failed: %w", e)
	}
	return out, nil
}

// setCapabilityGrant adds, or removes, an inheritable full-access ACE for sid
// on path.
//
// # Why the DACL is read, merged and written back rather than replaced
//
// SetNamedSecurityInfo takes a whole DACL, so building one from the single
// entry would DELETE every existing ACE on the object — on a workspace root
// that is the operator's own access. ACLFromEntries with the current DACL as
// the merge base is what turns "set this ACL" into "add this ACE", and it also
// makes the operation idempotent: re-applying the same trustee replaces its
// entry instead of appending a second one, which is what keeps a DACL from
// growing by one ACE per run until it hits its 64KB ceiling.
//
// # Why inheritance is on
//
// SUB_CONTAINERS_AND_OBJECTS_INHERIT is CONTAINER_INHERIT_ACE|OBJECT_INHERIT_ACE.
// Without it the grant would apply to the workspace directory itself and to
// nothing inside it, so the child could rename the root and not write a single
// file — a sandbox that appears to work until the first build.
//
// GENERIC_ALL is not over-broad despite how it reads. The restricting-SID pass
// is an AND with the normal pass, so what this actually grants is bounded above
// by the rights the operator already had on the object; naming less here would
// subtract from those rather than from anything the sandbox is containing.
func setCapabilityGrant(path string, sid *windows.SID, grant bool) error {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("GetNamedSecurityInfo(%s) failed: %w", path, err)
	}
	current, _, err := sd.DACL()
	if err != nil {
		return fmt.Errorf("reading the DACL of %s failed: %w", path, err)
	}
	mode := windows.ACCESS_MODE(windows.GRANT_ACCESS)
	if !grant {
		mode = windows.REVOKE_ACCESS
	}
	entry := windows.EXPLICIT_ACCESS{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        mode,
		Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_UNKNOWN,
			TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}
	merged, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{entry}, current)
	if err != nil {
		return fmt.Errorf("building the merged DACL for %s failed: %w", path, err)
	}
	// DACL_SECURITY_INFORMATION alone, without PROTECTED: the object keeps
	// whatever it inherits from its parent. Protecting it here would silently
	// sever inheritance on the operator's project directory, and unprotecting it
	// again on Close is not something a crash can be relied upon to do.
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION, nil, nil, merged, nil); err != nil {
		return fmt.Errorf("SetNamedSecurityInfo(%s) failed: %w", path, err)
	}
	return nil
}

// restrictedTokens holds the per-tier tokens and the grants written for them.
//
// One set per sandbox, built at construction and immutable afterwards except
// for Close. The mutex guards only the teardown, which is reachable both from
// the shutdown path and from a deferred cleanup while a Prepare may still be
// handing the token out.
type restrictedTokens struct {
	mu     sync.Mutex
	tokens map[AccessTier]windows.Token
	// granted records the paths whose DACL carries the write grant, so Close
	// can take it back off. It is a list rather than being recomputed because
	// the config could name a path that stopped existing mid-session, and
	// revoking what was actually written is the only version that is correct.
	granted []string
	sid     *windows.SID
	closed  bool
}

// tokenFor returns the token a command at tier must run under, or 0 for none.
//
// FullAccess gets no token on purpose: the tier means "no write restriction",
// and handing it a restricted token would be a restriction. This mirrors the
// landlock backend, whose FullAccess ruleset grants write on "/".
func (r *restrictedTokens) tokenFor(tier AccessTier) windows.Token {
	if r == nil || tier == FullAccess {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0
	}
	return r.tokens[tier]
}

// Close revokes every grant and closes every token.
//
// Revocation is best-effort and errors are joined rather than returned on the
// first failure: a path that vanished mid-session must not stop the remaining
// ACEs from being taken off, and leaving a grant behind is the direction that
// matters — it would keep naming a principal that a later yanshi run holds.
func (r *restrictedTokens) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	var firstErr error
	for _, p := range r.granted {
		if err := setCapabilityGrant(p, r.sid, false); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	r.granted = nil
	for _, t := range r.tokens {
		if t != 0 {
			_ = t.Close()
		}
	}
	r.tokens = nil
	return firstErr
}

// newRestrictedTokens builds the token set for cfg, proving the mechanism works
// on this host before it is used for anything real.
//
// # Why the self-check runs on throwaway directories first
//
// The same reason the landlock probe uses its own policy rather than the
// production one: a check that ran against the operator's workspace would have
// to write a file into their project to prove writes work, and would have
// nowhere safe to prove that a write is REFUSED. Two directories this function
// creates and deletes give both observations with no side effect outside the
// temp directory.
//
// # Why any failure returns nil rather than a partially-working sandbox
//
// Every path out of here that is not fully successful reports a probe that is
// not enforcing, which makes windowsReport keep Effective=DegradedHostGuard and
// Enforced=false. That is the same posture Windows had before this backend
// existed, so a mistake in the Win32 below degrades yanshi to its previous
// behaviour instead of breaking every spawn — which matters more than usual
// here, because this code cannot be executed on the machine it was written on.
func newRestrictedTokens(cfg Config) (*restrictedTokens, restrictedTokenProbe) {
	if cfg.Tier == FullAccess {
		return nil, restrictedTokenProbe{}
	}
	p := restrictedTokenProbe{Attempted: true}

	workspace := ResolvePath(cfg.WorkspaceRoot)
	if workspace == "" {
		p.Detail = "no workspace root is configured, so there is nothing to grant and a " +
			"restricted token would refuse every write including the ones the operator wants"
		return nil, p
	}
	if st, err := os.Stat(workspace); err != nil || !st.IsDir() {
		p.Detail = fmt.Sprintf("the configured workspace root %s is not an existing "+
			"directory, so the grant has nothing to attach to", workspace)
		return nil, p
	}

	if detail, ok := probeRestrictedToken(); !ok {
		p.Detail = detail
		return nil, p
	}
	p.TokenCreated, p.WorkspaceWritable, p.OutsideDenied = true, true, true

	set, err := buildRestrictedTokens(workspace)
	if err != nil {
		p.Detail = err.Error()
		return nil, p
	}
	p.ACLsApplied = true
	return set, p
}

// buildRestrictedTokens creates both tier tokens and writes the real grants.
//
// Both tokens are built regardless of cfg.Tier because the tier a command runs
// at is PER INVOCATION: secproc's UseSandboxTier can ask for read-only on a
// workspace-write sandbox, and the landlock backend already honours spec.Tier
// rather than the configured one. Building only the configured tier's token
// would silently run those invocations unconfined.
//
// The grants are written for the write capability SID only. The read-only token
// restricts to a SID that is granted nowhere, which is what makes read-only mean
// "no writes anywhere" rather than "no writes outside the workspace" — and it is
// why the two tiers need two SIDs rather than two tokens over one.
func buildRestrictedTokens(workspace string) (*restrictedTokens, error) {
	writeSID, err := windows.StringToSid(sandboxCapabilitySID)
	if err != nil {
		return nil, fmt.Errorf("the sandbox capability SID is not parseable: %w", err)
	}
	set := &restrictedTokens{tokens: map[AccessTier]windows.Token{}, sid: writeSID}

	for _, path := range append([]string{workspace}, windowsScratchPaths()...) {
		if err := setCapabilityGrant(path, writeSID, true); err != nil {
			// Take back whatever landed before giving up: a half-written grant
			// set is an ACE on the operator's disk with no sandbox using it.
			_ = set.Close()
			return nil, fmt.Errorf("could not grant the sandbox capability on %s: %w", path, err)
		}
		set.granted = append(set.granted, path)
	}

	for tier, sid := range map[AccessTier]string{
		WorkspaceWrite: sandboxCapabilitySID,
		ReadOnly:       sandboxReadOnlyCapabilitySID,
	} {
		tok, err := createRestrictedToken(sid)
		if err != nil {
			_ = set.Close()
			return nil, fmt.Errorf("could not create the %s token: %w", tier, err)
		}
		set.tokens[tier] = tok
	}
	return set, nil
}

// restrictedProbeTimeout bounds the enforcement self-check.
//
// Same rationale as the job object's: bootstrap constructs this backend
// synchronously, so an unbounded wait is a hung startup, and the fail-closed
// answer to "the mechanism did not respond" is to report degraded.
const restrictedProbeTimeout = 30 * time.Second

// probeRestrictedToken determines whether WRITE_RESTRICTED really confines on
// this host, returning a reason when it does not.
//
// # Why it spawns processes
//
// Because the API return codes cannot answer the question. CreateRestrictedToken
// and SetNamedSecurityInfo both succeed on a host where the resulting token then
// writes wherever it likes — which is what a policy that strips restricted
// tokens, or a filesystem that ignores DACLs (a FAT32 or exFAT volume, or a
// network share whose server does not honour them), looks like. On such a
// volume every observation in the setup is green and nothing is enforced.
//
// So the probe asserts the two OBSERVABLE outcomes that matter, and needs both:
// a child under the token writes inside the one granted directory, and the same
// child CANNOT write a sibling of it. Asserting only the denial passes on a
// token that refuses everything, which is a sandbox that breaks every command;
// asserting only the write passes on a token that restricts nothing.
//
// # Why a throwaway capability SID
//
// The real capability SID is granted on the system temp directory, which is
// where these directories live. Probing with it would make "outside" already
// writable and the denial assertion vacuous — a green probe proving nothing,
// which is the exact failure mode this package keeps re-learning.
func probeRestrictedToken() (string, bool) {
	base, err := os.MkdirTemp("", "yanshi-sandbox-probe")
	if err != nil {
		return fmt.Sprintf("cannot create a directory to verify confinement in: %v", err), false
	}
	defer os.RemoveAll(base)

	inside := filepath.Join(base, "granted")
	outside := filepath.Join(base, "ungranted")
	for _, d := range []string{inside, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Sprintf("cannot create the probe directory %s: %v", d, err), false
		}
	}

	sid, err := windows.StringToSid(sandboxProbeCapabilitySID)
	if err != nil {
		return fmt.Sprintf("the probe capability SID is not parseable: %v", err), false
	}
	if err := setCapabilityGrant(inside, sid, true); err != nil {
		return fmt.Sprintf("a capability grant could not be written to a directory this "+
			"process just created (%v); the ACL mechanism this backend rests on does not "+
			"work here — a FAT/exFAT volume or a share that ignores DACLs would look like "+
			"this", err), false
	}

	tok, err := createRestrictedToken(sandboxProbeCapabilitySID)
	if err != nil {
		return fmt.Sprintf("could not create a restricted token: %v", err), false
	}
	defer tok.Close()

	if err := runUnderToken(tok, inside, filepath.Join(inside, "written")); err != nil {
		return fmt.Sprintf("a child holding a restricted token could not write into the "+
			"one directory granted to it (%v); the token would refuse every command "+
			"rather than confine it", err), false
	}
	leak := filepath.Join(outside, "leaked")
	if err := runUnderToken(tok, inside, leak); err == nil {
		return "a child holding a restricted token wrote outside the directory granted " +
			"to it; WRITE_RESTRICTED does not confine on this host", false
	}
	if _, err := os.Stat(leak); err == nil {
		// The command reported failure and the file exists anyway. That is a
		// stranger state than a plain permission failure and must not be read
		// as a denial: something wrote it.
		return "a child holding a restricted token reported failure but the file it was " +
			"denied appeared anyway; the observation cannot be attributed to the token", false
	}
	return "", true
}

// runUnderToken runs one redirect under tok and reports whether it succeeded.
//
// # Why cmd.exe and a redirect rather than a purpose-built helper
//
// The helper would have to be this binary, and this binary is what the sandbox
// exists to confine — the landlock backend refuses to wrap itself for exactly
// that reason. A shell redirect is the smallest thing that performs a real
// CreateFile through the same code path any tool would.
//
// TEMP and TMP point at the granted directory rather than the host's. Without
// that, cmd.exe's own scratch writes would land in an ungranted location and the
// probe would measure "the shell could not start" instead of "the write was
// refused" — the two produce the same exit status and the wrong one would make
// this backend degrade on every host.
func runUnderToken(tok windows.Token, tempDir, target string) error {
	ctx, cancel := context.WithTimeout(context.Background(), restrictedProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, comspec(), "/c", `(echo yanshi)>"`+target+`"`)
	cmd.SysProcAttr = &windows.SysProcAttr{
		Token:         syscall.Token(tok),
		CreationFlags: windows.CREATE_NO_WINDOW,
	}
	// A minimal environment rather than the operator's: it cannot change the
	// result, and inheriting theirs would push credentials into a child for no
	// reason. SystemRoot and ComSpec are what the loader and the shell need;
	// PATH is deliberately absent because the command names no program.
	cmd.Env = []string{
		"SystemRoot=" + os.Getenv("SystemRoot"),
		"windir=" + os.Getenv("windir"),
		"ComSpec=" + comspec(),
		"TEMP=" + tempDir,
		"TMP=" + tempDir,
	}
	return cmd.Run()
}
