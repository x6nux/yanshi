package lsp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"testing"
)

func TestWriteMessage_ContentLengthFraming(t *testing.T) {
	var buf bytes.Buffer
	payload := map[string]any{"jsonrpc": "2.0", "method": "ping", "params": map[string]any{"n": 7}}
	if err := WriteMessage(&buf, payload); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	if !bytes.Contains(buf.Bytes(), []byte("Content-Length:")) {
		t.Errorf("缺少 Content-Length 头,得到 %q", buf.String())
	}
	body := bytes.SplitN(buf.Bytes(), []byte("\r\n\r\n"), 2)[1]
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body 非合法 JSON: %v (body=%q)", err, body)
	}
	params := got["params"].(map[string]any)
	if int(params["n"].(float64)) != 7 {
		t.Errorf("params.n 还原失败: %v", params["n"])
	}
}

func TestReadMessage_RoundTrip(t *testing.T) {
	var buf bytes.Buffer
	_ = WriteMessage(&buf, map[string]any{"jsonrpc": "2.0", "id": 42, "result": "ok"})
	got, err := ReadMessage(bufio.NewReader(&buf))
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if int(got["id"].(float64)) != 42 || got["result"] != "ok" {
		t.Errorf("round-trip 失败: %v", got)
	}
}

// TestReadMessage_PersistentBufferKeepsPrefetchedBytes 是评审 #1 的回归保护:
// 连写两条帧,bufio 一次预读可能把第二条的前缀也读进 buffer;用同一个 *bufio.Reader
// 连读两次必须能各自还原,不能丢帧。
func TestReadMessage_PersistentBufferKeepsPrefetchedBytes(t *testing.T) {
	var buf bytes.Buffer
	_ = WriteMessage(&buf, map[string]any{"jsonrpc": "2.0", "id": 1, "result": "a"})
	_ = WriteMessage(&buf, map[string]any{"jsonrpc": "2.0", "id": 2, "result": "b"})

	br := bufio.NewReader(&buf)
	m1, err := ReadMessage(br)
	if err != nil {
		t.Fatalf("第一次 ReadMessage: %v", err)
	}
	m2, err := ReadMessage(br)
	if err != nil {
		t.Fatalf("第二次 ReadMessage: %v (可能因 new-reader 丢了预读字节)", err)
	}
	if int(m1["id"].(float64)) != 1 || int(m2["id"].(float64)) != 2 {
		t.Errorf("连读两条帧错乱: m1=%v m2=%v", m1, m2)
	}
}
