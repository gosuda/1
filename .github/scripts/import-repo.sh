#!/usr/bin/env bash
set -euo pipefail

PREFIX="${1:?usage: import-repo.sh <monorepo-prefix> <owner/repo> [branch]}"
REPO="${2:?usage: import-repo.sh <monorepo-prefix> <owner/repo> [branch]}"
BRANCH="${3:-main}"
WF_NAME="${REPO##*/}"

REPO_ROOT=$(git rev-parse --show-toplevel)
cd "$REPO_ROOT"

git fetch --filter=blob:none --no-tags origin \
  "+refs/suda/main:refs/remotes/origin/suda/main" \
  "+refs/suda/history:refs/remotes/origin/suda/history"

OLD_MAIN=$(git rev-parse refs/remotes/origin/suda/main)
OLD_HIST=$(git rev-parse refs/remotes/origin/suda/history)
BASE_TREE=$(git rev-parse "${OLD_MAIN}^{tree}")

EXT_URL="https://github.com/${REPO}.git"
git fetch --no-tags --depth=1 "$EXT_URL" "refs/heads/${BRANCH}"
EXT_SHA=$(git rev-parse FETCH_HEAD)
EXT_TREE=$(git rev-parse "FETCH_HEAD^{tree}")

TMP_IDX=$(mktemp -u "${TMPDIR:-/tmp}/suda-import-idx.XXXXXX")
export GIT_INDEX_FILE="$TMP_IDX"
trap 'rm -f "$TMP_IDX"' EXIT

CLEAN_PREFIX="${PREFIX#/}"
CLEAN_PREFIX="${CLEAN_PREFIX%/}"

git read-tree "$BASE_TREE"
git rm -r --cached --ignore-unmatch -- "$CLEAN_PREFIX" >/dev/null 2>&1 || true
git read-tree --prefix="${CLEAN_PREFIX}/" "$EXT_TREE"
NEW_TREE=$(git write-tree)
unset GIT_INDEX_FILE

if CTRL_TREE=$(git rev-parse --verify -q "HEAD^{tree}"); then
  NEW_TREE=$("$(dirname "$0")/sync-control-plane.sh" --overlay-tree "$NEW_TREE" "$CTRL_TREE")
fi

go -C .github/scripts/capy run ./cmd/capy check-visibility -repo "$REPO_ROOT" -base-tree "$BASE_TREE" -tree "$NEW_TREE"

MSG=$(printf "chore(%s): import %s@%s into //%s\n\nSuda-Import: %s=%s@%s\n" \
  "$CLEAN_PREFIX" "$REPO" "${EXT_SHA:0:7}" "$CLEAN_PREFIX" "$CLEAN_PREFIX" "$REPO" "$EXT_SHA")

SNAP=$(git commit-tree "$NEW_TREE" -m "$MSG")
HIST=$(git commit-tree "$NEW_TREE" -p "$OLD_HIST" -m "$MSG")

git push --atomic origin \
  --force-with-lease="refs/suda/main:${OLD_MAIN}" \
  --force-with-lease="refs/suda/history:${OLD_HIST}" \
  "${SNAP}:refs/suda/main" \
  "${HIST}:refs/suda/history"

python3 - "$REPO_ROOT/capy.star" "$REPO_ROOT/docs/README.md" "$WF_NAME" "$CLEAN_PREFIX" "$REPO" "$SNAP" <<'PY'
import re, sys

capy_path, readme_path, wf_name, prefix, repo, snap = sys.argv[1:7]

with open(capy_path, "r", encoding="utf-8") as f:
    capy_src = f.read()

entry = f'    ("{wf_name}", "{prefix}", "{repo}", False),\n'
if f'"{prefix}"' not in capy_src and "PILOT_REPOS = [\n" in capy_src:
    capy_src = capy_src.replace("PILOT_REPOS = [\n", "PILOT_REPOS = [\n" + entry, 1)
    with open(capy_path, "w", encoding="utf-8") as f:
        f.write(capy_src)

with open(readme_path, "r", encoding="utf-8") as f:
    readme_src = f.read()

row = f"| `//{prefix}` | `{repo}` | [Browse `//{prefix}`](https://magpie.gosuda.org/tree/{prefix}) | [Commits `//{prefix}`](https://magpie.gosuda.org/history/{prefix}) |\n"
if f"`//{prefix}`" not in readme_src and "<!-- SUDA_EXPLORER_END -->" in readme_src:
    readme_src = readme_src.replace("<!-- SUDA_EXPLORER_END -->", row + "<!-- SUDA_EXPLORER_END -->", 1)
    with open(readme_path, "w", encoding="utf-8") as f:
        f.write(readme_src)
PY

echo "Imported ${REPO}@${EXT_SHA} into //${CLEAN_PREFIX} (refs/suda/main=${SNAP})"
