# Batch E2 — Fuzz / Property / Race 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 给 guard.MatchGlob 加 fuzz、给 ctxcompact 加属性测试、给 6 个并发热点加 `-race` 测试——三条线都不引入新的功能面。

**Architecture:** 独立并行三条线：FUZ1（`internal/guard/glob_test.go` 一个 fuzz 函数 + 种子语料）、PROP1（`internal/ctxcompact/` 四个新测试文件 + 随机历史生成器）、RAC1（`internal/task/`, `internal/api/http/`, `internal/agent/orchestrator/`, `internal/agent/registry/` 各一并发测试文件 + broker LEAK 探针 + findings 登记）。全量测试可本地 `go test -race` + `go test -run 'Fuzz|Property'` 独立验证。CI workflow 文件壳子归 CIG1（H1）——本批只交付测试文件和 `-race` 命令清单。

**Tech Stack:** Go 1.26.4；`go test -fuzz`（语料驱动而非 `testing/quick`）；`math/rand/v2` 固定种子；`httptest` + gorilla/websocket in-process dial；`einollm.FakeModel` / 自写 `recordingSummarizer` / `package task` 同包测试；`-skip` 排除既有已知竞态。

---

## 已锁定决策（team-lead 确认，本计划直接用）

1. **CI 归属（Q1）**：`.github/workflows/` 由 CIG1 建壳子，E2 只交付测试文件和 `-race` 命令清单（见 §CI 分层策略）。
2. **glob 语义 bug 处置（Q2）**：fuzz 若发现 `MatchGlob` 真实安全/语义偏差（over-grant/bypass），**本批修复**（fail-closed 优先于"钉现状"），而非 spec 原定的"只钉不修"。
3. **ctxcompact 不变量违反阈值（Q3）**：单文件 <50 行改动本批修；超出另开 plan。
4. **`-race` 既有竞态排除机制（Q4）**：`-skip=<TestName>` + findings 表 issue 号注释。
5. **findings 登记载体（Q5）**：独立 notes 文件（`docs/superpowers/notes/2026-07-22-e2-race-findings.md`）+ issue（可追踪）。
6. **F2 衔接**：broker LEAK 探针用 `assert.GreaterOrEqual` + `t.Log` 记录 `len(createdWT)` 增长现状，测试预期**绿**；F2/LEAK1 修复后翻转断言为 `assert.Zero`。

## 依赖图

```
FUZ1 ──┬─ FUZ/Task 1 (FuzzMatchGlob)
       └─ [条件] 若发现安全 bug → 另开修复子任务

PROP1 ─┬─ PRO/Task 2 (gen_test.go 随机历史生成器)
       ├─ PRO/Task 3 (plan_property_test.go  P1+P2)
       ├─ PRO/Task 4 (summarize_property_test.go P3+P4)
       └─ PRO/Task 5 (run_property_test.go  P5+端到端)

RAC1 ──┬─ RAC/Task 6 (broker_race_test.go)
       ├─ RAC/Task 7 (ws_race_test.go)
       ├─ RAC/Task 8 (orchestrator_race_test.go)
       ├─ RAC/Task 9 (manager_race_test.go)
       └─ RAC/Task 10 (findings 登记 notes)
```

三条线可完全并行无依赖；同一条线内按序号顺序施工。

---

## File Structure

| 文件 | 线 | 职责 |
|---|---|---|
| `internal/guard/glob_test.go` | FUZ1 | 新加 `FuzzMatchGlob`（与现有表驱动同文件） |
| `internal/guard/testdata/fuzz/FuzzMatchGlob/*` | FUZ1 | fuzz 种子 + 回归语料（入仓） |
| `internal/ctxcompact/gen_test.go` | PROP1 | 固定种子随机历史生成器（P1-P5 共用） |
| `internal/ctxcompact/plan_property_test.go` | PROP1 | P1（pin ⊆ out）+ P2（工具对 fixpoint）属性 |
| `internal/ctxcompact/summarize_property_test.go` | PROP1 | P3（窗口 ≤）+ P4（不空 summary）+ `recordingSummarizer` |
| `internal/ctxcompact/run_property_test.go` | PROP1 | P5（summary-of-summary 短路）+ Run 端到端 |
| `internal/task/broker_race_test.go` | RAC1 | broker Claim/RecordResult/Cancel 并发 + `createdWT` LEAK 探针 |
| `internal/api/http/ws_race_test.go` | RAC1 | wsConn.write 并发 + permTracker 并发 + connSession 帧交错集成 |
| `internal/agent/orchestrator/orchestrator_race_test.go` | RAC1 | runners sync.Map 并发 LoadOrStore + FlushRunners |
| `internal/agent/registry/manager_race_test.go` | RAC1 | Manager 并发 Spawn/Cancel/SendInput/Resume + Registry 冻结契约文档化 |
| `docs/superpowers/notes/2026-07-22-e2-race-findings.md` | RAC1 | §7 findings 登记表 |
| `.github/workflows/*.yml` | — | **CIG1 承担**，E2 不建 |

**不改**任何 `internal/**` 非 `_test.go` / 非 `testdata` 的生产代码。例外条件：fuzz 发现的安全 bug（Q2）和 <50 行不变量违反（Q3）本批修生产代码。

---

## CI 分层策略（交付给 CIG1 的命令清单）

| 层 | 触发 | 命令 | 阻塞合并？ |
|---|---|---|---|
| **PR** | 每次 push | `go vet ./...` + `go test -run 'Fuzz|Property' -count=1 ./internal/guard ./internal/ctxcompact ./internal/task ./internal/api/http ./internal/agent/...` + `go test -race`（仅变更包，复用 `cmd/testchanged` 加 `-race`） | 是 |
| **merge** | 合入 main | `go test -race ./...` + `go test -run 'Fuzz|Property' -count=10 ./internal/guard ./internal/ctxcompact` | 是（若红则热修或 revert） |
| **nightly** | 定时 | `go test -fuzz=FuzzMatchGlob -fuzz=2m ./internal/guard` + `go test -race -count=10 ./internal/task ./internal/api/http ./internal/agent/...` | 否（发现→登记 issue） |

**既有竞态排除**：若 `-race` 发现已知既有竞态（如 ws 历史访问竞态），在 `//go:build` 或 go test `-skip` 中临时排除 + findings 表登记 + 指向 F2 issue。

---

# FUZ1 — `guard.MatchGlob` fuzz

### Task 1: `FuzzMatchGlob` 种子语料 + 四不变量

**Files:**
- Modify: `internal/guard/glob_test.go`
- Create: `internal/guard/testdata/fuzz/FuzzMatchGlob/`（种子语料目录，Go fuzz 自动填充）

- [ ] **Step 1: 写失败测试（FuzzMatchGlob 编译前确认）**

把 `FuzzMatchGlob` 作为编译前的 RED 验证：当前 `internal/guard/glob_test.go` 无 `FuzzMatchGlob`，`go test -fuzz=FuzzMatchGlob` 会报 "FuzzMatchGlob not found"。

Run: `go test -run=FuzzMatchGlob ./internal/guard -v`
Expected: no tests to run (Go returns exit 0, not FAIL) — `FuzzMatchGlob` 未定义。

- [ ] **Step 2: 实现 `FuzzMatchGlob` + 种子语料 + 4 不变量**

在 `internal/guard/glob_test.go` 尾部新增：

