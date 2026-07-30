package guard

import (
	"regexp"
	"strings"
)

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
