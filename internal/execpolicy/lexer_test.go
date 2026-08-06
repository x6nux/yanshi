package execpolicy

import "testing"

func TestLexConsumesMultiByteOperatorsExactlyOnce(t *testing.T) {
	got, err := Lex(`go test ./... && printf ok || cat x >> out 2>>err`)
	if err != nil {
		t.Fatal(err)
	}
	want := []Token{
		{Kind: Word, Text: "go"}, {Kind: Word, Text: "test"}, {Kind: Word, Text: "./..."},
		{Kind: AndIf, Text: "&&"}, {Kind: Word, Text: "printf"}, {Kind: Word, Text: "ok"},
		{Kind: OrIf, Text: "||"}, {Kind: Word, Text: "cat"}, {Kind: Word, Text: "x"},
		{Kind: Redirect, Text: ">>"}, {Kind: Word, Text: "out"},
		{Kind: Redirect, Text: "2>>"}, {Kind: Word, Text: "err"},
	}
	if len(got) != len(want) {
		t.Fatalf("tokens=%#v, want=%#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("token[%d]=%#v, want=%#v", i, got[i], want[i])
		}
	}
}

// TestLexRejectsExpansionAndGlobBypasses —— 见台账。
//
// ledger: A1/S06#3 已知绕过样例（IFS、$()、glob 注入）有回归测试
func TestLexRejectsExpansionAndGlobBypasses(t *testing.T) {
	for _, raw := range []string{
		`printf $IFS`, `printf ${IFS}`, `printf $VAR`, `printf $(id)`,
		"printf `id`", `printf %PATH%`, `cat *.go`, `cat file?.go`,
	} {
		if _, err := Lex(raw); err == nil {
			t.Fatalf("Lex(%q) must reject expansion/glob syntax", raw)
		}
	}
}

func TestLexAcceptsQuotedWordsAbsolutePathsAndWindowsExeCase(t *testing.T) {
	for _, raw := range []string{
		`go test ./...`, `/usr/bin/go test ./...`, `C:\\Go\\bin\\GO.EXE version`,
		`printf "hello world"`, `printf 'hello world'`,
	} {
		if _, err := Lex(raw); err != nil {
			t.Fatalf("Lex(%q): %v", raw, err)
		}
	}
}

func TestLex_EmptyInputRejected(t *testing.T) {
	_, err := Lex("")
	if err == nil {
		t.Fatal("empty input must be rejected")
	}
}

func TestLex_UnterminatedQuoteRejected(t *testing.T) {
	_, err := Lex(`printf "hello`)
	if err == nil {
		t.Fatal("unterminated quote must be rejected")
	}
}

func TestLex_TrailingEscapeRejected(t *testing.T) {
	_, err := Lex(`printf hello\`)
	if err == nil {
		t.Fatal("trailing escape must be rejected")
	}
}

func TestLex_StandaloneAmpersandRejected(t *testing.T) {
	_, err := Lex(`sleep 10 &`)
	if err == nil {
		t.Fatal("standalone & must be rejected")
	}
}

func TestLex_RedirectWithFdNumbers(t *testing.T) {
	got, err := Lex(`echo hello 2>/dev/null 1>&2`)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) < 5 {
		t.Fatalf("expected at least 5 tokens, got %d: %#v", len(got), got)
	}
	if got[2].Kind != Redirect || got[2].Text != "2>" {
		t.Fatalf("expected redirect 2>, got %#v", got[2])
	}
}

func TestLex_EscapedCharInDoubleQuote(t *testing.T) {
	got, err := Lex(`printf "hello\nworld"`)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1].Text != "hellonworld" {
		t.Fatalf("expected token with unescaped \\n (backslash removed, n kept), got %#v", got)
	}
}
