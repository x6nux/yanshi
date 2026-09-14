// builtinpack.go — 把随二进制发布的内置技能包嵌入并在启动时物化到用户技能目录。
//
// 设计取舍：Loader/Registry/Body/ReadFile 全部建立在 os 文件路径上（Body 惰性
// 读 SKILL.md、ReadFile 做 on-demand 参考文件、S7 内容扫描逐文件跑），给它们
// 再加一条 fs.FS 抽象意味着三条读路径都要分叉。因此这里走"首次物化"路线：
// go:embed 把 skills/builtin-pack/ 打进二进制，启动时把其中每个技能目录
// 物化到用户技能目录（~/.yanshi/skills），仅当同名目录不存在时才写 —— 用户
// 改过的版本永远不被覆盖（first-seen-wins 在目录层面向上抬起）。
//
// 物化而非直接从内存 serve 的第二个理由：skill 的 requires 探针、可信标记、
// 禁用标记、S7 扫描全都以目录为载体，内存态技能会让这些机制全部失效。
//
// 失败语义：物化失败（权限、磁盘）只降级为 stderr 警告，绝不阻断启动 ——
// 内置技能包是便利品，不是正确性依赖。

package skills

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// builtinPack carries the shipped skill directories. Each immediate child of
// builtin-pack/ is one skill dir (SKILL.md plus optional reference files),
// mirroring the on-disk layout the Loader already scans.
//
//go:embed all:builtin-pack
var builtinPack embed.FS

const builtinPackRoot = "builtin-pack"

// MaterializeBuiltinPack writes every embedded skill dir that does not already
// exist under userSkillsDir. Existing directories are left untouched: the user
// wins over the pack, matching the loader's first-seen-wins rule.
//
// It returns the names of the skill dirs it materialized (empty when everything
// was already present) so callers can log what appeared.
func MaterializeBuiltinPack(userSkillsDir string) ([]string, error) {
	entries, err := builtinPack.ReadDir(builtinPackRoot)
	if err != nil {
		return nil, fmt.Errorf("skills: embedded pack unreadable: %w", err)
	}
	var created []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dst := filepath.Join(userSkillsDir, e.Name())
		if _, statErr := os.Stat(dst); statErr == nil {
			continue // user already has (possibly edited) this skill
		}
		if err := materializeDir(builtinPack, filepath.Join(builtinPackRoot, e.Name()), dst); err != nil {
			return created, fmt.Errorf("skills: materialize builtin skill %q: %w", e.Name(), err)
		}
		created = append(created, e.Name())
	}
	return created, nil
}

// materializeDir copies srcDir (inside the embedded FS) to dstDir on disk,
// creating parents. Symlinks are not preserved — embed.FS carries none — and
// file modes are derived from the name: executables are not a thing in a skill
// pack, so everything lands 0o755 for dirs and 0o644 for files.
func materializeDir(fsys embed.FS, srcDir, dstDir string) error {
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return err
	}
	entries, err := fsys.ReadDir(srcDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		src := filepath.Join(srcDir, e.Name())
		dst := filepath.Join(dstDir, e.Name())
		if e.IsDir() {
			if err := materializeDir(fsys, src, dst); err != nil {
				return err
			}
			continue
		}
		data, err := fsys.ReadFile(src)
		if err != nil {
			return err
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// MaterializeSummary renders the one-line boot log for a materialization run.
// Nil/empty counts become "none", so callers never print a bare pair of
// brackets — the zero-value log line should read as a sentence, not debris.
func MaterializeSummary(created []string) string {
	if len(created) == 0 {
		return "none"
	}
	return strings.Join(created, ", ")
}
