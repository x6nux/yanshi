// main.go —— tuidbg 的 CLI 入口：子命令分发、参数解析、退出码。
//
// 这是整个工具里唯一有 os.Exit 的地方，也是唯一把"某个函数返回的 int"变成
// "进程退出码"的地方。前面四层（session.go 的 tmux 原语、sgr.go 的解析、
// render.go 的光栅、shot.go 的判决）都已经把各自的结局压成一个 int 往上传，
// 本文件的全部职责就是别在最后一步把它弄丢。
//
// **没有任何一条路径隐式返回 0。** parseCommand 的出口是一个带 default 的
// switch，每个 case 都以 return 结束 —— Go 的 missing-return 分析因此认定它
// 是终止语句，函数尾部连一句 `return 0` 都写不出来（写了编译器会报 unreachable
// 之外的问题：它根本不需要那一句）。这不是洁癖：这个工具的前身把"失败报成成功"
// 做出来过四次，而其中最省事的一种做法就是在某条分支尾部漏掉 return，让它掉进
// 函数末尾那个 0。结构上不给这条缝留位置，比逐条 review 便宜。
//
// 参数处理与执行被切成两段（parseCommand → action），理由是**它们对环境的
// 依赖不同**：命令行写错了是命令行的事，与这台机器上有没有 tmux 无关，所以
// 参数错误必须先于 tmux 检查报出来。反过来，tmux 检查只需要写一次而不是在
// 五个 case 里各抄一遍 —— 抄五遍的版本会在新增第六个子命令时漏掉。
//
// 本注释与 package 子句之间**故意**留了空行：它描述的是这个文件，不是这个包。

package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	// defaultSession 是不带 -session 时用的会话名。它仍然会被加上
	// sessionPrefix，不做例外 —— 裸名一律带前缀，见 sessionTarget。
	defaultSession = "tuidbg"

	// defaultCols/defaultRows 是默认的 pane 尺寸。100x30 是这个工具在真实
	// yanshi TUI 上量下来够用的一档：再窄输入框会折行，再矮 transcript 存不住
	// 一轮问答。复现用户的布局问题时用 -cols/-rows 对齐对方的终端。
	defaultCols = 100
	defaultRows = 30

	// defaultTimeoutSec 是 -wait 的默认等待秒数。
	defaultTimeoutSec = 10.0

	// usageRC 是"命令行写错了"的退出码。取 2 是 Unix 的惯例（1 留给"程序跑了
	// 但没成功"），也让调用方能把"我调用姿势不对"与"被测程序有问题"分开 ——
	// 这个区分对一个跑在 agent 循环里的工具尤其重要，两者的修复动作完全不同。
	usageRC = 2
)

// main 只做一件事：把 run 的返回值交给 os.Exit。
//
// 所有逻辑都在 run 里而不在这里，为的是它可以被测试直接调用并拿到退出码 ——
// main 自己是测不了的（os.Exit 会把测试进程一起带走）。这个工具的退出码就是
// 它的主要契约，一个测不了退出码的入口等于契约没有测试保护。
func main() {
	os.Exit(run(os.Args[1:]))
}

// run 执行一次命令行调用，返回进程退出码。
//
// 顺序是承重的：**先把命令行完全解析完，再检查 tmux**。
// 反过来（先查 tmux）会让一台没装 tmux 的机器上连 `tuidbg -h` 都报"没有 tmux"，
// 而帮助信息与这台机器上装了什么毫无关系；也会让一个拼错的 flag 被报成环境问题，
// 把人送去装一个其实已经装好的东西。
func run(args []string) int {
	action, code := parseCommand(args)
	if action == nil {
		return code
	}
	if code := ensureTmux(); code != 0 {
		return code
	}
	return action()
}

