// Package main implements featurestatus, a reader for
// docs/feature-status.yaml — S0's feature status ledger. It prints the
// overall tally and a per-work-package breakdown, so progress on "how much
// of the planned surface actually works" is a command rather than a
// 4-hour audit. Used for ad-hoc governance checks; not used at runtime.
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

type entry struct {
	ID      string `yaml:"id"`
	Package string `yaml:"package"`
	Verdict string `yaml:"verdict"`
	Title   string `yaml:"title"`
	// Evidence is decoded as a raw node because the ledger uses two shapes for
	// it — a scalar for open entries and a clause-number mapping for terminal
	// ones (see the GOV8 gate in internal/archtest/status_evidence_test.go).
	// This tool only tallies verdicts and never reads it; a concrete type here
	// would just make the counter fail whenever the evidence schema moves.
	Evidence yaml.Node `yaml:"evidence"`
}

func isTerminal(v string) bool { return v == "done" || v == "removed" }

// pkgOrder sorts W1..W10 numerically ("-" last) rather than lexically, which
// would put W10 between W1 and W2.
func pkgOrder(p string) int {
	if !strings.HasPrefix(p, "W") {
		return 1 << 30
	}
	n, err := strconv.Atoi(p[1:])
	if err != nil {
		return 1 << 30
	}
	return n
}

func main() {
	path := flag.String("f", "docs/feature-status.yaml", "path to the ledger")
	openOnly := flag.Bool("open", false, "list only entries not yet in a terminal state")
	flag.Parse()

	data, err := os.ReadFile(*path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "featurestatus:", err)
		os.Exit(1)
	}
	var entries []entry
	if err := yaml.Unmarshal(data, &entries); err != nil {
		fmt.Fprintln(os.Stderr, "featurestatus: bad YAML:", err)
		os.Exit(1)
	}

	if *openOnly {
		var pending []entry
		for _, e := range entries {
			if !isTerminal(e.Verdict) {
				pending = append(pending, e)
			}
		}
		sort.Slice(pending, func(i, j int) bool {
			if pkgOrder(pending[i].Package) != pkgOrder(pending[j].Package) {
				return pkgOrder(pending[i].Package) < pkgOrder(pending[j].Package)
			}
			return pending[i].ID < pending[j].ID
		})
		for _, e := range pending {
			fmt.Printf("%-4s %-18s %-10s %s\n", e.Package, e.ID, e.Verdict, e.Title)
		}
		fmt.Printf("\n%d open of %d\n", len(pending), len(entries))
		return
	}

	type stat struct{ total, terminal int }
	byPkg := map[string]*stat{}
	byVerdict := map[string]int{}
	overall := stat{}
	for _, e := range entries {
		if byPkg[e.Package] == nil {
			byPkg[e.Package] = &stat{}
		}
		byPkg[e.Package].total++
		byVerdict[e.Verdict]++
		overall.total++
		if isTerminal(e.Verdict) {
			byPkg[e.Package].terminal++
			overall.terminal++
		}
	}

	pkgs := make([]string, 0, len(byPkg))
	for p := range byPkg {
		pkgs = append(pkgs, p)
	}
	sort.Slice(pkgs, func(i, j int) bool { return pkgOrder(pkgs[i]) < pkgOrder(pkgs[j]) })

	fmt.Println("S0 feature status")
	fmt.Println("=================")
	fmt.Printf("terminal: %d/%d (%.0f%%)\n\n",
		overall.terminal, overall.total,
		100*float64(overall.terminal)/float64(overall.total))

	fmt.Println("by work package:")
	for _, p := range pkgs {
		s := byPkg[p]
		fmt.Printf("  %-4s %2d/%2d\n", p, s.terminal, s.total)
	}

	fmt.Println("\nby verdict:")
	verdicts := make([]string, 0, len(byVerdict))
	for v := range byVerdict {
		verdicts = append(verdicts, v)
	}
	sort.Strings(verdicts)
	for _, v := range verdicts {
		fmt.Printf("  %-10s %2d\n", v, byVerdict[v])
	}
}
