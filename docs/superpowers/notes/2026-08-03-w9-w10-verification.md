# W9 / W10 核验报告（合并）

> 2026-08-03 实测。所有行号与计数为亲自核对的当前值。

---

# W9 对外契约

## 1. 两份 schema 的实测 `$defs`

```
python3 -c "
import json
for v in ['v1','v1.1']:
    d=json.load(open(f'sdk/schema/{v}/agent-api.schema.json'))
    print(v, len(d.get('\$defs',{})), sorted(d.get('\$defs',{})))"
```

- `sdk/schema/v1/agent-api.schema.json` → **21**
- `sdk/schema/v1.1/agent-api.schema.json` → **1**（只有 `Item`）

v1.1 缺的 20 个：`ApiErrorBody`、`Capabilities`、`ContextItem`、`FileChange`、`ImageAttach`、`InterruptResponse`、`ItemType`、`Range`、`Thread`、`ThreadInterruptParams`、`ThreadResumeParams`、`ThreadResumeResponse`、`ThreadStartParams`、`ThreadStartResponse`、`ThreadStatus`、`Turn`、`TurnStartParams`、`TurnStartResponse`、`TurnStatus`、`Version`。

### ⚠️ 但「缺」这个词是误导的 —— 本次核验最重要的发现

**v1.1 不是 v1 的退化副本，它是一个 `allOf: [{"$ref": "../v1/agent-api.schema.json"}]` 覆盖层** —— 结构上它*已经*是「v1 + delta」。它自己的 1 个 `$defs.Item`（加 `reasoningTokens`，`minimum: 0`）就是那个 delta。

**而这个 delta 是死的。** Ajv 实测（在 `sdk/ts/` 内跑）：注册 v1 后编译 v1.1 成功，然后喂一个 `reasoningTokens: -5` 的 item —— **验证通过（返回 true，errors 为 null）**。

原因：v1 根节点的 `anyOf` 引用 `#/$defs/Item`，该指针在 v1 自己的 `$id` scope 内解析，**永远解析不到 v1.1 的本地 `$defs.Item`**。所以 v1.1 的覆盖层从未被任何求值路径触及。

v1.1 的 `description` 自述：「Fixture-only v1.1 schema. D1 currently emits only 'v1' … **Not served by D1**」。仓库里唯一的 schema 端点是 `GET /api/v1/schema/agent-v1.json`（`internal/api/http/agent_v1.go`），它吐的是 `internal/api/v1.SchemaBytes()`，即**第三份** schema。

### 真正的漂移是四路的，不是两路的

实测矩阵（比对 `internal/api/v1/types.go` 的 struct tag、`sdk/schema/v1` 的 `properties`、`sdk/ts/v1.ts` 的 interface、`sdk/python/.../generated.py` 的字段+alias）：

| 轴 | 实测差异 |
|---|---|
| 运行时 `schema.go` vs `sdk/schema/v1` | **3 vs 21** $defs（运行时只有 Thread/Turn/Item） |
| Go vs `sdk/schema/v1` | `Item` 多 `fileChange`；`TurnStartParams` 多 `context`；`ThreadSnapshot` 在 schema 中不存在；9 个 $defs 无 Go 对应物 |
| Go vs `sdk/ts/v1.ts` | ⚠️ **TS 缺 `TurnStartParams.images`**；缺 `ThreadSnapshot` |
| Go vs Python | Python 有 `context` 但 ⚠️ **缺 `images`**；`Item` 多 `fileChange` |

其中 `ContextItem`/`FileChange`/`Range` 是**有意的**前瞻字段，`sdk/schema/CONTRACT_HANDOFF.md:37-49` 明确记录（D2 附加、D1 忽略未知字段）。**`images` 缺失是无意的真漂移。**

## 2. 现有 contract 测试实测

**`sdk/ts/tests/contract.test.ts`** —— 唯一的 schema 加载是 `const schemaRoot = new URL("../../schema/v1/", ...)`，即**只加载 v1**。6 个断言：全部 response fixture 过 v1 校验、`items.jsonl` 每条过校验、sequence 恰为 `[1..7]`、`structured.result` 深等、未知 item type 保留、坏 sequence/空 threadId/`version:"v2"` 被拒。

> 行号说明：读的是文件内容，未另跑带行号输出，所以「:15-16」这个具体行号不背书；能背书的是**全文只有这一处 schema 加载，且路径是 `schema/v1/`**。

**`sdk/ts/tests/version-matrix.test.ts`** —— 7 个测试，全部走 `AgentClient` + 假 `fetch`，**只操作 `X-Yanshi-API-Version` 头和 body 里的 `version` 字符串**：v1 接受、v1.1 接受（并保留 `futureField`）、`v2`/`undefined`/`garbage`/`v1.` 抛 `ApiVersionError`、未知字段容忍。**它从不读取任何 schema 文件。**

**确认：全仓没有任何测试比较两份 schema 的内容。** grep 了 `sdk/ts/tests/`、`sdk/python/tests/`、全部 Go `*_test.go`，没有任何一处同时读入 v1 与 v1.1、或比较 `$defs` 集合。

