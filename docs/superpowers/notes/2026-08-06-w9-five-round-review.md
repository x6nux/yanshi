# W9 五轮评审记录（2026-08-06）

W9 十个任务完成后按 `docs/superpowers/review-checklist.md` 跑的五轮。
**独立评审 subagent 配额（200/200）在本会话仍然耗尽，五轮全部是主循环自评**
—— 与 W7、W8 同样的限制，同样记在最前面。

六条修复，全部是本工作包自己当天提交引入的。

## R1 — 配置缝（一条阻塞）

**`SidecarPath` 让两个 `-config` 塌缩到同一个运行时存储。** 它替换扩展名
而不是追加后缀，于是 `config.yaml` 与 `config.yml` 都变成
`config.appstate.json`；扩展名缺失的点文件更彻底 —— `filepath.Ext(".hidden")`
返回**整个名字**（没有主干），trim 后为空、落进 fallback。两个 `-config`
不同的 `yanshi app` 进程从此读写同一个文件，互相静默覆盖。

顺带：`*.appstate.json` 进 `.gitignore`。它是运行时产物，名字跟着 `-config`
走，没有固定名可以逐个列。

## R2 — 门禁正反探针（零阻塞）

本包新增的门禁本轮逐条探过，全部命中：

| 探针 | 结果 |
|---|---|
| sdk-contract 加 `continue-on-error` | 红 |
| sdk-contract 去掉 `[test]` extra | 红 |
| docs.yml 少一条 `sdk/**` | 红（且点名「1 次，want 2」） |
| headless smoke 换宽 `grep` | 红 |
| R6 白名单塞一个不导入的包 | 红（死条目） |
| R6 白名单去掉一个真导入者 | 红（未登记） |
| 幻影名 `spawn_agent` 放回屏蔽集合 | 红 |
| parity 删一条豁免 / Go 加一个字段 | 两个方向都红 |
| 跨传输：HTTP 多一个字段 / 少一个字段 | 两个方向都红 |
| `SchemaBytes` 回到自建字面量 | 红 |
| v1.1 覆盖层重新变死 | **Go 结构断言与 Ajv 行为断言各自独立地红** |
| pytest alias 扫描的 `_aliases()` 返回空 | 红（正对照生效） |

最后一条值得单记：结构可达与真的拒绝是两件事，两层防线各自抓到了同一个回归。

## R3 — 文档虚报（三条）

删掉伪生成器之后，三处仍在宣传它：

1. `docs/api/sdk-ts.md` 正文「类型由 `cmd/api-schema` 生成」+ 末节
   「`sdk/ts/v1.ts` 是 `cmd/api-schema -out` 生成的契约镜像……修改后重生成」
   —— 那个 flag 与那半命令都已不存在。
2. `docs/api/resources.md` 头部「从 `internal/api/v1` 生成」「修改 `types.go`
   后重生成」—— T7 删掉第二张手工表后它只从 JSON Schema 生成，改 `types.go`
   不影响本页；两者是**对账**关系而非同源。
3. `CLAUDE.md` 说两个 SDK 是「从中生成/校验的客户端」—— 两侧都是手工镜像。

## R4 — 边界与状态（两条）

1. **`FileConfig` 的多进程缝。** 没有任何东西阻止两个 `yanshi app` 共享一个
   `-config`，而每个各持内存快照、flush **整份文档**，于是第二个写入者抹掉
   第一个写过的每一个键 —— last-writer-wins 是对整个 store 而不是 per key；
   一个长跑的读者还会一直服务启动时的快照，别的进程写进去的键它永远读不到，
   报「config key is not set」而那个键明明在文件里。
   改成「文件即真相」：去掉缓存，Read 读盘、Write 读-改-写。
   **残余竞态具名写下** —— 两个进程同一瞬间写不同的键仍可能丢一个，关掉它
   需要文件锁，那是为一个实际没人并发写的 store 引入的真实依赖。

2. **跨传输对照只比了 happy path。** 两条传输各自映射 service 错误
   （HTTP → status code，JSON-RPC → 标准码），T4 那三条测试对这一半一言不发。
   补的判据比的是**等价类**不是相等：404 与 -32602 是同一个答案的两种说法，
   钉住的是「两边都说 NO」以及没有哪一边静默成功。

## R5 — 台账证据逐句复核（两条）

1. **`D1/APS1#1「JSON-RPC thread/turn 可用」`** 引的三条里两条是 thread 的
   happy path、第三条整条都是错误路径 —— **turn 那一半没有正向证据**。
2. **`H2/APIREF1#1「v1 API 有参考」`** 引的是生成器的渲染测试。必要而不充分：
   它们证明生成器**能产出**那些块，在内存里；哪怕 `docs/api/resources.md`
   从未被写出、或写过之后被清空，它们照样通过 —— Go 侧没有任何东西读过那两个
   文件。读它们的只有 `docs.yml` 的 `git diff --exit-code`，那是 workflow
   步骤不是测试，而且它的 paths 过滤器可能跳过。

与 W8-R5 三条同一形状：**证据落在「零件对不对」上，子句问的是「产品做不做
得到」**。

## 本轮最值得记的三件事

1. **`internal/agent/spawn` 整包 `//go:build ignore`。** R6 白名单的死条目
   检测发现的：grep 说它 import 了 orchestrator，`go list` 说没有。它不是
   「造好了没接上」，而是**根本不参与编译**。计划把它算作 6 个导入者之一，
   正是基于 grep。它留下的残骸是一个占着「屏蔽」槽位的幻影名 `spawn_agent`
   —— 屏蔽列表里的幻影比允许列表里的更隐蔽：后者让权限读起来更宽，前者让
   限制读起来更严，且哪天真有人用这个名字注册工具，它会静默豁免于这条为它
   而写的屏蔽。

2. **伪生成器的处置是删除而不是修复。** 一段自称生成器的手抄字面量，不如
   没有。真生成的额外收益只剩「注释与类型细节也不漂」，代价是一个 Go→TS
   类型映射器；守门人换成四路 parity 测试，那正是被丢弃的
   `_ = v1.SchemaBytes()` 假装在做的事。

3. **两张表层叠时后写的赢。** `paramResponseDefs()` 盖在 schema 渲染之上，
   于是 `resources.md` 印着一个 TS/Python 都没有的 `images`，并在
   `ThreadResumeResponse.items` 已从四路契约删掉之后继续印它。
   **一个手工表在替不存在的字段作证。**

## 未覆盖 / 移交

- **`D2/V15` 保持 partial**，卡的是子句 1 的 `stream`：D1 未提供
  `/api/v1/threads/{id}/stream`，Python 客户端在 `transport="ws"` 时直接 raise。
  那是 D1 的端点缺口，不属于 W9 的对外契约范围 → **W10 判定是补端点还是
  改验收**。
- `internal/agent/spawn` 是留是删 → **W10**。
- W8 移交的 WS/SSE 附件历史分岔：本包**未做**。它需要服务端把展开后的文本
  回 publish 给客户端（`history_replaced` 帧是现成载体），是 wire 契约变更 ——
  W9 的十个任务里没有它的位置，**继续移交 W10**。
- W6-R5 移交的 7 个幻影帧构造器仍未处理 → **W10**。
- `G/VISION-TOOL` 最后一条子句 → **W10**。
- W6 Task 12 的 11 条台账翻牌仍未做。
- **独立评审仍为零。** 需要提高 `CLAUDE_CODE_MAX_SUBAGENTS_PER_SESSION`
  或换新会话。
