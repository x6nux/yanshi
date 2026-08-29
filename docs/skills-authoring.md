# Writing Yanshi skills

A **skill** is a directory containing a `SKILL.md` file whose YAML frontmatter
describes the skill and whose markdown body contains the instructions the agent
follows. Skills follow the [agentskills.io](https://agentskills.io/) format and
are loaded with progressive disclosure: only the frontmatter is read at startup,
and the body is read only when the skill is actually invoked.

This guide covers the format, where to put skills, how invocation works, and the
security model. For reference, the five builtin dev-workflow skills live under
`skills/` at the repo root.

## The SKILL.md format

```
<skill-dir>/SKILL.md
<skill-dir>/reference.md   # optional, read on demand
<skill-dir>/scripts/...    # optional, executed via shell_run
```

A SKILL.md has two parts:

1. **YAML frontmatter** between `---` delimiters, with two required fields.
2. **Markdown body** after the second `---` — the instructions the agent receives when the skill is invoked.

### Frontmatter rules

| Field | Rules |
| --- | --- |
| `name` | Required. Matches `^[a-zA-Z0-9-]+$`, 1–64 characters. |
| `description` | Required. Up to 1024 characters. Third person. Describe **when** to use the skill, not what it does internally — the model uses this line to decide whether to invoke the skill, so "Use when ..." phrasing works best. |

The loader validates both fields. A directory whose SKILL.md has a bad name or
description (or is missing) is **skipped** — one bad skill never fails the load.
When two roots contain a skill with the same name, **first-seen-wins** (builtin
before user before plugin).

### Body guidelines

- Keep the body under ~500 lines. The whole body enters the model's context on
  invocation, so concise beats comprehensive.
- Lead with a one-line summary of when to use the skill, then a numbered
  workflow, then escalation rules.
- Reference large or rarely needed material from separate files (see below)
  rather than inlining it.

## Progressive disclosure

Loading is staged so a large library of skills costs almost nothing at startup:

1. **Startup** — only frontmatter (`name` + `description`) is parsed. This populates the registry and the "Available skills" block injected into the orchestrator's system prompt.
2. **Invocation** — `Registry.Body(skill)` reads and returns the markdown body (frontmatter stripped), lazily.
3. **On-demand references** — `Registry.ReadFile(skill, relpath)` reads an additional file from the skill directory when the body instructs the agent to consult it. Paths must be relative to the skill dir; absolute paths and traversal (`..`) are rejected.
4. **Scripts** — a skill may instruct the agent to run scripts via the `shell_run` tool. Scripts are **executed**, not loaded into context, so they do not consume tokens and are not trusted as instructions.

## Where to put skills

Three roots are scanned at boot (configurable in `config.yaml` under `skills:`):

| Root | Location | Source tag | Notes |
| --- | --- | --- | --- |
| Builtin | `skills/<name>/SKILL.md` | `builtin` | Shipped with Yanshi. Set via `skills.builtin_dir` (default `skills`). |
| User | `~/.yanshi/skills/<name>/SKILL.md` | `user` | Per-user skills. Set via `skills.user_dir`. |
| Plugin | `~/.yanshi/plugins/<plugin>/skills/<name>/SKILL.md` | `plugin:<plugin>` | Discovered when `<plugin>/.yanshi-plugin/plugin.json` exists. Set via `skills.plugin_dir`. |

A plugin directory is only treated as a skill root if it contains a
`.yanshi-plugin/plugin.json` manifest (with at least a `name` field) **and** a
`skills/` subdirectory. Plugin discovery is non-fatal: a corrupt plugin root
logs a warning to stderr and the rest of the skills still load.

### Name collisions across roots

Roots are scanned in order and the **first** one to provide a given name wins.
A skill that loses is not silently discarded: `/skills` lists it under the
winner as

```
  - review (project) [enabled, trusted]
      shadowed: user copy at /home/u/.yanshi/skills/review is ignored
```

The ignored **directory** is printed, not just the source label, because
"which file is being ignored" is the question you are actually asking — and a
label does not answer it when several roots share one. Resolution order itself
is unchanged by this reporting; renaming one of the two copies is the fix.

### Re-validating an installed skill

`/skill validate [name]` re-runs the checks that gate an install — frontmatter
parses, `name` is 1–64 characters of letters/digits/dashes, `description` is
1–1024 characters, no symlinks anywhere in the directory, and the directory
name matches the frontmatter name. With no argument it validates every
installed skill.

This exists because those rules used to live only inside `Install`, so a skill
edited by hand after installation — the normal way people iterate on one — could
not be checked by anything. The symlink ban is re-checked rather than assumed:
a directory can acquire one after it is in place, and a symlink can read outside
the skill directory or smuggle in a `.trusted` marker.

There is deliberately **no `/skill update`**. `uninstall` followed by `install`
already composes, shares one validation path, and cannot leave a half-updated
directory behind; a dedicated verb would buy one less command and add that
failure mode.

## How invocation works

Two paths, both ending with the skill body entering the turn as guidance:

- **Model-driven** — the orchestrator's system prompt includes an "Available
  skills" block listing each skill's name and description. The model calls the
  `skill_use` tool with `{ "name": "<skill>" }`; the tool looks the skill up in
  the registry, reads its body, and returns it for the model to follow.
- **User-forced** — in the chat REPL, prefix a message with
  `/skill <name> [task]`. The handler loads the body, then either appends the
  task (`body\n\n---\nTask: <task>`) or, with no task, asks the model to
  acknowledge the skill is loaded and await instructions. An unknown skill
  returns an SSE error listing the available skill names.

## Security notes

- **Skill bodies are trusted instructions.** They are injected verbatim into the
  turn, so only install skills from sources you trust (builtin, your own user
  dir, and vetted plugins).
- **Scripts run through `shell_run`.** That means they inherit the acting
  agent's shell profile **and** the guard's shell policy. Chaining with `&&`,
  `||`, `;` or `|` is allowed, but each segment is checked separately against
  that policy and the strictest verdict decides the whole command — so a chain
  is only as runnable as its least-allowed part. `shell_run` still rejects
  outright the forms whose real payload is not in the command string: command
  substitution (`$()`, backticks), process substitution, subshells, here-
  documents, background `&`, and newlines. Prefer a script file over an inline
  one-liner when you need any of those.
- **Reference files are sandboxed** to the skill directory. `Registry.ReadFile`
  rejects absolute paths and any relative path that escapes the skill dir.
- **Filesystem and network actions** taken by the agent while following a skill
  are still checked against the agent's permission profile (`fs.read`/`fs.write`
  globs, `net.hosts`, etc.) — a skill cannot escalate past the profile.

