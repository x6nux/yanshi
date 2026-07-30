package tools

import (
	"fmt"
	"strings"
)

// This file makes fs_edit robust to the whitespace/line-ending differences that
// routinely trip up a model reproducing old_string: tabs vs spaces, trailing
// whitespace, and \r\n vs \n. The strategy is a two-tier match:
//
//  1. EXACT (runEdit): the fast, common path — a byte-exact substring match.
//  2. LENIENT (lenientFind, below): when the exact match misses, re-scan the
//     file line-by-line with each line normalized (tabs expanded, trailing
//     whitespace stripped, \r\n folded) and locate the line sequence matching
//     the normalized old_string. A second, more aggressive normalization
//     (leading whitespace stripped too) is tried when the first finds nothing —
//     this catches an indent-width mismatch (model wrote 2 spaces, file has a
//     tab) that fixed-width tab expansion can't reconcile.
//
// When even the lenient match fails, editNotFoundError returns the actual file
// content so the model can see what's there and self-correct instead of looping.

// editRange is a half-open byte range [Start, End) into the original file data.
type editRange struct{ Start, End int }

// lenientFind locates oldString in data using whitespace-lenient, line-oriented
// matching. It returns one editRange per occurrence (in file order), so the
// caller can detect ambiguity the same way the exact path does. Returns nil when
// nothing matches even leniently.
func lenientFind(data []byte, oldString string) []editRange {
	s := string(data)
	dataLines, dataOffs := splitLinesWithOffsets(s)
	oldLines := splitLines(oldString)
	if len(oldLines) == 0 {
		return nil
	}
	oldTrailingNL := strings.HasSuffix(strings.ReplaceAll(strings.ReplaceAll(oldString, "\r\n", "\n"), "\r", "\n"), "\n")

	// Try each normalization from most-preservative to most-aggressive; the
	// first that yields any match wins.
	for _, norm := range []lineNorm{normTabTrailing, normContentOnly} {
		ranges := matchLineSeq(s, dataLines, oldLines, dataOffs, oldTrailingNL, norm)
		if len(ranges) > 0 {
			return ranges
		}
	}
	return nil
}

// lineNorm is a per-line normalization function used for lenient comparison.
type lineNorm func(string) string

// normTabTrailing expands tabs to 4 spaces and strips trailing whitespace. This
// reconciles tab/space indentation (when widths align) and trailing-whitespace
// differences while preserving internal structure.
func normTabTrailing(s string) string {
	return strings.TrimRight(strings.ReplaceAll(s, "\t", "    "), " \t")
}

// normContentOnly strips ALL leading and trailing whitespace, comparing lines by
// their bare content. This is the fallback for indent-width mismatches (model's
// leading spaces can't be reconciled with the file's tabs by fixed-width
// expansion). Multi-line old_string context keeps false positives rare.
func normContentOnly(s string) string {
	return strings.TrimLeft(strings.TrimRight(s, " \t"), " \t")
}

// matchLineSeq finds every site where normalized(oldLines) matches a run of
// normalized(dataLines), returning each site's original byte range. s is the raw
// file text (for terminator scanning); dataOffs[i] is the byte offset of line i.
func matchLineSeq(s string, dataLines, oldLines []string, dataOffs []int, oldTrailingNL bool, norm lineNorm) []editRange {
	n := len(oldLines)
	if n == 0 || n > len(dataLines) {
		return nil
	}
	normData := make([]string, len(dataLines))
	for i, l := range dataLines {
		normData[i] = norm(l)
	}
	normOld := make([]string, n)
	for i, l := range oldLines {
		normOld[i] = norm(l)
	}
	var ranges []editRange
	for i := 0; i+n <= len(normData); i++ {
		ok := true
		for j := 0; j < n; j++ {
			if normData[i+j] != normOld[j] {
				ok = false
				break
			}
		}
		if ok {
			last := i + n - 1
			start := dataOffs[i]
			end := dataOffs[last] + len(dataLines[last]) // content end (\r already stripped)
			if oldTrailingNL {
				end += terminatorLen(s, end)
			}
			ranges = append(ranges, editRange{Start: start, End: end})
		}
	}
	return ranges
}

// terminatorLen returns the length of the line terminator at s[at:] (2 for
// "\r\n", 1 for "\n" or a lone "\r", 0 at EOF / no terminator). s[at] is expected
// to be at or past the line content (a \r or \n if a terminator is present).
func terminatorLen(s string, at int) int {
	if at >= len(s) {
		return 0
	}
	if s[at] == '\r' && at+1 < len(s) && s[at+1] == '\n' {
		return 2
	}
	if s[at] == '\n' || s[at] == '\r' {
		return 1
	}
	return 0
}

// splitLinesWithOffsets splits s into lines (without their \n terminator) and
// records the byte offset where each line begins in s. A trailing \r (from a
// \r\n terminator) is stripped from the line TEXT but the offset still points at
// the line's true start in s. A final newline does not produce a trailing empty
// line (it is not a real line to match).
func splitLinesWithOffsets(s string) (lines []string, offsets []int) {
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, strings.TrimSuffix(s[start:i], "\r"))
			offsets = append(offsets, start)
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, strings.TrimSuffix(s[start:], "\r"))
		offsets = append(offsets, start)
	}
	return
}

// splitLines splits s into lines for comparison, folding \r\n and lone \r to \n
// first and dropping a trailing empty element produced by a final newline.
func splitLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	parts := strings.Split(s, "\n")
	if len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}

// editNotFoundError builds an error that surfaces the actual file content so the
// model can see the real formatting and retry with a correct old_string, rather
// than looping blind. Up to 15 lines are shown (the head is usually where the
// model's stale view diverges).
func editNotFoundError(path string, data []byte) error {
	return fmt.Errorf("old_string not found in %s (even after normalizing tabs/trailing whitespace/line endings).\n"+
		"Re-read the file (fs_read %s) and copy the exact text. The actual file begins:\n%s",
		path, path, previewLines(data, 15))
}

// previewLines returns the first n lines of data as a quoted, indented block for
// inclusion in an error message. Control chars are escaped so tabs/CR are
// visible, and long lines are truncated.
func previewLines(data []byte, n int) string {
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	for i, l := range lines {
		l = strings.ReplaceAll(l, "\r", "\\r")
		l = strings.ReplaceAll(l, "\t", "\\t")
		if len(l) > 100 {
			l = l[:100] + "…"
		}
		lines[i] = "    | " + l
	}
	return strings.Join(lines, "\n")
}
