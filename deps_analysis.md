# deps_raw.txt — Complete Dependency & Structural Analysis

Generated: 2026-07-20
Tool:      cmd/pkganalyze (go list -json ./internal/...)
Total:     25 internal packages (including sub-packages), ~28K production LOC, ~23K test LOC.

---

## 1. STRUCTURE

### 1.1 Hierarchical Layout (by import path depth)

```
internal/                   (package root — 25 packages)
├── config/                 [1 file, 152 LOC]     Configuration loading + env var expansion
├── guard/                  [4 files, 400 LOC]    ** foundational ** safety/injection
├── llm/                    [2 files, 141 LOC]    Abstract LLM interface
├── lockfile/               [2 files, 205 LOC]    PID-based backend process discovery
├── plugin/                 [1 file, 50 LOC]      Go plugin host
├── proto/                  [1 file, 399 LOC]     Shared JSON frame vocabulary (WS + SSE)
├── version/                [1 file, 5 LOC]       Build-time version stamp
├── instruct/               [1 file, 160 LOC]     Instruction/prompt handling
├── skills/                 [2 files, 239 LOC]    Skill registry (SKILL.md loader)
├── store/                  [6 files, 848 LOC]    SQLite storage (session, KV, memory, task)
├── ctxcompact/             [11 files, 947 LOC]   Context compression engine
├── vcs/                    [2 files, 1086 LOC]   Auto-version-control (SQLite-based, git-like)
│   └── mcp/                [1 file, 456 LOC]     MCP protocol server (for external ACP agents)
├── llm/eino/               [9 files, 2642 LOC]   Eino model adapters, providers, fake, resilient
├── tools/                  [21 files, 4645 LOC]  ** largest package ** all agent tools
├── task/                   [1 file, 246 LOC]     Background task broker
├── agent/
│   ├── registry/           [2 files, 90 LOC]     Agent type registry
│   ├── orchestrator/       [4 files, 1464 LOC]   Eino ADK orchestrator (ReAct + sub-agent)
│   ├── goalloop/           [11 files, 1660 LOC]  Self-driven goal loop (plan→implement→eval→judge)
│   ├── worker/             [2 files, 404 LOC]    Worker mode (async execution)
│   └── spawn/              [1 file, 170 LOC]     Agent spawner
├── acp/                    [7 files, 1519 LOC]   External agent subprocess (codex/claudecode)
├── api/http/               [7 files, 2232 LOC]   HTTP server (WS primary, SSE secondary)
├── bootstrap/              [1 file, 368 LOC]     ** composition root ** wires everything
├── cli/                    [9 files, 1697 LOC]   CLI session resolver, backend adapters
│   └── tui/                [12 files, 5126 LOC]  Bubble Tea TUI (view, model, events, etc.)
```

### 1.2 Dependency Layers

**Layer 0 (Foundation — zero internal deps):**
`guard`, `instruct`, `proto`, `skills`, `store`, `lockfile`, `llm`, `ctxcompact`, `plugin`, `version`

All other packages import from this layer. These are the stable base that everything rests on.

**Layer 1 (Infrastructure — imports only Layer 0):**
`config` → `guard`
`llm/eino` → `config`, `ctxcompact`
`vcs` → `guard`, `store`
`task` → `store`, `vcs`
`vcs/mcp` → `vcs`

**Layer 2 (Agent Engine — imports Layer 0–1):**
`agent/registry` → `guard`
`agent/worker` → `guard`, `store`
`agent/orchestrator` → `guard`, `llm/eino`, `proto`, `tools`
`acp` → `guard`
`tools` → `guard`, `instruct`, `proto`, `skills`, `store`, `vcs`

**Layer 3 (Higher Agents):**
`agent/goalloop` → `acp`, `guard`, `vcs`

**Layer 4 (API & CLI):**
`api/http` → `agent/orchestrator`, `ctxcompact`, `guard`, `llm/eino`, `proto`, `skills`, `store`, `task`, `tools`
`cli` → `acp`, `bootstrap`, `config`, `lockfile`, `proto`, `store`
`cli/tui` → `cli`, `ctxcompact`, `guard`, `proto`, `version`

**Layer 5 (Composition Root — imports all layers):**
`bootstrap` → `agent/orchestrator`, `api/http`, `config`, `guard`, `instruct`, `llm/eino`, `skills`, `store`, `task`, `tools`, `vcs`

### 1.3 Package Responsibilities (expanded)

