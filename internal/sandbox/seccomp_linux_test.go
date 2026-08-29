//go:build linux

package sandbox

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// seccompHelperEnv routes the test binary into the subprocess that installs a
// real filter.
//
// A filter is irreversible and, with TSYNC, applies to every thread — so
// installing one in the test runner would confine the rest of the suite and
// every child it spawns. The behaviour is therefore measured in a child that is
// allowed to be a one-way street, the same shape internal/harden uses.
const seccompHelperEnv = "YANSHI_SECCOMP_TEST_HELPER"

// TestSeccompHelper is the subprocess half: it installs the filter and reports
// what each probed syscall answered.
//
// It probes through the raw syscalls rather than through net.Dial, because
// net.Dial on a blocked socket() produces an error string that a
// DNS failure or a refused connection could equally produce. The errno is the
// thing under test.
func TestSeccompHelper(t *testing.T) {
	if os.Getenv(seccompHelperEnv) == "" {
		// THIS IS A NON-VERDICT, NOT A PASS: unlike the requireEnforcing* skips
		// elsewhere in this package, this one is unconditional — it fires on
		// every top-level `go test` run regardless of host capability, because
		// this function is only ever a real test when runSeccompHelper below
		// re-execs the binary with seccompHelperEnv set. Its own PASS/SKIP says
		// nothing about whether the filter enforces; that verdict comes from
		// the parent tests (TestSeccompDeniesNonUnixSocketsUnderNetworkDeny and
		// TestSeccompAlwaysDeniesMemoryAndUringSyscalls) reading this process's
		// stdout.
		t.Skip("subprocess helper; driven by the tests below")
	}
	netDeny := os.Getenv(seccompHelperEnv) == "netdeny"
	if err := applySeccomp(netDeny); err != nil {
		os.Stdout.WriteString("APPLY-FAILED " + err.Error() + "\n")
		os.Exit(0)
	}

	report := func(label string, err error) {
		if err == nil {
			os.Stdout.WriteString(label + "=ok\n")
			return
		}
		os.Stdout.WriteString(label + "=" + err.Error() + "\n")
	}

	if fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM, 0); err == nil {
		_ = unix.Close(fd)
		report("unix-socket", nil)
	} else {
		report("unix-socket", err)
	}
	if fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0); err == nil {
		_ = unix.Close(fd)
		report("inet-socket", nil)
	} else {
		report("inet-socket", err)
	}
	if fd, err := unix.Socket(unix.AF_INET6, unix.SOCK_DGRAM, 0); err == nil {
		_ = unix.Close(fd)
		report("inet6-socket", nil)
	} else {
		report("inet6-socket", err)
	}
	// PTRACE_PEEKDATA against pid 0, which is not a valid target: permitted it
	// answers ESRCH, filtered it answers EPERM, and either way nothing is
	// traced. PTRACE_TRACEME would be the obvious probe and is a trap — if the
	// filter were broken it would SUCCEED, leaving this process traced by the
	// test runner, which is not a tracer and would hang on the next stop.
	_, _, errno := unix.Syscall(unix.SYS_PTRACE, uintptr(unix.PTRACE_PEEKDATA), 0, 0)
	report("ptrace", errnoOrNil(errno))
	_, _, errno = unix.Syscall(unix.SYS_IO_URING_SETUP, 0, 0, 0)
	report("io_uring", errnoOrNil(errno))

	os.Stdout.WriteString("DONE\n")
	os.Exit(0)
}

func errnoOrNil(errno unix.Errno) error {
	if errno == 0 {
		return nil
	}
	return errno
}

// seccompTestsOptionalEnv lets a host that genuinely cannot install a filter
// downgrade the behaviour tests below from a failure to a skip.
//
// It must be set EXPLICITLY, and that is the whole design. See
// requireSeccompInstallable.
const seccompTestsOptionalEnv = "YANSHI_SECCOMP_TESTS_OPTIONAL"

