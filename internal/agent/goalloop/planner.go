package goalloop

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// FakePlanner returns a pre-scripted Plan, useful for tests and non-LLM demos.
type FakePlanner struct {
	Tests []AcceptanceTest
	Steps []string
	Err   error
}

// Plan returns the scripted plan (or Err if set).
func (p FakePlanner) Plan(_ context.Context, g Goal) (Plan, error) {
	if p.Err != nil {
		return Plan{}, p.Err
	}
	return Plan{
		Goal:  g.Text,
		Tests: p.Tests,
		Steps: p.Steps,
	}, nil
}

// LLMPlanner asks a chat model to produce a structured plan as JSON.
type LLMPlanner struct {
	Model model.BaseChatModel
	// Sink, when non-nil, accumulates the token usage of each Plan call so the
	// goal loop's budget check reflects real spend. Nil is tolerated (no-op).
	Sink *UsageSink
}

// planJSON is the JSON schema the model is asked to return.
type planJSON struct {
	Goal  string           `json:"goal"`
	Tests []AcceptanceTest `json:"tests"`
	Steps []string         `json:"steps"`
}

// Plan builds a prompt, calls the model, and parses the JSON response.
func (p LLMPlanner) Plan(ctx context.Context, g Goal) (Plan, error) {
	prompt := fmt.Sprintf(
		"You are a software planning assistant. Given the following feature goal, "+
			"produce a plan with acceptance tests and implementation steps.\n\n"+
			"Feature goal: %s\n\n"+
			"Respond with ONLY a JSON object (no prose) in this exact format:\n"+
			`{"goal":"<echoed goal>","tests":[{"name":"<test name>","command":"<shell command>"}],"steps":["<step 1>","<step 2>"]}`,
		g.Text,
	)

	msgs := []*schema.Message{schema.UserMessage(prompt)}
	resp, err := p.Model.Generate(ctx, msgs)
	if err != nil {
		return Plan{}, fmt.Errorf("llm planner generate: %w", err)
	}
	addUsage(p.Sink, resp) // G02: count this call's tokens in the shared sink

	content := resp.Content
	raw := extractJSON(content)

	var pj planJSON
	if err := json.Unmarshal([]byte(raw), &pj); err != nil {
		return Plan{}, fmt.Errorf("llm planner parse response: %w", err)
	}

	result := Plan{
		Goal:  pj.Goal,
		Tests: pj.Tests,
		Steps: pj.Steps,
	}

	if len(result.Steps) == 0 && len(result.Tests) == 0 {
		return Plan{}, fmt.Errorf("llm planner: response has no steps and no tests")
	}

	return result, nil
}

// extractJSON finds the first JSON object in s, stripping markdown code
// fences (```json ... ``` or ``` ... ```) if present.
func extractJSON(s string) string {
	s = strings.TrimSpace(s)

	// Strip code fences.
	if strings.HasPrefix(s, "```") {
		// Remove the opening fence line.
		if idx := strings.Index(s, "\n"); idx >= 0 {
			s = s[idx+1:]
		}
		s = strings.TrimSpace(s)
		// Remove the closing fence.
		if idx := strings.LastIndex(s, "```"); idx >= 0 {
			s = s[:idx]
		}
		s = strings.TrimSpace(s)
	}

	return s
}
