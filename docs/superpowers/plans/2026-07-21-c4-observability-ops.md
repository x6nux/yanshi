# Batch C4 — 可观测运维 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 建立结构化日志、OpenTelemetry、feature flags、USD 成本估算和增强 doctor，使 session/turn/tool 可追踪、成本可解释、实验能力可控、运维检查可机读且不泄露敏感数据。

**Architecture:** OBS1 先建立 `internal/observe/log` 并按 guard/orchestrator/api 优先级迁移诊断输出；OBS2 在其上建立 `internal/observe/otel`，通过 context 串联 session/turn/tool span，并在现有 `RetryCallback`/`sleepRetry` 单点计数。OBS3 建 `internal/features` 注册表，并同步 `proto/frame.go`、`ws.go`、`wsbackend.go`、`StreamEvent`、TUI 与 SSE fallback。COST1 用独立计费累加器按每次 provider usage 的 `(PromptTokens-CachedTokens)`、`CachedTokens`、`CompletionTokens` 求和，绝不把覆盖语义的 `tokensIn` 当累计账本；未知模型用 `CostKnown=false` 渲染 `N/A`。O07 保留 doctor 现有数据模型与渲染器，只增量添加 MCP/LSP/permissions，并让未落地 sandbox 保持 `warn`。

**Tech Stack:** Go 1.26.4、`log/slog`、Bubble Tea、SQLite、`go.opentelemetry.io/otel`、OTLP/HTTP trace/metric exporters。

**范围:** `docs/feature-roadmap-codex-deepseek.md` Batch C4 的 OBS1、OBS2、OBS3、COST1、O07。

---

## 不变量与边界

1. 默认日志不得记录 API key、secret、authorization、prompt、messages、原始 `argsJSON`、shell command、路径、host 或 provider error body。
2. 不机械替换 doctor `RenderText`、exec stdout、SSE `event:`/`data:`、TUI render、工具结果字符串、VCS hash 序列化。
3. guard 审计只记录 `tool`、`decision`、`source`、脱敏后的 reason 分类；`tools.Authorize` 是权限决策单点，`GuardedTool.Stream` 是工具执行单点。
4. UnknownToolsHandler 继续把未知工具作为工具结果回喂模型，不改为 Go error。
5. OTel 关闭或 exporter 初始化失败时退化为 no-op；业务启动、turn、tool 不得因此失败。
6. retry metric 只接入 `internal/llm/eino/resilient.go:sleepRetry`；不新增重试循环。
7. `/features` 是 WS 控制命令；SSE `SendFrame` 明确返回 `"/features requires a WebSocket backend"`。
8. `features_set.enabled` 必须能表达 `false`，因此 wire payload 使用 `*bool`，不使用 `bool,omitempty`。
9. 每次 provider usage 只累计一次；同一 stream 的 chunk usage 是累计值，只取 stream 末值。judge usage 是独立 provider usage，也计费。
10. `tokensIn`、`cachedTokens`、`reasoningTokens` 继续保持“最新上下文覆盖”语义；新增 billed 字段才是累计账本。
11. 未知模型成本是 `N/A`，不是 `$0`；已知模型且零 token 才能显示 `$0.0000`。
12. doctor 保留 `CheckResult`、`DoctorReport`、`StatusOK/Warn/Fail`、`RenderText`、`RenderJSON`、`ExitCode`；新检查只读，sandbox 在 S08 前保持 `warn`。
13. 新依赖通过 `bootstrap.Build` 装配；不得对 `app.Server.Handler` 做类型断言回填 `PriceTab`/`FeaturesReg`。

## 文件结构

| 文件 | 职责 |
|---|---|
| `internal/observe/log/{log,redact}.go` | slog setup、context IDs、递归脱敏、安全错误分类 |
| `internal/observe/otel/{provider,metrics}.go` | OTLP provider、no-op、span/metric helpers |
| `internal/features/features.go` | flag spec/stage/default/owner/strict/runtime override |
| `internal/llm/eino/pricing.go` | 模型价格、per-usage 账本、成本计算与格式化 |
| `internal/config/config.go` | observability/features/pricing 配置 DTO |
| `internal/bootstrap/bootstrap.go` | logger/features/pricing/OTel 组合根装配与 shutdown |
| `internal/api/http/{server,ws,chat}.go` | 依赖注入、WS 累计账本、SSE 成本、features 控制帧 |
| `internal/proto/frame.go` | cost/features wire vocabulary |
| `internal/cli/{backend,wsbackend,ssebackend}.go` | wire→StreamEvent 与 SSE 明确失败 |
| `internal/store/{store,session,session_list}.go` | 5 个计费列及读写 |
| `internal/cli/tui/{commands,events,model}.go` | `/cost`、`/stats`、`/features` |
| `internal/cli/doctor.go` | 增量 MCP/LSP/permissions 检查 |

---

## Task 1: OBS1 日志叶子包——slog、context IDs 与默认脱敏

**Files:**
- Create: `internal/observe/log/log.go`
- Create: `internal/observe/log/redact.go`
- Create: `internal/observe/log/log_test.go`

- [ ] **Step 1: 写失败测试**

```go
// internal/observe/log/log_test.go
package log

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestHandlerRedactsDirectBoundNestedAndErrorAttrs(t *testing.T) {
	var buf bytes.Buffer
	logger := New(Config{Writer: &buf, Format: "json", Level: "debug"})
	logger = logger.With("api_key", "sk-ant-bound")
	logger.Info("request",
		"prompt", "private prompt",
		slog.Group("request", "tool_args", `{"path":"C:/secret"}`),
		"err", errors.New("Bearer hidden-token"),
	)
	got := buf.String()
	for _, secret := range []string{"sk-ant-bound", "private prompt", "C:/secret", "hidden-token"} {
		if strings.Contains(got, secret) {
			t.Fatalf("log leaked %q: %s", secret, got)
		}
	}
	if strings.Count(got, Redacted) < 4 {
		t.Fatalf("expected every sensitive value to be redacted: %s", got)
	}
}

func TestHandlerAddsCorrelationIDsFromContext(t *testing.T) {
	var buf bytes.Buffer
	logger := New(Config{Writer: &buf, Format: "json", Level: "info"})
	ctx := WithIDs(context.Background(), IDs{
		TraceID: "0123456789abcdef0123456789abcdef",
		SessionID: "session-1",
		TurnID: "turn-2",
		Tool: "fs_read",
	})
	logger.InfoContext(ctx, "correlated")
	got := buf.String()
	for _, want := range []string{`"trace_id":"0123456789abcdef0123456789abcdef"`, `"session_id":"session-1"`, `"turn_id":"turn-2"`, `"tool":"fs_read"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %s in %s", want, got)
		}
	}
}

func TestParseLevelAndSafeErrorType(t *testing.T) {
	if got := ParseLevel("warn"); got != slog.LevelWarn {
		t.Fatalf("ParseLevel(warn) = %v", got)
	}
	if got := SafeErrorType(errors.New("api_key=secret")); got != "*errors.errorString" {
		t.Fatalf("SafeErrorType = %q", got)
	}
	if got := SafeErrorType(nil); got != "" {
		t.Fatalf("SafeErrorType(nil) = %q", got)
	}
}
```

- [ ] **Step 2: 运行测试，确认因包尚不存在而失败**

Run: `go test ./internal/observe/log -v`

Expected: FAIL，报告 `New`、`Config`、`WithIDs`、`IDs`、`Redacted` 未定义。

- [ ] **Step 3: 实现递归脱敏**

```go
// internal/observe/log/redact.go
package log

import (
	"fmt"
	"log/slog"
	"strings"
	"unicode"
)

// Redacted is the only replacement written for sensitive values.
const Redacted = "[REDACTED]"

var sensitiveKeys = map[string]struct{}{
	"apikey": {}, "authorization": {}, "password": {}, "secret": {},
	"token": {}, "accesstoken": {}, "refreshtoken": {},
	"prompt": {}, "messages": {}, "input": {},
	"args": {}, "argsjson": {}, "toolargs": {}, "arguments": {},
	"command": {}, "path": {}, "paths": {}, "host": {}, "url": {},
	"headers": {}, "query": {},
}

func normalizedKey(key string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, key)
}

func sensitiveKey(key string) bool {
	_, ok := sensitiveKeys[normalizedKey(key)]
	return ok
}

func looksSensitiveValue(value string) bool {
	v := strings.TrimSpace(strings.ToLower(value))
	return strings.HasPrefix(v, "sk-") ||
		strings.HasPrefix(v, "bearer ") ||
		strings.HasPrefix(v, "xoxb-") ||
		strings.HasPrefix(v, "xoxp-") ||
		strings.Contains(v, "api_key=") ||
		strings.Contains(v, "authorization:")
}

// redactAttr recursively sanitizes groups and also handles error values stored
// through slog.Any. It is used both at Handle time and before WithAttrs binds
// attributes, so pre-bound secrets cannot bypass the handler.
func redactAttr(attr slog.Attr) slog.Attr {
	attr.Value = attr.Value.Resolve()
	if sensitiveKey(attr.Key) {
		return slog.String(attr.Key, Redacted)
	}
	if attr.Value.Kind() == slog.KindGroup {
		children := attr.Value.Group()
		clean := make([]slog.Attr, 0, len(children))
		for _, child := range children {
			clean = append(clean, redactAttr(child))
		}
		return slog.Attr{Key: attr.Key, Value: slog.GroupValue(clean...)}
	}
	if attr.Value.Kind() == slog.KindString && looksSensitiveValue(attr.Value.String()) {
		return slog.String(attr.Key, Redacted)
	}
	if attr.Value.Kind() == slog.KindAny {
		if _, ok := attr.Value.Any().(error); ok {
			return slog.String(attr.Key, Redacted)
		}
		if looksSensitiveValue(fmt.Sprint(attr.Value.Any())) {
			return slog.String(attr.Key, Redacted)
		}
	}
	return attr
}

func redactAttrs(attrs []slog.Attr) []slog.Attr {
	clean := make([]slog.Attr, len(attrs))
	for i, attr := range attrs {
		clean[i] = redactAttr(attr)
	}
	return clean
}
```

- [ ] **Step 4: 实现 logger、correlation context 与安全错误分类**

```go
// internal/observe/log/log.go
// Package log configures yanshi's process-wide structured logger. It injects
// correlation IDs from context and redacts sensitive values before any handler
// can serialize them.
package log

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

// Config controls the process logger. Zero values mean info/json/os.Stderr.
type Config struct {
	Level string
	Format string
	Writer io.Writer
}

// IDs are safe correlation fields. They deliberately exclude prompts, tool
// arguments, commands, paths, hosts, and provider error text.
type IDs struct {
	TraceID string
	SessionID string
	TurnID string
	Tool string
}

type idsKey struct{}

// WithIDs merges IDs with an existing context value.
func WithIDs(ctx context.Context, ids IDs) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	current := IDsFromContext(ctx)
	if ids.TraceID != "" { current.TraceID = ids.TraceID }
	if ids.SessionID != "" { current.SessionID = ids.SessionID }
	if ids.TurnID != "" { current.TurnID = ids.TurnID }
	if ids.Tool != "" { current.Tool = ids.Tool }
	return context.WithValue(ctx, idsKey{}, current)
}

// IDsFromContext returns the bound correlation fields.
func IDsFromContext(ctx context.Context) IDs {
	if ctx == nil { return IDs{} }
	ids, _ := ctx.Value(idsKey{}).(IDs)
	return ids
}

// NewTraceID returns a 16-byte lowercase hex identifier compatible in length
// with an OpenTelemetry trace ID. Entropy failure degrades to an empty ID.
func NewTraceID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil { return "" }
	return hex.EncodeToString(raw[:])
}

// NewTurnID returns an independent 8-byte correlation identifier.
func NewTurnID() string {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil { return "" }
	return hex.EncodeToString(raw[:])
}

// ParseLevel maps user configuration to slog levels; unknown values are info.
func ParseLevel(level string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug": return slog.LevelDebug
	case "warn", "warning": return slog.LevelWarn
	case "error": return slog.LevelError
	default: return slog.LevelInfo
	}
}

type redactHandler struct{ inner slog.Handler }

func (h *redactHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *redactHandler) Handle(ctx context.Context, record slog.Record) error {
	clean := slog.NewRecord(record.Time, record.Level, record.Message, record.PC)
	record.Attrs(func(attr slog.Attr) bool {
		clean.AddAttrs(redactAttr(attr))
		return true
	})
	ids := IDsFromContext(ctx)
	if ids.TraceID != "" { clean.AddAttrs(slog.String("trace_id", ids.TraceID)) }
	if ids.SessionID != "" { clean.AddAttrs(slog.String("session_id", ids.SessionID)) }
	if ids.TurnID != "" { clean.AddAttrs(slog.String("turn_id", ids.TurnID)) }
	if ids.Tool != "" { clean.AddAttrs(slog.String("tool", ids.Tool)) }
	return h.inner.Handle(ctx, clean)
}

func (h *redactHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &redactHandler{inner: h.inner.WithAttrs(redactAttrs(attrs))}
}

func (h *redactHandler) WithGroup(name string) slog.Handler {
	return &redactHandler{inner: h.inner.WithGroup(name)}
}

// New creates a redacting structured logger.
func New(cfg Config) *slog.Logger {
	writer := cfg.Writer
	if writer == nil { writer = os.Stderr }
	opts := &slog.HandlerOptions{Level: ParseLevel(cfg.Level)}
	var inner slog.Handler
	if strings.EqualFold(cfg.Format, "text") {
		inner = slog.NewTextHandler(writer, opts)
	} else {
		inner = slog.NewJSONHandler(writer, opts)
	}
	return slog.New(&redactHandler{inner: inner})
}

// Setup installs the redacting logger as slog.Default.
func Setup(cfg Config) *slog.Logger {
	logger := New(cfg)
	slog.SetDefault(logger)
	return logger
}

// SafeErrorType returns only the concrete type, never err.Error(). Provider
// errors often contain response bodies, prompts, or credentials.
func SafeErrorType(err error) string {
	if err == nil { return "" }
	return fmt.Sprintf("%T", err)
}

// WarnErr logs a diagnostic without serializing the error body.
func WarnErr(ctx context.Context, message string, err error, attrs ...slog.Attr) {
	if err == nil { return }
	attrs = append(attrs, slog.String("error_type", SafeErrorType(err)))
	slog.LogAttrs(ctx, slog.LevelWarn, message, attrs...)
}
```

- [ ] **Step 5: 运行测试并提交**

Run: `go test ./internal/observe/log -v`

Expected: PASS；JSON 中不出现测试 secret，且 bound/group/error attr 全部被替换。

```bash
git add internal/observe/log/log.go internal/observe/log/redact.go internal/observe/log/log_test.go
git commit -m "feat(observe): add redacting structured logger"
```

---

## Task 2: 配置 DTO——observability、features 与 pricing override

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`
- Modify: `config.example.yaml`

- [ ] **Step 1: 写配置解析测试**

```go
// append to internal/config/config_test.go
func TestLoadObservabilityFeaturesAndPricing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := `
server: { http_addr: "127.0.0.1:0" }
storage: { sqlite_path: ":memory:" }
observability:
  log: { level: "debug", format: "text" }
  otel:
    enabled: true
    endpoint: "http://127.0.0.1:4318"
    service_name: "yanshi-test"
    sample_ratio: 0.25
features:
  strict: true
  overrides:
    observe.otel_export: true
pricing:
  overrides:
    custom-model:
      input_per_million: 2
      cache_hit_per_million: 0.2
      output_per_million: 8
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil { t.Fatal(err) }
	if cfg.Observability.Log.Level != "debug" || cfg.Observability.Log.Format != "text" {
		t.Fatalf("log config = %+v", cfg.Observability.Log)
	}
	if !cfg.Observability.OTel.Enabled || cfg.Observability.OTel.SampleRatio != 0.25 {
		t.Fatalf("otel config = %+v", cfg.Observability.OTel)
	}
	if !cfg.Features.Strict || !cfg.Features.Overrides["observe.otel_export"] {
		t.Fatalf("features config = %+v", cfg.Features)
	}
	price := cfg.Pricing.Overrides["custom-model"]
	if price.InputPerM != 2 || price.CacheHitPerM != 0.2 || price.OutputPerM != 8 {
		t.Fatalf("pricing override = %+v", price)
	}
}

func TestLoadObservabilityDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("storage: { sqlite_path: ':memory:' }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil { t.Fatal(err) }
	if cfg.Observability.Log.Level != "info" || cfg.Observability.Log.Format != "json" {
		t.Fatalf("log defaults = %+v", cfg.Observability.Log)
	}
	if cfg.Observability.OTel.ServiceName != "yanshi" {
		t.Fatalf("service default = %q", cfg.Observability.OTel.ServiceName)
	}
}
```

在现有 import 块加入：

```go
import (
	"os"
	"path/filepath"
	"testing"
)
```

- [ ] **Step 2: 运行测试，确认字段未定义**

Run: `go test ./internal/config -run 'TestLoadObservability' -v`

Expected: FAIL，`Config.Observability`、`Config.Features`、`Config.Pricing` 未定义。

- [ ] **Step 3: 用下面的完整结构替换 `Config`，并加入导出 DTO**

```go
type Config struct {
	Server ServerConfig `yaml:"server"`
	Storage StorageConfig `yaml:"storage"`
	Token string `yaml:"token"`
	LLM LLMConfig `yaml:"llm"`
	Agents []AgentConfig `yaml:"agents"`
	Profiles map[string]guard.PermissionProfile `yaml:"profiles"`
	Skills SkillsConfig `yaml:"skills"`
	VCS VCSConfig `yaml:"vcs"`
	Compaction CompactionConfig `yaml:"compaction"`
	Observability ObservabilityConfig `yaml:"observability"`
	Features FeaturesConfig `yaml:"features"`
	Pricing PricingConfig `yaml:"pricing"`
}

// ObservabilityConfig groups process logging and OpenTelemetry settings.
type ObservabilityConfig struct {
	Log LogConfig `yaml:"log"`
	OTel OTelConfig `yaml:"otel"`
}

type LogConfig struct {
	Level string `yaml:"level"`
	Format string `yaml:"format"`
}

type OTelConfig struct {
	Enabled bool `yaml:"enabled"`
	Endpoint string `yaml:"endpoint"`
	ServiceName string `yaml:"service_name"`
	SampleRatio float64 `yaml:"sample_ratio"`
}

type FeaturesConfig struct {
	Strict bool `yaml:"strict"`
	Overrides map[string]bool `yaml:"overrides"`
}

type PricingConfig struct {
	Overrides map[string]ModelPricingOverride `yaml:"overrides"`
}

// ModelPricingOverride is config's transport-neutral YAML DTO. bootstrap
// converts it to einollm.ModelPricing so config remains a leaf package.
type ModelPricingOverride struct {
	InputPerM float64 `yaml:"input_per_million"`
	CacheHitPerM float64 `yaml:"cache_hit_per_million"`
	OutputPerM float64 `yaml:"output_per_million"`
}
```

- [ ] **Step 4: 在现有 `applyDefaults` 末尾加入确定默认值**

```go
func (c *Config) applyDefaults() {
	if c.Compaction.Threshold == 0 { c.Compaction.Threshold = 0.8 }
	if c.Compaction.KeepRecent == 0 { c.Compaction.KeepRecent = 4 }
	if c.Compaction.ContextWindow == 0 { c.Compaction.ContextWindow = 256000 }
	if c.Compaction.ChunkThreshold == 0 { c.Compaction.ChunkThreshold = 0.9 }
	if c.Observability.Log.Level == "" { c.Observability.Log.Level = "info" }
	if c.Observability.Log.Format == "" { c.Observability.Log.Format = "json" }
	if c.Observability.OTel.ServiceName == "" { c.Observability.OTel.ServiceName = "yanshi" }
	if c.Observability.OTel.Enabled && c.Observability.OTel.SampleRatio == 0 {
		c.Observability.OTel.SampleRatio = 1
	}
}
```

- [ ] **Step 5: 在 `config.example.yaml` 末尾加入真实配置示例**

```yaml
observability:
  log:
    level: info
    format: json
  otel:
    enabled: false
    endpoint: "http://127.0.0.1:4318"
    service_name: yanshi
    sample_ratio: 1.0

features:
  strict: false
  overrides: {}

pricing:
  overrides: {}
```

- [ ] **Step 6: 运行测试并提交**

Run: `go test ./internal/config -v`

Expected: PASS；空配置得到 info/json/yanshi 默认值，override 字段精确解析。

```bash
git add internal/config/config.go internal/config/config_test.go config.example.yaml
git commit -m "feat(config): add observability features and pricing settings"
```

---

## Task 3: OBS1 装配与首批迁移——bootstrap/session/guard/orchestrator/api

**Files:**
- Modify: `internal/bootstrap/bootstrap.go`
- Modify: `internal/cli/session.go`
- Modify: `internal/tools/permctx.go`
- Modify: `internal/tools/permctx_test.go`
- Modify: `internal/agent/orchestrator/orchestrator.go`
- Modify: `internal/api/http/ws.go`
- Test: existing package tests plus `internal/tools/permctx_test.go`

- [ ] **Step 1: 写 guard 审计失败测试（验证不记录原始参数）**

```go
// append to internal/tools/permctx_test.go
func TestAuthorizeLogsDecisionWithoutArguments(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(obslog.New(obslog.Config{Writer: &buf, Format: "json", Level: "debug"}))
	defer slog.SetDefault(old)

	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_read"}},
	})
	if err := Authorize(ctx, guard.Action{Tool: "fs_read"}, `{"path":"C:/secret","api_key":"sk-test"}`); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if !strings.Contains(got, `"decision":"allow"`) || !strings.Contains(got, `"tool":"fs_read"`) {
		t.Fatalf("missing audit fields: %s", got)
	}
	for _, forbidden := range []string{"C:/secret", "sk-test", "argsJSON"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("audit leaked %q: %s", forbidden, got)
		}
	}
}
```

在测试 import 块加入：

```go
import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/guard"
	obslog "github.com/x6nux/yanshi/internal/observe/log"
)
```

- [ ] **Step 2: 运行测试，确认缺少审计日志**

Run: `go test ./internal/tools -run TestAuthorizeLogsDecisionWithoutArguments -v`

Expected: FAIL，输出不含 `decision`。

- [ ] **Step 3: 用下面完整函数替换 `Authorize`，并加入审计 helper**

```go
func auditPermission(ctx context.Context, tool, decision, source, reasonCode string) {
	attrs := []slog.Attr{
		slog.String("tool", tool),
		slog.String("decision", decision),
		slog.String("source", source),
	}
	if reasonCode != "" {
		attrs = append(attrs, slog.String("reason_code", reasonCode))
	}
	slog.LogAttrs(ctx, slog.LevelInfo, "permission decision", attrs...)
}

// Authorize is the single guard decision point. argsJSON is used only for the
// interactive PermissionRequest shown to the user and is never logged.
func Authorize(ctx context.Context, action guard.Action, argsJSON string) error {
	prof, ok := ProfileFromContext(ctx)
	if !ok {
		auditPermission(ctx, action.Tool, "deny", "fail_closed", "missing_profile")
		return &DenyErr{Reason: "no permission profile in context"}
	}
	if al := allowlistFrom(ctx); al != nil && al.allows(allowKey(action)) {
		auditPermission(ctx, action.Tool, "allow", "session_allowlist", "")
		return nil
	}
	dec := guard.New().Check(prof, action)
	if dec.Allowed {
		auditPermission(ctx, action.Tool, "allow", "static_profile", "")
		return nil
	}
	ask, hasCallback := permissionCallback(ctx)
	if hasCallback {
		req := PermissionRequest{Tool: action.Tool, Args: argsJSON, Reason: dec.Reason}
		switch ask(req) {
		case PermissionAllow:
			auditPermission(ctx, action.Tool, "allow", "interactive_once", "static_denied")
			return nil
		case PermissionAlwaysAllow:
			if al := allowlistFrom(ctx); al != nil { al.record(allowKey(action)) }
			auditPermission(ctx, action.Tool, "allow", "interactive_always", "static_denied")
			return nil
		default:
			auditPermission(ctx, action.Tool, "deny", "interactive", "user_denied")
			return &DenyErr{Reason: dec.Reason}
		}
	}
	auditPermission(ctx, action.Tool, "deny", "static_profile", "policy_denied")
	return &DenyErr{Reason: dec.Reason}
}
```

在 `internal/tools/permctx.go` import 块加入 `"log/slog"`。注意代码刻意**不**记录 `dec.Reason`、`argsJSON`、FS paths、shell、host；`reason_code` 是固定词表。

- [ ] **Step 4: 在 `bootstrap.Build` 最前装默认 logger，加载配置后按配置重装**

用下面代码替换 `Build` 的前三行：

```go
func Build(opts Options) (*App, error) {
	// Install a safe logger before config loading so even load failures and all
	// later soft-degradation paths use the redacting handler.
	obslog.Setup(obslog.Config{Level: "info", Format: "json"})
	cfg, err := config.Load(opts.ConfigPath)
	if err != nil {
		return nil, fmt.Errorf("bootstrap: load config: %w", err)
	}
	obslog.Setup(obslog.Config{
		Level: cfg.Observability.Log.Level,
		Format: cfg.Observability.Log.Format,
	})

	st, err := store.Open(cfg.Storage.SQLitePath)
	if err != nil {
		return nil, fmt.Errorf("bootstrap: open store: %w", err)
	}
```

在 import 块加入：

```go
obslog "github.com/x6nux/yanshi/internal/observe/log"
```

- [ ] **Step 5: 精确迁移两个 bootstrap 软降级和 session 后台错误**

将 `bootstrap.go` 的两个 `fmt.Fprintf(os.Stderr, ...)` 分别替换为：

```go
obslog.WarnErr(context.Background(), "vcs initialization failed; tracking disabled", vcsErr)
```

```go
obslog.WarnErr(context.Background(), "skill plugin discovery failed; plugin skills disabled", err)
```

将 `internal/cli/session.go` 的 `fmt.Fprintln(os.Stderr, "serve:", err)` 替换为：

```go
obslog.WarnErr(context.Background(), "in-process server stopped", err)
```

并从该文件删除不再使用的 `fmt`/`os` import，加入：

```go
obslog "github.com/x6nux/yanshi/internal/observe/log"
```

`cmd/yanshi/main.go` 的 unknown subcommand、connected/prompt、tool progress、doctor render、exec protocol、goal usage/error 文本继续直接写 stderr：这些是 CLI 合约，不是内部日志。

- [ ] **Step 6: 在 WS turn context 注入 session/turn/trace IDs，并加安全 start/end 日志**

在 `case "user_message"` 中完成 `ensureSession` 后、任何 model 调用前加入：

```go
turnIDs := obslog.IDs{
	TraceID: obslog.NewTraceID(),
	SessionID: cs.sessionID,
	TurnID: obslog.NewTurnID(),
}
turnCtx = obslog.WithIDs(turnCtx, turnIDs)
slog.InfoContext(turnCtx, "turn started",
	"model", cs.displayModel(),
	"thinking", cs.thinking != "",
	"has_output_schema", len(cf.OutputSchema) > 0,
)
```

在写 `done` 前加入（不记录 assistant text 或 error body）：

```go
slog.InfoContext(turnCtx, "turn finished",
	"model", cs.displayModel(),
	"turns", cs.turns,
	"tool_calls", cs.toolCalls,
	"completion_tokens", usage.CompletionTokens+judgeCompletionTokens,
)
```

`internal/api/http/ws.go` import 块加入：

```go
"log/slog"
obslog "github.com/x6nux/yanshi/internal/observe/log"
```

- [ ] **Step 7: 让非 WS orchestrator 入口补齐缺失 IDs，不改变 context 注入顺序**

在 `orchestrator.go` 加：

```go
func ensureTurnIDs(ctx context.Context) context.Context {
	ids := obslog.IDsFromContext(ctx)
	if ids.TraceID == "" { ids.TraceID = obslog.NewTraceID() }
	if ids.TurnID == "" { ids.TurnID = obslog.NewTurnID() }
	return obslog.WithIDs(ctx, ids)
}

func (o *Orchestrator) prepareTurnContext(ctx context.Context) context.Context {
	ctx = ensureTurnIDs(ctx)
	ctx = tools.WithProfile(ctx, o.profile)
	ctx = tools.WithWorkRoot(ctx, o.workRoot)
	ctx = o.bindSubAgentRunner(ctx)
	if o.vcsScope.VCS != nil { ctx = tools.WithVCS(ctx, o.vcsScope) }
	return ctx
}
```

随后把 `Query`、`Events`、`EventsWithHistory`、`EventsWithHistoryOpts` 各自重复的 profile/workroot/subagent/VCS 注入块替换为：

```go
ctx = o.prepareTurnContext(ctx)
```

这保持 CLAUDE.md 规定的注入顺序；不引用不存在的 `o.cfg.ModelName`、`sessionID` 或 telemetry 字段。

- [ ] **Step 8: 运行定向测试并提交**

Run: `go test ./internal/observe/log ./internal/tools ./internal/agent/orchestrator ./internal/api/http ./internal/bootstrap ./internal/cli -v`

Expected: PASS；guard 允许/拒绝语义、UnknownToolsHandler、WS turn、session shutdown 均不回归。

```bash
git add internal/bootstrap/bootstrap.go internal/cli/session.go internal/tools/permctx.go internal/tools/permctx_test.go internal/agent/orchestrator/orchestrator.go internal/api/http/ws.go
git commit -m "feat(observability): wire slog and audit turn permissions"
```

---

## Task 4: OBS3 核心——feature registry、stage/default/owner 与 strict

**Files:**
- Create: `internal/features/features.go`
- Create: `internal/features/features_test.go`

- [ ] **Step 1: 写失败测试**

```go
// internal/features/features_test.go
package features

import (
	"strings"
	"testing"
)

func TestRegistryDefaultsRuntimeSetAndList(t *testing.T) {
	r := NewRegistry(false)
	for _, spec := range DefaultSpecs() { r.Register(spec) }
	if !r.Enabled("observe.slog_trace_id") { t.Fatal("stable default must be enabled") }
	if r.Enabled("observe.otel_export") { t.Fatal("experimental default must be disabled") }
	if err := r.Set("observe.otel_export", true); err != nil { t.Fatal(err) }
	if !r.Enabled("observe.otel_export") { t.Fatal("runtime set did not apply") }
	rows := r.List()
	if len(rows) != len(DefaultSpecs()) { t.Fatalf("rows = %d", len(rows)) }
	for _, row := range rows {
		if row.Owner == "" || row.Stage == "" { t.Fatalf("incomplete row: %+v", row) }
	}
}

func TestRegistryStrictRejectsUnknownByNameAtomically(t *testing.T) {
	r := NewRegistry(true)
	r.Register(Spec{Key: "known", Stage: Stable, Default: false, Owner: "test"})
	err := r.ApplyMap(map[string]bool{"known": true, "typo_flag": true})
	if err == nil || !strings.Contains(err.Error(), "typo_flag") {
		t.Fatalf("expected named unknown error, got %v", err)
	}
	if r.Enabled("known") { t.Fatal("strict batch must be atomic") }
}

func TestRegistryNonStrictIgnoresUnknown(t *testing.T) {
	r := NewRegistry(false)
	r.Register(Spec{Key: "known", Stage: Beta, Default: false, Owner: "test"})
	if err := r.ApplyMap(map[string]bool{"known": true, "ignored": true}); err != nil { t.Fatal(err) }
	if !r.Enabled("known") || r.Enabled("ignored") { t.Fatalf("unexpected state: %+v", r.List()) }
}

func TestRegistrySetAlwaysRejectsUnknown(t *testing.T) {
	r := NewRegistry(false)
	if err := r.Set("missing", true); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("runtime set must reject unknown key: %v", err)
	}
}
```

- [ ] **Step 2: 运行测试，确认包未定义**

Run: `go test ./internal/features -v`

Expected: FAIL，包或构造器未定义。

- [ ] **Step 3: 实现完整 registry**

```go
// internal/features/features.go
// Package features owns process-scoped feature flags. Defaults and metadata are
// registered in bootstrap; YAML overrides apply at startup; /features applies
// non-persistent runtime overrides.
package features

import (
	"fmt"
	"os"
	"sort"
	"sync"
)

type Stage string

const (
	Stable Stage = "stable"
	Beta Stage = "beta"
	Experimental Stage = "experimental"
)

type Spec struct {
	Key string
	Stage Stage
	Default bool
	Owner string
}

type Row struct {
	Key string
	Stage string
	Enabled bool
	Owner string
}

type Registry struct {
	mu sync.RWMutex
	strict bool
	specs map[string]Spec
	values map[string]bool
}

// NewRegistry combines YAML strict mode with YANSHI_FEATURES_STRICT=1.
func NewRegistry(strict bool) *Registry {
	return &Registry{
		strict: strict || os.Getenv("YANSHI_FEATURES_STRICT") == "1",
		specs: make(map[string]Spec),
		values: make(map[string]bool),
	}
}

func DefaultSpecs() []Spec {
	return []Spec{
		{Key: "observe.slog_trace_id", Stage: Stable, Default: true, Owner: "C4/OBS1"},
		{Key: "observe.otel_export", Stage: Experimental, Default: false, Owner: "C4/OBS2"},
		{Key: "observe.cost_in_status", Stage: Beta, Default: true, Owner: "C4/COST1"},
	}
}

