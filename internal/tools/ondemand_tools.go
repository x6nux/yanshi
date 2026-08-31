package tools

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/cloudwego/eino/schema"
)

// ToolDiscoveryTools 承载 W-F-11 的两个逃生门工具。检索式按需加载的风险是
// 「模型看不见的工具就等于不存在」，这两个工具是那条风险的对冲：
//
//   - tools_list 列出**全部**注册工具的名字与一句话描述——不管有没有被检索
//     选中。它是模型发现自己少看见了的唯一途径。
//   - tools_load 按名字把工具 spec 装进本轮视野（写 toolLoadState），下一
//     次模型调用起可见。检索 miss 的工具靠它回来。
//
// 两者都由按需加载打开时的组合根注册；未开启时不注册（模型看得到全量
// schema，逃生门没有存在意义）。
type ToolDiscoveryTools struct {
	List *GuardedTool
	Load *GuardedTool
}

// Tools 返回两个逃生门工具，供组合根按既有 Tools() 惯例展开注册。
func (d *ToolDiscoveryTools) Tools() []*GuardedTool {
	return []*GuardedTool{d.List, d.Load}
}

// NewToolDiscoveryTools 用**注册全集**的元数据构造逃生门。全集在组合根装配
// 完成后才有——所以这两个工具最后构造、最后追加，列表里包含它们自己以外的
// 一切（加上它们自己的两行，由 metas 调用方决定要不要带上）。
func NewToolDiscoveryTools(metas []ToolMeta) *ToolDiscoveryTools {
	sorted := make([]ToolMeta, len(metas), len(metas)+2)
	copy(sorted, metas)
	// 逃生门把自己也列进全集——tools_list 的产出是「全部注册工具」，独缺自己
	// 两条会让模型以为还有没列出的东西。
	sorted = append(sorted,
		ToolMeta{Name: ToolsListToolName, Desc: "list every registered tool with a one-line description"},
		ToolMeta{Name: ToolsLoadToolName, Desc: "load tool specs by exact name so they become visible"},
	)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	descOf := make(map[string]string, len(sorted))
	for _, m := range sorted {
		descOf[m.Name] = m.Desc
	}

	renderList := func(state *toolLoadState, filter string) string {
		var b strings.Builder
		for _, m := range sorted {
			if filter != "" && !strings.Contains(strings.ToLower(m.Name), strings.ToLower(filter)) &&
				!strings.Contains(strings.ToLower(m.Desc), strings.ToLower(filter)) {
				continue
			}
			b.WriteString("- " + m.Name)
			if state != nil && state.Has(m.Name) {
				b.WriteString(" [loaded]")
			}
			if m.Desc != "" {
				b.WriteString(" -- " + m.Desc)
			}
			b.WriteString("\n")
		}
		return b.String()
	}

	d := &ToolDiscoveryTools{}
	d.List = NewGuardedTool(
		ToolsListToolName, "Tool list",
		"List EVERY tool registered in this environment with a one-line description, including tools whose "+
			"spec is not currently in your view (on-demand loading keeps the full set out of the schema). "+
			"Call this whenever a capability you need seems missing, then load what you found with tools_load.",
		10*time.Second,
		params(map[string]*schema.ParameterInfo{
			"filter": {Type: schema.String, Desc: "Optional case-insensitive substring filter on name or description"},
		}),
		SyncStream(func(ctx context.Context, argsJSON string) (string, error) {
			var a struct {
				Filter string `json:"filter"`
			}
			if err := ParseArgs(argsJSON, &a); err != nil {
				return "", err
			}
			// 状态未绑定 = 按需加载没开 = 全量 schema 本来就在视野里；列表
			// 照给（它仍然是对的），loaded 标记省略。
			state, _ := ToolLoadStateFromContext(ctx)
			out := renderList(state, a.Filter)
			if out == "" {
				return errorResult(fmt.Sprintf("no tool matches filter %q; call without the filter for the full list", a.Filter)), nil
			}
			return out, nil
		}),
	)
	d.Load = NewGuardedTool(
		ToolsLoadToolName, "Load tools",
		"Load tool specs by exact name (from tools_list) so they become visible to you on your next call. "+
			"Use it when tools_list showed a tool you need that you cannot currently see. Repeat calls load more.",
		10*time.Second,
		params(map[string]*schema.ParameterInfo{
			"names": {Type: schema.Array, ElemInfo: &schema.ParameterInfo{Type: schema.String},
				Desc: "Exact registered tool names to load", Required: true},
		}),
		SyncStream(func(ctx context.Context, argsJSON string) (string, error) {
			var a struct {
				Names []string `json:"names"`
			}
			if err := ParseArgs(argsJSON, &a); err != nil {
				return "", err
			}
			if len(a.Names) == 0 {
				return errorResult("no names given; call tools_list first, then pass exact names here"), nil
			}
			state, ok := ToolLoadStateFromContext(ctx)
			if !ok {
				// 不是占位：按需加载关闭的部署里这个工具不会被注册；走到这
				// 里的调用只来自把它直接构造出来却没绑 turn 状态的调用方
				//（测试、内嵌器）。如实报告，不假装加载。
				return errorResult("tool spec loading is not active in this session"), nil
			}
			known := make(map[string]struct{}, len(sorted))
			for _, m := range sorted {
				known[m.Name] = struct{}{}
			}
			var unknown []string
			for _, n := range a.Names {
				if _, ok := known[strings.TrimSpace(n)]; !ok {
					unknown = append(unknown, n)
				}
			}
			if len(unknown) > 0 {
				return errorResult(fmt.Sprintf("unknown tool name(s): %s. Call tools_list for the exact registered names",
					strings.Join(unknown, ", "))), nil
			}
			newly := state.Load(a.Names)
			if len(newly) == 0 {
				return "all requested tools were already loaded: " + strings.Join(a.Names, ", "), nil
			}
			var b strings.Builder
			b.WriteString(fmt.Sprintf("Loaded %d tool spec(s), visible from your next call:\n", len(newly)))
			for _, n := range newly {
				b.WriteString("- " + n)
				if desc := descOf[n]; desc != "" {
					b.WriteString(" -- " + desc)
				}
				b.WriteString("\n")
			}
			return b.String(), nil
		}),
	)
	return d
}
