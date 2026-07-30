package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SpillThreshold is the single uniform cap on any GuardedTool's output (64 KiB,
// ≈16k tokens). Results at or below this size are returned verbatim; larger
// results are written to a temp file under <workRoot>/.yanshi/tmp/spillover/ and
// replaced with a head+tail preview plus the temp path. fs_read self-guards and
// never returns an oversized string, so it never actually spills (see fs.go).
const SpillThreshold = 64 * 1024

// spillDir is the subpath under the work root where oversized tool outputs land.
const spillDir = ".yanshi/tmp/spillover"

// Preview window shown in place of an oversized result. Head+tail so trailing
// info (shell exit code, final JSON fields) stays visible alongside the head.
// The byte budgets keep the preview well under SpillThreshold even for inputs
// with a few very long lines.
const (
	spillHeadLines  = 15
	spillTailLines  = 10
	spillHeadBudget = 16 * 1024
	spillTailBudget = 8 * 1024
)

// spillIfTooLong returns result unchanged when len(result) <= SpillThreshold.
// Otherwise it writes result to a temp file under
// <workRoot>/.yanshi/tmp/spillover/<tool>-<unixms>-<rand>.txt and returns a
// head+tail preview plus the path and usage guidance. workRoot is read from ctx
// (WithWorkRoot); when empty it falls back to ".". A write failure degrades
// gracefully: the result is truncated to SpillThreshold with a footer noting the
// spill failed — a disk error must never fail the tool call itself.
func spillIfTooLong(ctx context.Context, toolName, result string) string {
	if len(result) <= SpillThreshold {
		return result
	}
	root := WorkRootFromContext(ctx)
	if root == "" {
		root = "."
	}
	dir := filepath.Join(root, spillDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return degradedSpill(result, err)
	}
	name := fmt.Sprintf("%s-%d-%s.txt", toolName, time.Now().UnixMilli(), randSuffix(4))
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(result), 0o600); err != nil {
		return degradedSpill(result, err)
	}
	return spillPreview(result, relPath(root, path))
}

// degradedSpill truncates result to SpillThreshold and appends a footer noting
// the spill failed — used when the temp file cannot be written.
func degradedSpill(result string, cause error) string {
	trunc := result
	if len(trunc) > SpillThreshold {
		// Byte-truncate (may split a multi-byte UTF-8 rune); acceptable for a
		// degraded error path — JSON encoding scrubs the invalid trailing bytes.
		trunc = trunc[:SpillThreshold]
	}
	return trunc + fmt.Sprintf("\n\n[... spill to temp file failed (%v); result truncated to %d bytes ...]", cause, SpillThreshold)
}

// spillPreview builds the head+tail preview returned in place of an oversized
// result. rel is the path surfaced to the model (relative to the work root so it
// can be passed back to fs_read). Head and tail are byte-capped so the preview
// stays under SpillThreshold even for pathological inputs. When the input has
// only a few lines (total ≤ spillHeadLines) the whole thing fits in the head and
// no tail is appended; when total is between head and head+tail, the tail is the
// remainder rather than duplicating head lines.
func spillPreview(result, rel string) string {
	lines := strings.Split(result, "\n")
	total := len(lines)

	headSrc := lines
	if len(headSrc) > spillHeadLines {
		headSrc = headSrc[:spillHeadLines]
	}
	headEnd := len(headSrc) // = min(total, spillHeadLines)
	head := capLines(headSrc, spillHeadBudget)

	// Tail starts spillTailLines before EOF, but never overlaps the head.
	tailStart := total - spillTailLines
	if tailStart < headEnd {
		tailStart = headEnd
	}

	var b strings.Builder
	fmt.Fprintf(&b, "[spilled: %d lines / %s → %s]\n%s", total, humanBytes(len(result)), rel, head)
	if total > headEnd {
		if omit := tailStart - headEnd; omit > 0 {
			fmt.Fprintf(&b, "\n[... %d lines omitted ...]", omit)
		}
		if tailStart < total {
			b.WriteString("\n")
			b.WriteString(capLines(lines[tailStart:], spillTailBudget))
		}
	}
	b.WriteString("\nUse summarize(path) to condense, or fs_read(path, offset, end) to page.")
	return b.String()
}

// capLines joins lines with newlines, but stops (truncating the final line) once
// the accumulated size would exceed budget. Guarantees the result is ≤ budget
// bytes for any input where each line is itself ≤ budget.
func capLines(lines []string, budget int) string {
	var b strings.Builder
	for i, ln := range lines {
		sep := ""
		if i > 0 {
			sep = "\n"
		}
		if b.Len()+len(sep)+len(ln) > budget {
			remain := budget - b.Len() - len(sep)
			if remain > 0 {
				b.WriteString(sep)
				// Byte-truncate the final line (may split a rune); acceptable for
				// a preview and avoids a panic since remain ≤ len(ln).
				b.WriteString(ln[:remain])
			}
			break
		}
		b.WriteString(sep)
		b.WriteString(ln)
	}
	return b.String()
}

// relPath returns path relative to root when possible (so the model gets a
// jail-under-root path it can hand back to fs_read), else path unchanged.
func relPath(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(rel)
}

// randSuffix returns n random bytes as a 2n-char hex string; on the rare
// crypto/rand read failure it returns a fixed fallback.
func randSuffix(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "00000000"
	}
	return hex.EncodeToString(b)
}

// Sweep removes all regular files under <root>/.yanshi/tmp/spillover/. Call at
// process start to clear leftovers from a previous run. A missing directory is a
// no-op; per-file removal errors are ignored (best-effort). Subdirectories are
// left in place (the spillover dir is flat).
func Sweep(root string) {
	if root == "" {
		root = "."
	}
	dir := filepath.Join(root, spillDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
}
