# M4 ACP Real-CLI E2E (deferred)

The ACP client (`internal/acp`) is fully covered by the in-process `FakeAgent` tests
(initialize, session/new, session/prompt streaming, inbound fs/terminal/permission gating,
cancel/close, launch descriptors, spawn argv wiring). Real-CLI end-to-end was NOT run and is
gated on the following — none of which block M4 acceptance:

- **npx + network**: `claudecode`/`codex` launch via `npx @agentclientprotocol/{claude-agent-acp,codex-acp}`,
  which must be fetched on first run. `opencode` is not installed locally.
- **Provider auth**: the `npx` wrappers drive the underlying Claude Code / Codex CLI, which
  expects credentials for Anthropic's / OpenAI's API (or a logged-in account). A custom
  NewAPI gateway base URL is not necessarily honored by the wrapper.
- **Caps advertised**: `Spawn` advertises `fs.read/write` + `terminal` capabilities, so the
  agent MAY route fs/terminal through the client (where the `GuardPolicy` gates them). Native
  agents may also touch the disk directly — ACP enforcement is cooperative, not a sandbox
  (see spec §docs/superpowers/specs/2026-07-14-autocode-agent-design.md §3.3, §6).

When a real key + npx are available, run:
```
go test ./internal/acp/ -run TestSpawnRealClaude -tags acp_e2e
```
(add such a build-taged test that calls `Spawn(ctx, "claudecode", tmpDir, nil)` →
`Prompt("say hello in one word")` → asserts a non-empty stopReason + ≥1 text event).

The client-side logic this would exercise is already proven by the fake-agent suite.
