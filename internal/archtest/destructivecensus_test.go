// Package archtest — the census of every place that consumes the guard's
// reading of a shell command.
//
// # Why this file exists at all
//
// Four rounds of W-B closed the axis "how can a command be written into this
// one string". Three ADRs (0018/0019/0020) turned "a program-name table can
// never be finished" into structural criteria, and a close-out verification
// confirmed that axis had converged.
//
// The two Blocking findings of the round after it were on a DIFFERENT axis, and
// they were the same defect twice:
//
//   - W-1: the write dimension read only the segment's first program word, so
//     one prefix runner erased it. The deletion dimension had had the
//     name-independent reading for three ADRs; the write dimension had never
//     been given it.
//   - W-2: shell v2 handed the MODEL's own "workdir" JSON to the destructive
//     dimension. shell v1 had carried a comment naming that exact bug since
//     W-B-02; v2 shipped without it.
//
// Neither is "one more spelling". Both are ONE READING WITH SEVERAL CONSUMPTION
// POINTS, WIRED AT SOME OF THEM. That axis is different from the spelling axis
// in the one way that matters: THE SPELLINGS ARE UNBOUNDED AND THE CONSUMPTION
// POINTS ARE NOT. So it can be enumerated, and a prose enumeration would be
// obsolete by the next commit — which is why this is a test and not a table in
// a document.
//
// # WHAT THIS GATE ENFORCES, AND WHAT IT DOES NOT
//
// Read this before quoting the file as evidence that anything is closed. A
// verification round demonstrated both halves, and the second half is the one
// a reader is likely to get wrong.
//
// MACHINE-ENFORCED — the KEY SET, in both directions. A production site under
// internal/ or cmd/ that supplies or consumes the reading and is not in the
// table fails; a table entry whose site no longer exists fails. "Supplies or
// consumes" is decided by a go/ast scan with a stated shape:
//
//   - a composite literal of a shell-carrying type with a non-empty Shell
//     field, WITH or WITHOUT an explicit type (an elided element type is
//     inherited one level from the enclosing slice/array/map literal),
//   - an assignment to a field named Shell, in a declaration whose vocabulary
//     includes one of those types (see mentionsShellCarryingType for why that
//     qualifier is a heuristic and what it costs),
//   - a call to ClassifyDestruction or segmentWriteTargets,
//
// found inside a FUNCTION BODY or a PACKAGE-LEVEL var/const initialiser.
// Everything outside that shape is outside the gate: sdk/ and third_party/ are
// not scanned (the same boundary GOV2/GOV3 draw), a two-level elided literal is
// not resolved, and a value whose type the AST cannot name is not typed.
//
// NOT ENFORCED — THE CONTENT OF ANY ENTRY. The second test below checks that
// Workdir is non-empty and that Note is longer than a token phrase. Both are
// LENGTH checks and neither reads the source. A verification rewrote the most load-bearing
// entry in the table (shell_v2.go::launchAction, W-2's own fix) to say the
// launch carries no boundary at all, with a fluent Note citing a sibling
// entry's argument, and both tests passed. So:
//
//	relabelling a wired site "not wired"      not caught
//	describing a Workdir as its opposite      not caught
//	an entry that is confident and wrong      not caught
//
// This is the same division CLAUDE.md draws for the exemption tables ("dead
// entries are machine-enforced; only-remove-never-add is convention"), and it
// is the division ADR-0011 draws for the ledger. The gate makes a site
// IMPOSSIBLE TO LEAVE UNDISCOVERED; whether what the entry says about it is
// true is a review question, and the same round found one entry
// (gate.go::runGate) whose third sentence contradicted the source. Checking
// three entries against the code found it. Reading the table did not.
//
// It carries no GOV number, for the same reason slashcmd_test.go does not: it
// was not part of the numbered governance spec.
package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// destructiveReadingSite is one production place where the guard's reading of a
// shell command is either SUPPLIED or CONSUMED.
//
// The two are one census on purpose. W-1 was a missing consumer and W-2 was a
// wrong supplier, and a census that only listed one kind would have found only
// one of them.
type destructiveReadingSite struct {
	// Workdir answers "where does the boundary the destructive dimension
	// measures against come from at this site". The answer that matters is
	// whether the MODEL can widen it.
	Workdir string
	// Interpreter answers "does this site tell guard which shell language the
	// string is written in" (W-B-05). Empty means the POSIX reader is used.
	Interpreter string
	// Note is the site's own standing: wired, or not wired with the reason.
	// "not wired" is a legitimate answer — several consumers treat every
	// non-Allow verdict as a refusal, which makes a missing Workdir strictly
	// tightening — but it has to be WRITTEN DOWN rather than discovered by the
	// next reviewer.
	Note string
}