// ensureTmux 检查 tmux 是否可执行，返回 0 表示可以继续。
//
// 这一层检查是**诊断性的**而不是安全性的：session.go 的 tmuxRun 本来就会把
// "起不来 tmux"报成 tmuxMissingRC，所以漏掉这里不会让失败变成成功。它存在的
// 理由只有一个 —— exec.LookPath 的失败信息（"executable file not found in
// $PATH"）说不出装它的办法，而这是新机器上最常见的一次失败。
//
// 退出码与 tmuxRun 用同一个 tmuxMissingRC，两处说的是同一件事，不该给调用方
// 两个码去区分同一个原因。
func ensureTmux() int {
	if _, err := exec.LookPath("tmux"); err != nil {
		fmt.Fprintln(os.Stderr,
			"tuidbg: 需要 tmux，但它不在 PATH 上。macOS: brew install tmux")
		return tmuxMissingRC
	}
	return 0
}

// usage 把用法写到 w。
//
// 写成"接收一个 io.Writer"而不是写死 os.Stderr：`-h` 要求帮助时它是**正常输出**
// 该进 stdout（否则 `tuidbg -h | less` 是空的），而没给子命令时它是**错误提示**
// 该进 stderr。同一份文本，两个去向，退出码也不同（0 与 usageRC）。
func usage(w io.Writer) {
	fmt.Fprint(w, `tuidbg —— 用 tmux 驱动 alt-screen TUI 的调试工具

用法：
  tuidbg start [-session NAME] [-cols N] [-rows N] [-cwd DIR] -- 命令...
  tuidbg send  [-session NAME] 文本
  tuidbg key   [-session NAME] 键名...
  tuidbg shot  [-session NAME] [-wait 正则] [-timeout 秒] [-ansi] [-png 路径]
  tuidbg stop  [-session NAME]

默认：-session tuidbg，-cols 100，-rows 30，-timeout 10。
会话名一律加 tuidbg- 前缀；所有 tmux 目标都用 = 强制精确匹配，不会误伤别的会话。

键名用 tmux 的词表：Enter Escape Tab Up Down BSpace C-c C-u …

以 - 开头的位置参数要写在 -- 之后，例如：
  tuidbg send -- -x
  tuidbg key -- -R          # 注意 -R 会被 tmux 当键名发出去，不是当选项

shot 的退出码：
  0     -wait 命中；或没给 -wait；或被测程序以 0 退出（后者看 stderr 分辨）
  124   等超时。最后一屏仍会打印，照着它看卡在哪
  3     -png 渲染失败。它会盖掉上面那些码，stderr 上有原因
  1     会话不存在、抓屏失败，或 -wait 不是合法正则
  其它   被测程序的真实退出码（它已经退出了）

单个子命令的 flag 详情：tuidbg <子命令> -h
`)
}

// newFlagSet 建一个子命令的 FlagSet，并预置所有子命令都有的 -session。
//
// 五个子命令都要 -session，抽在这里而不是各写一遍：写五遍的版本里，改默认值
// 或改帮助文本时漏掉一个是必然会发生的事，而漏掉的那个不会有任何症状 ——
// 它只是安静地用着另一个默认会话名。
//
// 用 ContinueOnError 而不是默认的 ExitOnError：ExitOnError 会自己调 os.Exit(2)，
// 把退出码的决定权从本文件手里拿走，run 也就再也测不了参数错误那条路径。
//
// **两股输出各进一个缓冲区，一个字都不直接落到流上。** flag 包默认把错误和
// 用法一起写 fs.Output()（默认 os.Stderr），于是同一份文本在"用户按了 -h"
// 与"用户写错了"两种场合去向相同 —— 而这两件事一个是成功一个是失败，
// 该进的流本来就不同（实测过一版：`tuidbg -h` 走 stdout 而 `tuidbg shot -h`
// 走 stderr，同一个 flag 在同一个工具里去了两个流）。分开缓冲之后，
// parseFlags 才有可能按结局决定去向，也才有可能把本工具自己的提示插进
// 错误行与用法之间，而不是垫在整坨 flag 包输出的最底下。
func newFlagSet(name, usageLine string) (*flag.FlagSet, *string, *flagBufs) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	bufs := &flagBufs{}
	fs.SetOutput(&bufs.errOut)
	fs.Usage = func() {
		fmt.Fprintf(&bufs.usage, "用法：tuidbg %s\n\n可用 flag：\n", usageLine)
		saved := fs.Output()
		fs.SetOutput(&bufs.usage)
		fs.PrintDefaults()
		fs.SetOutput(saved)
	}
	session := fs.String("session", defaultSession, "会话名（内部会加 tuidbg- 前缀）")
	return fs, session, bufs
}

