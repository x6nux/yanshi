# ADR-0026: 内容安全拒答不触发跨 provider failover

- 状态：accepted
- 日期：2026-08-30

## 背景（Context）

W-C-13 给 `internal/llm/eino/resilient.go` 的 `runStream` 加了一条 mid-stream failover：`streamErr` 分支命中 `isNonRetryableClientErr` 时，链条推进到下一个 provider 重试整轮请求，而不是把错误直接抛给调用方。`isNonRetryableClientErr` 当时收了三类：`ClassClientError`（真 4xx）、`ClassContextOverflow`（上下文超限）、`ClassContentSafety`（内容安全拒答）——三类共同的表面特征是「同一个请求重放到*同一个* provider 也不会成功」，因此都被判定为「换一个 provider 有意义」。

复评（review-whole.md M-5）指出这条推理对前两类成立、对第三类不成立：**内容安全拒答否决的是请求本身，不是某一个 provider**。链条里的每一个 provider 面对的是逐字相同的消息历史，`ClassContentSafety` 的语义是"这个 provider 的安全策略判定这段内容不该被回答"——把同一段内容原样送给链条里下一个 provider，不是"换一条路线绕过一次故障"，而是**在没有告知调用方的情况下，逐个尝试直到找到一个安全判断更松的 provider**，用重试掩盖了第一个 provider 的安全判断，而不是让它浮出到调用方。

**扫描范围比 review 明确指出的更大。** M-5 原文只点名了 `runStream` 的 `streamErr` 分支（W-C-13 新增的那条），但 `ResilientModel.Generate` 里那个先于 W-C-13 就存在的链式循环——对每个 provider 遇到任何 `err != nil` 都无条件推进到下一个——同样会把 `ClassContentSafety` 的错误当成"换一个再试"的信号，且从未被 `isNonRetryableClientErr` 或任何分类判据收窄过。两条路径独立实现、控制流形状也不同（`Generate` 是无条件的 for 循环，`Stream` 是被 `isNonRetryableClientErr` 门控的分支），但都必须服从同一条裁定。

### 被否决的替代方案

**A. 只改 `runStream`，`Generate` 保持无条件推进不变。** 这只会让同一个语义缺陷在两条路径上给出不同答案：流式请求内容安全拒答后原样抛出，非流式请求内容安全拒答后静默换 provider 重试。调用方看到的行为取决于走哪条 API，而这两条路径在 `ResilientModel` 对外的契约里没有任何区别对待的理由。

**B. 把内容安全拒答整体从错误分类里摘出来，改造成一种"成功但被拒绝"的返回值。** 会改变 `ClassifyError`/`ErrorClass` 对外的返回类型契约，牵动 `isNonRetryableClientErr` 现有的全部消费点（它仍然要管"这个 provider 还要不要重试"），而本条要解决的只是 failover 这一个下游问题，不需要动分类体系本身。

## 决策（Decision）

**`ClassContentSafety` 的错误必须让调用方看到原始拒答，不得推进到链条里下一个 provider。**

落点：

- `Stream` 路径新增 `isFailoverEligibleErr`——`isNonRetryableClientErr` 去掉 `ClassContentSafety` 后剩下的两类（`ClassClientError`、`ClassContextOverflow`）。`streamErr` 分支的判据从 `isNonRetryableClientErr(recvErr)` 换成 `isFailoverEligibleErr(recvErr)`；`isNonRetryableClientErr` 本身不变，它仍然正确地回答"这个 provider 还要不要重试"，只是不再兼职回答"要不要换下一个 provider"。
- `Generate` 路径新增 `isContentSafetyRefusal`，在链式循环里对每个 provider 的错误做一次检查：命中即直接 `return nil, err`，不进入"链条耗尽"的包装（因为链条并未耗尽——只问过这一个 provider）。`Generate` 的循环没有可以收窄的既有分类闸门，所以这里是独立的早退，而不是复用 `isFailoverEligibleErr`。

## 后果（Consequences）

> 含**不可违反的约束**（加粗）。

- **不可违反的约束：`ClassContentSafety` 的错误经过 `ResilientModel`（无论 `Generate` 还是 `Stream`）时，链条中除第一个被问到的 provider 外，其余 provider 必须一次都不被调用。** 看守是 `internal/llm/eino::TestResilientModel_GenerateContentSafetyDoesNotFailOver` 与 `internal/llm/eino::TestResilientModel_StreamContentSafetyDoesNotFailOver`，两者都用调用计数断言第二个 provider 的调用次数为 0。
- **不可违反的约束：`isNonRetryableClientErr` 继续覆盖 `ClassContentSafety`，且继续门控"是否重试同一个 provider"。** 这条语义没有变化——内容安全拒答依然不该在同一个 provider 上重试。看守是既有的 `internal/llm/eino::TestClassifyError`（其中的 `ClassContentSafety` 分类用例）加上上面两条新测试（下游 failover 行为）共同覆盖，`isNonRetryableClientErr` 不因本条而改名或改行为。
- **不可违反的约束：真正的 failover 场景（4xx、上下文超限）不得回归。** 看守是既有的 `internal/llm/eino::TestResilientModel_StreamFailoverOnNonRetryableMidStreamErr`（404）与 `internal/llm/eino::TestResilientModel_FailoverToNext`（瞬时错误），本条改动后两者继续通过，证明 `isFailoverEligibleErr` 与 `isContentSafetyRefusal` 的边界画在了正确的位置。
- 代价：一个"某个 provider 对这段内容判了安全拒答，但链条里下一个 provider 本会放行"的请求，现在会在第一个 provider 就终止，而不是自动找到一个更宽松的判断。这是本条刻意接受的行为——把安全判断静默地多试几家绕过去，本身就是这条裁定要防止的事，不是要优化掉的摩擦。

## 关联

- 来源：review-whole.md M-5；裁定 RC-11。
- 相关代码落点：`internal/llm/eino/resilient.go`（`Generate`、`runStream`、`isFailoverEligibleErr`、`isContentSafetyRefusal`）、`internal/llm/eino/errclass.go`（`ClassifyError`/`ClassContentSafety`，未改动，仅被复用）。
- W-C-13（mid-stream failover 本身的引入）：本条是对它的收窄，不是撤销——`ClassClientError`/`ClassContextOverflow` 两类的 failover 行为不受影响。
