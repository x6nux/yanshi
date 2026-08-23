#!/usr/bin/env python3
"""tuidbg —— 用 tmux 驱动 alt-screen TUI 的调试工具。

为什么是 tmux 而不是 pty+pyte：VT100 状态机、alt-screen 切换、光标重绘
全部由 tmux 负责，本文件一行都不实现。另外 tmux 会话活在本进程之外，
所以每次调用都可以是独立短命进程，天然适合 agent 的探索式调试。
"""
import argparse
import contextlib
import html
import io
import os
import re
import shlex
import shutil
import subprocess
import sys
import tempfile
import time

PREFIX = "tuidbg-"
EXIT_MARKER = "__TUIDBG_EXIT__"
TIMEOUT_RC = 124  # 与 coreutils timeout(1) 一致，好让调用方区分"超时"与"被测程序的退出码"

# --png 渲染失败。**它与"被测程序恰好以 3 退出"撞码，这是有意接受的**：
# 退出码只有一条整数通道，而被测程序的退出码可以是任意值，换成哪个数都能撞。
# 撞了怎么分辨 —— 看 stderr，两种情形各自必然打印一行且互斥：
#   PNG 成功 → "tuidbg: PNG 已写入 <路径>"（此时 rc 是被测程序的真实退出码）
#   PNG 失败 → "tuidbg: PNG 渲染失败：<原因>"
# 方向是刻意选的：**PNG 失败一律盖掉正常退出码**。反过来（保留正常码、
# 只在 stderr 抱怨）就是本工具已经修过四次的那个病 —— 失败被报成成功；
# 而现在最坏的情况只是"成功被报成疑似失败"，响亮且能靠 stderr 澄清。
PNG_FAIL_RC = 3


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
    """跑一条 tmux 命令，返回 CompletedProcess。
    stdout/stderr 都捕获为字符串；check=False 时由调用方检查 returncode。"""
    return subprocess.run(
        ["tmux", *args], capture_output=True, text=True, check=check
    )


def session_exists(name):
    return tmux("has-session", "-t", session_target(name)).returncode == 0


def cmd_start(name, cols, rows, argv, cwd):
    """起一个 detached 会话跑 argv。

    被测命令包在 wrapper 里：退出后打印带退出码的标记，再 exec sleep 挂住
    不让 pane 死。不用 tmux 的 remain-on-exit —— 实测它画的
    "Pane is dead (status N, ...)" 那行会挤掉屏幕首行内容。

    边界：argv 以 `exec` 开头时整个 shell 会被替换掉，标记不会打印、pane 直接死
    （调试 TUI 程序时不会这么写，故不做防护）。
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
        print(f"tuidbg: {r.stderr.strip()}", file=sys.stderr)
        return 1
    return 0


def _selftest_names():
    assert session_target("foo") == "=tuidbg-foo"
    assert pane_target("foo") == "=tuidbg-foo:0.0"
    assert session_target("tuidbg") == "=tuidbg-tuidbg"  # 裸名一律加前缀，不做例外

    # 取最后一个匹配：屏幕上可能先有 transcript 里碰巧出现的同名字面量
    assert dead_exit_code("no marker here") is None
    assert dead_exit_code(f"{EXIT_MARKER}=7") == 7
    assert dead_exit_code(f"用户在调屏幕上打了 {EXIT_MARKER}=1\n{EXIT_MARKER}=42") == 42
    print("names OK")


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
    BSpace / C-c / C-u 等。

    `--` 不能省：send-keys 在 -t 之后仍解析选项，`-R` 这种会被当 flag
    执行（实测静默清屏且 rc=0），而不是当键名发出去。
    """
    r = tmux("send-keys", "-t", pane_target(name), "--", *keys)
    if r.returncode != 0:
        print(f"tuidbg: key 失败: {r.stderr.strip()}", file=sys.stderr)
        return 1
    return 0


def capture(name, ansi=False):
    """抓当前屏幕。-p 打到 stdout，-e 保留 ANSI 颜色序列。

    -J 不能省：不加时 capture-pane 返回物理屏幕，光标靠近右边缘时
    wrapper 的退出标记会被折成两行（实测 `__TUIDBG_E` + `XIT__=3`），
    正则匹配不上 → 崩溃被报成 rc=0 成功。-J 把折行接回逻辑行。
    """
    args = ["capture-pane", "-p", "-J", "-t", pane_target(name)]
    if ansi:
        args.insert(2, "-e")
    r = tmux(*args)
    if r.returncode != 0:
        return None
    return r.stdout


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
    assert screen is not None, "capture 失败（会话没了？）"
    assert tricky in screen, f"特殊字符应原样出现，屏幕是:\n{screen}"

    # 键名词必须当字面量发出去 —— 这条才咬得住 -l：
    # 少了 -l，tmux 会把 "Escape" 解析成转义键，屏幕上是 ^[ 而不是这个词
    cmd_send(name, "Escape")
    cmd_key(name, ["Enter"])
    time.sleep(0.4)

    screen = capture(name)
    assert screen is not None, "capture 失败（会话没了？）"
    assert "Escape" in screen, f"-l 缺失：键名词被解析成了按键，屏幕是:\n{screen}"

    cmd_stop(name)
    print("input OK")


