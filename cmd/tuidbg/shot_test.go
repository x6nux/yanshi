// shot_test.go —— shot 的轮询逻辑与退出码契约的测试。
//
// 用例按 (a)..(g) 编号，与 Python 前身 _selftest_shot 里的编号一一对应：
// 那七个用例每一个都是被一次真实的 bug 换来的，编号保留是为了两边能对读。
//
// 这里同样是驱动**真实 tmux**，理由与 session_test.go 一致：shot 的三条
// 判据（退出标记出现、正则命中、deadline 到期）全都建立在真实的进程生命周期
// 与真实的抓屏时序上，一个 fake 只会复读我们已经相信的东西。
//
// 会话名沿用 "gotest-shot-*" 命名空间，且每个用例一个独立名字 —— 用例之间
// 共用一个会话名会让 -run 单跑某一个和整包跑得到不同的初始状态。

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// captureOut 在 fn 执行期间同时接管 os.Stdout 与 os.Stderr，返回两者的内容。
//
// 两个流都要接管：shot 的契约横跨两者 —— 屏幕内容在 stdout，诊断在 stderr，
// 而"超时也必须打印最后一屏""PNG 结局必须恰好说一句话"这类断言只有拿到
// 对应的那个流才做得了。只接管 stdout 的话，stderr 会喷进测试输出里把
// 真正的失败信息淹掉。
//
// 两个流各起一个 goroutine 排空：写端把管道缓冲写满而没人读会死锁，
// 而一个挂住的测试比一个失败的测试更难查。
func captureOut(t *testing.T, fn func()) (stdout, stderr string) {
	t.Helper()

	rOut, wOut, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	rErr, wErr, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}

	savedOut, savedErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = wOut, wErr

	outCh, errCh := make(chan string, 1), make(chan string, 1)
	drain := func(r *os.File, ch chan string) {
		var sb strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			sb.Write(buf[:n])
			if err != nil {
				break
			}
		}
		ch <- sb.String()
	}
	go drain(rOut, outCh)
	go drain(rErr, errCh)

	func() {
		defer func() {
			os.Stdout, os.Stderr = savedOut, savedErr
			_ = wOut.Close()
			_ = wErr.Close()
		}()
		fn()
	}()

	stdout, stderr = <-outCh, <-errCh
	_ = rOut.Close()
	_ = rErr.Close()
	return stdout, stderr
}

// startShotSession 起一个跑 script 的会话，并登记退出时的清理。
//
// 每个用例一个独立会话名，清理走 t.Cleanup 而不是函数尾部的 cmdStop：
// 用例失败时 t.Fatalf 会直接返回，尾部的清理根本执行不到，于是一个
// 失败的用例会把会话留在机器上，让下一次跑同一个用例撞上 cmdStart 的
// 拒绝重名 —— 一次失败污染后续所有运行。
func startShotSession(t *testing.T, name string, cols, rows int, script string) {
	t.Helper()
	full := sessionPrefix + name
	killExact(full)
	t.Cleanup(func() { killExact(full) })
	if rc := cmdStart(name, cols, rows, []string{"bash", "-c", script}, ""); rc != 0 {
		t.Fatalf("cmdStart rc = %d, want 0", rc)
	}
}

// waitForMarker 轮询到 wrapper 的退出标记出现为止。
//
// 用轮询而不是固定 sleep：固定 sleep 要么在慢机器上不够（用例变 flaky）、
// 要么在快机器上白等。这也让"进程已经死了"成为用例的**前置条件**而不是
// 一个赌注 —— (d)(f)(g) 这几个用例的语义都建立在这个前提上。
func waitForMarker(t *testing.T, name string) {
	t.Helper()
	screen := waitForScreen(t, name, func(s string) bool {
		_, ok := deadExitCode(s)
		return ok
	})
	if _, ok := deadExitCode(screen); !ok {
		t.Fatalf("等不到退出标记，最后一屏:\n%s", screen)
	}
}

// requireBash 跳过需要 bash 的用例。所有用例都用 bash -c 造被测程序的行为
// （退出码、折行、时序），没有 bash 就没有被测对象。
func requireBash(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash 不在 PATH 上；跳过")
	}
}

