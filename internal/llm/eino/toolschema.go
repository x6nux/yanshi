// internal/llm/eino/toolschema.go
//
// M6: tool JSON Schema sanitization.
//
// yanshi ships a large, deeply nested tool surface. eino derives each tool's
// parameter schema from a Go struct, which produces perfectly legal JSON Schema
// 2020-12 — `$defs` plus `$ref` for every named nested type, `["string","null"]`
// unions for optional pointers, `$schema`/`$id` annotations at the root. The
// OpenAI and Anthropic first-party endpoints accept all of it.
//
// Domestic gateways, vLLM's OpenAI-compatible server, llama.cpp and several
// relay products do not. They reject the request outright (400) or, worse,
// accept it and hand the model a schema their own grammar compiler silently
// mangled. The failure is per-request and total: every turn fails, and the
// error text names a keyword rather than a tool, so the operator has nothing to
// act on.
//
// The fix is the one QwenPaw arrived at (see
// providers/openai_chat_model_compat.py::_sanitize_tool_schemas): rewrite each
// tool's parameter schema into the portable subset immediately before it goes
// on the wire. The passes here are that file's list, in its order, with two
// additions the requirement asked for (combinator flattening, root annotation
// stripping).
//
// WHAT IS AND IS NOT LOSSY. Ref inlining, `$schema`/`$id` removal, nullable
// flattening and regex-shorthand expansion are all value-set preserving: the
// sanitized schema accepts exactly the same documents. `oneOf`/`allOf`
// flattening is preserving only when the branches merge; when they do not, the
// keyword is dropped and the position degrades to a LOOSER schema (it accepts
// more). Degrading loose rather than tight is deliberate — a tightened schema
// would make the provider reject arguments the tool would have accepted, which
// is a silent capability loss, whereas a loosened one at worst lets a malformed
// argument reach the tool's own decoder, which already validates.
package eino

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/eino-contrib/jsonschema"

	"github.com/cloudwego/eino/schema"
)

// droppedSchemaKeywords are keywords removed outright by the sanitizer.
//
// Two groups, dropped for different reasons. The `$`-prefixed annotations and
// `$defs`/`definitions` are META: after inlineSchemaRefs there is nothing left
// to refer to them, and several strict validators reject a `$schema` they do
// not recognise. The rest are STRUCTURAL keywords from draft 2019-09/2020-12
// that the OpenAI function-calling schema subset does not implement at all —
// a provider that sees them either errors or ignores them, and ignoring them
// silently is the dangerous half (a `dependentRequired` that is ignored makes
// the model believe a constraint is enforced when it is not).
var droppedSchemaKeywords = map[string]bool{
	"$schema":               true,
	"$id":                   true,
	"$anchor":               true,
	"$comment":              true,
	"$dynamicRef":           true,
	"$dynamicAnchor":        true,
	"$defs":                 true,
	"definitions":           true,
	"if":                    true,
	"then":                  true,
	"else":                  true,
	"dependentSchemas":      true,
	"dependentRequired":     true,
	"patternProperties":     true,
	"propertyNames":         true,
	"prefixItems":           true,
	"contains":              true,
	"minContains":           true,
	"maxContains":           true,
	"unevaluatedItems":      true,
	"unevaluatedProperties": true,
	"contentSchema":         true,
}

// schemaValueKeys are keywords whose VALUE is itself a schema (or, legally, a
// boolean schema). Recursion is position-aware for exactly this reason: a bare
// `true` under `items` means "accept anything" and must become `{}`, while a
// bare `true` under `uniqueItems` is an ordinary boolean annotation and must
// stay a boolean. A position-blind walker corrupts the second while fixing the
// first.
var schemaValueKeys = map[string]bool{
	"items":                true,
	"not":                  true,
	"additionalProperties": true,
	"additionalItems":      true,
}

// schemaListKeys are keywords whose value is a LIST of schemas.
var schemaListKeys = map[string]bool{
	"allOf": true,
	"anyOf": true,
	"oneOf": true,
}

