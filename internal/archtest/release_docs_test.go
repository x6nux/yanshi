package archtest

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExamplesAreRunnableAndExercised pins EX1's three clauses against the
// directory and the workflow that runs it.
//
// Counting examples is not enough — an example nobody executes rots exactly
// like documentation nobody reads, and this repo had one:
// examples/goalloop-config/run.sh shipped its own assertions and was in no CI
// step at all. So the check is coverage of the EXECUTION side too.
//
// ledger: H2/EX1#2 可跑
//
// ledger: H2/EX1#1 ≥5 个示例
func TestExamplesAreRunnableAndExercised(t *testing.T) {
	entries, err := os.ReadDir(abs("examples"))
	if err != nil {
		t.Fatalf("read examples/: %v", err)
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	if len(dirs) < 5 {
		t.Errorf("examples/ has %d example(s), the acceptance asks for at least 5: %v",
			len(dirs), dirs)
	}

	// "Exercised" means a CI step runs it, or the named Go test drives it.
	//
	// The Go-test side is an explicit table rather than a scan for
	// `examples/<dir>` across every _test.go: a scan counts a test that merely
	// MENTIONS the path in a comment, which is the loose direction — an
	// example could go uncovered while looking covered. Each row here names
	// the test that actually loads it, and TestExampleCoverageTestsExist below
	// checks those tests are real.
	byGoTest := map[string]string{
		"custom-skill": "internal/skills::TestShippedExampleSkillLoads",
	}
	docs := readWorkflow(t, "docs.yml")
	var unexercised []string
	for _, d := range dirs {
		if strings.Contains(docs, "examples/"+d) || byGoTest[d] != "" {
			continue
		}
		unexercised = append(unexercised, d)
	}
	if len(unexercised) > 0 {
		t.Errorf("no CI step touches %d example(s): %s\n"+
			"  an example nobody runs rots exactly like documentation nobody reads — "+
			"examples/goalloop-config carried its own assertions and was in no step "+
			"at all until W10", len(unexercised), strings.Join(unexercised, ", "))
	}
}

// TestExamplesCoverTheMainIntegrationPoints is EX1's third clause: the set has
// to span the surfaces, not just be numerous.
//
// ledger: H2/EX1#3 覆盖主要集成点
func TestExamplesCoverTheMainIntegrationPoints(t *testing.T) {
	want := map[string]string{
		"custom-tool":     "extending the tool registry",
		"custom-skill":    "the SKILL.md skill system",
		"headless-exec":   "the headless single-prompt runner",
		"headless-batch":  "batch input modes",
		"goalloop-config": "the self-driven goal loop",
		"sdk-python":      "the Python client",
		"sdk-typescript":  "the TypeScript client",
	}
	for dir, what := range want {
		if _, err := os.Stat(abs(filepath.Join("examples", dir))); err != nil {
			t.Errorf("examples/%s is missing; nothing demonstrates %s", dir, what)
		}
	}
}

// TestContributingDocumentsTheEnforcedGates is CONTRIB1's "约定可执行" clause.
//
// The file was 66 lines of genuinely load-bearing conventions and mentioned
// internal/archtest, cmd/codelines, the exemption tables and cmd/featurestatus
// exactly zero times — while W0 had just made `governance` a hard gate. It
// also never said that changing config / -h text / the schema requires
// re-running the generators. A contributor following it landed on two red jobs
// with no idea why.
//
// ledger: H2/CONTRIB1#2 约定可执行
//
// ledger: H1/VER1#3 发布流程文档化
//
// ledger: H2/CONTRIB1#1 CONTRIBUTING 存在
func TestContributingDocumentsTheEnforcedGates(t *testing.T) {
	data, err := os.ReadFile(abs("CONTRIBUTING.md"))
	if err != nil {
		t.Fatalf("read CONTRIBUTING.md: %v", err)
	}
	doc := string(data)
	for _, want := range []string{
		"internal/archtest", "cmd/codelines", "cmd/covercheck", "cmd/featurestatus",
		"cmd/gendocs", "cmd/api-schema", "GOV1", "GOV9",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("CONTRIBUTING.md never mentions %s; a contributor hits that gate "+
				"red without being told it exists", want)
		}
	}
}

