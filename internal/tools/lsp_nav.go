package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/lsp"
)

// LSPNavigator is the code-navigation half of a language server, kept separate
// from LSPManager on purpose.
//
// LSPManager is the diagnostics contract the edit tools consume, and several
// tests implement it with small fakes. Widening it would have made every one of
// those fakes stop compiling for the sake of methods they never call, and would
// have forced any future diagnostics-only implementation to carry five stubs.
// The two contracts are joined at the value, not at the type: the orchestrator
// binds a *lsp.Manager, which satisfies both, and lspNavFromContext recovers
// this one with a type assertion. A manager that only does diagnostics still
// binds fine — the nav tools then report that navigation is unavailable, which
// is the truth.
type LSPNavigator interface {
	// Enabled reports whether any language server is available.
	Enabled() bool
	// Definition returns the definition sites of the symbol at path:line:col
	// (both 1-based).
	Definition(ctx context.Context, path string, line, col int) ([]lsp.Location, error)
	// References returns every reference to the symbol at path:line:col.
	References(ctx context.Context, path string, line, col int, includeDeclaration bool) ([]lsp.Location, error)
	// Hover returns the signature and documentation at path:line:col, or nil.
	Hover(ctx context.Context, path string, line, col int) (*lsp.Hover, error)
	// DocumentSymbols lists the symbols declared in path.
	DocumentSymbols(ctx context.Context, path string) ([]lsp.SymbolInfo, error)
	// ResolveSymbol turns a symbol NAME into candidate positions. It is what
	// makes the positional methods above reachable from a model, which holds a
	// name and not a line number.
	ResolveSymbol(ctx context.Context, name string) ([]lsp.SymbolInfo, error)
}

// LSPNavTimeout bounds one navigation tool call end to end, including the
// symbol-name resolution round trip that precedes a positional request.
//
// Two LSP round trips fit inside it with room for a cold server to index the
// workspace on the first one. It is the tool-level ceiling; lsp.DefaultNavTimeout
// bounds each individual request underneath.
const LSPNavTimeout = 45 * time.Second

// lspRefLimitDefault is how many references one call returns when the model
// does not ask for a number.
//
// References are unbounded in a way the other navigation results are not: a
// definition has one or two sites, but a reference query on a popular symbol
// (an error type, a logger, a context key) legitimately returns thousands. Each
// one costs a path, a line number, and a line of source. Returning them all
// does not merely waste tokens, it evicts the rest of the conversation —
// meaning the tool that was supposed to answer a question about the code
// destroys the context in which the answer was needed.
const lspRefLimitDefault = 50

// lspRefLimitMax caps what the model may request. The limit argument exists so
// a model that knows it wants more can ask; it does not exist to let it opt out
// of the bound.
const lspRefLimitMax = 300

// lspSymbolCandidateLimit caps how many alternative definition sites are listed
// when a name is ambiguous. The list is a hint for a follow-up call with
// explicit coordinates, not a result in itself.
const lspSymbolCandidateLimit = 20

// LSPNavTools bundles the code-navigation tools: lsp_definition, lsp_references,
// lsp_hover, and lsp_symbols.
type LSPNavTools struct {
	// Definition resolves a symbol to where it is declared.
	Definition *GuardedTool
	// References lists every use of a symbol.
	References *GuardedTool
	// Hover returns the type signature and documentation for a symbol.
	Hover *GuardedTool
	// Symbols lists the symbols declared in a file.
	Symbols *GuardedTool
}

