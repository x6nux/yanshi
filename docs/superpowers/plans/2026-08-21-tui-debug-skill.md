# tui-debug skill 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 交付一个 Claude Code skill，用 tmux 驱动 yanshi 的 alt-screen Bubble Tea TUI，让调试者能起会话、喂按键、把屏幕当纯文本读回来。

**Architecture:** 单文件 Python（stdlib only）包一层 tmux 子命令。VT100 解析、alt-screen、光标重绘全部由 tmux 负责，本工具只做进程编排与等待逻辑。会话在 Python 进程之外持久，因此每次调用都是独立的短命进程。

**Tech Stack:** Python 3.13 stdlib（`subprocess` / `argparse` / `re` / `time` / `shutil` / `sys`）、tmux 3.7c。无第三方依赖、无框架。

**Spec:** `docs/superpowers/specs/2026-08-21-tui-debug-skill-design.md`

---

## 实测确定的实现约束

以下全部在本机（macOS / tmux 3.7c）跑过真实探针，不是推断。实现时**必须**遵守，偏离会引入静默错误：

| 约束 | 实测证据 | 后果 |
|---|---|---|
| **所有 `-t` 目标必须带 `=` 前缀** | `capture-pane -pt 'tuidbg:0.0'` 抓到了 `tuidbg-lit` 的屏幕内容 | tmux 的 `-t` 默认是**前缀匹配**。不加 `=` 会让 `stop tuidbg` 误杀 `tuidbg-x`、`shot tuidbg` 抓错会话 |
| **pane 目标写 `=会话名:0.0`** | `-t '=tuidbg-lit'` 报 `can't find pane`；`-t '=tuidbg-lit:0.0'` 正常 | `send-keys`/`capture-pane` 要 pane 目标，只给会话名不够 |
| **会话目标写 `=会话名`** | `has-session -t '=tuidbg'` 对 `tuidbg-x` 返回 rc=1（正确拒绝） | `has-session`/`kill-session` 要会话目标，不带 `:0.0` |
| **不用 `remain-on-exit`** | pane 死后 tmux 画的 `Pane is dead (status N, ...)` 行挤掉了首行 `HELLO` | 内容丢失。改用 wrapper + `exec sleep` 挂住 |
| **wrapper 用 `exec sleep 100000` 挂住** | `bash cmd; printf '__TUIDBG_EXIT__=%s\n' $?; exec sleep 100000` → 屏幕留住 `START` + `__TUIDBG_EXIT__=42`，`pane_dead=0` | pane 不死，屏幕内容与退出码同时可读 |
| **`send-keys -l --` 对特殊字符安全** | 发 `--wait; $(x) \`y\` "z"` 原样出现在屏幕上 | 无需自己转义。`--` 终止选项解析，防止文本以 `-` 开头被当 flag |
| **`new-session -c DIR` 生效，且不能省** | 不带 `-c` 时 pane 落在 tmux **server** 的默认目录（从 `/tmp` 起的 server 给出 `/tmp`），与调用者 cwd 无关 | 相对路径的被测命令会 command not found |
| **tmux 在无 TTY 下可用** | agent 的非交互 Bash 里 `tmux -V` 正常 | 这是整个方案对 agent 可用的前提 |
| **`--wait` 锚点必须"locale 无关 **且** 只在 TUI 接管后才出现"** | 本机 `LC_ALL=zh_CN.UTF-8`，输入框渲染为 `输入消息…（Enter 发送，Ctrl+Enter 换行）`；英文 catalog 是 `Type a message`。而 `yanshi` 虽两种 locale 都在，却也出现在 TUI 接管**之前**的启动 stderr 里（`yanshi: logs -> …`）—— 实测 0.1–0.3s 窗口内它已命中 2 次而屏幕只有 5 行 | 选错锚点有两种败法：语言选错 → 稳定超时 124（响亮）；选到启动早期就存在的串 → **rc=0 + 残屏**（静默，更糟，正是 spec 禁止的"对着空屏猜"）。`Ctrl\+Enter` 在输入框提示语内、两种 locale 都有、只在 TUI 画完后出现，实测稳定拿到完整 20–24 行 |

