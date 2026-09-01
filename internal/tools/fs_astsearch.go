package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/x6nux/yanshi/internal/sandbox"
	"github.com/x6nux/yanshi/internal/secproc"
)

// ast_search: structural (AST) code search, backed by the external ast-grep
// CLI.
//
// It is a SEPARATE tool from fs_search rather than an output_mode or an engine
// parameter on it, and that separation is the point. The two take mutually
// unintelligible query languages: `func $NAME($$$ARGS) { $$$ }` is a valid
// ast-grep pattern and a Go regexp that matches almost nothing, while
// `if err != nil \{\s*\}` is a valid regexp and an ast-grep parse error. A
// model that sees one tool with an engine switch will sooner or later pair the
// engine of one with the syntax of the other, and the failure mode is a
// confident empty result rather than an error — which is the single worst
// answer a search tool can give, because it reads as "there are none".
//
// Why an external binary rather than go/ast: the queries this closes the gap
// for are cross-language ("every place any language swallows an error"), and
// go/ast answers only for Go. ast-grep carries tree-sitter grammars for
// ~20 languages behind one pattern syntax.
//
// Absence of the binary is NOT an error. It is a documented, model-facing
// result naming the alternative (fs_search) and the fix (install ast-grep),
// because the model can act on both and can act on neither if the turn aborts.

const (
	// astSearchTimeout bounds one ast-grep invocation.
	astSearchTimeout = 30 * time.Second
	// astSearchDefaultMax is the match cap when the model does not ask for one.
	astSearchDefaultMax = 100
	// astSearchMaxCap is the ceiling on the model's own max_matches, so a
	// pathological pattern cannot request an unbounded dump.
	astSearchMaxCap = 1000
	// astSearchSnippetChars truncates one match's source text. A structural
	// match can be an entire function body; the model needs the location and
	// enough text to recognize it, and can fs_read the rest.
	astSearchSnippetChars = 400
)

// astGrepBinaries lists the names the ast-grep CLI ships under, in probe
// order. Both are checked because which one lands on PATH depends on the
// installer: the cargo and npm packages install `ast-grep` and alias `sg`,
// while some distro packages ship only one. Requiring the user to know which
// is a support question with no upside.
//
// `sg` is probed SECOND on purpose: on many Linux installs `sg` is also
// util-linux's set-group-ID command, which is not ast-grep and would fail
// confusingly. Preferring the unambiguous long name means the collision is
// only reachable on a machine that has util-linux's sg and no ast-grep, where
// the invocation fails and the error is reported verbatim.
var astGrepBinaries = []string{"ast-grep", "sg"}

// astLookPath is the PATH resolver used to find ast-grep. A package variable
// so tests can simulate a machine with and without the binary without
// mutating the process PATH, which is shared with every parallel test and any
// subprocess they spawn.
var astLookPath = exec.LookPath

// astGrepBinary returns the resolved path of the ast-grep CLI and whether one
// was found.
func astGrepBinary() (string, bool) {
	for _, name := range astGrepBinaries {
		if p, err := astLookPath(name); err == nil {
			return p, true
		}
	}
	return "", false
}

// AstGrepMissingMessage is the model-facing explanation returned when no
// ast-grep binary is on PATH.
//
// Exported so the skill/doc layer and the tool return the same words, and so a
// test can assert the guidance without matching on prose it also authored.
// It names the fallback FIRST: the model's next action matters more than the
// operator's, and "use fs_search" is something it can do this turn.
const AstGrepMissingMessage = "ast-grep is not installed on this machine, so structural search is unavailable. " +
	"Use fs_search with a regexp instead (it can approximate many structural queries), " +
	"or ask the user to install ast-grep (https://ast-grep.github.io) and retry."

// astSearchArgs is the ast_search tool's argument shape.
type astSearchArgs struct {
	Pattern    string `json:"pattern"`
	Language   string `json:"language"`
	Path       string `json:"path"`
	MaxMatches int    `json:"max_matches"`
}

