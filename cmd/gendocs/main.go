// Package main implements cmd/gendocs: a dev-time documentation generator that
// keeps user-facing docs in sync with code. It owns two modes, each producing a
// BEGIN/END GENERATED block that CI gates via `git diff --exit-code`:
//
//   - -config <path>: reflect internal/config.Config → config-skeleton table.
//   - -help-all <files...>: capture `yanshi <sub> -h` → help:<id> snapshots.
//
// cmd/api-schema owns API-schema→markdown (its -markdown mode); cmd/gendocs
// owns CLI/config→markdown. Both share internal/docgen for the block primitive.
// The generators are dev-time tools (go run ./cmd/gendocs); they are not part
// of the release yanshi binary.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/x6nux/yanshi/internal/docgen"
)

func main() {
	os.Exit(runGendocs(os.Args[1:], os.Stderr))
}

// runGendocs is the testable core of cmd/gendocs: it parses the -config and
// -help-all flags, dispatches to the config-skeleton or help-snapshot
// generator, and returns the exit code (0 ok / 1 runtime / 2 usage) instead of
// calling os.Exit. Split from main so both generation paths and the usage error
// are unit-testable.
func runGendocs(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("gendocs", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "write the config-skeleton block into this markdown file")
	helpAll := fs.Bool("help-all", false, "capture every subcommand's help and rewrite help:<id> blocks in the files given as positional args")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	switch {
	case *configPath != "":
		if err := writeConfigSkeleton(*configPath); err != nil {
			fmt.Fprintln(stderr, "gendocs:", err)
			return 1
		}
		return 0
	case *helpAll:
		if err := writeAllHelpSnapshots(fs.Args()); err != nil {
			fmt.Fprintln(stderr, "gendocs:", err)
			return 1
		}
		return 0
	default:
		fs.Usage()
		return 2
	}
}

// writeConfigSkeleton renders the config-skeleton block into path, creating the
// file (and parent dir) if absent.
func writeConfigSkeleton(path string) error {
	content := RenderConfigSkeleton()
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if mkErr := os.MkdirAll(parentDir(path), 0o755); mkErr != nil {
			return mkErr
		}
	}
	return docgen.RewriteBlock(path, configSkeletonBlockID, content)
}

// parentDir returns the directory portion of path, treating both os.PathSeparator
// and '/' as separators (so Windows and POSIX paths both work). Returns "." when
// path has no separator.
func parentDir(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == os.PathSeparator || path[i] == '/' {
			return path[:i]
		}
	}
	return "."
}