// TestDeadExitCodeTakesLastMatch 把 deadExitCode 当纯函数测。
//
// **最后一个匹配**是这里唯一有分量的一条：TUI 自己的 transcript 完全可能
// 碰巧带着同样的字面量（最典型的就是有人正在用这个工具调试这个工具），
// wrapper 打的那个永远在最后。取第一个会拿屏幕内容里的假码当退出码报出去，
// 而这种错误没有任何外部症状 —— 只有这条断言拦得住。
func TestDeadExitCodeTakesLastMatch(t *testing.T) {
	if _, ok := deadExitCode("屏幕上什么标记都没有\nhello world"); ok {
		t.Error("没有标记却报告找到了")
	}

	code, ok := deadExitCode("some output\n" + exitMarker + "=7\n")
	if !ok || code != 7 {
		t.Errorf("单个标记：得到 (%d, %v)，想要 (7, true)", code, ok)
	}

	// 两个标记：必须取最后一个。两个值都挑成非零且互不相同，
	// 否则"取第一个"与"取最后一个"会得出同样的结果，断言恒真。
	screen := "用户在屏幕上打了 " + exitMarker + "=1\n" + exitMarker + "=42\n"
	code, ok = deadExitCode(screen)
	if !ok || code != 42 {
		t.Errorf("两个标记：得到 (%d, %v)，想要 (42, true)（1 = 取了第一个）", code, ok)
	}

	// 标记后面跟的不是数字，等于没有标记 —— 不能把它当成 0 报出去。
	if _, ok := deadExitCode(exitMarker + "=abc"); ok {
		t.Error("标记后面不是数字，却报告找到了退出码")
	}
}

// TestShotMissingSession 覆盖入口处的会话检查。
//
// 断言的是 stderr 的内容而不只是 rc：rc=1 在这个工具里同时是"抓屏失败"
// 的码，两者靠消息区分，而"先 start"这句人话正是这条路径唯一的产出。
func TestShotMissingSession(t *testing.T) {
	requireTmux(t)

	var rc int
	_, stderr := captureOut(t, func() {
		rc = shot("gotest-shot-no-such-session", "", 5*time.Second, false, "")
	})
	if rc != 1 {
		t.Errorf("会话不存在应返回 1，得到 %d", rc)
	}
	if !strings.Contains(stderr, "不存在") {
		t.Errorf("stderr 没说清会话不存在：%q", stderr)
	}
}

// TestShotA_WaitHits 是用例 (a)：--wait 命中 → 0。
func TestShotA_WaitHits(t *testing.T) {
	requireTmux(t)
	requireBash(t)

	const name = "gotest-shot-a"
	startShotSession(t, name, 60, 10, "sleep 0.5; echo READY; sleep 100")

	var rc int
	stdout, _ := captureOut(t, func() {
		rc = shot(name, "READY", 5*time.Second, false, "")
	})
	if rc != 0 {
		t.Errorf("--wait 命中应返回 0，得到 %d", rc)
	}
	if !strings.Contains(stdout, "READY") {
		t.Errorf("命中时 stdout 少了屏幕内容：%q", stdout)
	}
}

// TestShotB_WaitTimesOut 是用例 (b)：--wait 超时 → 124，**且最后一屏必须在 stdout 上**。
//
// 那条 stdout 断言是这个用例的重点，不是附赠品。Python 前身的注释声称
// "超时必须打印最后一屏"，断言却只查了返回码 —— 把那次写删掉，整包测试
// 依然全绿。一个声称自己在保护某件事、实际没有的测试，比没有测试更坏：
// 它会让下一个人相信那件事有人管。
func TestShotB_WaitTimesOut(t *testing.T) {
	requireTmux(t)
	requireBash(t)

	const name = "gotest-shot-b"
	startShotSession(t, name, 60, 10, "echo NOPE; sleep 100")
	waitForScreen(t, name, func(s string) bool { return strings.Contains(s, "NOPE") })

	var rc int
	stdout, stderr := captureOut(t, func() {
		rc = shot(name, "NEVER_APPEARS", time.Second, false, "")
	})
	if rc != timeoutRC {
		t.Errorf("超时应返回 %d，得到 %d", timeoutRC, rc)
	}
	if !strings.Contains(stdout, "NOPE") {
		t.Errorf("超时必须打印最后一屏，实际 stdout: %q", stdout)
	}
	// 诊断里要点出那个没等到的正则，否则调用方看见 124 也不知道在等什么。
	if !strings.Contains(stderr, "NEVER_APPEARS") {
		t.Errorf("超时诊断没点名那个正则：%q", stderr)
	}
}

