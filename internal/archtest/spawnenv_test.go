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
// NOT ENFORCED, in three separate directions. All three are stated because the
// sentence "adding a spawn site costs a sentence somebody has to write" is true
// of a NEW FILE and false of the other two cases, and a reader who takes it as
// universal is the reader this census was written to protect.
//
//  1. WHETHER THE ANSWER IS TRUE. This is a text scan over an AST, not a taint
//     analysis; it can tell you the answer is about the right function, it
//     cannot tell "cmd.Env = netpolicy.ScrubbedEnviron()" from a sentence
//     saying so. Do not quote this file as proof that a child is scrubbed;
//     quote the child test in internal/netpolicy for the mechanism and this
//     table for the coverage.
//
//  2. GRANULARITY: per-DECLARATION, not per-CALL. spawnSitesIn returns a
//     DEDUPLICATED set of declaration names, so a second spawn added inside a
//     declaration this table already names costs nothing at all. That is B1's
//     own shape one level down — shellCommand, startOne and Run are precisely
//     the functions most likely to grow a second spawn — and it is measured,
//     not theoretical: a second exec.CommandContext with no cmd.Env, added
//     inside shellCommand, leaves this gate green.
//
//     Deliberately not fixed, and the cost is the reason. Per-call enforcement
//     requires the entry to carry a NUMBER of spawn calls per declaration, and
//     that is exactly the shape CLAUDE.md forbids under "don't write how many
//     things another file currently has": the number describes a file this
//     table does not live in, it rots on that file's next refactor, and the
//     cheapest way to discharge a rotted number is to bump it. The defence
//     against a second leaky spawn is the netpolicy child tests; this table's
//     job is coverage bookkeeping.
//
//  3. PHANTOM NAMES IN AN ANSWER. The SUBJECT direction checks "every real
//     declaration is named", not "every name is a real declaration", so
//     "shellCommand and totallyBogusNeverExisted set …" passes. The names in
//     an answer are therefore not evidence that those declarations exist.
//
//     Deliberately not fixed, and here the cost is the interesting half: the
//     fix is to give each entry a structure (a []string of declarations plus a
//     prose note) and check set equality. The check would then bind to the
//     LIST while the prose floated free again — which is exactly the
//     decoupling B1 WAS: an answer describing shell_run's route while the code
//     under it was task_gate_run's. Names woven into the sentence are what
//     makes the reader of the sentence read the name.
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
	// Construction, not a spawn: prepare builds a stand-in *exec.Cmd for the
	// sandbox seam and never starts it. The entry exists because the detector
	// recognises the CONSTRUCTION (see spawnCallRe's doc for why it cannot
	// anchor on .Start()), and because "this Cmd is never started" is the
	// answer a reader needs — adding one line here would otherwise turn a file
	// with no census obligation into a real spawn point.
	"internal/shell/childlaunch.go": "prepare constructs an &exec.Cmd{} whose Env is " +
		"spec.Env — what childLaunchPosture.env already built — hands it to Sandbox.Prepare " +
		"so a backend can rewrite argv or add to the environment, copies the mutations back " +
		"into the LaunchSpec, and NEVER calls Start. The spawn happens in the OS factory " +
		"(internal/shell/process.go).",
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
	"internal/shell/snapshot.go": "CaptureSnapshot: cmd.Env = os.Environ(), INHERITS " +
		"deliberately. The child is the operator's own login shell reading the operator's " +
		"own startup files, and a scrubbed HOME/USER would make it read a different user's " +
		"files or none — the capture would then describe an environment nobody has. What it " +
		"prints is scrubbed downstream instead: Snapshot.Apply layers it into the base that " +
		"childLaunchPosture.env then runs ScrubCredentials over, so an rc file exporting an " +
		"API key is stripped on exactly the path that strips yanshi's own.",
	"internal/clipimg/clipimg.go": "commandOutput: netpolicy.ScrubbedEnviron() in that single " +
		"package-level seam, so all four platform backends are covered at once.",
	"internal/tools/screenshot.go": "captureCommand sets netpolicy.ScrubbedEnviron(). The " +
		"three platform files build their argv and hand it here, so the darwin, linux and " +
		"windows backends cannot disagree about the child's environment.",
	"cmd/yanshi/pr.go": "realGHExec: netpolicy.ScrubbedEnviron(netpolicy.GitHubCLICredentialEnv...) " +
		"for gh, which authenticates with GH_TOKEN, GH_ENTERPRISE_TOKEN or not at all. " +
		"detectGitHubRemote: netpolicy.ScrubbedEnviron() with no allowlist — `git remote " +
		"get-url` is a local read of .git/config and needs no credential at all. It used to " +
		"inherit, on the argument that a local read has no network leg; that argument is about " +
		"where the credentials could GO, not about whether the child receives them, and the " +
		"same file scrubbing one spawn and not the other is the asymmetry that makes the next " +
		"reader guess.",
	"internal/agent/goalloop/implementer.go": "gitWorktreeCommand: netpolicy.ScrubbedEnviron(). " +
		"Local worktree add/remove/list, no network leg.",
	"internal/cli/tui/editor.go": "buildEditorCmd (called by startExternalEditor) INHERITS, " +
		"deliberately. Ctrl+E hands the terminal to the operator's own $VISUAL/$EDITOR/vi, " +
		"synchronously and in the foreground — the same login-shell posture " +
		"internal/shell/snapshot.go documents. Nothing it reads leaves the machine on its " +
		"own, and a scrubbed PATH/HOME would break the operator's own editor configuration " +
		"for a purely local edit.",
	"internal/cli/tui/startup.go": "runInDir: INHERITS, deliberately. Runs `git diff --shortstat` " +
		"and `gh pr view` in the project directory (W-E-09 branch summary). Both commands are " +
		"purely local reads that summarise the working tree — no credentials leave the machine. " +
		"Scrubbing PATH would break git and gh on installations that rely on shell profile vars. " +
		"Callers: runInDir (called by fetchGitStatus).",
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
	"internal/sandbox/restrictedtoken_windows.go": "runUnderToken builds the child's Env " +
		"explicitly from five variables (SystemRoot, windir, ComSpec, TEMP, TMP) and is " +
		"reached only from the construction-time self-check. Production spawns are not " +
		"started here at all: Prepare attaches a token to a command the shell factory " +
		"already built, so that factory owns the environment.",

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
// A third shape is recognised in the AST rather than here, because a regexp
// cannot separate it from a map type: see execCmdLiteral for `&exec.Cmd{…}`.
//
// # KNOWN BLIND SPOTS — a list of what somebody thought of, NOT an enumeration
//
// The shapes below are pinned by the `unrecognised` table in
// TestSpawnCensusRecognisesEveryKnownSpawnShape, so if the detector ever grows
// to catch one, that test says so and this paragraph gets corrected. What no
// test can pin is the shape nobody has thought of yet, and THAT is the thing to
// carry away from this comment:
//
//	spawn through a function value or an interface method (`f := exec.Command;
//	f(...)`); through reflection; through cgo; through a raw
//	syscall.Syscall(SYS_EXECVE, …); through syscall.NewLazyDLL("kernel32").
//	NewProc("CreateProcessW").Call(…); or through a package aliased away from
//	the four names in the second group.
//
// # Why this list keeps being wrong, four times now
//
// Every previous version of this comment was written as an enumeration and was
// not one, and the mechanism was identical each time: the author listed the
// shapes THEY could think of, found no counter-example in the tree, and wrote
// "none exists here today" — which reads as a property of the repository while
// being a property of the author's imagination. The four:
//
//  1. The AST gate that walked only *ast.FuncDecl, so every spawn in a
//     package-level `var f = func(){…}` seam had no owning declaration.
//  2. destructivecensus_test.go's findDestructiveReadingSites, which saw only
//     composite literals carrying an EXPLICIT type — a literal whose type is
//     inferred from context has a nil CompositeLit.Type and was invisible, as
//     was every field assigned after construction.
//  3. This detector's first version, which knew only ".Command(" — so
//     internal/shell/console_windows.go, a real CreateProcess site whose unix
//     twin WAS in the table, was invisible along with both syscall.Exec sites.
//  4. This one: `&exec.Cmd{…}` + cmd.Start(), the SECOND idiomatic way to
//     start a process in Go and the one the standard library's own docs show
//     for SysProcAttr, was missing while the paragraph above it said "stated
//     rather than implied". A live instance was already in the tree
//     (internal/shell/childlaunch.go) and the file was not in the census.
//
// The lesson is not "add a fifth entry". It is that a blind-spot list can only
// ever be an inventory of considered shapes, so it must be LABELLED as one —
// a reader who treats it as exhaustive stops looking, and the first three
// misses were all found by somebody who kept looking anyway.
var spawnCallRe = regexp.MustCompile(
	`\b[A-Za-z_][A-Za-z0-9_]*\.Command(Context)?\(` +
		`|\b(syscall|unix|windows|os)\.(Exec|ForkExec|StartProcess|CreateProcess|CreateProcessAsUser)\(`)