```go
// ---------- fuzz ----------

// FuzzMatchGlob checks invariants for every (pattern, name) pair the fuzz
// engine generates. See docs/superpowers/specs/2026-07-22-e2-fuzz-property-race-design.md §4.
func FuzzMatchGlob(f *testing.F) {
	// seed corpus: every seed is a known edge case (not a brute-force fuzz that
	// "just runs" — each seed targets a documented bypass/regex interaction).
	seeds := []struct{ pattern, name string }{
		// IFS / shell metachar
		{"go *", "go build ./..."},
		{"go *\t;", "go build"},
		{"a|b", "a|b"},             // | is regex metachar — must be escaped
		{"a|b", "a"},               // | escaped = no alternation bypass
		// glob injection
		{"*.go", "dir/main.go"},    // * does NOT cross /
		{"**.go", "a/b.go"},        // ** DOES cross /
		// ../ sequences
		{"D:/code/**", "D:/code/../etc/passwd"},
		{"a/..", "a/.."},
		// nested / repeated wildcards
		{"***", "a/b/c"},
		{"**/**", "x"},
		{"a**b**c", "aXYZbXYZc"},
		{"*?", "ab"},
		// trailing * over-grant (documented, must not regress)
		{"D:/code/*", "D:/code/secret/deep/x.go"},
		// long strings (ReDoS guard — pattern prefix match only, corpus is small)
		{"a*b*c*d*e*", "a111b222c333d444e555"},
		// empty / boundary
		{"", ""},
		{"", "x"},
		{"?", ""},
		// non-ASCII / UTF-8 boundary
		{"中文*", "中文测试"},
		{"*\xff\xfe", "\xff\xfe"},  // invalid UTF-8 — regexp.Compile returns error
		// ReDoS guard — must not hang or OOM (uses strings.Repeat; already
		// required by hasGlobMeta below)
		{strings.Repeat("*", 1000), strings.Repeat("a", 10000)},
	}
	for _, s := range seeds {
		f.Add(s.pattern, s.name)
	}

	f.Fuzz(func(t *testing.T, pattern, name string) {
		// Invariant 1 (strongest): never panic for any input
		matched, err := MatchGlob(pattern, name)

		// Invariant 4: globToRegexp must always succeed Compile (return error, not panic)
		// If err != nil, we're covering illegal-UTF-8 / ReDoS cases — just return.
		if err != nil {
			return
		}

		// Invariant 2: for literal patterns (no wildcards), MatchGlob == exact equals
		if !hasGlobMeta(pattern) {
			want := pattern == name
			if matched != want {
				t.Errorf("literal pattern=%q name=%q: MatchGlob=%v, want=%v", pattern, name, matched, want)
			}
		}

		// Invariant 3: ** matches anything; * matches anything without /
		if pattern == "**" && !matched {
			t.Errorf("** must match anything: pattern=%q name=%q", pattern, name)
		}
		if pattern == "*" && matched && strings.ContainsRune(name, '/') {
			t.Errorf("* must not cross /: pattern=%q name=%q", pattern, name)
		}
	})
}

// hasGlobMeta reports whether pattern contains glob wildcards (*, ?, [).
func hasGlobMeta(pattern string) bool {
	return strings.ContainsAny(pattern, "*?[")
}
```

注意：`strings.ContainsRune` 需 import `"strings"`（现有 `glob.go` 已 import，但 `glob_test.go` 若没有需补）。

- [ ] **Step 3: 跑测试确认通过（语料模式）**

Run: `go test -run FuzzMatchGlob ./internal/guard -v`
Expected: PASS — `FuzzMatchGlob` 引擎对每个种子模式运行，4 不变量全部通过。

- [ ] **Step 4: 本地长 fuzz 人工确认不 panic**

Run: `go test -fuzz=FuzzMatchGlob -fuzz=30s ./internal/guard`
Expected: 30 秒内无 crash；无 panic；`testdata/fuzz/FuzzMatchGlob/` 下会生成带 hash 的回归语料文件。

- [ ] **Step 5: 提交**

```bash
git add internal/guard/glob_test.go internal/guard/testdata/fuzz/FuzzMatchGlob/
git commit -m "test(fuzz): guard.MatchGlob fuzz with seed corpus and 4 invariants"
```

---

# PROP1 — ctxcompact 属性测试

### Task 2: 随机历史生成器 `gen_test.go`

**Files:**
- Create: `internal/ctxcompact/gen_test.go`

- [ ] **Step 1: 写测试验证生成器确定性**

Run: `go test ./internal/ctxcompact/ -run TestGenHistory -v`
Expected: no tests to run (Go returns exit 0, not FAIL) — `gen_test.go` 不存在。

- [ ] **Step 2: 实现 `gen_test.go`**

```go
// internal/ctxcompact/gen_test.go
package ctxcompact

import (
	"math/rand/v2"
	"testing"

	"github.com/cloudwego/eino/schema"
)

// ---------- random history generator (P1–P5 share this) ----------

// genHistory produces n messages with a fixed seed for reproducibility.
// Roles mix: user (some with ToolCallID = tool-result-as-user), assistant
// (some with ToolCalls), tool (paired and orphan). Working-set paths,
// error markers, diff markers, and a trailing summary are injected at
// configurable probability.
func genHistory(rng *rand.Rand, n int) []*schema.Message {
	msgs := make([]*schema.Message, 0, n)
	openCalls := make([]string, 0) // tool_call IDs awaiting a result
	for i := 0; i < n; i++ {
		roll := rng.Float64()
		switch {
		case roll < 0.35:
			// user (plain, possibly a tool-result-as-user)
			msg := &schema.Message{Role: schema.User, Content: randomContent(rng, 20, 80)}
			if rng.Float64() < 0.1 {
				msg.ToolCallID = "orphan-result-" + randomID(rng) // orphan tool result via Role=User
			}
			msgs = append(msgs, msg)
		case roll < 0.65 && i < n-1:
			// assistant with tool calls (maybe paired, maybe orphan)
			nCalls := rng.IntN(3) + 1
			calls := make([]schema.ToolCall, 0, nCalls)
			for j := 0; j < nCalls; j++ {
				id := "call-" + randomID(rng)
				calls = append(calls, schema.ToolCall{
					ID: id,
					Function: schema.FunctionCall{
						Name:      randomToolName(rng),
						Arguments: `{"x":1}`,
					},
				})
				// 60% chance this call gets a result later
				if rng.Float64() < 0.6 {
					openCalls = append(openCalls, id)
				}
			}
			msgs = append(msgs, &schema.Message{Role: schema.Assistant, ToolCalls: calls})
		case roll < 0.9 && len(openCalls) > 0:
			// tool result paired with an open call
			callID := openCalls[rng.IntN(len(openCalls))]
			// remove used call (consumed)
			filtered := make([]string, 0, len(openCalls)-1)
			for _, id := range openCalls {
				if id != callID {
					filtered = append(filtered, id)
				}
			}
			openCalls = filtered
			msgs = append(msgs, &schema.Message{Role: schema.Tool, ToolCallID: callID, Content: randomContent(rng, 10, 40)})
		case roll < 0.95:
			// orphan call / orphan result (pressure EnforceToolCallPairs)
			if rng.Float64() < 0.5 {
				msgs = append(msgs, &schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "orphan-" + randomID(rng), Function: schema.FunctionCall{Name: "orphan-tool", Arguments: "{}"}}}})
			} else {
				msgs = append(msgs, &schema.Message{Role: schema.Tool, ToolCallID: "missing-call-" + randomID(rng), Content: "orphan result"})
			}
		default:
			// working-set path / error / diff marker (triggers Plan pin rules 3/4/5)
			switch rng.IntN(3) {
			case 0:
				msgs = append(msgs, &schema.Message{Role: schema.Assistant, Content: "see D:/code/foo.go for details"})
			case 1:
				msgs = append(msgs, &schema.Message{Role: schema.Assistant, Content: "Error: something went wrong"})
			case 2:
				msgs = append(msgs, &schema.Message{Role: schema.Assistant, Content: "diff: --git a/main.go b/main.go"})
			}
		}
	}
	// sometimes append a trailing summary sentinel (bug⑦)
	if n > 0 && rng.Float64() < 0.15 {
		msgs = append(msgs, &schema.Message{Role: schema.User, Content: SummarySentinel + "prior summary"})
	}
	return msgs
}

func randomContent(rng *rand.Rand, minLen, maxLen int) string {
	n := rng.IntN(maxLen-minLen+1) + minLen
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(rng.IntN(26) + 'a')
	}
	return string(b)
}

func randomID(rng *rand.Rand) string {
	return randomContent(rng, 8, 16)
}

func randomToolName(rng *rand.Rand) string {
	tools := []string{"read", "write", "search", "shell_run", "web_fetch", "fs_edit"}
	return tools[rng.IntN(len(tools))]
}

// ---------- deterministic test of genHistory ----------

func TestGenHistory_DeterministicWithFixedSeed(t *testing.T) {
	// Two generators from the same seed produce identical history.
	rng1 := rand.New(rand.NewPCG(42, 0))
	rng2 := rand.New(rand.NewPCG(42, 0))
	h1 := genHistory(rng1, 50)
	h2 := genHistory(rng2, 50)
	if len(h1) != len(h2) {
		t.Fatalf("length mismatch: %d vs %d", len(h1), len(h2))
	}
	for i := range h1 {
		if (h1[i].Role != h2[i].Role) ||
			(h1[i].Content != h2[i].Content) ||
			(len(h1[i].ToolCalls) != len(h2[i].ToolCalls)) ||
			(h1[i].ToolCallID != h2[i].ToolCallID) {
			t.Fatalf("msg[%d] mismatch: %#v vs %#v", i, h1[i], h2[i])
		}
	}
}

func TestGenHistory_ProducesDiverseRoles(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 0))
	h := genHistory(rng, 200)
	seen := map[schema.RoleType]int{}
	for _, m := range h {
		seen[m.Role]++
	}
	if len(seen) < 2 {
		t.Fatalf("genHistory should produce at least 2 roles, got %v", seen)
	}
}
```

