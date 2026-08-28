# yanshi × codex × QwenPaw 能力审计（2026-08-27）

一份文件两个用途：**第一部分**是 yanshi 未实现能力的优先级清单（决策用），**第二部分**是三方 322 条完整对照表（全景与证据）。第一部分的每条都能在第二部分找到出处。

## 版本与规模

| 项目 | 版本 | 规模 | 语言 |
|---|---|---|---|
| yanshi | `1c3760a` | 464 个非测试 `.go` | Go |
| codex | `4cb8d86`（2026-08-27） | 3390 个 `.rs`，104 个 crate | Rust |
| QwenPaw | `ec25aaee`（2026-08-27） | 2272 `.py` + 1226 `ts/tsx` | Python + TS |

## 图例

| 符号 | 含义 |
|---|---|
| ✅ | 有，且已接线到生产路径 |
| ⚠️ | 有但残缺：未接线 / 未在默认 profile / 平台缺失 / 有已知破损 |
| ❌ | 没有 |
| ➖ | 与该项目定位无关，不作为缺口 |

`建议` 列：**A**=建议做 / **B**=可选 / **C**=不建议（与「编码 agent + 自驱动目标循环 + 单二进制本地部署」定位冲突） / **F**=先修（已交付功能的破损，优先于任何新特性）

> 判据沿用 `2026-08-08-yanshi-vs-qwenpaw-matrix.md`：补上它，yanshi 作为「编码 agent + 自驱动目标循环 + 单二进制本地部署」是否变强。平台化/云端托管/企业管控一律 C。

## 怎么用这份文件

| 你想做什么 | 看哪里 |
|---|---|
| 决定接下来做什么 | **第一部分**，P0 → P1 第一梯队 → 其余 |
| 确认某个能力 yanshi 到底有没有 | **第二部分**对应域的表 |
| 确认某条结论可不可信 | 文末「可信度与方法」，标注了哪些是实测、哪些只是 grep、哪些未复核 |
| 避免砍掉自己的优势 | 文末「不要误伤的护城河」 |

**三级划分总览**（从 322 条里筛出 yanshi 侧为 ❌ 或 ⚠️ 的 165 条）：

| 级别 | 条数 | 含义 |
|---|---|---|
| **P0 必须实现** | 11 | 已交付功能的破损。**不做会持续造成损害**，且用户以为这些功能是好的 |
| **P1 建议实现** | 32 | 明确提升产品力，与定位一致，投入产出为正 |
| **P2 暂不实现** | 122 | 可选（91）+ 与定位冲突（28）+ 已判无需做（3） |

> **P0 与 P1 的区别不是重要性，是性质。** P0 是「已经写了、用户以为能用、实际坏的」；P1 是「本来就没有」。坏掉的东西比缺失的东西优先，因为它会主动误导人。

---

# 第一部分：yanshi 未实现能力（按优先级）

# P0 必须实现（11 条）

## P0-1 图片不计入 token 估算 ❌

**实测数字**：同样 1MiB 载荷，放 `Content` 计 **299602 tokens**，放 `UserInputMultiContent` 计 **8 tokens**（仅每消息固定开销）。

`internal/ctxcompact/tokens.go::estimateMessageTokens` 只累加 `Content`/`ReasoningContent`/`ToolCalls`，而 `internal/agent/orchestrator/multimodal.go` 把完整 base64 data URL 塞进 `UserInputMultiContent`。

**后果**：贴图会话里图片算 0 token → 压缩门永不触发 → 直接撞 provider 400。**落点**：`tokens.go` 加多模态分支（图片按 detail 估算，不能用 `len(data)/4`）。已有现成探针可转成回归测试。

## P0-2 工具输出不脱敏就发给 provider ❌

`RedactPatterns` 的消费端只有崩溃报告（`observe/log/crash.go`）、压缩（`ctxcompact/redact.go`）、入库（`store/store.go`）。**`internal/tools` 下零命中**。

**后果**：`cat .env`、`env`、`printenv` 的输出原样进 transcript，并随下一轮请求发给模型厂商。**这是本次刚提交的 S6 修复的另一半**——那次修的是审计表落库，工具输出→模型这条路没覆盖。**落点**：工具结果返回前统一过 Redactor（参照 codex `secrets/src/sanitizer.rs` 的作用位置）。

## P0-3 CJK 检索三条链全失效 ❌

**实测**（真驱动、真 FTS5）：

```
"截止日期" → 0 命中    "项目" → 0 命中    "周二" → 0 命中    "张伟" → 0 命中
"项目的截止日期是周二，负责人是张伟" → 1 命中     "deadline" → 1 命中
```

`internal/store/store.go` 的 `tokenize='porter unicode61'` 不切中文词，整句被当成一个 token。

**后果**：`history_search`、`SearchMemory`、`memory_autorecall` 三条链在中文会话下同时失效——**而 CLAUDE.md 规定本仓交互语言就是中文**。**落点**：加 CJK 查询检测 + 有界 LIKE 回退（QwenPaw `memoryspace.py` 的做法），或换 tokenizer。

## P0-4 会话恢复丢掉全部工具轮 ⚠️

`internal/api/http/ws_handlers.go` 恢复时只映射 `Role` + `Content`，且 role 只分 user/assistant（`if m.Role == "assistant"` else user）。store 明明存了 `ToolCallID`/`ToolName`/`ToolArgs`，恢复时全丢，**tool 消息还被错当成 user**。

**后果**：恢复会话后 ReAct 历史缺工具轮，模型看不见自己做过什么。**落点**：恢复循环补齐三个字段与 tool role 映射。

## P0-5 记忆蒸馏链零调用点 ⚠️

`internal/tools/memory_distill.go::DistillMemories` + `internal/store/memory_distill.go::ApplyDistillation` 整条链**无任何生产 caller**。

**后果**：memories 表只增不并，长期使用后召回质量下降。这是本仓 MEMORY 里「写了但零读者」教训的第九次复发。**落点**：接一个触发器（每 N 轮 / turn 后台 / 手动命令），或**明确删掉**这条死链。

## P0-6 流式空闲无看门狗 ❌

`internal/llm/eino/resilient.go::consumeStream` 只在 `Recv` 返错时动作。网关不断连也不发数据就**永久挂起**；`loopguard` 的 `DeadlineGate` 在迭代边界检查，进不去也就永不触发。

**后果**：无人值守的 goal loop 被一条僵死流吃掉整轮预算。**落点**：加首块 + 稳态双超时预算（参照 QwenPaw `stream_progress.py`：只算等上游的时间，空控制块不续命）。

## P0-7 `acp_delegate` 不在默认 profile ⚠️

已在组合根注册，但不在 `internal/bootstrap/profile.go` 的默认 allow list 中。

**后果**：WS 上每次调用弹窗、SSE 上永久 fail-closed。与 profile 注释里为 `agent_spawn` 等给出的「避免倒挂梯度」理由不一致，属同类遗漏。**落点**：profile allow list 加一行（是授权变更，需确认意图）。

## P0-8 沙箱挂载路径不展开 `~` / `$VAR` ❌

`internal/sandbox/` 无 expanduser 等价物（`guard/pathnorm.go` 有，但只服务破坏性删除判定，没接进沙箱挂载）。

**后果**：不展开就拼成 `<workspace>/~/.cache/uv`，路径不存在 → 后端静默丢挂载 → **授权写了、挂载没生效、日志干净**。又一个「写了但零读者」形状。**落点**：挂载计划构造处接 `pathnorm`。

## P0-9 `shell_run` 未绑 factory 时绕过 secproc ⚠️

`internal/tools/shell.go` 只在 context 绑了 `SecureProcessFactory` 时走 secproc，否则回落到同一函数后半段的直接 pipe 路径。

**后果**：不受信程序的强制入口被绕过。CLAUDE.md 已记录，收敛归 W6。**落点**：要么保证 factory 恒绑，要么回落路径也过 Authorize。

## P0-10 危险命令穿透仅一层 ⚠️

本次刚补的 `internal/guard/prefixrunner.go` 覆盖 18 种前缀执行器，但只穿透一层。codex 递归穿透 wrapper 上限 8 层。

**后果**：`sudo nohup rm -rf /` 这类嵌套形态仍可能漏判。**落点**：`stripCommandPrefix` 改成循环直到不动点（带层数上限）。

## P0-11 记忆 FTS 检索（同 P0-3）⚠️

与 P0-3 同根因，落点相同，一并修。

---

# P1 建议实现（32 条）

按投入产出排序。**前 5 条是投入最小、日常收益最高的**。

## 第一梯队：小改动，高频收益

| # | 能力 | 现状 | 落点与理由 |
|---|---|---|---|
| P1-1 | **Esc-Esc 回溯 fork** | ⚠️ | **服务端能力已有**（`commands_session_memory.go::cmdFork` 走 `proto.NewForkSession`），缺的只是零打字的交互路径。codex：两下 Esc + Enter，原 prompt 自动填回编辑框。「上一句问歪了」是最高频动作 |
| P1-2 | **模型可查剩余 token** | ❌ | 一个工具的量。yanshi 预算只在 `loopguard/budget.go`、`ctxcompact/budget.go` 内部，模型无从查询。**模型知道还剩多少就会自己收敛输出**——压缩是被动的，这是主动的 |
| P1-3 | **外部编辑器起草长 prompt** | ❌ | 全仓 `tea.Exec`/Suspend/EDITOR 零命中，只能在单行 textarea 里写。Bubble Tea 原生支持 `tea.Exec`。贴长规格时最痛的一处 |
| P1-4 | **diff 语法高亮 + 行号** | ❌ | 现在只有 LCS + 三色 sigil（`entries.go::renderDiff`），无行号无高亮；且 `fs_write` 新文件只显示 "wrote N lines"。**主界面是编码 agent，看 diff 就是核心动作** |
| P1-5 | **Ctrl+T transcript 全屏浮层** | ❌ | alt-screen 下终端原生选择失效，长回答回看/复制现在很难。codex 有独立 pager + 实时 tail + raw 复制模式 |

