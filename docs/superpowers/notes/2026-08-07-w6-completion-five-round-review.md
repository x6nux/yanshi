# W6 补完（6 条翻牌）五轮评审记录（2026-08-07）

W6 的最后 6 条台账翻牌（B3/W07、M1/SPEC-TOOLIF、B2/LSP1、B3/GH1、A3/MCP1、
A3/C13、A3/V16 —— 共 7 条，B3/W07 起头）完成后按
`docs/superpowers/review-checklist.md` 跑的五轮。
**独立评审 subagent 配额仍然耗尽，五轮全部是主循环自评** —— 与 W7–W10 同样的限制。

W6 现为 11/11，台账 44/63（70%）。

## 翻牌过程中就地发现的实现缺陷（不是评审轮次找到的）

这一包与前几包的差别：**7 条里 4 条的失败不是「缺测试」而是「缺实现」**，
补测试的过程本身就是发现。逐条记：

1. **git 工具一直在读操作员的 `~/.gitconfig`**（B3/W07#2）。`gitEnvIsolation`
   设了 `GIT_CONFIG_NOSYSTEM` 与 `XDG_CONFIG_HOME`，注释断言这覆盖了
   `~/.gitconfig`。**不覆盖**：git 只在 `~/.gitconfig` 缺席时才看 XDG。
   实测（全局 `core.excludesFile = *.go`）：`git_status` 对一个含未跟踪 `.go`
   文件的工作树报告干净。补 `GIT_CONFIG_GLOBAL`。
   > 顺带一提，第一版测试选的配置项是 `status.showUntrackedFiles`，**不可能失败**
   > —— 每个 spec 都显式传 `--untracked-files=all`，命令行永远压过配置。

2. **诊断调用的超时只是「传参」**（B2/LSP1#3）。`diagFor` 传 0（= 用实现自己的
   默认），`diagForStaged` 传剩余预算。`LSPManager` 是**接口**、`Diagnostics` 是
   **无 ctx 的同步调用** —— 「超时不阻塞 turn」这条保证完全依赖被注入实现守约，
   而类型系统不承载它。用卡住的 stub 实测：**一个 turn 被挂住整整 60 秒**。
   抽 `diagnosticsWithin`（goroutine + select）把边界移到调用方。

3. **GitHub 工具对第三方散文零处理**（B3/GH1#3/#4/#5）。PR 标题与正文由开 PR 的人
   书写，和 yanshi 自己产出的字段装在同一个 JSON 信封里；没有定界、没有 spill
   分支、未认证与「PR 不存在」同码折成 `gh: exited 1`。三件都是新实现。

4. **palette 有一个挂死**（A3/MCP1 顺带）。`paletteMove` 跳过分组头用的是无界
   循环，全是头时（每个 server 零工具 —— 一批 server 全挂就长这样）永远转下去，
   UI 冻死且无日志。

## R1 — 配置缝（两条，一条修一条证伪）

1. **`gopls@latest`**（本包新加的 lsp-e2e job）。断言的是诊断的**行号**，那是
   gopls 输出的性质 —— 上游一次改动就能让这个 job 因为与本仓无关的原因变红，
   而第一个看到的人无从分辨。钉到 `v0.22.0`。

2. **「生产预算 800ms 对真实 gopls 冷启动不够」—— 被探针证伪。** 实测
   （darwin/arm64，含模块索引）**~120ms**。但这道缝的另一半是真的：e2e 测试用
   30s 上限而生产默认 800ms，**gopls 变慢时生产静默返回空诊断，而测试照绿**，
   且那在生产里不是错误，是一段空的 diagnostics —— 读起来就是「这文件没问题」。
   改成断言实测耗时 ≤ 4× 生产默认，把这件事变成可观测的。

> 与 W10-R2 同一形状：**计划/直觉的论断被探针证伪，但探的过程暴露了真问题**。
> 注释按实测结论写，不抄原来的猜测。

## R2 — 门禁正反探针（零阻塞）

本包新增门禁 `TestRetiredToolSymbolsDoNotReturn` 逐条探过：

| 探针 | 结果 |
|---|---|
| 生产文件里加回 `lineProgressWriter` 声明 | 红（点名文件行号） |
| `shell.go` 注释里既有的那处提及 | **不误伤**（AST 不含注释） |
| 再往注释里加一处两个符号都提到 | 不误伤 |
| walk 根改成单包（scanned 下限） | 红，且信息点名「walk 没到达树」 |
| 从 `overlayImmuneGateFiles` 删掉登记 | 红 |

翻牌过程中每条子句各自的变异探针（共 14 条）全部先红后绿，逐条记在各自的
台账注释里。

> **注释与门禁的岔路，本仓已经走过三次**（CI bench `continue-on-error`、
> cliff `^feat!`、goreleaser `LICENSE`），三次都是 grep 型门禁匹配到自己的
> 解释文字。这次直接解析 AST：**注释里的标识符不是标识符**，岔路不存在。

## R3 — 文档虚报（一条，结构性）

`docs/superpowers/acceptance-breakdown.md` 的「当前状态」栏 —— 每条标题的
「台账 `partial`」、汇总表的 `1/1/2` 计数、每子句的「已兑现/部分/未兑现」——
**一次会话翻掉 6 条就当场作废 6 处**。230 子句逐条追改不可持续，追不动就会变成
没人信的陈述。

