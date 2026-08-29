package archtest

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
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
// keeps the tenth from happening is this file: a new production file whose
// spawn spawnCallRe recognises must state, in one line, what the child's
// environment is. (It did not keep the tenth from happening. task_gate_run was
// IN this table with an answer describing a different tool's code path — see
// the internal/tools/shell.go entry. The lesson recorded there is that an entry
// answering about the wrong call site reads as coverage.)
//
// # What is machine-enforced, and what is not
//
// ENFORCED, in three directions.
//
//  1. KEY SET, forward. A non-test file under internal/ or cmd/ holding a spawn
//     call spawnSitesIn RECOGNISES, and not listed here, fails.
//
//  2. KEY SET, backward. An entry naming a file that no longer spawns anything
//     fails — a dead entry is a pre-authorisation for whatever reappears at
//     that path.
//
//  3. SUBJECT. Every answer must name, verbatim, each top-level declaration in
//     that file that actually contains a spawn call. A declaration that is
//     renamed or deleted takes its entry red with it.
//
// The third direction is the one this table was missing, and it is the same
// gap CLAUDE.md records the eight debt tables closing: they originally checked
// only "is this still a violation" and not "does the subject still exist", so
// an entry pointing at something deleted became a PERMANENT PRE-AUTHORISATION.
// A census entry decays the same way and did: internal/tools/shell.go was
// listed with an answer describing shell_run's route through secproc.Launch,
// which W-B-02 had deleted from that file. The only spawn left there was
// shellCommand's, task_gate_run's, and it inherited every credential — while
// the row above it read like coverage. Naming shellCommand is now the price of
// the row, and deleting shellCommand is now a failure rather than a silence.
//
// "A spawn call spawnSitesIn recognises" is narrower than "spawns anything",
// and the difference is not a quibble: the first version of this file
// recognised only exec.Command, so two syscall.Exec sites and a
// windows.CreateProcess site were invisible while this paragraph claimed
// otherwise. The recognised and unrecognised shapes are enumerated on
// spawnCallRe and pinned by TestSpawnCensusRecognisesEveryKnownSpawnShape.
//
// NOT ENFORCED: whether the answer is TRUE. This is a text scan over an AST,
// not a taint analysis; it can tell you the answer is about the right function,
// it cannot tell "cmd.Env = netpolicy.ScrubbedEnviron()" from a sentence saying
// so. The value is that adding a spawn site now costs a sentence somebody has
// to write, about a function they have to name. Do not quote this file as proof
// that a child is scrubbed; quote the child test in internal/netpolicy for the
// mechanism and this table for the coverage.
var spawnEnvCensus = map[string]string{
	// Each answer opens with the declaration that holds the spawn call, because
	// that name is what the SUBJECT direction checks. Where a file has two, both
	// are named.

	// ---- runtime, credential-scrubbed -------------------------------------
	"internal/shell/process.go": "OSProcessFactory.Start: cmd.Env from LaunchSpec.Env, built " +
		"by childLaunchPosture.env — host baseline, credentials stripped under AllowEnv, " +
		"managed proxy variables published. The reference posture.",
	"internal/shell/console_unix.go": "StartPTYProcess: the same LaunchSpec.Env as the pipe " +
		"path; the PTY backend differs in the console, not the environment.",
	"internal/shell/console_windows.go": "startConPTYClient: conptyEnvBlock(spec.Env) — the " +
		"same LaunchSpec.Env as its unix twin, packed into the UTF-16 block CreateProcess " +
		"wants. The unix twin was in this table from the start and this one was not, purely " +
		"because the detector knew exec.Command and not CreateProcess.",
	// The exec.Command in this file is NOT shell_run's — shell_run reaches the
	// kernel through secproc.Launch and never lands here. The first version of
	// this entry described shell_run anyway and so answered for a spawn point
	// that does not exist in the file, while the one that does inherited every
	// credential. A census that answers about the wrong call site is worse than
	// no entry: it reads as coverage. Naming the declaration is what makes that
	// mistake mechanical to catch.
	"internal/tools/shell.go": "shellCommand sets netpolicy.ScrubbedEnviron() with no " +
		"allowlist (widened only by the operator-level YANSHI_ALLOW_CHILD_ENV). Its only " +
		"caller is task_gate_run, whose command string comes from the model and whose output " +
		"is fed back to the model as evidence.",
	"internal/mcp/manager.go": "startOne → stdioServerEnv: netpolicy.ScrubbedEnviron() plus " +
		"the server's own mcp.servers.<name>.env layered on top, so an operator-declared " +
		"token survives and an inherited one does not.",
	"internal/lsp/manager.go": "spawnLocked → languageServerEnv: netpolicy.ScrubbedEnviron() " +
		"with no per-server allowlist; an operator who needs NETRC or npm_config_* names it " +
		"in YANSHI_ALLOW_CHILD_ENV.",
	"internal/skills/install.go": "Clone: netpolicy.ScrubbedEnviron(). The clone URL is " +
		"public HTTPS; an authenticated clone uses git's credential helper, which reads " +
		"config files rather than the environment.",
	"internal/execprobe/probe.go": "Run: netpolicy.ScrubbedEnviron(). A `tool --version` " +
		"probe reads a banner and needs nothing else.",
	"internal/clipimg/clipimg.go": "commandOutput: netpolicy.ScrubbedEnviron() in that single " +
		"package-level seam, so all four platform backends are covered at once.",
	"internal/tools/screenshot.go": "captureCommand sets netpolicy.ScrubbedEnviron(). The " +
		"three platform files build their argv and hand it here, so the darwin, linux and " +
		"windows backends cannot disagree about the child's environment.",
	"cmd/yanshi/pr.go": "realGHExec: netpolicy.ScrubbedEnviron(netpolicy.GitHubCLICredentialEnv...) " +
		"for gh, which authenticates with GH_TOKEN, GH_ENTERPRISE_TOKEN or not at all. " +
		"detectGitHubRemote inherits — `git remote get-url` is a local read of .git/config " +
		"with no network leg.",
	"internal/agent/goalloop/implementer.go": "gitWorktreeCommand: netpolicy.ScrubbedEnviron(). " +
		"Local worktree add/remove/list, no network leg.",

	// ---- execve of THIS process: the environment is already whatever the
	// ---- launcher built, and os.Environ() reads it back rather than adding to it
	"internal/execbroker/shim.go": "RunShim: syscall.Exec with os.Environ() UNCHANGED. This " +
		"process IS the child — the shim was started by the launcher with an already-scrubbed " +
		"environment, and os.Environ() here is that environment read back. Passing it through " +
		"is also deliberate on its own terms: an approved `sudo make install` runs a make that " +
		"may itself invoke sudo, and dropping the shim variables would make one approval " +
		"silently cover everything below it.",
	"internal/sandbox/landlock_linux.go": "RunLandlockHelper: syscall.Exec with os.Environ() " +
		"UNCHANGED, same reason — the helper is this process re-execing the target it was told " +
		"to confine, so os.Environ() is the environment childLaunchPosture already built for " +
		"it. Building a new one here would DISCARD the scrub rather than apply it.",

	// ---- runtime, deliberately NOT scrubbed --------------------------------
	"internal/agent/goalloop/evaluators.go": "runShellCommand INHERITS, deliberately. This " +
		"runs the PROJECT'S OWN test command (`sh -c <cfg.TestCommand>`), which routinely " +
		"needs the credentials that project's tests are configured with — a DATABASE_URL, a " +
		"service token. Scrubbing here would break test suites for a leak the operator " +
		"already accepted by pointing the goal loop at their repo. The command comes from " +
		"config, not from the model.",

	// ---- sandbox backends: argv rewriting and probes ------------------------
	"internal/sandbox/sandbox_linux_bwrap.go": "runBwrapProbe sets cmd.Env = []string{} " +
		"(nothing at all). Production spawns are argv REWRITES of a command the shell " +
		"factory already built, so the environment is that factory's.",
	"internal/sandbox/sandbox_linux_landlock.go": "runLandlockProbe sets an empty Env, and " +
		"Prepare rewrites argv rather than spawning.",
	"internal/sandbox/sandbox_darwin.go":  "runProbe sets an empty Env; production is argv rewrite only.",
	"internal/sandbox/sandbox_windows.go": "runJobProbe sets an empty Env; production is argv rewrite only.",

	// ---- developer tooling, not part of the running product -----------------
	"cmd/covercheck/main.go":  "run: developer tool, executes `go test` from the maintainer's own shell.",
	"cmd/depsanalyze/main.go": "main: developer tool, executes `go list`.",
	"cmd/gendocs/help.go": "captureHelpLive and ensureYanshiBinary: developer tool, builds and " +
		"runs the yanshi binary to capture -h text.",
	"cmd/testchanged/main.go": "runGoTest, runGit and isGoPackage: developer tool, runs " +
		"`git diff`, `go list` and `go test`.",
	"cmd/tuidbg/session.go": "tmuxRun: developer tool, drives tmux.",
}