补充实测：`cd sdk/ts && npx vitest run` → **4 files / 29 tests 全过**（286ms）。**现状不是「测试红了没人管」，而是「测试压根没覆盖这条轴」。**

## 3. schema parity 方案：**B**（保持分离 + 枚举有意差异的 parity 测试）

1. **A 在结构上已经实现了** —— v1.1 就是 `allOf` + `$ref` 覆盖层 —— 而它照样出了缺陷（覆盖层求值不到，delta 死掉），说明「结构上不可能漂移」这个承诺靠 `$ref` 拿不到，**A 解决不了实际发生的那个 bug**
2. **C（折回 v1）会删掉唯一一份「未来 minor 长什么样」的可执行样本**，而 `version-matrix.test.ts` 的 v1.1 宽容度断言恰恰需要这个形状存在才有意义
3. **真正的漂移轴不是 v1↔v1.1，而是 Go types ↔ sdk/schema/v1 ↔ TS ↔ Python 这个四路轴**，B 是唯一能把「`images` 少了」和「`fileChange` 是故意的」区分开的方案 —— 它要求每条差异有名字和理由
4. B 的有意差异表天然是债务计数器，与 W0 已确立的「豁免表只减不增 + 死条目失败」语义同款
5. ⚠️ **落点应在 Go 侧**（`internal/api/v1` 的一条 parity 测试）而非 TS 侧，因为 `go test ./...` 是无条件硬跑的，而 Node/Python 工具链在 CI 里是可选步骤 —— 这样 parity 失败在没装 Node 的机器上也会红

对 v1.1 那条死覆盖层，B 的处置是：把它作为一条**具名**差异写进表里，并附一条断言「v1.1 的 `$defs.Item` 必须真正参与求值」（用 `reasoningTokens: -5` 必须被拒作为断言）—— **逼它要么修好（改成 `properties.reasoningTokens` 直挂或调整 `$ref` scope），要么删掉**。

## 4. V14 `divergent` 的具体内容与处置

审计列的三条偏差，逐条复核**全部成立**：

1. **两份 schema 并存且详略不同** —— `internal/api/v1/schema.go` 的 `schemaDocument` 只有 3 个 `$defs`（Thread/Turn/Item），`$id` 是 `https://yanshi.dev/schema/agent-api-v1.json`；`sdk/schema/v1` 有 21 个，`$id` 是 `.../agent-api/v1/agent-api.schema.json`。⚠️ **两个 `$id` 不同 —— 它们是两份自称不同身份的文档，不是同一份的两个副本。** 且 `schema.go:13` 的注释仍写着「Task 9 expands this document (params/response shapes, item type enum)」，即原设计的扩展从未落到运行时
2. **`TurnStartParams.Images` 是死字段** —— `types.go:139` 声明，`types_test.go:49` 有 camelCase 测试，但 grep `internal/api/v1/` + `internal/appserver/` 全部非测试代码，`Images` 的命中**只有 types.go:139 那一行本身**。`service.go:313` 构造 `TurnOpts` 时只填 `ThinkingEffort` 和 `OutputSchema`。且 `orchestrator.ApplyImages`（`multimodal.go:16`）**全仓零非测试调用点**
3. **`ThreadSnapshot.Items` 永远为空** —— `service.go:286` 是 `return ThreadSnapshot{Version: Version, Thread: thread}`，从不设 `Items`；而 `agent_v1.go` 的 resume handler 与 `appserver/server.go` 的 dispatch **都在转发 `snapshot.Items`**，`types.go:101/154` 都声明了它。**客户端拿到的永远是缺省省略的空数组**

**选择：主要收敛到设计，一处碰不得，一处接受偏离。**

1. 第 1 条（两份 schema）**收敛** —— 让运行时 `schemaDocument` 与 `sdk/schema/v1` 统一到一个真相源，并删掉 `schema.go:13` 那句假注释；不统一的话 §3 的 parity 测试没有可锚定的基准
2. ⚠️ 第 2 条（`Images`）**W9 不做** —— spec §4.3 明确把「`ApplyImages` 接进 turn 路径、`TurnOpts` 加图像字段」划给 **W1**，W9 去做就是重复投入；W9 只负责在 parity 表里把「TS/Python 缺 `images`」记为**待 W1 关闭**的具名差异
3. 第 3 条（`Items`）**收敛** —— 要么真填、要么从 wire 契约删掉；**一个两条传输都在转发的永空字段是对外契约的谎报**
4. `ContextItem`/`FileChange`/`Range`（Go 无对应物）**接受偏离** —— `CONTRACT_HANDOFF.md:37-49` 记录了它们是 D2 前瞻字段、D1 有意忽略
5. 所以验收标准需要重写的只是第 4 条那一小块 —— 「有版本+Schema」应改为「**单一** schema 真相源 + parity 测试守门」

## 5. 其余 4 项 delta

