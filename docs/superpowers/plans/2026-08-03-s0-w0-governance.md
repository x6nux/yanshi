# S0 / W0 防复发治理断言 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 给 yanshi 加三条机器强制的治理断言（GOV4/GOV5/GOV6）与一套 63 项功能状态台账，把「重跑 118 个子代理的审计」变成 `go test`，并让「零件造好、总装线没接」这类失效模式在 CI 上不可能再发生。

**Architecture:** 沿用仓库既有的 `internal/archtest` 治理测试模式（纯 stdlib 的 `go/ast` 分析 + 只减不增的豁免表）。GOV4（装配可达性）与 GOV6（context 注入闭环）是静态 AST 分析，落在 `internal/archtest`；GOV5（工具注册一致性）因工具名藏在构造函数内部无法静态提取，改为 `internal/bootstrap` 的运行时集成测试（`--fake-model` 跑完整 `Build` 后比对）。状态台账是一份 YAML 单一真相源，配一个 dev 工具统计和一条防虚报断言。

**Tech Stack:** Go 1.26.4，stdlib `go/ast`/`go/parser`/`go/token`，`gopkg.in/yaml.v3`（仓库已有），`testify/require`（`internal/bootstrap` 测试既有依赖）。

**Spec:** `docs/superpowers/specs/2026-08-03-yanshi-roadmap-design.md` §4.2、§5

---

## 前置：为什么 W0 排在最前

W0 落地后 CI **仍然全绿**——已知违规先写进豁免表（每条带对应工作包编号）。此后每修一个工作包就删掉相应豁免条目，**豁免表长度即剩余债务计数**。这样 W1-W10 的进度是机器可验证的，不依赖人工重跑审计。

---

## 文件结构

### 新建

| 文件 | 职责 |
|---|---|
| `internal/archtest/assembly_test.go` | GOV4：`internal/bootstrap` 包内调用图可达性分析 + `assemblyExceptions` 豁免表 |
| `internal/archtest/ctxinject_test.go` | GOV6：全 `internal/` 扫描 `func With*(ctx context.Context, ...) context.Context` 的非测试调用点 + `ctxInjectExceptions` 豁免表 |
| `internal/archtest/status_test.go` | 台账断言：verdict 枚举、终态必须有可验证 evidence、条目数恒为 63、id 唯一、package 值合法 |
| `internal/archtest/removal_test.go` | O12：断言 `ide/vscode/` 与 `scripts/check-d2.sh` 不存在 |
| `internal/bootstrap/wiring_test.go` | GOV5：`--fake-model` 跑 `Build` 后比对注册工具名 vs profile allow 列表 + `toolWiringExceptions` 豁免表 |
| `docs/feature-status.yaml` | 63 项功能状态台账（单一真相源） |
| `cmd/featurestatus/main.go` | 读台账输出统计（dev 工具，不参与运行时） |

### 修改

| 文件 | 改什么 |
|---|---|
| `internal/bootstrap/bootstrap.go` | ① 把默认 profile 字面量提取为包级函数 `DefaultOrchestratorProfile()` ② `App` 加 `ToolNames []string` 字段并在 `Build` 中填充 ③ **从 allow 列表删掉幽灵名 `fs_patch` 与 `fs_mkdir`**（二者从未是注册工具；本计划唯一一处改动出厂 profile 内容的生产改动） |
| `internal/agent/orchestrator/completion.go:90` | `WithNewTurnRecorder` 改为调用 `WithTurnRecorder`（DRY + 给它一个真实生产调用点 + 修正 `:74` 的假 doc） |
| `internal/archtest/helpers_test.go:1-8` | 包 doc 注释的「exclusively stdlib」表述限定作用域（`status_test.go` 用 yaml.v3） |
| `.github/workflows/ci.yml:116,138` | 删两处 `continue-on-error: true` |

### 删除

| 路径 | 理由 |
|---|---|
| `ide/vscode/` | 审计 `D2 O12`，spec §3.2 ④ 决定以移除方式结案 |
| `scripts/check-d2.sh` | 只服务于 `ide/vscode/`，全仓无 workflow 引用 |

---

## 任务总览

| # | 任务 | 产出 | 依赖 |
|---|---|---|---|
| 1 | GOV4 装配可达性 | `assembly_test.go`，抓到 `BuildC1` | — |
| 2 | 修 `WithNewTurnRecorder` 的重复实现 | 一行改动，为 GOV6 清障 | — |
| 3 | GOV6 context 注入闭环 | `ctxinject_test.go`，抓到 `registry.WithRole` | 2 |
| 4 | 暴露工具名与默认 profile | `App.ToolNames` + `DefaultOrchestratorProfile()` + 删幽灵名 `fs_patch`/`fs_mkdir` | — |
| 5 | GOV5 工具注册一致性 | `wiring_test.go`，抓到 9 个 shell 工具 | 4 |
| 6 | 状态台账 YAML | `docs/feature-status.yaml`，63 项 | — |
| 7 | 台账防虚报断言 | `status_test.go` | 6 |
| 8 | `cmd/featurestatus` 统计工具 | 统计表输出 | 6 |
| 9 | O12 移除 + 收紧 CI 软门禁 | 删两处目录/脚本 + `removal_test.go` + 删两行 CI | 7 |

**任务 1、2、4、6 无依赖，可并行。** 其余按依赖串行。

---

## Task 1: GOV4 装配可达性断言

**背景（实测）：** `internal/bootstrap` 有且仅有 4 个导出 `Build*` 函数——`Build`（`bootstrap.go:259`）、`BuildRLM`（`c1.go:56`）、`BuildAutomation`（`c1.go:91`）、`BuildC1`（`c1.go:134`）。`BuildC1` 内部调用后两者（`c1.go:150`、`c1.go:155`），但 `Build` 从不调用 `BuildC1` —— 整个 C1 批次是死代码（审计 P0-2）。

**豁免语义（关键设计）：** 豁免条目被当作**额外的 BFS 根**，而非简单跳过。这样豁免 `BuildC1` 会让 `BuildRLM`/`BuildAutomation` 经它传递可达，W1 修好一处三个同时绿——正是 spec §4.2 要的行为。

**Files:**
- Create: `internal/archtest/assembly_test.go`

- [ ] **Step 1: 写断言测试（此时会失败）**

创建 `internal/archtest/assembly_test.go`：

```go
// Package archtest — GOV4 assembly reachability.
//
// GOV4 catches the repo's dominant failure mode: a component package is
// written, tested, and green, but never wired into the composition root, so
// it is dead code at runtime. The 2026-07-31 audit found this pattern behind
// 53% of "partially implemented" features. See
// docs/superpowers/specs/2026-08-03-yanshi-roadmap-design.md §4.2.
package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// assemblyExceptions maps an exported Build* function in internal/bootstrap
// to the work package that will wire it into Build.
//
// Exempted functions are treated as ADDITIONAL BFS ROOTS, not as skipped
// nodes. That is deliberate: exempting BuildC1 makes BuildRLM and
// BuildAutomation reachable through it, so W1's single fix (calling BuildC1
// from Build) turns all three green in one commit.
//
// Entries may only be REMOVED, never added. A dead entry — the function is
// now reachable from Build without needing to be a root — fails the test.
var assemblyExceptions = map[string]string{
	"BuildC1": "W1 装配线：bootstrap.Build 尚未调用 BuildC1（审计 P0-2），见 spec §4.3 W1",
}

// bootstrapCallGraph parses every non-test .go file in internal/bootstrap and
// returns (same-package call graph, exported Build* name → "file:line").
//
// Only unqualified calls (*ast.Ident) are edges: a call like foo() is a
// same-package call, whereas pkg.Foo() is not and cannot reach a local
// Build* function.
func bootstrapCallGraph(t *testing.T) (map[string]map[string]bool, map[string]string) {
	t.Helper()
	root := moduleRoot(t)
	files := goFiles(t, filepath.Join(root, "internal", "bootstrap"))

	graph := make(map[string]map[string]bool)
	builds := make(map[string]string)

	fset := token.NewFileSet()
	for _, path := range files {
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Recv != nil {
				continue // methods are outside the Build* contract
			}
			name := fd.Name.Name
			if fd.Name.IsExported() && strings.HasPrefix(name, "Build") {
				pos := fset.Position(fd.Name.Pos())
				builds[name] = short(pos.Filename, root) + ":" + strconv.Itoa(pos.Line)
			}
			callees := make(map[string]bool)
			if fd.Body != nil {
				ast.Inspect(fd.Body, func(n ast.Node) bool {
					if ce, ok := n.(*ast.CallExpr); ok {
						if id, ok := ce.Fun.(*ast.Ident); ok {
							callees[id.Name] = true
						}
					}
					return true
				})
			}
			graph[name] = callees
		}
	}
	return graph, builds
}

// reachableFrom returns the set of function names reachable from roots by
// following same-package call edges.
func reachableFrom(graph map[string]map[string]bool, roots []string) map[string]bool {
	seen := make(map[string]bool)
	queue := append([]string(nil), roots...)
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		for callee := range graph[cur] {
			if !seen[callee] {
				queue = append(queue, callee)
			}
		}
	}
	return seen
}

// TestGOV4BuildFunctionsReachable verifies every exported Build* function in
// internal/bootstrap is transitively reachable from Build — i.e. it is
// actually part of the assembly line rather than dead code.
func TestGOV4BuildFunctionsReachable(t *testing.T) {
	graph, builds := bootstrapCallGraph(t)

	if _, ok := builds["Build"]; !ok {
		t.Fatal("GOV4: composition root Build not found in internal/bootstrap — " +
			"the analyzer is looking at the wrong package")
	}

	roots := []string{"Build"}
	for name := range assemblyExceptions {
		roots = append(roots, name)
	}
	sort.Strings(roots)
	reachable := reachableFrom(graph, roots)

	var unreachable []string
	for name, loc := range builds {
		if !reachable[name] {
			unreachable = append(unreachable, name+"  ("+loc+")")
		}
	}
	sort.Strings(unreachable)
	if len(unreachable) > 0 {
		t.Errorf("GOV4: %d exported Build* function(s) in internal/bootstrap are "+
			"unreachable from Build — they are dead code at runtime:\n  %s\n\n"+
			"Fix: call them (directly or transitively) from bootstrap.Build. If the\n"+
			"wiring is deferred to a later work package, add an entry to\n"+
			"assemblyExceptions naming that package.",
			len(unreachable), strings.Join(unreachable, "\n  "))
	}

	// Dead-entry check: recompute reachability WITHOUT the exception roots.
	// An exempted function that is now reachable on its own has been wired
	// up, so its entry is stale and must be deleted.
	base := reachableFrom(graph, []string{"Build"})
	var dead []string
	for name := range assemblyExceptions {
		if base[name] {
			dead = append(dead, name)
		}
	}
	sort.Strings(dead)
	if len(dead) > 0 {
		t.Errorf("GOV4: %d stale assemblyExceptions entr(ies) — these functions are "+
			"now reachable from Build and their exemptions must be DELETED:\n  %s",
			len(dead), strings.Join(dead, "\n  "))
	}
}
```

