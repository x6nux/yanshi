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

    T=skills/tui-debug/tuidbg.py

    python3 $T start --cwd "$PWD" -- ./yanshi --fake-model -inprocess
    python3 $T shot --wait 'Ctrl\+Enter' --timeout 20
    python3 $T send '帮我看看这个文件'
    python3 $T key Enter
    python3 $T shot --wait 'assistant:' --timeout 30
    python3 $T stop

多会话并行加 `--session 名字`（每个子命令都要带）。

### 子命令

| 命令 | 作用 |
|---|---|
| `start [--cols N] [--rows N] [--cwd DIR] -- 命令...` | 起 detached 会话 |
| `send 文本` | 打字，不回车。特殊字符无需转义 |
| `key 键名...` | 发按键。tmux 词表：`Enter` `Escape` `Tab` `Up` `Down` `BSpace` `C-c` `C-u` |
| `shot [--wait 正则] [--timeout 秒] [--ansi] [--png 路径]` | 抓屏到 stdout；`--png` 另存一张 PNG |
| `stop` | 杀会话 |

### shot 的退出码

| 码 | 含义 |
|---|---|
| 0 | `--wait` 命中；或没给 `--wait`；**或被测程序以 0 退出**（此时 `--wait` 可能从未命中 —— 看 stderr） |
| 124 | 等超时。**最后一屏仍会打印**，照着它看卡在哪 |
| 3 | `--png` 渲染失败（**会盖掉上面那些码**）。stderr 有原因 |
| 1 | 会话不存在，或抓屏失败 |
| 其它 | 被测程序的真实退出码。它已经退出了 |

`--wait` 遇到已退出的进程会立即短路，不会空等满 timeout。

### `--png`：把这一屏渲染成图片

    python3 $T shot --wait 'Ctrl\+Enter' --png /tmp/tui.png

文本抓屏丢掉了颜色、对齐和宽字符布局，PNG 留得住 —— 给人看，或者给
看得了图的 agent 看。实现是 `capture -e` 的 SGR 序列 → 内联样式 HTML →
`agent-browser` 截图，固定深色主题（背景 `#1e1e1e`、前景 `#d4d4d4`）。
ANSI→HTML 那一步是本工具自己做的（`agent-browser` 不认转义序列），
"起浏览器 → 开页面 → 截图"整个交给它。

**不给 `--png` 时行为逐字节不变** —— 与加这个功能之前的版本对拍验证过
（stdout/stderr/退出码三样都一致）。

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
- **`--png` 失败会盖掉正常的退出码，这是有意的。** 渲染失败一律返回 `3`，
  哪怕锚点命中了、或者被测程序本来要报别的码。因此 **`3` 有歧义**：被测程序
  自己以 3 退出时也是这个码。分辨方法看 stderr，两种情形各打一行且互斥：
  成功是 `PNG 已写入 <路径>`，失败是 `PNG 渲染失败：<原因>`。方向是刻意选的 ——
  反过来（PNG 失败却返回 0）就是"失败报成功"，这工具修过四次那个病。
- **`open` 失败之后 `screenshot` 仍然会成功，而且图是"上一个页面"的。** 实测
  （agent-browser 0.34.0）：`open` 一个不存在的 `file://` 返回 1，紧接着的
  `screenshot` 照样返回 0 并写出一张**魔数合格**的 PNG —— 内容是会话里残留的
  上一张页面。所以本工具**查 `open` 的退出码**，不能只看"有没有落盘一张像
  PNG 的文件"。这条是变异测试压住的：把那句检查删掉，渲染失败就会带着一张
  张冠李戴的图报成成功，正是这工具反复修过的那个老病的又一种形态。
- **截图用 `--full`，不是默认视口。** 默认是固定的 1280×577，`--cols 200
  --rows 60` 那种大 pane 会被**静默裁掉**（实测裁成 1280×577，加 `--full`
  则是 1694×1036）。小屏两者产物逐字节相同，所以一律加 `--full`。
  画布尺寸由内容决定，本工具不再自己算窗口大小。
- **`send` 的文本以 `-` 开头时要写 `send -- -x`。** 特殊字符在 tmux 那层
  无需转义，但 argparse 会把裸的 `-x` 当选项。
- **会话名一律带 `tuidbg-` 前缀**，`--session` 传裸名即可。所有 tmux 目标
  都用 `=` 强制精确匹配，不会误伤别的工具的会话。
- **卡住时可以直接接管**：`tmux attach -t tuidbg-<名字>`，`C-b d` 脱离。

## 自检

    python3 skills/tui-debug/tuidbg.py --selftest

拿 `bash`/`cat` 当被测对象跑通全链路，不依赖 `yanshi` 二进制，改 Go 代码
不会让它红。PNG 那部分分两层：SGR→HTML 的转换与失败路径不需要浏览器，
端到端截图需要 —— 没装 `agent-browser` 时它打 `png OK (skipped: no agent-browser)`
而不是判红。

## 依赖

tmux（macOS: `brew install tmux`）。Python 只用 stdlib。Unix only。
`--png` 另需 `agent-browser`（`npm i -g agent-browser`，首次可能还要跑一次
`agent-browser install` 下载浏览器）；不用这个参数就不需要它。
它跑在独占的 `--session tuidbg` 会话里，不会把你正开着的标签页导航走。
