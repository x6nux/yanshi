// Composition-root wiring for the adaptive model decorator (M5 quirks, M6 tool
// schema sanitizing, M7 rate limiting, M9 preflight, M10 usage accounting, C6
// overflow recovery).
//
// It lives in its own file rather than inside bootstrap.go for the ordinary
// reason (GOV2's 1000-line cap) and for a load-bearing one: the usage sink is
// the single place in the process that knows both internal/store and
// internal/llm/eino, and GOV1 forbids that dependency in EITHER direction —
// internal/store is a port and R5 bars a port from importing the service layer,
// while llm depending on store would invert the hexagon. Keeping the adapter
// here, in the only package allowed to know both, is what makes the narrow
// UsageSink interface worth defining at all.
package bootstrap

import (
	"context"
	"fmt"

	"github.com/cloudwego/eino/components/model"

	"github.com/x6nux/yanshi/internal/config"
	"github.com/x6nux/yanshi/internal/ctxcompact"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/store"
)

// storeUsageSink adapts *store.Store to einollm.UsageSink.
//
// See this file's header for why the adapter lives in the composition root
// rather than on either side of the dependency it bridges.
type storeUsageSink struct{ st *store.Store }

// RecordUsage persists one model call's token usage to the usage_log table.
//
// A zero rec.TS is passed through as zero rather than filled in here: the store
// stamps it, so there is exactly one clock in the path and a row can never be
// stamped twice with two slightly different times.
func (s storeUsageSink) RecordUsage(_ context.Context, rec einollm.UsageRecord) error {
	ts := int64(0)
	if !rec.TS.IsZero() {
		ts = rec.TS.Unix()
	}
	return s.st.AppendUsage(store.UsageEvent{
		TS:               ts,
		Provider:         rec.Provider,
		Model:            rec.Model,
		SessionID:        rec.SessionID,
		PromptTokens:     rec.PromptTokens,
		CompletionTokens: rec.CompletionTokens,
		CachedTokens:     rec.CachedTokens,
		ReasoningTokens:  rec.ReasoningTokens,
		CacheHit:         rec.CacheHit,
	})
}

// compile-time interface check: the adapter is only ever consumed through the
// interface, so a signature drift on either side must fail here rather than at
// the one call site that assigns it.
var _ einollm.UsageSink = storeUsageSink{}

// adaptiveDeps is what BuildAdaptiveModels needs from the composition root.
// It is a struct rather than six parameters because every field is optional in
// a different way, and a positional call would make "which nil disabled which
// feature" unreadable at the call site.
type adaptiveDeps struct {
	// Cfg is the loaded configuration, read for the rate limits, the sanitize
	// policy, and each provider's own window.
	Cfg *config.Config
	// Store receives usage rows. nil disables M10 accounting.
	Store *store.Store
	// Windows maps registry key to context window, as BuildProviders returned
	// it. A model absent from it gets overflow recovery disabled (C6 needs a
	// budget to compact toward).
	Windows map[string]int
	// Redactor strips secrets from the history handed to the forced-compaction
	// summariser, exactly as the proactive path does. nil disables it.
	Redactor ctxcompact.Redactor
}

// BuildAdaptiveModels wraps every provider model in an einollm.AdaptiveModel
// and returns the wrapped registry plus the wrapped failover chain, in the
// SAME order the chain arrived in.
//
// PLACEMENT: the wrapper goes INSIDE the resilient failover chain, one per
// provider, not once around it. Two reasons, and both are the kind that only
// show up in production:
//
//   - The repairs are per-model. A quirk learned about a strict gateway must
//     not be applied to the first-party provider it fails over to, and a rate
//     limit configured for one plan must not throttle another.
//   - The repairs need the RAW provider error. ResilientChatModel's job is to
//     move on to the next provider, so by the time an error reaches the
//     outside it has been classified and rewrapped for failover; the schema
//     rejection that M6 recognises is no longer distinguishable there from any
//     other 400.
//
// A model the wrapper cannot be built for (nil inner) is passed through
// unwrapped rather than dropped: losing a provider from the failover chain is
// a strictly worse failure than losing the adaptive behaviours on it.
func BuildAdaptiveModels(named map[string]model.BaseChatModel, chain []model.BaseChatModel,
	deps adaptiveDeps) (map[string]model.BaseChatModel, []model.BaseChatModel) {

	if len(named) == 0 {
		return named, chain
	}
	cfg := deps.Cfg
	quirks := einollm.NewQuirkStore()
	limiter := einollm.NewRateLimiter(
		einollm.RateLimitConfig{QPM: cfg.LLM.RateLimit.QPM, Burst: cfg.LLM.RateLimit.Burst},
		perModelRateLimits(cfg))
	var sink einollm.UsageSink
	if deps.Store != nil {
		sink = storeUsageSink{st: deps.Store}
	}
	sanitize := einollm.NormalizeSanitizeMode(cfg.LLM.SanitizeToolSchemas)
	kinds := providerKinds(cfg)

	adapted := make(map[string]model.BaseChatModel, len(named))
	// byInner lets the chain be rebuilt by IDENTITY. The chain and the map hold
	// the same pointers but the chain carries no keys, and reconstructing it
	// from sorted map order would silently reorder failover — which is the one
	// property of the chain that is configured rather than incidental.
	byInner := make(map[model.BaseChatModel]model.BaseChatModel, len(named))
	for key, inner := range named {
		a := einollm.NewAdaptiveModel(inner, einollm.AdaptiveConfig{
			ModelID:   key,
			Provider:  kinds[key],
			Quirks:    quirks,
			Limiter:   limiter,
			UsageSink: sink,
			Sanitize:  sanitize,
			Overflow: einollm.OverflowRecoveryConfig{
				ContextWindow: deps.Windows[key],
				Redactor:      deps.Redactor,
			},
		})
		if a == nil {
			adapted[key] = inner
			continue
		}
		adapted[key] = a
		byInner[inner] = a
	}

	outChain := make([]model.BaseChatModel, 0, len(chain))
	for _, m := range chain {
		if w, ok := byInner[m]; ok {
			outChain = append(outChain, w)
			continue
		}
		outChain = append(outChain, m)
	}
	return adapted, outChain
}

