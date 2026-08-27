package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/secproc"
)

// withAstGrep installs a fake PATH resolver for the duration of the test.
// present=false simulates a machine without ast-grep, which is the state of
// every CI runner and therefore the state that must be tested rather than
// assumed.
func withAstGrep(t *testing.T, present bool) {
	t.Helper()
	orig := astLookPath
	astLookPath = func(name string) (string, error) {
		if present && (name == "ast-grep" || name == "sg") {
			return "/fake/bin/" + name, nil
		}
		return "", errors.New("exec: \"" + name + "\": executable file not found in $PATH")
	}
	t.Cleanup(func() { astLookPath = orig })
}

// fakeAstGrep replaces the subprocess runner with one that returns canned
// output and records the spec it was handed.
//
// A fake rather than a mock: it has no expectations and asserts nothing on its
// own. The recorded spec is returned so the test — not the double — decides
// what about the invocation matters.
func fakeAstGrep(t *testing.T, res commandResult, err error) *secproc.SecureProcessSpec {
	t.Helper()
	var got secproc.SecureProcessSpec
	orig := secureCommandRunner
	secureCommandRunner = func(_ context.Context, spec secproc.SecureProcessSpec, _ time.Duration) (commandResult, error) {
		got = spec
		return res, err
	}
	t.Cleanup(func() { secureCommandRunner = orig })
	return &got
}

// astCtx builds a tool context with a profile that allows ast_search and reads
// anywhere, which is what lets the tests exercise the tool body rather than
// the authorization refusal (that has its own test below).
func astCtx(root string) context.Context {
	return WithWorkRoot(WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"ast_search"}},
		FS:    guard.FSPerm{Read: []string{"**"}},
	}), root)
}

// sampleAstGrepJSON is a two-match ast-grep `--json=compact` payload, in the
// shape the real CLI emits: 0-based line/column, a `lines` field carrying the
// matched source text.
const sampleAstGrepJSON = `[
{"file":"internal/a.go","lines":"if err != nil {\n\t}","text":"if err != nil {}",
 "range":{"start":{"line":9,"column":1},"end":{"line":11,"column":2}}},
{"file":"internal/sub/b.go","lines":"if err != nil {\n\t}","text":"if err != nil {}",
 "range":{"start":{"line":41,"column":3},"end":{"line":43,"column":4}}}
]`

// TestAstSearch_MissingBinaryIsAResultNotAnError is the load-bearing
// behavioural requirement: with no ast-grep installed the tool must hand the
// model an actionable explanation, not abort the turn with a Go error. A Go
// error propagates out of InvokableRun and ends the turn; a result lets the
// model switch to fs_search in the same turn.
func TestAstSearch_MissingBinaryIsAResultNotAnError(t *testing.T) {
	withAstGrep(t, false)
	// If the tool reached the launcher despite having no binary, this fails
	// the test rather than silently succeeding.
	secureCommandRunner = func(context.Context, secproc.SecureProcessSpec, time.Duration) (commandResult, error) {
		t.Fatal("ast_search must not launch a subprocess when no binary was found")
		return commandResult{}, nil
	}
	t.Cleanup(func() { secureCommandRunner = runSecureCapture })

	f := NewFSTools(t.TempDir())
	out, err := f.AstSearch.InvokableRun(astCtx(f.root), `{"pattern":"if $A {}","language":"go"}`)
	require.NoError(t, err, "a missing optional binary must not be a Go error")
	assert.Contains(t, out, "ast-grep is not installed")
	assert.Contains(t, out, "fs_search", "the model needs the fallback named")
	assert.Contains(t, out, "install", "and the operator needs the fix named")
}

