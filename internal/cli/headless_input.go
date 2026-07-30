package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// HeadlessInputMode selects how a headless command turns stdin into prompts.
type HeadlessInputMode string

// Headless input modes for converting stdin to prompts.
const (
	HeadlessInputText  HeadlessInputMode = "text"
	HeadlessInputLines HeadlessInputMode = "lines"
	HeadlessInputJSONL HeadlessInputMode = "jsonl"
)

// HeadlessInput is one prompt accepted by the headless runner. Resume is only
// honored for the first record; later records continue the same backend session.
type HeadlessInput struct {
	Prompt string
	Resume string
}

type headlessJSONLInput struct {
	Prompt string `json:"prompt"`
	Resume string `json:"resume,omitempty"`
}

// ReadHeadlessInputs reads all prompts before a backend is opened. Text mode
// treats the whole stream as one prompt; lines mode treats each non-empty line
// as a prompt; JSONL mode accepts one object per line and ignores unknown fields
// so a producer can add metadata without breaking v1 clients.
func ReadHeadlessInputs(r io.Reader, mode HeadlessInputMode) ([]HeadlessInput, error) {
	if r == nil {
		return nil, fmt.Errorf("headless input: nil reader")
	}
	switch mode {
	case HeadlessInputText:
		b, err := io.ReadAll(r)
		if err != nil {
			return nil, fmt.Errorf("read text input: %w", err)
		}
		prompt := strings.TrimSpace(string(b))
		if prompt == "" {
			return nil, fmt.Errorf("prompt is empty")
		}
		return []HeadlessInput{{Prompt: prompt}}, nil
	case HeadlessInputLines:
		var out []HeadlessInput
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			prompt := strings.TrimSpace(sc.Text())
			if prompt != "" {
				out = append(out, HeadlessInput{Prompt: prompt})
			}
		}
		if err := sc.Err(); err != nil {
			return nil, fmt.Errorf("read line input: %w", err)
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("prompt is empty")
		}
		return out, nil
	case HeadlessInputJSONL:
		var out []HeadlessInput
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		line := 0
		for sc.Scan() {
			line++
			text := strings.TrimSpace(sc.Text())
			if text == "" {
				continue
			}
			var in headlessJSONLInput
			if err := json.Unmarshal([]byte(text), &in); err != nil {
				return nil, fmt.Errorf("jsonl line %d: %w", line, err)
			}
			in.Prompt = strings.TrimSpace(in.Prompt)
			if in.Prompt == "" {
				return nil, fmt.Errorf("jsonl line %d: prompt is empty", line)
			}
			out = append(out, HeadlessInput{Prompt: in.Prompt, Resume: strings.TrimSpace(in.Resume)})
		}
		if err := sc.Err(); err != nil {
			return nil, fmt.Errorf("read jsonl input: %w", err)
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("prompt is empty")
		}
		return out, nil
	default:
		return nil, fmt.Errorf("invalid input mode %q (want text, lines, or jsonl)", mode)
	}
}
