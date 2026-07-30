package instruct

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func write(t *testing.T, path, body string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
}

func TestLoadHierarchical_ParentChildMergeOrder(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "AGENTS.md"), "ROOT")
	write(t, filepath.Join(root, "sub", "AGENTS.md"), "SUB")

	got := LoadHierarchical(root, filepath.Join(root, "sub"))
	assert.Contains(t, got, "ROOT")
	assert.Contains(t, got, "SUB")
	// Parent first, child after (child overrides by appearing later).
	assert.True(t, strings.Index(got, "ROOT") < strings.Index(got, "SUB"),
		"parent must precede child: %q", got)
}

func TestLoadHierarchical_PerLevelFallback(t *testing.T) {
	root := t.TempDir()
	// Root has only CLAUDE.md; sub has AGENTS.md; sub2 has AGENT.md.
	write(t, filepath.Join(root, "CLAUDE.md"), "ROOT-CLAUDE")
	write(t, filepath.Join(root, "sub", "AGENTS.md"), "SUB-AGENTS")
	write(t, filepath.Join(root, "sub2", "AGENT.md"), "SUB2-AGENT")

	got := LoadHierarchical(root, filepath.Join(root, "sub"))
	assert.Contains(t, got, "ROOT-CLAUDE", "root level falls back to CLAUDE.md")
	assert.Contains(t, got, "SUB-AGENTS", "AGENTS.md wins at sub")

	got2 := LoadHierarchical(root, filepath.Join(root, "sub2"))
	assert.Contains(t, got2, "SUB2-AGENT", "AGENT.md fallback at sub2")
	assert.False(t, strings.Contains(got2, "SUB-AGENTS"), "sibling dirs must NOT bleed in")
}

func TestLoadHierarchical_AgentsMdPreferredOverAgentMd(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "AGENTS.md"), "PLURAL")
	write(t, filepath.Join(dir, "AGENT.md"), "SINGULAR")
	got := LoadHierarchical(dir, dir)
	assert.Contains(t, got, "PLURAL")
	assert.False(t, strings.Contains(got, "SINGULAR"), "AGENTS.md must win over AGENT.md")
}

func TestLoadHierarchical_DifferentTargetsDifferentChains(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "AGENTS.md"), "ROOT")
	write(t, filepath.Join(root, "a", "AGENTS.md"), "A")
	write(t, filepath.Join(root, "b", "AGENTS.md"), "B")

	gotA := LoadHierarchical(root, filepath.Join(root, "a"))
	gotB := LoadHierarchical(root, filepath.Join(root, "b"))
	assert.Contains(t, gotA, "A")
	assert.False(t, strings.Contains(gotA, "B"), "chain for a/ must not include b/")
	assert.Contains(t, gotB, "B")
	assert.False(t, strings.Contains(gotB, "A"), "chain for b/ must not include a/")
}

func TestLoadHierarchical_EmptyWhenNoFile(t *testing.T) {
	root := t.TempDir()
	assert.Empty(t, LoadHierarchical(root, root))
	assert.Empty(t, LoadHierarchical(root, filepath.Join(root, "sub")))
}

func TestLoadHierarchical_ItemCapTruncates(t *testing.T) {
	root := t.TempDir()
	big := strings.Repeat("X", maxItemBytes+500)
	write(t, filepath.Join(root, "AGENTS.md"), big)
	got := LoadHierarchical(root, root)
	assert.True(t, len(got) < len(big), "single item must be truncated to <= cap+marker")
	assert.Contains(t, got, "truncated")
}

func TestLoadHierarchical_TotalCapTruncates(t *testing.T) {
	root := t.TempDir()
	// Several NESTED levels each near the item cap so the ancestor chain
	// root→.../e carries multiple files and the running total exceeds the total
	// cap partway through. (Siblings would not be on the chain and so could not
	// exercise the total cap — see TestLoadHierarchical_DifferentTargetsDifferentChains.)
	chunk := strings.Repeat("Y", maxItemBytes)
	dir := root
	for _, sub := range []string{"a", "b", "c", "d", "e"} {
		dir = filepath.Join(dir, sub)
		write(t, filepath.Join(dir, "AGENTS.md"), chunk)
	}
	got := LoadHierarchical(root, dir)
	assert.LessOrEqual(t, len(got), maxTotalBytes+200, "total must be capped near maxTotalBytes")
	assert.Contains(t, got, "truncated")
}

func TestNestedInstructions_ExcludesRoot(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "AGENTS.md"), "ROOT")
	write(t, filepath.Join(root, "sub", "AGENTS.md"), "SUB")

	got := NestedInstructions(root, filepath.Join(root, "sub"))
	assert.Contains(t, got, "SUB")
	assert.False(t, strings.Contains(got, "ROOT"),
		"NestedInstructions must exclude root level (already in system prompt)")
}

func TestNestedInstructions_EmptyForRootLevelFile(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "AGENTS.md"), "ROOT")
	// Target dir == root: no nested levels -> empty.
	assert.Empty(t, NestedInstructions(root, root))
}

func TestLoadHierarchical_TargetOutsideRootFallsBackToRoot(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "AGENTS.md"), "ROOT")
	outside := t.TempDir()
	write(t, filepath.Join(outside, "AGENTS.md"), "OUTSIDE")
	// Target dir outside root must NOT pull in outside content; falls back to root.
	got := LoadHierarchical(root, outside)
	assert.Contains(t, got, "ROOT")
	assert.False(t, strings.Contains(got, "OUTSIDE"))
}
