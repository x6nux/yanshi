//go:build windows

package cli

// posixModeBitsMeaningful reports whether Unix permission bits are the access
// control mechanism on this platform. On Windows they are not — access is
// governed by ACLs, and os.FileInfo.Mode synthesises the bits — so the
// permissions check abstains rather than printing a verdict it did not make.
func posixModeBitsMeaningful() bool { return false }
