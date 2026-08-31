package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/task/work"
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
//
// The LINE counts (how many head/tail lines to keep) are W-C-09's
// configurable policy — see einollm.TruncationSpec and
// einollm.DefaultTruncationSpec, whose {15, 10} values this constant pair
// used to BE before this ticket and which spillPreview now falls back to via
// tools.TruncationPolicyFromContext, so an unconfigured deployment's
// behavior is byte-identical to before. The BYTE budgets stay fixed
// constants here — see TruncationSpec's doc comment for why they are
// deliberately excluded from that struct (a safety cap, not a per-model
// editorial opinion).
const (
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
	rel, ok := spillFullText(ctx, toolName, result)
	if !ok {
		return degradedSpill(result, errSpillWriteFailed)
	}
	return spillPreview(ctx, result, rel)
}

// errSpillWriteFailed is the cause degradedSpill reports when spillFullText
// declined. The concrete os error is logged by spillFullText's own return
// path being a bool: a caller that only needs "did it land" should not have to
// thread an error it will not inspect, and the footer text is for the model,
// which cannot act on an errno either way.
var errSpillWriteFailed = errors.New("could not write the spillover file")

// spillFullText writes text verbatim to
// <workRoot>/.yanshi/tmp/spillover/<tool>-<unixms>-<rand>.txt and returns the
// path RELATIVE to the work root (so the model can hand it straight back to
// fs_read), plus whether the write landed.
//
// Split out of spillIfTooLong because T4's degrade tier needs exactly this and
// nothing else: it must place a recoverable copy on disk BEFORE shrinking a
// result that is nowhere near the 64 KiB spill cap. Having two functions
// writing files into the same directory with two naming schemes is how the
// janitor and Sweep would start disagreeing about what they own.
//
// A failure is reported rather than fatal: every caller has a defined answer
// for "no disk copy" (spillIfTooLong truncates with a footer, DegradeToolResult
// declines to shrink at all).
func spillFullText(ctx context.Context, toolName, text string) (string, bool) {
	root := WorkRootFromContext(ctx)
	if root == "" {
		root = "."
	}
	dir := filepath.Join(root, spillDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", false
	}
	name := fmt.Sprintf("%s-%d-%s.txt", toolName, time.Now().UnixMilli(), randSuffix(4))
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		return "", false
	}
	return relPath(root, path), true
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
// only a few lines (total ≤ the resolved policy's HeadLines) the whole thing
// fits in the head and no tail is appended; when total is between head and
// head+tail, the tail is the remainder rather than duplicating head lines.
//
// The head/tail LINE counts come from tools.TruncationPolicyFromContext
// (W-C-09) — the orchestrator resolves this once at bootstrap from
// ProviderConfig.TruncationPolicy / the model catalog and binds it via
// WithTruncationPolicy alongside WithWorkRoot. When ctx carries no policy (a
// sub-agent path, a test that never bound one), this falls back to
// einollm.DefaultTruncationSpec — the same {15, 10} this function hardcoded
// before this ticket, so an unconfigured deployment's output is unchanged.
func spillPreview(ctx context.Context, result, rel string) string {
	policy, ok := TruncationPolicyFromContext(ctx)
	if !ok {
		policy = einollm.DefaultTruncationSpec
	}

	lines := strings.Split(result, "\n")
	total := len(lines)

	headSrc := lines
	if len(headSrc) > policy.HeadLines {
		headSrc = headSrc[:policy.HeadLines]
	}
	headEnd := len(headSrc) // = min(total, policy.HeadLines)
	head := capLines(headSrc, spillHeadBudget)

	// Tail starts policy.TailLines before EOF, but never overlaps the head.
	tailStart := total - policy.TailLines
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

// HookSpillTaskTitle 是 W-F-09 里 hook 溢出输出落盘时创建的背书任务的标题
// 前缀。task_work_artifacts 对 task_work 有外键（且连接开着 foreign_keys），
// artifact 必须挂在真实任务下；hook 输出不从属于任何模型创建的任务，所以
// 每次落盘建一个专属任务 —— 它出现在 task_list 里是诚实的行为：那份输出
// 确实存在于磁盘上，操作员看得到、janitor 清得到。
const HookSpillTaskTitle = "hook output: "

// SpillHookOutput 把一段超阈值的 hook 附加输出落盘并返回模型可用的引用与
// 是否成功（W-F-09）。优先 artifact 路径（durable、配额管理、janitor 清扫、
// 引用经 artifact_read 按需取回）；没有 task manager（子代理、未接线的测试）
// 时退回临时 spillover 文件（引用经 fs_read 取回）；两者都失败返回 ok=false，
// 调用方保留截断文本 —— 磁盘问题绝不能把一次成功的调用变成失败，与
// spillIfTooLong 的降级原则相同。
//
// 这里只负责「放一份可取回的副本」。何时调用（哪些内容算溢出）与引用如何
// 进入模型可见文本，是 orchestrator hook 总线的决定 —— 两个包各管一半，
// 阈值改动才不会散落两处。
func SpillHookOutput(ctx context.Context, label, content string) (string, bool) {
	if strings.TrimSpace(content) == "" {
		return "", false
	}
	root := WorkRootFromContext(ctx)
	if root == "" {
		root = "."
	}
	if manager, ok := TaskManagerFromContext(ctx); ok {
		task, err := manager.Create(ctx, work.CreateReq{
			Title:  HookSpillTaskTitle + label,
			Prompt: fmt.Sprintf("Spillover artifact for oversized %s output (%d bytes). No execution attached.", label, len(content)),
		})
		if err == nil {
			artifact, werr := manager.WriteArtifact(ctx, task.ID, label, []byte(content), root)
			if werr == nil {
				return fmt.Sprintf("full output stored as artifact %s (%d bytes); retrieve with artifact_read", artifact.ID, artifact.Size), true
			}
		}
	}
	if rel, ok := spillFullText(ctx, label, content); ok {
		return fmt.Sprintf("full output written to %s; retrieve with fs_read", rel), true
	}
	return "", false
}
