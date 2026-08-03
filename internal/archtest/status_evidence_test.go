package archtest

// Ledger evidence gate (GOV8) — clause-level accounting for terminal verdicts.
//
// WHY THIS EXISTS
//
// The first version of the ledger gate (status_test.go) only checked that an
// evidence reference RESOLVED: the package directory exists, the test function
// name exists. It never counted anything. Acceptance criteria in this ledger
// are conjunctions — "可创建计划任务；按时触发入队；生命周期可控；持久化；
// approval 门禁" is five separate promises — so a single resolvable reference
// could hold up all five, and an entry could be flipped to done on the
// strength of its FIRST clause. Three review rounds found nine entries that
// had done exactly that. Nine instances of one hole is not nine mistakes; it
// is a missing gate.
//
// THE HANDSHAKE
//
// Counting alone does not fix it: "evidence count >= clause count" is passed
// by pasting five arbitrary test names. So the accounting here is two-sided.
//
//	forward   the ledger names, per clause index, the test(s) that prove it
//	backward  each named test's OWN doc comment carries a matching
//	          "ledger: <ID>#<n> <clause text>" line, echoing the clause verbatim
//
// A false claim therefore cannot be made from inside docs/feature-status.yaml
// alone. To pad an entry you must open the unrelated test file and write, in
// the comment directly above the test body, the sentence "this test proves
// <clause>". That claim is located at the point where it is falsifiable, shows
// up in the diff next to the code that contradicts it, and is the first thing
// anyone reading that test sees. The gate cannot judge semantic coverage — no
// gate can — but it can force the claim to be stated where lying is expensive.
//
// Legitimate one-to-many coverage is unaffected: a test that really does prove
// two clauses carries two marker lines and is cited by both.
//
// The backward scan also fails on STALE markers — a test claiming a clause the
// ledger no longer cites it for. Same semantics W0 established for
// lineExceptions and docExceptionSymbols: exemptions shrink, dead entries are
// errors, nothing rots quietly.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// evidenceField holds the "evidence:" value of a ledger entry, which is a
// scalar for non-terminal entries (always the empty string — there is nothing
// to prove yet) and a clause-index → references mapping for terminal ones.
//
// The two shapes share one field because they answer the same question at
// different resolutions, and a second field would let a terminal entry keep a
// stale scalar around as a decoy.
type evidenceField struct {
	// Scalar is the raw value when the YAML node was a plain string.
	Scalar string
	// Clauses maps a 1-based acceptance clause index to that clause's
	// "; "-separated evidence references. Nil unless Mapped is true.
	Clauses map[int]string
	// Mapped reports whether the YAML node was a mapping.
	Mapped bool
}

// UnmarshalYAML accepts either a scalar or a clause-index mapping and rejects
// every other node kind, so a typo like a YAML list fails loudly at load time
// rather than silently decoding to an empty evidence set.
func (e *evidenceField) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		e.Scalar = node.Value
		return nil
	case yaml.MappingNode:
		e.Mapped = true
		e.Clauses = make(map[int]string, len(node.Content)/2)
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := strings.TrimSpace(node.Content[i].Value)
			n, err := strconv.Atoi(key)
			if err != nil {
				return fmt.Errorf("evidence key %q is not an acceptance clause number", key)
			}
			if _, dup := e.Clauses[n]; dup {
				return fmt.Errorf("evidence clause %d listed twice", n)
			}
			e.Clauses[n] = node.Content[i+1].Value
		}
		return nil
	default:
		return fmt.Errorf("evidence must be a string or a clause-number mapping, got YAML kind %d", node.Kind)
	}
}

// Empty reports whether the entry carries no evidence at all.
func (e evidenceField) Empty() bool {
	return !e.Mapped && strings.TrimSpace(e.Scalar) == ""
}

// clauseSeparators are the two semicolons the ledger uses interchangeably.
// Entries were written by different hands; normalising here is cheaper than a
// pass over 63 entries and removes a way to hide a clause behind the wrong
// codepoint.
const clauseSeparators = "；;"

// splitClauses breaks an acceptance string into its conjunctive clauses.
func splitClauses(acceptance string) []string {
	var out []string
	for _, c := range strings.FieldsFunc(acceptance, func(r rune) bool {
		return strings.ContainsRune(clauseSeparators, r)
	}) {
		if c = strings.TrimSpace(c); c != "" {
			out = append(out, c)
		}
	}
	return out
}

// splitRefs breaks one clause's evidence value into individual references.
func splitRefs(v string) []string {
	var out []string
	for _, r := range strings.Split(v, ";") {
		if r = strings.TrimSpace(r); r != "" {
			out = append(out, r)
		}
	}
	return out
}

