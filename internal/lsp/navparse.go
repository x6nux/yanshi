package lsp

import (
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
)

// This file holds nothing but response parsing, because that is where the LSP
// spec's optionality lives: definition answers in three shapes, hover in four,
// and documentSymbol in two, and a server is free to switch between them
// between versions. Keeping the parsers apart from the request methods in
// nav.go means the shape table below can be tested directly against recorded
// payloads, with no client, pipe, or subprocess in the way.

// urlToPath decodes a file:// URI back to a local path. It is the inverse of
// pathToURL and undoes the same two transformations: percent-escapes (via
// net/url) and the leading slash that a Windows drive letter picks up
// ("/C:/x" -> "C:\x").
//
// A non-file scheme returns "". Servers legitimately answer with jdt:, zipfile:
// and untitled: URIs for locations that have no path on this disk, and turning
// those into a relative-looking path would send the model to open a file that
// does not exist. The caller keeps the raw URI in Location.URI either way.
func urlToPath(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "file" {
		return ""
	}
	p := u.Path
	// "/C:/x" -> "C:/x". Applies on any host OS: a Windows-produced URI can be
	// parsed on Linux (recorded fixtures, remote servers).
	if len(p) >= 3 && p[0] == '/' && p[2] == ':' {
		p = p[1:]
	}
	if runtime.GOOS == "windows" {
		p = filepath.FromSlash(p)
	}
	return filepath.Clean(p)
}

// parseRange reads an LSP Range into 1-based line/column quadruple. Absent or
// malformed ranges yield all zeros, which callers render as "unknown" rather
// than as line 0 -- there is no line 0 in any editor, so the two cannot be
// confused.
func parseRange(v any) (line, col, endLine, endCol int) {
	rng, ok := v.(map[string]any)
	if !ok {
		return 0, 0, 0, 0
	}
	if start, ok := rng["start"].(map[string]any); ok {
		line = int(toInt64Or(start["line"], -1)) + 1
		col = int(toInt64Or(start["character"], -1)) + 1
	}
	if end, ok := rng["end"].(map[string]any); ok {
		endLine = int(toInt64Or(end["line"], -1)) + 1
		endCol = int(toInt64Or(end["character"], -1)) + 1
	}
	return line, col, endLine, endCol
}

// locationFrom builds a Location from a uri plus an LSP Range value.
func locationFrom(uri string, rng any) Location {
	l, c, el, ec := parseRange(rng)
	return Location{Path: urlToPath(uri), URI: uri, Line: l, Column: c, EndLine: el, EndColumn: ec}
}

// parseLocations normalises a textDocument/definition (or declaration,
// implementation, typeDefinition) result into a flat []Location.
//
// The spec permits THREE shapes for the same request and gopls has shipped two
// of them: `Location | Location[] | LocationLink[] | null`. A parser that
// handles only the array-of-Location form -- the obvious one to write, because
// it is what every tutorial shows -- returns "no definition found" against a
// server answering with a single object, which is indistinguishable from the
// symbol genuinely having no definition.
//
// LocationLink is the shape that carries targetSelectionRange, the range of
// just the NAME at the definition site as opposed to targetRange, the whole
// declaration body. The narrow one is preferred: a caller quoting the source
// line wants the line the name is on, and for a 40-line function those are the
// same line only by luck.
func parseLocations(result any) []Location {
	switch v := result.(type) {
	case nil:
		return nil
	case map[string]any:
		if loc, ok := parseOneLocation(v); ok {
			return []Location{loc}
		}
		return nil
	case []any:
		out := make([]Location, 0, len(v))
		for _, item := range v {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if loc, ok := parseOneLocation(m); ok {
				out = append(out, loc)
			}
		}
		return out
	}
	return nil
}

// parseOneLocation accepts either a Location ({uri, range}) or a LocationLink
// ({targetUri, targetRange, targetSelectionRange}) object. ok=false means the
// object carried neither a uri nor a targetUri and is therefore not a location
// at all.
func parseOneLocation(m map[string]any) (Location, bool) {
	if uri, ok := m["uri"].(string); ok && uri != "" {
		return locationFrom(uri, m["range"]), true
	}
	if uri, ok := m["targetUri"].(string); ok && uri != "" {
		// Prefer targetSelectionRange (the name) over targetRange (the body).
		rng := m["targetSelectionRange"]
		if rng == nil {
			rng = m["targetRange"]
		}
		return locationFrom(uri, rng), true
	}
	return Location{}, false
}

