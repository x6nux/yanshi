package http

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestContextWindowFor_UsesRegistryKey locks in the C1 fix:
// ProviderWindows is keyed by the REGISTRY key (p.Model, e.g. "gpt-4o"), not
// p.Name ("openai"). A config with Name != Model — the typical production
// shape — must still hit the per-model window when queried by the registry
// key, because that is what the WS / SSE handlers pass (cs.model / req.Model,
// which is the model id selected via /model or sent by the client). The
// historical bug was bootstrap rebuilding the windows map by p.Name, which
// never matched the registry's model-id keys and silently disabled the
// per-model window feature (always fell back to ContextWindow).
func TestContextWindowFor_UsesRegistryKey(t *testing.T) {
	cc := CompactionConfig{
		ContextWindow:   999,                              // fallback — must NOT be returned for keyed models
		ProviderWindows: map[string]int{"gpt-4o": 128000}, // keyed by Model id
	}
	assert.Equal(t, 128000, contextWindowFor("gpt-4o", cc),
		"registry key (Model id) hits per-model window")
	assert.Equal(t, 999, contextWindowFor("openai", cc),
		"provider Name does NOT hit (keyed by Model)")
	assert.Equal(t, 999, contextWindowFor("unknown", cc),
		"unknown model falls back to ContextWindow")
}

// TestContextWindowFor_EmptyMapFallback locks the fallback behavior so the
// FakeModel / no-providers path (providerWindows == nil) keeps compaction
// working with the global ContextWindow.
func TestContextWindowFor_EmptyMapFallback(t *testing.T) {
	cc := CompactionConfig{
		ContextWindow:   256000,
		ProviderWindows: nil,
	}
	assert.Equal(t, 256000, contextWindowFor("gpt-4o", cc))
	assert.Equal(t, 256000, contextWindowFor("anything", cc))
}

// TestContextWindowFor_ZeroEntryIgnored ensures a zero (mis-configured) entry
// does not shadow the fallback — contextWindowFor must skip it and fall through.
func TestContextWindowFor_ZeroEntryIgnored(t *testing.T) {
	cc := CompactionConfig{
		ContextWindow:   1000,
		ProviderWindows: map[string]int{"bad": 0},
	}
	assert.Equal(t, 1000, contextWindowFor("bad", cc),
		"a zero ProviderWindows entry must NOT shadow the fallback")
}

// TestThresholdFor_UsesRegistryKey mirrors TestContextWindowFor_UsesRegistryKey
// for the W-C-01 (INF2) per-model auto-compact threshold: ProviderThresholds is
// keyed by the registry key (cs.model / req.Model), exactly like ProviderWindows,
// so the two ladders cannot silently diverge on which key they read.
func TestThresholdFor_UsesRegistryKey(t *testing.T) {
	cc := CompactionConfig{
		Threshold:          0.8,                                // fallback — must NOT be returned for keyed models
		ProviderThresholds: map[string]float64{"gpt-4o": 0.55}, // keyed by Model id
	}
	assert.Equal(t, 0.55, thresholdFor("gpt-4o", cc),
		"registry key (Model id) hits the per-model threshold")
	assert.Equal(t, 0.8, thresholdFor("openai", cc),
		"provider Name does NOT hit (keyed by Model)")
	assert.Equal(t, 0.8, thresholdFor("unknown", cc),
		"unknown model falls back to the global Threshold")
}

// TestThresholdFor_EmptyMapFallback locks the fallback behavior so the
// FakeModel / no-providers path (ProviderThresholds == nil) keeps compaction
// working with the global Threshold.
func TestThresholdFor_EmptyMapFallback(t *testing.T) {
	cc := CompactionConfig{
		Threshold:          0.9,
		ProviderThresholds: nil,
	}
	assert.Equal(t, 0.9, thresholdFor("gpt-4o", cc))
	assert.Equal(t, 0.9, thresholdFor("anything", cc))
}

// TestThresholdFor_ZeroEntryIgnored ensures a zero (mis-configured, or a
// catalog row that never populated auto_compact_threshold) entry does not
// shadow the fallback — thresholdFor must skip it and fall through.
func TestThresholdFor_ZeroEntryIgnored(t *testing.T) {
	cc := CompactionConfig{
		Threshold:          0.7,
		ProviderThresholds: map[string]float64{"bad": 0},
	}
	assert.Equal(t, 0.7, thresholdFor("bad", cc),
		"a zero ProviderThresholds entry must NOT shadow the fallback")
}