// TestShotC_ProgramExits 是用例 (c)：被测程序以 42 退出 → 返回 42。
//
// 42 是刻意挑的：既不是 0（成功）也不是 124（超时）也不是 1（内部失败）
// 也不是 3（PNG 失败），任何一条别的路径漏进来都会被这个值抓到。
func TestShotC_ProgramExits(t *testing.T) {
	requireTmux(t)
	requireBash(t)

	const name = "gotest-shot-c"
	startShotSession(t, name, 60, 10, "echo BYE; exit 42")
	waitForMarker(t, name)

	var rc int
	stdout, stderr := captureOut(t, func() {
		rc = shot(name, "", 5*time.Second, false, "")
	})
	if rc != 42 {
		t.Errorf("应报告被测程序的退出码 42，得到 %d", rc)
	}
	if !strings.Contains(stdout, "BYE") {
		t.Errorf("stdout 少了屏幕内容：%q", stdout)
	}
	if !strings.Contains(stderr, "已退出") {
		t.Errorf("stderr 没说清进程已退出：%q", stderr)
	}
}

// TestShotD_DeadShortCircuits 是用例 (d)：--wait 撞上已死进程要立刻短路。
//
// 断言里的**耗时**才是这个用例的内容：光断言 rc=7 的话，一个"等满 10 秒
// 再报 7"的实现照样通过，而那正是这条规则要禁止的行为。
func TestShotD_DeadShortCircuits(t *testing.T) {
	requireTmux(t)
	requireBash(t)

	const name = "gotest-shot-d"
	startShotSession(t, name, 60, 10, "exit 7")
	waitForMarker(t, name)

	var rc int
	start := time.Now()
	captureOut(t, func() {
		rc = shot(name, "NEVER_APPEARS", 10*time.Second, false, "")
	})
	elapsed := time.Since(start)

	if rc != 7 {
		t.Errorf("死进程应报告退出码 7，得到 %d", rc)
	}
	if elapsed > 3*time.Second {
		t.Errorf("应立即短路，实际等了 %v（timeout 是 10s）", elapsed)
	}
}

// TestShotE_MarkerSplitByPaneEdge 是用例 (e)：被 pane 边缘折断的标记仍须识别。
//
// 50 列 padding 打进 60 列的 pane，光标停在右边缘附近，wrapper 的标记于是
// 跨行 —— 实测抓到的是 `X__TUIDBG_E` 和下一行的 `XIT__=3`。少了 capture 的 -J，
// 这里返回 **0**：一次退出码为 3 的崩溃被报成干净的成功，这是这个工具
// 能产出的最坏输出。
func TestShotE_MarkerSplitByPaneEdge(t *testing.T) {
	requireTmux(t)
	requireBash(t)

	const name = "gotest-shot-e"
	startShotSession(t, name, 60, 10, `printf "%50s" X; exit 3`)
	waitForMarker(t, name)

	var rc int
	captureOut(t, func() {
		rc = shot(name, "", 5*time.Second, false, "")
	})
	if rc != 3 {
		t.Errorf("折行的标记也必须识别，得到 %d（0 说明 -J 掉了）", rc)
	}
}

// TestShotF_DeadCheckBeatsDeadline 是用例 (f)：死亡检查必须排在 deadline 检查之前。
//
// 顺序反了会把一次崩溃说成超时（124），而崩溃的真实退出码就此丢失。
// 只有当"标记已在屏上"与"deadline 已过"在**同一轮轮询**里同时成立时，
// 两种顺序才会给出不同的答案 —— 别的用例都看不见这个顺序：(d) 里进程
// 早就死了而 deadline 在 10 秒开外，挪到前面的 deadline 检查根本不会触发。
//
// 构造方式与 Python 前身不同，这是实测逼出来的。前身用 `sleep 1.0; exit 3`
// 配 1.05s 的 timeout 去**赌**那个约 0.1s 的窗口，而这个赌注两个方向都不牢：
// 程序是在 cmdStart 里就开始跑的，比 shot 的计时起点更早，所以标记通常在
// deadline **之前**就被看见 —— 那一轮里 deadline 尚未到期，反序的实现同样
// 返回 3，变异存活。反过来在负载高的机器上标记可能晚于 deadline 才出现，
// 正确的实现反而返回 124，用例假红。实测本机跑那个构造：见本文件末尾的
// TestShotF_HistoricalTimingShape。
//
// 这里改成让两个条件**确定性地**同时成立：先等到标记真的出现（前置条件，
// 不是赌注），再用 timeout=0 让 deadline 在第一轮轮询时就已经过期。
// 语义与那个窄窗口完全一致 —— 同一轮里两个条件都为真 —— 但没有任何时序竞争。
//
// 变异实测（把 deadline 检查挪到最前）：**只有这个用例变红**。(d) 绿、(c) 绿、
// 下面那个跑原始构造的 TestShotF_HistoricalTimingShape 也绿 —— 即原样照搬
// Python 那个赌窗口的构造，杀不掉这个变异。这个用例的存在理由就是这条测量。
func TestShotF_DeadCheckBeatsDeadline(t *testing.T) {
	requireTmux(t)
	requireBash(t)

	const name = "gotest-shot-f"
	startShotSession(t, name, 60, 10, "echo WORKING; exit 3")
	waitForMarker(t, name)

	var rc int
	captureOut(t, func() {
		// timeout=0：deadline 在进入循环时就已经过期，标记也已在屏上。
		rc = shot(name, "NEVER_APPEARS", 0, false, "")
	})
	if rc != 3 {
		t.Errorf("死亡检查须优先于 deadline，得到 %d（%d = 顺序反了）", rc, timeoutRC)
	}
}

