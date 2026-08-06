package tools

import "testing"

// ddgFixture is a trimmed but structurally faithful excerpt of what
// https://html.duckduckgo.com/html/ returns: each result is a result__a anchor
// whose href is a /l/?uddg= redirect wrapping the percent-encoded destination,
// followed by a result__snippet anchor. Both contain <b> highlight tags and
// HTML entities, which is why neither can be used verbatim.
const ddgFixture = `
<div class="result results_links results_links_deep web-result ">
  <div class="links_main links_deep result__body">
    <h2 class="result__title">
      <a rel="nofollow" class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fgo.dev%2Fdoc%2Feffective_go&amp;rut=abc">Effective <b>Go</b> &amp; style</a>
    </h2>
    <a class="result__snippet" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fgo.dev%2Fdoc%2Feffective_go">Tips for writing <b>clear</b>, idiomatic Go code &mdash; the canonical guide.</a>
  </div>
</div>
<div class="result results_links results_links_deep web-result ">
  <div class="links_main links_deep result__body">
    <h2 class="result__title">
      <a rel="nofollow" class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fpkg.go.dev%2Fnet%2Fhttp">net/http package</a>
    </h2>
    <a class="result__snippet" href="#">HTTP client and server implementations.</a>
  </div>
</div>
`

// TestParseDuckDuckGoHTML pins every field the old parser got wrong.
//
// It stored the entire raw <a ...> line as the Title, hardcoded URL to "", and
// never assigned Snippet anywhere in the file -- so a successful search
// returned a list of HTML fragments with no addresses and no descriptions. The
// endpoint it queried had also become an anti-bot page, so in practice the
// list was empty and the emptiness looked like "no results found".
//
// Parsing is a pure function precisely so this can be asserted without a
// network round trip: a test that hit the real endpoint would be testing
// DuckDuckGo's uptime, and would have gone green-then-silently-wrong the day
// the markup changed.
func TestParseDuckDuckGoHTML(t *testing.T) {
	got := parseDuckDuckGoHTML(ddgFixture, 10)
	if len(got) != 2 {
		t.Fatalf("want 2 results, got %d: %+v", len(got), got)
	}

	if got[0].Title != "Effective Go & style" {
		t.Errorf("title must be text, with tags stripped and entities decoded: %q", got[0].Title)
	}
	if got[0].URL != "https://go.dev/doc/effective_go" {
		t.Errorf("url must be unwrapped from the uddg redirect: %q", got[0].URL)
	}
	if got[0].Snippet != "Tips for writing clear, idiomatic Go code — the canonical guide." {
		t.Errorf("snippet was never assigned by the old parser: %q", got[0].Snippet)
	}

	if got[1].URL != "https://pkg.go.dev/net/http" {
		t.Errorf("second url: %q", got[1].URL)
	}
	if got[1].Snippet != "HTTP client and server implementations." {
		t.Errorf("second snippet: %q", got[1].Snippet)
	}
}

func TestParseDuckDuckGoHTMLEdges(t *testing.T) {
	t.Run("max results is honoured", func(t *testing.T) {
		if got := parseDuckDuckGoHTML(ddgFixture, 1); len(got) != 1 {
			t.Fatalf("want 1, got %d", len(got))
		}
	})
	t.Run("an anti-bot page yields nothing rather than garbage", func(t *testing.T) {
		if got := parseDuckDuckGoHTML(`<html><body>Please verify you are human</body></html>`, 10); len(got) != 0 {
			t.Fatalf("want 0 results, got %+v", got)
		}
	})
	t.Run("a result without a snippet still carries its url", func(t *testing.T) {
		in := `<a rel="nofollow" class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com">Only a title</a>`
		got := parseDuckDuckGoHTML(in, 10)
		if len(got) != 1 || got[0].URL != "https://example.com" || got[0].Snippet != "" {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("a direct href is passed through unchanged", func(t *testing.T) {
		in := `<a class="result__a" href="https://example.org/page">Direct</a>`
		got := parseDuckDuckGoHTML(in, 10)
		if len(got) != 1 || got[0].URL != "https://example.org/page" {
			t.Fatalf("got %+v", got)
		}
	})
}