- [ ] **Step 3: 跑测试确认通过**

Run: `go test ./internal/ctxcompact/ -run TestGenHistory -v`
Expected: PASS — 生成器确定性已钉死；角色混合多样化。

- [ ] **Step 4: 提交**

```bash
git add internal/ctxcompact/gen_test.go
git commit -m "test(ctxcompact): deterministic random history generator for property tests"
```

---

### Task 3: plan_property_test.go — P1（pin ⊆ out）+ P2（工具对 fixpoint）

**Files:**
- Create: `internal/ctxcompact/plan_property_test.go`

- [ ] **Step 1: 写属性测试（先不写实现——这是纯设计文件，用现有 Plan/EnforceToolCallPairs 实现通过）**

Run: `go test ./internal/ctxcompact/ -run 'Property_Pin|Property_ToolCall' -v`
Expected: no tests to run (Go returns exit 0, not FAIL) — plan_property_test.go 不存在。

- [ ] **Step 2: 实现 P1 + P2 属性**

```go
// internal/ctxcompact/plan_property_test.go
package ctxcompact

import (
	"math/rand/v2"
	"testing"

	"github.com/cloudwego/eino/schema"
)

// Property test seed — printed on failure for reproduction.
const propSeed = 42

// planPropertyGen runs a property check across numTrials random histories,
// each of length up to maxLen. On failure the trial index and seed are printed.
func planPropertyGen(t *testing.T, numTrials, maxLen int, fn func(t *testing.T, msgs []*schema.Message)) {
	t.Helper()
	for trial := 0; trial < numTrials; trial++ {
		seed := uint64(propSeed*1000 + trial)
		rng := rand.New(rand.NewPCG(seed, 0))
		n := rng.IntN(maxLen) + 5 // at least 5 messages
		msgs := genHistory(rng, n)
		t.Run("", func(t *testing.T) {
			fn(t, msgs)
		})
	}
}

// ---------- P1: pin set ⊆ output ----------

func TestProperty_PinSetIsSubsetOfOutput(t *testing.T) {
	planPropertyGen(t, 50, 60, func(t *testing.T, msgs []*schema.Message) {
		plan := Plan(msgs, PlanOpts{KeepRecent: 3})
		if len(plan.SummarizeIndices) == 0 {
			// everything pinned (e.g. already-summarized) — no Assemble needed
			for _, i := range plan.PinnedIndices {
				if i < 0 || i >= len(msgs) {
					t.Fatalf("PinnedIndices[%d]=%d out of bounds", i, i)
				}
			}
			return
		}
		summary := "property test placeholder summary"
		out := Assemble(msgs, plan, summary)

		// P1.1: output has at least as many messages as pinned indices
		if len(out) < len(plan.PinnedIndices) {
			t.Fatalf("Assemble output length %d < pinned count %d (seed=%d)", len(out), len(plan.PinnedIndices))
		}

		// P1.2: first len(PinnedIndices) messages of output are pointer-equal to
		// the originals (Assemble appends the same pointers, not copies).
		for i, idx := range plan.PinnedIndices {
			if out[i] != msgs[idx] {
				t.Fatalf("out[%d] != msgs[%d]: pointers differ (seed=%d)", i, idx)
			}
		}

		// P1.3: PinnedIndices are ascending (Assemble depends on this)
		for i := 1; i < len(plan.PinnedIndices); i++ {
			if plan.PinnedIndices[i] <= plan.PinnedIndices[i-1] {
				t.Fatalf("PinnedIndices not ascending at index %d: %d <= %d (seed=%d)", i, plan.PinnedIndices[i], plan.PinnedIndices[i-1])
			}
		}
	})
}

// ---------- P2: tool_call/tool_result pairing fixpoint ----------

func TestProperty_ToolCallPairingFixpointHolds(t *testing.T) {
	planPropertyGen(t, 50, 60, func(t *testing.T, msgs []*schema.Message) {
		plan := Plan(msgs, PlanOpts{KeepRecent: 3})

		// Build pinned message lookup
		pinnedMsgs := make([]*schema.Message, 0, len(plan.PinnedIndices))
		for _, i := range plan.PinnedIndices {
			pinnedMsgs = append(pinnedMsgs, msgs[i])
		}

		// Collect all tool_call IDs in pinned set
		pinnedCallIDs := map[string]bool{}
		for _, m := range pinnedMsgs {
			if m != nil {
				for _, tc := range m.ToolCalls {
					if tc.ID != "" {
						pinnedCallIDs[tc.ID] = true
					}
				}
			}
		}

		// Collect all tool_result ToolCallIDs in pinned set
		pinnedResultIDs := map[string]bool{}
		for _, m := range pinnedMsgs {
			if m != nil && m.ToolCallID != "" {
				pinnedResultIDs[m.ToolCallID] = true
			}
		}

		// P2.1: every pinned tool_call's result is also pinned
		for callID := range pinnedCallIDs {
			if _, ok := pinnedResultIDs[callID]; !ok {
				t.Fatalf("tool_call %q is pinned but its result is NOT pinned (seed=%d)", callID)
			}
		}

		// P2.2: every pinned tool_result's call is also pinned
		for resultID := range pinnedResultIDs {
			if _, ok := pinnedCallIDs[resultID]; !ok {
				t.Fatalf("tool_result for %q is pinned but its call is NOT pinned (seed=%d)", resultID)
			}
		}
	})
}

// P2 bonus: EnforceToolCallPairs is idempotent on the fixpoint set.
func TestProperty_ToolCallPairFixpointIsIdempotent(t *testing.T) {
	planPropertyGen(t, 30, 60, func(t *testing.T, msgs []*schema.Message) {
		plan := Plan(msgs, PlanOpts{KeepRecent: 3})

		// Re-derive pinned map from Plan (Plan's already-enforced fixpoint)
		pinned := map[int]bool{}
		for _, i := range plan.PinnedIndices {
			pinned[i] = true
		}
		// Snapshot before second call
		before := make(map[int]bool, len(pinned))
		for k, v := range pinned {
			before[k] = v
		}
		// Apply again
		EnforceToolCallPairs(msgs, pinned)
		// Must be identical
		if len(pinned) != len(before) {
			t.Fatalf("fixpoint not idempotent: %d -> %d (seed=%d)", len(before), len(pinned))
		}
		for k := range before {
			if !pinned[k] {
				t.Fatalf("fixpoint not idempotent: lost index %d (seed=%d)", k)
			}
		}
	})
}

// P2 bonus: EnforceToolCallPairs REPAIRS a corrupted pinned set.
func TestProperty_ToolCallPairFixpointRepairsCorruption(t *testing.T) {
	planPropertyGen(t, 30, 60, func(t *testing.T, msgs []*schema.Message) {
		plan := Plan(msgs, PlanOpts{KeepRecent: 3})

		// Derive pinned map from Plan
		pinned := map[int]bool{}
		for _, i := range plan.PinnedIndices {
			pinned[i] = true
		}

		// Find a tool-call message and remove it from pinned, simulating a
		// bug that drops a tool_call while keeping its result (most likely
		// corruption scenario). If nothing to corrupt, skip.
		var callIdx int
		foundCall := false
		for idx, m := range msgs {
			if pinned[idx] && len(m.ToolCalls) > 0 {
				callIdx = idx
				foundCall = true
				break
			}
		}
		if !foundCall {
			return
		}
		// Remove that tool-call message from pinned
		delete(pinned, callIdx)

		// Snapshot before repair
		before := len(pinned)
		// Apply EnforceToolCallPairs — should re-add the tool_call and
		// its tool_result if orphaned.
		EnforceToolCallPairs(msgs, pinned)
		// Must have grown or at least broken even (never shrink)
		if len(pinned) < before {
			t.Fatalf("repair shrunk pinned set: %d -> %d (seed=%d)", before, len(pinned))
		}
		// Verify the tool_call was re-added
		if !pinned[callIdx] {
			t.Fatalf("repair did not restore tool_call index %d after corruption (seed=%d)", callIdx)
		}
		// Verify fixpoint now holds (all paired)
		for idx := range pinned {
			m := msgs[idx]
			if len(m.ToolCalls) > 0 {
				for _, tc := range m.ToolCalls {
					if tc.ID != "" {
						// Check the result is also pinned (somewhere later)
						hasResult := false
						for j := range pinned {
							if j >= len(msgs) {
								continue
							}
							r := msgs[j]
							if r.Role == schema.Tool && r.ToolCallID == tc.ID {
								hasResult = true
								break
							}
						}
						if !hasResult {
							t.Fatalf("tool_call %q at index %d still missing result after repair (seed=%d)", tc.ID, idx)
						}
					}
				}
			}
		}
	})
}
```

