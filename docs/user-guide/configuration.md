# 配置参考

yanshi 从 `config.yaml` 加载配置（已被 gitignore；从被跟踪的 `config.example.yaml` 复制）。加载时先做 `${VAR}` 环境变量展开，再反序列化，最后 `applyDefaults` 补零值、`validate` 做范围校验。本页解释**每个顶层块的语义、默认值与块间关系**（解释"为什么"），字段级 key/type 速查见文末[生成骨架表](#配置字段骨架由-gendocs-生成)。

> `llm.providers[].api_key` 只接受两种写法：**明文字面量**，或 **`${VAR}`**（加载时由 `os.ExpandEnv` 展开，展开结果同样是明文）。两者都原样交给 provider SDK，不经任何凭据解析或加密存储。写 `secret://…` / `env://…` 不再有特殊含义 —— 那串字符会被当成 key 本身发出去。
>
> 明文 key 仍会注册进进程 Redactor，因此不会出现在日志、WS/SSE 帧或 SQLite 里。但 `config.yaml` 本身是明文文件（已被 gitignore），请自行控制它的读权限。
>
> `llm.providers[].headers`（map，W-C-02）是附加在每个请求上的自定义 HTTP 头（企业网关 token、Azure 网关 key 之类）。值同样接受 `${VAR}` 展开，且同样注册进 Redactor —— 只有值被注册，头名不算凭据。三种 provider kind 都支持：`openai` kind 由 `retryafter.go` 的传输层注入，`anthropic`/`openai-responses` 由各自的 `setHeaders` 注入；三者都在**内置头之后**应用，因此一条 `headers` 条目可以覆盖内置头名（例如自定义 `Authorization`、`anthropic-version`）而不是被静默盖掉。

## server

HTTP 服务（`http_addr`）与任务 broker（`task_addr`）监听地址。bare TUI / `serve` / `chat` 都从这里起 HTTP 服务；`task_addr` 是任务 broker 的独立监听。

## storage

SQLite 持久化（`sqlite_path`）。F1 的 WAL 相关项：`wal_max_open_conns` 放开读连接池上限（0/省略=4，1=旧行为串行）；`busy_timeout_ms` 跨进程锁冲突重试窗口（0/省略=5000）；`wal_auto_checkpoint` WAL 自动 checkpoint 页阈值（0/省略=1000，负数=禁用被动 checkpoint）。写仍由进程内 writeMu 串行，这些值只影响读并行度。用 `:memory:` 跑内存库（测试）。

## token

顶层 `token` 是 HTTP 服务的 bearer token。**loopback（127.0.0.1）免 token**——本地 TUI / `serve` 发现无需认证。非 loopback 部署必须改掉 `change-me`。

## llm

`providers` 是 provider 列表，每项有 `name`/`kind`（`openai`|`openai-responses`|`anthropic`）/`model`/`api_key`/`base_url`/`context_window`/`cost_class`/`multimodal`/`auto_compact_threshold`/`headers`/`max_retries`/`auth`。`context_window` 是该模型的 token 窗口；compaction 按它而非全局值估算。`auto_compact_threshold` 是该模型自己的压缩触发比例，覆盖 `compaction.threshold`（同一梯子：显式字段 > 模型目录命中 > 全局回退，W-C-01/ADR-0024）；取值必须是 `context_window` 的**分数**（`<= 1`，否则加载期拒绝启动——像绝对 token 数的值会静默错配压缩门），**负值**是显式信号，会为这一个 provider 单独关闭压缩，即使全局开关是开着的。`multimodal: true` 声明原生图像输入（Tier G）；当主模型非多模态时，bootstrap 自动选第一个 `multimodal==true` 的 provider 作为视觉辅助。`llm.providers` 为空时 `--fake-model` 自动接入确定性 fake model。

`max_retries`（int，W-C-07）是这个 provider 自己的重试上限，覆盖 `llm.max_retries` 这个全局回退值；不设置（省略）时用全局值，设置为 `0` 是显式的"这个 provider 一次都不重试"，两者的区别由指针类型钉住（config.go 的 M4 nil-means-omit 惯例）。`ResilientChatModel` 是重试的唯一权威，per-provider 预算在 failover 换到别的 provider 时会重置，不会带着上一个 provider 已经花掉的次数。负值在加载期直接拒绝启动（`llm.max_retries` 或某个 provider 的 `max_retries` < 0）。

⚠️ **同一个取值 `0` 在这两层是相反的语义。** 顶层 `llm.max_retries` 是普通 `int`（零值就是省略），`0`（或整段省略 `llm` 块）意味着"没配置，用 `ResilientChatModel` 的内置默认 10"；每个 provider 的 `max_retries` 是 `*int`，`0` 是**显式**配置出来的取值，意味着"这个 provider 一次都不重试"。原因就是上一段说的指针类型：顶层没有 nil-means-omit 的空间，`0` 只能读成"没说"；provider 级别有，`0` 因此能读成"说了，说的是零"。

`auth`（W-C-12）配置**命令产出型**凭据：`auth.command`（`[]string`，argv 形式，不经 shell 解析）是产出 token 的命令，`auth.refresh_interval`（duration，默认 `15m`）是刷新周期。配了 `auth` 时 `api_key` 可以留空。命令执行必须走 `secproc`（W-B-02 收敛后的唯一子进程入口），401 会触发一次命令重跑再重试。`auth.command` 为空、或 `refresh_interval` 为负值，都在加载期直接拒绝启动。

⚠️ **`auth.command` 拿不到 yanshi 自己继承的环境凭据。** 这条命令的 `AllowEnv` 留空（`runAuthCommand` 的实现），于是 `netpolicy.ScrubCredentials` 会把父进程环境里所有凭据类变量（`AWS_SECRET_ACCESS_KEY`、`OPENAI_API_KEY` 这类）连同其余环境一起清洗掉——理由与 `shell_run`/ACP agent 等其余不受信子进程完全一致：这是一条产出 token 的**外部命令**，对 yanshi 自己的凭据没有天然主张权。如果 helper 脚本本身依赖某个环境变量（例如用 `AWS_SECRET_ACCESS_KEY` 去换一个临时 STS token），它必须自己想办法拿到这个值（写进脚本、读配置文件、走脚本自己的登录态），而不能指望 yanshi 把它转发进来。

## agents

agent 声明列表（`name`/`type`=`local`|`external`|`remote`/`chain` provider 链/`profile`/`capabilities`）。

> ⚠️ **`agents[].profile` 目前是空操作。** 它没有任何生产读点（`cfg.Agents` 整个字段今天只被声明、不被消费），yaml 解析成功、启动零告警，改它不会改变任何权限。profile 的实际选取方式见下一节。

## profiles

权限 profile 的 map。每个 profile 是一个 `guard.PermissionProfile`，含**五维**：`tools`（glob 白名单，空=拒绝一切，见 [../adr/0003-guard-fail-closed-empty-allow.md](../adr/0003-guard-fail-closed-empty-allow.md)）、`mcp`（允许的 `mcp_<server>_<tool>` 白名单，**空=拒绝一切 MCP 工具，即便 `tools.allow` 含 `*`**，且它排在 `tools` **之前**检查）、`fs`（读/写路径 glob）、`shell`（`policy` + `patterns` + `rules`）、`net`（`allow` + `hosts`）。`security` 块的 sandbox/network/shell 是叠加在 profile 之上的**部署级**姿态。

**profile 靠 map 键名选中**，没有经过 `agents:` 的间接层：

- `orchestrator` —— 聊天/TUI 编排器固定读这个键（`internal/bootstrap::Build`）；缺失则退回内置的 `internal/bootstrap::DefaultOrchestratorProfile`。
- **worker 名** —— task API 的 `GET /api/v1/agent/profile?worker=<名>` 拿 `<名>` 当键名查（`internal/api/http::Server.TaskAPI`）。所以示例里的 `coding` profile 只对 `agent-worker -name coding` 生效；查不到时 fail-closed 退回 deny-all。

`shell.policy` 只接受四个值：`""`（等同 `allowlist`）、`allowlist`、`deny`、`denylist`；**没有 `allow`**，"不限制 shell" 用空 `patterns` 的 `denylist` 表达。写了别的值会在 `config.Load` 阶段直接报错退出（`profiles.<名>.shell.policy: unknown shell policy ...`）—— 因为运行时它会变成连 `yolo`/`auto` 都越不过的结构性 HardDeny。

> **这道校验只看 `shell.rules` 为空的 profile。** `rules` 非空时它完全接管 shell 维度、`policy` 根本不会被读到，那个值是**惰性**的（照它配的 profile 今天跑得好好的），所以校验放过它，不把一个不影响运行的字段变成启动失败。把 `rules` 清空后同一个值就变成活的了，下一次加载会拒。权威实现见 `internal/config::Config.validateProfiles` 的 doc 注释。

维度顺序与两档 HardDeny 详见 [guard.md](guard.md)。

## skills

技能发现路径：`builtin_dir`（内置，默认 `./skills`）、`user_dir`（用户，`~/.yanshi/skills`）、`plugin_dir`（插件，`~/.yanshi/plugins`）。详见 [skills.md](skills.md) 与 [../skills-authoring.md](../skills-authoring.md)。

## vcs

autoVCS 追踪配置。`ignore` 是额外的忽略 pattern（与内置忽略表合并）；`worktree_dir` 是 worktree 工作目录根（默认 `~/.yanshi/worktrees`）。零值（无 vcs 块）仍启用带默认值的追踪。详见 [autovcs.md](autovcs.md) 与 [../adr/0008-autovcs-context-injection-overrides-scope.md](../adr/0008-autovcs-context-injection-overrides-scope.md)。

## compaction

自动上下文压缩。`threshold`（默认 0.8）是触发压缩的 token 占比阈值（est. tokens ≥ threshold × context_window）；`keep_recent`（默认 4）是尾部保留的 user/assistant 对数；`context_window`（默认 256000）是**回退**窗口——当 provider 没设自己的 `context_window` 时用它；`model` 是可选的专用快速 summary 模型（空=用当前会话模型）；`chunk_threshold`（默认 0.9）是切分携带式分块的阈值。**`compaction.context_window` 与 provider 的 `context_window` 是回退关系**：provider 有值则优先。详见 [../compaction.md](../compaction.md) 与 [../adr/0006-compaction-unified-core-strict-window.md](../adr/0006-compaction-unified-core-strict-window.md)。

## memory

MEM1 用户记忆：跨会话偏好笔记，启动时作为独立 suffix 注入 system prompt。`enabled`（默认 false）、`user_path`（默认 `~/.yanshi/memory.md`）、`project_path`（默认 `<workRoot>/.yanshi/memory.md`）、`max_size`（每文件字节上限，0=默认）。

## security

部署级安全姿态（叠加在 profile 之上）。`sandbox`（`enabled` *bool：省略=默认 true，false=opt-out / `tier`=`read-only`|`workspace-write`|`full-access` / `network_deny`）、`network`（`default`=`deny` fail-closed / `allow`/`deny` glob host 列表，deny 优先 / `allow_private` loopback+RFC1918+link-local）、`shell`（`max_output_bytes` 默认 1MiB / `idle_timeout` 默认 30m）。零值（无 security 块）经 `applyDefaults` 等同于显式粘贴示例（sandbox 开 + read-only + deny + 1MiB + 30m）。

## subagents

子代理运行时（Batch B1）：`limit`（1..20，默认 10）、`persistence_path`（默认 `~/.yanshi/subagents.v1.json`）。

## batch

C1 批处理与自动化：`rlm_model`（RLM fan-out 用的廉价 provider 名，须有 `cost_class: cheap`）、`rlm_max_concurrency`（≤16）、`automation_tick_seconds`（调度间隔，默认 60）。

## lsp

B2-LSP1 编辑后诊断回喂：`enabled`（*bool，省略=默认 true）、`diag_timeout`（单文件诊断等待，默认 800ms）、`languages`（覆盖语言→server 表）。

内置表覆盖 yanshi 能按扩展名识别的**全部**语言：`go`（gopls）、`python`（pyright-langserver）、`typescript` / `javascript`（typescript-language-server）、`rust`（rust-analyzer）、`cpp`（clangd）。列一个你没装的 server 没有代价 —— 命令不在 PATH 上时该语言被静默剔除（软降级）。

**两道启用闸门，都必须过**：命令在 PATH 上，**且**工作区里有该语言的标志文件（`go.mod` / `pyproject.toml` / `tsconfig.json` / `Cargo.toml` / `compile_commands.json` 等）。第二道是必要的：在没有 `go.mod` 的目录里 gopls 对每个请求都报错，于是你会得到一个永远失败的子进程，而不是一个诚实的"此处无诊断"。

`languages` 里的每一项**只覆盖 command/args**，标志文件沿用内置值 —— 把 yanshi 指向你自己编译的 gopls，不等于要求它停止判断"这里是不是一个 Go 工作区"。

## mcp

MCP server 连接（`servers` map）。每个 server 注册为 `mcp_<server>_<tool>` 工具，**默认拒绝**（须在 profile 的 `mcp.allow` 里显式放行）。`transport` 可为 `http`（streamable HTTP）或 `stdio`（子进程）。

## observability

C4 结构化日志（`log`：`level` 默认 info / `format` 默认 json）+ OpenTelemetry（`otel`：`enabled` 默认 false / `endpoint` / `service_name` 默认 yanshi / `sample_ratio`，启用且为 0 时默认 1.0 全量采样）。

## features

OBS3 特性注册表：`strict`（true 时未知 flag 名直接报错，防拼写）、`overrides`（operator 种子值，覆盖 registry 内置 default）。

## pricing

COST1 模型单价覆盖（USD per million tokens）：`overrides` 是 model 名→单价的 map，bootstrap 转成价格表注入；未知模型走 N/A。

## secrets

S10 凭据存储后端，**只存 RFC 8628 device-flow token**（provider api_key 不走这里）：`backend`=`auto`（默认，优先 OS keyring，失败降级到加密文件）|`keyring`|`file`|`none`；`file_path`（空=`os.UserConfigDir()/yanshi/secrets.enc`）；`passphrase_env`（主口令环境变量名；空且 backend=auto 时跳过 fileStore）。

## auth

O03 provider-neutral 认证：只配置 RFC 8628 device authorization（`device_auth_enabled` / `client_id` / `providers[]`，端点须 HTTPS-only）。provider 的 api_key 不在这里管，见 `llm.providers`。

## i18n

I18N1 国际化：`ui_locale`=`auto`（默认，每次启动按 LC_ALL > LANG 重算）|`en`|`zh-Hans`；`output_language` 独立控制模型回复语言（空=不注入 system-prompt 指令，跟随用户输入语言）。

## tui

C15 TUI 偏好：`keymap`（默认 default）、`theme`（默认 default）、`vim`（*bool）、`high_contrast`（*bool）、`bindings`（key:action 映射，方向固定）。

本块是**项目层**偏好，W8 起真正影响 TUI 运行时（`internal/cli/tui::newModelWithPrefs` 读它，`internal/cli/tui::NewProgram` 接收它）。

优先级自低而高：内置默认 < 本块 < `prefs.json`（用户层）< 环境变量。运行时用 `/keymap`、`/vim`、`/contrast`、`/locale` 改，改动写进用户层并落盘；`/theme` 仍是仅当前会话的配色切换。

`tui.bindings` 里写坏的键位不会让启动失败 —— TUI 正是你用来修它的东西。`yanshi doctor` 报告问题，`/keymap diagnostics` 在 TUI 里打印同一份诊断，`/keymap reset` 让内置默认盖过本块。

## 不在 config.yaml 里的开关（环境变量）

下面两项**没有配置字段**，只认环境变量。放在这里是因为它们都会改变进程或子进程的安全姿态，而操作员在 `config.yaml` 里找不到它们。

### `YANSHI_NO_HARDEN`

设为任意非空值时，启动时的进程加固**整体跳过**（关 core dump、拒绝调试器 attach、清掉 `LD_*`/`DYLD_*`）。跳过时 stderr 会打一行 `yanshi: process hardening skipped (…)`，成功时一个字都不打。

存在的理由很具体：macOS 上的 `PT_DENY_ATTACH` 会让二进制**无法调试**，且事后 attach 会**杀掉进程**，维护者跑 `dlv exec ./yanshi` 需要一个不是「改源码」的出口。

它**不是**安全漏洞：这里每一项挡的都是**别的进程**，一个能设置本进程环境的攻击者在进程启动前就已经赢了。日常使用不要设。

### `YANSHI_ALLOW_CHILD_ENV`

逗号分隔的**变量名**列表，这些名字可以越过凭据清洗，被 yanshi 启动的辅助程序继承。

yanshi 起的每个辅助程序（MCP server、language server、`gh`、`git`、skills 的 clone、截图与剪贴板后端、版本探测、`task_gate_run` 的 gate 命令）拿到的都是**剥掉凭据的**环境。剥是对的，但有几个变量子进程真的需要：

| 变量 | 谁需要它 |
|---|---|
| `NETRC` | `gopls` 拉私有模块（`GOPRIVATE` 不在清洗名单里，会自己留下） |
| `npm_config_*` / `NPM_CONFIG_USERCONFIG` | 经 npm prefix 安装的 `pyright` / `typescript-language-server`；`npm test` 这类 gate 命令 |
| `SSH_AUTH_SOCK` | 走 SSH 的 git 操作 |

```sh
export YANSHI_ALLOW_CHILD_ENV=NETRC,npm_config_registry,SSH_AUTH_SOCK
```

四条边界：

- ⚠️ **不要在这里点名 provider key 或云凭据**（`OPENAI_API_KEY`、`ANTHROPIC_API_KEY`、`AWS_SECRET_ACCESS_KEY` 之类）。上面那张表里的 `task_gate_run` 是这批子进程中**唯一一个命令串由模型撰写**的：模型写 gate 命令、命令的输出又作为证据回喂给模型，所以一条 `printenv OPENAI_API_KEY` 就能把 key 送进对话。「这个变量是我自己 export 的」不等于「我希望模型读到它」。这条**没有代码拦着**，而且拦不了：这个开关能放回来的每个名字，按定义都是清洗规则匹配到的名字（`NETRC`、`SSH_AUTH_SOCK`、`npm_config_*` 全都是），按「像凭据就拒绝」去拦等于把这个开关本身拒掉。
- **只按名字**，不接受前缀或模式 —— 「凡是看起来像 token 的都放行」正是攻击者提供的值会满足的谓词。
- **不放宽 `shell_run` / ACP agent 那条路**。那条是**不受信程序**的发射路径，它的 allowlist 属于 profile 的安全姿态；让一个进程级环境变量悄悄放宽它，等于给了操作员一个不用改安全配置就能关掉安全控制的开关。
- 这个变量**自己不会进子进程环境** —— 它列举的正是「这台机器上还留着哪些凭据」，没有子进程需要这份清单。

`gh` 的凭据是另一条路：`GH_TOKEN` / `GITHUB_TOKEN` / `GH_CONFIG_DIR` / `GH_HOST` / `GH_ENTERPRISE_TOKEN` / `GITHUB_ENTERPRISE_TOKEN` 已经在代码里按名字放行，GitHub Enterprise 不需要额外设置。

## 配置字段骨架（由 gendocs 生成）

下表由 `go run ./cmd/gendocs -config docs/user-guide/configuration.md` 从 `internal/config.Config` 反射生成；列出每个字段的 key（dotted 路径）与类型。说明列留空，语义见上方各块 prose。修改 Config struct 后重生成；不要手改本区块。CI 守门确保 struct 与骨架一一对应（[../adr/README.md](../adr/README.md) 见 governance）。

<!-- BEGIN GENERATED: config-skeleton -->
### schema_version

| key | type | 说明 |
|---|---|---|
| schema_version | int | |

### server

| key | type | 说明 |
|---|---|---|
| server.http_addr | string | |
| server.task_addr | string | |

### storage

| key | type | 说明 |
|---|---|---|
| storage.sqlite_path | string | |
| storage.wal_max_open_conns | int | |
| storage.busy_timeout_ms | int | |
| storage.wal_auto_checkpoint | int | |
| storage.retention_days | int | |
| storage.memory_auto_extract | bool | |
| storage.memory_quota | int | |

### token

| key | type | 说明 |
|---|---|---|
| token | string | |

### llm

| key | type | 说明 |
|---|---|---|
| llm.providers | []ProviderConfig | |
| llm.rate_limit.qpm | int | |
| llm.rate_limit.burst | int | |
| llm.sanitize_tool_schemas | string | |
| llm.preflight | *bool | |
| llm.stream_first_chunk_timeout | duration | |
| llm.stream_idle_timeout | duration | |
| llm.max_retries | int | |

### agents

| key | type | 说明 |
|---|---|---|
| agents | []AgentConfig | |

### profiles

| key | type | 说明 |
|---|---|---|
| profiles | map[string]PermissionProfile | |

### skills

| key | type | 说明 |
|---|---|---|
| skills.builtin_dir | string | |
| skills.user_dir | string | |
| skills.plugin_dir | string | |

### vcs

| key | type | 说明 |
|---|---|---|
| vcs.ignore | []string | |
| vcs.worktree_dir | string | |

### batch

| key | type | 说明 |
|---|---|---|
| batch.rlm_model | string | |
| batch.rlm_max_concurrency | int | |
| batch.automation_tick_seconds | int | |

### compaction

| key | type | 说明 |
|---|---|---|
| compaction.threshold | float | |
| compaction.keep_recent | int | |
| compaction.context_window | int | |
| compaction.model | string | |
| compaction.chunk_threshold | float | |
| compaction.cooldown_fraction | float | |
| compaction.cooldown_duration | string | |
| compaction.hard_force_fraction | float | |

### loop_guard

| key | type | 说明 |
|---|---|---|
| loop_guard.repetition_enabled | bool | |
| loop_guard.repetition_window | int | |
| loop_guard.repetition_warn_after | int | |
| loop_guard.repetition_stop_after | int | |
| loop_guard.max_tool_calls | int | |
| loop_guard.per_tool_calls | map[string]int | |
| loop_guard.turn_timeout | duration | |
| loop_guard.max_turn_tokens | int | |

### memory

| key | type | 说明 |
|---|---|---|
| memory.enabled | bool | |
| memory.user_path | string | |
| memory.project_path | string | |
| memory.max_size | int | |

### security

| key | type | 说明 |
|---|---|---|
| security.sandbox.enabled | *bool | |
| security.sandbox.tier | string | |
| security.sandbox.network_deny | bool | |
| security.network.default | string | |
| security.network.allow | []string | |
| security.network.deny | []string | |
| security.network.allow_private | bool | |
| security.network.inspect_https | bool | |
| security.network.methods | []NetworkMethodRule | |
| security.shell.max_output_bytes | int | |
| security.shell.idle_timeout | duration | |
| security.shell.max_concurrent | int | |
| security.shell.capture_profile | bool | |
| security.shell.profile_shell | string | |
| security.guardian_prompt_file | string | |

### subagents

| key | type | 说明 |
|---|---|---|
| subagents.limit | int | |
| subagents.persistence_path | string | |

### goal

| key | type | 说明 |
|---|---|---|
| goal.max_tokens | int | |
| goal.max_iterations | int | |

### lsp

| key | type | 说明 |
|---|---|---|
| lsp.enabled | *bool | |
| lsp.diag_timeout | string | |
| lsp.languages | map[string]LanguageServerSpec | |

### mcp

| key | type | 说明 |
|---|---|---|
| mcp.servers | map[string]*MCPServerConfig | |

### observability

| key | type | 说明 |
|---|---|---|
| observability.log.level | string | |
| observability.log.format | string | |
| observability.log.file | string | |
| observability.log.stderr_in_tui | bool | |
| observability.log.max_size_mb | int | |
| observability.log.max_backups | int | |
| observability.otel.enabled | bool | |
| observability.otel.endpoint | string | |
| observability.otel.service_name | string | |
| observability.otel.sample_ratio | float | |

### features

| key | type | 说明 |
|---|---|---|
| features.strict | bool | |
| features.overrides | map[string]bool | |

### pricing

| key | type | 说明 |
|---|---|---|
| pricing.overrides | map[string]ModelPricingOverride | |

### secrets

| key | type | 说明 |
|---|---|---|
| secrets.backend | string | |
| secrets.file_path | string | |
| secrets.passphrase_env | string | |

### auth

| key | type | 说明 |
|---|---|---|
| auth.device.client_id | string | |
| auth.device.device_auth_enabled | bool | |
| auth.device.providers | []DeviceProviderConfig | |

### i18n

| key | type | 说明 |
|---|---|---|
| i18n.ui_locale | string | |
| i18n.output_language | string | |

### tui

| key | type | 说明 |
|---|---|---|
| tui.vim | *bool | |
| tui.keymap | string | |
| tui.bindings | map[string]string | |
| tui.theme | string | |
| tui.high_contrast | *bool | |
| tui.frecency | *bool | |
<!-- END GENERATED: config-skeleton -->
