package guard

import (
	"path/filepath"
	"strings"
	"testing"
)

// wideWriteProfile is the shipped example profile's FS posture: everything
// readable, everything writable. Every case below runs under it, because the
// hole W-B-18 closes only exists when the write globs already say yes.
func wideWriteProfile() PermissionProfile {
	return PermissionProfile{
		Tools: ToolsPerm{Allow: []string{"*"}},
		FS:    FSPerm{Read: []string{"**"}, Write: []string{"**"}},
		Shell: ShellPerm{Policy: "denylist"},
	}
}

// TestProtectedMetadataWriteIsRefusedInsideTheWritableRoot is the W-B-18
// acceptance: "即使在可写根下也拒写 .git".
//
// Each case is a DIFFERENT route into the FS write dimension, because the gate
// lives in checkFS and three separate readers feed it. A fix wired into only
// the fs_write path would leave the two shell routes exactly as they were, and
// `> .git/hooks/pre-commit` is the cheaper of the two to reach for.
func TestProtectedMetadataWriteIsRefusedInsideTheWritableRoot(t *testing.T) {
	g := New()
	work := t.TempDir()
	hook := filepath.ToSlash(filepath.Join(work, ".git", "hooks", "pre-commit"))

	cases := []struct {
		name   string
		action Action
	}{
		{"fs_write tool", Action{
			Tool:    "fs_write",
			Workdir: work,
			FS:      FSWant{Op: "write", Paths: []string{hook}},
		}},
		{"shell redirection", Action{
			Tool:    "shell_run",
			Workdir: work,
			Shell:   "echo hi > " + hook,
		}},
		{"shell write operand", Action{
			Tool:    "shell_run",
			Workdir: work,
			Shell:   "tee " + hook,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := g.Check(wideWriteProfile(), tc.action)
			if d.Verdict == Allow {
				t.Fatalf("write into .git was allowed under write:[**]: %+v", d)
			}
			if !strings.Contains(d.Reason, ".git") {
				t.Fatalf("denial does not name the directory it refused: %q", d.Reason)
			}
		})
	}
}

// TestProtectedMetadataIsWriteOnlyAndSegmentExact keeps the gate from becoming
// the kind of false-positive machine operators switch off.
//
// Two ways it could: refusing READS (every git command reads .git/config), and
// prefix-matching instead of segment-matching (.github holds ordinary
// reviewable YAML and is where CI lives, so refusing writes to it would break
// a routine task with a security-shaped message).
func TestProtectedMetadataIsWriteOnlyAndSegmentExact(t *testing.T) {
	g := New()
	work := t.TempDir()
	prof := wideWriteProfile()

	read := Action{Tool: "fs_read", Workdir: work, FS: FSWant{Op: "read",
		Paths: []string{filepath.ToSlash(filepath.Join(work, ".git", "config"))}}}
	if d := g.Check(prof, read); d.Verdict != Allow {
		t.Fatalf("reading .git/config must stay ordinary work, got %+v", d)
	}

	github := Action{Tool: "fs_write", Workdir: work, FS: FSWant{Op: "write",
		Paths: []string{filepath.ToSlash(filepath.Join(work, ".github", "workflows", "ci.yml"))}}}
	if d := g.Check(prof, github); d.Verdict != Allow {
		t.Fatalf(".github is not .git; writing a workflow must not be refused: %+v", d)
	}
}

// TestProtectedMetadataAcceptsTheLiteralGrantAndOperatorAdditions covers the
// two ends of "受保护路径集合可配".
//
// The escape hatch is the same one every other gate in sensitive.go uses: a
// pattern with no wildcard in it is an operator naming the path on purpose.
// The additions go the other way and are additive only — the built-in entries
// survive a config that lists something else entirely, which is what stops a
// writable config.yaml from being a way to reopen the gate.
func TestProtectedMetadataAcceptsTheLiteralGrantAndOperatorAdditions(t *testing.T) {
	g := New()
	work := t.TempDir()
	hook := filepath.ToSlash(filepath.Join(work, ".git", "hooks", "pre-commit"))

	granted := wideWriteProfile()
	granted.FS.Write = append(granted.FS.Write, hook)
	if d := g.Check(granted, Action{Tool: "fs_write", Workdir: work,
		FS: FSWant{Op: "write", Paths: []string{hook}}}); d.Verdict != Allow {
		t.Fatalf("a literal fs.write grant must admit the path, got %+v", d)
	}

	custom := wideWriteProfile()
	custom.FS.Protected = []string{".myagent"}
	mine := filepath.ToSlash(filepath.Join(work, ".myagent", "instructions.md"))
	if d := g.Check(custom, Action{Tool: "fs_write", Workdir: work,
		FS: FSWant{Op: "write", Paths: []string{mine}}}); d.Verdict == Allow {
		t.Fatalf("operator addition .myagent was not honoured: %+v", d)
	}
	// ...and the built-ins are still there next to it.
	if d := g.Check(custom, Action{Tool: "fs_write", Workdir: work,
		FS: FSWant{Op: "write", Paths: []string{hook}}}); d.Verdict == Allow {
		t.Fatalf("configuring an addition dropped the built-in set: %+v", d)
	}
}

// TestProtectedMetadataSurvivesAnExpansionInTheSegment pins the elided second
// reading.
//
// The matcher compares literal directory segments, so an unresolved expansion
// spliced into one breaks the comparison without changing where the write
// lands. sensitive.go already learned this for ~/.s${x}sh/authorized_keys; a
// third matcher that did not inherit it would be the same hole at a new
// anchor.
func TestProtectedMetadataSurvivesAnExpansionInTheSegment(t *testing.T) {
	work := t.TempDir()
	target := filepath.ToSlash(filepath.Join(work, ".g${x}it", "hooks", "pre-commit"))
	entry, hit := IsProtectedMetadataPath(target, work, nil)
	if !hit || entry != ".git" {
		t.Fatalf("expansion inside the segment escaped the gate: entry=%q hit=%v", entry, hit)
	}
}