**`D1/APS1`** —— JSON-RPC 2.0 dispatch 真实且已接线（`internal/appserver/server.go` 的 `dispatch` 覆盖 initialize/capabilities/thread/start|resume|interrupt/turn/start|interrupt/config/read|write/shutdown，标准错误码 `rpc.go:15-21`），与 HTTP 共用同一 `*v1.Service`；**缺口**：⚠️ `cmd/yanshi/app.go:46` **无条件** `appserver.NewMemoryConfig()`（即使传了 `-config` 也一样），所以 `config/read|write` 永不落盘，而 `docs/api/jsonrpc.md` 把它们描述成「读/写运行时配置」；且没有 HTTP↔JSON-RPC 的跨传输行为一致性对照测试。
→ ✅ **秘密路径拒绝仍完好并有测试**：`config.go:15-20` 的 `secretPathFragments`（token/api_key/apikey/secret）+ `validateConfigKey:73` 逐 dot-segment 小写比对 + `password` 子串匹配，读写两侧都在 decode **之前**拒绝；`config_test.go` 在位（95 行）。**这条不能动。**

**`D1/V12`** —— stdin 三模式解析真实（`internal/cli/headless_input.go:37 ReadHeadlessInputs`，text/lines/jsonl，1MiB 行上限，jsonl 逐行带行号报错），resume 只在第 0 条生效；**缺口**：⚠️ `cmd/yanshi/headless.go` 的 `--file` 分支把整个文件当**一条** prompt（`inputs = []cli.HeadlessInput{{Prompt: strings.TrimSpace(string(data))}}`），**完全绕过 `cfg.Input` 模式、从不调用 `ReadHeadlessInputs`**，所以 `--input jsonl --file <3行文件>` 只跑 1 个 turn 而非 3；`docs.yml:105` 的 CI smoke 跑的正是这条命令但**输出丢给 `/dev/null`、零断言**，所以掩盖了它。
→ ✅ **退出码无需改动**：`mapExecError`（`main.go:1014-1025`）是 nil→0 / DeadlineExceeded→124 / Canceled→130 / 其余→1，加 flag 解析失败→2。修 `--file` 是纯输入路径改动，**不触碰 0/1/2/124/130**。

**`D2/V15`** —— TS/Python 客户端主流程真实可跑（`AgentClient` 的 start/resume/interrupt/cancel/run 都在，TS 29 测试本地全过）；**缺口**：⚠️ **「类型生成」是伪生成器** —— `cmd/api-schema/main.go` 里从 `text := \`...\`` 到函数末尾是**一整段硬编码 Go 原始字符串字面量**，逐字符抄写 TS 接口，**从不解析 `internal/api/v1` 的任何东西**；唯一接触点是 `main.go:176` 的 `_ = v1.SchemaBytes()`，**返回值被丢弃**，注释自称「guards the generator against silent drift」但 `_ =` 语义上不可能检测任何东西。实测 `go run ./cmd/api-schema | diff - sdk/ts/v1.ts` → **IDENTICAL，但这是循环自证**（生成器和产物是同一段字面量）。Python 侧 `generated.py:1-16` 自述「Hand-mirrored … D2 maintains the Python mirror by hand」，`pyproject.toml` 的 `[generate]` extra 里 `datamodel-code-generator` **从未被任何脚本调用**。

**`H2/APIREF1`** —— 生成区块真实（`docs/api/resources.md` 有 11 个 `api-defs:*` 标记，`cmd/api-schema/markdown.go` 的 `runMarkdown` 是真实幂等实现）；**缺口**：① ⚠️ `markdown.go` 的 `paramResponseDefs()` 是**手工维护表**（doc 自己承认「hand-maintained field map … mirrors the hand-written TS interfaces in main.go」），所以 resources.md 的 `images` 行是手写的、**与 TS 缺 `images` 的事实并存而无人报警**；② `docs/api/schema.md` 头部（`schemaDocHeader`）**自相矛盾** —— 同时声称是「`sdk/schema/v1/agent-api.schema.json` 的完整 JSON Schema」**和**「从 `internal/api/v1/schemaDocument` 生成」，而后者只有 3 个 $defs；③ ⚠️ `docs/api/sdk-python.md` 的示例用 `item.toolName`，但 Python 属性名是 `tool_name`、`toolName` 只是 pydantic alias（`generated.py:121`），**属性访问必抛 `AttributeError`** —— 与 `examples/sdk-python/main.py:25` 是同一个 bug；④ `jsonrpc.md` 对 `config/read|write` 的描述与 APS1 的 in-memory 现实不符。

## 6. SDK 测试的 CI 现状

**确认：`sdk/ts` 与 `sdk/python` 的测试套件当前没有任何 workflow 在跑。**

`grep -rn 'sdk\|pytest\|vitest\|npm\|python' .github/workflows/` 的全部命中只落在 `docs.yml`：
- `docs.yml:98-100` —— `npm --prefix sdk/ts install` + `npx tsc --noEmit`。**只 typecheck，不跑 vitest**
- `docs.yml:104-106` —— `py_compile` + `pip install -e sdk/python` + `import yanshi_sdk`。**只 import smoke，不跑 pytest**

全仓 workflow 里 **`vitest` 与 `pytest` 零命中**。

