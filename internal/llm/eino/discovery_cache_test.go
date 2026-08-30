package eino

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakeFetcher is a Fetcher test double that counts calls and returns
// caller-controlled models/etag/err — no httptest server needed here since
// Cache never does its own HTTP; that's OllamaClient's/LMStudioClient's job,
// already covered by their own test files.
type fakeFetcher struct {
	name   string
	mu     sync.Mutex
	calls  int
	models []DiscoveredModel
	etag   string
	err    error
}

func (f *fakeFetcher) Name() string { return f.name }

func (f *fakeFetcher) FetchModels(ctx context.Context) ([]DiscoveredModel, string, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	return f.models, f.etag, f.err
}

func (f *fakeFetcher) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func newTestCache(t *testing.T, ttl time.Duration) *Cache {
	t.Helper()
	c, err := NewCache(t.TempDir(), ttl)
	if err != nil {
		t.Fatalf("NewCache err = %v", err)
	}
	return c
}

// TestCache_Get_RefreshAuto_ServesWithinTTL proves RefreshAuto's whole
// point: a second Get within the TTL must not hit the network again.
func TestCache_Get_RefreshAuto_ServesWithinTTL(t *testing.T) {
	c := newTestCache(t, time.Hour)
	f := &fakeFetcher{name: "ollama", models: []DiscoveredModel{{ID: "llama3"}}}

	if _, err := c.Get(context.Background(), f, RefreshAuto); err != nil {
		t.Fatalf("first Get err = %v", err)
	}
	if _, err := c.Get(context.Background(), f, RefreshAuto); err != nil {
		t.Fatalf("second Get err = %v", err)
	}
	if got := f.callCount(); got != 1 {
		t.Fatalf("fetch called %d times, want 1 (second Get should be served from cache)", got)
	}
}

