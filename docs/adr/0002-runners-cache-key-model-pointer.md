# ADR-0002: runners 缓存以 model 指针为键

- 状态：accepted
- 日期：2026-07-22

## 背景（Context）

编排器把 Eino 的 `adk.ChatModelAgent` 包裹在 ReAct 循环里。每个 `ChatModelAgent` 绑定一个具体的 model。会话运行中，用户可能用 `/model` 切换 provider（从而切换底层 model）。如果每次切换都重建整个编排器，开销大且会丢失 ReAct 的中间状态。

## 决策（Decision）

编排器用一个 `runners sync.Map` 缓存 `adk.Runner`，**以 `model.BaseChatModel` 指针为键**。`/model` 切换只是换了一个 model 指针，按新指针在缓存里查；查不到才新建一个 runner 并缓存。同一个 model 指针复用同一个 runner。

## 后果（Consequences）

- 按 turn 切换 model 无需重建编排器，`/model` 在会话中途切换 provider 即由这条实现。
- **不可违反的约束**：bootstrap 必须保证同一 provider 的 model 对象**复用**，避免每次构造一个新指针导致缓存失效（指针不同 = 不同的缓存键 = 反复重建 runner）。
- 代价：缓存的 key 是裸指针，依赖 model 对象身份的稳定性；这是有意为之的权衡。

## 关联

- 来源：synthesis §9.1；`CLAUDE.md`「编排器」段。
- 代码落点：`internal/agent/orchestrator/`（runners `sync.Map`）。
