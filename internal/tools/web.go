package tools

import (
	"context"
	"fmt"
	htmlpkg "html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/x6nux/yanshi/internal/netpolicy"
)

// WebTools exposes web_fetch (HTTP GET) and web_search (search query).
type WebTools struct {
	maxBytes   int
	timeout    time.Duration
	searchBase string // override for tests
	Fetch      *GuardedTool
	Search     *GuardedTool
}

// NewWebTools builds web tools. maxBytes caps response body size (0 → default
// 1 MiB); timeout caps each HTTP request (0 → default 30s). Both the fetch and
// search tools are constructed with the same timeout/maxBytes.
func NewWebTools(maxBytes int, timeout time.Duration) *WebTools {
	w := &WebTools{maxBytes: maxBytes, timeout: timeout, searchBase: "https://html.duckduckgo.com/html/"}
	if w.maxBytes <= 0 {
		w.maxBytes = 1 << 20 // 1 MiB default
	}
	if w.timeout <= 0 {
		w.timeout = 30 * time.Second
	}
	w.Fetch = NewGuardedTool(
		"web_fetch", "Fetch", "Fetch a URL via HTTP GET and return the response body as text.",
		w.timeout,
		params(map[string]*schema.ParameterInfo{
			"url": {Type: schema.String, Desc: "URL to fetch", Required: true},
		}),
		SyncStream(w.runFetch),
	)
	w.Search = NewGuardedTool(
		"web_search", "Search",
		"Search the web and return a list of result titles and URLs.",
		w.timeout,
		params(map[string]*schema.ParameterInfo{
			"query":       {Type: schema.String, Required: true, Desc: "Search query"},
			"max_results": {Type: schema.Integer, Desc: "Max results (default 10)"},
			"site":        {Type: schema.String, Desc: "Restrict results to one domain, e.g. go.dev"},
			"freshness": {Type: schema.String,
				Desc: "Restrict results by age: day | week | month | year"},
		}),
		SyncStream(w.runSearch),
	)
	return w
}

type fetchArgs struct {
	URL string `json:"url"`
}

// truncatingReader reads from r up to limit bytes, then signals EOF. The
// `truncated` flag is set when the underlying reader had more data than limit.
// Unlike io.LimitReader, it reports whether truncation occurred so the caller
// can annotate the output.
type truncatingReader struct {
	r         io.Reader
	limit     int
	total     int
	truncated bool
}

func (t *truncatingReader) Read(p []byte) (int, error) {
	if t.total >= t.limit {
		t.truncated = true
		return 0, io.EOF
	}
	n, err := t.r.Read(p)
	t.total += n
	if t.total > t.limit {
		// Trim this chunk to stay within the limit.
		excess := t.total - t.limit
		n -= excess
		t.total = t.limit
		t.truncated = true
		if n <= 0 {
			return 0, io.EOF
		}
	}
	return n, err
}

