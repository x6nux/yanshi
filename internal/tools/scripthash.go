package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// maxHashedScriptBytes bounds what we are willing to read to identify a
// script. A "script" larger than this is not a script anyone reviewed, and
// hashing an arbitrarily large file on the permission path would let a tool
// call stall the turn by pointing at something huge.
const maxHashedScriptBytes = 4 << 20 // 4 MiB

// scriptInterpreters are the programs whose FIRST non-flag operand is a script
// file to execute. `sh x.sh` runs x.sh; `ls x.sh` merely mentions it.
//
// The shells here overlap with the ones an operator might reasonably run
// interactively, which is fine: an invocation with no script operand (`bash`,
// `python -c "..."`) yields no path and is simply not treated as a script
// execution.
var scriptInterpreters = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true,
	"fish": true, "python": true, "python2": true, "python3": true,
	"node": true, "deno": true, "bun": true,
	"ruby": true, "perl": true, "php": true, "lua": true, "Rscript": true,
	"powershell": true, "pwsh": true, "osascript": true,
}

// interpreterFlagsTakingValue are the interpreter options that consume the
// next argument, so the operand after them is NOT the script. Without this,
// `python -m http.server` would hash "http.server" as though it were a file
// (it is not one, so the hash would fail and the call would simply be re-
// asked — correct, but for the wrong reason and only by luck).
var interpreterFlagsTakingValue = map[string]bool{
	"-c": true, "-m": true, "-e": true, "-W": true, "-X": true,
	"--eval": true, "--module": true, "--command": true, "-Command": true,
	"-File": true, // powershell -File x.ps1: the VALUE is the script
}

// scriptPathFromCommand returns the path of the script a command executes, or
// "" when the command does not execute a script file.
//
// Two shapes count:
//
//	sh install.sh / python3 setup.py / node build.js   — interpreter + operand
//	./install.sh / /tmp/setup.sh                       — the program IS a script
//
// A bare program name with no path separator (`ls`, `make`) is never treated
// as a script: it resolves through PATH to something the user did not name,
// and hashing whatever PATH happens to point at today would attach an
// approval to a moving target.
func scriptPathFromCommand(cmd string) string {
	program, args, ok := lexCommandLite(cmd)
	if !ok || program == "" {
		return ""
	}
	base := normalizeProgramBase(program)
	if scriptInterpreters[base] {
		return firstOperand(args)
	}
	// The program itself is the script: ./install.sh, /tmp/setup.sh. Requiring
	// a path separator (or a leading dot) is what excludes bare PATH lookups.
	if strings.ContainsAny(program, "/\\") || strings.HasPrefix(program, ".") {
		return program
	}
	return ""
}

// firstOperand returns the first argument that is not a flag and not a flag's
// value. Returns "" when every argument is consumed by flags.
func firstOperand(args []string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			if i+1 < len(args) {
				return args[i+1]
			}
			return ""
		}
		if strings.HasPrefix(a, "-") {
			if interpreterFlagsTakingValue[a] {
				// -File names the script; every other value-taking flag
				// consumes something that is not one.
				if a == "-File" && i+1 < len(args) {
					return args[i+1]
				}
				i++
			}
			continue
		}
		return a
	}
	return ""
}

// normalizeProgramBase reduces "/usr/bin/python3" and "PYTHON3.EXE" to
// "python3" so the interpreter table matches however the program was spelled.
// It mirrors guard's normalizeProgramWord; the duplication is one small
// function across a package boundary that must not import guard.
func normalizeProgramBase(w string) string {
	w = strings.ReplaceAll(w, "\\", "/")
	if i := strings.LastIndex(w, "/"); i >= 0 {
		w = w[i+1:]
	}
	w = strings.TrimSuffix(strings.ToLower(w), ".exe")
	// Rscript is the one interpreter whose canonical spelling is not lower
	// case; the table holds it capitalised, so restore that one name.
	if w == "rscript" {
		return "Rscript"
	}
	return w
}

// lexCommandLite splits a command into a program word and its arguments,
// honouring single and double quotes. It is deliberately simpler than a shell:
// commands reaching this point have already been refused if they contain a
// control operator, so there is no pipeline or substitution to handle.
func lexCommandLite(cmd string) (program string, args []string, ok bool) {
	var tokens []string
	var cur strings.Builder
	quote := byte(0)
	inTok := false
	flush := func() {
		if inTok {
			tokens = append(tokens, cur.String())
			cur.Reset()
			inTok = false
		}
	}
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		if quote != 0 {
			if c == quote {
				quote = 0
				continue
			}
			cur.WriteByte(c)
			inTok = true
			continue
		}
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			flush()
		case c == '\'' || c == '"':
			quote = c
			inTok = true
		default:
			cur.WriteByte(c)
			inTok = true
		}
	}
	if quote != 0 {
		return "", nil, false
	}
	flush()
	if len(tokens) == 0 {
		return "", nil, false
	}
	return tokens[0], tokens[1:], true
}

// CommandRunsAScript reports whether a shell command executes a script file
// whose contents can be hashed. It is the predicate the WS auto-approval path
// uses to decide whether a model verdict is worth remembering: a rule is only
// safe to keep when its scope pins the script's content, and that pin exists
// only when the file was actually readable.
func CommandRunsAScript(shell, workdir string) bool {
	return hashScriptForCommand(shell, workdir) != ""
}

// hashScriptForCommand returns the SHA-256 of the script a command executes,
// or "" when the command runs no script or the file cannot be read.
//
// Returning "" for an unreadable file is the fail-safe direction. It makes
// CommandRunsAScript false, so the WS path returns a plain Allow instead of
// AllowPersistent and no rule is recorded: the call is asked about every time
// rather than approved on the strength of a path alone. workdir resolves
// relative paths; an empty workdir leaves them relative to the process, which
// is what the shell tool itself would do.
func hashScriptForCommand(shell, workdir string) string {
	path := scriptPathFromCommand(shell)
	if path == "" {
		return ""
	}
	if !filepath.IsAbs(path) && workdir != "" {
		path = filepath.Join(workdir, path)
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.IsDir() || info.Size() > maxHashedScriptBytes {
		return ""
	}
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(f, maxHashedScriptBytes)); err != nil {
		return ""
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}
