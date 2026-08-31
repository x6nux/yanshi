package orchestrator

import (
	"context"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
)

// dynamic.go —— W-F-23 运行时注入的动态工具。
//
// 链路：传输层（WS）校验并构造 tools.NewClientTool → 放进本连接的动态集合 →
// 每次 turn 经 TurnOpts.DynamicTools 带进来 → withTurnContext 绑定到 ctx 并把
// 名字并入 toolreg 注册集 → 下面的中间件在 BeforeAgent 把它们加进本次 run 的
// dispatch 集合（runCtx.Tools 是 eino 写明的可改入口，改它同时改变 dispatch
// 与模型可见 schema，一次到位）。
//
// 子代理逃逸门（与 OnDemand 同一条纪律，同一提交检查 runSubAgentTurn）：动态
// 工具是**连接**捐赠的，不是编排器的配置。withTurnContext 无条件绑定一个动态
// 集合值（空集合也绑），子代理 turn 走自己的 withTurnContext 时空集合会遮蔽
// 父 ctx 里继承来的那个——绑定即遮蔽，泄漏在结构上不可表达。子代理拿到的
// 始终是自己的（空）集合。
type dynamicToolsValue struct {
	tools []BaseTool
}

// WithDynamicTools 绑定本 turn 的动态工具集。无条件绑定（包括空集）是刻意的：
// 空集合要把父作用域继承来的值遮蔽掉，否则一个 client_ 工具会顺着 ctx 链漏进
// 子代理。见 dynamic.go 文件头。
func WithDynamicTools(ctx context.Context, t []BaseTool) context.Context {
	return context.WithValue(ctx, dynamicToolsKey{}, &dynamicToolsValue{tools: t})
}

type dynamicToolsKey struct{}

// dynamicToolsFromContext 读回本 turn 的动态工具；未绑定返回 nil（合法：测试
// 与非 WS 入口不绑）。
func dynamicToolsFromContext(ctx context.Context) []BaseTool {
	v, ok := ctx.Value(dynamicToolsKey{}).(*dynamicToolsValue)
	if !ok || v == nil {
		return nil
	}
	return v.tools
}

// dynamicToolMiddleware 把 ctx 里的动态工具加进本次 run 的 dispatch。
// 实例无状态（集合在 ctx 里），可以无条件装进 orchestratorMiddlewares——
// 无动态工具的 turn 它是直通。
type dynamicToolMiddleware struct {
	*adk.BaseChatModelAgentMiddleware
}

func newDynamicToolMiddleware() *dynamicToolMiddleware {
	return &dynamicToolMiddleware{BaseChatModelAgentMiddleware: &adk.BaseChatModelAgentMiddleware{}}
}

// BeforeAgent 是 eino 的 per-run 入口：返回的 Tools 即本次 run 的 dispatch 集
// 合（同时决定模型可见的 schema）。追加而非替换——内置工具一个不能少。
func (m *dynamicToolMiddleware) BeforeAgent(
	ctx context.Context,
	runCtx *adk.ChatModelAgentContext,
) (context.Context, *adk.ChatModelAgentContext, error) {
	if runCtx == nil {
		return ctx, runCtx, nil
	}
	if dyn := dynamicToolsFromContext(ctx); len(dyn) > 0 {
		extra := make([]tool.BaseTool, len(dyn))
		for i, t := range dyn {
			extra[i] = t
		}
		runCtx.Tools = append(runCtx.Tools, extra...)
	}
	return ctx, runCtx, nil
}

// dynamicToolNames 提取动态工具的注册名（toolreg 并集用）。
func dynamicToolNames(dyn []BaseTool) []string {
	names := make([]string, 0, len(dyn))
	for _, t := range dyn {
		if info, err := t.Info(context.Background()); err == nil && info != nil && info.Name != "" {
			names = append(names, info.Name)
		}
	}
	return names
}
