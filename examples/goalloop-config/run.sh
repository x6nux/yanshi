#!/usr/bin/env bash
# Two-iteration fake-model goal-loop demo. Zero API keys / CLIs.
#
# Builds yanshi, ensures a loadable config exists, runs the goal loop with a
# 2-iteration budget, and asserts it produced output.
set -euo pipefail

repo="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$repo"

go build -o yanshi ./cmd/yanshi

if [ ! -f config.yaml ]; then
  printf 'server:\n  http_addr: "127.0.0.1:0"\nstorage:\n  sqlite_path: ":memory:"\n' > config.yaml
fi

out=$(./yanshi goal --fake-model --max-iters 2 -goal "add a smoke test" 2>&1 || true)
echo "$out"
if [ -z "$out" ]; then
  echo "goalloop-config: empty output" >&2
  exit 1
fi
# The fake loop prints an iter line per iteration and a final decision line.
if ! echo "$out" | grep -q "decision:"; then
  echo "goalloop-config: no decision line" >&2
  exit 1
fi
echo "goalloop-config OK"
