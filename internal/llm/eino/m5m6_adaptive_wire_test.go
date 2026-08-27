// internal/llm/eino/m5m6_adaptive_wire_test.go
//
// M5 (learned quirks) and M6 (tool-schema sanitization) over the wire.
//
// Both features are claims about how a REQUEST changes after a failure, and
// both are easy to test in a way that cannot fail. A unit test can call
// SanitizeToolSchema and assert "$ref" is gone from the returned map — and that
// proves the rewriter works, not that anything installs it, not that the retry
// carries it, and not that the second request the provider receives is
// different from the first.
//
// The interesting assertions are therefore all of the form "compare request N
// to request N+1 as the SERVER saw them":
//
//   - the first body contains the offending construct,
//   - the second body does not,
//   - and there is a second body at all (exactly one retry, not zero and not a
//     loop).
//
// M5's observability is tested too, because "it silently started working" is a
// failure mode of its own: a model being compensated for is something the
// operator has to be able to discover, and the only trace of it is a log line.
package eino

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/eino-contrib/jsonschema"
)

// refSchemaTool is a tool whose parameter schema uses exactly the constructs
// strict gateways reject: a $ref into $defs, a nullable type union, and a
// oneOf. It is the input for both the M6 and M5 tests.
func refSchemaTool(t *testing.T) *schema.ToolInfo {
	t.Helper()
	js := &jsonschema.Schema{}
	raw := `{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"type": "object",
		"$defs": {
			"Point": {
				"type": "object",
				"properties": {"x": {"type": "integer"}, "y": {"type": "integer"}}
			}
		},
		"properties": {
			"origin":   {"$ref": "#/$defs/Point"},
			"nickname": {"type": ["string", "null"]},
			"mode":     {"oneOf": [{"type": "string"}, {"type": "integer"}]}
		},
		"required": ["origin"]
	}`
	if err := json.Unmarshal([]byte(raw), js); err != nil {
		t.Fatal(err)
	}
	return &schema.ToolInfo{Name: "plot", Desc: "plot a point",
		ParamsOneOf: schema.NewParamsOneOfByJSONSchema(js)}
}

// schemaRejectionBody is the 400 a strict gateway answers a $ref schema with.
const schemaRejectionBody = `{"error":{"message":"Invalid schema for function 'plot': ` +
	`unsupported schema keyword $ref","type":"invalid_request_error"}}`

// adaptiveOverStub builds the production provider stack against s and wraps it
// in an AdaptiveModel configured the way bootstrap configures it.
func adaptiveOverStub(t *testing.T, s *stubProvider, cfg AdaptiveConfig) *AdaptiveModel {
	t.Helper()
	inner, _ := buildStubModel(t, s, nil)
	cfg.ModelID = "stub-model-a"
	a := NewAdaptiveModel(inner, cfg)
	if a == nil {
		t.Fatal("NewAdaptiveModel returned nil")
	}
	return a
}

// bodyHasSchemaConstructs reports which of the portability-hostile constructs
// appear anywhere in a raw request body.
func bodyHasSchemaConstructs(raw string) (hasRef, hasDefs, hasOneOf, hasNullUnion bool) {
	return strings.Contains(raw, `"$ref"`),
		strings.Contains(raw, `"$defs"`),
		strings.Contains(raw, `"oneOf"`),
		strings.Contains(raw, `"null"`)
}

// TestM6_SanitizeAlwaysStripsConstructsBeforeTheyLeave proves the `always`
// policy rewrites the schema on the FIRST request — the point of `always` being
// that a deployment talking only to a strict gateway need not pay one failed
// request per model to learn.
func TestM6_SanitizeAlwaysStripsConstructsBeforeTheyLeave(t *testing.T) {
	s := newStubProvider(t, nil)
	a := adaptiveOverStub(t, s, AdaptiveConfig{Sanitize: SanitizeAlways})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := a.Generate(ctx, []*schema.Message{schema.UserMessage("plot 1,2")},
		model.WithTools([]*schema.ToolInfo{refSchemaTool(t)})); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	reqs := s.chatRequests()
	if len(reqs) != 1 {
		t.Fatalf("want exactly 1 request under `always` (no failure to learn from), got %d", len(reqs))
	}
	ref, defs, oneOf, null := bodyHasSchemaConstructs(reqs[0].Raw)
	t.Logf("first body constructs: $ref=%v $defs=%v oneOf=%v null=%v", ref, defs, oneOf, null)
	if ref || defs || oneOf {
		t.Errorf("sanitize=always still shipped $ref=%v $defs=%v oneOf=%v to the endpoint", ref, defs, oneOf)
	}
	if !strings.Contains(reqs[0].Raw, `"plot"`) {
		t.Error("the tool itself vanished from the request; sanitization must rewrite, not drop")
	}
}

