# autocode Phase 2 — Tools, Skills & Tiered Dev Workflows (Design Spec)

**Date:** 2026-07-15
**Status:** Approved (brainstormed 2026-07-15)
**Phase:** M7 (follows M1–M6, all merged to `main`, tag `m1-foundation`)
**Module:** `github.com/x6nux/autocode`

---

## 1. Goal

Make autocode a genuinely useful coding agent by adding three coherent capabilities:

1. **A real tool palette** — the filesystem/shell/time primitives every coding agent needs.
2. **Standard-compatible skill support** — load, discover, and invoke `SKILL.md` skill packs (agentskills.io spec), so external libraries like `superpowers` work unmodified and autocode's own know-how ships as skills.
3. **A built-in library of tiered development workflows** — five `SKILL.md` packs (T0–T4) that encode modern dev practice from quick-fix to autonomous project, with auto- and manual-tier selection.

The three are one layer: **skills orchestrate tools**, and the **workflows are skills**.

## 2. Background (what exists after M1–M6)

- **Orchestrator** (`internal/agent/orchestrator`): Eino ADK `ChatModelAgent` + `Runner`. `Config{Model, Tools []BaseTool, Instruction, MaxIters, Profile}`. `Query` / `Events` inject the profile via `tools.WithProfile`.
- **Tools** (`internal/tools`): `memory_search`/`recall`/`write`, `web_fetch`. All are `GuardedTool` (`tool.InvokableTool`) built via `NewGuardedTool(name, desc, params, run)`. Helpers: `params()` (JSON-schema builder, fixed for the `required:null` bug), `ParseArgs`, `toJSON`, `hostOnly`.
- **Guard** (`internal/guard`): `PermissionProfile{FS, Tools, Shell, Net}`. `Guard.Check(profile, Action{Tool, FS, Shell, NetHost})` short-circuits on first failing dimension. `FSWant{Op:"read"|"write", Paths}`. Shell metacharacters (`&& & || ; | `` $( \n \r > <`) always rejected. Glob matching supports `**`.
- **Critical guard pattern:** `GuardedTool.InvokableRun` checks only the *tools* allow-list (`Action{Tool: name}`). Each tool's own `run` re-invokes `guard.New().Check(...)` for its specific dimension (see `web_fetch` → `NetHost`). New tools must follow this.
- **Bootstrap** (`internal/bootstrap`): wires config→store→model→tools→orchestrator→HTTP server. Tools assembled here; profile resolved from `cfg.Profiles["orchestrator"]`.
- **Config** (`internal/config`): YAML via `yaml.v3`. `Config{Server, Storage, Token, LLM, Agents, Profiles map[string]guard.PermissionProfile}`.
- **Goal Loop** (`internal/agent/goalloop`): `Trigger.Decide` → `/goal ` prefix = explicit; else `Classifier` + `Confirmer`. `Loop.Run` = plan→implement(team)→evaluate(3 angles)→judge→loop/complete. `LLMPlanner`, `ACPImplementer`, `TestEvaluator`/`IntentEvaluator`/`QualityEvaluator`, `AggregateJudge`.
- **CLI** (`cmd/autocode`): `serve` / `chat` (REPL → POST `/api/v1/chat`, SSE) / `goal`.
- **ACP** (`internal/acp`): external coding agents (opencode/codex/claudecode) via JSON-RPC stdio.
- **Plugin host** (`internal/plugin`): go-plugin skeleton for connectors.

## 3. Scope

**In scope:**
- 8 new tools (`fs_read`, `fs_write`, `fs_edit`, `fs_list`, `fs_search`, `fs_glob`, `shell_run`, `time_now`).
- Skill system: loader, registry, progressive disclosure, plugin/marketplace discovery, `skill_use` tool, `/skill` command.
- 5 workflow skill packs (T0–T4) + tier classifier (rule + LLM).
- Bootstrap + orchestrator + CLI integration.

**Out of scope (deferred):**
- `web_search` (needs a search backend) — optional later.
- Remote marketplace fetching (HTTP plugin registries) — local plugin-dir scan only for now.
- A skill editor/authoring UI.
- Vector/RAG search over skills (FTS5 over skill bodies is a later nicety).

## 4. Architecture Overview

