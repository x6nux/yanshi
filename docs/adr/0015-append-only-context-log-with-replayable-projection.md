# ADR-0015: 活动窗口改由 append-only 日志投影重建

- 状态：accepted
- 日期：2026-08-28

## 背景（Context）

活动窗口（`connSession.history`）当前是**就地重写的有损单副本**。C1 已经补上了持久化先行：`ws_compaction.go::maybeAutoCompact` 与 `ws_compaction.go::compactNow` 都先 `flushHistory` 把整窗写进 `messages` 表，flush 失败就拒绝压缩。所以「原文写不下来就不驱逐」这一半**已经成立**，本 ADR 不动它。

不成立的是另一半：**没有人读那份原文**。`ws_compaction.go::loadSession` 走 `store.Store.Messages`，那是一句无差别的 `SELECT … WHERE session_id = ? ORDER BY seq ASC`。日志里既有压缩前的原文，也有压缩后追加的摘要，投影层不存在，于是两者一起回到窗口里。

实测（探针，本 ADR 落笔前跑过，随后删除；等价断言由 W-D-01 的回归测试常驻）：

| 阶段 | 窗口条数 |
|---|---|
| 压缩前 | 11 |
| 压缩后 | 4 |
| 重连后 | **11** |

重连后的窗口是「全部原文 + 那条摘要」，**比压缩前更大**。后果是三重的：压缩被完整撤销、摘要钱白付、下一轮再压一次再付一次。会话越长越贵，而这条路径在每次断线重连、每次多窗口自愈重发现时都会走到。

被否决的替代方案：

- **给 `messages` 加一列 `compacted_by`，压缩时 UPDATE。** 最小改动，但把日志变成可变的。撤销要再 UPDATE 一次，并发下两个连接同时压缩同一会话会互相覆盖，且「这一行为什么被隐藏」不留痕迹。
- **压缩时 DELETE 被取代的行。** 与 C1 的持久化先行直接冲突 —— 那条规则存在的全部理由就是原文不能没了。
- **另起一套 JSONL 分片（codex 的做法）。** 单二进制里已经有 store 层，再引一套文件格式就是第二个真相源；备份、迁移、损坏自愈全部要写两遍。

## 决策（Decision）

新增一张只 INSERT 的 `context_events` 表承载压缩标记，`messages` 表保持原样；活动窗口改由**投影函数**重建，而不是裸读。

```sql
CREATE TABLE IF NOT EXISTS context_events (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id  TEXT    NOT NULL,
    kind        TEXT    NOT NULL,   -- 'compact' | 'undo'
    hidden_seq  INTEGER NOT NULL,   -- compact: 保留尾部的起点；undo: 忽略
    pinned_seqs TEXT    NOT NULL DEFAULT '',  -- JSON 数组：水位线以下仍然存活的 seq
    created_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_context_events_session
    ON context_events(session_id, id);
```

投影 `Store.ProjectWindow(sessionID)`：按 `id ASC` 折叠事件，`compact` 压栈、`undo` 弹栈，栈顶即当前边界（空栈为 `hidden_seq = 0` 且无 pin），然后 `SELECT … WHERE session_id = ? AND (seq >= ? OR seq IN (…)) ORDER BY seq ASC`。

**为什么需要两个字段而不是一个水位线。** 初稿只有 `hidden_seq`，并声称「压缩后窗口 = 日志的一个后缀」。**那是错的，实现时被反例推翻**：`ctxcompact.Plan` 会 pin **任意位置**的 user 原文、working-set 路径与错误/diff 标记，所以压缩后的窗口是一个**带洞的集合**而不是后缀。只用水位线会同时犯两个方向的错 —— 丢掉被 pin 的开场请求（模型重连后忘记用户最初要什么），以及把水位线以上一条本该驱逐的消息重新放进来。更糟的是它可能把一对 tool_call / tool_result 从中间切开，留下一个孤儿 tool result，而 provider 会直接拒绝那种请求。

`hidden_seq` 因此只表达**保留尾部的起点**，`pinned_seqs` 补上尾部之下的散落 pin。两者合起来才等于活动窗口。

边界由 WS 压缩路径在**压缩后那次 flush 之后**算出，**按位置而不是按内容**。

「按内容」那条路走过一次并被证伪，值得留在这里：初版用 `AssignDedupKeys` + `messages` 上 `(session_id, dedup_key)` 的唯一索引反查每条存活消息的 seq。它看起来是在复用既有机制，实际带着一个致命的别名 —— **dedup key 含批内序号，而压缩恰好改变了批次组成**。压缩前 flush 写 `[X, "ok", Y, "ok"]` 得到 key `ok#0`/`ok#1`；压缩后 flush 只写 `["ok"(第二条), Z]`，存活那条的批内序号变成 0，于是撞上**第一条** "ok" 的 key，解析到错误的 seq。后果不止是顺序错位：它能把 tool_result 排到它的 tool_call 之前，产出 orphan result，provider 硬拒整个请求。触发条件只是「窗口里有两条字节相同的消息」，比如连着两次 "continue"。

**所以让 `AppendMessages` 返回逐行 seq 也救不了** —— 返回的仍是别名后的那个。现在的做法是从日志顶端按真实行位置向下走，把压缩后窗口逐条对齐到它实际占据的行（`internal/api/http/ws_compaction.go` 的 `windowBoundary` / `keptWindowSeqs`），全程不碰 `dedup_key`。

撤销就是再 append 一条 `undo`。日志本身一个字节都不变。

## 后果（Consequences）