func (r *Registry) Register(spec Spec) {
	if spec.Key == "" || spec.Owner == "" || spec.Stage == "" {
		panic("features: key, stage, and owner are required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.specs[spec.Key]; exists { panic("features: duplicate flag " + spec.Key) }
	r.specs[spec.Key] = spec
	r.values[spec.Key] = spec.Default
}

func (r *Registry) Enabled(key string) bool {
	if r == nil { return false }
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, known := r.specs[key]
	return known && r.values[key]
}

// Set is used by /features and always rejects unknown keys. Non-strict only
// changes startup ApplyMap behavior; it never invents runtime flags.
func (r *Registry) Set(key string, enabled bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, known := r.specs[key]; !known {
		return fmt.Errorf("features: unknown flag %q", key)
	}
	r.values[key] = enabled
	return nil
}

// ApplyMap is atomic in strict mode. In non-strict mode unknown config keys are
// ignored so a newer config can be used by an older binary.
func (r *Registry) ApplyMap(overrides map[string]bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.strict {
		for key := range overrides {
			if _, known := r.specs[key]; !known {
				return fmt.Errorf("features: unknown flag %q (strict mode)", key)
			}
		}
	}
	for key, enabled := range overrides {
		if _, known := r.specs[key]; known { r.values[key] = enabled }
	}
	return nil
}

func (r *Registry) List() []Row {
	if r == nil { return nil }
	r.mu.RLock()
	defer r.mu.RUnlock()
	rows := make([]Row, 0, len(r.specs))
	for key, spec := range r.specs {
		rows = append(rows, Row{Key: key, Stage: string(spec.Stage), Enabled: r.values[key], Owner: spec.Owner})
	}
	rank := map[Stage]int{Stable: 0, Beta: 1, Experimental: 2}
	sort.Slice(rows, func(i, j int) bool {
		left, right := rank[Stage(rows[i].Stage)], rank[Stage(rows[j].Stage)]
		if left != right { return left < right }
		return rows[i].Key < rows[j].Key
	})
	return rows
}
```

- [ ] **Step 4: 运行测试并提交**

Run: `go test ./internal/features -v`

Expected: PASS；strict error 明确包含未知 flag 名，非 strict 不把未知 key 放入 registry。

```bash
git add internal/features/features.go internal/features/features_test.go
git commit -m "feat(features): add staged strict feature registry"
```

---

## Task 5: COST1 单价表与 per-usage 账本

**Files:**
- Create: `internal/llm/eino/pricing.go`
- Create: `internal/llm/eino/pricing_test.go`

- [ ] **Step 1: 写失败测试，覆盖真实模型 ID 与 cache 价、未知模型、账本累加**

```go
// internal/llm/eino/pricing_test.go
package eino

import (
	"math"
	"testing"
)

func TestDefaultPricingRealAnthropicModels(t *testing.T) {
	tab := DefaultPricing()
	cases := map[string]ModelPricing{
		"claude-fable-5": {InputPerM: 10, CacheHitPerM: 1, OutputPerM: 50},
		"claude-mythos-5": {InputPerM: 10, CacheHitPerM: 1, OutputPerM: 50},
		"claude-opus-4-8": {InputPerM: 5, CacheHitPerM: 0.5, OutputPerM: 25},
		"claude-opus-4-7": {InputPerM: 5, CacheHitPerM: 0.5, OutputPerM: 25},
		"claude-opus-4-6": {InputPerM: 5, CacheHitPerM: 0.5, OutputPerM: 25},
		"claude-sonnet-5": {InputPerM: 3, CacheHitPerM: 0.3, OutputPerM: 15},
		"claude-sonnet-4-6": {InputPerM: 3, CacheHitPerM: 0.3, OutputPerM: 15},
		"claude-haiku-4-5": {InputPerM: 1, CacheHitPerM: 0.1, OutputPerM: 5},
	}
	for model, want := range cases {
		got, ok := tab[model]
		if !ok { t.Fatalf("model %q missing from DefaultPricing", model) }
		if got.InputPerM != want.InputPerM || got.OutputPerM != want.OutputPerM || got.CacheHitPerM != want.CacheHitPerM {
			t.Fatalf("model %s pricing = %+v want %+v", model, got, want)
		}
	}
	for _, forbidden := range []string{"claude-opus-4-5", "claude-sonnet-4-8", "claude-3-7-sonnet"} {
		if _, ok := tab[forbidden]; ok { t.Fatalf("forbidden legacy id %q in table", forbidden) }
	}
}

func TestCostKnownSplitsCacheAndOutput(t *testing.T) {
	tab := DefaultPricing()
	price := tab["claude-opus-4-8"]
	plain, _ := CostOK(tab, "claude-opus-4-8", Usage{Prompt: 1_000_000, Completion: 1_000_000})
	if math.Abs(plain-(price.InputPerM+price.OutputPerM)) > 1e-9 {
		t.Fatalf("plain cost = %v", plain)
	}
	cached, _ := CostOK(tab, "claude-opus-4-8", Usage{Prompt: 1_000_000, Cached: 1_000_000})
	if cached >= plain { t.Fatalf("cached cost %v must be cheaper than %v", cached, plain) }
}

func TestCostUnknownModelReportsNA(t *testing.T) {
	tab := DefaultPricing()
	cost, known := CostOK(tab, "acme-proprietary-1", Usage{Prompt: 100, Completion: 50})
	if known || cost != 0 { t.Fatalf("unknown model must be (0,false), got (%v,%v)", cost, known) }
}

func TestLedgerAccumulatesPerProviderUsage(t *testing.T) {
	tab := DefaultPricing()
	price := tab["claude-sonnet-5"]
	var ledger Ledger
	ledger.Add(Usage{Prompt: 8000, Cached: 6000, Completion: 1200})
	ledger.Add(Usage{Prompt: 12000, Cached: 11000, Completion: 3000})
	cost, known := ledger.Cost(tab, "claude-sonnet-5")
	if !known { t.Fatal("sonnet-5 must be known") }
	wantInput := (2000 + 1000) / 1_000_000.0 * price.InputPerM
	wantCache := (6000 + 11000) / 1_000_000.0 * price.CacheHitPerM
	wantOutput := (1200 + 3000) / 1_000_000.0 * price.OutputPerM
	if math.Abs(cost-(wantInput+wantCache+wantOutput)) > 1e-9 {
		t.Fatalf("ledger cost = %v want %v", cost, wantInput+wantCache+wantOutput)
	}
	if ledger.Billed.CachedTokens != 17000 || ledger.Billed.InputTokens != 3000 || ledger.Billed.OutputTokens != 4200 {
		t.Fatalf("ledger billed = %+v", ledger.Billed)
	}
}

func TestLedgerReportsNAForUnknownModel(t *testing.T) {
	var ledger Ledger
	ledger.Add(Usage{Prompt: 500, Completion: 100})
	_, known := ledger.Cost(DefaultPricing(), "unknown-model")
	if known { t.Fatal("unknown model must not be known") }
}

func TestFormatCostRanges(t *testing.T) {
	cases := map[float64]string{
		0: "N/A",
		0.00001: "<$0.0001",
		0.001: "$0.0010",
		0.1: "$0.100",
		12.345: "$12.35",
	}
	for in, want := range cases {
		if got := FormatCost(in, true); got != want {
			t.Fatalf("FormatCost(%v, known=true) = %q want %q", in, got, want)
		}
	}
	if got := FormatCost(0.4231, false); got != "N/A" {
		t.Fatalf("FormatCost known=false must be N/A, got %q", got)
	}
}

func TestMergePricingKeepsOverlayAndBase(t *testing.T) {
	base := DefaultPricing()
	overlay := map[string]ModelPricing{
		"custom": {InputPerM: 2, CacheHitPerM: 0.2, OutputPerM: 8},
		"claude-opus-4-8": {InputPerM: 9, CacheHitPerM: 0.9, OutputPerM: 90},
	}
	merged := MergePricing(base, overlay)
	if merged["custom"].InputPerM != 2 { t.Fatalf("overlay custom missing") }
	if merged["claude-opus-4-8"].InputPerM != 9 { t.Fatalf("overlay must win") }
	if merged["claude-haiku-4-5"].InputPerM != 1 { t.Fatalf("base entry lost") }
	if base["claude-opus-4-8"].InputPerM != 5 { t.Fatalf("base mutated") }
}
```

- [ ] **Step 2: 运行测试，确认符号未定义**

Run: `go test ./internal/llm/eino -run 'Pricing|Cost|Ledger|MergePricing|FormatCost' -v`

Expected: FAIL，`DefaultPricing`、`CostOK`、`Ledger`、`FormatCost`、`MergePricing` 未定义。

- [ ] **Step 3: 实现单价表、Ledger 与格式化器**

```go
// internal/llm/eino/pricing.go
package eino

import "fmt"

// ModelPricing is the per-million-token USD price for one model. CacheHitPerM
// is the discount for prompt-cache hits (Anthropic cache_read ~0.1x input).
type ModelPricing struct {
	InputPerM float64
	CacheHitPerM float64
	OutputPerM float64
}

// Usage is the minimum projection of orchestrator.TurnUsage that pricing needs.
// eino is a leaf package and must not import orchestrator; bootstrap and the WS
// handler perform the field mapping.
type Usage struct {
	Prompt int
	Cached int
	Completion int
	Reasoning int
}

// DefaultPricing returns the fallback table. Prices reflect Anthropic's publicly
// published pricing (claude-api skill, cached 2026-07-21). Users override with
// config Pricing.Overrides. The exact model IDs are authoritative; do not
// append date suffixes or fabricate legacy IDs.
func DefaultPricing() map[string]ModelPricing {
	return map[string]ModelPricing{
		"claude-fable-5": {InputPerM: 10, CacheHitPerM: 1, OutputPerM: 50},
		"claude-mythos-5": {InputPerM: 10, CacheHitPerM: 1, OutputPerM: 50},
		"claude-opus-4-8": {InputPerM: 5, CacheHitPerM: 0.5, OutputPerM: 25},
		"claude-opus-4-7": {InputPerM: 5, CacheHitPerM: 0.5, OutputPerM: 25},
		"claude-opus-4-6": {InputPerM: 5, CacheHitPerM: 0.5, OutputPerM: 25},
		"claude-sonnet-5": {InputPerM: 3, CacheHitPerM: 0.3, OutputPerM: 15},
		"claude-sonnet-4-6": {InputPerM: 3, CacheHitPerM: 0.3, OutputPerM: 15},
		"claude-haiku-4-5": {InputPerM: 1, CacheHitPerM: 0.1, OutputPerM: 5},
	}
}

func MergePricing(base, overlay map[string]ModelPricing) map[string]ModelPricing {
	merged := make(map[string]ModelPricing, len(base)+len(overlay))
	for key, price := range base { merged[key] = price }
	for key, price := range overlay { merged[key] = price }
	return merged
}

// CostOK returns the dollar estimate for one usage and whether the model is
// known. Unknown models always return (0, false) so callers render N/A, never
// $0.00 for something they could not price.
func CostOK(tab map[string]ModelPricing, model string, u Usage) (float64, bool) {
	price, ok := tab[model]
	if !ok { return 0, false }
	return computeCost(price, u), true
}

func computeCost(price ModelPricing, u Usage) float64 {
	cached := clampNonNeg(u.Cached)
	if cached > u.Prompt { cached = u.Prompt }
	miss := u.Prompt - cached
	cost := float64(cached)/1_000_000*price.CacheHitPerM +
		float64(miss)/1_000_000*price.InputPerM +
		float64(clampNonNeg(u.Completion))/1_000_000*price.OutputPerM
	return cost
}

func clampNonNeg(n int) int {
	if n < 0 { return 0 }
	return n
}

// BilledUsage holds the per-session cumulative billable tokens. Each provider
// usage contributes its own (Prompt-Cached, Cached, Completion); we never reuse
// the WS handler's overwrite-semantics tokensIn for accounting.
type BilledUsage struct {
	InputTokens int
	CachedTokens int
	OutputTokens int
}

// Ledger accumulates billable tokens across a session. Each Add sees one
// provider usage event (including the judge call) and adds its non-cached
// input, cache hit, and output contributions.
type Ledger struct {
	Billed BilledUsage
}

func (l *Ledger) Add(u Usage) {
	cached := clampNonNeg(u.Cached)
	if cached > u.Prompt { cached = u.Prompt }
	miss := u.Prompt - cached
	l.Billed.InputTokens += miss
	l.Billed.CachedTokens += cached
	l.Billed.OutputTokens += clampNonNeg(u.Completion)
}

// Cost returns the accumulated dollar cost and known flag for the session.
func (l *Ledger) Cost(tab map[string]ModelPricing, model string) (float64, bool) {
	price, ok := tab[model]
	if !ok { return 0, false }
	return float64(l.Billed.InputTokens)/1_000_000*price.InputPerM +
		float64(l.Billed.CachedTokens)/1_000_000*price.CacheHitPerM +
		float64(l.Billed.OutputTokens)/1_000_000*price.OutputPerM, true
}

// FormatCost renders a dollar estimate. When known is false the renderer always
// returns "N/A" so users can distinguish unknown pricing from a $0.00 session.
func FormatCost(cost float64, known bool) string {
	if !known || cost < 0 { return "N/A" }
	switch {
	case cost == 0: return "$0.0000"
	case cost < 0.0001: return "<$0.0001"
	case cost < 0.01: return fmt.Sprintf("$%.4f", cost)
	case cost < 1: return fmt.Sprintf("$%.3f", cost)
	default: return fmt.Sprintf("$%.2f", cost)
	}
}
```

- [ ] **Step 4: 运行测试并提交**

Run: `go test ./internal/llm/eino -run 'Pricing|Cost|Ledger|MergePricing|FormatCost' -v`

Expected: PASS；模型 ID、cache 折扣、unknown N/A、累加账本、MergePricing 不可变性全部命中。

```bash
git add internal/llm/eino/pricing.go internal/llm/eino/pricing_test.go
git commit -m "feat(eino): add pricing table and per-usage cost ledger"
```

---

## Task 6: 持久化层——5 列计费迁移与 UpdateSessionMeta 扩展

**Files:**
- Modify: `internal/store/store.go`
- Modify: `internal/store/session.go`
- Modify: `internal/store/session_list.go`
- Modify: `internal/store/session_test.go`（若已有；否则创建）
- Modify: `internal/api/http/ws.go`（唯一现存调用点，第 9 参先传 `store.BillingMeta{}` 占位以保持本 commit 可构建；Task 9 再替换为 `cs.billingMeta()`）

- [ ] **Step 1: 写迁移与读写测试**

```go
// append to internal/store/session_test.go (create the file if missing)
package store

import (
	"path/filepath"
	"testing"
)

func TestMigrateAndPersistBillingColumns(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "yanshi.db"))
	if err != nil { t.Fatal(err) }
	defer st.Close()

	id, err := st.CreateSession("billing")
	if err != nil { t.Fatal(err) }
	if err := st.UpdateSessionMeta(id, "claude-opus-4-8", "low",
		5, 4, 3, 2, 1,
		BillingMeta{InputTokens: 3, CachedTokens: 2, OutputTokens: 4, CostUSD: 0.0125, CostKnown: true},
	); err != nil { t.Fatal(err) }

	got, err := st.GetSession(id)
	if err != nil || got == nil { t.Fatalf("GetSession: %v %v", got, err) }
	if got.BilledInputTokens != 3 || got.BilledCachedTokens != 2 || got.BilledOutputTokens != 4 {
		t.Fatalf("billed tokens = %+v", got)
	}
	if got.CostUSD != 0.0125 || !got.CostKnown {
		t.Fatalf("cost fields = %+v", got)
	}

	list, err := st.ListSessions(0)
	if err != nil { t.Fatal(err) }
	if len(list) != 1 || list[0].BilledInputTokens != 3 {
		t.Fatalf("ListSessions did not surface billed tokens: %+v", list)
	}
}
```

- [ ] **Step 2: 运行测试，确认 `BillingMeta` 与新列未定义**

Run: `go test ./internal/store -run TestMigrateAndPersistBillingColumns -v`

Expected: FAIL，`BillingMeta` 未定义、列不存在。

- [ ] **Step 3: 在 `store.go:migrate` 末尾加入五列迁移**

在 `addColumnIfMissing("sessions", "archived", ...)` 之后追加：

```go
if err := s.addColumnIfMissing("sessions", "billed_input_tokens", "INTEGER NOT NULL DEFAULT 0"); err != nil {
	return err
}
if err := s.addColumnIfMissing("sessions", "billed_cached_tokens", "INTEGER NOT NULL DEFAULT 0"); err != nil {
	return err
}
if err := s.addColumnIfMissing("sessions", "billed_output_tokens", "INTEGER NOT NULL DEFAULT 0"); err != nil {
	return err
}
if err := s.addColumnIfMissing("sessions", "cost_usd", "REAL NOT NULL DEFAULT 0"); err != nil {
	return err
}
if err := s.addColumnIfMissing("sessions", "cost_known", "INTEGER NOT NULL DEFAULT 0"); err != nil {
	return err
}
```

同步在 `const schema` 的 `CREATE TABLE sessions` 中加入这五列，使全新数据库与迁移后数据库一致：

```sql
billed_input_tokens  INTEGER NOT NULL DEFAULT 0,
billed_cached_tokens INTEGER NOT NULL DEFAULT 0,
billed_output_tokens INTEGER NOT NULL DEFAULT 0,
cost_usd             REAL    NOT NULL DEFAULT 0,
cost_known           INTEGER NOT NULL DEFAULT 0
```

- [ ] **Step 4: 扩展 `UpdateSessionMeta` 签名与 `BillingMeta`**

替换 `UpdateSessionMeta`：

```go
// BillingMeta carries the per-session cumulative billable tokens and cost.
// CostKnown=false means the session used at least one model not in the pricing
// table; renderers must show N/A, not $0.
type BillingMeta struct {
	InputTokens int
	CachedTokens int
	OutputTokens int
	CostUSD float64
	CostKnown bool
}

