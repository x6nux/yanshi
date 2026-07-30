package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/eino-contrib/jsonschema"
	orderedmap "github.com/wk8/go-ordered-map/v2"
)

// params builds a ParamsOneOf from a map of ParameterInfo.
// Unlike schema.NewParamsOneOfByParams, this leaves Required as nil (omitted
// from JSON) when no parameters are required, avoiding any "required" issues
// in LLM tool schemas.
func parameterSchema(p *schema.ParameterInfo) *jsonschema.Schema {
	out := &jsonschema.Schema{Type: string(p.Type), Description: p.Desc}
	if len(p.Enum) > 0 {
		out.Enum = make([]any, len(p.Enum))
		for i, value := range p.Enum {
			out.Enum[i] = value
		}
	}
	if p.ElemInfo != nil {
		out.Items = parameterSchema(p.ElemInfo)
	}
	if len(p.SubParams) > 0 {
		out.Properties = orderedmap.New[string, *jsonschema.Schema]()
		keys := make([]string, 0, len(p.SubParams))
		for key := range p.SubParams {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			child := p.SubParams[key]
			out.Properties.Set(key, parameterSchema(child))
			if child.Required {
				out.Required = append(out.Required, key)
			}
		}
	}
	return out
}

func params(m map[string]*schema.ParameterInfo) *schema.ParamsOneOf {
	if len(m) == 0 {
		return nil
	}
	root := &jsonschema.Schema{Type: "object", Properties: orderedmap.New[string, *jsonschema.Schema]()}
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := m[key]
		root.Properties.Set(key, parameterSchema(value))
		if value.Required {
			root.Required = append(root.Required, key)
		}
	}
	return schema.NewParamsOneOfByJSONSchema(root)
}

// toJSON marshals v to a JSON string. On marshal failure it returns a
// properly-escaped JSON object with an "error" key (never hand-built).
func toJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		b, _ = json.Marshal(struct {
			Error string `json:"error"`
		}{err.Error()})
		return string(b)
	}
	return string(b)
}

// runTool is a test helper that invokes a GuardedTool's InvokableRun.
func runTool(ctx context.Context, t tool.InvokableTool, argsJSON string) (string, error) {
	return t.InvokableRun(ctx, argsJSON)
}

// hostOnly extracts the hostname (without port) from rawURL.
func hostOnly(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// humanBytes renders n as e.g. "280 KiB" / "1.2 MiB" / "900 B". Shared by the
// spillover preview and fs_read's oversize self-guard error message.
func humanBytes(n int) string {
	const k = 1024
	switch {
	case n >= k*k:
		return fmt.Sprintf("%.1f MiB", float64(n)/float64(k*k))
	case n >= k:
		return fmt.Sprintf("%.0f KiB", float64(n)/float64(k))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
