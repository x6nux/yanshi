// Package main: markdown generation for cmd/api-schema.
//
// The -markdown mode renders the canonical JSON Schema
// (internal/api/v1.SchemaBytes) into fenced markdown blocks guarded by
// BEGIN/END GENERATED markers, so docs/api/schema.md and docs/api/resources.md
// can be regenerated idempotently and gated in CI via `git diff --exit-code`.
//
// Two kinds of blocks are produced, both sourced from the same embedded
// schema document, so the markdown and the TypeScript output (the -out path)
// can never diverge into two competing sources of truth:
//
//   - api-schema-full: the entire JSON Schema, pretty-printed inside a ```json
//     fence. Backs docs/api/schema.md.
//   - api-defs:<Name>: one field-table per $defs entry (Thread/Turn/Item) plus
//     the params/responses shapes maintained below (mirroring the hand-written
//     TS interfaces and internal/api/v1/types.go). Backs docs/api/resources.md.
//
// The replace-or-append primitive itself (docgen.RewriteBlock) is shared with
// cmd/gendocs via internal/docgen so the two generators never drift on marker
// handling.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/x6nux/yanshi/internal/api/v1"
	"github.com/x6nux/yanshi/internal/docgen"
)

// beginMarker / endMarker are thin wrappers over docgen so the rest of this
// file reads in terms of "markers" without each call site spelling the prefix.
func beginMarker(id string) string { return docgen.Begin(id) }
func endMarker(id string) string   { return docgen.End(id) }

// renderBlocks maps every generated block id to its inner content (the text
// between the markers). RenderMarkdown stitches these into one document; the
// -markdown generator distributes them to files based on which markers a target
// already carries.
func renderBlocks(schema []byte) map[string]string {
	blocks := map[string]string{}
	blocks["api-schema-full"] = renderSchemaFull(schema)
	for name, table := range renderDefsTables(schema) {
		blocks["api-defs:"+name] = table
	}
	for _, d := range paramResponseDefs() {
		blocks["api-defs:"+d.Name] = renderDefTable(d)
	}
	return blocks
}

// defOrder is the stable display order for the resource/param/response tables
// in resources.md. $defs entries not listed here (none today) fall back to
// alphabetical via renderBlocks consumers.
var defOrder = []string{
	"Thread", "Turn", "Item",
	"ThreadStartParams", "ThreadResumeParams", "ThreadInterruptParams", "TurnStartParams",
	"ThreadStartResponse", "ThreadResumeResponse", "TurnStartResponse", "InterruptResponse",
}

// RenderMarkdown renders the full schema-full block followed by every
// api-defs:<Name> field table, each wrapped in its own BEGIN/END markers. The
// single returned string is the concatenation of all blocks in stable order;
// callers that need per-file distribution use renderBlocks + RewriteBlock.
func RenderMarkdown(schema []byte) string {
	blocks := renderBlocks(schema)
	var sb strings.Builder
	for _, id := range orderedBlockIDs(blocks) {
		sb.WriteString(docgen.Wrap(id, blocks[id]))
		sb.WriteString("\n")
	}
	return sb.String()
}

// orderedBlockIDs returns block ids with schema-full first, then defOrder, with
// any unmapped ids (e.g. extra $defs) appended alphabetically so new resources
// surface deterministically.
func orderedBlockIDs(blocks map[string]string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(blocks))
	if blocks["api-schema-full"] != "" {
		out = append(out, "api-schema-full")
		seen["api-schema-full"] = true
	}
	for _, name := range defOrder {
		id := "api-defs:" + name
		if blocks[id] != "" {
			out = append(out, id)
			seen[id] = true
		}
	}
	// Any leftover (unexpected) ids, sorted for determinism.
	leftover := make([]string, 0)
	for id := range blocks {
		if !seen[id] {
			leftover = append(leftover, id)
		}
	}
	sort.Strings(leftover)
	out = append(out, leftover...)
	return out
}

// renderSchemaFull pretty-prints the schema bytes inside a ```json fence. The
// re-encode normalises key order/whitespace so the committed block is stable
// regardless of how the caller marshalled the input bytes.
func renderSchemaFull(schema []byte) string {
	var doc any
	if err := json.Unmarshal(schema, &doc); err != nil {
		// SchemaBytes returns the embedded sdk/schema/v1 document, which the
		// Go test suite parses on every run; fall back to the raw bytes so the
		// block is never empty if it somehow does not.
		return "```json\n" + string(schema) + "\n```"
	}
	pretty, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "```json\n" + string(schema) + "\n```"
	}
	return "```json\n" + string(pretty) + "\n```"
}

// jsonSchemaTable renders one $defs entry as a field table. A property's type
// comes from its "type" keyword (or "const" for fixed values); required is
// derived from the sibling "required" array.
func renderDefsTables(schema []byte) map[string]string {
	var doc struct {
		Defs map[string]struct {
			Type       string                    `json:"type"`
			Required   []string                  `json:"required"`
			Properties map[string]map[string]any `json:"properties"`
		} `json:"$defs"`
	}
	out := map[string]string{}
	if err := json.Unmarshal(schema, &doc); err != nil {
		return out
	}
	for name, def := range doc.Defs {
		required := map[string]bool{}
		for _, r := range def.Required {
			required[r] = true
		}
		d := defTable{Name: name, Required: required}
		for _, prop := range sortedKeys(def.Properties) {
			d.Fields = append(d.Fields, defField{
				Name:     prop,
				Type:     propertyType(def.Properties[prop]),
				Required: required[prop],
			})
		}
		out[name] = renderDefTable(d)
	}
	return out
}