⚠️ **额外发现**：`docs.yml` 的 `paths` 过滤器**没有 `sdk/**`**。所以只改 `sdk/` 时，**连现有的 typecheck 都不会触发**。这比「测试没跑」更严重一层。

`scripts/check-d2.sh` 已确认不存在（W0 `994dfd6` 删除，`removal_test.go` 断言它和 `ide/vscode/` 都不得复现）。

**新入口计划**：在 `ci.yml` 加 `sdk-contract` job（**不放 docs.yml** —— 它是 paths-filtered 的，会漏掉 sdk-only 改动；ci.yml 无 paths 过滤，PR 必跑）。Node 20 + Python 3.11，`npm --prefix sdk/ts ci` → `vitest run`，`pip install -e 'sdk/python[test]'` → `pytest sdk/python`，外加 `docs.yml` 的 `paths` 补 `sdk/**`。**并且 parity 断言本体落在 Go 侧**，进 `go test ./...` 这个无条件硬门禁 —— 即使某天 `sdk-contract` 被 Node 环境问题卡住，漂移仍会红。

本地实测：`npx vitest run` → 4 files / 29 tests 全过；`sdk/python` 的 pytest **本机跑不了**（`import jsonschema` → `ModuleNotFoundError`，`[test]` extra 未安装）—— **这本身就是「没人在跑」的旁证**。

## 7. 建议加 archtest 断言守护 v1 层不变式，**但不能挂在 `portAllowlists` 上**

`internal/agent/orchestrator` 的非测试导入者实测**恰好 6 个**：`internal/agent/spawn/spawn.go`、`internal/api/http/chat.go`、`internal/api/http/ws.go`、`internal/api/http/ws_compaction.go`、`internal/api/v1/service.go`、`internal/bootstrap/bootstrap.go`。**小到今天就能钉成一张白名单**，之后任何新前端/新渠道直连 orchestrator 会立刻红。

⚠️ **方向性说明**：`portAllowlists` 是 `port包 → 允许的依赖` 映射，方向是「谁能被这个包依赖」；而这里需要的是**反向**断言「谁能依赖这个服务层包」。`deps_test.go` 现有的 `serviceLayerPrefixes` + `isServiceLayer`（R5：ports 不得导入 service layer）是最接近的邻居，但语义也不同。**所以需要一张新的小表（`orchestratorImporters`），复用 `buildImportGraph` helper，不复用 `portAllowlists`。**

已知的、**刻意保留**的两条：`ws.go` 与 `v1/service.go` 各自构造 `TurnOpts` 并调 `EventsWithHistoryOpts`（spec §4.3 明确说收敛点在 **S3**，**W9 不得动**）。这两条进白名单并标注 S3。

## 8. 审计 / spec 的过时与错误

1. ⚠️ **spec §4.3 W9 那行「v1 有 21 个 `$defs`，v1.1 现只剩 1 个」的数字对、框架错。** 「剩」字暗示 v1.1 是一份正在退化的副本；实际它是 `allOf` + `$ref` 覆盖层。**按这个框架去修（补齐 20 个 $defs）会把一个正确的结构改坏。** 真实缺陷是覆盖层**求值不到**
2. **「两份 divergent schema 正在发给客户端」被 v1.1 自己的 description 否证** —— 「Not served by D1」，且唯一 schema 端点吐的是**第三份**（运行时 3-$defs 文档），而它恰恰是最贫瘠的
3. ⚠️ **「v1.1 审计时是 3、现在是 1，所以缺口在扩大」这个论断无法证实，且怀疑是串行。** 审计 V14 条目里出现的「3 个 $defs」明确指 **`internal/api/v1/schema.go` 的 Thread/Turn/Item**，不是 v1.1。审计全文没有给出 v1.1 的 $defs 计数。**不要把「缺口在扩大」写进任何计划的论证里** —— 它缺证据
4. **审计 V14 evidence 的 `bootstrap.go` 行号已漂移**（引 `:820 NewService`、`:945 srv.AgentV1`、`:996 AgentAPI`；实测 `Models:` 现在在 854 与 1022）—— W0 加了 `App.ToolNames` 与 `DefaultOrchestratorProfile()`。不影响判定（`status_test.go` 按设计不校验行号），但复核者按行号找会找错
5. **审计中「把 `check-d2.sh` 接进 CI」已作废**
6. **`docs/api/schema.md` 的生成头部自相矛盾**（`schemaDocHeader`）—— 本次新查出；是第 4 节第 1 条那个「两份 schema」问题在文档层的投影
7. ⚠️ **`docs.yml` 的 `paths` 缺 `sdk/**`** —— 审计与 spec 都未记录。审计在 W10 名下记了「补 `cmd/yanshi/**`」，同一个列表还缺 `sdk/**`，**建议一并处理**
8. **`markdown.go:paramResponseDefs()` 是手工表这件事，审计判 APIREF1「无差异」时低估了** —— 它正是 `images` 字段在 resources.md 里出现的来源，**一个手工表在替一个不存在于 TS/Python 的字段作证**

---

# W10 发布就绪

## 1. 实测覆盖率

```
go test -cover ./internal/proto/... ./internal/bootstrap/... ./internal/store/...
```