// schemaMapKeys are keywords whose value is a NAME → schema map.
var schemaMapKeys = map[string]bool{
	"properties": true,
}

// mergeableAllOfKeys are the keywords an `allOf` branch may contain and still
// be merged into its parent. A branch carrying anything else (a nested
// combinator, a conditional, a numeric bound that would have to be intersected
// with the parent's) cannot be merged without inventing semantics, so the whole
// `allOf` degrades instead.
var mergeableAllOfKeys = map[string]bool{
	"type":                 true,
	"properties":           true,
	"required":             true,
	"description":          true,
	"title":                true,
	"additionalProperties": true,
	"enum":                 true,
	"default":              true,
	"format":               true,
}

// regexShorthands maps ECMA-262 regex shorthand escapes to the equivalent
// character classes. The mapping is exact for the ASCII-only semantics JSON
// Schema's `pattern` keyword defines, so the substitution is lossless.
//
// It exists because llama.cpp compiles `pattern` into a GBNF grammar whose
// parser understands character classes and not escape shorthands; an
// unsubstituted `\d` makes the whole grammar fail to build.
var regexShorthands = map[string]string{
	`\d`: "[0-9]",
	`\D`: "[^0-9]",
	`\w`: "[a-zA-Z0-9_]",
	`\W`: "[^a-zA-Z0-9_]",
	`\s`: "[\t\n\r\f\v ]",
	`\S`: "[^\t\n\r\f\v ]",
}

// regexShorthandRe matches the shorthand escapes regexShorthands rewrites.
var regexShorthandRe = regexp.MustCompile(`\\[dDwWsS]`)

// SanitizeToolSchema rewrites one tool parameter schema into the portable
// subset described in this file's comment, returning a NEW map. The input is
// never mutated, because the caller's schema is shared across providers and
// only some of them want the rewrite.
//
// A nil or empty input yields the minimal object schema `{"type":"object"}`
// rather than nil: every OpenAI-compatible endpoint requires `parameters` to be
// an object schema, and a tool that takes no arguments still has to say so.
func SanitizeToolSchema(in map[string]any) map[string]any {
	if len(in) == 0 {
		return map[string]any{"type": "object"}
	}
	inlined := inlineSchemaRefs(in, collectDefs(in), map[string]bool{})
	node, ok := inlined.(map[string]any)
	if !ok {
		return map[string]any{"type": "object"}
	}
	out := sanitizeSchemaNode(node)
	if _, hasType := out["type"]; !hasType {
		out["type"] = "object"
	}
	return out
}

// collectDefs gathers the root's named definitions from both the 2019-09+
// `$defs` keyword and the draft-07 `definitions` keyword. Tools generated from
// Go structs use `$defs`; hand-written and MCP-supplied schemas still use
// `definitions`, and a sanitizer that knew only one would leave the other's
// refs dangling.
func collectDefs(root map[string]any) map[string]any {
	defs := map[string]any{}
	for _, key := range []string{"$defs", "definitions"} {
		if m, ok := root[key].(map[string]any); ok {
			for name, def := range m {
				defs[name] = def
			}
		}
	}
	return defs
}

// resolveLocalRef returns the definition a local `#/$defs/Name` or
// `#/definitions/Name` pointer names, or nil when ref is external or malformed.
// External refs are left alone deliberately: rewriting a URL we cannot fetch
// into an empty schema would silently widen the tool's contract.
func resolveLocalRef(ref string, defs map[string]any) any {
	if !strings.HasPrefix(ref, "#/") {
		return nil
	}
	parts := strings.Split(ref[2:], "/")
	if len(parts) != 2 {
		return nil
	}
	if parts[0] != "$defs" && parts[0] != "definitions" {
		return nil
	}
	return defs[parts[1]]
}

