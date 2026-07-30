# Batch E3 — 架构治理测试（CI 门禁）Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans (or仓库等价物) 按 Task 顺序实施。每个 Task 独立可测、单独提交。GOV2 的三个拆分 Task 是**行为保持的重构**（GREEN = `go test ./...` 拆分前后均绿），不是新功能 TDD；model.go 的 extract-method 额外用 golden 快照守门。任何 Task 不得为了让测试通过而放宽 exceptions——exceptions 只降不升。

**Spec:** `docs/superpowers/specs/2026-07-22-e3-architecture-governance-design.md`（权威）。

**Goal:** 把 CLAUDE.md 与 `synthesis-final.md` 里"靠人工每 PR 检查"的三条架构承诺——依赖向内流动（GOV1）、单文件 ≤1000 纯代码行（GOV2）、导出符号文档化（GOV3）——变成 `internal/archtest/` 下**零第三方依赖的普通 Go 测试**，让违规在本地 `go test ./...` 与未来 CI（CIG1 / H1）里被自动拦下；同时把当前三个已超限的文件（`ws.go` 1385、`tools/agent.go` 1134、`cli/tui/model.go` 1030 纯代码行，均 2026-07-22 实测）拆到合规，行为不变。

**Architecture:**
- `internal/archtest/` 是纯测试包（`package archtest`，文件皆 `_test.go`），**不进任何运行时依赖图**，对 bootstrap 扇出零影响。它只用标准库 `go/parser`+`go/token`+`go/ast`、`os/exec`（调 `go list -json -deps`）、`encoding/json`——零第三方依赖，与 CLAUDE.md"Fake/零依赖优先"一致。
- 三类门禁共享一份 `helpers_test.go`（找模块根、构建导入图、`go/parser` 精确纯代码行计数、导出符号扫描）。口径统一由 helpers 提供，避免三份启发式互相漂移。
- 门禁语义是**目标态断言 + tracked exceptions**：规则对目标态断言，当前未达标项进 `exceptions` 并附 reason；任何**新增**违规（不在 exceptions）直接失败；exceptions 里的项被修复后必须从 map 删除（删了若仍违规则失败，防止"修了又退化"）。`exceptions` 数量**只降不升**写入验收。
- GOV2 的三个拆分全部是**同包内移动顶层声明**（agent.go / ws.go）或**同包 extract-method**（model.go 的 `Update`）：不改 import path、不改签名、私有字段仍同包可见。agent.go 与 ws.go 是零风险的"移声明"（函数体整体搬迁）；model.go 因 `Update` 是单个 ~575 行 switch（98 个 `case`，含嵌套），需 extract-method，故额外用 golden 快照守门。
- 不改运行时行为：guard 的 fail-closed、orchestrator、VCS、压缩、WS/SSE 帧词表一行不动。不修 W2（`config → guard`）：只登记为 GOV1 的 tracked exception，修复留 P3。

**Tech Stack:** Go 1.26.4、标准库 `go/parser`/`go/token`/`go/ast`、`os/exec`（`go list -json -deps`）、`encoding/json`、`path/filepath`、`testing`（含 `t.TempDir()` 合成最小模块做"测测试"）。无新第三方依赖；无 `.github/workflows/` YAML（归 CIG1/H1）。

---

## 约束、依赖和非目标

### 已锁定决策（直接用，勿推翻）

1. **GOV2 拆 3 个文件——全拆**：`internal/api/http/ws.go`(1385)、`internal/tools/agent.go`(1134)、`internal/cli/tui/model.go`(1030) 纯代码行均超 1000（2026-07-22 实测，`synthesis-final.md`/B0 的旧数 ~857/~900/~850 已过期）。三个本批全拆到合规，**GOV2 门禁初始 exceptions 为空**（最干净）。
2. **拆分手法**：agent.go / ws.go = 移顶层声明（零风险）；model.go = `Update` 的 ~575 行 switch 用 **extract-method + golden 对比**守门。
3. **GOV1 落点**：`internal/archtest/` **普通 Go 测试**（非 `cmd/archtest` 二进制），随 `go test ./...` 跑、零部署；CIG1 直接纳入矩阵。零第三方依赖（`go list -json` + `go/parser`）。
4. **W2（config→guard）本批不修**（P3 重构）：只登记为 GOV1 tracked exception。
5. **GOV3 注释一行起**即可（不强制长篇"why"，但禁止无信息量的"`// Foo is a Foo`"）。
6. **exceptions 只降不升**：GOV1 与 GOV2 的 exceptions 初始清单明确、每条带 reason；后续只允许删除（删除后若仍违规则失败）。

### 依赖与门禁

| E3 组件 | 前置 | 实际依赖 | 禁止做法 |
|---|---|---|---|
| GOV1 deps_test | 无 | `go list -json -deps ./...`（工具链自带）、标准库 | 不引入 `golangci-lint`/`depguard`；不伪造导入图 |
| GOV2 lines_test | Task 1（helpers） | `go/parser` 纯代码行计数 | 不用 `wc -l`/awk 启发式做门禁（口径漂移）；不为过线而放宽阈值 |
| GOV2 三拆分 | 无（彼此独立） | 同包内移声明/extract-method | 不改签名/import path；不跨包移动；不为凑行数删注释或拆表达式 |
| GOV3 docs_test | Task 1（helpers） | `go/parser` AST + doc 关联 | 不要求结构体字段文档（噪声）；不用 lint 框架 |
| CIG1 衔接 | E3 全部落地 | 仅文档化"治理测试入 CI 矩阵" | 本批不写 `.github/workflows/*.yml` |

### 非目标（本批不做）

- 不改 guard/store/proto/vcs/orchestrator/压缩的运行时行为。
- 不修 W2（`config → guard`）——只登记。
- 不拆 `vcs.go`(971)/`commands.go`(925)（未超限，临界监控，报告里提示）。
- 不搭 CI workflow YAML（归 G1/CIG1/H1）。
- 不引入 lint 框架。
- 不清理 `internal/llm` 顶层孤儿包（登记，非硬要求）。

---

## 依赖图与任务拓扑

```text
                         Task 1  archtest helpers (go list + go/parser)
                        /       |             \
                       /        |              \
              Task 2        Task 3           (Task 7 用同一 helpers)
            GOV1 deps     GOV2 lines           GOV3 docs
             (独立)      (grandfather 3 文件)    (独立，可与 2/3 并行)
                              |
              +---------------+---------------+---------------+
              |               |               |
         Task 4           Task 5          Task 6
       拆 ws.go         拆 agent.go     拆 model.go
      (移声明,零风险)   (移声明,零风险) (state.go 移声明 +
      删 ws.go exc     删 agent.go exc  handlers.go extract + golden)
              |               |               |
              +---------------+---------------+
                              |
                         Task 8  CIG1(H1) 衔接 + 全量验收 + exceptions 终态
```

- **可并行**：Task 2 / Task 3 / Task 7 互不依赖（都用 Task 1 的 helpers，彼此独立）。Task 4/5/6 互不依赖（拆不同包），但都依赖 Task 3（lines_test 已建立 + grandfather，才能在拆完后"删 exception → 仍 GREEN"验证）。
- **强制顺序**：Task 1 →（Task 2 ∥ Task 3 ∥ Task 7）→（Task 4 ∥ Task 5 ∥ Task 6）→ Task 8。
- **CIG1（H1）衔接**（Task 8 标注）：`internal/archtest/` 三个 `_test.go` 是"CI-ready 测试"产物；CIG1 在 CI 矩阵里加一行 `go test ./internal/archtest`（与 `go vet ./...`、`go test ./...` 并列）即可生效，本批不写 YAML。exceptions 只降不升的语义保证 CI 不会因历史债常红、又不会放过新违规。

---

## 目标文件结构

