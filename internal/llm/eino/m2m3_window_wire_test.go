// internal/llm/eino/m2m3_window_wire_test.go
//
// M2 (static context-window catalog) and M3 (local providers are not given a
// cloud model's window), tested through the object production actually uses:
// the `windows` map BuildProviders returns.
//
// contextwindow_test.go tests ResolveContextWindow directly, which is the right
// place for the resolution rules. What it cannot see is whether the resolved
// value reaches the map the rest of the system reads — and that seam has failed
// before. BuildProviders' own doc comment records the historical bug: the
// windows map was once rebuilt by provider NAME while every lookup used the
// model-id registry key, so the per-model window was dead for every provider
// and nothing said so.
//
// So the assertions here are all of the form "configure a provider the way an
// operator would, run BuildProviders, and look up the window the way the http
// layer looks it up".
//
// Why the window mattering is not obvious: it is the denominator of the
// compaction threshold. Resolve it too HIGH and compaction never fires — the
// prompt grows past the real limit and the turn dies, or worse the server
// silently drops the head of the prompt (taking the system prompt and the tool
// definitions with it) and the session appears to work while the model can no
// longer see its instructions. That silent-truncation case is exactly what M3
// exists for.
package eino

import (
	"testing"

	"github.com/x6nux/yanshi/internal/config"
)

// windowFor builds a single-provider config and returns the window
// BuildProviders resolved for it, under the registry key the http layer would
// use.
func windowFor(t *testing.T, p config.ProviderConfig) int {
	t.Helper()
	cfg := &config.Config{}
	cfg.LLM.Providers = []config.ProviderConfig{p}
	_, _, windows, _, _, err := BuildProviders(cfg)
	if err != nil {
		t.Fatalf("BuildProviders(%+v): %v", p, err)
	}
	key := p.Model
	if key == "" {
		key = p.Name
	}
	w, ok := windows[key]
	if !ok {
		t.Fatalf("no window under registry key %q; windows=%v — a missing key is the historical "+
			"bug shape: every lookup falls back to the global default and nothing reports it",
			key, windows)
	}
	return w
}

// TestM2_RoutingPrefixesResolveToTheRealFamilyWindow is the catalog's reason
// for existing. Operators do not type bare model ids: they type whatever their
// gateway calls the model, and the same Claude appears as
// "anthropic/claude-...", "us.anthropic.claude-...", or with an "openrouter/"
// on the front.
//
// A catalog that only matched bare ids would answer the conservative fallback
// for all of them, which is the safe direction but throws away most of the
// value; a catalog that matched too loosely would answer confidently and
// wrongly, which is the dangerous direction.
func TestM2_RoutingPrefixesResolveToTheRealFamilyWindow(t *testing.T) {
	cases := []struct {
		model string
		want  int
	}{
		{"claude-opus-4-8", 200_000},
		{"anthropic/claude-sonnet-5", 200_000},
		{"openrouter/anthropic/claude-opus-4-8", 200_000},
		{"us.anthropic.claude-sonnet-5-v1:0", 200_000},
		{"azure/gpt-4o", 128_000},
		{"gpt-4o", 128_000},
		{"gpt-4o-mini", 128_000},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			got := windowFor(t, config.ProviderConfig{
				Name: "p", Kind: "openai", Model: tc.model, APIKey: "k",
				BaseURL: "https://gateway.example.com/v1",
			})
			t.Logf("%s → %d tokens", tc.model, got)
			if got != tc.want {
				t.Errorf("window = %d, want %d: the routing prefix was not penetrated, so this "+
					"model's compaction threshold is computed against the wrong capacity", got, tc.want)
			}
		})
	}
}

// TestM2_LegacyFamilyIsNotShadowedByItsCatchAll pins the longest-match rule.
// "claude-2" is a 100K legacy family and "claude" is the 200K catch-all; if
// order or length ranking regressed, a claude-2 deployment would size against
// twice its real window and compaction would never fire.
func TestM2_LegacyFamilyIsNotShadowedByItsCatchAll(t *testing.T) {
	got := windowFor(t, config.ProviderConfig{
		Name: "legacy", Kind: "openai", Model: "claude-2.1", APIKey: "k",
		BaseURL: "https://gateway.example.com/v1",
	})
	t.Logf("claude-2.1 → %d tokens", got)
	if got != 100_000 {
		t.Errorf("window = %d, want 100000: the specific legacy row lost to the family catch-all", got)
	}
}

// TestM2_UnknownModelFallsBackConservatively pins the asymmetry argument. A
// model nothing recognises must resolve DOWNWARD: guessing low costs one extra
// compaction, guessing high costs the turn.
func TestM2_UnknownModelFallsBackConservatively(t *testing.T) {
	got := windowFor(t, config.ProviderConfig{
		Name: "mystery", Kind: "openai", Model: "totally-new-model-9000", APIKey: "k",
		BaseURL: "https://gateway.example.com/v1",
	})
	t.Logf("unknown model → %d tokens", got)
	if got != DefaultContextWindow {
		t.Errorf("window = %d, want the conservative default %d", got, DefaultContextWindow)
	}
	if got > 200_000 {
		t.Errorf("the fallback %d is large enough to disable compaction on most models", got)
	}
}