## 第二梯队：中等改动，结构性收益

| # | 能力 | 现状 | 落点与理由 |
|---|---|---|---|
| P1-6 | **生命周期 Hook 引擎** | ❌ | guard 只能答 allow/deny，**改不了参数、加不了上下文、挂不上用户自己的检查**。codex 的 `PreToolUseOutcome{should_block, block_reason, additional_contexts, updated_input}` 补的正是这个维度。**落点干净**：orchestrator 已有 loopguard middleware 链与 per-turn context 注入，hook 分发挂同一处，不动 guard 判决语义 |
| P1-7 | **MCP server 反向请求（readLoop）** | ❌ | `internal/mcp/stdio.go` 是严格 req→resp 无 readLoop，server 主动发的 elicitation / progress / listChanged **全部收不到**。现代 MCP server 在 yanshi 下会挂死或静默丢。**这条更像兼容性 bug，可考虑提到 P0** |
| P1-8 | **每 server 工具 allow/deny** | ❌ | `MCPServerConfig` 只能整 server 开关；一个塞 40 个工具的大 server 会撑爆 schema |
| P1-9 | **provider 自定义 HTTP 头** | ❌ | `ProviderConfig` 无任何 header 字段。**Azure、OpenRouter、企业网关、灰度路由全靠请求头**——缺了整类 provider 接不上，且与账号体系无关 |
| P1-10 | **数据驱动模型能力目录** | ❌ | 窗口、价格、多模态、推理档位散在三个 Go 表（`contextwindow.go`、`pricing.go`），加一个模型要改代码发版。codex `models.json` 一份表同时服务 `/model` 选择、压缩阈值与工具输出截断 |
| P1-11 | **按需加载工具 spec** | ❌ | 50 个注册工具全量进 schema，工具面还在长，每轮都烧 token。codex 用 BM25 检索 + defer_loading |
| P1-12 | **结构化 shell 解析** | ⚠️ | `lexShellLite` 只做词法切分，遇 `&&`/`;`/`\|` 整条 HardDeny。**正是 CLAUDE.md 里「请改为顺序执行多条命令」那条长期摩擦的解法** |
| P1-13 | **回合级聚合 diff** | ❌ | 只有单文件 `unifiedDiff` 与两 ref 比对。「本轮到底改了什么」是审阅与 goalloop evaluate 的基本输入 |
| P1-14 | **模型中途向用户提问** | ❌ | `elicit\|ask_user\|request_user_input` 全仓零命中。**WS 已有双向通道与权限询问机制，补一种帧即可**；长目标循环缺它只能靠猜 |
| P1-15 | **子代理 worktree 隔离** | ⚠️ | 子代理共用同一个 WorkRoot（`subagent.go` 无 cwd 参数），仅 goalloop 的 ACP 路径有 worktree。**并发子代理互相踩文件** |
| P1-16 | **规则增补（下次不再问）** | ❌ | 有会话级规则但不持久化成 execpolicy。高频摩擦 |
| P1-17 | **模型主动申请权限** | ⚠️ | 只有 `EscalateOnSandboxViolation`——**违规后被动**升一档、上限一次。主动申请比撞墙后猜省一整轮 |
| P1-18 | **真 PTY 交互式进程** | ❌ | `StartPTYProcess` 三平台一律返回 `ErrPTYUnavailable`。跑不了 REPL、`ssh`、带 TUI 的安装器、检测 isatty 才输出的测试。**shell v2 的 Start/Write/Read 骨架已在，缺的只是 console 实现** |
| P1-19 | **Ollama 深度集成** | ⚠️ | 只把 "ollama" 当窗口启发式字符串。**「单二进制本地部署」正是本地模型场景**，缺首启拉模型与可用性探测 |
| P1-20 | **命令式 token 鉴权** | ❌ | 只有静态 api_key。短期凭证（vault/SSO/内网签发）**会在长跑 goal loop 中途过期** |
| P1-21 | **跨会话历史检索** | ❌ | `SearchMessages` 强制非空 sessionID → 无法回答「上周那个 bug 怎么修的」 |
| P1-22 | **跨会话记忆自动生成** | ❌ | 只有模型主动 `memory_write`。**自驱动 goalloop 跑完不留任何长期资产** |
| P1-23 | **冷会话压缩归档 + 保留期** | ❌ | 无归档压缩无保留期，**单二进制本地部署下 `yanshi.db` 无界增长** |
| P1-24 | **token 预算式压缩兜底** | ❌ | `RunSummary` 失败即无兜底，summary 模型挂掉时 turn 直接撞窗。codex 的兜底是不调模型直接开新窗口 |
| P1-25 | **角色文件化** | ⚠️ | 角色是 `agentroles.go` 里硬编码的 7 个 RoleDef，**用户自定义专家角色只能改代码重编译** |
| P1-26 | **plugin manifest 一体化** | ❌ | `plugins.go` 的 plugin.json 只认 `skills/` 目录。**单二进制本地部署最需要「装一个包，工具+技能+钩子齐活」** |
| P1-27 | **防伪完成的续跑提示词** | ❌ | goalloop 的 judge 是评估器投票，没有针对「模型谎报完成」的提示词级防线。codex `continuation.md`：无进展判定、完成审计、阻塞需连续三轮 |
| P1-28 | **未强制约束逐字段告警** | ⚠️ | `CapabilityReport` 只有整体 Effective/Enforced/Reason，**只做到后端级没做到字段级** |

## 第三梯队：安全加固（重要但非紧急）

| # | 能力 | 现状 | 落点与理由 |
|---|---|---|---|
| P1-29 | **进程自身加固** | ❌ | 单二进制常驻且内存持有 provider API key，**同用户本地进程可直接 ptrace 读出**；一次 panic 的 core dump 同理。codex 在 pre-main 关 core dump、`PT_DENY_ATTACH`、清 `LD_*`/`DYLD_*` |
| P1-30 | **seccomp 网络系统调用过滤** | ❌ | **这是「managed proxy 可被 raw socket 绕过」那条已知弱点的解药**。codex 的 proxy 敢承认自己是 env-var 级，因为 seccomp 在下面兜底；yanshi 的 proxy 说同样的话时下面是空的 |

## 第四梯队：架构级（收益大，工作量最大，建议最后评估）

| # | 能力 | 现状 | 落点与理由 |
|---|---|---|---|
| P1-31 | **append-only rollout + 可重放投影** | ❌ | codex 所有 ResponseItem 原样落 JSONL，压缩只写一条 `Compacted` 标记，反向扫描重建窗口——**原文永不丢失、压缩可撤销可分叉**，且 chunk 超窗上界问题在它那侧不存在。yanshi 是「就地重写活动窗口」的有损单副本。**这是架构替换不是增量补丁** |
| P1-32 | **code-mode（JS 内编排工具）** | ❌ | 在 JS 沙箱里跑模型写的编排脚本，工具挂全局对象，无 fs 无网络。**一个「搜 50 文件、过滤、回 3 条」的动作从 50 轮工具往返压成 1 次 exec**；guard 六维可原样套在每次回调上；Go 侧用 `goja` 即可，不必引 V8 的 CGO 负担。**新范式，工作量大** |

---

# P2 暂不实现（122 条）

## P2-a 可选（91 条）

有价值但非必须，可在对应模块被动到时顺手做。完整列表见 `2026-08-27-three-way-capability-matrix.md` 中 `建议` 列为 **B** 的行。较值得留意的几条：

- **补丁 dry-run 成 diff 供审批**、**补丁模糊匹配分级放宽**、**Lark grammar 约束补丁**——降低大 patch 的失败率
- **审批缓存 + 命令规范化**——重试不重复弹窗，防同义命令绕过
- **网络访问逐域审批**、**HTTPS MITM 按方法规则**——现在 `net.allow` 放行域名后该域任意写操作全通
- **受保护元数据路径**——即使在可写根下也拒写 `.git`
- **终端能力探测与降级**——现在硬编码 `termenv.ANSI256`，`NO_COLOR`/`TERM=dumb` 全仓零命中
- **桌面通知（OSC9/BEL）**、**终端标题**、**OSC 8 超链接**、**`/diff` 命令**
- **持久化用户消息队列**——可向运行中或离线会话排消息
- **git 子进程全套硬化**、**内嵌 git 库**
- **web_search 后端可插拔**——现在 DuckDuckGo HTML 端点写死、正则刮 HTML，对方改版即碎
- **错误分类补 `content_safety` 与 404 failover**——404 现在判不可重试且不换 provider
- **代码签名/公证**、**一键安装脚本**、**自更新**
- **状态库损坏自愈**、**指标标签基数校验**、**供应链依赖冷却期**

## P2-b 明确不建议（28 条）

与「编码 agent + 自驱动目标循环 + 单二进制本地部署」定位冲突，**做了反而是负担**：

| 类别 | 条目 |
|---|---|
| 云端托管 | 云任务 / best-of-N、远程执行服务（Noise 加密通道）、云端配置包、守护进程自更新、隧道公网暴露 |
| 多端平台 | Web 控制台、Tauri 桌面端、Docker 镜像（部分）、Hub 多租户、mini-app 平台 |
| IM 与多模态 | 13+ IM 渠道、语音/SIP、实时语音会话、Creator 多模态创作、浏览器自动化、Computer Use、邮件触发 |
| 企业管控 | agent 身份签名（Ed25519）、workload identity、AWS Bedrock/SigV4、FedRAMP 头、安全风险评分快照 |
| 账号绑定 | ChatGPT 订阅登录、backend-client 用量积分 API |
| 其他 | 插件市场/远程源、模型自助装插件、嵌入/向量检索（yanshi 走 FTS 与单二进制一致）、声明式 YAML 循环编译、双击键和弦、终端图形协议、仓库 blob 体积门禁、commit 署名注入、身份/人格文件 |

