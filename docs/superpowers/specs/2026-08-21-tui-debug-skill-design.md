# tui-debug skill 设计

日期：2026-08-21
状态：待实现

## 要解决的问题

`yanshi` 的 Bubble Tea TUI 跑在 alt-screen 里，**无法通过管道驱动**。CLAUDE.md 目前给出的全部启动自检手段只有两条：`./yanshi -h`（打印用法就退出，根本没进 TUI）和 `timeout 5 ./yanshi --fake-model -inprocess`（盲跑，看不见任何画面）。

后果是 TUI 层的改动只能靠 `internal/cli/tui` 的单测验证 —— 而单测验证的是 `Model.Update`/`View` 的返回值，不是真实终端里那块像素。启动崩溃、渲染错位、按键绑定失效、宽字符截断这几类问题，单测全绿的同时可以稳稳复现在真机上。

**目标**：让调试者（人或 agent）能起一个真实 TUI、喂按键、把屏幕当纯文本读回来。

## 方案：拿 tmux 当终端模拟器

Python 只用 stdlib `subprocess` 包一层 tmux 命令：

| 动作 | tmux 底层 |
|---|---|
| 起会话 | `new-session -d -x COLS -y ROWS` |
| 打字 | `send-keys -l <文本>`（literal，不走键名解析） |
| 按键 | `send-keys <键名...>`（`Enter` / `Escape` / `C-c` / `Up` / `Tab` / `BSpace`） |
| 抓屏 | `capture-pane -p`（加 `-e` 保留 ANSI 颜色） |
| 探活 | `display-message -p '#{pane_dead}|#{pane_dead_status}|#{alternate_on}'` |

VT100 状态机、alt-screen 切换、光标定位重绘全部是 tmux 的职责，本项目一行都不实现。

### 已实测验证的假设

写这份 spec 之前在本机（tmux 3.7c / macOS）跑过真实探针，不是推断：

1. **alt-screen 可抓** —— 对真实 `./yanshi --fake-model -inprocess` 会话 `capture-pane -p` 拿到了完整的 banner + 输入框 + 状态栏，同时 `#{alternate_on}` 报 `1`。这是整个方案的决定性前提，它成立。
2. **输入回路可用** —— `send-keys -l 'hello from tuidbg'` 后屏幕上的输入框出现该文本；再发 `Enter`，屏幕转为 `you:` / `assistant:` 的 transcript 渲染。
3. **`remain-on-exit` 不可用** —— 进程退出后 tmux 画的 `Pane is dead (status N, ...)` 那一行会挤掉屏幕内容（探针里首行 `HELLO` 因此丢失）。因此本设计**不用** `remain-on-exit`，改用 wrapper（见下）。

### 为什么不是 pty + pyte

`pty` + `pyte` 需要新增一个 Python 包，且要自己实现 select 循环、时序等待、resize。它原本的卖点是跨平台，**但 `pty` 模块本身就是 Unix-only**，与 tmux 方案在 Windows 上一样跑不了，卖点不成立。

tmux 另外白捡两件事：会话在进程之外持久（每次 shell 调用都是新进程，状态却留在 tmux 里，天然适合 agent 的探索式调试）；出问题时人可以 `tmux attach -t <会话>` 直接接管现场。

## 交付物

`.claude/skills/tui-debug/` 两个文件：

- `SKILL.md` —— 何时用、怎么用、坑
- `tuidbg.py` —— 单文件，stdlib only

## 接口契约

```
tuidbg.py start [--session NAME] [--cols 100] [--rows 30] -- <命令...>
tuidbg.py send  [--session NAME] <文本>
tuidbg.py key   [--session NAME] <键名...>
tuidbg.py shot  [--session NAME] [--wait 正则] [--timeout 秒] [--ansi]
tuidbg.py stop  [--session NAME]
tuidbg.py --selftest
```

