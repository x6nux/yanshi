# G03+G02: T0–T4 难度路由接通 + Goal Token 预算累计 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 修掉 `internal/agent/goalloop` 的两个缺陷——(G03) `autocode goal -tier t0..t2` 当前只打印一行就 `return`、从不进入执行路径；(G02) `Budget.SpentTokens` 从不随 LLM 调用累计，令 token 预算形同虚设。修完后：每个 tier（T0–T4）都产生真实结果，auto/强制 tier 可测，预算耗尽可靠停止并把原因持久化。

**Architecture:**
- **G02（统一 usage accounting）**：新增 `UsageSink`——一个线程安全的累加器，被 planner / intent / quality / tierer 共享。每个组件在 `model.Generate` 之后把 `resp.ResponseMeta.Usage` `Add` 进 sink（每次独立调用累加一次，天然避免父子重复计算）。`Loop` 把预算判定从静态的 `Budget.SpentTokens` 切到 `sink.Snapshot().Total()`；耗尽时返回带 `StopReason` 的 `Decision`，并由 `cmd/autocode` 把 run 记录写进现有 `kv` 表（复用、零迁移）。`Budget.SpentTokens` 字段保留作为无 sink 时的回退（既有 `TestLoop_BudgetExceeded` 不破坏）。
- **G03（接通 tier 路由）**：在 `tier.go` 加纯函数 `Tier.Path()` / `Tier.SkillName()` / `EvaluatorsForTier` / `ResolveTierFlag` / `EscalationHint`，把"tier → 执行路径"的决策从 `cmd/autocode/main.go` 下沉到可单测的 goalloop 包。`main.go` 删掉静默 `return` 分支：real path 上 T0–T2（`Path()=="lightweight"`）走一次 `app.Orch.Query`（带 tier 技能体的 orchestrator 单轮，产出真实文件编辑），T3–T4（`Path()=="loop"`）走既有 goalloop；`auto` 经 `RuleTierer` 选 tier 后同样分派。`--fake-model` 路径不动（保持确定性两轮 demo）。tier 耗尽且 `<T4` 时 `Decision.StopReason="escalate"` 并带升级提示——升级不再静默退出。

**Tech Stack:** Go stdlib；`github.com/cloudwego/eino/components/model`、`github.com/cloudwego/eino/schema`（既有）；`github.com/stretchr/testify`（既有测试）；现有 `internal/agent/goalloop`、`internal/bootstrap`（`App.Orch`/`App.Skills`/`App.Store`）、`internal/store`（`KVSet`）。

**不变性（回归约束）：** `--fake-model` 两轮 demo 必须字节级保持可用（`TestLoop_FailOnceThenPass` 不破坏）；既有 `TestLoop_BudgetExceeded` / `TestLoop_MaxIterationsExhausted` 断言的 Summary 子串（`budget exceeded` / `max iterations`）必须保留；既有 `Planner`/`Implementer`/`Evaluator`/`Judge` 接口签名不变（只给 struct 加可选 `Sink`/`Tier` 字段）。

---

## File Structure

- **Create** `internal/agent/goalloop/usage.go` — `Usage`、`Usage.Total()`、`UsageSink{Add,Snapshot}`、`usageFromMeta`、`addUsage`。G02 的统一累加核心，被 planner/evaluator/tierer 共用（DRY）。
- **Create** `internal/agent/goalloop/usage_test.go` — sink 累加、`Total()` 回退、nil 安全。
- **Create** `internal/agent/goalloop/record.go` — `RunRecord`（JSON）+ `NewRunRecord`，把一次 goal run 的 tier/complete/stopReason/usage/iterations 序列化进 kv。
- **Create** `internal/agent/goalloop/record_test.go` — `RunRecord` 序列化往返。
- **Modify** `internal/agent/goalloop/planner.go` — `LLMPlanner` 加 `Sink *UsageSink`；`Plan` 在 `Generate` 后 `addUsage`。
- **Modify** `internal/agent/goalloop/planner_test.go` — 加 usage 累加测试。
- **Modify** `internal/agent/goalloop/evaluators.go` — `IntentEvaluator` / `QualityEvaluator` 各加 `Sink *UsageSink`；`Evaluate` 在 `Generate` 后 `addUsage`。
- **Modify** `internal/agent/goalloop/evaluators_test.go` — 加两个 evaluator 的 usage 累加测试。
- **Modify** `internal/agent/goalloop/tier.go` — 加 `Tier.SkillName()`、`Tier.Path()`、`EvaluatorsForTier`、`ResolveTierFlag`、`EscalationHint`、`tierFlag`；`LLMTierer` 加 `Sink *UsageSink`（可选）。
- **Modify** `internal/agent/goalloop/tier_test.go` — 加上述纯函数 + tierer usage 测试。
- **Modify** `internal/agent/goalloop/types.go` — `Decision` 加 `StopReason string` 与 `Usage Usage`；加 `StopReason*` 常量。
- **Modify** `internal/agent/goalloop/loop.go` — `Config` 加 `Sink *UsageSink`、`Tier Tier`；`spent()`/`overBudget()`/`usageSnapshot()`；预算判定切到 sink；budget-exceeded 与 max-iterations 两处返回 `StopReason`+`Usage`+升级提示。
- **Modify** `internal/agent/goalloop/loop_test.go` — 加 sink 驱动预算停止 + 升级提示测试。
- **Modify** `cmd/autocode/main.go` — 删静默 return；`resolveGoalTier` 改调 `goalloop.ResolveTierFlag`；real path 按 `Tier.Path()` 分派（T0–T2 orchestrator+skill 单轮；T3–T4 loop）；接线 sink；run 结束写 `RunRecord` 进 `app.Store`。

---

## Task 1: `Usage` 类型 + `UsageSink` 累加器（G02 基础）

**Files:**
- Create: `internal/agent/goalloop/usage.go`
- Create: `internal/agent/goalloop/usage_test.go`

- [ ] **Step 1: 写失败测试**

`internal/agent/goalloop/usage_test.go`:
```go
package goalloop

import (
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
)

func TestUsageSink_AddAccumulates(t *testing.T) {
	t.Parallel()
	s := &UsageSink{}
	s.Add(Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15})
	s.Add(Usage{PromptTokens: 20, CompletionTokens: 7, TotalTokens: 27})
	got := s.Snapshot()
	assert.Equal(t, 30, got.PromptTokens, "prompt tokens sum across calls")
	assert.Equal(t, 12, got.CompletionTokens, "completion tokens sum across calls")
	assert.Equal(t, 42, got.TotalTokens, "total tokens sum across calls")
}

func TestUsage_SnapshotIsCopy(t *testing.T) {
	t.Parallel()
	s := &UsageSink{}
	s.Add(Usage{PromptTokens: 1, TotalTokens: 1})
	snap := s.Snapshot()
	snap.PromptTokens = 999 // mutating the snapshot must not touch the sink
	assert.Equal(t, 1, s.Snapshot().PromptTokens, "snapshot must be a copy")
}

func TestUsage_Total_FallsBackToInOutSum(t *testing.T) {
	t.Parallel()
	// When the provider omits TotalTokens (common for some gateways), Total()
	// falls back to prompt+completion so the budget check still works.
	u := Usage{PromptTokens: 40, CompletionTokens: 2, TotalTokens: 0}
	assert.Equal(t, 42, u.Total())
	u2 := Usage{PromptTokens: 40, CompletionTokens: 2, TotalTokens: 99}
	assert.Equal(t, 99, u2.Total(), "non-zero TotalTokens wins")
}

func TestAddUsage_NilSafe(t *testing.T) {
	t.Parallel()
	// Components call addUsage unconditionally; a nil sink or nil message must
	// not panic.
	assert.NotPanics(t, func() {
		addUsage(nil, &schema.Message{})
		addUsage(&UsageSink{}, nil)
	})
}

func TestUsageFromMeta(t *testing.T) {
	t.Parallel()
	msg := &schema.Message{ResponseMeta: &schema.ResponseMeta{
		Usage: &schema.TokenUsage{PromptTokens: 8, CompletionTokens: 3, TotalTokens: 11},
	}}
	assert.Equal(t, Usage{PromptTokens: 8, CompletionTokens: 3, TotalTokens: 11}, usageFromMeta(msg.ResponseMeta))

	assert.Equal(t, Usage{}, usageFromMeta(nil), "nil meta → zero usage")
	assert.Equal(t, Usage{}, usageFromMeta(&schema.ResponseMeta{}), "nil usage → zero usage")
}
```

- [ ] **Step 2: 运行测试确认失败**

Run:
```sh
go test ./internal/agent/goalloop -run "TestUsageSink_|TestUsage_Total|TestAddUsage|TestUsageFromMeta" -v
```
Expected: FAIL（`Usage` / `UsageSink` / `addUsage` / `usageFromMeta` 未定义）。

