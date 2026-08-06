package main

import (
	"bytes"
	"context"
	"fmt"
	"github.com/x6nux/yanshi/internal/bootstrap"
	"os"
	osexec "os/exec"
	"strconv"
	"strings"

	"github.com/x6nux/yanshi/internal/tools"
)

// runPR is the entry point for `yanshi pr <PR-number-or-URL>`. It fetches PR
// metadata + diff via `gh`, parses the metadata via the shared FetchGitHubContext
// narrow export, then runs the headless review pipeline.
// prBuildConfigPath is the config `yanshi pr` builds its App from. A
// package-level var so tests can point it at a temp config instead of
// whatever the developer has in the working directory.
var prBuildConfigPath = "config.yaml"

func runPR(ctx context.Context, prInput string) int {
	repo, number := parsePRInput(prInput)
	if repo == "" || number <= 0 {
		fmt.Fprintln(os.Stderr, "Usage: yanshi pr <PR-number>  (run from the repo directory)")
		fmt.Fprintln(os.Stderr, "       yanshi pr <full-URL>   (any repo)")
		return 1
	}

	// Run `gh pr view` to get JSON metadata.
	viewOut, viewErr, err := ghExec(ctx, "pr", "view", "--repo", repo, "--json",
		"number,title,body,headRefName,baseRefName,author,files,changedFiles",
		strconv.Itoa(number))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error running gh: %v\n%s\n", err, viewErr)
		return 1
	}

	ghCtx, err := tools.FetchGitHubContext(repo, number, viewOut)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing GitHub context: %v\n", err)
		return 1
	}

	// Get diff via `gh pr diff`.
	diff, diffErr, err := ghExec(ctx, "pr", "diff", "--repo", repo, strconv.Itoa(number))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error getting diff: %v\n%s\n", err, diffErr)
		return 1
	}

	// Build review input and run headless.
	//
	// The App is built here rather than reusing the process context because
	// the review tool reads its sub-agent runner, profile and work root from
	// context values that ONLY a turn binds. Passing the bare ctx made
	// streamReview answer "review requires a bound sub-agent runner" on every
	// single invocation -- `yanshi pr` had never worked, and nothing at build
	// or startup said so.
	app, err := bootstrap.Build(bootstrap.Options{ConfigPath: prBuildConfigPath})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Review failed: cannot build app: %v\n", err)
		return 1
	}
	defer func() { _ = app.Shutdown(context.Background()) }()
	if app.Orch == nil {
		fmt.Fprintln(os.Stderr, "Review failed: no orchestrator (configure an llm provider)")
		return 1
	}
	ctx = app.Orch.BindHeadlessContext(ctx)

	prompt := buildPRReviewPrompt(ghCtx, diff)
	result, err := runHeadlessPrompt(ctx, "review", prompt)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Review failed: %v\n", err)
		return 1
	}
	fmt.Println(result)
	return 0
}

// ghExec runs the `gh` CLI with args and returns (stdout, stderr, err). It is a
// package-level var so tests can inject a fake runner (returning canned
// metadata/diff) without spawning gh or needing GitHub auth; production leaves
// the default, which shells out to the real gh binary.
var ghExec = realGHExec

func realGHExec(ctx context.Context, args ...string) (stdout, stderr string, err error) {
	var out, errb bytes.Buffer
	cmd := osexec.CommandContext(ctx, "gh", args...)
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if rerr := cmd.Run(); rerr != nil {
		return out.String(), errb.String(), rerr
	}
	return out.String(), errb.String(), nil
}

// parsePRInput returns (repo, number) from a URL or raw number.
func parsePRInput(input string) (string, int) {
	// Try URL: https://github.com/owner/repo/pull/42
	if strings.Contains(input, "github.com") {
		parts := strings.Split(strings.TrimRight(input, "/"), "/")
		for i, p := range parts {
			if p == "pull" && i+1 < len(parts) {
				n, err := strconv.Atoi(parts[i+1])
				if err != nil {
					return "", 0
				}
				if i-2 >= 0 {
					return strings.Join(parts[i-2:i], "/"), n
				}
				return "", 0
			}
		}
		return "", 0
	}

	// Try raw number: infer repo from git remote.
	n, err := strconv.Atoi(input)
	if err != nil {
		return "", 0
	}
	repo := detectGitHubRemote()
	return repo, n
}

// detectGitHubRemote runs `git remote get-url origin` and extracts owner/repo.
func detectGitHubRemote() string {
	var out bytes.Buffer
	cmd := osexec.Command("git", "remote", "get-url", "origin")
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return ""
	}
	return parseGitHubRemoteURL(strings.TrimSpace(out.String()))
}

// parseGitHubRemoteURL extracts owner/repo from a git remote URL — both the SSH
// form (git@github.com:owner/repo.git) and the HTTPS form
// (https://github.com/owner/repo). Non-github remotes and unparseable strings
// return "". Extracted from detectGitHubRemote so the SSH/HTTPS/non-github
// branches are unit-testable without spawning git.
func parseGitHubRemoteURL(remote string) string {
	remote = strings.TrimSuffix(remote, ".git")
	if idx := strings.Index(remote, "github.com:"); idx >= 0 {
		return remote[idx+len("github.com:"):]
	}
	if idx := strings.Index(remote, "github.com/"); idx >= 0 {
		return remote[idx+len("github.com/"):]
	}
	return ""
}

func buildPRReviewPrompt(ghCtx tools.GitHubContext, diff string) string {
	return fmt.Sprintf(`Review PR #%d (%s) by %s

Title: %s
Description: %s
Files changed: %d

Diff:
%s

Return findings in JSON exactly as: {"findings":[{"file":"path","line":N,"severity":"high|medium|low|info","message":"...","rule":"..."}]}`,
		ghCtx.Number, ghCtx.HeadRef, ghCtx.Author, ghCtx.Title, ghCtx.Body, len(ghCtx.Files), diff)
}

// runHeadlessPrompt dispatches a headless tool invocation. Only "review" is
// currently supported.
func runHeadlessPrompt(ctx context.Context, toolName, diff string) (string, error) {
	switch toolName {
	case "review":
		return tools.RunReviewHeadless(ctx, diff)
	default:
		return "", fmt.Errorf("unknown headless tool: %s", toolName)
	}
}
