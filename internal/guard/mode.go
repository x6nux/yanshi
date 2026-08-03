package guard

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// PermissionMode is the interactive permission policy a session runs under. It
// governs what happens when the static PermissionProfile would DENY a tool call
// (i.e. the permission callback is about to prompt the user):
//
//   - ModeDefault:     prompt the user (the original behavior).
//   - ModeAllowEdits:  auto-approve file edit/mkdir tools; prompt for the rest.
//   - ModeYOLO:        auto-approve everything, never prompt.
//   - ModeAuto:        ask an AI to rate the call's risk 1-10; auto-approve when
//     the score is <= AutoThreshold, otherwise prompt.
//   - ModePlan:        只读模式。runtime guard（guard.PlanToolAllowed）只放行
//     只读工具与 task/checklist 元数据；任何写操作直接 deny。
//     不参与 Shift+Tab cycle（CycleMode 永不返回 ModePlan）。
type PermissionMode string

// Permission mode values.
const (
	ModeDefault    PermissionMode = "default"
	ModeAllowEdits PermissionMode = "allow-edits"
	ModeYOLO       PermissionMode = "yolo"
	ModeAuto       PermissionMode = "auto"
	ModePlan       PermissionMode = "plan"
)

// DefaultAutoThreshold is the risk ceiling ModeAuto auto-approves under when the
// user has not set one explicitly. 4 keeps safe/light commands (ls, cat, build,
// git status) flowing and prompts for writes, deletes, and anything destructive.
const DefaultAutoThreshold = 4

// allModes 是 Modes() 返回的完整列表（含 ModePlan）。供 TUI 的 /mode 帮助、
// status line 与 NormalizeMode 校验使用。
//
// cycleOrder 是 Shift+Tab 推进的子集 —— ModePlan 不在里面，让用户只有显式
// /plan 才能进入只读模式，避免误切（已决策约束 §7）。
var (
	allModes   = []PermissionMode{ModeDefault, ModeAllowEdits, ModeAuto, ModeYOLO, ModePlan}
	cycleOrder = []PermissionMode{ModeDefault, ModeAllowEdits, ModeAuto, ModeYOLO}
)

// Modes returns the ordered list of all permission modes (including ModePlan),
// for /mode help and external iteration. 调用方不可假定顺序与 CycleMode 一致。
func Modes() []PermissionMode { return append([]PermissionMode(nil), allModes...) }

// planAllowedTools 是 Plan mode 的唯一工具白名单（已决策约束 §4）：
// 允许读取、任务元数据创建与计划/清单更新；拒绝 shell、文件写、网络写、
// VCS 写、task_cancel。task_create 是 plan-safe 元数据写，解决"进入 Plan mode
// 后没有 task_id"的自举问题。
//
// runtime Authorize（tools.PlanToolAllowed 跨包 re-export）与 orchestrator 的
// filterPlanTools 共用它，杜绝两份白名单漂移。
var planAllowedTools = map[string]struct{}{
	"fs_read": {}, "fs_list": {},
	"memory_search": {}, "memory_recall": {},
	"vcs_log": {}, "vcs_diff": {},
	"time_now": {}, "summarize": {}, "analysis": {},
	"task_create": {}, "task_list": {}, "task_read": {},
	"update_plan": {}, "checklist_write": {}, "checklist_add": {},
	"checklist_update": {}, "checklist_list": {},
	"todo_write": {}, "todo_add": {}, "todo_update": {}, "todo_list": {},
	"artifact_read": {},
}

// PlanToolAllowed 是 Plan mode 工具白名单的唯一真值。runtime Authorize
// （tools.permctx.go）与 orchestrator 的 filterPlanTools（跨包 tools.PlanToolAllowed
// re-export）共用它，杜绝两份白名单漂移。
func PlanToolAllowed(name string) bool {
	_, ok := planAllowedTools[name]
	return ok
}

