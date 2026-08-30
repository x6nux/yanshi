package eino

// W-C-06: disk cache for local-runtime discovery results.
//
// This is the one piece all four C3 items share: OllamaClient (W-C-03) and
// LMStudioClient (W-C-05) both implement Fetcher, so Cache never needs to
// know which runtime it's caching; W-C-15's probe verdicts ride the same
// per-runtime cache file through a second pair of methods
// (GetImageSupport/PutImageSupport) because a probe targets one model, not
// a whole listing, and has no natural place in Fetcher's shape.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Fetcher is the one shared discovery interface: anything that can list a
// local runtime's currently-available models. Cache depends only on this
// interface, not on OllamaClient or LMStudioClient directly, so adding a
// third local runtime later means implementing Fetcher, not touching Cache.
type Fetcher interface {
	// Name identifies the runtime for the cache filename and error
	// messages ("ollama", "lmstudio").
	Name() string
	// FetchModels lists what the runtime currently has, following the same
	// return-value contract as OllamaClient.ListModels/LMStudioClient.
	// ListModels (ADR-0025): non-nil error means unreachable/unparsable,
	// nil error with a possibly-empty slice means reachable. etag is the
	// runtime's own change token when it has one — always "" today, since
	// neither Ollama's nor LM Studio's REST API sends one; see
	// contentHashETag for what Cache substitutes when etag is empty.
	FetchModels(ctx context.Context) (models []DiscoveredModel, etag string, err error)
}

// RefreshPolicy selects one of the three refresh tiers the W-C-06
// acceptance bullet asks for.
type RefreshPolicy int

const (
	// RefreshAuto serves the cached listing without a network call when it
	// is younger than the Cache's TTL; past the TTL (or with no cache yet)
	// it fetches live. This is the default a caller should use on every
	// normal request.
	RefreshAuto RefreshPolicy = iota
	// RefreshForce always fetches live, ignoring the TTL — for an explicit
	// "refresh now" user action.
	RefreshForce
	// RefreshCacheOnly never touches the network — the "offline startup
	// uses cache" acceptance bullet's tier. It returns whatever is on disk
	// regardless of age, or an error if nothing is on disk yet.
	RefreshCacheOnly
)

// DefaultCacheTTL is how long a listing is served without a live re-fetch
// under RefreshAuto. 5 minutes: long enough that opening a model picker
// twice in a row doesn't hit the network twice, short enough that a model
// pulled a few minutes ago (the exact action an operator takes right before
// checking whether it worked) shows up on the next normal request without
// needing RefreshForce.
const DefaultCacheTTL = 5 * time.Minute

// CachedListing is the on-disk (and in-memory return) shape for one
// runtime's cached discovery state. It is also the unit PutImageSupport/
// GetImageSupport read and write — W-C-15's probe results live in the same
// file as the models listing rather than a separate cache, per the spec's
// "探测结果回写缓存层" note and ADR-0025's "one Cache, one interface" design.
type CachedListing struct {
	Runtime   string    `json:"runtime"`
	FetchedAt time.Time `json:"fetched_at"`
	// ETag is the runtime's own change token when it sent one, otherwise a
	// locally-computed content hash (see contentHashETag) — either way, two
	// listings with the same ETag have the same model set.
	ETag   string            `json:"etag"`
	Models []DiscoveredModel `json:"models"`
	// FetchError carries the last live-fetch attempt's error text when
	// Get had to fall back to serving a stale cache because the runtime
	// was unreachable — see Get's doc comment. Empty on a listing that was
	// itself the result of a successful fetch.
	FetchError string `json:"fetch_error,omitempty"`
	// ImageSupport holds W-C-15 verdicts (probed or documented) keyed by
	// model id.
	ImageSupport map[string]ImageSupport `json:"image_support,omitempty"`
}

// Cache persists Fetcher results to disk, one JSON file per runtime.
type Cache struct {
	dir string
	ttl time.Duration
	// mu serializes this process's reads/writes to a runtime's cache file.
	// It does not protect against a second yanshi process (or `yanshi doctor`
	// run concurrently with the TUI) writing the same file at the same time
	// — the atomic tmp+rename write in save makes a torn write impossible,
	// but a benign lost-update race (last writer wins) between two
	// processes is possible. Cache data is disposable and re-fetchable, so
	// that race is not worth a cross-process file lock.
	mu sync.Mutex
}