- [ ] **Step 3: 实现 `usage.go`**

`internal/agent/goalloop/usage.go`:
```go
package goalloop

import (
	"sync"

	"github.com/cloudwego/eino/schema"
)

// Usage is the token consumption of one or more model calls. It mirrors the
// subset of orchestrator.TurnUsage that the goal loop needs for budget
// accounting; defining it locally keeps goalloop from depending on the
// orchestrator package (dependency direction stays inward).
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
}

// Total returns the canonical token count for budget checks. Providers that
// report a non-zero TotalTokens use it directly; providers that omit it (some
// gateways leave it 0) fall back to prompt+completion so the budget still
// bites.
func (u Usage) Total() int {
	if u.TotalTokens > 0 {
		return u.TotalTokens
	}
	return u.PromptTokens + u.CompletionTokens
}

// UsageSink is the shared, concurrency-safe accumulator for goal-loop token
// accounting. One sink is wired into every LLM-calling component (planner,
// intent/quality evaluator, LLMTierer) and into the Loop, so every distinct
// model.Generate call is counted exactly once. Add SUMS per call (it does not
// overwrite): different components make independent Generate calls, each with
// its own usage, so summing across calls is correct — this is also what makes
// parent/child accounting non-duplicating (each call reports into the shared
// sink once, never twice).
type UsageSink struct {
	mu sync.Mutex
	u  Usage
}

// Add sums o into the accumulator. Safe for concurrent use.
func (s *UsageSink) Add(o Usage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.u.PromptTokens += o.PromptTokens
	s.u.CompletionTokens += o.CompletionTokens
	s.u.TotalTokens += o.TotalTokens
}

// Snapshot returns a copy of the accumulated usage. Callers may mutate the
// returned value freely; it is detached from the sink.
func (s *UsageSink) Snapshot() Usage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.u
}

// usageFromMeta maps a model response's ResponseMeta.Usage into a Usage. A nil
// meta (no usage reported — e.g. FakeModel, or a provider that omits it) yields
// the zero Usage, so adding it is a no-op.
func usageFromMeta(meta *schema.ResponseMeta) Usage {
	if meta == nil || meta.Usage == nil {
		return Usage{}
	}
	return Usage{
		PromptTokens:     meta.Usage.PromptTokens,
		CompletionTokens: meta.Usage.CompletionTokens,
		TotalTokens:      meta.Usage.TotalTokens,
	}
}

// addUsage records msg's model-call usage into sink. It is nil-safe on both
// arguments so that LLM-calling components can call it unconditionally right
// after model.Generate without sprinkling nil checks at every call site (DRY).
func addUsage(sink *UsageSink, msg *schema.Message) {
	if sink == nil || msg == nil {
		return
	}
	sink.Add(usageFromMeta(msg.ResponseMeta))
}
```

- [ ] **Step 4: 运行测试确认通过**

Run:
```sh
go test ./internal/agent/goalloop -run "TestUsageSink_|TestUsage_Total|TestAddUsage|TestUsageFromMeta" -v
```
Expected: PASS。

- [ ] **Step 5: 提交**

```sh
git add internal/agent/goalloop/usage.go internal/agent/goalloop/usage_test.go
git commit -m "feat(goalloop): add UsageSink token accumulator (G02)"
```

---

## Task 2: `LLMPlanner` 记录 usage（G02）

**Files:**
- Modify: `internal/agent/goalloop/planner.go`
- Modify: `internal/agent/goalloop/planner_test.go`

- [ ] **Step 1: 写失败测试**

追加到 `internal/agent/goalloop/planner_test.go` 末尾：
```go
func TestLLMPlanner_RecordsUsage(t *testing.T) {
	t.Parallel()
	// A canned plan reply that ALSO carries token usage on ResponseMeta.
	canned := schema.AssistantMessage(
		`{"goal":"x","tests":[{"name":"t","command":"go test ./..."}],"steps":["s1"]}`,
		nil,
	)
	canned.ResponseMeta = &schema.ResponseMeta{
		Usage: &schema.TokenUsage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120},
	}
	m := einollm.NewFakeModelWithMessages([]*schema.Message{canned}, nil)

	sink := &UsageSink{}
	pl := LLMPlanner{Model: m, Sink: sink}
	_, err := pl.Plan(context.Background(), Goal{Text: "implement foo"})
	require.NoError(t, err)

	got := sink.Snapshot()
	assert.Equal(t, 120, got.TotalTokens, "planner must add its Generate usage to the sink")
	assert.Equal(t, 100, got.PromptTokens)
}

func TestLLMPlanner_NilSinkNoPanic(t *testing.T) {
	t.Parateral()
	canned := schema.AssistantMessage(
		`{"goal":"x","tests":[],"steps":["s1"]}`, nil,
	)
	m := einollm.NewFakeModelWithMessages([]*schema.Message{canned}, nil)
	pl := LLMPlanner{Model: m} // Sink nil
	_, err := pl.Plan(context.Background(), Goal{Text: "x"})
	require.NoError(t, err, "nil Sink must be tolerated")
}
```
> 注：第二条测试里 `t.Parateral()` 是笔误，写的时候用 `t.Parallel()`。最终代码里务必是 `t.Parallel()`。

- [ ] **Step 2: 运行确认失败**

Run:
```sh
go test ./internal/agent/goalloop -run "TestLLMPlanner_RecordsUsage|TestLLMPlanner_NilSinkNoPanic" -v
```
Expected: FAIL / 编译失败（`LLMPlanner.Sink` 字段不存在；usage 未累计）。

- [ ] **Step 3: 修改 `planner.go`**

(a) 在 `LLMPlanner` 结构体（约 33-35 行）加字段：
```go
// LLMPlanner asks a chat model to produce a structured plan as JSON.
type LLMPlanner struct {
	Model model.BaseChatModel
	// Sink, when non-nil, accumulates the token usage of each Plan call so the
	// goal loop's budget check reflects real spend. Nil is tolerated (no-op).
	Sink *UsageSink
}
```

(b) 在 `Plan` 方法里、`resp, err := p.Model.Generate(ctx, msgs)`（约 56 行）之后、`content := resp.Content`（约 61 行）之前，插入一行：
```go
	resp, err := p.Model.Generate(ctx, msgs)
	if err != nil {
		return Plan{}, fmt.Errorf("llm planner generate: %w", err)
	}
	addUsage(p.Sink, resp) // G02: count this call's tokens in the shared sink
```
（即在错误检查通过后、读 `resp.Content` 前调用 `addUsage`。）

- [ ] **Step 4: 运行确认通过**

Run:
```sh
go test ./internal/agent/goalloop -run "TestLLMPlanner_" -v
```
Expected: 全部 `TestLLMPlanner_*` PASS（含既有解析/错误用例 + 新 usage 用例）。

- [ ] **Step 5: 提交**

```sh
git add internal/agent/goalloop/planner.go internal/agent/goalloop/planner_test.go
git commit -m "feat(goalloop): LLMPlanner records token usage into sink (G02)"
```

---

## Task 3: `IntentEvaluator` + `QualityEvaluator` 记录 usage（G02）

**Files:**
- Modify: `internal/agent/goalloop/evaluators.go`
- Modify: `internal/agent/goalloop/evaluators_test.go`

- [ ] **Step 1: 写失败测试**

追加到 `internal/agent/goalloop/evaluators_test.go` 末尾：
```go
// verdictMsg builds an assistant message carrying a parseable verdict JSON and
// the given token usage, for evaluator usage-accounting tests.
func verdictMsg(usage *schema.TokenUsage) *schema.Message {
	msg := schema.AssistantMessage(`{"pass":true,"gaps":[],"reason":"ok"}`, nil)
	msg.ResponseMeta = &schema.ResponseMeta{Usage: usage}
	return msg
}

func TestIntentEvaluator_RecordsUsage(t *testing.T) {
	t.Parallel()
	m := einollm.NewFakeModelWithMessages([]*schema.Message{
		verdictMsg(&schema.TokenUsage{PromptTokens: 50, CompletionTokens: 5, TotalTokens: 55}),
	}, nil)
	sink := &UsageSink{}
	ev := IntentEvaluator{Model: m, Sink: sink}
	_, err := ev.Evaluate(context.Background(), Goal{Text: "g"}, Plan{Steps: []string{"s"}}, "")
	require.NoError(t, err)
	assert.Equal(t, 55, sink.Snapshot().TotalTokens, "intent evaluator must count its Generate call")
}

func TestQualityEvaluator_RecordsUsage(t *testing.T) {
	t.Parallel()
	m := einollm.NewFakeModelWithMessages([]*schema.Message{
		verdictMsg(&schema.TokenUsage{PromptTokens: 60, CompletionTokens: 6, TotalTokens: 66}),
	}, nil)
	sink := &UsageSink{}
	ev := QualityEvaluator{Model: m, Sink: sink}
	_, err := ev.Evaluate(context.Background(), Goal{Text: "g"}, Plan{Steps: []string{"s"}}, "")
	require.NoError(t, err)
	assert.Equal(t, 66, sink.Snapshot().TotalTokens, "quality evaluator must count its Generate call")
}

func TestEvaluators_SharedSinkSumsAcrossBoth(t *testing.T) {
	t.Parallel()
	// Two evaluators sharing ONE sink must not double-count: each contributes
	// its own single Generate call, summed once.
	shared := &UsageSink{}
	intentM := einollm.NewFakeModelWithMessages([]*schema.Message{
		verdictMsg(&schema.TokenUsage{TotalTokens: 10}),
	}, nil)
	qualityM := einollm.NewFakeModelWithMessages([]*schema.Message{
		verdictMsg(&schema.TokenUsage{TotalTokens: 20}),
	}, nil)
	ie := IntentEvaluator{Model: intentM, Sink: shared}
	qe := QualityEvaluator{Model: qualityM, Sink: shared}
	_, _ = ie.Evaluate(context.Background(), Goal{Text: "g"}, Plan{Steps: []string{"s"}}, "")
	_, _ = qe.Evaluate(context.Background(), Goal{Text: "g"}, Plan{Steps: []string{"s"}}, "")
	assert.Equal(t, 30, shared.Snapshot().TotalTokens, "shared sink sums both evaluators exactly once")
}
```