| 包 | 实测 | spec 目标 |
|---|---:|---:|
| `internal/proto` | **100.0%** | 80% |
| `internal/bootstrap` | **94.1%** | 50% |
| `internal/store` | **95.5%** | 75% |

与审计（95.7/100/94.2）基本一致，W0 之后 bootstrap 94.2→94.1、store 95.7→95.5（自然漂移，非退化）。

**三个数全部远超目标 → COV2/COV3 的「覆盖率」子条件今天就满足，缺的只有门禁。**

⚠️ **审计里 COV3 标题写的「23%」是陈旧数字**，可溯源到 `docs/archive/synthesis-report-v2.md:343`（2026-07 快照）。同理 COV2 的「67%」实测 100%。**两项的真实缺口不是覆盖率，而是测试语义**（见 §12）。

## 2. PKG1 —— 已修，且 `goreleaser check` 已进 CI

- `.goreleaser.yaml` **没有 `changelog:` 段了**。取而代之是一段 8 行注释，明确解释「不写 changelog 段」与「不能写 `changelog.disable: true`」的区别：disable 检查在 pipe 的 `Skip()` 里而非 `Run()` 里，禁用会连带跳过 `--release-notes` 的加载，**发出空 body 的 Release**
- 版本已 pin：`release.yml` 与 `ci.yml` 都是 `version: "~> v2.7"`（下限理由是 `archives.format_overrides` 用复数 `formats` 键）
- ✅ **`ci.yml` 有 `release-config` job（硬门禁）跑 `goreleaser-action@v6 … args: check`**，注释写明「配置错误应在 PR 上暴露，而不是在 tag push 那一刻」。**审计的建议已经落地**

**PKG1 的「配置会炸」这一条已结案。** 剩余缺口是流水线从未真跑过（§8）+ 归档物缺 LICENSE（§10）。

## 3. usage 常量：**没有 `auth`，而且 `pr` 也没有**

- `cmd/yanshi/main.go:38 var usage = ...`，正文到 `:77`
- usage 的 Subcommands 段列出：`(none)`、`chat`、`exec`、`serve`、`app`、`goal`、`vcs-mcp`、`doctor` —— **8 项**
- 实际可分发（`main.go:110-136` 的 switch + `:245-256` 的 managed dispatcher）：`serve`、`chat`、`exec`、`app`、`goal`、`vcs-mcp`、**`pr`**（`:123`）、**`auth`**（`:249`）、`doctor` —— **9 项 + 裸调用**

⚠️ **是两个隐藏子命令，不是一个。审计只抓到 `auth`。**

旁证：`cmd/gendocs/help.go:30` 的 `yanshiSubcommands` 列全了 10 项（含 `pr`/`auth`），`gendocs_test.go` 也断言它与 main.go 的 switch 同步 —— 但**没有任何断言把 `usage` 文本和 dispatch 对起来**。所以 `entrypoints.md` 里 `pr`/`auth` 的 `-h` 快照是有的，唯独顶层 `yanshi -h` 看不到它们。

**这正是 W10 该补的那条断言（usage ↔ dispatch 双向一致），一条测试同时销掉两个隐藏命令。**

## 4. 覆盖率门禁现状

```
rg -n -- '-cover' .github/workflows/      → NO MATCH（零命中）
rg -n 'cover' internal/archtest/          → 仅 3 处散文命中
```

`internal/archtest/` 现有 8 个文件（assembly/ctxinject/deps/docs/helpers/lines/removal/status），**没有任何覆盖率断言**。

**全仓无覆盖率门禁，任何一处从 94% 掉到 20% 都不会红。**

## 5. 覆盖率强制方案：**CI job + `cmd/covercheck`，不放 archtest**

新建 dev 工具 `cmd/covercheck`（阈值表 + 解析 `coverage: NN.N% of statements`），本地一条命令 `go run ./cmd/covercheck` 可复现；`ci.yml` 加一个 `coverage` 硬门禁 job 调它。阈值取 `max(spec 验收值, 实测值-3pp)` → **proto 97 / store 92 / bootstrap 91**（既满足 spec 契约，又是真正的 ratchet，不给 44 个百分点的免费退化空间）。

**为什么不放 archtest**：
1. ⚠️ **递归与自指**：archtest 断言要 `go test -cover` 就得在 `go test` 里再起 `go test`；范围一旦写成 `./...` 直接自我递归，写死三个包又是另一张需维护的表，且 `-race` job 下会变成嵌套非 race 运行
2. **代价与重复**：`go test ./...` 跑在三平台，archtest 版本会把 bootstrap（11.3s）+ store（6.4s）的嵌套测试重复三遍；bootstrap 的测试会真起 sqlite 与 `127.0.0.1:0` 监听，**嵌套在另一个持有临时目录的 `go test` 里是端口/lockfile 争用的邀请函**
3. **类别不同**：GOV1-GOV6 全是对**源码结构**的静态断言（AST / go list），覆盖率是 `go test` 的**运行产物**，塞进同一张桌子会让「archtest = 结构治理」这个清晰边界糊掉；而 CI job 同样是机器强制的，并不比 archtest 弱