- [ ] **Step 2: 先用空豁免表验证它真能抓到 BuildC1**

临时把 `assemblyExceptions` 改成 `map[string]string{}`，然后运行：

```bash
go test ./internal/archtest -run TestGOV4BuildFunctionsReachable -v
```

预期：**FAIL**，输出包含

```
GOV4: 3 exported Build* function(s) in internal/bootstrap are unreachable from Build
  BuildAutomation  (internal/bootstrap/c1.go:91)
  BuildC1  (internal/bootstrap/c1.go:134)
  BuildRLM  (internal/bootstrap/c1.go:56)
```

这一步证明断言不是空跑。

- [ ] **Step 3: 恢复豁免表，验证「豁免即根」的传递性生效**

把 `assemblyExceptions` 改回只含 `BuildC1` 一条，运行：

```bash
go test ./internal/archtest -run TestGOV4BuildFunctionsReachable -v
```

预期：**PASS**。`BuildRLM`/`BuildAutomation` 经 `BuildC1` 传递可达，**不需要各自的豁免条目**——这验证了「豁免即额外 BFS 根」的设计。

- [ ] **Step 4: 验证死条目检测能触发**

临时给 `assemblyExceptions` 加一条 `"Build": "假条目"`（`Build` 本来就是根，必然基础可达），运行：

```bash
go test ./internal/archtest -run TestGOV4BuildFunctionsReachable -v
```

预期：**FAIL**，输出含 `stale assemblyExceptions entr(ies)` 与 `Build`。验证后删掉这条假条目。

- [ ] **Step 5: 跑全量确认无回归**

```bash
go test ./internal/archtest
```

预期：PASS（GOV1/GOV2/GOV3 + 新增 GOV4 全绿）

- [ ] **Step 6: Commit**

```bash
git add internal/archtest/assembly_test.go
git commit -m "test(archtest): add GOV4 assembly reachability gate

Exported Build* functions in internal/bootstrap must be transitively
reachable from Build. Catches the audit's dominant failure mode: a
component package written, tested, and green, but never wired into the
composition root.

Exemptions act as additional BFS roots, so wiring BuildC1 will turn
BuildRLM and BuildAutomation green in the same commit (W1).

Refs: spec 2026-08-03 §4.2 GOV4, audit P0-2"
```

---

## Task 2: 修 `WithNewTurnRecorder` 的重复实现

**背景：** `internal/agent/orchestrator/completion.go` 里两个函数都直接调 `context.WithValue(ctx, recorderKey{}, ...)`：

- `:78` `WithTurnRecorder(ctx, rec)` —— doc 声称「Callers (ws.go's turn loop)」，但**全仓非测试代码零调用**，只有 4 个测试用它注入指定 recorder
- `:90` `WithNewTurnRecorder(ctx)` —— `ws.go:670` 真正在用的那个，自己复制了一遍 `context.WithValue`

**为什么这是 Task 2 而不是一条 GOV6 豁免：** 让 `WithNewTurnRecorder` 调用 `WithTurnRecorder` 是一行改动，同时解决三件事——消除重复实现（仓库约定「重复逻辑必须抽成公共函数」）、给 `WithTurnRecorder` 一个真实生产调用点（GOV6 自动绿，无需豁免）、修正 `:74` 那句假 doc。比写豁免条目更省事，也不用改动 4 个既有测试。

**Files:**
- Modify: `internal/agent/orchestrator/completion.go:74-92`

- [ ] **Step 1: 写测试，证明两条路径绑定的是同一个 key**

在 `internal/agent/orchestrator/completion_test.go` 末尾追加：

```go
// TestWithNewTurnRecorderDelegates proves WithNewTurnRecorder routes through
// WithTurnRecorder rather than duplicating the context.WithValue call — the
// two must stay a single binding implementation (DRY), and the delegation is
// what gives WithTurnRecorder a production call site (GOV6).
func TestWithNewTurnRecorderDelegates(t *testing.T) {
	ctx := WithNewTurnRecorder(context.Background())
	if rec, _ := ctx.Value(recorderKey{}).(*turnRecorder); rec == nil {
		t.Fatal("WithNewTurnRecorder must bind a non-nil recorder")
	}
	// Both paths must be observable under the same key — one key, one
	// binding implementation.
	direct := WithTurnRecorder(context.Background(), &turnRecorder{})
	if rec, _ := direct.Value(recorderKey{}).(*turnRecorder); rec == nil {
		t.Fatal("WithTurnRecorder must bind a non-nil recorder")
	}
}
```

> **注**：`completion.go` **没有**具名的 recorder 访问器——四处读取（`:82`/`:91`/`:104`/`:140`）全是内联的 `ctx.Value(recorderKey{}).(*turnRecorder)`。所以测试也用内联断言，不要去找 `turnRecorderFrom` 之类的函数（不存在）。
>
> `completion_test.go` 是 `package orchestrator`（非 `_test` 后缀包），可以访问未导出的 `recorderKey` 与 `turnRecorder`。既有的 `TestWithTurnRecorder_Nil` 在 `coverage_test.go:109`，不在 `completion_test.go`。

- [ ] **Step 2: 跑测试确认当前通过（这是重构，不是新行为）**

```bash
go test ./internal/agent/orchestrator -run TestWithNewTurnRecorderDelegates -v
```

预期：PASS。此测试锁定重构前后的等价行为。

- [ ] **Step 3: 改实现**

把 `completion.go:74-92` 的两个函数改为：

```go
// WithTurnRecorder binds a turnRecorder into ctx. The recorder middleware
// writes to it during the turn, and Orchestrator.JudgeCompletion reads it
// after. Returns ctx unchanged when rec is nil (nil makes the binding a
// no-op).
//
// This is the single binding implementation; WithNewTurnRecorder delegates
// here. Production callers use WithNewTurnRecorder (ws.go's turn loop);
// tests use this form when they need to inject a specific recorder.
func WithTurnRecorder(ctx context.Context, rec *turnRecorder) context.Context {
	if rec == nil {
		return ctx
	}
	return context.WithValue(ctx, recorderKey{}, rec)
}

// WithNewTurnRecorder creates a fresh per-turn recorder, binds it into ctx,
// and returns the new ctx. This is the convenience entry point for callers
// (ws.go's turn loop) that just need a recorder bound — the recorder type
// itself stays unexported. The recorder middleware populates it during the
// turn; JudgeCompletion(ctx) reads it after.
func WithNewTurnRecorder(ctx context.Context) context.Context {
	return WithTurnRecorder(ctx, &turnRecorder{})
}
```

改动实质只有两处：`WithNewTurnRecorder` 的函数体从 `context.WithValue(...)` 变成 `WithTurnRecorder(ctx, &turnRecorder{})`，以及 `WithTurnRecorder` doc 里删掉「Callers (ws.go's turn loop)」这句假陈述、补上真实的调用关系。

- [ ] **Step 4: 跑包内全量测试**

```bash
go test ./internal/agent/orchestrator
```

预期：PASS（含既有的 `TestWithTurnRecorder_Nil` —— nil 短路行为未变）

- [ ] **Step 5: Commit**

```bash
git add internal/agent/orchestrator/completion.go internal/agent/orchestrator/completion_test.go
git commit -m "refactor(orchestrator): WithNewTurnRecorder delegates to WithTurnRecorder

Both functions duplicated the same context.WithValue call. Delegating
collapses them to one binding implementation, gives WithTurnRecorder a
real production call site (it previously had none outside tests), and
drops a doc comment that claimed ws.go called it — ws.go calls
WithNewTurnRecorder.

Unblocks GOV6 without needing an exemption entry.

Refs: spec 2026-08-03 §4.2 GOV6"
```

---

## Task 3: GOV6 context 注入闭环断言

**背景（实测）：** 全仓有 39 个签名形如 `func With<X>(ctx context.Context, ...) context.Context` 的导出函数，其中零非测试调用点的有 2 个：

| 函数 | 处置 |
|---|---|
| `internal/agent/registry.WithRole`（`context.go:25`）| 真问题。消费侧 `orchestrator.go:717-724` 齐全，但 role 恒为空串 → 7 个角色的 `PromptPrefix` + `RolePolicy` + 输出契约全程空转（审计 P0-5）。**进豁免表，归属 W1** |
| `internal/agent/orchestrator.WithTurnRecorder`（`completion.go:78`）| **Task 2 已解决**，不需要豁免 |

所以本任务落地后 `ctxInjectExceptions` 只有 1 条。

**精度设计：** 限定调用点匹配到**具体包**，而非只比函数名。做法是读每个文件的 import 列表，把 `pkg.WithX(...)` 的 `pkg` 别名解析回完整 import path 再比对。只按名字匹配会让两个包里的同名 `WithProfile` 互相「顶包」，**掩盖真实违规**——这正是治理断言最不能出的错。

**Files:**
- Create: `internal/archtest/ctxinject_test.go`

- [ ] **Step 1: 写断言测试**

创建 `internal/archtest/ctxinject_test.go`：

```go
// Package archtest — GOV6 context injection closure.
//
// A context injector (func With<X>(ctx, ...) context.Context) with no
// production call site means the value is never bound, so every consumer
// downstream silently reads the zero value. The 2026-07-31 audit found
// registry.WithRole in exactly this state: the whole consumer chain
// (PromptPrefix, RolePolicy, output contract) existed and was tested, but
// ran against an empty role forever. See spec §4.2 GOV6.
package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// ctxInjectExceptions maps "<module-relative pkg>.<Func>" to the work package
// that will add the missing production call site.
//
// Entries may only be REMOVED, never added. A dead entry — the injector now
// has a production call site — fails the test.
var ctxInjectExceptions = map[string]string{
	"internal/agent/registry.WithRole": "W1 装配线：Manager.runAgentLoop 派生 child ctx 时未绑 role（审计 P0-5），见 spec §4.3 W1",
}

// ctxInjector is one exported context-injecting function declaration.
type ctxInjector struct {
	Pkg  string // module-relative package dir, e.g. "internal/agent/registry"
	Name string
	Loc  string // "file:line", module-relative
}

func (c ctxInjector) key() string { return c.Pkg + "." + c.Name }

// isContextContext reports whether e is the type expression context.Context.
func isContextContext(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == "context" && sel.Sel.Name == "Context"
}

// pkgDirOf returns the module-relative directory of a file path.
func pkgDirOf(path, root string) string {
	return filepath.ToSlash(filepath.Dir(short(path, root)))
}

// findCtxInjectors scans internal/ for exported functions whose signature is
// func With<X>(ctx context.Context, ...) context.Context.
func findCtxInjectors(t *testing.T) []ctxInjector {
	t.Helper()
	root := moduleRoot(t)
	files := goFiles(t, filepath.Join(root, "internal"))

	var out []ctxInjector
	fset := token.NewFileSet()
	for _, path := range files {
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Recv != nil || !fd.Name.IsExported() {
				continue
			}
			if !strings.HasPrefix(fd.Name.Name, "With") {
				continue
			}
			// Exactly one result, of type context.Context.
			if fd.Type.Results == nil || len(fd.Type.Results.List) != 1 {
				continue
			}
			if !isContextContext(fd.Type.Results.List[0].Type) {
				continue
			}
			// First parameter must be context.Context.
			if fd.Type.Params == nil || len(fd.Type.Params.List) == 0 {
				continue
			}
			if !isContextContext(fd.Type.Params.List[0].Type) {
				continue
			}
			pos := fset.Position(fd.Name.Pos())
			out = append(out, ctxInjector{
				Pkg:  pkgDirOf(path, root),
				Name: fd.Name.Name,
				Loc:  short(pos.Filename, root) + ":" + strconv.Itoa(pos.Line),
			})
		}
	}
	return out
}

// findCtxInjectorCalls scans all non-test .go under internal/ and cmd/ and
// returns the set of "<pkg>.<Func>" keys that are actually CALLED.
//
// Qualified calls (pkg.WithX) resolve the selector's package alias back to a
// full import path via the file's import list, then to a module-relative dir.
// Matching on the bare function name would let a same-named function in a
// different package mask a real violation.
func findCtxInjectorCalls(t *testing.T) map[string]bool {
	t.Helper()
	root := moduleRoot(t)
	mp := modulePath(t)
	files := goFiles(t,
		filepath.Join(root, "internal"),
		filepath.Join(root, "cmd"),
	)

	called := make(map[string]bool)
	fset := token.NewFileSet()
	for _, path := range files {
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		selfPkg := pkgDirOf(path, root)

		// alias -> module-relative package dir, for this file's imports.
		aliases := make(map[string]string)
		for _, imp := range f.Imports {
			ipath := strings.Trim(imp.Path.Value, `"`)
			if !strings.HasPrefix(ipath, mp+"/") {
				continue // not a module-internal import
			}
			rel := strings.TrimPrefix(ipath, mp+"/")
			name := filepath.Base(rel)
			if imp.Name != nil {
				name = imp.Name.Name
			}
			aliases[name] = rel
		}

		ast.Inspect(f, func(n ast.Node) bool {
			ce, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fn := ce.Fun.(type) {
			case *ast.Ident: // same-package call: WithX(...)
				if strings.HasPrefix(fn.Name, "With") {
					called[selfPkg+"."+fn.Name] = true
				}
			case *ast.SelectorExpr: // qualified call: pkg.WithX(...)
				id, ok := fn.X.(*ast.Ident)
				if !ok || !strings.HasPrefix(fn.Sel.Name, "With") {
					return true
				}
				if dir, ok := aliases[id.Name]; ok {
					called[dir+"."+fn.Sel.Name] = true
				}
			}
			return true
		})
	}
	return called
}

