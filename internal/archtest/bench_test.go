package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// benchPackages are the three paths the benchmark suite covers, and the same
// three ci.yml and nightly.yml name. Kept in one place so a package added to
// one side and forgotten on the other shows up as a failure rather than as
// silently missing coverage.
var benchPackages = []string{
	"internal/vcs",
	"internal/tools",
	"internal/agent/orchestrator",
}

// benchmarkNamesIn returns every Benchmark* function declared in a package dir.
func benchmarkNamesIn(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(abs(dir))
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, filepath.Join(abs(dir), e.Name()), nil, 0)
		if err != nil {
			t.Fatalf("parse %s/%s: %v", dir, e.Name(), err)
		}
		for _, decl := range af.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Recv != nil || !strings.HasPrefix(fd.Name.Name, "Benchmark") {
				continue
			}
			names = append(names, fd.Name.Name)
		}
	}
	return names
}

// TestBenchmarksExistForTheKeyPaths asserts the three hot paths still carry a
// benchmark.
//
// A static check is the right shape here and a dynamic one is not: `go test`
// does not run benchmarks, so nothing in the default suite would notice a
// deleted Benchmark function, and running them from a Test would cost seconds
// per package to re-assert a fact the source already states.
//
// ledger: F2/BENCH1#1 关键路径有基准
func TestBenchmarksExistForTheKeyPaths(t *testing.T) {
	for _, pkg := range benchPackages {
		if got := benchmarkNamesIn(t, pkg); len(got) == 0 {
			t.Errorf("%s has no Benchmark function: the CI bench job runs this package "+
				"and would measure nothing", pkg)
		}
	}
}

// readWorkflow returns a CI workflow's text.
func readWorkflow(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(abs(filepath.Join(".github", "workflows", name)))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// TestBenchCIGateRunsEveryBenchmarkPackageAboveOneIteration pins the gate that
// found the defect it exists to prevent.
//
// BenchmarkOrchestratorTurn was broken for an unknown length of time while CI
// stayed green: the only bench job was continue-on-error, and the local habit
// was -benchtime=1x -- the single value that hid it, because the FakeModel
// exhausts its script on the SECOND iteration. So two things have to hold, and
// neither is implied by the other: the job must be a hard gate, and it must ask
// for more than one iteration.
//
// ledger: F2/BENCH1#3 大回归可发现
func TestBenchCIGateRunsEveryBenchmarkPackageAboveOneIteration(t *testing.T) {
	ci := readWorkflow(t, "ci.yml")
	line := ""
	for _, l := range strings.Split(ci, "\n") {
		if strings.Contains(l, "-bench=") {
			line = l
			break
		}
	}
	if line == "" {
		t.Fatal("ci.yml runs no benchmarks: a benchmark that fails to RUN is a broken test, " +
			"and nothing else in CI executes them")
	}
	if strings.Contains(line, "-benchtime=1x") {
		t.Error("-benchtime=1x is the one value that hides a benchmark whose second " +
			"iteration fails; it must ask for more")
	}
	if !strings.Contains(line, "-benchtime=") {
		t.Error("no -benchtime: the default 1s run is fine locally but makes the CI job's " +
			"duration depend on machine speed")
	}
	for _, pkg := range benchPackages {
		if !strings.Contains(line, "./"+pkg) {
			t.Errorf("the ci.yml bench command does not cover ./%s: %q", pkg, line)
		}
	}
	if strings.Contains(ci, "continue-on-error") {
		// Not a blanket ban -- but the bench job specifically must not carry it,
		// and this file is where that would be noticed.
		idx := strings.Index(ci, "bench-compiles")
		soft := strings.Index(ci, "continue-on-error")
		if idx >= 0 && soft > idx && soft-idx < 600 {
			t.Error("the bench job is continue-on-error again: that is exactly the state " +
				"in which a broken benchmark stayed green")
		}
	}
}

// TestNightlyRecordsTheBenchmarkTrend covers the other half: the numbers.
//
// "Does it run" is a hard fact CI asserts cheaply; "is it slower than last
// week" is a noisy measurement that would manufacture flakes as a hard gate.
// The split is deliberate, so the trend job has to exist and has to actually
// compare against a previous baseline -- a job that writes new.txt and never
// diffs it records nothing.
//
// ledger: F2/BENCH1#2 CI 记录趋势
func TestNightlyRecordsTheBenchmarkTrend(t *testing.T) {
	nightly := readWorkflow(t, "nightly.yml")
	if !strings.Contains(nightly, "benchstat") {
		t.Error("nightly runs no benchstat: without a comparison the artifact is a number " +
			"nobody diffs")
	}
	if !strings.Contains(nightly, "old.txt") || !strings.Contains(nightly, "new.txt") {
		t.Error("nightly has no old/new baseline pair: benchstat needs both to report a trend")
	}
	if !strings.Contains(nightly, "bench-results") {
		t.Error("nightly does not publish the bench-results artifact, so tomorrow's run " +
			"has nothing to compare against")
	}
}
