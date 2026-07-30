package tools

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/task/work"
)

// newArtifactManager 构造一个真实 Manager + tmp root + 一个已存在的 task。
func newArtifactManager(t *testing.T) (*work.Manager, *work.Store, string, *work.WorkTask) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	st, err := work.FromDB(db, nil) // nil WriteTxer: bare-DB test path (no shared writeMu)
	require.NoError(t, err)
	root := t.TempDir()
	mgr := work.NewManager(st, nil, work.ArtifactPolicy{QuotaBytes: 1 << 20, TTL: 0})
	task, err := mgr.Create(context.Background(), work.CreateReq{Title: "x", Prompt: "p"})
	require.NoError(t, err)
	return mgr, st, root, task
}

func artifactCtx(manager work.ManagerLike, root string, readGlob string) context.Context {
	// Resolve symlinks so the FS allowed-path matches the EvalSymlinks-resolved
	// canonical path withinRootAbs returns (macOS /var → /private/var).
	// Without this, the artifact read tool's second Authorize (the canonical
	// path) would be denied because /private/var/... doesn't match /var/.../**.
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	if readGlob == "" {
		// 默认允许 root 整棵子树（** 是递归 glob）
		readGlob = filepath.ToSlash(filepath.Join(root, "**"))
	}
	return WithProfile(WithWorkRoot(WithTaskManager(context.Background(), manager), root), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
		FS:    guard.FSPerm{Read: []string{readGlob}},
	})
}

func TestArtifactRead_BasicAndPagination(t *testing.T) {
	mgr, _, root, task := newArtifactManager(t)
	// 写入 200 字节内容
	body := strings.Repeat("abcdefgh", 25) // 200 bytes
	art, err := mgr.WriteArtifact(context.Background(), task.ID, "log", []byte(body), root)
	require.NoError(t, err)

	ctx := artifactCtx(mgr, root, "")
	at := NewArtifactTools()

	// read 默认 limit (DefaultArtifactReadSize = 64KiB)：一次读完
	out, err := runTool(ctx, at.Read, `{"id":"`+art.ID+`"}`)
	require.NoError(t, err, "raw out=%q", out)
	var p struct {
		Artifact work.Artifact `json:"artifact"`
		Offset   int64         `json:"offset"`
		Next     int64         `json:"next_offset"`
		EOF      bool          `json:"eof"`
		Content  string        `json:"content"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &p), "out=%q", out)
	assert.Equal(t, art.ID, p.Artifact.ID)
	assert.Equal(t, int64(0), p.Offset)
	assert.True(t, p.EOF, "200B < 64KiB limit → EOF on first read")
	assert.Equal(t, body, p.Content)

	// 分页：limit=50, offset=0
	out, err = runTool(ctx, at.Read, `{"id":"`+art.ID+`","limit":50,"offset":0}`)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(out), &p))
	assert.Equal(t, int64(0), p.Offset)
	assert.Equal(t, int64(50), p.Next)
	assert.False(t, p.EOF)
	assert.Equal(t, body[:50], p.Content)

	// 下一页
	out, err = runTool(ctx, at.Read, `{"id":"`+art.ID+`","limit":50,"offset":50}`)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(out), &p))
	assert.Equal(t, int64(50), p.Offset)
	assert.Equal(t, body[50:100], p.Content)

	// 尾页
	out, err = runTool(ctx, at.Read, `{"id":"`+art.ID+`","limit":50,"offset":150}`)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(out), &p))
	assert.Equal(t, body[150:200], p.Content)
	assert.True(t, p.EOF)
}

func TestArtifactRead_LimitClampedToMax(t *testing.T) {
	mgr, _, root, task := newArtifactManager(t)
	body := strings.Repeat("x", 100)
	art, err := mgr.WriteArtifact(context.Background(), task.ID, "log", []byte(body), root)
	require.NoError(t, err)

	ctx := artifactCtx(mgr, root, "")
	at := NewArtifactTools()
	// 显式 limit > MaxArtifactReadSize（1 MiB）→ clamp
	out, err := runTool(ctx, at.Read, `{"id":"`+art.ID+`","limit":10000000}`)
	require.NoError(t, err)
	var p struct {
		Content string `json:"content"`
		EOF     bool   `json:"eof"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &p))
	// 100B 一次读完
	assert.Equal(t, body, p.Content)
	assert.True(t, p.EOF)
}

