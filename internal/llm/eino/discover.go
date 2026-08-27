// internal/llm/eino/discover.go
//
// M9: model-catalogue discovery and startup preflight.
//
// A typo in `model:` used to be discovered by the model itself, on the first
// real turn, as a 404 — and the classifier files a 404 as ClassClientError,
// which is correctly NOT retried, so the turn simply failed. The user's first
// interaction with a freshly configured yanshi was an opaque provider error
// with no indication that the fix was one character in a YAML file.
//
// The provider already knows the answer: every OpenAI-compatible endpoint
// serves `GET /v1/models`. Asking once at startup turns a mid-turn failure into
// a startup log line that names the misspelling and the closest real model.
//
// NON-BLOCKING IS THE WHOLE DESIGN. Plenty of legitimate deployments have no
// such endpoint: Azure serves deployments rather than models, GitHub Models
// omits the listing entirely, air-gapped relays and llama.cpp builds vary. If
// preflight could refuse a startup, this feature would be a new way to break
// working configurations — so every failure mode (no endpoint, timeout, auth
// rejection, unparsable body) degrades to "skipped" with one log line, and the
// only outcome that ever produces a warning is a listing that SUCCEEDED and did
// not contain the configured model.
package eino

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"
)

// DefaultDiscoveryTimeout bounds one catalogue fetch.
//
// Short on purpose: this runs on the startup path, its result is advisory, and
// a provider whose control plane needs more than five seconds to list models is
// exactly the provider whose listing we should stop waiting for.
const DefaultDiscoveryTimeout = 5 * time.Second

// ModelCatalog is one provider's advertised model ids.
type ModelCatalog struct {
	// Provider is the config name of the provider that was queried.
	Provider string
	// Models are the ids the endpoint advertised, in the order returned.
	Models []string
}

// modelsResponse is the OpenAI `GET /v1/models` body. Only `id` is read: the
// remaining fields (object, created, owned_by) vary between gateways and none
// of them affects whether a configured name exists.
type modelsResponse struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

// modelsEndpoint derives the listing URL from a provider base URL.
//
// The base URL operators configure already includes the version segment for
// most providers ("https://api.example.com/v1"), so appending "/models" is
// correct there; when it does not, "/v1/models" is appended instead. Getting
// this wrong costs a 404 that is indistinguishable from "no listing endpoint",
// which is precisely the outcome that must not be reported as a config error.
func modelsEndpoint(baseURL string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if trimmed == "" {
		return ""
	}
	if strings.HasSuffix(trimmed, "/v1") || strings.Contains(trimmed, "/v1/") {
		return trimmed + "/models"
	}
	return trimmed + "/v1/models"
}

// FetchModelCatalog performs one `GET /v1/models` against baseURL.
//
// The returned error means "no usable listing", never "the configuration is
// wrong" — see the file comment. An empty baseURL yields an error immediately
// rather than defaulting to api.openai.com: a provider that did not configure
// one is using the SDK's own default, and probing a host the operator never
// named would leak the API key to it.
func FetchModelCatalog(ctx context.Context, client *http.Client, baseURL, apiKey string) ([]string, error) {
	url := modelsEndpoint(baseURL)
	if url == "" {
		return nil, fmt.Errorf("eino: model discovery: no base_url configured")
	}
	if client == nil {
		client = &http.Client{Timeout: DefaultDiscoveryTimeout}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("eino: model discovery: %w", err)
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("eino: model discovery: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("eino: model discovery: %s returned %d", url, resp.StatusCode)
	}
	var body modelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("eino: model discovery: decode %s: %w", url, err)
	}
	out := make([]string, 0, len(body.Data))
	for _, m := range body.Data {
		if m.ID != "" {
			out = append(out, m.ID)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("eino: model discovery: %s advertised no models", url)
	}
	return out, nil
}

// PreflightResult is the verdict for one configured provider.
type PreflightResult struct {
	// Provider is the config name.
	Provider string
	// Model is the configured model id.
	Model string
	// Skipped is true when no catalogue could be fetched. SkipReason says why.
	// A skipped preflight asserts NOTHING about the model.
	Skipped bool
	// SkipReason is the fetch failure, present only when Skipped.
	SkipReason string
	// Found is true when Model appeared in the catalogue.
	Found bool
	// Suggestions are the closest catalogue entries when Found is false,
	// nearest first, capped at maxSuggestions.
	Suggestions []string
}

// maxSuggestions caps the candidate list in a warning.
//
// Three, because the list exists to be read at a glance in a startup log. A
// gateway advertising 400 models would otherwise turn one misspelling into a
// screenful, and the operator would scroll past the line that was trying to
// help them.
const maxSuggestions = 3

// CheckModelName reports whether model is in catalog, plus the nearest
// alternatives when it is not.
//
// Matching is case-insensitive because gateways are inconsistent about case in
// ids while their APIs accept either.
func CheckModelName(model string, catalog []string) (bool, []string) {
	if model == "" {
		return false, nil
	}
	want := strings.ToLower(model)
	for _, c := range catalog {
		if strings.ToLower(c) == want {
			return true, nil
		}
	}
	return false, nearestModels(model, catalog)
}

// scoredModel pairs a candidate with its distance for stable sorting.
type scoredModel struct {
	name string
	dist int
}

// containmentBonus makes any substring relationship outrank every edit
// distance. It is larger than any plausible model-id length, so a container
// can never be pushed below a non-container by the extra-length tiebreak.
const containmentBonus = 1 << 20

// nearestModels ranks catalogue entries by edit distance to model.
//
// A SUBSTRING MATCH OUTRANKS EVERY EDIT DISTANCE, which is the case that
// actually matters in practice: the real mistake is almost never a
// single-character typo but a missing or extra qualifier — "gpt-4o" against a
// catalogue serving "gpt-4o-2026-05-13", or "claude-sonnet-5" against
// "anthropic/claude-sonnet-5". Pure Levenshtein ranks those far apart because
// the qualifier is long, and would suggest an unrelated short id instead.
//
// Among containers the tiebreak is how much extra text separates them from
// what was typed, ascending — the same "fewest edits away" principle the
// fallback uses, so the two halves of the ranking do not contradict each other.
func nearestModels(model string, catalog []string) []string {
	if len(catalog) == 0 {
		return nil
	}
	lower := strings.ToLower(model)
	scored := make([]scoredModel, 0, len(catalog))
	for _, c := range catalog {
		lc := strings.ToLower(c)
		d := levenshtein(lower, lc)
		if strings.Contains(lc, lower) || strings.Contains(lower, lc) {
			extra := len(lc) - len(lower)
			if extra < 0 {
				extra = -extra
			}
			d = extra - containmentBonus
		}
		scored = append(scored, scoredModel{name: c, dist: d})
	}
	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].dist != scored[j].dist {
			return scored[i].dist < scored[j].dist
		}
		return scored[i].name < scored[j].name
	})
	n := len(scored)
	if n > maxSuggestions {
		n = maxSuggestions
	}
	out := make([]string, 0, n)
	for _, s := range scored[:n] {
		out = append(out, s.name)
	}
	return out
}

