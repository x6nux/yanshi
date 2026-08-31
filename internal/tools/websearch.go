package tools

import (
	"context"
	"encoding/json"
	"fmt"
	htmlpkg "html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/x6nux/yanshi/internal/netpolicy"
)

// W-F-27: web_search 的后端可插拔。
//
// 定时炸弹的形状：此前 DuckDuckGo 的 HTML 端点写死在 NewWebTools 里、靠正则刮
// HTML，对方改版即碎，且没有内网/自托管替代路径。现在端点由配置给出、后端由
// 配置挑选，搜索逻辑收在 SearchBackend 后面——新后端（自托管 searxng、未来的
// 其它源）只实现这一个接口，web_search 工具本身不感知具体上游。
//
// 「失败伪装成成功」的防线在 SearchStatus：后端必须区分「上游答了、确实没有
// 命中」（SearchEmpty）与「我们根本没能从应答里读出结果」（SearchParseError /
// SearchBackendError）。空列表在这两种情形下含义完全不同——前者是关于查询的
// 证据，后者只说明管道断了；把后者渲染成空结果集，模型会把「端点改版了」读成
// 「网上没有这个」。历史上正是这个形状让坏掉的解析器存活了数月（见
// parseDuckDuckGoHTML 的 doc 注释）。
type SearchBackend interface {
	// Search 执行一次查询。返回 error 仅用于工具层该向模型回喂的硬错误
	// （参数非法、策略拒绝）；上游/解析层面的失败走 SearchOutcome.Status，
	// 让「搜不到」与「读不出」在返回值上可分辨。
	Search(ctx context.Context, q SearchQuery) (SearchOutcome, error)

	// Name 返回后端名（与配置里 tools.web_search.backend 的取值一致）。
	Name() string
}

// SearchQuery 是一次归一化后的搜索请求。Freshness 的取值表由各后端自行解释
//（duckduckgo 映射为 df 参数、searxng 映射为 time_range），不认识的值由后端
// 拒绝——一个被接受却被丢弃的过滤条件比被拒绝的更糟：调用方会拿到看起来过滤
// 过、实际没过滤的结果。
type SearchQuery struct {
	Query      string
	Site       string
	Freshness  string
	MaxResults int
}

// SearchStatus 区分一次搜索的四种结局。ok 之外的三种都携带 Note，模型读到的
// JSON 里会带 status 字段与这段说明，空结果不再冒充「没有命中」。
type SearchStatus string

const (
	// SearchOK：上游应答、解析出了结果列表（列表非空；「确实没有命中」属于
	// SearchEmpty，两者不能共用一个状态）。
	SearchOK SearchStatus = "ok"
	// SearchEmpty：上游明确表示没有命中（应答形态里带有它自己的无结果标记）。
	SearchEmpty SearchStatus = "empty"
	// SearchBackendError：管道失败——网络错误、非 2xx 应答。对查询本身不构成
	// 任何证据。
	SearchBackendError SearchStatus = "backend_error"
	// SearchParseError：HTTP 层成功、但应答里读不出任何已知的结果形态——
	// 最典型的原因是上游改版。这一档就是「正则刮 HTML 路径的失败检测」。
	SearchParseError SearchStatus = "parse_error"
)

// SearchOutcome 是后端交给工具层的标准化结果。
type SearchOutcome struct {
	Status  SearchStatus
	Note    string       // Status != SearchOK 时给人/模型读的解释
	Results []searchItem // Status == SearchOK 时的命中列表
}

// DefaultDuckDuckGoEndpoint 是 duckduckgo 后端在配置未给 endpoint 时的落点。
// 它是默认值而不是写死值：config.yaml 的 tools.web_search.endpoint 一经设置
// 即覆盖它，包括把 duckduckgo 指向一个镜像端点。
const DefaultDuckDuckGoEndpoint = "https://html.duckduckgo.com/html/"

// NewSearchBackend 按 (backend, endpoint) 构造搜索后端，是配置进入工具层的
// 唯一入口。backend 为空取 duckduckgo（既有行为的兼容落点）；searxng 必须
// 给出 endpoint（自托管实例没有公共默认值，猜一个只会把查询发给错误的主机）；
// 未知名直接报错而不是静默回落——配置拼错却拿到另一个后端，等于操作员想要的
// 内网路径悄悄不存在。
func NewSearchBackend(backend, endpoint string, maxBytes int, timeout time.Duration) (SearchBackend, error) {
	switch backend {
	case "", "duckduckgo":
		return NewDuckDuckGoSearch(endpoint, maxBytes, timeout), nil
	case "searxng":
		if strings.TrimSpace(endpoint) == "" {
			return nil, fmt.Errorf("tools: web_search backend %q requires tools.web_search.endpoint (a self-hosted searxng JSON URL)", backend)
		}
		return NewSearXNGSearch(endpoint, maxBytes, timeout), nil
	default:
		return nil, fmt.Errorf("tools: unknown web_search backend %q (known: duckduckgo, searxng)", backend)
	}
}

