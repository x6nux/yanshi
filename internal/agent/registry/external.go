package registry

import "github.com/x6nux/yanshi/internal/guard"

// RegisterExternal is a convenience helper that registers an external ACP
// agent entry (KindExternal) with the given name, capabilities, and
// permission profile. The actual delegation wiring (spawn + prompt) is M6.
func RegisterExternal(r *Registry, name, description string, caps []string, prof guard.PermissionProfile) {
	r.Register(Entry{
		Name:         name,
		Kind:         KindExternal,
		Description:  description,
		Capabilities: caps,
		Profile:      prof,
	})
}
