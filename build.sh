#!/usr/bin/env bash
# Build yanshi. Two modes:
#   ./build.sh            dev build: inject only BuildStamp; Version stays at the
#                         in-source default (0.4.0). This is what `go build` users get.
#   ./build.sh release    release build: inject Version from the latest semver git
#                         tag (`git describe --tags --abbrev=0 --match 'v[0-9]*'`),
#                         stripping the leading "v". Falls back to the dev default
#                         when no v* tag exists. CI releases use goreleaser, not
#                         this script — but this path keeps a local release build
#                         reproducible without it. Requires bash (Git Bash on Windows).
set -euo pipefail

stamp=$(date +%y%m%d%H%M)
ldflags="-X github.com/x6nux/yanshi/internal/version.BuildStamp=${stamp}"
out=yanshi.exe

if [[ "${1:-}" == "release" || "${RELEASE:-}" == "1" ]]; then
  # --match 'v[0-9]*' restricts version injection to semver tags.
  #
  # It used to say this "skips the m1..m9 milestone tag namespace" — those tags
  # do not exist. This repository has NO tags at all (`git tag | wc -l` = 0),
  # so the fallback branch below is the only path this script has ever taken.
  # The match pattern is still right: it is what keeps a future non-semver tag
  # out of the version string, and it mirrors cliff.toml's tag_pattern.
  if ver=$(git describe --tags --abbrev=0 --match 'v[0-9]*' 2>/dev/null); then
    ver=${ver#v}
    ldflags="${ldflags} -X github.com/x6nux/yanshi/internal/version.Version=${ver}"
    echo "release build: version=${ver} stamp=${stamp}"
  else
    echo "release build: no v* semver tag found; falling back to in-source default"
  fi
else
  echo "dev build: stamp=${stamp}"
fi

go build -ldflags "${ldflags}" -o "${out}" ./cmd/yanshi
echo "built ${out}"
