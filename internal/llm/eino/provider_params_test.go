package eino

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/config"
)

// intPtr / f32Ptr build the pointer generation params.
func intPtr(v int) *int         { return &v }
func f32Ptr(v float32) *float32 { return &v }

// TestBuildProviders_WindowsAreResolvedNotCopied is the M2 wiring assertion.
//
// The windows map used to be populated only for providers that set
// `context_window` by hand. An omitted key left the model absent from the map,
// so contextWindowFor fell through to the global CompactionConfig.ContextWindow
// (256000 by default) and a 128K model computed its compaction threshold
// against twice its capacity — the gate never opened. Now every provider gets
// an answer, and the fallback only sees models nothing recognises.
func TestBuildProviders_WindowsAreResolvedNotCopied(t *testing.T) {
	cfg := &config.Config{}
	cfg.LLM.Providers = []config.ProviderConfig{
		// No context_window: the catalog must answer.
		{Name: "gpt", Kind: "openai", Model: "gpt-4o", APIKey: "k"},
		// No context_window, behind a routing prefix: the catalog must still
		// answer, which is the prefix-penetration requirement.
		{Name: "router", Kind: "openai", Model: "openrouter/anthropic/claude-opus-4-8", APIKey: "k"},
		// Explicit config: must win over the catalog's 200000.
		{Name: "explicit", Kind: "openai", Model: "claude-sonnet-5", APIKey: "k", ContextWindow: 1_000_000},
		// Unknown model: falls to the conservative default rather than being
		// left absent (absent is what produced the 256K inheritance bug).
		{Name: "unknown", Kind: "openai", Model: "inhouse-v7", APIKey: "k"},
	}

	_, _, windows, _, err := BuildProviders(cfg)
	require.NoError(t, err)

	assert.Equal(t, 128_000, windows["gpt-4o"],
		"an omitted context_window must resolve from the catalog, not stay absent")
	assert.Equal(t, 200_000, windows["openrouter/anthropic/claude-opus-4-8"],
		"routing prefixes must not defeat the catalog")
	assert.Equal(t, 1_000_000, windows["claude-sonnet-5"],
		"an explicit context_window must beat the catalog")
	assert.Equal(t, DefaultContextWindow, windows["inhouse-v7"],
		"an unknown model must get the conservative default, not be omitted")
}

// TestBuildProviders_LocalProviderDoesNotInheritCloudWindow is the M3 wiring
// assertion, and the accident it prevents is specific: a local `qwen3-coder`
// given the family's 262144 cloud window computes a compaction threshold far
// above what a default Ollama serve accepts. Compaction never fires, and the
// server does not reject the over-long prompt — it drops the head, taking the
// system prompt with it. The session keeps answering while the model can no
// longer see its instructions.
func TestBuildProviders_LocalProviderDoesNotInheritCloudWindow(t *testing.T) {
	cloudWindow, ok := KnownContextWindow("qwen3-coder")
	require.True(t, ok)
	require.Greater(t, cloudWindow, LocalContextWindow,
		"fixture is only meaningful while the cloud entry is larger than the local default")

	cfg := &config.Config{}
	cfg.LLM.Providers = []config.ProviderConfig{
		{Name: "local-ollama", Kind: "openai", Model: "qwen3-coder", APIKey: "k", BaseURL: "http://127.0.0.1:11434/v1"},
		{Name: "cloud-qwen", Kind: "openai", Model: "qwen3-max", APIKey: "k"},
	}

	_, _, windows, _, err := BuildProviders(cfg)
	require.NoError(t, err)

	assert.Equal(t, LocalContextWindow, windows["qwen3-coder"],
		"a loopback base_url must keep the cloud catalog entry out")
	assert.Equal(t, 262_144, windows["qwen3-max"],
		"the same-vendor CLOUD provider must still get its catalog entry")
}

