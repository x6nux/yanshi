# ADR-0003: Guard fail-closed——空 Allow 拒绝一切

- 状态：accepted
- 日期：2026-07-22

## 背景（Context）

Guard 是 yanshi 的安全核心，做四维权限检查（tools/fs/shell/net）。一个常见的失败模式是"开发者新增了一个工具，但忘了在 profile 里给它配权限"。如果 guard 在这种情况下默认放行（fail-open），新工具就以全权限静默运行——一个权限提升漏洞。

## 决策（Decision）

`checkTools` 在 `Tools.Allow` 为空时**一律拒绝**（fail-closed）。空的工具白名单不是"无约束"，而是"什么都不允许"。新增任何工具都必须在 profile 里**显式**配权限才能被调用。

## 后果（Consequences）

> 这是架构级安全承诺，不可妥协。

- 新增工具默认不可调用，开发者必须显式 opt-in；权限提升需要主动配置，不会因遗忘而发生。
- **不可违反的约束**：**绝不可改为默认允许**（空 Allow = 放行）。`Tools.Allow` 为空时 `Check` 必须短路拒绝。
- 代价：profile 配置更繁琐（每个工具都要列）；这是安全换便利的刻意取舍。

## 关联

- 来源：synthesis §9.2；`CLAUDE.md`「Guard」段。
- 代码落点：`internal/guard/`（`checkTools`）。
