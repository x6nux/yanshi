package goalloop

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"runtime"
	"strings"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/x6nux/yanshi/internal/guard"
)

// ---------------------------------------------------------------------------
// TestEvaluator — runs each AcceptanceTest command and collects results.
// ---------------------------------------------------------------------------

// TestEvaluator runs each plan's AcceptanceTest commands in the workdir
// and reports pass/fail based on exit codes.
type TestEvaluator struct {
	// Profile is the permission profile used to gate shell commands.
	// When zero-valued (Tools.Allow empty), a default allowlist is used.
	Profile guard.PermissionProfile
}

// defaultTestProfile returns a conservative shell-allowlist profile used
// when TestEvaluator.Profile is not set.
func defaultTestProfile() guard.PermissionProfile {
	return guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
		Shell: guard.ShellPerm{
			Policy:   "allowlist",
			Patterns: []string{"go test*", "go build*", "go vet*", "go fmt*", "echo *"},
		},
	}
}

// WithProfile sets the permission profile used to gate test commands.
func (e *TestEvaluator) WithProfile(p guard.PermissionProfile) *TestEvaluator {
	e.Profile = p
	return e
}

// shellRunner runs a command via the platform shell and returns combined output + exit error.
func runShellCommand(ctx context.Context, command, workdir string) (string, error) {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd", "/c", command)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", command)
	}
	if workdir != "" {
		cmd.Dir = workdir
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// Evaluate runs each acceptance test command. Pass = all exit 0.
// Gaps contains the names of failing tests. Evidence is combined output (truncated).
// Commands are gated through the permission profile: disallowed commands
// are marked failed without being executed (fail-closed).
func (e TestEvaluator) Evaluate(ctx context.Context, _ Goal, p Plan, workdir string) (EvalVerdict, error) {
	const maxEvidence = 4096

	// Fail-closed: a plan with no acceptance tests cannot be verified.
	if len(p.Tests) == 0 {
		return EvalVerdict{
			Evaluator: "test",
			Pass:      false,
			Gaps:      []string{"no acceptance tests configured"},
		}, nil
	}

	profile := e.Profile
	if len(profile.Tools.Allow) == 0 {
		profile = defaultTestProfile()
	}
	g := guard.New()

	var (
		gaps     []string
		evidence strings.Builder
		allPass  = true
	)

	for _, test := range p.Tests {
		// Gate the command through the guard before executing.
		d := g.Check(profile, guard.Action{Tool: "test.run", Shell: test.Command})
		if !d.IsAllowed() {
			allPass = false
			gaps = append(gaps, test.Name)
			gap := fmt.Sprintf("command not allowed by guard: %s", test.Command)
			if evidence.Len() < maxEvidence {
				remaining := maxEvidence - evidence.Len()
				chunk := gap
				if len(chunk) > remaining {
					chunk = chunk[:remaining]
				}
				evidence.WriteString(fmt.Sprintf("=== %s ===\n%s\n", test.Name, chunk))
			}
			continue
		}

		out, err := runShellCommand(ctx, test.Command, workdir)
		if evidence.Len() < maxEvidence {
			remaining := maxEvidence - evidence.Len()
			chunk := out
			if len(chunk) > remaining {
				chunk = chunk[:remaining]
			}
			evidence.WriteString(fmt.Sprintf("=== %s ===\n%s\n", test.Name, chunk))
		}
		if err != nil {
			allPass = false
			gaps = append(gaps, test.Name)
		}
	}

	return EvalVerdict{
		Evaluator: "test",
		Pass:      allPass,
		Evidence:  evidence.String(),
		Gaps:      gaps,
	}, nil
}

// ---------------------------------------------------------------------------
// FakeEvaluator — returns a scripted verdict (for tests).
// ---------------------------------------------------------------------------

// FakeEvaluator returns the pre-set Verdict, or Err if non-nil.
type FakeEvaluator struct {
	Verdict EvalVerdict
	Err     error
}

// Evaluate returns the scripted verdict.
func (e FakeEvaluator) Evaluate(_ context.Context, _ Goal, _ Plan, _ string) (EvalVerdict, error) {
	if e.Err != nil {
		return EvalVerdict{}, e.Err
	}
	return e.Verdict, nil
}

// ---------------------------------------------------------------------------
// IntentEvaluator — LLM-based: does the implementation match the goal intent?
// ---------------------------------------------------------------------------

// IntentEvaluator uses an LLM to judge whether the implementation matches the goal intent.
type IntentEvaluator struct {
	Model model.BaseChatModel
	// Sink accumulates each Evaluate call's token usage (G02). Nil = no-op.
	Sink *UsageSink
}

// llmVerdictJSON is the JSON schema the model is asked to return.
type llmVerdictJSON struct {
	Pass   bool     `json:"pass"`
	Gaps   []string `json:"gaps"`
	Reason string   `json:"reason"`
}

// Evaluate judges whether the implementation matches the goal intent.
func (e IntentEvaluator) Evaluate(ctx context.Context, g Goal, p Plan, workdir string) (EvalVerdict, error) {
	prompt := fmt.Sprintf(
		"You are evaluating whether an implementation matches the original goal.\n\n"+
			"Goal: %s\n"+
			"Plan steps: %s\n"+
			"Workdir: %s\n\n"+
			"Respond with ONLY a JSON object (no prose):\n"+
			`{"pass":true/false,"gaps":["<gap 1>","<gap 2>"],"reason":"<one sentence>"}`,
		g.Text,
		strings.Join(p.Steps, "; "),
		workdir,
	)

	msgs := []*schema.Message{schema.UserMessage(prompt)}
	resp, err := e.Model.Generate(ctx, msgs)
	if err != nil {
		return EvalVerdict{}, fmt.Errorf("intent evaluator generate: %w", err)
	}
	addUsage(e.Sink, resp) // G02: count this call's tokens

	vj, err := parseLLMVerdict(resp.Content)
	if err != nil {
		return EvalVerdict{}, fmt.Errorf("intent evaluator parse response: %w", err)
	}

	return EvalVerdict{
		Evaluator: "intent",
		Pass:      vj.Pass,
		Evidence:  vj.Reason,
		Gaps:      vj.Gaps,
	}, nil
}

// ---------------------------------------------------------------------------
// QualityEvaluator — LLM-based: is the code quality acceptable?
// ---------------------------------------------------------------------------

// QualityEvaluator uses an LLM to judge code quality of the implementation.
type QualityEvaluator struct {
	Model model.BaseChatModel
	// Sink accumulates each Evaluate call's token usage (G02). Nil = no-op.
	Sink *UsageSink
}

// Evaluate judges code quality of the implementation.
func (e QualityEvaluator) Evaluate(ctx context.Context, g Goal, p Plan, workdir string) (EvalVerdict, error) {
	prompt := fmt.Sprintf(
		"You are evaluating the code quality of an implementation.\n\n"+
			"Goal: %s\n"+
			"Plan steps: %s\n"+
			"Workdir: %s\n\n"+
			"Consider: naming, structure, error handling, test coverage.\n"+
			"Respond with ONLY a JSON object (no prose):\n"+
			`{"pass":true/false,"gaps":["<gap 1>"],"reason":"<one sentence>"}`,
		g.Text,
		strings.Join(p.Steps, "; "),
		workdir,
	)

	msgs := []*schema.Message{schema.UserMessage(prompt)}
	resp, err := e.Model.Generate(ctx, msgs)
	if err != nil {
		return EvalVerdict{}, fmt.Errorf("quality evaluator generate: %w", err)
	}
	addUsage(e.Sink, resp) // G02: count this call's tokens

	vj, err := parseLLMVerdict(resp.Content)
	if err != nil {
		return EvalVerdict{}, fmt.Errorf("quality evaluator parse response: %w", err)
	}

	return EvalVerdict{
		Evaluator: "quality",
		Pass:      vj.Pass,
		Evidence:  vj.Reason,
		Gaps:      vj.Gaps,
	}, nil
}

// parseLLMVerdict extracts and parses an llmVerdictJSON from the model's response.
// It tolerates markdown code fences and leading/trailing whitespace.
func parseLLMVerdict(content string) (llmVerdictJSON, error) {
	raw := extractJSON(content)
	var vj llmVerdictJSON
	if err := json.Unmarshal([]byte(raw), &vj); err != nil {
		return llmVerdictJSON{}, fmt.Errorf("parse: %w", err)
	}
	return vj, nil
}
