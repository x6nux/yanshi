# headless-exec

最小单 turn 文本进/出：用 `--fake-model` 跑一个 prompt，打印 assistant 文本。零 API key。

## 跑

```sh
bash run.sh
```

或手动：

```sh
go build -o yanshi ./cmd/yanshi
./yanshi exec --fake-model -p "hello"
```

`exec` 把 assistant 文本打到 stdout（`-output text`，默认）；`-output jsonl` 改为每事件一个 JSON 对象。退出码稳定：0 ok / 1 运行错误 / 2 用法 / 124 超时 / 130 取消。详见 [../../docs/user-guide/entrypoints.md](../../docs/user-guide/entrypoints.md)。