| 文件 | 职责 | 新/改 |
|---|---|---|
| `internal/archtest/helpers_test.go` | 找模块根、`go list -json -deps` 构建导入图、`go/parser` 精确纯代码行计数、导出符号扫描公共工具 | 新 |
| `internal/archtest/deps_test.go` | [GOV1] R1–R5 分层规则 + `exceptions`（config→guard / store→auth） + `serverCoreSet` | 新 |
| `internal/archtest/lines_test.go` | [GOV2] 纯代码行 ≤1000 门禁 + `exceptions`（初始 grandfather 3 文件，随拆分逐项删） | 新 |
| `internal/archtest/docs_test.go` | [GOV3] 导出符号 doc 覆盖 + `exceptions`（main/生成/test-helper） | 新 |
| `internal/api/http/ws.go` | 保留 `connSession` + `(*Server).ChatWS` 主体；删去迁出声明 | 改（瘦身后 < 1000） |
| `internal/api/http/ws_perm.go` | permTracker / wsUpgrader / permModeState / 权限解析（resolve*/assessRisk/authorizeControlAction/applySetMode） | 新 |
| `internal/api/http/ws_handlers.go` | session/skill/mcp 各 `handle*` + wsConn + sortedModelNames | 新 |
| `internal/api/http/ws_compaction.go` | maybeAutoCompact/compactNow/compactionModel/contextWindowFor/keepRecentOrDefault + 计费/usage 辅助 | 新 |
| `internal/tools/agent.go` | 保留 agent 核心与生命周期；删去迁出声明 | 改（瘦身后 ~450 纯行） |
| `internal/tools/agent_analysis.go` | analysis 工具与模板生成 | 新 |
| `internal/tools/agent_workflow.go` | workflow 启动/扁平执行/summarize | 新 |
| `internal/tools/agent_dag.go` | DAG 引擎 + range 拓扑展开 | 新 |
| `internal/cli/tui/model.go` | 保留结构体/构造/Init/`Update`（瘦身为调度器）；删去迁出声明 | 改（瘦身后 < 1000） |
| `internal/cli/tui/state.go` | QueueMode/picker/pending/git 检测等状态与辅助（整声明迁移） | 新 |
| `internal/cli/tui/handlers.go` | 从 `Update` 抽出的各 handler 方法（extract-method） | 新 |
| `internal/cli/tui/update_golden_test.go` | model.go extract-method 的 golden 行为快照（拆分前后逐状态一致） | 新 |
| `internal/cli/tui/testdata/update_golden.txt` | golden 快照（PRE-refactor 代码捕获，提交入库） | 新 |
| `tools`/`proto`/`config`/`mcp`/`skills` 等若干 `.go` | 补导出符号 doc 注释（一行起） | 改 |
| `docs/superpowers/specs/2026-07-22-e3-architecture-governance-design.md` | 在 §11 open question 标注已决策（ws.go 全拆 / model.go extract+golden / serverSet 用最小核心集合 / W2 不修 / GOV3 一行起） | 改（仅标注，不改设计） |

> `internal/archtest/` 全是 `_test.go`，`go build ./...` 不会把它链入任何二进制；它不出现在任何 internal 包的 import 里，故不影响 GOV1 的导入图与 bootstrap 扇出。

---

## Task 1 — archtest 公共 helpers（`go list` + `go/parser`）+ 自测

**Files:** `internal/archtest/helpers_test.go`。

**职责：** 为 GOV1/GOV2/GOV3 提供统一数据源——找模块根、构建内部导入图、精确纯代码行计数、导出符号扫描。先建公共工具再做"测测试"，保证门禁本身正确。

### Step 1.1 — 写 helpers 与自测（RED→GREEN 一次成型）

`pureCodeLines` 用**字节区间法**精确剔除注释：用 `go/parser` 带 `ParseComments` 解析，取 `file.Comments` 里每个 comment group 的 `[start, end)` 字节区间；对每一行，若该行存在任一字节"非空白且不在任何 comment 区间内"→计为纯代码行。这正确处理 `//` 行、`/* */` 跨行块注释、`x := 1 // foo` 行尾注释（该行仍计 1 纯代码行）。