// perModelRateLimits builds the per-model override table keyed the same way
// einollm.BuildProviders keys its registry, so a limit configured on a provider
// reaches the bucket that provider's calls actually consult.
//
// A provider that sets neither qpm nor burst gets NO entry rather than a zero
// entry: an explicit zero-QPM entry would read as "unlimited" and would
// override a global default the operator did set, which is the opposite of
// "inherit".
func perModelRateLimits(cfg *config.Config) map[string]einollm.RateLimitConfig {
	out := map[string]einollm.RateLimitConfig{}
	for key, p := range providerConfigsByKey(cfg) {
		if p.QPM == 0 && p.Burst == 0 {
			continue
		}
		out[key] = einollm.RateLimitConfig{QPM: p.QPM, Burst: p.Burst}
	}
	return out
}

// providerKinds maps registry key to adapter kind, for the provider label on
// usage rows.
func providerKinds(cfg *config.Config) map[string]string {
	out := map[string]string{}
	for key, p := range providerConfigsByKey(cfg) {
		out[key] = p.Kind
	}
	return out
}

// providerConfigsByKey re-derives the registry key for each configured
// provider.
//
// The key derivation MUST match einollm.BuildProviders exactly — model id,
// falling back to name, falling back to a positional label, with a used-set so
// two providers on the same model do not collide. A second, subtly different
// derivation here would produce a table whose keys nothing ever looks up, which
// is the silent-no-op shape: every rate limit configured per provider would be
// ignored and no error would say so.
func providerConfigsByKey(cfg *config.Config) map[string]config.ProviderConfig {
	out := make(map[string]config.ProviderConfig, len(cfg.LLM.Providers))
	used := make(map[string]bool, len(cfg.LLM.Providers))
	for i, p := range cfg.LLM.Providers {
		key := p.Model
		if key == "" || used[key] {
			key = p.Name
		}
		if key == "" || used[key] {
			key = fmt.Sprintf("model-%d", i)
		}
		used[key] = true
		out[key] = p
	}
	return out
}

// RunPreflight performs the M9 startup model-name check and logs the result.
//
// It returns nothing, and that is the design: no outcome of a preflight may
// reach a caller who could treat it as fatal. The check talks to every
// configured provider's catalogue endpoint, which is exactly the network call
// most likely to be unavailable on the deployment least able to tolerate a
// refused startup. A wrong model name is worth a loud warning naming the
// nearest matches; it is not worth a server that will not boot.
//
// cfg.LLM.Preflight == nil means enabled: the default is on, and only an
// explicit `preflight: false` turns it off.
func RunPreflight(ctx context.Context, cfg *config.Config) {
	if cfg.LLM.Preflight != nil && !*cfg.LLM.Preflight {
		return
	}
	if len(cfg.LLM.Providers) == 0 {
		return
	}
	probes := make([]einollm.ProviderProbe, 0, len(cfg.LLM.Providers))
	for _, p := range cfg.LLM.Providers {
		probes = append(probes, einollm.ProviderProbe{
			Name: p.Name, Model: p.Model, BaseURL: p.BaseURL, APIKey: p.APIKey,
		})
	}
	pctx, cancel := context.WithTimeout(ctx, einollm.DefaultDiscoveryTimeout)
	defer cancel()
	einollm.LogPreflight(einollm.PreflightModels(pctx, nil, probes))
}
