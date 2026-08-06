package skills

import (
	"path/filepath"
	"testing"
)

// TestShippedExampleSkillLoads runs the example skill through the real loader.
//
// examples/custom-skill is one of two examples no CI step touched — and unlike
// the others it has no run.sh to add, because it is a SKILL.md rather than a
// program. Asserting it here is stronger than a shell step anyway: it proves
// the frontmatter this repo tells authors to write is the frontmatter the
// loader actually accepts, rather than proving a file exists.
//
// ledger: H2/EX1#2 可跑
func TestShippedExampleSkillLoads(t *testing.T) {
	root := filepath.Join("..", "..", "examples", "custom-skill")
	reg, err := NewLoader(User(root)).Load()
	if err != nil {
		t.Fatalf("the example skill directory does not load: %v", err)
	}
	skill, ok := reg.Get("reverse-echo")
	if !ok {
		names := make([]string, 0)
		for _, s := range reg.List() {
			names = append(names, s.Name)
		}
		t.Fatalf("examples/custom-skill/reverse-echo did not load; registry holds %v", names)
	}
	if skill.Description == "" {
		t.Error("the example skill has no description; the loader lists skills to the " +
			"model by name AND description, so an empty one is unusable in practice")
	}
	body, err := reg.Body(skill)
	if err != nil || body == "" {
		t.Errorf("the example skill has no body: %v", err)
	}
}
