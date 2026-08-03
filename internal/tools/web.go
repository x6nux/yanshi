package tools

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
	w := &WebTools{maxBytes: maxBytes, timeout: timeout, searchBase: "https://lite.duckduckgo.com/lite"}
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, w.searchBase+"?q="+url.QueryEscape(a.Query), nil)
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
	html := string(body)
	results := make([]searchItem, 0, a.MaxResults)
	// Simple HTML extraction: find <a> tags with href in search results.
	for _, line := range strings.Split(html, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "<a ") && strings.Contains(line, "class=\"result-link\"") {
			results = append(results, searchItem{Title: line, URL: ""})
			if len(results) >= a.MaxResults {
				break
			}
		}
	}
	return toJSON(searchResult{Results: results}), nil
}
