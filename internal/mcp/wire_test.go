package mcp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"testing"
)

func TestWriteMessage_ContentLengthFraming(t *testing.T) {
	var buf bytes.Buffer
	payload := map[string]any{"jsonrpc": "2.0", "method": "tools/list"}
	if err := WriteMessage(&buf, payload); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	if !bytes.Contains(buf.Bytes(), []byte("Content-Length:")) {
		t.Errorf("缺少 Content-Length 头")
	}
	body := bytes.SplitN(buf.Bytes(), []byte("\r\n\r\n"), 2)[1]
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body 非合法 JSON: %v", err)
	}
	if got["method"] != "tools/list" {
		t.Errorf("method mismatch: %v", got["method"])
	}
}

func TestReadMessage_ContentLengthRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	_ = WriteMessage(&buf, map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"ok": true}})
	got, err := ReadMessage(bufio.NewReader(&buf))
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	res, _ := got["result"].(map[string]any)
	if res["ok"] != true {
		t.Errorf("round-trip 失败: %v", got)
	}
}

func TestReadMessage_NewlineDelimited(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteString(`{"jsonrpc":"2.0","id":1,"result":"ok"}` + "\n")
	buf.WriteString(`{"jsonrpc":"2.0","id":2,"result":"ok2"}` + "\n")
	r := bufio.NewReader(&buf)
	msg1, err := ReadMessage(r)
	if err != nil {
		t.Fatalf("ReadMessage line1: %v", err)
	}
	if id, _ := msg1["id"].(float64); int(id) != 1 {
		t.Errorf("line1 id = %v", msg1["id"])
	}
	msg2, err := ReadMessage(r)
	if err != nil {
		t.Fatalf("ReadMessage line2: %v", err)
	}
	if id, _ := msg2["id"].(float64); int(id) != 2 {
		t.Errorf("line2 id = %v", msg2["id"])
	}
}
