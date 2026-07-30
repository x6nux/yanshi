# Yanshi 全量功能路线图 — codex + deepseek-tui 对照

> **生成日期**：2026-07-21
> **方法**：对 `reference/codex`(OpenAI codex-rs)与 `reference/deepseek-tui`(Hmbown DeepSeek-TUI)做全量功能挖掘,逐项对照 yanshi 当前代码(非盲信 07-18 文档),去重、重排、补设计骨架。
> **参考来源**：`reference/codex/AGENTS.md` + `codex-rs/` crate 全集；`reference/deepseek-tui/README*.md` + `AGENTS.md` + `docs/{TOOL_SURFACE,SUBAGENTS,MODES,MEMORY,MCP}.md` + `crates/tui/src/` 文件清单。
> **前置文档**：`docs/feature-comparison-with-codex.md`(07-18,仅 codex,61 项)、`docs/synthesis-final.md`(07-20,7 源综合)。本文件是它们的**后继与超集**——新增 deepseek-tui 对照、重验代码现状、为每项补设计骨架。
> **上次代码扫描**：2026-07-21 · **Go** 1.26.4 · **模块** `github.com/x6nux/yanshi`

---

## 0. 阅读说明

### 0.1 字段定义

- **状态**：`缺失`(无实现) · `占位`(有类型/入口,无主流程) · `部分`(有基础,覆盖/可靠性有缺口) · `已完成`(对照后确认无需再做)
- **优先级**：`P0`(核心竞争力/解锁后续) · `P1`(关键增强) · `P2`(体验/自动化) · `P3`(平台/生态,长期)
- **ID**：保留旧 `feature-comparison-with-codex.md` 的 ID(S06/V16/G05…)以便对照;deepseek-tui 独有或既有文档遗漏的新项用 `NEW-*` 或领域前缀(`LSP`/`RLM`/`AU`/`UX`/`DT`/`GH`/`APS`…)。
- **依赖**：引用本表 ID,`-` 表示无表内前置。
- **预估**：`h`=小时,`d`=天,`w`=周;含设计+实现+测试,不含上游 PR review 往返。

### 0.2 每项条目格式(设计骨架 · 深度②)

```
### [ID] 功能名  (优先级 | 状态 | 参考来源)
- 缺口: 现状与差距
- 落点: 包/文件(新建 N / 修改 M)
- 设计: 核心类型·与现有架构接法·数据流
- 依赖: [ID…]
- 风险: 关键风险与兜底
- 验收: 可判定标准
- 预估: 工作量
```

### 0.3 与现有架构的接法约定(全文档通用)

新增能力统一遵循以下既有模式,设计章节不再重复展开,只写差异:

| 模式 | 约定 |
|---|---|
| 装配 | 唯一组合根 `internal/bootstrap/Build`,按 `config→store→vcs→model→tools→orchestrator→http→task` 顺序接入;非致命失败走软降级(参照 VCS) |
| 工具鉴权/scope | 通过 context 注入(`tools.WithProfile`/`WithVCS`/`WithSubAgentRunner`),**不进工具参数** |
| 安全 | guard 四维 fail-closed 空校验;新工具默认 `Tools.Allow` 空=拒绝,需显式放行 |
| 传输 | 新帧类型同步加 `proto/frame.go` + `ws.go` + `ssebackend.go`(WS 持有历史 / SSE 回放) |
| 子进程 | 全部受 sandbox + execpolicy + 网络策略约束(A1 落地后) |
| 测试 | Fake 优先(`einollm.FakeModel`/`FakeBackend`/`FakeAgent`),不引 mock 框架 |
| 软降级 | 外部依赖(语言服务器/MCP server/gh CLI)缺失时禁用该子系统,不阻塞启动 |

---

## 1. 执行摘要

### 1.1 缺口全景

审计后 **55 项**(B0 的 3 项已于 2026-07-21 实测完成,剩余 **52 项缺口**),按领域:

| 领域 | 数 | 最高优先级代表 |
|---|---|---|
| ~~前置技术债~~ ✅ | 0 | 3 项已实测完成(见 §2) |
| 安全执行 | 5 | OS sandbox |
| 任务与计划 | 4 | durable tasks |
| MCP 生态 | 3 | 通用 client |
| 子代理 | 3 | 完整生命周期 |
| **编辑反馈 ⭐** | **2** | **LSP 诊断回喂 / 逐轮回滚 UI**(codex 对照完全遗漏) |
| 开发者工具 | 6 | GitHub 工具集 |
| 批量与自动化 | 3 | rlm_query 批量 LLM |
| TUI 体验 | 8 | 全局 Ctrl+K |
| 会话与记忆 | 4 | 用户 memory 文件 |
| 可观测运维 | 5 | slog 结构化日志 |
| 平台生态 | 5 | 版本化 Agent API + app-server + SDK + IDE |
| 安全凭据 | 2 | secrets/keyring + auth manager |
| i18n/无障碍 | 2 | locale 系统 |

### 1.2 分批总览(B0 前置 + Tier A–D 共 13 批 = 14 批)

| 批 | 名称 | 项数 | 优先级 | 预估 | 解锁 |
|---|---|---|---|---|---|
| **B0** ✅ | ~~前置技术债~~ | 3 | — | 已结项 | 2026-07-21 实测三项已完成/不再适用(见 §2) |
| **A1** | 安全执行底座 | 5 | P0 | ~3w | shell 真实执行 / 后台 jobs / 网络工具可信 |
| **A2** | 任务与计划模型 | 4 | P0/P1 | ~2w | 验证门 / artifacts / automations |
| **A3** | MCP 生态 | 3 | P0/P1 | ~2w | 外部工具无限扩展 |
| **B1** | 子代理增强 | 3 | P1 | ~2w | 并行编排 / 角色化委派 |
| **B2** | 编辑反馈 ⭐ | 2 | P1 | ~2w | 编辑后自纠 / 一键回退 |
| **B3** | 开发者工具 | 6 | P1/P2 | ~2w | git/test/diagnostics/GitHub 一体 |
| **C1** | 批量与自动化 | 3 | P2 | ~2w | 批量分类 / 定时任务 |
| **C2** | TUI 体验 | 8 | P2 | ~2w | 可用性大幅提升 |
| **C3** | 会话与记忆 | 4 | P1/P2 | ~2w | 跨会话偏好 / 分支对话 |
| **C4** | 可观测运维 | 5 | P2 | ~2w | 线上诊断 / 成本可见 |
| **D1** | headless + API + app-server | 3 | P2/P3 | ~3w | CI / 外部客户端 |
| **D2** | SDK + IDE | 2 | P3 | ~3w | 生态接入 |
| **D3** | secrets + auth + i18n + keymap | 4 | P3 | ~2w | 安全凭据 / 多语言 |

**总预估**:约 **29 周串行**(单人全职,各批 design+implement+test 之和,B0 已完成不计入);2-3 路并行墙钟约 **12-16 周**。Tier A(A1-A3)约 7 周是最高杠杆投入。

### 1.3 推荐执行顺序

1. **B0 已结项**(2026-07-21 实测完成,见 §2)——直接从 Tier A 开始
2. **A1 → A2 → A3**(可 A1/A3 并行,A2 依赖 A1 的 shell)→ 打地基,~7 周
3. **B2(LSP + 回滚)尽早插入**——它是 deepseek 独有、用户体感最强、且独立无强依赖,~2 周,可在 A 系列并行
4. B1/B3 → C 系列 → D 系列(长期)

---

## 2. Batch B0 — 前置技术债 ✅ 已结项(2026-07-21 代码实测)

