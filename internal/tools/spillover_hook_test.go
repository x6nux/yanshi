package tools

// W-F-09 的落盘函数测试。验收「引用可经 artifact_read 取回」在这里用真实
// Manager（内存 sqlite + 真实磁盘文件）+ 真实 artifact_read 工具端到端验证。
// 会变红的变异：
//
//	删掉 SpillHookOutput 的 artifact 分支（直接走 spillover 文件）→
//	第一条断言的 "artifact" 引用消失，变红；
//	把 SpillHookOutput 的失败改成返回错误上抛 → 降级测试红。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSpillHookOutputArtifactReferenceIsRetrievable(t *testing.T) {
	mgr, _, root, _ := newArtifactManager(t)
	ctx := artifactCtx(mgr, root, "")

	content := strings.Repeat("hook line with real value\n", 500)
	ref, ok := SpillHookOutput(ctx, "hook-checker", content)
	require.True(t, ok)
	assert.Contains(t, ref, "artifact_read", "引用必须指向 artifact_read 可用的形态")

	// 从引用里取出 artifact id，并真的用 artifact_read 取回全文。
	re := regexp.MustCompile(`artifact (\S+)`)
	m := re.FindStringSubmatch(ref)
	require.Len(t, m, 2, "引用里必须带 artifact id: %q", ref)
	artID := m[1]

	tool := NewArtifactTools()
	raw, err := tool.Read.InvokableRun(ctx, `{"id":"`+artID+`"}`)
	require.NoError(t, err, "artifact_read 必须能取回 hook 溢出输出")
	var resp struct {
		Content string `json:"content"`
		EOF     bool   `json:"eof"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &resp))
	assert.Equal(t, content, resp.Content, "取回的字节必须与溢出原文逐字节一致")
	assert.True(t, resp.EOF)
}

func TestSpillHookOutputFallsBackToSpilloverFile(t *testing.T) {
	// 没有 task manager：退回临时 spillover 文件，引用指向 fs_read。
	root := t.TempDir()
	ctx := WithWorkRoot(context.Background(), root)
	content := "fallback payload"
	ref, ok := SpillHookOutput(ctx, "hook-fallback", content)
	require.True(t, ok)
	assert.Contains(t, ref, "fs_read")

	re := regexp.MustCompile(`written to ([^;\s]+)`)
	m := re.FindStringSubmatch(ref)
	require.Len(t, m, 2)
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(m[1])))
	require.NoError(t, err)
	assert.Equal(t, content, string(data))
}

func TestSpillHookOutputEmptyContentIsDeclined(t *testing.T) {
	_, ok := SpillHookOutput(context.Background(), "hook-empty", "   ")
	assert.False(t, ok, "空内容不该产生任务与文件")
}

func TestSpillHookOutputCreatesBackingTask(t *testing.T) {
	// artifact 表对 task_work 有外键：落盘必须伴随一个真实背书任务，
	// 而不是把伪 task id 硬塞进外键列。背书任务出现在 task 列表里是
	// 诚实行为 —— 那份输出确实存在。
	mgr, _, root, _ := newArtifactManager(t)
	ctx := artifactCtx(mgr, root, "")
	ref, ok := SpillHookOutput(ctx, "hook-backed", "payload")
	require.True(t, ok)

	artID := regexp.MustCompile(`artifact (\S+)`).FindStringSubmatch(ref)
	require.Len(t, artID, 2)
	artifact, err := mgr.ReadArtifact(ctx, artID[1])
	require.NoError(t, err)
	task, err := mgr.Read(ctx, artifact.TaskID)
	require.NoError(t, err, "背书任务必须真实存在（外键不是摆设）")
	assert.Contains(t, task.Title, "hook output:")
	assert.Contains(t, task.Title, "hook-backed")
}
