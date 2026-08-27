# 实测记录：模型运行时（M1–M10 + C6 + C8 + C9）

日期：2026-08-08
范围：`internal/llm/`、`internal/config/`
手法：在 loopback 上起 OpenAI 兼容桩 server，让**真的 yanshi 二进制**把它当 provider 去调；
能测到的一律测**服务端观察到的到达时刻 / 请求体 / 请求条数**，而不是读代码推断。

## 先验证通路（否则后面所有「通过」都是假的）

```
$ /tmp/ys exec -config /tmp/ysrun/config.yaml -p "hello" -output jsonl -timeout 60s
{"type":"agent_chunk","text":"STUB-REPLY-OK"}
{"type":"status","model":"stub-model-a","sessionId":"5ee5…","tokensIn":11,"tokensOut":7}
{"type":"done","text":"Done 0 tools uses 25 tokens 0s"}

$ cat /tmp/stub/requests.log
=== GET /v1/models
=== POST /v1/chat/completions at 2026-08-08T14:28:41.737013+08:00
Auth: Bearer stub-key
BODY: {"model":"stub-model-a","messages":[…],"stream":true,"tools":[…96 个…]}
```

桩确实收到了带正确 api_key 的请求、preflight 确实打了 `/v1/models`、回复确实流回了 TUI。
通路成立，后面的结论才有意义。

---

## 验证矩阵

| ID | 结论 | 依据 |
|----|------|------|
| M1 Retry-After + 429 冷却 | **FIXED** | 修前 5.0s/10.0s（盲目指数退避），修后 3.01s/3.01s（服务端要的值）|
| M2 上下文窗口目录 | OK | 7 种带前缀的名字全部解析出正确窗口 |
| M3 本地 provider 不套云端窗口 | OK | 6 种本地 base_url 全部拿到 8192 而非 262144 |
| M4 生成参数上线 | OK | wire body 里 `"temperature":0` 与「不设则整个 key 消失」两个方向都实测 |
| M5 模型怪癖学习 | OK | 请求 1 带 `$ref` 被拒 → 请求 2 不带 `$ref` 且成功；日志说得出原因 |
| M6 schema 净化 | OK | always/never 正反两侧都测，反侧证明 fixture 真的在造那些构造 |
| M7 QPM 限流 | OK | qpm=600/burst=1 实测到达间隔 100ms；qpm=0 实测 0.87ms；取消 152ms 生效 |
| M8 错误分类 | OK | 终态 4 种各 1 次请求，瞬时 6 种各 ≥2 次；含 500-引用-404 回归用例 |
| M9 清单发现与预检 | OK | 真二进制点名 typo 并给候选；404 不阻断（0s）、挂起 60s 被 5s 超时兜住 |
| M10 用量落盘 | OK | 真二进制 3 轮 → usage_log 6 行，provider/model/session/token 全对，聚合查询可用 |
| C6 溢出反应式恢复 | **FIXED** | 恢复本身 OK；但报错把**消息条数**当 token 报（`26 → 16 tokens`）|
| C8 token 计数 | OK | CJK 1.32 tokens/rune（chars/4 会低估数倍）；单调性实测 |
| C9 输出预留 / 硬上限 | OK | 三种窗口都实测预留了可观份额；本地拒绝的请求不发出去 |

---

## FIXED 1 — M1：openai 这条路上 Retry-After 一直是死的

**这是本轮最重要的发现，而且它长成了本仓反复栽跟头的那个形状。**

`errclass_test.go` 里 M1 的每个零件都有测试而且全绿：`RetryAfterFromHeader` 两种 RFC 9110
形态都解析、`HeaderError` 把 header 送进 `ClassifyError`、`RateLimitBackoffWith` 优先用服务端
给的冷却。**但这些测试全都是自己手搓一个 `http.Header` 递进去的 —— 而生产环境里没有任何东西
会那么做。**

根因在依赖里：go-openai 的 `handleErrorResp`（`client.go:317`）读完 body 就把 `resp.Header`
丢了，`APIError` 只留 `HTTPStatusCode`、`RequestError` 只留 body 字节。于是 `HeaderError`
只有 anthropic 与 openai-responses 两个自己持有 `*http.Response` 的适配器造得出来，
**而 `kind: openai` 是默认值、是所有 OpenAI 兼容网关 / vLLM / 本地运行时用的那一条。**