def dead_exit_code(screen):
    """屏幕里若已出现 wrapper 的退出标记，返回退出码；否则 None。

    取最后一个匹配：TUI 的 transcript 里可能碰巧出现同样的字面量
    （比如用户就在调试这个工具本身），最后一个才是 wrapper 打的。
    """
    hits = re.findall(rf"{re.escape(EXIT_MARKER)}=(\d+)", screen)
    return int(hits[-1]) if hits else None


# --- ANSI → HTML ---------------------------------------------------------
# 只处理 SGR（ESC[...m）。实测真实 yanshi TUI 的一屏里 121 个转义序列
# **全部**是 SGR，零个光标移动 —— tmux 已经把屏幕归一化成"文本 + 颜色"了。
# 其余形态（OSC、光标移动等）一律静默丢弃：宁可少一点样式，也不能把裸的
# 转义字节吐进 HTML 变成可见乱码，更不能崩。

_SGR_RE = re.compile(r"\x1b\[[0-9;]*m")
# 非 SGR 的 CSI（末字节不是 m）、OSC（\x1b] … BEL/ST）、以及落单的双字符转义。
_JUNK_ESC_RE = re.compile(r"\x1b\[[0-9;?]*[a-ln-zA-Z]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)?|\x1b[@-Z\\-_]")


def strip_sgr(text):
    """去掉 SGR 序列，还原成纯文本。

    实测保证（用真实 yanshi TUI 的一屏对拍）：`strip_sgr(capture(ansi=True))`
    与 `capture(ansi=False)` **逐字节相同**。这条等式是 --png 敢于总是按
    ansi 抓屏的前提 —— 不然为了拿颜色就得改抓屏方式，而 --wait 的正则和
    退出标记都是在抓屏结果上匹配的，等式不成立就会静默改掉 shot 的语义。
    """
    return _SGR_RE.sub("", text)


BG_DEFAULT = "#1e1e1e"
FG_DEFAULT = "#d4d4d4"

# 0-15 用 xterm 的经典取值。只有这 16 个是"约定俗成"没法算出来的，
# 剩下 240 个全部由下面的公式生成 —— 手抄 256 行表格既臃肿又必错。
_ANSI16 = [
    "#000000", "#cd0000", "#00cd00", "#cdcd00",
    "#0000ee", "#cd00cd", "#00cdcd", "#e5e5e5",
    "#7f7f7f", "#ff0000", "#00ff00", "#ffff00",
    "#5c5cff", "#ff00ff", "#00ffff", "#ffffff",
]


def _palette256():
    """生成 xterm 256 色表：16 基础色 + 6×6×6 色立方 + 24 级灰阶。"""
    pal = list(_ANSI16)
    levels = [0, 95, 135, 175, 215, 255]
    for r in levels:
        for g in levels:
            for b in levels:
                pal.append(f"#{r:02x}{g:02x}{b:02x}")
    for i in range(24):
        v = 8 + 10 * i
        pal.append(f"#{v:02x}{v:02x}{v:02x}")
    return pal


PALETTE = _palette256()


def _blank_state():
    return {"fg": None, "bg": None, "bold": False, "dim": False,
            "italic": False, "underline": False, "reverse": False}


def _apply_sgr(state, params):
    """把一条 SGR 的参数序列作用到样式状态上。

    用下标循环而不是 for-each：38/48 会**吃掉后面的参数**
    （`38;5;N` 或 `38;2;R;G;B`），for-each 会把 N 当成独立的颜色码。
    不认识的参数静默跳过。
    """
    i = 0
    while i < len(params):
        p = params[i]
        if p in (38, 48):
            key = "fg" if p == 38 else "bg"
            if i + 1 < len(params) and params[i + 1] == 5 and i + 2 < len(params):
                idx = params[i + 2]
                if 0 <= idx < len(PALETTE):
                    state[key] = PALETTE[idx]
                i += 3
                continue
            if i + 1 < len(params) and params[i + 1] == 2 and i + 4 < len(params):
                r, g, b = params[i + 2], params[i + 3], params[i + 4]
                if all(0 <= v <= 255 for v in (r, g, b)):
                    state[key] = f"#{r:02x}{g:02x}{b:02x}"
                i += 5
                continue
            i += 1  # 畸形的 38/48，跳过它自己就好
            continue
        if p == 0:
            state.update(_blank_state())
        elif p == 1:
            state["bold"] = True
        elif p == 2:
            state["dim"] = True
        elif p == 3:
            state["italic"] = True
        elif p == 4:
            state["underline"] = True
        elif p == 7:
            state["reverse"] = True
        elif p in (21, 22):
            state["bold"] = state["dim"] = False
        elif p == 23:
            state["italic"] = False
        elif p == 24:
            state["underline"] = False
        elif p == 27:
            state["reverse"] = False
        elif p == 39:
            state["fg"] = None
        elif p == 49:
            state["bg"] = None
        elif 30 <= p <= 37:
            state["fg"] = _ANSI16[p - 30]
        elif 90 <= p <= 97:
            state["fg"] = _ANSI16[p - 90 + 8]
        elif 40 <= p <= 47:
            state["bg"] = _ANSI16[p - 40]
        elif 100 <= p <= 107:
            state["bg"] = _ANSI16[p - 100 + 8]
        # 其余（4;3 之类的扩展下划线、闪烁…）一概忽略
        i += 1


