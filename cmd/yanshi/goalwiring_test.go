package main

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/goalloop"
	"github.com/x6nux/yanshi/internal/store"
)

// TestGoalLoopResumeIsWired is the gate that GOV4 cannot be: the goal loop is
// assembled in runGoal, not in internal/bootstrap, so nothing in the archtest
// suite can tell whether the resume machinery is actually connected to it.
//
// It was written because deleting the two lines it checks left every other
// test in this package and in internal/agent/goalloop passing. The feature
// would have gone back to being dead code — persisted state written by nobody,
// resumed by nobody — with no signal at all. This test is the signal.
//
// It reads the source rather than the behaviour because the alternative is
// running the real path, which needs bootstrap.Build, a model provider and an
// external agent CLI. What the resume logic DOES is covered by the goalloop
// tests; the only thing left unguarded, and therefore the only thing checked
// here, is that runGoal hands it the store and the flag set.
func TestGoalLoopResumeIsWired(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	require.NoError(t, err)

	// Scoped to runGoal. resetGoalRun resolves a workdir too, correctly and
	// from its own parameter, and it has its own end-to-end test; a file-wide
	// scan would collect it and make this assertion about two functions.
	var runGoalDecl *ast.FuncDecl
	for _, d := range file.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Name.Name == "runGoal" && fn.Recv == nil {
			runGoalDecl = fn
		}
	}
	require.NotNil(t, runGoalDecl, "runGoal must exist for this gate to mean anything")

	// The fields the real path's goalloop.Config must set, and the expression
	// each must be set to. Matching the value too keeps `State: nil` from
	// passing a check that only looked for the key, and keeps Budget from
	// being quietly replaced by a literal that ignores the flags.
	want := map[string]string{
		"State":          "loopStore",
		"BudgetExplicit": "explicitBudgetFlags(fs)",
		"Budget":         "budget",
	}

	// Collected per literal, not merged. runGoal builds two goalloop.Configs —
	// the fake demo path and the real one — and they share field names, so a
	// single flat map would let one path answer for the other. Only the real
	// path wires State, which is what identifies it here.
	var configs []map[string]string
	// goalloop.Goal carries the working directory, which is the key the resume
	// point is stored under. Feeding it anything but the resolved -workdir
	// makes every project on the machine share one resume row.
	var goalWorkdirs []string

	ast.Inspect(runGoalDecl, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		isConfig := isSelector(lit.Type, "goalloop", "Config")
		isGoal := isSelector(lit.Type, "goalloop", "Goal")
		if !isConfig && !isGoal {
			return true
		}
		fields := map[string]string{}
		for _, elt := range lit.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if key, ok := kv.Key.(*ast.Ident); ok {
				fields[key.Name] = exprString(fset, kv.Value)
			}
		}
		if isConfig {
			configs = append(configs, fields)
		} else if wd, ok := fields["Workdir"]; ok {
			goalWorkdirs = append(goalWorkdirs, wd)
		}
		return true
	})

	var real map[string]string
	realIndex := -1
	for i, c := range configs {
		if _, ok := c["State"]; !ok {
			continue
		}
		require.Nil(t, real, "only one goalloop.Config may wire State; found several")
		real, realIndex = c, i
	}
	require.NotNil(t, real,
		"no goalloop.Config in runGoal sets State — the goal loop neither persists "+
			"nor resumes, and no other test notices")

	// WHICH literal carries State, not just that one does. Identifying the real
	// path by "it is the one with State" is impersonatable: moving State and
	// BudgetExplicit off the real literal and onto the fake one in the same edit
	// leaves State appearing exactly once, so the fake answers for the real and
	// every field below is checked against the wrong literal. Measured — the
	// whole assertion passed under that compound mutation.
	//
	// runGoal builds the fake demo Config in the `if` branch and the real one in
	// the `else`, so the real literal is the second in source order. That is
	// positional and a branch reorder would have to update this line; that edit
	// is visible in review, whereas the impersonation was not.
	require.Len(t, configs, 2,
		"runGoal should build exactly two goalloop.Configs (fake demo path, real path); "+
			"if that changed, the positional check below needs rethinking")
	require.Equal(t, 1, realIndex,
		"State is wired on the FIRST goalloop.Config — that is the fake demo path. "+
			"The demo is supposed to run identically every time; persisting its resume "+
			"point means a demo run can be resumed into, and the real path is left "+
			"without one")

	for field, value := range want {
		require.Equalf(t, value, real[field],
			"runGoal must build its real-path goalloop.Config with %s: %s", field, value)
	}

	require.Equal(t, []string{"wd"}, goalWorkdirs,
		"goalloop.Goal must carry the resolved -workdir; it is the key the resume "+
			"point is stored under")

	// ...and that wd is the -workdir flag, not some other path. Checking only
	// the identifier above was not enough: swapping absWorkdir(*workdir) for
	// absWorkdir(".") left every test green while making every project on the
	// machine share a single resume row. The chain is flag -> absWorkdir -> wd
	// -> Goal.Workdir, and it is only guarded if all three links are.
	var workdirSources []string
	ast.Inspect(runGoalDecl, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) == 0 || len(as.Rhs) != 1 {
			return true
		}
		lhs, ok := as.Lhs[0].(*ast.Ident)
		if !ok || lhs.Name != "wd" {
			return true
		}
		call, ok := as.Rhs[0].(*ast.CallExpr)
		if !ok {
			return true
		}
		fn, ok := call.Fun.(*ast.Ident)
		if !ok || fn.Name != "absWorkdir" || len(call.Args) != 1 {
			return true
		}
		workdirSources = append(workdirSources, exprString(fset, call.Args[0]))
		return true
	})
	require.Equal(t, []string{"*workdir"}, workdirSources,
		"wd must come from absWorkdir(*workdir); anything else silently detaches the "+
			"resume point from the directory the operator named")
}

