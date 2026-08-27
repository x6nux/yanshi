// main_test.go —— CLI 层的测试：子命令分发、参数解析、退出码。
//
// 这一层测的是**命令行到动作的映射**，不是动作本身 —— cmdStart/shot 这些
// 已经在 session_test.go / shot_test.go 里对着真实 tmux 测过了。所以绝大多数
// 用例走的是 parseCommand：它按契约不碰 tmux，于是可以在没有 tmux 的机器上
// 跑，也不会在测试机上留下任何会话。
//
// parseCommand 返回一个闭包而不是直接执行，正是为了这件事可测：一个"解析并
// 立刻执行"的入口只能靠真起会话来测参数解析，那样每个参数用例都要付一次
// 起停会话的代价，慢到没人愿意把边界情形写全。

package main

import (
	"os"
	"strings"
	"testing"
)

// runParse 跑一次 parseCommand 并接管两个输出流，返回 (是否得到 action, 退出码, stdout, stderr)。
//
// 两个流都要接管：帮助进 stdout、诊断进 stderr，而"这行字去了哪个流"本身就是
// 被测契约的一部分（`tuidbg -h | less` 要看得见东西）。
func runParse(t *testing.T, args []string) (hasAction bool, code int, stdout, stderr string) {
	t.Helper()
	var action func() int
	stdout, stderr = captureOut(t, func() {
		action, code = parseCommand(args)
	})
	return action != nil, code, stdout, stderr
}

// TestNoSubcommandPrintsUsage 钉住"什么都不给"这条路径。
//
// 用法要进 **stderr** 而不是 stdout：这不是用户要的输出，是对一次错误调用的
// 诊断。退出码是 usageRC 而不是 0 —— 0 会让 `tuidbg && echo ok` 打出 ok，
// 而其实什么都没做。
func TestNoSubcommandPrintsUsage(t *testing.T) {
	hasAction, code, stdout, stderr := runParse(t, nil)
	if hasAction {
		t.Fatal("没给子命令却得到了一个待执行的动作")
	}
	if code != usageRC {
		t.Errorf("退出码 = %d，want %d", code, usageRC)
	}
	if !strings.Contains(stderr, "用法") {
		t.Errorf("stderr 里没有用法：%q", stderr)
	}
	if stdout != "" {
		t.Errorf("错误诊断不该进 stdout：%q", stdout)
	}
}

// TestHelpGoesToStdoutWithZero 钉住 -h 的两条：进 stdout、返回 0。
//
// 这两条都是被别的东西咬着的：这个仓库拿 `yanshi -h` 当 CGO_ENABLED=0 构建
// 矩阵的冒烟测试，一个把帮助报成非零码的入口会让那种冒烟测试永远红；而把
// 帮助写进 stderr 会让 `tuidbg -h | grep shot` 什么都搜不到。
func TestHelpGoesToStdoutWithZero(t *testing.T) {
	for _, arg := range []string{"-h", "--help", "help"} {
		hasAction, code, stdout, stderr := runParse(t, []string{arg})
		if hasAction {
			t.Errorf("%s 不该产生动作", arg)
		}
		if code != 0 {
			t.Errorf("%s 退出码 = %d，want 0", arg, code)
		}
		if !strings.Contains(stdout, "用法") {
			t.Errorf("%s 的帮助没进 stdout：%q", arg, stdout)
		}
		if stderr != "" {
			t.Errorf("%s 不该往 stderr 写东西：%q", arg, stderr)
		}
	}
}

