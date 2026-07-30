# ADR-0001: UnknownToolsHandler 以结果返回而非 error

- 状态：accepted
- 日期：2026-07-22

## 背景（Context）

ReAct 编排器（Eino ADK）在模型输出一个工具调用时分发它。当模型幻觉出一个不存在的工具名（或拼错了一个真实工具名）时，编排器需要一个处理策略。最直白的做法是返回一个 Go `NodeRunError`，让 ADK 把它当作 turn 级错误。

但这样会**中断整个 turn**：一次工具名幻觉就让一整轮对话失败，模型连自我纠正（"哦，那个工具不存在，我换一个"）的机会都没有。工具名幻觉在长工具清单下是高频噪声，把它升级为 turn 终止错误代价过高。

## 决策（Decision）

`UnknownToolsHandler` 把幻觉的工具名**作为工具结果**（一条文本说明"该工具不存在"）返回给 ADK，而不是作为 Go error。ADK 因此把它当成本轮的一个普通工具结果回喂给模型，模型可以据此重试。

## 后果（Consequences）

> 这是编排器的**幻觉容忍**机制，是承重设计。

- 模型获得自我纠正的机会，单次幻觉不浪费一整轮。
- **不可违反的约束**：`UnknownToolsHandler` **永远不要改为返回 `NodeRunError`**——那会中断整个 turn，破坏幻觉容忍承诺。改动工具分发逻辑时必须保留"以结果返回"这一行为。
- 代价：turn 内可能多耗一两步让模型收敛到真实工具；用 fake model 测试时这一路径被显式覆盖。

## 关联

- 来源：synthesis §9.1；`CLAUDE.md`「编排器」段。
- 代码落点：`internal/agent/orchestrator/`（UnknownToolsHandler 绑定）。