// requireSeccompInstallable decides whether an unavailable seccomp is a skip or
// a failure, and defaults to FAILURE.
//
// # Why the default flipped
//
// These two behaviour tests are the only evidence that the filter does anything
// — everything else about it is a pure-function assertion on the BPF program's
// shape. They were unconditionally skipped when seccompSupported() said no,
// which produces a PASS on a runner that quietly forbids nested seccomp. The
// batch report that shipped them said so in as many words: "seeing a skip means
// it was not verified, do not read it as passing". A status that can only be
// read correctly by someone who also read a warning in a report is not a
// status. It is the same defect GOV8 already refuses in the ledger — a test
// that compiled, ran, reported pass, and asserted nothing.
//
// # What is a genuine "not supported" and what is a run that did not happen
//
// The file is //go:build linux, so GOOS carries no information here. The real
// discriminator is WHOSE property the unavailability is:
//
//   - The architecture has no verified syscall-number mapping in this package
//     (seccompAuditArch). That is a property of THIS CODE, it is the same answer
//     on every host of that architecture, and there is nothing a runner could do
//     about it. Skip, and say which arch.
//
//   - Everything else — the kernel refusing the probe, a container policy
//     forbidding a nested filter — is a property of THIS RUN. On the CI leg that
//     is meant to be the evidence for W-B-09, that is precisely the case that
//     must be loud: the leg exists to exercise this, and a leg that silently
//     stops exercising it leaves an acceptance clause permanently "pending CI".
//
// A host in the second category that cannot be fixed sets
// seccompTestsOptionalEnv. That is one deliberate act by whoever owns the
// runner, recorded where the runner is configured, instead of a default that
// answers "fine" for everyone.
func requireSeccompInstallable(t *testing.T) {
	t.Helper()
	err := seccompSupported()
	if err == nil {
		return
	}
	if _, mapped := seccompAuditArch[runtime.GOARCH]; !mapped {
		t.Skipf("this package has no verified seccomp syscall mapping for GOARCH=%s; "+
			"that is a gap in the code, not in this host", runtime.GOARCH)
	}
	if os.Getenv(seccompTestsOptionalEnv) != "" {
		t.Skipf("seccomp is unavailable here and %s is set: %v", seccompTestsOptionalEnv, err)
	}
	t.Fatalf("seccomp could not be installed on this host: %v\n\n"+
		"This is a FAILURE rather than a skip on purpose. The two behaviour tests in this "+
		"file are the only evidence that the filter blocks anything; skipping them yields a "+
		"green run that is indistinguishable from a verified one, and W-B-09's acceptance "+
		"names the linux CI leg as where it is checked. If this host genuinely cannot install "+
		"a filter — a container with a policy that forbids nesting — set %s where that "+
		"runner is configured, which records the decision instead of defaulting to it.",
		err, seccompTestsOptionalEnv)
}

// runSeccompHelper spawns the helper and returns its label=value lines.
func runSeccompHelper(t *testing.T, mode string) map[string]string {
	t.Helper()
	requireSeccompInstallable(t)
	cmd := exec.Command(os.Args[0], "-test.run", "^TestSeccompHelper$", "-test.v=false")
	cmd.Env = append(os.Environ(), seccompHelperEnv+"="+mode)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("seccomp helper failed: %v\nstdout: %s", err, out)
	}
	text := string(out)
	if strings.Contains(text, "APPLY-FAILED") {
		t.Fatalf("the filter could not be installed on this host: %s", text)
	}
	if !strings.Contains(text, "DONE") {
		t.Fatalf("helper did not run to completion — the filter may have killed it: %s", text)
	}
	got := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		if name, value, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			got[name] = value
		}
	}
	return got
}

// TestSeccompDeniesNonUnixSocketsUnderNetworkDeny is the acceptance for the
// audit finding this layer exists to answer: the managed proxy is an
// environment variable, and before this filter a child that opened a socket
// directly ignored it completely.
//
// Both directions are asserted. Blocking every socket would also "pass" a
// one-sided test and would break anything speaking to a unix socket — a
// container runtime, a language server, syslog — so the AF_UNIX case is not
// a courtesy assertion.
func TestSeccompDeniesNonUnixSocketsUnderNetworkDeny(t *testing.T) {
	got := runSeccompHelper(t, "netdeny")
	if got["unix-socket"] != "ok" {
		t.Errorf("AF_UNIX sockets must still work: %s", got["unix-socket"])
	}
	for _, family := range []string{"inet-socket", "inet6-socket"} {
		if got[family] == "ok" {
			t.Errorf("%s was permitted under NetworkDeny: the filter is not enforcing", family)
		}
		if !strings.Contains(got[family], "permission denied") {
			t.Errorf("%s should be refused with EACCES, got %q", family, got[family])
		}
	}
}