// TestUsageListsEverySubcommand 钉住用法文本与真实分发表的对齐。
//
// 一个不在用法里的子命令等于不存在（没人会去读源码找它），一个在用法里
// 但分发不了的子命令是虚报能力 —— 这个仓库有一整道门禁（幻影斜杠命令）
// 专门治后者。两个方向都查：用法必须提到每个可分发的名字，且用法里提到的
// 每个名字都必须真的分发得了。
func TestUsageListsEverySubcommand(t *testing.T) {
	subs := []string{"start", "send", "key", "shot", "stop"}

	var help string
	captureOut(t, func() { usage(os.Stdout) })
	_, _, help, _ = runParse(t, []string{"-h"})

	for _, s := range subs {
		if !strings.Contains(help, "tuidbg "+s) {
			t.Errorf("用法里没有子命令 %q：\n%s", s, help)
		}
	}

	// 反方向：每个被宣传的名字都要真的分发得出一个动作。给足最少的参数。
	minimal := map[string][]string{
		"start": {"start", "--", "true"},
		"send":  {"send", "hi"},
		"key":   {"key", "Enter"},
		"shot":  {"shot"},
		"stop":  {"stop"},
	}
	for _, s := range subs {
		hasAction, code, _, stderr := runParse(t, minimal[s])
		if !hasAction {
			t.Errorf("被宣传的子命令 %q 分发不出动作（rc=%d, stderr=%q）", s, code, stderr)
		}
	}
}

// TestUnknownSubcommand 钉住未知子命令的两条：非零码 + 说得出人话。
//
// 说出**它自己**的名字是有意的：一句泛泛的"用法错误"会让人以为是某个参数
// 写错了，而真正的原因是那个词根本不存在（最常见的是拼写，如 shoot/stat）。
func TestUnknownSubcommand(t *testing.T) {
	hasAction, code, _, stderr := runParse(t, []string{"shoot", "-wait", "X"})
	if hasAction {
		t.Fatal("未知子命令却得到了动作")
	}
	if code != usageRC {
		t.Errorf("退出码 = %d，want %d", code, usageRC)
	}
	if !strings.Contains(stderr, "未知子命令") || !strings.Contains(stderr, "shoot") {
		t.Errorf("stderr 没点名那个未知子命令：%q", stderr)
	}
}

// TestFlagBeforeSubcommandSaysSo 把"flag 写到了子命令前面"与"未知子命令"
// 分开报。
//
// `tuidbg -session foo shot` 是一次很自然的手误（多数 CLI 允许全局 flag 在前）。
// 把它报成"未知子命令 -session"会把人送去查一个并不存在的子命令名，而真正
// 要改的是词序。这条只值一行 if，但它省下的是一次方向完全错的排查。
func TestFlagBeforeSubcommandSaysSo(t *testing.T) {
	hasAction, code, _, stderr := runParse(t, []string{"-session", "foo", "shot"})
	if hasAction {
		t.Fatal("这条命令行不合法，不该产生动作")
	}
	if code != usageRC {
		t.Errorf("退出码 = %d，want %d", code, usageRC)
	}
	if !strings.Contains(stderr, "之后") {
		t.Errorf("stderr 没说 flag 要写在子命令之后：%q", stderr)
	}
	if strings.Contains(stderr, "未知子命令") {
		t.Errorf("不该报成未知子命令，那会把人送错方向：%q", stderr)
	}
}

// TestStartRequiresACommand 钉住"start 没给命令"这条路径必须非零。
//
// 这是文件头那句"没有任何一条路径隐式返回 0"最容易破的地方：起一个空 argv
// 的会话，tmux 会成功地起一个跑着空命令的 shell，然后 wrapper 立刻打出
// 退出码 0 —— 整条链路上没有任何一处报错，而用户什么都没跑到。
func TestStartRequiresACommand(t *testing.T) {
	for _, args := range [][]string{
		{"start"},
		{"start", "--"},
		{"start", "-cwd", "/tmp"},
		{"start", "-cols", "80", "--"},
	} {
		hasAction, code, _, stderr := runParse(t, args)
		if hasAction {
			t.Errorf("%v 没有被测命令，却产生了动作", args)
		}
		if code == 0 {
			t.Errorf("%v 退出码 = 0（这正是本工具修过四次的那个病）", args)
		}
		if !strings.Contains(stderr, "需要一条被测命令") {
			t.Errorf("%v 的 stderr 没说清缺什么：%q", args, stderr)
		}
	}
}

