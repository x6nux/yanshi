package sandbox

// This file adds the one seam a container-style backend needs and the
// Sandbox interface cannot express.
//
// # Why Prepare is not enough
//
// Prepare receives a command that has NOT been started. That is exactly right
// for an argv-rewriting backend (macOS re-heads the command with sandbox-exec)
// and exactly wrong for a backend whose mechanism is a kernel object a RUNNING
// process must be attached to. Windows Job Objects are the second kind:
// AssignProcessToJobObject takes a process handle, so it cannot run before the
// process exists.
//
// # Why this is a second interface rather than a method on Sandbox
//
// Adding PostStart to Sandbox would force every current and future adapter —
// darwin's seatbelt, linux's, the shared degraded stub, and every test double
// anyone writes — to carry a method three of them have nothing to do. The
// type-assertion form costs one `if` at each launch site and leaves the other
// platforms untouched; see PostStartSandbox for what that `if` must look like.
//
// # The alternative that was rejected, and why it is not available here
//
// The textbook fix for the assign-after-create race is CREATE_SUSPENDED:
// create the process suspended, assign it to the job, then resume the initial
// thread. Nothing can run before the job contains it, so the window is closed
// by construction.
//
// That is not reachable from this package. The *exec.Cmd handed to Prepare on
// the production path is a STAND-IN: shell.childLaunchPosture.prepare builds a
// throwaway Cmd, lets the backend mutate it, and then copies back exactly four
// fields — Program, Dir, Env and Args. SysProcAttr is not among them, and
// shell.LaunchSpec has no field to carry it, so a CreationFlags value set here
// would be silently dropped and the spawn would proceed unsuspended. The suspend
// approach therefore needs a change in the shell launch path (a LaunchSpec field
// plus a resume step), not a change here, and shipping a CREATE_SUSPENDED flag
// that the launch path discards would be a mechanism that reads as if it worked.
//
// The residual race is documented on PostStart and reflected in the Windows
// backend's Reason string rather than papered over.

// PostStartSandbox is the optional half of the Sandbox contract, implemented
// only by backends whose mechanism attaches to a process that is already
// running.
//
// Launch sites opt in with a type assertion, and the assertion must be made
// against the value they were handed rather than against a stored concrete
// type:
//
//	if ps, ok := sb.(sandbox.PostStartSandbox); ok {
//	    if err := ps.PostStart(proc.PID()); err != nil {
//	        // The child is NOT contained. It has already been terminated by
//	        // PostStart; fail the launch rather than returning a process the
//	        // report claims is inside a job.
//	        return err
//	    }
//	}
//
// A backend that does not implement this interface needs no call at all — the
// assertion simply fails and the launch proceeds, which is why adding the seam
// did not have to touch darwin, linux or the degraded stub.
type PostStartSandbox interface {
	Sandbox

	// PostStart binds the freshly started process identified by pid into
	// whatever OS container this backend maintains.
	//
	// # Contract for callers
	//
	// It MUST be called after the process has started and BEFORE anything
	// reaps it (Wait) or releases its handle. That ordering is not politeness,
	// it is what makes the pid unambiguous: on Windows a process id stays
	// reserved for as long as any handle to the process object is open, and
	// the Go runtime holds such a handle from os.StartProcess until Wait or
	// Release. Call PostStart after the reap and the id may already name a
	// different process — which this method would then bind into a
	// kill-on-close job and terminate at Close.
	//
	// # Contract for implementations
	//
	// Returning an error means the process is NOT contained. An implementation
	// MUST terminate the process before returning that error: the caller was
	// told by Report() that children run inside a container, and quietly
	// handing back a running process that escaped it is the over-claim this
	// package exists to prevent. Callers propagate the error and abandon the
	// launch.
	//
	// A non-enforcing (degraded) backend returns nil without doing anything —
	// the same rule Prepare follows. It has nothing to attach the process to,
	// and failing the spawn on a host where the mechanism is unavailable would
	// be an outage rather than a degradation.
	//
	// # The race this cannot close
	//
	// Between process creation and this call the child is running and
	// uncontained. A grandchild it spawns inside that window is NOT
	// retroactively pulled into the job when the parent is assigned, so it can
	// outlive Close. In practice the window is the few microseconds between
	// the OS factory returning and the launch site's next statement, while a
	// child that spawns anything must first be scheduled and parse its own
	// input — but "narrow" is not "closed", and closing it requires
	// CREATE_SUSPENDED support in the shell launch path (see this file's
	// header).
	PostStart(pid int) error
}