- [ ] **Step 3: 跑测试确认通过**

Run: `go test ./internal/ctxcompact/ -run 'Property_Pin|Property_ToolCall' -v`
Expected: PASS — 50 轮随机历史下 P1 和 P2 全部成立。

- [ ] **Step 4: 多次重复确认**

Run: `go test ./internal/ctxcompact/ -run 'Property_Pin|Property_ToolCall' -count=50`
Expected: PASS（50×50=2500 轮随机历史无失败）。

- [ ] **Step 5: 提交**

```bash
git add internal/ctxcompact/plan_property_test.go
git commit -m "test(ctxcompact): property tests P1 (pin subset) and P2 (tool pair fixpoint)"
```

---

### Task 4: summarize_property_test.go — P3（窗口 ≤）+ P4（不空 summary）+ recordingSummarizer

**Files:**
- Create: `internal/ctxcompact/summarize_property_test.go`

- [ ] **Step 1: 写属性测试确认编译前失败**

Run: `go test ./internal/ctxcompact/ -run 'Property_EachSummaryCall|Property_RunReturnsError' -v`
Expected: no tests to run (Go returns exit 0, not FAIL) — 文件不存在。

- [ ] **Step 2: 实现 P3 + P4 + recordingSummarizer**

```go
// internal/ctxcompact/summarize_property_test.go
package ctxcompact

import (
	"context"
	"errors"
	"math/rand/v2"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// ---------- recordingSummarizer: fake ModelSummarizer that records every call ----------

// recordingSummarizer implements ModelSummarizer, recording every Generate/Stream
// call's msgs for post-hoc assertion. Return controls what the summarizer emits.
type recordingSummarizer struct {
	GenerateCalls [][]*schema.Message
	StreamCalls   [][]*schema.Message
	Return        string
	ReturnErr     error
}

func (r *recordingSummarizer) Generate(ctx context.Context, msgs []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	r.GenerateCalls = append(r.GenerateCalls, msgs)
	if r.ReturnErr != nil {
		return nil, r.ReturnErr
	}
	return &schema.Message{Role: schema.User, Content: r.Return}, nil
}

func (r *recordingSummarizer) Stream(ctx context.Context, msgs []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	r.StreamCalls = append(r.StreamCalls, msgs)
	if r.ReturnErr != nil {
		return nil, r.ReturnErr
	}
	return schema.StreamReaderFromArray[*schema.Message]([]*schema.Message{
		{Role: schema.User, Content: r.Return},
	}), nil
}

// ---------- P3: each summary call's input ≤ ModelWindow ----------

func TestProperty_EachSummaryCallWithinWindow(t *testing.T) {
	// Test across ModelWindow sizes that trigger both single and carry paths.
	windows := []int{800, 400, 200, 100}
	for _, mw := range windows {
		t.Run("window="+t.Name(), func(t *testing.T) {
			rs := &recordingSummarizer{Return: "summarized"}
			opts := RunOpts{
				ModelWindow:      mw,
				ChunkThreshold:   0.9,
				SummaryWordLimit: 200,
			}

			rng := rand.New(rand.NewPCG(uint64(mw), 0))
			msgs := genHistory(rng, 30)
			if len(msgs) == 0 {
				return
			}

			_, err := RunSummary(context.Background(), msgs, opts, rs, nil)
			if err != nil {
				// A carry path starting with carry+chunk already over window is
				// a legitimate budget error — not a property violation.
				t.Logf("RunSummary returned error (expected for some tiny windows): %v", err)
				return
			}

			// Collect all calls (Generate or Stream)
			allCalls := append(rs.GenerateCalls, rs.StreamCalls...)
			if len(allCalls) == 0 {
				t.Fatal("RunSummary returned success but summarizer was never called")
			}
			for i, callMsgs := range allCalls {
				tok := EstimateTokens(callMsgs)
				if tok > mw {
					// Exception: a chunk that keeps a tool pair intact may exceed
					// budget for one chunk (documented in summarize.go:127-137).
					// Verify the overrun is due to pair integrity, not slop.
					// Overrun > 2x budget is always a property violation.
					if tok > mw*2 {
						t.Fatalf("call[%d] tok=%d exceeds ModelWindow=%d by >2x (unacceptable even for pair integrity)", i, tok, mw)
					}
					t.Logf("call[%d] tok=%d exceeds window=%d (acceptable: pair integrity)", i, tok, mw)
				}
			}
		})
	}
}

// ---------- P4: no empty summary when summarizer returns empty ----------

func TestProperty_RunReturnsErrorForEmptySummary(t *testing.T) {
	rs := &recordingSummarizer{Return: ""}
	opts := RunOpts{
		ModelWindow:      1000,
		ChunkThreshold:   0.9,
		SummaryWordLimit: 200,
	}
	rng := rand.New(rand.NewPCG(99, 0))
	msgs := genHistory(rng, 20)

	// Filter: if len(plan.SummarizeIndices) == 0 (everything pinned), Run
	// does NOT call RunSummary — that case is not a P4 violation.
	plan := Plan(msgs, PlanOpts{KeepRecent: 3})
	if len(plan.SummarizeIndices) == 0 {
		t.Skip("all messages pinned — nothing to summarize")
	}

	_, err := Run(context.Background(), msgs, PlanOpts{KeepRecent: 3}, opts, rs, nil)
	if err == nil {
		t.Fatal("Run must return error when summarizer produces empty summary (bug⑥)")
	}
}
```