配套：`cmd/covercheck` 登记进 CLAUDE.md 的 dev 工具清单与 CONTRIBUTING。⚠️ 首次 CI 运行后按 **ubuntu 实测值**复核阈值（本次只在 macOS 测过）。

## 6. `docs.yml` 的 paths

push 与 pull_request 两处**完全相同**，均为 7+1 条：`docs/**`、`examples/**`、`cmd/api-schema/**`、`cmd/gendocs/**`、`internal/docgen/**`、`internal/config/**`、`internal/api/v1/**`、`.github/workflows/docs.yml`。

**不含 `cmd/yanshi/**`** —— 缺口成立。CLI 帮助快照的真相源是 `cmd/yanshi/{main,app,headless}.go` 里的 FlagSet（`rg -l NewFlagSet` 只命中这三个文件），**改 flag 文案的 PR 不会触发 docs job，`help:*` 快照静默漂移**。

⚠️ **第二个同类缺口**：**`sdk/**` 也不在 paths 里**，而 docs.yml 的最后两步（TS typecheck、`pip install -e sdk/python`）真实依赖它。改 `sdk/ts/v1.ts` 破坏 examples 同样不会触发。**建议一并补，两行。**

（顺带核实：四个生成器当前**无 diff** —— 跑了全部四条命令 + `git diff --stat docs/`，输出 DOCS-CLEAN。）

## 7. `nightly.yml:18` 的 `continue-on-error`

仍在。原文：`continue-on-error: true   # soft until E2 fuzz targets land`

**判断：该在 W10 收紧。** `go test -list 'Fuzz.*' ./internal/guard/...` 实测输出 `FuzzMatchGlob` —— **注释写的那个前提条件已经成立**，留着就是又一条「文档描述与现实不符」；而且 nightly 本来就不阻塞合并，`continue-on-error` 唯一作用就是把红色变成静默，**恰好抹掉 fuzz 这类「价值 100% 在告警」的 job 的全部价值**。

第 38 行的 bench 保持软 —— 它是 trend-only、`go test -bench=. ./...` 全树跑无 pass/fail 语义。

⚠️ **同时应一并处理两个「内部逃生口」**（同一失效模式，只是藏在 shell 里而不是 YAML 键上）：
- `nightly.yml` fuzz-long 步骤里的 `if ! go test -list … ; then echo "…soft-pass"; exit 0; fi`
- `ci.yml` fuzz-seed 步骤里同款的 `echo "no fuzz/property targets yet (E2 pending); soft-pass"; exit 0`

两处都意味着「**有人删光 fuzz/property 目标 → job 照样绿**」。E2 资产（`FuzzMatchGlob` + `internal/ctxcompact` 的 8 个 `TestProperty_*`）已实测存在，应改成「目标缺失即失败」。

## 8. release 流水线 dry-run

**没有跑。本机没有 goreleaser，也没有 git-cliff。**

```
which goreleaser → not found
which git-cliff  → not found
git tag | wc -l  → 0
git tag -l 'v[0-9]*' | wc -l → 0
```

⚠️ **本仓库连 m1..m9 里程碑 tag 都没有**（一个 tag 都没有）。流水线确实一次都没被触发过，`build.sh` 的「无 v* tag 回落」分支是当前唯一走过的路径。

**W10 必做**：
```bash
go install github.com/goreleaser/goreleaser/v2@v2.7.0     # 与 CI 同一 pin
goreleaser check
goreleaser release --snapshot --clean --skip=publish
# 验收（逐条肉眼核）：
#   ls dist/*.tar.gz dist/*.zip  → 恰好 4 个
#   ls dist/checksums.txt        → 存在且 4 行 sha256
#   tar tf 任一 tar.gz           → 含 yanshi + config.example.yaml + README.md
#   解包后 ./yanshi -h           → 退出 0（CGO_ENABLED=0 -tags=nokeyring 产物的唯一真实冒烟）
```
建议把 snapshot 加进 `nightly.yml`（+ `workflow_dispatch`），让「配置能解析」升级为「产物能构建」的每日守卫。

### ⚠️ 同时发现 release.yml 两处真实缺陷（属 VER1）

1. **`git-cliff --latest --output CHANGELOG.md`** —— `--latest` 只处理最新 tag 区间，`--output` 是覆盖写。第一次发布碰巧没事（无前序 tag = 全历史），**第二次发布会把 CHANGELOG.md 整个替换成只有 v2 那一节，v1 的历史丢失**。RELEASE_NOTES.md 用 `--latest` 是对的，**CHANGELOG.md 必须去掉 `--latest`**
2. **「commit changelog」步骤在 goreleaser 之前，且做 `git push origin HEAD:main`** —— tag push 时 checkout 是 detached HEAD；只要 main 已往前走过，这个 push 就是 non-fast-forward → 步骤失败 → **整个 Release 发不出去**。**changelog 回写绝不该阻断产物发布**：应移到 goreleaser 之后，并允许其失败

## 9. cliff.toml 与 S0 提交历史

历史规模：`git rev-list --count HEAD` = 32（压缩过的干净历史，root 是 `700d83a chore: init repo with design spec`）。

