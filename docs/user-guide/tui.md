# TUI 指南

yanshi 的默认入口是一个 Bubble Tea 全屏 TUI（alt-screen）。它是一个**本地轻客户端**：通过 lockfile + healthz 发现当前项目正在运行的后端，找不到就在进程内嵌入一个。多窗口自愈：当后端 owner 退出时，断开的客户端重新发现，第一个发现无存活后端的客户端引导一个新的。

## 启动

```sh
./yanshi --fake-model -inprocess   # 进程内后端 + fake model
./yanshi                           # 自动发现或嵌入后端
```

> alt-screen TUI 无法通过管道驱动。无 TTY 的环境用 [entrypoints.md](entrypoints.md) 里的 headless 路径。

## 键位

- **Enter**：发送一个 turn。
- **Ctrl+Enter**：在输入框换行。

> 区分 Enter 与 Ctrl+Enter 依赖本地 bubbletea fork（上游在 Windows 上把两者都收敛为 `KeyEnter`）；详见 [../../CONTRIBUTING.md](../../CONTRIBUTING.md)。**键位方案目前不可切换**：TUI 运行时的键位是写死的 default（见 `internal/cli/tui/model.go::newModel`），`config.yaml` 的 `tui.keymap` 目前只被 `yanshi doctor` 读来做校验，不影响运行时。

## `/` 前缀命令

在输入框输入 `/` 调出命令。常用的有：

- `/model`：切换当前会话的 provider/model（按 turn 切换，无需重建编排器）。
- `/skill`：调用一个技能。
- `/theme`：列出 / 切换配色主题。高对比度用 `/theme high-contrast`；不带参数会弹出选择器。**只影响当前会话，不落盘。**

> **键位方案、Vim 模式、高对比与界面语言都有运行时开关**（W8 接通）：
>
> | 命令 | 作用 |
> |---|---|
> | `/keymap` | 查看当前方案；`/keymap reset` 恢复内置默认并写下 tombstone（项目 `tui.bindings` 从此不再覆盖）；`/keymap diagnostics` 打印键位诊断 |
> | `/vim on\|off` | 开关 vim 模式编辑 |
> | `/contrast on\|off` | 开关高对比主题 |
> | `/locale auto\|en\|zh-Hans` | 切换界面语言 |
>
> 四个命令都**落盘**到 `os.UserConfigDir()/yanshi/prefs.json`（用户层），下次启动仍然生效。
> 优先级自低而高：内置默认 < `config.yaml` 的 `tui.*` / `i18n.ui_locale` < `prefs.json` < 环境变量
> （`YANSHI_THEME` / `YANSHI_KEYMAP` / `YANSHI_HIGH_CONTRAST` / `YANSHI_VIM` / `YANSHI_UI_LOCALE`）。
> 写盘只写用户层：来自项目配置的值不会被复制进 `prefs.json`，否则项目改了它也改不动。
>
> `/locale auto` 写下的是 `auto` 而**不是**当次解析出的语言 —— auto 的语义是「每次启动重新看 `LC_ALL`/`LANG`」，
> 把解析结果写死会让它安静地不再跟随系统。

## 交互式权限

工具调用可能需要你授权。权限模式（叠加在 guard profile 之上）：

| 模式 | 行为 |
|---|---|
| `default` | 每个需要授权的操作都询问你 |
| `allow-edits` | 自动放行文件编辑类操作 |
| `yolo` | 自动放行所有操作（谨慎） |
| `auto` | 自动模式 |

交互式权限**仅在 WebSocket（主传输）路径可用**；SSE 备用路径用静态 profile，不弹窗（见 [../adr/0010-sse-static-profile-no-interactive-perm.md](../adr/0010-sse-static-profile-no-interactive-perm.md)）。权限弹窗通过 WebSocket 往返。

## 多窗口自愈

多个 yanshi 窗口共享同一个项目后端：第一个启动的认领 lockfile（位于 OS cache 目录下、按项目划分）并作为后端 owner；后续窗口发现它并作为客户端连接。owner 退出时，lockfile 带 PID 存活回收——断开的客户端重新发现，第一个发现没有存活后端的客户端引导一个新后端并认领 lockfile。

## `yanshi` 用法

