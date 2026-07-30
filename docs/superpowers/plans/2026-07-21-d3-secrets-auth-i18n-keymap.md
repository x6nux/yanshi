# Batch D3 — 密钥/认证/i18n/键位 (S10/O03/I18N1/C15) 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:subagent-driven-development` (推荐) 或 `superpowers:executing-plans` 逐 Task 实施。每完成一个 Task，先运行该 Task 的定向测试，再提交；不要把多个 Task 合成一个大提交。所有 Task 都以 RED→GREEN 节奏推进：先写失败测试、跑一次确认 RED（编译失败或断言失败都算），再补实现转 GREEN。

**Goal:** 完成 roadmap §15 的四项能力：S10 安全凭据存储与统一脱敏、O03 provider-neutral 认证生命周期、I18N1 `en`/`zh-Hans` UI locale、C15 可配置 keymap/Vim/高对比主题，并保持现有 fake-model、WS/SSE、TUI、doctor 行为兼容。

**Architecture (本批最终架构，已落 20 项结构性修复):**

1. **S10 secrets** — 新建叶子包 `internal/secrets`。核心只依赖标准库 + `golang.org/x/crypto/argon2` (go.sum 已锁定 `v0.36.0`)。`Store` 为 adapter 接口，内建 `osKeyringStore` (keyring 不可用时软降级，**不**阻塞引导，只警告)、`FileStore` (加密文件 fallback，Argon2id + AES-256-GCM，文件头携带 KDF 名称与参数；未知版本/KDF fail-closed，为后续迁移保留显式分派点)。`Redactor` 在配置解析后登记秘密，并在 D3 stderr 日志、WS 出站、SSE 出站、SQLite 写入 (**含 `CreateSession` 与 `UpdateSessionTitle`，不只 `AppendMessage`**) 统一脱敏；`safeError` 通过 `Unwrap() error` 保留 raw cause 以维持 `errors.Is/As` 链，**但调用方把 `Unwrap()` 结果再次 `Error()` 仍会泄漏原文**，因此 `safeError` 仅作为边界返回值，业务逻辑不得 re-stringify raw cause。
2. **O03 auth** — 新建 `internal/auth`，**依赖 `internal/secrets`**。`Manager` 支持 API key、RFC 8628 device authorization (`Clock` / `Sleeper` 注入以便测试时序)、`Status`、`Logout`。凭据源 (`CredentialSource`) 显式区分 `secret://service/account`、`env://VAR`、`legacy-insecure` (opt-in，文本字面量)；无安全 backend 时拒绝 raw literal 引用。`Manager` 在 `bootstrap.Build` 中**先于** `einollm.BuildProviders(cfg)` 装配，把解析后的 `APIKey` 写回 `cfg.LLM.Providers[i].APIKey`，确保 `BuildProviders` 拿到的 cfg 已是最终明文。
3. **I18N1 i18n** — 新建 `internal/i18n`，`go:embed` 外置 JSON catalog (`en.json` / `zh-Hans.json`)。`Bundle` 负责 locale 归一化 (`auto`/`en`/`zh-Hans`)、自动探测与 fallback。**`auto` 持久化原值，每次启动重算 effective locale** (跨两次启动改 `LC_ALL` / `LANG` 会得到不同结果)。TUI 持有 UI `Bundle`，模型输出语言由独立 **`i18n.output_language`** 统一配置名控制。`helpEntry` 重构为在 `cmdHelp` 时持有预渲染行，避免每个 entry 都得带 bundle。
4. **C15 keymap** — 新建叶子包 `internal/keymap` 提供 semantic `Action`、完整候选校验与 `VimResult{Action, Consumed}`；TUI adapter 只负责 Bubble Tea/command/persistence 接线。runtime key spelling 委托本地 Bubble Tea fork 的 `tea.KeyMsg.String()`，paste/multi-rune 永不触发 shortcut。**OBS3 feature-flag 尚未实现** (`internal/features/` 不存在)，C15 正式豁免 OBS3 前置门控并精确同步 roadmap。`/keymap reset` 重建 built-in defaults，并持久化 `KeymapReset` tombstone 屏蔽低优先级 project bindings；preferences 使用 injected replace seam 做跨平台原子替换测试。Visual mode 在 D3 仅跟踪模式并把 j/k 映射为 viewport navigation，不承诺 selection extension。

**Tech Stack:** Go 1.26.4；标准库 `crypto/aes`/`cipher`/`crypto/rand`/`encoding/binary`/`encoding/json`/`embed`/`net/http`/`os`/`path/filepath`/`sync`/`time`/`context`/`fmt`/`strings`/`strconv`；`golang.org/x/term` (无回显口令读取)；`golang.org/x/crypto/argon2` (KDF，go.sum 已锁 `v0.36.0`)；`golang.org/x/sys/windows`（Windows `MoveFileEx` 原子替换）；OS keyring adapter 固定 `github.com/zalando/go-keyring v0.2.6`，并提供 `nokeyring` build-tag stub。

**Spec:** `docs/feature-roadmap-codex-deepseek.md` §15 `[S10]`/`[O03]`/`[I18N1]`/`[C15]`。**OBS3 同步:** Task 13 精确修改现有 OBS3 的 `设计` 行和 C15 的 `设计`/`依赖`/`验收` 行，记录“C15 在 D3 正式豁免 OBS3 feature-flag gate”；不追加虚构字段或与原文格式脱节的 bullet。

---

## 已确认的真实落点与约束

- `internal/config/config.go:84` 的 `ProviderConfig.APIKey string` YAML 字段为 `api_key`，`os.ExpandEnv` 在反序列化前展开 `${VAR}`；新方案必须兼容该路径，**且** raw literal 在无安全 backend 时必须被拒绝。
- `internal/llm/eino/provider.go:45` 的真实入口是 `BuildProviders(cfg *config.Config)`；secret ref 必须在调用它之前解析成仅驻留内存的 `APIKey`，写回 `cfg.LLM.Providers[i].APIKey`。
- `internal/bootstrap/bootstrap.go` 的真实组合根是 `Build(opts Options)`；它已负责 `config → store → vcs → model → tools → orchestrator → http`。`einollm.BuildProviders(cfg)` 在 L126 调用，auth.Manager 必须在 L120 之前装配并已写回 cfg。S10/O03 在该函数中插入两段（Task 5 的 secrets 装配段与 Task 8 的 auth.Manager 接管段，**Task 8 替换 Task 5 的内联 resolver loop**），并新增 `App.Redactor *secrets.Redactor` + `App.Auth *auth.Manager` 字段；测试侧需要新增 `Options.Cfg *config.Config` 入参（不经磁盘加载直接注入）。
- `internal/api/http/ws.go:1471` 的 `(*wsConn).write` 是 WS 唯一出站边界 (已持写 mutex，无脱敏)；这是 WS 侧唯一的脱敏拦截点。
- `internal/api/http/chat.go:231` 的 `writeSSEFrame` 是 SSE 出站边界，**共 10 处调用** (L98 / L106 / L109 / L177 / L200 / L212 / L222 / L223 直接调用 + L58 / L66 经 `writeSSEError` L246 间接调用)；改造方案在 `writeSSEFrame` 与 `writeSSEError` 内部统一调 redactor，避免 10 处都改。
- `internal/store/session.go:20/34/50` 的 `CreateSession(title)`、`AppendMessage(sessionID, seq, role, content)`、`UpdateSessionTitle(sessionID, title)` 是 DB 明文防线，**全部** 需要脱敏 (不只 `AppendMessage`)。
- `internal/cli/tui/commands.go:588` 的 `helpEntry` 当前为 `struct{}`，`render(_ int, _ spinner.Model)` 不接收 bundle；C15/I18N 重构为 `cmdHelp` 时预渲染行，避免每 entry 持 bundle。
- `internal/cli/tui/model_test.go:68` 的 `recordingSession` 字段为 `sentText []string` (**不是** `lastPrompt`) 和 `frames []proto.ClientFrame`；测试断言走 `r.sentText[len(r.sentText)-1]` 与 `r.frames`。
- `internal/cli/tui/permissions.go:161` 的 `persistPermMode = true` 是测试可禁用开关的同款模式（preferences 持久化照搬）；`permModeFile()` 返回 `os.UserConfigDir()/yanshi/perm_mode.json`。
- `internal/cli/ssebackend.go:179` 明确拒绝控制帧；O03 采用 CLI 子命令而不是新增聊天控制帧，避免破坏 WS/SSE 词表对称性。
- `internal/features/` **不存在** (OBS3 未实现)；C15 不依赖 feature-flag 门控，正式豁免并同步 roadmap。
- `go.sum` 已锁定 `golang.org/x/crypto v0.36.0` (argon2id、pbkdf2 均可用)；**不**需要新依赖。
- 本计划不会假装标准库或 `x/sys` 提供跨 Windows/macOS/Linux 的统一 keyring API。跨平台实现经过 `Store` adapter；具体依赖在 Task 2 的决策门锁定 (先跑 `CGO_ENABLED=0 go build ./...` 验证，失败则切 build-tag fallback)。

---

## File Structure

| 文件 | 职责 | 新建/改 |
|---|---|---|
| `internal/config/config.go` | 新增 `SecretsConfig` / `AuthConfig` / `I18NConfig` / `TUIConfig` 字段；`OutputLanguage` 统一在 `i18n.output_language` | 改 |
| `internal/config/config_test.go` | 新字段解析单测 | 改 |
| `config.example.yaml` | 给出 `secrets`/`auth`/`i18n`/`tui.bindings` 的可复制示例，binding 方向固定为 `key: action` | 改 |
| `internal/secrets/secrets.go` | `Store` 接口、`Redactor`、`SafeLogger`、`safeError`、`CredentialRef` | 新建 |
| `internal/secrets/file_store.go` | `FileStore`：Argon2id + AES-256-GCM，文件头含 KDF 名/参数；未知版本/KDF fail-closed | 新建 |
| `internal/secrets/file_store_replace_unix.go` / `file_store_replace_windows.go` | 加密文件在 Unix/Windows 上的原子覆盖 adapter | 新建 |
| `internal/secrets/file_store_test.go` | 加密/解密、KDF 头、口令错误、损坏文件、注入 replace failure 后磁盘与内存均回滚 | 新建 |
| `internal/secrets/keyring.go` | `Store` 接口（含 `Available()`）、`NewOSKeyringStore` 入口 | 新建 |
| `internal/secrets/keyring_enabled.go` | `//go:build !nokeyring` 真实 keyring 调用 (zalando/go-keyring) | 新建 |
| `internal/secrets/keyring_disabled.go` | `//go:build nokeyring` 软降级 stub (`Available` + Get/Set/Delete 全返回 `ErrKeyringUnavailable`) | 新建 |
| `internal/secrets/manager.go` | `Manager`：auto 模式优先 OS keyring，失败软降级到 fileStore | 新建 |
| `internal/secrets/manager_test.go` | auto/force-file/no-backend 三模式 + 软降级路径 | 新建 |
| `internal/secrets/redactor_test.go` | redactor 注册、替换、safeError.Unwrap 链 | 新建 |
| `internal/secrets/cli.go` | `readSecret` (无回显 stdin)、`authCommand`、`RunAuth` | 新建 |
| `internal/secrets/cli_test.go` | `TestMainAuth`：完整 CLI auth 流程 (stdin / --api-key-stdin / cleanup) | 新建 |
| `internal/api/http/ws.go` | `wsConn.write` 在 `json.Marshal` 后、`WriteMessage` 前对完整 wire JSON 调 `RedactJSON` (新增 `redactor *secrets.Redactor` 字段) | 改 |
| `internal/api/http/chat.go` | `writeSSEFrame` / `writeSSEError` 内部调 redactor；server 持 redactor 引用 | 改 |
| `internal/api/http/server.go` | `Server` 加 `redactor *secrets.Redactor` 字段；构造函数注入 | 改 |
| `internal/store/session.go` | `CreateSession` / `AppendMessage` / `UpdateSessionTitle` 三处调 redactor | 改 |
| `internal/store/store.go` | `Store` 持 `redactor *secrets.Redactor` 字段 | 改 |
| `internal/store/session_test.go` | 三处脱敏测试 (含 CreateSession 与 UpdateSessionTitle) | 改 |
| `internal/auth/auth.go` | `Manager`、`CredentialSource`、metadata Save/Load/Delete port、metadata-aware `Status`、补偿式 `Logout`/device-token commit | 新建 |
| `internal/auth/auth_test.go` | Manager 单测：API key、`secret://`、`env://`、`legacy-insecure`、raw literal 拒绝 | 新建 |
| `internal/auth/lifecycle_test.go` | Status/Logout metadata lifecycle、旧 token 恢复、`errors.Join` rollback | 新建 |
| `internal/auth/device.go` | `DeviceProvider` 接口、`GenericRFC8628Provider`（HTTPS-only、no redirect、1 MiB body cap）、`Clock`/`Sleeper` 注入 | 新建 |
| `internal/auth/device_test.go` | device flow：expiry/slow_down/cancel/leak/body-limit/metadata rollback/injected-clock | 新建 |
| `internal/store/auth.go` | `authSQLiteAdapter` + `AuthMetadataFromDB(db)`：Save/Load/Delete non-secret source/expiry | 新建 |
| `internal/store/auth_test.go` | schema exact columns + upsert/load/delete/not-found，断言 metadata 无 secret 列 | 新建 |
| `internal/store/store.go` | SQLite `schema` 新增 `auth_metadata` 表；Store 同时持 transcript redactor | 改 |
| `internal/bootstrap/bootstrap.go` | `config → store → secrets → auth/metadata → credential resolution → ProviderBuilder`，并以 `SafeLogger` 输出 non-fatal warnings | 改 |
| `internal/bootstrap/bootstrap_test.go` | 装配顺序、disabled device gate、output language/default instruction、provider seam | 改 |
| `internal/cli/session.go` / `session_test.go` | in-process serve error 通过 process redactor + `SafeLogger` 输出并有 sentinel 门禁 | 改 |
| `internal/i18n/catalog/en.json` | 英文 catalog（与唯一 manifest exact equality） | 新建 |
| `internal/i18n/catalog/zh-Hans.json` | 简体中文 catalog（与唯一 manifest exact equality） | 新建 |
| `internal/i18n/i18n.go` | `Bundle`、locale 归一化、真正 `LC_ALL > LANG`、`auto` 每次构造重算、fallback | 新建 |
| `internal/i18n/i18n_test.go` | locale 归一化、catalog exact set、auto 跨 reload、C/POSIX | 新建 |
| `internal/cli/tui/commands.go` | `helpEntry` 重构为预渲染行；`/keymap`/`/vim`/`/contrast` 命令 | 改 |
| `internal/cli/tui/commands_test.go` | helpEntry/catalog 回归 + 新命令/persistence failure 测试 | 改 |
| `internal/cli/tui/preferences.go` | sparse `Preferences`、四层 override + defaults、injected 原子保存、reset tombstone | 新建 |
| `internal/cli/tui/preferences_test.go` | 五级优先级、optional bool、auto reload、replace failure/旧文件/tmp cleanup | 新建 |
| `internal/cli/tui/preferences_replace_unix.go` / `preferences_replace_windows.go` | Unix rename / Windows `MoveFileEx(REPLACE_EXISTING|WRITE_THROUGH)` | 新建 |
| `internal/cli/tui/keymap.go` | TUI adapter：valid subset、typed diagnostics、Vim consumed dispatch、reset defaults | 新建 |
| `internal/cli/tui/keymap_test.go` | real `tea.KeyMsg`、paste/multi-rune、unsupported name、localized diagnostic、reset | 新建 |
| `internal/keymap/keymap.go` | semantic `Action`、Bubble Tea fork normalization、raw override deterministic validation | 新建 |
| `internal/keymap/keymap_test.go` | conflict/invalid/normalized duplicate、真实 Ctrl key、paste guard | 新建 |
| `internal/keymap/vim.go` / `vim_test.go` | `VimResult{Action,Consumed}` modal machine；D3 Visual 只做 viewport j/k | 新建 |
| `internal/agent/orchestrator/orchestrator.go` | 导出唯一 `DefaultInstruction` | 改 |
| `cmd/yanshi/main.go` | production auth/doctor dispatcher、status expiry、TUI optional flags | 改 |
| `cmd/yanshi/main_test.go` | API-key/device E2E、metadata lifecycle、doctor、DB main/WAL no-secret scan | 改 |
| `internal/cli/doctor.go` / `doctor_test.go` | D3 五项检查，keymap raw error 固定安全文案 | 改 |
| `docs/feature-roadmap-codex-deepseek.md` | 精确修改 OBS3/C15 现有设计/依赖/验收行 | 改 |

---

## 任务依赖图

```
Task 1 (S10-L1: config schema + Store/Redactor/SafeLogger contracts)
  └─ Task 2 (S10-L2: keyring + Argon2id/AES-GCM FileStore + OS replace)
      └─ Task 3 (S10-L3: secrets Manager + bounded CLI input)
          └─ Task 4 (S10-L4: WS/SSE/SQLite serialized-boundary redaction)
              └─ Task 5 (S10-L5: bootstrap ordering + SafeLogger warnings/session sink)
                  └─ Task 6 (O03-L1: auth.Manager + provider-neutral credential refs)
                      └─ Task 7 (O03-L2: RFC 8628 + timing/limit/leak/commit compensation)
                          └─ Task 8 (O03-L3: SQLite metadata + bootstrap auth/ProviderBuilder)
                              └─ Task 9 (O03-L4: production auth CLI + loopback E2E)
                                  └─ Task 14 (O03-L5: metadata Load/Delete + Status/Logout compensation)

Task 1 ─ Task 10 (I18N1-L1: embedded catalogs + exact manifest + auto locale)
                 └─ Task 11 (I18N1-L2: TUI prefs/locale + output-language split)

Task 1 ─ Task 12 (C15-L1: semantic keymap core + VimResult)
Task 10 + Task 11 + Task 12
                 └─ Task 13 (C15-L2: TUI adapter/commands/doctor/roadmap exemption)

Task 2 + Task 4 + Task 7 + Task 9 + Task 10 + Task 11 + Task 13 + Task 14
                 └─ Task 15 (explicit D3 security/acceptance gates)
```

`Task 14` 必须在 Task 9 后实施，因为它扩展 Task 7/8 的 `MetadataStore` 并修改现有 CLI status/logout contract；`Task 15` 只加门禁，不补生产逻辑。

---

## Task 1 — S10-L1: secrets 配置 schema + Redactor + safeError

**结构性修复落点:** #4 (统一 safe logger/error sink 覆盖 SQLite)、#11 (writeSSEFrame 10 处覆盖的基础)、#3 (CredentialSource/Ref 类型提前在此定义)。

### Files

- Create `internal/secrets/secrets.go`
- Create `internal/secrets/redactor_test.go`
- Modify `internal/config/config.go` (加 `SecretsConfig` / `AuthConfig` / `I18NConfig` / `TUIConfig` 与默认值)
- Modify `internal/config/config_test.go` (新增字段解析、`tui.bindings` 方向、默认值测试)
- Modify `config.example.yaml` (新增 D3 配置示例；binding 固定为 `key: action`)

### Failure Test (RED)

`internal/secrets/redactor_test.go` — `TestRedactor_ReplacesAllRegisteredSecrets` 断言：

```go
package secrets

import (
    "errors"
    "strings"
    "testing"
)

func TestRedactor_ReplacesAllRegisteredSecrets(t *testing.T) {
    r := NewRedactor()
    r.Register("sk-live-1234567890abcdef")
    r.Register("Bearer abcdef0123456789")

    got := r.Redact("error: api call with sk-live-1234567890abcdef failed; header Bearer abcdef0123456789")
    if strings.Contains(got, "sk-live-") || strings.Contains(got, "abcdef0123456789") {
        t.Fatalf("redactor leaked secret: %q", got)
    }
    if !strings.Contains(got, "[REDACTED]") {
        t.Fatalf("expected [REDACTED] marker, got %q", got)
    }
}

func TestSafeError_PreservesUnwrapChain(t *testing.T) {
    sentinel := errors.New("raw cause with sk-live-1234567890abcdef")
    r := NewRedactor()
    r.Register("sk-live-1234567890abcdef")
    safe := r.SafeError(sentinel)
    if !errors.Is(safe, sentinel) {
        t.Fatalf("errors.Is broken: SafeError must Unwrap to raw cause")
    }
    // Risk: calling Error() on the unwrapped err still leaks. Callers must not
    // re-stringify the cause; only the boundary-rendered text is safe.
    if !strings.Contains(safe.Error(), "[REDACTED]") {
        t.Fatalf("SafeError.Error must be redacted, got %q", safe.Error())
    }
}

func TestSafeLogger_RedactsFormattedSentinel(t *testing.T) {
    const sentinel = "sk-stderr-sentinel-123456"
    var out strings.Builder
    r := NewRedactor()
    r.Register(sentinel)
    log := NewSafeLogger(&out, r)
    log.Printf("device auth failed: %v\n", errors.New("provider echoed "+sentinel))
    if strings.Contains(out.String(), sentinel) {
        t.Fatalf("SafeLogger leaked sentinel: %q", out.String())
    }
    if !strings.Contains(out.String(), "[REDACTED]") {
        t.Fatalf("SafeLogger omitted redaction marker: %q", out.String())
    }
}

func TestCredentialRef_Parse(t *testing.T) {
    cases := []struct {
        in          string
        allowLegacy bool
        wantKind    string
        wantErr     bool
    }{
        {"secret://openai/main", false, "secret", false},
        {"env://OPENAI_API_KEY", false, "env", false},
        {"sk-legacy-enabled", true, "legacy", false},
        {"sk-live-raw", false, "", true},
    }
    for _, c := range cases {
        ref, err := ParseCredentialRef(c.in, c.allowLegacy)
        if c.wantErr {
            if err == nil {
                t.Fatalf("ParseCredentialRef(%q): want err, got %+v", c.in, ref)
            }
            continue
        }
        if err != nil {
            t.Fatalf("ParseCredentialRef(%q): %v", c.in, err)
        }
        if ref.Kind != c.wantKind {
            t.Fatalf("ParseCredentialRef(%q): kind=%s want %s", c.in, ref.Kind, c.wantKind)
        }
    }
}
```

`internal/config/config_test.go` — `TestConfig_NewSecretsBlock`:

```go
func TestConfig_NewSecretsBlock(t *testing.T) {
    yaml := []byte(`
secrets:
  backend: auto
  file_path: ~/.yanshi/secrets.enc
auth:
  device:
    client_id: test-id
i18n:
  ui_locale: auto
  output_language: en
tui:
  vim: true
  high_contrast: false
  bindings:
    ctrl+k: scroll_down
`)
    tmp := t.TempDir() + "/cfg.yaml"
    if err := os.WriteFile(tmp, yaml, 0600); err != nil {
        t.Fatal(err)
    }
    cfg, err := Load(tmp)
    if err != nil {
        t.Fatalf("Load: %v", err)
    }
    if cfg.Secrets.Backend != "auto" || cfg.Auth.Device.ClientID != "test-id" ||
        cfg.I18N.UILocale != "auto" || cfg.I18N.OutputLanguage != "en" ||
        cfg.TUI.Vim == nil || !*cfg.TUI.Vim ||
        cfg.TUI.HighContrast == nil || *cfg.TUI.HighContrast ||
        cfg.TUI.Bindings["ctrl+k"] != "scroll_down" {
        t.Fatalf("new fields not parsed: %+v", cfg)
    }
}

func TestConfig_D3Defaults(t *testing.T) {
    path := filepath.Join(t.TempDir(), "config.yaml")
    if err := os.WriteFile(path, []byte("{}\n"), 0600); err != nil {
        t.Fatal(err)
    }
    cfg, err := Load(path)
    if err != nil {
        t.Fatal(err)
    }
    if cfg.Secrets.Backend != "auto" || cfg.Secrets.FilePath == "" ||
        cfg.I18N.UILocale != "auto" || cfg.TUI.KeymapName != "default" ||
        cfg.TUI.Theme != "default" {
        t.Fatalf("D3 defaults not applied: %+v", cfg)
    }
}
```

### Expected (GREEN 后行为)

- `Redactor.Redact(s)` 把所有 `Register` 过的秘密子串替换成 `[REDACTED]`，覆盖 stderr / WS / SSE / SQLite 的统一调用点。
- `Redactor.SafeError(err)` 返回 `*safeError`，其 `Error()` 已脱敏但 `Unwrap()` 仍指向 raw cause；`errors.Is/As` 链不破。
- `ParseCredentialRef` 接受 `secret://`、`env://`；raw literal 仅在 `allowLegacy=true` 时产生 `Kind="legacy"`，否则返回 `ErrRawLiteralRefused`。安全 backend 是否存在由 `Manager.Resolve(Kind="secret")` 另行 fail-closed；parser 不伪装知道 runtime backend。
- `SafeLogger.Printf/Println` 先完整格式化、再通过同一个 `Redactor` 脱敏后写入显式 `io.Writer`；D3 不新增直接 `fmt.Fprint*(os.Stderr, ...)`。
- `Config` 新增 `Secrets`、`Auth`、`I18N`、`TUI` 四个块，YAML 解析无歧义。

### Implementation

`internal/secrets/secrets.go`:

```go
// Package secrets provides secure credential storage and a unified redactor
// applied at every output boundary (stderr logs, WS frames, SSE frames, SQLite
// writes). The Redactor is the single source of truth for what counts as a
// secret in yanshi's runtime: every secret string is registered once after
// config load, and every boundary calls Redact on the rendered text.
//
// Design decision: SafeError preserves the raw cause via Unwrap so
// errors.Is/As chains stay intact (orchestrator code matches sentinel errors
// by identity). The trade-off is that a caller who re-stringifies the
// unwrapped cause re-leaks the secret — therefore SafeError is only used at
// boundaries that render Error() once and forward the resulting text, never
// as a generic error wrapper inside business logic.
package secrets

import (
    "errors"
    "fmt"
    "io"
    "sort"
    "strconv"
    "strings"
    "sync"
)

// ErrRawLiteralRefused is returned by ParseCredentialRef when the input is a
// raw literal (not a secret:// / env:// / legacy-insecure ref) AND the caller
// has not opted into legacy insecure mode. This is fail-closed: a raw API key
// in config silently working would defeat S10's threat model.
var ErrRawLiteralRefused = errors.New("secrets: raw literal credential refused (use secret:// or env:// reference, or set legacy-insecure)")

// ErrKeyringUnavailable is returned by the OS keyring adapter when no platform
// backend is wired (CGO disabled, no D-Bus on Linux, etc). The Manager treats
// this as a soft-degrade trigger, not a fatal error.
var ErrKeyringUnavailable = errors.New("secrets: OS keyring unavailable")

// ErrSecretNotFound is distinct from ErrKeyringUnavailable: a missing entry
// proves the backend answered and is therefore available. Manager auto mode
// must not misclassify a normal miss as an unavailable keyring.
var ErrSecretNotFound = errors.New("secrets: secret not found")

// CredentialRef is the parsed form of a credential source string. Kind is one
// of "secret" / "env" / "legacy" / "none"; Service/Account are populated for
// Kind=="secret"; VarName is populated for Kind=="env". Raw is preserved for
// diagnostics but MUST NOT be logged through any non-redacted path.
type CredentialRef struct {
    Kind    string
    Service string
    Account string
    VarName string
    Raw     string
}

// ParseCredentialRef parses a credential reference. allowLegacy controls
// whether "legacy-insecure" and raw literals are accepted; the Manager only
// passes true when explicitly configured (legacy opt-in). The empty string
// parses to a zero-ref (treated as "no credential configured").
func ParseCredentialRef(s string, allowLegacy bool) (CredentialRef, error) {
    switch {
    case s == "":
        return CredentialRef{Kind: "none"}, nil
    case strings.HasPrefix(s, "secret://"):
        rest := strings.TrimPrefix(s, "secret://")
        parts := strings.SplitN(rest, "/", 2)
        if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
            return CredentialRef{}, fmt.Errorf("secrets: invalid secret:// ref %q (want secret://service/account)", s)
        }
        return CredentialRef{Kind: "secret", Service: parts[0], Account: parts[1], Raw: s}, nil
    case strings.HasPrefix(s, "env://"):
        v := strings.TrimPrefix(s, "env://")
        if v == "" {
            return CredentialRef{}, fmt.Errorf("secrets: invalid env:// ref %q (want env://VARNAME)", s)
        }
        return CredentialRef{Kind: "env", VarName: v, Raw: s}, nil
    default:
        // Every non-reference value is a raw literal. It is accepted only
        // when auth.legacy_insecure is explicitly true for this process.
        if !allowLegacy {
            return CredentialRef{}, ErrRawLiteralRefused
        }
        return CredentialRef{Kind: "legacy", Raw: s}, nil
    }
}

// Redactor is a concurrency-safe registry of secret substrings. Register each
// secret exactly once after resolution; every output boundary then calls
// Redact on the rendered text. Replacements are stable strings so log grep
// still groups by "which secret leaked".
type Redactor struct {
    mu      sync.RWMutex
    secrets []string
}

// NewRedactor returns an empty Redactor.
func NewRedactor() *Redactor { return &Redactor{} }

// Register adds a secret substring to the registry. Empty values are ignored.
// Re-registration is idempotent.
func (r *Redactor) Register(secret string) {
    if secret == "" {
        return
    }
    r.mu.Lock()
    defer r.mu.Unlock()
    for _, s := range r.secrets {
        if s == secret {
            return
        }
    }
    r.secrets = append(r.secrets, secret)
    sort.Slice(r.secrets, func(i, j int) bool {
        return len(r.secrets[i]) > len(r.secrets[j])
    })
}

// Redact replaces every registered secret substring in s with "[REDACTED]".
// Concurrent-safe. Returns s unchanged if no secrets registered.
func (r *Redactor) Redact(s string) string {
    r.mu.RLock()
    defer r.mu.RUnlock()
    if len(r.secrets) == 0 {
        return s
    }
    out := s
    for _, secret := range r.secrets {
        out = strings.ReplaceAll(out, secret, "[REDACTED]")
    }
    return out
}

// SafeError wraps err so that Error() is redacted but Unwrap() returns the
// raw cause (preserving errors.Is/As). See package doc for the re-stringify
// risk. SafeError(nil) returns nil.
func (r *Redactor) SafeError(err error) error {
    if err == nil {
        return nil
    }
    return &safeError{cause: err, text: r.Redact(err.Error())}
}

type safeError struct {
    cause error
    text  string
}

func (e *safeError) Error() string { return e.text }
func (e *safeError) Unwrap() error { return e.cause }

// RedactJSON redacts both plain and JSON-escaped spellings of each secret.
// This is required at WS/SSE boundaries: json.Marshal escapes quotes,
// backslashes and control characters, so replacing only the raw spelling
// would miss a key such as `abc"def` after marshal.
func (r *Redactor) RedactJSON(data []byte) []byte {
    r.mu.RLock()
    defer r.mu.RUnlock()
    out := string(data)
    for _, secret := range r.secrets {
        quoted := strconv.Quote(secret)
        escaped := quoted[1 : len(quoted)-1]
        out = strings.ReplaceAll(out, secret, "[REDACTED]")
        out = strings.ReplaceAll(out, escaped, "[REDACTED]")
    }
    return []byte(out)
}

// SafeLogger is the only stderr/log sink introduced by D3. It formats first,
// then redacts once before writing. New D3 code must not call fmt.Fprintf on
// os.Stderr directly; bootstrap constructs one process-wide SafeLogger and
// passes it to secrets/auth/device builders.
type SafeLogger struct {
    out io.Writer
    redactor *Redactor
}

func NewSafeLogger(out io.Writer, r *Redactor) *SafeLogger {
    return &SafeLogger{out: out, redactor: r}
}

func (l *SafeLogger) Printf(format string, args ...any) {
    text := fmt.Sprintf(format, args...)
    if l.redactor != nil {
        text = l.redactor.Redact(text)
    }
    _, _ = io.WriteString(l.out, text)
}

func (l *SafeLogger) Println(args ...any) {
    l.Printf("%s", fmt.Sprintln(args...))
}

// MergeRedactors returns a Redactor whose registry is the union of all
// inputs. Used by bootstrap to combine provider-level redactors into one
// process-wide Redactor handed to ws.Conn, Server, and Store.
func MergeRedactors(rs ...*Redactor) *Redactor {
    merged := NewRedactor()
    for _, r := range rs {
        if r == nil {
            continue
        }
        r.mu.RLock()
        for _, s := range r.secrets {
            merged.Register(s)
        }
        r.mu.RUnlock()
    }
    return merged
}
```

`internal/config/config.go` 增量 (在现有 `Config` struct 内追加，不动其它字段):

```go
type Config struct {
    Secrets SecretsConfig `yaml:"secrets"`
    Auth    AuthConfig    `yaml:"auth"`
    I18N    I18NConfig    `yaml:"i18n"`
    TUI     TUIConfig     `yaml:"tui"`
}

// SecretsConfig configures the credential storage backend. Backend is "auto"
// (default; prefer OS keyring, fall back to encrypted file if passphrase
// available), "keyring" (force OS keyring, fail if unavailable), "file"
// (force encrypted file), or "none" (no secret storage; secret:// refs fail
// to resolve). FilePath defaults to os.UserConfigDir()/yanshi/secrets.enc.
// PassphraseEnv is the env var name holding the master passphrase; if unset
// and backend is "auto", fileStore is skipped (text secrets still resolve).
type SecretsConfig struct {
    Backend       string `yaml:"backend"`
    FilePath      string `yaml:"file_path"`
    PassphraseEnv string `yaml:"passphrase_env"`
}

// AuthConfig configures provider-neutral authentication. Device.ClientID is
// used for RFC 8628 device authorization flows.
type AuthConfig struct {
    LegacyInsecure bool `yaml:"legacy_insecure"`
    Device struct {
        ClientID          string                 `yaml:"client_id"`
        DeviceAuthEnabled bool                   `yaml:"device_auth_enabled"`
        Providers         []DeviceProviderConfig `yaml:"providers"`
    } `yaml:"device"`
}

// DeviceProviderConfig is one RFC 8628 provider declaration. Endpoints are
// validated HTTPS-only at load/build time; loopback HTTP is accepted only for
// deterministic httptest acceptance.
type DeviceProviderConfig struct {
    ID        string   `yaml:"id"`
    ClientID  string   `yaml:"client_id"`
    DeviceURL string   `yaml:"device_url"`
    TokenURL  string   `yaml:"token_url"`
    Scopes    []string `yaml:"scopes"`
}

// I18NConfig configures localization. UILocale is one of "auto" (default;
// re-resolved at every startup from LC_ALL/LANG), "en", or "zh-Hans".
// OutputLanguage independently controls the model's response language; empty
// means "follow user input language" (no system-prompt directive).
type I18NConfig struct {
    UILocale       string `yaml:"ui_locale"`
    OutputLanguage string `yaml:"output_language"`
}

// TUIConfig configures TUI preferences that can also be set via /keymap,
// /vim, /contrast at runtime. Vim is *bool so we can distinguish unset
// (follow preferences.json) from explicitly disabled (force off).
type TUIConfig struct {
    Vim          *bool             `yaml:"vim"`
    KeymapName   string            `yaml:"keymap"`
    Bindings     map[string]string `yaml:"bindings"`
    Theme        string            `yaml:"theme"`
    HighContrast *bool             `yaml:"high_contrast"`
}
```

在现有 `(*Config).applyDefaults()` 末尾追加（`config.go` import 增加 `path/filepath`）：

```go
if c.Secrets.Backend == "" {
    c.Secrets.Backend = "auto"
}
if c.Secrets.FilePath == "" {
    configDir, err := os.UserConfigDir()
    if err != nil {
        configDir = "."
    }
    c.Secrets.FilePath = filepath.Join(configDir, "yanshi", "secrets.enc")
}
if c.I18N.UILocale == "" {
    c.I18N.UILocale = "auto"
}
if c.TUI.KeymapName == "" {
    c.TUI.KeymapName = "default"
}
if c.TUI.Theme == "" {
    c.TUI.Theme = "default"
}
```

`config.example.yaml` 追加的可复制示例（`bindings` 的方向必须是 `key: action`，与 `keymap.NewDefaultBuilder` 一致）：

```yaml
secrets:
  backend: auto
  file_path: "" # empty => os.UserConfigDir()/yanshi/secrets.enc
  passphrase_env: YANSHI_PASSPHRASE

auth:
  legacy_insecure: false
  device:
    device_auth_enabled: false
    providers: []

i18n:
  ui_locale: auto
  output_language: ""

tui:
  keymap: default
  theme: default
  vim: false
  high_contrast: false
  bindings:
    enter: send
    ctrl+enter: newline
```

`internal/config/config_test.go` 的 import delta 明确加入 `path/filepath`；上面的 `TestConfig_NewSecretsBlock` 与 `TestConfig_D3Defaults` 共用现有 `os` import。

### Pass

```sh
go test ./internal/secrets -run "TestRedactor|TestSafeError|TestCredentialRef"
go test ./internal/config -run "TestConfig_(NewSecretsBlock|D3Defaults)"
```

### Commit

```
feat(secrets): Redactor + CredentialRef + config schema (S10-L1)

Adds internal/secrets leaf package with the fail-closed raw-literal refusal,
safeError that preserves errors.Is via Unwrap (with documented re-stringify
risk), and the SecretsConfig/AuthConfig/I18NConfig/TUIConfig blocks. The
Redactor is the single sink every output boundary (WS/SSE/SQLite) will call
in subsequent tasks.
```

---

## Task 2 — S10-L2: keyring adapter + 加密 fileStore (Argon2id)

**结构性修复落点:** #5 (合并前锁定 keyring 版本 + `CGO_ENABLED=0 go build ./...` 验证 + build-tag fallback)、#6 (Argon2id 替代 PBKDF2 + 文件格式含 KDF 名称/参数/升级兼容)。

### 决策门 (锁定在动手前)

1. **依赖选择:** `github.com/zalando/go-keyring` (纯 Go，无 CGO；通过 `keyring.Get/Set/Delete` 调用 platform-specific backend：Windows 凭据管理器 / macOS Keychain / Linux Secret Service)。这是唯一同时满足 "无 CGO" 与 "跨平台" 的稳定选择。**在引入前先跑** `go get github.com/zalando/go-keyring@v0.2.6 && CGO_ENABLED=0 go build ./...` 验证编译通过；如失败再跑 `go build -tags nokeyring ./internal/secrets`，两种 build 之一必须成功。
2. **Build-tag 策略:** 默认编译路径（cgo 与非 cgo 都包括）走 `keyring_enabled.go` 直接调用 zalando 纯 Go 库；`-tags nokeyring` 的发布构建走 `keyring_disabled.go` 的 stub-only fallback（`Available()` 与所有 Get/Set/Delete 返回 `ErrKeyringUnavailable`）。cgo 与 `nokeyring` 是正交维度：`zalando/go-keyring` 是纯 Go，cgo 开关不影响 keyring 可用性。关键修复：**`Available()` 与 `Get("__yanshi_probe__", …)` 区分"无 backend"与"无条目"** —— missing entry 返回 `ErrSecretNotFound`，missing backend 返回 `ErrKeyringUnavailable`，auto 模式据此决定是否软降级到 fileStore。
3. **版本锁定:** 在 `go.mod` 用 `go get github.com/zalando/go-keyring@v0.2.6`；记录到本计划的 Open Decisions。**不**使用 "latest" / 未固定版本。
4. **fileStore 格式 (v1):**
   ```
   magic[4]    = "YSCE" (yanshi secrets)
   version[1]  = 0x01
   kdfName[16] = "argon2id         " (padding to 16)
   salt[32]    = random per file
   kdfParams[] = JSON {time,memory,threads} (length-prefixed u16)
   nonce[12]   = AES-GCM nonce
   ciphertext  = AES-256-GCM(provider_data_json)
   ```
   `provider_data_json` = `{"entries":[{"service":"...","account":"...","secret":"..."}]}`。未来切换 KDF 只需新增 `kdfName` 与 `version=0x02`，老文件靠 magic+version 兼容读。

### Files

- Create `internal/secrets/file_store.go`
- Create `internal/secrets/file_store_replace_unix.go`
- Create `internal/secrets/file_store_replace_windows.go`
- Create `internal/secrets/file_store_test.go`
- Create `internal/secrets/keyring.go`
- Create `internal/secrets/keyring_enabled.go` (`//go:build !nokeyring`)
- Create `internal/secrets/keyring_disabled.go` (`//go:build nokeyring`)
- Create `internal/secrets/keyring_test.go`
- Modify `go.mod` (`go get github.com/zalando/go-keyring@v0.2.6`)
- Modify `go.sum` (由上述固定版本的 `go get` 生成校验和)

### Failure Test (RED)

`internal/secrets/file_store_test.go`:

```go
package secrets

import (
    "bytes"
    "encoding/binary"
    "encoding/json"
    "errors"
    "fmt"
    "os"
    "path/filepath"
    "strings"
    "sync"
    "testing"
)

func TestFileStore_RoundTrip(t *testing.T) {
    path := filepath.Join(t.TempDir(), "secrets.enc")
    fs, err := NewFileStore(path, []byte("correct-passphrase"))
    if err != nil {
        t.Fatalf("NewFileStore: %v", err)
    }
    if err := fs.Set("openai", "main", "sk-test-12345"); err != nil {
        t.Fatalf("Set: %v", err)
    }
    got, err := fs.Get("openai", "main")
    if err != nil {
        t.Fatalf("Get: %v", err)
    }
    if got != "sk-test-12345" {
        t.Fatalf("round-trip mismatch: %q", got)
    }
}

func TestFileStore_WrongPassphraseFails(t *testing.T) {
    path := filepath.Join(t.TempDir(), "secrets.enc")
    fs1, _ := NewFileStore(path, []byte("correct"))
    _ = fs1.Set("svc", "acct", "v")
    // Wrong passphrase must fail to read existing file (GCM auth tag mismatch).
    _, err := NewFileStore(path, []byte("wrong"))
    if err == nil {
        t.Fatal("expected error on wrong passphrase")
    }
}

func TestFileStore_KDFHeaderIsArgon2id(t *testing.T) {
    path := filepath.Join(t.TempDir(), "secrets.enc")
    fs, _ := NewFileStore(path, []byte("p"))
    _ = fs.Set("svc", "acct", "v")
    data, _ := os.ReadFile(path)
    if string(data[:4]) != "YSCE" {
        t.Fatalf("magic missing: %q", data[:4])
    }
    if data[4] != 0x01 {
        t.Fatalf("version: %d", data[4])
    }
    kdf := strings.TrimRight(string(data[5:21]), " ")
    if kdf != "argon2id" {
        t.Fatalf("kdfName: %q want argon2id", kdf)
    }
}

func maliciousKDFEnvelope(t *testing.T, params any) []byte {
    t.Helper()
    paramsJSON, err := json.Marshal(params)
    if err != nil {
        t.Fatal(err)
    }
    data := append([]byte(nil), []byte("YSCE")...)
    data = append(data, 0x01)
    data = append(data, []byte("argon2id        ")...) // exactly 16 bytes
    data = append(data, make([]byte, 32)...)           // salt
    plen := make([]byte, 2)
    binary.BigEndian.PutUint16(plen, uint16(len(paramsJSON)))
    data = append(data, plen...)
    data = append(data, paramsJSON...)
    data = append(data, make([]byte, 12+16)...) // nonce + dummy GCM tag
    return data
}

// Untrusted header values are validated before Argon2id. Without this gate a
// tiny malicious file can request unbounded memory/CPU or pass zero threads to
// argon2.IDKey. Each case must return an error without reaching the KDF.
func TestFileStore_RejectsUnsafeKDFParameters(t *testing.T) {
    tests := []struct {
        name    string
        time    uint32
        memory  uint32
        threads uint8
    }{
        {"zero-time", 0, 64 * 1024, 2},
        {"excessive-time", 11, 64 * 1024, 2},
        {"low-memory", 1, 8*1024 - 1, 2},
        {"excessive-memory", 1, 256*1024 + 1, 2},
        {"zero-threads", 1, 64 * 1024, 0},
        {"excessive-threads", 1, 64 * 1024, 17},
    }
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            path := filepath.Join(t.TempDir(), "secrets.enc")
            data := maliciousKDFEnvelope(t, struct {
                Time    uint32 `json:"time"`
                Memory  uint32 `json:"memory"`
                Threads uint8  `json:"threads"`
            }{tt.time, tt.memory, tt.threads})
            if err := os.WriteFile(path, data, 0600); err != nil {
                t.Fatal(err)
            }
            _, err := NewFileStore(path, []byte("p"))
            if err == nil || !strings.Contains(err.Error(), "unsafe kdf params") {
                t.Fatalf("want bounded-kdf rejection, got %v", err)
            }
        })
    }
}

func TestFileStore_RejectsOversizedKDFParameterJSON(t *testing.T) {
    path := filepath.Join(t.TempDir(), "secrets.enc")
    data := append([]byte(nil), []byte("YSCE")...)
    data = append(data, 0x01)
    data = append(data, []byte("argon2id        ")...)
    data = append(data, make([]byte, 32)...)
    plen := make([]byte, 2)
    binary.BigEndian.PutUint16(plen, 1025)
    data = append(data, plen...)
    data = append(data, make([]byte, 1025+12+16)...)
    if err := os.WriteFile(path, data, 0600); err != nil {
        t.Fatal(err)
    }
    _, err := NewFileStore(path, []byte("p"))
    if err == nil || !strings.Contains(err.Error(), "kdf params length") {
        t.Fatalf("want bounded-header rejection, got %v", err)
    }
}

func TestFileStore_CorruptFileFails(t *testing.T) {
    path := filepath.Join(t.TempDir(), "secrets.enc")
    _ = os.WriteFile(path, []byte("YSCE\x01garbage"), 0600)
    _, err := NewFileStore(path, []byte("p"))
    if err == nil {
        t.Fatal("expected error on corrupt file")
    }
}

// TestFileStore_PreservesJSONStructure validates that the ciphertext payload
// is a stable JSON shape (forward-compat: future KDFs still read v1 entries).
func TestFileStore_PreservesJSONStructure(t *testing.T) {
    path := filepath.Join(t.TempDir(), "secrets.enc")
    fs, _ := NewFileStore(path, []byte("p"))
    _ = fs.Set("openai", "main", "v1")
    // Inspect via internal accessor (see implementation).
    entries, _ := fs.debugEntries()
    if len(entries) != 1 || entries[0].Service != "openai" {
        var b []byte
        b, _ = json.Marshal(entries)
        t.Fatalf("unexpected entries: %s", b)
    }
}

// TestFileStore_ReplaceFailureRollsBackDiskAndMemory injects failure at the
// exact atomic-replace seam. It does not delete/restore the target itself: the
// failed Set must preserve both the prior encrypted bytes and fs.entries.
func TestFileStore_ReplaceFailureRollsBackDiskAndMemory(t *testing.T) {
    path := filepath.Join(t.TempDir(), "secrets.enc")
    fs, err := NewFileStore(path, []byte("pass"))
    if err != nil {
        t.Fatal(err)
    }
    if err := fs.Set("openai", "main", "old-secret"); err != nil {
        t.Fatal(err)
    }
    oldBytes, err := os.ReadFile(path)
    if err != nil {
        t.Fatal(err)
    }

    oldReplace := replaceEncryptedFile
    replaceEncryptedFile = func(src, dst string) error {
        if dst != path || !strings.HasPrefix(src, path+".tmp.") {
            t.Fatalf("unexpected replace %q -> %q", src, dst)
        }
        return errors.New("injected encrypted-file replace failure")
    }
    t.Cleanup(func() { replaceEncryptedFile = oldReplace })

    err = fs.Set("openai", "main", "new-secret")
    if err == nil || !strings.Contains(err.Error(), "injected encrypted-file replace failure") {
        t.Fatalf("expected injected replace failure, got %v", err)
    }
    got, err := fs.Get("openai", "main")
    if err != nil || got != "old-secret" {
        t.Fatalf("in-memory rollback failed: got=%q err=%v", got, err)
    }
    after, err := os.ReadFile(path)
    if err != nil {
        t.Fatal(err)
    }
    if !bytes.Equal(after, oldBytes) {
        t.Fatal("encrypted target changed after failed replace")
    }
    matches, err := filepath.Glob(path + ".tmp.*")
    if err != nil {
        t.Fatal(err)
    }
    if len(matches) != 0 {
        t.Fatalf("new temp file not cleaned: %v", matches)
    }
}

func TestFileStore_ConcurrentAccessIsSerialized(t *testing.T) {
    path := filepath.Join(t.TempDir(), "secrets.enc")
    fs, err := NewFileStore(path, []byte("pass"))
    if err != nil {
        t.Fatal(err)
    }
    const workers = 8
    errs := make(chan error, workers)
    var wg sync.WaitGroup
    for i := 0; i < workers; i++ {
        i := i
        wg.Add(1)
        go func() {
            defer wg.Done()
            account := fmt.Sprintf("account-%d", i)
            value := fmt.Sprintf("secret-%d", i)
            if err := fs.Set("service", account, value); err != nil {
                errs <- err
                return
            }
            got, err := fs.Get("service", account)
            if err != nil {
                errs <- err
                return
            }
            if got != value {
                errs <- fmt.Errorf("%s: got %q want %q", account, got, value)
            }
        }()
    }
    wg.Wait()
    close(errs)
    for err := range errs {
        t.Error(err)
    }
}
```

`internal/secrets/keyring_test.go`:

```go
package secrets

import (
    "errors"
    "testing"
)

// TestKeyring_FailsGracefullyWhenUnavailable exercises the soft-degrade path:
// when the OS keyring is not present (CI without D-Bus, `-tags nokeyring`
// build), the adapter's Available probe AND its Get must return
// ErrKeyringUnavailable (not a panic, not a generic error, and NOT
// ErrSecretNotFound — a missing entry is a different condition that the
// Manager must be able to distinguish from a missing backend).
func TestKeyring_FailsGracefullyWhenUnavailable(t *testing.T) {
    s := NewOSKeyringStore()
    // Available() probe: distinguishes "no backend" from "missing entry".
    if err := s.Available(); err == nil {
        t.Skip("OS keyring IS available on this host; soft-degrade path skipped")
    } else if !errors.Is(err, ErrKeyringUnavailable) {
        // Any other error means the keyring backend answered (so it exists
        // but is locked / errored). That's a different test path; skip here.
        t.Skipf("keyring returned non-unavailable error; backend is present: %v", err)
    }
    // Backend unavailable: Get must also report unavailable, NOT not-found.
    if _, err := s.Get("any", "any"); !errors.Is(err, ErrKeyringUnavailable) {
        t.Fatalf("Get on unavailable backend: want ErrKeyringUnavailable, got %v", err)
    }
}

// TestKeyring_MissingEntryReturnsSecretNotFound verifies the OTHER direction
// of the availability contract: when the backend is reachable but the entry
// is absent, Get must return ErrSecretNotFound (not ErrKeyringUnavailable).
// This keeps Manager.auto from mis-classifying a cold-cache lookup as a
// backend failure and falling back to fileStore when it should not.
func TestKeyring_MissingEntryReturnsSecretNotFound(t *testing.T) {
    s := NewOSKeyringStore()
    if err := s.Available(); err != nil {
        t.Skipf("keyring backend unavailable on this host; skipping: %v", err)
    }
    if _, err := s.Get("__definitely_not_stored__", "__test__"); err != ErrSecretNotFound {
        t.Fatalf("missing entry: want ErrSecretNotFound, got %v", err)
    }
}
```

### Expected

- `NewFileStore(path, passphrase)` 解析 magic/version/kdfName/salt/params；若文件不存在则创建空 store，写入时序列化全部 entries。
- Argon2id 写入参数固定为 `time=1, memory=64*1024 KiB, threads=2`；读取不可信 header 时先强制 `time=1..10`、`memory=8*1024..256*1024 KiB`、`threads=1..16` 且参数 JSON 不超过 1024 bytes，再调用 `argon2.IDKey`，恶意 header 不得触发无界 CPU/内存或 panic。
- magic/version/kdfName/salt/params/nonce 组成 GCM AAD；错误 passphrase 或 header/ciphertext 篡改均在 GCM auth 或更早的格式/边界校验处返回 error。
- `NewOSKeyringStore()` 在 `-tags nokeyring` 时返回 stub；默认 build 在 platform backend 缺失时让 `Available/Get/Set/Delete` 以 `errors.Is(err, ErrKeyringUnavailable)` 可识别地失败。CGO 开关本身不选择实现。
- `FileStore.Set/Delete` 以 copy-on-write 更新 `entries`；只有 OS-specific atomic replace 成功后才提交内存状态。replace 失败时旧磁盘 bytes 和旧内存值都不变，新 temp 被清理。
- 单个 `FileStore` 实例以 `sync.RWMutex` 串行化写入并允许并发读取；`go test -race` 覆盖该进程内合同。D3 不提供多进程 writer coordination：应用的单后端 owner 是唯一 writer；Unix `rename` / Windows `MoveFileEx` 保证 replace-existing 原子可见性，但本任务不宣称 Unix 目录项已 `fsync` 因而具备断电级 durability。

### Implementation

`internal/secrets/keyring.go`:

```go
// Package secrets — OS keyring adapter. The implementation is split by build
// tag so a `-tags nokeyring` build (used for static release binaries and CI
// without a keyring daemon) gets the stub-only fallback that always returns
// ErrKeyringUnavailable, while the default build gets the real platform
// keyring call. cgo / non-cgo is orthogonal: zalando/go-keyring is pure Go.

package secrets

// Store is the credential storage adapter. Implementations:
//   - osKeyringStore: backed by OS keyring (zalando/go-keyring)
//   - fileStore: AES-256-GCM encrypted local file (Argon2id KDF)
//   - noKeyringStore: stub returned when -tags nokeyring is active
type Store interface {
    Available() error
    Get(service, account string) (string, error)
    Set(service, account, secret string) error
    Delete(service, account string) error
}

// Available reports whether this concrete backend can be used. It is NOT the
// same as "an entry exists": a missing entry returns ErrSecretNotFound from
// Get, while a missing *backend* (no D-Bus, keyring locked, no session, ...)
// returns ErrKeyringUnavailable. FileStore always returns nil once opened.
// Manager uses this explicit port instead of inferring backend health from a
// sentinel Get result.

// NewOSKeyringStore returns the OS keyring Store. The default build returns a
// real keyring-backed store; the nokeyring build returns a stub. This
// indirection keeps the public API stable across build configurations.
func NewOSKeyringStore() Store {
    return newOSKeyringStoreImpl()
}
```

`internal/secrets/keyring_enabled.go`:

```go
//go:build !nokeyring

package secrets

import (
    "errors"
    "fmt"

    "github.com/zalando/go-keyring"
)

type osKeyringStore struct{}

func newOSKeyringStoreImpl() Store { return &osKeyringStore{} }

// Available reports whether the OS keyring is usable on this host. zalando/
// go-keyring does not expose a probe directly, so we issue a Get against a
// sentinel (service, account) pair: ErrNotFound means the backend works (it
// answered with a real "not found" response), any other non-nil error means
// the backend is unavailable (no D-Bus, locked keyring, platform error).
func (osKeyringStore) Available() error {
    if _, err := keyring.Get("__yanshi_probe__", "__yanshi_probe__"); err == nil {
        return nil
    } else if errors.Is(err, keyring.ErrNotFound) {
        return nil
    } else {
        return fmt.Errorf("%w: %v", ErrKeyringUnavailable, err)
    }
}

func (osKeyringStore) Get(service, account string) (string, error) {
    s, err := keyring.Get(service, account)
    if err != nil {
        if errors.Is(err, keyring.ErrNotFound) {
            return "", ErrSecretNotFound
        }
        return "", fmt.Errorf("%w: get failed: %v", ErrKeyringUnavailable, err)
    }
    return s, nil
}

func (osKeyringStore) Set(service, account, secret string) error {
    if err := keyring.Set(service, account, secret); err != nil {
        return fmt.Errorf("%w: set failed: %v", ErrKeyringUnavailable, err)
    }
    return nil
}

func (osKeyringStore) Delete(service, account string) error {
    err := keyring.Delete(service, account)
    switch {
    case err == nil:
        return nil
    case errors.Is(err, keyring.ErrNotFound):
        return ErrSecretNotFound
    default:
        return fmt.Errorf("%w: delete failed: %v", ErrKeyringUnavailable, err)
    }
}
```

`internal/secrets/keyring_disabled.go`:

```go
//go:build nokeyring

package secrets

// noKeyringStore is the soft-degrade stub for `-tags nokeyring` builds. Every
// call returns ErrKeyringUnavailable; the Manager then falls back to fileStore
// if a passphrase is configured, or refuses secret:// refs (text values still
// resolve). This MUST NOT panic or return a different error type.
type noKeyringStore struct{}

func newOSKeyringStoreImpl() Store { return noKeyringStore{} }

func (noKeyringStore) Available() error                   { return ErrKeyringUnavailable }
func (noKeyringStore) Get(string, string) (string, error) { return "", ErrKeyringUnavailable }
func (noKeyringStore) Set(string, string, string) error   { return ErrKeyringUnavailable }
func (noKeyringStore) Delete(string, string) error        { return ErrKeyringUnavailable }
```

`internal/secrets/file_store.go`:

```go
package secrets

import (
    "crypto/aes"
    "crypto/cipher"
    "crypto/rand"
    "encoding/binary"
    "encoding/json"
    "errors"
    "fmt"
    "io"
    "os"
    "path/filepath"
    "strings"
    "sync"

    "golang.org/x/crypto/argon2"
)

const (
    fileMagic      = "YSCE"
    fileVersion    = 0x01
    kdfNameArgon2  = "argon2id"
    saltLen        = 32
    nonceLen       = 12
    kdfTime        = 1
    kdfMemory      = 64 * 1024
    kdfThreads     = 2
    keyLen         = 32 // AES-256

    minKDFTime       = 1
    maxKDFTime       = 10
    minKDFMemoryKiB  = 8 * 1024
    maxKDFMemoryKiB  = 256 * 1024
    minKDFThreads    = 1
    maxKDFThreads    = 16
    maxKDFParamsJSON = 1024
)

type kdfParams struct {
    Time    uint32 `json:"time"`
    Memory  uint32 `json:"memory"`
    Threads uint8  `json:"threads"`
}

type fileEntry struct {
    Service string `json:"service"`
    Account string `json:"account"`
    Secret  string `json:"secret"`
}

type filePayload struct {
    Entries []fileEntry `json:"entries"`
}

// FileStore implements Store backed by an AES-256-GCM encrypted file. The KDF
// is Argon2id; parameters and salt are stored in the file header so future
// upgrades can read legacy files without re-encrypting.
type FileStore struct {
    mu         sync.RWMutex
    path       string
    passphrase []byte
    entries    []fileEntry
}

// replaceEncryptedFile is the injectable seam around the OS-specific atomic
// replacement. Tests restore it with t.Cleanup.
var replaceEncryptedFile = replaceEncryptedFileOS

// NewFileStore opens or creates the encrypted file. If the file does not
// exist, the store starts empty and persists on the first Set. If it exists,
// it must have the YSCE magic and version 0x01 with kdfName "argon2id".
func NewFileStore(path string, passphrase []byte) (*FileStore, error) {
    if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
        return nil, fmt.Errorf("secrets: mkdir: %w", err)
    }
    fs := &FileStore{path: path, passphrase: passphrase}
    data, err := os.ReadFile(path)
    if err != nil {
        if os.IsNotExist(err) {
            return fs, nil // empty store; persisted on first Set
        }
        return nil, fmt.Errorf("secrets: read file: %w", err)
    }
    if err := fs.load(data); err != nil {
        return nil, fmt.Errorf("secrets: load: %w", err)
    }
    return fs, nil
}

func (fs *FileStore) load(data []byte) error {
    if len(data) < 4+1+16+saltLen {
        return errors.New("secrets: file too short")
    }
    if string(data[:4]) != fileMagic {
        return fmt.Errorf("secrets: bad magic %q", data[:4])
    }
    if data[4] != fileVersion {
        return fmt.Errorf("secrets: unsupported version %d", data[4])
    }
    kdfName := trimPad(string(data[5:21]))
    if kdfName != kdfNameArgon2 {
        return fmt.Errorf("secrets: unsupported kdfName %q", kdfName)
    }
    off := 21
    salt := data[off : off+saltLen]
    off += saltLen
    if off+2 > len(data) {
        return errors.New("secrets: truncated kdf params")
    }
    paramsLen := int(binary.BigEndian.Uint16(data[off : off+2]))
    off += 2
    if paramsLen == 0 || paramsLen > maxKDFParamsJSON {
        return fmt.Errorf("secrets: kdf params length %d outside 1..%d",
            paramsLen, maxKDFParamsJSON)
    }
    if off+paramsLen > len(data) {
        return errors.New("secrets: truncated kdf params body")
    }
    var params kdfParams
    if err := json.Unmarshal(data[off:off+paramsLen], &params); err != nil {
        return fmt.Errorf("secrets: kdf params json: %w", err)
    }
    if err := validateKDFParams(params); err != nil {
        return err
    }
    off += paramsLen
    if off+nonceLen > len(data) {
        return errors.New("secrets: truncated nonce")
    }
    nonce := data[off : off+nonceLen]
    off += nonceLen
    aad := data[:off] // authenticate every header byte, including the nonce
    ciphertext := data[off:]
    key := argon2.IDKey(fs.passphrase, salt, params.Time, params.Memory, params.Threads, keyLen)
    block, err := aes.NewCipher(key)
    if err != nil {
        return fmt.Errorf("secrets: aes new: %w", err)
    }
    gcm, err := cipher.NewGCM(block)
    if err != nil {
        return fmt.Errorf("secrets: gcm new: %w", err)
    }
    plaintext, err := gcm.Open(nil, nonce, ciphertext, aad)
    if err != nil {
        return errors.New("secrets: wrong passphrase or corrupt ciphertext (GCM auth failed)")
    }
    var payload filePayload
    if err := json.Unmarshal(plaintext, &payload); err != nil {
        return fmt.Errorf("secrets: payload json: %w", err)
    }
    fs.entries = payload.Entries
    return nil
}

func validateKDFParams(p kdfParams) error {
    if p.Time < minKDFTime || p.Time > maxKDFTime ||
        p.Memory < minKDFMemoryKiB || p.Memory > maxKDFMemoryKiB ||
        p.Threads < minKDFThreads || p.Threads > maxKDFThreads {
        return fmt.Errorf(
            "secrets: unsafe kdf params: time=%d memory=%d threads=%d",
            p.Time, p.Memory, p.Threads,
        )
    }
    return nil
}

func (fs *FileStore) persist(entries []fileEntry) error {
    plaintext, err := json.Marshal(filePayload{Entries: entries})
    if err != nil {
        return fmt.Errorf("secrets: marshal: %w", err)
    }
    salt := make([]byte, saltLen)
    if _, err := io.ReadFull(rand.Reader, salt); err != nil {
        return fmt.Errorf("secrets: salt: %w", err)
    }
    key := argon2.IDKey(fs.passphrase, salt, kdfTime, kdfMemory, kdfThreads, keyLen)
    block, err := aes.NewCipher(key)
    if err != nil {
        return fmt.Errorf("secrets: aes new: %w", err)
    }
    gcm, err := cipher.NewGCM(block)
    if err != nil {
        return fmt.Errorf("secrets: gcm new: %w", err)
    }
    nonce := make([]byte, nonceLen)
    if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
        return fmt.Errorf("secrets: nonce: %w", err)
    }
    paramsJSON, err := json.Marshal(kdfParams{
        Time: kdfTime, Memory: kdfMemory, Threads: kdfThreads,
    })
    if err != nil {
        return fmt.Errorf("secrets: marshal kdf params: %w", err)
    }
    if len(paramsJSON) > maxKDFParamsJSON {
        return fmt.Errorf("secrets: kdf params length %d exceeds %d",
            len(paramsJSON), maxKDFParamsJSON)
    }

    var aad []byte
    aad = append(aad, []byte(fileMagic)...)
    aad = append(aad, fileVersion)
    aad = append(aad, padKdf(kdfNameArgon2)...)
    aad = append(aad, salt...)
    plen := make([]byte, 2)
    binary.BigEndian.PutUint16(plen, uint16(len(paramsJSON)))
    aad = append(aad, plen...)
    aad = append(aad, paramsJSON...)
    aad = append(aad, nonce...)
    ciphertext := gcm.Seal(nil, nonce, plaintext, aad)
    buf := append(append([]byte(nil), aad...), ciphertext...)

    suffix, err := randomHex(8)
    if err != nil {
        return err
    }
    tmp := fs.path + ".tmp." + suffix
    if err := os.WriteFile(tmp, buf, 0600); err != nil {
        return fmt.Errorf("secrets: write tmp: %w", err)
    }
    if err := replaceEncryptedFile(tmp, fs.path); err != nil {
        _ = os.Remove(tmp)
        return fmt.Errorf("secrets: replace encrypted file: %w", err)
    }
    return nil
}

func (fs *FileStore) Available() error { return nil }

func (fs *FileStore) Get(service, account string) (string, error) {
    fs.mu.RLock()
    defer fs.mu.RUnlock()
    for _, e := range fs.entries {
        if e.Service == service && e.Account == account {
            return e.Secret, nil
        }
    }
    return "", ErrSecretNotFound
}

func (fs *FileStore) Set(service, account, secret string) error {
    fs.mu.Lock()
    defer fs.mu.Unlock()
    next := append([]fileEntry(nil), fs.entries...)
    found := false
    for i, e := range next {
        if e.Service == service && e.Account == account {
            next[i].Secret = secret
            found = true
            break
        }
    }
    if !found {
        next = append(next, fileEntry{Service: service, Account: account, Secret: secret})
    }
    if err := fs.persist(next); err != nil {
        return err
    }
    fs.entries = next
    return nil
}

func (fs *FileStore) Delete(service, account string) error {
    fs.mu.Lock()
    defer fs.mu.Unlock()
    next := append([]fileEntry(nil), fs.entries...)
    for i, e := range next {
        if e.Service == service && e.Account == account {
            next = append(next[:i], next[i+1:]...)
            if err := fs.persist(next); err != nil {
                return err
            }
            fs.entries = next
            return nil
        }
    }
    return ErrSecretNotFound
}

// Enumerate returns the distinct accounts stored under service. Used by
// auth.Manager.ListAccounts so CLI `yanshi auth status` can list every
// account for a provider without scanning arbitrary unrelated services.
func (fs *FileStore) Enumerate(service string) ([]string, error) {
    fs.mu.RLock()
    defer fs.mu.RUnlock()
    seen := map[string]bool{}
    var out []string
    for _, e := range fs.entries {
        if e.Service == service && !seen[e.Account] {
            seen[e.Account] = true
            out = append(out, e.Account)
        }
    }
    return out, nil
}

// debugEntries is test-only (not exported beyond the package) and lets the
// JSON-shape test inspect the in-memory payload without re-decrypting.
func (fs *FileStore) debugEntries() ([]fileEntry, error) {
    fs.mu.RLock()
    defer fs.mu.RUnlock()
    return append([]fileEntry(nil), fs.entries...), nil
}

func padKdf(name string) string {
    if len(name) >= 16 {
        return name[:16]
    }
    return name + strings.Repeat(" ", 16-len(name))
}
func trimPad(s string) string { return strings.TrimRight(s, " ") }

func randomHex(n int) (string, error) {
    b := make([]byte, n)
    if _, err := io.ReadFull(rand.Reader, b); err != nil {
        return "", fmt.Errorf("secrets: generate temp suffix: %w", err)
    }
    return fmt.Sprintf("%x", b), nil
}
```

`internal/secrets/file_store_replace_unix.go`:

```go
//go:build !windows

package secrets

import "os"

func replaceEncryptedFileOS(src, dst string) error { return os.Rename(src, dst) }
```

`internal/secrets/file_store_replace_windows.go`:

```go
//go:build windows

package secrets

import (
    "errors"
    "os"

    "golang.org/x/sys/windows"
)

// MoveFileEx(REPLACE_EXISTING|WRITE_THROUGH) supplies the replace-existing
// semantics that os.Rename does not guarantee on Windows.
func replaceEncryptedFileOS(src, dst string) error {
    srcp, err := windows.UTF16PtrFromString(src)
    if err != nil {
        return err
    }
    dstp, err := windows.UTF16PtrFromString(dst)
    if err != nil {
        return err
    }
    err = windows.MoveFileEx(srcp, dstp,
        windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
    if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
        return os.ErrNotExist
    }
    return err
}
```

### Pass

```sh
go get github.com/zalando/go-keyring@v0.2.6
CGO_ENABLED=0 go build ./...
go test -race ./internal/secrets -run "TestFileStore"
go test ./internal/secrets -run "TestKeyring"
go test -tags nokeyring ./internal/secrets -run "TestKeyring_FailsGracefullyWhenUnavailable"
go build -tags nokeyring ./internal/secrets
```

### Commit

```
feat(secrets): Argon2id fileStore + OS keyring adapter (S10-L2)

FileStore uses AES-256-GCM with Argon2id (time=1, memory=64MiB, threads=2),
carries kdfName+params in the header, and copy-on-write + OS-specific atomic
replace preserves disk and memory state on failure. The default keyring build
uses zalando/go-keyring in both CGO and non-CGO modes; `-tags nokeyring` selects
the explicit unavailable stub. `CGO_ENABLED=0` and `nokeyring` builds are both
gated before commit.
```

---

## Task 3 — S10-L3: secrets CLI (readSecret + authCommand)

**结构性修复落点:** #13 (CLI auth 完整 — readSecret/authCommand/TestMainAuth 完整代码，--api-key-stdin 真实解析，唯一测试 provider/account，cleanup 失败路径也执行)。

### Files

- Create `internal/secrets/manager.go`
- Create `internal/secrets/manager_test.go`
- Create `internal/secrets/cli.go`
- Create `internal/secrets/cli_test.go`

### Failure Test (RED)

`internal/secrets/manager_test.go`:

```go
package secrets

import (
    "path/filepath"
    "testing"
)

func TestManager_ForcedFileRoundTrip(t *testing.T) {
    // Force FileStore by pointing backend at "file" with a passphrase.
    t.Setenv("YANSHI_TEST_PASS", "test-passphrase")
    path := filepath.Join(t.TempDir(), "secrets.enc")
    mgr, err := NewManager(Config{
        Backend:       "file",
        FilePath:      path,
        PassphraseEnv: "YANSHI_TEST_PASS",
    })
    if err != nil {
        t.Fatalf("NewManager: %v", err)
    }
    defer mgr.Close()

    if err := mgr.Set("openai", "main", "sk-from-file"); err != nil {
        t.Fatalf("Set: %v", err)
    }
    got, err := mgr.Store().Get("openai", "main")
    if err != nil {
        t.Fatalf("Get: %v", err)
    }
    if got != "sk-from-file" {
        t.Fatalf("got %q", got)
    }
}

func TestManager_NoBackendRefusesSecretRef(t *testing.T) {
    mgr, err := NewManager(Config{Backend: "none"})
    if err != nil {
        t.Fatalf("NewManager: %v", err)
    }
    defer mgr.Close()
    _, err = mgr.Resolve(CredentialRef{Kind: "secret", Service: "x", Account: "y"})
    if err == nil {
        t.Fatal("expected error resolving secret:// with no backend")
    }
}

func TestManager_LegacyLiteralPassesThroughWhenAllowed(t *testing.T) {
    mgr, _ := NewManager(Config{Backend: "none"})
    defer mgr.Close()
    got, err := mgr.Resolve(CredentialRef{Kind: "legacy", Raw: "sk-legacy"})
    if err != nil {
        t.Fatalf("Resolve: %v", err)
    }
    if got != "sk-legacy" {
        t.Fatalf("got %q", got)
    }
}

func TestManager_AutoWithoutPassphraseSkipsFileStore(t *testing.T) {
    // Backend=auto but no passphrase set: must not fatal, only warn.
    // t.Setenv is the only safe test env mutation on Windows; use a unique
    // name we know is not exported by the test runner. Do not call os.Unsetenv.
    path := filepath.Join(t.TempDir(), "secrets.enc")
    mgr, err := NewManager(Config{
        Backend:       "auto",
        FilePath:      path,
        PassphraseEnv: "YANSHI_TEST_PASS_AUTO_ABSENT",
    })
    if err != nil {
        t.Fatalf("auto mode must not fatal without passphrase: %v", err)
    }
    defer mgr.Close()
    // secret:// must fail (no keyring, no fileStore); env:// and legacy must still work.
    if _, err := mgr.Resolve(CredentialRef{Kind: "secret", Service: "x", Account: "y"}); err == nil {
        t.Fatal("secret:// must fail when no backend available")
    }
}
```

`internal/secrets/cli_test.go` — `TestMainAuth`:

```go
package secrets

import (
    "bytes"
    "path/filepath"
    "strings"
    "testing"
)

// TestMainAuth exercises the complete CLI auth flow on ONE provider/account
// (per structural fix #13: unique test provider/account, no sweep across
// multiple). Covers: stdin prompt, --api-key-stdin, cleanup-failure path.
func TestMainAuth(t *testing.T) {
    path := filepath.Join(t.TempDir(), "secrets.enc")
    t.Setenv("YANSHI_TEST_PASS", "test-passphrase")

    // 1) Interactive stdin: provide API key via simulated tty input.
    in := &bytes.Buffer{}
    in.WriteString("sk-test-from-stdin\n")
    out := &bytes.Buffer{}
    cmd := AuthCommand{
        Provider:   "openai",
        Account:    "main",
        Backend:    "file",
        FilePath:   path,
        Stdin:      in,
        Stdout:     out,
        Passphrase: []byte("test-passphrase"),
    }
    if err := cmd.Run(); err != nil {
        t.Fatalf("Run interactive: %v", err)
    }
    mgr, _ := NewManager(Config{Backend: "file", FilePath: path, PassphraseEnv: "YANSHI_TEST_PASS"})
    defer func() { _ = mgr.Close() }()
    got, err := mgr.Store().Get("openai", "main")
    if err != nil {
        t.Fatalf("Get after set: %v", err)
    }
    if got != "sk-test-from-stdin" {
        t.Fatalf("got %q", got)
    }

    // 2) --api-key-stdin: read secret from raw stdin bytes (no prompt).
    raw := &bytes.Buffer{}
    raw.WriteString("sk-from-stdin-flag")
    out2 := &bytes.Buffer{}
    cmd2 := AuthCommand{
        Provider:    "openai",
        Account:     "main",
        Backend:     "file",
        FilePath:    path,
        APIKeyStdin: true,
        Stdin:       raw,
        Stdout:      out2,
        Passphrase:  []byte("test-passphrase"),
    }
    if err := cmd2.Run(); err != nil {
        t.Fatalf("Run api-key-stdin: %v", err)
    }
    // Each command owns a FileStore snapshot; reopen after cmd2 commits so the
    // assertion observes the newly encrypted on-disk state rather than mgr's
    // intentionally stale in-memory snapshot.
    if err := mgr.Close(); err != nil {
        t.Fatal(err)
    }
    mgr, err = NewManager(Config{
        Backend:       "file",
        FilePath:      path,
        PassphraseEnv: "YANSHI_TEST_PASS",
    })
    if err != nil {
        t.Fatal(err)
    }
    got2, err := mgr.Store().Get("openai", "main")
    if err != nil {
        t.Fatal(err)
    }
    if got2 != "sk-from-stdin-flag" {
        t.Fatalf("got %q after --api-key-stdin", got2)
    }

    // 3) Cleanup-failure path: --delete on a non-existent account must
    // execute the cleanup branch and return a non-nil error (recorded in
    // stderr) WITHOUT corrupting the existing entry.
    out3 := &bytes.Buffer{}
    cmd3 := AuthCommand{
        Provider: "openai",
        Account:  "nonexistent",
        Backend:  "file",
        FilePath: path,
        Delete:   true,
        Stdout:   out3,
        Passphrase: []byte("test-passphrase"),
    }
    err = cmd3.Run()
    if err == nil {
        t.Fatal("delete on missing account must return error")
    }
    if !strings.Contains(err.Error(), "not found") {
        t.Fatalf("delete error must be informative: %v", err)
    }
    // Existing entry must still be intact.
    got3, _ := mgr.Store().Get("openai", "main")
    if got3 != "sk-from-stdin-flag" {
        t.Fatalf("existing entry corrupted after failed delete: %q", got3)
    }
}

func TestAuthCommand_RejectsOversizedSecretInput(t *testing.T) {
    for _, tc := range []struct {
        name        string
        apiKeyStdin bool
        suffix      string
    }{
        {"api-key-stdin", true, ""},
        {"interactive-reader", false, "\n"},
    } {
        t.Run(tc.name, func(t *testing.T) {
            cmd := AuthCommand{
                Provider:    "openai",
                Account:     "main",
                Backend:     "file",
                FilePath:    filepath.Join(t.TempDir(), "secrets.enc"),
                Passphrase:  []byte("test-passphrase"),
                APIKeyStdin: tc.apiKeyStdin,
                Stdin: bytes.NewBufferString(
                    strings.Repeat("x", maxSecretInputBytes+1) + tc.suffix,
                ),
                Stdout: &bytes.Buffer{},
            }
            err := cmd.Run()
            if err == nil || !strings.Contains(err.Error(), "exceeds 65536 bytes") {
                t.Fatalf("want bounded-input error, got %v", err)
            }
        })
    }
}
```

### Expected

- `Manager.Resolve(ref)` 在 `Kind=secret` 时查询 backend；`env` 时从 `os.Getenv` 读；`legacy` 时返回 Raw。
- `auto` 模式无 passphrase 时返回 manager（不报错），但 `secret://` ref 解析失败。
- `AuthCommand.Run()` 处理三种模式：交互式（仅当注入的 `Stdin` 本身是 terminal `*os.File` 时调用 `term.ReadPassword`）、`--api-key-stdin`（通过 `io.LimitReader` 最多接受 64 KiB）、`--delete`；不得通过 `os.Stdin.Fd()` 绕过注入 reader。
- terminal、注入的非 terminal reader、`--api-key-stdin` 三条输入路径均在写入 backend 前拒绝超过 65536 bytes 的 secret；错误不得回显输入内容。
- 每个 `FileStore` 是内存快照；另一个 `AuthCommand` 写入后，测试必须重开 manager 再检查磁盘值，不能用旧 manager 制造错误断言。
- `--delete` 在条目不存在时返回错误且不破坏其他条目（cleanup 失败路径）。

### Implementation

`internal/secrets/manager.go`:

```go
package secrets

import (
    "fmt"
    "io"
    "os"
)

// Manager is the unified credential resolver. It owns a Store (keyring,
// file, or none) and resolves CredentialRefs to plaintext secrets. The
// Manager itself never holds plaintext secrets beyond the Resolve return
// value; callers are responsible for registering them with the Redactor.
type Manager struct {
    cfg    Config
    store  Store
    redact *Redactor
    warn   *SafeLogger
}

// Config is the manager-level view of secrets configuration.
type Config struct {
    Backend       string
    FilePath      string
    PassphraseEnv string
    // Passphrase is a runtime-only injection used by CLI/tests. It is never
    // serialized and takes precedence over PassphraseEnv when non-empty.
    Passphrase []byte
    // Stderr is the optional warning sink. NewManager always wraps it in a
    // SafeLogger using the same Redactor owned by the Manager.
    Stderr io.Writer
}

// NewManager constructs a Manager per Backend:
//   - "auto": try OS keyring; if unavailable and passphrase is set, use
//     fileStore; otherwise return a Manager with store=nil (secret:// refs
//     will fail, but env:// and legacy still work). Soft-degrade: never fatals.
//   - "keyring": force OS keyring; if unavailable, return error.
//   - "file": force fileStore; require passphrase.
//   - "none": store stays nil; only env:// and legacy resolve.
func NewManager(cfg Config) (*Manager, error) {
    m := &Manager{cfg: cfg, redact: NewRedactor()}
    if cfg.Stderr != nil {
        m.warn = NewSafeLogger(cfg.Stderr, m.redact)
    }
    switch cfg.Backend {
    case "", "auto":
        ks := NewOSKeyringStore()
        // Use Available() probe so "backend not installed" stays distinct
        // from "entry missing". A backend that answers "not found" to a
        // probe Get IS available; the old logic confused the two and fell
        // back to fileStore on every cold-cache lookup.
        if err := ks.Available(); err == nil {
            m.store = ks
            break
        }
        // Keyring unavailable: try fileStore if passphrase configured.
        pass := m.passphrase()
        if len(pass) > 0 && cfg.FilePath != "" {
            fs, ferr := NewFileStore(cfg.FilePath, pass)
            if ferr != nil {
                if m.warn != nil {
                    m.warn.Printf("warn: keyring unavailable and fileStore failed: %v (secret:// refs will not resolve)", ferr)
                }
            } else {
                m.store = fs
            }
        } else if m.warn != nil {
            m.warn.Println("warn: keyring unavailable and no passphrase configured; secret:// refs will not resolve")
        }
    case "keyring":
        ks := NewOSKeyringStore()
        if err := ks.Available(); err != nil {
            return nil, fmt.Errorf("secrets: backend=keyring but OS keyring unavailable: %w", err)
        }
        m.store = ks
    case "file":
        pass := m.passphrase()
        if len(pass) == 0 {
            return nil, fmt.Errorf("secrets: backend=file but no passphrase configured")
        }
        fs, err := NewFileStore(cfg.FilePath, pass)
        if err != nil {
            return nil, err
        }
        m.store = fs
    case "none":
        // store stays nil
    default:
        return nil, fmt.Errorf("secrets: unknown backend %q", cfg.Backend)
    }
    return m, nil
}

func (m *Manager) passphrase() []byte {
    if len(m.cfg.Passphrase) > 0 {
        return append([]byte(nil), m.cfg.Passphrase...)
    }
    if m.cfg.PassphraseEnv == "" {
        return nil
    }
    return []byte(os.Getenv(m.cfg.PassphraseEnv))
}

// Store returns the underlying Store (may be nil in "none" or soft-degrade).
// Exposed so the CLI can call Set/Delete directly.
func (m *Manager) Store() Store { return m.store }

// Redactor returns the Manager's redactor so callers can register resolved
// secrets in one place.
func (m *Manager) Redactor() *Redactor { return m.redact }

// Resolve returns the plaintext secret for ref. Fails closed for Kind=secret
// when no backend is available.
func (m *Manager) Resolve(ref CredentialRef) (string, error) {
    switch ref.Kind {
    case "", "none":
        return "", nil
    case "secret":
        if m.store == nil {
            return "", fmt.Errorf("secrets: cannot resolve %s/%s: no backend configured", ref.Service, ref.Account)
        }
        return m.store.Get(ref.Service, ref.Account)
    case "env":
        v, ok := os.LookupEnv(ref.VarName)
        if !ok {
            return "", fmt.Errorf("secrets: env var %s not set", ref.VarName)
        }
        return v, nil
    case "legacy":
        return ref.Raw, nil
    default:
        return "", fmt.Errorf("secrets: unknown ref kind %q", ref.Kind)
    }
}

// Set stores a secret in the configured backend. Used by the CLI auth flow.
func (m *Manager) Set(service, account, secret string) error {
    if m.store == nil {
        return fmt.Errorf("secrets: no backend configured")
    }
    return m.store.Set(service, account, secret)
}

// Delete removes a secret. Errors from the underlying store (including
// "not found") are returned to the caller so the CLI can format them.
func (m *Manager) Delete(service, account string) error {
    if m.store == nil {
        return fmt.Errorf("secrets: no backend configured")
    }
    return m.store.Delete(service, account)
}

// Close is a no-op for current backends; exists for future keyed backends.
func (m *Manager) Close() error { return nil }
```

`internal/secrets/cli.go`:

```go
package secrets

import (
    "fmt"
    "io"
    "os"
    "strings"

    "golang.org/x/term"
)

const maxSecretInputBytes = 64 * 1024

// AuthCommand implements the `yanshi auth` subcommand. It supports three
// modes: interactive (prompt for key with echo off), --api-key-stdin (read
// raw bytes from stdin), and --delete (remove the stored entry). All modes
// target exactly ONE (provider, account) pair — the CLI forbids bulk
// operations to prevent accidental leak.
type AuthCommand struct {
    Provider    string
    Account     string
    Backend       string
    FilePath      string
    PassphraseEnv string
    Passphrase    []byte // runtime-only; takes precedence over PassphraseEnv
    APIKeyStdin   bool
    Delete      bool

    // Manager is optional. Production runAuthSub injects its already-open
    // process Manager so set/delete share the same FileStore snapshot and
    // Redactor. nil preserves standalone package tests.
    Manager *Manager
    Stdin   io.Reader
    Stdout  io.Writer
}

// Run executes the command. Returns an error on any failure; the caller is
// expected to print it and exit non-zero. Cleanup branches (--delete on a
// missing entry) still execute fully and return the underlying store error
// (per structural fix #13: cleanup failure path must be reachable & correct).
func (c *AuthCommand) Run() error {
    if c.Provider == "" || c.Account == "" {
        return fmt.Errorf("auth: --provider and --account are required")
    }
    if c.Stdin == nil {
        c.Stdin = os.Stdin
    }
    if c.Stdout == nil {
        c.Stdout = os.Stdout
    }

    mgr := c.Manager
    ownsManager := false
    if mgr == nil {
        var err error
        mgr, err = NewManager(Config{
            Backend:       c.Backend,
            FilePath:      c.FilePath,
            PassphraseEnv: c.PassphraseEnv,
            Passphrase:    c.Passphrase,
        })
        if err != nil {
            return err
        }
        ownsManager = true
    }
    if ownsManager {
        defer mgr.Close()
    }

    if c.Delete {
        if err := mgr.Delete(c.Provider, c.Account); err != nil {
            return fmt.Errorf("auth: delete %s/%s: %w", c.Provider, c.Account, err)
        }
        fmt.Fprintf(c.Stdout, "deleted %s/%s\n", c.Provider, c.Account)
        return nil
    }

    var key string
    if c.APIKeyStdin {
        raw, err := readLimitedSecret(c.Stdin)
        if err != nil {
            return fmt.Errorf("auth: read stdin: %w", err)
        }
        key = strings.TrimRight(string(raw), "\r\n")
    } else {
        fmt.Fprintf(c.Stdout, "Enter API key for %s/%s: ", c.Provider, c.Account)
        // Only a real injected *os.File may use term.ReadPassword. Consulting
        // os.Stdin here would ignore AuthCommand.Stdin and make buffer/pipe
        // tests read from the test runner's terminal.
        inputFile, isFile := c.Stdin.(*os.File)
        if !isFile || !term.IsTerminal(int(inputFile.Fd())) {
            line, err := readLine(c.Stdin)
            if err != nil {
                return fmt.Errorf("auth: read stdin line: %w", err)
            }
            key = strings.TrimRight(line, "\r\n")
        } else {
            b, err := term.ReadPassword(int(inputFile.Fd()))
            if err != nil {
                return fmt.Errorf("auth: read password: %w", err)
            }
            fmt.Fprintln(c.Stdout)
            if len(b) > maxSecretInputBytes {
                return fmt.Errorf("auth: secret input exceeds %d bytes", maxSecretInputBytes)
            }
            key = string(b)
        }
    }

    if key == "" {
        return fmt.Errorf("auth: empty API key refused")
    }
    if err := mgr.Set(c.Provider, c.Account, key); err != nil {
        return fmt.Errorf("auth: set %s/%s: %w", c.Provider, c.Account, err)
    }
    fmt.Fprintf(c.Stdout, "stored %s/%s\n", c.Provider, c.Account)
    return nil
}

func readLimitedSecret(r io.Reader) ([]byte, error) {
    raw, err := io.ReadAll(io.LimitReader(r, maxSecretInputBytes+1))
    if err != nil {
        return nil, err
    }
    if len(raw) > maxSecretInputBytes {
        return nil, fmt.Errorf("secret input exceeds %d bytes", maxSecretInputBytes)
    }
    return raw, nil
}

func readLine(r io.Reader) (string, error) {
    var buf strings.Builder
    b := make([]byte, 1)
    for {
        if _, err := r.Read(b); err != nil {
            if err == io.EOF && buf.Len() > 0 {
                return buf.String(), nil
            }
            return "", err
        }
        if b[0] == '\n' {
            return buf.String(), nil
        }
        if buf.Len() >= maxSecretInputBytes {
            return "", fmt.Errorf("secret input exceeds %d bytes", maxSecretInputBytes)
        }
        buf.WriteByte(b[0])
    }
}
```

### Pass

```sh
go test ./internal/secrets -run "TestManager|TestMainAuth|TestAuthCommand"
```

### Commit

```
feat(secrets): Manager + CLI auth command (S10-L3)

Manager implements auto/keyring/file/none backends with soft-degrade (auto
mode without passphrase still resolves env:// and legacy refs). AuthCommand
covers interactive, --api-key-stdin, and --delete modes on one
(provider,account); the --delete failure path returns the underlying store
error without corrupting other entries.
```

---

## Task 4 — S10-L4: 边界脱敏 (WS × 1 + SSE × 10 + SQLite × 3)

**结构性修复落点:** #4 (统一 safe logger/error sink 覆盖所有运行时 stderr/WS/SSE/SQLite，**含 `CreateSession` 和 `UpdateSessionTitle`**)、#11 (writeSSEFrame 10 处调用全覆盖，改 writeSSEError 签名 / Server 方法)。

### Files

- Modify `internal/api/http/server.go` (`Server` 加 `redactor *secrets.Redactor`)
- Modify `internal/api/http/ws.go` (`wsConn.write` 在 marshal 后、`WriteMessage` 前调 `RedactJSON`)
- Modify `internal/api/http/chat.go` (`writeSSEFrame` / `writeSSEError` 调 `RedactJSON`)
- Modify `internal/store/store.go` (`Store` 加 `redactor *secrets.Redactor`)
- Modify `internal/store/session.go` (`CreateSession` / `AppendMessage` / `UpdateSessionTitle` 三处)
- Modify `internal/api/http/ws_test.go` (新增 WS serialized-boundary 脱敏测试)
- Modify `internal/api/http/chat_test.go` (新增 SSE serialized-boundary 与 pre-stream error 脱敏测试)
- Modify `internal/store/session_test.go` (三处脱敏测试)

### Failure Test (RED)

`internal/api/http/chat_test.go` — `TestSSE_RedactsAllFrameTypes`:

```go
package http

import (
    "encoding/json"
    "net/http/httptest"
    "strings"
    "testing"

    "github.com/x6nux/yanshi/internal/proto"
    "github.com/x6nux/yanshi/internal/secrets"
)

// TestSSE_RedactsAllFrameTypes validates the serialized boundary shared by
// every ServerFrame passed through writeSSEFrame. The test deliberately calls
// the boundary helper directly; it does not pretend to execute each chat.go
// handler branch. Because all production call sites must adopt the new helper
// signature for chat.go to compile, one boundary implementation protects them
// uniformly. The secret contains JSON-meaningful bytes (quote + backslash), so
// RedactJSON must redact both raw and escaped spellings.
func TestSSE_RedactsAllFrameTypes(t *testing.T) {
    r := secrets.NewRedactor()
    secret := `sk-sse-"quoted"-\slash`
    r.Register(secret)
    structured, err := json.Marshal(map[string]string{"value": secret})
    if err != nil {
        t.Fatal(err)
    }

    cases := []proto.ServerFrame{
        proto.NewError("failed with key " + secret),
        proto.NewDone(),
        proto.NewCompactChunk("chunk contains " + secret),
        proto.NewHistoryReplaced(nil),
        proto.NewStatus("m", "", 0, 0, 0, 0),
        proto.NewStructuredResult(structured),
    }
    for i, f := range cases {
        rec := httptest.NewRecorder()
        writeSSEFrame(rec, nil, f, r)
        body := rec.Body.String()
        if strings.Contains(body, secret) || strings.Contains(body, `sk-sse-\"quoted\"-\\slash`) {
            t.Fatalf("case %d (%s) leaked secret: %q", i, f.Type, body)
        }
    }

    // writeSSEError is the pre-stream path used by both current call sites.
    rec := httptest.NewRecorder()
    writeSSEError(rec, "pre-stream error with "+secret, r)
    if strings.Contains(rec.Body.String(), secret) ||
        strings.Contains(rec.Body.String(), `sk-sse-\"quoted\"-\\slash`) {
        t.Fatalf("writeSSEError leaked: %q", rec.Body.String())
    }
}
```

`internal/api/http/ws_test.go` — `TestWS_RedactsOutboundFrames`:

```go
// Import delta for ws_test.go:
//   "net/http"
//   "github.com/x6nux/yanshi/internal/secrets"
// Existing imports already provide httptest, strings, testing,
// github.com/gorilla/websocket, require, and proto.
func TestWS_RedactsOutboundFrames(t *testing.T) {
    r := secrets.NewRedactor()
    secret := `sk-ws-"quoted"-\slash`
    r.Register(secret)

    upgrader := websocket.Upgrader{}
    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
        raw, err := upgrader.Upgrade(w, req, nil)
        require.NoError(t, err)
        defer raw.Close()
        wc := &wsConn{Conn: raw, redactor: r}
        wc.write(proto.NewError("leaked: " + secret))
    }))
    defer srv.Close()

    c := dial(t, "ws"+strings.TrimPrefix(srv.URL, "http"))
    defer c.Close()
    _, data, err := c.ReadMessage()
    require.NoError(t, err)
    require.NotContains(t, string(data), secret)
    require.NotContains(t, string(data), `sk-ws-\"quoted\"-\\slash`)
    require.Contains(t, string(data), "[REDACTED]")
}
```

`internal/store/session_test.go` — `TestStore_RedactsAllWritePaths`:

```go
func TestStore_RedactsAllWritePaths(t *testing.T) {
    st, err := Open(":memory:")
    if err != nil {
        t.Fatal(err)
    }
    defer st.Close()
    r := secrets.NewRedactor()
    secret := "sk-db-leak-12345"
    r.Register(secret)
    st.SetRedactor(r)

    id, err := st.CreateSession("session with " + secret)
    if err != nil {
        t.Fatal(err)
    }
    var title string
    if err := st.DB.QueryRow("SELECT title FROM sessions WHERE id = ?", id).Scan(&title); err != nil {
        t.Fatal(err)
    }
    if strings.Contains(title, secret) {
        t.Fatalf("CreateSession leaked: %q", title)
    }

    if err := st.AppendMessage(id, 0, "user", "message with "+secret); err != nil {
        t.Fatal(err)
    }
    msgs, err := st.Messages(id)
    if err != nil {
        t.Fatal(err)
    }
    if strings.Contains(msgs[len(msgs)-1].Content, secret) {
        t.Fatalf("AppendMessage leaked: %q", msgs[len(msgs)-1].Content)
    }

    if err := st.UpdateSessionTitle(id, "title with "+secret); err != nil {
        t.Fatal(err)
    }
    if err := st.DB.QueryRow("SELECT title FROM sessions WHERE id = ?", id).Scan(&title); err != nil {
        t.Fatal(err)
    }
    if strings.Contains(title, secret) {
        t.Fatalf("UpdateSessionTitle leaked: %q", title)
    }
}
```

### Expected

- `Server` 持 `redactor *secrets.Redactor`；`writeSSEFrame` 只在 `ServerFrame.SSEEvent()` 已生成 JSON 后调用 `RedactJSON`，因此覆盖真实 `Text`、`ToolArgs`、`StructuredResult`、`Messages`、`Sessions` 以及未来新增字段；`writeSSEError` 必须委托这一边界，不能访问不存在的 `Error` / `Content` 字段。
- `wsConn` 持 `redactor *secrets.Redactor`；`write` 在 `json.Marshal` 后、`WriteMessage` 前对 marshal 出来的 byte slice 执行字符串替换（即先 marshal 再 `bytes.ReplaceAll`）。
- `Store.SetRedactor(r)` 注入；`CreateSession` / `AppendMessage` / `UpdateSessionTitle` 三处在写入前对 `title` / `content` 调 redactor。
- 一个新的进程级 redactor（由 bootstrap 合并所有 provider 的 redactor）同时注入到 Server 和 Store。

### Implementation

`internal/api/http/server.go` 精确结构改动（`Config` 与 `Server` 各增加一个字段，`New` 透传；其它现有字段保持原样）：

```go
// Config holds settings for constructing a Server.
type Config struct {
    Token      string
    Compaction CompactionConfig
    Store      *store.Store
    Redactor   *secrets.Redactor
}

// Server holds the mux, auth token, compaction config, persistence adapter,
// and the process-wide redactor used by both transports.
type Server struct {
    mux        *http.ServeMux
    token      string
    compaction CompactionConfig
    store      *store.Store
    redactor   *secrets.Redactor
}

func New(cfg Config) *Server {
    return &Server{
        mux:        http.NewServeMux(),
        token:      cfg.Token,
        compaction: cfg.Compaction,
        store:      cfg.Store,
        redactor:   cfg.Redactor,
    }
}
```

`internal/api/http/ws.go` 在现有 upgrade 成功点把同一 redactor 放进连接：

```go
conn := &wsConn{Conn: raw, redactor: s.redactor}
```

`internal/api/http/chat.go` 改动 (修改签名 — 兼容性靠所有 10 处调用点都升级):

```go
// Before:
//   func writeSSEFrame(w http.ResponseWriter, fl http.Flusher, f proto.ServerFrame)
//   func writeSSEError(w http.ResponseWriter, msg string)
//
// After:
func writeSSEFrame(w http.ResponseWriter, fl http.Flusher, f proto.ServerFrame, r *secrets.Redactor) {
    event, data := f.SSEEvent()
    if r != nil {
        // Redact the already-marshaled JSON bytes. RedactJSON handles both raw
        // and JSON-escaped forms (quotes/backslashes/control characters), so
        // every current and future ServerFrame field is covered without naming
        // fields that do not exist on proto.ServerFrame.
        data = r.RedactJSON(data)
    }
    _, _ = w.Write([]byte("event: " + event + "\ndata: "))
    _, _ = w.Write(data)
    _, _ = w.Write([]byte("\n\n"))
    if fl != nil {
        fl.Flush()
    }
}

func writeSSEError(w http.ResponseWriter, msg string, r *secrets.Redactor) {
    fl, _ := w.(http.Flusher)
    w.Header().Set("Content-Type", "text/event-stream")
    w.Header().Set("Cache-Control", "no-cache")
    writeSSEFrame(w, fl, proto.NewError(msg), r)
    writeSSEFrame(w, fl, proto.NewDone(), r)
}
```

同文件 10 个现有输出点全部显式追加 `s.redactor`，不得漏 pre-stream 分支：

```go
// Pre-stream errors (current L58, L66):
writeSSEError(w, "empty request", s.redactor)
writeSSEError(w, errMsg, s.redactor)

// The eight direct frame sites (current L98, L106, L109, L177, L200,
// L212, L222, L223) keep their existing frame expression and append the
// fourth argument. Representative exact shapes:
writeSSEFrame(w, fl, proto.NewCompactChunk(chunk), s.redactor)
writeSSEFrame(w, fl, proto.NewHistoryReplaced(compactedHistory), s.redactor)
writeSSEFrame(w, fl, st, s.redactor)
writeSSEFrame(w, fl, f, s.redactor)
writeSSEFrame(w, fl, proto.NewError(schemaErrText), s.redactor)
writeSSEFrame(w, fl, proto.NewStructuredResult(structuredResult), s.redactor)
writeSSEFrame(w, fl, sseStatus, s.redactor)
writeSSEFrame(w, fl, proto.NewDone(), s.redactor)
```

其中 schema failure 先构造 `schemaErrText`，避免在代码块中用省略表达式：

```go
schemaErrText := "output did not match the required schema after " +
    strconv.Itoa(attempt+1) + " attempt(s): " + verr.Error()
writeSSEFrame(w, fl, proto.NewError(schemaErrText), s.redactor)
```

`internal/api/http/ws.go` 改动 (`wsConn` 增加字段 + `write` 边界脱敏):

```go
type wsConn struct {
    *websocket.Conn
    mu       sync.Mutex
    redactor *secrets.Redactor
}

func (w *wsConn) write(f proto.ServerFrame) {
    data, err := json.Marshal(f)
    if err != nil {
        return
    }
    if w.redactor != nil {
        // Redact the marshaled JSON, including escaped forms of secrets.
        data = w.redactor.RedactJSON(data)
    }
    w.mu.Lock()
    defer w.mu.Unlock()
    _ = w.Conn.WriteMessage(websocket.TextMessage, data)
}
```

`internal/store/store.go` 改动:

```go
type Store struct {
    DB       *sql.DB
    redactor *secrets.Redactor
}

// SetRedactor injects the process-wide redactor. Called by bootstrap after
// all provider secrets have been registered.
func (s *Store) SetRedactor(r *secrets.Redactor) { s.redactor = r }

func (s *Store) redact(text string) string {
    if s.redactor == nil {
        return text
    }
    return s.redactor.Redact(text)
}
```

`internal/store/session.go` 改动 (三处写入):

```go
// CreateSession inserts a new session and returns its id. The title is
// redacted before persistence so secret substrings never reach SQLite.
func (s *Store) CreateSession(title string) (string, error) {
    id := newID()
    now := time.Now().Unix()
    _, err := s.DB.Exec(
        "INSERT INTO sessions (id, title, created_at, updated_at) VALUES (?, ?, ?, ?)",
        id, s.redact(title), now, now,
    )
    if err != nil {
        return "", err
    }
    return id, nil
}

// AppendMessage adds a message to a session at the given sequence number.
func (s *Store) AppendMessage(sessionID string, seq int, role, content string) error {
    id := newID()
    now := time.Now().Unix()
    _, err := s.DB.Exec(
        `INSERT INTO messages (id, session_id, seq, role, content, created_at)
         VALUES (?, ?, ?, ?, ?, ?)`,
        id, sessionID, seq, role, s.redact(content), now,
    )
    if err != nil {
        return err
    }
    _, err = s.DB.Exec("UPDATE sessions SET updated_at = ? WHERE id = ?", now, sessionID)
    return err
}

// UpdateSessionTitle updates the title of a session after redaction.
func (s *Store) UpdateSessionTitle(sessionID, title string) error {
    _, err := s.DB.Exec(
        "UPDATE sessions SET title = ? WHERE id = ?",
        s.redact(title), sessionID,
    )
    return err
}
```

### Pass

```sh
go test ./internal/api/http -run "TestSSE_RedactsAllFrameTypes|TestWS_RedactsOutboundFrames"
go test ./internal/store -run TestStore_RedactsAllWritePaths
```

### Commit

```
feat(secrets): boundary redaction at WS/SSE/SQLite (S10-L4)

writeSSEFrame and writeSSEError now take a *secrets.Redactor; all 10 SSE
output points in chat.go are covered by the boundary function. wsConn.write
operates on marshaled JSON so new ServerFrame fields inherit coverage
automatically. Store.CreateSession / AppendMessage / UpdateSessionTitle all
redact before SQL write.
```

---

## Task 5 — S10-L5: bootstrap 接 secrets + 写回 cfg.APIKey

**结构性修复落点:** #1 (O03 凭据必须到达 BuildProviders — 定义凭据源/引用 + 优先级，在模型前装配 auth.Manager — S10 部分先到位)。

### Files

- Modify `internal/bootstrap/bootstrap.go`
- Modify `internal/bootstrap/bootstrap_test.go`
- Modify `internal/cli/session.go` / `internal/cli/session_test.go`（同一 process `SafeOutput`；替换 Close 的 raw serve stderr）
- Modify `cmd/yanshi/main.go` / `cmd/yanshi/main_test.go`（所有 direct stderr/FlagSet sink 使用 process `SafeLogger`）

### Failure Test (RED)

`internal/bootstrap/bootstrap_test.go` — `TestBuild_SecretsResolvesBeforeProviders`:

```go
func TestBuild_SecretsResolvesBeforeProviders(t *testing.T) {
    // Stage a config with env:// ref + legacy-insecure opt-in + raw literal.
    dir := t.TempDir()
    t.Setenv("YANSHI_TEST_OPENAI_KEY", "sk-from-env")
    cfg := &config.Config{
        LLM: config.LLMConfig{
            Providers: []config.ProviderConfig{
                {Name: "p1", Kind: "openai", Model: "gpt-fake", APIKey: "env://YANSHI_TEST_OPENAI_KEY"},
                {Name: "p2", Kind: "openai", Model: "gpt-fake", APIKey: "sk-legacy-pass"},
                // p3 uses a real raw literal without opt-in: must fail to build.
                {Name: "p3", Kind: "openai", Model: "gpt-fake", APIKey: "sk-raw-not-allowed"},
            },
        },
        Secrets: config.SecretsConfig{Backend: "none"},
        Storage: config.StorageConfig{SQLitePath: filepath.Join(dir, "yanshi.db")},
    }
    _, err := Build(Options{Cfg: cfg, FakeModel: true})
    if err == nil {
        t.Fatal("Build must fail when a raw-literal APIKey is present and Auth.LegacyInsecure is false")
    }

    // Flip the opt-in and drop p3 to assert the happy path.
    cfg.Auth.LegacyInsecure = true
    cfg.LLM.Providers = cfg.LLM.Providers[:2]
    app, err := Build(Options{Cfg: cfg, FakeModel: true})
    if err != nil {
        t.Fatalf("Build: %v", err)
    }
    defer app.Shutdown(context.Background())
    if cfg.LLM.Providers[0].APIKey != "sk-from-env" {
        t.Errorf("p1 APIKey not resolved: %q", cfg.LLM.Providers[0].APIKey)
    }
    if cfg.LLM.Providers[1].APIKey != "sk-legacy-pass" {
        t.Errorf("p2 APIKey not resolved: %q", cfg.LLM.Providers[1].APIKey)
    }
    leaked := app.Redactor.Redact("error containing sk-from-env")
    if strings.Contains(leaked, "sk-from-env") {
        t.Errorf("redactor did not register resolved env secret: %q", leaked)
    }
}
```

### Expected

- `Build` 在 L120 之前构造 `secrets.Manager`，对每个 `cfg.LLM.Providers[i].APIKey` 调 `ParseCredentialRef` + `Manager.Resolve`，写回 `cfg.LLM.Providers[i].APIKey`。
- Raw literal 在 `Auth.LegacyInsecure == false` 时 → Build 返回错误（fail-closed）。
- `Build` 在解析后将明文注册到合并 redactor，把 redactor 注入 `Server` 和 `Store`。

### Implementation

`internal/bootstrap/bootstrap.go` 改动。Task 5 先把独立可编译所需的类型字段落地；Task 8 只再增加 `App.Auth`、`Options.AuthDeps` 与 `Options.ProviderBuilder`：

```go
// Add to the existing App struct.
Redactor *secrets.Redactor

// Add to the existing Options struct. A pre-loaded config is a test seam;
// production leaves it nil and still uses ConfigPath.
Cfg *config.Config
// Output is the process-wide redactor/logger aggregation. Production main and
// cli.Session inject the same pointer; nil is normalized once by Build.
Output *secrets.SafeOutput
```

把 `Build` 当前无条件 `config.Load` 前导替换为：

```go
output := effectiveSafeOutput(opts.Output)
redactor := output.Redactor
safeLog := output.Logger

var cfg *config.Config
if opts.Cfg != nil {
    cfg = opts.Cfg
} else {
    loaded, err := config.Load(opts.ConfigPath)
    if err != nil {
        return nil, fmt.Errorf("bootstrap: load config: %w", err)
    }
    cfg = loaded
}
```

在真实 `store.Open` 成功后立即建立统一失败清理 guard，并删除后续错误分支里零散的 `st.Close()`；最终成功返回前把 `closeStoreOnError=false`。这覆盖 Task 5/8 新增的所有 early return，避免 Windows 测试残留打开的 SQLite：

```go
closeStoreOnError := true
defer func() {
    if closeStoreOnError {
        _ = st.Close()
    }
}()
```

在现有 Build 函数中插入 secrets 装配段，位置在 `store.Open` 之后、`einollm.BuildProviders(cfg)` 之前：

```go
// In Build, after cfg load and before einollm.BuildProviders(cfg):

// --- S10: secrets + redactor -------------------------------------------
secretMgr, err := secrets.NewManager(secrets.Config{
    Backend:       cfg.Secrets.Backend,
    FilePath:      cfg.Secrets.FilePath,
    PassphraseEnv: cfg.Secrets.PassphraseEnv,
    Output:        output,
})
if err != nil {
    return nil, fmt.Errorf("bootstrap: secrets manager: %w", err)
}
redactor := secretMgr.Redactor()

// legacyRaw is the per-process opt-in: only when Auth.LegacyInsecure is
// explicitly true do raw literal APIKeys get accepted. Otherwise
// ParseCredentialRef fails closed and Build aborts — the threat model is
// "raw paste of an API key silently working defeats S10".
legacyRaw := cfg.Auth.LegacyInsecure

for i := range cfg.LLM.Providers {
    p := &cfg.LLM.Providers[i]
    if p.APIKey == "" {
        continue
    }
    ref, err := secrets.ParseCredentialRef(p.APIKey, legacyRaw)
    if err != nil {
        return nil, fmt.Errorf("bootstrap: provider %s: %w", p.Name, err)
    }
    resolved, err := secretMgr.Resolve(ref)
    if err != nil {
        return nil, fmt.Errorf("bootstrap: provider %s resolve credential reference: %w", p.Name, err)
    }
    if resolved == "" {
        return nil, fmt.Errorf("bootstrap: provider %s resolved credential is empty", p.Name)
    }
    redactor.Register(resolved)
    p.APIKey = resolved
}

// Inject at the real construction sites (there is no `serverCfg` variable):
st.SetRedactor(redactor)
// In the existing apihttp.New(apihttp.Config{...}) literal add:
//     Redactor: redactor,
// In the final return &App{...} literal add:
//     Redactor: redactor,
// Immediately before that successful return set:
//     closeStoreOnError = false
//
// Replace the two real non-fatal bootstrap warnings exactly:
//
//     safeLog.Printf("yanshi: vcs init failed (tracking disabled): %v\n", vcsErr)
//     safeLog.Printf("yanshi: skill plugin discovery failed: %v\n", err)
//
// `safeLog` is defined once above from `output.Logger`; no raw
// fmt.Fprint*(os.Stderr, ...) remains in bootstrap.go.

func effectiveSafeOutput(output *secrets.SafeOutput) *secrets.SafeOutput {
    if output != nil {
        return output
    }
    return secrets.NewSafeOutput(os.Stderr, nil)
}
```

`internal/cli/session.go` — 在 `Options` 结构加 `Output` 字段，`serve` goroutine 使用 process logger 而非 raw stderr：

```go
// Add to the existing Options struct:
Output *secrets.SafeOutput

// In Session struct (unexported): add:
output *secrets.SafeOutput

// In newSession, after s.output is set:
s.output = opts.Output
if s.output == nil {
    s.output = secrets.NewSafeOutput(io.Discard, nil)
}

// When passing Options to bootstrap.Build:
app, err := bootstrap.Build(bootstrap.Options{
    ConfigPath: s.configPath, FakeModel: s.fakeModel, Output: s.output,
})

// Replace raw serve-error:
s.output.Logger.Println("serve:", err)
```

`session.go` imports: 若 `fmt` 只被该 serve-error 使用则删除；增加 `io` 与 `"github.com/x6nux/yanshi/internal/secrets"`。

Task 5 的 `App.Redactor` 与 `Options.Cfg` 已在本 Task 落地，因此本 Task 的 RED 不依赖 Task 8 才能编译。

### Pass

```sh
go test ./internal/bootstrap -run TestBuild_SecretsResolvesBeforeProviders
```

### Commit

```
feat(secrets): bootstrap resolves credential refs before BuildProviders (S10-L5)

Raw literal APIKeys are refused (fail-closed) unless the provider opts in
with "legacy-insecure". Resolved plaintext keys are written back to
cfg.LLM.Providers[i].APIKey and registered with the process-wide redactor,
which is injected into the Store and Server for boundary redaction.
```

---

## Task 6 — O03-L1: auth.Manager core + CredentialSource

**结构性修复落点:** #1 (凭据源到达 BuildProviders — O03 侧的 Manager 接口定义)、#3 (secret:// / env:// / legacy-insecure 显式 + raw literal 拒绝 — auth 层也调 secrets.ParseCredentialRef)。

### Files

- Create `internal/auth/auth.go`
- Create `internal/auth/auth_test.go`

### Failure Test (RED)

`internal/auth/auth_test.go`:

```go
package auth

import (
    "context"
    "errors"
    "path/filepath"
    "testing"

    "github.com/x6nux/yanshi/internal/secrets"
)

func newTestManager(t *testing.T) *Manager {
    t.Helper()
    path := filepath.Join(t.TempDir(), "secrets.enc")
    t.Setenv("YANSHI_PASSPHRASE", "test-pass")
    smgr, err := secrets.NewManager(secrets.Config{
        Backend:       "file",
        FilePath:      path,
        PassphraseEnv: "YANSHI_PASSPHRASE",
    })
    if err != nil {
        t.Fatalf("secrets.NewManager: %v", err)
    }
    return NewManager(smgr)
}

func TestManager_ResolveSource(t *testing.T) {
    m := newTestManager(t)
    _ = m.secrets.Store().Set("openai", "main", "sk-from-secret-store")

    cases := []struct {
        name        string
        in          string
        allowLegacy bool
        want        string
        err         bool
    }{
        {"secret ref", "secret://openai/main", false, "sk-from-secret-store", false},
        {"env ref", "env://PATH", false, "", false},
        {"legacy opt-in", "sk-legacy", true, "sk-legacy", false},
        {"raw literal refused", "sk-raw", false, "", true},
    }
    for _, c := range cases {
        t.Run(c.name, func(t *testing.T) {
            src := CredentialSource{APIKeyRef: c.in, LegacyInsecure: c.allowLegacy}
            got, err := m.ResolveAPIKey(context.Background(), src)
            if c.err {
                if err == nil {
                    t.Fatalf("ResolveAPIKey(%q): want error", c.in)
                }
                if !errors.Is(err, secrets.ErrRawLiteralRefused) {
                    t.Fatalf("ResolveAPIKey(%q): want ErrRawLiteralRefused, got %v", c.in, err)
                }
                return
            }
            if err != nil {
                t.Fatalf("ResolveAPIKey(%q): %v", c.in, err)
            }
            if c.name == "secret ref" && got != c.want {
                t.Fatalf("got %q want %q", got, c.want)
            }
            if c.name == "legacy opt-in" && got != c.want {
                t.Fatalf("got %q want %q", got, c.want)
            }
        })
    }
}

func TestManager_Status_Logout(t *testing.T) {
    m := newTestManager(t)
    _ = m.secrets.Store().Set("openai", "main", "k")
    st, err := m.Status("openai", "main")
    if err != nil {
        t.Fatalf("Status: %v", err)
    }
    if !st.Authenticated {
        t.Fatal("expected Authenticated=true")
    }
    if err := m.Logout("openai", "main"); err != nil {
        t.Fatalf("Logout: %v", err)
    }
    st, _ = m.Status("openai", "main")
    if st.Authenticated {
        t.Fatal("expected Authenticated=false after Logout")
    }
}

func TestManager_ProviderAccountUniqueness(t *testing.T) {
    m := newTestManager(t)
    _ = m.secrets.Store().Set("openai", "main", "k1")
    _ = m.secrets.Store().Set("openai", "alt", "k2")
    _ = m.secrets.Store().Set("anthropic", "main", "k3")

    seen := map[string]bool{}
    list, _ := m.ListAccounts("openai")
    for _, a := range list {
        key := "openai" + "/" + a
        if seen[key] {
            t.Fatalf("duplicate %s", key)
        }
        seen[key] = true
    }
    if len(list) != 2 {
        t.Fatalf("ListAccounts(openai) = %v, want 2", list)
    }
}
```

### Expected

- `auth.Manager` 持 `secrets.Manager` 指针；`ResolveAPIKey(ctx, src)` 根据 `src.APIKeyRef` 调 `secrets.ParseCredentialRef`（**fail-closed on raw literal**）+ `secrets.Manager.Resolve`。
- `Status(provider, account)` 返回 `Status{Authenticated bool, Source string, LastUsed time.Time}`，查询 secrets.Store。
- `Logout` 调 `secrets.Manager.Delete`。
- `ListAccounts(provider)` 返回该 provider 下所有 account（去重）。

### Implementation

`internal/auth/auth.go`:

```go
// Package auth provides provider-neutral authentication lifecycle: API key
// resolution from secret:// / env:// / legacy-insecure refs, RFC 8628 device
// authorization (Task 7), and Status / Logout queries. It depends only on
// internal/secrets.
package auth

import (
    "context"
    "errors"
    "fmt"
    "time"

    "github.com/x6nux/yanshi/internal/secrets"
)

var ErrResolvedCredentialEmpty = errors.New("auth: resolved credential is empty")

// CredentialSource describes how a provider obtains its API key. APIKeyRef is
// a secrets.CredentialRef string (secret://service/account, env://VAR,
// "legacy-insecure", or raw literal — raw literals fail closed unless
// LegacyInsecure is true). DeviceProviderID names a DeviceProvider registered
// with the Manager for RFC 8628 device flow (empty = no device).
type CredentialSource struct {
    APIKeyRef        string
    LegacyInsecure   bool
    DeviceProviderID string
}

// Status reports the current authentication state for a (provider, account).
type Status struct {
    Provider     string
    Account      string
    Authenticated bool
    Source       string // "secret" / "env" / "legacy" / "device" / ""
    LastUsed     time.Time
}

// Manager is the auth facade. It composes a secrets.Manager (for key storage)
// with a registry of DeviceProviders added by Task 7. Task 6 deliberately
// keeps only the secrets side so this Task's RED→GREEN package test compiles
// before device.go exists.
type Manager struct {
    secrets *secrets.Manager
}

// NewManager constructs an auth Manager. Task 7 extends the struct with the
// device provider registry plus Clock/Sleeper fields.
func NewManager(sm *secrets.Manager) *Manager {
    return &Manager{secrets: sm}
}

// Secrets returns the underlying secrets.Manager (for bootstrap to inject
// the resolved redactor and for CLI auth to call Set/Delete).
func (m *Manager) Secrets() *secrets.Manager { return m.secrets }

// ResolveAPIKey resolves src.APIKeyRef to a plaintext key. Raw literals are
// refused (fail-closed) unless src.LegacyInsecure is true.
// Context is respected for env lookups via a future OS-level cancel (no I/O
// today, but the signature is ready for device flow re-use).
func (m *Manager) ResolveAPIKey(ctx context.Context, src CredentialSource) (string, error) {
    if src.APIKeyRef == "" {
        return "", nil
    }
    ref, err := secrets.ParseCredentialRef(src.APIKeyRef, src.LegacyInsecure)
    if err != nil {
        return "", err
    }
    if ctx.Err() != nil {
        return "", ctx.Err()
    }
    value, err := m.secrets.Resolve(ref)
    if err != nil {
        return "", err
    }
    if ref.Kind != "none" && value == "" {
        return "", ErrResolvedCredentialEmpty
    }
    return value, nil
}

// Status queries the secrets store for an entry. Authenticated is true iff
// the entry exists (any non-empty value).
func (m *Manager) Status(provider, account string) (Status, error) {
    if m.secrets.Store() == nil {
        return Status{Provider: provider, Account: account}, nil
    }
    v, err := m.secrets.Store().Get(provider, account)
    if errors.Is(err, secrets.ErrSecretNotFound) {
        return Status{Provider: provider, Account: account}, nil
    }
    if err != nil {
        return Status{}, fmt.Errorf("auth: query credential status: %w", err)
    }
    return Status{
        Provider:      provider,
        Account:       account,
        Authenticated: v != "",
        Source:        "secret",
    }, nil
}

// Logout deletes the stored credential. It never swallows a Store error:
// nil means the entry existed and was deleted; ErrSecretNotFound means no
// credential was present; any other error means the backend failed. Keeping
// all three outcomes distinct lets the CLI report cleanup failures with a
// non-zero exit code and lets callers retain errors.Is/As semantics.
//
// Note (structural fix #13): a previous draft swallowed ALL Delete errors
// here and returned nil, which made TestMainAuth_E2E step 4 (logout a
// nonexistent account must exit non-zero) impossible.
func (m *Manager) Logout(provider, account string) error {
    if m.secrets.Store() == nil {
        return fmt.Errorf("auth: no backend configured")
    }
    return m.secrets.Store().Delete(provider, account)
}

// ListAccounts returns the unique set of accounts stored under provider.
// Implementation: scan the fileStore entries (CGO-enabled keyring backends
// may not support enumeration — those backends return an empty list with a
// nil error; callers must not treat empty as "fully scanned").
func (m *Manager) ListAccounts(provider string) ([]string, error) {
    type enumerator interface {
        Enumerate(prefix string) ([]string, error)
    }
    enum, ok := m.secrets.Store().(enumerator)
    if !ok {
        return nil, nil
    }
    return enum.Enumerate(provider)
}
```

(FileStore 增量补 `Enumerate` 方法 — 见 Task 2 file_store.go 末尾追加: `func (fs *FileStore) Enumerate(prefix string) ([]string, error)` 返回所有 service==prefix 的 account 列表。)

### Pass

```sh
go test ./internal/auth -run "TestManager_"
```

### Commit

```
feat(auth): Manager + CredentialSource + Status/Logout (O03-L1)

auth.Manager wraps secrets.Manager and exposes ResolveAPIKey (fail-closed
on raw literals via secrets.ParseCredentialRef), Status, Logout, and
ListAccounts. Status reports Authenticated+Source; Logout preserves
ErrSecretNotFound as a non-nil cleanup result instead of claiming idempotent
success. The Manager is the facade bootstrap composes with DeviceProviders in
Task 7.
```

---

## Task 7 — O03-L2: device auth (RFC 8628) + Clock/Sleeper + HTTPS-only

**结构性修复落点:** #2 (`buildDeviceProviders` 验证 + AuthDeps.Providers 注入 + httptest CLI 验收)、#10 (Device endpoint 默认仅 HTTPS，loopback 测试例外；请求 timeout、context cancel、错误统一脱敏)、#12 (Device auth 时序：Clock/Sleeper 注入；expiry/slow_down/cancel/device-code-leak/metadata-rollback 测试；ExpiresIn<=0 返回 error)。

### Files

- Create `internal/auth/device.go`
- Create `internal/auth/device_test.go`
- Modify `internal/auth/auth.go` (device registry、injected credential-store seam、metadata compensation)

### Failure Test (RED)

`internal/auth/device_test.go` — 全覆盖 device flow 的 6 个时序边界:

```go
package auth

import (
    "context"
    "encoding/json"
    "errors"
    "fmt"
    "net/http"
    "net/http/httptest"
    "strings"
    "sync"
    "sync/atomic"
    "testing"
    "time"

    "github.com/x6nux/yanshi/internal/secrets"
)

type fakeClock struct {
    mu sync.Mutex
    t  time.Time
}
func newFakeClock(sec int64) *fakeClock {
    return &fakeClock{t: time.Unix(sec, 0)}
}
func (f *fakeClock) Now() time.Time {
    f.mu.Lock()
    defer f.mu.Unlock()
    return f.t
}
func (f *fakeClock) Advance(d time.Duration) {
    f.mu.Lock()
    f.t = f.t.Add(d)
    f.mu.Unlock()
}

type fakeSleeper struct {
    clock *fakeClock
    slept time.Duration
}
func (f *fakeSleeper) Sleep(ctx context.Context, d time.Duration) error {
    select {
    case <-ctx.Done():
        return ctx.Err()
    default:
        f.slept += d
        if f.clock != nil {
            f.clock.Advance(d)
        }
        return nil
    }
}

// TestDeviceFlow_HappyPath exercises the standard poll-once-succeeds path.
func TestDeviceFlow_HappyPath(t *testing.T) {
    var polls int32
    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        switch r.URL.Path {
        case "/device/code":
            json.NewEncoder(w).Encode(map[string]any{
                "device_code":      "dc-123",
                "user_code":        "USER-CODE",
                "verification_uri": "https://example.com/device",
                "expires_in":       900,
                "interval":         5,
            })
        case "/token":
            atomic.AddInt32(&polls, 1)
            json.NewEncoder(w).Encode(map[string]any{
                "access_token": "sk-device-token",
                "token_type":   "bearer",
            })
        }
    }))
    defer srv.Close()

    clk := newFakeClock(1000)
    slp := &fakeSleeper{clock: clk}
    p := GenericRFC8628Provider{
        GenericRFC8628Config: GenericRFC8628Config{
            ClientID:   "test-id",
            DeviceURL:  srv.URL + "/device/code",
            TokenURL:   srv.URL + "/token",
            HTTPClient: srv.Client(),
        },
        PollMin: time.Second,
    }
    tok, err := p.Authorize(context.Background(), clk, slp, func(st StatusUpdate) {})
    if err != nil {
        t.Fatalf("Authorize: %v", err)
    }
    if tok.AccessToken != "sk-device-token" {
        t.Fatalf("token: %q", tok.AccessToken)
    }
    if atomic.LoadInt32(&polls) != 1 {
        t.Fatalf("expected 1 poll, got %d", polls)
    }
}

// TestDeviceFlow_SlowDownBacksOff: first poll returns slow_down → interval
// MUST increase by 5s; subsequent poll succeeds.
func TestDeviceFlow_SlowDownBacksOff(t *testing.T) {
    var polls int32
    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if r.URL.Path == "/token" {
            n := atomic.AddInt32(&polls, 1)
            if n == 1 {
                w.WriteHeader(400)
                json.NewEncoder(w).Encode(map[string]any{"error": "slow_down"})
                return
            }
            json.NewEncoder(w).Encode(map[string]any{"access_token": "sk-ok"})
        } else {
            json.NewEncoder(w).Encode(map[string]any{
                "device_code": "dc", "user_code": "U",
                "verification_uri": "https://example.com/device",
                "expires_in": 900, "interval": 2,
            })
        }
    }))
    defer srv.Close()
    clk := newFakeClock(0)
    slp := &fakeSleeper{clock: clk}
    p := GenericRFC8628Provider{
        GenericRFC8628Config: GenericRFC8628Config{
            ClientID:   "test",
            DeviceURL:  srv.URL + "/device/code",
            TokenURL:   srv.URL + "/token",
            HTTPClient: srv.Client(),
        },
    }
    if _, err := p.Authorize(context.Background(), clk, slp, func(st StatusUpdate) {}); err != nil {
        t.Fatalf("Authorize: %v", err)
    }
    if slp.slept < 7*time.Second { // 2 initial + 5 backoff
        t.Fatalf("slow_down backoff not applied: slept %v", slp.slept)
    }
}

// TestDeviceFlow_ExpiredDeviceCode: poll returns expired_token → Manager
// must return an error (not loop forever) AND must not leak the device_code.
func TestDeviceFlow_ExpiredDeviceCode(t *testing.T) {
    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if r.URL.Path == "/token" {
            w.WriteHeader(400)
            json.NewEncoder(w).Encode(map[string]any{"error": "expired_token"})
            return
        }
        json.NewEncoder(w).Encode(map[string]any{
            "device_code": "dc-SECRET", "user_code": "U",
            "verification_uri": "https://example.com/device",
            "expires_in": 900, "interval": 1,
        })
    }))
    defer srv.Close()
    p := GenericRFC8628Provider{
        GenericRFC8628Config: GenericRFC8628Config{
            ClientID: "test", DeviceURL: srv.URL + "/device/code",
            TokenURL: srv.URL + "/token", HTTPClient: srv.Client(),
        },
    }
    clk := newFakeClock(0)
    _, err := p.Authorize(context.Background(), clk, &fakeSleeper{clock: clk}, func(st StatusUpdate) {})
    if err == nil {
        t.Fatal("expected error on expired_token")
    }
    if strings.Contains(err.Error(), "dc-SECRET") {
        t.Fatalf("device_code leaked in error: %v", err)
    }
}

// TestDeviceFlow_ContextCancel: context cancellation must abort the poll
// loop on the next iteration boundary (no later).
func TestDeviceFlow_ContextCancel(t *testing.T) {
    block := make(chan struct{})
    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if r.URL.Path == "/token" {
            <-block
            return
        }
        json.NewEncoder(w).Encode(map[string]any{
            "device_code": "dc", "user_code": "U",
            "verification_uri": "https://example.com/device",
            "expires_in": 900, "interval": 1,
        })
    }))
    defer srv.Close()
    defer close(block)
    ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
    defer cancel()
    p := GenericRFC8628Provider{
        GenericRFC8628Config: GenericRFC8628Config{
            ClientID: "test", DeviceURL: srv.URL + "/device/code",
            TokenURL: srv.URL + "/token", HTTPClient: srv.Client(),
        },
    }
    clk := newFakeClock(0)
    _, err := p.Authorize(ctx, clk, &fakeSleeper{clock: clk}, func(st StatusUpdate) {})
    if err == nil {
        t.Fatal("expected context error")
    }
}

// TestDeviceFlow_ExpiresInZeroReturnsError: RFC 8628 requires expires_in > 0;
// a 0/negative value must be rejected immediately (not divide-by-zero).
func TestDeviceFlow_ExpiresInZeroReturnsError(t *testing.T) {
    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        json.NewEncoder(w).Encode(map[string]any{
            "device_code": "dc", "user_code": "U", "expires_in": 0, "interval": 1,
        })
    }))
    defer srv.Close()
    p := GenericRFC8628Provider{
        GenericRFC8628Config: GenericRFC8628Config{
            ClientID: "test", DeviceURL: srv.URL + "/device/code",
            TokenURL: srv.URL + "/token", HTTPClient: srv.Client(),
        },
    }
    clk := newFakeClock(0)
    _, err := p.Authorize(context.Background(), clk, &fakeSleeper{clock: clk}, func(st StatusUpdate) {})
    if err == nil {
        t.Fatal("expected error on expires_in=0")
    }
}

// TestDeviceFlow_HTTPSOnlyByDefault: a non-loopback HTTP scheme must be
// rejected at provider construction. Loopback (127.0.0.1, ::1, localhost)
// is allowed for tests.
func TestDeviceFlow_HTTPSOnlyByDefault(t *testing.T) {
    _, err := NewGenericRFC8628Provider(GenericRFC8628Config{
        ClientID:  "x",
        DeviceURL: "http://api.example.com/device/code",
        TokenURL:  "http://api.example.com/token",
    })
    if err == nil {
        t.Fatal("expected error on http:// non-loopback URL")
    }
    // Loopback is allowed.
    p, err := NewGenericRFC8628Provider(GenericRFC8628Config{
        ClientID:  "x",
        DeviceURL: "http://127.0.0.1:9999/device/code",
        TokenURL:  "http://[::1]:9999/token",
    })
    if err != nil {
        t.Fatalf("loopback http must be allowed: %v", err)
    }
    _ = p
}

func TestDeviceFlow_ValidatesLiteralOnEveryAuthorize(t *testing.T) {
    tests := []struct {
        name string
        cfg  GenericRFC8628Config
    }{
        {
            name: "empty-client-id",
            cfg: GenericRFC8628Config{
                DeviceURL: "https://example.com/device",
                TokenURL: "https://example.com/token",
            },
        },
        {
            name: "endpoint-userinfo",
            cfg: GenericRFC8628Config{
                ClientID: "id",
                DeviceURL: "https://user:password@example.com/device",
                TokenURL: "https://example.com/token",
            },
        },
        {
            name: "empty-endpoint-host",
            cfg: GenericRFC8628Config{
                ClientID: "id",
                DeviceURL: "https:///device",
                TokenURL: "https://example.com/token",
            },
        },
    }
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            p := GenericRFC8628Provider{GenericRFC8628Config: tt.cfg}
            _, err := p.Authorize(context.Background(), newFakeClock(0),
                &fakeSleeper{}, nil)
            if err == nil {
                t.Fatal("literal must be revalidated by Authorize")
            }
            for _, forbidden := range []string{
                "user:password", "example.com", "/device", "/token",
            } {
                if strings.Contains(err.Error(), forbidden) {
                    t.Fatalf("validation error leaked endpoint detail %q: %v", forbidden, err)
                }
            }
        })
    }
}

func TestDeviceFlow_RequiresVerificationURI(t *testing.T) {
    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        _ = json.NewEncoder(w).Encode(map[string]any{
            "device_code": "dc", "user_code": "U", "expires_in": 60,
        })
    }))
    defer srv.Close()
    p := GenericRFC8628Provider{GenericRFC8628Config: GenericRFC8628Config{
        ClientID: "id", DeviceURL: srv.URL, TokenURL: srv.URL,
        HTTPClient: srv.Client(),
    }}
    _, err := p.Authorize(context.Background(), newFakeClock(0),
        &fakeSleeper{}, nil)
    if err == nil || err.Error() != "auth: invalid device authorization response" {
        t.Fatalf("want fixed invalid-response error, got %v", err)
    }
}

func TestDeviceFlow_LiteralProviderNormalizesDefaults(t *testing.T) {
    var polls int32
    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if r.URL.Path == "/token" {
            atomic.AddInt32(&polls, 1)
            _ = json.NewEncoder(w).Encode(map[string]any{
                "access_token": "sk-json-tag",
                "token_type": "bearer",
            })
            return
        }
        // interval intentionally omitted. PollMin and Timeout are also zero in
        // the literal below; Authorize must normalize all three at runtime.
        _ = json.NewEncoder(w).Encode(map[string]any{
            "device_code": "dc-defaults",
            "user_code": "U",
            "verification_uri": "https://example.com/device",
            "expires_in": 60,
        })
    }))
    defer srv.Close()

    clk := newFakeClock(0)
    slp := &fakeSleeper{clock: clk}
    p := GenericRFC8628Provider{
        GenericRFC8628Config: GenericRFC8628Config{
            ClientID:   "id",
            DeviceURL:  srv.URL + "/device/code",
            TokenURL:   srv.URL + "/token",
            HTTPClient: srv.Client(),
        },
    }
    tok, err := p.Authorize(context.Background(), clk, slp, nil)
    if err != nil {
        t.Fatal(err)
    }
    if tok.AccessToken != "sk-json-tag" {
        t.Fatalf("access_token JSON tag not decoded: %#v", tok)
    }
    if slp.slept != 5*time.Second || atomic.LoadInt32(&polls) != 1 {
        t.Fatalf("runtime defaults not normalized: slept=%v polls=%d", slp.slept, polls)
    }
}

func TestDeviceFlow_RejectsRedirectAndRegistersRequestSecrets(t *testing.T) {
    const deviceCode = "dc-redaction-sentinel"
    const clientSecret = "client-secret-sentinel"
    r := secrets.NewRedactor()
    var redirected int32
    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
        switch req.URL.Path {
        case "/device/code":
            _ = json.NewEncoder(w).Encode(map[string]any{
                "device_code": deviceCode,
                "user_code": "U",
                "verification_uri": "https://example.com/device",
                "expires_in": 60,
                "interval": 1,
            })
        case "/token":
            http.Redirect(w, req, "/redirected?client_secret="+clientSecret, http.StatusFound)
        case "/redirected":
            atomic.AddInt32(&redirected, 1)
        }
    }))
    defer srv.Close()

    p, err := NewGenericRFC8628Provider(GenericRFC8628Config{
        ClientID:     "id",
        ClientSecret: clientSecret,
        DeviceURL:    srv.URL + "/device/code",
        TokenURL:     srv.URL + "/token",
        HTTPClient:   srv.Client(),
        Redactor:     r,
    })
    if err != nil {
        t.Fatal(err)
    }
    clk := newFakeClock(0)
    _, err = p.Authorize(context.Background(), clk, &fakeSleeper{clock: clk}, nil)
    if err == nil {
        t.Fatal("redirected token endpoint must fail")
    }
    if atomic.LoadInt32(&redirected) != 0 {
        t.Fatal("OAuth client followed redirect")
    }
    for _, sentinel := range []string{deviceCode, clientSecret} {
        if strings.Contains(err.Error(), sentinel) {
            t.Fatalf("error leaked %q: %v", sentinel, err)
        }
        if strings.Contains(r.Redact("value="+sentinel), sentinel) {
            t.Fatalf("redactor did not register %q", sentinel)
        }
    }
}

type fakeCredentialStore struct {
    values      map[string]string
    setCalls    int
    failSetCall int
    failSetErr  error
    failDelete  error
}

func credentialKey(service, account string) string { return service + "\x00" + account }
func (f *fakeCredentialStore) Available() error { return nil }
func (f *fakeCredentialStore) Get(service, account string) (string, error) {
    value, ok := f.values[credentialKey(service, account)]
    if !ok {
        return "", secrets.ErrSecretNotFound
    }
    return value, nil
}
func (f *fakeCredentialStore) Set(service, account, value string) error {
    f.setCalls++
    if f.failSetCall != 0 && f.setCalls == f.failSetCall {
        return f.failSetErr
    }
    f.values[credentialKey(service, account)] = value
    return nil
}
func (f *fakeCredentialStore) Delete(service, account string) error {
    if f.failDelete != nil {
        return f.failDelete
    }
    key := credentialKey(service, account)
    if _, ok := f.values[key]; !ok {
        return secrets.ErrSecretNotFound
    }
    delete(f.values, key)
    return nil
}

type metadataStoreFunc func(string, string, AuthMetadata) error
func (f metadataStoreFunc) SaveAuthMetadata(
    provider, account string,
    meta AuthMetadata,
) error {
    return f(provider, account, meta)
}

func managerWithCredentialStore(t *testing.T, st secrets.Store) *Manager {
    t.Helper()
    sm, err := secrets.NewManager(secrets.Config{Backend: "none"})
    if err != nil {
        t.Fatal(err)
    }
    t.Cleanup(func() { _ = sm.Close() })
    m := NewManager(sm)
    m.store = st
    return m
}

func TestCommitDeviceToken_MetadataFailureCompensates(t *testing.T) {
    metadataErr := errors.New("metadata-write-sentinel")
    for _, hadOld := range []bool{false, true} {
        t.Run(fmt.Sprintf("had-old-%v", hadOld), func(t *testing.T) {
            st := &fakeCredentialStore{values: map[string]string{}}
            if hadOld {
                st.values[credentialKey("provider", "main")] = "old-token"
            }
            m := managerWithCredentialStore(t, st)
            m.SetMetadataStore(metadataStoreFunc(func(
                string, string, AuthMetadata,
            ) error { return metadataErr }))

            err := m.commitDeviceToken("provider", "main", &DeviceToken{
                AccessToken: "new-token", ExpiresIn: 60,
            })
            if !errors.Is(err, metadataErr) {
                t.Fatalf("want metadata error, got %v", err)
            }
            got, getErr := st.Get("provider", "main")
            if hadOld {
                if getErr != nil || got != "old-token" {
                    t.Fatalf("old token not restored: got=%q err=%v", got, getErr)
                }
            } else if !errors.Is(getErr, secrets.ErrSecretNotFound) {
                t.Fatalf("new token not removed: got=%q err=%v", got, getErr)
            }
        })
    }
}

func TestCommitDeviceToken_RollbackFailureIsJoined(t *testing.T) {
    metadataErr := errors.New("metadata-write-sentinel")
    rollbackErr := errors.New("rollback-sentinel")
    tests := []struct {
        name  string
        store *fakeCredentialStore
    }{
        {
            name: "restore-old-token-fails",
            store: &fakeCredentialStore{
                values: map[string]string{
                    credentialKey("provider", "main"): "old-token",
                },
                failSetCall: 2,
                failSetErr: rollbackErr,
            },
        },
        {
            name: "delete-new-token-fails",
            store: &fakeCredentialStore{
                values: map[string]string{},
                failDelete: rollbackErr,
            },
        },
    }
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            m := managerWithCredentialStore(t, tt.store)
            m.SetMetadataStore(metadataStoreFunc(func(
                string, string, AuthMetadata,
            ) error { return metadataErr }))
            err := m.commitDeviceToken("provider", "main", &DeviceToken{
                AccessToken: "new-token", ExpiresIn: 60,
            })
            if !errors.Is(err, metadataErr) || !errors.Is(err, rollbackErr) {
                t.Fatalf("errors.Join must preserve both causes: %v", err)
            }
        })
    }
}

func TestCommitDeviceToken_UsesInjectedClockForExpiry(t *testing.T) {
    st := &fakeCredentialStore{values: map[string]string{}}
    m := managerWithCredentialStore(t, st)
    m.SetClock(newFakeClock(1234))
    var saved AuthMetadata
    m.SetMetadataStore(metadataStoreFunc(func(
        _, _ string, meta AuthMetadata,
    ) error {
        saved = meta
        return nil
    }))
    if err := m.commitDeviceToken("provider", "main", &DeviceToken{
        AccessToken: "access", RefreshToken: "refresh", ExpiresIn: 60,
    }); err != nil {
        t.Fatal(err)
    }
    want := time.Unix(1294, 0)
    if !saved.ExpiresAt.Equal(want) {
        t.Fatalf("ExpiresAt = %v, want injected-clock value %v", saved.ExpiresAt, want)
    }
}
```

### Expected

- `GenericRFC8628Provider.Authorize(ctx, clock, sleeper, progress)` 运行 RFC 8628 完整流程：`POST /device/code` → 间隔轮询 `POST /token` → 成功或失败。
- `slow_down` → 下一次 sleep 增加 5 秒（每 `interval` + 5）；`authorization_pending` → 维持 interval。
- `expired_token` → 立即返回 error，**device_code 不得出现在 error string**（用 fixed "device code expired" 文案）。
- `expires_in <= 0` → 在 `/device/code` 解析时即返回错误，**不**进入轮询循环。
- Context cancel → 下一次轮询前返回（`select { case <-ctx.Done(): ...; case <-time.After(...) }`）。
- `DeviceToken` 对 `access_token` / `refresh_token` / `expires_in` / `token_type` 使用显式 JSON tags；`Authorize` 每次调用都把 `Timeout<=0` 归一到 30 秒、`PollMin<=0` 和 omitted/zero interval 归一到 5 秒，不能依赖 constructor。
- 每次请求复制 caller 的 `http.Client` 并设置拒绝 redirect 的 `CheckRedirect`；transport error、endpoint reject、未知 OAuth `error` 都返回固定文案，不回显 URL、query、body、status code 或 provider error identifier。
- `client_id`、两个 endpoint 的绝对 URL/非空 host/无 userinfo/安全 scheme 在 constructor 与每次 `Authorize` 都校验，防止 exported struct literal 绕过；device response 的 `verification_uri` 必填并接受同一安全 URL 规则。
- `client_secret` 在 provider 构造/Authorize 时登记到共享 redactor，`device_code` 在解析后立即登记；`StatusUpdate` 不承载 error，progress stdout 因而只能输出固定 stage 文本。
- 默认拒绝非 loopback 的 `http://`；`https://` 与 loopback `http://` 允许。
- token 与 metadata 跨 adapter 写入使用补偿事务：metadata 失败时，新 credential 删除、旧 credential 恢复；rollback 失败以 `errors.Join` 同时保留 metadata 与 rollback cause。expiry 唯一使用注入的 `m.clk.Now()`。

### Implementation

`internal/auth/device.go`:

```go
package auth

import (
    "context"
    "encoding/json"
    "errors"
    "io"
    "net"
    "net/http"
    "net/url"
    "strings"
    "time"

    "github.com/x6nux/yanshi/internal/secrets"
)
type Clock interface {
    Now() time.Time
}

// Sleeper abstracts an interruptible sleep so context cancellation wakes the
// poll loop immediately. Tests also use it to advance fakeClock deterministically.
type Sleeper interface {
    Sleep(ctx context.Context, d time.Duration) error
}

// StatusUpdate is delivered to the progress callback during Authorize.
// Stage is one of "device_code_issued", "polling", "slow_down", "success",
// "error". UserCode / VerificationURI are populated at device_code_issued.
type StatusUpdate struct {
    Stage            string
    UserCode         string
    VerificationURI  string
    ExpiresAt        time.Time
}

// DeviceToken is the result of a successful RFC 8628 flow.
type DeviceToken struct {
    AccessToken  string `json:"access_token"`
    RefreshToken string `json:"refresh_token"`
    ExpiresIn    int    `json:"expires_in"`
    TokenType    string `json:"token_type"`
}

// DeviceProvider is the abstraction over provider-specific device flows.
// Implementations: GenericRFC8628Provider (HTTP-only, no SDK), future
// provider SDK adapters (GitHub, Google, etc).
type DeviceProvider interface {
    Authorize(ctx context.Context, clk Clock, slp Sleeper, progress func(StatusUpdate)) (*DeviceToken, error)
}

// GenericRFC8628Config configures a provider that follows RFC 8628 over
// HTTPS. DeviceURL and TokenURL must be HTTPS unless they are loopback
// (127.0.0.1, ::1, localhost).
type GenericRFC8628Config struct {
    ClientID     string
    ClientSecret string // optional; only some providers require
    DeviceURL    string
    TokenURL     string
    Scopes       []string
    HTTPClient   *http.Client // override for testing; copied per Authorize
    Timeout      time.Duration // per-request timeout; <=0 normalizes to 30s
    Redactor     *secrets.Redactor // registers client_secret and device_code
}

type GenericRFC8628Provider struct {
    GenericRFC8628Config
    PollMin time.Duration // minimum poll interval; default 5s
}

func NewGenericRFC8628Provider(cfg GenericRFC8628Config) (*GenericRFC8628Provider, error) {
    if strings.TrimSpace(cfg.ClientID) == "" {
        return nil, errors.New("auth: device client id is required")
    }
    if err := validateEndpoint(cfg.DeviceURL); err != nil {
        return nil, errors.New("auth: invalid device authorization endpoint")
    }
    if err := validateEndpoint(cfg.TokenURL); err != nil {
        return nil, errors.New("auth: invalid token endpoint")
    }
    if cfg.Redactor != nil && cfg.ClientSecret != "" {
        cfg.Redactor.Register(cfg.ClientSecret)
    }
    return &GenericRFC8628Provider{
        GenericRFC8628Config: cfg,
        PollMin:              5 * time.Second,
    }, nil
}

// normalizedRuntime applies security defaults on every Authorize call. Tests
// and callers may construct GenericRFC8628Provider as a literal, so constructor-
// only defaults are insufficient: Timeout=0 would otherwise expire immediately
// and PollMin=0 plus an omitted interval would spin without sleeping.
func (p *GenericRFC8628Provider) normalizedRuntime() (*http.Client, time.Duration, time.Duration) {
    timeout := p.Timeout
    if timeout <= 0 {
        timeout = 30 * time.Second
    }
    pollMin := p.PollMin
    if pollMin <= 0 {
        pollMin = 5 * time.Second
    }
    base := p.HTTPClient
    if base == nil {
        base = http.DefaultClient
    }
    client := *base
    client.CheckRedirect = func(*http.Request, []*http.Request) error {
        return http.ErrUseLastResponse
    }
    return &client, timeout, pollMin
}

func validateEndpoint(rawURL string) error {
    u, err := url.Parse(rawURL)
    if err != nil || u == nil || u.Host == "" || u.Hostname() == "" {
        return errors.New("invalid absolute URL")
    }
    if u.User != nil || u.Fragment != "" {
        return errors.New("userinfo and fragments are forbidden")
    }
    if u.Scheme == "https" {
        return nil
    }
    if u.Scheme != "http" {
        return errors.New("scheme not allowed")
    }
    host := u.Hostname()
    if host == "127.0.0.1" || host == "::1" ||
        strings.EqualFold(host, "localhost") {
        return nil
    }
    if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
        return nil
    }
    return errors.New("plain HTTP is allowed only on loopback")
}

func (p *GenericRFC8628Provider) Authorize(ctx context.Context, clk Clock, slp Sleeper, progress func(StatusUpdate)) (*DeviceToken, error) {
    // GenericRFC8628Provider is exported and may be built as a struct literal;
    // repeat all security validation on every call rather than trusting that the
    // constructor ran.
    if p == nil || strings.TrimSpace(p.ClientID) == "" {
        return nil, errors.New("auth: device client id is required")
    }
    if err := validateEndpoint(p.DeviceURL); err != nil {
        return nil, errors.New("auth: invalid device authorization endpoint")
    }
    if err := validateEndpoint(p.TokenURL); err != nil {
        return nil, errors.New("auth: invalid token endpoint")
    }
    client, timeout, pollMin := p.normalizedRuntime()
    effective := *p
    effective.HTTPClient = client
    effective.Timeout = timeout
    effective.PollMin = pollMin
    p = &effective
    if p.Redactor != nil && p.ClientSecret != "" {
        p.Redactor.Register(p.ClientSecret)
    }
    if clk == nil {
        clk = realClock{}
    }
    if slp == nil {
        slp = realSleeper{}
    }
    if progress == nil {
        progress = func(StatusUpdate) {}
    }

    // Step 1: POST /device/code
    deviceCode, userCode, verificationURI, expiresIn, interval, err := p.requestDeviceCode(ctx)
    if err != nil {
        progress(StatusUpdate{Stage: "error"})
        return nil, err
    }
    if p.Redactor != nil {
        p.Redactor.Register(deviceCode)
    }
    if expiresIn <= 0 {
        err := errors.New("auth: device authorization server returned expires_in <= 0")
        progress(StatusUpdate{Stage: "error"})
        return nil, err
    }
    expiresAt := clk.Now().Add(time.Duration(expiresIn) * time.Second)
    progress(StatusUpdate{
        Stage:           "device_code_issued",
        UserCode:        userCode,
        VerificationURI: verificationURI,
        ExpiresAt:       expiresAt,
    })

    // Step 2: poll /token until success, expired, or context cancel.
    currentInterval := time.Duration(interval) * time.Second
    if currentInterval < p.PollMin {
        currentInterval = p.PollMin
    }
    for {
        if clk.Now().After(expiresAt) {
            err := errors.New("auth: device code expired")
            progress(StatusUpdate{Stage: "error"})
            return nil, err
        }
        // Wait for either the sleep to finish or the context to cancel.
        select {
        case <-ctx.Done():
            err := ctx.Err()
            progress(StatusUpdate{Stage: "error"})
            return nil, err
        default:
        }
        if err := slp.Sleep(ctx, currentInterval); err != nil {
            progress(StatusUpdate{Stage: "error"})
            return nil, err
        }
        // fakeSleeper advances fakeClock here; realSleeper observes wall time.
        if !clk.Now().Before(expiresAt) {
            err := errors.New("auth: device code expired")
            progress(StatusUpdate{Stage: "error"})
            return nil, err
        }

        progress(StatusUpdate{Stage: "polling", ExpiresAt: expiresAt})
        tok, errCode, err := p.pollToken(ctx, deviceCode)
        if err != nil {
            // Network error: surface but keep polling (within expiry).
            continue
        }
        switch errCode {
        case "":
            progress(StatusUpdate{Stage: "success"})
            return tok, nil
        case "authorization_pending":
            // keep polling at current interval
            continue
        case "slow_down":
            currentInterval += 5 * time.Second
            progress(StatusUpdate{Stage: "slow_down", ExpiresAt: expiresAt})
            continue
        case "expired_token":
            // Do NOT include device_code in error text.
            err := errors.New("auth: device code expired")
            progress(StatusUpdate{Stage: "error"})
            return nil, err
        case "access_denied":
            err := errors.New("auth: user denied device authorization")
            progress(StatusUpdate{Stage: "error"})
            return nil, err
        default:
            // OAuth error identifiers are untrusted provider input. Do not
            // reflect the identifier, response body, endpoint, or status code.
            err := errors.New("auth: device authorization failed")
            progress(StatusUpdate{Stage: "error"})
            return nil, err
        }
    }
}

const maxOAuthResponseBytes = 1 << 20

func readOAuthResponse(body io.Reader) ([]byte, error) {
    raw, err := io.ReadAll(io.LimitReader(body, maxOAuthResponseBytes+1))
    if err != nil {
        return nil, err
    }
    if len(raw) > maxOAuthResponseBytes {
        return nil, errors.New("oauth response exceeds limit")
    }
    return raw, nil
}

func (p *GenericRFC8628Provider) requestDeviceCode(ctx context.Context) (deviceCode, userCode, verificationURI string, expiresIn, interval int, err error) {
    body := url.Values{"client_id": {p.ClientID}}
    if len(p.Scopes) > 0 {
        body.Set("scope", strings.Join(p.Scopes, " "))
    }
    reqCtx, cancel := context.WithTimeout(ctx, p.Timeout)
    defer cancel()
    req, err := http.NewRequestWithContext(reqCtx, "POST", p.DeviceURL, strings.NewReader(body.Encode()))
    if err != nil {
        return "", "", "", 0, 0, errors.New("auth: cannot build device authorization request")
    }
    req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
    resp, err := p.HTTPClient.Do(req)
    if err != nil {
        // Transport errors can embed the request URL (including query params),
        // so this boundary deliberately does not wrap or stringify err.
        return "", "", "", 0, 0, errors.New("auth: device authorization request failed")
    }
    defer resp.Body.Close()
    if resp.StatusCode < 200 || resp.StatusCode >= 300 {
        return "", "", "", 0, 0, errors.New("auth: device authorization endpoint rejected request")
    }
    raw, readErr := readOAuthResponse(resp.Body)
    if readErr != nil {
        return "", "", "", 0, 0,
            errors.New("auth: invalid device authorization response")
    }
    var payload struct {
        DeviceCode      string `json:"device_code"`
        UserCode        string `json:"user_code"`
        VerificationURI string `json:"verification_uri"`
        ExpiresIn       int    `json:"expires_in"`
        Interval        int    `json:"interval"`
    }
    if err := json.Unmarshal(raw, &payload); err != nil {
        return "", "", "", 0, 0,
            errors.New("auth: invalid device authorization response")
    }
    if payload.DeviceCode == "" || payload.UserCode == "" ||
        payload.ExpiresIn <= 0 || payload.VerificationURI == "" ||
        validateEndpoint(payload.VerificationURI) != nil {
        return "", "", "", 0, 0,
            errors.New("auth: invalid device authorization response")
    }
    return payload.DeviceCode, payload.UserCode, payload.VerificationURI, payload.ExpiresIn, payload.Interval, nil
}

func (p *GenericRFC8628Provider) pollToken(ctx context.Context, deviceCode string) (*DeviceToken, string, error) {
    body := url.Values{
        "client_id":   {p.ClientID},
        "device_code": {deviceCode},
        "grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
    }
    if p.ClientSecret != "" {
        body.Set("client_secret", p.ClientSecret)
    }
    reqCtx, cancel := context.WithTimeout(ctx, p.Timeout)
    defer cancel()
    req, err := http.NewRequestWithContext(reqCtx, "POST", p.TokenURL, strings.NewReader(body.Encode()))
    if err != nil {
        return nil, "", errors.New("auth: cannot build token request")
    }
    req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
    resp, err := p.HTTPClient.Do(req)
    if err != nil {
        return nil, "", errors.New("auth: token request failed")
    }
    defer resp.Body.Close()
    raw, err := readOAuthResponse(resp.Body)
    if err != nil {
        return nil, "", errors.New("auth: cannot read token response")
    }
    if resp.StatusCode == http.StatusOK {
        var tok DeviceToken
        if err := json.Unmarshal(raw, &tok); err != nil {
            return nil, "", errors.New("auth: invalid token response")
        }
        if tok.AccessToken == "" {
            return nil, "", errors.New("auth: token endpoint returned empty access_token")
        }
        return &tok, "", nil
    }
    var errResp struct {
        Error string `json:"error"`
    }
    if err := json.Unmarshal(raw, &errResp); err != nil || errResp.Error == "" {
        return nil, "", errors.New("auth: token endpoint rejected request")
    }
    switch errResp.Error {
    case "authorization_pending", "slow_down", "expired_token", "access_denied":
        return nil, errResp.Error, nil
    default:
        // The caller maps this sentinel to one fixed error string.
        return nil, "unknown_oauth_error", nil
    }
}

type realClock struct{}
func (realClock) Now() time.Time { return time.Now() }

type realSleeper struct{}
func (realSleeper) Sleep(ctx context.Context, d time.Duration) error {
    timer := time.NewTimer(d)
    defer timer.Stop()
    select {
    case <-ctx.Done():
        return ctx.Err()
    case <-timer.C:
        return nil
    }
}
```

`internal/auth/auth.go` 在 Task 7 的增量（与本 Task 同一 commit；此时
`DeviceProvider` / `Clock` / `Sleeper` 已由上面的 `device.go` 定义）：

```go
// AuthMetadata contains lifecycle data safe for SQLite. It must never grow
// access-token, refresh-token, device-code, user-code, or client-secret fields.
type AuthMetadata struct {
    Source    string
    ExpiresAt time.Time
}

// MetadataStore is implemented by internal/store/auth.go. SaveAuthMetadata
// must be a single SQLite upsert so a returned error leaves the previous row
// intact.
type MetadataStore interface {
    SaveAuthMetadata(provider, account string, meta AuthMetadata) error
}

// Replace Manager with the Task 7-complete shape.
type Manager struct {
    secrets         *secrets.Manager
    store           secrets.Store
    deviceProviders map[string]DeviceProvider
    clk             Clock
    slp             Sleeper
    metadata        MetadataStore
}

func NewManager(sm *secrets.Manager) *Manager {
    var credentialStore secrets.Store
    if sm != nil {
        credentialStore = sm.Store()
    }
    return &Manager{
        secrets:         sm,
        store:           credentialStore,
        deviceProviders: make(map[string]DeviceProvider),
        clk:             realClock{},
        slp:             realSleeper{},
    }
}

func (m *Manager) SetClock(c Clock) {
    if c != nil {
        m.clk = c
    }
}

func (m *Manager) SetSleeper(s Sleeper) {
    if s != nil {
        m.slp = s
    }
}

func (m *Manager) SetMetadataStore(store MetadataStore) {
    m.metadata = store
}

func (m *Manager) RegisterDeviceProvider(id string, dp DeviceProvider) {
    m.deviceProviders[id] = dp
}

// commitDeviceToken is a compensating transaction across the secret backend
// and SQLite metadata. It snapshots an existing credential before replacement:
// a metadata failure restores that old token; only a newly-created credential
// is deleted. Compensation failures are joined so callers never lose evidence
// of a partial rollback.
func (m *Manager) commitDeviceToken(
    provider, account string,
    tok *DeviceToken,
) error {
    if tok == nil || tok.AccessToken == "" {
        return errors.New("auth: empty device token refused")
    }
    if m.metadata == nil {
        return errors.New("auth: metadata store is not configured")
    }
    if m.clk == nil {
        return errors.New("auth: clock is not configured")
    }
    credentialStore := m.store
    if credentialStore == nil {
        return errors.New("auth: secret store is not configured")
    }

    oldToken, snapshotErr := credentialStore.Get(provider, account)
    hadOldToken := snapshotErr == nil
    if snapshotErr != nil && !errors.Is(snapshotErr, secrets.ErrSecretNotFound) {
        return fmt.Errorf("auth: snapshot existing device token: %w", snapshotErr)
    }

    m.secrets.Redactor().Register(tok.AccessToken)
    if tok.RefreshToken != "" {
        m.secrets.Redactor().Register(tok.RefreshToken)
    }
    if err := credentialStore.Set(provider, account, tok.AccessToken); err != nil {
        return fmt.Errorf("auth: persist device token: %w", err)
    }

    expiresAt := time.Time{}
    if tok.ExpiresIn > 0 {
        expiresAt = m.clk.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
    }
    metadataErr := m.metadata.SaveAuthMetadata(
        provider,
        account,
        AuthMetadata{Source: "device", ExpiresAt: expiresAt},
    )
    if metadataErr == nil {
        return nil
    }

    var rollbackErr error
    if hadOldToken {
        rollbackErr = credentialStore.Set(provider, account, oldToken)
    } else {
        rollbackErr = credentialStore.Delete(provider, account)
    }
    base := fmt.Errorf("auth: persist device metadata: %w", metadataErr)
    if rollbackErr != nil {
        return errors.Join(base, fmt.Errorf("auth: compensate device token: %w", rollbackErr))
    }
    return base
}
```

### Pass

```sh
go test ./internal/auth -run "Test(DeviceFlow_|CommitDeviceToken_)"
```

### Commit

```
feat(auth): RFC 8628 device flow with Clock/Sleeper + HTTPS-only (O03-L2)

GenericRFC8628Provider implements the full device authorization code flow
with deterministic timing (Clock/Sleeper injection). Covers slow_down
backoff, expired_token (no device_code leak), context cancellation,
expires_in<=0 early reject, and HTTPS-only enforcement with loopback
exception for tests. The redactor at the boundary covers error text.
```

---

## Task 8 — O03-L3: bootstrap 接 auth.Manager 在 BuildProviders 之前

**结构性修复落点:** #1 (O03 凭据必须到达 BuildProviders — 装配顺序的最终落地)、#2 (AuthDeps.Providers 注入到 bootstrap + buildDeviceProviders 候选集统一校验 + httptest injection test)。

### Files

- Modify `internal/store/store.go` (`schema` 增加 `auth_metadata`)
- Create `internal/store/auth.go`（`authSQLiteAdapter` + `AuthMetadataFromDB`）
- Create `internal/store/auth_test.go`（upsert + no-secret SQLite assertions）
- Modify `internal/bootstrap/bootstrap.go`
- Modify `internal/bootstrap/bootstrap_test.go`

### Failure Test (RED)

`internal/bootstrap/bootstrap_test.go` — `TestBuild_AuthManagerResolvesCredentialsBeforeProviders`:

```go
// TestBuild_AuthManagerResolvesCredentialsBeforeProviders verifies the strict
// ordering: auth.Manager must be constructed, credential sources resolved,
// and resolved keys written back to cfg BEFORE einollm.BuildProviders is
// called. We assert this by seeding a provider with secret:// ref + a
// pre-populated secret store, then checking the Build result's providers
// have the resolved key.
func TestBuild_AuthManagerResolvesCredentialsBeforeProviders(t *testing.T) {
    dir := t.TempDir()
    secretPath := filepath.Join(dir, "secrets.enc")
    t.Setenv("YANSHI_PASSPHRASE", "test-pass")
    // Pre-populate the secret store with the credential.
    smgr, err := secrets.NewManager(secrets.Config{
        Backend: "file", FilePath: secretPath, PassphraseEnv: "YANSHI_PASSPHRASE",
    })
    if err != nil { t.Fatal(err) }
    if err := smgr.Store().Set("openai", "main", "sk-secret-store-value"); err != nil {
        t.Fatal(err)
    }
    if err := smgr.Close(); err != nil {
        t.Fatal(err)
    }

    cfg := &config.Config{
        LLM: config.LLMConfig{
            Providers: []config.ProviderConfig{
                {Name: "p1", Kind: "openai", Model: "gpt-fake", APIKey: "secret://openai/main"},
            },
        },
        Secrets: config.SecretsConfig{Backend: "file", FilePath: secretPath, PassphraseEnv: "YANSHI_PASSPHRASE"},
        Storage: config.StorageConfig{SQLitePath: filepath.Join(dir, "yanshi.db")},
    }

    var builderCalls int
    recordingBuilder := func(got *config.Config) (
        map[string]model.BaseChatModel,
        []model.BaseChatModel,
        map[string]int,
        error,
    ) {
        builderCalls++
        if got.LLM.Providers[0].APIKey != "sk-secret-store-value" {
            t.Fatalf("provider builder observed unresolved key %q", got.LLM.Providers[0].APIKey)
        }
        fm := einollm.NewFakeModelWithMessages([]*schema.Message{
            schema.AssistantMessage("ok", nil),
        }, nil)
        fm.Repeat = true
        return map[string]model.BaseChatModel{"gpt-fake": fm},
            []model.BaseChatModel{fm},
            map[string]int{"gpt-fake": 128000},
            nil
    }

    app, err := Build(Options{
        Cfg:             cfg,
        FakeModel:       false,
        ProviderBuilder: recordingBuilder,
    })
    if err != nil {
        t.Fatalf("Build: %v", err)
    }
    defer app.Shutdown(context.Background())
    if builderCalls != 1 {
        t.Fatalf("provider builder calls = %d, want 1", builderCalls)
    }
    // cfg.LLM.Providers[0].APIKey must now hold the resolved plaintext.
    if cfg.LLM.Providers[0].APIKey != "sk-secret-store-value" {
        t.Fatalf("APIKey not resolved before BuildProviders: %q",
            cfg.LLM.Providers[0].APIKey)
    }
    // The redactor must have it registered.
    if strings.Contains(app.Redactor.Redact("error sk-secret-store-value"), "sk-secret-store-value") {
        t.Fatal("redactor missing the resolved secret")
    }
    // The auth Manager must be exposed on App for CLI/doctor.
    if app.Auth == nil {
        t.Fatal("App.Auth nil")
    }
    st, _ := app.Auth.Status("openai", "main")
    if !st.Authenticated {
        t.Fatal("auth.Status reports unauthenticated after Build")
    }
}

// TestBuild_DeviceProviderInjection (structural fix #2) covers both sources:
//   (a) cfg-driven providers get NewGenericRFC8628Provider validation, and a
//       non-HTTPS non-loopback URL aborts Build;
//   (b) AuthDeps.Providers override cfg entirely (replacement, not merge),
//       so tests can inject an httptest endpoint without it being re-
//       validated through NewGenericRFC8628Provider;
//   (c) duplicate / empty IDs are rejected whether from cfg or injection.
func TestBuild_DeviceProviderInjection(t *testing.T) {
    dir := t.TempDir()
    base := &config.Config{
        Secrets: config.SecretsConfig{Backend: "none"},
        Storage: config.StorageConfig{SQLitePath: filepath.Join(dir, "yanshi.db")},
    }

    // (a) bad cfg endpoint fails-closed.
    bad := *base
    bad.Auth.Device.DeviceAuthEnabled = true
    bad.Auth.Device.Providers = []config.DeviceProviderConfig{{
        ID: "p1", DeviceURL: "http://api.example.com/d", TokenURL: "https://api.example.com/t",
    }}
    if _, err := Build(Options{Cfg: &bad, FakeModel: true}); err == nil {
        t.Fatal("Build must reject non-HTTPS non-loopback device_url")
    }

    // (b) AuthDeps.Providers replaces cfg and skips NewGenericRFC8628Provider
    // validation — the test injects a stub that records calls, asserting the
    // httptest URL reaches the provider verbatim.
    good := *base
    good.Auth.Device.Providers = []config.DeviceProviderConfig{{
        ID: "ignored", DeviceURL: "http://example.invalid", TokenURL: "http://example.invalid",
    }}
    stub := &recordingDeviceProvider{}
    clock := &recordingClock{now: time.Unix(123, 0)}
    sleeper := &recordingSleeper{}
    app, err := Build(Options{
        Cfg: &good,
        FakeModel: true,
        AuthDeps: AuthDeps{
            Providers: []DeviceProviderBinding{
                {ID: "test-only", Provider: stub},
            },
            Clock:   clock,
            Sleeper: sleeper,
        },
    })
    if err != nil {
        t.Fatalf("Build with injected provider: %v", err)
    }
    defer app.Shutdown(context.Background())
    _, _ = app.Auth.RunDeviceFlow(
        context.Background(), "test-only", "main", io.Discard,
    )
    if stub.clockSeen != clock || stub.sleeperSeen != sleeper {
        t.Fatalf("device flow did not receive bootstrap Clock/Sleeper")
    }

    // (c) duplicate IDs in injection fail.
    dup := *base
    _, err = Build(Options{
        Cfg: &dup, FakeModel: true,
        AuthDeps: AuthDeps{Providers: []DeviceProviderBinding{
            {ID: "x", Provider: stub},
            {ID: "x", Provider: stub},
        }},
    })
    if err == nil {
        t.Fatal("Build must reject duplicate injected provider IDs")
    }
    if !strings.Contains(err.Error(), `"x"`) {
        t.Fatalf("duplicate error must name the id: %v", err)
    }
}

type recordingClock struct{ now time.Time }
func (c *recordingClock) Now() time.Time { return c.now }

type recordingSleeper struct{}
func (*recordingSleeper) Sleep(context.Context, time.Duration) error { return nil }

// recordingDeviceProvider is a Task 8 test double. It implements
// auth.DeviceProvider by recording the Authorize call's Clock/Sleeper pair
// so the bootstrap test can prove they were wired through. It returns a
// fixed error so no fake HTTP server is needed.
type recordingDeviceProvider struct {
    clockSeen   auth.Clock
    sleeperSeen auth.Sleeper
}

func (r *recordingDeviceProvider) Authorize(
    ctx context.Context,
    clk auth.Clock,
    slp auth.Sleeper,
    progress func(auth.StatusUpdate),
) (*auth.DeviceToken, error) {
    r.clockSeen = clk
    r.sleeperSeen = slp
    return nil, errors.New("recordingDeviceProvider: intentional failure")
}
```

`internal/store/auth_test.go` — adapter 与 schema 白名单：

```go
package store

import (
    "reflect"
    "testing"
    "time"

    "github.com/x6nux/yanshi/internal/auth"
)

func TestAuthSQLiteAdapterUpsertsOnlyNonSecretMetadata(t *testing.T) {
    st, err := Open(":memory:")
    if err != nil {
        t.Fatal(err)
    }
    defer st.Close()

    adapter := AuthMetadataFromDB(st.DB)
    first := time.Unix(1000, 0).UTC()
    second := time.Unix(2000, 0).UTC()
    if err := adapter.SaveAuthMetadata("openai", "main", auth.AuthMetadata{
        Source: "device", ExpiresAt: first,
    }); err != nil {
        t.Fatal(err)
    }
    if err := adapter.SaveAuthMetadata("openai", "main", auth.AuthMetadata{
        Source: "device", ExpiresAt: second,
    }); err != nil {
        t.Fatal(err)
    }

    var source string
    var expiresAt int64
    if err := st.DB.QueryRow(
        `SELECT source, expires_at FROM auth_metadata
         WHERE provider = ? AND account = ?`,
        "openai", "main",
    ).Scan(&source, &expiresAt); err != nil {
        t.Fatal(err)
    }
    if source != "device" || expiresAt != second.Unix() {
        t.Fatalf("metadata = (%q,%d), want (device,%d)", source, expiresAt, second.Unix())
    }

    columns, err := st.columns("auth_metadata")
    if err != nil {
        t.Fatal(err)
    }
    want := []string{"provider", "account", "source", "expires_at", "updated_at"}
    if !reflect.DeepEqual(columns, want) {
        t.Fatalf("auth_metadata columns = %v, want %v; secret columns are forbidden", columns, want)
    }
}
```

### Expected

- 在 Task 5 修改的基础上，bootstrap 进一步用 `auth.NewManager(secretMgr)` 构造 `auth.Manager`，并将其暴露到 `App.Auth`。
- Build 顺序：`config → store → secrets.Manager → auth.Manager + AuthMetadataFromDB(st.DB) → resolve credentials → write back cfg → BuildProviders → orchestrator → http server → task broker`。
- `internal/store/store.go` 的真实 `schema` 字符串创建 `auth_metadata(provider, account, source, expires_at, updated_at)`；没有 token/device-code/client-secret 列。`AuthMetadataFromDB(st.DB)` 返回 `authSQLiteAdapter`，bootstrap 立即通过 `authMgr.SetMetadataStore(...)` 注入。
- `auth.Manager` 暴露给 `App` 与 CLI/doctor；不新增聊天控制帧或未注册的 HTTP device endpoint。

### Implementation

`internal/store/store.go` 在现有 `const schema` 的 `kv` 表之后追加真实 migration DDL（`Open` 已统一执行整个 schema）：

```sql
CREATE TABLE IF NOT EXISTS auth_metadata (
    provider   TEXT NOT NULL,
    account    TEXT NOT NULL,
    source     TEXT NOT NULL,
    expires_at INTEGER NOT NULL DEFAULT 0,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (provider, account)
);
```

`internal/store/auth.go`：

```go
package store

import (
    "database/sql"
    "time"

    "github.com/x6nux/yanshi/internal/auth"
)

// authSQLiteAdapter is the outbound SQLite adapter for auth.MetadataStore.
// It persists only lifecycle metadata; its schema intentionally has no secret
// columns. Keeping the type private prevents callers from depending on SQLite.
type authSQLiteAdapter struct {
    db *sql.DB
}

// AuthMetadataFromDB follows the composition-root adapter style: bootstrap
// supplies the already-migrated Store.DB and receives the inward auth port.
func AuthMetadataFromDB(db *sql.DB) auth.MetadataStore {
    return &authSQLiteAdapter{db: db}
}

func (a *authSQLiteAdapter) SaveAuthMetadata(
    provider, account string,
    meta auth.AuthMetadata,
) error {
    expiresAt := int64(0)
    if !meta.ExpiresAt.IsZero() {
        expiresAt = meta.ExpiresAt.Unix()
    }
    _, err := a.db.Exec(
        `INSERT INTO auth_metadata
            (provider, account, source, expires_at, updated_at)
         VALUES (?, ?, ?, ?, ?)
         ON CONFLICT(provider, account) DO UPDATE SET
            source = excluded.source,
            expires_at = excluded.expires_at,
            updated_at = excluded.updated_at`,
        provider,
        account,
        meta.Source,
        expiresAt,
        time.Now().Unix(),
    )
    return err
}
```

`internal/bootstrap/bootstrap.go` 改动 (在 Task 5 secrets 装配段之后)。`App.Redactor`、`Options.Cfg` 与 conditional config load 已由 Task 5 添加；Task 8 只增加以下类型与字段：

```go
// Add to App:
Auth *auth.Manager

// ProviderBuilder is the production BuildProviders signature. Tests inject a
// recording implementation to prove it sees resolved credentials; nil uses
// einollm.BuildProviders.
type ProviderBuilder func(*config.Config) (
    map[string]model.BaseChatModel,
    []model.BaseChatModel,
    map[string]int,
    error,
)

// Add to Options (Cfg already exists from Task 5):
// ProviderBuilder is the ordering seam; nil normalizes to
// einollm.BuildProviders before the model selection branch.
ProviderBuilder ProviderBuilder

// AuthDeps lets tests inject RFC 8628 providers, a fake Clock, and a fake
// Sleeper so device-flow timing is deterministic without real network I/O.
// Production callers leave it zero — the loop below builds providers from
// cfg.Auth.Device.Providers and uses real Clock/Sleeper.
AuthDeps AuthDeps

// AuthDeps is the injection seam for auth-side collaborators.
type AuthDeps struct {
    // Providers overrides the cfg-derived device providers. When non-empty,
    // bootstrap skips the cfg.Auth.Device.Providers loop and uses exactly
    // these (already-validated) providers — so tests can supply an httptest
    // endpoint without parsing YAML. IDs still must be unique; bootstrap
    // verifies that here rather than trusting the caller.
    Providers []DeviceProviderBinding

    // Clock / Sleeper are passed to auth.Manager so device flow tests can
    // drive timing deterministically. nil => realClock / realSleeper.
    Clock   auth.Clock
    Sleeper auth.Sleeper
}

// DeviceProviderBinding pairs an ID with an auth.DeviceProvider. The ID is
// the value users put under cfg.Auth.Device.Providers[i].id (and the key
// under which the provider is registered for ResolveAPIKey fallback).
type DeviceProviderBinding struct {
    ID       string
    Provider auth.DeviceProvider
}
```

随后插入 auth.Manager 接管段：

```go
// Continue Build: construct auth.Manager (O03) and resolve credential sources
// BEFORE calling einollm.BuildProviders. This ordering is load-bearing:
// BuildProviders reads cfg.LLM.Providers[i].APIKey and passes it directly
// to provider SDK constructors; if we resolve after, those SDKs get refs
// instead of plaintext and every API call fails with 401.
//
// NOTE: this block supersedes the inline resolver loop in Task 5. The
// executing-plans agent lands Task 5 first with its simpler loop (no device
// providers), then Task 8 REPLACES that loop with the version below so the
// auth.Manager owns all credential resolution.
authMgr := auth.NewManager(secretMgr)
authMgr.SetMetadataStore(store.AuthMetadataFromDB(st.DB))
if opts.AuthDeps.Clock != nil {
    authMgr.SetClock(opts.AuthDeps.Clock)
}
if opts.AuthDeps.Sleeper != nil {
    authMgr.SetSleeper(opts.AuthDeps.Sleeper)
}

// Build and validate the complete provider candidate set before registering
// any one provider. Configured providers are inert unless DeviceAuthEnabled is
// true. Non-empty injected providers are an explicit test override and remain
// active even when the config flag is false.
var bindings []DeviceProviderBinding
if cfg.Auth.Device.DeviceAuthEnabled || len(opts.AuthDeps.Providers) > 0 {
    bindings, err = buildDeviceProviders(cfg.Auth.Device.Providers, opts.AuthDeps)
    if err != nil {
        return nil, err
    }
}
for _, b := range bindings {
    authMgr.RegisterDeviceProvider(b.ID, b.Provider)
}

// Resolve all provider credential sources. Raw literals fail-closed.
for i := range cfg.LLM.Providers {
    p := &cfg.LLM.Providers[i]
    if p.APIKey == "" {
        continue
    }
    src := auth.CredentialSource{
        APIKeyRef:      p.APIKey,
        LegacyInsecure: cfg.Auth.LegacyInsecure,
    }
    resolved, err := authMgr.ResolveAPIKey(context.Background(), src)
    if err != nil {
        return nil, fmt.Errorf("bootstrap: resolve credentials for %s: %w", p.Name, err)
    }
    if resolved != "" {
        redactor.Register(resolved)
        p.APIKey = resolved
    }
}

// Do not assign through a nonexistent local `app`; real Build constructs App
// only in its final return literal. Preserve authMgr until that literal and
// add:
//
//     Auth: authMgr,
//
// Redactor: redactor is already present from Task 5.
// Replace the real model branch's direct einollm.BuildProviders(cfg) call with:
providerBuilder := opts.ProviderBuilder
if providerBuilder == nil {
    providerBuilder = einollm.BuildProviders
}
// In the existing non-fake branch:
named, chain, windows, err := providerBuilder(cfg)
// Keep the existing error handling, NewResilientModel, and assignments to
// chatModel/providerModels/providerWindows unchanged.
```

真实 `Build` 的最终 `return &App{...}` literal 增加以下字段；不得在此前写
`app.Auth = ...`（仓库中此时没有 `app` 变量）：

```go
return &App{
    // all existing fields, including Task 5's Redactor, remain unchanged
    Auth: authMgr,
    // existing cancel field remains last
}, nil
```

同一文件增加完整候选集 builder（这是结构性修复 #2 的明确 API）：

```go
// buildDeviceProviders returns a fully validated candidate set. It performs
// ALL validation before Build registers anything, so a duplicate/empty ID or
// invalid URL cannot leave auth.Manager partially configured.
//
// Injection rule: when deps.Providers is non-empty it replaces (not merges
// with) configProviders. This makes tests deterministic and avoids ambiguous
// duplicate precedence. Production leaves deps empty and uses config only.
func buildDeviceProviders(
    configProviders []config.DeviceProviderConfig,
    defaultClientID string,
    redactor *secrets.Redactor,
    deps AuthDeps,
) ([]DeviceProviderBinding, error) {
    candidates := append([]DeviceProviderBinding(nil), deps.Providers...)
    if len(candidates) == 0 {
        for _, dp := range configProviders {
            clientID := strings.TrimSpace(dp.ClientID)
            if clientID == "" {
                clientID = strings.TrimSpace(defaultClientID)
            }
            provider, err := auth.NewGenericRFC8628Provider(auth.GenericRFC8628Config{
                ClientID:  clientID,
                DeviceURL: dp.DeviceURL,
                TokenURL:  dp.TokenURL,
                Scopes:    append([]string(nil), dp.Scopes...),
                Redactor:  redactor,
            })
            if err != nil {
                return nil, fmt.Errorf("bootstrap: device provider %q: %w", dp.ID, err)
            }
            candidates = append(candidates, DeviceProviderBinding{
                ID:       dp.ID,
                Provider: provider,
            })
        }
    }

    seen := make(map[string]bool, len(candidates))
    for _, c := range candidates {
        if strings.TrimSpace(c.ID) == "" {
            return nil, fmt.Errorf("bootstrap: device provider id must not be empty")
        }
        if c.Provider == nil {
            return nil, fmt.Errorf("bootstrap: device provider %q is nil", c.ID)
        }
        if seen[c.ID] {
            return nil, fmt.Errorf("bootstrap: duplicate device provider id %q", c.ID)
        }
        seen[c.ID] = true
    }
    return candidates, nil
}
```

`RegisterDeviceProvider`、`SetClock` 与 `SetSleeper` 已由 Task 7 添加；Task 8
只负责组合根注入，不重复定义 auth 包 API。

### Pass

```sh
go test ./internal/store -run TestAuthSQLiteAdapterUpsertsOnlyNonSecretMetadata
go test ./internal/bootstrap -run "TestBuild_(AuthManagerResolvesCredentialsBeforeProviders|DeviceProviderInjection)"
```

### Commit

```
feat(auth): bootstrap wires auth.Manager before BuildProviders (O03-L3)

Strict ordering: secrets.Manager -> auth.Manager -> resolve credentials ->
write back cfg.LLM.Providers[i].APIKey -> ProviderBuilder. The recording seam
proves the provider builder observes plaintext. Raw literals fail closed.
auth.Manager receives AuthMetadataFromDB(st.DB), is exposed on App, and only
registers configured device providers when DeviceAuthEnabled is true.
```

---

## Task 9 — O03-L4: CLI `yanshi auth` 完整接线 + TestMainAuth E2E

**结构性修复落点:** #2 (httptest CLI 验收)、#13 (TestMainAuth 完整代码 + --api-key-stdin 真实解析 + cleanup 失败路径)。

### Files

- Modify `cmd/yanshi/main.go` (production `runCLI` 先解析 leading global `--config`，再接 `auth`/真实 `runDoctor`；仅 `main` 调 `os.Exit`)
- Modify `cmd/yanshi/main_test.go` (`TestMainAuth` fileStore E2E + loopback RFC 8628 E2E)
- Create `internal/auth/cli.go` (`RunDeviceFlow`：固定 progress、commit token + metadata)
- Reuse `internal/store/auth.go` (`store.Open` + `AuthMetadataFromDB` 注入 auth CLI)

### Failure Test (RED)

`cmd/yanshi/main_test.go` — `TestMainAuth_E2E`:

```go
package main

import (
    "bytes"
    "context"
    "encoding/json"
    "fmt"
    "net/http"
    "net/http/httptest"
    "os"
    "path/filepath"
    "strings"
    "testing"
    "time"
)

// TestMainAuth_E2E exercises the `yanshi auth` subcommand end-to-end via
// the main entry. Covers set (stdin + --api-key-stdin), status, logout.
// Uses ONLY loopback HTTPS-style file-backed storage (no real keyring).
func TestMainAuth_E2E(t *testing.T) {
    dir := t.TempDir()
    t.Setenv("YANSHI_PASSPHRASE", "e2e-pass")
    cfgPath := filepath.Join(dir, "config.yaml")
    cfgBody := fmt.Sprintf(`
storage:
  sqlite_path: %q
secrets:
  backend: file
  file_path: %q
  passphrase_env: YANSHI_PASSPHRASE
`, filepath.Join(dir, "auth.db"), filepath.Join(dir, "secrets.enc"))
    if err := os.WriteFile(cfgPath, []byte(cfgBody), 0600); err != nil {
        t.Fatal(err)
    }

    // 1) Set via --api-key-stdin.
    var out bytes.Buffer
    code := runCLI([]string{"yanshi", "--config", cfgPath, "auth", "set",
        "--provider", "openai", "--account", "main", "--api-key-stdin"},
        strings.NewReader("sk-e2e-stdin"), &out)
    if code != 0 {
        t.Fatalf("auth set exited %d: %s", code, out.String())
    }

    // 2) Status reports Authenticated.
    out.Reset()
    code = runCLI([]string{"yanshi", "--config", cfgPath, "auth", "status",
        "--provider", "openai", "--account", "main"}, nil, &out)
    if code != 0 || !strings.Contains(out.String(), "Authenticated: true") {
        t.Fatalf("auth status: code=%d out=%s", code, out.String())
    }

    // 3) Logout removes the credential.
    out.Reset()
    code = runCLI([]string{"yanshi", "--config", cfgPath, "auth", "logout",
        "--provider", "openai", "--account", "main"}, nil, &out)
    if code != 0 {
        t.Fatalf("auth logout: code=%d out=%s", code, out.String())
    }
    out.Reset()
    code = runCLI([]string{"yanshi", "--config", cfgPath, "auth", "status",
        "--provider", "openai", "--account", "main"}, nil, &out)
    if strings.Contains(out.String(), "Authenticated: true") {
        t.Fatalf("after logout, status still reports Authenticated: %s", out.String())
    }

    // 4) Delete missing account returns non-zero but does not corrupt the
    //    existing entry of a sibling account.
    var out2 bytes.Buffer
    _ = runCLI([]string{"yanshi", "--config", cfgPath, "auth", "set",
        "--provider", "openai", "--account", "sibling", "--api-key-stdin"},
        strings.NewReader("sk-sibling"), &out2)
    out.Reset()
    code = runCLI([]string{"yanshi", "--config", cfgPath, "auth", "logout",
        "--provider", "openai", "--account", "nonexistent"}, nil, &out)
    if code == 0 {
        t.Fatal("logout nonexistent must exit non-zero")
    }
    out.Reset()
    code = runCLI([]string{"yanshi", "--config", cfgPath, "auth", "status",
        "--provider", "openai", "--account", "sibling"}, nil, &out)
    if !strings.Contains(out.String(), "Authenticated: true") {
        t.Fatalf("sibling corrupted by failed logout: %s", out.String())
    }
}

type authCLITestClock struct{ now time.Time }
func (c authCLITestClock) Now() time.Time { return c.now }

type authCLITestSleeper struct{}
func (authCLITestSleeper) Sleep(context.Context, time.Duration) error { return nil }

func TestMainAuth_DeviceE2E_ReopensPersistedToken(t *testing.T) {
    const accessToken = "device-access-sentinel"
    const refreshToken = "device-refresh-sentinel"
    var tokenPolls int
    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        switch r.URL.Path {
        case "/device":
            _ = json.NewEncoder(w).Encode(map[string]any{
                "device_code": "device-code-sentinel",
                "user_code": "USER-CODE",
                "verification_uri": "https://example.com/device",
                "expires_in": 300,
                "interval": 1,
            })
        case "/token":
            tokenPolls++
            _ = json.NewEncoder(w).Encode(map[string]any{
                "access_token": accessToken,
                "refresh_token": refreshToken,
                "expires_in": 120,
                "token_type": "bearer",
            })
        default:
            http.NotFound(w, r)
        }
    }))
    defer srv.Close()

    dir := t.TempDir()
    t.Setenv("YANSHI_PASSPHRASE", "device-pass")
    cfgPath := filepath.Join(dir, "config.yaml")
    cfgBody := fmt.Sprintf(`
storage:
  sqlite_path: %q
secrets:
  backend: file
  file_path: %q
  passphrase_env: YANSHI_PASSPHRASE
auth:
  device:
    device_auth_enabled: true
    providers:
      - id: loopback
        client_id: test-client
        device_url: %q
        token_url: %q
`, filepath.Join(dir, "auth.db"), filepath.Join(dir, "secrets.enc"),
        srv.URL+"/device", srv.URL+"/token")
    if err := os.WriteFile(cfgPath, []byte(cfgBody), 0600); err != nil {
        t.Fatal(err)
    }

    var stdout, stderr bytes.Buffer
    code := runCLIWithAuthDeps(
        []string{"yanshi", "--config", cfgPath, "auth", "device",
            "--provider", "loopback", "--account", "main"},
        strings.NewReader(""), &stdout, &stderr,
        authCLIDeps{
            Clock: authCLITestClock{now: time.Unix(1000, 0)},
            Sleeper: authCLITestSleeper{},
        },
    )
    if code != 0 || tokenPolls != 1 {
        t.Fatalf("device flow: code=%d polls=%d stdout=%q stderr=%q",
            code, tokenPolls, stdout.String(), stderr.String())
    }
    for _, secret := range []string{
        accessToken, refreshToken, "device-code-sentinel",
    } {
        if strings.Contains(stdout.String(), secret) ||
            strings.Contains(stderr.String(), secret) {
            t.Fatalf("device CLI leaked %q: stdout=%q stderr=%q",
                secret, stdout.String(), stderr.String())
        }
    }

    // A fresh dispatcher invocation constructs a fresh Manager/FileStore and
    // proves the token is visible after process-style reopen, not merely in the
    // first Manager's in-memory snapshot.
    stdout.Reset()
    stderr.Reset()
    code = runCLI([]string{"yanshi", "--config", cfgPath, "auth", "status",
        "--provider", "loopback", "--account", "main"}, nil, &stdout, &stderr)
    if code != 0 || !strings.Contains(stdout.String(), "Authenticated: true") {
        t.Fatalf("reopened status: code=%d stdout=%q stderr=%q",
            code, stdout.String(), stderr.String())
    }
}
```

### Expected

- `yanshi auth set --provider X --account Y [--api-key-stdin | interactive]` 写入 secrets store。
- `yanshi auth status --provider X --account Y` 打印 `Authenticated: true|false` + Source。
- `yanshi auth logout --provider X --account Y` 删除条目；不存在时 exit non-zero。
- `yanshi auth device --provider X --account Y` 通过真实 loopback RFC 8628 HTTP server 验收；使用注入 Clock/Sleeper 消除真实等待，token 成功后同时写 encrypted Store 与 SQLite metadata，新的 CLI invocation 能重新看到 `Authenticated: true`。
- 同一 provider 下不同 account 互不污染（Task 6 唯一性 + Task 9 落地）。
- production `runCLI(args, stdin, stdout, stderr) int` 去掉 program name 后先消费 leading `--config FILE` / `--config=FILE`，再识别 `auth` 或 `doctor`；`--config` 不会被误判成 subcommand。测试调用的就是 production dispatcher，不保留 test-only mirror。
- `runCLIWithAuthDeps` 只为 loopback device E2E 注入 `Clock`/`Sleeper`，其解析与 dispatch 逻辑与 `runCLI` 完全共用；仅顶层 `main` 根据返回码调用 `os.Exit`。

### Implementation

`cmd/yanshi/main.go` 改动:

```go
// main keeps the existing --version check first. Immediately after it, route
// auth/doctor invocations (including a leading global --config) through the
// production dispatcher. Remove the old `case "doctor": doctor(os.Args[2:])`
// branch from the later switch; only this top-level site exits for these paths.
if isManagedInvocation(os.Args[1:]) {
    os.Exit(runCLI(os.Args, os.Stdin, os.Stdout, os.Stderr))
}

// authCLIDeps is zero in production. The loopback E2E supplies deterministic
// time without replacing the dispatcher or the HTTP provider.
type authCLIDeps struct {
    Clock   auth.Clock
    Sleeper auth.Sleeper
}

func isManagedInvocation(args []string) bool {
    for len(args) > 0 {
        switch {
        case args[0] == "--config":
            if len(args) < 2 {
                return true // dispatcher prints the precise usage error
            }
            args = args[2:]
        case strings.HasPrefix(args[0], "--config="):
            args = args[1:]
        default:
            return args[0] == "auth" || args[0] == "doctor"
        }
    }
    return false
}

func parseManagedInvocation(args []string) (
    cfgPath, sub string,
    rest []string,
    err error,
) {
    cfgPath = "config.yaml"
    for len(args) > 0 {
        switch {
        case args[0] == "--config":
            if len(args) < 2 || strings.TrimSpace(args[1]) == "" {
                return "", "", nil,
                    errors.New("--config requires a non-empty path")
            }
            cfgPath = args[1]
            args = args[2:]
        case strings.HasPrefix(args[0], "--config="):
            cfgPath = strings.TrimPrefix(args[0], "--config=")
            if strings.TrimSpace(cfgPath) == "" {
                return "", "", nil,
                    errors.New("--config requires a non-empty path")
            }
            args = args[1:]
        default:
            sub = args[0]
            return cfgPath, sub, append([]string(nil), args[1:]...), nil
        }
    }
    return "", "", nil, errors.New("missing subcommand after global flags")
}

// runCLI is the production, testable dispatcher. args includes argv[0].
func runCLI(
    args []string,
    stdin io.Reader,
    stdout io.Writer,
    stderrOpt ...io.Writer,
) int {
    stderr := io.Writer(io.Discard)
    if len(stderrOpt) > 0 && stderrOpt[0] != nil {
        stderr = stderrOpt[0]
    }
    return runCLIWithAuthDeps(
        args, stdin, stdout, stderr, authCLIDeps{})
}

func runCLIWithAuthDeps(
    args []string,
    stdin io.Reader,
    stdout, stderr io.Writer,
    deps authCLIDeps,
) int {
    if stdin == nil {
        stdin = strings.NewReader("")
    }
    if stdout == nil {
        stdout = io.Discard
    }
    if stderr == nil {
        stderr = io.Discard
    }
    if len(args) == 0 {
        return 2
    }
    cfgPath, sub, rest, err := parseManagedInvocation(args[1:])
    if err != nil {
        secrets.NewSafeLogger(stderr, secrets.NewRedactor()).Printf(
            "yanshi: %v", err)
        return 2
    }
    switch sub {
    case "auth":
        return runAuthSub(
            context.Background(), rest, cfgPath,
            stdin, stdout, stderr, deps,
        )
    case "doctor":
        doctorArgs := append([]string{"-config", cfgPath}, rest...)
        return runDoctor(context.Background(), doctorArgs, stdout, stderr)
    default:
        secrets.NewSafeLogger(stderr, secrets.NewRedactor()).Printf(
            "unknown subcommand: %s", sub)
        return 2
    }
}

// runAuthSub dispatches auth subcommands without os.Exit and with explicit I/O.
func runAuthSub(
    ctx context.Context,
    args []string,
    cfgPath string,
    stdin io.Reader,
    stdout, stderr io.Writer,
    deps authCLIDeps,
) int {
    bootstrapLog := secrets.NewSafeLogger(stderr, secrets.NewRedactor())
    if len(args) == 0 {
        bootstrapLog.Println("usage: yanshi auth <set|status|logout|device> ...")
        return 2
    }
    cfg, err := loadConfigForAuth(cfgPath)
    if err != nil {
        bootstrapLog.Printf("load config: %v", err)
        return 1
    }
    // Construct secrets.Manager from cfg. PassphraseEnv is the canonical
    // environment reference. The CLI never copies its value into config or
    // output; FileStore reads it through secrets.Manager.
    smgr, err := secrets.NewManager(secrets.Config{
        Backend:       cfg.Secrets.Backend,
        FilePath:      cfg.Secrets.FilePath,
        PassphraseEnv: cfg.Secrets.PassphraseEnv,
        Stderr:        stderr,
    })
    if err != nil {
        bootstrapLog.Printf("secrets: %v", err)
        return 1
    }
    defer smgr.Close()
    safeLog := secrets.NewSafeLogger(stderr, smgr.Redactor())
    amgr := auth.NewManager(smgr)
    authDB, err := store.Open(cfg.Storage.SQLitePath)
    if err != nil {
        safeLog.Println("auth: open metadata store failed")
        return 1
    }
    defer authDB.Close()
    amgr.SetMetadataStore(store.AuthMetadataFromDB(authDB.DB))
    if deps.Clock != nil {
        amgr.SetClock(deps.Clock)
    }
    if deps.Sleeper != nil {
        amgr.SetSleeper(deps.Sleeper)
    }

    // Register device providers from cfg only when device auth is enabled.
    // HTTPS-only validation runs at construction; a bad endpoint fails the
    // CLI with a clear message rather than silently exiting 0.
    if cfg.Auth.Device.DeviceAuthEnabled {
        seen := make(map[string]bool, len(cfg.Auth.Device.Providers))
        for _, dp := range cfg.Auth.Device.Providers {
            if strings.TrimSpace(dp.ID) == "" || seen[dp.ID] {
                safeLog.Println("auth: device provider ids must be non-empty and unique")
                return 1
            }
            seen[dp.ID] = true
            clientID := strings.TrimSpace(dp.ClientID)
            if clientID == "" {
                clientID = strings.TrimSpace(cfg.Auth.Device.ClientID)
            }
            gdp, derr := auth.NewGenericRFC8628Provider(auth.GenericRFC8628Config{
                ClientID:  clientID,
                DeviceURL: dp.DeviceURL,
                TokenURL:  dp.TokenURL,
                Scopes:    append([]string(nil), dp.Scopes...),
                Redactor:  smgr.Redactor(),
            })
            if derr != nil {
                // Constructor errors are fixed strings and contain no endpoint.
                safeLog.Printf("auth: device provider %s: %v", dp.ID, derr)
                return 1
            }
            amgr.RegisterDeviceProvider(dp.ID, gdp)
        }
    }

    sub := args[0]
    rest := args[1:]
    fs := flag.NewFlagSet("auth "+sub, flag.ContinueOnError)
    fs.SetOutput(io.Discard) // tests require no stderr leak from flag.Parse
    provider := fs.String("provider", "", "provider name (e.g. openai)")
    account := fs.String("account", "main", "account name (default: main)")
    apiKeyStdin := fs.Bool("api-key-stdin", false, "read API key from stdin (no prompt)")
    if err := fs.Parse(rest); err != nil {
        safeLog.Printf("auth %s: %v", sub, err)
        return 2
    }

    switch sub {
    case "set":
        cmd := secrets.AuthCommand{
            Provider:    *provider,
            Account:     *account,
            Backend:       cfg.Secrets.Backend,
            FilePath:      cfg.Secrets.FilePath,
            PassphraseEnv: cfg.Secrets.PassphraseEnv,
            APIKeyStdin:   *apiKeyStdin,
            Manager:     smgr,
            Stdin:       stdin,
            Stdout:      stdout,
        }
        if err := cmd.Run(); err != nil {
            safeLog.Printf("%v", err)
            return 1
        }
        return 0
    case "status":
        st, err := amgr.Status(*provider, *account)
        if err != nil {
            safeLog.Printf("%v", err)
            return 1
        }
        fmt.Fprintf(stdout, "Provider: %s\nAccount: %s\nAuthenticated: %v\nSource: %s\n",
            st.Provider, st.Account, st.Authenticated, st.Source)
        return 0
    case "logout":
        // Missing credentials are a cleanup failure, not a silent success:
        // propagate ErrSecretNotFound and exit 1 so automation can detect
        // that nothing was deleted. Any other Delete error also exits 1.
        // TestMainAuth_E2E step 4 asserts the non-zero code and verifies the
        // sibling credential remains intact.
        if err := amgr.Logout(*provider, *account); err != nil {
            safeLog.Printf("%v", err)
            return 1
        }
        fmt.Fprintf(stdout, "logged out %s/%s\n", *provider, *account)
        return 0
    case "device":
        // `yanshi auth device <provider-id>` triggers an RFC 8628 flow via
        // auth.Manager.RunDeviceFlow. Requires a configured provider under
        // auth.device.providers; the loop above registered them. If no
        // provider matches, exit 2 with a clear message (do NOT silently
        // fall back to API-key flow).
        if *provider == "" {
            safeLog.Println("auth device: --provider <id> is required")
            return 2
        }
        if _, err := amgr.RunDeviceFlow(ctx, *provider, *account, stdout); err != nil {
            safeLog.Printf("auth device: %v", err)
            return 1
        }
        return 0
    default:
        safeLog.Printf("unknown auth subcommand: %s", sub)
        return 2
    }
}

// loadConfigForAuth is a thin wrapper around config.Load that test code can
// also call directly. It exists so the auth subcommand does not have to
// duplicate the config-loading path of `serve`/`chat`.
func loadConfigForAuth(cfgPath string) (*config.Config, error) {
    return config.Load(cfgPath)
}
```

`cmd/yanshi/main.go` 的 import delta 明确加入 `github.com/x6nux/yanshi/internal/auth`、`github.com/x6nux/yanshi/internal/config` 与 `github.com/x6nux/yanshi/internal/secrets`；`internal/store` 已存在。`cmd/yanshi/main_test.go` 不定义 dispatcher，只调用同包 production `runCLI` / `runCLIWithAuthDeps`。

`internal/auth/cli.go` (device CLI helper):

```go
package auth

import (
    "context"
    "errors"
    "fmt"
    "io"
)

// RunDeviceFlow triggers a device authorization flow via the named provider,
// then commits the access token under (providerID, account) plus non-secret
// metadata. Progress is deliberately fixed text; failures are returned to the
// SafeLogger caller and are never copied to stdout.
func (m *Manager) RunDeviceFlow(
    ctx context.Context,
    providerID, account string,
    stdout io.Writer,
) (*DeviceToken, error) {
    dp, ok := m.deviceProviders[providerID]
    if !ok {
        return nil, fmt.Errorf("auth: no device provider registered")
    }
    tok, err := dp.Authorize(ctx, m.clk, m.slp, func(st StatusUpdate) {
        switch st.Stage {
        case "device_code_issued":
            fmt.Fprintf(stdout, "Visit %s and enter code: %s\n", st.VerificationURI, st.UserCode)
        case "polling":
            fmt.Fprintln(stdout, "polling...")
        case "slow_down":
            fmt.Fprintln(stdout, "slowing down...")
        case "success":
            // Do not print success yet: token + metadata commit is still pending.
        }
    })
    if err != nil {
        return nil, err
    }
    if tok == nil {
        return nil, errors.New("auth: device provider returned no token")
    }
    if tok.AccessToken != "" {
        m.secrets.Redactor().Register(tok.AccessToken)
    }
    if tok.RefreshToken != "" {
        m.secrets.Redactor().Register(tok.RefreshToken)
    }
    if err := m.commitDeviceToken(providerID, account, tok); err != nil {
        return nil, err
    }
    fmt.Fprintln(stdout, "authenticated")
    return tok, nil
}
```

### Pass

```sh
go test ./internal/auth -run "Test(DeviceFlow_|CommitDeviceToken_)"
go test ./cmd/yanshi -run "TestMainAuth_(E2E|DeviceE2E_ReopensPersistedToken)"
```

### Commit

```
feat(auth): CLI yanshi auth {set,status,logout} + TestMainAuth E2E (O03-L4)

`yanshi auth set/status/logout` subcommands operate on one (provider, account)
pair via the unified secrets.Manager + auth.Manager. TestMainAuth covers
stdin, --api-key-stdin, and the cleanup-failure path (logout missing) without
corrupting sibling entries. `yanshi auth device` uses the Manager's injected
Clock/Sleeper, emits only fixed progress text, and commits the returned token
and SQLite metadata before reporting success.
```

---

## Task 10 — I18N1-L1: catalog (en + zh-Hans) + `auto` 跨启动重算

**结构性修复落点:** #7 (locale auto：持久化 `auto` 不解析值，effective locale 每次启动重算；测试跨两次启动改 LC_ALL/LANG)。

### Files

- Create `internal/i18n/catalog/en.json`
- Create `internal/i18n/catalog/zh-Hans.json`
- Create `internal/i18n/i18n.go`
- Create `internal/i18n/i18n_test.go`

### Failure Test (RED)

`internal/i18n/i18n_test.go`:

```go
package i18n

import (
    "reflect"
    "sort"
    "testing"
)

func TestBundle_EmptyCanonicalizesToAuto(t *testing.T) {
    t.Setenv("LC_ALL", "C")
    t.Setenv("LANG", "zh_CN.UTF-8")
    b, err := NewBundle("")
    if err != nil {
        t.Fatal(err)
    }
    if b.Persistent() != "auto" || b.Effective() != "en" {
        t.Fatalf("empty must canonicalize to auto; C locale wins: persistent=%s effective=%s",
            b.Persistent(), b.Effective())
    }
}

func TestBundle_AutoRecomputedEachLoad(t *testing.T) {
    // First load: empty LC_ALL, LANG=en_US.UTF-8 -> effective en.
    t.Setenv("LC_ALL", "")
    t.Setenv("LANG", "en_US.UTF-8")
    b1, _ := NewBundle("auto")
    if b1.Effective() != "en" {
        t.Fatalf("auto + LANG=en_US.UTF-8 -> expected en, got %s", b1.Effective())
    }
    // Second load: same persistent value "auto", but LANG=zh_CN.UTF-8 -> zh-Hans.
    t.Setenv("LANG", "zh_CN.UTF-8")
    b2, _ := NewBundle("auto")
    if b2.Effective() != "zh-Hans" {
        t.Fatalf("auto + LANG=zh_CN.UTF-8 -> expected zh-Hans, got %s", b2.Effective())
    }
    // Persistent value must NOT have been rewritten to a resolved locale;
    // "auto" stays "auto" so future loads keep recomputing.
    if b2.Persistent() != "auto" {
        t.Fatalf("Persistent corrupted to %s", b2.Persistent())
    }
}

func TestBundle_LCAllCStopsLANGFallback(t *testing.T) {
    for _, locale := range []string{"C", "POSIX"} {
        t.Run(locale, func(t *testing.T) {
            t.Setenv("LC_ALL", locale)
            t.Setenv("LANG", "zh_CN.UTF-8")
            b, err := NewBundle("auto")
            if err != nil {
                t.Fatal(err)
            }
            if b.Effective() != "en" {
                t.Fatalf("LC_ALL=%s must force en instead of falling through to LANG: %s",
                    locale, b.Effective())
            }
        })
    }
}

func TestBundle_ExplicitLocaleOverridesEnv(t *testing.T) {
    t.Setenv("LANG", "zh_CN.UTF-8")
    b, _ := NewBundle("en")
    if b.Effective() != "en" {
        t.Fatalf("explicit en must override LANG=zh_CN, got %s", b.Effective())
    }
}

func TestBundle_FallbackOnMissingKey(t *testing.T) {
    b, _ := NewBundle("zh-Hans")
    // Key exists in en, exists in zh-Hans too; non-existent key falls back
    // to the key itself (not an error).
    got := b.Get("nonexistent.key")
    if got != "nonexistent.key" {
        t.Fatalf("missing key should return key, got %q", got)
    }
}

func TestBundle_FallbackOnMissingLocale(t *testing.T) {
    b, err := NewBundle("fr-FR")
    if err == nil {
        t.Fatal("expected error for unsupported locale")
    }
    // Even on unsupported locale, default bundle falls back to en.
    b, _ = NewBundle("")
    _ = b
}

// TestBundle_CatalogKeyManifest compares each catalog independently with the
// canonical required-key manifest. Pairwise en/zh equality alone is insufficient:
// deleting the same key from both files must still fail this test.
func TestBundle_CatalogKeyManifest(t *testing.T) {
    want := append([]string(nil), requiredCatalogKeys...)
    sort.Strings(want)
    if len(want) == 0 {
        t.Fatal("requiredCatalogKeys is empty")
    }
    wantSet := make(map[string]bool, len(want))
    for _, key := range want {
        if wantSet[key] {
            t.Fatalf("duplicate manifest key %q", key)
        }
        wantSet[key] = true
    }

    for _, locale := range []string{"en", "zh-Hans"} {
        catalog, err := loadCatalog(locale)
        if err != nil {
            t.Fatalf("load %s: %v", locale, err)
        }
        got := make([]string, 0, len(catalog))
        for key := range catalog {
            got = append(got, key)
        }
        sort.Strings(got)
        if !reflect.DeepEqual(got, want) {
            t.Fatalf("%s keys = %v, want exact manifest %v", locale, got, want)
        }
    }
}
```

`auto` 的跨启动 persistence 测试只放在 Task 11 的 `internal/cli/tui/preferences_test.go`；`internal/i18n` 不拥有第二套 preferences 文件格式或 I/O 实现。

### Expected

- `NewBundle(persistent)` 接受 `""`、`"auto"`、`"en"`、`"zh-Hans"`；`""` 统一 canonicalize 为 persistent `"auto"`，unsupported locale 返回带英文 fallback bundle 的 error。
- `Effective()` 返回解析后的 locale；`Persistent()` 返回 canonical persistent 值（**`auto` 永远不被改写为 effective locale**）。
- `auto` 模式按真正的 `LC_ALL > LANG > "en"` 探测：非空 `LC_ALL=C` / `POSIX` 直接解析为 `en`，不得继续读取中文 `LANG`；识别 `zh_CN` / `zh_CN.UTF-8` / `zh-Hans` / `zh_Hans` 等变体。
- catalog 用 `go:embed` 编入；唯一 `requiredCatalogKeys` manifest 与 `en`、`zh-Hans` 各自的 key set 精确相等。两个 catalog 同时漏掉同一个 key 仍然 RED。
- i18n 包仅负责 catalog、locale detection 和 formatting；preferences persistence 的唯一 owner 是 Task 11 的 `internal/cli/tui`。

### Implementation

`internal/i18n/catalog/en.json`:

```json
{
  "tui.input.placeholder": "Type a message... (Enter to send, Ctrl+Enter for newline)",
  "tui.command.help.title": "Commands",
  "tui.command.help.row": "  /{name}  {help}",
  "tui.command.help.help": "list commands",
  "tui.command.help.model": "list / switch model",
  "tui.command.help.think": "set reasoning effort (low|medium|high|off)",
  "tui.command.help.mode": "set permission mode (default|allow-edits|yolo|auto [1-10])",
  "tui.command.help.queue_mode": "set/cycle queue mode (queue|single|batch)",
  "tui.command.help.clear": "reset conversation",
  "tui.command.help.config": "show active config",
  "tui.command.help.cost": "token usage this session",
  "tui.command.help.stats": "token consumption histogram (recent sessions)",
  "tui.command.help.compact": "compact context (WS only)",
  "tui.command.help.mcp": "list MCP servers",
  "tui.command.help.sessions": "list stored sessions",
  "tui.command.help.restore": "restore a stored session by ID",
  "tui.command.help.rename": "rename a session: /rename <id> <title>",
  "tui.command.help.archive": "hide a session: /archive <id>",
  "tui.command.help.unarchive": "restore a session: /unarchive <id>",
  "tui.command.help.archived": "list archived sessions",
  "tui.command.help.delete": "delete a session: /delete <id> yes",
  "tui.command.help.theme": "list / switch colour theme",
  "tui.command.help.locale": "show / set UI locale (auto|en|zh-Hans)",
  "tui.command.help.keymap": "show keymap diagnostics or restore defaults",
  "tui.command.help.vim": "show / set Vim mode (on|off)",
  "tui.command.help.contrast": "show / set high contrast (on|off)",
  "tui.command.locale.usage": "usage: /locale [auto|en|zh-Hans]",
  "tui.command.locale.current": "UI locale: {locale}",
  "tui.command.locale.changed": "UI locale set to {locale}",
  "tui.command.keymap.usage": "usage: /keymap [diagnostics|reset]",
  "tui.command.keymap.none": "keymap diagnostics: none",
  "tui.command.keymap.reset": "keymap restored to defaults",
  "tui.command.keymap.conflict": "conflict: {key} bound to {actions}",
  "tui.command.vim.usage": "usage: /vim [on|off]",
  "tui.command.vim.enabled": "Vim mode enabled",
  "tui.command.vim.disabled": "Vim mode disabled",
  "tui.command.contrast.usage": "usage: /contrast [on|off]",
  "tui.command.preference.persist_failed": "could not save preferences: {error}",
  "tui.status.model": "model",
  "tui.status.thinking": "thinking",
  "tui.status.tokens_in": "tokens in",
  "tui.status.tokens_out": "tokens out",
  "tui.error.connect_failed": "connection failed",
  "tui.error.auth_failed": "authentication failed",
  "tui.error.send_failed": "send failed",
  "tui.error.unknown_command": "unknown command: /{name} (try /help)",
  "tui.workflow.running": "running",
  "tui.workflow.completed": "completed",
  "tui.workflow.failed": "failed",
  "auth.cli.prompt": "Enter API key for {provider}/{account}: ",
  "auth.cli.stored": "stored {provider}/{account}",
  "auth.cli.deleted": "deleted {provider}/{account}",
  "auth.cli.status.authenticated": "Authenticated: true",
  "auth.cli.status.unauthenticated": "Authenticated: false",
  "keymap.diagnostics.conflict": "conflict: {key} bound to {a} and {b}",
  "keymap.diagnostics.normalized_duplicate": "duplicate key after normalization: {key}",
  "keymap.diagnostics.unknown_action": "unknown action: {action}",
  "keymap.diagnostics.invalid_key": "invalid key: {key}",
  "keymap.diagnostics.unsupported_keymap": "unsupported keymap: {name}",
  "keymap.diagnostics.reset": "keymap restored to defaults",
  "vim.mode.insert": "INSERT",
  "vim.mode.normal": "NORMAL",
  "vim.mode.visual": "VISUAL",
  "contrast.enabled": "high contrast enabled",
  "contrast.disabled": "high contrast disabled"
}
```

`internal/i18n/catalog/zh-Hans.json`:

```json
{
  "tui.input.placeholder": "输入消息…（Enter 发送，Ctrl+Enter 换行）",
  "tui.command.help.title": "命令",
  "tui.command.help.row": "  /{name}  {help}",
  "tui.command.help.help": "列出命令",
  "tui.command.help.model": "列出或切换模型",
  "tui.command.help.think": "设置推理强度（low|medium|high|off）",
  "tui.command.help.mode": "设置权限模式（default|allow-edits|yolo|auto [1-10]）",
  "tui.command.help.queue_mode": "设置或循环消息队列模式（queue|single|batch）",
  "tui.command.help.clear": "清空当前会话",
  "tui.command.help.config": "显示当前配置",
  "tui.command.help.cost": "显示本会话 token 用量",
  "tui.command.help.stats": "显示近期会话 token 直方图",
  "tui.command.help.compact": "压缩上下文（仅 WS）",
  "tui.command.help.mcp": "列出 MCP 服务",
  "tui.command.help.sessions": "列出已存会话",
  "tui.command.help.restore": "按 ID 恢复会话",
  "tui.command.help.rename": "重命名会话：/rename <id> <title>",
  "tui.command.help.archive": "隐藏会话：/archive <id>",
  "tui.command.help.unarchive": "恢复已归档会话：/unarchive <id>",
  "tui.command.help.archived": "列出已归档会话",
  "tui.command.help.delete": "删除会话：/delete <id> yes",
  "tui.command.help.theme": "列出或切换配色主题",
  "tui.command.help.locale": "显示或设置 UI 语言（auto|en|zh-Hans）",
  "tui.command.help.keymap": "显示键位诊断或恢复默认键位",
  "tui.command.help.vim": "显示或设置 Vim 模式（on|off）",
  "tui.command.help.contrast": "显示或设置高对比度（on|off）",
  "tui.command.locale.usage": "用法：/locale [auto|en|zh-Hans]",
  "tui.command.locale.current": "UI 语言：{locale}",
  "tui.command.locale.changed": "UI 语言已设为 {locale}",
  "tui.command.keymap.usage": "用法：/keymap [diagnostics|reset]",
  "tui.command.keymap.none": "键位诊断：无",
  "tui.command.keymap.reset": "键位已恢复默认值",
  "tui.command.keymap.conflict": "冲突：{key} 绑定到 {actions}",
  "tui.command.vim.usage": "用法：/vim [on|off]",
  "tui.command.vim.enabled": "Vim 模式已启用",
  "tui.command.vim.disabled": "Vim 模式已停用",
  "tui.command.contrast.usage": "用法：/contrast [on|off]",
  "tui.command.preference.persist_failed": "无法保存偏好：{error}",
  "tui.status.model": "模型",
  "tui.status.thinking": "思考",
  "tui.status.tokens_in": "输入 token",
  "tui.status.tokens_out": "输出 token",
  "tui.error.connect_failed": "连接失败",
  "tui.error.auth_failed": "认证失败",
  "tui.error.send_failed": "发送失败",
  "tui.error.unknown_command": "未知命令：/{name}（可尝试 /help）",
  "tui.workflow.running": "运行中",
  "tui.workflow.completed": "已完成",
  "tui.workflow.failed": "失败",
  "auth.cli.prompt": "输入 {provider}/{account} 的 API key：",
  "auth.cli.stored": "已存储 {provider}/{account}",
  "auth.cli.deleted": "已删除 {provider}/{account}",
  "auth.cli.status.authenticated": "已认证：是",
  "auth.cli.status.unauthenticated": "已认证：否",
  "keymap.diagnostics.conflict": "冲突：{key} 同时绑定到 {a} 和 {b}",
  "keymap.diagnostics.normalized_duplicate": "规范化后出现重复键：{key}",
  "keymap.diagnostics.unknown_action": "未知动作：{action}",
  "keymap.diagnostics.invalid_key": "无效键：{key}",
  "keymap.diagnostics.unsupported_keymap": "不支持的键位方案：{name}",
  "keymap.diagnostics.reset": "键位已恢复默认值",
  "vim.mode.insert": "插入",
  "vim.mode.normal": "普通",
  "vim.mode.visual": "可视",
  "contrast.enabled": "高对比度已启用",
  "contrast.disabled": "高对比度已关闭"
}
```

`internal/i18n/i18n.go`:

```go
// Package i18n provides locale-aware message lookup. Catalogs are embedded
// JSON (en, zh-Hans). The Bundle is constructed at startup with the
// PERSISTENT locale string ("auto" / "en" / "zh-Hans") and recomputes the
// EFFECTIVE locale on every NewBundle call — so "auto" tracks LC_ALL/LANG
// across restarts instead of being resolved once and frozen.
package i18n

import (
    "embed"
    "encoding/json"
    "fmt"
    "os"
    "strings"
)

//go:embed catalog/*.json
var catalogFS embed.FS

// Supported lists the locales with embedded catalogs. "auto" is a valid
// PERSISTENT value but not a member of Supported (it resolves to one).
var Supported = []string{"en", "zh-Hans"}

// requiredCatalogKeys is the canonical UI contract. Tests compare each
// embedded catalog independently against this exact set, so deleting the same
// key from both translations cannot make a pairwise-parity test pass.
var requiredCatalogKeys = []string{
    "tui.input.placeholder",
    "tui.command.help.title",
    "tui.command.help.row",
    "tui.command.help.help",
    "tui.command.help.model",
    "tui.command.help.think",
    "tui.command.help.mode",
    "tui.command.help.queue_mode",
    "tui.command.help.clear",
    "tui.command.help.config",
    "tui.command.help.cost",
    "tui.command.help.stats",
    "tui.command.help.compact",
    "tui.command.help.mcp",
    "tui.command.help.sessions",
    "tui.command.help.restore",
    "tui.command.help.rename",
    "tui.command.help.archive",
    "tui.command.help.unarchive",
    "tui.command.help.archived",
    "tui.command.help.delete",
    "tui.command.help.theme",
    "tui.command.help.locale",
    "tui.command.help.keymap",
    "tui.command.help.vim",
    "tui.command.help.contrast",
    "tui.command.locale.usage",
    "tui.command.locale.current",
    "tui.command.locale.changed",
    "tui.command.keymap.usage",
    "tui.command.keymap.none",
    "tui.command.keymap.reset",
    "tui.command.keymap.conflict",
    "tui.command.vim.usage",
    "tui.command.vim.enabled",
    "tui.command.vim.disabled",
    "tui.command.contrast.usage",
    "tui.command.preference.persist_failed",
    "tui.status.model",
    "tui.status.thinking",
    "tui.status.tokens_in",
    "tui.status.tokens_out",
    "tui.error.connect_failed",
    "tui.error.auth_failed",
    "tui.error.send_failed",
    "tui.error.unknown_command",
    "tui.workflow.running",
    "tui.workflow.completed",
    "tui.workflow.failed",
    "auth.cli.prompt",
    "auth.cli.stored",
    "auth.cli.deleted",
    "auth.cli.status.authenticated",
    "auth.cli.status.unauthenticated",
    "keymap.diagnostics.conflict",
    "keymap.diagnostics.normalized_duplicate",
    "keymap.diagnostics.unknown_action",
    "keymap.diagnostics.invalid_key",
    "keymap.diagnostics.unsupported_keymap",
    "keymap.diagnostics.reset",
    "vim.mode.insert",
    "vim.mode.normal",
    "vim.mode.visual",
    "contrast.enabled",
    "contrast.disabled",
}

// Bundle is a locale catalog. Persistent is the user's configured value
// (may be "auto"); Effective is the resolved locale actually used for lookups.
type Bundle struct {
    persistent string
    effective  string
    messages   map[string]string
}

// NewBundle constructs a Bundle from a persistent locale string. An empty or
// "auto" persistent value triggers LC_ALL/LANG detection on every call.
// Unsupported locales return an error.
func NewBundle(persistent string) (*Bundle, error) {
    if persistent == "" {
        persistent = "auto"
    }
    effective := persistent
    if persistent == "auto" {
        effective = detectLocale()
    }
    if !isSupported(effective) {
        // Unknown explicit locale: return a usable English bundle plus an error.
        messages, loadErr := loadCatalog("en")
        if loadErr != nil {
            return nil, loadErr
        }
        b := &Bundle{persistent: persistent, effective: "en", messages: messages}
        return b, fmt.Errorf("i18n: locale %q not supported; using en", persistent)
    }
    messages, err := loadCatalog(effective)
    if err != nil {
        return nil, err
    }
    return &Bundle{
        persistent: persistent,
        effective:  effective,
        messages:   messages,
    }, nil
}

// Persistent returns the original configured locale (may be "auto").
// Callers persisting preferences MUST use this, not Effective, so that
// changes to LC_ALL/LANG take effect on the next startup.
func (b *Bundle) Persistent() string { return b.persistent }

// Effective returns the resolved locale used for lookups.
func (b *Bundle) Effective() string { return b.effective }

// Get looks up key in the catalog. Missing keys fall back to the key itself
// (not an error) so the UI stays usable if a catalog lags behind code.
func (b *Bundle) Get(key string) string {
    if v, ok := b.messages[key]; ok {
        return v
    }
    return key
}

// GetF looks up key and substitutes {name} placeholders from vars.
func (b *Bundle) GetF(key string, vars map[string]string) string {
    s := b.Get(key)
    for k, v := range vars {
        s = strings.ReplaceAll(s, "{"+k+"}", v)
    }
    return s
}

func detectLocale() string {
    // LC_ALL truly overrides LANG. In particular C/POSIX means English and
    // must not fall through to a zh LANG value.
    if v := os.Getenv("LC_ALL"); v != "" {
        return normalizeLocale(v)
    }
    if v := os.Getenv("LANG"); v != "" {
        return normalizeLocale(v)
    }
    return "en"
}

func normalizeLocale(raw string) string {
    // Strip modifier/codeset and canonicalize separators/case for matching.
    s := raw
    if i := strings.IndexByte(s, '@'); i >= 0 {
        s = s[:i]
    }
    if i := strings.IndexByte(s, '.'); i >= 0 {
        s = s[:i]
    }
    s = strings.ReplaceAll(s, "_", "-")
    lower := strings.ToLower(s)
    switch {
    case lower == "c" || lower == "posix":
        return "en"
    case strings.HasPrefix(lower, "zh-hans"),
        lower == "zh-cn", lower == "zh-sg":
        return "zh-Hans"
    case strings.HasPrefix(lower, "zh-hant"),
        lower == "zh-tw", lower == "zh-hk":
        return "en"
    case strings.HasPrefix(lower, "en"):
        return "en"
    default:
        return "en"
    }
}

func isSupported(loc string) bool {
    for _, s := range Supported {
        if s == loc {
            return true
        }
    }
    return false
}

func loadCatalog(loc string) (map[string]string, error) {
    data, err := catalogFS.ReadFile("catalog/" + loc + ".json")
    if err != nil {
        return nil, fmt.Errorf("i18n: read embedded catalog %s: %w", loc, err)
    }
    var messages map[string]string
    if err := json.Unmarshal(data, &messages); err != nil {
        return nil, fmt.Errorf("i18n: decode embedded catalog %s: %w", loc, err)
    }
    return messages, nil
}

// Preferences persistence intentionally does not live in this package. The
// sole owner is internal/cli/tui/preferences.go (Task 11).
```

### Pass

```sh
go test ./internal/i18n -run TestBundle_
```

### Commit

```
feat(i18n): catalog + auto-recompute Bundle (I18N1-L1)

Embeds en + zh-Hans catalogs via go:embed. Bundle.Persistent stays "auto"
across loads; Bundle.Effective is recomputed each startup from true
LC_ALL-over-LANG precedence (including C/POSIX). Each catalog is checked
against the canonical requiredCatalogKeys manifest; preference file I/O remains
owned solely by internal/cli/tui.
```

---

## Task 11 — I18N1-L2: TUI 接线 + helpEntry 重构 + output_language

**结构性修复落点:** #9 (Vim tri-state + locale/theme/keymap 合并规则 — 此 Task 落 locale/theme 部分)、#14 (测试 API 修复 — loadPreferences 注入路径用 t.TempDir；persistPreferences 用 t.Cleanup 恢复)、#15 (i18n 接线 — helpEntry 不持有 bundle → 重构；统一配置名 `i18n.output_language`；列出 catalog key manifest + 选定 surface 无硬编码测试)。

### Files

- Modify `internal/cli/tui/commands.go` (`command.helpKey`、palette、`helpEntry`→`cmdHelpEntry` 预渲染)
- Modify `internal/cli/tui/commands_test.go` (help/palette 全部走 catalog)
- Create `internal/cli/tui/preferences.go`
- Create `internal/cli/tui/preferences_test.go`
- Create `internal/cli/tui/preferences_replace_unix.go`
- Create `internal/cli/tui/preferences_replace_windows.go`
- Modify `internal/cli/tui/model.go` (`bundle`、prefs、effective prefs、`NewProgramWithOptions`)
- Modify `internal/cli/tui/view_test.go` (选定 surface 的行为级本地化测试)
- Modify `cmd/yanshi/main.go` (把 flag/env/project 四层输入传给 TUI)
- Modify `cmd/yanshi/main_test.go` (TUI preference flag 解析测试)
- Modify `internal/bootstrap/bootstrap.go` (`i18n.output_language` 只追加到 project/default orchestrator instruction)
- Modify `internal/agent/orchestrator/orchestrator.go` (导出唯一 `DefaultInstruction`，避免 bootstrap 复制默认文案)
- Modify `internal/bootstrap/bootstrap_test.go` (output language 与 UI locale 独立且保留默认 instruction)

### Failure Test (RED)

`internal/cli/tui/commands_test.go` — `TestCmdHelpEntry_PreRendered`:

```go
package tui

import (
    "strings"
    "testing"

    "github.com/x6nux/yanshi/internal/i18n"
)

func TestCmdHelpEntry_PreRendered(t *testing.T) {
    b, _ := i18n.NewBundle("en")
    e := newCmdHelpEntry(b, commandTable)
    out := e.render(80, spinner.Model{})
    // Pre-rendered rows must contain the localized "commands" title.
    if !strings.Contains(out, b.Get("tui.command.help.title")) {
        t.Fatalf("render missing localized title: %q", out)
    }
    // Every command and its catalog-backed help must appear in the rendered rows.
    for _, c := range commandTable {
        if !strings.Contains(out, "/"+c.name) {
            t.Fatalf("render missing /%s", c.name)
        }
        if !strings.Contains(out, b.Get(c.helpKey)) {
            t.Fatalf("render missing localized help %q for /%s", c.helpKey, c.name)
        }
    }
}

// TestCmdHelpEntry_NoHardcodedEnglish asserts the rendering does not
// hardcode "Commands" or other untranslated text. It uses the bundle for
// every visible surface.
func TestCmdHelpEntry_NoHardcodedEnglish(t *testing.T) {
    b, _ := i18n.NewBundle("zh-Hans")
    e := newCmdHelpEntry(b, commandTable)
    out := e.render(80, spinner.Model{})
    if strings.Contains(out, "Commands") {
        t.Fatalf("hardcoded English 'Commands' in zh-Hans rendering: %q", out)
    }
}
```

`internal/cli/tui/preferences_test.go`:

```go
package tui

import (
    "errors"
    "os"
    "path/filepath"
    "strings"
    "testing"

    "github.com/x6nux/yanshi/internal/i18n"
)

// TestPreferences_FourLevelMerge exercises every sparse preference field with
// the exact priority: flag > env > user prefs > project config > defaults.
// Pointer booleans preserve "unset" versus an explicit false at every layer.
func TestPreferences_FourLevelMerge(t *testing.T) {
    f, tt := false, true
    project := Preferences{
        UILocale: "zh-Hans", ThemeName: "project-theme", KeymapName: "project-map",
        HighContrast: &tt, Vim: &tt,
    }
    user := Preferences{
        UILocale: "en", ThemeName: "user-theme", KeymapName: "user-map",
        HighContrast: &f, Vim: &f,
    }
    env := Preferences{
        UILocale: "zh-Hans", ThemeName: "env-theme", KeymapName: "env-map",
        HighContrast: &tt, Vim: &tt,
    }
    flags := Preferences{
        UILocale: "en", ThemeName: "flag-theme", KeymapName: "flag-map",
        HighContrast: &f, Vim: &f,
    }

    got := mergeTUIPrefs(flags, env, user, project)
    want := EffectivePreferences{
        UILocale: "en", ThemeName: "flag-theme", KeymapName: "flag-map",
        HighContrast: false, Vim: false,
    }
    if got != want {
        t.Fatalf("flag precedence: got %#v want %#v", got, want)
    }

    // Remove flags, then env must win all fields.
    got = mergeTUIPrefs(Preferences{}, env, user, project)
    want = EffectivePreferences{
        UILocale: "zh-Hans", ThemeName: "env-theme", KeymapName: "env-map",
        HighContrast: true, Vim: true,
    }
    if got != want {
        t.Fatalf("env precedence: got %#v want %#v", got, want)
    }

    // Explicit false in user prefs must beat project true.
    got = mergeTUIPrefs(Preferences{}, Preferences{}, user, project)
    want = EffectivePreferences{
        UILocale: "en", ThemeName: "user-theme", KeymapName: "user-map",
        HighContrast: false, Vim: false,
    }
    if got != want {
        t.Fatalf("user precedence: got %#v want %#v", got, want)
    }

    // With every sparse layer empty, defaults are stable.
    got = mergeTUIPrefs(Preferences{}, Preferences{}, Preferences{}, Preferences{})
    want = EffectivePreferences{
        UILocale: "auto", ThemeName: "default", KeymapName: "default",
        HighContrast: false, Vim: false,
    }
    if got != want {
        t.Fatalf("defaults: got %#v want %#v", got, want)
    }
}

// TestPreferences_LoadInjectsPath uses t.TempDir so the test does not
// touch the real prefs file (structural fix #14).
func TestPreferences_LoadInjectsPath(t *testing.T) {
    path := filepath.Join(t.TempDir(), "prefs.json")
    _ = os.WriteFile(path, []byte(`{"ui_locale":"en"}`), 0600)
    p, err := loadPreferences(path)
    if err != nil {
        t.Fatal(err)
    }
    if p.UILocale != "en" {
        t.Fatalf("UILocale: %q", p.UILocale)
    }
}

// TestPreferences_PersistAtomic deterministically covers stale temp files,
// replacement failure, old-file preservation, and temp cleanup on every OS.
func TestPreferences_PersistAtomic(t *testing.T) {
    dir := t.TempDir()
    path := filepath.Join(dir, "prefs.json")
    old := []byte(`{"ui_locale":"en"}`)
    if err := os.WriteFile(path, old, 0600); err != nil {
        t.Fatal(err)
    }
    // A stale temp file must not collide with the random suffix for this write.
    stale := path + ".tmp.stale"
    if err := os.WriteFile(stale, []byte("stale"), 0600); err != nil {
        t.Fatal(err)
    }

    oldReplace := replacePreferencesFile
    replacePreferencesFile = func(src, dst string) error {
        if dst != path || !strings.HasPrefix(src, path+".tmp.") {
            t.Fatalf("unexpected replace %q -> %q", src, dst)
        }
        return errors.New("injected replace failure")
    }
    t.Cleanup(func() { replacePreferencesFile = oldReplace })

    f := false
    err := persistPreferences(path, Preferences{UILocale: "zh-Hans", Vim: &f})
    if err == nil || !strings.Contains(err.Error(), "injected replace failure") {
        t.Fatalf("expected injected replace failure, got %v", err)
    }
    data, err := os.ReadFile(path)
    if err != nil {
        t.Fatal(err)
    }
    if string(data) != string(old) {
        t.Fatalf("old prefs changed: got %s want %s", data, old)
    }
    matches, err := filepath.Glob(path + ".tmp.*")
    if err != nil {
        t.Fatal(err)
    }
    if len(matches) != 1 || matches[0] != stale {
        t.Fatalf("new temp not cleaned or stale file changed: %v", matches)
    }
}

// TestPreferences_CleanupRestoresState ensures tests using t.Cleanup
// restore persistPermMode + path overrides (structural fix #14).
func TestPreferences_CleanupRestoresState(t *testing.T) {
    // Save the current test-disable toggle.
    oldDisable := preferencesPersistDisabled
    preferencesPersistDisabled = true
    t.Cleanup(func() { preferencesPersistDisabled = oldDisable })

    err := persistPreferences("/nonexistent/path/prefs.json", Preferences{})
    if err != nil {
        t.Fatalf("persist should be no-op when disabled: %v", err)
    }
}

func TestPreferences_AutoLocaleRecomputesAfterReload(t *testing.T) {
    path := filepath.Join(t.TempDir(), "prefs.json")
    if err := persistPreferences(path, Preferences{UILocale: "auto"}); err != nil {
        t.Fatal(err)
    }

    t.Setenv("LC_ALL", "en_US.UTF-8")
    firstPrefs, err := loadPreferences(path)
    if err != nil {
        t.Fatal(err)
    }
    first, err := i18n.NewBundle(firstPrefs.UILocale)
    if err != nil {
        t.Fatal(err)
    }
    if first.Persistent() != "auto" || first.Effective() != "en" {
        t.Fatalf("first = %q/%q", first.Persistent(), first.Effective())
    }

    t.Setenv("LC_ALL", "zh_CN.UTF-8")
    secondPrefs, err := loadPreferences(path)
    if err != nil {
        t.Fatal(err)
    }
    second, err := i18n.NewBundle(secondPrefs.UILocale)
    if err != nil {
        t.Fatal(err)
    }
    if second.Persistent() != "auto" || second.Effective() != "zh-Hans" {
        t.Fatalf("second = %q/%q", second.Persistent(), second.Effective())
    }
}

func boolPtr(b bool) *bool { return &b }
```

### Expected

- `cmdHelpEntry` 在构造时持 `*i18n.Bundle` + `commandTable` 快照；`render` 用 `bundle.Get("tui.command.help.title")` 与 `bundle.GetF("tui.command.help.row", ...)` 输出本地化文本。
- `mergeTUIPrefs(flags, env, user, project)` 对 locale/theme/keymap/high-contrast/Vim 五个用户选项统一按 `flag > env > user prefs > project config > defaults` 合并，返回无 tri-state 的 `EffectivePreferences`；各稀疏 bool 层用 `*bool` 保留 explicit-off。内部 `KeymapReset *bool` 是 `/keymap reset` 的持久化 tombstone：user=true 时 Task 13 不再套用低优先级 project bindings。
- `loadPreferences(path)` 与 `persistPreferences(path, p)` 用 `t.TempDir()` 注入路径；`persistPreferences` 在 rename 失败时清理 tmp 文件并保留旧配置。
- `preferencesPersistDisabled` 是测试可禁用的开关（同 `persistPermMode` 模式）。
- `i18n.output_language` 由 `cfg.I18N.OutputLanguage` 读取（**统一配置名**），注入到 orchestrator 的 system prompt（空值表示"跟随用户输入语言"，不加指令）。
- 选定本地化 surface 明确限定为 input placeholder、`/help`、command palette、unknown-command，以及 Task 13 新增的 `/locale|keymap|vim|contrast` 回应；测试以 zh-Hans 行为断言这些 surface 不回落到硬编码英文。内部状态值（`running`/`ok`/`error`）仍为协议值，不翻译。

### Implementation

`internal/cli/tui/commands.go` 改动（保留真实的 `[]command`，把 help 文案改为 catalog key，并替换现有 `helpEntry`）：

```go
// command keeps the existing table shape and handler signature. helpKey is a
// catalog key rather than visible English, so both /help and the palette use
// the same localized source.
type command struct {
    name    string
    helpKey string
    run     func(m model, args []string) (tea.Model, tea.Cmd)
}

var commandTable = []command{
    {name: "help", helpKey: "tui.command.help.help", run: cmdHelp},
    {name: "model", helpKey: "tui.command.help.model", run: cmdModel},
    {name: "think", helpKey: "tui.command.help.think", run: cmdThink},
    {name: "mode", helpKey: "tui.command.help.mode", run: cmdMode},
    {name: "queue-mode", helpKey: "tui.command.help.queue_mode", run: cmdQueueMode},
    {name: "clear", helpKey: "tui.command.help.clear", run: cmdClear},
    {name: "config", helpKey: "tui.command.help.config", run: cmdConfig},
    {name: "cost", helpKey: "tui.command.help.cost", run: cmdCost},
    {name: "stats", helpKey: "tui.command.help.stats", run: cmdStats},
    {name: "compact", helpKey: "tui.command.help.compact", run: cmdCompact},
    {name: "mcp", helpKey: "tui.command.help.mcp", run: cmdMCP},
    {name: "sessions", helpKey: "tui.command.help.sessions", run: cmdSessions},
    {name: "restore", helpKey: "tui.command.help.restore", run: cmdRestore},
    {name: "rename", helpKey: "tui.command.help.rename", run: cmdRename},
    {name: "archive", helpKey: "tui.command.help.archive", run: cmdArchive},
    {name: "unarchive", helpKey: "tui.command.help.unarchive", run: cmdUnarchive},
    {name: "archived", helpKey: "tui.command.help.archived", run: cmdArchived},
    {name: "delete", helpKey: "tui.command.help.delete", run: cmdDelete},
    {name: "theme", helpKey: "tui.command.help.theme", run: cmdTheme},
}

func (m model) runCommand(text string) (tea.Model, tea.Cmd) {
    name, args := parseCommand(text)
    cmd, ok := lookupCommand(name)
    if !ok {
        m.entries = append(m.entries, errorEntry{text: m.bundle.GetF(
            "tui.error.unknown_command", map[string]string{"name": name},
        )})
        m.refresh()
        m.viewport.GotoBottom()
        return m, nil
    }
    return cmd.run(m, args)
}

func (m model) paletteBlock() string {
    if len(m.paletteItems) == 0 {
        return ""
    }
    rows := make([]string, 0, len(m.paletteItems))
    for i, c := range m.paletteItems {
        line := fmt.Sprintf("  /%-12s  %s", c.name, toolMeta.Render(m.bundle.Get(c.helpKey)))
        if i == m.paletteSel {
            rows = append(rows, selPaletteStyle.Render("▶ "+line))
        } else {
            rows = append(rows, paletteStyle.Render(line))
        }
    }
    return strings.Join(rows, "\n")
}

// cmdHelp renders the localized command list locally.
func cmdHelp(m model, _ []string) (tea.Model, tea.Cmd) {
    m.entries = append(m.entries, newCmdHelpEntry(m.bundle, commandTable))
    m.refresh()
    m.viewport.GotoBottom()
    return m, nil
}

// cmdHelpEntry replaces the old helpEntry (which was a bare struct{} with no
// bundle access). It pre-renders the slash-command table at construction
// time so the render call is just a string return — this avoids every entry
// needing to hold a bundle (the old design would have required threading
// bundle into the entry interface signature, affecting ~20 entry types).
//
// Why pre-render instead of pass bundle into render(): the render signature
// (render(width int, sp spinner.Model) string) is shared across many entry
// types; changing it would ripple. Pre-rendering localizes the i18n change
// to one entry type, and the table is static per startup (locale change
// requires restart, which matches existing /theme behavior).
type cmdHelpEntry struct {
    rows string
}

func newCmdHelpEntry(b *i18n.Bundle, table []command) *cmdHelpEntry {
    var sb strings.Builder
    sb.WriteString(roleAsst.Render("▌ " + b.Get("tui.command.help.title")) + "\n")
    for _, c := range table {
        sb.WriteString(b.GetF("tui.command.help.row", map[string]string{
            "name": c.name,
            "help": b.Get(c.helpKey),
        }) + "\n")
    }
    sb.WriteString("\n")
    return &cmdHelpEntry{rows: sb.String()}
}

func (e *cmdHelpEntry) render(_ int, _ spinner.Model) string {
    return e.rows
}
```

`internal/cli/tui/preferences.go`:

```go
package tui

import (
    "crypto/rand"
    "encoding/hex"
    "encoding/json"
    "fmt"
    "os"
    "path/filepath"
    "strconv"
    "strings"
)

// Preferences is one sparse preference layer. It is also the JSON shape for
// user-level TUI preferences persisted to disk. Pointer booleans distinguish
// unset (nil) from explicitly false, so a higher layer can disable a true value
// from project config without losing intent.
type Preferences struct {
    UILocale     string `json:"ui_locale,omitempty"`
    ThemeName    string `json:"theme,omitempty"`
    KeymapName   string `json:"keymap,omitempty"`
    HighContrast *bool  `json:"high_contrast,omitempty"`
    Vim          *bool  `json:"vim,omitempty"`
    // KeymapReset is a sparse user-level tombstone for project tui.bindings.
    // /keymap reset stores true so defaults survive the next startup; nil means
    // project bindings may still apply.
    KeymapReset  *bool  `json:"keymap_reset,omitempty"`
}

// EffectivePreferences is the fully merged, non-sparse result consumed by the
// TUI. It never contains tri-state values.
type EffectivePreferences struct {
    UILocale     string
    ThemeName    string
    KeymapName   string
    HighContrast bool
    Vim          bool
    KeymapReset  bool
}

// preferencesPersistDisabled mirrors persistPermMode: tests flip it to skip
// actual disk writes. See TestPreferences_CleanupRestoresState.
var preferencesPersistDisabled = false

// replacePreferencesFile is a test seam around the OS-specific atomic replace.
// Tests restore it with t.Cleanup; production points at the build-tagged helper.
var replacePreferencesFile = replacePreferencesFileOS

// loadPreferences reads one sparse user layer. A missing file returns an empty
// layer rather than forcing "auto", because an absent user value must not mask
// project config during the four-level merge.
func loadPreferences(path string) (Preferences, error) {
    var p Preferences
    data, err := os.ReadFile(path)
    if err != nil {
        if os.IsNotExist(err) {
            return p, nil
        }
        return p, err
    }
    if err := json.Unmarshal(data, &p); err != nil {
        return Preferences{}, fmt.Errorf("decode TUI preferences: %w", err)
    }
    return p, nil
}

// persistPreferences writes prefs atomically: .tmp.<rand> then rename. On
// rename failure the tmp file is removed and the existing prefs file is
// left untouched.
func persistPreferences(path string, p Preferences) error {
    if preferencesPersistDisabled {
        return nil
    }
    if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
        return err
    }
    data, err := json.Marshal(p)
    if err != nil {
        return err
    }
    suffix, err := randomSuffix()
    if err != nil {
        return err
    }
    tmp := path + ".tmp." + suffix
    if err := os.WriteFile(tmp, data, 0600); err != nil {
        return err
    }
    if err := replacePreferencesFile(tmp, path); err != nil {
        _ = os.Remove(tmp)
        return err
    }
    return nil
}

func randomSuffix() (string, error) {
    b := make([]byte, 8)
    if _, err := rand.Read(b); err != nil {
        return "", fmt.Errorf("generate preferences temp suffix: %w", err)
    }
    return hex.EncodeToString(b), nil
}

// mergeTUIPrefs applies sparse layers from lowest to highest priority:
// defaults < project config < user prefs < env < flags. All five fields are
// merged uniformly; pointer booleans make explicit false override lower true.
func mergeTUIPrefs(flags, env, user, project Preferences) EffectivePreferences {
    out := EffectivePreferences{
        UILocale: "auto",
        ThemeName: "default",
        KeymapName: "default",
    }
    apply := func(layer Preferences) {
        if layer.UILocale != "" {
            out.UILocale = layer.UILocale
        }
        if layer.ThemeName != "" {
            out.ThemeName = layer.ThemeName
        }
        if layer.KeymapName != "" {
            out.KeymapName = layer.KeymapName
        }
        if layer.HighContrast != nil {
            out.HighContrast = *layer.HighContrast
        }
        if layer.Vim != nil {
            out.Vim = *layer.Vim
        }
        if layer.KeymapReset != nil {
            out.KeymapReset = *layer.KeymapReset
        }
    }
    apply(project)
    apply(user)
    apply(env)
    apply(flags)
    return out
}

// preferencesPath mirrors permModeFile: os.UserConfigDir()/yanshi/prefs.json.
// Tests override via t.TempDir + direct call to loadPreferences/persistPreferences.
func preferencesPath() string {
    dir, err := os.UserConfigDir()
    if err != nil {
        dir = "."
    }
    return filepath.Join(dir, "yanshi", "prefs.json")
}

// PreferencesFromEnv constructs the sparse environment layer. Empty variables
// remain unset; invalid booleans fail startup instead of silently changing mode.
func PreferencesFromEnv(getenv func(string) string) (Preferences, error) {
    p := Preferences{
        UILocale:   strings.TrimSpace(getenv("YANSHI_UI_LOCALE")),
        ThemeName:  strings.TrimSpace(getenv("YANSHI_THEME")),
        KeymapName: strings.TrimSpace(getenv("YANSHI_KEYMAP")),
    }
    var err error
    if p.HighContrast, err = optionalBool(getenv("YANSHI_HIGH_CONTRAST")); err != nil {
        return Preferences{}, fmt.Errorf("YANSHI_HIGH_CONTRAST: %w", err)
    }
    if p.Vim, err = optionalBool(getenv("YANSHI_VIM")); err != nil {
        return Preferences{}, fmt.Errorf("YANSHI_VIM: %w", err)
    }
    return p, nil
}

func optionalBool(raw string) (*bool, error) {
    raw = strings.TrimSpace(raw)
    if raw == "" {
        return nil, nil
    }
    v, err := strconv.ParseBool(raw)
    if err != nil {
        return nil, fmt.Errorf("expected boolean, got %q", raw)
    }
    return &v, nil
}
```

`internal/cli/tui/preferences_replace_unix.go`:

```go
//go:build !windows

package tui

import "os"

// replacePreferencesFileOS atomically replaces dst on Unix; rename(2) keeps
// the old file intact if the operation fails.
func replacePreferencesFileOS(src, dst string) error { return os.Rename(src, dst) }
```

`internal/cli/tui/preferences_replace_windows.go`:

```go
//go:build windows

package tui

import (
    "errors"
    "os"

    "golang.org/x/sys/windows"
)

// replacePreferencesFileOS uses MoveFileEx with REPLACE_EXISTING so an existing
// prefs file can be atomically replaced on Windows. WRITE_THROUGH asks the OS to
// flush the move before reporting success.
func replacePreferencesFileOS(src, dst string) error {
    srcp, err := windows.UTF16PtrFromString(src)
    if err != nil {
        return err
    }
    dstp, err := windows.UTF16PtrFromString(dst)
    if err != nil {
        return err
    }
    err = windows.MoveFileEx(srcp, dstp,
        windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
    if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
        return os.ErrNotExist
    }
    return err
}
```

`internal/cli/tui/model.go` 构造接线（保留测试依赖的 `newModel(sess, root)` 与公开 `NewProgram(sess, root)`；variadic option 保持现有 caller source-compatible）：

```go
// ProgramOptions carries sparse launch layers into the TUI. User preferences
// remain owned here: NewProgramWithOptions loads PreferencesPath and returns any
// read/decode error. The compatibility NewProgram constructor deliberately uses
// built-in defaults only and never touches disk, so existing tests/callers remain
// deterministic.
type ProgramOptions struct {
    Flags           Preferences
    Env             Preferences
    Project         Preferences
    Bindings        map[string]string
    PreferencesPath string
}
```

在现有 `type model struct` 的 `status string` 后应用这个精确字段补丁（Task 13 会在同一 struct 继续加 `keymap` / `vim`）：

```diff
 	status string
+	bundle    *i18n.Bundle
+	prefs     Preferences
+	effective EffectivePreferences
+	prefsPath string
```

`newModel` 与 `NewProgram` 精确替换为：

```go
func defaultBundle() *i18n.Bundle {
    b, err := i18n.NewBundle("en")
    if err != nil {
        panic(err) // embedded en catalog is a build invariant
    }
    return b
}

func newModel(sess tuiSession, root string) model {
    return newModelWithPreferences(sess, root, defaultBundle(), Preferences{},
        EffectivePreferences{UILocale: "en", ThemeName: "default", KeymapName: "default"}, "")
}

func newModelWithPreferences(sess tuiSession, root string, b *i18n.Bundle,
    user Preferences, effective EffectivePreferences, prefsPath string) model {
    vp := viewport.New(80, 10)
    sp := spinner.New()
    sp.Spinner = spinner.Dot
    m := model{
        sess: sess, input: newInput(), viewport: vp, spinner: sp,
        status: root, workDir: dirName(root), gitBranch: detectGitBranch(root),
        rootPath: root, permMode: loadSavedMode(), autoThreshold: loadSavedThreshold(),
        queueMode: QueueModeQueue, theme: ThemeDefault,
        bundle: b, prefs: user, effective: effective, prefsPath: prefsPath,
    }
    m.input.Placeholder = b.Get("tui.input.placeholder")
    m.startupBanner = &startupEntry{info: buildStartupHeader()}
    m.entries = append(m.entries, m.startupBanner)
    m.refresh()
    return m
}

func NewProgram(sess *cli.Session, root string) *tea.Program {
    // Compatibility path: no preferencesPath(), no disk I/O, and therefore no
    // new panic surface for the existing constructor.
    b := defaultBundle()
    effective := EffectivePreferences{
        UILocale: "en", ThemeName: "default", KeymapName: "default",
    }
    m := newModelWithPreferences(sess, root, b, Preferences{}, effective, "")
    return tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
}

func NewProgramWithOptions(sess *cli.Session, root string, opts ProgramOptions) (*tea.Program, error) {
    if opts.PreferencesPath == "" {
        opts.PreferencesPath = preferencesPath()
    }
    user, err := loadPreferences(opts.PreferencesPath)
    if err != nil {
        return nil, err
    }
    effective := mergeTUIPrefs(opts.Flags, opts.Env, user, opts.Project)
    b, err := i18n.NewBundle(effective.UILocale)
    if err != nil {
        return nil, err
    }
    if _, ok := themeByName(ThemeName(effective.ThemeName)); !ok {
        return nil, fmt.Errorf("unsupported TUI theme %q", effective.ThemeName)
    }
    m := newModelWithPreferences(sess, root, b, user, effective, opts.PreferencesPath)
    return tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion()), nil
}
```

`cmd/yanshi/main.go` 添加可区分“未设置/explicit false”的 flag 类型，并在 `runDefault` 与 `chatTUI` 的现有 `FlagSet` 上调用 `bindTUIFlags`：

```go
type optionalBoolFlag struct {
    set   bool
    value bool
}

func (f *optionalBoolFlag) String() string {
    if !f.set {
        return ""
    }
    return strconv.FormatBool(f.value)
}
func (f *optionalBoolFlag) IsBoolFlag() bool { return true }
func (f *optionalBoolFlag) Set(raw string) error {
    v, err := strconv.ParseBool(raw)
    if err != nil {
        return err
    }
    f.set, f.value = true, v
    return nil
}

type tuiFlagValues struct {
    locale, theme, keymap string
    vim, contrast         optionalBoolFlag
}

func bindTUIFlags(fs *flag.FlagSet, v *tuiFlagValues) {
    fs.StringVar(&v.locale, "ui-locale", "", "UI locale: auto|en|zh-Hans")
    fs.StringVar(&v.theme, "theme", "", "TUI theme")
    fs.StringVar(&v.keymap, "keymap", "", "TUI keymap name")
    fs.Var(&v.vim, "vim", "enable/disable Vim mode")
    fs.Var(&v.contrast, "high-contrast", "enable/disable high contrast")
}

func (v tuiFlagValues) preferences() tui.Preferences {
    p := tui.Preferences{UILocale: v.locale, ThemeName: v.theme, KeymapName: v.keymap}
    if v.vim.set {
        p.Vim = &v.vim.value
    }
    if v.contrast.set {
        p.HighContrast = &v.contrast.value
    }
    return p
}
```

`cmd/yanshi/main_test.go` 锁定 optional bool 的三种输入；测试 import 增加 `flag`：

```go
func TestTUIOptionalBoolFlags(t *testing.T) {
    cases := []struct {
        name string
        args []string
        want *bool
    }{
        {name: "absent", args: nil, want: nil},
        {name: "bare true", args: []string{"--vim"}, want: boolPtrMain(true)},
        {name: "explicit false", args: []string{"--vim=false"}, want: boolPtrMain(false)},
    }
    for _, tc := range cases {
        t.Run(tc.name, func(t *testing.T) {
            fs := flag.NewFlagSet("tui-flags", flag.ContinueOnError)
            fs.SetOutput(io.Discard)
            var values tuiFlagValues
            bindTUIFlags(fs, &values)
            if err := fs.Parse(tc.args); err != nil {
                t.Fatal(err)
            }
            got := values.preferences().Vim
            if tc.want == nil {
                if got != nil {
                    t.Fatalf("Vim = %v, want nil", *got)
                }
                return
            }
            if got == nil || *got != *tc.want {
                t.Fatalf("Vim = %v, want %v", got, *tc.want)
            }
        })
    }
}

func boolPtrMain(v bool) *bool { return &v }
```

这里只承诺标准 `flag` 语法 `--vim`、`--vim=true`、`--vim=false`（`--high-contrast` 同理）；不声明未实现的 `--no-vim` / `--no-high-contrast`。

对 `runDefault` 与 `chatTUI` 分别应用相同的精确接线；两处都在 `fs.Parse(args)` 前 bind，在 `runTUI` 调用中传 flags：

```diff
 	inProcess := fs.Bool("inprocess", false, "force in-process backend")
+	var tuiFlags tuiFlagValues
+	bindTUIFlags(fs, &tuiFlags)
 	fs.Parse(args)
@@
-	if err := runTUI(ctx, cli.Options{
+	if err := runTUI(ctx, cli.Options{
 		ConfigPath: *configPath, FakeModel: *fakeModel,
 		Server: *server, InProcess: *inProcess,
-	}); err != nil {
+	}, tui.ProgramOptions{Flags: tuiFlags.preferences()}); err != nil {
```

把现有 `runTUI` 替换为以下实现；`config.Load` 失败由原有 `Session.Resolve` 路径负责报告，只有成功加载时才形成 project layer：

```go
func runTUI(ctx context.Context, opts cli.Options, supplied ...tui.ProgramOptions) error {
    sess := cli.NewSession(opts)
    if err := sess.Resolve(ctx); err != nil {
        return err
    }
    defer sess.Close()

    launch := tui.ProgramOptions{}
    if len(supplied) > 0 {
        launch = supplied[0]
    }
    envPrefs, err := tui.PreferencesFromEnv(os.Getenv)
    if err != nil {
        return err
    }
    launch.Env = envPrefs
    if cfg, err := config.Load(opts.ConfigPath); err == nil {
        launch.Project = tui.Preferences{
            UILocale: cfg.I18N.UILocale, ThemeName: cfg.TUI.Theme,
            KeymapName: cfg.TUI.KeymapName, HighContrast: cfg.TUI.HighContrast,
            Vim: cfg.TUI.Vim,
        }
        launch.Bindings = cfg.TUI.Bindings
    }
    prog, err := tui.NewProgramWithOptions(sess, sess.Root(), launch)
    if err != nil {
        return err
    }
    go func() {
        <-ctx.Done()
        prog.Quit()
    }()
    _, err = prog.Run()
    return err
}
```

`internal/agent/orchestrator/orchestrator.go` 把现有默认文案提升为唯一导出常量，并让 `New` 使用它：

```go
const DefaultInstruction = "You are yanshi's orchestrator. Use tools when helpful."

// In New:
instruction := cfg.Instruction
if instruction == "" {
    instruction = DefaultInstruction
}
```

`internal/bootstrap/bootstrap.go` 在读取 project prompt 后拼接**独立的**模型输出语言指令（绝不读取 `I18N.UILocale`）；没有 project prompt 时先使用上面的 canonical default，不能让语言 directive 替换默认 orchestrator 能力说明：

```go
func appendOutputLanguageInstruction(base, outputLanguage string) string {
    outputLanguage = strings.TrimSpace(outputLanguage)
    if outputLanguage == "" {
        return base
    }
    if strings.TrimSpace(base) == "" {
        base = orchestrator.DefaultInstruction
    }
    directive := "Respond to the user in " + outputLanguage +
        ". Keep code, commands, identifiers, file paths, and quoted source text unchanged."
    return strings.TrimRight(base, "\n") + "\n\n" + directive
}

// At the existing loadProjectPrompt call site:
instruction := appendOutputLanguageInstruction(
    loadProjectPrompt(workRoot), cfg.I18N.OutputLanguage,
)
```

`internal/bootstrap/bootstrap_test.go` 添加独立性门禁：

```go
func TestOutputLanguageInstructionIndependentOfUILocale(t *testing.T) {
    cfg := config.Config{
        I18N: config.I18NConfig{UILocale: "zh-Hans", OutputLanguage: "English"},
    }
    got := appendOutputLanguageInstruction("project rule", cfg.I18N.OutputLanguage)
    if !strings.Contains(got, "Respond to the user in English") {
        t.Fatalf("missing output-language directive: %q", got)
    }
    if strings.Contains(got, cfg.I18N.UILocale) {
        t.Fatalf("UI locale leaked into model directive: %q", got)
    }
    if empty := appendOutputLanguageInstruction("project rule", ""); empty != "project rule" {
        t.Fatalf("empty output language must follow user input: %q", empty)
    }
    withoutProject := appendOutputLanguageInstruction("", "English")
    if !strings.Contains(withoutProject, orchestrator.DefaultInstruction) ||
        !strings.Contains(withoutProject, "Respond to the user in English") {
        t.Fatalf("language directive replaced default instruction: %q", withoutProject)
    }
}
```

`internal/cli/tui/view_test.go` 以行为测试锁定选定本地化 surface（input placeholder、command help/palette、unknown-command）；machine status（如 `running`/`ok`/`error`）仍保持协议值，不直接作为可见文案：

```go
func TestView_SelectedSurfacesUseBundle(t *testing.T) {
    b, err := i18n.NewBundle("zh-Hans")
    if err != nil {
        t.Fatal(err)
    }
    effective := EffectivePreferences{UILocale: "zh-Hans", ThemeName: "default", KeymapName: "default"}
    m := newModelWithPreferences(&fakeSession{}, t.TempDir(), b, Preferences{}, effective, "")
    if got := m.input.Placeholder; got != b.Get("tui.input.placeholder") {
        t.Fatalf("placeholder: got %q", got)
    }

    m.input.SetValue("/")
    m.updatePalette()
    palette := m.paletteBlock()
    if !strings.Contains(palette, b.Get("tui.command.help.help")) || strings.Contains(palette, "list commands") {
        t.Fatalf("palette is not localized: %q", palette)
    }

    tm, _ := m.runCommand("/does-not-exist")
    rendered := renderLast(tm.(model))
    want := b.GetF("tui.error.unknown_command", map[string]string{"name": "does-not-exist"})
    if !strings.Contains(rendered, want) {
        t.Fatalf("unknown command: got %q want %q", rendered, want)
    }
}
```

`internal/cli/tui/view.go` 不新增另一套 locale 状态；输入 placeholder 在 `newModelWithPreferences` 设置，palette/help/error 的实际可见文本由上面的 `commands.go` 边界读取 `m.bundle`。

### Pass

```sh
go test ./internal/cli/tui -run "TestCmdHelpEntry|TestPreferences_|TestView_SelectedSurfacesUseBundle"
go test ./cmd/yanshi -run TestTUIOptionalBoolFlags
go test ./internal/bootstrap -run TestOutputLanguageInstructionIndependentOfUILocale
```

### Commit

```
feat(i18n): TUI bundle wiring + helpEntry pre-render + Preferences atomic (I18N1-L2)

helpEntry refactored from bare struct{} to cmdHelpEntry that pre-renders
localized rows at construction (avoids threading bundle into the render
signature of ~20 entry types). Preferences (loadPreferences/persistPreferences)
uses t.TempDir in tests + atomic tmp+rename pattern; preferencesPersistDisabled
mirrors persistPermMode. i18n.output_language is the unified config name for
the model output directive.
```

---

## Task 12 — C15-L1: keymap core (Binding/Action/Normalizer) + Vim tri-state + 高对比

**结构性修复落点:** #9 (Vim tri-state `*bool` 已由 Task 11 合并，本 Task 定义无歧义 modal result)、#16 (runtime normalization 委托本地 Bubble Tea fork 的 `KeyMsg.String()`；paste/multi-rune 明确不是 shortcut；raw override 排序后逐条验证，不先折叠 normalized map)。

### Files

- Create `internal/keymap/keymap.go`
- Create `internal/keymap/keymap_test.go`
- Create `internal/keymap/vim.go`
- Create `internal/keymap/vim_test.go`

> Task 12 只落可独立测试的 core。Bubble Tea `model.Update`、命令、doctor、theme 与 roadmap 接线全部属于 Task 13，避免本 Task 的 Files 声称修改尚未发生的文件。

### Failure Test (RED)

`internal/keymap/keymap_test.go`:

```go
package keymap

import (
    "strings"
    "testing"

    tea "github.com/charmbracelet/bubbletea"
)

// TestBuilder_ValidateAfterBuild (structural fix #16): the Builder collects
// all bindings via Add() and ONLY validates on Build(). This catches
// conflicts that incremental registration would miss (e.g., A registers
// ctrl+k, then B registers ctrl+k — incremental validation on A's add would
// pass because B hasn't been added yet).
func TestBuilder_ValidateAfterBuild(t *testing.T) {
    b := NewBuilder()
    b.Add("ctrl+k", ActionScrollUp)
    b.Add("ctrl+k", ActionScrollDown) // conflict

    _, err := b.Build()
    if err == nil {
        t.Fatal("Build must report conflict")
    }
    if !strings.Contains(err.Error(), "ctrl+k") {
        t.Fatalf("conflict error must name the key: %v", err)
    }
}

func TestBuilder_RejectsUnknownActionAfterCollection(t *testing.T) {
    b := NewBuilder()
    b.Add("ctrl+x", Action("launch_missiles"))
    if _, err := b.Build(); err == nil || !strings.Contains(err.Error(), "launch_missiles") {
        t.Fatalf("unknown action must be diagnosed, got %v", err)
    }
}

// Runtime spelling is owned by the checked-in Bubble Tea fork. NormalizeKey
// delegates to KeyMsg.String instead of maintaining a second KeyType switch.
func TestNormalizeKey_DelegatesToBubbleTeaString(t *testing.T) {
    cases := []tea.KeyMsg{
        {Type: tea.KeyCtrlK},
        {Type: tea.KeyEnter},
        {Type: tea.KeyCtrlEnter},
        {Type: tea.KeyPgUp},
        {Type: tea.KeyRunes, Runes: []rune("a"), Alt: true},
    }
    for _, msg := range cases {
        got, ok := NormalizeKey(msg)
        if !ok {
            t.Fatalf("NormalizeKey(%+v) unexpectedly rejected", msg)
        }
        if want := strings.ToLower(msg.String()); got != want {
            t.Fatalf("NormalizeKey(%+v) = %q, want fork String %q", msg, got, want)
        }
    }
}

func TestNormalizeKey_PasteAndMultiRuneAreNotShortcuts(t *testing.T) {
    cases := []tea.KeyMsg{
        {Type: tea.KeyRunes, Runes: []rune("j"), Paste: true},
        {Type: tea.KeyRunes, Runes: []rune("jk")},
    }
    for _, msg := range cases {
        if got, ok := NormalizeKey(msg); ok || got != "" {
            t.Fatalf("bulk input normalized as shortcut: %q ok=%v", got, ok)
        }
    }
}

func TestBuild_DefaultLookupUsesRealKeyMessages(t *testing.T) {
    m, err := NewDefaultBuilder(nil).Build()
    if err != nil {
        t.Fatal(err)
    }
    if got := m.Lookup(tea.KeyMsg{Type: tea.KeyCtrlK}); got != ActionScrollUp {
        t.Fatalf("ctrl+k -> %v want %v", got, ActionScrollUp)
    }
    if got := m.Lookup(tea.KeyMsg{Type: tea.KeyCtrlJ}); got != ActionScrollDown {
        t.Fatalf("ctrl+j -> %v want %v", got, ActionScrollDown)
    }
    if got := m.Lookup(tea.KeyMsg{Type: tea.KeyCtrlZ}); got != ActionNone {
        t.Fatalf("unbound key -> %v want ActionNone", got)
    }
}

// A Go map can contain raw spellings that normalize to the same runtime key.
// Sorting raw keys before AddOverride makes the diagnostic deterministic and
// avoids collapsing them in an intermediate normalized map.
func TestNewDefaultBuilder_DetectsNormalizedOverrideDuplicate(t *testing.T) {
    b := NewDefaultBuilder(map[string]string{
        "CTRL+K": "scroll_up",
        "ctrl+k": "scroll_down",
    })
    m, err := b.Build()
    if err == nil {
        t.Fatal("normalized duplicate must fail validation")
    }
    ds := m.Diagnostics()
    if len(ds) != 1 || ds[0].Kind != "normalized_duplicate" || ds[0].Key != "ctrl+k" {
        t.Fatalf("diagnostics = %#v", ds)
    }
}

func TestBuilder_RejectsInvalidConfigKey(t *testing.T) {
    b := NewDefaultBuilder(map[string]string{"ctrl+not-a-key": "send"})
    m, err := b.Build()
    if err == nil {
        t.Fatal("invalid key must fail validation")
    }
    if len(m.Diagnostics()) != 1 || m.Diagnostics()[0].Kind != "invalid_key" {
        t.Fatalf("diagnostics = %#v", m.Diagnostics())
    }
}
```

`internal/keymap/vim_test.go`:

```go
package keymap

import (
    "testing"
)

// TestVim_TriStateDistinguishesUnsetAndFalse (structural fix #9): *bool so
// we can tell "user did not configure" (nil) from "user explicitly disabled"
// (false). This affects whether the TUI falls back to prefs.json.
func TestVim_TriStateDistinguishesUnsetAndFalse(t *testing.T) {
    var vim *bool
    mode := effectiveVimMode(vim, true /*prefsDefault*/)
    if mode != true {
        t.Fatal("nil Vim + prefsDefault=true must yield true")
    }
    f := false
    mode = effectiveVimMode(&f, true)
    if mode != false {
        t.Fatal("Vim=false must override prefsDefault=true")
    }
    tt := true
    mode = effectiveVimMode(&tt, false)
    if mode != true {
        t.Fatal("Vim=true must override prefsDefault=false")
    }
}

func TestVim_ModalResultSeparatesActionFromConsumption(t *testing.T) {
    v := NewVimMachine()

    // Insert-mode printable input is not consumed by Vim; textarea receives it.
    got := v.HandleKey("j", ActionNone)
    if got.Action != ActionNone || got.Consumed {
        t.Fatalf("insert j = %#v, want literal passthrough", got)
    }

    // Escape and i/a/o are transitions: no semantic action, but the original
    // key is consumed so it is never inserted into the textarea.
    got = v.HandleKey("esc", ActionNone)
    if v.Mode() != VimModeNormal || got.Action != ActionNone || !got.Consumed {
        t.Fatalf("escape = %#v mode=%v", got, v.Mode())
    }
    for _, key := range []string{"i", "a", "o"} {
        v.SetMode(VimModeNormal)
        got = v.HandleKey(key, ActionNone)
        if v.Mode() != VimModeInsert || got.Action != ActionNone || !got.Consumed {
            t.Fatalf("%s transition = %#v mode=%v", key, got, v.Mode())
        }
    }

    v.SetMode(VimModeNormal)
    if got = v.HandleKey("j", ActionNone); got.Action != ActionScrollDown || !got.Consumed {
        t.Fatalf("normal j = %#v", got)
    }
    if got = v.HandleKey("k", ActionNone); got.Action != ActionScrollUp || !got.Consumed {
        t.Fatalf("normal k = %#v", got)
    }
}

func TestVim_VisualModeTransitionsOnly(t *testing.T) {
    v := NewVimMachine()
    v.SetMode(VimModeNormal)
    got := v.HandleKey("v", ActionNone)
    if v.Mode() != VimModeVisual || !got.Consumed {
        t.Fatalf("v = %#v mode=%v", got, v.Mode())
    }
    // D3 does not implement text-selection extension. j/k retain viewport
    // navigation, and unknown visual keys are consumed rather than typed.
    if got = v.HandleKey("j", ActionNone); got.Action != ActionScrollDown || !got.Consumed {
        t.Fatalf("visual j = %#v", got)
    }
    if got = v.HandleKey("x", ActionNone); got.Action != ActionNone || !got.Consumed {
        t.Fatalf("visual x = %#v", got)
    }
}
```

### Expected

- `Builder.Add(key, action)` 收集普通候选；`NewDefaultBuilder(overrides)` 先排序 raw override key，再逐条 `AddOverride`。default+override 是合法替换；两个不同 raw key 归一化到同一 key 时产生确定性的 `normalized_duplicate`。
- `NormalizeKey(msg) (string, bool)` 在拒绝 `msg.Paste` 和 multi-rune 后委托本仓库 Bubble Tea fork 的 `msg.String()`；不维护第二套 `tea.KeyType` switch。
- `Build()` 即使验证失败也返回 valid-subset `*Map` 和 error，诊断 kind 固定为 `conflict`、`unknown_action`、`invalid_key`、`normalized_duplicate`；Task 13 可保持 UI 可用并显示诊断。
- `/keymap reset` 不属于 core 的“清空诊断”：Task 13 必须重新构建 built-in defaults，并持久化 reset preference。
- Vim tri-state 由 Task 11 的 `Preferences.Vim *bool` 合并；core 只实现 modal machine。
- `VimResult{Action, Consumed}` 把“无 action 但吃掉模式切换键”与“无 action且交给 textarea”分开；`i/a/o` 进入 Insert 且 `Consumed=true`，Insert printable 为 `Consumed=false`。Visual 在 D3 只提供 mode + viewport j/k，不承诺 selection extension。

### Implementation

`internal/keymap/keymap.go`:

```go
// Package keymap provides the core keybinding primitives shared across TUI
// surfaces. It is a leaf package (depends only on bubbletea) so both
// internal/cli/tui and future headless consumers can reuse it.
//
// Design: Builder collects Add() calls WITHOUT validating; only Build()
// performs conflict + unknown-action checks. This catches conflicts that
// span across registration calls (A adds ctrl+k, B adds ctrl+k — an
// incremental validator would miss the second one).
package keymap

import (
    "fmt"
    "sort"
    "strings"
    "unicode"
    "unicode/utf8"

    tea "github.com/charmbracelet/bubbletea"
)

// Action is the semantic action a keybinding invokes.
type Action string

const (
    ActionNone        Action = ""
    ActionSend        Action = "send"
    ActionNewline     Action = "newline"
    ActionCancel      Action = "cancel"
    ActionScrollUp    Action = "scroll_up"
    ActionScrollDown  Action = "scroll_down"
    ActionClear       Action = "clear"
    ActionHelp        Action = "help"
    ActionQuit        Action = "quit"
    ActionCommandMode Action = "command_mode"
)

var knownActions = map[Action]struct{}{
    ActionSend: {}, ActionNewline: {}, ActionCancel: {},
    ActionScrollUp: {}, ActionScrollDown: {}, ActionClear: {},
    ActionHelp: {}, ActionQuit: {}, ActionCommandMode: {},
}

// Builder collects keybindings and validates the complete candidate set on Build.
// A binding records whether it came from built-in defaults or user overrides so a
// legitimate override replaces a default while two normalized override spellings
// remain a diagnostic.
type Builder struct {
    bindings []binding
}

type binding struct {
    normalized string
    raw        string
    action     Action
    override   bool
    normalizeErr error
}

func NewBuilder() *Builder { return &Builder{} }

// NewDefaultBuilder sorts raw override keys before normalization. Do not first
// copy them into a normalized map: doing so would silently discard CTRL+K versus
// ctrl+k and make duplicate diagnostics nondeterministic.
func NewDefaultBuilder(overrides map[string]string) *Builder {
    defaults := map[string]Action{
        "enter": ActionSend, "ctrl+enter": ActionNewline,
        "ctrl+c": ActionCancel, "ctrl+k": ActionScrollUp,
        "ctrl+j": ActionScrollDown, "ctrl+l": ActionClear,
        "f1": ActionHelp, "ctrl+q": ActionQuit,
    }
    defaultKeys := make([]string, 0, len(defaults))
    for key := range defaults {
        defaultKeys = append(defaultKeys, key)
    }
    sort.Strings(defaultKeys)

    b := NewBuilder()
    for _, key := range defaultKeys {
        b.add(key, defaults[key], false)
    }
    overrideKeys := make([]string, 0, len(overrides))
    for raw := range overrides {
        overrideKeys = append(overrideKeys, raw)
    }
    sort.Strings(overrideKeys)
    for _, raw := range overrideKeys {
        b.add(raw, Action(strings.TrimSpace(overrides[raw])), true)
    }
    return b
}

// Add adds a same-priority candidate. It is used by core tests and callers that
// intentionally want duplicate detection, not default replacement semantics.
func (b *Builder) Add(rawKey string, action Action) {
    b.add(rawKey, action, false)
}

func (b *Builder) add(rawKey string, action Action, override bool) {
    normalized, err := normalizeConfigKey(rawKey)
    b.bindings = append(b.bindings, binding{
        normalized: normalized,
        raw: rawKey,
        action: action,
        override: override,
        normalizeErr: err,
    })
}

// Build returns the valid subset and an aggregate validation error. Returning the
// map on error lets the TUI remain operable while rendering typed diagnostics.
func (b *Builder) Build() (*Map, error) {
    m := b.buildInternal()
    if len(m.diagnostics) == 0 {
        return m, nil
    }
    return m, fmt.Errorf("keymap: validation failed: %s",
        strings.Join(diagStrings(m.diagnostics), "; "))
}

func (b *Builder) buildInternal() *Map {
    lookup := map[string]Action{}
    sourceIsOverride := map[string]bool{}
    overrideRaw := map[string]string{}
    var diags []Diagnostic

    for _, bd := range b.bindings {
        if bd.normalizeErr != nil {
            diags = append(diags, Diagnostic{
                Kind: "invalid_key", Key: bd.raw,
            })
            continue
        }
        if _, ok := knownActions[bd.action]; !ok {
            diags = append(diags, Diagnostic{
                Kind: "unknown_action", Key: bd.raw,
                Actions: []Action{bd.action},
            })
            continue
        }
        existing, exists := lookup[bd.normalized]
        if !exists {
            lookup[bd.normalized] = bd.action
            sourceIsOverride[bd.normalized] = bd.override
            if bd.override {
                overrideRaw[bd.normalized] = bd.raw
            }
            continue
        }
        if bd.override && !sourceIsOverride[bd.normalized] {
            // One user override replacing a built-in default is valid.
            lookup[bd.normalized] = bd.action
            sourceIsOverride[bd.normalized] = true
            overrideRaw[bd.normalized] = bd.raw
            continue
        }
        kind := "conflict"
        if bd.override && sourceIsOverride[bd.normalized] {
            kind = "normalized_duplicate"
        }
        diags = append(diags, Diagnostic{
            Kind: kind, Key: bd.normalized,
            RawKeys: []string{overrideRaw[bd.normalized], bd.raw},
            Actions: []Action{existing, bd.action},
        })
        // Deterministic first-valid wins for duplicates; do not apply bd.
    }
    sort.Slice(diags, func(i, j int) bool {
        if diags[i].Kind == diags[j].Kind {
            return diags[i].Key < diags[j].Key
        }
        return diags[i].Kind < diags[j].Kind
    })
    return &Map{lookup: lookup, diagnostics: diags}
}

// Map is the validated runtime lookup plus immutable startup diagnostics.
type Map struct {
    lookup      map[string]Action
    diagnostics []Diagnostic
}

// Lookup returns ActionNone for paste, multi-rune input, invalid keys, or an
// unbound normalized key.
func (m *Map) Lookup(msg tea.KeyMsg) Action {
    if m == nil {
        return ActionNone
    }
    normalized, ok := NormalizeKey(msg)
    if !ok {
        return ActionNone
    }
    return m.lookup[normalized]
}

func (m *Map) Diagnostics() []Diagnostic {
    if m == nil {
        return nil
    }
    out := make([]Diagnostic, len(m.diagnostics))
    copy(out, m.diagnostics)
    for i := range out {
        out[i].RawKeys = append([]string(nil), out[i].RawKeys...)
        out[i].Actions = append([]Action(nil), out[i].Actions...)
    }
    return out
}

// Diagnostic.Kind is one of conflict, unknown_action, invalid_key, or
// normalized_duplicate. RawKeys preserves both user spellings where relevant.
type Diagnostic struct {
    Kind    string
    Key     string
    RawKeys []string
    Actions []Action
}

func diagStrings(ds []Diagnostic) []string {
    out := make([]string, len(ds))
    for i, d := range ds {
        acts := make([]string, len(d.Actions))
        for j, a := range d.Actions {
            acts[j] = string(a)
        }
        out[i] = fmt.Sprintf("%s:%s -> [%s]", d.Kind, d.Key,
            strings.Join(acts, ", "))
    }
    sort.Strings(out)
    return out
}

// NormalizeKey uses the checked-in Bubble Tea fork as the canonical runtime
// spelling. Paste and multi-rune KeyRunes are text payloads, never shortcuts.
func NormalizeKey(msg tea.KeyMsg) (string, bool) {
    if msg.Paste || (msg.Type == tea.KeyRunes && len(msg.Runes) != 1) {
        return "", false
    }
    normalized, err := normalizeConfigKey(msg.String())
    return normalized, err == nil
}

var supportedConfigKeys = map[string]struct{}{
    "enter": {}, "tab": {}, "shift+tab": {}, "backspace": {}, "esc": {},
    "up": {}, "down": {}, "left": {}, "right": {}, "home": {}, "end": {},
    "pgup": {}, "pgdown": {}, "delete": {}, "insert": {}, "ctrl+enter": {},
    "ctrl+up": {}, "ctrl+down": {}, "ctrl+left": {}, "ctrl+right": {},
    "ctrl+home": {}, "ctrl+end": {}, "ctrl+pgup": {}, "ctrl+pgdown": {},
    "ctrl+\": {}, "ctrl+]": {}, "ctrl+_": {}, "ctrl+^": {},
    "f1": {}, "f2": {}, "f3": {}, "f4": {}, "f5": {}, "f6": {},
    "f7": {}, "f8": {}, "f9": {}, "f10": {}, "f11": {}, "f12": {},
}

// normalizeConfigKey validates the documented configuration grammar. It accepts
// exact special-key names, ctrl+a..ctrl+z, one printable rune, or alt+<rune>.
func normalizeConfigKey(raw string) (string, error) {
    s := strings.ToLower(strings.TrimSpace(raw))
    switch s {
    case "escape":
        s = "esc"
    case "return":
        s = "enter"
    }
    if _, ok := supportedConfigKeys[s]; ok {
        return s, nil
    }
    if len(s) == len("ctrl+a") && strings.HasPrefix(s, "ctrl+") &&
        s[len(s)-1] >= 'a' && s[len(s)-1] <= 'z' {
        return s, nil
    }
    if utf8.RuneCountInString(s) == 1 {
        r, _ := utf8.DecodeRuneInString(s)
        if unicode.IsPrint(r) {
            return s, nil
        }
    }
    if strings.HasPrefix(s, "alt+") {
        tail := strings.TrimPrefix(s, "alt+")
        if utf8.RuneCountInString(tail) == 1 {
            r, _ := utf8.DecodeRuneInString(tail)
            if unicode.IsPrint(r) {
                return s, nil
            }
        }
    }
    return "", fmt.Errorf("unsupported key %q", raw)
}
```

`internal/keymap/vim.go`:

```go
package keymap

// VimMode is the current modal state.
type VimMode int

const (
    VimModeNormal VimMode = iota
    VimModeInsert
    VimModeVisual
)

// VimMachine is the modal state machine. D3 Visual mode tracks mode and maps
// j/k to viewport navigation only; it does not claim text-selection extension.
type VimMachine struct {
    mode VimMode
}

func NewVimMachine() *VimMachine { return &VimMachine{mode: VimModeInsert} }
func (v *VimMachine) Mode() VimMode { return v.mode }
func (v *VimMachine) SetMode(m VimMode) { v.mode = m }

// VimResult separates semantic dispatch from input ownership. Consumed=true with
// ActionNone is load-bearing for i/a/o/v/Esc: the transition key must not leak
// into the textarea.
type VimResult struct {
    Action   Action
    Consumed bool
}

func (v *VimMachine) HandleKey(key string, configured Action) VimResult {
    // Explicit configured actions remain global in every mode.
    if configured != ActionNone {
        return VimResult{Action: configured, Consumed: true}
    }
    switch v.mode {
    case VimModeInsert:
        if key == "esc" {
            v.mode = VimModeNormal
            return VimResult{Consumed: true}
        }
        return VimResult{}
    case VimModeNormal:
        switch key {
        case "i", "a", "o":
            v.mode = VimModeInsert
            return VimResult{Consumed: true}
        case "v":
            v.mode = VimModeVisual
            return VimResult{Consumed: true}
        case "j":
            return VimResult{Action: ActionScrollDown, Consumed: true}
        case "k":
            return VimResult{Action: ActionScrollUp, Consumed: true}
        default:
            return VimResult{Consumed: true}
        }
    case VimModeVisual:
        switch key {
        case "esc":
            v.mode = VimModeNormal
            return VimResult{Consumed: true}
        case "j":
            return VimResult{Action: ActionScrollDown, Consumed: true}
        case "k":
            return VimResult{Action: ActionScrollUp, Consumed: true}
        default:
            return VimResult{Consumed: true}
        }
    default:
        return VimResult{}
    }
}

// effectiveVimMode implements the *bool tri-state merge (structural fix #9):
//   - nil (unset): use prefsDefault
//   - false (explicit): force off
//   - true (explicit): force on
func effectiveVimMode(flag *bool, prefsDefault bool) bool {
    if flag == nil {
        return prefsDefault
    }
    return *flag
}
```

### Pass

```sh
go test ./internal/keymap -run "TestBuilder|TestNormalizeKey|TestBuild_|TestNewDefaultBuilder|TestVim"
```

### Commit

```
feat(keymap): core Binding/Action/Normalizer + Vim tri-state (C15-L1)

Builder validates the full candidate list after sorting and normalizing every
raw override; normalized duplicates cannot disappear in an intermediate map.
NormalizeKey delegates runtime spelling to the local Bubble Tea fork and rejects
paste/multi-rune text. VimMachine returns VimResult{Action,Consumed}, so i/a/o
transitions are consumed while Insert-mode printable input reaches textarea.
```

---

## Task 13 — C15-L2: TUI adapter + `/keymap` diagnostics + doctor + OBS3 豁免同步

**结构性修复落点:** #8 (C15/OBS3 正式豁免并同步 roadmap 的真实条目)、#14 (测试用真实 `recordingSession.sentText` + `&fakeSession{}`)、#16 (paste guard 先于 semantic dispatch；`/keymap reset` 重建 defaults 并持久化 tombstone；diagnostic kind 本地化；modal `Consumed` 全行为)、#17 (Files 与实际修改一致)。

### Files

- Create `internal/cli/tui/keymap.go`
- Create `internal/cli/tui/keymap_test.go`
- Modify `internal/cli/tui/model.go` (model 增加 `keys *keymapAdapter`, KeyMsg 分支改 dispatch)
- Modify `internal/cli/tui/commands.go` (新增 `/keymap`, `/vim`, `/contrast`)
- Modify `internal/cli/tui/commands_test.go` (命令测试用 `recordingSession.sentText`)
- Modify `internal/cli/tui/styles.go` (启动时应用 `ThemeHighContrast`，关闭时恢复 effective base theme)
- Modify `internal/cli/tui/styles_test.go` (contrast on/off 恢复行为)
- Modify `internal/cli/doctor.go` (在真实 `RunDoctor` report 加 D3 五项检查)
- Modify `internal/cli/doctor_test.go` (D3 check status/无 secret 输出)
- Modify `cmd/yanshi/main_test.go` (通过真实 `runDoctor` 渲染路径断言)
- Modify `docs/feature-roadmap-codex-deepseek.md` (OBS3 豁免同步)

### Failure Test (RED)

`internal/cli/tui/keymap_test.go`:

```go
package tui

import (
    "strings"
    "testing"

    tea "github.com/charmbracelet/bubbletea"
    "github.com/x6nux/yanshi/internal/i18n"
    core "github.com/x6nux/yanshi/internal/keymap"
)

func TestKeymapAdapter_DispatchAndBulkInput(t *testing.T) {
    a, err := newKeymapAdapter("default", false, nil)
    if err != nil {
        t.Fatal(err)
    }
    got := a.resultFor(tea.KeyMsg{Type: tea.KeyCtrlK})
    if got.Action != core.ActionScrollUp || !got.Consumed {
        t.Fatalf("ctrl+k = %#v", got)
    }
    for _, msg := range []tea.KeyMsg{
        {Type: tea.KeyRunes, Runes: []rune("j"), Paste: true},
        {Type: tea.KeyRunes, Runes: []rune("jk")},
    } {
        got = a.resultFor(msg)
        if got.Action != core.ActionNone || got.Consumed {
            t.Fatalf("bulk input triggered shortcut: %#v", got)
        }
    }
}

func TestKeymapAdapter_OverrideDiagnosticsAndUnsupportedName(t *testing.T) {
    a, err := newKeymapAdapter("default", false, map[string]string{
        "ctrl+k": "scroll_down",
        "ctrl+x": "launch_missiles",
    })
    if err != nil {
        t.Fatalf("binding diagnostics are non-fatal at TUI startup: %v", err)
    }
    if got := a.resultFor(tea.KeyMsg{Type: tea.KeyCtrlK}); got.Action != core.ActionScrollDown {
        t.Fatalf("configured override ctrl+k = %#v", got)
    }
    if len(a.diagnostics()) == 0 || a.diagnostics()[0].Kind != "unknown_action" {
        t.Fatalf("missing unknown-action diagnostic: %#v", a.diagnostics())
    }

    if _, err := newKeymapAdapter("emacs", false, nil); err == nil {
        t.Fatal("unsupported keymap name must fail explicitly")
    }
}

func TestKeymapAdapter_VimConsumption(t *testing.T) {
    a, _ := newKeymapAdapter("default", true, nil)
    a.vim.SetMode(core.VimModeNormal)

    got := a.resultFor(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("i")})
    if got.Action != core.ActionNone || !got.Consumed || a.vim.Mode() != core.VimModeInsert {
        t.Fatalf("normal i = %#v mode=%v", got, a.vim.Mode())
    }
    got = a.resultFor(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
    if got.Action != core.ActionNone || got.Consumed {
        t.Fatalf("insert j = %#v, want textarea passthrough", got)
    }
}

func TestKeymapAdapter_ResetRestoresBuiltins(t *testing.T) {
    a, _ := newKeymapAdapter("default", false,
        map[string]string{"ctrl+k": "scroll_down"})
    if got := a.resultFor(tea.KeyMsg{Type: tea.KeyCtrlK}); got.Action != core.ActionScrollDown {
        t.Fatalf("precondition ctrl+k = %#v", got)
    }
    a.resetDefaults()
    if got := a.resultFor(tea.KeyMsg{Type: tea.KeyCtrlK}); got.Action != core.ActionScrollUp {
        t.Fatalf("reset ctrl+k = %#v, want built-in scroll_up", got)
    }
    if len(a.diagnostics()) != 0 {
        t.Fatalf("reset left diagnostics: %#v", a.diagnostics())
    }
}

func TestKeymapAdapter_DiagnosticKindsLocalized(t *testing.T) {
    bundle, err := i18n.NewBundle("zh-Hans")
    if err != nil {
        t.Fatal(err)
    }
    cases := []core.Diagnostic{
        {Kind: "conflict", Key: "ctrl+k", Actions: []core.Action{core.ActionScrollUp, core.ActionScrollDown}},
        {Kind: "unknown_action", Key: "ctrl+x", Actions: []core.Action{"bad"}},
        {Kind: "normalized_duplicate", Key: "ctrl+k", RawKeys: []string{"CTRL+K", "ctrl+k"}},
        {Kind: "invalid_key", Key: "ctrl+bad"},
        {Kind: "unsupported_keymap", Key: "emacs"},
    }
    for _, d := range cases {
        a := &keymapAdapter{extraDiagnostics: []core.Diagnostic{d}}
        out := a.diagnosticsText(bundle)
        if out == "" || strings.Contains(out, "keymap.diagnostics.") {
            t.Fatalf("kind %s not localized: %q", d.Kind, out)
        }
    }
}

func TestModelUpdate_PastePrecedesSemanticDispatch(t *testing.T) {
    m := newModel(&fakeSession{}, t.TempDir())
    m.keys, _ = newKeymapAdapter("default", true, nil)
    m.keys.vim.SetMode(core.VimModeNormal)
    m.input.Focus()

    next, _ := m.Update(tea.KeyMsg{
        Type: tea.KeyRunes, Runes: []rune("j"), Paste: true,
    })
    m = next.(model)
    if got := m.input.Value(); got != "j" {
        t.Fatalf("pasted j was consumed as Vim navigation: %q", got)
    }
}

func TestModelUpdate_VimTransitionConsumesOriginalRune(t *testing.T) {
    m := newModel(&fakeSession{}, t.TempDir())
    m.keys, _ = newKeymapAdapter("default", true, nil)
    m.keys.vim.SetMode(core.VimModeNormal)
    m.input.Focus()

    next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("i")})
    m = next.(model)
    if m.keys.vim.Mode() != core.VimModeInsert || m.input.Value() != "" {
        t.Fatalf("i transition leaked into input: mode=%v input=%q",
            m.keys.vim.Mode(), m.input.Value())
    }
}
```

`internal/cli/tui/commands_test.go` 的 import delta 明确增加 `path/filepath`、Bubble Tea alias `tea` 和 `corekeymap`；命令路由使用真实测试替身：

```go
func TestCommandKeymapResetRestoresDefaultsAndPersists(t *testing.T) {
    r := &recordingSession{}
    m := newModel(r, "/proj")
    m.prefsPath = filepath.Join(t.TempDir(), "prefs.json")
    m.keys, _ = newKeymapAdapter("default", false,
        map[string]string{"ctrl+k": "scroll_down"})

    m.input.SetValue("/keymap reset")
    mm, _ := m.submit()
    m = mm.(model)
    got := m.keys.resultFor(tea.KeyMsg{Type: tea.KeyCtrlK})
    if got.Action != corekeymap.ActionScrollUp {
        t.Fatalf("/keymap reset ctrl+k = %#v", got)
    }
    if m.prefs.KeymapReset == nil || !*m.prefs.KeymapReset ||
        m.prefs.KeymapName != "default" {
        t.Fatalf("reset preference not recorded: %#v", m.prefs)
    }
    persisted, err := loadPreferences(m.prefsPath)
    if err != nil {
        t.Fatal(err)
    }
    if persisted.KeymapReset == nil || !*persisted.KeymapReset {
        t.Fatalf("reset tombstone not persisted: %#v", persisted)
    }
    if len(r.sentText) != 0 {
        t.Fatalf("/keymap reset was sent to model: %v", r.sentText)
    }
}

func TestCommandVimExplicitFalse(t *testing.T) {
    m := newModel(&fakeSession{}, "/proj")
    m.keys.vimEnabled = true
    mm, _ := cmdVim(m, []string{"off"})
    m = mm.(model)
    if m.keys.vimEnabled {
        t.Fatal("explicit Vim=false must disable Vim (not fall back)")
    }
    if m.prefs.Vim == nil || *m.prefs.Vim {
        t.Fatalf("persisted Vim tri-state: %#v", m.prefs.Vim)
    }
}

func TestCommandContrastOffRestoresBaseTheme(t *testing.T) {
    m := newModel(&fakeSession{}, "/proj")
    m.effective.ThemeName = string(ThemeMuted)
    m.effective.HighContrast = true
    m.theme = ThemeHighContrast

    mm, _ := cmdContrast(m, []string{"off"})
    m = mm.(model)
    if m.theme != ThemeMuted || m.effective.HighContrast {
        t.Fatalf("contrast off: theme=%q effective=%v", m.theme,
            m.effective.HighContrast)
    }
}
```

`cmd/yanshi/main_test.go` — `TestDoctorD3Checks`:

```go
func TestDoctorD3Checks(t *testing.T) {
    dir := t.TempDir()
    cfgPath := filepath.Join(dir, "config.yaml")
    sentinel := "sk-doctor-render-sentinel"
    t.Setenv("YANSHI_DOCTOR_KEY", sentinel)
    configText := fmt.Sprintf(`
storage:
  sqlite_path: %q
server:
  http_addr: 127.0.0.1:0
secrets:
  backend: none
i18n:
  ui_locale: auto
tui:
  keymap: default
  high_contrast: true
llm:
  providers:
    - name: doctor
      kind: openai
      model: fake
      api_key: env://YANSHI_DOCTOR_KEY
`, filepath.Join(dir, "doctor.db"))
    if err := os.WriteFile(cfgPath, []byte(configText), 0600); err != nil {
        t.Fatal(err)
    }

    var out, errOut bytes.Buffer
    code := runDoctor(context.Background(), []string{"--config", cfgPath}, &out, &errOut)
    if code == 2 {
        t.Fatalf("doctor failed: stdout=%s stderr=%s", out.String(), errOut.String())
    }
    if strings.Contains(out.String(), sentinel) || strings.Contains(errOut.String(), sentinel) {
        t.Fatalf("doctor rendered secret: stdout=%q stderr=%q",
            out.String(), errOut.String())
    }
    for _, want := range []string{"secrets", "auth", "locale", "keymap", "high-contrast"} {
        if !strings.Contains(out.String(), want) {
            t.Errorf("doctor output missing %q: %s", want, out.String())
        }
    }
}
```

### Expected

- `newKeymapAdapter(keymapName, vimEnabled, overrides)` 只接受 `keymapName="default"`；其它名称返回显式 error。binding validation error 留在 typed diagnostics 中，UI 使用 valid subset，doctor 则 FAIL。
- `model.Update(tea.KeyMsg)` 保持 restore/picker/permission/YOLO/palette modal 最高优先级；之后先识别 paste/multi-rune 并交给 textarea，再调用 semantic adapter。`VimResult.Consumed` 决定是否继续 textarea，不能仅看 `ActionNone`。
- `/keymap diagnostics` 按每个 `Diagnostic.Kind` 使用独立 catalog key；`/keymap reset` 重建 built-in defaults、清除 diagnostics、设置 `prefs.KeymapName="default"` + `prefs.KeymapReset=true` 并先持久化再改 runtime，绝不发送到 model。
- `/vim on|off` 持久化 `prefs.Vim = *bool`；`/contrast on|off` 持久化，on 应用 `ThemeHighContrast`，off 恢复 `m.effective.ThemeName` 指向的 base theme（不是无条件 `ThemeDefault`）。
- doctor 输出 secrets/auth/locale/keymap/high-contrast 五项检查；keymap check 同时验证 name 和全部 raw overrides。
- roadmap 精确修改现有 OBS3/C15 条目的依赖/设计/验收行，不追加与文件格式脱节的新 bullet。

### Implementation

`internal/cli/tui/keymap.go`:

```go
package tui

import (
    "fmt"
    "strings"

    tea "github.com/charmbracelet/bubbletea"
    "github.com/x6nux/yanshi/internal/i18n"
    core "github.com/x6nux/yanshi/internal/keymap"
)

type keymapAdapter struct {
    active           *core.Map
    vim              *core.VimMachine
    vimEnabled       bool
    extraDiagnostics []core.Diagnostic
}

// newKeymapAdapter treats an unsupported named map as a fatal configuration
// error. Binding-level validation is non-fatal in the TUI: Build returns a valid
// subset and typed diagnostics, while doctor reports the same error as FAIL.
func newKeymapAdapter(
    keymapName string,
    vimEnabled bool,
    overrides map[string]string,
) (*keymapAdapter, error) {
    if keymapName == "" {
        keymapName = "default"
    }
    if keymapName != "default" {
        return nil, fmt.Errorf("unsupported keymap %q", keymapName)
    }
    active, _ := core.NewDefaultBuilder(overrides).Build()
    return &keymapAdapter{
        active: active, vim: core.NewVimMachine(), vimEnabled: vimEnabled,
    }, nil
}

func (a *keymapAdapter) resultFor(msg tea.KeyMsg) core.VimResult {
    raw, ok := core.NormalizeKey(msg)
    if !ok {
        return core.VimResult{}
    }
    configured := a.active.Lookup(msg)
    if !a.vimEnabled {
        return core.VimResult{
            Action: configured, Consumed: configured != core.ActionNone,
        }
    }
    return a.vim.HandleKey(raw, configured)
}

func (a *keymapAdapter) diagnostics() []core.Diagnostic {
    if a == nil {
        return nil
    }
    out := append([]core.Diagnostic(nil), a.extraDiagnostics...)
    if a.active != nil {
        out = append(out, a.active.Diagnostics()...)
    }
    return out
}

func (a *keymapAdapter) resetDefaults() {
    active, err := core.NewDefaultBuilder(nil).Build()
    if err != nil {
        panic(err) // built-in defaults are a package invariant
    }
    a.active = active
    a.extraDiagnostics = nil
}

func (a *keymapAdapter) diagnosticsText(b *i18n.Bundle) string {
    ds := a.diagnostics()
    if len(ds) == 0 {
        return b.Get("tui.command.keymap.none")
    }
    rows := make([]string, 0, len(ds))
    for _, d := range ds {
        vars := map[string]string{"key": d.Key, "name": d.Key}
        if len(d.Actions) > 0 {
            vars["action"] = string(d.Actions[0])
            vars["a"] = string(d.Actions[0])
        }
        if len(d.Actions) > 1 {
            vars["b"] = string(d.Actions[1])
        }
        key := "keymap.diagnostics." + d.Kind
        switch d.Kind {
        case "conflict", "unknown_action", "normalized_duplicate",
            "invalid_key", "unsupported_keymap":
            rows = append(rows, b.GetF(key, vars))
        default:
            rows = append(rows, b.GetF("keymap.diagnostics.invalid_key",
                map[string]string{"key": d.Key}))
        }
    }
    return strings.Join(rows, "\n")
}
```

`internal/cli/tui/model.go` 增加 adapter 字段并在 `NewProgramWithOptions` 中用最终 effective prefs + config bindings 初始化；测试用的 `newModel` 也必须得到非 nil adapter：

```diff
 	prefsPath string
+	keys      *keymapAdapter
```

```go
// In newModelWithPreferences, immediately after the model literal:
keys, err := newKeymapAdapter(effective.KeymapName, effective.Vim, nil)
if err != nil {
    panic(err) // built-in newModel uses only keymapName=default
}
m.keys = keys
if effective.HighContrast {
    m.theme = ThemeHighContrast
} else if named, ok := themeByName(ThemeName(effective.ThemeName)); ok {
    m.theme = named.Name
}

// In NewProgramWithOptions, immediately after newModelWithPreferences:
overrides := opts.Bindings
if effective.KeymapReset {
    overrides = nil // persisted /keymap reset masks lower project bindings
}
keys, err := newKeymapAdapter(effective.KeymapName, effective.Vim, overrides)
if err != nil {
    return nil, err
}
m.keys = keys
```

把下面的 helper 放进 `model.go`；它只调用真实字段/API（`m.sess.CancelCurrent`、`cmdClear`、`cmdHelp`、`viewport.LineUp/LineDown`）：

```go
func (m model) dispatchKeymapAction(msg tea.KeyMsg) (tea.Model, tea.Cmd, bool) {
    result := m.keys.resultFor(msg)
    if !result.Consumed {
        return m, nil, false
    }
    switch result.Action {
    case corekeymap.ActionSend:
        mm, cmd := m.submit()
        return mm, cmd, true
    case corekeymap.ActionNewline:
        m.input.InsertString("\n")
        m.growInput()
        m.reflow()
        return m, nil, true
    case corekeymap.ActionCancel:
        if m.streamCh != nil && !m.canceling {
            _ = m.sess.CancelCurrent()
            m.canceling = true
            return m, nil, true
        }
        return m, tea.Quit, true
    case corekeymap.ActionScrollUp:
        m.viewport.LineUp(1)
        return m, tea.Repaint, true
    case corekeymap.ActionScrollDown:
        m.viewport.LineDown(1)
        return m, tea.Repaint, true
    case corekeymap.ActionClear:
        mm, cmd := cmdClear(m, nil)
        return mm, cmd, true
    case corekeymap.ActionHelp:
        mm, cmd := cmdHelp(m, nil)
        return mm, cmd, true
    case corekeymap.ActionQuit:
        return m, tea.Quit, true
    default:
        // Consumed mode transitions (i/a/o/v/Esc) intentionally have ActionNone.
        return m, nil, true
    }
}
```

在现有 `case tea.KeyMsg:` 中保留并先执行 restore picker、model/mode/theme picker、permission popup、YOLO confirmation、command palette 的 modal 分支；它们必须继续优先消费按键。删除全局 switch 中 `KeyCtrlC`、`KeyEnter`（modal Enter 分支之后的 submit）、`KeyCtrlEnter` 三个硬编码分支。随后先放 bulk-input guard，再放唯一 semantic dispatch：

```go
// Paste/multi-rune input is text, never a shortcut. This guard must be after
// modal popups but before dispatchKeymapAction.
if msg.Type == tea.KeyRunes && (msg.Paste || len(msg.Runes) != 1) {
    m.inputPasted = m.inputPasted || msg.Paste ||
        len(msg.Runes) > bulkPasteRuneThreshold
    var cmd tea.Cmd
    m.input, cmd = m.input.Update(msg)
    m.growInput()
    m.reflow()
    return m, cmd
}
if mm, cmd, handled := m.dispatchKeymapAction(msg); handled {
    return mm, cmd
}
```

保留现有 `KeyPgUp/KeyPgDown` page-scroll、`KeyCtrlO` expand 等未进入 C15 action 词表的分支，但把它们排在 modal 之后、semantic dispatch 的适当位置：PgUp/PgDown 与 Ctrl+O 的既有专用行为不被 textarea 吞掉。删除后方旧 paste detection，避免同一个事件处理两次。这样不会引用不存在的 `execLocalCommand`，也不会把 palette/permission 的 Enter 错交给 `submit`。

`internal/cli/tui/commands.go` 在 `commandTable` 的 `theme` 行后增加真实 `command` 条目：

```go
    {name: "locale", helpKey: "tui.command.help.locale", run: cmdLocale},
    {name: "keymap", helpKey: "tui.command.help.keymap", run: cmdKeymap},
    {name: "vim", helpKey: "tui.command.help.vim", run: cmdVim},
    {name: "contrast", helpKey: "tui.command.help.contrast", run: cmdContrast},
```

命令实现使用仓库已有的 `summaryEntry` / `errorEntry`、`m.refresh()`、`m.viewport.GotoBottom()`；不存在 `textEntry` 或 `execLocalCommand`：

```go
func appendNotice(m model, text string) (tea.Model, tea.Cmd) {
    m.entries = append(m.entries, summaryEntry{text: text})
    m.refresh()
    m.viewport.GotoBottom()
    return m, nil
}

func appendCommandError(m model, text string) (tea.Model, tea.Cmd) {
    m.entries = append(m.entries, errorEntry{text: text})
    m.refresh()
    m.viewport.GotoBottom()
    return m, nil
}

// savePreferences commits disk first. On failure the model retains both its
// previous sparse prefs and previous runtime state.
func savePreferences(m model, next Preferences) error {
    if m.prefsPath == "" {
        return nil // unit-test/newModel path; NewProgramWithOptions always sets it
    }
    return persistPreferences(m.prefsPath, next)
}

func cmdLocale(m model, args []string) (tea.Model, tea.Cmd) {
    if len(args) == 0 {
        return appendNotice(m, m.bundle.GetF("tui.command.locale.current",
            map[string]string{"locale": m.bundle.Persistent()}))
    }
    if len(args) != 1 {
        return appendCommandError(m, m.bundle.Get("tui.command.locale.usage"))
    }
    nextBundle, err := i18n.NewBundle(args[0])
    if err != nil {
        return appendCommandError(m, m.bundle.Get("tui.command.locale.usage"))
    }
    next := m.prefs
    next.UILocale = nextBundle.Persistent()
    if err := savePreferences(m, next); err != nil {
        return appendCommandError(m, m.bundle.GetF(
            "tui.command.preference.persist_failed", map[string]string{"error": err.Error()}))
    }
    m.prefs = next
    m.bundle = nextBundle
    m.effective.UILocale = nextBundle.Persistent()
    m.input.Placeholder = nextBundle.Get("tui.input.placeholder")
    return appendNotice(m, nextBundle.GetF("tui.command.locale.changed",
        map[string]string{"locale": nextBundle.Persistent()}))
}

func cmdKeymap(m model, args []string) (tea.Model, tea.Cmd) {
    if len(args) == 0 || (len(args) == 1 && args[0] == "diagnostics") {
        return appendNotice(m, m.keys.diagnosticsText(m.bundle))
    }
    if len(args) == 1 && args[0] == "reset" {
        reset := true
        next := m.prefs
        next.KeymapName = "default"
        next.KeymapReset = &reset
        if err := savePreferences(m, next); err != nil {
            return appendCommandError(m, m.bundle.GetF(
                "tui.command.preference.persist_failed",
                map[string]string{"error": err.Error()}))
        }
        // Disk commit succeeded; now mutate runtime state.
        m.prefs = next
        m.effective.KeymapName = "default"
        m.effective.KeymapReset = true
        m.keys.resetDefaults()
        return appendNotice(m, m.bundle.Get("tui.command.keymap.reset"))
    }
    return appendCommandError(m, m.bundle.Get("tui.command.keymap.usage"))
}

func cmdVim(m model, args []string) (tea.Model, tea.Cmd) {
    if len(args) == 0 {
        key := "tui.command.vim.disabled"
        if m.keys.vimEnabled {
            key = "tui.command.vim.enabled"
        }
        return appendNotice(m, m.bundle.Get(key))
    }
    if len(args) != 1 || (args[0] != "on" && args[0] != "off") {
        return appendCommandError(m, m.bundle.Get("tui.command.vim.usage"))
    }
    enabled := args[0] == "on"
    next := m.prefs
    next.Vim = &enabled
    if err := savePreferences(m, next); err != nil {
        return appendCommandError(m, m.bundle.GetF(
            "tui.command.preference.persist_failed", map[string]string{"error": err.Error()}))
    }
    m.prefs = next
    m.effective.Vim = enabled
    m.keys.vimEnabled = enabled
    key := "tui.command.vim.disabled"
    if enabled {
        key = "tui.command.vim.enabled"
    }
    return appendNotice(m, m.bundle.Get(key))
}

func cmdContrast(m model, args []string) (tea.Model, tea.Cmd) {
    if len(args) == 0 {
        key := "contrast.disabled"
        if m.effective.HighContrast {
            key = "contrast.enabled"
        }
        return appendNotice(m, m.bundle.Get(key))
    }
    if len(args) != 1 || (args[0] != "on" && args[0] != "off") {
        return appendCommandError(m, m.bundle.Get("tui.command.contrast.usage"))
    }
    enabled := args[0] == "on"
    next := m.prefs
    next.HighContrast = &enabled
    if err := savePreferences(m, next); err != nil {
        return appendCommandError(m, m.bundle.GetF(
            "tui.command.preference.persist_failed", map[string]string{"error": err.Error()}))
    }
    m.prefs = next
    m.effective.HighContrast = enabled
    if enabled {
        m.theme = ThemeHighContrast
    } else {
        base, ok := themeByName(ThemeName(m.effective.ThemeName))
        if !ok {
            return appendCommandError(m, m.bundle.Get("tui.command.contrast.usage"))
        }
        m.theme = base.Name
    }
    key := "contrast.disabled"
    if enabled {
        key = "contrast.enabled"
    }
    return appendNotice(m, m.bundle.Get(key))
}
```

`internal/cli/doctor.go` 在真实 `RunDoctor` 的 config check 后追加五项结果（而不是在 `cmd/yanshi` 创建未调用的旁路函数）：

```go
// In RunDoctor, immediately after checkConfig:
checks = append(checks, checkSecretsConfig(cfg, cfgErr))
checks = append(checks, checkAuthConfig(cfg, cfgErr))
checks = append(checks, checkLocaleConfig(cfg, cfgErr))
checks = append(checks, checkKeymapConfig(cfg, cfgErr))
checks = append(checks, checkHighContrastConfig(cfg, cfgErr))
```

并添加以下纯检查函数；需在 `doctor.go` import `internal/i18n`、`internal/keymap`（alias `corekeymap`）和 `internal/secrets`：

```go
func checkSecretsConfig(cfg *config.Config, cfgErr error) CheckResult {
    if cfgErr != nil {
        return skipped("secrets", cfgErr)
    }
    switch cfg.Secrets.Backend {
    case "", "auto", "keyring", "none":
        return CheckResult{Name: "secrets", Status: StatusOK,
            Message: "backend=" + valueOr(cfg.Secrets.Backend, "auto")}
    case "file":
        envName := cfg.Secrets.PassphraseEnv
        if envName == "" {
            return CheckResult{Name: "secrets", Status: StatusFail,
                Message: "file backend requires secrets.passphrase_env"}
        }
        if _, ok := os.LookupEnv(envName); !ok {
            return CheckResult{Name: "secrets", Status: StatusWarn,
                Message: "file backend passphrase environment is not set"}
        }
        return CheckResult{Name: "secrets", Status: StatusOK,
            Message: "file backend passphrase environment is set"}
    default:
        return CheckResult{Name: "secrets", Status: StatusFail,
            Message: "unknown backend"}
    }
}

func checkAuthConfig(cfg *config.Config, cfgErr error) CheckResult {
    if cfgErr != nil {
        return skipped("auth", cfgErr)
    }
    checked := 0
    for _, provider := range cfg.LLM.Providers {
        if provider.APIKey == "" {
            continue
        }
        checked++
        if _, err := secrets.ParseCredentialRef(provider.APIKey, cfg.Auth.LegacyInsecure); err != nil {
            // Do not print err or APIKey: a rejected raw literal may itself be a secret.
            return CheckResult{Name: "auth", Status: StatusFail,
                Message: fmt.Sprintf("provider %q has an invalid credential reference", provider.Name)}
        }
    }
    return CheckResult{Name: "auth", Status: StatusOK,
        Message: fmt.Sprintf("%d credential reference(s) valid", checked)}
}

func checkLocaleConfig(cfg *config.Config, cfgErr error) CheckResult {
    if cfgErr != nil {
        return skipped("locale", cfgErr)
    }
    b, err := i18n.NewBundle(cfg.I18N.UILocale)
    if err != nil {
        return CheckResult{Name: "locale", Status: StatusFail, Message: err.Error()}
    }
    return CheckResult{Name: "locale", Status: StatusOK,
        Message: fmt.Sprintf("persistent=%s effective=%s", b.Persistent(), b.Effective())}
}

func checkKeymapConfig(cfg *config.Config, cfgErr error) CheckResult {
    if cfgErr != nil {
        return skipped("keymap", cfgErr)
    }
    name := cfg.TUI.KeymapName
    if name == "" {
        name = "default"
    }
    if name != "default" {
        return CheckResult{Name: "keymap", Status: StatusFail,
            Message: "unsupported keymap name"}
    }
    if _, err := corekeymap.NewDefaultBuilder(cfg.TUI.Bindings).Build(); err != nil {
        // Do not render err: a raw action/key in YAML is untrusted text and may
        // itself contain a credential. The TUI has localized typed diagnostics.
        return CheckResult{Name: "keymap", Status: StatusFail,
            Message: "key bindings are invalid; use /keymap diagnostics"}
    }
    return CheckResult{Name: "keymap", Status: StatusOK,
        Message: fmt.Sprintf("default keymap, %d override(s), no conflicts",
            len(cfg.TUI.Bindings))}
}

func checkHighContrastConfig(cfg *config.Config, cfgErr error) CheckResult {
    if cfgErr != nil {
        return skipped("high-contrast", cfgErr)
    }
    enabled := cfg.TUI.HighContrast != nil && *cfg.TUI.HighContrast
    return CheckResult{Name: "high-contrast", Status: StatusOK,
        Message: fmt.Sprintf("enabled=%v", enabled)}
}

func valueOr(value, fallback string) string {
    if value == "" {
        return fallback
    }
    return value
}
```

`internal/cli/doctor_test.go` 添加 report 级门禁：

```go
func TestDoctorD3ChecksDoNotLeakCredential(t *testing.T) {
    dir := t.TempDir()
    cfgPath := filepath.Join(dir, "config.yaml")
    sentinel := "sk-doctor-must-not-print"
    cfg := fmt.Sprintf(`
storage:
  sqlite_path: %q
server:
  http_addr: 127.0.0.1:0
secrets:
  backend: none
i18n:
  ui_locale: auto
tui:
  keymap: default
  high_contrast: true
llm:
  providers:
    - name: bad
      kind: openai
      model: fake
      api_key: %q
`, filepath.Join(dir, "doctor.db"), sentinel)
    if err := os.WriteFile(cfgPath, []byte(cfg), 0600); err != nil {
        t.Fatal(err)
    }
    report := RunDoctor(context.Background(), DoctorOptions{ConfigPath: cfgPath, Root: dir})
    names := map[string]bool{}
    for _, check := range report.Checks {
        names[check.Name] = true
        if strings.Contains(check.Message, sentinel) {
            t.Fatalf("doctor leaked credential through %s: %q", check.Name, check.Message)
        }
    }
    for _, want := range []string{"secrets", "auth", "locale", "keymap", "high-contrast"} {
        if !names[want] {
            t.Errorf("missing D3 check %q", want)
        }
    }
}
```

`cmd/yanshi/main_test.go` 的 RED 测试已在本 Task 的 Failure Test 段给出；实现后它通过真实 `runDoctor` 渲染路径，并允许既有 optional dependency checks 合法返回 WARN（exit 1），仅把 FAIL（exit 2）视为失败。

`docs/feature-roadmap-codex-deepseek.md` 只替换以下真实行（不新增虚构的 `blocks: C15` 字段）：

```diff
 ### [OBS3] feature flags  (P2 | 缺失 | codex comparison O02 / deepseek `features.rs`)
-- **设计**:flag 含 `stage(stable|beta|experimental)/default/owner`;CLI 可 list/enable/disable;strict mode 下未知 flag 报错
+- **设计**:flag 含 `stage(stable|beta|experimental)/default/owner`;CLI 可 list/enable/disable;strict mode 下未知 flag 报错;C15 在 D3 获正式豁免,不受该 gate 阻塞
@@
 ### [C15] keymap 配置  (P3 | 缺失 | codex comparison C15 / deepseek ACCESSIBILITY)
-- **设计**: 核心按键可重映射;Vim 开关;高对比主题(接现有 `/theme`);冲突可诊断+恢复默认
-- **依赖**: [OBS3](flag 门控)
+- **设计**: 核心按键可重映射;Vim 开关;高对比主题(接现有 `/theme`);冲突按类型诊断并可持久恢复默认
+- **依赖**: - (D3 正式豁免 OBS3 feature-flag gate)
@@
-- **验收**: 核心按键可重映射;Vim 开关;高对比主题;冲突可诊断
+- **验收**: 核心按键可重映射;Vim 开关;高对比主题;冲突可诊断并恢复默认
```

### Pass

```sh
go test ./internal/keymap
go test ./internal/cli/tui -run "TestKeymapAdapter|TestModelUpdate_|TestCommandKeymap|TestCommandVim|TestCommandContrast"
go test ./internal/cli -run TestDoctorD3ChecksDoNotLeakCredential
go test ./cmd/yanshi -run TestDoctorD3Checks
```

### Commit

```
feat(tui): keymap adapter + Vim commands + high-contrast + doctor (C15-L2)

TUI routes single-key KeyMsg values through semantic core.Action and consumes
Vim transitions without inserting i/a/o. Paste/multi-rune input bypasses
shortcuts. /keymap reset rebuilds and persists built-in defaults, /contrast off
restores the base theme, and doctor uses the same keymap validator without
printing untrusted key/action values. Roadmap records the C15 OBS3 exemption.
```

---

## Task 14 — O03-L5: SQLite metadata lifecycle + Status/Logout 补偿事务

**结构性修复落点:** #12（Task 7 已完成 token→metadata 写入补偿；本 Task 补齐 metadata Load/Delete、metadata-aware Status、Logout 双 adapter 补偿与 rollback join）。本 Task **不**重复定义 `commitDeviceToken`，也不引入 `time.Now()` 计算 token expiry。

### Files

- Modify `internal/auth/auth.go` (`MetadataStore` 扩展 Load/Delete；`Status` 去掉虚假 LastUsed，增加 ExpiresAt；Logout 补偿)
- Create `internal/auth/lifecycle_test.go`
- Modify `internal/auth/device_test.go` (把 Task 7 的 Save-only test adapter 升级为完整 `MetadataStore`)
- Modify `internal/store/auth.go` (SQLite Load/Delete)
- Modify `internal/store/auth_test.go` (upsert/load/delete/not-found)
- Modify `cmd/yanshi/main.go` (`auth status` 显示 non-secret expiry)
- Modify `cmd/yanshi/main_test.go` (status/logout 跨 invocation 生命周期)

### Failure Test (RED)

`internal/auth/lifecycle_test.go`：

```go
package auth

import (
    "errors"
    "testing"
    "time"

    "github.com/x6nux/yanshi/internal/secrets"
)

type memoryMetadataStore struct {
    rows      map[string]AuthMetadata
    loadErr   error
    deleteErr error
}

func metadataKey(provider, account string) string {
    return provider + "\x00" + account
}

func (m *memoryMetadataStore) SaveAuthMetadata(
    provider, account string,
    meta AuthMetadata,
) error {
    if m.rows == nil {
        m.rows = map[string]AuthMetadata{}
    }
    m.rows[metadataKey(provider, account)] = meta
    return nil
}

func (m *memoryMetadataStore) LoadAuthMetadata(
    provider, account string,
) (AuthMetadata, error) {
    if m.loadErr != nil {
        return AuthMetadata{}, m.loadErr
    }
    meta, ok := m.rows[metadataKey(provider, account)]
    if !ok {
        return AuthMetadata{}, ErrAuthMetadataNotFound
    }
    return meta, nil
}

func (m *memoryMetadataStore) DeleteAuthMetadata(provider, account string) error {
    if m.deleteErr != nil {
        return m.deleteErr
    }
    key := metadataKey(provider, account)
    if _, ok := m.rows[key]; !ok {
        return ErrAuthMetadataNotFound
    }
    delete(m.rows, key)
    return nil
}

func TestManager_StatusUsesMetadataAndPropagatesBackendFailure(t *testing.T) {
    credentials := &fakeCredentialStore{values: map[string]string{
        credentialKey("openai", "main"): "token",
    }}
    expires := time.Unix(2222, 0).UTC()
    metadata := &memoryMetadataStore{rows: map[string]AuthMetadata{
        metadataKey("openai", "main"): {Source: "device", ExpiresAt: expires},
    }}
    manager := managerWithCredentialStore(t, credentials)
    manager.SetMetadataStore(metadata)

    got, err := manager.Status("openai", "main")
    if err != nil {
        t.Fatal(err)
    }
    if !got.Authenticated || got.Source != "device" ||
        !got.ExpiresAt.Equal(expires) {
        t.Fatalf("status = %#v", got)
    }

    metadata.loadErr = errors.New("metadata-load-sentinel")
    if _, err := manager.Status("openai", "main");
        !errors.Is(err, metadata.loadErr) {
        t.Fatalf("metadata load error swallowed: %v", err)
    }
}

func TestManager_StatusWithoutMetadataFallsBackToSecretSource(t *testing.T) {
    credentials := &fakeCredentialStore{values: map[string]string{
        credentialKey("openai", "main"): "token",
    }}
    manager := managerWithCredentialStore(t, credentials)
    manager.SetMetadataStore(&memoryMetadataStore{rows: map[string]AuthMetadata{}})
    got, err := manager.Status("openai", "main")
    if err != nil || !got.Authenticated || got.Source != "secret" ||
        !got.ExpiresAt.IsZero() {
        t.Fatalf("status = %#v err=%v", got, err)
    }
}

func TestManager_LogoutDeletesCredentialAndMetadata(t *testing.T) {
    credentials := &fakeCredentialStore{values: map[string]string{
        credentialKey("openai", "main"): "old-token",
    }}
    metadata := &memoryMetadataStore{rows: map[string]AuthMetadata{
        metadataKey("openai", "main"): {Source: "device"},
    }}
    manager := managerWithCredentialStore(t, credentials)
    manager.SetMetadataStore(metadata)

    if err := manager.Logout("openai", "main"); err != nil {
        t.Fatal(err)
    }
    if _, err := credentials.Get("openai", "main");
        !errors.Is(err, secrets.ErrSecretNotFound) {
        t.Fatalf("credential survived logout: %v", err)
    }
    if _, err := metadata.LoadAuthMetadata("openai", "main");
        !errors.Is(err, ErrAuthMetadataNotFound) {
        t.Fatalf("metadata survived logout: %v", err)
    }
}

func TestManager_LogoutMetadataFailureRestoresCredential(t *testing.T) {
    metadataErr := errors.New("metadata-delete-sentinel")
    credentials := &fakeCredentialStore{values: map[string]string{
        credentialKey("openai", "main"): "old-token",
    }}
    metadata := &memoryMetadataStore{
        rows: map[string]AuthMetadata{
            metadataKey("openai", "main"): {Source: "device"},
        },
        deleteErr: metadataErr,
    }
    manager := managerWithCredentialStore(t, credentials)
    manager.SetMetadataStore(metadata)

    err := manager.Logout("openai", "main")
    if !errors.Is(err, metadataErr) {
        t.Fatalf("want metadata error, got %v", err)
    }
    got, getErr := credentials.Get("openai", "main")
    if getErr != nil || got != "old-token" {
        t.Fatalf("credential not restored: %q, %v", got, getErr)
    }
}

func TestManager_LogoutRollbackFailureIsJoined(t *testing.T) {
    metadataErr := errors.New("metadata-delete-sentinel")
    rollbackErr := errors.New("credential-restore-sentinel")
    credentials := &fakeCredentialStore{
        values: map[string]string{
            credentialKey("openai", "main"): "old-token",
        },
        failSetCall: 1,
        failSetErr: rollbackErr,
    }
    metadata := &memoryMetadataStore{
        rows: map[string]AuthMetadata{
            metadataKey("openai", "main"): {Source: "device"},
        },
        deleteErr: metadataErr,
    }
    manager := managerWithCredentialStore(t, credentials)
    manager.SetMetadataStore(metadata)

    err := manager.Logout("openai", "main")
    if !errors.Is(err, metadataErr) || !errors.Is(err, rollbackErr) {
        t.Fatalf("errors.Join lost a cause: %v", err)
    }
}

func TestManager_LogoutMissingReturnsNotFound(t *testing.T) {
    manager := managerWithCredentialStore(t,
        &fakeCredentialStore{values: map[string]string{}})
    manager.SetMetadataStore(&memoryMetadataStore{rows: map[string]AuthMetadata{}})
    if err := manager.Logout("openai", "main");
        !errors.Is(err, secrets.ErrSecretNotFound) {
        t.Fatalf("missing logout = %v", err)
    }
}
```

`internal/store/auth_test.go` 追加真实 adapter lifecycle：

```go
func TestAuthSQLiteAdapterLoadDeleteLifecycle(t *testing.T) {
    st, err := Open(":memory:")
    if err != nil {
        t.Fatal(err)
    }
    defer st.Close()
    adapter := AuthMetadataFromDB(st.DB)
    expires := time.Unix(3456, 0).UTC()

    if err := adapter.SaveAuthMetadata("openai", "main", auth.AuthMetadata{
        Source: "device", ExpiresAt: expires,
    }); err != nil {
        t.Fatal(err)
    }
    got, err := adapter.LoadAuthMetadata("openai", "main")
    if err != nil || got.Source != "device" || !got.ExpiresAt.Equal(expires) {
        t.Fatalf("load = %#v err=%v", got, err)
    }
    if err := adapter.DeleteAuthMetadata("openai", "main"); err != nil {
        t.Fatal(err)
    }
    if _, err := adapter.LoadAuthMetadata("openai", "main");
        !errors.Is(err, auth.ErrAuthMetadataNotFound) {
        t.Fatalf("load after delete = %v", err)
    }
    if err := adapter.DeleteAuthMetadata("openai", "main");
        !errors.Is(err, auth.ErrAuthMetadataNotFound) {
        t.Fatalf("second delete = %v", err)
    }
}
```

### Expected

- `AuthMetadata` 仍只含 `Source`、`ExpiresAt`；不新增 token/device/user/client-secret 字段。
- `MetadataStore` 完整支持 Save/Load/Delete，缺失 row 用 `ErrAuthMetadataNotFound`，与 SQLite/secret backend failure 可区分。
- `Status` 只有 credential 存在时才 `Authenticated=true`；metadata 存在则返回其 source/expiry，metadata 缺失回退 `Source="secret"`，真实 metadata backend error 不得吞掉。删除没有数据来源的 `LastUsed`。
- `Logout` 先 snapshot credential + metadata，再删 credential，最后删 metadata。metadata delete 失败时恢复旧 credential；恢复失败用 `errors.Join` 保留两个 cause。既无 token 也无 metadata 时返回 `secrets.ErrSecretNotFound`。
- device token expiry 仍由 Task 7 的 `m.clk.Now()` 计算；本 Task 不更改该函数。

### Implementation

`internal/auth/auth.go` 用以下定义替换 Task 6/7 的 Status/metadata port：

```go
var ErrAuthMetadataNotFound = errors.New("auth: metadata not found")

type Status struct {
    Provider      string
    Account       string
    Authenticated bool
    Source        string
    ExpiresAt     time.Time
}

type AuthMetadata struct {
    Source    string
    ExpiresAt time.Time
}

type MetadataStore interface {
    SaveAuthMetadata(provider, account string, meta AuthMetadata) error
    LoadAuthMetadata(provider, account string) (AuthMetadata, error)
    DeleteAuthMetadata(provider, account string) error
}

func (m *Manager) Status(provider, account string) (Status, error) {
    base := Status{Provider: provider, Account: account}
    if m.store == nil {
        return base, nil
    }
    value, err := m.store.Get(provider, account)
    if errors.Is(err, secrets.ErrSecretNotFound) {
        return base, nil
    }
    if err != nil {
        return Status{}, fmt.Errorf("auth: query credential status: %w", err)
    }
    if value == "" {
        return base, nil
    }
    base.Authenticated = true
    base.Source = "secret"
    if m.metadata == nil {
        return base, nil
    }
    meta, err := m.metadata.LoadAuthMetadata(provider, account)
    if errors.Is(err, ErrAuthMetadataNotFound) {
        return base, nil
    }
    if err != nil {
        return Status{}, fmt.Errorf("auth: load metadata: %w", err)
    }
    base.Source = meta.Source
    base.ExpiresAt = meta.ExpiresAt
    return base, nil
}

func (m *Manager) Logout(provider, account string) error {
    if m.store == nil {
        return errors.New("auth: no secret backend configured")
    }
    oldToken, tokenErr := m.store.Get(provider, account)
    hadToken := tokenErr == nil
    if tokenErr != nil && !errors.Is(tokenErr, secrets.ErrSecretNotFound) {
        return fmt.Errorf("auth: snapshot credential: %w", tokenErr)
    }

    hadMeta := false
    if m.metadata != nil {
        _, err := m.metadata.LoadAuthMetadata(provider, account)
        hadMeta = err == nil
        if err != nil && !errors.Is(err, ErrAuthMetadataNotFound) {
            return fmt.Errorf("auth: snapshot metadata: %w", err)
        }
    }
    if !hadToken && !hadMeta {
        return secrets.ErrSecretNotFound
    }

    if hadToken {
        if err := m.store.Delete(provider, account); err != nil {
            return fmt.Errorf("auth: delete credential: %w", err)
        }
    }
    if !hadMeta {
        return nil
    }
    if err := m.metadata.DeleteAuthMetadata(provider, account); err != nil {
        base := fmt.Errorf("auth: delete metadata: %w", err)
        if !hadToken {
            return base // Delete failed, so the metadata row remains unchanged.
        }
        if restoreErr := m.store.Set(provider, account, oldToken); restoreErr != nil {
            return errors.Join(base,
                fmt.Errorf("auth: restore credential: %w", restoreErr))
        }
        return base
    }
    return nil
}
```

`internal/auth/device_test.go` 把 Task 7 的 `metadataStoreFunc` 替换为完整 adapter；现有三处 Save 测试把 literal 改成 `metadataStoreFuncs{save: func(...) error { ... }}`：

```go
type metadataStoreFuncs struct {
    save   func(string, string, AuthMetadata) error
    load   func(string, string) (AuthMetadata, error)
    delete func(string, string) error
}

func (f metadataStoreFuncs) SaveAuthMetadata(
    provider, account string,
    meta AuthMetadata,
) error {
    return f.save(provider, account, meta)
}
func (f metadataStoreFuncs) LoadAuthMetadata(
    provider, account string,
) (AuthMetadata, error) {
    if f.load == nil {
        return AuthMetadata{}, ErrAuthMetadataNotFound
    }
    return f.load(provider, account)
}
func (f metadataStoreFuncs) DeleteAuthMetadata(provider, account string) error {
    if f.delete == nil {
        return ErrAuthMetadataNotFound
    }
    return f.delete(provider, account)
}

m.SetMetadataStore(metadataStoreFuncs{save: func(
    _ string, _ string, meta AuthMetadata,
) error {
    saved = meta
    return nil
}})
```

`internal/store/auth.go` 在 Task 8 Save 实现后追加：

```go
func (a *authSQLiteAdapter) LoadAuthMetadata(
    provider, account string,
) (auth.AuthMetadata, error) {
    var source string
    var expiresAt int64
    err := a.db.QueryRow(
        `SELECT source, expires_at FROM auth_metadata
         WHERE provider = ? AND account = ?`,
        provider, account,
    ).Scan(&source, &expiresAt)
    if errors.Is(err, sql.ErrNoRows) {
        return auth.AuthMetadata{}, auth.ErrAuthMetadataNotFound
    }
    if err != nil {
        return auth.AuthMetadata{}, err
    }
    meta := auth.AuthMetadata{Source: source}
    if expiresAt != 0 {
        meta.ExpiresAt = time.Unix(expiresAt, 0).UTC()
    }
    return meta, nil
}

func (a *authSQLiteAdapter) DeleteAuthMetadata(provider, account string) error {
    result, err := a.db.Exec(
        `DELETE FROM auth_metadata WHERE provider = ? AND account = ?`,
        provider, account,
    )
    if err != nil {
        return err
    }
    affected, err := result.RowsAffected()
    if err != nil {
        return err
    }
    if affected == 0 {
        return auth.ErrAuthMetadataNotFound
    }
    return nil
}
```

该文件 import 增加 `errors`；现有 `database/sql`、`time`、`auth` 继续使用。

`cmd/yanshi/main.go` 的 status 输出只增加 non-secret expiry：

```go
expiresAt := ""
if !st.ExpiresAt.IsZero() {
    expiresAt = st.ExpiresAt.UTC().Format(time.RFC3339)
}
fmt.Fprintf(stdout,
    "Provider: %s\nAccount: %s\nAuthenticated: %v\nSource: %s\nExpiresAt: %s\n",
    st.Provider, st.Account, st.Authenticated, st.Source, expiresAt)
```

### Pass

```sh
go test ./internal/auth -run "TestManager_(Status|Logout)|TestCommitDeviceToken_"
go test ./internal/store -run "TestAuthSQLiteAdapter"
go test ./cmd/yanshi -run "TestMainAuth_(E2E|DeviceE2E_ReopensPersistedToken)"
```

### Commit

```text
feat(auth): complete SQLite metadata status/logout lifecycle (O03-L5)

MetadataStore gains Load/Delete and a distinct not-found sentinel. Status returns
source/expiry without a fictional LastUsed value. Logout snapshots both adapters,
restores the old credential when metadata deletion fails, and joins rollback
failures without changing Task 7's injected-clock device-token transaction.
```

---

## Task 15 — D3 安全门禁 + acceptance（显式 suite，不用宽泛 regex）

**结构性修复落点:** #18（逐包安全 suite）、#19（复用 Task 2/11 的 injectable atomic-replace tests，不再写手工 backup/restore 伪原子测试）、#20（最终诚实 self-review）。

### Files

- Create `internal/secrets/security_test.go`
- Modify `internal/auth/device_test.go` (OAuth body limit + exact leak sentinels)
- Modify `cmd/yanshi/main_test.go` (SQLite 文件秘密扫描)

### Failure Test (RED)

`internal/secrets/security_test.go`：

```go
package secrets

import (
    "os"
    "path/filepath"
    "strings"
    "testing"
)

func TestSecurity_NoPlaintextInEncryptedFile(t *testing.T) {
    path := filepath.Join(t.TempDir(), "secrets.enc")
    fs, err := NewFileStore(path, []byte("strong-test-passphrase"))
    if err != nil {
        t.Fatal(err)
    }
    secret := "sk-security-gate-1234567890"
    if err := fs.Set("openai", "main", secret); err != nil {
        t.Fatal(err)
    }
    raw, err := os.ReadFile(path)
    if err != nil {
        t.Fatal(err)
    }
    if strings.Contains(string(raw), secret) {
        t.Fatal("encrypted file contains plaintext API key")
    }
}

func TestSecurity_RawLiteralFailsClosedWithoutLegacyOptIn(t *testing.T) {
    if _, err := ParseCredentialRef("sk-raw-live-key", false); err == nil {
        t.Fatal("raw literal accepted without legacy-insecure opt-in")
    }
}
```

不新增手工删除 target/备份/恢复的测试。FileStore replacement failure、old-file preservation、内存 snapshot rollback 与 tmp cleanup 已由 Task 2 的 injected `replaceFile` seam 确定性覆盖；preferences 同理由 Task 11 的 `replacePreferencesFile` seam 覆盖。

`internal/auth/device_test.go` 追加两个精确安全测试：

```go
func TestDeviceFlow_RejectsOversizedOAuthBodies(t *testing.T) {
    const oversized = maxOAuthResponseBytes + 1
    tests := []struct {
        name       string
        deviceBody bool
    }{
        {name: "device response", deviceBody: true},
        {name: "token response", deviceBody: false},
    }
    for _, tc := range tests {
        t.Run(tc.name, func(t *testing.T) {
            srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
                if r.URL.Path == "/device" && !tc.deviceBody {
                    _ = json.NewEncoder(w).Encode(map[string]any{
                        "device_code": "dc-limit",
                        "user_code": "CODE",
                        "verification_uri": "https://example.com/device",
                        "expires_in": 60,
                        "interval": 1,
                    })
                    return
                }
                _, _ = w.Write(bytes.Repeat([]byte("x"), oversized))
            }))
            defer srv.Close()
            p := &GenericRFC8628Provider{GenericRFC8628Config: GenericRFC8628Config{
                ClientID: "id", DeviceURL: srv.URL + "/device",
                TokenURL: srv.URL + "/token", HTTPClient: srv.Client(),
            }}
            clk := newFakeClock(0)
            if _, err := p.Authorize(context.Background(), clk,
                &fakeSleeper{clock: clk}, nil); err == nil {
                t.Fatal("oversized OAuth response accepted")
            }
        })
    }
}

func TestDeviceFlow_UnknownOAuthErrorHasFixedSafeText(t *testing.T) {
    sentinels := []string{
        "device-code-secret", "client-secret-value", "provider_weird_error",
        "provider-body-secret", "endpoint-query-secret", "418",
    }
    redactor := secrets.NewRedactor()
    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        switch r.URL.Path {
        case "/device":
            _ = json.NewEncoder(w).Encode(map[string]any{
                "device_code": "device-code-secret",
                "user_code": "CODE",
                "verification_uri": "https://example.com/device",
                "expires_in": 60,
                "interval": 1,
            })
        case "/token":
            w.WriteHeader(http.StatusTeapot)
            _, _ = io.WriteString(w,
                `{"error":"provider_weird_error","error_description":"provider-body-secret"}`)
        }
    }))
    defer srv.Close()

    p := &GenericRFC8628Provider{GenericRFC8628Config: GenericRFC8628Config{
        ClientID: "id", ClientSecret: "client-secret-value",
        DeviceURL: srv.URL + "/device",
        TokenURL: srv.URL + "/token?detail=endpoint-query-secret",
        HTTPClient: srv.Client(), Redactor: redactor,
    }}
    clk := newFakeClock(0)
    _, err := p.Authorize(context.Background(), clk,
        &fakeSleeper{clock: clk}, nil)
    if err == nil {
        t.Fatal("unknown OAuth error must fail")
    }
    for _, sentinel := range sentinels {
        if strings.Contains(err.Error(), sentinel) {
            t.Fatalf("OAuth error leaked %q: %v", sentinel, err)
        }
    }
    for _, secret := range []string{"device-code-secret", "client-secret-value"} {
        if strings.Contains(redactor.Redact("value="+secret), secret) {
            t.Fatalf("redactor did not register %q", secret)
        }
    }
}
```

该测试的 import delta 明确增加 `bytes`、`io`；其它 import 已由 Task 7 使用。

`cmd/yanshi/main_test.go` 在 `TestMainAuth_DeviceE2E_ReopensPersistedToken` 最后、所有 CLI invocation 已关闭 SQLite 后追加 raw DB 扫描：

```go
dbPath := filepath.Join(dir, "auth.db")
for _, path := range []string{dbPath, dbPath + "-wal"} {
    raw, err := os.ReadFile(path)
    if err != nil {
        if os.IsNotExist(err) {
            continue
        }
        t.Fatal(err)
    }
    for _, forbidden := range []string{
        accessToken,
        refreshToken,
        "device-code-sentinel",
        "USER-CODE",
        "client-secret-value",
    } {
        if bytes.Contains(raw, []byte(forbidden)) {
            t.Fatalf("SQLite file %s contains secret %q", path, forbidden)
        }
    }
}
```

这里同时检查 main DB 与可能存在的 WAL；`auth_metadata` 只允许 provider/account/source/expiry/timestamps。`client-secret-value` 由独立 OAuth leak test使用；即使将来生产 config 加 client-secret ref，该值也不得进入 metadata DB。

### Expected

- encrypted FileStore 无 plaintext；raw literal 无 opt-in fail-closed。
- RFC 8628 device/token body 均受 1 MiB 上限；unknown OAuth error 不泄漏 body、endpoint query、status、未知 identifier、device code 或 client secret。
- device CLI stdout/stderr 已由 Task 9 sentinel assertions 覆盖；SQLite main/WAL 都不得含 access token、refresh token、device code、user code、client secret。
- acceptance 显式运行安全相关完整包、non-CGO build、全量 test/vet；不使用可能漏项的 `-run "Secret|Auth|..."` 大 regex。

### Implementation

本 Task 只增加门禁测试；生产实现全部来自 Task 1–14。若任一 RED 失败，回到拥有该边界的 Task 修复，不在 acceptance test 中放宽断言：

- body limit 修复位置：Task 7 `readOAuthResponse(io.LimitReader(...))`；
- error leak 修复位置：Task 7 fixed external errors + Redactor registration；
- DB leak 修复位置：Task 8 schema/adapter 或 Task 14 lifecycle；
- atomic replace 修复位置：Task 2/11 injectable replace seams。

### Pass（显式安全 suite）

```sh
# S10/O03 核心与持久化边界
go test ./internal/secrets
go test ./internal/auth
go test ./internal/api/http
go test ./internal/store

# I18N/C15/TUI/doctor
go test ./internal/i18n
go test ./internal/keymap
go test ./internal/cli/tui
go test ./internal/cli

# 组合根与 CLI
go test ./internal/bootstrap
go test ./cmd/yanshi

# build-tag / non-CGO 门禁
go test -tags nokeyring ./internal/secrets
go build -tags nokeyring ./internal/secrets
CGO_ENABLED=0 go build ./...

# 最终仓库门禁
go test ./...
go vet ./...
```

Expected：每条命令 exit 0；允许 CLAUDE.md 已记录的 provider/e2e skip，不允许失败或 secret sentinel 出现在输出。上述命令仅在实施阶段执行；编写本计划时不运行。

### Commit

```text
test(d3): add explicit secret/auth/i18n/keymap acceptance gates

Adds exact no-plaintext, OAuth body-limit/error-leak, and SQLite main/WAL secret
scans. Reuses deterministic atomic-replace tests from their owning tasks and
runs explicit security packages, nokeyring/non-CGO builds, full tests, and vet.
```

---
## Catalog key manifest (I18N1 选定 surface)

所有下列 key 必须同时出现在 `en.json` 与 `zh-Hans.json`；`TestBundle_CatalogKeyManifest` 是漂移门禁：

```
tui.input.placeholder
tui.command.help.title
tui.command.help.row
tui.status.model
tui.status.thinking
tui.status.tokens_in
tui.status.tokens_out
tui.error.connect_failed
tui.error.auth_failed
tui.error.send_failed
tui.workflow.running
tui.workflow.completed
tui.workflow.failed
auth.cli.prompt
auth.cli.stored
auth.cli.deleted
auth.cli.status.authenticated
auth.cli.status.unauthenticated
keymap.diagnostics.conflict
keymap.diagnostics.unknown_action
keymap.diagnostics.reset
vim.mode.insert
vim.mode.normal
vim.mode.visual
contrast.enabled
contrast.disabled
```

**不在本批 catalog surface:** model-generated text、tool output、server error technical details、MCP server messages。这些保持原文；只有 UI chrome 与用户可执行的 auth/keymap 提示本地化。模型输出语言由 `i18n.output_language` 独立控制，不与 UI locale 联动。

---

## 四级 preferences 合并规则（显式）

所有字段遵守同一优先级：

```
CLI flag > env > user preferences.json > project config.yaml > defaults
```

| 字段 | flag | env | user prefs | project config | default | 三态规则 |
|---|---|---|---|---|---|---|
| Vim | `--vim` / `--no-vim` | `YANSHI_VIM` | `vim: *bool` | `tui.vim: *bool` | false | **nil=未设置，false=显式关** |
| UI locale | `--locale` | `YANSHI_UI_LOCALE` | `ui_locale` | `i18n.ui_locale` | auto | `auto` 原样持久化，每次启动重算 |
| output language | `--output-language` | `YANSHI_OUTPUT_LANGUAGE` | 不持久化 | `i18n.output_language` | empty | 与 UI locale 独立 |
| keymap | `--keymap` | `YANSHI_KEYMAP` | `keymap` | `tui.keymap` | default | 完整候选 map 后统一校验 |
| theme | `--theme` | `YANSHI_THEME` | `theme` | `tui.theme` | default | high-contrast 可独立覆盖 |
| high contrast | `--high-contrast` | `YANSHI_HIGH_CONTRAST` | `high_contrast` | `tui.high_contrast` | false | 显式 false 覆盖低层 true |

每个字段的 merge 都有 table-driven test，至少覆盖 default/config/prefs/env/flag 5 行；Vim/high-contrast 额外覆盖显式 false 覆盖低层 true。

---

## Structural Fix Matrix (20 项逐项落点)

| # | 必修项 | 落点 | 测试/门禁 |
|---|---|---|---|
| 1 | O03 凭据到达 BuildProviders | Task 5 + 8 (`cfg.LLM.Providers[i].APIKey` 先 resolve/write-back) | `TestBuild_AuthManagerResolvesCredentialsBeforeProviders` |
| 2 | `buildDeviceProviders` / `AuthDeps.Providers` / httptest CLI | Task 7 + 8 + 9 | `TestDeviceFlow_*`, `TestMainAuth_E2E` |
| 3 | `secret://`, `env://`, legacy opt-in, raw literal fail-closed | Task 1 + 5 + 6 + 14 | `TestCredentialRef_Parse`, `TestSecurity_RawLiteralFailsClosedWithoutLegacyOptIn` |
| 4 | 单一 safe sink 覆盖 stderr/WS/SSE/SQLite | Task 1 + 4 | `TestSSE_RedactsAllFrameTypes`, `TestWS_RedactsOutboundFrames`, `TestStore_RedactsAllWritePaths` |
| 5 | keyring 版本锁 + non-CGO build + fallback | Task 2 + 14 | `CGO_ENABLED=0 go build ./...` |
| 6 | Argon2id + KDF 名/参数/升级兼容 | Task 2 | `TestFileStore_KDFHeaderIsArgon2id` |
| 7 | locale auto 原样持久化/跨启动重算 | Task 10 | `TestBundle_AutoRecomputedEachLoad`, `TestBundle_PersistAutoAcrossTwoLoads` |
| 8 | C15/OBS3 gate 或正式豁免 | Task 13 | roadmap 精确修改 + `TestDoctorD3Checks` |
| 9 | Vim *bool tri-state + 四级优先级 | Task 11 + 12 | `TestPreferences_FourLevelMerge`, `TestVim_TriStateDistinguishesUnsetAndFalse` |
| 10 | Device HTTPS-only/timeout/cancel/safe errors | Task 7 | `TestDeviceFlow_HTTPSOnlyByDefault`, `TestDeviceFlow_ContextCancel` |
| 11 | SSE 全 10 输出点含 pre-stream error | Task 4 | `TestSSE_RedactsAllFrameTypes` + `writeSSEError` 测试 |
| 12 | Device timing/expiry/slow_down/cancel/leak/rollback | Task 7 + 14 | 6 个 `TestDeviceFlow_*` + metadata rollback |
| 13 | CLI auth 完整 + stdin flag + cleanup | Task 3 + 9 | 两个 `TestMainAuth` |
| 14 | 真实 test double/API + t.TempDir + cleanup restore | Task 11 + 13 | `recordingSession.sentText`, `&fakeSession{}`, `TestPreferences_*` |
| 15 | i18n helpEntry/config name/manifest/no hardcode | Task 10 + 11 | `TestCmdHelpEntry_*`, catalog manifest |
| 16 | keymap 全候选统一校验/reset/normalize/Vim j-k | Task 12 + 13 | `TestBuilder_ValidateAfterBuild`, `TestNormalizeKey_AllKeyTypes`, modal tests |
| 17 | Task12 file list + core/TUI/doctor 代码 | Task 12 + 13 | Files 清单 + package tests |
| 18 | 显式安全 suite，不用宽泛 regex | Task 14 | 9 个 package-level `go test` 命令 |
| 19 | preferences/fileStore 原子保存失败测试 | Task 2 + 11 + 14 | `.tmp` 冲突/rename fail/old intact/cleanup |
| 20 | 诚实 self-review | 本节下方 | 按真实接口与占位扫描 |

---

## 待决策 (仅保留无法在计划期验证的事项)

1. **OS keyring dependency:** 本计划锁定 `github.com/zalando/go-keyring v0.2.6`。实现者第一步必须 `go get github.com/zalando/go-keyring@v0.2.6`，然后 `CGO_ENABLED=0 go build ./...`。若此版本在 Windows/macOS/Linux 任一 release builder 失败，启用 `nokeyring` build tag 的 stub-only fallback，**不**换成 "latest"。
2. **Argon2id 参数调优:** 首版锁 `time=1, memory=64MiB, threads=2`。若低内存 CI/OOM，用 `memory=32MiB` 但必须 bump 文件头 params（仍是 v1，可读旧参数）；不得改回自写 PBKDF2。
3. **Traditional Chinese:** 仅 en + zh-Hans 在本批；zh-Hant/zh-TW/zh-HK 回退 en。后续新增 catalog 不改 Bundle API。
4. **Device providers:** generic RFC 8628 adapter 已有；真实 provider endpoint/client_id/scopes 需要逐 provider 配置，不在本批硬编码生产值。Task 8 示例的 Microsoft endpoint仅示意，实施时必须从 config 读取或不注册。
5. **OBS3:** 正式豁免仅针对 C15；未来 OBS3 实现时可选择将 C15 纳入，但这不是本批 blocker。

---

## Self-review (诚实)

- [x] **唯一目标文件:** 本计划编辑阶段只重写 `docs/superpowers/plans/2026-07-21-d3-secrets-auth-i18n-keymap.md`；没有改 `.go`、没有运行 build/test、没有提交 git。
- [x] **真实接口:** `BuildProviders(cfg *config.Config)`、`wsConn.write(f proto.ServerFrame)`、`writeSSEFrame` / `writeSSEError`、`Store.CreateSession` / `AppendMessage` / `UpdateSessionTitle`、`helpEntry struct{}`、`recordingSession.sentText`、`&fakeSession{}` 均按现有代码核验。
- [x] **SSE 计数:** 现有 `chat.go` 实际 10 个输出点，不照抄 review 的 "9 个"；计划通过边界函数签名统一覆盖 10 个。
- [x] **依赖方向:** S10 (`internal/secrets`) 不依赖 O03；O03 (`internal/auth`) 依赖 S10；bootstrap 负责组合，依赖图无环。
- [x] **BuildProviders 可达性:** auth.Manager 在模型前装配；resolved secret 写回 `cfg.LLM.Providers[i].APIKey` 后才调用 `einollm.BuildProviders(cfg)`。
- [x] **KDF:** 使用 `golang.org/x/crypto/argon2`，不手写 PBKDF2。文件头含 KDF 名/参数，upgrade path 明确。
- [x] **安全边界:** stderr/WS/SSE/SQLite 全覆盖；DB 包括 CreateSession/UpdateSessionTitle；safeError 的 Unwrap re-stringify 风险已写明。
- [x] **I18N:** `auto` 原样持久化、跨两次启动改 LANG 重算；统一 `i18n.output_language`；helpEntry 预渲染；catalog manifest + no-hardcode 测试。
- [x] **Keymap:** 候选 map 全量收集后统一 Build 校验；`/keymap reset` 清 diagnostics；Vim `*bool`；所有 modal j/k 行为有测试。
- [x] **Preferences 原子保存:** temp 冲突/rename 失败/旧配置保留/tmp cleanup 均列测试；测试路径用 `t.TempDir`，toggle 用 `t.Cleanup` 恢复。
- [x] **门禁:** Task 14 明确运行九个相关安全包 + `CGO_ENABLED=0 go build ./...` + 全量 test/vet，不依赖宽泛 regex。
- [ ] **代码片段不是可直接复制的完整 patch:** 本计划给出完整关键函数/类型/测试，但对现有超长文件 (`bootstrap.go`, `main.go`, `model.go`) 采用"增量"代码而非全文重贴，以避免覆盖并发改动；实施者必须按 Files 路径合并到现有函数。这里不声称代码已编译，实施仍须每 Task 按 RED→GREEN 校正 imports/现有签名。
- [ ] **真实 provider device metadata store:** 计划定义了 `MetadataStore` port 与 rollback，但尚未指定 SQLite adapter 的表迁移文件；实现时可先用 in-memory metadata，或在独立 migration task 补齐。此项不影响 API-key auth，但会阻塞 device token 生命周期完整上线。

**结论:** 14 个 Task 覆盖 S10 → O03 的强依赖链、I18N1、C15 与全部 20 个 review 必修项。计划是可施工的 TDD 路线，但不是已经验证通过的实现；所有命令都留给执行阶段，本文编写阶段未运行。

---

## C-batch 修复

本节为 review 的 C-batch 五项修复，追加在计划末尾。每项包含问题描述、RED 测试代码、实现代码、GREEN 验证命令、提交命令。实施者应在主计划的 15 个 Task **全部完成**后，按 C1–C5 顺序应用这些修复。

### C1 — `${VAR}` source preservation in config.Load

**问题:** `internal/config/config.go:112` 的 `os.ExpandEnv` 在反序列化前展开 `${VAR}`。当用户在 `api_key` 字段写入 `${MY_KEY}` 且环境变量已设置时，展开后的明文绕过 `ParseCredentialRef(allowLegacy=false)` 的 fail-closed 检查并作为 raw literal 被接受，而对 secret:// 或 env:// 引用无感知。即使 O03 auth.Manager 的 `ResolveAPIKey` 进一步拒绝 raw literal 引用（见 `CredentialSource.APIKeyRef`），但 `cfg.LLM.Providers[i].APIKey` 实际值已是明文——它会在 `ProviderBuilder` 中被当作有效 key 直接使用。

**修复:** 在 `config.go` 新增 `validateAPIKeyRefs()`，在 `Load` 的最后阶段扫描所有 `LLM.Providers[i].APIKey`：非 empty、非 `secret://`/`env://` 的 api_key 在没有 `Auth.LegacyInsecure=true` 时一律拒绝。这与 `ParseCredentialRef` 在 auth.Manager 侧的检查形成双重门禁。

同时修复 `os.ExpandEnv` 对 `api_key` 的副作用：读取原始 YAML 字符串，提取 `${VAR}` 模式的 api_key 值（未展开的 `${...}` 在 `os.ExpandEnv` 后变成 empty 或已展开值），在展开后重新检测这些模式。实施者应优先考虑在 `Load` 内拦截而非依赖事后 scan——但为最小侵入，采用后检测+拒绝的方案。

#### RED: config_test.go

```go
func TestConfig_RejectsExpandedVarAsRawLiteral(t *testing.T) {
    // When the user writes `api_key: ${SOME_SECRET}` and the env var IS set,
    // os.ExpandEnv expands it to the plaintext literal. This must be rejected
    // unless auth.legacy_insecure is explicitly true.

    cases := []struct {
        name         string
        configYAML   string
        env          map[string]string
        legacy       bool
        wantLoadFail bool
    }{
        {
            name: "expanded var is rejected without legacy opt-in",
            configYAML: `
llm:
  providers:
    - name: test
      kind: openai
      model: gpt-4
      api_key: ${MY_KEY}
auth:
  legacy_insecure: false
`,
            env:          map[string]string{"MY_KEY": "sk-raw-expanded"},
            legacy:       false,
            wantLoadFail: true,
        },
        {
            name: "expanded var accepted when legacy_insecure=true",
            configYAML: `
llm:
  providers:
    - name: test
      kind: openai
      model: gpt-4
      api_key: ${MY_KEY}
auth:
  legacy_insecure: true
`,
            env:          map[string]string{"MY_KEY": "sk-raw-expanded"},
            legacy:       true,
            wantLoadFail: false,
        },
        {
            name: "secret:// ref bypasses validation",
            configYAML: `
llm:
  providers:
    - name: test
      kind: openai
      model: gpt-4
      api_key: secret://openai/main
auth:
  legacy_insecure: false
`,
            wantLoadFail: false,
        },
        {
            name: "env:// ref bypasses validation",
            configYAML: `
llm:
  providers:
    - name: test
      kind: openai
      model: gpt-4
      api_key: env://MY_KEY
auth:
  legacy_insecure: false
`,
            wantLoadFail: false,
        },
    }

    for _, c := range cases {
        t.Run(c.name, func(t *testing.T) {
            for k, v := range c.env {
                t.Setenv(k, v)
            }
            path := filepath.Join(t.TempDir(), "config.yaml")
            if err := os.WriteFile(path, []byte(c.configYAML), 0600); err != nil {
                t.Fatal(err)
            }
            cfg, err := Load(path)
            if c.wantLoadFail {
                if err == nil {
                    t.Fatalf("expected Load to reject expanded raw literal, got cfg=%+v", cfg)
                }
                if !strings.Contains(err.Error(), "raw literal") &&
                    !strings.Contains(err.Error(), "api_key") &&
                    !strings.Contains(err.Error(), "legacy_insecure") {
                    t.Fatalf("error message does not mention the relevant constraint: %v", err)
                }
                return
            }
            if err != nil {
                t.Fatalf("unexpected Load failure: %v", err)
            }
        })
    }
}

func TestConfig_UnsetEnvVarBecomesEmpty(t *testing.T) {
    // When ${UNSET_VAR} references a missing env var, os.ExpandEnv expands it
    // to "". The post-Load api_key validation should accept empty api_key.
    t.Setenv("UNSET_VAR_EXISTS", "set")
    yaml := []byte(`
llm:
  providers:
    - name: test
      kind: openai
      model: gpt-4
      api_key: ${DOES_NOT_EXIST}
auth:
  legacy_insecure: false
`)
    path := filepath.Join(t.TempDir(), "config.yaml")
    if err := os.WriteFile(path, yaml, 0600); err != nil {
        t.Fatal(err)
    }
    cfg, err := Load(path)
    if err != nil {
        t.Fatalf("empty after expansion should be accepted: %v", err)
    }
    // The value must indeed be empty (i.e. the env var was unset).
    if cfg.LLM.Providers[0].APIKey != "" {
        t.Fatalf("unset env var should expand to empty, got %q", cfg.LLM.Providers[0].APIKey)
    }
}
```

#### Implementation: config.go

```go
import (
    "fmt"
    "os"
    "strings"

    "github.com/x6nux/yanshi/internal/guard"
    "gopkg.in/yaml.v3"
)

// validateAPIKeyRefs ensures every non-empty, non-reference api_key is either
// legacy-opted-in or rejected. This is the second gate after ParseCredentialRef:
// it catches raw literals that arrived via os.ExpandEnv of ${VAR} references,
// where the original YAML text was a legitimate-looking env var reference but
// the expanded result is a plaintext value.
func (c *Config) validateAPIKeyRefs(legacyAllowed bool) error {
    for i := range c.LLM.Providers {
        p := &c.LLM.Providers[i]
        if p.APIKey == "" {
            continue
        }
        if strings.HasPrefix(p.APIKey, "secret://") ||
            strings.HasPrefix(p.APIKey, "env://") {
            continue
        }
        // At this point p.APIKey is a plaintext value — either a raw literal
        // from the YAML or an ${VAR}-expanded value. Both bypass credential
        // resolution and fail closed without explicit opt-in.
        if !legacyAllowed {
            return fmt.Errorf(
                "config: provider %q api_key %q is a raw literal or expanded ${VAR}; "+
                    "use secret://service/account or env://VAR, "+
                    "or set auth.legacy_insecure=true to accept plaintext keys",
                p.Name, ellipsis(p.APIKey),
            )
        }
    }
    return nil
}

// ellipsis truncates a string to 32 chars for safe error messages.
func ellipsis(s string) string {
    if len(s) <= 32 {
        return s
    }
    return s[:16] + "..." + s[len(s)-13:]
}
```

在 `Load` 末尾 `cfg.applyDefaults()` 之后调用：

```go
func Load(path string) (*Config, error) {
    data, err := os.ReadFile(path)
    if err != nil {
        return nil, err
    }
    expanded := os.ExpandEnv(string(data))
    var cfg Config
    if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
        return nil, err
    }
    cfg.applyDefaults()
    if err := cfg.validateAPIKeyRefs(cfg.Auth.LegacyInsecure); err != nil {
        return nil, err
    }
    return &cfg, nil
}
```

注意：现有 `internal/config/config.go` 尚无 `Auth` 字段（D3 计划 Task 1 才添加 `AuthConfig`）。实施者在 D3 Task 1 完成**后**添加此验证，或在 Load 中单独判断 `legacy_insecure` 值（如果 AuthConfig 尚未添加，则默认 `legacyAllowed=false`）。

#### GREEN

```sh
go test ./internal/config -run "TestConfig_(RejectsExpandedVarAsRawLiteral|UnsetEnvVarBecomesEmpty)"
```

#### Commit

```text
fix(config): reject api_key as raw literal after os.ExpandEnv (C-batch C1)

Adds Load-time validation that rejects plaintext api_key values not using
secret:// or env:// references unless legacy_insecure is true. Catches
the case where a ${VAR} reference is expanded to a literal by os.ExpandEnv
before unmarshalling, which would otherwise bypass the credential-ref gate.
```

---

### C2 — checkedSecondsDuration overflow guards

**问题:** 计划中多处将 `int` 秒值直接转换为 `time.Duration` 并乘以 `time.Second`（行 3754、3763、4055，`time.Duration(expiresIn) * time.Second`）。当 `expiresIn` 或 `interval` 来自不可信的 OAuth 端点或配置时，其值可能在 `time.Duration` 的 int64 纳秒空间溢出。Go 的 `time.Duration` 是 int64 纳秒，`time.Second = 1e9`，因此 `int64 > math.MaxInt64 / 1e9 ≈ 9.22e18` 时乘法溢出。

**修复:** 提取 `safeDurationFromSeconds` 辅助函数，在所有 `seconds * time.Second` 的位置替换。该函数在溢出时返回 `math.MaxInt64`（约 292 年），确保加法结果在可预见的范围内。

#### RED: auth_test.go

```go
func TestSafeDurationFromSeconds(t *testing.T) {
    cases := []struct {
        name     string
        seconds  int
        want     time.Duration
        wantZero bool
    }{
        {name: "zero", seconds: 0, wantZero: true},
        {name: "negative", seconds: -1, wantZero: true},
        {name: "one hour", seconds: 3600, want: 3600 * time.Second},
        {name: "one day", seconds: 86400, want: 86400 * time.Second},
        {name: "ten years", seconds: 315_360_000, want: 315_360_000 * time.Second},
        {name: "max int32", seconds: math.MaxInt32,
            want: time.Duration(math.MaxInt32) * time.Second},
        // Overflow guard: this value would overflow int64 when multiplied by 1e9.
        {name: "overflow clamped", seconds: math.MaxInt64 / 1_000_000_000 + 1,
            want: time.Duration(math.MaxInt64)},
    }
    for _, c := range cases {
        t.Run(c.name, func(t *testing.T) {
            got := safeDurationFromSeconds(c.seconds)
            if c.wantZero {
                if got != 0 {
                    t.Fatalf("safeDurationFromSeconds(%d) = %v, want 0", c.seconds, got)
                }
                return
            }
            if got != c.want {
                t.Fatalf("safeDurationFromSeconds(%d) = %v, want %v", c.seconds, got, c.want)
            }
        })
    }
}
```

#### Implementation: auth.go

```go
import (
    "math"
    "time"
)

// safeDurationFromSeconds converts an int seconds value to time.Duration
// without overflow. Negative or zero returns 0. When seconds * time.Second
// would overflow int64, the maximum representable time.Duration is returned
// (~292 years), so callers can still Add() without wrapping around.
func safeDurationFromSeconds(seconds int) time.Duration {
    if seconds <= 0 {
        return 0
    }
    s := int64(seconds)
    if s > math.MaxInt64/int64(time.Second) {
        return time.Duration(math.MaxInt64)
    }
    return time.Duration(s) * time.Second
}
```

替换所有 `time.Duration(seconds) * time.Second` 的调用点（行 3754、3763、4055）：

```go
// 行 3754（device.go Authorize 的 expiresAt）:
expiresAt := clk.Now().Add(safeDurationFromSeconds(expiresIn))

// 行 3763（device.go Authorize 的 interval）:
currentInterval := safeDurationFromSeconds(interval)

// 行 4055（auth.go commitDeviceToken 的 expiresAt）:
expiresAt = m.clk.Now().Add(safeDurationFromSeconds(tok.ExpiresIn))
```

#### GREEN

```sh
go test ./internal/auth -run TestSafeDurationFromSeconds
```

验证这些调用点已替换：

```sh
grep -n 'time.Duration.*time.Second' internal/auth/device.go internal/auth/auth.go
# Expected: only safeDurationFromSeconds in auth/auth.go, no direct conversion in device.go
```

#### Commit

```text
fix(auth): add safeDurationFromSeconds overflow guard (C-batch C2)

All conversions from OAuth expires_in seconds to time.Duration now go
through safeDurationFromSeconds, which clamps to math.MaxInt64 instead of
overflowing. Covers Authorize (interval, expires_at) and commitDeviceToken
(expires_at).
```

---

### C3 — txMu transaction lock

**问题:** Task 14 中 `commitDeviceToken`、`Logout`、`Status` 三者对 `Manager` 的内部状态（`store`、`metadata`）进行读/写，但没有任何锁保护。当多个 goroutine 同时调用这些方法时（例如并发 status 查询与 device auth 完成），`metadata` map 的写入与读取会 data race；`commitDeviceToken` 的 snapshot/restore 序列也会因并发 `Logout` 或第二个 `commitDeviceToken` 的插入而得到不一致的快照。

**修复:** 在 `Manager` 结构体中新增 `txMu sync.Mutex`，将三个互斥方法的主体包裹在 `m.txMu.Lock()` / `defer m.txMu.Unlock()` 内。Status 和 Logout 是单 backend 操作可简短持有，commitDeviceToken 跨两个 backend 的补偿事务在最外层包裹。

#### RED: lifecycle_test.go

```go
func TestManager_CommitAndLogoutAreSerializedByTxMu(t *testing.T) {
    // This is a compilation/race-detection test: the presence of txMu in the
    // Manager struct and the Lock/Unlock calls in each method is what matters.
    // go test -race must not report races on the metadata map or store calls
    // when Status, Logout, and commitDeviceToken run concurrently.
    credentials := &fakeCredentialStore{values: map[string]string{
        credentialKey("openai", "main"): "existing-token",
    }}
    metadata := &memoryMetadataStore{rows: map[string]AuthMetadata{
        metadataKey("openai", "main"): {Source: "device"},
    }}
    manager := managerWithCredentialStore(t, credentials)
    manager.SetMetadataStore(metadata)
    manager.SetClock(newFakeClock(100))
    manager.SetSleeper(&fakeSleeper{clock: newFakeClock(100)})

    var wg sync.WaitGroup
    for i := 0; i < 10; i++ {
        wg.Add(1)
        go func() {
            defer wg.Done()
            _, _ = manager.Status("openai", "main")
        }()
        wg.Add(1)
        go func() {
            defer wg.Done()
            _ = manager.commitDeviceToken("openai", "main", &DeviceToken{
                AccessToken: "new-token",
                ExpiresIn:   3600,
                TokenType:   "Bearer",
            })
        }()
        wg.Add(1)
        go func() {
            defer wg.Done()
            _ = manager.Logout("openai", "main")
        }()
    }
    wg.Wait()
}
```

检查编译：

```go
// compileCheck ensures txMu is present and exported for test access
// (txMu is unexported so the test uses it via the struct literal trick below).
var _ = auth.Manager{}
var _ = func(m *auth.Manager) {
    // If this doesn't compile, txMu sync.Mutex is missing from Manager struct.
    // As txMu is unexported, the compiler test is deferred to the "go vet"
    // no-race check; test code calls the public API and relies on the
    // race detector to catch the absence.
}
```

#### Implementation: auth.go 的 Manager struct 与相关方法

在 Task 7/14 的 Manager 定义末尾添加 `txMu`：

```go
type Manager struct {
    secrets         *secrets.Manager
    store           secrets.Store
    deviceProviders map[string]DeviceProvider
    clk             Clock
    slp             Sleeper
    metadata        MetadataStore
    txMu            sync.Mutex // serializes commitDeviceToken / Status / Logout
}
```

在 Task 14 的 `commitDeviceToken`、`Status`、`Logout` 三方法首行各加锁：

```go
func (m *Manager) commitDeviceToken(provider, account string, tok *DeviceToken) error {
    m.txMu.Lock()
    defer m.txMu.Unlock()
    // ... existing body unchanged
}

func (m *Manager) Status(provider, account string) (Status, error) {
    m.txMu.Lock()
    defer m.txMu.Unlock()
    // ... existing body unchanged
}

func (m *Manager) Logout(provider, account string) error {
    m.txMu.Lock()
    defer m.txMu.Unlock()
    // ... existing body unchanged
}
```

注意：`Status` 使用 `Lock()` 而非 `RLock()`，因为其读操作需要在事务隔离下看到一致 snapshot。若后续性能开销显著，可改用 `sync.RWMutex`，但 C3 优先保证正确性。

#### GREEN

```sh
go test -race ./internal/auth -run "TestManager_(Status|Logout|Commit)" 2>&1 | grep -q "WARNING: DATA RACE" && echo "FAIL: race detected" || echo "PASS: no race"

# Optional: lock contention benchmark
go test -bench . ./internal/auth -benchtime=2x
```

#### Commit

```text
fix(auth): add txMu transaction lock to commitDeviceToken/Status/Logout (C-batch C3)

Wraps the three mutually-serialized Manager methods in a sync.Mutex so
concurrent reads (Status) and writes (commitDeviceToken, Logout) see a
consistent snapshot of store + metadata. Prevents data races when device
auth completion, status polling, and logout execute concurrently.
```

---

### C4 — concurrency test

**问题:** 虽然 C3 的 txMu 解决了串行化问题，但还缺少一个显式的并发安全性测试文件，在 `go test -race` 下验证 `commitDeviceToken`、`Logout`、`Status` 的多种交错组合不会触发 data race、panic、或挂起。该测试直接在包级使用 `t.Parallel()` 和 `go test -race` 运行，确保 CI 管道的 race 门禁覆盖这些方法。

**修复:** 在 `internal/auth/lifecycle_test.go` 中新增 `TestManager_ConcurrentAccessIsRaceFree`，用 16 个 goroutine 以随机间隔同时调用三个方法。

#### RED: lifecycle_test.go

```go
func TestManager_ConcurrentAccessIsRaceFree(t *testing.T) {
    t.Parallel() // run in parallel with other tests under -race

    // Pre-populate store and metadata so all three methods have work to do.
    credentials := &fakeCredentialStore{values: map[string]string{
        credentialKey("openai", "main"): "v1",
    }}
    metadata := &memoryMetadataStore{rows: map[string]AuthMetadata{
        metadataKey("openai", "main"): {Source: "device"},
    }}
    manager := managerWithCredentialStore(t, credentials)
    manager.SetMetadataStore(metadata)
    clk := newFakeClock(0)
    manager.SetClock(clk)
    manager.SetSleeper(&fakeSleeper{clock: clk})

    const goroutines = 16
    var wg sync.WaitGroup
    for i := 0; i < goroutines; i++ {
        wg.Add(1)
        go func() {
            defer wg.Done()
            _, _ = manager.Status("openai", "main")
        }()
        wg.Add(1)
        go func() {
            defer wg.Done()
            _ = manager.commitDeviceToken("openai", "main", &DeviceToken{
                AccessToken: "v2",
                ExpiresIn:   3600,
                TokenType:   "Bearer",
            })
        }()
        wg.Add(1)
        go func() {
            defer wg.Done()
            _ = manager.Logout("openai", "main")
        }()
    }
    wg.Wait()

    // After all goroutines complete, the metadata must be internally consistent.
    // It is legitimate for any one of the three call sequences to have won,
    // but the store and metadata must agree on a consistent state.
    st, err := manager.Status("openai", "main")
    if err != nil {
        t.Fatalf("final Status: %v", err)
    }
    if st.Authenticated && st.Source != "secret" && st.Source != "device" {
        t.Fatalf("final Status has inconsistent source=%q", st.Source)
    }
}
```

#### Implementation

不需要额外的生产代码；测试依赖 C3 的 `txMu` 和现有的 `commitDeviceToken`/`Status`/`Logout` 实现。测试中的 `fakeCredentialStore` 和 `memoryMetadataStore` 已由 Task 7/14 的测试文件定义，但需确认两者是并发安全的：

`fakeCredentialStore` 的 `Set`/`Get`/`Delete` 必须使用 `sync.RWMutex`：

```go
type fakeCredentialStore struct {
    mu        sync.RWMutex
    values    map[string]string
    failSetCall int
    failSetErr  error
    setCalls    int
}

func (f *fakeCredentialStore) Get(service, account string) (string, error) {
    f.mu.RLock()
    defer f.mu.RUnlock()
    v, ok := f.values[service+"\x00"+account]
    if !ok {
        return "", secrets.ErrSecretNotFound
    }
    return v, nil
}

func (f *fakeCredentialStore) Set(service, account, secret string) error {
    f.mu.Lock()
    defer f.mu.Unlock()
    f.setCalls++
    if f.failSetCall > 0 && f.setCalls >= f.failSetCall {
        return f.failSetErr
    }
    f.values[service+"\x00"+account] = secret
    return nil
}
```

`memoryMetadataStore` 也需要 `sync.Mutex`（在 C5 中覆盖）。

#### GREEN

```sh
go test -race -count=3 ./internal/auth -run TestManager_ConcurrentAccessIsRaceFree
```

`-count=3` 重复运行三次以暴露任何时序依赖的 race。期望输出三次 `PASS` 且无 race 告警。

#### Commit

```text
test(auth): add concurrent-access race-detection test (C-batch C4)

Adds TestManager_ConcurrentAccessIsRaceFree with 16 goroutines exercising
Status, commitDeviceToken, and Logout concurrently. Requires C3's txMu
and thread-safe fake stores. Run with `go test -race` in CI.
```

---

### C5 — metadata atomic contract

**问题:** `MetadataStore` 接口（Task 7/14 定义）的 `SaveAuthMetadata`/`LoadAuthMetadata`/`DeleteAuthMetadata` 在文档和代码中都没有明确声明其原子性保证。`authSQLiteAdapter`（Task 8/14）的 `SaveAuthMetadata` 和 `DeleteAuthMetadata` 不做 BEGIN/COMMIT 包装；`memoryMetadataStore`（Task 14 测试用）没有互斥锁保护其 map。当 `MetadataStore` 被并发访问时（见 C3/C4），Save 和 Load 的交错可能导致读到不一致的快照。

**修复:**
1. 在 `MetadataStore` 接口的 doc comment 中声明"每方法原子性"。
2. 在 `authSQLiteAdapter` 中增加 `txMu sync.Mutex`（或 `*sql.DB` 的 implicit tx），使 Save/Load/Delete 互斥。
3. 在 `memoryMetadataStore` 中增加 `sync.Mutex` 保护 map。
4. 新增测试验证并发 Get/Set/Delete 的原子性。

#### RED: auth_test.go（接口文档）与 lifecycle_test.go（并发原子性验证）

接口文档在 `auth.go` 中的更新：

```go
// MetadataStore provides atomic per-method access to auth lifecycle metadata.
// Each Save, Load, and Delete call MUST be serialised with respect to other
// calls on the same store instance: a goroutine observing a nil error from
// SaveAuthMetadata is guaranteed that a subsequent (or concurrent) Load will
// see the new row or the old row — never a partially-written mix of the two.
// Implementations use a sync.Mutex or a SQLite transaction internally.
type MetadataStore interface {
    SaveAuthMetadata(provider, account string, meta AuthMetadata) error
    LoadAuthMetadata(provider, account string) (AuthMetadata, error)
    DeleteAuthMetadata(provider, account string) error
}
```

并发原子性验证测试（放在 `internal/auth/lifecycle_test.go`，`package auth`；需新增 `import "github.com/x6nux/yanshi/internal/store"`、`"fmt"`、`"sync"`。`internal/store` 反向 import `internal/auth` 不构成 test-binary cycle，因为 production `auth` 先于 `store` 构建、`store` 先于 auth 的 test variant 构建）：

```go
func TestMetadataStore_ConcurrentSetLoadDeleteIsAtomic(t *testing.T) {
    // This test verifies C5's atomic contract: concurrent operations on the
    // same metadata store instance never produce a partially-written row.
    // The store under test is the real SQLite adapter (if available) or the
    // in-memory adapter used in lifecycle tests. Both must be race-free.

    st, err := store.Open(":memory:")
    if err != nil {
        t.Fatal(err)
    }
    defer st.Close()
    // lifecycle_test.go is `package auth`, so AuthMetadata is in-package and
    // AuthMetadataFromDB lives in `store`. Do NOT prefix either with `auth.`.
    adapter := store.AuthMetadataFromDB(st.DB)

    var wg sync.WaitGroup
    const goroutines = 8
    for i := 0; i < goroutines; i++ {
        wg.Add(1)
        i := i
        go func() {
            defer wg.Done()
            _ = adapter.SaveAuthMetadata("openai", "main", AuthMetadata{
                Source: fmt.Sprintf("iteration-%d", i),
            })
        }()
        wg.Add(1)
        go func() {
            defer wg.Done()
            _, _ = adapter.LoadAuthMetadata("openai", "main")
        }()
        wg.Add(1)
        go func() {
            defer wg.Done()
            _ = adapter.DeleteAuthMetadata("openai", "main")
        }()
    }
    wg.Wait()
}
```

#### Implementation: auth.go（接口文档）、store/auth.go（SQLite adapter txMu）、lifecycle_test.go（memoryMetadataStore）

`internal/store/auth.go` 的 `authSQLiteAdapter` 增加锁：

```go
type authSQLiteAdapter struct {
    db   *sql.DB
    txMu sync.Mutex // serialises Save/Load/Delete for atomic per-method guarantee
}

func (a *authSQLiteAdapter) SaveAuthMetadata(
    provider, account string,
    meta auth.AuthMetadata,
) error {
    a.txMu.Lock()
    defer a.txMu.Unlock()
    expiresAt := int64(0)
    if !meta.ExpiresAt.IsZero() {
        expiresAt = meta.ExpiresAt.Unix()
    }
    _, err := a.db.Exec(
        `INSERT INTO auth_metadata ... ON CONFLICT DO UPDATE ...`,
        // ... args unchanged
    )
    return err
}

func (a *authSQLiteAdapter) LoadAuthMetadata(
    provider, account string,
) (auth.AuthMetadata, error) {
    a.txMu.Lock()
    defer a.txMu.Unlock()
    // ... existing body unchanged
}

func (a *authSQLiteAdapter) DeleteAuthMetadata(
    provider, account string,
) error {
    a.txMu.Lock()
    defer a.txMu.Unlock()
    // ... existing body unchanged
}
```

`internal/auth/lifecycle_test.go` 的 `memoryMetadataStore` 增加锁：

```go
type memoryMetadataStore struct {
    mu        sync.Mutex
    rows      map[string]AuthMetadata
    loadErr   error
    deleteErr error
}

func (m *memoryMetadataStore) SaveAuthMetadata(
    provider, account string,
    meta AuthMetadata,
) error {
    m.mu.Lock()
    defer m.mu.Unlock()
    if m.rows == nil {
        m.rows = map[string]AuthMetadata{}
    }
    m.rows[metadataKey(provider, account)] = meta
    return nil
}

func (m *memoryMetadataStore) LoadAuthMetadata(
    provider, account string,
) (AuthMetadata, error) {
    m.mu.Lock()
    defer m.mu.Unlock()
    if m.loadErr != nil {
        return AuthMetadata{}, m.loadErr
    }
    meta, ok := m.rows[metadataKey(provider, account)]
    if !ok {
        return AuthMetadata{}, ErrAuthMetadataNotFound
    }
    return meta, nil
}

func (m *memoryMetadataStore) DeleteAuthMetadata(provider, account string) error {
    m.mu.Lock()
    defer m.mu.Unlock()
    if m.deleteErr != nil {
        return m.deleteErr
    }
    key := metadataKey(provider, account)
    if _, ok := m.rows[key]; !ok {
        return ErrAuthMetadataNotFound
    }
    delete(m.rows, key)
    return nil
}
```

新增 import（auth_test.go 已有 `sync`；如缺失则加）。

#### GREEN

```sh
# Race-detection test for both adapters. The SQLite adapter concurrency is
# exercised from the auth test (which imports store); this command re-runs
# the store package's adapter tests under -race as well.
go test -race ./internal/store -run TestAuthSQLiteAdapter
go test -race ./internal/auth -run "TestMetadataStore_ConcurrentSetLoadDeleteIsAtomic|TestManager_Concurrent"

# Static check: verify the interface doc updated
grep -q "MUST be serialised" internal/auth/auth.go && echo "PASS: interface doc updated"
```

注意：`store.Open(":memory:")` 假设 `internal/store` 包导出 `Open` 函数。实施时若测试文件在 `auth_test.go` 而不能引用 `store_test` 包的 unexported 函数，应使用 `store.Open` 或其测试帮助函数。

#### Commit

```text
fix(auth): add atomicity contract to MetadataStore + lock adapters (C-batch C5)

Declares the per-call atomicity guarantee on MetadataStore and enforces it
with a sync.Mutex on both authSQLiteAdapter (SQLite) and memoryMetadataStore
(test). A concurrent racy test covers both implementations under -race.
```

---

## Structural Fix Matrix — C-batch items (#21–#25)

追加到现有 20 项之后，标识 C1–C5 的复盘落点：

| # | 必修项 | 落点 | 测试/门禁 |
|---|---|---|---|
| 21 | `${VAR}` 展开后的 api_key 拒绝或 opt-in | C1 (config.go validateAPIKeyRefs) | `TestConfig_RejectsExpandedVarAsRawLiteral` |
| 22 | `time.Duration` 乘法溢出 guard | C2 (`safeDurationFromSeconds`) | `TestSafeDurationFromSeconds` |
| 23 | `txMu sync.Mutex` 串行化三个互斥方法 | C3 (Manager.txMu + Lock/Unlock) | `TestManager_CommitAndLogoutAreSerializedByTxMu` |
| 24 | 并发安全性 race-detection 测试 | C4 (lifecycle_test.go) | `go test -race -count=3 ./internal/auth -run TestManager_ConcurrentAccessIsRaceFree` |
| 25 | MetadataStore 每方法原子性保证 | C5 (接口 doc + adapter txMu) | `TestMetadataStore_ConcurrentSetLoadDeleteIsAtomic` + `grep` 接口 doc 检查 |

## C-batch 最终门禁

与 Task 15 的显式安全 suite 一起在实施阶段末尾运行：

```sh
# C1-C5 定向测试
go test -race ./internal/config -run "TestConfig_RejectsExpandedVar|TestConfig_UnsetEnvVar"
go test -race ./internal/auth -run "TestSafeDurationFromSeconds|TestManager_CommitAndLogoutAreSerializedByTxMu|TestManager_ConcurrentAccessIsRaceFree|TestMetadataStore_ConcurrentSetLoadDeleteIsAtomic"
go test -race ./internal/store -run "TestAuthSQLiteAdapter"

# 全包 race-detection 门禁（含所有 C-batch 修复）
go test -race ./internal/config ./internal/auth ./internal/store ./internal/secrets

# 确保注释/文档已更新
grep -q "MUST be serialised" internal/auth/auth.go && echo "C5 doc: pass"
grep -q "validateAPIKeyRefs" internal/config/config.go && echo "C1 gate: pass"
grep -q "safeDurationFromSeconds" internal/auth/auth.go && echo "C2 guard: pass"
grep -q "txMu" internal/auth/auth.go && echo "C3 lock: pass"
```

期望：每条命令 exit 0；race 检测零告警。实施期间 C-batch 门禁在所有 15 个 Task 完成并提交后运行。


