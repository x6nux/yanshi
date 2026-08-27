package log

// crashinstall.go is the missing half of crash.go.
//
// THE GAP THIS CLOSES. crash.go builds a complete, redacted, body-free crash
// report and knows how to announce it on stderr. Every part of it was covered
// by tests that called Dump directly, and every one of those tests passed. It
// had no production caller: a repository-wide search for NewCrashDumper and
// ReportCrash outside the package's own files returned nothing, and there was
// no deferred recover() anywhere in the turn path. A panic in the shipped
// binary therefore printed a bare Go traceback and wrote no report at all — the
// exact condition O10 exists to fix, with the machinery to fix it sitting one
// unwritten call away.
//
// That was MEASURED, not inferred: a child process was forked, made to panic,
// and its crash directory inspected afterwards. It was empty.
//
// WHY THIS FILE AND NOT A CALL IN main(). A crash handler has to be installed
// at the top of each entry point (serve, exec, goal, the TUI), and an installer
// each of them can call in one line is the difference between "wired" and
// "wired in four places, three of them correctly". Keeping it beside the dumper
// also means the recover/re-panic discipline below is reviewed next to the
// redaction rules it depends on.
//
// RE-PANIC IS MANDATORY. The handler recovers only long enough to write the
// report and then panics again with the original value. Swallowing a panic
// converts a loud crash into a process that keeps running with invariants
// already broken, which is strictly worse than crashing: the operator loses the
// traceback AND gets wrong answers. Capture is a side effect on the way down,
// never a rescue.

import (
	"context"
	"io"
	"os"
	"sync/atomic"
)

// CrashHandlerConfig configures InstallCrashHandler.
type CrashHandlerConfig struct {
	// Dir is where reports are written. Empty uses DefaultCrashDir().
	Dir string
	// Redactor is the process-wide secrets redactor. Nil is tolerated (the
	// dumper substitutes a no-op) because a report that exists and redacts
	// nothing is more useful than no report, and the alternative — skipping
	// the dump when the redactor is not ready yet — hides exactly the early
	// boot crashes that are hardest to reproduce.
	Redactor Redactor
	// Stderr receives the one-line "crash report -> <path>" announcement.
	// Nil defaults to os.Stderr.
	Stderr io.Writer
	// IncludeBodies forwards the dumper's debug switch. Off by default; see
	// CrashDumper.IncludeBodies for why flipping it is an operator decision.
	IncludeBodies bool
	// ConfigValues is the flat config view fingerprinted into every report, so
	// a crash can be tied to the configuration that produced it.
	ConfigValues map[string]string
}

// installedDumper holds the process-wide dumper so surfaces that report on
// crashes (doctor, a status command) can find the directory without being
// handed it. Nil until InstallCrashHandler runs.
var installedDumper atomic.Pointer[CrashDumper]

// InstalledCrashDumper returns the process-wide dumper, or nil when no crash
// handler has been installed.
//
// It exists so an operator surface can answer "where are the crash reports"
// without every such surface re-deriving the directory — a second derivation
// would drift from the first the moment either changed.
func InstalledCrashDumper() *CrashDumper { return installedDumper.Load() }

// InstallCrashHandler prepares crash capture for this process and returns a
// function to be deferred at the top of the entry point.
//
// Usage is deliberately two-step rather than a single Install() that defers
// internally, because a deferred call registered inside a helper runs when the
// HELPER returns, not when the caller does — which would arm the handler for
// microseconds and then disarm it for the life of the program. Making the
// caller write the defer puts the recover on the caller's own stack, where it
// can actually see the panic:
//
//	restore := obslog.InstallCrashHandler(obslog.CrashHandlerConfig{...})
//	defer restore()
//
// The returned function re-panics after writing the report, so the process
// still dies with its original traceback and exit status.
//
// Setup failure (an unwritable directory) is not fatal and is not silent: the
// returned function still exists and simply captures nothing, and the reason is
// reported on stderr at install time rather than during the crash. Refusing to
// start because the crash directory is unavailable would let a permissions
// problem in a diagnostic path take down the service it was meant to diagnose.
func InstallCrashHandler(cfg CrashHandlerConfig) func() {
	stderr := cfg.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	dir := cfg.Dir
	if dir == "" {
		d, err := DefaultCrashDir()
		if err != nil {
			warnCrashSetup(stderr, err)
			return func() {}
		}
		dir = d
	}
	d, err := NewCrashDumper(dir, cfg.Redactor)
	if err != nil {
		warnCrashSetup(stderr, err)
		return func() {}
	}
	d.IncludeBodies = cfg.IncludeBodies
	d.ConfigValues = cfg.ConfigValues
	installedDumper.Store(d)

	return func() {
		r := recover()
		if r == nil {
			return
		}
		// Best-effort: a failed dump must not replace the original panic with
		// a less informative one, so the path is ignored and the value is
		// re-raised either way.
		if path, derr := d.DumpPanic(context.Background(), r, nil); derr == nil && path != "" {
			announceCrash(stderr, path)
		}
		panic(r)
	}
}

// CaptureCrash writes a report for a non-panic failure that is nonetheless
// worth a scene — a turn that died with an error the operator will need to
// reconstruct — using whatever dumper InstallCrashHandler installed.
//
// It is a no-op when no handler is installed, which is what makes it safe to
// call from library code that does not know whether the embedding process
// opted in.
func CaptureCrash(ctx context.Context, err error, messages []MessageMeta, stderr io.Writer) string {
	d := installedDumper.Load()
	if d == nil || err == nil {
		return ""
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	return ReportCrash(ctx, d, "error", err, messages, stderr)
}

// announceCrash prints the one line that makes a report findable. It names the
// path and nothing else, keeping stderr's disclosure posture identical to the
// structured log's.
func announceCrash(w io.Writer, path string) {
	_, _ = io.WriteString(w, "yanshi: crash report -> "+path+"\n")
}

// warnCrashSetup reports that crash capture is unavailable, at install time.
// Saying so now — rather than discovering it mid-crash — is the difference
// between a known limitation and a mystery.
func warnCrashSetup(w io.Writer, err error) {
	_, _ = io.WriteString(w, "yanshi: crash reporting unavailable: "+err.Error()+"\n")
}