// isSelector reports whether e is the qualified identifier pkg.name.
func isSelector(e ast.Expr, pkg, name string) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == pkg
}

// exprString renders e back to source text for comparison.
func exprString(fset *token.FileSet, e ast.Expr) string {
	var buf bytes.Buffer
	if err := printer.Fprint(&buf, fset, e); err != nil {
		return ""
	}
	return buf.String()
}

// TestAbsWorkdirIsCleaned pins the normalisation the goal loop's resume point
// depends on. The workdir is the key that state is stored under, so
// `-workdir /repo` and `-workdir /repo/` must not be two different runs — the
// failure mode is a silently discarded resume point and a budget back at full,
// with nothing printed to explain it.
func TestAbsWorkdirIsCleaned(t *testing.T) {
	wd, err := os.Getwd()
	require.NoError(t, err)

	cases := []struct{ in, want string }{
		{"/repo/thing/", "/repo/thing"},
		{"/repo/thing", "/repo/thing"},
		{"/repo//thing", "/repo/thing"},
		{"/repo/thing/./", "/repo/thing"},
		{"/repo/other/../thing", "/repo/thing"},
	}
	for i := range cases {
		// The slash-rooted inputs are normalised by filepath.Clean, whose
		// Windows spelling is the current drive's root ("\repo\thing").
		// Deriving `want` from the same Clean keeps the table asserting the
		// CONTRACT (duplicates, dot segments and trailing separators collapse)
		// instead of one platform's spelling of it.
		cases[i].want = filepath.Clean(cases[i].want)
	}
	for _, tc := range cases {
		got, err := absWorkdir(tc.in)
		require.NoError(t, err)
		assert.Equal(t, tc.want, got, "absWorkdir(%q)", tc.in)
	}

	// The relative branch has to normalise too, and it is the one that used to
	// concatenate raw.
	rel, err := absWorkdir("sub/dir/")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(wd, "sub", "dir"), rel)
}