// NewLSPNavTools builds the four navigation tools. They take no constructor
// dependency: the language server manager is read from the turn context
// (WithLSP), the same way diagFor reads it, so concurrent turns cannot share
// one through a package-level variable.
func NewLSPNavTools() *LSPNavTools {
	posParams := map[string]*schema.ParameterInfo{
		"symbol":    {Type: schema.String, Desc: "symbol name to look up, e.g. \"Manager\" or \"Close\". Use this when you do not know the line/column. Either symbol or file+line+character is required."},
		"file":      {Type: schema.String, Desc: "path relative to the project root; required when using line/character"},
		"line":      {Type: schema.Integer, Desc: "1-based line number of the symbol occurrence"},
		"character": {Type: schema.Integer, Desc: "1-based column of the symbol occurrence (defaults to 1)"},
	}
	refParams := map[string]*schema.ParameterInfo{}
	for k, v := range posParams {
		refParams[k] = v
	}
	refParams["include_declaration"] = &schema.ParameterInfo{
		Type: schema.Boolean, Desc: "include the declaration itself among the references (default false)",
	}
	refParams["limit"] = &schema.ParameterInfo{
		Type: schema.Integer,
		Desc: fmt.Sprintf("max references to return (default %d, max %d)", lspRefLimitDefault, lspRefLimitMax),
	}
	refParams["offset"] = &schema.ParameterInfo{
		Type: schema.Integer, Desc: "skip this many references before returning (for paging past the limit)",
	}

	return &LSPNavTools{
		Definition: NewGuardedTool(
			"lsp_definition", "LSP definition",
			"Find where a symbol is defined, using the language server. Accurate across files and packages, unlike a text search. "+
				"Pass either a symbol name or an exact file+line+character.",
			LSPNavTimeout, params(posParams), SyncStream(runLSPDefinition),
		),
		References: NewGuardedTool(
			"lsp_references", "LSP references",
			"List every reference to a symbol, using the language server. Use this before renaming or changing a signature: "+
				"unlike fs_search it does not match comments or unrelated same-named symbols. Results are truncated; use limit/offset to page.",
			LSPNavTimeout, params(refParams), SyncStream(runLSPReferences),
		),
		Hover: NewGuardedTool(
			"lsp_hover", "LSP hover",
			"Return the type signature and documentation of a symbol, using the language server.",
			LSPNavTimeout, params(posParams), SyncStream(runLSPHover),
		),
		Symbols: NewGuardedTool(
			"lsp_symbols", "LSP symbols",
			"List the symbols declared in a file (an outline: types, functions, methods, with line numbers), "+
				"or search the whole workspace for symbols matching a name.",
			LSPNavTimeout,
			params(map[string]*schema.ParameterInfo{
				"file":  {Type: schema.String, Desc: "path relative to the project root; lists that file's outline"},
				"query": {Type: schema.String, Desc: "symbol name to search for across the workspace. Either file or query is required."},
			}),
			SyncStream(runLSPSymbols),
		),
	}
}

// Tools returns the navigation tools as a slice for registry assembly.
func (l *LSPNavTools) Tools() []*GuardedTool {
	return []*GuardedTool{l.Definition, l.References, l.Hover, l.Symbols}
}

// --- arg types ---

type lspPosArgs struct {
	Symbol    string `json:"symbol"`
	File      string `json:"file"`
	Line      int    `json:"line"`
	Character int    `json:"character"`
}

type lspRefArgs struct {
	lspPosArgs
	IncludeDeclaration bool `json:"include_declaration"`
	Limit              int  `json:"limit"`
	Offset             int  `json:"offset"`
}

type lspSymbolsArgs struct {
	File  string `json:"file"`
	Query string `json:"query"`
}

// --- result types ---

type lspLocationOut struct {
	File   string `json:"file"`
	Line   int    `json:"line,omitempty"`
	Column int    `json:"column,omitempty"`
	// Text is the source line at Line, trimmed of trailing whitespace. Empty
	// when the file is unreadable or the read was denied by the profile.
	Text string `json:"text,omitempty"`
	// URI is carried only for locations outside the project (a dependency in
	// the module cache, a jdt: URI), where File would be misleading.
	URI string `json:"uri,omitempty"`
}

type lspSymbolOut struct {
	Name      string `json:"name"`
	Kind      string `json:"kind,omitempty"`
	Container string `json:"container,omitempty"`
	File      string `json:"file"`
	Line      int    `json:"line,omitempty"`
	Column    int    `json:"column,omitempty"`
}

type lspDefinitionResult struct {
	Query       string           `json:"query"`
	Definitions []lspLocationOut `json:"definitions"`
	// Candidates lists the other symbols the name matched, when it matched
	// more than one. The model can re-ask with explicit coordinates.
	Candidates []lspSymbolOut `json:"candidates,omitempty"`
	Note       string         `json:"note,omitempty"`
}

