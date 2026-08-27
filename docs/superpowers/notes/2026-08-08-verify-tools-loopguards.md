# 实测记录：工具与循环护栏（C1 C2 C5 T1 T2 T3 T4 T12 L1–L7 S12）

日期：2026-08-08
范围：`internal/tools/`、`internal/agent/orchestrator/`、`internal/loopguard/`、`internal/task/work/`

本轮不是实现，是**实测**：把这些能力当真软件跑起来，看它们是否真的在工作。
方法是「先跑再读码」——每条能力先设计一个「坏了就会失败」的真实运行，跑它，看输出。

---

## 结论矩阵

| ID | 能力 | 结论 | 证据 |
|----|------|------|------|
| T1 | LSP 导航（按符号名） | OK 实测通过 | 真 gopls 子进程，断言行号 |
| T2 | AST 结构化搜索 | **FIXED 发现并修复 1 处真缺陷** | 真 ast-grep 二进制；零匹配被当成错误 |
| T3 | 长耗时转后台 | OK 实测通过 | 真 `sh` 子进程活过前台超时，落盘为证 |
| T4 | 工具结果即时降级 | OK 实测通过 | 真 turn 里 100×10KiB，非单条巨型 |
| T12 | tool_batch | OK 实测通过 | 真 fs_write；被拒步骤的**文件不存在** |
| L1 | 重复检测 | OK 实测通过 | 真 config 文件 → 真 turn |
| L2 | 工具调用预算 | OK 实测通过 | 真 config 文件 → 磁盘上的文件数 |
| L3 | wall-clock 超时 | OK 实测通过 | **真实时钟**（原测试注入时钟） |
| L4 | token 预算 | OK 实测通过 | 已有 E2E 覆盖，含累计 prompt 反例 |
| L5 | 续跑不重放 | OK 实测通过 | 真文件行数；**附反向对照** |
| L7 | 清单闸 | OK 实测通过 | 真命令退出码 + 真文件 |
| C1+C2 | 写穿 + 历史召回 | OK 实测通过（读侧） | 真 SQLite；跨会话隔离 |
| C3 | 驱逐地图 | OK 实测通过 | 已有 e2e：从**渲染文本**里取 seq 再回查 |
| C5 | 压力折叠 | NOTRUN（不在可写范围） | 落在 `internal/ctxcompact`，T4 半边已测 |
| S12 | 沙箱违规升级 | OK 实测通过 | 四条路径 + **新增挂起回调**用例 |

---

## FIXED：T2 —— 零匹配被当成工具故障

### 缺陷

`ast-grep` 遵循 grep 退出码约定：**0 = 找到，1 = 没找到，>1 = 出错**。
`runAstSearch` 把任何非零退出都当作失败。于是**每一次零匹配的结构化查询**都返回错误。

这不是边角情况，而是这个工具**最常见的成功结局**：
「这个代码库里有没有吞掉 error 的分支」——回答「没有」是正确且可行动的答案。

修前（真实输出，`TestAstSearchReal_NoMatchesIsAnEmptyResult`）：

```
✗ ast_search: ast-grep exited 1: []
```

模型读到这个只会认为工具坏了，于是重试同一查询，或退回 `fs_search` ——
**能力静默退化成它本要取代的东西，恰好在它工作正常的时候。**

### 为什么单元测试全绿

现有 16 个 `ast_search` 测试全部替换 `secureCommandRunner` 返回罐头 JSON。
那张 exit code 表里，作者选的每个「零匹配」样本 `ExitCode` 都是 **0** ——
测试自己造的退出码，自然符合测试自己的预期。**没有任何测试跑过真二进制。**

实测得到的真实退出码矩阵（ast-grep 0.45.1）：

```
func zzz() { $$$ }             exit=1 stdout=[]        # 零匹配
if err != nil {                exit=0 stdout=[]        # 解析告警走 stderr，退出 0
$X                             exit=0 stdout=[{...}]   # 有匹配
if err != nil { }              exit=0 stdout=[{...}]   # 有匹配
--lang nosuchlang              exit=2                  # 用法错误
```

### 修复

`internal/tools/fs_astsearch.go`：新增 `astGrepFoundNothing`，三个信号**同时**成立才算「搜索成功但零匹配」：

- 退出码恰为 1（用法错误是 2）
- stdout 为空或 `[]`
- **stderr 为空**