// destructiveReadingCensus is the enumeration. Both directions fail: a
// production site missing from here fails (that is the third consumption point
// the next review would otherwise find), and an entry with no site fails (a
// dead entry is a licence — it pre-authorises whatever reappears under that
// key).
//
// Keys are "<module-relative file>::<enclosing function>" rather than
// file:line, so a site that moves down the file does not fail the gate. Methods
// are keyed by their own name, which is unambiguous within one file.
var destructiveReadingCensus = map[string]destructiveReadingSite{
	// ---- SUPPLIERS: the sites that build the Action guard will read --------
	"internal/tools/shell.go::stream": {
		Workdir:     "s.root — the server's work root, NOT the model's workdir arg",
		Interpreter: "prog, from shell.ShellArgv(a.Env, …)",
		Note: "wired. It is where both decisions were originally made (W-B-02, W-B-05) " +
			"and the reason W-2 was findable at all: the comment naming the bug was already " +
			"in this function while v2 shipped without the fix.",
	},
	"internal/tools/shell_v2.go::launchAction": {
		Workdir:     "v.guardWorkdir(a.Workdir) — the root, unless the arg is INSIDE it",
		Interpreter: "prog, from shell.ShellArgv(\"\", a.Command)",
		Note: "wired by W-2/D-2. Before that it passed the model's own workdir field, " +
			"which let {\"workdir\":\"/\"} move an ancestor-of-the-root deletion off the " +
			"structural floor, and passed no interpreter at all.",
	},
	"internal/secproc/secproc.go::Launch": {
		Workdir:     "spec.Workdir — passed through from the spawn site",
		Interpreter: "spec.Interpreter — passed through",
		Note: "a relay, not a decision. It cannot be wrong on its own; it is in the census " +
			"because it is the funnel every untrusted spawn goes through, so a future spec " +
			"field that stops being copied here would be invisible.",
	},
	"internal/tools/gate.go::runGate": {
		Workdir:     "withinRootAbs(WorkRootFromContext(ctx), args.Cwd) — the CANONICAL spelling",
		Interpreter: "",
		Note: "Workdir wired and narrow-only in the same sense guardWorkdir is — a model-supplied " +
			"cwd outside the root is an ERROR here rather than a wider boundary — but NOT the " +
			"same value: guardWorkdir deliberately discards withinRootAbs's RESULT and returns " +
			"filepath.Clean(argWorkdir), because on macOS the canonical form rewrites /var into " +
			"/private/var while the command's own paths still say /var, and a boundary spelled " +
			"differently from the paths measured against it stops recognising them. runGate " +
			"passes the value guardWorkdir avoids, which under a symlinked root downgrades a " +
			"delete-the-root command from the catastrophic floor to an out-of-scope Prompt. " +
			"That is a Prompt in every mode (yolo included), so it is a precision loss and not " +
			"a silent pass; giving runGate guardWorkdir's spelling is the work package. This " +
			"entry SAID the two were the same shape until a verification checked it — see the " +
			"file header on what this table is and is not.",
	},
	"internal/tools/permctx.go::Authorize": {
		Workdir:     "action.Workdir — mirrored verbatim into PermissionRequest",
		Interpreter: "not carried on PermissionRequest",
		Note: "the bridge from the Action to the mode layer. PermissionRequest has no " +
			"Interpreter field, which is why the D-2 assertion is on the Action rather than " +
			"on a callback observation.",
	},
	"internal/tools/sandboxescalate.go::askEscalation": {
		Workdir:     "WorkRootFromContext(ctx) — the server's root",
		Interpreter: "not carried on PermissionRequest",
		Note: "wired. The command text reaches resolvePermissionMode's destructive fail-safe " +
			"with the server's boundary, so a sandbox escalation cannot be approved for a " +
			"deletion the mode layer would otherwise refuse.",
	},

	"internal/guard/guard.go::Check": {
		Workdir:     "a.Workdir — copied onto the second reading with `b := a`",
		Interpreter: "a.Interpreter — copied the same way",
		Note: "the SECOND READING (ADR-0017): after expandKnownParameters resolves the " +
			"parameters the string itself defines, `b := a; b.Shell = expanded` re-enters " +
			"check with the resolved command. A supplier, and one the census did not list " +
			"for two rounds because it uses field assignment rather than a composite " +
			"literal — which is exactly the spelling the scanner was widened to see. It " +
			"cannot be wrong on its own: b inherits every other field of a, and the result " +
			"is folded with moreSevere so a resolution can only tighten.",
	},

	// ---- CONSUMERS: the sites that read a verdict back --------------------
	"internal/guard/argvwrite.go::wordWriteTargets": {
		Workdir:     "NOT SET — this is inside the reading, which never sees a boundary",
		Interpreter: "",
		Note: "COMPOSITION, not a decision: it is the write reading calling itself on a " +
			"command written into one argv word (W-B-B2-12), the way segmentWriteTargets " +
			"calls argvWriteTargets. It is listed because the scanner keys on the CALL and " +
			"cannot tell a second internal entry point from an external consumer, and " +
			"leaving it out would mean loosening the scanner. Its output goes to " +
			"checkSegmentWrites, which is where the boundary and the profile are applied.",
	},
	"internal/guard/guard.go::checkDestructive": {
		Workdir:     "a.Workdir",
		Interpreter: "not consulted — lexShellLite grades both readings and keeps the worse",
		Note:        "the primary consumer. Every tier the destructive gate has comes out of here.",
	},
	"internal/guard/guard.go::checkSegmentWrites": {
		Workdir:     "a.Workdir, forwarded to checkFS",
		Interpreter: "a.Interpreter, via segmentsFor upstream",
		Note: "the WRITE reading's only consumer, and the one W-1 was about: it looked up the " +
			"segment's first program word alone until segmentWriteTargets gave it the " +
			"suffix walk the deletion side had had since ADR-0018.",
	},
	"internal/guard/guard.go::checkShell": {
		Workdir:     "a.Workdir, carried onto each nested payload's own Action",
		Interpreter: "a.Interpreter, carried onto each nested payload's own Action",
		Note: "the wrapper-payload re-entry: a `bash -c \"…\"` payload is one quoted word to " +
			"the outer reader, so its own segments are re-segmented and put through " +
			"checkSegmentWrites. It carries the boundary and the language forward rather than " +
			"defaulting them, which is why W-1's fix reaches inside a payload too.",
	},
	"internal/api/http/ws_perm.go::resolvePermissionMode": {
		Workdir:     "req.Workdir — which is action.Workdir, mirrored by Authorize",
		Interpreter: "not available; the mode layer re-reads the raw string",
		Note: "the mode layer's fail-safe. It was reading the model's own answer for shell v2 " +
			"until W-2, because it reads the same field the Action carried.",
	},
	"internal/agent/goalloop/evaluators.go::Evaluate": {
		Workdir:     "NOT SET (empty)",
		Interpreter: "",
		Note: "NOT WIRED, deliberately. Evaluate has a workdir parameter and passes it to " +
			"cmd.Dir, so wiring it is one word — but this consumer treats every non-Allow " +
			"verdict as a refusal, so an empty Workdir can only make the gate STRICTER " +
			"(absolute paths all read as out-of-scope). Setting it would make in-scope " +
			"deletions Allow, which is a widening, and widening an acceptance-test gate is " +
			"not a change to make while closing a security round.",
	},
	"internal/acp/policy.go::OnTerminal": {
		Workdir:     "NOT SET (empty)",
		Interpreter: "",
		Note: "NOT WIRED. GuardPolicy holds a profile and no work root, and the consumer " +
			"refuses on every non-Allow verdict, so the same strictly-tightening argument as " +
			"the evaluator applies. Giving GuardPolicy a root is the work package.",
	},
	"internal/acp/policy.go::OnPermission": {
		Workdir:     "NOT SET (empty)",
		Interpreter: "",
		Note: "NOT WIRED, and weaker than its sibling for a second reason: the string it " +
			"grades is upd.Title, the agent's own DESCRIPTION of the tool call, not " +
			"necessarily the argv. Any verdict here is about a display string, which is why " +
			"the ACP path also runs OnTerminal on the real command.",
	},
}