// astGrepMatch is one entry of ast-grep's `--json=compact` output. Only the
// fields consumed are declared; ast-grep emits considerably more (metaVariables,
// replacement, ruleId) and json.Unmarshal ignores the rest.
type astGrepMatch struct {
	File  string `json:"file"`
	Lines string `json:"lines"`
	Text  string `json:"text"`
	Range struct {
		Start struct {
			Line   int `json:"line"`
			Column int `json:"column"`
		} `json:"start"`
		End struct {
			Line   int `json:"line"`
			Column int `json:"column"`
		} `json:"end"`
	} `json:"range"`
}

// astSearchHit is one match as returned to the model. Line and column are
// 1-based (ast-grep emits 0-based) so they line up with fs_read's offset
// parameter and with what an editor shows.
type astSearchHit struct {
	Path    string `json:"path"`
	Line    int    `json:"line"`
	Column  int    `json:"column"`
	EndLine int    `json:"end_line"`
	Snippet string `json:"snippet"`
}

// astSearchResult is the tool's JSON payload.
type astSearchResult struct {
	Matches   []astSearchHit `json:"matches"`
	Truncated bool           `json:"truncated"`
	// Total is the number of matches ast-grep found, which may exceed
	// len(Matches). Reported so a truncated result says how much was left
	// behind: "100 shown" and "100 of 4000" call for different next actions,
	// and without the total the model cannot tell which it got.
	Total int `json:"total"`
}

// NewAstSearchTool builds the ast_search tool on an FSTools instance, which
// supplies the work root and the path jail.
//
// It hangs off FSTools rather than standing alone because it must resolve and
// authorize its `path` argument through exactly the same abs() + checkFS()
// choke point every other fs tool uses. A second, parallel implementation of
// the jail is how the jail acquires a hole.
func (f *FSTools) NewAstSearchTool() *GuardedTool {
	return NewGuardedTool(
		"ast_search", "AST search",
		"Search code by AST STRUCTURE using an ast-grep pattern (NOT a regexp). "+
			"Use $NAME to capture one node and $$$NAME to capture a sequence: "+
			"`if err != nil { $$$BODY }`, `func $NAME($$$ARGS) $RET { $$$ }`. "+
			"Answers queries a regexp cannot, e.g. finding every branch that swallows an error. "+
			"Requires the external ast-grep CLI; when it is absent the tool says so and fs_search is the fallback.",
		astSearchTimeout,
		params(map[string]*schema.ParameterInfo{
			"pattern": {Type: schema.String, Required: true,
				Desc: "ast-grep pattern (structural, not a regexp), e.g. `if err != nil { }`"},
			"language": {Type: schema.String, Required: true,
				Desc: "source language: go, python, typescript, tsx, javascript, rust, java, c, cpp, ruby, php, kotlin, swift, ..."},
			"path": {Type: schema.String,
				Desc: "file or directory to search, relative to the work root (default: whole project)"},
			"max_matches": {Type: schema.Integer,
				Desc: "cap on returned matches (default 100, max 1000)"},
		}),
		SyncStream(f.runAstSearch),
	)
}