第三条是第二次迭代加的，而且是被仓库**既有测试逼出来的**：初版只查退出码 + stdout，
立刻打红了 `TestAstSearch_ModelFixableFailuresAreResults` 里两条 `exit:1 + stderr:"error: ..."` 的用例。
这正是想要的方向——ast-grep 把**无法解析的 pattern** 报成 stderr 警告，
若只看退出码就会把「查询是坏的」翻译成「没找到」，
**把故障变成一个自信的、错误的否定答案，比原缺陷更糟。**

修后：

```
--- PASS: TestAstSearchReal_NoMatchesIsAnEmptyResult
```

### 留下的测试

- `internal/tools/fs_astsearch_real_test.go`（真二进制，无 ast-grep 时 skip）
- `internal/tools/fs_astsearch_test.go`：新增 `TestAstSearch_ExitOneWithEmptyArrayIsNoMatches`
  与 `TestAstSearch_ExitOneWithRealOutputIsStillAFailure`（后者两个方向：stdout 带诊断、stderr 带警告）。
  **刻意放在 fixture 测试旁边**，因为它必须在没有 ast-grep 的 CI runner 上跑——
  那正是回归会重新出现而无人察觉的地方。

---

## 顺带发现：ast_search 的真实路径从未被任何测试执行过

写第一版真二进制测试时，五个用例里四个立刻失败：

```
✗ ast_search: secproc: no Factory in context (fail-closed)
```

`ast_search` 经 `secproc` 拉子进程，而 `secproc` 是 fail-closed 的。
所有既有测试整体替换了 runner，所以**这条真实路径唯一不可或缺的 context 值，
在此之前没有任何 ast_search 测试绑定过**。
这不是产品缺陷（组合根绑得好好的），但它说明那 16 个测试离「工具能用」有多远。

---

## 各条实测细节

### T1 LSP 导航 —— OK

装了 `gopls`（`/Users/ll/go/bin/gopls`）。新增 `internal/tools/lsp_nav_gopls_test.go`，
对真 gopls 子进程跑 `lsp_definition` / `lsp_references` / `lsp_hover` / `lsp_symbols`。

**重点验证「按符号名」入口**（模型手里只有名字没有行列号，这条不通整个工具没人能用）：
`{"symbol":"Greeter"}` → 断言 `greeter.go:4`，**断言具体行号而不只是「有结果」**——
一个把所有符号解析到第 1 行的实现能满足「形状」断言却对模型毫无用处。

`lsp_references` 额外做**否定断言**：fixture 第 3 行的 doc 注释里含 "Greeter"，
正则会命中，语言服务器不得命中。这是「这确实是索引而非文本搜索」的判据。

写测试时踩到一次自己的坑：断言 gopls 把方法命名为 `Hello` 或 `Greeter.Hello`，
实测是 `(Greeter).Hello`。这是**测试 bug 不是产品 bug**，改成匹配方法名 + 声明行号，
避免把测试变成 gopls 版本探测器。

### T3 长耗时转后台 —— OK

新增 `internal/tools/background_real_test.go`，真 `sh` 脚本睡过前台超时后写标记文件。
三条断言按顺序排除三种「空心」可能：

1. 到点返回句柄而非错误（0.7s 超时，实测未阻塞）
2. **那一刻标记文件不存在**——否则命令是在超时内跑完的，这测试什么也没证明
3. 之后标记出现——进程真的活过了 turn

外加 `TestOffloadReal_CloseKillsSurvivingRuns`：故意脱离 turn 的进程，
必须随宿主进程一起死，否则同一机制就成了子进程泄漏。

### T12 tool_batch —— OK（攻击性测试）

新增 `internal/tools/toolbatch_real_test.go`。既有 batch 测试用 echo 工具往 slice 里追加，
那能钉住控制流，但**slice 条目和磁盘文件不是同一种证据**：
一个「授权正确、记录了拒绝、报告了干净停止，而被拒步骤的文件已经写下去了」的实现，
能满足它们每一条。

所以这里用真 `fs_write`，断言是**文件系统事后的状态**：

- profile 拒绝的步骤：`denied.txt` **不存在**，`after.txt` **不存在**（整批停下）
- 只允许 `tool_batch` 的 profile：两个文件都不存在（批处理本身不授予任何权限）
- 路径越狱：写到 work root 之外**没有发生**
- 链式引用：第 2 步真的拿到第 1 步的输出（断言文件内容含 marker，且不含字面量 `$1`）

