// Package main is cmd/api-schema: the generator for the Agent API v1
// documentation blocks in docs/api/.
//
// It parses the canonical JSON Schema (internal/api/v1.SchemaBytes, which
// returns the embedded sdk/schema/v1 document verbatim) and rewrites the
// BEGIN/END GENERATED markers in place, so `git diff --exit-code docs/` gates
// them in CI.
//
// It used to also emit sdk/ts/v1.ts and call itself "the TypeScript
// generator". It was not one: from `text := ` to the end of the function was a
// single hardcoded Go raw string literal transcribing the TypeScript
// interfaces character by character, and it never parsed anything from
// internal/api/v1. Its one point of contact with the real contract was
// `_ = v1.SchemaBytes()`, whose comment claimed to guard "against silent
// drift" — a discarded return value cannot detect anything. Running it and
// diffing against sdk/ts/v1.ts produced IDENTICAL, because the generator and
// its output were the same literal proving itself.
//
// So that half is gone rather than fixed. sdk/ts/v1.ts is a hand-maintained
// declaration file, like sdk/python's generated.py which has always said so,
// and internal/api/v1's parity test is what keeps both honest — it compares
// the field sets of all four statements of the contract and requires every
// difference to be named. That is the guard the discarded call pretended to be.
//
// Usage:
//
//	go run ./cmd/api-schema -markdown docs/api/schema.md
//	go run ./cmd/api-schema -markdown docs/api/resources.md
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

// run is the testable entry point: it parses args and runs the markdown
// generator, returning the process exit code.
func run(args []string, _, stderr io.Writer) int {
	fs := flag.NewFlagSet("api-schema", flag.ContinueOnError)
	fs.SetOutput(stderr)
	markdownPath := fs.String("markdown", "", "write generated markdown blocks to this file (schema doc or resources table source)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *markdownPath == "" {
		fmt.Fprintln(stderr, "cmd/api-schema: -markdown <path> is required")
		return 2
	}
	if err := runMarkdown(*markdownPath); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}
