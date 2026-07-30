package http

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestValidateStructuredOutput(t *testing.T) {
	personSchema := json.RawMessage(`{
		"type": "object",
		"properties": {"name": {"type": "string"}, "age": {"type": "integer"}},
		"required": ["name"],
		"additionalProperties": false
	}`)
	tests := []struct {
		name    string
		text    string
		schema  json.RawMessage
		wantErr bool
	}{
		{"valid object", `{"name":"Ada","age":36}`, personSchema, false},
		{"fenced json", "```json\n{\"name\":\"Ada\"}\n```", personSchema, false},
		{"missing required", `{"age":36}`, personSchema, true},
		{"extra property", `{"name":"Ada","x":1}`, personSchema, true},
		{"not json", `hello world`, personSchema, true},
		{"empty schema (text mode)", `{"any":"thing"}`, nil, false},
		{"invalid schema doc", `{"name":"Ada"}`, json.RawMessage(`{bad json`), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ValidateStructuredOutput(tc.text, tc.schema)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
	// Text mode: empty schema returns extracted bytes, no error, for any text
	// (including non-JSON): the no-schema path does NOT validate.
	if _, err := ValidateStructuredOutput("plain text", nil); err != nil {
		t.Fatalf("text mode must not error on non-JSON, got: %v", err)
	}
}

func TestExtractJSON(t *testing.T) {
	tests := []struct{ in, want string }{
		{`{"a":1}`, `{"a":1}`},
		{"```json\n{\"a\":1}\n```", `{"a":1}`},
		{"```\n{\"a\":1}\n```", `{"a":1}`},
		{`  {"a":1}  `, `{"a":1}`},
		{`plain`, `plain`},
	}
	for _, tc := range tests {
		if got := extractJSON(tc.in); got != tc.want {
			t.Errorf("extractJSON(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSchemaRetryReminder(t *testing.T) {
	r := schemaRetryReminder("prev output", errors.New("missing field name"))
	if r == "" || !strings.Contains(r, "missing field name") || !strings.Contains(r, "prev output") {
		t.Fatalf("reminder must include error and prev text: %q", r)
	}
}