// DefaultCacheDir returns os.UserCacheDir()/yanshi/discovery — the same
// os.UserCacheDir() root internal/lockfile uses for its run directory, and
// the same "evictable, not durable user state" semantics: deleting this
// directory is always safe, the next Get simply re-fetches.
func DefaultCacheDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("eino: discovery cache: resolve user cache dir: %w", err)
	}
	return filepath.Join(base, "yanshi", "discovery"), nil
}

// NewCache builds a Cache rooted at dir with the given TTL (used only under
// RefreshAuto). An empty dir resolves DefaultCacheDir(); ttl <= 0 uses
// DefaultCacheTTL.
func NewCache(dir string, ttl time.Duration) (*Cache, error) {
	if dir == "" {
		var err error
		dir, err = DefaultCacheDir()
		if err != nil {
			return nil, err
		}
	}
	if ttl <= 0 {
		ttl = DefaultCacheTTL
	}
	return &Cache{dir: dir, ttl: ttl}, nil
}

func (c *Cache) filePath(runtime string) string {
	return filepath.Join(c.dir, runtime+".json")
}

// Get returns runtime's cached (or freshly fetched, per policy) listing.
//
// Distinguishing "the runtime is unreachable" from "the runtime is
// reachable but empty" (ADR-0025's return-value contract) flows through
// unchanged from Fetcher.FetchModels: Get only wraps that contract with
// caching, it never converts an empty-but-successful listing into an error
// or vice versa.
//
// A live fetch that errors falls back to a stale on-disk cache when one
// exists, recording the error in the returned CachedListing.FetchError
// rather than either silently pretending the stale data is fresh or
// discarding it — this covers BOTH halves of "offline startup uses cache":
// no network at all, and a network that answers but is broken (daemon
// crashed mid-session, model directory corrupted). Only when there is no
// cache to fall back to does Get return the fetch error directly.
func (c *Cache) Get(ctx context.Context, f Fetcher, policy RefreshPolicy) (CachedListing, error) {
	path := c.filePath(f.Name())
	c.mu.Lock()
	defer c.mu.Unlock()

	existing, found, loadErr := c.load(path, f.Name())

	if policy == RefreshCacheOnly {
		if !found {
			if loadErr != nil {
				return CachedListing{}, fmt.Errorf("discovery cache: %s: no usable offline cache: %w", f.Name(), loadErr)
			}
			return CachedListing{}, fmt.Errorf("discovery cache: %s: no cache on disk and RefreshCacheOnly forbids a live fetch", f.Name())
		}
		return existing, nil
	}

	if policy == RefreshAuto && found && time.Since(existing.FetchedAt) < c.ttl {
		return existing, nil
	}

	models, etag, err := f.FetchModels(ctx)
	if err != nil {
		if found {
			stale := existing
			stale.FetchError = err.Error()
			return stale, nil
		}
		return CachedListing{}, err
	}

	listing := CachedListing{
		Runtime:      f.Name(),
		FetchedAt:    time.Now().UTC(),
		ETag:         etag,
		Models:       models,
		ImageSupport: existing.ImageSupport, // carried across a models refresh; a listing refresh says nothing about whether previously-probed models still behave the same, so it is neither wiped nor re-validated here.
	}
	if listing.ETag == "" {
		listing.ETag = contentHashETag(models)
	}
	if err := c.save(path, listing); err != nil {
		return listing, fmt.Errorf("discovery cache: %s: fetched fine but failed to persist: %w", f.Name(), err)
	}
	return listing, nil
}

