---
name: tui-debug
description: Use when debugging the yanshi Bubble Tea TUI - starting the real TUI in a terminal, sending keystrokes, and reading the rendered screen back as text. Needed because the alt-screen TUI cannot be driven through a pipe, so `timeout 5 ./yanshi` runs blind. Also use when verifying a TUI change actually renders, reproducing a startup crash, or checking key bindings end-to-end.
---

# tui-debug

用 tmux 当终端模拟器驱动 alt-screen TUI。tmux 负责 VT100 解析与屏幕状态，
本工具只做进程编排与等待。

## 为什么需要它

`yanshi` 的 TUI 跑在 alt-screen 里，无法通过管道驱动。`internal/cli/tui` 的
单测验证的是 `Model.Update`/`View` 的返回值，不是真实终端里那块像素 ——
启动崩溃、渲染错位、按键失效、宽字符截断这几类问题，单测全绿的同时可以
稳稳复现在真机上。

## 用法

    go build -o tuidbg ./cmd/tuidbg     # 或全程用 go run ./cmd/tuidbg

    ./tuidbg start -cwd "$PWD" -- ./yanshi --fake-model -inprocess
    ./tuidbg shot -wait 'Ctrl\+Enter' -timeout 20
    ./tuidbg send '帮我看看这个文件'
    ./tuidbg key Enter
    ./tuidbg shot -wait 'assistant:' -timeout 30
    ./tuidbg stop

**flag 是单横线**（Go 的 `flag` 包），不是 `--`。多会话并行加 `-session 名字`
（每个子命令都要带）。

### 子命令

| 命令 | 作用 |
|---|---|
| `start [-cols N] [-rows N] [-cwd DIR] -- 命令...` | 起 detached 会话 |
| `send 文本` | 打字，不回车。特殊字符无需转义 |
| `key 键名...` | 发按键。tmux 词表：`Enter` `Escape` `Tab` `Up` `Down` `BSpace` `C-c` `C-u` |
| `shot [-wait 正则] [-timeout 秒] [-ansi] [-png 路径]` | 抓屏到 stdout；`-png` 另存一张 PNG |
| `stop` | 杀会话 |

默认：`-session tuidbg`、`-cols 100`、`-rows 30`、`-timeout 10`。
单个子命令的 flag 详情跑 `tuidbg <子命令> -h`。

### shot 的退出码

| 码 | 含义 |
|---|---|
| 0 | `-wait` 命中；或没给 `-wait`；**或被测程序以 0 退出**（此时 `-wait` 可能从未命中 —— 看 stderr） |
| 124 | 等超时。**最后一屏仍会打印**，照着它看卡在哪 |
| 3 | `-png` 渲染失败（**会盖掉上面那些码**）。stderr 有原因 |
| 1 | 会话不存在、抓屏失败，或 `-wait` 不是合法正则 |
| 其它 | 被测程序的真实退出码。它已经退出了 |

`-wait` 遇到已退出的进程会立即短路，不会空等满 timeout。

### `-png`：把这一屏渲染成图片

    ./tuidbg shot -wait 'Ctrl\+Enter' -png /tmp/tui.png

文本抓屏丢掉了颜色、对齐和宽字符布局，PNG 留得住 —— 给人看，或者给
看得了图的 agent 看。实现是 `capture -e` 的 SGR 序列 → 直接光栅化成 PNG，
**全程纯 Go，不起浏览器、不调任何外部二进制**，固定深色主题
（背景 `#1e1e1e`、前景 `#d4d4d4`）。

字体从系统里按链查找（macOS 走 `Menlo.ttc`，中文回退 `Songti.ttc`/
`STHeiti Light.ttc`；Linux 走 `DejaVuSansMono.ttf`/`NotoSansMono-Regular.ttf`）。
**一个都加载不出来时返回 error 而不是给一张空白图** —— 这个工具的意义就是
那张图可以被信任。

**不给 `-png` 时行为逐字节不变**（stdout/stderr/退出码三样都一致）。

## 坑

