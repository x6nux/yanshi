# Yanshi 全局重命名迁移计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 将 autocode（module `github.com/x6nux/autocode`，binary `autocode`）全局重命名为 Yanshi（`github.com/x6nux/yanshi`，binary `yanshi`），全程保持 `go build` / `go vet` / `go test` 绿。

**Architecture:** 按"替换串类别"分 10 个 task。每类一个批量 `sed`（作用于 git 跟踪文件）+ 验证（build/vet/test/`-h`）+ commit。**非 TDD** —— rename 是机械替换，验证依赖现有编译/测试门禁，不是"先写失败测试"。

**Tech Stack:** Go 1.26.4、Git Bash（GNU `sed`/`find`/`xargs`）、modernc.org/sqlite、Bubble Tea。

**Spec:** `docs/superpowers/specs/2026-07-19-yanshi-rename-design.md`

## 全局约定（每个 sed 步骤适用）

- **只改 git 跟踪文件**：一律用 `git ls-files -z -- '<glob>' | xargs -0 sed -i ...`，不碰未跟踪临时文件（`_analyze_cov.go` / `cov2.html` / `pkglist.tmp`，以及未跟踪的 `docs/feature-comparison-with-codex.md`）。
- **CRLF**：Git Bash 的 GNU `sed -i` 保留原行尾。每步后 `git diff --stat` 抽查 —— 改动行数应与预期替换量匹配；若某文件显示"整文件增删"说明行尾被破坏，`git checkout -- <file>` 重来。
- **依赖顺序**：import → env → magic 文件名 → 路径/db → 剩余文本。前序 task 消耗掉的串不会被后序误伤（Task 7 全局 `s/autocode/yanshi/g` 之所以安全，是因为此时剩余的 `autocode` 全是纯文本词）。
- **每个 task 末尾 commit**：在 `main` 分支执行（用户已授权）。`git add -u && git commit -m "..."`。

---

### Task 1: Baseline 验证（起点必须绿）

**Files:** 无改动。

- [ ] **Step 1: 全量构建 + vet + 测试**

```bash
go build ./...
go vet ./...
go test ./...
```

Expected: build/vet 零错误；test 全过（带 `e2e_real` tag 的 skip 为预期）。**若起点非绿，停止并先修。**

- [ ] **Step 2: 不 commit（无改动）**

---

### Task 2: module path + import 全量替换

**Files:**
- Modify: `go.mod:1`
- Modify: 所有 `.go`（import path，大头在此）

- [ ] **Step 1: 改 go.mod module 声明**

`go.mod` 第 1 行 `module github.com/x6nux/autocode` → `module github.com/x6nux/yanshi`。

- [ ] **Step 2: 全量替换 import path**

```bash
git ls-files -z -- '*.go' | xargs -0 sed -i 's|github.com/x6nux/autocode|github.com/x6nux/yanshi|g'
```

- [ ] **Step 3: go mod tidy 重算依赖**

```bash
go mod tidy
```

- [ ] **Step 4: 验证**

```bash
go build ./...
go vet ./...
go test ./...
git diff --stat
```

Expected: 全绿；`git diff --stat` 改动文件数与 import 命中数匹配，无整文件重写。

- [ ] **Step 5: 残留检查**

```bash
git ls-files -z -- '*.go' | xargs -0 grep -n 'x6nux/autocode' || echo clean
```

Expected: `clean`。

- [ ] **Step 6: Commit**

```bash
git add -u && git commit -m "refactor(yanshi): rename module path autocode -> yanshi"
```

---

### Task 3: 主 binary 目录 git mv

**Files:**
- Rename: `cmd/autocode/` → `cmd/yanshi/`（含 `main.go`、`version.go`、`main_test.go`、`exec_test.go`）

- [ ] **Step 1: git mv 目录**

```bash
git mv cmd/autocode cmd/yanshi
```

