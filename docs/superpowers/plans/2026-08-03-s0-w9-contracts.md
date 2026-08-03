# S0/W9 对外契约 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把「三份自称不同身份的 schema + 一个从不解析任何东西的伪生成器 + 一套没有任何 workflow 在跑的 SDK 测试」收敛成一条有门禁守着的对外契约。

**Architecture:** 三条链路：① schema 真相源统一 + parity 测试（**落点在 Go 侧**，因为 `go test ./...` 是无条件硬跑的）；② `--file` 输入路径与 JSON-RPC 配置持久化两个真实 bug；③ CI 新增 `sdk-contract` job + 补 paths 过滤。

**Tech Stack:** Go 1.26.4，JSON Schema（Ajv）、vitest、pytest。

---

## 本计划的写法

**这是意图级计划，不是代码清单。** 每步说清楚：改哪个文件的哪个函数、断言什么行为、怎么观察、为什么重要、预期看到什么。**具体代码由实现者写** —— 你手里有编译器，本文档没有。

「已核对事实」是**实测结果**，可直接依赖；其余标识符请先 grep。

---

## ⚠️ 最重要的一条：spec 对 v1.1 的框架是错的

spec §4.3 W9 那行写「v1 有 21 个 `$defs`，v1.1 现只剩 1 个」。**数字对，框架错。**

「**剩**」字暗示 v1.1 是一份正在退化的副本。**实际它是一个 `allOf: [{"$ref": "../v1/agent-api.schema.json"}]` 覆盖层** —— 结构上它*已经*是「v1 + delta」，那 1 个 `$defs.Item`（加 `reasoningTokens`，`minimum: 0`）**就是**那个 delta。

⚠️ **按这个错误框架去修（补齐 20 个 `$defs`）会把一个正确的结构改坏。**

**真实缺陷是：那个 delta 是死的。** Ajv 实测（在 `sdk/ts/` 内跑）：注册 v1 后编译 v1.1 成功，然后喂一个 `reasoningTokens: -5` 的 item —— **验证通过（返回 true，errors 为 null）**。

原因：v1 根节点的 `anyOf` 引用 `#/$defs/Item`，该指针在 v1 自己的 `$id` scope 内解析，**永远解析不到 v1.1 的本地 `$defs.Item`** —— 覆盖层**从未被任何求值路径触及**。

> ⚠️ **另一条不要写进任何论证**：「v1.1 审计时是 3、现在是 1，所以缺口在扩大」**无法证实，且怀疑是串行**。审计 V14 条目里的「3 个 `$defs`」明确指 **`internal/api/v1/schema.go` 的 Thread/Turn/Item**，不是 v1.1；审计全文**没有给出 v1.1 的 `$defs` 计数**。**这条缺证据。**

---

## 已核对事实（实测）

### 三份 schema，三个不同身份

| 文档 | `$defs` 数 | `$id` |
|---|---:|---|
| `internal/api/v1/schema.go` 的 `schemaDocument`（**运行时唯一对外端点吐的**） | **3**（Thread/Turn/Item） | `https://yanshi.dev/schema/agent-api-v1.json` |
| `sdk/schema/v1/agent-api.schema.json` | **21** | `.../agent-api/v1/agent-api.schema.json` |
| `sdk/schema/v1.1/agent-api.schema.json` | **1**（`allOf` 覆盖层） | — |

⚠️ **两个 `$id` 不同 —— 它们是两份自称不同身份的文档，不是同一份的两个副本。**

`schema.go:13` 的注释仍写着「Task 9 expands this document (params/response shapes, item type enum)」—— **原设计的扩展从未落到运行时**。

唯一的 schema 端点是 `GET /api/v1/schema/agent-v1.json`（`internal/api/http/agent_v1.go`），吐的是 `v1.SchemaBytes()`，即**最贫瘠的那份**。

v1.1 的 `description` 自述：「Fixture-only v1.1 schema. D1 currently emits only 'v1' … **Not served by D1**」。

### 真正的漂移是四路的

| 轴 | 实测差异 |
|---|---|
| 运行时 `schema.go` vs `sdk/schema/v1` | **3 vs 21** `$defs` |
| Go vs `sdk/schema/v1` | `Item` 多 `fileChange`；`TurnStartParams` 多 `context`；`ThreadSnapshot` 在 schema 中不存在；9 个 `$defs` 无 Go 对应物 |
| Go vs `sdk/ts/v1.ts` | ⚠️ **TS 缺 `TurnStartParams.images`**；缺 `ThreadSnapshot` |
| Go vs Python | Python 有 `context` 但 ⚠️ **缺 `images`**；`Item` 多 `fileChange` |