// GetImageSupport returns the cached W-C-15 verdict for model under
// runtime, and whether one exists at all (never probed/documented yet).
//
// The returned error distinguishes "the cache file is corrupt or its
// Runtime field doesn't match runtime" from "there is no cache file at all,
// which is a normal never-probed-yet state, not a failure" (C3 review L-1)
// — mirroring Get's own found/err distinction (see Cache.Get's doc comment)
// instead of discarding load's error and folding both cases into the same
// (ImageSupport{}, false). A caller that only checks the bool cannot tell a
// hand-edited cache file demanding attention from an unremarkable first
// probe; a caller that also checks the error can.
func (c *Cache) GetImageSupport(runtime, model string) (ImageSupport, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	existing, found, err := c.load(c.filePath(runtime), runtime)
	if !found {
		return ImageSupport{}, false, err
	}
	v, ok := existing.ImageSupport[model]
	return v, ok, nil
}

// PutImageSupport persists a W-C-15 verdict (probed or documented) for
// model under runtime, merging into that runtime's cache file. It does not
// require a models listing to already be cached for that runtime — an
// operator can probe a model's image support before ever calling Get.
func (c *Cache) PutImageSupport(runtime, model string, verdict ImageSupport) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	path := c.filePath(runtime)
	// A missing, corrupt, or runtime-mismatched existing file degrades to
	// starting a fresh listing rather than blocking the write: losing a
	// stale/unreadable/forged cache is not a reason to refuse recording a
	// probe result that just succeeded.
	existing, _, _ := c.load(path, runtime)
	if existing.Runtime == "" {
		existing.Runtime = runtime
	}
	if existing.ImageSupport == nil {
		existing.ImageSupport = make(map[string]ImageSupport)
	}
	existing.ImageSupport[model] = verdict
	return c.save(path, existing)
}

// load reads, parses, and sanitizes expectedRuntime's cache file. found=false
// with err=nil means the file simply does not exist yet (a normal first-run
// state, not an error); found=false with a non-nil err means the file exists
// but could not be read, parsed, or trusted — callers distinguish these
// because RefreshCacheOnly reports the second case as "cache corrupt" rather
// than "no cache", and GetImageSupport now surfaces it too (C3 review L-1)
// instead of discarding it.
//
// A successfully-parsed listing is never returned verbatim: it passes
// through sanitizeLoadedListing first, UNLESS its own Runtime field doesn't
// match expectedRuntime, in which case it is rejected the same way a
// corrupt/unparsable file is (C3 review M-2's first suggested direction) —
// a file saved as ollama.json claiming to be a "lmstudio" listing (or one
// hand-written by an attacker who didn't know to set Runtime at all) is not
// safe to serve as expectedRuntime's cache.
func (c *Cache) load(path, expectedRuntime string) (listing CachedListing, found bool, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return CachedListing{}, false, nil
		}
		return CachedListing{}, false, err
	}
	if err := json.Unmarshal(data, &listing); err != nil {
		return CachedListing{}, false, fmt.Errorf("parse %s: %w", path, err)
	}
	if listing.Runtime != expectedRuntime {
		return CachedListing{}, false, fmt.Errorf("parse %s: runtime mismatch: file claims %q, expected %q", path, listing.Runtime, expectedRuntime)
	}
	return sanitizeLoadedListing(listing), true, nil
}

// sanitizeLoadedListing defends every reader of a disk-loaded CachedListing
// against a hand-written or stale-forged file (C3 review M-2; the Runtime
// check that would otherwise belong here lives in load, one level up,
// because a mismatch must reject the whole file rather than repair it):
//
//  1. A FetchedAt in the future is zeroed rather than clamped to now.
//     Clamping to now would make time.Since(FetchedAt) read ~0 on THIS
//     load, but the on-disk value itself is never rewritten by a read-only
//     load, so the very next load would see the same future timestamp and
//     clamp to a new "now" again — RefreshAuto's staleness check would then
//     read "just fetched" forever, exactly the "future FetchedAt
//     permanently suppresses re-fetching" forgery the review flagged,
//     merely re-armed on every call instead of fixed once. Zeroing makes
//     time.Since(FetchedAt) enormous instead, which reads as maximally
//     stale — RefreshAuto correctly falls through to a live fetch, and only
//     a genuine successful fetch (which sets FetchedAt = time.Now() in Get)
//     clears the zero value. Fail-safe direction: an untrustworthy
//     timestamp becomes "refetch to be sure", never "trust indefinitely".
//  2. Every ImageSupport entry's Source is downgraded from SourceProbed to
//     SourceDocumented. This is the most substantive check (per the
//     review): SourceProbed is supposed to mean "this process sent a real
//     image over the wire THIS SESSION and read the model's answer" — a
//     disk file cannot make that claim on a new process's behalf no matter
//     how it was produced, genuinely or by hand. Downgrading unconditionally
//     on every load (not just when something looks suspicious) means even a
//     GENUINELY probed verdict correctly reads as secondhand evidence the
//     moment it round-trips through disk — a hand-written
//     {"source":"probed","supported":true} for a model that was never
//     probed can no longer claim first-hand weight, because nothing that
//     came from disk can.
func sanitizeLoadedListing(listing CachedListing) CachedListing {
	if listing.FetchedAt.After(time.Now()) {
		listing.FetchedAt = time.Time{}
	}
	if len(listing.ImageSupport) > 0 {
		sanitized := make(map[string]ImageSupport, len(listing.ImageSupport))
		for model, verdict := range listing.ImageSupport {
			if verdict.Source == SourceProbed {
				verdict.Source = SourceDocumented
			}
			sanitized[model] = verdict
		}
		listing.ImageSupport = sanitized
	}
	return listing
}

