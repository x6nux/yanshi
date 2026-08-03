package acp

import (
	"reflect"
	"sort"
	"testing"
)

// TestLaunchSpec verifies that LaunchSpec returns the correct argv for each
// known agent name and an error for unknown names.
func TestLaunchSpec(t *testing.T) {
	tests := []struct {
		name     string
		wantArgv []string
		wantErr  bool
	}{
		{"opencode", []string{"opencode", "acp"}, false},
		{"claudecode", []string{"npx", "@agentclientprotocol/claude-agent-acp"}, false},
		{"codex", []string{"npx", "@agentclientprotocol/codex-acp"}, false},
		{"unknown", nil, true},
		{"", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			argv, err := LaunchSpec(tt.name)
			if (err != nil) != tt.wantErr {
				t.Fatalf("LaunchSpec(%q) error = %v, wantErr %v", tt.name, err, tt.wantErr)
			}
			if !tt.wantErr && !reflect.DeepEqual(argv, tt.wantArgv) {
				t.Errorf("LaunchSpec(%q) argv = %v, want %v", tt.name, argv, tt.wantArgv)
			}
		})
	}
}

func TestAgentNames(t *testing.T) {
	names := AgentNames()
	for _, want := range []string{"claudecode", "codex", "opencode"} {
		found := false
		for _, n := range names {
			if n == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("AgentNames() missing %q: got %v", want, names)
		}
	}
	if !sort.StringsAreSorted(names) {
		t.Errorf("AgentNames() must return sorted names: got %v", names)
	}
}