## P2-c 已判无需做（3 条）

表中 `建议` 列为 `—` 但 yanshi 侧标 ⚠️/❌ 的行，多为「三方都弱」或「yanshi 有等价替代」。

---

---

# 第二部分：三方完整对照表（322 条 / 15 域）

每行一个能力，`Y`=yanshi `C`=codex `Q`=QwenPaw。`建议` 列沿用原始四档：**F**=先修（对应 P0）/ **A**=建议做（对应 P1）/ **B**=可选 / **C**=不建议（后两者对应 P2）。


## 1. Agent 核心与编排

| 能力 | Y | C | Q | 建议 | 说明 |
|---|---|---|---|---|---|
| ReAct 主循环 | ✅ | ✅ | ✅ | — | 三方都有 |
| 幻觉工具名回喂重试 | ✅ | ❓ | ❓ | — | yanshi `UnknownToolsHandler`，是 ADR-0001 的承重决策 |
| 每 turn context 注入 | ✅ | ✅ | ✅ | — | yanshi `withTurnContext` 三层注入 |
| 按 turn 切 model | ✅ | ✅ | ✅ | — | yanshi 以 model 指针为键缓存 runner |
| turn 中断续跑 | ✅ | ✅ | ❓ | — | yanshi `continuation.go` |
| 结果卫生（超长输出降级） | ✅ | ✅ | ✅ | — | 三方都有 |
| **生命周期 Hook 引擎** | ❌ | ✅ | ✅ | **A** | codex 12 事件（PreToolUse/PostToolUse/PermissionRequest/Pre-PostCompact/SessionStart-End/UserPromptSubmit/SubagentStart-Stop/Stop/Interrupt）；**PreToolUse 可改写 tool_input 并阻断**。yanshi 的 guard 只能答 allow/deny，改不了参数、加不了上下文 |
| Hook 输出溢出落盘 | ➖ | ✅ | ❓ | B | 超 ~2500 token 落盘留引用（依赖上一条） |
| **回合级聚合 diff** | ❌ | ✅ | ❓ | **A** | codex `turn_diff_tracker.rs` 累积本轮净 diff（100ms 超时降级）。yanshi 只有单文件 `unifiedDiff` 与两 ref 比对；「本轮改了什么」是审阅与 goalloop evaluate 的基本输入 |
| **模型中途向用户提问** | ❌ | ✅ | ❓ | **A** | codex `elicitation.rs` + `request_user_input`。WS 已有双向通道，补一种帧即可；长目标循环缺它只能靠猜 |
| 异步向用户发消息 | ❌ | ✅ | ✅ | B | 发出后立即返回，回复异步到达 |
| 任务类型分派 | ⚠️ | ✅ | ✅ | B | codex 分 regular/compact/review/user_shell 四类生命周期；yanshi 只有 turn 一种 |
| 独立代码评审子会话 | ⚠️ | ✅ | ❓ | B | yanshi 有 `review` 工具但非独立会话形态 |
| 声明式循环编译（YAML） | ❌ | ❌ | ✅ | C | QwenPaw 用 YAML 把多阶段循环编译成停止处理器链；yanshi 的 loopguard 是 Go 侧组装 |
| 循环护栏（重复/预算/时钟） | ✅ | ❓ | ✅ | — | yanshi 四闸，状态按 turn 存 context |
| 分层指令加载 | ✅ | ✅ | ✅ | — | yanshi 逐级读 CLAUDE.md/AGENTS.md |
| 身份/人格文件 | ⚠️ | ✅ | ✅ | C | QwenPaw 有 PROFILE/SOUL/CONTACTS 四语言；codex 有 personality 模板。与编码 agent 定位弱相关 |
| 特性开关注册表 | ✅ | ❓ | ❓ | — | yanshi `internal/features` |
| 实时语音会话 | ➖ | ✅ | ✅ | C | 与定位无关 |
| 安全风险评分快照 | ❌ | ✅ | ❓ | C | codex 存分类器分数且明确不进模型上下文 |

## 2. 工具系统

| 能力 | Y | C | Q | 建议 | 说明 |
|---|---|---|---|---|---|
| 文件读写编辑族 | ✅ | ✅ | ✅ | — | |
| 文件搜索（grep/glob） | ✅ | ✅ | ✅ | — | |
| AST 结构化搜索 | ✅ | ❓ | ✅ | — | yanshi `ast_search` 走 secproc 只读子进程 |
| LSP 代码导航 | ✅ | ❓ | ✅ | — | yanshi `lsp_definition/references/hover/symbols` |
| shell 单次执行 | ⚠️ | ✅ | ✅ | **F** | yanshi 只在 context 绑了 factory 时走 secproc，否则回落直连 pipe（CLAUDE.md 已记，收敛归 W6） |
| shell 持久会话 | ✅ | ✅ | ✅ | — | yanshi shell v2 九工具 |
| **真 PTY 交互式进程** | ❌ | ✅ | ⚠️ | **A** | yanshi `StartPTYProcess` 三平台一律返回 `ErrPTYUnavailable`；跑不了 REPL、`ssh`、带 TUI 的安装器、以及检测 isatty 才输出的测试。shell v2 的 Start/Write/Read 骨架已在，缺的只是 console 实现 |
| 后台进程续写与轮询 | ✅ | ✅ | ✅ | — | yanshi `task_shell_*` |
| 工具批处理 | ✅ | ✅ | ✅ | — | yanshi `tool_batch` 每步各自 Authorize |
| 并行工具调用 | ⚠️ | ✅ | ✅ | B | codex 同轮多工具并发+取消；yanshi 的 tool_batch 是显式派发而非模型自发并行 |
| 输出溢出落盘 + 取回 | ✅ | ✅ | ❓ | — | yanshi `spillover.go` + `artifact_read` |
| **工具输出脱敏后再进模型** | ❌ | ✅ | ✅ | **F** | codex `secrets/src/sanitizer.rs` 作用于命令输出。yanshi `RedactPatterns` 消费端只有崩溃报告/压缩/入库，**`internal/tools` 零命中** —— `cat .env` 原样进 transcript 并发给 provider |
| **按需加载工具 spec** | ❌ | ✅ | ❓ | **A** | codex `tool_search`（BM25）+ defer_loading。yanshi 50 个注册工具全量进 schema，工具面还在长，每轮都烧 token |
| 动态工具（运行时注入） | ❌ | ✅ | ✅ | B | 客户端注入 function/namespace 规格 |
| apply_patch 结构化补丁 | ✅ | ✅ | ➖ | — | 三方格式不同，yanshi 有 `fs_patch`/`fs_diff` |
| 补丁模糊匹配分级放宽 | ⚠️ | ✅ | ❓ | B | codex 精确→忽略尾空白→忽略首尾→Unicode 归一四级 |
| 补丁 dry-run 成 diff 供审批 | ⚠️ | ✅ | ❓ | B | codex 不写盘就算出结果给审批 UI |
| 补丁用 Lark grammar 约束 | ❌ | ✅ | ❌ | B | codex `ToolSpec::Freeform{syntax:"lark"}`；yanshi 把整个 patch 当 JSON string，大 patch 的转义是真实故障源 |
| 计划/待办工具 | ✅ | ✅ | ✅ | — | yanshi 9 个自管理工具 |
| 证据关卡（跑验收命令） | ✅ | ❌ | ❓ | — | yanshi `task_gate_run`，**codex/QwenPaw 都没有**，是 yanshi 护城河 |
| web 抓取 + 搜索 | ✅ | ✅ | ✅ | — | 见下条 |
| **web_search 后端可插拔** | ❌ | ✅ | ✅ | B | yanshi 把 DuckDuckGo HTML 端点写死在构造函数，靠正则刮 HTML，对方改版即碎；无内网/离线替代路径 |
| 视觉读图 | ✅ | ✅ | ✅ | — | yanshi `image_describe` 辅模型 |
| 截图 | ✅ | ➖ | ✅ | — | yanshi `screenshot`（审批门控） |
| 浏览器自动化 | ❌ | ❌ | ✅ | C | QwenPaw 有完整浏览器 SDK；与单二进制定位冲突 |
| 廉价模型批量查询 | ⚠️ | ❌ | ❌ | — | yanshi `rlm_query`（需配 batch.rlm_model），**独有** |
| 里程碑自标 | ✅ | ❌ | ❌ | — | yanshi `milestone_set`，**独有**，压缩时保留 |
| **模型可查剩余 token** | ❌ | ✅ | ✅ | **A** | codex `get_context_remaining`；模型知道还剩多少就会自己收敛，压缩是被动的、这是主动的 |
| 模型主动开新窗口 | ❌ | ✅ | ❓ | B | codex `new_context_window`：不摘要直接开新窗 |
| 危险命令递归穿透识别 | ⚠️ | ✅ | ✅ | **F** | codex 递归穿透 wrapper 上限 8 层；yanshi 已修 18 种前缀执行器（`prefixrunner.go`），但仅一层 |
| PowerShell 结构化解析 | ❌ | ✅ | ❌ | B | codex 用 tree-sitter 解析 PowerShell 判危险 |
| shell 快照与还原 | ❌ | ✅ | ❌ | B | 捕获 zsh/bash/sh/PowerShell 可还原状态 |
| **结构化 shell 解析** | ⚠️ | ✅ | ❓ | **A** | yanshi `lexShellLite` 只做词法切分，遇 `&&`/`;`/`\|` 整条 HardDeny；codex `parse_command.rs` 能逐段判定 —— 正是 CLAUDE.md 里「请改为顺序执行多条命令」那条摩擦的解法 |

