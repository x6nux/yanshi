package tools

import (
	"testing"
)

func TestAgentToolsReturnsAll(t *testing.T) {
	m, _ := newTestManager(t)
	tools := NewAgentTools(nil)
	gt := tools.Tools()
	if len(gt) != 12 {
		t.Fatalf("AgentTools.Tools() should return 12 tools, got %d", len(gt))
	}
	_ = m // manager not needed for this test
}

func TestFSToolsReturnsAll(t *testing.T) {
	ft := NewFSTools(".")
	gt := ft.Tools()
	if len(gt) != 8 {
		t.Fatalf("FSTools.Tools() should return 8 tools, got %d", len(gt))
	}
}

func TestPlanToolsReturnsAll(t *testing.T) {
	pt := NewPlanTools()
	gt := pt.Tools()
	if len(gt) != 9 {
		t.Fatalf("PlanTools.Tools() should return 9 tools, got %d", len(gt))
	}
}

func TestTaskToolsReturnsAll(t *testing.T) {
	tt := NewTaskTools()
	gt := tt.Tools()
	if len(gt) != 4 {
		t.Fatalf("TaskTools.Tools() should return 4 tools, got %d", len(gt))
	}
}

func TestMemoryToolsReturnsAll(t *testing.T) {
	// MemoryTools needs a store, but we can test the Tools() method.
	mt := &MemoryTools{}
	gt := mt.Tools()
	if len(gt) != 4 {
		t.Fatalf("MemoryTools.Tools() should return 4 tools, got %d", len(gt))
	}
}

func TestGateToolsReturnsAll(t *testing.T) {
	gt := NewGateTools()
	tools := gt.Tools()
	if len(tools) != 1 {
		t.Fatalf("GateTools.Tools() should return 1 tool, got %d", len(tools))
	}
}

func TestArtifactToolsReturnsAll(t *testing.T) {
	at := NewArtifactTools()
	tools := at.Tools()
	if len(tools) != 1 {
		t.Fatalf("ArtifactTools.Tools() should return 1 tool, got %d", len(tools))
	}
}
