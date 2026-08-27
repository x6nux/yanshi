package eino

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestModelsEndpoint is the M9 URL-derivation table. Getting this wrong yields
// a 404 that is indistinguishable from "this provider has no listing endpoint",
// which is exactly the outcome that must never be reported as a config error.
func TestModelsEndpoint(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://api.openai.com/v1", "https://api.openai.com/v1/models"},
		{"https://api.openai.com/v1/", "https://api.openai.com/v1/models"},
		{"https://api.example.com", "https://api.example.com/v1/models"},
		{"https://gw.example.com/openai/v1/proxy", "https://gw.example.com/openai/v1/proxy/models"},
		{"http://localhost:11434", "http://localhost:11434/v1/models"},
		{"  https://api.example.com/v1  ", "https://api.example.com/v1/models"},
		{"", ""},
		{"   ", ""},
	}
	for _, tc := range cases {
		if got := modelsEndpoint(tc.in); got != tc.want {
			t.Errorf("modelsEndpoint(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// recordingTransport records every request URL and fails the round trip, so a
// test can assert that NO request was attempted without depending on whether
// the machine running it has network access.
type recordingTransport struct{ urls []string }

func (r *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.urls = append(r.urls, req.URL.String())
	return nil, errors.New("recordingTransport: no network in tests")
}

// TestFetchModelCatalog covers the success path and every degradation. All the
// failures are equal here: each returns an error the caller treats as "skip",
// never as "the configuration is wrong".
func TestFetchModelCatalog(t *testing.T) {
	t.Run("parses the OpenAI listing shape", func(t *testing.T) {
		var gotAuth string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("Authorization")
			if r.URL.Path != "/v1/models" {
				t.Errorf("path = %q, want /v1/models", r.URL.Path)
			}
			json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
				{"id": "gpt-4o", "object": "model"},
				{"id": "gpt-4o-mini"},
				{"id": ""}, // skipped: an id-less row names nothing
			}})
		}))
		defer srv.Close()
		got, err := FetchModelCatalog(context.Background(), srv.Client(), srv.URL, "sk-test")
		if err != nil {
			t.Fatalf("FetchModelCatalog: %v", err)
		}
		if want := []string{"gpt-4o", "gpt-4o-mini"}; !reflect.DeepEqual(got, want) {
			t.Errorf("catalog = %v, want %v", got, want)
		}
		if gotAuth != "Bearer sk-test" {
			t.Errorf("Authorization = %q", gotAuth)
		}
	})

	t.Run("an empty base_url does not probe a default host", func(t *testing.T) {
		// The assertion has to be "no request was ATTEMPTED", not merely "an
		// error came back". A mutation probe that replaced the guard with a
		// fallback to api.openai.com left this test green, because an offline
		// or firewalled test machine turns the fallback into a transport error
		// that looks identical to the refusal. The recording transport is what
		// makes the two distinguishable — and the behaviour being protected is
		// that the API key must not be sent to a host the operator never named.
		rt := &recordingTransport{}
		client := &http.Client{Transport: rt}
		_, err := FetchModelCatalog(context.Background(), client, "", "sk-secret")
		if err == nil {
			t.Fatal("want an error rather than a probe of a default host")
		}
		if len(rt.urls) != 0 {
			t.Errorf("an HTTP request was attempted with no base_url: %v", rt.urls)
		}
		if strings.Contains(err.Error(), "sk-secret") {
			t.Error("the API key leaked into the error")
		}
	})

	t.Run("404 degrades to an error, never a config verdict", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer srv.Close()
		if _, err := FetchModelCatalog(context.Background(), srv.Client(), srv.URL, ""); err == nil {
			t.Fatal("want an error for a 404 listing")
		}
	})

	t.Run("unparsable body", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Write([]byte("<html>not json</html>"))
		}))
		defer srv.Close()
		if _, err := FetchModelCatalog(context.Background(), srv.Client(), srv.URL, ""); err == nil {
			t.Fatal("want an error for an unparsable listing")
		}
	})

	t.Run("empty listing", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Write([]byte(`{"data":[]}`))
		}))
		defer srv.Close()
		if _, err := FetchModelCatalog(context.Background(), srv.Client(), srv.URL, ""); err == nil {
			t.Fatal("want an error when nothing was advertised")
		}
	})

	t.Run("a cancelled context does not hang", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(2 * time.Second)
		}))
		defer srv.Close()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := FetchModelCatalog(ctx, srv.Client(), srv.URL, ""); err == nil {
			t.Fatal("want an error for a cancelled fetch")
		}
	})
}