// inlineSchemaRefs substitutes every resolvable local `$ref` with the schema it
// names, merging the ref node's sibling annotations (a `description` written
// next to the ref) over the resolved body.
//
// resolving is the set of refs currently being expanded. A ref that reappears
// inside its own expansion is a cycle — a self-referential Go type — and is
// replaced by the empty schema `{}`. Inlining it would not terminate, and `{}`
// is the honest answer: at that depth the schema says nothing.
func inlineSchemaRefs(node any, defs map[string]any, resolving map[string]bool) any {
	switch t := node.(type) {
	case []any:
		out := make([]any, 0, len(t))
		for _, e := range t {
			out = append(out, inlineSchemaRefs(e, defs, resolving))
		}
		return out
	case map[string]any:
		if ref, ok := t["$ref"].(string); ok {
			if resolving[ref] {
				return map[string]any{}
			}
			if resolved, isMap := resolveLocalRef(ref, defs).(map[string]any); isMap {
				merged := make(map[string]any, len(resolved)+len(t))
				for k, v := range resolved {
					merged[k] = v
				}
				for k, v := range t {
					if k == "$ref" {
						continue
					}
					merged[k] = v
				}
				next := make(map[string]bool, len(resolving)+1)
				for k := range resolving {
					next[k] = true
				}
				next[ref] = true
				return inlineSchemaRefs(merged, defs, next)
			}
		}
		out := make(map[string]any, len(t))
		for k, v := range t {
			if k == "$defs" || k == "definitions" {
				continue
			}
			out[k] = inlineSchemaRefs(v, defs, resolving)
		}
		return out
	default:
		return node
	}
}

// sanitizeSchemaValue applies the sanitizer at a position where a SCHEMA is
// expected, converting the two legal boolean schemas into object form on the
// way: `true` (accept anything) becomes `{}` and `false` (accept nothing)
// becomes `{"not": {}}`.
func sanitizeSchemaValue(v any) any {
	switch t := v.(type) {
	case bool:
		if t {
			return map[string]any{}
		}
		return map[string]any{"not": map[string]any{}}
	case map[string]any:
		return sanitizeSchemaNode(t)
	case []any:
		out := make([]any, 0, len(t))
		for _, e := range t {
			out = append(out, sanitizeSchemaValue(e))
		}
		return out
	default:
		return v
	}
}

// sanitizeSchemaNode rewrites one schema object: drop unportable keywords,
// recurse into every schema position, then flatten nullable unions and
// combinators on the way back up.
//
// NULL BRANCHES ARE REMOVED BEFORE RECURSION, and the order is not cosmetic.
// normalizeTypeUnion rewrites a `{"type":"null"}` node into
// `{"type":"object"}` (a lone null type carries no information for a tool
// call), so a null branch that has already been recursed into is no longer
// recognisable as one — the parent would keep it as a spurious "object"
// alternative. dropNullBranches therefore runs on the RAW list, and the
// per-key record of what it removed is what lets flattenNullable know a sole
// survivor should be inlined.
//
// Combinator flattening runs AFTER recursion, because a branch may itself need
// flattening before the parent can decide whether it merges.
func sanitizeSchemaNode(node map[string]any) map[string]any {
	out := make(map[string]any, len(node))
	hadNull := map[string]bool{}
	for k, v := range node {
		if droppedSchemaKeywords[k] {
			continue
		}
		switch {
		case k == "additionalProperties":
			// `additionalProperties: true` is the JSON Schema default, and the
			// explicit form is what strict validators reject. Dropping it says
			// the same thing in a form everyone accepts.
			if b, isBool := v.(bool); isBool && b {
				continue
			}
			out[k] = sanitizeSchemaValue(v)
		case k == "required":
			// A boolean `required` inside a property definition is a common
			// hand-authoring mistake (the real keyword lives on the parent and
			// takes a list). It is meaningless where it sits, so it goes.
			if _, isBool := v.(bool); isBool {
				continue
			}
			out[k] = v
		case k == "pattern":
			if s, isStr := v.(string); isStr {
				out[k] = expandRegexShorthands(s)
			} else {
				out[k] = v
			}
		case schemaListKeys[k]:
			kept, dropped := dropNullBranches(v)
			hadNull[k] = dropped
			out[k] = sanitizeSchemaValue(kept)
		case schemaValueKeys[k]:
			out[k] = sanitizeSchemaValue(v)
		case schemaMapKeys[k]:
			m, isMap := v.(map[string]any)
			if !isMap {
				out[k] = v
				continue
			}
			sub := make(map[string]any, len(m))
			for name, s := range m {
				sub[name] = sanitizeSchemaValue(s)
			}
			out[k] = sub
		default:
			out[k] = v
		}
	}
	out = flattenNullable(out, hadNull)
	return flattenCombinators(out)
}

