// Package harden applies the process-level defences that must be in effect
// before yanshi does anything else: no core dumps, no debugger attach, and no
// inherited dynamic-loader injection variables.
//
// # What this protects, and what it does not
//
// yanshi holds provider API keys, VCS tokens and the operator's whole session
// transcript in its own address space. Three cheap OS mechanisms keep that
// memory from being read by something that is not yanshi:
//
//   - A core dump is the entire heap written to disk with default permissions.
//     Any crash — including one an untrusted child can provoke — would leave
//     every key in a file.
//   - ptrace (and its Windows/macOS equivalents) reads that same memory from a
//     live process, with no crash required.
//   - LD_PRELOAD / DYLD_INSERT_LIBRARIES run attacker-chosen code INSIDE the
//     process, which defeats both of the above by being on the inside already.
//
// The loader variables are the one case where the honest scope is narrower than
// it looks: by the time any Go code runs, the dynamic loader has already acted
// on them. Clearing them here cannot undo an injection into THIS process. What
// it does is stop the variables from being inherited by everything yanshi
// spawns — and yanshi builds every child environment from os.Environ() (see
// shell.childLaunchPosture), so without this a single exported
// DYLD_INSERT_LIBRARIES would be handed to every sandboxed command, every ACP
// agent and every MCP server. That is a real boundary; "hardens this process
// against injection" is not, and this package does not claim it.
//
// # Failure is never fatal
//
// Every step reports its own outcome and Apply keeps going. A container that
// forbids setrlimit, a kernel without the prctl, a macOS that refuses the
// ptrace call — none of those are reasons to refuse to start an agent server.
// The Report is what makes the degradation visible instead of silent.
package harden

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// DisableEnv is the environment variable an operator sets to skip hardening.
//
// It exists for one concrete reason: PT_DENY_ATTACH makes the process
// undebuggable on macOS, and a debugger that attaches afterwards kills it. A
// maintainer running `dlv exec ./yanshi` on the binary they are working on
// needs a way out that is not "edit the source".
//
// It is not a security hole worth closing. Everything this package does
// protects the process from OTHER processes; an attacker who can already set
// this process's environment has, by definition, won before it starts.
const DisableEnv = "YANSHI_NO_HARDEN"

// Step is the outcome of one hardening measure.
//
// Err is a string rather than an error so a Report round-trips through JSON —
// the helper subprocess in this package's tests is how the platform-specific
// steps are checked without hardening the test runner itself.
type Step struct {
	// Name identifies the measure ("core-dumps", "debugger", "loader-env").
	Name string `json:"name"`
	// Detail says what was actually done, including any read-back verification.
	Detail string `json:"detail"`
	// Err is empty when the step succeeded or did not apply to this platform.
	Err string `json:"err,omitempty"`
}

// Report is the outcome of a full Apply.
type Report struct {
	// Steps is one entry per measure, in the order they ran.
	Steps []Step `json:"steps"`
	// Skipped is true when DisableEnv was set and nothing was attempted.
	Skipped bool `json:"skipped,omitempty"`
}

// Failed reports whether any step failed, so a caller can decide whether the
// Report is worth printing.
func (r Report) Failed() bool {
	for _, s := range r.Steps {
		if s.Err != "" {
			return true
		}
	}
	return false
}

// WriteFailures prints one line per failed step, and nothing at all when every
// step succeeded.
//
// Silence on success is deliberate: this runs on every invocation of a CLI
// whose output is frequently piped, and three lines of "hardening ok" before
// each command trains an operator to ignore the fourth one that says it is not.
func (r Report) WriteFailures(w io.Writer) {
	if r.Skipped {
		fmt.Fprintf(w, "yanshi: process hardening skipped (%s is set)\n", DisableEnv)
		return
	}
	for _, s := range r.Steps {
		if s.Err != "" {
			fmt.Fprintf(w, "yanshi: hardening step %q did not apply: %s\n", s.Name, s.Err)
		}
	}
}

// Apply runs every hardening measure and returns what happened.
//
// Call it as early as possible in main, before any subsystem starts and before
// any child process can be spawned. It never returns an error and never exits:
// see the package doc for why a failed measure must not stop the server.
func Apply() Report {
	if os.Getenv(DisableEnv) != "" {
		return Report{Skipped: true}
	}
	return Report{Steps: []Step{
		disableCoreDumps(),
		denyDebugger(),
		scrubLoaderEnv(),
	}}
}

// loaderEnvPrefixes are the environment-variable namespaces the dynamic loader
// consults, all of which can make a child run code its caller never chose.
//
// Matching by PREFIX rather than by exact name is what makes this survive the
// loaders growing new knobs: glibc has added LD_AUDIT and LD_DEBUG_OUTPUT over
// time, and dyld's DYLD_ space is large enough that an exact list is a
// maintenance promise nobody keeps. The cost of the wide match is that
// LD_LIBRARY_PATH goes too — which is intended. A caller that legitimately
// needs to point a child at a private libdir sets it on that spawn's
// LaunchSpec.Env, where the decision is visible, rather than by exporting it
// into the whole server.
//
// Windows' equivalent injection vector is the AppInit_DLLs registry key and the
// PATH-order search, neither of which is an environment variable, so this list
// is a no-op there rather than incomplete.
var loaderEnvPrefixes = []string{
	"LD_",
	"DYLD_",
}

// loaderEnvKeep names variables that start with a scrubbed prefix and are not
// loader controls.
//
// LD_LIBRARY_PATH is deliberately NOT here — see loaderEnvPrefixes. This list
// exists so the prefix rule can stay wide without collateral damage from a
// name that merely looks like a loader variable; it is empty today and the
// lookup is what keeps adding an entry from being a rewrite.
var loaderEnvKeep = map[string]bool{}

// scrubLoaderEnv removes dynamic-loader injection variables from this process's
// environment, so nothing yanshi spawns inherits them.
//
// It reports the names it removed rather than a count: an operator whose
// tooling depended on one of these needs to know WHICH, and a bare "removed 2
// variables" sends them looking through their whole shell profile.
func scrubLoaderEnv() Step {
	var removed []string
	for _, entry := range os.Environ() {
		name, _, ok := strings.Cut(entry, "=")
		if !ok || loaderEnvKeep[name] {
			continue
		}
		if !hasLoaderPrefix(name) {
			continue
		}
		if err := os.Unsetenv(name); err != nil {
			return Step{Name: "loader-env", Detail: "unsetenv " + name, Err: err.Error()}
		}
		removed = append(removed, name)
	}
	if len(removed) == 0 {
		return Step{Name: "loader-env", Detail: "no loader injection variables were set"}
	}
	return Step{Name: "loader-env", Detail: "removed " + strings.Join(removed, ", ")}
}

// hasLoaderPrefix reports whether name is in one of the loader namespaces.
func hasLoaderPrefix(name string) bool {
	for _, prefix := range loaderEnvPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}
