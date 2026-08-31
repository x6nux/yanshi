package orchestrator

// W-F-09 的端到端测试：hook 的 additional_context 超过内联上限（每行
// 2KiB、共 8 条）之后，溢出部分落盘留引用，引用经 artifact_read 取回时
// 与原文逐字节一致。spec 验收原文：「超阈值的 hook 输出落盘留引用；引用
// 可经 artifact_read 取回」。
//
// 会变红的变异：
//
//	把 runPreToolUseHooks 里 overflowed 的落盘分支删掉（回到静默丢弃）→
//	out 里不再有 artifact 引用，本测试红；
//	把 SpillHookOutput 的引用文本改成不带 artifact id 的形状 → 取回步骤红。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/shell"
	"github.com/x6nux/yanshi/internal/task/work"
	"github.com/x6nux/yanshi/internal/tools"
)

// hookSpillFixture 装配真实管线需要的 ctx：hook 总线（manycontext 剧本的
// hook 输出 12 条上下文行，超过 8 条上限）+ 真实 work Manager（内存 sqlite）
// + workRoot + 放行 profile。
func hookSpillFixture(t *testing.T) (context.Context, *work.Manager, string) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	st, err := work.FromDB(db, nil)
	require.NoError(t, err)
	mgr := work.NewManager(st, nil, work.ArtifactPolicy{QuotaBytes: 4 << 20, TTL: 0})

	workRoot := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(workRoot); err == nil {
		workRoot = resolved
	}
	profile := fsWriteProfile()
	profile.Tools.Allow = append(profile.Tools.Allow, "artifact_read")
	ctx := tools.WithProfile(tools.WithWorkRoot(tools.WithTaskManager(
		context.Background(), mgr), workRoot), profile)
	ctx = tools.WithSecureProcessFactory(ctx, shell.UnsandboxedSecureFactory())
	ctx = withTurnHooks(ctx, HooksConfig{PreToolUse: []HookConfig{hookTestProgram(t, "manycontext")}})
	return ctx, mgr, workRoot
}

func TestPreToolUseHookOversizeContextSpillsToArtifact(t *testing.T) {
	ctx, mgr, workRoot := hookSpillFixture(t)
	d := &hookToolDouble{workRoot: workRoot}

	ep, err := newHookMiddleware().WrapInvokableToolCall(ctx, d.endpoint(), &adk.ToolContext{Name: "fs_write"})
	require.NoError(t, err)
	out, err := ep(ctx, `{"path":"notes.txt"}`)
	require.NoError(t, err)
	require.Contains(t, out, "wrote ", "落盘不得改变调用本身的成败")

	// 1) 模型可见结果里有引用行，且行数仍然被封顶（8 条内联 + 1 条引用）。
	var refLine string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "artifact") && strings.Contains(l, "artifact_read") {
			refLine = l
		}
	}
	require.NotEmpty(t, refLine, "溢出必须留一条引用行")
	artID := regexp.MustCompile(`artifact (\S+)`).FindStringSubmatch(refLine)
	require.Len(t, artID, 2, "引用行必须带 artifact id: %q", refLine)

	// 2) 引用真的经 artifact_read 取得原文 —— 与 hook 附加输出逐字节一致。
	raw, err := tools.NewArtifactTools().Read.InvokableRun(ctx, `{"id":"`+artID[1]+`"}`)
	require.NoError(t, err)
	var resp struct {
		Content string `json:"content"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &resp))
	for i := 0; i < 12; i++ {
		require.Contains(t, resp.Content, fmt.Sprintf("hook note %02d of 12", i),
			"被封顶丢弃的第 %d 条必须能从 artifact 里取回", i)
	}

	// 3) 背书任务真实存在（artifact 外键的另一半）。
	artifact, err := mgr.ReadArtifact(ctx, artID[1])
	require.NoError(t, err)
	_, err = mgr.Read(ctx, artifact.TaskID)
	require.NoError(t, err)
}

func TestPreToolUseHookInlineContextStaysBoundedWithSpill(t *testing.T) {
	// 内联上限不因落盘而放宽：>2KiB 的行仍然只进截断头部，全文在 artifact。
	ctx, _, workRoot := hookSpillFixture(t)
	d := &hookToolDouble{workRoot: workRoot}

	ep, err := newHookMiddleware().WrapInvokableToolCall(ctx, d.endpoint(), &adk.ToolContext{Name: "fs_write"})
	require.NoError(t, err)
	out, err := ep(ctx, `{"path":"notes.txt"}`)
	require.NoError(t, err)

	inline := strings.Count(out, "[hook ") - 1 // 减去引用行
	require.LessOrEqual(t, inline, hookContextMax, "内联行数不得因落盘而放宽")
	require.NotContains(t, out, strings.Repeat("hook note", 300),
		"超长行的全文不得直接进入模型可见结果")
}