- [ ] **Step 2: 验证新路径编译**

```bash
go build ./cmd/yanshi
go test ./cmd/yanshi
```

Expected: 通过（package 仍为 `main`，不受目录名影响）。

- [ ] **Step 3: Commit**

```bash
git add -u && git commit -m "refactor(yanshi): mv cmd/autocode -> cmd/yanshi"
```

---

### Task 4: 环境变量前缀 ACODE_ → YANSHI_

**Files:**
- Modify: `cmd/yanshi/main.go`、`internal/acp/spawn.go`、`internal/acp/spawn_test.go`、`internal/acp/e2e_real_test.go`、`internal/vcs/e2e_acp_test.go`、`internal/agent/goalloop/implementer.go`、`internal/agent/goalloop/implementer_vcs_test.go`

> `ACODE_`（A-C-O-D-E，autocode 缩写）是 env 前缀，**不是** `AUTOCODE_`。全局只作为 env var 名出现，可安全整串替换。

- [ ] **Step 1: 全量替换 ACODE_ → YANSHI_（仅 .go）**

```bash
git ls-files -z -- '*.go' | xargs -0 sed -i 's/ACODE_/YANSHI_/g'
```

- [ ] **Step 2: 验证**

```bash
go build ./...
go vet ./...
go test ./...
```

Expected: 全绿。

- [ ] **Step 3: 残留检查**

```bash
git ls-files -z -- '*.go' | xargs -0 grep -n 'ACODE_' || echo clean
```

Expected: `clean`。

- [ ] **Step 4: Commit**

```bash
git add -u && git commit -m "refactor(yanshi): rename env prefix ACODE_ -> YANSHI_"
```

---

### Task 5: magic 文件名 .autocodeignore / .autocode-plugin

**Files:**
- Modify: `internal/vcs/vcs.go`（`.autocodeignore` 读取于 `:535`）、`internal/vcs/vcs_test.go`
- Modify: `internal/skills/plugins.go`（`.autocode-plugin` 发现于 `:31`）、`internal/skills/skills_test.go`

- [ ] **Step 1: 替换两个 magic 名**

```bash
git ls-files -z -- '*.go' | xargs -0 sed -i 's/\.autocodeignore/.yanshiignore/g; s/\.autocode-plugin/.yanshi-plugin/g'
```

- [ ] **Step 2: 验证**

```bash
go build ./...
go test ./internal/vcs ./internal/skills
```

Expected: vcs 测试用 `.yanshiignore`、skills 测试用 `.yanshi-plugin`，全过。

- [ ] **Step 3: 残留检查**

```bash
git ls-files -z -- '*.go' | xargs -0 grep -n 'autocodeignore\|autocode-plugin' || echo clean
```

Expected: `clean`。

- [ ] **Step 4: Commit**

```bash
git add -u && git commit -m "refactor(yanshi): rename magic files (.autocodeignore/.autocode-plugin)"
```

---

### Task 6: 数据/路径目录与 db 默认名

**Files:**
- Modify: `internal/bootstrap/bootstrap.go:93`（`~/.autocode/worktrees` 默认）、`internal/cli/doctor.go:347`、`internal/lockfile/lockfile.go:34`（cache 子目录 `autocode`）、`cmd/yanshi/main.go`（`~/.autocode/worktrees`、`autocode.db`）、`internal/config/config_test.go`（`~/.autocode/skills` 断言）等

- [ ] **Step 1: 替换 ~/.autocode/ → ~/.yanshi/（.go）**

```bash
git ls-files -z -- '*.go' | xargs -0 sed -i 's|~/\.autocode/|~/.yanshi/|g'
```

- [ ] **Step 2: 替换 autocode.db → yanshi.db（.go）**

```bash
git ls-files -z -- '*.go' | xargs -0 sed -i 's/autocode\.db/yanshi.db/g'
```

- [ ] **Step 3: lockfile cache 目录名（精确）**