> 经 2026-07-21 **直接代码实测**(非引用 07-20 报告):三项均已完成或不再适用,**本批次无需施工**。根因:`synthesis-final.md`(07-20)基于更早快照,而这几天的 G02 budget、工具接口标准化、frame 清理已把它们清掉。

| 项 | roadmap 原说法 | 代码实测 | 判定 |
|---|---|---|---|
| **TD1** SpentTokens 死代码 | budget 控制完全失效 | `goalloop/usage.go` 完整 `UsageSink`+`addUsage`/`usageFromMeta`;`loop.go` `spent()`→sink、`overBudget()` 在循环顶 & plan 后双检查;planner/evaluators/tier 全接 Sink;`TestLoop_BudgetStopsOnAccumulatedUsage` 等测试守护 | ✅ 已完成 |
| **TD2** compactNow hack + 双压缩协调 | threshold=1.0/window=1 hack;双路径无冷却 | `compactNow`→`ctxcompact.ForceCompact`、`maybeAutoCompact`→`MaybeCompact`,hack 已消除。grep `cooldown/lastCompact` 无命中,但 threshold 已天然门控,重复压缩风险低 | 🟡 正式 API 已完成;冷却为边际加固 |
| **TD3** ws.go 超 1000 行 | ~1090 纯代码行 | 实测 1480 总行 / **857 纯代码行**;`api/http/` 已拆 7 文件(chat/server/session/structresult/taskapi/skillprefix/ws)。按"纯代码行 > 1000"标准**已不违规** | ✅ 不再适用 |

**两个可并入其他批次的小残余**(不单列批次):

- **ACPImplementer usage 不回流** —— `implementer.go` grep `Sink/Usage` 零命中,外部 CLI(codex/claudecode)的 token 未进 sink,goal loop budget 只算到 planner/evaluator。归 **B3/C4(ACP 可观测性)**。
- **mid-turn 压缩显式冷却** —— 缺 cooldown,但 threshold 门控基本够用。归 **ctxcompact 属性测试**(未来加固,非阻塞)。

---

## Tier A — 基础能力(P0)

> 这一层是后续所有"可信执行 / 任务编排 / 外部扩展"的地基。A1 是最硬的骨头(跨平台 sandbox),但解锁价值最高。

## 3. Batch A1 — 安全执行底座

### [S06] 结构化 Shell 策略 (execpolicy)  (P0 | 部分 | codex `execpolicy` / deepseek `execpolicy` crate)

- **缺口**:当前 `guard.checkShell` 只做 pattern + 元字符拒绝,无法安全表达"允许 `go test` 但禁止 `go test -run Evil`"这类细粒度规则
- **落点**:`internal/guard/`(改)+ 新 `internal/guard/execpolicy/`(命令解析器)
- **设计**:
  - 解析器把命令拆成 `(program, args, pipes, redirects)`,支持引号/转义;对管道每段独立判定
  - 规则 DSL:`{program: "go", allow_args: ["test", "build"], deny_flags: ["-tags=e2e_real"]}`,结果可解释(返回命中的规则 ID)
  - 与现有元字符拒绝叠加:execpolicy 通过后仍走元字符硬拦截(深度防御)
- **依赖**:-
- **风险**:shell 语法复杂(bash quoting/zsh)→明确只支持 POSIX sh 子集,复杂命令降级到"拒绝并提示顺序执行";绕过样例建回归集
- **验收**:能识别程序/参数/管道/重定向;规则结果可解释;已知绕过样例(IFS、$()、glob 注入)有回归测试
- **预估**:5-7d

### [S07] 持久审批规则  (P0 | 部分 | codex approval_policy / deepseek command_safety)

- **缺口**:交互式权限只有 exact-action 的 session allow,无 scope、期限、来源审计;`/mode auto [1-10]` 是粗粒度
- **落点**:`internal/guard/`(改)+ `internal/store/`(规则持久化)+ `internal/api/http/ws_perm.go`(规则命中/记录)
- **设计**:
  - 规则结构:`{action, scope: program/path, ttl: once|session|persistent, source: user|prefix-rule, created_at, expires_at}`
  - 审批升级路径:`once → session(prefix) → persistent rule`;持久规则可 `/permissions` 查看/撤销
  - 每次命中写审计事件(配合 C4 slog);过期自动失效
- **依赖**:[S06](execpolicy 提供结构化 action 才能做前缀匹配)
- **风险**:规则越权累积→默认 ttl=once,升级需显式;prefix 规则有绕过→必须经 execpolicy 校验
- **验收**:规则含来源/scope/过期;每次命中可审计;用户可查看撤销;前缀规则有绕过回归测试
- **预估**:3-4d

### [S08] OS 级 Sandbox  (P0 | 缺失 | codex linux-sandbox/Seatbelt / deepseek sandbox)

- **缺口**:无系统级进程/文件/注册表隔离,guard 只在 host 层检查,挡不住 shell 子进程直接越界
- **落点**:新 `internal/sandbox/`(跨平台抽象)+ `sandbox_windows.go`(job object/受限 token)+ `sandbox_unix.go`(landlock+seccomp;macOS seatbelt)
- **设计**:
  - 三档抽象:`ReadOnly` / `WorkspaceWrite` / `FullAccess`,工具/profile 声明所需档位
  - Windows:job object 限制 + 受限 token(至少阻止写工作区外);Unix:landlock(文件)+ seccomp(系统调用);macOS:`sandbox-exec`
  - bootstrap 探测能力,不支持→软降级到 host guard(打 warning)
- **依赖**:-
- **风险**:Windows 受限 token 复杂且易误杀→先做 job object 限文件写,受限 token 作增强;Unix landlock 需内核版本探测
- **验收**:Windows 至少 job object 拒绝越界文件写;Unix adapter 有统一接口+测试;越界操作被系统拒绝;不支持平台安全降级
- **预估**:1.5-2w

### [S09] 子进程网络隔离  (P0 | 缺失 | codex network-proxy / deepseek network_policy)

- **缺口**:host guard 的 net 白名单挡不住 shell/agent 子进程直接 `connect()`
- **落点**:`internal/sandbox/`(接 S08)+ 新 `internal/netpolicy/`(代理/放行)
- **设计**:
  - sandbox 内所有子进程默认 deny 出站;经本地 network proxy 代理,proxy 按 host/port 规则放行
  - DNS 解析后重定向同样校验(防 DNS 重定向绕过);决策写审计事件
  - 与 `tools/web.go` 的 host 白名单共用规则源
- **依赖**:[S08](sandbox 是网络隔离的执行点)
- **风险**:UDP/原始 socket 绕过→seccomp 收紧 socket 调用族;proxy 性能→仅代理出站
- **验收**:未授权连接失败;host/port 规则生效;DNS/重定向不能绕过;决策入审计
- **预估**:1w

### [T07/T08] Shell runtime v2 + 后台 `/jobs`  (P0 | 部分 | codex exec-server / deepseek exec_shell_*+task_shell_*+`/jobs`)

- **缺口**:`shell_run` 每次独立进程、拒绝大多数复杂命令;无 session id/PTY/stdin/后台任务
- **落点**:`internal/tools/shell.go`(改)+ 新 `internal/shell/`(session manager)
- **设计**:
  - 持久会话:`shell_start → {session_id, pid, PTY}`,后续 `shell_write_stdin`/`shell_read`/`shell_wait`/`shell_cancel` 按 id 操作
  - 后台任务:`task_shell_start` 立即返回,`task_shell_wait` 轮询增量输出;退出码/duration/尾部输出可查
  - 全部经 S06 execpolicy + S08 sandbox + S09 网络策略;超时/取消杀整个进程树(PDEATHSIG/JobObject)
  - TUI:`/jobs` 中心(list/wait/stdin/cancel);进程重启后 live 状态标 stale(参照 deepseek)