**与 spec §测试 的一处有意差异**：spec 描述自检用 `bash -c 'echo READY; read x; echo GOT:$x'` 走一次 `shot --wait GOT:` 往返；本计划改用 `cat` 并加了特殊字符断言。后者严格更强（额外覆盖 shell 元字符原样送达这条路径），故意为之。

---

## File Structure

| 文件 | 职责 |
|---|---|
| `.claude/skills/tui-debug/tuidbg.py` | 全部实现。argparse 分发 + 5 个子命令 + `--selftest`。单文件，约 200 行 |
| `.claude/skills/tui-debug/SKILL.md` | 何时用、怎么用、坑。frontmatter 带 `name` + `description` |

不拆多文件：总量约 200 行，拆开只会让读者跨文件追一条 `subprocess` 调用链。

---

## Task 1: 骨架 + tmux 前置检查 + 会话名推导

**Files:**
- Create: `.claude/skills/tui-debug/tuidbg.py`

- [ ] **Step 1: 写失败的自检片段**

先只写会话名推导与 tmux 检查这两个纯函数，配一个能跑的断言。在文件末尾写：

```python
import sys  # 临时 main 要用；Step 3 的完整 import 块会覆盖它


def _selftest_names():
    assert session_target("foo") == "=tuidbg-foo"
    assert pane_target("foo") == "=tuidbg-foo:0.0"
    assert session_target("tuidbg") == "=tuidbg-tuidbg"  # 裸名一律加前缀，不做例外
    print("names OK")


if __name__ == "__main__":
    # 临时入口，Task 5 装上 argparse 后删掉。
    # 每加一个 _selftest_* 就往这里加一行 —— 否则新写的自检不会被调用，
    # 下一个 Task 的"确认失败"步骤会假绿。
    if "--selftest" in sys.argv:
        _selftest_names()
    else:
        sys.exit("尚未实现")
```

- [ ] **Step 2: 运行确认失败**

Run: `python3 .claude/skills/tui-debug/tuidbg.py --selftest`
Expected: FAIL —— `NameError: name 'session_target' is not defined`（此时函数还没写）

- [ ] **Step 3: 写最小实现**

```python
#!/usr/bin/env python3
"""tuidbg —— 用 tmux 驱动 alt-screen TUI 的调试工具。

为什么是 tmux 而不是 pty+pyte：VT100 状态机、alt-screen 切换、光标重绘
全部由 tmux 负责，本文件一行都不实现。另外 tmux 会话活在本进程之外，
所以每次调用都可以是独立短命进程，天然适合 agent 的探索式调试。
"""
import argparse
import re
import shutil
import subprocess
import sys
import time

PREFIX = "tuidbg-"
EXIT_MARKER = "__TUIDBG_EXIT__"
TIMEOUT_RC = 124  # 与 coreutils timeout(1) 一致，好让调用方区分"超时"与"被测程序的退出码"


def session_target(name):
    """会话目标。`=` 前缀强制精确匹配 —— tmux 的 -t 默认是前缀匹配，
    不加 `=` 时 `stop tuidbg` 会误杀 `tuidbg-x`（实测）。"""
    return "=" + PREFIX + name


def pane_target(name):
    """pane 目标。send-keys/capture-pane 要的是 pane 而非会话，
    只给会话名会报 can't find pane（实测）。"""
    return "=" + PREFIX + name + ":0.0"


def need_tmux():
    if shutil.which("tmux") is None:
        sys.exit("tuidbg: 需要 tmux，但它不在 PATH 上。macOS: brew install tmux")


def tmux(*args, check=False):
    """跑一条 tmux 命令，返回 CompletedProcess。文本模式、吞掉 stderr 由调用方决定。"""
    return subprocess.run(
        ["tmux", *args], capture_output=True, text=True, check=check
    )
```

- [ ] **Step 4: 运行确认通过**