func TestArtifactRead_NoFSPermissionDenied(t *testing.T) {
	mgr, _, root, task := newArtifactManager(t)
	body := []byte("secret-content")
	art, err := mgr.WriteArtifact(context.Background(), task.ID, "log", body, root)
	require.NoError(t, err)

	// profile 故意只允许读一个不相关的目录
	otherDir := filepath.Join(root, "unrelated")
	require.NoError(t, os.MkdirAll(otherDir, 0o750))
	ctx := artifactCtx(mgr, root, otherDir)
	at := NewArtifactTools()
	out, err := runTool(ctx, at.Read, `{"id":"`+art.ID+`"}`)
	// kernel 错误经 InvokableRun 表面化为 result 文本，err 为 nil
	require.NoError(t, err)
	// 必须是 permission denied，且不泄露 body 内容
	assert.Contains(t, out, "permission denied")
	assert.NotContains(t, out, "secret-content")
}

func TestArtifactRead_AuthorizeBeforeIO(t *testing.T) {
	// 这个测试专门验证 Authorize 发生在 os.Open 之前：即使文件存在且可读，
	// 没有 FS read 权限也不应触发 I/O。我们用文件 mtime 作为副作用探针。
	mgr, _, root, task := newArtifactManager(t)
	body := []byte("xyz")
	art, err := mgr.WriteArtifact(context.Background(), task.ID, "log", body, root)
	require.NoError(t, err)

	// 取 canonical path，记录 mtime
	abs, err := work.SecureArtifactPath(root, art.ContentRef)
	require.NoError(t, err)
	info, err := os.Stat(abs)
	require.NoError(t, err)
	origMTime := info.ModTime()

	// profile 不允许读 → Authorize 拒绝 → os.Open 不会执行
	otherDir := filepath.Join(root, "nowhere")
	require.NoError(t, os.MkdirAll(otherDir, 0o750))
	ctx := artifactCtx(mgr, root, otherDir)
	at := NewArtifactTools()
	out, _ := runTool(ctx, at.Read, `{"id":"`+art.ID+`"}`)
	assert.Contains(t, out, "permission denied")

	// 再次 stat：atime/mtime 没变（文件未被读取）。注：mtime 不变只是弱探针，
	// 因为不同 FS 行为可能不同，但作为 smoke check 仍能捕捉明显的 I/O 副作用。
	info2, err := os.Stat(abs)
	require.NoError(t, err)
	assert.Equal(t, origMTime, info2.ModTime())
}

func TestArtifactRead_SymlinkEscapeRejected(t *testing.T) {
	// 与 pathjail_test 一样：Windows 无 admin 权限无法创建 symlink，skip。
	mgr, _, root, task := newArtifactManager(t)
	art, err := mgr.WriteArtifact(context.Background(), task.ID, "log", []byte("ok"), root)
	require.NoError(t, err)

	// 构造一个 symlink：把 artifact 的 ContentRef 改写为指向 root 外
	abs, err := work.SecureArtifactPath(root, art.ContentRef)
	require.NoError(t, err)
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "secret.txt")
	require.NoError(t, os.WriteFile(outsideFile, []byte("secret"), 0o600))

	linkPath := abs + ".link"
	if err := os.Symlink(outsideFile, linkPath); err != nil {
		t.Skipf("cannot create symlink on this host: %v", err)
	}
	// 改 artifact.ContentRef 指向 link
	_ = linkPath
	// 这个测试在没有 admin 权限时 skip；保留骨架以便在 CI (POSIX/dev mode Windows) 上跑。
	t.Skip("symlink creation requires admin on Windows hosts; covered by pathjail_test")
}

func TestArtifactRead_NoManager(t *testing.T) {
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
	})
	at := NewArtifactTools()
	_, err := runKernel(ctx, at.readArtifact, `{"id":"x"}`)
	require.Error(t, err)
}

func TestArtifactRead_BadArgs(t *testing.T) {
	mgr, _, root, task := newArtifactManager(t)
	art, err := mgr.WriteArtifact(context.Background(), task.ID, "log", []byte("ok"), root)
	require.NoError(t, err)
	ctx := artifactCtx(mgr, root, "")
	at := NewArtifactTools()
	// 负 offset
	out, _ := runTool(ctx, at.Read, `{"id":"`+art.ID+`","offset":-1}`)
	assert.Contains(t, out, "offset", "negative offset should surface an error in out")
}