- **依赖**:[S06][S08][S09]
- **风险**:PTY 跨平台差异大→抽象 `Console` 接口,Windows 用 ConPTY;进程树清理泄漏→`createdWT` 式 map 必须回收
- **验收**:长进程返回 session id;可续读/stdin;yield/timeout/输出上限/显式关闭;进程树取消干净;session 关闭按策略回收
- **预估**:1.5-2w

---

## 4. Batch A2 — 任务与计划模型

> deepseek-tui 的 **durable task** 是其任务/计划/验证/PR/automation 全家的承载对象。引入它能让 goalloop、子代理、验证门共享一个持久工作单元。

### [DT1] Durable tasks (TaskManager)  (P0 | 缺失 | deepseek `task_*` 工具 + TaskManager)

- **缺口**:yanshi 有 `internal/task/broker`(基础分发),但无面向模型/用户的持久"工作单元"——没有 create/list/read/cancel、无 thread/turn 关联、无时间线/artifacts
- **落点**:新 `internal/task/work/`(durable task 模型 + Manager)+ `internal/tools/task.go`(工具)+ `internal/store/`(持久化)
- **设计**:
  - `WorkTask{id, title, status: Pending|Running|Completed|Failed|Cancelled, thread_id, turn linkage, timeline[], checklist[], gates[], artifacts[], created_at}`,schema 版本化(参照 deepseek `subagents.v1.json`)
  - 工具:`task_create`(入队)/`task_list`/`task_read`(详情含时间线)/`task_cancel`(approval-required)
  - Manager 与现有 `task/broker` 复用分发,补 result 模型;`createdWT` 泄漏随此清理(synthesis R13)
- **依赖**:-
- **风险**:与现有 broker 职责重叠→明确 broker=传输分发,work=工作单元语义;并发上限
- **验收**:可创建/列出/读取/取消;状态机正确;thread/turn 关联准确;重启后持久恢复
- **预估**:5-7d

### [G05] Plan mode + update_plan/checklist 工具  (P1 | 缺失 | deepseek `update_plan`/`checklist_*` + MODES.md / codex collaboration `plan`)

- **缺口**:planner 只服务于独立 goal,普通会话 turn 没有"只读规划"模式;无结构化计划/清单工具
- **落点**:`internal/guard/mode.go`(加 plan 模式门禁)+ `internal/tools/plan.go`(新)+ `internal/cli/tui/commands.go`(`/plan`)
- **设计**:
  - Plan 模式:guard 对编辑类工具(fs_edit/fs_patch/shell 写)返回 deny,只放行读类;切换执行需用户确认,历史连续
  - `update_plan`:结构化 checklist(rows: {text, status});`checklist_write/add/update/list` 细粒度操作;todo_* 作兼容别名
  - 计划/清单状态走 activity/sentinel 帧(WS),不污染 transcript;SSE 静默
- **依赖**:-
- **风险**:plan→execute 切换时工具集变化导致缓存失效→切换点显式 flush runner 缓存
- **验收**:plan 模式禁编辑类工具;计划可流式更新;确认后切执行且历史连续;checklist 状态持久
- **预估**:4-5d

### [DT2] 验证门 (task_gate_run + evidence)  (P1 | 缺失 | deepseek `task_gate_run`)

- **缺口**:无结构化的"验证命令 + 证据"承载;模型跑测试结果散落在 shell 输出
- **落点**:`internal/tools/task.go`(扩展)+ `internal/task/work/`(gate 结构)
- **设计**:
  - `task_gate_run{command, cwd}` 跑一条已批准的验证命令,产出 `Evidence{command, cwd, exit_code, duration, classification: pass|fail, summary, log_artifact_ref}`,挂到当前 WorkTask
  - 大输出→artifact(见 DT3),transcript 只留摘要
- **依赖**:[DT1][T07/T08](经 shell runtime 执行)
- **风险**:命令本身需 approval→复用 S07 审批;classification 误判→只标 exit_code,classification 由模型/规则简单推断
- **验收**:gate 证据结构完整;大输出成 artifact;挂到正确 task;退出码/duration 准确
- **预估**:3d

### [DT3] Artifacts (大输出→摘要)  (P1 | 缺失 | deepseek artifacts)

- **缺口**:大输出靠 spillover(溢写到 fs_read),无"artifact = 摘要 + 可回查原文"模型
- **落点**:`internal/task/work/`(Artifact 类型)+ `internal/store/`(原文存储)+ `internal/tools/`(工具结果装配)
- **设计**:
  - `Artifact{id, summary, content_ref(store path), size, created_at}`;工具结果含 `summary` + `artifact_ref`,模型默认只见 summary,需详查走 `artifact_read`
  - 与 spillover 协同:超阈值既写 spillover 又产 artifact 摘要
- **依赖**:[DT1]
- **风险**:artifact 无界增长→配额 + TTL 清理;store 路径权限→受 workspace 权限约束
- **验收**:大输出成 artifact+摘要;模型可见摘要;原文可按需回查;有配额清理
- **预估**:3d

---

## 5. Batch A3 — MCP 生态

### [V16] 通用 MCP Client  (P0 | 缺失 | codex codex-mcp / deepseek `crates/mcp`)

- **缺口**:yanshi 只有 VCS MCP **server**,无 client 接入外部 MCP server(`/mcp` 固定返回空)
- **落点**:新 `internal/mcp/client/`(连接管理)+ `internal/tools/mcp.go`(工具桥)+ `internal/config/`(server 配置)
- **设计**:
  - YAML 配置 stdio 与 streamable HTTP server;支持 OAuth;`tools/list`+`call` 与 `resources/list`+`read` + prompts
  - 连接管理器(`mcp_connection_manager` 式)统一 tools 增删与调用,命名空间 `mcp_<server>_<tool>` 避免冲突
  - 启动超时、断线重连、健康检查;权限:每个 server 工具默认 deny,profile 显式放行(fail-closed 一致)
- **依赖**:[S07](server 工具的审批)
- **风险**:server 数量无界→上下文膨胀(配合 T18 动态工具发现,延迟加载);stdio server 崩溃拖累→进程隔离 + 崩溃移除
- **验收**:stdio/HTTP server 可配可连;tools/resources 可用;启动超时/重连/权限检查有测试;命名冲突可诊断
- **预估**:1.5-2w

### [C13] `/mcp` 实化管理界面  (P1 | 占位 | deepseek `/mcp` + palette 分组)

- **缺口**:`/mcp` 占位返回空 server list,无状态/启停/错误详情
- **落点**:`internal/cli/tui/commands.go`(改 cmdMCP)+ `internal/mcp/client/`(状态源)
- **设计**:展示 resolved 配置路径、每 server 的 enabled/transport/command|URL/timeout/连接错误/发现到的 tools-resources-prompts;支持 enable/disable/validate/reload(配置即时写,model-visible 工具池重启生效)
- **依赖**:[V16]
- **风险**:状态与实际连接漂移→状态源唯一为 client 连接管理器,TUI 只读投影
- **验收**:展示 server/tool/status/error;enable/disable 生效;状态与 client 实际连接一致
- **预估**:3d

### [MCP1] MCP palette 发现  (P2 | 缺失 | deepseek TOOL_SURFACE "MCP manager and palette discovery")

- **缺口**:命令 palette 不含 MCP 工具,模型与用户难发现已接入 server 能力
- **落点**:`internal/cli/tui/commands.go`(`updatePalette` 扩展)+ `internal/mcp/client/`(枚举)
- **设计**:palette 按 server 分组列出 MCP 工具,用 runtime 名 `mcp_<server>_<tool>`;disabled/failed server 仍可见(标灰)
- **依赖**:[V16][C13]
- **风险**:工具数量多撑爆 palette→分组折叠 + 搜索过滤
- **验收**:palette 含 MCP 工具分组;disabled/failed 可见标灰;命名与模型可见一致
- **预估**:2d

