# W7 五轮评审记录（2026-08-06）

W7 十一个任务全部完成后按 `docs/superpowers/review-checklist.md` 跑的五轮。
**独立评审 subagent 配额（200/200）在本会话已耗尽，五轮全部是主循环自评** ——
这条必须写在最前面，因为「自评五轮」与「五个独立视角各评一轮」不是同一件事，
把它们记成同一件事正是这份清单存在的理由。

七条修复里 **六条是本工作包自己当天提交引入的**。

## R1 — 配置缝（三条阻塞）

配置缝是本仓产出率最高、且 GOV1–GOV9 一条都看不见的一面。

1. **`observe.slog_trace_id` 关掉后并没有关掉关联 ID。**
   `ws.go` 只是不调 `WithIDs`，而 `internal/agent/orchestrator::ensureTurnIDs`
   会给任何缺 ID 的一轮补铸一套（它存在的理由是 goal loop / ACP / headless
   也要有关联日志）。于是 ID 在几帧之后原样回来，而且**不带 session.id** ——
   看起来可 join，实际不能。修法是 `internal/observe/log::WithoutIDs`：单向抑制，
   任何后续 `WithIDs` 都成为 no-op。
   原测试只捕到 `ws.go` 那两行日志，对此完全不敏感；新测试驱动一次工具调用，
   让 guard 在编排器边界之后写日志。

2. **SSE 的 `thread_id` / `turn_id` 未经消毒就进了日志字段与 span 属性。**
   这是全系统唯一由客户端选定的标识符。三种后果各不相同：换行符在 text 格式下
   伪造整行日志；无界长度是客户端可控的放大；任意 `session.id` 在 tracing 后端
   是无界基数。`internal/observe/log::SanitizeID` 在唯一入口做长度与字符集收口。

3. **`observe.cost_in_status` 只挡住 4 个上 wire 面里的 2 个。**
   会话列表（`/stats` 渲染它）与 `session_restored` 照样带成本。两处列表投影
   本来就是逐字相同的拷贝，合并成 `internal/api/http::Server.sessionInfos` 后
   把门放进去。**入库账本不设门**：关掉显示不该丢会计。

   为它写的静态完备门禁自己也经过两轮：第一版正则锚在「行首或点号」，
   单行复合字面量 `{CostUSD: ...}` 从中间漏过去 —— **反探针绿、正探针不绿**，
   这正是两个方向都要跑的理由。

## R2 — 门禁正反探针（一条）

新加的 `internal/archtest::TestBenchCIGateRunsEveryBenchmarkPackageAboveOneIteration`
里那条 hard-gate 判据是**空壳**：给 bench job 加 `continue-on-error: true`，门禁不动。
它在全文件搜索该短语并与 `bench-compiles` 比较字节偏移，而该短语出现在 job 上方的
**注释**里（那段注释解释的正是要防的状态）—— 判据把自己的立论当成了立论成立的证据。
改为按 job 的 YAML 块取、并剥掉注释行。

其余探针（bench 存在性、`-benchtime=1x`、nightly benchstat、WS/SSE 两个 turn span、
usage recorder）**都能被对应变异打红**。其中 turn span 的头两次探针只拿到编译失败，
不算数，改成保留 import 的等价空实现后才是真探针。

## R3 — 文档虚报（两条）

1. **一句事实错误的注释。** `ws.go` 的 flag 门注释写「there is one set of ids,
   not two」来论证日志与 span 一起关。`tools.ThreadLink` 是第二条关联通道，
   在几行之下绑定，喂的是工具审计记录。它**故意不受抑制** —— 日志偏好不该变成
   问责缺口 —— 但原句会让下一个读者去找一个不存在的 bug。

2. **`config.example.yaml` 从不写出任何 flag 名。** 它只有 `overrides: {}`，
   要知道能填什么得读 Go 源码。flag 什么都不做时无害，两个 flag 真正生效之后
   就不再无害。三个 flag 连同默认值与作用范围写进去，
   `internal/features::TestExampleConfigNamesEveryFlag` 读文件对账。

## R4 — 边界与状态（一条，两个传输）

**失败的一轮在两个传输上都报成功的 span。** SSE 声明了 `turnErr` 却从未赋值；
WS 只在 schema/judge 两个 cap 处赋值，`hadError` 与取消都没赋。于是模型失败、
客户端断开、干净跑完在任何 tracing 后端里长得一样 —— 而这正是 turn span 存在
要回答的那个问题。`go vet` 看不见：变量确实被读了，在 defer 里。

两个传输是两套独立的 turn 循环与错误处理，各配一条回归测试。这种分离本身就是
SSE 那一半被写下又忘了赋值的原因。

## R5 — 台账证据逐句复核（两条）

按「这条测试证明的是不是这条子句」而不是「它跑不跑」重读了 26 条 evidence。

1. **`C4/OBS1#3 级别可配`** 只引了 `ParseLevel` 的字符串映射表。必要而不充分：
   `New` 把 `cfg.Level` 丢掉，每个 `ParseLevel` case 照样过。补
   `TestConfiguredLevelActuallyFilters`，两个方向都驱动真实 handler。

2. **`C4/OBS2#3 可关闭`** 只引了 otel 包自己的开关，对配置链一无所知。
   补的 bootstrap 测试**写了两版**：第一版没有活的 collector，而 `Setup` 在
   collector 不可达时降级成 no-op，于是每个 case 都报 `Enabled()==false`，
   **两个方向的变异全绿**。加上 OTLP stub 给「都开」一个正对照之后，
   两个变异各自红在对应那一行。

   > 这条与 R1 第 3 条是同一个教训的两次复发：**没有正对照的否定断言，
   > 被任何「永远不启用」的实现满足。**

## 反复出现的形状（本轮再次确认）

- **「造好了没装上」**：本轮没有新增，但 R5 第 2 条的第一版是它的镜像
  —— 装配测试自己没装上观测点。
- **「测试把缺陷钉成预期行为」**：R2 的 hard-gate 判据、R5 的第一版 bootstrap
  测试都属于这一类的变体 —— 断言写下了，但它对被断言的事实不敏感。
- **配置缝仍是最高产的一面**：三条阻塞里三条都在这里，且**没有一条**能被
  GOV1–GOV9 中的任何一道看见。

## 未覆盖 / 移交

- `G/VISION-TOOL` 的 `/cost` 子句：实现构造上成立（图像 token 走 provider 的
  `PromptTokens`），但没有测试把「带图像的一轮」跑到成本上。整条移交 W8，
  理由写在 `docs/feature-status.yaml` 该条目上方。
- W6 Task 12 的 11 条台账翻牌仍未做，每条的阻塞点已逐条标注在台账里。
- **独立评审仍为零。** 需要提高 `CLAUDE_CODE_MAX_SUBAGENTS_PER_SESSION`
  或换新会话才能拿到真正独立的视角。
