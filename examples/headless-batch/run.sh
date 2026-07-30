#!/usr/bin/env bash
# JSONL batch headless demo. Zero API keys via --fake-model.
#
# Builds yanshi, ensures a loadable config exists, feeds sample.jsonl (one
# prompt per line) to exec --input jsonl, and asserts non-empty output.
set -euo pipefail

repo="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$repo"

go build -o yanshi ./cmd/yanshi

if [ ! -f config.yaml ]; then
  printf 'server:\n  http_addr: "127.0.0.1:0"\nstorage:\n  sqlite_path: ":memory:"\n' > config.yaml
fi

sample="examples/headless-batch/sample.jsonl"
out=$(./yanshi exec --fake-model --input jsonl --file "$sample")
echo "$out"
if [ -z "$out" ]; then
  echo "headless-batch: empty output" >&2
  exit 1
fi
# The fake model emits deterministic placeholder text per turn; we only assert
# the batch mechanism produced something for the JSONL inputs.
echo "headless-batch OK (processed $(grep -c . "$sample") prompts)"