// spawnCallRe matches a call that starts a program.
//
// # The shapes it recognises, and the shapes it does not
//
// RECOGNISED, in two groups with deliberately different strictness:
//
//   - <anything>.Command( / <anything>.CommandContext( — alias-tolerant,
//     because cmd/yanshi imports os/exec as osexec and a gate that only knew
//     the canonical spelling would have missed the `gh` site, one of the nine
//     this census was written for. ".Command(" is specific enough to os/exec
//     that the wide left-hand side costs nothing.
//
//   - (syscall|unix|windows|os).(Exec|ForkExec|StartProcess|CreateProcess|
//     CreateProcessAsUser)( — package-qualified rather than alias-tolerant,
//     and that asymmetry is the point. A bare `\w+\.Exec\(` would match every
//     db.Exec and stmt.Exec in internal/store, so the census would demand an
//     entry for a dozen files that spawn nothing; those entries then become
//     dead ones nobody can discharge, and the first response to that is to
//     weaken the gate. Four package names cover every spawn in this tree.
//
// NOT RECOGNISED, stated rather than implied: a spawn reached through a
// function value or an interface method (`f := exec.Command; f(...)`), through
// reflection, through cgo, or through a package aliased away from the four
// names above. None exists here today, and a text scan cannot see any of them —
// that is the same "NOT ENFORCED" boundary the file header states about answer
// truthfulness, applied to the detector rather than to the answers.
//
// This list grew because it was WRONG: the first version knew only ".Command(",
// so internal/shell/console_windows.go — a real CreateProcess site whose unix
// twin WAS in the table — was invisible, along with both syscall.Exec sites.
// The header sentence "a new production file that spawns anything must state
// what the child's environment is" was true of the intent and false of the
// mechanism. TestSpawnCensusRecognisesEveryKnownSpawnShape is what keeps the
// two together from here.
var spawnCallRe = regexp.MustCompile(
	`\b[A-Za-z_][A-Za-z0-9_]*\.Command(Context)?\(` +
		`|\b(syscall|unix|windows|os)\.(Exec|ForkExec|StartProcess|CreateProcess|CreateProcessAsUser)\(`)

