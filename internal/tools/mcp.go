package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/x6nux/yanshi/internal/mcp"
)

// NewMCPTools 从 MCP Manager 发现的工具构造 GuardedTool 列表。
// 每个工具名按 mcp_<server>_<tool> 注册，经过 Tools.Allow + MCP.Allow 双重门禁。
func NewMCPTools(mgr *mcp.Manager) []*GuardedTool {
	if mgr == nil || !mgr.Enabled() {
		return nil
	}
	descs, err := mgr.ListAllTools(context.Background())
	if err != nil {
		return nil
	}
	out := make([]*GuardedTool, 0, len(descs))
	for _, td := range descs {
		td := td
		desc := td.Description
		if desc == "" {
			desc = fmt.Sprintf("MCP tool from %q", td.ServerName)
		}
		p := buildMCPParams(td.InputSchema)
		if p == nil {
			p = params(map[string]*schema.ParameterInfo{
				"arguments": {Type: schema.String, Desc: "tool arguments as JSON"},
			})
		}
		out = append(out, NewGuardedTool(td.Qualified, "MCP:"+td.ServerName, desc, 5*time.Minute, p, SyncStream(func(ctx context.Context, args string) (string, error) {
			bound, ok := MCPFromContext(ctx)
			if !ok {
				return errorResult("MCP manager not bound"), nil
			}
			// CallToolRetry, not CallTool: it reconnects once on failure. The
			// retry wrapper existed with zero callers, so a server that died
			// between turns turned every tool call into a hard error the
			// model could only report, never recover from.
			res, err := bound.CallToolRetry(ctx, td.Qualified, []byte(args))
			if err != nil {
				return errorResult(err.Error()), nil
			}
			return string(res), nil
		})))
	}
	return out
}

func buildMCPParams(raw string) *schema.ParamsOneOf {
	var doc map[string]any
	if json.Unmarshal([]byte(raw), &doc) != nil {
		return nil
	}
	props, _ := doc["properties"].(map[string]any)
	infos := make(map[string]*schema.ParameterInfo, len(props))
	for name, pv := range props {
		p, _ := pv.(map[string]any)
		typ, _ := p["type"].(string)
		desc, _ := p["description"].(string)
		infos[name] = &schema.ParameterInfo{Type: schema.DataType(typ), Desc: desc}
	}
	return params(infos)
}