### 修前（真二进制，桩返回 429 + `Retry-After: 3`）

```
POST #1 t=1.013
POST #2 t=6.022     ← 等了 5.0s
POST #3 t=16.032    ← 等了 10.0s
```

5s→10s 是无 Retry-After 时的盲目指数退避。服务端说的 3 秒被完整忽略。

### 修了什么

新增 `internal/llm/eino/retryafter.go`：

- `headerCaptureTransport` —— RoundTripper，把失败响应（>=400）的 header 存进**每次调用独立**的
  holder（挂在该请求自己的 context 上）。响应本身原样返回、body 一个字节都不读，
  否则 SDK 就解析不出错误消息了。
- `HeaderAwareModel` —— 消费侧，在 `Generate`/`Stream` 外面装 holder，出错时把 header 接回错误上，
  复用已有的 `HeaderError`。已经是 `HeaderError` 的不重复包。

holder 做成**每次调用一个**是刻意的：放在 transport 上会被并发请求共享，子代理 fan-out 时会把
A provider 的 Retry-After 贴到 B provider 的错误上 —— 那比没有 header 更糟，因为它是**静默错误**。

### 修后（同一个场景，同一个二进制）

```
POST #1 t=2.045
POST #2 t=5.056     ← 等了 3.01s
POST #3 t=8.064     ← 等了 3.01s
```

再拿 `Retry-After: 2` × 三次 429 复核：间隔 `[2.01, 2.01, 2.01]`（盲目退避会是 5/10/20）。

HTTP-date 形态也实测通过：`Retry-After: <now+4s>` → 等 3.25s（HTTP-date 只有秒级精度）。
超长值 `Retry-After: 99999` 被 `MaxRetryAfter` 钳到 5 分钟 —— 这条不能真等，所以断言的是钳位
本身，但走的是**真 http 栈产生的真错误**。

### 探针（按「先提交再变异」的规矩，改的是已落盘的修复）

把 `return NewHeaderAwareModel(inner), nil` 退回 `return inner, nil`：

```
--- FAIL: TestM1_RetryAfterHeaderIsHonouredOverTheWire
    observed retry gap = 2.004632542s (server asked 300ms; blind fallback is 2s)
    retry gap 2.004632542s reached the blind fallback 2s — the Retry-After header was ignored
--- FAIL: TestM1_RetryAfterHTTPDateIsHonouredOverTheWire
--- FAIL: TestM1_AbsurdRetryAfterIsClampedNotObeyed
    retryAfter = 0s, want it clamped to 5m0s
--- FAIL: TestM1_HeaderReachesClassifierFromTheOpenAIAdapter
    no HeaderError in the chain: the openai adapter's response headers were dropped
```

5 条里 4 条变红。还原后全绿。

---

## FIXED 2 — C6：报错把消息条数当 token 报

C6 的恢复逻辑本身是对的（实测：38608 字节被拒 → 强制压缩 → 22869 字节重发 → 成功，
且只重试一次）。但两次都失败时那条给操作员看的错误是这样的：

```
context overflow persisted after forced compaction (26 → 16 tokens)
```

`overflowRetryError.Before/After` 的 doc 注释写的是「estimated token counts」，
`adaptive.go` 传进去的却是 `len(req.msgs)` —— **消息条数**。

这比没有数字更糟：没有模型的窗口是 16 token，操作员读到这行只会得出「压缩把历史毁了」，
而这条消息存在的全部目的（判断该调大 `context_window` 还是该去找那个不可分割的大段）
要基于两个小了三个数量级的数字来做。

修法：在 `retryError` 里用 `ctxcompact.EstimateTokens` 现场量，消息条数继续只当**是否真的
缩了**的判据（它对这件事是可靠的）。修后：

```
context overflow persisted after forced compaction (15304 → 9072 tokens)
```

探针：把 `Before: msgsBefore` 改回去 →
`Before=26 is not larger than the 26-message count: the field documented as a TOKEN count is
reporting messages` 变红。还原后绿。

---

## 逐条实测细节

### M2 / M3 上下文窗口（OK）

