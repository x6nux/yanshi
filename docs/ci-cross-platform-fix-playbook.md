# 跨平台 CI 全绿修复手册

> 本文档总结了把 `yanshi`（Go LLM agent 服务端）从 GitHub Actions 全红修到全绿的完整流程，提炼成可复用的排查与修复模式。适用场景：Go 项目接上多平台 CI（ubuntu/macos/windows + race）后大面积飘红，需要系统性定位根因并修复。

## 目录

- [一、排查流程：先归类，再下手](#一排查流程先归类再下手)
- [二、跨平台路径规范化](#二跨平台路径规范化)
- [三、SQLite / modernc 跨平台](#三sqlite--modernc-跨平台)
- [四、DATA RACE 消除模式](#四data-race-消除模式)
- [五、测试时序确定性](#五测试时序确定性)
- [六、CI 工程化技巧](#六ci-工程化技巧)
- [七、提交清理流程](#七提交清理流程)
- [八、快速检查清单](#八快速检查清单)

---

## 一、排查流程：先归类，再下手

CI 失败时**不要逐个 test 去 fix**，先拉日志归类。用 GitHub API 批量抓每个 job 的 `--- FAIL` 行：

```bash
TOKEN=$(printf 'protocol=https\nhost=github.com\n\n' | git credential fill | grep '^password=' | cut -d= -f2-)
RUN=<run-id>
for job in "test (ubuntu)" "test (macos)" "test (windows)" "race"; do
  JID=$(curl -sL -H "Authorization: Bearer $TOKEN" \
    "https://api.github.com/repos/x6nux/yanshi/actions/runs/$RUN/jobs" |
    python3 -c "import json,sys;d=json.load(sys.stdin);print(next(j['id'] for j in d['jobs'] if j['name']=='$job'))")
  echo "=== $job ==="
  curl -sL -H "Authorization: Bearer $TOKEN" \
    "https://api.github.com/repos/x6nux/yanshi/actions/jobs/$JID/logs" |
    python3 -c "import sys,re;[print(l.split()[2]) for l in sys.stdin if l.strip().startswith('--- FAIL')]" |
    sort -u
done
```

归类后通常落到这几类（按出现频率）：

| 现象 | 类别 | 章节 |
|------|------|------|
| 只在 macos/windows 红，ubuntu 绿；VCS 测试 "no changes to commit" 或 map 为空 | 跨平台路径 | [二](#二跨平台路径规范化) |
| `:memory:` 数据库行为异常 | SQLite 跨平台 | [三](#三sqlite--modernc-跨平台) |
| 只在 `race` job 红，报 `WARNING: DATA RACE` | 真竞态 | [四](#四data-race-消除模式) |
| `race` job 红，但每次失败的 test 不同；非 race job 绿 | `-race` timing flake | [五](#五测试时序确定性)、[六](#六ci-工程化技巧) |
| 整个包 `FAIL pkg 600.018s`（无具体 test 名） | 包级超时（多半是某个 test 挂死） | [五](#五测试时序确定性) |

**关键原则**：用户要求"从根因修，不要靠调超时掩盖"。下面多数修复都是根因修；少数（race instrumentation overhead）用 build-tag 缩放 deadline，会明确标注。

---

## 二、跨平台路径规范化

### 症状
- macOS CI 上 VCS 测试大面积红：`map[string]string{} does not contain "a.go"`、`vcs: no changes to commit`
- 同样代码在 ubuntu/windows 本地绿
- 根因：`InitRepo` 用 `filepath.EvalSymlinks` 规范化了 repo root（macOS `/var/folders` → `/private/var/folders`），但调用方传入的 edit 路径没规范化，`filepath.Rel` 算出来以 `..` 开头 → 静默跳过

### 修复模式：两个 helper，区分用途

```go
// canonicalPath：解析 symlink + Windows 小写化。用于"存储/比较"路径
// （worktree path、restore 目的地）。匹配 canonicalRepoRoot 的 case-folding。
func canonicalPath(p string) string {
    abs, err := filepath.Abs(p)
    if err != nil { return filepath.Clean(p) }
    if real, rerr := filepath.EvalSymlinks(abs); rerr == nil { abs = real }
    abs = filepath.Clean(abs)
    if runtime.GOOS == "windows" { abs = strings.ToLower(abs) }
    return abs
}

// resolveSymlinks：只解析 symlink，不 case-fold。用于"生成 VCS key"的路径
// （recordEdit/recordDelete 的 absPath）。关键：解析【父目录】而非全路径，
// 因为目标文件可能还不存在（新建文件先 RecordEditMain 再 fs_write）。
func resolveSymlinks(p string) string {
    abs, err := filepath.Abs(p)
    if err != nil { return filepath.Clean(p) }
    parent, name := filepath.Split(abs)
    if parent != "" {
        if real, rerr := filepath.EvalSymlinks(filepath.Clean(parent)); rerr == nil {
            return filepath.Join(real, name)  // 父目录解析 + 文件名原样
        }
    }
    if real, rerr := filepath.EvalSymlinks(abs); rerr == nil { return real }
    return filepath.Clean(abs)
}
```

### 应用点（凡是 `filepath.Rel(repoRoot, absPath)` 的地方都要查）

| 位置 | 用哪个 | 为什么 |
|------|--------|--------|
| `recordEdit(absPath)` | `resolveSymlinks` 两边 | key 要保 casing；Windows `filepath.Rel` 已大小写不敏感 |
| `recordDelete(absPath)` | 同上 | 同上 |
| `recordEdit(repoRoot)` / `recordDelete(repoRoot)` | `resolveSymlinks` | worktree row 可能绕过 `addWorktreeLocked` 直接插入（测试），没规范化 |
| `restoreScopeLocked(destDir)` | `canonicalPath` | 比较用，要 case-fold |
| `addWorktreeLocked(wtPath)` 存储时 | `canonicalPath` | 和 destDir 比较一致 |
| `vcs.New(worktreeDir)` | `canonicalPath` | 前缀一致性 |
| 测试 helper（`gateCtx`/`artifactCtx`/`RepoRoot` test） | `filepath.EvalSymlinks(root)` | `withinRootAbs` 返回解析后路径，profile 的 allow-path 也要解析后才能 glob 匹配 |

### 排查技巧
- 本地装 `act` 或直接看 macOS `/var` 是不是 `/private/var` 的 symlink：`ls -la /`
- 一个快速验证：在怀疑的 `filepath.Rel` 前后打印 `repoRoot` 和 `absPath`，看是否前缀不一致

---

## 三、SQLite / modernc 跨平台

### 症状
- macOS/windows 上 `:memory:` 数据库写入静默丢失（multi-connection 各自独立 db）
- `PRAGMA journal_mode=WAL` 在 `:memory:` 上行为异常

### 修复模式

```go
// 1. buildDSN：:memory: 不能追加 ?_pragma=（modernc 会当文件路径）
func buildDSN(path string, busyMs, autoCkpt int) string {
    if path == ":memory:" { return path }  // 裸 :memory:
    return path + "?_pragma=busy_timeout(" + ...
}

// 2. Store 加 inMemory 字段，OpenWith 里【真的赋值】（容易漏！）
func OpenWith(path string, opts OpenOptions) (*Store, error) {
    ...
    s := &Store{DB: db, inMemory: path == ":memory:"}  // ← 这行别漏
    if err := s.applyConnectionPragmas(); err != nil { ... }
}

// 3. applyConnectionPragmas / Close 都要查 inMemory 跳过 WAL
func (s *Store) applyConnectionPragmas() error {
    if s.inMemory { return nil }  // :memory: 不跑 WAL
    ...
}

// 4. :memory: 强制单连接
if path == ":memory:" { maxOpen = 1 }
```

### 常见坑
- `inMemory` 字段加了但 `OpenWith` 没赋值 → guard 永远不生效，WAL 照跑
- 测试用 `:memory:` + VCS：改成 `filepath.Join(t.TempDir(), "test.db")` 文件库更稳
- modernc/sqlite 在 macOS/windows 的 `:memory:?_pragma=` 会创建同名文件，不是内存库

---

## 四、DATA RACE 消除模式

### 4.1 channel close-vs-send（最常见）

**症状**：`WARNING: DATA RACE`，`runtime.closechan` vs `runtime.chansend`。典型在"读 goroutine 往 channel send，另一个 goroutine close 同一个 channel"。

**错误做法**：`safeSend` 里 `defer recover()` —— 能防 panic 但 race detector 照样报。

**正确模式：sender-owns-channel**（Go 官方推荐）

```go
// 读 goroutine 是 channel 的唯一 closer。cancel 不直接 close，而是关信号 channel。
type wsBackend struct {
    cur     chan StreamEvent
    curDone chan struct{}  // cancel 关这个；readLoop 的 send-select 捕获它
}

// readLoop 的 send：
select {
case cur <- ev:           // 正常发
case <-done:              // cancel 了 → readLoop 自己 close cur（它是 owner）
    b.takeAndCloseCur(cur)
}

// cancel goroutine：只关 curDone，绝不碰 cur
if b.curDone == curDone { close(curDone) }
```

**为什么不用 non-blocking send（`select { case ch<-ev: default: }`）**：满 buffer 时丢帧，依赖特定帧的测试会挂。blocking send + 信号 channel 才不丢帧。

### 4.2 ticker goroutine vs 主 goroutine 的 close

```go
// 错误：close(ch) 和 ticker 的 ch<- 竞态
tickDone := make(chan struct{})
go func(){ ... case ch<-ev: ... }()
defer close(ch)      // ← 主 goroutine 退出时 close，可能撞上 ticker 还在发
close(tickDone)      // ← 只发信号，ticker 可能还在 in-flight send

// 正确：加 tickStopped 信号，defer 里先关 tickDone 再等 tickStopped
tickStopped := make(chan struct{})
go func(){ defer close(tickStopped); ... }()
defer func(){ close(tickDone); <-tickStopped }()  // LIFO：在 close(ch) 前跑
```

### 4.3 json.Marshal vs 并发写（reflect 读字段竞态）

**症状**：`reflect.Value.String/Int/IsNil` + `time.MarshalJSON` 在 `encoding/json` 里报 race。

**根因**：struct 被 `pump()` goroutine 写（改 State/ExitCode），同时被另一个 goroutine `json.Marshal`。即使加了 `MarshalJSON` 方法，如果锁没绑对也是白搭。

```go
// 错误：meta.mu 是 nil（没赋值），MarshalJSON 的 nil-check 跳过加锁
type liveSession struct {
    mu   sync.Mutex
    meta Session  // meta.mu 是独立字段，默认 nil
}
// MarshalJSON: if s.mu != nil { lock }  ← nil，不加锁

// 正确：Start 里把 meta.mu 绑到 liveSession.mu
sess.meta.mu = &sess.mu  // MarshalJSON 用的锁 = pump 写用的锁
```

### 4.4 pre-cancelled context 的竞态

```go
// 错误：select 里 ctx.Done() 和 response 竞态，快 fake 可能先响应
select { case resp := <-ch: ...; case <-ctx.Done(): ... }

// 正确：进函数先查 ctx.Err()
func Call(ctx, ...) error {
    if err := ctx.Err(); err != nil { return err }  // 前置 fast-path
    ...
}
```

### 4.5 runSecureCapture 的 drain vs Wait

```go
// 错误：Wait 关闭 pipe，io.Copy 还在读 → "file already closed" 或丢输出
waitErr := cmd.Wait()           // ← 关 pipe
stdoutErr := <-stdoutDone       // ← drain goroutine 撞上关闭的 pipe

// 正确：先 drain 到 EOF，再 Wait
stdoutErr := <-stdoutDone       // ← 先 drain 完
stderrErr := <-stderrDone
waitErr := cmd.Wait()           // ← 再 reap
// 记得忽略 io.EOF（drain 正常结束返回 EOF）
if stdoutErr != nil && stdoutErr != io.EOF { return _, stdoutErr }
```

---

## 五、测试时序确定性

### 5.1 FakeModel 瞬间完成 → 并发断言失效

**症状**：`TestXxxRejectsConcurrentTurn` 之类，期望"第二个操作被拒"，但 FakeModel 的 turn 在第二个操作前就跑完了，`active==nil`，断言失败。`-race` 下调度差异放大。

**修复**：换 `BlockingModel`，turn 真的卡住，等 `Started` 信号后再做第二个操作。

```go
model := einollm.NewBlockingModel("resp")
o, _ := orchestrator.New(orchestrator.Config{Model: model})
svc, _ := NewService(Config{Orchestrator: o})  // ← 注意：DefaultModel-only 路径不调 model，必须用 Orchestrator

svc.StartTurn(...)      // turn 卡在 model.Generate
<-model.Started         // 等 model 真的 in-flight
// 现在做第二个操作，active 一定还在
svc.StartTurn(...)      // → ErrTurnAlreadyActive
close(model.Block)      // 收尾，放 goroutine 走
```

### 5.2 turn 生命周期边界（"done 之后还有 defer 没跑"）

**症状**：客户端看到 `done` 就立刻操作，但服务端 turn 的 defer（如 post-turn seal）还没跑，两者竞态。

**根因**：`runUserTurn` 发完 `done` frame 才 return，defer 在 return 时跑——晚于 done。

**修复**：把"必须先于 done 完成"的逻辑从 defer 改成显式调用，放在 `conn.write(done)` **之前**。用 `sealed` flag 保证只跑一次，defer 留作 error-path 兜底。

```go
sealed := false
sealPostTurn := func() {
    if sealed { return }
    sealed = true
    s.sealTurnBoundary(...)  // fold pending edits + 插 seam
}
defer sealPostTurn()                           // error-path 兜底
...
sealPostTurn()                                 // 正常路径：done 之前
conn.write(proto.NewDone())
```

**经典案例**：`SealMainTurnSeam` 会 `commitScope` fold pending edits 并清空 changeset。如果客户端在 done 后立刻 `RecordEditMain`，seal 的 fold 会"偷走"这个 edit → 客户端后续 `CommitMain` 报 "no changes to commit"。

### 5.3 "while a turn is running" 瞬态拒绝

WS handler 里 `list_seams`/`restore_turn` 在 turn 跑时会被 reader goroutine 内联拒绝（只发 error，不发 done）。客户端 drain done 会挂。`setInTurn(false)` 是 defer，在 done 之后才跑——存在窗口。

**修复**：客户端对这种瞬态 error 做重试（已有 `TestWS_RestoreTurn_PersistFailureIsFatalBeforeVCS` 的范式）：

```go
for {
    c.WriteJSON(proto.NewRestoreTurn(id, head))
    c.ReadJSON(&sf)
    if sf.Type == "error" && !contains(sf.Text, "while a turn is running") {
        break  // 真错误，不是瞬态拒绝
    }
}
```

### 5.4 `clampInt(0, 1, N)` 的默认值陷阱

```go
// 错误：clampInt(0,1,1800) 把 0 夹到 1，"10 分钟默认"成死代码
timeout := time.Duration(clampInt(args.TimeoutS, 1, 1800)) * time.Second
if timeout == 0 { timeout = 10 * time.Minute }  // ← 永远不进来

// 正确：默认值在 clamp 之前给
timeout := 10 * time.Minute
if args.TimeoutS > 0 {
    timeout = time.Duration(clampInt(args.TimeoutS, 1, 1800)) * time.Second
}
```

---

## 六、CI 工程化技巧

### 6.1 大包超时：拆步而非盲目加超时

`internal/api/http` 有 200+ 集成测试，单包在 CI 上可能 >10 分钟。不要一上来加 `-timeout 1800s`（掩盖挂死的 test）。先确认是"包真大"还是"有 test 挂死"：

- 本地 `go test -timeout 300s ./internal/api/http/...` 也超时 → 有 test 挂，按 [5.2](#52-turn-生命周期边界done-之后还有-defer-没跑) 排查
- 本地能过、CI 超时 → CI runner 慢，考虑拆步或适度加超时

### 6.2 race job 的 timing flake：每包重试

`-race` 让 orchestrator/ADK 路径慢 ~40x，timing 敏感 test 在单次跑会 flake（同 tree 非race 绿、重跑能过）。真 DATA RACE 跨 run 必现。用每包最多 3 次重试吸收 flake：

```yaml
- name: go test -race (per-package, up to 3 attempts)
  shell: bash
  run: |
    set -e
    fail=0
    for pkg in $(go list ./...); do
      for attempt in 1 2 3; do
        if go test -race -timeout 600s "$pkg"; then break
        elif [ "$attempt" -eq 3 ]; then echo "::error::race failed 3/3: $pkg"; fail=1; fi
      done
    done
    exit $fail
```

**注意**：这是对"环境性 nondeterminism"的合理容忍，不是掩盖真 bug。真 race 3 次都红。

### 6.3 build-tag 检测 `-race`（缩放 deadline）

某些 test 的 read deadline 在 `-race` 下确实不够（orchestrator instrumentation overhead，非 bug）。用 build-tag 检测：

```go
// detectrace_race.go
//go:build race
package http
const raceDetectorEnabled = true

// detectrace_norace.go
//go:build !race
package http
const raceDetectorEnabled = false

// 用法
deadline := 30 * time.Second
if raceDetectorEnabled { deadline = 180 * time.Second }
```

### 6.4 跨平台测试用 build tag，不要 runtime.GOOS

用户明确要求"为每个平台写专门的测试函数，通过标签启用"。别在一个 test 里 `if runtime.GOOS == "windows" {...} else {...}`，拆成：

- `xxx_cov_test.go`（跨平台共享）
- `xxx_cov_windows_test.go`（`//go:build windows`）
- `xxx_cov_unix_test.go`（`//go:build !windows`）

### 6.5 Windows shell 测试的 orphan-child 陷阱

`cmd /c ping ...` 杀 `cmd.exe` 会孤儿化 `ping.exe`，它继续占 stdout pipe，timeout 的 `✗` chunk 永远到不了。改用 `powershell -Command "Start-Sleep ..."`（单进程，kill 即死）。

---

## 七、提交清理流程

全绿后用户要求"合并提交、清理旧 CI 记录"。

### 7.1 squash CI-fix 小提交

```bash
# 1. 找 base（最后一个主题提交）
git log --oneline --reverse | head   # 找 init/test/docs/.../foundation 之后第一个 fix
BASE=<foundation-commit>

# 2. 备份（安全）
git branch backup-pre-squash HEAD

# 3. soft reset 到 base，所有改动进 staging
git reset --soft $BASE

# 4. 一个大提交（message 总结根因分类）
git commit -m "fix(ci): cross-platform test suite + DATA RACE elimination

Squashes N incremental commits. 根因分类：
- 跨平台路径（macOS symlink / Windows case / sqlite :memory:）
- DATA RACE（sender-owns-channel / ticker 同步 / MarshalJSON 锁绑定 / ctx.Err 前置）
- 时序确定性（seal-before-done / BlockingModel / drain-before-Wait）
..."

# 5. 验证 tree 一致（squash 前后内容相同）
git diff HEAD backup-pre-squash  # 应为空

# 6. force push
git push --force-with-lease origin main

# 7. 确认新 commit CI 绿后，删 backup
git branch -D backup-pre-squash
```

### 7.2 清理旧 CI run

```bash
TOKEN=$(printf 'protocol=https\nhost=github.com\n\n' | git credential fill | grep '^password=' | cut -d= -f2-)
python3 -c "
import json, urllib.request, os
T = '$TOKEN'
def api(m, url):
    req = urllib.request.Request(url, method=m, headers={'Authorization':'Bearer '+T,'Accept':'application/vnd.github+json'})
    try: return urllib.request.urlopen(req).status
    except urllib.error.HTTPError as e: return e.code
req = urllib.request.Request('https://api.github.com/repos/x6nux/yanshi/actions/runs?per_page=100',
    headers={'Authorization':'Bearer '+T,'Accept':'application/vnd.github+json'})
runs = json.loads(urllib.request.urlopen(req).read())['workflow_runs']
latest = {}
for r in runs:  # 每个 workflow 只留最新一条
    wf = r['name']
    if wf not in latest or r['id']>latest[wf]: latest[wf]=r['id']
for r in runs:
    if r['id'] in latest.values(): continue
    if api('DELETE', f\"https://api.github.com/repos/x6nux/yanshi/actions/runs/{r['id']}\")==204:
        pass
print('done')
"
```

---

## 八、快速检查清单

修复跨平台 CI 时逐项过一遍：

- [ ] 所有 `filepath.Rel(repoRoot, absPath)` 调用点：两边都 `resolveSymlinks` 了吗？
- [ ] `:memory:` 路径：`buildDSN` 裸返回、`inMemory` 字段真赋值、WAL pragma 跳过了吗？
- [ ] channel 有没有"一个 goroutine send、另一个 close"？改成 sender-owns。
- [ ] `json.Marshal` 的 struct 有没有并发写？`MarshalJSON` 的锁绑对了吗？
- [ ] `select { case <-ctx.Done() }` 前面有没有前置 `ctx.Err()` 检查？
- [ ] `cmd.Wait()` 是不是在 drain pipe 之后调用的？
- [ ] 期望"并发被拒"的 test：用的是 BlockingModel 还是 FakeModel（瞬间完成）？
- [ ] 客户端看到 `done` 就操作的场景：服务端的 post-turn 处理是不是在 done 之前？
- [ ] `clampInt(0, low, high)` 会不会把默认值夹错？
- [ ] Windows-only 的 shell test：`cmd /c` 会不会孤儿化子进程？
- [ ] race job 是"每次同一个 test 红"（真竞态，按四修）还是"每次不同 test 红"（timing flake，按六.2 加重试）？