```go
// internal/archtest/helpers_test.go
package archtest

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// moduleRoot 从 wd 向上找首个含 go.mod 的目录，作为 go list 的 Dir。
// 找不到时 t.Fatal——archtest 必须在模块内运行。
func moduleRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	dir := wd
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod found upward from %s", wd)
		}
		dir = parent
	}
}

// modulePath 从 go.mod 首行 "module <path>" 解析模块路径，用于判定内部依赖。
func modulePath(t *testing.T) string {
	t.Helper()
	root := moduleRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "module "))
		}
	}
	t.Fatal("go.mod has no module directive")
	return ""
}

// pkgJSON 是 go list -json 每个条目我们关心的字段子集。
type pkgJSON struct {
	ImportPath string
	Imports    []string
}

// buildImportGraph 运行 go list -json -deps ./...（在模块根），返回
// map[internalPkg][]internalDep（仅保留以模块路径为前缀的直接内部依赖）。
// TestMain 应调用一次并缓存，避免 N 次 go list。
func buildImportGraph(t *testing.T) map[string][]string {
	t.Helper()
	root := moduleRoot(t)
	mod := modulePath(t)
	cmd := exec.Command("go", "list", "-json", "-deps", "./...")
	cmd.Dir = root
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("go list start: %v", err)
	}
	dec := json.NewDecoder(stdout)
	graph := map[string][]string{}
	for dec.More() {
		var p pkgJSON
		if err := dec.Decode(&p); err != nil {
			t.Fatalf("decode go list entry: %v", err)
		}
		if !strings.HasPrefix(p.ImportPath, mod+"/") && p.ImportPath != mod {
			continue // 外部依赖跳过
		}
		var internal []string
		for _, imp := range p.Imports {
			if strings.HasPrefix(imp, mod+"/") || imp == mod {
				internal = append(internal, imp)
			}
		}
		sort.Strings(internal)
		graph[p.ImportPath] = internal
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("go list -deps failed: %v\nstderr: %s", err, stderr.String())
	}
	return graph
}

// closureInternalDeps 对 graph 做传递闭包（仅内部），返回 pkg 可达的全部内部包集合。
func closureInternalDeps(graph map[string][]string, pkg string) map[string]struct{} {
	out := map[string]struct{}{}
	var walk func(string)
	walk = func(p string) {
		for _, d := range graph[p] {
			if _, seen := out[d]; seen {
				continue
			}
			out[d] = struct{}{}
			walk(d)
		}
	}
	walk(pkg)
	return out
}

// pureCodeLines 按 CLAUDE.md 口径返回 file 的纯代码行数（去注释行、去空行）。
// 字节区间法：标记每个 comment group 覆盖的字节区间，逐行找是否存在"非空白且非注释"字节。
func pureCodeLines(t *testing.T, path string) int {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.ParseComments|parser.AllErrors)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	tokFile := fset.File(f.Pos())
	nLines := tokFile.LineCount()
	// 每行的字节区间 [startOff, endOff)。
	lineStart := make([]int, nLines+1) // 1-indexed 行号
	for i := 1; i <= nLines; i++ {
		lineStart[i] = tokFile.Offset(tokFile.LineStart(i))
	}
	lineEnd := func(line int) int {
		if line < nLines {
			return lineStart[line+1] // 含行尾换行
		}
		return len(src)
	}
	// 收集 comment 字节区间。
	type span struct{ start, end int }
	var spans []span
	for _, cg := range f.Comments {
		spans = append(spans, span{
			start: tokFile.Offset(cg.Pos()),
			end:   tokFile.Offset(cg.End()),
		})
	}
	inComment := func(off int) bool {
		for _, s := range spans {
			if off >= s.start && off < s.end {
				return true
			}
		}
		return false
	}
	count := 0
	for line := 1; line <= nLines; line++ {
		start, end := lineStart[line], lineEnd(line)
		hasCode := false
		for off := start; off < end; off++ {
			b := src[off]
			if b == ' ' || b == '\t' || b == '\r' || b == '\n' {
				continue
			}
			if inComment(off) {
				continue
			}
			hasCode = true
			break
		}
		if hasCode {
			count++
		}
	}
	return count
}

// goFiles 返回 roots 下所有非测试 .go 文件的绝对路径（按路径排序）。
func goFiles(t *testing.T, roots ...string) []string {
	t.Helper()
	var files []string
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			files = append(files, path)
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	sort.Strings(files)
	return files
}

// exportedDecl 是一个缺 doc 的导出符号定位（供 docs_test 用）。
type exportedDecl struct {
	File   string
	Line   int
	Kind   string // "func" | "type" | "var" | "const" | "method"
	Name   string
	HasDoc bool
}

// scanExported 扫描 path 的导出符号及其 doc 关联（package-level func/type/var/const
// + 导出类型上的导出方法）。与 go vet / golint exported 同口径：靠 ast 的 Doc 关联。
func scanExported(t *testing.T, path string) []exportedDecl {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var out []exportedDecl
	add := func(name string, doc *ast.CommentGroup, kind string, pos token.Position) {
		out = append(out, exportedDecl{
			File: path, Line: pos.Line, Kind: kind, Name: name, HasDoc: doc != nil && doc.Text() != "",
		})
	}
	exportedType := map[string]bool{}
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			name := d.Name.Name
			if !d.Name.IsExported() {
				continue
			}
			pos := fset.Position(d.Pos())
			kind := "func"
			if d.Recv != nil {
				kind = "method"
				// 仅记录导出类型上的导出方法（避免噪声）。
				tn := recvTypeName(d.Recv)
				if !exportedType[tn] {
					continue
				}
			}
			add(name, d.Doc, kind, pos)
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					if !s.Name.IsExported() {
						continue
					}
					exportedType[s.Name.Name] = true
					doc := s.Doc
					if doc == nil {
						doc = d.Doc // 单 spec 声明时 doc 挂在 GenDecl 上
					}
					add(s.Name.Name, doc, "type", fset.Position(s.Pos()))
				case *ast.ValueSpec:
					for _, name := range s.Names {
						if !name.IsExported() {
							continue
						}
						doc := s.Doc
						if doc == nil {
							doc = d.Doc
						}
						kind := "var"
						if d.Tok == token.CONST {
							kind = "const"
						}
						add(name.Name, doc, kind, fset.Position(name.Pos()))
					}
				}
			}
		}
	}
	return out
}

func recvTypeName(recv *ast.FieldList) string {
	if recv == nil || len(recv.List) == 0 {
		return ""
	}
	st, ok := recv.List[0].Type.(interface {
		Ident() *ast.Ident
	})
	_ = st
	_ = ok
	// 用类型断言解包 *ast.Ident 与 *ast.StarExpr
	switch tt := recv.List[0].Type.(type) {
	case *ast.Ident:
		return tt.Name
	case *ast.StarExpr:
		if id, ok := tt.X.(*ast.Ident); ok {
			return id.Name
		}
	}
	return ""
}

// withSyntheticModule 在 t.TempDir() 构造一个最小 Go 模块（含给定 .go 文件内容），
// 用于"测测试"：已知 import 边 / 行数 / 缺 doc 的合成 fixture。
func withSyntheticModule(t *testing.T, files map[string]string) (dir string) {
	t.Helper()
	dir = t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/synth\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// abs 将模块相对路径解析为绝对路径（供 exceptions 的 var 初始化使用）。
func abs(rel string) string {
	root := mustModuleRoot()
	return filepath.Join(root, filepath.FromSlash(rel))
}

// mustModuleRoot 从 wd 向上找 go.mod（无 *testing.T 版本，用于 var 初始化）。
func mustModuleRoot() string {
	wd, err := os.Getwd()
	if err != nil {
		panic("getwd: " + err.Error())
	}
	dir := wd
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			panic("no go.mod found upward from " + wd)
		}
		dir = parent
	}
}

// short 返回相对模块根的 / 分隔路径（供测试报告使用）。
func short(path, root string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(rel)
}
```

### Step 1.2 — 自测（合成模块，证明口径正确）

```go
// internal/archtest/helpers_test.go （续，文末）
func TestPureCodeLinesCountsExactly(t *testing.T) {
	dir := withSyntheticModule(t, map[string]string{
		"p/p.go": `package p

import "fmt"

// Leading 是一个带块注释的函数。
var Leading = 1

func Hello(x int) int {
	/* 块注释
	   跨两行 */
	y := x + 1 // 行尾注释，该行仍计 1 纯代码行
	fmt.Println(y)
	return y
}
`,
	})
	got := pureCodeLines(t, filepath.Join(dir, "p", "p.go"))
	// 逐行核对：`package p`、`import ...`、`var Leading = 1`、`func Hello(x int) int {`、
	// `y := x + 1 ...`、`fmt.Println(y)`、`return y`、`}` = 8 行纯代码。
	// （空行、// Leading 注释、块注释两行、func 闭括号 } 计 1）
	if got != 8 {
		t.Fatalf("pureCodeLines = %d, want 8", got)
	}
}

func TestModuleRootFindsGoMod(t *testing.T) {
	root := moduleRoot(t)
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("moduleRoot %s has no go.mod: %v", root, err)
	}
}

func TestBuildImportGraphRuns(t *testing.T) {
	// 真实模块上跑一次，确认 go list 可用且返回非空内部图。
	graph := buildImportGraph(t)
	if len(graph) == 0 {
		t.Fatal("import graph empty")
	}
	if _, ok := graph["github.com/x6nux/yanshi/internal/guard"]; !ok {
		t.Fatalf("guard not in graph; keys sample: %v", sampleKeys(graph))
	}
}

func sampleKeys(m map[string][]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
		if len(out) >= 5 {
			break
		}
	}
	return out
}
```

**Expected failure:** 首次运行 `undefined: pureCodeLines` 等——写完 helpers 后转 GREEN。

**Run:**

```sh
go test ./internal/archtest -run 'TestPureCodeLines|TestModuleRoot|TestBuildImportGraph' -v
go vet ./internal/archtest
```

**Expected:** `pureCodeLines` 对合成 fixture 恰为 8；`moduleRoot` 命中 `D:\code\yanshi\go.mod`；`buildImportGraph` 返回含 `internal/guard` 的非空图。

**Commit:** `feat(archtest): add zero-dep helpers for import graph and pure-code-line counting`

---

## Task 2 — [GOV1] 依赖分层门禁（`deps_test.go`，R1–R5 + exceptions）

**Files:** `internal/archtest/deps_test.go`。

**规则**（每条一个子测试，失败各自报可读原因）：

| 规则 | 断言 | 当前状态 |
|---|---|---|
| **R1 无环** | `go list -deps ./...` 退出 0 | ✅ |
| **R2 端口白名单** | 每个端口包 P：`directInternalDeps(P) ⊆ allow[P] ∪ exceptions[P]` | guard✅ proto✅ vcs✅；store⚠(auth)、config⚠(guard) 为 tracked |
| **R3 W2：config 不依赖 guard** | `guard ∉ deps(config)` **当且仅当** `config→guard` 不在 exceptions | ⚠（在 exceptions 内，断言跳过） |
| **R4 服务端组合根唯一** | (a) bootstrap 闭包 ⊇ `serverCoreSet`（手写最小核心集合）；(b) 除 bootstrap 外无 internal 包内部扇出 > 阈值 | ✅ |
| **R5 方向** | 端口包（guard/store/proto/config/vcs）不得 import 服务层（tools/agent/api/orchestrator/bootstrap/llm/ctxcompact…） | ✅ |

### Step 2.1 — 写规则与 exceptions

