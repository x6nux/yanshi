package lsp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
)

// LSP 用 JSON-RPC 2.0 over stdio,每条消息前面带 ASCII 头:
//
//	Content-Length: <字节数>\r\n
//	\r\n
//	<JSON body>
//
// 头与 body 之间用空行(\r\n\r\n)分隔。writeMessage/readMessage 只负责帧,
// 不解释 body 语义——body 的结构化解析在 client.go 按 message 类型做。
//
// ★ readMessage 接收一个持久的 *bufio.Reader(评审 #1):buf 在跨帧间保留预读
// 字节,避免每次 new bufio.Reader 丢掉下一帧的前缀。调用方(Client.readLoop、
// 测试)负责构造并复用同一个 reader。
func WriteMessage(w io.Writer, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("lsp: marshal: %w", err)
	}
	hdr := "Content-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n"
	if _, err := w.Write([]byte(hdr)); err != nil {
		return err
	}
	if _, err := w.Write(body); err != nil {
		return err
	}
	return nil
}

// readMessage 从 br 读一条帧,返回解码后的 body(map,因为请求/响应/通知结构不同,
// 调用方按 id/method/result 字段判型)。br 必须是跨帧复用的持久 reader。
func ReadMessage(br *bufio.Reader) (map[string]any, error) {
	var length int
	for {
		line, err := br.ReadBytes('\n')
		if err != nil {
			return nil, fmt.Errorf("lsp: read header: %w", err)
		}
		trimmed := bytes.TrimRight(line, "\r\n")
		if len(trimmed) == 0 {
			break // 空行 = 头结束
		}
		if bytes.HasPrefix(trimmed, []byte("Content-Length:")) {
			val := bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("Content-Length:")))
			n, err := strconv.Atoi(string(val))
			if err != nil {
				return nil, fmt.Errorf("lsp: bad Content-Length %q: %w", val, err)
			}
			length = n
		}
	}
	if length <= 0 {
		return nil, fmt.Errorf("lsp: no Content-Length header")
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(br, body); err != nil {
		return nil, fmt.Errorf("lsp: read body: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("lsp: unmarshal body: %w", err)
	}
	return m, nil
}