其中 `ContextItem`/`FileChange`/`Range` 是**有意的**前瞻字段，`sdk/schema/CONTRACT_HANDOFF.md:37-49` 明确记录（D2 附加、D1 忽略未知字段）。

⚠️ **`images` 缺失是无意的真漂移。**

### 现有 contract 测试实测

| 文件 | 实况 |
|---|---|
| `sdk/ts/tests/contract.test.ts` | 唯一的 schema 加载是 `new URL("../../schema/v1/", ...)` —— **只加载 v1**。6 个断言（fixture 过校验、items.jsonl 逐条过校验、sequence 恰为 `[1..7]`、`structured.result` 深等、未知 item type 保留、坏 sequence/空 threadId/`version:"v2"` 被拒） |
| `sdk/ts/tests/version-matrix.test.ts` | 7 个测试，**只操作 `X-Yanshi-API-Version` 头和 body 里的 `version` 字符串**，**从不读取任何 schema 文件** |

⚠️ **全仓没有任何测试比较两份 schema 的内容。** grep 了 `sdk/ts/tests/`、`sdk/python/tests/`、全部 Go `*_test.go`，**零命中**。

实测 `cd sdk/ts && npx vitest run` → **4 files / 29 tests 全过**（286ms）。

> **现状不是「测试红了没人管」，而是「测试压根没覆盖这条轴」。**

### CI 现状

⚠️ **`sdk/ts` 与 `sdk/python` 的测试套件当前没有任何 workflow 在跑。**

`grep -rn 'sdk\|pytest\|vitest\|npm\|python' .github/workflows/` 的全部命中只落在 `docs.yml`：
- `docs.yml:98-100` —— `npm --prefix sdk/ts install` + `npx tsc --noEmit`。**只 typecheck，不跑 vitest**
- `docs.yml:104-106` —— `py_compile` + `pip install -e sdk/python` + `import yanshi_sdk`。**只 import smoke，不跑 pytest**

全仓 workflow 里 **`vitest` 与 `pytest` 零命中**。

⚠️ **额外发现**：`docs.yml` 的 `paths` 过滤器**没有 `sdk/**`** —— **只改 `sdk/` 时，连现有的 typecheck 都不会触发**。这比「测试没跑」更严重一层。

本地实测：`sdk/python` 的 pytest **跑不了**（`import jsonschema` → `ModuleNotFoundError`，`[test]` extra 未安装）—— **这本身就是「没人在跑」的旁证**。

### 其余四项 delta