// findDestructiveReadingSites returns every production site that builds an
// Action/PermissionRequest/SecureProcessSpec carrying a Shell command, or calls
// ClassifyDestruction directly.
//
// Both shapes are needed and neither subsumes the other: a supplier is a
// composite literal with a Shell field, and a consumer that re-reads the raw
// string (resolvePermissionMode) is a plain call with no literal anywhere near
// it. Scanning only literals would have missed the mode layer, which is one of
// the two places W-2 was observable.
//
// # The three Go spellings this used to walk past
//
// It scanned FuncDecl bodies for composite literals with an EXPLICIT type. A
// verification wired four indirect constructions into a real call chain and
// three of them stayed invisible:
//
//	act := guard.Action{Tool: "x"}; act.Shell = cmd   field assignment
//	[]guard.Action{{Shell: cmd}}                      elided element type
//	var pkgAction = guard.Action{Shell: "…"}          package-level var
//
// None of the three is a way of evading a scanner; all three are ordinary Go,
// which is the property that matters — the census exists to catch the consumer a
// future contributor adds WITHOUT noticing, and a gate that only sees the one
// spelling the current code happens to use catches nobody. The evidence that
// this was not hypothetical is in the census itself: guard.Check's second
// reading (`b := a; b.Shell = expanded`) is a supplier inside the scan roots
// that the literal-only scan never listed.
//
// The assignment shape is matched on the FIELD NAME alone, because the AST
// carries no type for `act` and running the type checker over the module to
// learn one would be a different tool. The failure direction of that is an
// EXTRA census entry on some unrelated struct with a Shell field, which costs a
// reviewable line; the direction it removes is a supplier nobody listed.
func findDestructiveReadingSites(t *testing.T) map[string]string {
	t.Helper()
	root := moduleRoot(t)
	files := goFiles(t,
		filepath.Join(root, "internal"),
		filepath.Join(root, "cmd"),
	)
	out := make(map[string]string)
	fset := token.NewFileSet()
	for _, path := range files {
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		rel := short(path, root)
		scan := func(key string, shellTyped bool, n ast.Node) {
			// elided records, for each type-less composite literal, the element
			// type its enclosing slice/array/map literal declared.
			elided := map[*ast.CompositeLit]string{}
			ast.Inspect(n, func(n ast.Node) bool {
				switch node := n.(type) {
				case *ast.CompositeLit:
					for lit, name := range elidedElementTypes(node) {
						elided[lit] = name
					}
					if shellCarryingLiteral(node, elided[node]) {
						name := typeNameOf(node.Type)
						if name == "" {
							name = elided[node]
						}
						out[key] = "builds a shell-carrying " + name
					}
				case *ast.AssignStmt:
					if shellTyped && assignsShellField(node) {
						out[key] = "assigns the Shell field of an existing value"
					}
				case *ast.CallExpr:
					if name := readingCallName(node); name != "" {
						out[key] = "calls " + name
					}
				}
				return true
			})
		}
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Body != nil {
					scan(rel+"::"+d.Name.Name, mentionsShellCarryingType(d), d.Body)
				}
			case *ast.GenDecl:
				// Package-level var/const. Keyed by the declared name, which is
				// unambiguous within one file exactly as a function name is.
				for _, spec := range d.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok || len(vs.Names) == 0 {
						continue
					}
					scan(rel+"::"+vs.Names[0].Name, mentionsShellCarryingType(vs), vs)
				}
			}
		}
	}
	return out
}