// TestShotPatternMatchBeatsExpiredDeadline 是 (f) 的孪生：命中检查也必须排在
// deadline 之前。
//
// deadline 已过、而锚点恰好就在屏幕上时报超时，是在说一件假话：调用方要等的
// 东西明明已经到了。同样用 timeout=0 让它确定性成立，不去赌窗口。
func TestShotPatternMatchBeatsExpiredDeadline(t *testing.T) {
	requireTmux(t)
	requireBash(t)

	const name = "gotest-shot-order2"
	startShotSession(t, name, 60, 10, "echo READY; sleep 100")
	waitForScreen(t, name, func(s string) bool { return strings.Contains(s, "READY") })

	var rc int
	captureOut(t, func() {
		rc = shot(name, "READY", 0, false, "")
	})
	if rc != 0 {
		t.Errorf("锚点已在屏上时不该报超时，得到 %d", rc)
	}
}

// TestShotAnsiDoesNotChangeTheExitCode 钉住"--ansi 不得改变退出码"。
//
// 这条是变异测试逼出来的，而且**推翻了它自己的第一版理由**。原先代码里
// 对 ansi=true 多剥一次 SGR 再找退出标记，注释说是"防止一段 SGR 把标记从
// 中间劈开"。删掉那一步，整包全绿 —— 于是去量那个理由：tmux 3.7c，60 列
// pane，padding 从 52 扫到 62 让 wrapper 的标记逐个落在折行缝的各个位置，
// 并让背景色属性正好在缝处改变。**一次都劈不开**。tmux 只在属性变化处吐
// SGR，wrapper 那行 printf 是单段等属性文本，缝合时 SGR 只落在标记之前
// （`X^[[49m__TUIDBG_EXIT__=3`）。原来的理由是假的。
//
// 但删掉那一步照样是错的，只是错在别处 —— 这个用例第一次跑就把它抓了出来：
// 屏幕上一个**内部带 SGR** 的字面量（被测程序自己能打出来，见下面的 printf）
// 在 ansi=false 的那份文本里是被接好的、会被匹配到；用 screen 去找标记的话，
// 同一屏在 --ansi 下匹配不到它，两种模式于是给出**不同的退出码**。实测：
// ansi=false 得 99、ansi=true 得 3。
//
// 那正是 stripSGR 注释里那条等式要禁止的事："顺手要个颜色"静默改掉判决。
// 所以断言不是"哪个码才对"（那是"取最后一个"规则的已知边界，见 deadExitCode），
// 而是**两种模式必须给出同一个码**。这个形状同时杀掉两个方向的变异。
func TestShotAnsiDoesNotChangeTheExitCode(t *testing.T) {
	requireTmux(t)
	requireBash(t)

	const name = "gotest-shot-sgrmarker"
	// wrapper 先打真标记（exit 3），后台进程随后再打一个内部带 SGR 的字面量。
	startShotSession(t, name, 60, 10,
		`{ sleep 0.6; printf '__TUIDBG_\033[31mEXIT__=99\n'; sleep 100; } & exit 3`)
	waitForMarker(t, name)
	// 等那个带 SGR 的字面量也画上去，否则这个用例根本没造出它要测的局面。
	waitForScreen(t, name, func(s string) bool { return strings.Contains(s, "=99") })

	var got [2]int
	for i, ansi := range []bool{false, true} {
		captureOut(t, func() {
			got[i] = shot(name, "", 5*time.Second, ansi, "")
		})
	}
	if got[0] != got[1] {
		t.Errorf("--ansi 改变了退出码：ansi=false 得 %d，ansi=true 得 %d。"+
			"退出标记必须在两种模式下看到同一份文本（见 stripSGR 的等式）", got[0], got[1])
	}
}

