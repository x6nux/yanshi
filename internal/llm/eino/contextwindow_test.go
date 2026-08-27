package eino

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestKnownContextWindow_PenetratesRoutingPrefixes is the M2 core case: the
// catalog is keyed on model-id FRAGMENTS matched at a word boundary, so a
// single row answers for the same model no matter which gateway prefix the
// operator typed. Written as a table because the prefix zoo is the feature.
func TestKnownContextWindow_PenetratesRoutingPrefixes(t *testing.T) {
	cases := []struct {
		name   string
		model  string
		tokens int
	}{
		{"bare anthropic id", "claude-sonnet-5", 200_000},
		{"openrouter two-level prefix", "openrouter/anthropic/claude-opus-4-8", 200_000},
		{"openrouter one-level prefix", "anthropic/claude-sonnet-5", 200_000},
		{"bedrock dotted region prefix", "us.anthropic.claude-sonnet-5-20250101-v1:0", 200_000},
		{"bedrock eu region prefix", "eu.anthropic.claude-opus-4-8", 200_000},
		{"azure slash prefix", "azure/gpt-4o", 128_000},
		{"vertex colon prefix", "vertex:gemini-1.5-pro", 2_097_152},
		{"uppercase id", "ANTHROPIC/CLAUDE-SONNET-5", 200_000},
		{"surrounding whitespace", "  claude-sonnet-5  ", 200_000},
		{"deployment suffix", "gpt-4o-2024-11-20", 128_000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := KnownContextWindow(tc.model)
			require.True(t, ok, "catalog should resolve %q", tc.model)
			assert.Equal(t, tc.tokens, got)
		})
	}
}

// TestKnownContextWindow_LongestPatternWins proves a specific row beats its
// family catch-all regardless of where either sits in the catalog literal.
// Without the length sort, "claude" (200K) would shadow "claude-2" (100K) and a
// legacy Bedrock deployment would compact against twice its real window.
func TestKnownContextWindow_LongestPatternWins(t *testing.T) {
	cases := []struct {
		model  string
		tokens int
	}{
		{"claude-2.1", 100_000},
		{"claude-instant-1.2", 100_000},
		{"claude-sonnet-5", 200_000},
		{"qwen-plus-latest", 1_000_000},
		{"qwen-plus", 131_072},
		{"qwen3-coder-plus", 1_000_000},
		{"qwen3-coder-30b", 262_144},
		{"gemini-1.5-pro-002", 2_097_152},
		{"gemini-2.5-flash", 1_048_576},
		{"grok-4-fast-reasoning", 2_000_000},
		{"grok-4", 256_000},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			got, ok := KnownContextWindow(tc.model)
			require.True(t, ok)
			assert.Equal(t, tc.tokens, got)
		})
	}
}

// TestKnownContextWindow_BoundaryRuleRejectsGluedMatches proves matching is
// anchored at a word boundary rather than being a bare substring search. Both
// halves matter: "o3" inside "custom-4o3x" must NOT match (that is the false
// positive a bare Contains would produce), while a "-" or "/" separator must.
//
// The negative case deliberately avoids a "gpt-" prefix: "gpt-4" is itself a
// catalog row, so "gpt-4o3x" resolves through THAT row and would prove nothing
// about the boundary rule.
func TestKnownContextWindow_BoundaryRuleRejectsGluedMatches(t *testing.T) {
	cases := []struct {
		name  string
		model string
		want  bool
	}{
		{"glued alphanumeric prefix is not a boundary", "custom-4o3x", false},
		{"dash is a boundary", "openai-o3-mini", true},
		{"slash is a boundary", "openai/o3", true},
		{"start of string is a boundary", "o3", true},
		{"empty id resolves nothing", "", false},
		{"whitespace-only id resolves nothing", "   ", false},
		{"unknown family resolves nothing", "some-inhouse-model-v7", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ok := KnownContextWindow(tc.model)
			assert.Equal(t, tc.want, ok)
		})
	}
}

// boolPtr is a local helper for the *bool override field.
func boolPtr(b bool) *bool { return &b }

