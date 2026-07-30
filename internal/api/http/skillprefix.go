package http

import (
	"fmt"
	"sort"
	"strings"

	"github.com/x6nux/yanshi/internal/skills"
)

// resolveQuery interprets a user message: if it begins with "/skill <name>
// [task]", the named skill's body is loaded and injected as guidance for the
// turn (with the optional task appended). Returns (query, ""). For an unknown
// skill, a missing registry, or a body-load error it returns ("", errMsg). A
// plain message is returned unchanged.
func resolveQuery(reg *skills.Registry, message string) (query, errMsg string) {
	rest, ok := strings.CutPrefix(message, "/skill ")
	if !ok {
		return message, ""
	}
	rest = strings.TrimSpace(rest)
	if reg == nil {
		return "", "no skill registry available"
	}
	parts := strings.SplitN(rest, " ", 2)
	name := parts[0]
	task := ""
	if len(parts) == 2 {
		task = strings.TrimSpace(parts[1])
	}
	sk, ok := reg.Get(name)
	if !ok {
		available := make([]string, 0)
		for _, s := range reg.List() {
			available = append(available, s.Name)
		}
		sort.Strings(available)
		return "", fmt.Sprintf("unknown skill %q; available: %s", name, strings.Join(available, ", "))
	}
	body, err := reg.Body(sk)
	if err != nil {
		return "", "failed to load skill " + name + ": " + err.Error()
	}
	if task != "" {
		return body + "\n\n---\nTask: " + task, ""
	}
	return body + "\n\n---\n(Skill " + name + " loaded. Briefly acknowledge and await instructions.)", ""
}