// save writes listing atomically: encode to a random temp file in the same
// directory, then rename over the real path. os.Rename replaces an
// existing destination on every platform this repo targets (Go's own
// os.Rename doc: "If newpath already exists and is not a directory, Rename
// replaces it") — unlike internal/cli/tui/preferences.go's
// replacePreferencesFileOS or internal/secrets' equivalent, this does not
// need a MoveFileEx+MOVEFILE_WRITE_THROUGH wrapper for extra durability,
// because a discovery cache entry is disposable: if a write is lost to a
// crash or a locked file on Windows, the next Get simply re-fetches from
// the network, which prefs/secrets/vcs cannot do for their data.
func (c *Cache) save(path string, listing CachedListing) error {
	if err := os.MkdirAll(c.dir, 0700); err != nil {
		return err
	}
	// MkdirAll is a no-op — including on permissions — when c.dir already
	// exists (e.g. left over from an older yanshi version that created it
	// with looser bits, or an operator's umask widening it at creation
	// time): it never tightens an existing directory's mode, only sets one
	// on creation. Chmod unconditionally on every save so a cache directory
	// that already exists gets actively tightened, not just correctly
	// created once (C3 review L-2). Best-effort: a chmod failure (e.g. this
	// process doesn't own the directory) is not a reason to abandon an
	// otherwise-successful write — the atomic tmp+rename below still
	// protects the file itself with 0600.
	_ = os.Chmod(c.dir, 0700)
	data, err := json.MarshalIndent(listing, "", "  ")
	if err != nil {
		return err
	}
	suffix, err := randomSuffix()
	if err != nil {
		return err
	}
	tmp := path + ".tmp." + suffix
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// randomSuffix mirrors internal/cli/tui/preferences.go's helper of the same
// name and purpose (a collision-resistant temp-file suffix for an atomic
// write) — reimplemented here rather than imported because internal/cli/tui
// is a UI-layer package and internal/llm/eino has no business depending on
// it for an unrelated two-line helper.
func randomSuffix() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate discovery cache temp suffix: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// contentHashETag substitutes a deterministic local "did the listing
// change" signal for runtimes that send no real ETag — which, per the
// FetchModels doc comment, is both Ollama and LM Studio today. This is a
// DELIBERATE substitution, not an assumption that these daemons implement
// HTTP caching: it never gates whether Get performs a live fetch (there is
// no conditional-GET support to exploit for that), it only lets a caller
// compare two CachedListing.ETag values to tell "nothing changed" from
// "the model set changed" without diffing the full Models slice. Models is
// sorted by ID first so two fetches returning the same set in a different
// order (a server iterating an unordered map internally) hash identically.
func contentHashETag(models []DiscoveredModel) string {
	sorted := make([]DiscoveredModel, len(models))
	copy(sorted, models)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	h := sha256.New()
	enc := json.NewEncoder(h)
	for _, m := range sorted {
		_ = enc.Encode(m) // hash.Hash.Write never errors; DiscoveredModel always marshals.
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}
