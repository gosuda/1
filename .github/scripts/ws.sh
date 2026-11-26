#!/usr/bin/env bash
# ws.sh: client-side workspace commands for the gosuda/0 monorepo (the ws-* and suda-* aliases in .gitconfig).
#
# Run from anywhere inside a worktree prepared by `git suda-init`. Trunk and landed history are read from
# `origin` (gosuda/0, read-only); everything is written to `ws` (gosuda/1). Commands that submit a change
# first give its tip a Change-Id, which stays the same across amend, rebase and branch rename. `gh` is
# optional: it only opens or finds the review pull request.
#
#   change-id                     ensure HEAD has a Change-Id; print it
#   save                          ensure a Change-Id; push HEAD to refs/ws/<actor>/<id>
#   land                          ensure a Change-Id; push HEAD to refs/heads/land/<id>
#   pr                            ensure a Change-Id; push base/review refs and refs/ws/cl/<change>/<sha>; open the CL
#   ps                            list the patchsets of the current change, oldest first
#   psdiff <a> <b>                range-diff between two patchsets (PS<n>, <n>, or a sha prefix)
#   autosave once|start|stop|status
#                                 private WIP backup to refs/ws/autosave/<actor>/<id>; leaves index, HEAD, branch alone
#   resume [--force] [<actor>/<id>]
#                                 restore an autosaved working state as uncommitted changes
#   cl <number|I<40 hex>>         find a landed change on refs/suda/history
#   wt <actor>/<id> [<dir>]       extra workspace: a git worktree on origin/suda/main
#   init                          install the tools into the common git dir and configure the remotes (suda-init)
#   fetch <git fetch args>        git fetch, serialized with every other fetch of this clone
#
# `git suda-init` installs this script as <common git dir>/suda-ws.sh next to suda.gitconfig, and the aliases run
# that copy, so every branch and every worktree of the clone uses the same version, including branches whose tree
# predates this file. All worktrees of a clone share their remote-tracking refs, and two concurrent fetches fail
# with "cannot lock ref"; every fetch therefore runs under one lock in the common git dir.
#
# Usage errors exit 2 with one line; runtime errors exit 1.
set -euo pipefail

