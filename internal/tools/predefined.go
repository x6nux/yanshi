// Package tools holds yanshi's Eino tool implementations, guard-wrapped.
package tools

import (
	"strings"
)

// PredefinedAgentDef defines a reusable agent template.
// The tool layer uses these definitions to build prompts and DAG workflows
// on the fly, so LLM calls consistently follow the same structure without
// the user having to re-specify granular steps.
type PredefinedAgentDef struct {
	// Name is the unique identifier, e.g. "analysis".
	Name string `json:"name"`

	// Description explains what this agent does.
	Description string `json:"description"`

	// PromptTmpl is the default prompt template.
	// The placeholder {{target}} is replaced with the actual target path.
	// Extra placeholders like {{detail}} may be used by specific tools.
	PromptTmpl string `json:"prompt_tmpl"`

	// Workflow is an optional DAG workflow definition for multi-step mode.
	// When non-nil, the tool may choose to run the workflow instead of a
	// single sub-agent to get deeper, more structured results.
	Workflow *WorkflowDef `json:"workflow,omitempty"`
}

// PredefinedAgents is the built-in registry of reusable agent templates.
// Tools such as "analysis" reference these by name.
var PredefinedAgents = map[string]PredefinedAgentDef{

	// -----------------------------------------------------------------------
	// Analysis – comprehensive code / project analysis
	// -----------------------------------------------------------------------
	"analysis": {
		Name:        "analysis",
		Description: "快速分析代码项目结构、架构模式、依赖关系和潜在问题",
		PromptTmpl: `你是一个资深的代码分析专家。请全面分析目标代码：

目标路径: {{target}}

请从以下几个方面进行深入分析：

## 1. 项目结构
- 目录组织方式和层次结构
- 关键文件及其职责
- 构建/配置体系

## 2. 架构模式
- 使用的架构风格（MVC、微服务、事件驱动等）
- 设计模式应用
- 模块划分与职责边界

## 3. 依赖关系
- 内部模块之间的依赖关系
- 外部框架/库的使用
- 循环依赖或过度耦合问题

## 4. 代码质量
- 代码风格一致性
- 潜在的bug或安全隐患
- 可维护性评估
- 测试覆盖情况

## 5. 改进建议
- 架构优化方向
- 可改进的技术债务
- 性能/安全增强点

请提供**结构化、详实、可操作**的分析报告。`,

		Workflow: &WorkflowDef{
			Steps: []WorkflowStepDef{
				{
					ID:     "A1",
					Prompt: "扫描项目结构：分析 {{target}} 的目录结构、关键文件列表、构建配置和整体组织方式。列出所有重要的目录和文件，并简要说明其用途。",
					Deps:   nil,
				},
				{
					ID:     "B1",
					Prompt: "基于 {{A1.output}} 分析架构模式：分析 {{target}} 使用的架构风格、设计模式、模块划分和职责边界。评估架构的合理性和扩展性。",
					Deps:   []string{"A1"},
				},
				{
					ID:     "B2",
					Prompt: "基于 {{A1.output}} 分析依赖关系：分析 {{target}} 的内部模块依赖、外部库依赖、可能的循环依赖或过度耦合问题。生成依赖图描述。",
					Deps:   []string{"A1"},
				},
				{
					ID:     "B3",
					Prompt: "基于 {{A1.output}} 分析代码质量：评估 {{target}} 的代码风格、潜在bug、安全隐患、可维护性和测试覆盖情况。列出具体问题位置和改进建议。",
					Deps:   []string{"A1"},
				},
				{
					ID:     "C1",
					Prompt: "综合以下分析结果，生成一份完整的分析报告：\n\n## 项目结构\n{{A1.output}}\n\n## 架构模式\n{{B1.output}}\n\n## 依赖关系\n{{B2.output}}\n\n## 代码质量\n{{B3.output}}\n\n请整合上述分析，去除重复内容，补充缺失的关联分析，形成一个结构清晰、重点突出的最终报告。",
					Deps:   []string{"A1", "B1", "B2", "B3"},
				},
			},
		},
	},

	// -----------------------------------------------------------------------
	// Summarize – read a file and condense it into a structured summary
	// -----------------------------------------------------------------------
	"summarize": {
		Name:        "summarize",
		Description: "读取文件并产出结构化总结（支持大文件分页读取）",
		PromptTmpl: `你是内容总结专家。请阅读目标文件并产出结构化总结。

目标文件: {{target}}
{{focus_line}}
要求:
- 用 fs_read 分页读取（offset/end），不要假设一次能读完。
- 产出: ① 核心要点 ② 结构/章节 ③ 关键片段（必要时引用行号）。
- 总结不超过 {{max_lines}} 行。
- 若是日志/输出，提取异常、错误、关键时间线。
- 若是代码，概述职责、公开符号、依赖关系。`,
	},

	// -----------------------------------------------------------------------
	// Review – chunked code review via sub-agent
	// -----------------------------------------------------------------------
	"review": {
		Name:        "review",
		Description: "分块代码评审：每个 48 KiB chunk 由子代理产出结构化 findings，再合并去重",
		PromptTmpl: `You are reviewing chunk {{INDEX}} of {{TOTAL}} of a pull request on {{REPO}}#{{NUMBER}}.

Return STRICT JSON of the form:
{"findings":[{"file":"path","line":N,"severity":"high|medium|low|info","message":"...","rule":"..."}]}

If you find no issues, return {"findings":[]}.

Diff chunk:
{{CHUNK}}
`,
	},

	// -----------------------------------------------------------------------
	// Placeholder for future predefined agents:
	//   "refactor"   – suggest and apply refactoring
	//   "review"     – code review (PR-style)
	//   "docgen"     – generate documentation
	//   "testgen"    – generate test cases
	//   "bugfinder"  – deep bug / vulnerability analysis
	// -----------------------------------------------------------------------
}

// GetPredefinedAgent returns a copy of the named predefined agent definition.
// The second return value reports whether the agent was found.
func GetPredefinedAgent(name string) (PredefinedAgentDef, bool) {
	a, ok := PredefinedAgents[name]
	return a, ok
}

// ListPredefinedAgents returns all registered agent names.
func ListPredefinedAgents() []string {
	names := make([]string, 0, len(PredefinedAgents))
	for n := range PredefinedAgents {
		names = append(names, n)
	}
	return names
}

// FillPrompt replaces placeholders in the template with values from vars.
// Supported placeholders: {{target}}, {{name}}, and any key in vars.
func FillPrompt(tmpl string, vars map[string]string) string {
	s := tmpl
	for k, v := range vars {
		s = strings.ReplaceAll(s, "{{"+k+"}}", v)
	}
	return s
}
