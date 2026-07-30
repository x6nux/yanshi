# headless-batch

JSONL 批处理：把多个 prompt 喂给 `exec --input jsonl`，每个 prompt 一个 turn。零 API key。

`sample.jsonl` 每行一个 JSON 对象 `{"prompt": "..."}`（可选 `"resume": "<sessionId>"` 续接之前的会话）。

## 跑

```sh
bash run.sh
```

或手动：

```sh
go build -o yanshi ./cmd/yanshi
./yanshi exec --fake-model --input jsonl --file examples/headless-batch/sample.jsonl
```

`--input jsonl` 把每行解析成一个 prompt；`--file` 从文件读（默认 stdin）。详见 [../../docs/user-guide/entrypoints.md](../../docs/user-guide/entrypoints.md)。