// TestGOV6ContextInjectorsHaveCallSites verifies every exported context
// injector under internal/ is actually called from non-test code.
func TestGOV6ContextInjectorsHaveCallSites(t *testing.T) {
	injectors := findCtxInjectors(t)
	if len(injectors) < 10 {
		t.Fatalf("GOV6: only %d context injectors found — the scanner is almost "+
			"certainly broken (the repo has dozens)", len(injectors))
	}
	called := findCtxInjectorCalls(t)

	var orphans []string
	for _, inj := range injectors {
		k := inj.key()
		if called[k] {
			continue
		}
		if _, exempt := ctxInjectExceptions[k]; exempt {
			continue
		}
		orphans = append(orphans, k+"  ("+inj.Loc+")")
	}
	sort.Strings(orphans)
	if len(orphans) > 0 {
		t.Errorf("GOV6: %d context injector(s) have no production call site — the "+
			"bound value is never set, so every consumer reads the zero value:\n  %s\n\n"+
			"Fix: add the missing call, or delete the injector if it is dead. If the\n"+
			"wiring is deferred, add an entry to ctxInjectExceptions naming the work package.",
			len(orphans), strings.Join(orphans, "\n  "))
	}

	// Dead-entry check: an exempted injector that now has a call site has
	// been wired up, so its exemption must be deleted.
	var dead []string
	for k := range ctxInjectExceptions {
		if called[k] {
			dead = append(dead, k)
		}
	}
	sort.Strings(dead)
	if len(dead) > 0 {
		t.Errorf("GOV6: %d stale ctxInjectExceptions entr(ies) — these injectors now "+
			"have production call sites and their exemptions must be DELETED:\n  %s",
			len(dead), strings.Join(dead, "\n  "))
	}
}
```

- [ ] **Step 2: 用空豁免表验证它抓到 WithRole**

临时把 `ctxInjectExceptions` 改成 `map[string]string{}`，运行：

```bash
go test ./internal/archtest -run TestGOV6ContextInjectorsHaveCallSites -v
```

预期：**FAIL**，且孤儿列表**恰好只有一条**：

```
GOV6: 1 context injector(s) have no production call site
  internal/agent/registry.WithRole  (internal/agent/registry/context.go:25)
