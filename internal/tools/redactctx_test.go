package tools

import (
	"context"
	"os"
	"path/filepath"
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

// TestExplicitCredentialFileReadIsNotRedacted proves the W-A-02 clause that
// the first round dropped from the ledger: an fs tool that reads a path the
// user named must still return that path's content.
//
// The failure it pins is not hypothetical and not about registered secrets.
// secrets.Redact also runs the SHAPE-based pass, so before this exemption a
// PEM block, a JWT or an sk-/ghp_/AKIA token in ANY file the agent read was
// replaced before the model saw it — and since every distinct secret collapses
// to the same literal "[REDACTED]", "compare the key in .env with the one in
// config.yaml" made the model confidently report that two different keys
// matched. Neither key is registered here, for exactly that reason: nothing in
// the process resolved them.
//
// The second half is what makes this a test of the DISTINCTION rather than of
// "redaction is off": the identical bytes coming out of an execution-shaped
// tool must still be redacted. Delete the exemption and the first half fails;
// widen it to every tool and the second half fails.
//
// ledger: A2/W-A-02#5 显式读取凭据文件的 fs 工具不因本改动失效
func TestExplicitCredentialFileReadIsNotRedacted(t *testing.T) {
	const (
		envKey  = "sk-proj-AAAAAAAAAAAAAAAAAAAAAAAAAAAA1111"
		confKey = "sk-proj-BBBBBBBBBBBBBBBBBBBBBBBBBBBB2222"
	)
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, ".env"),
		[]byte("OPENAI_API_KEY="+envKey+"\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "config.yaml"),
		[]byte("api_key: "+confKey+"\n"), 0o600))

	// A redactor that also carries a process-resolved provider key, so the
	// exemption is exercised with a live registry rather than an empty one.
	r := secrets.NewRedactor()
	r.Register("sk-proc-CCCCCCCCCCCCCCCCCCCCCCCCCCCC3333")

	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
		FS:    guard.FSPerm{Read: []string{root + "/**"}},
	})
	ctx = WithRedactor(ctx, r)
	ctx = WithWorkRoot(ctx, root)

	fs := NewFSTools(root)
	envOut, err := fs.Read.InvokableRun(ctx, `{"path":".env"}`)
	require.NoError(t, err)
	confOut, err := fs.Read.InvokableRun(ctx, `{"path":"config.yaml"}`)
	require.NoError(t, err)

	require.Contains(t, envOut, envKey,
		"fs_read of a path the user named came back redacted; the tool is now useless "+
			"for the one job it was asked to do")
	require.Contains(t, confOut, confKey)
	require.NotEqual(t, envOut, confOut,
		"two different keys rendered identically; a model asked to compare them "+
			"would report that they match")

	// The other side of the distinction: the same bytes out of an execution
	// tool are still redacted, including the process-resolved key.
	leak := "OPENAI_API_KEY=" + envKey +
		"\nRESOLVED=sk-proc-CCCCCCCCCCCCCCCCCCCCCCCCCCCC3333\nPATH=/usr/bin"
	shellOut, err := leakyTool(t, leak).InvokableRun(ctx, "{}")
	require.NoError(t, err)
	require.NotContains(t, shellOut, envKey,
		"a shaped credential in an execution tool's stdout must stay redacted")
	require.NotContains(t, shellOut, "sk-proc-CCCCCCCCCCCCCCCCCCCCCCCCCCCC3333",
		"a process-resolved provider key must never reach the model from an execution tool")
	require.Contains(t, shellOut, "PATH=/usr/bin")
}