// TestBuildProviders_ExplicitLocalOverride proves the escape hatch in both
// directions: `local: true` keeps the catalog out even for a cloud-looking
// URL, and `local: false` opts a LAN-addressed gateway back in.
func TestBuildProviders_ExplicitLocalOverride(t *testing.T) {
	yes, no := true, false
	cfg := &config.Config{}
	cfg.LLM.Providers = []config.ProviderConfig{
		{Name: "proxied-local", Kind: "openai", Model: "gpt-4o", APIKey: "k", BaseURL: "https://models.corp.example", Local: &yes},
		{Name: "lan-gateway", Kind: "openai", Model: "claude-sonnet-5", APIKey: "k", BaseURL: "http://10.1.2.3:8080/v1", Local: &no},
	}

	_, _, windows, _, err := BuildProviders(cfg)
	require.NoError(t, err)

	assert.Equal(t, LocalContextWindow, windows["gpt-4o"])
	assert.Equal(t, 200_000, windows["claude-sonnet-5"])
}

// TestProviderShape_MapsEveryResolutionInput guards the single conversion
// point. ProviderShape names exactly the fields resolution reads; if a field is
// added there and not mapped in providerShape, resolution silently ignores the
// operator's configuration — the "written but unread" failure mode.
func TestProviderShape_MapsEveryResolutionInput(t *testing.T) {
	local := true
	p := config.ProviderConfig{
		Kind:          "ollama",
		Model:         "qwen3-coder",
		BaseURL:       "http://127.0.0.1:11434",
		ContextWindow: 4096,
		Local:         &local,
	}
	got := providerShape(p)
	assert.Equal(t, ProviderShape{
		Kind:          "ollama",
		Model:         "qwen3-coder",
		BaseURL:       "http://127.0.0.1:11434",
		ContextWindow: 4096,
		Local:         &local,
	}, got)
}

// TestAnthropicGenerationParams is the M4 wire assertion for the Anthropic
// adapter: the configured values reach the request body, and — the part a
// value type would have broken — an explicit ZERO temperature is transmitted
// rather than dropped as if unset.
func TestAnthropicGenerationParams(t *testing.T) {
	cases := []struct {
		name            string
		cfg             AnthropicModelConfig
		wantMaxTokens   float64
		wantTemperature any
		wantTopP        any
	}{
		{
			name:            "unset params are omitted entirely",
			cfg:             AnthropicModelConfig{},
			wantMaxTokens:   4096,
			wantTemperature: nil,
			wantTopP:        nil,
		},
		{
			name:            "configured params reach the wire",
			cfg:             AnthropicModelConfig{MaxTokens: 32000, Temperature: f32Ptr(0.7), TopP: f32Ptr(0.95)},
			wantMaxTokens:   32000,
			wantTemperature: 0.7,
			wantTopP:        0.95,
		},
		{
			name:            "an explicit zero temperature is transmitted, not dropped",
			cfg:             AnthropicModelConfig{Temperature: f32Ptr(0)},
			wantMaxTokens:   4096,
			wantTemperature: float64(0),
			wantTopP:        nil,
		},
		{
			name:            "an explicit zero top_p is transmitted, not dropped",
			cfg:             AnthropicModelConfig{TopP: f32Ptr(0)},
			wantMaxTokens:   4096,
			wantTemperature: nil,
			wantTopP:        float64(0),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := captureAnthropicBody(t, tc.cfg)
			assert.Equal(t, tc.wantMaxTokens, body["max_tokens"])
			assertJSONField(t, body, "temperature", tc.wantTemperature)
			assertJSONField(t, body, "top_p", tc.wantTopP)
		})
	}
}

