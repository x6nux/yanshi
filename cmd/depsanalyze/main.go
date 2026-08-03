// Package main implements depsanalyze, a dependency-graph analysis tool for
// yanshi's internal packages. It runs `go list -json ./internal/...`, decodes
// each package's imports/files, and prints fan-in/fan-out tables, dependency
// layers, size outliers, test-coverage ratios, and risk flags (high fan-out,
// over-1000-line packages, low test coverage, sinks, external/supply-chain
// deps). Output is plain text for ad-hoc analysis; not used at runtime.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// Package is the subset of `go list -json` fields that depsanalyze decodes:
// the package's import path/name/dir, its import lists, and its Go file lists
// (production + test). Only these fields are consumed; the rest of the go list
// output is ignored.
type Package struct {
	ImportPath  string   `json:"ImportPath"`
	Imports     []string `json:"Imports"`
	Name        string   `json:"Name"`
	Dir         string   `json:"Dir"`
	TestImports []string `json:"TestImports"`
	GoFiles     []string `json:"GoFiles"`
	TestGoFiles []string `json:"TestGoFiles"`
}

func main() {
	root := "."
	if len(os.Args) > 1 {
		root = os.Args[1]
	}
	cmd := exec.Command("go", "list", "-json", "./internal/...")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "go list failed: %v\n%s\n", err, string(out))
		os.Exit(1)
	}
	dec := json.NewDecoder(strings.NewReader(string(out)))
	var packages []Package
	for {
		var pkg Package
		if err := dec.Decode(&pkg); err != nil {
			break
		}
		if strings.Contains(pkg.ImportPath, "/test") || strings.Contains(pkg.ImportPath, "/testdata") {
			continue
		}
		packages = append(packages, pkg)
	}

	type Info struct {
		ShortPath string
		Files     int
		Lines     int
		TestFiles int
		TestLines int
		Imports   []string
		IntDeps   []string
	}

	var infos []Info
	depMap := map[string][]string{}

	for _, pkg := range packages {
		short := strings.TrimPrefix(pkg.ImportPath, "github.com/x6nux/yanshi/")
		var intDeps []string
		for _, imp := range pkg.Imports {
			if strings.HasPrefix(imp, "github.com/x6nux/yanshi/internal") && imp != pkg.ImportPath {
				intDeps = append(intDeps, strings.TrimPrefix(imp, "github.com/x6nux/yanshi/internal/"))
			}
		}
		depMap[short] = intDeps

		lines := 0
		for _, f := range pkg.GoFiles {
			l, _ := countLines(filepath.Join(pkg.Dir, f))
			lines += l
		}
		tlines := 0
		for _, f := range pkg.TestGoFiles {
			l, _ := countLines(filepath.Join(pkg.Dir, f))
			tlines += l
		}

		// collect all imports (internal + external) for full analysis
		infos = append(infos, Info{
			ShortPath: short,
			Files:     len(pkg.GoFiles),
			Lines:     lines,
			TestFiles: len(pkg.TestGoFiles),
			TestLines: tlines,
			Imports:   pkg.Imports,
			IntDeps:   intDeps,
		})
	}

	sort.Slice(infos, func(i, j int) bool { return infos[i].ShortPath < infos[j].ShortPath })

	// 1. Raw dependency table
	fmt.Println("=== RAW DEPENDENCY TABLE (production code only) ===")
	fmt.Printf("%-45s %-6s %-8s %s\n", "Package", "Files", " Lines", "InternalDeps")
	fmt.Println(strings.Repeat("-", 120))
	for _, info := range infos {
		depStr := strings.Join(info.IntDeps, ", ")
		if depStr == "" {
			depStr = "(none)"
		}
		fmt.Printf("%-45s %-6d %-8d [%s]\n", info.ShortPath, info.Files, info.Lines, depStr)
	}

	// 2. Test coverage ratios
	fmt.Println("\n\n=== TEST COVERAGE ===")
	fmt.Printf("%-45s %-10s %-10s %s\n", "Package", "ImplLines", "TestLines", "Ratio")
	for _, info := range infos {
		ratio := 0.0
		if info.Lines > 0 {
			ratio = float64(info.TestLines) / float64(info.Lines)
		}
		fmt.Printf("%-45s %-10d %-10d %.2f\n", info.ShortPath, info.Lines, info.TestLines, ratio)
	}

	// 3. Dependency count (fan-out)
	fmt.Println("\n\n=== FAN-OUT (deps per package) ===")
	var sortedByFanOut []Info
	sortedByFanOut = append(sortedByFanOut, infos...)
	sort.Slice(sortedByFanOut, func(i, j int) bool {
		return len(sortedByFanOut[i].IntDeps) > len(sortedByFanOut[j].IntDeps)
	})
	fmt.Printf("%-45s %s\n", "Package", "FanOut")
	for _, info := range sortedByFanOut {
		fmt.Printf("%-45s %d\n", info.ShortPath, len(info.IntDeps))
	}

	// 4. Fan-in (how many packages depend on each)
	fanIn := map[string]int{}
	for _, info := range infos {
		for _, dep := range info.IntDeps {
			fanIn[dep]++
		}
	}
	type fi struct {
		name  string
		count int
	}
	var fiList []fi
	for k, v := range fanIn {
		fiList = append(fiList, fi{k, v})
	}
	sort.Slice(fiList, func(i, j int) bool { return fiList[i].count > fiList[j].count })
	fmt.Println("\n\n=== FAN-IN (most-depended-upon) ===")
	fmt.Printf("%-45s %s\n", "Package", "FanIn")
	for _, f := range fiList {
		fmt.Printf("%-45s %d\n", f.name, f.count)
	}

	// 5. Size outliers
	fmt.Println("\n\n=== SIZE OUTLIERS ===")
	var sortedBySize []Info
	sortedBySize = append(sortedBySize, infos...)
	sort.Slice(sortedBySize, func(i, j int) bool {
		return sortedBySize[i].Lines > sortedBySize[j].Lines
	})
	fmt.Printf("%-45s %s\n", "Package", "ImplLines")
	for _, info := range sortedBySize {
		if info.Lines > 1000 {
			fmt.Printf("%-45s %d *** OVER 1000\n", info.ShortPath, info.Lines)
		} else {
			fmt.Printf("%-45s %d\n", info.ShortPath, info.Lines)
		}
	}

	// 6. Dependency graph for cycle analysis
	fmt.Println("\n\n=== DEPENDENCY EDGES ===")
	for _, info := range infos {
		for _, dep := range info.IntDeps {
			fmt.Printf("  %s -> %s\n", info.ShortPath, dep)
		}
	}

	// 7. Layers (sorted by dependency depth)
	fmt.Println("\n\n=== DEPENDENCY LAYERS ===")
	// compute layer: a package's layer is max(layer of dep)+1, deps with no internal deps are layer 0
	layer := map[string]int{}
	changed := true
	for changed {
		changed = false
		for _, info := range infos {
			maxDepLayer := -1
			for _, dep := range info.IntDeps {
				if l, ok := layer[dep]; ok {
					if l > maxDepLayer {
						maxDepLayer = l
					}
				}
			}
			newLayer := maxDepLayer + 1
			if newLayer != layer[info.ShortPath] {
				layer[info.ShortPath] = newLayer
				changed = true
			}
		}
	}
	// Print by layer
	var byLayer [][]string
	for _, info := range infos {
		l := layer[info.ShortPath]
		for len(byLayer) <= l {
			byLayer = append(byLayer, nil)
		}
		byLayer[l] = append(byLayer[l], info.ShortPath)
	}
	for l, pkgs := range byLayer {
		fmt.Printf("Layer %d:\n", l)
		for _, p := range pkgs {
			fmt.Printf("  %s\n", p)
		}
	}

	// 8. Risk flags
	fmt.Println("\n\n=== RISK FLAGS ===")

	// 8a. Packages with too many deps (>5 internal deps)
	fmt.Println("\nHigh fan-out (>=5 internal deps):")
	for _, info := range sortedByFanOut {
		if len(info.IntDeps) >= 5 {
			fmt.Printf("  %-45s fan_out=%d\n", info.ShortPath, len(info.IntDeps))
		}
	}

	// 8b. Packages depended upon by many (>=5)
	fmt.Println("\nHigh fan-in (>=5 dependents):")
	for _, f := range fiList {
		if f.count >= 5 {
			fmt.Printf("  %-45s fan_in=%d\n", f.name, f.count)
		}
	}

	// 8c. Packages > 1000 lines production code
	fmt.Println("\nOver 1000 lines production code:")
	for _, info := range sortedBySize {
		if info.Lines > 1000 {
			fmt.Printf("  %-45s %d lines\n", info.ShortPath, info.Lines)
		}
	}

	// 8d. Low test coverage (<0.3)
	fmt.Println("\nLow test coverage (< 0.3):")
	for _, info := range infos {
		ratio := 0.0
		if info.Lines > 0 {
			ratio = float64(info.TestLines) / float64(info.Lines)
		}
		if ratio < 0.3 && info.Lines > 100 {
			fmt.Printf("  %-45s ratio=%.2f impl=%d test=%d\n", info.ShortPath, ratio, info.Lines, info.TestLines)
		}
	}

	// 8e. No internal deps but >500 lines (might indicate missing segregation)
	fmt.Println("\nLeaf packages (>500 lines, no internal deps):")
	for _, info := range infos {
		if len(info.IntDeps) == 0 && info.Lines > 500 {
			fmt.Printf("  %-45s %d lines\n", info.ShortPath, info.Lines)
		}
	}

	// 8f. Packages with no dependents (sink)
	sinkDeps := map[string]bool{}
	for _, info := range infos {
		sinkDeps[info.ShortPath] = false
	}
	for _, info := range infos {
		for _, dep := range info.IntDeps {
			sinkDeps[dep] = true
		}
	}
	fmt.Println("\nSink packages (no one imports them internally):")
	var sinkList []string
	for k, v := range sinkDeps {
		if !v {
			sinkList = append(sinkList, k)
		}
	}
	sort.Strings(sinkList)
	for _, s := range sinkList {
		fmt.Printf("  %s\n", s)
	}

	// 8g. Packages that import external packages (risks: supply chain)
	fmt.Println("\nPackages importing external libs (supply chain risk):")
	internalPrefix := "github.com/x6nux/yanshi/"
	for _, info := range infos {
		var extDeps []string
		for _, imp := range info.Imports {
			if !strings.HasPrefix(imp, internalPrefix) {
				extDeps = append(extDeps, imp)
			}
		}
		if len(extDeps) > 0 {
			fmt.Printf("  %s:\n", info.ShortPath)
			for _, ed := range extDeps {
				fmt.Printf("    - %s\n", ed)
			}
		}
	}
}

func countLines(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, b := range data {
		if b == '\n' {
			count++
		}
	}
	return count, nil
}