Run: `python3 .claude/skills/tui-debug/tuidbg.py --selftest`

Expected: 打印 `names OK`，退出码 0

- [ ] **Step 5: Commit**

```bash
git add .claude/skills/tui-debug/tuidbg.py
git commit -m "feat(tui-debug): tmux target helpers with exact-match prefix"
```

---

## Task 2: start / stop

**Files:**
- Modify: `.claude/skills/tui-debug/tuidbg.py`

- [ ] **Step 1: 写失败的自检**

扩充自检，跑真实 tmux 会话：

```python
def _selftest_lifecycle():
    name = "selftest"
    tmux("kill-session", "-t", session_target(name))  # 清理上一轮残留
    rc = cmd_start(name, 60, 10, ["bash", "-c", "echo READY; sleep 100"], None)
    assert rc == 0, f"start 应成功，得到 {rc}"
    assert session_exists(name), "start 后会话应存在"

    # start 撞名必须报错，不能静默复用/重建
    rc2 = cmd_start(name, 60, 10, ["bash", "-c", "sleep 100"], None)
    assert rc2 != 0, "撞名的 start 应失败"

    cmd_stop(name)
    assert not session_exists(name), "stop 后会话应消失"
    print("lifecycle OK")
```

- [ ] **Step 2: 运行确认失败**

先往 Task 1 那个临时 main 里加一行 `_selftest_lifecycle()`（跟在 `_selftest_names()` 后面）—— 不加的话新自检根本不会被调用，这一步会假绿。

Run: `python3 .claude/skills/tui-debug/tuidbg.py --selftest`
Expected: FAIL —— `NameError: name 'cmd_start' is not defined`

- [ ] **Step 3: 写实现**

```python
def session_exists(name):
    return tmux("has-session", "-t", session_target(name)).returncode == 0


def cmd_start(name, cols, rows, argv, cwd):
    """起一个 detached 会话跑 argv。

    被测命令包在 wrapper 里：退出后打印带退出码的标记，再 exec sleep 挂住
    不让 pane 死。不用 tmux 的 remain-on-exit —— 实测它画的
    "Pane is dead (status N, ...)" 那行会挤掉屏幕首行内容。
    """
    if session_exists(name):
        print(
            f"tuidbg: 会话 {PREFIX}{name} 已存在。先 stop，或换 --session 名字。\n"
            f"        （不静默复用也不静默重建 —— 那会让你以为在看新进程，实际在看旧的）",
            file=sys.stderr,
        )
        return 1

    inner = " ".join(shlex.quote(a) for a in argv)
    wrapped = f"{inner}; printf '{EXIT_MARKER}=%s\\n' $?; exec sleep 100000"

    args = ["new-session", "-d", "-s", PREFIX + name, "-x", str(cols), "-y", str(rows)]
    if cwd:
        args += ["-c", cwd]  # 用 tmux 的 -c，不要在命令串里 cd
    args.append(wrapped)

    r = tmux(*args)
    if r.returncode != 0:
        print(f"tuidbg: 起会话失败: {r.stderr.strip()}", file=sys.stderr)
        return r.returncode
    return 0


def cmd_stop(name):
    """只杀这一个会话。session_target 的 `=` 保证不会波及 tuidbg-x 之类的邻居。"""
    r = tmux("kill-session", "-t", session_target(name))
    if r.returncode != 0:
        print(f"tuidbg: 会话 {PREFIX}{name} 不存在", file=sys.stderr)
        return 1
    return 0
```

顶部 import 加 `shlex`。

- [ ] **Step 4: 运行确认通过**

Run: `python3 .claude/skills/tui-debug/tuidbg.py --selftest`
Expected: 打印 `names OK` 与 `lifecycle OK`，退出码 0

- [ ] **Step 5: 手工验证不误杀邻居（这是 `=` 前缀存在的全部理由）**

