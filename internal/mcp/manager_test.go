package mcp

import (
	"context"
	"testing"
)

func TestManager_StartDisableEnableRoute(t *testing.T) {
	ts, fake := NewFakeHTTPServer([]ToolDescriptor{{ToolName: "hello"}})
	defer ts.Close()
	m := NewManager(map[string]*ServerConfig{"srv": {Name: "srv", Enabled: true, Transport: TransportHTTP, URL: ts.URL}})
	st := m.StartAll(context.Background())
	if len(st) != 1 || st[0].Status != StatusReady {
		t.Fatalf("status=%+v", st)
	}
	tools, _ := m.ListAllTools(context.Background())
	if len(tools) != 1 || tools[0].Qualified != "mcp_srv_hello" {
		t.Fatalf("tools=%+v", tools)
	}
	if _, err := m.CallTool(context.Background(), "mcp_srv_hello", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if fake.CallCount != 1 {
		t.Fatalf("calls=%d", fake.CallCount)
	}
	_ = m.Disable(context.Background(), "srv")
	tools, _ = m.ListAllTools(context.Background())
	if len(tools) != 0 {
		t.Fatalf("disable tools=%+v", tools)
	}
	if err := m.Enable(context.Background(), "srv"); err != nil {
		t.Fatal(err)
	}
}
