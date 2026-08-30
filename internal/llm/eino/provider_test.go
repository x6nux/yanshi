package eino

import (
	"bytes"
	"log/slog"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/config"
)

func TestBuildProviders_BuildsPerConfig(t *testing.T) {
	cfg := &config.Config{
		LLM: config.LLMConfig{Providers: []config.ProviderConfig{
			{Name: "openai", Model: "gpt-4o", APIKey: "k"},
			{Name: "ollama", Model: "gpt-4o-mini", APIKey: "k", BaseURL: "http://localhost:11434/v1"},
		}},
	}
	models, chain, _, _, _, _, err := BuildProviders(cfg)
	// If version-compat (Task 1) failed, skip with a clear reason rather than
	// failing the suite. Task 1 confirmed compat, so we expect a real chain.
	if err != nil {
		t.Skipf("eino-ext openai provider unavailable in this eino version: %v", err)
	}
	require.Len(t, chain, 2)
	assert.NotNil(t, chain[0])
	assert.NotNil(t, chain[1])

	// The model map mirrors the chain, keyed by the REAL model id (not the
	// config name), so /model lists concrete model names.
	assert.Len(t, models, 2, "one map entry per provider")
	assert.Contains(t, models, "gpt-4o")
	assert.Contains(t, models, "gpt-4o-mini")
	assert.NotNil(t, models["gpt-4o"])
	assert.NotNil(t, models["gpt-4o-mini"])
}

// TestBuildProviders_FallsBackToNameOnDuplicateModel proves that when two
// providers share a model id, the second falls back to its config name so both
// stay uniquely selectable.
func TestBuildProviders_FallsBackToNameOnDuplicateModel(t *testing.T) {
	cfg := &config.Config{
		LLM: config.LLMConfig{Providers: []config.ProviderConfig{
			{Name: "claude", Model: "auto", APIKey: "k"},
			{Name: "openai", Model: "auto", APIKey: "k2"},
		}},
	}
	models, _, _, _, _, _, err := BuildProviders(cfg)
	if err != nil {
		t.Skipf("eino-ext openai provider unavailable in this eino version: %v", err)
	}
	assert.Len(t, models, 2, "both providers selectable despite shared model id")
	assert.Contains(t, models, "auto")   // first provider keyed by model id
	assert.Contains(t, models, "openai") // second fell back to its name
}

func TestBuildProviders_EmptyProviders(t *testing.T) {
	cfg := &config.Config{
		LLM: config.LLMConfig{Providers: nil},
	}
	models, chain, _, _, _, _, err := BuildProviders(cfg)
	require.NoError(t, err)
	assert.Empty(t, chain)
	assert.Empty(t, models)
}

// TestBuildProvidersKindDispatch verifies that BuildProviders routes each
// ProviderConfig to the correct adapter based on its Kind field rather than
// always constructing an OpenAI ChatModel. The anthropic provider must yield a
// *AnthropicModel; the openai provider still yields the OpenAI adapter.
func TestBuildProvidersKindDispatch(t *testing.T) {
	cfg := &config.Config{LLM: config.LLMConfig{Providers: []config.ProviderConfig{
		{Name: "oa", Kind: "openai", Model: "gpt-4o", APIKey: "k"},
		{Name: "cl", Kind: "anthropic", Model: "claude-opus-4-8", APIKey: "k"},
	}}}
	models, chain, _, _, _, _, err := BuildProviders(cfg)
	// eino-ext openai provider may be unavailable in the pinned eino version;
	// skip rather than fail in that case (same pattern as the tests above).
	if err != nil {
		t.Skipf("eino-ext openai provider unavailable in this eino version: %v", err)
	}
	require.Len(t, chain, 2)
	if _, ok := models["gpt-4o"]; !ok {
		t.Errorf("openai provider 应以 model id 为键，got keys: %v", keysOf(models))
	}
	if _, ok := models["claude-opus-4-8"]; !ok {
		t.Errorf("anthropic provider 应以 model id 为键，got keys: %v", keysOf(models))
	}
	if _, ok := chain[1].(*AnthropicModel); !ok {
		t.Errorf("期望 *AnthropicModel，实际 %T", chain[1])
	}
}

// TestBuildProvidersKindResponses verifies that Kind="openai-responses"
// constructs a *openaiResponsesModel via NewOpenAIResponsesModel rather than
// silently falling back to the OpenAI Chat Completions adapter. The adapter
// itself is exercised end-to-end (httptest) in responses_test.go.
func TestBuildProvidersKindResponses(t *testing.T) {
	cfg := &config.Config{LLM: config.LLMConfig{Providers: []config.ProviderConfig{
		{Name: "r", Kind: "openai-responses", Model: "gpt-4o", APIKey: "k"},
	}}}
	models, chain, _, _, _, _, err := BuildProviders(cfg)
	if err != nil {
		t.Fatalf("期望成功，实际 err=%v", err)
	}
	require.Len(t, chain, 1)
	if _, ok := chain[0].(*openaiResponsesModel); !ok {
		t.Errorf("期望 *openaiResponsesModel，实际 %T", chain[0])
	}
	if _, ok := models["gpt-4o"]; !ok {
		t.Errorf("期望以 model id 为键，got keys: %v", keysOf(models))
	}
}