- 压缩在重连后不再被撤销，摘要只付一次钱。
- 投影是 `idx_messages_session` 上的一段范围扫描，代价随**窗口**大小而不是会话总长走。W-D-05「大会话的重建耗时不随总长线性增长」由此顺带满足，不需要单独实现一次反向扫描。
- 撤销压缩、检查点、分叉都退化成「在事件流上多 append 一条」，不需要各自碰存储格式。
- **不可违反的约束 1：`context_events` 只接受 INSERT。** 该表不得出现 `UPDATE` 或 `DELETE` 语句，唯一例外是 `DeleteSession` 级联清理整个会话。撤销、修正、回滚一律靠追加新事件表达。
- **不可违反的约束 2：迁移双向可读，旧会话逐字节等同现状。** 没有任何 `context_events` 行的会话，`ProjectWindow` 必须返回与 `Messages` 完全相同的切片。这条要有常驻回归测试，不能只是注释 —— 旧会话是绝大多数，投影一旦对它们有偏差，就是给每个存量用户换了历史。
- **不可违反的约束 5：投影必须与活动窗口在「行」这一层逐条相等，不得是「差不多」。** 具体到两条不可退让的性质：(a) 被 `Plan` pin 住的消息一条都不能丢 —— 它 pin 的正是判定为最不该丢的东西；(b) tool_call 与它的 tool_result 不得被边界切开，孤儿 tool result 会让 provider 直接拒绝整个请求。**这条是补写的**：初稿的单水位线设计违反了它而当时没看出来，是实现阶段用反例推翻的。任何简化边界表示的后续改动都要先对着这两条性质检查。

  **「行」这个限定词是必要的，不是措辞含糊。** 日志存的是行不是消息，而两者不是一一对应：`storeMessagesFor` 把「一条带 ToolCalls 的 assistant」拆成散文行 + 每个 tool call 一行，`restoreMessages` 则把相邻的重新合起来。于是往返之后**内容、顺序、配对三样都原样保留，唯独分组会变** —— 窗口里作为两条消息存在的东西可能回来时是一条。这不是本决策引入的，`restoreMessages` 早于它存在；provider 对两种分组都接受，所以它无害。约束在**行**这一层是精确可断言的，在消息层只是「基本成立」，因此断言写在行层。若日后有人需要分组也保真，那要在日志里加一个「本行与上一行同属一条消息」的标记，是另一条 ADR。
- **不可违反的约束 3：`ctxcompact` 的三步不变。** `Plan` / `EnforceToolCallPairs` / `Assemble` 操作的是投影出来的切片，本决策只改「切片从哪来」。`internal/ctxcompact` 不得因此 import `internal/store` —— 那会让一个纯函数包长出存储依赖。**这条由 `internal/archtest::TestADR0015_CtxcompactMustNotDependOnStore` 强制，直接依赖与传递依赖都算。** 此处原先写的是「GOV1 会红」，那是错的：GOV1 的 `TestR2_PortAllowlist` 只约束 port 包，而 `ctxcompact` 不在其中，`TestR5_PortsMustNotDependOnServiceLayer` 管的又是反方向 —— 所以这条「不可违反」的约束在被写下之后一直没有任何机器守护。它正是本工作包反复抓到的那个形态：只存在于散文里的规则。
- **不可违反的约束 4：`takeChunk` 的超窗上界照旧成立。** 真实上界仍是「窗口 + 历史中最大不可分割段」，随并行工具数线性增长。它是分块摘要的性质，与存储模型无关 —— 不得因本决策宣称它被解决。
- **不可违反的约束 6：对齐失败时拒绝压缩，绝不写一个猜出来的边界。** 整套边界计算建立在一条跨层不变量上 —— **flush 前的投影就是活动窗口**。这条不变量**实测可以被普通对话违反**（`flushHistory` 对整条日志去重，包括已隐藏的行，所以模型压缩后重复一句与隐藏行逐字节相同的话，那句就永远写不进日志），因此「它不会被违反」不能作为设计前提。

  违反时唯一可接受的行为是**放弃这次压缩**（上层照旧收到「没压」，窗口偏大但完整），或把边界退回上一次的值。**不可接受的是照压不误再写一个对不上的边界** —— 那条路实测的结果是窗口只剩摘要一条，保留尾部与全部 pin 一起丢掉。方向必须与 C1 的持久化先行一致：**宁可窗口偏大，不可丢内容**。此处曾把「退化成仅保留尾部」写进注释，实测为假；任何关于退化行为的说法都要跑过再写。

- 代价：压缩路径多一次写。它在同一个 `WriteTx` 里，与 flush 相邻，量级可忽略；但**事件写失败必须与 flush 失败同等对待** —— 拒绝压缩，而不是压缩了却不记标记（那会让下一次重连把刚驱逐的东西全拉回来，即今天的 bug）。

## 关联

- 来源：`docs/superpowers/specs/2026-08-27-capability-roadmap-design.md` §1.2 INF3、§3 W-D-01。
- 相关代码落点：`internal/store/`（事件表与投影）、`internal/api/http/ws_compaction.go`（压缩路径写标记、`loadSession` 改走投影）。
- 前置：ADR-0013（压缩量纲约束）继续成立，本决策不改阈值语义。
- 解锁：W-D-02（跨会话检索）、W-D-03（跨会话记忆生成）、W-D-04（冷会话归档）、W-D-05（尾部重建）、W-D-06（检查点）、W-D-07（记忆溯源）、W-D-08（持久化队列）、W-D-09（输入历史落盘）、W-D-16（目标跨会话恢复）。