| Package | Core Responsibility | Key Export(s) |
|---|---|---|
| `guard` | Four-dimensional permission check (tools, fs, shell, net) + interactive mode | `Guard.Check()`, `PermissionProfile`, `Decision` |
| `store` | SQLite persistence: sessions (CRUD), KV, memory, task store | `Store`, `Session`, `KVStore` |
| `vcs` | SQLite-based auto-version control: tree-level tracking, branching, merging | `VCS`, `Repo`, `Worktree`, `Diff`, `Commit` |
| `vcs/mcp` | MCP server exposing VCS tools to external ACP agents | `Server` (stdio JSON-RPC) |
| `ctxcompact` | Context window compression: plan→preserve→summarize→assemble | `MaybeCompact()`, `Run()`, `Plan()`, `Summarize()` |
| `proto` | Shared JSON frame vocabulary for WS + SSE transport | `ClientFrame`, `ServerFrame`, `SSEEvent()` |
| `llm` | Abstract LLM interface + retry wrapper | `ChatModel`, `ResilientChatModel` |
| `llm/eino` | Eino adapter: providers (openai, anthropic), fake, compacting, resilient | `FakeModel`, `ResilientChatModel`, `CompactingModel` |
| `tools` | Agent tools: fs (read/write/edit/patch), shell, web, memory, VCS, sub-agent | `GuardedTool`, `DefineTools()`, `PredefinedAgents` |
| `instruct` | System prompt generation | `BuildSystemPrompt()` |
| `skills` | SKILL.md loading, plugin detection | `Registry`, `Load()`, `Plugins()` |
| `task` | Background task broker (async agent execution) | `Broker`, `Task`, `Execute()` |
| `agent/orchestrator` | Eino ADK ReAct loop: turn orchestration, sub-agent delegation | `Orchestrator`, `Runner`, `RunTurn()` |
| `agent/goalloop` | Self-driven goal loop: plan→implement→evaluate→judge | `Loop`, `Planner`, `Implementer`, `Judge` |
| `agent/registry` | Agent type registry | `Registry`, `Register()`, `Get()` |
| `agent/worker` | Worker mode for async agent execution | `Worker`, `Executor` |
| `agent/spawn` | Agent spawner | `Spawn()` |
| `acp` | External agent subprocess (codex/claudecode CLI) | `Client`, `FakeAgent`, `Spawn()`, `Policy` |
| `api/http` | HTTP server: WS (primary), SSE (secondary), session/task management | `Server`, `HandleWS()`, `HandleSSE()` |
| `bootstrap` | Composition root: wires config→store→vcs→model→tools→orchestrator→http→task | `Build()`, `App` |
| `cli` | Session resolver, backend discovery, WS/SSE backend adapters | `Session`, `Backend`, `WSBackend`, `SSEBackend` |
| `cli/tui` | Bubble Tea TUI: model, view, events, permissions, styles, startup | `Model`, `View`, `Update()`, `Program` |
| `config` | YAML configuration loading + `${VAR}` env expansion | `Load()`, `Config` |
| `lockfile` | Cross-platform PID-based lockfile for backend discovery | `Lockfile`, `Acquire()`, `Release()` |
| `plugin` | Go plugin host | `Host` |
| `version` | Build-time version stamp | `Version` |

---

## 2. DEPENDENCY ANALYSIS

### 2.1 Dependency Graph (internal only)

```
bootstrap ──────────────────────────────────────────────────────────────────────┐
  ├── agent/orchestrator ─── guard, llm/eino, proto, tools                     │
  │                          ├── guard                                         │
  │                          ├── llm/eino ─── config, ctxcompact               │
  │                          │                 └── ctxcompact ── (none)        │
  │                          ├── proto ──── (none)                             │
  │                          └── tools ──── guard, instruct, proto, skills,    │
  │                                        store, vcs                          │
  │                                          ├── vcs ─── guard, store          │
  │                                          └── store ── (none)               │
  ├── api/http ─── agent/orchestrator, ctxcompact, guard, llm/eino,           │
  │                proto, skills, store, task, tools                            │
  │                  └── task ─── store, vcs                                    │
  ├── config ─── guard                                                         │
  ├── instruct ── (none)                                                        │
  ├── skills ──── (none)                                                        │
  ├── vcs ─────── guard, store                                                  │
  └── (also: llm/eino, store, task, tools, guard)                              │
                                                                                │
cli ────────────────────────────────────────────────────────────────────────────┘
  ├── acp ──────── guard
  ├── bootstrap ── (everything above)
  ├── config ───── guard
  ├── lockfile ─── (none)
  ├── proto ────── (none)
  └── store ────── (none)
       │
       └── cli/tui ─── cli, ctxcompact, guard, proto, version

agent/goalloop ─── acp, guard, vcs
agent/worker ──── guard, store
agent/registry ── guard
```