// dropNullBranches removes the null-only members of a raw combinator list,
// reporting whether anything was removed. A non-list value passes through
// untouched with false.
func dropNullBranches(v any) (any, bool) {
	list, ok := v.([]any)
	if !ok {
		return v, false
	}
	kept := make([]any, 0, len(list))
	for _, e := range list {
		if isNullSchema(e) {
			continue
		}
		kept = append(kept, e)
	}
	return kept, len(kept) != len(list)
}

// isNullSchema reports whether s accepts only JSON null — the branch a
// generated optional/pointer field contributes to an `anyOf`.
func isNullSchema(s any) bool {
	m, ok := s.(map[string]any)
	if !ok {
		return false
	}
	switch t := m["type"].(type) {
	case string:
		return t == "null"
	case []any:
		if len(t) == 0 {
			return false
		}
		for _, e := range t {
			if str, isStr := e.(string); !isStr || str != "null" {
				return false
			}
		}
		return true
	}
	return false
}

// flattenNullable finishes the null removal dropNullBranches started, and
// removes JSON `null` from the node's own type union.
//
// hadNull says which combinator keys actually lost a branch. That distinction
// matters: a sole surviving branch is inlined into the parent ONLY when a null
// sibling was removed. A single-branch `anyOf` that never had a null sibling is
// something the author wrote deliberately, and collapsing it would silently
// change a schema no provider objected to.
//
// Optionality survives the rewrite: a parameter is optional because it is
// absent from the parent's `required` list, not because its own schema admits
// null. Gateways that front Gemini-shaped backends reject the null branch in a
// function declaration outright, so keeping it costs the whole request and
// buys nothing.
func flattenNullable(node map[string]any, hadNull map[string]bool) map[string]any {
	for _, key := range []string{"anyOf", "oneOf"} {
		if !hadNull[key] {
			continue
		}
		kept, ok := node[key].([]any)
		if !ok {
			continue
		}
		if len(kept) == 1 {
			if branch, isMap := kept[0].(map[string]any); isMap {
				return normalizeTypeUnion(mergeBranchOverSiblings(branch, node, key))
			}
		}
		if len(kept) == 0 {
			delete(node, key)
		}
	}
	return normalizeTypeUnion(node)
}

// mergeBranchOverSiblings folds a sole surviving combinator branch into its
// parent: the branch's own keywords win, and the parent's other keywords (the
// `description` a generator puts NEXT to the combinator, most often) fill the
// gaps.
func mergeBranchOverSiblings(branch, parent map[string]any, combinatorKey string) map[string]any {
	merged := make(map[string]any, len(branch)+len(parent))
	for k, v := range branch {
		merged[k] = v
	}
	for k, v := range parent {
		if k == combinatorKey {
			continue
		}
		if _, exists := merged[k]; !exists {
			merged[k] = v
		}
	}
	return merged
}