def _css(state):
    """状态 → 内联 style 字符串；全默认时返回空串（省掉无意义的 span）。"""
    fg, bg = state["fg"], state["bg"]
    if state["reverse"]:
        # 反显要在**解析默认值之后**交换，否则 `\x1b[7m` 单独出现时
        # 两边都是 None，交换等于没交换，反显就丢了。
        fg, bg = bg or BG_DEFAULT, fg or FG_DEFAULT
    bits = []
    if fg:
        bits.append(f"color:{fg}")
    if bg:
        bits.append(f"background:{bg}")
    if state["bold"]:
        bits.append("font-weight:bold")
    if state["dim"]:
        bits.append("opacity:.6")
    if state["italic"]:
        bits.append("font-style:italic")
    if state["underline"]:
        bits.append("text-decoration:underline")
    return ";".join(bits)


def ansi_to_html(text):
    """把带 SGR 的屏幕文本转成一张完整的 HTML 页面（内联样式，无外部依赖）。

    固定深色主题：背景 #1e1e1e、默认前景 #d4d4d4 —— 终端里没有"页面背景"
    这个概念，默认色得由渲染方定，浅色主题会让绝大多数 TUI 的配色失真。
    """
    text = _JUNK_ESC_RE.sub("", text)
    state = _blank_state()
    out = []
    pos = 0
    for m in _SGR_RE.finditer(text):
        chunk = text[pos:m.start()]
        if chunk:
            style = _css(state)
            esc = html.escape(chunk, quote=False)
            out.append(f'<span style="{style}">{esc}</span>' if style else esc)
        body = m.group(0)[2:-1]
        # 空参数（ESC[m）按标准等价于 ESC[0m
        params = [int(x) if x else 0 for x in body.split(";")] if body else [0]
        _apply_sgr(state, params)
        pos = m.end()
    tail = text[pos:]
    if tail:
        style = _css(state)
        esc = html.escape(tail, quote=False)
        out.append(f'<span style="{style}">{esc}</span>' if style else esc)

    return (
        "<!DOCTYPE html><html><head><meta charset='utf-8'><style>"
        f"html,body{{margin:0;padding:0;background:{BG_DEFAULT}}}"
        f"pre{{margin:0;padding:{PAD_PX}px;background:{BG_DEFAULT};color:{FG_DEFAULT};"
        f"font:{FONT_PX}px/{LINE_PX}px "
        "Menlo,'DejaVu Sans Mono',Consolas,monospace;"
        # tab-size 对齐终端；white-space:pre 保住行内的连续空格（TUI 全靠它对齐）
        "white-space:pre;tab-size:8}"
        "</style></head><body><pre>" + "".join(out) + "</pre></body></html>"
    )


# 排版常量。喂给 ansi_to_html 的 CSS，全部是在这台机器上量出来的：
#   * 行高由 line-height 钉死 17px：30 行量到 526px = 30×17+2×8，误差 0。
#     不靠字体的自然行距，那个随字体变，换台机器就错位。
# 两侧各留 PAD_PX 的内边距：不留的话最右一列的字形会贴着画布边缘，
# 斜体和粗体的墨水会被裁掉一两个像素。
# （曾经还有个 CHAR_PX=8.42875 用来按列数算画布宽度 —— 那是手搓 Chrome
#  `--window-size` 时代的产物，画布尺寸现在由 agent-browser 的 --full 自己
#  按内容定，不用再算。）
FONT_PX = 14
LINE_PX = 17
PAD_PX = 8

# 渲染器：agent-browser（全局 npm 包）。它把"起浏览器 → 开页面 → 截图"
# 三件事包好了，本文件因此不再自己找 Chrome、不再算窗口尺寸。
RENDERER = "agent-browser"
# 用独占会话名，免得把用户（或别的工具）正开着的那个标签页导航走。
# 注意别碰 agbt-* 那批，那是另一个工具的。
RENDER_SESSION = "tuidbg"