```bash
tmux new-session -d -s tuidbg-neighbor 'sleep 100'
python3 .claude/skills/tui-debug/tuidbg.py start --session n -- bash -c 'sleep 100'
python3 .claude/skills/tui-debug/tuidbg.py stop --session n
tmux has-session -t '=tuidbg-neighbor' && echo "邻居幸存 ✓"
tmux kill-session -t '=tuidbg-neighbor'
```

Expected: 打印 `邻居幸存 ✓`

- [ ] **Step 6: Commit**

```bash
git add .claude/skills/tui-debug/tuidbg.py
git commit -m "feat(tui-debug): start/stop with exit-code wrapper"
```

---

## Task 3: send / key

**Files:**
- Modify: `.claude/skills/tui-debug/tuidbg.py`

- [ ] **Step 1: 写失败的自检**

```python
def _selftest_input():
    name = "selftest"
    tmux("kill-session", "-t", session_target(name))
    cmd_start(name, 60, 10, ["cat"], None)
    time.sleep(0.4)

    # 特殊字符必须原样到达 —— send-keys -l 不做 shell 解释
    tricky = '--wait; $(x) `y` "z"'
    cmd_send(name, tricky)
    cmd_key(name, ["Enter"])
    time.sleep(0.4)

    screen = capture(name)
    assert tricky in screen, f"特殊字符应原样出现，屏幕是:\n{screen}"

    cmd_stop(name)
    print("input OK")
```

- [ ] **Step 2: 运行确认失败**

同样先往临时 main 里加一行 `_selftest_input()`。

Run: `python3 .claude/skills/tui-debug/tuidbg.py --selftest`
Expected: FAIL —— `NameError: name 'cmd_send' is not defined`

- [ ] **Step 3: 写实现**

```python
def cmd_send(name, text):
    """打字，不回车。-l 是 literal（不解析键名），-- 终止选项解析
    以免以 `-` 开头的文本被当成 flag。实测特殊字符无需自己转义。"""
    r = tmux("send-keys", "-t", pane_target(name), "-l", "--", text)
    if r.returncode != 0:
        print(f"tuidbg: send 失败: {r.stderr.strip()}", file=sys.stderr)
        return 1
    return 0


def cmd_key(name, keys):
    """发按键。键名用 tmux 的词表：Enter / Escape / Tab / Up / Down /
    BSpace / C-c / C-u 等。"""
    r = tmux("send-keys", "-t", pane_target(name), *keys)
    if r.returncode != 0:
        print(f"tuidbg: key 失败: {r.stderr.strip()}", file=sys.stderr)
        return 1
    return 0


def capture(name, ansi=False):
    """抓当前屏幕。-p 打到 stdout，-e 保留 ANSI 颜色序列。"""
    args = ["capture-pane", "-p", "-t", pane_target(name)]
    if ansi:
        args.insert(2, "-e")
    r = tmux(*args)
    if r.returncode != 0:
        return None
    return r.stdout
```

- [ ] **Step 4: 运行确认通过**

Run: `python3 .claude/skills/tui-debug/tuidbg.py --selftest`
Expected: 三行 OK 全部打印，退出码 0

- [ ] **Step 5: Commit**

```bash
git add .claude/skills/tui-debug/tuidbg.py
git commit -m "feat(tui-debug): send/key input primitives"
```

---

## Task 4: shot —— 唯一非平凡的逻辑

**Files:**
- Modify: `.claude/skills/tui-debug/tuidbg.py`

这一步实现 spec 里的「wrapper 与 shot 的配合契约」。三条机制缺一不可：抓屏前先查退出标记、已退出则以真实退出码退出、`--wait` 轮询期间每轮重查标记以便中途死亡时短路。

- [ ] **Step 1: 写失败的自检**

