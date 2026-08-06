# W6 Task 9 重验记录 —— LSP1 / C13 / MCP1 / V16

日期：2026-08-06。方法：逐条 grep + 读源码，不采信审计原文。

## 背景：为什么这道关卡是强制的

W6 计划开头的警告是「审计对 W07/DT4/DT5/GH1 的判定**理由**已大面积过时」。
执行 Task 3–8 时这条被验证了**六次**：每一项审计给的首要理由都已不成立，
但每一项底下都压着一个独立的、往往更隐蔽的第二缺口。结论碰巧站得住，
过程完全不能照抄。

本关卡因此要求对剩下四项重新亲验。

## 结果：这四项的审计判定**全部属实**

与前六项相反。逐条：

### V16-c —— MCP health 完全未接线（最严重）

`internal/mcp/health.go` 是完整实现，而三个导出符号**零个非测试调用点**：

```sh
grep -rn 'SetHealthConfig\|StartHealthLoop\|CallToolRetry' --include='*.go' internal cmd | grep -v _test.go
# 只有 health.go 里的定义本身
```

后果：`StartAll` 之后死掉的 server **永远停在 Ready**，它的工具继续被通告给模型，
每次调用都失败且不尝试重连。

**GOV4 为什么没抓到**：GOV4 断言的是「bootstrap 里每个导出的 `Build*` 能从 `Build` 到达」，
而 `buildMCPManager` **是**可达的 —— 没接上的是组合根**已经持有的那个组件**上的方法。
审计把「零件造好、总装没接」定性为主导失效模式，它在门禁看的那一层**下面一层**复发了。

### V16-a —— `ServerStatus.Resources` 恒 nil

`ListResources` 在 `client.go` 有接口声明、`httpclient.go` 与 `stdio.go` 各有实现，
**零个调用点**；`.Resources =` 全仓无赋值。

### V16-b —— prompts 零实现

`internal/mcp/*.go` 非测试代码里 grep `Prompt` 无任何命中。

### LSP1-a —— `DefaultLanguages` 只有 go + python

`manager.go:33` 确认。

## 据此对 Task 10/11 的调整

Task 9 的 Step 2 允许按实测重写后续步骤。本轮先接了**成本最低、后果最重**的一条
（health loop + `CallToolRetry`），其余三条（Resources 聚合、prompts、语言表扩展）
维持计划原样，因为它们是**增功能**而非**修断裂**——断裂是「写好了没接」，
而那三条是「没写」。两者优先级不同：前者是当场可修的欺骗性状态
（组件看起来在工作），后者是诚实的缺失。

## 一条给治理层的线索

V16-c 与 W6 Task 2 的 `netpolicy.ManagedEnv` 是**同一种形状**：导出符号、
实现完整、有测试、零生产调用点，而 GOV6 只认 `With<X>(ctx,…) context.Context`
这一种签名。一条「导出符号零生产调用点」的通用门禁会同时抓到这两个，
但也会误伤大量合法的库导出（sdk/、给测试用的 seam），所以它需要一张
豁免表和一次认真的设计 —— 记在这里作为线索，不在 W6 内实施。

---

# W6 五轮评审记录（2026-08-06，主循环自评）

subagent 配额本会话十次实测锁死（200/200），五轮全部自评，**独立评审 0 轮**。

| 轮次 | 角度 | 发现 |
|---|---|---|
| R1 | 配置缝 | **实** —— `cfg.LSP.Override` 重建条目时丢掉默认 Markers，覆盖一个语言的命令就静默改变了它的**启动闸门**（已修 `30e2028`） |
| R2 | 边界与状态面 | 零发现。race 干净；`rememberOpen` 的 cap 边界正确；`hostOnly(searchBase)` 与新端点一致（补了断言） |
| R3 | 空壳测试识别 | **实** —— `TestMergeNumstatByPathDeduplicates` 直接驱动纯函数，摘掉调用点后照绿。去重可被静默拔线（已补端到端断言） |
| R4 | 文档虚报 | **实** —— `configuration.md` 仍写「空=内置 {go, python}」，实际已 6 种，且未提两道启用闸门 |
| R5 | 幻影名与死代码 | **实，未修** —— 见下 |

## R5 的未处理项：7 个幻影帧构造器

与本轮删掉的 `mcp_list` 完全同型：有构造器、有往返测试（让它们看起来在被维护）、**零生产构造点**。

`NewDisableSkill` / `NewEnableSkill` / `NewTrustSkill` / `NewUninstallSkill` /
`NewUntrustSkill` / `NewUserMessageWithSchema` / `NewToolProgress`

现场清点命令（不要抄数字）：

```sh
for f in $(grep -oE 'func New[A-Za-z]+\(' internal/proto/frame.go | sed 's/func //;s/(//' | sort -u); do
  n=$(grep -rn "proto\.$f(\|[^a-zA-Z]$f(" --include='*.go' internal cmd | grep -v _test.go \
      | grep -v 'internal/proto/frame.go' | wc -l | tr -d ' ')
  [ "$n" = "0" ] && echo "$f"
done
```

**`NewToolProgress` 是其中最具误导性的一个**：`internal/cli/tui/model.go` 里有
`case "tool_progress":` 分支、`internal/api/v1/events.go` 里也有 —— 两个消费者在等一个
**没有任何服务端会构造**的帧。读代码的人会以为这条链路是活的。

未在本轮删除，因为删 wire 词表要同时动 parity 测试、SSE golden 与 `sdk/` 契约，
而这需要一次完整的、不赶时间的编辑。**归 W9（对外契约）**，那正是它的主场。

⚠️ 删除时的判据（与 `mcp_list` 那次相同）：**服务端零构造点 = 客户端永远收不到**，
所以删掉它不改变任何外部可观察行为；留着它则会持续把「有名字、有测试」误读为
「有实现」。
