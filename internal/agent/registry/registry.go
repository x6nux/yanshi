// Package registry tracks available sub-agents (local, external, remote).
package registry

import (
	"github.com/x6nux/yanshi/internal/guard"
)

// Kind classifies a sub-agent's execution model.
type Kind string

// Sub-agent execution model kinds.
const (
	KindLocal    Kind = "local"    // in-process Eino agent
	KindExternal Kind = "external" // local subprocess via ACP (M4)
	KindRemote   Kind = "remote"   // network worker via Task API (M5)
)

// Entry describes a registered sub-agent.
type Entry struct {
	Name         string
	Kind         Kind
	Description  string
	Capabilities []string // tool patterns this agent can handle
	Profile      guard.PermissionProfile
}

// Registry holds sub-agent entries by name.
type Registry struct {
	entries map[string]Entry
}

// New returns an empty Registry.
func New() *Registry { return &Registry{entries: map[string]Entry{}} }

// Register adds or replaces an entry.
func (r *Registry) Register(e Entry) { r.entries[e.Name] = e }

// Get returns an entry by name.
func (r *Registry) Get(name string) (Entry, bool) {
	e, ok := r.entries[name]
	return e, ok
}

// All returns every entry.
func (r *Registry) All() []Entry {
	out := make([]Entry, 0, len(r.entries))
	for _, e := range r.entries {
		out = append(out, e)
	}
	return out
}

// ByCapability returns entries whose Capabilities match the given glob pattern
// (an entry matches if any of its capability patterns match via the guard's glob).
func (r *Registry) ByCapability(want string) []Entry {
	g := guard.New()
	var out []Entry
	for _, e := range r.entries {
		for _, c := range e.Capabilities {
			if ok, _ := matchCapability(g, c, want); ok {
				out = append(out, e)
				break
			}
		}
	}
	return out
}

// matchCapability checks whether `want` is covered by capability pattern `c`.
func matchCapability(g *guard.Guard, c, want string) (bool, error) {
	// reuse guard's glob by constructing a profile that allows c and checking want
	dec := g.Check(guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{c}}},
		guard.Action{Tool: want})
	return dec.IsAllowed(), nil
}
