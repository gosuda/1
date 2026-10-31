# shellcheck shell=bash
# Shared helpers for create-repo.sh and import-repo.sh. Sourced, not executed.
#
# Both helpers run inside a workspace branch created by `git ws-new`, build exactly one local
# commit on top of HEAD, and print the next steps. They never push, never touch refs/suda/*,
# and never create GitHub repositories. The lander lands the commit after `git ws-save`/`git ws-land`.

WS_DOMAINS="web net ipc storage crypto ai devtools corp third_party experimental"

ws_die() {
  echo "error: $*" >&2
  exit 1
}

ws_usage() {
  echo "error: $*" >&2
  exit 2
}

# ws_validate_prefix <prefix>: <domain>/<name>[/<sub>...] under a known google3 domain.
ws_validate_prefix() {
  local prefix="$1" first re='^[A-Za-z0-9._/-]+$'
  case "$prefix" in
    /*) ws_usage "prefix must not start with '/': $prefix" ;;
    */) ws_usage "prefix must not end with '/': $prefix" ;;
  esac
  [[ $prefix =~ $re ]] || ws_usage "prefix may contain only letters, digits, '.', '_', '-' and '/': $prefix"
  case "/$prefix/" in
    */./* | */../* | *//*) ws_usage "prefix must not contain '.', '..', or empty segments: $prefix" ;;
  esac
  case "$prefix" in
    */*) ;;
    *) ws_usage "prefix must be <domain>/<name>: $prefix" ;;
  esac
  first="${prefix%%/*}"
  case " $WS_DOMAINS " in
    *" $first "*) ;;
    *) ws_usage "prefix must start with one of: $WS_DOMAINS (got: $first)" ;;
  esac
}

# ws_validate_repo <owner/repo>
ws_validate_repo() {
  local re='^[A-Za-z0-9_][A-Za-z0-9_.-]*/[A-Za-z0-9_][A-Za-z0-9_.-]*$'
  [[ $1 =~ $re ]] || ws_usage "repository must be <owner>/<repo>: $1"
}

# ws_validate_branch <branch>
ws_validate_branch() {
  local re='^[A-Za-z0-9._/-]+$'
  if ! { [[ $1 =~ $re ]] && git check-ref-format --branch "$1" >/dev/null 2>&1; }; then
    ws_usage "invalid branch name: $1"
  fi
}

# ws_validate_module <go-module-path>
ws_validate_module() {
  local re='^[A-Za-z0-9._~/-]+$'
  [[ $1 =~ $re ]] || ws_usage "invalid Go module path: $1"
}

# ws_check_workspace: sets REPO_ROOT, moves to it, and refuses unsafe states.
ws_check_workspace() {
  REPO_ROOT=$(git rev-parse --show-toplevel 2>/dev/null) || ws_die "not inside a git worktree"
  cd "$REPO_ROOT"
  git symbolic-ref -q HEAD >/dev/null || ws_die "HEAD is detached; run from a workspace branch created with 'git ws-new'"
  git rev-parse --verify -q HEAD >/dev/null || ws_die "HEAD has no commits; run from a workspace branch created with 'git ws-new'"
  local roots
  roots=$(git rev-list --max-parents=0 HEAD | wc -l | tr -d ' ')
  [ "$roots" = 1 ] || ws_die "HEAD must have exactly one root commit (the trunk snapshot); found $roots"
  if [ -n "$(git status --porcelain)" ]; then
    ws_die "working tree has uncommitted changes; commit or stash them first"
  fi
}

# ws_prepare_sparse <prefix>: makes the prefix and docs/ visible when sparse checkout is on.
ws_prepare_sparse() {
  if [ "$(git config --bool core.sparseCheckout 2>/dev/null || true)" = true ]; then
    git sparse-checkout add "$1" docs
  fi
}

# ws_owner_login: prints the GitHub login used for OWNERS ($SUDA_OWNER, else gh api user).
ws_owner_login() {
  local login="${SUDA_OWNER:-}" re='^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?$'
  if [ -z "$login" ] && command -v gh >/dev/null 2>&1; then
    login=$(gh api user -q .login 2>/dev/null || true)
  fi
  login="${login#@}"
  [ -n "$login" ] || ws_die "cannot determine the OWNERS login: set SUDA_OWNER=<github-login> or run 'gh auth login'"
  [[ $login =~ $re ]] || ws_die "invalid GitHub login: $login"
  printf '%s\n' "$login"
}

