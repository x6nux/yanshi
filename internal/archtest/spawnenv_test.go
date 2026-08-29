package archtest

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// spawnenv_test.go is the census of what every subprocess yanshi starts sees in
// its environment.
//
// # Why it exists
//
// secproc's package header says every spawn of an untrusted program goes
// through Authorize. That is about AUTHORIZATION. It says nothing about the
// child's ENVIRONMENT, and the two came apart badly: nine production files
// called exec.Command without setting cmd.Env, which in Go means "inherit the
// parent's whole environment". yanshi's parent environment holds the operator's
// provider API keys, cloud credentials and VCS tokens.
//
// The affected children were not exotic. They were stdio MCP servers (a binary
// named by a config file), language servers, `gh`, `git`, the screenshot
// backend a model tool starts, the clipboard helpers, and the version probes
// that run at boot. Every one of them received OPENAI_API_KEY. The scrub that
// covers shell_run and the ACP agents lives in shell.childLaunchPosture, and
// none of these goes through it.
//
// One fix is not enough here, because the defect was not one mistake — it was
// nine independent authors each writing the two-line form of exec.Command. What
// keeps the tenth from happening is this file: a new production file that
// spawns anything must state, in one line, what the child's environment is.
//
// # What is machine-enforced, and what is not
//
// ENFORCED: the KEY SET, in both directions. A non-test file under internal/ or
// cmd/ containing an exec.Command call and not listed here fails. An entry
// naming a file that no longer spawns anything fails — a dead entry is a
// pre-authorisation for whatever reappears at that path.
//
// NOT ENFORCED: whether the answer is TRUE. This is a text scan, not a taint
// analysis; it cannot tell "cmd.Env = netpolicy.ScrubbedEnviron()" from a
// comment saying so. The value is that adding a spawn site now costs a sentence
// somebody has to write, which is exactly the moment to notice the question.
// Do not quote this file as proof that a child is scrubbed; quote the child
// test in internal/netpolicy for the mechanism and this table for the coverage.
var spawnEnvCensus = map[string]string{
	// ---- runtime, credential-scrubbed -------------------------------------
	"internal/shell/process.go": "cmd.Env from LaunchSpec.Env, built by " +
		"childLaunchPosture.env: host baseline, credentials stripped under AllowEnv, " +
		"managed proxy variables published. The reference posture.",
	"internal/shell/console_unix.go": "same LaunchSpec.Env as the pipe path; the PTY " +
		"backend differs in the console, not the environment.",
	"internal/tools/shell.go": "goes through secproc.Launch → shell.DefaultSecureFactory, " +
		"so the posture above applies. shell_run declares no AllowEnv: no credentials at all.",
	"internal/mcp/manager.go": "netpolicy.ScrubbedEnviron() plus the server's own " +
		"mcp.servers.<name>.env layered on top, so an operator-declared token survives and " +
		"an inherited one does not.",
	"internal/lsp/manager.go": "netpolicy.ScrubbedEnviron(), no allowlist: no language " +
		"server authenticates to anything.",
	"internal/skills/install.go": "netpolicy.ScrubbedEnviron(). The clone URL is public " +
		"HTTPS; an authenticated clone uses git's credential helper, which reads config " +
		"files rather than the environment.",
	"internal/execprobe/probe.go": "netpolicy.ScrubbedEnviron(). A `tool --version` probe " +
		"reads a banner and needs nothing else.",
	"internal/clipimg/clipimg.go": "netpolicy.ScrubbedEnviron() in the single commandOutput " +
		"seam, so all four platform backends are covered at once.",
	"internal/tools/screenshot.go": "captureCommand sets netpolicy.ScrubbedEnviron(). " +
		"The three platform files build their argv and hand it here, so the darwin, linux " +
		"and windows backends cannot disagree about the child's environment.",
	"cmd/yanshi/pr.go": "netpolicy.ScrubbedEnviron(netpolicy.GitHubCLICredentialEnv...) for " +
		"gh, which authenticates with GH_TOKEN or not at all. The `git remote get-url` call " +
		"in the same file inherits — it is a local read of .git/config with no network leg.",
	"internal/agent/goalloop/implementer.go": "gitWorktreeCommand → " +
		"netpolicy.ScrubbedEnviron(). Local worktree add/remove/list, no network leg.",

	// ---- runtime, deliberately NOT scrubbed --------------------------------
	"internal/agent/goalloop/evaluators.go": "INHERITS, deliberately. This runs the " +
		"PROJECT'S OWN test command (`sh -c <cfg.TestCommand>`), which routinely needs the " +
		"credentials that project's tests are configured with — a DATABASE_URL, a service " +
		"token. Scrubbing here would break test suites for a leak the operator already " +
		"accepted by pointing the goal loop at their repo. The command comes from config, " +
		"not from the model.",

	// ---- sandbox backends: argv rewriting and probes ------------------------
	"internal/sandbox/sandbox_linux_bwrap.go": "the probe sets cmd.Env = []string{} " +
		"(nothing at all). Production spawns are argv REWRITES of a command the shell " +
		"factory already built, so the environment is that factory's.",
	"internal/sandbox/sandbox_linux_landlock.go": "same: runLandlockProbe sets an empty " +
		"Env, and Prepare rewrites argv rather than spawning.",
	"internal/sandbox/sandbox_darwin.go":  "argv rewrite only; the probe sets an empty Env.",
	"internal/sandbox/sandbox_windows.go": "argv rewrite only; the probe sets an empty Env.",

	// ---- developer tooling, not part of the running product -----------------
	"cmd/covercheck/main.go":  "developer tool: runs `go test` from the maintainer's own shell.",
	"cmd/depsanalyze/main.go": "developer tool: runs `go list`.",
	"cmd/gendocs/help.go":     "developer tool: runs the yanshi binary to capture -h text.",
	"cmd/testchanged/main.go": "developer tool: runs `git diff` and `go test`.",
	"cmd/tuidbg/session.go":   "developer tool: drives tmux.",
}