```bash
sed -i 's|"autocode", "run"|"yanshi", "run"|g' internal/lockfile/lockfile.go
```

（`lockfile.go:34` 的 `filepath.Join(base, "autocode", "run")` —— lockfile 写到 `<cache>/autocode/run/`，改为 `yanshi`。）

- [ ] **Step 4: 验证**

```bash
go build ./...
go vet ./...
go test ./...
git diff --stat
```

Expected: 全绿。

- [ ] **Step 5: 残留检查**

```bash
git ls-files -z -- '*.go' | xargs -0 grep -nE '\.autocode/|autocode\.db' || echo clean
```

Expected: `clean`。

- [ ] **Step 6: Commit**

```bash
git add -u && git commit -m "refactor(yanshi): rename data dir ~/.autocode + autocode.db -> yanshi"
```

---

### Task 7: 剩余人机可见文本 autocode → yanshi

**Files:**
- Modify: `cmd/yanshi/main.go`（usage 文本、stderr 前缀如 `"autocode serve: ..."`、`flag.NewFlagSet("autocode")`、`fmt.Println("autocode", Version)`）、`internal/bootstrap/bootstrap.go`（stderr/注释）、`internal/config/config.go`（注释）及所有 .go 中剩余的小写 `autocode`。

> 到此为止，import / `ACODE_` / `.autocodeignore` / `.autocode-plugin` / `~/.autocode/` / `autocode.db` / lockfile-dir 已全部不含 `autocode`。剩余 `autocode` 全部是纯文本词（命令名、stderr 前缀、注释），可安全全局替换为 `yanshi`。

- [ ] **Step 1: 替换前确认剩余分布**

```bash
git ls-files -z -- '*.go' | xargs -0 grep -n 'autocode'
```

记录命中 —— 应只剩文本/注释类（usage、stderr、`flag.NewFlagSet("autocode")`、注释）。

- [ ] **Step 2: 全量替换剩余小写 autocode → yanshi（.go）**

```bash
git ls-files -z -- '*.go' | xargs -0 sed -i 's/autocode/yanshi/g'
```

- [ ] **Step 3: 验证**

```bash
go build -o yanshi.exe ./cmd/yanshi
go vet ./...
go test ./...
./yanshi.exe -h
```

Expected: build/test 全绿；`-h` 输出的 usage 全部显示 `yanshi`（无 `autocode`）。

- [ ] **Step 4: 残留检查**

```bash
git ls-files -z -- '*.go' | xargs -0 grep -ni 'autocode' || echo clean
```

Expected: `clean`（.go 里无任何大小写 `autocode`）。

- [ ] **Step 5: Commit**

```bash
git add -u && git commit -m "refactor(yanshi): rename user-visible strings autocode -> yanshi"
```

---

### Task 8: config.example.yaml

**Files:**
- Modify: `config.example.yaml`（`sqlite_path: "autocode.db"`、`~/.autocode/skills`、`~/.autocode/plugins`、`~/.autocode/worktrees`）

> `config.yaml`（gitignored）不在范围；用户本地若有需自行改。

- [ ] **Step 1: 替换 db 名与路径**

```bash
sed -i 's/autocode\.db/yanshi.db/g; s|~/\.autocode/|~/.yanshi/|g' config.example.yaml
```

- [ ] **Step 2: 验证**

```bash
go test ./internal/config
```

Expected: 通过（`config_test.go` 的 `~/.autocode/skills` 断言已在 Task 6 改为 `~/.yanshi/skills`，与本步一致）。

- [ ] **Step 3: Commit**

```bash
git add config.example.yaml && git commit -m "refactor(yanshi): config.example.yaml autocode -> yanshi"
```

---

### Task 9: 活文档（README 重写 + 声明/tagline/Why + CLAUDE.md + docs + skills）

