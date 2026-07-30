# 快速开始（零 API key）

本指南让你**不需要任何 LLM API key**、不需要安装额外工具，三步跑通 yanshi。全程使用内置的 `--fake-model`——一个确定性 fake model，`llm.providers` 为空时也会被自动选中。

## 1. 构建

```sh
go build -o yanshi ./cmd/yanshi
```

产物是单个 `yanshi` 二进制（Windows 上是 `yanshi.exe`），既是客户端也是服务端。

## 2. 准备配置

```sh
cp config.example.yaml config.yaml
```

`config.yaml` 已被 gitignore（从被跟踪的 `config.example.yaml` 复制而来）。里面的 `${VAR}` 形式环境变量会在加载前展开。**用 `--fake-model` 时你不需要填任何真实的 API key**——留空即可。

> **提示**：如果你的环境里已经导出了 `OPENAI_API_KEY` / `ANTHROPIC_API_KEY` 等变量，`config.example.yaml` 里的 `${VAR}` 会被展开成真实值，触发 raw-literal 校验（拒绝明文 key）。只想跑 fake model 时，用一份**不含 `llm.providers`** 的最小 config 更省事：
> ```yaml
> server: { http_addr: "127.0.0.1:0" }
> storage: { sqlite_path: ":memory:" }
> ```

## 3. 启动 TUI

```sh
./yanshi --fake-model -inprocess
```

`-inprocess` 强制在进程内嵌入后端（跳过后端发现），`--fake-model` 接入确定性 fake model。你会看到 Bubble Tea 全屏 TUI。

> **alt-screen TUI 无法通过管道驱动**（它接管终端）。在 CI / 管道 / 无 TTY 的环境里用下面的 headless 路径；或用 `./yanshi -h` 打印用法并退出 0 做一个最小自检。

## 你会看到什么

- 启动后是一个输入框。**Enter 发送**一个 turn，**Ctrl+Enter 换行**（这一区分来自本地 bubbletea fork，见 [../../CONTRIBUTING.md](../../CONTRIBUTING.md)）。
- 发送一段文字后，fake model 会回放一个确定性回复；如果触发了工具调用，你会看到 `tool.call` / `tool.result` 流式事件。
- 用 `/` 前缀可调出命令（如 `/model`、`/skill`），详见 [tui.md](tui.md)。

## 4. Headless 替代（无 TUI 环境）

```sh
./yanshi exec --fake-model -p "hello"
```

`exec` 是 headless 单/多 prompt 运行器：读 `-p`、`--file` 或 stdin，把 assistant 文本打到 stdout。`--fake-model` 不需要任何 API key。退出码稳定：0 成功 / 1 运行错误 / 2 用法 / 124 超时 / 130 取消。

## 下一步

- [configuration.md](configuration.md)：逐块理解配置。
- [tui.md](tui.md)：TUI 命令、键位、交互式权限。
- [entrypoints.md](entrypoints.md)：`serve` / `app` / SDK / IDE 各入口的适用场景。
- [../../examples/](../../examples/)：可复制即跑的示例脚本与 SDK 端到端样例。