type lspReferencesResult struct {
	Query      string           `json:"query"`
	References []lspLocationOut `json:"references"`
	Total      int              `json:"total"`
	Returned   int              `json:"returned"`
	Offset     int              `json:"offset,omitempty"`
	Truncated  bool             `json:"truncated"`
	Candidates []lspSymbolOut   `json:"candidates,omitempty"`
	Note       string           `json:"note,omitempty"`
}

type lspHoverResult struct {
	Query      string         `json:"query"`
	File       string         `json:"file,omitempty"`
	Line       int            `json:"line,omitempty"`
	Column     int            `json:"column,omitempty"`
	Contents   string         `json:"contents,omitempty"`
	Candidates []lspSymbolOut `json:"candidates,omitempty"`
	Note       string         `json:"note,omitempty"`
}

type lspSymbolsResult struct {
	Query   string         `json:"query"`
	Symbols []lspSymbolOut `json:"symbols"`
	Total   int            `json:"total"`
	Note    string         `json:"note,omitempty"`
}

// lspUnavailable is the result shape returned when no language server is bound
// or available.
type lspUnavailable struct {
	Available bool   `json:"lsp_available"`
	Reason    string `json:"reason"`
	Advice    string `json:"advice"`
}

// lspNavFromContext recovers the navigator bound for this turn.
//
// It returns a rendered tool RESULT rather than a Go error when navigation is
// unavailable, because a Go error out of a tool aborts the ReAct turn (see
// ADR-0001 and UnknownToolsHandler). "No language server installed" is not a
// failure of the turn, it is an answer: the model should fall back to fs_search
// and keep going. The advice string says so explicitly, because a model told
// only "unavailable" retries the same call.
func lspNavFromContext(ctx context.Context) (LSPNavigator, string) {
	mgr, ok := LSPFromContext(ctx)
	if !ok || mgr == nil {
		return nil, toJSON(lspUnavailable{
			Reason: "no language server is configured for this session",
			Advice: "use fs_search / fs_glob to locate the symbol textually",
		})
	}
	nav, ok := mgr.(LSPNavigator)
	if !ok {
		return nil, toJSON(lspUnavailable{
			Reason: "the bound language-server manager supports diagnostics only",
			Advice: "use fs_search / fs_glob to locate the symbol textually",
		})
	}
	if !nav.Enabled() {
		return nil, toJSON(lspUnavailable{
			Reason: "no language server is installed for this workspace, or the workspace has no marker file (go.mod, package.json, Cargo.toml, ...)",
			Advice: "use fs_search / fs_glob to locate the symbol textually",
		})
	}
	return nav, ""
}

// lspAnchor is a resolved starting point for a positional request, plus the
// other candidates the name matched.
type lspAnchor struct {
	path       string
	line       int
	column     int
	query      string
	candidates []lspSymbolOut
	note       string
}

// resolveAnchor turns the model's arguments into a concrete file position.
//
// Two entry forms are supported and the symbol-name one is the important half:
// definition, references and hover are all positional, while a model reasoning
// about code holds a NAME. Without name resolution the positional tools are
// reachable only by first guessing coordinates — which is the regex hunt they
// exist to replace.
//
// Explicit coordinates win when both are given: the model has more context than
// the resolver, and an exact position is never ambiguous.
func resolveAnchor(ctx context.Context, nav LSPNavigator, toolName, argsJSON string, a lspPosArgs) (*lspAnchor, error) {
	if a.File != "" && a.Line > 0 {
		abs, err := lspResolveInRoot(ctx, a.File)
		if err != nil {
			return nil, err
		}
		// Reading a file's symbols through the language server is an FS read of
		// that file, so it goes through the same authorization fs_read does.
		if err := Authorize(ctx, guard.Action{
			Tool: toolName,
			FS:   guard.FSWant{Op: "read", Paths: []string{abs}},
		}, argsJSON); err != nil {
			return nil, err
		}
		col := a.Character
		if col < 1 {
			col = 1
		}
		return &lspAnchor{
			path: abs, line: a.Line, column: col,
			query: fmt.Sprintf("%s:%d:%d", a.File, a.Line, col),
		}, nil
	}
	if strings.TrimSpace(a.Symbol) == "" {
		return nil, errors.New("provide either symbol, or file plus line")
	}

	syms, err := nav.ResolveSymbol(ctx, a.Symbol)
	if err != nil {
		return nil, err
	}
	syms = lspFilterLocatable(syms)
	if len(syms) == 0 {
		return nil, fmt.Errorf("no symbol named %q found in the workspace", a.Symbol)
	}
	first := syms[0]
	anchor := &lspAnchor{
		path:   first.Location.Path,
		line:   first.Location.Line,
		column: first.Location.Column,
		query:  a.Symbol,
	}
	if err := Authorize(ctx, guard.Action{
		Tool: toolName,
		FS:   guard.FSWant{Op: "read", Paths: []string{anchor.path}},
	}, argsJSON); err != nil {
		return nil, err
	}
	if len(syms) > 1 {
		anchor.candidates = lspSymbolsOut(syms, lspSymbolCandidateLimit)
		anchor.note = fmt.Sprintf(
			"%q matched %d symbols; answered for the first one. Re-run with file/line/character from candidates to pick another.",
			a.Symbol, len(syms))
	}
	return anchor, nil
}