// elidedElementTypes returns the element type name that lit's type-less child
// literals inherit — `[]guard.Action{{…}}` and `map[string]guard.Action{"k": {…}}`.
//
// One level, which is the level the shape occurs at. A `[][]guard.Action{{{…}}}`
// would need the same walk twice and has never appeared; when it does, the inner
// literal is simply not listed, which the gate reports as a missing site rather
// than as silence only if some other shape in the same function is seen — so the
// boundary is written here rather than assumed away.
func elidedElementTypes(lit *ast.CompositeLit) map[*ast.CompositeLit]string {
	var elem ast.Expr
	switch t := lit.Type.(type) {
	case *ast.ArrayType:
		elem = t.Elt
	case *ast.MapType:
		elem = t.Value
	default:
		return nil
	}
	if star, ok := elem.(*ast.StarExpr); ok {
		elem = star.X
	}
	name := typeNameOf(elem)
	if name == "" {
		return nil
	}
	out := map[*ast.CompositeLit]string{}
	for _, e := range lit.Elts {
		if kv, ok := e.(*ast.KeyValueExpr); ok {
			e = kv.Value
		}
		if child, ok := e.(*ast.CompositeLit); ok && child.Type == nil {
			out[child] = name
		}
	}
	return out
}

// mentionsShellCarryingType reports whether the declaration names one of the
// shell-carrying types anywhere — in a parameter, a local literal, a conversion.
//
// It is the type check the assignment shape cannot do directly, and it is a
// HEURISTIC rather than a resolution: it says "an Action is in the vocabulary of
// this function", not "the receiver of this assignment is one. The precise
// question needs go/types over the whole module, which is a different tool from
// the one every other gate in this package uses.
//
// Named rather than inlined because it is the whole reason the assignment shape
// does not fire on internal/config/policy.go::NarrowProfile, whose
// `out.Shell = narrowShell(…)` writes a ShellPerm onto a PermissionProfile and
// has nothing to do with a command. Delete the guard and that function is the
// first thing the gate demands an entry for.
func mentionsShellCarryingType(n ast.Node) bool {
	found := false
	ast.Inspect(n, func(n ast.Node) bool {
		if found {
			return false
		}
		switch e := n.(type) {
		case *ast.SelectorExpr:
			if shellCarryingTypes[typeNameOf(e)] {
				found = true
			}
		case *ast.Ident:
			if shellCarryingTypes[e.Name] {
				found = true
			}
		}
		return !found
	})
	return found
}

