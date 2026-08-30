# ADR-0024: 模型能力目录是数据，覆盖优先级是代码

- 状态：accepted
- 日期：2026-08-30

## 背景（Context）

W-C-01（INF2）是 capability-roadmap 里另外 14 个 W-C-* 工作项共同的地基：在此之前，
一个模型的 context window（`internal/llm/eino/contextwindow.go` 的
`contextWindowCatalog` Go 字面量表）与定价（`pricing.go` 的 `DefaultPricing` 函数体）
是两张写死在源码里的表。给目录加一个模型、或者给一个已有模型改价，此前都是一次
Go 代码改动 + 重新构建 —— 对一个新模型发布周期以周计的仓库，这个摩擦本身就是问题。

验收明文要求四件事，其中第三、四条此前都不存在对应的机制：

1. 新增模型只改数据文件，不改 Go 代码。
2. 表中查不到的模型走安全默认，不阻止启动。
3. `ProviderConfig` 的同名字段覆盖表值。
4. 压缩阈值按窗口**比例**而非绝对值，且**取自表**（此前 `CompactionConfig.Threshold`
   是全局唯一值，没有任何按模型覆盖的路径）。**Ruling RC-3（评审 review-c1.md
   F-4）：这一条验收的是机制——解析器、派生索引、覆盖梯子——而不是数据本身。
   `models.yaml` 这一轮交付时 `auto_compact_threshold` 字段有零行填充**（见
   `models.yaml` 文件头「Values below」段落之后的说明）；伪造一批看起来合理的
   比例数字来让这条验收"看起来更完整"，会把「未经调研」冒充成「已调研」，比留白
   更糟。数据由后续 W-C 工作项按型号逐条调研补齐。

被否决的替代方案：

- **JSON 而非 YAML。** 否决：本仓所有面向操作员编辑的数据文件（`config.example.yaml`
  本身）都是 YAML；`gopkg.in/yaml.v3` 已是直接依赖，换格式没有换来任何东西，只换来
  「操作员要在两种语法间切换」。
- **每个模型一个文件（目录式目录）。** 否决：51 条 context-window 行 + 8 条定价行
  当前体量下，一个文件比 51 个文件更容易做「新增一行」这个最常见操作的 diff 审查；
  拆分没有解决任何真实存在的问题（YAGNI）。
- **阈值内嵌进 context_window 行、按 `window * 默认比例` 派生。** 否决：验收第 4 条
  明确要求「取自表」而不是「由窗口计算」——把阈值定义成窗口的一个固定倍数，会让「给
  一个爱重复调用工具的模型单独调低阈值」这个真实需求（config.example.yaml 新增注释
  举的例子）无法表达，因为倍数是全局常量而不是每模型数据。

## 决策（Decision）

**能力数据活在一个 embed 的 YAML 文件里；覆盖优先级活在 Go 代码里，且优先级顺序对
窗口和阈值必须逐字对称。**

1. `internal/llm/eino/models.yaml`（`//go:embed`）承载全部目录数据，`modelcatalog.go`
   在包 init 时解析一次（`mustParseModelCatalog`），派生出三张索引：
   `buildContextWindowCatalog`（边界子串匹配，见 `contextwindow.go` 文件头「为什么
   窗口要穿透网关前缀」的论证）、`buildPricingCatalog` 与 `buildAutoCompactThresholds`
   （两者都是**精确匹配**——定价与阈值不应该因为一个网关前缀恰好包含某个 family 片段
   就被继承，见 `catalogAliases` 的文档注释）。
2. **每行可以是两种角色之一，也可以两者都是**：family 行（裸片段 id，只给
   `context_window`，一个保守下界）与 specific 行（精确 shipped 模型 id，通常带
   `pricing`/`auto_compact_threshold`）。两者共存在同一个 `models:` 列表里，靠
   `context_window <= 0` / `pricing == nil` / `auto_compact_threshold <= 0` 这三个
   「没有意见」判据自然分流到各自的派生索引——不需要一个 `kind: family|specific`
   字段来分辨。
3. **覆盖优先级对 window 和 threshold 是同一把梯子**：显式 provider 配置
   （`ProviderConfig.ContextWindow` / `.AutoCompactThreshold`，经 `ProviderShape` 投影）
   > 目录命中 > 调用方自己的全局回退。`ResolveAutoCompactThreshold` 的文档注释原话
   「Precedence mirrors ResolveContextWindow」——这不是巧合而是约束：两条通路分别喂给
   pre-turn（`internal/api/http`）与 mid-turn（`internal/agent/orchestrator`）两套
   完全独立的 `CompactionConfig`，如果两个梯子的判据不对称，同一个模型在两条路径上
   会得到不同的「压缩什么时候触发」的答案，而用户看到的百分比与实际触发点分别来自
   这两条路径的不同半。
