# custom-tool

用 Go 写一个最小自定义工具（`echo`），展示 yanshi 工具的**构造**与**权限**模型。

## 跑

```sh
go run ./examples/custom-tool
```

输出三段：构造信息 → 无 profile 时被 fail-closed 拒绝 → 绑定 permissive profile 后成功 echo。

## 构造（公开面）

工具用公开 API 构造，与内置工具同一形状：

```go
params := schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
    "text": {Type: schema.String, Required: true, Desc: "..."},
})
stream := tools.SyncStream(func(ctx, argsJSON) (string, error) { ... })
tool := tools.NewGuardedTool("echo", "Echo", "...", 5*time.Second, params, stream)
```

- `tools.NewGuardedTool`（`internal/tools`）：构造一个受 guard 保护的工具。
- `schema.NewParamsOneOfByParams`（`github.com/cloudwego/eino/schema`）：从参数表生成 JSON Schema。
- `tools.SyncStream`：把同步函数包成 StreamFunc。
- `tools.WithProfile(ctx, guard.PermissionProfile{...})`：把权限 profile 注入 context（编排器每个 turn 都这么做）。

GuardedTool 是 **fail-closed** 的（见 [../../docs/adr/0003](../../docs/adr/0003-guard-fail-closed-empty-allow.md)）：context 里没绑 profile 就拒绝。

## API gap（已记录，不在示例里 hack）

> yanshi 暂未提供**公开的"外部工具注册点"**——工具在 `internal/bootstrap.Build` 里装配。因此本示例展示的是**构造 + Tool 契约 + 权限模型**；要把 `echo` 真正暴露给模型，目前需要把它加到 `internal/bootstrap` 的工具集里。

这是"示例驱动的外部 API gap"，已反馈给后续 batch（见 [../README.md](../README.md) 汇总）。本示例**不**临时导出 internal 符号、**不**绕过 guard。
