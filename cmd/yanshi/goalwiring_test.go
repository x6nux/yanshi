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

	// The fields runGoal must set on the goalloop.Config it builds, and the
	// expression each must be set to. Matching the value too keeps `State: nil`
	// from passing a check that only looked for the key.
	want := map[string]string{
		"State":          "loopStore",
		"BudgetExplicit": "explicitBudgetFlags(fs)",
	}

	got := map[string]string{}
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok || !isSelector(lit.Type, "goalloop", "Config") {
			return true
		}
		for _, elt := range lit.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok {
				continue
			}
			if _, wanted := want[key.Name]; wanted {
				got[key.Name] = exprString(fset, kv.Value)
			}
		}
		return true
	})

	for field, value := range want {
		require.Equalf(t, value, got[field],
			"runGoal must build its goalloop.Config with %s: %s — without it the goal loop "+
				"neither persists nor resumes, and no other test notices", field, value)
	}
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

	workdir := "/repo/resetme"
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