// flagBufs 接住一个 FlagSet 的两股输出，好让 parseFlags 按结局决定它们的去向。
//
// errOut 是 flag 包写的那一行诊断（"flag provided but not defined: -x"），
// usage 是用法全文。分开存是因为它们在两种结局里的**顺序与去向都不同**：
// -h 时只要用法、进 stdout；出错时要"错误 → 怎么改 → 用法"三段，进 stderr。
type flagBufs struct {
	errOut strings.Builder
	usage  strings.Builder
}

// parseFlags 解析一个子命令的 flag，返回 (退出码, 是否已经处理完)。
//
// handled 为真时调用方必须立刻返回那个码，不能继续。三种结局：
//   - 解析成功 → (0, false)，继续往下走
//   - 用户要帮助（-h）→ (0, true)，用法进 **stdout**，这是**成功**不是错误：
//     把 -h 报成非零码会让 `tuidbg shot -h` 在 `set -e` 的脚本里炸掉，也会让
//     冒烟测试（这个仓库用 `yanshi -h` 做过同样的事）分不清"帮助能打印"与
//     "程序坏了"；把它写进 stderr 则会让 `tuidbg shot -h | grep png` 搜不到
//     任何东西。顶层 `tuidbg -h` 走的就是 stdout，两处必须一致。
//   - 解析失败 → (usageRC, true)，按"**错在哪 → 怎么改 → 用法**"的顺序进 stderr
//
// 中间那句"怎么改"是本工具自己加的，它补的是 stdlib 那句话缺的那一半：
// Go 的 flag 在遇到第一个**非 flag** 参数时才停止解析，所以一个以 `-` 开头的
// **位置**参数（`send -x`）会被报成 "flag provided but not defined: -x" ——
// 那句话既不提 `--` 也不提这可能根本不是个 flag，读完不知道该怎么改。
// 措辞同时涵盖拼错 flag 与 dash-positional 两种成因，因为 stdlib 给的
// 是同一句话，本工具分不出来，**猜一个说死了不如两个都说**。
func parseFlags(fs *flag.FlagSet, bufs *flagBufs, args []string) (int, bool) {
	err := fs.Parse(args)
	switch {
	case err == nil:
		return 0, false
	case errors.Is(err, flag.ErrHelp):
		fmt.Fprint(os.Stdout, bufs.usage.String())
		return 0, true
	default:
		fmt.Fprint(os.Stderr, bufs.errOut.String())
		fmt.Fprintf(os.Stderr,
			"tuidbg: 要么是 flag 拼错了，要么这是一个以 - 开头的位置参数 ——\n"+
				"        后者要写在 -- 之后，例如 `tuidbg %s -- -x`\n",
			fs.Name())
		fmt.Fprint(os.Stderr, bufs.usage.String())
		return usageRC, true
	}
}

// stripSeparator 去掉位置参数里领头的那个 `--`。
//
// Go 的 flag 包**已经**把第一个 `--` 当作解析终止符吃掉了（实测：
// `start -cwd X -- ./yanshi -inprocess` 之后 fs.Args() 是三个元素，没有 `--`），
// 所以正常写法根本走不到这个函数的循环体里。它存在是为了两件事：
//
// 其一，`tuidbg start -- -- ./yanshi` 这种多写一个的写法（实测第二个 `--`
// 会原样留在位置参数里，不去掉就会变成被测命令的名字，报 command not found，
// 而错误信息里那个 `--` 看起来像是本工具自己吐出来的）。
//
// 其二，Python 前身用的是 argparse.REMAINDER，那边 `--` 是留在列表里的，
// 于是 main 里有一句 `argv[1:] if argv[0] == '--'`。两边行为保持一致，
// 免得同一条命令行在两个实现下含义不同。
//
// 只去掉一个：`--` 之后的第二个 `--` 是被测命令自己的参数（`git log --`
// 这类写法真实存在），吃掉它就是在改用户的命令。
func stripSeparator(args []string) []string {
	if len(args) > 0 && args[0] == "--" {
		return args[1:]
	}
	return args
}