SELF=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/$(basename "${BASH_SOURCE[0]}")
WS_TMP=""
FETCH_LOCK=""
cleanup() {
  if [ -n "$WS_TMP" ]; then
    rm -rf "$WS_TMP"
  fi
  if [ -n "$FETCH_LOCK" ]; then
    rm -rf "$FETCH_LOCK"
  fi
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

die() {
  echo "error: $*" >&2
  exit 1
}

usage_error() {
  echo "error: $*" >&2
  exit 2
}

log() {
  printf '%s ws.sh: %s\n' "$(date '+%Y-%m-%dT%H:%M:%S')" "$*" >&2
}

usage() {
  cat <<'EOF'
usage: ws.sh <subcommand> [args]
  change-id                     ensure HEAD has a Change-Id and print it
  save | land                   ensure a Change-Id, then push HEAD to refs/ws/<actor>/<id> or refs/heads/land/<id>
  pr                            ensure a Change-Id, push the review pair and refs/ws/cl/<change>/<sha>, open the CL
  ps                            list the patchsets of the current change, oldest first
  psdiff <a> <b>                range-diff of two patchsets: PS<n>, <n>, or a sha prefix
  autosave once|start|stop|status
                                private WIP backup to refs/ws/autosave/<actor>/<id> on ws
  resume [--force] [<actor>/<id>]
                                restore an autosaved working state as uncommitted changes
  cl <number|I<40 hex>>         find a landed change on refs/suda/history
  wt <actor>/<id> [<dir>]       extra workspace: git worktree on origin/suda/main
  init                          install the tools into the common git dir, configure the remotes, fetch
  fetch <git fetch args>        git fetch under the clone-wide fetch lock
EOF
}

# need_worktree: moves to the toplevel of the current worktree.
need_worktree() {
  local top
  top=$(git rev-parse --show-toplevel 2>/dev/null) || die "not inside a git worktree"
  cd "$top"
}

# need_branch: sets BRANCH to the checked-out branch.
need_branch() {
  BRANCH=$(git symbolic-ref -q --short HEAD) ||
    die "HEAD is detached; check out a workspace branch created with 'git ws-new'"
}

# parse_workspace <actor>/<id>: sets ACTOR and ID; returns 1 unless the name is a valid workspace name.
parse_workspace() {
  local re='^([A-Za-z0-9._-]+)/([A-Za-z0-9._-]+)$'
  [[ $1 =~ $re ]] || return 1
  git check-ref-format "refs/heads/ws/$1" >/dev/null 2>&1 || return 1
  ACTOR=${BASH_REMATCH[1]}
  ID=${BASH_REMATCH[2]}
}

# git_dir_file <name>: the path of a file kept in this worktree's git directory.
git_dir_file() {
  printf '%s/%s\n' "$(git rev-parse --absolute-git-dir)" "$1"
}

# common_dir: the absolute git directory shared by every worktree of this clone.
common_dir() {
  (cd "$(git rev-parse --git-common-dir)" && pwd)
}

# with_fetch_lock <command...>: runs the command while holding the clone-wide fetch lock, a directory in the
# common git dir that holds the owner's pid. A lock whose owner is gone is broken by one waiter at a time (the
# .break directory), so two waiters never remove a lock that a third process has just taken.
with_fetch_lock() {
  local lock pid start timeout rc=0 told=0
  lock="$(common_dir)/suda-fetch.lock"
  timeout=${SUDA_FETCH_LOCK_TIMEOUT:-600}
  start=$SECONDS
  while ! mkdir "$lock" 2>/dev/null; do
    pid=$(cat "$lock/pid" 2>/dev/null || true)
    if [ -n "$pid" ] && ! kill -0 "$pid" 2>/dev/null && mkdir "$lock.break" 2>/dev/null; then
      if [ "$(cat "$lock/pid" 2>/dev/null || true)" = "$pid" ]; then
        rm -rf "$lock"
        log "removed a fetch lock left behind by pid $pid"
      fi
      rmdir "$lock.break"
      continue
    fi
    if [ $((SECONDS - start)) -ge "$timeout" ]; then
      die "another fetch of this clone (pid ${pid:-unknown}) has held $lock for ${timeout}s; remove the directory if no git fetch is running"
    fi
    if [ "$told" -eq 0 ]; then
      log "waiting for another fetch of this clone to finish"
      told=1
    fi
    sleep 0.2
  done
  FETCH_LOCK=$lock
  printf '%s\n' "$$" >"$lock/pid"
  "$@" || rc=$?
  rm -rf "$lock"
  FETCH_LOCK=""
  return "$rc"
}

# is_embargo_name <name>: true when a path segment is `sec` or starts with `sec-` (docs/security-embargo.md).
is_embargo_name() {
  local seg rest="$1/"
  while [ -n "$rest" ]; do
    seg=${rest%%/*}
    rest=${rest#*/}
    case "$seg" in
      sec | sec-*) return 0 ;;
    esac
  done
  return 1
}

# patchset_ns <change>: the ref namespace of a change's patchsets. Embargoed work (a branch with a `sec` or
# `sec-*` segment, or a tip subject starting with `sec-`) keeps them under refs/ws/cl/sec/: magpie never lists
# such references and the lander keeps them for the embargo TTL instead of the workspace TTL.
patchset_ns() {
  local branch subject
  branch=$(git symbolic-ref -q --short HEAD || true)
  subject=$(git log -1 --format=%s HEAD)
  if is_embargo_name "$branch" || [[ $subject == sec-* ]]; then
    printf 'refs/ws/cl/sec/%s\n' "$1"
  else
    printf 'refs/ws/cl/%s\n' "$1"
  fi
}

# single_root <rev>: prints the one parentless commit (the trunk snapshot) that <rev>'s history starts from.
single_root() {
  local roots
  git rev-parse --verify -q "$1^{commit}" >/dev/null || die "$1 has no commits"
  roots=$(git rev-list --max-parents=0 "$1")
  case "$roots" in
    "") die "$1 has no root commit" ;;
    *$'\n'*) die "$1 must have exactly one root commit (the trunk snapshot); found several" ;;
  esac
  printf '%s\n' "$roots"
}