// TestEverySpawnSiteDeclaresItsChildEnvironment enumerates the spawn surface in
// both directions.
func TestEverySpawnSiteDeclaresItsChildEnvironment(t *testing.T) {
	root := moduleRoot(t)
	found := map[string][]string{}

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
			sites, parseErr := spawnSitesIn(string(raw))
			if parseErr != nil {
				return fmt.Errorf("parse %s: %w", path, parseErr)
			}
			if len(sites) == 0 {
				return nil
			}
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				return relErr
			}
			found[filepath.ToSlash(rel)] = sites
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}

	var missing, dead, unnamed []string
	for file, sites := range found {
		answer, ok := spawnEnvCensus[file]
		if !ok {
			missing = append(missing, file)
			continue
		}
		for _, decl := range sites {
			if !mentionsIdentifier(answer, decl) {
				unnamed = append(unnamed, file+": "+decl)
			}
		}
	}
	for file := range spawnEnvCensus {
		if len(found[file]) == 0 {
			dead = append(dead, file)
		}
	}
	sort.Strings(missing)
	sort.Strings(dead)
	sort.Strings(unnamed)

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
	if len(unnamed) > 0 {
		t.Errorf("%d spawn site(s) are in a file this census lists, but the entry never names "+
			"the declaration holding them:\n  %s\n\n"+
			"An answer that does not name its subject decays into a pre-authorisation: the "+
			"internal/tools/shell.go row described shell_run long after shell_run stopped "+
			"routing through that file, and the task_gate_run spawn that was actually there "+
			"inherited every credential under cover of it. Name the declaration verbatim in "+
			"the answer, or fix the answer to be about the code that is now there.",
			len(unnamed), strings.Join(unnamed, "\n  "))
	}
}

