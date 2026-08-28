// Per-turn re-rendering of the system prompt.
//
// The prompt has two halves with very different lifetimes:
//
//   - STATIC — the operator instruction, the skill meta-prompt, the memory
//     block, and the probed environment (OS, shell, toolchain versions).
//     Rendered ONCE, in New(), and never re-rendered: buildEnvInfo spawns a
//     dozen-odd subprocesses, so re-probing per turn would put that many
//     process launches in front of every model call to recompute values that
//     cannot change without the machine changing.
//   - VOLATILE — the wall-clock date and the model THIS turn runs against.
//     Both are wrong the moment they are baked. A server that stays up past
//     midnight kept telling the model the date it booted on; and the prompt
//     never named the model at all, so /model swapped the provider underneath
//     a model that was never told it had been swapped.
//
// # "Incremental" here means the RENDERING, not the transmission
//
// There is no incremental system channel to send a merge patch through.
// adk's defaultGenModelInput turns the instruction into ONE schema.SystemMessage
// on every call, and both transports rebuild the whole request each time — SSE
// in particular replays CLIENT-held history, so anything a turn appends to a
// server-side slice is discarded when the turn ends. The receiver keeps no
// state between requests, and a patch needs a receiver that remembers the
// document it is patching. What is incremental is which half gets RECOMPUTED:
// the static half is reused byte-for-byte, which is also the only thing a
// provider's prefix cache can key on, and the volatile half is appended last so
// the bytes that do change sit where they invalidate the least of it.
//
// # Why this does not touch the runners cache
//
// runnerFor memoises an *adk.Runner per {model, mode}, and the agent inside it
// was built with the instruction as a CONFIG field — baked at construction.
// Putting the volatile lines there would make a /model switch depend on
// evicting that cache (FlushRunners), and eviction is not even sufficient:
// switching BACK to a previously used model would rebuild its entry from
// whatever the prompt looked like at that moment, and the first turn to reuse
// it would silently carry a stale one. So the runner keeps only the static
// half, and this middleware supplies the volatile half per run through adk's
// BeforeAgent hook — ChatModelAgentContext.Instruction is applied to a fresh
// copy of the exec context on every Run and never written back onto the agent.
// The cache stays correct because what it caches no longer varies per turn.
package orchestrator

import (
	"context"
	"strings"
	"time"

	"github.com/cloudwego/eino/adk"
)

// volatileSectionHeader opens the per-turn block, in the same "--- Name ---"
// shape New() already uses for the environment block it follows.
const volatileSectionHeader = "\n\n--- Session ---\n"

// turnModelKey is the context key for the registry name of the model a turn
// runs against.
type turnModelKey struct{}

// withTurnModelID binds the turn's registry model name onto ctx.
//
// Unexported on purpose: the only producer (withTurnContext) and the only
// consumer (renderVolatileSections, via the middleware below) are both in this
// package, so exporting it would add public surface with no caller outside —
// and an exported injector owes GOV6 a production call site for a reason that
// would not apply here.
//
// TurnOpts.ModelID rather than TurnOpts.Model, because Model is a
// model.BaseChatModel interface value from which the name it was registered
// under cannot be recovered. See TurnOpts.ModelID's own doc comment.
func withTurnModelID(ctx context.Context, modelID string) context.Context {
	if modelID == "" {
		return ctx
	}
	return context.WithValue(ctx, turnModelKey{}, modelID)
}

// turnModelIDFromContext reads back what withTurnModelID bound, or "" when the
// entry point did not select a model (Query/Events, sub-agent turns).
func turnModelIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(turnModelKey{}).(string)
	return id
}

// renderVolatileSections renders the block that is recomputed every turn.
//
// now is a parameter rather than a time.Now() call inside so the date line is
// testable without a clock seam; the middleware passes time.Now.
//
// An empty modelID omits the Model line entirely instead of printing a
// placeholder: the entry points that do not select a model (Query, Events,
// sub-agent turns) have nothing truthful to put there, and "Model: default"
// tells the model strictly less than saying nothing while still costing a line
// the reader has to reconcile against the provider it is actually talking to.
func renderVolatileSections(now time.Time, modelID string) string {
	var b strings.Builder
	b.WriteString("Date: " + now.Format("2006-01-02") + "\n")
	if modelID != "" {
		b.WriteString("Model: " + modelID + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// systemPromptRefresher is the adk.ChatModelAgentMiddleware that appends the
// volatile block to the cached agent's static instruction, once per Run.
//
// Stateless, like resultHygiene and loopGuardMiddleware and for the same
// reason: runnerFor memoises runners, so one instance serves every turn on that
// model. Everything that varies comes off the turn's context.
type systemPromptRefresher struct {
	*adk.BaseChatModelAgentMiddleware
}

// newSystemPromptRefresher builds a systemPromptRefresher ready for
// installation on an adk.ChatModelAgentConfig.Handlers slice.
func newSystemPromptRefresher() *systemPromptRefresher {
	return &systemPromptRefresher{BaseChatModelAgentMiddleware: &adk.BaseChatModelAgentMiddleware{}}
}

// BeforeAgent re-renders the volatile sections onto the run's instruction.
//
// BeforeAgent and not BeforeModelRewriteState: the instruction is not part of
// the message state — adk converts it to the system message inside
// genModelInput, downstream of every state hook — so BeforeAgent is the only
// hook that can reach it. It also runs once per turn rather than once per ReAct
// iteration, which is the right granularity: the date and the selected model
// are fixed for the duration of a turn, and re-rendering mid-turn would let the
// system message change under a half-finished tool-call sequence.
func (s *systemPromptRefresher) BeforeAgent(
	ctx context.Context,
	runCtx *adk.ChatModelAgentContext,
) (context.Context, *adk.ChatModelAgentContext, error) {
	if runCtx == nil {
		return ctx, runCtx, nil
	}
	runCtx.Instruction += volatileSectionHeader + renderVolatileSections(time.Now(), turnModelIDFromContext(ctx))
	return ctx, runCtx, nil
}
