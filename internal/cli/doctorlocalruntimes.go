package cli

// I-1 (C3 remediation, top priority): before this file existed,
// internal/llm/eino's local-runtime discovery package (W-C-03 OllamaClient,
// W-C-05 LMStudioClient, W-C-06 Cache, W-C-15 ProbeImageSupport/
// PutImageSupport) had exactly zero production call sites — every exercise
// of it was a unit test building its own httptest.Server. `checkLocalRuntimes`
// is the genuine production consumer the review demanded: it is wired into
// RunDoctor's fixed check sequence (see doctor.go), so every real `yanshi
// doctor` invocation calls it, not just a test binary.
//
// This also resolves Info-2 (PutImageSupport had zero production callers
// connecting a probe's output back to the cache): reportLMStudio calls
// ProbeImageSupport and persists the verdict via PutImageSupport for models
// LM Studio itself already reports loaded — see that function's doc comment
// for why "already loaded" is the gate, not "every listed model".
//
// Neither runtime being installed is the common case, not a degraded one —
// most operators use only cloud providers and have never run `ollama` or LM
// Studio. checkLocalRuntimes therefore mirrors checkMCP's "no mcp servers
// configured" -> StatusOK rather than checkACP's "some binary missing" ->
// StatusWarn: there being nothing to detect is not itself a problem. Per
// runtime unavailability is still reported, just as informational text in
// Message, not as a downgraded Status — this is the "report truthfully,
// never fail-closed, never silent" idiom every other optional-local-thing
// check in this file (checkACP, checkMCP, checkPTY) already follows.
//
// It never starts a runtime: reportOllama/reportLMStudio only ever issue a
// GET against a listing endpoint (via Cache.Get -> Fetcher.FetchModels),
// the same "Probe, never launch" boundary checkMCP's doc comment draws for
// MCP servers. It also never triggers a COLD model load — see
// reportLMStudio's doc comment for the mechanism.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/x6nux/yanshi/internal/llm/eino"
)

// doctorLocalRuntimeProbeTimeout bounds each of checkLocalRuntimesWith's two
// checks independently (one context.WithTimeout per runtime, not one shared
// across both — sharing a single deadline would let a hang in the first
// quietly starve the second's budget too, misreporting a healthy runtime as
// unavailable just because it was checked second).
//
// It is far shorter than eino.DefaultDiscoveryHTTPTimeout (10s, tuned for a
// real listing/pull operation elsewhere — see that constant's own doc
// comment) because `yanshi doctor` is a synchronous CLI command a human is
// waiting on, and review finding I-2 identified the failure shape that
// timeout does not bound well here: a port that accepts TCP but never
// answers HTTP (a stuck daemon, a firewall DROP after the SYN, an unrelated
// service that happens to own the port) leaves a doctor run hanging up to
// DefaultDiscoveryHTTPTimeout per runtime — up to ~20s total for both —
// where the far more common case (nothing listening at all) returns in
// well under a second (ECONNREFUSED is immediate). 3s is long enough that a
// loopback daemon under load still answers, short enough that a doctor run
// against I-2's worst case stays at ~6s instead of ~20s.
const doctorLocalRuntimeProbeTimeout = 3 * time.Second

// checkLocalRuntimes is RunDoctor's I-1 check. It builds a real
// eino.Cache rooted at the default OS cache directory (the same one a live
// `yanshi doctor` run and a future model-picker feature would both read)
// and a real OllamaClient/LMStudioClient pointed at each runtime's default
// loopback address — this is genuine production wiring, not a test double;
// the actual check logic lives in checkLocalRuntimesWith, parameterized by
// cache and clients the same way reportOllama/reportLMStudio already are,
// so I-2's deadline can be exercised against a real hung listener in a test
// without binding to the two hardcoded default ports (127.0.0.1:11434 /
// 127.0.0.1:1234), which would either collide with a real Ollama/LM Studio
// running on the dev machine or assume those ports are free on CI.
//
// offline selects eino.RefreshCacheOnly instead of eino.RefreshAuto — the
// review-whole.md M-1 wiring for RefreshCacheOnly, whose own doc comment
// names "the offline startup uses cache" acceptance bullet's tier. Before
// this, RefreshCacheOnly had zero production callers; RefreshAuto's
// existing stale-fallback path (listing.FetchError != "") already satisfies
// that acceptance bullet when a runtime merely becomes unreachable
// mid-session, but never as a matter of policy the OPERATOR can ask for
// ahead of time (a sandboxed CI run with no loopback egress at all, where
// even attempting the TCP connect is undesirable, not just tolerable when
// it fails).
func checkLocalRuntimes(ctx context.Context, offline bool) CheckResult {
	cache, err := eino.NewCache("", 0)
	if err != nil {
		// DefaultCacheDir failing (os.UserCacheDir with no HOME/XDG_CACHE_HOME
		// resolvable) is an environment problem, not a runtime-detection
		// result — warn, matching checkKeyringAvailability's "the mechanism
		// itself is unavailable" framing rather than folding it into the
		// per-runtime OK message below.
		return CheckResult{Name: "local-runtimes", Status: StatusWarn,
			Message: fmt.Sprintf("discovery cache unavailable: %v", err)}
	}
	policy := eino.RefreshAuto
	if offline {
		policy = eino.RefreshCacheOnly
	}
	return checkLocalRuntimesWith(ctx, cache, eino.NewOllamaClient("", nil), eino.NewLMStudioClient("", "", nil), policy)
}

