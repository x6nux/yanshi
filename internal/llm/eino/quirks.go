// internal/llm/eino/quirks.go
//
// M5: runtime learning of per-model quirks.
//
// The failure this exists for: switch a session to a model that demands
// `reasoning_content` on every assistant message, and every turn afterwards
// dies with a 400 — because the history was produced by a NON-reasoning model
// and those fields were never there. Nothing in the configuration is wrong, so
// nothing in the configuration can be fixed; the user sees an opaque provider
// error, forever, on a model that works fine in a fresh session.
//
// The shape of the fix is QwenPaw's (providers/model_capability_cache.py +
// retry_chat_model.py): a process-scoped map from model id to the quirks
// LEARNED about it, written only after a confirmed failure-then-recovery cycle,
// and consulted before every subsequent request so the first call after a model
// switch is the only one that pays.
//
// THREE PROPERTIES ARE LOAD-BEARING:
//
//   - Learning is EVIDENCE-GATED. A quirk is recorded only when the repair was
//     actually applied AND the retried request succeeded. Recording on the
//     error alone would let one ambiguous 400 permanently alter every future
//     request to that model.
//   - Repair is IDEMPOTENT and NARROW. Each repair rewrites the smallest thing
//     that can explain the error, and re-applying it to an already-repaired
//     request changes nothing.
//   - Learning is OBSERVABLE. Every transition logs at WARN with the model, the
//     quirk, and the provider text that triggered it. A system that silently
//     starts behaving differently is indistinguishable from an intermittent
//     bug, and the operator would have no way to know that a model is being
//     compensated for.
package eino

import (
	"log/slog"
	"strings"
	"sync"

	"github.com/cloudwego/eino/schema"
)

// Quirk names one learned deviation from the OpenAI-compatible baseline.
type Quirk string

// The quirks yanshi can learn. Each has exactly one detector (quirkFromError)
// and one repair (applyQuirk), and both are listed in AllQuirks so a new quirk
// cannot be half-added.
const (
	// QuirkNeedsReasoningContent means the model rejects assistant messages
	// that carry no reasoning_content. DeepSeek-style thinking models do this
	// whenever the history was produced by a different model.
	QuirkNeedsReasoningContent Quirk = "needs_reasoning_content"
	// QuirkRejectsToolSchemaRefs means the model's endpoint cannot parse the
	// portable-JSON-Schema features yanshi's generated tool schemas use ($ref /
	// $defs / type unions). The repair is the M6 sanitizer, applied to this
	// model only.
	QuirkRejectsToolSchemaRefs Quirk = "rejects_tool_schema_refs"
	// QuirkRejectsSystemRole means the endpoint refuses the "system" role and
	// wants the instructions as a leading user message instead. Several
	// self-hosted chat templates behave this way.
	QuirkRejectsSystemRole Quirk = "rejects_system_role"
)

// AllQuirks is every quirk in detection order. Order matters: quirkFromError
// returns the FIRST match, and a provider message can mention more than one
// thing, so the most specific detector must come first.
var AllQuirks = []Quirk{
	QuirkNeedsReasoningContent,
	QuirkRejectsToolSchemaRefs,
	QuirkRejectsSystemRole,
}

// quirkMarkers maps each quirk to the lowercase substrings that identify it in
// a provider error message.
//
// Keyword matching is the only option available: these all arrive as HTTP 400
// with no machine-readable code distinguishing them from each other or from an
// ordinary malformed request. The markers are therefore chosen to be phrases
// that cannot plausibly occur in an unrelated error — a bare "schema" would
// match nearly every validation failure and is not listed.
var quirkMarkers = map[Quirk][]string{
	QuirkNeedsReasoningContent: {
		"reasoning_content",
		"reasoning content is required",
	},
	QuirkRejectsToolSchemaRefs: {
		"$ref",
		"$defs",
		"unsupported schema keyword",
		"invalid schema for function",
		"unknown field in schema",
	},
	QuirkRejectsSystemRole: {
		"system role is not supported",
		"role 'system' is not",
		`role "system" is not`,
		"only user and assistant roles",
	},
}

// reasoningPlaceholder is what a repaired assistant message gets for its
// reasoning_content.
//
// A single space rather than the empty string, because the endpoints that
// demand the field validate it as non-empty — an empty string reproduces the
// same 400 and would make the repair look like it "did not apply". QwenPaw uses
// the same value for the same reason.
const reasoningPlaceholder = " "

// QuirkStore is the process-scoped, concurrency-safe record of what has been
// learned about each model.
//
// Process-scoped and NOT persisted, deliberately. A quirk is a property of the
// endpoint currently serving a model id, and that changes without warning —
// a gateway upgrade, a different base_url behind the same name, a model alias
// repointed. Persisting the map would carry a stale compensation across
// restarts with nothing to invalidate it; re-learning costs exactly one
// extra round trip per model per process.
type QuirkStore struct {
	mu     sync.RWMutex
	byName map[string]map[Quirk]bool
}

// NewQuirkStore returns an empty store.
func NewQuirkStore() *QuirkStore {
	return &QuirkStore{byName: map[string]map[Quirk]bool{}}
}

// Has reports whether q has been learned for model.
func (s *QuirkStore) Has(model string, q Quirk) bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.byName[model][q]
}