### 2.2 Fan-Out (outgoing deps)

| Package | Fan-Out | Assessment |
|---|---|---|
| `bootstrap` | **11** | Expected — this IS the composition root, by design |
| `api/http` | **9** | **High** — couples transport to almost every domain |
| `cli` | 6 | Moderate — session management needs many services |
| `tools` | 6 | **High** — tool definitions reach across the entire domain |
| `cli/tui` | 5 | Moderate — UI needs guard, proto, compression |
| `agent/orchestrator` | 4 | Moderate |
| `agent/goalloop` | 3 | Low |
| `vcs` | 2 | Low |
| `task` | 2 | Low |
| `agent/worker` | 2 | Low |
| `llm/eino` | 2 | Low |
| `acp` | 1 | Minimal |
| `vcs/mcp` | 1 | Minimal |
| `config` | 1 | Minimal |
| 10 packages | 0 | Stable leaf packages |

### 2.3 Fan-In (incoming dependents)

| Package | Fan-In | Risk Level | Reason |
|---|---|---|---|
| **`guard`** | **11** | 🔴 **Critical** | EVERY agent path depends on guard. A bug here is catastrophic. |
| `store` | **7** | 🟠 **High** | Persistence layer — data corruption risk |
| `vcs` | **5** | 🟠 **High** | Version control — data integrity risk |
| `proto` | **5** | 🟠 **High** | Wire protocol — any change breaks client/server sync |
| `ctxcompact` | **3** | 🟡 Moderate | Compaction logic shared across WS + TUI |
| `tools` | **3** | 🟡 Moderate | Tool definitions reused by orchestrator + API + bootstrap |
| `llm/eino` | **3** | 🟡 Moderate | Model adapters used by orchestrator, API, bootstrap |
| `skills` | **3** | 🟢 Low | Skill registry |
| `config` | **3** | 🟢 Low | Configuration loading |
| `agent/orchestrator` | 2 | 🟢 Low | |
| `acp` | 2 | 🟢 Low | |
| `instruct` | 2 | 🟢 Low | |
| `task` | 2 | 🟢 Low | |
| `api/http` | 1 | 🟢 Low | |
| `version` | 1 | 🟢 Low | |
| `cli` | 1 | 🟢 Low | |
| `bootstrap` | 1 | 🟢 Low | |
| `lockfile` | 1 | 🟢 Low | |

### 2.4 Cycle Analysis

**No bidirectional dependency pairs detected.** The dependency graph is a clean DAG.

This is a significant architectural achievement — no import cycles despite 25 packages and 25K LOC. The layered design (foundation → infrastructure → agents → API → bootstrap) is strictly enforced by Go's compiler.

---

## 3. RISKS

### 3.1 Single Points of Failure 🔴

| Package | Risk | Mitigation Needed |
|---|---|---|
| **`guard`** | 11 dependents, 400 production LOC, test coverage=0.79. Any bug in glob matching or permission logic affects the entire system. | Fuzz tests for `MatchGlob`; property-based tests for permission matrices; audit log in all check paths |
| **`proto`** | 5 dependents, 399 LOC. Every WS and SSE message flows through here. Type mismatch between client/server can cause silent data loss. | Add wire-format compatibility tests; version field on frames; regression test suite for every frame type |
| **`store`** | 7 dependents, 848 LOC, test coverage=0.59 (lowest among critical packages). SQLite migrations and session lifecycle bugs can cause data loss. | Increase test coverage to >0.80; add migration tests; add corruption detection |

### 3.2 Package Size Hotspots 🔴

| Package | LOC (prod) | LOC (test) | Total | Assessment |
|---|---|---|---|---|
| **`cli/tui`** | 5126 | 4119 | 9245 | **Largest** — 12 files. High complexity from event handling + view rendering + permissions UI + diff display |
| **`tools`** | 4645 | 3315 | 7960 | 21 files. Spans fs (6 files), shell, web, memory, VCS, guard, sub-agent, skills, time, spillover. Too broad in scope. |
| **`llm/eino`** | 2642 | 2637 | 5279 | 9 files. Multiple providers + fake + resilient + compacting + output schema. |
| **`api/http`** | 2232 | 3055 | 5287 | 7 files + 15 test files. WS + SSE + sessions + tasks + compaction + skills prefix |
| **`cli`** | 1697 | 1555 | 3252 | 9 files. Backend adapters + session + exec + select + doctor |
| **`agent/goalloop`** | 1660 | 1836 | 3496 | 11 files. Loop + planner + implementer + evaluators + judge + tier + trigger + record + usage |
| **`acp`** | 1519 | 1443 | 2962 | 7 files. Client + transport + spawn + launch + policy + fake |

