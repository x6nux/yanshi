// work 包的 securepath 薄封装：唯一目的是让 janitor 与未来的 ReadArtifact
// 能 canonical 化一个 artifact content_ref（相对 root），同时 jail 它到 root
// 之内，**不复制** pathjail 的 symlink/volume/case 算法。依赖方向：work → pathjail。
package work

import (
	"path/filepath"

	"github.com/x6nux/yanshi/internal/pathjail"
)

// SecureArtifactPath 把 artifact 的 content_ref（相对 root，使用 '/' 分隔）
// 解析并 jail 到 root 内。返回的绝对路径可直接传给 os.ReadFile / os.Remove。
//
// 失败原因：ref 解析后逃逸出 root（含 symlink escape、跨 volume、Windows
// 大小写绕过）；或 ref / root 在 EvalSymlinks 时不存在。janitor 在调用后
// 用 `os.Remove` 吞掉 ENOENT，所以不存在文件不是问题。
func SecureArtifactPath(root, ref string) (string, error) {
	return pathjail.WithinRootAbs(root, filepath.Join(root, filepath.FromSlash(ref)))
}
