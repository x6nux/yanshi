package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/secrets"
)

// leakyTool builds a GuardedTool whose Result carries the given string
// verbatim, standing in for `shell_run` printing `env`.
func leakyTool(t *testing.T, payload string) *GuardedTool {
	t.Helper()
	return NewGuardedTool("leaky", "Leaky", "emits its payload", time.Minute, nil,
		func(ctx context.Context, argsJSON string) <-chan ToolChunk {
			ch := make(chan ToolChunk, 1)
			ch <- ToolChunk{Result: payload, Text: payload}
			close(ch)
			return ch
		})
}

// allowAll is a profile that permits the fake tool so the test exercises the
// result path rather than a denial.
func allowAll() guard.PermissionProfile {
	return guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"*"}}}
}

// ledger: A2/W-A-02#1 工具结果在返回给编排器之前经过 Redactor
func TestInvokableRunRedactsRegisteredSecrets(t *testing.T) {
	const key = "sk-test-DEADBEEFdeadbeef0123456789"
	r := secrets.NewRedactor()
	r.Register(key)

	ctx := WithProfile(context.Background(), allowAll())
	ctx = WithRedactor(ctx, r)

	out, err := leakyTool(t, "OPENAI_API_KEY="+key+"\nPATH=/usr/bin").InvokableRun(ctx, "{}")
	require.NoError(t, err)
	require.NotContains(t, out, key, "a registered secret reached the model verbatim")
	require.Contains(t, out, "PATH=/usr/bin", "redaction must not eat the rest of the output")
}

// ledger: A2/W-A-02#2 未绑定 Redactor 时工具结果逐字节不变
func TestInvokableRunWithoutRedactorIsByteIdentical(t *testing.T) {
	const payload = "OPENAI_API_KEY=sk-test-DEADBEEFdeadbeef0123456789\nPATH=/usr/bin"

	ctx := WithProfile(context.Background(), allowAll())

	out, err := leakyTool(t, payload).InvokableRun(ctx, "{}")
	require.NoError(t, err)
	require.Equal(t, payload, out,
		"an unbound redactor must leave the pre-W-A-02 behaviour byte-identical")
}

// ledger: A2/W-A-02#3 TUI 路径的 Text 字段不受脱敏影响
func TestStreamTextFieldIsNotRedacted(t *testing.T) {
	const key = "sk-test-DEADBEEFdeadbeef0123456789"
	r := secrets.NewRedactor()
	r.Register(key)

	ctx := WithProfile(context.Background(), allowAll())
	ctx = WithRedactor(ctx, r)

	var text strings.Builder
	for c := range leakyTool(t, key).Stream(ctx, "{}") {
		text.WriteString(c.Text)
	}
	require.Contains(t, text.String(), key,
		"Text goes to the local TUI, not to the provider; redacting it would hide the operator's own key from them")
}
