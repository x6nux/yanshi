// Package docgen holds the shared BEGIN/END GENERATED block primitives used by
// the dev-time documentation generators (cmd/api-schema -markdown and
// cmd/gendocs). Both generators rewrite fenced markdown blocks in place so CI
// can assert `git diff --exit-code` against the canonical source; the
// replace-or-append logic is identical, so it lives here rather than being
// copy-pasted between two package main entry points.
//
// The package is a leaf: it imports only the standard library, so it does not
// participate in any hexagonal port allowlist or the bootstrap core set.
package docgen

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// Begin returns the BEGIN GENERATED marker line for id.
func Begin(id string) string { return "<!-- BEGIN GENERATED: " + id + " -->" }

// End returns the END GENERATED marker line for id.
func End(id string) string { return "<!-- END GENERATED: " + id + " -->" }

// Wrap encloses content between the BEGIN/END markers for id on their own
// lines, so RewriteBlock can locate them line-orientedly.
func Wrap(id, content string) string {
	return Begin(id) + "\n" + content + "\n" + End(id)
}

// RewriteBlock replaces the inner content of the BEGIN/END GENERATED block
// tagged id in the file at path. When the marker is absent (including an empty
// or non-existent file) it appends a fresh block at the end. The operation is
// idempotent: two calls with the same (path, id, content) produce identical
// bytes, so CI can regenerate in place without churning the committed file.
func RewriteBlock(path, id, content string) error {
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	src := string(data)
	block := Wrap(id, content)

	if strings.Contains(src, Begin(id)) {
		// Replace the whole marker pair (inclusive) with the new block. The
		// regex is anchored on the literal markers so surrounding prose and
		// newlines are untouched.
		pat := regexp.QuoteMeta(Begin(id)) + `[\s\S]*?` + regexp.QuoteMeta(End(id))
		re := regexp.MustCompile(pat)
		src = re.ReplaceAllStringFunc(src, func(string) string { return block })
	} else {
		// Append. Guarantee exactly one blank line of separation and a single
		// trailing newline for deterministic diffs.
		switch {
		case len(src) == 0:
			src = block + "\n"
		case strings.HasSuffix(src, "\n\n"):
			src += block + "\n"
		case strings.HasSuffix(src, "\n"):
			src += "\n" + block + "\n"
		default:
			src += "\n\n" + block + "\n"
		}
	}
	return os.WriteFile(path, []byte(src), 0o644)
}