// TestAstSearch_ParsesMatches covers the happy path end to end: ast-grep's
// 0-based coordinates become 1-based, and paths come back relative to the work
// root so they can be pasted straight into fs_read.
func TestAstSearch_ParsesMatches(t *testing.T) {
	withAstGrep(t, true)
	spec := fakeAstGrep(t, commandResult{Stdout: sampleAstGrepJSON}, nil)

	f := NewFSTools(t.TempDir())
	out, err := f.AstSearch.InvokableRun(astCtx(f.root),
		`{"pattern":"if err != nil { $$$ }","language":"go"}`)
	require.NoError(t, err)

	var got astSearchResult
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.Len(t, got.Matches, 2)
	assert.Equal(t, 2, got.Total)
	assert.False(t, got.Truncated)

	assert.Equal(t, "internal/a.go", got.Matches[0].Path)
	assert.Equal(t, 10, got.Matches[0].Line, "ast-grep line 9 is line 10 to a human")
	assert.Equal(t, 2, got.Matches[0].Column)
	assert.Equal(t, 12, got.Matches[0].EndLine)
	assert.Contains(t, got.Matches[0].Snippet, "if err != nil")

	assert.Equal(t, "internal/sub/b.go", got.Matches[1].Path)
	assert.Equal(t, 42, got.Matches[1].Line)

	// The invocation itself: pattern and language reach ast-grep as separate
	// flag values (never concatenated into one argv element), the output is
	// requested as compact JSON, and the launch declares read-only.
	assert.Contains(t, spec.Args, "--pattern")
	assert.Contains(t, spec.Args, "if err != nil { $$$ }")
	assert.Contains(t, spec.Args, "--lang")
	assert.Contains(t, spec.Args, "go")
	assert.Contains(t, spec.Args, "--json=compact")
	assert.Equal(t, "ast_search", spec.Tool)
}

// TestAstSearch_GoesThroughSecproc pins the launch path. shell_run's fallback
// to a direct pipe is the known deviation from the secproc rule; a NEW tool
// must not add a second one, and the only way to notice it did would be a test
// that asserts the launcher was used.
func TestAstSearch_GoesThroughSecproc(t *testing.T) {
	withAstGrep(t, true)
	launched := false
	orig := secureCommandRunner
	secureCommandRunner = func(_ context.Context, spec secproc.SecureProcessSpec, _ time.Duration) (commandResult, error) {
		launched = true
		assert.NotEmpty(t, spec.Program, "the spec must name the program secproc will authorize")
		return commandResult{Stdout: "[]"}, nil
	}
	t.Cleanup(func() { secureCommandRunner = orig })

	f := NewFSTools(t.TempDir())
	_, err := f.AstSearch.InvokableRun(astCtx(f.root), `{"pattern":"$X","language":"go"}`)
	require.NoError(t, err)
	assert.True(t, launched, "ast_search must launch through the secproc-backed runner")
}

// TestAstSearch_Truncates proves the result is capped, and that the cap
// reports what it left behind: "100 shown" and "100 of 4000" call for
// different next actions and without Total the model cannot tell them apart.
func TestAstSearch_Truncates(t *testing.T) {
	cases := []struct {
		name       string
		hits       int
		maxMatches int
		wantLen    int
		wantTrunc  bool
	}{
		{"under the default cap", 5, 0, 5, false},
		{"exactly at the default cap", astSearchDefaultMax, 0, astSearchDefaultMax, false},
		{"over the default cap", astSearchDefaultMax + 10, 0, astSearchDefaultMax, true},
		{"explicit smaller cap", 20, 3, 3, true},
		{"negative cap falls back to the default", astSearchDefaultMax + 1, -5, astSearchDefaultMax, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withAstGrep(t, true)
			fakeAstGrep(t, commandResult{Stdout: manyAstMatches(tc.hits)}, nil)

			f := NewFSTools(t.TempDir())
			args := `{"pattern":"$X","language":"go","max_matches":` + itoaT(tc.maxMatches) + `}`
			out, err := f.AstSearch.InvokableRun(astCtx(f.root), args)
			require.NoError(t, err)

			var got astSearchResult
			require.NoError(t, json.Unmarshal([]byte(out), &got))
			assert.Len(t, got.Matches, tc.wantLen)
			assert.Equal(t, tc.wantTrunc, got.Truncated)
			assert.Equal(t, tc.hits, got.Total, "the total must survive truncation")
		})
	}
}