// checkLocalRuntimesWith is checkLocalRuntimes' body, parameterized by cache
// and clients so a test can point it at a real (fake-protocol or hung)
// listener instead of the production default addresses, and by policy so a
// test can exercise eino.RefreshCacheOnly without threading a DoctorOptions
// all the way through.
func checkLocalRuntimesWith(ctx context.Context, cache *eino.Cache, ollamaClient *eino.OllamaClient, lmstudioClient *eino.LMStudioClient, policy eino.RefreshPolicy) CheckResult {
	ollamaCtx, cancelOllama := context.WithTimeout(ctx, doctorLocalRuntimeProbeTimeout)
	defer cancelOllama()
	ollama := reportOllama(ollamaCtx, cache, ollamaClient, policy)

	lmstudioCtx, cancelLMStudio := context.WithTimeout(ctx, doctorLocalRuntimeProbeTimeout)
	defer cancelLMStudio()
	lmstudio := reportLMStudio(lmstudioCtx, cache, lmstudioClient, policy)

	return CheckResult{Name: "local-runtimes", Status: StatusOK, Message: ollama + "; " + lmstudio}
}

// reportOllama reports what Cache.Get (through client.FetchModels) found for
// Ollama, going through the disk cache — not a bare client.ListModels call —
// so a `yanshi doctor` run actually exercises and warms the same on-disk
// cache file a future model picker would read (W-C-06's whole reason to
// exist), and so a doctor run taken while the daemon is briefly unreachable
// can still report yesterday's listing (Cache.Get's stale-fallback path)
// instead of a bare connection-refused error.
//
// It never calls ProbeImageSupport. Ollama's /api/tags reports no
// loaded/not-loaded state at all — DiscoveredModel.Loaded's doc comment is
// explicit that it is always false for every Ollama result, not "confirmed
// not loaded" — so there is no field here to gate a probe on the way
// reportLMStudio gates on Loaded below. Probing every listed model
// regardless of residency would routinely COLD LOAD a model doctor never
// asked to load (Ollama JIT-loads a model to answer a chat-completions
// request), which is exactly the side effect checkMCP's doc comment says a
// diagnostic command must never have.
func reportOllama(ctx context.Context, cache *eino.Cache, client *eino.OllamaClient, policy eino.RefreshPolicy) string {
	listing, err := cache.Get(ctx, client, policy)
	if err != nil {
		return fmt.Sprintf("ollama: unavailable (%v)", err)
	}
	if listing.FetchError != "" {
		return fmt.Sprintf("ollama: unreachable, showing stale cache (%s)", listing.FetchError)
	}
	return fmt.Sprintf("ollama: %d model(s) (%s)", len(listing.Models), modelIDSummary(listing.Models))
}