```

> ⚠️ **如果这里出现的不是 1 条而是 2 条（含 `orchestrator.WithTurnRecorder`）**，说明 Task 2 没做或没生效——回去先完成 Task 2。
>
> ⚠️ **如果出现远多于 2 条**，说明包解析有误报。按 spec §7 的阈值判断：>5 条才需要把扫描范围收窄到 `internal/agent/**` + `internal/tools/**`；实测应为 2 条，不该触发收窄。

- [ ] **Step 3: 恢复豁免表，确认转绿**

改回只含 `internal/agent/registry.WithRole` 一条：

```bash
go test ./internal/archtest -run TestGOV6ContextInjectorsHaveCallSites -v
```

预期：**PASS**

- [ ] **Step 4: 验证死条目检测**

临时加一条已知有调用点的，例如 `"internal/tools.WithProfile": "假条目"`（`orchestrator.go` 在调它）：

```bash
go test ./internal/archtest -run TestGOV6ContextInjectorsHaveCallSites -v
```

预期：**FAIL**，含 `stale ctxInjectExceptions entr(ies)`。验证后删掉假条目。

- [ ] **Step 5: 跑全量**

```bash
go test ./internal/archtest
```

预期：PASS

- [ ] **Step 6: Commit**

```bash
git add internal/archtest/ctxinject_test.go
git commit -m "test(archtest): add GOV6 context injection closure gate

Exported context injectors (func With<X>(ctx, ...) context.Context) must
have a production call site. Without one the value is never bound and every
consumer downstream silently reads the zero value — the exact shape of the
registry.WithRole defect, where the entire role machinery existed, was
tested, and ran against an empty role forever.

Call-site matching resolves package aliases to import paths rather than
matching bare function names, so a same-named function in another package
cannot mask a real violation.

Refs: spec 2026-08-03 §4.2 GOV6, audit P0-5"
```

---

## Task 4: 暴露注册工具名与默认 profile

**背景：** GOV5 需要两样东西，目前都拿不到：

1. **实际注册的工具名集合** —— `allTools` 是 `Build` 内的局部变量（`bootstrap.go:588-761` 命令式 `append`），构造完直接进 `orchConfig.Tools`（`:776`），`App` 上无任何访问器
2. **默认 profile 的 allow 列表** —— 是 `Build` 内的结构体字面量（`bootstrap.go:626-643`），且会被 `cfg.Profiles["orchestrator"]` 覆盖，测试无法单独取到出厂默认值

**这是 W0 唯一新增的生产代码 API。** 两处改动都极小，且各自独立有用（`ToolNames` 也能给 `doctor` 用）。

**Files:**
- Modify: `internal/bootstrap/bootstrap.go`（`App` 结构体、`Build` 中 profile 构造处、`allTools` 收尾处 `:761`、`App` 字面量 `:983`）

- [ ] **Step 1: 写测试（此时不编译）**

创建 `internal/bootstrap/wiring_test.go`（本任务只放前两个测试，GOV5 主体在 Task 5）：

```go
package bootstrap_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/bootstrap"
)

// TestDefaultOrchestratorProfileIsStable proves the factory-default profile
// is reachable independently of config, which is what lets GOV5 compare the
// shipped allow list against the shipped tool registry.
func TestDefaultOrchestratorProfileIsStable(t *testing.T) {
	p := bootstrap.DefaultOrchestratorProfile()
	require.NotEmpty(t, p.Tools.Allow, "default profile must name concrete tools, not fail open")
	require.True(t, p.Net.Allow, "default profile allows net (see bootstrap.go comment)")
}

// TestAppExposesToolNames proves a built App reports the tool names actually
// registered with the orchestrator.
func TestAppExposesToolNames(t *testing.T) {
	app := buildMinimalApp(t) // helper from bootstrap_test.go:40
	require.NotEmpty(t, app.ToolNames, "App.ToolNames must list the registered tools")
	require.Contains(t, app.ToolNames, "fs_read", "fs_read is always registered")
}
```

> ⚠️ **helper 名字有陷阱**：用 `buildMinimalApp`（`internal/bootstrap/bootstrap_test.go:40`，属 `package bootstrap_test`）。同目录还有一个 `buildFakeApp`（`bootstrap_mcp_test.go:12`），但它属于**内部包** `package bootstrap`——`wiring_test.go` 声明的是 `package bootstrap_test`，选错直接不编译。

- [ ] **Step 2: 运行确认不编译**

```bash
go test ./internal/bootstrap -run 'TestDefaultOrchestratorProfileIsStable|TestAppExposesToolNames' -v
```

预期：**FAIL**，`undefined: bootstrap.DefaultOrchestratorProfile` 与 `app.ToolNames undefined`

- [ ] **Step 3: 提取默认 profile 为包级函数**

在 `internal/bootstrap/bootstrap.go` 中 `Build` 之外新增（放在 `Build` 定义之前）：

```go
// DefaultOrchestratorProfile returns the factory-default permission profile
// for the orchestrator. The orchestrator no longer falls back to
// Tools={"*"}: when the operator did not configure profiles.orchestrator, we
// ship this concrete "coding" profile naming the tools the orchestrator
// actually uses, so a forgotten profile block stays least-privilege rather
// than fail-open. Operators who need shell/net widening must declare it in
// config.yaml.
//
// Exported so GOV5 (internal/bootstrap/wiring_test.go) can compare the
// shipped allow list against the shipped tool registry without having to
// reach into Build.
func DefaultOrchestratorProfile() guard.PermissionProfile {
	return guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{
			// NB: "fs_patch" and "fs_mkdir" used to be listed here and were
			// dropped — neither has ever been a registered tool. The patch
			// tool's real name is "apply_patch" (internal/tools/fs.go:99),
			// which is already allowed below; there is no mkdir tool at all.
			"fs_read", "fs_list", "fs_search", "fs_glob", "fs_write", "fs_edit",
			"shell_run", "shell_start", "shell_read", "shell_write_stdin", "shell_wait", "shell_cancel",
			"task_shell_start", "task_shell_wait", "task_shell_stdin", "task_shell_cancel",
			"memory_search", "memory_recall", "memory_write",
			"web_fetch", "web_search", "time_now", "skill_use", "vcs_*",
			"agent_start", "workflow_start", "analysis", "summarize",
			"apply_patch",
			// B3 developer tools
			"git_status", "git_diff", "run_tests", "diagnostics",
			"github_pr_context", "github_comment", "github_approve", "github_merge",
			"review",
		}},
		Net: guard.NetPerm{Allow: true},
	}
}
```

然后把 `Build` 里 `bootstrap.go:626-643` 的字面量替换为：

```go
	profile := DefaultOrchestratorProfile()
	if cfg.Profiles != nil {
		if p, ok := cfg.Profiles["orchestrator"]; ok {
			profile = p
		}
	}
```

（原字面量上方那段解释性注释随字面量一起移进 `DefaultOrchestratorProfile` 的 doc，不要两处重复。）

**⚠️ 提取时顺手删掉两个幽灵名 `fs_patch` 与 `fs_mkdir`。** 二者从未是注册工具：patch 工具的真名是 `apply_patch`（`internal/tools/fs.go:99`，且已在 allow 列表里单独出现），`fs_mkdir` 则全仓不存在（`FSTools.Tools()` 只注册 read/write/edit/list/glob/search/patch，见 `fs.go:113-121`）。

**这不是行为变更**——allow 一个不存在的工具本就不产生任何效果。但它是「授权了不存在的工具」这类谎报的另外两例，与 GOV5 要抓的是同一种问题。这两个和九个 shell 工具的区别在于**修法相反**：shell 工具是「该注册没注册」（W1 去注册），这两个是「名字压根是错的」（现在就删）。删掉后 GOV5 的幽灵集合正好剩九个 shell 工具，豁免表也只需九条。

- [ ] **Step 3b: 验证删除没有连带影响**

```bash
rg -n '"fs_patch"' --glob '!*_test.go' --glob '!reference/**' --glob '!docs/**' .
rg -n 'fs_mkdir'  --glob '!*_test.go' --glob '!reference/**' --glob '!docs/**' .
```

**预期（两条命令的期望不同，别混淆）：**

| 命令 | 预期 | 含义 |
|---|---|---|
| `"fs_patch"` | **零命中** | 删掉 profile 那处后就没有别的引用了 |
| `fs_mkdir` | **8 处命中，全部保留不动** | 见下表 |

`fs_mkdir` 残留的 8 处**与工具注册表完全无关**，不阻断本次删除：

| 位置 | 是什么 |
|---|---|
| `internal/guard/mode.go:123` | `editTools` map —— `ModeAllowEdits` 的自动批准集 |
| `internal/cli/tui/styles.go:406` | TUI 工具显示名映射 |
| `internal/cli/tui/entries.go:845` | TUI silent-tool 分支 |
| `internal/cli/tui/frecency.go:217` | frecency 的路径提取分支 |
| `entries.go:405,410`、`frecency.go:212`、`commands.go:383` | 注释（4 处） |

删除 profile 条目后跑 `go test ./internal/guard ./internal/cli/tui` 应全绿——这几张表是独立数据结构，不读 profile 的 allow 列表。

> 🔍 **顺带发现（不属 W0 范围，但要记下来）**：`fs_mkdir` 出现在 guard 的自动批准集和 TUI 的三张表里——**整个仓库都以为存在一个 `fs_mkdir` 工具，而它从来没有**。这与 GOV5 抓的是同一类幽灵，只是发生在**消费侧**而非授权侧。W0 只负责让它可见；清理归 W1（装配线），见本计划末尾「不在 W0 范围内的事」。

- [ ] **Step 4: 给 App 加 ToolNames 字段**

在 `App` 结构体（`bootstrap.go:69` 起）中加：

```go
	// ToolNames lists the names of every tool registered with the
	// orchestrator, in registration order. Populated by Build from the
	// tools' own Info(). Exposed so GOV5 can assert the permission
	// profile's allow list and the registry agree — the audit found the
	// profile allowing nine shell tools that were never registered.
	ToolNames []string
```

- [ ] **Step 5: 在 Build 中填充**

在 `allTools` 最后一次 `append` 之后（`bootstrap.go:761` 的 `NewScreenshotTool` 那行之后）插入：

```go
	// GOV5 seam: snapshot the registered tool names while allTools is still
	// in scope. Info() is pure metadata on every tool implementation, so the
	// background context here can never block.
	toolNames := make([]string, 0, len(allTools))
	for _, tl := range allTools {
		info, err := tl.Info(context.Background())
		if err != nil {
			return nil, fmt.Errorf("tool registry: Info failed: %w", err)
		}
		toolNames = append(toolNames, info.Name)
	}
```

然后在 `App` 字面量（`bootstrap.go:983` 的 `return &App{`）中加一行：

```go
		ToolNames: toolNames,
```

- [ ] **Step 6: 跑测试确认通过**

```bash
go test ./internal/bootstrap -run 'TestDefaultOrchestratorProfileIsStable|TestAppExposesToolNames' -v
```

预期：**PASS**

- [ ] **Step 7: 跑包全量与治理测试**

```bash
go test ./internal/bootstrap ./internal/archtest
```

预期：PASS。特别注意 GOV3（导出符号必须有 doc）——新增的 `DefaultOrchestratorProfile` 与 `App.ToolNames` 都已带 doc 注释。

- [ ] **Step 8: Commit**

```bash
git add internal/bootstrap/bootstrap.go internal/bootstrap/wiring_test.go
git commit -m "feat(bootstrap): expose registered tool names and default profile

Adds App.ToolNames (populated from each tool's own Info()) and extracts the
factory-default orchestrator profile into DefaultOrchestratorProfile().

Both are seams for GOV5, which asserts the permission profile's allow list
and the tool registry agree. Neither was reachable before: allTools was a
local in Build and the default profile was an inline literal that config
could overwrite.

Refs: spec 2026-08-03 §4.2 GOV5"
```

---

## Task 5: GOV5 工具注册一致性断言

**背景（实测）：** `bootstrap.go:629-630` 的默认 profile allow 列表里有 9 个工具名从未注册进 `allTools`：

```
shell_start  shell_read  shell_write_stdin  shell_wait  shell_cancel
task_shell_start  task_shell_wait  task_shell_stdin  task_shell_cancel
```

`tools.NewShellV2Tools`（`internal/tools/shell_v2.go:49`）全仓非测试零调用。**任何读 profile 的人都会以为这九个工具可用**——这比单纯的「没接线」更具误导性。这条问题审计本身没发现，是 2026-08-03 复核新查出的。

**为什么是运行时测试而非静态分析：** `allTools` 是命令式 `append`，工具名藏在 `tools.NewTestRunTool()` 这类构造函数内部，AST 拿不到。改为 `--fake-model` 跑一次完整 `Build` 后比对。**附带收益**：这条测试直接推进审计项 `E1 COV3`（bootstrap 集成测试覆盖率 23% → 50%+），一份代码销两个账。

**双向比对的不对称性：**
- **allow 有、注册表无** → 硬失败（授权了不存在的工具，是安全语义上的谎报）
- **注册表有、profile 未 allow** → 只 `t.Logf` 列出（收紧型 profile 是完全合法的配置，不该失败）

**Files:**
- Modify: `internal/bootstrap/wiring_test.go`（Task 4 已创建）

- [ ] **Step 1: 追加 GOV5 主体测试**

在 `internal/bootstrap/wiring_test.go` 追加：

```go
// toolWiringExceptions maps a tool name present in the default profile's
// allow list but absent from the tool registry, to the work package that
// will register it.
//
// Entries may only be REMOVED, never added. A dead entry — the tool is now
// registered — fails the test.
var toolWiringExceptions = map[string]string{
	"shell_start":       "W1 装配线：NewShellV2Tools 未注册（审计 P1-7）",
	"shell_read":        "W1 装配线：NewShellV2Tools 未注册（审计 P1-7）",
	"shell_write_stdin": "W1 装配线：NewShellV2Tools 未注册（审计 P1-7）",
	"shell_wait":        "W1 装配线：NewShellV2Tools 未注册（审计 P1-7）",
	"shell_cancel":      "W1 装配线：NewShellV2Tools 未注册（审计 P1-7）",
	"task_shell_start":  "W1 装配线：NewShellV2Tools 未注册（审计 P1-7）",
	"task_shell_wait":   "W1 装配线：NewShellV2Tools 未注册（审计 P1-7）",
	"task_shell_stdin":  "W1 装配线：NewShellV2Tools 未注册（审计 P1-7）",
	"task_shell_cancel": "W1 装配线：NewShellV2Tools 未注册（审计 P1-7）",
}

// TestGOV5ProfileAllowMatchesToolRegistry verifies the default orchestrator
// profile does not authorize tools that were never registered.
//
// A name in the allow list that has no registered tool is worse than a
// missing feature: anyone reading the profile concludes the capability
// exists. The audit missed this entirely; the 2026-08-03 re-verification
// found nine such names.
//
// The phantom set depends on the config buildMinimalApp uses — several tools
// register conditionally (that config yields 59). A different config can
// shift the set, so treat the exemption table as tied to this harness.
func TestGOV5ProfileAllowMatchesToolRegistry(t *testing.T) {
	app := buildMinimalApp(t)

	registered := make(map[string]bool, len(app.ToolNames))
	for _, n := range app.ToolNames {
		registered[n] = true
	}
	require.NotEmpty(t, registered, "tool registry must not be empty")

	allowed := bootstrap.DefaultOrchestratorProfile().Tools.Allow

	var phantom []string
	concrete := make(map[string]bool)
	for _, name := range allowed {
		if strings.ContainsAny(name, "*?[") {
			continue // wildcard entries cannot be checked by exact name
		}
		concrete[name] = true
		if registered[name] {
			continue
		}
		if _, exempt := toolWiringExceptions[name]; exempt {
			continue
		}
		phantom = append(phantom, name)
	}
	sort.Strings(phantom)
	if len(phantom) > 0 {
		t.Errorf("GOV5: default profile allows %d tool(s) that are NOT registered — "+
			"the profile advertises capabilities that do not exist:\n  %s\n\n"+
			"Fix: register the tools in bootstrap.Build, or remove them from\n"+
			"DefaultOrchestratorProfile. If registration is deferred, add entries to\n"+
			"toolWiringExceptions naming the work package.",
			len(phantom), strings.Join(phantom, "\n  "))
	}

	// Dead-entry check: an exempted name that is now registered has been
	// wired up, so its exemption must be deleted.
	var dead []string
	for name := range toolWiringExceptions {
		if registered[name] {
			dead = append(dead, name)
		}
	}
	sort.Strings(dead)
	if len(dead) > 0 {
		t.Errorf("GOV5: %d stale toolWiringExceptions entr(ies) — these tools are now "+
			"registered and their exemptions must be DELETED:\n  %s",
			len(dead), strings.Join(dead, "\n  "))
	}

	// Advisory only: a registered tool the default profile does not allow is
	// legitimate (a tightened profile is a valid configuration), but listing
	// them makes a forgotten authorization easy to spot in CI logs.
	//
	// Wildcards are re-applied here (they were skipped in the phantom check
	// above) so vcs_commit and friends are not reported as unauthorized when
	// "vcs_*" already covers them.
	var unauthorized []string
	for _, n := range app.ToolNames {
		if concrete[n] {
			continue
		}
		covered := false
		for _, pat := range allowed {
			if ok, _ := path.Match(pat, n); ok {
				covered = true
				break
			}
		}
		if !covered {
			unauthorized = append(unauthorized, n)
		}
	}
	sort.Strings(unauthorized)
	if len(unauthorized) > 0 {
		t.Logf("GOV5 (advisory): %d registered tool(s) are not named in the default "+
			"profile's allow list — verify this is intentional tightening and not a "+
			"forgotten authorization:\n  %s",
			len(unauthorized), strings.Join(unauthorized, "\n  "))
	}
}
```

补充 import：`path`、`sort`、`strings`。

> `path`（不是 `path/filepath`）—— 工具名是斜杠无关的标识符，`path.Match` 的 glob 语义与 guard 的一致。

> ⚠️ 通配项跳过说明：allow 列表里有 `vcs_*`。精确名比对无法处理通配，直接跳过是正确的——通配项本就意味着「这一族全放行」，不构成「授权了不存在的工具」的谎报。

- [ ] **Step 2: 用空豁免表验证抓到 9 个**

临时把 `toolWiringExceptions` 改成 `map[string]string{}`，运行：

```bash
go test ./internal/bootstrap -run TestGOV5ProfileAllowMatchesToolRegistry -v
```

预期：**FAIL**，且列表**恰好 9 条**：

```
GOV5: default profile allows 9 tool(s) that are NOT registered
  shell_cancel
  shell_read
  shell_start
  shell_wait
  shell_write_stdin
  task_shell_cancel
  task_shell_start
  task_shell_stdin
  task_shell_wait
```

> ⚠️ **若出现 11 条而非 9 条**（多出 `fs_mkdir` 与 `fs_patch`），说明 Task 4 Step 3 漏了删这两个幽灵名——回去补删，不要给它们加豁免条目。
>
> ⚠️ 若出现其他名字，**停下来核对**：说明还有别的幽灵工具，记录下来并判定是「该注册没注册」（加豁免、归 W1）还是「名字压根是错的」（直接从 profile 删）。少于 9 条则说明 `App.ToolNames` 填充有误。
>
> ℹ️ GOV5 的结果取决于 `buildMinimalApp` 的最小配置——部分工具是条件注册的（实测该配置下注册 59 个）。换配置可能改变幽灵集合，这一点已写进测试的 doc 注释。

- [ ] **Step 3: 恢复豁免表，确认转绿**

```bash
go test ./internal/bootstrap -run TestGOV5ProfileAllowMatchesToolRegistry -v
```

预期：**PASS**，并在 `-v` 输出里能看到 advisory 那段列出未授权但已注册的工具（信息性，不影响结果）。

- [ ] **Step 4: 验证死条目检测**

临时加一条 `"fs_read": "假条目"`（`fs_read` 必然已注册）：

```bash
go test ./internal/bootstrap -run TestGOV5ProfileAllowMatchesToolRegistry -v
```

预期：**FAIL**，含 `stale toolWiringExceptions entr(ies)`。验证后删掉。

- [ ] **Step 5: 跑包全量**

```bash
go test ./internal/bootstrap
```

预期：PASS

- [ ] **Step 6: Commit**

```bash
git add internal/bootstrap/wiring_test.go
git commit -m "test(bootstrap): add GOV5 profile/registry consistency gate

The default orchestrator profile authorizes nine shell tools that were
never registered (shell_start..task_shell_cancel). A name in the allow list
with no registered tool is worse than a missing feature: anyone reading the
profile concludes the capability exists.

Runtime rather than static: allTools is built imperatively and the names
live inside constructors, so AST analysis cannot reach them. Building a
fake-model App also advances COV3 (bootstrap integration coverage).

Reverse direction is advisory only — a tightened profile is valid config.

Refs: spec 2026-08-03 §4.2 GOV5, audit P1-7"
```

---

## Task 6: 状态台账 `docs/feature-status.yaml`

**背景：** 「32% → 100%」必须是机器算出来的。台账是 63 项的**单一真相源**，`docs/feature-status-audit.md` 降级为历史快照。

**分工（重要）：** 台账里只有 `id` → `package` 的映射需要人的判断（来自 spec §4.3），`verdict` 与 `acceptance` 都能从审计文档机械提取。**不要手工转录 63 条验收标准**——转录必然出错，用脚本抽。

**Files:**
- Create: `docs/feature-status.yaml`

- [ ] **Step 1: 确认审计文档里的条目与判定可机械提取**

```bash
awk '/^## 3\. 未实现/,/^## 4\./' docs/feature-status-audit.md | rg -c '^#### '
awk '/^## 4\. 实现有差别/,/^## 5\./' docs/feature-status-audit.md | rg -c '^#### '
awk '/^## 5\. 部分实现/,/^## 6\./' docs/feature-status-audit.md | rg -c '^#### '
```

预期输出：`12`、`2`、`50`（合计 64；移出 `A1 S08` 后为台账的 63 项）

- [ ] **Step 2: 写生成脚本**

创建临时脚本 `/tmp/genledger.sh`（**不入库**，一次性用完即弃）：

```bash
#!/usr/bin/env bash
# 从审计文档提取 id / verdict / acceptance，与手工维护的 id→package 映射合并。
set -euo pipefail
AUDIT=docs/feature-status-audit.md

extract() {  # $1=章节起始正则 $2=章节结束正则 $3=verdict
  awk "/$1/,/$2/" "$AUDIT" | awk -v v="$3" '
    /^#### / {
      line=$0
      gsub(/^#### `/,"",line); gsub(/` /,"/",line)
      split(line, a, " — ")
      id=a[1]; title=a[2]
      next
    }
    /^\- \*\*验收标准\*\*/ && id != "" {
      acc=$0
      sub(/^\- \*\*验收标准\*\*：/,"",acc)
      gsub(/"/,"'"'"'",acc)
      printf "%s\t%s\t%s\t%s\n", id, v, title, acc
      id=""
    }'
}

