# Commit Convention

yanshi uses a [Conventional Commits](https://www.conventionalcommits.org/) subset.
The CHANGELOG is generated from these prefixes by git-cliff (`cliff.toml`),
so a clean prefix + scope keeps the release notes accurate. This is enforced by
review and CHANGELOG proofreading, **not** by a commit-lint tool (the repo ships
no golangci-lint config).

## Prefixes

| prefix | meaning | CHANGELOG group |
|---|---|---|
| `feat` | new user-facing capability | Features |
| any type + `!` (`feat!`, `fix(api)!`, …) | breaking change (or footer `BREAKING CHANGE:`) | ⚠ Breaking Changes |
| `fix` | bug fix | Bug Fixes |
| `perf` | performance improvement | Performance |
| `refactor` | code restructure, no behavior change | Refactor |
| `docs` | documentation only | Documentation |
| `test` | test-only change | Tests |
| `chore` / `ci` / `build` / `style` | tooling, CI, build, formatting | Maintenance |
| `revert` | revert a prior commit | Reverted |

`chore(release): ...` is skipped by git-cliff (release commits don't clutter the log).

This table is the contract `cliff.toml` implements; when they disagree, the
config has been the one that drifted. Two examples, both found in W10: the
breaking-change rule was `^feat!`, so `fix(api)!: drop ThreadSnapshot.Items`
shipped as an ordinary bug fix; and `style` had no rule at all, so
`style: gofmt the whole tree` landed in a free-floating section outside the
ordering every other group declares.

## Scope

Use a domain code matching the area of the codebase, consistent with existing
history: `doctor`, `config`, `auth`, `secrets`, `orchestrator`, `vcs`, `version`,
`bootstrap`, `tui`, `guard`, `api`, `appserver`, `cli`, etc.

## Examples

    feat(version): parse semver and make Version overridable via ldflags
    fix(doctor): keep sandbox check honest about S08 gap
    feat(config)!: require schema_version on load

    BREAKING CHANGE: configs without schema_version now default to 1 and warn.

## Breaking changes

Prefer the `!` form (`feat(scope)!: ...`). If a multi-line justification is
needed, add a `BREAKING CHANGE:` footer — git-cliff groups either form under
⚠ Breaking Changes. A breaking change must bump `SupportedSchemaVersion` in
`internal/config/config.go` and be called out in `docs/upgrade-guide.md`.
