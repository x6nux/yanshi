package tools

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/netpolicy"
)

// setSearchEndpoint points a WebTools' search backend at an arbitrary endpoint
// (httptest servers in tests, unreachable hosts in degradation tests). The
// endpoint lives on the backend now (W-F-27) — it is no longer a field of
// WebTools that production never configured.
func setSearchEndpoint(w *WebTools, endpoint string) {
	w.search = NewDuckDuckGoSearch(endpoint, w.maxBytes, w.timeout)
}

// searchCtx is the minimal context runSearch needs: a profile for Authorize
// (InvokableRun path) and an allow-all network policy so the backend may dial
// the httptest host.
func searchCtx() context.Context {
	ctx := WithProfile(context.Background(), profileWithWebTool())
	return WithNetworkPolicy(ctx, allowAllPolicy())
}

// antiBotPage is a well-formed HTML page with neither result anchors nor a
// no-results marker — the shape a 200-scored challenge page has. Under the old
// contract this parsed to zero results and returned as a SUCCESSFUL empty
// search.
const antiBotPage = `<!doctype html><html><head><title>Attention Required</title></head>
<body><div class="challenge">Please complete the security check</div></body></html>`

// noResultsPage carries DuckDuckGo's own no-results marker, i.e. the page
// shape a legitimately-matched-nothing query produces.
const noResultsPage = `<!doctype html><html><body>
<div class="no-results">No results.</div></body></html>`

// TestWFS27_ParseErrorIsNotSilentEmpty is THE failure-detection clause: a page
// that yields no anchors and carries no no-results marker must come back as
// parse_error, not as an empty success. This is the exact path on which the
// previous parser survived broken for months — every search returned an empty
// list and an empty list looked like "nothing matched".
//
// Mutation: delete the ddgNoResultsMarkers fallback branch in
// DuckDuckGoSearch.Search (report empty unconditionally) and this goes red —
// the two situations collapse back into one indistinguishable empty list.
func TestWFS27_ParseErrorIsNotSilentEmpty(t *testing.T) {
	for name, page := range map[string]string{
		"anti-bot page":    antiBotPage,
		"unrelated markup": "<html><body><p>hello</p></body></html>",
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(page))
			}))
			defer srv.Close()

			w := NewWebTools(1<<20, 5*time.Second)
			setSearchEndpoint(w, srv.URL)
			out, err := w.Search.InvokableRun(searchCtx(), `{"query":"go docs"}`)
			require.NoError(t, err)
			assert.Contains(t, out, `"status":"parse_error"`,
				"an unparseable page must be reported as a broken read, not as an empty search: %s", out)
			assert.NotContains(t, out, `"status":"empty"`)
		})
	}
}

// TestWFS27_EmptyIsDistinguishableFromParseError pins the other half: when the
// page DOES carry the no-results marker, the tool must say "empty" — a
// legitimately matched-nothing query is evidence about the query, and collapsing
// it into parse_error would make every miss look like a broken pipe.
//
// Mutation: make the marker list empty and this turns red (the no-results page
// would be misread as parse_error).
func TestWFS27_EmptyIsDistinguishableFromParseError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w2write(w, noResultsPage)
	}))
	defer srv.Close()

	w := NewWebTools(1<<20, 5*time.Second)
	setSearchEndpoint(w, srv.URL)
	out, err := w.Search.InvokableRun(searchCtx(), `{"query":"xyzzy qwerty nothing"}`)
	require.NoError(t, err)
	assert.Contains(t, out, `"status":"empty"`)
	assert.Contains(t, out, `"results":[]`)
}

// TestWFS27_HTTPErrorIsBackendError pins the transport-tier clause: a non-2xx
// answer (rate limit, anti-bot 403) is a backend_error regardless of body —
// the challenge page's body must not be handed to the HTML parser at all.
//
// Mutation: drop the status-code check in readSearchBody and this goes red
// (the 403 body would parse to parse_error or empty instead).
func TestWFS27_HTTPErrorIsBackendError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w2write(w, antiBotPage)
	}))
	defer srv.Close()

	w := NewWebTools(1<<20, 5*time.Second)
	setSearchEndpoint(w, srv.URL)
	out, err := w.Search.InvokableRun(searchCtx(), `{"query":"go docs"}`)
	require.NoError(t, err)
	assert.Contains(t, out, `"status":"backend_error"`)
	assert.Contains(t, out, "HTTP 403")
}

// TestWFS27_NetworkFailureIsVisible pins that an unreachable endpoint degrades
// visibly: no hard failure (the turn must not die), but backend_error + note —
// never the silent empty success the old code returned.
func TestWFS27_NetworkFailureIsVisible(t *testing.T) {
	w := NewWebTools(1<<20, 5*time.Second)
	setSearchEndpoint(w, "http://127.0.0.1:1/x")
	out, err := w.Search.InvokableRun(searchCtx(), `{"query":"go docs"}`)
	require.NoError(t, err)
	assert.Contains(t, out, `"status":"backend_error"`)
	assert.Contains(t, out, "unreachable")
}

// TestWFS27_OkStatusCarriesResults keeps the happy path honest: results ride
// under status "ok", and the old title/URL/snippet content still surfaces.
func TestWFS27_OkStatusCarriesResults(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w2write(w, ddgPage)
	}))
	defer srv.Close()

	w := NewWebTools(1<<20, 5*time.Second)
	setSearchEndpoint(w, srv.URL)
	out, err := w.Search.InvokableRun(searchCtx(), `{"query":"go docs"}`)
	require.NoError(t, err)
	assert.Contains(t, out, `"status":"ok"`)
	for _, want := range []string{"The Go Docs", "https://go.dev/doc/", "Everything about Go."} {
		assert.Contains(t, out, want)
	}
}