// TestIsLocalProvider covers M3's detection surface: explicit override, kind
// names, and the URL heuristic (loopback, private ranges, LAN hostnames).
func TestIsLocalProvider(t *testing.T) {
	cases := []struct {
		name string
		p    ProviderShape
		want bool
	}{
		{"explicit local true overrides a cloud URL", ProviderShape{BaseURL: "https://api.openai.com", Local: boolPtr(true)}, true},
		{"explicit local false overrides a LAN URL", ProviderShape{BaseURL: "http://192.168.1.10:8000/v1", Local: boolPtr(false)}, false},
		{"kind ollama", ProviderShape{Kind: "ollama"}, true},
		{"kind lmstudio", ProviderShape{Kind: "LMStudio"}, true},
		{"kind vllm", ProviderShape{Kind: " vllm "}, true},
		{"kind llama.cpp", ProviderShape{Kind: "llama.cpp"}, true},
		{"loopback ipv4", ProviderShape{BaseURL: "http://127.0.0.1:11434/v1"}, true},
		{"loopback ipv4 non-standard octet", ProviderShape{BaseURL: "http://127.5.5.5:8080"}, true},
		{"loopback ipv6", ProviderShape{BaseURL: "http://[::1]:1234/v1"}, true},
		{"localhost hostname", ProviderShape{BaseURL: "http://localhost:8000/v1"}, true},
		{"host.docker.internal", ProviderShape{BaseURL: "http://host.docker.internal:11434"}, true},
		{"rfc1918 10/8", ProviderShape{BaseURL: "http://10.0.0.5:8000"}, true},
		{"rfc1918 172.16/12", ProviderShape{BaseURL: "http://172.20.3.4:8000"}, true},
		{"rfc1918 192.168/16", ProviderShape{BaseURL: "http://192.168.0.42:1234/v1"}, true},
		{"link-local 169.254/16", ProviderShape{BaseURL: "http://169.254.1.1:8000"}, true},
		{"mdns .local suffix", ProviderShape{BaseURL: "http://workstation.local:11434"}, true},
		{"bare host:port without scheme", ProviderShape{BaseURL: "127.0.0.1:11434"}, true},
		{"public ip is not local", ProviderShape{BaseURL: "http://8.8.8.8:8000"}, false},
		{"172.32 is outside the rfc1918 block", ProviderShape{BaseURL: "http://172.32.0.1:8000"}, false},
		{"cloud endpoint", ProviderShape{BaseURL: "https://api.anthropic.com"}, false},
		{"empty base url is cloud by construction", ProviderShape{Kind: "openai"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, IsLocalProvider(tc.p))
		})
	}
}

// TestResolveContextWindow covers the full precedence ladder, including the M3
// rule that makes this more than a lookup: a local provider must NOT inherit
// the cloud catalog entry for the same model family. The "local qwen3-coder"
// row is the concrete accident being prevented — 262144 would put the
// compaction threshold above what a default Ollama serve accepts, so
// compaction never fires and the server drops the prompt head instead.
func TestResolveContextWindow(t *testing.T) {
	cases := []struct {
		name     string
		p        ProviderShape
		fallback int
		want     int
		source   ContextWindowSource
	}{
		{
			name:   "explicit config wins over the catalog",
			p:      ProviderShape{Model: "claude-sonnet-5", ContextWindow: 1_000_000},
			want:   1_000_000,
			source: WindowFromConfig,
		},
		{
			name:   "explicit config wins for a local provider too",
			p:      ProviderShape{Kind: "ollama", Model: "qwen3-coder:30b", ContextWindow: 65_536},
			want:   65_536,
			source: WindowFromConfig,
		},
		{
			name:   "catalog answers for a cloud provider",
			p:      ProviderShape{Model: "openrouter/anthropic/claude-opus-4-8"},
			want:   200_000,
			source: WindowFromCatalog,
		},
		{
			name:   "local provider does NOT inherit the cloud catalog entry",
			p:      ProviderShape{Kind: "ollama", Model: "qwen3-coder:30b"},
			want:   LocalContextWindow,
			source: WindowFromLocalDefault,
		},
		{
			name:   "loopback base_url makes an openai-kind provider local",
			p:      ProviderShape{Kind: "openai", Model: "gpt-4o", BaseURL: "http://127.0.0.1:1234/v1"},
			want:   LocalContextWindow,
			source: WindowFromLocalDefault,
		},
		{
			name:     "local ignores the caller fallback, which is a cloud-sized number",
			p:        ProviderShape{Kind: "ollama", Model: "llama-3.3-70b"},
			fallback: 256_000,
			want:     LocalContextWindow,
			source:   WindowFromLocalDefault,
		},
		{
			name:     "unknown cloud model uses the caller fallback",
			p:        ProviderShape{Model: "inhouse-model-v7"},
			fallback: 200_000,
			want:     200_000,
			source:   WindowFromFallback,
		},
		{
			name:   "unknown cloud model with no fallback uses the conservative default",
			p:      ProviderShape{Model: "inhouse-model-v7"},
			want:   DefaultContextWindow,
			source: WindowFromDefault,
		},
		{
			name:   "empty model with no fallback uses the conservative default",
			p:      ProviderShape{},
			want:   DefaultContextWindow,
			source: WindowFromDefault,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, src := ResolveContextWindow(tc.p, tc.fallback)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.source, src)
		})
	}
}

