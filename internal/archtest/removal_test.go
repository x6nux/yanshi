// Package archtest — removal assertions.
//
// Some audit items close by deleting code rather than finishing it. A ledger
// entry with verdict "removed" points here: the evidence for "we removed it"
// has to be a test that fails if it comes back.
package archtest

import (
	"os"
	"path/filepath"
	"testing"
)

// TestVSCodeExtensionRemoved asserts the VS Code extension and its CI helper
// are gone.
//
// Audit item D2/O12 closed by removal (spec §3.2 ④): the extension was never
// finished (runWithRecovery was never imported by extension.ts, and the
// README advertised reconnect behaviour that did not exist), and with TUI and
// the Web IDE both first-class, a third front end has an overlapping audience
// and does not earn its maintenance cost.
//
// This is the evidence backing that ledger entry — if either path reappears,
// it must come with a decision to reverse §3.2 ④.
func TestVSCodeExtensionRemoved(t *testing.T) {
	root := moduleRoot(t)
	for _, rel := range []string{
		"ide/vscode",
		"scripts/check-d2.sh",
	} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err == nil {
			t.Errorf("%s still exists — audit item D2/O12 closed by removal "+
				"(spec §3.2 ④). If this is a deliberate reversal, update the spec "+
				"and the ledger entry first.", rel)
		}
	}
}
