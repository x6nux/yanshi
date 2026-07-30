#!/usr/bin/env bash
# Minimal single-turn headless demo. Zero API keys via --fake-model.
#
# Builds yanshi, ensures a loadable config exists, runs one prompt, and
# asserts the assistant produced non-empty output.
set -euo pipefail

# Resolve repo root (examples/headless-exec -> ../../).
repo="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$repo"

go build -o yanshi ./cmd/yanshi

# config.yaml is gitignored; create a minimal no-providers one if absent so
# --fake-model loads without tripping raw-literal key validation.
if [ ! -f config.yaml ]; then
  printf 'server:\n  http_addr: "127.0.0.1:0"\nstorage:\n  sqlite_path: ":memory:"\n' > config.yaml
fi

out=$(./yanshi exec --fake-model -p "hello")
echo "$out"
if [ -z "$out" ]; then
  echo "headless-exec: empty output" >&2
  exit 1
fi
echo "headless-exec OK"