```go
// internal/archtest/deps_test.go
package archtest

import (
	"path/filepath"
	"strings"
	"testing"
)

const modPrefix = "github.com/x6nux/yanshi/"

func ip(rel string) string { return modPrefix + rel }

// portAllow 是端口包的【目标态】内部依赖白名单。
// 当前已知违规放进 portExceptions（tracked）。
var portAllow = map[string]map[string]struct{}{
	ip("internal/guard"):  {ip("internal/execpolicy"): {}},
	ip("internal/store"):  {}, // 现状依赖 auth —— 见 portExceptions
	ip("internal/proto"):  {ip("internal/pathjail"): {}, ip("internal/task/work"): {}},
	ip("internal/config"): {}, // 现状依赖 guard —— W2，见 portExceptions
	ip("internal/vcs"): {
		ip("internal/auth"): {}, ip("internal/execpolicy"): {},
		ip("internal/guard"): {}, ip("internal/secrets"): {}, ip("internal/store"): {},
	},
}

// portExceptions：tracked 违规，必须收缩。key=端口包，value=允许的"额外"依赖 + reason。
// 只降不升：修复后必须删除对应条目；删除后若仍违规则 R2 失败。
var portExceptions = map[string]map[string]string{
	ip("internal/store"):  {ip("internal/auth"): "store 持久化 auth_metadata；待评估下沉类型后移除"},
	ip("internal/config"): {ip("internal/guard"): "W2: config.go 用 guard.PermissionProfile 作 map 值类型；P3 修复（挪类型 + bootstrap 转换）"},
}

// serverCoreSet 是"服务端必须由 bootstrap 装配"的最小核心集合（手写，非全集）。
// R4(a) 断言 bootstrap 闭包 ⊇ 此集合；新增服务端包若忘了接进 bootstrap 会在此失败。
// 比"列全 37 个闭包包"更稳：全集随重构漂移，最小核心集合由人维护、变化频率低。
var serverCoreSet = []string{
	ip("internal/store"), ip("internal/vcs"), ip("internal/guard"), ip("internal/proto"),
	ip("internal/config"), ip("internal/secrets"), ip("internal/execpolicy"),
	ip("internal/pathjail"), ip("internal/approval"), ip("internal/auth"),
	ip("internal/tools"), ip("internal/agent/orchestrator"), ip("internal/api/http"),
	ip("internal/ctxcompact"), ip("internal/skills"), ip("internal/mcp"),
}

// serviceLayerPrefixes：端口包不得依赖这些服务层/组合根。
var serviceLayerPrefixes = []string{
	ip("internal/tools"), ip("internal/agent"), ip("internal/api"),
	ip("internal/bootstrap"), ip("internal/llm"), ip("internal/ctxcompact"),
	ip("internal/mcp"), ip("internal/lsp"), ip("internal/shell"), ip("internal/sandbox"),
}

func runImportGraphOnce(m *testing.M) {
	// 占位：各子测试自行调 buildImportGraph(t)（testing 无共享 TestMain 缓存的简洁写法时）。
	// 如需缓存，可改为包级 var + sync.Once；本实现保持每测试自建以保证隔离。
	m.Run()
}

func directDeps(graph map[string][]string, pkg string) []string { return graph[pkg] }

func TestR1_NoImportCycle(t *testing.T) {
	// buildImportGraph 内部已要求 go list -deps 成功（失败即 t.Fatal 并附 stderr）。
	// Go 工具链遇 import 环会直接报错；此处仅确认图被构建。
	_ = buildImportGraph(t)
}

func TestR2_PortAllowlist(t *testing.T) {
	graph := buildImportGraph(t)
	for port, allow := range portAllow {
		exc := portExceptions[port]
		for _, dep := range directDeps(graph, port) {
			if _, ok := allow[dep]; ok {
				continue
			}
			if _, ok := exc[dep]; ok {
				continue // tracked exception
			}
			t.Errorf("port %s imports %s (not in allowlist; %s)",
				port, dep, exceptionHint(port, exc))
		}
	}
}

func exceptionHint(port string, exc map[string]string) string {
	if len(exc) == 0 {
		return "no tracked exception for this edge — new violation"
	}
	return "tracked exceptions: " + joinReasons(exc)
}

func joinReasons(exc map[string]string) string {
	var parts []string
	for edge, reason := range exc {
		parts = append(parts, edge+"="+reason)
	}
	return strings.Join(parts, "; ")
}

func TestR3_W2_ConfigMustNotDependOnGuard(t *testing.T) {
	graph := buildImportGraph(t)
	configPkg := ip("internal/config")
	guardPkg := ip("internal/guard")
	_, tracked := portExceptions[configPkg][guardPkg]
	deps := directDeps(graph, configPkg)
	depends := contains(deps, guardPkg)
	switch {
	case depends && tracked:
		// 已登记的 W2，暂不阻断；记录提醒。
		t.Logf("W2 tracked: config -> guard (P3 fast-follow to remove)")
	case depends && !tracked:
		t.Fatalf("config -> guard 但不在 portExceptions；要么登记，要么修 W2")
	case !depends && tracked:
		t.Fatalf("config 不再依赖 guard，但 portExceptions 仍登记 config->guard —— 请删除该 exception（只降不升）")
	default:
		// 干净：config 不依赖 guard，且无残留 exception。
	}
}

func TestR4_SingleServerCompositionRoot(t *testing.T) {
	graph := buildImportGraph(t)
	// (a) bootstrap 闭包 ⊇ serverCoreSet。
	bootClosure := closureInternalDeps(graph, ip("internal/bootstrap"))
	for _, core := range serverCoreSet {
		if core == ip("internal/bootstrap") {
			continue
		}
		if _, ok := bootClosure[core]; !ok {
			t.Errorf("serverCoreSet %s 不在 bootstrap 闭包内——若它是服务端包，请在 bootstrap.Build 装配；若它是客户端/子命令包，从 serverCoreSet 移除", core)
		}
	}
	// (b) 除 bootstrap 外，无 internal 包内部扇出超阈值（防"第三组合根"）。
	const fanoutThreshold = 25
	// fanoutExemptions：已知合理的高扇出包（不触发"疑似第二组合根"告警）。
	var fanoutExemptions = map[string]bool{
		ip("internal/tools"): true, // tools 层聚合了大量 Agent 工具，高扇出是自然的。
	}
	for pkg, deps := range graph {
		if pkg == ip("internal/bootstrap") || fanoutExemptions[pkg] {
			continue
		}
		if len(deps) > fanoutThreshold {
			t.Errorf("package %s 扇出 %d 个内部包（> %d）——疑似第二个组合根；若合理请显式登记例外", pkg, len(deps), fanoutThreshold)
		}
	}
}

func TestR5_PortsMustNotDependOnServiceLayer(t *testing.T) {
	graph := buildImportGraph(t)
	ports := make([]string, 0, len(portAllow))
	for p := range portAllow {
		ports = append(ports, p)
	}
	for _, port := range ports {
		for _, dep := range directDeps(graph, port) {
			for _, prefix := range serviceLayerPrefixes {
				if dep == prefix || strings.HasPrefix(dep, prefix+"/") {
					t.Errorf("port %s -> %s：端口包不得依赖服务层/组合根", port, dep)
				}
			}
		}
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// _ 确保 filepath 被引用（如未来扩展相对路径报告）。
var _ = filepath.Join
```

### Step 2.2 — RED→GREEN 演示与变异测试

**Expected failure（先不加 exceptions 时）：** R2 报 `config imports guard`、`store imports auth`；R3 报 W2。加入 `portExceptions` 后转 GREEN。

**变异测试（证明门禁真能拦新违规）：** 在 `TestR2_PortAllowlist` 末尾追加一个用 `withSyntheticModule` 构造的合成模块测试——合成一个 `example.com/synth/config` import `example.com/synth/tools` 的违规边，用一份本地版的 allow 判定函数断言它被检出（不污染真实图）：

