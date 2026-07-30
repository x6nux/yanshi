# Upgrade Guide

This guide covers yanshi's config schema versioning, how to upgrade between
versions, and the release runbook (including the `doctor --release`
self-check). For the commit conventions that feed the CHANGELOG, see
`docs/commit-convention.md`.

## schema_version mechanism

`internal/config/config.go` defines `SupportedSchemaVersion` (currently `1`).
`Load` gates every config on it:

| config's `schema_version` | Load behavior |
|---|---|
| omitted (or `0`) | normalized to `SupportedSchemaVersion` — **forward-compatible** (A–D evolution was purely additive, so a missing field is safe) |
| equal to `SupportedSchemaVersion` | loaded as-is |
| lower than `SupportedSchemaVersion` | migrated in-memory via `MigrateConfig` (no disk rewrite; the user's file is untouched) |
| higher than `SupportedSchemaVersion` | **rejected** — the user must upgrade yanshi first |

`Load` **never rewrites the user's disk file**. Migration is in-memory only;
callers that want to persist a migrated config must do so explicitly.

### A–D → v1.0 path

All config evolution through batches A–D was additive (new optional fields with
safe defaults). There is **no destructive migration** at v1. First-time
install: copy `config.example.yaml` → `config.yaml` and edit. Existing configs:
they keep working (missing `schema_version` reads as the current version).

### When to bump the schema

Bump `SupportedSchemaVersion` **only** on the first destructive config change
(a renamed/removed field, a changed default semantics that isn't backward-
compatible). When you do:

1. Increment `SupportedSchemaVersion` in `internal/config/config.go`.
2. Add a `case` to `MigrateConfig` that transforms the old shape into the new one.
3. Add a `BREAKING CHANGE:` footer to the commit (see `docs/commit-convention.md`)
   so git-cliff groups it under ⚠ Breaking Changes.
4. Update this guide with the migration notes.

### Field deprecation policy

Deprecate → warn for N releases → remove (removal is a schema bump). A
deprecated field should emit a `doctor` warn so operators notice.

## Release runbook

Before cutting a release:

1. **Run the release self-check.** `yanshi doctor --release` must exit `0`.
   - Exit `2` (fail): **do not release**. A release-blocking check failed
     (e.g. the configured port is in use, or the config's `schema_version` is
     rejected by this build).
   - Exit `1` (warn): release only after a human confirms each warn is
     acceptable (e.g. `wal` warns "WAL not active" on a pre-F1 build — fine if
     you intend to ship without WAL; not fine if F1 is supposed to be in).
   - `--release` promotes release-blocking warns (port-in-use, config-version
     anomalies) to fails. Without `--release` those are only warns.

2. **Tag a semver release.** `git tag v1.0.0 && git push origin HEAD:main --tags`.
   The tag must match `v[0-9]*` (the `v` prefix + digits) so the
   version-injection path (`git describe --match 'v[0-9]*'`) and git-cliff
   (`tag_pattern = "v[0-9]*"` in `cliff.toml`) pick it up. Milestone tags
   (`m1`..`m9`) are deliberately skipped.

3. **The `release.yml` workflow fires on the tag push.** It:
   - runs git-cliff to generate release notes from Conventional Commits,
   - builds four `CGO_ENABLED=0 -tags nokeyring` targets
     (windows/amd64, linux/amd64, linux/arm64, darwin/arm64) via goreleaser,
   - attaches `checksums.txt` (SHA256),
   - publishes the GitHub Release with the git-cliff notes.

4. **Verify the Release.** It must contain four archives + `checksums.txt` +
   the git-cliff-generated body. Download the windows/amd64 archive and run
   `yanshi -h` (exit 0) as a smoke check. Full TUI keybinding verification
   (Ctrl+Enter vs Enter) requires a real TTY — that's a manual checklist item,
   not automatable in CI (alt-screen TUI can't be pipe-driven; see CLAUDE.md).

## Checksum verification (for users)

v1 release artifacts are **not code-signed** (see non-goals in the spec). After
downloading, verify the SHA256 checksum:

```sh
sha256sum -c checksums.txt   # GNU coreutils
# or on macOS:
shasum -a 256 -c checksums.txt
```

A mismatch means the download was corrupted or tampered with — re-download or
do not use.
