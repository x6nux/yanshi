// shot.go —— 抓屏命令：轮询 → 打印屏幕 → 定退出码。
//
// 这是整个工具里唯一有分支逻辑的地方，也是唯一能把"失败报成成功"这件事真正
// 做出来的地方：前面三层（tmux 原语、SGR 解析、PNG 光栅）出错都会显式报错，
// 而本文件要把"程序崩了""没等到""抓不到屏"三种截然不同的结局压进一个 int。
// 压错了，调用方（多半是个 agent 循环）就会拿着 rc=0 继续往下走。
//
// 因此本文件里每一处顺序、每一次"这里为什么不合并"的选择都是承重的，
// 逐条记在下面的注释里。它们不是设计出来的，是 Python 前身被真实 bug
// 打出来的 —— 对应的用例在 shot_test.go 里按 (a)..(g) 编号保留。
//
// 本注释与 package 子句之间**故意**留了空行：它描述的是这个文件，不是这个包。

package main

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"time"
)

const (
	// timeoutRC 是 --wait 没等到时的退出码。取 124 是为了跟 coreutils 的
	// timeout(1) 对齐：调用方（包括人）看见 124 就知道是"没等到"，
	// 不需要先去查这个工具自己的约定。
	timeoutRC = 124

	// pngFailRC 是 --png 渲染或落盘失败时的退出码，它会**盖掉**本该返回的
	// 那个码（见 emit）。
	//
	// 为什么敢盖：被测程序可以返回任意值，所以不存在一个"不会撞车"的常数。
	// 既然撞车无法避免，就只能选一个撞车方向 —— 盖掉的最坏情况是
	// "成功被报成疑似失败"（吵闹、一看 stderr 就能分辨），不盖的最坏情况是
	// "失败被报成成功"（安静、没人会发现）。前者可解，后者不可解。
	pngFailRC = 3

	// pollInterval 是两次抓屏之间的间隔。抓一次屏要起一个 tmux 子进程，
	// 所以这个值同时决定了 CPU 开销与 --wait 的响应粒度。
	pollInterval = 100 * time.Millisecond
)

// exitMarkerRe 匹配 wrapper 打在屏幕上的退出标记。
//
// 用 QuoteMeta 而不是把 exitMarker 直接拼进正则：现在这个字面量里没有元字符，
// 但它是 session.go 的常量，改名时不该由本文件的正则去承担后果。
var exitMarkerRe = regexp.MustCompile(regexp.QuoteMeta(exitMarker) + `=(\d+)`)

// deadExitCode 在屏幕里找 wrapper 的退出标记，返回被测程序的退出码。
//
// **取最后一个匹配，不是第一个。** TUI 自己的 transcript 里完全可能碰巧出现
// 同一个字面量 —— 最典型的就是有人正在用这个工具调试这个工具，屏幕上于是
// 印着上一轮的标记。wrapper 打的那个永远在最后（它是被测程序退出之后才打的），
// 所以最后一个才是真的。取第一个会拿着一个陈旧的、甚至是屏幕内容里的假码
// 报出去，而这种错误没有任何外部症状。
//
// 数字解析失败时报"没找到"而不是报一个截断值：`\d+` 能匹配到任意长的数字串
// （屏幕内容里的巧合），而 wrapper 打的 `$?` 只可能是 0-255。往"没找到"倒，
// 最坏是继续轮询到超时；往"找到了"倒，最坏是拿一个编造的码当退出码。
func deadExitCode(screen string) (int, bool) {
	hits := exitMarkerRe.FindAllStringSubmatch(screen, -1)
	if len(hits) == 0 {
		return 0, false
	}
	code, err := strconv.Atoi(hits[len(hits)-1][1])
	if err != nil {
		return 0, false
	}
	return code, true
}