| 项 | 缺口 | 位置 |
|---|---|---|
| **APS1** | JSON-RPC 2.0 dispatch **真实且已接线**（覆盖 initialize/capabilities/thread/turn/config/shutdown，标准错误码 `rpc.go:15-21`），与 HTTP 共用同一 `*v1.Service` | `internal/appserver/server.go` |
| APS1 | ⚠️ `app.go:46` **无条件** `appserver.NewMemoryConfig()`（**即使传了 `-config` 也一样**）→ `config/read\|write` **永不落盘**，而 `docs/api/jsonrpc.md` 把它们描述成「读/写运行时配置」 | `cmd/yanshi/app.go:46` |
| APS1 | 无 HTTP↔JSON-RPC 的跨传输行为一致性对照测试 | — |
| APS1 | ✅ **秘密路径拒绝完好并有测试**（`secretPathFragments` token/api_key/apikey/secret + 逐 dot-segment 小写比对 + `password` 子串，**读写两侧都在 decode 之前拒绝**）—— **这条不能动** | `config.go:15-20`、`:73`；`config_test.go` 95 行 |
| **V12** | stdin 三模式解析**真实**（text/lines/jsonl，1MiB 行上限，jsonl 逐行带行号报错），resume 只在第 0 条生效 | `internal/cli/headless_input.go:37` |
| V12 | ⚠️ `--file` 分支把整个文件当**一条** prompt，**完全绕过 `cfg.Input` 模式、从不调用 `ReadHeadlessInputs`** → `--input jsonl --file <3行文件>` 只跑 1 个 turn 而非 3 | `cmd/yanshi/headless.go` |
| V12 | ⚠️ `docs.yml:105` 的 CI smoke **跑的正是这条命令**，但**输出丢给 `/dev/null`、零断言** —— 掩盖了它 | — |
| V12 | ✅ **退出码无需改动**：`mapExecError` 是 nil→0 / DeadlineExceeded→124 / Canceled→130 / 其余→1，加 flag 解析失败→2 | `main.go:1014-1025` |
| **V15** | TS/Python 客户端主流程真实可跑（start/resume/interrupt/cancel/run 都在，TS 29 测试本地全过） | — |
| V15 | ⚠️ **「类型生成」是伪生成器** —— 从 `text := \`...\`` 到函数末尾是**一整段硬编码 Go 原始字符串字面量**，逐字符抄写 TS 接口，**从不解析 `internal/api/v1` 的任何东西**；唯一接触点 `main.go:176` 的 `_ = v1.SchemaBytes()` **返回值被丢弃**，注释自称「guards the generator against silent drift」但 `_ =` **语义上不可能检测任何东西** | `cmd/api-schema/main.go` |
| V15 | 实测 `go run ./cmd/api-schema \| diff - sdk/ts/v1.ts` → **IDENTICAL，但这是循环自证**（生成器和产物是同一段字面量） | — |
| V15 | Python 侧 `generated.py:1-16` 自述「Hand-mirrored … D2 maintains the Python mirror by hand」；`pyproject.toml` 的 `[generate]` extra 里 `datamodel-code-generator` **从未被任何脚本调用** | — |
| **APIREF1** | 生成区块**真实**（`resources.md` 有 11 个 `api-defs:*` 标记，`runMarkdown` 是真实幂等实现） | `cmd/api-schema/markdown.go` |
| APIREF1 | ⚠️ `paramResponseDefs()` 是**手工维护表**（doc 自己承认「hand-maintained field map … mirrors the hand-written TS interfaces in main.go」）→ `resources.md` 的 `images` 行是**手写的**，**与 TS 缺 `images` 的事实并存而无人报警** | — |
| APIREF1 | ⚠️ `docs/api/schema.md` 头部（`schemaDocHeader`）**自相矛盾** —— 同时声称是「`sdk/schema/v1` 的完整 JSON Schema」**和**「从 `internal/api/v1/schemaDocument` 生成」，而后者只有 3 个 `$defs` | — |
| APIREF1 | ⚠️ `docs/api/sdk-python.md` 的示例用 `item.toolName`，但 Python 属性名是 `tool_name`、`toolName` **只是 pydantic alias**（`generated.py:121`）→ **属性访问必抛 `AttributeError`**（与 `examples/sdk-python/main.py:25` 同一个 bug） | — |
| APIREF1 | `jsonrpc.md` 对 `config/read\|write` 的描述与 APS1 的 in-memory 现实不符 | — |

### `internal/agent/orchestrator` 的非测试导入者恰好 6 个

`internal/agent/spawn/spawn.go`、`internal/api/http/chat.go`、`internal/api/http/ws.go`、`internal/api/http/ws_compaction.go`、`internal/api/v1/service.go`、`internal/bootstrap/bootstrap.go`。

**小到今天就能钉成一张白名单。**

---

## 四条裁定

**裁定 1 — schema parity 采用 Option B（保持分离 + 枚举有意差异的 parity 测试），落点在 Go 侧。**

| 方案 | 为什么否决 |
|---|---|
| A（让 v1.1 用 `$ref` 继承 v1） | **结构上已经实现了** —— v1.1 就是 `allOf` + `$ref` 覆盖层 —— **而它照样出了缺陷**（覆盖层求值不到，delta 死掉）。说明「结构上不可能漂移」这个承诺**靠 `$ref` 拿不到**，A **解决不了实际发生的那个 bug** |
| C（折回 v1，删掉 v1.1） | **会删掉唯一一份「未来 minor 长什么样」的可执行样本**，而 `version-matrix.test.ts` 的 v1.1 宽容度断言**恰恰需要这个形状存在才有意义** |

**B 是唯一能把「`images` 少了」和「`fileChange` 是故意的」区分开的方案** —— 它要求**每条差异有名字和理由**。有意差异表天然是债务计数器，与 W0 已确立的「豁免表只减不增 + 死条目失败」语义**同款**。

⚠️ **落点必须在 Go 侧**（`internal/api/v1` 的一条 parity 测试），**不是 TS 侧** —— 因为 `go test ./...` 是**无条件硬跑**的，而 Node/Python 工具链在 CI 里是**可选步骤**。这样 parity 失败在**没装 Node 的机器上也会红**。

