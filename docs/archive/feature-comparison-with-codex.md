# Yanshi 后续功能开发清单

> 分析日期：2026-07-18  
> 依据：当前仓库 `cmd/`、`internal/`、`skills/` 与本地快照 `reference/codex/`。  
> 本文件只记录尚需开发的功能，不包含已经完成或只需保持的能力。

状态范围：

- `部分`：已有基础，但主流程、覆盖范围或可靠性仍有明确缺口。
- `占位`：存在代码、类型或入口，但尚未形成可用主流程。
- `缺失`：当前仓库没有可用实现。

下表按 P0 到 P3 排序。同一功能只出现一次；依赖列引用本表 ID，`-` 表示没有表内前置依赖。

| ID | 领域 | 后续功能 | 状态 | 当前缺口 | Codex 参考 | 开发目标 | 优先级 | 依赖 | 验收标准 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| A12 | Agent 核心 | 结构化最终输出 | 缺失 | 没有 per-turn JSON Schema 输出约束 | `exec` 和 SDK 支持 output schema | 在公共 turn 层校验并返回结构化结果 | P0 | - | 可传入 JSON Schema；结果校验失败可重试且最终返回明确错误；text 模式不受影响 |
| S06 | 安全 | 结构化 Shell 策略 | 部分 | 目前依赖 pattern 和元字符拒绝，不能安全表达复杂命令 | execpolicy 解析命令并执行细粒度规则 | 用结构化命令解析替代纯字符串判断 | P0 | - | 能识别程序、参数、管道和重定向；规则结果可解释；绕过样例有回归测试 |
| S07 | 安全 | 持久审批规则 | 部分 | only exact-action session allow，没有 scope、期限和审计 | 命令前缀规则、approval preset 和升级路径 | 支持 once、session、prefix rule 和持久规则 | P0 | S06 | 规则含来源、scope、过期时间；每次命中可审计；用户可查看和撤销 |
| S08 | 安全 | OS 级 Sandbox | 缺失 | 没有系统级进程、文件或注册表隔离 | Seatbelt、Landlock/bwrap、Windows sandbox | 统一 `read-only`、`workspace-write`、`full-access` sandbox abstraction | P0 | - | Windows 至少有受限 token/job object；越界文件和进程操作被系统拒绝；Unix adapter 有统一接口与测试 |
| S09 | 安全 | 子进程网络隔离 | 缺失 | host Guard 不能阻止 Shell 子进程直接联网 | network proxy、sandbox 网络策略和网络审批 | 对 sandbox 内所有子进程实施默认拒绝、显式放行 | P0 | S08 | 未授权连接失败；host/port 规则生效；DNS 和重定向不能绕过；决策写入审计事件 |
| T06 | 工具 | 多文件 Patch | 缺失 | 没有 patch parser、dry-run、原子应用和 patch 级审批 | 原生 `apply_patch` runtime | 提供可靠的 add/update/delete/move 多文件编辑工具 | P0 | - | 支持四类文件操作；dry-run 可返回 diff；失败不留下半成品；成功变更进入 autoVCS tracking |
| T07 | 工具 | Shell runtime v2 | 部分 | `shell_run` 拒绝大多数复杂命令，难以覆盖真实构建流程 | shell/unified exec 可运行复杂命令 | 在策略和 sandbox 内安全执行真实脚本 | P0 | S06,S08 | 支持平台原生 shell；输出、退出码、duration 稳定；超时和取消能终止整个进程树 |
| T08 | 工具 | 持久 Shell 与 stdin | 缺失 | 每次调用是独立进程，没有 session id、PTY 或 stdin | `exec_command`、`write_stdin` 和 session manager | 建立跨平台持久进程会话 | P0 | S06,S08 | 长进程返回 session id；可继续读取和写 stdin；支持 yield、timeout、输出上限和显式关闭 |
| V12 | 会话与服务 | 无头 `yanshi exec` | 缺失 | `chat --no-tui` 只是逐行单 turn SSE REPL | `codex exec` 支持 stdin、JSONL、resume 和稳定退出码 | 提供 CI 和脚本可用的无头执行入口 | P0 | A12 | 支持 prompt/stdin；text 与 JSONL 输出；取消、超时和错误有稳定退出码；可恢复 session |
| V16 | 会话与服务 | 通用 MCP client | 缺失 | 无 server 配置、连接、tool/resource 聚合和状态管理 | stdio/HTTP MCP、OAuth、tools/resources/templates | 实现可运营的 MCP client v1 | P0 | S07 | YAML 配置 stdio 和 streamable HTTP；tools/list/call 与 resources list/read 可用；启动超时、断线重连和权限检查有测试 |
| A11 | Agent 核心 | 分层项目指令发现 | 部分 | 只读取仓库根 `AGENT.md`，回退 `CLAUDE.md` | 分层 `AGENTS.md` 按目录作用域合并 | 构建 cwd 到目标文件的分层指令链 | P1 | - | 支持父目录到子目录覆盖；不同目标文件得到正确指令；单项注入有硬大小上限 |
| A13 | Agent 核心 | 多模态用户输入 | 占位 | provider adapter 可编码 image part，但 CLI 和 WS 只有文本输入 | CLI/SDK 支持图片、文件 mention 和 IDE context | 统一 text、local image、file mention 输入 item | P1 | V14 | TUI、exec、API 均可传图片；校验大小和类型；不支持图片的 provider 返回可理解错误 |
| C07 | CLI/TUI | 消息排队模式 | 部分 | queue/single/batch handler 已存在但 `/queue-mode` 未注册 | turn 中 steer/queue 的完整交互 | 接通并稳定三种排队策略 | P1 | - | 命令可切换模式；顺序、批量合并、取消和断线恢复有测试；队列状态始终可见 |
| C13 | CLI/TUI | MCP 管理界面 | 占位 | `/mcp` 固定返回空 server list | `/mcp` 和 CLI 展示 server、tool、状态及登录 | 让用户在 TUI 查看和管理 MCP | P1 | V16 | 展示 server/tool/startup status；支持 enable/disable 和错误详情；状态与 client 实际连接一致 |
| C14 | CLI/TUI | 完整会话选择器 | 部分 | 只能 list/restore，缺少 fork、archive、delete 和筛选 | resume/fork/archive/delete picker，可按 cwd/all 过滤 | 统一全部会话生命周期操作 | P1 | V09,V10 | picker 支持 cwd、时间和状态筛选；操作后列表即时一致；危险删除需要确认 |
| T09 | 工具 | 后台终端任务 | 缺失 | 没有后台 job 列表、等待和停止 | `/ps`、`/stop` 管理 background terminal | 在持久 Shell 上提供后台任务生命周期 | P1 | T08 | 可 list/wait/stop；退出状态和尾部输出可查询；session 关闭时按策略回收 |
| T13 | 工具 | 图片查看 | 缺失 | 没有本地图片读取和像素检查工具 | `view_image` 支持 detail 和多环境 | 向模型提供受限、可验证的本地图片内容 | P1 | A13 | 支持常见格式和 detail；路径受 workspace 权限控制；损坏或超大图片安全失败 |
| T16 | 工具 | 结构化询问用户 | 缺失 | turn 中无法暂停并收集结构化选项 | `request_user_input` 支持问题和模式限制 | 复用双向传输实现 request/response 工具 | P1 | V14 | 支持 1-3 个问题、选项和自由输入；取消/超时可恢复 turn；无双向客户端时明确降级 |
| V09 | 会话与服务 | 会话 Fork | 缺失 | 无法复制历史并生成独立 session | CLI、TUI、app-server 支持 thread fork 和 ephemeral fork | 支持安全分支会话 | P1 | V14 | fork 生成新 ID；原历史不可变；消息、模型和 usage 边界正确；支持从指定 turn fork |
| V10 | 会话与服务 | Rename、Archive、Delete | 缺失 | 只有 clear，没有持久生命周期状态 | rename、archive、unarchive、delete | 完成会话元数据和状态管理 | P1 | V14 | rename 可检索；archive 默认不出现在活跃列表；delete 有确认并清理关联数据 |
| V13 | 会话与服务 | 结构化代码 Review | 部分 | analysis 没有 review 基线和 findings 契约 | `review` 命令与 `/review` 支持变更审查 | 提供专用、可自动消费的 review 工作流 | P1 | A12,T06,V12 | 支持 working tree、base ref、commit；输出 severity、file、line、finding；无问题时明确返回 |
| V14 | 会话与服务 | 公共 Agent API v1 | 部分 | HTTP/SSE/WS 使用内部 frame，缺少版本化资源模型 | app-server v2 的 thread/turn/item JSON-RPC 和 schema generation | 在现有 server 上稳定 thread/turn/item 契约 | P1 | V12 | start/resume/interrupt 和流式 item 可用；协议有版本与 JSON Schema；背压、未知字段和兼容测试完善 |
| V17 | 会话与服务 | 通用 Agent MCP server | 部分 | `vcs-mcp` 只暴露 autoVCS 五个工具 | Codex 可作为 MCP server 暴露 Agent 能力 | 保留 VCS server，并新增 Agent MCP server | P1 | V14,V16 | MCP client 可创建/恢复 thread、启动/取消 turn 并接收结果；与 Agent API 行为一致 |
| E06 | 扩展生态 | Hooks v1 | 缺失 | 没有 session、turn、tool 生命周期 hook | hooks crate、管理 UI 和工具生命周期贡献点 | 提供受控的 pre/post hook 执行器 | P1 | S07,V14 | 支持 session/turn/tool pre/post；有超时和失败策略；输入输出脱敏；hook 不能绕过权限 |
| G02 | 任务/VCS | Goal Token budget | 占位 | `SpentTokens` 不随 LLM 调用累计 | goal、usage limit 和 context budget 使用真实 usage | 建立统一 usage accounting 并驱动预算终止 | P1 | V14 | 每次模型调用累计 usage；父子 Agent 不重复计算；达到预算可靠停止并持久化原因 |
| G03 | 任务/VCS | 接通 T0-T4 难度路由 | 部分 | tierer 和 skills 存在，但显式 T0-T2 只打印后返回 | collaboration modes、skills、goal 选择工作流 | 让每个 tier 都进入真实执行路径 | P1 | V12 | T0-T4 均能产生实际结果；auto 与强制 tier 可测试；升级规则不会静默退出 |
| G04 | 任务/VCS | Goal 暂停与恢复 | 缺失 | CLI 退出即丢失 loop 状态 | 长任务 goal 可跨 turn continuation | 持久化完整 Goal Loop 状态 | P1 | G02,V14 | 保存 plan、iteration、verdict、budget；进程重启后从安全边界恢复；取消和失败状态可查询 |
| G05 | 任务/VCS | Plan mode 与计划工具 | 缺失 | planner 只用于独立 goal，普通 turn 没有 plan mode | `/plan` collaboration mode 与 plan tool | 会话支持只读规划和显式进入执行 | P1 | V14 | plan mode 禁止编辑类工具；计划可流式更新；用户确认后切换执行且历史连续 |
| M04 | 任务/VCS | 多 Agent 生命周期控制 | 部分 | 缺少 list、message、follow-up、wait、interrupt、resume、close；独立 spawn 实现未装配 | 完整多 Agent 工具集和线程切换 | 建立统一 Agent registry 和 typed events | P1 | V14 | 全部生命周期操作可用；线程树、深度、并发和 usage 可查询；取消不会泄漏任务 |
| M05 | 任务/VCS | Agent 角色与模型覆盖 | 部分 | 只能继承 profile/instruction，缺少可选 role/model override | agent roles、nickname、模型和 reasoning override | 为子 Agent 提供受策略限制的执行配置 | P1 | M04 | 可选择已允许角色和模型；越权配置拒绝；元数据在恢复和事件中保持一致 |
| E03 | 扩展生态 | Skill 管理 | 部分 | 只能发现和强制调用，无 list/install/enable/disable 管理 | `/skills`、skill creator/installer | 增加可诊断的 Skill 生命周期 | P2 | - | 可列出来源和冲突；支持安装、启停和校验；恶意路径与重复名称安全处理 |
| E05 | 扩展生态 | 可执行 Plugin runtime | 占位 | `plugin.Host` 仅内存注册，进程加载未实现，bootstrap 未接生命周期 | plugin、core-plugins 和 extension API | 建立版本化、隔离的插件运行时 | P2 | E06,S08 | manifest 有 schema/version；tools/skills/hooks 可贡献；插件崩溃不拖垮主进程；支持启停 |
| M06 | 任务/VCS | Agent 线程 UI | 部分 | 只能嵌套显示 usage/progress，不能切换线程 | `/agent`、`/subagents` 和 agent graph | 在 TUI 中浏览和控制 Agent 线程树 | P2 | M04 | 可切换 active thread；显示状态、父子关系和 usage；操作与 registry 实时一致 |
| M07 | 任务/VCS | 批量 Agent jobs | 部分 | broker 可分发任务，但没有批量输入和 job result 模型工具 | spawn_agents_on_csv、report_agent_job_result | 复用 broker 提供批量任务编排 | P2 | M04 | 可提交结构化批量任务；限制并发；逐项结果、重试和汇总状态可查询 |
| O02 | 运维平台 | Feature flags | 缺失 | 没有统一实验功能注册和 CLI 管理 | features list/enable/disable，带 stage 和 strict config | 为实验能力提供集中发布开关 | P2 | V14 | flag 有 stage/default/owner；CLI 可列出和切换；未知 flag 在 strict mode 报错 |
| O05 | 运维平台 | 遥测与指标 | 缺失 | 只有日志和局部 usage/retry event | OTel、analytics、Sentry 和 tool/turn metrics | 建立默认脱敏的可观测性基础 | P2 | V14 | session/turn/tool 有 trace id；记录 latency/token/retry/error；支持关闭和 OTel export |
| O07 | 运维平台 | `yanshi doctor` | 缺失 | 没有环境自检命令 | doctor 检查安装、配置、认证和 runtime | 一次性诊断核心依赖和常见配置错误 | P2 | S08,V16 | 检查 config、DB、provider、sandbox、MCP、ACP、端口和权限；支持人类与 JSON 输出 |
| O08 | 运维平台 | Debug 与模型目录 | 部分 | 只有 fake model、日志和 `/config`，没有统一 debug 命令 | debug models、prompt、config、trace 和 model manager | 提供机器可读、默认脱敏的诊断入口 | P2 | O05 | 可输出模型目录、有效配置来源、模型可见 prompt 和 trace 摘要；不打印 secret |
| T11 | 工具 | Web Search | 缺失 | 没有搜索、过滤和结构化引用 | hosted/standalone web search | 提供与 fetch 分权的结构化搜索工具 | P2 | S07,V16 | 支持 query、域名和时间过滤；返回标题、摘要、URL；重定向和访问均受网络策略约束 |
| T18 | 工具 | 动态工具发现 | 缺失 | 所有工具在 bootstrap 静态装配 | tool search、dynamic/extension tools、namespace 和 deferred exposure | 按需暴露 MCP/插件工具，控制 schema 上下文 | P2 | E05,V16 | 支持 namespace、搜索和延迟加载；冲突命名可诊断；工具数量不导致上下文无界增长 |
| V11 | 会话与服务 | Ephemeral 与 Side conversation | 缺失 | 没有临时分支和旁路对话 | ephemeral thread、`/side`、`/btw` | 在持久会话旁提供不污染主历史的临时分支 | P2 | V09 | side history 与主线隔离；可返回或丢弃；关闭后按策略清理且不影响原 session |
| V15 | 会话与服务 | TypeScript/Python SDK | 缺失 | 没有官方 client library | 官方 TS/Python SDK 支持 thread、stream、resume 和 schema output | 先交付 TypeScript，再提供 Python 对等接口 | P2 | A12,V14 | SDK 支持 start/resume/run/stream/cancel；类型由协议生成；有跨版本契约测试 |
| W07 | 任务/VCS | Git 专用体验 | 部分 | 只能经 Shell 调 Git，缺少产品级 base/diff/review 语义 | `/diff`、review base、apply cloud diff 和 Git repo 检查 | 增加 Git adapter，同时保留 autoVCS | P2 | V13 | 支持 status、base diff、untracked 和 commit selection；不修改用户 Git 配置；与 autoVCS 状态边界明确 |
| C15 | CLI/TUI | Keymap 与无障碍 | 缺失 | 快捷键和样式固定 | keymap、Vim、theme、title/statusline 配置 | 只实现高价值键位和可访问性配置 | P3 | O02 | 核心按键可重映射；支持 Vim 开关和高对比主题；冲突配置可诊断并恢复默认 |
| C16 | CLI/TUI | IDE 与文件 Mention | 缺失 | 没有 mention、selection 或 open files 输入 | `/mention`、`/ide` 注入编辑器上下文 | 把文件和 IDE 状态作为有界输入 item | P3 | A13,V14 | 文件 mention 可选择和预览；IDE selection/open files 有大小上限；路径权限始终生效 |
| E07 | 扩展生态 | Apps 与 Connectors | 占位 | Connector interface 存在但没有实现和装配 | apps/connectors 提供外部服务工具和 OAuth | 基于插件运行时接入外部服务 | P3 | E05 | 至少一个真实 connector 完成启动、认证、工具调用和关闭；故障隔离；权限可配置 |
| E08 | 扩展生态 | Plugin Marketplace | 缺失 | 没有安装源、签名、版本和更新 | marketplace add/list/remove/browse | 在稳定插件 API 上建立可信分发 | P3 | E05,S10 | 支持查询、安装、更新、移除；校验来源和签名；版本不兼容时拒绝并可回滚 |
| O03 | 运维平台 | 登录与账号 | 缺失 | 只有配置/env API key，没有账号生命周期 | API key、browser/device code、Bedrock 和 account/rate limits | 建立 provider-neutral auth manager | P3 | S10 | 支持 API key 和至少一种交互登录；status/logout 可用；错误不泄漏凭据；provider 可扩展 |
| O06 | 运维平台 | Feedback 与诊断包 | 缺失 | 没有脱敏日志打包和反馈通道 | `/feedback`、rollout trace 和错误上报 | 生成用户可审阅的最小诊断包 | P3 | O05,S10 | 包含版本、脱敏 trace 和错误上下文；用户发送前可查看；默认不含 prompt、源码和 secret |
| O09 | 运维平台 | Shell completion | 缺失 | 没有 completion 子命令 | 生成 bash、zsh、fish、PowerShell completion | 根据稳定 CLI schema 生成补全脚本 | P3 | O02 | 四种 shell 脚本可生成；子命令、flag 和枚举补全正确；有 smoke test |
| O10 | 运维平台 | 安全自动更新 | 缺失 | 只能手动构建或替换二进制 | update 与平台安装流程 | 提供可验证、可回滚的升级流程 | P3 | O07,S10 | 校验签名和版本；支持检查、升级、失败回滚；不覆盖用户配置和数据库 |
| O12 | 运维平台 | IDE Extension | 缺失 | 没有 IDE protocol 或 extension | VS Code/IDE context 与 app-server | 使用公共 API 和 SDK 构建最小 IDE 集成 | P3 | C16,V15 | 支持发起/取消 turn、流式输出、selection/open files 和 diff；断线可恢复 |
| O13 | 运维平台 | Desktop 与 Remote control | 缺失 | 只有本地 TUI/HTTP daemon | Desktop app、app-server daemon、proxy、remote control | 在强认证下提供受控远程会话 | P3 | O12,V06 | 本地和远程身份隔离；连接需显式授权；可查看/中断 turn；默认不对公网监听 |
| S10 | 安全 | 凭据与 Secret 保护 | 缺失 | API key 来自 YAML/env，没有 keyring 和统一脱敏 | keyring、secrets crate 和 auth manager | 建立 OS keyring、secret handle 和统一脱敏层 | P3 | - | secret 不写入日志、事件或 DB 明文；支持 keyring 读写/删除；无 keyring 时有安全降级 |
| T14 | 工具 | 图片生成 | 缺失 | 没有 image generation tool | image-generation extension/skill | 在多模态和插件边界内提供可选图片生成 | P3 | A13,T13 | 生成结果以文件 item 返回；输出路径受权限控制；provider 不可用时明确降级 |
| V06 | 会话与服务 | 远程服务认证 | 部分 | 只有 static Bearer token 和 loopback bypass | account auth、app-server client identity 和登录 API | 为多人和远程部署提供强身份与 token 生命周期 | P3 | O03,S10 | token 可签发、轮换和撤销；区分 client/user；远程请求默认认证；审计不含 secret |

