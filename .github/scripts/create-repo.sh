#!/usr/bin/env bash
# Creates a project in the current workspace branch as one local commit: <prefix>/README.md,
# <prefix>/OWNERS, <prefix>/go.mod, the capy.star PILOT_REPOS entry, and the docs/README.md
# explorer row, plus a second commit adding the module to the root go.work. corp/ and experimental/
# prefixes get no capy.star entry and no explorer row. Never pushes. Run `git ws-save` and `git ws-land` afterwards.
#
# Usage: create-repo.sh <prefix> <owner/repo> [go-module-path]
#   <prefix>      google3 path under a known domain, e.g. net/relaysrv
#   <owner/repo>  public mirror repository, e.g. gosuda/relaysrv
#   OWNERS login: $SUDA_OWNER, else `gh api user -q .login`
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=workspace-lib.sh
. "$SCRIPT_DIR/workspace-lib.sh"

if [ $# -lt 2 ] || [ $# -gt 3 ]; then
  ws_usage "usage: create-repo.sh <prefix> <owner/repo> [go-module-path]"
fi
PREFIX="$1"
REPO="$2"
ws_validate_prefix "$PREFIX"
ws_validate_repo "$REPO"
NAME="${REPO##*/}"
MOD_PATH="${3:-gosuda.org/$NAME}"
ws_validate_module "$MOD_PATH"

ws_check_workspace
OWNER=$(ws_owner_login)
ws_prepare_sparse "$PREFIX"

ws_register "$NAME" "$PREFIX" "$REPO" main

ws_write_new "$PREFIX/README.md" "$(printf '# %s\n\nPackage `%s` in `//%s`.' "$NAME" "$MOD_PATH" "$PREFIX")"
ws_write_new "$PREFIX/OWNERS" "@$OWNER"
ws_write_new "$PREFIX/go.mod" "$(printf 'module %s\n\ngo 1.27.3' "$MOD_PATH")"

if ws_commit "$PREFIX" "feat($PREFIX): initialize project //$PREFIX ($REPO)"; then
  # The new module joins the root go.work in its own commit (the root go.work is never exported).
  [ ! -f go.work ] || "$SCRIPT_DIR/go-work-add.sh" "$PREFIX"
  ws_next_steps "$REPO" "$PREFIX"
fi