**对 v1.1 那条死覆盖层的处置**：把它作为一条**具名**差异写进表里，并附一条断言「v1.1 的 `$defs.Item` 必须真正参与求值」（用 `reasoningTokens: -5` **必须被拒**作为断言）—— **逼它要么修好，要么删掉**。

**裁定 2 — `V14` 的三条偏离：两条收敛，一条不碰（属 W1），一条接受。**

| 偏离 | 处置 | 理由 |
|---|---|---|
| 两份 schema 并存且详略不同 | **收敛** | 让运行时 `schemaDocument` 与 `sdk/schema/v1` 统一到一个真相源，并删掉 `schema.go:13` 那句假注释。**不统一的话 parity 测试没有可锚定的基准** |
| `TurnStartParams.Images` 是死字段 | ⚠️ **W9 不做** | spec §4.3 明确把「`ApplyImages` 接进 turn 路径、`TurnOpts` 加图像字段」划给 **W1**（W1 Task 12/13）。W9 去做就是重复投入。**W9 只负责在 parity 表里把「TS/Python 缺 `images`」记为待 W1 关闭的具名差异** |
| `ThreadSnapshot.Items` 永远为空 | **收敛** | `service.go:286` 从不设 `Items`，而 `agent_v1.go` 的 resume handler 与 `appserver/server.go` 的 dispatch **都在转发它** —— **一个两条传输都在转发的永空字段是对外契约的谎报**。要么真填、要么从 wire 契约删掉 |
| `ContextItem`/`FileChange`/`Range` 无 Go 对应物 | **接受偏离** | `CONTRACT_HANDOFF.md:37-49` 记录了它们是 D2 前瞻字段、D1 有意忽略 |

**裁定 3 — 新增 CI job 落在 `ci.yml` 不是 `docs.yml`。**
`docs.yml` 是 **paths-filtered** 的，会漏掉 sdk-only 改动；`ci.yml` **无 paths 过滤，PR 必跑**。

**同时补 `docs.yml` 的 `paths` 加 `sdk/**`** —— 它最后两步（TS typecheck、`pip install -e sdk/python`）**真实依赖它**。

> 💡 审计在 W10 名下记了「补 `cmd/yanshi/**`」，**同一个列表还缺 `sdk/**`，建议一并处理**。

**裁定 4 — orchestrator 导入者白名单需要一张新表，不能挂在 `portAllowlists` 上。**

⚠️ **方向不同**：`portAllowlists` 是「**port 包 → 允许的依赖**」，即「这个包能依赖谁」；而这里需要的是**反向**断言「**谁能依赖这个服务层包**」。

`deps_test.go` 现有的 `serviceLayerPrefixes` + `isServiceLayer`（R5：ports 不得导入 service layer）是最接近的邻居，但**语义也不同**。

→ **需要一张新的小表，复用 `buildImportGraph` helper，不复用 `portAllowlists`。**

**已知的、刻意保留的两条**：`ws.go` 与 `v1/service.go` 各自构造 `TurnOpts` 并调 `EventsWithHistoryOpts`（spec §4.3 明确说收敛点在 **S3**，**W9 不得动**）。**这两条进白名单并标注 S3。**

---

## Task 1: 统一 schema 真相源（V14 偏离 1）

**Files:** Modify `internal/api/v1/schema.go`（含 `:13` 的假注释）、`sdk/schema/v1/agent-api.schema.json`（可能）；Test `internal/api/v1/`

- [ ] **Step 1: 写失败测试**

**断言什么**：运行时 `SchemaBytes()` 吐出的文档与 `sdk/schema/v1/agent-api.schema.json` 的 `$defs` 集合**一致**（或差异全部在有意差异表内）。

**为什么重要**：唯一对外的 schema 端点（`GET /api/v1/schema/agent-v1.json`）吐的是**三份里最贫瘠的那份**（3 个 `$defs`），而 SDK 校验用的是 21 个的那份。**客户端拿到的自述与实际校验标准不是一回事。**

**预期**：FAIL，3 vs 21。

- [ ] **Step 2: 统一**

**做什么**：让两者指向**一个真相源**。⚠️ **先判断哪个方向更省** —— 是让运行时嵌入 `sdk/schema/v1`（`embed.FS`），还是让 `sdk/schema/v1` 由运行时生成。**两个 `$id` 不同**，统一时要决定用哪个。

**同时删掉 `schema.go:13` 的假注释**（「Task 9 expands this document…」—— 那个扩展从未发生）。

- [ ] **Step 3: 全量与提交**

