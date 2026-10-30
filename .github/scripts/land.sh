#!/usr/bin/env bash
set -euo pipefail

LAND_REF="${1:?usage: land.sh refs/heads/land/<id>}"
REPO_ROOT=$(git rev-parse --show-toplevel)
cd "$REPO_ROOT"

git fetch --filter=blob:none origin \
  "+refs/heads/main:refs/remotes/origin/main" \
  "+refs/suda/main:refs/remotes/origin/suda/main" \
  "+refs/suda/history:refs/remotes/origin/suda/history" \
  "+${LAND_REF}:${LAND_REF}"

OLD_MAIN=$(git rev-parse refs/remotes/origin/suda/main)
OLD_HIST=$(git rev-parse refs/remotes/origin/suda/history)
WORK_SHA=$(git rev-parse "$LAND_REF")

BASE=$(git rev-list --max-parents=0 "$WORK_SHA")
if [ "$(wc -l <<<"$BASE" | tr -d ' ')" -ne 1 ]; then
  echo "error: workspace ${LAND_REF} has multiple root commits" >&2
  exit 2
fi

BASE_TREE=$(git rev-parse "${BASE}^{tree}")
MAIN_TREE=$(git rev-parse "${OLD_MAIN}^{tree}")
WORK_TREE=$(git rev-parse "${WORK_SHA}^{tree}")

if [ "$MAIN_TREE" = "$BASE_TREE" ]; then
  NEW_TREE="$WORK_TREE"
elif OUT=$(git merge-tree --write-tree --merge-base="$BASE_TREE" "$MAIN_TREE" "$WORK_TREE"); then
  NEW_TREE=$(head -n1 <<<"$OUT")
else
  rc=$?
  echo "$OUT" >&2
  exit "$rc"
fi

CAPY_BIN=$(mktemp "${TMPDIR:-/tmp}/capy-bin.XXXXXX")
trap 'rm -f "$CAPY_BIN"' EXIT
go -C .github/scripts/capy build -o "$CAPY_BIN" ./cmd/capy

"$CAPY_BIN" check-visibility -repo "$REPO_ROOT" -base-tree "$MAIN_TREE" -tree "$NEW_TREE"

RAW_MSG=$(git log -1 --format="%B" "$WORK_SHA")
GIT_AUTHOR_NAME=$(git log -1 --format="%an" "$WORK_SHA")
GIT_AUTHOR_EMAIL=$(git log -1 --format="%ae" "$WORK_SHA")
GIT_AUTHOR_DATE=$(git log -1 --format="%aI" "$WORK_SHA")
export GIT_AUTHOR_NAME GIT_AUTHOR_EMAIL GIT_AUTHOR_DATE

"$CAPY_BIN" check-owners \
  -repo "$REPO_ROOT" \
  -base-tree "$MAIN_TREE" \
  -tree "$NEW_TREE" \
  -author "${GIT_AUTHOR_NAME} <${GIT_AUTHOR_EMAIL}>" \
  -msg "$RAW_MSG"

if CTRL_TREE=$(git rev-parse --verify -q "refs/remotes/origin/main^{tree}"); then
  NEW_TREE=$("$(dirname "$0")/sync-control-plane.sh" --overlay-tree "$NEW_TREE" "$CTRL_TREE")
fi

SNAP=$(git commit-tree "$NEW_TREE" -m "$RAW_MSG" -m "Suda-Landed-From: ${LAND_REF}")
HIST=$(git commit-tree "$NEW_TREE" -p "$OLD_HIST" -m "$RAW_MSG" -m "Suda-Landed-From: ${LAND_REF}")

git push --atomic origin \
  --force-with-lease="refs/suda/main:${OLD_MAIN}" \
  --force-with-lease="refs/suda/history:${OLD_HIST}" \
  --force-with-lease="${LAND_REF}:${WORK_SHA}" \
  "${SNAP}:refs/suda/main" \
  "${HIST}:refs/suda/history" \
  ":${LAND_REF}"

TASK_ID="${LAND_REF#refs/heads/land/}"
if [ "$TASK_ID" != "$LAND_REF" ]; then
  git push origin ":refs/heads/base/${TASK_ID}" ":refs/heads/review/${TASK_ID}" >/dev/null 2>&1 || true
fi

if [ -n "${MIRROR_TOKEN:-}" ]; then
  while IFS=$'\t' read -r wf_name prefix dest_repo branch; do
    old_sub=$(git rev-parse --verify -q "${MAIN_TREE}:${prefix}" || true)
    new_sub=$(git rev-parse --verify -q "${NEW_TREE}:${prefix}" || true)
    if [ -z "$new_sub" ] || [ "$old_sub" = "$new_sub" ]; then
      continue
    fi
    export_tree=$("$CAPY_BIN" export-tree \
      -repo "$REPO_ROOT" -config "${REPO_ROOT}/capy.star" -workflow "$wf_name" -tree "$NEW_TREE")
    export_msg=$("$CAPY_BIN" format-msg \
      -config "${REPO_ROOT}/capy.star" -workflow "$wf_name" -source-rev "$SNAP" -msg "$RAW_MSG")
    ext_url="https://x-access-token:${MIRROR_TOKEN}@github.com/${dest_repo}.git"
    git fetch --filter=blob:none --no-tags --depth=1 "$ext_url" "+refs/heads/${branch}:refs/remotes/mirror/${wf_name}"
    ext_parent=$(git rev-parse "refs/remotes/mirror/${wf_name}")
    if old_wf_tree=$(git rev-parse --verify -q "${ext_parent}:.github/workflows"); then
      new_wf_tree=$(git rev-parse --verify -q "${export_tree}:.github/workflows" || true)
      if [ "$old_wf_tree" != "$new_wf_tree" ]; then
        TMP_WF_IDX=$(mktemp -u "${TMPDIR:-/tmp}/suda-wf-idx.XXXXXX")
        GIT_INDEX_FILE="$TMP_WF_IDX" GIT_NO_LAZY_FETCH=1 git --no-lazy-fetch read-tree "$export_tree"
        GIT_INDEX_FILE="$TMP_WF_IDX" GIT_NO_LAZY_FETCH=1 git --no-lazy-fetch rm -r --cached --ignore-unmatch -- .github/workflows >/dev/null 2>&1 || true
        git ls-tree -r "$old_wf_tree" | awk -F'\t' '{print $1 "\t.github/workflows/" $2}' | \
          GIT_INDEX_FILE="$TMP_WF_IDX" GIT_NO_LAZY_FETCH=1 git --no-lazy-fetch update-index --index-info
        export_tree=$(GIT_INDEX_FILE="$TMP_WF_IDX" GIT_NO_LAZY_FETCH=1 git --no-lazy-fetch write-tree --missing-ok)
        rm -f "$TMP_WF_IDX"
      fi
    fi
    git diff-tree -r -z --no-commit-id --diff-filter=AM "$ext_parent" "$export_tree" | \
      perl -0ne 'if (/^:\S+\s+\S+\s+([0-9a-f]+)\s+([0-9a-f]+)\s+[AM]/) { print "$1\n" unless $1 =~ /^0+$/; print "$2\n"; }' | \
      git cat-file --batch >/dev/null
    ext_commit=$(printf "%s" "$export_msg" | git commit-tree "$export_tree" -p "$ext_parent")
    git push --force-with-lease="refs/heads/${branch}:${ext_parent}" \
      "$ext_url" "${ext_commit}:refs/heads/${branch}"
  done < <("$CAPY_BIN" list -config "${REPO_ROOT}/capy.star")
fi