func (s *Store) UpdateSessionMeta(sessionID, model, thinking string, tokensIn, tokensOut, turns, cached, reasoning int, billing BillingMeta) error {
	known := 0
	if billing.CostKnown { known = 1 }
	_, err := s.DB.Exec(
		"UPDATE sessions SET model = ?, thinking = ?, tokens_in = ?, tokens_out = ?, turns = ?, cached_tokens = ?, reasoning_tokens = ?, "+
			"billed_input_tokens = ?, billed_cached_tokens = ?, billed_output_tokens = ?, cost_usd = ?, cost_known = ?, updated_at = ? WHERE id = ?",
		model, thinking, tokensIn, tokensOut, turns, cached, reasoning,
		billing.InputTokens, billing.CachedTokens, billing.OutputTokens, billing.CostUSD, known,
		time.Now().Unix(), sessionID,
	)
	return err
}
```

`internal/store/session.go` 已 import `time`；无需新增。

同一 Step 内必须修改唯一现存 callsite，否则本 commit 编译失败。`internal/api/http/ws.go:1086` 现有调用（8 参）：

```go
_ = s.store.UpdateSessionMeta(cs.sessionID, cs.model, cs.thinking, cs.tokensIn, cs.tokensOut, cs.turns, cs.cachedTokens, cs.reasoningTokens)
```

替换为（9 参，第 9 参 `store.BillingMeta{}` 占位 —— Task 9 Step 4 再把占位换为 `cs.billingMeta()`）：

```go
_ = s.store.UpdateSessionMeta(cs.sessionID, cs.model, cs.thinking, cs.tokensIn, cs.tokensOut, cs.turns, cs.cachedTokens, cs.reasoningTokens, store.BillingMeta{})
```

不能声明"编译器以后强制"：Task 6 的 commit 必须自己可构建，所以占位必须落在同一 commit 里。Task 7/8 不再触碰这行；Task 9 用 `cs.billingMeta()` 替换 `store.BillingMeta{}`。

- [ ] **Step 5: 扩展 `SessionSummary`、`listSessionsWhere`、`GetSession`**

```go
type SessionSummary struct {
	ID string
	Title string
	CreatedAt int64
	UpdatedAt int64
	Model string
	Thinking string
	TokensIn int
	TokensOut int
	CachedTokens int
	ReasoningTokens int
	Turns int
	Archived bool
	BilledInputTokens int
	BilledCachedTokens int
	BilledOutputTokens int
	CostUSD float64
	CostKnown bool
}
```

替换 `listSessionsWhere` 与 `GetSession` 的 SELECT/Scan：

```go
const sessionColumns = "id, title, created_at, updated_at, model, thinking, tokens_in, tokens_out, turns, cached_tokens, reasoning_tokens, archived, billed_input_tokens, billed_cached_tokens, billed_output_tokens, cost_usd, cost_known"

func scanSession(scanner interface{ Scan(dest ...any) error }, ss *SessionSummary) error {
	var archived int
	var known int
	err := scanner.Scan(
		&ss.ID, &ss.Title, &ss.CreatedAt, &ss.UpdatedAt, &ss.Model, &ss.Thinking,
		&ss.TokensIn, &ss.TokensOut, &ss.Turns, &ss.CachedTokens, &ss.ReasoningTokens,
		&archived,
		&ss.BilledInputTokens, &ss.BilledCachedTokens, &ss.BilledOutputTokens,
		&ss.CostUSD, &known,
	)
	ss.Archived = archived != 0
	ss.CostKnown = known != 0
	return err
}

func (s *Store) listSessionsWhere(where string, limit int) ([]SessionSummary, error) {
	q := "SELECT " + sessionColumns + " FROM sessions " + where + " ORDER BY updated_at DESC"
	args := []any{}
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.DB.Query(q, args...)
	if err != nil { return nil, err }
	defer rows.Close()
	var out []SessionSummary
	for rows.Next() {
		var ss SessionSummary
		if err := scanSession(rows, &ss); err != nil { return nil, err }
		out = append(out, ss)
	}
	return out, rows.Err()
}

func (s *Store) GetSession(id string) (*SessionSummary, error) {
	var ss SessionSummary
	err := scanSession(s.DB.QueryRow("SELECT "+sessionColumns+" FROM sessions WHERE id = ?", id), &ss)
	if err == sql.ErrNoRows { return nil, nil }
	if err != nil { return nil, err }
	return &ss, nil
}
```

保留 `ListSessions`/`ListArchivedSessions` 的对外签名不变。

- [ ] **Step 6: 运行测试并提交**

Run: `go test ./internal/store ./internal/api/http -v`

Expected: PASS；新列默认 0、`BillingMeta` 写入后读回一致，ListSessions 带回 billed 字段；ws.go 占位调用使 `./internal/api/http` 在新签名下编译通过。

```bash
git add internal/store/store.go internal/store/session.go internal/store/session_list.go internal/store/session_test.go internal/api/http/ws.go
git commit -m "feat(store): add per-session billing columns and ledger-aware update"
```

---

## Task 7: Wire vocabulary——proto、StreamEvent、wsbackend 与 SSE 失败语义

**Files:**
- Modify: `internal/proto/frame.go`
- Modify: `internal/proto/frame_test.go`
- Modify: `internal/cli/backend.go`
- Modify: `internal/cli/wsbackend.go`
- Modify: `internal/cli/ssebackend.go`
- Modify: `internal/cli/backend_test.go`（若已存在；否则创建）

- [ ] **Step 1: 写协议与映射测试**

```go
// append to internal/proto/frame_test.go
package proto

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestFeaturesSetPayloadEncodesFalseEnabled(t *testing.T) {
	frame := NewFeaturesSet("observe.otel_export", false)
	if frame.FeaturesSet == nil || frame.FeaturesSet.Enabled == nil {
		t.Fatal("Enabled must be a *bool so false survives omitempty")
	}
	if *frame.FeaturesSet.Enabled != false {
		t.Fatalf("Enabled = %v", *frame.FeaturesSet.Enabled)
	}
	raw, _ := json.Marshal(frame)
	if !bytes.Contains(raw, []byte(`"enabled":false`)) {
		t.Fatalf("wire form must encode enabled:false: %s", raw)
	}
}

func TestStatusFrameCarriesCostAndFeatures(t *testing.T) {
	st := NewStatusWithMode("claude-opus-4-8", "low", 100, 50, 1, 200000, "default", 0)
	st.CostUSD = 0.25
	st.CostKnown = true
	st.Features = []FeatureRow{{Key: "observe.otel_export", Stage: "experimental", Enabled: false, Owner: "C4"}}
	raw, _ := json.Marshal(st)
	for _, want := range []string{`"cost_usd":0.25`, `"cost_known":true`, `"features":[{`, `"owner":"C4"`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("missing %s in %s", want, raw)
		}
	}
}
```

```go
// append to internal/cli/backend_test.go (create the file if missing)
package cli

import (
	"testing"

	"github.com/x6nux/yanshi/internal/proto"
)

func TestToStreamEventMapsCostAndFeatures(t *testing.T) {
	enabled := false
	frame := proto.ServerFrame{
		Type: "status",
		CostUSD: 0.42,
		CostKnown: true,
		Features: []proto.FeatureRow{{Key: "x", Stage: "beta", Enabled: true, Owner: "C4"}},
	}
	_ = proto.NewFeaturesSet("k", enabled) // ensure constructor compiles
	ev := toStreamEvent(frame)
	if ev.CostUSD != 0.42 || !ev.CostKnown || len(ev.Features) != 1 {
		t.Fatalf("event = %+v", ev)
	}
}

func TestIsControlReplyIncludesFeatures(t *testing.T) {
	if !isControlReply("features") {
		t.Fatal(`"features" must close the control reply channel`)
	}
}
```

- [ ] **Step 2: 运行测试，确认符号未定义**

Run: `go test ./internal/proto ./internal/cli -run 'FeaturesSetPayload|StatusFrameCarries|ToStreamEventMapsCost|IsControlReplyIncludesFeatures' -v`

Expected: FAIL，`NewFeaturesSet`、`FeatureRow`、`StreamEvent.CostUSD`、`isControlReply` 不接受 `features`。

- [ ] **Step 3: 扩展 `proto/frame.go`**

在文件顶部 `ClientFrame` 文档块和 `ServerFrame` 文档块的 type 表里加入新行：

```text
// features_list        request the runtime feature flag table (reply: features)
// features_set         toggle one flag; Enabled *bool so false is serialised (reply: features)
// features             FeatureRow[] list with stage/enabled/owner (reply to features_list / features_set)
// status (extended)    CostUSD, CostKnown, Features (optional)
```

在 `ClientFrame` 末尾加入：

```go
// FeaturesSet carries a /features toggle. Enabled is *bool so that false is
// serialised on the wire — a bare bool with omitempty would drop "off" toggles.
FeaturesSet *FeaturesSetPayload `json:"features_set,omitempty"`
```

```go
type FeaturesSetPayload struct {
	Key string `json:"key"`
	Enabled *bool `json:"enabled"`
}

func NewFeaturesList() ClientFrame { return ClientFrame{Type: "features_list"} }

func NewFeaturesSet(key string, enabled bool) ClientFrame {
	e := enabled
	return ClientFrame{Type: "features_set", FeaturesSet: &FeaturesSetPayload{Key: key, Enabled: &e}}
}
```

在 `ServerFrame` 末尾加入：

```go
// CostUSD is the per-session cumulative USD estimate (COST1). CostKnown=false
// indicates the pricing table lacked at least one used model; renderers must
// show "N/A" rather than "$0.00". omitempty keeps the legacy shape when unset.
CostUSD float64 `json:"cost_usd,omitempty"`
CostKnown bool `json:"cost_known,omitempty"`
Features []FeatureRow `json:"features,omitempty"`
```

```go
type FeatureRow struct {
	Key string `json:"key"`
	Stage string `json:"stage"`
	Enabled bool `json:"enabled"`
	Owner string `json:"owner,omitempty"`
}

func NewFeaturesReply(rows []FeatureRow) ServerFrame {
	return ServerFrame{Type: "features", Features: rows}
}
```

在 `SessionInfo` 末尾加入：

```go
CostUSD float64 `json:"cost_usd,omitempty"`
CostKnown bool `json:"cost_known,omitempty"`
```

`NewStatus` 与 `NewStatusWithMode` 的签名保持不变；调用方通过 `st.CostUSD = ...` 赋值，与现有 `st.CachedTokens = ...` 同模式。

- [ ] **Step 4: 扩展 `cli/backend.go` 的 `StreamEvent`**

在 `StructuredResult json.RawMessage` 后追加：

```go
// COST1 fields. CostKnown=false means N/A. Populated from ServerFrame in
// wsbackend.toStreamEvent; zero on SSE (which now reports cost per turn in the
// status frame too — see chat.go).
CostUSD float64
CostKnown bool

// Features carries the /features reply rows.
Features []proto.FeatureRow
```

- [ ] **Step 5: 扩展 `wsbackend.go`**

把 `isControlReply` 的 case 列表改为：

```go
case "models", "status", "mcp_list", "sessions", "session_restored", "session_ack", "features":
	return true
```

在 `toStreamEvent` 返回值中加入三个字段：

```go
return StreamEvent{
	// ... existing fields ...
	StructuredResult: f.StructuredResult,
	CostUSD: f.CostUSD,
	CostKnown: f.CostKnown,
	Features: f.Features,
}
```

- [ ] **Step 6: 让 SSE 对 `/features` 给出明确失败**

把 `ssebackend.SendFrame` 替换为：

```go
func (b *sseBackend) SendFrame(_ context.Context, f proto.ClientFrame) (<-chan StreamEvent, error) {
	switch f.Type {
	case "features_list", "features_set":
		return nil, errors.New("/features requires a WebSocket backend (SSE is stateless)")
	default:
		return nil, ErrSSEControlUnsupported
	}
}
```

`errors` 已在该文件 import 列表中。

- [ ] **Step 7: 运行测试并提交**

Run: `go test ./internal/proto ./internal/cli -v`

Expected: PASS；`features_set` 的 false 在 wire 上保留，`features` 关闭控制通道，SSE 返回明确错误。

```bash
git add internal/proto/frame.go internal/proto/frame_test.go internal/cli/backend.go internal/cli/wsbackend.go internal/cli/ssebackend.go internal/cli/backend_test.go
git commit -m "feat(proto): add cost and features frames with SSE fallback"
```

---

## Task 8: 组合根装配 features/pricing，并通过 `apihttp.Config` 注入

**Files:**
- Modify: `internal/bootstrap/bootstrap.go`
- Modify: `internal/bootstrap/bootstrap_test.go`
- Modify: `internal/api/http/server.go`

- [ ] **Step 1: 写 bootstrap 测试**

```go
// append to internal/bootstrap/bootstrap_test.go
func TestBuildWiresFeaturesAndPricing(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	body := fmt.Sprintf(`
server: { http_addr: "127.0.0.1:0" }
storage: { sqlite_path: %q }
features:
  strict: true
  overrides:
    observe.cost_in_status: true
pricing:
  overrides:
    custom-model:
      input_per_million: 2
      cache_hit_per_million: 0.2
      output_per_million: 8
`, toYAMLPath(filepath.Join(dir, "yanshi.db")))
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil { t.Fatal(err) }

	app, err := Build(Options{ConfigPath: cfgPath, FakeModel: true})
	if err != nil { t.Fatal(err) }
	defer app.Shutdown(context.Background())
	if app.Features == nil || !app.Features.Enabled("observe.cost_in_status") {
		t.Fatalf("features not wired: %#v", app.Features)
	}
	if app.Pricing["custom-model"].InputPerM != 2 {
		t.Fatalf("pricing override not wired: %+v", app.Pricing["custom-model"])
	}
	if app.Pricing["claude-opus-4-8"].InputPerM != 5 {
		t.Fatalf("default pricing missing: %+v", app.Pricing)
	}
}

func TestBuildStrictFeaturesNamesUnknownFlag(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	body := fmt.Sprintf(`
storage: { sqlite_path: %q }
features:
  strict: true
  overrides:
    typo_observe_flag: true
`, toYAMLPath(filepath.Join(dir, "yanshi.db")))
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil { t.Fatal(err) }
	_, err := Build(Options{ConfigPath: cfgPath, FakeModel: true})
	if err == nil || !strings.Contains(err.Error(), "typo_observe_flag") {
		t.Fatalf("expected named strict flag error, got %v", err)
	}
}
```

测试文件 import 块确保包含：

```go
import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)
```

- [ ] **Step 2: 运行测试，确认 `App.Features`/`App.Pricing` 未定义**

Run: `go test ./internal/bootstrap -run 'TestBuildWiresFeaturesAndPricing|TestBuildStrictFeaturesNamesUnknownFlag' -v`

Expected: FAIL，字段未定义。

- [ ] **Step 3: 扩展 `api/http/server.go` 的依赖注入**

替换 `Config` 与 `Server` 结构（保留现有 `CompactionConfig` 不变）：

```go
type Config struct {
	Token string
	Compaction CompactionConfig
	Store *store.Store
	PriceTab map[string]einollm.ModelPricing
	FeaturesReg *features.Registry
}

type Server struct {
	mux *http.ServeMux
	token string
	compaction CompactionConfig
	store *store.Store
	priceTab map[string]einollm.ModelPricing
	featuresReg *features.Registry
}

func New(cfg Config) *Server {
	return &Server{
		mux: http.NewServeMux(),
		token: cfg.Token,
		compaction: cfg.Compaction,
		store: cfg.Store,
		priceTab: cfg.PriceTab,
		featuresReg: cfg.FeaturesReg,
	}
}
```

在 import 块加入：

```go
"github.com/x6nux/yanshi/internal/features"
einollm "github.com/x6nux/yanshi/internal/llm/eino"
```

- [ ] **Step 4: 扩展 `bootstrap.App` 并构造 registry/table**

在 `App` 加：

```go
Features *features.Registry
Pricing map[string]einollm.ModelPricing
```

在 logger setup 后、`store.Open` 前构造：

```go
featureReg := features.NewRegistry(cfg.Features.Strict)
for _, spec := range features.DefaultSpecs() { featureReg.Register(spec) }
if err := featureReg.ApplyMap(cfg.Features.Overrides); err != nil {
	return nil, fmt.Errorf("bootstrap: features: %w", err)
}