---

## Task 2: parity 测试与有意差异表（裁定 1）

> 依赖 Task 1（需要一个可锚定的基准）。

**Files:** Create `internal/api/v1/parity_test.go`；Test

- [ ] **Step 1: 写四路 parity 断言**

**断言什么**：Go struct tag ↔ `sdk/schema/v1` ↔ `sdk/ts/v1.ts` ↔ `sdk/python` 的字段集合一致，**差异必须在有意差异表内**。

**表里应有的初始条目**（每条要有**名字和理由**）：
- `ContextItem` / `FileChange` / `Range` 无 Go 对应物 —— **有意**（`CONTRACT_HANDOFF.md:37-49`，D2 前瞻字段）
- TS/Python 缺 `TurnStartParams.images` —— ⚠️ **待 W1 关闭**（裁定 2）
- v1.1 的 `$defs.Item` 覆盖层 —— **待修或待删**（见 Step 2）

⚠️ **落点在 Go 侧**（裁定 1）—— `go test ./...` 无条件硬跑。

- [ ] **Step 2: 加「v1.1 覆盖层必须真正参与求值」的断言**

**断言什么**：一个 `reasoningTokens: -5` 的 item **必须被 v1.1 拒绝**。

**实测现状**：Ajv **验证通过**（返回 true，errors 为 null）—— v1 根节点的 `anyOf` 引用 `#/$defs/Item`，在 v1 自己的 `$id` scope 内解析，**永远解析不到 v1.1 的本地 `$defs.Item`**。

**这条断言逼那个死覆盖层要么修好，要么删掉。**

⚠️ **不要按 spec 的框架去「补齐 20 个 `$defs`」** —— 那会把一个正确的 `allOf` 覆盖层结构改坏。

- [ ] **Step 3: 全量与提交**

---

## Task 3: `ThreadSnapshot.Items` 的谎报（V14 偏离 3）

**Files:** Modify `internal/api/v1/service.go`（`:286`）或 `types.go`（`:101`/`:154`）；Test

- [ ] **Step 1: 写失败测试**

**断言什么**：resume 一个有历史的 thread，返回的 snapshot **要么真的带 items，要么契约里根本没有这个字段**。

**为什么重要**：`service.go:286` 从不设 `Items`，而 `agent_v1.go` 的 resume handler 与 `appserver/server.go` 的 dispatch **都在转发它**，`types.go:101`/`:154` 都声明了它 —— **客户端拿到的永远是缺省省略的空数组**。**一个两条传输都在转发的永空字段是对外契约的谎报。**

- [ ] **Step 2: 二选一并写明理由**

**要么真填、要么从 wire 契约删掉。** 在 commit 里说明为什么选这一边。

- [ ] **Step 3: 全量与提交**

---

## Task 4: JSON-RPC 配置持久化（APS1）

**Files:** Modify `cmd/yanshi/app.go`（`:46`）；Test `internal/appserver/`

- [ ] **Step 1: 写失败测试**

**断言什么**：`yanshi app -config <path>` 启动后，`config/write` **真的落盘**，重启后 `config/read` 能读到。

**为什么重要**：`app.go:46` **无条件** `appserver.NewMemoryConfig()`（**即使传了 `-config` 也一样**）→ `config/read|write` **永不落盘**，而 `docs/api/jsonrpc.md` **把它们描述成「读/写运行时配置」**。

**预期**：FAIL，重启后读不到。

- [ ] **Step 2: 实现**

⚠️ **秘密路径拒绝不能动** —— `config.go:15-20` 的 `secretPathFragments`（token/api_key/apikey/secret）+ `validateConfigKey:73` 逐 dot-segment 小写比对 + `password` 子串匹配，**读写两侧都在 decode 之前拒绝**，`config_test.go` 在位（95 行）。**这条是承重的。**

**落盘实现必须保持同样的拒绝时机**（decode 之前）。

- [ ] **Step 3: 补 HTTP↔JSON-RPC 跨传输一致性测试**

**断言什么**：同一个操作经 HTTP 与经 JSON-RPC 得到**行为一致**的结果。

**为什么**：两者共用同一 `*v1.Service`，但**没有对照测试**守住这条。

- [ ] **Step 4: 全量与提交**

---

## Task 5: `--file` 绕过输入模式（V12）

**Files:** Modify `cmd/yanshi/headless.go`；Modify `.github/workflows/docs.yml`（`:105`）；Test `internal/cli/`

- [ ] **Step 1: 写失败测试**