- [ ] **Step 2: 运行确认失败**

Run:
```sh
go test ./internal/agent/goalloop -run "TestIntentEvaluator_RecordsUsage|TestQualityEvaluator_RecordsUsage|TestEvaluators_SharedSinkSumsAcrossBoth" -v
```
Expected: FAIL / 编译失败（`IntentEvaluator.Sink` / `QualityEvaluator.Sink` 字段不存在）。

- [ ] **Step 3: 修改 `evaluators.go`**

(a) `IntentEvaluator` 结构体（约 153-155 行）加字段：
```go
// IntentEvaluator uses an LLM to judge whether the implementation matches the goal intent.
type IntentEvaluator struct {
	Model model.BaseChatModel
	// Sink accumulates each Evaluate call's token usage (G02). Nil = no-op.
	Sink *UsageSink
}
```

(b) `QualityEvaluator` 结构体（约 201-203 行）加字段：
```go
// QualityEvaluator uses an LLM to judge code quality of the implementation.
type QualityEvaluator struct {
	Model model.BaseChatModel
	// Sink accumulates each Evaluate call's token usage (G02). Nil = no-op.
	Sink *UsageSink
}
```

(c) 在 `IntentEvaluator.Evaluate` 里 `resp, err := e.Model.Generate(ctx, msgs)`（约 178 行）的错误检查之后、`vj, err := parseLLMVerdict(resp.Content)`（约 183 行）之前，插一行：
```go
	addUsage(e.Sink, resp) // G02: count this call's tokens
```

(d) 在 `QualityEvaluator.Evaluate` 里 `resp, err := e.Model.Generate(ctx, msgs)`（约 220 行）的错误检查之后、`vj, err := parseLLMVerdict(resp.Content)`（约 225 行）之前，插一行：
```go
	addUsage(e.Sink, resp) // G02: count this call's tokens
```

- [ ] **Step 4: 运行确认通过**

Run:
```sh
go test ./internal/agent/goalloop -run "TestIntentEvaluator|TestQualityEvaluator|TestEvaluators_" -v
```
Expected: 全 PASS（含既有 evaluator 用例 + 新 usage 用例）。

- [ ] **Step 5: 提交**

```sh
git add internal/agent/goalloop/evaluators.go internal/agent/goalloop/evaluators_test.go
git commit -m "feat(goalloop): intent/quality evaluators record usage (G02)"
```

---

## Task 4: tier 路由纯函数（G03 基础）+ LLMTierer usage

**Files:**
- Modify: `internal/agent/goalloop/tier.go`
- Modify: `internal/agent/goalloop/tier_test.go`

> 把"tier → 执行路径/skill/evaluator 集合/升级提示"的决策下沉成 goalloop 包内的纯函数，这样 `cmd/autocode/main.go` 只做薄薄的接线，而所有分支逻辑都能在 goalloop 里单测（`main` 包难单测）。

- [ ] **Step 1: 写失败测试**

追加到 `internal/agent/goalloop/tier_test.go` 末尾（`package goalloop`，已有 `schema`/`einollm` import）：
```go
func TestTier_SkillName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		tier Tier
		want string
	}{
		{TierQuickFix, "dev-quick-fix"},
		{TierStandard, "dev-standard-feature"},
		{TierDesigned, "dev-designed-feature"},
		{TierTeam, "dev-team-feature"},
		{TierAutonomous, "dev-autonomous-project"},
		{Tier(999), "dev-standard-feature"}, // unknown → safe default
	}
	for _, c := range cases {
		assert.Equal(t, c.want, c.tier.SkillName(), "tier %v", c.tier)
	}
}

func TestTier_Path(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		tier Tier
		want string
	}{
		{TierQuickFix, "lightweight"},
		{TierStandard, "lightweight"},
		{TierDesigned, "lightweight"},
		{TierTeam, "loop"},
		{TierAutonomous, "loop"},
	} {
		assert.Equal(t, c.want, c.tier.Path(), "tier %v", c.tier)
	}
}

func TestEvaluatorsForTier(t *testing.T) {
	t.Parallel()
	m := einollm.NewFakeModel([]string{"x"}, nil)
	assert.Len(t, EvaluatorsForTier(TierQuickFix, m, nil), 1, "T0: test only")
	assert.Len(t, EvaluatorsForTier(TierStandard, m, nil), 2, "T1: test+intent")
	assert.Len(t, EvaluatorsForTier(TierDesigned, m, nil), 3, "T2: test+intent+quality")
	assert.Len(t, EvaluatorsForTier(TierAutonomous, m, nil), 3, "T4: all three")
	// Nil model: only the non-LLM TestEvaluator is returned regardless of tier.
	assert.Len(t, EvaluatorsForTier(TierAutonomous, nil, nil), 1, "nil model → test only")
}

func TestEvaluatorsForTier_StampSink(t *testing.T) {
	t.Parallel()
	m := einollm.NewFakeModel([]string{"x"}, nil)
	sink := &UsageSink{}
	evals := EvaluatorsForTier(TierDesigned, m, sink)
	// The intent + quality evaluators must carry the shared sink so their usage
	// flows into the loop's budget. (TestEvaluator has no Sink field.)
	assert.Equal(t, sink, evals[1].(IntentEvaluator).Sink)
	assert.Equal(t, sink, evals[2].(QualityEvaluator).Sink)
}

func TestResolveTierFlag(t *testing.T) {
	t.Parallel()
	// auto → RuleTierer (forced=false)
	t0, forced, err := ResolveTierFlag("auto", "fix the typo")
	require.NoError(t, err)
	assert.False(t, forced)
	assert.Equal(t, TierQuickFix, t0)

	// explicit t2 → forced
	t2, forced, err := ResolveTierFlag("t2", "anything")
	require.NoError(t, err)
	assert.True(t, forced)
	assert.Equal(t, TierDesigned, t2)

	// invalid → error (caller prints + exits)
	_, _, err = ResolveTierFlag("t9", "x")
	assert.Error(t, err)
}

func TestEscalationHint(t *testing.T) {
	t.Parallel()
	assert.NotEmpty(t, EscalationHint(TierQuickFix), "T0 gets an upgrade hint")
	assert.Contains(t, EscalationHint(TierStandard), "t2")
	assert.Empty(t, EscalationHint(TierAutonomous), "T4 has nowhere to escalate")
}

func TestLLMTierer_RecordsUsage(t *testing.T) {
	t.Parallel()
	msg := schema.AssistantMessage("designed", nil)
	msg.ResponseMeta = &schema.ResponseMeta{Usage: &schema.TokenUsage{TotalTokens: 7}}
	m := einollm.NewFakeModelWithMessages([]*schema.Message{msg}, nil)
	sink := &UsageSink{}
	lt := LLMTierer{Model: m, Fallback: RuleTierer{}, Sink: sink}
	_, err := lt.Tier(context.Background(), "refactor the module")
	require.NoError(t, err)
	assert.Equal(t, 7, sink.Snapshot().TotalTokens, "LLMTierer must count its Generate call")
}
```

- [ ] **Step 2: 运行确认失败**

Run:
```sh
go test ./internal/agent/goalloop -run "TestTier_SkillName|TestTier_Path|TestEvaluatorsForTier|TestResolveTierFlag|TestEscalationHint|TestLLMTierer_RecordsUsage" -v
```
Expected: FAIL / 编译失败（`SkillName`/`Path`/`EvaluatorsForTier`/`ResolveTierFlag`/`EscalationHint` 未定义；`LLMTierer.Sink` 不存在）。

