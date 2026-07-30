package mcp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
)

// WriteMessage 编码 v 为 JSON 并按 Content-Length 帧写入 w（标准 LSP stdio 帧）。
func WriteMessage(w io.Writer, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("mcp: marshal: %w", err)
	}
	hdr := "Content-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n"
	if _, err := w.Write([]byte(hdr)); err != nil {
		return fmt.Errorf("mcp: write header: %w", err)
	}
	if _, err := w.Write(body); err != nil {
		return fmt.Errorf("mcp: write body: %w", err)
	}
	return nil
}

// WriteLineMessage 编码 v 为 JSON 并以单行（带 `\n`）写入 w（Yanshi VCS MCP server 格式）。
// 用于与 internal/vcs/mcp/server.go 这类只读 newline-delimited JSON 的 server 互通。
func WriteLineMessage(w io.Writer, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("mcp: marshal: %w", err)
	}
	if _, err := w.Write(append(body, '\n')); err != nil {
		return fmt.Errorf("mcp: write line: %w", err)
	}
	return nil
}

// ReadMessage 从 r 读一条 JSON-RPC 帧。自动检测两种格式：
//   - 第一行以 "Content-Length:" 开头 → LSP 风格多行头 + 空行 + body。
//   - 否则 → 把第一行作为 newline-delimited JSON 解析。
//
// 返回解码后的 map（字段 jsonrpc/id/method/params/result/error 按需取用）。
func ReadMessage(r *bufio.Reader) (map[string]any, error) {
	first, err := r.ReadBytes('\n')
	if err != nil && len(first) == 0 {
		return nil, fmt.Errorf("mcp: read first line: %w", err)
	}
	trimmed := bytes.TrimRight(first, "\r\n")
	if bytes.HasPrefix(trimmed, []byte("Content-Length:")) {
		val := bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("Content-Length:")))
		length, err := strconv.Atoi(string(val))
		if err != nil {
			return nil, fmt.Errorf("mcp: bad Content-Length %q: %w", string(val), err)
		}
		for {
			line, err := r.ReadBytes('\n')
			if err != nil {
				return nil, fmt.Errorf("mcp: read header line: %w", err)
			}
			if len(bytes.TrimRight(line, "\r\n")) == 0 {
				break
			}
		}
		body := make([]byte, length)
		if _, err := io.ReadFull(r, body); err != nil {
			return nil, fmt.Errorf("mcp: read body: %w", err)
		}
		return parseJSON(body)
	}
	return parseJSON(bytes.TrimSpace(first))
}

func parseJSON(body []byte) (map[string]any, error) {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("mcp: unmarshal: %w", err)
	}
	return m, nil
}
