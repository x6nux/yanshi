package eino

import (
	"encoding/json"

	"github.com/cloudwego/eino/components/model"
)

// outputSchemaOptions is the eino-package-owned impl-specific option struct
// that carries a per-turn JSON Schema for structured output. It is consumed
// ONLY by this package's own adapters (anthropic.go, responses.go) via
// model.GetImplSpecificOptions(&outputSchemaOptions{}, opts...).
//
// Why an eino-owned type (and not openai.WithReasoningEffort-style)? The
// reasoning_effort option produced by eino-ext/libs/acl/openai wraps a setter
// over the acl package's UNEXPORTED *openaiOptions, so no caller outside that
// package can decode it — and indeed reasoning_effort never reaches the custom
// anthropic/responses adapters today (they only call model.GetCommonOptions).
// By owning the option type HERE, the package's own adapters can decode it,
// while the openai (eino-ext chat completions) path — which decodes its own
// *openaiOptions — silently ignores it (model.GetImplSpecificOptions[T] type-
// asserts each option's setter against func(*T) and skips non-matching ones;
// see eino@v0.9.12/components/model/option.go:239-255). That type isolation is
// exactly what makes "openai 路径不受影响" structural rather than convention-based.
type outputSchemaOptions struct {
	Schema json.RawMessage
	// ThinkingEffort is "low" | "medium" | "high" for a turn that asked for
	// extended thinking. It rides THIS struct rather than one of its own, and
	// that is load-bearing: GetImplSpecificOptions type-asserts each option's
	// setter against func(*T) and silently skips the ones that do not match,
	// so a second struct would be invisible to the decoder that reads this one
	// — no error, no warning, a field that is never set.
	ThinkingEffort string
}

// OutputSchemaOption returns a per-call model.Option that carries a JSON Schema
// (schemaDoc) for structured output on a single Generate/Stream invocation, or
// nil when schemaDoc is empty so the text-mode path forwards nothing and stays
// byte-identical to pre-A12.
//
// Pass the returned option through the ADK agent via adk.WithChatModelOptions
// (same forwarding shape as ReasoningEffortOption). The anthropic and responses
// adapters decode it in Generate/Stream and map it onto the provider's native
// structured-output request field; the openai (eino-ext) path does not decode
// it and is unaffected.
//
// model.Option is a struct (not an interface); a nil *model.Option signals
// "no option" so callers skip wiring it when nil.
func OutputSchemaOption(schemaDoc json.RawMessage) *model.Option {
	if len(schemaDoc) == 0 {
		return nil
	}
	opt := model.WrapImplSpecificOptFn(func(o *outputSchemaOptions) {
		o.Schema = schemaDoc
	})
	return &opt
}

// ThinkingOption returns a per-call option asking the provider for extended
// thinking at the given effort, or nil when the turn did not ask for any.
//
// It is separate from ReasoningEffortOption, which produces
// openai.WithReasoningEffort — an option the Anthropic adapter never decodes.
// That is why /think high on a Claude model changed nothing on the wire while
// the TUI, the classifier and the frame vocabulary all behaved as though it
// had: the effort reached a decoder that this provider does not use.
//
// Callers pass both. The openai path reads its own option and ignores this
// one; the anthropic and responses adapters read this one. The type isolation
// is what keeps "the other provider is unaffected" structural rather than a
// convention someone has to remember.
func ThinkingOption(effort string) *model.Option {
	switch effort {
	case "low", "medium", "high":
	default:
		return nil
	}
	opt := model.WrapImplSpecificOptFn(func(o *outputSchemaOptions) {
		o.ThinkingEffort = effort
	})
	return &opt
}