# trailer_change_id <rev>: prints the Change-Id trailer of <rev>'s message, if it has one.
trailer_change_id() {
  git cat-file commit "$1" | sed '1,/^$/d' | git interpret-trailers --parse |
    sed -n 's/^Change-Id: \(I[0-9a-f]\{40\}\)$/\1/p' | tail -n 1
}

# find_change_id <root>: prints the newest Change-Id among the commits in <root>..HEAD, if any.
find_change_id() {
  local c id
  while IFS= read -r c; do
    id=$(trailer_change_id "$c")
    if [ -n "$id" ]; then
      printf '%s\n' "$id"
      return 0
    fi
  done < <(git rev-list "$1..HEAD")
  return 0
}

# new_change_id: a fresh I<40 hex> derived from the committer identity, the time, random bytes and HEAD.
new_change_id() {
  local seed
  seed=$({
    git var GIT_COMMITTER_IDENT
    date +%s
    head -c 32 /dev/urandom | od -An -tx1
    git rev-parse HEAD
  } | git hash-object --stdin)
  printf 'I%s\n' "$seed"
}

# ensure_change_id: sets CHANGE_ID to HEAD's change. A tip without a Change-Id gets one by a message-only
# amend (reusing the newest Change-Id below it, else a new one). The trunk snapshot stays unidentified.
ensure_change_id() {
  local root tip_id
  CHANGE_ID=""
  root=$(single_root HEAD)
  if [ "$(git rev-parse HEAD)" = "$root" ]; then
    return 0
  fi
  tip_id=$(trailer_change_id HEAD)
  if [ -n "$tip_id" ]; then
    CHANGE_ID=$tip_id
    return 0
  fi
  git diff --cached --quiet || die "the tip has no Change-Id and the index has staged changes; commit or unstage them first"
  CHANGE_ID=$(find_change_id "$root")
  if [ -z "$CHANGE_ID" ]; then
    CHANGE_ID=$(new_change_id)
  fi
  git cat-file commit HEAD | sed '1,/^$/d' |
    git interpret-trailers --trailer "Change-Id: $CHANGE_ID" |
    git commit --amend --only --no-verify --cleanup=verbatim -q -F -
}