会话默认名 `tuidbg`。**`--session` 接受裸名字（如 `foo`），内部统一加 `tuidbg-` 前缀得到真实 tmux 会话名（`tuidbg-foo`）。** 前缀在用户侧完全透明，因此 `stop` 的"只接受带前缀的名字"这条检查是冗余的 —— 前缀由实现保证，不是给用户声明的约束。

### 承重行为

**`shot --wait` 是唯一非平凡的逻辑。** 轮询 `capture-pane` 直到正则命中或超时。超时时**打印最后一屏并以非零码退出** —— 不静默失败，否则调用方会对着空屏猜。默认 timeout 10 秒，轮询间隔 100ms。

**被测命令的 wrapper 与 `shot` 的配合契约**：

1. wrapper 在被测命令退出后打印一行标记 `__TUIDBG_EXIT__=<退出码>`，然后 `sleep infinity` 挂住不让 pane 死。
2. `shot` 抓屏时**总是先检查标记是否已出现**（单次 capture + grep，不轮询）：
   - 若已退出，立即打印最后一屏 + 解析出的退出码到 stderr，**并以该退出码退出**（不是 0，也不是超时的 124）
   - 若未退出，进入 `--wait` 轮询逻辑：命中即返回屏幕内容、超时则打印最后一屏 + 以 124 退出
3. `--wait` 轮询期间每轮都重新检查标记 —— 进程中途退出时**立即短路**，不再空等到 timeout。

这样做的结果：`shot` 不加 `--wait` 时也能报告退出（超时码与真实退出码不同，调用方能区分）；`--wait` 不会对着已死进程傻等 10 秒；退出码通过屏幕文本 + 工具退出码两个通道都可见。

（注意物理限制：TUI 正常退出时终端会切回主屏幕，alt-screen 上的内容按终端语义本就消失，任何方案都救不回 —— 能救的是崩溃时来不及切屏的那一类，以及退出码本身。）

**`stop` 杀会话时的安全边界。** 本机已存在其它工具的 tmux 会话（如 `agbt-*`）。`stop` 对推导出的会话名（`tuidbg-<用户传的名字>`）执行 `kill-session -t`，tmux 自己保证精确匹配 —— 不提供任何"清理全部会话"的操作，只杀一个。

**`start` 遇到同名已存在会话时报错退出**，提示先 `stop`。不静默复用、不静默杀掉重建 —— 两者都会让调用方以为自己在看新起的进程，实际在看旧的。

**前置检查。** `tuidbg.py` 在任何子命令执行前确认 `tmux` 在 PATH 上，缺失时给出明确错误而不是让 `FileNotFoundError` 冒出来。

## 测试

`tuidbg.py --selftest`：拿 `bash -c 'echo READY; read x; echo GOT:$x'` 当被测对象，assert 断言跑通 start → shot --wait READY → send → shot --wait GOT: → stop 全链路，再断言一次超时路径确实非零退出。**不依赖 `yanshi` 二进制**，因此改 Go 代码不会让它红。

无框架、无 fixture。这是本工具全部的自动化验证。

## 明确不做

- **不发明场景 DSL**（yaml/脚本格式）—— 子命令加调用方自己的 shell 脚本已经是脚本
- **不做截图 diff / golden 比对** —— `internal/cli/tui/update_golden_test.go` 已在 Go 侧做这件事
- **不做 Windows 回退** —— 调试工具，且被否决的 pty 方案同样不支持
- **不进 CI** —— 这是给人和 agent 交互式用的工具，不是门禁

## 验收

1. `tuidbg.py --selftest` 通过
2. 起真实 `./yanshi --fake-model -inprocess`，`shot --wait` 能等到 banner 并抓回完整首屏
3. `send` + `key Enter` 后能抓到 transcript 里的回应文本
4. 被测命令非零退出时，`shot` 仍抓得到最后一屏并报告退出码
5. `stop` 后 `tmux ls` 里该会话消失，且其它前缀的会话不受影响
