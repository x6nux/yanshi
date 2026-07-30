# spawn_agent 工具设计

## 目的

允许主 Agent（orchestrator）在同一个进程中创建子 Agent 来执行特定任务，
并返回结果。支持同时创建多个子 Agent 并发执行。

## 约束

1. 子 Agent 是**同步**执行的 — 工具调用会阻塞直到子 Agent 完成
2. 每个子 Agent 拥有独立的 ReAct 循环（ADK ChatModelAgent）
3. 子 Agent 共享父 Agent 的模型和工具集
4. 支持可选的 VCS 工作树隔离
5. 通过 PermissionProfile 控制安全边界

## 接口

### 工具：`spawn_agent`

**参数：**
| 参数名 | 类型 | 必需 | 默认 | 描述 |
|--------|------|------|------|------|
| `goal` | string | 是 | — | 子 Agent 的任务描述 |
| `timeout_seconds` | integer | 否 | 300 | 最大执行时间（秒） |
| `model_name` | string | 否 | "" | 使用的模型名（空=父 Agent 的模型）|

**返回结果：**
```json
{
  "result": "子 Agent 的最终输出文本",
  "agent": "sub-<uuid>"
}
```

**错误返回：**
```json
{
  "error": "错误描述"
}
```

## 实现

### 文件

1. `internal/tools/spawn.go` — SpawnAgentTool 实现
2. `internal/tools/spawn_test.go` — 单元测试
3. `internal/bootstrap/bootstrap.go` — 注册工具

### SpawnAgentTool 结构

```go
type SpawnAgentTool struct {
    model       model.BaseChatModel       // 用于子 Agent 的 LLM
    tools       []tool.BaseTool           // 子 Agent 可用的工具
    profile     guard.PermissionProfile   // 子 Agent 的权限
    store       *store.Store              // 存储引用（用于 VCS 等）
    vcs         *vcs.VCS                  // VCS 实例（可选）
    vcsRepoID   string                    // VCS 仓库 ID（可选）
    models      map[string]model.BaseChatModel  // 可选模型切换
}
```

### 子 Agent 创建流程

1. 解析参数（goal, timeout, model_name）
2. 选择模型（model_name → models map, 或使用默认模型）
3. 创建子 Agent 的 VCS scope（可选工作树）
4. 创建 `orchestrator.Orchestrator` 实例
5. 设置超时 context
6. 调用 `orchestrator.Query()` 执行任务
7. 返回结果
8. 清理 VCS 工作树（如有）

## 测试策略

1. 使用 FakeModel 测试工具的基本功能
2. 验证子 Agent 能收到 goal 并返回结果
3. 验证超时机制
4. 验证多个并发子 Agent
5. 验证错误处理
