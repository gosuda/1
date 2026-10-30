#!/usr/bin/env bash
set -euo pipefail

overlay_control_plane_tree() {
  local target_tree="$1"
  local ctrl_tree="$2"
  local tmp_idx
  tmp_idx=$(mktemp -u "${TMPDIR:-/tmp}/suda-ctrl-idx.XXXXXX")
  GIT_INDEX_FILE="$tmp_idx" GIT_NO_LAZY_FETCH=1 git --no-lazy-fetch read-tree "$target_tree"
  while IFS= read -r entry; do
    [ -z "$entry" ] && continue
    GIT_INDEX_FILE="$tmp_idx" GIT_NO_LAZY_FETCH=1 git --no-lazy-fetch rm -r --cached --ignore-unmatch -- "$entry" >/dev/null 2>&1 || true
  done < <(git ls-tree --name-only "$ctrl_tree")
  git ls-tree -r "$ctrl_tree" | GIT_INDEX_FILE="$tmp_idx" GIT_NO_LAZY_FETCH=1 git --no-lazy-fetch update-index --index-info
  local out_tree
  out_tree=$(GIT_INDEX_FILE="$tmp_idx" GIT_NO_LAZY_FETCH=1 git --no-lazy-fetch write-tree --missing-ok)
  rm -f "$tmp_idx"
  printf "%s\n" "$out_tree"
}

if [ "${1:-}" = "--overlay-tree" ]; then
  TARGET_TREE="${2:?usage: sync-control-plane.sh --overlay-tree <target-tree> [ctrl-tree]}"
  CTRL_TREE="${3:-HEAD}"
  overlay_control_plane_tree "$TARGET_TREE" "$CTRL_TREE"
  exit 0
fi

REPO_ROOT=$(git rev-parse --show-toplevel)
cd "$REPO_ROOT"

git fetch --filter=blob:none --no-tags origin \
  "+refs/heads/main:refs/remotes/origin/main" \
  "+refs/suda/main:refs/remotes/origin/suda/main" \
  "+refs/suda/history:refs/remotes/origin/suda/history"

CTRL_SHA=$(git rev-parse refs/remotes/origin/main)
CTRL_TREE=$(git rev-parse "${CTRL_SHA}^{tree}")
OLD_MAIN=$(git rev-parse refs/remotes/origin/suda/main)
OLD_HIST=$(git rev-parse refs/remotes/origin/suda/history)
MAIN_TREE=$(git rev-parse "${OLD_MAIN}^{tree}")

NEW_TREE=$(overlay_control_plane_tree "$MAIN_TREE" "$CTRL_TREE")
if [ "$NEW_TREE" = "$MAIN_TREE" ]; then
  echo "refs/suda/main control plane is already synchronized with refs/heads/main (${CTRL_SHA:0:7})"
  exit 0
fi

MSG=$(printf "chore(control-plane): sync refs/heads/main@%s into refs/suda/main\n\nSuda-Control-Plane-Rev: %s\n" "${CTRL_SHA:0:7}" "$CTRL_SHA")
SNAP=$(git commit-tree "$NEW_TREE" -m "$MSG")
HIST=$(git commit-tree "$NEW_TREE" -p "$OLD_HIST" -m "$MSG")

git push --atomic origin \
  --force-with-lease="refs/suda/main:${OLD_MAIN}" \
  --force-with-lease="refs/suda/history:${OLD_HIST}" \
  "${SNAP}:refs/suda/main" \
  "${HIST}:refs/suda/history"

echo "Synchronized control plane refs/heads/main@${CTRL_SHA:0:7} into refs/suda/main=${SNAP}"