// TestWFS27_BackendFactory pins the config seam: "" and "duckduckgo" build the
// default backend (empty endpoint → DefaultDuckDuckGoEndpoint, non-empty → the
// given one); searxng requires an endpoint; an unknown name is an error that
// names the known ones — never a silent fallback.
func TestWFS27_BackendFactory(t *testing.T) {
	b, err := NewSearchBackend("", "", 0, 0)
	require.NoError(t, err)
	assert.Equal(t, "duckduckgo", b.Name())

	b, err = NewSearchBackend("duckduckgo", "https://mirror.example/html/", 0, 0)
	require.NoError(t, err)
	ddg, ok := b.(*DuckDuckGoSearch)
	require.True(t, ok)
	assert.Equal(t, "https://mirror.example/html/", ddg.endpoint)

	_, err = NewSearchBackend("searxng", "", 0, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "endpoint")

	b, err = NewSearchBackend("searxng", "https://searx.internal/search", 0, 0)
	require.NoError(t, err)
	assert.Equal(t, "searxng", b.Name())

	_, err = NewSearchBackend("startpage", "", 0, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duckduckgo")
	assert.Contains(t, err.Error(), "searxng")
}

// TestWFS27_SearXNGBackend drives the self-hosted backend against a fixture
// instance: result mapping, format=json, freshness → time_range, site folded
// into q.
func TestWFS27_SearXNGBackend(t *testing.T) {
	var gotPath string
	var gotQuery map[string][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.Query()
		_, _ = w2write(w, `{"results":[
			{"title":"Go Home","url":"https://go.dev/","content":"The Go programming language"},
			{"title":"Spec","url":"https://go.dev/ref/spec","content":"The Go spec"}
		]}`)
	}))
	defer srv.Close()

	b, err := NewSearchBackend("searxng", srv.URL+"/search", 0, 0)
	require.NoError(t, err)
	ctx := WithNetworkPolicy(context.Background(), &netpolicy.Policy{Default: "allow", AllowPrivate: true})
	out, err := b.Search(ctx, SearchQuery{Query: "go generics", Site: "go.dev", Freshness: "month", MaxResults: 1})
	require.NoError(t, err)

	assert.Equal(t, SearchOK, out.Status)
	require.Len(t, out.Results, 1, "max_results must cap the outcome")
	assert.Equal(t, "Go Home", out.Results[0].Title)
	assert.Equal(t, "https://go.dev/", out.Results[0].URL)
	assert.Equal(t, "The Go programming language", out.Results[0].Snippet)
	assert.Equal(t, "/search", gotPath)
	assert.Equal(t, "json", strings.Join(gotQuery["format"], ""))
	assert.Equal(t, "month", strings.Join(gotQuery["time_range"], ""))
	assert.Contains(t, strings.Join(gotQuery["q"], ""), "site:go.dev")
}

// TestWFS27_SearXNGNonJSONIsParseError pins the JSON backend's broken-read
// detection: an instance that answers with an HTML error page (json format
// disabled) must be parse_error, not empty.
func TestWFS27_SearXNGNonJSONIsParseError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w2write(w, "<html><body>json format is disabled</body></html>")
	}))
	defer srv.Close()

	b, err := NewSearchBackend("searxng", srv.URL, 0, 0)
	require.NoError(t, err)
	ctx := WithNetworkPolicy(context.Background(), &netpolicy.Policy{Default: "allow", AllowPrivate: true})
	out, err := b.Search(ctx, SearchQuery{Query: "x"})
	require.NoError(t, err)
	assert.Equal(t, SearchParseError, out.Status)
}

// TestWFS27_SearXNGEmptyResults pins that an instance replying with an empty
// results array is reported as empty (evidence about the query), with the
// results key present so the model reads a stable shape.
func TestWFS27_SearXNGEmptyResults(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w2write(w, `{"results":[]}`)
	}))
	defer srv.Close()

	b, err := NewSearchBackend("searxng", srv.URL, 0, 0)
	require.NoError(t, err)
	ctx := WithNetworkPolicy(context.Background(), &netpolicy.Policy{Default: "allow", AllowPrivate: true})
	out, err := b.Search(ctx, SearchQuery{Query: "x"})
	require.NoError(t, err)
	assert.Equal(t, SearchEmpty, out.Status)
	assert.Empty(t, out.Results)
}

// TestWFS27_StatusFieldDistinguishesEndToEnd drives the TOOL (not the backend)
// for all three non-ok statuses so the wire-level claim holds: the JSON the
// model reads carries a different status for each — the distinguishability is
// the acceptance criterion, and it must hold at the tool boundary.
func TestWFS27_StatusFieldDistinguishesEndToEnd(t *testing.T) {
	mk := func(page string, code int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
			_, _ = w2write(w, page)
		}))
	}
	cases := []struct {
		name string
		page string
		code int
		want string
	}{
		{"no-results marker", noResultsPage, http.StatusOK, `"status":"empty"`},
		{"unparseable page", antiBotPage, http.StatusOK, `"status":"parse_error"`},
		{"http 403", antiBotPage, http.StatusForbidden, `"status":"backend_error"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := mk(tc.page, tc.code)
			defer srv.Close()
			w := NewWebTools(1<<20, 5*time.Second)
			setSearchEndpoint(w, srv.URL)
			out, err := w.Search.InvokableRun(searchCtx(), `{"query":"q"}`)
			require.NoError(t, err)
			if !strings.Contains(out, tc.want) {
				t.Fatalf("want %s in %s", tc.want, out)
			}
		})
	}
}

// w2write exists so the table-driven servers above can ignore write errors
// without repeating the two-value dance inline.
func w2write(w http.ResponseWriter, s string) (int, error) {
	return w.Write([]byte(s))
}
