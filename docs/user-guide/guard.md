# 安全守卫（Guard）

yanshi 的安全核心是 guard：一个**无状态、fail-closed**的六维权限检查器（`internal/guard::Guard.Check`）。每次工具调用前，guard 按 destructive → mcp → tools → fs → shell → net 依次检查，**第一个不满足的维度即短路拒绝**。

## 六个维度

顺序有意义：拒绝理由来自**第一个**说不的维度，后面的维度根本不跑。

| 顺序 | 维度 | 检查什么 |
|---|---|---|
| 1 | **destructive** | 破坏性删除。**与 profile 无关**，最先跑，再宽松的 profile 也绕不过（见下）。 |
| 2 | **mcp** | 动态发现的 `mcp_<server>_<tool>` 的独立白名单。**空 `allow` 拒绝一切 MCP 工具，哪怕 `tools.allow` 是 `["*"]`**。 |
| 3 | **tools** | 工具名 glob 白名单。**空 `allow` 一律拒绝**（见下）。 |
| 4 | **fs** | 读 / 写路径的 glob 白名单。 |
| 5 | **shell** | 拆段（拆不出就拒）→ 每段：重定向目标进 fs 判定 + `rules`（execpolicy）或 `policy` + `patterns`（见下）→ 取最严的一段。 |
| 6 | **net** | 是否允许出站 + host 白名单。 |

> **mcp 排在 tools 前面不是笔误。** 它是独立的一道门，所以 `tools: { allow: ["*"] }` **放不出任何 MCP 工具** —— `config.example.yaml` 的 `orchestrator` profile 里那句 `mcp: { allow: [] }` 就是在显式关掉它们。要用 MCP 工具，必须在 `mcp.allow` 里按 server/tool 逐个（或按 glob，如 `"mcp_github_*"`）放行。

## shell 的三层与四个合法 policy

shell 维度先拆段（见下一节），再对**每一段**过下面两层，任一层给出结论就短路；整条取最严的一段：

1. **拆段** —— 拆不出来就无条件、不可覆盖地拒绝。见下一节。
2. **`rules`（execpolicy 结构化规则表）** —— 非空时**完全接管**，`policy`/`patterns` 根本不会被读到。
3. **`policy` + `patterns`（legacy glob 开关）** —— `rules` 为空时才生效。

`policy` 只有四个合法值，**没有 `allow`**：

| policy | 含义 |
|---|---|
| `""`（省略） | 等同 `allowlist`。 |
| `allowlist` | 命中 `patterns` 之一 → 放行；否则 **Prompt**（交互式可批准）。 |
| `denylist` | 命中 `patterns` 之一 → 拒绝（**可覆盖**）；否则放行。**"不限制 shell" 用空 patterns 的 `denylist` 表达。** |
| `deny` | 一律拒绝（**可覆盖**）。 |

> ⚠️ **`rules` 为空时写错 policy 会让 shell 永久锁死。** 未知值落进 `checkShellPolicy` 的 default 分支，返回的是**结构性 HardDeny** —— `yolo` 和 `auto` 都越不过去（见下方"两档 HardDeny"）。因此配置加载时就会校验：`profiles.<名>.shell.policy` 不在上表里，`config.Load` 直接报错退出，不会让它拖到第一次 `shell_run` 才炸。
>
> **`rules` 非空时这道校验不跑**，和上面第 2 层说的一致：`rules` 完全接管，`policy` 根本不会被读到，写错的值是**惰性**的，不影响这个 profile 今天的任何行为。校验只在那个值真的能拒绝东西时才拦，不会因为一个 guard 从不读的字段拒绝一次启动。清空 `rules` 后该值变成活的，下一次加载就会被拒。范围与理由见 `internal/config::Config.validateProfiles` 的 doc 注释。

## 两档 HardDeny：结构性 vs 可覆盖

拒绝分两档（`Decision.Overridable`）：