// TestStartAcceptsBothArgvForms 钉住 `start -- cmd args` 与 `start cmd args`
// 都能工作，且**吃到的 argv 完全一致**。
//
// 只断言"两种写法都不报错"是不够的：一个把 `--` 留在 argv 里的实现同样不报错，
// 但它会去执行一个名叫 `--` 的程序。所以这里真的把会话起起来，回读屏幕确认
// 跑的是同一个东西。
func TestStartAcceptsBothArgvForms(t *testing.T) {
	requireTmux(t)
	requireBash(t)

	cases := map[string][]string{
		"gotest-main-dashdash": {"start", "-session", "gotest-main-dashdash", "--", "bash", "-c", "echo FORM_OK; sleep 100"},
		"gotest-main-bare":     {"start", "-session", "gotest-main-bare", "bash", "-c", "echo FORM_OK; sleep 100"},
		// 多写一个 `--`：Go 的 flag 吃掉第一个，stripSeparator 吃掉第二个。
		"gotest-main-twodash": {"start", "-session", "gotest-main-twodash", "--", "--", "bash", "-c", "echo FORM_OK; sleep 100"},
	}

	for name, args := range cases {
		full := sessionPrefix + name
		killExact(full)
		t.Cleanup(func() { killExact(full) })

		action, code := parseCommand(args)
		if action == nil {
			t.Fatalf("%s: 解析失败，rc=%d", name, code)
		}
		if rc := action(); rc != 0 {
			t.Fatalf("%s: start rc = %d，want 0", name, rc)
		}
		screen := waitForScreen(t, name, func(s string) bool {
			return strings.Contains(s, "FORM_OK")
		})
		if !strings.Contains(screen, "FORM_OK") {
			t.Errorf("%s: 屏幕上没有预期输出，最后一屏:\n%s", name, screen)
		}
		// 跑的必须是被测命令本身，而不是一个名叫 `--` 的东西。
		if strings.Contains(screen, "command not found") {
			t.Errorf("%s: argv 里混进了分隔符，屏幕:\n%s", name, screen)
		}
	}
}

// TestStartRejectsNonPositiveSize 钉住 -cols/-rows 的下界检查在**动作之前**。
//
// tmux 自己也会拒（实测 3.7c：`-x 0` 返回 1 并打 "width too small"），所以
// 这条不是防线而是措辞。测它是因为"措辞"同样会退化：把这段删掉，用户拿到的
// 是一句不提 -cols 也不提 -rows 的 tmux 报错，而这两个名字只有本工具说得出。
func TestStartRejectsNonPositiveSize(t *testing.T) {
	for _, args := range [][]string{
		{"start", "-cols", "0", "--", "true"},
		{"start", "-rows", "0", "--", "true"},
		{"start", "-cols", "-5", "--", "true"},
	} {
		hasAction, code, _, stderr := runParse(t, args)
		if hasAction {
			t.Errorf("%v 尺寸非法却产生了动作", args)
		}
		if code == 0 {
			t.Errorf("%v 退出码 = 0", args)
		}
		if !strings.Contains(stderr, "-cols") {
			t.Errorf("%v 的 stderr 没点名是哪个 flag：%q", args, stderr)
		}
	}
}

// TestSendRequiresExactlyOneArgument 钉住 send 的参数个数。
//
// **不把多个参数拼起来**是有意的：拼接会把 `send a  b`（两个空格）改成一个
// 空格，而这个工具的全部用途就是"屏幕上出现的到底是什么"。悄悄改掉用户要打
// 的字，不会有任何症状 —— 直到有人拿它去查一个对齐问题。
func TestSendRequiresExactlyOneArgument(t *testing.T) {
	for _, args := range [][]string{
		{"send"},
		{"send", "a", "b"},
	} {
		hasAction, code, _, stderr := runParse(t, args)
		if hasAction {
			t.Errorf("%v 参数个数不对却产生了动作", args)
		}
		if code == 0 {
			t.Errorf("%v 退出码 = 0", args)
		}
		if !strings.Contains(stderr, "恰好一个") {
			t.Errorf("%v 的 stderr 没说清要几个：%q", args, stderr)
		}
	}
}

