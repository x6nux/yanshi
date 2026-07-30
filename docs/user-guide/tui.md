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

> 区分 Enter 与 Ctrl+Enter 依赖本地 bubbletea fork（上游在 Windows 上把两者都收敛为 `KeyEnter`）；详见 [../../CONTRIBUTING.md](../../CONTRIBUTING.md)。`/keymap` 可切换键位方案，`tui.keymap` 配置默认方案。

## `/` 前缀命令

在输入框输入 `/` 调出命令。常用的有：

- `/model`：切换当前会话的 provider/model（按 turn 切换，无需重建编排器）。
- `/skill`：调用一个技能。
- `/keymap`、`/vim`、`/contrast`：切换键位 / Vim 模式 / 高对比度主题（写入 preferences.json）。

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
  yanshi goal    [-config config.yaml] [-fake-model] [-workdir DIR] [-agent claudecode] [-max-iters 5] [-goal "text"] [-tier auto|t0..t4]
  yanshi vcs-mcp (env-driven; spawned by the ACP adapter — YANSHI_DB_PATH/YANSHI_REPO_ID/YANSHI_WT_ID/YANSHI_AGENT/YANSHI_WORKTREE_DIR)
  yanshi doctor [-config FILE] [-json] [-release]

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
  doctor   One-time self-check of config, database, providers, ACP CLIs,
           lockfile, port, directories, and sandbox status. Prints ok/warn/fail
           per check; -json emits machine-readable output. Exit 0 all ok /
           1 warn / 2 fail. Never prints secrets.
```
<!-- END GENERATED: help:yanshi -->