前缀分布：`11 test · 7 docs · 5 feat · 4 fix · 2 chore · 1 refactor · 1 ci · 1 style`

⚠️ **问题一：`style` 不在任何 parser 里。** `28024f2 style: gofmt the whole tree` —— `cliff.toml` 的 `commit_parsers` 没有 `^style`，`docs/commit-convention.md` 的前缀表也没有。配置是 `filter_unconventional=false` + `filter_commits=false`，该提交会带着空 group 进模板，而 body 里的 `commits | group_by(attribute="group")` 对无该属性的对象是**静默跳过**的 → **这条提交在 CHANGELOG 里凭空消失，不报错**。修法二选一：`cliff.toml` 补 `{ message = "^style", group = "Maintenance" }` 并同步 commit-convention.md；或规定禁用 `style` 前缀。**倾向前者**（历史已用，且 `style` 是 conventional commits 标准前缀之一）。

⚠️ **问题二：`docs/commit-convention.md` 自身已漂移。** "Scope" 段把 `ide-vscode` 列为推荐 scope —— **W0 已经把 `ide/vscode/` 整个删掉了**（`removal_test.go` 在守）。**这是 W10 文档扫描该抓的第一条。**

除此之外 32 条提交全部合规。现有 `CHANGELOG.md`（18 行）是**手写种子**，末尾明确写着「第一个 tag 会用 git-cliff 生成体替换本种子」—— 逻辑成立，但**要先修掉 `--latest` 那个覆盖写 bug，否则替换后第二次发布就丢历史**。

## 10. CONTRIB1 / EX1 / UDOC1 delta（W0 之后重读）

### CONTRIB1
- **现状**：`CONTRIBUTING.md` 66 行，8 个承重约定段 + ADR 流程 + conventional commit + 忽略产物，引用的符号全部真实可达
- ⚠️ **缺口①：完全没提机器强制治理**。W0 刚把 `governance` job 变成硬门禁并新增 GOV4/5/6 + 台账断言，CONTRIBUTING 里对 `internal/archtest`、`cmd/codelines`、三张豁免表、`cmd/featurestatus` **零字提及**；也没提「改 config/-h/schema 必须重跑四个生成器并提交」。**贡献者照着 CONTRIBUTING 走会在 governance/docs 两个 job 上撞墙**
- ⚠️ **缺口②**：`docs/archive/README.md` 的映射表**只有 3 条**，但目录里有 7 个 md；末尾断言 synthesis-* 三份与 deps_analysis「为未跟踪文件，故不在本目录」—— **两处都假**：三份 synthesis-* 就在 `docs/archive/` 且已被 git 跟踪；`deps_analysis.md` 也被跟踪，只是躺在**仓库根**没归档。`feature-roadmap-e-h.md` 在目录里却不在表里
- ⚠️ **缺口③（W0 之后新增，审计没有）**：**仓库根有 4 个已跟踪的垃圾文件**会随开源仓库暴露给所有人：`demo.txt`（还是 dirty 状态）、`diffdemo.txt`（一段贴进来的 agent 输出）、`deps_analysis.md`、`deps_analyze.py`。`git ls-files | grep -v /` 可复现

### EX1
- **现状**：7 个示例（≥5 达标），全部 `--fake-model` 零 key；docs.yml 覆盖了 custom-tool build、headless exec/batch、TS typecheck、Python parse+import。W0 未触碰
- ⚠️ **缺口①：`examples/sdk-python/main.py:25` 运行期必崩**。`item.toolName` 不是属性 —— `generated.py:121` 是 `tool_name: Optional[str] = Field(default=None, alias="toolName")`，**alias 只作输入键**。改成 `item.tool_name` 即可。TS 孪生示例没事是因为 `sdk/ts/v1.ts:48` 确实叫 `toolName`。**CI 只做 `py_compile` + `import`，抓不到**
- **缺口②**：`examples/goalloop-config/run.sh` 存在且自带断言（grep `decision:`），但**不在 docs.yml 任何一步里**；`custom-skill/reverse-echo/SKILL.md` 也没有任何结构字段断言

### UDOC1
- **现状**：`docs/user-guide/` 8 页齐全，索引可达，getting-started 零依赖四步；`entrypoints.md` 的 10 个 `help:*` 生成块**包含 `pr` 与 `auth`**（W0 动过这个文件）；四个生成器当前零 diff
- **缺口①**：`docs.yml` paths 缺 `cmd/yanshi/**`（§6）—— **直接违反 UDOC1 验收第 3 条「与实际不漂移」**
- ⚠️ **缺口②**：**根 README.md（254 行）对 `docs/user-guide`、`docs/api`、`docs/adr`、`examples/`、`CONTRIBUTING.md` 的引用数为 0**（实测 `grep -cE` = 0，只提了 `docs/skills-authoring.md` 与 `docs/vcs.md`）。**开源发布后陌生人从 README 找不到用户指南、贡献指南、示例**
- **缺口③（W8 负责修，W10 负责加门）**：`tui.md` 与 `configuration.md` 仍宣称 `/keymap` `/vim` `/contrast` 可用
- **缺口④（全新）**：`commandTable` 里 `{name: "features", …}` 连续出现两遍，`/help` 会列两次

