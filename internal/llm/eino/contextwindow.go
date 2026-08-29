package eino

import (
	"net"
	"net/url"
	"strings"
)

// This file holds the static context-window catalog (M2) and the local-provider
// detection that gates it (M3).
//
// WHY A CATALOG EXISTS AT ALL. Compaction fires at Threshold * window. The
// window used to come from one place only — `provider.context_window`, hand
// written per provider — and an omitted key fell back to the global
// CompactionConfig.ContextWindow (256000). A 128K model configured without the
// key therefore computed its threshold against twice its real capacity, so the
// gate never opened and the request was rejected (or silently head-truncated)
// by the server instead. That is the same failure mode W4 fixed for the
// mid-turn path, arriving through configuration instead of through wiring.
//
// RESOLUTION ORDER is fixed and one-directional (see ResolveContextWindow):
//
//	explicit config > catalog > conservative fallback
//
// WHY THE FALLBACK IS SMALL. Guessing high and guessing low are not symmetric
// mistakes. Guess too high and the threshold sits above the model's real
// capacity: compaction NEVER runs, the prompt grows past the limit, and the
// turn dies (or the provider drops the head of the prompt, which is worse —
// the system prompt goes first and the failure is silent). Guess too low and
// the only cost is compacting earlier than strictly necessary: one extra
// summarization call, a slightly shorter live context, and the turn still
// completes. So every unknown resolves DOWNWARD, and every catalog entry that
// varies by snapshot records the family's safe LOWER documented bound.

// DefaultContextWindow is the fallback window (128K tokens) used when neither
// an explicit config value nor the catalog resolves a model.
//
// 128K is the smallest window shared by essentially every current frontier
// model, so it under-guesses rather than over-guesses for anything newer. See
// the file comment for why under-guessing is the safe direction.
const DefaultContextWindow = 128 * 1024

// LocalContextWindow is the fallback for locally served models (Ollama, LM
// Studio, vLLM, llama.cpp, …) — 8192 tokens, deliberately far below
// DefaultContextWindow.
//
// A local runtime does not serve the cloud model's window: `ollama run
// qwen3-coder:30b` defaults to num_ctx=4096..8192 regardless of the family's
// 262K cloud figure, and it does not reject an over-long prompt — it silently
// drops the head, taking the system prompt and the tool definitions with it.
// The result is a session that appears to work while the model can no longer
// see its own instructions. 8192 matches the common llama.cpp/Ollama default
// context, so the compaction gate fires before the server starts discarding.
// An operator who has raised num_ctx sets `context_window` explicitly, which
// wins outright.
const LocalContextWindow = 8 * 1024

// contextWindowEntry is one catalog row: a lowercase model-id fragment and the
// input window it implies.
type contextWindowEntry struct {
	pattern string
	tokens  int
}

// contextWindowCatalog maps model-id fragments to input-context windows.
//
// Matching is case-insensitive and anchored at a WORD BOUNDARY inside the id
// (see matchesAtBoundary), which is what lets a single row cover the same
// model across every routing prefix operators actually type:
// "anthropic/claude-sonnet-5" (OpenRouter), "us.anthropic.claude-sonnet-5"
// (Bedrock), "azure/gpt-4o" (Azure), "openrouter/anthropic/claude-opus-4-8".
// The LONGEST pattern wins (contextWindowPatterns sorts by length), so a
// specific row beats its family catch-all regardless of the order written
// here — "claude-2" (100K legacy) is not shadowed by "claude" (200K).
//
// Values are the safe LOWER documented bound whenever a family's window varies
// by snapshot or by opt-in header: Anthropic's 1M-context Sonnet is a beta
// header the operator opts into, so the family row stays at the standard 200K
// and the operator who enabled the beta sets `context_window` explicitly.
// W-C-01 (INF2): this used to be a Go literal here. It is now built from the
// embedded models.yaml (modelcatalog.go's buildContextWindowCatalog) so that
// adding a model is a data-file edit, not a Go code change. The values, the
// boundary-substring matching, and the longest-pattern-wins precedence are
// all unchanged -- see models.yaml for the current rows and their provenance.
var contextWindowCatalog = buildContextWindowCatalog(modelCatalog)

// contextWindowPatterns is contextWindowCatalog sorted longest-pattern-first so
// a specific row always beats its family catch-all no matter where either is
// written in the literal above. Built once at init; the catalog is immutable.
var contextWindowPatterns = sortedByPatternLength(contextWindowCatalog)