// TestShotG_ExitZeroCollidesWithMatch 是用例 (g)：被测程序以 0 退出、而 --wait
// 从未命中 —— rc 也是 0，与"命中"撞码。
//
// 这是退出码唯一分不开的一对，调用方必须查 stdout 内容。这个用例的价值全在
// 那两条内容断言上：锚点**不在**屏幕上（否则用例自己失效了，它测的就不再是
// "没命中"），而最后一屏**在** stdout 上（这是调用方唯一能用来分辨的东西）。
// 这一对曾骗过文档作者一次。
func TestShotG_ExitZeroCollidesWithMatch(t *testing.T) {
	requireTmux(t)
	requireBash(t)

	const name = "gotest-shot-g"
	startShotSession(t, name, 60, 10, "echo NOTHING_USEFUL; exit 0")
	waitForMarker(t, name)

	var rc int
	stdout, stderr := captureOut(t, func() {
		rc = shot(name, "NEVER_APPEARS", 5*time.Second, false, "")
	})
	if rc != 0 {
		t.Errorf("以 0 退出应透传 0，得到 %d", rc)
	}
	if strings.Contains(stdout, "NEVER_APPEARS") {
		t.Error("锚点本不该出现却出现了，用例失效")
	}
	if !strings.Contains(stdout, "NOTHING_USEFUL") {
		t.Errorf("必须打印最后一屏，实际 stdout: %q", stdout)
	}
	// 退出码分不开，诊断必须分得开：这条路径要说清"没命中就已经退出了"。
	if !strings.Contains(stderr, "--wait 未命中") {
		t.Errorf("stderr 没说清是未命中即退出：%q", stderr)
	}
}

// TestShotDeadNoteOnlyWhenPatternMissed 钉住那句补充说明的触发条件。
//
// 它只在"给了 --wait 且它确实没命中"时才能出现。按"给没给 --wait"来判是错的：
// `--wait READY` 配一个打印 READY 之后就退出的程序，会得到一句"--wait 未命中"，
// 而 READY 明明白白就在屏幕上 —— 诊断信息自己在说假话，比没有诊断更坏。
//
// 与 (g) 是同一句话的两个方向：(g) 断言该出现时出现，这里断言不该出现时不出现。
func TestShotDeadNoteOnlyWhenPatternMissed(t *testing.T) {
	requireTmux(t)
	requireBash(t)

	const name = "gotest-shot-note"
	startShotSession(t, name, 60, 10, "echo READY; exit 5")
	waitForMarker(t, name)

	var rc int
	_, stderr := captureOut(t, func() {
		rc = shot(name, "READY", 5*time.Second, false, "")
	})
	if rc != 5 {
		t.Errorf("应报告退出码 5，得到 %d", rc)
	}
	if !strings.Contains(stderr, "已退出") {
		t.Errorf("stderr 没说清进程已退出：%q", stderr)
	}
	if strings.Contains(stderr, "--wait 未命中") {
		t.Errorf("READY 就在屏幕上，却声称 --wait 未命中：%q", stderr)
	}
}

// TestShotCaptureFailureIsNotABlankScreen 覆盖轮询中途抓屏失败的那条路径。
//
// 会话在轮询过程中被杀掉 —— 入口检查已经过了，capture 于是返回 error。
// 这条路径必须返回 1 并说人话，**绝不能**当成一屏空白：Python 前身写的是
// `capture(...) or ""`，抓屏失败就此掉进下面"无 pattern 即成功"的分支，
// 返回 0 且 stdout 空白，调用方以为一切正常。capture 返回两个值就是为了
// 让这里分得开，这条用例是那个设计唯一的消费侧证据。
func TestShotCaptureFailureIsNotABlankScreen(t *testing.T) {
	requireTmux(t)
	requireBash(t)

	const name = "gotest-shot-vanish"
	full := sessionPrefix + name
	startShotSession(t, name, 60, 10, "echo ALIVE; sleep 100")
	waitForScreen(t, name, func(s string) bool { return strings.Contains(s, "ALIVE") })

	// 进入轮询之后再杀。--wait 用一个永不命中的锚点，让它一定会走到第二轮。
	go func() {
		time.Sleep(250 * time.Millisecond)
		killExact(full)
	}()

	var rc int
	stdout, stderr := captureOut(t, func() {
		rc = shot(name, "NEVER_APPEARS", 30*time.Second, false, "")
	})
	if rc != 1 {
		t.Errorf("中途抓屏失败应返回 1，得到 %d（0 = 失败被当成了空屏幕）", rc)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Errorf("抓屏失败不该往 stdout 写屏幕内容：%q", stdout)
	}
	if !strings.Contains(stderr, "抓屏失败") {
		t.Errorf("stderr 没说清是抓屏失败：%q", stderr)
	}
}

