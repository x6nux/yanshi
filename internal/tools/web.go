package tools

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/x6nux/yanshi/internal/netpolicy"
)

// WebTools exposes web_fetch (HTTP GET) and web_search (search query).
type WebTools struct {
	maxBytes int
	timeout  time.Duration
	// search 是 W-F-27 的可插拔后端（见 websearch.go）。nil 落到默认的
	// duckduckgo——与「后端可配」之前的旧路径行为一致。
	search SearchBackend
	Fetch  *GuardedTool
	Search *GuardedTool
}

// NewWebTools builds web tools with the default duckduckgo search backend.
// maxBytes caps response body size (0 → default 1 MiB); timeout caps each HTTP
// request (0 → default 30s). Both the fetch and search tools are constructed
// with the same timeout/maxBytes.
func NewWebTools(maxBytes int, timeout time.Duration) *WebTools {
	return NewWebToolsWithSearch(maxBytes, timeout, nil)
}

// NewWebToolsWithSearch builds web tools with an explicit search backend
// (W-F-27). A nil backend falls back to the default duckduckgo backend, i.e.
// the exact pre-pluggable behaviour. Bootstrap resolves the backend from
// config (tools.web_search) via NewSearchBackend; tests pass an
// httptest-backed one.
func NewWebToolsWithSearch(maxBytes int, timeout time.Duration, sb SearchBackend) *WebTools {
	w := &WebTools{maxBytes: maxBytes, timeout: timeout, search: sb}
	if w.maxBytes <= 0 {
		w.maxBytes = 1 << 20 // 1 MiB default
	}
	if w.timeout <= 0 {
		w.timeout = 30 * time.Second
	}
	if w.search == nil {
		w.search = NewDuckDuckGoSearch("", w.maxBytes, w.timeout)
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
		// W-B-12's net dimension is consumed HERE, and this is the only place
		// it ever was going to be: netpolicy replaced the profile-based
		// guard.NetHost check, so nothing downstream of a permission profile
		// judges a host any more. Without these three lines the operator
		// approves, the model reads granted=true, and this function returns the
		// identical DenyErr.
		granted, ok := grantedNetworkPolicy(ctx, "web_fetch", host, policy)
		if !ok {
			return "", &DenyErr{Reason: d.Reason}
		}
		policy = granted
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

// searchPayload 是 web_search 回喂模型的 JSON。status 字段是 W-F-27 的失败
// 检测收口：模型读到的不再是裸结果列表，而是四种结局之一——"ok"（命中），
// "empty"（上游明确无命中），"backend_error"（管道失败，对查询不构成证据），
// "parse_error"（读不出应答形态，解析器坏了）。此前网络错误被渲染成一次成功
// 的空搜索，模型会把「上游挂了」读成「网上没有」。
type searchPayload struct {
	Status  string       `json:"status"`
	Note    string       `json:"note,omitempty"`
	Results []searchItem `json:"results"`
}

func (w *WebTools) runSearch(ctx context.Context, argsJSON string) (string, error) {
	var a webSearchArgs
	if err := ParseArgs(argsJSON, &a); err != nil {
		return "", err
	}
	if a.MaxResults <= 0 || a.MaxResults > 50 {
		a.MaxResults = 10
	}
	out, err := w.search.Search(ctx, SearchQuery{
		Query:      a.Query,
		Site:       a.Site,
		Freshness:  a.Freshness,
		MaxResults: a.MaxResults,
	})
	if err != nil {
		return "", err
	}
	return toJSON(searchPayload{
		Status:  string(out.Status),
		Note:    out.Note,
		Results: out.Results,
	}), nil
}
