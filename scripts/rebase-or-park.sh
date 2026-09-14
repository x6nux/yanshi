#!/bin/bash
# rebase-or-park.sh — 拉取远程更新 rebase 进当前分支；冲突严重就把当前更改存到一个备用分支。
# 用法: rebase-or-park.sh [branch]   (默认当前分支)
#
# 行为:
#   1. git fetch origin
#   2. 若 upstream 无新提交 → 结束（Nothing to do）
#   3. git rebase origin/<branch>
#      - 成功 → 完成
#      - 冲突 → 先尝试 rerere 自动解决；仍冲突则统计冲突文件数
#        * ≤2 个文件 → 停在冲突态等人工（不破坏任何东西）
#        * >2 个文件（严重冲突）→ rebase --abort，把本地提交原样推到
#          park/<branch>-<date> 分支保存，本地 reset 回 origin/<branch>
set -euo pipefail

BRANCH="${1:-$(git rev-parse --abbrev-ref HEAD)}"
PARK_PREFIX="park"
# PR 特性分支默认 rebase 到 origin/main（PR 的意义就是进 main）；
# 若分支名以 park/ 开头或显式传了第二参数，则用 origin/<branch> 自己。
BASE="${2:-main}"
if [[ "${BRANCH}" == park/* || "${BRANCH}" == main ]]; then
  BASE="${BRANCH}"
fi
UPSTREAM="origin/${BASE}"

git fetch origin

BEHIND=$(git rev-list --count "HEAD..${UPSTREAM}")
if [ "$BEHIND" -eq 0 ]; then
  echo "OK: ${BRANCH} 已是最新（无远程更新）"
  exit 0
fi
echo "远程 ${UPSTREAM} 有 ${BEHIND} 个新提交，尝试 rebase..."

if git rebase "${UPSTREAM}" 2>/dev/null; then
  echo "OK: rebase 干净完成"
  exit 0
fi

# rerere 已在 git 配置时可能自动解决一部分；这里处理仍然冲突的情况
CONFLICTS=$(git diff --name-only --diff-filter=U | wc -l | tr -d ' ')
echo "rebase 冲突，涉及 ${CONFLICTS} 个文件"

if [ "${CONFLICTS}" -le 2 ]; then
  echo "冲突较少（≤2 文件），已停在冲突态，请手动解决后 'git rebase --continue'。"
  echo "放弃本次 rebase: git rebase --abort"
  exit 1
fi

# 严重冲突：abort，把本地提交park到备份分支
BASE="$(git rev-parse --short "${UPSTREAM}")"
PARK_BRANCH="${PARK_PREFIX}/${BRANCH}-${BASE}-$(date +%Y%m%d-%H%M%S)"
git rebase --abort
LOCAL_HEAD="$(git rev-parse --short HEAD)"
git branch "${PARK_BRANCH}" "${LOCAL_HEAD}"
git reset --hard "${UPSTREAM}"
echo "PARKED: 严重冲突（${CONFLICTS} 文件）。本地提交已保存到分支 ${PARK_BRANCH}"
echo "  原 HEAD: ${LOCAL_HEAD}"
echo "  本地 ${BRANCH} 已与 ${UPSTREAM} 对齐。"
echo "  事后恢复: git cherry-pick ${LOCAL_HEAD}  或  git rebase --onto ${BRANCH} ${BASE} ${PARK_BRANCH}"
exit 2
