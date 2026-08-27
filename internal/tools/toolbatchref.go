// internal/tools/toolbatchref.go
//
// T12's reference substitution. This is the entire "language" of tool_batch,
// and keeping it this small is a security property rather than a style choice.
//
// # The grammar, in full
//
//	$N          the whole result text of step N
//	$N.k        one field of step N's result, when that result parses as JSON
//	$N.k.j.0    nested; a numeric component indexes an array
//
// There is nothing else. No arithmetic, no calls, no comparisons, no string
// concatenation operators (adjacency in a string literal is all you get), no
// computed field names, no wildcards, no slicing. A reference either names a
// value that exists or it is an error.
//
// # Why substitution operates on decoded JSON, never on argument text
//
// The naive implementation does strings.Replace over the raw argument JSON
// before unmarshalling it. That is an injection primitive: a step result
// containing `"` or `}` reshapes the argument OBJECT, so a prior tool's output
// — which may be file contents, a fetched page, or a commit message an
// attacker wrote — can add or overwrite arguments the model never asked for.
// `{"path":"$0"}` with a result of `x","recursive":true` becomes a different
// call than the one the model made and the operator approved.
//
// So substitution here walks the DECODED structure and replaces string leaves.
// The result is inserted as a value, and a value cannot become syntax. The
// worst a hostile prior result can do is be a long or strange string in the
// slot the model chose for it.
//
// # Why a whole-string reference keeps its JSON type
//
// `"$0"` alone yields step 0's decoded value when it parses as JSON (object,
// array, number, bool), and its raw text otherwise. That is what makes
// `{"steps": "$0"}` usable for a tool whose parameter is an object. A
// reference EMBEDDED in a longer string (`"prefix $0"`) always stringifies,
// because there is nowhere for a non-string value to go.
package tools

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// refPattern matches one reference. The path is a dot-separated run of
// identifier or index components; the character class deliberately excludes
// everything that could be an operator, a quote, a bracket or whitespace, so a
// malformed reference fails to match and is left as literal text rather than
// being half-interpreted.
//
// `[A-Za-z0-9_]` for a component allows JSON keys with underscores and array
// indices, and nothing else. A key containing a dot, a space or a DASH is
// simply not reachable — accepting a quoted-key syntax to reach it would be
// the first step toward the expression language this file exists to avoid.
//
// The dash is excluded for a sharper reason than the others, and a test found
// it: `-` is simultaneously a plausible JSON key character (`content-type`)
// and the most natural separator to write between two references
// (`"$0.a-$1.b"`). Allowing it makes those two readings collide and the regex
// can only have one. The separator reading wins because joining two results is
// the common case.
//
// The cost is real and is NOT that dashed keys fail loudly — that was the
// first draft of this comment and the test disproved it. `$0.content-type`
// against `{"content-type":…}` does error, but against a payload that ALSO has
// a `content` key it resolves `$0.content` and appends a literal `-type`,
// silently. Both readings have a silent-failure shape; this one's is rarer.
// TestBatchDashIsASeparatorNotAKeyCharacter asserts both halves so a future
// edit reverses the trade deliberately rather than by accident.
var refPattern = regexp.MustCompile(`\$(\d+)((?:\.[A-Za-z0-9_]+)*)`)

// wholeRefPattern matches a string that is EXACTLY one reference and nothing
// else, which is the case where the referenced value keeps its JSON type.
var wholeRefPattern = regexp.MustCompile(`^\$(\d+)((?:\.[A-Za-z0-9_]+)*)$`)

// maxRefDepth caps how deep a path may reach. Not a security boundary — the
// walk is over an already-parsed value and terminates regardless — but a
// 200-component path is a model malfunction, and failing loudly beats
// returning "no such field" from somewhere in the middle of it.
const maxRefDepth = 16

// substituteRefs rewrites every string leaf of args, replacing references to
// earlier step results.
//
// results holds the outputs of the steps that have ALREADY run, so a reference
// to a step at or beyond the current index is an error rather than an empty
// string. That distinction matters: a model that mistakenly refers forward
// gets told so, instead of receiving a call with a silently blanked argument
// and a plausible-looking result.
func substituteRefs(args json.RawMessage, results []string) (json.RawMessage, error) {
	if len(args) == 0 {
		return json.RawMessage("{}"), nil
	}
	var decoded any
	if err := json.Unmarshal(args, &decoded); err != nil {
		// Args that are not JSON cannot contain a substitutable structure;
		// pass them through untouched rather than inventing a text-level
		// substitution path (which would be the injection shape above).
		return args, nil
	}
	replaced, err := substituteValue(decoded, results)
	if err != nil {
		return nil, err
	}
	out, err := json.Marshal(replaced)
	if err != nil {
		return nil, fmt.Errorf("re-encode step args: %w", err)
	}
	return out, nil
}