// TestClampMaxMatches covers the ceiling on its own. Driving it through the
// tool would need >1000 matches, at which point the tool's JSON crosses the
// spillover threshold and comes back as a head+tail preview — so the assertion
// would be on the spill layer, not on the clamp.
func TestClampMaxMatches(t *testing.T) {
	cases := []struct{ in, want int }{
		{0, astSearchDefaultMax},
		{-1, astSearchDefaultMax},
		{-99999, astSearchDefaultMax},
		{1, 1},
		{astSearchDefaultMax, astSearchDefaultMax},
		{astSearchMaxCap, astSearchMaxCap},
		{astSearchMaxCap + 1, astSearchMaxCap},
		{99999, astSearchMaxCap},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, clampMaxMatches(tc.in), "clampMaxMatches(%d)", tc.in)
	}
}

// TestAstSearch_TruncatesLongSnippets proves one structural match cannot flood
// the context: an AST match can be an entire function body.
func TestAstSearch_TruncatesLongSnippets(t *testing.T) {
	withAstGrep(t, true)
	long := strings.Repeat("x", astSearchSnippetChars*3)
	payload := `[{"file":"a.go","lines":"` + long + `","range":{"start":{"line":0,"column":0},"end":{"line":0,"column":0}}}]`
	fakeAstGrep(t, commandResult{Stdout: payload}, nil)

	f := NewFSTools(t.TempDir())
	out, err := f.AstSearch.InvokableRun(astCtx(f.root), `{"pattern":"$X","language":"go"}`)
	require.NoError(t, err)
	var got astSearchResult
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.Len(t, got.Matches, 1)
	assert.LessOrEqual(t, len(got.Matches[0].Snippet), astSearchSnippetChars+len("…"))
	assert.True(t, strings.HasSuffix(got.Matches[0].Snippet, "…"))
}

// TestAstSearch_ModelFixableFailuresAreResults enumerates every failure the
// model can correct by changing its own arguments. Each must come back as a
// result so the model gets another attempt inside the same turn.
func TestAstSearch_ModelFixableFailuresAreResults(t *testing.T) {
	cases := []struct {
		name    string
		args    string
		stdout  string
		exit    int
		stderr  string
		wantSub string
	}{
		{name: "empty pattern", args: `{"pattern":"","language":"go"}`, wantSub: "empty pattern"},
		{name: "whitespace pattern", args: `{"pattern":"   ","language":"go"}`, wantSub: "empty pattern"},
		{name: "missing language", args: `{"pattern":"$X"}`, wantSub: "missing language"},
		{
			name: "language that would be read as a flag",
			args: `{"pattern":"$X","language":"--config=/etc/evil"}`, wantSub: "must not start with '-'",
		},
		{
			name:   "ast-grep rejects the pattern",
			args:   `{"pattern":"if err != nil {","language":"go"}`,
			exit:   1,
			stderr: "error: Cannot parse the pattern",
			// The CLI's own words must survive to the model, which is the only
			// thing that tells it WHICH part of the pattern was wrong.
			wantSub: "Cannot parse the pattern",
		},
		{
			name:    "unknown language",
			args:    `{"pattern":"$X","language":"klingon"}`,
			exit:    1,
			stderr:  "error: invalid value 'klingon' for '--lang'",
			wantSub: "invalid value 'klingon'",
		},
		{
			name:    "unparseable output",
			args:    `{"pattern":"$X","language":"go"}`,
			stdout:  "this is not json",
			wantSub: "could not parse ast-grep output",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withAstGrep(t, true)
			fakeAstGrep(t, commandResult{Stdout: tc.stdout, Stderr: tc.stderr, ExitCode: tc.exit}, nil)

			f := NewFSTools(t.TempDir())
			out, err := f.AstSearch.InvokableRun(astCtx(f.root), tc.args)
			require.NoError(t, err, "a model-fixable failure must not abort the turn")
			assert.Contains(t, out, tc.wantSub)
		})
	}
}

