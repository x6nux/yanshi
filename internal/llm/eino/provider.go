package eino

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/model"

	"github.com/x6nux/yanshi/internal/config"
)

// BuildProviders builds the configured providers as both an ordered failover
// chain and a name→model map, so a session can switch models mid-conversation
// (the map) while the resilient default falls across them in config order
// (the chain).
//
// Dispatch is by ProviderConfig.Kind:
//   - "openai" (default when Kind is empty) → OpenAI Chat Completions adapter,
//   - "anthropic" → Anthropic Messages API adapter (anthropic.go),
//   - "openai-responses" → OpenAI Responses API adapter (responses.go),
//     the unified successor to Chat Completions.
//
// The HTTP client intentionally has NO retry and NO timeout at the transport
// layer: ResilientChatModel (see resilient.go) is the SINGLE retry authority —
// it handles transient failures (network blips, 5xx, gateway EOFs, mid-stream
// drops) with exponential backoff and failover across the chain. A second
// retry loop here would double-count attempts, double the backoff wall-clock,
// and muddy retry observability (the TUI's "↻ retry N/M…" line tracks the
// model-layer count). Real 4xx client errors are also filtered at the model
// layer (isNonRetryableClientErr), so there is nothing left for transport
// retry to do usefully. http.DefaultTransport still provides connection reuse
// and proper protocol-level behavior.
//
// Returns (models, chain, windows, thresholds, truncations, err): models
// keys each entry by the provider's REAL model id (with fallbacks — see
// chooseKey); chain is the same models in config order (for
// NewResilientModel); windows maps the SAME registry key → p.ContextWindow
// for every provider that sets one, so the http layer's per-model window
// lookup (contextWindowFor) hits when the session queries it with the
// registry key (cs.model / req.Model — the model id, not the config name).
// Bootstrap forwards `windows` directly; do NOT rebuild it from
// cfg.LLM.Providers by p.Name (the historical bug — keys did not match the
// registry's model-id keys, so the per-model window was dead).
//
// W-C-01 (INF2): thresholds mirrors windows for the auto-compact threshold —
// same registry key, same "only present when something resolved a value"
// shape — so the pre-turn (contextWindowFor's sibling, thresholdFor) and
// mid-turn (CompactionConfig.ProviderThresholds) paths both key off it the
// same way they already key off windows. A model with neither an explicit
// config override nor a catalog row is simply absent from this map; the
// caller's own CompactionConfig.Threshold applies unchanged (see
// ResolveAutoCompactThreshold, modelcatalog.go).
//
// M-4: truncations mirrors windows/thresholds for W-C-09's truncation
// policy, same registry key, same "only present when something resolved a
// value" shape — ResolveTruncationPolicy(p.TruncationPolicy, p.Model) per
// provider, present only when its second return is true. Before this field
// existed, ProviderConfig.TruncationPolicy's own doc comment documented it
// as a PER-PROVIDER override while bootstrap.go only ever resolved
// cfg.LLM.Providers[0] once at boot and bound that single value
// unconditionally for every turn — a config with providers[1].truncation_policy
// set had that value read by ResolveTruncationPolicy's doc comment and
// nothing else; see orchestrator.Config.ProviderTruncationPolicies, the
// consumer this map now feeds.
//
// registrars is optional (variadic, so the many existing call sites that do
// not exercise W-C-12's B-2 credential registration keep compiling
// unchanged) — the first non-nil element, if any, is forwarded to every
// provider's auth.command token source so a command-produced credential is
// scrubbed from logs/WS/SSE/SQLite the same way a config api_key or header
// already is. See SecretRegistrar's doc comment (cmdauth.go).
func BuildProviders(cfg *config.Config, registrars ...SecretRegistrar) (map[string]model.BaseChatModel, []model.BaseChatModel, map[string]int, map[string]float64, map[string]TruncationSpec, error) {
	ctx := context.Background()
	registrar := firstRegistrar(registrars)
	chain := make([]model.BaseChatModel, 0, len(cfg.LLM.Providers))
	models := make(map[string]model.BaseChatModel, len(cfg.LLM.Providers))
	windows := make(map[string]int, len(cfg.LLM.Providers))
	thresholds := make(map[string]float64, len(cfg.LLM.Providers))
	truncations := make(map[string]TruncationSpec, len(cfg.LLM.Providers))
	// The registry is keyed by the provider's REAL model id (config `model`),
	// so /model lists and switches on the concrete model name (e.g.
	// "claude-opus-4-8") rather than the config label (e.g. "claude"). When the
	// model id is empty or shared by another provider (e.g. a generic "auto"),
	// fall back to the config `name` then a synthetic key so every provider stays
	// uniquely selectable.
	used := make(map[string]bool, len(cfg.LLM.Providers))
	chooseKey := func(p config.ProviderConfig, i int) string {
		for _, cand := range []string{p.Model, p.Name, fmt.Sprintf("model-%d", i)} {
			if cand != "" && !used[cand] {
				return cand
			}
		}
		return fmt.Sprintf("model-%d", i)
	}
	for i, p := range cfg.LLM.Providers {
		m, err := buildOne(ctx, p, registrar)
		if err != nil {
			return nil, nil, nil, nil, nil, fmt.Errorf("eino: build provider %q (kind=%q): %w", p.Name, p.Kind, err)
		}
		chain = append(chain, m)
		key := chooseKey(p, i)
		used[key] = true
		models[key] = m
		// Mirror the SAME registry key into the windows map. contextWindowFor
		// queries by cs.model/req.Model (the model id), so the windows key MUST
		// be the registry key — not p.Name — or the per-model window silently
		// misses and compaction falls back to the global ContextWindow.
		//
		// M2/M3: the window is RESOLVED, not copied. An omitted
		// `context_window` used to leave the key absent, so the model inherited
		// the global 256K fallback and a 128K model's compaction gate never
		// opened. ResolveContextWindow answers for every provider — explicit
		// config first, then the static catalog (skipped for local runtimes),
		// then a conservative default — so the map is now total and the
		// fallback only applies to models nothing recognises.
		//
		// A model neither the catalog nor the operator recognises is NOT a
		// startup error (INF2 acceptance #2: an unlisted model must degrade,
		// not block boot) — it is logged so the degradation is observable,
		// same as discover.go's "configured model not advertised" warning.
		w, src := ResolveContextWindow(providerShape(p), 0)
		if w > 0 {
			windows[key] = w
		}
		if src == WindowFromDefault {
			slog.Warn("model not in context-window catalog; using conservative default",
				"model", p.Model, "provider", p.Name, "default_tokens", w)
		}
		if t, ok := ResolveAutoCompactThreshold(providerShape(p)); ok {
			thresholds[key] = t
		}
		if spec, ok := ResolveTruncationPolicy(p.TruncationPolicy, p.Model); ok {
			truncations[key] = spec
		}
	}
	return models, chain, windows, thresholds, truncations, nil
}