- **`--wait` 的锚点要挑得准，两个条件缺一不可。**
  ①**locale 无关**：界面文案走 i18n，输入框英文是 `Type a message`、中文是
  `输入消息…`，选错语言 → 稳定超时 124（响亮，好排查）。
  ②**只在 TUI 接管屏幕后才出现**：`yanshi` 满足①却不满足② —— 它也出现在
  启动 stderr（`yanshi: logs -> …`）里，实测启动后 0.1–0.3s 内它已命中而
  屏幕只有 5 行，`shot` 于是 **rc=0 返回残屏**，静默假绿，比超时糟得多。
  用 `Ctrl\+Enter`（输入框提示语内，两条都满足；反斜杠是因为 `--wait` 收正则）。
- **`$?` 为 0 不等于锚点命中。** 被测程序以 0 退出时 `shot` 也返回 0（退出码
  透传优先于 `--wait`），此时 stderr 会写 `（--wait 未命中即已退出）`。要断言
  "画面上真的出现了 X"，就得检查 stdout 内容，不能只看退出码 —— 这与上一条
  是同一个道理的两种触发方式。
- **`start` 记得带 `--cwd`。** 不带时 pane 用的是 **tmux server 的**默认目录，
  不是你 shell 的 cwd —— server 可能是几天前在别处起的，相对路径会
  command not found。传 `--cwd "$PWD"` 最省心。（另注：`--cwd` 指向不存在的
  目录时 tmux 会静默忽略它、用继承的 cwd，所以路径写错不会报错。）
- **TUI 正常退出后屏幕是空的。** 终端会切回主屏幕，alt-screen 的内容按语义
  就该消失，任何工具都救不回。能救的是崩溃时来不及切屏的那类，以及退出码。
- **`--wait` 的正则匹配的是渲染后的屏幕**，不是原始输出。TUI 会截断长行、
  插入边框字符，所以匹配短的稳定片段（`assistant:`）比匹配整句可靠。
- **窗口尺寸影响渲染**。默认 100x30。复现布局问题时用 `--cols`/`--rows`
  对齐用户的终端尺寸。
- **`-png` 失败会盖掉正常的退出码，这是有意的。** 渲染失败一律返回 `3`，
  哪怕锚点命中了、或者被测程序本来要报别的码。因此 **`3` 有歧义**：被测程序
  自己以 3 退出时也是这个码。分辨方法看 stderr，两种情形各打一行且互斥：
  成功是 `PNG 已写入 <路径>`，失败是 `PNG 渲染失败：<原因>`。方向是刻意选的 ——
  反过来（PNG 失败却返回 0）就是"失败报成功"，这工具修过四次那个病。
- **以 `-` 开头的位置参数要写在 `--` 之后**，例如 `send -- -x`。特殊字符在
  tmux 那层无需转义，但 Go 的 `flag` 会把裸的 `-x` 当选项。注意
  `key -- -R`：`-R` 会被当键名**发给 tmux**，而 tmux 的 `-R` 是"重画屏幕"，
  不加 `--` 则它被当选项吃掉、静默清屏还返回 0。
- **会话名一律带 `tuidbg-` 前缀**，`-session` 传裸名即可。所有 tmux 目标
  都用 `=` 强制精确匹配，不会误伤别的工具的会话。
- **卡住时可以直接接管**：`tmux attach -t tuidbg-<名字>`，`C-b d` 脱离。

## 自检

    go test ./cmd/tuidbg

取代了 Python 版的 `--selftest`（`shot_test.go` 的用例编号与它一一对应）。
拿 `bash`/`cat` 当被测对象跑通全链路，不依赖 `yanshi` 二进制，改 Go 代码
不会让它红。

## 依赖

tmux（macOS: `brew install tmux`）。Unix only。其余全是 Go 标准库加
`golang.org/x/image`（已在 `go.mod` 里）—— **不需要 Python，也不需要浏览器**。
`-png` 需要系统上有等宽字体，找不到时它显式报错而不是给张空白图。