// shot 轮询屏幕直到出结果，把屏幕写到 stdout，并返回本命令的退出码。
//
// wait 为空表示不等任何东西（第一次抓屏就返回）。timeout 只约束 --wait 那条路径。
//
// **三个检查的顺序是承重的：已退出 > --wait 命中 > 超时。**
//
// "已退出"必须排第一，因为一个死掉的进程永远不会再让正则命中 —— 顺序反过来，
// --wait 就会对着一具尸体空等满整个 timeout，然后用一个"超时"的退出码
// 去描述一次崩溃，把真正的死因盖掉。实测：一个 1.0s 时退出的程序配 1.05s 的
// timeout，正确顺序返回 3，顺序反了返回 124（shot_test.go 的用例 (f)）。
//
// "--wait 命中"必须排在"超时"之前，理由同构：deadline 已过而锚点恰好就在
// 屏幕上时，报超时是在说一件假话。这一条用 timeout=0 能确定性地测出来
// （TestShotPatternMatchBeatsExpiredDeadline），不必去赌那个几十毫秒的窗口。
//
// --png 挂在**每一条**有屏幕内容的返回路径上（命中/超时/已退出），不是只挂在
// 成功那条：崩溃那一屏恰恰是最想看的一屏。
func shot(name string, wait string, timeout time.Duration, ansi bool, pngPath string) int {
	if !sessionExists(name) {
		fmt.Fprintf(os.Stderr, "tuidbg: 会话 %s%s 不存在。先 start。\n", sessionPrefix, name)
		return 1
	}

	// 正则在这里编译并把错误当人话报出去。Go 里写错正则不会像 Python 那样
	// 喷 traceback，但 MustCompile 会 panic —— 对一个用户随手敲进来的
	// --wait 来说，panic 和 traceback 是同一件事。
	var pattern *regexp.Regexp
	if wait != "" {
		p, err := regexp.Compile(wait)
		if err != nil {
			fmt.Fprintf(os.Stderr, "tuidbg: --wait 不是合法正则: %v\n", err)
			return 1
		}
		pattern = p
	}

	// time.Now 带单调时钟读数，Before 比较走的就是它，所以对时的系统不会
	// 让 deadline 提前或推后 —— 等价于 Python 那边显式的 time.monotonic()。
	deadline := time.Now().Add(timeout)

	// 要 PNG 就必须带颜色抓（不然渲出来是一张灰白的图），但吐给 stdout、
	// 喂给正则的仍然是 --ansi 说了算的那一份。stripSGR 的注释里记着这两者
	// 逐字节相同的实测结论 —— 所以不带 --ansi 时，加不加 --png 的文本行为完全一致。
	grabANSI := ansi || pngPath != ""

	for {
		// 不写 `screen, _ := capture(...)`：空串是合法屏幕（刚起的会话就是空的），
		// error 才是抓屏失败，合并两者会让失败走进下面"无 pattern 即成功"的分支 ——
		// rc=0 且 stdout 空白，调用方以为一切正常。capture 返回两个值就是为了
		// 让这里分得开，见它的注释。
		raw, err := capture(name, grabANSI)
		if err != nil {
			fmt.Fprintf(os.Stderr,
				"tuidbg: 抓屏失败（会话 %s%s 中途消失？）：%v\n", sessionPrefix, name, err)
			return 1
		}

		screen := raw
		if !ansi {
			screen = stripSGR(raw)
		}

		// 退出标记**一律**在去掉 SGR 之后的文本上找，哪怕用户要的是 --ansi。
		// ansi 为假时 plain 与 screen 本就是同一个串，这一步只对 --ansi 生效。
		//
		// 理由不是"防止 SGR 把标记劈开" —— 那个理由实测**不成立**：tmux 3.7c，
		// 60 列 pane，padding 从 52 到 62 逐个扫过折行缝、并让背景色属性正好
		// 在缝处改变，wrapper 的标记**一次都没被劈开**。tmux 只在属性变化处吐
		// SGR，而 wrapper 那行 printf 是一次性打出的单段等属性文本，缝合时
		// SGR 只落在标记之前（形如 `X^[[49m__TUIDBG_EXIT__=3`）。
		//
		// 真正的理由是 stripSGR 注释里那条等式的直接推论：**--ansi 不得改变
		// 退出码**。屏幕上一个内部带 SGR 的字面量（`__TUIDBG_<ESC>[31mEXIT__=99`，
		// 被测程序自己就能打出来）在 ansi=false 那份文本里是被接好的、会被匹配到；
		// 若这里改用 screen，同一屏在 --ansi 下就匹配不到它，于是两种模式给出
		// **不同的退出码** —— "顺手要个颜色"静默改掉了这个工具的判决。
		// 匹配到它是否理想是另一回事（那是"取最后一个"规则的已知边界，见
		// deadExitCode），但两种模式必须给出同一个答案。
		plain := screen
		if ansi {
			plain = stripSGR(raw)
		}

		if code, ok := deadExitCode(plain); ok {
			note := fmt.Sprintf("\ntuidbg: 被测进程已退出，退出码 %d", code)
			// 这句补充只在"给了 --wait 且它确实没命中"时才成立。
			// 只按"给没给 --wait"来判是错的：`--wait READY` 配一个打印
			// READY 之后就退出的程序，会得到一句"--wait 未命中"，而 READY
			// 明明白白就在屏幕上 —— 诊断信息自己在说假话，比没有诊断更坏。
			if pattern != nil && !pattern.MatchString(screen) {
				note += "（--wait 未命中即已退出）"
			}
			return emit(screen, raw, code, pngPath, note)
		}

		if pattern == nil || pattern.MatchString(screen) {
			return emit(screen, raw, 0, pngPath, "")
		}

		if !time.Now().Before(deadline) {
			note := fmt.Sprintf("\ntuidbg: 等 %s 没等到 /%s/。上面是最后一屏。", timeout, wait)
			return emit(screen, raw, timeoutRC, pngPath, note)
		}

		time.Sleep(pollInterval)
	}
}