// parseCommand 把命令行解析成一个待执行的动作，或者一个直接可用的退出码。
//
// 返回 (action, code)：action 非 nil 时 code 无意义，调用方去跑 action；
// action 为 nil 时命令行已经处理完（帮助打完了、或者参数错了），返回 code。
//
// **本函数不碰 tmux，也不产生任何副作用**（除了往 stderr 写诊断）。这条边界
// 让"命令行合法性"可以在一台没有 tmux 的机器上被完整测试，也让 run 里那句
// ensureTmux 有一个明确的位置：在这之后、在 action 之前。
//
// 尾部的 switch 带 default 且每个 case 都 return，Go 因此认定整个 switch 是
// 终止语句 —— 函数末尾**写不出**一个兜底的 `return nil, 0`。见文件头注释。
func parseCommand(args []string) (func() int, int) {
	if len(args) == 0 {
		usage(os.Stderr)
		return nil, usageRC
	}

	switch sub := args[0]; sub {
	case "-h", "--help", "help":
		// 帮助是被明确要求的输出，进 stdout、返回 0。
		usage(os.Stdout)
		return nil, 0

	case "start":
		fs, session, bufs := newFlagSet("start",
			"start [-session NAME] [-cols N] [-rows N] [-cwd DIR] -- 命令...")
		cols := fs.Int("cols", defaultCols, "pane 宽度（列）")
		rows := fs.Int("rows", defaultRows, "pane 高度（行）")
		cwd := fs.String("cwd", "", "被测命令的工作目录（不给则继承 tmux server 的，多半不是你想要的）")
		if code, done := parseFlags(fs, bufs, args[1:]); done {
			return nil, code
		}
		// 两种写法都收：`start -- cmd args` 与 `start cmd args`。前者是文档里
		// 的写法，后者是人手快时的写法 —— Go 的 flag 在第一个非 flag 参数处
		// 就停了，所以后者本来就能工作，没有理由为了统一而拒绝它。
		argv := stripSeparator(fs.Args())
		if len(argv) == 0 {
			fmt.Fprintln(os.Stderr,
				"tuidbg: start 需要一条被测命令，写在 -- 之后。例如：\n"+
					`        tuidbg start -cwd "$PWD" -- ./yanshi --fake-model -inprocess`)
			return nil, usageRC
		}
		// tmux 自己也会拒绝 0 和负数（实测 3.7c：`-x 0` 与 `-x -5` 都返回 1
		// 并打 "width too small"），所以这一条不是防线而是措辞：tmux 那句话
		// 说不出是哪个 flag 错了，而 -cols/-rows 是本工具的名字，只有本工具
		// 说得出来。
		if *cols < 1 || *rows < 1 {
			fmt.Fprintf(os.Stderr,
				"tuidbg: -cols/-rows 必须是正数，得到 -cols %d -rows %d\n", *cols, *rows)
			return nil, usageRC
		}
		return func() int { return cmdStart(*session, *cols, *rows, argv, *cwd) }, 0

	case "send":
		fs, session, bufs := newFlagSet("send", "send [-session NAME] 文本")
		if code, done := parseFlags(fs, bufs, args[1:]); done {
			return nil, code
		}
		// 恰好一个参数，多了少了都报错。**不把多个参数用空格拼起来**：
		// 拼接会把 `send a  b`（两个空格）悄悄改成一个空格，而这个工具的用途
		// 正是"屏幕上出现的到底是什么"。要求引号，比替用户猜一个可能是错的
		// 答案好 —— 何况猜错了不会有任何症状。
		text := stripSeparator(fs.Args())
		if len(text) != 1 {
			fmt.Fprintf(os.Stderr,
				"tuidbg: send 要恰好一个文本参数（得到 %d 个）。带空格的文本请加引号：\n"+
					"        tuidbg send '纯 Go 渲染测试 hello'\n"+
					"        以 - 开头的文本写在 -- 之后：tuidbg send -- -x\n", len(text))
			return nil, usageRC
		}
		return func() int { return cmdSend(*session, text[0]) }, 0

	case "key":
		fs, session, bufs := newFlagSet("key", "key [-session NAME] 键名...")
		if code, done := parseFlags(fs, bufs, args[1:]); done {
			return nil, code
		}
		keys := stripSeparator(fs.Args())
		if len(keys) == 0 {
			fmt.Fprintln(os.Stderr,
				"tuidbg: key 至少要一个键名。tmux 的词表：Enter Escape Tab Up Down BSpace C-c C-u …\n"+
					"        以 - 开头的键名写在 -- 之后：tuidbg key -- -R")
			return nil, usageRC
		}
		// 键名不在本工具这边校验：tmux 的键名词表是 tmux 的，抄一份到这里就
		// 会有两份词表，而它们迟早不一致 —— 那时本工具会拒掉一个 tmux 明明
		// 认识的键。cmdKey 会把 tmux 自己的错误原样转出来。
		return func() int { return cmdKey(*session, keys) }, 0

	case "shot":
		fs, session, bufs := newFlagSet("shot",
			"shot [-session NAME] [-wait 正则] [-timeout 秒] [-ansi] [-png 路径]")
		wait := fs.String("wait", "", "轮询直到该正则在屏幕上命中")
		timeout := fs.Float64("timeout", defaultTimeoutSec, "-wait 的等待上限（秒）")
		ansi := fs.Bool("ansi", false, "stdout 保留 ANSI 颜色序列")
		png := fs.String("png", "", "额外把这一屏渲染成 PNG（纯 Go，不需要浏览器）")
		if code, done := parseFlags(fs, bufs, args[1:]); done {
			return nil, code
		}
		// shot 不吃位置参数。不报错的话，`shot -wait READY 30`（把 timeout
		// 写成位置参数）会静默用默认的 10 秒 —— 一次看起来像"TUI 太慢"的超时，
		// 实际是命令行写错了。
		if extra := fs.Args(); len(extra) > 0 {
			fmt.Fprintf(os.Stderr,
				"tuidbg: shot 不接受位置参数，多出来的是 %q。秒数要写成 -timeout 30\n", extra)
			return nil, usageRC
		}
		// **-wait 的正则不在这里编译。** shot() 在进入轮询循环之前就编译它，
		// 并把语法错误当人话报成 rc=1（见 shot.go 的注释）。在这里再编译一次
		// 只是同一个 stdlib 调用的第二个调用点：它不可能给出不同的判决，却会
		// 让 shot.go 里那段检查在生产中永不触发 —— 一段写了没人走的代码，
		// 正是这个仓库反复记录的那类缺陷。
		return func() int {
			return shot(*session, *wait, time.Duration(*timeout*float64(time.Second)), *ansi, *png)
		}, 0

	case "stop":
		fs, session, bufs := newFlagSet("stop", "stop [-session NAME]")
		if code, done := parseFlags(fs, bufs, args[1:]); done {
			return nil, code
		}
		if extra := fs.Args(); len(extra) > 0 {
			fmt.Fprintf(os.Stderr, "tuidbg: stop 不接受位置参数，多出来的是 %q\n", extra)
			return nil, usageRC
		}
		return func() int { return cmdStop(*session) }, 0

	default:
		// 把未知子命令与"看起来像 flag"的东西分开说：`tuidbg -x` 十有八九是
		// 把子命令的 flag 写到了子命令前面（`tuidbg -session foo shot`），
		// 那条错误说"未知子命令 -session"会把人往错的方向送。
		if strings.HasPrefix(sub, "-") {
			fmt.Fprintf(os.Stderr,
				"tuidbg: %q 看起来是个 flag，但 flag 要写在子命令**之后**：\n"+
					"        tuidbg shot -session foo（不是 tuidbg -session foo shot）\n", sub)
		} else {
			fmt.Fprintf(os.Stderr,
				"tuidbg: 未知子命令 %q。可用的是：start send key shot stop\n", sub)
		}
		usage(os.Stderr)
		return nil, usageRC
	}
}