**File-level check**: no individual `.go` file exceeds 1000 code lines (pure code, no comments/blank lines). The largest is `tools/agent.go` at 942 lines. This satisfies the codebase convention.

### 3.3 Test Coverage Risks 🟡

| Package | Impl LOC | Test LOC | Ratio | Assessment |
|---|---|---|---|---|
| **`bootstrap`** | 368 | 86 | **0.23** | 🔴 Lowest. Composition root has no integration tests. Bugs in wiring silently fail. |
| `ctxcompact` | 947 | 365 | **0.39** | 🟡 Complex compression logic with multiple failure modes. Undertested. |
| `plugin` | 50 | 48 | 0.96 | ✅ |
| `store` | 848 | 497 | **0.59** | 🟡 Core persistence — data loss risk |
| `agent/registry` | 90 | 50 | 0.56 | 🟡 Lower priority (small package) |
| `lockfile` | 205 | 106 | 0.52 | 🟡 Process synchronization — race condition risk |
| `cli/tui` | 5126 | 4119 | 0.80 | ✅ Good coverage for largest package |
| `version` | 5 | 0 | **0.00** | 🟢 Negligible (2-line package) |
| `vcs/mcp` | 456 | 344 | 0.75 | ✅ |
| `api/http` | 2232 | 3055 | 1.37 | ✅ Excellent (test-heavy) |

### 3.4 External Dependency Risk

| Package | External Dependencies | Risk Level |
|---|---|---|
| `cli/tui` | bubbletea, lipgloss, glamour, fsnotify, termenv, bubbles | 🟡 — many UI deps, API stability risk |
| `tools` | eino (model, tool, schema), go-ordered-map, jsonschema | 🟡 — coupled to Eino tool framework |
| `api/http` | gorilla/websocket, eino (model, schema), jsonschema | 🟡 — WebSocket contract is versioned |
| `llm/eino` | eino (model, schema), eino-ext/openai | 🟡 — provider API drift risk |
| `store` | modernc.org/sqlite | 🟢 — stable CGo-free SQLite |
| `proto` | eino/schema | 🟢 — minimal surface |
| `guard` | stdlib only | ✅ — zero external deps |

### 3.5 Sink Packages (no dependents — potential dead code)

**All 25 packages** appear as sinks because the analysis scans only `./internal/...`. Packages like `cli/tui`, `acp`, `agent/goalloop` etc. are consumed by `cmd/` binaries or by external callers. No dead-code concern.

---

## 4. IMPROVEMENT OPPORTUNITIES

### 4.1 Architectural Improvements

#### 🔧 Split `tools/` into sub-packages (High Impact)

**Problem**: `tools` (21 files, 4645 prod LOC) spans 10+ tool categories and depends on 6 internal packages. It's the second-largest package and has the second-highest fan-out among non-bootstrap packages.

**Recommendation**: Organize by tool category:
```
tools/               — common types, GuardedTool wrapper, registration
tools/fs/            — fs_read, fs_write, fs_edit, fs_patch, fs_diff
tools/shell/         — shell_run
tools/vcs/           — vcs_commit, vcs_log, vcs_diff, vcs_restore, vcs_merge
tools/web/           — web_fetch
tools/memory/        — memory
tools/skill/         — skill_use
tools/subagent/      — agent delegation tools
tools/time/          — time_now
tools/spillover/     — spillover handling
```

**Benefit**: Reduces cognitive load per package, enables independent testing, clarifies ownership boundaries.

#### 🔧 Split `cli/tui/` into sub-packages (Medium Impact)

**Problem**: 5126 prod LOC in 12 files — largest package. Handles model, view, events, commands, permissions, diff display, styles, startup, debounce, queue, input.

**Recommendation**:
```
tui/                 — Model, Program, main entry
tui/view/            — rendering, styles, diff display
tui/state/           — model state, events, queue
tui/perm/            — permission dialogs
tui/cmd/             — commands, startup
```

#### 🔧 Extract `guard/` into a standalone module (Medium Impact)

**Problem**: `guard` is foundational (11 dependents), zero external deps, and operates independently of the yanshi codebase. It has well-defined interfaces (`Check(Action, Profile) Decision`).

**Recommendation**: Move to a separate Go module (`github.com/x6nux/yanshi-guard` or similar). This would:
- Force a stable API surface
- Enable independent versioning and CI
- Reduce the blast radius of guard changes
- Make it reusable for other projects