// propertyType renders the JSON Schema type of a property map. "const" wins
// (renders as the literal, e.g. `"v1"`); "type" is used otherwise; an object
// with neither (e.g. structuredResult's empty schema) renders as "any".
func propertyType(prop map[string]any) string {
	if c, ok := prop["const"]; ok {
		return fmt.Sprintf("%q", fmt.Sprint(c))
	}
	if t, ok := prop["type"]; ok {
		return fmt.Sprint(t)
	}
	if _, ok := prop["$ref"]; ok {
		return refName(fmt.Sprint(prop["$ref"]))
	}
	return "any"
}

// refName turns "#/$defs/Turn" into "Turn".
func refName(ref string) string {
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		return ref[i+1:]
	}
	return ref
}

// defField is one row of a generated field table.
type defField struct {
	Name     string
	Type     string
	Required bool
}

// defTable is a named, ordered field table.
type defTable struct {
	Name     string
	Required map[string]bool
	Fields   []defField
}

// renderDefTable renders a defTable as a markdown table with a fixed
// 字段/类型/required/说明 header. The 说明 column is intentionally left blank —
// prose lives outside the generated block so it can be edited freely.
func renderDefTable(d defTable) string {
	var sb strings.Builder
	sb.WriteString("| 字段 | 类型 | required | 说明 |\n")
	sb.WriteString("|---|---|---|---|\n")
	for _, f := range d.Fields {
		req := ""
		if f.Required {
			req = "yes"
		}
		fmt.Fprintf(&sb, "| %s | %s | %s | |\n", f.Name, f.Type, req)
	}
	return strings.TrimSuffix(sb.String(), "\n")
}

// paramResponseDefs is the hand-maintained field map for the request/response
// shapes that live in internal/api/v1/types.go but are NOT (yet) declared in
// the schema's $defs. It mirrors the hand-written TS interfaces in main.go
// and must be updated alongside them when the wire contract changes. The
// `images` field on TurnStartParams reflects Tier G (multimodal).
func paramResponseDefs() []defTable {
	stringType := func(name string, req bool) defField {
		return defField{Name: name, Type: "string", Required: req}
	}
	return []defTable{
		{
			Name: "ThreadStartParams",
			Fields: []defField{
				stringType("version", false), stringType("title", false),
				stringType("model", false), stringType("thinking", false),
			},
		},
		{
			Name: "ThreadResumeParams",
			Fields: []defField{
				stringType("version", false), stringType("threadId", true),
			},
		},
		{
			Name: "ThreadInterruptParams",
			Fields: []defField{
				stringType("version", false), stringType("threadId", true),
				stringType("turnId", false),
			},
		},
		{
			Name: "TurnStartParams",
			Fields: []defField{
				stringType("version", false), stringType("threadId", true),
				stringType("input", true), stringType("model", false),
				stringType("thinking", false),
				{Name: "outputSchema", Type: "object", Required: false},
				{Name: "images", Type: "array", Required: false},
			},
		},
		{
			Name: "ThreadStartResponse",
			Fields: []defField{
				stringType("version", true), {Name: "thread", Type: "Thread", Required: true},
			},
		},
		{
			Name: "ThreadResumeResponse",
			Fields: []defField{
				stringType("version", true), {Name: "thread", Type: "Thread", Required: true},
				{Name: "items", Type: "Item[]", Required: false},
			},
		},
		{
			Name: "TurnStartResponse",
			Fields: []defField{
				stringType("version", true), {Name: "turn", Type: "Turn", Required: true},
			},
		},
		{
			Name: "InterruptResponse",
			Fields: []defField{
				stringType("version", true), {Name: "ok", Type: "boolean", Required: true},
				stringType("threadId", true), stringType("turnId", false),
			},
		},
	}
}

// sortedKeys returns the keys of m in sorted order for deterministic table rows.
func sortedKeys(m map[string]map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// schemaDocHeader is the minimal prose prepended to a freshly-created
// docs/api/schema.md. It stays out of the generated block so the marker can be
// regenerated in place without touching the prose.
const schemaDocHeader = "# v1 JSON Schema\n\n" +
	"> 以下为 `sdk/schema/v1/agent-api.schema.json` 的完整 JSON Schema，由\n" +
	"> `go run ./cmd/api-schema -markdown` 经 `internal/api/v1/schema.go::SchemaBytes` 生成 ——\n" +
	"> 它返回的就是那个文件本身（`sdk/schema/schema.go::V1`），所以这两句同时为真。\n" +
	"> 修改 schema 后重生成；不要手改本区块。\n\n"

// runMarkdown is the -markdown generator entry point. It renders every block
// from the canonical schema and distributes them into path:
//
//   - When path does not exist, it is created as a schema-doc file (header +
//     the api-schema-full block). This is how docs/api/schema.md is bootstrapped.
//   - When path exists, every block whose marker is already present is rewritten
//     in place; blocks whose markers are absent are left alone so a file that
//     only carries api-defs:* markers (e.g. resources.md) is not polluted with
//     the schema-full dump.
//
// Both branches are idempotent, so CI can run the generator and assert no diff.
func runMarkdown(path string) error {
	blocks := renderBlocks(v1.SchemaBytes())

	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if os.IsNotExist(err) {
		// Bootstrap a schema-doc file: header + the full schema block.
		if mkErr := os.MkdirAll(filepath.Dir(path), 0o755); mkErr != nil {
			return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), mkErr)
		}
		return os.WriteFile(path, []byte(schemaDocHeader+docgen.Wrap("api-schema-full", blocks["api-schema-full"])+"\n"), 0o644)
	}
	existing := string(data)
	for _, id := range orderedBlockIDs(blocks) {
		if !strings.Contains(existing, beginMarker(id)) {
			continue // file does not carry this marker; do not append
		}
		if err := docgen.RewriteBlock(path, id, blocks[id]); err != nil {
			return err
		}
	}
	return nil
}