overlay := make(map[string]einollm.ModelPricing, len(cfg.Pricing.Overrides))
for modelName, price := range cfg.Pricing.Overrides {
	overlay[modelName] = einollm.ModelPricing{
		InputPerM: price.InputPerM,
		CacheHitPerM: price.CacheHitPerM,
		OutputPerM: price.OutputPerM,
	}
}
priceTab := einollm.MergePricing(einollm.DefaultPricing(), overlay)
```

在 `apihttp.New(apihttp.Config{...})` 的 struct literal 内加入：

```go
PriceTab: priceTab,
FeaturesReg: featureReg,
```

在最终 `return &App{...}` 的 struct literal 内加入：

```go
Features: featureReg,
Pricing: priceTab,
```

在 import 块加入：

```go
"github.com/x6nux/yanshi/internal/features"
```

`einollm` 已有 import。**禁止**通过 `app.Server.(*http.Server).Handler.(*apihttp.Server)` 回填字段；所有依赖从 `apihttp.Config` 构造时注入。

- [ ] **Step 5: 运行测试并提交**

Run: `go test ./internal/api/http ./internal/bootstrap -v`

Expected: PASS；strict error 含未知 flag 名，custom 与默认价格同时存在。

```bash
git add internal/api/http/server.go internal/bootstrap/bootstrap.go internal/bootstrap/bootstrap_test.go
git commit -m "feat(bootstrap): wire features and pricing through api config"
```

---

## Task 9: WS/SSE feature controls 与按 provider usage 计费

**Files:**
- Modify: `internal/api/http/ws.go`
- Modify: `internal/api/http/ws_test.go`
- Modify: `internal/api/http/chat.go`
- Modify: `internal/api/http/chat_test.go`

- [ ] **Step 1: 写 WS 账本与 feature handler 测试**

```go
// append to internal/api/http/ws_test.go
func TestConnSessionBillsEveryProviderUsageAndJudge(t *testing.T) {
	s := &Server{priceTab: map[string]einollm.ModelPricing{
		"test-model": {InputPerM: 2, CacheHitPerM: 0.5, OutputPerM: 8},
	}}
	cs := connSession{model: "test-model", costKnown: true}
	cs.addProviderUsage(context.Background(), s, orchestrator.TurnUsage{PromptTokens: 100, CachedTokens: 40, CompletionTokens: 10})
	cs.addProviderUsage(context.Background(), s, orchestrator.TurnUsage{PromptTokens: 80, CachedTokens: 20, CompletionTokens: 5})
	cs.addProviderUsage(context.Background(), s, orchestrator.TurnUsage{PromptTokens: 50, CachedTokens: 50, CompletionTokens: 2})
	if cs.billing.Billed.InputTokens != 120 || cs.billing.Billed.CachedTokens != 110 || cs.billing.Billed.OutputTokens != 17 {
		t.Fatalf("wrong billed ledger: %+v", cs.billing.Billed)
	}
	want := (120*2.0 + 110*0.5 + 17*8.0) / 1_000_000
	if math.Abs(cs.costUSD-want) > 1e-12 || !cs.costKnown {
		t.Fatalf("cost=(%f,%v), want=(%f,true)", cs.costUSD, cs.costKnown, want)
	}
}

func TestConnSessionUnknownModelIsNotZeroCost(t *testing.T) {
	cs := connSession{model: "unknown-model", costKnown: true}
	cs.addProviderUsage(context.Background(), &Server{priceTab: einollm.DefaultPricing()}, orchestrator.TurnUsage{PromptTokens: 10})
	if cs.costKnown { t.Fatal("unknown model must be N/A") }
}

func TestFeatureRowsAndUnknownSet(t *testing.T) {
	reg := features.NewRegistry(true)
	reg.Register(features.Spec{Key: "observe.cost_in_status", Stage: features.Beta, Default: false, Owner: "runtime"})
	if err := setFeature(reg, proto.FeaturesSetPayload{Key: "observe.cost_in_status", Enabled: boolPtr(true)}); err != nil { t.Fatal(err) }
	rows := featureRows(reg)
	if len(rows) != 1 || !rows[0].Enabled { t.Fatalf("rows=%+v", rows) }
	err := setFeature(reg, proto.FeaturesSetPayload{Key: "typo", Enabled: boolPtr(true)})
	if err == nil || !strings.Contains(err.Error(), "typo") { t.Fatalf("err=%v", err) }
	err = setFeature(reg, proto.FeaturesSetPayload{Key: "observe.cost_in_status"})
	if err == nil || !strings.Contains(err.Error(), "enabled") { t.Fatalf("err=%v", err) }
}

func boolPtr(v bool) *bool { return &v }
```

测试 import 块加入：

```go
"math"
"strings"

"github.com/x6nux/yanshi/internal/features"
einollm "github.com/x6nux/yanshi/internal/llm/eino"
```

- [ ] **Step 2: 运行测试，确认 helper/字段未定义**

Run: `go test ./internal/api/http -run 'TestConnSession|TestFeatureRowsAndUnknownSet' -v`

Expected: FAIL，`addProviderUsage`、`billing`、`setFeature` 未定义。

- [ ] **Step 3: 给 WS session 增加不可与 context counter 混用的 billed ledger**

在 `connSession` 加字段：

```go
billing einollm.Ledger
costUSD float64
costKnown bool
hasBilledUsage bool
```

增加 helper：

```go
func usageForPricing(u orchestrator.TurnUsage) einollm.Usage {
	return einollm.Usage{
		Prompt: u.PromptTokens,
		Cached: u.CachedTokens,
		Completion: u.CompletionTokens,
		Reasoning: u.ReasoningTokens,
	}
}

func (cs *connSession) addProviderUsage(_ context.Context, s *Server, u orchestrator.TurnUsage) {
	priced := usageForPricing(u)
	if priced.Prompt <= 0 && priced.Cached <= 0 && priced.Completion <= 0 { return }
	cs.billing.Add(priced)
	cost, known := einollm.CostOK(s.priceTab, cs.displayModel(), priced)
	if !cs.hasBilledUsage {
		cs.costKnown = known
		cs.hasBilledUsage = true
	} else {
		cs.costKnown = cs.costKnown && known
	}
	if known { cs.costUSD += cost }
}

func (cs *connSession) resetBilling(s *Server) {
	cs.billing = einollm.Ledger{}
	cs.costUSD = 0
	cs.hasBilledUsage = false
	_, cs.costKnown = s.priceTab[cs.displayModel()]
}

func (cs *connSession) billingMeta() store.BillingMeta {
	return store.BillingMeta{
		InputTokens: cs.billing.Billed.InputTokens,
		CachedTokens: cs.billing.Billed.CachedTokens,
		OutputTokens: cs.billing.Billed.OutputTokens,
		CostUSD: cs.costUSD,
		CostKnown: cs.costKnown,
	}
}
```

WS 文件 import 块加入：

```go
"github.com/x6nux/yanshi/internal/features"
```

`einollm`、`store` 已有 import。创建 `connSession` 并确定 `defaultModel` 后调用：

```go
cs.resetBilling(s)
```

`statusFrame` return 前加入：

```go
st.CostUSD = cs.costUSD
st.CostKnown = cs.costKnown
```

`clear` 分支在清零 context counters 后调用：

```go
cs.resetBilling(s)
```

`set_model` 成功切换后，若尚无 billed usage，刷新未知价格状态：

```go
if !cs.hasBilledUsage { _, cs.costKnown = s.priceTab[cs.displayModel()] }
```

- [ ] **Step 4: 在每次 provider usage 发生点记账一次**

把 WS `onUsage` 改为：

```go
onUsage := func(u orchestrator.TurnUsage) {
	cs.addProviderUsage(turnCtx, s, u)
	st := cs.statusFrame(s)
	st.TokensIn = u.PromptTokens
	st.TokensOut = cs.tokensOut + u.CompletionTokens
	st.CachedTokens = u.CachedTokens
	st.ReasoningTokens = u.ReasoningTokens
	conn.write(st)
}
```

在每次 judge 返回后**立即**记账，不能只保存最后一个 `judgeUsage`：

```go
complete, reason, ju := o.JudgeCompletion(turnCtx)
cs.addProviderUsage(turnCtx, s, ju)
judgeCompletionTokens += ju.CompletionTokens
judgeUsage = ju
```

这保留现有 `tokensIn` 覆盖语义，同时让模型多次 ReAct、schema retry、stop-judge retry 和每次 judge 都进入累计 billed ledger。

持久化改为：

```go
_ = s.store.UpdateSessionMeta(
	cs.sessionID, cs.model, cs.thinking,
	cs.tokensIn, cs.tokensOut, cs.turns,
	cs.cachedTokens, cs.reasoningTokens,
	cs.billingMeta(),
)
```

- [ ] **Step 5: restore/list 同步 billed/cost（精确补丁）**

`cs.loadSession`（`internal/api/http/ws.go:307-346`）在现有 `cs.turns = ss.Turns` 之后、`return nil` 之前精确插入：

```go
	cs.turns = ss.Turns
	cs.billing.Billed = einollm.BilledUsage{
		InputTokens: ss.BilledInputTokens,
		CachedTokens: ss.BilledCachedTokens,
		OutputTokens: ss.BilledOutputTokens,
	}
	cs.costUSD = ss.CostUSD
	cs.costKnown = ss.CostKnown
	cs.hasBilledUsage = ss.BilledInputTokens + ss.BilledCachedTokens + ss.BilledOutputTokens > 0
	return nil
}
```

`handleRestoreSession`（`internal/api/http/ws.go:1186-1201`）内联块在 `cs.turns = ss.Turns` 之后、`conn.write(proto.NewSessionRestored(...))` 之前精确插入，并把 `NewSessionRestored` 的返回值赋给变量以便补 cost 字段：

```go
	cs.model = ss.Model
	cs.thinking = ss.Thinking
	cs.tokensIn = ss.TokensIn
	cs.tokensOut = ss.TokensOut
	cs.cachedTokens = ss.CachedTokens
	cs.reasoningTokens = ss.ReasoningTokens
	cs.turns = ss.Turns
	cs.billing.Billed = einollm.BilledUsage{
		InputTokens: ss.BilledInputTokens,
		CachedTokens: ss.BilledCachedTokens,
		OutputTokens: ss.BilledOutputTokens,
	}
	cs.costUSD = ss.CostUSD
	cs.costKnown = ss.CostKnown
	cs.hasBilledUsage = ss.BilledInputTokens + ss.BilledCachedTokens + ss.BilledOutputTokens > 0

	// Reply with the restored session state.
	restored := proto.NewSessionRestored(sessionID, hist, cs.model, cs.thinking, cs.tokensIn, cs.tokensOut, cs.turns)
	restored.CostUSD = cs.costUSD
	restored.CostKnown = cs.costKnown
	conn.write(restored)
}
```

`handleSessionList` 和 `handleArchivedSessionList` 构造每个 `proto.SessionInfo` 时加入（与现有字段同级）：

```go
CostUSD: ss.CostUSD,
CostKnown: ss.CostKnown,
```

两处补丁使用相同的 `einollm.BilledUsage` 字面量，避免 `loadSession`（方法调用方：`session_restore` 入口的 `cs.loadSession(s, id)`）和 `handleRestoreSession`（内联实现，不调用 `loadSession`）之间出现状态漂移。

- [ ] **Step 6: 实现 features list/set WS 控制分支**

增加 helpers：

```go
func featureRows(reg *features.Registry) []proto.FeatureRow {
	if reg == nil { return nil }
	listed := reg.List()
	rows := make([]proto.FeatureRow, 0, len(listed))
	for _, row := range listed {
		rows = append(rows, proto.FeatureRow{
			Key: row.Key,
			Stage: row.Stage,
			Enabled: row.Enabled,
			Owner: row.Owner,
		})
	}
	return rows
}

func setFeature(reg *features.Registry, payload proto.FeaturesSetPayload) error {
	if reg == nil { return errors.New("feature registry is unavailable") }
	if payload.Key == "" { return errors.New("feature key is required") }
	if payload.Enabled == nil { return errors.New("feature enabled value is required") }
	return reg.Set(payload.Key, *payload.Enabled)
}
```

在 `switch cf.Type` 的 `user_message` 前加入：

```go
case "features_list":
	conn.write(proto.NewFeaturesReply(featureRows(s.featuresReg)))
case "features_set":
	if cf.FeaturesSet == nil {
		conn.write(proto.NewError("features_set payload is required"))
		continue
	}
	if err := setFeature(s.featuresReg, *cf.FeaturesSet); err != nil {
		conn.write(proto.NewError(err.Error()))
		continue
	}
	conn.write(proto.NewFeaturesReply(featureRows(s.featuresReg)))
```

import 块确保有标准库 `errors`。`enabled=false` 经 `*bool` 到这里仍然非 nil，测试必须保持覆盖该分支。

- [ ] **Step 7: SSE 每次 callback 累计 billed usage**

在每个请求的 retry loop 前初始化：

```go
billingModel := req.Model
if billingModel == "" {
	if names := sortedModelNames(models); len(names) > 0 { billingModel = names[0] }
}
ledger := einollm.Ledger{}
costUSD := 0.0
_, costKnown := s.priceTab[billingModel]
hasBilledUsage := false
onUsage := func(u orchestrator.TurnUsage) {
	priced := usageForPricing(u)
	if priced.Prompt <= 0 && priced.Cached <= 0 && priced.Completion <= 0 { return }
	ledger.Add(priced)
	cost, known := einollm.CostOK(s.priceTab, billingModel, priced)
	if !hasBilledUsage { costKnown = known; hasBilledUsage = true } else { costKnown = costKnown && known }
	if known { costUSD += cost }
}
```

把 SSE classifier 调用改成四参数形式：

```go
orchestrator.ClassifyEventsWithUsage(iter, &usage, func(f proto.ServerFrame) {
	if f.Type == "error" { hadError = true }
	if f.Type == "agent_chunk" { assistantText += f.Text }
	writeSSEFrame(w, fl, f)
}, onUsage)
```

最终 status 加：

```go
sseStatus.CostUSD = costUSD
sseStatus.CostKnown = costKnown
```

这里 ledger 是 request 累计账本；每次 retry 的 live `usage` 可以覆盖，但已经发生的 provider usage 不得从账单中删除。

- [ ] **Step 8: 运行 API 测试并提交**

Run: `go test ./internal/api/http -v`

Expected: PASS；feature set/list、unknown N/A、cache split、judge 独立 usage 均通过。

```bash
git add internal/api/http/ws.go internal/api/http/ws_test.go internal/api/http/chat.go internal/api/http/chat_test.go
git commit -m "feat(api): expose feature controls and accurate usage billing"
```

---

## Task 10: TUI `/cost`、`/stats` 与 `/features`

**Files:**
- Modify: `internal/cli/tui/model.go`
- Modify: `internal/cli/tui/events.go`
- Modify: `internal/cli/tui/commands.go`
- Modify: `internal/cli/tui/commands_test.go`
- Modify: `internal/cli/tui/model_test.go`

- [ ] **Step 1: 写命令路由和渲染测试**

```go
// append to internal/cli/tui/commands_test.go
func TestModel_CommandFeaturesSendsListEnableDisable(t *testing.T) {
	falseValue := false
	trueValue := true
	cases := []struct {
		input string
		want proto.ClientFrame
	}{
		{input: "/features", want: proto.NewFeaturesList()},
		{input: "/features enable observe.cost_in_status", want: proto.ClientFrame{Type: "features_set", FeaturesSet: &proto.FeaturesSetPayload{Key: "observe.cost_in_status", Enabled: &trueValue}}},
		{input: "/features disable observe.cost_in_status", want: proto.ClientFrame{Type: "features_set", FeaturesSet: &proto.FeaturesSetPayload{Key: "observe.cost_in_status", Enabled: &falseValue}}},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			rec := &recordingSession{}
			m := newModel(rec, "/proj")
			_, _ = m.runCommand(tc.input)
			require.Equal(t, []proto.ClientFrame{tc.want}, rec.frames)
		})
	}
}

func TestStatusEntryRendersKnownAndUnknownCost(t *testing.T) {
	known := stripANSI((&statusEntry{tokensIn: 100, tokensOut: 20, costUSD: 0.012345, costKnown: true}).render(120, newSpinner()))
	assert.Contains(t, known, "$0.012345")
	unknown := stripANSI((&statusEntry{tokensIn: 100, costKnown: false}).render(120, newSpinner()))
	assert.Contains(t, unknown, "N/A")
	assert.NotContains(t, unknown, "$0.000000")
}

func TestStatsEntryAggregatesKnownCostAndNamesUnknown(t *testing.T) {
	e := &statsEntry{sessions: []proto.SessionInfo{
		{Title: "known", TokensIn: 100, CostUSD: 0.25, CostKnown: true},
		{Title: "unknown", TokensOut: 10, CostKnown: false},
	}}
	out := stripANSI(e.render(120, newSpinner()))
	assert.Contains(t, out, "known")
	assert.Contains(t, out, "$0.250000")
	assert.Contains(t, out, "unknown")
	assert.Contains(t, out, "N/A")
	assert.Contains(t, out, "1 unknown")
}

