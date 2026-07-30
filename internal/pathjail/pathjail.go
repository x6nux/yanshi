// Package pathjail 提供 canonical root-jail 校验：candidate 必须严格位于 root
// 之内（含 symlink eval、Windows volume 与大小写）。
//
// 这是仓库内 root-jail 的唯一 canonical 实现。`internal/tools` 与
// `internal/task/work` 都通过薄封装复用 WithinRootAbs，禁止复制其算法。
// 依赖方向：tools → pathjail、work → pathjail；**绝无 work → tools**。
package pathjail

import (
	"errors"
	"path/filepath"
	"runtime"
	"strings"
)

// WithinRootAbs 返回 candidate 解析（含 symlink eval）后的绝对路径，并保证它
// 严格位于 root（同样解析后）之内；否则返回 error。Windows 下额外比较 volume
// （EqualFold）并按大小写不敏感复核拼接结果，堵住盘符/大小写绕过。
//
// 注意：candidate 必须是已存在的路径（EvalSymlinks 会 stat），调用方对不存在
// 的候选路径应在调用前先创建或拒绝。janitor 删除 artifact 文件时，metadata
// 一定对应一个曾经存在的文件，因此即使文件已被并发删除也只在 `os.Remove`
// 那一层吞掉错误，不在 WithinRootAbs 这里加额外容忍。
func WithinRootAbs(root, candidate string) (string, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	candidateAbs, err := filepath.Abs(candidate)
	if err != nil {
		return "", err
	}
	rootReal, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return "", err
	}
	candidateReal, err := filepath.EvalSymlinks(candidateAbs)
	if err != nil {
		return "", err
	}
	rootVolume := filepath.VolumeName(rootReal)
	candidateVolume := filepath.VolumeName(candidateReal)
	if runtime.GOOS == "windows" {
		if !strings.EqualFold(rootVolume, candidateVolume) {
			return "", errors.New("pathjail: path is on a different volume")
		}
	} else if rootVolume != candidateVolume {
		return "", errors.New("pathjail: path is on a different volume")
	}
	rel, err := filepath.Rel(rootReal, candidateReal)
	if err != nil {
		return "", err
	}
	if rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("pathjail: path escapes work root")
	}
	if runtime.GOOS == "windows" {
		// filepath.Rel 在 Windows 上可能依赖大小写；用 EqualFold 复核 join 结果。
		joined := filepath.Clean(filepath.Join(rootReal, rel))
		if !strings.EqualFold(joined, filepath.Clean(candidateReal)) {
			return "", errors.New("pathjail: path escapes work root")
		}
	}
	return candidateReal, nil
}
