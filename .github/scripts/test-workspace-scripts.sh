#!/usr/bin/env bash
# Smoke test for import-repo.sh, create-repo.sh and go-work-add.sh against a throwaway trunk-like
# repository and local "upstream" repositories. No network, no GitHub.
#
# Usage: .github/scripts/test-workspace-scripts.sh
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null
export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@example.org GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@example.org
export SUDA_OWNER=tester

fail() {
  echo "FAIL: $*" >&2
  exit 1
}
check() { # check <description> <command...>
  local d="$1"
  shift
  "$@" >/dev/null 2>&1 || fail "$d"
}
check_not() {
  local d="$1"
  shift
  if "$@" >/dev/null 2>&1; then fail "$d"; fi
}

# A trunk-like repository: one root commit holding the files the helpers edit.
WS="$TMP/ws"
mkdir -p "$WS/docs" "$WS/web/app"
git -C "$WS" init -q -b main
cat >"$WS/capy.star" <<'EOF'
PILOT_REPOS = [
    ("app", "web/app", "gosuda/app", False),
]
EOF
printf '# Explorer\n<!-- SUDA_EXPLORER_START -->\n| a |\n<!-- SUDA_EXPLORER_END -->\n' >"$WS/docs/README.md"
printf 'go 1.24.0\n\nuse (\n\t./web/app\n)\n\nreplace example.org/unpublished v0.0.1 => ./web/app\n' >"$WS/go.work"
printf 'module example.org/app\n\ngo 1.24.0\n' >"$WS/web/app/go.mod"
git -C "$WS" add -A
git -C "$WS" commit -q -m trunk
git -C "$WS" switch -q -c ws/tester/x

# Upstream repositories (single commit on main).
mkup() { # mkup <dir> <module> <go-version> [replace-line]
  mkdir -p "$1"
  git -C "$1" init -q -b main
  printf 'module %s\n\ngo %s\n%s\n' "$2" "$3" "${4:-}" >"$1/go.mod"
  printf 'package x\n' >"$1/x.go"
  git -C "$1" add -A
  git -C "$1" commit -q -m init
}
mkup "$TMP/up-lib" example.org/lib 1.24.0
mkup "$TMP/up-private" example.org/private 1.24.0
mkup "$TMP/up-bad" example.org/bad 1.24.0 'replace example.org/lib => ../lib'

cd "$WS"

# 1. Public import: capy.star entry and explorer row, unmodified tree, no go.work change.
SUDA_IMPORT_URL="$TMP/up-lib" "$SCRIPT_DIR/import-repo.sh" crypto/lib gosuda/lib >/dev/null
check "capy.star lists crypto/lib" grep -q '"crypto/lib", "gosuda/lib"' capy.star
check "explorer row for crypto/lib" grep -q '| `//crypto/lib` |' docs/README.md
check "the import commit leaves go.work alone" test -z "$(git diff --name-only HEAD~1 HEAD -- go.work)"
check "imported file is byte-identical" cmp "$TMP/up-lib/go.mod" crypto/lib/go.mod

# 2. corp import: files only; never a capy.star entry or an explorer row.
SUDA_IMPORT_URL="$TMP/up-private" "$SCRIPT_DIR/import-repo.sh" corp/private gosuda/private >/dev/null
check_not "corp prefix must not be registered in capy.star" grep -q 'corp/private' capy.star
check_not "corp prefix must not get an explorer row" grep -q 'corp/private' docs/README.md
check "corp tree imported" test -f corp/private/go.mod

# 3. go-work-add.sh: adds the module, keeps replace directives and sorted use entries.
"$SCRIPT_DIR/go-work-add.sh" crypto/lib corp/private >/dev/null
check "go.work uses crypto/lib" grep -qx $'\t./crypto/lib' go.work
check "go.work uses corp/private" grep -qx $'\t./corp/private' go.work
check "go.work keeps its replace directive" grep -q '^replace example.org/unpublished v0.0.1 => ./web/app$' go.work
sorted=$(sed -n '/^use (/,/^)/p' go.work | sed '1d;$d' | sort)
check "go.work use block is sorted" test "$sorted" = "$(sed -n '/^use (/,/^)/p' go.work | sed '1d;$d')"
check "go.work additions are one commit" test "$(git log -1 --format=%s)" != ""
before=$(git rev-parse HEAD)
"$SCRIPT_DIR/go-work-add.sh" crypto/lib >/dev/null
check "go-work-add.sh is idempotent" test "$(git rev-parse HEAD)" = "$before"

# 4. A newer go directive raises go.work's.
mkup "$TMP/up-new" example.org/new 1.26.1
SUDA_IMPORT_URL="$TMP/up-new" "$SCRIPT_DIR/import-repo.sh" net/new gosuda/new >/dev/null
"$SCRIPT_DIR/go-work-add.sh" net/new >/dev/null
check "go.work go directive is raised" grep -qx 'go 1.26.1' go.work

# 5. go-work-add.sh refuses a module with a local replace.
SUDA_IMPORT_URL="$TMP/up-bad" "$SCRIPT_DIR/import-repo.sh" net/bad gosuda/bad >/dev/null
before=$(git rev-parse HEAD)
check_not "a module with a local replace must be refused" "$SCRIPT_DIR/go-work-add.sh" net/bad
check "refusal leaves HEAD alone" test "$(git rev-parse HEAD)" = "$before"
check_not "refusal leaves go.work alone" grep -q 'net/bad' go.work

# 6. create-repo.sh: project files in one commit, go.work in the next; corp is never registered.
"$SCRIPT_DIR/create-repo.sh" storage/fresh gosuda/fresh >/dev/null
check "create-repo registers a public project" grep -q '"storage/fresh", "gosuda/fresh"' capy.star
check "create-repo adds the module to go.work" grep -qx $'\t./storage/fresh' go.work
"$SCRIPT_DIR/create-repo.sh" corp/secret gosuda/secret >/dev/null
check_not "create-repo must not register a corp project" grep -q 'corp/secret' capy.star
check "create-repo still adds the corp module to go.work" grep -qx $'\t./corp/secret' go.work

echo "ok"