// runAstSearch resolves and authorizes the search path, launches ast-grep
// through secproc, and renders the matches.
//
// Every failure that the model can do something about — no binary, a bad
// pattern, an unknown language, no matches — comes back as a RESULT rather
// than a Go error. Returning an error would abort the turn (see ADR-0001 on
// UnknownToolsHandler for the same reasoning applied to tool names), which
// costs the model the chance to fix its pattern. Only a jail violation and an
// authorization denial stay errors, because those must not be retried into.
func (f *FSTools) runAstSearch(ctx context.Context, argsJSON string) (string, error) {
	var a astSearchArgs
	if err := ParseArgs(argsJSON, &a); err != nil {
		return "", err
	}
	if strings.TrimSpace(a.Pattern) == "" {
		return errorResult("ast_search: empty pattern"), nil
	}
	if strings.TrimSpace(a.Language) == "" {
		return errorResult("ast_search: missing language (ast-grep needs to know which grammar to parse with; " +
			"pass e.g. language=\"go\")"), nil
	}
	// The language reaches ast-grep as an argv operand, so it gets the same
	// leading-dash / NUL check every other operand slot in this package gets.
	// The pattern does NOT: it is the one argument whose whole purpose is to
	// carry punctuation, and it is passed after `--pattern` where ast-grep can
	// only read it as that flag's value.
	if err := validateArgvOperand("language", a.Language); err != nil {
		return errorResult("ast_search: " + err.Error()), nil
	}

	bin, ok := astGrepBinary()
	if !ok {
		return errorResult("ast_search: " + AstGrepMissingMessage), nil
	}

	root := a.Path
	if root == "" {
		root = "."
	}
	// Same authorization as fs_search: a read of the target path, through the
	// interactive-capable Authorize so a profile that would deny can still be
	// escalated to the user on WS.
	if _, err := f.checkFS(ctx, "read", "ast_search", argsJSON, root); err != nil {
		return "", err
	}
	absRoot, err := f.abs(ctx, root)
	if err != nil {
		return "", err
	}

	maxMatches := clampMaxMatches(a.MaxMatches)
	workRoot := f.rootFor(ctx)

	res, err := secureCommandRunner(ctx, secproc.SecureProcessSpec{
		Tool:    "ast_search",
		Program: bin,
		Dir:     workRoot,
		Args: []string{
			"run",
			"--pattern", a.Pattern,
			"--lang", a.Language,
			"--json=compact",
			absRoot,
		},
		UseSandboxTier: sandbox.ReadOnly,
	}, astSearchTimeout)
	if err != nil {
		return errorResult("ast_search: " + err.Error()), nil
	}
	if res.ExitCode != 0 && !astGrepFoundNothing(res) {
		return errorResult("ast_search: ast-grep exited " +
			fmt.Sprint(res.ExitCode) + ": " + commandFailureTail(res)), nil
	}
	return renderAstMatches(res.Stdout, workRoot, maxMatches)
}

// astGrepExitNoMatch is the exit code ast-grep uses for "the pattern matched
// nothing", following the grep convention (0 found, 1 not found, >1 error).
const astGrepExitNoMatch = 1

// astGrepFoundNothing reports whether a nonzero exit is ast-grep's way of
// saying the search succeeded and found no matches.
//
// This is the difference between an answer and a failure, and getting it wrong
// makes the tool useless for its most common outcome. Treating every nonzero
// exit as an error turned "there are no such branches in this codebase" -- a
// correct, actionable answer, and the one a refactoring-survey query returns
// most of the time -- into `ast_search: ast-grep exited 1: []`, which reads to
// a model as a broken tool. It then either retries the same query or falls
// back to fs_search, i.e. the capability silently degrades to the thing it was
// added to replace, on exactly the queries where it was working correctly.
//
// The exit code alone is NOT enough, and neither is the exit code plus an
// empty stdout. All three signals are required:
//
//   - exit code exactly 1 (a usage error exits 2);
//   - stdout empty or the empty JSON array, since a run that died before
//     searching has no payload and a run that matched has a non-empty one;
//   - stderr empty, because ast-grep reports a pattern it could not parse as a
//     WARNING on stderr while still exiting with a match-like status. Without
//     this clause a malformed pattern would be reported to the model as "no
//     matches found" -- turning a failure into a confident, wrong, negative
//     answer, which is strictly worse than the bug this replaced.
//
// Verified against ast-grep 0.45.1: a zero-match run exits 1 with `[]` on
// stdout and nothing on stderr; an unparseable pattern exits 0 with a stderr
// warning; an unknown --lang exits 2.
func astGrepFoundNothing(res commandResult) bool {
	if res.ExitCode != astGrepExitNoMatch {
		return false
	}
	if strings.TrimSpace(res.Stderr) != "" {
		return false
	}
	switch strings.TrimSpace(res.Stdout) {
	case "", "[]":
		return true
	}
	return false
}

