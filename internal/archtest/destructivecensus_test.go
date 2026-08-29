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
		Workdir:     "withinRootAbs(WorkRootFromContext(ctx), args.Cwd) — canonical, narrow-only",
		Interpreter: "",
		Note: "Workdir wired, and it is the same narrowing-only shape guardWorkdir uses: a " +
			"model-supplied cwd outside the root is an ERROR here rather than a wider " +
			"boundary. Interpreter is absent and that is a real gap on Windows only — the " +
			"gate accepts a single argv (metacharacters are refused), which is the shape " +
			"least affected by picking the wrong reader.",
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

	// ---- CONSUMERS: the sites that read a verdict back --------------------
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
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			key := rel + "::" + fd.Name.Name
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				switch node := n.(type) {
				case *ast.CompositeLit:
					if shellCarryingLiteral(node) {
						out[key] = "builds a shell-carrying " + typeNameOf(node.Type)
					}
				case *ast.CallExpr:
					if name := readingCallName(node); name != "" {
						out[key] = "calls " + name
					}
				}
				return true
			})
		}
	}
	return out
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
// non-empty Shell field set.
//
// The "non-empty" half matters: internal/guard builds Action literals for its
// own FS sub-checks that deliberately carry no command, and every tool that
// authorizes by name alone builds `guard.Action{Tool: "…"}`. Neither reaches
// the reading, and listing them would bury the sites that do.
func shellCarryingLiteral(lit *ast.CompositeLit) bool {
	if !shellCarryingTypes[typeNameOf(lit.Type)] {
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