- **结构性 HardDeny**（`Overridable=false`）—— **任何模式都越不过**：shell 结构读不出来、execpolicy parse-error、未知 shell policy、灾难性批量删除、命令嵌套层数超出 guard 的解包预算。
- **可覆盖 HardDeny**（`Overridable=true`）—— profile 能说"不"的一切：空的 tools/fs allowlist、空的 mcp allowlist、`shell.policy: "deny"`、denylist 命中、execpolicy `hard_deny` 规则、`net.allow: false`。`yolo` 直接越过，`auto` 交给 AI 判断。

> **本页这份枚举比 `CLAUDE.md` 的同名枚举短一项，两边都对**（两边的当前条数都别从这里读，`CLAUDE.md` 那份自带现场清点命令）。少的那一项是源码里 `checkShellPolicy` 的另一个结构性分支（`switch result.Verdict` 的 `default`），它是**防御性的、从任何配置都到不了**：`execpolicy.Evaluate` 的出口集合是 `allow` / `prompt` / `hard_deny`，三个都被前面的 `case` 接住了。规则里把 `decision` 写错（比如 `decision: warn`）不会走到那里 —— `Evaluate` 自己先把它转成 `hard_deny`，落进 `case "hard_deny", "deny":`，那是**可覆盖**的一档，`yolo` 能越过。本页不列它，`CLAUDE.md` 那份枚举面向改 guard 源码的人，把源码分支也数进去。这个"出口集合到不了 default"由 `internal/guard::TestExecPolicyVerdictsAreHandledByCheckShell` 钉住。
>
> ⚠️ **别把这个差别读成「不可达就不数」——那个判据不成立。** 上面第 3 条「未知 shell policy」同样从 yaml 走不到：`rules` 为空时 `internal/config::Config.validateProfiles` 让 `config.Load` 当场拒绝加载，`rules` 非空时 `checkShellPolicy` 在 execpolicy 分支里就 return 了、policy switch 根本不可达（那个函数的 doc 注释写了为什么两条都不留活口）。按「不可达就不数」这条也该删，本页就只剩 3 条。**在两个都不可达的分支之间**，判据是**有没有对应的配置面**：`shell.policy` 是操作者会亲手写进 yaml 的键，所以即使当前不可达也留在本页；execpolicy 的 verdict 词表不是任何配置字段，写规则的人碰不到它，所以只留在 `CLAUDE.md`。

> 这个判据**只在裁决不可达分支时用，不要提升成全页判据**：本页除「未知 shell policy」之外的每一条都不对应任何 yaml 键（shell 结构读不出来、execpolicy parse-error、灾难性批量删除、命令嵌套超预算……），它们由**命令内容**触发而不是由配置打开 —— 照配置面去筛会把本页**砍到只剩「未知 shell policy」那一条**。本页的入选标准始终是「操作者需要知道它拦得住什么」，配置面只是不可达分支的补充裁决。
>
> （这句话原先写的是「从 4 条砍成 1 条」，而上面那份清单早已不是 4 条 —— 一个裸计数在描述**另一行的当前内容**，改那一行的人不会回来改这里。写「砍到只剩哪一条」就没有这个问题：差集是稳定的，条数不是。）

## 破坏性删除门（profile 无关）