## 3. 权限与审批

| 能力 | Y | C | Q | 建议 | 说明 |
|---|---|---|---|---|---|
| 多维权限检查 | ✅ | ✅ | ✅ | — | yanshi 六维顺序短路（destructive→mcp→tools→fs→shell→net），顺序承重 |
| 结构性 HardDeny 分档 | ✅ | ⚠️ | ⚠️ | — | yanshi 5 类不可越过 + Overridable 两档，**比 codex 的 approval 层更硬** |
| 破坏性删除分级 | ✅ | ✅ | ✅ | — | Catastrophic 全模式拦 / OutOfScope 弹窗 |
| 前缀执行器穿透（sudo 等） | ✅ | ✅ | ✅ | — | yanshi 本次刚补（18 种拼法） |
| 权限模式档位 | ✅ | ✅ | ✅ | — | yanshi default/allow-edits/yolo/auto/plan |
| **AI 判定自动批准** | ✅ | ✅ | ⚠️ | — | yanshi `auto` 模式 + codex Guardian，两边都是 fail-closed 退化弹窗。**yanshi 这块不落后** |
| Guardian 政策可定制模板 | ❌ | ✅ | ❌ | B | codex `policy.md` + node REPL 专项政策 |
| 人工推翻 AI 拒绝 | ❌ | ✅ | ❓ | B | codex `ApproveGuardianDeniedAction` |
| execpolicy 规则引擎 | ✅ | ✅ | ✅ | — | 三方都有 |
| **规则增补（下次不再问）** | ❌ | ✅ | ✅ | **A** | codex 批准时可把命令前缀写回策略文件；QwenPaw 有审批泛化（高危动词保留精确匹配）。yanshi 有会话级规则但不持久化成 execpolicy |
| 审批缓存（重试不重复弹窗） | ⚠️ | ✅ | ❓ | B | codex `with_cached_approval` + 命令规范化防同义绕过 |
| 脚本哈希绑定审批 | ✅ | ❓ | ❓ | — | yanshi 改内容需重批，**独有** |
| **模型主动申请权限** | ⚠️ | ✅ | ❓ | **A** | codex `request_permissions`（scope 可选本轮/整会话，net+fs 分维）。yanshi 只有 `EscalateOnSandboxViolation` —— **违规后被动**升一档、上限一次 |
| 网络访问逐域审批 | ❌ | ✅ | ✅ | B | codex 代理拦截 HTTP/HTTPS/SOCKS5 逐域询问并可保存 |
| 权限档案求交（父子会话） | ⚠️ | ✅ | ❓ | B | codex 无法安全求交时显式报错；yanshi 子代理走角色收窄 |
| 受保护元数据路径 | ⚠️ | ✅ | ❓ | B | codex 即使在可写根下也拒写 `.git`/`.agents`/`.codex` |
| 运行期工具名 fail-closed | ✅ | ❓ | ❓ | — | yanshi `internal/toolreg`，**独有**（补编译期 GOV5/GOV7 够不到的运行期缝） |
| 权限审计落库可查 | ✅ | ✅ | ✅ | — | |
| 审批超时降级 | ✅ | ✅ | ✅ | — | yanshi 超时 fail-closed；QwenPaw 300s 转沙箱隔离执行 |
| 四档执行级别 | ⚠️ | ✅ | ✅ | B | QwenPaw STRICT/SMART/AUTO/OFF |

## 4. 沙箱与进程隔离

| 能力 | Y | C | Q | 建议 | 说明 |
|---|---|---|---|---|---|
| macOS Seatbelt | ✅ | ✅ | ✅ | — | yanshi `sbpl.go` 已接线（**纠正**：曾疑未接线，实为 `sandbox_darwin.go` 调用） |
| Linux bubblewrap | ✅ | ✅ | ✅ | — | yanshi `bwrapargs.go` 已接线，真实内核 6.12.54 实测 4 条拦截 |
| Linux Landlock | ⚠️ | ✅ | ✅ | B | yanshi 有实现但**从未在真实内核执行过**（Docker linuxkit 无 `CONFIG_SECURITY_LANDLOCK`） |
| **seccomp 网络系统调用过滤** | ❌ | ✅ | ❌ | **A** | codex 拦 `connect/bind/listen/sendto/socket` 非 AF_UNIX，恒拦 `ptrace`/`process_vm_readv`/`io_uring`。**这是「managed proxy 可被 raw socket 绕过」那条已知弱点的解药** —— codex 的 proxy 敢承认自己是 env-var 级，因为 seccomp 在下面兜底 |
| Windows Job Object | ⚠️ | ➖ | ➖ | B | yanshi 有实现但未真跑（开发机 darwin） |
| **Windows 受限令牌 + ACL** | ❌ | ✅ | ✅ | B | codex `CreateRestrictedToken` + capability SID 打 ACL + 独立桌面 + WFP 网络过滤。**证明了绕开 AppContainer 也有更便宜的路** |
| Windows AppContainer | ❌ | ❌ | ✅ | C | yanshi 明确不做（`os/exec` 不暴露 STARTUPINFOEX），如实报 `DegradedHostGuard` |
| Windows deny-read + reparse | ❌ | ✅ | ❓ | B | 对敏感路径加 deny-read ACE 并覆盖 reparse 解析后的真实路径 |
| 内置 bwrap 二进制 + 摘要校验 | ❌ | ✅ | ❌ | B | codex 自带 bwrap，摘要校验失败退出码 8 |
| 沙箱拒绝启发式识别 | ✅ | ✅ | ❓ | — | yanshi `sandboxviolation.go` 识别因沙箱失败 |
| **未强制约束逐字段告警** | ⚠️ | ❓ | ✅ | **A** | QwenPaw `report_unenforced_config` 后端申报真正 enforce 的字段集、其余逐条 WARNING。yanshi `CapabilityReport` 只有整体 Effective/Enforced/Reason，**只做到后端级没做到字段级** |
| 沙箱挂载路径 `~`/`$VAR` 展开 | ❌ | ❓ | ✅ | **F** | 不展开就拼成 `<workspace>/~/.cache/uv`，路径不存在 → 后端静默丢挂载、授权变空转。yanshi `internal/sandbox` 无 expanduser 等价物（`guard/pathnorm.go` 有但只服务删除判定） |
| **进程自身加固** | ❌ | ✅ | ❓ | **A** | codex pre-main 关 core dump、`PT_DENY_ATTACH`/`PR_SET_DUMPABLE=0`、清 `LD_*`/`DYLD_*`。yanshi 单二进制常驻且内存持有 provider API key，同用户本地进程可直接 ptrace 读出 |
| 嵌套 exec 拦截式提权 | ❌ | ✅ | ❓ | B | codex `EXEC_WRAPPER` 劫持 execve 逐次裁决；yanshi 粒度是整个 `shell_run`（脚本第 7 行那个 sudo 只能整条重跑无沙箱） |
| API key 代理（子进程不见 key） | ❌ | ✅ | ❓ | B | codex `responses-api-proxy` stdin 收 key + mlock + 栈 zeroize |
| HTTPS MITM + 域内按方法规则 | ❌ | ✅ | ❓ | B | yanshi CONNECT 过主机名后即盲隧道，`net.allow` 放行域名后该域任意写操作全通 |
| 凭据环境剥离 | ✅ | ✅ | ✅ | — | yanshi `envscrub.go` |
| 路径监禁 | ✅ | ✅ | ✅ | — | yanshi `pathjail` 收敛到 work root |
| 远程执行服务（加密通道） | ❌ | ✅ | ❌ | C | codex Noise 握手（X25519+ML-KEM-768+AES-GCM） |

## 5. 上下文与压缩

| 能力 | Y | C | Q | 建议 | 说明 |
|---|---|---|---|---|---|
| 统一压缩核心 | ✅ | ✅ | ✅ | — | yanshi `ctxcompact.Run` 一条路径服务 pre/mid-turn 两个触发点 |
| 工具调用配对保护 | ✅ | ✅ | ❓ | — | yanshi fixpoint 保证 tool_call/result 不被切断 |
| 保留计划（pin 策略） | ✅ | ✅ | ✅ | — | yanshi pin 尾部/user 原文/working-set/错误标记 |
| 携带式分块摘要 | ✅ | ❓ | ❓ | — | yanshi 超窗按预算切块串行摘要 |
| 摘要质量门 | ✅ | ❓ | ✅ | — | yanshi 过短/纯元话语判拒，**独有** |
| 驱逐地图 | ✅ | ✅ | ✅ | — | 三方都有（yanshi `evictionmap.go` / codex fragment markers） |
| 里程碑保留 | ✅ | ❌ | ❌ | — | yanshi **独有** |
| 摘要脱敏 | ✅ | ✅ | ❓ | — | |
| 溢出反应式恢复 | ✅ | ✅ | ✅ | — | yanshi provider 报超窗后即时重压再试 |
| 窗口按 provider 取值 | ✅ | ✅ | ✅ | — | W4 修复后 mid-turn 也真了 |
| **图片计入 token 估算** | ❌ | ✅ | ✅ | **F** | 实测：1MiB 载荷放 `Content` 计 **299602 tokens**，放 `UserInputMultiContent` 计 **8 tokens**。`estimateMessageTokens` 完全不看多模态字段 → 贴图会话压缩门永不触发，直接撞 provider 400 |
| **append-only rollout + 可重放投影** | ❌ | ✅ | ⚠️ | **A** | codex 所有 ResponseItem 原样落 JSONL，压缩只写一条 `Compacted` 标记，反向扫描重建窗口 —— **原文永不丢失、压缩可撤销可分叉**。yanshi 是「就地重写活动窗口」的有损单副本，且 chunk 超窗上界问题在 codex 侧不存在 |
| **token 预算式压缩兜底** | ❌ | ✅ | ❓ | **A** | codex `compact_token_budget.rs`：不调模型直接开新窗口。yanshi `RunSummary` 失败即无兜底，summary 模型挂掉时 turn 直接撞窗 |
| 压缩生命周期 hook | ❌ | ✅ | ❓ | B | codex Pre/PostCompact，三条压缩路径共用 |
| 压缩模型回退 | ⚠️ | ✅ | ✅ | B | codex 对超窗/限额/过载换模型重试并记遥测 |
| 远程压缩（服务端） | ➖ | ✅ | ❌ | C | 绑定 OpenAI 后端 |
| 初始上下文重注入策略 | ⚠️ | ✅ | ❓ | B | codex 区分 mid-turn（插在最后一条 user 前）与 pre-turn（清空下轮重注） |
| 工具输出截断策略可配 | ⚠️ | ✅ | ✅ | B | codex TruncationPolicy 头尾保留 |
| 上下文片段可识别标记 | ⚠️ | ✅ | ❓ | B | codex 每条注入自带 kind + 起止标记，可定位/剥离/去重；yanshi 仅 SummarySentinel 一种 |
| 逐 turn 增量重渲染系统提示 | ❌ | ✅ | ❓ | B | codex `world_state` RFC 7386 merge patch 只发变化段；yanshi 构造期一次拼死，`/model` 切换等当前 turn 内模型看不见 |
| 手动 /compact | ✅ | ✅ | ✅ | — | |
| 按轮次回滚 | ✅ | ✅ | ✅ | — | yanshi `/restore-turn` |