func (w *WebTools) runFetch(ctx context.Context, argsJSON string) (string, error) {
	var a fetchArgs
	if err := ParseArgs(argsJSON, &a); err != nil {
		return "", err
	}

	// web_fetch shares ONE host-policy source with the loopback proxy. The
	// profile-based guard.NetHost check that lived here is replaced by
	// netpolicy.Policy so the operator's security.network block is applied
	// uniformly. Fail-closed: if no policy is bound, deny.
	policy, ok := NetworkPolicyFromContext(ctx)
	if !ok {
		return "", &DenyErr{Reason: "no network policy in context"}
	}
	host := hostOnly(a.URL)
	if host == "" {
		return "", &DenyErr{Reason: "invalid url / empty host"}
	}
	if d := policy.CheckHost(host); !d.Allowed {
		return "", &DenyErr{Reason: d.Reason}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return "", fmt.Errorf("web.fetch: build request: %w", err)
	}
	cli := &http.Client{
		Timeout:   w.timeout,
		Transport: netpolicy.NewTransport(policy),
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("web.fetch: stopped after 10 redirects")
			}
			host := req.URL.Hostname()
			if host == "" {
				return &DenyErr{Reason: "redirect target has empty host"}
			}
			if d := policy.CheckHost(host); !d.Allowed {
				return &DenyErr{Reason: "redirect denied: " + d.Reason}
			}
			return nil
		},
	}
	resp, err := cli.Do(req)
	if err != nil {
		return "", fmt.Errorf("web.fetch: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("web.fetch: HTTP %d", resp.StatusCode)
	}
	// Tier G entry C: an image/* response never returns its raw bytes as "text".
	// The net policy above has already decided this request was allowed — the
	// branch below only changes how the AUTHORIZED response is PRESENTED: as a
	// real attachment when the turn's model is multimodal, otherwise as the
	// original hint (no binary in a text message, ever).
	if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(ct, "image/") {
		if imageAttachEnabled(ctx) {
			if out, ok := attachResponseImage(ctx, req.URL.String(), ct, resp.Body); ok {
				return out, nil
			}
		}
		return fmt.Sprintf("[image response: %s (%s) -- call image_describe with this URL or fetch the bytes to understand it]", req.URL.String(), ct), nil
	}
	tr := &truncatingReader{r: resp.Body, limit: w.maxBytes}
	body, err := io.ReadAll(tr)
	if err != nil {
		return "", fmt.Errorf("web.fetch: read body: %w", err)
	}
	out := string(body)
	if tr.truncated {
		out += fmt.Sprintf("\n[body truncated: kept %d of %d bytes]", w.maxBytes, tr.total)
	}
	return out, nil
}

type webSearchArgs struct {
	Query      string `json:"query"`
	MaxResults int    `json:"max_results"`
	Site       string `json:"site"`
	Freshness  string `json:"freshness"`
}

// freshnessCodes maps the tool's vocabulary onto DuckDuckGo's df parameter.
//
// The tool takes words rather than passing df through: a caller that guesses
// "1d" or "past_week" would otherwise get an unfiltered search that looks
// filtered, and an unknown value is rejected below rather than dropped.
var freshnessCodes = map[string]string{
	"day": "d", "week": "w", "month": "m", "year": "y",
}

// searchQuery folds the site restriction into the query string.
//
// site: is a query operator, not a form field — the endpoint has no separate
// domain parameter — so the restriction has to travel inside q. A caller that
// already wrote "site:" in the query keeps theirs; adding a second one returns
// nothing at all.
func searchQuery(query, site string) string {
	site = strings.TrimSpace(site)
	if site == "" || strings.Contains(strings.ToLower(query), "site:") {
		return query
	}
	return strings.TrimSpace(query) + " site:" + site
}

type searchResult struct {
	Results []searchItem `json:"results"`
}

type searchItem struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet,omitempty"`
}

func (w *WebTools) runSearch(ctx context.Context, argsJSON string) (string, error) {
	var a webSearchArgs
	if err := ParseArgs(argsJSON, &a); err != nil {
		return "", err
	}
	if a.MaxResults <= 0 || a.MaxResults > 50 {
		a.MaxResults = 10
	}
	policy, ok := NetworkPolicyFromContext(ctx)
	if !ok {
		return "", &DenyErr{Reason: "no network policy in context"}
	}
	searchHost := hostOnly(w.searchBase)
	if searchHost == "" {
		return "", &DenyErr{Reason: "invalid search base URL"}
	}
	if d := policy.CheckHost(searchHost); !d.Allowed {
		return "", &DenyErr{Reason: d.Reason}
	}
	// POST with a form body, not GET with a query string. The lite endpoint
	// this used to query became an anti-bot page that returns no results at
	// all, and the html endpoint answers a GET the same way; the POST form is
	// the shape it actually serves results to.
	form := url.Values{"q": {searchQuery(a.Query, a.Site)}}
	if f := strings.ToLower(strings.TrimSpace(a.Freshness)); f != "" {
		code, ok := freshnessCodes[f]
		if !ok {
			return "", fmt.Errorf("web.search: unknown freshness %q (want day, week, month or year)", a.Freshness)
		}
		form.Set("df", code)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.searchBase, strings.NewReader(form.Encode()))
	if req != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if err != nil {
		return "", fmt.Errorf("web.search: build request: %w", err)
	}
	cli := &http.Client{
		Timeout:   w.timeout,
		Transport: netpolicy.NewTransport(policy),
	}
	resp, err := cli.Do(req)
	if err != nil {
		// Network errors return an empty result set, not a hard failure — the
		// search tool degrades to "no results" rather than blocking the turn.
		return toJSON(searchResult{Results: []searchItem{}}), nil
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(w.maxBytes)))
	if err != nil {
		return "", fmt.Errorf("web.search: read: %w", err)
	}
	results := parseDuckDuckGoHTML(string(body), a.MaxResults)
	return toJSON(searchResult{Results: results}), nil
}

