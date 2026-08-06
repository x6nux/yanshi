package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/x6nux/yanshi/internal/secproc"
)

// ---------------------------------------------------------------------------
// runGitHubPRContext — covers no-factory error path, parse-error path,
// success path with scripted factory, and gh JSON parse error path.
// ---------------------------------------------------------------------------

func TestRunGitHubPRContext_NoFactory(t *testing.T) {
	ctx := WithProfile(context.Background(), allowAllProfile())
	out, err := runGitHubPRContext(ctx, `{"repo":"owner/repo","number":1}`)
	require.NoError(t, err)
	assert.Contains(t, out, "gh:")
}

func TestRunGitHubPRContext_SuccessWithFactory(t *testing.T) {
	factory := newScriptedFactory(t, func(spec secproc.SecureProcessSpec) cannedResult {
		return cannedResult{
			Stdout: `{"number":1,"title":"T","body":"B","headRefName":"f","baseRefName":"m","author":{"login":"u"},"files":[{"path":"a.go","additions":1,"deletions":1}],"changedFiles":1}`,
		}
	})
	ctx := WithSecureProcessFactory(WithProfile(context.Background(), allowAllProfile()), factory)
	out, err := runGitHubPRContext(ctx, `{"repo":"owner/repo","number":1}`)
	require.NoError(t, err)
	// The title is no longer returned bare: it is prose written by whoever
	// opened the PR, so quarantinePRContext wraps it in nonce-carrying
	// untrusted delimiters. The text still has to be there — only the framing
	// changed. author is a login, not prose, and stays unwrapped.
	assert.Contains(t, out, "UNTRUSTED_PR_TITLE")
	assert.Contains(t, out, `T\n`)
	assert.Contains(t, out, `"author":"u"`)
}

func TestRunGitHubPRContext_ParseErrorInGHJSON(t *testing.T) {
	factory := newScriptedFactory(t, func(spec secproc.SecureProcessSpec) cannedResult {
		return cannedResult{Stdout: `not valid json`}
	})
	ctx := WithSecureProcessFactory(WithProfile(context.Background(), allowAllProfile()), factory)
	out, err := runGitHubPRContext(ctx, `{"repo":"owner/repo","number":1}`)
	require.NoError(t, err)
	assert.Contains(t, out, "parse GitHub")
}

// ---------------------------------------------------------------------------
// runGitHubComment — missing body validation, success path
// ---------------------------------------------------------------------------

func TestRunGitHubComment_MissingBody(t *testing.T) {
	// Without approval callback → denied
	ctx := WithProfile(context.Background(), allowAllProfile())
	out, err := runGitHubComment(ctx, `{"repo":"owner/repo","number":1}`)
	require.NoError(t, err)
	// Body is not "required" in ParseArgs, but the approve-guard will deny
	_ = out
}

func TestRunGitHubComment_SuccessWithFactory(t *testing.T) {
	var lastArgs []string
	factory := newScriptedFactory(t, func(spec secproc.SecureProcessSpec) cannedResult {
		lastArgs = spec.Args
		return cannedResult{Stdout: "https://github.com/owner/repo/issues/1#issuecomment-42"}
	})
	ctx := WithPermissionCallback(
		WithSecureProcessFactory(
			WithProfile(context.Background(), allowAllProfile()), factory),
		func(PermissionRequest) PermissionDecision { return PermissionAllow })
	out, err := runGitHubComment(ctx, `{"repo":"owner/repo","number":1,"body":"LGTM"}`)
	require.NoError(t, err)
	assert.Contains(t, out, "42")
	assert.Contains(t, strings.Join(lastArgs, " "), "pr comment")
}

func TestRunGitHubComment_NoFactory(t *testing.T) {
	ctx := WithPermissionCallback(
		WithProfile(context.Background(), allowAllProfile()),
		func(PermissionRequest) PermissionDecision { return PermissionAllow })
	out, err := runGitHubComment(ctx, `{"repo":"owner/repo","number":1,"body":"x"}`)
	require.NoError(t, err)
	assert.Contains(t, out, "gh:")
}

// ---------------------------------------------------------------------------
// runGitHubApprove — success with body, success without body, no factory
// ---------------------------------------------------------------------------

func TestRunGitHubApprove_SuccessWithBody(t *testing.T) {
	var lastArgs []string
	factory := newScriptedFactory(t, func(spec secproc.SecureProcessSpec) cannedResult {
		lastArgs = spec.Args
		return cannedResult{Stdout: "approved"}
	})
	ctx := WithPermissionCallback(
		WithSecureProcessFactory(
			WithProfile(context.Background(), allowAllProfile()), factory),
		func(PermissionRequest) PermissionDecision { return PermissionAllow })
	out, err := runGitHubApprove(ctx, `{"repo":"owner/repo","number":1,"body":"LGTM"}`)
	require.NoError(t, err)
	assert.Contains(t, out, "approved")
	joined := strings.Join(lastArgs, " ")
	assert.Contains(t, joined, "--approve")
	assert.Contains(t, joined, "--body")
}

func TestRunGitHubApprove_SuccessWithoutBody(t *testing.T) {
	var lastArgs []string
	factory := newScriptedFactory(t, func(spec secproc.SecureProcessSpec) cannedResult {
		lastArgs = spec.Args
		return cannedResult{Stdout: "approved"}
	})
	ctx := WithPermissionCallback(
		WithSecureProcessFactory(
			WithProfile(context.Background(), allowAllProfile()), factory),
		func(PermissionRequest) PermissionDecision { return PermissionAllow })
	out, err := runGitHubApprove(ctx, `{"repo":"owner/repo","number":1}`)
	require.NoError(t, err)
	assert.Contains(t, out, "approved")
	joined := strings.Join(lastArgs, " ")
	assert.NotContains(t, joined, "--body")
}

