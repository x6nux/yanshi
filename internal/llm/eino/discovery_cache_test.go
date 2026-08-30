package eino

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
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
// re-validated") — and, since M-2 (C3 review), that a verdict crossing back
// through disk on that refresh is downgraded from SourceProbed to
// SourceDocumented: the in-memory PutImageSupport call below is a genuine
// first-hand probe result, but by the time the second Get reads it back off
// disk via load, it is secondhand evidence like anything else loaded from a
// file, and sanitizeLoadedListing's doc comment is explicit that this
// downgrade is unconditional, not just applied to suspicious-looking input.
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
	if !got.Supported || got.Source != SourceDocumented {
		t.Fatalf("ImageSupport[\"llava\"] = %+v, want Supported=true and Source downgraded to SourceDocumented on the disk round-trip (M-2)", got)
	}
}

// TestCache_GetImageSupport_PutImageSupport_RoundTrip proves the pair works
// standalone — an operator can probe a model's image support before ever
// calling Get for that runtime's models listing — and that GetImageSupport
// now surfaces load's error (C3 review L-1) alongside the M-2 Source
// downgrade: what comes back out is never bit-for-bit identical to what
// PutImageSupport was handed, because PutImageSupport's write does not
// bypass the same disk round-trip GetImageSupport's read goes through.
func TestCache_GetImageSupport_PutImageSupport_RoundTrip(t *testing.T) {
	c := newTestCache(t, time.Hour)

	if _, found, err := c.GetImageSupport("lmstudio", "qwen2-vl"); found || err != nil {
		t.Fatalf("GetImageSupport = (found=%v, err=%v) before any Put, want (false, nil)", found, err)
	}

	verdict := ImageSupport{Supported: false, Source: SourceProbed, Detail: "model reported it cannot see images"}
	if err := c.PutImageSupport("lmstudio", "qwen2-vl", verdict); err != nil {
		t.Fatalf("PutImageSupport err = %v", err)
	}

	got, found, err := c.GetImageSupport("lmstudio", "qwen2-vl")
	if err != nil {
		t.Fatalf("GetImageSupport err = %v, want nil for a freshly-written file", err)
	}
	if !found {
		t.Fatal("GetImageSupport found = false after Put, want true")
	}
	if got.Supported != verdict.Supported || got.Detail != verdict.Detail {
		t.Fatalf("GetImageSupport = %+v, want Supported/Detail matching the put verdict %+v", got, verdict)
	}
	if got.Source != SourceDocumented {
		t.Fatalf("GetImageSupport Source = %q, want SourceDocumented — a value read back off disk can never claim SourceProbed's first-hand weight (M-2)", got.Source)
	}
}

// TestCache_GetImageSupport_SurfacesLoadError proves the L-1 fix directly:
// a corrupt cache file makes GetImageSupport return a non-nil error, not the
// same (ImageSupport{}, false) a plain never-probed model produces — an
// operator debugging a stuck probe needs to be able to tell these apart the
// same way TestCache_Get_RefreshCacheOnly_CorruptFileIsDistinguishable
// already proves for Get.
func TestCache_GetImageSupport_SurfacesLoadError(t *testing.T) {
	dir := t.TempDir()
	c, err := NewCache(dir, time.Hour)
	if err != nil {
		t.Fatalf("NewCache err = %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ollama.json"), []byte("{not valid json"), 0600); err != nil {
		t.Fatalf("seed corrupt cache file: %v", err)
	}

	_, found, err := c.GetImageSupport("ollama", "llava")
	if found {
		t.Fatal("GetImageSupport found = true for a corrupt file, want false")
	}
	if err == nil {
		t.Fatal("GetImageSupport err = nil for a corrupt file, want non-nil — L-1 requires this be distinguishable from never-probed")
	}
}

