# 入口与运行模式

yanshi 单二进制有多个入口，对应不同的运行模式。每个入口给"适用场景 + 一行启动命令"。

## bare TUI（默认）

```sh
./yanshi                # 自包含 TUI：发现或嵌入后端
```

适用：日常交互式使用。详见 [tui.md](tui.md)。

## serve（共享 daemon）

```sh
./yanshi serve [-config config.yaml] [-fake-model] [-addr ADDR]
```

适用：作为共享 HTTP daemon 长跑；同项目的其它 yanshi 调用会发现它。SIGINT/SIGTERM 优雅关闭。

<!-- BEGIN GENERATED: help:serve -->
```text
Usage of serve:
  -addr string
    	override the config's HTTP listen address
  -config string
    	path to configuration file (default "config.yaml")
  -fake-model
    	use a deterministic fake model (no API keys needed)
```
<!-- END GENERATED: help:serve -->

## chat（TUI 或 headless）

```sh
./yanshi chat            # 同 bare TUI
./yanshi chat --no-tui -p "hello"   # headless：共享 headless 运行器
```

`--no-tui` 退到共享 headless 运行器（`exec` 同源），默认按行输入，支持 JSONL 输出、resume、timeout、稳定退出码。`-server`/`-inprocess` 强制后端模式。

<!-- BEGIN GENERATED: help:chat -->
```text
Usage of chat:
  -config string
    	path to configuration file (default "config.yaml")
  -fake-model
    	use a deterministic fake model
  -inprocess
    	force in-process backend
  -server string
    	force connect to this server URL
  -token string
    	bearer token (ignored for loopback)
```
<!-- END GENERATED: help:chat -->

## exec（headless 单/多 prompt）

```sh
./yanshi exec --fake-model -p "hello"
./yanshi exec --input jsonl --output jsonl < prompts.jsonl
```

适用：脚本 / CI / 无 TTY。读 `-p`、`--file` 或 stdin（text/lines/jsonl），assistant 文本打 stdout（text 模式）或每事件一个 JSONL（jsonl 模式）。稳定退出码：0 ok / 1 运行错误 / 2 用法 / 124 超时 / 130 取消。`--resume` 续接一次之前的会话。

<!-- BEGIN GENERATED: help:exec -->
```text
Usage of exec:
  -config string
    	path to configuration file (default "config.yaml")
  -fake-model
    	use deterministic fake model
  -file string
    	read input from FILE instead of stdin
  -inprocess
    	force in-process backend
  -input string
    	input mode: text | lines | jsonl (default "text")
  -output string
    	output format: text | jsonl (default "text")
  -p string
    	prompt text; with input=text only
  -prompt string
    	alias for -p
  -resume string
    	restore session id before the first turn
  -server string
    	force connect to this server URL
  -timeout duration
    	abort after this duration (0 = no limit)
yanshi exec: flag: help requested
```
<!-- END GENERATED: help:exec -->

## app（JSON-RPC 2.0 app-server）

```sh
./yanshi app [-config config.yaml] [-fake-model]
```

适用：被本地 supervisor（IDE 扩展、notebook 运行时、CLI 包装）以 stdio 上的 JSON-RPC 2.0 驱动。驱动**同一个** v1 agent service（语义与 HTTP/SSE 不漂移）；item 流以 `item/updated` 通知（每行一个 JSON 对象）到达；诊断走 stderr 以保 stdout 可解析。详见 [../api/jsonrpc.md](../api/jsonrpc.md)。

<!-- BEGIN GENERATED: help:app -->
```text
Usage of app:
  -config string
    	path to configuration file (default "config.yaml")
  -fake-model
    	use deterministic fake model (no API keys needed)
```
<!-- END GENERATED: help:app -->

## goal（自驱动目标循环）

```sh
./yanshi goal --fake-model --max-iters 2 -goal "add a test"
./yanshi goal -tier auto -goal "..."
```

适用：自驱动 plan→implement→evaluate→judge 循环。详见 [goalloop.md](goalloop.md)。

<!-- BEGIN GENERATED: help:goal -->
```text
Usage of goal:
  -agent string
    	external agent for implementation (real path) (default "claudecode")
  -config string
    	path to configuration file (default "config.yaml")
  -fake-model
    	use fake planner/implementer/evaluator (no API keys or CLIs needed)
  -goal string
    	goal text (alternatively, pass as positional arg)
  -history int
    	print the last N goal run records and exit (0 = run a goal)
  -max-iters int
    	maximum goal loop iterations (default 5)
  -max-tokens int
    	token budget for the whole goal run (0 = unlimited)
  -tier string
    	difficulty tier: "auto" (RuleTierer) or t0..t4 (quick-fix, standard, designed, team, autonomous) (default "auto")
  -workdir string
    	working directory for implementation (default ".")
```
<!-- END GENERATED: help:goal -->

## vcs-mcp（autoVCS MCP server）

```sh
./yanshi vcs-mcp   # 环境变量驱动，由 ACP adapter 拉起
```

适用：作为 stdio 上的 MCP server 暴露 `vcs_*` 工具给被拉起的外部 agent。环境变量配置：`YANSHI_DB_PATH` / `YANSHI_REPO_ID` / `YANSHI_WT_ID` / `YANSHI_AGENT` / `YANSHI_WORKTREE_DIR`。通常不手动调用。

<!-- BEGIN GENERATED: help:vcs-mcp -->
```text

```
<!-- END GENERATED: help:vcs-mcp -->

## pr（PR 审阅）

```sh
./yanshi pr <PR-number>      # 当前仓库
./yanshi pr <full-URL>       # 任意仓库
```

适用：拉取一个 GitHub PR 并在会话里审阅。

<!-- BEGIN GENERATED: help:pr -->
```text
Usage: yanshi pr <PR-number>  (run from the repo directory)
       yanshi pr <full-URL>   (any repo)
```
<!-- END GENERATED: help:pr -->

## auth（凭据管理）

```sh
./yanshi auth set --provider openai
./yanshi auth status
./yanshi auth logout
./yanshi auth device --provider <id>
```

适用：管理 provider 凭据（set/status/logout，或 RFC 8628 device flow）。

<!-- BEGIN GENERATED: help:auth -->
```text
usage: yanshi auth <set|status|logout|device> ...
```
<!-- END GENERATED: help:auth -->

## doctor（自检）

```sh
./yanshi doctor [-config FILE] [-json]
```

适用：一次性自检 config、数据库、providers、ACP CLI、lockfile、端口、目录、sandbox。每项打印 ok/warn/fail；`-json` 输出机器可读。退出码：0 全 ok / 1 warn / 2 fail。从不打印 secrets。

<!-- BEGIN GENERATED: help:doctor -->
```text
Usage of doctor:
  -config string
    	path to configuration file (default "config.yaml")
  -json
    	emit machine-readable JSON instead of human-readable text
  -release
    	promote release-blocking warns to fails (release runbook; see docs/upgrade-guide.md)
```
<!-- END GENERATED: help:doctor -->

## SDK 与 IDE

- TypeScript SDK（`@x6nux/yanshi-sdk`）与 Python SDK（`yanshi_sdk`）：以代码驱动同一个 v1 agent service。最小端到端见 [../api/sdk-ts.md](../api/sdk-ts.md) / [../api/sdk-python.md](../api/sdk-python.md)，可跑样例见 [../../examples/](../../examples/)。