// TestResetGoalRun covers the operator's escape hatch end to end: with no
// -reset, digging the stale row out of SQLite by hand is the only way to
// abandon a resumed budget.
func TestResetGoalRun(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "reset.db")
	configPath := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(configPath,
		[]byte("storage:\n  sqlite_path: "+dbPath+"\n"), 0o600))

	// absWorkdir normalises the workdir (on Windows a slash-rooted literal
	// becomes a current-drive path), and every KV key and output line below
	// uses that normalised spelling. Asserting the POSIX literal was a
	// guaranteed mismatch on Windows — derive the expectation from the same
	// normalisation the implementation applies.
	workdir := absWorkdirSafe("/repo/resetme")
	st, err := store.Open(dbPath)
	require.NoError(t, err)
	// A resume point the operator wants gone. The key format is goalloop's, so
	// this writes through the same door the loop does.
	require.NoError(t, goalloop.ResetGoalState(st, workdir))
	live := `{"objective":"x","budget":{"MaxIterations":6,"MaxTokens":250},"iterations":2,"complete":false}`
	require.NoError(t, st.KVSet("goalstate:"+workdir, live))
	require.NoError(t, st.Close())

	var out bytes.Buffer
	require.Equal(t, exitOK, resetGoalRun(configPath, workdir, &out))
	assert.Contains(t, out.String(), workdir)

	st2, err := store.Open(dbPath)
	require.NoError(t, err)
	defer st2.Close()
	blob, ok, err := st2.KVGet("goalstate:" + workdir)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Contains(t, blob, `"complete":true`,
		"a cleared row must be one the loop refuses to resume from")
}

// TestResetGoalRunReportsAnUnreadableConfig keeps the failure path from
// exiting 0 on a config it never managed to read.
func TestResetGoalRunReportsAnUnreadableConfig(t *testing.T) {
	var out bytes.Buffer
	got := resetGoalRun(filepath.Join(t.TempDir(), "nope", "config.yaml"), ".", &out)
	assert.Equal(t, exitErr, got)
	assert.Empty(t, out.String())
}

// TestRunGoalDispatchesTheExitEarlyFlags drives -reset and -history through
// runGoal's argument parsing rather than calling their handlers directly.
//
// Calling the handler is not the same test. It skips the flag-to-dispatch
// hop, which is its own piece of code and can be removed on its own: neutering
// `if *reset` left the whole feature unreachable with every test still green,
// because every -reset test went straight to resetGoalRun. -history had the
// identical hole for the same reason, and had had it far longer.
//
// The exit code alone is enough to catch it. Both branches return before the
// "goal text is required" check, so a dispatch that stops firing falls through
// to exitUsage instead of exitOK.
func TestRunGoalDispatchesTheExitEarlyFlags(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "dispatch.db")
	cfgPath := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(
		"storage:\n  sqlite_path: \""+strings.ReplaceAll(dbPath, "\\", "/")+"\"\n"), 0o600))

	workdir := t.TempDir()
	resolved, err := absWorkdir(workdir)
	require.NoError(t, err)

	// Seed a resume point for -reset to clear, written through the same key
	// the loop uses.
	st, err := store.Open(dbPath)
	require.NoError(t, err)
	require.NoError(t, st.KVSet("goalstate:"+resolved,
		`{"objective":"x","budget":{"MaxIterations":6,"MaxTokens":250},"iterations":2,"complete":false}`))
	require.NoError(t, st.Close())

	// No -goal on purpose: clearing a resume point must not require retyping
	// the goal being abandoned.
	require.Equal(t, exitOK, runGoal([]string{"-config", cfgPath, "-reset", "-workdir", workdir}))

	st2, err := store.Open(dbPath)
	require.NoError(t, err)
	defer st2.Close()
	blob, ok, err := st2.KVGet("goalstate:" + resolved)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Contains(t, blob, `"complete":true`, "-reset must actually reach resetGoalRun")

	assert.Equal(t, exitOK, runGoal([]string{"-config", cfgPath, "-history", "5"}),
		"-history must reach printGoalHistory instead of falling through to the goal-text check")
}