- [ ] **Step 3: 修改 `tier.go`**

(a) import 块加 `"fmt"`（若未导入；当前 tier.go 只导入 `context`/`strings`/`model`/`schema`）：
```go
import (
	"context"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)
```

(b) `LLMTierer` 结构体（约 79-82 行）加 `Sink` 字段：
```go
type LLMTierer struct {
	Model    model.BaseChatModel
	Fallback Tierer
	// Sink accumulates each Tier call's token usage (G02). Nil = no-op.
	Sink *UsageSink
}
```

(c) 在 `LLMTierer.Tier` 里 `out, err := l.Model.Generate(...)`（约 102-104 行）之后、`if err == nil && out != nil`（约 105 行）之前，插一行：
```go
	addUsage(l.Sink, out) // G02: count this call's tokens (even on error path out may be nil → no-op)
```
> 注意：`addUsage` 对 nil `out` 安全（见 Task 1）。这里在 err 判断前调用即可，因为 Generate 出错时 `out` 通常为 nil，`addUsage` 直接返回。

(d) 在文件末尾追加新函数：
```go
// SkillName returns the SKILL.md registry name for the tier. Each maps to a
// directory under skills/ (dev-quick-fix … dev-autonomous-project). An unknown
// tier falls back to the standard skill (safe default, mirrors RuleTierer).
func (t Tier) SkillName() string {
	switch t {
	case TierQuickFix:
		return "dev-quick-fix"
	case TierStandard:
		return "dev-standard-feature"
	case TierDesigned:
		return "dev-designed-feature"
	case TierTeam:
		return "dev-team-feature"
	case TierAutonomous:
		return "dev-autonomous-project"
	}
	return "dev-standard-feature"
}

// Path returns the execution path the tier uses:
//   - "lightweight" (T0–T2): a single orchestrator+skill turn that follows the
//     tier's SKILL.md via the already-wired fs/shell tools.
//   - "loop" (T3–T4): the full plan-implement-evaluate-judge goal loop with
//     workers and multi-evaluator judging.
//
// The lightweight tiers map to skills whose workflow a single agent turn can
// perform (quick fix, TDD standard, designed-feature spec→plan); the loop tiers
// map to skills that explicitly use the Goal Loop's Lead/Worker/Integrator.
func (t Tier) Path() string {
	if t <= TierDesigned {
		return "lightweight"
	}
	return "loop"
}

// EvaluatorsForTier returns the evaluator set appropriate for the tier:
//   - T0: TestEvaluator only (a quick fix must at least pass its tests).
//   - T1: + IntentEvaluator (did the standard feature meet the goal intent?).
//   - T2–T4: + QualityEvaluator (the designed/team/autonomous bar includes code
//     quality).
//
// A nil model yields only TestEvaluator (the non-LLM one), so callers without a
// chat model still get a meaningful (if shallow) evaluation. sink, when
// non-nil, is stamped onto each LLM-backed evaluator so every Generate call in
// the tier's evaluator set flows into the one shared budget (G02). Used by the
// goal loop path; the lightweight path relies on the orchestrator turn instead.
func EvaluatorsForTier(t Tier, m model.BaseChatModel, sink *UsageSink) []Evaluator {
	base := []Evaluator{TestEvaluator{}}
	if m == nil {
		return base
	}
	switch {
	case t <= TierQuickFix:
		return base
	case t == TierStandard:
		return []Evaluator{TestEvaluator{}, IntentEvaluator{Model: m, Sink: sink}}
	default: // TierDesigned, TierTeam, TierAutonomous
		return []Evaluator{
			TestEvaluator{},
			IntentEvaluator{Model: m, Sink: sink},
			QualityEvaluator{Model: m, Sink: sink},
		}
	}
}

// ResolveTierFlag maps a -tier flag value to a Tier:
//   - "auto": runs RuleTierer over text (forced=false).
//   - "t0".."t4": forces that tier (forced=true).
//   - anything else: returns an error the caller surfaces as a usage error.
//
// Extracted from cmd/autocode so the mapping is unit-testable (package main is
// not). The caller still owns printing/exiting on error.
func ResolveTierFlag(flagValue, text string) (Tier, bool, error) {
	if flagValue == "auto" {
		t, _ := RuleTierer{}.Tier(context.Background(), text)
		return t, false, nil
	}
	t, ok := ParseForcedTier(flagValue)
	if !ok {
		return TierStandard, false, fmt.Errorf("invalid tier %q (want auto or t0..t4)", flagValue)
	}
	return t, true, nil
}

// tierFlag is the inverse of ParseForcedTier: Tier → "t0".."t4".
var tierFlagTable = [5]string{"t0", "t1", "t2", "t3", "t4"}

func tierFlag(t Tier) string {
	if int(t) >= 0 && int(t) < len(tierFlagTable) {
		return tierFlagTable[t]
	}
	return "t1"
}

// EscalationHint returns a human-readable recommendation to re-run at the next
// higher tier, or "" for TierAutonomous (the top tier has nowhere to escalate).
//
// The dev-* skills each have an "Escalate" section that tells the AGENT when to
// re-tier; EscalationHint is the loop-side counterpart: when a lower tier
// exhausts its budget/iterations, the Decision surfaces this hint instead of
// exiting silently (G03: "升级规则不会静默退出").
func EscalationHint(t Tier) string {
	if t >= TierAutonomous {
		return ""
	}
	next := t + 1
	return fmt.Sprintf("consider escalating to tier %s (-tier %s)", next.String(), tierFlag(next))
}
```

- [ ] **Step 4: 运行确认通过**

Run:
```sh
go test ./internal/agent/goalloop -run "TestTier_|TestEvaluatorsForTier|TestResolveTierFlag|TestEscalationHint|TestLLMTierer_|TestRuleTierer|TestLLMTierer_Fallback" -v
```
Expected: 全 PASS（含既有 tierer 用例 + 新路由/usage 用例）。

- [ ] **Step 5: 提交**

```sh
git add internal/agent/goalloop/tier.go internal/agent/goalloop/tier_test.go
git commit -m "feat(goalloop): tier path/skill/evaluator routing + escalation hint (G03)"
```

---

## Task 5: Loop 按 sink 计预算 + `Decision.StopReason/Usage` + 升级提示（G02 核心 + G03 升级）

**Files:**
- Modify: `internal/agent/goalloop/types.go`
- Modify: `internal/agent/goalloop/loop.go`
- Modify: `internal/agent/goalloop/loop_test.go`

- [ ] **Step 1: 写失败测试**