// sortedByPatternLength returns entries ordered by descending pattern length.
// Insertion sort over a few dozen rows keeps this allocation-cheap and avoids
// pulling sort into a package-level initializer for no measurable gain.
func sortedByPatternLength(entries []contextWindowEntry) []contextWindowEntry {
	out := make([]contextWindowEntry, len(entries))
	copy(out, entries)
	for i := 1; i < len(out); i++ {
		e := out[i]
		j := i - 1
		for j >= 0 && len(out[j].pattern) < len(e.pattern) {
			out[j+1] = out[j]
			j--
		}
		out[j+1] = e
	}
	return out
}

// matchesAtBoundary reports whether pattern occurs in modelID at a word
// boundary — start of string, or preceded by a non-alphanumeric byte.
//
// The boundary rule is what makes prefix penetration work without false
// positives: "/", ".", ":" and "-" are all non-alphanumeric, so "claude"
// matches "anthropic/claude-sonnet-5", "us.anthropic.claude-sonnet-5" and
// "bedrock:claude", while "o3" does NOT match "gpt-4o3x" (preceded by the
// alphanumeric "4o"). Model ids are ASCII in every provider catalog we target,
// so byte-wise inspection is equivalent to rune-wise here.
func matchesAtBoundary(modelID, pattern string) bool {
	for i := 0; ; {
		j := strings.Index(modelID[i:], pattern)
		if j < 0 {
			return false
		}
		at := i + j
		if at == 0 || !isAlnumByte(modelID[at-1]) {
			return true
		}
		i = at + 1
	}
}

