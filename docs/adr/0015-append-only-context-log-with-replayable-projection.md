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

边界由 WS 压缩路径在**压缩后那次 flush 之后**算出。尾部起点锚定在保留尾部展开成的行数上（`log_top − rows(kept tail)`）；散落 pin 的 seq 通过 `AssignDedupKeys` + `messages` 上 `(session_id, dedup_key)` 的唯一索引反查得到 —— 复用既有去重机制，不需要改 `AppendMessages` 的签名。

撤销就是再 append 一条 `undo`。日志本身一个字节都不变。

## 后果（Consequences）

- 压缩在重连后不再被撤销，摘要只付一次钱。
- 投影是 `idx_messages_session` 上的一段范围扫描，代价随**窗口**大小而不是会话总长走。W-D-05「大会话的重建耗时不随总长线性增长」由此顺带满足，不需要单独实现一次反向扫描。
- 撤销压缩、检查点、分叉都退化成「在事件流上多 append 一条」，不需要各自碰存储格式。
- **不可违反的约束 1：`context_events` 只接受 INSERT。** 该表不得出现 `UPDATE` 或 `DELETE` 语句，唯一例外是 `DeleteSession` 级联清理整个会话。撤销、修正、回滚一律靠追加新事件表达。
- **不可违反的约束 2：迁移双向可读，旧会话逐字节等同现状。** 没有任何 `context_events` 行的会话，`ProjectWindow` 必须返回与 `Messages` 完全相同的切片。这条要有常驻回归测试，不能只是注释 —— 旧会话是绝大多数，投影一旦对它们有偏差，就是给每个存量用户换了历史。
- **不可违反的约束 5：投影必须与活动窗口逐条相等，不得是「差不多」。** 具体到两条不可退让的性质：(a) 被 `Plan` pin 住的消息一条都不能丢 —— 它 pin 的正是判定为最不该丢的东西；(b) tool_call 与它的 tool_result 不得被边界切开，孤儿 tool result 会让 provider 直接拒绝整个请求。**这条是补写的**：初稿的单水位线设计违反了它而当时没看出来，是实现阶段用反例推翻的。任何简化边界表示的后续改动都要先对着这两条性质检查。
- **不可违反的约束 3：`ctxcompact` 的三步不变。** `Plan` / `EnforceToolCallPairs` / `Assemble` 操作的是投影出来的切片，本决策只改「切片从哪来」。`internal/ctxcompact` 不得因此 import `internal/store`（GOV1：那会让一个纯函数包长出存储依赖）。
- **不可违反的约束 4：`takeChunk` 的超窗上界照旧成立。** 真实上界仍是「窗口 + 历史中最大不可分割段」，随并行工具数线性增长。它是分块摘要的性质，与存储模型无关 —— 不得因本决策宣称它被解决。
- 代价：压缩路径多一次写。它在同一个 `WriteTx` 里，与 flush 相邻，量级可忽略；但**事件写失败必须与 flush 失败同等对待** —— 拒绝压缩，而不是压缩了却不记标记（那会让下一次重连把刚驱逐的东西全拉回来，即今天的 bug）。

## 关联

- 来源：`docs/superpowers/specs/2026-08-27-capability-roadmap-design.md` §1.2 INF3、§3 W-D-01。
- 相关代码落点：`internal/store/`（事件表与投影）、`internal/api/http/ws_compaction.go`（压缩路径写标记、`loadSession` 改走投影）。
- 前置：ADR-0013（压缩量纲约束）继续成立，本决策不改阈值语义。
- 解锁：W-D-02（跨会话检索）、W-D-03（跨会话记忆生成）、W-D-04（冷会话归档）、W-D-05（尾部重建）、W-D-06（检查点）、W-D-07（记忆溯源）、W-D-08（持久化队列）、W-D-09（输入历史落盘）、W-D-16（目标跨会话恢复）。