#### 🔧 Increase `bootstrap` test coverage (High Impact)

**Problem**: 0.23 test ratio for the composition root. Integration bugs are the hardest to diagnose.

**Recommendation**: Add integration tests that:
- Build a minimal App with fake model + in-memory store
- Verify all components wire without panic
- Test component lifecycle (start → graceful shutdown)
- Test error propagation (e.g., VCS init failure → app continues with VCS disabled)

### 4.2 Code Quality Improvements

#### 📋 `ctxcompact` — Add more edge-case tests

The compression engine has complex logic: Plan → Preserve → Summarize → Assemble, with fixpoint pairing and chunked summary. Test coverage at 0.39 leaves many edge cases uncovered:
- Empty message list
- Single message
- Messages with only tool_call/tool_result pairs
- Messages exceeding the context window
- Concurrent compaction requests
- Summary model failure → fallback behavior

#### 📋 `proto` — Add wire compatibility tests

With 5 dependents and shared between WS and SSE transports, frame changes must not break existing clients. Add a regression test that serializes every frame type and asserts the JSON wire format matches a golden file.

#### 📋 `lockfile` — Add race-condition tests

Lockfile operations are inherently racy (process crashes, concurrent acquires). Add tests that simulate process crashes and concurrent access.

### 4.3 Process Improvements

#### 📋 Introduce `go vet` + linting in CI

The CLAUDE.md mentions `go vet ./...` but the repo has no golangci-lint config. Adding consistent linting would catch:
- Misspelled exports
- Unused parameters
- Ineffective assignments
- Missing error checks

#### 📋 Formalize the file-size limit check

The 1000-line limit (pure code lines) is documented in CLAUDE.md but not enforced by CI. Add a CI step that checks this.

#### 📋 Add architecture governance test

Add a `govet`-style test that enforces the dependency layering:
- `bootstrap` may import any internal package
- `api/http` and `cli/*` may not import each other
- `guard` may not import any yanshi internal package (already satisfied)
- `tools` may not import `agent/orchestrator` (already satisfied)

This prevents regressions as the codebase grows.

### 4.4 Supply Chain Risk Mitigation

#### 📋 Pin Eino dependency versions more explicitly

Eino (cloudwego) is the most critical external dependency. The `go.mod` shows it's pulled as a direct dep. Consider:
- Using `go mod verify` in CI
- Running a weekly dependency update check
- Monitoring the Eino changelog for breaking changes

#### 📋 Reduce gorilla/websocket dependency surface

`gorilla/websocket` is used by both `api/http` (server) and `cli` (client). If the WebSocket protocol needs only a subset of features, consider wrapping it in an internal abstraction to make potential future migration easier.

---

## 5. SUMMARY

### What's Working Well ✅

| Aspect | Assessment |
|---|---|
| **Dependency graph** | Clean DAG with no cycles — excellent architecture |
| **Package cohesion** | Responsibilities are well-defined and mostly single-purpose |
| **Test culture** | Most packages have >0.70 test ratio; many exceed 1.0 (more test than prod code) |
| **File size discipline** | No file exceeds 1000 code lines — convention is respected |
| **Foundation stability** | `guard`, `store`, `proto` have zero internal deps — stable base |
| **Composition root** | `bootstrap` centralizes wiring — enables easy testing of individual components |
| **Cross-platform** | `lockfile/alive_*.go` and bubbletea fork handle Windows properly |
| **Fake-first testing** | `FakeModel`, `FakeAgent`, `FakePlanner`, `FakeBackend` — testability by design |

### What Needs Attention 🔴

| Issue | Severity | Effort |
|---|---|---|
| `bootstrap` test coverage (0.23) | High | Medium |
| `ctxcompact` test coverage (0.39) | Medium | Medium |
| `tools` package too broad (4645 LOC, 21 files) | Medium | Large |
| `cli/tui` package too large (5126 LOC) | Medium | Large |
| `guard` is single point of failure (11 dependents) | High | Small (add fuzz tests) |
| `store` test coverage (0.59) for critical persistence | Medium | Medium |
| No linting/architecture governance in CI | Medium | Small |

### Quick Wins 🏆

1. Add fuzz test for `guard.MatchGlob` — catches data-driven edge cases
2. Add `go vet` to CI pipeline
3. Add `bootstrap` integration test (build minimal App, verify wiring)
4. Add `proto` frame golden file test
5. Add file-size limit check to CI
6. Document the dependency layering in an `ARCHITECTURE.md` for new contributors

---

*End of analysis. Generated from `deps_raw.txt` (cmd/pkganalyze output) + supplementary code inspection.*