追加到 `internal/agent/goalloop/loop_test.go` 末尾（`package goalloop`，已有 testify/imports）：
```go
func TestLoop_BudgetStopsOnAccumulatedUsage(t *testing.T) {
	t.Parallel()
	// Simulate two evaluators each adding usage to a shared sink, then verify
	// the loop stops BEFORE the next iteration once the sink crosses MaxTokens.
	// We pre-charge the sink past the budget so the check at the top of iter 2
	// fires (the loop checks before planning each iteration).
	sink := &UsageSink{}
	sink.Add(Usage{TotalTokens: 150}) // already over a 100-token budget

	planner := FakePlanner{Steps: []string{"s1"}}
	impl := &FakeImplementer{Result: "done"}
	loop := New(Config{
		Planner:    planner,
		Implementer: impl,
		Evaluators: []Evaluator{&CounterEvaluator{passAt: 1}},
		Judge:      AggregateJudge{},
		Budget:     Budget{MaxIterations: 5, MaxTokens: 100},
		Sink:       sink,
	})

	decision, err := loop.Run(context.Background(), Goal{Text: "x"}, func(Event) {})
	require.NoError(t, err)
	assert.False(t, decision.Complete)
	assert.Equal(t, StopReasonTokenBudget, decision.StopReason, "must record WHY it stopped")
	assert.Contains(t, decision.Summary, "budget exceeded")
	assert.Greater(t, decision.Usage.Total(), 0, "Decision carries the usage snapshot")
}

func TestLoop_BudgetCheckMidIteration_AfterPlan(t *testing.T) {
	t.Parallel()
	// A planner that charges the sink on Plan, crossing the budget mid-turn.
	// The loop must stop right after Plan (before Implement), not run a full
	// expensive iteration.
	sink := &UsageSink{}
	planner := &chargingPlanner{sink: sink, perCall: Usage{TotalTokens: 200}}
	impl := &FakeImplementer{Result: "done"}

	loop := New(Config{
		Planner:    planner,
		Implementer: impl,
		Evaluators: []Evaluator{&CounterEvaluator{passAt: 1}},
		Judge:      AggregateJudge{},
		Budget:     Budget{MaxIterations: 5, MaxTokens: 100},
		Sink:       sink,
	})
	decision, err := loop.Run(context.Background(), Goal{Text: "x"}, func(Event) {})
	require.NoError(t, err)
	assert.Equal(t, StopReasonTokenBudget, decision.StopReason)
	assert.Equal(t, 0, impl.Calls(), "must not Implement once the plan blew the budget")
}

// chargingPlanner is a Planner that adds perCall to sink on every Plan call.
type chargingPlanner struct {
	sink    *UsageSink
	perCall Usage
}

func (p *chargingPlanner) Plan(context.Context, Goal) (Plan, error) {
	p.sink.Add(p.perCall)
	return Plan{Goal: "x", Steps: []string{"s1"}, Tests: []AcceptanceTest{{Name: "t", Command: "true"}}}, nil
}

func TestLoop_MaxIterationsHasEscalationHint(t *testing.T) {
	t.Parallel()
	// A low tier that never passes must surface an escalation hint (not exit
	// silently) and tag StopReason=escalate.
	planner := FakePlanner{Steps: []string{"s1"}}
	impl := &FakeImplementer{Result: "done"}
	loop := New(Config{
		Planner:    planner,
		Implementer: impl,
		Evaluators: []Evaluator{&CounterEvaluator{passAt: 100}},
		Judge:      AggregateJudge{},
		Budget:     Budget{MaxIterations: 2},
		Tier:       TierQuickFix,
	})
	decision, err := loop.Run(context.Background(), Goal{Text: "x"}, func(Event) {})
	require.NoError(t, err)
	assert.False(t, decision.Complete)
	assert.Contains(t, decision.Summary, "max iterations", "keep the existing substring")
	assert.Contains(t, decision.Summary, "escalat", "append the escalation hint")
	assert.Equal(t, StopReasonEscalate, decision.StopReason)
}

func TestLoop_T4MaxIterationsNoEscalationHint(t *testing.T) {
	t.Parallel()
	// T4 is the top tier: exhaustion reports max-iterations without an upgrade
	// hint, and StopReason stays max_iterations (not escalate).
	planner := FakePlanner{Steps: []string{"s1"}}
	impl := &FakeImplementer{Result: "done"}
	loop := New(Config{
		Planner:    planner,
		Implementer: impl,
		Evaluators: []Evaluator{&CounterEvaluator{passAt: 100}},
		Judge:      AggregateJudge{},
		Budget:     Budget{MaxIterations: 1},
		Tier:       TierAutonomous,
	})
	decision, err := loop.Run(context.Background(), Goal{Text: "x"}, func(Event) {})
	require.NoError(t, err)
	assert.NotContains(t, decision.Summary, "escalat")
	assert.Equal(t, StopReasonMaxIters, decision.StopReason)
}
```

- [ ] **Step 2: 运行确认失败**

Run:
```sh
go test ./internal/agent/goalloop -run "TestLoop_BudgetStopsOnAccumulatedUsage|TestLoop_BudgetCheckMidIteration_AfterPlan|TestLoop_MaxIterationsHasEscalationHint|TestLoop_T4MaxIterationsNoEscalationHint" -v
```
Expected: FAIL / 编译失败（`Config.Sink`/`Config.Tier` 不存在；`Decision.StopReason`/`Usage` 不存在；`StopReason*` 常量未定义）。

- [ ] **Step 3a: 修改 `types.go`**

把 `Decision` 结构体（约 34-38 行）替换为：
```go
// Decision is the Judge's aggregate verdict: is the goal complete?
type Decision struct {
	Complete bool     // true if all evaluators pass
	Gaps     []string // concatenated gaps from all failing evaluators
	Summary  string   // one-line summary of the decision
	// StopReason explains why the loop terminated (G02 persistence + G03
	// escalation). Empty (StopReasonComplete) when the goal completed; otherwise
	// one of StopReasonTokenBudget / StopReasonMaxIters / StopReasonEscalate.
	StopReason string
	// Usage is the accumulated token spend at termination (G02). Mirrored from
	// the shared UsageSink so callers (e.g. the CLI's persisted run record) get
	// the final tally without a back-channel.
	Usage Usage
}

// Stop reason constants carried on Decision.StopReason. The zero value
// (StopReasonComplete) means the goal completed successfully.
const (
	StopReasonComplete    = ""
	StopReasonTokenBudget = "token_budget"
	StopReasonMaxIters    = "max_iterations"
	StopReasonEscalate    = "escalate"
)
```

- [ ] **Step 3b: 修改 `loop.go`**

(a) `Config` 结构体（约 10-16 行）替换为：
```go
// Config holds the components and budget for the Goal Loop.
type Config struct {
	Planner     Planner
	Implementer Implementer
	Evaluators  []Evaluator
	Judge       Judge
	Budget      Budget
	// Sink, when non-nil, is the shared token accumulator every LLM-calling
	// component writes to (G02). The loop drives its budget check from this sink
	// (spent = sink.Snapshot().Total()); when nil the loop falls back to the
	// static Budget.SpentTokens field so existing pre-G02 callers/tests work.
	Sink *UsageSink
	// Tier is the difficulty tier this run was dispatched at (G03). It only
	// affects the exhaustion message (EscalationHint) — it does not change the
	// pipeline, which is set by the caller via the components above. Zero value
	// (TierQuickFix) is safe.
	Tier Tier
}
```

(b) 在 `type Loop struct` / `func New` 之后、`Run` 之前（约 29-30 行之间），插入预算辅助方法：
```go
// spent returns the current total token spend. When a UsageSink is wired
// (Config.Sink != nil) it is the live, accumulated total across every model
// call; otherwise it falls back to the static Budget.SpentTokens field for
// pre-G02 callers. This fallback is what keeps TestLoop_BudgetExceeded green.
func (l *Loop) spent() int {
	if l.cfg.Sink != nil {
		return l.cfg.Sink.Snapshot().Total()
	}
	return l.cfg.Budget.SpentTokens
}

// overBudget reports whether the token budget has been crossed. A zero
// MaxTokens disables the token budget entirely (iteration budget still applies).
func (l *Loop) overBudget() bool {
	return l.cfg.Budget.MaxTokens > 0 && l.spent() > l.cfg.Budget.MaxTokens
}

// usageSnapshot returns the accumulated usage to stamp onto a terminal Decision.
func (l *Loop) usageSnapshot() Usage {
	if l.cfg.Sink != nil {
		return l.cfg.Sink.Snapshot()
	}
	return Usage{}
}

// budgetExceededDecision builds the terminal Decision for a token-budget stop,
// carrying the spend and the canonical stop reason so the CLI can persist it.
func (l *Loop) budgetExceededDecision(at string) Decision {
	return Decision{
		Complete:   false,
		Summary:    fmt.Sprintf("budget exceeded %s (%d tokens > %d)", at, l.spent(), l.cfg.Budget.MaxTokens),
		StopReason: StopReasonTokenBudget,
		Usage:      l.usageSnapshot(),
	}
}
```

(c) 替换 `Run` 里顶部的 token budget check（当前约 64-66 行的 `if l.cfg.Budget.MaxTokens > 0 && l.cfg.Budget.SpentTokens > l.cfg.Budget.MaxTokens { return Decision{...}, nil }`）为：
```go
		// Token budget check (G02): driven by the shared sink when wired.
		if l.overBudget() {
			return l.budgetExceededDecision("before iteration"), nil
		}
```

(d) 在 Plan 成功之后、Implement 之前（当前约 76-78 行，`emit("Plan", ...)` 之后、`// --- Implement ---` 注释之前），插入一次预算复查，让预算在一次昂贵的 plan 调用后就生效：
```go
		emit("Plan", fmt.Sprintf("%d steps, %d tests", len(plan.Steps), len(plan.Tests)), iter)

		// Re-check budget after the planner's LLM call so an oversized plan
		// stops the loop before we pay for an expensive Implement/Evaluate.
		if l.overBudget() {
			return l.budgetExceededDecision("after plan"), nil
		}
```

(e) 替换 `Run` 末尾的 max-iterations 返回（当前约 118-122 行）为带 StopReason + 升级提示的版本：
```go
	// Exhausted all iterations without completion.
	hint := EscalationHint(l.cfg.Tier)
	summary := fmt.Sprintf("max iterations (%d) reached without completion", l.cfg.Budget.MaxIterations)
	reason := StopReasonMaxIters
	// A sub-top tier that ran out of budget surfaces an explicit upgrade
	// recommendation instead of exiting silently (G03: 升级规则不静默退出).
	if hint != "" {
		summary += "; " + hint
		reason = StopReasonEscalate
	}
	return Decision{
		Complete:   false,
		Summary:    summary,
		StopReason: reason,
		Usage:      l.usageSnapshot(),
	}, nil
```

- [ ] **Step 4: 运行确认通过（含既有回归）**