// TestAstSearch_ExitOneWithEmptyArrayIsNoMatches pins the grep exit-code
// convention, which this tool got wrong until a real-binary run exposed it.
//
// ast-grep exits 1 for "searched fine, matched nothing" (0 found, 1 not found,
// >1 error). Treating every nonzero exit as a failure turned the single most
// common successful outcome of a structural survey query into
// `ast_search: ast-grep exited 1: []` -- which a model reads as a broken tool,
// so it retries or falls back to fs_search. The capability degraded to the
// thing it was added to replace, precisely when it was working.
//
// This lives beside the fixture-driven tests rather than only in the
// real-binary file because it must run on a CI runner with no ast-grep, which
// is where the regression would otherwise reappear unseen.
func TestAstSearch_ExitOneWithEmptyArrayIsNoMatches(t *testing.T) {
	for _, stdout := range []string{"[]", "", "  \n"} {
		withAstGrep(t, true)
		fakeAstGrep(t, commandResult{Stdout: stdout, ExitCode: 1}, nil)
		f := NewFSTools(t.TempDir())
		out, err := f.AstSearch.InvokableRun(astCtx(f.root), `{"pattern":"$X","language":"go"}`)
		require.NoError(t, err)
		var got astSearchResult
		require.NoError(t, json.Unmarshal([]byte(out), &got),
			"exit 1 with %q on stdout must be the empty RESULT, not an error string: %s", stdout, out)
		assert.Empty(t, got.Matches)
		assert.Equal(t, 0, got.Total)
	}
}

// TestAstSearch_ExitOneWithRealOutputIsStillAFailure is the other half: the
// no-match reprieve is granted on the exit code AND an empty payload AND an
// empty stderr, never on the exit code alone.
//
// Both carriers are checked because ast-grep uses both. A run that died after
// printing to stdout, and a run that parsed the pattern badly and warned on
// stderr, would each otherwise be reported to the model as "no matches found"
// -- converting a failure into a confident, wrong, negative answer, which is
// strictly worse than the bug this replaced.
func TestAstSearch_ExitOneWithRealOutputIsStillAFailure(t *testing.T) {
	cases := []struct {
		name   string
		stdout string
		stderr string
	}{
		{"diagnostic on stdout", "Cannot parse the pattern", ""},
		{"warning on stderr with an empty payload", "[]", "Warning: Pattern contains an ERROR node"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withAstGrep(t, true)
			fakeAstGrep(t, commandResult{Stdout: tc.stdout, Stderr: tc.stderr, ExitCode: 1}, nil)
			f := NewFSTools(t.TempDir())
			out, err := f.AstSearch.InvokableRun(astCtx(f.root), `{"pattern":"$X","language":"go"}`)
			require.NoError(t, err, "a model-fixable failure must not abort the turn")
			assert.Contains(t, out, "exited 1",
				"a nonzero exit carrying real output must stay a reported failure, not "+
					"become a silent 'no matches'")
		})
	}
}

// TestAstSearch_NoMatchesIsAnEmptyList proves "found nothing" is reported as a
// well-formed empty result rather than as an error or as null, so the model
// can distinguish it from a failure.
func TestAstSearch_NoMatchesIsAnEmptyList(t *testing.T) {
	for _, stdout := range []string{"", "   \n", "[]"} {
		withAstGrep(t, true)
		fakeAstGrep(t, commandResult{Stdout: stdout}, nil)
		f := NewFSTools(t.TempDir())
		out, err := f.AstSearch.InvokableRun(astCtx(f.root), `{"pattern":"$X","language":"go"}`)
		require.NoError(t, err)
		var got astSearchResult
		require.NoError(t, json.Unmarshal([]byte(out), &got))
		assert.Empty(t, got.Matches)
		assert.False(t, got.Truncated)
		assert.Contains(t, out, `"matches":[]`, "an empty result must serialize as [] and not null")
	}
}

