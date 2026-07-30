# Yanshi VS Code Extension

Run [Yanshi](https://github.com/x6nux/yanshi) agent turns from inside VS Code.
Streams the assistant's response to an OutputChannel, renders file changes as
in-memory diffs, and survives transient disconnects by replaying items from
the last seen sequence.

## Prerequisites

1. A running Yanshi server exposing the D1 v1 Agent API at `/api/v1/*`
   (today: HTTP + SSE; JSON-RPC app-server optional). The legacy
   `/api/v1/chat` SSE/WS routes are NOT this contract; they remain as the
   TUI's transport.
2. VS Code 1.92+ on Node 20+.
3. The `@x6nux/yanshi-sdk` npm package; this extension ships it in the VSIX.

## Build & package

```sh
# From the repo root:
npm --prefix sdk/ts run build          # build the SDK first
npm --prefix ide/vscode install
npm --prefix ide/vscode run package    # produces ide/vscode/yanshi-vscode-0.1.0.vsix
```

Install the VSIX manually (`Extensions: Install from VSIX`) until the
extension lands on the Marketplace.

## Configuration

Open VS Code Settings and configure:

- `yanshi.serverUrl` — origin of the Yanshi server (default:
  `http://127.0.0.1:8080`).
- `yanshi.streamTransport` — `"sse"` (D1 today) or `"ws"` (forward-looking,
  not yet served by D1).
- `yanshi.maxOpenFiles` — max open workspace files attached as IDE context
  per turn (0–16, default 8).
- `yanshi.maxContextBytes` — shared UTF-8 byte budget across selection +
  open files (1024–262144, default 32768).

## Token storage

The Bearer token is stored in VS Code SecretStorage only (never in
`settings.json`, never in workspace files, never in event payloads or
telemetry). The extension currently reads the token on every turn; if
SecretStorage is unavailable (e.g. unlocked keychain missing on Linux), the
extension refuses to send authenticated requests and surfaces the failure in
the OutputChannel.

## Commands

- `Yanshi: Run Turn` — prompt for input and start a turn on the current
  thread (creating one if needed). The active editor's selection and
  bounded open files are attached as `context`.
- `Yanshi: Cancel Turn` — abort the active stream and send
  `thread/interrupt`. No-op if the turn already reached `turn.completed`
  or a prior cancel; the output channel marks `[cancelled]` only when a
  real cancellation is dispatched.
- `Yanshi: Show Last Diff` — open the most recent FileChange item as an
  in-memory diff. Never writes to workspace files. D1 does not yet emit
  fileChange items; this command is a no-op until D1 starts producing them.

## Output channel

Items are rendered as they arrive:

- `message.delta` -> appended text
- `reasoning.delta` -> `[thinking] <text>`
- `tool.call` / `tool.result` / `tool.progress` -> `[tool...]`
- `structured.result` -> `[result] <summary>`
- `turn.error` -> `[error] <error>`
- `turn.completed` -> `[done] <status>`
- unknown item type -> `[event] ignored <type> at sequence <n>`

## Recovery model

When the SSE stream disconnects mid-turn (network drop, server restart), the
extension raises a `[connection error]` line in the output channel. The
underlying SDK surfaces `StreamDisconnectedError` with the highest item
sequence observed; the IDE's `runWithRecovery` state machine
(`src/recovery.ts`) sleeps with exponential backoff, calls `thread/resume`
to refresh the in-memory item snapshot, and restarts the stream. Duplicate
item ids delivered during replay are de-duplicated via a `seen` Set so the
output channel does not render the same delta twice.

D1 does not yet implement cursor-based stream replay on the server side, so
reconnect today may not pick up items emitted between the disconnect and the
resume call; the state machine is the contract for when D1 adds replay
support.

## D1 handoff

See [`sdk/schema/CONTRACT_HANDOFF.md`](../../sdk/schema/CONTRACT_HANDOFF.md)
for the provisional-to-canonical replacement checklist. The IDE's wire
expectations are isolated in `src/config.ts`, `src/extension.ts`, and
`src/output.ts`; a D1 contract change touches three files at most.