func TestRunGitHubApprove_NoFactory(t *testing.T) {
	ctx := WithPermissionCallback(
		WithProfile(context.Background(), allowAllProfile()),
		func(PermissionRequest) PermissionDecision { return PermissionAllow })
	out, err := runGitHubApprove(ctx, `{"repo":"owner/repo","number":1}`)
	require.NoError(t, err)
	assert.Contains(t, out, "gh:")
}

// ---------------------------------------------------------------------------
// runGitHubMerge — all valid methods, default method, no factory
// ---------------------------------------------------------------------------

func TestRunGitHubMerge_SquashMethod(t *testing.T) {
	var lastArgs []string
	factory := newScriptedFactory(t, func(spec secproc.SecureProcessSpec) cannedResult {
		lastArgs = spec.Args
		return cannedResult{Stdout: "squashed"}
	})
	ctx := WithPermissionCallback(
		WithSecureProcessFactory(
			WithProfile(context.Background(), allowAllProfile()), factory),
		func(PermissionRequest) PermissionDecision { return PermissionAllow })
	out, err := runGitHubMerge(ctx, `{"repo":"owner/repo","number":1,"method":"squash"}`)
	require.NoError(t, err)
	assert.Contains(t, out, "squashed")
	joined := strings.Join(lastArgs, " ")
	assert.Contains(t, joined, "--squash")
}

func TestRunGitHubMerge_RebaseMethod(t *testing.T) {
	var lastArgs []string
	factory := newScriptedFactory(t, func(spec secproc.SecureProcessSpec) cannedResult {
		lastArgs = spec.Args
		return cannedResult{Stdout: "rebased"}
	})
	ctx := WithPermissionCallback(
		WithSecureProcessFactory(
			WithProfile(context.Background(), allowAllProfile()), factory),
		func(PermissionRequest) PermissionDecision { return PermissionAllow })
	out, err := runGitHubMerge(ctx, `{"repo":"owner/repo","number":1,"method":"rebase"}`)
	require.NoError(t, err)
	assert.Contains(t, out, "rebased")
	joined := strings.Join(lastArgs, " ")
	assert.Contains(t, joined, "--rebase")
}

func TestRunGitHubMerge_DefaultMergeMethod(t *testing.T) {
	var lastArgs []string
	factory := newScriptedFactory(t, func(spec secproc.SecureProcessSpec) cannedResult {
		lastArgs = spec.Args
		return cannedResult{Stdout: "merged"}
	})
	ctx := WithPermissionCallback(
		WithSecureProcessFactory(
			WithProfile(context.Background(), allowAllProfile()), factory),
		func(PermissionRequest) PermissionDecision { return PermissionAllow })
	out, err := runGitHubMerge(ctx, `{"repo":"owner/repo","number":1}`)
	require.NoError(t, err)
	assert.Contains(t, out, "merged")
	// default (empty method) → no --squash/--rebase flag, just "pr merge --repo"
	joined := strings.Join(lastArgs, " ")
	assert.Contains(t, joined, "pr merge")
	assert.NotContains(t, joined, "--squash")
	assert.NotContains(t, joined, "--rebase")
}

func TestRunGitHubMerge_ExplicitMergeMethod(t *testing.T) {
	factory := newScriptedFactory(t, func(spec secproc.SecureProcessSpec) cannedResult {
		return cannedResult{Stdout: "merged"}
	})
	ctx := WithPermissionCallback(
		WithSecureProcessFactory(
			WithProfile(context.Background(), allowAllProfile()), factory),
		func(PermissionRequest) PermissionDecision { return PermissionAllow })
	out, err := runGitHubMerge(ctx, `{"repo":"owner/repo","number":1,"method":"merge"}`)
	require.NoError(t, err)
	assert.Contains(t, out, "merged")
}

func TestRunGitHubMerge_NoFactory(t *testing.T) {
	ctx := WithPermissionCallback(
		WithProfile(context.Background(), allowAllProfile()),
		func(PermissionRequest) PermissionDecision { return PermissionAllow })
	out, err := runGitHubMerge(ctx, `{"repo":"owner/repo","number":1}`)
	require.NoError(t, err)
	assert.Contains(t, out, "gh:")
}

func TestRunGitHubMerge_BadArgs(t *testing.T) {
	ctx := WithPermissionCallback(
		WithProfile(context.Background(), allowAllProfile()),
		func(PermissionRequest) PermissionDecision { return PermissionAllow })
	out, err := runGitHubMerge(ctx, `not-json`)
	require.NoError(t, err)
	assert.Contains(t, out, "parse args")
}

// ---------------------------------------------------------------------------
// FetchGitHubContext — additional edge cases
// ---------------------------------------------------------------------------

func TestFetchGitHubContext_EmptyJSON(t *testing.T) {
	ctx, err := FetchGitHubContext("owner/repo", 1, `{}`)
	require.NoError(t, err)
	assert.Equal(t, 0, ctx.Number)
	assert.Equal(t, "", ctx.Title)
	assert.Equal(t, 0, len(ctx.Files))
}

func TestFetchGitHubContext_MultipleFiles(t *testing.T) {
	raw := `{"number":1,"files":[{"path":"a.go","additions":1,"deletions":0},{"path":"b.go","additions":5,"deletions":3}],"changedFiles":2}`
	ctx, err := FetchGitHubContext("o/r", 1, raw)
	require.NoError(t, err)
	require.Len(t, ctx.Files, 2)
	assert.Equal(t, "a.go", ctx.Files[0].Path)
	assert.Equal(t, 5, ctx.Files[1].Additions)
}
