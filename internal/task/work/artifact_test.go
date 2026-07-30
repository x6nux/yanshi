package work

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newManagerWithTmpRoot 构造一个 Manager 与一个临时 root 目录，用于测试
// artifact 落盘、quota、TTL 与 SecureArtifactPath。
func newManagerWithTmpRoot(t *testing.T, policy ArtifactPolicy) (*Manager, *Store, string) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	st, err := FromDB(db, nil)
	require.NoError(t, err)
	root := t.TempDir()
	mgr := NewManager(st, nil, policy)
	return mgr, st, root
}

func TestManagerWriteArtifactPersistsAndRollsBack(t *testing.T) {
	ctx := context.Background()
	mgr, st, root := newManagerWithTmpRoot(t, ArtifactPolicy{})

	task, err := mgr.Create(ctx, CreateReq{Title: "x", Prompt: "p"})
	require.NoError(t, err)

	body := []byte("hello\nworld")
	art, err := mgr.WriteArtifact(ctx, task.ID, "log", body, root)
	require.NoError(t, err)
	assert.NotEmpty(t, art.ID)
	assert.Equal(t, int64(len(body)), art.Size)
	assert.Contains(t, art.ContentRef, ".yanshi/artifacts/"+task.ID+"/")

	// 文件落盘
	abs, err := SecureArtifactPath(root, art.ContentRef)
	require.NoError(t, err)
	got, err := os.ReadFile(abs)
	require.NoError(t, err)
	assert.Equal(t, body, got)

	// metadata 落库
	stored, err := st.GetArtifact(ctx, art.ID)
	require.NoError(t, err)
	assert.Equal(t, art.ID, stored.ID)
	assert.Equal(t, art.Size, stored.Size)
}

func TestManagerWriteArtifactQuotaExceededNoLeak(t *testing.T) {
	ctx := context.Background()
	// Quota = 15 bytes，刚好放一个 10 字节的；第二个 6 字节会被拒（10+6=16>15）
	mgr, st, root := newManagerWithTmpRoot(t, ArtifactPolicy{QuotaBytes: 15, TTL: time.Hour})

	task, err := mgr.Create(ctx, CreateReq{Title: "x", Prompt: "p"})
	require.NoError(t, err)

	// 第一个 10 字节 artifact 成功
	_, err = mgr.WriteArtifact(ctx, task.ID, "a", []byte("0123456789"), root)
	require.NoError(t, err)

	// 第二个 10 字节会被拒（10 + 10 > 16）
	_, err = mgr.WriteArtifact(ctx, task.ID, "b", []byte("ABCDEF"), root)
	require.Error(t, err)

	// 第二个的 tmp/目标文件不应残留
	entries, err := os.ReadDir(filepath.Join(root, ".yanshi", "artifacts", task.ID))
	require.NoError(t, err)
	// 只有第一个 artifact 文件 + 可能的 .tmp（必须为 0 个 .tmp）
	var files []string
	var tmps []string
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" || filepath.Base(e.Name())[len(e.Name())-4:] == ".tmp" {
			tmps = append(tmps, e.Name())
			continue
		}
		files = append(files, e.Name())
	}
	assert.Empty(t, tmps, "no .tmp residue on quota rejection; got %v", tmps)
	assert.Len(t, files, 1)

	// metadata 也只有 1 条
	used, err := st.ArtifactBytes(ctx, task.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(10), used)
}

func TestSweepArtifactsDeletesMetadataAndFile(t *testing.T) {
	ctx := context.Background()
	mgr, st, root := newManagerWithTmpRoot(t, ArtifactPolicy{})

	task, err := mgr.Create(ctx, CreateReq{Title: "x", Prompt: "p"})
	require.NoError(t, err)

	// 写两个 artifact；手动把第一个的 created_at 调到 3 天前
	old1, err := mgr.WriteArtifact(ctx, task.ID, "old", []byte("old"), root)
	require.NoError(t, err)
	newArt, err := mgr.WriteArtifact(ctx, task.ID, "new", []byte("new"), root)
	require.NoError(t, err)

	oldTS := time.Now().Add(-3 * 24 * time.Hour).Unix()
	_, err = st.db.ExecContext(ctx, `UPDATE task_work_artifacts SET created_at=? WHERE id=?`, oldTS, old1.ID)
	require.NoError(t, err)

	// 文件确实在
	oldAbs, err := SecureArtifactPath(root, old1.ContentRef)
	require.NoError(t, err)
	_, err = os.Stat(oldAbs)
	require.NoError(t, err)

	// 扫 1 天前
	require.NoError(t, SweepArtifacts(ctx, st, root, time.Now().Add(-24*time.Hour)))

	// metadata 删了一条
	_, err = st.GetArtifact(ctx, old1.ID)
	require.Error(t, err)
	// new 仍在
	_, err = st.GetArtifact(ctx, newArt.ID)
	require.NoError(t, err)

	// 文件也删了
	_, err = os.Stat(oldAbs)
	require.True(t, os.IsNotExist(err), "old artifact file must be removed; stat err=%v", err)
}

func TestSummarizeArtifact(t *testing.T) {
	// empty
	assert.Equal(t, "(empty)", summarizeArtifact([]byte("")))
	assert.Equal(t, "(empty)", summarizeArtifact([]byte("   \n\t")))
	// binary
	assert.Equal(t, "(binary)", summarizeArtifact([]byte("hello\x00world")))
	// single line, short
	assert.Equal(t, "hi there", summarizeArtifact([]byte("hi there")))
	// multiline, first line wins
	assert.Equal(t, "first", summarizeArtifact([]byte("first\nsecond\nthird")))
	// long line truncated to 120 runes
	long := make([]rune, 500)
	for i := range long {
		long[i] = 'x'
	}
	s := summarizeArtifact([]byte(string(long)))
	assert.LessOrEqual(t, len([]rune(s)), 120)
}