// lspFilterLocatable drops symbols the server returned without a usable
// position. WorkspaceSymbol permits a location with a uri and no range, and a
// position of line 0 would be sent to the server as line -1.
func lspFilterLocatable(syms []lsp.SymbolInfo) []lsp.SymbolInfo {
	out := syms[:0:0]
	for _, s := range syms {
		if s.Location.Path != "" && s.Location.Line > 0 {
			out = append(out, s)
		}
	}
	return out
}

// --- run functions ---

func runLSPDefinition(ctx context.Context, argsJSON string) (string, error) {
	nav, unavailable := lspNavFromContext(ctx)
	if nav == nil {
		return unavailable, nil
	}
	var a lspPosArgs
	if err := ParseArgs(argsJSON, &a); err != nil {
		return "", err
	}
	anchor, err := resolveAnchor(ctx, nav, "lsp_definition", argsJSON, a)
	if err != nil {
		return "", err
	}
	locs, err := nav.Definition(ctx, anchor.path, anchor.line, anchor.column)
	if err != nil {
		return "", err
	}
	// A server that cannot resolve the position still leaves us the
	// declaration site the symbol search found, which is the definition for
	// every symbol that has exactly one. Returning nothing there would be a
	// regression against the search we already did.
	if len(locs) == 0 && a.Symbol != "" {
		locs = []lsp.Location{{Path: anchor.path, Line: anchor.line, Column: anchor.column}}
	}
	res := lspDefinitionResult{
		Query:       anchor.query,
		Definitions: lspLocationsOut(ctx, "lsp_definition", locs, len(locs)),
		Candidates:  anchor.candidates,
		Note:        anchor.note,
	}
	if res.Definitions == nil {
		res.Definitions = []lspLocationOut{}
		res.Note = strings.TrimSpace(res.Note + " no definition found for that position")
	}
	return toJSON(res), nil
}

func runLSPReferences(ctx context.Context, argsJSON string) (string, error) {
	nav, unavailable := lspNavFromContext(ctx)
	if nav == nil {
		return unavailable, nil
	}
	var a lspRefArgs
	if err := ParseArgs(argsJSON, &a); err != nil {
		return "", err
	}
	anchor, err := resolveAnchor(ctx, nav, "lsp_references", argsJSON, a.lspPosArgs)
	if err != nil {
		return "", err
	}
	locs, err := nav.References(ctx, anchor.path, anchor.line, anchor.column, a.IncludeDeclaration)
	if err != nil {
		return "", err
	}

	limit := a.Limit
	if limit <= 0 {
		limit = lspRefLimitDefault
	}
	if limit > lspRefLimitMax {
		limit = lspRefLimitMax
	}
	offset := a.Offset
	if offset < 0 {
		offset = 0
	}
	total := len(locs)
	window := locs
	if offset >= total {
		window = nil
	} else {
		window = locs[offset:]
	}
	truncated := len(window) > limit
	if truncated {
		window = window[:limit]
	}

	res := lspReferencesResult{
		Query:      anchor.query,
		References: lspLocationsOut(ctx, "lsp_references", window, limit),
		Total:      total,
		Returned:   len(window),
		Offset:     offset,
		Truncated:  truncated,
		Candidates: anchor.candidates,
		Note:       anchor.note,
	}
	if res.References == nil {
		res.References = []lspLocationOut{}
	}
	if truncated {
		res.Note = strings.TrimSpace(res.Note + fmt.Sprintf(
			" showing %d of %d references; re-run with offset=%d for the next page.",
			len(window), total, offset+len(window)))
	}
	return toJSON(res), nil
}