### ⚠️⚠️ 两条与「发布就绪」直接冲突、审计与 spec 都漏掉的硬缺口

1. **仓库没有 LICENSE 文件**（`ls LICENSE*` 无匹配）。**这是开源首发的硬阻塞**，也意味着 `.goreleaser.yaml` 的 `archives.files`（现为 `config.example.yaml` + `README.md`）补上 LICENSE 后必须同步加一行
2. `SECURITY.md` 的联系邮箱是 `security@x6nux.dev *(placeholder — replace before going public)*` —— **文件自己写着「公开前替换」**

## 11. S0 完成验收自动化

落在 `internal/archtest/completion_test.go`，一条 `TestS0CompletionGate`：先读 `docs/feature-status.yaml` 数终态（`done` + `removed`）。三张豁免表分处两个包（`assemblyExceptions`/`ctxInjectExceptions` 在 archtest，`toolWiringExceptions` 在 `internal/bootstrap/wiring_test.go`），**Go 变量跨包读不到**，所以用 archtest 既有的 `go/ast` helper **解析这三个 map 字面量、数元素个数**，与 W0 三张表的语义完全同源。

断言逻辑是**自武装**的：终态 < 63 时输出进度并放行（W1-W9 期间不误伤），**终态 == 63 时硬断言三张表元素数全为 0**，任何一条残留就点名所属包与条目并失败 —— 因为 W0 播的 11 条豁免全部归属 W1，**残留即证明某个包没做完**。跟着 `go test ./...` 在三平台自动跑，不依赖任何人记得去看。

## 12. 审计 / spec 的过时与错误

1. ⚠️ **`go test ./...` 在 W3 agent 的在途实验期间是红的** —— `internal/agent/registry::TestResumeRejectsRuntimeStillActive` 失败。**这与 W3 的 LEAK2 是同一片代码，归属 W3 而非 W10**，但它会直接卡住 W10 的完成验收，**必须点名移交**。（主会话后续确认：W3 回滚后 HEAD 全绿，该红是并行实验的假象，但也**独立佐证了 W3 的预测**——那个修复确实会打红这条测试）
2. **审计 COV3 的「23%」与 COV2 的「67%」都是陈旧数**（实测 94.1% / 100%），来源 `docs/archive/synthesis-report-v2.md:343` 的 2026-07 快照。**spec §4.3 W10 写成「覆盖率门禁入 CI」略微掩盖了真实缺口是测试语义**
3. **审计与 spec 都只记了 `auth` 一个隐藏子命令，实际是 `auth` + `pr` 两个**
4. **spec §5.1 与 VER1 证据都称「只有 m1..m9 里程碑 tag」—— 实际本仓库一个 tag 都没有**。`build.sh` 与 `docs/upgrade-guide.md:64` 里「里程碑标签 m1..m9 被刻意跳过」的表述目前是对不存在对象的描述
5. **审计 COV2 说「31 个 ServerFrame Type」—— AST 实测是 34**（33 个字符串字面量 + 1 个 `SubagentEventType` 常量；`goldenFrames()` 35 行覆盖 34 型，`status` 有两个构造器）。但审计指出的两个真实弱点**成立且已复现**：`goldenFrames()` 里 `NewTaskUpdate(nil)` 因 `frame.go:726` 的 nil 短路返回零值，golden 第 **82-83 行**冻结的是 `event: `（空）/ `data: {"type":""}`；`TestVocabulary_Symmetry`（`frame_test.go:501`）断言 `f.Type == event`，而 `SSEEvent()`（`frame.go:669-673`）第一行就是 `event = f.Type` —— **恒真重言式**。好消息：一条「golden 实际发出的 Type 集合 == frame.go 声明的 34 个 Type」的断言**今天就是红的**（缺 `task_update`、多一个空串），是干净的 TDD 起点
6. **审计 COV3 指的两条弱测试属实**：`TestBuild_VCSSoftDegrade`（`bootstrap_test.go:104`）只调 `buildMinimalApp` 后断言 `app.VCS != nil`，**根本没制造失败**。补充实现路径：`bootstrap.go:519` 的 `workRoot, _ := os.Getwd()` 是硬编码 cwd，导致该分支不可注入 —— 最小修法是给 `Options`（`bootstrap.go:175`，已有 `Cfg`/`ProviderBuilder`/`AuthDeps` 三个同类测试 seam）加一个带 doc 注释的 `WorkRoot string`，指向不存在的目录即可让 `canonicalRepoRoot`（`vcs.go:521` 的 `EvalSymlinks`）失败，跨平台可靠
7. ⚠️ **spec §4.3 W10 漏了两项发布硬阻塞**：LICENSE 不存在、SECURITY.md 联系方式仍是自标 placeholder。**按 §2 约束 4「开源产品标准」，这两条比表内任何一项都更该在 W10 收口**
8. `docs/archive/README.md` 末段关于 synthesis-* / deps_analysis 的两句是**可证伪的错误陈述**