// List returns the quirks learned for model, in AllQuirks order. Returns nil
// when nothing has been learned, so a caller can distinguish "no quirks" from
// "empty set" without a second call.
func (s *QuirkStore) List(model string) []Quirk {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	set := s.byName[model]
	if len(set) == 0 {
		return nil
	}
	out := make([]Quirk, 0, len(set))
	for _, q := range AllQuirks {
		if set[q] {
			out = append(out, q)
		}
	}
	return out
}

// Learn records q for model and reports whether this was a NEW fact.
//
// The bool is what makes the log line honest: the caller logs only on a true
// return, so a model that fails-and-recovers on every one of a hundred turns
// produces one WARN line, not a hundred. evidence is the provider text that
// justified the quirk and goes into that line — an operator reading "yanshi is
// now injecting reasoning_content for model X" needs to see what X said.
func (s *QuirkStore) Learn(model string, q Quirk, evidence string) bool {
	if s == nil || model == "" {
		return false
	}
	s.mu.Lock()
	set := s.byName[model]
	if set == nil {
		set = map[Quirk]bool{}
		s.byName[model] = set
	}
	if set[q] {
		s.mu.Unlock()
		return false
	}
	set[q] = true
	s.mu.Unlock()

	slog.Warn("learned model quirk",
		"model", model,
		"quirk", string(q),
		"evidence", truncateEvidence(evidence),
		"effect", quirkEffect(q))
	return true
}

// Forget drops everything learned about model. Used when a provider is
// reconfigured mid-process, where the previous evidence no longer describes the
// endpoint being talked to.
func (s *QuirkStore) Forget(model string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	delete(s.byName, model)
	s.mu.Unlock()
}

// maxEvidenceBytes bounds the provider text carried into a log line. Provider
// 400 bodies routinely embed the entire rejected request; logging that once per
// learned quirk would put the full conversation into the log at WARN.
const maxEvidenceBytes = 240

// truncateEvidence trims provider text to maxEvidenceBytes, collapsing
// newlines so one log record stays one line.
func truncateEvidence(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > maxEvidenceBytes {
		return s[:maxEvidenceBytes] + "…"
	}
	return s
}

// quirkEffect describes, in operator terms, what yanshi will now do
// differently. It goes in the log line because the quirk name alone says what
// the MODEL does, not what WE do about it, and the second is the part that
// explains a behaviour change.
func quirkEffect(q Quirk) string {
	switch q {
	case QuirkNeedsReasoningContent:
		return "assistant history messages get a placeholder reasoning_content"
	case QuirkRejectsToolSchemaRefs:
		return "tool schemas are sanitized ($ref inlined, unions flattened) for this model"
	case QuirkRejectsSystemRole:
		return "system messages are folded into a leading user message"
	}
	return "unknown"
}

// QuirkFromError returns the quirk a provider error points at, and whether any
// matched.
//
// It runs only for errors the classifier already filed as ClassClientError:
// every quirk manifests as a 400, and a transient 5xx whose body happens to
// quote a schema fragment must not teach us anything. Gating on the class is
// what keeps an unrelated outage from permanently changing how a model is
// addressed.
func QuirkFromError(err error) (Quirk, bool) {
	if err == nil {
		return "", false
	}
	if ClassifyError(err).Class != ClassClientError {
		return "", false
	}
	text := strings.ToLower(err.Error())
	for _, q := range AllQuirks {
		for _, marker := range quirkMarkers[q] {
			if strings.Contains(text, marker) {
				return q, true
			}
		}
	}
	return "", false
}

// applyReasoningContent gives every assistant message that lacks one a
// placeholder reasoning_content, returning a new slice and whether anything
// changed.
//
// It copies each message it modifies instead of writing through the pointer:
// the same []*schema.Message is the ADK's live history, and mutating it would
// make the placeholder permanent — visible to every later turn, to the
// compaction summariser, and to the transcript, for a model the session may
// have already switched away from.
func applyReasoningContent(msgs []*schema.Message) ([]*schema.Message, bool) {
	changed := false
	out := make([]*schema.Message, len(msgs))
	for i, m := range msgs {
		if m == nil || m.Role != schema.Assistant || m.ReasoningContent != "" {
			out[i] = m
			continue
		}
		cp := *m
		cp.ReasoningContent = reasoningPlaceholder
		out[i] = &cp
		changed = true
	}
	return out, changed
}

// applyNoSystemRole rewrites system messages into user messages, merging
// consecutive ones so the model sees a single leading instruction block.
//
// Merging rather than emitting several user messages matters for the endpoints
// that motivate this quirk: their chat templates alternate strictly, and two
// adjacent user turns are rejected by the same validator that rejected the
// system role.
func applyNoSystemRole(msgs []*schema.Message) ([]*schema.Message, bool) {
	var systemParts []string
	rest := make([]*schema.Message, 0, len(msgs))
	for _, m := range msgs {
		if m != nil && m.Role == schema.System {
			if m.Content != "" {
				systemParts = append(systemParts, m.Content)
			}
			continue
		}
		rest = append(rest, m)
	}
	if len(systemParts) == 0 {
		return msgs, false
	}
	merged := schema.UserMessage(strings.Join(systemParts, "\n\n"))
	return append([]*schema.Message{merged}, rest...), true
}
