package orchestrator

import (
	"context"
	"strings"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"

	"github.com/x6nux/yanshi/internal/tools"
)

// toolspec.go —— W-F-11 按需加载工具 spec 的过滤中间件。
//
// 落点在 BeforeModelRewriteState 的 state.ToolInfos（eino 文档写明的「发给模型
// 的工具清单、可改、是模型调用的真相源」），而不是 dispatch 集合
// （ToolsConfig.Tools）：dispatch 保持注册全集，授权路径（Authorize）、未知
// 工具兜底（UnknownToolsHandler 的全集名单）、plan 模式的静态过滤全部照旧。
// 改 dispatch 集合就得重建 runner、打破按 model memoise 的缓存键；改
// ToolInfos 只影响「模型看见什么」。
//
// 与 loopguard 中间件同一条纪律：本实例被同 model 的所有会话共享，**不得持有
// 任何按 turn 的可变状态**——每轮加载集合（toolLoadState）绑在 turn ctx 上，
// 由 tools_load 工具写、本中间件读。
type toolSpecGate struct {
	*adk.BaseChatModelAgentMiddleware
	retriever  *tools.ToolRetriever
	always     map[string]struct{}
	maxVisible int
	// full 是本 runner 的完整 ToolInfo 清单，构造时从 dispatch 子集取一次。
	// 每轮过滤从它重建，而不是在 state.ToolInfos 的（已被自己过滤过的）持久
	// 化副本上继续挑——原因见 BeforeModelRewriteState 内的注释。
	full []*schema.ToolInfo
}

// newToolSpecGate 用本 runner 的 dispatch 子集建语料（plan 模式的 runner 拿到
// 的是 plan 过滤后的子集，检索只会在这个子集内收窄，跨不过 plan 的静态白名
// 单）。escapeHatch（tools_list / tools_load）并入 always——「逃生门自己被
// 藏掉」等于机制锁死。
func newToolSpecGate(registered []tool.BaseTool, cfg tools.ToolLoadConfig) *toolSpecGate {
	metas := make([]tools.ToolMeta, 0, len(registered))
	full := make([]*schema.ToolInfo, 0, len(registered))
	for _, t := range registered {
		if info, err := t.Info(context.Background()); err == nil && info != nil && info.Name != "" {
			metas = append(metas, tools.ToolMeta{Name: info.Name, Desc: info.Desc})
			full = append(full, info)
		}
	}
	maxVisible := cfg.MaxVisible
	if maxVisible <= 0 {
		maxVisible = tools.DefaultMaxVisibleTools
	}
	always := make(map[string]struct{}, len(cfg.Always)+2)
	for _, n := range cfg.Always {
		always[n] = struct{}{}
	}
	always[tools.ToolsListToolName] = struct{}{}
	always[tools.ToolsLoadToolName] = struct{}{}
	return &toolSpecGate{
		BaseChatModelAgentMiddleware: &adk.BaseChatModelAgentMiddleware{},
		retriever:                    tools.NewToolRetriever(metas),
		always:                       always,
		maxVisible:                   maxVisible,
		full:                         full,
	}
}

// BeforeModelRewriteState 在每次模型调用前把 state.ToolInfos 收窄到
// always ∪ 本轮已加载 ∪ 检索 Top-K。三次全绿的退化路径：
//
//   - 检索器没选出来（空查询/全零分）：视野 = always + 已加载，这是诚实的
//     退化——乱选比不选危险；
//   - 状态未绑定（不该发生，withTurnContext 无条件绑定）：当作「没有显式
//     加载」，仍能工作；
//   - ToolInfos 已空：原样返回，没东西可过滤。
//
// 过滤只收窄从不放大：plan 模式的 ToolInfos 已是静态白名单子集，检索与
// 加载只会更小。
func (m *toolSpecGate) BeforeModelRewriteState(
	ctx context.Context,
	state *adk.ChatModelAgentState,
	_ *adk.ModelContext,
) (context.Context, *adk.ChatModelAgentState, error) {
	if state == nil || len(m.full) == 0 {
		return ctx, state, nil
	}
	visible := make(map[string]struct{}, len(m.always)+m.maxVisible)
	for n := range m.always {
		visible[n] = struct{}{}
	}
	if loaded, ok := tools.ToolLoadStateFromContext(ctx); ok {
		for _, n := range loaded.Names() {
			visible[n] = struct{}{}
		}
	}
	for _, n := range m.retriever.Select(lastUserMessageForSpecGate(state.Messages), m.maxVisible) {
		visible[n] = struct{}{}
	}
	// 从 gate 自己持有的全量清单重建，而不是从 state.ToolInfos 里挑——ADK
	// 会把本 hook 返回的 state 持久化为下一轮的起点，在持久化副本上再过滤
	// 是单调收缩：这一轮被藏掉的工具，之后永远回不来（tools_load 装了也没
	// 有用，实测抓到的正是这个）。每轮从全量重算，可见性才是「当前集合」的
	// 函数而不是历史过滤的函数。
	out := make([]*schema.ToolInfo, 0, len(visible))
	for _, ti := range m.full {
		if ti == nil {
			continue
		}
		if _, keep := visible[ti.Name]; keep {
			out = append(out, ti)
		}
	}
	state.ToolInfos = out
	return ctx, state, nil
}

// lastUserMessageForSpecGate 取最近一条 user 消息的文本作为检索查询。压缩
// 摘要也是 user 形态的消息——它概括了当轮意图，作查询正是想要的。
func lastUserMessageForSpecGate(msgs []*schema.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if m := msgs[i]; m != nil && m.Role == schema.User {
			return m.Content
		}
	}
	return strings.TrimSpace("")
}
