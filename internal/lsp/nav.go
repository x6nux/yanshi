package lsp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// This file adds the request half of the protocol: definition, references,
// hover, and the two symbol searches. Until it existed the client was
// write-only for everything except diagnostics -- it could tell a server about
// an edit and read what the server volunteered, but could not ask it a
// question. Finding a symbol's callers therefore meant a regex over the tree,
// which cannot tell a call from a comment mentioning it and cannot follow a
// rename.
//
// The two symbol searches are not optional extras. definition/references/hover
// are all POSITIONAL: they need a file, a line, and a column. A model holds a
// NAME. Without workspace/symbol to turn one into the other, the positional
// three are reachable only by guessing coordinates, which is exactly the regex
// hunt they were meant to replace.

// ErrDisabled is returned by the navigation methods when no language server is
// available for the workspace (none installed, or no marker file). It is a
// distinct value, not a generic error, so the tool layer can turn it into a
// model-facing explanation instead of surfacing it as a failure.
var ErrDisabled = errors.New("lsp: no language server available for this workspace")

// ErrNoServer is returned when a language server exists for the workspace but
// not for the requested file's language (e.g. asking about a .md file in a Go
// repo).
var ErrNoServer = errors.New("lsp: no language server for this file type")

// DefaultNavTimeout bounds one navigation request.
//
// It is deliberately far above the 800ms diagnostics default: diagnostics are
// a best-effort garnish on an edit that already happened and an empty result
// costs the model nothing, whereas a navigation request IS the answer, and a
// cold gopls resolving workspace/symbol over a large module can take seconds
// on its first call. Timing out here does not degrade gracefully -- it looks
// to the model exactly like "this symbol does not exist".
const DefaultNavTimeout = 15 * time.Second

// ensureOpen sends textDocument/didOpen for uri if this client has not opened
// it yet, and does nothing otherwise.
//
// Servers answer positional requests against their own copy of the document,
// and gopls rejects requests for files it was never told about. Opening is
// therefore a precondition of every method in this file.
//
// It deliberately does NOT touch editGen. editGen is the diagnostics freshness
// counter: Diagnostics blocks until a publication for the current generation
// arrives, and a file merely READ by a navigation request has no pending
// publication to wait for. Bumping it here would make the next Diagnostics call
// on that file block for its full timeout and then return stale data.
func (c *Client) ensureOpen(uri, text string) error {
	c.diagMu.Lock()
	if c.opened[uri] {
		c.diagMu.Unlock()
		return nil
	}
	c.opened[uri] = true
	c.docVer[uri]++
	ver := c.docVer[uri]
	c.diagMu.Unlock()

	return c.notify("textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{
			"uri":        uri,
			"languageId": languageID(uri),
			"version":    ver,
			"text":       text,
		},
	})
}

// requestNav issues a request bounded by max rather than by the client's
// handshake timeout.
//
// The handshake ceiling (5s on the exec path, the diagnostics timeout on the
// Dial one) is the wrong bound for navigation in both directions: too short for
// a cold workspace/symbol, and irrelevant to a warm hover. request() takes the
// MINIMUM of its own timeout and the context deadline, so a caller cannot raise
// the ceiling by passing a longer deadline -- the only way to widen it is here.
func (c *Client) requestNav(ctx context.Context, max time.Duration, method string, params any) (any, error) {
	if max <= 0 {
		max = DefaultNavTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, max)
	defer cancel()
	// Raise the per-request ceiling for this call only. c.timeout is read
	// (not mutated) by request, so a temporary widening would race other
	// requests; instead the deadline above is the sole bound and we hand
	// request a timeout large enough that the context always wins.
	resp, err := c.requestUnbounded(ctx, method, params)
	if err != nil {
		return nil, err
	}
	return resp["result"], nil
}

