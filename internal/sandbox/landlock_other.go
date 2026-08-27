//go:build !linux

package sandbox

import "errors"

// RunLandlockHelper is the non-Linux stub for the re-exec helper.
//
// It exists so the CLI's dispatcher can reference the helper unconditionally.
// Guarding the call site with a build tag instead would put a platform fork in
// package main and, worse, make the token dispatch itself platform-specific:
// the argv would then be accepted on Linux and fall through to the ordinary
// subcommand switch everywhere else, where `__landlock_exec` is an unknown
// subcommand and the error message would talk about a typo rather than about a
// binary being invoked in a way only its own Linux sandbox should invoke it.
//
// Returning an error rather than panicking keeps the failure inside the same
// channel every other dispatch failure uses. It is not reachable in normal
// operation: only the Linux Landlock backend ever produces this argv.
func RunLandlockHelper([]string) error {
	return errors.New("sandbox: the Landlock helper is Linux-only; " +
		"this binary was invoked with the helper token on a platform that has no Landlock")
}