## 6. 记忆与检索

| 能力 | Y | C | Q | 建议 | 说明 |
|---|---|---|---|---|---|
| 记忆写入（分维度） | ✅ | ✅ | ✅ | — | yanshi `WriteMemoryScoped` 带 project/agent 维度 |
| 记忆 FTS 检索 | ⚠️ | ✅ | ✅ | **F** | 见下条 CJK |
| **CJK 检索** | ❌ | ❓ | ✅ | **F** | 实测 yanshi（真驱动真 FTS5）：`截止日期`/`项目`/`周二`/`张伟` **全部 0 命中**，只有搜整串才中；英文正常。`tokenize='porter unicode61'` 不切中文词 → `history_search`、`SearchMemory`、`memory_autorecall` 三条链在中文会话同时失效，**而本仓交互语言就是中文** |
| 自动召回注入 | ✅ | ✅ | ✅ | — | yanshi `AutoRecall` ≤3 条/900 字，已在 WS turn 前接线 |
| 相关性判据（防误召回） | ✅ | ❓ | ❓ | — | yanshi Relevant/RequiredOverlap |
| Markdown 记忆文件 | ✅ | ✅ | ✅ | — | yanshi user/project memory.md |
| **记忆蒸馏/合并** | ⚠️ | ✅ | ✅ | **F** | yanshi `DistillMemories` + `ApplyDistillation` **整条链零生产调用点**，memories 表只增不并。codex 有 Phase1/Phase2 两阶段管线，QwenPaw 有 `/dream` |
| **跨会话记忆自动生成** | ❌ | ✅ | ✅ | **A** | codex 根会话启动时后台跑 Phase1 逐 rollout 抽取、Phase2 子代理整合，带租约认领/热度排序/未用剪裁/配额守卫。yanshi 只有模型主动 `memory_write` —— **自驱动 goalloop 跑完不留任何长期资产** |
| **跨会话历史检索** | ❌ | ✅ | ✅ | **A** | codex `rollout/src/search.rs` 用 ripgrep 扫全部 rollout（压缩文件也能搜）。yanshi `SearchMessages` 强制非空 sessionID → 无法回答「上周那个 bug 怎么修的」 |
| 记忆引用溯源 | ❌ | ✅ | ❓ | B | codex 解析 citation_entries 与 rollout_ids 建溯源 |
| 记忆读取遥测 | ❌ | ✅ | ❓ | C | codex 从 shell 命令识别读了哪份记忆并计数 |
| 一键清空记忆 | ⚠️ | ✅ | ✅ | B | |
| 输入历史落盘（全局） | ❌ | ✅ | ❓ | B | codex append-only `history.jsonl` 带文件锁与字节上限裁剪 |
| 嵌入/向量检索 | ❌ | ❓ | ✅ | C | QwenPaw 接 ReMe 框架；yanshi 走 FTS，与单二进制定位一致 |
| 主动对话触发 | ❌ | ❌ | ✅ | C | QwenPaw 空闲时主动生成消息 |

## 7. 会话与持久化

| 能力 | Y | C | Q | 建议 | 说明 |
|---|---|---|---|---|---|
| SQLite 存储层 | ✅ | ✅ | ⚠️ | — | yanshi 统一 Open/WriteTx/迁移/Redactor；QwenPaw 用 JSON 文件存会话 |
| 会话 CRUD + 归档 | ✅ | ✅ | ✅ | — | |
| 消息日志幂等追加 | ✅ | ✅ | ✅ | — | yanshi `AssignDedupKeys` |
| 会话分叉 | ✅ | ✅ | ✅ | — | yanshi 从任意 seq 复制 |
| turn 回退 | ✅ | ✅ | ✅ | — | yanshi 快照→截断→失败回滚三步 |
| **会话恢复保真度** | ⚠️ | ✅ | ❓ | **F** | yanshi `ws_handlers.go` 恢复时**只映射 Role+Content**，且 role 只分 user/assistant。store 明明存了 `ToolCallID`/`ToolName`/`ToolArgs` 却全丢，**tool 消息还被错当成 user** → 恢复后模型看不见自己做过什么 |
| **冷会话压缩归档** | ❌ | ✅ | ❓ | **A** | codex 后台 worker 压成 `.jsonl.zst`，读取侧透明解压 + 保留期。yanshi 无归档压缩无保留期，**单二进制本地部署下 `yanshi.db` 无界增长** |
| 尾部反向扫描重建 | ❌ | ✅ | ❓ | B | codex 从文件末尾向前读，无需全量加载 |
| 会话列表分页（游标） | ⚠️ | ✅ | ✅ | B | |
| 单写者锁（多进程） | ✅ | ✅ | ❓ | — | yanshi lockfile 选举 + PID 存活回收 + `SweepStale` |
| 用量日志与聚合 | ✅ | ✅ | ✅ | — | yanshi 逐次记 token/成本可出账单 |
| 任务表全生命周期 | ✅ | ✅ | ✅ | — | yanshi claim/heartbeat/requeue/finalize |
| 图片内容寻址存储 | ✅ | ✅ | ❓ | — | |
| **检查点 / 快照系统** | ⚠️ | ⚠️ | ✅ | B | QwenPaw 用 Git 对象存快照树 + dry-run + 事务性恢复（恢复期暂停写者）+ 恢复前自动快照。yanshi 有 autoVCS 但无「会话+记忆+文件」三选一的选择性恢复 |
| 备份与还原（签名） | ❌ | ❌ | ✅ | C | QwenPaw HMAC 签名 + 信任模式 |
| 线程分组/项目 | ❌ | ✅ | ✅ | C | |
| 状态库损坏自愈 | ❌ | ✅ | ❓ | B | codex 检测损坏则备份重建 |
| 历史回填任务 | ❌ | ✅ | ❓ | C | codex 分批把旧 rollout 元数据扫进 SQLite |

## 8. 模型运行时与 provider

| 能力 | Y | C | Q | 建议 | 说明 |
|---|---|---|---|---|---|
| 多 provider 适配 | ✅ | ✅ | ✅ | — | yanshi openai/anthropic/openai-responses；QwenPaw 21+ |
| 推理档位（thinking/effort） | ✅ | ✅ | ✅ | — | |
| 失败回退链 | ✅ | ✅ | ✅ | — | 见下条 failover 语义 |
| 流式重试/续流 | ✅ | ✅ | ✅ | — | |
| Retry-After 遵从 | ✅ | ✅ | ✅ | — | 本次刚修（openai 路径此前一直是死的） |
| 限流令牌桶 | ✅ | ✅ | ✅ | — | yanshi 全局 + per-model |
| 错误分类 | ⚠️ | ✅ | ✅ | B | yanshi 5 类；QwenPaw 8 类含 `content_safety`；**404 在 yanshi 判不可重试且不换 provider** |
| 用量与成本计价 | ✅ | ✅ | ✅ | — | |
| 结构化输出 / schema 净化 | ✅ | ✅ | ❓ | — | |
| provider 怪癖表 | ✅ | ❓ | ❓ | — | yanshi `quirks.go` 记录各家差异 |
| 模型发现 | ⚠️ | ✅ | ✅ | B | yanshi 启动一次性不落盘；codex 带 ETag + TTL 磁盘缓存 + 三档刷新策略（离线可用） |
| **provider 自定义 HTTP 头** | ❌ | ✅ | ❓ | **A** | yanshi `ProviderConfig` **无任何 header 字段**。Azure、OpenRouter、企业网关、灰度路由全靠请求头 —— 缺了整类 provider 接不上，且与账号体系无关 |
| **命令式 token 鉴权** | ❌ | ✅ | ✅ | **A** | codex `auth.command` + refresh_interval + 401 后重跑。yanshi 只有静态 api_key，**短期凭证（vault/SSO）会在长跑 goal loop 中途过期** |
| **流式空闲超时看门狗** | ❌ | ✅ | ✅ | **F** | yanshi `consumeStream` 只在 `Recv` 返错时动作，**网关不断连也不发数据就永久挂起**；`DeadlineGate` 在迭代边界检查、进不去也就永不触发 → 无人值守 goal loop 被一条僵死流吃掉整轮预算 |
| **数据驱动模型能力目录** | ❌ | ✅ | ✅ | **A** | codex `models.json`（context_window/推理档位/输入模态/truncation/auto_compact 阈值/priority）+ 磁盘缓存。yanshi 散在三个 Go 表（`contextwindow.go` 模式表、`pricing.go` 价格表），加一个模型要改代码发版 |
| per-provider 重试次数 | ❌ | ✅ | ❓ | B | yanshi 只有全局 MaxRetries |
| 配额窗口头解析 | ❌ | ✅ | ❓ | B | codex 解析 `x-*-primary-used-percent`/window-minutes/reset-at → 撞限前降速 |
| **Ollama 深度集成** | ⚠️ | ✅ | ✅ | **A** | yanshi 只把 "ollama" 当窗口启发式字符串。codex 有探活双端点、`/api/tags` 列模型、`/api/pull` NDJSON 流式拉取带进度。**「单二进制本地部署」正是本地模型场景** |
| LM Studio 集成 | ❌ | ✅ | ✅ | B | 探活、列模型、load_model 预热 |
| 内嵌 llama.cpp 运行时 | ❌ | ❌ | ✅ | C | QwenPaw 自动分配端口与上下文长度 |
| 多模态能力探针（实测） | ⚠️ | ❓ | ✅ | B | QwenPaw 实测图像/视频支持并标记来源是文档还是实测 |
| Provider OAuth 登录 | ⚠️ | ✅ | ✅ | C | yanshi 有 device flow；codex 的 ChatGPT 订阅登录绑定 OpenAI 账号体系 |
| AWS Bedrock / SigV4 | ❌ | ✅ | ❌ | C | |
| FakeModel（无 key 测试） | ✅ | ❓ | ❓ | — | yanshi 确定性 fake，**测试基础设施护城河** |

