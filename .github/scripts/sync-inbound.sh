#!/usr/bin/env bash
set -euo pipefail

WF_NAME="${1:?usage: sync-inbound.sh <workflow-name> [external-ref] [ws|land]}"
EXT_REF="${2:-refs/heads/main}"
MODE="${3:-ws}"

REPO_ROOT=$(git rev-parse --show-toplevel)
cd "$REPO_ROOT"

META=$(go -C .github/scripts/capy run ./cmd/capy list -config "${REPO_ROOT}/capy.star" | awk -F'\t' -v w="$WF_NAME" '$1 == w { print $2 "\t" $3 "\t" $4 }')
if [ -z "$META" ]; then
  echo "error: unknown workflow ${WF_NAME} in capy.star" >&2
  exit 2
fi

IFS=$'\t' read -r PREFIX DEST_REPO _ <<<"$META"

git fetch --filter=blob:none --no-tags origin "+refs/suda/main:refs/remotes/origin/suda/main"
SUDA_MAIN=$(git rev-parse refs/remotes/origin/suda/main)
BASE_TREE=$(git rev-parse "${SUDA_MAIN}^{tree}")

if [ -n "${MIRROR_TOKEN:-}" ]; then
  EXT_URL="https://x-access-token:${MIRROR_TOKEN}@github.com/${DEST_REPO}.git"
else
  EXT_URL="https://github.com/${DEST_REPO}.git"
fi

git fetch --no-tags --depth=1 "$EXT_URL" "$EXT_REF"
EXT_SHA=$(git rev-parse FETCH_HEAD)
EXT_MSG=$(git log -1 --format="%B" "$EXT_SHA")
GIT_AUTHOR_NAME=$(git log -1 --format="%an" "$EXT_SHA")
GIT_AUTHOR_EMAIL=$(git log -1 --format="%ae" "$EXT_SHA")
GIT_AUTHOR_DATE=$(git log -1 --format="%aI" "$EXT_SHA")
export GIT_AUTHOR_NAME GIT_AUTHOR_EMAIL GIT_AUTHOR_DATE

if grep -q "^GitOrigin-RevId:" <<<"$EXT_MSG"; then
  ORIGIN_SHA=$(awk '/^GitOrigin-RevId:/ { print $2 }' <<<"$EXT_MSG" | tail -n1)
  if [ "$ORIGIN_SHA" = "$SUDA_MAIN" ]; then
    echo "Skipping inbound commit ${EXT_SHA}: already originated from refs/suda/main (${SUDA_MAIN})"
    exit 0
  fi
fi

EXT_TREE=$(git rev-parse "${EXT_SHA}^{tree}")
NEW_ROOT=$(go -C .github/scripts/capy run ./cmd/capy import-tree \
  -repo "$REPO_ROOT" -config "${REPO_ROOT}/capy.star" \
  -workflow "$WF_NAME" -base-tree "$BASE_TREE" -sub-tree "$EXT_TREE")

COMMIT_MSG=$(printf "%s\n\nSuda-Inbound-From: %s@%s\n" "$EXT_MSG" "$DEST_REPO" "$EXT_SHA")
WS_COMMIT=$(git commit-tree "$NEW_ROOT" -p "$SUDA_MAIN" -m "$COMMIT_MSG")

SAFE_REF=$(tr '/:' '-' <<<"${EXT_REF#refs/}")
if [ "$MODE" = "land" ]; then
  TARGET_REF="refs/heads/land/capy-${WF_NAME}-${SAFE_REF}"
else
  TARGET_REF="refs/ws/capy/${WF_NAME}-${SAFE_REF}"
fi

git push --force origin "${WS_COMMIT}:${TARGET_REF}"
echo "Imported ${DEST_REPO}@${EXT_SHA} into ${TARGET_REF} (commit=${WS_COMMIT})"

if [ "$MODE" = "land" ]; then
  "${REPO_ROOT}/.github/scripts/land.sh" "$TARGET_REF"
fi