---

## Tier B — 核心增强(P1)

## 6. Batch B1 — 子代理增强

> yanshi 已有 `agent_start`/`workflow_start`/`analysis`(DAG)。本批补齐 deepseek 式的**异步生命周期 + 角色化 + 持久化**,让并行编排可用。

### [M04] 完整生命周期 (wait/result/send_input/resume/assign/list)  (P1 | 部分 | deepseek `agent_*` 全集)

- **缺口**:只有同步 spawn,缺 list/message/follow-up/wait/interrupt/resume;无统一 registry 与 typed events
- **落点**:`internal/tools/agent.go`+`subagent.go`(扩展,注意先拆分见 A7)、`internal/agent/registry/`(统一 registry)
- **设计**:
  - `agent_spawn → agent_id`(立即返回,父继续);`agent_wait`/`agent_result`/`agent_send_input`/`agent_assign`/`agent_cancel`/`agent_resume`/`agent_list`
  - 统一 Agent registry(Arc<RwLock>),typed events 经现有 ToolChunkCallback 通道;线程树/深度/并发/usage 可查
  - 取消经 `CancellationToken` 链(子随父取消),不泄漏任务
- **依赖**:[DT1](可选用 durable task 承载长生命周期)
- **风险**:并发与取消竞态→registry 读写锁 + race 测试;跨重启 resume 的 task handle 丢失→标 Interrupted
- **验收**:全部生命周期操作可用;线程树/深度/并发/usage 可查;取消不泄漏;resume 跨重启可尝试
- **预估**:1w

### [M05] 子代理 7 角色  (P1 | 部分 | deepseek SUBAGENTS.md 角色分类)

- **缺口**:子代理只能继承 profile/instruction,缺 role/model override;无 explore/plan/review/implementer/verifier 等姿态
- **落点**:`internal/tools/predefined.go`(角色 prompt)+ `internal/tools/subagent.go`(`agent_type` 分派 + tool allowlist)
- **设计**:
  - 7 角色:`general/explore/plan/review/implementer/verifier/custom`,各带 system prompt 前缀与工具集姿态(读写/shell 权限矩阵见 SUBAGENTS 表)
  - `custom` 需显式 `allowed_tools`;角色 + 可选 model/reasoning override,越权配置拒绝;元数据在 resume/events 一致
- **依赖**:[M04]
- **风险**:角色 prompt 与 guard profile 冲突→角色只收紧不放宽;override 越权→校验落在 guard
- **验收**:7 角色可 选;权限矩阵符合;越权拒绝;别名大小写不敏感;未知值返回可接受集
- **预估**:4-5d

### [M04b] 持久化 + 并发上限 + 输出契约  (P1 | 部分 | deepseek `subagents.v1.json` + 输出 5 段式)

- **缺口**:子代理无跨重启持久化、无并发上限、无结构化输出契约
- **落点**:`internal/agent/registry/`(持久化)+ `internal/tools/subagent.go`(并发门 + 输出格式)
- **设计**:
  - 持久化 `~/.yanshi/subagents.v1.json`(schema 版本化);`session_boot_id` 区分当/历史会话,`agent_list` 默认当会话,`include_archived` 看全部
  - 并发上限默认 10(配 `[subagents].max_concurrent`,硬上限 20),只数 running;满则 spawn 返回 cap 错误
  - 输出契约 5 段:`SUMMARY/CHANGES/EVIDENCE/RISKS/BLOCKERS`,父读 EVIDENCE 作 working-set
- **依赖**:[M04]
- **风险**:持久化 schema 前向兼容→新字段 `serde(default)` 式(opcional);输出契约模型不遵守→prompt 强约束 + 宽松解析
- **验收**:重启后可 list/resume;并发上限生效;输出 5 段可解析;父可消费 EVIDENCE
- **预估**:4-5d

---

## 7. Batch B2 — 编辑反馈 ⭐(deepseek 独有,codex 对照完全遗漏)

> 这两项是 deepseek-tui 相对 codex 的差异化优势,用户体感最强,且**独立无强依赖**,建议尽早插入(可与 A 系列并行)。

### [LSP1] LSP 诊断回喂  (P1 | 缺失 | deepseek `core/engine/lsp_hooks.rs`)

- **缺口**:每次编辑后无内联编译/类型错误回喂,模型看不到自己引入的错误,只能靠重跑测试发现
- **落点**:新 `internal/lsp/`(Manager + 多语言适配)+ `internal/tools/diagnostics.go`(诊断工具)+ orchestrator 编辑后钩子
- **设计**:
  - Manager 按 workspace 启动 language server:`gopls`(Go)/`pyright`(Python)/`typescript-language-server`/`clangd`(C/C++)/`rust-analyzer`,按文件扩展/workspace 探测
  - 订阅 `textDocument/publishDiagnostics` → 聚合 `Diagnostic{file, line, severity, msg, source}`;编辑类工具(fs_edit/fs_patch)完成后触发 wait-for-diagnostics(带超时),结果作为 sentinel/tool-result 回喂模型
  - bootstrap 软降级:无 server 二进制→禁用该语言,不阻塞启动(沿用 VCS 模式)
  - WS 路径:诊断走 activity/sentinel,不进 transcript;SSE 路径静默跳过(无持久连接)
- **依赖**:-(独立;编辑类工具触发)
- **风险**:server 启动慢/卡死→启动超时 + 进程回收 + 单次诊断超时;多语言误探测→按已知标志文件(go.mod/package.json/Cargo.toml…)确认;文件量大→增量诊断只管编辑过的文件
- **验收**:编辑后模型收到诊断;server 缺失安全降级;超时不阻塞 turn;Go/Python/TS 至少一种端到端可用
- **预估**:1.5-2w

### [RB1] 逐轮回滚 UI (seam 快照 + `/restore-turn` + revert_turn)  (P1 | 缺失 | deepseek `seam_manager.rs` + `/restore`)

- **缺口**:yanshi 有 autoVCS(每次编辑自动追踪)和 VCS restore 工具,但**无一键"回退到某一 turn 之前"的 UI**;模型/用户无法快速撤销一整轮改动
- **落点**:`internal/vcs/`(每 turn 前后快照钩子)+ `internal/tools/vcs.go`(revert_turn)+ `internal/cli/tui/commands.go`(`/restore-turn`)
- **设计**:
  - 复用 autoVCS:在每个 turn 开始/结束打"seam"快照(轻量,只记 commit ref),不动用户自己的 `.git`(类 deepseek side-git)
  - `revert_turn <turn_id>`:把 main 工作副本回退到该 turn 之前的 seam;`/restore-turn` TUI 列出最近 N 个 seam 供选择
  - 回滚本身记为新 seam(可再回退);与 goalloop worktree 边界明确(只管 main 工作副本)
- **依赖**:-(autoVCS 已有)
- **风险**:快照膨胀→只存 ref 不存全量;并发编辑/回滚竞态→回滚走串行化路径;误回退生产改动→危险操作确认 + 回滚可再回退
- **验收**:每 turn 前后有快照;`/restore-turn` 可列出选择;revert_turn 正确回退;回滚可逆;不影响用户 .git
- **预估**:5-7d

---

## 8. Batch B3 — 开发者工具

> 把高频 shell 操作提升为结构化专用工具(参照 deepseek TOOL_SURFACE 立场:专用工具优先于 shell,结构化输出胜过自由文本)。

### [W07] git_status / git_diff 专用工具  (P2 | 部分 | deepseek `git_status`/`git_diff`)

