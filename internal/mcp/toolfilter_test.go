package mcp

import (
	"context"
	"testing"
)

// AdmitsTool 的语义矩阵：deny 压过 allow；空 allow 全放（配置缺省形状，
// 老配置一个不差）；非空 allow 只放命中；glob 可用；坏 pattern 不命中
// （allow 侧 fail-closed）。
//
// 变异：把 AdmitsTool 的 deny 循环挪到 allow 判定之后（allow 命中即返回
// true）→ deny-wins 用例变红；把空 allow 分支改成返回 false → 缺省形状
// 用例变红（等于把老配置全部静默清零，这正是验收里「语义不变」要拦的）。
func TestServerConfigAdmitsTool(t *testing.T) {
	cases := []struct {
		name        string
		allow, deny []string
		tool        string
		want        bool
	}{
		{"empty allow admits all", nil, nil, "anything", true},
		{"empty allow admits all under deny misses", nil, []string{"other"}, "anything", true},
		{"non-empty allow filters", []string{"alpha", "beta"}, nil, "gamma", false},
		{"non-empty allow admits hit", []string{"alpha"}, nil, "alpha", true},
		{"deny wins over allow", []string{"*"}, []string{"deploy_prod"}, "deploy_prod", false},
		{"deny miss under broad allow", []string{"*"}, []string{"other"}, "deploy_staging", true},
		{"glob allow", []string{"search_*"}, nil, "search_web", true},
		{"glob deny", nil, []string{"admin_*"}, "admin_reset", false},
		{"malformed allow pattern matches nothing", []string{"["}, nil, "alpha", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &ServerConfig{Name: "srv", ToolAllow: tc.allow, ToolDeny: tc.deny}
			if got := cfg.AdmitsTool(tc.tool); got != tc.want {
				t.Fatalf("AdmitsTool(%q) with allow=%v deny=%v = %v, want %v",
					tc.tool, tc.allow, tc.deny, got, tc.want)
			}
		})
	}
}

// startOne 在注册前过滤：deny/allow 之外的 advertised 工具根本进不了
// toolMap，对模型不可见。鉴权语义仍在 guard 的 mcp 维度（空 profile allow
// 照旧 fail-closed，由 guard 包自己的测试钉住）—— 本条只钉注册面收窄。
//
// 变异：删掉 startOne 里 AdmitsTool 的 continue → 本测试变红（三个工具
// 全部注册）。
func TestManagerStartOneAppliesToolFilter(t *testing.T) {
	ts, _ := NewFakeHTTPServer([]ToolDescriptor{
		{ToolName: "alpha"}, {ToolName: "beta"}, {ToolName: "gamma"},
	})
	defer ts.Close()
	m := NewManager(map[string]*ServerConfig{
		"srv": {
			Name: "srv", Enabled: true, Transport: TransportHTTP, URL: ts.URL,
			ToolAllow: []string{"alpha", "beta"},
			ToolDeny:  []string{"beta"},
		},
	})
	if st := m.StartAll(context.Background()); len(st) != 1 || st[0].Status != StatusReady {
		t.Fatalf("status=%+v", st)
	}
	tools, err := m.ListAllTools(context.Background())
	if err != nil {
		t.Fatalf("ListAllTools: %v", err)
	}
	if len(tools) != 1 || tools[0].Qualified != "mcp_srv_alpha" {
		t.Fatalf("filtered tools = %+v, want only mcp_srv_alpha", tools)
	}
}
