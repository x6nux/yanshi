package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/x6nux/yanshi/internal/agent/automation"
)

// AutomationTools 聚合 AU1 的八个 GuardedTool。八个工具的描述都向模型承诺
// "Approval required"，因此全部经 NewApprovalGuardedTool → AuthorizeApprovalRequired
// 构造：每次调用都必须由用户逐次批准，profile 的 Tools.Allow（哪怕 "*"）、
// approval.Manager 的历史规则、yolo/auto 模式都无法绕过。
//
// 后果（不是缺陷，是「人工批准」的必然结论）：AuthorizeApprovalRequired 在 context
// 里没有 permission callback 时直接 DenyErr，而只有 WebSocket 传输会注入 callback，
// 所以这八个工具在 SSE、v1、task-agent 三条非交互式路径上恒被拒绝，只在 WS/TUI 上可用。
// 若将来要让它们在非交互式传输上可用，正确做法是加 profile 级预授权策略，而不是把这里
// 改回 NewGuardedTool（那会让描述重新对用户撒谎）。
type AutomationTools struct {
	Create *GuardedTool
	List   *GuardedTool
	Read   *GuardedTool
	Update *GuardedTool
	Pause  *GuardedTool
	Resume *GuardedTool
	Delete *GuardedTool
	Run    *GuardedTool
}

// NewAutomationTools 构造八个工具。manager 必须非 nil。
func NewAutomationTools(manager *automation.Manager) *AutomationTools {
	jsonInput := params(map[string]*schema.ParameterInfo{
		"input": {
			Type: schema.String,
			Desc: "JSON object containing the operation arguments",
		},
	})
	set := &AutomationTools{}

	set.Create = NewApprovalGuardedTool(
		"automation_create", "Create automation",
		"Create a persistent scheduled automation. Approval required.",
		time.Minute, jsonInput,
		func(ctx context.Context, args string) <-chan ToolChunk {
			return runAutomationCreate(ctx, manager, args)
		},
	)
	set.List = NewApprovalGuardedTool(
		"automation_list", "List automations",
		"List persistent automations. Approval required even though this is read-only.",
		time.Minute, jsonInput,
		func(ctx context.Context, args string) <-chan ToolChunk {
			return runAutomationList(ctx, manager, args)
		},
	)
	set.Read = NewApprovalGuardedTool(
		"automation_read", "Read automation",
		"Read an automation and recent runs. Approval required.",
		time.Minute, jsonInput,
		func(ctx context.Context, args string) <-chan ToolChunk {
			return runAutomationRead(ctx, manager, args)
		},
	)
	set.Update = NewApprovalGuardedTool(
		"automation_update", "Update automation",
		"Update prompt, schedule, cwds, or active state. Approval required.",
		time.Minute, jsonInput,
		func(ctx context.Context, args string) <-chan ToolChunk {
			return runAutomationUpdate(ctx, manager, args)
		},
	)
	set.Pause = NewApprovalGuardedTool(
		"automation_pause", "Pause automation",
		"Pause future scheduling. Approval required.",
		time.Minute, jsonInput,
		func(ctx context.Context, args string) <-chan ToolChunk {
			return runAutomationIDOp(ctx, manager, args, manager.Pause)
		},
	)
	set.Resume = NewApprovalGuardedTool(
		"automation_resume", "Resume automation",
		"Resume scheduling from a newly computed next run. Approval required.",
		time.Minute, jsonInput,
		func(ctx context.Context, args string) <-chan ToolChunk {
			return runAutomationIDOp(ctx, manager, args, manager.Resume)
		},
	)
	set.Delete = NewApprovalGuardedTool(
		"automation_delete", "Delete automation",
		"Delete an automation and its run history. Approval required.",
		time.Minute, jsonInput,
		func(ctx context.Context, args string) <-chan ToolChunk {
			return runAutomationIDOp(ctx, manager, args, manager.Delete)
		},
	)
	set.Run = NewApprovalGuardedTool(
		"automation_run", "Run automation",
		"Queue one durable task for an automation now. Approval required.",
		time.Minute, jsonInput,
		func(ctx context.Context, args string) <-chan ToolChunk {
			return runAutomationRun(ctx, manager, args)
		},
	)
	return set
}

// Tools 返回八个 automation 工具，供 bootstrap 一次性注册。
//
// 存在的理由：组合根一律通过 Tools() 把一组工具追加进 allTools（见 FSTools/
// TaskTools 等）。逐字段 append 八行会在新增/删除工具时漏改一处而静默丢工具，
// 而这里的名单和结构体字段同处一个文件，改动可见。
func (a *AutomationTools) Tools() []*GuardedTool {
	var ts []*GuardedTool
	for _, t := range []*GuardedTool{
		a.Create, a.List, a.Read, a.Update, a.Pause, a.Resume, a.Delete, a.Run,
	} {
		if t != nil {
			ts = append(ts, t)
		}
	}
	return ts
}

