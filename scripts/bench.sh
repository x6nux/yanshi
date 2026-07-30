#!/usr/bin/env bash
# scripts/bench.sh - run F2 benchmarks and compare against the main baseline.
#
# Usage:
#   ./scripts/bench.sh           # run all F2 benchmarks, save as new baseline
#   ./scripts/bench.sh --diff    # run benchmarks, then benchstat-compare vs baseline
#
# CIG1 nightly: runs ALL sub-benchmarks (default).
# PR gate (CIG1): runs only fast subsets by passing a -bench sub-pattern.
#
# benchstat is a one-time dependency: go install golang.org/x/perf/cmd/benchstat@latest
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

BENCH_PKGS="./internal/vcs ./internal/tools ./internal/agent/orchestrator"
BENCH_ARGS="-bench=. -benchmem -count=5"
OUTPUT_DIR=".bench-results"
THRESHOLD_PCT=20  # regression alarm threshold (informational; benchstat reports deltas)
export THRESHOLD_PCT

mkdir -p "$OUTPUT_DIR"

echo "=== Running F2 benchmarks (count=5 for stable benchstat) ==="
go test $BENCH_ARGS $BENCH_PKGS 2>&1 | tee "$OUTPUT_DIR/new.txt" | grep -E "^(Benchmark|ok|PASS|FAIL)" || true

if [ "${1:-}" = "--diff" ] && [ -f "$OUTPUT_DIR/old.txt" ]; then
    echo ""
    echo "=== Comparing against baseline (old.txt → new.txt) ==="
    if command -v benchstat >/dev/null 2>&1; then
        benchstat "$OUTPUT_DIR/old.txt" "$OUTPUT_DIR/new.txt" | tee "$OUTPUT_DIR/diff.txt"
        echo ""
        echo "Review diff.txt for regressions above ${THRESHOLD_PCT}% (informational; not a hard gate)."
    else
        echo "benchstat not found — install with:"
        echo "  go install golang.org/x/perf/cmd/benchstat@latest"
        echo "Raw results saved to $OUTPUT_DIR/new.txt"
    fi
else
    cp "$OUTPUT_DIR/new.txt" "$OUTPUT_DIR/old.txt"
    echo ""
    echo "=== Saved as new baseline (.bench-results/old.txt) ==="
    echo "Run './scripts/bench.sh --diff' next time to compare against this baseline."
fi