// ledgerMarker is one "ledger: <ID>#<n> <clause>" claim found in a test's doc
// comment.
type ledgerMarker struct {
	// Pkg is the module-relative package path, e.g. "internal/tools".
	Pkg string
	// Test is the test function whose doc comment carried the marker.
	Test string
	// ID is the ledger entry claimed, e.g. "C1/AU1".
	ID string
	// Clause is the 1-based acceptance clause index claimed.
	Clause int
	// Text is the clause text as transcribed into the comment.
	Text string
	// File is the module-relative path of the test file.
	File string
}

// markerRe matches a marker line inside a doc comment. The ID group excludes
// "#" so that "D2/O12#1" splits at the right place.
var markerRe = regexp.MustCompile(`(?m)^\s*ledger:\s*([^\s#]+)#(\d+)\s+(\S.*?)\s*$`)

// parseMarkers extracts every ledger marker from a doc comment body.
func parseMarkers(doc string) []ledgerMarker {
	var out []ledgerMarker
	for _, m := range markerRe.FindAllStringSubmatch(doc, -1) {
		n, err := strconv.Atoi(m[2])
		if err != nil {
			continue
		}
		out = append(out, ledgerMarker{ID: m[1], Clause: n, Text: strings.TrimSpace(m[3])})
	}
	return out
}

// testDocs returns, for every top-level test function declared in pkgDir, its
// doc comment body keyed by function name.
func testDocs(t *testing.T, pkgDir string) map[string]string {
	t.Helper()
	docs := map[string]string{}
	matches, err := filepath.Glob(filepath.Join(pkgDir, "*_test.go"))
	if err != nil {
		return docs
	}
	fset := token.NewFileSet()
	for _, path := range matches {
		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			continue
		}
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Recv != nil {
				continue
			}
			if fd.Doc != nil {
				docs[fd.Name.Name] = fd.Doc.Text()
			} else if _, seen := docs[fd.Name.Name]; !seen {
				docs[fd.Name.Name] = ""
			}
		}
	}
	return docs
}

// checkTerminalEvidence audits one terminal entry and returns its problems.
//
// Rules, in the order a reader should think about them:
//
//  1. evidence must be a clause mapping (a bare string cannot say which clause
//     it covers, which is the whole failure this gate exists for);
//  2. its keys must be exactly 1..N for N acceptance clauses — a missing key
//     is an unproven promise, an extra key is a clause that no longer exists;
//  3. every reference must be a TEST reference. A terminal verdict asserts
//     behaviour, and only an executable assertion can carry that claim; a file
//     path proves a file exists;
//  4. each cited test must echo the clause back from its own doc comment.
func checkTerminalEvidence(t *testing.T, root string, e ledgerEntry) []string {
	t.Helper()
	clauses := splitClauses(e.Acceptance)
	if len(clauses) == 0 {
		return []string{e.ID + ": acceptance has no clauses to prove"}
	}
	if !e.Evidence.Mapped {
		return []string{fmt.Sprintf("%s: verdict %s needs per-clause evidence "+
			"(a mapping of clause number -> reference, one key per acceptance "+
			"clause; this entry has %d clauses), not a flat string",
			e.ID, e.Verdict, len(clauses))}
	}

	var problems []string
	for n := range e.Evidence.Clauses {
		if n < 1 || n > len(clauses) {
			problems = append(problems, fmt.Sprintf("%s: evidence names clause %d "+
				"but acceptance has %d clause(s) — stale evidence for a clause that "+
				"no longer exists", e.ID, n, len(clauses)))
		}
	}
	for i, clause := range clauses {
		n := i + 1
		raw, ok := e.Evidence.Clauses[n]
		if !ok {
			problems = append(problems, fmt.Sprintf("%s: acceptance clause %d (%q) "+
				"has no evidence — a terminal verdict must prove EVERY clause, not "+
				"the first one", e.ID, n, clause))
			continue
		}
		refs := splitRefs(raw)
		if len(refs) == 0 {
			problems = append(problems, fmt.Sprintf("%s: clause %d (%q) has an empty "+
				"evidence value", e.ID, n, clause))
			continue
		}
		for _, ref := range refs {
			if !strings.Contains(ref, "::") {
				problems = append(problems, fmt.Sprintf("%s: clause %d (%q) cites %q — "+
					"terminal evidence must be a TEST reference (pkg/path::TestName); a "+
					"file reference proves a file exists, not that the clause holds",
					e.ID, n, clause, ref))
				continue
			}
			if reason := checkEvidence(t, root, ref); reason != "" {
				problems = append(problems, fmt.Sprintf("%s: clause %d bad evidence — %s",
					e.ID, n, reason))
				continue
			}
			pkg, name, _ := strings.Cut(ref, "::")
			docs := testDocs(t, filepath.Join(root, filepath.FromSlash(pkg)))
			var matched bool
			var transcribed []string
			for _, mk := range parseMarkers(docs[name]) {
				if mk.ID != e.ID || mk.Clause != n {
					continue
				}
				if mk.Text == clause {
					matched = true
					break
				}
				transcribed = append(transcribed, mk.Text)
			}
			if matched {
				continue
			}
			if len(transcribed) > 0 {
				problems = append(problems, fmt.Sprintf("%s: clause %d — %s transcribes "+
					"the clause as %q but the ledger says %q; one of the two is wrong",
					e.ID, n, ref, transcribed[0], clause))
				continue
			}
			problems = append(problems, fmt.Sprintf("%s: clause %d (%q) cites %s, but "+
				"that test's doc comment does not claim it. Add the line "+
				"\"ledger: %s#%d %s\" to the test's doc comment — the claim belongs "+
				"where it can be checked against the test body",
				e.ID, n, clause, ref, e.ID, n, clause))
		}
	}
	return problems
}