// providerShape projects a config.ProviderConfig onto the minimal shape
// context-window resolution reads. It is the ONE conversion point, so a new
// resolution input is added to ProviderShape and mapped here rather than
// duplicated at each call site.
func providerShape(p config.ProviderConfig) ProviderShape {
	return ProviderShape{
		Kind:                 p.Kind,
		Model:                p.Model,
		BaseURL:              p.BaseURL,
		ContextWindow:        p.ContextWindow,
		Local:                p.Local,
		AutoCompactThreshold: p.AutoCompactThreshold,
	}
}

// normalizeKind canonicalizes ProviderConfig.Kind so dispatch is forgiving of
// case differences, surrounding whitespace, and the "responses" shorthand.
// Empty Kind defaults to "openai" (the historical behavior before Kind existed).
// Unknown values are returned lowercased and trimmed unchanged, so a future
// adapter can register without touching this switch.
func normalizeKind(k string) string {
	switch strings.ToLower(strings.TrimSpace(k)) {
	case "", "openai":
		return "openai"
	case "anthropic":
		return "anthropic"
	case "openai-responses", "responses":
		return "openai-responses"
	default:
		return strings.ToLower(strings.TrimSpace(k))
	}
}

// buildOne constructs a single model adapter for p based on its Kind. It is the
// per-provider dispatch point extracted from BuildProviders so the chain loop
// stays readable and each branch is independently testable. See BuildProviders
// for the no-retry/no-timeout HTTP client rationale.
//
// M4: the generation parameters (MaxTokens / Temperature / TopP) are forwarded
// as POINTERS all the way to the wire. Nil means "the operator said nothing",
// and every adapter omits the field entirely in that case, so a provider that
// does not configure them produces the exact request body it produced before.
// A plain value type would erase the difference between "unset" and "set to 0",
// and 0 is meaningful for both temperature (deterministic judge calls) and
// top_p (greedy decoding).
func buildOne(ctx context.Context, p config.ProviderConfig, registrar SecretRegistrar) (model.BaseChatModel, error) {
	// W-C-12: apiKeyOrPlaceholder stands in for the three adapters' non-empty
	// APIKey validation when the real credential comes from auth.command
	// instead. It never reaches the wire — each branch's authRefreshTransport
	// overwrites the credential header on every request before the
	// RoundTripper it wraps sees it. See placeholderAPIKeyForCommandAuth.
	apiKeyOrPlaceholder := p.APIKey
	if apiKeyOrPlaceholder == "" && p.Auth != nil {
		apiKeyOrPlaceholder = placeholderAPIKeyForCommandAuth
	}
	switch normalizeKind(p.Kind) {
	case "anthropic":
		// x-api-key is Anthropic's credential header, sent raw (no "Bearer "
		// prefix) — see AnthropicModel.setHeaders.
		var hc *http.Client
		if p.Auth != nil {
			hc = &http.Client{Transport: authRefreshHTTPTransport(http.DefaultTransport, p.Auth, "x-api-key", false, registrar)}
		}
		return NewAnthropicModel(ctx, &AnthropicModelConfig{
			APIKey:      apiKeyOrPlaceholder,
			Model:       p.Model,
			BaseURL:     p.BaseURL,
			MaxTokens:   derefOr(p.MaxTokens, 0),
			Temperature: p.Temperature,
			TopP:        p.TopP,
			Headers:     p.Headers,
			HTTPClient:  hc,
		})
	case "openai-responses":
		// Authorization: Bearer <key> — see openaiResponsesModel.setHeaders.
		var hc *http.Client
		if p.Auth != nil {
			hc = &http.Client{Transport: authRefreshHTTPTransport(http.DefaultTransport, p.Auth, "Authorization", true, registrar)}
		}
		return NewOpenAIResponsesModel(ctx, &ResponsesConfig{
			APIKey:          apiKeyOrPlaceholder,
			Model:           p.Model,
			BaseURL:         p.BaseURL,
			MaxOutputTokens: derefOr(p.MaxTokens, 0),
			Temperature:     p.Temperature,
			TopP:            p.TopP,
			Headers:         p.Headers,
			HTTPClient:      hc,
		})
	default: // "openai" and any unrecognized kind (backward-compat)
		// go-openai builds Authorization: Bearer <APIKey> itself, so unlike
		// the two hand-rolled adapters above the credential header is
		// already on the request by the time it reaches this transport —
		// authRefreshHTTPTransport's Set still overwrites it with the
		// command-sourced token every request, same as it overrides the
		// two adapters' own setHeaders. It sits BELOW headerCaptureTransport
		// (base, not the client's outer Transport) so it runs LAST, closest
		// to the wire: a static W-C-02 operator header of the same name must
		// not be able to shadow the live, just-refreshed credential.
		hc := newHeaderCaptureClient(p.Headers)
		if p.Auth != nil {
			hc = &http.Client{Transport: &headerCaptureTransport{
				base:    authRefreshHTTPTransport(http.DefaultTransport, p.Auth, "Authorization", true, registrar),
				headers: p.Headers,
			}}
		}
		inner, err := openai.NewChatModel(ctx, &openai.ChatModelConfig{
			APIKey:  apiKeyOrPlaceholder,
			Model:   p.Model,
			BaseURL: p.BaseURL,
			// Pointers forwarded as-is: eino-ext's ChatModelConfig uses the
			// same nil-means-omit convention, so no translation is needed and
			// an unconfigured provider keeps its previous request body.
			MaxTokens:   p.MaxTokens,
			Temperature: p.Temperature,
			TopP:        p.TopP,
			// No Timeout, no retry Transport: see BuildProviders doc comment.
			// The transport DOES capture failed responses' headers, which is
			// observation rather than policy — go-openai discards resp.Header
			// when it builds its error, so without this the Retry-After of
			// every 429 from an OpenAI-compatible endpoint is unrecoverable
			// and M1's cooldown degrades to a blind exponential. See
			// retryafter.go. The same transport also injects p.Headers
			// (W-C-02) — go-openai owns request construction end to end for
			// this kind, so the transport is the only seam that sees the
			// outgoing *http.Request before it is sent.
			HTTPClient: hc,
		})
		if err != nil {
			return nil, err
		}
		// The consumer half of the capture: rejoins the captured header to the
		// error so ClassifyError can read Retry-After from the authoritative
		// source. Only the openai kind needs it; the other two adapters build
		// their own HeaderError.
		return NewHeaderAwareModel(inner), nil
	}
}

