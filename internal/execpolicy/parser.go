package execpolicy

import (
	"fmt"
	"path/filepath"
	"strings"
)

// RedirectSpec is a parsed redirection operator + its target word.
// Target is empty for "&>N"-style operators that fold a fd reference into the
// operator itself (e.g. "2>&1"); in that case the full operator carries the
// information and Target is "".
type RedirectSpec struct {
	Operator string
	Target   string
}

// Segment is a single executable inside a pipeline / control chain. A simple
// `go test ./...` parses to one Segment with Program="go", Args=["test","./..."].
//
// Two of the fields are populated by ParseCommandList only, and Parse leaves
// them zero. That asymmetry is deliberate rather than an omission: Parse
// describes ONE command for the rule engine, which matches on program and
// argument words and has no use for either field, while ParseCommandList
// describes a command LIST for the guard, which has to know how the segments
// are joined and what the operator's own source text was.
type Segment struct {
	Program   string
	Args      []string
	Redirects []RedirectSpec

	// Operator is the control operator that separates this segment from the
	// NEXT one (AndIf, OrIf, Pipe, Semi), or NoOperator on the last segment.
	// ParseCommandList only.
	Operator TokenKind

	// Text is the VERBATIM source slice this segment was cut from, whitespace
	// trimmed. ParseCommandList only.
	//
	// It is a slice rather than a re-join of Program+Args because the guard
	// matches profile globs against it, and a re-join loses quoting: the
	// policy layer would be matching `rm -rf /my dir` (two targets) against a
	// pattern written for `rm -rf "/my dir"` (one). For a single-segment
	// command Text is therefore byte-identical to the trimmed input, which is
	// what keeps the segmented guard path behaviour-identical to the
	// whole-string one for every unchained command.
	Text string
}

// Command is the fully parsed shell command. Segments is non-empty; Control
// captures a trailing &&/|| if present (the lexer accepts these for
// diagnostics; A1's guard still hard-denies them — see policy.go).
type Command struct {
	Segments []Segment
	Control  TokenKind
}

// Parse consumes the lexer's Token slice and produces a structured Command.
// It rejects:
//   - A leading redirection (no Program to attach it to);
//   - A redirection without a following Word target (e.g. "cat >" with no
//     file);
//   - A trailing operator (Pipe / AndIf / OrIf) with no following Segment.
//
// Note on AndIf/OrIf: A1 keeps the guard's HardDeny on these via the
// shell-metacharacter firewall (Task 1). Parse records the Control token for
// observability, but Evaluate (Task 5) still returns hard_deny for any
// non-zero Control — parsing them is strictly for explainability, not
// execution.
func Parse(raw string) (Command, error) {
	tokens, err := Lex(raw)
	if err != nil {
		return Command{}, err
	}
	var cmd Command
	var seg Segment
	flushSegment := func() error {
		if seg.Program == "" {
			return fmt.Errorf("execpolicy: operator without executable segment")
		}
		cmd.Segments = append(cmd.Segments, seg)
		seg = Segment{}
		return nil
	}
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		switch tok.Kind {
		case Word:
			if seg.Program == "" {
				seg.Program = normalizeProgram(tok.Text)
			} else {
				seg.Args = append(seg.Args, tok.Text)
			}
		case Redirect:
			if seg.Program == "" {
				return Command{}, fmt.Errorf("execpolicy: redirect before executable")
			}
			target := ""
			if !strings.Contains(tok.Text, "&") {
				if i+1 >= len(tokens) || tokens[i+1].Kind != Word {
					return Command{}, fmt.Errorf("execpolicy: redirect %q missing target", tok.Text)
				}
				i++
				target = tokens[i].Text
			}
			seg.Redirects = append(seg.Redirects, RedirectSpec{Operator: tok.Text, Target: target})
		case Pipe:
			if err := flushSegment(); err != nil {
				return Command{}, err
			}
		case AndIf, OrIf:
			if err := flushSegment(); err != nil {
				return Command{}, err
			}
			cmd.Control = tok.Kind
			// &&/|| are recognized structurally but A1 guard keeps the hard
			// metacharacter deny; parsing them does NOT make them executable.
		default:
			return Command{}, fmt.Errorf("execpolicy: unsupported token %q", tok.Text)
		}
	}
	if err := flushSegment(); err != nil {
		return Command{}, err
	}
	return cmd, nil
}

// normalizeProgram canonicalizes a program word so prefix rules can match
// across absolute paths, Windows backslash paths, and trailing .exe suffixes.
// Examples:
//
//	"/usr/bin/go"          → "go"
//	"C:\\Go\\bin\\GO.EXE"  → "go"
//	"go.exe"               → "go"
//	"printf"               → "printf"
func normalizeProgram(raw string) string {
	clean := strings.ReplaceAll(raw, "\\", "/")
	name := filepath.Base(clean)
	name = strings.TrimSuffix(strings.ToLower(name), ".exe")
	return name
}