// emit 是 shot 的统一出口：打印屏幕 → 按需渲染 PNG → 定退出码。
//
// 三条返回路径都从这里走，所以"超时也要打印最后一屏"这件事不是各分支各自
// 记得做，而是结构上做不掉。Python 前身那边它是三处独立的调用，注释声称
// 超时会打印屏幕，测试却只断言了退出码 —— 把那次写删掉没有任何测试会红。
//
// 没给 --png 时，行为逐字节等价于"写屏幕、返回 rc"。
//
// 给了 --png 时，stderr 上**必然**恰好出现两行里的一行：写成功（带路径）
// 或写失败（带原因）。这两行是互斥且穷尽的，因为"给了 --png 却既没有文件
// 也没有一句话"正是这个工具修过四次的失败形态。渲染失败会盖掉 rc，
// 理由见 pngFailRC；代价是被测程序真以 3 退出、PNG 又恰好成功时 rc 仍是 3
// 却不是 PNG 的错 —— 那一行"PNG 已写入"就是用来分辨这种情况的。
func emit(screen, raw string, rc int, pngPath, note string) int {
	// 每次都现取 os.Stdout 而不是缓存它：测试靠替换这个变量来抓输出。
	_, _ = os.Stdout.WriteString(screen)
	if note != "" {
		fmt.Fprintln(os.Stderr, note)
	}
	if pngPath == "" {
		return rc
	}

	if err := writePNG(raw, pngPath); err != nil {
		fmt.Fprintf(os.Stderr, "tuidbg: PNG 渲染失败：%v\n", err)
		return pngFailRC
	}
	fmt.Fprintf(os.Stderr, "tuidbg: PNG 已写入 %s\n", pngPath)
	return rc
}

// writePNG 把一屏带颜色的抓屏渲染成 PNG 并落到 path。
//
// **先在内存里渲完，再一次性写文件。** 不用 os.Create + 直接写：Create 会
// 立刻截断目标文件，此时若字体加载失败（renderGridPNG 保证一个字节都不写，
// 见它的注释），磁盘上就留下一个 0 字节的文件 —— 上一张好图没了，新图也没有，
// 而文件确实存在。缓冲之后，只有拿到完整字节才会去碰那个路径，
// 失败时目标文件保持原样。这是 Python 前身那边"产物自检"（查文件大小与 PNG
// 魔数）在纯 Go 管线里的对应物：那边是事后验尸，这里是根本不产出半成品。
func writePNG(raw, path string) error {
	var buf bytes.Buffer
	if err := renderGridPNG(parseANSI(raw), &buf); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}