// derefOr returns *v, or def when v is nil. Used where an adapter config field
// is a value type whose zero already means "use the adapter default" (Anthropic
// MaxTokens, Responses MaxOutputTokens), so the nil/zero distinction the config
// layer preserves is not needed past this point.
func derefOr[T any](v *T, def T) T {
	if v == nil {
		return def
	}
	return *v
}

// SortedModelNames returns the registry keys of a name→model map in sorted
// order. Returns nil for a nil or empty map.
//
// Sorted (not map-iteration) order is what makes "the first name" a stable,
// reproducible choice across processes — see ResolveModelName.
func SortedModelNames(models map[string]model.BaseChatModel) []string {
	if len(models) == 0 {
		return nil
	}
	names := make([]string, 0, len(models))
	for name := range models {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ResolveModelName resolves the registry name of the model a turn actually runs
// on: `requested` when the caller named one, else the first name in sorted order
// ("" when the registry is empty).
//
// This is the ONE definition of the empty-model fallback, shared by every turn
// entry point (WS session default, SSE per-request model, /api/v1 turn params).
// Those paths use the resolved name for user-visible status, for cost
// attribution, and — since the image fan-out landed — to decide whether the turn
// runs on a multimodal model, so a fallback that drifted between transports
// would show one model, bill another, and drop attachments on a third.
//
// Note it resolves a NAME, not a model: an unknown `requested` name is returned
// as-is rather than replaced. Selecting the model instance stays with the caller
// (an absent name means "orchestrator default"), so an unrecognized name keeps
// its existing meaning on every path.
func ResolveModelName(models map[string]model.BaseChatModel, requested string) string {
	if requested != "" {
		return requested
	}
	if names := SortedModelNames(models); len(names) > 0 {
		return names[0]
	}
	return ""
}