// TestAstSearch_EnforcesThePathJail proves the path argument goes through the
// SAME abs() jail as every other fs tool, so ast_search cannot become the hole
// in it. These stay Go errors, not results: a jail violation must not be
// retried into.
func TestAstSearch_EnforcesThePathJail(t *testing.T) {
	for _, p := range []string{"../outside", "../../etc", "/etc/passwd"} {
		t.Run(p, func(t *testing.T) {
			withAstGrep(t, true)
			secureCommandRunner = func(context.Context, secproc.SecureProcessSpec, time.Duration) (commandResult, error) {
				t.Fatal("a jailed path must never reach the launcher")
				return commandResult{}, nil
			}
			t.Cleanup(func() { secureCommandRunner = runSecureCapture })

			f := NewFSTools(t.TempDir())
			out, err := f.AstSearch.InvokableRun(astCtx(f.root),
				`{"pattern":"$X","language":"go","path":"`+p+`"}`)
			require.NoError(t, err, "GuardedTool converts the error into a result chunk")
			assert.Contains(t, out, "✗", "the refusal must be visible to the model")
		})
	}
}

// TestAstSearch_DeniedByProfile proves the tool is authorized like every other
// one: a profile that does not allow ast_search refuses it before any
// subprocess is launched.
func TestAstSearch_DeniedByProfile(t *testing.T) {
	withAstGrep(t, true)
	secureCommandRunner = func(context.Context, secproc.SecureProcessSpec, time.Duration) (commandResult, error) {
		t.Fatal("an unauthorized call must never reach the launcher")
		return commandResult{}, nil
	}
	t.Cleanup(func() { secureCommandRunner = runSecureCapture })

	f := NewFSTools(t.TempDir())
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_read"}},
	})
	out, err := f.AstSearch.InvokableRun(ctx, `{"pattern":"$X","language":"go"}`)
	require.NoError(t, err)
	assert.Contains(t, out, "permission denied")
}

// TestAstSearch_IsRegistered proves the tool reaches the registry. A tool that
// is built, tested and never wired is the repo's documented dominant failure
// mode; the assertion is on Tools(), which is what bootstrap iterates.
func TestAstSearch_IsRegistered(t *testing.T) {
	f := NewFSTools(t.TempDir())
	require.NotNil(t, f.AstSearch)
	var names []string
	for _, tl := range f.Tools() {
		info, err := tl.Info(context.Background())
		require.NoError(t, err)
		names = append(names, info.Name)
	}
	assert.Contains(t, names, "ast_search")
	assert.Contains(t, names, "fs_search", "both search tools must be present")
}

// TestAstSearch_IsSeparateFromFsSearch pins the design decision that the two
// searches are distinct tools. Folding AST search into fs_search as a mode
// would let the model pair one engine's syntax with the other's, whose failure
// shape is a confident empty result — the worst answer a search tool can give.
func TestAstSearch_IsSeparateFromFsSearch(t *testing.T) {
	f := NewFSTools(t.TempDir())
	fsInfo, err := f.Search.Info(context.Background())
	require.NoError(t, err)
	astInfo, err := f.AstSearch.Info(context.Background())
	require.NoError(t, err)
	assert.NotEqual(t, fsInfo.Name, astInfo.Name)

	// Neither tool may offer the other's engine as a parameter.
	fsSchema, err := fsInfo.ParamsOneOf.ToJSONSchema()
	require.NoError(t, err)
	fsProps := propertyNames(t, fsSchema)
	assert.NotContains(t, fsProps, "language")
	assert.NotContains(t, fsProps, "engine")

	astSchema, err := astInfo.ParamsOneOf.ToJSONSchema()
	require.NoError(t, err)
	astProps := propertyNames(t, astSchema)
	assert.Contains(t, astProps, "language")
	assert.NotContains(t, astProps, "output_mode")
	assert.NotContains(t, astProps, "engine")

	// The description must warn the model off regexps explicitly, because the
	// failure it prevents is silent.
	assert.Contains(t, astInfo.Desc, "NOT a regexp")
}