def png_artifact_error(path):
    """产物自检：文件不存在 / 是空的 / 魔数不对，各返回一句原因；没问题返回 None。

    单独拎出来**是为了能在没有渲染器的机器上测到它**。它原本内联在渲染器
    调用之后，只有真跑一次浏览器才覆盖得到 —— 变异测试实测：把这三条检查
    逐条删掉，整套自检照样全绿（存活）。而这三条正是"渲染器喷了一堆 stderr
    但其实成功了"与"真失败"之间唯一可靠的判据，不能没有测试盯着。
    """
    if not os.path.isfile(path):
        return f"没生成 {path}"
    if os.path.getsize(path) == 0:
        return f"生成的 {path} 是空文件"
    with open(path, "rb") as f:
        if f.read(4) != b"\x89PNG":
            return f"{path} 不是 PNG（开头的魔数不对）"
    return None


def _run_renderer(args, timeout=120):
    """跑一条 agent-browser 子命令。

    返回 (CompletedProcess 或 None, 出错原因 或 None)。超时与起不来都归成
    一句人话 —— 这个工具的每条失败路径都必须留下可读的痕迹。
    """
    try:
        r = subprocess.run([RENDERER, "--session", RENDER_SESSION, *args],
                           capture_output=True, text=True, timeout=timeout)
    except subprocess.TimeoutExpired:
        return None, f"{RENDERER} {args[0]} 超过 {timeout}s 没返回（已杀掉）"
    except OSError as e:
        return None, f"起不来 {RENDERER}：{e}"
    return r, None


def _renderer_tail(r):
    """把 agent-browser 的输出压成一行，拼进失败消息里。
    它把 `✗ …` 打在 stderr，成功的 `✓ …` 打在 stdout。"""
    tail = (r.stderr or r.stdout or "").strip().splitlines()[-3:]
    return "；最后几行输出：" + " | ".join(tail) if tail else ""


def render_png(text, path, keep_html=None):
    """把带 SGR 的屏幕渲染成 PNG。成功返回 None，失败返回一句人话的原因。

    返回"原因字符串"而不是抛异常，是为了让调用方能把它拼进 stderr 那一行 ——
    这个工具的每条失败路径都必须留下可读的痕迹，traceback 不算。

    渲染交给 agent-browser：写一张临时 HTML → open file:// → screenshot。
    画布尺寸不再自己算，用 `--full` 让它按内容定 —— 实测不加时是固定的
    1280×577 视口，`--cols 200 --rows 60` 那种大 pane 会被**静默裁掉**
    （实测裁成 1280×577，加 --full 则是 1694×1036）；小屏两者逐字节相同。
    """
    if shutil.which(RENDERER) is None:
        return (f"找不到 {RENDERER}（PATH 上没有）。装它：npm i -g {RENDERER}"
                f"（首次可能还要跑一次 `{RENDERER} install` 下载浏览器）。")

    doc = ansi_to_html(text)
    path = os.path.abspath(path)

    parent = os.path.dirname(path) or "."
    if not os.path.isdir(parent):
        return f"输出目录不存在：{parent}"

    # 先删掉可能存在的旧文件：下面靠"文件是否存在"判定成败，
    # 留着上一次的产物会让这次的失败看起来像成功（本工具修过四次的老毛病）。
    with contextlib.suppress(OSError):
        os.remove(path)

    tmpdir = tempfile.mkdtemp(prefix="tuidbg-png-")
    hpath = os.path.join(tmpdir, "screen.html")
    try:
        with open(hpath, "w", encoding="utf-8") as f:
            f.write(doc)
        if keep_html:
            shutil.copyfile(hpath, keep_html)

        # **open 的退出码必须查**：实测 open 失败（rc=1）之后，紧跟的
        # screenshot 仍然 rc=0 并写出一张**上一个页面**的合格 PNG ——
        # 不查这一步，"渲染失败"就会带着一张张冠李戴的图报成成功，
        # 正是本工具反复修过的那个病的又一种变种。
        r, err = _run_renderer(["open", "file://" + hpath])
        if err:
            return err
        if r.returncode != 0:
            return f"{RENDERER} 打不开那张临时 HTML{_renderer_tail(r)}"

        r, err = _run_renderer(["screenshot", "--full", path])
        if err:
            return err

        # **不只看退出码**：产物本身存在且像个 PNG 才算成功。两个方向都要 ——
        # rc≠0 时补上产物诊断，rc=0 但没落盘时同样判失败。
        bad = png_artifact_error(path)
        if bad:
            return f"{RENDERER} 退出码 {r.returncode}，{bad}{_renderer_tail(r)}"
        if r.returncode != 0:
            return f"{RENDERER} 退出码 {r.returncode}{_renderer_tail(r)}"
        return None
    finally:
        shutil.rmtree(tmpdir, ignore_errors=True)