- **缺口**:只能经 shell 调 git,无产品级 status/diff 结构化语义
- **落点**:`internal/tools/git.go`(新;与 vcs.go 区分:此为用户 .git 适配,vcs.go 是 autoVCS)
- **设计**:`git_status`→结构化 `{staged, unstaged, untracked}`;`git_diff`→working-tree 或 staged、按文件分块;只读,不修改用户 git 配置;与 autoVCS 状态边界明确
- **依赖**:-
- **风险**:大 diff 撑爆上下文→超阈值走 DT3 artifact;子模块/工作树边界→限定仓库根
- **验收**:status/diff 结构化;不修改用户配置;大 diff 成 artifact;边界清晰
- **预估**:2-3d

### [DT4] run_tests 工具  (P2 | 缺失 | deepseek `run_tests`)

- **缺口**:跑测试只能 shell `go test`,无结构化结果(pass/fail 计数、失败用例)
- **落点**:`internal/tools/testrun.go`(新)
- **设计**:按 workspace 探测构建系统(Go→`go test`、cargo→`cargo test`、npm→`npm test`),解析输出为 `{total, passed, failed, skipped, failures[]}`;可传 args;经 T07/T08 执行
- **依赖**:[T07/T08]
- **风险**:输出格式多样→每种构建系统一个解析器,未知格式降级原文+artifact;长测试→后台 task_shell
- **验收**:至少 Go 解析正确;结构化计数+失败列表;超时/取消干净;大输出成 artifact
- **预估**:3-4d

### [DT5] diagnostics 工具  (P2 | 缺失 | deepseek `diagnostics`)

- **缺口**:无一次调用取 workspace/git/sandbox/toolchain 概况
- **落点**:`internal/tools/diagnostics.go`(新;与 [LSP1] 共用 internal/lsp 诊断源)
- **设计**:一次返回 `{workspace_root, vcs_state, sandbox_mode, toolchains: {go, node, python… versions}, open_diagnostics_count}`;聚合 LSP1 的诊断计数
- **依赖**:[LSP1](诊断源)
- **风险**:探测慢→各子项独立超时 + 并发
- **验收**:一次调用聚合;各子项可独立失败不拖垮;toolchain 版本准确
- **预估**:2d

### [GH1] GitHub 工具集 (issue/PR context, comment, close)  (P2 | 缺失 | deepseek `github_*`)

- **缺口**:无 GitHub 集成;`pr <N>` 拉取预填 review 也没有
- **落点**:`internal/tools/github.go`(新);经 `gh` CLI(已认证)
- **设计**:
  - 只读:`github_issue_context`/`github_pr_context`(`gh issue/pr view`,可选 `gh pr diff --patch`);大 body/diff→artifact
  - 写(approval-required):`github_comment`/`github_close_issue`,后者要求非空验收标准+证据,拒绝脏工作树(除非显式允许);**绝不因 agent 停止就关 issue**
  - 入口:`yanshi pr <N>` 拉取 PR 预填 review 提示
- **依赖**:[DT3](大 body→artifact)、[S07](写操作审批)
- **风险**:gh 未装/未认证→软降级 + 明确错误;PR 注入(AGENTS.md 警告)→把 issue/PR 内容当**不可信输入**,不自动安装其提到的依赖/链接
- **验收**:只读 context 可用;写操作需审批且需证据;大 body 成 artifact;未认证明确降级;注入内容不被当指令执行
- **预估**:4-5d

### [T11] web_search 工具  (P2 | 缺失 | deepseek `web_search`/codex web-search)

- **缺口**:`tools/web.go` 只有 fetch,无搜索/过滤/结构化引用
- **落点**:`internal/tools/web.go`(扩展 search)
- **设计**:`web_search{query, domains[], since}`→`{title, snippet, url, ref_id}[]`;重定向/访问受 S09 网络策略约束;与 fetch 分权(已知 URL 用 fetch)
- **依赖**:[S07][S09]
- **风险**:搜索后端依赖→支持 DuckDuckGo(无 key)+ 可配 Bing/Tavily;结果质量→配额+超时
- **验收**:返回标题/摘要/URL;域名/时间过滤生效;重定向受策略约束;后端不可用降级
- **预估**:3d

### [V13] 结构化 code review  (P1 | 部分 | deepseek `/review` + `review` 角色 / codex review templates)

- **缺口**:`analysis` 工具无 review 基线和 findings 契约;无专用 `/review` 命令
- **落点**:`internal/tools/agent.go`(review predefined agent,补 findings 契约)+ `internal/cli/tui/commands.go`(`/review`)
- **设计**:支持 working tree / base ref / commit;输出 `Finding{severity, file, line, message, suggestion}[]`;无问题明确返回 clean;结合 M05 `review` 角色(只读、不打补丁)
- **依赖**:[M05][A12 output schema]
- **风险**:大 diff→artifact + 分块;severity 主观→给明确分级定义
- **验收**:支持三种 base;findings 结构化含 severity/file/line;clean 明确;只读不改
- **预估**:3-4d

---

## Tier C — 体验与自动化(P2)

## 9. Batch C1 — 批量与自动化

### [RLM1] rlm_query 批量并行 LLM  (P2 | 缺失 | deepseek `rlm_query`)

- **缺口**:无低成本批量分类/推理原语;要对 N 项分类只能 N 次 sub-agent(贵)或 N 次串行调用(慢)
- **落点**:`internal/tools/rlm.go`(新);用廉价 model(可配,如 flash/haiku)
- **设计**:一次性并发 1-16 个非流式 Chat Completions(`llm_query_batched` 式),每项 ~数百 token,秒级返回;cap=16/次;工具描述写明 cost-class 与 cap,引导模型在"1 个 sub-agent 够 vs 并行查询"间正确选择
- **依赖**:-
- **风险**:结果与输入顺序对应→带 index;廉价模型质量→限定分类/抽取类任务,prompt 明示
- **验收**:1-16 并发;顺序对应;cap 生效;成本显著低于 sub-agent
- **预估**:3-4d

### [AU1] Automations (计划任务)  (P2 | 缺失 | deepseek `automation_*`)

- **缺口**:无定时/触发式自动化任务
- **落点**:`internal/agent/automation/`(新;调度器)+ `internal/tools/automation.go`+ `internal/store/`(持久)
- **设计**:`automation_create{prompt, schedule(cron/interval), cwds[], status}`;`list/read/update/pause/resume/delete/run`;run 时入队一个 durable task(DT1);全部 approval-required
- **依赖**:[DT1]
- **风险**:调度器与 broker 协作→明确 broker 跑即时任务,automation 负责触发入队;多 cwd→逐个入队
- **验收**:可创建计划任务;按时触发入队;生命周期可控;持久化;approval 门禁
- **预估**:5-7d

### [M07] CSV 批量 agent jobs  (P2 | 部分 | codex comparison M07)

- **缺口**:broker 可分发但无批量结构化输入与 job result 模型
- **落点**:`internal/tools/agent.go`(批量入口)+ `internal/task/`(job result)
- **设计**:提交 CSV/结构化批量任务,限并发,逐项 spawn sub-agent,汇总 `report_agent_job_result`;复用 M04 registry 与并发上限
- **依赖**:[M04]
- **风险**:并发爆炸→共享 M04b cap;失败项重试策略
- **验收**:可提交批量任务;限并发;逐项结果+汇总可查
- **预估**:3-4d

---

## 10. Batch C2 — TUI 体验

> 现状:yanshi 已有**斜杠命令 palette**(`/`-prefix 自动补全)、`/think`(推理强度)、`/mode`、会话生命周期命令。本批补 deepseek 式的全局操作与输入体验。

### [UX1] 全局命令面板 Ctrl+K  (P2 | 部分 | deepseek `palette.rs`)

