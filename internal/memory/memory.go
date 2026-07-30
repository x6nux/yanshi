// Package memory is yanshi's user-level preference notes (MEM1).
//
// Design mirrors internal/instruct: read two sources (user + project), each
// source and the total are hard-capped with a truncation marker when oversized;
// the difference is that memory is "append" not "read-only", so this package
// also provides Append, which writes a timestamped bullet. The remember tool
// and the /memory command share this code.
//
// Injection path: bootstrap passes the result of ComposeBlock(...) to
// Orchestrator.memorySuffix (independent of Instruction), and orchestrator.New
// appends it to Instruction AND baseInstruction at construction end — so
// runSubAgentTurn appends memorySuffix after an instructionOverride too,
// guaranteeing sub-agents inherit it.
package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// defaultMaxBytes is the per-source cap when max=0 (32 KiB, matching
// instruct.maxItemBytes).
const defaultMaxBytes = 32 << 10

// Load reads the user and project memory files and concatenates them in
// "user first, project last" order. A missing/empty file is skipped; when both
// are empty Load returns "". max=0 uses defaultMaxBytes; max>0 truncates each
// source independently (mirroring instruct.maxItemBytes).
func Load(userPath, projectPath string, max int) string {
	if max <= 0 {
		max = defaultMaxBytes
	}
	var parts []string
	if b, ok := readTrimmed(userPath); ok {
		parts = append(parts, capItem(b, max))
	}
	if b, ok := readTrimmed(projectPath); ok {
		parts = append(parts, capItem(b, max))
	}
	return strings.Join(parts, "\n\n")
}

// ComposeBlock is the bootstrap entry point that injects memory into the system
// prompt: when enabled is false OR both sources are missing/empty it returns "",
// otherwise SystemBlock(Load(...), userPath, max). bootstrap calls this and
// passes the result as orchestrator.Config.MemorySuffix.
func ComposeBlock(enabled bool, userPath, projectPath string, max int) string {
	if !enabled {
		return ""
	}
	content := Load(userPath, projectPath, max)
	if content == "" {
		return ""
	}
	return SystemBlock(content, userPath, max)
}

// SystemBlock wraps content in a <user_memory source="..."> block. Empty content
// returns "". When the payload exceeds max bytes it is truncated and tagged with
// "(truncated...)". max=0 uses defaultMaxBytes.
func SystemBlock(content, source string, max int) string {
	if max <= 0 {
		max = defaultMaxBytes
	}
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return ""
	}
	payload := trimmed
	if len(payload) > max {
		payload = payload[:max] +
			"\n…(truncated, raise memory.max_size or trim " + filepath.Base(source) + ")"
	}
	return fmt.Sprintf("<user_memory source=%q>\n%s\n</user_memory>", source, payload)
}

// Append writes entry as a timestamped bullet to path, creating parent
// directories as needed. Leading '#' is stripped; after stripping, an empty
// entry returns an error and no file is written. Timestamp format: UTC,
// minute precision.
func Append(path, entry string) error {
	trimmed := strings.TrimSpace(strings.TrimLeft(entry, "#"))
	if trimmed == "" {
		return fmt.Errorf("memory: entry is empty after stripping '#' prefix")
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("memory: mkdir: %w", err)
		}
	}
	ts := time.Now().UTC().Format("2006-01-02 15:04 UTC")
	line := fmt.Sprintf("- (%s) %s\n", ts, trimmed)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("memory: open: %w", err)
	}
	defer f.Close()
	if _, err := f.WriteString(line); err != nil {
		return fmt.Errorf("memory: write: %w", err)
	}
	return nil
}

// capItem is structurally identical to instruct.capItem: truncate to max bytes
// with a marker when exceeded.
func capItem(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "\n[... truncated: memory file cap reached ...]"
}

// readTrimmed reads path and TrimSpaces the result; missing/empty/read-error all
// return ("", false).
func readTrimmed(path string) (string, bool) {
	if path == "" {
		return "", false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	s := strings.TrimSpace(string(data))
	if s == "" {
		return "", false
	}
	return s, true
}