def _selftest_png():
    """PNG 分两层测：转换器（不需要浏览器）与端到端（需要 agent-browser，缺了就跳过）。"""
    # --- 单元层：SGR → HTML。这层是 --png 唯一自己写的逻辑，必须咬得住颜色。
    doc = ansi_to_html("\x1b[38;5;99mPURPLE\x1b[39m plain")
    # 99 是 yanshi TUI 里最常见的那个色（实测一屏出现 38 次），拿它当基准。
    # 99-16=83 → (2,1,5) → levels[2],levels[1],levels[5] = (135,95,255)
    assert "#875fff" in doc, f"256 色前景没进 HTML：{doc}"
    assert ">PURPLE<" in doc, "文本内容丢了"
    assert "plain" in doc and "38;5;99" not in doc, "转义序列不该以字面量出现在 HTML 里"

    assert "#cd0000" in ansi_to_html("\x1b[31mred"), "基础色 31 没转成红"
    assert "#ff0000" in ansi_to_html("\x1b[91mbright"), "亮色 91 没转成亮红"
    assert "background:#0000ee" in ansi_to_html("\x1b[44mbg"), "背景色 44 没转成背景"
    assert "#102030" in ansi_to_html("\x1b[38;2;16;32;48mtc"), "truecolour 没处理"
    assert "font-weight:bold" in ansi_to_html("\x1b[1mB"), "粗体丢了"
    assert "text-decoration:underline" in ansi_to_html("\x1b[4mU"), "下划线丢了"

    # 灰阶与色立方的边界值 —— 公式写错一个偏移就会整片偏色
    assert "#080808" in ansi_to_html("\x1b[38;5;232mg"), "灰阶起点错"
    assert "#eeeeee" in ansi_to_html("\x1b[38;5;255mg"), "灰阶终点错"
    assert "#000000" in ansi_to_html("\x1b[38;5;16mc"), "色立方起点错"
    assert "#ffffff" in ansi_to_html("\x1b[38;5;231mc"), "色立方终点错"

    # 0 必须清干净状态，否则整屏都会被第一个颜色染上
    reset = ansi_to_html("\x1b[31;1mA\x1b[0mB")
    assert reset.rsplit("B", 1)[0].count("#cd0000") == 1, f"reset 没清掉颜色：{reset}"

    # 反显要交换，且单独出现时得用默认色兜底（两边都是 None 时交换等于没交换）
    rev = ansi_to_html("\x1b[7mR")
    assert f"color:{BG_DEFAULT}" in rev and f"background:{FG_DEFAULT}" in rev, f"反显没生效：{rev}"

    # 38;5;N 必须**连吃三个参数**。用 44 而不是 99 来试：少吃一个时 99 落在
    # 所有 case 之外会被静默忽略、结果反而正确（变异测试实测这条会存活），
    # 而 44 会被当成 `48`… 不，是被当成"背景色 44"，于是凭空多出一块蓝底。
    # 断言整个 span 逐字相等（而不是 "background" not in 全文 —— 页面自己的
    # <style> 里就有 background，那样写在正确代码上也会红，实测过）。
    # 少吃一个参数时，44 会再被当成"背景色 44"，style 里多出 background:#0000ee。
    n44 = ansi_to_html("\x1b[38;5;44mX")
    assert '<span style="color:#00d7d7">X</span>' in n44, f"38;5;44 的参数没吃干净：{n44}"

    # HTML 转义：TUI 里出现 `<` `&` 是家常便饭，漏转会毁掉整个文档结构。
    # **两条路径都要测**：带 SGR 时走 chunk 分支、纯文本时走 tail 分支 ——
    # 只测后者的话，把 chunk 分支的 escape 删掉变异测试也杀不掉（实测存活）。
    esc = ansi_to_html("a<b>&c")
    assert "&lt;b&gt;" in esc and "&amp;c" in esc, f"HTML 未转义（tail 路径）：{esc}"
    esc2 = ansi_to_html("\x1b[31m<i>&amp\x1b[0m<j>")
    assert "<i>" not in esc2 and "&lt;i&gt;" in esc2, f"HTML 未转义（chunk 路径）：{esc2}"
    assert "<j>" not in esc2 and "&lt;j&gt;" in esc2, f"HTML 未转义（尾部）：{esc2}"

    # 不认识的序列静默跳过，既不崩也不吐字面量（光标移动、OSC 标题、私有 SGR）
    junk = ansi_to_html("\x1b[2J\x1b[10;5H\x1b]0;title\x07\x1b[38;5;999mX\x1b[?25lY")
    assert "X" in junk and "Y" in junk, f"正常文本被吃掉了：{junk}"
    assert "\x1b" not in junk and "[10;5H" not in junk, f"转义字节漏进了 HTML：{junk}"

    # strip_sgr 与 ansi_to_html 看到的是同一批文本
    assert strip_sgr("\x1b[31mred\x1b[0m") == "red", "strip_sgr 没去干净"

    # --- 失败路径：**不需要浏览器也必须测**，因为它是这次改动里唯一有可能
    # 重演"失败报成功"那个老病的地方。用"输出目录不存在"当触发器 ——
    # 真实失败、不用 mock，且无论有没有 agent-browser 都必然失败。
    bad = os.path.join(tempfile.gettempdir(), "tuidbg-no-such-dir-xyz", "a.png")
    assert render_png("x", bad) is not None, "写不出文件却报告了成功"

    # 产物自检的三条判据逐条压住（不需要浏览器）
    assert png_artifact_error(bad) is not None, "不存在的文件被判为合格产物"
    fd, probe = tempfile.mkstemp(suffix=".png", prefix="tuidbg-probe-")
    os.close(fd)
    try:
        # 断言**消息内容**而不只是"非 None"：空文件同样过不了下面的魔数检查，
        # 所以这条检查的价值全在诊断措辞上。只断言非 None 的话，把它删掉
        # 变异测试杀不掉（实测存活）—— 那样就等于没有测试保护这句人话。
        assert "空文件" in (png_artifact_error(probe) or ""), "空文件的诊断消息不对"
        with open(probe, "wb") as f:
            f.write(b"<html>not a png at all")
        assert png_artifact_error(probe) is not None, "魔数不对却被判为合格产物"
        with open(probe, "wb") as f:
            f.write(b"\x89PNG\r\n\x1a\n" + b"rest")
        assert png_artifact_error(probe) is None, "合格的 PNG 被误判为失败"
    finally:
        with contextlib.suppress(OSError):
            os.remove(probe)

    name = "selftest"
    tmux("kill-session", "-t", session_target(name))
    cmd_start(name, 60, 10, ["bash", "-c", "echo READY; sleep 100"], None)
    try:
        buf = io.StringIO()
        with contextlib.redirect_stdout(buf):
            rc = cmd_shot(name, wait=r"READY", timeout=10, ansi=False, png=bad)
        # 锚点是命中的（正常该返回 0），但 PNG 没写出来 —— 必须报失败。
        # 变异测试实测：把这里改回 `return rc` 时，除了这条断言没有任何测试会红。
        assert rc == PNG_FAIL_RC, f"PNG 失败必须返回 {PNG_FAIL_RC}，得到 {rc}（0 = 失败被报成了成功）"
        assert "READY" in buf.getvalue(), "PNG 失败不该吞掉屏幕内容"
    finally:
        cmd_stop(name)

    # 被测程序以 42 退出、同时 PNG 又失败 —— 按 PNG_FAIL_RC 的注释，PNG 失败盖掉它。
    tmux("kill-session", "-t", session_target(name))
    cmd_start(name, 60, 10, ["bash", "-c", "echo BYE; exit 42"], None)
    time.sleep(0.8)
    try:
        with contextlib.redirect_stdout(io.StringIO()):
            rc = cmd_shot(name, wait=None, timeout=5, ansi=False, png=bad)
        assert rc == PNG_FAIL_RC, f"PNG 失败应盖掉被测程序的 42，得到 {rc}"
    finally:
        cmd_stop(name)

    if shutil.which(RENDERER) is None:
        print(f"png OK (skipped: no {RENDERER})")
        return

    # --- 端到端层：真起一个会话，让 bash 画点带色的东西，渲染成 PNG。
    tmux("kill-session", "-t", session_target(name))
    painted = r'printf "\033[38;5;99mPURPLE\033[0m \033[41mRED_BG\033[0m READY\n"; sleep 100'
    cmd_start(name, 60, 10, ["bash", "-c", painted], None)

    fd, png = tempfile.mkstemp(suffix=".png", prefix="tuidbg-selftest-")
    os.close(fd)
    fd, htm = tempfile.mkstemp(suffix=".html", prefix="tuidbg-selftest-")
    os.close(fd)
    try:
        buf = io.StringIO()
        with contextlib.redirect_stdout(buf):
            rc = cmd_shot(name, wait=r"READY", timeout=10, ansi=False, png=png)
        assert rc == 0, f"带 --png 的 shot 应返回 0，得到 {rc}"

        assert os.path.isfile(png), "PNG 没生成"
        size = os.path.getsize(png)
        assert size > 0, "PNG 是空文件"
        with open(png, "rb") as f:
            assert f.read(4) == b"\x89PNG", "产物不是 PNG（魔数不对）"

        # --png 不得改变 stdout：没给 --ansi 就该是纯文本
        assert "\x1b" not in buf.getvalue(), "--png 把 ANSI 泄漏进了 stdout"
        assert "PURPLE" in buf.getvalue(), f"stdout 少了内容：{buf.getvalue()!r}"

        # 中间 HTML 里确实带着颜色（PNG 本身没法在这里判读）
        render_png(capture(name, ansi=True), png, keep_html=htm)
        doc = open(htm, encoding="utf-8").read()
        assert "#875fff" in doc, "端到端的 HTML 里没有那个紫色"

        # 渲染器**真的跑了、却没写出产物**：把输出路径指向一个已存在的目录。
        # 这条覆盖的是 screenshot 调用**之后**那段产物检查 —— 前面"目录不存在"
        # 的用例在调渲染器之前就返回了，够不到这里（变异测试实测：把
        # `bad = png_artifact_error(path)` 改成 `bad = None` 只有这条会红）。
        asdir = tempfile.mkdtemp(prefix="tuidbg-asdir-")
        try:
            err = render_png("x", asdir)
            assert err is not None, "渲染器没写出产物却报告了成功"
        finally:
            shutil.rmtree(asdir, ignore_errors=True)

        # **open 失败必须判失败** —— 哪怕紧跟的 screenshot 会成功。实测
        # agent-browser 在 open 失败（rc=1）后，screenshot 仍然 rc=0 并写出
        # 一张**上一个页面**的合格 PNG：不查 open 的退出码，这里就会拿着
        # 一张张冠李戴的图报成功，正是"失败报成功"那个老病的最新变种。
        # 先渲染一次让会话里留着一张好页面，再让 open 打不开的东西进来。
        fd, stale = tempfile.mkstemp(suffix=".png", prefix="tuidbg-stale-")
        os.close(fd)
        try:
            assert render_png("\x1b[31mSTALE", stale) is None, "预热渲染就失败了"
            gone = os.path.join(tempfile.mkdtemp(prefix="tuidbg-gone-"), "x.html")
            # 让 open 拿到一个不存在的 file:// —— 靠猴补 ansi_to_html 做不到，
            # 直接把渲染器指向缺失路径最省事：临时目录建了又删。
            os.rmdir(os.path.dirname(gone))
            r, err = _run_renderer(["open", "file://" + gone])
            assert err is None, f"探针自己没跑起来：{err}"
            assert r.returncode != 0, \
                "open 一个不存在的 file:// 竟然 rc=0 —— 上面那条防线的前提没了"
        finally:
            with contextlib.suppress(OSError):
                os.remove(stale)
    finally:
        for p in (png, htm):
            with contextlib.suppress(OSError):
                os.remove(p)
        cmd_stop(name)
        # 别把浏览器会话留着。只关我们自己那个具名会话，不碰别人的。
        _run_renderer(["close"], timeout=30)

    print(f"png OK ({size} bytes)")