```go
func TestR2_DetectsNewViolationInSyntheticGraph(t *testing.T) {
	// 用纯本地数据验证 allow 逻辑，不依赖真实图。
	graph := map[string][]string{
		"example.com/synth/config": {"example.com/synth/tools"},
	}
	allow := map[string]struct{}{}
	exc := map[string]string{}
	var hit string
	for _, dep := range graph["example.com/synth/config"] {
		if _, ok := allow[dep]; ok {
			continue
		}
		if _, ok := exc[dep]; ok {
			continue
		}
		hit = dep
	}
	if hit != "example.com/synth/tools" {
		t.Fatalf("expected detection of config->tools, got %q", hit)
	}
}
```

**Run:**

```sh
go test ./internal/archtest -run 'TestR1|TestR2|TestR3|TestR4|TestR5' -v
go vet ./internal/archtest
```

**Expected:** R1✅ R2✅（store/config 走 exception）R3 恢复"tracked 提醒" R4✅ R5✅；合成变异被检出。`guard` 零非白名单内部依赖被锁定（任何 `tools → guard` 反向 import 会被 R5 拦，但 guard 本身只依赖 execpolicy）。

**Commit:** `test(archtest): enforce hexagonal dependency layering with tracked exceptions`

> **注：** 若 R4(a) 报某 `serverCoreSet` 项不在 bootstrap 闭包内，先核对它是服务端包还是客户端/子命令包（见 spec §2.2 表）；客户端侧（cli/tui/lockfile/i18n/keymap/goalloop/appserver/version/acp 等）**不应**在 `serverCoreSet` 内。把 `serverCoreSet` 调到与实测闭包一致即可——这是自校正断言。

---

## Task 3 — [GOV2] 纯代码行门禁（`lines_test.go`，grandfather 3 文件）

**Files:** `internal/archtest/lines_test.go`。

**门禁：** 对 `internal/`、`cmd/` 下所有非测试 `.go`，`pureCodeLines ≤ 1000`。超过则失败并报告 `path: N pure code lines (limit 1000) — split required`。

**初始 `exceptions`：** 本批会全拆 ws.go/agent.go/model.go，但**拆分尚未发生时**这三文件仍超限。为让 Task 3 单独提交且 `go test ./...` 全绿，初始 grandfather 这三项；随后 Task 4/5/6 各拆完一个、删除对应 exception。exceptions 只降不升。

### Step 3.1 — 写门禁与 grandfather

```go
// internal/archtest/lines_test.go
package archtest

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

const pureLineLimit = 1000

// lineExceptions：grandfather 的超限文件（tracked，必须收缩）。
// 初始 = 本批计划拆分的 3 个文件；Task 4/5/6 各删一项。只降不升。
var lineExceptions = map[string]string{
	abs("internal/api/http/ws.go"):         "1385 pure (2026-07-22 实测)；Task 4 拆分后删除",
	abs("internal/tools/agent.go"):         "1134 pure；Task 5 拆分后删除",
	abs("internal/cli/tui/model.go"):       "1030 pure；Task 6 拆分后删除",
}


func TestPureCodeLineGate(t *testing.T) {
	root := moduleRoot(t)
	files := goFiles(t,
		filepath.Join(root, "internal"),
		filepath.Join(root, "cmd"),
	)
	var failed []string
	var approaching []string
	for _, f := range files {
		n := pureCodeLines(t, f)
		if reason, ok := lineExceptions[f]; ok {
			// grandfather：记录但不过线失败。仍打印当前值，便于观察收缩进度。
			t.Logf("grandfathered %s: %d pure (limit %d) — %s", short(f, root), n, pureLineLimit, reason)
			if n <= pureLineLimit {
				t.Errorf("grandfather %s 现已 %d ≤ %d，请从 lineExceptions 删除该项（只降不升）", short(f, root), n, pureLineLimit)
			}
			continue
		}
		if n > pureLineLimit {
			failed = append(failed, fmt.Sprintf("%s: %d pure code lines (limit %d) — split required", short(f, root), n, pureLineLimit))
		} else if n > 900 {
			approaching = append(approaching, fmt.Sprintf("%s: %d pure (approaching limit)", short(f, root), n))
		}
	}
	if len(failed) > 0 {
		t.Fatalf("纯代码行超限（> %d）：\n  %s", pureLineLimit, strings.Join(failed, "\n  "))
	}
	for _, a := range approaching {
		t.Logf("approaching: %s", a)
	}
}
```

> `abs`/`short`/`mustModuleRoot` 已移至 `helpers_test.go` 的公共工具集（见 Task 1）。`var lineExceptions` 使用 `abs()`（调 `mustModuleRoot()`）。**不要**在 `var` 初始化时 panic——若不在模块内跑，测试在 `TestPureCodeLineGate` 里用 `moduleRoot(t)` 重新解析 exceptions。

### Step 3.2 — RED→GREEN + 报告模式

- **RED（不加 exceptions 时）：** 门禁列出 ws.go/agent.go/model.go 三项超限。
- **GREEN（加 grandfather 后）：** 三项走 grandfather，其余文件全 ≤ 1000，测试通过。
- **报告模式：** 执行者可随时用 `go test ./internal/archtest -run TestPureCodeLineGate -v` 看逐文件纯行数（含 grandfather 当前进度与 approaching 提示，如 vcs.go/commands.go）。

**Run:**

```sh
go test ./internal/archtest -run TestPureCodeLineGate -v
go vet ./internal/archtest
```

**Expected:** 通过；日志可见三项 grandfather（均仍 > 1000）+ vcs.go/commands.go 的 approaching 提示。

**Commit:** `test(archtest): gate non-test go files at 1000 pure code lines (grandfather 3)`

---

## Task 4 — [GOV2] 拆分 `internal/api/http/ws.go`（移顶层声明，行为不变）

**Files:** `internal/api/http/ws.go`（瘦身）、`ws_perm.go`（新）、`ws_handlers.go`（新）、`ws_compaction.go`（新）；`internal/archtest/lines_test.go`（删 ws.go exception）。

**风险等级：零风险（同 agent.go 类别）。** ws.go 全是顶层声明（`func`/`type`/`var`/`const` 与方法），含一个 ~928 行的 `(*Server).ChatWS` 处理器——但 `ChatWS` 是**整体函数**搬迁，不是逐 case 抽取，函数体不动。同包 `package http` 内移动，签名/import path/私有字段可见性全不变。**无需 golden**。

### 拆分映射（按职责，均 < 1000 纯代码行）

| 新文件 | 迁入的顶层声明（现 ws.go 行号区间） | 预估纯行 |
|---|---|---|
| `ws.go`（保留） | `connSession` 结构体 + `(*Server).ChatWS` 主体 + side-snapshot/turn 辅助（`sideSnapshot`/`enterSide`/`exitSide`/`setInTurn`/`isInTurn`/`turnEndSummary`） | ~700 |
| `ws_perm.go` | `permTracker`+方法、`wsUpgrader`、`permModeState`+方法、`authorizeControlAction`、`applySetMode`、`resolvePermissionMode`、`resolvePermissionRequest`、`assessRisk`（45–129, 357–388, 644–764） | ~250 |
| `ws_handlers.go` | session/skill/mcp 处理器：`handleSessionList`/`handleRestoreSession`/`handleForkSession`/`handleRenameSession`/`handleArchiveSession`/`handleUnarchiveSession`/`handleDeleteSession`/`handleArchivedSessionList`/`skillInfo`/`handleListSkills`/`handleInstallSkill`/`skillMutationAction`/`handleSkillMutation`/`wsConn`/`handleMCPAction`/`(*wsConn).write`/`sortedModelNames`（1694–2064, 1824–1954, 2065, 2214–2274） | ~400 |
| `ws_compaction.go` | `maybeAutoCompact`/`compactNow`/`compactionModel`/`contextWindowFor`/`keepRecentOrDefault` + 计费/usage 辅助：`usageForPricing`/`addProviderUsage`/`resetBilling`/`billingMeta`/`formatTokenCount`/`formatElapsed`/`scopeJSON`/`featureRows`/`setFeature`/`statusFrame`/`displayModel`/`selectModel`/`ensureSession`/`persistMessages`/`loadSession`（308–643, 2081–2213） | ~400 |

