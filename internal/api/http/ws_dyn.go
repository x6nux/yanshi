package http

// ws_dyn.go —— W-F-23 动态工具的 WebSocket 传输半边。
//
// 注入（tool_inject，主循环处理）：客户端捐赠一份 function 规格；校验与构造
// 在 tools.NewClientTool（不可信输入处理都在那边），成功后挂到本连接的动态
// 集合，从**下一条消息起的 turn** 生效（TurnOpts 在 turn 开始时快照——turn
// 进行中注入不改变正在运行的 turn。这是一条边界而非缺陷：turn 的工具面在
// turn 开始时定型，与 plan 模式的「每 turn 读一次」同一哲学）。
//
// 执行（tool_invoke / tool_result，跨 goroutine 往返）：模型调用注入工具时，
// 工具的执行体在 turn 的工具 goroutine 里发起 tool_invoke 帧，然后把自己阻塞
// 在 per-call 的 channel 上；tool_result 由**读 goroutine**送达（主循环被
// turn 阻塞着，与 permission_response 的走法完全一致）。
//
// SSE 没有客户端→服务端的帧通道，注入从设计上就不支持；服务端也绝不会向
// SSE 连接发出 tool_invoke（动态集合是 connSession 的，SSE turn 不带它）。

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/tools"
)

// dynResult 是一次客户端工具执行的结果。
type dynResult struct {
	text string
	err  error
}

// clientToolError 标记客户端侧执行失败：文本原样回喂模型（错误结果，不是 Go
// error——模型可以读到原因并重试或换路）。
type clientToolError struct{ text string }

func (e *clientToolError) Error() string { return e.text }

// dynPendingID 是 tool_invoke 的连接内自增关联 id。
var dynPendingID atomic.Uint64

// injectDynamicTool 把一个已构造好的动态工具挂到连接上（并发安全）。
func (cs *connSession) injectDynamicTool(t orchestrator.BaseTool) {
	cs.dynMu.Lock()
	defer cs.dynMu.Unlock()
	cs.dynTools = append(cs.dynTools, t)
}

// dynamicSnapshot 返回当前动态工具集的拷贝——turn 开始时快照一次，turn 进行
// 中的新注入不影响正在运行的 turn。
func (cs *connSession) dynamicSnapshot() []orchestrator.BaseTool {
	cs.dynMu.Lock()
	defer cs.dynMu.Unlock()
	out := make([]orchestrator.BaseTool, len(cs.dynTools))
	copy(out, cs.dynTools)
	return out
}

// registerDynInvoke 登记一个等待 tool_result 的 channel。
func (cs *connSession) registerDynInvoke(id string) chan dynResult {
	ch := make(chan dynResult, 1)
	cs.dynMu.Lock()
	defer cs.dynMu.Unlock()
	if cs.dynPending == nil {
		cs.dynPending = make(map[string]chan dynResult)
	}
	cs.dynPending[id] = ch
	return ch
}

// deliverDynResult 把 tool_result 送达等待者；无等待者（超时/取消后迟到）时
// 是 no-op。channel 缓冲为 1，送达永不阻塞读 goroutine。
func (cs *connSession) deliverDynResult(id string, r dynResult) {
	cs.dynMu.Lock()
	ch, ok := cs.dynPending[id]
	delete(cs.dynPending, id)
	cs.dynMu.Unlock()
	if !ok {
		return
	}
	select {
	case ch <- r:
	default:
	}
}

// handleToolInject 处理一条 tool_inject 帧：校验 spec、构造 GuardedTool、挂到
// 连接。失败写 error 帧（逐条拒绝原因）；成功写 tool_progress 帧确认。
func handleToolInject(conn *wsConn, cs *connSession, cf proto.ClientFrame) {
	spec := tools.ClientToolSpec{
		Name:        cf.Name,
		Description: cf.Text,
		Parameters:  cf.ToolSchema,
	}
	tool, err := tools.NewClientTool(spec, clientInvokeFor(conn, cs, spec.Name))
	if err != nil {
		conn.write(proto.NewError(fmt.Sprintf("tool_inject rejected: %v", err)))
		return
	}
	cs.injectDynamicTool(tool)
	conn.write(proto.NewToolProgress(spec.Name,
		"injected; callable from your next message (subject to the permission profile)"))
}

// clientInvokeFor 绑定注入工具的执行回调：发 tool_invoke、阻塞等 tool_result。
// 超时与取消由 ctx 负责（GuardedTool 已把 ClientToolTimeout 套在 runCtx 上）。
func clientInvokeFor(conn *wsConn, cs *connSession, name string) tools.ClientInvoke {
	return func(ctx context.Context, argsJSON string) (string, error) {
		id := fmt.Sprintf("dyn-%d", dynPendingID.Add(1))
		ch := cs.registerDynInvoke(id)
		// conn.write 本身不回传错误：客户端断开会 cancel 连接 ctx，turn 随之
		// 取消，这里的 ctx.Done 会兜底——不存在「发了帧但永远没人回」的悬挂。
		conn.write(proto.NewToolInvoke(id, name, argsJSON))
		select {
		case r := <-ch:
			return r.text, r.err
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}
