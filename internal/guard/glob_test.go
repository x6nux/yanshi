package guard

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMatchGlob(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{"D:/code/**", "D:/code/yanshi/main.go", true},
		{"D:/code/**", "D:/code/a/b/c.go", true},
		{"D:/code/**", "C:/other/x.go", false},
		{"*.go", "main.go", true},
		{"*.go", "dir/main.go", false}, // * does not cross /
		{"fs.*", "fs.write", true},
		{"fs.*", "web.search", false},
		{"*", "anything", true},
		{"**", "any/path/here", true},
		{"*.github.com", "api.github.com", true},
		{"go *", "go build ./...", true},
	}
	for _, c := range cases {
		got, err := MatchGlob(c.pattern, c.name)
		assert.NoError(t, err, "pattern=%q name=%q", c.pattern, c.name)
		assert.Equal(t, c.want, got, "pattern=%q name=%q", c.pattern, c.name)
	}
}

// TestMatchGlob_TrailingStarOnPathMatchesSubtree pins the security-relevant
// semantic that a trailing single "*" on a path pattern compiles to ".*" and
// therefore matches the ENTIRE subtree (including "/" separators), not just
// immediate children. For example, "D:/code/*" matches "D:/code/secret/deep/x.go".
// This is the documented over-grant behavior in glob.go; the recommended form
// for recursive matching is "**" (e.g., "D:/code/**"). This test guards against
// an unintended regression that would silently narrow the trailing-"*" semantic.
func TestMatchGlob_TrailingStarOnPathMatchesSubtree(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		// Trailing single "*" on a path matches the whole subtree (over-grant).
		{"D:/code/*", "D:/code/secret/deep/x.go", true},
		// The recommended recursion form "**" also matches the subtree.
		{"D:/code/**", "D:/code/secret/deep/x.go", true},
	}
	for _, c := range cases {
		got, err := MatchGlob(c.pattern, c.name)
		assert.NoError(t, err, "pattern=%q name=%q", c.pattern, c.name)
		assert.Equal(t, c.want, got, "pattern=%q name=%q", c.pattern, c.name)
	}
}

// ---------- fuzz ----------

// FuzzMatchGlob checks invariants for every (pattern, name) pair the fuzz
// engine generates.
func FuzzMatchGlob(f *testing.F) {
	// seed corpus
	seeds := []struct{ pattern, name string }{
		// IFS / shell metachar
		{"go *", "go build ./..."},
		{"go *\t;", "go build"},
		{"a|b", "a|b"},
		{"a|b", "a"},
		// glob injection
		{"*.go", "dir/main.go"},
		{"**.go", "a/b.go"},
		// ../ sequences
		{"D:/code/**", "D:/code/../etc/passwd"},
		{"a/..", "a/.."},
		// nested / repeated wildcards
		{"***", "a/b/c"},
		{"**/**", "x"},
		{"a**b**c", "aXYZbXYZc"},
		{"*?", "ab"},
		// trailing * over-grant (documented)
		{"D:/code/*", "D:/code/secret/deep/x.go"},
		// long strings (ReDoS guard)
		{"a*b*c*d*e*", "a111b222c333d444e555"},
		// empty / boundary
		{"", ""},
		{"", "x"},
		{"?", ""},
		// non-ASCII / UTF-8 boundary
		{"中文*", "中文测试"},
		{"*\xff\xfe", "\xff\xfe"},
		// ReDoS guard
		{strings.Repeat("*", 1000), strings.Repeat("a", 10000)},
	}
	for _, s := range seeds {
		f.Add(s.pattern, s.name)
	}

	f.Fuzz(func(t *testing.T, pattern, name string) {
		// Invariant 1: never panic
		matched, err := MatchGlob(pattern, name)

		// Invariant 4: error = compile failure, return early
		if err != nil {
			return
		}

		// Invariant 2: for literal patterns, MatchGlob == exact equals
		if !hasGlobMeta(pattern) {
			want := pattern == name
			if matched != want {
				t.Errorf("literal pattern=%q name=%q: MatchGlob=%v, want=%v", pattern, name, matched, want)
			}
		}

		// Invariant 3: ** matches any string without newlines (Go's .* does not cross \n).
		if pattern == "**" && !strings.Contains(name, "\n") && !matched {
			t.Errorf("** must match any non-newline string: pattern=%q name=%q", pattern, name)
		}
	})
}

// hasGlobMeta reports whether pattern contains glob wildcards (*, ?, [).
func hasGlobMeta(pattern string) bool {
	return strings.ContainsAny(pattern, "*?[")
}