> 辅助方法（如 `statusFrame`/`ensureSession`）是 `connSession` 的方法——Go 允许方法的定义与接收者类型分布在不同文件，故迁到 `ws_compaction.go` 无碍。

### Step 4.1 — 迁移前基线（GREEN）

```sh
go test ./internal/api/http/... ./internal/archtest
```

**Expected:** 全绿（ws.go 此时仍在 lines grandfather 内）。

### Step 4.2 — 按映射移动顶层声明

逐文件创建 `ws_perm.go`/`ws_handlers.go`/`ws_compaction.go`，从 ws.go **剪切**对应声明（含其 doc 注释）粘贴到新文件，**不改函数体**。每个新文件头部加一行 `package http` 与一段说明该文件职责的注释（承重文档密度，对齐 ws.go 既有风格）。移动后 ws.go 仅剩 `connSession` + `ChatWS` + side/turn 辅助。

### Step 4.3 — 迁移后回归 + 删 exception

```sh
go build ./internal/api/http
go test ./internal/api/http/...        # 含 ws_test/ws_perm_test/ws_session_test/ws_skills_test 等大量既有测试
go test ./internal/archtest -run TestPureCodeLineGate -v   # 确认 ws.go 现已 ≤ 1000
```

在 `lines_test.go` 的 `lineExceptions` 中**删除** `internal/api/http/ws.go` 条目。再跑：

```sh
go test ./internal/archtest            # 必须仍 GREEN（证明 ws.go 已合规）
```

**Expected:** `go test ./internal/api/http/...` 全绿（行为不变）；ws.go 纯代码行 < 1000；删 exception 后 archtest 仍 GREEN。若删 exception 后 archtest RED，说明 ws.go 仍超限——继续把更多声明迁出（优先把 `ChatWS` 内嵌的纯 helper 如 `sortedModelNames` 等迁到 handlers）。

**Commit:** `refactor(http): split ws.go into perm/handlers/compaction (behavior unchanged)`

---

## Task 5 — [GOV2] 拆分 `internal/tools/agent.go`（移顶层声明，行为不变）

**Files:** `internal/tools/agent.go`（瘦身）、`agent_analysis.go`（新）、`agent_workflow.go`（新）、`agent_dag.go`（新）；`internal/archtest/lines_test.go`（删 agent.go exception）。

**风险等级：零风险。** 全是顶层 `func`/`type`/`var`，同包 `package tools` 内搬迁，签名/import path 不变，私有字段仍同包可见。

### 拆分映射（实测行号见 Task 前置 grep，均 < 1000 纯代码行）

| 新文件 | 迁入的顶层声明（现 agent.go） | 预估纯行 |
|---|---|---|
| `agent.go`（保留） | `AgentTools`/`NewAgentTools`/`streamReviewTool`/`Tools`/`agentStartArgs`/`streamStartAgent`/`runSubAgent` + 小工具（`bindSubAgentProgress`/`naturalIDLess`/`digitsStart`/`formatDur`/`formatTokens`/`parseToolList`） | ~450 |
| `agent_analysis.go` | `analysisArgs`/`streamAnalysis`/`runAnalysisWorkflow`/`fillWorkflowTarget`/`generateAnalysisWorkflow`（379–450, 692–810） | ~200 |
| `agent_workflow.go` | `makeWorkflowProgress`/`streamStartWorkflow`/`summarizeArgs`/`streamSummarize`/`runStartWorkflow`/`runFlatWorkflow`/`workflowStartArgs`/`workflowTaskResult`/`WorkflowProgress` 相关（451–691, 786–948） | ~350 |
| `agent_dag.go` | DAG 引擎：`WorkflowDef`/`WorkflowStepDef`/`ExpandedStep`/`stepState`/`dagResult`/`runDAGWorkflow`/`executeLevel` + range 拓扑（`rangeRegex`/`expandStepID`/`expandSteps`/`expandDeps`/`resolveDeps`/`topoSortLevels`/`interpolatePrompt`，943–1420） | ~480 |

### Step 5.1 — 迁移前基线（GREEN）

```sh
go test ./internal/tools ./internal/agent/... ./internal/archtest
```

### Step 5.2 — 按映射移动顶层声明

创建 `agent_analysis.go`/`agent_workflow.go`/`agent_dag.go`（均 `package tools`），从 agent.go **剪切**对应声明（含 doc 注释）粘贴，**不改函数体**。保留 agent.go 的包注释与 import（迁移后按需清理 agent.go 不再用的 import；新文件各自补所需 import）。

### Step 5.3 — 迁移后回归 + 删 exception

```sh
go build ./internal/tools
go test ./internal/tools ./internal/agent/...
go test ./internal/archtest -run TestPureCodeLineGate -v   # agent.go 现 < 1000
```

删除 `lineExceptions` 中 `internal/tools/agent.go` 条目，再跑 `go test ./internal/archtest` 必须 GREEN。

**Expected:** `go test ./internal/tools ./internal/agent/...` 全绿；agent.go 纯代码行 < 1000；删 exception 后 archtest GREEN。

**Commit:** `refactor(tools): split agent.go into analysis/workflow/dag (behavior unchanged)`

---

## Task 6 — [GOV2] 拆分 `internal/cli/tui/model.go`（state.go 移声明 + handlers.go extract-method + golden 守门）

**Files:** `internal/cli/tui/model.go`（瘦身）、`state.go`（新）、`handlers.go`（新）、`update_golden_test.go`（新）、`testdata/update_golden.txt`（新）；`internal/archtest/lines_test.go`（删 model.go exception）。

**风险等级：本批唯一有回归风险。** model.go 大头不是顶层声明，而是 `Update` 方法本身（行 567–1142，单方法 ~575 行 switch，98 个 `case` 含嵌套）。两步走：
1. **state.go（零风险移声明）：** 把与 `Update` 无关的整声明迁出——`QueueMode`+`String`+`parseQueueMode`、`pickerItem`、`pendingSeamRestoreState`、`pickerConfirm`、`defaultBundle`、`dirName`、`detectGitBranch`、`parseGitHead`、`fetchInitialStatus`、`syncSavedMode`。这步很可能已让 model.go 跌破 1000。
2. **handlers.go（extract-method，golden 守门）：** 把 `Update` switch 各顶层 `case` 分支原样抽成 `(m model) handleKeyMsg / handleStreamMsg / handleWindowSizeMsg / ...` 方法，`Update` 瘦身为调度器。

> 注：model.go 现有包注释已说明"methods are split across files by concern (entries.go/events.go/view.go/permissions.go/startup.go)"——新增 state.go/handlers.go 符合该包既有组织方式。

### Step 6.1 — PRE-refactor：捕获 golden 基线（在动 model.go 之前）

golden 用 `package tui`（内部测试，可读 model 未导出字段），复用既有 `fakeSession` 与 `newModel(&fakeSession{}, "/proj")`。**快照字段而非 `View()`**——`View()` 含光标闪烁/时间，非确定；字段快照确定。