var (
	// ddgResultRe matches one result anchor: class="result__a", an href, and
	// the inner markup that holds the title.
	ddgResultRe = regexp.MustCompile(`(?is)<a[^>]*class="result__a"[^>]*href="([^"]*)"[^>]*>(.*?)</a>`)
	// ddgSnippetRe matches the description anchor that follows a result.
	ddgSnippetRe = regexp.MustCompile(`(?is)<a[^>]*class="result__snippet"[^>]*>(.*?)</a>`)
	ddgTagRe     = regexp.MustCompile(`(?s)<[^>]*>`)
	ddgSpaceRe   = regexp.MustCompile(`\s+`)
)

// parseDuckDuckGoHTML extracts title, URL and snippet from a DuckDuckGo HTML
// results page.
//
// It is a pure function so it can be asserted against a fixture. A test that
// queried the live endpoint would be testing DuckDuckGo's uptime, and -- worse
// -- would keep passing on the day the markup changed, because the tool
// degrades a failed search to an empty result set rather than an error. That
// is exactly how the previous parser went unnoticed: it stored the raw <a>
// line as the title, hardcoded URL to "", never assigned Snippet at all, and
// pointed at an endpoint that had become an anti-bot page. Every search
// returned an empty list, and an empty list is indistinguishable from "nothing
// matched".
//
// Snippets are paired by ORDER rather than by containment: the markup nests
// each snippet in the same result block as its anchor, but matching blocks
// with a regex is fragile in a way that ordering is not, and DuckDuckGo emits
// them strictly interleaved. A result with no snippet simply gets none --
// pairing must not shift every later snippet up by one.
func parseDuckDuckGoHTML(html string, max int) []searchItem {
	if max <= 0 {
		max = 10
	}
	anchors := ddgResultRe.FindAllStringSubmatchIndex(html, -1)
	snippets := ddgSnippetRe.FindAllStringSubmatchIndex(html, -1)

	out := make([]searchItem, 0, len(anchors))
	for i, m := range anchors {
		if len(out) >= max {
			break
		}
		item := searchItem{
			URL:   ddgResolveURL(html[m[2]:m[3]]),
			Title: ddgText(html[m[4]:m[5]]),
		}
		// The snippet belonging to this anchor is the first one that starts
		// after it and before the next anchor begins.
		nextAnchor := len(html)
		if i+1 < len(anchors) {
			nextAnchor = anchors[i+1][0]
		}
		for _, sm := range snippets {
			if sm[0] > m[1] && sm[0] < nextAnchor {
				item.Snippet = ddgText(html[sm[2]:sm[3]])
				break
			}
		}
		out = append(out, item)
	}
	return out
}

// ddgResolveURL unwraps DuckDuckGo's /l/?uddg= redirect to the real
// destination. A direct href is returned unchanged, so a markup change that
// drops the redirect degrades to a working URL rather than to nothing.
func ddgResolveURL(href string) string {
	href = htmlpkg.UnescapeString(strings.TrimSpace(href))
	if strings.HasPrefix(href, "//") {
		href = "https:" + href
	}
	u, err := url.Parse(href)
	if err != nil {
		return href
	}
	if target := u.Query().Get("uddg"); target != "" {
		return target
	}
	return href
}

// ddgText strips tags, decodes entities and collapses whitespace. The <b>
// highlight tags DuckDuckGo wraps around matched terms are why the raw inner
// markup cannot be used as a title.
func ddgText(s string) string {
	s = ddgTagRe.ReplaceAllString(s, "")
	s = htmlpkg.UnescapeString(s)
	return strings.TrimSpace(ddgSpaceRe.ReplaceAllString(s, " "))
}