// substituteValue walks a decoded JSON value, rewriting string leaves.
//
// Maps and slices are rebuilt rather than mutated in place: the decoded value
// is shared with nothing here, but rebuilding makes the function total and
// side-effect free, which is what lets it be tested one shape at a time.
func substituteValue(v any, results []string) (any, error) {
	switch t := v.(type) {
	case string:
		return substituteString(t, results)
	case []any:
		out := make([]any, len(t))
		for i, elem := range t {
			r, err := substituteValue(elem, results)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, elem := range t {
			// Keys are NOT substituted. A reference in a key position would
			// let a prior result decide which argument is being set, which is
			// the computed-field-name capability this design excludes.
			r, err := substituteValue(elem, results)
			if err != nil {
				return nil, err
			}
			out[k] = r
		}
		return out, nil
	default:
		return v, nil
	}
}

// substituteString handles one string leaf: whole-string references keep their
// type, embedded ones stringify.
func substituteString(s string, results []string) (any, error) {
	if m := wholeRefPattern.FindStringSubmatch(s); m != nil {
		return resolveRef(m[1], m[2], results)
	}
	if !strings.Contains(s, "$") {
		return s, nil
	}
	var outer error
	out := refPattern.ReplaceAllStringFunc(s, func(match string) string {
		m := refPattern.FindStringSubmatch(match)
		val, err := resolveRef(m[1], m[2], results)
		if err != nil {
			if outer == nil {
				outer = err
			}
			return match
		}
		return stringifyRef(val)
	})
	if outer != nil {
		return nil, outer
	}
	return out, nil
}

// resolveRef looks up one reference and returns the referenced value.
//
// The step result is parsed as JSON only when a PATH is present. A bare `$0`
// on a tool that returns plain text must yield that text, not a parse error;
// a path on that same result has nothing to walk and says so.
func resolveRef(indexText, path string, results []string) (any, error) {
	idx, err := strconv.Atoi(indexText)
	if err != nil {
		return nil, fmt.Errorf("bad step reference $%s", indexText)
	}
	if idx < 0 || idx >= len(results) {
		return nil, fmt.Errorf("reference $%d names step %d, but only steps 0..%d have run "+
			"(a step may only reference EARLIER steps)", idx, idx, len(results)-1)
	}
	raw := results[idx]
	if path == "" {
		var decoded any
		if err := json.Unmarshal([]byte(raw), &decoded); err == nil {
			return decoded, nil
		}
		return raw, nil
	}
	var decoded any
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		return nil, fmt.Errorf("reference $%d%s asks for a field, but step %d's result is not JSON",
			idx, path, idx)
	}
	return walkPath(decoded, idx, path)
}

// walkPath follows a dot path through a decoded value.
//
// A numeric component indexes an array; anything else keys an object. A
// component that is numeric AND names an object key resolves as the key,
// because a JSON object with the key "0" is far more likely than a model
// intending array semantics on an object.
func walkPath(v any, idx int, path string) (any, error) {
	parts := strings.Split(strings.TrimPrefix(path, "."), ".")
	if len(parts) > maxRefDepth {
		return nil, fmt.Errorf("reference $%d%s is more than %d levels deep", idx, path, maxRefDepth)
	}
	cur := v
	for _, part := range parts {
		switch node := cur.(type) {
		case map[string]any:
			next, ok := node[part]
			if !ok {
				return nil, fmt.Errorf("reference $%d%s: step %d's result has no field %q",
					idx, path, idx, part)
			}
			cur = next
		case []any:
			n, err := strconv.Atoi(part)
			if err != nil {
				return nil, fmt.Errorf("reference $%d%s: %q is not an array index", idx, path, part)
			}
			if n < 0 || n >= len(node) {
				return nil, fmt.Errorf("reference $%d%s: index %d is out of range (length %d)",
					idx, path, n, len(node))
			}
			cur = node[n]
		default:
			return nil, fmt.Errorf("reference $%d%s: %q cannot be applied to a scalar", idx, path, part)
		}
	}
	return cur, nil
}

// stringifyRef renders a referenced value for embedding inside a larger
// string. Strings embed verbatim; everything else embeds as its JSON form,
// which is the only rendering that round-trips.
func stringifyRef(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}