### L1/L2 全栈 —— OK

新增 `internal/bootstrap/loopguard_config_test.go`：**真 YAML 文件** → 真 `bootstrap.Build` → 真 turn。

orchestrator 包已有 E2E，但它在 Go 里构造 `orchestrator.Config`，留下两个未验证环节：
① 操作员写的 YAML 键是否真的到达 gate 读的那个字段；② 被拒的调用是否**在副作用之前**被拒。
所以这里模型请求写 5 个文件、预算给 2，断言是**磁盘上剩几个文件**。

**做了变异探针**确认测试不是假绿：把 `MaxToolCalls` 映射改成常量 0，
`TestLoopGuardConfigReachesARealTurn_TotalToolBudget` 当场变红，其余不动。已还原。

同时留了**零配置对照**（不写 `loop_guard` 块 → 5 个文件全部落盘），
否则一个无条件安装 gate 的实现也能通过前两条。

### L3 真实时钟 —— OK

既有 `TestLoopGuardDeadlineStopsAtBoundary` 注入时钟，这对钉「检查发生在哪」是对的，
但**注意不到一个接在测试专用时钟上的 guard**：`turnGuard.now` 由 `WithLoopGuard` 从 `time.Now` 填，
若哪次编辑在那个位置留了测试缝，所有注入时钟的测试照绿，而生产没有任何 turn 会超时。

新增 `TestLoopGuardE2E_DeadlineStopsOnTheRealClock`：真 250ms 限额 + 真会睡的工具，
MaxIters=200，实测 0.52s 停下。

### L5 续跑不重放 —— OK

新增 `internal/agent/orchestrator/continuation_real_test.go`。
工具是**追加**而非覆写（幂等的 `fs_write` 执行一次和两次留下同样内容，正是要排除的歧义）。
断言是文件行数：续跑后必须仍是 1 行。

**附反向对照** `TestL5Real_FullReplayDoesReExecute`：从 turn 的**起始历史**重放（L5 之前的行为），
断言必须是 2 行。没有它，上面那条在 fixture 不再复现危害时会静默变成空断言。

### L7 清单闸 —— OK

新增 `internal/tools/gate_chain_real_test.go`，跑完整链条：
真 `task_gate_run` 起子进程 → 真退出码 → 持久化为 Evidence → gate 决策。

命令条件跑两次真命令（`false` 然后 `true`），Finish 必须先被拒后被放行，
中间**只有两个真实进程的退出码**发生变化。
文件条件同理：造真文件前被拒、造后放行，任务本身不变。
外加信任边界：模型自己把条件项标成 done、而条件未满足时，Finish 仍必须被拒，
且**修正后的状态要落库**（操作员事后要能查「我为什么被挡」）。

两处是**我的测试写错**、不是产品缺陷，记下来避免下次重复：
① 生命周期是 pending → running → completed，直接从 pending Finish 会被状态机拒绝，**掩盖 gate 自己的判决**；
② 未知 task id 的拒绝是**工具 result 不是 Go error**（ADR-0001：Go error 会中断整个 turn）。
第 ② 条我先用探针把真实输出打出来才下结论，没有直接改断言。

### C1+C2 联合 —— OK（读侧）

写侧 `persistMessages` 在 `internal/api/http`（不在我可写范围）。
读侧新增 `internal/tools/history_c1c2_test.go`：把**真 ReAct turn 形状**的 eino 历史，
经与 WS transport 相同的转换写入真 SQLite，再用真 `history_search` / `history_read` 取回。

marker 字符串**只出现在工具结果里**，所以搜到它只可能是读了 tool 行——
C1 之前那些行根本没被写过，这条断言就是当初能抓到它的那条。
另外钉住 system prompt 不得入库，以及**跨会话隔离**（session id 来自 context 从不来自参数）。

一处我的断言过严：`history_search` 会用 `«»` 包裹命中词，
剥掉装饰再比对文本——穿过装饰断言会让每次改高亮样式都打红一堆与召回无关的测试。

### S12 沙箱违规升级 —— OK

四条路径既有测试全绿（批准重试 / 拒绝 / 超时 / 无 callback），
其中超时那条用**穷举**形式钉住了整个 `PermissionDecision` 词表。

