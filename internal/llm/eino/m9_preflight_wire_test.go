// internal/llm/eino/m9_preflight_wire_test.go
//
// M9 over the wire: does startup preflight actually reach a catalogue endpoint,
// name a wrong model, and — the property that matters more — decline to break a
// startup when the endpoint is missing or slow?
//
// The non-blocking half is the one that needs a real server. "It does not
// block" is a claim about a timeout interacting with a network call, and the
// only honest way to test it is to point it at something that genuinely hangs
// and measure how long the caller waited. A test with an injected fake clock
// would prove the timeout constant is passed somewhere, not that the call
// returns.
//
// Measured against the real binary: /v1/models answering 404 cost 0s and the
// turn succeeded; /v1/models sleeping 60s cost ~5s (the discovery timeout) and
// the turn still succeeded.
package eino

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestM9_PreflightNamesAWrongModelAndItsNeighbours proves the whole point of
// the feature: a typo produces a message that says which name is wrong and what
// the real ones are, at startup, rather than an opaque 404 on the first turn.
func TestM9_PreflightNamesAWrongModelAndItsNeighbours(t *testing.T) {
	s := newStubProvider(t, nil)
	s.models = []string{"stub-model-a", "stub-model-b", "gpt-4o-mini"}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	results := PreflightModels(ctx, nil, []ProviderProbe{
		{Name: "stub", Model: "stub-model-A-typo", BaseURL: s.URL, APIKey: "stub-key"},
	})
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	r := results[0]
	t.Logf("preflight result: ok=%v skipped=%v suggestions=%v reason=%q", r.Found, r.Skipped, r.Suggestions, r.SkipReason)

	if r.Skipped {
		t.Fatal("preflight was skipped although the catalogue endpoint answered")
	}
	if r.Found {
		t.Fatal("a model absent from the catalogue was reported as present")
	}
	if len(r.Suggestions) == 0 {
		t.Error("no candidates offered; the operator is told the name is wrong but not what to use")
	}
	joined := strings.Join(r.Suggestions, ",")
	if !strings.Contains(joined, "stub-model-a") {
		t.Errorf("closest = %v, want it to include the near-miss the operator meant", r.Suggestions)
	}

	// The endpoint really was contacted — not inferred from config.
	var sawModels bool
	for _, q := range s.allRequests() {
		if strings.HasSuffix(q.Path, "/models") {
			sawModels = true
		}
	}
	if !sawModels {
		t.Error("no request reached /v1/models; the verdict was produced without asking the provider")
	}
}

// TestM9_CorrectModelPassesQuietly is the false-positive control. A warning
// that fires for correct configurations trains operators to ignore it, which
// costs more than not having it.
func TestM9_CorrectModelPassesQuietly(t *testing.T) {
	s := newStubProvider(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	results := PreflightModels(ctx, nil, []ProviderProbe{
		{Name: "stub", Model: "stub-model-a", BaseURL: s.URL, APIKey: "stub-key"},
	})
	r := results[0]
	t.Logf("preflight result: found=%v skipped=%v reason=%q", r.Found, r.Skipped, r.SkipReason)
	if !r.Found {
		t.Errorf("a correctly configured model was reported as missing (reason=%q)", r.SkipReason)
	}
}

// TestM9_MissingCatalogueEndpointIsSkippedNotFailed covers the common internal
// deployment: Azure serves deployments rather than models, GitHub Models omits
// the listing, and plenty of relays 404 it. None of those is a configuration
// error and none may produce a warning that says one exists.
func TestM9_MissingCatalogueEndpointIsSkippedNotFailed(t *testing.T) {
	for _, status := range []int{404, 401, 500} {
		t.Run(http404Name(status), func(t *testing.T) {
			s := newStubProvider(t, nil)
			s.modelsStatus = status
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			r := PreflightModels(ctx, nil, []ProviderProbe{
				{Name: "stub", Model: "whatever", BaseURL: s.URL, APIKey: "k"},
			})[0]
			t.Logf("status %d → skipped=%v found=%v reason=%q", status, r.Skipped, r.Found, r.SkipReason)
			if !r.Skipped {
				t.Errorf("a %d from the catalogue endpoint was not treated as 'no listing'; "+
					"deployments without the endpoint would be warned about a correct config", status)
			}
		})
	}
}

// http404Name labels a status for a subtest name.
func http404Name(status int) string {
	switch status {
	case 404:
		return "404 no such endpoint"
	case 401:
		return "401 listing not permitted"
	default:
		return "500 control plane down"
	}
}

// TestM9_SlowCatalogueDoesNotBlockStartup is the load-bearing one.
//
// An air-gapped or firewalled deployment does not get a refusal from the
// catalogue host, it gets silence — the connection hangs until something times
// it out. If that something were absent, every yanshi start in such an
// environment would stall for the OS connect timeout (minutes), and the feature
// meant to improve first-run experience would be the thing making it worst.
//
// The stub sleeps far longer than DefaultDiscoveryTimeout, and the test asserts
// the caller came back on the timeout's schedule.
func TestM9_SlowCatalogueDoesNotBlockStartup(t *testing.T) {
	s := newStubProvider(t, nil)
	s.modelsDelay = 30 * time.Second

	// Bound the probe the way bootstrap.RunPreflight does.
	ctx, cancel := context.WithTimeout(context.Background(), DefaultDiscoveryTimeout)
	defer cancel()
	start := time.Now()
	r := PreflightModels(ctx, nil, []ProviderProbe{
		{Name: "stub", Model: "stub-model-a", BaseURL: s.URL, APIKey: "k"},
	})[0]
	waited := time.Since(start)
	t.Logf("hung catalogue returned after %v: skipped=%v reason=%q", waited, r.Skipped, r.SkipReason)

	if waited > DefaultDiscoveryTimeout+3*time.Second {
		t.Errorf("preflight waited %v on an unresponsive catalogue; startup would stall by that "+
			"much on every air-gapped deployment", waited)
	}
	if !r.Skipped {
		t.Error("a timed-out probe must degrade to 'skipped', never to a config warning")
	}
}

// TestM9_NoBaseURLIsNotProbed pins the credential-safety rule from
// FetchModelCatalog: a provider that configured no base_url is using the SDK's
// own default, and probing a host the operator never named would send the API
// key somewhere they did not choose.
func TestM9_NoBaseURLIsNotProbed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r := PreflightModels(ctx, nil, []ProviderProbe{
		{Name: "nourl", Model: "gpt-4o", BaseURL: "", APIKey: "secret"},
	})[0]
	t.Logf("no base_url → skipped=%v reason=%q", r.Skipped, r.SkipReason)
	if !r.Skipped {
		t.Error("a provider with no base_url must be skipped, not probed at a default host")
	}
}