`rm -rf` 打到 `/`、`~`、`$HOME`、`*`、`/etc`、`/usr`、`/home`、`C:\`、工作目录自身或其祖先 = **Catastrophic**，结构性 HardDeny，**所有模式都拦**（`yolo` 也拦）。删除工作目录之外的路径 = **OutOfScope**，升级为 Prompt。工作目录**内部**的 `rm -rf build/` 不受此门限制，仍由 shell 维度决定。

程序词的**拼法**一律看穿而不是拒绝：`r\m`（反斜杠转义）、`FOO=1 rm -rf /`（赋值前缀）、`{ rm -rf /; }` 与 `if …; then rm -rf /; fi`（复合命令与保留字）、`eval rm -rf /`、`$'\x72\x6d'`（ANSI-C）、以及 `sudo` / `nohup` / `timeout` 这类前缀执行器，都会被读回它真正要跑的那个程序。PowerShell 的 `Remove-Item` 及其别名同样在删除程序表里。

**嵌套有上限，而且用完了是拒绝不是放行。** wrapper（`bash -c "…"`）、`su -c`、`eval` 与前缀执行器共用同一个 8 层预算。八层之内照常读到底；超过之后，如果底下还藏着一条命令，判 **Unreadable** —— 同样是结构性 HardDeny，理由文案会明说是"嵌套太深、读不到真正要跑的程序"，而不是借用灾难性删除的说法。`sudo nohup timeout 5 nice -n 19 rm -rf /` 才用掉四层，正常写法碰不到这条。

### 读不懂的 payload 会弹窗（**Opaque**）

同一条原则的另一面：**"没读到 payload"不等于"读到了一个安全的 payload"**。当没有任何读法认领这条命令、而它仍然带着一段本工具读不懂的东西时，判 **Opaque** —— 一个 **Prompt**，不是结构性地板。

会落进这一档的两类：

- **另一种语言的程序**：一个本工具不认识的程序，带着 `-c` / `-e` / `--command` / `--eval` / `--execute` 中的一个，且它后面那个操作数长得像程序而不像选项值（含空格、括号、`$`、`;`、`|` 之类）。`python3 -c "…"`、`perl -e "…"`、`node --eval "…"`、`powershell -EncodedCommand <base64>` 都在这里。
- **wrapper 的兜底**：`bash +o posix -c "rm -rf /"` 用了一种本工具的 flag 扫描不认的 shell 选项拼法，于是没有任何读法拿到那个 `-c` 的内容 —— 这种"没人读过"的形态从静默放行变成弹窗。

**这是刻意的过严，代价是知情接受的。** 一些完全正常的命令会开始要你点一下：`psql -c "SELECT 1"`、`osascript -e '…'`、你自己写的带 `-c` 的小工具。方向是这样选的：判错成"多问一次"你会立刻看见并抱怨，判错成"静默放行"没有任何痕迹。

**不会落进这一档的**：`tail -c 100`、`cut -c 1-5`、`gcc -c foo.c`、`ssh -e none host`（操作数是选项值不是程序），以及 `grep -e "a b"`、`sed -e 's/a b/c/'`、`git -c core.pager="less -R" log`、`docker run -e "A=b c"`、`jq -e '.a|.b'`、`kubectl logs -c web`、`curl -d '{"a": 1}' url`（这些程序在一张"它的 `-c`/`-e` 操作数按文档就不是程序"的缓解表里）。**这张表漏一个条目的代价是多一次弹窗**，所以碰到你觉得不该弹的命令，那是缺表项而不是缺防线。

> ⚠️ **`rsync -e` 曾经在这张表里，那是错放。** 表项的理由写着「`-e` 是传输用的远端 shell」—— 而**传输用的远端 shell 就是一个程序，rsync 会去执行它**，`rsync -e 'sh -c "rm -rf /"' a h:b` 因此一直是 Allow。它已经从表里删掉，代价是 `rsync -e "ssh -p 22" src dst` 这种日常写法现在会弹一次窗。**这正是这张表的失败方向应该长的样子**：漏一条 = 多一次弹窗（你看得见），错放一条 = 静默放行（没有任何痕迹）。

### 位置操作数里的程序

上面两节的 payload 都由 flag 标出来。`awk 'BEGIN{system("rm -rf /")}'` 一个 flag 都没有 —— awk 的程序就是**第一个位置操作数**，实测 Allow 且真 shell 真跑，而同一段 payload 写成 `awk -e '…'` 早就会弹窗了。防线取决于作者有没有多打一个选项。

没有 flag 标记时判据更严：操作数要**同时**含空白和结构标点（`;` `(` `)` `{` `}` `|` `&` `<` `>` 反引号）才算。只有空格不算（否则 `mkdir "my new dir"`、每一条 commit message 都要弹窗），只有标点也不算（否则 `cd $HOME`、`ls ${HOME}`、`cp $SRC $DST` 都要弹窗）。

这一档**封顶在弹窗**，即使那段操作数读起来像一条灾难命令也一样：没有任何东西声明过它是命令，`mkdir "rm -rf /; x"` 只是建了个名字很怪的目录。

**已知的边界**（写下来而不是绕过去）：两者只占其一的语句看不见 —— `gdb -ex 'shell rm -rf /'`（只有空格）、`deno eval "Deno.removeSync('/')"`（只有标点）。

**没有任何模式会替你批准这一档。** `default` / `allow-edits` 弹窗，`auto` 交给模型判，**`yolo` 也弹窗** —— 它走的是 `task_cancel` 那条路（不自动放行、交回显式审批），而不是 Catastrophic 那条（直接拦）。差别在这一档自己的说法：越界删除知道删的东西在项目外面，Opaque 只知道**没人读过这段 payload**，而不可申诉的拒绝要求理由能被陈述。SSE 没有 callback，这一档在那条路上一律 fail-closed。曾经 `yolo` 是直接放行的，那让 `pkexec rm -rf /` 与 `GIT_SSH_COMMAND='rm -rf /' git fetch` 在最常用的自动模式下无声通过；见 [../adr/0020-a-command-in-a-word-nobody-read.md](../adr/0020-a-command-in-a-word-nobody-read.md)。理由与四条不可违反的约束见 [../adr/0018-an-unread-payload-is-a-refusal-not-a-pass.md](../adr/0018-an-unread-payload-is-a-refusal-not-a-pass.md)。

> ⚠️ **档位是从 payload 读出来的，不是从程序名读出来的。** 上面那句「yolo 直接放行」只在 payload **读不出灾难性读法**时成立。如果那段操作数本身能被读成一条 shell 命令、而且读出来是灾难档（`fish -c "rm -rf /"`、`nu -c "rm -rf /"`、你自己写的那个带 `-c` 的小工具收到 `rm -rf /`），判决就是 **Catastrophic** —— 结构性 HardDeny，`yolo` 也拦。
>
> 这条是补一个倒置：`bash` 在本工具的 shell 表里、`fish` 不在，于是**同一条命令**一个任何模式都拦、一个 yolo 直接跑，"换一个它没听说过的 shell" 成了通用的绕过路子。**程序名的集合是无限的，payload 危险不危险是有限可判的**，所以判据挪到了后者。理由见 [../adr/0019-the-tier-follows-the-payload-not-the-program-name.md](../adr/0019-the-tier-follows-the-payload-not-the-program-name.md)。

### 尾部 argv 里的命令（不带任何 flag）

上一节的 `-c` / `-e` 是**用 flag 标出来**的 payload。还有一种更常见的写法：**命令直接接在程序名后面**，没有任何 flag 标记它。

`sudo` / `timeout` / `nohup` / `xargs` 这类前缀执行器一直在一张表里；表外的同类程序（`pkexec`、`firejail`、`bwrap`、`strace`、`systemd-run`、`toolbox run`，以及明天出现的下一个）以前一律静默放行。判据现在是结构性的：**argv 的任何一个后缀，只要它自己读得出一条破坏性命令，就报出来** —— 前面那个程序叫什么无关紧要。

两档，差别在"这个程序会不会执行它的 argv"是不是已知的：

- **表里的前缀执行器**（`sudo` `timeout` `taskset` `ssh` …）按定义就是跑 argv 的，所以判**全档**：`taskset -c 0 rm -rf /` 是结构性 HardDeny。
- **表外的程序**封顶在 **Opaque（弹窗）**：`pkexec rm -rf /` 会问你一次，**在 `yolo` 下也会问**（ADR-0020 之前不会 —— 那一档整个被 yolo 自动放行，于是这行「会问你一次」在最常用的自动模式下是假的）。它可能真的执行，也可能只是把这些词当数据 —— 这正是不知道的那件事，而 `echo rm -rf /` 只是打印六个词，把它变成不可申诉的拒绝就太过了。`echo` / `printf` 整个豁免。

**这道检查不管前面那层有没有"读懂"过这条命令。** `taskset -c 0 rm -rf /` 曾经是 Allow，因为表项把 `-c` 同时当成取值 flag 和 CPU 掩码位置参数，把 `rm` 吃掉了 —— 而"被某个读法认领过"这件事本身会把兜底关掉。**"我读错了"拿到的判决比"我读不动"还弱**，这道检查就是为了不再有这种倒置。

### 写文件的目标是参数时也算写

重定向（`>`）的目标一直会进 fs 维度。**用参数指定写入目标的程序同样会** —— `tee`、`cp` / `mv` / `ln` / `install` 的最后一个操作数、`sed -i` 的文件、`dd of=`、`gzip` / `xz` 一族原地替换的文件、`truncate`，以及 PowerShell 里 `Set-Content` / `Add-Content` / `Out-File` / `Export-Csv` / `Tee-Object` / `New-Item` 的 `-Path` / `-FilePath`（或第一个位置参数）。

在 PowerShell 那边这不是补一个角落：用 cmdlet 写文件本来就是**惯用写法**，重定向才是少见的那个，所以只判重定向等于几乎什么都没判。

## fail-closed：空 Allow 拒绝一切

> 这是架构级安全承诺，不可妥协。详见 [../adr/0003-guard-fail-closed-empty-allow.md](../adr/0003-guard-fail-closed-empty-allow.md)。

空的工具白名单不是"无约束"，而是"什么都不允许"。新增任何工具都必须在 profile 里**显式**配权限才能被调用——不会因开发者忘了配权限而静默放行。

## shell 命令的结构：逐段判定，读不出来就拒

`shell_run` 先把命令拆成**段**（以 `&&`、`||`、`;`、`|` 为界），然后对**每一段**跑一遍完整的 shell 判定（rules 或 glob 白名单），整条命令的判决取**最严的那一段**。任何一段被拒，整条被拒。

这意味着：

- `git status && go test` 在 `patterns: ["git *", "go test"]` 下**可以跑**了（两段都命中白名单）。以前含 `&&` 一律拒绝，需要拆成两次调用。
- `git status && curl http://x` 在同一份 patterns 下**整条被拒**：`curl` 那段不命中，最严的那段决定整条。
- **重定向的目标路径会进 fs 维度判定**。`echo x > ~/.ssh/authorized_keys` 的程序是 `echo`，只看程序等于没看；去掉前导 fd 数字后以 `<` 开头的按读、其余按写，送去和 `fs.write` / `fs.read` 白名单以及内建凭据 denylist 对账。
  - **`>&文件` 算写**，不是描述符复制。bash / sh / zsh 三者都把 `>&` 后面的非数字词当成文件写进去；只有 `>&数字`（如 `2>&1`）和 `>&-` 不指向文件。
  - **重定向可以写在命令词之前**，`>/dev/null rm -rf /` 仍然是一条 `rm -rf /`，破坏性删除门照样拦。

