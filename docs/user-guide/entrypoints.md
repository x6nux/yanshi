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
  -approve string
    	answer permission requests the server left to a human: never | required (one-shot allow for irreversible external effects) | all (default "never")
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
  -mode string
    	permission mode for the run: default | allow-edits | yolo | auto | strict | plan (empty = the connection's current mode)
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
    	token budget for the whole goal run (0 = unlimited); when resuming, a value typed here replaces the stored one
  -reset
    	discard the saved resume point for -workdir and exit, so the next run starts over with a full budget
  -tier string
    	difficulty tier: "auto" (model classifies, keyword table as fallback) or t0..t4 (quick-fix, standard, designed, team, autonomous) (default "auto")
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

## enqueue（向会话排队消息）

```sh
./yanshi enqueue <session-id> 跑一遍 release 脚本
./yanshi enqueue -list <session-id>
```

适用：向一个**当前没有连接**（或正在运行）的会话排一条用户消息。消息落在项目数据库里，
下一次该会话被 **headless 运行**恢复时按入队顺序投递，投递后标记已消费（至多一次）。
`-list` 只看不取。

能取到队列的只有这两条写法（都走 `runHeadlessCommand`）：

```sh
./yanshi exec -resume <session-id> -p "继续"
printf '继续\n' | ./yanshi chat --no-tui -resume <session-id>
```

**交互式 TUI（不带 `--no-tui` 的 `yanshi chat`）没有 `-resume` 这个 flag**，
也不会消费队列 —— `chat` 只有在检测到 `--no-tui` 时才路由到 headless 入口。

<!-- BEGIN GENERATED: help:enqueue -->
```text
Usage: yanshi enqueue [-config FILE] <session-id> <message...>
       yanshi enqueue [-config FILE] -list <session-id>

Queue a user message for a session, whether or not anything is connected to it.
The message is stored in the project database and delivered, in enqueue order,
the next time that session is resumed by a HEADLESS run:

  yanshi exec -resume <session-id> -p "..."
  yanshi chat --no-tui -resume <session-id>

(the second reads its prompts from stdin, one per line). The interactive TUI —
plain "yanshi chat" — has no such flag and does not drain the queue.

  -list   show what is waiting for a session without consuming it

The message may be given as several arguments; they are joined with spaces.
```
<!-- END GENERATED: help:enqueue -->

## -b（后台守护进程）

```sh
./yanshi -b [-config config.yaml] [-fake-model] [-addr ADDR] [-json] [-wait 30s]
./yanshi serve -b            # 同一条请求
```

适用：把后端作为**脱离本终端**的守护进程起起来。它与 `yanshi serve &` 的差别是这个功能存在的理由：子进程进自己的会话（unix `setsid`，Windows `DETACHED_PROCESS`），终端关闭或 SIGHUP 打不到它；输出落到锁文件旁边的 per-project 日志；命令在守护进程**回答 readiness 之后**才返回，脚本的下一条不必和 bootstrap 抢跑。**启动是幂等的**：已有存活的 owner 就报告它并退出 0，不会起第二个（同一个 SQLite 上两个后端正是锁文件要防的状态）；owner 活着但还没 ready 时会等它。`-json` 输出 `{started,alreadyRunning,pid,addr,log,root}`。之后用 `yanshi daemon status|stop|reload` 操作它。

## ipc（本地 IPC：unix socket 上的 JSON-RPC）

```sh
./yanshi ipc initialize                 # 一次性调用，打印 result
./yanshi ipc <method> [-params JSON] [-root DIR]
printf '%s\n' '{"jsonrpc":"2.0","id":1,"method":"capabilities"}' | ./yanshi ipc
```

适用：**不带 token、不猜端口**地驱动一个已经在跑的守护进程。socket 在锁文件旁边、由文件系统保护（`0600`），协议与 `yanshi app` 逐字相同（线程/回合/条目 + 控制面方法）。无 method 时做 stdio 桥：stdin 的 NDJSON 直接进 socket。`-root` 默认取当前目录，也可用 `YANSHI_ROOT`。

<!-- BEGIN GENERATED: help:ipc -->
```text
usage: yanshi ipc [-root DIR] [-config FILE]
       yanshi ipc <method> [-params JSON] [-root DIR] [-config FILE]

  (no method)   Bridge stdio to the daemon: newline-delimited JSON-RPC in on
                stdin, responses and item/updated notifications out on stdout.
                This is the same protocol "yanshi app" speaks on stdio; the
                only difference is that the server is the project's RUNNING
                daemon instead of a process you just started.
  <method>      Send one request and print its result (or the error), then
                exit. -params takes the JSON params object.

The socket lives in the per-user cache directory next to the daemon's lockfile,
is created 0600, and needs no token: the filesystem is the access control. Start
a daemon with "yanshi -b" if nothing is listening.
```
<!-- END GENERATED: help:ipc -->

## session（离线会话管理）

```sh
./yanshi session list [-archived] [-limit N] [-json]
./yanshi session show <id> [-tail N] [-json]
./yanshi session rename <id> <title>
./yanshi session archive <id> | unarchive <id>
./yanshi session delete <id> yes
```

适用：TUI 的 `/sessions` `/rename` `/archive` `/delete` 的脚本面。**不需要守护进程**（直接开项目的 SQLite），所以后端卡死时这些命令照样可用。`delete` 要求字面量 `yes`，与 TUI 同一道闸。

<!-- BEGIN GENERATED: help:session -->
```text
usage: yanshi session <verb> [args] [-config FILE] [-json]

  list   [-archived] [-limit N]   stored sessions, newest first
  show   <id> [-tail N]           one session: metadata, token ledger, last turns
  rename <id> <title>             set the session title
  fork   <id> [-upto N]           copy a session (N = stop after that seq; default all)
  archive <id> | unarchive <id>   hide / restore a session
  delete <id> yes                 delete a session and its messages

Every verb takes -config FILE (default config.yaml) and, where it prints a
result, -json for one machine-readable object instead of text.

These are the operations the TUI exposes as /sessions, /rename, /archive,
/unarchive, /archived and /delete. They run OFFLINE against the project's
SQLite file (no daemon required), which is what makes them usable in a script
and usable when the backend is wedged — the same reason yanshi doctor never
needs it either.
```
<!-- END GENERATED: help:session -->

## usage 与控制面（skills / features / approvals / jobs / mcp / vcs）

```sh
./yanshi usage [<session-id>] [-limit N] [-json]     # 离线：读账本
./yanshi skills list | show <name> | enable|disable|trust|untrust <name>
./yanshi features list | set <key> on|off
./yanshi approvals list [-session ID] | revoke <rule-id>
./yanshi jobs list | read <id> [-max N] | write <id> <data> | cancel <id>
./yanshi mcp list | enable <name> | disable <name>
./yanshi vcs log [-limit N] | diff <from> [to]
./yanshi models list
```

适用：把只在 TUI 里存在的运维动作搬到脚本里。除 `usage` 外都描述**某个进程的实时状态**，因此走守护进程的 IPC socket：没有守护进程时它们**报错**并提示 `yanshi -b`，而不是返回空列表 —— 「没有作业」和「我看不见持有作业的进程」是两件不同的事。`features set` 是**非持久**覆盖（与 `/features` 一致），要永久生效请改 `config.yaml`。⚠️ 裸 `yanshi mcp` 仍然是 **stdio MCP server**，只有 `list|enable|disable` 开头的才是管理动词。

<!-- BEGIN GENERATED: help:usage -->
```text
usage: yanshi <verb> [args] [-root DIR] [-json]

  usage     [<session-id>] [-limit N]     token/cost roll-up (offline)
  skills    list | show <name>            loaded skills, their state and body
  features  list | set <key> on|off       runtime feature flags
  approvals list [-session ID] | revoke <rule-id> [-session ID]
  jobs      list | read <id> [-max N] | cancel <id> | write <id> <data>
  mcp       list | enable <name> | disable <name>
  vcs       log [-limit N] | diff <from> [to]
  models    list                         models a session can switch to

Everything except usage/session reads state that lives in a RUNNING daemon and
goes through its IPC socket; start one with "yanshi -b". -json prints the raw
result object instead of a table.
```
<!-- END GENERATED: help:usage -->

<!-- BEGIN GENERATED: help:skills -->
```text
usage: yanshi <verb> [args] [-root DIR] [-json]

  usage     [<session-id>] [-limit N]     token/cost roll-up (offline)
  skills    list | show <name>            loaded skills, their state and body
  features  list | set <key> on|off       runtime feature flags
  approvals list [-session ID] | revoke <rule-id> [-session ID]
  jobs      list | read <id> [-max N] | cancel <id> | write <id> <data>
  mcp       list | enable <name> | disable <name>
  vcs       log [-limit N] | diff <from> [to]
  models    list                         models a session can switch to

Everything except usage/session reads state that lives in a RUNNING daemon and
goes through its IPC socket; start one with "yanshi -b". -json prints the raw
result object instead of a table.
```
<!-- END GENERATED: help:skills -->

<!-- BEGIN GENERATED: help:features -->
```text
usage: yanshi <verb> [args] [-root DIR] [-json]

  usage     [<session-id>] [-limit N]     token/cost roll-up (offline)
  skills    list | show <name>            loaded skills, their state and body
  features  list | set <key> on|off       runtime feature flags
  approvals list [-session ID] | revoke <rule-id> [-session ID]
  jobs      list | read <id> [-max N] | cancel <id> | write <id> <data>
  mcp       list | enable <name> | disable <name>
  vcs       log [-limit N] | diff <from> [to]
  models    list                         models a session can switch to

Everything except usage/session reads state that lives in a RUNNING daemon and
goes through its IPC socket; start one with "yanshi -b". -json prints the raw
result object instead of a table.
```
<!-- END GENERATED: help:features -->

<!-- BEGIN GENERATED: help:approvals -->
```text
usage: yanshi <verb> [args] [-root DIR] [-json]

  usage     [<session-id>] [-limit N]     token/cost roll-up (offline)
  skills    list | show <name>            loaded skills, their state and body
  features  list | set <key> on|off       runtime feature flags
  approvals list [-session ID] | revoke <rule-id> [-session ID]
  jobs      list | read <id> [-max N] | cancel <id> | write <id> <data>
  mcp       list | enable <name> | disable <name>
  vcs       log [-limit N] | diff <from> [to]
  models    list                         models a session can switch to

Everything except usage/session reads state that lives in a RUNNING daemon and
goes through its IPC socket; start one with "yanshi -b". -json prints the raw
result object instead of a table.
```
<!-- END GENERATED: help:approvals -->

<!-- BEGIN GENERATED: help:jobs -->
```text
usage: yanshi <verb> [args] [-root DIR] [-json]

  usage     [<session-id>] [-limit N]     token/cost roll-up (offline)
  skills    list | show <name>            loaded skills, their state and body
  features  list | set <key> on|off       runtime feature flags
  approvals list [-session ID] | revoke <rule-id> [-session ID]
  jobs      list | read <id> [-max N] | cancel <id> | write <id> <data>
  mcp       list | enable <name> | disable <name>
  vcs       log [-limit N] | diff <from> [to]
  models    list                         models a session can switch to

Everything except usage/session reads state that lives in a RUNNING daemon and
goes through its IPC socket; start one with "yanshi -b". -json prints the raw
result object instead of a table.
```
<!-- END GENERATED: help:jobs -->

<!-- BEGIN GENERATED: help:vcs -->
```text
usage: yanshi <verb> [args] [-root DIR] [-json]

  usage     [<session-id>] [-limit N]     token/cost roll-up (offline)
  skills    list | show <name>            loaded skills, their state and body
  features  list | set <key> on|off       runtime feature flags
  approvals list [-session ID] | revoke <rule-id> [-session ID]
  jobs      list | read <id> [-max N] | cancel <id> | write <id> <data>
  mcp       list | enable <name> | disable <name>
  vcs       log [-limit N] | diff <from> [to]
  models    list                         models a session can switch to

Everything except usage/session reads state that lives in a RUNNING daemon and
goes through its IPC socket; start one with "yanshi -b". -json prints the raw
result object instead of a table.
```
<!-- END GENERATED: help:vcs -->

## 无人值守的审批（`exec -approve`）

```sh
./yanshi exec -mode yolo -approve required -p "跑一遍发布检查"
```

`-mode` 决定**服务端**自己解决什么（`yolo` 放行 profile 策略类拒绝、`auto` 交给 guardian 模型），`-approve` 决定**客户端**如何回答服务端故意留给人的问题：`never`（默认，一律拒绝）、`required`（对不可逆外部效果一次性放行，仍拒 force-prompt 工具）、`all`（连 force-prompt 也一次性放行）。策略**只会发一次性 allow**，不会写永久规则。

## auth（凭据管理）

```sh
./yanshi auth status
./yanshi auth logout
./yanshi auth device --provider <id>
```

适用：管理 RFC 8628 device-flow 会话（status/logout/device）。provider 的
api_key **不在这里管** —— 写进 `config.yaml` 的 `llm.providers[].api_key`，
可以是明文字面量或 `${VAR}`。

<!-- BEGIN GENERATED: help:auth -->
```text
usage: yanshi auth <status|logout|device> ...
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
  -fix
    	repair an allowlisted set of problems after reporting (see -fix-only for the list)
  -fix-dry-run
    	with -fix, report what would be repaired without touching anything
  -fix-only string
    	comma-separated subset of repairs to run (default: all allowlisted)
  -json
    	emit machine-readable JSON instead of human-readable text
  -offline
    	check local-runtime discovery from the on-disk cache only, never over the network (for sandboxes with no loopback egress)
  -release
    	promote release-blocking warns to fails (release runbook; see docs/upgrade-guide.md)
```
<!-- END GENERATED: help:doctor -->

## SDK 与 IDE

- TypeScript SDK（`@x6nux/yanshi-sdk`）与 Python SDK（`yanshi_sdk`）：以代码驱动同一个 v1 agent service。最小端到端见 [../api/sdk-ts.md](../api/sdk-ts.md) / [../api/sdk-python.md](../api/sdk-python.md)，可跑样例见 [../../examples/](../../examples/)。