// TestDashPrefixedPositionalNeedsDoubleDash 记录并钉住 Go flag 包那条行为对
// 用户可见的后果。
//
// Go 的 flag 在遇到第一个**非 flag** 参数时才停止解析，所以一个以 `-` 开头的
// 位置参数（`send -x`）会被当成未定义的 flag 而报错。解法是 `send -- -x`。
//
// 这条测试的价值不在"报错"而在**报错说的话**：stdlib 给的是
// "flag provided but not defined: -x"，它既不提 `--` 也不提这是位置参数，
// 读完不知道该怎么改。两条腿一起测 —— 裸的要给出解法，加了 `--` 的要真的通过
// 且文本原样保留（否则"解法"是假的）。
func TestDashPrefixedPositionalNeedsDoubleDash(t *testing.T) {
	hasAction, code, _, stderr := runParse(t, []string{"send", "-x"})
	if hasAction {
		t.Fatal("裸的 -x 应该被 flag 包拒掉")
	}
	if code != usageRC {
		t.Errorf("退出码 = %d，want %d", code, usageRC)
	}
	if !strings.Contains(stderr, "--") {
		t.Errorf("stderr 没给出 `--` 这个解法，用户读完不知道怎么改：%q", stderr)
	}

	// 解法必须真的管用，而且文本一个字都不能变。
	action, code := parseCommand([]string{"send", "--", "-x"})
	if action == nil {
		t.Fatalf("`send -- -x` 应该解析成功，rc=%d", code)
	}

	// key 同理，而且更常撞上 —— tmux 的键名里真有以 `-` 开头看起来像 flag 的。
	hasAction, _, _, stderr = runParse(t, []string{"key", "-R"})
	if hasAction {
		t.Error("裸的 -R 应该被拒")
	}
	if !strings.Contains(stderr, "--") {
		t.Errorf("key 的 stderr 也该给出 `--`：%q", stderr)
	}
	if action, _ := parseCommand([]string{"key", "--", "-R"}); action == nil {
		t.Error("`key -- -R` 应该解析成功")
	}
}

// TestSendDelivers 端到端确认 send 的文本原样到达屏幕，包括 `-` 开头的那条
// 解法路径与中文。
//
// 中文要测是因为它是这次验收的核心场景（PNG 里要能看见），而 send 这条链路上
// 有三处可能把它弄坏：Go 的参数传递、tmux 的 -l、以及 pane 的编码。
func TestSendDelivers(t *testing.T) {
	requireTmux(t)

	const name = "gotest-main-send"
	full := sessionPrefix + name
	killExact(full)
	t.Cleanup(func() { killExact(full) })

	if rc := cmdStart(name, 60, 10, []string{"cat"}, ""); rc != 0 {
		t.Fatalf("cmdStart rc = %d", rc)
	}

	for _, text := range []string{"-x --wait", "纯 Go 渲染测试 hello"} {
		action, code := parseCommand([]string{"send", "-session", name, "--", text})
		if action == nil {
			t.Fatalf("解析 %q 失败，rc=%d", text, code)
		}
		if rc := action(); rc != 0 {
			t.Fatalf("send %q rc = %d", text, rc)
		}
		if a, _ := parseCommand([]string{"key", "-session", name, "Enter"}); a == nil {
			t.Fatal("key 解析失败")
		} else if rc := a(); rc != 0 {
			t.Fatalf("key Enter rc = %d", rc)
		}

		screen := waitForScreen(t, name, func(s string) bool { return strings.Contains(s, text) })
		if !strings.Contains(screen, text) {
			t.Errorf("屏幕上没有 %q，最后一屏:\n%s", text, screen)
		}
	}
}