## 9. MCP 与扩展

| 能力 | Y | C | Q | 建议 | 说明 |
|---|---|---|---|---|---|
| MCP client（stdio） | ✅ | ✅ | ✅ | — | |
| MCP client（HTTP/SSE） | ✅ | ✅ | ✅ | — | |
| 健康检查与重连 | ✅ | ✅ | ❓ | — | yanshi 探活失败自动重启 |
| MCP OAuth（PKCE） | ✅ | ✅ | ✅ | — | yanshi 完整授权码流 + 令牌进 keyring |
| MCP 工具 fail-closed opt-in | ✅ | ⚠️ | ⚠️ | — | yanshi mcp 维度排在 tools 之前是**刻意**的（宽泛 `Tools.Allow` 不得静默授权新 MCP server） |
| **MCP server 反向请求** | ❌ | ✅ | ❓ | **A** | yanshi `stdio.go` 是严格 req→resp **无 readLoop**，server 主动发的 elicitation / progress / listChanged **全部收不到** → 现代 MCP server（要用户确认、报长任务进度）在 yanshi 下会挂死或静默丢 |
| **每 server 工具 allow/deny** | ❌ | ✅ | ✅ | **A** | yanshi `MCPServerConfig` 只能整 server 开关；一个塞 40 个工具的大 server 会撑爆 schema |
| MCP 工具目录缓存 | ❌ | ✅ | ❓ | B | codex 按连接身份+配置指纹 LRU 缓存 schema |
| MCP 资源读取 | ⚠️ | ✅ | ❓ | B | codex 列举/分页/读取 + 订阅资源事件 |
| MCP 重定向同源限制 | ❌ | ✅ | ❓ | B | 防凭据随重定向外流 |
| MCP 协议版本兼容 | ❌ | ✅ | ❓ | B | codex 在两版协议间切换 |
| **把自己作为通用 MCP server** | ⚠️ | ✅ | ➖ | B | yanshi 只有 `vcs-mcp`（5 个 VCS 工具）；codex 暴露 `codex`+`codex-reply` 两工具可续接会话 → 别的 agent 能把它当子 agent 调 |
| SKILL.md 技能系统 | ✅ | ✅ | ✅ | — | yanshi 发现/加载/前缀命名空间 |
| 技能安全扫描 | ✅ | ❓ | ✅ | — | yanshi 扫描规则 + 混淆检测 + 门禁默认拒装（本次刚补同形字/hex 两条绕过） |
| 技能依赖声明与探测 | ✅ | ✅ | ❓ | — | yanshi `requires.go` |
| 技能安装（本地/URL/git/归档） | ✅ | ✅ | ✅ | — | |
| 模型撰写新技能 | ⚠️ | ✅ | ✅ | B | yanshi `skill_write` 需 user skills dir |
| **隐式 skill 调用识别** | ❌ | ✅ | ❓ | B | codex 从 shell 命令读到 SKILL.md 或跑 skill 的 scripts/ 即判定被调用 |
| **plugin manifest 一体化** | ❌ | ✅ | ✅ | **A** | codex 一个 manifest 同时声明 skills + MCP servers + hooks + apps。yanshi `plugins.go` 的 plugin.json **只认 `skills/` 目录**；单二进制本地部署最需要「装一个包，工具+技能+钩子齐活」 |
| 插件宿主 | ⚠️ | ✅ | ✅ | — | yanshi `internal/plugin/host.go` **零 import**（骨架未接线） |
| 插件市场 / 远程源 | ❌ | ✅ | ✅ | C | codex curated/npm/bundle 三种源；QwenPaw 四个注册中心 |
| 模型自助装插件 | ❌ | ✅ | ❓ | C | 需用户批准的安装请求 |

## 10. 多 agent 与协作

| 能力 | Y | C | Q | 建议 | 说明 |
|---|---|---|---|---|---|
| 子代理衍生 | ✅ | ✅ | ✅ | — | yanshi `agent_spawn/start` 两套 |
| 子代理深度上限 | ✅ | ✅ | ❓ | — | yanshi `MaxSubAgentDepth` |
| 托管子代理注册表 | ✅ | ✅ | ✅ | — | yanshi 生命周期+持久化+外部 agent 接入 |
| 角色化子代理 | ⚠️ | ✅ | ❓ | **A** | yanshi 角色是 `agentroles.go` 里**硬编码的 7 个 RoleDef**；codex 扫 `{config}/agents/*.toml`（model/instructions/features/skills 白名单）→ 用户自定义专家角色目前只能改代码重编译 |
| DAG 工作流编排 | ✅ | ⚠️ | ✅ | — | yanshi `agent_dag`/`agent_workflow` 多步依赖并发，**比 codex 强** |
| 批处理 DSL | ✅ | ❓ | ❓ | — | yanshi `agent_batch` 一次跑一组同构任务 |
| agent 间消息 | ⚠️ | ✅ | ✅ | B | codex spawn/message/followup/result 四类；yanshi 有 `send_input` 但非通用消息总线 |
| **带历史 fork 派生子代理** | ❌ | ✅ | ❓ | B | codex `FullHistory` / `LastNTurns(n)`；yanshi 子代理只继承 instruction 与工具集，长任务要重新喂上下文 |
| **子代理 worktree 隔离** | ⚠️ | ✅ | ✅ | **A** | yanshi 子代理共用同一个 WorkRoot（`subagent.go` 无 cwd 参数），仅 goalloop 的 ACP 路径有 worktree → **并发子代理互相踩文件**。codex 有 `codex-thread.json` 原子绑定 + keep_count 自动清理 |
| agent 血缘图查询 | ⚠️ | ✅ | ❓ | B | yanshi registry 有 ParentID 但只写 JSON 快照，无「杀掉整棵子树」「列活跃后代」 |
| ACP 客户端（拉起外部 CLI） | ✅ | ⚠️ | ✅ | — | yanshi 拉起 codex/claudecode 并交付 VCS-MCP + 策略，**比 codex 强** |
| ACP server 端 | ✅ | ✅ | ✅ | — | yanshi 暴露给 Zed 等宿主 |
| **`acp_delegate` 未进默认 profile** | ⚠️ | ➖ | ➖ | **F** | 已在组合根注册，但**不在 `profile.go` 默认 allow list** → WS 每次弹窗、SSE 永久 fail-closed。与 profile 注释里为 `agent_spawn` 给出的「避免倒挂梯度」理由不一致 |
| 任务 broker + 远程 worker | ✅ | ⚠️ | ⚠️ | — | yanshi 独立进程连 Task API 领活 |
| **code-mode（JS 内编排工具）** | ❌ | ✅ | ❌ | **A** | codex 在 V8 isolate 跑模型写的 JS，工具挂全局 `tools` 对象，无 fs 无网络，`store()/load()` 跨 cell。**一个「搜 50 文件、过滤、回 3 条」的动作从 50 轮往返压成 1 次 exec**；guard 六维可原样套在每次回调上；Go 侧用 goja 即可，不必引 V8 的 CGO 负担 |
| 云端任务 / best-of-N | ➖ | ✅ | ➖ | C | 绑定 OpenAI 云后端 |
| IM 渠道（13+） | ➖ | ➖ | ✅ | C | 钉钉/飞书/企微/Telegram/Discord/Slack/Matrix/SIP 等，平台化 |
| Hub 多租户 | ➖ | ➖ | ✅ | C | Docker/本地双供给 + WS 代理 |
| agent 身份签名（Ed25519） | ❌ | ✅ | ❌ | C | 车队/合规管控 |

## 11. VCS 与代码操作

