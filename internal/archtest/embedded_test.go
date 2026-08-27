package archtest

import (
	"os"
	"path/filepath"
	"testing"
)

// embeddedExampleCopies pairs each compiled-in asset with the repository file it
// must stay byte-identical to.
//
// This is a MIRROR, not a debt table: it exists because //go:embed cannot reach
// outside its own package directory, so an asset that must both (a) ship inside
// the binary and (b) be browsable at the repository root has to exist twice.
// Two copies of one file drift silently — the failure mode is an operator
// running `yanshi init` and getting a config that does not match the
// config.example.yaml they were reading on GitHub, which is unfalsifiable from
// either side alone.
var embeddedExampleCopies = map[string]string{
	filepath.Join("internal", "cli", "embedded", "config.example.yaml"): "config.example.yaml",
}

// TestEmbeddedExampleConfigMatchesRoot verifies every compiled-in copy of a
// repository asset is byte-identical to the original.
//
// The root config.example.yaml is the authoritative, documented, human-edited
// file; the embedded copy is what `yanshi init` and `yanshi doctor -fix`
// actually write when the operator is not standing in the source tree, which is
// every real first run. Editing one and not the other means the binary ships a
// config the docs do not describe.
//
// Failure is reported as "copy these bytes", not "reconcile these files":
// the root file is the source of truth by construction, so the fix is always
// the same one-line cp.
func TestEmbeddedExampleConfigMatchesRoot(t *testing.T) {
	root := moduleRoot(t)
	for copyRel, sourceRel := range embeddedExampleCopies {
		copyPath := filepath.Join(root, copyRel)
		sourcePath := filepath.Join(root, sourceRel)

		source, err := os.ReadFile(sourcePath)
		if err != nil {
			t.Errorf("read authoritative %s: %v", sourceRel, err)
			continue
		}
		embedded, err := os.ReadFile(copyPath)
		if err != nil {
			t.Errorf("read embedded copy %s: %v — the binary would fail to build, "+
				"or the mirror entry is stale", copyRel, err)
			continue
		}
		if string(embedded) != string(source) {
			t.Errorf("%s has drifted from %s (%d vs %d bytes).\n"+
				"The root file is authoritative; refresh the embedded copy:\n"+
				"    cp %s %s",
				copyRel, sourceRel, len(embedded), len(source), sourceRel, copyRel)
		}
	}
}