4. **查不到的模型必须安全降级，且降级必须可观察而不是可观察的沉默**：
   `ResolveContextWindow` 对未知模型落到 `WindowFromDefault`，`BuildProviders` 在
   这一分支记一条 `slog.Warn`（模型名、provider 名、给出的默认值全部在场）而不是
   报错——验收第 2 条「不阻止启动」与 `docs/compaction.md`/CLAUDE.md 一贯的「非致命
   启动失败打到 stderr、以子系统降级方式继续」姿态一致。`KnownAutoCompactThreshold`
   / `ResolveAutoCompactThreshold` 对「表里没有」的回答是显式的 `(0, false)`，调用方
   据此保留自己原有的全局阈值——**不是**返回一个隐含的 0 阈值（那会让每一轮都触发
   压缩）。
5. **压缩总开关只认全局 `CompactionConfig.Threshold`，按模型解出来的阈值只能在开关
   已经打开之后调整数值，不能把它重新打开。** `wrapCompaction` 的 `cc.Threshold <= 0`
   判据在按模型阈值参数被引入之前就是压缩的唯一开关；这次改动新增了第四个参数
   `threshold float64`，语义是「已解出的、可能来自目录/配置覆盖的值」——**只用于
   填充，从不参与决定是否启用**。理由：操作员显式把全局阈值设成 0（关闭压缩）是一个
   意图明确的操作决定；一个模型恰好在目录里有一条 `auto_compact_threshold` 数据不
   应该悄悄推翻它。同一约束在 pre-turn 一侧由 `ws_compaction.go` 的 `thresholdFor`
   实现：先判 `cc.Threshold <= 0` 直接返回（不看 `ProviderThresholds`），未命中才查
   per-model 表——次序颠倒会让评审 F-1 描述的那个洞重新出现：操作员关闭的全局压缩
   被一条目录/配置命中悄悄重新打开。

5b. **`threshold` 参数本身还携带第二种、与「填充数值」正交的语义（Ruling RC-4，
   评审 F-10）：为负是一个 provider 级别的显式关闭指令，与「没有意见」（`0`，落回
   全局）和「调整数值」（正数）都不同。** 三层必须用同一套判据，且都是在各自那条
   `cc.Threshold <= 0` 全局短路检查**之后**才检查的一个独立分支——不是同一个
   if：`ResolveAutoCompactThreshold`（`modelcatalog.go`，供给层，
   `p.AutoCompactThreshold < 0` 时原样返回 `(负值, true)`）、`thresholdFor`
   （`ws_compaction.go`，pre-turn 消费层，先判全局开关，再判
   `ProviderThresholds[model]` 存在且非零则原样返回，负值不被特殊拦截，直接
   传给调用方，调用方把 `<=0` 都当「关」）、`wrapCompaction`（`orchestrator.go`，
   mid-turn 消费层，`cc.Threshold <= 0` 之后新增 `if threshold < 0 { return m }`）。
   这不是对上一条「全局开关判据只能是 `cc.Threshold <= 0`」的违反：全局开关自己的
   判据字面未变，`threshold < 0` 管辖的始终只是「这一个已解析出的 provider」，且
   永远读不到、也写不回 `cc.Threshold`——两者必须保持互相独立，见后果段落的新约束。
6. **五个当前不消费的字段（`max_output`/`modalities`/`reasoning_efforts`/
   `truncation_policy`/`priority`）照样解析进结构体，只是没有 Go 代码读它们。**
   这是为后续 W-C-* 工作项（capability-roadmap 的其余 14 项，多数要读这些字段）
   预留 schema，不做二次迁移；`modelcatalog_test.go` 的
   `TestParse_AllSchemaFieldsRoundTrip` 钉住这五个字段确实被解析而不是被
   yaml 标签打字错误静默丢弃。

## 后果（Consequences）

- 新增或改价一个模型现在是编辑 `models.yaml` 一行，不触碰任何 `.go` 文件——验收
  第 1 条由 `internal/llm/eino::TestAcceptance1_NewModelIsADataFileEditOnly` 用一个
  仅存在于测试内 YAML fixture 里、任何 Go 源码从未提及的模型 id 直接证明。
