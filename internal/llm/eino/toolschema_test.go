package eino

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/eino-contrib/jsonschema"

	"github.com/cloudwego/eino/schema"
)

// mustJSON decodes a JSON literal into a generic map, failing the test on a
// malformed fixture.
func mustJSON(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("bad fixture: %v", err)
	}
	return m
}

// TestSanitizeToolSchema is the M6 table: each row is a schema shape a gateway
// has been observed to reject, and the exact portable form it must become.
func TestSanitizeToolSchema(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "inlines a local $ref and drops $defs",
			in: `{"type":"object","properties":{"target":{"$ref":"#/$defs/Point"}},
			      "$defs":{"Point":{"type":"object","properties":{"x":{"type":"integer"}}}}}`,
			want: `{"type":"object","properties":{"target":{"type":"object","properties":{"x":{"type":"integer"}}}}}`,
		},
		{
			name: "inlines a draft-07 definitions ref",
			in: `{"type":"object","properties":{"p":{"$ref":"#/definitions/S"}},
			      "definitions":{"S":{"type":"string"}}}`,
			want: `{"type":"object","properties":{"p":{"type":"string"}}}`,
		},
		{
			name: "ref sibling annotations win over the resolved body",
			in: `{"type":"object","properties":{"p":{"$ref":"#/$defs/S","description":"local"}},
			      "$defs":{"S":{"type":"string","description":"shared"}}}`,
			want: `{"type":"object","properties":{"p":{"type":"string","description":"local"}}}`,
		},
		{
			name: "circular ref becomes an empty schema instead of hanging",
			in: `{"type":"object","properties":{"n":{"$ref":"#/$defs/Node"}},
			      "$defs":{"Node":{"type":"object","properties":{"next":{"$ref":"#/$defs/Node"}}}}}`,
			want: `{"type":"object","properties":{"n":{"type":"object","properties":{"next":{}}}}}`,
		},
		{
			name: "external $ref is left alone",
			in:   `{"type":"object","properties":{"p":{"$ref":"https://example.com/s.json"}}}`,
			want: `{"type":"object","properties":{"p":{"$ref":"https://example.com/s.json"}}}`,
		},
		{
			name: "drops $schema and $id",
			in:   `{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"urn:x","type":"object"}`,
			want: `{"type":"object"}`,
		},
		{
			name: "flattens a nullable type union to the single member",
			in:   `{"type":"object","properties":{"p":{"type":["string","null"]}}}`,
			want: `{"type":"object","properties":{"p":{"type":"string"}}}`,
		},
		{
			name: "keeps a multi-member union minus null",
			in:   `{"type":"object","properties":{"p":{"type":["string","integer","null"]}}}`,
			want: `{"type":"object","properties":{"p":{"type":["string","integer"]}}}`,
		},
		{
			name: "a null-only type becomes object",
			in:   `{"type":"object","properties":{"p":{"type":"null"}}}`,
			want: `{"type":"object","properties":{"p":{"type":"object"}}}`,
		},
		{
			name: "drops the null branch of an anyOf and inlines the survivor",
			in: `{"type":"object","properties":{"p":{"description":"d",
			      "anyOf":[{"type":"string"},{"type":"null"}]}}}`,
			want: `{"type":"object","properties":{"p":{"type":"string","description":"d"}}}`,
		},
		{
			name: "keeps a multi-branch anyOf minus null",
			in: `{"type":"object","properties":{"p":{
			      "anyOf":[{"type":"string"},{"type":"integer"},{"type":"null"}]}}}`,
			want: `{"type":"object","properties":{"p":{"anyOf":[{"type":"string"},{"type":"integer"}]}}}`,
		},
		{
			name: "merges a mergeable allOf into the parent",
			in: `{"type":"object","allOf":[{"properties":{"a":{"type":"string"}},"required":["a"]},
			      {"properties":{"b":{"type":"integer"}},"required":["b"]}]}`,
			want: `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"integer"}},
			        "required":["a","b"]}`,
		},
		{
			// The conditional branch is emptied by the keyword-drop pass BEFORE
			// mergeability is judged, so what remains is mergeable and the
			// useful half survives. Judging mergeability on the raw branch
			// would throw away `a` to avoid semantics that had already been
			// removed.
			name: "an allOf branch of dropped keywords empties out and the rest merges",
			in: `{"type":"object","allOf":[{"properties":{"a":{"type":"string"}}},
			      {"if":{"type":"string"},"then":{"minLength":1}}]}`,
			want: `{"type":"object","properties":{"a":{"type":"string"}}}`,
		},
		{
			name: "drops an unmergeable allOf rather than inventing semantics",
			in: `{"type":"object","allOf":[{"properties":{"a":{"type":"string"}}},
			      {"anyOf":[{"type":"string"},{"type":"integer"}]}]}`,
			want: `{"type":"object"}`,
		},
		{
			name: "inlines a single-branch oneOf",
			in:   `{"type":"object","properties":{"p":{"oneOf":[{"type":"string","minLength":1}]}}}`,
			want: `{"type":"object","properties":{"p":{"type":"string","minLength":1}}}`,
		},
		{
			name: "collapses a same-type oneOf to that type",
			in: `{"type":"object","properties":{"p":{
			      "oneOf":[{"type":"string","format":"email"},{"type":"string","format":"uri"}]}}}`,
			want: `{"type":"object","properties":{"p":{"type":"string"}}}`,
		},
		{
			name: "drops a disagreeing oneOf and leaves the position loose",
			in: `{"type":"object","properties":{"p":{"description":"d",
			      "oneOf":[{"type":"string"},{"type":"integer"}]}}}`,
			want: `{"type":"object","properties":{"p":{"description":"d"}}}`,
		},
		{
			name: "boolean schema true becomes {} at a schema position",
			in:   `{"type":"object","properties":{"p":{"type":"array","items":true}}}`,
			want: `{"type":"object","properties":{"p":{"type":"array","items":{}}}}`,
		},
		{
			name: "boolean schema false becomes not-anything",
			in:   `{"type":"object","properties":{"p":{"type":"array","items":false}}}`,
			want: `{"type":"object","properties":{"p":{"type":"array","items":{"not":{}}}}}`,
		},
		{
			name: "boolean-valued keyword uniqueItems stays a boolean",
			in:   `{"type":"object","properties":{"p":{"type":"array","uniqueItems":true}}}`,
			want: `{"type":"object","properties":{"p":{"type":"array","uniqueItems":true}}}`,
		},
		{
			name: "additionalProperties true is dropped, false is kept",
			in: `{"type":"object","additionalProperties":true,
			      "properties":{"p":{"type":"object","additionalProperties":false}}}`,
			want: `{"type":"object","properties":{"p":{"type":"object","additionalProperties":{"not":{}}}}}`,
		},
		{
			name: "boolean required inside a property is dropped",
			in:   `{"type":"object","properties":{"p":{"type":"string","required":true}}}`,
			want: `{"type":"object","properties":{"p":{"type":"string"}}}`,
		},
		{
			name: "list required is preserved",
			in:   `{"type":"object","properties":{"p":{"type":"string"}},"required":["p"]}`,
			want: `{"type":"object","properties":{"p":{"type":"string"}},"required":["p"]}`,
		},
		{
			name: "expands regex shorthands in pattern",
			in:   `{"type":"object","properties":{"p":{"type":"string","pattern":"^\\d+\\w$"}}}`,
			want: `{"type":"object","properties":{"p":{"type":"string","pattern":"^[0-9]+[a-zA-Z0-9_]$"}}}`,
		},
		{
			name: "drops draft-2020 structural keywords providers ignore",
			in: `{"type":"object","dependentRequired":{"a":["b"]},"propertyNames":{"pattern":"^x"},
			      "prefixItems":[{"type":"string"}],"unevaluatedProperties":false}`,
			want: `{"type":"object"}`,
		},
		{
			name: "an untyped root gains type object",
			in:   `{"properties":{"p":{"type":"string"}}}`,
			want: `{"type":"object","properties":{"p":{"type":"string"}}}`,
		},
		{
			name: "nested arrays of objects recurse",
			in: `{"type":"object","properties":{"list":{"type":"array",
			      "items":{"type":["object","null"],"properties":{"x":{"$ref":"#/$defs/S"}}}}},
			      "$defs":{"S":{"type":["string","null"]}}}`,
			want: `{"type":"object","properties":{"list":{"type":"array",
			        "items":{"type":"object","properties":{"x":{"type":"string"}}}}}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SanitizeToolSchema(mustJSON(t, tc.in))
			want := mustJSON(t, tc.want)
			if !reflect.DeepEqual(got, want) {
				gj, _ := json.Marshal(got)
				wj, _ := json.Marshal(want)
				t.Errorf("SanitizeToolSchema mismatch\n got: %s\nwant: %s", gj, wj)
			}
		})
	}
}

// TestSanitizeToolSchemaDoesNotMutateInput pins that the caller's schema
// survives untouched: the same map is shared across providers and only some of
// them want the rewrite.
func TestSanitizeToolSchemaDoesNotMutateInput(t *testing.T) {
	in := mustJSON(t, `{"type":"object","$schema":"x","properties":{"p":{"type":["string","null"]}},
	                    "$defs":{"S":{"type":"string"}}}`)
	before, _ := json.Marshal(in)
	SanitizeToolSchema(in)
	after, _ := json.Marshal(in)
	if string(before) != string(after) {
		t.Errorf("input mutated:\nbefore %s\nafter  %s", before, after)
	}
}

// TestSanitizeToolSchemaEmptyInput pins that a no-parameter tool still gets an
// object schema — every OpenAI-compatible endpoint requires one.
func TestSanitizeToolSchemaEmptyInput(t *testing.T) {
	for _, in := range []map[string]any{nil, {}} {
		got := SanitizeToolSchema(in)
		if !reflect.DeepEqual(got, map[string]any{"type": "object"}) {
			t.Errorf("SanitizeToolSchema(%v) = %v, want {type:object}", in, got)
		}
	}
}

// TestSanitizeToolInfoRoundTrip drives the real ToolInfo path: a schema built
// the way eino builds one, through the sanitizer, and back into a ToolInfo the
// adapters can serialize.
func TestSanitizeToolInfoRoundTrip(t *testing.T) {
	raw := mustJSON(t, `{"$schema":"https://json-schema.org/draft/2020-12/schema",
	  "type":"object",
	  "properties":{"path":{"$ref":"#/$defs/P"},"limit":{"type":["integer","null"]}},
	  "required":["path"],
	  "$defs":{"P":{"type":"string","description":"a path"}}}`)
	rawBytes, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	js := &jsonschema.Schema{}
	if err := json.Unmarshal(rawBytes, js); err != nil {
		t.Fatal(err)
	}
	ti := &schema.ToolInfo{Name: "fs_read", Desc: "read", ParamsOneOf: schema.NewParamsOneOfByJSONSchema(js)}

	out, err := SanitizeToolInfo(ti)
	if err != nil {
		t.Fatalf("SanitizeToolInfo: %v", err)
	}
	if out == ti {
		t.Fatal("SanitizeToolInfo returned the input pointer; it must copy")
	}
	gotSchema, err := out.ToJSONSchema()
	if err != nil {
		t.Fatalf("ToJSONSchema: %v", err)
	}
	gotBytes, err := json.Marshal(gotSchema)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(gotBytes, &got); err != nil {
		t.Fatal(err)
	}
	if _, has := got["$defs"]; has {
		t.Error("$defs survived sanitization")
	}
	if _, has := got["$schema"]; has {
		t.Error("$schema survived sanitization")
	}
	props, _ := got["properties"].(map[string]any)
	path, _ := props["path"].(map[string]any)
	if path["type"] != "string" {
		t.Errorf("path.type = %v, want string (ref not inlined)", path["type"])
	}
	if path["description"] != "a path" {
		t.Errorf("path.description = %v, want the inlined def's description", path["description"])
	}
	limit, _ := props["limit"].(map[string]any)
	if limit["type"] != "integer" {
		t.Errorf("limit.type = %v, want integer (null branch not flattened)", limit["type"])
	}
	// The original must be untouched: it is still the schema other providers see.
	origSchema, _ := ti.ToJSONSchema()
	origBytes, _ := json.Marshal(origSchema)
	if !json.Valid(origBytes) || !containsKey(t, origBytes, "$defs") {
		t.Error("the original ToolInfo lost its $defs; sanitization must not mutate the input")
	}
}

// containsKey reports whether the top level of a JSON object has key.
func containsKey(t *testing.T, raw []byte, key string) bool {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return false
	}
	_, ok := m[key]
	return ok
}

// TestSanitizeToolInfoNoParams pins that a parameterless tool is returned
// unchanged rather than rebuilt (rebuilding risks losing fields for no gain).
func TestSanitizeToolInfoNoParams(t *testing.T) {
	ti := &schema.ToolInfo{Name: "ping"}
	out, err := SanitizeToolInfo(ti)
	if err != nil {
		t.Fatalf("SanitizeToolInfo: %v", err)
	}
	if out != ti {
		t.Error("a parameterless tool should be returned as-is")
	}
	out, err = SanitizeToolInfo(nil)
	if err != nil || out != nil {
		t.Errorf("SanitizeToolInfo(nil) = (%v, %v), want (nil, nil)", out, err)
	}
}

// TestSanitizeToolInfosForwardsAll pins the slice-level contract: a full result
// always comes back, so one unconvertible tool cannot remove the tool set from
// the turn.
func TestSanitizeToolInfosForwardsAll(t *testing.T) {
	js := &jsonschema.Schema{}
	if err := json.Unmarshal([]byte(`{"type":"object"}`), js); err != nil {
		t.Fatal(err)
	}
	in := []*schema.ToolInfo{
		{Name: "a", ParamsOneOf: schema.NewParamsOneOfByJSONSchema(js)},
		{Name: "b"},
	}
	out, err := SanitizeToolInfos(in)
	if err != nil {
		t.Fatalf("SanitizeToolInfos: %v", err)
	}
	if len(out) != len(in) {
		t.Fatalf("len(out) = %d, want %d", len(out), len(in))
	}
	if empty, _ := SanitizeToolInfos(nil); empty != nil {
		t.Errorf("SanitizeToolInfos(nil) = %v, want nil", empty)
	}
}

// TestExpandRegexShorthands pins the lossless substitutions individually, so a
// future edit to the table is caught at the mapping and not only through a
// whole-schema fixture.
func TestExpandRegexShorthands(t *testing.T) {
	cases := []struct{ in, want string }{
		{`\d`, "[0-9]"},
		{`\D`, "[^0-9]"},
		{`\w`, "[a-zA-Z0-9_]"},
		{`\W`, "[^a-zA-Z0-9_]"},
		{`\s`, "[\t\n\r\f\v ]"},
		{`\S`, "[^\t\n\r\f\v ]"},
		{`^\d{3}-\w+$`, "^[0-9]{3}-[a-zA-Z0-9_]+$"},
		{`no shorthands`, "no shorthands"},
		{`[0-9]`, "[0-9]"},
	}
	for _, tc := range cases {
		if got := expandRegexShorthands(tc.in); got != tc.want {
			t.Errorf("expandRegexShorthands(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestMergeRequiredIsStable pins the sorted, de-duplicated union: an unstable
// tool schema breaks prompt caching on every provider that hashes the request.
func TestMergeRequiredIsStable(t *testing.T) {
	got := mergeRequired([]any{"b", "a"}, []any{"a", "c"})
	want := []any{"a", "b", "c"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mergeRequired = %v, want %v", got, want)
	}
	if again := mergeRequired([]any{"b", "a"}, []any{"a", "c"}); !reflect.DeepEqual(again, got) {
		t.Errorf("mergeRequired is not deterministic: %v then %v", got, again)
	}
}
