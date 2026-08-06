package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// dispatchedSubcommands parses main.go for every case label in the two
// dispatch switches — the top-level one and the managed dispatcher.
//
// It reads the source rather than a hand-kept list for the same reason
// gendocs's yanshiSubcommands has a test behind it: a list is maintained by
// whoever already forgot to update the other place.
func dispatchedSubcommands(t *testing.T) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	// The two functions that route a subcommand name, named explicitly.
	//
	// Matching on the switch TAG instead (`sub`, `cmd`) is what the first
	// version did, and it picked up runAuthSub's inner switch over auth's own
	// verbs — reporting set/status/logout/device as missing top-level commands
	// — while missing the top-level `switch argv[1]` entirely, which made
	// every real subcommand look like a phantom.
	routers := map[string]bool{"dispatch": true, "runCLIWithAuthDeps": true}

	out := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || !routers[fn.Name.Name] {
			return true
		}
		ast.Inspect(fn, collectCases(out))
		return false
	})
	return out
}

// collectCases records the string case labels of every switch in a subtree.
func collectCases(out map[string]bool) func(ast.Node) bool {
	return func(n ast.Node) bool {
		sw, ok := n.(*ast.SwitchStmt)
		if !ok {
			return true
		}
		for _, stmt := range sw.Body.List {
			cc, ok := stmt.(*ast.CaseClause)
			if !ok {
				continue
			}
			for _, expr := range cc.List {
				lit, ok := expr.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				if s, err := strconv.Unquote(lit.Value); err == nil && s != "" {
					out[s] = true
				}
			}
		}
		return true
	}
}

// usageSubcommandRe matches an entry in the usage text's Subcommands block:
// two spaces, a name, then whitespace.
var usageSubcommandRe = regexp.MustCompile(`(?m)^  ([a-z][a-z-]*)\s{2,}\S`)

// usageSubcommands extracts the names listed in usage's Subcommands section.
func usageSubcommands() map[string]bool {
	_, block, found := strings.Cut(usage, "Subcommands:")
	if !found {
		return nil
	}
	out := map[string]bool{}
	for _, m := range usageSubcommandRe.FindAllStringSubmatch(block, -1) {
		out[m[1]] = true
	}
	return out
}

// TestUsageListsEverySubcommandItDispatches closes the gap that hid `pr` and
// `auth` from `yanshi -h`.
//
// cmd/gendocs's yanshiSubcommands is already reconciled against main.go's
// switch by gendocs_test.go, which is why docs/user-guide/entrypoints.md HAS
// -h snapshots for both — but nothing reconciled the usage TEXT with dispatch,
// so the top-level help a user actually reads listed 8 of the 10 things the
// binary answers to. An audit of this repo caught `auth` and missed `pr`; a
// list maintained by hand is how that happens twice.
//
// Both directions: a name in usage that dispatches nowhere is a documented
// command that does not exist.
//
// ledger: H2/UDOC1#3 与实际不漂移
func TestUsageListsEverySubcommandItDispatches(t *testing.T) {
	dispatched := dispatchedSubcommands(t)
	if len(dispatched) < 5 {
		t.Fatalf("only %d dispatch cases found; the AST scan is broken and every "+
			"assertion below is vacuous", len(dispatched))
	}
	listed := usageSubcommands()
	if len(listed) == 0 {
		t.Fatal("usage has no Subcommands block")
	}

	var missing, phantom []string
	for name := range dispatched {
		if !listed[name] {
			missing = append(missing, name)
		}
	}
	for name := range listed {
		// "(none)" is the bare invocation and has no case label.
		if name == "none" || dispatched[name] {
			continue
		}
		phantom = append(phantom, name)
	}
	sort.Strings(missing)
	sort.Strings(phantom)

	if len(missing) > 0 {
		t.Errorf("`yanshi -h` does not list %d dispatched subcommand(s): %s\n"+
			"  the binary answers to them; the help a user reads does not say so",
			len(missing), strings.Join(missing, ", "))
	}
	if len(phantom) > 0 {
		t.Errorf("`yanshi -h` lists %d subcommand(s) nothing dispatches: %s",
			len(phantom), strings.Join(phantom, ", "))
	}
}