// textDocumentPosition builds the params shared by definition/references/hover.
// line and col are 1-based on the way in and 0-based on the wire.
func textDocumentPosition(uri string, line, col int) map[string]any {
	if line < 1 {
		line = 1
	}
	if col < 1 {
		col = 1
	}
	return map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": line - 1, "character": col - 1},
	}
}

// Definition asks for the definition sites of the symbol at (line, col), both
// 1-based. An empty slice with a nil error means the server answered and found
// nothing, which is a different fact from a failed request.
func (c *Client) Definition(ctx context.Context, uri string, line, col int, max time.Duration) ([]Location, error) {
	res, err := c.requestNav(ctx, max, "textDocument/definition", textDocumentPosition(uri, line, col))
	if err != nil {
		return nil, err
	}
	return parseLocations(res), nil
}

// References asks for every reference to the symbol at (line, col), both
// 1-based. includeDeclaration controls whether the declaration itself is one of
// them -- servers differ on the default, so it is always sent explicitly.
func (c *Client) References(ctx context.Context, uri string, line, col int, includeDeclaration bool, max time.Duration) ([]Location, error) {
	params := textDocumentPosition(uri, line, col)
	params["context"] = map[string]any{"includeDeclaration": includeDeclaration}
	res, err := c.requestNav(ctx, max, "textDocument/references", params)
	if err != nil {
		return nil, err
	}
	return parseLocations(res), nil
}

// Hover asks for the type signature and documentation at (line, col), both
// 1-based. A nil Hover with a nil error means the server had nothing to say
// about that position.
func (c *Client) Hover(ctx context.Context, uri string, line, col int, max time.Duration) (*Hover, error) {
	res, err := c.requestNav(ctx, max, "textDocument/hover", textDocumentPosition(uri, line, col))
	if err != nil {
		return nil, err
	}
	return parseHover(res), nil
}

// DocumentSymbols lists every symbol declared in uri, with nested symbols
// flattened and their parent recorded as Container.
func (c *Client) DocumentSymbols(ctx context.Context, uri string, max time.Duration) ([]SymbolInfo, error) {
	res, err := c.requestNav(ctx, max, "textDocument/documentSymbol", map[string]any{
		"textDocument": map[string]any{"uri": uri},
	})
	if err != nil {
		return nil, err
	}
	return parseSymbols(res, uri), nil
}

// WorkspaceSymbols searches the whole workspace for symbols matching query.
// This is the name-to-position bridge the positional requests depend on.
func (c *Client) WorkspaceSymbols(ctx context.Context, query string, max time.Duration) ([]SymbolInfo, error) {
	res, err := c.requestNav(ctx, max, "workspace/symbol", map[string]any{"query": query})
	if err != nil {
		return nil, err
	}
	return parseSymbols(res, ""), nil
}

// --- Manager-level, path-based API ---

// navTimeout returns the configured per-request navigation bound.
func (m *Manager) navTimeout() time.Duration {
	if m.cfg.NavTimeout > 0 {
		return m.cfg.NavTimeout
	}
	return DefaultNavTimeout
}

// clientForFile resolves the language server for path and makes sure the
// server has the file open, reading the current on-disk contents.
//
// A read failure is not fatal: the server indexes the workspace itself and may
// well answer from its own copy. What would be fatal is skipping the didOpen,
// so a file we cannot read is still announced -- with empty text, which the
// server replaces from disk on its next own read.
func (m *Manager) clientForFile(path string) (*Client, string, error) {
	if !m.Enabled() {
		return nil, "", ErrDisabled
	}
	lang := detectLanguage(path)
	if lang == "" {
		return nil, "", ErrNoServer
	}
	c, err := m.clientFor(lang)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrNoServer, err)
	}
	uri := pathToURL(path)
	body, readErr := os.ReadFile(path)
	if readErr != nil {
		body = nil
	}
	if err := c.ensureOpen(uri, string(body)); err != nil {
		return nil, "", err
	}
	m.rememberOpen(path)
	return c, uri, nil
}