// normalizeTypeUnion collapses a `type` array to the single form providers
// accept. A union with one non-null member becomes that member; a union with
// several keeps the list minus null (several providers do accept a plain
// union, and narrowing to one member would reject valid arguments); a
// null-only type becomes "object", matching QwenPaw's choice — a position that
// accepts only null carries no information for a tool call, and "object" is the
// permissive form every provider parses.
func normalizeTypeUnion(node map[string]any) map[string]any {
	raw, ok := node["type"]
	if !ok {
		return node
	}
	if s, isStr := raw.(string); isStr {
		if s == "null" {
			node["type"] = "object"
		}
		return node
	}
	list, isList := raw.([]any)
	if !isList {
		return node
	}
	kept := make([]any, 0, len(list))
	for _, e := range list {
		if s, isStr := e.(string); isStr && s == "null" {
			continue
		}
		kept = append(kept, e)
	}
	switch len(kept) {
	case 0:
		node["type"] = "object"
	case 1:
		node["type"] = kept[0]
	default:
		node["type"] = kept
	}
	return node
}

// flattenCombinators removes `allOf` and `oneOf`.
//
// `allOf` is an INTERSECTION, so a merge is only sound when every branch
// contributes keywords that combine by union (properties, required) or agree
// (type). allOfMergeable decides that; when it says no, the keyword is dropped
// and the position loosens rather than acquiring semantics nobody checked.
//
// `oneOf` is an EXCLUSIVE union with no portable equivalent. A single branch
// inlines exactly. Several branches that agree on `type` collapse to that type
// — the model still learns the shape, it just loses the discrimination. Several
// branches that disagree leave the position untyped, i.e. "anything".
//
// `anyOf` is deliberately NOT flattened: OpenAI, Anthropic and every gateway
// tested against accept it, and it is the keyword the null-flattening pass
// above already reduces to its useful core.
func flattenCombinators(node map[string]any) map[string]any {
	if branches, ok := node["allOf"].([]any); ok {
		delete(node, "allOf")
		if allOfMergeable(branches) {
			for _, b := range branches {
				node = mergeAllOfBranch(node, b.(map[string]any))
			}
		}
	}
	if branches, ok := node["oneOf"].([]any); ok {
		switch {
		case len(branches) == 1:
			if branch, isMap := branches[0].(map[string]any); isMap {
				return mergeBranchOverSiblings(branch, node, "oneOf")
			}
			delete(node, "oneOf")
		default:
			delete(node, "oneOf")
			if t := commonBranchType(branches); t != "" {
				if _, hasType := node["type"]; !hasType {
					node["type"] = t
				}
			}
		}
	}
	return node
}

// allOfMergeable reports whether every branch is an object map restricted to
// mergeableAllOfKeys. An empty list is not mergeable — there is nothing to
// merge and the caller should simply drop the keyword.
func allOfMergeable(branches []any) bool {
	if len(branches) == 0 {
		return false
	}
	for _, b := range branches {
		m, ok := b.(map[string]any)
		if !ok {
			return false
		}
		for k := range m {
			if !mergeableAllOfKeys[k] {
				return false
			}
		}
	}
	return true
}

// mergeAllOfBranch folds one mergeable `allOf` branch into its parent:
// `properties` union (parent wins a collision, because the parent's own
// declaration is the more specific one), `required` union with duplicates
// removed and a stable order, everything else filled in only where absent.
func mergeAllOfBranch(node, branch map[string]any) map[string]any {
	for k, v := range branch {
		switch k {
		case "properties":
			src, isMap := v.(map[string]any)
			if !isMap {
				continue
			}
			dst, _ := node["properties"].(map[string]any)
			if dst == nil {
				dst = map[string]any{}
			}
			for name, s := range src {
				if _, exists := dst[name]; !exists {
					dst[name] = s
				}
			}
			node["properties"] = dst
		case "required":
			node["required"] = mergeRequired(node["required"], v)
		default:
			if _, exists := node[k]; !exists {
				node[k] = v
			}
		}
	}
	return node
}

// mergeRequired unions two `required` lists, de-duplicating and sorting so the
// result is stable across map iteration order (an unstable tool schema breaks
// prompt caching on every provider that hashes the request).
func mergeRequired(a, b any) []any {
	seen := map[string]bool{}
	add := func(v any) {
		list, ok := v.([]any)
		if !ok {
			return
		}
		for _, e := range list {
			if s, isStr := e.(string); isStr {
				seen[s] = true
			}
		}
	}
	add(a)
	add(b)
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]any, 0, len(names))
	for _, name := range names {
		out = append(out, name)
	}
	return out
}