// TestReadmeReachesEveryEntryPoint is UDOC1's "覆盖主要用法" clause at the
// front door.
//
// Measured before W10: the README's reference count for docs/user-guide,
// docs/api, docs/adr, examples/ and CONTRIBUTING.md was zero, all five. A
// stranger arriving at the open-source repo could not find the user guide, the
// contribution guide, or a single example from the page they land on.
//
// ledger: H2/UDOC1#1 覆盖主要用法
func TestReadmeReachesEveryEntryPoint(t *testing.T) {
	data, err := os.ReadFile(abs("README.md"))
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	doc := string(data)
	for _, want := range []string{
		"docs/user-guide", "docs/api", "docs/adr", "examples/", "CONTRIBUTING.md",
		"LICENSE",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("README.md does not link %s", want)
		}
	}
}

// TestLicenseShipsWithTheBinary is PKG1's "release 产物完整" clause for the one
// file whose absence is a licensing problem rather than a packaging one.
//
// The repo had no LICENSE at all until W10, and .goreleaser.yaml's
// archives.files listed only config.example.yaml and README.md — so adding the
// file without adding the row would have shipped every downloaded copy without
// the notice MIT requires to travel with it.
//
// ledger: H1/PKG1#2 release 产物完整
func TestLicenseShipsWithTheBinary(t *testing.T) {
	if _, err := os.Stat(abs("LICENSE")); err != nil {
		t.Fatalf("the repository has no LICENSE: %v", err)
	}
	data, err := os.ReadFile(abs(".goreleaser.yaml"))
	if err != nil {
		t.Fatalf("read .goreleaser.yaml: %v", err)
	}
	files, _, found := strings.Cut(withoutComments(string(data), "#"), "checksum:")
	if !found {
		t.Fatal(".goreleaser.yaml has no checksum block; the file shape changed")
	}
	// Comments stripped first. The row is accompanied by a comment saying why
	// LICENSE ships, and the first version of this check matched THAT — delete
	// the actual entry and it stayed green. Same shape as W7's bench gate and
	// W10's cliff parser check: a gate reading its own explanation as config.
	if !strings.Contains(files, "LICENSE") {
		t.Error("archives.files does not include LICENSE: downloaded copies would " +
			"ship without the notice")
	}
}

// TestSecurityContactIsNotAPlaceholder is CONTRIB1's docs-hygiene half.
//
// SECURITY.md carried `security@x6nux.dev *(placeholder — replace before going
// public)*`. The file said, in its own words, that it was not ready to be
// public — and it was about to be.
//
// ledger: H2/CONTRIB1#3 docs 结构清晰
func TestSecurityContactIsNotAPlaceholder(t *testing.T) {
	data, err := os.ReadFile(abs("SECURITY.md"))
	if err != nil {
		t.Fatalf("read SECURITY.md: %v", err)
	}
	if strings.Contains(strings.ToLower(string(data)), "placeholder") {
		t.Error("SECURITY.md still self-describes a contact as a placeholder")
	}
}

// TestExampleCoverageTestsExist keeps the byGoTest table above honest.
//
// A row there exempts an example from the CI-step requirement, so a row
// naming a test that does not exist would be a silent hole: the example looks
// covered and nothing runs it.
func TestExampleCoverageTestsExist(t *testing.T) {
	for example, ref := range map[string]string{
		"custom-skill": "internal/skills::TestShippedExampleSkillLoads",
	} {
		pkg, fn, ok := strings.Cut(ref, "::")
		if !ok {
			t.Errorf("malformed reference %q for examples/%s", ref, example)
			continue
		}
		found := false
		err := filepath.WalkDir(abs(pkg), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, "_test.go") {
				return nil
			}
			data, readErr := os.ReadFile(path)
			if readErr == nil && strings.Contains(string(data), "func "+fn+"(t *testing.T)") {
				found = true
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", pkg, err)
		}
		if !found {
			t.Errorf("examples/%s is exempted from the CI-step requirement by %s, "+
				"which does not exist", example, ref)
		}
	}
}

