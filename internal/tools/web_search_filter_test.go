package tools

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// captureSearchForm stands in for the search endpoint and records the form the
// tool posted.
func captureSearchForm(t *testing.T, page string) (*httptest.Server, func() map[string][]string) {
	t.Helper()
	var got map[string][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// ParseForm BEFORE anything reads the body — it consumes r.Body, so
		// draining it first leaves the form empty and every assertion below
		// fails for a reason that has nothing to do with the tool.
		_ = r.ParseForm()
		got = r.Form
		_, _ = io.WriteString(w, page)
	}))
	t.Cleanup(srv.Close)
	return srv, func() map[string][]string { return got }
}

const ddgPage = `<div class="result">
<a class="result__a" href="https://go.dev/doc/">The Go Docs</a>
<a class="result__snippet">Everything about Go.</a>
</div>`

// TestWebSearchReturnsTitleSnippetAndURL is the first acceptance clause,
// asserted against a fixture rather than the live endpoint.
//
// Hitting DuckDuckGo for real would test its uptime, and — worse — would keep
// passing on the day its markup changed, because a failed search degrades to
// an empty result set rather than an error. An empty list is indistinguishable
// from "nothing matched", which is exactly how a previous parser stayed broken
// unnoticed.
//
// ledger: B3/T11#1 返回标题/摘要/URL
func TestWebSearchReturnsTitleSnippetAndURL(t *testing.T) {
	srv, _ := captureSearchForm(t, ddgPage)
	ctx := WithNetworkPolicy(WithProfile(context.Background(), profileWithWebTool()), allowAllPolicy())
	w := NewWebTools(1<<20, 5*time.Second)
	setSearchEndpoint(w, srv.URL)

	out, err := w.Search.InvokableRun(ctx, `{"query":"go docs"}`)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	for _, want := range []string{"The Go Docs", "https://go.dev/doc/", "Everything about Go."} {
		if !strings.Contains(out, want) {
			t.Errorf("result is missing %q: %s", want, out)
		}
	}
}

// TestWebSearchAppliesSiteAndFreshnessFilters is the second clause.
//
// Both filters have to reach the wire, and they reach it differently: site: is
// a query OPERATOR folded into q (the endpoint has no domain field), while
// freshness is the df form field. A filter that is accepted and dropped is
// worse than one that is rejected — the caller gets unfiltered results that
// look filtered.
//
// ledger: B3/T11#2 域名/时间过滤生效
func TestWebSearchAppliesSiteAndFreshnessFilters(t *testing.T) {
	srv, form := captureSearchForm(t, ddgPage)
	ctx := WithNetworkPolicy(WithProfile(context.Background(), profileWithWebTool()), allowAllPolicy())
	w := NewWebTools(1<<20, 5*time.Second)
	setSearchEndpoint(w, srv.URL)

	if _, err := w.Search.InvokableRun(ctx,
		`{"query":"generics","site":"go.dev","freshness":"week"}`); err != nil {
		t.Fatalf("search: %v", err)
	}
	posted := form()
	if q := strings.Join(posted["q"], ""); !strings.Contains(q, "site:go.dev") {
		t.Errorf("the site filter did not reach the query: q=%q", q)
	}
	if df := strings.Join(posted["df"], ""); df != "w" {
		t.Errorf("freshness=week sent df=%q, want w", df)
	}

	// An unknown value is rejected, not silently dropped. The tool layer
	// packages a tool error as result TEXT rather than a Go error (so the
	// model can read and retry), which is why this checks the output.
	out, err := w.Search.InvokableRun(ctx, `{"query":"generics","freshness":"past_week"}`)
	if err == nil && !strings.Contains(out, "unknown freshness") {
		t.Errorf("an unrecognised freshness was accepted (%s); the caller would get "+
			"unfiltered results that look filtered", out)
	}

	// A caller who already wrote site: keeps theirs — two site: operators
	// match nothing at all.
	srv2, form2 := captureSearchForm(t, ddgPage)
	setSearchEndpoint(w, srv2.URL)
	if _, err := w.Search.InvokableRun(ctx,
		`{"query":"generics site:pkg.go.dev","site":"go.dev"}`); err != nil {
		t.Fatalf("search: %v", err)
	}
	if q := strings.Join(form2()["q"], ""); strings.Count(strings.ToLower(q), "site:") != 1 {
		t.Errorf("two site: operators in one query returns nothing: q=%q", q)
	}
}

// TestWebSearchRedirectStaysUnderPolicy is the third clause.
//
// The search client has no CheckRedirect of its own (web_fetch does); the
// guarantee comes from PolicyDialer, which authorises the host at CONNECT
// time — so a redirect to a denied host cannot be followed because the dial
// for it never succeeds.
//
// ledger: B3/T11#3 重定向受策略约束
func TestWebSearchRedirectStaysUnderPolicy(t *testing.T) {
	denied := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, ddgPage)
	}))
	t.Cleanup(denied.Close)
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, denied.URL+"/results", http.StatusFound)
	}))
	t.Cleanup(redirector.Close)

	ctx := WithNetworkPolicy(WithProfile(context.Background(), profileWithWebTool()), denyAllPolicy())
	w := NewWebTools(1<<20, 5*time.Second)
	setSearchEndpoint(w, redirector.URL)

	out, err := w.Search.InvokableRun(ctx, `{"query":"anything"}`)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if strings.Contains(out, "The Go Docs") {
		t.Error("the redirect target was fetched under a deny-all policy")
	}
}