**断言什么**：`--input jsonl --file <3 行文件>` 跑出 **3 个 turn**，不是 1 个。

**为什么重要**：`--file` 分支把整个文件当**一条** prompt，**完全绕过 `cfg.Input` 模式、从不调用 `ReadHeadlessInputs`**。

⚠️ **`docs.yml:105` 的 CI smoke 跑的正是这条命令，但输出丢给 `/dev/null`、零断言** —— **它掩盖了这个 bug**。

**预期**：FAIL，只有 1 个 turn。

- [ ] **Step 2: 实现**

**做什么**：`--file` 分支改为走 `ReadHeadlessInputs`（`internal/cli/headless_input.go:37`），复用它已有的三模式解析（text/lines/jsonl、1MiB 行上限、jsonl 逐行带行号报错）。

⚠️ **退出码不能动** —— `mapExecError`（`main.go:1014-1025`）的 nil→0 / DeadlineExceeded→124 / Canceled→130 / 其余→1 / flag 解析失败→2 **全部保持**。修 `--file` 是**纯输入路径改动**。

- [ ] **Step 3: 给 CI smoke 加断言**

⚠️ **把 `docs.yml:105` 的 `/dev/null` 换成真实断言** —— 否则修好了也没人守住。

- [ ] **Step 4: 全量与提交**

---

## Task 6: 真生成器或诚实的手工镜像（V15）

**Files:** Modify `cmd/api-schema/main.go`（含 `:176`）、`sdk/python/`；Test

**背景：** ⚠️ **「类型生成」是伪生成器** —— 从 `text := \`...\`` 到函数末尾是**一整段硬编码 Go 原始字符串字面量**，逐字符抄写 TS 接口，**从不解析 `internal/api/v1` 的任何东西**。

唯一接触点 `main.go:176` 的 `_ = v1.SchemaBytes()` —— **返回值被丢弃**，注释自称「guards the generator against silent drift」，但 **`_ =` 语义上不可能检测任何东西**。

实测 `go run ./cmd/api-schema | diff - sdk/ts/v1.ts` → **IDENTICAL，但这是循环自证**（生成器和产物是同一段字面量）。

- [ ] **Step 1: 裁决走哪条路**

**两个方向，二选一并写明理由**：

| 方向 | 代价 | 收益 |
|---|---|---|
| **真生成** —— 从 `internal/api/v1` 的 struct 反射出 TS/Python 类型 | 要写一个真实的类型映射器 | 结构上不可能漂移 |
| **诚实的手工镜像** —— 承认是手写，**靠 Task 2 的 parity 测试守门** | 几乎零 | parity 测试已经在写了，**这条差异只是多一个表项** |

⚠️ **不能维持现状** —— 现状是「自称生成器但其实是手抄，且自称有防漂移守卫但那行代码不可能起作用」。**两处自述都是假的。**

> 💡 Python 侧 `generated.py:1-16` **已经诚实自述**「Hand-mirrored … D2 maintains the Python mirror by hand」—— **TS 侧应当向它看齐**，而不是反过来。

- [ ] **Step 2: 实现所选方向**

**若选「诚实镜像」**：删掉 `_ = v1.SchemaBytes()` 那行**和**它的假注释；把 `main.go` 的 doc 改成「手工维护的镜像，由 `internal/api/v1` 的 parity 测试守门」。

**若选「真生成」**：`_ =` 必须变成真实的使用。

- [ ] **Step 3: 处理 `pyproject.toml` 的死 extra**

`[generate]` extra 里的 `datamodel-code-generator` **从未被任何脚本调用**。**要么接上，要么删掉。**

- [ ] **Step 4: 全量与提交**

---

## Task 7: 文档生成与三处假陈述（APIREF1）

**Files:** Modify `cmd/api-schema/markdown.go`（`paramResponseDefs`、`schemaDocHeader`）、`docs/api/{sdk-python,jsonrpc}.md`、`examples/sdk-python/main.py`（`:25`）

- [ ] **Step 1: 修 `docs/api/schema.md` 头部的自相矛盾**

⚠️ `schemaDocHeader` **同时声称**是「`sdk/schema/v1/agent-api.schema.json` 的完整 JSON Schema」**和**「从 `internal/api/v1/schemaDocument` 生成」，而后者只有 3 个 `$defs`。

> **Task 1 统一真相源后，这句话才可能同时为真。** 顺序不能反。

- [ ] **Step 2: 修 `sdk-python.md` 与 example 的 `AttributeError`**

