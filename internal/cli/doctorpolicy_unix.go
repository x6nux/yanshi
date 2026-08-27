//go:build !windows

package cli

// posixModeBitsMeaningful reports whether Unix permission bits are the access
// control mechanism on this platform. See doctorpolicy.go's checkPolicyFilePerms
// for why the check abstains rather than guessing on the platforms where they
// are not.
func posixModeBitsMeaningful() bool { return true }
