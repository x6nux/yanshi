package v1

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	sdkschema "github.com/x6nux/yanshi/sdk/schema"
)

// The v1 wire contract is stated in four places: the Go structs this package
// serves, the JSON Schema clients validate against, the TypeScript interfaces
// in sdk/ts, and the pydantic models in sdk/python. Nothing compared them —
// grepping every Go test, sdk/ts/tests and sdk/python/tests for a cross-source
// comparison returned zero hits — so the four drifted independently and the
// SDKs silently lost TurnStartParams.images.
//
// This test lives on the GO side on purpose. `go test ./...` runs
// unconditionally on every platform in CI; the Node and Python toolchains are
// optional steps. Parity failures have to be visible on a machine with neither
// installed, or the gate only guards the branches that were already careful.
//
// It reads sdk/ts/v1.ts and sdk/python's generated.py as TEXT rather than
// invoking their toolchains, for the same reason.

// paritySource names one of the three non-Go statements of the contract.
type paritySource string

const (
	srcSchema paritySource = "schema"
	srcTS     paritySource = "ts"
	srcPython paritySource = "python"
)

// parityTypes are the contract types every source must state identically.
//
// Types that exist in only one source are not listed: ContextItem, FileChange,
// Range, SelectionContext and OpenFileContext are D2-provisional IDE-context
// shapes with no Go counterpart by design (sdk/schema/CONTRACT_HANDOFF.md),
// and ItemUpdatedNotification is a TS-side transport envelope. Listing them
// would turn a documented design decision into 30 exception entries.
var parityTypes = map[string]any{
	"Thread":                Thread{},
	"Turn":                  Turn{},
	"Item":                  Item{},
	"ThreadStartParams":     ThreadStartParams{},
	"ThreadResumeParams":    ThreadResumeParams{},
	"ThreadInterruptParams": ThreadInterruptParams{},
	"TurnStartParams":       TurnStartParams{},
	"ThreadStartResponse":   ThreadStartResponse{},
	"ThreadResumeResponse":  ThreadResumeResponse{},
	"TurnStartResponse":     TurnStartResponse{},
	"InterruptResponse":     InterruptResponse{},
	"Capabilities":          Capabilities{},
}

// intentionalDifferences enumerates every field a source states differently
// from Go, with the reason. A key is "<source>:<sign><Type>.<field>" where "+"
// means the source has a field Go does not and "-" means Go has one it lacks.
//
// The direction is part of the key because the two mean opposite things: "+"
// is usually a forward-looking field a client tolerates, "-" is usually a
// feature the SDKs cannot reach.
//
// Same semantics as the repo's other debt tables: an entry that no longer
// describes a real difference FAILS, so the table can only shrink. Adding a
// row is a contract decision and belongs in review, not in a test edit.
var intentionalDifferences = map[string]string{
	"schema:+Item.fileChange": "D2-provisional IDE diff payload. D1 ignores unknown fields; " +
		"documented in sdk/schema/CONTRACT_HANDOFF.md.",
	"python:+Item.fileChange": "Mirrors the schema's D2-provisional field so a Python client " +
		"can round-trip a D2 payload without losing it.",
	"schema:+TurnStartParams.context": "D2-provisional IDE context (open files, selection). " +
		"Same handoff decision as Item.fileChange.",
	"python:+TurnStartParams.context": "Mirrors the schema's D2-provisional field.",

	"ts:-TurnStartParams.images": "The image-attachment path is being wired in W1 (ApplyImages " +
		"into the turn path). Until it is, sending images from the SDK would reach a " +
		"parameter the server does not yet read. Close this row when W1 lands.",
	"python:-TurnStartParams.images": "Same as the TS row: pending W1.",
}

// goFields returns the wire names of a struct's fields, honouring json tags.
func goFields(v any) map[string]bool {
	out := map[string]bool{}
	for f := range reflect.TypeOf(v).Fields() {
		if !f.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		switch name {
		case "-":
			continue
		case "":
			name = f.Name
		}
		out[name] = true
	}
	return out
}

