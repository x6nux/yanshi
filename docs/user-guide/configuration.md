# 配置参考

yanshi 从 `config.yaml` 加载配置（已被 gitignore；从被跟踪的 `config.example.yaml` 复制）。加载时先做 `${VAR}` 环境变量展开，再反序列化，最后 `applyDefaults` 补零值、`validate` 做范围校验。本页解释**每个顶层块的语义、默认值与块间关系**（解释"为什么"），字段级 key/type 速查见文末[生成骨架表](#配置字段骨架由-gendocs-生成)。

> 安全提醒：`llm.providers[].api_key` 若是明文字面量或 `${VAR}` 展开后的明文，会被 fail-closed 校验拒绝。用 `secret://service/account`、`env://VAR`，或显式 `auth.legacy_insecure=true` 接受明文。

## server

HTTP 服务（`http_addr`）与任务 broker（`task_addr`）监听地址。bare TUI / `serve` / `chat` 都从这里起 HTTP 服务；`task_addr` 是任务 broker 的独立监听。

## storage

SQLite 持久化（`sqlite_path`）。F1 的 WAL 相关项：`wal_max_open_conns` 放开读连接池上限（0/省略=4，1=旧行为串行）；`busy_timeout_ms` 跨进程锁冲突重试窗口（0/省略=5000）；`wal_auto_checkpoint` WAL 自动 checkpoint 页阈值（0/省略=1000，负数=禁用被动 checkpoint）。写仍由进程内 writeMu 串行，这些值只影响读并行度。用 `:memory:` 跑内存库（测试）。

## token

顶层 `token` 是 HTTP 服务的 bearer token。**loopback（127.0.0.1）免 token**——本地 TUI / `serve` 发现无需认证。非 loopback 部署必须改掉 `change-me`。

## llm

`providers` 是 provider 列表，每项有 `name`/`kind`（`openai`|`openai-responses`|`anthropic`）/`model`/`api_key`/`base_url`/`context_window`/`cost_class`/`multimodal`。`context_window` 是该模型的 token 窗口；compaction 按它而非全局值估算。`multimodal: true` 声明原生图像输入（Tier G）；当主模型非多模态时，bootstrap 自动选第一个 `multimodal==true` 的 provider 作为视觉辅助。`llm.providers` 为空时 `--fake-model` 自动接入确定性 fake model。

## agents

agent 声明列表（`name`/`type`=`local`|`external`|`remote`/`chain` provider 链/`profile` 权限 profile 名/`capabilities`）。`profile` 引用下方 `profiles` map 里的一个具名 profile。

## profiles

权限 profile 的 map（键名被 agent 的 `profile` 字段引用）。每个 profile 是一个 `guard.PermissionProfile`，含四维：`tools`（glob 白名单，空=拒绝一切，见 [../adr/0003-guard-fail-closed-empty-allow.md](../adr/0003-guard-fail-closed-empty-allow.md)）、`fs`（读/写路径 glob）、`shell`（`policy` + `patterns`）、`net`（`allow` + `hosts`）、`mcp`（允许的 `mcp_<server>_<tool>` 白名单，即便 `tools.allow` 含 `*` 也单独把关）。`security` 块的 sandbox/network/shell 是叠加在 profile 之上的**部署级**姿态。

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

B2-LSP1 编辑后诊断回喂：`enabled`（*bool，省略=默认 true）、`diag_timeout`（单文件诊断等待，默认 800ms）、`languages`（覆盖/扩展语言→server 表，空=内置 {go: gopls, python: pyright-langserver}）。找不到的 server 静默跳过（软降级）。

## mcp

MCP server 连接（`servers` map）。每个 server 注册为 `mcp_<server>_<tool>` 工具，**默认拒绝**（须在 profile 的 `mcp.allow` 里显式放行）。`transport` 可为 `http`（streamable HTTP）或 `stdio`（子进程）。

## observability

C4 结构化日志（`log`：`level` 默认 info / `format` 默认 json）+ OpenTelemetry（`otel`：`enabled` 默认 false / `endpoint` / `service_name` 默认 yanshi / `sample_ratio`，启用且为 0 时默认 1.0 全量采样）。

## features

OBS3 特性注册表：`strict`（true 时未知 flag 名直接报错，防拼写）、`overrides`（operator 种子值，覆盖 registry 内置 default）。

## pricing

COST1 模型单价覆盖（USD per million tokens）：`overrides` 是 model 名→单价的 map，bootstrap 转成价格表注入；未知模型走 N/A。

## secrets

S10 凭据存储后端：`backend`=`auto`（默认，优先 OS keyring，失败降级到加密文件）|`keyring`|`file`|`none`；`file_path`（空=`os.UserConfigDir()/yanshi/secrets.enc`）；`passphrase_env`（主口令环境变量名；空且 backend=auto 时跳过 fileStore）。

## auth

O03 provider-neutral 认证：`legacy_insecure`（默认 false，fail-closed 拒绝明文 key）；`device` 块配置 RFC 8628 device authorization（`device_auth_enabled` / `client_id` / `providers[]`，端点须 HTTPS-only）。

## i18n

I18N1 国际化：`ui_locale`=`auto`（默认，每次启动按 LC_ALL > LANG 重算）|`en`|`zh-Hans`；`output_language` 独立控制模型回复语言（空=不注入 system-prompt 指令，跟随用户输入语言）。

## tui

C15 TUI 偏好：`keymap`（默认 default）、`theme`（默认 default）、`vim`（*bool）、`high_contrast`（*bool）、`bindings`（key:action 映射，方向固定）。

⚠️ **本块目前只被 `yanshi doctor` 读来做校验，不影响 TUI 运行时**：`internal/cli/tui/model.go::newModel` 把主题与键位写死为 `default`，而 `internal/cli/tui/preferences.go` 的分层合并（`mergeTUIPrefs` 及其 `preferences.json` 读写）**生产调用点为零**。台账 `D3/C15` 记为 `partial` 就是这条接线断点。运行时唯一可改的是配色：TUI 里用 `/theme`（`/theme high-contrast` 即高对比度），仅当前会话有效、不落盘。此处以前写着「也可运行时 `/keymap`、`/vim`、`/contrast` 改」，那三个命令从未注册过。

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

### token

| key | type | 说明 |
|---|---|---|
| token | string | |

### llm

| key | type | 说明 |
|---|---|---|
| llm.providers | []ProviderConfig | |

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
| security.shell.max_output_bytes | int | |
| security.shell.idle_timeout | duration | |

### subagents

| key | type | 说明 |
|---|---|---|
| subagents.limit | int | |
| subagents.persistence_path | string | |

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
| auth.legacy_insecure | bool | |
| auth.auto_migrate | bool | |
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
<!-- END GENERATED: config-skeleton -->
