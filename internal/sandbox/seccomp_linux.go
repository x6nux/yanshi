//go:build linux

package sandbox

import (
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// This file is the seccomp-BPF layer the Landlock backend installs on the
// confined process, immediately before it execve()s the target.
//
// # Why it exists
//
// The Landlock backend's own header states, correctly, that a process confined
// by it "can still open sockets and reach any host the routing table allows,
// regardless of Config.NetworkDeny". That sentence is the hole this file
// closes. Everywhere else, egress control is the managed proxy — which is an
// ENVIRONMENT VARIABLE and is honoured only by programs that choose to read it.
// A child that opens a socket directly never consults HTTPS_PROXY, so on the
// Landlock path the proxy was a request rather than a boundary.
//
// # What the filter can and cannot see, precisely
//
// A seccomp filter runs on syscall arguments as REGISTER VALUES. It cannot
// dereference a pointer, which means it cannot look at the sockaddr passed to
// connect(2), bind(2) or sendto(2) and cannot therefore distinguish
// "connect to a unix socket" from "connect to 10.0.0.1" at those calls.
// Attempting it would be a time-of-check/time-of-use hole even if the kernel
// allowed it, because userspace could rewrite the struct after the check.
//
// The enforcement is therefore at socket CREATION, where the address family is
// an immediate argument: socket(2) and socketpair(2) are permitted only for
// AF_UNIX. Every one of connect/bind/listen/sendto then operates on a
// descriptor that must have come from a permitted call, so the acceptance
// property ("non-AF_UNIX connect/bind/listen/sendto/socket are blocked") holds
// transitively rather than by naming each syscall.
//
// The one gap in that reasoning is an INHERITED descriptor: a socket the parent
// opened and passed down is not created by the child and no filter sees it. On
// this launch path the child inherits stdin/stdout/stderr and nothing else, so
// there is no such descriptor — but the property is a property of the launcher,
// not of the filter, and it is written down here rather than assumed.
//
// # The unconditional denials
//
// ptrace, process_vm_readv/writev and the io_uring family are denied whatever
// NetworkDeny says. The first two are how a confined process reads another
// process's memory — including yanshi's, which holds the operator's keys —
// and there is no PID namespace on this backend to stop it. io_uring is denied
// because it is a second, asynchronous path to file and network operations that
// the seccomp filter cannot see: an io_uring_enter() submission queue performs
// opens and connects without issuing those syscalls, so leaving it open would
// make every rule above advisory.

// seccompAuditArch maps GOARCH to the AUDIT_ARCH_* token the kernel reports in
// seccomp_data.arch.
//
// The arch check is not a portability nicety, it is the first thing the filter
// does and skipping it is a known bypass: on x86-64 the same process can issue
// x32 syscalls, whose numbers are the x86-64 ones with 0x40000000 set, so a
// filter that compared only nr would let the x32 form of socket(2) through
// unexamined. Any architecture not listed here has no verified syscall-number
// mapping, and seccompSupported reports it as unavailable rather than
// installing a filter whose numbers might name entirely different calls.
var seccompAuditArch = map[string]uint32{
	"amd64":   unix.AUDIT_ARCH_X86_64,
	"arm64":   unix.AUDIT_ARCH_AARCH64,
	"386":     unix.AUDIT_ARCH_I386,
	"arm":     unix.AUDIT_ARCH_ARM,
	"riscv64": unix.AUDIT_ARCH_RISCV64,
	"ppc64le": unix.AUDIT_ARCH_PPC64LE,
	"s390x":   unix.AUDIT_ARCH_S390X,
	"loong64": unix.AUDIT_ARCH_LOONGARCH64,
}

// seccompAlwaysDenied are the syscall numbers refused at every tier and
// regardless of the network policy. See the file header for why each is here.
var seccompAlwaysDenied = []uint32{
	unix.SYS_PTRACE,
	unix.SYS_PROCESS_VM_READV,
	unix.SYS_PROCESS_VM_WRITEV,
	unix.SYS_IO_URING_SETUP,
	unix.SYS_IO_URING_ENTER,
	unix.SYS_IO_URING_REGISTER,
}

// seccomp_data field offsets, from the kernel's uapi definition. nr and arch
// are 32-bit; args are 64-bit and only the low word is compared here, which is
// correct for an address family (a 32-bit int passed in a register).
const (
	seccompOffsetNR   = 0
	seccompOffsetArch = 4
	seccompOffsetArg0 = 16
)

// Classic-BPF opcodes used by the filter. They are spelled out rather than
// taken from a helper package because the whole program is nine instruction
// kinds and a dependency here would be larger than the code it replaced.
const (
	bpfLoadWordAbs = unix.BPF_LD | unix.BPF_W | unix.BPF_ABS
	bpfJumpEqualK  = unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K
	bpfReturnK     = unix.BPF_RET | unix.BPF_K
)

// seccompSupported reports whether this process can install a filter, or why
// not.
//
// Two independent questions, both of which have to be yes:
//
//   - Does this build know the architecture's audit token? A filter installed
//     with the wrong one either kills everything or matches nothing.
//   - Does the kernel implement seccomp(2) in filter mode? The probe passes a
//     NULL filter pointer, so a kernel that supports the mode fails with EFAULT
//     while one that does not fails with EINVAL or ENOSYS. Nothing is installed
//     on this process either way — which matters, because a filter here would
//     be inherited by every child yanshi ever spawns, including the ones the
//     sandbox is not meant to confine.
func seccompSupported() error {
	if _, ok := seccompAuditArch[runtime.GOARCH]; !ok {
		return fmt.Errorf("no verified seccomp syscall mapping for GOARCH=%s", runtime.GOARCH)
	}
	_, _, errno := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER, 0, 0)
	switch errno {
	case unix.EFAULT:
		return nil
	case unix.ENOSYS:
		return fmt.Errorf("kernel does not implement seccomp(2) (needs CONFIG_SECCOMP_FILTER)")
	case unix.EINVAL:
		return fmt.Errorf("kernel does not support seccomp filter mode")
	default:
		return fmt.Errorf("seccomp availability probe failed: %w", errno)
	}
}