// assignsShellField reports whether the statement writes a non-empty value into
// a field named Shell — `act.Shell = cmd`, the spelling a composite-literal scan
// cannot see. `x.Shell = ""` is not a command, exactly as in the literal case.
func assignsShellField(as *ast.AssignStmt) bool {
	for i, lhs := range as.Lhs {
		sel, ok := lhs.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Shell" {
			continue
		}
		if i < len(as.Rhs) {
			if bl, ok := as.Rhs[i].(*ast.BasicLit); ok && bl.Value == `""` {
				continue
			}
		}
		return true
	}
	return false
}

// shellCarryingTypes are the struct types that carry a shell command onward to
// the destructive/write reading. Matching on the TYPE rather than on the field
// name alone keeps `struct{ Shell string }` bookkeeping types out.
var shellCarryingTypes = map[string]bool{
	"Action":                    true, // guard.Action
	"PermissionRequest":         true, // tools.PermissionRequest
	"SecureProcessSpec":         true, // secproc.SecureProcessSpec
	"guard.Action":              true,
	"tools.PermissionRequest":   true,
	"secproc.SecureProcessSpec": true,
}

// shellCarryingLiteral reports whether lit is one of those types WITH a
// non-empty Shell field set. inherited is the element type name a type-less
// literal takes from the slice/array/map literal enclosing it, or "".
//
// The "non-empty" half matters: internal/guard builds Action literals for its
// own FS sub-checks that deliberately carry no command, and every tool that
// authorizes by name alone builds `guard.Action{Tool: "…"}`. Neither reaches
// the reading, and listing them would bury the sites that do.
func shellCarryingLiteral(lit *ast.CompositeLit, inherited string) bool {
	name := typeNameOf(lit.Type)
	if name == "" {
		name = inherited
	}
	if !shellCarryingTypes[name] {
		return false
	}
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		id, ok := kv.Key.(*ast.Ident)
		if !ok || id.Name != "Shell" {
			continue
		}
		// `Shell: ""` is not a command.
		if bl, ok := kv.Value.(*ast.BasicLit); ok && bl.Value == `""` {
			return false
		}
		return true
	}
	return false
}

// typeNameOf renders a composite literal's type as "Name" or "pkg.Name".
func typeNameOf(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		if id, ok := t.X.(*ast.Ident); ok {
			return id.Name + "." + t.Sel.Name
		}
		return t.Sel.Name
	}
	return ""
}