**Files:**
- Modify: `README.md`、`CLAUDE.md`、`docs/vcs.md`、`docs/skills-authoring.md`、`docs/analysis-report.md`、`skills/dev-team-feature/SKILL.md`
- **不改**：`docs/superpowers/specs/2026-07-{10..18}-*.md`、`docs/superpowers/plans/2026-07-{10..18}-*.md`（历史，spec §4.3）
- **注意**：`docs/feature-comparison-with-codex.md` 当前未跟踪（`git status` `??`）—— `git ls-files` 不会触及它；若用户要保留，先 `git add` 再单独处理，否则跳过。

- [ ] **Step 1: 批量替换文档中的 magic 名/路径/db/env 前缀**

```bash
git ls-files -z -- 'README.md' 'CLAUDE.md' 'docs/*.md' 'skills/**/*.md' \
  | grep -zvE 'docs/superpowers/(specs|plans)/2026-07-1[0-8]-' \
  | xargs -0 sed -i 's/\.autocodeignore/.yanshiignore/g; s/\.autocode-plugin/.yanshi-plugin/g; s|~/\.autocode/|~/.yanshi/|g; s/autocode\.db/yanshi.db/g; s/ACODE_/YANSHI_/g'
```

- [ ] **Step 2: 文档正文 `autocode` 语境化替换（命令→yanshi，产品名→Yanshi）**

文档里 `autocode` 有两种语境，`sed` 单规则难可靠区分，分三步：

```bash
# (a) 命令调用语境：./autocode  或  反引号内 `autocode<空格>
git ls-files -z -- 'README.md' 'CLAUDE.md' 'docs/*.md' 'skills/**/*.md' \
  | grep -zvE 'docs/superpowers/(specs|plans)/2026-07-1[0-8]-' \
  | xargs -0 sed -i 's|\./autocode|./yanshi|g; s|`autocode |`yanshi |g'

# (b) 剩余 autocode → Yanshi（产品名/标题语境）
git ls-files -z -- 'README.md' 'CLAUDE.md' 'docs/*.md' 'skills/**/*.md' \
  | grep -zvE 'docs/superpowers/(specs|plans)/2026-07-1[0-8]-' \
  | xargs -0 sed -i 's/autocode/Yanshi/g'
```

- [ ] **Step 3: Edit 逐文件核对命令语境遗漏**

(a) 的规则覆盖 `./autocode` 与 `` `autocode `` 开头，但 README 中还有**行首裸命令**（如 code block 里的 `autocode serve`、`autocode goal`、`autocode chat --no-tui`）。用 Edit 工具逐处把 `README.md` / `CLAUDE.md` 中 code block 行首的 `Yanshi`（被 (b) 误大写）改回 `yanshi`。核对时对照 Step 4 的 grep 命中。

- [ ] **Step 4: README 顶部加 tagline + not-affiliated 声明**

把 `README.md` 第 1 行 `# Yanshi` 替换为：

```markdown
# Yanshi

> **Yanshi — the self-driven coding agent.** 偃师 —— 自驱的编码 agent

Named after 偃师 (Yǎnshī), the legendary artisan who built an autonomous automaton in 《列子·汤问》. Not affiliated with [chaitin/yanshi](https://github.com/chaitin/yanshi).
```

- [ ] **Step 5: README 加 "Why Yanshi?" 小节**

在介绍段之后、`## Quick start` 之前插入：

```markdown
## Why Yanshi?

偃师出自《列子·汤问》—— 上古工匠，造出能歌舞的"倡者"（自动人偶）献给周穆王；剖开只见皮革木胶颜料，内别无他物。世界最早的"自动机械"传说之一。

Yanshi 造的是会自己动的编码 agent：自驱动 goal loop、ReAct 编排、子代理委派、ACP 拉起外部 agent。名字就是产品语义。
```

- [ ] **Step 6: 验证活文档无残留**