// searchHTTPClient 组一个受当前 netpolicy 约束的 HTTP client。策略缺失按
// DenyErr 拒绝（与 web_fetch 的 fail-closed 一致）；host 不在允许集时先问
// 审批管理器（grantedNetworkPolicy），与既有 web_search 的授权路径逐字节相同。
func searchHTTPClient(ctx context.Context, toolName, endpoint string, timeout time.Duration) (*http.Client, error) {
	policy, ok := NetworkPolicyFromContext(ctx)
	if !ok {
		return nil, &DenyErr{Reason: "no network policy in context"}
	}
	host := hostOnly(endpoint)
	if host == "" {
		return nil, &DenyErr{Reason: "invalid search endpoint URL"}
	}
	if d := policy.CheckHost(host); !d.Allowed {
		granted, ok := grantedNetworkPolicy(ctx, toolName, host, policy)
		if !ok {
			return nil, &DenyErr{Reason: d.Reason}
		}
		policy = granted
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: netpolicy.NewTransport(policy),
	}, nil
}

// readSearchBody 把上游应答体读进来并封顶，返回 (body, backendErrOutcome)。
// 非 2xx 一律是 backend_error：反爬/限流页（403 challenge、429）的应答体
// 恰恰长着「没有结果」的样子，把它们交给 HTML 解析器只会得到一次静默的空。
func readSearchBody(resp *http.Response, maxBytes int) (string, SearchOutcome, bool) {
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", SearchOutcome{
			Status: SearchBackendError,
			Note:   fmt.Sprintf("search backend answered HTTP %d (rate limit or anti-bot page); this says nothing about whether the query matches", resp.StatusCode),
		}, false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxBytes)))
	if err != nil {
		return "", SearchOutcome{
			Status: SearchBackendError,
			Note:   fmt.Sprintf("search backend response could not be read: %v", err),
		}, false
	}
	return string(body), SearchOutcome{}, true
}

// DuckDuckGoSearch 是默认后端：POST 表单到 DuckDuckGo 的 HTML 端点，正则刮
// 出结果。端点不再写死在构造函数里——它来自配置（tools.web_search.endpoint），
// 未配置时落到 DefaultDuckDuckGoEndpoint。刮 HTML 的路径按 W-F-27 的验收带
// 失败检测：解析结果为空时必须用页面自身的无结果标记区分「没有命中」与「读
// 不出」，见 ddgNoResultsMarkers。
type DuckDuckGoSearch struct {
	endpoint string
	maxBytes int
	timeout  time.Duration
}