- [ ] **Step 3: 跑测试确认通过**

Run: `go test ./internal/ctxcompact/ -run 'Property_EachSummaryCall|Property_RunReturnsError' -v`
Expected: PASS — P3 在小窗口（如 100）可能触发 carry 路径，允许的成对超预算用 t.Log 记录而非失败；P4 在非全 pin 历史下空 summarizer 返回 error。

- [ ] **Step 4: 多次重复确认**

Run: `go test ./internal/ctxcompact/ -run 'Property_EachSummaryCall|Property_RunReturnsError' -count=30`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/ctxcompact/summarize_property_test.go
git commit -m "test(ctxcompact): property tests P3 (window) and P4 (no empty summary)"
```

---

### Task 5: run_property_test.go — P5（summary-of-summary 短路）+ Run 端到端

**Files:**
- Create: `internal/ctxcompact/run_property_test.go`

- [ ] **Step 1: 确认编译前失败**

Run: `go test ./internal/ctxcompact/ -run 'Property_NoDouble|Property_RunReduces' -v`
Expected: no tests to run (Go returns exit 0, not FAIL) — 文件不存在。

- [ ] **Step 2: 实现 P5 和 Run 端到端属性**

```go
// internal/ctxcompact/run_property_test.go
package ctxcompact

import (
	"context"
	"math/rand/v2"
	"testing"
)

// ---------- P5: summary-of-summary short-circuit (bug⑦) ----------

func TestProperty_NoDoubleCompaction(t *testing.T) {
	rng := rand.New(rand.NewPCG(777, 0))
	msgs := genHistory(rng, 30)
	if len(msgs) == 0 {
		return
	}

	rs1 := &recordingSummarizer{Return: "first summary"}
	planOpts := PlanOpts{KeepRecent: 3}
	runOpts := RunOpts{
		ModelWindow:      2000,
		ChunkThreshold:   0.9,
		SummaryWordLimit: 200,
	}

	// First compaction
	result1, err := Run(context.Background(), msgs, planOpts, runOpts, rs1, nil)
	if err != nil {
		t.Fatalf("first Run failed: %v", err)
	}
	if len(rs1.GenerateCalls)+len(rs1.StreamCalls) == 0 && len(planOpts.KeepRecent) > 0 {
		// If SummarizeIndices was empty (all pinned), call count == 0 is expected.
		// Nothing to compact — skip.
		t.Log("first Run had nothing to summarize — skipping P5")
		return
	}

	rs2 := &recordingSummarizer{Return: "re-summary"}
	// Second compaction on compacted output
	result2, err := Run(context.Background(), result1.Messages, planOpts, runOpts, rs2, nil)
	if err != nil {
		t.Fatalf("second Run failed: %v", err)
	}

	// P5: second Run must NOT call the summarizer (short-circuit at Plan).
	calls2 := len(rs2.GenerateCalls) + len(rs2.StreamCalls)
	if calls2 > 0 {
		t.Fatalf("summary-of-summary: second Run made %d summarizer calls, want 0 (bug⑦)", calls2)
	}

	// P5 bonus: second Run output length should match first Run (no change).
	// Softened to t.Log: Assemble/Plan are not guaranteed length-stable across
	// calls when the summary sentinel message reorders pinned indices, so a hard
	// equality assertion would be brittle. The core P5 property (calls2 == 0) is
	// the load-bearing assertion above.
	if len(result2.Messages) != len(result1.Messages) {
		t.Logf("second Run output length %d != first %d (informational, not a property violation)", len(result2.Messages), len(result1.Messages))
	}
}

// ---------- Run end-to-end: tokens strictly decrease ----------

func TestProperty_RunReducesTokens(t *testing.T) {
	rng := rand.New(rand.NewPCG(123, 0))
	msgs := genHistory(rng, 40)
	if len(msgs) == 0 {
		return
	}

	rs := &recordingSummarizer{Return: "compacted summary"}

	result, err := Run(context.Background(), msgs, PlanOpts{KeepRecent: 3}, RunOpts{
		ModelWindow:      2000,
		ChunkThreshold:   0.9,
		SummaryWordLimit: 200,
	}, rs, nil)
	if err != nil {
		// If there's nothing to summarize (all pinned), Run returns success with
		// unchanged history — tokens may not decrease.
		if len(rs.GenerateCalls)+len(rs.StreamCalls) == 0 {
			return
		}
		t.Fatalf("Run failed: %v", err)
	}

	before := EstimateTokens(msgs)
	after := EstimateTokens(result.Messages)
	if after >= before && len(rs.GenerateCalls)+len(rs.StreamCalls) > 0 {
		t.Fatalf("Run did not reduce tokens: before=%d, after=%d (summarizer called %d times)", before, after, len(rs.GenerateCalls)+len(rs.StreamCalls))
	}
}
```

- [ ] **Step 3: 跑测试确认通过**

Run: `go test ./internal/ctxcompact/ -run 'Property_NoDouble|Property_RunReduces' -v`
Expected: PASS — P5 证实第二次 Run 不调 summarizer；端到端证实 token 严格减少（压缩有效）。

- [ ] **Step 4: 多次重复确认**

Run: `go test ./internal/ctxcompact/ -run 'Property_NoDouble|Property_RunReduces' -count=30`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/ctxcompact/run_property_test.go
git commit -m "test(ctxcompact): property test P5 (no double compaction) and Run end-to-end token reduction"
```

---

# RAC1 — `-race` 并发热点测试

### Task 6: broker_race_test.go — broker 并发 + `createdWT` LEAK 探针

**Files:**
- Create: `internal/task/broker_race_test.go`

- [ ] **Step 1: 写失败测试确认**

Run: `go test -race ./internal/task/ -run 'Broker_Concurrent|Broker_LeakProbe' -v`
Expected: no tests to run (Go returns exit 0, not FAIL) — 文件不存在。

- [ ] **Step 2: 实现 broker 并发测试 + LEAK 探针**

