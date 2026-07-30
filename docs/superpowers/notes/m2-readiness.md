# M2 Readiness Notes (carry-over from M1 final review)

M1 (Foundation & Guard) is complete (`m1-foundation` tag). The final holistic review
flagged the following as **M2's job** — none block M1, but M2 must address them.

## Must-fix in M2

1. **Extend the `llm` abstraction for streaming + tool calls.** M1's `llm.Client` is
   non-streaming (`Chat(ctx, []Message) (Response, error)`) with no function-calling
   surface; `Message` has no `ToolCalls`/`ToolCallID`; `Response` has no finish-reason/usage;
   `ResilientClient` has no streaming retry/failover path. M2 wires Eino + SSE + agent
   tool-use loops — extend these types/interface (or document where Eino types are used
   directly) before wiring. This was a deliberate M1 choice; just don't discover it late.

2. **Config env-var expansion.** `config.example.yaml` uses `${OPENAI_API_KEY}` but
   `config.Load` never calls `os.ExpandEnv` — the literal string would be sent as the key.
   Fix: expand env vars (on raw YAML text or per string value) in the loader, or drop the
   `${…}` form. M2 provider wiring hits this immediately.

3. **SQLite PRAGMAs + concurrency.** `Open` sets `SetMaxOpenConns(1)` (serializes reads
   too — fine for M1 tests, a bottleneck for concurrent M2 agents). Foreign keys are
   declared in the schema but NOT enforced (SQLite defaults `foreign_keys=OFF`). In M2:
   `PRAGMA foreign_keys=ON`, `journal_mode=WAL`, `busy_timeout`; relax MaxOpenConns with a
   small read pool.

4. **Schema: `UNIQUE(session_id, seq)` on `messages`.** Currently two messages with the
   same `(sid, seq)` both insert. Add a unique index or enforce monotonic seq in the store.

5. **Config validation.** Add a `Validate()`: providers non-empty, each
   `agent.profile` exists in `profiles`, token set, shell policy ∈ {allowlist,denylist,deny}.

## M2 design notes (sharp edges to honor)

- **`tools/guard` wrapper is load-bearing.** `guard.Check` only inspects dimensions the
  caller populates on `Action`. The M2 tool wrapper MUST faithfully translate each
  invocation (tool name + `FS`/`Shell`/`NetHost` as appropriate). If `FS.Paths` is empty,
  the FS check is skipped. The guard trusts what it's told.
- **Guard sharp edges:**
  - Trailing single `*` on a path pattern matches the whole subtree (`D:/code/*` ≈ `D:/code/**`).
    Use `**` for recursion. (Pinned by `TestMatchGlob_TrailingStarOnPathMatchesSubtree`.)
  - No symlink evaluation — a symlink inside an allowed dir pointing outside escapes the
    guard. `tools/guard` should `filepath.EvalSymlinks` before checking if that matters.
  - Matcher is case-sensitive; Windows/macOS FS is case-insensitive → safe direction
    (over-deny) but expect false denials if profile and paths differ in case.
- **Type bridge:** `store.Message` ↔ `llm.Message` conversion is M2's job (intentionally
  separate types in M1).
- **New store tables coming in M2:** `tasks` (Task Broker) + an FTS5 virtual table (memory).
- **Provider config:** `ProviderConfig.BaseURL` is already present (useful for Ollama/Ark
  custom endpoints).

## Environmental follow-up

- `go test -race ./...` could NOT be run in the M1 environment (no gcc/CGO on this Windows
  box). Run it once in a CGO-capable env before/when merging. No shared-mutable-state hazards
  were found by inspection (`fakeClient` uses atomics; store is single-conn; `ResilientClient`
  has no shared state).