```
                    ┌───────────────────────────────────────────┐
                    │              Orchestrator                  │
                    │  Instruction = base + "Available skills"   │
                    │  Tools = [memory_*, web_fetch, fs_*,       │
                    │           shell_run, time_now, skill_use]  │
                    └───────────────┬───────────────────────────┘
                                    │ (all GuardedTool → PermissionGuard)
            ┌───────────────────────┼───────────────────────────────┐
            ▼                       ▼                               ▼
   internal/tools            internal/skills               internal/agent/goalloop
   (8 new + 4 existing)      Loader/Registry/              Trigger + Tierer
                             skill_use tool                /dev routing (T3/T4)
                                    │
                                    ▼
              skills/ (builtin T0–T4) + ~/.autocode/skills + plugins/*/skills
              (standard SKILL.md — loadable from external packs like superpowers)
```

Three subsystems, each independently testable, wired together in `bootstrap`.

---

## 5. Subsystem A — Tool Layer

All new tools live in `internal/tools/`, are `GuardedTool` instances, and follow the **self-check pattern**: the `run` func re-invokes `guard.New().Check(profile, Action{...})` with the tool's specific dimension set (mirroring `web_fetch`). Tool names use the existing underscore convention.

### 5.1 Filesystem tools (`internal/tools/fs.go`)

**`fs_read`** — read a file, optionally a line range.
- Params: `path` (string, required), `offset` (int), `limit` (int).
- Behavior: read file, return content with leading line numbers (`path:line` clickable form optional). Hard cap on bytes (default 256 KiB) to protect context.
- Guard: `Action{Tool:"fs_read", FS:{Op:"read", Paths:[abs(path)]}}`.

**`fs_write`** — create or overwrite a file.
- Params: `path` (string, required), `content` (string, required).
- Behavior: write bytes (create parent dirs). Refuses to overwrite a path not read first? — No (keep simple); the guard's write-list is the control. Returns `{"wrote": path, "bytes": n}`.
- Guard: `Action{Tool:"fs_write", FS:{Op:"write", Paths:[abs(path)]}}`.

**`fs_edit`** — exact string replacement (the `Edit` primitive).
- Params: `path` (string, required), `old_string` (string, required), `new_string` (string, required), `replace_all` (bool).
- Behavior: read file, replace `old_string` (unique unless `replace_all`), write back. Fail if `old_string` not found or non-unique without `replace_all`. Returns `{"edited": path, "replacements": n}`.
- Guard: `Action{Tool:"fs_edit", FS:{Op:"write", Paths:[abs(path)]}}`.

**`fs_list`** — list a directory.
- Params: `path` (string, required), `pattern` (string, glob filter).
- Behavior: return sorted entries (name + size + is_dir). Cap entry count (default 2000).
- Guard: `Action{Tool:"fs_list", FS:{Op:"read", Paths:[abs(path)]}}`.

**`fs_search`** — content search (the `Grep` primitive).
- Params: `pattern` (string, required, Go regexp), `path` (string, default "."), `glob` (string, file-name filter), `output_mode` (enum: `files_with_matches` default | `content` | `count`), `context` (int, lines of context for `content`), `head_limit` (int, default 250).
- Behavior: pure-Go recursive walk under `path`; skip binaries (NUL-byte heuristic) and dirs matching a default ignore set (`node_modules`, `.git`, `vendor`); match each file's content with `regexp`. No shell-out, no `ripgrep` dependency. v1 ignores `.gitignore` (tracked as a follow-up).
- Guard: `Action{Tool:"fs_search", FS:{Op:"read", Paths:[abs(path)]}}` — the whole subtree under `path` must be readable. (Read-list patterns with `**` cover this.)

**`fs_glob`** — file-name pattern match (the `Glob` primitive).
- Params: `pattern` (string, required, supports `**`), `path` (string, default ".").
- Behavior: walk and match file paths against `pattern` using the existing `internal/guard/glob.go` matcher. Return sorted matches.
- Guard: `Action{Tool:"fs_glob", FS:{Op:"read", Paths:[abs(path)]}}`.

**Shared helper:** `absPath(path string) (string, error)` — resolve to absolute (relative to a configurable work-root, default CWD). All fs tools abs-resolve before guard + I/O.

### 5.2 Shell tool (`internal/tools/shell.go`)

**`shell_run`** — execute a shell command.
- Params: `command` (string, required), `workdir` (string, default work-root), `timeout` (int seconds, default 120).
- Behavior: `exec.Command` with `sh -c` (Unix) / `cmd /c` (Windows). Capture combined stdout+stderr, exit code. Enforce timeout (kill + return partial). **The guard's metachar deny applies** — so `&&`, `|`, `>`, `<`, etc. are rejected before exec. The agent issues sequential single commands (e.g. `go test ./...` then `git commit` separately). Returns `{"stdout":..., "stderr":..., "exit":..., "duration_ms":...}`.
- Guard: `Action{Tool:"shell_run", Shell: command}`. Reuses the existing `checkShell` (allowlist + metachar deny). Also FS-checks the `workdir` as a read.
- **Security note:** metachar restriction is deliberate and matches the M1–M6 guard model. Skills document this (issue one command per call). Loosening it is explicitly out of scope.