func runLSPHover(ctx context.Context, argsJSON string) (string, error) {
	nav, unavailable := lspNavFromContext(ctx)
	if nav == nil {
		return unavailable, nil
	}
	var a lspPosArgs
	if err := ParseArgs(argsJSON, &a); err != nil {
		return "", err
	}
	anchor, err := resolveAnchor(ctx, nav, "lsp_hover", argsJSON, a)
	if err != nil {
		return "", err
	}
	h, err := nav.Hover(ctx, anchor.path, anchor.line, anchor.column)
	if err != nil {
		return "", err
	}
	res := lspHoverResult{
		Query:      anchor.query,
		File:       lspDisplayPath(ctx, anchor.path),
		Line:       anchor.line,
		Column:     anchor.column,
		Candidates: anchor.candidates,
		Note:       anchor.note,
	}
	if h != nil {
		res.Contents = h.Contents
	}
	if res.Contents == "" {
		res.Note = strings.TrimSpace(res.Note + " the language server has no documentation for that position")
	}
	return toJSON(res), nil
}

func runLSPSymbols(ctx context.Context, argsJSON string) (string, error) {
	nav, unavailable := lspNavFromContext(ctx)
	if nav == nil {
		return unavailable, nil
	}
	var a lspSymbolsArgs
	if err := ParseArgs(argsJSON, &a); err != nil {
		return "", err
	}
	if a.File == "" && strings.TrimSpace(a.Query) == "" {
		return "", errors.New("provide either file (for an outline) or query (for a workspace search)")
	}

	var (
		syms []lsp.SymbolInfo
		err  error
		q    string
	)
	if a.File != "" {
		abs, aerr := lspResolveInRoot(ctx, a.File)
		if aerr != nil {
			return "", aerr
		}
		if err := Authorize(ctx, guard.Action{
			Tool: "lsp_symbols",
			FS:   guard.FSWant{Op: "read", Paths: []string{abs}},
		}, argsJSON); err != nil {
			return "", err
		}
		syms, err = nav.DocumentSymbols(ctx, abs)
		q = a.File
	} else {
		syms, err = nav.ResolveSymbol(ctx, a.Query)
		q = a.Query
	}
	if err != nil {
		return "", err
	}

	res := lspSymbolsResult{Query: q, Total: len(syms)}
	res.Symbols = lspSymbolsOut(syms, lspRefLimitMax)
	if res.Symbols == nil {
		res.Symbols = []lspSymbolOut{}
		res.Note = "no symbols found"
	}
	if len(res.Symbols) < len(syms) {
		res.Note = fmt.Sprintf("showing %d of %d symbols", len(res.Symbols), len(syms))
	}
	return toJSON(res), nil
}

// --- rendering ---

// lspLocationsOut renders locations for the model, attaching the source line at
// each one.
//
// The source line is what makes a reference list readable: "manager.go:118" is
// a coordinate, "manager.go:118  defer m.Close()" is an answer. Reading it back
// costs one file read per distinct file, which is why the contents are cached
// across the whole list rather than re-read per location — a reference list is
// usually many hits in few files.
func lspLocationsOut(ctx context.Context, toolName string, locs []lsp.Location, limit int) []lspLocationOut {
	if len(locs) == 0 {
		return nil
	}
	if limit > 0 && len(locs) > limit {
		locs = locs[:limit]
	}
	cache := map[string][]string{}
	out := make([]lspLocationOut, 0, len(locs))
	for _, l := range locs {
		item := lspLocationOut{Line: l.Line, Column: l.Column}
		if l.Path == "" {
			// Not a file:// location (jdt:, untitled:); the raw URI is all
			// there is, and pretending it is a path would send the model to
			// open something that does not exist.
			item.URI = l.URI
			out = append(out, item)
			continue
		}
		item.File = lspDisplayPath(ctx, l.Path)
		if !lspOutsideRoot(ctx, l.Path) {
			item.Text = lspSourceLine(ctx, toolName, cache, l.Path, l.Line)
		} else {
			// A definition in the module cache or SDK is a legitimate answer,
			// but its contents are outside the jail every fs tool enforces, so
			// the location is reported and the line is not.
			item.URI = l.URI
		}
		out = append(out, item)
	}
	return out
}