| 能力 | Y | C | Q | 建议 | 说明 |
|---|---|---|---|---|---|
| **自动编辑追踪（autoVCS）** | ✅ | ❌ | ❌ | — | yanshi fs 工具每次编辑自动入 SQLite VCS，**agent 无需配合**。codex/QwenPaw 都没有等价物 —— **最大护城河** |
| VCS worktree 分支与三方合并 | ✅ | ✅ | ✅ | — | yanshi 从 main_head 分出、树级三方合并回并 |
| 编辑时间线 | ✅ | ⚠️ | ✅ | — | yanshi 按时间列出编辑事件与提交 |
| 恢复预览（dry-run） | ✅ | ✅ | ✅ | — | yanshi 生成恢复计划先给用户看 |
| 冻结检测（外部改动） | ✅ | ❓ | ❓ | — | yanshi apply 前检出外部改动即拒，**独有** |
| symlink 逃逸防护 | ✅ | ✅ | ❓ | — | |
| 孤儿 worktree 回收 | ✅ | ✅ | ❓ | — | |
| VCS GC | ⚠️ | ✅ | ❓ | — | 本次刚修（年龄下限此前无法关闭，goal loop 场景一个 commit 都收不掉） |
| git 只读工具 | ✅ | ✅ | ✅ | — | yanshi `git_status`/`git_diff` 结构化 |
| git 子进程硬化 | ⚠️ | ✅ | ❓ | B | codex 注入 `safe.bareRepository`、禁 hooks、剥 `GIT_*`、超时杀进程树。yanshi 已停读 operator 全局 gitconfig，但未做全套 |
| 内嵌 git 库（无需 git 二进制） | ❌ | ✅ | ❌ | B | codex 用 gix 建一次性基线仓算增量 diff |
| GitHub PR 工具 | ✅ | ⚠️ | ❓ | — | yanshi `pr_context` 只读 + comment/approve/merge 审批门控 |
| 不可信 PR 内容标注 | ✅ | ❓ | ❓ | — | yanshi 把 PR 正文标为数据防提示注入，**独有** |
| 诊断汇总 | ✅ | ✅ | ✅ | — | yanshi `diagnostics` 汇总编译/lint |
| 测试运行结构化 | ✅ | ❓ | ✅ | — | yanshi `run_tests` 跑 Go/cargo/npm |
| 代码审查工具 | ✅ | ✅ | ❓ | — | yanshi `review` 分块读 diff |
| **外部 agent 配置迁移** | ❌ | ✅ | ❌ | B | codex 导入十类配置（config/skills/AGENTS.md/plugins/MCP/subagents/hooks/commands/memory/sessions），幂等台账去重 |
| 分支摘要 / PR 状态 | ❌ | ✅ | ❓ | B | codex TUI 显示分支、与默认分支增删行、经 gh 查开放 PR |
| commit/PR 署名注入 | ❌ | ✅ | ❓ | C | Co-authored-by |

## 12. TUI / CLI 交互

| 能力 | Y | C | Q | 建议 | 说明 |
|---|---|---|---|---|---|
| 全屏 TUI | ✅ | ✅ | ✅ | — | yanshi Bubble Tea；codex ratatui；QwenPaw Textual |
| **后端发现与自举** | ✅ | ⚠️ | ⚠️ | — | yanshi lockfile + healthz 找后端，找不到就进程内起一个；多窗口自愈选举。**独有** |
| 双传输（WS 主 / SSE 备） | ✅ | ⚠️ | ✅ | — | |
| 斜杠命令 | ✅ | ✅ | ✅ | — | yanshi 38 个 / codex ~60 个 |
| 斜杠命令补全弹窗 | ✅ | ✅ | ✅ | — | |
| @ 路径补全（frecency） | ✅ | ✅ | ❓ | — | yanshi 模糊 + 频近度排序 |
| 多类目提及搜索 | ❌ | ✅ | ❓ | B | codex 全部/文件系统/插件三模式循环 |
| 权限对话框 | ✅ | ✅ | ✅ | — | |
| 剪贴板图片粘贴 | ✅ | ✅ | ✅ | — | yanshi Ctrl+V |
| 输入队列 / 暂存 | ✅ | ✅ | ❓ | — | yanshi 模型忙时排队 + stash |
| 偏好持久化（原子写） | ✅ | ✅ | ❓ | — | yanshi 主题/对比度/keymap/locale |
| 可配置键位 + vim 模式 | ✅ | ✅ | ❓ | — | yanshi `/keymap` `/vim` 本次已毕业 |
| 交互式键位向导 | ⚠️ | ✅ | ❓ | B | codex 捕获按键、检冲突、写回配置 |
| 双击键和弦 | ❌ | ✅ | ❌ | C | |
| 国际化 | ✅ | ⚠️ | ✅ | — | yanshi en/zh-Hans |
| **外部编辑器起草长 prompt** | ❌ | ✅ | ❓ | **A** | codex `$EDITOR`/`$VISUAL`；yanshi 全仓 `tea.Exec`/Suspend/EDITOR 零命中，只能在单行 textarea 里写 —— 贴长规格时最痛的一处 |
| **Esc-Esc 回溯 fork** | ⚠️ | ✅ | ❓ | **A** | yanshi 已有 `/fork <seq>` 服务端能力，缺零打字的交互路径。codex：两下 Esc + Enter，**原 prompt 自动填回编辑框**。「上一句问歪了」是最高频动作 |
| **Ctrl+T transcript 全屏浮层** | ❌ | ✅ | ❓ | **A** | codex 独立 pager + 实时 tail + raw 复制模式。yanshi alt-screen 下终端原生选择失效，长回答回看/复制现在很难 |
| **diff 语法高亮 + 行号** | ❌ | ✅ | ❓ | **A** | codex syntect + 按 truecolor/256/16 分档调色板。yanshi 只有 LCS + 三色 sigil，无行号无高亮；主界面是编码 agent，看 diff 就是核心动作 |
| 终端能力探测与降级 | ❌ | ✅ | ❓ | B | yanshi 硬编码 `termenv.ANSI256`，`NO_COLOR`/`COLORTERM`/`TERM=dumb` 全仓零命中 → 低色终端/亮背景配色会崩 |
| 桌面通知（OSC9 / BEL） | ⚠️ | ✅ | ❓ | B | yanshi 只有应用内 toast，长任务切走后不知道回来 |
| 终端标题 / 状态栏 | ❌ | ✅ | ❓ | B | 多窗口同时跑 agent 时靠标题分辨 |
| OSC 8 语义超链接 | ❌ | ✅ | ❓ | B | 文件路径/PR 链接可点 |
| `/diff` 看工作区改动 | ❌ | ✅ | ❓ | B | 审 agent 改了什么不必切窗口 |
| 流式 markdown 渐进渲染 | ⚠️ | ✅ | ❓ | B | yanshi 流式 pending 明确渲染为纯文本，格式与表格要等结束 |
| 会话恢复选择器（带预览） | ⚠️ | ✅ | ✅ | B | yanshi 有 `/sessions` 但无 transcript 预览 |
| 首次运行引导 | ⚠️ | ✅ | ✅ | B | yanshi 有 `init` 但非 TUI 内向导 |
| 终端图形协议（Kitty/Sixel） | ❌ | ✅ | ❌ | C | |
| Ctrl+Z 挂起恢复 | ❌ | ✅ | ❓ | B | |
| headless / 非交互执行 | ✅ | ✅ | ✅ | — | yanshi `exec` |
| doctor 自检 + 自动修 | ✅ | ✅ | ✅ | — | yanshi `doctor.go`+`doctorfix.go` |
| daemon 控制 | ✅ | ✅ | ✅ | — | yanshi status/stop/reload |
| provider 配置 CLI | ✅ | ⚠️ | ✅ | — | yanshi 交互式增改并写 YAML（本次修了非 TTY 强制交互） |
| Web 控制台 | ❌ | ➖ | ✅ | C | QwenPaw 十页面 Tauri 桌面端 |
| **TUI 可脚本化驱动** | ✅ | ❌ | ❌ | — | yanshi `cmd/tuidbg`（tmux 驱动 + 纯 Go 光栅化截图），**独有的测试基础设施** |

## 13. 自驱动与任务

| 能力 | Y | C | Q | 建议 | 说明 |
|---|---|---|---|---|---|
| **目标循环（plan→impl→eval→judge）** | ✅ | ⚠️ | ✅ | — | yanshi `goalloop` 四阶段 + 三类评估器 + 聚合裁判。codex 的 goal 是「idle 时注入续跑提示」，**yanshi 的编排更完整** |
| 分层开发技能 T0–T4 | ✅ | ❌ | ❌ | — | yanshi `RuleTierer` 按目标文本选层，**独有** |
| 双预算（迭代 + token） | ✅ | ✅ | ✅ | — | |
| 运行记录可回看 | ✅ | ✅ | ❓ | — | yanshi 每轮计划/结果落盘 |
| **防伪完成的续跑提示词** | ❌ | ✅ | ❓ | **A** | codex `continuation.md`：无进展判定、完成审计、**阻塞需连续三轮**。yanshi 的 judge 是评估器投票，没有针对「模型谎报完成」的提示词级防线 |
| 预算耗尽软着陆 | ⚠️ | ✅ | ✅ | B | codex 耗尽时注入收尾提示而非硬切；yanshi 直接停 |
| 目标跨会话恢复 | ⚠️ | ✅ | ✅ | B | codex objective + 预算存 SQLite |
| 定时自动化（cron） | ✅ | ❌ | ✅ | — | yanshi `automation_*` 八工具 + `yanshi schedule` |
| 持久任务 + 认领/心跳 | ✅ | ⚠️ | ✅ | — | yanshi broker + 远程 worker |
| 任务工作区隔离 | ✅ | ✅ | ❓ | — | yanshi 每任务独立目录 + 写路由 + 清单门 |
| **持久化用户消息队列** | ❌ | ✅ | ❓ | B | codex `codex queue` 可向**运行中或离线**会话排消息，跨进程轮询 data_version |
| 后台交互式进程并发上限 | ⚠️ | ✅ | ❓ | B | codex 最多 64 并发 |
| 定时休眠工具 | ❌ | ✅ | ❓ | B | codex `sleep` 最长 12 小时，新输入提前唤醒 |
| Mission 多阶段（PRD→实现→验证） | ⚠️ | ❌ | ✅ | B | QwenPaw 独立模式；yanshi goalloop 已覆盖大部分 |
| Computer Use / 桌面自动化 | ❌ | ❌ | ✅ | C | |
| 邮件触发任务 | ❌ | ❌ | ✅ | C | |