⚠️ `docs/api/sdk-python.md` 的示例用 `item.toolName`，但 Python 属性名是 `tool_name`、`toolName` **只是 pydantic alias**（`generated.py:121`）—— **属性访问必抛 `AttributeError`**。

**同一个 bug 也在 `examples/sdk-python/main.py:25`。两处一起改。**

> ⚠️ **CI 只做 `py_compile` + `import`，抓不到运行期错误** —— 所以 Task 8 的 pytest job 是这条的守门人。

- [ ] **Step 3: 处理 `paramResponseDefs()` 的手工表**

⚠️ 它是**手工维护表**（doc 自己承认「hand-maintained field map … mirrors the hand-written TS interfaces in main.go」）→ `resources.md` 的 `images` 行是**手写的**，**与 TS 缺 `images` 的事实并存而无人报警**。

**做什么**：让 Task 2 的 parity 测试**覆盖到这张表**，或让它从真相源生成。**二选一，写明理由。**

- [ ] **Step 4: 修 `jsonrpc.md` 对 `config/read|write` 的描述**

> Task 4 落盘后，原描述才成为真话。**顺序不能反。**

- [ ] **Step 5: 全量与提交**

---

## Task 8: CI 门禁（裁定 3）

**Files:** Modify `.github/workflows/ci.yml`、`.github/workflows/docs.yml`

- [ ] **Step 1: 在 `ci.yml` 新增 `sdk-contract` job**

**跑什么**：Node 20 + Python 3.11；`npm --prefix sdk/ts ci` → `vitest run`；`pip install -e 'sdk/python[test]'` → `pytest sdk/python`。

⚠️ **放 `ci.yml` 不放 `docs.yml`**（裁定 3）—— `docs.yml` 是 paths-filtered 的，会漏掉 sdk-only 改动。

- [ ] **Step 2: 给 `docs.yml` 的 `paths` 补 `sdk/**`**

⚠️ **只改 `sdk/` 时，连现有的 typecheck 都不会触发** —— 这比「测试没跑」更严重一层。

> 💡 审计在 W10 名下记了「补 `cmd/yanshi/**`」—— **同一个列表，建议一并处理**。

- [ ] **Step 3: 验证 pytest 真的能跑**

⚠️ 本地实测 `sdk/python` 的 pytest **跑不了**（`import jsonschema` → `ModuleNotFoundError`，`[test]` extra 未安装）—— **确认 `[test]` extra 声明了所需依赖**。

- [ ] **Step 4: 提交**

---

## Task 9: orchestrator 导入者白名单（裁定 4）

**Files:** Create/Modify `internal/archtest/deps_test.go` 或新文件

- [ ] **Step 1: 写白名单断言**

**断言什么**：`internal/agent/orchestrator` 的非测试导入者**只能是白名单里的 6 个**。

⚠️ **需要一张新表，不能挂在 `portAllowlists` 上** —— 方向相反（`portAllowlists` 是「这个包能依赖谁」，这里要「谁能依赖这个包」）。**复用 `buildImportGraph` helper。**

**白名单初始 6 条**：`internal/agent/spawn/spawn.go`、`internal/api/http/{chat,ws,ws_compaction}.go`、`internal/api/v1/service.go`、`internal/bootstrap/bootstrap.go`。

**其中两条标注 S3**：`ws.go` 与 `v1/service.go` 各自构造 `TurnOpts` 并调 `EventsWithHistoryOpts` —— spec §4.3 明确说收敛点在 **S3**，**W9 不得动**。

**为什么值得做**：小到今天就能钉住，之后**任何新前端/新渠道直连 orchestrator 会立刻红**。

- [ ] **Step 2: 提交**

---

## Task 10: 台账翻牌 + W9 收尾验证

- [ ] **Step 1: 翻牌 5 条**

| 条目 id | 现 verdict | 证据来自 |
|---|---|---|
| `D1/APS1` | partial | Task 4 |
| `D1/V12` | partial | Task 5 |
| `D1/V14` | **divergent** | Task 1/2/3 |
| `D2/V15` | partial | Task 6 |
| `H2/APIREF1` | partial | Task 7 |

⚠️ **`D1/V14` 的 acceptance 需要重写**：「有版本+Schema」→「**单一** schema 真相源 + parity 测试守门」（裁定 2）。

⚠️ **`D2/V15` 的 evidence 必须写明走的是哪条路**（真生成 vs 诚实镜像 + parity 守门）。

