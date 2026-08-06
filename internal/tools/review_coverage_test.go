package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/guard"
)

// ledger: B3/V13#3 clean 明确
func TestStreamReviewTool(t *testing.T) {
	runner := SubAgentRunner(func(ctx context.Context, prompt string, allowed []string, instr string) (string, error) {
		return `{"findings":[{"file":"test.go","line":1,"severity":"high","message":"bug","rule":"test"}]}`, nil
	})
	profile := guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"*"}}}
	ctx := WithProfile(WithSubAgentRunner(context.Background(), runner), profile)

	tool := NewAgentTools(nil)
	ch := tool.Review.Stream(ctx, `{"diff":"@@ -1 +1 @@\n-old\n+new\n"}`)
	var result string
	for c := range ch {
		if c.Result != "" {
			result += c.Result
		}
	}
	if !strings.Contains(result, "test.go") {
		t.Fatalf("expected findings in result: %q", result)
	}
}

func TestStreamReviewToolMissingRunner(t *testing.T) {
	profile := guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"*"}}}
	ctx := WithProfile(context.Background(), profile)
	tool := NewAgentTools(nil)
	ch := tool.Review.Stream(ctx, `{"diff":"@@ -1 +1 @@\n-old\n+new\n"}`)
	var result string
	for c := range ch {
		if c.Result != "" {
			result += c.Result
		}
	}
	if !strings.Contains(result, "review requires") {
		t.Fatalf("expected 'requires' error, got: %q", result)
	}
}

func TestRunReviewHeadless(t *testing.T) {
	runner := SubAgentRunner(func(ctx context.Context, prompt string, allowed []string, instr string) (string, error) {
		return `{"findings":[{"file":"main.go","line":5,"severity":"medium","message":"issue","rule":"test"}]}`, nil
	})
	profile := guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"*"}}}
	ctx := WithProfile(WithSubAgentRunner(context.Background(), runner), profile)

	result, err := RunReviewHeadless(ctx, "@@ -1,3 +1,4 @@\n func main() {\n+\n")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "main.go") {
		t.Fatalf("expected main.go in result: %q", result)
	}
}

func TestRunReviewHeadlessNoRunner(t *testing.T) {
	profile := guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"*"}}}
	ctx := WithProfile(context.Background(), profile)
	result, err := RunReviewHeadless(ctx, "diff")
	if err != nil {
		t.Fatal(err) // RunReviewHeadless returns errorResult, not Go error
	}
	if !strings.Contains(result, "requires") {
		t.Fatalf("expected requires error, got: %q", result)
	}
}

func TestFormatTokens(t *testing.T) {
	tests := []struct {
		n    int
		want string
	}{
		{0, "0"},
		{500, "500"},
		{999, "999"},
		{1000, "1K"},
		{1200, "1.2K"},
		{56000, "56K"},
		{999999, "1000K"},
		{1000000, "1M"},
		{1500000, "1.5M"},
		{1000000000, "1B"},
		{2500000000, "2.5B"},
	}
	for _, tc := range tests {
		got := formatTokens(tc.n)
		if got != tc.want {
			t.Errorf("formatTokens(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}
