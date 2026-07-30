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
Usage of yanshi:
  -config string
    	path to configuration file (default "config.yaml")
  -fake-model
    	use a deterministic fake model
  -inprocess
    	force in-process backend (skip discovery)
  -server string
    	force connect to this server URL (skip discovery)
```
<!-- END GENERATED: help:yanshi -->
