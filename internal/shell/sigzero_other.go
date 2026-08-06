//go:build !unix

package shell

import "os"

// sigZero has no meaningful equivalent off Unix. The tests that use it are
// gated on CanKillTreeOnPlatform, which is false on those platforms, so this
// exists only to keep the package compiling.
func sigZero() os.Signal { return os.Interrupt }