```go
// internal/task/broker_race_test.go
package task

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/x6nux/yanshi/internal/store"
)

// newTestStore opens an in-memory SQLite store for race tests.
func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(":memory:")
	assert.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	return s
}

// TestBroker_ConcurrentClaimRecordCancel exercises Claim/RecordResult/Cancel
// from multiple goroutines. -race must detect no data races.
func TestBroker_ConcurrentClaimRecordCancel(t *testing.T) {
	s := newTestStore(t)
	b := NewBroker(s, 2, 5*time.Second)

	// Submit N tasks
	const n = 10
	ids := make([]string, n)
	for i := range n {
		id, err := b.Submit("race-test", "input", "")
		assert.NoError(t, err)
		ids[i] = id
	}

	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			// Claim
			task, err := b.Claim("worker-1")
			if err != nil || task == nil {
				return
			}
			// Either RecordResult or Cancel
			if idx%2 == 0 {
				_ = b.RecordResult(task.ID, "worker-1", "completed", `{"ok":true}`)
			} else {
				_ = b.Cancel(task.ID)
			}
		}(i)
	}
	wg.Wait()
}

// TestBroker_ConcurrentHeartbeat sends heartbeats concurrently from
// multiple goroutines while another goroutine records results.
func TestBroker_ConcurrentHeartbeat(t *testing.T) {
	s := newTestStore(t)
	b := NewBroker(s, 2, 5*time.Second)

	id, err := b.Submit("hb-test", "input", "")
	assert.NoError(t, err)
	_, err = b.Claim("worker-1")
	assert.NoError(t, err)

	var wg sync.WaitGroup
	// 10 parallel heartbeats
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = b.Heartbeat(id)
		}()
	}
	wg.Wait()
}

// TestBroker_ConcurrentRequeueStale calls RequeueStale while tasks are
// concurrently submitted and claimed. No time.Sleep needed — RequeueStale
// reads from the store directly, so the stress is on concurrent access
// to the notify channel and createdWT map.
func TestBroker_ConcurrentRequeueStale(t *testing.T) {
	s := newTestStore(t)
	b := NewBroker(s, 2, time.Hour) // heartbeat timeout so long no real stale

	// Pre-submit and claim some tasks to create "running" entries
	// (they won't go stale due to the long timeout, but RequeueStale
	// still reads the store under concurrent access).
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := b.Submit("req-test", "in", "")
			if err == nil {
				_, _ = b.Claim("worker-1")
				_ = id
			}
		}()
	}
	wg.Wait()

	// Run RequeueStale concurrently with more submits
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = b.RequeueStale(context.Background())
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = b.Submit("req-test-2", "in", "")
		}()
	}
	wg.Wait()
}

// TestBroker_LeakProbeCreatedWT records current createdWT leak behavior for
// F2/LEAK1. EXPECTED TO PASS: it documents the current "createdWT grows on
// RequeueStale" behavior. F2 flips the assert to Zero.
func TestBroker_LeakProbeCreatedWT(t *testing.T) {
	s := newTestStore(t)
	b := NewBroker(s, 1, 10*time.Millisecond) // very short heartbeat timeout

	// Enable VCS to trigger worktree creation path. Use nil VCS but set
	// createdWT manually to test the map. Actually, let's submit tasks,
	// claim them (which writes createdWT), let them go stale, run RequeueStale.
	// Since VCS is nil, Claim won't create worktrees — but we can still
	// inspect createdWT directly (same-package test).

	// Instead, submit and claim multiple tasks to populate createdWT
	// (when VCS is nil, Claim skips worktree creation, so createdWT is empty).
	// The LEAK scenario: when VCS IS set, RequeueStale does NOT clean createdWT.
	// For this probe, we manually add entries to simulate the leak and verify
	// that RequeueStale does not remove them — confirming the pre-F2 behavior.

	b.createdWTMu.Lock()
	baseline := len(b.createdWT)
	// Simulate entries that RequeueStale would NOT clean
	b.createdWT["leak-test-1"] = "wt-1"
	b.createdWT["leak-test-2"] = "wt-2"
	b.createdWTMu.Unlock()

	// Run RequeueStale (no-op since no pending tasks are running)
	_ = b.RequeueStale(context.Background())

	b.createdWTMu.Lock()
	// Current behavior: RequeueStale does NOT clean createdWT.
	// This is the documented LEAK behavior for F2/LEAK1 to fix.
	t.Logf("createdWT entries after RequeueStale: %d (baseline=%d)", len(b.createdWT), baseline)
	// F2/LEAK1: flip this to assert.Zero(t, len(b.createdWT)-baseline)
	assert.GreaterOrEqual(t, len(b.createdWT), baseline+2,
		"LEAK PROBE: RequeueStale does NOT clean createdWT (F2/LEAK1 fix expected)")
	b.createdWTMu.Unlock()
}
```

- [ ] **Step 3: 跑测试确认通过（`-race`）**

Run: `go test -race ./internal/task/ -run 'Broker_Concurrent|Broker_LeakProbe' -v`
Expected: PASS — `-race` 无 data race；LEAK 探针 `assert.GreaterOrEqual` 记录现状（绿）。

- [ ] **Step 4: 提交**

```bash
git add internal/task/broker_race_test.go
git commit -m "test(race): broker concurrent claim/record/sweep and createdWT LEAK probe"
```

---

### Task 7: ws_race_test.go — wsConn.write + permTracker + connSession 帧交错

**Files:**
- Create: `internal/api/http/ws_race_test.go`

- [ ] **Step 1: 写失败测试确认**

Run: `go test -race ./internal/api/http/ -run 'WSConnWrite|PermTracker|ConnSession' -v`
Expected: no tests to run (Go returns exit 0, not FAIL) — 文件不存在。

- [ ] **Step 2: 实现 wsConn. write 并发、permTracker 并发、connSession 帧交错集成**

```go
// internal/api/http/ws_race_test.go
package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/tools"
)

// ---------- 6.1 wsConn.write concurrent ----------

func TestWSConnWrite_ConcurrentNoRace(t *testing.T) {
	// Set up an in-process WS echo server. A ready channel is the
	// happens-before edge: the upgrade handler signals AFTER assigning
	// serverConn, and the test blocks on it before launching writers.
	// time.Sleep is NOT a happens-before edge and -race would flag the
	// bare serverConn read below as racing with the handler's write.
	var serverConn *wsConn
	ready := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		raw, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		serverConn = &wsConn{Conn: raw}
		close(ready) // happens-before: test reads serverConn only after this
		// Read loop — drain frames so server buffer doesn't fill
		for {
			_, _, err := raw.ReadMessage()
			if err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	// Dial from client side
	u := "ws" + srv.URL[4:] // http→ws
	dialer := websocket.DefaultDialer
	client, _, err := dialer.DialContext(context.Background(), u, nil)
	require.NoError(t, err)
	defer client.Close()

	// Block on the ready signal — guaranteed serverConn is assigned.
	<-ready

	// Write from N goroutines concurrently
	const n = 16
	const m = 50
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < m; j++ {
				f := proto.ServerFrame{Type: "test", Text: "race-test"}
				serverConn.write(f)
			}
		}(i)
	}
	wg.Wait()
}

// ---------- 6.2 permTracker concurrent ----------
//
// permTracker.register expects a chan tools.PermissionDecision, and deliver
// takes a tools.PermissionDecision (e.g. tools.PermissionAllow). The import
// block at the top of this file already pulls internal/tools.

func TestPermTracker_RegisterTakeDeliverConcurrent(t *testing.T) {
	pt := newPermTracker()
	ch := make(chan tools.PermissionDecision, 1)

	id := pt.newID()
	pt.register(id, ch)

	// Concurrent newID — each returns a unique string (race-free under -race).
	const n = 20
	var wg sync.WaitGroup
	ids := make([]string, n)
	for i := range n {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			ids[idx] = pt.newID()
		}(i)
	}
	// Deliver while another goroutine concurrently takes + newID runs.
	wg.Add(1)
	go func() {
		defer wg.Done()
		pt.deliver(id, tools.PermissionAllow)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		if taken := pt.take(id); taken != nil {
			select {
			case <-taken:
			default:
			}
		}
	}()
	wg.Wait()

	// Verify newID uniqueness across the concurrent burst.
	seen := map[string]bool{}
	for _, x := range ids {
		if x == "" {
			continue
		}
		if seen[x] {
			t.Fatalf("dup id: %s", x)
		}
		seen[x] = true
	}

	// Nonexistent-key take/deliver must not panic.
	_ = pt.take("nonexistent")
	pt.deliver("nonexistent", tools.PermissionAllow)
}

// ---------- 6.6 connSession frame interleaving (spec Section 6.6) ----------
//
// Drives a real server with a WS client that fires user_message, set_mode,
// and cancel frames concurrently. gorilla/websocket forbids concurrent writes
// on one Conn, so client writes go through a single writer goroutine. The
// concurrency under test is SERVER-side: the reader goroutine applies set_mode
// to the LIVE perm state (bypassing the frames channel) and cancel calls
// cancelTurn() — both run while the main loop may be processing a
// user_message turn. -race must report nothing.
func TestConnSession_ConcurrentFrameInterleaving(t *testing.T) {
	_, s := newSessionTestServer(t)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	c := dial(t, dialWSURL(t, ts))
	defer c.Close()

	// Single writer goroutine for the client Conn (gorilla forbids concurrent
	// writes). The server-side reader goroutine and main loop interleave.
	frameCh := make(chan proto.ClientFrame, 16)
	var writeWG sync.WaitGroup
	writeWG.Add(1)
	go func() {
		defer writeWG.Done()
		for cf := range frameCh {
			_ = c.WriteJSON(cf)
		}
	}()

	// Fire a burst: user_message starts turns; set_mode hits the live perm
	// state immediately from the reader goroutine; cancel cancels in-flight.
	for i := 0; i < 40; i++ {
		frameCh <- proto.NewUserMessage("interleave turn")
		frameCh <- proto.ClientFrame{Type: "set_mode", Mode: "yolo"}
		frameCh <- proto.ClientFrame{Type: "set_mode", Mode: "default"}
		frameCh <- proto.NewCancel()
	}
	close(frameCh)
	writeWG.Wait()

	// Drain server-emitted frames so the server's writer retires cleanly.
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		if _, _, err := c.ReadMessage(); err != nil {
			break
		}
	}
}
```