Run:
```sh
go test ./internal/agent/goalloop -run "TestLoop_" -v
```
Expected: 全 PASS——既有 `TestLoop_BudgetExceeded`（无 sink → 回退 `SpentTokens=200>100`，Summary 含 "budget exceeded"）、`TestLoop_MaxIterationsExhausted`（默认 Tier=TierQuickFix<T4 → Summary 含 "max iterations" 且带 "escalat"，StopReason=escalate）+ 新 4 个测试。

> 如果 `TestLoop_MaxIterationsExhausted` 因 Tier 默认值变化（现在带升级提示）而需对齐，确认它只断言 `Contains "max iterations"`（现状如此），无需改测试。

- [ ] **Step 5: 提交**

```sh
git add internal/agent/goalloop/types.go internal/agent/goalloop/loop.go internal/agent/goalloop/loop_test.go
git commit -m "feat(goalloop): sink-driven budget + Decision.StopReason/Usage + escalation (G02/G03)"
```

---

## Task 6: `RunRecord` 持久化记录（G02 持久化）

**Files:**
- Create: `internal/agent/goalloop/record.go`
- Create: `internal/agent/goalloop/record_test.go`

> "达到预算可靠停止并持久化原因"——把一次 goal run 的结果序列化成 JSON，由 `cmd/autocode` 写进现有 `kv` 表（`store.KVSet`，零迁移）。记录本身是纯数据 + 构造函数，在 goalloop 包内单测；写库由 main 调 store API（store 侧已有测试）。

- [ ] **Step 1: 写失败测试**

`internal/agent/goalloop/record_test.go`:
```go
package goalloop

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewRunRecord_FieldsAndReason(t *testing.T) {
	t.Parallel()
	sink := &UsageSink{}
	sink.Add(Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15})
	d := Decision{
		Complete:   false,
		Summary:    "budget exceeded after plan (215 tokens > 200)",
		StopReason: StopReasonTokenBudget,
		Usage:      sink.Snapshot(),
	}
	rec := NewRunRecord(TierDesigned, d, sink.Snapshot(), 3)
	assert.Equal(t, "designed", rec.Tier)
	assert.False(t, rec.Complete)
	assert.Equal(t, StopReasonTokenBudget, rec.StopReason)
	assert.Equal(t, 3, rec.Iterations)
	assert.Equal(t, 15, rec.Usage.TotalTokens)
	assert.Equal(t, "budget exceeded after plan (215 tokens > 200)", rec.Summary)
}

func TestRunRecord_RoundTripsJSON(t *testing.T) {
	t.Parallel()
	rec := NewRunRecord(
		TierAutonomous,
		Decision{Complete: true, Summary: "all 3 evaluators passed", StopReason: StopReasonComplete},
		Usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3},
		4,
	)
	data, err := json.Marshal(rec)
	require.NoError(t, err)

	var back RunRecord
	require.NoError(t, json.Unmarshal(data, &back))
	assert.Equal(t, rec, back, "record must survive a JSON round-trip")

	// The stop reason must be present in the wire form so a future reader of the
	// kv row can tell WHY the run ended without re-deriving it.
	assert.Contains(t, string(data), `"stop_reason"`)
	assert.Contains(t, string(data), `"tier":"autonomous"`)
}
```

- [ ] **Step 2: 运行确认失败**

Run:
```sh
go test ./internal/agent/goalloop -run "TestNewRunRecord_|TestRunRecord_RoundTripsJSON" -v
```
Expected: FAIL / 编译失败（`RunRecord` / `NewRunRecord` 未定义）。

- [ ] **Step 3: 实现 `record.go`**

`internal/agent/goalloop/record.go`:
```go
package goalloop

// RunRecord is the durable summary of one goal-loop run, written to the store's
// kv table by cmd/autocomplete after Run returns (G02: 持久化停止原因). It is
// pure data with JSON tags so it survives a kv round-trip without a schema
// migration — the kv table already holds arbitrary string values.
type RunRecord struct {
	Tier       string `json:"tier"`
	Complete   bool   `json:"complete"`
	StopReason string `json:"stop_reason"`
	Summary    string `json:"summary"`
	Iterations int    `json:"iterations"`
	Usage      Usage  `json:"usage"`
}

// NewRunRecord assembles a RunRecord from a finished run. tier is the resolved
// Tier the run was dispatched at; decision is the Loop's (or lightweight path's)
// terminal Decision; usage is the final accumulated spend; iterations is the
// number of plan-implement-evaluate-judge cycles executed (1 for the
// lightweight single-turn path).
func NewRunRecord(tier Tier, decision Decision, usage Usage, iterations int) RunRecord {
	return RunRecord{
		Tier:       tier.String(),
		Complete:   decision.Complete,
		StopReason: decision.StopReason,
		Summary:    decision.Summary,
		Iterations: iterations,
		Usage:      usage,
	}
}
```

- [ ] **Step 4: 运行确认通过**

Run:
```sh
go test ./internal/agent/goalloop -run "TestNewRunRecord_|TestRunRecord_RoundTripsJSON" -v
```
Expected: PASS。

- [ ] **Step 5: 提交**

```sh
git add internal/agent/goalloop/record.go internal/agent/goalloop/record_test.go
git commit -m "feat(goalloop): RunRecord for persisted run outcome (G02)"
```

---

## Task 7: `cmd/autocode/main.go` 接通 tier 分派 + sink + 持久化（G03 接通 + G02 接线）

**Files:**
- Modify: `cmd/autocode/main.go`（`goal` 函数及 `resolveGoalTier`，约 353-506 行）

> 这是 G03 的"接通"核心：删掉 `forced && resolvedTier <= TierDesigned` 的静默 `return`，按 `Tier.Path()` 把 real path 分派到 lightweight（orchestrator+skill 单轮，T0–T2）或 loop（T3–T4）。`--fake-model` 路径不变（确定性 demo）。real path 末尾把 `RunRecord` 写进 `app.Store`。

- [ ] **Step 1: 改 import（加 `"time"`）**

`cmd/autocode/main.go` 顶部 import 块（约 10-31 行）已有 `encoding/json`、`store`、`goalloop`、`bootstrap`。加 `"time"`（若已存在则跳过）：
```go
	"strings"
	"sync"
	"syscall"
	"time"
```

- [ ] **Step 2: 替换静默 return 分支 + real path 分派**

定位 `goal` 函数里这段（当前约 378-390 行）：
```go
	// Resolve the development tier. "auto" uses the RuleTierer; t0..t4 forces a
	// specific tier via the shared goalloop.ParseForcedTier mapping.
	resolvedTier, forced := resolveGoalTier(*tierFlag, text)

	// Explicit T0-T2 override routes to the lightweight path (orchestrator +
	// skill); the full runner is wired up in T25. For auto mode (the default)
	// and explicit T3-T4, fall through to the existing goal loop so behavior
	// stays non-breaking.
	if forced && resolvedTier <= goalloop.TierDesigned {
		fmt.Printf("[tier: %s] lightweight path (orchestrator + skill)\n", resolvedTier)
		return
	}
	fmt.Printf("[tier: %s]\n", resolvedTier)
```
替换为（删静默 return；保留 tier 打印；`forced` 仍计算但不再阻断）：
```go
	// Resolve the development tier. "auto" uses the RuleTierer; t0..t4 forces a
	// specific tier. G03: every tier now enters a real execution path — the
	// silent "lightweight path" print-and-return for forced T0-T2 is removed.
	resolvedTier, forced := resolveGoalTier(*tierFlag, text)
	_ = forced // kept for the (future) "was the tier explicit?" distinction
	fmt.Printf("[tier: %s] path: %s\n", resolvedTier, resolvedTier.Path())
```

- [ ] **Step 3: 替换 `else`（real path）分支，按 Path 分派 + 接 sink + 持久化**