// TestDefaultsAreConservative pins the direction of the two fallbacks rather
// than their exact values. The asymmetry argument in contextwindow.go's file
// comment is the whole reason both constants are small: guessing high disables
// compaction and kills the turn, guessing low costs one extra summarization.
// If someone raises either constant to "match the modern frontier", this fails
// and points them at that argument.
func TestDefaultsAreConservative(t *testing.T) {
	assert.LessOrEqual(t, DefaultContextWindow, 128*1024,
		"the global default must under-guess; see contextwindow.go on why high guesses are the dangerous direction")
	assert.Less(t, LocalContextWindow, DefaultContextWindow,
		"a local runtime serves a smaller window than any cloud default")
	assert.LessOrEqual(t, LocalContextWindow, 32*1024,
		"local default must stay at or below a common llama.cpp/Ollama num_ctx")
}

// TestCatalogCoversMajorFamilies asserts the catalog is actually broad enough
// to do its job. The M2 brief asked for 30+ families; a table that only knew
// three would pass every other test here while leaving most real configs on the
// fallback path.
func TestCatalogCoversMajorFamilies(t *testing.T) {
	require.GreaterOrEqual(t, len(contextWindowCatalog), 30,
		"catalog must cover the major families; a thin table silently sends most models to the fallback")

	// Every vendor the repo can plausibly be pointed at should resolve.
	for _, id := range []string{
		"claude-sonnet-5", "gpt-4o", "gpt-5", "o3", "gemini-2.0-flash",
		"qwen3-max", "deepseek-chat", "kimi-k2", "glm-4.6", "minimax-m3",
		"grok-4", "mistral-large-latest", "command-r-plus", "llama-3.3-70b",
		"codestral-latest",
	} {
		_, ok := KnownContextWindow(id)
		assert.True(t, ok, "catalog should know %q", id)
	}
}

// TestCatalogEntriesAreSane guards the table itself: a zero or negative window
// would make ResolveContextWindow return a window compaction cannot divide
// against, and a duplicated pattern means one of the two rows is dead code that
// can never be reached (the sort makes the tie order unstable).
func TestCatalogEntriesAreSane(t *testing.T) {
	seen := make(map[string]bool, len(contextWindowCatalog))
	for _, e := range contextWindowCatalog {
		assert.NotEmpty(t, e.pattern, "empty pattern would match every model")
		assert.Equal(t, e.pattern, strings.ToLower(e.pattern),
			"patterns are matched against a lowercased id, so an uppercase pattern is dead")
		assert.Greater(t, e.tokens, 0, "pattern %q has a non-positive window", e.pattern)
		assert.False(t, seen[e.pattern], "duplicate pattern %q: one of the rows is unreachable", e.pattern)
		seen[e.pattern] = true
	}
}

// TestSortedByPatternLengthIsDescending pins the invariant the longest-wins
// rule depends on, independently of any particular catalog content.
func TestSortedByPatternLengthIsDescending(t *testing.T) {
	for i := 1; i < len(contextWindowPatterns); i++ {
		assert.GreaterOrEqual(t,
			len(contextWindowPatterns[i-1].pattern),
			len(contextWindowPatterns[i].pattern),
			"patterns must be sorted longest-first or a family catch-all can shadow a specific row")
	}
	assert.Len(t, contextWindowPatterns, len(contextWindowCatalog),
		"the sort must not drop or duplicate rows")
}