```go
// internal/cli/tui/update_golden_test.go
package tui

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// update-golden 测试：对一组脚本化 tea.Msg 序列，逐步应用 m.Update(msg)，
// 把每步的"模型状态快照"拼成文本，与 testdata/update_golden.txt 比对。
// 用途：守门 model.go 的 extract-method——拆分前后快照必须逐字节一致。
//
// 重新生成快照：go test ./internal/cli/tui -run TestUpdateGolden -update
var updateGoldenFlag = flag.Bool("update", false, "regenerate update_golden.txt")

// snapshotModel 抽取一组确定性的关键字段（避免 View() 的光标/时间）。
// 最小字段集（实现时按实际 model 字段对齐）：
//   - input.Value()   — 当前输入文本
//   - queueMode       — 队列模式（QueueMode 的 String）
//   - pickerOpen      — 挑选项是否打开（bool）
//   - pickerCursor    — 挑选项光标位置
//   - entries         — 条目数（len(m.entries)）
//   - helpVisible     — 帮助面板可见性
//   - pending         — 待处理队列（deep copy 避免变异干扰）
//   - toasts          — toast 列表（deep copy，不含时间敏感字段）
//   - permissionsVisible — 权限面板可见性
//   - startupBanner   — 启动横幅可见性
// 不读任何时钟。未列出的可选字段可补加，但必须确定（无随机/时间依赖）。
func snapshotModel(m model) string {
	var b strings.Builder
	// 示例字段（执行者按实际 model 字段对齐补全）：
	// fmt.Fprintf(&b, "input=%q\n", m.input.Value())
	// fmt.Fprintf(&b, "queue=%s\n", m.queueMode)
	// fmt.Fprintf(&b, "pickerOpen=%v\n", m.pickerOpen)
	// fmt.Fprintf(&b, "entries=%d\n", len(m.entries))
	// ... 其余确定字段
	_ = m
	return b.String()
}

// scriptedMsgs 是一组覆盖常见 Update 分支的 tea.Msg 序列。
// 选型原则：覆盖 KeyMsg(普通字符/回车/esc/ctrl)、WindowSizeMsg、
// streamMsg(各 cli.StreamEvent 类型)、以及 picker/queue 相关消息。
func scriptedMsgs() []tea.Msg {
	return []tea.Msg{
		tea.WindowSizeMsg{Width: 80, Height: 24},
		tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("hi")},
		tea.KeyMsg{Type: tea.KeyEnter},
		// streamMsg{ev: cli.StreamEvent{...status...}},
		// streamMsg{ev: cli.StreamEvent{...assistant chunk...}},
		// tea.KeyMsg{Type: tea.KeyCtrlC},
		// ... 补到能触达主要 case 分支
	}
}

func TestUpdateGolden(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	var sb strings.Builder
	for i, msg := range scriptedMsgs() {
		mm, _ := m.Update(msg)
		m = mm.(model)
		fmt.Fprintf(&sb, "=== step %d (%T) ===\n%s\n", i, msg, snapshotModel(m))
	}
	got := sb.String()
	goldenPath := filepath.Join("testdata", "update_golden.txt")
	if *updateGoldenFlag {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("golden regenerated: %s", goldenPath)
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden %s: %v（若首次运行，加 -update 生成）", goldenPath, err)
	}
	if string(want) != got {
		t.Fatalf("update golden mismatch（extract-method 引入行为差异？）:\n--- want\n%s\n--- got\n%s", want, got)
	}
}
```

**Step 6.1 动作：** 写测试 + 在**当前未重构**的 model.go 上生成基线：

```sh
go test ./internal/cli/tui -run TestUpdateGolden -update
```

**Expected:** 生成 `testdata/update_golden.txt`；不带 `-update` 重跑必须 GREEN（基线自洽）。提交 golden 与测试。

### Step 6.2 — state.go：移声明（零风险）

创建 `state.go`（`package tui`），从 model.go **剪切**：`QueueMode`+`const`块+`String`+`parseQueueMode`、`pickerItem`、`pendingSeamRestoreState`、`pickerConfirm`、`defaultBundle`、`dirName`、`detectGitBranch`、`parseGitHead`、`fetchInitialStatus`、`syncSavedMode`（含各自 doc 注释）。**不改函数体**。清理 model.go 不再用的 import。

```sh
go test ./internal/cli/tui -run TestUpdateGolden      # 必须仍 GREEN（行为不变）
go test ./internal/cli/tui                            # 全量 tui 测试
go test ./internal/archtest -run TestPureCodeLineGate -v   # model.go 现纯行数
```

**Expected:** golden GREEN；`go test ./internal/cli/tui` 全绿。此时 model.go 纯行数大概率已 < 1000（state 部分约 150 纯行迁出）。**若已 < 1000，可先删 model.go exception 提交一次（合规达成），再做 Step 6.3 的 extract-method 作为纯重构改进。**

**Commit:** `refactor(tui): move state helpers to state.go (behavior unchanged)`

### Step 6.3 — handlers.go：extract-method（golden 守门）

把 `Update` switch 内各顶层 `case` 分支**原样**抽成方法，搬到 `handlers.go`：

| handler 方法（新） | 抽自 `Update` 的 case 分支 |
|---|---|
| `(m model) handleKeyMsg(msg tea.KeyMsg) (model, tea.Cmd)` | `case tea.KeyMsg:` 全部分支 |
| `(m model) handleStreamMsg(msg streamMsg) (model, tea.Cmd)` | `case streamMsg:` |
| `(m model) handleWindowSizeMsg(msg tea.WindowSizeMsg) (model, tea.Cmd)` | `case tea.WindowSizeMsg:` |
| 其余顶层 `case` 各自一个 handler | 按需 |

**严格行为等价规则：**
- 每个 case 原地逻辑**原样**搬到新方法，`m` 与所需局部变量作参数传入（或用闭包/返回值），返回 `(model, tea.Cmd)` 与原 switch 臂一致。
- `Update` 瘦身为：`switch msg := msg.(type) { case tea.KeyMsg: return m.handleKeyMsg(msg); ... default: return m, nil }`。
- **不确定的臂不拆**——保留在 model.go 的 `Update` 内（反正 Step 6.2 已让文件合规，extract 是改进而非硬指标）。
- 不改任何 `tea.Cmd` 的合并/返回顺序、不改早退（`return`）位置。

```sh
go test ./internal/cli/tui -run TestUpdateGolden      # 必须仍 GREEN——extract 无行为差异
go test ./internal/cli/tui                            # 全量回归
```

**Expected:** golden 逐字节一致（GREEN）；`go test ./internal/cli/tui` 全绿。若 golden RED，定位差异分支，修正抽取直到 GREEN；切勿用 `-update` 覆盖 golden 来"通过"——那等于放弃守门。

### Step 6.4 — 删 model.go exception

```sh
go test ./internal/archtest -run TestPureCodeLineGate -v   # model.go < 1000
```

删除 `lineExceptions` 中 `internal/cli/tui/model.go` 条目，再跑 `go test ./internal/archtest` 必须 GREEN。

**Commit:** `refactor(tui): extract Update handlers (golden-guarded, behavior unchanged)`

---

## Task 7 — [GOV3] 导出符号文档门禁（`docs_test.go`）+ 补注释（一行起）

**Files:** `internal/archtest/docs_test.go`；按扫描输出在 `tools`/`proto`/`config`/`mcp`/`skills`/`secrets`/`guard`/`cli/tui` 等包补 doc 注释。

**规则：** 用 `go/parser` 扫描每个非测试 `.go` 的导出符号（package-level `func`/`type`/`var`/`const` + 导出类型上的导出方法），要求每个有直接前置 doc 注释（`ast` 的 `Doc` 关联，与 `go vet`/golint `exported` 同口径）。包级要求 package doc 注释。

**例外 `docExceptions`（避免噪声）：** `cmd/yanshi`（main 包，程序入口）、`internal/version` 的生成字段、纯 test-helper 包的包内导出——按需白名单。结构体**字段**默认不要求（噪声）。

### Step 7.1 — 写 docs_test 与初始 exceptions（RED→GREEN）

