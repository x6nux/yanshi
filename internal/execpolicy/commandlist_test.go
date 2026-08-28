package execpolicy_test

import (
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/execpolicy"
)

// TestParseCommandListSplitsOnEveryControlOperator pins the four operators the
// guard now judges segment by segment. `;` is the one that has no Lex support
// at all, so it is the one a regression would most plausibly drop.
func TestParseCommandListSplitsOnEveryControlOperator(t *testing.T) {
	cases := []struct {
		raw   string
		texts []string
		ops   []execpolicy.TokenKind
	}{
		{"git status && go test", []string{"git status", "go test"},
			[]execpolicy.TokenKind{execpolicy.AndIf, execpolicy.NoOperator}},
		{"git status || go test", []string{"git status", "go test"},
			[]execpolicy.TokenKind{execpolicy.OrIf, execpolicy.NoOperator}},
		{"git status ; go test", []string{"git status", "go test"},
			[]execpolicy.TokenKind{execpolicy.Semi, execpolicy.NoOperator}},
		{"cat f | grep x", []string{"cat f", "grep x"},
			[]execpolicy.TokenKind{execpolicy.Pipe, execpolicy.NoOperator}},
		{"a && b ; c | d", []string{"a", "b", "c", "d"},
			[]execpolicy.TokenKind{execpolicy.AndIf, execpolicy.Semi, execpolicy.Pipe, execpolicy.NoOperator}},
		{"ls;", []string{"ls"}, []execpolicy.TokenKind{execpolicy.Semi}},
	}
	for _, tc := range cases {
		segs, err := execpolicy.ParseCommandList(tc.raw)
		if err != nil {
			t.Fatalf("ParseCommandList(%q): %v", tc.raw, err)
		}
		if len(segs) != len(tc.texts) {
			t.Fatalf("ParseCommandList(%q): got %d segments, want %d", tc.raw, len(segs), len(tc.texts))
		}
		for i, seg := range segs {
			if seg.Text != tc.texts[i] {
				t.Errorf("ParseCommandList(%q) segment %d Text = %q, want %q", tc.raw, i, seg.Text, tc.texts[i])
			}
			if seg.Operator != tc.ops[i] {
				t.Errorf("ParseCommandList(%q) segment %d Operator = %v, want %v", tc.raw, i, seg.Operator, tc.ops[i])
			}
		}
	}
}

// TestParseCommandListTextIsAVerbatimSlice is the load-bearing half of the
// guard's behaviour compatibility: an unchained command must reach the profile
// globs byte-identical to what the caller passed, quoting and inner spacing
// included. A re-joined Program+Args would silently rewrite it.
func TestParseCommandListTextIsAVerbatimSlice(t *testing.T) {
	for _, raw := range []string{
		`rm -rf "/my dir"`,
		`git commit -m 'two  spaces'`,
		`go  test   ./...`,
		`  ls -la  `,
	} {
		segs, err := execpolicy.ParseCommandList(raw)
		if err != nil {
			t.Fatalf("ParseCommandList(%q): %v", raw, err)
		}
		if len(segs) != 1 {
			t.Fatalf("ParseCommandList(%q): got %d segments, want 1", raw, len(segs))
		}
		if want := strings.TrimSpace(raw); segs[0].Text != want {
			t.Errorf("ParseCommandList(%q).Text = %q, want %q", raw, segs[0].Text, want)
		}
	}
}