# ws_write_new <path> <content>: creates a file and stages it. Tracked files are kept, never overwritten.
ws_write_new() {
  local path="$1" content="$2"
  if [ -n "$(git ls-files -- "$path")" ]; then
    echo "kept existing $path"
    return 0
  fi
  [ ! -e "$path" ] || ws_die "$path exists but is not tracked; remove it first"
  mkdir -p -- "$(dirname -- "$path")"
  printf '%s\n' "$content" >"$path"
  git add -- "$path"
}

# ws_register <name> <prefix> <owner/repo> <branch>: adds the capy.star PILOT_REPOS entry and the
# docs/README.md explorer row. Idempotent: existing entries are left alone and files are written only
# when their content changes. Nothing is written unless every edit validates.
ws_register() {
  case "${2%%/*}" in
    corp | experimental)
      # corp is never exported and experimental is a sandbox: no mirror workflow, no explorer row.
      echo "capy.star, docs/README.md: //$2 is never exported; not registered"
      return 0
      ;;
  esac
  python3 - "$REPO_ROOT/capy.star" "$REPO_ROOT/docs/README.md" "$1" "$2" "$3" "$4" <<'PY'
import re
import sys

capy_path, readme_path, name, prefix, repo, branch = sys.argv[1:7]


def fail(msg):
    sys.exit(f"error: {msg}")


def read(path):
    with open(path, encoding="utf-8") as f:
        return f.read()


capy = read(capy_path)
start = capy.find("PILOT_REPOS = [\n")
if start < 0:
    fail("capy.star: PILOT_REPOS list not found")
end = capy.find("\n]\n", start)
if end < 0:
    fail("capy.star: end of PILOT_REPOS list not found")
body = capy[start:end]
capy_new = capy
if re.search(r'\(\s*"[^"]*",\s*"' + re.escape(prefix) + r'"\s*,', body):
    print(f"capy.star: //{prefix} already registered")
else:
    if re.search(r'\(\s*"' + re.escape(name) + r'"\s*,', body):
        fail(f"capy.star: workflow name {name!r} is already used by another prefix")
    if branch == "main":
        entry = f'    ("{name}", "{prefix}", "{repo}", False),\n'
    else:
        entry = f'    ("{name}", "{prefix}", "{repo}", False, "{branch}"),\n'
    capy_new = capy[: end + 1] + entry + capy[end + 1 :]
    print(f"capy.star: registered {name} -> {repo}")

readme = read(readme_path)
s = readme.find("<!-- SUDA_EXPLORER_START -->")
e = readme.find("<!-- SUDA_EXPLORER_END -->")
if s < 0 or e < s:
    fail("docs/README.md: SUDA_EXPLORER_START/END markers not found")
if readme[e - 1 : e] != "\n":
    fail("docs/README.md: SUDA_EXPLORER_END must start its own line")
readme_new = readme
if f"| `//{prefix}` |" in readme[s:e]:
    print(f"docs/README.md: //{prefix} row already present")
else:
    row = (
        f"| `//{prefix}` | `{repo}` | [Browse `//{prefix}`](https://magpie.gosuda.org/tree/{prefix}) "
        f"| [Commits `//{prefix}`](https://magpie.gosuda.org/history/{prefix}) |\n"
    )
    readme_new = readme[:e] + row + readme[e:]
    print(f"docs/README.md: added //{prefix} row")

if capy_new != capy:
    with open(capy_path, "w", encoding="utf-8") as f:
        f.write(capy_new)
if readme_new != readme:
    with open(readme_path, "w", encoding="utf-8") as f:
        f.write(readme_new)
PY
}

# ws_commit <prefix> <message>: stages <prefix>/, capy.star, and docs/README.md, then commits once.
# Returns 1 without committing when nothing changed.
ws_commit() {
  local prefix="$1" msg="$2"
  git add -A -- "$prefix" capy.star docs/README.md
  if git diff --cached --quiet; then
    echo "no changes: //$prefix is already in this workspace; nothing to commit"
    return 1
  fi
  git commit -q -m "$msg"
  echo "committed $(git rev-parse --short HEAD) on $(git symbolic-ref --short HEAD): $(git log -1 --format=%s)"
}

# ws_next_steps <owner/repo>: prints the submission steps and the mirror prerequisite.
ws_next_steps() {
  cat <<EOF

Next steps:
  git ws-save
  git ws-land

The mirror repository $1 must exist and be writable by the suda-lander App before the first export.
If it does not exist yet, an org owner creates it with:
  gh repo create $1 --public
Until it exists, the lander reports \`destination missing\` for this workflow.
EOF
}