{
  extract '^## 3\. 未实现' '^## 4\.' missing
  extract '^## 4\. 实现有差别' '^## 5\.' divergent
  extract '^## 5\. 部分实现' '^## 6\.' partial
} > /tmp/ledger.tsv

wc -l /tmp/ledger.tsv   # 应为 64
```

运行：

```bash
bash /tmp/genledger.sh
```

预期：`/tmp/ledger.tsv` 有 **64** 行。若少于 64，说明某些条目的验收标准行格式不同——用 `rg -n '^#### ' docs/feature-status-audit.md | wc -l` 对照后手工补齐缺的那几条。

- [ ] **Step 3: 写 id → package 映射**

这是**唯一需要人判断的部分**，直接来自 spec §4.3。创建 `/tmp/pkgmap.tsv`：

```
C1/AU1	W1
C1/M07	W1
C1/RLM1	W1
A1/T07/T08	W1
B1/M05	W1
B1/M04b	W1
G/VISION	W1
G/VISION-TOOL	W1
A2/G05	W1
M1/G02	W2
B0/TD1	W2
F2/LEAK3	W2
M1/G03	W2
F2/LEAK2	W3
F1/WAL1	W3
A2/DT1	W3
B1/M04	W3
A2/DT2	W3
F2/CCL1	W4
E2/PROP1	W4
A1/S06	W5
A1/S07	W5
A1/S09	W5
D3/S10	W5
M1/SPEC-TOOLIF	W6
B3/W07	W6
B3/DT4	W6
B3/DT5	W6
B3/GH1	W6
B3/T11	W6
B3/V13	W6
B2/LSP1	W6
A3/C13	W6
A3/MCP1	W6
A3/V16	W6
C4/COST1	W7
C4/OBS1	W7
C4/OBS2	W7
C4/OBS3	W7
M1/O07	W7
C4/O07	W7
F2/BENCH1	W7
C2/UX1	W8
C2/UX2	W8
C2/UX3	W8
C2/UX4	W8
C2/UX8	W8
D3/C15	W8
D3/I18N1	W8
C3/E03	W8
D1/APS1	W9
D1/V12	W9
D1/V14	W9
D2/V15	W9
H2/APIREF1	W9
H1/PKG1	W10
H1/VER1	W10
E1/COV2	W10
E1/COV3	W10
H2/CONTRIB1	W10
H2/EX1	W10
H2/UDOC1	W10
D2/O12	-
```

**63 行。** `A1/S08` 刻意不在其中——它已移出至 S1（spec §4.1）。

校验：

```bash
wc -l /tmp/pkgmap.tsv                      # 63
cut -f2 /tmp/pkgmap.tsv | sort | uniq -c   # W1=9 W2=4 W3=5 W4=2 W5=4 W6=11 W7=7 W8=8 W9=5 W10=7 -=1
```

- [ ] **Step 4: 合并生成台账并核对 S08 被排除**

```bash
join -t$'\t' -1 1 -2 1 <(sort -k1,1 /tmp/ledger.tsv) <(sort -k1,1 /tmp/pkgmap.tsv) | wc -l
```

预期：**63**。若是 62，说明某条 id 的写法在两张表里不一致（最可能是 `A1/T07/T08`——审计里标题是 `T07/T08`，含斜杠）；对齐后重跑。

确认被排除的正是 S08：

```bash
comm -23 <(cut -f1 /tmp/ledger.tsv | sort) <(cut -f1 /tmp/pkgmap.tsv | sort)
```

预期：只输出 `A1/S08`

- [ ] **Step 5: 生成 `docs/feature-status.yaml`**

```bash
{
  echo "# yanshi 功能状态台账 — S0 的单一真相源。"
  echo "#"
  echo "# 本文件取代 docs/feature-status-audit.md 作为当前状态依据；审计文档降级为"
  echo "# 2026-07-31 的历史快照。63 项 = 审计 64 项 - A1/S08（已移出至 S1，见"
  echo "# docs/superpowers/specs/2026-08-03-yanshi-roadmap-design.md §4.1）。"
  echo "#"
  echo "# verdict: partial | missing | divergent | done | removed"
  echo "#   done / removed 是两档终态，都计入「S0 完成」。"
  echo "# evidence: 终态条目必填。两种合法形态（校验规则见 internal/archtest/status_test.go）："
  echo "#   - 文件引用: internal/foo/bar.go:123   （只校验文件存在，不校验行号）"
  echo "#   - 测试引用: internal/foo::TestName    （包路径，非文件路径）"
  echo ""
  join -t$'\t' -1 1 -2 1 <(sort -k1,1 /tmp/ledger.tsv) <(sort -k1,1 /tmp/pkgmap.tsv) \
  | awk -F'\t' '{
      printf "- id: \"%s\"\n", $1
      printf "  package: \"%s\"\n", $5
      printf "  verdict: %s\n", $2
      printf "  title: \"%s\"\n", $3
      printf "  acceptance: \"%s\"\n", $4
      printf "  evidence: \"\"\n\n"
    }'
} > docs/feature-status.yaml
```

- [ ] **Step 6: 校验生成结果**

```bash
rg -c '^- id:' docs/feature-status.yaml                  # 63
rg -o 'verdict: \w+' docs/feature-status.yaml | sort | uniq -c   # partial 50, missing 11, divergent 2
rg -c 'A1/S08' docs/feature-status.yaml                  # 0
go run ./cmd/gendocs -h >/dev/null 2>&1 || true          # 确认没破坏任何生成器
```

预期计数：`partial` 50、`missing` 11、`divergent` 2，合计 63。

> `missing` 是 11 而非审计的 12，差的正是移出的 `A1/S08`。

- [ ] **Step 7: 人工抽查 3 条**

打开 `docs/feature-status.yaml`，随机挑 3 条与审计文档对照，确认 `title` 与 `acceptance` 没有被 awk 截断或错位。特别检查含全角冒号、引号、斜杠的条目（`A1/T07/T08`、`M1/SPEC-TOOLIF`）。

- [ ] **Step 8: Commit**

```bash
git add docs/feature-status.yaml
git commit -m "docs: add machine-readable feature status ledger