func TestFeaturesEntryRendersStageOwnerAndDisabled(t *testing.T) {
	e := featuresEntry{rows: []proto.FeatureRow{{
		Key: "observe.cost_in_status", Stage: "beta", Enabled: false, Owner: "runtime",
	}}}
	out := stripANSI(e.render(120, newSpinner()))
	assert.Contains(t, out, "observe.cost_in_status")
	assert.Contains(t, out, "beta")
	assert.Contains(t, out, "disabled")
	assert.Contains(t, out, "runtime")
}
```

- [ ] **Step 2: 运行测试，确认 command/entry/cost 字段缺失**

Run: `go test ./internal/cli/tui -run 'TestModel_CommandFeatures|TestStatusEntryRenders|TestStatsEntryAggregates|TestFeaturesEntryRenders' -v`

Expected: FAIL，`cmdFeatures`、`featuresEntry`、`costUSD` 未定义。

- [ ] **Step 3: 扩展 model 与 status 事件**

在 `model` 的 session status fields 加：

```go
costUSD float64
costKnown bool
```

在 `applyStatus` 的 token 字段后加：

```go
m.costUSD = ev.CostUSD
m.costKnown = ev.CostKnown
```

在 pending status 赋值块加：

```go
m.pendingStatus.costUSD = ev.CostUSD
m.pendingStatus.costKnown = ev.CostKnown
```

在 `applyEvent` 的 `session_restored` 分支同步：

```go
m.costUSD = ev.CostUSD
m.costKnown = ev.CostKnown
```

- [ ] **Step 4: 实现 `/features` 命令**

`commandTable` 加：

```go
{name: "features", help: "list / enable / disable feature flags", run: cmdFeatures},
```

增加 handler：

```go
func cmdFeatures(m model, args []string) (tea.Model, tea.Cmd) {
	if len(args) == 0 { return m.sendControlFrame(proto.NewFeaturesList()) }
	if len(args) != 2 || (args[0] != "enable" && args[0] != "disable") {
		m.entries = append(m.entries, errorEntry{text: "usage: /features [enable|disable <key>]"})
		m.refresh()
		m.viewport.GotoBottom()
		return m, nil
	}
	return m.sendControlFrame(proto.NewFeaturesSet(args[1], args[0] == "enable"))
}
```

- [ ] **Step 5: 为 `/cost` 加 USD 且 unknown 显式 `N/A`**

扩展 `statusEntry`：

```go
type statusEntry struct {
	model, thinking string
	tokensIn, tokensOut, turns int
	cachedTokens, reasoningTokens int
	costUSD float64
	costKnown bool
}
```

增加 formatter：

```go
func formatCostUSD(cost float64, known bool) string {
	if !known { return "N/A" }
	return fmt.Sprintf("$%.6f", cost)
}
```

在 `statusEntry.render` 的 token 行后加入：

```go
b.WriteString(fmt.Sprintf("    estimated cost: %s\n", formatCostUSD(e.costUSD, e.costKnown)))
```

`/config` 复用同一 status block，因此也会显示同一份安全、明确的 USD 状态；不另建重复 renderer。

- [ ] **Step 6: 完整替换 `statsEntry.render`，聚合 session USD**

```go
func (e *statsEntry) render(_ int, _ spinner.Model) string {
	var b strings.Builder
	b.WriteString("  " + toolName.Render("stats") + toolMeta.Render("  · per-session token and USD consumption") + "\n\n")
	type row struct {
		label string
		total int
		costUSD float64
		costKnown bool
	}
	var rows []row
	sumTokens := 0
	sumKnownCost := 0.0
	unknownCosts := 0
	for _, s := range e.sessions {
		total := s.TokensIn + s.TokensOut
		if total <= 0 { continue }
		label := s.Title
		if label == "" { label = "(untitled)" }
		rows = append(rows, row{label: label, total: total, costUSD: s.CostUSD, costKnown: s.CostKnown})
		sumTokens += total
		if s.CostKnown { sumKnownCost += s.CostUSD } else { unknownCosts++ }
	}
	if len(rows) == 0 {
		b.WriteString("    " + warnStyle.Render("(no token usage recorded yet — send a message first)") + "\n\n")
		return b.String()
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].total > rows[j].total })
	if len(rows) > 15 { rows = rows[:15] }
	maxTotal := rows[0].total
	for _, r := range rows {
		barLen := 0
		if maxTotal > 0 { barLen = (r.total*statsBarWidth + maxTotal - 1) / maxTotal }
		bar := okStyle.Render(strings.Repeat("█", barLen)) + strings.Repeat("░", statsBarWidth-barLen)
		count := fmt.Sprintf("%7s", formatTokens(r.total))
		cost := formatCostUSD(r.costUSD, r.costKnown)
		b.WriteString(fmt.Sprintf("    %s %s  %-12s %s\n", bar, toolMeta.Render(count), cost, r.label))
	}
	avg := sumTokens / len(rows)
	b.WriteString(fmt.Sprintf("\n    %s  %d sessions · total %s · avg %s · known cost %s",
		toolMeta.Render("summary"), len(rows), formatTokens(sumTokens), formatTokens(avg), formatCostUSD(sumKnownCost, true)))
	if unknownCosts > 0 { b.WriteString(fmt.Sprintf(" · %d unknown (N/A)", unknownCosts)) }
	b.WriteString("\n\n")
	return b.String()
}
```

未知 session 不计入 `sumKnownCost`，同时明确显示 unknown 数；绝不把未知价格伪装为 `$0`。

- [ ] **Step 7: 增加 feature entry 与事件分支**

```go
type featuresEntry struct { rows []proto.FeatureRow }

func (e featuresEntry) render(_ int, _ spinner.Model) string {
	var b strings.Builder
	b.WriteString("  " + toolName.Render("features") + "\n")
	if len(e.rows) == 0 {
		b.WriteString("    " + warnStyle.Render("(none registered)") + "\n\n")
		return b.String()
	}
	for _, row := range e.rows {
		state := warnStyle.Render("disabled")
		if row.Enabled { state = okStyle.Render("enabled") }
		b.WriteString(fmt.Sprintf("    %-32s %-12s %-10s %s\n", row.Key, row.Stage, state, toolMeta.Render(row.Owner)))
	}
	b.WriteString("\n")
	return b.String()
}
```

在 `applyEvent` switch 加：

```go
case "features":
	m.entries = append(m.entries, featuresEntry{rows: ev.Features})
```

该分支直接消费 Task 7 中 `wsbackend.toStreamEvent` 映射的 `Features`；SSE backend 返回的明确错误继续走既有 `errorEntry` 用户可见路径。

- [ ] **Step 8: 运行 TUI 测试并提交**

Run: `go test ./internal/cli/tui -v`

Expected: PASS；false wire value、known/unknown cost、历史聚合与 feature metadata 均可见。

```bash
git add internal/cli/tui/model.go internal/cli/tui/events.go internal/cli/tui/commands.go internal/cli/tui/commands_test.go internal/cli/tui/model_test.go
git commit -m "feat(tui): add feature controls and USD cost views"
```

---

## Task 11: O07 doctor — MCP / LSP / permissions 增量检查

**Files:**
- Modify: `internal/cli/doctor.go`
- Modify: `internal/cli/doctor_test.go`

- [ ] **Step 1: 写新检查的失败测试**

```go
// append to internal/cli/doctor_test.go
func TestRunDoctor_IncludesObservabilityChecks(t *testing.T) {
	dir := t.TempDir()
	cfgBody := fmt.Sprintf(`
server: { http_addr: "127.0.0.1:0" }
storage: { sqlite_path: %q }
llm:
  providers:
    - { name: "openai", kind: "openai", model: "gpt-4o", api_key: "sk-test" }
vcs:
  worktree_dir: %q
`, filepath.Join(dir, "yanshi.db"), filepath.Join(dir, "wt"))
	rep := RunDoctor(context.Background(), DoctorOptions{ConfigPath: writeTempConfig(t, cfgBody), Root: t.TempDir()})
	for _, name := range []string{"mcp", "lsp", "permissions"} {
		c := findCheck(t, rep, name)
		if c.Status != StatusOK && c.Status != StatusWarn {
			t.Errorf("%s: got %s (%s), want ok or warn", name, c.Status, c.Message)
		}
	}
	// Sandbox must remain warn (S08 not done).
	if c := findCheck(t, rep, "sandbox"); c.Status != StatusWarn {
		t.Errorf("sandbox: got %s, want warn", c.Status)
	}
}

func TestCheckMCP_ServersListedOrNoneConfigured(t *testing.T) {
	c := checkMCP(&config.Config{}, nil)
	if c.Status != StatusOK || !strings.Contains(c.Message, "no mcp servers") {
		t.Errorf("default: got %s (%s)", c.Status, c.Message)
	}
}

func TestCheckLSP_ReportsProbes(t *testing.T) {
	c := checkLSP(context.Background(), t.TempDir())
	if c.Status != StatusOK && c.Status != StatusWarn {
		t.Errorf("got %s (%s)", c.Status, c.Message)
	}
	if !strings.Contains(c.Message, "lsp") {
		t.Errorf("expected 'lsp' in message: %s", c.Message)
	}
}

func TestCheckPermissions_ProfilesAndInteractiveMode(t *testing.T) {
	c := checkPermissions(&config.Config{
		Profiles: map[string]guard.PermissionProfile{
			"coding": {Tools: guard.ToolsPerm{Allow: []string{"fs_read", "fs_write"}}},
		},
	}, nil)
	if c.Status != StatusOK && c.Status != StatusWarn {
		t.Errorf("got %s (%s)", c.Status, c.Message)
	}
	if !strings.Contains(c.Message, "coding") {
		t.Errorf("expected profile name in message: %s", c.Message)
	}
}

func TestCheckPermissions_ConfigLoadSkipped(t *testing.T) {
	c := checkPermissions(nil, errors.New("missing"))
	if c.Status != StatusWarn || !strings.Contains(c.Message, "skipped") {
		t.Errorf("got %s (%s)", c.Status, c.Message)
	}
}
```

测试 import 块补充：

```go
"errors"

"github.com/x6nux/yanshi/internal/guard"
```

- [ ] **Step 2: 运行测试，确认新检查未定义**

Run: `go test ./internal/cli -run 'TestRunDoctor_IncludesObservabilityChecks|TestCheckMCP_|TestCheckLSP_|TestCheckPermissions_' -v`

Expected: FAIL，`checkMCP`、`checkLSP`、`checkPermissions` 未定义。

- [ ] **Step 3: 实现三个增量检查**

`doctor.go` 现有 import 块为（见 `internal/cli/doctor.go:1-17`）：

```go
import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/x6nux/yanshi/internal/acp"
	"github.com/x6nux/yanshi/internal/config"
	"github.com/x6nux/yanshi/internal/lockfile"
	"github.com/x6nux/yanshi/internal/store"
)
```

替换为下面的完整 import 块（新增 `"sort"`、`"time"`，以及 `internal/guard` —— checkPermissions 读取 prof.Tools.Allow：

```go
import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/x6nux/yanshi/internal/acp"
	"github.com/x6nux/yanshi/internal/config"
	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/lockfile"
	"github.com/x6nux/yanshi/internal/store"
)
```

**不要新增 `findCheck` / `writeTempConfig` helper**：两者已在 `internal/cli/doctor_test.go:21,33` 存在并被本 Task 测试复用。reality reviewer 此项是误报。

在 `RunDoctor` 末尾、`return` 前插入：

```go
checks = append(checks, checkMCP(cfg, cfgErr))
checks = append(checks, checkLSP(ctx, root))
checks = append(checks, checkPermissions(cfg, cfgErr))
```

实现：

```go
// checkMCP reports whether any MCP server is exposed via the chat transport.
// yanshi's chat MCP registry is currently empty: the only MCP server yanshi
// runs is vcs-mcp, which serves the ACP path via `yanshi vcs-mcp` and is not
// attached to chat. A zero count is therefore OK (not warn). doctor does NOT
// probe each configured server (YAGNI for the current scope — wire servers
// through the chat MCP registry when that lands).
func checkMCP(cfg *config.Config, cfgErr error) CheckResult {
	if cfgErr != nil { return skipped("mcp", cfgErr) }
	// yanshi has no dedicated MCP config block today; the chat registry is
	// populated programmatically. Surface that honestly instead of inventing
	// a config key.
	return CheckResult{
		Name: "mcp",
		Status: StatusOK,
		Message: "no mcp servers exposed via chat (vcs-mcp serves the ACP path)",
	}
}

// checkLSP probes whether an LSP binary is reachable. yanshi does not bundle an
// LSP server; the probe is advisory (warn when none is found, OK when one is).
// The probe target is the generic "gopls" binary when present — it is the
// baseline Go LSP and is what most users have on PATH in this repo. A 500ms
// timeout keeps the probe snappy on systems without it.
func checkLSP(ctx context.Context, root string) CheckResult {
	probeCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	bin := "gopls"
	if _, err := exec.LookPath(bin); err != nil {
		return CheckResult{Name: "lsp", Status: StatusWarn, Message: fmt.Sprintf("lsp: %q not in PATH (optional; install for code intelligence)", bin)}
	}
	cmd := exec.CommandContext(probeCtx, bin, "-rpc.trace")
	cmd.Dir = root
	if err := cmd.Start(); err != nil {
		return CheckResult{Name: "lsp", Status: StatusWarn, Message: fmt.Sprintf("lsp: start %q: %v", bin, err)}
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	return CheckResult{Name: "lsp", Status: StatusOK, Message: fmt.Sprintf("lsp: %q present", bin)}
}

// checkPermissions reports the configured profiles and the effective
// interactive permission mode. yanshi's static profile is read from
// cfg.Profiles[<orchestrator profile>]; the interactive mode is session-scoped
// (default/allow-edits/yolo/auto) so doctor only reports its presence.
func checkPermissions(cfg *config.Config, cfgErr error) CheckResult {
	if cfgErr != nil { return skipped("permissions", cfgErr) }
	names := make([]string, 0, len(cfg.Profiles))
	for name := range cfg.Profiles { names = append(names, name) }
	sort.Strings(names)
	if len(names) == 0 {
		return CheckResult{Name: "permissions", Status: StatusWarn, Message: "no profiles configured (guard default-denies all tools)"}
	}
	details := make([]string, 0, len(names))
	for _, name := range names {
		prof := cfg.Profiles[name]
		tools := "no tools"
		if len(prof.Tools.Allow) > 0 {
			tools = fmt.Sprintf("%d tools allowed", len(prof.Tools.Allow))
		}
		details = append(details, fmt.Sprintf("%s (%s)", name, tools))
	}
	return CheckResult{Name: "permissions", Status: StatusOK, Message: strings.Join(details, "; ")}
}
```

`sort` 已在标准库；确保 doctor.go import 块包含 `"sort"`（之前没有则加）。

- [ ] **Step 4: 运行测试并提交**

Run: `go test ./internal/cli -run 'TestRunDoctor|TestCheck' -v`

Expected: PASS；新检查 warn/ok 均可，sandbox 仍是 warn。

```bash
git add internal/cli/doctor.go internal/cli/doctor_test.go
git commit -m "feat(doctor): add MCP, LSP, and permissions checks"
```

---

## Task 12: OBS2 OpenTelemetry provider 与 OTLP 软降级

**Files:**
- Create: `internal/observe/otel/otel.go`
- Create: `internal/observe/otel/otel_test.go`
- Modify: `go.mod`
- Modify: `go.sum`

- [ ] **Step 1: 固定真实 OTel modules**

Run:

```bash
go get go.opentelemetry.io/otel@v1.38.0 \
  go.opentelemetry.io/otel/sdk@v1.38.0 \
  go.opentelemetry.io/otel/sdk/metric@v1.38.0 \
  go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp@v1.38.0 \
  go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp@v1.38.0
```

Expected: `go.mod`/`go.sum` 增加 OTel API、SDK、OTLP HTTP trace/metric exporter；不引入自制伪 API。

- [ ] **Step 2: 写 disabled 与 exporter failure 测试**

```go
// internal/observe/otel/otel_test.go
package otelobs

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// restoreOTelGlobals snapshots the process-global TracerProvider and
// MeterProvider and restores them at test end. Setup() and setupWithFactories
// both call otel.SetTracerProvider / SetMeterProvider, so any test that
// exercises them MUST install this cleanup and MUST NOT call t.Parallel — the
// OTel globals are process-wide and concurrent SetTracerProvider calls race.
func restoreOTelGlobals(t *testing.T) {
	t.Helper()
	prevTP := otel.GetTracerProvider()
	prevMP := otel.GetMeterProvider()
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		otel.SetMeterProvider(prevMP)
	})
}

// resetOTelNoop is the defensive baseline installed at test start so a
// previous test that set a real provider cannot leak into this one before
// Setup runs.
func resetOTelNoop() {
	otel.SetTracerProvider(tracenoop.NewTracerProvider())
	otel.SetMeterProvider(metricnoop.NewMeterProvider())
}

func TestSetupDisabledReturnsNoop(t *testing.T) {
	restoreOTelGlobals(t)
	resetOTelNoop()
	rt := Setup(context.Background(), Config{})
	if rt.Enabled() { t.Fatal("disabled config must be no-op") }
	if err := rt.Shutdown(context.Background()); err != nil { t.Fatal(err) }
}

