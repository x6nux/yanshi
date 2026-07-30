# Yanshi 用户指南

> 面向**终端用户**的"怎么用 yanshi"。贡献者请看 [../../CONTRIBUTING.md](../../CONTRIBUTING.md)；架构决策与不可违反的约束请看 [../adr/](../adr/)；权威的内部架构描述请看根目录 `CLAUDE.md`。

yanshi 是一个 Go LLM agent 服务端：以 ReAct 编排器为核心，自带 fail-closed 安全守卫、基于 SQLite 的自动版本控制（autoVCS）、标准技能系统、自驱动目标循环，以及一个 Bubble Tea TUI。**单个 `yanshi` 二进制同时充当客户端（TUI）与服务端**——无需另起 daemon。

## 第一次跑？从这里开始

[getting-started.md](getting-started.md) —— 零 API key、零外部依赖，用 `--fake-model` 三步跑通。

## 专题页

| 专题 | 内容 |
|---|---|
| [getting-started.md](getting-started.md) | 零依赖首跑（`--fake-model`） |
| [configuration.md](configuration.md) | 全配置块说明 + 生成的配置骨架表 |
| [tui.md](tui.md) | TUI 命令、键位、交互式权限、多窗口自愈 |
| [entrypoints.md](entrypoints.md) | 各入口（TUI / serve / exec / app / SDK / IDE）的适用场景 |
| [skills.md](skills.md) | 技能放哪、怎么写 SKILL.md、渐进披露 |
| [autovcs.md](autovcs.md) | 编辑自动追踪、worktree 的用户面视角 |
| [goalloop.md](goalloop.md) | `yanshi goal` 的 plan→implement→evaluate→judge |
| [guard.md](guard.md) | 四维权限、profile、交互式模式、fail-closed |

## 相关参考

- 对外 API 参考（资源 / 事件 / JSON-RPC / SDK）：[../api/](../api/)
- 可运行示例（headless / SDK / 自定义工具）：[../../examples/](../../examples/)
- 活技术参考：[../compaction.md](../compaction.md)（上下文压缩）、[../vcs.md](../vcs.md)（autoVCS 内部）、[../skills-authoring.md](../skills-authoring.md)（技能编写权威指南）。