// TestShotInvalidWaitRegex 钉住"用户敲错正则得到一句人话而不是 panic"。
//
// Go 这边不用 MustCompile 的理由与 Python 那边不喷 traceback 是同一个：
// --wait 是用户随手敲进来的字符串，它的语法错误是一种正常的用户输入，
// 不是程序的内部错误。
func TestShotInvalidWaitRegex(t *testing.T) {
	requireTmux(t)
	requireBash(t)

	const name = "gotest-shot-badre"
	startShotSession(t, name, 60, 10, "sleep 100")

	var rc int
	_, stderr := captureOut(t, func() {
		rc = shot(name, "(unclosed", 5*time.Second, false, "")
	})
	if rc != 1 {
		t.Errorf("非法正则应返回 1，得到 %d", rc)
	}
	if !strings.Contains(stderr, "合法正则") {
		t.Errorf("stderr 没说清是正则不合法：%q", stderr)
	}
}

// TestShotPNGFailureOverridesRC 钉住 pngFailRC 的**覆盖**语义。
//
// 两条腿覆盖的是同一条规则的两种代价：
//
// 其一，锚点命中（本该返回 0）而 PNG 写不出 —— 必须报 pngFailRC。
// Python 那边的变异测试实测：把这里改回 `return rc`，除了这条断言没有
// 任何测试会红。
//
// 其二，被测程序真以 42 退出、PNG 又失败 —— pngFailRC **盖掉** 42。
// 这条腿才是"覆盖"这个词的内容：只测第一条腿的话，一个"仅在 rc==0 时
// 才改成 3"的实现照样通过，而那不是这里想要的语义。
//
// 触发器用"输出目录不存在"：真实失败、不用打桩、且与本机有没有字体无关。
func TestShotPNGFailureOverridesRC(t *testing.T) {
	requireTmux(t)
	requireBash(t)

	bad := filepath.Join(t.TempDir(), "no-such-dir-xyz", "a.png")

	t.Run("命中路径", func(t *testing.T) {
		const name = "gotest-shot-pngfail1"
		startShotSession(t, name, 60, 10, "echo READY; sleep 100")
		waitForScreen(t, name, func(s string) bool { return strings.Contains(s, "READY") })

		var rc int
		stdout, stderr := captureOut(t, func() {
			rc = shot(name, "READY", 10*time.Second, false, bad)
		})
		if rc != pngFailRC {
			t.Errorf("PNG 失败必须返回 %d，得到 %d（0 = 失败被报成了成功）", pngFailRC, rc)
		}
		if !strings.Contains(stdout, "READY") {
			t.Errorf("PNG 失败不该吞掉屏幕内容：%q", stdout)
		}
		if !strings.Contains(stderr, "PNG 渲染失败") {
			t.Errorf("stderr 没说清 PNG 失败：%q", stderr)
		}
	})

	t.Run("盖掉被测程序的退出码", func(t *testing.T) {
		const name = "gotest-shot-pngfail2"
		startShotSession(t, name, 60, 10, "echo BYE; exit 42")
		waitForMarker(t, name)

		var rc int
		captureOut(t, func() {
			rc = shot(name, "", 5*time.Second, false, bad)
		})
		if rc != pngFailRC {
			t.Errorf("PNG 失败应盖掉被测程序的 42，得到 %d", rc)
		}
	})
}