```python
def _selftest_shot():
    name = "selftest"

    # (a) --wait 命中
    tmux("kill-session", "-t", session_target(name))
    cmd_start(name, 60, 10, ["bash", "-c", "sleep 0.5; echo READY; sleep 100"], None)
    rc = cmd_shot(name, wait=r"READY", timeout=5, ansi=False)
    assert rc == 0, f"--wait 命中应返回 0，得到 {rc}"
    cmd_stop(name)

    # (b) --wait 超时 → 124，且必须打印最后一屏而不是静默
    tmux("kill-session", "-t", session_target(name))
    cmd_start(name, 60, 10, ["bash", "-c", "echo NOPE; sleep 100"], None)
    rc = cmd_shot(name, wait=r"NEVER_APPEARS", timeout=1, ansi=False)
    assert rc == TIMEOUT_RC, f"超时应返回 {TIMEOUT_RC}，得到 {rc}"
    cmd_stop(name)

    # (c) 被测程序退出 → 以它的真实退出码退出（不是 0，也不是 124）
    tmux("kill-session", "-t", session_target(name))
    cmd_start(name, 60, 10, ["bash", "-c", "echo BYE; exit 42"], None)
    time.sleep(0.8)
    rc = cmd_shot(name, wait=None, timeout=5, ansi=False)
    assert rc == 42, f"应报告被测程序的退出码 42，得到 {rc}"
    cmd_stop(name)

    # (d) --wait 遇到已死进程要立刻短路，不能空等满 timeout
    tmux("kill-session", "-t", session_target(name))
    cmd_start(name, 60, 10, ["bash", "-c", "exit 7"], None)
    time.sleep(0.5)
    t0 = time.monotonic()
    rc = cmd_shot(name, wait=r"NEVER_APPEARS", timeout=10, ansi=False)
    elapsed = time.monotonic() - t0
    assert rc == 7, f"死进程应报告退出码 7，得到 {rc}"
    assert elapsed < 3, f"应立即短路，实际等了 {elapsed:.1f}s"
    cmd_stop(name)

    print("shot OK")
```

- [ ] **Step 2: 运行确认失败**

同样先往临时 main 里加一行 `_selftest_shot()`。

Run: `python3 .claude/skills/tui-debug/tuidbg.py --selftest`
Expected: FAIL —— `NameError: name 'cmd_shot' is not defined`

- [ ] **Step 3: 写实现**

```python
def dead_exit_code(screen):
    """屏幕里若已出现 wrapper 的退出标记，返回退出码；否则 None。

    取最后一个匹配：TUI 的 transcript 里可能碰巧出现同样的字面量
    （比如用户就在调试这个工具本身），最后一个才是 wrapper 打的。
    """
    hits = re.findall(rf"{re.escape(EXIT_MARKER)}=(\d+)", screen)
    return int(hits[-1]) if hits else None


def cmd_shot(name, wait, timeout, ansi):
    """抓屏。返回值即本命令的退出码。

    优先级：被测程序已退出 > --wait 命中 > 超时。
    "已退出"排第一，是为了让 --wait 不对着一具尸体空等满 timeout ——
    死掉的进程永远不会再让正则命中，那 10 秒纯属浪费，而且超时码会
    把死因说成"没等到"，掩盖真正的原因。
    """
    if not session_exists(name):
        print(f"tuidbg: 会话 {PREFIX}{name} 不存在。先 start。", file=sys.stderr)
        return 1

    deadline = time.monotonic() + timeout
    pattern = re.compile(wait) if wait else None
    screen = ""

    while True:
        screen = capture(name, ansi) or ""

        code = dead_exit_code(screen)
        if code is not None:
            sys.stdout.write(screen)
            print(
                f"\ntuidbg: 被测进程已退出，退出码 {code}"
                + ("（--wait 未命中即已退出）" if pattern else ""),
                file=sys.stderr,
            )
            return code

        if pattern is None or pattern.search(screen):
            sys.stdout.write(screen)
            return 0

        if time.monotonic() >= deadline:
            sys.stdout.write(screen)
            print(
                f"\ntuidbg: 等 {timeout}s 没等到 /{wait}/。上面是最后一屏。",
                file=sys.stderr,
            )
            return TIMEOUT_RC

        time.sleep(0.1)
```

- [ ] **Step 4: 运行确认通过**

