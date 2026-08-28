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
// There is no incremental channel FOR THE SYSTEM PROMPT to send a merge patch
// through. adk's defaultGenModelInput turns the instruction into ONE
// schema.SystemMessage on every call, rebuilt from scratch each time: the
// receiver holds no previous version of it, and a patch needs a receiver that
// remembers the document being patched.
//
// The narrow wording is deliberate. Message HISTORY does have a durable
// channel on both transports — WS holds it server-side, and SSE has
// history_replaced (chat.go) to push a rewritten history back to the client
// that owns it. What has no such channel is this one system message, which is
// regenerated from server state per request and never round-trips. So the
// finding is "no incremental channel at this layer", not "SSE cannot carry
// increments at all".
//
// What IS incremental here is which half gets RECOMPUTED:
// the static half is reused byte-for-byte, which is also the only thing a
// provider's prefix cache can key on, and the volatile half is appended last so
// the bytes that do change sit where they invalidate the least of it.
//
// # Why this does not touch the runners cache
//
// runnerFor memoises an *adk.Runner per runnerCacheKey, and the agent inside it
// was built with the instruction as a CONFIG field — baked at construction.
// Putting the volatile lines there would make a /model switch depend on
// evicting that cache, and eviction is not even sufficient: switching BACK to a
// previously used model would rebuild its entry from whatever the prompt looked
// like at that moment, and the first turn to reuse it would silently carry a
// stale one. (An eviction method did exist, unused, and has since been deleted
// — see the runners field.) So the runner keeps only the static
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
// It takes TurnOpts.ModelID rather than TurnOpts.Model because Model is a
// model.BaseChatModel interface value from which the name it was registered
// under cannot be recovered. See TurnOpts.ModelID's own doc comment.
//
// # Only a PINNED model may be named, and this is the load-bearing part
//
// withTurnContext binds this only when opts.Model is non-nil. ModelID alone is
// not a claim about which provider serves the call: all three transports fill
// it with einollm.ResolveModelName, whose fallback is the first name in SORTED
// order, and they set opts.Model only when the caller named a model the
// registry actually has. So a turn with a nil Model runs on o.rawModel — the
// einollm.ResilientModel wrapping the CONFIG-ORDER failover chain — and there
// is no single name that is true of it: the head is a different model than the
// sorted-first one whenever config order and alphabetical order disagree, and
// the chain may fail over to a further entry mid-turn anyway.
//
// That name has always been approximate; what is new is the audience. Until
// this section existed it fed status frames and cost attribution, where a
// human reads it next to the transcript. Telling the MODEL "you are X" makes
// it an input the model reasons from — it will claim X's capabilities, context
// window, and knowledge cutoff. Saying nothing costs the model one fact it
// managed without until now; saying the wrong thing is unrecoverable from
// inside the turn. Hence silence for the unpinned case rather than a
// best-effort guess.
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

// sysPromptNow is the clock the volatile section reads.
//
// A package-level variable and not a plain time.Now() call, because the one
// property that needs a clock is the one that cannot be observed without
// moving it: a MEMOISED runner must still emit today's date tomorrow. The
// middleware is constructed inside runnerFor, so a test has no other handle on
// it. Swap it with a t.Cleanup restore; like t.Setenv, that makes the test
// non-parallel, which is the whole price.
var sysPromptNow = time.Now

// renderVolatileSections renders the block that is recomputed every turn.
//
// now is a parameter rather than a clock read inside, so the formatting is
// testable as a pure function.
//
// An empty modelID omits the Model line entirely instead of printing a
// placeholder. Two groups land here: the entry points that never select a
// model (Query, Events, sub-agent turns), and — the common one — every turn
// where the user has not run /model, which executes on the failover chain
// rather than on any one named provider. See withTurnModelID for why a
// best-effort name is worse than none once the model itself is the reader.
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
	runCtx.Instruction += volatileSectionHeader + renderVolatileSections(sysPromptNow(), turnModelIDFromContext(ctx))
	return ctx, runCtx, nil
}