// TestShotPNGFailureLeavesNoStubFile 钉住 writePNG 的"先渲完再落盘"。
//
// 把输出指向一个**已存在且内容已知**的文件，再让渲染失败（没字体时）或成功。
// 关键断言只有一条：渲染失败时那个文件必须原封不动。直接 os.Create 会立刻
// 把它截成 0 字节 —— 上一张好图没了、新图也没有，而文件确实存在，
// 于是"文件在不在"这个最朴素的检查会给出错误的答案。
//
// 本机有字体时这条渲染会成功，那就换个必然失败的触发器（目录不存在的父路径
// 够不到这个断言，因为它连文件都不会碰）——用一个**只读目录**下的路径，
// 让 os.WriteFile 在渲染成功之后才失败。
func TestShotPNGFailureLeavesNoStubFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "existing.png")
	const sentinel = "上一张好图的内容"
	if err := os.WriteFile(target, []byte(sentinel), 0o644); err != nil {
		t.Fatalf("准备 sentinel 文件失败：%v", err)
	}

	// 直接测 writePNG，不经 tmux：这条规则是关于文件系统的，与会话无关。
	// 用一个必然渲染失败的输入 —— 把字体候选清空。
	saved := fontCandidatesForTest
	fontCandidatesForTest = []fontCandidate{}
	t.Cleanup(func() { fontCandidatesForTest = saved })

	if err := writePNG("hello", target); err == nil {
		t.Fatal("没有字体却报告渲染成功")
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("sentinel 文件读不回来了：%v", err)
	}
	if string(got) != sentinel {
		t.Errorf("渲染失败却动了目标文件：得到 %q，想要 %q（空 = 被截断了）", got, sentinel)
	}
}

// TestShotPNGSuccessReportsPath 钉住成功那一半：PNG 写出来了要说，且不改 rc。
//
// 与失败那条合起来构成"stderr 上必然恰好出现两行里的一行"的两个方向。
// 少了这一条，一个"永远不打印任何 PNG 相关信息"的实现能通过全部失败用例，
// 而调用方就此失去了分辨"rc=3 是被测程序的还是 PNG 的"的唯一依据。
func TestShotPNGSuccessReportsPath(t *testing.T) {
	requireTmux(t)
	requireBash(t)
	requireFonts(t)

	const name = "gotest-shot-pngok"
	startShotSession(t, name, 60, 10, "echo READY; sleep 100")
	waitForScreen(t, name, func(s string) bool { return strings.Contains(s, "READY") })

	out := filepath.Join(t.TempDir(), "shot.png")
	var rc int
	stdout, stderr := captureOut(t, func() {
		rc = shot(name, "READY", 10*time.Second, false, out)
	})
	if rc != 0 {
		t.Errorf("PNG 成功时应透传 0，得到 %d", rc)
	}
	if !strings.Contains(stderr, "PNG 已写入") || !strings.Contains(stderr, out) {
		t.Errorf("stderr 没报告 PNG 写入路径：%q", stderr)
	}
	if strings.Contains(stderr, "PNG 渲染失败") {
		t.Errorf("同时报告了成功与失败：%q", stderr)
	}
	// 不带 --ansi 时，--png 不得把转义序列泄漏进 stdout。
	if strings.ContainsRune(stdout, 0x1b) {
		t.Errorf("--png 把 ANSI 泄漏进了 stdout：%q", stdout)
	}

	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("PNG 没生成：%v", err)
	}
	if len(b) < 8 || string(b[:4]) != "\x89PNG" {
		t.Errorf("产物不是 PNG（魔数不对），前 8 字节 %q", b[:min(8, len(b))])
	}
}

// TestShotPNGCapturesTheCrashScreen 钉住 --png 挂在**每一条**有屏幕内容的
// 返回路径上，而不是只挂在成功那条。
//
// 崩溃那一屏恰恰是最想看的一屏，而"给了 --png 却没文件、退出码还是被测程序
// 自己的"正是这个工具修过四次的失败形态。超时那条同理。
func TestShotPNGCapturesTheCrashScreen(t *testing.T) {
	requireTmux(t)
	requireBash(t)
	requireFonts(t)

	t.Run("崩溃屏", func(t *testing.T) {
		const name = "gotest-shot-pngcrash"
		startShotSession(t, name, 60, 10, "echo BOOM; exit 9")
		waitForMarker(t, name)

		out := filepath.Join(t.TempDir(), "crash.png")
		var rc int
		captureOut(t, func() {
			rc = shot(name, "", 5*time.Second, false, out)
		})
		if rc != 9 {
			t.Errorf("崩溃屏带 --png 应返回 9，得到 %d", rc)
		}
		if st, err := os.Stat(out); err != nil || st.Size() == 0 {
			t.Errorf("崩溃那一屏没被渲染出来：err=%v", err)
		}
	})

	t.Run("超时屏", func(t *testing.T) {
		const name = "gotest-shot-pngto"
		startShotSession(t, name, 60, 10, "echo NOPE; sleep 100")
		waitForScreen(t, name, func(s string) bool { return strings.Contains(s, "NOPE") })

		out := filepath.Join(t.TempDir(), "timeout.png")
		var rc int
		captureOut(t, func() {
			rc = shot(name, "NEVER_APPEARS", 500*time.Millisecond, false, out)
		})
		if rc != timeoutRC {
			t.Errorf("超时屏带 --png 应返回 %d，得到 %d", timeoutRC, rc)
		}
		if st, err := os.Stat(out); err != nil || st.Size() == 0 {
			t.Errorf("超时那一屏没被渲染出来：err=%v", err)
		}
	})
}