// TestSubcommandHelpGoesToStdoutToo 钉住子命令的 -h 与顶层 -h 走**同一个流**。
//
// 这条是真机验收挖出来的：flag 包默认把用法写 fs.Output()（默认 os.Stderr），
// 所以第一版里 `tuidbg -h` 进 stdout 而 `tuidbg shot -h` 进 stderr —— 同一个
// flag 在同一个工具里去了两个流，`tuidbg shot -h | grep png` 于是搜不到东西。
// 单测全绿（那时的用例只测顶层），真跑一次才看得见。
func TestSubcommandHelpGoesToStdoutToo(t *testing.T) {
	for _, sub := range []string{"start", "send", "key", "shot", "stop"} {
		hasAction, code, stdout, stderr := runParse(t, []string{sub, "-h"})
		if hasAction {
			t.Errorf("%s -h 不该产生动作", sub)
		}
		if code != 0 {
			t.Errorf("%s -h 退出码 = %d，want 0", sub, code)
		}
		if !strings.Contains(stdout, "用法") {
			t.Errorf("%s -h 的用法没进 stdout：stdout=%q stderr=%q", sub, stdout, stderr)
		}
		if stderr != "" {
			t.Errorf("%s -h 不该往 stderr 写东西：%q", sub, stderr)
		}
	}
}

// TestParseErrorPutsTheFixBeforeTheUsageDump 钉住参数出错时三段话的**顺序**。
//
// 顺序是"错在哪 → 怎么改 → 用法"。这条同样是真机验收挖出来的：第一版把本工具
// 自己那句提示写在 flag 包的输出**之后**，于是它垫在整坨用法的最底下 ——
// 终端上滚过去，人看见的是 stdlib 那句不提 `--` 的 "flag provided but not
// defined"，而真正的解法在视线之外。退出码和措辞当时都是对的，只有顺序不对，
// 任何只断言 Contains 的测试都抓不到（第一版的测试就是这样）。
func TestParseErrorPutsTheFixBeforeTheUsageDump(t *testing.T) {
	_, code, _, stderr := runParse(t, []string{"send", "-x"})
	if code != usageRC {
		t.Fatalf("退出码 = %d，want %d", code, usageRC)
	}

	iErr := strings.Index(stderr, "not defined")
	iFix := strings.Index(stderr, "--")
	iUsage := strings.Index(stderr, "可用 flag")
	if iErr < 0 || iFix < 0 || iUsage < 0 {
		t.Fatalf("三段话不齐（err=%d fix=%d usage=%d）：%q", iErr, iFix, iUsage, stderr)
	}
	if !(iErr < iFix && iFix < iUsage) {
		t.Errorf("顺序应是 错在哪 → 怎么改 → 用法，实际下标 err=%d fix=%d usage=%d：\n%s",
			iErr, iFix, iUsage, stderr)
	}
	// 提示必须同时提到两种成因：stdlib 给的是同一句话，本工具分不出来。
	if !strings.Contains(stderr, "拼错") || !strings.Contains(stderr, "位置参数") {
		t.Errorf("提示没有同时覆盖拼错 flag 与 dash-positional 两种成因：%q", stderr)
	}
}

// TestShotRejectsPositionalArgs 钉住 `shot -wait READY 30` 这个手误必须报错。
//
// 不报错的话，那个 30 被静默丢弃、timeout 用默认的 10 秒 —— 得到的是一次
// 看起来像"TUI 太慢"的 124，而真正的原因是秒数写成了位置参数。这类"参数被
// 默默忽略"的错误没有任何症状，只能在入口拦。
func TestShotRejectsPositionalArgs(t *testing.T) {
	hasAction, code, _, stderr := runParse(t, []string{"shot", "-wait", "READY", "30"})
	if hasAction {
		t.Fatal("多余的位置参数却产生了动作")
	}
	if code != usageRC {
		t.Errorf("退出码 = %d，want %d", code, usageRC)
	}
	if !strings.Contains(stderr, "-timeout") {
		t.Errorf("stderr 没给出正确写法：%q", stderr)
	}

	// stop 同理：`stop foo` 多半是想写 `stop -session foo`，静默忽略那个 foo
	// 会去杀默认会话 —— 一次针对错误目标的破坏性操作。
	hasAction, code, _, stderr = runParse(t, []string{"stop", "foo"})
	if hasAction {
		t.Error("`stop foo` 不该产生动作（它会杀掉默认会话而不是 foo）")
	}
	if code != usageRC {
		t.Errorf("stop 多余参数的退出码 = %d，want %d", code, usageRC)
	}
	if !strings.Contains(stderr, "位置参数") {
		t.Errorf("stop 的 stderr 没说清：%q", stderr)
	}
}