// spawnSitesIn parses src and returns, sorted and deduplicated, the name of
// every top-level declaration containing a call spawnCallRe recognises.
//
// # Why an AST and not a line scan
//
// The line scan this replaced had to filter comment lines by hand, because five
// of the ".Command(" matches in this repository are prose and a census that
// counted them would demand entries for files that spawn nothing — entries
// nobody could ever discharge, which is how a gate gets weakened. Parsing
// removes that whole class: comments are not in the tree. It also produces the
// thing the SUBJECT check needs, which a line scan cannot: WHICH declaration
// the call is in.
//
// # The two enclosing forms, and why both are handled
//
// A spawn is inside a FuncDecl, or inside a func literal assigned to a
// package-level var. Handling only FuncDecl is a mistake this repository has
// made before in an AST gate, and it is not hypothetical here:
// internal/clipimg/clipimg.go and cmd/testchanged/main.go both put a spawn in a
// package-level `var f = func(...)` seam precisely so tests can swap it. A
// walker that saw only FuncDecl would report those files as having a spawn with
// no owning declaration, and the natural next step is to silently skip them.
//
// A spawn that is in neither form (a var initializer calling exec.Command
// directly, say) is reported under the sentinel name "<file>", so the entry has
// to say that word and the situation stays visible instead of being dropped.
func spawnSitesIn(src string) ([]string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "src.go", src, 0)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, decl := range file.Decls {
		owner := declOwnerName(decl)
		ast.Inspect(decl, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			var rendered strings.Builder
			if printer.Fprint(&rendered, fset, call.Fun) != nil {
				return true
			}
			// The trailing "(" is what spawnCallRe anchors on, and Fun is the
			// callee without it.
			if spawnCallRe.MatchString(rendered.String() + "(") {
				seen[owner] = true
			}
			return true
		})
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// declOwnerName is the name an answer has to mention for a spawn inside decl.
func declOwnerName(decl ast.Decl) string {
	switch d := decl.(type) {
	case *ast.FuncDecl:
		return d.Name.Name
	case *ast.GenDecl:
		if d.Tok != token.VAR {
			break
		}
		for _, spec := range d.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || len(vs.Names) == 0 {
				continue
			}
			return vs.Names[0].Name
		}
	}
	return "<file>"
}

// mentionsIdentifier reports whether answer names ident as a word.
//
// Word-bounded rather than substring so "Start" in an answer is the method
// Start and not the tail of "StartPTYProcess" — the point of the check is that
// renaming the declaration breaks the row, and a substring match would survive
// exactly the renames that matter.
func mentionsIdentifier(answer, ident string) bool {
	re := regexp.MustCompile(`(^|[^\w])` + regexp.QuoteMeta(ident) + `($|[^\w])`)
	return re.MatchString(answer)
}