63 entries (audit's 64 minus A1/S08, which moved to S1). This file
supersedes docs/feature-status-audit.md as the source of truth for current
state; the audit becomes a 2026-07-31 historical snapshot.

Verdicts and acceptance criteria were extracted mechanically from the audit
rather than transcribed by hand. Only the id -> work-package mapping is
hand-authored, from spec §4.3.

Refs: spec 2026-08-03 §5.1"
```

---

## Task 7: 台账防虚报断言

**背景：** 台账只有配上断言才有意义。没有断言，任何人都能把 `verdict` 改成 `done` 让数字好看。

**与 spec §5.3 的一处有意偏离：** spec 写「测试引用用 `go test -list` 校验」。改为**扫描该包的 `*_test.go` 做 AST 匹配**——语义等价（都是「这个测试存在」），但不需要编译整个包，治理测试跑得快得多。这是实现层的优化，不改变契约。

**Files:**
- Create: `internal/archtest/status_test.go`
- Modify: `internal/archtest/helpers_test.go:1-8`（包 doc 的 stdlib 表述限定作用域）

- [ ] **Step 1: 写断言测试**

创建 `internal/archtest/status_test.go`：

```go
// Package archtest — feature status ledger integrity.
//
// docs/feature-status.yaml is S0's single source of truth for "how much of
// the planned surface actually works". A ledger nobody checks is a ledger
// anybody can edit to look good, so these assertions make a terminal verdict
// cost real evidence.
package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// ledgerSize is the fixed number of entries in the S0 ledger: the audit's 64
// items minus A1/S08, which moved to the S1 sub-project (spec §4.1). A
// changed count means someone added or dropped scope without updating the
// spec.
const ledgerSize = 63

type ledgerEntry struct {
	ID         string `yaml:"id"`
	Package    string `yaml:"package"`
	Verdict    string `yaml:"verdict"`
	Title      string `yaml:"title"`
	Acceptance string `yaml:"acceptance"`
	Evidence   string `yaml:"evidence"`
}

var (
	validVerdicts = map[string]bool{
		"partial": true, "missing": true, "divergent": true,
		"done": true, "removed": true,
	}
	terminalVerdicts = map[string]bool{"done": true, "removed": true}
	validPackages    = map[string]bool{
		"W1": true, "W2": true, "W3": true, "W4": true, "W5": true,
		"W6": true, "W7": true, "W8": true, "W9": true, "W10": true,
		"-": true, // O12, closed by removal
	}
)

func loadLedger(t *testing.T) []ledgerEntry {
	t.Helper()
	path := filepath.Join(moduleRoot(t), "docs", "feature-status.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ledger unreadable: %v", err)
	}
	var entries []ledgerEntry
	if err := yaml.Unmarshal(data, &entries); err != nil {
		t.Fatalf("ledger is not valid YAML: %v", err)
	}
	return entries
}

// testExistsInPkg reports whether pkgDir contains a *_test.go declaring a
// top-level func named testName.
//
// This replaces `go test -list` (spec §5.3): same question, no compile.
func testExistsInPkg(t *testing.T, pkgDir, testName string) bool {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(pkgDir, "*_test.go"))
	if err != nil || len(matches) == 0 {
		return false
	}
	fset := token.NewFileSet()
	for _, path := range matches {
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			continue
		}
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if ok && fd.Recv == nil && fd.Name.Name == testName {
				return true
			}
		}
	}
	return false
}

// checkEvidence validates one evidence string and returns "" when valid or a
// human-readable reason when not.
//
// Two legal forms:
//
//	internal/foo/bar.go:123   file reference — path must exist; the line
//	                          number is NOT checked (it drifts with any
//	                          unrelated edit, and a gate that reddens for no
//	                          reason gets nolint'd away)
//	internal/foo::TestName    test reference — PACKAGE path, not file path
func checkEvidence(t *testing.T, root, ev string) string {
	t.Helper()
	if strings.Contains(ev, "::") {
		parts := strings.SplitN(ev, "::", 2)
		pkgDir := filepath.Join(root, filepath.FromSlash(parts[0]))
		if st, err := os.Stat(pkgDir); err != nil || !st.IsDir() {
			return "package dir not found: " + parts[0] +
				" (test references use a PACKAGE path, not a file path)"
		}
		if !testExistsInPkg(t, pkgDir, parts[1]) {
			return "no test named " + parts[1] + " in package " + parts[0]
		}
		return ""
	}
	idx := strings.LastIndex(ev, ":")
	if idx <= 0 {
		return "malformed: expected \"path/to/file.go:LINE\" or \"pkg/path::TestName\""
	}
	rel := ev[:idx]
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
		return "file not found: " + rel
	}
	return ""
}

// TestFeatureStatusLedgerIntegrity guards the ledger against the one failure
// mode that would make every other governance assertion pointless: flipping
// a verdict to a terminal state without doing the work.
func TestFeatureStatusLedgerIntegrity(t *testing.T) {
	root := moduleRoot(t)
	entries := loadLedger(t)

	if len(entries) != ledgerSize {
		t.Fatalf("ledger has %d entries, expected %d — scope changed without a spec "+
			"update (see spec §4.1)", len(entries), ledgerSize)
	}

	seen := make(map[string]bool, len(entries))
	var problems []string
	for _, e := range entries {
		if e.ID == "" {
			problems = append(problems, "an entry has an empty id")
			continue
		}
		if seen[e.ID] {
			problems = append(problems, e.ID+": duplicate id")
		}
		seen[e.ID] = true

		if !validVerdicts[e.Verdict] {
			problems = append(problems, e.ID+": invalid verdict "+e.Verdict)
		}
		if !validPackages[e.Package] {
			problems = append(problems, e.ID+": invalid package "+e.Package)
		}
		if e.Acceptance == "" {
			problems = append(problems, e.ID+": empty acceptance criteria")
		}

		if !terminalVerdicts[e.Verdict] {
			continue
		}
		if e.Evidence == "" {
			problems = append(problems, e.ID+": verdict "+e.Verdict+
				" requires non-empty evidence")
			continue
		}
		if reason := checkEvidence(t, root, e.Evidence); reason != "" {
			problems = append(problems, e.ID+": bad evidence — "+reason)
		}
		if e.Verdict == "removed" && !strings.Contains(e.Evidence, "::") {
			problems = append(problems, e.ID+": verdict removed requires a TEST "+
				"reference (pkg::TestName) — it must assert the thing is gone, and a "+
				"file reference cannot do that")
		}
	}

	sort.Strings(problems)
	if len(problems) > 0 {
		t.Errorf("docs/feature-status.yaml has %d problem(s):\n  %s",
			len(problems), strings.Join(problems, "\n  "))
	}
}