// TestShotAnsiPassthrough 钉住 --ansi 与退出标记识别之间的关系。
//
// 带 --ansi 时 stdout 上要有转义序列（那是这个开关的全部内容），而退出标记
// 的识别**不受影响** —— 标记一律在去掉 SGR 之后的文本上找。不这么做的话，
// 一段恰好落在标记中间的 SGR 就能让崩溃被报成成功，与 capture 少了 -J
// 是同款病灶，只是换了个劈断它的东西。
func TestShotAnsiPassthrough(t *testing.T) {
	requireTmux(t)
	requireBash(t)

	const name = "gotest-shot-ansi"
	startShotSession(t, name, 60, 10, `printf "\033[31mRED\033[0m\n"; exit 11`)
	waitForMarker(t, name)

	var rc int
	stdout, _ := captureOut(t, func() {
		rc = shot(name, "", 5*time.Second, false, "")
	})
	if rc != 11 {
		t.Errorf("应返回 11，得到 %d", rc)
	}
	if strings.ContainsRune(stdout, 0x1b) {
		t.Errorf("不带 --ansi 时 stdout 不该有转义序列：%q", stdout)
	}

	var rcAnsi int
	stdoutAnsi, _ := captureOut(t, func() {
		rcAnsi = shot(name, "", 5*time.Second, true, "")
	})
	if rcAnsi != 11 {
		t.Errorf("--ansi 不该改变退出码，得到 %d", rcAnsi)
	}
	if !strings.ContainsRune(stdoutAnsi, 0x1b) {
		t.Errorf("--ansi 时 stdout 应带转义序列：%q", stdoutAnsi)
	}
}

// TestShotF_HistoricalTimingShape 跑 Python 前身用例 (f) 的原始构造，
// 并把观察到的结果**打印**出来而不是断言它。
//
// 存在的理由是记录一次实测，不是当门禁：那个构造靠赌一个约 0.1s 的窗口，
// 两个方向都不牢（见 TestShotF_DeadCheckBeatsDeadline 的注释）。断言它
// 会得到一个在负载高时假红、在变异下又未必真红的用例 —— 两种坏处都占。
//
// "未必真红"是量出来的，不是推测：把死亡检查降位的两种重排各跑一遍，
// 这个用例**两次都绿**。它赌的那个窗口在本机根本没赌中过（见下面 t.Logf
// 打出的耗时：标记总是在 deadline 之前就被看见）。
//
// 这里只断言一件确定成立的事：无论落在窗口的哪一侧，返回的都必须是
// 3 或 timeoutRC 之一，绝不能是 0（那就是崩溃被报成成功了）。
func TestShotF_HistoricalTimingShape(t *testing.T) {
	requireTmux(t)
	requireBash(t)
	if testing.Short() {
		t.Skip("-short：跳过这个纯记录用途的 1s 用例")
	}

	const name = "gotest-shot-f-hist"
	startShotSession(t, name, 60, 10, "sleep 1.0; exit 3")

	var rc int
	start := time.Now()
	captureOut(t, func() {
		rc = shot(name, "NEVER_APPEARS", 1050*time.Millisecond, false, "")
	})
	t.Logf("原始 (f) 构造：rc=%d，耗时 %v（3 = 落在窗口内，%d = 标记晚于 deadline）",
		rc, time.Since(start), timeoutRC)

	if rc != 3 && rc != timeoutRC {
		t.Errorf("得到 %d，只可能是 3 或 %d", rc, timeoutRC)
	}
}
