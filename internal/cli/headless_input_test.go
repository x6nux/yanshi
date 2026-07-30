package cli

import (
	"strings"
	"testing"
)

func TestReadHeadlessInputs_Text(t *testing.T) {
	got, err := ReadHeadlessInputs(strings.NewReader("  hello\nworld\n"), HeadlessInputText)
	if err != nil {
		t.Fatalf("ReadHeadlessInputs: %v", err)
	}
	if len(got) != 1 || got[0].Prompt != "hello\nworld" {
		t.Fatalf("text input = %#v", got)
	}
}

func TestReadHeadlessInputs_LinesSkipsBlankLines(t *testing.T) {
	got, err := ReadHeadlessInputs(strings.NewReader("one\n\n two \n"), HeadlessInputLines)
	if err != nil {
		t.Fatalf("ReadHeadlessInputs: %v", err)
	}
	if len(got) != 2 || got[0].Prompt != "one" || got[1].Prompt != "two" {
		t.Fatalf("line input = %#v", got)
	}
}

func TestReadHeadlessInputs_JSONL(t *testing.T) {
	input := "{\"prompt\":\"one\"}\n\n{\"prompt\":\"two\",\"resume\":\"sess-2\"}\n"
	got, err := ReadHeadlessInputs(strings.NewReader(input), HeadlessInputJSONL)
	if err != nil {
		t.Fatalf("ReadHeadlessInputs: %v", err)
	}
	if len(got) != 2 || got[0].Prompt != "one" || got[1].Resume != "sess-2" {
		t.Fatalf("jsonl input = %#v", got)
	}
}

func TestReadHeadlessInputs_JSONLRejectsMissingPrompt(t *testing.T) {
	_, err := ReadHeadlessInputs(strings.NewReader("{\"resume\":\"sess-2\"}\n"), HeadlessInputJSONL)
	if err == nil {
		t.Fatal("missing prompt should fail")
	}
}

func TestReadHeadlessInputs_UnknownMode(t *testing.T) {
	_, err := ReadHeadlessInputs(strings.NewReader("x"), HeadlessInputMode("yaml"))
	if err == nil {
		t.Fatal("unknown input mode should fail")
	}
}