// buildSeccompFilter assembles the classic-BPF program.
//
// The program is written with FORWARD jumps only, because classic BPF has no
// backward branch: the verifier rejects one outright, and the natural shape
// ("check the family, then jump back to the shared allow") is exactly what a
// reader reaches for first. Every jump offset below is therefore a small
// forward count into the tail of the program, and the layout is:
//
//	load  arch
//	jeq   <this arch> ? skip 1 : fall through
//	ret   KILL_PROCESS                 // wrong ABI: not an errno, a kill
//	load  nr
//	for each always-denied nr:
//	  jeq  nr ? fall through : skip 1
//	  ret  ERRNO(EPERM)
//	// only when netDeny:
//	jeq   socket     ? skip 1 : fall through
//	jeq   socketpair ? fall through : skip 3
//	load  arg0                          // the address family
//	jeq   AF_UNIX ? skip 1 : fall through
//	ret   ERRNO(EACCES)
//	ret   ALLOW
//
// The wrong-architecture case is a KILL rather than an errno on purpose. An
// errno would let a program that guessed the ABI keep probing; a mismatch here
// is not a policy decision but a sign that the filter's syscall numbers mean
// nothing, and continuing under a filter that does not apply is precisely the
// bypass the check exists for.
//
// EPERM for the always-denied set and EACCES for a refused address family, so
// the two are distinguishable in a child's error message: "operation not
// permitted" from ptrace and "permission denied" from socket are what a
// developer reads when a tool fails inside the sandbox.
func buildSeccompFilter(netDeny bool) ([]unix.SockFilter, error) {
	arch, ok := seccompAuditArch[runtime.GOARCH]
	if !ok {
		return nil, fmt.Errorf("sandbox: no seccomp syscall mapping for GOARCH=%s", runtime.GOARCH)
	}
	prog := []unix.SockFilter{
		{Code: bpfLoadWordAbs, K: seccompOffsetArch},
		{Code: bpfJumpEqualK, Jt: 1, Jf: 0, K: arch},
		{Code: bpfReturnK, K: unix.SECCOMP_RET_KILL_PROCESS},
		{Code: bpfLoadWordAbs, K: seccompOffsetNR},
	}
	for _, nr := range seccompAlwaysDenied {
		prog = append(prog,
			unix.SockFilter{Code: bpfJumpEqualK, Jt: 0, Jf: 1, K: nr},
			unix.SockFilter{Code: bpfReturnK, K: unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)},
		)
	}
	if netDeny {
		prog = append(prog,
			// socket(2): jump over the socketpair test to the family check.
			unix.SockFilter{Code: bpfJumpEqualK, Jt: 1, Jf: 0, K: uint32(unix.SYS_SOCKET)},
			// socketpair(2): fall through to the family check, or skip the whole
			// three-instruction block to the final ALLOW.
			unix.SockFilter{Code: bpfJumpEqualK, Jt: 0, Jf: 3, K: uint32(unix.SYS_SOCKETPAIR)},
			unix.SockFilter{Code: bpfLoadWordAbs, K: seccompOffsetArg0},
			unix.SockFilter{Code: bpfJumpEqualK, Jt: 1, Jf: 0, K: uint32(unix.AF_UNIX)},
			unix.SockFilter{Code: bpfReturnK, K: unix.SECCOMP_RET_ERRNO | uint32(unix.EACCES)},
		)
	}
	prog = append(prog, unix.SockFilter{Code: bpfReturnK, K: unix.SECCOMP_RET_ALLOW})
	if len(prog) > 4096 {
		return nil, fmt.Errorf("sandbox: seccomp program is %d instructions, over the kernel limit", len(prog))
	}
	return prog, nil
}

// applySeccomp installs the filter on the CALLING process, irreversibly, for it
// and every descendant across execve.
//
// PR_SET_NO_NEW_PRIVS is required by seccomp(2) for an unprivileged caller and
// is set here rather than relied upon from applyLandlock: the two are separate
// mechanisms and the order in which the helper applies them is not this
// function's business to assume. prctl is idempotent, so setting it twice costs
// one syscall and removes a coupling.
//
// TSYNC is requested so the filter reaches every thread of the Go runtime, not
// just the one that happened to run this code. Without it a goroutine on
// another OS thread would remain unfiltered — and since the helper calls this
// before execve, an unfiltered thread is the difference between "the target is
// confined" and "the target is confined unless it is scheduled elsewhere".
func applySeccomp(netDeny bool) error {
	prog, err := buildSeccompFilter(netDeny)
	if err != nil {
		return err
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("prctl(PR_SET_NO_NEW_PRIVS): %w", err)
	}
	fprog := unix.SockFprog{Len: uint16(len(prog)), Filter: &prog[0]}
	if _, _, errno := unix.Syscall(
		unix.SYS_SECCOMP,
		unix.SECCOMP_SET_MODE_FILTER,
		unix.SECCOMP_FILTER_FLAG_TSYNC,
		uintptr(unsafe.Pointer(&fprog)),
	); errno != 0 {
		return fmt.Errorf("seccomp(SECCOMP_SET_MODE_FILTER): %w", errno)
	}
	return nil
}