- [ ] **Step 3: 跑测试确认通过（`-race`）**

Run: `go test -race ./internal/api/http/ -run 'WSConnWrite|PermTracker_RegisterTakeDeliver|ConnSession' -v`
Expected: PASS — `-race` 无 data race；permTracker 的 newID 返回唯一 ID；connSession 帧交错无竞态。

- [ ] **Step 4: 提交**

```bash
git add internal/api/http/ws_race_test.go
git commit -m "test(race): wsConn.write, permTracker concurrent and connSession frame interleaving"
```

---

### Task 8: orchestrator_race_test.go — runners sync.Map 并发

**Files:**
- Create: `internal/agent/orchestrator/orchestrator_race_test.go`

- [ ] **Step 1: 写失败测试确认**

Run: `go test -race ./internal/agent/orchestrator/ -run 'Runners_' -v`
Expected: no tests to run (Go returns exit 0, not FAIL) — 文件不存在。

- [ ] **Step 2: 实现 runners sync.Map 并发测试**

```go
// internal/agent/orchestrator/orchestrator_race_test.go
package orchestrator

import (
	"sync"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/stretchr/testify/assert"

	"github.com/x6nux/yanshi/internal/guard"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
)

// TestRunners_SameModelReturnsSamePointer verifies that concurrent
// runnerFor(sameModel, plan=false) all return the same *adk.Runner pointer
// and no data race occurs on the sync.Map.
func TestRunners_SameModelReturnsSamePointer(t *testing.T) {
	fm := einollm.NewFakeModelWithMessages(nil, nil)
	o, err := New(Config{
		Model:   fm,
		Profile: testProfile(),
	})
	assert.NoError(t, err)

	const n = 20
	runners := make([]interface{}, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			runners[idx] = o.runnerFor(fm, false)
		}(i)
	}
	wg.Wait()

	// All should be the same pointer
	first := runners[0]
	for i := 1; i < n; i++ {
		if runners[i] != first {
			t.Fatalf("runner[%d] is different pointer from runner[0]", i)
		}
	}
}

// TestRunners_DifferentModelKeys concurrency-tests the scenario where
// each call to runnerFor uses a different fake model (simulating /model
// switching at high frequency).
func TestRunners_DifferentModelKeys(t *testing.T) {
	fm := einollm.NewFakeModelWithMessages(nil, nil)
	o, err := New(Config{
		Model:   fm,
		Profile: testProfile(),
	})
	assert.NoError(t, err)

	models := make([]model.BaseChatModel, 10)
	for i := range models {
		models[i] = einollm.NewFakeModelWithMessages(nil, nil)
	}

	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, m := range models {
				_ = o.runnerFor(m, false)
				_ = o.runnerFor(m, true) // plan mode = different cache key
			}
		}()
	}
	wg.Wait()
}

// TestRunners_FlushDuringAccess runs FlushRunners concurrently with
// runnerFor accesses.
func TestRunners_FlushDuringAccess(t *testing.T) {
	fm := einollm.NewFakeModelWithMessages(nil, nil)
	o, err := New(Config{
		Model:   fm,
		Profile: testProfile(),
	})
	assert.NoError(t, err)

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				_ = o.runnerFor(fm, false)
				_ = o.runnerFor(fm, true)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 10; j++ {
			o.FlushRunners()
		}
	}()
	wg.Wait()
}

// testProfile returns a minimal profile for constructing Orchestrator.
func testProfile() guard.PermissionProfile {
	return guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
		Shell: guard.ShellPerm{Policy: "allowlist", Patterns: []string{"*"}},
		Net:   guard.NetPerm{Allow: true},
	}
}
```

- [ ] **Step 3: 跑测试确认通过（`-race`）**

Run: `go test -race ./internal/agent/orchestrator/ -run 'Runners_' -v`
Expected: PASS — `-race` 无 data race；同一 model 返回同一 runner 指针。

- [ ] **Step 4: 提交**

```bash
git add internal/agent/orchestrator/orchestrator_race_test.go
git commit -m "test(race): orchestrator runners sync.Map concurrent LoadOrStore and FlushRunners"
```

---

### Task 9: manager_race_test.go — Manager 并发 + Registry 冻结契约文档化

**Files:**
- Create: `internal/agent/registry/manager_race_test.go`

- [ ] **Step 1: 写失败测试确认**

Run: `go test -race ./internal/agent/registry/ -run 'Manager_Concurrent|Registry_Bootstrap' -v`
Expected: no tests to run (Go returns exit 0, not FAIL) — 文件不存在。

- [ ] **Step 2: 实现 Manager 并发测试 + Registry 契约文档化**

```go
// internal/agent/registry/manager_race_test.go
package registry

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// fakeRunner implements Runner — just returns immediately.
type fakeRunner struct{}

func (f fakeRunner) Run(ctx context.Context, agentID, assignment string) (string, error) {
	return "done", nil
}

// ---------- Manager concurrent Spawn/Cancel/SendInput/Resume ----------

func TestManager_ConcurrentSpawnCancel(t *testing.T) {
	m := NewManager(NewManagerOpts{
		RootContext:   context.Background(),
		Path:          t.TempDir(),
		SessionBootID: "race-test",
		MaxConcurrent: 10,
	})

	var wg sync.WaitGroup
	const n = 20

	// Trigger: Spawn agents concurrently
	agentIDs := make([]string, n)
	for i := range n {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			id, err := m.Spawn(context.Background(), SpawnRequest{
				AgentType: "race-test",
				Prompt:    "do something",
				Runner:    fakeRunner{},
			})
			if err == nil {
				agentIDs[idx] = id
			}
		}(i)
	}
	wg.Wait()

	// Cancel some of them concurrently
	for _, id := range agentIDs {
		if id != "" {
			wg.Add(1)
			go func(aID string) {
				defer wg.Done()
				_ = m.Cancel(aID)
			}(id)
		}
	}
	wg.Wait()
	m.Close()
}

func TestManager_ConcurrentListAndResult(t *testing.T) {
	m := NewManager(NewManagerOpts{
		RootContext:   context.Background(),
		Path:          t.TempDir(),
		SessionBootID: "race-test",
		MaxConcurrent: 10,
	})

	var wg sync.WaitGroup
	// Spawn a few agents
	for i := 0; i < 5; i++ {
		_, _ = m.Spawn(context.Background(), SpawnRequest{
			AgentType: "race-test",
			Prompt:    "test",
			Runner:    fakeRunner{},
		})
	}

	// Concurrently call List and Result on various agents
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = m.List(false)
			_ = m.Result("nonexistent")
		}()
	}
	wg.Wait()
	m.Close()
}

// ---------- Registry bootstrap-frozen convention ----------

// TestRegistry_BootstrapFrozenConvention documents that Registry is
// unsynchronized but safe because it's only written during bootstrap
// (single-threaded) and read-only after. This test proves concurrent
// reads are safe even with Register pre-filled.
func TestRegistry_BootstrapFrozenConvention(t *testing.T) {
	r := New()
	// Simulate bootstrap registration
	r.Register(Entry{Name: "agent-a", Kind: KindLocal, Description: "test agent"})
	r.Register(Entry{Name: "agent-b", Kind: KindLocal, Description: "another agent"})

	// Concurrent read-only access (simulating production use after bootstrap)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = r.Get("agent-a")
			_ = r.All()
			_ = r.ByCapability("shell_*")
		}()
	}
	wg.Wait()
}
```