// TestAstGrepBinary_PrefersLongName proves the probe order. `sg` collides with
// util-linux's set-group-ID command on many Linux installs, so the
// unambiguous name must win when both resolve.
func TestAstGrepBinary_PrefersLongName(t *testing.T) {
	orig := astLookPath
	astLookPath = func(name string) (string, error) { return "/usr/bin/" + name, nil }
	t.Cleanup(func() { astLookPath = orig })
	p, ok := astGrepBinary()
	require.True(t, ok)
	assert.Equal(t, "/usr/bin/ast-grep", p)
}

// TestAstGrepBinary_FallsBackToSg proves the short name is still accepted when
// it is the only one installed.
func TestAstGrepBinary_FallsBackToSg(t *testing.T) {
	orig := astLookPath
	astLookPath = func(name string) (string, error) {
		if name == "sg" {
			return "/usr/bin/sg", nil
		}
		return "", errors.New("not found")
	}
	t.Cleanup(func() { astLookPath = orig })
	p, ok := astGrepBinary()
	require.True(t, ok)
	assert.Equal(t, "/usr/bin/sg", p)
}

func TestRelForDisplay(t *testing.T) {
	root := t.TempDir()
	abs, err := filepath.Abs(root)
	require.NoError(t, err)
	cases := []struct {
		name string
		root string
		in   string
		want string
	}{
		{"already relative", root, "internal/a.go", "internal/a.go"},
		{"absolute inside root", root, filepath.Join(abs, "internal", "a.go"), "internal/a.go"},
		{"empty stays empty", root, "", ""},
		{"absolute outside root is left alone", root, string(filepath.Separator) + "etc",
			filepath.ToSlash(string(filepath.Separator) + "etc")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, relForDisplay(tc.root, tc.in))
		})
	}
}

// TestRenderAstMatches_HandlesMissingLinesField proves the `text` field is
// used when `lines` is absent — ast-grep omits `lines` for some rule shapes,
// and a snippet-less result is a location the model cannot recognize.
func TestRenderAstMatches_HandlesMissingLinesField(t *testing.T) {
	out, err := renderAstMatches(
		`[{"file":"a.go","text":"foo()","range":{"start":{"line":0,"column":0},"end":{"line":0,"column":5}}}]`,
		".", 10)
	require.NoError(t, err)
	var got astSearchResult
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.Len(t, got.Matches, 1)
	assert.Equal(t, "foo()", got.Matches[0].Snippet)
}

// --- helpers ---

// manyAstMatches builds an ast-grep payload with n matches.
func manyAstMatches(n int) string {
	var b strings.Builder
	b.WriteString("[")
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"file":"f` + itoaT(i) + `.go","lines":"x",` +
			`"range":{"start":{"line":0,"column":0},"end":{"line":0,"column":1}}}`)
	}
	b.WriteString("]")
	return b.String()
}

// itoaT renders an int (including negatives) without importing strconv.
func itoaT(n int) string {
	neg := n < 0
	if neg {
		n = -n
	}
	if n == 0 {
		return "0"
	}
	var buf [24]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// propertyNames extracts the top-level property names of a tool's OpenAPI
// parameter schema.
func propertyNames(t *testing.T, schema any) []string {
	t.Helper()
	b, err := json.Marshal(schema)
	require.NoError(t, err)
	var doc struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	require.NoError(t, json.Unmarshal(b, &doc))
	names := make([]string, 0, len(doc.Properties))
	for k := range doc.Properties {
		names = append(names, k)
	}
	return names
}

var _ = os.Stat