**拆不出段的形态仍然一律拒绝，且是结构性 HardDeny**（`yolo` / `auto` 都越不过）：命令替换 `$(…)` 与反引号、进程替换 `<(…)` / `>(…)`、子 shell 括号 `( )`、here-document `<<`、后台执行的单个 `&`、裸换行与回车、未闭合的引号、结尾的反斜杠。这些形态里「真正要跑的文本」不在被判定的这个字符串里，所以判它等于判了别的东西。

- **命令替换在双引号里也拒，只有单引号里才是数据。** `"$(…)"` 和 `` "`…`" `` 在 POSIX shell 里照样执行替换，所以 `rm -rf "$(echo /)"` 与不带引号的写法是同一件事，判决也是同一档。曾经不是：`rm -rf "$(echo /)"`、`eval "$(echo rm) -rf /"`、`echo k > "$(echo ~/.ssh/authorized_keys)"` 等六种拼法实测走到 Allow，而 `/bin/sh` 真的删了根、真的写了 `authorized_keys`。`echo '$(1+1)'` 这种单引号写法照常可用。

**`shell_run` 带 `env: "powershell"` 时换一个读法。** PowerShell 的转义符是反引号、路径分隔符是反斜杠，POSIX shell 恰好反过来，所以用 POSIX 的读法去读一条 PowerShell 命令会把路径里的分隔符全吃掉 —— `Remove-Item -Recurse C:\temp` 的目标会变成 `C:temp`。它同样是「对词的内容宽容、对结构严格」：`$(…)`、`@(…)`、`${…}`、括号分组、脚本块 `{ }`、here-string、调用运算符与后台的 `&`、`#` 注释、`<`（PowerShell 本身就不支持）、裸换行、未闭合引号与结尾反引号一律拒绝，且同样是结构性 HardDeny。`cmd` **不走**这个读法，仍按 POSIX 读 —— 见下面那条已知边界。

