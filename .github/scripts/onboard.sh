#!/usr/bin/env bash
# One-time (and refresh) setup of a gosuda monorepo clone.
#
#   gh api -H 'Accept: application/vnd.github.raw' repos/gosuda/0/contents/.github/scripts/onboard.sh \
#     | bash -s -- [-d <dir>] [-p ssh|https] [<domain>/<project> ...]
#
# The script is served from gosuda/0 on purpose: only the lander and org owners can write there.
# Never fetch it from gosuda/1, which every member can write.
#
# Fresh run  : clones gosuda/0 read-only (blob-less, sparse), registers the aliases, adds the `ws`
#              remote (gosuda/1), disables pushes to `origin`, and selects the sparse directories.
#              Uses SSH when GitHub accepts your key, otherwise HTTPS with the GitHub CLI's login (`-p` forces one).
# Re-run     : in an existing clone that is on a clean `main`, refreshes the aliases from the current
#              control plane. Workspace branches are never touched.
set -euo pipefail

DIR="gosuda-0"
PROTO=""
REPO="${SUDA_REMOTE_URL:-}"   # override for tests: full clone URL of gosuda/0
SPARSE=()

die() { echo "onboard: $*" >&2; exit 1; }

while [ $# -gt 0 ]; do
  case "$1" in
    -d|--dir) DIR="${2:?missing value for $1}"; shift 2 ;;
    -p|--protocol) PROTO="${2:?missing value for $1}"; shift 2 ;;
    -h|--help)
      echo "usage: onboard.sh [-d <dir>] [-p ssh|https] [<domain>/<project> ...]"
      exit 0 ;;
    -*) die "unknown option $1" ;;
    *) SPARSE+=("$1"); shift ;;
  esac
done

# Without -p, use SSH when GitHub accepts this machine's SSH key, otherwise HTTPS through the
# GitHub CLI's credentials.
if [ -z "$PROTO" ] && [ -z "$REPO" ]; then
  if ssh -o BatchMode=yes -o ConnectTimeout=8 -T git@github.com 2>&1 | grep -q "successfully authenticated"; then
    PROTO=ssh
  else
    PROTO=https
  fi
fi
case "${PROTO:-ssh}" in
  ssh) URL_DEFAULT="git@github.com:gosuda/0.git" ;;
  https) URL_DEFAULT="https://github.com/gosuda/0.git" ;;
  *) die "protocol must be ssh or https" ;;
esac
REPO="${REPO:-$URL_DEFAULT}"
USE_GH_CREDENTIALS=0
if [ "${PROTO:-}" = "https" ] && command -v gh >/dev/null && gh auth status >/dev/null 2>&1; then
  USE_GH_CREDENTIALS=1
fi
clone_cfg=()
[ "$USE_GH_CREDENTIALS" = 1 ] && clone_cfg=(-c "credential.helper=!gh auth git-credential")

for p in "${SPARSE[@]+"${SPARSE[@]}"}"; do
  case "$p" in
    /*|*..*|"") die "invalid directory '$p'" ;;
    web/*|net/*|ipc/*|storage/*|crypto/*|ai/*|devtools/*|corp/*|third_party/*|experimental/*|web|net|ipc|storage|crypto|ai|devtools|corp|third_party|experimental) ;;
    *) die "'$p' is not under a known domain (web net ipc storage crypto ai devtools corp third_party experimental)" ;;
  esac
done

command -v git >/dev/null || die "git is required"
gv=$(git --version | sed -E 's/^git version ([0-9]+)\.([0-9]+).*/\1 \2/')
read -r gmaj gmin <<<"$gv"
if [ "${gmaj:-0}" -lt 2 ] || { [ "$gmaj" -eq 2 ] && [ "${gmin:-0}" -lt 30 ]; }; then
  die "git 2.30 or newer is required (found $(git --version))"
fi

if [ -d "$DIR/.git" ] || [ -f "$DIR/.git" ]; then
  # ---- refresh an existing clone ----
  cd "$DIR"
  if [ -z "${SUDA_REMOTE_URL:-}" ]; then
    case "$(git config --get remote.origin.url || true)" in
      *gosuda/0|*gosuda/0.git) ;;
      *) die "$DIR is a git repository, but its origin is not gosuda/0" ;;
    esac
  fi
  branch=$(git symbolic-ref --short -q HEAD || true)
  [ "$branch" = "main" ] || die "refresh needs the clone on 'main' (it is on '${branch:-a detached HEAD}'); run: git switch main"
  [ -z "$(git status --porcelain)" ] || die "refresh needs a clean working tree; commit or stash first"
  git fetch -q --filter=blob:none origin refs/heads/main
  git reset -q --hard FETCH_HEAD
  echo "onboard: refreshed the control plane in $DIR"
else
  [ ! -e "$DIR" ] || die "$DIR exists and is not a git clone"
  git clone -q "${clone_cfg[@]+"${clone_cfg[@]}"}" --filter=blob:none --sparse "$REPO" "$DIR"
  cd "$DIR"
  echo "onboard: cloned gosuda/0 (read-only) into $DIR"
fi

git config include.path ../.gitconfig
[ "$USE_GH_CREDENTIALS" = 1 ] && git config credential.helper '!gh auth git-credential'
git suda-init
if [ "${#SPARSE[@]}" -gt 0 ]; then
  git sparse-checkout set .agents .github "${SPARSE[@]}"
fi

if ! command -v gh >/dev/null; then
  echo "onboard: note: install the GitHub CLI (gh) to use 'git ws-pr' and to read landing statuses" >&2
fi

cat <<EOF

Done. This clone is read-only for gosuda/0; all pushes go to the 'ws' remote (gosuda/1).

Next:
  cd $DIR
  git sparse-checkout add <domain>/<project>      # directories your task needs
  git ws-new <your-github-login>/<task>           # start a workspace
  ... edit, commit ...
  git ws-save && git ws-sync && git ws-land       # save, rebase, submit

Guide: https://github.com/gosuda/1/blob/main/docs/onboarding.md
EOF
