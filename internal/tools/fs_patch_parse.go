package tools

import (
	"fmt"
	"strings"
)

// patchOp kinds.
const (
	opAdd = iota
	opUpdate
	opDelete
	opMove
)

// patchOp is one parsed operation from a patch. path is the target for
// add/update/delete and the DESTINATION for move; from is the move source.
type patchOp struct {
	kind    int
	path    string
	from    string // move source
	addBody string // add: raw file content (verbatim)
	updOld  string // update: search text (context + removed lines joined by \n)
	updNew  string // update: replacement text (context + added lines joined by \n)
}

// parsePatch parses patch text into an ordered op list. Format (codex-style
// envelope; bodies are self-defined):
//
//	*** Begin Patch
//	*** Add File: <path>
//	<raw content lines until the next "*** " header>
//	*** Update File: <path>
//	 <context line>      (a single leading space)
//	-<removed line>
//	+<added line>
//	*** Delete File: <path>
//	*** Move File: <from>
//	*** To: <to>
//	*** End Patch
//
// Add bodies are taken VERBATIM (no per-line prefix) so the model can paste file
// content directly. Update bodies are unified-style: a leading space is context
// (kept on both sides), "-" is a removal (old side only), "+" is an addition
// (new side only); a completely blank line is treated as a context line with
// empty text so blank lines in the matched region are preserved. The whole block
// becomes ONE search/replace pair (old = context+removed joined by "\n", new =
// context+added joined by "\n"); for multiple disjoint edits in one file use
// multiple Update blocks (each is applied in order). A line within a block that
// starts with "*** " terminates the block (so file content starting with "*** "
// cannot appear inside an Add/Update body — a documented v1 limitation).
func parsePatch(text string) ([]patchOp, error) {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1] // drop trailing empty from final newline
	}
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "*** Begin Patch" {
		return nil, fmt.Errorf("patch must start with %q", "*** Begin Patch")
	}
	var ops []patchOp
	i := 1
	for i < len(lines) {
		line := lines[i]
		switch {
		case strings.HasPrefix(line, "*** End Patch"):
			return ops, nil
		case strings.HasPrefix(line, "*** Add File: "):
			path := strings.TrimSpace(strings.TrimPrefix(line, "*** Add File: "))
			if path == "" {
				return nil, fmt.Errorf("Add File: empty path at line %d", i+1)
			}
			body, next := takeRawBody(lines, i+1)
			ops = append(ops, patchOp{kind: opAdd, path: path, addBody: body})
			i = next
		case strings.HasPrefix(line, "*** Update File: "):
			path := strings.TrimSpace(strings.TrimPrefix(line, "*** Update File: "))
			if path == "" {
				return nil, fmt.Errorf("Update File: empty path at line %d", i+1)
			}
			body, next := takeRawBody(lines, i+1)
			old, neu, err := parseUpdateBody(body, i+1)
			if err != nil {
				return nil, err
			}
			ops = append(ops, patchOp{kind: opUpdate, path: path, updOld: old, updNew: neu})
			i = next
		case strings.HasPrefix(line, "*** Delete File: "):
			path := strings.TrimSpace(strings.TrimPrefix(line, "*** Delete File: "))
			if path == "" {
				return nil, fmt.Errorf("Delete File: empty path at line %d", i+1)
			}
			ops = append(ops, patchOp{kind: opDelete, path: path})
			i++
		case strings.HasPrefix(line, "*** Move File: "):
			from := strings.TrimSpace(strings.TrimPrefix(line, "*** Move File: "))
			if from == "" {
				return nil, fmt.Errorf("Move File: empty source at line %d", i+1)
			}
			if i+1 >= len(lines) || !strings.HasPrefix(lines[i+1], "*** To: ") {
				return nil, fmt.Errorf("Move File: missing %q at line %d", "*** To:", i+2)
			}
			to := strings.TrimSpace(strings.TrimPrefix(lines[i+1], "*** To: "))
			if to == "" {
				return nil, fmt.Errorf("Move File: empty destination at line %d", i+2)
			}
			ops = append(ops, patchOp{kind: opMove, path: to, from: from})
			i += 2
		default:
			return nil, fmt.Errorf("unexpected line %d: %q (expected a *** header)", i+1, line)
		}
	}
	return nil, fmt.Errorf("patch missing %q", "*** End Patch")
}

// takeRawBody collects lines until the next "*** " header (or end of input),
// returning the joined body and the index of the terminating header line (to
// resume parsing from). Used for both Add bodies (verbatim) and Update bodies
// (prefix-encoded).
func takeRawBody(lines []string, start int) (string, int) {
	var body []string
	i := start
	for i < len(lines) && !strings.HasPrefix(lines[i], "*** ") {
		body = append(body, lines[i])
		i++
	}
	return strings.Join(body, "\n"), i
}

// parseUpdateBody decodes a unified-style update body into one (old, new)
// search/replace pair. startLine is the 1-based line of the body's first line
// (for error messages).
func parseUpdateBody(body string, startLine int) (old, neu string, err error) {
	if body == "" {
		return "", "", fmt.Errorf("Update File: empty body at line %d", startLine)
	}
	var oldParts, newParts []string
	for idx, raw := range strings.Split(body, "\n") {
		switch {
		case raw == "":
			oldParts = append(oldParts, "")
			newParts = append(newParts, "")
		case strings.HasPrefix(raw, " "):
			oldParts = append(oldParts, raw[1:])
			newParts = append(newParts, raw[1:])
		case strings.HasPrefix(raw, "-"):
			oldParts = append(oldParts, strings.TrimPrefix(raw, "-"))
		case strings.HasPrefix(raw, "+"):
			newParts = append(newParts, strings.TrimPrefix(raw, "+"))
		default:
			return "", "", fmt.Errorf("Update File: line %d has no context/-/+ prefix: %q", startLine+idx, raw)
		}
	}
	old = strings.Join(oldParts, "\n")
	neu = strings.Join(newParts, "\n")
	if old == "" {
		return "", "", fmt.Errorf("Update File: body has no context or removed lines at line %d", startLine)
	}
	return old, neu, nil
}