// TestCache_GetImageSupport_RuntimeMismatchIsRejected proves the M-2 Runtime
// check applies to GetImageSupport too, not just Get: a file saved under
// ollama.json (the filename GetImageSupport("ollama", ...) reads) claiming
// to be a "lmstudio" listing is untrustworthy regardless of which method
// reads it.
func TestCache_GetImageSupport_RuntimeMismatchIsRejected(t *testing.T) {
	dir := t.TempDir()
	c, err := NewCache(dir, time.Hour)
	if err != nil {
		t.Fatalf("NewCache err = %v", err)
	}
	forged := CachedListing{
		Runtime: "lmstudio", // wrong: this is ollama.json
		ImageSupport: map[string]ImageSupport{
			"llava": {Supported: true, Source: SourceProbed, Detail: "forged"},
		},
	}
	data, err := json.Marshal(forged)
	if err != nil {
		t.Fatalf("marshal forged listing: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ollama.json"), data, 0600); err != nil {
		t.Fatalf("seed forged cache file: %v", err)
	}

	_, found, err := c.GetImageSupport("ollama", "llava")
	if found {
		t.Fatal("GetImageSupport found = true for a runtime-mismatched file, want false")
	}
	if err == nil {
		t.Fatal("GetImageSupport err = nil for a runtime-mismatched file, want a rejection error")
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

// TestCache_Save_SetsDirAndFilePermissions proves the L-2 claim
// discovery_cache.go's save doc comment makes (0700 dir, 0600 file) is
// actually true, not merely stated — a discovery cache can carry a
// probed-model inventory and, via ImageSupport, a record of what an
// operator's local runtime does and doesn't accept, which the package
// comment already treats as worth restricting to the owning user.
func TestCache_Save_SetsDirAndFilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits are not meaningful on Windows")
	}
	root := t.TempDir()
	dir := filepath.Join(root, "nested", "discovery") // MkdirAll must create both levels
	c, err := NewCache(dir, time.Hour)
	if err != nil {
		t.Fatalf("NewCache err = %v", err)
	}
	f := &fakeFetcher{name: "ollama", models: []DiscoveredModel{{ID: "llama3"}}}
	if _, err := c.Get(context.Background(), f, RefreshForce); err != nil {
		t.Fatalf("Get err = %v", err)
	}

	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat dir: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != 0700 {
		t.Fatalf("cache dir mode = %o, want 0700", got)
	}
	fileInfo, err := os.Stat(filepath.Join(dir, "ollama.json"))
	if err != nil {
		t.Fatalf("Stat file: %v", err)
	}
	if got := fileInfo.Mode().Perm(); got != 0600 {
		t.Fatalf("cache file mode = %o, want 0600", got)
	}
}