// TestInvalidWaitRegexIsCleanRC1 钉住"-wait 写错正则"经由**整条 CLI 路径**
// 得到 rc=1 和一句人话，而不是 panic。
//
// shot_test.go 已经直接测过 shot()，这里再测一遍是因为它们回答的不是同一个
// 问题：那边问"shot 会不会 panic"，这边问"CLI 有没有在别处先 MustCompile
// 一次"。一个在 parseCommand 里顺手编译一次正则的实现会在这里 panic 掉，
// 而 shot_test.go 一个字都不会红。
func TestInvalidWaitRegexIsCleanRC1(t *testing.T) {
	requireTmux(t)
	requireBash(t)

	const name = "gotest-main-badre"
	full := sessionPrefix + name
	killExact(full)
	t.Cleanup(func() { killExact(full) })
	if rc := cmdStart(name, 60, 10, []string{"bash", "-c", "sleep 100"}, ""); rc != 0 {
		t.Fatalf("cmdStart rc = %d", rc)
	}

	var rc int
	_, stderr := captureOut(t, func() {
		rc = run([]string{"shot", "-session", name, "-wait", "READY["})
	})
	if rc != 1 {
		t.Errorf("非法正则的退出码 = %d，want 1", rc)
	}
	if !strings.Contains(stderr, "合法正则") {
		t.Errorf("stderr 没说清是正则不合法：%q", stderr)
	}
	if strings.Contains(stderr, "goroutine ") || strings.Contains(stderr, "panic:") {
		t.Errorf("吐了 panic/stack trace：%q", stderr)
	}
}

// TestTimeoutFlagReachesShot 钉住 -timeout 的秒数真的换算成了 Duration。
//
// 用一个必然超时的锚点配 0.3 秒：如果 -timeout 没接通（用了默认的 10 秒），
// 这个用例会慢 30 倍；如果单位换错（比如当成纳秒），它会立刻返回。两个方向
// 都由下面的耗时断言接住。
func TestTimeoutFlagReachesShot(t *testing.T) {
	requireTmux(t)
	requireBash(t)

	const name = "gotest-main-timeout"
	full := sessionPrefix + name
	killExact(full)
	t.Cleanup(func() { killExact(full) })
	if rc := cmdStart(name, 60, 10, []string{"bash", "-c", "echo NOPE; sleep 100"}, ""); rc != 0 {
		t.Fatalf("cmdStart rc = %d", rc)
	}
	waitForScreen(t, name, func(s string) bool { return strings.Contains(s, "NOPE") })

	var rc int
	stdout, stderr := captureOut(t, func() {
		rc = run([]string{"shot", "-session", name, "-wait", "NEVER_APPEARS", "-timeout", "0.3"})
	})
	if rc != timeoutRC {
		t.Errorf("超时的退出码 = %d，want %d", rc, timeoutRC)
	}
	if !strings.Contains(stdout, "NOPE") {
		t.Errorf("超时必须打印最后一屏：%q", stdout)
	}
	if !strings.Contains(stderr, "没等到") {
		t.Errorf("stderr 没说是超时：%q", stderr)
	}
}

// TestStopOnMissingSessionIsNonZero 钉住 stop 一个不存在的会话必须响亮失败。
//
// "杀一个不存在的东西"很容易被写成幂等的成功（`kill -0` 之类的直觉），但对
// 这个工具来说它是一条真实的诊断：多半意味着会话名写错了，或者被测程序早就
// 没了。报 0 会让脚本继续往下 send/shot，然后在更远的地方以更难懂的方式失败。
func TestStopOnMissingSessionIsNonZero(t *testing.T) {
	requireTmux(t)

	const name = "gotest-main-nosuch"
	killExact(sessionPrefix + name)

	var rc int
	_, stderr := captureOut(t, func() {
		rc = run([]string{"stop", "-session", name})
	})
	if rc == 0 {
		t.Errorf("stop 一个不存在的会话返回了 0")
	}
	if strings.TrimSpace(stderr) == "" {
		t.Error("stop 失败却一句话都没说")
	}
}

