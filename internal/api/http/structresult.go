package http

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// maxSchemaRetries caps how many times a turn is re-run when the model's output
// fails JSON Schema validation. Mirrors ws.go's maxIncompleteRetries so the
// structured-output path and the task_end mandatory-completion path share one
// retry budget shape.
const maxSchemaRetries = 3

var (
	schemaCacheMu sync.Mutex
	schemaCache   = map[string]*jsonschema.Schema{}
)

// ValidateStructuredOutput parses text as JSON and validates it against
// schemaDoc (a JSON Schema document). On success it returns the extracted JSON
// bytes.
//
// An empty schemaDoc is the text-mode no-op: it returns the extracted bytes and
// a nil error WITHOUT parsing or validating — callers gate structured behavior
// on len(schemaDoc) > 0 so the text path is byte-identical to today. The bypass
// happens before JSON parsing so non-JSON model output is accepted verbatim
// when no schema is in force.
//
// Models frequently wrap JSON in ```json fences; extractJSON trims those before
// parsing.
func ValidateStructuredOutput(text string, schemaDoc json.RawMessage) (json.RawMessage, error) {
	raw := json.RawMessage(extractJSON(text))
	if len(schemaDoc) == 0 {
		// Text mode: no schema in force — accept anything, including non-JSON
		// prose. Returning here before json.Unmarshal is what keeps the text
		// path byte-identical to today's un-validated output.
		return raw, nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("output is not valid JSON: %w", err)
	}
	sch, err := compileSchema(schemaDoc)
	if err != nil {
		return nil, fmt.Errorf("invalid output schema: %w", err)
	}
	if err := sch.Validate(value); err != nil {
		return nil, fmt.Errorf("output does not match schema: %w", err)
	}
	return raw, nil
}

// compileSchema compiles (and memoizes by document content) a JSON Schema.
//
// jsonschema/v6's Compiler.AddResource takes a parsed JSON value (not an
// io.Reader), so we unmarshal the schema document into an any first. The cache
// key is the raw document bytes: schemas are content-addressed and the same
// per-turn schema is reused across retries without recompiling.
//
// The cache is per-process append-only (no eviction): acceptable for v1 since
// schemas repeat within a session; revisit with an LRU/cap if a long-lived
// server sees unbounded growth.
func compileSchema(doc json.RawMessage) (*jsonschema.Schema, error) {
	key := string(doc)
	schemaCacheMu.Lock()
	defer schemaCacheMu.Unlock()
	if sch, ok := schemaCache[key]; ok {
		return sch, nil
	}
	var node any
	if err := json.Unmarshal(doc, &node); err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("schema.json", node); err != nil {
		return nil, err
	}
	sch, err := c.Compile("schema.json")
	if err != nil {
		return nil, err
	}
	schemaCache[key] = sch
	return sch, nil
}

// extractJSON trims surrounding whitespace and a single pair of ``` / ```json
// fences so fenced model output parses. Returns the trimmed string unchanged
// when no fence is present.
//
// It does NOT extract a brace-balanced span: v1 relies on the schema-retry
// reminder (schemaRetryReminder) to teach the model to emit pure JSON on the
// next attempt, rather than parsing prose-surrounded JSON here. Caveat: the
// trailing ``` search (LastIndex) may over-trim JSON whose string values
// themselves contain ```; accepted as a v1 limitation.
func extractJSON(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		if nl := strings.IndexByte(s, '\n'); nl >= 0 {
			s = strings.TrimSpace(s[nl+1:])
		}
		if i := strings.LastIndex(s, "```"); i >= 0 {
			s = strings.TrimSpace(s[:i])
		}
	}
	return s
}

// schemaRetryReminder builds the user-role reminder appended to history when a
// schema-validation retry is attempted. It always includes both the validation
// error (so the model knows what to fix) and the previous output (so the model
// can repair rather than start over); schemaRetryReminder's callers rely on
// both being present.
func schemaRetryReminder(prevText string, verr error) string {
	return fmt.Sprintf(
		"Your previous reply did not satisfy the required output schema and was discarded:\n%[1]s\n\n"+
			"Reply again with a single JSON value that matches the schema exactly. Your previous reply was:\n%[2]s",
		verr.Error(), prevText)
}