补了一条更硬的：`TestEscalationHangingCallbackNeverEscalates`。
既有超时测试覆盖的是 S5 **返回**的形状（现实情况——`awaitDecision` 会先把过期转成 decision），
但它只能证明「返回 deny 被正确处理」，**证明不了循环里没有「超时就继续」分支**——
一个立刻回答的 callback 从不会走到等待逻辑。

新测试让 callback 一直阻塞到 turn 被取消才返回零值 decision。
断言落在**档位**上：升级是提权，唯一必须「没有人点头就不可达」的结局，就是在更宽的 tier 上重试。

---

## NOTRUN

- **C5 压力折叠**：`ctxcompact.FoldToolResults` 落在 `internal/ctxcompact`，本轮不在我可写范围。
  退而求其次：验证了同一场景的另一半 **T4**（`internal/tools/spilldegrade.go` +
  orchestrator 的 `degradeHistory`），用的正是任务要求的形状——**100 条各 10KiB，不是单条巨型**
  （单条巨型走的是 64KiB spillover 老路径，测它等于没测新能力）。
  三条断言：总量真的降了、最近 N 条**没有**被折叠、被折叠的能通过恢复指针取回真实原文。
- **L6 可插拔停止条件框架**：无独立行为，由 L1–L4 的实测间接覆盖。

---

## 并行 agent 的在途编译错误（非本人造成，未修）

跑测试期间三次撞到邻居包的半成品，均确认非我改动、等待后自行恢复，未顺手修：

- `internal/llm/eino/retryafter.go:194` `undefined: errors`（M1/M2 在途）
- `internal/vcs/crossproc.go` 多处 undefined（V8 在途）
- `internal/shell/procfactory.go:82` `f.posture undefined`

---

## 留下的可重跑测试

全部无外部付费依赖；需要外部二进制的用 `t.Skip` 而非失败。

| 文件 | 覆盖 | CI 可跑 |
|------|------|---------|
| `internal/tools/lsp_nav_gopls_test.go` | T1 真 gopls | 是（无 gopls 则 skip）|
| `internal/tools/fs_astsearch_real_test.go` | T2 真 ast-grep | 是（无 ast-grep 则 skip）|
| `internal/tools/fs_astsearch_test.go`（新增 2 条） | T2 退出码回归 | **是（无需外部二进制）** |
| `internal/tools/background_real_test.go` | T3 真子进程 | 是（Windows skip）|
| `internal/tools/toolbatch_real_test.go` | T12 真副作用 | 是 |
| `internal/tools/gate_chain_real_test.go` | L7 真命令+真文件 | 是（Windows skip）|
| `internal/tools/history_c1c2_test.go` | C1+C2 联合 | 是 |
| `internal/tools/sandboxescalate_test.go`（新增 1 条） | S12 挂起回调 | 是 |
| `internal/agent/orchestrator/continuation_real_test.go` | L5 + 反向对照 | 是 |
| `internal/agent/orchestrator/degrade_real_test.go` | T4 100×10KiB | 是 |
| `internal/agent/orchestrator/loopguard_test.go`（新增 1 条） | L3 真实时钟 | 是 |
| `internal/bootstrap/loopguard_config_test.go` | L1/L2 全栈 | 是 |

---

## 门禁

```
go build ./...                     OK
go test ./internal/tools           ok  31.355s
go test ./internal/agent/orchestrator  ok  26.816s
go test ./internal/loopguard       ok
go test ./internal/task/work       ok
go test ./internal/archtest        ok   8.301s
go test ./internal/bootstrap       ok  25.957s
```

---

## 方法论小结（值得记住的两条）

1. **罐头 fixture 测不出外部工具的约定。** T2 的缺陷不在解析、不在截断、不在鉴权——
   那三样各有充分测试。它在**退出码的语义**上，而那是外部程序的约定，
   任何由测试自己填写 `ExitCode` 的 fixture 都只会复述测试作者的假设。
   判据：这个测试里有没有哪个数字，是「被测系统的对手方」决定的？如果有，就必须跑真的。

2. **副作用要用副作用来断言。** T12、L5、L2、L7 四条我都刻意把断言落在
   「磁盘上有什么」而不是「报告里写了什么」。
   报告记录的是**说了什么**，文件记录的是**发生了什么**，
   而这些能力的全部价值恰好在于让这两者一致。
