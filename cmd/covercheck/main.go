// Package main is cmd/covercheck: a per-package statement-coverage ratchet.
//
// The repo had no coverage gate at all — `rg -- '-cover' .github/workflows/`
// returned nothing — so any of these packages could have gone from 94% to 20%
// without a single job turning red. The spec's acceptance numbers (proto 80,
// store 75, bootstrap 50) were all met with 20-45 points to spare, which means
// enforcing THOSE would have left that much free room to regress. The
// thresholds here are max(spec floor, measured - 3pp): still a contract, but a
// real ratchet.
//
// It lives in cmd/ rather than in internal/archtest deliberately. An archtest
// assertion would have to run `go test -cover` from inside `go test` — a
// self-recursive invocation if the scope is ./..., a second table to maintain
// if it is not, and a nested non-race run under the -race job. Coverage is a
// RUNTIME product of the test binary; the archtest gates are static assertions
// over source (AST, go list). A CI job is no weaker for being a job.
//
// Usage:
//
//	go run ./cmd/covercheck            # run the tests and check
//	go run ./cmd/covercheck -v         # also print each package's coverage
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// thresholds is the minimum statement coverage each package must keep.
//
// Raise a number when coverage rises durably; lowering one is a decision that
// belongs in review, not in a red-build fix. The spec floors these sit above
// are proto 80 / store 75 / bootstrap 50.
var thresholds = map[string]float64{
	"github.com/x6nux/yanshi/internal/proto":     94.0,
	"github.com/x6nux/yanshi/internal/store":     92.0,
	"github.com/x6nux/yanshi/internal/bootstrap": 91.0,
}

// coverageLine matches `go test -cover` output: the package path and percent.
var coverageLine = regexp.MustCompile(`^(?:ok|FAIL)\s+(\S+).*?coverage:\s+([0-9.]+)%`)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("covercheck", flag.ContinueOnError)
	fs.SetOutput(stderr)
	verbose := fs.Bool("v", false, "print every package's coverage, not just failures")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	pkgs := make([]string, 0, len(thresholds))
	for p := range thresholds {
		pkgs = append(pkgs, p)
	}
	sort.Strings(pkgs)

	// -count=1 defeats the test cache: a cached "ok" line carries no coverage
	// figure, so a cached run would report every package as missing and fail
	// for a reason that has nothing to do with coverage.
	cmd := exec.Command("go", append([]string{"test", "-count=1", "-cover"}, pkgs...)...)
	cmd.Stderr = stderr
	out, err := cmd.Output()
	if err != nil {
		fmt.Fprintf(stderr, "covercheck: go test failed: %v\n", err)
		fmt.Fprint(stderr, string(out))
		return 1
	}

	measured := map[string]float64{}
	for _, line := range strings.Split(string(out), "\n") {
		m := coverageLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		pct, convErr := strconv.ParseFloat(m[2], 64)
		if convErr != nil {
			continue
		}
		measured[m[1]] = pct
	}

	failed := false
	for _, pkg := range pkgs {
		want := thresholds[pkg]
		got, ok := measured[pkg]
		if !ok {
			fmt.Fprintf(stderr, "covercheck: no coverage reported for %s\n", pkg)
			failed = true
			continue
		}
		switch {
		case got < want:
			fmt.Fprintf(stderr, "covercheck: %s is at %.1f%%, below the %.1f%% floor\n",
				pkg, got, want)
			failed = true
		case *verbose:
			fmt.Fprintf(stdout, "%-50s %5.1f%%  (floor %.1f%%)\n", pkg, got, want)
		}
	}
	if failed {
		fmt.Fprintln(stderr, "\ncovercheck: coverage fell below a floor. Add tests, or "+
			"lower the floor in cmd/covercheck with a reason in the commit message.")
		return 1
	}
	fmt.Fprintf(stdout, "covercheck: %d package(s) at or above their floor\n", len(pkgs))
	return 0
}