定位 `goal` 函数 real path 分支（当前约 421-458 行，从 `} else {` 到构造完 `loop = goalloop.New(...)` 的 `}`）。整段替换为：
```go
	} else {
		// Real path: build the app to get the LLM model + orchestrator + store,
		// then dispatch by tier. G03: T0-T2 (lightweight) run a single
		// orchestrator+skill turn that edits files via the bound fs/shell
		// tools; T3-T4 (loop) run the full plan-implement-evaluate-judge cycle.
		app, err := bootstrap.Build(bootstrap.Options{
			ConfigPath: *configPath,
			FakeModel:  false,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "autocode goal: bootstrap: %v\n", err)
			os.Exit(1)
		}
		defer app.Shutdown(ctx)

		// Shared token sink (G02): every LLM-calling component adds to it; the
		// loop drives its budget from it and the persisted record carries it.
		sink := &goalloop.UsageSink{}

		if resolvedTier.Path() == "lightweight" {
			// --- T0-T2: one orchestrator turn with the tier's skill body ---
			decision := runLightweightGoal(ctx, app, resolvedTier, text)
			persistGoalRun(app.Store, resolvedTier, decision, sink.Snapshot(), 1)
			fmt.Printf("decision: complete=%v, summary=%s\n", decision.Complete, decision.Summary)
			if decision.Complete {
				os.Exit(0)
			}
			os.Exit(1)
		}

		// --- T3-T4: full goal loop ---
		chatModel := app.Model
		planner := goalloop.LLMPlanner{Model: chatModel, Sink: sink}
		impl := &goalloop.ACPImplementer{Agent: *agent}
		// M8: when the VCS + repo are available, wire the autoVCS worker path
		// (worktree branch + merge) and deliver the vcs MCP server to spawned
		// ACP agents. Gated on VCSRepoID so a failed InitRepo leaves the worker
		// on the git-worktree fallback.
		if app.VCSRepoID != "" {
			impl = impl.WithVCS(app.VCS, app.VCSRepoID, app.VCSDBPath, app.WorktreeDir)
		}
		evals := goalloop.EvaluatorsForTier(resolvedTier, chatModel, sink)

		loop = goalloop.New(goalloop.Config{
			Planner:     planner,
			Implementer: impl,
			Evaluators:  evals,
			Judge:       goalloop.AggregateJudge{},
			Budget:      goalloop.Budget{MaxIterations: *maxIters},
			Sink:        sink,
			Tier:        resolvedTier,
		})
	}
```

并在 `loop.Run` 调用（约 462-474 行）之后、`fmt.Printf("decision: ...")`（约 470 行）之前，把持久化接上。定位：
```go
	decision, err := loop.Run(ctx, g, func(e goalloop.Event) {
		fmt.Printf("[iter %d] %s: %s\n", e.Iteration, e.Phase, e.Detail)
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "autocode goal: %v\n", err)
		os.Exit(1)
	}
```
在 `if err != nil { ... }` 之后、`fmt.Printf("decision: ...")`（约 470 行）之前插入持久化（注意：fake path 没有 `app.Store`，只在 real path 持久化——用变量标记，见 Step 4 的 `storeRef`）。为保持 fake/real 两条路径都能走到同一段打印逻辑，在函数顶部声明 `var runStore *store.Store`，real path 里设 `runStore = app.Store`，然后在 decision 打印前：
```go
	if runStore != nil {
		persistGoalRun(runStore, resolvedTier, decision, sinkForReport.Snapshot(), loop.Iterations())
	}
```
> 实现细节见 Step 4：`sinkForReport` 在 fake path 下为 nil-safe（用一个返回 0 的 helper 或单独处理）。为避免在 fake path 引入 sink，更简单的做法是把持久化 **内联进 real path 两条分支**（lightweight 分支已写；loop 分支在 `loop.Run` 返回后紧跟一行）。下面 Step 4 给出完整收尾。

- [ ] **Step 4: 补三个 helper 函数 + lightweight/loop 两处持久化收尾**

在 `main.go` 末尾（`resolveGoalTier` 之后，约 506 行之后）追加：
```go
// runLightweightGoal executes the T0-T2 lightweight path: one orchestrator
// turn that follows the tier's SKILL.md (loaded from the app's skill registry)
// and edits files via the orchestrator's bound tools. It returns a Decision
// carrying the assistant summary and, when the tier is below T4, an escalation
// hint so an undersized tier surfaces a next step instead of exiting silently
// (G03).
func runLightweightGoal(ctx context.Context, app *bootstrap.App, tier goalloop.Tier, text string) goalloop.Decision {
	prompt := text
	if skill, ok := app.Skills.Get(tier.SkillName()); ok {
		if body, err := app.Skills.Body(skill); err == nil && body != "" {
			prompt = body + "\n\n---\n\nGoal: " + text
		}
	}
	result, err := app.Orch.Query(ctx, prompt)
	if err != nil {
		return goalloop.Decision{
			Complete: false,
			Summary:  "lightweight turn error: " + err.Error(),
		}
	}
	summary := result
	if hint := goalloop.EscalationHint(tier); hint != "" {
		// Non-silent: even a finished low-tier turn advertises the upgrade path.
		summary = result + " (" + hint + ")"
	}
	return goalloop.Decision{Complete: true, Summary: summary}
}

// persistGoalRun writes a RunRecord for the finished goal run into the store's
// kv table (G02: persist why the run ended). Failures are best-effort: a
// persistence error is logged to stderr but never fails the goal command.
func persistGoalRun(st *store.Store, tier goalloop.Tier, decision goalloop.Decision, usage goalloop.Usage, iterations int) {
	if st == nil {
		return
	}
	rec := goalloop.NewRunRecord(tier, decision, usage, iterations)
	data, err := json.Marshal(rec)
	if err != nil {
		fmt.Fprintf(os.Stderr, "autocode goal: encode run record: %v\n", err)
		return
	}
	key := fmt.Sprintf("goalrun:%d", time.Now().Unix())
	if err := st.KVSet(key, string(data)); err != nil {
		fmt.Fprintf(os.Stderr, "autocode goal: persist run record: %v\n", err)
	}
}
```

然后修两处持久化调用，使其与 fake/real 两条路径都自洽：

- **Loop 分支持久化**：在 real-path loop 的 `loop.Run` 返回、`err == nil` 之后、`fmt.Printf("decision: ...")` 之前插入：
```go
		persistGoalRun(app.Store, resolvedTier, decision, sink.Snapshot(), loop.Iterations())
```
（注意：`loop.Run` 是在 `goal` 函数体末尾、两条分支汇合处调用的；由于 fake path 没有 `app`/`sink`，需要把持久化限定在 real path。最干净的做法是：把 `loop.Run` + 持久化 + 打印 分成 fake/real 各自的一段。下面给出重构后的汇合段。）

替换 `goal` 函数末尾（当前约 460-474 行，从 `g := goalloop.Goal{...}` 到 `os.Exit(1)`）为下面两段（保持 fake path 与 real-path-loop 各自完整）：
```go
	g := goalloop.Goal{Text: text, Workdir: wd}

	// Fake path: deterministic two-iteration demo (unchanged). No store/sink.
	if *fakeModel {
		decision, err := loop.Run(ctx, g, func(e goalloop.Event) {
			fmt.Printf("[iter %d] %s: %s\n", e.Iteration, e.Phase, e.Detail)
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "autocode goal: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("decision: complete=%v, summary=%s\n", decision.Complete, decision.Summary)
		if decision.Complete {
			os.Exit(0)
		}
		os.Exit(1)
	}

	// Real path loop (T3-T4): the loop was built in the else branch above. Run
	// it, persist the outcome, and report. (The T0-T2 lightweight path already
	// ran, persisted, and exited inside the else branch.)
	decision, err := loop.Run(ctx, g, func(e goalloop.Event) {
		fmt.Printf("[iter %d] %s: %s\n", e.Iteration, e.Phase, e.Detail)
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "autocode goal: %v\n", err)
		os.Exit(1)
	}
	persistGoalRun(loopStore, resolvedTier, decision, loopSink.Snapshot(), loop.Iterations())
	fmt.Printf("decision: complete=%v, summary=%s\n", decision.Complete, decision.Summary)
	if decision.Complete {
		os.Exit(0)
	}
	os.Exit(1)
}
```
为此需把 real-path 的 `app`/`sink` 提到函数作用域：在 `var loop *goalloop.Loop`（约 401 行）旁加：
```go
	var loop *goalloop.Loop
	var loopSink *goalloop.UsageSink
	var loopStore *store.Store
```
并把 real-path loop 分支里的局部 `sink := &goalloop.UsageSink{}` 改为 `loopSink = &goalloop.UsageSink{}`、`sink` 引用全改 `loopSink`、`app.Store` 赋给 `loopStore = app.Store`（在 bootstrap 成功后）。lightweight 分支里也用 `app.Store`（已在分支内）。

> 这些是把"两条路径各有独立收尾"改顺的机械重命名；命令行行为不变，只是 fake/real 各自完整、持久化只在 real path 触发。

- [ ] **Step 5: 确认 evaluator 集合已带 sink（无需额外 helper）**

`EvaluatorsForTier` 在 Task 4 已经接收 `sink *UsageSink` 并把它盖到 `IntentEvaluator`/`QualityEvaluator` 上，所以 Step 3 的 `goalloop.EvaluatorsForTier(resolvedTier, chatModel, sink)` 一行就完成了"每个 LLM evaluator 的 usage 进同一预算"。**不引入** `WireSink` 之类的额外 helper（YAGNI；且值类型 evaluator 无法原地改字段，`EvaluatorsForTier` 在构造时写 `Sink` 字段才是正确做法）。

此处无代码改动，仅作为核对点：确认 Step 3 用的是三参数版 `EvaluatorsForTier(tier, model, sink)`。

- [ ] **Step 6: 同步改 `resolveGoalTier` 用 `goalloop.ResolveTierFlag`**