def cmd_shot(name, wait, timeout, ansi, png=None):
    """抓屏。返回值即本命令的退出码。

    优先级：被测程序已退出 > --wait 命中 > 超时。
    "已退出"排第一，是为了让 --wait 不对着一具尸体空等满 timeout ——
    死掉的进程永远不会再让正则命中，那 10 秒纯属浪费，而且超时码会
    把死因说成"没等到"，掩盖真正的原因。

    --png 挂在**每一条**有屏幕内容的返回路径上（命中/超时/被测程序已退出），
    不是只挂在成功那条：崩溃那一屏恰恰是最想看的，而"给了 --png 却没文件、
    退出码还是 0"正是这个工具修过四次的失败形态。
    """
    if not session_exists(name):
        print(f"tuidbg: 会话 {PREFIX}{name} 不存在。先 start。", file=sys.stderr)
        return 1

    deadline = time.monotonic() + timeout
    pattern = re.compile(wait) if wait else None

    # 要 PNG 就一定得按 ansi 抓（不然渲出来是一张灰白的图），
    # 但吐给 stdout / 喂给正则的仍是 --ansi 说了算的那一份。
    # strip_sgr 的 docstring 里记着这两者逐字节相同的实测结论 ——
    # 所以不带 --ansi 时，加不加 --png 的文本行为完全一致。
    grab_ansi = ansi or png is not None

    while True:
        # 不写 `or ""`：空串是合法屏幕（刚起的会话就是空的），None 是抓屏失败，
        # 合并两者会让失败走进下面"无 pattern 即成功"的分支 —— rc=0 且 stdout
        # 空白，调用方以为一切正常。这与 capture 少了 -J 时把崩溃报成 rc=0
        # 是同一个病灶：把"抓不到"和"抓到空的"当成同一个结果。
        raw = capture(name, grab_ansi)
        if raw is None:
            print(
                f"tuidbg: 抓屏失败（会话 {PREFIX}{name} 中途消失？）",
                file=sys.stderr,
            )
            return 1
        screen = raw if ansi else strip_sgr(raw)

        code = dead_exit_code(screen)
        if code is not None:
            note = (
                f"\ntuidbg: 被测进程已退出，退出码 {code}"
                + ("（--wait 未命中即已退出）" if pattern and not pattern.search(screen) else "")
            )
            return _emit(screen, raw, code, png, note)

        if pattern is None or pattern.search(screen):
            return _emit(screen, raw, 0, png)

        if time.monotonic() >= deadline:
            note = f"\ntuidbg: 等 {timeout}s 没等到 /{wait}/。上面是最后一屏。"
            return _emit(screen, raw, TIMEOUT_RC, png, note)

        time.sleep(0.1)