// TestFeatureStatusLedgerProgress prints the current tally. It never fails —
// it exists so CI logs carry the number without anyone running a tool.
func TestFeatureStatusLedgerProgress(t *testing.T) {
	entries := loadLedger(t)
	counts := make(map[string]int)
	for _, e := range entries {
		counts[e.Verdict]++
	}
	done := counts["done"] + counts["removed"]
	t.Logf("S0 progress: %d/%d terminal (done=%d removed=%d) | "+
		"remaining: partial=%d missing=%d divergent=%d",
		done, len(entries), counts["done"], counts["removed"],
		counts["partial"], counts["missing"], counts["divergent"])
}
```

- [ ] **Step 2: 运行，确认台账当前通过**

```bash
go test ./internal/archtest -run 'TestFeatureStatusLedger' -v
```

预期：**PASS**，且进度日志为

```
S0 progress: 0/63 terminal (done=0 removed=0) | remaining: partial=50 missing=11 divergent=2
```

- [ ] **Step 3: 验证防虚报确实生效**

临时把 `docs/feature-status.yaml` 里任意一条改成 `verdict: done`（`evidence` 留空），运行：

```bash
go test ./internal/archtest -run TestFeatureStatusLedgerIntegrity -v
```

预期：**FAIL**，含 `verdict done requires non-empty evidence`。

再把 `evidence` 填成一个不存在的路径 `internal/nope/nope.go:1`：

预期：**FAIL**，含 `file not found: internal/nope/nope.go`。

再填成 `internal/archtest/helpers_test.go::TestNope`（**故意用文件路径而非包路径**）：

预期：**FAIL**，含 `package dir not found`——这验证了 spec 里踩过的那个坑（示例误写文件路径）真的会被拦下。

验证完把这条改回原状。

- [ ] **Step 4: 更新 archtest 包 doc 注释**

`internal/archtest/helpers_test.go:1-8` 的包注释现在写着「rely **exclusively** on the standard library」，而 `status_test.go` 用了 `gopkg.in/yaml.v3`——这句会变成假陈述，而 GOV3 正是管 doc 注释的治理规则。改为：

```go
// Package archtest provides zero-dependency test helpers for architecture
// governance tests (GOV1/GOV2/GOV3/GOV4/GOV6).
//
// The helpers in THIS FILE rely exclusively on the standard library
// (go/parser, go/token, go/ast, encoding/json, os/exec) to avoid import-cycle
// risk with the internal packages they are designed to test.
//
// One test in the package steps outside that rule: status_test.go reads
// docs/feature-status.yaml with gopkg.in/yaml.v3. That dependency is already
// in go.mod (internal/config uses it) and it parses a docs file rather than
// any internal package, so it carries no cycle risk.
package archtest
```

- [ ] **Step 5: 跑全量治理测试**

```bash
go test ./internal/archtest
```

预期：PASS（GOV1/2/3/4/6 + 台账断言全绿）

- [ ] **Step 6: Commit**

```bash
git add internal/archtest/status_test.go internal/archtest/helpers_test.go
git commit -m "test(archtest): add feature status ledger integrity gate

A terminal verdict (done/removed) now costs real evidence: a file path that
exists, or a test that exists. Without this, the ledger is a number anyone
can edit upward.

Evidence deliberately does NOT check line numbers — they drift with any
unrelated edit, and a gate that reddens for no reason gets nolint'd away.
Test references are validated by AST scan rather than 'go test -list':
same question, no compile.

Refs: spec 2026-08-03 §5.3"
```

---

## Task 8: `cmd/featurestatus` 统计工具

**背景：** 台账断言只在 CI 日志里打进度。开发者需要一个随时能跑的统计。与 `cmd/codelines`、`cmd/depsanalyze` 同属不参与运行时的 dev 工具。

**YAGNI 边界：** 只做「读台账、打统计表、按包分组」。**不做** README 徽章生成、不做 HTML 报告、不做趋势图——需要时再加。

**Files:**
- Create: `cmd/featurestatus/main.go`

- [ ] **Step 1: 写实现**

创建 `cmd/featurestatus/main.go`：

```go
// Package main implements featurestatus, a reader for
// docs/feature-status.yaml — S0's feature status ledger. It prints the
// overall tally and a per-work-package breakdown, so progress on "how much
// of the planned surface actually works" is a command rather than a
// 4-hour audit. Used for ad-hoc governance checks; not used at runtime.
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

type entry struct {
	ID       string `yaml:"id"`
	Package  string `yaml:"package"`
	Verdict  string `yaml:"verdict"`
	Title    string `yaml:"title"`
	Evidence string `yaml:"evidence"`
}

func isTerminal(v string) bool { return v == "done" || v == "removed" }

// pkgOrder sorts W1..W10 numerically ("-" last) rather than lexically, which
// would put W10 between W1 and W2.
func pkgOrder(p string) int {
	if !strings.HasPrefix(p, "W") {
		return 1 << 30
	}
	n, err := strconv.Atoi(p[1:])
	if err != nil {
		return 1 << 30
	}
	return n
}

func main() {
	path := flag.String("f", "docs/feature-status.yaml", "path to the ledger")
	openOnly := flag.Bool("open", false, "list only entries not yet in a terminal state")
	flag.Parse()

	data, err := os.ReadFile(*path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "featurestatus:", err)
		os.Exit(1)
	}
	var entries []entry
	if err := yaml.Unmarshal(data, &entries); err != nil {
		fmt.Fprintln(os.Stderr, "featurestatus: bad YAML:", err)
		os.Exit(1)
	}

	if *openOnly {
		var pending []entry
		for _, e := range entries {
			if !isTerminal(e.Verdict) {
				pending = append(pending, e)
			}
		}
		sort.Slice(pending, func(i, j int) bool {
			if pkgOrder(pending[i].Package) != pkgOrder(pending[j].Package) {
				return pkgOrder(pending[i].Package) < pkgOrder(pending[j].Package)
			}
			return pending[i].ID < pending[j].ID
		})
		for _, e := range pending {
			fmt.Printf("%-4s %-18s %-10s %s\n", e.Package, e.ID, e.Verdict, e.Title)
		}
		fmt.Printf("\n%d open of %d\n", len(pending), len(entries))
		return
	}

	type stat struct{ total, terminal int }
	byPkg := map[string]*stat{}
	byVerdict := map[string]int{}
	overall := stat{}
	for _, e := range entries {
		if byPkg[e.Package] == nil {
			byPkg[e.Package] = &stat{}
		}
		byPkg[e.Package].total++
		byVerdict[e.Verdict]++
		overall.total++
		if isTerminal(e.Verdict) {
			byPkg[e.Package].terminal++
			overall.terminal++
		}
	}

	pkgs := make([]string, 0, len(byPkg))
	for p := range byPkg {
		pkgs = append(pkgs, p)
	}
	sort.Slice(pkgs, func(i, j int) bool { return pkgOrder(pkgs[i]) < pkgOrder(pkgs[j]) })

	fmt.Println("S0 feature status")
	fmt.Println("=================")
	fmt.Printf("terminal: %d/%d (%.0f%%)\n\n",
		overall.terminal, overall.total,
		100*float64(overall.terminal)/float64(overall.total))

	fmt.Println("by work package:")
	for _, p := range pkgs {
		s := byPkg[p]
		fmt.Printf("  %-4s %2d/%2d\n", p, s.terminal, s.total)
	}

	fmt.Println("\nby verdict:")
	verdicts := make([]string, 0, len(byVerdict))
	for v := range byVerdict {
		verdicts = append(verdicts, v)
	}
	sort.Strings(verdicts)
	for _, v := range verdicts {
		fmt.Printf("  %-10s %2d\n", v, byVerdict[v])
	}
}
```

- [ ] **Step 2: 跑起来**

```bash
go run ./cmd/featurestatus
```

预期输出：

```
S0 feature status
=================
terminal: 0/63 (0%)

by work package:
  W1    0/ 9
  W2    0/ 4
  W3    0/ 5
  W4    0/ 2
  W5    0/ 4
  W6    0/11
  W7    0/ 7
  W8    0/ 8
  W9    0/ 5
  W10   0/ 7
  -     0/ 1

by verdict:
  divergent   2
  missing    11
  partial    50
```

> ⚠️ 这里的每包计数必须与 spec §4.3 的表格完全一致（9/4/5/2/4/11/7/8/5/7/1）。**不一致就是台账映射写错了，回 Task 6 修**。

- [ ] **Step 3: 验证 `-open` 模式**

```bash
go run ./cmd/featurestatus -open | head -12
go run ./cmd/featurestatus -open | tail -1
```

预期：按 W1→W10 顺序列出（**W10 排在 W9 之后而非 W1 之后**，验证 `pkgOrder` 的数值排序生效），末行为 `63 open of 63`

- [ ] **Step 4: 确认治理测试仍绿**

```bash
go test ./internal/archtest
go vet ./cmd/featurestatus
```

预期：均 PASS。注意 GOV3 —— `cmd/` 下的导出符号也要有 doc；本文件除 `main` 外无导出符号，`package main` 已有 doc 注释。

- [ ] **Step 5: 把它登记进 CLAUDE.md 的 dev 工具清单**

`CLAUDE.md` 的「其余 dev 工具（不参与运行时）」一段列了 `cmd/depsanalyze` / `cmd/agent-worker`，「命令」一节列了 `cmd/codelines`。在 dev 工具那段补一句：

```markdown
`cmd/featurestatus` 读 `docs/feature-status.yaml` 打印 S0 功能状态统计（`-open` 只列未结项）。
```

- [ ] **Step 6: Commit**

```bash
git add cmd/featurestatus/main.go CLAUDE.md
git commit -m "feat(cmd): add featurestatus ledger reader

Prints the S0 tally and a per-work-package breakdown from
docs/feature-status.yaml, so 'how much actually works' is a command
rather than a 4-hour, 118-subagent audit. -open lists what is left.

Dev tool only; not wired into the runtime.

Refs: spec 2026-08-03 §5.2"
```

---

## Task 9: O12 移除、收紧 CI 软门禁、台账夺权

三件收尾事，都很小，合并成一个任务。

**① O12 以移除方式结案（spec §3.2 ④）。** 审计判 `D2 O12` 未实现：`runWithRecovery` 从未被 `extension.ts` import，`README.md` 在描述不存在的断线重连能力。既已定 TUI + Web IDE 双主力，第三个门面受众重叠、收益不足。

**② 收紧 CI 软门禁（审计 P2-14）。** `ci.yml` 的 `governance` 与 `fuzz-seed` 两个 job 带 `continue-on-error: true`，注释写着「soft until E3/E2 lands」——E3/E2 资产早已落地全绿。**这一步必须放在最后**：前面八个任务给 `governance` job 新增了 GOV4/GOV5/GOV6 与台账断言，先收紧会把未完成的中间态变成硬失败。

**③ 台账夺权（spec §5.4）。** 给 `docs/feature-status-audit.md` 加头部说明，把它降级为历史快照。

**Files:**
- Delete: `ide/vscode/`、`scripts/check-d2.sh`
- Create: `internal/archtest/removal_test.go`
- Modify: `docs/feature-status.yaml`（`D2/O12` → `removed`）、`.github/workflows/ci.yml`、`docs/feature-status-audit.md`、`sdk/schema/CONTRACT_HANDOFF.md`

- [ ] **Step 1: 先写断言（TDD：此时应通过，因为东西还在——所以要先看它失败的反面）**

创建 `internal/archtest/removal_test.go`：

```go
// Package archtest — removal assertions.
//
// Some audit items close by deleting code rather than finishing it. A ledger
// entry with verdict "removed" points here: the evidence for "we removed it"
// has to be a test that fails if it comes back.
package archtest

import (
	"os"
	"path/filepath"
	"testing"
)

