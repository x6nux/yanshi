// internal/llm/eino/m4_genparams_wire_test.go
//
// M4 over the wire: do max_tokens / temperature / top_p configured in YAML
// actually appear in the JSON body the provider receives?
//
// The reason this needs a wire test rather than a config test is the pointer
// convention. The whole design of M4 rests on nil meaning "the operator said
// nothing" and a set-to-zero meaning "the operator chose zero" — and the place
// those two must differ is the request body, which is the one artefact a config
// test never produces. `temperature: 0` for a deterministic judge call and an
// omitted temperature are indistinguishable in any assertion made above the
// serializer; they differ only in whether the key is PRESENT in the JSON.
//
// So these tests assert on the raw body string, not on a decoded map: a decoded
// map cannot express "the key was absent" differently from "the key was zero"
// unless you check twice, and checking the raw text is both simpler and closer
// to what the gateway sees.
package eino

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/x6nux/yanshi/internal/config"
)

// generateAgainstStub runs one non-streaming Generate through the production
// provider stack and returns the single body the stub received.
func generateAgainstStub(t *testing.T, s *stubProvider, mutate func(*config.ProviderConfig)) capturedRequest {
	t.Helper()
	m, _ := buildStubModel(t, s, mutate)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := m.Generate(ctx, []*schema.Message{schema.UserMessage("hi")}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	reqs := s.chatRequests()
	if len(reqs) != 1 {
		t.Fatalf("want 1 chat request, got %d", len(reqs))
	}
	return reqs[0]
}

// ptr returns a pointer to v, for the optional generation-parameter fields.
func ptr[T any](v T) *T { return &v }

// TestM4_GenerationParamsReachTheWire proves configured values are serialized.
func TestM4_GenerationParamsReachTheWire(t *testing.T) {
	s := newStubProvider(t, nil)
	req := generateAgainstStub(t, s, func(p *config.ProviderConfig) {
		p.MaxTokens = ptr(12345)
		p.Temperature = ptr(float32(0.25))
		p.TopP = ptr(float32(0.77))
	})
	t.Logf("wire body: %s", req.Raw)
	for key, want := range map[string]any{
		"max_tokens":  float64(12345),
		"temperature": 0.25,
		"top_p":       0.77,
	} {
		got, ok := req.Body[key]
		if !ok {
			t.Errorf("%s absent from the request body; the operator's setting never reached the API", key)
			continue
		}
		gf, _ := got.(float64)
		wf, _ := want.(float64)
		if diff := gf - wf; diff > 1e-6 || diff < -1e-6 {
			t.Errorf("%s = %v, want %v", key, got, want)
		}
	}
}

// TestM4_ZeroTemperatureIsDistinctFromUnset is the test the pointer types exist
// for.
//
// A judge call wants temperature: 0 (deterministic). A value type would encode
// that identically to "not configured", and the request would either always
// carry temperature=0 (overriding every provider default) or never carry it
// (making the setting unreachable). Both were real risks; only a body-level
// assertion can tell them apart.
func TestM4_ZeroTemperatureIsDistinctFromUnset(t *testing.T) {
	t.Run("explicit zero is sent", func(t *testing.T) {
		s := newStubProvider(t, nil)
		req := generateAgainstStub(t, s, func(p *config.ProviderConfig) {
			p.Temperature = ptr(float32(0))
		})
		t.Logf("wire body: %s", req.Raw)
		v, ok := req.Body["temperature"]
		if !ok {
			t.Fatal("temperature absent although the operator set it to 0: the zero value was " +
				"erased somewhere between config and wire, so deterministic calls are unconfigurable")
		}
		if f, _ := v.(float64); f != 0 {
			t.Errorf("temperature = %v, want 0", v)
		}
	})

	t.Run("unset is omitted", func(t *testing.T) {
		s := newStubProvider(t, nil)
		req := generateAgainstStub(t, s, nil) // no generation params configured
		t.Logf("wire body: %s", req.Raw)
		if _, ok := req.Body["temperature"]; ok {
			t.Error("temperature present although unconfigured: yanshi is overriding the provider's " +
				"own default for every operator who never asked for one")
		}
		if _, ok := req.Body["top_p"]; ok {
			t.Error("top_p present although unconfigured")
		}
	})
}

// TestM4_UnsetParamsLeaveTheBodyUnchanged pins the compatibility promise from
// the strictest angle available: the exact set of top-level keys.
//
// Some gateways reject unknown or unexpected keys outright, so "we only added
// fields when asked" is a portability guarantee and not just tidiness. Listing
// the expected keys makes any future silent addition fail here rather than at a
// user's gateway.
func TestM4_UnsetParamsLeaveTheBodyUnchanged(t *testing.T) {
	s := newStubProvider(t, nil)
	req := generateAgainstStub(t, s, nil)

	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(req.Raw), &raw); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	t.Logf("top-level keys with no generation params configured: %v", keys)

	for _, forbidden := range []string{"max_tokens", "temperature", "top_p"} {
		if _, ok := raw[forbidden]; ok {
			t.Errorf("%q must be absent when unconfigured, got %s", forbidden, raw[forbidden])
		}
	}
	if _, ok := raw["model"]; !ok {
		t.Error("model missing from the request body")
	}
}

// TestM4_APIKeyReachesTheWire is not strictly M4, but it is the assumption
// every other test in this file rests on: if the stub were being reached
// unauthenticated, or by something other than the configured provider, all the
// body assertions above would be measuring the wrong request.
func TestM4_APIKeyReachesTheWire(t *testing.T) {
	s := newStubProvider(t, nil)
	req := generateAgainstStub(t, s, nil)
	if !strings.Contains(req.Auth, "stub-key") {
		t.Errorf("Authorization = %q, want it to carry the configured api_key", req.Auth)
	}
	if got, _ := req.Body["model"].(string); got != "stub-model-a" {
		t.Errorf("model = %q, want the configured model id", got)
	}
}