⚠️ 若 W1 尚未关闭 `images` 漂移，**parity 表里那条保持开放**，但**不阻塞 `V14` 翻牌** —— 它已经被具名记录了。

- [ ] **Step 2: 台账门与计数**

Run: `go test ./internal/archtest -run TestFeatureStatus` → PASS（总数仍为 63）
Run: `go run ./cmd/featurestatus`

- [ ] **Step 3: 全量验证**

```bash
go build ./... && go vet ./... && go test ./...
go test ./internal/archtest
cd sdk/ts && npx vitest run && cd ../..
pip install -e 'sdk/python[test]' && pytest sdk/python
go run ./cmd/api-schema -markdown docs/api/schema.md
go run ./cmd/api-schema -markdown docs/api/resources.md
go run ./cmd/gendocs -config docs/user-guide/configuration.md
go run ./cmd/gendocs -help-all docs/user-guide/tui.md docs/user-guide/entrypoints.md
git diff --exit-code docs/
```

- [ ] **Step 4: 提交**

---

## W9 验收清单

- [ ] 运行时 schema 端点与 `sdk/schema/v1` **同源**
- [ ] parity 测试在 **Go 侧**，四路差异全部在**具名**的有意差异表内
- [ ] `reasoningTokens: -5` **被 v1.1 拒绝**（或 v1.1 已删除）
- [ ] `ThreadSnapshot.Items` **要么真填、要么不在契约里**
- [ ] `yanshi app -config <path>` 的 `config/write` **真的落盘**；秘密路径拒绝**未被削弱**
- [ ] `--input jsonl --file <3 行>` 跑出 **3 个 turn**；CI smoke **有断言**
- [ ] `cmd/api-schema` 的自述**与现实一致**（不再自称生成器却手抄）
- [ ] `sdk-python.md` 与 `examples/sdk-python/main.py` **不再抛 `AttributeError`**
- [ ] `ci.yml` 有 `sdk-contract` job，vitest 与 pytest **都真的跑**
- [ ] `docs.yml` 的 `paths` **含 `sdk/**`**
- [ ] orchestrator 导入者白名单**已钉住 6 条**

## 依赖与移交

| 事项 | 关系 |
|---|---|
| `TurnStartParams.Images` 打通 | **W1**（Task 12/13）—— W9 只在 parity 表里具名记录 |
| `ws.go` / `v1/service.go` 各自构造 `TurnOpts` 的收敛 | **S3**（spec §4.3 明确）—— 白名单里标注，**W9 不得动** |
| `docs.yml` paths 补 `cmd/yanshi/**` | **W10**（同一个列表，Task 8 可一并做） |
| GOV1 的 orchestrator 反向白名单 | 本包 Task 9 落地 |

## spec / 审计中已被证伪的论断（不要照着做）

1. ⚠️ **spec §4.3 W9 对 v1.1 的框架是错的** —— 「剩 1 个」暗示退化副本，实际是 `allOf` + `$ref` 覆盖层。**按这个框架补齐 20 个 `$defs` 会把正确结构改坏。** 真实缺陷是**覆盖层求值不到**。
2. **「两份 divergent schema 正在发给客户端」被 v1.1 自己的 description 否证** —— 「Not served by D1」，且唯一 schema 端点吐的是**第三份**（运行时 3-`$defs` 文档），而它恰恰是最贫瘠的。
3. ⚠️ **「v1.1 审计时是 3、现在是 1，所以缺口在扩大」无法证实，且怀疑是串行** —— 审计里的「3 个 `$defs`」明确指 `schema.go` 的 Thread/Turn/Item，**不是 v1.1**；审计全文没给出 v1.1 的计数。**不要把这条写进任何论证。**
4. **审计 V14 evidence 的 `bootstrap.go` 行号已漂移**（W0 加了 `App.ToolNames` 与 `DefaultOrchestratorProfile()`）。不影响判定（`status_test.go` 按设计不校验行号），但**按行号找会找错**。
5. **审计中「把 `check-d2.sh` 接进 CI」已作废** —— W0 已删除该脚本，`removal_test.go` 断言它不得复现。
6. ⚠️ **`docs.yml` 的 `paths` 缺 `sdk/**`** —— 审计与 spec 都未记录。
7. **审计判 APIREF1「无差异」时低估了 `paramResponseDefs()` 是手工表这件事** —— 它正是 `images` 字段在 `resources.md` 里出现的来源，**一个手工表在替一个不存在于 TS/Python 的字段作证**。