```bash
git ls-files -z -- 'README.md' 'CLAUDE.md' 'docs/*.md' 'skills/**/*.md' \
  | grep -zvE 'docs/superpowers/(specs|plans)/2026-07-1[0-8]-' \
  | xargs -0 grep -ni 'autocode\|ACODE' || echo clean
```

Expected: `clean`（本次 design spec / plan 自身命中是元层面，不在 `git ls-files` 的上述 glob 命中范围内 —— 它们在 `docs/superpowers/{specs,plans}/` 下且文件名为 `2026-07-19-*`，不被 `grep -zv 2026-07-1[0-8]` 排除，但也不在活文档核对目标里，手动放过）。

- [ ] **Step 7: Commit**

```bash
git add -u && git commit -m "docs(yanshi): rewrite README + living docs autocode -> Yanshi"
```

---

### Task 10: 最终全量验证 + 残留扫描

**Files:** 无新改动；收尾验证。

- [ ] **Step 1: 全量 build/vet/test**

```bash
go build -o yanshi.exe ./cmd/yanshi
go vet ./...
go test ./...
```

Expected: 全绿（`e2e_real` skip 为预期）。

- [ ] **Step 2: 启动自检**

```bash
./yanshi.exe -h
timeout 5 ./yanshi.exe --fake-model -inprocess
```

Expected: `-h` 退出 0，usage 全 `yanshi`；`--fake-model -inprocess` 无报错启动（timeout 杀掉）。

- [ ] **Step 3: subcommand 可用性**

```bash
./yanshi.exe doctor -json | head
./yanshi.exe vcs-mcp </dev/null
```

Expected: `doctor` 输出 JSON 自检；`vcs-mcp` 因缺 `YANSHI_REPO_ID` 等环境变量会退出，但应正常启动并报缺参，而非崩溃。

- [ ] **Step 4: 全库残留扫描（排除历史/元文档/产物）**

```bash
git ls-files -z -- '*.go' '*.yaml' '*.yml' '*.md' \
  | grep -zvE 'docs/superpowers/(specs|plans)/2026-07-1[0-8]-' \
  | xargs -0 grep -niE 'autocode|ACODE'
```

Expected: 仅 `docs/superpowers/specs/2026-07-19-yanshi-rename-design.md` 与 `docs/superpowers/plans/2026-07-19-yanshi-rename.md`（即本 spec / plan 自身，元层面讨论 rename，正确）命中；其余 clean。

- [ ] **Step 5: go.sum / module 一致性**

```bash
go mod tidy && git diff --stat go.mod go.sum
```

Expected: 无意外变化（Task 2 已 tidy）。

- [ ] **Step 6: 最终 Commit（如有修补）**

```bash
git add -u
git diff --cached --quiet || git commit -m "chore(yanshi): post-rename cleanup"
```

---

## Self-Review

- **Spec 覆盖**：spec §4.2 七维度 → Task 2（module）/ 3（binary）/ 4（env）/ 5（magic）/ 6（路径+db）/ 7（UI 文本）/ 8（config）/ 9（文档）全覆盖；§4.3 排除项 → 每个 task 用 `git ls-files` 只碰跟踪文件 + 残留扫描排除历史 `specs/plans/2026-07-1[0-8]`；§4.4 不迁移 → 无 task（决策记在 spec）；§5.2 门禁 → 每 task 内 + Task 10。
- **Placeholder**：无 TBD / TODO；每个 sed/Edit 都给了精确命令与插入文本。
- **一致性**：替换串按依赖顺序排列（import → env → magic → path/db → 文本），前序消耗的串不会被后序误伤；Task 7 的全局 `s/autocode/yanshi/g` 在前序 task 已清理特定模式后才执行，安全。
- **风险与应对**：README 命令/产品名语境区分由 `sed`(a)/(b) + Step 3 Edit 核对兜底；CRLF 由 `git diff --stat` 抽查兜底；`go.sum` 由 `go mod tidy` 兜底。
