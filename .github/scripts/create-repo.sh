#!/usr/bin/env bash
set -euo pipefail

PREFIX="${1:?usage: create-repo.sh <monorepo-prefix> <owner/repo> [go-module-path] [public|private]}"
REPO="${2:?usage: create-repo.sh <monorepo-prefix> <owner/repo> [go-module-path] [public|private]}"
MOD_PATH="${3:-gosuda.org/${REPO##*/}}"
VISIBILITY="${4:-public}"
WF_NAME="${REPO##*/}"

REPO_ROOT=$(git rev-parse --show-toplevel)
cd "$REPO_ROOT"

CLEAN_PREFIX="${PREFIX#/}"
CLEAN_PREFIX="${CLEAN_PREFIX%/}"

git fetch --filter=blob:none --no-tags origin \
  "+refs/suda/main:refs/remotes/origin/suda/main" \
  "+refs/suda/history:refs/remotes/origin/suda/history"

OLD_MAIN=$(git rev-parse refs/remotes/origin/suda/main)
OLD_HIST=$(git rev-parse refs/remotes/origin/suda/history)
BASE_TREE=$(git rev-parse "${OLD_MAIN}^{tree}")

README_OID=$(printf "# %s\n\nPackage \`%s\` in \`//%s\`.\n" "$WF_NAME" "$MOD_PATH" "$CLEAN_PREFIX" | git hash-object -w --stdin)
OWNERS_OID=$(printf "* @gosuda/core\n" | git hash-object -w --stdin)
GOMOD_OID=$(printf "module %s\n\ngo 1.27.0\n" "$MOD_PATH" | git hash-object -w --stdin)

TMP_IDX=$(mktemp -u "${TMPDIR:-/tmp}/suda-create-idx.XXXXXX")
export GIT_INDEX_FILE="$TMP_IDX"
trap 'rm -f "$TMP_IDX"' EXIT

git read-tree "$BASE_TREE"
git update-index --add --cacheinfo "100644,${README_OID},${CLEAN_PREFIX}/README.md"
git update-index --add --cacheinfo "100644,${OWNERS_OID},${CLEAN_PREFIX}/OWNERS"
git update-index --add --cacheinfo "100644,${GOMOD_OID},${CLEAN_PREFIX}/go.mod"
NEW_TREE=$(git write-tree)
unset GIT_INDEX_FILE

if CTRL_TREE=$(git rev-parse --verify -q "HEAD^{tree}"); then
  NEW_TREE=$("$(dirname "$0")/sync-control-plane.sh" --overlay-tree "$NEW_TREE" "$CTRL_TREE")
fi

MSG=$(printf "feat(%s): initialize project //%s (%s)\n" "$CLEAN_PREFIX" "$CLEAN_PREFIX" "$REPO")
SNAP=$(git commit-tree "$NEW_TREE" -m "$MSG")
HIST=$(git commit-tree "$NEW_TREE" -p "$OLD_HIST" -m "$MSG")

git push --atomic origin \
  --force-with-lease="refs/suda/main:${OLD_MAIN}" \
  --force-with-lease="refs/suda/history:${OLD_HIST}" \
  "${SNAP}:refs/suda/main" \
  "${HIST}:refs/suda/history"

if [ "${CREATE_GITHUB_MIRROR:-0}" = "1" ]; then
  if ! gh repo view "$REPO" >/dev/null 2>&1; then
    gh repo create "$REPO" "--${VISIBILITY}" --description "Mirror of //${CLEAN_PREFIX} in gosuda/0"
  fi
  SUB_TREE=$(git rev-parse "${NEW_TREE}:${CLEAN_PREFIX}")
  EXT_MSG=$(printf "%s\n\nGitOrigin-RevId: %s\n" "$MSG" "$SNAP")
  EXT_COMMIT=$(git commit-tree "$SUB_TREE" -m "$EXT_MSG")
  if [ -n "${MIRROR_TOKEN:-}" ]; then
    EXT_URL="https://x-access-token:${MIRROR_TOKEN}@github.com/${REPO}.git"
  else
    EXT_URL="https://github.com/${REPO}.git"
  fi
  git push "$EXT_URL" "${EXT_COMMIT}:refs/heads/main"
fi

python3 - "$REPO_ROOT/capy.star" "$REPO_ROOT/docs/README.md" "$WF_NAME" "$CLEAN_PREFIX" "$REPO" "$SNAP" <<'PY'
import sys

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

echo "Initialized //${CLEAN_PREFIX} in refs/suda/main=${SNAP}"