// TestCache_Get_RefreshAuto_RefetchesPastTTL proves the other half: once the
// TTL elapses, RefreshAuto fetches live again.
func TestCache_Get_RefreshAuto_RefetchesPastTTL(t *testing.T) {
	c := newTestCache(t, 5*time.Millisecond)
	f := &fakeFetcher{name: "ollama", models: []DiscoveredModel{{ID: "llama3"}}}

	if _, err := c.Get(context.Background(), f, RefreshAuto); err != nil {
		t.Fatalf("first Get err = %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	if _, err := c.Get(context.Background(), f, RefreshAuto); err != nil {
		t.Fatalf("second Get err = %v", err)
	}
	if got := f.callCount(); got != 2 {
		t.Fatalf("fetch called %d times, want 2 (TTL should have expired)", got)
	}
}

// TestCache_Get_RefreshForce_AlwaysFetches proves RefreshForce ignores the
// TTL entirely, even immediately after a Get that just populated the cache.
func TestCache_Get_RefreshForce_AlwaysFetches(t *testing.T) {
	c := newTestCache(t, time.Hour)
	f := &fakeFetcher{name: "ollama", models: []DiscoveredModel{{ID: "llama3"}}}

	if _, err := c.Get(context.Background(), f, RefreshForce); err != nil {
		t.Fatalf("first Get err = %v", err)
	}
	if _, err := c.Get(context.Background(), f, RefreshForce); err != nil {
		t.Fatalf("second Get err = %v", err)
	}
	if got := f.callCount(); got != 2 {
		t.Fatalf("fetch called %d times, want 2 (RefreshForce must never serve cache)", got)
	}
}

// TestCache_Get_RefreshCacheOnly_NoNetworkEver proves the "offline startup
// uses cache" acceptance bullet's third tier: RefreshCacheOnly never touches
// the Fetcher, whether or not a cache file exists.
func TestCache_Get_RefreshCacheOnly_NoNetworkEver(t *testing.T) {
	c := newTestCache(t, time.Hour)
	f := &fakeFetcher{name: "ollama", models: []DiscoveredModel{{ID: "llama3"}}}

	// No cache file exists yet.
	if _, err := c.Get(context.Background(), f, RefreshCacheOnly); err == nil {
		t.Fatal("Get err = nil, want an error — no cache on disk and RefreshCacheOnly forbids a live fetch")
	}
	if got := f.callCount(); got != 0 {
		t.Fatalf("fetch called %d times, want 0 — RefreshCacheOnly must never call the network", got)
	}

	// Populate via a different policy, then confirm RefreshCacheOnly serves
	// it without an additional fetch.
	if _, err := c.Get(context.Background(), f, RefreshForce); err != nil {
		t.Fatalf("populating Get err = %v", err)
	}
	listing, err := c.Get(context.Background(), f, RefreshCacheOnly)
	if err != nil {
		t.Fatalf("RefreshCacheOnly Get err = %v, want the now-populated cache", err)
	}
	if len(listing.Models) != 1 || listing.Models[0].ID != "llama3" {
		t.Fatalf("listing.Models = %+v, want [{llama3}]", listing.Models)
	}
	if got := f.callCount(); got != 1 {
		t.Fatalf("fetch called %d times, want 1 (only the populating Get, not the RefreshCacheOnly one)", got)
	}
}

// TestCache_Get_StaleFallbackOnFetchError is the "offline startup uses
// cache" bullet's other half: a live fetch that FAILS after a cache already
// exists must serve the stale data (with FetchError populated) rather than
// erroring outright — a daemon that crashed mid-session must not make an
// operator's previously-seen model list disappear.
func TestCache_Get_StaleFallbackOnFetchError(t *testing.T) {
	c := newTestCache(t, time.Hour)
	f := &fakeFetcher{name: "ollama", models: []DiscoveredModel{{ID: "llama3"}}}

	if _, err := c.Get(context.Background(), f, RefreshForce); err != nil {
		t.Fatalf("populating Get err = %v", err)
	}

	f.err = errors.New("daemon crashed mid-session")
	listing, err := c.Get(context.Background(), f, RefreshForce)
	if err != nil {
		t.Fatalf("Get err = %v, want nil — a stale cache exists to fall back to", err)
	}
	if len(listing.Models) != 1 || listing.Models[0].ID != "llama3" {
		t.Fatalf("listing.Models = %+v, want the stale [{llama3}]", listing.Models)
	}
	if listing.FetchError != "daemon crashed mid-session" {
		t.Fatalf("listing.FetchError = %q, want the fetch error recorded", listing.FetchError)
	}
}

// TestCache_Get_ErrorWhenNoCacheToFallBackTo proves the fetch error is NOT
// swallowed when there is nothing stale to serve instead — this is the
// unreachable-vs-empty distinction propagated one layer up: an error from
// Fetcher must remain an error in Cache.Get's return value when there is no
// cache, exactly as ADR-0025 requires.
func TestCache_Get_ErrorWhenNoCacheToFallBackTo(t *testing.T) {
	c := newTestCache(t, time.Hour)
	wantErr := errors.New("connection refused")
	f := &fakeFetcher{name: "ollama", err: wantErr}

	listing, err := c.Get(context.Background(), f, RefreshAuto)
	if err == nil {
		t.Fatalf("Get err = nil, listing = %+v, want the fetch error surfaced (no cache exists yet)", listing)
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want it to wrap/equal %v", err, wantErr)
	}
}

// TestCache_Get_ReachableButEmpty_NotAnError proves Cache.Get preserves the
// reachable-but-empty case through the caching layer: a Fetcher that
// succeeds with zero models must not become an error at the Cache boundary
// either.
func TestCache_Get_ReachableButEmpty_NotAnError(t *testing.T) {
	c := newTestCache(t, time.Hour)
	f := &fakeFetcher{name: "ollama", models: []DiscoveredModel{}}

	listing, err := c.Get(context.Background(), f, RefreshAuto)
	if err != nil {
		t.Fatalf("Get err = %v, want nil for a reachable-but-empty listing", err)
	}
	if len(listing.Models) != 0 {
		t.Fatalf("listing.Models = %+v, want empty", listing.Models)
	}
}

// TestCache_ImageSupport_PreservedAcrossModelsRefresh proves W-C-15 verdicts
// survive a models-listing refresh (per discovery_cache.go's Get doc
// comment: "carried across a models refresh, neither wiped nor
// re-validated").
func TestCache_ImageSupport_PreservedAcrossModelsRefresh(t *testing.T) {
	c := newTestCache(t, time.Hour)
	f := &fakeFetcher{name: "ollama", models: []DiscoveredModel{{ID: "llava"}}}

	if _, err := c.Get(context.Background(), f, RefreshForce); err != nil {
		t.Fatalf("populating Get err = %v", err)
	}
	if err := c.PutImageSupport("ollama", "llava", ImageSupport{Supported: true, Source: SourceProbed, Detail: "saw red"}); err != nil {
		t.Fatalf("PutImageSupport err = %v", err)
	}

	// A second models refresh must not wipe the recorded verdict.
	listing, err := c.Get(context.Background(), f, RefreshForce)
	if err != nil {
		t.Fatalf("second Get err = %v", err)
	}
	got, ok := listing.ImageSupport["llava"]
	if !ok {
		t.Fatal("ImageSupport[\"llava\"] missing after a models refresh, want it preserved")
	}
	if !got.Supported || got.Source != SourceProbed {
		t.Fatalf("ImageSupport[\"llava\"] = %+v, want the probed verdict intact", got)
	}
}

// TestCache_GetImageSupport_PutImageSupport_RoundTrip proves the pair works
// standalone — an operator can probe a model's image support before ever
// calling Get for that runtime's models listing.
func TestCache_GetImageSupport_PutImageSupport_RoundTrip(t *testing.T) {
	c := newTestCache(t, time.Hour)

	if _, found := c.GetImageSupport("lmstudio", "qwen2-vl"); found {
		t.Fatal("GetImageSupport found = true before any Put, want false")
	}

	verdict := ImageSupport{Supported: false, Source: SourceProbed, Detail: "model reported it cannot see images"}
	if err := c.PutImageSupport("lmstudio", "qwen2-vl", verdict); err != nil {
		t.Fatalf("PutImageSupport err = %v", err)
	}

	got, found := c.GetImageSupport("lmstudio", "qwen2-vl")
	if !found {
		t.Fatal("GetImageSupport found = false after Put, want true")
	}
	if got != verdict {
		t.Fatalf("GetImageSupport = %+v, want %+v", got, verdict)
	}
}

// TestCache_Save_NoTmpFileLeak proves a successful save leaves no .tmp.*
// artifact behind — the atomic-write idiom's whole point.
func TestCache_Save_NoTmpFileLeak(t *testing.T) {
	dir := t.TempDir()
	c, err := NewCache(dir, time.Hour)
	if err != nil {
		t.Fatalf("NewCache err = %v", err)
	}
	f := &fakeFetcher{name: "ollama", models: []DiscoveredModel{{ID: "llama3"}}}
	if _, err := c.Get(context.Background(), f, RefreshForce); err != nil {
		t.Fatalf("Get err = %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(dir, "*.tmp.*"))
	if err != nil {
		t.Fatalf("Glob err = %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("leftover temp files after a successful save: %v", matches)
	}
}

// TestContentHashETag_OrderIndependent proves contentHashETag hashes the
// SORTED model set — two fetches returning the same models in different
// order (a server iterating an unordered map internally) must produce the
// same ETag, or every such fetch would look like a spurious "changed"
// signal to a caller comparing ETags.
func TestContentHashETag_OrderIndependent(t *testing.T) {
	a := []DiscoveredModel{{ID: "llama3"}, {ID: "mistral"}, {ID: "phi3"}}
	b := []DiscoveredModel{{ID: "phi3"}, {ID: "llama3"}, {ID: "mistral"}}
	if contentHashETag(a) != contentHashETag(b) {
		t.Fatalf("contentHashETag differs by order: %q vs %q", contentHashETag(a), contentHashETag(b))
	}
}

// TestContentHashETag_ChangesWithContent proves the hash is not a constant
// — a different model set must produce a different ETag, or it would be
// useless as a change signal.
func TestContentHashETag_ChangesWithContent(t *testing.T) {
	a := []DiscoveredModel{{ID: "llama3"}}
	b := []DiscoveredModel{{ID: "llama3"}, {ID: "mistral"}}
	if contentHashETag(a) == contentHashETag(b) {
		t.Fatal("contentHashETag identical for different model sets, want different")
	}
}

// TestCache_Get_RefreshCacheOnly_CorruptFileIsDistinguishable proves load's
// found=false-with-err distinction (a corrupt file vs. simply no file) is
// visible through Get's error under RefreshCacheOnly — an operator debugging
// "why is my offline cache not working" needs "the file is corrupt" not
// collapsed into the same message as "there is no file".
func TestCache_Get_RefreshCacheOnly_CorruptFileIsDistinguishable(t *testing.T) {
	dir := t.TempDir()
	c, err := NewCache(dir, time.Hour)
	if err != nil {
		t.Fatalf("NewCache err = %v", err)
	}
	f := &fakeFetcher{name: "ollama"}
	if err := os.WriteFile(filepath.Join(dir, "ollama.json"), []byte("{not valid json"), 0600); err != nil {
		t.Fatalf("seed corrupt cache file: %v", err)
	}

	_, err = c.Get(context.Background(), f, RefreshCacheOnly)
	if err == nil {
		t.Fatal("Get err = nil, want an error for a corrupt cache file")
	}
	if got := f.callCount(); got != 0 {
		t.Fatalf("fetch called %d times, want 0 — RefreshCacheOnly must never call the network even on a corrupt file", got)
	}
}
