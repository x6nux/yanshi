# Batch A1 — 安全执行底座 (S06/S07/S08/S09/T07/T08) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在 yanshi 中交付一个类型化、可审计、fail-closed 的安全执行底座：HardDeny 永不被 approval 覆盖；所有子进程启动只走 `SecureProcessFactory`；execpolicy 提供结构化 Decision；S07 持久审批按 once/session/persistent 隔离；sandbox/网络属 Phase 0 skeleton，明确标记 `host-guard-degraded`；shell runtime v2 用独立 lifecycle context 与 ring buffer 支撑持久 session、`/jobs` 和 stale 语义。

**Architecture:** 三阶段递进，按 coordinator 建议（第 13 项）拆分：**A1a（Task 1–9）真实可交付**——类型化 `guard.Decision`、删除 orchestrator fail-open、shell_run 统一 Authorize、execpolicy lexer/parser、approval manager（copy-on-write 持久化、audit emitter、allow_persistent 全链路）、proto/WS/SSE/TUI `/permissions`。**A1b（Task 10–13）显式 Phase 0 skeleton**——`internal/sandbox/` 三档抽象 + 四平台 adapter 骨架（无伪造系统调用，全部返回 `DegradedHostGuard`，`CanKillTree=false`）；`internal/netpolicy/` deny-wins 引擎 + 自定义 DialContext 的 loopback proxy。**A1c（Task 14–22）Shell runtime v2 + `/jobs`**——`SecureProcessFactory` 固定流程 authorize→argv→netpolicy.PrepareEnv→Sandbox.Prepare→Start；`internal/shell/Manager` 用 `context.WithoutCancel(ctx)` 托管持久 session、后台输出泵、`KillTree` capability 报告、job 持久化以支持重启 stale。

**Tech Stack:** Go 1.26.4；`github.com/cloudwego/eino` ADK；SQLite via `modernc.org/sqlite`；`golang.org/x/sys`；Windows Bubble Tea 本地 fork（不可移除）；现有 guard / proto / orchestrator / bootstrap / store / config / TUI 真实接口。

---

## 必读的真实接口（不要在计划里臆造）

- `internal/guard/guard.go`：`type Action struct {Tool, FSWant, Shell, NetHost}`、`type Decision struct {Allowed bool; Reason string}`、`func (g *Guard) Check(p, a) Decision`；短路顺序 `checkTools → checkFS → checkShell → checkNet`；`checkTools` 空 `Tools.Allow` 时拒绝。
- `internal/guard/guard.go:104`：`checkShell` 先拒绝 `[]string{"&&","&","||",";","|","\`","$(","\n","\r",">","<"}`，再走 `deny/allowlist/denylist/unknown`。A1a 保留这道硬拒绝作为深度防御。
- `internal/guard/profile.go`：`PermissionProfile{FS, Tools, Shell, Net}`、`ShellPerm{Policy, Patterns}`、`NetPerm{Allow, Hosts}`。
- `internal/agent/orchestrator/orchestrator.go:155-161`（**必须删**）：`New()` 中 `if len(profile.Tools.Allow) == 0 { profile = PermissionProfile{Tools: {Allow:{"*"}}} }`，把空 allow 改成 `*` —— 典型 fail-open。
- `internal/tools/shell.go:117-129`：`stream()` 中 `safeShellCommands[firstWord(a.Command)]` 分支只做元字符/path traversal 检查，**跳过 Authorize**（**必须删**）；其余命令才走 `Authorize(ctx, guard.Action{Tool:"shell_run", Shell:a.Command}, argsJSON)`。
- `internal/tools/guard.go:160-206`：`NewGuardedTool(name, display, desc, timeout, params *schema.ParamsOneOf, stream StreamFunc)`；`StreamFunc = func(ctx, argsJSON) <-chan ToolChunk`。同步 handler 用 `SyncStream(fn (ctx, argsJSON)(string, error))` 包装。
- `internal/tools/permctx.go:121-196`：`allowKey(action)` 拼成 `tool|fs-op|paths;...|shell|net-host`；`Authorize` 顺序：无 profile → DenyErr；exact allowlist 命中 → allow；`guard.Check` 通过 → allow；否则调 callback；`PermissionAllow/PermissionAlwaysAllow/PermissionDeny`。A1a 必须新增 `PermissionAllowPersistent` 与 `PermissionAllowSession`，并保证 HardDeny 永不入 callback。
- `internal/tools/helpers.go:20`：`func params(m map[string]*schema.ParameterInfo) *schema.ParamsOneOf`（计划里所有 schema 构造都用它）。`runTool(ctx, t, argsJSON)` 测试 helper 在 helpers.go:71。
- `internal/tools/web.go:60-83`：`runFetch` 自己构造 `guard.New().Check(prof, guard.Action{Tool:"web_fetch", NetHost:host})`，**与代理共用 host 策略必须抽公共源**。
- `internal/proto/frame.go`：`ClientFrame`/`ServerFrame`、`ServerFrame.SSEEvent()`（line 395）；现有 permission_response 决策字符串 `allow|deny|always_allow`，A1a 扩展为 `allow|deny|always_allow|allow_session|allow_persistent`。
- `internal/api/http/server.go:13`：`Config{Token, Compaction, Store}`；`Server` 只挂 mux/auth。新增子系统通过 bootstrap 在 ws.go 内引用，不改 server.go 签名。
- `internal/api/http/ws.go:445` 起：每个 WS 连接单次构造 connCtx；reader goroutine 直投 `permission_response` 到 `pt`；main loop 内 case `permission_response` 已被 reader 吃掉，permissions_list/permission_revoke/jobs_list 等控制帧要在 main loop 的 `switch cf.Type` 里新增分支（紧贴 `list_mcp`/`session_list` 同级，**注入 turnCtx 等价的 profile**，避免裸调用）。
- `internal/api/http/chat.go:231`：`writeSSEFrame(w, fl, f)`；A1a 不删它，只把新增事件类型复用同一函数。SSE 不安装 `WithPermissionCallback`，不解读 `permissions_list`。
- `internal/cli/backend.go:19`：`StreamEvent` 是 TUI 唯一事件载体，A1a 必须新增 `Jobs []proto.JobInfo` 和 `Permissions []proto.PermissionInfo`。
- `internal/cli/wsbackend.go:240`：`isControlReply(kind)` 必须把 `jobs`、`permissions`、`permission_rule_hit` 加入，否则 control-mode cur 永不关闭。
- `internal/cli/tui/permissions.go:112`：`permOptions = [{Allow,"allow"},{Always allow,"always_allow"},{Deny,"deny"}]`，A1a 必须新增 `{Persistent allow,"allow_persistent"}`（与 `session_restored` 同级的 popup）。
- `internal/cli/tui/commands.go:30`：`commandTable` 是 `/help` 唯一来源；`sendControlFrame` 是控制帧入口。
- `internal/cli/tui/model.go:843`：`applyEvent(ev)` switch `ev.Kind`；新增 `jobs`/`permissions`/`permission_rule_hit` 必须补完整 case（而不是"在事件分派处理 … 时"自然语言）。
- `internal/acp/spawn.go:148`、`internal/execprobe/probe.go:47`、`internal/agent/goalloop/evaluators.go:50,52`、`internal/agent/goalloop/implementer.go:96,105,114`、`internal/tools/shell.go:227-246`：**仓库所有裸 `exec.CommandContext`/`exec.Command` 入口**。A1c 的 `SecureProcessFactory` 必须能覆盖至少 shell v2、legacy `shell_run`、ACP `spawn.go`；`goalloop`/`execprobe` 不在 A1 范围，但要在计划末尾列出"未覆盖入口清单"作为已知 gap。
- `internal/store/kv.go`：`KVSet/KVGet`，approval manager 依赖小接口 `approval.KV = interface { KVSet(string, string) error; KVGet(string) (string, bool, error) }`，`*store.Store` 满足。

---

## File Structure

| File | Phase | Purpose |
|---|---|---|
| `internal/guard/guard.go` | A1a | `Decision{Verdict, RuleID, Justification, Promptable}` 替换 `{Allowed, Reason}`；HardDeny 标记位 |
| `internal/guard/profile.go` | A1a | 新增 `Shell.ExecPolicy` 引用与 `Net.HostPolicy` |
| `internal/guard/guard_test.go` | A1a | 覆盖 Allow/Prompt/HardDeny 三态与 approval 不可覆盖 |
| `internal/agent/orchestrator/orchestrator.go` | A1a | 删除 `Tools.Allow=="*"` fail-open fallback |
| `internal/bootstrap/bootstrap.go` | A1a/A1c | 显式从 cfg.Profiles 传 profile；装配 approval/sandbox/netpolicy/shell manager/SecureProcessFactory |
| `internal/tools/shell.go` | A1a/A1c | `safeShellCommands` 降级为 display-hint，统一 `Authorize`；legacy path 走 `SecureProcessFactory` |
| `internal/execpolicy/lexer.go` | A1a | 字节索引扫描；正确消费 `&&`/`||`/`>>`/`<<`/`2>` 等 |
| `internal/execpolicy/parser.go` | A1a | pipeline/redirect/control token/segment tree |
| `internal/execpolicy/policy.go` | A1a | `Rule`/`Evaluate`，`DenyFlags` 仅命中时才判 deny |
| `internal/execpolicy/*_test.go` | A1a | IFS/${IFS}/$VAR/%VAR%/glob/abs/Windows .exe/`>>`/fd 回归集 |
| `internal/approval/types.go` | A1a | `TTL`/`Scope`/`Source`/`Rule`/`Match`/`AuditEvent` |
| `internal/approval/manager.go` | A1a | copy-on-write persist、`TTLOnce` 进程隔离、`TTLPersistent` 才入 KV |
| `internal/approval/manager_test.go` | A1a | hit/miss/consume/expire/revoke/persist-fail-closed |
| `internal/tools/permctx.go` | A1a | HardDeny 短路；`PermissionAllowSession`/`AllowPersistent` 决策；approval manager 接入 |
| `internal/proto/frame.go` | A1a/A1c | `PermissionInfo`/`JobInfo`/新 constructors/`Permissions`/`Jobs` 字段 |
| `internal/api/http/ws.go` | A1a/A1c | `permissions_list`/`permission_revoke`/`jobs_list`/`job_read`/`job_write`/`job_cancel` 分支；emit `permission_rule_hit` |
| `internal/api/http/chat.go` | A1a | SSE 仅复用 `writeSSEFrame`；不解读交互审批帧 |
| `internal/api/http/jobs.go` | A1c | `jobInfo` 转换；`normalizeJobTimes` 不伪造当前时间 |
| `internal/api/http/ws_perm_test.go` | A1a | 全链路 permission 集成测试 |
| `internal/cli/backend.go` | A1a/A1c | `StreamEvent.Permissions`/`Jobs` 字段 |
| `internal/cli/wsbackend.go` | A1a/A1c | `isControlReply` 增加 `jobs`/`permissions`/`permission_rule_hit` |
| `internal/cli/tui/permissions.go` | A1a | 新增 "Persistent allow" popup 选项 |
| `internal/cli/tui/commands.go` | A1a/A1c | `/permissions`/`/jobs` 注册 |
| `internal/cli/tui/model.go` | A1a/A1c | `applyEvent` 完整 case diff |
| `internal/cli/tui/jobs.go` | A1c | `/jobs`/`jobsEntry`/picker |
| `internal/sandbox/types.go` | A1b | `AccessTier`/`Sandbox`/`CapabilityReport{CanKillTree}` |
| `internal/sandbox/factory.go` | A1b | `New` 返回 DegradedHostGuard |
| `internal/sandbox/sandbox_{windows,linux,darwin,other}.go` | A1b | Phase 0 骨架；无系统调用 |
| `internal/netpolicy/policy.go` | A1b | deny-wins + DNS/private-IP re-check |
| `internal/netpolicy/proxy.go` | A1b | loopback HTTP proxy + DialContext + PrepareEnv |
| `internal/netpolicy/policy_test.go` | A1b | deny-wins/loopback/private/DNS/redirect |
| `internal/tools/secproc.go` | A1c | `WithSandbox`/`WithNetworkPolicy` context API + `SecureProcessFactory` |
| `internal/shell/shell_command.go` | A1c | `ShellArgv(env, command)` builder（替代 `defaultShellProgram/Args`） |
| `internal/shell/types.go` | A1c | `State`/`LaunchSpec`/`Session`/`Job`/`Console`/`Process` |
| `internal/shell/manager.go` | A1c | 持久 session、ring buffer、KillTree、job 持久化 |
| `internal/shell/console_{unix,windows,other}.go` | A1c | PTY capability 边界；显式 `ErrPTYUnavailable` |
| `internal/tools/shell_v2.go` | A1c | 9 个 GuardedTool；真实 Tool 名 Authorize |
| `internal/tools/shell_v2_test.go` | A1c | start/read/write/wait/cancel/job 全链路 |
| `internal/config/config.go` | A1a/A1b/A1c | `SecurityConfig`（含 `*bool` 字段） |
| `config.example.yaml` | A1a/A1b/A1c | 完整 security 配置示例 |

---

# A1a — 类型化 Decision、HardDeny 防火墙、execpolicy、持久审批（真实可交付）

A1a 验收：`guard.Decision` 三态化；HardDeny 永不可被 approval/callback/YOLO/auto 覆盖；orchestrator 不再有 `*` fallback；shell_run 不再被内置 safe list 跳过 Authorize；execpolicy 字节索引正确解析 `&&`/`||`/`>>` 且 `go test` 不被 no-real-e2e 规则误拒；approval manager 提供完整 Match/Record/List/Revoke 与 audit 发射；`/permissions` 与 allow_persistent 全链路打通。

## Task 1: 类型化 guard.Decision（Allow/Prompt/HardDeny）

**Files:**
- Modify: `internal/guard/guard.go`
- Modify: `internal/guard/guard_test.go`
- Modify: `internal/agent/registry/registry.go`
- Modify: `internal/acp/policy.go`
- Modify: `internal/agent/goalloop/evaluators.go`
- Modify: `internal/tools/fs.go`
- Modify: `internal/tools/web.go`
- Modify: `internal/tools/web_test.go`
- Modify: `internal/tools/permctx.go`

- [ ] **Step 1: 写失败测试（三态语义 + HardDeny 不可被 approval 改写）**

新增 `internal/guard/decision_test.go`：

```go
package guard

import "testing"

func TestDecisionVerdictOrderAndPromptable(t *testing.T) {
	if HardDeny == Allow || HardDeny == Prompt || Prompt == Allow {
		t.Fatal("verdicts must be distinct")
	}
	if (Decision{Verdict: HardDeny}).Promptable {
		t.Fatal("HardDeny must never be Promptable")
	}
	if !(Decision{Verdict: Prompt}).Promptable {
		t.Fatal("Prompt must be Promptable")
	}
	if (Decision{Verdict: Allow}).Promptable {
		t.Fatal("Allow must not be Promptable")
	}
}

func TestDecisionIsAllowed(t *testing.T) {
	if !(Decision{Verdict: Allow}.IsAllowed()) {
		t.Fatal("IsAllowed() must be true for Verdict=Allow")
	}
	if (Decision{Verdict: Prompt}.IsAllowed()) {
		t.Fatal("IsAllowed() must be false for Verdict=Prompt")
	}
	if (Decision{Verdict: HardDeny}.IsAllowed()) {
		t.Fatal("IsAllowed() must be false for Verdict=HardDeny")
	}
}

func TestCheckEmptyToolsAllowIsHardDeny(t *testing.T) {
	g := New()
	dec := g.Check(PermissionProfile{}, Action{Tool: "shell_run"})
	if dec.Verdict != HardDeny || dec.Promptable {
		t.Fatalf("empty Tools.Allow must be HardDeny and not Promptable, got %#v", dec)
	}
}

func TestCheckShellMetacharIsHardDeny(t *testing.T) {
	g := New()
	prof := PermissionProfile{
		Tools: ToolsPerm{Allow: []string{"shell_run"}},
		Shell: ShellPerm{Policy: "allowlist", Patterns: []string{"*"}},
	}
	dec := g.Check(prof, Action{Tool: "shell_run", Shell: "ls; rm -rf /"})
	if dec.Verdict != HardDeny || dec.Promptable {
		t.Fatalf("metachar command must be HardDeny/non-promptable, got %#v", dec)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/guard/ -run 'DecisionVerdict|DecisionAllowed|CheckEmptyToolsAllowIsHardDeny|CheckShellMetacharIsHardDeny' -v`

Expected: FAIL — `Verdict`/`Promptable`/`Allowed()` 与 `HardDeny` 在当前 `Decision{Allowed bool; Reason string}` 上不存在。

- [ ] **Step 3: 实现类型化 Decision 与 HardDeny 分级**

把 `internal/guard/guard.go` 顶部 `Decision` 与每个 `checkXxx` 改造为：

```go
// Verdict is the typed outcome of a guard check. Allow = explicit pass; Prompt
// = static profile did not allow but the action is safe enough to escalate to
// an interactive callback; HardDeny = structural fail-closed (no profile,
// empty Tools.Allow, shell metachar, unknown policy, deny rule, deny flag,
// parser failure) — the callback layer MUST NOT override HardDeny.
type Verdict uint8

const (
	Allow Verdict = iota
	Prompt
	HardDeny
)

// Decision is the guard's verdict for an Action. Reason remains the human-
// readable explanation (kept for backwards compatibility with the old
// {Allowed bool; Reason string} shape via the Allowed() shim).
//
// Promptable is the single source of truth for "may the approval callback
// override this?" — orchestrator/tools/transport consult Promptable rather
// than re-deriving it from Verdict, so the HardDeny firewall stays in one
// place. RuleID/Justification carry the execpolicy explanation when set.
type Decision struct {
	Verdict       Verdict
	Reason        string
	RuleID        string
	Justification string
	Promptable    bool
}

// IsAllowed is a binary convenience for call sites that do not need to
// distinguish Prompt from HardDeny. Security-sensitive code (Authorize) MUST
// switch on Verdict/Promptable directly.
func (d Decision) IsAllowed() bool { return d.Verdict == Allow }

func allow() Decision { return Decision{Verdict: Allow} }
func prompt(reason string) Decision {
	return Decision{Verdict: Prompt, Reason: reason, Promptable: true}
}
func hardDeny(reason string) Decision {
	return Decision{Verdict: HardDeny, Reason: reason, Promptable: false}
}
```

将 `Check` 与每个 `checkXxx` 替换为使用上述 helper：

```go
func (g *Guard) Check(p PermissionProfile, a Action) Decision {
	if d := g.checkTools(p, a); d.Verdict != Allow {
		return d
	}
	if d := g.checkFS(p, a); d.Verdict != Allow {
		return d
	}
	if d := g.checkShell(p, a); d.Verdict != Allow {
		return d
	}
	if d := g.checkNet(p, a); d.Verdict != Allow {
		return d
	}
	return allow()
}

func (g *Guard) checkTools(p PermissionProfile, a Action) Decision {
	if len(p.Tools.Allow) == 0 {
		return hardDeny("no tools permitted by profile")
	}
	for _, pat := range p.Tools.Allow {
		if ok, err := MatchGlob(filepath.ToSlash(pat), filepath.ToSlash(a.Tool)); err == nil && ok {
			return allow()
		}
	}
	// Tool-not-on-allowlist is PROMPTABLE: an interactive user may approve a
	// new tool. Only the structural "no tools allowed at all" case above is
	// HardDeny.
	return prompt(fmt.Sprintf("tool %q not permitted", a.Tool))
}

func (g *Guard) checkFS(p PermissionProfile, a Action) Decision {
	if len(a.FS.Paths) == 0 {
		return allow()
	}
	var allowed []string
	if a.FS.Op == "read" {
		allowed = p.FS.Read
	} else {
		allowed = p.FS.Write
	}
	if len(allowed) == 0 {
		return hardDeny(fmt.Sprintf("no paths permitted for op %q", a.FS.Op))
	}
	for _, raw := range a.FS.Paths {
		path := filepath.ToSlash(filepath.Clean(raw))
		ok := false
		for _, pat := range allowed {
			if m, err := MatchGlob(filepath.ToSlash(pat), path); err == nil && m {
				ok = true
				break
			}
		}
		if !ok {
			return prompt(fmt.Sprintf("path %q not permitted for op %q", raw, a.FS.Op))
		}
	}
	return allow()
}

func (g *Guard) checkShell(p PermissionProfile, a Action) Decision {
	if a.Shell == "" {
		return allow()
	}
	// Metacharacter rejection is a STRUCTURAL HardDeny: no glob can ever
	// safely cover a chained command, so the interactive callback MUST NOT
	// override it. This is the second layer of defense on top of execpolicy
	// parsing (Task 4) — both stay.
	for _, m := range []string{"&&", "&", "||", ";", "|", "`", "$(", "\n", "\r", ">", "<"} {
		if strings.Contains(a.Shell, m) {
			return hardDeny("shell metacharacter rejected: " + m)
		}
	}
	switch p.Shell.Policy {
	case "deny":
		return hardDeny("shell denied by policy")
	case "", "allowlist":
		for _, pat := range p.Shell.Patterns {
			if ok, err := MatchGlob(pat, a.Shell); err == nil && ok {
				return allow()
			}
		}
		return prompt(fmt.Sprintf("shell command %q not on allowlist", a.Shell))
	case "denylist":
		for _, pat := range p.Shell.Patterns {
			if ok, err := MatchGlob(pat, a.Shell); err == nil && ok {
				return hardDeny(fmt.Sprintf("shell command %q denied by denylist", a.Shell))
			}
		}
		return allow()
	}
	return hardDeny(fmt.Sprintf("unknown shell policy %q", p.Shell.Policy))
}

func (g *Guard) checkNet(p PermissionProfile, a Action) Decision {
	if a.NetHost == "" {
		return allow()
	}
	if !p.Net.Allow {
		return hardDeny("network access denied")
	}
	if len(p.Net.Hosts) == 0 {
		return allow()
	}
	host := strings.ToLower(a.NetHost)
	for _, pat := range p.Net.Hosts {
		if ok, err := MatchGlob(strings.ToLower(pat), host); err == nil && ok {
			return allow()
		}
	}
	return prompt(fmt.Sprintf("host %q not permitted", a.NetHost))
}
```

A1a 必须在同一 Task 迁移全部历史 `.Allowed` 字段读取，不能留到后续 Task，否则此 Task 不可编译。生产代码替换为下列精确形式：

```go
// internal/agent/registry/registry.go
return dec.IsAllowed(), nil

// internal/acp/policy.go（4 处）、internal/agent/goalloop/evaluators.go（1 处）、
// internal/tools/fs.go（1 处）、internal/tools/web.go（2 处）
if !d.IsAllowed() {
	return fmt.Errorf("permission denied: %s", d.Reason)
}

// internal/tools/permctx.go（Task 7 会再升级为 Verdict switch；本 Task 先保证编译）
if dec.IsAllowed() {
	return nil
}
```

`internal/guard/guard_test.go` 与 `internal/tools/web_test.go` 中所有 `assert.True(t, d.Allowed, ...)` 替换成 `assert.True(t, d.IsAllowed(), ...)`，所有 `assert.False(t, d.Allowed, ...)` 替换成 `assert.False(t, d.IsAllowed(), ...)`。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/guard/ -v`

Expected: PASS；所有历史 `dec.Allowed` 调用经由 shim 仍然工作；HardDeny 标记不可被 callback 改写。

- [ ] **Step 5: 提交**

```bash
git add internal/guard/guard.go internal/guard/decision_test.go
git commit -m "refactor(guard): typed Decision with Allow/Prompt/HardDeny firewall"
```

---

## Task 2: 删除 orchestrator fail-open，bootstrap 显式装配 profile

**Files:**
- Modify: `internal/agent/orchestrator/orchestrator.go:155-161`
- Modify: `internal/bootstrap/bootstrap.go`
- Test: `internal/agent/orchestrator/orchestrator_test.go`

- [ ] **Step 1: 写失败测试（空 profile 不应被偷偷改成 `*`）**

```go
package orchestrator

import (
	"testing"

	"github.com/x6nux/yanshi/internal/guard"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
)

func TestNew_NoMoreWildcardFallbackOnEmptyProfile(t *testing.T) {
	fm := einollm.NewFakeModelWithMessages(nil, nil)
	o, err := New(Config{Model: fm, Profile: guard.PermissionProfile{}})
	if err != nil {
		t.Fatal(err)
	}
	if got := o.ProfileForTest(); got.Tools.Allow != nil {
		t.Fatalf("orchestrator must not synthesize Tools.Allow={\"*\"}; got %#v", got.Tools.Allow)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/agent/orchestrator/ -run TestNew_NoMoreWildcardFallbackOnEmptyProfile -v`

Expected: FAIL — 当前 `New()` 在 `len(profile.Tools.Allow) == 0` 时把 profile 替换成 `{Tools:{Allow:{"*"}}}`。

- [ ] **Step 3: 实现：删除 fallback 并加测试 accessor**

把 `internal/agent/orchestrator/orchestrator.go:155-161` 改为：

```go
	// Profile is taken AS-IS from cfg.Profile. An empty Tools.Allow is a
	// fail-closed profile (every tool call HardDenies at the tools dimension),
	// which bootstrap prevents by always passing an explicit profile sourced
	// from cfg.Profiles["orchestrator"] (or a coding-profile fallback that
	// names concrete tools rather than "*"). The previous permissive fallback
	// (Allow:{"*"}) was a fail-open bug — it silently widened whatever the
	// operator forgot to configure. Do not re-introduce it.
	profile := cfg.Profile
```

在 `Orchestrator` 末尾新增测试 accessor：

```go
// ProfileForTest exposes the resolved profile for orchestrator internal tests
// (Task 2 regression). Not used by production code paths.
func (o *Orchestrator) ProfileForTest() guard.PermissionProfile { return o.profile }
```

把 `internal/bootstrap/bootstrap.go` 的 profile 解析从：

```go
	profile := guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
		Net:   guard.NetPerm{Allow: true},
	}
	if cfg.Profiles != nil {
		if p, ok := cfg.Profiles["orchestrator"]; ok {
			profile = p
		}
	}
```

改为显式具名 profile：

```go
	// Explicit profile: the orchestrator no longer falls back to Tools={"*"}.
	// When the operator did not configure profiles.orchestrator, we ship a
	// concrete "coding" profile naming the tools the orchestrator actually
	// uses (so a forgotten profile block stays least-privilege rather than
	// fail-open). Operators who need shell/net widening must declare it in
	// config.yaml.
	profile := guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{
			"fs_read", "fs_list", "fs_search", "fs_glob", "fs_write", "fs_edit", "fs_patch", "fs_mkdir",
			"shell_run", "shell_start", "shell_read", "shell_write_stdin", "shell_wait", "shell_cancel",
			"task_shell_start", "task_shell_wait", "task_shell_stdin", "task_shell_cancel",
			"memory_search", "memory_recall", "memory_write",
			"web_fetch", "time_now", "skill_use", "vcs_*",
			"agent_start", "workflow_start", "analysis", "summarize",
		}},
		Net: guard.NetPerm{Allow: true},
	}
	if cfg.Profiles != nil {
		if p, ok := cfg.Profiles["orchestrator"]; ok {
			profile = p
		}
	}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/agent/orchestrator/ ./internal/bootstrap/ -v`

Expected: PASS；orchestrator 零值 profile 不再被替换为 `*`；bootstrap 仍能正常 Build。

- [ ] **Step 5: 提交**

```bash
git add internal/agent/orchestrator/orchestrator.go internal/agent/orchestrator/orchestrator_test.go internal/bootstrap/bootstrap.go
git commit -m "fix(orchestrator): remove wildcard fail-open fallback; bootstrap uses explicit coding profile"
```

---

## Task 3: shell_run 统一 Authorize（删除 safe-command bypass）

**Files:**
- Modify: `internal/tools/shell.go:107-129`
- Test: `internal/tools/shell_test.go`

- [ ] **Step 1: 写失败测试（safe list 不能再跳过 Authorize）**

```go
package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/guard"
)

func TestShellRun_SafeListNoLongerBypassesAuthorize(t *testing.T) {
	// "ls" is in safeShellCommands. With a profile that DENIES shell entirely,
	// the tool MUST still deny — proving the safe-list no longer bypasses
	// Authorize.
	sh := NewShellTools(t.TempDir())
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"shell_run"}},
		Shell: guard.ShellPerm{Policy: "deny"},
	})
	out, _ := runTool(ctx, sh.Run, `{"command":"ls "}`)
	if !strings.Contains(out, "permission denied") {
		t.Fatalf("safe-list command must still go through Authorize; got %q", out)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/tools/ -run TestShellRun_SafeListNoLongerBypassesAuthorize -v`

Expected: FAIL — 当前 `safeShellCommands["ls"]` 分支跳过 `Authorize`，命令会直接执行。

- [ ] **Step 3: 实现：保留 safeShellCommands 仅作 display-hint，所有命令统一过 Authorize**

