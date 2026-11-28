#!/usr/bin/env bash
# Imports https://github.com/<owner/repo> (branch, default main) into <prefix>/ of the current
# workspace branch as one local commit, plus the capy.star PILOT_REPOS entry and the docs/README.md
# explorer row (none for corp/ and experimental/ prefixes). Never pushes. The import is an unmodified
# copy so that the mirror tip equals the export of the prefix: add its Go modules to the root go.work in
# a second commit with `go-work-add.sh <prefix>`. Run `git ws-save` and `git ws-land` afterwards.
#
# Usage: import-repo.sh <prefix> <owner/repo> [branch]
#   SUDA_IMPORT_URL overrides the fetch URL (tests use a local repository).
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=workspace-lib.sh
. "$SCRIPT_DIR/workspace-lib.sh"

if [ $# -lt 2 ] || [ $# -gt 3 ]; then
  ws_usage "usage: import-repo.sh <prefix> <owner/repo> [branch]"
fi
PREFIX="$1"
REPO="$2"
BRANCH="${3:-main}"
ws_validate_prefix "$PREFIX"
ws_validate_repo "$REPO"
ws_validate_branch "$BRANCH"
NAME="${REPO##*/}"

ws_check_workspace
ws_prepare_sparse "$PREFIX"

URL="${SUDA_IMPORT_URL:-https://github.com/$REPO.git}"
git fetch --quiet --no-tags --depth=1 "$URL" "refs/heads/$BRANCH"
SRC_SHA=$(git rev-parse FETCH_HEAD)
SRC_TREE=$(git rev-parse "FETCH_HEAD^{tree}")

ws_register "$NAME" "$PREFIX" "$REPO" "$BRANCH"

git rm -r -q --ignore-unmatch -- "$PREFIX"
git read-tree --prefix="$PREFIX/" -u "$SRC_TREE"

MSG=$(printf 'chore(%s): import %s@%s into //%s\n\nSuda-Import: %s=%s@%s' \
  "$PREFIX" "$REPO" "${SRC_SHA:0:7}" "$PREFIX" "$PREFIX" "$REPO" "$SRC_SHA")

if ws_commit "$PREFIX" "$MSG"; then
  ws_next_steps "$REPO" "$PREFIX"
fi