func TestSetupExporterFailureSoftDegradesToNoop(t *testing.T) {
	restoreOTelGlobals(t)
	resetOTelNoop()
	boom := errors.New("collector unavailable")
	rt := setupWithFactories(context.Background(), Config{Enabled: true, ServiceName: "yanshi", SampleRatio: 1}, factories{
		trace: func(context.Context, Config) (sdktrace.SpanExporter, error) { return nil, boom },
		metric: func(context.Context, Config) (sdkmetric.Exporter, error) { return nil, boom },
	})
	if rt.Enabled() { t.Fatal("exporter failure must soft-degrade to no-op") }
}

func TestNormalizeConfig(t *testing.T) {
	// Pure function — no global state touched, no isolation needed.
	cfg := normalizeConfig(Config{Enabled: true, SampleRatio: 2})
	if cfg.ServiceName != "yanshi" || cfg.SampleRatio != 1 {
		t.Fatalf("normalized=%+v", cfg)
	}
	cfg = normalizeConfig(Config{Enabled: true, SampleRatio: -1})
	if cfg.SampleRatio != 0 { t.Fatalf("ratio=%v", cfg.SampleRatio) }
}
```

- [ ] **Step 3: 运行测试，确认 package 未实现**

Run: `go test ./internal/observe/otel -v`

Expected: FAIL，`Config`/`Setup`/`setupWithFactories` 未定义。

- [ ] **Step 4: 实现真实 SDK provider**

```go
// internal/observe/otel/otel.go
// Package otelobs owns yanshi's OpenTelemetry providers and OTLP exporters.
// Callers receive a Runtime even when setup fails: exporter failures are
// deliberately soft because observability must never prevent the agent booting.
package otelobs

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// Config is intentionally provider-neutral. Endpoint is the OTLP/HTTP
// host:port accepted by WithEndpoint, for example "127.0.0.1:4318".
type Config struct {
	Enabled bool
	Endpoint string
	ServiceName string
	SampleRatio float64
}

type factories struct {
	trace func(context.Context, Config) (sdktrace.SpanExporter, error)
	metric func(context.Context, Config) (sdkmetric.Exporter, error)
}

type Runtime struct {
	enabled bool
	traces *sdktrace.TracerProvider
	metrics *sdkmetric.MeterProvider
	noopTracer trace.TracerProvider
	noopMeter metric.MeterProvider
}

func normalizeConfig(cfg Config) Config {
	if cfg.ServiceName == "" { cfg.ServiceName = "yanshi" }
	if cfg.SampleRatio < 0 { cfg.SampleRatio = 0 }
	if cfg.SampleRatio > 1 { cfg.SampleRatio = 1 }
	return cfg
}

func traceExporter(ctx context.Context, cfg Config) (sdktrace.SpanExporter, error) {
	opts := make([]otlptracehttp.Option, 0, 1)
	if cfg.Endpoint != "" { opts = append(opts, otlptracehttp.WithEndpoint(cfg.Endpoint)) }
	return otlptracehttp.New(ctx, opts...)
}

func metricExporter(ctx context.Context, cfg Config) (sdkmetric.Exporter, error) {
	opts := make([]otlpmetrichttp.Option, 0, 1)
	if cfg.Endpoint != "" { opts = append(opts, otlpmetrichttp.WithEndpoint(cfg.Endpoint)) }
	return otlpmetrichttp.New(ctx, opts...)
}

var defaults = factories{trace: traceExporter, metric: metricExporter}

func Noop() *Runtime {
	return &Runtime{
		noopTracer: tracenoop.NewTracerProvider(),
		noopMeter: metricnoop.NewMeterProvider(),
	}
}

func installNoop() *Runtime {
	rt := Noop()
	otel.SetTracerProvider(rt.noopTracer)
	otel.SetMeterProvider(rt.noopMeter)
	return rt
}

func collectorAvailable(ctx context.Context, endpoint string) bool {
	if endpoint == "" { return true }
	probeCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(probeCtx, "tcp", endpoint)
	if err != nil { return false }
	_ = conn.Close()
	return true
}

func Setup(ctx context.Context, cfg Config) *Runtime {
	if !cfg.Enabled { return installNoop() }
	cfg = normalizeConfig(cfg)
	if !collectorAvailable(ctx, cfg.Endpoint) {
		slog.WarnContext(ctx, "otel collector unavailable; telemetry disabled", "error_type", "collector_unreachable")
		return installNoop()
	}
	return setupWithFactories(ctx, cfg, defaults)
}

func setupWithFactories(ctx context.Context, cfg Config, f factories) *Runtime {
	expTrace, err := f.trace(ctx, cfg)
	if err != nil {
		slog.WarnContext(ctx, "otel trace exporter unavailable; telemetry disabled", "error_type", "trace_exporter_setup")
		return installNoop()
	}
	expMetric, err := f.metric(ctx, cfg)
	if err != nil {
		_ = expTrace.Shutdown(ctx)
		slog.WarnContext(ctx, "otel metric exporter unavailable; telemetry disabled", "error_type", "metric_exporter_setup")
		return installNoop()
	}
	res := resource.NewWithAttributes(
		"",
		attribute.String("service.name", cfg.ServiceName),
	)
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(expTrace),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SampleRatio))),
		sdktrace.WithResource(res),
	)
	reader := sdkmetric.NewPeriodicReader(expMetric, sdkmetric.WithInterval(15*time.Second))
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(reader),
		sdkmetric.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	return &Runtime{enabled: true, traces: tp, metrics: mp}
}

func (r *Runtime) Enabled() bool { return r != nil && r.enabled }

func (r *Runtime) Tracer(name string) trace.Tracer {
	if r == nil || !r.enabled { return tracenoop.NewTracerProvider().Tracer(name) }
	return r.traces.Tracer(name)
}

func (r *Runtime) Meter(name string) metric.Meter {
	if r == nil || !r.enabled { return metricnoop.NewMeterProvider().Meter(name) }
	return r.metrics.Meter(name)
}

func (r *Runtime) Shutdown(ctx context.Context) error {
	if r == nil || !r.enabled { return nil }
	return errors.Join(r.traces.Shutdown(ctx), r.metrics.Shutdown(ctx))
}
```

关键真实性检查：

- resource 使用 `resource.NewWithAttributes("", attribute.String(...))`。
- trace 使用 `sdktrace.NewTracerProvider`、`WithBatcher`、`WithSampler`、`WithResource`。
- metric 使用 `sdkmetric.NewMeterProvider`、`NewPeriodicReader`、`WithResource`。
- exporter 使用 `otlptracehttp.New` 与 `otlpmetrichttp.New`。
- 不出现不存在的 `sdktrace.NewResource`、`sdktrace.Resource`。
- provider error body 不作为 attribute/log 值；日志只写固定 `error_type`。

- [ ] **Step 5: 运行 provider 测试并提交**

Run: `go test ./internal/observe/otel -v`

Expected: PASS；测试不访问真实 collector，失败 factory 确认 no-op 软降级。

```bash
git add go.mod go.sum internal/observe/otel/otel.go internal/observe/otel/otel_test.go
git commit -m "feat(observe): add OTLP OpenTelemetry runtime"
```

---

## Task 13: OBS2 集成 — turn/tool/retry spans + bootstrap 接线

**Files:**
- Create: `internal/observe/otel/instrument.go`
- Create: `internal/observe/otel/instrument_test.go`
- Modify: `internal/bootstrap/bootstrap.go`
- Modify: `internal/bootstrap/bootstrap_test.go`
- Modify: `internal/agent/orchestrator/orchestrator.go`
- Modify: `internal/api/http/ws.go`
- Modify: `internal/api/http/chat.go`
- Modify: `internal/tools/guard.go`
- Modify: `internal/llm/eino/resilient.go`
- Modify: `CLAUDE.md`
- Modify: `docs/feature-roadmap-codex-deepseek.md`

- [ ] **Step 1: 写 OTel bootstrap 装配测试**

```go
// append to internal/bootstrap/bootstrap_test.go
func TestBuildSetsUpOTelAndShutsDown(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	body := fmt.Sprintf(`
server: { http_addr: "127.0.0.1:0" }
storage: { sqlite_path: %q }
observability:
  otel:
    enabled: true
    endpoint: "127.0.0.1:4318"
    sample_ratio: 1
features:
  overrides:
    observe.otel_export: true
`, toYAMLPath(filepath.Join(dir, "yanshi.db")))
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil { t.Fatal(err) }

	app, err := Build(Options{ConfigPath: cfgPath, FakeModel: true})
	if err != nil { t.Fatal(err) }
	if app.OTel == nil { t.Fatal("otel runtime must be wired") }
	if err := app.Shutdown(context.Background()); err != nil { t.Fatal(err) }
}
```

`otlptracehttp.New`/`otlpmetrichttp.New` 创建 exporter 时通常不主动连接 collector；因此测试只断言 Runtime 已装配并能 shutdown，不把本机是否运行 collector 作为单元测试前提。

- [ ] **Step 2: 运行测试，确认 `App.OTel` 未定义**

Run: `go test ./internal/bootstrap -run TestBuildSetsUpOTelAndShutsDown -v`

Expected: FAIL，`App.OTel` 未定义。

- [ ] **Step 3: App 添加 OTel runtime 并在 Build/Shutdown 接线**

```go
// App struct
OTel *otelobs.Runtime
```

bootstrap.go import 块加入：

```go
"github.com/x6nux/yanshi/internal/observe/otel"
```

在 `Build` 内、HTTP server 构造前装配 OTel：

```go
otelRT := otelobs.Setup(context.Background(), otelobs.Config{
	Enabled: cfg.Observability.OTel.Enabled && featureReg.Enabled("observe.otel_export"),
	Endpoint: cfg.Observability.OTel.Endpoint,
	ServiceName: cfg.Observability.OTel.ServiceName,
	SampleRatio: cfg.Observability.OTel.SampleRatio,
})
```

`return &App{...}` 字面量加入：

```go
OTel: otelRT,
```

`Shutdown` 改为：

```go
func (a *App) Shutdown(ctx context.Context) error {
	if a.cancel != nil { a.cancel() }
	var errs []error
	if err := a.Server.Shutdown(ctx); err != nil { errs = append(errs, err) }
	if a.OTel != nil {
		if err := a.OTel.Shutdown(ctx); err != nil { errs = append(errs, err) }
	}
	if cerr := a.Store.Close(); cerr != nil { errs = append(errs, cerr) }
	return errors.Join(errs...)
}
```

bootstrap.go import 块加入标准库 `errors`。

- [ ] **Step 4: 新建无敏感属性的 span/metric helpers**

```go
// internal/observe/otel/instrument_test.go
package otelobs

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestToolSpanNeverRecordsErrorBody(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	otel.SetTracerProvider(tp)
	_, end := StartTool(context.Background(), "shell_run")
	end(errors.New("provider body contains sk-super-secret"))
	spans := recorder.Ended()
	if len(spans) != 1 { t.Fatalf("ended spans=%d", len(spans)) }
	span := spans[0]
	if span.Name() != "tool.shell_run" { t.Fatalf("name=%q", span.Name()) }
	for _, attr := range span.Attributes() {
		if strings.Contains(attr.Value.AsString(), "sk-super-secret") {
			t.Fatalf("secret leaked in attribute: %+v", attr)
		}
	}
	if len(span.Events()) != 0 {
		t.Fatalf("RecordError would leak error text; events=%+v", span.Events())
	}
}
```

```go
// internal/observe/otel/instrument.go
package otelobs

import (
	"context"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	obslog "github.com/x6nux/yanshi/internal/observe/log"
)

var (
	instrumentMeter = otel.Meter("github.com/x6nux/yanshi")
	sessionLatency, _ = instrumentMeter.Float64Histogram("yanshi.session.duration", metric.WithUnit("s"))
	turnLatency, _ = instrumentMeter.Float64Histogram("yanshi.turn.duration", metric.WithUnit("s"))
	toolLatency, _ = instrumentMeter.Float64Histogram("yanshi.tool.duration", metric.WithUnit("s"))
	tokenCounter, _ = instrumentMeter.Int64Counter("yanshi.llm.tokens", metric.WithUnit("{token}"))
	retryCounter, _ = instrumentMeter.Int64Counter("yanshi.llm.retry", metric.WithUnit("{attempt}"))
	errorCounter, _ = instrumentMeter.Int64Counter("yanshi.operation.errors", metric.WithUnit("{error}"))
)

func startOperation(ctx context.Context, spanName string, kind trace.SpanKind,
	histogram metric.Float64Histogram, attrs ...attribute.KeyValue) (context.Context, func(error)) {
	started := time.Now()
	ctx, span := otel.Tracer("github.com/x6nux/yanshi").Start(ctx, spanName,
		trace.WithSpanKind(kind), trace.WithAttributes(attrs...))
	if sc := span.SpanContext(); sc.IsValid() {
		ids := obslog.IDsFromContext(ctx)
		ids.TraceID = sc.TraceID().String()
		ctx = obslog.WithIDs(ctx, ids)
	}
	var once sync.Once
	return ctx, func(err error) {
		once.Do(func() {
			endAttrs := attrs
			if err != nil {
				errType := obslog.SafeErrorType(err)
				span.SetAttributes(attribute.String("error.type", errType))
				span.SetStatus(codes.Error, "operation failed")
				errorCounter.Add(ctx, 1, metric.WithAttributes(attribute.String("error.type", errType)))
				endAttrs = append(append([]attribute.KeyValue{}, attrs...), attribute.String("error.type", errType))
			}
			histogram.Record(ctx, time.Since(started).Seconds(), metric.WithAttributes(endAttrs...))
			span.End()
		})
	}
}

func StartSession(ctx context.Context) (context.Context, func(error)) {
	ids := obslog.IDsFromContext(ctx)
	attrs := make([]attribute.KeyValue, 0, 1)
	if ids.SessionID != "" { attrs = append(attrs, attribute.String("session.id", ids.SessionID)) }
	return startOperation(ctx, "agent.session", trace.SpanKindServer, sessionLatency, attrs...)
}

func SetSessionID(ctx context.Context, sessionID string) {
	if sessionID == "" { return }
	trace.SpanFromContext(ctx).SetAttributes(attribute.String("session.id", sessionID))
}

func StartTurn(ctx context.Context, modelName string) (context.Context, func(error)) {
	ids := obslog.IDsFromContext(ctx)
	attrs := make([]attribute.KeyValue, 0, 3)
	if ids.SessionID != "" { attrs = append(attrs, attribute.String("session.id", ids.SessionID)) }
	if ids.TurnID != "" { attrs = append(attrs, attribute.String("turn.id", ids.TurnID)) }
	if modelName != "" { attrs = append(attrs, attribute.String("model.name", modelName)) }
	return startOperation(ctx, "agent.turn", trace.SpanKindInternal, turnLatency, attrs...)
}

func StartTool(ctx context.Context, toolName string) (context.Context, func(error)) {
	return startOperation(ctx, "tool."+toolName, trace.SpanKindInternal, toolLatency,
		attribute.String("tool.name", toolName))
}

func RecordUsage(ctx context.Context, modelName string, prompt, cached, completion, reasoning int) {
	if prompt < 0 { prompt = 0 }
	if cached < 0 { cached = 0 }
	if cached > prompt { cached = prompt }
	if completion < 0 { completion = 0 }
	if reasoning < 0 { reasoning = 0 }
	base := []attribute.KeyValue{attribute.String("model.name", modelName)}
	for _, item := range []struct { kind string; value int }{
		{kind: "input", value: prompt - cached},
		{kind: "cache_hit", value: cached},
		{kind: "output", value: completion},
		{kind: "reasoning", value: reasoning},
	} {
		if item.value == 0 { continue }
		attrs := append(append([]attribute.KeyValue{}, base...), attribute.String("token.kind", item.kind))
		tokenCounter.Add(ctx, int64(item.value), metric.WithAttributes(attrs...))
	}
}