// execCmdLiteral reports whether lit is a `pkg.Cmd{…}` composite literal — the
// os/exec form that builds the command struct directly instead of through
// exec.Command.
//
// # Why construction and not the Start() that follows it
//
// The thing that actually spawns is cmd.Start()/Run()/Output(), and anchoring
// there is not an option: ".Start(" and ".Run(" name a method on half the types
// in this repository (every Manager, every server, every session), so the
// census would demand entries for dozens of files that start no process — and
// entries nobody can discharge are how a gate gets weakened back out. The
// construction is the only text-visible anchor that is specific to os/exec.
//
// The cost of anchoring there is over-approximation in the safe direction: a
// Cmd that is built and never started still demands a row. Exactly one exists
// (internal/shell/childlaunch.go's sandbox stand-in), its row says so in one
// sentence, and that row is what makes ADDING a Start() to it a change to an
// answer rather than an invisible new spawn point.
//
// # Why the package qualifier is not checked
//
// `X.Cmd{}` for any X, matching the alias-tolerance of ".Command(" — cmd/yanshi
// imports os/exec as osexec, and a detector that only knew the canonical
// spelling is miss #3 in spawnCallRe's list. The over-match (some other
// package's type named Cmd) costs one honest sentence in a row; the under-match
// costs a silent credential leak.
//
// A type expression is required, which is what keeps `map[string]*exec.Cmd{}`
// out: that literal's Type is an *ast.MapType, and its inner *exec.Cmd never
// appears as a composite literal at all. internal/lsp/manager.go holds one, and
// a regexp for `\bexec\.Cmd\{` would have matched it.
func execCmdLiteral(lit *ast.CompositeLit) bool {
	sel, ok := lit.Type.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	if _, ok := sel.X.(*ast.Ident); !ok {
		return false
	}
	return sel.Sel.Name == "Cmd"
}

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
// every top-level declaration containing a call spawnCallRe recognises or a
// command struct execCmdLiteral recognises.
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
			switch node := n.(type) {
			case *ast.CallExpr:
				var rendered strings.Builder
				if printer.Fprint(&rendered, fset, node.Fun) != nil {
					return true
				}
				// The trailing "(" is what spawnCallRe anchors on, and Fun is
				// the callee without it.
				if spawnCallRe.MatchString(rendered.String() + "(") {
					seen[owner] = true
				}
			case *ast.CompositeLit:
				if execCmdLiteral(node) {
					seen[owner] = true
				}
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
		{"&exec.Cmd composite literal", "func spawn() error { c := &exec.Cmd{Path: p, Args: argv}; return c.Start() }", "spawn"},
		{"exec.Cmd value literal", "func spawn() { c := exec.Cmd{Path: p}; _ = c.Run() }", "spawn"},
		{"aliased exec.Cmd literal", "func spawn() { _ = &osexec.Cmd{Path: p} }", "spawn"},
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
		// The reason execCmdLiteral checks the AST type instead of matching
		// `\bexec\.Cmd\{` textually: internal/lsp/manager.go holds this exact
		// line, and demanding a census row for a map declaration is how a gate
		// earns its first unanswerable entry.
		"a map of Cmd pointers": "func q() { m := map[string]*exec.Cmd{}; _ = m }",
		"a slice of Cmd":        "func q() { s := []exec.Cmd{}; _ = s }",
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

	// unrecognised pins the KNOWN blind spots spawnCallRe's doc names, so that
	// paragraph stops being an unverifiable claim. It is the honest half of a
	// dishonest history: four times running, this repository wrote a shape list
	// as if it were an enumeration and it was an inventory of what its author
	// happened to think of.
	//
	// What this table CAN do: catch the doc going stale in the direction where
	// the detector improves. What it deliberately does NOT do: assert that the
	// blind-spot list is complete. It cannot — a shape nobody has imagined has
	// no row here, which is exactly how all four misses happened.
	unrecognised := []struct{ name, src string }{
		{"function value", "var spawner = exec.Command\n\nfunc spawn() { _ = spawner(\"true\") }"},
		{"interface method", "func spawn(r Runner) error { return r.Run(p, argv) }"},
		{"reflection", "func spawn() { reflect.ValueOf(exec.Command).Call(args) }"},
		{"raw syscall number", "func spawn() { _, _, _ = syscall.Syscall(syscall.SYS_EXECVE, a, b, c) }"},
		{"lazy DLL proc", "func spawn() { syscall.NewLazyDLL(\"kernel32\").NewProc(\"CreateProcessW\").Call(a) }"},
		{"aliased syscall package", "func spawn() error { return sys.Exec(p, argv, env) }"},
	}
	for _, tc := range unrecognised {
		got, err := spawnSitesIn("package p\n\n" + tc.src + "\n")
		if err != nil {
			t.Errorf("blind-spot case %q did not parse: %v", tc.name, err)
			continue
		}
		if len(got) > 0 {
			t.Errorf("blind spot %q is now RECOGNISED (attributed to %v). That is an "+
				"improvement, not a regression — but spawnCallRe's doc still lists it as a "+
				"shape this detector cannot see, and a stale blind-spot list is the exact "+
				"defect that paragraph exists to record. Move the row to the recognised "+
				"table above and delete it from the doc:\n  %s", tc.name, got, tc.src)
		}
	}
}
