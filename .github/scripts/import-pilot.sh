#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT=$(git rev-parse --show-toplevel)
cd "$REPO_ROOT"

TMP_IDX=$(mktemp -u "${TMPDIR:-/tmp}/suda-idx.XXXXXX")
export GIT_INDEX_FILE="$TMP_IDX"
trap 'rm -f "$TMP_IDX"' EXIT

git read-tree --empty

OWNERS_OID=$(printf "* @gosuda/core\n" | git hash-object -w --stdin)
GOWORK_OID=$(cat <<'EOF' | git hash-object -w --stdin
go 1.27.0

use (
	./ipc/hqq/HQQ
	./ipc/hqq/hqq-sqlite3
	./ipc/hqq/hqq-zstd
	./net/ivnp
	./net/portalite
	./web/gosuda
	./web/securl
)
EOF
)

git update-index --add --cacheinfo "100644,${OWNERS_OID},OWNERS"
git update-index --add --cacheinfo "100644,${GOWORK_OID},go.work"

PILOT_LIST=(
  "web/gosuda:gosuda/website"
  "net/ivnp:gosuda/IVNP"
  "ipc/hqq/HQQ:gosuda/HQQ"
  "ipc/hqq/hqq-sqlite3:gosuda/hqq-sqlite3"
  "ipc/hqq/hqq-zstd:gosuda/hqq-zstd"
  "web/securl:gosuda/securl"
  "net/portalite:gosuda/portalite"
)

TRAILERS=""
for entry in "${PILOT_LIST[@]}"; do
  prefix="${entry%%:*}"
  repo="${entry#*:}"
  url="https://github.com/${repo}.git"
  echo "Fetching ${repo} into //${prefix}..." >&2
  git fetch --no-tags --depth=1 "$url" HEAD
  sha=$(git rev-parse FETCH_HEAD)
  tree=$(git rev-parse "FETCH_HEAD^{tree}")
  git read-tree --prefix="${prefix}/" "$tree"
  TRAILERS="${TRAILERS}Suda-Import: ${prefix}=${repo}@${sha}"$'\n'
done

ROOT_TREE=$(git write-tree)
MSG=$(printf "chore: initialize refs/suda/main with 7 pilot repositories\n\n%s" "$TRAILERS")

SNAP=$(git commit-tree "$ROOT_TREE" -m "$MSG")

if git ls-remote --exit-code origin refs/suda/history >/dev/null 2>&1; then
  git fetch --no-tags --depth=1 origin "+refs/suda/history:refs/remotes/origin/suda/history"
  OLD_HIST=$(git rev-parse refs/remotes/origin/suda/history)
  HIST=$(git commit-tree "$ROOT_TREE" -p "$OLD_HIST" -m "$MSG")
  OLD_MAIN=$(git ls-remote origin refs/suda/main | awk '{print $1}')
  git push --atomic origin \
    --force-with-lease="refs/suda/main:${OLD_MAIN}" \
    --force-with-lease="refs/suda/history:${OLD_HIST}" \
    "${SNAP}:refs/suda/main" \
    "${HIST}:refs/suda/history"
else
  git push --atomic origin \
    "${SNAP}:refs/suda/main" \
    "${SNAP}:refs/suda/history"
fi

echo "Published refs/suda/main=${SNAP} (root tree=${ROOT_TREE})"
"${REPO_ROOT}/.github/scripts/update-readme-links.sh" "$SNAP"