// TestSeccompAlwaysDeniesMemoryAndUringSyscalls pins the "regardless of the
// network policy" half.
//
// It runs with netDeny FALSE on purpose. Those three are not an egress control:
// ptrace and process_vm_readv read another process's memory — yanshi's, which
// holds the operator's keys, on a backend with no PID namespace to stop it —
// and io_uring performs opens and connects without issuing those syscalls, so
// leaving it reachable would make every other rule advisory.
func TestSeccompAlwaysDeniesMemoryAndUringSyscalls(t *testing.T) {
	got := runSeccompHelper(t, "netopen")
	if got["inet-socket"] != "ok" {
		t.Errorf("with NetworkDeny off, AF_INET must still work: %s", got["inet-socket"])
	}
	if got["ptrace"] == "ok" {
		t.Error("ptrace was permitted: a confined child can read this process's memory")
	}
	if got["io_uring"] == "ok" {
		t.Error("io_uring_setup was permitted: the filter can be bypassed asynchronously")
	}
	if !strings.Contains(got["ptrace"], "not permitted") {
		t.Errorf("ptrace should be refused with EPERM, got %q", got["ptrace"])
	}
}

// TestSeccompFilterStartsWithAnArchitectureCheck pins the one instruction whose
// absence is a silent bypass rather than a visible bug.
//
// On x86-64 a process can issue the same call through the x32 ABI, whose
// numbers are the 64-bit ones with 0x40000000 set. A filter that compared only
// seccomp_data.nr would match none of them and let every denied syscall through
// in its x32 form, while every behavioural test above still passed — they all
// use the native ABI.
func TestSeccompFilterStartsWithAnArchitectureCheck(t *testing.T) {
	prog, err := buildSeccompFilter(true)
	if err != nil {
		t.Fatalf("buildSeccompFilter: %v", err)
	}
	if len(prog) < 4 {
		t.Fatalf("filter is implausibly short: %+v", prog)
	}
	if prog[0].Code != bpfLoadWordAbs || prog[0].K != seccompOffsetArch {
		t.Fatalf("the first instruction must load seccomp_data.arch, got %+v", prog[0])
	}
	if prog[1].Code != bpfJumpEqualK || prog[1].K != seccompAuditArch[runtime.GOARCH] {
		t.Fatalf("the second instruction must compare this architecture, got %+v", prog[1])
	}
	if prog[2].Code != bpfReturnK || prog[2].K != unix.SECCOMP_RET_KILL_PROCESS {
		t.Fatalf("an architecture mismatch must kill, not return an errno, got %+v", prog[2])
	}
	if prog[3].Code != bpfLoadWordAbs || prog[3].K != seccompOffsetNR {
		t.Fatalf("the syscall number must be loaded after the arch check, got %+v", prog[3])
	}
}

// TestSeccompFilterOmitsTheSocketBlockWhenEgressIsPermitted pins that NetDeny
// really selects, rather than the filter denying sockets unconditionally and
// the flag being decoration.
//
// The behavioural tests above cannot catch that on their own: a host where the
// helper cannot install a filter skips them, and this one is a pure function.
func TestSeccompFilterOmitsTheSocketBlockWhenEgressIsPermitted(t *testing.T) {
	loadsArg0 := func(netDeny bool) bool {
		prog, err := buildSeccompFilter(netDeny)
		if err != nil {
			t.Fatalf("buildSeccompFilter(%t): %v", netDeny, err)
		}
		for _, insn := range prog {
			if insn.Code == bpfLoadWordAbs && insn.K == seccompOffsetArg0 {
				return true
			}
		}
		return false
	}
	if loadsArg0(false) {
		t.Error("with NetworkDeny off the filter must not inspect the address family at all")
	}
	if !loadsArg0(true) {
		t.Error("with NetworkDeny on the filter must inspect socket(2)'s address family")
	}
}