> ⚠️ **已知边界：Windows 上的 `cmd.exe` 命令目前按 POSIX 规则解析，它的 `^` 转义不被识别。**
>
> 这不是免责声明，是让你据此做决定的一条事实。`cmd` 的转义符是第三种（`^`），本仓只有 POSIX 与
> PowerShell 两个 reader，所以一条写给 cmd 的命令是被 POSIX 的规则读的：`^` 在 POSIX 那边不是转义符，
> 会被当成普通字符留在词里；而 POSIX 的 `\` 是转义符，在 cmd 那边却是路径分隔符，于是
> `del C:\temp` 这类路径的分隔符会在读的时候被吃掉。
>
> **影响什么。** `env` 留空或写 `auto` 时，**Windows 上解析成 `cmd`**（`internal/shell::ShellArgv`），
> 所以这是 Windows 的默认路径而不是一个偏门配置。受影响的是**按命令文本做判断的那两层**：
> `shell.patterns` 白名单/黑名单的 glob 匹配，以及重定向目标进 fs 维度时的路径。
> **不受影响的是破坏性删除门** —— 它用的是自己那个宽容的词法器（两种读法都判、取更严的），
> 与 reader 的选择无关，所以 `rd /s C:\` 一类照样拦得住。
>
> **你可以怎么做。** ① 在 Windows 上想依赖 shell 白名单时，把 `env` 显式写成 `powershell`
> （有专门的 reader）；② 或者不要在 pattern 里依赖 `^` 与 `\` 的转义语义，用更粗的 glob，
> 靠交互式审批而不是靠 pattern 精确匹配来收口。
>
> 审批缓存这一侧已经把 cmd 当成**独立语言**了（`approval.Scope.Interpreter`），所以一条 cmd 命令
> 的批准不会串到同样文本的 sh 命令上——**分开的是 scope，不是 reader**。

理由、代价与不变量见 [../adr/0004-guard-stateless-and-shell-metachar-hardblock.md](../adr/0004-guard-stateless-and-shell-metachar-hardblock.md) 的补充后果一节。

> **注意一个额外的收口**：链式命令**没法交互批准**。落到 Prompt 的链会在审批作用域构造时被拒（一条批准规则不能覆盖多个可执行段），所以链要么每一段都被静态放行、要么被拒 —— 弹窗那条路对它不开。

## profile

权限来自 `profiles:` 配置 map（见 [configuration.md](configuration.md)）。每个 profile 是一条具名的**五维**策略（`tools` / `mcp` / `fs` / `shell` / `net` —— destructive 是 profile 之外的结构性维度，不可配置）。示例里的 `coding` profile 给了一个"全工具 + 指定 MCP server + 仓库读写 + allowlist shell + 出站网络"的例子。

### profile 名怎么被选中

profile **不是**按 `agents[].profile` 字段选的 —— 那个字段今天**没有任何生产读点**。当前只有两条真实的选取路径，都以 **profile 的 map 键名**为准：

| 谁 | 怎么选 |
|---|---|
| 编排器（TUI / WS / SSE 聊天） | 固定读键名 `orchestrator`（`internal/bootstrap::Build`）。没有这个键就退回内置的 `DefaultOrchestratorProfile`。 |
| task-API 的远程 worker | 用 **worker 名**当键名查（`internal/api/http::Server.TaskAPI`）。`cmd/agent-worker -name coding` 才会拿到 `coding` profile；查不到时 fail-closed 退回 deny-all。 |

所以示例里的 `coding` profile 想生效，要么把它改名成 `orchestrator`（让聊天用上），要么起一个 `-name coding` 的 worker。`agents:` 块里写 `profile: "coding"` 不会有任何效果，也不会有任何告警。

## 交互式权限模式

在 profile 之上叠加交互式模式（仅 WS 路径可用，见 [tui.md](tui.md)）：

| 模式 | 行为 |
|---|---|
| `default` | 普通拒绝弹窗询问；profile 策略拒绝（`policy: "deny"` 等）**静默拒绝**，不问。 |
| `allow-edits` | 编辑类工具（`internal/guard::EditToolNames`）免提示放行，其余同 `default`。 |
| `yolo` | 越过全部 profile 策略（含 MCP allowlist）。**仍然拦**：灾难性删除、工作目录之外的删除、强制批准工具。 |
| `auto` | 灾难性删除直接拦、越界删除弹窗；**其余一切交给 AI 判断**（`guard.AutoApprovalPrompt`），Go 侧没有静态白/黑名单。模型拿到完整命令原文 + 会话上下文（用户最近的请求、workdir、策略拒绝理由）答 ALLOW/ASK。风险类别写在提示词里，四组：伸出项目之外（提权/关机/磁盘/系统账户/防火墙/系统包管理器/定时任务/远程执行）、不可逆（force-push、删除 VCS 未记录的东西、容器逃逸）、**执行没人读过的代码**（下载即执行、从 `/tmp` `~/Downloads` 跑脚本 —— 远程脚本必须先落盘审计）、数据外泄（外传项目内容/凭据、`env` 把 API key 打进 transcript）。无模型、超时、出错、回复读不懂 → 一律弹窗；auto 退化成 manual，不退化成放行。无阈值可调。 |
| `plan` | 只读，写操作一律拒绝。 |

> **`yolo` 不是"放行所有"。** 结构性 HardDeny（shell 结构读不出来、未知 policy、execpolicy parse-error、**命令嵌套深过 guard 愿意拆的层数**）与灾难性删除在任何模式下都拦得住；强制批准工具（下一节）也一样。`yolo` 越过的是 **profile 说的"不"**。权威枚举在本页上面那一节，这里是提示框，不是清单。

> SSE 备用路径用静态 profile，**不支持**交互式弹窗（见 [../adr/0010-sse-static-profile-no-interactive-perm.md](../adr/0010-sse-static-profile-no-interactive-perm.md)）。

## 强制批准工具（approval-required）：只在交互式传输上可用

有一类工具的破坏力或成本高到不适合被任何静态策略预先放行，它们被标记为**强制批准**：每次调用都必须由用户当场点"允许"，**profile 的 `tools.allow`（哪怕是 `"*"`）、历史授权记录、`yolo` / `auto` 模式一律绕不过**，"始终允许"这个选项对它们也无效。

当前的强制批准工具：

| 工具 | 为什么 |
|---|---|
| `automation_create` / `list` / `read` / `update` / `pause` / `resume` / `delete` / `run` | 持久化定时任务：一次批准会让 agent 在未来无人值守地反复运行 |
| `agent_batch` | 一次调用扇出 N 个子 agent，成本远高于普通工具调用 |
| `github_comment` / `github_approve` / `github_merge` | 对外部仓库的不可撤销写操作 |
| `screenshot` | 抓取屏幕内容 |

**必然后果：这些工具只在 WebSocket / TUI 上可用。** 强制批准需要一条能把弹窗送达用户的双向通道，而只有 WS 传输具备。在 SSE、v1 REST、task-agent 这三条非交互式路径上调用它们，会**恒定**收到：

```
✗ permission denied: tool requires explicit approval
```

这不是缺陷，也不是配置错误 —— 没有人在场，就没有人能批准。若需要在非交互式路径上使用这类能力，正确做法是引入 profile 级的**预授权策略**，而不是把工具降级成普通门禁（那会让它的描述对用户撒谎）。