- **缺口**:现有 palette 仅 `/`-prefix 命令名补全,无全局动作面板(切换模式/模型/会话/工具等非命令动作)
- **落点**:`internal/cli/tui/commands.go`(扩展)+ `events.go`(Ctrl+K 绑定,bubbletea fork 已支持区分 Ctrl+Enter/Enter,键位可绑)
- **设计**:Ctrl+K 打开 fuzzy 动作面板(命令 + 模式切换 + 模型选择 + 会话跳转 + MCP 工具),统一 `updatePalette` 扩展为多源
- **依赖**:-
- **风险**:动作集膨胀→分组 + 模糊排序;键位与 Enter 发送冲突→Ctrl+K 独立不冲突
- **验收**:Ctrl+K 打开全局面板;fuzzy 过滤;覆盖命令/模式/模型/会话;Esc 关闭
- **预估**:3-4d

### [UX2] F1 可搜索帮助  (P2 | 缺失 | deepseek KEYBINDINGS)

- **缺口**:`/help` 仅列命令表,无键位/模式/可搜索帮助面板
- **落点**:`internal/cli/tui/view.go`(F1 面板)
- **设计**:F1 打开可搜索帮助(命令 + 键位 + 模式说明),输入过滤;复用 palette 渲染
- **依赖**:-
- **风险**:帮助文本与实际漂移→从 commandTable/键位表自动生成
- **验收**:F1 打开;可搜索;内容自动生成不漂移
- **预估**:2d

### [UX3] @path 文件附加上下文  (P2 | 缺失 | deepseek `commands/attachment.rs`)

- **缺口**:输入框无文件/目录附加;多模态/IDE context 也没有([A13] 占位)
- **落点**:`internal/cli/tui/entries.go`+`events.go`(@ 触发补全)+ `internal/tools/`(附加为有界 context item)
- **设计**:`@path` 触发文件补全(经 UX4 frecency 排序);选中后作为有界 user context item 注入(硬大小上限,超阈值→fs_read 提示);路径始终经 guard fs 校验
- **依赖**:[UX4]
- **风险**:大文件注入撑爆→硬上限 + 提示 fs_read;路径越权→guard 校验
- **验收**:`@` 触发补全;附加有界;越权拒绝;超大提示 fs_read
- **预估**:3d

### [UX4] 文件 frecency  (P2 | 缺失 | deepseek `file-frecency.jsonl`)

- **缺口**:文件补全无近期选择学习
- **落点**:`internal/cli/tui/`(frecency 存储)+ 补全排序
- **设计**:`~/.yanshi/file-frecency.jsonl`(freq+recency 衰减),影响 UX3 @path 与 UX1 面板排序
- **依赖**:-
- **风险**:隐私→文件名明文存本地(与 memory 一致,用户可控)
- **验收**:近期选择靠前;衰减合理;可禁用
- **预估**:1-2d

### [UX5] 草稿 stash `/stash`  (P2 | 缺失 | deepseek `composer_stash.rs`/`commands/stash.rs`)

- **缺口**:无草稿暂存;打断时输入丢失
- **落点**:`internal/cli/tui/`(stash 存储)+ `commands.go`(`/stash list/pop/drop`)
- **设计**:Ctrl+S 暂存当前草稿(`/stash list`/`/stash pop`);多条 stash;持久到 store
- **依赖**:-
- **风险**:与 queue 模式交互→明确 stash=暂停,queue=排队发送
- **验收**:可暂存/列出/恢复/删除;持久;不与 queue 冲突
- **预估**:2d

### [UX6] prompt 历史 Alt+R  (P2 | 缺失 | deepseek `composer_history.rs`)

- **缺口**:无输入历史搜索与草稿恢复
- **落点**:`internal/cli/tui/`(历史存储)+ 键位 Alt+R
- **设计**:记录已发送 prompt,Alt+R 模糊搜索+恢复为草稿;Alt+↑ 编辑最后一条排队消息
- **依赖**:-
- **风险**:历史膨胀→上限 + 去重
- **验收**:可搜索历史;恢复为草稿;上限生效
- **预估**:2d

### [UX7] 堆叠 toast 通知  (P2 | 缺失 | deepseek v0.8.10)

- **缺口**:状态提示互相覆盖
- **落点**:`internal/cli/tui/view.go`(toast 队列)
- **设计**:toast 排队叠放显示,自动过期;不互相覆盖
- **依赖**:-
- **风险**:遮挡内容→半透明/限时/最多 N 条
- **验收**:多条 toast 叠放;过期自动消失;不覆盖
- **预估**:1-2d

### [UX8] 思考流式展示  (P2 | 部分 | deepseek thinking 模式流式)

- **缺口**:reasoning_effort 后端已支持(low/med/high/off,`/think`),但**思考过程未单独流式可视化展示**;frame.go 注释提到 reasoning/reasoning_content 解析
- **落点**:`internal/proto/frame.go`(thinking chunk 帧)+ `internal/llm/eino/`(分离 reasoning_content 流)+ `internal/cli/tui/view.go`(思考区可视化)
- **设计**:把 provider 的 reasoning/thinking 块作为独立 chunk 流式转发(区别于正文),TUI 在折叠区/侧栏实时展示;不进 transcript 正文但可展开
- **依赖**:-
- **风险**:各 provider thinking 字段不一(openai reasoning_content / anthropic thinking blocks)→归一化;非思考模型无此块→静默
- **验收**:思考模型可见流式思考;正文与思考分离;非思考模型无影响;可折叠
- **预估**:3-4d

---

## 11. Batch C3 — 会话与记忆

### [MEM1] 用户 memory 文件  (P1 | 部分 | deepseek `docs/MEMORY.md` + `remember` 工具)

- **缺口**:yanshi 有 `instruct`(session 指令)和 `memory.go` 工具,但无"用户级持久偏好笔记注入 system prompt"的统一模型
- **落点**:`internal/instruct/`(扩展,加 user-level memory.md)+ `internal/tools/memory.go`(`remember` 写入)
- **设计**:`~/.yanshi/memory.md`(+ 项目级 `.yanshi/memory.md`),启动注入 system prompt(有界,超阈值→摘要);`remember` 工具追加;子代理继承;写入不进标准 approval 流(用户自己的笔记)
- **依赖**:-
- **风险**:注入膨胀→硬上限 + 与 ctxcompact 协同(pin 策略);与 memory.go 现有工具职责区分
- **验收**:偏好跨会话保持;remember 可写;注入有界;子代理继承
- **预估**:3d

### [V09] 会话 fork  (P1 | 缺失 | codex comparison V09)

- **缺口**:无法复制历史生成独立 session
- **落点**:`internal/store/`(fork 操作)+ `internal/cli/tui/commands.go`(`/fork`)
- **设计**:fork 从指定 turn 复制历史生成新 ID,原历史不可变;消息/模型/usage 边界正确
- **依赖**:-
- **风险**:大历史复制成本→引用计数/COW
- **验收**:fork 新 ID;原不可变;边界正确;可从指定 turn
- **预估**:2-3d

### [V11] ephemeral / side 对话  (P2 | 缺失 | codex comparison V11)

- **缺口**:无临时分支对话,主线历史易被污染
- **落点**:`internal/store/`(side thread)+ `internal/cli/tui/commands.go`(`/side`/`/btw`)
- **设计**:side history 与主线隔离,可返回/丢弃;关闭后按策略清理
- **依赖**:[V09]
- **风险**:线程模型复杂→明确 side 不持久(默认)
- **验收**:side 隔离;可返回/丢弃;清理不影响主 session
- **预估**:3d

### [E03] skill 从 GitHub 安装 + 管理  (P2 | 部分 | deepseek `/skill install github:`)