// isAlnumByte reports whether b is an ASCII letter or digit.
func isAlnumByte(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// KnownContextWindow returns the cataloged input window for modelID and whether
// the catalog knew it. A false second return means "not in the table" — the
// caller falls back to DefaultContextWindow (or LocalContextWindow), it does
// NOT mean zero.
//
// Prefixes are penetrated: gateway/region/deployment prefixes are ignored
// because matching is boundary-anchored substring, not equality. See
// contextWindowCatalog.
func KnownContextWindow(modelID string) (int, bool) {
	normalized := strings.ToLower(strings.TrimSpace(modelID))
	if normalized == "" {
		return 0, false
	}
	for _, e := range contextWindowPatterns {
		if matchesAtBoundary(normalized, e.pattern) {
			return e.tokens, true
		}
	}
	return 0, false
}

// localProviderKinds are ProviderConfig.Kind values (lowercased) that name a
// local-serving runtime outright. These do not need URL inspection: whatever
// host they point at, the model is served by a runtime whose window is set by
// its own launch flags, not by the family's cloud figure.
var localProviderKinds = map[string]bool{
	"ollama":    true,
	"lmstudio":  true,
	"lm-studio": true,
	"vllm":      true,
	"llamacpp":  true,
	"llama.cpp": true,
	"localai":   true,
}

// localHostNames are exact hostnames (lowercased) that always mean "this
// machine". Hosts that merely resolve to a loopback address are NOT included:
// resolution is not available at config-load time and would make the decision
// depend on DNS state.
var localHostNames = map[string]bool{
	"localhost":            true,
	"host.docker.internal": true,
	"ollama":               true,
	"lmstudio":             true,
}

// IsLocalProvider reports whether p is served by a local/LAN runtime rather
// than a cloud API, which decides whether the static catalog applies (M3).
//
// Precedence: an explicit `local:` in config wins outright — including an
// explicit false, which opts a LAN-addressed gateway back into the catalog.
// Otherwise the heuristic answers, in order:
//
//  1. Kind names a local runtime (ollama, lmstudio, vllm, llama.cpp, localai).
//  2. BaseURL host is loopback (127.0.0.0/8, ::1), a known local hostname, or
//     a private/link-local address (RFC 1918 10/8, 172.16/12, 192.168/16, plus
//     169.254/16 and fc00::/7).
//
// A provider with no BaseURL is a cloud provider by construction (the adapter
// falls back to the vendor's public endpoint), so it is NOT local.
func IsLocalProvider(p ProviderShape) bool {
	if p.Local != nil {
		return *p.Local
	}
	if localProviderKinds[strings.ToLower(strings.TrimSpace(p.Kind))] {
		return true
	}
	return isLocalBaseURL(p.BaseURL)
}

// ProviderShape is the minimal projection of config.ProviderConfig that
// context-window resolution needs.
//
// It exists so ResolveContextWindow and IsLocalProvider stay callable from
// tests and from future callers without threading a whole config.Config, and
// so the resolution rules are stated over a struct that names exactly the four
// inputs they read — a reader can see at a glance that nothing else
// participates in the decision.
type ProviderShape struct {
	// Kind is ProviderConfig.Kind ("openai", "anthropic", "ollama", …).
	Kind string
	// Model is the model id the catalog is matched against.
	Model string
	// BaseURL is the provider endpoint; "" means the vendor default (cloud).
	BaseURL string
	// ContextWindow is the explicitly configured window; <= 0 means unset.
	ContextWindow int
	// Local overrides the local/cloud heuristic; nil means "heuristic decides".
	Local *bool
	// AutoCompactThreshold is the explicitly configured per-provider
	// auto-compact threshold (config.ProviderConfig.AutoCompactThreshold);
	// <= 0 means unset. See ResolveAutoCompactThreshold (modelcatalog.go) for
	// the resolution ladder this field participates in — it is the config
	// layer that outranks the catalog, mirroring ContextWindow's role here.
	AutoCompactThreshold float64
}

// isLocalBaseURL reports whether rawURL points at this machine or the local
// network. An unparsable URL is treated as NOT local: the conservative answer
// for an address we cannot read is to keep the larger cloud default out of the
// picture only when we are sure, and a malformed URL is more likely a typo'd
// cloud endpoint than a local runtime.
func isLocalBaseURL(rawURL string) bool {
	raw := strings.TrimSpace(rawURL)
	if raw == "" {
		return false
	}
	// A bare "host:port" (no scheme) does not parse into Host; give url.Parse
	// something it can split by prepending a scheme when none is present.
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return false
	}
	if localHostNames[host] {
		return true
	}
	// ".local" is the mDNS suffix — a LAN name by definition.
	if strings.HasSuffix(host, ".local") {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

// ResolveContextWindow returns the input-context window for one provider and
// the source that decided it. It is the SINGLE resolution entry point: the
// compaction threshold and any usage display must both go through it, or the
// percentage a user sees and the moment compaction fires can diverge.
//
// Order (see the file comment for the asymmetry argument):
//
//  1. p.ContextWindow when > 0 — an explicit operator decision always wins,
//     including a value that happens to equal one of the defaults.
//  2. The static catalog, UNLESS the provider is local: a local runtime's
//     window comes from its launch flags, and assuming the family's cloud
//     figure disables compaction while the server truncates the prompt head.
//  3. LocalContextWindow for local providers, DefaultContextWindow otherwise.
//
// fallback lets a caller supply the operator's global
// CompactionConfig.ContextWindow instead of DefaultContextWindow for step 3;
// pass 0 to use DefaultContextWindow. It is IGNORED for local providers —
// the global fallback is a cloud-sized number and applying it locally is
// exactly the bug this function exists to prevent.
func ResolveContextWindow(p ProviderShape, fallback int) (int, ContextWindowSource) {
	if p.ContextWindow > 0 {
		return p.ContextWindow, WindowFromConfig
	}
	local := IsLocalProvider(p)
	if !local {
		if w, ok := KnownContextWindow(p.Model); ok {
			return w, WindowFromCatalog
		}
	}
	if local {
		return LocalContextWindow, WindowFromLocalDefault
	}
	if fallback > 0 {
		return fallback, WindowFromFallback
	}
	return DefaultContextWindow, WindowFromDefault
}

// ContextWindowSource names which rule decided a resolved window. Callers use
// it for diagnostics ("where did this number come from?"); the compaction path
// only needs the number.
type ContextWindowSource string

// Context-window resolution sources, in precedence order.
const (
	// WindowFromConfig means an explicit provider.context_window was set.
	WindowFromConfig ContextWindowSource = "config"
	// WindowFromCatalog means the static model catalog matched.
	WindowFromCatalog ContextWindowSource = "catalog"
	// WindowFromLocalDefault means the provider is locally served and got the
	// conservative LocalContextWindow.
	WindowFromLocalDefault ContextWindowSource = "local-default"
	// WindowFromFallback means the caller-supplied fallback was used.
	WindowFromFallback ContextWindowSource = "fallback"
	// WindowFromDefault means nothing matched and DefaultContextWindow applied.
	WindowFromDefault ContextWindowSource = "default"
)