// TestSpawnCensusRecognisesEveryKnownSpawnShape drives spawnSitesIn directly,
// so the detector's reach is pinned independently of what happens to be in the
// tree today, and so is the declaration name it attributes each call to.
//
// The census walk cannot do this job on its own: it only ever reports the
// shapes it already sees, so a shape it is blind to produces a GREEN run that
// is indistinguishable from "there is no such call here". That is exactly how
// internal/shell/console_windows.go stayed out of the table while its unix twin
// sat in it — the same defect, three times now in this repository, of an AST or
// text gate whose recognised forms are narrower than its stated purpose.
//
// The negative cases are load-bearing in the other direction. `db.Exec(` is the
// reason the syscall group is package-qualified instead of alias-tolerant:
// internal/store is full of them, and a census that demanded an entry for every
// file holding one would be weakened back out within a release.
func TestSpawnCensusRecognisesEveryKnownSpawnShape(t *testing.T) {
	// wantOwner is the declaration the call must be attributed to, which is the
	// half the SUBJECT direction of the census depends on.
	recognised := []struct{ name, src, wantOwner string }{
		{"exec.Command", "func spawn() { cmd := exec.Command(\"true\"); _ = cmd }", "spawn"},
		{"exec.CommandContext", "func spawn(ctx C) { _ = exec.CommandContext(ctx, \"true\") }", "spawn"},
		{"aliased os/exec", "func spawn() { _ = osexec.Command(\"true\") }", "spawn"},
		{"syscall.Exec", "func spawn() error { return syscall.Exec(p, argv, env) }", "spawn"},
		{"syscall.ForkExec", "func spawn() { _, _ = syscall.ForkExec(p, argv, attr) }", "spawn"},
		{"os.StartProcess", "func spawn() { _, _ = os.StartProcess(n, argv, attr) }", "spawn"},
		{"windows.CreateProcess", "func spawn() { _ = windows.CreateProcess(nil, c, nil, nil, false, 0, e, d, s, p) }", "spawn"},
		{"unix.Exec", "func spawn() error { return unix.Exec(p, argv, env) }", "spawn"},
		{"method receiver", "func (f *F) Start() { _ = exec.Command(\"true\") }", "Start"},
		{"package-level var seam", "var commandOutput = func() { _ = exec.CommandContext(ctx, \"x\") }", "commandOutput"},
		{"nested in a closure", "func spawn() { go func() { _ = exec.Command(\"true\") }() }", "spawn"},
	}
	for _, tc := range recognised {
		got, err := spawnSitesIn("package p\n\n" + tc.src + "\n")
		if err != nil {
			t.Errorf("shape %q did not parse: %v", tc.name, err)
			continue
		}
		if len(got) == 0 {
			t.Errorf("spawn shape %q is not recognised; a production file using it would "+
				"be waved through with no entry in spawnEnvCensus:\n  %s", tc.name, tc.src)
			continue
		}
		if got[0] != tc.wantOwner {
			t.Errorf("shape %q was attributed to %q, want %q — the census entry would have to "+
				"name a declaration that is not the one holding the call", tc.name, got[0], tc.wantOwner)
		}
	}

	ignored := map[string]string{
		"database Exec":           "func q() { _, _ = db.Exec(\"delete from t\") }",
		"prepared statement Exec": "func q() { _, _ = stmt.Exec(id) }",
		"transaction ExecContext": "func q() { _, _ = tx.ExecContext(ctx, s) }",
		"a comment mentioning it": "// exec.Command is what this used to do.\nfunc q() {}",
		"a doc line":              "// syscall.Exec replaces the process image.\nfunc q() {}",
		"a string literal":        "const s = \"os.StartProcess(\"",
		"unrelated identifier":    "func q() { _ = buildCommand(name) }",
	}
	for name, src := range ignored {
		got, err := spawnSitesIn("package p\n\n" + src + "\n")
		if err != nil {
			t.Errorf("negative case %q did not parse: %v", name, err)
			continue
		}
		if len(got) > 0 {
			t.Errorf("%q is treated as a spawn site (attributed to %v); the census would demand "+
				"an entry for a file that starts no process, and that entry can never be "+
				"discharged:\n  %s", name, got, src)
		}
	}
}