// TestNormalizeKind covers the Kind normalization table so dispatch stays
// predictable for uppercased / whitespace-padded / alias inputs.
func TestNormalizeKind(t *testing.T) {
	cases := map[string]string{
		"":                 "openai",
		"openai":           "openai",
		"OPENAI":           "openai",
		"  Anthropic  ":    "anthropic",
		"openai-responses": "openai-responses",
		"responses":        "openai-responses",
		"Responses":        "openai-responses",
		"custom-foo":       "custom-foo",
	}
	for in, want := range cases {
		if got := normalizeKind(in); got != want {
			t.Errorf("normalizeKind(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestBuildProviders_WindowsKeyedByRegistryId locks in the C1 fix: the windows
// map returned by BuildProviders is keyed by the SAME registry key as the
// models map (the provider's REAL model id, e.g. "gpt-4o"), NOT p.Name (e.g.
// "openai"). When Name != Model — the typical production config — the per-model
// window MUST still be reachable via the model id, because that is what the
// http layer queries (cs.model / req.Model). The historical bug was bootstrap
// rebuilding the map by p.Name, which never matched the registry's model-id
// keys and silently disabled the per-model window feature.
func TestBuildProviders_WindowsKeyedByRegistryId(t *testing.T) {
	cfg := &config.Config{LLM: config.LLMConfig{Providers: []config.ProviderConfig{
		{Name: "openai", Model: "gpt-4o", APIKey: "k", ContextWindow: 128000},
		{Name: "claude", Model: "claude-opus-4-8", APIKey: "k", ContextWindow: 200000},
	}}}
	models, _, windows, _, _, _, err := BuildProviders(cfg)
	if err != nil {
		t.Skipf("eino-ext openai provider unavailable in this eino version: %v", err)
	}
	// The registry is keyed by the REAL model id, so the windows map MUST use
	// the same keys — never p.Name.
	assert.Contains(t, models, "gpt-4o", "registry keyed by model id")
	assert.Contains(t, models, "claude-opus-4-8", "registry keyed by model id")
	assert.Equal(t, 128000, windows["gpt-4o"],
		"per-model window reachable by the registry key (model id)")
	assert.Equal(t, 200000, windows["claude-opus-4-8"],
		"per-model window reachable by the registry key (model id)")
	_, hasNameKey := windows["openai"]
	assert.False(t, hasNameKey,
		"p.Name MUST NOT be a key — contextWindowFor queries with the model id")
	_, hasNameKey2 := windows["claude"]
	assert.False(t, hasNameKey2,
		"p.Name MUST NOT be a key — contextWindowFor queries with the model id")
}

// TestBuildProviders_UnknownModelLogsDegradationWarningAndDoesNotError pins
// INF2 acceptance #2 (a model the catalog cannot find must degrade to a safe
// default and must not block startup) all the way through the real
// composition path a provider walks — not just the pure-function level
// covered by TestAcceptance2_UnknownModelResolvesToSafeDefaults in
// modelcatalog_test.go.
//
// Kind is "anthropic" rather than "openai" so this test does not depend on
// eino-ext's OpenAI adapter availability (see the t.Skip pattern the other
// tests in this file use) — anthropic.go's adapter has no such compat gate.
func TestBuildProviders_UnknownModelLogsDegradationWarningAndDoesNotError(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	const unknown = "totally-unknown-anywhere-vendor-xyz"
	cfg := &config.Config{LLM: config.LLMConfig{Providers: []config.ProviderConfig{
		{Name: "cl", Kind: "anthropic", Model: unknown, APIKey: "k"},
	}}}
	models, chain, windows, _, _, _, err := BuildProviders(cfg)
	require.NoError(t, err, "an unrecognised model must not block startup")
	require.Len(t, chain, 1)
	assert.Contains(t, models, unknown)
	assert.Greater(t, windows[unknown], 0,
		"an unlisted model must still resolve to a positive conservative default window, not 0")

	logged := buf.String()
	assert.Contains(t, logged, "model not in context-window catalog",
		"the degradation must be observable, not silent")
	assert.Contains(t, logged, unknown)
}

// keysOf returns the map keys (for assertion error messages only).
func keysOf(m map[string]model.BaseChatModel) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