// Definition returns the definition sites of the symbol at path:line:col
// (1-based).
func (m *Manager) Definition(ctx context.Context, path string, line, col int) ([]Location, error) {
	c, uri, err := m.clientForFile(path)
	if err != nil {
		return nil, err
	}
	return c.Definition(ctx, uri, line, col, m.navTimeout())
}

// References returns every reference to the symbol at path:line:col (1-based).
func (m *Manager) References(ctx context.Context, path string, line, col int, includeDeclaration bool) ([]Location, error) {
	c, uri, err := m.clientForFile(path)
	if err != nil {
		return nil, err
	}
	return c.References(ctx, uri, line, col, includeDeclaration, m.navTimeout())
}

// Hover returns the type signature and documentation at path:line:col
// (1-based), or nil when the server has nothing for that position.
func (m *Manager) Hover(ctx context.Context, path string, line, col int) (*Hover, error) {
	c, uri, err := m.clientForFile(path)
	if err != nil {
		return nil, err
	}
	return c.Hover(ctx, uri, line, col, m.navTimeout())
}

// DocumentSymbols lists the symbols declared in path.
func (m *Manager) DocumentSymbols(ctx context.Context, path string) ([]SymbolInfo, error) {
	c, uri, err := m.clientForFile(path)
	if err != nil {
		return nil, err
	}
	return c.DocumentSymbols(ctx, uri, m.navTimeout())
}

// WorkspaceSymbols searches every available language server for query and
// merges the results.
//
// Every server is asked, not just one, because a workspace can hold more than
// one language and the caller has only a name -- it cannot know which server
// owns it. The cost is bounded: New already pruned the language table to the
// servers whose marker files are actually present, so a Go module asks gopls
// and nothing else.
//
// Languages are iterated in sorted order so the merged result is stable across
// runs; a result list whose order depends on map iteration would make the
// truncation in the tool layer non-deterministic, silently dropping a different
// symbol each call.
func (m *Manager) WorkspaceSymbols(ctx context.Context, query string) ([]SymbolInfo, error) {
	if !m.Enabled() {
		return nil, ErrDisabled
	}
	if strings.TrimSpace(query) == "" {
		return nil, errors.New("lsp: empty symbol query")
	}
	langs := make([]string, 0, len(m.cfg.Languages))
	for lang := range m.Languages() {
		langs = append(langs, lang)
	}
	sort.Strings(langs)

	var out []SymbolInfo
	var lastErr error
	seen := map[string]bool{}
	for _, lang := range langs {
		c, err := m.clientFor(lang)
		if err != nil {
			lastErr = err
			continue
		}
		syms, err := c.WorkspaceSymbols(ctx, query, m.navTimeout())
		if err != nil {
			lastErr = err
			continue
		}
		for _, s := range syms {
			key := fmt.Sprintf("%s|%s|%d|%d", s.Location.URI, s.Name, s.Location.Line, s.Location.Column)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, s)
		}
	}
	if len(out) == 0 && lastErr != nil {
		return nil, lastErr
	}
	return out, nil
}

// ResolveSymbol turns a symbol NAME into candidate positions, preferring exact
// name matches over the substring matches most servers also return.
//
// gopls answers workspace/symbol with fuzzy matches: querying "Close" returns
// "Closer", "CloseAll", and every method named Close in every package. Handing
// that whole list to the caller as-is makes the common case (one exact match)
// indistinguishable from the ambiguous one. Exact matches, when any exist, are
// therefore returned alone.
func (m *Manager) ResolveSymbol(ctx context.Context, name string) ([]SymbolInfo, error) {
	syms, err := m.WorkspaceSymbols(ctx, name)
	if err != nil {
		return nil, err
	}
	var exact []SymbolInfo
	for _, s := range syms {
		if s.Name == name {
			exact = append(exact, s)
		}
	}
	if len(exact) > 0 {
		return exact, nil
	}
	return syms, nil
}