- **缺口**:`internal/skills/` 只本地发现+强制调用,无 list/install/enable/disable/校验
- **落点**:`internal/skills/`(生命周期管理)+ `internal/cli/tui/commands.go`(`/skills`)+ 安装器
- **设计**:`/skill install github:<owner>/<repo>` 拉取到 `~/.yanshi/skills/`;`/skills` 列出来源/冲突;enable/disable/validate/trust;恶意路径与重名安全处理;已装技能在会话上下文列出,匹配描述时模型可 `load_skill`
- **依赖**:-
- **风险**:安装第三方代码→trust 机制 + 路径校验 + 不自动执行;重名→来源前缀
- **验收**:可安装/列出/启停/校验;恶意路径安全;重名可诊断;模型可 load 匹配技能
- **预估**:4-5d

---

## 12. Batch C4 — 可观测运维

### [OBS1] slog 结构化日志  (P0/P2 | 缺失 | synthesis-final A12/R4)

- **缺口**:`fmt.Print` 散布,无线上结构化诊断
- **落点**:全仓库逐步替换 → `log/slog`
- **设计**:统一 logger(session/turn/tool 带 trace id);级别可配;默认脱敏(secret/prompt 不入日志);为 S07 审计、OBS2 OTel 提供基座
- **依赖**:-
- **风险**:迁移面广→分批替换,优先 guard/orchestrator/api;性能→采样
- **验收**:关键路径结构化日志;secret 不入日志;级别可配;采样不丢关键错误
- **预估**:3-5d

### [OBS2] OTel 遥测  (P2 | 缺失 | codex `otel` crate)

- **缺口**:无 trace/metrics,只有局部 usage/retry event
- **落点**:新 `internal/observe/`(OTel exporter)+ orchestrator/store/vcs 埋点
- **设计**:session/turn/tool 有 trace id 与 span;记录 latency/token/retry/error;支持 OTLP export 与关闭;默认脱敏
- **依赖**:[OBS1]
- **风险**:埋点性能→采样率可配;provider 不可用→软降级
- **验收**:trace 链可导出;latency/token/retry/error 可观测;可关闭;脱敏
- **预估**:5-7d

### [OBS3] feature flags  (P2 | 缺失 | codex comparison O02 / deepseek `features.rs`)

- **缺口**:无统一实验功能开关
- **落点**:新 `internal/features/`(flag 注册)+ CLI `/features`
- **设计**:flag 含 `stage(stable|beta|experimental)/default/owner`;CLI 可 list/enable/disable;strict mode 下未知 flag 报错
- **依赖**:-
- **风险**:flag 泛滥→stage 约束 + 文档
- **验收**:flag 注册/切换;strict mode 报错未知 flag;新功能可灰度
- **预估**:2-3d

### [COST1] $ 成本估算  (P2 | 缺失 | deepseek `pricing.rs`/`cost_status.rs`)

- **缺口**:`/cost`/`/stats` 只有 token 数,无 $ 估算
- **落点**:`internal/llm/eino/`(pricing 表)+ `internal/cli/tui/commands.go`(`/cost` 扩展)
- **设计**:内置各 model 的 input/output/cache 单价表(可配覆盖);按 usage 算 $;`/cost` 显示 token+$,`/stats` 显示历史会话 $ 聚合
- **依赖**:[TD1](真实 usage)
- **风险**:价格变动→表可配 + 注明时效;缓存命中价不同→区分
- **验收**:`/cost` 显示 $;聚合正确;价格可配;缓存价区分
- **预估**:2-3d

### [O07] doctor 增强  (P2 | 部分 | codex comparison O07 / deepseek `doctor`)

- **缺口**: `yanshi doctor` 基础已存在(Lane6 ✅),但未覆盖 sandbox/MCP/ACP/端口/权限全检
- **落点**:`cmd/yanshi/`(doctor 子命令扩展)
- **设计**:检查 config/DB/provider/sandbox/MCP/ACP/端口/权限/LSP server;支持人类 + JSON 输出
- **依赖**:[S08][V16][LSP1](检查项随其落地)
- **风险**:检查副作用→只读检查
- **验收**: 覆盖各子系统;JSON 可机读;失败明确指引
- **预估**:2d

---

## Tier D — 平台与生态(P3,长期)

## 13. Batch D1 — headless + 版本化 API + app-server

### [V12] headless exec 增强  (P2 | 部分 | codex `exec` / synthesis Lane1b)

- **缺口**:`yanshi chat --no-tui` 基础已有(逐行单 turn SSE REPL),但缺 JSONL 输出、resume、稳定退出码、stdin 批量
- **落点**:`cmd/yanshi/`(exec 模式)+ `internal/cli/`
- **设计**:支持 prompt/stdin/文件输入;text 与 JSONL 输出;取消/超时/错误稳定退出码;可 resume session;CI 友好
- **依赖**:[A12 output schema](已完成)
- **风险**:退出码契约稳定化→文档化 + 测试
- **验收**:stdin/JSONL 可用;退出码稳定;可 resume;CI 可脚本化
- **预估**:4-5d

### [V14] 版本化 Agent API v1  (P1 | 部分 | codex app-server v2 / deepseek app-server)

- **缺口**:HTTP/SSE/WS 用内部 frame,缺版本化资源模型(thread/turn/item)与 JSON Schema
- **落点**:`internal/api/`(v1 资源模型)+ `internal/proto/`(版本化)
- **设计**:`thread.start/resume/interrupt` + 流式 item;协议带版本 + JSON Schema;背压/未知字段/兼容测试;camelCase 线上命名
- **依赖**:[V12]
- **风险**:契约稳定→版本化 + 兼容测试矩阵
- **验收**:start/resume/interrupt + 流式 item 可用;有版本+Schema;兼容测试完善
- **预估**:1.5-2w

### [APS1] app-server (JSON-RPC)  (P3 | 缺失 | codex `app-server` v2 / deepseek `crates/app-server`)