## A minimal example

```markdown
---
name: greeter
description: Use when the user asks to greet someone or produce a welcome message
---

# Greeter

Produce a short, friendly greeting.

## Workflow
1. Ask for the recipient's name if not given (or infer from the task).
2. Emit a one-line greeting using that name.
3. Offer to customize tone (formal/casual) if asked.

## Escalate
If the request is actually a broader onboarding flow, hand back to the caller.
```

Drop this at `~/.yanshi/skills/greeter/SKILL.md`, restart the server, and it
appears in "Available skills". Invoke with `/skill greeter say hi to Sam`.

## Reference: the builtin dev packs

The five skills under `skills/` are the best reference for real-world authoring,
and double as the development-workflow tiers routed by `/dev`:

| Tier | Skill | Body shape |
| --- | --- | --- |
| T0 | `dev-quick-fix` | locate → confirm → edit → verify (one command per shell_run) → commit; escalate if >1 file. |
| T1 | `dev-standard-feature` | strict TDD: read → write failing test → run red → implement → run green → refactor → commit. |
| T2 | `dev-designed-feature` | brainstorm → write spec → write plan → implement task-by-task → review → merge. |
| T3 | `dev-team-feature` | spec + plan → Lead decomposes → Workers parallel in worktrees → Integrator merges → evaluate. |
| T4 | `dev-autonomous-project` | full goal loop: plan → implement → evaluate (tests + intent + quality) → judge → loop/complete. |

Read any of them directly — they are intentionally short. The `/dev tN <task>`
chat command and the `goal -tier tN` flag force a tier; `/dev <task>` lets the
`RuleTierer` pick one from the task text.
