// Package toolreg carries the set of tool names actually registered with the
// running agent, so the authorization path can refuse a name that no tool
// answers to.
//
// # The hole this closes
//
// yanshi already has two compile-time gates against phantom tool names — GOV5
// reconciles the guard profile's allow list against the registry, GOV7 does the
// same for the allow-edits auto-approval set. Both reason about Go source. Nor
// is either reachable at runtime: they are tests.
//
// At runtime the tool NAME in a guard.Action is just a string, and several
// authorization subjects are assembled rather than looked up — a
// secproc.SecureProcessSpec carries a free-form Tool field, and a spec that
// forgets to set it authorizes the empty name. What happens next is the
// problem: guard's tool dimension answers "not on the allowlist" with Prompt,
// not with a deny, because an interactive user may legitimately approve a tool
// the profile has not seen. So an unregistered — possibly empty, possibly
// model-supplied — name arrives at the user as a permission dialog with an
// "Allow" button, describing a tool that does not exist. Approving it records
// an approval rule for a phantom scope.
//
// The fix is fail-closed and deliberately narrow: if the caller has told us
// which names are real, a name outside that set is denied outright and never
// reaches the callback. There is no dialog to click, because there is nothing
// a user could sensibly consent to.
//
// # Why the set is bound per turn instead of being a package-level global
//
// The registered set is not a constant of the program. Sub-agents run with a
// filtered tool subset, plan mode runs with another, and MCP tools appear and
// disappear as servers connect. A global would be written by whichever
// component started last and read by everyone, which is how a sub-agent ends
// up authorizing against the parent's wider surface. Binding it on the
// context makes the set follow the execution scope that produced it.
//
// # Why an absent set means "allow", not "deny"
//
// Fail-closed is the rule for a security decision made with incomplete
// information about the ACTION. This is a decision made with incomplete
// information about the CONFIGURATION: no set bound means nobody told this
// process what its tools are. Denying everything in that case turns a wiring
// omission into a total outage of every tool, in every headless caller and
// every test, while denying nothing leaves behaviour exactly as it was before
// this package existed. The gate is a tightening layered on top of guard's
// own checks, never a replacement for them, so degrading to "no extra
// tightening" is the honest failure mode. Callers that need the check to be
// live must bind the set — the orchestrator does it for every turn.
package toolreg

import (
	"context"
	"sort"
	"strings"
)

// Set is an immutable set of registered tool names.
//
// The zero value is a usable empty set that reports nothing as registered;
// callers distinguish "no set bound" from "empty set bound" through the
// second return value of FromContext, not by comparing against the zero value.
type Set struct {
	names map[string]struct{}
}

// NewSet builds a Set from names. Empty and blank names are dropped: they can
// only come from a spec that failed to fill its Tool field, and admitting one
// would make the empty name "registered" and defeat the whole check.
func NewSet(names []string) Set {
	m := make(map[string]struct{}, len(names))
	for _, n := range names {
		if strings.TrimSpace(n) == "" {
			continue
		}
		m[n] = struct{}{}
	}
	return Set{names: m}
}

// Has reports whether name is registered.
func (s Set) Has(name string) bool {
	if s.names == nil {
		return false
	}
	_, ok := s.names[name]
	return ok
}

// Len reports how many names the set holds.
func (s Set) Len() int { return len(s.names) }

// Names returns the registered names, sorted, for diagnostics and error text.
func (s Set) Names() []string {
	out := make([]string, 0, len(s.names))
	for n := range s.names {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// registeredKey is the context key for a bound Set.
type registeredKey struct{}

// WithRegistered binds the registered tool-name set onto ctx.
//
// An empty names slice binds NOTHING — the context is returned unchanged. The
// alternative (binding an empty set) would deny every tool call in the scope,
// and the way a caller arrives at an empty slice is a registry that has not
// been populated yet, not an operator who meant "no tools may run".
func WithRegistered(ctx context.Context, names []string) context.Context {
	set := NewSet(names)
	if set.Len() == 0 {
		return ctx
	}
	return context.WithValue(ctx, registeredKey{}, set)
}

// FromContext reads back the set bound by WithRegistered. The boolean reports
// whether a set was bound at all, which is what separates "this name is not a
// tool" from "nobody told us what the tools are".
func FromContext(ctx context.Context) (Set, bool) {
	s, ok := ctx.Value(registeredKey{}).(Set)
	return s, ok && s.Len() > 0
}

// UnregisteredError is returned by Check for a name no registered tool answers
// to.
//
// It is a distinct type so the authorization layer can classify it: this is a
// STRUCTURAL refusal, in the same family as guard's non-overridable HardDeny.
// No permission mode overrides it and no approval rule can pre-authorize it,
// because the thing being refused does not exist — there is no correct way for
// a user to say yes.
type UnregisteredError struct {
	// Tool is the name that was refused. Empty when the caller supplied no
	// name at all, which is itself one of the shapes this catches.
	Tool string
}

// Error implements error.
func (e *UnregisteredError) Error() string {
	if e.Tool == "" {
		return "unregistered tool: authorization requested with an empty tool name"
	}
	return "unregistered tool: " + e.Tool + " is not a registered tool of this agent"
}

// Check reports whether name may be authorized against the set bound on ctx.
//
// It returns nil when the name is registered AND when no set is bound (see the
// package doc for why the second case is not a denial). It returns an
// *UnregisteredError otherwise, including for an empty name whenever a set is
// bound.
func Check(ctx context.Context, name string) error {
	set, bound := FromContext(ctx)
	if !bound {
		return nil
	}
	if set.Has(name) {
		return nil
	}
	return &UnregisteredError{Tool: name}
}