- **缺口**: 无 JSON-RPC app-server(IDE/桌面/远程 daemon 接入)
- **落点**:新 `internal/appserver/`(JSON-RPC v2)+ `cmd/yanshi/`(app 子命令)
- **设计**:在 V14 资源模型上暴露 JSON-RPC(`<resource>/<method>`,资源单数);*Params/*Response/*Notification 命名;TS 类型生成;thread/turn/item + config 读写
- **依赖**:[V14]
- **风险**:双协议(HTTP+JSON-RPC)维护→共用 V14 资源模型
- **验收**:JSON-RPC thread/turn 可用;TS 类型可生成;与 HTTP 行为一致
- **预估**:1.5-2w

---

## 14. Batch D2 — SDK + IDE

### [V15] TS / Python SDK  (P3 | 缺失 | codex comparison V15)

- **缺口**:无官方 client library
- **落点**: 新 `sdk/{ts,python}/`
- **设计**:先 TS 后 Python;支持 start/resume/run/stream/cancel;类型由 V14 协议生成;跨版本契约测试
- **依赖**:[V14]
- **风险**:协议变动→生成驱动 + 契约测试
- **验收**:start/resume/run/stream/cancel 可用;类型生成;契约测试
- **预估**:2w

### [O12] IDE 扩展  (P3 | 缺失 | codex comparison O12)

- **缺口**: 无 IDE protocol/extension
- **落点**: 新 `ide/vscode/`(扩展)
- **设计**: 用 V14 公共 API + V15 SDK;发起/取消 turn、流式输出、selection/open files、diff;断线恢复
- **依赖**: [V14][V15][UX3]
- **风险**: IDE API 变动→最小依赖
- **验收**: turn 发起/取消/流式;selection/open files 注入;断线恢复
- **预估`:2w

---

## 15. Batch D3 — secrets + auth + i18n + keymap

### [S10] secrets / keyring  (P3 | 缺失 | codex comparison S10 / deepseek `crates/secrets`)

- **缺口**: API key 来自 YAML/env,无 OS keyring 与统一脱敏
- **落点**: 新 `internal/secrets/`(keyring handle + 脱敏层)
- **设计**: OS keyring 读/写/删;统一脱敏(secret 不入日志/事件/DB 明文);无 keyring→安全降级(加密文件 + passphrase 或 env)
- **依赖**: -
- **风险**: 跨平台 keyring 差异→adapter;降级安全→明确警告
- **验收**: secret 不入日志/DB 明文;keyring 读写删;无 keyring 安全降级
- **预估`:4-5d

### [O03] auth manager  (P3 | 缺失 | codex comparison O03)

- **缺口**: 只有 config/env API key,无账号生命周期
- **落点**: 新 `internal/auth/`(provider-neutral manager)
- **设计**: 支持 API key + 至少一种交互登录(browser/device code);status/logout;错误不泄漏凭据;provider 可扩展(Bedrock 等)
- **依赖**: [S10]
- **风险**: OAuth 流程→限定少数 provider
- **验收**: API key + 一种交互登录可用;status/logout;不泄漏凭据
- **预估`:1w

### [I18N1] locale / i18n  (P3 | 缺失 | deepseek `localization.rs`/`LOCALIZATION.md`)

- **缺口**: TUI 文案硬编码,无多语言
- **落点**: 新 `internal/i18n/`(locale + catalog)+ TUI 文案外提
- **设计**: 支持 en/zh-Hans(先两种,可扩 ja/pt-BR);UI 语言与模型输出语言独立;`/config locale` + 自动检测(LC_ALL/LANG);catalog 可贡献
- **依赖**: -
- **风险**: 外提面广→先覆盖命令名/状态/错误等高频文案;与现有中文文案一致
- **验收**: 至少 en/zh-Hans 切换;UI 与输出语言独立;自动检测
- **预估`:1w

### [C15] keymap 配置  (P3 | 缺失 | codex comparison C15 / deepseek ACCESSIBILITY)

- **缺口**: 快捷键固定
- **落点**: `internal/cli/tui/`(keymap 加载)+ 配置文件
- **设计**: 核心按键可重映射;Vim 开关;高对比主题(接现有 `/theme`);冲突可诊断+恢复默认
- **依赖**: [OBS3](flag 门控)
- **风险**: 键位冲突→校验 + 默认恢复
- **验收**: 核心按键可重映射;Vim 开关;高对比主题;冲突可诊断
- **预估`:4-5d

---

## 附录

### 附录 A:与旧 `feature-comparison-with-codex.md` 的 ID 映射

| 旧 ID | 本表 ID | 说明 |
|---|---|---|
| S06 | S06 | 结构化 Shell 策略 |
| S07 | S07 | 持久审批规则 |
| S08 | S08 | OS sandbox |
| S09 | S09 | 网络隔离 |
| S10 | S10 | secrets/keyring |
| T06 | (已完成) | 多文件 patch — `fs_patch.go` 已存在 |
| T07/T08 | T07/T08 | Shell runtime v2(合并) |
| T09 | T07/T08 | 后台 jobs 并入 shell runtime |
| T11 | T11 | web search |
| T13/T14 | (A13 多模态占位) | 图片查看/生成 — 暂未列入(低优先) |
| T16 | (UX 体系) | 结构化询问用户 — 可并入权限交互 |
| T18 | (V16 动态发现) | 动态工具发现 — 并入 MCP client |
| A11 | (已完成) | 分层指令 — `instruct` 包 |
| A12 | (已完成) | 结构化输出 — `outputschema.go` |
| A13 | A13 占位 | 多模态输入(本表未单列,并入 UX3) |
| C07 | (已完成) | 队列模式 — `queue.go` + `/queue-mode` |
| C13 | C13 | MCP 管理界面 |
| C14 | (已完成) | 会话选择器 — rename/archive/delete 已有 |
| C15/C16 | C15/UX3 | keymap / mention |
| V06 | (D 系列) | 远程认证 — 并入 O03 |
| V09/V10 | V09/(已完成) | fork / rename-archive-delete |
| V11 | V11 | side 对话 |
| V12 | V12 | headless exec(部分已完成) |
| V13 | V13 | 结构化 review |
| V14 | V14 | 版本化 API |
| V15 | V15 | SDK |
| V16 | V16 | MCP client |
| V17 | (APS1 并入) | Agent MCP server |
| E03/E05/E06/E07/E08 | E03/(插件体系) | skill/plugin/hooks/connectors/marketplace — 本表聚焦 E03,其余待插件 runtime(E05)落地后展开 |
| G02/G03/G04/G05 | TD1/G03/G04/G05 | goal budget/tier/resume/plan |
| M04-M07 | M04/M05/M04b/M07 | 多 Agent 体系 |
| O02-O13 | OBS3/OBS1/OBS2/O07/O09/O10/O12/O13 | 运维(本表选取高价值项) |
| W07 | W07 | Git 体验 |

**新增项(旧表无,来自 deepseek-tui)**:DT1/DT2/DT3/DT4/DT5/GH1/RLM1/AU1/MCP1/UX1-8/MEM1/LSP1/RB1/APS1/I18N1/COST1。

### 附录 B:依赖图(关键链)

```
B0(TD1/2/3) ✅已结项(依赖已满足) ──┐
              ├─→ A1(S06→S07→S08→S09→T07/08) ──→ DT2(gate) ─→ DT3(artifact)
              │                              └─→ DT4(run_tests), T11(web)
A2(DT1) ──→ DT2/DT3/AU1/M07
A3(V16→C13→MCP1)
B1(M04→M05→M04b) ─→ M07, V13
B2(LSP1, RB1)  [独立,可早做]
B3 依赖 A1/B1/LSP1/DT3
C 系列大多依赖 A/B
D 系列(V14→APS1/V15→O12)为长链
```

### 附录 C:每批工作量汇总

| 批 | 工作量 | 关键产出 |
|---|---|---|
| ~~B0~~ ✅ | 已完成 | 2026-07-21 实测三项已完成/不再适用 |
| A1 | ~3w | 可信执行底座 |
| A2 | ~2w | durable task + plan + gate + artifact |
| A3 | ~2w | MCP client 全家 |
| B1 | ~2w | 富子代理 |
| B2 | ~2w | LSP 回喂 + 逐轮回滚 ⭐ |
| B3 | ~2w | git/test/diag/GitHub/review |
| C1 | ~2w | 批量 LLM + automations |
| C2 | ~2w | TUI 可用性 |
| C3 | ~2w | memory + fork + skill 安装 |
| C4 | ~2w | slog + OTel + flags + $ + doctor |
| D1 | ~3w | headless + API + app-server |
| D2 | ~3w | SDK + IDE |
| D3 | ~2w | secrets + auth + i18n + keymap |
| **合计** | **~29w 串行 / 12-16w 并行** | 全量功能对齐 |

---

> **核心结论**:deepseek-tui 相对 codex 的真正增量价值集中在 **B2(LSP 诊断回喂 + 逐轮回滚 UI)** 与 **A2(durable task/plan/gate/artifact 全家)**——前者是 codex 对照完全遗漏的用户体感项,后者是任务/验证/自动化的承载基础。Tier A(~7 周)是最高杠杆投入;B2 因独立无依赖,建议与 A 系列并行尽早落地。
>
> 本计划为每项提供了设计骨架(落点/接口草案/架构接法/风险/验收),任一批次可直接进入 `writing-plans` 产出可施工任务。