// TestParseCommandListRejectsUnjudgeableStructure enumerates the forms that
// stay a structural HardDeny after INF1. Each one hides the text that actually
// runs (or the line it runs on) somewhere this string cannot show, so accepting
// it would mean the policy layer judged something other than what executes.
func TestParseCommandListRejectsUnjudgeableStructure(t *testing.T) {
	for _, raw := range []string{
		"ls $(whoami)",
		"ls `whoami`",
		"diff <(a) <(b)",
		"tee >(cat)",
		"(rm -rf /)",
		"rm -rf / )",
		"cat <<EOF",
		"cat <<<word",
		"sleep 5 &",
		"ls\nrm -rf /",
		"ls\rrm -rf /",
		`echo "unterminated`,
		`echo 'unterminated`,
		`echo trailing\`,
		"; ls",
		"ls ;; rm",
		"cat >",
		"> out.txt",
	} {
		if segs, err := execpolicy.ParseCommandList(raw); err == nil {
			t.Errorf("ParseCommandList(%q) = %+v, want error", raw, segs)
		}
	}
}

// TestParseCommandListKeepsGlobsAndVariablesParseable is the reason this is a
// second front-end rather than a reuse of Lex. The default shipped profile is a
// glob allowlist under which `ls *.go` has always been an ordinary command;
// routing the split through Lex (which rejects `*`, `?`, `[`, `$VAR`) would
// turn every one of these into a structural HardDeny.
func TestParseCommandListKeepsGlobsAndVariablesParseable(t *testing.T) {
	for _, raw := range []string{
		"ls *.go",
		"grep -r foo src/**",
		"echo $HOME",
		"ls [abc]*",
		"rm -rf %USERPROFILE%",
		`dir C:\Users`,
	} {
		if _, err := execpolicy.ParseCommandList(raw); err != nil {
			t.Errorf("ParseCommandList(%q) = %v, want no error", raw, err)
		}
	}
}

// TestParseCommandListQuotedOperatorsAreData: an operator inside quotes is an
// argument, not a segment boundary. Getting this wrong in the permissive
// direction would let `grep "a|b"` be judged as a `grep "a` segment piped into
// a `b"` one, and every downstream verdict would be about commands nobody ran.
func TestParseCommandListQuotedOperatorsAreData(t *testing.T) {
	for _, raw := range []string{`grep "a|b" x`, `grep 'a && b' x`, `echo "a;b"`} {
		segs, err := execpolicy.ParseCommandList(raw)
		if err != nil {
			t.Fatalf("ParseCommandList(%q): %v", raw, err)
		}
		if len(segs) != 1 {
			t.Fatalf("ParseCommandList(%q): got %d segments, want 1", raw, len(segs))
		}
	}
}

// TestParseCommandListExtractsRedirectTargets pins INF1's third load-bearing
// constraint: `echo x > ~/.ssh/authorized_keys` has program `echo`, so a policy
// that only reads the program has not read the dangerous half.
func TestParseCommandListExtractsRedirectTargets(t *testing.T) {
	cases := []struct {
		raw    string
		ops    []string
		target []string
	}{
		{"echo x > out.txt", []string{">"}, []string{"out.txt"}},
		{"echo x >> out.txt", []string{">>"}, []string{"out.txt"}},
		{"cat < in.txt", []string{"<"}, []string{"in.txt"}},
		{"echo x > ~/.ssh/authorized_keys", []string{">"}, []string{"~/.ssh/authorized_keys"}},
		{"go build 2> err.log", []string{"2>"}, []string{"err.log"}},
		{"go build 2>err.log", []string{"2>"}, []string{"err.log"}},
		{"go build &> all.log", []string{"&>"}, []string{"all.log"}},
		{"go build > o.txt 2>&1", []string{">", "2>&1"}, []string{"o.txt", ""}},
		// `>&word` with a NON-NUMERIC word writes the file, on bash, sh and
		// zsh alike. Reading it as a descriptor duplication left the target
		// empty and the write invisible to the FS dimension; measured, that
		// planted a key in ~/.ssh/authorized_keys with no prompt while the `>`
		// spelling of the same command was refused.
		{"echo x >& out.txt", []string{">&"}, []string{"out.txt"}},
		{"echo x >&out.txt", []string{">&"}, []string{"out.txt"}},
		{"echo x >& ~/.ssh/authorized_keys", []string{">&"}, []string{"~/.ssh/authorized_keys"}},
		{"echo x 2>& out.txt", []string{"2>&"}, []string{"out.txt"}},
		{"cat <& in.txt", []string{"<&"}, []string{"in.txt"}},
		// …and the two spellings that really do name no file. `>&1x` is the
		// discriminator between them: the digits are the start of a filename,
		// not a descriptor, so consuming them eagerly would lose the target.
		{"go build >&2", []string{">&2"}, []string{""}},
		{"go build >&-", []string{">&-"}, []string{""}},
		{"echo x >&1x", []string{">&"}, []string{"1x"}},
	}
	for _, tc := range cases {
		segs, err := execpolicy.ParseCommandList(tc.raw)
		if err != nil {
			t.Fatalf("ParseCommandList(%q): %v", tc.raw, err)
		}
		if len(segs) != 1 {
			t.Fatalf("ParseCommandList(%q): got %d segments, want 1", tc.raw, len(segs))
		}
		if len(segs[0].Redirects) != len(tc.ops) {
			t.Fatalf("ParseCommandList(%q): got %d redirects, want %d", tc.raw, len(segs[0].Redirects), len(tc.ops))
		}
		for i, r := range segs[0].Redirects {
			if r.Operator != tc.ops[i] || r.Target != tc.target[i] {
				t.Errorf("ParseCommandList(%q) redirect %d = %q→%q, want %q→%q",
					tc.raw, i, r.Operator, r.Target, tc.ops[i], tc.target[i])
			}
		}
		if segs[0].Program != "echo" && segs[0].Program != "cat" && segs[0].Program != "go" {
			t.Errorf("ParseCommandList(%q).Program = %q", tc.raw, segs[0].Program)
		}
	}
}

// TestParseCommandListNormalizesTheProgram: Program/Args are normalized the
// same way Parse normalizes them, so a path-qualified or .exe-suffixed program
// in a chain resolves to the same name a lone command would.
//
// The unquoted-Windows-path spelling is absent on purpose. A bare backslash is
// an ESCAPE here, exactly as in Lex and in every real shell, so `C:\Go\bin`
// flattens to `C:Gobin` — which is why the guard matches profile globs against
// Segment.Text (a verbatim slice) and never against a re-join of these fields.
// A caller that means a literal backslash single-quotes it (double quotes go
// through the same escape rule as Lex), and that spelling is covered below.
func TestParseCommandListNormalizesTheProgram(t *testing.T) {
	segs, err := execpolicy.ParseCommandList(`/usr/bin/go test && 'C:\Go\bin\GO.EXE' vet`)
	if err != nil {
		t.Fatalf("ParseCommandList: %v", err)
	}
	if len(segs) != 2 || segs[0].Program != "go" || segs[1].Program != "go" {
		t.Fatalf("got %+v, want two segments both normalized to \"go\"", segs)
	}
	if len(segs[0].Args) != 1 || segs[0].Args[0] != "test" {
		t.Errorf("segment 0 Args = %v, want [test]", segs[0].Args)
	}
	if len(segs[1].Args) != 1 || segs[1].Args[0] != "vet" {
		t.Errorf("segment 1 Args = %v, want [vet]", segs[1].Args)
	}
}
