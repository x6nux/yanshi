package lsp

import (
	"encoding/json"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// decodeResult parses a recorded JSON-RPC result payload the way readLoop
// would, so the parsers below are fed exactly the Go shapes a real server
// produces (float64 numbers, map[string]any objects) rather than hand-built
// literals that could accidentally use int and hide a conversion bug.
func decodeResult(t *testing.T, payload string) any {
	t.Helper()
	var wrapper map[string]any
	if err := json.Unmarshal([]byte(payload), &wrapper); err != nil {
		t.Fatalf("bad fixture: %v", err)
	}
	return wrapper["result"]
}

// TestParseLocations_AllThreeWireShapes pins the three shapes the LSP spec
// allows for a single definition request.
//
// The spec says `Location | Location[] | LocationLink[] | null`, and servers
// really do differ: the array-of-Location form is what every tutorial shows,
// so a parser handling only that one looks correct against gopls today and
// returns "not found" against a server (or a future gopls) that answers with a
// bare object. That failure is invisible — "no definition" is a legal answer.
func TestParseLocations_AllThreeWireShapes(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    []Location
	}{
		{
			name:    "null result means no definition",
			payload: `{"result":null}`,
			want:    nil,
		},
		{
			name:    "single Location object, not wrapped in an array",
			payload: `{"result":{"uri":"file:///work/a.go","range":{"start":{"line":10,"character":5},"end":{"line":10,"character":9}}}}`,
			want: []Location{{
				Path: filepath.Clean("/work/a.go"), URI: "file:///work/a.go",
				Line: 11, Column: 6, EndLine: 11, EndColumn: 10,
			}},
		},
		{
			name:    "Location array",
			payload: `{"result":[{"uri":"file:///work/a.go","range":{"start":{"line":0,"character":0},"end":{"line":0,"character":3}}},{"uri":"file:///work/b.go","range":{"start":{"line":7,"character":1},"end":{"line":7,"character":4}}}]}`,
			want: []Location{
				{Path: filepath.Clean("/work/a.go"), URI: "file:///work/a.go", Line: 1, Column: 1, EndLine: 1, EndColumn: 4},
				{Path: filepath.Clean("/work/b.go"), URI: "file:///work/b.go", Line: 8, Column: 2, EndLine: 8, EndColumn: 5},
			},
		},
		{
			name: "LocationLink array prefers targetSelectionRange over targetRange",
			// targetRange spans the whole 12-line function; targetSelectionRange
			// is just the name on line 4 (0-based 3). Quoting targetRange's start
			// would point the model at the doc comment above the declaration.
			payload: `{"result":[{"targetUri":"file:///work/c.go","targetRange":{"start":{"line":3,"character":0},"end":{"line":15,"character":1}},"targetSelectionRange":{"start":{"line":3,"character":5},"end":{"line":3,"character":12}}}]}`,
			want: []Location{{
				Path: filepath.Clean("/work/c.go"), URI: "file:///work/c.go",
				Line: 4, Column: 6, EndLine: 4, EndColumn: 13,
			}},
		},
		{
			name:    "LocationLink without targetSelectionRange falls back to targetRange",
			payload: `{"result":[{"targetUri":"file:///work/d.go","targetRange":{"start":{"line":2,"character":0},"end":{"line":9,"character":1}}}]}`,
			want: []Location{{
				Path: filepath.Clean("/work/d.go"), URI: "file:///work/d.go",
				Line: 3, Column: 1, EndLine: 10, EndColumn: 2,
			}},
		},
		{
			name:    "objects that are neither Location nor LocationLink are dropped",
			payload: `{"result":[{"nonsense":1},{"uri":"file:///work/e.go","range":{"start":{"line":0,"character":0},"end":{"line":0,"character":1}}}]}`,
			want: []Location{{
				Path: filepath.Clean("/work/e.go"), URI: "file:///work/e.go",
				Line: 1, Column: 1, EndLine: 1, EndColumn: 2,
			}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseLocations(decodeResult(t, tc.payload))
			if len(got) != len(tc.want) {
				t.Fatalf("got %d locations, want %d: %+v", len(got), len(tc.want), got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("location %d:\n got %+v\nwant %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestParseHover_AllFourContentShapes pins the four `contents` shapes.
//
// MarkupContent is the only one current servers send, which is exactly why the
// other three are easy to drop: a parser written against gopls output today
// produces an empty hover, silently, against any server pinned to an older
// protocol version.
func TestParseHover_AllFourContentShapes(t *testing.T) {
	cases := []struct {
		name     string
		payload  string
		want     string
		wantLine int
	}{
		{
			name:    "MarkupContent (current servers)",
			payload: `{"result":{"contents":{"kind":"markdown","value":"func Close() error"}}}`,
			want:    "func Close() error",
		},
		{
			name:    "bare string (LSP 3.0 MarkedString)",
			payload: `{"result":{"contents":"func Close() error"}}`,
			want:    "func Close() error",
		},
		{
			name:    "MarkedString object gets fenced with its language",
			payload: `{"result":{"contents":{"language":"go","value":"func Close() error"}}}`,
			want:    "```go\nfunc Close() error\n```",
		},
		{
			name:    "array mixing MarkedString objects and bare strings",
			payload: `{"result":{"contents":[{"language":"go","value":"func Close() error"},"Close shuts the manager down."]}}`,
			want:    "```go\nfunc Close() error\n```\n\nClose shuts the manager down.",
		},
		{
			name:     "range is converted to 1-based",
			payload:  `{"result":{"contents":"x","range":{"start":{"line":4,"character":2},"end":{"line":4,"character":7}}}}`,
			want:     "x",
			wantLine: 5,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := parseHover(decodeResult(t, tc.payload))
			if h == nil {
				t.Fatal("parseHover returned nil for a response that carried contents")
			}
			if h.Contents != tc.want {
				t.Errorf("contents:\n got %q\nwant %q", h.Contents, tc.want)
			}
			if tc.wantLine != 0 && h.Line != tc.wantLine {
				t.Errorf("line = %d, want %d", h.Line, tc.wantLine)
			}
		})
	}
}

func TestParseHover_EmptyAndNull(t *testing.T) {
	for _, payload := range []string{`{"result":null}`, `{"result":{"contents":""}}`, `{"result":{"contents":{"kind":"markdown","value":""}}}`} {
		if h := parseHover(decodeResult(t, payload)); h != nil {
			t.Errorf("%s: expected nil hover, got %+v", payload, h)
		}
	}
}

// TestParseSymbols_BothDocumentSymbolShapes pins the two documentSymbol
// response shapes plus the workspace/symbol one.
//
// The nested DocumentSymbol form carries NO uri on any node — the uri is
// implied by the request. A parser that reads `location.uri` unconditionally
// yields symbols whose path is "", which renders as a result the model cannot
// open, and does so only against servers that chose the nested form.
func TestParseSymbols_BothDocumentSymbolShapes(t *testing.T) {
	t.Run("nested DocumentSymbol tree flattens with containers and inherits the request uri", func(t *testing.T) {
		payload := `{"result":[{"name":"Manager","kind":23,
			"range":{"start":{"line":10,"character":0},"end":{"line":40,"character":1}},
			"selectionRange":{"start":{"line":10,"character":5},"end":{"line":10,"character":12}},
			"children":[
				{"name":"Close","kind":6,
				 "range":{"start":{"line":30,"character":0},"end":{"line":35,"character":1}},
				 "selectionRange":{"start":{"line":30,"character":18},"end":{"line":30,"character":23}}}
			]}]}`
		got := parseSymbols(decodeResult(t, payload), "file:///work/manager.go")
		if len(got) != 2 {
			t.Fatalf("expected the tree to flatten to 2 symbols, got %d: %+v", len(got), got)
		}
		if got[0].Name != "Manager" || got[0].Kind != SymbolStruct || got[0].Container != "" {
			t.Errorf("parent symbol wrong: %+v", got[0])
		}
		// selectionRange (the name) not range (the body): line 11, not 11..41.
		if got[0].Location.Line != 11 || got[0].Location.Column != 6 {
			t.Errorf("parent should point at the NAME (selectionRange), got %d:%d", got[0].Location.Line, got[0].Location.Column)
		}
		if got[1].Name != "Close" || got[1].Container != "Manager" {
			t.Errorf("child should record its parent as container: %+v", got[1])
		}
		want := filepath.Clean("/work/manager.go")
		for _, s := range got {
			if s.Location.Path != want {
				t.Errorf("%s: path = %q, want %q — a nested DocumentSymbol carries no uri, "+
					"so it must inherit the one the request was made against", s.Name, s.Location.Path, want)
			}
		}
	})

	t.Run("flat SymbolInformation uses its own location and containerName", func(t *testing.T) {
		payload := `{"result":[{"name":"Close","kind":6,"containerName":"Manager",
			"location":{"uri":"file:///work/manager.go","range":{"start":{"line":29,"character":17},"end":{"line":29,"character":22}}}}]}`
		got := parseSymbols(decodeResult(t, payload), "file:///ignored.go")
		if len(got) != 1 {
			t.Fatalf("got %d symbols", len(got))
		}
		if got[0].Container != "Manager" || got[0].Location.Line != 30 || got[0].Location.Column != 18 {
			t.Errorf("wrong: %+v", got[0])
		}
		if got[0].Location.Path != filepath.Clean("/work/manager.go") {
			t.Errorf("a SymbolInformation must use ITS OWN uri, not the fallback: %q", got[0].Location.Path)
		}
	})

	t.Run("WorkspaceSymbol with no range yields line 0, not line 1", func(t *testing.T) {
		// A WorkspaceSymbol may legally omit the range (resolved later via
		// workspaceSymbol/resolve). Line 0 is the honest "unknown"; turning an
		// absent range into line 1 would send the model to the top of the file
		// and look like a real answer.
		payload := `{"result":[{"name":"Close","kind":12,"location":{"uri":"file:///work/x.go"}}]}`
		got := parseSymbols(decodeResult(t, payload), "")
		if len(got) != 1 {
			t.Fatalf("got %d symbols", len(got))
		}
		if got[0].Location.Line != 0 {
			t.Errorf("absent range must yield line 0 (unknown), got %d", got[0].Location.Line)
		}
	})

	t.Run("unnamed entries are dropped", func(t *testing.T) {
		payload := `{"result":[{"kind":12},{"name":"Real","kind":12,"location":{"uri":"file:///w/x.go","range":{"start":{"line":0,"character":0},"end":{"line":0,"character":1}}}}]}`
		got := parseSymbols(decodeResult(t, payload), "")
		if len(got) != 1 || got[0].Name != "Real" {
			t.Fatalf("expected only the named symbol, got %+v", got)
		}
	})

	t.Run("non-array result yields nothing", func(t *testing.T) {
		if got := parseSymbols(decodeResult(t, `{"result":null}`), ""); got != nil {
			t.Errorf("null result should yield nil, got %+v", got)
		}
	})
}

func TestSymbolKindString(t *testing.T) {
	cases := map[SymbolKind]string{
		SymbolStruct: "struct", SymbolMethod: "method", SymbolFunction: "function",
		SymbolInterface: "interface", SymbolTypeParameter: "type-parameter",
		SymbolKind(0): "", SymbolKind(99): "",
	}
	for k, want := range cases {
		if got := k.String(); got != want {
			t.Errorf("SymbolKind(%d).String() = %q, want %q", k, got, want)
		}
	}
}

// TestURLToPath_RoundTripsPathToURL pins the inverse relationship. The two
// functions are only ever used as a pair — pathToURL on the way out,
// urlToPath on the way back — so an asymmetry (a percent-escape that survives,
// a drive-letter slash that does not get stripped) turns every navigation
// result into a path that does not exist on disk.
func TestURLToPath_RoundTripsPathToURL(t *testing.T) {
	paths := []string{
		filepath.Join(t.TempDir(), "main.go"),
		filepath.Join(t.TempDir(), "dir with spaces", "a b.go"),
		filepath.Join(t.TempDir(), "中文目录", "文件.go"),
		filepath.Join(t.TempDir(), "pct%20literal.go"),
	}
	for _, p := range paths {
		uri := pathToURL(p)
		back := urlToPath(uri)
		if back != filepath.Clean(p) {
			t.Errorf("round trip lost the path:\n in  %q\n uri %q\n out %q", p, uri, back)
		}
	}
}

func TestURLToPath_NonFileSchemesYieldEmpty(t *testing.T) {
	// A jdt:/untitled: location is a real answer with no path on this disk.
	// Fabricating a path for it would send the model to open a file that does
	// not exist; the caller keeps the raw URI instead.
	for _, uri := range []string{
		"jdt://contents/rt.jar/java.lang/String.class",
		"untitled:Untitled-1",
		"zipfile:///a.zip::b/c.py",
		"",
		"://malformed",
	} {
		if got := urlToPath(uri); got != "" {
			t.Errorf("urlToPath(%q) = %q, want empty", uri, got)
		}
	}
}

func TestURLToPath_StripsWindowsDriveSlashOnAnyHost(t *testing.T) {
	// A Windows-produced URI can reach a non-Windows host via a recorded
	// fixture or a remote server, so the leading-slash strip must not be
	// conditioned on runtime.GOOS.
	got := urlToPath("file:///C:/work/main.go")
	want := "C:/work/main.go"
	if runtime.GOOS == "windows" {
		want = `C:\work\main.go`
	}
	if got != want {
		t.Errorf("urlToPath = %q, want %q", got, want)
	}
	if strings.HasPrefix(got, "/") {
		t.Errorf("drive-letter URI kept its leading slash: %q", got)
	}
}