// TestResponsesGenerationParams is the M4 wire assertion for the Responses
// adapter. Same shape as the Anthropic table; the field is max_output_tokens
// here because the Responses API renamed it.
func TestResponsesGenerationParams(t *testing.T) {
	cases := []struct {
		name            string
		cfg             ResponsesConfig
		wantMaxTokens   float64
		wantTemperature any
		wantTopP        any
	}{
		{
			name:            "unset params are omitted entirely",
			cfg:             ResponsesConfig{},
			wantMaxTokens:   4096,
			wantTemperature: nil,
			wantTopP:        nil,
		},
		{
			name:            "configured params reach the wire",
			cfg:             ResponsesConfig{MaxOutputTokens: 64000, Temperature: f32Ptr(0.2), TopP: f32Ptr(0.5)},
			wantMaxTokens:   64000,
			wantTemperature: 0.2,
			wantTopP:        0.5,
		},
		{
			name:            "an explicit zero temperature is transmitted, not dropped",
			cfg:             ResponsesConfig{Temperature: f32Ptr(0)},
			wantMaxTokens:   4096,
			wantTemperature: float64(0),
			wantTopP:        nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := captureResponsesBody(t, tc.cfg)
			assert.Equal(t, tc.wantMaxTokens, body["max_output_tokens"])
			assertJSONField(t, body, "temperature", tc.wantTemperature)
			assertJSONField(t, body, "top_p", tc.wantTopP)
		})
	}
}

// assertJSONField asserts a field is absent when want is nil, or present with
// the given value otherwise. Absence is the assertion that matters for the
// "unset stays off the wire" rows — a present-but-zero field would change the
// request for every provider that never configured it.
func assertJSONField(t *testing.T, body map[string]any, key string, want any) {
	t.Helper()
	got, present := body[key]
	if want == nil {
		assert.False(t, present, "%s must be omitted when unset, got %v", key, got)
		return
	}
	require.True(t, present, "%s must be present", key)
	assert.InDelta(t, want, got, 1e-6)
}

// captureAnthropicBody runs one Generate against a stub server and returns the
// decoded request body.
func captureAnthropicBody(t *testing.T, cfg AnthropicModelConfig) map[string]any {
	t.Helper()
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`))
	}))
	defer srv.Close()

	cfg.APIKey = "k"
	cfg.Model = "claude-sonnet-5"
	cfg.BaseURL = srv.URL
	m, err := NewAnthropicModel(context.Background(), &cfg)
	require.NoError(t, err)

	_, err = m.Generate(context.Background(), []*schema.Message{schema.UserMessage("hi")})
	require.NoError(t, err)
	return body
}

// captureResponsesBody runs one Generate against a stub server and returns the
// decoded request body.
func captureResponsesBody(t *testing.T, cfg ResponsesConfig) map[string]any {
	t.Helper()
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`))
	}))
	defer srv.Close()

	cfg.APIKey = "k"
	cfg.Model = "gpt-4o"
	cfg.BaseURL = srv.URL
	m, err := NewOpenAIResponsesModel(context.Background(), &cfg)
	require.NoError(t, err)

	_, err = m.Generate(context.Background(), []*schema.Message{schema.UserMessage("hi")})
	require.NoError(t, err)
	return body
}