// lspSymbolsOut renders symbols for the model, capped at limit.
func lspSymbolsOut(syms []lsp.SymbolInfo, limit int) []lspSymbolOut {
	if len(syms) == 0 {
		return nil
	}
	if limit > 0 && len(syms) > limit {
		syms = syms[:limit]
	}
	out := make([]lspSymbolOut, 0, len(syms))
	for _, s := range syms {
		out = append(out, lspSymbolOut{
			Name:      s.Name,
			Kind:      s.Kind.String(),
			Container: s.Container,
			File:      s.Location.Path,
			Line:      s.Location.Line,
			Column:    s.Location.Column,
		})
	}
	return out
}

// lspSourceLine returns the 1-based line n of path, trimmed. It re-checks the
// profile for each distinct file with the STATIC check rather than the
// interactive one: a reference list can span dozens of files, and one dialog
// per file would make the tool unusable. A denied file loses its snippet and
// keeps its location, which is strictly more information than dropping it.
func lspSourceLine(ctx context.Context, toolName string, cache map[string][]string, path string, n int) string {
	if n < 1 {
		return ""
	}
	lines, ok := cache[path]
	if !ok {
		lines = nil
		if lspStaticReadAllowed(ctx, toolName, path) {
			if body, err := os.ReadFile(path); err == nil {
				lines = strings.Split(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n")
			}
		}
		cache[path] = lines
	}
	if n > len(lines) {
		return ""
	}
	return strings.TrimRight(lines[n-1], " \t")
}

// lspStaticReadAllowed reports whether the acting profile permits reading path,
// consulting the static profile only (no permission callback, no prompt).
func lspStaticReadAllowed(ctx context.Context, toolName, path string) bool {
	prof, ok := ProfileFromContext(ctx)
	if !ok {
		return false
	}
	d := guard.New().Check(prof, guard.Action{
		Tool: toolName,
		FS:   guard.FSWant{Op: "read", Paths: []string{filepath.Clean(path)}},
	})
	return d.IsAllowed()
}

// lspResolveInRoot resolves a model-supplied path against the work root and
// enforces the same jail the fs tools do. It reuses fs.go's helpers rather than
// re-deriving the rules, so a change to the jail cannot apply to fs_read and
// miss the navigation tools.
func lspResolveInRoot(ctx context.Context, p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", errors.New("empty file path")
	}
	root := WorkRootFromContext(ctx)
	if root == "" {
		return "", errors.New("no work root in context")
	}
	cleanRoot := filepath.Clean(root)
	if isAbsolutePath(p) {
		cleaned := filepath.Clean(p)
		if !withinRoot(cleaned, cleanRoot) {
			return "", fmt.Errorf("absolute path %q is outside the project root", p)
		}
		return cleaned, nil
	}
	resolved := filepath.Clean(filepath.Join(cleanRoot, p))
	if !withinRoot(resolved, cleanRoot) {
		return "", errors.New("path must stay within the project root")
	}
	return resolved, nil
}

// lspOutsideRoot reports whether path lives outside the work root. Language
// servers routinely answer with locations in the module cache or the language
// SDK, which are real answers but not readable through the fs jail.
func lspOutsideRoot(ctx context.Context, path string) bool {
	root := WorkRootFromContext(ctx)
	if root == "" {
		return true
	}
	return !withinRoot(filepath.Clean(path), filepath.Clean(root))
}

// lspDisplayPath renders an absolute path relative to the work root when it
// lives inside it, so results read the same way the model wrote its request.
// Paths outside the root stay absolute — a relative-looking "../../.." form
// would be both unusable and misleading.
func lspDisplayPath(ctx context.Context, path string) string {
	root := WorkRootFromContext(ctx)
	if root == "" || path == "" {
		return path
	}
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil || strings.HasPrefix(rel, "..") {
		return path
	}
	return filepath.ToSlash(rel)
}
