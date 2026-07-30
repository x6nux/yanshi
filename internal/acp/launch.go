package acp

import (
	"fmt"
	"sort"
)

// launchDescriptors maps agent names to their argv for spawning an ACP subprocess.
var launchDescriptors = map[string][]string{
	"opencode":    {"opencode", "acp"},
	"claudecode":  {"npx", "@agentclientprotocol/claude-agent-acp"},
	"codex":       {"npx", "@agentclientprotocol/codex-acp"},
}

// LaunchSpec resolves an agent name to the argv used to spawn its ACP subprocess.
// It returns an error for unknown agent names.
func LaunchSpec(name string) ([]string, error) {
	argv, ok := launchDescriptors[name]
	if !ok {
		return nil, fmt.Errorf("acp: unknown agent %q", name)
	}
	return argv, nil
}

// AgentNames returns the sorted list of known external agent names (the keys
// of launchDescriptors). Callers that need to enumerate every launchable agent
// — for example `yanshi doctor` probing which agent CLIs are on PATH — use
// this instead of hardcoding the list, so the set of agents lives in exactly
// one place (launchDescriptors) and stays in sync as new agents are added.
func AgentNames() []string {
	names := make([]string, 0, len(launchDescriptors))
	for name := range launchDescriptors {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
