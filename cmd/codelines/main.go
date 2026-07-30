// Package main implements codelines, a pure-code-line counter for yanshi. It
// walks internal/ and, for each non-test .go file, counts code lines
// (excluding blank lines and //-comment lines — the same caliber as the
// CLAUDE.md 1000-line rule), printing each file's count sorted descending and
// flagging any over 1000. Used for ad-hoc governance checks; not used at
// runtime.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

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
	filepath.Walk(filepath.Join(root, "internal"), func(path string, info os.FileInfo, err error) error {
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
	sort.Slice(files, func(i, j int) bool { return files[i].lines > files[j].lines })
	for _, f := range files {
		flag := ""
		if f.lines > 1000 {
			flag = " *** OVER 1000"
		}
		fmt.Printf("%-70s %d%s\n", f.path, f.lines, flag)
	}
}