把 `resolveGoalTier`（约 492-506 行）替换为薄包装（决策逻辑下沉到 goalloop，可单测）：
```go
// resolveGoalTier maps the -tier flag to a Tier via the shared goalloop mapping.
// "auto" runs the RuleTierer over text (forced=false); "t0".."t4" force a tier
// (forced=true). An unrecognized value exits with a usage error.
func resolveGoalTier(flagValue, text string) (goalloop.Tier, bool) {
	tier, forced, err := goalloop.ResolveTierFlag(flagValue, text)
	if err != nil {
		fmt.Fprintf(os.Stderr, "autocode goal: %v\n", err)
		os.Exit(2)
	}
	return tier, forced
}
```

- [ ] **Step 7: 构建确认**

Run:
```sh
go build -o autocode ./cmd/autocode
```
Expected: 成功（无编译错误）。若重构后有未使用 import（如 `"time"` 在 fake-only 分支），按编译器提示增删。

- [ ] **Step 8: 全量 goalloop 测试确认不破坏**

Run:
```sh
go test ./internal/agent/goalloop -v
```
Expected: 全 PASS（含 Task 1-6 的全部新测试 + 既有测试）。

- [ ] **Step 9: 提交**

```sh
git add cmd/autocode/main.go internal/agent/goalloop/evaluators.go internal/agent/goalloop/tier.go internal/agent/goalloop/tier_test.go
git commit -m "feat(goal): dispatch T0-T2 lightweight / T3-T4 loop, wire sink + persist (G03/G02)"
```

---

## Task 8: 全量回归 + smoke

**Files:**
- 无新增；运行全量测试 + vet + build + fake smoke。

- [ ] **Step 1: 全量测试**

Run:
```sh
go test ./...
```
Expected: 全 PASS（允许 CLAUDE.md 记载的预期 `t.Skip`：`e2e_real` 门禁、部分 eino/bootstrap 测试在 openai provider 不可用时 skip）。

- [ ] **Step 2: vet**

Run:
```sh
go vet ./...
```
Expected: 无输出。

- [ ] **Step 3: 构建**

Run:
```sh
go build -o autocode ./cmd/autocode
```
Expected: 成功。

- [ ] **Step 4: fake-model smoke（两轮 demo 仍可用）**

Run:
```sh
timeout 30 ./autocode goal -fake-model -max-iters 5 -goal "implement foo"
```
Expected: 打印 `[tier: ...] path: loop`（`auto` 对 "implement foo" 经 RuleTierer 选 `standard`，但 `--fake-model` 走 fake 路径，与 tier 分派无关）+ 两轮 `[iter N] Plan/Implement/Evaluate/Judge` + 最终 `decision: complete=true`，退出码 0。证明 `--fake-model` 两轮 demo 未被破坏。

- [ ] **Step 5: tier 强制 smoke（不再静默返回）**

Run:
```sh
timeout 30 ./autocode goal -fake-model -tier t2 -goal "fix typo"
```
Expected: 打印 `[tier: designed] path: lightweight`（**不再**打印旧的 "lightweight path (orchestrator + skill)" 后立即退出；现在打印 path 后继续走 fake 两轮 demo）。证明静默 return 已移除。

- [ ] **Step 6: 提交（若有零散小修）**

```sh
git add -A
git commit -m "test: G03+G02 regression green" || echo "nothing to commit"
```

---

## Self-Review（写完后自查结果）

1. **Spec 覆盖（对照 feature-comparison 行 G02/G03 的验收）**：
   - G03「T0-T4 均能产生实际结果（不再静默返回）」→ Task 7 Step 2 删除 `forced && resolvedTier<=TierDesigned` 的静默 return；real path 上 T0-T2 走 `runLightweightGoal`（真实 orchestrator turn + 文件编辑）、T3-T4 走 goalloop。✅
   - G03「auto 与强制 tier 可测」→ Task 4 `TestResolveTierFlag`（auto + t0..t4 + 非法）、`TestTier_Path`、`TestTier_SkillName`、`TestEvaluatorsForTier` 全为纯函数单测。✅
   - G03「升级规则不静默退出」→ Task 4 `EscalationHint` + Task 5 `TestLoop_MaxIterationsHasEscalationHint`（低 tier 耗尽 → Summary 带 "escalat"、`StopReason=escalate`）；T4 无提示（`TestLoop_T4MaxIterationsNoEscalationHint`）。✅
   - G02「每次模型调用累计 usage」→ Task 1 `UsageSink.Add` + Task 2/3/4 在 `LLMPlanner`/`IntentEvaluator`/`QualityEvaluator`/`LLMTierer` 的 `Generate` 后 `addUsage`，各有单测。✅
   - G02「父子 Agent 不重复计算」→ 共享单一 sink、`Add` 按独立调用累加（Task 3 `TestEvaluators_SharedSinkSumsAcrossBoth` 证明两个 evaluator 共享 sink 各计一次 = 30，非 60）。✅
   - G02「达到预算可靠停止」→ Task 5 `overBudget()` 在迭代顶部（`TestLoop_BudgetStopsOnAccumulatedUsage`）+ Plan 之后（`TestLoop_BudgetCheckMidIteration_AfterPlan`，证明 plan 后即停、不 Implement）。✅
   - G02「持久化原因」→ Task 6 `RunRecord`（带 `stop_reason` JSON tag，`TestRunRecord_RoundTripsJSON` 验证 wire form 含 `stop_reason`）+ Task 7 `persistGoalRun` 经 `store.KVSet` 写 kv。✅

2. **Placeholder 扫描**：
   - 无 TODO / "add error handling" / "similar to" 占位；每步代码完整。Task 7 Step 5 是核对点（确认用三参数 `EvaluatorsForTier`），无代码改动。
   - Task 7 Step 3-4 的 main.go 重构因函数体较长，用"定位+替换+追加 helper"描述而非整函数重贴——这是必要的（避免重复 460 行），但每段都给了精确行号 + 完整新代码。可接受。`EvaluatorsForTier` 直接在构造时盖 `Sink`（Task 4），main.go 无需 `WireSink` 之类额外 helper。

3. **类型一致性**：
   - `Usage` / `UsageSink` / `Usage.Total()` 在 Task 1 定义，Task 2/3/4/5/6 引用一致。✅
   - `addUsage(sink, msg)` 签名在 Task 1 定义，Task 2/3/4 调用一致。✅
   - `Decision.StopReason` / `Usage` + `StopReason*` 常量在 Task 5 定义，Task 6 `NewRunRecord` / Task 7 `runLightweightGoal` 引用一致。✅
   - `Config.Sink` / `Config.Tier` 在 Task 5 定义，Task 7 main.go 引用一致。✅
   - `Tier.Path()` / `SkillName()` / `EscalationHint` / `ResolveTierFlag` / `EvaluatorsForTier(t,m,sink)` 在 Task 4 定义（已带 `sink` 参数，构造 Intent/Quality 时盖 `Sink`），Task 7 main.go 一行 `EvaluatorsForTier(resolvedTier, chatModel, sink)` 直接消费。✅
   - `RunRecord` / `NewRunRecord` 在 Task 6 定义，Task 7 `persistGoalRun` 引用一致。✅

4. **已知边界（非 placeholder，是显式范围）**：
   - **lightweight 路径的 usage 不进 goalloop sink**：`app.Orch.Query` 返回 string，其 token usage 走 orchestrator 自己的 `TurnUsage`（已在 /cost 链路里），不回灌 goalloop sink。goalloop sink 只驱动 loop 路径（T3-T4）预算。若日后要让 lightweight 也计预算，用 `app.Orch.EventsWithHistory` + `orchestrator.ClassifyEventsWithUsage` 排空进 sink（本 plan 不做，显式声明）。
   - **`--fake-model` 不走 tier 分派**：保持确定性两轮 demo；tier 只被打印。tier 分派逻辑由 goalloop 纯函数单测覆盖。
   - **lightweight 路径无 evaluator**：单轮 orchestrator turn 的"完成"= turn 无错返回；它不带 test/intent 判定（那需要 goalloop 的 evaluator）。若要 lightweight 也跑 TestEvaluator，后续可在 `runLightweightGoal` 后追加一次 `TestEvaluator.Evaluate`（本 plan 不做）。
   - **持久化为 best-effort**：`persistGoalRun` 的写库错误只打 stderr，不 fail 命令（goal 结果已打印）。

## 执行交接

Plan complete and saved to `docs/superpowers/plans/2026-07-18-m1-lane3-g03-g02-goalloop.md`. 两种执行方式：

1. **Subagent-Driven（推荐）** — 每个任务派一个新 subagent，任务间 review（Task 7 改动面最大、涉及 main.go 多处替换，建议单独 review）。
2. **Inline Execution** — 本会话内按 executing-plans 批次执行 + checkpoint。
