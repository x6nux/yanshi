// Package version is the single source of truth for the yanshi version number
// and the VCS revision the binary was built from.
package version

import (
	"fmt"
	"runtime/debug"
	"strconv"
	"strings"
)

// Version is the current yanshi version. Bump this before each release.
//
// This is a var (not a const) so release builds can override it via ldflags:
//
//	go build -ldflags "-X github.com/x6nux/yanshi/internal/version.Version=1.0.0"
//
// dev builds (`go build` / `go run`) keep the default "0.4.0" below; release
// builds (goreleaser / `build.sh release`) inject the semver git tag instead.
// -X only patches string vars, never consts, hence the var declaration.
var Version = "0.4.0"

// BuildStamp is the build timestamp injected via ldflags (format YYMMDDHHMM,
// e.g. "2607210129" = 2026-07-21 01:29). Go doesn't embed the build time
// itself — vcs.time is the commit time and stays fixed across rebuilds of the
// same commit — so the build script passes the wall-clock time at compile:
//
//	go build -ldflags "-X github.com/x6nux/yanshi/internal/version.BuildStamp=$(date +%y%m%d%H%M)"
//
// Empty when built without the flag (plain "go build") — callers should omit
// the segment then. See build.sh at the repo root.
var BuildStamp string

// GitHash is the short commit hash the running binary was built from (first 7
// hex chars of vcs.revision), read once at package init via Go's build-info
// VCS stamping. A trailing "*" marks a dirty tree (uncommitted changes at build
// time). Empty when the binary wasn't built from a git repo (e.g. -buildvcs=false
// or a `go install`ed module) — callers should omit the segment then.
//
// No ldflags / go:generate needed: `go build` inside a git repo stamps the
// revision automatically (Go 1.18+); the value travels with the binary.
var GitHash = func() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	h, modified := "", false
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			if len(s.Value) >= 7 {
				h = s.Value[:7]
			}
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	if h == "" {
		return ""
	}
	if modified {
		h += "*"
	}
	return h
}()

// Semver is a parsed semantic version. Prerelease/Build are the raw substrings
// after '-' / '+' (empty when absent). Parse rejects milestone tags like
// m9-cli-tui so the release path's `git describe --match 'v[0-9]*'` semantics
// are testable without git.
type Semver struct {
	Major      int
	Minor      int
	Patch      int
	Prerelease string
	Build      string
}

// Parse parses a semver string, tolerating an optional leading "v". Returns an
// error for milestone tags, missing segments, leading zeros, or non-numeric
// version components. Used by the release path to validate that a git tag is a
// real semver before injecting it via ldflags.
func Parse(v string) (Semver, error) {
	v = strings.TrimPrefix(v, "v")
	if v == "" {
		return Semver{}, fmt.Errorf("version: empty")
	}
	// Split build metadata first (+ is not legal inside pre-release).
	var build string
	if i := strings.IndexByte(v, '+'); i >= 0 {
		build = v[i+1:]
		v = v[:i]
		if build == "" {
			return Semver{}, fmt.Errorf("version: empty build metadata")
		}
	}
	var pre string
	if i := strings.IndexByte(v, '-'); i >= 0 {
		pre = v[i+1:]
		v = v[:i]
		if pre == "" {
			return Semver{}, fmt.Errorf("version: empty pre-release")
		}
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return Semver{}, fmt.Errorf("version: want major.minor.patch, got %q", v)
	}
	var s Semver
	var err error
	if s.Major, err = parseNum(parts[0]); err != nil {
		return Semver{}, fmt.Errorf("version: major: %w", err)
	}
	if s.Minor, err = parseNum(parts[1]); err != nil {
		return Semver{}, fmt.Errorf("version: minor: %w", err)
	}
	if s.Patch, err = parseNum(parts[2]); err != nil {
		return Semver{}, fmt.Errorf("version: patch: %w", err)
	}
	s.Prerelease = pre
	s.Build = build
	return s, nil
}

// parseNum rejects non-numeric and leading-zero components ("01" is illegal in
// semver; "0" alone is fine).
func parseNum(s string) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("empty component")
	}
	if len(s) > 1 && s[0] == '0' {
		return 0, fmt.Errorf("leading zero in %q", s)
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("non-numeric %q", s)
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// String renders the canonical form (no "v" prefix).
func (s Semver) String() string {
	out := fmt.Sprintf("%d.%d.%d", s.Major, s.Minor, s.Patch)
	if s.Prerelease != "" {
		out += "-" + s.Prerelease
	}
	if s.Build != "" {
		out += "+" + s.Build
	}
	return out
}