Run: `python3 .claude/skills/tui-debug/tuidbg.py --selftest`
Expected: 四行 OK 全部打印，退出码 0。(d) 那条应在 1 秒内完成而非 10 秒

- [ ] **Step 5: Commit**

```bash
git add .claude/skills/tui-debug/tuidbg.py
git commit -m "feat(tui-debug): shot with wait/timeout/exit-code contract"
```

---

## Task 5: CLI 装配

**Files:**
- Modify: `.claude/skills/tui-debug/tuidbg.py`

- [ ] **Step 1: 写 argparse 分发**

```python
def build_parser():
    p = argparse.ArgumentParser(
        prog="tuidbg", description="用 tmux 驱动 alt-screen TUI 的调试工具"
    )
    p.add_argument("--selftest", action="store_true", help="跑自检（不需要 yanshi 二进制）")
    sub = p.add_subparsers(dest="cmd")

    def with_session(sp):
        sp.add_argument("--session", default="tuidbg", help="会话名（内部加 tuidbg- 前缀）")
        return sp

    s = with_session(sub.add_parser("start", help="起一个会话跑命令"))
    s.add_argument("--cols", type=int, default=100)
    s.add_argument("--rows", type=int, default=30)
    s.add_argument("--cwd", default=None)
    s.add_argument("argv", nargs=argparse.REMAINDER, help="-- 之后是被测命令")

    s = with_session(sub.add_parser("send", help="打字（不回车）"))
    s.add_argument("text")

    s = with_session(sub.add_parser("key", help="发按键：Enter/Escape/C-c/Up/Tab..."))
    s.add_argument("keys", nargs="+")

    s = with_session(sub.add_parser("shot", help="抓屏"))
    s.add_argument("--wait", default=None, help="轮询直到该正则命中")
    s.add_argument("--timeout", type=float, default=10.0)
    s.add_argument("--ansi", action="store_true", help="保留 ANSI 颜色序列")

    with_session(sub.add_parser("stop", help="杀掉会话"))
    return p


def main():
    args = build_parser().parse_args()
    need_tmux()

    if args.selftest:
        _selftest_names()
        _selftest_lifecycle()
        _selftest_input()
        _selftest_shot()
        print("\n全部自检通过 ✓")
        return 0

    if args.cmd is None:
        build_parser().print_help()
        return 2

    if args.cmd == "start":
        argv = args.argv[1:] if args.argv and args.argv[0] == "--" else args.argv
        if not argv:
            sys.exit("tuidbg: start 需要命令，写在 -- 之后")
        return cmd_start(args.session, args.cols, args.rows, argv, args.cwd)
    if args.cmd == "send":
        return cmd_send(args.session, args.text)
    if args.cmd == "key":
        return cmd_key(args.session, args.keys)
    if args.cmd == "shot":
        return cmd_shot(args.session, args.wait, args.timeout, args.ansi)
    if args.cmd == "stop":
        return cmd_stop(args.session)
    return 2


if __name__ == "__main__":
    sys.exit(main())
```

删掉 Task 1 里那个临时的 `if __name__` 块。

- [ ] **Step 2: 运行完整自检**

Run: `python3 .claude/skills/tui-debug/tuidbg.py --selftest`
Expected: 五行 OK + `全部自检通过 ✓`，退出码 0

- [ ] **Step 3: 对真实 TUI 端到端验证（spec 验收 2 与 3）**

```bash
cd /Users/ll/code/yanshi
T=".claude/skills/tui-debug/tuidbg.py"
python3 $T start --cols 100 --rows 30 --cwd /Users/ll/code/yanshi -- ./yanshi --fake-model -inprocess
python3 $T shot --wait 'Ctrl\+Enter' --timeout 20 | tail -5
python3 $T send 'hello from tuidbg'
python3 $T key Enter
python3 $T shot --wait 'assistant:' --timeout 30 | tail -12
python3 $T stop
```

Expected: 第一个 shot 的 `tail -5` 里能看到**输入框那一行 + 状态栏那一行**（约 20–24 行完整首屏），不是一两行残屏；第二个抓到 `you:` / `assistant:` 的 transcript