// readingEntryPoints are the two readings themselves, by their entry-point
// name: the deletion classifier and the write-target extractor.
//
// Only the ENTRY points are listed, not their helpers. Listing argvWriteTargets
// as well would enter segmentWriteTargets — the reading's own composition — into
// a census of the places that CONSUME it, which is noise that makes the real
// entries harder to see.
var readingEntryPoints = map[string]bool{
	"ClassifyDestruction": true, // the deletion reading
	"segmentWriteTargets": true, // the write reading
}

// readingCallName returns the reading ce invokes, in either the qualified or the
// same-package spelling, or "" if it invokes neither.
func readingCallName(ce *ast.CallExpr) string {
	switch fn := ce.Fun.(type) {
	case *ast.Ident:
		if readingEntryPoints[fn.Name] {
			return fn.Name
		}
	case *ast.SelectorExpr:
		if readingEntryPoints[fn.Sel.Name] {
			return fn.Sel.Name
		}
	}
	return ""
}

// TestDestructiveReadingConsumptionPointsAreEnumerated is the gate.
//
// A new production site that supplies or consumes the shell reading fails until
// it is entered in the census with its three answers. That is the whole point:
// W-1 and W-2 were both a reading that existed and a site that did not use it,
// and neither was visible from the site that DID use it.
func TestDestructiveReadingConsumptionPointsAreEnumerated(t *testing.T) {
	sites := findDestructiveReadingSites(t)
	if len(sites) < 8 {
		t.Fatalf("only %d sites found — the scanner is almost certainly broken; the "+
			"census itself lists %d", len(sites), len(destructiveReadingCensus))
	}

	var missing []string
	for key, what := range sites {
		if _, listed := destructiveReadingCensus[key]; !listed {
			missing = append(missing, key+"  ("+what+")")
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("%d production site(s) supply or consume the guard's shell reading and are "+
			"not in destructiveReadingCensus:\n  %s\n\n"+
			"This axis is FINITE, which is the only reason it can be closed. Add an entry "+
			"answering three questions: where the destructive boundary comes from (and "+
			"whether the model can widen it), whether the interpreter is handed over, and "+
			"whether the site is wired — \"not wired, because …\" is a legitimate answer, "+
			"being undiscovered is not.",
			len(missing), strings.Join(missing, "\n  "))
	}

	var dead []string
	for key := range destructiveReadingCensus {
		if _, found := sites[key]; !found {
			dead = append(dead, key)
		}
	}
	sort.Strings(dead)
	if len(dead) > 0 {
		t.Errorf("%d census entr(ies) name a site that no longer supplies or consumes the "+
			"shell reading (renamed, moved or deleted):\n  %s\n\n"+
			"Delete the entry. A dead entry is a licence: it pre-authorises whatever "+
			"reappears under that key, which is how an unwired subject stays invisible.",
			len(dead), strings.Join(dead, "\n  "))
	}
}

// TestDestructiveCensusEntriesAnswerTheQuestions refuses a census entry that is
// present but says nothing.
//
// Without it the gate above is satisfiable by pasting a key with three empty
// strings, which is exactly the move that makes an exemption table useless —
// the entry exists, the reviewer's eye slides over it, and nothing was decided.
// Workdir and Note must be non-empty; Interpreter may legitimately be empty
// (that IS the answer for the sites that hand over no language), so it is the
// one field not required.
//
// # WHAT IT DOES NOT DO, STATED HERE SO THE NEXT READER DOES NOT INFER IT
//
// It measures LENGTH. It does not read the site, so it cannot tell a true
// answer from a false one, and a measured experiment confirmed that an entry
// can be rewritten into a fluent, self-consistent lie and stay green. "Not
// wired, because …" being a legitimate answer is what makes that possible: the
// legitimacy is judged by a human, and there is no other judge. The file
// header's "WHAT THIS GATE ENFORCES" section is the whole division; do not
// quote this test as evidence that the table is accurate.
func TestDestructiveCensusEntriesAnswerTheQuestions(t *testing.T) {
	for key, site := range destructiveReadingCensus {
		if strings.TrimSpace(site.Workdir) == "" {
			t.Errorf("%s: Workdir is blank. Write where the destructive boundary comes from, "+
				"or \"NOT SET (empty)\" if it does not.", key)
		}
		if len(strings.TrimSpace(site.Note)) < 40 {
			t.Errorf("%s: Note is missing or too short to say anything. It has to state "+
				"whether the site is wired, and if not, why that is acceptable.", key)
		}
	}
}