// TestLedgerEvidenceIsClauseComplete is the forward half of the handshake:
// every acceptance clause of every terminal entry names a test, and that test
// claims the clause back.
func TestLedgerEvidenceIsClauseComplete(t *testing.T) {
	root := moduleRoot(t)
	var problems []string
	for _, e := range loadLedger(t) {
		if !terminalVerdicts[e.Verdict] || e.Evidence.Empty() {
			continue // absence of evidence is status_test.go's error to report
		}
		problems = append(problems, checkTerminalEvidence(t, root, e)...)
	}
	if len(problems) > 0 {
		t.Errorf("ledger evidence does not cover its acceptance criteria (%d problem(s)):\n  %s",
			len(problems), strings.Join(problems, "\n  "))
	}
}

// TestLedgerMarkersAreLive is the backward half: no test may claim a ledger
// clause that the ledger does not cite it for.
//
// Without this, flipping an entry back to partial would leave the old claims
// standing in the test files, and the next person to read them would believe a
// promise the ledger had already withdrawn.
func TestLedgerMarkersAreLive(t *testing.T) {
	root := moduleRoot(t)

	cited := map[string]bool{} // "ID#n|pkg::Test"
	clauseText := map[string]string{}
	for _, e := range loadLedger(t) {
		clauses := splitClauses(e.Acceptance)
		for n, raw := range e.Evidence.Clauses {
			if n >= 1 && n <= len(clauses) {
				clauseText[fmt.Sprintf("%s#%d", e.ID, n)] = clauses[n-1]
			}
			for _, ref := range splitRefs(raw) {
				cited[fmt.Sprintf("%s#%d|%s", e.ID, n, ref)] = true
			}
		}
	}

	var problems []string
	for _, dir := range []string{"internal", "cmd"} {
		root := root
		base := filepath.Join(root, dir)
		err := filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, "_test.go") {
				return err
			}
			pkgRel := filepath.ToSlash(mustRel(t, root, filepath.Dir(path)))
			for name, doc := range testDocs(t, filepath.Dir(path)) {
				for _, mk := range parseMarkers(doc) {
					key := fmt.Sprintf("%s#%d", mk.ID, mk.Clause)
					ref := pkgRel + "::" + name
					if !cited[key+"|"+ref] {
						problems = append(problems, fmt.Sprintf("%s claims %q but the "+
							"ledger does not cite it for that clause — either the ledger "+
							"moved on (delete the marker) or the citation is missing",
							ref, key))
						continue
					}
					if want := clauseText[key]; want != "" && mk.Text != want {
						problems = append(problems, fmt.Sprintf("%s transcribes %s as %q, "+
							"ledger says %q", ref, key, mk.Text, want))
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	problems = dedup(problems)
	if len(problems) > 0 {
		t.Errorf("stale or mistranscribed ledger markers (%d):\n  %s",
			len(problems), strings.Join(problems, "\n  "))
	}
}

// mustRel is filepath.Rel with the error folded into the test.
func mustRel(t *testing.T, base, target string) string {
	t.Helper()
	rel, err := filepath.Rel(base, target)
	if err != nil {
		t.Fatal(err)
	}
	return rel
}

// dedup removes repeated strings, preserving first-seen order. The walk visits
// every *_test.go in a package but testDocs already covers the whole package,
// so findings would otherwise repeat once per file.
func dedup(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := in[:0]
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