**只看退出码不够** —— 下面第二条坑的失败形态正是 rc=0 配残屏，退出码看不见它。所以这一步必须真读 `tail -5` 的内容。

两处坑，都是实测踩出来的：

- **`--cwd` 不能省。** `new-session` 不带 `-c` 时用的是 **tmux server 的**默认目录，不是你 shell 的 cwd —— server 可能是几天前在别的目录起的。相对路径 `./yanshi` 会 command not found（实测报 `load config: open config.yaml: no such file or directory`）。
- **`--wait` 的锚点要挑得准，两个条件缺一不可。** ①locale 无关：输入框提示语英文是 `Type a message`、中文是 `输入消息…`，选错语言 → 稳定超时 124。②只在 TUI 接管后出现：`yanshi` 满足①却不满足② —— 它也在启动 stderr（`yanshi: logs -> …`）里，实测启动后 0.1–0.3s 窗口内已命中而屏幕只有 5 行，`shot` 会 **rc=0 返回残屏**，静默假绿。`Ctrl\+Enter` 两条都满足（反斜杠是因为 `--wait` 收正则）。

- [ ] **Step 4: Commit**

```bash
git add .claude/skills/tui-debug/tuidbg.py
git commit -m "feat(tui-debug): CLI dispatch and selftest entry"
```

---

## Task 6: SKILL.md

**Files:**
- Create: `.claude/skills/tui-debug/SKILL.md`

- [ ] **Step 1: 写文档**

frontmatter 的 `description` 决定我什么时候会想起用它，要把触发场景写全：

```markdown
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

    T=.claude/skills/tui-debug/tuidbg.py

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
| `shot [--wait 正则] [--timeout 秒] [--ansi]` | 抓屏到 stdout |
| `stop` | 杀会话 |

### shot 的退出码

| 码 | 含义 |
|---|---|
| 0 | 抓到了（`--wait` 命中，或没给 `--wait`） |
| 124 | 等超时。**最后一屏仍会打印**，照着它看卡在哪 |
| 其它 | 被测程序的真实退出码。它已经退出了 |

`--wait` 遇到已退出的进程会立即短路，不会空等满 timeout。

## 坑

- **`--wait` 的锚点要挑得准，两个条件缺一不可。**
  ①**locale 无关**：界面文案走 i18n，输入框英文是 `Type a message`、中文是
  `输入消息…`，选错语言 → 稳定超时 124（响亮，好排查）。
  ②**只在 TUI 接管屏幕后才出现**：`yanshi` 满足①却不满足② —— 它也出现在
  启动 stderr（`yanshi: logs -> …`）里，实测启动后 0.1–0.3s 内它已命中而
  屏幕只有 5 行，`shot` 于是 **rc=0 返回残屏**，静默假绿，比超时糟得多。
  用 `Ctrl\+Enter`（输入框提示语内，两条都满足；反斜杠是因为 `--wait` 收正则）。
- **`start` 记得带 `--cwd`。** 不带时 pane 用的是 **tmux server 的**默认目录，
  不是你 shell 的 cwd —— server 可能是几天前在别处起的，相对路径会
  command not found。传 `--cwd "$PWD"` 最省心。
- **TUI 正常退出后屏幕是空的。** 终端会切回主屏幕，alt-screen 的内容按语义
  就该消失，任何工具都救不回。能救的是崩溃时来不及切屏的那类，以及退出码。
- **`--wait` 的正则匹配的是渲染后的屏幕**，不是原始输出。TUI 会截断长行、
  插入边框字符，所以匹配短的稳定片段（`assistant:`）比匹配整句可靠。
- **窗口尺寸影响渲染**。默认 100x30。复现布局问题时用 `--cols`/`--rows`
  对齐用户的终端尺寸。
- **`send` 的文本以 `-` 开头时要写 `send -- -x`。** 特殊字符在 tmux 那层
  无需转义，但 argparse 会把裸的 `-x` 当选项。
- **会话名一律带 `tuidbg-` 前缀**，`--session` 传裸名即可。所有 tmux 目标
  都用 `=` 强制精确匹配，不会误伤别的工具的会话。
- **卡住时可以直接接管**：`tmux attach -t tuidbg-<名字>`，`C-b d` 脱离。
- **`stop` / `shot` 对不存在的会话返回 1。**

## 自检

    python3 .claude/skills/tui-debug/tuidbg.py --selftest

拿 `bash`/`cat` 当被测对象跑通全链路，不依赖 `yanshi` 二进制，改 Go 代码
不会让它红。

## 依赖

tmux（macOS: `brew install tmux`）。Python 只用 stdlib。Unix only。
```