// NewDuckDuckGoSearch 构造 duckduckgo 后端。endpoint 为空取
// DefaultDuckDuckGoEndpoint；maxBytes/timeout 非正数时取工具层的默认值。
func NewDuckDuckGoSearch(endpoint string, maxBytes int, timeout time.Duration) *DuckDuckGoSearch {
	if strings.TrimSpace(endpoint) == "" {
		endpoint = DefaultDuckDuckGoEndpoint
	}
	if maxBytes <= 0 {
		maxBytes = 1 << 20
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &DuckDuckGoSearch{endpoint: endpoint, maxBytes: maxBytes, timeout: timeout}
}

// Name 实现 SearchBackend。
func (d *DuckDuckGoSearch) Name() string { return "duckduckgo" }

// ddgNoResultsMarkers 是 DuckDuckGo HTML 应答在「查询合法但无命中」时页面里
// 会出现的小写形态。解析出 0 条结果时拿它们当判据：命中任一标记 → SearchEmpty
// （这是关于查询的证据）；一个都不命中 → SearchParseError（页面形态我们不认识
// 了——改版、反爬页、镜像端点的异形应答都落这里）。这张表只收「确认出现过」
// 的形态，猜的形态不收：一张含假阳性的表会把真断管道读成「没有命中」，那正是
// 本机制要消灭的那个失败方向。
var ddgNoResultsMarkers = []string{
	`class="no-results"`,
	"no results",
}

// Search 实现 SearchBackend。freshness 映射为 df 表单字段；site 折进 q（见
// searchQuery）。
func (d *DuckDuckGoSearch) Search(ctx context.Context, q SearchQuery) (SearchOutcome, error) {
	if q.MaxResults <= 0 {
		q.MaxResults = 10
	}
	form := url.Values{"q": {searchQuery(q.Query, q.Site)}}
	if f := strings.ToLower(strings.TrimSpace(q.Freshness)); f != "" {
		code, ok := freshnessCodes[f]
		if !ok {
			return SearchOutcome{}, fmt.Errorf("web.search: unknown freshness %q (want day, week, month or year)", q.Freshness)
		}
		form.Set("df", code)
	}
	cli, err := searchHTTPClient(ctx, "web_search", d.endpoint, d.timeout)
	if err != nil {
		return SearchOutcome{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.endpoint, strings.NewReader(form.Encode()))
	if req != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if err != nil {
		return SearchOutcome{}, fmt.Errorf("web.search: build request: %w", err)
	}
	resp, err := cli.Do(req)
	if err != nil {
		// 网络错误降级为 backend_error 而非硬失败：搜索坏掉不该中断整个
		// turn——但降级必须可见（status 字段 + Note），不再渲染成空结果集。
		return SearchOutcome{
			Status: SearchBackendError,
			Note:   fmt.Sprintf("search backend unreachable: %v", err),
		}, nil
	}
	body, fail, ok := readSearchBody(resp, d.maxBytes)
	if !ok {
		return fail, nil
	}
	results := parseDuckDuckGoHTML(body, q.MaxResults)
	if len(results) > 0 {
		return SearchOutcome{Status: SearchOK, Results: results}, nil
	}
	// 0 条命中：用页面自身的无结果标记分流「搜不到」与「读不出」。
	lower := strings.ToLower(body)
	for _, m := range ddgNoResultsMarkers {
		if strings.Contains(lower, m) {
			return SearchOutcome{
				Status:  SearchEmpty,
				Note:    "the backend answered and reported no matches for this query",
				Results: []searchItem{},
			}, nil
		}
	}
	return SearchOutcome{
		Status: SearchParseError,
		Note: "the backend answered but the page contained no recognizable results and no no-results marker; " +
			"the endpoint's markup may have changed (this is a broken parser, not an empty web)",
		Results: []searchItem{},
	}, nil
}

// searxngResponse 是 searxng JSON API（/search?format=json）的应答形状，只取
// 本工具消费的字段。
type searxngResponse struct {
	Results []struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Content string `json:"content"`
	} `json:"results"`
}

// SearXNGSearch 是自托管后端：走 searxng 实例的 JSON API。它是 spec 点名的
// 「内网替代路径」——组织把 searxng 架在内网、netpolicy 只放行那个主机，搜索
// 面就从公共互联网换成了自己的索引，DuckDuckGo 改版不再影响这个部署。
type SearXNGSearch struct {
	endpoint string
	maxBytes int
	timeout  time.Duration
}

// NewSearXNGSearch 构造 searxng 后端。endpoint 必须指向实例的 search 路径
// （如 https://searx.internal/search）；空 endpoint 在 Search 时报错兜底
//（配置层已先行校验，这里是构造函数无法返回错误的兜底）。
func NewSearXNGSearch(endpoint string, maxBytes int, timeout time.Duration) *SearXNGSearch {
	if maxBytes <= 0 {
		maxBytes = 1 << 20
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &SearXNGSearch{endpoint: endpoint, maxBytes: maxBytes, timeout: timeout}
}

// Name 实现 SearchBackend。
func (s *SearXNGSearch) Name() string { return "searxng" }

// Search 实现 SearchBackend。searxng 的 freshness 词表与工具层一致
//（day/week/month/year 原样映射为 time_range）；site 同样折进 q。JSON 缺失
// results 字段（如实例禁用了 json 格式、返回的是 HTML 错误页）按 parse_error
// 报告——那条路径同样是「读不出」而非「没有命中」。
func (s *SearXNGSearch) Search(ctx context.Context, q SearchQuery) (SearchOutcome, error) {
	if strings.TrimSpace(s.endpoint) == "" {
		return SearchOutcome{}, fmt.Errorf("web.search: searxng backend requires an endpoint URL")
	}
	if q.MaxResults <= 0 {
		q.MaxResults = 10
	}
	u, err := url.Parse(s.endpoint)
	if err != nil {
		return SearchOutcome{}, fmt.Errorf("web.search: invalid searxng endpoint: %w", err)
	}
	params := url.Values{
		"q":      {searchQuery(q.Query, q.Site)},
		"format": {"json"},
	}
	if f := strings.ToLower(strings.TrimSpace(q.Freshness)); f != "" {
		switch f {
		case "day", "week", "month", "year":
			params.Set("time_range", f)
		default:
			return SearchOutcome{}, fmt.Errorf("web.search: unknown freshness %q (want day, week, month or year)", q.Freshness)
		}
	}
	u.RawQuery = params.Encode()
	cli, err := searchHTTPClient(ctx, "web_search", s.endpoint, s.timeout)
	if err != nil {
		return SearchOutcome{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if req != nil {
		req.Header.Set("Accept", "application/json")
	}
	if err != nil {
		return SearchOutcome{}, fmt.Errorf("web.search: build request: %w", err)
	}
	resp, err := cli.Do(req)
	if err != nil {
		return SearchOutcome{
			Status: SearchBackendError,
			Note:   fmt.Sprintf("search backend unreachable: %v", err),
		}, nil
	}
	body, fail, ok := readSearchBody(resp, s.maxBytes)
	if !ok {
		return fail, nil
	}
	var parsed searxngResponse
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		return SearchOutcome{
			Status: SearchParseError,
			Note: fmt.Sprintf("the searxng instance answered but not with the JSON API (%v); "+
				"check that the endpoint points at %s and the instance allows format=json", err, s.endpoint),
			Results: []searchItem{},
		}, nil
	}
	if len(parsed.Results) == 0 {
		return SearchOutcome{
			Status:  SearchEmpty,
			Note:    "the searxng instance answered and reported no matches for this query",
			Results: []searchItem{},
		}, nil
	}
	out := make([]searchItem, 0, len(parsed.Results))
	for _, r := range parsed.Results {
		if len(out) >= q.MaxResults {
			break
		}
		out = append(out, searchItem{Title: r.Title, URL: r.URL, Snippet: r.Content})
	}
	return SearchOutcome{Status: SearchOK, Results: out}, nil
}

// freshnessCodes 把工具层的词表映射到 DuckDuckGo 的 df 参数。
//
// 工具收词而不是透传 df：猜「1d」或「past_week」的调用方会拿到一个看起来过滤
// 过、实际没过滤的搜索，未知值在下面被拒绝而不是被丢弃。
var freshnessCodes = map[string]string{
	"day": "d", "week": "w", "month": "m", "year": "y",
}

// searchQuery 把 site 限定折进查询串。
//
// site: 是查询操作符不是表单字段——两个后端都没有独立的域名参数——所以这个
// 限定只能随 q 走。已经写了 "site:" 的调用方保留自己的写法：再加一个会一条
// 结果都搜不到。
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

var (
	// ddgResultRe 匹配一个结果锚：class="result__a"、一个 href、以及承载
	// 标题的内层标记。
	ddgResultRe = regexp.MustCompile(`(?is)<a[^>]*class="result__a"[^>]*href="([^"]*)"[^>]*>(.*?)</a>`)
	// ddgSnippetRe 匹配跟在结果后面的描述锚。
	ddgSnippetRe = regexp.MustCompile(`(?is)<a[^>]*class="result__snippet"[^>]*>(.*?)</a>`)
	ddgTagRe     = regexp.MustCompile(`(?s)<[^>]*>`)
	ddgSpaceRe   = regexp.MustCompile(`\s+`)
)

// parseDuckDuckGoHTML 从 DuckDuckGo HTML 结果页里抽出 title、URL 与 snippet。
//
// 它是纯函数，因此可以对着 fixture 断言。真打线上端点的测试测的是 DuckDuckGo
// 的可用性，而且更糟——在它改版那天照样通过，因为工具把失败的搜索降级成空
// 结果集而不是错误。空列表与「没有命中」不可区分，这正是上一个解析器坏掉却
// 无人察觉的原因：它把整行 <a> 存成标题、URL 硬编码为 ""、从不给 Snippet 赋
// 值、还指着一个已经变成反爬页的端点。每次搜索都返回空列表，而空列表看起来
// 就像「什么都没搜到」。
//
// 注意：本函数只负责「解析出什么」。0 条命中时「搜不到」还是「读不出」的分流
// 在 DuckDuckGoSearch.Search 里（ddgNoResultsMarkers），这里没有页面级上下文。
//
// snippet 按顺序配对而不是按包含关系：标记把每个 snippet 嵌在与其锚相同的
// 结果块里，但用正则匹配块是比顺序更脆的做法，而 DuckDuckGo 严格交错地产出
// 它们。没有 snippet 的结果就不带——配对绝不能把后面每个 snippet 都上移一格。
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
		// 属于这个锚的 snippet 是起点在它之后、下一个锚开始之前的第一个。
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

// ddgResolveURL 解开 DuckDuckGo 的 /l/?uddg= 重定向，取真实目的地。直接给的
// href 原样返回，于是「改版丢了重定向」降级为可用的 URL 而不是空。
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

// ddgText 剥标签、解实体、折叠空白。DuckDuckGo 给匹配词包的 <b> 高亮标记就是
// 标题不能直接用内层原始标记的原因。
func ddgText(s string) string {
	s = ddgTagRe.ReplaceAllString(s, "")
	s = htmlpkg.UnescapeString(s)
	return strings.TrimSpace(ddgSpaceRe.ReplaceAllString(s, " "))
}