// TestVSCodeExtensionRemoved asserts the VS Code extension and its CI helper
// are gone.
//
// Audit item D2/O12 closed by removal (spec §3.2 ④): the extension was never
// finished (runWithRecovery was never imported by extension.ts, and the
// README advertised reconnect behaviour that did not exist), and with TUI and
// the Web IDE both first-class, a third front end has an overlapping audience
// and does not earn its maintenance cost.
//
// This is the evidence backing that ledger entry — if either path reappears,
// it must come with a decision to reverse §3.2 ④.
func TestVSCodeExtensionRemoved(t *testing.T) {
	root := moduleRoot(t)
	for _, rel := range []string{
		"ide/vscode",
		"scripts/check-d2.sh",
	} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err == nil {
			t.Errorf("%s still exists — audit item D2/O12 closed by removal "+
				"(spec §3.2 ④). If this is a deliberate reversal, update the spec "+
				"and the ledger entry first.", rel)
		}
	}
}
```

- [ ] **Step 2: 运行，确认它现在是失败的**

```bash
go test ./internal/archtest -run TestVSCodeExtensionRemoved -v
```

预期：**FAIL**，两条都在（`ide/vscode still exists`、`scripts/check-d2.sh still exists`）。这证明断言不是空跑。

- [ ] **Step 3: 执行删除**

```bash
git rm -r ide/vscode
git rm scripts/check-d2.sh
rm -rf ide          # ← 必须有这一步，见下方说明
```

> ⚠️ **`rm -rf ide` 不能省。** `ide/vscode/.gitignore:1` 忽略了 `node_modules/`，所以 `git rm -r` 只删掉被跟踪的文件，`ide/vscode/node_modules/` 会原样留在磁盘上——`TestVSCodeExtensionRemoved` 用 `os.Stat` 检查目录存在性，Step 4 会直接 FAIL。
>
> 执行后确认：`ls ide 2>&1` 应输出 `No such file or directory`。

- [ ] **Step 4: 运行，确认转绿**

```bash
go test ./internal/archtest -run TestVSCodeExtensionRemoved -v
```

预期：**PASS**

- [ ] **Step 5: 清理文档中对它的描述**

```bash
rg -n 'ide/vscode|check-d2|VS ?Code' \
   --glob '!reference/**' --glob '!docs/superpowers/**' \
   --glob '!docs/archive/**' --glob '!docs/feature-status-audit.md' .
```

> ⚠️ **`--glob '!docs/archive/**'` 不能省。** 不排除的话会多命中 3 处历史文档（`feature-comparison-with-codex.md:68`、`feature-roadmap-codex-deepseek.md:706`、`feature-roadmap-e-h.md:136`）。按本计划自己的「不伪造历史」原则，归档文档记录的是当时的决策，**一律不改**。

预期命中**两个**文件，都要改：

**① `sdk/schema/CONTRACT_HANDOFF.md`** —— 把描述 VS Code 扩展为交付物的段落改为：

```markdown
> VS Code 扩展（审计 D2/O12）已于 2026-08 移除，见
> docs/superpowers/specs/2026-08-03-yanshi-roadmap-design.md §3.2 ④。
> 本文件其余部分描述的契约不受影响。
```

**② `docs/user-guide/entrypoints.md:213`** —— 该行写着「VS Code 扩展：经 app-server（JSON-RPC）接入」。删掉这一条目。

> ✅ 这一行在 `<!-- END GENERATED: help:doctor -->` **之后**，不在生成块内，手改安全——不会被 `docs.yml` 的 `git diff --exit-code` 打回。改完仍建议跑一次生成器确认：`go run ./cmd/gendocs -help-all docs/user-guide/tui.md docs/user-guide/entrypoints.md && git diff --exit-code docs/user-guide/entrypoints.md`

`docs/superpowers/specs/2026-07-22-h2-docs-examples-contrib-design.md` 是历史 spec，**不改**——历史文档记录当时的决策，改它等于伪造历史。

> ℹ️ **给后来重跑这条 rg 的人**：三个 `--glob '!...'` 排除掉的命中**全部是有意保留的**——`docs/archive/**` 与 `docs/superpowers/**` 是历史文档与决策记录，`docs/feature-status-audit.md` 是 2026-07-31 的快照。看到它们仍提及 VS Code 扩展不代表清理没做干净，不要再走一遍这个循环。

- [ ] **Step 6: 更新台账条目**

把 `docs/feature-status.yaml` 里 `D2/O12` 那条改为：

```yaml
- id: "D2/O12"
  package: "-"
  verdict: removed
  title: "IDE 扩展（VS Code）"
  acceptance: "ide/vscode/ 与 scripts/check-d2.sh 不存在；文档无对其作为交付物的描述"
  evidence: "internal/archtest::TestVSCodeExtensionRemoved"
```

> ⚠️ `evidence` 是**包路径** `internal/archtest`，不是文件路径 `internal/archtest/removal_test.go`。写成文件路径会被 `checkEvidence` 拦下（Task 7 Step 3 专门验证过这个坑）。

- [ ] **Step 7: 验证台账断言接受这条 removed**

```bash
go test ./internal/archtest -run 'TestFeatureStatusLedger' -v
go run ./cmd/featurestatus
```

预期：断言 PASS；统计显示 `terminal: 1/63 (2%)`，`by verdict` 里出现 `removed 1`、`missing` 降为 10。

**这一步端到端验证了 `removed` 终态的完整路径**：台账 → evidence 校验 → 测试存在性 → 统计计数。

- [ ] **Step 8: 收紧 CI 软门禁**

编辑 `.github/workflows/ci.yml`：

**`governance` job** —— 删 `continue-on-error: true`，更新 job 名，删掉「目录不存在就软放行」的兜底（archtest 现在必然存在）：

```yaml
  governance:
    name: governance (GOV1-GOV6 + ledger)
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
          cache: true
      - name: run governance tests
        run: go test ./internal/archtest
```

**`fuzz-seed` job** —— 只删 `continue-on-error: true` 那一行，其余不动。

- [ ] **Step 9: 台账夺权**

在 `docs/feature-status-audit.md` 的标题下方、`> **审计日期**` 那行之前插入：

```markdown
> ⚠️ **本文件是 2026-07-31 的历史快照，不是当前状态的权威描述。**
> 当前状态以 `docs/feature-status.yaml` 为唯一真相源（`go run ./cmd/featurestatus` 查看统计）。
> 本报告的价值在于它记录了每一项判定的 `file:line` 证据与对抗式证伪过程，可用于追溯「为什么当初这么判」。
> 分解与修复计划见 `docs/superpowers/specs/2026-08-03-yanshi-roadmap-design.md`。
```

（这正是本报告自己对两份归档路线图所做的事——现在轮到它了。）

- [ ] **Step 10: 全量验证**

```bash
go build ./... && go vet ./... && go test ./...
```

预期：全绿。**这是 W0 的总验收**——三条治理断言 + 台账断言 + 一条移除断言全部常绿，且 CI 不再有软门禁。

- [ ] **Step 11: Commit**

```bash
git add -A
git commit -m "chore: close O12 by removal, harden CI gates, cede authority to ledger

O12: the VS Code extension never worked (runWithRecovery was never imported,
the README advertised reconnect behaviour that did not exist) and a third
front end alongside TUI and the Web IDE has an overlapping audience. Removed,
with a test that fails if it comes back — that test is the evidence backing
the ledger's first 'removed' verdict, which exercises the whole terminal-state
path end to end.

CI: drop continue-on-error from governance and fuzz-seed. The comments said
'soft until E3/E2 lands'; E3 and E2 landed, and governance now also carries
GOV4/GOV5/GOV6 and the ledger assertions.

Audit doc: demoted to a historical snapshot pointing at the ledger — the same
treatment it gave the two roadmaps it replaced.

Refs: spec 2026-08-03 §3.2 (4), §5.4, audit P2-14"
```

---

## W0 完成后的状态

| 项 | 值 |
|---|---|
| 治理断言 | GOV1/2/3（既有）+ GOV4/GOV5/GOV6（新）+ 台账完整性 + 移除断言 |
| 豁免表 | 3 张，共 **11 条**（`assemblyExceptions` 1 + `ctxInjectExceptions` 1 + `toolWiringExceptions` 9），**全部归属 W1** |
| 台账 | 63 项，`terminal: 1/63`（O12 removed） |
| CI | 无软门禁；`governance` job 硬跑全部治理断言 |

**豁免表的 11 条全部归属 W1，不是巧合**——W1 就是「装配线」包，W0 抓到的每一条断线都在它的范围内。**做完 W1，三张豁免表应该同时清空。** 这是下一份 plan（`S0/W1`）的天然验收信号。

## W0 过程中的新发现（移交 W1）

**消费侧也有幽灵工具。** Task 4 删掉 profile 里的 `fs_mkdir` 时发现，这个从未存在过的工具名还活在 4 处生产代码里：

| 位置 | 是什么 | 后果 |
|---|---|---|
| `internal/guard/mode.go:123` | `editTools` map（`ModeAllowEdits` 自动批准集） | 一个永远不会被调用的工具占着自动批准名额 |
| `internal/cli/tui/styles.go:406` | 工具显示名映射 | 死条目 |
| `internal/cli/tui/entries.go:845` | silent-tool 的 `case` 标签 | **活 case 里的死标签**——同一 case 还列着 `fs_read`/`fs_write` 等真实工具，所以整个 case 是活的，只有 `fs_mkdir` 这个标签永不匹配 |
| `internal/cli/tui/frecency.go:217` | 路径提取的 `case` 标签 | 同上 |

**这与 GOV5 抓的是同一类幽灵，只是发生在消费侧而非授权侧**——GOV5 只看 profile allow 列表 vs 注册表，看不到「消费侧以为存在的工具」。

W0 **不清理**它（超出「让断线可见」的职责），但必须记下来，否则 profile 条目一删就再没人记得。**移交 W1**：清理这 4 处，并考虑是否值得加一条 GOV7（消费侧工具名必须存在于注册表）。

## 不在 W0 范围内的事

- **修任何断线** —— W0 只负责让断线**可见且不可复发**，修复是 W1-W10 的事
- **清理 `fs_mkdir` 的 4 处消费侧残留** —— 见上一节，移交 W1
- **`orchestrator.WithTurnRecorder` 之外的 GOV6 误报处理** —— 实测只有 2 条，Task 2/3 已各自消化
- **覆盖率门禁**（spec §4.3 W10）—— 属 W10，虽然同样是 CI 门禁但依赖 COV2/COV3 先达标
- **`docs.yml` 的 `paths` 补 `cmd/yanshi/**`**（审计 P2-16）—— 属 W10