- **不可违反的约束**：**窗口与阈值的覆盖优先级梯子必须保持逐字对称。** 任何一侧
  加一层新的中间来源（例如「组织级默认」），另一侧必须同一提交里跟进，否则
  pre-turn 与 mid-turn 会在同一个模型上给出不同判决——这正是本 ADR 背景段落里
  「两条路径必须一起改」那句 CLAUDE.md 承重注释的数据层版本。
- **不可违反的约束**：**`wrapCompaction` 的全局开关判据只能是 `cc.Threshold <= 0`。**
  `TestWrapCompaction_GlobalThresholdZeroStaysOffEvenWithACatalogHit` 钉住这条——
  谁把按模型解出的阈值接入这一判据，等于让一条目录数据能重新打开操作员关掉的功能。
  `ws_compaction.go` 的 `thresholdFor` 有同一约束的 pre-turn 版本，由
  `TestThresholdFor_GlobalOffStaysOffEvenWithACatalogHit` 钉住（F-1）。
- **不可违反的约束（W-C-04）**：**per-provider 的负值关闭（决策点 5b）与全局开关
  必须是两个永远不合并的独立判据。** `wrapCompaction` 的 `threshold < 0` 分支、
  `thresholdFor` 对负值的原样透传，都不得改写成读取或影响 `cc.Threshold` 本身——
  合并两者会让「全局开关只能是 `cc.Threshold <= 0`」这条约束失去意义（一个足够
  「负」的 per-provider 值就可能被复用来间接判断或改变全局开关的可达性）。
  `TestWrapCompaction_NegativeResolvedThresholdDisablesJustThisModel` 与
  `TestThresholdFor_NegativePerModelValueDisablesJustThatProvider` 分别在 mid-turn
  与 pre-turn 两侧钉住「全局开关仍为 ON、只有这一个 provider 被关闭」。
- **不可违反的约束**：**`auto_compact_threshold` 是窗口的分数（0~1 区间的比例），
  从不是绝对 token 数。** `buildAutoCompactThresholds` 原样传递该值，不做任何与
  窗口大小相关的换算；下游（`ctxcompact`）拿到分数后自己乘以已解出的窗口。把这两步
  合并会让「这个模型窗口翻倍后阈值该如何变化」这个问题失去唯一答案。
- **不可违反的约束**：**非正值（`<= 0`）的 `auto_compact_threshold` 必须从派生表中
  剔除，不能作为字面 0 出现。** 字面 0 会被 `thresholdFor`/`ResolveAutoCompactThreshold`
  的调用方误读成「每次都压缩」，而真实语义是「这条数据没有意见」。
- 定价与阈值用精确匹配、窗口用边界子串匹配，这个不对称是刻意保留（继承自
  `contextwindow.go` 既有设计）而非本 ADR 引入的新决定——本 ADR 只是把它推广到
  第三张表（阈值）时重申了同一条理由：网关前缀应该继承窗口下界，不应该继承定价或
  阈值这类「必须精确对应一次真实计费快照」的数据。
- 代价：三张派生索引（窗口、定价、阈值）现在都从同一次 `mustParseModelCatalog`
  解析派生，`models.yaml` 的一次解析失败会让全部三者一起在包 init 时 panic——这是
  刻意的（见 `mustParseModelCatalog` 文档注释：构建期缺陷与运行期「模型未知」是
  两个不同的问题，不应该用同一种「静默降级」处理），但也意味着这个文件不能再像
  从前两张独立 Go 字面量表那样「一张表语法错误、另一张表照常工作」。

## 关联

- 来源：`docs/superpowers/specs/2026-08-27-capability-roadmap-design.md` §5.1（W-C-01
  验收四条）与 §1.2 INF2 字段契约；CLAUDE.md「编排器」章节的压缩双路径段落。
- 相关代码落点：`internal/llm/eino/models.yaml`、`internal/llm/eino/modelcatalog.go`、
  `internal/llm/eino/contextwindow.go`、`internal/llm/eino/pricing.go`、
  `internal/llm/eino/provider.go`（`BuildProviders`）、
  `internal/agent/orchestrator/orchestrator.go`（`CompactionConfig.thresholdFor`、
  `wrapCompaction`）、`internal/api/http/ws_compaction.go`（`thresholdFor`）、
  `internal/bootstrap/bootstrap.go`（`providerThresholds` 接线）、
  `internal/config/config.go`（`ProviderConfig.AutoCompactThreshold`）。
- 相关 ADR：[ADR-0006](0006-compaction-unified-core-strict-window.md)（压缩统一核心与
  严格窗口）——本 ADR 不改变那份决策的核心（`ctxcompact.Run`），只改变喂给它的窗口与
  阈值现在各自有一个按模型解出的来源。