// clampMaxMatches normalizes the model's max_matches into the supported
// range: absent or non-positive means the default, and anything above the
// ceiling is clamped to it.
//
// A separate function because the ceiling matters most at values where the
// tool's own output crosses the spillover threshold, and a test that drives it
// through the tool at those sizes asserts on a head+tail preview rather than
// on the truncation it meant to check.
func clampMaxMatches(requested int) int {
	if requested <= 0 {
		return astSearchDefaultMax
	}
	if requested > astSearchMaxCap {
		return astSearchMaxCap
	}
	return requested
}

// renderAstMatches parses ast-grep's compact JSON and renders at most
// maxMatches hits, relative to workRoot.
//
// Split out from runAstSearch so the parsing and truncation rules are testable
// against fixture bytes without a binary on PATH — which is the only way they
// can be tested on a CI runner that does not have ast-grep, i.e. all of them.
func renderAstMatches(stdout, workRoot string, maxMatches int) (string, error) {
	trimmed := strings.TrimSpace(stdout)
	if trimmed == "" {
		return toJSON(astSearchResult{Matches: []astSearchHit{}}), nil
	}
	var raw []astGrepMatch
	if err := json.Unmarshal([]byte(trimmed), &raw); err != nil {
		return errorResult("ast_search: could not parse ast-grep output: " + err.Error()), nil
	}
	out := astSearchResult{
		Matches: make([]astSearchHit, 0, min(len(raw), maxMatches)),
		Total:   len(raw),
	}
	for _, m := range raw {
		if len(out.Matches) >= maxMatches {
			out.Truncated = true
			break
		}
		snippet := strings.TrimRight(m.Lines, "\r\n")
		if snippet == "" {
			snippet = strings.TrimRight(m.Text, "\r\n")
		}
		if len(snippet) > astSearchSnippetChars {
			snippet = snippet[:astSearchSnippetChars] + "…"
		}
		out.Matches = append(out.Matches, astSearchHit{
			Path:    relForDisplay(workRoot, m.File),
			Line:    m.Range.Start.Line + 1,
			Column:  m.Range.Start.Column + 1,
			EndLine: m.Range.End.Line + 1,
			Snippet: snippet,
		})
	}
	return toJSON(out), nil
}

// relForDisplay renders an ast-grep file path relative to the work root, with
// forward slashes, falling back to the original when it cannot be made
// relative.
//
// Paths go back to the model in the same shape fs_read accepts, so a match can
// be opened by pasting the path straight through. An absolute path would work
// too (abs() accepts root-anchored absolutes) but leaks the machine's
// directory layout into the transcript on every single hit.
func relForDisplay(workRoot, p string) string {
	if p == "" {
		return ""
	}
	base := workRoot
	if base == "" {
		base = "."
	}
	absBase, err := filepath.Abs(base)
	if err != nil {
		return filepath.ToSlash(p)
	}
	absP := p
	// A rooted-but-not-absolute path ("\etc" on windows) is anchored at the
	// volume root, and the OS resolves it against the CURRENT drive — not
	// against workRoot. filepath.IsAbs answers false for it, so Join-ing it
	// under absBase displayed an in-root path that never resolves where the
	// real one does (2026-09-01 CI: "/etc" shown as "etc"). Treat it as its
	// own absolute form; Rel then fails on the volume mismatch and the
	// outside-root fallback below returns it unchanged.
	if !filepath.IsAbs(absP) && !hasRootPrefix(absP) {
		absP = filepath.Join(absBase, p)
	}
	rel, err := filepath.Rel(absBase, filepath.Clean(absP))
	if err != nil || strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(p)
	}
	return filepath.ToSlash(rel)
}

// hasRootPrefix reports whether p is anchored at a filesystem root: POSIX
// "/x", or the volume-less rooted Windows form "\x". On POSIX a leading
// backslash is just an ordinary (legal) filename character, but treating it
// as rooted changes nothing there — Rel still fails or yields a ".." prefix,
// and the fallback returns p unchanged.
func hasRootPrefix(p string) bool {
	return strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\`)
}