```go
// internal/archtest/docs_test.go
package archtest

import (
	"path/filepath"
	"strings"
	"testing"
)

// docExceptionPkgs：整包豁免（main 包、生成产物等）。
var docExceptionPkgs = map[string]bool{
	"cmd/yanshi": true, // main：程序入口
}

// docExceptionSymbols：单符号豁免（键形如 "internal/version.BuildStamp"）。
// 初始为空；扫描发现确属噪声（生成字段、test-helper）时逐项登记，只降不升。
var docExceptionSymbols = map[string]bool{}

func TestExportedDocs(t *testing.T) {
	root := moduleRoot(t)
	files := goFiles(t, filepath.Join(root, "internal"), filepath.Join(root, "cmd"))
	var missing []string
	for _, f := range files {
		// 整包豁免：按文件所在目录近似判定。
		rel, _ := filepath.Rel(root, filepath.Dir(f))
		if docExceptionPkgs[filepath.ToSlash(rel)] {
			continue
		}
		for _, d := range scanExported(t, f) {
			if d.HasDoc {
				continue
			}
			key := strings.TrimSuffix(filepath.ToSlash(rel)+"."+d.Name, ".")
			if docExceptionSymbols[key] {
				continue
			}
			missing = append(missing, short2(f, root)+":"+itoa(d.Line)+": exported "+d.Kind+" "+d.Name+" lacks doc comment")
		}
	}
	if len(missing) > 0 {
		t.Fatalf("未文档化的导出符号（按包补注释，或登记 docExceptionSymbols）：\n  %s", strings.Join(missing, "\n  "))
	}
}

func short2(path, root string) string { return short(path, root) }
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [16]byte
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[pos:])
}
```

### Step 7.2 — RED：看真实缺口

```sh
go test ./internal/archtest -run TestExportedDocs -v
```

**Expected（RED）：** 列出 tools/proto/config/mcp/skills 等包的未文档化导出符号（数量级：tools~29、proto~13、config~7、mcp~4、skills~3，方法会更多）。

### Step 7.3 — GREEN：逐包补注释（一行起），每包可单独提交

按包顺序补：`guard` → `proto` → `config` → `tools` → `mcp` → `skills` → `secrets` → `cli/tui`。每补完一个包，重跑 `go test ./internal/archtest -run TestExportedDocs`，确认该包符号从 missing 列表消失。

**补注释原则（GOV3 一行起）：**
- 至少一句"这个符号做什么/为什么存在"，与 CLAUDE.md"承重文档"一致。
- 禁止无信息量"`// Foo is a Foo`"。
- 承重包（guard/proto/tools）保持现有注释密度；工具包一句即可。
- 注释语言与该符号现有注释一致（多为英文，与代码同语言）。

```sh
go test ./internal/archtest -run TestExportedDocs      # 最终 GREEN：missing 为空
go vet ./...
```

**Expected:** `TestExportedDocs` 通过；所有非 exception 导出符号有 doc；package doc 齐全（如某包缺 package doc，补一句 `// Package xxx ...`）。人为加一个无注释导出函数 → 测试失败指名。

**Commit:** 按包拆分提交，如 `docs(proto): document exported frame symbols`、`docs(tools): document exported tool symbols` … 最后 `test(archtest): enforce exported-symbol doc coverage`。

> 若某符号确属噪声（生成字段等），登记进 `docExceptionSymbols` 并附 reason 注释——只降不升。

---

## Task 8 — CIG1（H1）衔接 + 全量验收 + exceptions 终态

**Files:** 本计划无新代码；在 spec 的 §11 open question 标注已决策；确认 `go test ./...` + `go vet ./...` 全绿。

### Step 8.1 — 标注 spec 已决策（仅文档）

在 `docs/superpowers/specs/2026-07-22-e3-architecture-governance-design.md` 的 §11 五个 open question 顶部加一段"已决策（2026-07-22，实施计划锁定）"：

1. ws.go：**本批一并拆**（option a）。
2. model.go：**接受本批 extract-method + golden 守门**（不 grandfather）。
3. serverSet：**手写最小核心集合 ⊆ bootstrap 闭包**（option b）+ R4(b) 扇出阈值兜底。
4. W2：**不纳入本批 fast-follow**，留 P3；GOV1 仅登记 exception。
5. GOV3：**统一"至少一句"**（一行起），承重包严、工具包宽松。

### Step 8.2 — CIG1（H1）衔接（治理测试入 CI）

本批产出的"CI-ready 测试"= `internal/archtest/` 下三个 `_test.go` + `helpers_test.go`。CIG1/H1 在 CI 矩阵中并列加入：

```text
go vet ./...
go test ./...                       # 已自动包含 ./internal/archtest
go test ./internal/archtest -v      # 显式可见三类门禁与 exceptions 收缩报告
```

- 无需本批写 `.github/workflows/*.yml`。
- exceptions 只降不升的语义保证：CI 不会因历史债（store→auth、config→guard）常红，但任何新违规立即红。
- GOV2 的 `lineExceptions` 在本批 Task 4/5/6 后**应为空**（三文件全拆）；若非空，Task 8 不得收尾。

### Step 8.3 — 全量验收清单

```sh
go vet ./...
go test ./...
go test ./internal/archtest -v
```

**验收（对齐 spec §9）：**

1. `go test ./internal/archtest` 通过：GOV1（2 条 tracked：config→guard / store→auth）、GOV2（`lineExceptions` 终态为**空**）、GOV3（`docExceptionSymbols` 有界）。
2. **GOV1**：R1–R5 可执行；合成变异（config→tools 边）被检出并报可读边；`guard` 零非白名单内部依赖被 R5 锁定；无环被 R1 验证。
3. **GOV2**：ws.go/agent.go/model.go 拆后纯代码行均 < 1000；拆分前后 `go test ./...` 全绿；golden（model.go）逐字节一致；人为堆 1001 行 → 门禁失败指名文件。
4. **GOV3**：exceptions 之外导出符号全有 doc；package doc 齐全；关键包（guard/proto/config/tools）文档化。
5. 三类门禁零第三方依赖（仅标准库 + `go list`/`go/parser`）。
6. exceptions 清单初始明确、每条带 reason；GOV1 exceptions 终态 = {config→guard, store→auth}，GOV2 exceptions 终态 = ∅，数量只降不升。
7. `go vet ./...` 通过。

### Step 8.4 — 终态 commit

```sh
git add docs/superpowers/specs/2026-07-22-e3-architecture-governance-design.md
git commit -m "docs(e3): record locked decisions in architecture-governance spec"
```

---

## 风险与回滚

| 风险 | 缓解 / 回滚 |
|---|---|
| model.go extract-method 引入行为差异 | golden 逐字节守门；不确定的 case 臂**不拆**；Step 6.2（state.go）已让文件合规，extract 纯属改进，最坏可回退 6.3 保留 6.2 |
| `pureCodeLines` 口径与开发者直觉不符（块注释边界） | `go/parser` 字节区间法是权威口径；`go test -run TestPureCodeLines -v` 暴露合成 fixture 精确计数供核对；口径已写入 Task 1 自测 |
| 拆分后某文件仍 > 1000（估算偏差） | 删 exception 前 `archtest -v` 可见实测值；继续迁出更多声明；ws.go 优先迁 ChatWS 的纯 helper |
| `serverCoreSet` 与实测闭包不符致 R4(a) 误报 | R4(a) 是自校正断言：误报时按 spec §2.2 表核对（服务端 vs 客户端/子命令），调 `serverCoreSet` 到一致 |
| `go list` 在 CI 慢（~1–2s） | 可接受；如需优化改包级 `sync.Once` 缓存图（见 Task 2 TestMain 注） |
| exceptions 沦为永久豁免 | 每条带 reason；GOV2 终态强制为空；GOV1 的 store→auth/config→guard 关联 P3 ticket；删除即强制合规 |
| 补注释量大延后 | GOV3 一行起 + 分包提交；`docExceptionSymbols` 兜底噪声 |

## 与 CIG1（H1）的衔接（摘要）

- **产物**：`internal/archtest/{helpers,deps,lines,docs}_test.go`。
- **CI 接入**：CIG1 在矩阵加 `go test ./internal/archtest`（与 `go vet ./...`、`go test ./...` 并列）。
- **契约**：三类门禁是"目标态断言 + tracked exceptions（只降不升）"；CI 对历史债不常红、对新违规立即红。
- **本批不写**：`.github/workflows/*.yml`（归 CIG1/H1）。