## 14. 可观测与运维

| 能力 | Y | C | Q | 建议 | 说明 |
|---|---|---|---|---|---|
| OTel trace/metric | ✅ | ✅ | ⚠️ | — | yanshi 三级 span（会话/turn/工具）+ 可 Noop |
| 结构化日志 + trace ID | ✅ | ✅ | ✅ | — | |
| 日志脱敏 | ✅ | ✅ | ✅ | — | yanshi `SafeLogger` |
| 日志轮转 | ✅ | ❓ | ✅ | — | yanshi 实测 4MiB 真写、跨代不丢行 |
| 健康与就绪端点 | ✅ | ✅ | ✅ | — | |
| 权限审计查询 | ✅ | ✅ | ✅ | — | |
| 用量聚合出账单 | ✅ | ✅ | ✅ | — | |
| 崩溃报告（脱敏） | ✅ | ✅ | ✅ | — | |
| **反馈上报（含 doctor 报告）** | ❌ | ✅ | ❓ | B | codex 独立于 RUST_LOG 的 4MiB 环形缓冲 + Sentry 上传 |
| 指标名与标签基数校验 | ❌ | ✅ | ❓ | B | 防标签爆炸 |
| 遥测目标双分流 | ❌ | ✅ | ❓ | B | codex `log_only` 可带 prompt，`trace_safe` 只留长度与布尔 |
| **网络策略决策审计流** | ⚠️ | ✅ | ❓ | B | codex 每次决策投出带 scope/decision/来源/原因的事件；yanshi 有 `CheckHost` 审计但无事件流 |
| 策略热重载 | ❌ | ✅ | ✅ | B | 运行中换网络策略 |
| 本地加密密钥库 | ✅ | ✅ | ✅ | — | yanshi AES-256-GCM file store + keyring |
| 覆盖率门禁 | ✅ | ❓ | ❓ | — | yanshi `covercheck` 按包阈值 |
| **架构治理门禁 GOV1–GOV9** | ✅ | ❌ | ❌ | — | 分层/行数/文档/装配可达/profile 对账/context 注入/编辑工具/台账逐句对账/符号引用。**codex 与 QwenPaw 都没有等价机制 —— 最独特的工程资产** |
| 依赖 fan-in/out 分析 | ✅ | ❓ | ❓ | — | yanshi `depsanalyze` |
| 功能台账（逐句证据） | ✅ | ❌ | ❌ | — | yanshi `feature-status.yaml` + GOV8 |
| 供应链策略（依赖冷却期） | ❌ | ✅ | ❓ | B | codex 新包 7 天冷却、blockExoticSubdeps |
| 仓库 blob 体积门禁 | ❌ | ✅ | ❌ | C | |

## 15. 部署与分发

| 能力 | Y | C | Q | 建议 | 说明 |
|---|---|---|---|---|---|
| **单二进制（客户端+服务端）** | ✅ | ⚠️ | ❌ | — | yanshi 一个可执行文件；codex 需 npm 壳包 + 平台二进制；QwenPaw 需 Python 运行时 |
| 多子命令 | ✅ | ✅ | ✅ | — | yanshi 18 个 / codex ~30 / QwenPaw 26 |
| argv0 多路复用 | ❌ | ✅ | ❌ | B | codex 按调用名分派 sandbox/apply_patch/helper |
| 版本注入构建 | ✅ | ✅ | ✅ | — | |
| CI 硬门禁（多平台） | ✅ | ✅ | ✅ | — | yanshi 三平台 test + vet + race + CGO_ENABLED=0 矩阵 + governance |
| 文档 diff-gate | ✅ | ❓ | ❓ | — | yanshi 生成文档未同步则 CI 失败，**独有** |
| 发布流水线（多平台产物） | ✅ | ✅ | ✅ | — | |
| **代码签名 / 公证** | ❌ | ✅ | ⚠️ | B | codex macOS Azure Key Vault + rcodesign 公证 staple；Linux cosign sigstore |
| 一键安装脚本 | ❌ | ✅ | ⚠️ | B | codex install.sh 带并发锁与陈旧回收 |
| 自更新 | ❌ | ✅ | ✅ | B | codex 查 registry 比对版本，按安装来源给正确升级命令 |
| 安装来源探测 | ❌ | ✅ | ❓ | B | npm/pnpm/bun/brew/standalone 六类 |
| 契约真相源（JSON Schema） | ✅ | ✅ | ❓ | — | yanshi `sdk/schema/` embed 后 API 原样吐出 |
| Python / TS SDK | ✅ | ✅ | ➖ | — | yanshi 手工镜像 + 四路一致性测试对账 |
| 协议绑定自动生成 | ❌ | ✅ | ❓ | B | codex 导出 TS 类型与 JSON Schema |
| 多传输 app-server | ⚠️ | ✅ | ⚠️ | B | codex stdio/unix/ws/off 四种部署形态 |
| 守护进程自更新 | ❌ | ✅ | ❓ | C | |
| Docker 镜像 | ❌ | ⚠️ | ✅ | B | |
| 桌面端（Tauri） | ❌ | ➖ | ✅ | C | |
| 隧道 / 公网暴露 | ❌ | ⚠️ | ✅ | C | |
| 无 keyring 降级构建 | ✅ | ❓ | ❓ | — | yanshi `-tags=nokeyring` |

---

---

# 附录：不要误伤的护城河

圈选时请注意，以下是三方对比中 yanshi **独有或明显更强**的，不在任何缺口清单里：

| 能力 | 说明 |
|---|---|
| **autoVCS 自动编辑追踪** | fs 工具每次编辑自动入 SQLite VCS，agent 无需配合。codex/QwenPaw **都没有等价物** |
| **GOV1–GOV9 机器强制治理 + 逐句对账台账** | 分层/行数/文档/装配可达/profile 对账/context 注入/编辑工具/台账证据/符号引用。两个参照物**都没有任何等价机制** |
| **guard 六维顺序 + 5 类结构性 HardDeny** | 比 codex 的 approval 层更硬 |
| **运行期工具名 fail-closed（`toolreg`）** | 补编译期 GOV5/GOV7 够不到的运行期缝 |
| **证据关卡 `task_gate_run`** | 跑验收命令并记录证据，codex/QwenPaw 都没有 |
| **goalloop 四阶段 + T0–T4 分层技能** | 比 codex 的「idle 注入续跑提示」编排完整 |
| **DAG 工作流 + 批处理 DSL** | 多步依赖并发编排 |
| **单二进制** | codex 需 npm 壳包 + 平台二进制，QwenPaw 需 Python 运行时 |
| **后端发现与多窗口自愈选举** | lockfile + healthz，找不到就进程内起一个 |
| **不可信 PR 内容标注** | 把 PR 正文标为数据防提示注入 |
| **脚本哈希绑定审批** | 改内容需重批 |
| **冻结检测** | apply 前检出外部改动即拒 |
| **摘要质量门** | 摘要过短/纯元话语判拒 |
| **里程碑保留** | 原文驱逐后保留模型自标的里程碑 |
| **`cmd/tuidbg`** | 可脚本化驱动 alt-screen TUI（tmux + 纯 Go 光栅化截图）的测试基础设施 |
| **FakeModel / Fake 优先测试体系** | 无 API key 的确定性测试 |
| **文档 diff-gate** | 生成文档未同步则 CI 失败 |

# 附录：可信度与方法

**数据源**：三方均为 2026-08-27 最新版本 —— yanshi `1c3760a` / codex `4cb8d86` / QwenPaw `ec25aaee`。全部结论基于**本地克隆的真实代码**，非训练记忆。

**产出方式**：12 个并行 subagent 分域读码（codex 分 3 组覆盖 15 域、QwenPaw 1 组、yanshi 自查 1 组），另有一轮 7 个缺口导向的 agent 交叉验证。

**可信度分层 —— 动手前请按这个分层决定要不要先复核**：

| 层级 | 条目 | 依据 |
|---|---|---|
| **实测** | P0-1（图片 token）、P0-3（CJK 检索） | 写探针跑真实驱动，跑完已删。数字是测出来的 |
| **grep 核验** | P0-2、P0-4、P0-5、P0-7 | 逐条确认调用点/字段映射 |
| **未逐条复核** | 其余全部 | 附了证据路径，但由 subagent 报告，**动手前建议先核那一条** |

**已知的一处自我更正**：早前曾顺着 agent 报告说 sandbox 的 bwrap/SBPL 路径可疑，实际 `BuildBwrapArgs` 由 `sandbox_linux_bwrap.go` 调用、`BuildProfile` 由 `sandbox_darwin.go` 调用，**都已接线**，表中已按实际标 ✅。

**其他注意**：
- 表中 `❓` 表示该 agent 未覆盖到，**不代表「没有」**。
- codex 侧域划分以 `reference/codex/codex-rs/` 目录为准；`codex-rs/config.md` 已是跳转存根，正文在其仓库根 `docs/config.md`。
- 三级划分是**判断不是结论**。P0 的边界（「已交付功能的破损」）是清晰的；P1 与 P2 之间的线可以按路线图移动。