窗口是压缩阈值的**分母**，猜高的后果不对称：压缩永不触发，请求要么被拒、要么被服务端静默
砍掉 prompt 头（system prompt 和工具定义先没，会话看起来还在跑但模型已经看不见自己的指令了）。

带前缀的名字全部穿透（实测）：`anthropic/claude-sonnet-5`、`openrouter/anthropic/claude-opus-4-8`、
`us.anthropic.claude-sonnet-5-v1:0` → 200000；`azure/gpt-4o` → 128000。
`claude-2.1` → 100000（没被 `claude` catch-all 遮住）。未知模型 → 保守回退 131072。

M3：同一个 `qwen3-coder`（目录里明确是 262144），换成 6 种本地 base_url
（loopback / `localhost` / 三个 RFC1918 段 / `*.local`）全部拿到 8192。
`context_window: 65536` 覆盖成功、`local: false` 能把 LAN 网关放回目录。

### M4 生成参数（OK）

桩回显请求体，实测：

```
配置 max_tokens/temperature/top_p → {"model":…,"max_tokens":12345,"temperature":0.25,"top_p":0.77}
配置 temperature: 0              → {"model":…,"temperature":0}
什么都不配                        → {"model":…,"messages":[…]}    ← key 整个消失
```

第二三行就是指针类型存在的理由，而且**只有在 wire body 上才区分得出来**。

### M7 限流（OK，实测到达速率）

```
qpm=600 burst=1，4 个并发：gaps=[100.5ms 99.1ms 101.4ms]，首末跨度 302ms
qpm=0，4 个串行：871µs（不限速确实零开销）
qpm=1 burst=1 用掉令牌后取消：152ms 返回 context canceled，第二个请求没有发出去
每模型独立：一个模型桶空时，另一个模型等了 573µs
```

取消那条有实际意义：一次 fan-out 排在 20 QPM 后面能排出几分钟，Ctrl-C 不该等完整个队列。

### M8 错误分类（OK，含回归用例）

按**请求条数**判定（可重试 = 多于 1 次）：

```
400/401/403/404 → 各 1 次请求（短路，不重试）
500/502/503/504/408/529 → 各 2 次（重试且恢复）
429 → 2 次
上下文溢出 400 → 1 次（原样重试只会复现，必须先压缩）
connection refused → 分类为可重试
```

主控点名的那个回归用例：HTTP 状态 500、body 里写着 `backend returned 404 from origin`
→ **2 次请求**（判为可重试）。配对的反向控制也在：真 404 仍然 1 次
（修法不能是「把所有 404 变成可重试」）。

### M9 预检（OK，真二进制）

```
$ /tmp/ys exec -config config.m9.yaml …   # model 写成 "stub-model-A-typo"
{"level":"WARN","msg":"configured model not advertised by provider","provider":"stub",
 "model":"stub-model-A-typo","closest":"stub-model-a, stub-model-b, gpt-4o-mini",
 "effect":"calls to this model will fail with a client error until the name is corrected"}
```

不阻断启动（这对内网 provider 是常态）：

```
/v1/models 返回 404     → 整个 exec 耗时 0s，turn 正常完成
/v1/models 挂起 60s     → 整个 exec 耗时 6s，turn 正常完成（被 5s DefaultDiscoveryTimeout 兜住）
```

另外验证了没配 `base_url` 的 provider **不会被探测** —— 否则等于把 api_key 发给一个操作员
从没指定过的主机。

### M10 用量落盘（OK；主控说的那个假阴性已复核）

主控确认过的假阴性：`--fake-model` 绕开 provider 路径，所以 usage_log 空是**预期**。
用桩 provider 走真实路径复测：

```
$ for i in 1 2 3; do /tmp/ys exec -config /tmp/m10/config.yaml -p "turn $i"; done
$ sqlite3 /tmp/m10/yanshi.db "select * from usage_log"
id  ts          provider  model         sess      pt  ct  cached  cache_hit
1   1786172539  openai    stub-model-a  17a98408  11  7   0       0
…（6 行：每轮 2 次真实 provider 调用 —— turn 本身 + completion judge）

$ 按天/按模型聚合
day         model         provider  calls  pt  ct
2026-08-08  stub-model-a  openai    6      66  42
```