- [ ] **Step 2: 确认没触发仓库门禁**

SKILL.md 是 `.claude/` 下的活 `.md`，GOV9 会扫它的 `路径::符号` 引用。上面的文档没写任何符号引用，但还是跑一遍确认：

Run: `go test ./internal/archtest -run 'TestGOV9|TestPhantomSlash' 2>&1 | tail -5`
Expected: PASS（本文档不引用 Go 符号，也不宣传斜杠命令）

- [ ] **Step 3: Commit**

```bash
git add .claude/skills/tui-debug/SKILL.md
git commit -m "docs(tui-debug): skill usage doc"
```

---

## Task 7: 更新 CLAUDE.md 的过时表述

**Files:**
- Modify: `CLAUDE.md`（「运行」那段）

spec 的问题陈述引用了 CLAUDE.md 里「alt-screen TUI 无法通过管道驱动；启动自检可用 `./yanshi -h` 或 `timeout 5 ...`」这句。工具落地后这句就不再是全部实情了。

- [ ] **Step 1: 改那一句**

找到 `**运行。**` 开头那段，把结尾的自检说明改成：

```markdown
alt-screen TUI 无法通过管道驱动；启动自检可用 `./yanshi -h`（打印用法并退出 0）或 `timeout 5 ./yanshi --fake-model -inprocess`。**需要真看见画面时**用 `.claude/skills/tui-debug/`（tmux 起真实 TUI、喂按键、把屏幕抓回纯文本）。
```

- [ ] **Step 2: 跑扫 CLAUDE.md 的那几道门禁**

Run: `go test ./internal/archtest 2>&1 | tail -5`
Expected: PASS。（GOV9、幻影斜杠命令、D2 removal 三道都扫 CLAUDE.md 本身）

- [ ] **Step 3: Commit**

```bash
git add CLAUDE.md
git commit -m "docs: point TUI self-check at the tui-debug skill"
```

---

## 最终验收

对齐 spec 的五条验收标准：

- [ ] `python3 .claude/skills/tui-debug/tuidbg.py --selftest` 通过（标准 1）
- [ ] 起真实 `./yanshi --fake-model -inprocess`，`shot --wait` 等到 banner 并抓回完整首屏（标准 2）—— **要真读屏幕内容，不能只看退出码**：本计划评审期间正是靠"rc=0 但只有 1 行"抓出了一个坏锚点，退出码对那种失败完全无感
- [ ] `send` + `key Enter` 后抓到 transcript 里的回应（标准 3）
- [ ] 被测命令非零退出时 `shot` 抓得到最后一屏并以真实退出码退出（标准 4，自检 (c) 覆盖）
- [ ] `stop` 后 `tmux ls` 里该会话消失，其它前缀会话不受影响（标准 5，Task 2 Step 5 覆盖）
- [ ] `go test ./internal/archtest` 全绿

## 明确不做（沿用 spec）

- 不发明场景 DSL —— 子命令 + 调用方的 shell 脚本已经是脚本
- 不做截图 diff / golden 比对 —— `internal/cli/tui` 的 golden 测试已在 Go 侧做
- 不做 Windows 回退 —— 调试工具，且被否决的 pty 方案同样不支持
- 不进 CI —— 交互式调试工具，不是门禁
