# Yanshi 示例

可复制即跑的示例。**全部用 `--fake-model`，零 API key、零外部 CLI**。CI（`.github/workflows/docs.yml`）验证它们可编译 / 可冒烟。

| 示例 | 说明 | 怎么跑 |
|---|---|---|
| [headless-exec/](headless-exec/) | 单 turn 文本进/出 | `bash headless-exec/run.sh` |
| [headless-batch/](headless-batch/) | JSONL 批处理多 prompt | `bash headless-batch/run.sh` |
| [sdk-typescript/](sdk-typescript/) | TypeScript SDK 端到端 | 见其 README |
| [sdk-python/](sdk-python/) | Python SDK 端到端 | 见其 README |
| [custom-tool/](custom-tool/) | 用 Go 写一个自定义工具 | 见其 README |
| [custom-skill/](custom-skill/) | 自定义 SKILL.md 目录结构 | 见其 README |
| [goalloop-config/](goalloop-config/) | `yanshi goal` 两轮演示 + 配置 | `bash goalloop-config/run.sh` |

> 示例脚本会 `go build` 并在需要时写一份最小 `config.yaml`（已 gitignore）。loopback（127.0.0.1）免 bearer token。

## 示例驱动的 API gap（反馈后续 batch）

custom-tool 示例暴露一个 gap：yanshi 暂未提供**公开的"外部工具注册点"**——工具在 `internal/bootstrap.Build` 装配，所以一个外部构造的 `*tools.GuardedTool` 目前无法注入运行中的 server 而不改动 internal。本批**不**临时导出 internal 符号、**不**在示例里 hack；详见 [custom-tool/README.md](custom-tool/README.md)。该 gap 已记录，留给后续 batch 决定是否新增公开注册 API。


## 快速冒烟

```sh
go build -o yanshi ./cmd/yanshi
./yanshi exec --fake-model -p "hello"
```
