package cli

// review-whole.md M-1: eino.OllamaClient.PullModel, eino.LMStudioClient.
// LoadModel, and eino.RefreshForce had zero production callers — every
// exercise of them was a unit test building its own httptest.Server.
// `yanshi models pull`/`yanshi models preheat` (cmd/yanshi/models.go) are
// the genuine, explicitly user-invoked entry points the review demanded.
//
// These are deliberately NOT folded into `yanshi doctor`: doctorlocalruntimes.
// go's package comment draws a "Probe, never launch" boundary for that
// command — a pull downloads gigabytes and a preheat cold-loads a model into
// memory, both real side effects a diagnostic command must never have.
import (
	"context"
	"fmt"
	"io"

	"github.com/x6nux/yanshi/internal/llm/eino"
)

// ModelsPullOptions configures RunModelsPull.
type ModelsPullOptions struct {
	// BaseURL overrides Ollama's default loopback address; empty uses
	// eino.DefaultOllamaBaseURL.
	BaseURL string
	// Model is the Ollama model tag to pull, e.g. "llama3.1:8b".
	Model string
	// Progress receives one line per eino.PullProgress update as the pull
	// streams; nil discards them.
	Progress io.Writer
}

// RunModelsPull pulls opts.Model into the local Ollama daemon via
// eino.OllamaClient.PullModel (review-whole.md M-1: W-C-03's only
// production call site) and then force-refreshes the on-disk discovery
// cache so the newly pulled model shows up on the very next `yanshi
// doctor`/model-picker read instead of waiting out eino.DefaultCacheTTL.
//
// The real cache/client construction lives here rather than in
// runModelsPullWith, the same split checkLocalRuntimes/checkLocalRuntimesWith
// use — so a test can point runModelsPullWith at a temp-dir cache and an
// httptest.Server client instead of the operator's real
// eino.DefaultCacheDir() and loopback Ollama daemon.
func RunModelsPull(ctx context.Context, opts ModelsPullOptions) error {
	if opts.Model == "" {
		return fmt.Errorf("yanshi models pull: -model is required")
	}
	cache, err := eino.NewCache("", 0)
	if err != nil {
		return fmt.Errorf("yanshi models pull: %w", err)
	}
	client := eino.NewOllamaClient(opts.BaseURL, nil)
	return runModelsPullWith(ctx, cache, client, opts.Model, opts.Progress)
}

// runModelsPullWith is RunModelsPull's body, parameterized by cache and
// client for testability (see RunModelsPull's doc comment).
func runModelsPullWith(ctx context.Context, cache *eino.Cache, client *eino.OllamaClient, model string, progress io.Writer) error {
	onProgress := func(p eino.PullProgress) {
		if progress == nil {
			return
		}
		if p.Total > 0 {
			fmt.Fprintf(progress, "%s: %s (%d/%d)\n", model, p.Status, p.Completed, p.Total)
			return
		}
		fmt.Fprintf(progress, "%s: %s\n", model, p.Status)
	}
	if err := client.PullModel(ctx, model, onProgress); err != nil {
		return fmt.Errorf("yanshi models pull: %w", err)
	}
	refreshLocalRuntimeCache(ctx, cache, client)
	return nil
}

// ModelsPreheatOptions configures RunModelsPreheat.
type ModelsPreheatOptions struct {
	// BaseURL overrides LM Studio's default loopback address; empty uses
	// eino.DefaultLMStudioBaseURL.
	BaseURL string
	// APIKey is LM Studio's optional bearer token; empty when unset.
	APIKey string
	// Model is the LM Studio model id to load.
	Model string
	// Load carries llama.cpp's optional tuning knobs; the zero value
	// requests every llama.cpp default (see eino.LoadOptions).
	Load eino.LoadOptions
}

// RunModelsPreheat warms opts.Model into LM Studio's memory via
// eino.LMStudioClient.LoadModel (review-whole.md M-1: W-C-05's "load_model
// 预热" acceptance bullet's only production call site) and then
// force-refreshes the on-disk discovery cache so the model's new Loaded
// state shows up on the next read. See RunModelsPull's doc comment for why
// the real cache/client construction is split out from
// runModelsPreheatWith.
func RunModelsPreheat(ctx context.Context, opts ModelsPreheatOptions) (eino.LoadResult, error) {
	if opts.Model == "" {
		return eino.LoadResult{}, fmt.Errorf("yanshi models preheat: -model is required")
	}
	cache, err := eino.NewCache("", 0)
	if err != nil {
		return eino.LoadResult{}, fmt.Errorf("yanshi models preheat: %w", err)
	}
	client := eino.NewLMStudioClient(opts.BaseURL, opts.APIKey, nil)
	return runModelsPreheatWith(ctx, cache, client, opts.Model, opts.Load)
}

// runModelsPreheatWith is RunModelsPreheat's body, parameterized by cache
// and client for testability (see RunModelsPull's doc comment).
func runModelsPreheatWith(ctx context.Context, cache *eino.Cache, client *eino.LMStudioClient, model string, load eino.LoadOptions) (eino.LoadResult, error) {
	result, err := client.LoadModel(ctx, model, load)
	if err != nil {
		return eino.LoadResult{}, fmt.Errorf("yanshi models preheat: %w", err)
	}
	refreshLocalRuntimeCache(ctx, cache, client)
	return result, nil
}

// refreshLocalRuntimeCache force-refreshes f's on-disk listing in cache
// (eino.RefreshForce — review-whole.md M-1: its only production call site)
// after a pull/preheat, so the operator's very next doctor/model-picker
// read reflects the change immediately instead of waiting out
// eino.DefaultCacheTTL. Best-effort: a refresh failure (e.g. the runtime
// briefly refusing the very next connection) does not fail the pull/preheat
// that already succeeded — the model IS pulled/loaded either way, this step
// is only a convenience so the cache doesn't lag behind it.
func refreshLocalRuntimeCache(ctx context.Context, cache *eino.Cache, f eino.Fetcher) {
	_, _ = cache.Get(ctx, f, eino.RefreshForce)
}