// TestCoverageFloorsAreEnforcedInCI pins the coverage gate's existence.
//
// cmd/covercheck is a tool; a tool nobody runs is the "built but never
// assembled" shape this repo keeps producing. Before W10 there was no coverage
// gate anywhere — `rg -- '-cover' .github/workflows/` returned nothing — so
// any of these packages could have gone from 94% to 20% without a red job.
//
// ledger: E1/COV2#1 覆盖率 ≥80%
// ledger: E1/COV3#1 覆盖率 ≥50%
func TestCoverageFloorsAreEnforcedInCI(t *testing.T) {
	body, ok := workflowJobBody(readWorkflow(t, "ci.yml"), "coverage")
	if !ok {
		t.Fatal("ci.yml has no coverage job: cmd/covercheck exists but nothing runs it")
	}
	if !strings.Contains(body, "cmd/covercheck") {
		t.Error("the coverage job does not run cmd/covercheck")
	}
	if jobLevelContinueOnError(body) {
		t.Error("the coverage job is continue-on-error and cannot go red")
	}

	// The floors themselves must exist for each package the ledger claims.
	src, err := os.ReadFile(abs(filepath.Join("cmd", "covercheck", "main.go")))
	if err != nil {
		t.Fatalf("read cmd/covercheck: %v", err)
	}
	for _, want := range []string{"internal/proto", "internal/store", "internal/bootstrap"} {
		if !strings.Contains(withoutComments(string(src), "//"), want) {
			t.Errorf("cmd/covercheck has no floor for %s", want)
		}
	}
}

// TestBubbleteaForkIsPinned is PKG1's "fork 行为保留" clause.
//
// The Windows Ctrl+Enter vs Enter distinction the TUI's send/newline binding
// depends on lives in third_party/bubbletea; upstream collapses VK_RETURN to
// KeyEnter regardless of modifiers. Dropping the replace directive would build
// fine and silently merge the two keys.
//
// ledger: H1/PKG1#4 fork 行为保留
func TestBubbleteaForkIsPinned(t *testing.T) {
	mod, err := os.ReadFile(abs("go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	if !strings.Contains(string(mod), "=> ./third_party/bubbletea") {
		t.Error("go.mod no longer replaces bubbletea with the local fork: upstream " +
			"collapses VK_RETURN to KeyEnter regardless of modifiers, so Enter=send / " +
			"Ctrl+Enter=newline would silently become one key on Windows")
	}
	if _, err := os.Stat(abs(filepath.Join("third_party", "bubbletea", "go.mod"))); err != nil {
		t.Errorf("third_party/bubbletea is gone but go.mod still points at it: %v", err)
	}
}

// TestGettingStartedRunsInCI is UDOC1's "getting started 可零依赖跑通" clause.
//
// The ledger cited cmd/gendocs's config-skeleton test for this. That test
// reconciles the skeleton with config.example.yaml's top-level keys — a real
// check, of an unrelated fact. Nothing said the getting-started path itself
// still works.
//
// What makes the claim checkable is that the guide's zero-dependency steps are
// literally what CI runs. The alt-screen TUI step is the one exception and
// says so: a Bubble Tea program cannot be driven through a pipe, which is why
// CLAUDE.md points at `yanshi -h` for a startup smoke instead.
//
// ledger: H2/UDOC1#2 getting started 可零依赖跑通
func TestGettingStartedRunsInCI(t *testing.T) {
	guide, err := os.ReadFile(abs(filepath.Join("docs", "user-guide", "getting-started.md")))
	if err != nil {
		t.Fatalf("read getting-started.md: %v", err)
	}
	text := string(guide)
	for _, step := range []string{
		"go build -o yanshi ./cmd/yanshi",
		`exec --fake-model -p "hello"`,
	} {
		if !strings.Contains(text, step) {
			t.Errorf("getting-started.md no longer shows %q; this test pins the wrong "+
				"steps and would keep passing on a guide that changed underneath it", step)
		}
	}

	ci := readWorkflow(t, "ci.yml") + readWorkflow(t, "docs.yml")
	if !strings.Contains(ci, "go build") {
		t.Error("no workflow builds the binary the guide's first step builds")
	}
	// The one command a new user runs to see output.
	if !strings.Contains(ci, `exec --fake-model -p "hi"`) &&
		!strings.Contains(ci, `exec --fake-model -p "hello"`) {
		t.Error("no workflow runs the guide's `yanshi exec --fake-model -p` step; " +
			"the zero-dependency path is documented and unverified")
	}
}
