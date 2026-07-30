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
