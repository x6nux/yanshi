# sdk-python

Python SDK 端到端示例：建 thread → 起 turn 消费 item 流 → cancel。用 `yanshi_sdk`（`sdk/python/src`）。

## 跑

两步（两个终端）：

```sh
# 1. 起一个 fake-model 后端（loopback 免 token）
./yanshi serve --fake-model -addr 127.0.0.1:8080

# 2. 跑示例（PYTHONPATH 指向 SDK 源码）
PYTHONPATH=sdk/python/src python examples/sdk-python/main.py
```

## 解析检查

```sh
python -m py_compile examples/sdk-python/main.py
PYTHONPATH=sdk/python/src python -c "import yanshi_sdk"
```

无需安装——直接用源码（`PYTHONPATH=sdk/python/src`）。详见 [../../docs/api/sdk-python.md](../../docs/api/sdk-python.md)。