// TestBuildOne_ForwardsGenerationParams proves the config→adapter hop actually
// happens for every kind. A field can be declared in config, honoured by the
// adapter, and still do nothing because nobody copied it across — which is the
// dominant defect shape in this repo, so it gets its own assertion rather than
// being assumed from the two wire tests above.
func TestBuildOne_ForwardsGenerationParams(t *testing.T) {
	t.Run("anthropic", func(t *testing.T) {
		m, err := buildOne(context.Background(), config.ProviderConfig{
			Kind: "anthropic", Model: "claude-sonnet-5", APIKey: "k",
			MaxTokens: intPtr(32000), Temperature: f32Ptr(0.1), TopP: f32Ptr(0.9),
		}, nil)
		require.NoError(t, err)
		am, ok := m.(*AnthropicModel)
		require.True(t, ok)
		assert.Equal(t, 32000, am.config.MaxTokens)
		require.NotNil(t, am.config.Temperature)
		assert.InDelta(t, 0.1, *am.config.Temperature, 1e-6)
		require.NotNil(t, am.config.TopP)
		assert.InDelta(t, 0.9, *am.config.TopP, 1e-6)
	})

	t.Run("openai-responses", func(t *testing.T) {
		m, err := buildOne(context.Background(), config.ProviderConfig{
			Kind: "openai-responses", Model: "gpt-4o", APIKey: "k",
			MaxTokens: intPtr(64000), Temperature: f32Ptr(0), TopP: f32Ptr(0.5),
		}, nil)
		require.NoError(t, err)
		rm, ok := m.(*openaiResponsesModel)
		require.True(t, ok)
		assert.Equal(t, 64000, rm.cfg.MaxOutputTokens)
		require.NotNil(t, rm.cfg.Temperature, "an explicit zero must survive the hop")
		assert.InDelta(t, 0, *rm.cfg.Temperature, 1e-6)
		require.NotNil(t, rm.cfg.TopP)
		assert.InDelta(t, 0.5, *rm.cfg.TopP, 1e-6)
	})

	t.Run("anthropic with unset params keeps the adapter default", func(t *testing.T) {
		m, err := buildOne(context.Background(), config.ProviderConfig{
			Kind: "anthropic", Model: "claude-sonnet-5", APIKey: "k",
		}, nil)
		require.NoError(t, err)
		am := m.(*AnthropicModel)
		assert.Equal(t, 4096, am.config.MaxTokens)
		assert.Nil(t, am.config.Temperature)
		assert.Nil(t, am.config.TopP)
	})
}

// TestDerefOr covers the nil/value helper directly, including the case that
// motivates it: a configured ZERO must still be returned as zero rather than
// being confused with the "use the default" signal, which is the adapter
// config's own convention for MaxTokens.
func TestDerefOr(t *testing.T) {
	assert.Equal(t, 7, derefOr(intPtr(7), 3))
	assert.Equal(t, 3, derefOr((*int)(nil), 3))
	assert.Equal(t, 0, derefOr(intPtr(0), 3))
	assert.InDelta(t, float32(0.5), derefOr(f32Ptr(0.5), float32(1)), 1e-6)
}

// TestAdapterErrorsCarryResponseHeaders is the M1 supply-side assertion: the
// adapters must wrap their non-200 errors so the retry layer can read
// Retry-After from the authoritative header. Without this wrapping the header
// is lost at the fmt.Errorf boundary and only the text fallback remains.
func TestAdapterErrorsCarryResponseHeaders(t *testing.T) {
	t.Run("anthropic", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "37")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"type":"rate_limit_error"}}`))
		}))
		defer srv.Close()

		m, err := NewAnthropicModel(context.Background(), &AnthropicModelConfig{
			APIKey: "k", Model: "claude-sonnet-5", BaseURL: srv.URL,
		})
		require.NoError(t, err)
		_, genErr := m.Generate(context.Background(), []*schema.Message{schema.UserMessage("hi")})
		require.Error(t, genErr)

		got := ClassifyError(genErr)
		assert.Equal(t, ClassRateLimit, got.Class)
		assert.Equal(t, 429, got.Status)
		assert.Equal(t, 37*1e9, float64(got.RetryAfter), "Retry-After must survive to the retry layer")
	})

	t.Run("responses", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "12")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"code":"rate_limit_exceeded"}}`))
		}))
		defer srv.Close()

		m, err := NewOpenAIResponsesModel(context.Background(), &ResponsesConfig{
			APIKey: "k", Model: "gpt-4o", BaseURL: srv.URL,
		})
		require.NoError(t, err)
		_, genErr := m.Generate(context.Background(), []*schema.Message{schema.UserMessage("hi")})
		require.Error(t, genErr)

		got := ClassifyError(genErr)
		assert.Equal(t, ClassRateLimit, got.Class)
		assert.Equal(t, 12*1e9, float64(got.RetryAfter))
	})
}
