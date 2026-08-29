package tools

import (
	"reflect"
	"strings"
	"testing"
)

// TestSpawnRoleRefusesAnUnrepresentableIntersection is the W-B-19 acceptance:
// 无法安全求交时显式报错而非静默取宽.
//
// The shape is a role whose pattern MATCHES the caller's pattern as a string
// while denoting a strictly smaller set. Before this change intersectToolSets
// asked exactly that question — "does a pattern on the other side match this
// string" — and answered by keeping the caller's wider pattern, so a sub-agent
// under a role limited to `fs_?` came away holding `fs_*`.
//
// Both halves are asserted: nothing is handed out, AND the refusal says which
// pattern it could not express, because "denied" with no reason leaves the
// caller (usually a model) to guess again.
func TestSpawnRoleRefusesAnUnrepresentableIntersection(t *testing.T) {
	kept, unprovable := intersectToolSets([]string{"fs_?"}, []string{"fs_*"})
	if len(kept) != 0 {
		t.Fatalf("the wider caller pattern survived an intersection with a narrower role: %v", kept)
	}
	if !reflect.DeepEqual(unprovable, []string{"fs_*", "fs_?"}) {
		t.Fatalf("unprovable = %v, want both patterns named", unprovable)
	}

	// narrowRoleTools rather than resolveSpawnRole, because the shipped catalog
	// contains no pattern that can produce this shape — which is why it has to
	// be constructed rather than discovered.
	custom := RoleDef{Name: "probe", AllowedTools: []string{"fs_?"}}
	got, err := narrowRoleTools(custom, []string{"fs_*"})
	if err == nil {
		t.Fatalf("an unrepresentable intersection was accepted, yielding %v", got)
	}
	if !strings.Contains(err.Error(), "cannot be expressed") ||
		!strings.Contains(err.Error(), "fs_*") {
		t.Fatalf("refusal does not name the unexpressible pattern: %v", err)
	}
}

// TestSpawnRoleKeepsTheShippedCatalogWorking is the regression half.
//
// Tightening a containment test is the kind of change that fixes the exotic
// case and breaks every ordinary one. These are the combinations the shipped
// roles actually produce, and each asserts the exact resulting set rather than
// just "no error" — a narrowing to the empty set would also be error-free at
// this level and would silently disable the sub-agent.
func TestSpawnRoleKeepsTheShippedCatalogWorking(t *testing.T) {
	cases := []struct {
		name   string
		role   string
		caller []string
		want   []string
	}{
		{"universal role keeps a literal", "general", []string{"fs_read"}, []string{"fs_read"}},
		{"universal role keeps a wildcard", "general", []string{"fs_*"}, []string{"fs_*"}},
		{"caller wildcard selects the role's literals", "explore", []string{"fs_*"},
			[]string{"fs_read", "fs_glob", "fs_search"}},
		{"role wildcard admits a matching literal", "implementer", []string{"memory_search"},
			[]string{"memory_search"}},
		{"identical wildcards survive", "implementer", []string{"memory_*"}, []string{"memory_*"}},
		{"literal on both sides", "verifier", []string{"shell_run", "fs_read"},
			[]string{"shell_run", "fs_read"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, got, err := resolveSpawnRole(tc.role, tc.caller)
			if err != nil {
				t.Fatalf("ordinary spawn refused: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("effective tools = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestSpawnRoleStillDistinguishesDisjointFromUnrepresentable keeps the two
// refusals apart.
//
// They are different facts about the request and only one is fixable by
// rewording it. Collapsing them would put "name the tools explicitly" in front
// of a caller who asked a read-only role for a tool it will never have.
func TestSpawnRoleStillDistinguishesDisjointFromUnrepresentable(t *testing.T) {
	_, _, err := resolveSpawnRole("explore", []string{"fs_write"})
	if err == nil {
		t.Fatal("a read-only role accepted fs_write")
	}
	if strings.Contains(err.Error(), "cannot be expressed") {
		t.Fatalf("a plainly disjoint request was reported as unrepresentable: %v", err)
	}
	if !strings.Contains(err.Error(), "allows none of the requested tools") {
		t.Fatalf("unexpected refusal text: %v", err)
	}
}
