// Package main: cmd/gendocs config-skeleton generator.
//
// The -config mode reflects over internal/config.Config and emits a markdown
// skeleton table grouped by top-level YAML key. The table is the structural
// backbone of docs/user-guide/configuration.md: prose is written by hand around
// it, and the generated rows are gated in CI (Task 15 Advisory 4) so a Config
// struct field can never silently appear in code without a documentation row
// (or vice versa).
//
// Design notes:
//   - Keys are dotted paths (server.http_addr, security.sandbox.enabled) so the
//     key column is unambiguous across groups and matches the recursive yaml-tag
//     walk the CI consistency test performs.
//   - Slices and maps are leaves (we render []ProviderConfig, not every field of
//     ProviderConfig) so the skeleton stays a flat index, not a full expansion.
//   - time.Duration renders as "duration"; *bool renders as "*bool" so the
//     omit-vs-false distinction (SandboxConfig.Enabled, LSPConfig.Enabled) is
//     visible to operators reading the docs.
package main

import (
	"reflect"
	"strings"
	"time"

	"github.com/x6nux/yanshi/internal/config"
)

// configRow is one emitted key/type row.
type configRow struct {
	key string
	typ string
}

// configSkeletonBlockID is the BEGIN/END GENERATED id under which the skeleton
// is written in docs/user-guide/configuration.md.
const configSkeletonBlockID = "config-skeleton"

// RenderConfigSkeleton reflects over config.Config and returns the inner
// content of the config-skeleton block (no markers). It is deterministic and
// idempotent: the same Config type always yields byte-identical output.
//
// Group headings render as "###" (h3) so the block nests cleanly under a
// parent section in configuration.md without colliding with the hand-written
// "## <block>" prose headings.
func RenderConfigSkeleton() string {
	var sb strings.Builder
	t := reflect.TypeOf(config.Config{})
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		key := yamlKey(f)
		if key == "" {
			continue
		}
		sb.WriteString("### " + key + "\n\n")
		sb.WriteString("| key | type | 说明 |\n")
		sb.WriteString("|---|---|---|\n")
		for _, r := range leafRows(f.Type, key) {
			sb.WriteString("| " + r.key + " | " + r.typ + " | |\n")
		}
		sb.WriteString("\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}

// leafRows walks t and returns one row per leaf field, prefixing each key with
// prefix + ".". Structs recurse (except time.Duration); every other kind
// (scalar, slice, map, pointer) is a single leaf row.
func leafRows(t reflect.Type, prefix string) []configRow {
	dt := t
	for dt.Kind() == reflect.Ptr {
		dt = dt.Elem()
	}
	if dt.Kind() == reflect.Struct && dt != reflect.TypeOf(time.Duration(0)) {
		var rows []configRow
		for i := 0; i < dt.NumField(); i++ {
			sf := dt.Field(i)
			key := yamlKey(sf)
			if key == "" {
				continue
			}
			rows = append(rows, leafRows(sf.Type, prefix+"."+key)...)
		}
		// A struct with no exported/yaml-tagged fields still emits one row so
		// the group is never empty (and the struct's own tag is represented).
		if len(rows) == 0 {
			return []configRow{{prefix, typeDisplayName(t)}}
		}
		return rows
	}
	return []configRow{{prefix, typeDisplayName(t)}}
}

// typeDisplayName renders a Go type as a compact, YAML-friendly type string.
// Pointers are kept (so *bool reads as "*bool"), and time.Duration is rendered
// as "duration".
func typeDisplayName(t reflect.Type) string {
	if t.Kind() == reflect.Ptr {
		return "*" + typeDisplayName(t.Elem())
	}
	if t == reflect.TypeOf(time.Duration(0)) {
		return "duration"
	}
	switch t.Kind() {
	case reflect.Slice:
		return "[]" + typeDisplayName(t.Elem())
	case reflect.Map:
		return "map[" + typeDisplayName(t.Key()) + "]" + typeDisplayName(t.Elem())
	case reflect.Bool:
		return "bool"
	case reflect.Int, reflect.Int64:
		return "int"
	case reflect.Float32, reflect.Float64:
		return "float"
	case reflect.String:
		return "string"
	default:
		if name := t.Name(); name != "" {
			return name
		}
		return t.String()
	}
}

// yamlKey extracts the YAML key from a struct field's `yaml` tag. Returns "" for
// fields without a tag or with an explicit "-" (skip) tag.
func yamlKey(f reflect.StructField) string {
	tag := f.Tag.Get("yaml")
	if tag == "" {
		return ""
	}
	key := strings.Split(tag, ",")[0]
	if key == "-" {
		return ""
	}
	if key == "" {
		key = strings.ToLower(f.Name)
	}
	return key
}
