package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/eino-contrib/jsonschema"
)

// W-F-23: 动态工具 —— 客户端在运行时注入 function 规格，模型当轮即可调用；
// 调用经回调往返给注入它的客户端执行。
//
// 注入面是模型/客户端可控的，所以这里的每一步都按**不可信输入**对待：
//
//   - 名字必须匹配 client_ 前缀的白名单形态——前缀防冒充内置工具（fs_read
//     之类不可能被注入顶替），形态防注入怪名字污染 schema/审批作用域；
//   - 描述与参数 schema 尺寸封顶——schema 会进模型请求，无上限的描述就是
//     免费的上下文炸弹；
//   - schema 必须能解析成 JSON Schema 对象——解析失败在注入时拒绝，而不是
//     在第一次模型调用时炸掉整个 turn；
//   - 描述只是 schema 数据（随工具定义发给 provider），它不进任何系统提示词、
//     不被任何 Go 代码当指令解释；
//   - 执行过 guard：工具仍是 GuardedTool，每次调用照常走 Authorize——注入
//     本身不授权执行，profile/审批链不变；
//   - 运行期受 internal/toolreg 检查：注入的名字由组合根并入 per-turn 注册
//     集（orchestrator.WithDynamicTools 一侧），没注入的名字照样被结构性
//     拒绝且不弹窗。
type ClientToolSpec struct {
	// Name 是工具注册名，必须匹配 clientNameRe。
	Name string `json:"name"`
	// Description 是模型可见的一句话描述。
	Description string `json:"description"`
	// Parameters 是 JSON Schema（object 形态）的参数定义，可为空（无参工具）。
	Parameters json.RawMessage `json:"parameters"`
}

// clientNameRe 是注入工具名的唯一合法形态。强制 client_ 前缀：与内置工具的
// 名字空间物理隔离，一个与内置工具同名的注入请求在这里就被拒绝，冒充无从
// 谈起。
var clientNameRe = regexp.MustCompile(`^client_[a-z][a-z0-9_]{0,62}$`)

const (
	// maxClientToolDescBytes 封顶描述文本。schema 进模型请求；一条无上限的
	// 描述是一次免费的上下文注入。
	maxClientToolDescBytes = 4096
	// maxClientToolSchemaBytes 封顶参数 schema 的原始字节数。
	maxClientToolSchemaBytes = 32 << 10
	// ClientToolTimeout 是单次客户端往返的上限。客户端卡死不能把整个 turn
	// 挂死；超时以错误结果回喂模型。
	ClientToolTimeout = 120 * time.Second
)

// Validate 检查注入 spec 的形态合法性。全部通过后 spec 才允许变成注册工具。
func (s ClientToolSpec) Validate() error {
	if !clientNameRe.MatchString(s.Name) {
		return fmt.Errorf("client tool name %q must match client_<lowercase_identifier>", s.Name)
	}
	if len(s.Description) > maxClientToolDescBytes {
		return fmt.Errorf("client tool %s: description too long (%d > %d bytes)",
			s.Name, len(s.Description), maxClientToolDescBytes)
	}
	if len(s.Parameters) > maxClientToolSchemaBytes {
		return fmt.Errorf("client tool %s: parameters schema too large (%d > %d bytes)",
			s.Name, len(s.Parameters), maxClientToolSchemaBytes)
	}
	if len(s.Parameters) > 0 {
		var probe map[string]any
		if err := json.Unmarshal(s.Parameters, &probe); err != nil {
			return fmt.Errorf("client tool %s: parameters is not a JSON object: %w", s.Name, err)
		}
	}
	return nil
}

// paramsOneOf 把注入的原始 schema 解析成 eino 的 ParamsOneOf。空 schema 给
// 一个空 object（无参工具）。
func (s ClientToolSpec) paramsOneOf() (*schema.ParamsOneOf, error) {
	if len(s.Parameters) == 0 || len(strings.TrimSpace(string(s.Parameters))) == 0 {
		return schema.NewParamsOneOfByJSONSchema(&jsonschema.Schema{Type: "object"}), nil
	}
	var parsed jsonschema.Schema
	if err := json.Unmarshal(s.Parameters, &parsed); err != nil {
		return nil, fmt.Errorf("client tool %s: parameters is not a valid JSON Schema: %w", s.Name, err)
	}
	if parsed.Type == "" {
		parsed.Type = "object"
	}
	return schema.NewParamsOneOfByJSONSchema(&parsed), nil
}

// ClientInvoke 是传输层绑定的回调：把一次工具调用往返给注入它的客户端执行。
// 返回 (结果文本, nil) 或 ("" , err)；err 作为工具错误结果回喂模型。
type ClientInvoke func(ctx context.Context, argsJSON string) (string, error)

// NewClientTool 把注入 spec 变成注册工具。spec 先 Validate；执行体走绑定的
// invoke 回调。返回的仍是 GuardedTool：授权、审计、redaction、errcnt 熔断
// 与内置工具走完全相同的管线——注入不产生第二条执行管线。
func NewClientTool(spec ClientToolSpec, invoke ClientInvoke) (*GuardedTool, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if invoke == nil {
		return nil, fmt.Errorf("client tool %s: no invoke callback bound", spec.Name)
	}
	params, err := spec.paramsOneOf()
	if err != nil {
		return nil, err
	}
	// TUI 块标题用固定文案：注入工具的语义在描述里，名字在 tool_call 帧里。
	return NewGuardedTool(
		spec.Name, "Client tool",
		spec.Description,
		ClientToolTimeout,
		params,
		SyncStream(func(ctx context.Context, argsJSON string) (string, error) {
			return invoke(ctx, argsJSON)
		}),
	), nil
}