func RecordRetry(ctx context.Context, attempt, maxAttempts int, err error) {
	retryCounter.Add(ctx, 1, metric.WithAttributes(
		attribute.Int("retry.attempt", attempt),
		attribute.Int("retry.max", maxAttempts),
		attribute.String("error.type", obslog.SafeErrorType(err)),
	))
}
```

Run: `go test ./internal/observe/otel -run TestToolSpanNeverRecordsErrorBody -v`

Expected: PASS；span 只有固定 operation/tool/model/ID/error-type 属性，没有 prompt、args 或错误正文。

- [ ] **Step 5: 在同步 `Query` 路径覆盖完整 turn span**

orchestrator.go import 块加入：

```go
otelobs "github.com/x6nux/yanshi/internal/observe/otel"
```

Task 3 已定义 `prepareTurnContext(ctx) context.Context`，其职责仍然只是补 IDs 并按 profile → workroot → sub-agent runner → VCS 顺序注入；不要在返回 raw async iterator 的 `Events*` 函数里 `defer span.End()`，否则函数一返回 iterator，span 就会在 stream 尚未消费时提前结束。

把 `Query` 改为 named returns，并在 iterator 被完整 drain 的同步边界开/关 turn span：

```go
func (o *Orchestrator) Query(ctx context.Context, userMessage string) (answer string, retErr error) {
	ctx = o.prepareTurnContext(ctx)
	ctx, endTurn := otelobs.StartTurn(ctx, "")
	defer func() { endTurn(retErr) }()
	iter := o.runner.Query(ctx, userMessage)
	var acc finalOutputAccumulator
	for {
		ev, ok := iter.Next()
		if !ok { break }
		if ev.Err != nil {
			return "", fmt.Errorf("orchestrator: agent error: %w", ev.Err)
		}
		if ev.Output == nil || ev.Output.MessageOutput == nil { continue }
		mv := ev.Output.MessageOutput
		if mv.IsStreaming && mv.MessageStream != nil {
			msg, err := mv.GetMessage()
			if err != nil { return "", fmt.Errorf("orchestrator: drain stream: %w", err) }
			acc.observe(msg)
			continue
		}
		acc.observe(mv.Message)
	}
	return acc.finalize()
}
```

Streaming `Events*` 的 span lifetime 由 Step 9 的 WS/SSE drain 边界管理；这样同步和 streaming 两类入口都在真实完成点关闭 span。

- [ ] **Step 6: 给 `GuardedTool.Stream` 加 tool span**

`Stream` 改为：

```go
func (g *GuardedTool) Stream(ctx context.Context, argsJSON string) <-chan ToolChunk {
	ctx, end := otelobs.StartTool(ctx, g.name)
	if err := Authorize(ctx, guard.Action{Tool: g.name}, argsJSON); err != nil {
		end(err)
		ch := make(chan ToolChunk, 1)
		pushErrChunk(ch, err)
		close(ch)
		return ch
	}
	runCtx, cancel := context.WithTimeout(ctx, g.timeout)
	out := g.stream(runCtx, argsJSON)
	wrapped := make(chan ToolChunk, 16)
	tuiCB := ToolChunkCallbackFromContext(ctx)
	go func() {
		defer cancel()
		defer close(wrapped)
		var toolErr error
		for c := range out {
			if c.Err != nil { toolErr = c.Err }
			if tuiCB != nil { tuiCB(g.name, c) }
			wrapped <- c
		}
		end(toolErr)
	}()
	return wrapped
}
```

guard.go import 块加入：

```go
otelobs "github.com/x6nux/yanshi/internal/observe/otel"
```

`otelobs.StartTool` 内部通过全局 `otel.Tracer` 取得 provider（Task 12 的 `Setup` 已 `otel.SetTracerProvider`），所以 GuardedTool 自身不需要 tracer 字段或构造选项——既保持 `NewGuardedTool` 签名不变，也避免 bootstrap 遍历 `allTools` 做类型断言。`end(toolErr)` 在 `otelobs` 内部只把 `error.type`（`%T`）写入 attribute，不携带 body。

- [ ] **Step 7: 在 `sleepRetry` 中加 retry 计数（唯一重试单点）**

`internal/llm/eino/resilient.go` 现有 import 块（`internal/llm/eino/resilient.go:1-14`）只含 stdlib + cloudwego/eino。在 import 块末尾追加 `otelobs` 别名导入（不要引入 `go.opentelemetry.io/otel/metric` —— 我们只通过 `otelobs.RecordRetry` 这一个 helper 访问）：

```go
import (
	“context”
	“errors”
	“fmt”
	“io”
	“math”
	“net”
	“strings”
	“time”

	“github.com/cloudwego/eino/components/model”
	“github.com/cloudwego/eino/schema”

	otelobs “github.com/x6nux/yanshi/internal/observe/otel”
)
```

`sleepRetry` 在 callback 调用后、sleep 前增加一行：

```go
	otelobs.RecordRetry(ctx, attempt, maxAttempts, err)
```

`otelobs.RecordRetry` 内部使用全局 `otel.Meter`，attribute 只含 `retry.attempt`、`retry.max`、`error.type`（`obslog.SafeErrorType(err)`），不含 `err.Error()`。resilient.go 不再需要直接依赖 `go.opentelemetry.io/otel/metric` 或新增字段——这既满足 CLAUDE.md “重复逻辑必须抽成公共函数”的约束，也避免在 `ResilientChatModel` 上多开 setter。

- [ ] **Step 8: bootstrap 装配 OTel、features gate 与 orchestrator/tools 接线**

Task 8 已经装配 features registry；这里增加 OTel gate（与 Step 3 相同的唯一 setup，不要重复调用）：

```go
otelRT := otelobs.Setup(context.Background(), otelobs.Config{
	Enabled: cfg.Observability.OTel.Enabled && featureReg.Enabled("observe.otel_export"),
	Endpoint: cfg.Observability.OTel.Endpoint,
	ServiceName: cfg.Observability.OTel.ServiceName,
	SampleRatio: cfg.Observability.OTel.SampleRatio,
})
```

把 `OTel: otelRT` 存进 `App`，并按 Step 3 的顺序在 `Server.Shutdown` 后、`Store.Close` 前 flush/shutdown telemetry。GuardedTool 不需要 bootstrap 注入（Step 6 使用全局 `otel.Tracer`），resilient model 也不需要注入（Step 7 使用全局 `otel.Meter`）；避免对 `*einollm.ResilientChatModel` 与 `*tools.GuardedTool` 做类型断言回填。

- [ ] **Step 9: 在 WS/SSE stream drain 边界关闭 session/turn spans，并记录 usage metrics**

`internal/api/http/ws.go` import 块加入：

```go
otelobs "github.com/x6nux/yanshi/internal/observe/otel"
```

创建连接 context 后打开 session span：

```go
connCtx, cancel := context.WithCancel(r.Context())
defer func() { cancel() }()
connCtx, endSession := otelobs.StartSession(connCtx)
defer func() { endSession(nil) }()
```

`ensureSession` 成功创建/restore session 后调用：

```go
otelobs.SetSessionID(connCtx, cs.sessionID)
```

在 `user_message` 分支 `turnCtx, release := makeTurnCtx()` 后、Task 3 的 IDs 注入后打开 turn span：

```go
turnCtx, endTurn := otelobs.StartTurn(turnCtx, cs.displayModel())
var turnFailed bool
```

`ClassifyEventsWithUsage` emit callback 遇到 error frame 时设置：

```go
if f.Type == "error" { turnFailed = true }
```

在现有 `release()` 前关闭：

```go
var turnErr error
if turnCtx.Err() != nil {
	turnErr = turnCtx.Err()
} else if turnFailed {
	turnErr = errors.New("turn failed")
}
endTurn(turnErr)
release()
```

把 Task 9 的 helper 签名更新为带 context，并在每个 provider usage 入口同时记成本 ledger 与 token metric：

```go
func (cs *connSession) addProviderUsage(ctx context.Context, s *Server, u orchestrator.TurnUsage) {
	priced := usageForPricing(u)
	if priced.Prompt <= 0 && priced.Cached <= 0 && priced.Completion <= 0 { return }
	cs.billing.Add(priced)
	otelobs.RecordUsage(ctx, cs.displayModel(), priced.Prompt, priced.Cached, priced.Completion, priced.Reasoning)
	cost, known := einollm.CostOK(s.priceTab, cs.displayModel(), priced)
	if !cs.hasBilledUsage {
		cs.costKnown = known
		cs.hasBilledUsage = true
	} else {
		cs.costKnown = cs.costKnown && known
	}
	if known { cs.costUSD += cost }
}
```

WS `onUsage` 与 judge 调用分别改为：

```go
cs.addProviderUsage(turnCtx, s, u)
cs.addProviderUsage(turnCtx, s, ju)
```

测试中调用使用 `context.Background()`。

SSE 在 billing model 确定后打开 turn span并让所有 attempt 共用该 context：

```go
turnCtx, endTurn := otelobs.StartTurn(r.Context(), billingModel)
var turnFailed bool
```

把 `tools.WithErrCounter(r.Context())` 改为 `tools.WithErrCounter(turnCtx)`；SSE `onUsage` 在 `ledger.Add(priced)` 后加入：

```go
otelobs.RecordUsage(turnCtx, billingModel, priced.Prompt, priced.Cached, priced.Completion, priced.Reasoning)
```

emit callback 遇到 error frame 时同时 `turnFailed = true`；最终 status 发送前关闭：

```go
var turnErr error
if turnCtx.Err() != nil {
	turnErr = turnCtx.Err()
} else if turnFailed {
	turnErr = errors.New("turn failed")
}
endTurn(turnErr)
```

这使 session/turn/tool spans 形成父子关系，latency/error metrics 在真实完成点记录；provider usage callback 仍是 Task 9 的唯一 token/cost 入口，不会因额外遍历 stream chunk 双计。

- [ ] **Step 10: 运行全套测试并提交**

Run: `go test ./internal/bootstrap ./internal/agent/orchestrator ./internal/tools ./internal/llm/eino -v`

Expected: PASS；现有 fake-model 流不变；OTel no-op provider 在没有 collector 时也不影响 turn。

```bash
git add internal/bootstrap/bootstrap.go internal/bootstrap/bootstrap_test.go \
  internal/agent/orchestrator/orchestrator.go \
  internal/tools/guard.go \
  internal/llm/eino/resilient.go \
  CLAUDE.md docs/feature-roadmap-codex-deepseek.md
git commit -m "feat(observe): wire OTel spans, retry metric, and tracer injection"
```

- [ ] **Step 11: 更新 CLAUDE.md 段落**

在 `### 组合根` 或紧接其后的子段加入：

```markdown
### Observability（`internal/observe`）

OBS1 logger（`observe/log`）通过 `slog.Handler` 包装做脱敏，并在 turn context
中注入 trace/session/turn/tool ID（`WithIDs`/`IDsFromContext`）。敏感 key 与
值（prompt、args、api key、shell 命令、host、provider error body）一律替换为
`[REDACTED]`；provider error 仅以 `%T` 类型入日志。装配点在 `bootstrap.Build`
（config load 前装安全默认 logger，成功后按配置重装）。

OBS2 OTel（`observe/otel`）用真实 SDK（`sdktrace.NewTracerProvider` + OTLP HTTP
exporter）。exporter setup 失败软降级为 no-op；WS 连接开 `agent.session` span，
WS/SSE 在真实 stream drain 边界开关 `agent.turn` span，同步 `Orchestrator.Query`
也覆盖完整 iterator；`GuardedTool.Stream` 开 `tool.<name>` span；
`ResilientChatModel.sleepRetry` 是唯一 retry 计数点（`yanshi.llm.retry` counter，
attribute 只携带 `error.type` 不携带 body）。instrument helper 会把 OTel trace ID
写回 `obslog.IDs`，使结构化日志与 span 可关联。新增重试循环或工具入口前请复用
这些 spans/metric，不要另立循环。
```

- [ ] **Step 12: roadmap 完成标记**

在 `docs/feature-roadmap-codex-deepseek.md` 的 Batch C4 段落，按该文件已有 checkbox 格式勾选 OBS1/OBS2/OBS3/COST1/O07 已落地；只修改这五个条目的状态，不重排其他 batch。

- [ ] **Step 13: Self-Review — spec 覆盖与占位扫描**

对照本计划自查：

- OBS1 logger + IDs + 脱敏 + guard/orchestrator/api 迁移 — Task 1/3 覆盖。
- 非日志输出（doctor RenderText/exec stdout/SSE event/TUI render/工具 result/VCS hash）保持不变 — Task 1/3/11 显式排除。
- OBS2 OTel session/turn/tool trace + latency/token/retry/error span/metric + OTLP + 软降级 — Task 12/13 覆盖。
- OBS2 复用 `RetryCallback`/`sleepRetry`，不新建重试循环 — Task 13 Step 6 在 sleepRetry 单点加 counter。
- OBS3 features registry + strict 未知 flag 报错包含 flag 名 + `/features` 控制帧 proto→ws→wsbackend→StreamEvent→TUI 同步 + SSE 明确失败 + `enabled=false` 不丢 — Task 4/7/9/10 覆盖。
- COST1 pricing table + cache-split 计费 + unknown N/A + 配置覆盖 + `/cost`、`/stats` — Task 5/6/8/9/10 覆盖。
- COST1 不变量（每次 provider usage 分别计 max/0+/0+；stream chunk 末值；judge usage；unknown N/A） — Task 5/9 显式保持。
- O07 doctor sandbox/MCP/LSP/port/permissions + 既有数据模型 + sandbox warn + 增量 — Task 11 覆盖。

对全文 grep 以下字符串，确认零命中：

```bash
grep -nE 'TODO|TBD|FIXME|XXX|同上|执行时再查|newTestOrchestrator|testSpinner|einollmModelPricing|cfg.ModelName|sdktrace.Resource|sdktrace.NewResource|attrs.Len|rows := //' docs/superpowers/plans/2026-07-21-c4-observability-ops.md
```

Expected: 0 行命中（或仅 Step 11 本身的禁词列表 — 把该段改成反引号包裹可避免误报）。

类型一致性复核：

- `feature.Spec.Key/Stage/Default/Owner` 与 `feature.Row.Key/Stage/Enabled/Owner` 命名一致（Task 4、Task 7、Task 9、Task 10）。
- `proto.FeatureRow` 与 `features.Row` 字段一致。
- `einollm.ModelPricing`、`einollm.Ledger.Add`、`einollm.CostOK`、`einollm.DefaultPricing`、`einollm.MergePricing`、`einollm.FormatCost` 在 Task 5、6、8、9、10、11 中使用方式一致。
- `store.BillingMeta` 字段在 Task 6、Task 9 中一致。
- `proto.FeaturesSetPayload.Enabled *bool` 在 Task 7、9、10 中一致（`enabled=false` wire 不丢）。
- `log.IDs{TraceID,SessionID,TurnID,Tool}` 与 `log.WithIDs` 在 Task 1、Task 3、Task 13 中使用方式一致。

风险与回滚：

- OTel exporter 失败软降级为 no-op，runtime 不会阻塞 boot。
- features strict 模式在启动时拒绝未知 flag，避免运行时发明 flag。
- COST1 Ledger 与 WS context counter 完全分离；任何 billed 计算 bug 都不会污染既有 `tokensIn` 覆盖语义。
- 所有现有 `UpdateSessionMeta` 调用点（WS、SSE 测试）都接收新 `BillingMeta` 参数；旧签名删除，编译器强制 catch。
- OTel 接入通过全局 `otel.Tracer`/`otel.Meter`；`Setup` 是唯一一次 `otel.SetTracerProvider`/`SetMeterProvider` 调用。instrument helpers 用 package-level `otel.Meter(...)` 与 `otel.Tracer(...)` 取得 provider，业务代码（orchestrator/guard/resilient）不再各自拿 `*Runtime` 字段，组合根也不会出现类型断言回填。

---

## 最终交付与执行清单

- **Task 数**：13（Task 1 OBS1 logger → Task 13 OBS2 集成 + WS/SSE span 接线 + 文档 + 自审）。
- **依赖顺序**：Task 1 → Task 2 → Task 3 → Task 4 → Task 5 → Task 6 → Task 7 → Task 8 → Task 9 → Task 10 → Task 11 → Task 12 → Task 13。Task 12 只依赖 Task 1 的 `obslog.SafeErrorType`，可与 Task 4–11 并行起步；但 Task 13 必须在 Task 8（features registry + pricing 注入）、Task 9（per-usage 计费 helper）与 Task 12（OTel provider + instrument helpers）之后。
- **已消除的占位**：旧初稿的全部 `同上`、`执行时再查`、`return ""`、`rows := // ...`、`type Usage = pricingUsage`、`einollmModelPricing`、伪造模型 ID、错误 Opus 价格（$15/$75）、`sdktrace.Resource`/`sdktrace.NewResource`、`attrs.Len`、`cfg.ModelName`、orchestrator `sessionID` 字段引用、span.go 空占位、SSE 半支持 `/features`、handler 类型断言回填 price table、`cost==0` N/A 歧义、doctor 测试 `return ""` fixture、GuardedTool 自带 tracer 字段回填、`ResilientChatModel.WithRetryMeter` 回填 — 均不再出现。
- **保留的待决策点**：
  1. **价格表更新渠道**：当前内置 8 个真实 Anthropic 模型 ID，未来新增模型仍需修改 `internal/llm/eino/pricing.go`。可考虑在 release 流程中从 Anthropic 公开价格 JSON 自动生成，但不在本 batch 范围。
  2. **OTel sampler 默认 ratio**：默认 `1.0`（全量）。生产部署可能需要调低；通过 `config.yaml` `observability.otel.sample_ratio` 控制，运维侧决策。
  3. **features `/features enable`** 在 SSE 下明确失败；未来若 SSE 接入可观察的 control 通道需另议，但当前 SSE 是 stateless POST-per-turn，无 control frame 通道。
  4. **doctor LSP probe** 仅检测 `gopls`；其他 LSP（rust-analyzer、pyright 等）按需扩展，但当前 yanshi 是 Go 仓库，保留 gopls 单点。
  5. **guard audit log 频率**：权限拒绝固定记录到 logger（`reason_code` 字段）。生产环境若 QPS 极高，可加采样；当前每次都记。

执行者按 Task 1 → 13 顺序进行即可；每个 Task 自带 TDD（先红后绿）与独立 commit。