包级另测了流式路径：**一次调用一行**（provider 把 usage 放在最后一个 chunk，
按 chunk 累加会把一次调用的 token 乘以 chunk 数）；失败调用零行；sink 报错不影响 turn 成功。

### C8 / C9（OK）

C8 方向性（估算必须偏高，偏低会让压缩门开得太晚、然后吃 400）：

```
ascii prose          0.50 tokens/rune
dense json tool args 0.47
cjk                  1.32     ← chars/4 会低估数倍
mixed cjk and code   0.70
```

C9 输出预留（实测三种窗口下 `CheckContextLimit` 能接受的最大输入）：

```
window 8192   → 最大接受 4982   （预留 3210）
window 32000  → 最大接受 19903  （预留 12097）
window 128000 → 最大接受 79588  （预留 48412）
```

以及本地拒绝确实不发请求：512-token 窗口 + 装不下的历史 → 桩只看到 1 次请求。

---

## 留下的可重跑测试

全部在 `internal/llm/eino/`，桩 server 走 `httptest`（loopback、进程内），
**零外部依赖，CI 可跑，`-race` 干净**（实测 8.1s 通过）。

| 文件 | 内容 |
|------|------|
| `stubprovider_test.go` | 桩 server 与 harness。规矩写在包头：必须走 `BuildProviders`，断言必须落在「桩看到了什么」和「真的花了多久」上 |
| `m1_retryafter_wire_test.go` | 5 条，含那条 dropped-header 回归 |
| `m2m3_window_wire_test.go` | 6 条，窗口解析 + 本地覆盖 + windows map 键一致性 |
| `m4_genparams_wire_test.go` | 4 条，wire body 上的参数存在/缺失 |
| `m5m6_adaptive_wire_test.go` | 6 条，请求 N 与 N+1 对比 + 学习可观测 + 误学防护 |
| `m7_ratelimit_wire_test.go` | 4 条，实测到达速率 |
| `m8_errorclass_wire_test.go` | 7 条（含子测试），按请求条数判定 |
| `m9_preflight_wire_test.go` | 5 条，含真挂起的 server |
| `m10_usage_wire_test.go` | 6 条，含流式一次一行 |
| `c6_overflow_wire_test.go` | 6 条，含那条单位回归 |
| `c8c9_budget_wire_test.go` | 5 条 |

顺带修了桩自己的两个坑，都记在注释里，因为它们各骗了我一轮：

1. **桩对流式请求回了 completion 对象** —— `ctxcompact` 的摘要器是流式的，于是它收到空响应，
   报出来是 `summarizer returned nothing`，看起来完全像压缩坏了。现在 `stubResponse.Content`
   按客户端要的形态渲染。
2. **桩的挂起用了 `time.Sleep`** —— handler 活得比放弃它的客户端还久，`httptest.Close`
   于是把整个测试二进制堵满 30s。改成等 `r.Context().Done()`，5.0s 就回来了。

还有一个不是坑而是**证据**：桩最早对所有请求都回 7 个字符的 `STUB-OK`，强制压缩全部失败于
`summary is 7 runes; 15878 runes of input require at least 15` —— 那是 C10 摘要质量门在真的干活。
现在桩会回一段像样的摘要。

## 没做 / 做不到的

- **NOTRUN：真 provider 调用。** 按要求不用付费 API；环境里的 `ANTHROPIC_API_KEY` 对 Anthropic
  无效（主控已实测 401）。退而求其次：桩 server 反而能造出真调用造不出来的场景
  —— 精确的 `Retry-After` 秒数与 HTTP-date、`Retry-After: 99999`、可控的 400 文本、
  60 秒挂起、精确的 429 次数。M1 那个缺陷就是靠「精确 3 秒」量出来的。
- **`internal/store` 的聚合实现没动**（是别人的领域）。M10 的聚合我是用真 DB 跑 SQL 验证的，
  不是读代码。
- **`internal/ctxcompact` 没动**（别人的领域）。C8/C9 只从模型层这一侧断言其后果。

## 其它 agent 造成的编译问题

无。`go build ./...` 全程干净。

## 复核命令

```sh
go build ./...
go test ./internal/llm/... ./internal/config
go test ./internal/archtest ./internal/bootstrap
go test -race ./internal/llm/eino -run 'TestM[0-9]|TestC[689]'
```
