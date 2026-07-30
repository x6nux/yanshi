// Package archtest — architecture governance tests for GOV3 exported-symbol
// doc coverage.
//
// This file enforces that all exported symbols (funcs, types, vars, consts,
// methods on exported types) in internal/ and cmd/ have doc comments. Whole-
// package exemptions live in docExceptionPkgs; individual symbol exemptions
// live in docExceptionSymbols. Exceptions must only be removed, never added.
package archtest

import (
	"path/filepath"
	"strings"
	"testing"
)

// docExceptionPkgs: whole-package exemptions (main, generated files, etc.)
var docExceptionPkgs = map[string]bool{
	"cmd/yanshi": true, // main package - entry point
}

// docExceptionSymbols: single-symbol exemptions (generated fields, test helpers, etc.)
// Populated only when a symbol is genuinely noisy and not worth documenting.
// Only removal, never addition.
var docExceptionSymbols = map[string]bool{}

// TestExportedDocs verifies that every exported symbol in internal/ and cmd/
// has a doc comment. Missing doc comments cause the test to fail, listing each
// undocumented symbol by file, line, kind, and name.
func TestExportedDocs(t *testing.T) {
	root := moduleRoot(t)
	files := goFiles(t,
		filepath.Join(root, "internal"),
		filepath.Join(root, "cmd"),
	)
	var missing []string
	for _, f := range files {
		rel, _ := filepath.Rel(root, filepath.Dir(f))
		pkgRel := filepath.ToSlash(rel)
		if docExceptionPkgs[pkgRel] {
			continue
		}
		for _, d := range scanExported(t, f) {
			if d.HasDoc {
				continue
			}
			key := pkgRel + "." + d.Name
			if docExceptionSymbols[key] {
				continue
			}
			missing = append(missing, short(f, root)+":"+itoa(d.Line)+": exported "+d.Kind+" "+d.Name+" lacks doc comment")
		}
	}
	if len(missing) > 0 {
		t.Fatalf("Undocumented exported symbols (add doc comments or register in docExceptionSymbols):\n  %s", strings.Join(missing, "\n  "))
	}
}

// itoa converts an integer to its decimal string representation without importing
// strconv (keeping archtest zero-dependency beyond the standard library).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [16]byte
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[pos:])
}