// TestM6_SanitizeNeverShipsConstructsUnchanged is the negative control for the
// test above, and it is not optional: without it, a sanitizer that always ran
// (or a schema that never contained the constructs in the first place) would
// make the `always` test pass for the wrong reason.
func TestM6_SanitizeNeverShipsConstructsUnchanged(t *testing.T) {
	s := newStubProvider(t, nil)
	a := adaptiveOverStub(t, s, AdaptiveConfig{Sanitize: SanitizeNever})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := a.Generate(ctx, []*schema.Message{schema.UserMessage("plot 1,2")},
		model.WithTools([]*schema.ToolInfo{refSchemaTool(t)})); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	reqs := s.chatRequests()
	if len(reqs) != 1 {
		t.Fatalf("want 1 request, got %d", len(reqs))
	}
	ref, defs, oneOf, _ := bodyHasSchemaConstructs(reqs[0].Raw)
	t.Logf("sanitize=never body constructs: $ref=%v $defs=%v oneOf=%v", ref, defs, oneOf)
	if !ref && !defs && !oneOf {
		t.Error("no hostile construct reached the wire even with sanitize=never — the fixture is " +
			"not exercising what the `always` test claims to strip, so that test proves nothing")
	}
}

// TestM5_SchemaRejectionIsLearnedAndTheRetryDiffers is the core M5 test: the
// stub rejects the first request with a schema-shaped 400, and the SECOND
// request must be materially different (sanitized) and succeed.
//
// It asserts on the pair of bodies rather than on the store, because a quirk
// recorded in a map that no request consults is precisely the "written but
// unread" shape this repo keeps rediscovering.
func TestM5_SchemaRejectionIsLearnedAndTheRetryDiffers(t *testing.T) {
	s := newStubProvider(t, func(n int, _ capturedRequest) stubResponse {
		if n == 1 {
			return stubResponse{Status: 400, Body: schemaRejectionBody}
		}
		return stubResponse{}
	})
	quirks := NewQuirkStore()
	a := adaptiveOverStub(t, s, AdaptiveConfig{Quirks: quirks, Sanitize: SanitizeAuto})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := a.Generate(ctx, []*schema.Message{schema.UserMessage("plot 1,2")},
		model.WithTools([]*schema.ToolInfo{refSchemaTool(t)}))
	if err != nil {
		t.Fatalf("the schema rejection was not recovered from: %v", err)
	}
	if out.Content != "STUB-OK" {
		t.Errorf("content = %q, want STUB-OK", out.Content)
	}

	reqs := s.chatRequests()
	if len(reqs) != 2 {
		t.Fatalf("want exactly 2 requests (one rejected, one repaired), got %d — "+
			"0 extra means no repair was attempted, >1 extra means the budget is not 1", len(reqs))
	}
	ref1, defs1, oneOf1, _ := bodyHasSchemaConstructs(reqs[0].Raw)
	ref2, defs2, oneOf2, _ := bodyHasSchemaConstructs(reqs[1].Raw)
	t.Logf("request 1 constructs: $ref=%v $defs=%v oneOf=%v", ref1, defs1, oneOf1)
	t.Logf("request 2 constructs: $ref=%v $defs=%v oneOf=%v", ref2, defs2, oneOf2)
	if !ref1 && !defs1 {
		t.Error("the rejected request carried no $ref/$defs — the fixture is not reproducing the " +
			"condition M5 is supposed to learn from")
	}
	if ref2 || defs2 || oneOf2 {
		t.Errorf("the RETRIED request still carries $ref=%v $defs=%v oneOf=%v: the repair did not "+
			"reach the wire", ref2, defs2, oneOf2)
	}
	if reqs[0].Raw == reqs[1].Raw {
		t.Error("the retry sent a byte-identical body; resending an unchanged request can only " +
			"reproduce the same failure and costs another charge")
	}

	// The quirk must be recorded only AFTER the repaired request succeeded, and
	// it must be recorded, or the next call pays the same failed request again.
	learned := quirks.List("stub-model-a")
	t.Logf("learned quirks: %v", learned)
	if len(learned) != 1 || learned[0] != QuirkRejectsToolSchemaRefs {
		t.Errorf("learned = %v, want exactly [%s]", learned, QuirkRejectsToolSchemaRefs)
	}
}