<!-- BEGIN GENERATED: help:yanshi -->
```text
yanshi 0.4.0 — the CLI for the yanshi agent server.

Usage:
  yanshi                                self-contained TUI (discovers or embeds the backend)
  yanshi chat    [--no-tui] [-server URL] [-inprocess] [-fake-model] [-config FILE] [-token TOKEN]
  yanshi chat    [--no-tui] [-p "prompt" | stdin] [--input text|lines|jsonl] [-output text|jsonl] [-timeout 1m] [-resume ID]
  yanshi exec    [-p "prompt" | stdin] [--input text|lines|jsonl] [-output text|jsonl] [-timeout 1m] [-resume ID]
  yanshi serve   [-config config.yaml] [-fake-model] [-addr ADDR]
  yanshi app     [-config config.yaml] [-fake-model]
  yanshi goal    [-config config.yaml] [-fake-model] [-workdir DIR] [-agent claudecode] [-max-iters 5] [-max-tokens 0] [-goal "text"] [-tier auto|t0..t4]
  yanshi vcs-mcp (env-driven; spawned by the ACP adapter — YANSHI_DB_PATH/YANSHI_REPO_ID/YANSHI_WT_ID/YANSHI_AGENT/YANSHI_WORKTREE_DIR)
  yanshi init    [-config FILE] [-template FILE] [-force]
  yanshi daemon  status|stop|reload [-root DIR] [-json] [-config FILE] [-timeout 20s]
  yanshi schedule list|show|pause|resume|run-now|delete [ID] [-root DIR] [-json]
  yanshi provider add|list [-config FILE] [-name N] [-kind K] [-model M] [-api-key K] [-replace] [-json]
  yanshi models  pull|preheat -model NAME [-base-url URL]
  yanshi acp     [-config config.yaml] [-fake-model]
  yanshi doctor [-config FILE] [-json] [-release] [-offline] [-fix] [-fix-only LIST] [-fix-dry-run]
  yanshi pr      <PR-number> | <full-URL>
  yanshi enqueue [-config FILE] <session-id> <message...> | -list <session-id>
  yanshi auth    status|logout|device [-provider NAME] [-account NAME]
  yanshi auth    mcp-login <server> | mcp-logout <server>

Subcommands:
  (none)   Launch the self-contained TUI. Discovers a running backend for the
           current project via a lockfile, or embeds one in-process. WebSocket is
           the primary transport (multi-turn, tool-aware); SSE is the fallback.
  chat     Same TUI as the bare invocation. --no-tui drops to the shared headless
           runner (defaults to line input so one-prompt-per-line still works).
           The headless path adds JSONL output, resume, timeout, and stable exit
           codes (0/1/2/124/130). -server/-inprocess force a backend.
  exec     Headless single/multi-prompt runner. Reads prompts via -p, --file, or
           stdin (text/lines/jsonl). Prints assistant text to stdout (text mode)
           or one stable JSONL object per event (jsonl mode), with progress and
           session id on stderr. Stable exit codes: 0 ok, 1 runtime error, 2
           usage, 124 timeout, 130 cancelled. --resume continues a prior session
           once; --fake-model needs no API key.
  serve    Start the HTTP server as a shared daemon (SIGINT/SIGTERM to stop).
           Other yanshi invocations in the same project discover it.
  app      Run the JSON-RPC 2.0 app-server on stdio. Drives the same shared
           v1 agent service as HTTP; item streams arrive as item/updated
           notifications (one JSON object per line). Diagnostics go to stderr
           so stdout stays parseable. -fake-model needs no API key.
  goal     Run the self-driven goal loop (plan-implement-evaluate-judge).
  vcs-mcp  Run the autoVCS MCP server on stdio (spawned by the ACP adapter).
  init     Generate a config.yaml from config.example.yaml. Refuses to
           overwrite an existing config unless -force (which backs it up
           first). ${VAR} references are left as references — nothing writes
           a credential to disk — and the summary names every provider
           environment variable that is still unset.
  daemon   Operate an already-running backend for this project, found through
           the same lockfile the TUI uses. status prints pid / address /
           uptime / readiness (exit 0 only when ready). stop asks it to shut
           down and waits for the process to go. reload makes it re-read the
           config, applying what can be applied and REFUSING the rest with a
           reason — a listen address or a database path cannot change under a
           running process, and reload says so rather than pretending.
  schedule  Operate the scheduled automations held by the running daemon: list
           them with their next fire time, show one with its run history,
           pause / resume / run-now / delete. Creating an automation stays a
           model-facing tool; this is the operations surface.
  provider  Add or list the LLM providers in config.yaml. add prompts for
           whatever the flags omit (and needs every value as a flag when no
           terminal is attached, so it scripts). The API key goes into the
           secrets backend and only a secret:// reference is written to the
           config, so the file stays safe to copy and to attach to a report.
           Providers are bound at boot, so a new one needs a restart.
  models   Explicitly pull an Ollama model or preheat (cold-load) an LM
           Studio model. Unlike doctor, which only ever probes, these have a
           real side effect the operator asked for, and force-refresh the
           local discovery cache afterward.
  acp      Speak the Agent Client Protocol as the AGENT on stdio, exposing
           yanshi's own orchestrator to an ACP host such as Zed. Protocol
           frames go to stdout and diagnostics to stderr, the same contract
           the app subcommand uses. This is the reverse of the ACP CLIENT the goal
           loop and acp_delegate use to drive somebody else's agent.
  doctor   One-time self-check of config, database, providers, ACP CLIs,
           lockfile, port, directories, and sandbox status. Prints ok/warn/fail
           per check; -json emits machine-readable output. Exit 0 all ok /
           1 warn / 2 fail. Never prints secrets. -fix additionally performs a
           closed allowlist of repairs (missing directories, missing required
           config blocks, dead lockfiles, over-permissive file modes), backing
           up every file it edits and refusing the file-editing ones when not
           attached to a terminal. It never touches provider credentials and
           never deletes a database.
  pr       Fetch a GitHub pull request into the session as context. Takes a
           PR number (run from the repo directory) or a full URL (any repo).
  enqueue  Queue a user message for a session, connected or not. It is stored
           in the project database and delivered, in enqueue order, the next
           time that session is resumed by a headless run ("exec -resume" or
           "chat --no-tui -resume"); the interactive TUI has no -resume flag.
           -list shows what is waiting without consuming it.
  auth     Manage authenticated sessions: RFC 8628 device flow (status /
           logout / device) and MCP OAuth (mcp-login / mcp-logout, the
           authorization_code + PKCE flow for an enterprise MCP server; the
           tokens go to the secrets backend and the refresh token is rotated
           automatically). Never echoes a secret. Provider api_keys are not
           managed here — use "yanshi provider add", which puts the key in the
           secrets backend too.
```
<!-- END GENERATED: help:yanshi -->
