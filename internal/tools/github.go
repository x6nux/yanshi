package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/x6nux/yanshi/internal/sandbox"
	"github.com/x6nux/yanshi/internal/secproc"
)

// GitHubTools bundles guarded GitHub sub-tools (PR context, comment, approve, merge).
type GitHubTools struct {
	PRContext *GuardedTool
	Comment   *GuardedTool
	Approve   *GuardedTool
	Merge     *GuardedTool
}

// GitHubContext holds parsed GitHub PR metadata.
type GitHubContext struct {
	Number      int              `json:"number"`
	Title       string           `json:"title"`
	Body        string           `json:"body"`
	Author      string           `json:"author"`
	HeadRef     string           `json:"head_ref"`
	BaseRef     string           `json:"base_ref"`
	Files       []GitHubFileStat `json:"files"`
	ChangedFile int              `json:"changed_files"`
}

// GitHubFileStat holds per-file stats from a GitHub PR.
type GitHubFileStat struct {
	Path      string `json:"path"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
}

// FetchGitHubContext is a NARROW export: a pure parser that takes the JSON
// stdout of `gh pr view --json number,title,body,...` and returns a structured
// GitHubContext. Both the github_pr_context tool and the `yanshi pr` command
// use it. The caller (tool or command) is responsible for actually executing
// `gh` — this function only parses.
func FetchGitHubContext(repo string, number int, ghJSON string) (GitHubContext, error) {
	var raw struct {
		Number      int    `json:"number"`
		Title       string `json:"title"`
		Body        string `json:"body"`
		HeadRefName string `json:"headRefName"`
		BaseRefName string `json:"baseRefName"`
		Author      struct {
			Login string `json:"login"`
		} `json:"author"`
		Files []struct {
			Path      string `json:"path"`
			Additions int    `json:"additions"`
			Deletions int    `json:"deletions"`
		} `json:"files"`
		ChangedFiles int `json:"changedFiles"`
	}
	if err := json.Unmarshal([]byte(ghJSON), &raw); err != nil {
		return GitHubContext{}, fmt.Errorf("parse GitHub PR %s#%d: %w", repo, number, err)
	}
	ctx := GitHubContext{
		Number: raw.Number, Title: raw.Title, Body: raw.Body,
		HeadRef: raw.HeadRefName, BaseRef: raw.BaseRefName,
		Author: raw.Author.Login, ChangedFile: raw.ChangedFiles,
	}
	for _, f := range raw.Files {
		ctx.Files = append(ctx.Files, GitHubFileStat{Path: f.Path, Additions: f.Additions, Deletions: f.Deletions})
	}
	return ctx, nil
}

// NewGitHubTools constructs the guarded GitHub tools.
func NewGitHubTools(ghFactory secproc.Factory) *GitHubTools {
	_ = ghFactory // factory is bound per-turn via context, not at construction
	return &GitHubTools{
		PRContext: NewGuardedTool("github_pr_context", "GitHub PR context",
			"Fetch PR metadata, files, and diff from GitHub.", 30*time.Second,
			params(map[string]*schema.ParameterInfo{
				"repo":   {Type: schema.String, Required: true, Desc: "owner/repo"},
				"number": {Type: schema.Integer, Required: true, Desc: "PR number"},
			}), SyncStream(runGitHubPRContext)),
		Comment: NewApprovalGuardedTool("github_comment", "GitHub comment",
			"Post a comment on a GitHub PR.", 30*time.Second,
			params(map[string]*schema.ParameterInfo{
				"repo":   {Type: schema.String, Required: true, Desc: "owner/repo"},
				"number": {Type: schema.Integer, Required: true, Desc: "PR number"},
				"body":   {Type: schema.String, Required: true, Desc: "Comment body"},
			}), SyncStream(runGitHubComment)),
		Approve: NewApprovalGuardedTool("github_approve", "GitHub approve",
			"Approve a GitHub PR.", 30*time.Second,
			params(map[string]*schema.ParameterInfo{
				"repo":   {Type: schema.String, Required: true, Desc: "owner/repo"},
				"number": {Type: schema.Integer, Required: true, Desc: "PR number"},
				"body":   {Type: schema.String, Desc: "Approval comment (optional)"},
			}), SyncStream(runGitHubApprove)),
		Merge: NewApprovalGuardedTool("github_merge", "GitHub merge",
			"Merge a GitHub PR.", 30*time.Second,
			params(map[string]*schema.ParameterInfo{
				"repo":   {Type: schema.String, Required: true, Desc: "owner/repo"},
				"number": {Type: schema.Integer, Required: true, Desc: "PR number"},
				"method": {Type: schema.String, Enum: []string{"merge", "squash", "rebase"}, Desc: "Merge method (default merge)"},
			}), SyncStream(runGitHubMerge)),
	}
}

// ghSpec builds a secproc spec for running `gh` with the given args.
func ghSpec(args ...string) secproc.SecureProcessSpec {
	return secproc.SecureProcessSpec{
		Tool: "github", Program: "gh", Args: args,
		UseSandboxTier: sandbox.FullAccess, // gh needs network + gh config reads
	}
}

// ghFailure reports the error result for a `gh` invocation that did not
// succeed, or "" when it did. Both failure shapes must be surfaced: a launch
// error (err) AND a non-zero exit code. Only checking err made every gh
// failure — not authenticated, PR not found, network refused — look like a
// success with empty stdout, so github_comment answered {"id":""} for a
// comment that was never posted.
func ghFailure(res commandResult, err error) string {
	if err != nil {
		return errorResult("gh: " + err.Error())
	}
	if res.ExitCode == 0 {
		return ""
	}
	return errorResult(fmt.Sprintf("gh: exited %d: %s", res.ExitCode, commandFailureTail(res)))
}

func runGitHubPRContext(ctx context.Context, argsJSON string) (string, error) {
	var p struct {
		Repo   string `json:"repo"`
		Number int    `json:"number"`
	}
	if err := ParseArgs(argsJSON, &p); err != nil {
		return errorResult(err.Error()), nil
	}
	res, err := secureCommandRunner(ctx, ghSpec("pr", "view", "--repo", p.Repo, "--json",
		"number,title,body,headRefName,baseRefName,author,files,changedFiles", fmt.Sprintf("%d", p.Number)), 30*time.Second)
	if fail := ghFailure(res, err); fail != "" {
		return fail, nil
	}
	ghCtx, err := FetchGitHubContext(p.Repo, p.Number, res.Stdout)
	if err != nil {
		return errorResult(err.Error()), nil
	}
	return toJSON(ghCtx), nil
}

func runGitHubComment(ctx context.Context, argsJSON string) (string, error) {
	var p struct {
		Repo   string `json:"repo"`
		Number int    `json:"number"`
		Body   string `json:"body"`
	}
	if err := ParseArgs(argsJSON, &p); err != nil {
		return errorResult(err.Error()), nil
	}
	res, err := secureCommandRunner(ctx, ghSpec("pr", "comment", "--repo", p.Repo, "--body", p.Body, fmt.Sprintf("%d", p.Number)), 30*time.Second)
	if fail := ghFailure(res, err); fail != "" {
		return fail, nil
	}
	return toJSON(struct {
		ID string `json:"id"`
	}{ID: strings.TrimSpace(res.Stdout)}), nil
}

func runGitHubApprove(ctx context.Context, argsJSON string) (string, error) {
	var p struct {
		Repo   string `json:"repo"`
		Number int    `json:"number"`
		Body   string `json:"body,omitempty"`
	}
	if err := ParseArgs(argsJSON, &p); err != nil {
		return errorResult(err.Error()), nil
	}
	ghArgs := []string{"pr", "review", "--approve", "--repo", p.Repo}
	if p.Body != "" {
		ghArgs = append(ghArgs, "--body", p.Body)
	}
	ghArgs = append(ghArgs, fmt.Sprintf("%d", p.Number))
	res, err := secureCommandRunner(ctx, ghSpec(ghArgs...), 30*time.Second)
	if fail := ghFailure(res, err); fail != "" {
		return fail, nil
	}
	return toJSON(struct {
		Result string `json:"result"`
	}{Result: strings.TrimSpace(res.Stdout)}), nil
}

func runGitHubMerge(ctx context.Context, argsJSON string) (string, error) {
	var p struct {
		Repo   string `json:"repo"`
		Number int    `json:"number"`
		Method string `json:"method"`
	}
	if err := ParseArgs(argsJSON, &p); err != nil {
		return errorResult(err.Error()), nil
	}
	switch p.Method {
	case "", "merge", "squash", "rebase":
	default:
		return errorResult(fmt.Sprintf("unknown merge method %q (merge|squash|rebase)", p.Method)), nil
	}
	ghArgs := []string{"pr", "merge", "--repo", p.Repo}
	if p.Method != "" {
		ghArgs = append(ghArgs, "--"+p.Method)
	}
	ghArgs = append(ghArgs, fmt.Sprintf("%d", p.Number))
	res, err := secureCommandRunner(ctx, ghSpec(ghArgs...), 30*time.Second)
	if fail := ghFailure(res, err); fail != "" {
		return fail, nil
	}
	return toJSON(struct {
		Result string `json:"result"`
	}{Result: strings.TrimSpace(res.Stdout)}), nil
}
