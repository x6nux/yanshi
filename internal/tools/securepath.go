// tools 包的 securepath 薄封装：唯一目的是让 gate cwd 与未来的 artifact_read
// 复用同一个 canonical root-jail（pathjail.WithinRootAbs），**不复制** 算法。
// 依赖方向：tools → pathjail。
package tools

import "github.com/x6nux/yanshi/internal/pathjail"

// withinRootAbs 是 tools 包内的薄封装，供 gate cwd、artifact_read 复用，
// 避免这些调用点直接 import pathjail 散落各处。
//
// 与本包 fs.go 里已有的 withinRoot（clean+prefix 字符串比较）的区别：
// fs.go 的 withinRoot 不做 symlink eval，只用于已 clean 过的路径快速比较；
// withinRootAbs 会 EvalSymlinks，能堵住 symlink escape。
// 二者并存是有意的：fast-path 用字符串，slow-path 用 canonical kernel。
func withinRootAbs(root, candidate string) (string, error) {
	return pathjail.WithinRootAbs(root, candidate)
}
