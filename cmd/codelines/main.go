// Package main implements codelines, a pure-code-line counter for yanshi. It
// walks internal/ and cmd/ and, for each non-test .go file, counts code lines
// (excluding blank lines and //-comment lines — the same caliber as the
// CLAUDE.md per-file line rule), printing each file's count sorted descending
// and flagging any over the limit. Used for ad-hoc governance checks; not used
// at runtime.
//
// The walked set MATCHES GOV2's (internal/ + cmd/, non-test files only). It
// used to stop at internal/, which made this tool quietly blind to exactly the
// files GOV2 would redden on — a preflight check that cannot reproduce the
// gate's verdict trains people to trust the wrong answer. The line COUNT is
// still an approximation: this walker treats any line starting with "//" as a
// comment, where GOV2 (internal/archtest.pureCodeLines) works from parsed
// comment spans and so also discounts /* */ blocks and trailing comments. The
// gate remains the authority; this is the fast look.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// pureLineLimit mirrors internal/archtest's own pureLineLimit. It cannot be
// imported — that constant lives in a _test.go file — so this is a hand-kept
// copy, pinned by internal/archtest::TestCodelinesLimitMatchesGate. A preflight
// tool that flags a different threshold than the gate is worse than no
// preflight: it is confidently wrong in one direction or the other.
const pureLineLimit = 5000

func main() {
	root := "."
	if len(os.Args) > 1 {
		root = os.Args[1]
	}
	type fc struct {
		path  string
		lines int
	}
	var files []fc
	for _, dir := range []string{"internal", "cmd"} {
		filepath.Walk(filepath.Join(root, dir), func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return nil
			}
			codeLines := 0
			for _, line := range strings.Split(string(data), "\n") {
				trimmed := strings.TrimSpace(line)
				if trimmed != "" && !strings.HasPrefix(trimmed, "//") {
					codeLines++
				}
			}
			rel, _ := filepath.Rel(root, path)
			files = append(files, fc{rel, codeLines})
			return nil
		})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].lines > files[j].lines })
	for _, f := range files {
		flag := ""
		if f.lines > pureLineLimit {
			flag = fmt.Sprintf(" *** OVER %d", pureLineLimit)
		}
		fmt.Printf("%-70s %d%s\n", f.path, f.lines, flag)
	}
}
