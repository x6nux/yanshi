package execpolicy

import "testing"

// TestDecodeANSIC is the table for the $'...' decoder. Every escape form bash
// honors is covered, plus the two behaviours that keep decoding honest:
// unrecognized escapes keep their backslash (bash does the same, so the decoder
// cannot manufacture a token the shell would not produce), and an unterminated
// span is left alone.
func TestDecodeANSIC(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		decoded bool
	}{
		{"no ansi-c span", "rm -rf /tmp", "rm -rf /tmp", false},
		{"hex escapes spell rm -rf /", `$'\x72\x6d -rf /'`, "rm -rf /", true},
		{"single-digit hex", `$'\x7a'`, "z", true},
		{"octal escapes", `$'\162\155'`, "rm", true},
		{"short octal", `$'\11'`, "\t", true},
		{"simple escapes", `$'a\nb\tc\\d'`, "a\nb\tc\\d", true},
		{"escaped quote", `$'it\'s'`, "it's", true},
		{"bell and escape", `$'\a\e'`, "\a\x1b", true},
		{"control char", `$'\cA'`, "\x01", true},
		{"unicode short", `$'A'`, "A", true},
		{"unicode long", `$'\U00000042'`, "B", true},
		{"unknown escape keeps backslash", `$'\q'`, `\q`, true},
		{"bare x with no digits", `$'\x'`, `\x`, true},
		{"span in the middle of a command", `bash -c $'\x72\x6d' /tmp`, "bash -c rm /tmp", true},
		{"multiple spans", `$'\x61'$'\x62'`, "ab", true},
		{"unterminated span is left verbatim", `$'\x72\x6d`, `$'\x72\x6d`, false},
		{"empty span", `$''`, "", true},
		{"dollar without quote is untouched", "$HOME/x", "$HOME/x", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, decoded := DecodeANSIC(c.in)
			if got != c.want || decoded != c.decoded {
				t.Fatalf("DecodeANSIC(%q) = (%q, %v), want (%q, %v)", c.in, got, decoded, c.want, c.decoded)
			}
		})
	}
}

// TestParseCommandListDecodesANSICWords is the reason the decoder moved into
// this package.
//
// A redirect target spelled in ANSI-C reached guard.checkFS as its literal
// source text, so the built-in credential denylist — which matches on the
// `~/.ssh` directory prefix — did not fire, and the write was allowed.
// ParseCommandList and guard's own lexer now resolve a word to the same bytes.
func TestParseCommandListDecodesANSICWords(t *testing.T) {
	for _, c := range []struct {
		raw        string
		wantProg   string
		wantArgs   []string
		wantTarget string
	}{
		{`echo ssh-rsa AAAA > ~/$'\x2e\x73\x73\x68'/authorized_keys`,
			"echo", []string{"ssh-rsa", "AAAA"}, "~/.ssh/authorized_keys"},
		{`echo x > $'\x6f'ut`, "echo", []string{"x"}, "out"},
		{`echo x > "ou"$'\x74'`, "echo", []string{"x"}, "out"},
		{`$'\x72\x6d' -rf /tmp/x`, "rm", []string{"-rf", "/tmp/x"}, ""},
		// A decoded span joins the CURRENT WORD and is never re-scanned, so an
		// operator spelled in hex stays one word instead of splitting the list.
		{`echo $'\x26\x26' x`, "echo", []string{"&&", "x"}, ""},
		{`echo $'\x3e' x`, "echo", []string{">", "x"}, ""},
	} {
		segs, err := ParseCommandList(c.raw)
		if err != nil {
			t.Errorf("ParseCommandList(%q) = error %v", c.raw, err)
			continue
		}
		if len(segs) != 1 {
			t.Errorf("ParseCommandList(%q) produced %d segments, want 1 — a quoted span must not split the list", c.raw, len(segs))
			continue
		}
		if segs[0].Program != c.wantProg {
			t.Errorf("ParseCommandList(%q).Program = %q, want %q", c.raw, segs[0].Program, c.wantProg)
		}
		if len(segs[0].Args) != len(c.wantArgs) {
			t.Errorf("ParseCommandList(%q).Args = %q, want %q", c.raw, segs[0].Args, c.wantArgs)
		} else {
			for i, a := range c.wantArgs {
				if segs[0].Args[i] != a {
					t.Errorf("ParseCommandList(%q).Args[%d] = %q, want %q", c.raw, i, segs[0].Args[i], a)
				}
			}
		}
		got := ""
		for _, r := range segs[0].Redirects {
			if r.Target != "" {
				got = r.Target
			}
		}
		if got != c.wantTarget {
			t.Errorf("ParseCommandList(%q) redirect target = %q, want %q — the FS dimension "+
				"judges this string, so it has to be the one the shell opens", c.raw, got, c.wantTarget)
		}
	}
	// An unterminated span is a truncated string: any reading of it is a guess,
	// so it joins the other structural refusals rather than being decoded
	// half-way.
	if _, err := ParseCommandList(`echo x > $'\x6f`); err == nil {
		t.Error(`ParseCommandList("echo x > $'\x6f") = nil error, want a structural refusal`)
	}
}