// oneChunk 把同步 fn 包装成单 chunk StreamFunc。
func oneChunk(fn func() (string, error)) <-chan ToolChunk {
	out := make(chan ToolChunk, 1)
	go func() {
		defer close(out)
		value, err := fn()
		if err != nil {
			out <- ToolChunk{Result: err.Error()}
			return
		}
		out <- ToolChunk{Result: value}
	}()
	return out
}

// decodeInput 把工具 args（{"input":"<json>"}）解码到 target。
func decodeInput(args string, target any) error {
	var envelope struct {
		Input string `json:"input"`
	}
	if err := json.Unmarshal([]byte(args), &envelope); err != nil {
		return err
	}
	if envelope.Input == "" {
		return fmt.Errorf("input is required")
	}
	return json.Unmarshal([]byte(envelope.Input), target)
}

func runAutomationCreate(_ context.Context, manager *automation.Manager, args string) <-chan ToolChunk {
	return oneChunk(func() (string, error) {
		var input struct {
			Name         string   `json:"name"`
			Prompt       string   `json:"prompt"`
			ScheduleKind string   `json:"schedule_kind"`
			Schedule     string   `json:"schedule"`
			Cwds         []string `json:"cwds"`
			Paused       bool     `json:"paused"`
		}
		if err := decodeInput(args, &input); err != nil {
			return "", err
		}
		schedule, err := automation.ParseSchedule(input.ScheduleKind, input.Schedule)
		if err != nil {
			return "", err
		}
		item, err := manager.Create(automation.CreateInput{
			Name: input.Name, Prompt: input.Prompt,
			Schedule: schedule, Cwds: input.Cwds, Paused: input.Paused,
		})
		if err != nil {
			return "", err
		}
		encoded, err := json.Marshal(item)
		if err != nil {
			return "", err
		}
		return string(encoded), nil
	})
}

func runAutomationList(_ context.Context, manager *automation.Manager, _ string) <-chan ToolChunk {
	return oneChunk(func() (string, error) {
		items, err := manager.List()
		if err != nil {
			return "", err
		}
		encoded, err := json.Marshal(items)
		if err != nil {
			return "", err
		}
		return string(encoded), nil
	})
}

func runAutomationRead(_ context.Context, manager *automation.Manager, args string) <-chan ToolChunk {
	return oneChunk(func() (string, error) {
		var input struct {
			ID string `json:"id"`
		}
		if err := decodeInput(args, &input); err != nil {
			return "", err
		}
		item, runs, err := manager.Read(input.ID)
		if err != nil {
			return "", err
		}
		encoded, err := json.Marshal(struct {
			Automation automation.Automation `json:"automation"`
			Runs       []automation.Run      `json:"runs"`
		}{item, runs})
		if err != nil {
			return "", err
		}
		return string(encoded), nil
	})
}

func runAutomationUpdate(_ context.Context, manager *automation.Manager, args string) <-chan ToolChunk {
	return oneChunk(func() (string, error) {
		var input struct {
			ID           string    `json:"id"`
			Name         string    `json:"name"`
			Prompt       string    `json:"prompt"`
			ScheduleKind string    `json:"schedule_kind"`
			Schedule     string    `json:"schedule"`
			Cwds         *[]string `json:"cwds"`
			Active       *bool     `json:"active"`
		}
		if err := decodeInput(args, &input); err != nil {
			return "", err
		}
		var schedule *automation.Schedule
		if input.ScheduleKind != "" || input.Schedule != "" {
			parsed, err := automation.ParseSchedule(input.ScheduleKind, input.Schedule)
			if err != nil {
				return "", err
			}
			schedule = &parsed
		}
		var name, prompt *string
		if input.Name != "" {
			name = &input.Name
		}
		if input.Prompt != "" {
			prompt = &input.Prompt
		}
		item, err := manager.Update(automation.UpdateInput{
			ID: input.ID, Name: name, Prompt: prompt,
			Schedule: schedule, Cwds: input.Cwds, Active: input.Active,
		})
		if err != nil {
			return "", err
		}
		encoded, err := json.Marshal(item)
		if err != nil {
			return "", err
		}
		return string(encoded), nil
	})
}

// runAutomationIDOp 是 Pause/Resume/Delete 的公共形状：都是单字段 id。
func runAutomationIDOp(_ context.Context, manager *automation.Manager, args string, op func(string) error) <-chan ToolChunk {
	return oneChunk(func() (string, error) {
		var input struct {
			ID string `json:"id"`
		}
		if err := decodeInput(args, &input); err != nil {
			return "", err
		}
		if err := op(input.ID); err != nil {
			return "", err
		}
		return `{"ok":true}`, nil
	})
}

func runAutomationRun(ctx context.Context, manager *automation.Manager, args string) <-chan ToolChunk {
	return oneChunk(func() (string, error) {
		var input struct {
			ID string `json:"id"`
		}
		if err := decodeInput(args, &input); err != nil {
			return "", err
		}
		run, err := manager.RunNow(ctx, input.ID)
		if err != nil {
			return "", err
		}
		encoded, err := json.Marshal(run)
		if err != nil {
			return "", err
		}
		return string(encoded), nil
	})
}