cmd_change_id() {
  [ $# -eq 0 ] || usage_error "usage: ws.sh change-id"
  ensure_change_id
  [ -n "$CHANGE_ID" ] || die "HEAD is the trunk snapshot; there is no change to identify"
  printf '%s\n' "$CHANGE_ID"
}

# push_ws <refspec>...: pushes to ws with an explicit lease on each destination: the push is refused if that
# ref moved on ws since it was read here. Needed for refs outside the fetch mapping (refs/heads/*), which git
# cannot lease by tracking ref; a ref that does not exist must still be absent (--force-with-lease=<ref>:).
push_ws() {
  local spec dst cur
  local -a leases=()
  for spec in "$@"; do
    dst=${spec#*:}
    cur=$(git ls-remote ws "$dst" | awk -v ref="$dst" '$2 == ref { print $1 }')
    leases+=("--force-with-lease=$dst:$cur")
  done
  git push "${leases[@]}" ws "$@"
}

cmd_save() {
  [ $# -eq 0 ] || usage_error "usage: ws.sh save"
  need_branch
  ensure_change_id
  git push --force-with-lease ws "HEAD:refs/$BRANCH"
}

cmd_land() {
  [ $# -eq 0 ] || usage_error "usage: ws.sh land"
  need_branch
  ensure_change_id
  push_ws "HEAD:refs/heads/land/$(basename "$BRANCH")"
}

# review_compare_url <id>: the compare page of the review pair, for when no pull request can be opened.
review_compare_url() {
  printf 'https://github.com/gosuda/1/compare/base/%s...review/%s\n' "$1" "$1"
}

# open_review_pr <id>: prints "<number> <url>" of the open pull request from review/<id>, if there is one.
open_review_pr() {
  gh pr list --repo gosuda/1 --head "review/$1" --state open --json number,url \
    -q '.[0] // empty | "\(.number) \(.url)"' 2>/dev/null || true
}

cmd_pr() {
  local id base subject body sha existing url
  [ $# -eq 0 ] || usage_error "usage: ws.sh pr"
  need_branch
  ensure_change_id
  [ -n "$CHANGE_ID" ] || die "HEAD is the trunk snapshot; commit the change before 'git ws-pr'"
  id=$(basename "$BRANCH")
  base=$(single_root HEAD)
  sha=$(git rev-parse HEAD)
  subject=$(git log -1 --format=%s HEAD)
  body=$(git log -1 --format=%b HEAD)
  push_ws "HEAD:refs/$BRANCH" "$base:refs/heads/base/$id" "HEAD:refs/heads/review/$id" \
    "HEAD:$(patchset_ns "$CHANGE_ID")/$sha"
  if ! command -v gh >/dev/null 2>&1; then
    review_compare_url "$id"
    return 0
  fi
  existing=$(open_review_pr "$id")
  if [ -n "$existing" ]; then
    printf 'CL %s: %s (new patchset %s)\n' "${existing%% *}" "${existing#* }" "$(git rev-parse --short=7 HEAD)"
    return 0
  fi
  if url=$(gh pr create --repo gosuda/1 --base "base/$id" --head "review/$id" --title "$subject" --body "$body"); then
    url=$(printf '%s\n' "$url" | tail -n 1)
    printf 'CL %s: %s\n' "${url##*/}" "$url"
  else
    review_compare_url "$id"
  fi
}

# fetch_patchsets <namespace>: fetches the immutable patchset refs under refs/ws/cl/[sec/]<change> from ws, blobless.
fetch_patchsets() {
  with_fetch_lock git fetch --quiet --no-tags --filter=blob:none ws "+$1/*:refs/remotes/ws/ws/${1#refs/ws/}/*" ||
    die "cannot fetch the patchsets $1/* from ws"
}

# patchsets <namespace>: prints "<n> <sha> <short-sha> <iso-date> <subject>" per patchset, oldest first.
patchsets() {
  local n=0 sha short date subject
  while IFS=' ' read -r sha short date subject; do
    n=$((n + 1))
    printf '%s %s %s %s %s\n' "$n" "$sha" "$short" "$date" "$subject"
  done < <(git for-each-ref --sort=committerdate \
    --format='%(objectname) %(objectname:short=7) %(committerdate:iso-strict) %(contents:subject)' \
    "refs/remotes/ws/ws/${1#refs/ws/}")
}

# current_change: prints the change of HEAD (the newest Change-Id in <root>..HEAD); dies without one.
current_change() {
  local root id
  root=$(single_root HEAD)
  id=$(find_change_id "$root")
  [ -n "$id" ] || die "HEAD has no Change-Id; run 'git ws-id' or 'git ws-pr' first"
  printf '%s\n' "$id"
}

cmd_ps() {
  local change ns list n sha short date subject mark here
  [ $# -eq 0 ] || usage_error "usage: ws.sh ps"
  change=$(current_change)
  ns=$(patchset_ns "$change")
  fetch_patchsets "$ns"
  list=$(patchsets "$ns")
  if [ -z "$list" ]; then
    echo "no patchsets for change $change on ws; publish one with 'git ws-pr'" >&2
    return 0
  fi
  here=$(git rev-parse HEAD)
  while IFS=' ' read -r n sha short date subject; do
    mark=" "
    if [ "$sha" = "$here" ]; then
      mark="*"
    fi
    printf '%s PS%s %s %s %s\n' "$mark" "$n" "$short" "$date" "$subject"
  done <<<"$list"
}

# resolve_patchset <patchsets> <name>: prints the full sha named by PS<n>, a bare <n>, or a sha prefix.
resolve_patchset() {
  local list="$1" name="$2" n sha short date subject num hits=0 match=""
  if [[ $name =~ ^PS([0-9]+)$ || $name =~ ^([0-9]+)$ ]]; then
    num=$((10#${BASH_REMATCH[1]}))
    while IFS=' ' read -r n sha short date subject; do
      if [ "$n" = "$num" ]; then
        printf '%s\n' "$sha"
        return 0
      fi
    done <<<"$list"
    if [[ $name == PS* ]] || ! [[ $name =~ ^[0-9a-f]{4,40}$ ]]; then
      die "no patchset $name for this change"
    fi
  elif ! [[ $name =~ ^[0-9a-f]{4,40}$ ]]; then
    usage_error "not a patchset of this change: $name (use PS<n>, <n>, or a sha prefix)"
  fi
  while IFS=' ' read -r n sha short date subject; do
    case "$sha" in
      "$name"*)
        hits=$((hits + 1))
        match=$sha
        ;;
    esac
  done <<<"$list"
  [ "$hits" -eq 1 ] || die "sha prefix $name matches $hits patchsets of this change"
  printf '%s\n' "$match"
}

cmd_psdiff() {
  local change ns list a b root_a root_b
  [ $# -eq 2 ] || usage_error "usage: ws.sh psdiff <a> <b>"
  change=$(current_change)
  ns=$(patchset_ns "$change")
  fetch_patchsets "$ns"
  list=$(patchsets "$ns")
  [ -n "$list" ] || die "no patchsets for change $change on ws"
  a=$(resolve_patchset "$list" "$1")
  b=$(resolve_patchset "$list" "$2")
  root_a=$(single_root "$a")
  root_b=$(single_root "$b")
  git range-diff "$root_a..$a" "$root_b..$b"
}

# autosave_target: sets BRANCH, ACTOR, ID and AUTOSAVE_REF for the checked-out ws/<actor>/<id> branch.
autosave_target() {
  need_branch
  if [ "${BRANCH%%/*}" != ws ] || ! parse_workspace "${BRANCH#ws/}"; then
    die "autosave needs a workspace branch ws/<actor>/<id>; the current branch is $BRANCH"
  fi
  AUTOSAVE_REF="refs/ws/autosave/$ACTOR/$ID"
}

# autosave_once: records the working state (tracked changes, deletions, untracked files that are neither
# ignored nor oversized) as a commit on top of HEAD and pushes it to AUTOSAVE_REF. It works on a copy of
# the index, so the real index, HEAD and branch are never touched.
autosave_once() {
  local real max tree head_tree last last_file last_tree last_parent commit remote expected size f list
  [ $# -eq 0 ] || usage_error "usage: ws.sh autosave once"
  autosave_target
  max=${SUDA_AUTOSAVE_MAX_BYTES:-5242880}
  case "$max" in
    '' | *[!0-9]*) usage_error "SUDA_AUTOSAVE_MAX_BYTES must be a whole number of bytes" ;;
  esac
  WS_TMP=$(mktemp -d "$(git_dir_file suda-autosave.XXXXXX)")
  list="$WS_TMP/untracked"
  real=$(git rev-parse --git-path index)
  if [ -f "$real" ]; then
    cp "$real" "$WS_TMP/index"
  fi
  : >"$list"
  git ls-files --others --exclude-standard -z | while IFS= read -r -d '' f; do
    if [ -L "$f" ]; then
      :
    elif [ -f "$f" ] && [ -r "$f" ]; then
      size=$(wc -c <"$f" | tr -d ' ')
      if [ "$size" -gt "$max" ]; then
        log "skipped untracked $f: $size bytes exceeds SUDA_AUTOSAVE_MAX_BYTES=$max"
        continue
      fi
    else
      log "skipped untracked $f: not a readable regular file"
      continue
    fi
    printf '%s\0' "$f" >>"$list"
  done
  GIT_INDEX_FILE="$WS_TMP/index" git add -u
  if [ -s "$list" ]; then
    GIT_INDEX_FILE="$WS_TMP/index" git update-index -z --add --stdin <"$list"
  fi
  tree=$(GIT_INDEX_FILE="$WS_TMP/index" git write-tree)

  head_tree=$(git rev-parse 'HEAD^{tree}')
  last_file=$(git_dir_file suda-autosave.last)
  last=""
  if [ -f "$last_file" ]; then
    last=$(cat "$last_file")
  fi
  last_tree=""
  last_parent=""
  if [ -n "$last" ] && git cat-file -e "$last^{commit}" 2>/dev/null; then
    last_tree=$(git rev-parse "$last^{tree}")
    last_parent=$(git rev-parse "$last^")
  fi
  # Nothing to back up when the state was already saved on this HEAD, or when it equals HEAD and no
  # autosave exists yet. A later autosave on a working tree that was committed since (state equal to
  # the new HEAD) still replaces the older, now stale, autosave so that resume restores the commit.
  if [ "$tree" = "$last_tree" ] && [ "$(git rev-parse HEAD)" = "$last_parent" ]; then
    echo "autosave: nothing new to save on $BRANCH"
    return 0
  fi
  if [ -z "$last_tree" ] && [ "$tree" = "$head_tree" ]; then
    echo "autosave: nothing new to save on $BRANCH"
    return 0
  fi

  commit=$(git commit-tree "$tree" -p HEAD -m "autosave: $BRANCH $(date -u '+%Y-%m-%dT%H:%M:%SZ')")
  remote=$(git ls-remote ws "$AUTOSAVE_REF") || die "cannot list $AUTOSAVE_REF on ws; nothing pushed"
  expected=$(printf '%s\n' "$remote" | awk -v ref="$AUTOSAVE_REF" '$2 == ref { print $1 }')
  if ! git push --quiet --force-with-lease="$AUTOSAVE_REF:$expected" ws "$commit:$AUTOSAVE_REF"; then
    die "push of $(git rev-parse --short "$commit") to $AUTOSAVE_REF failed; the next run retries"
  fi
  printf '%s\n' "$commit" >"$last_file"
  echo "autosaved $(git rev-parse --short "$commit") to $AUTOSAVE_REF"
}

# autosave_pid: prints the pid of this worktree's running autosave loop; returns 1 when none runs.
autosave_pid() {
  local pid
  pid=$(cat "$(git_dir_file suda-autosave.pid)" 2>/dev/null || true)
  case "$pid" in
    '' | *[!0-9]*) return 1 ;;
  esac
  kill -0 "$pid" 2>/dev/null || return 1
  case "$(ps -o command= -p "$pid" 2>/dev/null || true)" in
    *ws.sh*) ;;
    *) return 1 ;;
  esac
  printf '%s\n' "$pid"
}

autosave_start() {
  local interval pid log_file
  [ $# -eq 0 ] || usage_error "usage: ws.sh autosave start"
  autosave_target
  interval=${SUDA_AUTOSAVE_INTERVAL:-10}
  case "$interval" in
    '' | *[!0-9]*) usage_error "SUDA_AUTOSAVE_INTERVAL must be a whole number of seconds" ;;
  esac
  [ "$interval" -gt 0 ] || usage_error "SUDA_AUTOSAVE_INTERVAL must be at least 1 second"
  if pid=$(autosave_pid); then
    die "autosave is already running (pid $pid)"
  fi
  log_file=$(git_dir_file suda-autosave.log)
  SUDA_AUTOSAVE_INTERVAL=$interval nohup bash "$SELF" autosave loop >>"$log_file" 2>&1 </dev/null &
  pid=$!
  printf '%s\n' "$pid" >"$(git_dir_file suda-autosave.pid)"
  echo "autosave started (pid $pid, every ${interval}s, log $log_file)"
}

# autosave_loop: the detached process started by `autosave start`. Each round runs `autosave once` in a
# fresh process, so a failed push is logged and retried instead of ending the loop. It exits when its
# pid file is removed (by `autosave stop`) or replaced.
autosave_loop() {
  local interval pid_file
  interval=${SUDA_AUTOSAVE_INTERVAL:-10}
  pid_file=$(git_dir_file suda-autosave.pid)
  printf '%s\n' "$$" >"$pid_file"
  while [ "$(cat "$pid_file" 2>/dev/null || true)" = "$$" ]; do
    bash "$SELF" autosave once || log "autosave failed; retrying in ${interval}s"
    sleep "$interval"
  done
}

autosave_stop() {
  local pid i=0
  [ $# -eq 0 ] || usage_error "usage: ws.sh autosave stop"
  if ! pid=$(autosave_pid); then
    rm -f "$(git_dir_file suda-autosave.pid)"
    echo "autosave is not running"
    return 0
  fi
  kill "$pid"
  while kill -0 "$pid" 2>/dev/null && [ "$i" -lt 50 ]; do
    sleep 0.1
    i=$((i + 1))
  done
  rm -f "$(git_dir_file suda-autosave.pid)"
  echo "autosave stopped (pid $pid)"
}

autosave_status() {
  local pid last last_file
  [ $# -eq 0 ] || usage_error "usage: ws.sh autosave status"
  if pid=$(autosave_pid); then
    echo "running (pid $pid)"
  else
    echo "stopped"
  fi
  last_file=$(git_dir_file suda-autosave.last)
  last=""
  if [ -f "$last_file" ]; then
    last=$(cat "$last_file")
  fi
  if [ -n "$last" ] && git rev-parse -q --verify "$last^{commit}" >/dev/null 2>&1; then
    echo "last autosave: $(git rev-parse --short "$last")"
  else
    echo "last autosave: none"
  fi
}

cmd_autosave() {
  local action="${1:-}"
  if [ $# -gt 0 ]; then
    shift
  fi
  case "$action" in
    once) autosave_once "$@" ;;
    start) autosave_start "$@" ;;
    stop) autosave_stop "$@" ;;
    status) autosave_status "$@" ;;
    loop) autosave_loop "$@" ;;
    *) usage_error "usage: ws.sh autosave once|start|stop|status" ;;
  esac
}

cmd_resume() {
  local force=0 name="" branch commit parent cur ref
  while [ $# -gt 0 ]; do
    case "$1" in
      --force) force=1 ;;
      -*) usage_error "usage: ws.sh resume [--force] [<actor>/<id>]" ;;
      *)
        [ -z "$name" ] || usage_error "usage: ws.sh resume [--force] [<actor>/<id>]"
        name=$1
        ;;
    esac
    shift
  done
  if [ -n "$name" ]; then
    parse_workspace "$name" || usage_error "invalid workspace name: $name (want <actor>/<id>)"
  else
    need_branch
    case "$BRANCH" in
      ws/*) parse_workspace "${BRANCH#ws/}" || die "current branch $BRANCH is not ws/<actor>/<id>" ;;
      *) die "current branch $BRANCH is not a workspace branch; pass <actor>/<id>" ;;
    esac
  fi
  [ -z "$(git status --porcelain)" ] ||
    die "the working tree has changes; commit, stash or remove them before resume"
  ref="refs/ws/autosave/$ACTOR/$ID"
  with_fetch_lock git fetch --quiet --no-tags --filter=blob:none ws "+$ref:refs/remotes/ws/ws/autosave/$ACTOR/$ID" ||
    die "cannot fetch $ref from ws; is there an autosave for ws/$ACTOR/$ID?"
  commit=$(git rev-parse --verify "refs/remotes/ws/ws/autosave/$ACTOR/$ID^{commit}")
  parent=$(git rev-parse --verify "$commit^1") || die "autosave $commit has no parent to restore onto"
  branch="ws/$ACTOR/$ID"
  cur=$(git rev-parse --verify -q "refs/heads/$branch" || true)
  if [ -n "$cur" ] && [ "$cur" != "$parent" ] && [ "$force" -ne 1 ]; then
    die "$branch is at ${cur:0:7}, not at the autosave parent ${parent:0:7}; pass --force to resume onto it"
  fi
  if [ -z "$cur" ]; then
    git switch -q -c "$branch" "$parent"
  elif [ "$(git symbolic-ref -q --short HEAD || true)" != "$branch" ]; then
    git switch -q "$branch"
  fi
  git read-tree --reset -u "$commit"
  git reset -q --mixed "$parent"
  printf '%s\n' "$commit" >"$(git_dir_file suda-autosave.last)"
  echo "resumed $ref onto $branch at ${parent:0:7}; the autosaved changes are uncommitted"
}

cmd_cl() {
  local pattern hits
  [ $# -eq 1 ] || usage_error "usage: ws.sh cl <number|I<40 hex>>"
  case "$1" in
    I*)
      [[ $1 =~ ^I[0-9a-f]{40}$ ]] || usage_error "not a Change-Id: $1"
      pattern="^Change-Id: $1\$"
      ;;
    *)
      [[ $1 =~ ^[0-9]+$ ]] || usage_error "expected a review number or a Change-Id: $1"
      pattern="^Suda-Review: .*#$1\$"
      ;;
  esac
  with_fetch_lock git fetch --quiet --no-tags --filter=blob:none origin refs/suda/history ||
    die "cannot fetch refs/suda/history from origin"
  hits=$(git log FETCH_HEAD -E --format='%h %ad %s' --date=short --grep="$pattern")
  [ -n "$hits" ] || exit 1
  printf '%s\n' "$hits"
}

cmd_wt() {
  local name dir p mode
  local -a names
  [ $# -ge 1 ] && [ $# -le 2 ] || usage_error "usage: ws.sh wt <actor>/<id> [<dir>]"
  name=$1
  parse_workspace "$name" || usage_error "invalid workspace name: $name (want <actor>/<id>)"
  dir=${2:-"../$ID"}
  with_fetch_lock git fetch --quiet --no-tags --filter=blob:none origin || die "cannot fetch origin"
  git worktree add --quiet --no-checkout -b "ws/$name" "$dir" origin/suda/main
  if [ "$(git config --bool core.sparseCheckout 2>/dev/null || true)" = true ]; then
    mode=--cone
    if [ "$(git config --bool core.sparseCheckoutCone 2>/dev/null || true)" = false ]; then
      mode=--no-cone
    fi
    names=(.agents .github)
    while IFS= read -r p; do
      names+=("$p")
    done < <(git sparse-checkout list)
    git -C "$dir" sparse-checkout set "$mode" "${names[@]}"
  fi
  git -C "$dir" checkout --quiet
  echo "worktree $dir on ws/$name"
}

# cmd_init: what `git suda-init` does. It installs .gitconfig and this script from the checkout into the common git
# dir (by rename, so a running autosave loop keeps reading its old copy), keeps the installed tools when the
# checkout predates them, configures the trunk and ws remotes, and fetches the trunk.
cmd_init() {
  local top common
  [ $# -eq 0 ] || usage_error "usage: ws.sh init"
  top=$(git rev-parse --show-toplevel)
  common=$(common_dir)
  if [ -f "$top/.github/scripts/ws.sh" ] && [ -f "$top/.gitconfig" ]; then
    cp "$top/.gitconfig" "$common/suda.gitconfig.tmp"
    cp "$top/.github/scripts/ws.sh" "$common/suda-ws.sh.tmp"
    mv -f "$common/suda.gitconfig.tmp" "$common/suda.gitconfig"
    mv -f "$common/suda-ws.sh.tmp" "$common/suda-ws.sh"
    echo "suda-init: installed .gitconfig and ws.sh from $top into $common"
  elif [ -f "$common/suda-ws.sh" ] && [ -f "$common/suda.gitconfig" ]; then
    echo "suda-init: this checkout predates .github/scripts/ws.sh; keeping the installed tools" >&2
  else
    die "this checkout has no .github/scripts/ws.sh and no tools are installed; run git suda-init on main or a current workspace"
  fi
  git config include.path suda.gitconfig
  git config --replace-all remote.origin.fetch '+refs/suda/*:refs/remotes/origin/suda/*' '\+refs/suda/\*'
  git config remote.origin.pushurl no-push://gosuda-0-is-written-by-the-lander-only
  if ! git config remote.ws.url >/dev/null 2>&1; then
    git remote add ws "$(git remote get-url origin | sed -E 's#/0(\.git)?$#/1\1#')"
  fi
  git config remote.ws.fetch '+refs/ws/*:refs/remotes/ws/ws/*'
  with_fetch_lock git fetch --filter=blob:none origin
}

cmd_fetch() {
  with_fetch_lock git fetch "$@"
}

main() {
  local cmd="${1:-}"
  if [ $# -gt 0 ]; then
    shift
  fi
  case "$cmd" in
    "") usage_error "usage: ws.sh <change-id|save|land|pr|ps|psdiff|autosave|resume|cl|wt|init|fetch> [args]" ;;
    -h | --help | help)
      usage
      return 0
      ;;
  esac
  need_worktree
  case "$cmd" in
    change-id) cmd_change_id "$@" ;;
    save) cmd_save "$@" ;;
    land) cmd_land "$@" ;;
    pr) cmd_pr "$@" ;;
    ps) cmd_ps "$@" ;;
    psdiff) cmd_psdiff "$@" ;;
    autosave) cmd_autosave "$@" ;;
    resume) cmd_resume "$@" ;;
    cl) cmd_cl "$@" ;;
    wt) cmd_wt "$@" ;;
    init) cmd_init "$@" ;;
    fetch) cmd_fetch "$@" ;;
    *) usage_error "unknown subcommand: $cmd" ;;
  esac
}

main "$@"