// TestM3_LocalProvidersDoNotInheritCloudWindows is M3, and it is the test with
// the nastiest failure mode behind it.
//
// `ollama run qwen3-coder:30b` serves a num_ctx of a few thousand tokens
// regardless of the family's 262K cloud figure, and it does NOT reject an
// over-long prompt — it drops the head, taking the system prompt and the tool
// definitions with it. The session then appears to work while the model can no
// longer see its own instructions, which is far harder to diagnose than an
// error would be.
//
// The model id below is deliberately one the catalog KNOWS (qwen3-coder,
// 262144). The whole point is that a local base_url must override a confident
// catalog hit.
func TestM3_LocalProvidersDoNotInheritCloudWindows(t *testing.T) {
	cloud := windowFor(t, config.ProviderConfig{
		Name: "cloud", Kind: "openai", Model: "qwen3-coder", APIKey: "k",
		BaseURL: "https://dashscope.example.com/v1",
	})
	t.Logf("same model id via a cloud endpoint → %d tokens", cloud)
	if cloud != 262_144 {
		t.Fatalf("catalog hit = %d, want 262144; without a confident cloud answer this test "+
			"cannot show that local OVERRIDES one", cloud)
	}

	for _, baseURL := range []string{
		"http://127.0.0.1:11434/v1",   // loopback (ollama default)
		"http://localhost:1234/v1",    // loopback by name (LM Studio)
		"http://192.168.1.50:8000/v1", // RFC1918
		"http://10.0.0.7:8000/v1",     // RFC1918
		"http://172.16.3.9:8000/v1",   // RFC1918
		"http://workstation.local/v1", // mDNS
	} {
		t.Run(baseURL, func(t *testing.T) {
			got := windowFor(t, config.ProviderConfig{
				Name: "local", Kind: "openai", Model: "qwen3-coder", APIKey: "k", BaseURL: baseURL,
			})
			t.Logf("%s → %d tokens", baseURL, got)
			if got == cloud {
				t.Errorf("a local runtime was given the cloud window %d; compaction will never "+
					"fire and the server will silently truncate the prompt head", got)
			}
			if got != LocalContextWindow {
				t.Errorf("window = %d, want LocalContextWindow %d", got, LocalContextWindow)
			}
		})
	}
}

// TestM3_ExplicitWindowBeatsEverything pins the escape hatch. An operator who
// raised num_ctx must be able to say so, and their number must beat both the
// catalog and the local heuristic — otherwise the safe default becomes a
// ceiling nobody can lift.
func TestM3_ExplicitWindowBeatsEverything(t *testing.T) {
	got := windowFor(t, config.ProviderConfig{
		Name: "local", Kind: "openai", Model: "qwen3-coder", APIKey: "k",
		BaseURL: "http://127.0.0.1:11434/v1", ContextWindow: 65536,
	})
	t.Logf("local + explicit context_window → %d tokens", got)
	if got != 65536 {
		t.Errorf("window = %d, want the operator's explicit 65536", got)
	}
}

// TestM3_LocalFalseOptsALANGatewayBackIntoTheCatalog covers the other
// direction: a real cloud-grade gateway hosted on the LAN would otherwise be
// mistaken for a local runtime and throttled to 8K.
func TestM3_LocalFalseOptsALANGatewayBackIntoTheCatalog(t *testing.T) {
	no := false
	got := windowFor(t, config.ProviderConfig{
		Name: "lan-gw", Kind: "openai", Model: "gpt-4o", APIKey: "k",
		BaseURL: "http://192.168.1.10:8080/v1", Local: &no,
	})
	t.Logf("LAN gateway with local:false → %d tokens", got)
	if got != 128_000 {
		t.Errorf("window = %d, want the catalog's 128000: local:false must re-enable the catalog", got)
	}
}

// TestM2_WindowsMapIsKeyedLikeTheRegistry is the anti-regression test for the
// historical bug named in BuildProviders' doc comment.
//
// Two providers, the second with no model id, so the key derivation has to fall
// back to the config name. Both keys must be present, because a lookup miss is
// silent: the caller falls back to the global window and the per-model value
// simply never applies.
func TestM2_WindowsMapIsKeyedLikeTheRegistry(t *testing.T) {
	cfg := &config.Config{}
	cfg.LLM.Providers = []config.ProviderConfig{
		{Name: "first", Kind: "openai", Model: "gpt-4o", APIKey: "k", BaseURL: "https://a.example/v1"},
		{Name: "second", Kind: "openai", Model: "", APIKey: "k", BaseURL: "https://b.example/v1"},
	}
	named, _, windows, _, _, err := BuildProviders(cfg)
	if err != nil {
		t.Fatalf("BuildProviders: %v", err)
	}
	t.Logf("registry keys=%v windows=%v", SortedModelNames(named), windows)
	for key := range named {
		if _, ok := windows[key]; !ok {
			t.Errorf("registry key %q has no window entry; every lookup for it silently falls "+
				"back to the global default", key)
		}
	}
	if len(windows) != len(named) {
		t.Errorf("windows has %d entries for %d models; the map must be total", len(windows), len(named))
	}
}