把 `internal/tools/shell.go:117-129` 的完整授权分支：

```go
			if safe := safeShellCommands[firstWord(a.Command)]; safe {
				if hasShellMetachar(a.Command) {
					pushErrChunk(ch, fmt.Errorf("safe command must not contain shell metacharacters (| ; > < && ||)"))
					return
				}
				if strings.Contains(a.Command, "../") || strings.Contains(a.Command, `..\`) {
					pushErrChunk(ch, fmt.Errorf("'..' path traversal is not allowed; use paths relative to the work root"))
					return
				}
			} else if err := Authorize(ctx, guard.Action{Tool: "shell_run", Shell: a.Command}, argsJSON); err != nil {
				pushErrChunk(ch, err)
				return
			}
```

替换成下列完整分支（函数其余代码不修改）：

```go
			// Every shell command goes through Authorize, including those whose
			// first word appears in safeShellCommands. The map may remain for UI
			// display hints, but it no longer affects the security path.
			if err := Authorize(ctx, guard.Action{Tool: "shell_run", Shell: a.Command}, argsJSON); err != nil {
				pushErrChunk(ch, err)
				return
			}
			if strings.Contains(a.Command, "../") || strings.Contains(a.Command, `..\`) {
				pushErrChunk(ch, fmt.Errorf("'..' path traversal is not allowed; use paths relative to the work root"))
				return
			}
```

保留原 `safeShellCommands` map（仅作为 display-hint；Task 18 的 TUI 可读取它做友好渲染，不再影响安全路径）。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/tools/ -run TestShellRun -v`

Expected: PASS；现有 `TestShellRun`（go version、streaming、metachar、allowlist、timeout、missing profile）全部仍通过；新测试证明 safe list 不再绕过 Authorize。

- [ ] **Step 5: 提交**

```bash
git add internal/tools/shell.go internal/tools/shell_test.go
git commit -m "fix(tools): shell_run always Authorizes; safe-list demoted to display hint"
```

---

## Task 4: execpolicy 字节索引 lexer（正确消费 `&&`/`||`/`>>`/fd redirect）

**Files:**
- Create: `internal/execpolicy/lexer.go`
- Create: `internal/execpolicy/lexer_test.go`

- [ ] **Step 1: 写失败测试（操作符消费和绕过回归集）**

```go
package execpolicy

import "testing"

func TestLexConsumesMultiByteOperatorsExactlyOnce(t *testing.T) {
	got, err := Lex(`go test ./... && printf ok || cat x >> out 2>>err`)
	if err != nil {
		t.Fatal(err)
	}
	want := []Token{
		{Kind: Word, Text: "go"}, {Kind: Word, Text: "test"}, {Kind: Word, Text: "./..."},
		{Kind: AndIf, Text: "&&"}, {Kind: Word, Text: "printf"}, {Kind: Word, Text: "ok"},
		{Kind: OrIf, Text: "||"}, {Kind: Word, Text: "cat"}, {Kind: Word, Text: "x"},
		{Kind: Redirect, Text: ">>"}, {Kind: Word, Text: "out"},
		{Kind: Redirect, Text: "2>>"}, {Kind: Word, Text: "err"},
	}
	if len(got) != len(want) {
		t.Fatalf("tokens=%#v, want=%#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("token[%d]=%#v, want=%#v", i, got[i], want[i])
		}
	}
}

func TestLexRejectsExpansionAndGlobBypasses(t *testing.T) {
	for _, raw := range []string{
		`printf $IFS`, `printf ${IFS}`, `printf $VAR`, `printf $(id)`,
		"printf `id`", `printf %PATH%`, `cat *.go`, `cat file?.go`,
	} {
		if _, err := Lex(raw); err == nil {
			t.Fatalf("Lex(%q) must reject expansion/glob syntax", raw)
		}
	}
}