// commonBranchType returns the `type` every branch agrees on, or "" when they
// disagree or any branch is untyped.
func commonBranchType(branches []any) string {
	common := ""
	for _, b := range branches {
		m, ok := b.(map[string]any)
		if !ok {
			return ""
		}
		t, isStr := m["type"].(string)
		if !isStr || t == "" {
			return ""
		}
		if common == "" {
			common = t
			continue
		}
		if common != t {
			return ""
		}
	}
	return common
}

// expandRegexShorthands substitutes the ECMA-262 shorthand escapes with their
// character-class equivalents. See regexShorthands for why.
func expandRegexShorthands(pattern string) string {
	return regexShorthandRe.ReplaceAllStringFunc(pattern, func(m string) string {
		if repl, ok := regexShorthands[m]; ok {
			return repl
		}
		return m
	})
}

// SanitizeToolInfo returns a copy of ti whose parameter schema has been through
// SanitizeToolSchema, or ti itself when the tool declares no parameters (there
// is nothing to sanitize and rebuilding it would only risk losing fields).
//
// An error from any step is returned rather than swallowed: the caller decides
// whether an unsanitizable tool should be sent unchanged (SanitizeToolInfos'
// choice) or the whole request refused. Silently returning the original here
// would make a systematic conversion failure indistinguishable from a schema
// that simply needed no work.
func SanitizeToolInfo(ti *schema.ToolInfo) (*schema.ToolInfo, error) {
	if ti == nil || ti.ParamsOneOf == nil {
		return ti, nil
	}
	js, err := ti.ToJSONSchema()
	if err != nil {
		return nil, fmt.Errorf("eino: tool %q: to json schema: %w", ti.Name, err)
	}
	if js == nil {
		return ti, nil
	}
	raw, err := json.Marshal(js)
	if err != nil {
		return nil, fmt.Errorf("eino: tool %q: marshal schema: %w", ti.Name, err)
	}
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return nil, fmt.Errorf("eino: tool %q: decode schema: %w", ti.Name, err)
	}
	cleanRaw, err := json.Marshal(SanitizeToolSchema(generic))
	if err != nil {
		return nil, fmt.Errorf("eino: tool %q: encode sanitized schema: %w", ti.Name, err)
	}
	clean := &jsonschema.Schema{}
	if err := json.Unmarshal(cleanRaw, clean); err != nil {
		return nil, fmt.Errorf("eino: tool %q: reparse sanitized schema: %w", ti.Name, err)
	}
	out := *ti
	out.ParamsOneOf = schema.NewParamsOneOfByJSONSchema(clean)
	return &out, nil
}

// SanitizeToolInfos sanitizes every tool, returning a new slice. A tool whose
// schema cannot be converted is forwarded UNCHANGED and named in the returned
// error, so one bad tool degrades to the previous behaviour for itself instead
// of removing the whole tool set from the turn.
//
// The error is advisory — every element of the returned slice is usable — which
// is why it comes back alongside a full result rather than instead of one.
func SanitizeToolInfos(tools []*schema.ToolInfo) ([]*schema.ToolInfo, error) {
	if len(tools) == 0 {
		return tools, nil
	}
	out := make([]*schema.ToolInfo, 0, len(tools))
	var failed []string
	for _, ti := range tools {
		clean, err := SanitizeToolInfo(ti)
		if err != nil {
			name := "<nil>"
			if ti != nil {
				name = ti.Name
			}
			failed = append(failed, name)
			out = append(out, ti)
			continue
		}
		out = append(out, clean)
	}
	if len(failed) > 0 {
		return out, fmt.Errorf("eino: tool schema sanitization failed for %s (forwarded unchanged)",
			strings.Join(failed, ", "))
	}
	return out, nil
}