def _emit(screen, raw, rc, png, note=None):
    """shot 的统一出口：打印屏幕 → 按需渲染 PNG → 定退出码。

    没给 --png 时逐字节等价于原来的 `sys.stdout.write(screen); return rc`。

    退出码的合并规则见 PNG_FAIL_RC 的注释：渲染失败**盖掉** rc。
    代价是被测程序真以 3 退出、同时 PNG 又成功了的场合，rc 仍是 3 却不是
    PNG 的错 —— 那一行 "PNG 已写入" 就是为了让这种情况能被分辨。
    """
    sys.stdout.write(screen)
    if note:
        print(note, file=sys.stderr)
    if png is None:
        return rc

    err = render_png(raw, png)
    if err:
        print(f"tuidbg: PNG 渲染失败：{err}", file=sys.stderr)
        return PNG_FAIL_RC
    print(f"tuidbg: PNG 已写入 {png}", file=sys.stderr)
    return rc


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
    time.sleep(0.4)
    buf = io.StringIO()
    with contextlib.redirect_stdout(buf):
        rc = cmd_shot(name, wait=r"NEVER_APPEARS", timeout=1, ansi=False)
    assert rc == TIMEOUT_RC, f"超时应返回 {TIMEOUT_RC}，得到 {rc}"
    assert "NOPE" in buf.getvalue(), f"超时必须打印最后一屏，实际 stdout:\n{buf.getvalue()!r}"
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

    # (e) 标记被 pane 边缘折断时仍须识别 —— 少了 capture 的 -J，
    # 退出码 3 的崩溃会被报成 rc=0 成功（最坏的失败形态）
    tmux("kill-session", "-t", session_target(name))
    cmd_start(name, 60, 10, ["bash", "-c", 'printf "%50s" X; exit 3'], None)
    time.sleep(0.8)
    rc = cmd_shot(name, wait=None, timeout=5, ansi=False)
    assert rc == 3, f"折行的标记也必须识别，得到 {rc}（rc=0 说明 -J 掉了）"
    cmd_stop(name)

    # (f) 死亡检查必须排在 deadline 检查之前 —— 只有在两个条件同一轮成立的
    # 窄窗口里才看得见：顺序反了会返回 124，把崩溃说成超时
    tmux("kill-session", "-t", session_target(name))
    cmd_start(name, 60, 10, ["bash", "-c", "sleep 1.0; exit 3"], None)
    rc = cmd_shot(name, wait=r"NEVER_APPEARS", timeout=1.05, ansi=False)
    assert rc == 3, f"死亡检查须优先于 deadline，得到 {rc}（124 = 顺序反了）"
    cmd_stop(name)

    # (g) 被测程序以 0 退出且 --wait 从未命中 —— rc 也是 0，与"命中"撞码。
    # 这是退出码唯一分不开的一对，调用方必须查 stdout 内容。曾骗过文档作者一次。
    tmux("kill-session", "-t", session_target(name))
    cmd_start(name, 60, 10, ["bash", "-c", "echo NOTHING_USEFUL; exit 0"], None)
    time.sleep(0.8)
    buf = io.StringIO()
    with contextlib.redirect_stdout(buf):
        rc = cmd_shot(name, wait=r"NEVER_APPEARS", timeout=5, ansi=False)
    assert rc == 0, f"以 0 退出应透传 0，得到 {rc}"
    assert "NEVER_APPEARS" not in buf.getvalue(), "锚点本不该出现却出现了，用例失效"
    assert "NOTHING_USEFUL" in buf.getvalue(), f"必须打印最后一屏，实际:\n{buf.getvalue()!r}"
    cmd_stop(name)

    print("shot OK")


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
    s.add_argument("--png", default=None, metavar="路径",
                   help="额外把这一屏渲染成 PNG（需要 agent-browser；失败返回 3）")

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
        _selftest_png()
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
        # 正则在这里先编译一次：cmd_shot 里那次 re.compile 无保护，
        # 用户敲错正则会喷 traceback 而不是一句人话
        if args.wait is not None:
            try:
                re.compile(args.wait)
            except re.error as e:
                print(f"tuidbg: --wait 不是合法正则: {e}", file=sys.stderr)
                return 1
        return cmd_shot(args.session, args.wait, args.timeout, args.ansi, args.png)
    if args.cmd == "stop":
        return cmd_stop(args.session)
    return 2


if __name__ == "__main__":
    sys.exit(main())