// TestCheckModelName is the M9 matching + suggestion table. The suggestion
// ranking is the part that matters: the real mistake is almost never a
// single-character typo but a missing or extra qualifier.
func TestCheckModelName(t *testing.T) {
	catalog := []string{
		"gpt-4o-2026-05-13", "gpt-4o-mini", "o3-pro", "gpt-5",
		"anthropic/claude-sonnet-5", "anthropic/claude-opus-4-8",
	}
	cases := []struct {
		name      string
		model     string
		catalog   []string
		wantFound bool
		wantFirst string
	}{
		{name: "exact match", model: "o3-pro", catalog: catalog, wantFound: true},
		{
			name: "case-insensitive match", model: "O3-Pro", catalog: catalog, wantFound: true,
		},
		{
			name:      "a missing date qualifier suggests the dated id",
			model:     "gpt-4o",
			catalog:   catalog,
			wantFirst: "gpt-4o-mini", // shortest containing candidate
		},
		{
			name:      "a missing routing prefix suggests the prefixed id",
			model:     "claude-sonnet-5",
			catalog:   catalog,
			wantFirst: "anthropic/claude-sonnet-5",
		},
		{
			// The discriminating row for the containment rule. "gpt-4" is 1
			// edit from "gpt-5" and 7 from "gpt-4o-mini", so pure Levenshtein
			// suggests the WRONG family. A mutation probe that removed the
			// containment bonus left the rest of this table green — none of
			// the other rows had a near-miss non-container to be fooled by.
			name:      "containment beats a closer non-container",
			model:     "gpt-4",
			catalog:   catalog,
			wantFirst: "gpt-4o-mini",
		},
		{
			name:      "a typo falls back to edit distance",
			model:     "o3-prq",
			catalog:   catalog,
			wantFirst: "o3-pro",
		},
		{name: "empty model", model: "", catalog: catalog},
		{name: "empty catalog", model: "gpt-4o", catalog: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			found, suggestions := CheckModelName(tc.model, tc.catalog)
			if found != tc.wantFound {
				t.Fatalf("found = %v, want %v", found, tc.wantFound)
			}
			if found {
				if suggestions != nil {
					t.Errorf("a found model got suggestions: %v", suggestions)
				}
				return
			}
			if tc.wantFirst == "" {
				if len(suggestions) != 0 {
					t.Errorf("suggestions = %v, want none", suggestions)
				}
				return
			}
			if len(suggestions) == 0 || suggestions[0] != tc.wantFirst {
				t.Errorf("suggestions = %v, want %q first", suggestions, tc.wantFirst)
			}
			if len(suggestions) > maxSuggestions {
				t.Errorf("got %d suggestions, cap is %d", len(suggestions), maxSuggestions)
			}
		})
	}
}

// TestNearestModelsIsCapped pins the log-line bound: a gateway advertising
// hundreds of models must not turn one misspelling into a screenful.
func TestNearestModelsIsCapped(t *testing.T) {
	catalog := make([]string, 0, 100)
	for i := 0; i < 100; i++ {
		catalog = append(catalog, "model-"+strings.Repeat("x", i%7))
	}
	got := nearestModels("model-y", catalog)
	if len(got) > maxSuggestions {
		t.Errorf("got %d suggestions, want at most %d", len(got), maxSuggestions)
	}
}

// TestLevenshtein pins the distance function the fallback ranking relies on.
func TestLevenshtein(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"", "abc", 3},
		{"abc", "", 3},
		{"abc", "abc", 0},
		{"abc", "abd", 1},
		{"kitten", "sitting", 3},
		{"gpt-4o", "gpt-4o-mini", 5},
	}
	for _, tc := range cases {
		if got := levenshtein(tc.a, tc.b); got != tc.want {
			t.Errorf("levenshtein(%q,%q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

// TestPreflightModels drives the whole preflight over a mix of providers, and
// asserts the property the feature stands on: it NEVER fails, and a provider
// with no listing endpoint yields Skipped rather than a not-found verdict.
func TestPreflightModels(t *testing.T) {
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"data":[{"id":"gpt-4o-2026-05-13"},{"id":"o3-pro"}]}`))
	}))
	defer good.Close()
	none := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer none.Close()

	results := PreflightModels(context.Background(), good.Client(), []ProviderProbe{
		{Name: "ok", Model: "o3-pro", BaseURL: good.URL},
		{Name: "typo", Model: "gpt-4o", BaseURL: good.URL},
		{Name: "no-listing", Model: "whatever", BaseURL: none.URL},
		{Name: "no-url", Model: "whatever"},
	})
	if len(results) != 4 {
		t.Fatalf("got %d results, want 4", len(results))
	}
	if !results[0].Found || results[0].Skipped {
		t.Errorf("provider ok: %+v, want Found", results[0])
	}
	if results[1].Found || results[1].Skipped {
		t.Errorf("provider typo: %+v, want a not-found verdict", results[1])
	}
	if len(results[1].Suggestions) == 0 {
		t.Error("provider typo got no suggestions; the warning would be unactionable")
	}
	for _, i := range []int{2, 3} {
		if !results[i].Skipped {
			t.Errorf("provider %s: %+v, want Skipped (no listing endpoint is normal)",
				results[i].Provider, results[i])
		}
		if results[i].SkipReason == "" {
			t.Errorf("provider %s skipped with no reason", results[i].Provider)
		}
	}
	// LogPreflight must not panic on any verdict shape.
	LogPreflight(results)
	LogPreflight(nil)
}
