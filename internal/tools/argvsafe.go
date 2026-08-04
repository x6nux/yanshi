package tools

import (
	"fmt"
	"strings"
)

// validateArgvOperand rejects a model-supplied string that is about to occupy
// an OPERAND slot in a child process's argv — a git revision, a `go test`
// package pattern, a `cargo test` filter.
//
// The bug class it exists to close is ARGUMENT injection, which is not shell
// injection and is not fixed by the defences that stop shell injection. Every
// launch in this package goes through exec without a shell, so quoting,
// metacharacter rejection (guard's `&&`/`|`/`$()` denial) and NUL stripping
// are all irrelevant here: the value already arrives at the callee as one
// intact argv element. The problem is that argv carries no types. The callee
// alone decides what each element MEANS, and essentially every Unix CLI
// decides it by looking at the first byte: a leading dash makes the element an
// option instead of data. A ref the model chose therefore stops being a ref
// and becomes a flag — and CLI flag surfaces are enormous and write-capable.
// git alone hands out arbitrary file creation through `--output=<path>` and
// `--output-indicator-new=…`, arbitrary reads through `-O<orderfile>`, and
// arbitrary execution through `--ext-diff`; `go test` writes an arbitrary
// executable anywhere with `-o <path>`. git_diff declares sandbox.ReadOnly and
// run_tests declares sandbox.WorkspaceWrite, so any of those turns a tool into
// a write primitive strictly outside the tier it advertised — and on the
// platforms where the sandbox is still phase0 (see internal/sandbox) that
// declared tier is the only boundary there is.
//
// The check is a WHITELIST of SHAPE — "must not look like an option" — and
// deliberately not a blacklist of known-dangerous flags. A blacklist has to
// enumerate every write-capable option of every program yanshi ever launches,
// stays correct only until those programs cut a release, and is silently wrong
// in between; `--output` was found by hand here, `--output-indicator-new` and
// `-O` were not, and neither list is closed. Rejecting the one syntactic
// property that makes ALL of them reachable costs a single prefix comparison
// and cannot go stale.
//
// A leading dash is the ONLY thing rejected beyond emptiness and NUL, because
// dashes are ordinary elsewhere in these values: `my-branch`, `v1.0-rc1`,
// `feature/x-y` and `./cmd/foo-bar` must all keep working. Over-rejecting here
// would push callers back onto raw strings, which is how the hole appeared.
//
// This is the FIRST of two layers for git specifically; gitDiffCommands also
// passes `--end-of-options` so that git itself refuses to read the operand as
// an option. Neither layer is load-bearing alone: this one generalises to
// programs that have no `--end-of-options` (go, cargo), and that one survives
// a future caller that forgets to validate.
func validateArgvOperand(kind, value string) error {
	if value == "" {
		return fmt.Errorf("empty %s", kind)
	}
	if strings.HasPrefix(value, "-") {
		return fmt.Errorf("invalid %s %q: must not start with '-' (it would be parsed as a command-line option, not as a value)", kind, value)
	}
	if strings.ContainsRune(value, 0) {
		return fmt.Errorf("invalid %s %q: contains NUL", kind, value)
	}
	return nil
}