// levenshtein returns the edit distance between a and b using a rolling
// single-row DP (O(len(b)) memory).
func levenshtein(a, b string) int {
	ar, br := []rune(a), []rune(b)
	if len(ar) == 0 {
		return len(br)
	}
	if len(br) == 0 {
		return len(ar)
	}
	prev := make([]int, len(br)+1)
	cur := make([]int, len(br)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ar); i++ {
		cur[0] = i
		for j := 1; j <= len(br); j++ {
			cost := 1
			if ar[i-1] == br[j-1] {
				cost = 0
			}
			cur[j] = minInt(minInt(cur[j-1]+1, prev[j]+1), prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(br)]
}

// minInt returns the smaller of two ints.
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ProviderProbe is the minimum a preflight needs to know about a provider.
// It is a local shape rather than config.ProviderConfig so this function stays
// callable from tests and from the composition root without either of them
// building a full config.
type ProviderProbe struct {
	// Name is the config label, used in log lines.
	Name string
	// Model is the configured model id to verify.
	Model string
	// BaseURL is the endpoint to query.
	BaseURL string
	// APIKey authenticates the listing request.
	APIKey string
}

// PreflightModels checks every provider's configured model against its
// advertised catalogue and returns one result per provider.
//
// It NEVER returns an error, and that signature is the contract: there is no
// failure of this function that should be able to reach a caller who might
// treat it as fatal. Providers are probed sequentially — a startup has a
// handful of them, and sequential probing keeps the log lines in config order,
// which is the order the operator will look for them in.
func PreflightModels(ctx context.Context, client *http.Client, providers []ProviderProbe) []PreflightResult {
	out := make([]PreflightResult, 0, len(providers))
	for _, p := range providers {
		res := PreflightResult{Provider: p.Name, Model: p.Model}
		catalog, err := FetchModelCatalog(ctx, client, p.BaseURL, p.APIKey)
		if err != nil {
			res.Skipped = true
			res.SkipReason = err.Error()
			out = append(out, res)
			continue
		}
		res.Found, res.Suggestions = CheckModelName(p.Model, catalog)
		out = append(out, res)
	}
	return out
}

// LogPreflight emits one line per result: DEBUG for a skip (expected and
// uninteresting), DEBUG for a confirmed model, WARN only for a model that a
// SUCCESSFUL listing did not contain.
//
// The asymmetry is the point. A skip is the normal state for several supported
// providers, so warning on it would train operators to ignore the whole
// category — including the one line that means something.
func LogPreflight(results []PreflightResult) {
	for _, r := range results {
		switch {
		case r.Skipped:
			slog.Debug("model preflight skipped",
				"provider", r.Provider, "model", r.Model, "reason", r.SkipReason)
		case r.Found:
			slog.Debug("model preflight ok", "provider", r.Provider, "model", r.Model)
		default:
			slog.Warn("configured model not advertised by provider",
				"provider", r.Provider,
				"model", r.Model,
				"closest", strings.Join(r.Suggestions, ", "),
				"effect", "calls to this model will fail with a client error until the name is corrected")
		}
	}
}