// TestM5_LearnedQuirkIsAppliedToTheNextCallWithoutFailingAgain is the payoff
// half: having learned, the SECOND call must be sanitized from its first
// request, so the cost of learning is one failed request per model per process
// and not one per call.
func TestM5_LearnedQuirkIsAppliedToTheNextCallWithoutFailingAgain(t *testing.T) {
	s := newStubProvider(t, func(n int, req capturedRequest) stubResponse {
		// Reject ANY request carrying $ref, whenever it arrives. A stub that
		// only rejected request #1 would pass this test even if the learned
		// quirk were never applied.
		if strings.Contains(req.Raw, `"$ref"`) {
			return stubResponse{Status: 400, Body: schemaRejectionBody}
		}
		return stubResponse{}
	})
	quirks := NewQuirkStore()
	a := adaptiveOverStub(t, s, AdaptiveConfig{Quirks: quirks, Sanitize: SanitizeAuto})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	tools := model.WithTools([]*schema.ToolInfo{refSchemaTool(t)})
	if _, err := a.Generate(ctx, []*schema.Message{schema.UserMessage("first")}, tools); err != nil {
		t.Fatalf("first call: %v", err)
	}
	afterFirst := len(s.chatRequests())
	if _, err := a.Generate(ctx, []*schema.Message{schema.UserMessage("second")}, tools); err != nil {
		t.Fatalf("second call: %v", err)
	}
	reqs := s.chatRequests()
	t.Logf("requests after call 1 = %d, after call 2 = %d", afterFirst, len(reqs))

	if afterFirst != 2 {
		t.Fatalf("first call should cost one rejection plus one repair, got %d requests", afterFirst)
	}
	if got := len(reqs) - afterFirst; got != 1 {
		t.Errorf("second call issued %d requests, want 1: the learned quirk was not applied up "+
			"front, so every call keeps paying the discovery cost", got)
	}
	if ref, _, _, _ := bodyHasSchemaConstructs(reqs[afterFirst].Raw); ref {
		t.Error("the second call's FIRST request still carried $ref")
	}
}

// TestM5_LearningIsObservable pins the third load-bearing property from
// quirks.go: a model that silently starts behaving differently is
// indistinguishable from an intermittent bug, so every learned quirk must
// produce a log record naming the model, the quirk, and the provider's own
// words.
//
// The evidence lives ONLY in that log line — QuirkStore keeps a set of quirks
// and nothing else — so capturing slog is not an indirection here, it is the
// only place the property is observable at all. A test that asserted on the
// store would be asserting on a different claim.
func TestM5_LearningIsObservable(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	s := newStubProvider(t, func(n int, _ capturedRequest) stubResponse {
		if n == 1 {
			return stubResponse{Status: 400, Body: schemaRejectionBody}
		}
		return stubResponse{}
	})
	a := adaptiveOverStub(t, s, AdaptiveConfig{Quirks: NewQuirkStore(), Sanitize: SanitizeAuto})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := a.Generate(ctx, []*schema.Message{schema.UserMessage("plot")},
		model.WithTools([]*schema.ToolInfo{refSchemaTool(t)})); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	logged := buf.String()
	t.Logf("captured log:\n%s", logged)
	if !strings.Contains(logged, "learned model quirk") {
		t.Fatal("nothing was logged when the model's behaviour started being compensated for; " +
			"the operator would only see that it 'suddenly started working'")
	}
	for _, want := range []string{"stub-model-a", string(QuirkRejectsToolSchemaRefs)} {
		if !strings.Contains(logged, want) {
			t.Errorf("log record does not name %q", want)
		}
	}
	if !strings.Contains(strings.ToLower(logged), "schema") {
		t.Error("log record does not carry the provider's own words as evidence")
	}
}

// TestM5_UnrelatedErrorIsNotLearned is the false-positive guard. A quirk
// permanently alters every future request to a model, so an ordinary 400 that
// happens to be a genuine bad request must not teach anything — otherwise one
// malformed call degrades the model for the rest of the process.
func TestM5_UnrelatedErrorIsNotLearned(t *testing.T) {
	s := newStubProvider(t, func(int, capturedRequest) stubResponse {
		return stubResponse{Status: 400,
			Body: `{"error":{"message":"messages[0].content must be a string","type":"invalid_request_error"}}`}
	})
	quirks := NewQuirkStore()
	a := adaptiveOverStub(t, s, AdaptiveConfig{Quirks: quirks, Sanitize: SanitizeAuto})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := a.Generate(ctx, []*schema.Message{schema.UserMessage("x")},
		model.WithTools([]*schema.ToolInfo{refSchemaTool(t)})); err == nil {
		t.Fatal("want the genuine bad request to surface as an error")
	}
	if learned := quirks.List("stub-model-a"); len(learned) != 0 {
		t.Errorf("learned %v from an unrelated 400; one malformed call must not permanently "+
			"alter every future request to this model", learned)
	}
	if n := len(s.chatRequests()); n != 1 {
		t.Errorf("issued %d requests for an unrecoverable error, want 1 (no pointless retry)", n)
	}
}
