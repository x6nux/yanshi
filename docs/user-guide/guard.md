# 安全守卫（Guard）

yanshi 的安全核心是 guard：一个**无状态、fail-closed**的四维权限检查器。每次工具调用前，guard 按 tools → fs → shell → net 四维检查，**第一个不满足的维度即短路拒绝**。

## 四个维度

| 维度 | 检查什么 |
|---|---|
| **tools** | 工具名 glob 白名单。**空 `Allow` 一律拒绝**（见下）。 |
| **fs** | 读 / 写路径的 glob 白名单。 |
| **shell** | 命令策略（`allow`/`deny`/`allowlist`）+ 白名单 pattern。 |
| **net** | host 白名单 + 是否允许出站。 |

## fail-closed：空 Allow 拒绝一切

> 这是架构级安全承诺，不可妥协。详见 [../adr/0003-guard-fail-closed-empty-allow.md](../adr/0003-guard-fail-closed-empty-allow.md)。

空的工具白名单不是"无约束"，而是"什么都不允许"。新增任何工具都必须在 profile 里**显式**配权限才能被调用——不会因开发者忘了配权限而静默放行。

## shell 元字符硬拦截

`shell_run` 在 glob 白名单之外**额外硬拦截**元字符：`&&`、`||`、`;`、`|`、反引号、`$()`、`>`、`<`、换行。无论 glob 配置如何，含这些字符的命令一律拒绝（见 [../adr/0004-guard-stateless-and-shell-metachar-hardblock.md](../adr/0004-guard-stateless-and-shell-metachar-hardblock.md)）。

> 需要顺序执行多条命令时，**改为多次顺序调用**，不要试图放开元字符。

## profile

权限来自 `profiles:` 配置 map（见 [configuration.md](configuration.md)）。每个 profile 是一个具名四维策略；agent 的 `profile` 字段引用一个 profile 名。示例里的 `coding` profile 给了一个"全工具 + 仓库读写 + allowlist shell + 出站网络"的例子。

## 交互式权限模式

在 profile 之上叠加交互式模式（仅 WS 路径可用，见 [tui.md](tui.md)）：

| 模式 | 行为 |
|---|---|
| `default` | 每个需授权操作都询问 |
| `allow-edits` | 自动放行编辑类操作 |
| `yolo` | 自动放行所有（谨慎） |
| `auto` | 自动模式 |

> SSE 备用路径用静态 profile，**不支持**交互式弹窗（见 [../adr/0010-sse-static-profile-no-interactive-perm.md](../adr/0010-sse-static-profile-no-interactive-perm.md)）。