// spawnCallRe matches a call to exec.Command / exec.CommandContext, under any
// import alias, ignoring lines that are pure comments.
//
// Alias-tolerant because cmd/yanshi imports os/exec as osexec, and a gate that
// only knew the canonical spelling would have missed the `gh` site — one of the
// nine this census was written for.
var spawnCallRe = regexp.MustCompile(`\b[A-Za-z_][A-Za-z0-9_]*\.Command(Context)?\(`)

// commentLineRe matches a line whose first non-space characters open a comment.
var commentLineRe = regexp.MustCompile(`^\s*(//|\*|/\*)`)

// TestEverySpawnSiteDeclaresItsChildEnvironment enumerates the spawn surface in
// both directions.
func TestEverySpawnSiteDeclaresItsChildEnvironment(t *testing.T) {
	root := moduleRoot(t)
	found := map[string]bool{}

	for _, dir := range []string{"internal", "cmd"} {
		base := filepath.Join(root, dir)
		err := filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				name := d.Name()
				if name == "testdata" || strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".") {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			raw, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			if !fileSpawns(string(raw)) {
				return nil
			}
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				return relErr
			}
			found[filepath.ToSlash(rel)] = true
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}

	var missing, dead []string
	for file := range found {
		if _, ok := spawnEnvCensus[file]; !ok {
			missing = append(missing, file)
		}
	}
	for file := range spawnEnvCensus {
		if !found[file] {
			dead = append(dead, file)
		}
	}
	sort.Strings(missing)
	sort.Strings(dead)

	if len(missing) > 0 {
		t.Errorf("%d file(s) start a subprocess and do not say what its environment is:\n  %s\n\n"+
			"Go's exec.Cmd inherits the parent environment when Env is nil, and yanshi's "+
			"parent environment holds the operator's provider API keys. Add an entry to "+
			"spawnEnvCensus answering, in one line, what the child sees. \"INHERITS, "+
			"deliberately, because …\" is a legitimate answer; silence is not.",
			len(missing), strings.Join(missing, "\n  "))
	}
	if len(dead) > 0 {
		t.Errorf("%d census entr(y|ies) name a file that no longer spawns anything:\n  %s\n\n"+
			"A dead entry pre-authorises whatever reappears at that path — the next spawn "+
			"site added to one of these files would be waved through. Remove it.",
			len(dead), strings.Join(dead, "\n  "))
	}
}

// fileSpawns reports whether src contains a real exec.Command call rather than
// only a mention of one in a comment.
//
// The comment filter is the same lesson CLAUDE.md records for counting these
// call sites by hand: five of the matches in this repository are prose, and a
// census that counted them would demand an entry for a file that spawns
// nothing — which then becomes a dead entry nobody can discharge.
func fileSpawns(src string) bool {
	for _, line := range strings.Split(src, "\n") {
		if commentLineRe.MatchString(line) {
			continue
		}
		if spawnCallRe.MatchString(line) {
			return true
		}
	}
	return false
}
