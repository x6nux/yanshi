#!/usr/bin/env bash
# scripts/check-d2.sh — D2 (SDK + IDE) release gate.
#
# Runs the full TS SDK + Python SDK + VS Code extension test matrices in
# dependency order. Exits non-zero on any failure. CI should call this on
# every change to sdk/ or ide/.
#
# This script does NOT run Go tests; D2 is language-pure. Run `go test ./...`
# separately for D1 server changes.
#
# Token / file content / telemetry leak checks:
#   - No `console.log(process.env...)` anywhere in the SDK or IDE source.
#   - The OutputRenderer.fail() prints error.message only — never request
#     bodies or headers (audited in tests/test_client.py and the IDE
#     cancel-race.test.ts).
#   - The token only enters the Authorization header via Transport._headers /
#     transport.authHeaders; no test forwards it elsewhere.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

echo "== TS SDK typecheck + tests =="
npm --prefix sdk/ts run typecheck
npm --prefix sdk/ts run test

echo "== Python SDK tests =="
python -m pytest sdk/python/tests -q

echo "== Python contract entry point =="
python -m yanshi_sdk.contract

echo "== VS Code extension compile + tests =="
npm --prefix ide/vscode run compile
npm --prefix ide/vscode run test

echo ""
echo "D2 release gate OK."
