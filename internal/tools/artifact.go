// artifact_read: authorize-before-I/O 读取 WorkTask 持久化工件。
//
// 这是 durable task 工具族里唯一会读取已落盘文件的入口。它的设计约束：
//   - Manager.ReadArtifact 只返回 metadata（含 ContentRef），不读字节；
//   - lexical path（filepath.Join(root, ContentRef)）先过一次 Authorize，
//     让 profile/guard 在 EvalSymlinks 之前就能拒绝（无 FS read 权限即短路）；
//   - canonical path（withinRootAbs 解析 symlink 后）再过一次 Authorize，
//     堵住 symlink 把已批准的 lexical path 指向 root 外；
//   - 两次 Authorize 都通过后，才 os.Open + Seek + Read；
//   - limit 上限 MaxArtifactReadSize（1 MiB）；超出 clamp 而非拒绝。
//
// 与已决策约束 §11、§14、Task 4 pathjail 一致。
package tools

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/task/work"
)

// ArtifactTools 暴露 artifact_read 工具。
type ArtifactTools struct {
	Read *GuardedTool
}

// NewArtifactTools 构造 artifact_read。默认 30s timeout（文件读取 + Authorize）。
func NewArtifactTools() *ArtifactTools {
	at := &ArtifactTools{}
	at.Read = NewGuardedTool(
		"artifact_read", "Artifact Read",
		"Read a slice of a durable task artifact (file). Authorized before I/O.",
		30*time.Second,
		params(map[string]*schema.ParameterInfo{
			"id":     {Type: schema.String, Desc: "artifact id", Required: true},
			"offset": {Type: schema.Integer, Desc: "byte offset (default 0)"},
			"limit":  {Type: schema.Integer, Desc: "max bytes (default 64KiB, max 1MiB)"},
		}),
		SyncStream(at.readArtifact),
	)
	return at
}

// Tools returns all artifact tools as a slice for convenience.
func (a *ArtifactTools) Tools() []*GuardedTool { return []*GuardedTool{a.Read} }

type artifactReadArgs struct {
	ID     string `json:"id"`
	Offset int64  `json:"offset"`
	Limit  int    `json:"limit"`
}

// readArtifact 是 artifact_read 的 kernel。两次 Authorize 之间夹着 withinRootAbs：
// 第一次拒绝无 FS read 权限的请求；第二次拒绝 symlink escape。
func (a *ArtifactTools) readArtifact(ctx context.Context, argsJSON string) (string, error) {
	var args artifactReadArgs
	if err := ParseArgs(argsJSON, &args); err != nil {
		return "", err
	}
	if args.Offset < 0 {
		return "", errors.New("tools: offset must be non-negative")
	}
	manager, err := requireTaskManager(ctx)
	if err != nil {
		return "", err
	}
	artifact, err := manager.ReadArtifact(ctx, args.ID)
	if err != nil {
		return "", err
	}
	root := WorkRootFromContext(ctx)
	if root == "" {
		root = "."
	}
	lexical := filepath.Join(root, filepath.FromSlash(artifact.ContentRef))
	// (1) lexical path authorize：在 EvalSymlinks 之前。
	// 无 FS read 权限的 profile 在这里短路，不触发任何 I/O。
	if err := Authorize(ctx, guard.Action{
		Tool: "artifact_read",
		FS:   guard.FSWant{Op: "read", Paths: []string{lexical}},
	}, argsJSON); err != nil {
		return "", err
	}
	// (2) canonical path resolve：pathjail canonical kernel（EvalSymlinks + volume + case）。
	// symlink escape 在这里被拒绝。
	canonical, err := withinRootAbs(root, lexical)
	if err != nil {
		return "", err
	}
	// (3) canonical authorize：防止 symlink 把已批准 lexical 指向 root 外的合法路径。
	if err := Authorize(ctx, guard.Action{
		Tool: "artifact_read",
		FS:   guard.FSWant{Op: "read", Paths: []string{canonical}},
	}, argsJSON); err != nil {
		return "", err
	}
	// (4) limit clamp：默认 64KiB，最大 1 MiB；超出 clamp 而非拒绝。
	limit := args.Limit
	if limit <= 0 {
		limit = int(work.DefaultArtifactReadSize)
	}
	if int64(limit) > work.MaxArtifactReadSize {
		limit = int(work.MaxArtifactReadSize)
	}
	// (5) 终于可以打开文件。任何 I/O 错误都是真正的系统错误（非权限拒绝）。
	file, err := os.Open(canonical)
	if err != nil {
		return "", err
	}
	defer file.Close()
	if _, err := file.Seek(args.Offset, io.SeekStart); err != nil {
		return "", err
	}
	buffer := make([]byte, limit)
	n, readErr := file.Read(buffer)
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return "", readErr
	}
	eof := args.Offset+int64(n) >= artifact.Size
	return toJSON(struct {
		Artifact work.Artifact `json:"artifact"`
		Offset   int64         `json:"offset"`
		Next     int64         `json:"next_offset"`
		EOF      bool          `json:"eof"`
		Content  string        `json:"content"`
	}{
		Artifact: artifact,
		Offset:   args.Offset,
		Next:     args.Offset + int64(n),
		EOF:      eof,
		Content:  string(buffer[:n]),
	}), nil
}