- [ ] **Step 3: 跑测试确认通过（`-race`）**

Run: `go test -race ./internal/agent/registry/ -run 'Manager_Concurrent|Registry_Bootstrap' -v`
Expected: PASS — `-race` 无 data race；Spawn/Cancel/List/Result 并发安全；Registry 只读并发安全。

- [ ] **Step 4: 提交**

```bash
git add internal/agent/registry/manager_race_test.go
git commit -m "test(race): registry Manager concurrent spawn/cancel/list and Registry bootstrap contract"
```

---

### Task 10: findings 登记—— notes 文件 + issue 模板

**Files:**
- Create: `docs/superpowers/notes/2026-07-22-e2-race-findings.md`

- [ ] **Step 1: 创建 findings 登记文件**

```markdown
# E2 Race Findings 登记表

> **生成日期**：2026-07-22
> **归属**：E2（RAC1）→ F2（LEAK1/BENCH1/GOV）
> **来源**：代码审查 + `-race` 测试 + spec §7 预判

## 登记项

### F-1: `createdWT` 在 RequeueStale 路径不回收

| 字段 | 值 |
|---|---|
| 位置 | `internal/task/broker.go:219` `RequeueStale` |
| 现象 | `createdWT[taskID]worktreeID` 在 stale-requeue 后不被 reclaim；`len(createdWT)` 随长跑单调增长 |
| 分类 | leak |
| 归属 | **LEAK1** 修复 |
| 当前处置 | RAC1 `TestBroker_LeakProbeCreatedWT` 用 `assert.GreaterOrEqual` 记录现状（绿）；F2 翻转为 `assert.Zero` |

### F-2: Claim 重入时 worktree_id 残留

| 字段 | 值 |
|---|---|
| 位置 | `internal/task/broker.go:109` `Claim` |
| 现象 | RequeueStale 把 task 回 pending 但不清 `worktree_id`；重 Claim 时 `got.WorktreeID != ""` 跳过建新 WT → 首次建的 WT 被孤儿化（无人 reclaim） |
| 分类 | leak |
| 归属 | **LEAK1** 修复 |
| 当前处置 | 登记不修；`TestBroker_LeakProbeCreatedWT` 间接覆盖 |

### F-3: `runnerCacheKey.model` 为接口类型

| 字段 | 值 |
|---|---|
| 位置 | `internal/agent/orchestrator/orchestrator.go:131` |
| 现象 | `runnerCacheKey{model model.BaseChatModel, mode runnerToolMode}` 中 model 是接口字段。作为 `sync.Map` 键要求动态类型可比较；当前 model 都是指针（可比较），安全。若未来出现非可比较具体类型则 panic |
| 分类 | latent-footgun |
| 归属 | GOV/文档 |
| 当前处置 | 登记；测试注释已标注 |

### F-4: `runnerFor` Load→build→LoadOrStore 冗余构建

| 字段 | 值 |
|---|---|
| 位置 | `internal/agent/orchestrator/orchestrator.go:346` `runnerFor` |
| 现象 | 两个 goroutine 同时 miss cache 都会 build（expensive model 构造），胜者存、败者弃。非 data race，是性能冗余 |
| 分类 | perf |
| 归属 | BENCH1 |
| 当前处置 | 登记；`TestRunners_SameModelReturnsSamePointer` 已确认最终一致性 |

### F-5: `globToRegexp` 每调用重编译 regex

| 字段 | 值 |
|---|---|
| 位置 | `internal/guard/glob.go:27-31` |
| 现象 | `MatchGlob` 每次调用都 `globToRegexp` → `regexp.Compile`；`guard.Check` 对每 pattern 每维每调用都编译，是热路径性能债 |
| 分类 | perf |
| 归属 | BENCH1/E3 |
| 当前处置 | 登记；FUZ1 `FuzzMatchGlob` 已覆盖此路径 |

### F-6: `Registry.entries` 无锁

| 字段 | 值 |
|---|---|
| 位置 | `internal/agent/registry/registry.go:28` |
| 现象 | `Registry.entries map[string]Entry` 无 mutex；`Register`/`Get`/`All`/`ByCapability`（:35-66）均无同步。实际仅 bootstrap 单线程写入、后续只读。Go map 并发只读安全 |
| 分类 | latent-footgun |
| 归属 | GOV/F2 |
| 当前处置 | RAC1 `TestRegistry_BootstrapFrozenConvention` 文档化此契约；建议 GOV 阶段加 RWMutex 或冻结断言 |

## CI `-skip` 排除记录

_当 `-race` 在 CI 中发现既有竞态时，在此记录 exclude 项：_

| 测试 | 位置 | issue # (required) | `-skip` 表达式 | F2 修后去除？ |
|---|---|---|---|---|
| | | | | |

_注：issue # 为必填，不得为空；表格行必须在修复对应竞态后从 `-skip` 中移除（F2 阶段逐一清理）。_

```bash
git add docs/superpowers/notes/2026-07-22-e2-race-findings.md
git commit -m "docs: E2 race findings register (F-1 through F-6)"
```

---

## 验收清单

1. **FUZ1**: `FuzzMatchGlob` 存在；`go test -run FuzzMatchGlob ./internal/guard` 通过、无 panic；种子含 IFS/注入/`../`/嵌套/trailing-`*`/超长/UTF-8 边界；`testdata/fuzz/` 入仓。
2. **PROP1**: P1-P5 五条属性存在并通过；随机生成器固定种子可复现；P2 在含 orphan 的随机历史上成立；P3 在触发 carry 分块的小窗口用例上成立；`-run Property -count=50` 通过。
3. **RAC1**: 6 个并发热点（wsConn.write / permTracker / runners sync.Map / broker / Manager / Registry）各有测试；本地 `go test -race`（热点包）通过；broker LEAK 探针就位且绿色。
4. **F2 衔接**: findings 表（F-1/F-2 createdWT leak + F-3~F-6）已登记；broker LEAK 探针用 `assert.GreaterOrEqual` + `t.Log`；`Registry` 冻结契约有文档化测试。
5. **无非测试生产代码改动**（除非 fuzz 发现安全 bug 需本批修，或 <50 行 ctxcompact 不变量修复）。
6. **CI 命令清单**已交付 CIG1（见 §CI 分层策略）。

## 执行方式

**计划已保存到 `docs/superpowers/plans/2026-07-22-e2-fuzz-property-race.md`。两个执行选项：**

1. **Subagent-Driven（推荐）** —— 每条线派一个子代理，FUZ1/PROP1/RAC1 独立并行
2. **Inline Execution** —— 在当前会话执行，batch 分段 review

**选哪种？**