// reportLMStudio reports what Cache.Get found for LM Studio, the same way
// reportOllama does for Ollama, and additionally records a W-C-15
// image-support verdict for every model in the listing: a PROBED one for
// every model LM Studio's own /api/v0/models response already reports
// State=="loaded" (DiscoveredModel.Loaded), a DOCUMENTED one (review-whole.md
// M-1: eino.DocumentedImageSupport's only production call site) for every
// model it does not. ProbeImageSupport sends a real chat-completions
// request, and LM Studio JIT-loads whatever model a chat-completions
// request names if it is not already resident; gating the probe on Loaded
// is what keeps this check to Probe-only (see checkLocalRuntimes' package
// comment) instead of using a diagnostic command to cold-load models behind
// the operator's back. A not-loaded model carries no such risk for the
// DOCUMENTED half: DeclaredMultimodal is LM Studio's own "type":"vlm"
// metadata, already sitting in the listing this function fetched — turning
// it into an ImageSupport verdict costs no extra request.
//
// A model that already carries ANY cached verdict is skipped, UNLESS that
// verdict is SourceProbeFailed — repeating an identical measurement on every
// doctor invocation buys nothing once it has genuinely been measured once.
// The check cannot be "skip only if Source == SourceProbed": GetImageSupport
// reads through disk on every call, and sanitizeLoadedListing (M-2)
// unconditionally downgrades every SourceProbed entry to SourceDocumented
// the moment it round-trips through the cache file — a disk file can never
// vouch for "this process observed this measurement this session" on a
// later reader's behalf, including THIS function's own next invocation.
// So the only value GetImageSupport can ever hand back for a genuinely
// probed-and-persisted model is SourceDocumented, never SourceProbed; a
// gate written as "== SourceProbed" would never match and would silently
// re-probe (and re-cold-load-risk-check, though never re-load since it IS
// loaded) every model on every single doctor run. SourceProbeFailed is
// exempt from the skip by its own doc comment (M-1): it means "unknown,
// retry later", not a negative result, and retrying on the next doctor run
// is exactly the "later" that comment asks for — and sanitizeLoadedListing
// does NOT touch SourceProbeFailed entries, so this value round-trips
// through disk unchanged and stays visible to this gate. This gate now
// applies BEFORE the Loaded branch (it used to guard only the probe path),
// so a documented verdict is equally durable across runs.
//
// Under eino.RefreshCacheOnly (checkLocalRuntimes' offline argument), the
// probe half is skipped even for a loaded, unmeasured model:
// ProbeImageSupport is itself a live network round trip, and
// RefreshCacheOnly's whole contract (see its own doc comment) is "never
// touches the network" — an unmeasured loaded model is left unmeasured
// rather than silently reaching out anyway just because Cache.Get already
// happened to have a listing on disk. The documented half is unaffected: it
// reads only the listing already in hand, no network call either way.
func reportLMStudio(ctx context.Context, cache *eino.Cache, client *eino.LMStudioClient, policy eino.RefreshPolicy) string {
	listing, err := cache.Get(ctx, client, policy)
	if err != nil {
		return fmt.Sprintf("lmstudio: unavailable (%v)", err)
	}
	if listing.FetchError != "" {
		return fmt.Sprintf("lmstudio: unreachable, showing stale cache (%s)", listing.FetchError)
	}
	probed, documented := 0, 0
	for _, m := range listing.Models {
		if v, found, _ := cache.GetImageSupport("lmstudio", m.ID); found && v.Source != eino.SourceProbeFailed {
			continue
		}
		if !m.Loaded {
			verdict := eino.DocumentedImageSupport(m.DeclaredMultimodal, "LM Studio type=vlm")
			if err := cache.PutImageSupport("lmstudio", m.ID, verdict); err != nil {
				continue // best-effort: a persist failure here does not fail the whole check.
			}
			documented++
			continue
		}
		if policy == eino.RefreshCacheOnly {
			continue
		}
		verdict := client.ProbeImageSupport(ctx, m.ID)
		if err := cache.PutImageSupport("lmstudio", m.ID, verdict); err != nil {
			continue // best-effort: a persist failure here does not fail the whole check.
		}
		probed++
	}
	msg := fmt.Sprintf("lmstudio: %d model(s) (%s)", len(listing.Models), modelIDSummary(listing.Models))
	if probed > 0 {
		msg += fmt.Sprintf(", image-support probed for %d loaded model(s)", probed)
	}
	if documented > 0 {
		msg += fmt.Sprintf(", image-support documented for %d unloaded model(s)", documented)
	}
	return msg
}

// modelIDSummary renders at most 5 sorted model IDs plus a "+N more" suffix,
// so one doctor line stays readable even against a runtime with dozens of
// pulled models.
func modelIDSummary(models []eino.DiscoveredModel) string {
	if len(models) == 0 {
		return "none"
	}
	ids := make([]string, len(models))
	for i, m := range models {
		ids[i] = m.ID
	}
	sort.Strings(ids)
	const maxShown = 5
	if len(ids) > maxShown {
		return strings.Join(ids[:maxShown], ", ") + fmt.Sprintf(", +%d more", len(ids)-maxShown)
	}
	return strings.Join(ids, ", ")
}