处置**不是**逐条改，而是在文档头部写明：状态栏是写作时快照，现状唯一权威是
`docs/feature-status.yaml`（`go run ./cmd/featurestatus` 现算）；本文追的是
**不随状态变化**的那半 —— 子句怎么切、每句要什么形状的证据、什么样的测试骗得
过去。那半正是查表时要读的东西。

## R4 — 边界与状态（一条真，一条证伪）

1. **证伪：`diagnosticsWithin` 引入的并发不会 race。** 它让原本串行的多文件诊断
   变成「超时后前一个 goroutine 仍在跑，下一个已开始」。核查 `Client.Diagnostics`
   —— mutex 保护的轮询、只读共享状态、`cloneDiag` 返回副本，并发安全；被抛弃的
   goroutine 在自己的 deadline 到时写进 buffered channel 后退出，不泄漏。
   `go test -race` 复核通过。

2. **真问题：timer 与传给实现的 timeout 完全相等，是掷硬币。** 实现恰好准时返回
   时，`select` 在两个都就绪的 case 里随机选 —— 会丢掉**已经拿到的**诊断。
   给兜底 timer 加 50ms 余量：调用方的边界是**实现违约时的兜底**，不是主时钟。

## R5 — 台账证据逐句复核（三条，全是同一形状）

1. **`A3/V16#2「tools/resources 可用」`引的两条证据都只覆盖 resources。**
   tools 那一半靠的是「枚举得出来」—— 那是子句 1。一个列得出来但 `CallTool`
   丢掉载荷的工具满足枚举且完全没用。补断言：server 自己的内容逐字回到调用方。

2. **`M1/SPEC-TOOLIF#3` 的门禁只扫了两个符号，「废弃 JSON 包装」那半无证据。**
   被废的 JSON 信封没留下任何具名符号可以禁止，所以只能断言**取代它的行为**：
   模型收到的是 Result 字段的拼接，不是一个把进度和答案装在一起的包装对象。
   补引子句 2 那条测试，并在它的注释里写明为什么它同时承担两条。

3. **`A3/C13#1「展示 server/tool/status/error」`的证据在 wire 层。**
   子句说**展示**，而一个 snapshot 携带了 error、渲染函数再把它丢掉，wire 断言
   完全满足。两个不同的层，操作员只看其中一个。补渲染层断言（含失败标记在
   去样式后仍可辨）。

> **与 W8-R5、W9-R5、W10-R5 同一形状：证据落在「零件对不对」上，子句问的是
> 「产品做不做得到」。连续四轮各抓到 1–3 条，GOV8 一条都拦不住** —— 这正是
> ADR-0011 写明的边界。

## 本包最值得记的三件事

1. **「注释断言了一件假的事」比「没有注释」更贵。** `gitEnvIsolation` 的
   注释白纸黑字写着 XDG 覆盖了 `~/.gitconfig`，于是没人再去验。缺陷活了下来，
   而且是它自己的文档在保护它。

2. **接口 + 无 ctx 的同步方法 = 一条无法强制的时序保证。** 传一个 timeout 参数
   看起来像在设边界，实际是在提要求。类型系统不承载「你必须在这个时间内返回」。
   要么改签名收 ctx，要么调用方自己 select —— 中间没有第三种。

3. **本仓第一次拉起真实 language server。** 此前全部 LSP 测试（含那个 net.Pipe
   fixture）对话的都是本仓自己写的东西。断言选**行号**而不是「有诊断」：
   LSP 0-based 与编辑器 1-based 的差一错会把模型支去读没问题的代码，
   摘掉 `client.go` 的 `+1` 当场变红。配套 CI job 装 gopls 并把 SKIP 也判失败
   —— 一条永远 skip 的测试读起来就是 pass。

## 未覆盖 / 移交

- **剩余 19 条台账翻牌**：W1 (9)、W2 (3)、W3 (5)、W4 (1)、W9 (1)。
- `D2/V15` 仍卡在子句 1 的 `stream`：**需要判定是补 `/api/v1/threads/{id}/stream`
  端点还是改验收**。
- `B0/TD1`（W2）挡在**验收文本**而非代码：5 条子句里 2 条不是行为断言，
  无法用测试证伪，要先改写 acceptance 并显式改写 `acceptancePins` 那一行。
- WS/SSE 附件历史分岔（W8-R4 发现，三轮移交未做）—— 需要 wire 契约变更。
- W6-R5 移交的 7 个幻影帧构造器。
- `G/VISION-TOOL` 最后一条子句。
- MCP 的 prompts（零实现）与 OAuth 收口 —— **不在 A3/V16 与 A3/C13 的 acceptance
  子句里**，是产品改进项，按 GOV8 的逐句口径不构成翻牌阻塞。
- ubuntu 侧覆盖率阈值复核 + lsp-e2e job 的首次真实运行（本机 darwin 验过）。
- **独立评审仍为零。** 需要提高 `CLAUDE_CODE_MAX_SUBAGENTS_PER_SESSION` 或换新会话。