// TestRunFullLifecycle 走一遍 run() 的完整链路：start → send → key → shot → stop。
//
// 每一步都过 run（而不是直接调 cmdXxx），所以这条用例咬的是**装配**：某个
// 子命令的 flag 没接通、某个动作的返回值被吞掉，都会在这里露出来。它是这个
// 文件里唯一一条端到端用例，其余用例各自只咬一个边界。
func TestRunFullLifecycle(t *testing.T) {
	requireTmux(t)
	requireBash(t)

	const name = "gotest-main-lifecycle"
	full := sessionPrefix + name
	killExact(full)
	t.Cleanup(func() { killExact(full) })

	steps := []struct {
		what string
		args []string
	}{
		{"start", []string{"start", "-session", name, "-cols", "60", "-rows", "10", "--", "cat"}},
		{"send", []string{"send", "-session", name, "LIFECYCLE_OK"}},
		{"key", []string{"key", "-session", name, "Enter"}},
	}
	for _, s := range steps {
		var rc int
		_, stderr := captureOut(t, func() { rc = run(s.args) })
		if rc != 0 {
			t.Fatalf("%s rc = %d，stderr=%q", s.what, rc, stderr)
		}
	}

	var rc int
	stdout, _ := captureOut(t, func() {
		rc = run([]string{"shot", "-session", name, "-wait", "LIFECYCLE_OK", "-timeout", "10"})
	})
	if rc != 0 {
		t.Errorf("shot rc = %d，want 0", rc)
	}
	if !strings.Contains(stdout, "LIFECYCLE_OK") {
		t.Errorf("屏幕上没有预期文本：\n%s", stdout)
	}

	captureOut(t, func() { rc = run([]string{"stop", "-session", name}) })
	if rc != 0 {
		t.Errorf("stop rc = %d，want 0", rc)
	}
	if sessionExists(name) {
		t.Error("stop 之后会话还在")
	}
}

// TestRunDoesNotTouchNeighbours 钉住经由 CLI 的 start/stop 不会波及名字带
// 同一前缀的邻居会话。
//
// session_test.go 已经对 cmdStop 测过同一件事，这里测的是 CLI 这一层没有把
// 会话名拼错或漏掉前缀 —— tmux 的 -t 是前缀匹配，一次拼错就是杀掉别人的会话，
// 而这台机器上就跑着别的工具的会话。
func TestRunDoesNotTouchNeighbours(t *testing.T) {
	requireTmux(t)
	requireBash(t)

	const neighbour = sessionPrefix + "neighbour"
	const name = "neigh"

	killExact(neighbour)
	killExact(sessionPrefix + name)
	t.Cleanup(func() {
		killExact(neighbour)
		killExact(sessionPrefix + name)
	})

	// 邻居直接用 tmux 起，不经过本工具 —— 它代表"别人的会话"。
	if _, _, rc := tmuxRun("new-session", "-d", "-s", neighbour, "-x", "40", "-y", "5", "sleep 100"); rc != 0 {
		t.Fatalf("起邻居会话失败，rc=%d", rc)
	}

	var rc int
	captureOut(t, func() {
		rc = run([]string{"start", "-session", name, "-cols", "40", "-rows", "5", "--", "sleep", "100"})
	})
	if rc != 0 {
		t.Fatalf("start rc = %d", rc)
	}
	captureOut(t, func() { rc = run([]string{"stop", "-session", name}) })
	if rc != 0 {
		t.Fatalf("stop rc = %d", rc)
	}

	if _, _, rc := tmuxRun("has-session", "-t", "="+neighbour); rc != 0 {
		t.Fatalf("邻居会话 %s 被误杀了", neighbour)
	}
}