// schemaFields returns the property names of one $defs entry, or nil when the
// schema does not describe that type at all.
func schemaFields(t *testing.T, typeName string) map[string]bool {
	t.Helper()
	var doc struct {
		Defs map[string]struct {
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(sdkschema.V1(), &doc); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}
	def, ok := doc.Defs[typeName]
	if !ok {
		return nil
	}
	out := map[string]bool{}
	for k := range def.Properties {
		out[k] = true
	}
	return out
}

func readSDK(t *testing.T, parts ...string) string {
	t.Helper()
	p := filepath.Join(append([]string{"..", "..", "..", "sdk"}, parts...)...)
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(data)
}

var (
	tsFieldRe = regexp.MustCompile(`(?m)^\s{2}(\w+)\??:`)
	pyFieldRe = regexp.MustCompile(`(?m)^\s{4}(\w+):`)
	pyAliasRe = regexp.MustCompile(`alias="([^"]+)"`)
)

// tsFields extracts the property names of one exported interface.
func tsFields(src, typeName string) map[string]bool {
	body, ok := blockAfter(src, "export interface "+typeName+" {", "\n}")
	if !ok {
		return nil
	}
	out := map[string]bool{}
	for _, m := range tsFieldRe.FindAllStringSubmatch(body, -1) {
		out[m[1]] = true
	}
	return out
}

// pyFields extracts the WIRE names of one pydantic model's fields: the alias
// when present, the attribute name otherwise.
//
// Reading the alias rather than the attribute name is the whole point — the
// Python attribute for threadId is thread_id, and comparing attribute names
// against Go's json tags would report every camelCase field as a difference.
func pyFields(src, typeName string) map[string]bool {
	body, ok := blockAfter(src, "\nclass "+typeName+"(ModelBase):", "\n\n\n")
	if !ok {
		return nil
	}
	out := map[string]bool{}
	for line := range strings.SplitSeq(body, "\n") {
		m := pyFieldRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if alias := pyAliasRe.FindStringSubmatch(line); alias != nil {
			out[alias[1]] = true
			continue
		}
		// A trailing underscore avoids shadowing a Python builtin (range_);
		// the wire name is the bare one.
		out[strings.TrimSuffix(m[1], "_")] = true
	}
	return out
}

// blockAfter returns the text between a header and the next terminator, which
// is enough structure for two hand-written declaration files and avoids
// carrying a TypeScript and a Python parser in a Go test.
func blockAfter(src, header, terminator string) (string, bool) {
	_, rest, found := strings.Cut(src, header)
	if !found {
		return "", false
	}
	if body, _, ok := strings.Cut(rest, terminator); ok {
		return body, true
	}
	return rest, true
}

// TestContractParityAcrossFourSources compares every contract type's field set
// across Go, the JSON Schema, the TypeScript client and the Python client.
func TestContractParityAcrossFourSources(t *testing.T) {
	tsSrc := readSDK(t, "ts", "v1.ts")
	pySrc := readSDK(t, "python", "src", "yanshi_sdk", "generated.py")

	used := map[string]bool{}
	names := make([]string, 0, len(parityTypes))
	for name := range parityTypes {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, typeName := range names {
		want := goFields(parityTypes[typeName])
		sources := map[paritySource]map[string]bool{
			srcSchema: schemaFields(t, typeName),
			srcTS:     tsFields(tsSrc, typeName),
			srcPython: pyFields(pySrc, typeName),
		}
		for _, src := range []paritySource{srcSchema, srcTS, srcPython} {
			got := sources[src]
			if got == nil {
				t.Errorf("%s does not describe %s at all — a contract type stated in Go "+
					"and absent from a client is not a field-level difference, it is a "+
					"missing type", src, typeName)
				continue
			}
			for _, d := range diffKeys(string(src), typeName, want, got) {
				if reason, ok := intentionalDifferences[d.key]; ok {
					used[d.key] = true
					_ = reason
					continue
				}
				t.Errorf("%s\n  add a row to intentionalDifferences with the reason, "+
					"or fix the source", d.msg)
			}
		}
	}

	for key := range intentionalDifferences {
		if !used[key] {
			t.Errorf("intentionalDifferences[%q] no longer describes a real difference — "+
				"the sources agree now, so delete the row. A stale exemption is a "+
				"pre-authorisation for the difference to come back unnoticed.", key)
		}
	}
}

type parityDiff struct{ key, msg string }

// diffKeys reports each field present on exactly one side, in both directions.
func diffKeys(src, typeName string, goSide, other map[string]bool) []parityDiff {
	var out []parityDiff
	for f := range other {
		if !goSide[f] {
			out = append(out, parityDiff{
				key: src + ":+" + typeName + "." + f,
				msg: src + " states " + typeName + "." + f + ", Go does not",
			})
		}
	}
	for f := range goSide {
		if !other[f] {
			out = append(out, parityDiff{
				key: src + ":-" + typeName + "." + f,
				msg: "Go serves " + typeName + "." + f + ", " + src + " cannot express it",
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key < out[j].key })
	return out
}