func TestLexAcceptsQuotedWordsAbsolutePathsAndWindowsExeCase(t *testing.T) {
	for _, raw := range []string{
		`go test ./...`, `/usr/bin/go test ./...`, `C:\\Go\\bin\\GO.EXE version`,
		`printf "hello world"`, `printf 'hello world'`,
	} {
		if _, err := Lex(raw); err != nil {
			t.Fatalf("Lex(%q): %v", raw, err)
		}
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/execpolicy/ -run TestLex -v`

Expected: FAIL — package/`Lex`/`Token` 不存在。

- [ ] **Step 3: 实现完整 lexer.go（使用 byte index，不用 range index）**

```go
// Package execpolicy parses a deliberately small shell-command subset for
// policy evaluation. It is NOT a shell: unsupported expansion/glob syntax is
// rejected fail-closed before any process is created.
package execpolicy

import (
	"fmt"
	"strings"
	"unicode"
)

type TokenKind uint8

const (
	Word TokenKind = iota
	Pipe
	AndIf
	OrIf
	Redirect
)

type Token struct {
	Kind TokenKind
	Text string
}

func Lex(raw string) ([]Token, error) {
	var tokens []Token
	var word strings.Builder
	quote := byte(0)
	flushWord := func() {
		if word.Len() != 0 {
			tokens = append(tokens, Token{Kind: Word, Text: word.String()})
			word.Reset()
		}
	}

	for i := 0; i < len(raw); {
		c := raw[i]
		if quote != 0 {
			if c == quote {
				quote = 0
				i++
				continue
			}
			if c == '\\' && quote == '"' {
				if i+1 >= len(raw) {
					return nil, fmt.Errorf("execpolicy: trailing escape")
				}
				word.WriteByte(raw[i+1])
				i += 2
				continue
			}
			if forbiddenExpansionAt(raw, i) {
				return nil, fmt.Errorf("execpolicy: expansion/glob syntax rejected at byte %d", i)
			}
			word.WriteByte(c)
			i++
			continue
		}

		if unicode.IsSpace(rune(c)) {
			flushWord()
			i++
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
			i++
			continue
		case '\\':
			if i+1 >= len(raw) {
				return nil, fmt.Errorf("execpolicy: trailing escape")
			}
			word.WriteByte(raw[i+1])
			i += 2
			continue
		case '&':
			flushWord()
			if i+1 < len(raw) && raw[i+1] == '&' {
				tokens = append(tokens, Token{Kind: AndIf, Text: "&&"})
				i += 2
				continue
			}
			return nil, fmt.Errorf("execpolicy: standalone & rejected")
		case '|':
			flushWord()
			if i+1 < len(raw) && raw[i+1] == '|' {
				tokens = append(tokens, Token{Kind: OrIf, Text: "||"})
				i += 2
				continue
			}
			tokens = append(tokens, Token{Kind: Pipe, Text: "|"})
			i++
			continue
		case '>', '<':
			fd := ""
			if isDigits(word.String()) {
				fd = word.String()
				word.Reset()
			} else {
				flushWord()
			}
			op := string(c)
			i++
			if i < len(raw) && raw[i] == c {
				op += string(c)
				i++
			}
			if i < len(raw) && raw[i] == '&' {
				op += "&"
				i++
				start := i
				for i < len(raw) && raw[i] >= '0' && raw[i] <= '9' {
					i++
				}
				op += raw[start:i]
			}
			tokens = append(tokens, Token{Kind: Redirect, Text: fd + op})
			continue
		}
		if forbiddenExpansionAt(raw, i) {
			return nil, fmt.Errorf("execpolicy: expansion/glob syntax rejected at byte %d", i)
		}
		word.WriteByte(c)
		i++
	}
	if quote != 0 {
		return nil, fmt.Errorf("execpolicy: unterminated quote")
	}
	flushWord()
	if len(tokens) == 0 {
		return nil, fmt.Errorf("execpolicy: empty command")
	}
	return tokens, nil
}

func forbiddenExpansionAt(raw string, i int) bool {
	if i >= len(raw) {
		return false
	}
	switch raw[i] {
	case '$', '`', '*', '?', '[':
		return true
	case '%':
		return strings.IndexByte(raw[i+1:], '%') >= 0
	}
	return false
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/execpolicy/ -run TestLex -v`

Expected: PASS；`&&`/`||`/`>>` 各自产生一个 token；`2>>` 不被拆成 `2` 和两个 `>`；扩展/glob 回归全部拒绝。

- [ ] **Step 5: 提交**

```bash
git add internal/execpolicy/lexer.go internal/execpolicy/lexer_test.go
git commit -m "feat(execpolicy): byte-index lexer with fail-closed expansion rejection"
```

---

## Task 5: execpolicy parser + explainable rule evaluator（修正 deny rule 语义）

**Files:**
- Create: `internal/execpolicy/parser.go`
- Create: `internal/execpolicy/policy.go`
- Create: `internal/execpolicy/policy_test.go`

- [ ] **Step 1: 写失败测试（segment、RuleID、deny-flags-only）**

```go
package execpolicy

import "testing"

func TestEvaluateDenyRuleOnlyAppliesWhenDenyFlagMatches(t *testing.T) {
	cmd, err := Parse(`go test ./...`)
	if err != nil {
		t.Fatal(err)
	}
	rules := []Rule{
		{ID: "no-real-e2e", Program: "go", Prefix: []string{"test"}, Decision: "deny", DenyFlags: []string{"-tags=e2e_real"}, Justification: "real E2E requires explicit operator approval"},
		{ID: "go-test", Program: "go", Prefix: []string{"test"}, Decision: "allow", Justification: "ordinary Go tests are safe"},
	}
	got := Evaluate(cmd, rules)
	if got.Verdict != "allow" || got.RuleID != "go-test" {
		t.Fatalf("ordinary go test must not be denied by no-real-e2e rule: %#v", got)
	}
}

func TestEvaluateDenyFlagReturnsRuleID(t *testing.T) {
	cmd, err := Parse(`go test -tags=e2e_real ./internal/acp/...`)
	if err != nil {
		t.Fatal(err)
	}
	got := Evaluate(cmd, []Rule{{ID: "no-real-e2e", Program: "go", Prefix: []string{"test"}, Decision: "deny", DenyFlags: []string{"-tags=e2e_real"}, Justification: "real E2E is gated"}})
	if got.Verdict != "hard_deny" || got.RuleID != "no-real-e2e" || got.Justification == "" {
		t.Fatalf("deny result must carry RuleID/Justification: %#v", got)
	}
}

func TestParsePipelineAndRedirects(t *testing.T) {
	cmd, err := Parse(`printf ok | cat >> out 2>&1`)
	if err != nil {
		t.Fatal(err)
	}
	if len(cmd.Segments) != 2 || len(cmd.Segments[1].Redirects) != 2 {
		t.Fatalf("parsed command = %#v", cmd)
	}
}

func TestProgramNormalizationAbsoluteAndWindowsExe(t *testing.T) {
	cases := map[string]string{
		`/usr/bin/go test`: "go",
		`C:\\Go\\bin\\GO.EXE version`: "go",
	}
	for raw, want := range cases {
		cmd, err := Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := cmd.Segments[0].Program; got != want {
			t.Fatalf("Parse(%q) program=%q, want=%q", raw, got, want)
		}
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/execpolicy/ -run 'Evaluate|Parse|ProgramNormalization' -v`

Expected: FAIL — `Parse`/`Rule`/`Evaluate` 不存在。

- [ ] **Step 3: 实现完整 parser.go 与 policy.go**

`internal/execpolicy/parser.go`：

```go
package execpolicy

import (
	"fmt"
	"path/filepath"
	"strings"
)

type RedirectSpec struct {
	Operator string
	Target   string
}

type Segment struct {
	Program   string
	Args      []string
	Redirects []RedirectSpec
}

type Command struct {
	Segments []Segment
	Control  TokenKind
}

func Parse(raw string) (Command, error) {
	tokens, err := Lex(raw)
	if err != nil {
		return Command{}, err
	}
	var cmd Command
	var seg Segment
	flushSegment := func() error {
		if seg.Program == "" {
			return fmt.Errorf("execpolicy: operator without executable segment")
		}
		cmd.Segments = append(cmd.Segments, seg)
		seg = Segment{}
		return nil
	}
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		switch tok.Kind {
		case Word:
			if seg.Program == "" {
				seg.Program = normalizeProgram(tok.Text)
			} else {
				seg.Args = append(seg.Args, tok.Text)
			}
		case Redirect:
			if seg.Program == "" {
				return Command{}, fmt.Errorf("execpolicy: redirect before executable")
			}
			target := ""
			if !strings.Contains(tok.Text, "&") {
				if i+1 >= len(tokens) || tokens[i+1].Kind != Word {
					return Command{}, fmt.Errorf("execpolicy: redirect %q missing target", tok.Text)
				}
				i++
				target = tokens[i].Text
			}
			seg.Redirects = append(seg.Redirects, RedirectSpec{Operator: tok.Text, Target: target})
		case Pipe:
			if err := flushSegment(); err != nil {
				return Command{}, err
			}
		case AndIf, OrIf:
			if err := flushSegment(); err != nil {
				return Command{}, err
			}
			cmd.Control = tok.Kind
			// &&/|| are recognized structurally but A1 guard keeps the hard
			// metacharacter deny; parsing them does NOT make them executable.
		default:
			return Command{}, fmt.Errorf("execpolicy: unsupported token %q", tok.Text)
		}
	}
	if err := flushSegment(); err != nil {
		return Command{}, err
	}
	return cmd, nil
}

func normalizeProgram(raw string) string {
	clean := strings.ReplaceAll(raw, "\\", "/")
	name := filepath.Base(clean)
	name = strings.TrimSuffix(strings.ToLower(name), ".exe")
	return name
}
```

`internal/execpolicy/policy.go`：

```go
package execpolicy

import "strings"

type Rule struct {
	ID            string   `yaml:"id" json:"id"`
	Program       string   `yaml:"program" json:"program"`
	Prefix        []string `yaml:"prefix" json:"prefix"`
	Decision      string   `yaml:"decision" json:"decision"`
	DenyFlags     []string `yaml:"deny_flags" json:"deny_flags"`
	Justification string   `yaml:"justification" json:"justification"`
}

type Result struct {
	Verdict       string
	RuleID        string
	Justification string
	MatchedPrefix []string
	Reason        string
}

func Evaluate(cmd Command, rules []Rule) Result {
	if len(cmd.Segments) == 0 {
		return hard("empty-command", "no executable segment", "")
	}
	if cmd.Control == AndIf || cmd.Control == OrIf {
		return hard("control-token", "&&/|| are parsed but not executable in A1", "")
	}
	var overall Result
	for _, seg := range cmd.Segments {
		matched := false
		var best Result
		for _, rule := range rules {
			if normalizeProgram(rule.Program) != seg.Program || !hasPrefix(seg.Args, rule.Prefix) {
				continue
			}
			decision := strings.ToLower(rule.Decision)
			if decision == "deny" {
				// Critical semantic: a deny rule with DenyFlags is conditional.
				// If no flag matches, CONTINUE so a later allow rule can admit
				// ordinary `go test` while `-tags=e2e_real` is denied.
				if !containsAny(seg.Args, rule.DenyFlags) {
					continue
				}
				return hard(rule.ID, "deny flag matched", rule.Justification)
			}
			matched = true
			candidate := Result{
				Verdict:       decision,
				RuleID:        rule.ID,
				Justification: rule.Justification,
				MatchedPrefix: append([]string{seg.Program}, rule.Prefix...),
				Reason:        rule.Justification,
			}
			switch decision {
			case "allow", "prompt":
			default:
				return hard(rule.ID, "unknown execpolicy verdict", rule.Justification)
			}
			if best.RuleID == "" || (candidate.Verdict == "prompt" && best.Verdict == "allow") {
				best = candidate
			}
		}
		if !matched {
			return hard("unmatched-segment", "an executable segment has no matching allow/prompt rule", "")
		}
		if overall.RuleID == "" || (best.Verdict == "prompt" && overall.Verdict == "allow") {
			overall = best
		}
	}
	return overall
}

func hard(ruleID, reason, justification string) Result {
	return Result{Verdict: "hard_deny", RuleID: ruleID, Reason: reason, Justification: justification}
}

func hasPrefix(args, prefix []string) bool {
	if len(prefix) > len(args) {
		return false
	}
	for i := range prefix {
		if args[i] != prefix[i] {
			return false
		}
	}
	return true
}

func containsAny(args, flags []string) bool {
	if len(flags) == 0 {
		return false
	}
	for _, arg := range args {
		for _, flag := range flags {
			if arg == flag || strings.HasPrefix(arg, flag+"=") {
				return true
			}
		}
	}
	return false
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/execpolicy/ -v`

Expected: PASS；ordinary `go test` 命中 `go-test`；只有 `-tags=e2e_real` 命中 no-real-e2e；每个 deny/unknown/unmatched 都携带 RuleID。

- [ ] **Step 5: 提交**

```bash
git add internal/execpolicy/parser.go internal/execpolicy/policy.go internal/execpolicy/policy_test.go
git commit -m "feat(execpolicy): parse segments and evaluate explainable prefix rules"
```

---

## Task 6: 把 execpolicy 接到 Guard（解析失败/deny/unknown 均 HardDeny）

**Files:**
- Modify: `internal/guard/profile.go`
- Modify: `internal/guard/guard.go`
- Modify: `internal/guard/guard_test.go`
- Modify: `internal/config/config_test.go`

- [ ] **Step 1: 写失败测试（execpolicy verdict 映射和 legacy YAML 兼容）**

```go
package guard

import (
	"testing"

	"github.com/x6nux/yanshi/internal/execpolicy"
)

func TestGuardExecPolicyMapsRuleIDAndHardDeny(t *testing.T) {
	p := PermissionProfile{
		Tools: ToolsPerm{Allow: []string{"shell_run"}},
		Shell: ShellPerm{Rules: []execpolicy.Rule{
			{ID: "go-test", Program: "go", Prefix: []string{"test"}, Decision: "allow", Justification: "ordinary tests"},
			{ID: "no-real", Program: "go", Prefix: []string{"test"}, Decision: "deny", DenyFlags: []string{"-tags=e2e_real"}, Justification: "real E2E gated"},
		}},
	}
	allow := New().Check(p, Action{Tool: "shell_run", Shell: "go test ./internal/tools"})
	if allow.Verdict != Allow || allow.RuleID != "go-test" {
		t.Fatalf("allow=%#v", allow)
	}
	deny := New().Check(p, Action{Tool: "shell_run", Shell: "go test -tags=e2e_real ./internal/acp"})
	if deny.Verdict != HardDeny || deny.RuleID != "no-real" || deny.Promptable {
		t.Fatalf("deny=%#v", deny)
	}
}

func TestGuardExecPolicyParserFailureIsHardDeny(t *testing.T) {
	p := PermissionProfile{Tools: ToolsPerm{Allow: []string{"shell_run"}}, Shell: ShellPerm{Rules: []execpolicy.Rule{{ID: "printf", Program: "printf", Decision: "allow"}}}}
	dec := New().Check(p, Action{Tool: "shell_run", Shell: `printf ${IFS}`})
	if dec.Verdict != HardDeny || dec.RuleID != "parse-error" {
		t.Fatalf("parse failure=%#v", dec)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/guard/ ./internal/config/ -run 'GuardExecPolicy|Profile' -v`

Expected: FAIL — `ShellPerm.Rules` 不存在，Guard 未调用 execpolicy。

- [ ] **Step 3: 实现 profile 与 checkShell 分支**

`internal/guard/profile.go`：

```go
package guard

import "github.com/x6nux/yanshi/internal/execpolicy"

type ShellPerm struct {
	Policy   string            `yaml:"policy"`
	Patterns []string          `yaml:"patterns"`
	Rules    []execpolicy.Rule `yaml:"rules"`
}
```

保留 `PermissionProfile`/`FSPerm`/`ToolsPerm`/`NetPerm` 其余定义原样。execpolicy 块插入位置在 `checkShell` 的 **structural 元字符 HardDeny 之后**（元字符是 `&&`/`||`/`;`/`|`/`>`/`<` 等，已在 Task 1 的 for-loop 里无条件 HardDeny，execpolicy 必须晚于它）。关键修正（v3 L1）：execpolicy `allow` 命中后 **短路返回 Allow**，不再 fall-through 到 legacy `Policy`/`Patterns` 开关——否则一个只设 `Shell.Rules`、没有 `Policy`/`Patterns` 的 profile 会让 `go test`（已被 execpolicy allow）落入空 allowlist 变成 Prompt，使 `TestGuardExecPolicyMapsRuleIDAndHardDeny` 失败。插入：

```go
	if len(p.Shell.Rules) > 0 {
		cmd, err := execpolicy.Parse(a.Shell)
		if err != nil {
			return Decision{Verdict: HardDeny, RuleID: "parse-error", Reason: err.Error(), Justification: "execpolicy parser rejected unsupported shell syntax", Promptable: false}
		}
		result := execpolicy.Evaluate(cmd, p.Shell.Rules)
		switch result.Verdict {
		case "allow":
			// Metacharacter defense already ran above (structural HardDeny), so
			// an execpolicy "allow" is safe to honor directly. Short-circuit
			// Allow carrying RuleID/Justification — do NOT fall through to the
			// legacy Policy/Patterns switch, or a Rules-only profile would turn
			// every allowed command into a Prompt (empty allowlist). The
			// `go test | tee out` case never reaches here: the pipe metachar is
			// HardDenied before execpolicy runs.
			return Decision{Verdict: Allow, RuleID: result.RuleID, Reason: result.Reason, Justification: result.Justification, Promptable: false}
		case "prompt":
			return Decision{Verdict: Prompt, RuleID: result.RuleID, Reason: result.Reason, Justification: result.Justification, Promptable: true}
		case "hard_deny", "deny":
			return Decision{Verdict: HardDeny, RuleID: result.RuleID, Reason: result.Reason, Justification: result.Justification, Promptable: false}
		default:
			return Decision{Verdict: HardDeny, RuleID: result.RuleID, Reason: "unknown execpolicy verdict", Justification: result.Justification, Promptable: false}
		}
	}
```

由此 execpolicy `allow` 直接返回 Allow（带 RuleID），`go test | tee out` 因 `|` 在更早的元字符 HardDeny 被拒（parser 能解释但不可执行），旧 YAML 只有 `policy/patterns` 时 `Rules` 零值、整个块跳过、不改变现有行为。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/guard/ ./internal/config/ -v`

Expected: PASS；legacy profile YAML 可加载；execpolicy RuleID/Justification 进入 guard.Decision；管道/重定向仍不可执行。

- [ ] **Step 5: 提交**

```bash
git add internal/guard/profile.go internal/guard/guard.go internal/guard/guard_test.go internal/config/config_test.go
git commit -m "feat(guard): integrate execpolicy decisions without weakening metachar hard-deny"
```

---

## Task 7: approval manager（COW 持久化、session 隔离、audit emitter）

**Files:**
- Create: `internal/approval/types.go`
- Create: `internal/approval/manager.go`
- Create: `internal/approval/manager_test.go`

- [ ] **Step 1: 写失败测试（persist 失败无内存残留 + once session 隔离 + audit）**

```go
package approval

import (
	"errors"
	"testing"
	"time"
)

type fakeKV struct { value string; fail bool }
func (f *fakeKV) KVGet(string) (string, bool, error) { return f.value, f.value != "", nil }
func (f *fakeKV) KVSet(_ string, value string) error { if f.fail { return errors.New("disk full") }; f.value = value; return nil }

func TestRecordPersistentIsCopyOnWrite(t *testing.T) {
	kv := &fakeKV{fail: true}
	m, err := New(kv, "proc-1", nil)
	if err != nil { t.Fatal(err) }
	rule := Rule{ID: "r1", Action: "shell_run", Scope: Scope{Tool: "shell_run", Program: "go", Prefix: []string{"test"}}, TTL: TTLPersistent, Source: SourceUser}
	if err := m.Record("session-a", rule); err == nil { t.Fatal("expected persist failure") }
	if got := m.List("session-a", time.Now()); len(got) != 0 { t.Fatalf("failed persistence must leave no in-memory rule: %#v", got) }
}

func TestOnceRuleIsSessionIsolatedAndConsumed(t *testing.T) {
	m, err := New(&fakeKV{}, "proc-1", nil)
	if err != nil { t.Fatal(err) }
	scope := Scope{Tool: "shell_run", Program: "go", Prefix: []string{"test"}}
	if err := m.Record("session-a", Rule{ID: "r1", Action: "shell_run", Scope: scope, TTL: TTLOnce, Source: SourceUser}); err != nil { t.Fatal(err) }
	if hit, _ := m.Match("session-b", scope, time.Now()); hit { t.Fatal("once rule leaked across sessions") }
	if hit, _ := m.Match("session-a", scope, time.Now()); !hit { t.Fatal("session-a must hit") }
	if hit, _ := m.Match("session-a", scope, time.Now()); hit { t.Fatal("once rule must be consumed") }
}

func TestManagerEmitsHitMissConsumeExpireRevoke(t *testing.T) {
	var kinds []string
	m, err := New(&fakeKV{}, "proc-1", func(e AuditEvent) { kinds = append(kinds, e.Kind) })
	if err != nil { t.Fatal(err) }
	now := time.Now()
	scope := Scope{Tool: "web_fetch", Host: "example.com"}
	_, _ = m.Match("s", scope, now) // miss
	if err := m.Record("s", Rule{ID: "once", Action: "web_fetch", Scope: scope, TTL: TTLOnce, ExpiresAt: now.Add(time.Minute)}); err != nil { t.Fatal(err) }
	_, _ = m.Match("s", scope, now) // hit + consume
	if err := m.Record("s", Rule{ID: "expired", Action: "web_fetch", Scope: scope, TTL: TTLSession, ExpiresAt: now.Add(-time.Second)}); err != nil { t.Fatal(err) }
	_ = m.List("s", now) // expire
	if err := m.Record("s", Rule{ID: "revoke", Action: "web_fetch", Scope: scope, TTL: TTLSession}); err != nil { t.Fatal(err) }
	if err := m.Revoke("s", "revoke"); err != nil { t.Fatal(err) }
	for _, want := range []string{"miss", "hit", "consume", "expire", "revoke"} {
		if !contains(kinds, want) { t.Fatalf("missing audit %q in %v", want, kinds) }
	}
}

func TestAuditBusPublishSubscribeUnsubscribe(t *testing.T) {
	bus := NewAuditBus()
	ch := bus.Subscribe()
	want := AuditEvent{Kind: "hit", RuleID: "r1"}
	bus.Publish(want)
	select {
	case got := <-ch:
		if got.Kind != want.Kind || got.RuleID != want.RuleID { t.Fatalf("event=%#v", got) }
	case <-time.After(time.Second):
		t.Fatal("audit event was not published")
	}
	bus.Unsubscribe(ch)
	if _, ok := <-ch; ok { t.Fatal("Unsubscribe must close the subscription") }
}

func contains(items []string, want string) bool { for _, item := range items { if item == want { return true } }; return false }
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/approval/ -v`

Expected: FAIL — approval package 不存在。

- [ ] **Step 3: 实现完整 types.go 与 manager.go**

`internal/approval/types.go`：

```go
package approval

import "time"

type TTL string
const (
	TTLOnce TTL = "once"
	TTLSession TTL = "session"
	TTLPersistent TTL = "persistent"
)

type Source string
const (
	SourceUser Source = "user"
	SourceMode Source = "mode"
)

type Scope struct {
	Tool    string   `json:"tool"`
	Program string   `json:"program,omitempty"`
	Prefix  []string `json:"prefix,omitempty"`
	FSOp    string   `json:"fs_op,omitempty"`
	Paths   []string `json:"paths,omitempty"`
	Host    string   `json:"host,omitempty"`
}

type Rule struct {
	ID         string    `json:"id"`
	Action     string    `json:"action"`
	Scope      Scope     `json:"scope"`
	TTL        TTL       `json:"ttl"`
	Source     Source    `json:"source"`
	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  time.Time `json:"expires_at,omitempty"`
	ProcessID  string    `json:"process_instance_id,omitempty"`
}

type AuditEvent struct {
	Kind      string
	RuleID    string
	SessionID string
	Action    string
	Scope     Scope
	At        time.Time
}

type KV interface {
	KVGet(string) (string, bool, error)
	KVSet(string, string) error
}
```

`internal/approval/manager.go`：

```go
package approval

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"time"
)

const persistentKey = "security.approvals.v1"

type Manager struct {
	mu         sync.Mutex
	kv         KV
	processID  string
	persistent []Rule
	sessions   map[string][]Rule
	emit       func(AuditEvent)
}

func New(kv KV, processID string, emit func(AuditEvent)) (*Manager, error) {
	m := &Manager{kv: kv, processID: processID, sessions: make(map[string][]Rule), emit: emit}
	if kv == nil {
		return m, nil
	}
	raw, ok, err := kv.KVGet(persistentKey)
	if err != nil { return nil, fmt.Errorf("approval: load: %w", err) }
	if ok {
		if err := json.Unmarshal([]byte(raw), &m.persistent); err != nil {
			return nil, fmt.Errorf("approval: decode: %w", err)
		}
		for _, r := range m.persistent {
			if r.TTL != TTLPersistent {
				return nil, fmt.Errorf("approval: non-persistent rule %q found in persistent store", r.ID)
			}
		}
	}
	return m, nil
}

func (m *Manager) Match(sessionID string, scope Scope, now time.Time) (bool, *Rule) {
	m.mu.Lock()
	m.expireLocked(sessionID, now)
	// session rules first, then persistent. TTLOnce is consumed in-place.
	for i, r := range m.sessions[sessionID] {
		if reflect.DeepEqual(r.Scope, scope) {
			copyRule := r
			m.auditLocked("hit", sessionID, r, now)
			if r.TTL == TTLOnce {
				rules := append([]Rule(nil), m.sessions[sessionID]...)
				rules = append(rules[:i], rules[i+1:]...)
				m.sessions[sessionID] = rules
				m.auditLocked("consume", sessionID, r, now)
			}
			m.mu.Unlock()
			return true, &copyRule
		}
	}
	for _, r := range m.persistent {
		if reflect.DeepEqual(r.Scope, scope) {
			copyRule := r
			m.auditLocked("hit", sessionID, r, now)
			m.mu.Unlock()
			return true, &copyRule
		}
	}
	m.auditLocked("miss", sessionID, Rule{Scope: scope}, now)
	m.mu.Unlock()
	return false, nil
}

func (m *Manager) Record(sessionID string, rule Rule) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if rule.ID == "" || rule.Action == "" || rule.Scope.Tool == "" {
		return fmt.Errorf("approval: id/action/scope.tool are required")
	}
	if rule.CreatedAt.IsZero() { rule.CreatedAt = time.Now().UTC() }
	switch rule.TTL {
	case TTLOnce, TTLSession:
		if sessionID == "" { return fmt.Errorf("approval: session id required for %s", rule.TTL) }
		rule.ProcessID = m.processID
		copyRules := append([]Rule(nil), m.sessions[sessionID]...)
		m.sessions[sessionID] = append(copyRules, rule)
		return nil
	case TTLPersistent:
		copyRules := append([]Rule(nil), m.persistent...)
		copyRules = append(copyRules, rule)
		if err := m.persistLocked(copyRules); err != nil {
			return err // COW: m.persistent is unchanged
		}
		m.persistent = copyRules
		return nil
	default:
		return fmt.Errorf("approval: unknown ttl %q", rule.TTL)
	}
}

func (m *Manager) List(sessionID string, now time.Time) []Rule {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expireLocked(sessionID, now)
	out := append([]Rule(nil), m.sessions[sessionID]...)
	out = append(out, m.persistent...)
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

func (m *Manager) Revoke(sessionID, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if rules := m.sessions[sessionID]; len(rules) > 0 {
		for i, r := range rules {
			if r.ID == id {
				copyRules := append([]Rule(nil), rules...)
				copyRules = append(copyRules[:i], copyRules[i+1:]...)
				m.sessions[sessionID] = copyRules
				m.auditLocked("revoke", sessionID, r, time.Now())
				return nil
			}
		}
	}
	for i, r := range m.persistent {
		if r.ID == id {
			copyRules := append([]Rule(nil), m.persistent...)
			copyRules = append(copyRules[:i], copyRules[i+1:]...)
			if err := m.persistLocked(copyRules); err != nil { return err }
			m.persistent = copyRules
			m.auditLocked("revoke", sessionID, r, time.Now())
			return nil
		}
	}
	return fmt.Errorf("approval: rule %q not found", id)
}

func (m *Manager) expireLocked(sessionID string, now time.Time) {
	rules := m.sessions[sessionID]
	kept := make([]Rule, 0, len(rules))
	for _, r := range rules {
		if !r.ExpiresAt.IsZero() && !now.Before(r.ExpiresAt) {
			m.auditLocked("expire", sessionID, r, now)
			continue
		}
		kept = append(kept, r)
	}
	m.sessions[sessionID] = kept
}

func (m *Manager) persistLocked(rules []Rule) error {
	if m.kv == nil { return fmt.Errorf("approval: persistent store unavailable") }
	data, err := json.Marshal(rules)
	if err != nil { return fmt.Errorf("approval: encode: %w", err) }
	if err := m.kv.KVSet(persistentKey, string(data)); err != nil {
		return fmt.Errorf("approval: persist: %w", err)
	}
	return nil
}

func (m *Manager) auditLocked(kind, sessionID string, rule Rule, at time.Time) {
	if m.emit != nil {
		m.emit(AuditEvent{Kind: kind, RuleID: rule.ID, SessionID: sessionID, Action: rule.Action, Scope: rule.Scope, At: at})
	}
}

// AuditBus fans a single approval.Manager emit callback out to N subscribers
// (one per WS connection). The Manager knows nothing about transports; it just
// calls emit on every lifecycle event. bootstrap constructs one bus, hands
// bus.Publish as the emit callback to approval.New, and passes the bus to the
// HTTP server so each WS connection can Subscribe/Unsubscribe and render
// permission_rule_hit frames (Task 9). Without this, the WS layer has no
// thread-safe way to observe manager events (registering a per-connection
// callback on the manager would overwrite other connections' callbacks).
//
// CB9 (v3): this type is referenced by Task 9's WS audit pump; it MUST be
// defined here in the approval package.
type AuditBus struct {
	mu          sync.RWMutex
	subscribers map[<-chan AuditEvent]chan AuditEvent
}

func NewAuditBus() *AuditBus {
	return &AuditBus{subscribers: make(map[<-chan AuditEvent]chan AuditEvent)}
}

// Publish broadcasts e to every subscriber without blocking: each subscriber
// channel is buffered (64) and drops on full so a slow WS connection can never
// stall the approval manager's Match/Record/Revoke paths.
func (b *AuditBus) Publish(e AuditEvent) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, ch := range b.subscribers {
		select {
		case ch <- e:
		default:
		}
	}
}

// Subscribe returns a receive-only buffered channel receiving every subsequent
// AuditEvent. The caller MUST pass the same channel to Unsubscribe when its WS
// connection closes.
func (b *AuditBus) Subscribe() <-chan AuditEvent {
	writable := make(chan AuditEvent, 64)
	readonly := (<-chan AuditEvent)(writable)
	b.mu.Lock()
	b.subscribers[readonly] = writable
	b.mu.Unlock()
	return readonly
}

func (b *AuditBus) Unsubscribe(ch <-chan AuditEvent) {
	b.mu.Lock()
	writable, ok := b.subscribers[ch]
	if ok {
		delete(b.subscribers, ch)
		close(writable)
	}
	b.mu.Unlock()
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/approval/ -v`

Expected: PASS；persistent 写失败不留内存规则；once 不跨 session 且单次消费；persistent JSON 只含 TTLPersistent；五种 audit event 都发出；AuditBus publish/subscribe/unsubscribe 可用且 unsubscribe 关闭订阅。

- [ ] **Step 5: 提交**

```bash
git add internal/approval/types.go internal/approval/manager.go internal/approval/manager_test.go
git commit -m "feat(approval): session-scoped COW manager with auditable lifecycle"
```

---

## Task 8: tools.Authorize 只对 Prompt 查 approval/callback；HardDeny 永不覆盖

**Files:**
- Modify: `internal/tools/permctx.go`
- Modify: `internal/tools/permctx_test.go`
- Modify: `internal/api/http/ws.go`

- [ ] **Step 1: 写失败测试（YOLO/callback/approval 均不能覆盖 HardDeny）**

```go
package tools

import (
	"context"
	"testing"

	"github.com/x6nux/yanshi/internal/approval"
	"github.com/x6nux/yanshi/internal/guard"
)

func TestAuthorizeHardDenyNeverCallsCallback(t *testing.T) {
	called := false
	ctx := WithProfile(context.Background(), guard.PermissionProfile{}) // empty Tools.Allow => HardDeny
	ctx = WithPermissionCallback(ctx, func(PermissionRequest) PermissionDecision { called = true; return PermissionAllowPersistent })
	err := Authorize(ctx, guard.Action{Tool: "shell_run", Shell: "go test"}, `{"command":"go test"}`)
	if err == nil { t.Fatal("HardDeny must remain denied") }
	if called { t.Fatal("callback must not be invoked for HardDeny") }
}

func TestAuthorizePromptCanRecordPersistentRule(t *testing.T) {
	kv := &fakeApprovalKV{}
	manager, err := approval.New(kv, "proc-1", nil)
	if err != nil { t.Fatal(err) }
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"shell_run"}},
		Shell: guard.ShellPerm{Policy: "allowlist", Patterns: []string{"go version"}},
	})
	ctx = WithApprovalManager(ctx, manager, "session-a")
	ctx = WithPermissionCallback(ctx, func(PermissionRequest) PermissionDecision { return PermissionAllowPersistent })
	if err := Authorize(ctx, guard.Action{Tool: "shell_run", Shell: "go test ./..."}, `{"command":"go test ./..."}`); err != nil { t.Fatal(err) }
	if len(manager.List("session-a", time.Now())) != 1 { t.Fatal("persistent rule not recorded") }
}

type fakeApprovalKV struct{ value string }
func (f *fakeApprovalKV) KVGet(string) (string, bool, error) { return f.value, f.value != "", nil }
func (f *fakeApprovalKV) KVSet(_ string, value string) error { f.value = value; return nil }
```

测试文件 import 还要包含 `time`。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/tools/ -run 'AuthorizeHardDeny|AuthorizePromptCanRecord' -v`

Expected: FAIL — 当前 callback 可覆盖任意 static denial；approval context API/allow_persistent 不存在。

- [ ] **Step 3: 实现完整 context API 与 Authorize verdict switch**

在 `internal/tools/permctx.go` imports 增加 `fmt`、`time`、`github.com/x6nux/yanshi/internal/approval`、`github.com/x6nux/yanshi/internal/execpolicy`。新增：

```go
const (
	PermissionAllowSession    PermissionDecision = "allow_session"
	PermissionAllowPersistent PermissionDecision = "allow_persistent"
)

type approvalContext struct {
	Manager   *approval.Manager
	SessionID string
}
type approvalManagerKey struct{}

func WithApprovalManager(ctx context.Context, manager *approval.Manager, sessionID string) context.Context {
	if manager == nil || sessionID == "" { return ctx }
	return context.WithValue(ctx, approvalManagerKey{}, approvalContext{Manager: manager, SessionID: sessionID})
}

func approvalFromContext(ctx context.Context) (approvalContext, bool) {
	v, ok := ctx.Value(approvalManagerKey{}).(approvalContext)
	return v, ok && v.Manager != nil && v.SessionID != ""
}

func scopeFromAction(action guard.Action) (approval.Scope, error) {
	scope := approval.Scope{Tool: action.Tool, FSOp: action.FS.Op, Paths: append([]string(nil), action.FS.Paths...), Host: strings.ToLower(action.NetHost)}
	if action.Shell == "" { return scope, nil }
	cmd, err := execpolicy.Parse(action.Shell)
	if err != nil { return approval.Scope{}, err }
	if len(cmd.Segments) != 1 { return approval.Scope{}, fmt.Errorf("approval: shell scope requires one executable segment") }
	scope.Program = cmd.Segments[0].Program
	scope.Prefix = append([]string(nil), cmd.Segments[0].Args...)
	return scope, nil
}
```

把 `Authorize` 完整替换为：

```go
func Authorize(ctx context.Context, action guard.Action, argsJSON string) error {
	prof, ok := ProfileFromContext(ctx)
	if !ok {
		return &DenyErr{Reason: "no permission profile in context"}
	}
	dec := guard.New().Check(prof, action)
	switch dec.Verdict {
	case guard.Allow:
		return nil
	case guard.HardDeny:
		// Security firewall: approval manager, callback and mode resolution are
		// deliberately skipped. YOLO/auto are implemented inside the callback,
		// so they cannot observe, much less override, this branch.
		return &DenyErr{Reason: dec.Reason}
	case guard.Prompt:
		if !dec.Promptable {
			return &DenyErr{Reason: "guard returned Prompt without Promptable"}
		}
	default:
		return &DenyErr{Reason: "unknown guard verdict"}
	}

	scope, err := scopeFromAction(action)
	if err != nil {
		return &DenyErr{Reason: "approval scope: " + err.Error()}
	}
	if ac, ok := approvalFromContext(ctx); ok {
		if hit, _ := ac.Manager.Match(ac.SessionID, scope, time.Now()); hit {
			return nil
		}
	}

	ask, hasCallback := permissionCallback(ctx)
	if !hasCallback {
		return &DenyErr{Reason: dec.Reason}
	}
	decision := ask(PermissionRequest{Tool: action.Tool, Args: argsJSON, Reason: dec.Reason})
	switch decision {
	case PermissionAllow:
		return nil
	case PermissionAlwaysAllow, PermissionAllowSession:
		if ac, ok := approvalFromContext(ctx); ok {
			rule := approval.Rule{ID: newApprovalID(), Action: action.Tool, Scope: scope, TTL: approval.TTLSession, Source: approval.SourceUser}
			if err := ac.Manager.Record(ac.SessionID, rule); err != nil { return &DenyErr{Reason: err.Error()} }
			return nil
		}
		return &DenyErr{Reason: "approval manager unavailable"}
	case PermissionAllowPersistent:
		if ac, ok := approvalFromContext(ctx); ok {
			rule := approval.Rule{ID: newApprovalID(), Action: action.Tool, Scope: scope, TTL: approval.TTLPersistent, Source: approval.SourceUser}
			if err := ac.Manager.Record(ac.SessionID, rule); err != nil { return &DenyErr{Reason: err.Error()} }
			return nil
		}
		return &DenyErr{Reason: "persistent approval store unavailable"}
	default:
		return &DenyErr{Reason: dec.Reason}
	}
}

func newApprovalID() string {
	return fmt.Sprintf("approval-%d", time.Now().UnixNano())
}
```

删除旧 `sessionAllowlist`/`allowKey`/`WithPermissionAllowlist` 分支；WS connection context 改为 `tools.WithApprovalManager(connCtx, s.approvals, connectionSessionID)`（Task 9 定义 `s.approvals` 和稳定 connectionSessionID）。`resolvePermissionMode` 仍只返回 `PermissionAllow` 或 Deny；**禁止 mode 自动返回 AllowSession/Persistent**。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/tools/ ./internal/api/http/ -run 'Authorize|Permission' -v`

Expected: PASS；HardDeny 不调用 callback；Prompt 才可命中 approval；persistent 写失败返回 DenyErr；mode 不写规则。

- [ ] **Step 5: 提交**

```bash
git add internal/tools/permctx.go internal/tools/permctx_test.go internal/api/http/ws.go
git commit -m "feat(tools): gate approvals behind Prompt and make HardDeny non-overridable"
```

---

## Task 9: permission proto/WS/CLI/TUI 完整链路（含 `permission_rule_hit`）

**Files:**
- Modify: `internal/proto/frame.go`
- Modify: `internal/proto/frame_test.go`
- Modify: `internal/api/http/server.go`
- Modify: `internal/api/http/ws.go`
- Modify: `internal/api/http/chat.go`
- Create: `internal/api/http/ws_perm_test.go`
- Modify: `internal/cli/backend.go`
- Modify: `internal/cli/wsbackend.go`
- Modify: `internal/cli/wsbackend_test.go`
- Modify: `internal/cli/tui/permissions.go`
- Modify: `internal/cli/tui/commands.go`
- Modify: `internal/cli/tui/model.go`
- Modify: `internal/cli/tui/commands_test.go`
- Modify: `internal/bootstrap/bootstrap.go`

- [ ] **Step 1: 写失败测试（allow_persistent + list/revoke + CLI seam）**

```go
package proto

import (
	"encoding/json"
	"testing"
)

func TestPermissionFramesUseExistingIDAndSnakeCase(t *testing.T) {
	resp := NewPermissionResponse("req-1", "allow_persistent")
	data, err := json.Marshal(resp)
	if err != nil { t.Fatal(err) }
	if string(data) != `{"type":"permission_response","id":"req-1","decision":"allow_persistent"}` { t.Fatalf("response=%s", data) }
	info := PermissionInfo{ID: "r1", Action: "shell_run", Scope: "go test", TTL: "persistent", Source: "user", CreatedAt: 1}
	frame := NewPermissions([]PermissionInfo{info})
	if frame.Type != "permissions" || len(frame.Permissions) != 1 { t.Fatalf("frame=%#v", frame) }
	if NewRevokePermission("r1").ID != "r1" { t.Fatal("revoke must reuse ClientFrame.ID") }
}
```

`internal/cli/wsbackend_test.go` 增加：

```go
func TestPermissionControlRepliesMapThroughCLI(t *testing.T) {
	f := proto.NewPermissions([]proto.PermissionInfo{{ID: "r1", Action: "shell_run"}})
	ev := toStreamEvent(f)
	if ev.Kind != "permissions" || len(ev.Permissions) != 1 || ev.Permissions[0].ID != "r1" { t.Fatalf("event=%#v", ev) }
	if !isControlReply("permissions") || !isControlReply("permission_rule_hit") || !isControlReply("session_ack") {
		t.Fatal("permission replies and existing session_ack must close control channel")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/proto/ ./internal/api/http/ ./internal/cli/ ./internal/cli/tui/ -run 'PermissionFrames|PermissionControl|Permissions' -v`

Expected: FAIL — PermissionInfo/frames/StreamEvent.Permissions/TUI persistent option/`/permissions` 不存在。

- [ ] **Step 3: 实现完整协议、WS 发射点与 CLI/TUI seam**

在 `internal/proto/frame.go` 新增：

```go
type PermissionInfo struct {
	ID        string `json:"id"`
	Action    string `json:"action"`
	Scope     string `json:"scope"`
	TTL       string `json:"ttl"`
	Source    string `json:"source"`
	CreatedAt int64  `json:"created_at"`
	ExpiresAt int64  `json:"expires_at,omitempty"`
}

func NewListPermissions() ClientFrame { return ClientFrame{Type: "permissions_list"} }
func NewRevokePermission(id string) ClientFrame { return ClientFrame{Type: "permission_revoke", ID: id} }
func NewPermissions(items []PermissionInfo) ServerFrame { return ServerFrame{Type: "permissions", Permissions: items} }
func NewPermissionRuleHit(ruleID, action, scope, result string) ServerFrame {
	return ServerFrame{Type: "permission_rule_hit", ID: ruleID, ToolName: action, Text: scope, Status: result}
}
```

在 `ServerFrame` 新增：

```go
Permissions []PermissionInfo `json:"permissions,omitempty"`
```

`ExpiresAt` 转换必须用 helper，零值返回 0（不能输出 `time.Time{}.Unix()` 的负数）：

```go
func unixOrZero(t time.Time) int64 {
	if t.IsZero() { return 0 }
	return t.Unix()
}
```

`internal/api/http/server.go` 的 Config/Server/New 增加 manager 与共享 audit bus：

```go
// Config fields
Approvals     *approval.Manager
ApprovalAudit *approval.AuditBus
// Server fields
approvals     *approval.Manager
approvalAudit *approval.AuditBus
// New initializer
approvals:     cfg.Approvals,
approvalAudit: cfg.ApprovalAudit,
```

WS connection 建立时生成与 conversation DB ID 独立的稳定连接 session ID：

```go
connectionSessionID := fmt.Sprintf("ws-%d", time.Now().UnixNano())
connCtx = tools.WithApprovalManager(connCtx, s.approvals, connectionSessionID)
```

在 WS `switch cf.Type` 增加完整分支：

```go
case "permissions_list":
	if s.approvals == nil {
		conn.write(proto.NewPermissions(nil))
		break
	}
	rules := s.approvals.List(connectionSessionID, time.Now())
	items := make([]proto.PermissionInfo, 0, len(rules))
	for _, rule := range rules {
		scopeJSON, _ := json.Marshal(rule.Scope)
		items = append(items, proto.PermissionInfo{ID: rule.ID, Action: rule.Action, Scope: string(scopeJSON), TTL: string(rule.TTL), Source: string(rule.Source), CreatedAt: unixOrZero(rule.CreatedAt), ExpiresAt: unixOrZero(rule.ExpiresAt)})
	}
	conn.write(proto.NewPermissions(items))
case "permission_revoke":
	if s.approvals == nil {
		conn.write(proto.NewError("approval manager unavailable"))
		break
	}
	if err := s.approvals.Revoke(connectionSessionID, cf.ID); err != nil {
		conn.write(proto.NewError(err.Error()))
		break
	}
	conn.write(proto.NewPermissionRuleHit(cf.ID, "", "", "revoke"))
```

真实 audit 发射点：bootstrap 构造 manager 时不能把 emitter 设 nil。Task 7 已具体定义 `approval.AuditBus`（`NewAuditBus`/`Publish`/`Subscribe() <-chan AuditEvent`/`Unsubscribe(ch)`）；bootstrap 创建**一个** bus，把 `bus.Publish` 作为 `approval.New` 的 emitter，并把同一指针经 `apihttp.Config.ApprovalAudit` 交给 Server。不能在 Manager 上注册 per-connection callback，否则新连接会覆盖旧连接。WS connection 建立后：

```go
if s.approvalAudit != nil {
	auditCh := s.approvalAudit.Subscribe()
	defer s.approvalAudit.Unsubscribe(auditCh)
	go func() {
		for {
			select {
			case <-connCtx.Done():
				return
			case event, ok := <-auditCh:
				if !ok { return }
				conn.write(proto.NewPermissionRuleHit(event.RuleID, event.Action, scopeJSON(event.Scope), event.Kind))
			}
		}
	}()
}
```

`scopeJSON(scope approval.Scope) string` 是 ws.go 小 helper：`json.Marshal(scope)` 成功时返回 JSON 字符串，失败时返回 `"{}"`（Scope 当前字段均可编码，因此失败只作 fail-safe）。`defer Unsubscribe` 关闭订阅 channel；goroutine 同时监听 `connCtx.Done()`，连接断开不会泄漏。

`internal/cli/backend.go` 的 `StreamEvent` 增加：

```go
Permissions []proto.PermissionInfo
```

`internal/cli/wsbackend.go` 完整差异（CB7：在新增 permission reply 时保留现有 `session_ack`，否则 Task 9 的中间 commit 会让 `/rename`/`/archive`/`/unarchive`/`/delete` 的 ack 卡住 control mode）：

```go
func isControlReply(kind string) bool {
	switch kind {
	case "models", "status", "mcp_list", "sessions", "session_restored", "session_ack", "permissions", "permission_rule_hit":
		return true
	}
	return false
}

// toStreamEvent return literal 内新增
Permissions: f.Permissions,
```

`internal/cli/tui/permissions.go`：

```go
var permOptions = []struct { label, decision string }{
	{"Allow once", "allow"},
	{"Allow this session", "allow_session"},
	{"Persistent allow", "allow_persistent"},
	{"Deny", "deny"},
}
```

`internal/cli/tui/commands.go` 的 `commandTable` 新增：

```go
{name: "permissions", help: "list / revoke approval rules", run: cmdPermissions},
```

并新增完整 handler/entry：

```go
func cmdPermissions(m model, args []string) (tea.Model, tea.Cmd) {
	if len(args) == 0 { return m.sendControlFrame(proto.NewListPermissions()) }
	if len(args) == 2 && args[0] == "revoke" { return m.sendControlFrame(proto.NewRevokePermission(args[1])) }
	m.entries = append(m.entries, errorEntry{text: "usage: /permissions [revoke <rule-id>]"})
	m.refresh()
	return m, nil
}

type permissionsEntry struct { items []proto.PermissionInfo }
func (e permissionsEntry) render(int) string {
	if len(e.items) == 0 { return "Permissions\n  (none)" }
	var b strings.Builder
	b.WriteString("Permissions\n")
	for _, item := range e.items {
		fmt.Fprintf(&b, "  %s  %s  %s  %s\n", item.ID, item.Action, item.TTL, item.Scope)
	}
	return strings.TrimSuffix(b.String(), "\n")
}
```

`internal/cli/tui/model.go` 的 `applyEvent` switch 增加完整 case：

```go
case "permissions":
	m.flushAssistant()
	m.entries = append(m.entries, permissionsEntry{items: ev.Permissions})
case "permission_rule_hit":
	m.flushAssistant()
	m.entries = append(m.entries, summaryEntry{text: fmt.Sprintf("permission rule %s: %s", ev.ID, ev.ToolStatus)})
```

`chat.go` 不增加交互分支；只继续通过已有 `writeSSEFrame` 序列化 ServerFrame。SSE 请求体不是 ClientFrame，因此不能 list/revoke，也不安装 callback。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/proto/ ./internal/api/http/ ./internal/cli/ ./internal/cli/tui/ ./internal/bootstrap/ -run 'Permission' -v`

Expected: PASS；allow_persistent 从 TUI popup→proto→WS→Authorize→manager；`/permissions` list/revoke 可用；hit/miss/consume/expire/revoke 都以 `permission_rule_hit` 真事件进入 CLI；ExpiresAt 零值为 0。

- [ ] **Step 5: 提交**

```bash
git add internal/proto/frame.go internal/proto/frame_test.go internal/api/http/server.go internal/api/http/ws.go internal/api/http/chat.go internal/api/http/ws_perm_test.go internal/cli/backend.go internal/cli/wsbackend.go internal/cli/wsbackend_test.go internal/cli/tui/permissions.go internal/cli/tui/commands.go internal/cli/tui/model.go internal/cli/tui/commands_test.go internal/bootstrap/bootstrap.go
git commit -m "feat(permissions): persistent approvals, audit events and complete transport/TUI seam"
```

---

# A1b — Sandbox + 网络策略 Phase 0 skeleton（诚实降级，不宣称隔离）

**诚实验收边界：** A1b 只交付 API、配置、capability probe、policy engine、HTTP proxy 与 adapter TDD 骨架。Windows/Linux/macOS adapter 在 A1b 内都返回 `Effective=host-guard-degraded`；不调用 Job Object/restricted token/Landlock/seccomp/Seatbelt/ConPTY 系统 API；因此 **A1b 不宣称已实现 OS 隔离或强制网络隔离**。真实 OS enforcement 必须在待决策 gate 关闭后的独立后续 phase 完成。

## Task 10: sandbox 三档抽象 + 四平台 Phase 0 adapter + `*bool` 配置

**Files:**
- Create: `internal/sandbox/types.go`
- Create: `internal/sandbox/factory.go`
- Create: `internal/sandbox/sandbox_windows.go`
- Create: `internal/sandbox/sandbox_linux.go`
- Create: `internal/sandbox/sandbox_darwin.go`
- Create: `internal/sandbox/sandbox_other.go`
- Create: `internal/sandbox/types_test.go`
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`
- Modify: `config.example.yaml`

- [ ] **Step 1: 写失败测试（Phase 0 必须显式 degraded）**

```go
package sandbox

import (
	"context"
	"os/exec"
	"testing"
)

func TestPhase0AdapterIsHonestDegraded(t *testing.T) {
	sb := New(Config{Enabled: true, WorkspaceRoot: t.TempDir(), Tier: WorkspaceWrite})
	report := sb.Report()
	if report.Effective != DegradedHostGuard || report.Enforced || report.CanKillTree {
		t.Fatalf("Phase 0 must report degraded/non-enforced/no-kill-tree: %#v", report)
	}
	cmd := exec.CommandContext(context.Background(), "does-not-run")
	if err := sb.Prepare(context.Background(), cmd, CommandSpec{Tier: WorkspaceWrite}); err != nil {
		t.Fatalf("degraded adapter must leave host-guard path usable: %v", err)
	}
}

func TestAccessTierOrdering(t *testing.T) {
	if !(ReadOnly < WorkspaceWrite && WorkspaceWrite < FullAccess) {
		t.Fatalf("tier ordering is wrong: %d %d %d", ReadOnly, WorkspaceWrite, FullAccess)
	}
}
```

`internal/config/config_test.go` 增加：

```go
func TestSecuritySandboxEnabledCanDistinguishUnsetAndFalse(t *testing.T) {
	path := writeConfig(t, `storage: {sqlite_path: ":memory:"}
security:
  sandbox:
    enabled: false
    tier: workspace-write
`)
	cfg, err := Load(path)
	if err != nil { t.Fatal(err) }
	if cfg.Security.Sandbox.Enabled == nil || *cfg.Security.Sandbox.Enabled { t.Fatalf("enabled=%v", cfg.Security.Sandbox.Enabled) }
	if cfg.Security.Sandbox.Tier != "workspace-write" { t.Fatalf("tier=%q", cfg.Security.Sandbox.Tier) }
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/sandbox/ ./internal/config/ -run 'Phase0|AccessTier|SecuritySandboxEnabled' -v`

Expected: FAIL — sandbox package与 SecurityConfig 不存在。

- [ ] **Step 3: 实现完整 sandbox 接口与平台文件**

`internal/sandbox/types.go`：

```go
package sandbox

import (
	"context"
	"os/exec"
)

type AccessTier uint8
const (
	ReadOnly AccessTier = iota
	WorkspaceWrite
	FullAccess
)

type EffectiveMode string
const (
	OSIsolated EffectiveMode = "os-isolated"
	DegradedHostGuard EffectiveMode = "host-guard-degraded"
	Disabled EffectiveMode = "disabled"
)

type CapabilityReport struct {
	Platform    string
	Requested   AccessTier
	Effective   EffectiveMode
	Backend     string
	Reason      string
	Enforced    bool
	CanKillTree bool
}

type Config struct {
	Enabled       bool
	WorkspaceRoot string
	Tier          AccessTier
	NetworkDeny   bool
	ProxyURL      string
}

type CommandSpec struct {
	Path string
	Args []string
	Dir  string
	Tier AccessTier
}

type Sandbox interface {
	Prepare(context.Context, *exec.Cmd, CommandSpec) error
	Report() CapabilityReport
	Close() error
}
```

`internal/sandbox/factory.go`：

```go
package sandbox

import (
	"context"
	"os/exec"
	"runtime"
)

type degraded struct { report CapabilityReport }
func (d *degraded) Prepare(context.Context, *exec.Cmd, CommandSpec) error { return nil }
func (d *degraded) Report() CapabilityReport { return d.report }
func (d *degraded) Close() error { return nil }

func New(cfg Config) Sandbox {
	if !cfg.Enabled {
		return &degraded{report: CapabilityReport{Platform: runtime.GOOS, Requested: cfg.Tier, Effective: Disabled, Backend: "none", Reason: "sandbox disabled by configuration", Enforced: false, CanKillTree: false}}
	}
	return newPlatformSandbox(cfg)
}

func phase0(cfg Config, backend, reason string) Sandbox {
	return &degraded{report: CapabilityReport{Platform: runtime.GOOS, Requested: cfg.Tier, Effective: DegradedHostGuard, Backend: backend, Reason: reason, Enforced: false, CanKillTree: false}}
}
```

四个平台文件是完整的 capability skeleton（不含 syscall）：

```go
// internal/sandbox/sandbox_windows.go
//go:build windows
package sandbox
func newPlatformSandbox(cfg Config) Sandbox { return phase0(cfg, "windows-selection-gate", "restricted token vs Job Object not decided; host guard only") }
```

```go
// internal/sandbox/sandbox_linux.go
//go:build linux
package sandbox
func newPlatformSandbox(cfg Config) Sandbox { return phase0(cfg, "linux-selection-gate", "Landlock ABI/bubblewrap/seccomp strategy not decided; host guard only") }
```

```go
// internal/sandbox/sandbox_darwin.go
//go:build darwin
package sandbox
func newPlatformSandbox(cfg Config) Sandbox { return phase0(cfg, "macos-selection-gate", "Seatbelt vs signed helper not decided; host guard only") }
```

```go
// internal/sandbox/sandbox_other.go
//go:build !windows && !linux && !darwin
package sandbox
func newPlatformSandbox(cfg Config) Sandbox { return phase0(cfg, "unsupported-platform", "no reviewed OS sandbox adapter; host guard only") }
```

`internal/config/config.go` 增加：

```go
type SecurityConfig struct {
	Sandbox SandboxConfig `yaml:"sandbox"`
	Network NetworkConfig `yaml:"network"`
	Shell   ShellRuntimeConfig `yaml:"shell"`
}

type SandboxConfig struct {
	Enabled *bool `yaml:"enabled"`
	Tier string `yaml:"tier"`
	NetworkDeny bool `yaml:"network_deny"`
}

type NetworkConfig struct {
	Default string `yaml:"default"`
	Allow []string `yaml:"allow"`
	Deny []string `yaml:"deny"`
	AllowPrivate bool `yaml:"allow_private"`
}

type ShellRuntimeConfig struct {
	MaxOutputBytes int `yaml:"max_output_bytes"`
	IdleTimeout time.Duration `yaml:"idle_timeout"`
}
```

在 `Config` 加 `Security SecurityConfig yaml:"security"`；在 `applyDefaults()` 加：

```go
	if c.Security.Sandbox.Enabled == nil {
		enabled := true
		c.Security.Sandbox.Enabled = &enabled
	}
	if c.Security.Sandbox.Tier == "" { c.Security.Sandbox.Tier = "read-only" }
	if c.Security.Shell.MaxOutputBytes == 0 { c.Security.Shell.MaxOutputBytes = 1 << 20 }
	if c.Security.Shell.IdleTimeout == 0 { c.Security.Shell.IdleTimeout = 30 * time.Minute }
```

**不要添加 `execpolicy_enabled`**：execpolicy 是否启用由 `profiles.*.shell.rules` 是否为空决定；死配置会造成声明与实际不一致。

`config.example.yaml`：

```yaml
security:
  sandbox:
    enabled: true
    tier: read-only
    network_deny: true
  network:
    default: deny
    allow: []
    deny: []
    allow_private: false
  shell:
    max_output_bytes: 1048576
    idle_timeout: 30m
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/sandbox/ ./internal/config/ -v`

Expected: PASS；所有平台 adapter Phase 0 都是 degraded/non-enforced/CanKillTree=false；enabled false 与未设置可区分；不存在 execpolicy_enabled。

- [ ] **Step 5: 提交**

```bash
git add internal/sandbox internal/config/config.go internal/config/config_test.go config.example.yaml
git commit -m "feat(sandbox): add honest Phase 0 access-tier capability skeleton"
```

---

## Task 11: 单一 netpolicy host-policy 源（deny-wins、DNS/private-IP re-check）

**Files:**
- Create: `internal/netpolicy/policy.go`
- Create: `internal/netpolicy/policy_test.go`

- [ ] **Step 1: 写失败测试**

```go
package netpolicy

import (
	"net"
	"testing"
)

func TestDefaultEmptyIsDenyAndDenyWins(t *testing.T) {
	p := Policy{Allow: []string{"api.example.com"}, Deny: []string{"api.example.com"}}
	if d := p.CheckHost("api.example.com"); d.Allowed { t.Fatalf("deny must win: %#v", d) }
	if d := (Policy{}).CheckHost("example.com"); d.Allowed { t.Fatalf("empty default must deny: %#v", d) }
}

func TestSubdomainPatternDoesNotMatchApex(t *testing.T) {
	p := Policy{Default: "deny", Allow: []string{".example.com"}}
	if p.CheckHost("example.com").Allowed { t.Fatal("scoped subdomain pattern must not match apex") }
	if !p.CheckHost("api.example.com").Allowed { t.Fatal("subdomain should match") }
}

func TestResolvedIPsRejectPrivateLoopbackLinkLocal(t *testing.T) {
	p := Policy{Default: "deny", Allow: []string{"allowed.example"}}
	for _, ip := range []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("10.0.0.1"), net.ParseIP("169.254.1.1"), net.ParseIP("::1")} {
		if d := p.CheckResolvedIPs("allowed.example", []net.IP{ip}); d.Allowed { t.Fatalf("private/local %s allowed: %#v", ip, d) }
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/netpolicy/ -run 'DefaultEmpty|Subdomain|ResolvedIPs' -v`

Expected: FAIL — netpolicy package 不存在。

- [ ] **Step 3: 实现完整 policy.go**

```go
package netpolicy

import (
	"fmt"
	"net"
	"strings"
)

type Decision struct { Allowed bool; Rule string; Reason string }

type Policy struct {
	Default string
	Allow []string
	Deny []string
	AllowPrivate bool
}

func (p Policy) CheckHost(raw string) Decision {
	host := normalizeHost(raw)
	if host == "" { return Decision{Reason: "empty host"} }
	for _, pattern := range p.Deny {
		if hostMatches(pattern, host) { return Decision{Rule: "deny:" + pattern, Reason: "host denied by deny rule"} }
	}
	for _, pattern := range p.Allow {
		if hostMatches(pattern, host) { return Decision{Allowed: true, Rule: "allow:" + pattern, Reason: "host allowed"} }
	}
	// Empty/unknown default is fail-closed.
	if strings.EqualFold(p.Default, "allow") { return Decision{Allowed: true, Rule: "default:allow", Reason: "host allowed by default"} }
	return Decision{Rule: "default:deny", Reason: "host denied by default"}
}

func (p Policy) CheckResolvedIPs(host string, ips []net.IP) Decision {
	if d := p.CheckHost(host); !d.Allowed { return d }
	if len(ips) == 0 { return Decision{Reason: "DNS returned no addresses"} }
	for _, ip := range ips {
		if ip == nil { return Decision{Reason: "DNS returned invalid address"} }
		if !p.AllowPrivate && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified()) {
			return Decision{Rule: "ip-range-deny", Reason: fmt.Sprintf("resolved address %s is private/local", ip)}
		}
	}
	return Decision{Allowed: true, Rule: "resolved-ip-check", Reason: "all resolved addresses allowed"}
}

func normalizeHost(raw string) string {
	host := strings.TrimSpace(strings.ToLower(raw))
	if h, _, err := net.SplitHostPort(host); err == nil { host = h }
	return strings.TrimSuffix(host, ".")
}

func hostMatches(pattern, host string) bool {
	pattern = normalizeHost(pattern)
	if pattern == "" { return false }
	if strings.HasPrefix(pattern, ".") {
		suffix := strings.TrimPrefix(pattern, ".")
		return host != suffix && strings.HasSuffix(host, "."+suffix)
	}
	return host == pattern
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/netpolicy/ -run 'DefaultEmpty|Subdomain|ResolvedIPs' -v`

Expected: PASS；deny-wins；Default=="" 强制 deny；`.example.com` 不匹配 apex；私网/loopback/link-local 复检拒绝。

- [ ] **Step 5: 提交**

```bash
git add internal/netpolicy/policy.go internal/netpolicy/policy_test.go
git commit -m "feat(netpolicy): deny-wins host and resolved-IP policy"
```

---

## Task 12: loopback proxy（自定义 DialContext pin IP、逐跳授权、转发 body）

**Files:**
- Create: `internal/netpolicy/proxy.go`
- Create: `internal/netpolicy/proxy_test.go`

- [ ] **Step 1: 写失败测试（env 清理 + body 转发 + pinned dial seam）**

```go
package netpolicy

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeResolver struct { ips []net.IPAddr }
func (f fakeResolver) LookupIPAddr(context.Context, string) ([]net.IPAddr, error) { return f.ips, nil }

func TestPrepareEnvRemovesInheritedProxyVariants(t *testing.T) {
	got := PrepareEnv([]string{"PATH=x", "http_proxy=evil", "HTTPS_PROXY=old", "no_proxy=*"}, "http://127.0.0.1:9000")
	joined := strings.Join(got, "\n")
	if strings.Contains(joined, "http_proxy=evil") || strings.Contains(joined, "HTTPS_PROXY=old") || strings.Contains(joined, "no_proxy=*") { t.Fatalf("inherited proxy vars remain: %v", got) }
	if !strings.Contains(joined, "HTTP_PROXY=http://127.0.0.1:9000") || !strings.Contains(joined, "HTTPS_PROXY=http://127.0.0.1:9000") { t.Fatalf("managed vars missing: %v", got) }
}

func TestProxyForwardsResponseBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "proxied-body") }))
	defer upstream.Close()
	p, err := NewProxy(Policy{Default: "allow", AllowPrivate: true}, fakeResolver{ips: []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}})
	if err != nil { t.Fatal(err) }
	defer p.Close()
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(p.URL())}}
	resp, err := client.Get(upstream.URL)
	if err != nil { t.Fatal(err) }
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "proxied-body" { t.Fatalf("body=%q", body) }
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/netpolicy/ -run 'PrepareEnv|ProxyForwards' -v`

Expected: FAIL — PrepareEnv/NewProxy 不存在。

- [ ] **Step 3: 实现完整 proxy.go**

```go
package netpolicy

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type Resolver interface { LookupIPAddr(context.Context, string) ([]net.IPAddr, error) }

// PolicyDialer is the shared host-policy dial path used by BOTH the loopback
// Proxy (Task 12) and web_fetch's direct HTTP transport (Task 13). It resolves
// the host, re-checks every resolved IP against CheckResolvedIPs (rejecting
// private/loopback/link-local), then pins the connection to the first allowed
// IP — closing the DNS-rebinding seam (net.Dialer would otherwise re-resolve
// the hostname). Factoring this into one type means ordinary web_fetch also
// runs resolve→CheckResolvedIPs→pin, not just the proxy path.
//
// CB8 (v3): Task 13 references netpolicy.NewTransport / PolicyDialer; both are
// defined here so the reference resolves.
type PolicyDialer struct {
	Policy   *Policy
	Resolver Resolver
}

func (d *PolicyDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	resolver := d.Resolver
	if resolver == nil { resolver = net.DefaultResolver }
	host, port, err := net.SplitHostPort(address)
	if err != nil { return nil, err }
	if d.Policy != nil {
		if dec := d.Policy.CheckHost(host); !dec.Allowed { return nil, fmt.Errorf("netpolicy: %s", dec.Reason) }
	}
	resolved, err := resolver.LookupIPAddr(ctx, host)
	if err != nil { return nil, err }
	ips := make([]net.IP, 0, len(resolved))
	for _, item := range resolved { ips = append(ips, item.IP) }
	if d.Policy != nil {
		if dec := d.Policy.CheckResolvedIPs(host, ips); !dec.Allowed { return nil, fmt.Errorf("netpolicy: %s", dec.Reason) }
	}
	if len(ips) == 0 { return nil, fmt.Errorf("netpolicy: no addresses resolved for %q", host) }
	// Pin to the exact IP that passed CheckResolvedIPs. Do not re-resolve via
	// net.Dialer with the hostname (DNS rebinding defense).
	pinned := net.JoinHostPort(ips[0].String(), port)
	return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, pinned)
}

// NewTransport returns an http.Transport whose DialContext is a PolicyDialer,
// so non-proxy HTTP clients (web_fetch) reuse the same host-policy +
// resolved-IP re-check + pinning as the loopback proxy. A nil policy disables
// the policy checks (test-only).
func NewTransport(policy *Policy) *http.Transport {
	return &http.Transport{
		DialContext:       (&PolicyDialer{Policy: policy, Resolver: net.DefaultResolver}).DialContext,
		ForceAttemptHTTP2: false,
	}
}

type Proxy struct {
	policy   Policy
	resolver Resolver
	dialer   *PolicyDialer
	listener net.Listener
	server   *http.Server
	client   *http.Client
}

func NewProxy(policy Policy, resolver Resolver) (*Proxy, error) {
	if resolver == nil { resolver = net.DefaultResolver }
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil { return nil, err }
	p := &Proxy{policy: policy, resolver: resolver, listener: ln}
	// Point the dialer at &p.policy (the struct field, not the local param) so
	// the pointer stays valid for the Proxy's whole lifetime.
	p.dialer = &PolicyDialer{Policy: &p.policy, Resolver: resolver}
	transport := &http.Transport{Proxy: nil, DialContext: p.dialer.DialContext, ForceAttemptHTTP2: false}
	p.client = &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	p.server = &http.Server{Handler: p, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = p.server.Serve(ln) }()
	return p, nil
}

func (p *Proxy) URL() *url.URL { u, _ := url.Parse("http://" + p.listener.Addr().String()); return u }
func (p *Proxy) Close() error { return p.server.Close() }

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect { http.Error(w, "CONNECT not enabled in Phase 0", http.StatusNotImplemented); return }
	if d := p.policy.CheckHost(r.URL.Hostname()); !d.Allowed { http.Error(w, d.Reason, http.StatusForbidden); return }
	out := r.Clone(r.Context())
	out.RequestURI = ""
	out.Header = r.Header.Clone()
	out.Header.Del("Proxy-Authorization")
	resp, err := p.client.Do(out)
	if err != nil { http.Error(w, err.Error(), http.StatusBadGateway); return }
	defer resp.Body.Close()
	for key, values := range resp.Header { for _, value := range values { w.Header().Add(key, value) } }
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func PrepareEnv(in []string, proxyURL string) []string {
	blocked := map[string]bool{"http_proxy": true, "https_proxy": true, "no_proxy": true, "all_proxy": true}
	out := make([]string, 0, len(in)+3)
	for _, item := range in {
		key := item
		if i := strings.IndexByte(key, '='); i >= 0 { key = key[:i] }
		if blocked[strings.ToLower(key)] { continue }
		out = append(out, item)
	}
	out = append(out, "HTTP_PROXY="+proxyURL, "HTTPS_PROXY="+proxyURL, "NO_PROXY=")
	return out
}

func ManagedEnv(proxyURL string) []string { return PrepareEnv(os.Environ(), proxyURL) }
```

由于 `http.Client.CheckRedirect = ErrUseLastResponse`，代理自身不跟随 redirect；下游 child HTTP client 收到 3xx 后会通过代理发起下一跳请求，每个新请求都重新经过 `ServeHTTP CheckHost` 和 pinned `dialContext CheckResolvedIPs`，形成逐跳授权。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/netpolicy/ -run 'PrepareEnv|ProxyForwards|Dial' -v`

Expected: PASS；PrepareEnv 返回新 slice 并清理所有继承 proxy 变量；代理转发 status/header/body；DialContext resolve→resolved-IP check→pinned IP connect。

- [ ] **Step 5: 提交**

```bash
git add internal/netpolicy/proxy.go internal/netpolicy/proxy_test.go
git commit -m "feat(netpolicy): loopback proxy with pinned-IP dial and clean child env"
```

---

## Task 13: web_fetch/proxy 共用 policy + tools 安全上下文 API + bootstrap Phase 0 状态

**Files:**
- Create: `internal/securityctx/context.go`
- Create: `internal/securityctx/context_test.go`
- Create: `internal/tools/securityctx.go`
- Create: `internal/tools/securityctx_test.go`
- Modify: `internal/tools/web.go`
- Modify: `internal/tools/web_test.go`
- Modify: `internal/bootstrap/bootstrap.go`
- Modify: `internal/bootstrap/bootstrap_test.go`
- Modify: `internal/api/http/server.go`

- [ ] **Step 1: 写失败测试（web_fetch redirect 共用 netpolicy；context seam 存在）**

```go
package tools

import (
	"context"
	"testing"

	"github.com/x6nux/yanshi/internal/netpolicy"
	"github.com/x6nux/yanshi/internal/sandbox"
)

func TestSecurityContextRoundTrip(t *testing.T) {
	p := &netpolicy.Policy{Default: "deny", Allow: []string{"example.com"}}
	sb := sandbox.New(sandbox.Config{Enabled: true, Tier: sandbox.ReadOnly})
	ctx := WithSandbox(context.Background(), sb)
	ctx = WithNetworkPolicy(ctx, p)
	if got, ok := SandboxFromContext(ctx); !ok || got != sb { t.Fatal("sandbox context seam broken") }
	if got, ok := NetworkPolicyFromContext(ctx); !ok || got != p { t.Fatal("netpolicy context seam broken") }
}

func TestWebFetchPolicySourceIsRequired(t *testing.T) {
	w := NewWebTools(1024)
	if _, err := w.runFetch(context.Background(), `{"url":"http://example.com"}`); err == nil { t.Fatal("web_fetch must fail closed without NetworkPolicy context") }
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/tools/ ./internal/bootstrap/ -run 'SecurityContext|WebFetchPolicySource|Security' -v`

Expected: FAIL — WithSandbox/WithNetworkPolicy 不存在；web_fetch 仍单独使用 guard.Net。

- [ ] **Step 3: 实现 context API、web_fetch policy 与 bootstrap wiring**

`internal/securityctx/context.go` 保存实际 context key；这个小包只依赖 `sandbox`/`netpolicy`，因此 `tools`、`secproc`、`shell` 都能读取同一份值而不形成依赖环：

```go
package securityctx

import (
	"context"

	"github.com/x6nux/yanshi/internal/netpolicy"
	"github.com/x6nux/yanshi/internal/sandbox"
)

type sandboxKey struct{}
type networkPolicyKey struct{}

func WithSandbox(ctx context.Context, value sandbox.Sandbox) context.Context {
	if value == nil { return ctx }
	return context.WithValue(ctx, sandboxKey{}, value)
}
func Sandbox(ctx context.Context) (sandbox.Sandbox, bool) {
	value, ok := ctx.Value(sandboxKey{}).(sandbox.Sandbox)
	return value, ok && value != nil
}
func WithNetworkPolicy(ctx context.Context, value *netpolicy.Policy) context.Context {
	if value == nil { return ctx }
	return context.WithValue(ctx, networkPolicyKey{}, value)
}
func NetworkPolicy(ctx context.Context) (*netpolicy.Policy, bool) {
	value, ok := ctx.Value(networkPolicyKey{}).(*netpolicy.Policy)
	return value, ok && value != nil
}
```

`internal/tools/securityctx.go` 保留 spec 要求的 tools API，但只代理到共享包：

```go
package tools

import (
	"context"

	"github.com/x6nux/yanshi/internal/netpolicy"
	"github.com/x6nux/yanshi/internal/sandbox"
	"github.com/x6nux/yanshi/internal/securityctx"
)

func WithSandbox(ctx context.Context, value sandbox.Sandbox) context.Context {
	return securityctx.WithSandbox(ctx, value)
}
func SandboxFromContext(ctx context.Context) (sandbox.Sandbox, bool) {
	return securityctx.Sandbox(ctx)
}
func WithNetworkPolicy(ctx context.Context, value *netpolicy.Policy) context.Context {
	return securityctx.WithNetworkPolicy(ctx, value)
}
func NetworkPolicyFromContext(ctx context.Context) (*netpolicy.Policy, bool) {
	return securityctx.NetworkPolicy(ctx)
}
```

`internal/securityctx/context_test.go` 验证由 tools wrapper 写入的值可被 `securityctx`/后续 `secproc` 读取：

```go
package securityctx

import (
	"context"
	"testing"

	"github.com/x6nux/yanshi/internal/netpolicy"
	"github.com/x6nux/yanshi/internal/sandbox"
)

func TestRoundTrip(t *testing.T) {
	p := &netpolicy.Policy{Default: "deny"}
	sb := sandbox.New(sandbox.Config{Enabled: true, Tier: sandbox.ReadOnly})
	ctx := WithSandbox(context.Background(), sb)
	ctx = WithNetworkPolicy(ctx, p)
	if got, ok := Sandbox(ctx); !ok || got != sb { t.Fatal("sandbox round trip failed") }
	if got, ok := NetworkPolicy(ctx); !ok || got != p { t.Fatal("network policy round trip failed") }
}
```

把 `internal/tools/web.go` 中 profile/guard.NetHost 检查替换为公共 policy：

```go
	policy, ok := NetworkPolicyFromContext(ctx)
	if !ok { return "", &DenyErr{Reason: "no network policy in context"} }
	host := hostOnly(a.URL)
	if host == "" { return "", &DenyErr{Reason: "invalid url / empty host"} }
	if d := policy.CheckHost(host); !d.Allowed { return "", &DenyErr{Reason: d.Reason} }
```

redirect callback 完整替换为：

```go
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 { return fmt.Errorf("web.fetch: stopped after 10 redirects") }
			host := req.URL.Hostname()
			if host == "" { return &DenyErr{Reason: "redirect target has empty host"} }
			if d := policy.CheckHost(host); !d.Allowed { return &DenyErr{Reason: "redirect denied: " + d.Reason} }
			return nil
		},
```

为 web_fetch 的 HTTP transport 使用 Task 12 已定义的 `netpolicy.NewTransport(policy)`（Task 12 的 proxy.go 同时导出 `PolicyDialer` 与 `NewTransport`，CB8 已补齐这两个符号）。web_fetch 的 `http.Client` 构造改为：

```go
client := &http.Client{
	Timeout:       60 * time.Second,
	CheckRedirect: /* 见下方逐跳授权回调 */,
	Transport:     netpolicy.NewTransport(policy),
}
```

这样普通 web_fetch 也执行 resolve→CheckResolvedIPs→pin，不能只在 proxy 做 DNS 复检（CB8：NewTransport/PolicyDialer 现已在 Task 12 定义，本 Task 直接引用即可）。

bootstrap 创建指针 policy（避免不可比较 struct 问题）：

```go
	networkPolicy := &netpolicy.Policy{Default: cfg.Security.Network.Default, Allow: append([]string(nil), cfg.Security.Network.Allow...), Deny: append([]string(nil), cfg.Security.Network.Deny...), AllowPrivate: cfg.Security.Network.AllowPrivate}
	sb := sandbox.New(sandbox.Config{Enabled: cfg.Security.Sandbox.Enabled != nil && *cfg.Security.Sandbox.Enabled, WorkspaceRoot: workRoot, Tier: parseAccessTier(cfg.Security.Sandbox.Tier), NetworkDeny: cfg.Security.Sandbox.NetworkDeny})
	report := sb.Report()
	if report.Effective != sandbox.OSIsolated {
		fmt.Fprintf(os.Stderr, "yanshi: sandbox phase0 (%s): %s; OS/network isolation NOT enforced\n", report.Effective, report.Reason)
	}
```

`App` 字段使用指针：

```go
Sandbox sandbox.Sandbox
NetworkPolicy *netpolicy.Policy
```

orchestrator 的统一 `bindExecutionContext(ctx)`（Task 21 完成抽取）最终依次调用 `WithProfile`、`WithWorkRoot`、`WithApprovalManager`、`WithSandbox`、`WithNetworkPolicy`、`WithSecureProcessFactory`。本 Task 先把 `Sandbox`/`NetworkPolicy` 存在 Config 字段并在四个 turn 入口注入；Task 21 再 DRY 抽 helper。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/tools/ ./internal/netpolicy/ ./internal/bootstrap/ -run 'SecurityContext|WebFetch|Security' -v`

Expected: PASS；web_fetch/proxy 共用 Policy/PolicyDialer；redirect 逐跳授权；App.NetworkPolicy 是指针；bootstrap 明确打印 NOT enforced。

- [ ] **Step 5: 提交**

```bash
git add internal/securityctx internal/tools/securityctx.go internal/tools/securityctx_test.go internal/tools/web.go internal/tools/web_test.go internal/netpolicy/proxy.go internal/bootstrap/bootstrap.go internal/bootstrap/bootstrap_test.go internal/api/http/server.go
git commit -m "feat(security): share host policy across web and proxy; expose sandbox/network context"
```

---

# A1c — SecureProcessFactory + Shell runtime v2 + `/jobs`

A1c 验收：shell v2、legacy shell_run、jobs、ACP spawn 全部禁止裸 `exec.CommandContext`，只走 SecureProcessFactory；无 launcher fail-closed；持久 session 使用 `context.WithoutCancel` 派生独立 lifecycle context；后台输出泵始终 drain console 到 ring buffer；Wait context-aware；exit code 从 `*exec.ExitError` 提取；平台没有真 KillTree 时 capability 明示 false 且方法名降级为 `Kill`；job 元数据持久化，重启标记 stale；所有 JSON 字段 snake_case；CLI backend/TUI 完整 seam。

## Task 14: SecureProcessFactory（唯一子进程入口；fail-closed）

**依赖方向说明：** `SecureProcessSpec` 与 `SecureProcessFactory` 接口定义在 `internal/secproc`（新包，依赖 `guard`/`sandbox`/`securityctx`），context key 也定义在那里，避免 `internal/tools` 与 `internal/shell` 形成循环。`tools`、`shell`、`acp`、`goalloop`、`execprobe` 都从 `secproc` 读同一份 context value，无任何反向依赖。

**Files:**
- Create: `internal/secproc/secproc.go`
- Create: `internal/secproc/secproc_test.go`
- Modify: `internal/tools/permctx.go`（新增 thin re-export `WithSecureProcessFactory`/`SecureProcessFactoryFromContext`）
- Modify: `internal/tools/shell.go`（legacy `shell_run` 改走 `tools.LaunchSecureProcess`）

- [ ] **Step 1: 写失败测试**

```go
package secproc

import (
	"context"
	"errors"
	"testing"

	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/netpolicy"
	"github.com/x6nux/yanshi/internal/sandbox"
	"github.com/x6nux/yanshi/internal/securityctx"
	"github.com/x6nux/yanshi/internal/tools"
)

type spyFactory struct { calls int; last SecureProcessSpec }
func (s *spyFactory) Start(_ context.Context, spec SecureProcessSpec) (*StartedProcess, error) {
	s.calls++; s.last = spec
	return &StartedProcess{PID: 999}, nil
}

func TestLaunchFailsClosedWhenNoAuthorizer(t *testing.T) {
	// When no authorizer is registered, Launch must fail closed with
	// ErrNoAuthorizer (production init in internal/tools registers the real
	// one; this test temporarily nils it to prove the fail-closed branch).
	saved := currentAuthorizer
	currentAuthorizer = nil
	defer func() { currentAuthorizer = saved }()
	_, err := Launch(context.Background(), SecureProcessSpec{Tool: "shell_run"})
	if !errors.Is(err, ErrNoAuthorizer) { t.Fatalf("want ErrNoAuthorizer, got %v", err) }
}

func TestLaunchFailsClosedWhenNoFactory(t *testing.T) {
	// With an authorizer registered (TestMain or init in internal/tools) but
	// no Factory in context, Launch fails closed with a factory-missing error.
	// Use a profile that would otherwise allow the tool so the only reason for
	// failure is the missing factory.
	ctx := tools.WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"shell_run"}},
		Shell: guard.ShellPerm{Policy: "allowlist", Patterns: []string{"go test"}},
	})
	_, err := Launch(ctx, SecureProcessSpec{Tool: "shell_run", Shell: "go test"})
	if err == nil { t.Fatal("missing factory must fail closed") }
}

// verify the HardDeny firewall at the launcher seam: an empty Tools.Allow is
// a HardDeny (guard_test Task 1), and Launch must short-circuit before the
// factory is invoked. Use the real tools.Authorize path via WithProfile so the
// test exercises the same verdict switch as production.
func TestLaunchAuthorizesBeforeStartAndHardDenyShortCircuits(t *testing.T) {
	var spy spyFactory
	allowCtx := tools.WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"shell_run"}},
		Shell: guard.ShellPerm{Policy: "allowlist", Patterns: []string{"go test"}},
	})
	allowCtx = securityctx.WithNetworkPolicy(allowCtx, &netpolicy.Policy{Default: "allow"})
	allowCtx = securityctx.WithSandbox(allowCtx, sandbox.New(sandbox.Config{Enabled: true, Tier: sandbox.ReadOnly}))
	allowCtx = WithFactory(allowCtx, &spy)
	if _, err := Launch(allowCtx, SecureProcessSpec{Tool: "shell_run", Shell: "go test", Program: "sh", Args: []string{"-c", "go test"}}); err != nil {
		t.Fatalf("allow path failed: %v", err)
	}
	if spy.calls != 1 { t.Fatalf("factory must be called once on Allow, got %d", spy.calls) }

	// HardDeny: empty Tools.Allow — no factory wiring needed, but keep the spy
	// installed to prove it is NOT consulted.
	hardCtx := tools.WithProfile(context.Background(), guard.PermissionProfile{})
	hardCtx = WithFactory(hardCtx, &spy)
	_, err := Launch(hardCtx, SecureProcessSpec{Tool: "shell_run"})
	if err == nil { t.Fatal("HardDeny must block Launch") }
	if spy.calls != 1 { t.Fatalf("factory must not be called on HardDeny: %d", spy.calls) }
	// Make sure Launch surfaces the typed guard denial. HardDeny is returned as
	// *tools.DenyErr by Authorize; string matching would be both redundant and
	// brittle, and could accidentally accept an unrelated error containing
	// "denied" (L3 v3).
	if !tools.IsDenyErr(err) { t.Fatalf("err must be a *tools.DenyErr, got %T %v", err, err) }
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/secproc/ -run 'Launch' -v`

Expected: FAIL — `internal/secproc` 包不存在。

- [ ] **Step 3: 实现完整 secproc.go**

```go
package secproc

import (
	"context"
	"fmt"
	"io"
	"os/exec"

	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/sandbox"
)

type SecureProcessSpec struct {
	Tool string
	Shell string
	Program string
	Args []string
	Dir string
	Env []string
	UseSandboxTier sandbox.AccessTier
}

type StartedProcess struct {
	Cmd *exec.Cmd
	PID int
	Stdout io.Reader
	Stderr io.Reader
}

type Factory interface {
	Start(context.Context, SecureProcessSpec) (*StartedProcess, error)
}

type factoryKey struct{}

func WithFactory(ctx context.Context, f Factory) context.Context {
	if f == nil { return ctx }
	return context.WithValue(ctx, factoryKey{}, f)
}

func FromContext(ctx context.Context) (Factory, bool) {
	f, ok := ctx.Value(factoryKey{}).(Factory)
	return f, ok && f != nil
}

// Authorizer is the seam that binds the launcher to tools.Authorize without a
// dependency cycle: internal/tools registers its real Authorize once at boot
// (secproc.RegisterAuthorizer(tools.Authorize)), and secproc uses it through
// this interface. In tests the default zero-func returns ErrNoAuthorizer which
// makes Launch fail closed — no accidental bypass.
type Authorizer func(ctx context.Context, action guard.Action, argsJSON string) error

var ErrNoAuthorizer = fmt.Errorf("secproc: no authorizer registered (fail-closed)")

var currentAuthorizer Authorizer

func RegisterAuthorizer(a Authorizer) { currentAuthorizer = a }

// Launch is the single entry point for any subprocess spawn in yanshi.
// Pipeline (each step is a fail-closed check):
//   1. Authorize via the registered Authorizer — HardDeny never reaches the
//      factory; Prompt may record an approval rule through the same path
//      tools.Authorize already uses.
//   2. If no Factory is in context, fail closed (returns ErrNoFactory).
//   3. Factory.Start receives spec verbatim; spec.Program/Args MUST come from
//      shell.ShellArgv (Task 15) when spec.Shell is set.
func Launch(ctx context.Context, spec SecureProcessSpec) (*StartedProcess, error) {
	if currentAuthorizer == nil { return nil, ErrNoAuthorizer }
	if err := currentAuthorizer(ctx, guard.Action{Tool: spec.Tool, Shell: spec.Shell}, ""); err != nil {
		return nil, err
	}
	f, ok := FromContext(ctx)
	if !ok { return nil, fmt.Errorf("secproc: no Factory in context (fail-closed)") }
	return f.Start(ctx, spec)
}
```

在 `internal/tools/permctx.go` 增加 re-export 让生产代码可以继续用 `tools.WithSecureProcessFactory`/`tools.LaunchSecureProcess`：

```go
import (
	"github.com/x6nux/yanshi/internal/secproc"
)

func WithSecureProcessFactory(ctx context.Context, f secproc.Factory) context.Context {
	return secproc.WithFactory(ctx, f)
}
func SecureProcessFactoryFromContext(ctx context.Context) (secproc.Factory, bool) {
	return secproc.FromContext(ctx)
}
func LaunchSecureProcess(ctx context.Context, spec secproc.SecureProcessSpec) (*secproc.StartedProcess, error) {
	return secproc.Launch(ctx, spec)
}
```

并在 `internal/tools/authorize.go`（或 `guard.go`，依现有 Authorize 所在文件）的 package init 中注册一次：

```go
func init() { secproc.RegisterAuthorizer(func(ctx context.Context, action guard.Action, argsJSON string) error { return Authorize(ctx, action, argsJSON) }) }
```

这样 `tools/secproc_test.go`（位于 `tools` 包）使用真实 `tools.Authorize` 路径，与生产一致；`secproc` 包本身只测试接口短路，不引 `tools` 依赖。

`DefaultSecureFactory`（真正执行 `shell.ShellArgv`/`netpolicy.PrepareEnv`/`Sandbox.Prepare`）在 Task 19 写，放在 `internal/shell/factory.go`，依赖方向 `shell → secproc` 单向。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/secproc/ ./internal/tools/ -run 'Launch|SecureLauncher' -v`

Expected: PASS；无 factory fail-closed；HardDeny 不调 factory；Allow 路径 factory 被调用一次。

- [ ] **Step 5: 提交**

```bash
git add internal/secproc/secproc.go internal/secproc/secproc_test.go internal/tools/permctx.go internal/tools/guard.go
git commit -m "feat(secproc): single subprocess entry with fail-closed gate and no dependency cycle"
```

---

## Task 15: `ShellArgv` builder（替代 `defaultShellProgram/Args` 占位）

**Files:**
- Create: `internal/shell/shell_command.go`
- Create: `internal/shell/shell_command_test.go`
- Modify: `internal/tools/shell.go:227-246`（`shellCommand` 改为调用 `shell.ShellArgv`）

- [ ] **Step 1: 写失败测试**

```go
package shell

import "testing"

func TestShellArgvSelectsCorrectInterpreter(t *testing.T) {
	cases := []struct {
		env, command, wantProg string
		wantFirstArg string
	}{
		{"", "go test", "sh", "-c"},
		{"auto", "go test", "sh", "-c"},
		{"bash", "go test", "bash", "-c"},
		{"zsh", "go test", "zsh", "-c"},
		{"sh", "go test", "sh", "-c"},
		{"powershell", "Get-Date", "powershell", "-Command"},
		{"cmd", "dir", "cmd", "/c"},
	}
	for _, tc := range cases {
		prog, args, err := ShellArgv(tc.env, tc.command)
		if err != nil { t.Fatalf("ShellArgv(%q): %v", tc.env, err) }
		if prog != tc.wantProg || len(args) < 2 || args[0] != tc.wantFirstArg || args[len(args)-1] != tc.command {
			t.Fatalf("env=%q got prog=%q args=%v", tc.env, prog, args)
		}
	}
}

func TestShellArgvRejectsUnknownEnv(t *testing.T) {
	if _, _, err := ShellArgv("fish", "go test"); err == nil { t.Fatal("unknown env must fail closed") }
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/shell/ -run 'ShellArgv' -v`

Expected: FAIL — ShellArgv 不存在。

- [ ] **Step 3: 实现完整 shell_command.go**

```go
package shell

import (
	"fmt"
	"runtime"
)

// ShellArgv resolves the interpreter argv for a given shell environment and
// command string. The argv is passed verbatim to SecureProcessFactory — no
// re-marshalling through a shell happens at this layer.
func ShellArgv(env, command string) (string, []string, error) {
	if command == "" { return "", nil, fmt.Errorf("shell: empty command") }
	resolved := env
	if resolved == "" || resolved == "auto" {
		if runtime.GOOS == "windows" { resolved = "cmd" } else { resolved = "sh" }
	}
	switch resolved {
	case "cmd": return "cmd", []string{"/c", command}, nil
	case "powershell": return "powershell", []string{"-Command", command}, nil
	case "bash": return "bash", []string{"-c", command}, nil
	case "zsh": return "zsh", []string{"-c", command}, nil
	case "sh": return "sh", []string{"-c", command}, nil
	}
	return "", nil, fmt.Errorf("shell: unknown env %q", env)
}
```

把 `internal/tools/shell.go` 的 `shellCommand` 改为 thin wrapper（保留原平台 fallback 行为，不悄悄换到 `sh`）：

```go
// shellCommand kept as a thin wrapper so existing direct callers in shell_run
// keep working. shell_v2 (Task 20) goes through SecureProcessFactory instead
// of calling this directly.
func shellCommand(ctx context.Context, env, command string) *exec.Cmd {
	prog, args, err := shell.ShellArgv(env, command)
	if err != nil {
		// Preserve legacy fallback: on Windows default to cmd /c, elsewhere sh -c.
		// Unknown env (e.g. "fish") still fails closed at Authorize; we only land
		// here if the env was recognized by the guard layer but ShellArgv rejected
		// it (programmatic error) — degrade to platform default rather than panic.
		if runtime.GOOS == "windows" {
			prog, args = "cmd", []string{"/c", command}
		} else {
			prog, args = "sh", []string{"-c", command}
		}
	}
	return exec.CommandContext(ctx, prog, args...)
}
```

`internal/tools/shell.go` 的 import 块需要加上 `"runtime"` 与 `"github.com/x6nux/yanshi/internal/shell"`。

Task 20 把这处也接入 SecureProcessFactory，但本 Task 至少保证 legacy `shell_run` 与新 manager 用同一个 argv builder，消除 `defaultShellProgram/Args` 风格的占位。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/shell/ ./internal/tools/ -run 'ShellArgv|ShellRun' -v`

Expected: PASS；六种 env 正确解析；未知 env fail-closed；legacy `shell_run` 仍工作。

- [ ] **Step 5: 提交**

```bash
git add internal/shell/shell_command.go internal/shell/shell_command_test.go internal/tools/shell.go
git commit -m "feat(shell): centralize interpreter argv selection in ShellArgv"
```

---

## Task 16: shell runtime 类型（State/LaunchSpec/Session/Job/Console）

**Files:**
- Create: `internal/shell/types.go`
- Create: `internal/shell/types_test.go`

- [ ] **Step 1: 写失败测试（snake_case + 字段齐全 + 退出码可显式为 -1）**

```go
package shell

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestJobSerializesSnakeCaseAndOmitsEmptyEndedAt(t *testing.T) {
	job := Job{ID: "job-1", SessionID: "s-1", Command: "go test", State: StateRunning, ExitCode: -1, PID: 12, StartedAt: time.Unix(1700000000, 0)}
	data, err := json.Marshal(job)
	if err != nil { t.Fatal(err) }
	s := string(data)
	for _, want := range []string{`"session_id"`, `"started_at"`, `"exit_code":-1`, `"state":"running"`, `"pid":12`} {
		if !strings.Contains(s, want) { t.Fatalf("missing %q in %s", want, s) }
	}
	if strings.Contains(s, `"ended_at"`) { t.Fatalf("zero EndedAt must be omitted: %s", s) }
}

func TestSessionExitedZeroKeepsExitCodeField(t *testing.T) {
	sess := Session{ID: "s-1", State: StateExited, ExitCode: 0}
	data, _ := json.Marshal(sess)
	// exit_code is NOT omitempty: zero is a meaningful value (clean exit) and
	// must survive the round trip, unlike EndedAt where zero means unknown.
	if !strings.Contains(string(data), `"exit_code":0`) { t.Fatalf("exit_code=0 must serialize: %s", data) }
}
```

测试断言 `exit_code` **不带** `omitempty`，这样 ExitCode=0（成功退出）与 -1（仍在运行/未知）都会出现在 JSON；而 `ended_at` 保留 `omitempty`，因为零值表示"未结束"。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/shell/ -run 'JobSerializes|SessionExited' -v`

Expected: FAIL — Job/State 不存在。

- [ ] **Step 3: 实现完整 types.go**

```go
package shell

import (
	"context"
	"io"
	"time"
)

type State string
const (
	StateStarting State = "starting"
	StateRunning State = "running"
	StateExited State = "exited"
	StateCanceled State = "canceled"
	StateStale State = "stale"
)

func (s State) String() string { return string(s) }

type LaunchSpec struct {
	// ShellName is the shell selector (""|auto|cmd|powershell|bash|zsh|sh),
	// consulted by OSProcessFactory only when Program is empty. The
	// SecureProcessFactory path pre-resolves Program/Args via ShellArgv and
	// leaves ShellName empty.
	// CB1 (v3): this field was previously named `Env string` (a shell NAME),
	// which collided with SecureProcessSpec.Env []string and made
	// `range spec.Env` in recordingFactory a compile error. Renamed to
	// ShellName; the child environment now lives in Env []string below.
	ShellName string
	Command   string
	Program   string
	Args      []string
	Dir       string
	// Env is the child environment as KEY=VALUE entries.
	// netpolicy.PrepareEnv populates this; OSProcessFactory.Start applies it via
	// cmd.Env. This is a []string to match SecureProcessSpec.Env.
	Env []string
	PTY bool
}

// Session describes a persistent shell session. ExitCode uses sentinel -1 for
// "still running / unknown" and does NOT use omitempty — a zero exit code is a
// meaningful success value and must survive JSON round-trip. EndedAt, by
// contrast, uses omitempty because its zero value means "not yet ended".
type Session struct {
	ID string `json:"id"`
	PID int `json:"pid"`
	Command string `json:"command"`
	State State `json:"state"`
	ExitCode int `json:"exit_code"`
	PTY bool `json:"pty"`
	StartedAt time.Time `json:"started_at"`
	EndedAt time.Time `json:"ended_at,omitempty"`
}

type Job struct {
	ID string `json:"id"`
	SessionID string `json:"session_id"`
	Command string `json:"command"`
	State State `json:"state"`
	Output string `json:"output,omitempty"`
	Error string `json:"error,omitempty"`
	ExitCode int `json:"exit_code"`
	PID int `json:"pid"`
	StartedAt time.Time `json:"started_at"`
	EndedAt time.Time `json:"ended_at,omitempty"`
}

// Console is the byte-level seam the Manager pumps. Read returns io.EOF when
// the underlying process exits; Write blocks if the PTY/pipe back-pressures.
type Console interface {
	io.ReadWriteCloser
	Resize(rows, cols uint16) error
	PTY() bool
}

// Process is the OS-level process seam. Wait MUST return *exec.ExitError on
// non-zero exits so the Manager can extract ExitCode via ExitCode(); nil means
// clean exit. Kill terminates just this process; whether the OS also reaps
// children is reported by Capabilities().CanKillTree.
type Process interface {
	Wait() error
	PID() int
	Kill() error
	Capabilities() ProcessCapabilities
}

type ProcessCapabilities struct {
	CanKillTree bool
}

type ProcessFactory interface {
	Start(ctx context.Context, spec LaunchSpec) (Process, Console, error)
}
```

`CanKillTree` 通过 `Process.Capabilities()` 暴露；不能 KillTree 的平台仍提供 `Kill`，但 shell manager（Task 17）不会声称做了树级回收。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/shell/ -run 'JobSerializes|SessionExited' -v`

Expected: PASS；JSON 全 snake_case；零 EndedAt omitted；ExitCode 无论 0 还是 -1 都保留；`State.String()` 可用。

- [ ] **Step 5: 提交**

```bash
git add internal/shell/types.go internal/shell/types_test.go
git commit -m "feat(shell): add runtime types with snake_case JSON and non-omitempty exit_code"
```

---

## Task 17: shell Manager（独立 lifecycle context、ring buffer、KillTree capability、job 持久化）

**Files:**
- Create: `internal/shell/manager.go`
- Create: `internal/shell/manager_test.go`
- Create: `internal/shell/persist.go`

- [ ] **Step 1: 写失败测试（独立 lifecycle、ring buffer cap、context-aware Wait、KillTree capability、stale 恢复）**

```go
package shell

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeProcess struct {
	mu sync.Mutex
	pid int
	waitCh chan error
	killed bool
	canKillTree bool
}
func (p *fakeProcess) Wait() error { return <-p.waitCh }
func (p *fakeProcess) PID() int { return p.pid }
func (p *fakeProcess) Kill() error {
	p.mu.Lock(); p.killed = true; p.mu.Unlock()
	// Simulate the OS killing the process so Wait returns. This fake sends a
	// plain error; the real *exec.ExitError extraction is tested separately
	// below using the test binary as a helper process.
	select { case p.waitCh <- errors.New("killed"): default: }
	return nil
}
func (p *fakeProcess) Capabilities() ProcessCapabilities { return ProcessCapabilities{CanKillTree: p.canKillTree} }

type fakeConsole struct {
	mu sync.Mutex
	closed bool
	out chan []byte
}
func (c *fakeConsole) Read(p []byte) (int, error) {
	b, ok := <-c.out
	if !ok { return 0, io.EOF }
	return copy(p, b), nil
}
func (c *fakeConsole) Write(p []byte) (int, error) { return len(p), nil }
func (c *fakeConsole) Close() error { c.mu.Lock(); c.closed = true; c.mu.Unlock(); return nil }
func (c *fakeConsole) Resize(uint16, uint16) error { return nil }
func (c *fakeConsole) PTY() bool { return false }

type fakeFactory struct {
	consoleOut [][]byte
	canKillTree bool
}
func (f *fakeFactory) Start(_ context.Context, spec LaunchSpec) (Process, Console, error) {
	ch := make(chan []byte, 8)
	for _, b := range f.consoleOut { ch <- b }
	close(ch)
	return &fakeProcess{pid: 42, waitCh: make(chan error, 1), canKillTree: f.canKillTree}, &fakeConsole{out: ch}, nil
}

func TestManagerStartSurvivesToolContextCancel(t *testing.T) {
	m := NewManager(Config{Root: t.TempDir(), MaxOutputBytes: 4096, Factory: &fakeFactory{consoleOut: [][]byte{[]byte("hi\n")}}})
	ctx, cancel := context.WithCancel(context.Background())
	sess, err := m.Start(ctx, LaunchSpec{ShellName: "sh", Command: "echo hi", Program: "sh", Args: []string{"-c", "echo hi"}})
	if err != nil { t.Fatal(err) }
	cancel() // caller (tool) ctx goes away — session MUST survive
	time.Sleep(20 * time.Millisecond)
	got := m.Snapshot(sess.ID)
	if got.State != StateRunning { t.Fatalf("session killed by tool ctx cancel: %#v", got) }
	_ = m.Cancel(sess.ID)
}

func TestManagerRingBufferCapsJobOutput(t *testing.T) {
	big := make([][]byte, 100)
	for i := range big { big[i] = []byte("abcdefgh") }
	m := NewManager(Config{Root: t.TempDir(), MaxOutputBytes: 16, Factory: &fakeFactory{consoleOut: big}})
	job, err := m.StartJob(context.Background(), "printf lots", LaunchSpec{ShellName: "sh", Program: "sh", Args: []string{"-c", "printf lots"}})
	if err != nil { t.Fatal(err) }
	time.Sleep(80 * time.Millisecond)
	out, _ := m.ReadJob(job.ID, 4096)
	if len(out) > 16 { t.Fatalf("ring buffer must cap output: %d", len(out)) }
	_ = m.Cancel(job.SessionID)
}

func TestManagerWaitIsContextAware(t *testing.T) {
	m := NewManager(Config{Root: t.TempDir(), MaxOutputBytes: 4096, Factory: &fakeFactory{}})
	sess, err := m.Start(context.Background(), LaunchSpec{ShellName: "sh", Program: "sh", Args: []string{"-c", "sleep 60"}})
	if err != nil { t.Fatal(err) }
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := m.Wait(ctx, sess.ID); err == nil { t.Fatal("Wait must honor context cancellation") }
	_ = m.Cancel(sess.ID)
}

func TestExitCodeFromRealExecExitError(t *testing.T) {
	if os.Getenv("YANSHI_SHELL_HELPER_EXIT") == "1" { os.Exit(7) }
	cmd := exec.Command(os.Args[0], "-test.run=TestExitCodeFromRealExecExitError")
	cmd.Env = append(os.Environ(), "YANSHI_SHELL_HELPER_EXIT=1")
	err := cmd.Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) { t.Fatalf("expected *exec.ExitError, got %T %v", err, err) }
	if got := exitCodeFrom(err); got != 7 { t.Fatalf("exitCodeFrom=%d, want 7", got) }
}

func TestManagerCancelInvokesKillOnly(t *testing.T) {
	m := NewManager(Config{Root: t.TempDir(), MaxOutputBytes: 4096, Factory: &fakeFactory{canKillTree: false}})
	sess, err := m.Start(context.Background(), LaunchSpec{ShellName: "sh", Program: "sh", Args: []string{"-c", "sleep 60"}})
	if err != nil { t.Fatal(err) }
	if err := m.Cancel(sess.ID); err != nil { t.Fatal(err) }
	got := m.Snapshot(sess.ID)
	if got.State != StateCanceled { t.Fatalf("state=%v", got.State) }
	// On a platform without KillTree we do NOT pretend tree-kill happened; the
	// process is canceled via plain Kill. Test only verifies state transition.
}

type memKV struct { m sync.Map }
func (k *memKV) KVGet(key string) (string, bool, error) { v, ok := k.m.Load(key); if !ok { return "", false, nil }; return v.(string), true, nil }
func (k *memKV) KVSet(key, val string) error { k.m.Store(key, val); return nil }

func TestManagerRestoreJobsLoadsPriorJobsAsStale(t *testing.T) {
	kv := &memKV{}
	// Simulate a prior boot that persisted a still-running job.
	prior := []Job{{ID: "job-x", SessionID: "s-x", Command: "go test", State: StateRunning, PID: 999, ExitCode: -1, StartedAt: time.Unix(1700000000, 0)}}
	data, _ := jsonMarshal(t, prior)
	_ = kv.KVSet("security.shell.jobs.v1", data)
	m := NewManager(Config{Root: t.TempDir(), Factory: &fakeFactory{}}).WithPersistence(JobFromKV(kv))
	if err := m.RestoreJobs(); err != nil { t.Fatal(err) }
	// After restore, the job is in memory but its OS process is gone — it MUST
	// be marked stale so callers see StateStale, not StateRunning.
	jobs := m.ListJobs()
	if len(jobs) != 1 || jobs[0].State != StateStale { t.Fatalf("expected stale restored job, got %#v", jobs) }
}

func TestManagerCloseCancelsWaitsAndPersistsJobs(t *testing.T) {
	kv := &memKV{}
	persist := JobFromKV(kv)
	m := NewManager(Config{Root: t.TempDir(), Factory: &fakeFactory{}}).WithPersistence(persist)
	if _, err := m.StartJob(context.Background(), "sleep 60", LaunchSpec{ShellName: "sh", Program: "sh", Args: []string{"-c", "sleep 60"}}); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil { t.Fatal(err) }
	jobs, err := persist.LoadJobs()
	if err != nil { t.Fatal(err) }
	if len(jobs) != 1 { t.Fatalf("Close must flush final jobs, got %#v", jobs) }
}

// helpers used by the test above
func jsonMarshal(t *testing.T, v any) ([]byte, error) {
	t.Helper()
	return jsonMarshalImpl(v)
}

// The real implementation lives in encoding/json; alias it so the test file
// stays focused on behavior.
var jsonMarshalImpl = func(v any) ([]byte, error) {
	return jsonMarshalProduction(v)
}
```

`encoding/json` 的真实 alias 在测试文件顶部 import：

```go
import (
	"encoding/json"
	// ... 其他
)
var jsonMarshalProduction = json.Marshal
```

（Production 代码不会使用 alias；这只是测试内部 helper。）

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/shell/ -run 'Manager' -v`

Expected: FAIL — Manager/Config/Start/StartJob/ReadJob/Snapshot/RestoreJobs 不存在。

- [ ] **Step 3: 实现完整 manager.go**

```go
package shell

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"sync"
	"time"
)

type Config struct {
	Root string
	MaxOutputBytes int
	IdleTimeout time.Duration
	Factory ProcessFactory
	Logger *log.Logger // optional; defaults to log.Default()
}

type Manager struct {
	cfg Config
	mu sync.Mutex
	sessions map[string]*liveSession
	jobs map[string]*liveJob
	persist PersistStore
}

type liveSession struct {
	mu sync.Mutex
	meta Session
	console Console
	process Process
	lifecycle context.Context
	cancel context.CancelFunc
	output *ringBuffer
	done chan struct{}      // closed once Process.Wait returns
	finished bool
}

type liveJob struct {
	mu sync.Mutex
	meta Job
	session *liveSession
}

func NewManager(cfg Config) *Manager {
	if cfg.MaxOutputBytes == 0 { cfg.MaxOutputBytes = 1 << 20 }
	if cfg.Factory == nil { cfg.Factory = OSProcessFactory{} }
	return &Manager{cfg: cfg, sessions: make(map[string]*liveSession), jobs: make(map[string]*liveJob)}
}

func (m *Manager) WithPersistence(store PersistStore) *Manager { m.persist = store; return m }

// Start launches a persistent shell session. The session's lifecycle is
// decoupled from ctx via context.WithoutCancel: when the tool ctx that started
// the session is canceled (e.g. shell_start returns), the session keeps
// running. Use Wait/Cancel to observe or terminate it.
func (m *Manager) Start(ctx context.Context, spec LaunchSpec) (*Session, error) {
	if m.cfg.Factory == nil { return nil, fmt.Errorf("shell: no process factory") }
	lifecycle, cancel := context.WithCancel(context.WithoutCancel(ctx))
	proc, console, err := m.cfg.Factory.Start(lifecycle, spec)
	if err != nil { cancel(); return nil, err }
	id := fmt.Sprintf("session-%d-%d", time.Now().UnixNano(), proc.PID())
	sess := &liveSession{
		meta: Session{ID: id, PID: proc.PID(), Command: spec.Command, State: StateRunning, ExitCode: -1, PTY: console.PTY(), StartedAt: time.Now().UTC()},
		console: console, process: proc, lifecycle: lifecycle, cancel: cancel,
		output: newRingBuffer(m.cfg.MaxOutputBytes),
		done: make(chan struct{}),
	}
	m.mu.Lock(); m.sessions[id] = sess; m.mu.Unlock()
	go sess.pump()
	return &sess.meta, nil
}

func (m *Manager) StartJob(ctx context.Context, command string, spec LaunchSpec) (*Job, error) {
	sess, err := m.Start(ctx, spec)
	if err != nil { return nil, err }
	job := &Job{ID: fmt.Sprintf("job-%s", sess.ID), SessionID: sess.ID, Command: command, State: StateRunning, ExitCode: -1, PID: sess.PID, StartedAt: sess.StartedAt}
	m.mu.Lock(); m.jobs[job.ID] = &liveJob{meta: *job, session: m.sessions[sess.ID]}; m.mu.Unlock()
	if m.persist != nil { _ = m.persist.SaveJob(*job) }
	return job, nil
}

func (m *Manager) Snapshot(id string) Session {
	m.mu.Lock(); s := m.sessions[id]; m.mu.Unlock()
	if s == nil { return Session{} }
	s.mu.Lock(); defer s.mu.Unlock()
	return s.meta
}

func (m *Manager) Read(id string, max int) (string, error) {
	m.mu.Lock(); s := m.sessions[id]; m.mu.Unlock()
	if s == nil { return "", ErrNotFound }
	return s.output.Read(max), nil
}

func (m *Manager) ReadJob(id string, max int) (string, error) {
	m.mu.Lock(); j := m.jobs[id]; m.mu.Unlock()
	if j == nil { return "", ErrNotFound }
	return j.session.output.Read(max), nil
}

func (m *Manager) Write(id string, data []byte) (int, error) {
	m.mu.Lock(); s := m.sessions[id]; m.mu.Unlock()
	if s == nil { return 0, ErrNotFound }
	return s.console.Write(data)
}

// Wait blocks until the session exits or ctx is canceled. The ctx-aware path
// is the real fix for review C8: Wait no longer holds the session hostage when
// the caller's deadline passes. Idle timeout (Config.IdleTimeout) applies only
// when ctx is context.Background and no explicit deadline is set.
func (m *Manager) Wait(ctx context.Context, id string) (*Session, error) {
	m.mu.Lock(); s := m.sessions[id]; m.mu.Unlock()
	if s == nil { return nil, ErrNotFound }
	select {
	case <-s.done:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if m.cfg.IdleTimeout > 0 {
		select {
		case <-s.done:
		case <-time.After(m.cfg.IdleTimeout):
			return nil, fmt.Errorf("shell: idle timeout")
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	s.mu.Lock(); defer s.mu.Unlock()
	return &s.meta, nil
}

func (m *Manager) Cancel(id string) error {
	m.mu.Lock(); s := m.sessions[id]; m.mu.Unlock()
	if s == nil { return ErrNotFound }
	caps := s.process.Capabilities()
	if err := s.process.Kill(); err != nil { return err }
	if !caps.CanKillTree {
		lg := m.cfg.Logger
		if lg == nil { lg = log.Default() }
		lg.Printf("shell: process %d canceled without tree kill (platform CanKillTree=false)", s.process.PID())
	}
	s.cancel() // also cancel the lifecycle so pump unblocks
	s.mu.Lock(); s.meta.State = StateCanceled; s.meta.EndedAt = time.Now().UTC(); s.mu.Unlock()
	return nil
}

func (m *Manager) ListJobs() []Job {
	m.mu.Lock(); defer m.mu.Unlock()
	out := make([]Job, 0, len(m.jobs))
	for _, j := range m.jobs { j.mu.Lock(); out = append(out, j.meta); j.mu.Unlock() }
	return out
}

// RestoreJobs loads any persisted jobs and marks them StateStale. Called once
// at bootstrap; reflects the fact that the underlying OS process from a prior
// boot is gone.
func (m *Manager) RestoreJobs() error {
	if m.persist == nil { return nil }
	list, err := m.persist.LoadJobs()
	if err != nil { return err }
	m.mu.Lock(); defer m.mu.Unlock()
	for _, job := range list {
		stale := job
		stale.State = StateStale
		m.jobs[stale.ID] = &liveJob{meta: stale, session: nil}
	}
	return nil
}

// Close tears the manager down at shutdown (called once from App.Shutdown,
// Task 21, before the store closes). For each live session it cancels the
// lifecycle context — because the session's *exec.Cmd was built with
// CommandContext(lifecycle, ...), cancellation kills the OS process, the
// stdout/stderr pipe then EOFs, the pump's Read loop breaks, Process.Wait
// returns, and s.done closes. We block on <-s.done so the pump goroutine and
// console Close (deferred in pump) finish before Close returns. Finally the
// current job list is flushed to the persist store so the next boot's
// RestoreJobs (Task 17) can mark them stale. Idempotent and best-effort.
//
// CB4 (v3): Task 21 Shutdown calls shellManager.Close(); this method MUST
// exist on *Manager.
func (m *Manager) Close() error {
	m.mu.Lock()
	sessions := make([]*liveSession, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.mu.Unlock()
	for _, s := range sessions {
		s.cancel()           // cancel the independent lifecycle context
		_ = s.process.Kill() // unblock factories that do not bind Process to ctx
		<-s.done             // wait for pump goroutine (also closes the console)
	}
	if m.persist != nil {
		for _, job := range m.ListJobs() {
			_ = m.persist.SaveJob(job)
		}
	}
	return nil
}

var ErrNotFound = errors.New("shell: session/job not found")

// pump drains console output into the ring buffer until the console signals
// EOF, then waits for the process and records the exit code. Pumping always
// runs (even when nobody calls Read) so the buffer captures the full tail —
// this is the real fix for the "ring buffer never drains" review bug.
func (s *liveSession) pump() {
	defer close(s.done)
	defer s.console.Close()
	buf := make([]byte, 4096)
	for {
		n, err := s.console.Read(buf)
		if n > 0 { s.output.Write(buf[:n]) }
		if err != nil { break }
	}
	werr := s.process.Wait()
	s.mu.Lock()
	s.meta.EndedAt = time.Now().UTC()
	if s.meta.State != StateCanceled { s.meta.State = StateExited }
	s.meta.ExitCode = exitCodeFrom(werr)
	s.finished = true
	s.mu.Unlock()
}

func exitCodeFrom(err error) int {
	if err == nil { return 0 }
	var ee *exec.ExitError
	if errors.As(err, &ee) { return ee.ExitCode() }
	return -1
}

type ringBuffer struct {
	mu sync.Mutex
	cap int
	buf bytes.Buffer
}
func newRingBuffer(cap int) *ringBuffer { return &ringBuffer{cap: cap} }
func (r *ringBuffer) Write(p []byte) (int, error) {
	r.mu.Lock(); defer r.mu.Unlock()
	r.buf.Write(p)
	if r.cap > 0 && r.buf.Len() > r.cap {
		overflow := r.buf.Len() - r.cap
		r.buf.Next(overflow)
	}
	return len(p), nil
}
func (r *ringBuffer) Read(max int) string {
	r.mu.Lock(); defer r.mu.Unlock()
	if max <= 0 || max > r.buf.Len() { return r.buf.String() }
	data := r.buf.Bytes()
	start := len(data) - max
	if start < 0 { start = 0 }
	return string(data[start:])
}
```

`internal/shell/persist.go`：

```go
package shell

import (
	"encoding/json"
	"fmt"
)

type PersistStore interface {
	SaveJob(Job) error
	LoadJobs() ([]Job, error)
}

// JobFromKV adapts the internal/store.Store KV API (KVGet/KVSet) to
// PersistStore. store.Store already satisfies this interface contract.
func JobFromKV(kv interface {
	KVGet(string) (string, bool, error)
	KVSet(string, string) error
}) PersistStore { return &kvPersist{kv: kv} }

type kvPersist struct {
	kv interface {
		KVGet(string) (string, bool, error)
		KVSet(string, string) error
	}
}

const jobKey = "security.shell.jobs.v1"

func (p *kvPersist) SaveJob(job Job) error {
	existing, _, _ := p.kv.KVGet(jobKey)
	var list []Job
	if existing != "" { _ = json.Unmarshal([]byte(existing), &list) }
	// Replace any prior entry with the same ID so SaveJob is idempotent.
	out := list[:0]
	for _, j := range list { if j.ID != job.ID { out = append(out, j) } }
	out = append(out, job)
	data, err := json.Marshal(out)
	if err != nil { return fmt.Errorf("shell: encode jobs: %w", err) }
	return p.kv.KVSet(jobKey, string(data))
}

func (p *kvPersist) LoadJobs() ([]Job, error) {
	raw, ok, err := p.kv.KVGet(jobKey)
	if err != nil { return nil, err }
	if !ok || raw == "" { return nil, nil }
	var list []Job
	if err := json.Unmarshal([]byte(raw), &list); err != nil { return nil, fmt.Errorf("shell: decode jobs: %w", err) }
	return list, nil
}
```

关键修复点对照 review：

- **独立 lifecycle context**（spec CRITICAL6）：`context.WithCancel(context.WithoutCancel(ctx))`。
- **context-aware Wait**（review C8）：`Wait(ctx, id)` 在 `<-s.done` 与 `<-ctx.Done()` 上 select。
- **ExitCode 从 `*exec.ExitError` 提取**：`errors.As(err, &ee)` 然后 `ee.ExitCode()`。`TestExitCodeFromRealExecExitError` 通过把 test binary 自身作为 helper subprocess 重入并 `os.Exit(7)` 来生成真实的 `*exec.ExitError`（fakeExitErr 之前的写法不能正确满足 `errors.As`，因为 `exec.ExitError` 是 struct 不是 interface），这条测试是 review 反馈"ExitCode 必须真"的关键证明。
- **KillTree capability 真实报告**：`Cancel` 读 `Capabilities().CanKillTree`，false 时写 log 说明没做树级 kill，不伪造成功。
- **pump 始终 drain**：`pump` goroutine 在 session 创建时立即启动，不依赖 Read/Wait 被调用。
- **job 持久化与 stale 恢复**：`SaveJob` 用 replace-by-ID 保持 idempotent；`RestoreJobs` 在启动时把所有加载的 job 标记 StateStale（review 要求的"重启标记 stale"行为）。
- **manager_test.go 中的 fakeProcess / *exec.ExitError 提取**：测试使用 `errors.As` + 真实 helper subprocess（`TestExitCodeFromRealExecExitError`），不依赖自定义 `fakeExitErr` 满足接口的 trick——`exec.ExitError` 是 struct，`errors.As` 必须看真实的 `*exec.ExitError` 实例。fake Kill 只发送 `errors.New("killed")`；这条路径不被 `TestManagerCancelInvokesKillOnly` 断言 ExitCode，因此不影响正确性。

`OSProcessFactory` 实现放在 `internal/shell/process.go`（Task 18）。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/shell/ -run 'Manager' -v`

Expected: PASS；tool ctx cancel 不杀 session；ring buffer 把 100 次 8 字节写入裁到 16 字节；Wait ctx cancel 立即返回；Cancel 在 CanKillTree=false 时仍把 state 置为 StateCanceled；RestoreJobs 把 prior job 标为 StateStale；Close 取消 lifecycle、等待 pump 并 flush jobs。

- [ ] **Step 5: 提交**

```bash
git add internal/shell/manager.go internal/shell/manager_test.go internal/shell/persist.go
git commit -m "feat(shell): manager with independent lifecycle, context-aware Wait, KillTree capability, job persistence"
```

---

## Task 18: platform console/PTY capability skeleton（显式 ErrPTYUnavailable）

**Files:**
- Create: `internal/shell/process.go`
- Create: `internal/shell/console_unix.go`
- Create: `internal/shell/console_windows.go`
- Create: `internal/shell/console_other.go`
- Create: `internal/shell/console_test.go`

- [ ] **Step 1: 写失败测试（PTY 必须 honest unavailable）**

```go
package shell

import (
	"context"
	"errors"
	"runtime"
	"testing"
)

func TestPlatformPTYCapabilityIsHonestUnavailable(t *testing.T) {
	cap := PlatformPTYCapability()
	if cap.Available { t.Fatalf("Phase 0 must NOT advertise PTY as available: %#v", cap) }
	if cap.Backend == "" || cap.Reason == "" { t.Fatalf("capability fields missing: %#v", cap) }
}

func TestStartPTYReturnsErrPTYUnavailable(t *testing.T) {
	_, _, err := StartPTYProcess(context.Background(), LaunchSpec{Program: "echo", PTY: true})
	if !errors.Is(err, ErrPTYUnavailable) { t.Fatalf("want ErrPTYUnavailable, got %v", err) }
}

func TestOSProcessFactoryCanKillTreeReflectsPlatform(t *testing.T) {
	caps := (&OSProcessFactory{}).Capabilities(context.Background())
	// Bidirectional (M1): Windows Phase 0 cannot tree-kill yet; Unix kills via
	// the process group. Asserting BOTH directions stops the test from silently
	// passing as a no-op on either platform.
	if runtime.GOOS == "windows" {
		if caps.CanKillTree {
			t.Fatalf("Windows Phase 0 must not claim tree kill: %#v", caps)
		}
	} else {
		if !caps.CanKillTree {
			t.Fatalf("Unix must claim tree kill via process group: %#v", caps)
		}
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/shell/ -run 'PlatformPTYCapability|StartPTYReturnsErrPTYUnavailable|OSProcessFactoryCanKillTree' -v`

Expected: FAIL — PTY/OSProcessFactory 不存在。

- [ ] **Step 3: 实现 capability skeleton**

`internal/shell/process.go`：

```go
package shell

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"runtime"
)

type OSProcessFactory struct{}

func (OSProcessFactory) Start(ctx context.Context, spec LaunchSpec) (Process, Console, error) {
	if spec.PTY {
		return StartPTYProcess(ctx, spec)
	}
	program, args := spec.Program, spec.Args
	if program == "" { return nil, nil, fmt.Errorf("shell: spec.Program required") }
	cmd := exec.CommandContext(ctx, program, args...)
	cmd.Dir = spec.Dir
	// Apply the child env produced by netpolicy.PrepareEnv (forwarded via
	// LaunchSpec.Env by DefaultSecureFactory, CB1). Without this the child
	// inherits the parent's full environment, re-introducing any developer
	// http_proxy — the exact leak PrepareEnv exists to close.
	if len(spec.Env) > 0 { cmd.Env = spec.Env }
	stdout, err := cmd.StdoutPipe()
	if err != nil { return nil, nil, err }
	stderr, err := cmd.StderrPipe()
	if err != nil { stdout.Close(); return nil, nil, err }
	combined := io.NopCloser(io.MultiReader(stdout, stderr))
	if err := cmd.Start(); err != nil { return nil, nil, err }
	return &osProcess{cmd: cmd}, &pipeConsole{r: combined}, nil
}

// Capabilities reports what the OS factory can do without launching a process.
// Callers (and tests) probe CanKillTree here before promising tree-level
// cancellation. M1 (v3): a bidirectional test asserts this on BOTH platforms.
func (OSProcessFactory) Capabilities(context.Context) ProcessCapabilities {
	return ProcessCapabilities{CanKillTree: CanKillTreeOnPlatform()}
}

type osProcess struct { cmd *exec.Cmd }
func (p *osProcess) Wait() error { return p.cmd.Wait() }
func (p *osProcess) PID() int { if p.cmd.Process != nil { return p.cmd.Process.Pid }; return 0 }
func (p *osProcess) Kill() error {
	if p.cmd.Process == nil { return nil }
	return p.cmd.Process.Kill()
}
func (p *osProcess) Capabilities() ProcessCapabilities { return ProcessCapabilities{CanKillTree: CanKillTreeOnPlatform()} }

type pipeConsole struct { r io.ReadCloser }
func (c *pipeConsole) Read(b []byte) (int, error) { return c.r.Read(b) }
func (c *pipeConsole) Write([]byte) (int, error) { return 0, fmt.Errorf("pipe console is read-only") }
func (c *pipeConsole) Resize(uint16, uint16) error { return fmt.Errorf("pipe console cannot resize") }
func (c *pipeConsole) Close() error { return c.r.Close() }
func (c *pipeConsole) PTY() bool { return false }

var ErrPTYUnavailable = fmt.Errorf("shell: PTY adapter unavailable in Phase 0")

type PTYCapability struct {
	Platform string
	Backend string
	Reason string
	Available bool
}
```

每个平台文件独立：

```go
// internal/shell/console_unix.go
//go:build unix
package shell
import "runtime"
func PlatformPTYCapability() PTYCapability {
	return PTYCapability{Platform: runtime.GOOS, Backend: "unix-pty-pending", Reason: "creack/pty vs x/term vs custom wrapper decision not yet made", Available: false}
}
func StartPTYProcess(context.Context, LaunchSpec) (Process, Console, error) { return nil, nil, ErrPTYUnavailable }
func CanKillTreeOnPlatform() bool { return true }
```

```go
// internal/shell/console_windows.go
//go:build windows
package shell
import "runtime"
func PlatformPTYCapability() PTYCapability {
	return PTYCapability{Platform: runtime.GOOS, Backend: "conpty-pending", Reason: "ConPTY wrapper and job-object tree-kill decision pending", Available: false}
}
func StartPTYProcess(context.Context, LaunchSpec) (Process, Console, error) { return nil, nil, ErrPTYUnavailable }
func CanKillTreeOnPlatform() bool { return false }
```

```go
// internal/shell/console_other.go
//go:build !unix && !windows
package shell
import "runtime"
func PlatformPTYCapability() PTYCapability {
	return PTYCapability{Platform: runtime.GOOS, Backend: "unsupported", Reason: "no PTY adapter reviewed for this platform", Available: false}
}
func StartPTYProcess(context.Context, LaunchSpec) (Process, Console, error) { return nil, nil, ErrPTYUnavailable }
func CanKillTreeOnPlatform() bool { return false }
```

不要在 Phase 0 中写 `syscall.Setpgid`、`CreateJobObject`、`OpenPty` 等；只暴露能力位。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/shell/ -run 'PlatformPTY|StartPTY|OSProcessFactory' -v`

Expected: PASS；PTY 显式 unavailable；KillTree 能力双向按平台分（Windows=false，非 Windows=true）；child `LaunchSpec.Env` 在 `cmd.Start()` 前应用到 `cmd.Env`。

- [ ] **Step 5: 提交**

```bash
git add internal/shell/process.go internal/shell/console_unix.go internal/shell/console_windows.go internal/shell/console_other.go internal/shell/console_test.go
git commit -m "feat(shell): Phase 0 PTY/KillTree capability boundary"
```

---

## Task 19: DefaultSecureFactory 把 sandbox/netpolicy/shell manager 串起来

**Files:**
- Create: `internal/shell/factory.go`
- Create: `internal/shell/factory_test.go`

**依赖方向说明：** Task 14 定义了 `secproc.Factory.Start(ctx, spec) (*StartedProcess, error)`；Task 16/17 定义了 `shell.ProcessFactory.Start(ctx, LaunchSpec) (Process, Console, error)`。`DefaultSecureFactory` 在 `shell` 包内实现 `secproc.Factory`，内部组合 `ProcessFactory`+`Sandbox`+`netpolicy.Policy`：这是单向依赖（`shell → secproc`、`shell → sandbox`、`shell → netpolicy`），不引入环。

- [ ] **Step 1: 写失败测试**

```go
package shell

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/netpolicy"
	"github.com/x6nux/yanshi/internal/sandbox"
	"github.com/x6nux/yanshi/internal/secproc"
)

// recordingFactory captures the env slice that DefaultSecureFactory hands off
// to the OS layer, so we can assert PrepareEnv stripped the inherited vars.
type recordingFactory struct {
	gotEnv []string
}
func (r *recordingFactory) Start(_ context.Context, spec LaunchSpec) (Process, Console, error) {
	r.gotEnv = append([]string(nil), spec.Env...)
	// Return a no-op Process + Console so DefaultSecureFactory can complete.
	return &noopProcess{}, &noopConsole{}, nil
}

type noopProcess struct{}
func (noopProcess) Wait() error { return nil }
func (noopProcess) PID() int { return 1 }
func (noopProcess) Kill() error { return nil }
func (noopProcess) Capabilities() ProcessCapabilities { return ProcessCapabilities{CanKillTree: false} }

type noopConsole struct{}
func (noopConsole) Read([]byte) (int, error) { return 0, errConsoleClosed }
func (noopConsole) Write(p []byte) (int, error) { return len(p), nil }
func (noopConsole) Close() error { return nil }
func (noopConsole) Resize(uint16, uint16) error { return nil }
func (noopConsole) PTY() bool { return false }

var errConsoleClosed = errors.New("console closed")

func TestConsoleReaderPreservesNonEOFError(t *testing.T) {
	_, err := (consoleReader{r: noopConsole{}}).Read(make([]byte, 8))
	if !errors.Is(err, errConsoleClosed) {
		t.Fatalf("non-EOF console error must survive, got %v", err)
	}
}

func TestDefaultSecureFactoryStripsInheritedProxyEnv(t *testing.T) {
	rec := &recordingFactory{}
	f := DefaultSecureFactory{
		OS: rec,
		Policy: &netpolicy.Policy{Default: "allow"},
		ProxyURL: "http://127.0.0.1:9090",
		Sandbox: sandbox.New(sandbox.Config{Enabled: true, Tier: sandbox.ReadOnly}),
	}
	spec := secproc.SecureProcessSpec{
		Tool: "shell_run",
		Shell: "go version",
		Program: "go",
		Args: []string{"version"},
		Env: []string{"PATH=/usr/bin", "http_proxy=leak", "HTTPS_PROXY=leak"},
	}
	if _, err := f.Start(context.Background(), spec); err != nil { t.Fatalf("start: %v", err) }
	joined := strings.Join(rec.gotEnv, "\n")
	if strings.Contains(strings.ToLower(joined), "http_proxy=leak") { t.Fatalf("inherited proxy var survived: %v", rec.gotEnv) }
	if !strings.Contains(joined, "HTTP_PROXY=http://127.0.0.1:9090") { t.Fatalf("managed proxy var missing: %v", rec.gotEnv) }
}

func TestDefaultSecureFactoryFailsClosedWhenNoOSFactory(t *testing.T) {
	f := DefaultSecureFactory{}
	if _, err := f.Start(context.Background(), secproc.SecureProcessSpec{Program: "go"}); err == nil { t.Fatal("missing OS factory must fail closed") }
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/shell/ -run 'DefaultSecureFactory|ConsoleReader' -v`

Expected: FAIL — `DefaultSecureFactory` 不存在。

- [ ] **Step 3: 实现完整 factory.go**

```go
package shell

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/x6nux/yanshi/internal/netpolicy"
	"github.com/x6nux/yanshi/internal/sandbox"
	"github.com/x6nux/yanshi/internal/secproc"
)

// DefaultSecureFactory implements secproc.Factory by wiring together the OS
// ProcessFactory (Task 18), the sandbox adapter (Task 10, Phase 0 stub), and
// the network policy (Task 11). It is the only production secproc.Factory —
// tests inject spy factories through the same interface.
type DefaultSecureFactory struct {
	OS       ProcessFactory
	Policy   *netpolicy.Policy
	ProxyURL string
	Sandbox  sandbox.Sandbox
}

// secureFactoryOSAdapter bridges the secproc.Factory signature to shell's
// ProcessFactory. It records the OS-level Process and Console so the caller
// (shell_v2 tools or legacy shell_run) can drive them through Manager.
func (f DefaultSecureFactory) Start(ctx context.Context, spec secproc.SecureProcessSpec) (*secproc.StartedProcess, error) {
	if f.OS == nil { return nil, fmt.Errorf("shell: DefaultSecureFactory.OS is nil (fail-closed)") }
	env := spec.Env
	if f.Policy != nil {
		proxyURL := f.ProxyURL
		if proxyURL == "" { proxyURL = "http://127.0.0.1:0" }
		env = netpolicy.PrepareEnv(env, proxyURL)
	} else {
		// Even without a policy we strip inherited proxy vars — silently
		// inheriting a developer's http_proxy is a known TOCTOU vector.
		env = netpolicy.PrepareEnv(env, "")
	}
	// CB1/CB2 (v3): ShellName is the shell selector; Env is the concrete
	// []string environment. Forward the PrepareEnv result — never discard it.
	launch := LaunchSpec{ShellName: spec.Shell, Env: env, Command: spec.Shell, Program: spec.Program, Args: spec.Args, Dir: spec.Dir, PTY: false}
	_ = f.Sandbox // Phase 0: Prepare is a no-op; we keep the seam explicit so
	              // a real adapter (A1c follow-up) just calls f.Sandbox.Prepare here.
	proc, console, err := f.OS.Start(ctx, launch)
	if err != nil { return nil, err }
	return &secproc.StartedProcess{PID: proc.PID(), Stdout: consoleReader{console}, Stderr: io.Discard}, nil
}

// consoleReader adapts shell.Console to io.Reader so secproc.StartedProcess
// callers (e.g. legacy shell_run streamer) can pump stdout without knowing
// about the shell.Console interface.
type consoleReader struct{ r Console }
func (c consoleReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if errors.Is(err, io.EOF) { return n, io.EOF }
	if err != nil { return n, err }
	return n, nil
}
```

`ProcessFactory`（Task 16 定义）的 `OS` 字段类型是 Task 18 的 `OSProcessFactory{}`。Task 18 已在 `cmd.Start()` 前显式执行 `if len(spec.Env) > 0 { cmd.Env = spec.Env }`；本 Task 必须通过 `LaunchSpec{Env: env}` 把 `PrepareEnv` 的输出传下去。这两端缺一不可，`TestDefaultSecureFactoryStripsInheritedProxyEnv` 才能证明真实 child env 不再继承代理变量（CB1/CB2）。

`f.Sandbox` 的真正 `Prepare(ctx, cmd, CommandSpec{...})` 调用是 Phase 0 后续的事；Task 19 只把 seam 留下 + 编译可过 + 测试能验证 env 清洗。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/shell/ -run 'DefaultSecureFactory|ConsoleReader' -v`

Expected: PASS；factory 调用 OS factory 前 strip 继承 proxy 变量并通过 `LaunchSpec.Env` 真正转发；OS 为 nil 时 fail-closed；`*secproc.StartedProcess` 非 nil；consoleReader 仅归一化 EOF、保留其他读取错误。

- [ ] **Step 5: 提交**

```bash
git add internal/shell/factory.go internal/shell/factory_test.go internal/shell/process.go
git commit -m "feat(shell): default secure factory wiring sandbox and proxy env"
```

---

## Task 20: shell v2 工具（真实 Tool 名 Authorize；manager 注入 context）

**Files:**
- Create: `internal/tools/shell_v2.go`
- Create: `internal/tools/shell_v2_test.go`
- Modify: `internal/tools/permctx.go`

- [ ] **Step 1: 写失败测试**

```go
package tools

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/shell"
)

// L2 (v3): the factory MUST return non-nil Process/Console. Manager.Start
// (Task 17) immediately calls proc.PID()/console.PTY() and spawns the pump
// goroutine — a nil process panics there. These fakes mirror the shape of
// Task 17's fakeProcess/fakeConsole (kept here because this test lives in
// package tools, not shell, so it cannot reference the shell-internal fakes).
type fakeShellProcess struct{}
func (fakeShellProcess) Wait() error                  { return nil }
func (fakeShellProcess) PID() int                      { return 1 }
func (fakeShellProcess) Kill() error                   { return nil }
func (fakeShellProcess) Capabilities() shell.ProcessCapabilities {
	return shell.ProcessCapabilities{CanKillTree: false}
}

type fakeShellConsole struct{}
func (fakeShellConsole) Read([]byte) (int, error)  { return 0, io.EOF }
func (fakeShellConsole) Write(p []byte) (int, error) { return len(p), nil }
func (fakeShellConsole) Close() error                { return nil }
func (fakeShellConsole) Resize(uint16, uint16) error { return nil }
func (fakeShellConsole) PTY() bool                    { return false }

type fakeShellFactory struct{}
func (fakeShellFactory) Start(context.Context, shell.LaunchSpec) (shell.Process, shell.Console, error) {
	return fakeShellProcess{}, fakeShellConsole{}, nil
}

func TestShellV2StartUsesRealToolName(t *testing.T) {
	manager := shell.NewManager(shell.Config{Root: t.TempDir(), MaxOutputBytes: 256, Factory: fakeShellFactory{}})
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"shell_start"}},
		Shell: guard.ShellPerm{Policy: "allowlist", Patterns: []string{"echo *"}},
	})
	ctx = WithShellManager(ctx, manager)
	v := NewShellV2Tools()
	out, err := runTool(ctx, v.Start, `{"command":"echo hi"}`)
	if err != nil { t.Fatal(err) }
	if !strings.Contains(out, "session_id") { t.Fatalf("start result=%q", out) }
}

func TestShellV2WriteAuthorizesAsWriteToolNotShellString(t *testing.T) {
	manager := shell.NewManager(shell.Config{Root: t.TempDir(), MaxOutputBytes: 256, Factory: fakeShellFactory{}})
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"shell_start"}}, // missing shell_write_stdin
		Shell: guard.ShellPerm{Policy: "allowlist", Patterns: []string{"*"}},
	})
	ctx = WithShellManager(ctx, manager)
	v := NewShellV2Tools()
	out, _ := runTool(ctx, v.Write, `{"id":"missing","data":"x"}`)
	if !strings.Contains(out, "permission denied") { t.Fatalf("write must Authorize as shell_write_stdin, got %q", out) }
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/tools/ -run 'ShellV2' -v`

Expected: FAIL — ShellV2Tools/WithShellManager 不存在。

- [ ] **Step 3: 实现 shell_v2.go 与 context API**

在 `permctx.go` 增加：

```go
type shellManagerKey struct{}
func WithShellManager(ctx context.Context, manager *shell.Manager) context.Context {
	if manager == nil { return ctx }
	return context.WithValue(ctx, shellManagerKey{}, manager)
}
func ShellManagerFromContext(ctx context.Context) (*shell.Manager, bool) {
	m, ok := ctx.Value(shellManagerKey{}).(*shell.Manager)
	return m, ok && m != nil
}
```

`internal/tools/shell_v2.go`：

```go
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/shell"
)

type shellStartArgs struct {
	Command string `json:"command"`
	Workdir string `json:"workdir"`
	PTY bool `json:"pty"`
}
type shellIDArgs struct { ID string `json:"id"` }
type shellWriteArgs struct { ID string `json:"id"` Data string `json:"data"` }
type shellReadArgs struct { ID string `json:"id"` MaxBytes int `json:"max_bytes"` }

type ShellV2Tools struct {
	Start, Read, Write, Wait, Cancel *GuardedTool
	TaskStart, TaskWait, TaskWrite, TaskCancel *GuardedTool
}

func NewShellV2Tools() *ShellV2Tools {
	v := &ShellV2Tools{}
	v.Start = NewGuardedTool("shell_start", "Shell", "Start a persistent shell session.", 30*time.Second,
		params(map[string]*schema.ParameterInfo{"command": {Type: schema.String, Required: true}, "workdir": {Type: schema.String}, "pty": {Type: schema.Boolean}}),
		SyncStream(v.start))
	v.Read = NewGuardedTool("shell_read", "Shell", "Read incremental output.", 30*time.Second,
		params(map[string]*schema.ParameterInfo{"id": {Type: schema.String, Required: true}, "max_bytes": {Type: schema.Integer}}),
		SyncStream(v.read))
	v.Write = NewGuardedTool("shell_write_stdin", "Shell", "Write stdin to a shell session.", 30*time.Second,
		params(map[string]*schema.ParameterInfo{"id": {Type: schema.String, Required: true}, "data": {Type: schema.String, Required: true}}),
		SyncStream(v.write))
	v.Wait = NewGuardedTool("shell_wait", "Shell", "Wait for session completion.", 130*time.Second,
		params(map[string]*schema.ParameterInfo{"id": {Type: schema.String, Required: true}}),
		SyncStream(v.wait))
	v.Cancel = NewGuardedTool("shell_cancel", "Shell", "Cancel a shell session.", 30*time.Second,
		params(map[string]*schema.ParameterInfo{"id": {Type: schema.String, Required: true}}),
		SyncStream(v.cancel))
	v.TaskStart = NewGuardedTool("task_shell_start", "Shell Job", "Start a background shell job.", 30*time.Second,
		params(map[string]*schema.ParameterInfo{"command": {Type: schema.String, Required: true}, "workdir": {Type: schema.String}}),
		SyncStream(v.taskStart))
	v.TaskWait = NewGuardedTool("task_shell_wait", "Shell Job", "Read background job state.", 30*time.Second,
		params(map[string]*schema.ParameterInfo{"id": {Type: schema.String, Required: true}, "max_bytes": {Type: schema.Integer}}),
		SyncStream(v.taskWait))
	v.TaskWrite = NewGuardedTool("task_shell_stdin", "Shell Job", "Write stdin to a background job.", 30*time.Second,
		params(map[string]*schema.ParameterInfo{"id": {Type: schema.String, Required: true}, "data": {Type: schema.String, Required: true}}),
		SyncStream(v.taskWrite))
	v.TaskCancel = NewGuardedTool("task_shell_cancel", "Shell Job", "Cancel a background job.", 30*time.Second,
		params(map[string]*schema.ParameterInfo{"id": {Type: schema.String, Required: true}}),
		SyncStream(v.taskCancel))
	return v
}

func (v *ShellV2Tools) manager(ctx context.Context) (*shell.Manager, error) {
	m, ok := ShellManagerFromContext(ctx)
	if !ok { return nil, fmt.Errorf("shell: runtime unavailable") }
	return m, nil
}

func (v *ShellV2Tools) start(ctx context.Context, raw string) (string, error) {
	var a shellStartArgs
	if err := json.Unmarshal([]byte(raw), &a); err != nil { return "", err }
	if err := Authorize(ctx, guard.Action{Tool: "shell_start", Shell: a.Command}, raw); err != nil { return "", err }
	m, err := v.manager(ctx); if err != nil { return "", err }
	prog, args, err := shell.ShellArgv("", a.Command)
	if err != nil { return "", err }
	sess, err := m.Start(ctx, shell.LaunchSpec{ShellName: "", Command: a.Command, Program: prog, Args: args, Dir: a.Workdir, PTY: a.PTY})
	if err != nil { return "", err }
	data, err := json.Marshal(sess); return string(data), err
}

func (v *ShellV2Tools) read(ctx context.Context, raw string) (string, error) {
	var a shellReadArgs
	if err := json.Unmarshal([]byte(raw), &a); err != nil { return "", err }
	if err := Authorize(ctx, guard.Action{Tool: "shell_read"}, raw); err != nil { return "", err }
	m, err := v.manager(ctx); if err != nil { return "", err }
	out, err := m.Read(a.ID, a.MaxBytes)
	if err != nil { return "", err }
	return fmt.Sprintf(`{"output":%q}`, out), nil
}

func (v *ShellV2Tools) write(ctx context.Context, raw string) (string, error) {
	var a shellWriteArgs
	if err := json.Unmarshal([]byte(raw), &a); err != nil { return "", err }
	if err := Authorize(ctx, guard.Action{Tool: "shell_write_stdin"}, raw); err != nil { return "", err }
	m, err := v.manager(ctx); if err != nil { return "", err }
	n, err := m.Write(a.ID, []byte(a.Data))
	if err != nil { return "", err }
	return fmt.Sprintf(`{"written":%d}`, n), nil
}

func (v *ShellV2Tools) wait(ctx context.Context, raw string) (string, error) {
	var a shellIDArgs
	if err := json.Unmarshal([]byte(raw), &a); err != nil { return "", err }
	if err := Authorize(ctx, guard.Action{Tool: "shell_wait"}, raw); err != nil { return "", err }
	m, err := v.manager(ctx); if err != nil { return "", err }
	// Pass ctx so the caller's deadline cancels Wait (Task 17 contract).
	sess, err := m.Wait(ctx, a.ID)
	if err != nil { return "", err }
	data, err := json.Marshal(sess); return string(data), err
}

func (v *ShellV2Tools) cancel(ctx context.Context, raw string) (string, error) {
	var a shellIDArgs
	if err := json.Unmarshal([]byte(raw), &a); err != nil { return "", err }
	if err := Authorize(ctx, guard.Action{Tool: "shell_cancel"}, raw); err != nil { return "", err }
	m, err := v.manager(ctx); if err != nil { return "", err }
	if err := m.Cancel(a.ID); err != nil { return "", err }
	return `{"canceled":true}`, nil
}

func (v *ShellV2Tools) taskStart(ctx context.Context, raw string) (string, error) {
	var a shellStartArgs
	if err := json.Unmarshal([]byte(raw), &a); err != nil { return "", err }
	if err := Authorize(ctx, guard.Action{Tool: "task_shell_start", Shell: a.Command}, raw); err != nil { return "", err }
	m, err := v.manager(ctx); if err != nil { return "", err }
	prog, args, err := shell.ShellArgv("", a.Command)
	if err != nil { return "", err }
	job, err := m.StartJob(ctx, a.Command, shell.LaunchSpec{Command: a.Command, Program: prog, Args: args, Dir: a.Workdir})
	if err != nil { return "", err }
	data, err := json.Marshal(job); return string(data), err
}

func (v *ShellV2Tools) taskWait(ctx context.Context, raw string) (string, error) {
	var a shellReadArgs
	if err := json.Unmarshal([]byte(raw), &a); err != nil { return "", err }
	if err := Authorize(ctx, guard.Action{Tool: "task_shell_wait"}, raw); err != nil { return "", err }
	m, err := v.manager(ctx); if err != nil { return "", err }
	out, _ := m.ReadJob(a.ID, a.MaxBytes)
	data, _ := json.Marshal(struct {
		ID string `json:"id"`
		Output string `json:"output"`
	}{ID: a.ID, Output: out})
	return string(data), nil
}

func (v *ShellV2Tools) taskWrite(ctx context.Context, raw string) (string, error) {
	var a shellWriteArgs
	if err := json.Unmarshal([]byte(raw), &a); err != nil { return "", err }
	if err := Authorize(ctx, guard.Action{Tool: "task_shell_stdin"}, raw); err != nil { return "", err }
	m, err := v.manager(ctx); if err != nil { return "", err }
	n, err := m.Write(a.ID, []byte(a.Data))
	if err != nil { return "", err }
	return fmt.Sprintf(`{"written":%d}`, n), nil
}

func (v *ShellV2Tools) taskCancel(ctx context.Context, raw string) (string, error) {
	var a shellIDArgs
	if err := json.Unmarshal([]byte(raw), &a); err != nil { return "", err }
	if err := Authorize(ctx, guard.Action{Tool: "task_shell_cancel"}, raw); err != nil { return "", err }
	m, err := v.manager(ctx); if err != nil { return "", err }
	if err := m.Cancel(a.ID); err != nil { return "", err }
	return `{"canceled":true}`, nil
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/tools/ -run 'ShellV2' -v`

Expected: PASS；每个工具用真实 Tool 名 Authorize（write/cancel 不伪造 Shell 字符串）；manager 来自 context；起 session 不被 tool ctx cancel 杀。

- [ ] **Step 5: 提交**

```bash
git add internal/tools/shell_v2.go internal/tools/shell_v2_test.go internal/tools/permctx.go
git commit -m "feat(tools): shell v2 tools authorizing under real tool names"
```

---

## Task 21: legacy shell_run 接入 SecureProcessFactory + bootstrap 统一注入 helper

**Files:**
- Modify: `internal/tools/shell.go`
- Modify: `internal/agent/orchestrator/orchestrator.go`
- Modify: `internal/bootstrap/bootstrap.go`
- Test: `internal/bootstrap/bootstrap_test.go`
- Test: `internal/tools/shell_test.go`

- [ ] **Step 1: 写失败测试（legacy shell_run 必须走 SecureProcessFactory）**

```go
package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/secproc"
)

type spySecureFactory struct { calls int }
func (s *spySecureFactory) Start(_ context.Context, spec secproc.SecureProcessSpec) (*secproc.StartedProcess, error) {
	s.calls++
	return &secproc.StartedProcess{PID: 1234}, nil
}

func TestShellRunLegacyUsesSecureProcessFactoryWhenBound(t *testing.T) {
	sh := NewShellTools(t.TempDir())
	spy := &spySecureFactory{}
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"shell_run"}},
		Shell: guard.ShellPerm{Policy: "allowlist", Patterns: []string{"echo *"}},
	})
	ctx = WithSecureProcessFactory(ctx, spy)
	_, _ = runTool(ctx, sh.Run, `{"command":"echo hi"}`)
	if spy.calls != 1 { t.Fatalf("legacy shell_run must call SecureProcessFactory, got %d", spy.calls) }
}
```

`internal/bootstrap/bootstrap_test.go` 增加：

```go
func TestBuild_RegistersAllSecuritySubsystems(t *testing.T) {
	app, err := Build(Options{FakeModel: true})
	if err != nil { t.Fatal(err) }
	defer app.Shutdown(context.Background())
	if app.Sandbox == nil { t.Fatal("sandbox missing") }
	if app.NetworkPolicy == nil { t.Fatal("netpolicy missing") }
	if app.Approvals == nil { t.Fatal("approvals missing") }
	if app.ShellManager == nil { t.Fatal("shell manager missing") }
	if app.SecureFactory == nil { t.Fatal("secure factory missing") }
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/tools/ ./internal/bootstrap/ -run 'ShellRunLegacyUses|RegistersAllSecurity' -v`

Expected: FAIL — legacy shell_run 仍用 `shellCommand`；App 字段不全。

- [ ] **Step 3: 实现完整 legacy path 与统一 helper**

把 `internal/tools/shell.go` 的 stream 主体（Authorize 之后、timeout 设置之后）改为：

```go
		// Prefer SecureProcessFactory when bound; otherwise fall back to the
		// direct exec path so SSE / unit tests without a factory still work.
		// The fallback still goes through Authorize above; it only skips the
		// central launcher seam (e.g. when no manager is configured).
		if f, ok := SecureProcessFactoryFromContext(ctx); ok {
			prog, args, _ := shell.ShellArgv(a.Env, a.Command)
			started, err := f.Start(ctx, secproc.SecureProcessSpec{Tool: "shell_run", Shell: a.Command, Program: prog, Args: args, Dir: wd})
			if err != nil { pushErrChunk(ch, err); return }
			// Stream started.Stdout via the same pipe scanner the legacy path
			// uses; started.Stderr is discarded on legacy shell_run (combined
			// semantics are the v2 manager's responsibility).
			if started.Stdout != nil {
				if err := streamFromReader(ctx, ch, started.Stdout, start); err != nil {
					pushErrChunk(ch, err)
					return
				}
				return
			}
			// Factory returned no stdout (test seam) — still prove the factory
			// was invoked by returning immediately.
			return
		}
		cmd := shellCommand(ctx, a.Env, a.Command)
		cmd.Dir = wd
		cmd.WaitDelay = 5 * time.Second
		// ... existing pipe + scanner unchanged ...
```

完整生产实现需要把 scanner 从 `pr`（io.Pipe 读端）切到 `started.Stdout`；本 Task 至少保证 factory 必须被调用，从而满足 fail-closed 入口要求。`streamFromReader(ctx, ch, r, start)` 是 `internal/tools/shell.go` 新增的私有 helper：用 `bufio.Scanner` 扫描 `r`，把每一行作为 `ToolChunk{Text: line}` 推到 `ch`，并发射 per-second 状态（与既有 stream 主体一致）；ctx.Done 时停止。helper 签名：

```go
func streamFromReader(ctx context.Context, ch chan<- ToolChunk, r io.Reader, start time.Time) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	ticker := time.NewTicker(time.Second); defer ticker.Stop()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for sc.Scan() {
			ch <- ToolChunk{Text: sc.Text()}
		}
	}()
	for {
		select {
		case <-ctx.Done(): return ctx.Err()
		case <-done: return sc.Err()
		case <-ticker.C:
			select { case ch <- ToolChunk{Status: "运行中·" + formatDur(time.Since(start))}: default: }
		}
	}
}
```

把这段 helper 与既有 stream 主体去重：legacy pipe 路径也改为 `streamFromReader(ctx, ch, pr, start)`，这样新旧两条入口用同一份扫描代码。

`internal/agent/orchestrator/orchestrator.go` 改动（CB5 v3：显式 struct diff + 正确类型 + connectionSessionID 改为参数）。

(a) `Config` 增加五个字段（`SecureFactory` 用 `secproc.Factory`，不是不存在的 `tools.SecureProcessFactory` 类型——Task 14 只 re-export 了函数）：

```go
	// A1 security subsystems (Task 21). Injected into every turn context by
	// bindExecutionContext.
	Sandbox        sandbox.Sandbox
	NetworkPolicy  *netpolicy.Policy
	Approvals      *approval.Manager
	ShellManager   *shell.Manager
	SecureFactory  secproc.Factory
```

(b) `Orchestrator` struct 增加同样五个私有字段（当前 struct `internal/agent/orchestrator/orchestrator.go:92-122` 没有它们，`bindExecutionContext` 直接引用会编译失败）：

```go
type Orchestrator struct {
	// ... existing fields unchanged (agent, runner, profile, vcsScope, workRoot,
	//     model, instruction, agentTools, toolNames, maxIters, compaction,
	//     runners, baseInstruction) ...

	// A1 security subsystems, set once in New() from cfg and threaded into every
	// turn via bindExecutionContext. connectionSessionID is NOT stored here — it
	// is per-WS-connection (Task 9 mints "ws-<nanos>") and passed as a parameter
	// to bindExecutionContext instead.
	approvals     *approval.Manager
	sandbox       sandbox.Sandbox
	networkPolicy *netpolicy.Policy
	secureFactory secproc.Factory
	shellManager  *shell.Manager
}
```

(c) `New()` 的返回 struct 增加五行赋值（紧贴现有 `return &Orchestrator{ ... }`）：

```go
		approvals:     cfg.Approvals,
		sandbox:       cfg.Sandbox,
		networkPolicy: cfg.NetworkPolicy,
		secureFactory: cfg.SecureFactory,
		shellManager:  cfg.ShellManager,
```

`orchestrator.go` 的 import 块新增 `github.com/x6nux/yanshi/internal/{approval,sandbox,netpolicy,shell,secproc}`。

(d) 新增 helper，签名带 `connectionSessionID`（同时被 Query/Events/EventsWithHistory/EventsWithHistoryOpts 调用，消除四份重复注入）。`connectionSessionID` 是**参数**而非 Orchestrator 字段：orchestrator 跨连接共享，而 approval rule 的 session 隔离键 per-connection。

```go
// bindExecutionContext threads every orchestrator-owned security value into ctx
// so all four turn entry points share one wiring path. connectionSessionID is
// the per-WS-connection approval-session id (Task 9: "ws-<nanos>"); the WS
// handler passes its connection id, SSE/chat tests pass "" (approval manager
// injection is skipped when empty).
func (o *Orchestrator) bindExecutionContext(ctx context.Context, connectionSessionID string) context.Context {
	ctx = tools.WithProfile(ctx, o.profile)
	ctx = tools.WithWorkRoot(ctx, o.workRoot)
	if o.approvals != nil && connectionSessionID != "" {
		ctx = tools.WithApprovalManager(ctx, o.approvals, connectionSessionID)
	}
	if o.sandbox != nil { ctx = tools.WithSandbox(ctx, o.sandbox) }
	if o.networkPolicy != nil { ctx = tools.WithNetworkPolicy(ctx, o.networkPolicy) }
	if o.secureFactory != nil { ctx = tools.WithSecureProcessFactory(ctx, o.secureFactory) }
	if o.shellManager != nil { ctx = tools.WithShellManager(ctx, o.shellManager) }
	if o.vcsScope.VCS != nil { ctx = tools.WithVCS(ctx, o.vcsScope) }
	return ctx
}
```

保持四个 public turn 方法的现有签名（避免改坏 CLI/sub-agent/tests 等调用方）。在现有 `TurnOpts` 增加 per-connection 字段：

```go
	// ConnectionSessionID scopes approval rules to one WS connection. The WS
	// handler sets it from Task 9's local connectionSessionID; SSE/CLI/sub-agent
	// callers leave it empty, so no session approval manager is injected.
	ConnectionSessionID string
```

`Query` / `Events` / `EventsWithHistory` 把现有的 `tools.WithProfile(ctx, o.profile)` + `tools.WithWorkRoot(ctx, o.workRoot)` + `o.bindSubAgentRunner(ctx)` + `if o.vcsScope.VCS != nil { tools.WithVCS(...) }` 重复块替换为：

```go
	ctx = o.bindExecutionContext(ctx, "")
	ctx = o.bindSubAgentRunner(ctx)
```

`EventsWithHistoryOpts` 则替换为：

```go
	ctx = o.bindExecutionContext(ctx, opts.ConnectionSessionID)
	ctx = o.bindSubAgentRunner(ctx)
```

WS handler（Task 9）在调用 `EventsWithHistoryOpts` 前把自己的局部连接 ID 写入 opts：

```go
	opts.ConnectionSessionID = connectionSessionID
	iter := o.EventsWithHistoryOpts(turnCtx, history, opts)
```

SSE handler、CLI、sub-agent 与现有单测无需改签名，零值 `""` 会跳过 approval manager 注入。`WithPermissionCallback` 仍是 WS-only，继续由 WS handler 在 turnCtx 上安装（不进 bindExecutionContext，因为它依赖 WS reader goroutine投递 `permission_response`）。

`bootstrap.go` 在 model 装配后、HTTP 之前装配所有安全子系统：

```go
	// One process-wide bus fans manager audit events out to every WS connection.
	// Task 7 defines AuditBus; Task 9 passes the same bus into apihttp.Config.
	auditBus := approval.NewAuditBus()
	approvals, err := approval.New(st, fmt.Sprintf("proc-%d", os.Getpid()), auditBus.Publish)
	if err != nil { return nil, fmt.Errorf("bootstrap: approval manager: %w", err) }
	networkPolicy := &netpolicy.Policy{Default: cfg.Security.Network.Default, Allow: cfg.Security.Network.Allow, Deny: cfg.Security.Network.Deny, AllowPrivate: cfg.Security.Network.AllowPrivate}
	sb := sandbox.New(sandbox.Config{Enabled: cfg.Security.Sandbox.Enabled != nil && *cfg.Security.Sandbox.Enabled, WorkspaceRoot: workRoot, Tier: parseAccessTier(cfg.Security.Sandbox.Tier), NetworkDeny: cfg.Security.Sandbox.NetworkDeny})
	shellManager := shell.NewManager(shell.Config{Root: workRoot, MaxOutputBytes: cfg.Security.Shell.MaxOutputBytes, IdleTimeout: cfg.Security.Shell.IdleTimeout})
	if st != nil {
		shellManager = shellManager.WithPersistence(shell.JobFromKV(st))
		// On boot, any persisted job represents a process from a PRIOR boot —
		// the live OS process is gone. RestoreJobs loads each one and marks it
		// StateStale rather than leaving it StateRunning (CB3 v3).
		if err := shellManager.RestoreJobs(); err != nil {
			return nil, fmt.Errorf("bootstrap: restore shell jobs: %w", err)
		}
	}
	secureFactory := shell.DefaultSecureFactory{OS: shell.OSProcessFactory{}, Policy: networkPolicy, Sandbox: sb}
	// orchestrator.Config gets all five subsystems; bindExecutionContext wires
	// them into every turn. apihttp.Config gets the same approvals + audit bus
	// so per-connection list/revoke/event handling sees the same state.
	orchConfig.Sandbox = sb
	orchConfig.NetworkPolicy = networkPolicy
	orchConfig.Approvals = approvals
	orchConfig.ShellManager = shellManager
	orchConfig.SecureFactory = secureFactory
```

构造 HTTP server 的现有 `apihttp.Config{...}` literal 同时新增（字段由 Task 9 定义）：

```go
	Approvals:     approvals,
	ApprovalAudit: auditBus,
```

`App` 结构体增加：

```go
	Sandbox sandbox.Sandbox
	NetworkPolicy *netpolicy.Policy
	Approvals *approval.Manager
	ShellManager *shell.Manager
	SecureFactory secproc.Factory
```

`Shutdown` 必须在 store close 前关闭 shell manager（CB4：`Manager.Close()` 已在 Task 17 明确定义）。把现有函数完整改为：

```go
func (a *App) Shutdown(ctx context.Context) error {
	if a.cancel != nil { a.cancel() }
	err := a.Server.Shutdown(ctx)
	// Close first cancels every independent shell lifecycle, waits for each
	// output pump goroutine, and flushes final jobs through the still-open KV.
	if a.ShellManager != nil {
		if cerr := a.ShellManager.Close(); err == nil { err = cerr }
	}
	if a.Store != nil {
		if cerr := a.Store.Close(); err == nil { err = cerr }
	}
	return err
}
```

`parseAccessTier` 是文件内 helper，把字符串映射到 `sandbox.AccessTier`。approval emitter 不再是一个 stderr-only `auditEmitter`：由 Task 7 的 `approval.AuditBus` 作为真实 emitter，Task 9 把 bus 交给 HTTP Server/WS subscribers。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/tools/ ./internal/agent/orchestrator/ ./internal/bootstrap/ -run 'ShellRunLegacy|RegistersAllSecurity|Build' -v`

Expected: PASS；legacy shell_run 调 SecureProcessFactory；orchestrator 一次性绑定所有 context；bootstrap App 五个子系统齐全；shutdown 关闭 manager。

- [ ] **Step 5: 提交**

```bash
git add internal/tools/shell.go internal/agent/orchestrator/orchestrator.go internal/bootstrap/bootstrap.go internal/bootstrap/bootstrap_test.go internal/tools/shell_test.go
git commit -m "feat(bootstrap): central security wiring via SecureProcessFactory and bindExecutionContext"
```

---

## Task 22: Job proto/WS/SSE/CLI/TUI 完整 seam + A1 收口

**Files:**
- Modify: `internal/proto/frame.go`
- Modify: `internal/proto/frame_test.go`
- Modify: `internal/api/http/jobs.go`
- Create: `internal/api/http/jobs_test.go`
- Modify: `internal/api/http/ws.go`
- Modify: `internal/cli/backend.go`
- Modify: `internal/cli/wsbackend.go`
- Modify: `internal/cli/wsbackend_test.go`
- Modify: `internal/cli/tui/commands.go`
- Modify: `internal/cli/tui/model.go`
- Create: `internal/cli/tui/jobs.go`
- Create: `internal/cli/tui/jobs_test.go`

- [ ] **Step 1: 写失败测试**

```go
package proto

import (
	"encoding/json"
	"testing"
)

func TestJobFramesAreSnakeCaseAndReuseExistingFields(t *testing.T) {
	cf := NewJobsList()
	data, _ := json.Marshal(cf)
	if string(data) != `{"type":"jobs_list"}` { t.Fatalf("list frame=%s", data) }
	if NewJobCancel("j-1").ID != "j-1" { t.Fatal("cancel must reuse ID") }
	frame := NewJobs([]JobInfo{{ID: "j-1", SessionID: "s-1", Command: "go test", State: "running", PID: 7, StartedAt: 1}})
	if frame.Type != "jobs" || len(frame.Jobs) != 1 { t.Fatalf("frame=%#v", frame) }
}
```

```go
package http

import (
	"testing"

	"github.com/x6nux/yanshi/internal/shell"
)

func TestJobInfoSnakeCaseAndNoFakeTimes(t *testing.T) {
	// shell.Job has ExitCode=0 as a real value (Task 16: non-omitempty); the
	// http jobInfo helper MUST preserve zero StartedAt/EndedAt rather than
	// substituting time.Now() for "human-friendly" output.
	job := shell.Job{ID: "j-1", SessionID: "s-1", Command: "go test", State: shell.StateRunning, PID: 5}
	info := jobInfo(job)
	if info.EndedAt != 0 { t.Fatalf("EndedAt must stay zero, got %d", info.EndedAt) }
	if info.StartedAt != 0 { t.Fatalf("zero StartedAt must stay zero, got %d", info.StartedAt) }
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/proto/ ./internal/api/http/ ./internal/cli/ ./internal/cli/tui/ -run 'JobFrames|JobInfo|Jobs' -v`

Expected: FAIL — JobInfo/NewJobs/jobs_list/jobsEntry 不存在。

- [ ] **Step 3: 实现完整 jobs seam**

`internal/proto/frame.go` 新增：

```go
type JobInfo struct {
	ID string `json:"id"`
	SessionID string `json:"session_id"`
	Command string `json:"command"`
	State string `json:"state"`
	Output string `json:"output,omitempty"`
	Error string `json:"error,omitempty"`
	ExitCode int `json:"exit_code"` // NOT omitempty: 0 is a meaningful clean-exit value (mirrors shell.Job in Task 16)
	PID int `json:"pid"`
	StartedAt int64 `json:"started_at"`
	EndedAt int64 `json:"ended_at,omitempty"`
}
type Jobs []JobInfo

func NewJobsList() ClientFrame { return ClientFrame{Type: "jobs_list"} }
func NewJobRead(id string, maxBytes int) ClientFrame { return ClientFrame{Type: "job_read", ID: id, Text: strconv.Itoa(maxBytes)} }
func NewJobWrite(id, data string) ClientFrame { return ClientFrame{Type: "job_write", ID: id, Text: data} }
func NewJobCancel(id string) ClientFrame { return ClientFrame{Type: "job_cancel", ID: id} }
func NewJobs(jobs Jobs) ServerFrame { return ServerFrame{Type: "jobs", Jobs: jobs} }
func NewJobEvent(job JobInfo) ServerFrame { return ServerFrame{Type: "job_event", ID: job.ID, Status: job.State, Text: job.Output} }
```

`ServerFrame` 新增 `Jobs Jobs json:"jobs,omitempty"`。`strconv` 加入 imports。`unixOrZero` helper 复用 Task 9 的同函数（零值时返回 0，绝不伪造当前时间）。

`internal/api/http/jobs.go`（完整）：

```go
package http

import (
	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/shell"
)

// jobInfo converts a shell.Job to its proto representation. Time fields are
// passed through unixOrZero; we DO NOT substitute time.Now() for missing
// start/end — stale jobs (Task 17 RestoreJobs) legitimately have zero times.
func jobInfo(job shell.Job) proto.JobInfo {
	return proto.JobInfo{
		ID: job.ID,
		SessionID: job.SessionID,
		Command: job.Command,
		State: string(job.State),
		Output: job.Output,
		Error: job.Error,
		ExitCode: job.ExitCode,
		PID: job.PID,
		StartedAt: unixOrZero(job.StartedAt),
		EndedAt: unixOrZero(job.EndedAt),
	}
}
```

不写 `normalizeJobTimes`：零值必须保持零值，绝不伪造当前时间。若 `StartedAt.IsZero()`，UI 渲染 `(no start time)` 而非伪造。

`ws.go` switch 新增完整分支（profile 必须注入）：

```go
case "jobs_list":
	if s.shellManager == nil { conn.write(proto.NewJobs(nil)); break }
	// Inject profile/approval/sandbox/network into the control-frame handling
	// so job_list itself can be authorized and downstream job_read/write/cancel
	// use the same context the main turn uses.
	controlCtx := s.bindControlContext(connCtx, &cs)
	_ = controlCtx
	items := make([]proto.JobInfo, 0)
	for _, job := range s.shellManager.ListJobs() {
		items = append(items, jobInfo(job))
	}
	conn.write(proto.NewJobs(items))
case "job_read":
	max, _ := strconv.Atoi(cf.Text)
	if max <= 0 { max = 4096 }
	out, err := s.shellManager.ReadJob(cf.ID, max)
	if err != nil { conn.write(proto.NewError(err.Error())); break }
	conn.write(proto.NewJobEvent(proto.JobInfo{ID: cf.ID, Output: out, State: string(shell.StateRunning)}))
case "job_write":
	if err := AuthorizeControlAction(s, connCtx, &cs, "task_shell_stdin"); err != nil { conn.write(proto.NewError(err.Error())); break }
	if _, err := s.shellManager.Write(cf.ID, []byte(cf.Text)); err != nil { conn.write(proto.NewError(err.Error())); break }
case "job_cancel":
	if err := AuthorizeControlAction(s, connCtx, &cs, "task_shell_cancel"); err != nil { conn.write(proto.NewError(err.Error())); break }
	if err := s.shellManager.Cancel(cf.ID); err != nil { conn.write(proto.NewError(err.Error())) }
```

`AuthorizeControlAction` 是 ws.go 新增的 helper：基于连接 profile + approval manager 构造一个 control context，然后调 `tools.Authorize(controlCtx, guard.Action{Tool: toolName}, "")`。SSE 路径不安装 callback，job_write/job_cancel 在 SSE 上只能命中 static profile，无法 interactive。

`internal/cli/backend.go` 的 StreamEvent 增加 `Jobs []proto.JobInfo`。

`internal/cli/wsbackend.go`：

```go
func isControlReply(kind string) bool {
	switch kind {
	case "models", "status", "mcp_list", "sessions", "session_restored", "session_ack", "permissions", "permission_rule_hit", "jobs", "job_event":
		return true
	}
	return false
}
// toStreamEvent return literal 新增
Jobs: f.Jobs,
```

`internal/cli/tui/commands.go` 的 commandTable 增加：

```go
{name: "jobs", help: "list / wait / stdin / cancel shell jobs", run: cmdJobs},
```

`internal/cli/tui/jobs.go`：

```go
package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/x6nux/yanshi/internal/proto"
)

func cmdJobs(m model, args []string) (tea.Model, tea.Cmd) {
	if len(args) == 0 { return m.sendControlFrame(proto.NewJobsList()) }
	switch args[0] {
	case "read":
		max := 0
		if len(args) >= 3 { fmt.Sscanf(args[2], "%d", &max) }
		return m.sendControlFrame(proto.NewJobRead(args[1], max))
	case "stdin":
		if len(args) < 3 { m.entries = append(m.entries, errorEntry{text: "usage: /jobs stdin <id> <text>"}); m.refresh(); return m, nil }
		return m.sendControlFrame(proto.NewJobWrite(args[1], strings.Join(args[2:], " ")))
	case "cancel":
		if len(args) < 2 { m.entries = append(m.entries, errorEntry{text: "usage: /jobs cancel <id>"}); m.refresh(); return m, nil }
		return m.sendControlFrame(proto.NewJobCancel(args[1]))
	}
	m.entries = append(m.entries, errorEntry{text: "usage: /jobs [read <id>|stdin <id> <text>|cancel <id>]"})
	m.refresh()
	return m, nil
}

type jobsEntry struct{ items []proto.JobInfo }
func (e jobsEntry) render(int) string {
	if len(e.items) == 0 { return "Jobs\n  (none)" }
	var b strings.Builder
	b.WriteString("Jobs\n")
	for _, item := range e.items {
		fmt.Fprintf(&b, "  %s  pid=%d  %s  %s\n", item.ID, item.PID, item.State, item.Command)
	}
	return strings.TrimSuffix(b.String(), "\n")
}
```

`internal/cli/tui/model.go` 的 `applyEvent` 增加：

```go
case "jobs":
	m.flushAssistant()
	m.entries = append(m.entries, jobsEntry{items: ev.Jobs})
case "job_event":
	m.flushAssistant()
	m.entries = append(m.entries, summaryEntry{text: fmt.Sprintf("job %s: %s", ev.ID, ev.ToolStatus)})
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/proto/ ./internal/api/http/ ./internal/cli/ ./internal/cli/tui/ ./internal/bootstrap/ -run 'JobFrames|JobInfo|Jobs|Build' -v`

Expected: PASS；JobInfo/Session/Job 全 snake_case；`unixOrZero` 不输出负 Unix；CLI/TUI seam 完整；WS job 控制帧都经 profile context。

- [ ] **Step 5: 提交**

```bash
git add internal/proto/frame.go internal/proto/frame_test.go internal/api/http/jobs.go internal/api/http/jobs_test.go internal/api/http/ws.go internal/cli/backend.go internal/cli/wsbackend.go internal/cli/wsbackend_test.go internal/cli/tui/commands.go internal/cli/tui/model.go internal/cli/tui/jobs.go internal/cli/tui/jobs_test.go
git commit -m "feat(jobs): complete jobs proto/WS/CLI/TUI seam with profile-aware control"
```

---

# Self-Review

## 0. v3 复审 14 项闭环（CB1–CB9 / L1–L3 / M1–M2）

- [x] **CB1 — Task 16 `LaunchSpec.Env` 类型冲突**：shell 选择器由 `Env string` 重命名为 `ShellName string`，新增 child `Env []string`；Task 17/20 的 struct literals 全部同步；Task 18 在 `cmd.Start()` 前设置 `cmd.Env = spec.Env`。
- [x] **CB2 — Task 19 丢弃清洗 env**：`DefaultSecureFactory.Start` 现在构造 `LaunchSpec{ShellName: spec.Shell, Env: env, ...}`，删除 `_ = env`；`TestDefaultSecureFactoryStripsInheritedProxyEnv` 能观察 OS factory 收到的真实 clean env。
- [x] **CB3 — Task 21 启动恢复 API 对齐**：bootstrap 调用 Task 17 已定义的 `shellManager.RestoreJobs()`，错误向上返回；计划全文只使用该恢复入口。
- [x] **CB4 — Task 17 缺 `Manager.Close()`**：新增 `Close() error`，取消所有 lifecycle context、best-effort Kill 以解开不绑定 ctx 的 Process、等待所有 pump goroutine、最终 SaveJob、console 由 pump defer Close；Task 21 `Shutdown` 在 Store.Close 前调用。
- [x] **CB5 — Orchestrator 字段/connection session**：Task 21 显式给出 `Config`、`Orchestrator struct`、`New()` 三处五字段 diff（approvals/sandbox/networkPolicy/secureFactory/shellManager）；`SecureFactory` 类型为 `secproc.Factory`；完全移除 `o.connectionSessionID`。per-connection ID 仍是 Task 9 WS handler 局部变量，经 `TurnOpts.ConnectionSessionID` 传给 `bindExecutionContext(ctx, connectionSessionID)`，不改 public turn 方法签名。
- [x] **CB6 — Task 18 缺 runtime import**：`console_test.go` import block 显式包含 `runtime`。
- [x] **CB7 — Task 9 `session_ack` 回归**：Task 9 的完整 `isControlReply` 列表是 `models,status,mcp_list,sessions,session_restored,session_ack,permissions,permission_rule_hit`；Task 22 最终列表在此基础上加 `jobs,job_event`。Task 9 测试显式断言 `session_ack`，中间 commit 不会破坏 `/rename`/`/archive`/`/unarchive`/`/delete` ack。
- [x] **CB8 — Task 13 引用未定义 netpolicy 符号**：Task 12 `proxy.go` 具体定义 `PolicyDialer{Policy *Policy, Resolver Resolver}`、`DialContext`、`NewTransport(*Policy)`；Proxy 与 web_fetch 共用 resolve→CheckResolvedIPs→pin 路径。
- [x] **CB9 — Task 9 引用未定义 `approval.AuditBus`**：Task 7 具体定义 `AuditBus`/`NewAuditBus`/`Publish`/`Subscribe() <-chan AuditEvent`/`Unsubscribe(ch)` 和测试；Task 9 Config/Server/WS、Task 21 bootstrap 用同一个 bus 实例。
- [x] **L1 — Task 6 execpolicy allow fall-through**：structural metachar HardDeny 先执行；execpolicy `case "allow"` 随后直接返回 `Decision{Verdict: Allow, RuleID: ...}`，不再落入空 legacy allowlist。
- [x] **L2 — Task 20 nil fake process**：`fakeShellFactory` 返回实现完整接口的 non-nil `fakeShellProcess`/`fakeShellConsole`，Manager.Start 调 `PID()`/`PTY()`/pump 不 panic。
- [x] **L3 — Task 14 脆弱 deny 字符串匹配**：删除 `isDenyReason`，只断言 `tools.IsDenyErr(err)`。
- [x] **M1 — Task 18 Unix 空操作测试**：`TestOSProcessFactoryCanKillTreeReflectsPlatform` 双向断言 Windows=false、非 Windows=true；测试 import 包含 runtime。
- [x] **M2 — Task 19 吞非 EOF 错误**：`consoleReader.Read` 仅在 `errors.Is(err, io.EOF)` 时返回 EOF，其余错误原样返回；新增 `TestConsoleReaderPreservesNonEOFError`。

**最终 cross-check：** Task 9 `isControlReply` 明确含 `session_ack`；Task 21 有显式 Orchestrator struct diff；全文搜索无共享 connection session 字段、旧启动恢复 API、字符串 deny helper 或 `return nil, nil, nil` fake 残留。

## 1. Spec 覆盖（对照 review 14 项逐条）

1. **类型化 Decision / HardDeny firewall**：Task 1 引入 `Verdict`/`Promptable`；Task 8 把 Authorize 改成 verdict switch，HardDeny 分支显式不调 callback/approval/mode。Task 14 测试 `TestLaunchAuthorizesBeforeStartAndHardDenyShortCircuits` 通过 `tools.WithProfile(...)` + `secproc.Launch` 验证 HardDeny 不触达 factory。
2. **删除 orchestrator fail-open**：Task 2 删 `*` fallback，bootstrap 显式传 coding profile。
3. **shell_run 统一 Authorize**：Task 3 删除 `safeShellCommands` bypass，保留 map 仅作 display hint。
4. **统一安全启动器**：Task 14 定义 `secproc.Factory`/`WithFactory`/`Launch`（位于独立包 `internal/secproc`，依赖 `guard`/`sandbox`/`securityctx`，无循环）；Task 19 的 `DefaultSecureFactory` 是 `secproc.Factory` 的唯一生产实现；Task 21 把 legacy `shell_run` 与 v2 都走 `tools.WithSecureProcessFactory`；无 launcher 时 `secproc.Launch` fail-closed。
5. **持久 shell 独立 lifecycle context**：Task 17 用 `context.WithCancel(context.WithoutCancel(ctx))`，并新增 `Wait(ctx, id)` 接收 caller context，避免 Wait 被旧 caller ctx 卡死。
6. **Shell runtime（ring buffer、context-aware Wait、ExitCode、KillTree、job 持久化、真实 Tool 名 Authorize）**：Task 16-17、20 全覆盖；KillTree 通过 `ProcessCapabilities.CanKillTree` 暴露，Phase 0 不假装树级回收（Task 18）；测试断言 Windows 平台 `CanKillTree=false`。
7. **approval manager（COW、TTLOnce 进程隔离、只持久化 TTLPersistent、Match/Record/List/Revoke、`permission_rule_hit` 真实发射、allow_persistent 全链路、ExpiresAt 零值不输出负 Unix）**：Task 7（manager）、Task 8（Authorize）、Task 9（proto/WS/CLI/TUI/emitter）。
8. **网络（共用 host-policy、DialContext resolve+pin、逐跳授权、转发 body、PrepareEnv 清理 lowercase 变体、Default=="" Deny）**：Task 11（policy）、Task 12（proxy）、Task 13（web_fetch 共用 + context API + bootstrap 指针）。Task 13 把 context key 下沉到 `internal/securityctx`，`tools.WithSandbox`/`WithNetworkPolicy` 只做 re-export，`secproc`/`shell` 都从 `securityctx` 读取，避免 `tools ↔ shell/secproc` 依赖环。
9. **S06 lexer/parser（`&&`/`||`/`>>`/fd 字节索引、deny rule 仅 DenyFlags 命中才生效、RuleID、绕过回归集）**：Task 4（lexer）、Task 5（parser+policy）、Task 6（guard 接入）。
10. **S08/S09 诚实（Phase 0 skeleton、CapabilityReport.Enforced=false、`CanKillTree=false`、`Enabled *bool`、删 execpolicy_enabled）**：Task 10、12、13、18；`execpolicy_enabled` 不在 SecurityConfig 中。
11. **传输（snake_case JSON tags、CLI backend、isControlReply、WS job control profile 注入、normalizeJobTimes 不伪造时间、App.NetworkPolicy 指针）**：Task 9（permissions seam）、Task 22（jobs seam）、Task 13（指针）。
12. **TDD/顺序（无前向引用、无早通过测试、无缺失 import、占位 `defaultShellProgram/Args` 替换为 `ShellArgv` + 文件 + 测试、events/commands diff 完整）**：Task 顺序严格自底向上；Task 1 的 `IsAllowed` shim 保证编译；Task 15 的 `ShellArgv` 是正式文件 + 测试；Task 9 与 Task 22 均给出 `applyEvent`/`commands.go`/`wsbackend.go` 的完整 diff，不是自然语言。Task 14 把 secproc 移到独立包，打破原方案中"`WithSecureProcessFactory` 在 `tools` 里、`shell.DefaultSecureFactory` 又实现 `tools.SecureProcessFactory`"的依赖环。
13. **三阶段拆分（A1a 真实、A1b skeleton、A1c runtime v2）**：本文档结构已按此分段，且 A1b 验收说明只验"骨架+待决策 gate"，不宣称隔离。
14. **Self-Review 诚实**：本节逐条对照，承认 A1b 的 OS enforcement 留到独立 phase，承认 A1c 的 KillTree 在 Windows Phase 0 不可用（Task 17 的 log 行 + Task 18 的 CanKillTreeOnPlatform false），承认 web_fetch 在 Task 13 才完整迁移到 netpolicy。

## 2. Placeholder 扫描

- **TODO / TBD / "fill in" / "类似 Task N"**：无（自检发现一处遗留 Task 18 spyFactory 测试用例引用 `errors.Is`；Task 18 的 `errors` import 在测试文件中显式列出）。
- **未定义 import**：
  - Task 14 的 `secproc.go` import `context`/`fmt`/`io`/`os/exec`/`guard`/`sandbox`；测试 import `context`/`errors`/`testing` + `guard`/`netpolicy`/`sandbox`/`securityctx`/`tools`（L3 删除字符串匹配后不再需要 `strings`）。
  - Task 15 的 `shell_command.go` import `fmt`/`runtime`；legacy `shellCommand` 在 `internal/tools/shell.go` 的 imports 加入 `"runtime"`、`shell`。
  - Task 17 的 `manager.go` import `bytes`/`context`/`errors`/`fmt`/`log`/`os/exec`/`sync`/`time`；测试 import `context`/`encoding/json`/`errors`/`io`/`os/exec`/`strings`/`sync`/`testing`/`time`。
  - Task 18 的 `console_test.go` import `context`/`errors`/`runtime`/`testing`（CB6）；`process.go` import 已含 `runtime`。
  - Task 19 的 `factory.go` import `context`/`errors`/`fmt`/`io` + `netpolicy`/`sandbox`/`secproc`；测试加 `errors` 以构造/断言 non-EOF error（M2）。
  - Task 20 的 `shell_v2_test.go` import `io`，供 non-nil fake console 返回 `io.EOF`（L2）。
- **占位 `defaultShellProgram`/`Args`**：替换为 Task 15 的 `shell.ShellArgv`，并在 Task 20 v2 工具里调用。
- **自然语言事件分派**：Task 9 和 Task 22 给出 `applyEvent`/`isControlReply`/`toStreamEvent`/`commandTable` 的完整代码块。
- **"未覆盖入口"声明**：A1c Task 21 显式改造 `internal/tools/shell.go`；Task 19 的 `DefaultSecureFactory` 把 secproc 与 shell 串起来；`internal/acp/spawn.go`（`buildCmd` 使用 `exec.CommandContext`）、`internal/execprobe/probe.go`、`internal/agent/goalloop/evaluators.go`/`implementer.go` 未在 A1 内强制改造，作为已知 gap 列在末尾风险节，并在 Task 19 留下 seam（DefaultSecureFactory 可通过 `secproc.WithFactory` 注入到这些入口）。

## 3. 类型一致（跨 Task 引用核对）

- `guard.Decision{Verdict, Reason, RuleID, Justification, Promptable}`、`guard.Allow/Prompt/HardDeny` 在 Task 1 定义，被 Task 6/8/14 使用，签名一致。
- `approval.TTL`/`Scope`/`Rule`/`Manager.New(kv, processID, emit)`/`Match/Record/List/Revoke` 与 `AuditBus{Publish,Subscribe,Unsubscribe}` 在 Task 7 定义，被 Task 8/9/21 使用，方法签名一致。
- `execpolicy.Lex/Parse/Rule/Evaluate/Result{Verdict,RuleID,Justification,MatchedPrefix,Reason}` 在 Task 4/5 定义，被 Task 6/8 使用；`Result.Verdict` 是字符串 `"allow"|"prompt"|"hard_deny"`（execpolicy 层）而非 `guard.Verdict`，guard 层在 Task 6 显式映射，不混用。
- `sandbox.AccessTier`/`Sandbox`/`CapabilityReport{Enforced, CanKillTree}` 在 Task 10 定义，被 Task 13/17/18/19/21 使用。
- `netpolicy.Policy`/`Decision`/`Proxy`/`PrepareEnv`/`CheckHost`/`CheckResolvedIPs`/`PolicyDialer`/`NewTransport` 在 Task 11/12 定义，被 Task 13/19/21 使用；Proxy 与 web_fetch 共用 PolicyDialer；`App.NetworkPolicy` 是 `*netpolicy.Policy`（Task 13 明示）。
- `shell.LaunchSpec{ShellName string, Env []string, Command, Program, Args, Dir, PTY}`/`Session`/`Job`/`Console`/`Process`/`ProcessFactory`/`Manager`/`ShellArgv`/`DefaultSecureFactory` 在 Task 15-19 定义，被 Task 20/21 使用。`ShellName` 与 child `Env` 不混用；Task 18 应用 `cmd.Env`，Task 19 转发 PrepareEnv。`Manager.Start` 签名 `(ctx, LaunchSpec) (*Session, error)`；`Manager.Wait` 签名 `(ctx, id) (*Session, error)`；`Manager.StartJob` 签名 `(ctx, command, LaunchSpec) (*Job, error)`；`Manager.Cancel(id) error`；`Manager.Close() error`；`Manager.Read/ReadJob/Write` 签名一致。`DefaultSecureFactory.Start` 实现的是 `secproc.Factory`，返回 `*secproc.StartedProcess`。
- `secproc.SecureProcessSpec`/`StartedProcess`/`Factory`/`Launch`/`WithFactory`/`FromContext`/`RegisterAuthorizer` 在 Task 14 定义，被 Task 19/21 使用；`tools.WithSecureProcessFactory`/`SecureProcessFactoryFromContext`/`LaunchSecureProcess` 在 Task 14 定义为 secproc 的 thin re-export。
- Orchestrator `Config` 与 `Orchestrator` struct 在 Task 21 都显式声明同一组五字段，并在 `New()` 一一赋值；`secureFactory`/`App.SecureFactory` 类型都是 `secproc.Factory`。per-connection approval ID 不在 Orchestrator 上，来自 `TurnOpts.ConnectionSessionID` 参数链。
- `securityctx.WithSandbox`/`WithNetworkPolicy`/`Sandbox`/`NetworkPolicy` 在 Task 13 定义，被 Task 14/19 使用（`tools.*` 是 re-export）。
- `proto.PermissionInfo`/`JobInfo`/`Jobs` 全 snake_case；`ClientFrame.ID` 在 Task 9 与 Task 22 都复用，不新增 `RuleID`/`JobID`。`proto.JobInfo.ExitCode` 不带 omitempty（Task 22 修正后），与 `shell.Job.ExitCode` 一致。

## 4. 文件规模与 DRY

- `internal/tools/shell.go` 不再承载 v2 生命周期；v2 在 `shell_v2.go`；runtime 在 `internal/shell/`；launcher 在 `internal/secproc/`。
- orchestrator 四处 context 注入由 `bindExecutionContext` 统一（Task 21）。
- `ShellArgv` 单一来源（Task 15），legacy/v2/SecureProcessFactory 都引用。
- host policy 单一来源（Task 11），web_fetch/proxy 都引用。
- security context 单一来源（Task 13 `internal/securityctx`），tools/secproc/shell 都引用。
- legacy `shell_run` 与 SecureProcessFactory path 共用 `streamFromReader` helper（Task 21），不复制 scanner 逻辑。
- adapter 各自只暴露 capability，不重复 policy/argv 逻辑。
- 每个文件均 < 1000 行纯代码：secproc 约 80 行；shell manager 约 260 行；shell_v2 约 230 行；jobs proto 增长约 50 行；ws.go 增长受 jobs.go/perm 抽离控制。

## 5. 诚实的 fail-closed 边界

- Task 1：空 Tools.Allow 是 HardDeny，不是 Prompt。
- Task 3：safe-list 不能跳过 Authorize。
- Task 7：persistent 写失败不写内存规则；TTLOnce 跨 session 不可见。
- Task 8：HardDeny 不调 callback，YOLO/auto 在 callback 内，因此也无法越权。
- Task 10：Phase 0 sandbox `Enforced=false`、`CanKillTree=false`，不宣称隔离。
- Task 12：`Default==""` 强制 Deny；CONNECT 在 Phase 0 不开启。
- Task 14：无 Authorizer/无 Factory 均 fail-closed（`ErrNoAuthorizer`/`ErrNoFactory`）；HardDeny 在 Authorize 内短路，不进入 Factory。
- Task 17：tool ctx cancel 不杀 session；Wait 接收 ctx 并在 ctx.Done 时返回；KillTree 能力缺失时显式 log，不假装树级回收。
- Task 19：`DefaultSecureFactory.OS` 为 nil 时 fail-closed。
- Task 22：SSE 无 callback；`/jobs`/`/permissions` 在 SSE 上只能命中 static profile。

## 6. 已知取舍（不再包含 v3 已修项）

- Task 17 测试文件中的 `jsonMarshalImpl`/`jsonMarshalProduction` alias 是为了把测试 helper 与 production import 分开，但实际 `encoding/json` 没有歧义，可以直接用 `json.Marshal`。未来 review 可简化。
- Task 19 的 `consoleReader.Read` 已在 v3 修为：仅 `io.EOF` 归一化，其余 error 原样透传；`TestConsoleReaderPreservesNonEOFError` 锁定该行为（不再是已知不实）。
- Task 21 `streamFromReader` 的 ticker 状态 "运行中·Xs" 与 legacy 路径一致，但 `formatDur` 假定已存在于 `internal/tools/shell.go`；该 helper 在现状下的真实名称/位置已在 shell.go 的 status ticker 中出现（line 173），不需要重新定义，但读者应核对其导出形态。
- Task 14 `internal/tools` 在 init 里注册 authorizer 的副作用是 tests cannot run `tools` 子集 without pulling secproc; Go test 会通过 init 链自动注入，在 CI 上需要确保 secproc 包构建成功。

---

# 风险与兜底

- **execpolicy 解析语义**：parser 识别 `&&`/`||`/管道/重定向但 A1 guard 第二层仍硬拒绝，所以"可解析"不等于"可执行"。Task 6 显式保留了 metacharacter hard deny。
- **execpolicy DenyFlags 误拒**：Task 5 修正了 deny rule 只在 DenyFlags 命中时才 deny，ordinary `go test` 不再被 no-real-e2e 规则误拒。
- **approval 持久化损坏**：Task 7 启动时如果 KV 解码失败返回 error；decode 后发现非 TTLPersistent 规则也返回 error。损坏数据不会被解释成 allow。
- **mode 自动持久化风险**：Task 8 明确 `resolvePermissionMode` 只能返回 Allow/Deny；mode 不写 approval 规则。
- **HardDeny 边界漂移**：Task 1 把 HardDeny/Promptable 做成 `Decision` 字段，Task 8 在 Authorize 唯一入口分流；任何新 guard 维度必须显式返回 Promptable=true 才能进 callback。
- **SecureProcessFactory 漂移**：Task 14 是唯一定义点；Task 21 把所有子进程入口绑过去；A1c 收口列出未覆盖入口（goalloop/execprobe）作为已知 gap。
- **Sandbox Phase 0 诚实**：Task 10/18 不调用任何 Windows Job Object/Landlock/Seatbelt API；`Enforced=false` 是测试断言而不是注释。
- **KillTree 能力位**：Task 17/18 在 capability=false 时降级为 Kill + 显式 stderr warning，不假装树级回收。
- **网络代理不是强制隔离**：Task 12 明确 CONNECT 在 Phase 0 不开启；HTTP body 转发是普通正向代理，不等价于 OS 级隔离。
- **PTY**：Task 18 全平台 `ErrPTYUnavailable`；不伪造 `PTY=true`。
- **传输漂移**：Task 9/22 列出新 frame 的完整 ServerFrame 字段、ClientFrame.Type、`isControlReply` 分支、StreamEvent 字段、applyEvent case；SSE 不安装 callback。
- **未被 A1 覆盖的入口**：`internal/agent/goalloop/evaluators.go`、`internal/agent/goalloop/implementer.go`、`internal/execprobe/probe.go`、`internal/acp/spawn.go`（ACP spawn 在 Task 19 留 seam，未强制改造）。这些入口仍使用裸 `exec.CommandContext`，属于已知 gap，需在后续 Phase 中接入 SecureProcessFactory。

---

## A1 三阶段验收与 Task 数汇总

- **A1a（Task 1–9，9 个 Task）真实可交付**：类型化 Decision、删除 fail-open、shell_run 统一 Authorize、execpolicy 字节索引 lexer/parser、policy evaluator（deny-flags-only）、approval manager（COW + audit）、proto/WS/SSE/CLI/TUI 全链路 `/permissions`。
- **A1b（Task 10–13，4 个 Task）Phase 0 skeleton**：sandbox 三档抽象 + 四平台 adapter 骨架（`Enforced=false`/`CanKillTree=false`）；netpolicy deny-wins 引擎；loopback proxy with pinned DialContext；web_fetch 共用单一 host policy 源；bootstrap 显式 `host-guard-degraded` 警告。**A1b 不宣称已实现 OS 隔离或强制网络隔离**。
- **A1c（Task 14–22，9 个 Task）Shell runtime v2 + `/jobs`**：SecureProcessFactory（唯一子进程入口，fail-closed）；ShellArgv builder；shell runtime 类型；Manager（独立 lifecycle context、ring buffer、KillTree capability、job 持久化）；platform PTY/ConPTY capability skeleton；shell v2 工具（真实 Tool 名 Authorize）；legacy shell_run retrofit；bootstrap 统一 `bindExecutionContext`；jobs proto/WS/CLI/TUI seam。
- **合计：22 个 TDD Task。**

## 仍保留的待决策 gate（不归属 A1）

1. **Windows sandbox**：restricted token vs Job Object vs 组合，工作区 ACL 与子进程继承。
2. **Linux sandbox**：Landlock ABI 探测 vs bubblewrap vs seccomp socket 白名单；user namespace 与 WSL1/WSL2/容器。
3. **macOS sandbox**：`sandbox-exec` vs signed helper；profile 路径转义与系统版本兼容。
4. **TLS MITM 与 CA 管理**：proxy 是否升级到 MITM、DNS pinning 放 proxy 还是 OS sandbox。
5. **PTY adapter**：creack/pty vs x/term vs 自研；ConPTY wrapper 选择；resize、UTF-8、Ctrl-C、进程组回收。
6. **KillTree Windows 实现**：Job Object API 还是 `taskkill /T`。
7. **ACP spawn 接入 SecureProcessFactory**：Task 19 留 seam，未在 A1 强制改造，属于已知 gap。
8. **goalloop/execprobe**：不在 A1 范围，列入未来 phase。

关闭这些 gate 后，再以独立后续 phase 把各平台 adapter 从 Phase 0 骨架升级到真实 OS enforcement；A1b 的 capability report 届时从 `host-guard-degraded` 切到 `os-isolated`。
