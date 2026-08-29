package guard

import (
	"regexp"
	"strings"
)

// GlobCovers reports whether some pattern in patterns PROVABLY grants
// everything candidate would grant.
//
// "Provably" is the operative word and is why this is not a glob match. Three
// cases are accepted:
//
//   - A universal pattern ("*" / "**") grants everything.
//   - An exact string match: candidate IS one of the patterns.
//   - A candidate LITERAL (no glob metacharacter) that some pattern matches.
//
// A candidate that itself contains wildcards and is not literally present is
// REJECTED even when it looks narrower, because glob containment is not
// decidable by matching: `fs_r*` is not matched by `fs_read` yet grants
// strictly more than it, and — the direction that actually bites —
// `fs_*` IS matched by the pattern `fs_?` while granting strictly more than it.
// A plain match test therefore admits a candidate wider than the set it was
// checked against.
//
// Rejecting is the conservative direction: the candidate is dropped, so the
// intersection loses a permission rather than gaining one.
//
// # Why it lives in guard rather than at either call site
//
// It had two callers and one implementation. config.coveredByAny (the trusted
// policy file × local config narrowing) got the criterion right; the sub-agent
// role narrowing (tools.intersectToolSets) used a bidirectional
// filepath.Match and silently kept the wider side. The predicate belongs where
// MatchGlob is, so the two narrowings cannot disagree about what "narrower"
// means — which is the whole failure W-B-19 names.
func GlobCovers(patterns []string, candidate string) bool {
	for _, p := range patterns {
		if p == "*" || p == "**" || p == candidate {
			return true
		}
	}
	if HasGlobMeta(candidate) {
		return false
	}
	for _, p := range patterns {
		if ok, err := MatchGlob(p, candidate); err == nil && ok {
			return true
		}
	}
	return false
}

// HasGlobMeta reports whether s contains a glob metacharacter, i.e. whether it
// denotes a SET of names rather than one name. Exported alongside GlobCovers
// because callers building an error message need to say which side of that
// line an entry fell on.
func HasGlobMeta(s string) bool {
	return strings.ContainsAny(s, "*?[]")
}

// MatchGlob reports whether name matches a glob pattern.
// Supported wildcards:
//   - "**" matches any sequence including path separators.
//   - "*" matches any sequence except "/" when followed by more pattern
//     (within-segment wildcard). A trailing "*" (last char of the pattern)
//     matches any remaining characters including "/", acting as a
//     "rest-of-input" wildcard for tool, shell, and host suffixes.
//   - "?" matches a single non-"/" char.
//
// Trailing "*" semantics: A pattern ending with "*" (e.g., "go *") becomes ".*"
// to match tool/shell/host suffixes that may contain "/" (e.g., "go build ./...").
// WARNING: For filesystem path patterns, this means a trailing "*" (e.g.,
// "D:/code/*") will match the entire subtree, which may over-grant permission.
// Use "**" for recursive path matching (e.g., "D:/code/**") and avoid trailing
// single "*" on path patterns.
//
// "**/foo" matches "something/foo" but NOT bare "foo" at root (unlike gitignore)
// because the literal "/" around "**" is still required.
func MatchGlob(pattern, name string) (bool, error) {
	re, err := globToRegexp(pattern)
	if err != nil {
		return false, err
	}
	return re.MatchString(name), nil
}

func globToRegexp(pattern string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); {
		c := pattern[i]
		switch c {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				b.WriteString(".*") // ** : match across separators
				i += 2
				continue
			}
			if i+1 == len(pattern) {
				b.WriteString(".*") // trailing * : match rest of input
			} else {
				b.WriteString("[^/]*") // * : within a segment
			}
		case '?':
			b.WriteString("[^/]")
		default:
			if strings.IndexByte(`\.+()|[]{}^$`, c) >= 0 {
				b.WriteByte('\\')
			}
			b.WriteByte(c)
		}
		i++
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}