### 5.3 Time tool (`internal/tools/time.go`)

**`time_now`** — current time.
- Params: none.
- Behavior: return `{"iso8601":..., "unix":..., "offset_seconds":...}`. No guard dimension (read-only, no profile action needed beyond the tools allow-list).

### 5.4 Tool assembly

`bootstrap.Build` constructs `FSTools{Read,Write,Edit,List,Search,Glob}`, `ShellTools{Run}`, `TimeTools{Now}` (each a small struct holding a work-root, like `WebTools`/`MemoryTools`) and appends their `*GuardedTool` fields to `allTools` alongside the existing memory/web tools.

---

## 6. Subsystem B — Skill System (`internal/skills/`)

Standard-compatible with [agentskills.io](https://agentskills.io/specification). Ground-truth reference: the `superpowers` plugin at `~/.claude/plugins/.../superpowers/5.0.7`.

### 6.1 On-disk format

```
skill-name/                  # directory name = skill identity (hyphens)
  SKILL.md                   # REQUIRED: YAML frontmatter + markdown body
  reference.md               # optional, loaded on-demand
  scripts/helper.py          # optional, EXECUTED (via shell_run), not loaded
```

**`SKILL.md` frontmatter (only hard requirement):**
```yaml
---
name: skill-name-with-hyphens      # ≤64 chars, [a-zA-Z0-9-]
description: Use when [triggers]   # ≤1024 chars, third person
---
```
Body: markdown instructions (<500 lines ideal). Additional frontmatter fields (version, author, keywords) are tolerated and ignored in v1.

### 6.2 Types

```go
package skills

type Skill struct {
    Name        string // frontmatter
    Description string // frontmatter
    Dir         string // absolute path to the skill directory
    Source      string // "builtin" | "user" | "plugin:<name>"
}

type Registry struct {
    skills map[string]*Skill // keyed by Name; first-seen wins (priority order)
}

type Loader struct {
    dirs []string // search roots, priority order (builtin first)
}

func NewLoader(dirs ...string) *Loader
func (l *Loader) Load() (*Registry, error)                 // scan, parse frontmatter, validate
func (r *Registry) Get(name string) (*Skill, bool)
func (r *Registry) List() []*Skill
func (r *Registry) MetaPrompt() string                    // "Available skills:\n- name: desc ..." for system prompt
func (r *Registry) Body(s *Skill) (string, error)         // lazy: read SKILL.md, strip frontmatter
func (r *Registry) ReadFile(s *Skill, rel string) (string, error) // on-demand reference file (sanitized relpath)
```

**Frontmatter parse:** read `SKILL.md`, split on `---` fences, `yaml.v3` unmarshal block into `struct{Name,Description string}`. Validate name regex + lengths. Skip dirs without `SKILL.md` or with invalid frontmatter (log, don't fail the whole load — one bad skill must not break the registry).

**Progressive disclosure (the key mechanic):**
1. `Load()` reads **only frontmatter** for every skill → cheap registry.
2. `Body()` reads the markdown body **only when the skill is invoked**.
3. `ReadFile()` reads a reference file **only when the skill tells the agent to**.
Large reference content costs zero tokens until accessed.

### 6.3 Discovery sources (priority order)

1. **Builtin:** `<repo>/skills/` (the T0–T4 packs + any shipped know-how).
2. **User:** `~/.autocode/skills/`.
3. **Plugins:** each dir under `cfg.Skills.PluginDir` (default `~/.autocode/plugins/`) that contains a `.autocode-plugin/plugin.json`; its skills live at `<plugin>/skills/*/SKILL.md`.

**Plugin manifest** (`.autocode-plugin/plugin.json`):
```go
type PluginManifest struct {
    Name        string `json:"name"`
    Description string `json:"description"`
    Version     string `json:"version"`
}
```
`discoverPlugins(root)` scans `root/*/​.autocode-plugin/plugin.json`. A `marketplace.json` at the root listing plugins is read but v1 treats it as documentation (no remote fetch). Source tag for plugin skills = `"plugin:<PluginManifest.Name>"`.

First-seen-wins on name conflict (builtin > user > plugin), so autocode's own skills can't be shadowed.

### 6.4 Invocation

**`skill_use` tool** (`internal/tools/skill.go`) — a `GuardedTool`:
- Params: `name` (string, required).
- `run`: look up the skill in the registry (passed in via closure at construction: `NewSkillUseTool(reg *skills.Registry)`), return `Body(skill)` as the tool result. The model then follows those instructions. Allowed when `skill_use` (or `*`) is in the profile's tools allow-list.
- This is the Eino-clean mapping: skill metadata sits in the system prompt; the model calls `skill_use("brainstorming")` → body returns as a tool result → model proceeds. **No orchestrator rebuild.**

**`/skill <name>` CLI command** — explicit user invocation in the `chat` REPL and HTTP chat layer:
- The chat layer recognizes the `/skill ` prefix **before** sending to the model, fetches `Registry.Body`, and injects it as a system-role message for that turn (then the user's actual follow-up is the next turn). For the HTTP path this is handled in `internal/api/http/chat.go` so all clients (CLI, future web/IM) behave identically.
- Unknown skill → error message listing available skills.

**Namespace reconciliation:** tools use underscores (`memory_search`), skills use hyphens (`brainstorming`). Different registries, no collision. Slash commands: `/skill <name>` (skill), `/dev [tier] <task>` (workflow).

### 6.5 Script execution

A skill may reference `scripts/foo.py` to be **executed**, not read. The skill body tells the agent to call `shell_run` with the script path (e.g. `shell_run` with `command: "python scripts/foo.py"`). Because `shell_run` is guard-gated, script execution inherits the acting agent's shell permissions. No special sandbox beyond the existing guard. Documented in the skill-authoring guidance.

---

## 7. Subsystem C — Tiered Dev Workflow Skills

Five `SKILL.md` packs under `<repo>/skills/dev/`, difficulty-ordered, each a standard skill invokable via `skill_use` or `/skill`. Higher tiers compose lower ones.

| Tier | Skill name | Trigger (when to use) | Workflow summary | Agent mechanism |
|------|-----------|----------------------|------------------|-----------------|
| **T0** | `dev-quick-fix` | Single-file, obvious change (typo, config tweak, small bug) | locate (`fs_search`) → edit (`fs_edit`) → verify (`shell_run` tests) → commit | Single orchestrator turn; no design |
| **T1** | `dev-standard-feature` | One focused feature/bugfix, TDD, 1–3 files | reproduce → write failing test → implement → green → refactor → commit | Single agent, TDD discipline |
| **T2** | `dev-designed-feature` | Feature needing design first (multi-file, API surface, unclear scope) | brainstorm → spec → plan → TDD implement → review → merge | Introduces `docs/` spec+plan structure; single agent or lead+1 |
| **T3** | `dev-team-feature` | Multi-component, parallelizable, needs a team | spec → plan → Lead decomposes → Workers parallel (worktree-isolated) → Integrator merges → evaluate | Goal Loop implementer team (M6) |
| **T4** | `dev-autonomous-project` | Open-ended goal, self-driven, multi-iteration | plan → implement(team) → evaluate (3 angles) → judge → loop/complete | Full Goal Loop (M6) |

Each pack's body specifies: entry signals, step-by-step procedure (checklists), which tools to use, when to escalate to the next tier, and exit criteria. T2 references the brainstorming→writing-plans flow; T3/T4 reference the Goal Loop.

### 7.1 Tier selection (`internal/agent/goalloop/tier.go`)

Extends the existing dual-entry model ("命令触发 也允许自动判断"):

```go
type Tier int
const ( TierQuickFix Tier = iota; TierStandard; TierDesigned; TierTeam; TierAutonomous )

type Tierer interface { Tier(ctx context.Context, task string) (Tier, error) }

type RuleTierer struct{}    // heuristic on keywords/signal words
type LLMTierer struct{ Model model.BaseChatModel; Prompt string } // structured-output classification
```

- **RuleTierer** signals: "typo/config/tweak" → T0; "bug/test/fix" single-file → T1; "design/refactor/multi-file" → T2; "component/parallel/team" → T3; "project/epic/autonomous/build a system" → T4.
- **LLMTierer** asks the model to classify (returns tier + one-line reason); **RuleTierer is the fallback** if the model call fails.
- **Manual override:** `/dev t2 implement login throttling` forces tier 2. `/dev implement login throttling` auto-selects.
- **Routing:** T0–T2 run as a single orchestrator turn with the selected skill loaded (via `skill_use` internally) — lightweight, no full Goal Loop. T3–T4 enter the existing Goal Loop with team config. This keeps simple work cheap.

### 7.2 Integration with the Goal Loop trigger

`Trigger.Decide` gains a tier outcome: `/dev ` prefix (explicit tier or auto) returns `(enter=true, tier, reason)`; a goal-impl intent returns `(enter=true, auto-tier, "auto+confirm")`; otherwise `(false, _, "not a goal")`. The CLI `goal`/`chat` paths consume the tier to decide lightweight-vs-loop.

---

## 8. Integration Points

### 8.1 Bootstrap (`internal/bootstrap/bootstrap.go`)
After building memory/web tools, build fs/shell/time tools and the skill registry:
```go
fsTools := tools.NewFSTools(workRoot)
shellTools := tools.NewShellTools(workRoot)
timeTools := tools.NewTimeTools()
allTools = append(allTools, fsTools.Tools()...)
allTools = append(allTools, shellTools.Run, timeTools.Now)

reg, err := skills.NewLoader(cfg.Skills.BuiltinDir, cfg.Skills.UserDir, ...pluginDirs...).Load()
skillUse := tools.NewSkillUseTool(reg)
allTools = append(allTools, skillUse)
```
Pass `reg` to the orchestrator so its `Instruction` includes `reg.MetaPrompt()`. Wire `reg` + `Tierer` into the goal-loop trigger path.

### 8.2 Orchestrator (`internal/agent/orchestrator`)
`New` prepends `cfg.SkillMetaPrompt` (if set) to `Instruction`, e.g.:
```
You are autocode's orchestrator. Use tools when helpful.

Available skills (call skill_use to load one):
- dev-quick-fix: Use when ...
- brainstorming: Use when ...
```
No other orchestrator change — `skill_use` is just another `GuardedTool`.

### 8.3 CLI / HTTP chat
- `chat` REPL: intercept `/skill <name>` and `/dev [tier] <task>` locally for `/skill` preview, but route both through the server so the orchestrator executes them. `/dev` invokes the goal-loop path (existing `goal` wiring, extended with tier).
- `goal` subcommand: add `-tier` flag (0–4 or `auto`).

### 8.4 Config (`internal/config/config.go`)
```go
type SkillsConfig struct {
    BuiltinDir string `yaml:"builtin_dir"` // default: "<exe_dir>/skills" or "./skills"
    UserDir    string `yaml:"user_dir"`     // default: "~/.autocode/skills"
    PluginDir  string `yaml:"plugin_dir"`   // default: "~/.autocode/plugins"
}
// add to Config: Skills SkillsConfig `yaml:"skills"`
```

---

## 9. Security Model

- **Every new tool is a `GuardedTool`.** fs tools self-check `FS` (read/write lists); `shell_run` self-checks `Shell` (allowlist + metachar deny); `time_now` needs only the tools allow-list.
- **Skill bodies are trusted instructions** loaded into the agent's context. Skills come from builtin (shipped, trusted), user (`~/.autocode`, owner-controlled), or plugin dirs (operator-installed). v1 treats installed skills as trusted (same trust level as config). A future hardening could sign plugin manifests; out of scope here.
- **Script execution** in skills goes through `shell_run` → inherits the agent's shell profile. No bypass.
- **`fs_search`/`fs_glob`** respect the read-list: the search root must match a read pattern; individual file reads during search are covered by the root's read permission (no per-file re-check needed because the root glob with `**` already authorizes the subtree — documented as a deliberate trade-off).
- **Path traversal:** all fs paths are abs-resolved and cleaned before guard matching; `..` is normalized by `filepath.Clean`. Symlinks are followed by the OS; v1 does not add symlink-specific restrictions (documented).

---

## 10. Data Model Summary

- `skills.Skill`, `skills.Registry`, `skills.Loader`, `skills.PluginManifest` (new package).
- `tools.FSTools`/`ShellTools`/`TimeTools` structs + `NewSkillUseTool` (new in `internal/tools`).
- `goalloop.Tier`, `goalloop.Tierer`, `goalloop.RuleTierer`, `goalloop.LLMTierer` (new in `goalloop`).
- `config.SkillsConfig` (new).
- On-disk: `skills/dev/{dev-quick-fix,dev-standard-feature,dev-designed-feature,dev-team-feature,dev-autonomous-project}/SKILL.md` (+ optional supporting files).

---

## 11. Testing Strategy

Each subsystem tested in isolation, then E2E:

- **fs tools:** temp-dir table tests for read/write/edit (incl. non-unique old_string, missing file), list, glob, search (regex, binary skip, ignore set); guard-deny tests (path outside read/write list → `DenyErr`); empty-profile fail-closed.
- **shell tool:** run a real command (`go version`), capture exit code; timeout enforcement; metachar rejection (`&&`, `|`) → `DenyErr`; allowlist deny.
- **time tool:** format/fields sanity.
- **skills loader:** build a temp skill tree (valid, missing-SKILL, bad-frontmatter, name-collision) → assert registry contents, first-seen-wins, frontmatter validation, lazy `Body`, `ReadFile` path-sanitization (reject `..`).
- **plugin discovery:** temp plugin dir with `.autocode-plugin/plugin.json` + `skills/` → discovered; malformed plugin skipped.
- **`skill_use` tool:** returns body of a known skill; unknown skill → error.
- **tierer:** `RuleTierer` table for signal words; `LLMTierer` with a fake model returning a fixed tier; fallback path when model errors.
- **orchestrator integration:** instruction contains skill metadata; `skill_use` callable end-to-end with a fake model that emits a `skill_use` tool call.
- **bootstrap:** boots with skill dirs; registry non-empty when builtin skills present.
- **E2E:** fake-model orchestrator resolves `/dev t0 fix typo` → loads `dev-quick-fix` → calls `fs_edit` on a temp file (profile permitting) → reports done.

All tests pure-Go, no network, no CGO (consistent with M1–M6).

---

## 12. Phasing / Milestones

One phase (M7), broken into five task groups for the implementation plan:

- **M7-T1 Tool layer:** `fs.go`, `shell.go`, `time.go` + tests + bootstrap assembly + profile config examples.
- **M7-T2 Skill core:** `internal/skills/` (loader, registry, frontmatter parse, plugin discovery, lazy body/readfile) + tests.
- **M7-T3 Skill invocation + orchestrator:** `skill_use` tool, `MetaPrompt` injection in orchestrator, `/skill` chat-layer handling, config `SkillsConfig`.
- **M7-T4 Workflow library + tierer:** five `skills/dev/*/SKILL.md` packs, `Tierer` (rule + LLM), `/dev` routing into goal loop, `-tier` flag.
- **M7-T5 E2E + docs:** integration tests, README/skill-authoring notes, sample permission profiles for a coding agent.

Writing-plans will expand each group into TDD tasks.

---

## 13. Risks & Open Questions

- **R1 — `fs_search` scope of read-permission:** searching a subtree authorizes reading every file under it (via the root glob). Acceptable and documented; the alternative (per-file re-check) is too slow and the operator controls read-lists anyway.
- **R2 — shell metachar strictness vs. coding ergonomics:** `&&`/`|`/`>` are denied, so pipelines and redirection require multiple tool calls. This is the existing guard contract; skills will coach the pattern. Loosening is a separate, security-reviewed decision.
- **R3 — LLM tierer accuracy:** a weak model may mis-tier. Mitigated by RuleTierer fallback + manual `/dev tN` override + skills coaching escalation ("if scope grows beyond your tier, stop and re-tier").
- **R4 — Skill trust:** installed skills run as instructions + may drive `shell_run`. v1 trusts operator-installed skills. Signing/sandboxing is future work.
- **OQ1 — Should `/skill` persist for the whole session or one turn?** Default: one turn (loaded as a system message for that turn); a `--sticky` variant can be added later. (Recommend one-turn for v1; revisit if workflows need session-long skill context.)
- **OQ2 — `.gitignore` awareness in `fs_search`:** deferred; v1 uses a static ignore set (`node_modules`, `.git`, `vendor`). Tracked as a follow-up.

---

## 14. Acceptance Criteria

The phase is done when:
1. All 8 new tools exist, are guard-wrapped, and pass their test suites.
2. `internal/skills` loads builtin + user + plugin skill dirs per the agentskills.io format; `superpowers` (or an equivalent external pack) dropped into the plugin dir is discoverable and `skill_use`-able.
3. `/skill <name>` and `skill_use` both load a skill's body into the agent context.
4. The five T0–T4 `SKILL.md` packs exist and are invokable.
5. `/dev [tier] <task>` and auto-detection route to the right tier; T3/T4 drive the existing Goal Loop.
6. `go build ./...`, `go vet ./...`, `go test ./...` all pass; no CGO; no network in tests.
7. An E2E fake-model run completes a T0 quick-fix on a temp file end-to-end.
