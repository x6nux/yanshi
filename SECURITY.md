# Security Policy

## Supported Versions

We currently provide security updates for the latest release only.

| Version | Supported          |
| ------- | ------------------ |
| latest  | :white_check_mark: |
| < latest | :x:               |

## Reporting a Vulnerability

If you discover a security vulnerability in **yanshi**, please report it privately.

**Do not** file a public GitHub issue — instead, send details to:

- **Email**: xunl47236@gmail.com
- **GitHub Advisory**: https://github.com/x6nux/yanshi/security/advisories/new

Your report will be acknowledged within 48 hours. We ask that you allow up to 90 days for a fix before disclosing the vulnerability publicly.

## Scope

- **In scope**: the Go server (`cmd/yanshi`), guard subsystem (`internal/guard`), VCS (`internal/vcs`), WebSocket / SSE API, TUI client, and the build/release pipeline.
- **Out of scope**: transitive dependencies, documentation typos, theoretical attacks requiring physical access.

## Vulnerability Disclosure Process

1. Reporter submits a private advisory.
2. Maintainer triages within 48 hours.
3. A fix branch is prepared and tested.
4. A new release is published with the fix, and the advisory is made public.

## Preferred Languages

We prefer English for security communications, but Chinese is also welcome.