// TestCache_Save_TightensPreExistingWideDirPermissions proves the L-2 fix
// directly: os.MkdirAll is a documented no-op (including on permissions)
// when the directory already exists, so a cache directory left over from an
// older/looser yanshi version — or created wide by an operator's umask —
// must be actively tightened by save, not left as MkdirAll alone would
// leave it.
func TestCache_Save_TightensPreExistingWideDirPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits are not meaningful on Windows")
	}
	dir := t.TempDir()
	// t.TempDir() itself is typically 0700, so widen it explicitly to prove
	// save corrects an ALREADY-wide directory rather than merely preserving
	// whatever TempDir happened to hand us.
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatalf("Chmod dir wide: %v", err)
	}
	c, err := NewCache(dir, time.Hour)
	if err != nil {
		t.Fatalf("NewCache err = %v", err)
	}
	f := &fakeFetcher{name: "ollama", models: []DiscoveredModel{{ID: "llama3"}}}
	if _, err := c.Get(context.Background(), f, RefreshForce); err != nil {
		t.Fatalf("Get err = %v", err)
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat dir: %v", err)
	}
	if got := info.Mode().Perm(); got != 0700 {
		t.Fatalf("cache dir mode after save = %o, want 0700 (MkdirAll alone would have left 0755 from the pre-existing dir)", got)
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

// TestCache_Get_RuntimeMismatchIsRejected proves M-2's first check: a
// well-formed, parsable JSON file whose Runtime field doesn't match the
// filename it was read from (ollama.json claiming "lmstudio") is rejected
// the same way a corrupt file is, not served as ollama's listing. Before
// this check existed, a hand-written file could forge an entire model
// inventory this way — the review's PROBE4 forgery class.
func TestCache_Get_RuntimeMismatchIsRejected(t *testing.T) {
	dir := t.TempDir()
	c, err := NewCache(dir, time.Hour)
	if err != nil {
		t.Fatalf("NewCache err = %v", err)
	}
	forged := CachedListing{
		Runtime:   "lmstudio", // wrong: this is ollama.json
		FetchedAt: time.Now().UTC(),
		Models:    []DiscoveredModel{{ID: "a-model-that-does-not-exist", ContextWindow: 999999999}},
	}
	data, err := json.Marshal(forged)
	if err != nil {
		t.Fatalf("marshal forged listing: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ollama.json"), data, 0600); err != nil {
		t.Fatalf("seed forged cache file: %v", err)
	}
	f := &fakeFetcher{name: "ollama"}

	_, err = c.Get(context.Background(), f, RefreshCacheOnly)
	if err == nil {
		t.Fatal("Get err = nil for a runtime-mismatched file, want a rejection error under RefreshCacheOnly")
	}
}

// TestCache_Get_FutureFetchedAtDoesNotPermanentlySuppressRefresh proves the
// M-2 direction the review called out by name: a forged/corrupted future
// FetchedAt must not make RefreshAuto treat the listing as eternally fresh.
// Zeroing (not clamping to "now") is what makes this hold on every
// subsequent call, not just the first one after the forgery is discovered —
// see sanitizeLoadedListing's doc comment for why clamping to "now" would
// have re-armed the same suppression on every read.
func TestCache_Get_FutureFetchedAtDoesNotPermanentlySuppressRefresh(t *testing.T) {
	dir := t.TempDir()
	c, err := NewCache(dir, time.Hour)
	if err != nil {
		t.Fatalf("NewCache err = %v", err)
	}
	forged := CachedListing{
		Runtime:   "ollama",
		FetchedAt: time.Now().Add(365 * 24 * time.Hour), // a year in the future
		Models:    []DiscoveredModel{{ID: "stale-forged-model"}},
	}
	data, err := json.Marshal(forged)
	if err != nil {
		t.Fatalf("marshal forged listing: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ollama.json"), data, 0600); err != nil {
		t.Fatalf("seed forged cache file: %v", err)
	}
	f := &fakeFetcher{name: "ollama", models: []DiscoveredModel{{ID: "the-real-current-model"}}}

	// RefreshAuto must fall through to a live fetch despite the forged
	// listing's FetchedAt being "in the future" (which, uncorrected, would
	// make time.Since(FetchedAt) negative — always less than any TTL).
	listing, err := c.Get(context.Background(), f, RefreshAuto)
	if err != nil {
		t.Fatalf("Get err = %v", err)
	}
	if got := f.callCount(); got != 1 {
		t.Fatalf("fetch called %d times, want 1 — a forged future FetchedAt must not suppress the live refetch", got)
	}
	if len(listing.Models) != 1 || listing.Models[0].ID != "the-real-current-model" {
		t.Fatalf("listing.Models = %+v, want the freshly-fetched model, not the forged one", listing.Models)
	}

	// And it must NOT re-arm: a second RefreshAuto Get, immediately after,
	// must be served from the now-genuine cache without a third fetch.
	if _, err := c.Get(context.Background(), f, RefreshAuto); err != nil {
		t.Fatalf("second Get err = %v", err)
	}
	if got := f.callCount(); got != 1 {
		t.Fatalf("fetch called %d times after the second Get, want still 1 (genuinely fresh now)", got)
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