// NormalizeMode maps a loose user string to a PermissionMode, returning "" (and
// ok=false) for an unrecognized value so callers can reject bad input uniformly.
func NormalizeMode(s string) (PermissionMode, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "default", "normal":
		return ModeDefault, true
	case "allow-edits", "allow_edits", "edits", "edit", "auto-edit", "autoedits":
		return ModeAllowEdits, true
	case "yolo", "danger", "all":
		return ModeYOLO, true
	case "auto", "ai", "risk":
		return ModeAuto, true
	case "plan", "read-only", "readonly", "ro":
		return ModePlan, true
	}
	return "", false
}

// CycleMode returns the mode after cur in cycleOrder (wrapping). Used by the
// Shift+Tab binding. An empty/unset mode (the zero value) is treated as
// ModeDefault so cycling works before the first status reply. ModePlan 永不
// 出现在 cycle 中：从 ModePlan 调用 CycleMode 直接回到 ModeDefault（让
// Shift+Tab 退出 plan，与 /plan-off 行为对齐）。
func CycleMode(cur PermissionMode) PermissionMode {
	if cur == ModePlan {
		return ModeDefault
	}
	if cur == "" {
		cur = ModeDefault
	}
	for i, m := range cycleOrder {
		if m == cur {
			return cycleOrder[(i+1)%len(cycleOrder)]
		}
	}
	return cycleOrder[0]
}

// editTools are the tool names ModeAllowEdits auto-approves without prompting:
// filesystem writes/edits and directory creation are reversible enough to skip
// the prompt; shell, network, and everything else still prompt.
var editTools = map[string]bool{
	"fs_write": true,
	"fs_edit":  true,
	"fs_mkdir": true,
}

// IsEditTool reports whether name is a file-edit/mkdir tool (the set
// ModeAllowEdits auto-approves).
func IsEditTool(name string) bool { return editTools[name] }

// ModeLabel is the short display string shown in the footer.
func ModeLabel(m PermissionMode) string {
	switch m {
	case ModeAllowEdits:
		return "edits"
	case ModeYOLO:
		return "YOLO"
	case ModeAuto:
		return "auto"
	case ModePlan:
		return "plan"
	default:
		return "default"
	}
}

// RiskPrompt builds the prompt sent to the model in ModeAuto to rate a tool
// call's risk. It is deliberately terse and asks for a bare integer so parsing
// is robust. Includes date + OS so the model can contextualize the risk (e.g.
// destructive commands on a production server vs a dev workstation).
// Kept here (model-free) so it can be unit-tested.
func RiskPrompt(tool, args string) string {
	date := time.Now().Format("2006-01-02")
	return fmt.Sprintf(`You are a security risk evaluator for an autonomous coding agent.
Date: %s
Rate the risk of executing the following tool call on a 1-10 scale:

  1-2  read-only / harmless      (ls, cat, find, grep, read, status)
  3-4  low risk, reversible      (mkdir, build, test, git status/add)
  5-6  moderate, file changes    (writes in safe dirs, npm install)
  7-8  hard to reverse           (deletes, force ops, network mutations)
  9-10 destructive / irreversible (rm -rf, force push, drop db, sudo)

Tool: %s
Arguments: %s

Reply with ONLY a single integer 1-10. No words, no explanation.`, date, tool, args)
}

// ParseRiskScore extracts the first 1-10 integer from the model's reply,
// clamping into range. "rm" → fail; "9" → 9; "Risk: 7." → 7; "eleven" → fail.
// On failure the caller should fall back to prompting the user (fail-safe).
func ParseRiskScore(reply string) (int, bool) {
	s := strings.TrimSpace(reply)
	// Scan for the first run of digits in the reply.
	start := -1
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			if start == -1 {
				start = i
			}
		} else if start != -1 {
			n, err := strconv.Atoi(s[start:i])
			if err == nil {
				return clampRisk(n), true
			}
			start = -1
		}
	}
	if start != -1 {
		if n, err := strconv.Atoi(s[start:]); err == nil {
			return clampRisk(n), true
		}
	}
	return 0, false
}

func clampRisk(n int) int {
	if n < 1 {
		return 1
	}
	if n > 10 {
		return 10
	}
	return n
}