// parseHover normalises a textDocument/hover result.
//
// `contents` has four legal shapes across LSP versions: a bare string, a
// MarkedString object {language, value}, an array mixing those two, and a
// MarkupContent {kind, value}. MarkupContent is the only one current servers
// send, and it is also the only one whose top-level object has no "value"
// ambiguity -- which is why the older shapes are easy to forget and why a
// server pinned to an older protocol version silently produces empty hovers.
func parseHover(result any) *Hover {
	m, ok := result.(map[string]any)
	if !ok {
		return nil
	}
	text := strings.TrimSpace(markedContent(m["contents"]))
	h := &Hover{Contents: text}
	h.Line, h.Column, h.EndLine, h.EndColumn = parseRange(m["range"])
	if h.Contents == "" && h.Line == 0 {
		return nil
	}
	return h
}

// markedContent renders any of the MarkedString / MarkupContent shapes as
// markdown. A {language, value} pair becomes a fenced block so the model sees
// the signature as code rather than as prose that happens to contain parens.
func markedContent(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case map[string]any:
		value, _ := c["value"].(string)
		if value == "" {
			return ""
		}
		// MarkupContent has "kind" (plaintext|markdown) and needs no fence;
		// MarkedString has "language" and does.
		if lang, ok := c["language"].(string); ok {
			return "```" + lang + "\n" + value + "\n```"
		}
		return value
	case []any:
		parts := make([]string, 0, len(c))
		for _, item := range c {
			if s := markedContent(item); s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, "\n\n")
	}
	return ""
}

// parseSymbols normalises a documentSymbol or workspace/symbol result.
//
// documentSymbol answers with either a flat []SymbolInformation (each entry
// carrying its own location, including a uri) or a NESTED []DocumentSymbol
// tree, whose children carry ranges but no uri at all -- the uri is implied by
// the request. fallbackURI supplies it, so a caller asking about one file gets
// usable paths out of both shapes. Pass "" for workspace/symbol, where every
// entry must carry its own uri.
//
// The tree is flattened with the parent's name recorded as Container, so a
// method comes back as {Name: "Close", Container: "Manager"} rather than as an
// entry the caller has to walk a tree to interpret.
func parseSymbols(result any, fallbackURI string) []SymbolInfo {
	items, ok := result.([]any)
	if !ok {
		return nil
	}
	var out []SymbolInfo
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		out = appendSymbol(out, m, fallbackURI, "")
	}
	return out
}

// appendSymbol converts one symbol object and recurses into its children.
func appendSymbol(out []SymbolInfo, m map[string]any, fallbackURI, container string) []SymbolInfo {
	name, _ := m["name"].(string)
	if name == "" {
		return out
	}
	si := SymbolInfo{
		Name:      name,
		Kind:      SymbolKind(toInt64Or(m["kind"], 0)),
		Container: container,
	}
	if si.Container == "" {
		si.Container, _ = m["containerName"].(string)
	}

	if loc, ok := m["location"].(map[string]any); ok {
		// SymbolInformation / WorkspaceSymbol: location carries the uri. A
		// WorkspaceSymbol may legally omit the range entirely (it is then
		// resolved by workspaceSymbol/resolve), which is why locationFrom
		// tolerates a nil range instead of requiring one.
		uri, _ := loc["uri"].(string)
		si.Location = locationFrom(uri, loc["range"])
	} else {
		// DocumentSymbol: ranges only, uri implied by the request. Prefer
		// selectionRange (the name) over range (the whole body) for the same
		// reason parseOneLocation prefers targetSelectionRange.
		rng := m["selectionRange"]
		if rng == nil {
			rng = m["range"]
		}
		si.Location = locationFrom(fallbackURI, rng)
	}
	out = append(out, si)

	if children, ok := m["children"].([]any); ok {
		for _, child := range children {
			cm, ok := child.(map[string]any)
			if !ok {
				continue
			}
			out = appendSymbol(out, cm, fallbackURI, name)
		}
	}
	return out
}
