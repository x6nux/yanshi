package tools

import (
	"os"
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/guard"
)

// TestGateActionCarriesWorkdir pins the field that connects task_gate_run to
// the destructive-deletion dimension.
//
// guard.ClassifyDestruction needs a workdir to apply its two workdir-relative
// rules — "deletes the working directory itself" and "deletes an ancestor of
// it". With Workdir empty it short-circuits past both, so a gate command could
// remove the tree it was invoked in while shell_run, which does pass its root,
// refused the identical string.
//
// Asserted against the classifier rather than through the tool, because the
// tool needs a task manager, a store and a live process to reach its Authorize
// call — and a test that heavy would be pinning the plumbing, not the rule.
func TestGateActionCarriesWorkdir(t *testing.T) {
	const wd = "/home/me/proj"
	cmd := "rm -rf " + wd

	withWorkdir := guard.ClassifyDestruction(cmd, wd)
	if withWorkdir != guard.DestructionCatastrophic {
		t.Errorf("with Workdir: got %v, want Catastrophic — deleting the workdir must be refused in every mode", withWorkdir)
	}

	without := guard.ClassifyDestruction(cmd, "")
	if without == guard.DestructionCatastrophic {
		t.Error("guard: the empty-Workdir case now classifies as Catastrophic; " +
			"this test's premise (that Workdir is what enables the rule) needs rechecking")
	}

	// The production call site must therefore set it.
	src, err := os.ReadFile("gate.go")
	if err != nil {
		t.Fatalf("read gate.go: %v", err)
	}
	if !strings.Contains(string(src), "Workdir: cwdResolved") {
		t.Error("gate.go's guard.Action no longer sets Workdir: the destructive " +
			"dimension silently stops applying its workdir-relative rules")
	}
}
