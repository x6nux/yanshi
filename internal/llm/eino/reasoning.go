package eino

import (
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino-ext/components/model/openai"
)

// ReasoningEffortOption returns a per-call model.Option that sets
// `reasoning_effort` on an OpenAI-compatible chat model for a single Generate/
// Stream invocation, or nil when effort is empty or unrecognized.
//
// Valid values: "low", "medium", "high" (any other value, including "", yields
// nil so the call is a no-op — this keeps non-OpenAI models unaffected). The
// returned option is the one produced by
// eino-ext/components/model/openai.WithReasoningEffort; the upstream acl
// (eino-ext/libs/acl/openai chat_model.go:570-602) merges it over the model's
// configured default at request-build time. Pass it through the ADK agent via
// adk.WithChatModelOptions so the agent forwards it to model.Generate.
//
// model.Option is a struct (not an interface), so a nil *model.Option is used
// to signal "no option"; callers skip wiring it when nil.
//
// Keeping this helper in the eino package confines the provider-specific
// import (eino-ext/components/model/openai) to the LLM layer; callers (e.g. the
// orchestrator) handle only a generic model.Option.
func ReasoningEffortOption(effort string) *model.Option {
	var opt model.Option
	switch effort {
	case "low":
		opt = openai.WithReasoningEffort(openai.ReasoningEffortLevelLow)
	case "medium":
		opt = openai.WithReasoningEffort(openai.ReasoningEffortLevelMedium)
	case "high":
		opt = openai.WithReasoningEffort(openai.ReasoningEffortLevelHigh)
	default:
		return nil
	}
	return &opt
}
