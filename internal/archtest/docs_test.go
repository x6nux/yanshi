// Package archtest — architecture governance tests for GOV3 exported-symbol
// doc coverage.
//
// This file enforces that all exported symbols (funcs, types, vars, consts,
// methods on exported types) in internal/ and cmd/ have doc comments. Whole-
// package exemptions live in docExceptionPkgs; individual symbol exemptions
// live in docExceptionSymbols.
//
// Both tables carry the same semantics W0 gave lineExceptions: entries may
// only be REMOVED, never added, AND a dead entry is an error. An exemption
// whose subject has become compliant (or has disappeared) no longer excuses
// anything; leaving it in place pre-authorises the next regression and lets
// the table read as if the debt were larger than it is.
package archtest

import (
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// docExceptionPkgs: whole-package exemptions (main, generated files, etc.)
// Only removal, never addition. An exempted package with no undocumented
// exported symbols left — or no longer present at all — is a dead entry and
// fails the test.
var docExceptionPkgs = map[string]bool{}

// docExceptionSymbols: single-symbol exemptions (generated fields, test helpers, etc.)
// Populated only when a symbol is genuinely noisy and not worth documenting.
// Only removal, never addition. Keys are "pkg/path.SymbolName"; an entry whose
// symbol now has a doc comment, or no longer exists, is a dead entry and fails
// the test.
var docExceptionSymbols = map[string]bool{}

// TestExportedDocs verifies that every exported symbol in internal/ and cmd/
// has a doc comment. Missing doc comments cause the test to fail, listing each
// undocumented symbol by file, line, kind, and name.
//
// Exempted packages and symbols are still scanned — the scan result is what
// tells us whether their exemption is still doing any work.
func TestExportedDocs(t *testing.T) {
	root := moduleRoot(t)
	files := goFiles(t,
		filepath.Join(root, "internal"),
		filepath.Join(root, "cmd"),
	)

	var missing []string
	scannedPkgs := map[string]bool{}
	undocumentedInPkg := map[string]int{}
	// symbolHasDoc maps "pkg/path.Name" to whether EVERY declaration under
	// that key is documented. Methods of different types can share a name, so
	// one undocumented occurrence is enough to keep an exemption alive.
	symbolHasDoc := map[string]bool{}

	for _, f := range files {
		rel, _ := filepath.Rel(root, filepath.Dir(f))
		pkgRel := filepath.ToSlash(rel)
		scannedPkgs[pkgRel] = true
		for _, d := range scanExported(t, f) {
			key := pkgRel + "." + d.Name
			if d.HasDoc {
				if _, seen := symbolHasDoc[key]; !seen {
					symbolHasDoc[key] = true
				}
				continue
			}
			symbolHasDoc[key] = false
			undocumentedInPkg[pkgRel]++
			if docExceptionPkgs[pkgRel] || docExceptionSymbols[key] {
				continue
			}
			missing = append(missing, short(f, root)+":"+itoa(d.Line)+": exported "+d.Kind+" "+d.Name+" lacks doc comment")
		}
	}

	var dead []string
	for pkgRel := range docExceptionPkgs {
		switch {
		case !scannedPkgs[pkgRel]:
			dead = append(dead, pkgRel+": package no longer exists under internal/ or cmd/ — delete the docExceptionPkgs entry")
		case undocumentedInPkg[pkgRel] == 0:
			dead = append(dead, pkgRel+": every exported symbol in this package now has a doc comment — delete the docExceptionPkgs entry (no dead entries)")
		}
	}
	for key := range docExceptionSymbols {
		hasDoc, exists := symbolHasDoc[key]
		switch {
		case !exists:
			dead = append(dead, key+": no such exported symbol — delete the docExceptionSymbols entry")
		case hasDoc:
			dead = append(dead, key+": this symbol now has a doc comment — delete the docExceptionSymbols entry (no dead entries)")
		}
	}
	sort.Strings(dead)

	if len(missing) > 0 {
		t.Errorf("Undocumented exported symbols (add doc comments or register in docExceptionSymbols):\n  %s", strings.Join(missing, "\n  "))
	}
	if len(dead) > 0 {
		t.Errorf("GOV3: %d dead doc exemption(s) — the exempted subject is now compliant "+
			"or gone, so the exemption must be DELETED:\n  %s", len(dead), strings.Join(dead, "\n  "))
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
