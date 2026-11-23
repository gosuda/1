#!/usr/bin/env bash
# Smoke test for import-repo.sh, create-repo.sh, go-work-add.sh and ws.sh against a throwaway trunk-like
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

# 3b. go.mod files under testdata/ or vendor/ are fixtures and never join go.work.
mkup "$TMP/up-fixture" example.org/fixture 1.24.0
mkdir -p "$TMP/up-fixture/testdata/mod" "$TMP/up-fixture/vendor/v"
printf 'module example.org/fixture/testdata\n' >"$TMP/up-fixture/testdata/mod/go.mod"
printf 'module example.org/fixture/vendored\n' >"$TMP/up-fixture/vendor/v/go.mod"
git -C "$TMP/up-fixture" add -A
git -C "$TMP/up-fixture" commit -q -m fixtures
SUDA_IMPORT_URL="$TMP/up-fixture" "$SCRIPT_DIR/import-repo.sh" storage/fixture gosuda/fixture >/dev/null
"$SCRIPT_DIR/go-work-add.sh" storage/fixture >/dev/null
check "the fixture's root module joins go.work" grep -qx $'\t./storage/fixture' go.work
check_not "testdata modules stay out of go.work" grep -q 'testdata' go.work
check_not "vendor modules stay out of go.work" grep -q 'vendor' go.work

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

# 7. ws.sh against throwaway stand-ins: origin = gosuda/0 (trunk and history), ws = gosuda/1 (workspaces).
# gh is a double on PATH, so no network is touched: one double opens a PR, the other fails (no gh).
REPO=$(cd "$SCRIPT_DIR/../.." && pwd)
WS_SH="$SCRIPT_DIR/ws.sh"
G="$TMP/gosuda"
GH_STATE="$TMP/gh"
export SUDA_TEST_GH="$GH_STATE" SUDA_AUTOSAVE_MAX_BYTES=100
mkdir -p "$G" "$GH_STATE" "$TMP/bin-gh-ok" "$TMP/bin-gh-fail"
cat >"$TMP/bin-gh-ok/gh" <<'EOF'
#!/usr/bin/env bash
# gh double: no PR until one is created; then one open review PR, reported the way gh -q prints it.
case "$1 $2" in
  "pr list") if [ -f "$SUDA_TEST_GH/open" ]; then cat "$SUDA_TEST_GH/open"; fi ;;
  "pr create")
    echo "$*" >>"$SUDA_TEST_GH/created"
    echo "42 https://github.com/gosuda/1/pull/42" >"$SUDA_TEST_GH/open"
    echo "https://github.com/gosuda/1/pull/42"
    ;;
  *) exit 1 ;;
esac
EOF
printf '#!/usr/bin/env bash\nexit 1\n' >"$TMP/bin-gh-fail/gh"
chmod +x "$TMP/bin-gh-ok/gh" "$TMP/bin-gh-fail/gh"

match_id() { [[ $1 =~ ^I[0-9a-f]{40}$ ]]; }
trailer_id_of() { git cat-file commit "$1" | sed -n 's/^Change-Id: \(I[0-9a-f]\{40\}\)$/\1/p'; }
exit_is() { # exit_is <status> <command...>: succeeds when the command exits with <status>
  local want="$1" got=0
  shift
  "$@" >/dev/null 2>&1 || got=$?
  [ "$got" -eq "$want" ]
}

for r in "$G/0.git" "$G/1.git"; do
  git init -q --bare "$r"
  git -C "$r" config uploadpack.allowFilter true
  git -C "$r" config uploadpack.allowAnySHA1InWant true
done

# Trunk snapshots are parentless commits; the trunk source carries the alias file and ws.sh itself.
SRC="$TMP/trunk7"
git init -q -b main "$SRC"
mkdir -p "$SRC/web/app" "$SRC/net/other" "$SRC/.github/scripts" "$SRC/.agents"
printf 'app\n' >"$SRC/web/app/a.txt"
printf 'other\n' >"$SRC/net/other/b.txt"
printf 'agents\n' >"$SRC/.agents/README"
cp "$REPO/.gitconfig" "$SRC/.gitconfig"
cp "$SCRIPT_DIR/ws.sh" "$SRC/.github/scripts/ws.sh"
git -C "$SRC" add -A
git -C "$SRC" commit -q -m trunk
snapshot() { git -C "$SRC" commit-tree "$1" -m "$2"; }
SNAP1=$(snapshot "$(git -C "$SRC" rev-parse 'HEAD^{tree}')" "trunk snapshot 1")
git -C "$SRC" push -q "$G/0.git" "$SNAP1:refs/suda/main"

# Landed history: one commit with a Change-Id and a review number, one with only a Change-Id.
LANDED="I$(printf 'ab%.0s' $(seq 1 20))"
OTHER="I$(printf 'cd%.0s' $(seq 1 20))"
TREE1=$(git -C "$SRC" rev-parse "$SNAP1^{tree}")
H1=$(git -C "$SRC" commit-tree "$TREE1" -m "feat(app): landed change

Change-Id: $LANDED
Suda-Review: gosuda/1#42")
H2=$(git -C "$SRC" commit-tree "$TREE1" -p "$H1" -m "fix(app): later change

Change-Id: $OTHER")
git -C "$SRC" push -q "$G/0.git" "$H2:refs/suda/history"

# The workspace: sparse like a real one, then suda-init, which copies .gitconfig and adds the ws remote.
W="$TMP/work7"
git init -q -b main "$W"
git -C "$W" remote add origin "$G/0.git"
git -C "$W" fetch -q --filter=blob:none origin '+refs/suda/*:refs/remotes/origin/suda/*'
git -C "$W" sparse-checkout set --cone web/app .github .agents
git -C "$W" switch -q -c ws/tester/task-1 origin/suda/main
git -C "$W" config include.path ../.gitconfig
git -C "$W" suda-init
cd "$W"

# 7a. Change-Id: generated once, reused by amend, by a newer commit, and after a rebase onto a new snapshot.
echo "app v2" >web/app/a.txt
git add web/app/a.txt
git commit -q -m "web/app: first change"
ID1=$(bash "$WS_SH" change-id) || fail "change-id failed on a fresh tip"
check "change-id is I plus 40 lowercase hex" match_id "$ID1"
check "the tip message carries the trailer" grep -qx "Change-Id: $ID1" <(git cat-file commit HEAD)
H_BEFORE=$(git rev-parse HEAD)
check "a second change-id call reuses the id" test "$(bash "$WS_SH" change-id)" = "$ID1"
check "a second change-id call does not rewrite the tip" test "$(git rev-parse HEAD)" = "$H_BEFORE"
echo "app v3" >web/app/a.txt
git add web/app/a.txt
git commit -q --amend --no-edit
check "amend keeps the id" test "$(bash "$WS_SH" change-id)" = "$ID1"
echo "b" >web/app/b.txt
git add web/app/b.txt
git commit -q -m "web/app: second change"
check "a newer commit reuses the id of the commit below it" test "$(bash "$WS_SH" change-id)" = "$ID1"
check "the newer tip carries the reused id" grep -qx "Change-Id: $ID1" <(git cat-file commit HEAD)
printf 'other v2\n' >"$SRC/net/other/b.txt"
git -C "$SRC" add -A
git -C "$SRC" commit -q -m "trunk 2"
SNAP2=$(snapshot "$(git -C "$SRC" rev-parse 'HEAD^{tree}')" "trunk snapshot 2")
git -C "$SRC" push -q --force "$G/0.git" "$SNAP2:refs/suda/main"
git -C "$W" fetch -q --filter=blob:none origin
GIT_COMMITTER_DATE='2026-11-01T10:00:00 +0000' git rebase -q --onto origin/suda/main "$SNAP1"
check "the rebase keeps the id" test "$(bash "$WS_SH" change-id)" = "$ID1"
check "the chain is rooted at the new snapshot" test "$(git rev-list --max-parents=0 HEAD)" = "$SNAP2"

# 7b. A tip without a Change-Id is not amended while the index has staged changes; a tip with one is no obstacle.
git commit -q --amend --only -m "web/app: second change"
check "the amend dropped the trailer" test -z "$(trailer_id_of HEAD)"
echo staged >web/app/staged.txt
git add web/app/staged.txt
check "change-id refuses to amend an index with staged changes" exit_is 1 bash "$WS_SH" change-id
check "the refusal left the tip alone" test -z "$(trailer_id_of HEAD)"
git rm -q --cached web/app/staged.txt
rm -f web/app/staged.txt
check "change-id gives the tip the reused id once the index is clean" test "$(bash "$WS_SH" change-id)" = "$ID1"
check "the tip carries the reused id again" test "$(trailer_id_of HEAD)" = "$ID1"
echo staged >web/app/staged.txt
git add web/app/staged.txt
check "a tip that has its id works with staged changes" test "$(bash "$WS_SH" change-id)" = "$ID1"
git rm -q --cached web/app/staged.txt
rm -f web/app/staged.txt

# 7c. save, land and pr; patchsets accumulate across a rebase; ps, psdiff; pr without gh.
check "save pushes the workspace branch" bash "$WS_SH" save
check "save pushed the tip" test "$(git -C "$G/1.git" rev-parse refs/ws/tester/task-1)" = "$(git rev-parse HEAD)"
check "land pushes the landing ref" bash "$WS_SH" land
check "the landing ref names the branch id" test "$(git -C "$G/1.git" rev-parse refs/heads/land/task-1)" = "$(git rev-parse HEAD)"
PR1=$(PATH="$TMP/bin-gh-ok:$PATH" GIT_COMMITTER_DATE='2026-11-01T10:00:00 +0000' bash "$WS_SH" pr) || fail "first pr failed"
check "first pr opens the CL" test "$PR1" = "CL 42: https://github.com/gosuda/1/pull/42"
PS1_SHA=$(git rev-parse HEAD)
check "pr pushes the base ref at the snapshot" test "$(git -C "$G/1.git" rev-parse refs/heads/base/task-1)" = "$SNAP2"
check "pr pushes the review ref at the tip" test "$(git -C "$G/1.git" rev-parse refs/heads/review/task-1)" = "$PS1_SHA"
check "pr pushes patchset 1" test "$(git -C "$G/1.git" rev-parse -q --verify "refs/ws/cl/$ID1/$PS1_SHA")" = "$PS1_SHA"
check "pr asked gh for base/review" grep -q -- "--base base/task-1 --head review/task-1" "$GH_STATE/created"

printf 'other v3\n' >"$SRC/net/other/b.txt"
git -C "$SRC" add -A
git -C "$SRC" commit -q -m "trunk 3"
SNAP3=$(snapshot "$(git -C "$SRC" rev-parse 'HEAD^{tree}')" "trunk snapshot 3")
git -C "$SRC" push -q --force "$G/0.git" "$SNAP3:refs/suda/main"
git -C "$W" fetch -q --filter=blob:none origin
GIT_COMMITTER_DATE='2026-11-01T11:00:00 +0000' git rebase -q --onto origin/suda/main "$SNAP2"
check "the second rebase keeps the id" test "$(bash "$WS_SH" change-id)" = "$ID1"
PR2=$(PATH="$TMP/bin-gh-ok:$PATH" bash "$WS_SH" pr) || fail "second pr failed"
check "second pr reports the open CL and its new patchset" grep -q '^CL 42: https://github.com/gosuda/1/pull/42 (new patchset ' <<<"$PR2"
check "second pr does not open another pull request" test "$(wc -l <"$GH_STATE/created" | tr -d ' ')" = 1
PS2_SHA=$(git rev-parse HEAD)
check "second pr keeps patchset 1" test "$(git -C "$G/1.git" rev-parse -q --verify "refs/ws/cl/$ID1/$PS1_SHA")" = "$PS1_SHA"
check "second pr adds patchset 2" test "$(git -C "$G/1.git" rev-parse -q --verify "refs/ws/cl/$ID1/$PS2_SHA")" = "$PS2_SHA"
check "second pr moves the base ref to the new snapshot" test "$(git -C "$G/1.git" rev-parse refs/heads/base/task-1)" = "$SNAP3"

PS_OUT=$(bash "$WS_SH" ps) || fail "ps failed"
check "ps lists PS1 before PS2" test "$(printf '%s\n' "$PS_OUT" | grep -o 'PS[0-9]' | tr '\n' ' ')" = "PS1 PS2 "
check "ps marks the patchset at HEAD" grep -q '^\* PS2 ' <<<"$PS_OUT"
DIFF=$(bash "$WS_SH" psdiff PS1 2) || fail "psdiff failed"
check "psdiff prints a range-diff" test -n "$DIFF"
check "psdiff accepts a sha prefix" bash "$WS_SH" psdiff "${PS1_SHA:0:10}" PS2

URL=$(PATH="$TMP/bin-gh-fail:$PATH" bash "$WS_SH" pr) || fail "pr without a working gh failed"
check "pr falls back to the compare URL without gh" test "$URL" = "https://github.com/gosuda/1/compare/base/task-1...review/task-1"

# 7d. autosave: a private commit with the working state; the real index, HEAD and branch stay put.
echo "wip" >>web/app/a.txt
rm web/app/b.txt
echo "new" >web/app/new.txt
head -c 200 /dev/zero >web/app/big.bin
printf '*.local\n' >>.git/info/exclude
echo "local" >web/app/private.local
BEFORE_HEAD=$(git rev-parse HEAD)
BEFORE_BRANCH=$(git symbolic-ref HEAD)
BEFORE_INDEX=$(git ls-files --stage | git hash-object --stdin)
BEFORE_STATUS=$(git status --porcelain)
out1=$(bash "$WS_SH" autosave once 2>&1) || fail "autosave once failed: $out1"
AS1=$(git -C "$G/1.git" rev-parse -q --verify refs/ws/autosave/tester/task-1) || fail "autosave ref was not created"
check "autosave logs the oversized untracked file" grep -q 'skipped untracked web/app/big.bin' <<<"$out1"
TREE_FILES=$(git -C "$G/1.git" ls-tree -r --name-only "$AS1")
check "autosave tree holds the untracked file" grep -qx 'web/app/new.txt' <<<"$TREE_FILES"
check_not "autosave tree lacks the oversized file" grep -qx 'web/app/big.bin' <<<"$TREE_FILES"
check_not "autosave tree lacks the ignored file" grep -qx 'web/app/private.local' <<<"$TREE_FILES"
check_not "autosave tree lacks the deleted file" grep -qx 'web/app/b.txt' <<<"$TREE_FILES"
check "autosave tree holds the modification" test "$(git -C "$G/1.git" show "$AS1:web/app/a.txt")" = "$(printf 'app v3\nwip')"
check "autosave leaves HEAD alone" test "$(git rev-parse HEAD)" = "$BEFORE_HEAD"
check "autosave leaves the branch alone" test "$(git symbolic-ref HEAD)" = "$BEFORE_BRANCH"
check "autosave leaves the index alone" test "$(git ls-files --stage | git hash-object --stdin)" = "$BEFORE_INDEX"
check "autosave leaves the working state alone" test "$(git status --porcelain)" = "$BEFORE_STATUS"
out2=$(bash "$WS_SH" autosave once 2>&1) || fail "second autosave failed"
check "an unchanged tree creates no new autosave commit" test "$(git -C "$G/1.git" rev-parse refs/ws/autosave/tester/task-1)" = "$AS1"

SUDA_AUTOSAVE_INTERVAL=1 bash "$WS_SH" autosave start >/dev/null || fail "autosave start failed"
check_not "a second autosave start refuses" bash "$WS_SH" autosave start
check "autosave status reports running" grep -q '^running' <<<"$(bash "$WS_SH" autosave status)"
echo "more" >>web/app/a.txt
i=0
while [ "$i" -lt 50 ] && [ "$(git -C "$G/1.git" rev-parse -q --verify refs/ws/autosave/tester/task-1)" = "$AS1" ]; do
  sleep 0.2
  i=$((i + 1))
done
check "the running loop autosaves a change" test "$(git -C "$G/1.git" rev-parse refs/ws/autosave/tester/task-1)" != "$AS1"
check "autosave stop stops the loop" bash "$WS_SH" autosave stop
check "autosave status after stop is stopped" grep -q '^stopped' <<<"$(bash "$WS_SH" autosave status)"
AS_LAST=$(git -C "$G/1.git" rev-parse refs/ws/autosave/tester/task-1)

# 7e. resume on a second clone: the working state comes back as uncommitted changes on the parent.
C2="$TMP/clone7"
git init -q -b main "$C2"
git -C "$C2" remote add ws "$G/1.git"
cd "$C2"
echo dirty >dirty.txt
check_not "resume refuses a dirty working tree" bash "$WS_SH" resume tester/task-1
rm -f dirty.txt
check "resume restores the autosaved state" bash "$WS_SH" resume tester/task-1
PARENT=$(git -C "$G/1.git" rev-parse "$AS_LAST^1")
check "resume leaves HEAD at the autosave parent" test "$(git rev-parse HEAD)" = "$PARENT"
check "resume leaves the branch on ws/tester/task-1" test "$(git symbolic-ref --short HEAD)" = ws/tester/task-1
check "resume restores the modified file" test "$(cat web/app/a.txt)" = "$(git -C "$G/1.git" show "$AS_LAST:web/app/a.txt")"
check "resume restores the new file" test "$(cat web/app/new.txt)" = new
check_not "resume restores the deletion" test -e web/app/b.txt
check_not "resume does not bring back the oversized file" test -e web/app/big.bin
check "resume shows the changes against the parent" test "$(git status --porcelain | sort)" = "$(printf ' D web/app/b.txt\n M web/app/a.txt\n?? web/app/new.txt\n' | sort)"
git reset -q --hard
git clean -qfd
git commit -q --allow-empty -m "diverged"
check_not "resume refuses a branch that moved off the autosave parent" bash "$WS_SH" resume tester/task-1
check "resume --force moves the branch back onto the parent" bash "$WS_SH" resume --force tester/task-1
check "the forced resume lands on the parent" test "$(git rev-parse HEAD)" = "$PARENT"

# 7f. cl, wt, and the aliases from .gitconfig.
cd "$W"
check "cl finds a landed change by Change-Id" grep -q 'feat(app): landed change' <<<"$(bash "$WS_SH" cl "$LANDED")"
check "cl finds a landed change by review number" grep -q 'feat(app): landed change' <<<"$(bash "$WS_SH" cl 42)"
check_not "cl exits 1 when no change matches" bash "$WS_SH" cl 4
check_not "cl exits 1 for an unknown Change-Id" bash "$WS_SH" cl "I$(printf '1%.0s' $(seq 1 40))"
check "wt creates a worktree on the new workspace branch" bash "$WS_SH" wt tester/task-2
WT="$TMP/task-2"
check "wt branch is ws/tester/task-2" test "$(git -C "$WT" symbolic-ref --short HEAD)" = ws/tester/task-2
check "wt starts at origin/suda/main" test "$(git -C "$WT" rev-parse HEAD)" = "$(git -C "$W" rev-parse origin/suda/main)"
check "wt materializes the sparse cone" test -f "$WT/web/app/a.txt"
check_not "wt leaves directories outside the cone out" test -e "$WT/net/other/b.txt"
check "wt keeps .github" test -f "$WT/.github/scripts/ws.sh"
check "wt rejects a malformed name with exit 2" exit_is 2 bash "$WS_SH" wt 'bad name'
check_not "the trunk snapshot has no change to identify" bash -c "cd '$WT' && bash '$WS_SH' change-id"
check "git ws-id runs ws.sh" test "$(git ws-id)" = "$(bash "$WS_SH" change-id)"
check "git suda-cl runs cl" grep -q 'landed change' <<<"$(git suda-cl 42)"

# 7g. Committing the autosaved work replaces the stale autosave, so that resume restores the commit.
git add web/app/a.txt web/app/new.txt
git rm -q web/app/b.txt
git commit -q -m "web/app: the work in progress, committed"
bash "$WS_SH" autosave once >/dev/null 2>&1 || fail "autosave after the commit failed"
AS3=$(git -C "$G/1.git" rev-parse refs/ws/autosave/tester/task-1)
check "autosave after a commit replaces the older autosave" test "$AS3" != "$AS_LAST"
check "the new autosave sits on the new HEAD" test "$(git -C "$G/1.git" rev-parse "$AS3^1")" = "$(git rev-parse HEAD)"
check "the new autosave carries exactly the committed tree" test "$(git -C "$G/1.git" rev-parse "$AS3^{tree}")" = "$(git rev-parse 'HEAD^{tree}')"
bash "$WS_SH" autosave once >/dev/null 2>&1 || fail "repeated autosave failed"
check "repeating it creates no new autosave" test "$(git -C "$G/1.git" rev-parse refs/ws/autosave/tester/task-1)" = "$AS3"

# 7h. suda-init installs .gitconfig and ws.sh into the common git dir and the aliases run that copy: a branch whose
# tree predates ws.sh keeps working, suda-init there keeps the installed tools, and a linked worktree installs
# into the common dir as well.
check "suda-init installed ws.sh into the common git dir" cmp "$WS_SH" "$W/.git/suda-ws.sh"
check "suda-init installed the alias file into the common git dir" cmp "$REPO/.gitconfig" "$W/.git/suda.gitconfig"
git switch -q -c ws/tester/old-tools origin/suda/main
git rm -q .github/scripts/ws.sh .gitconfig
git commit -q -m "web/app: a branch that predates the tools"
check "the old branch has no ws.sh in its tree" test ! -e .github/scripts/ws.sh
check "git ws-id runs the installed copy on a branch without ws.sh" match_id "$(git ws-id)"
INSTALLED=$(git hash-object "$W/.git/suda-ws.sh")
INIT_OUT=$(git suda-init 2>&1) || fail "suda-init on a checkout without ws.sh failed: $INIT_OUT"
check "suda-init there says it keeps the installed tools" grep -q 'keeping the installed tools' <<<"$INIT_OUT"
check "suda-init there leaves the installed ws.sh alone" test "$(git hash-object "$W/.git/suda-ws.sh")" = "$INSTALLED"
git switch -q ws/tester/task-1
git worktree add -q -b ws/tester/task-3 "$TMP/wt3" origin/suda/main
rm -f "$W/.git/suda-ws.sh"
check "suda-init in a linked worktree succeeds" bash -c "cd '$TMP/wt3' && git suda-init"
check "it installs into the common git dir" test -f "$W/.git/suda-ws.sh"
check_not "it does not install into the worktree's private git dir" test -e "$W/.git/worktrees/wt3/suda-ws.sh"

# 7i. Fetches of all worktrees are serialized by one lock in the common git dir (without it, two concurrent fetches
# that move the same remote-tracking ref fail with "cannot lock ref"). A lock left by a dead process is broken; a
# lock held by a live one makes the fetch wait and then give up.
FAILS=0
for i in 1 2 3 4 5 6; do
  S=$(snapshot "$(git -C "$SRC" rev-parse 'HEAD^{tree}')" "trunk round $i")
  git -C "$SRC" push -q --force "$G/0.git" "$S:refs/suda/main"
  git suda-ws fetch -q origin 2>>"$TMP/fetch-errors" &
  P1=$!
  (cd "$TMP/wt3" && git suda-ws fetch -q origin) 2>>"$TMP/fetch-errors" &
  P2=$!
  wait "$P1" || FAILS=$((FAILS + 1))
  wait "$P2" || FAILS=$((FAILS + 1))
done
check "concurrent fetches from two worktrees all succeed" test "$FAILS" -eq 0
check "the last fetch saw the last trunk snapshot" test "$(git rev-parse origin/suda/main)" = "$S"
LOCK="$W/.git/suda-fetch.lock"
mkdir "$LOCK"
sh -c 'echo $$ >"$1"' _ "$LOCK/pid"
check "a lock left by a dead process is broken" git suda-ws fetch -q origin
check "the broken lock is gone afterwards" test ! -e "$LOCK"
mkdir "$LOCK"
echo $$ >"$LOCK/pid"
check "a lock held by a live process makes the fetch give up after the timeout" exit_is 1 env SUDA_FETCH_LOCK_TIMEOUT=1 git suda-ws fetch -q origin
check "giving up leaves the other process's lock in place" test "$(cat "$LOCK/pid")" = "$$"
rm -rf "$LOCK"

# 7j. Embargoed work keeps its patchsets under refs/ws/cl/sec/ (hidden by magpie, embargo TTL in the lander), by
# branch name or by a sec- subject.
git switch -q -c ws/tester/sec/sec-202611-0a1b2c3d origin/suda/main
echo "fix" >web/app/a.txt
git commit -qam "sec-202611-0a1b2c3d: harden input handling"
PATH="$TMP/bin-gh-fail:$PATH" bash "$WS_SH" pr >/dev/null 2>&1 || fail "pr on an embargoed branch failed"
SEC_ID=$(trailer_id_of HEAD)
check "an embargoed branch publishes its patchset under refs/ws/cl/sec/" git -C "$G/1.git" rev-parse -q --verify "refs/ws/cl/sec/$SEC_ID/$(git rev-parse HEAD)"
check_not "and not under refs/ws/cl/<change>/" test -n "$(git -C "$G/1.git" for-each-ref "refs/ws/cl/$SEC_ID")"
check "ps lists the embargoed patchset" grep -q '^\* PS1 ' <<<"$(bash "$WS_SH" ps)"
git switch -q -c ws/tester/plain-name origin/suda/main
echo "fix 2" >web/app/a.txt
git commit -qam "sec-202611-0a1b2c3e: harden more input handling"
PATH="$TMP/bin-gh-fail:$PATH" bash "$WS_SH" pr >/dev/null 2>&1 || fail "pr with an embargoed subject failed"
check "a sec- subject alone also selects refs/ws/cl/sec/" git -C "$G/1.git" rev-parse -q --verify "refs/ws/cl/sec/$(trailer_id_of HEAD)/$(git rev-parse HEAD)"
git switch -q ws/tester/task-1

cd "$SCRIPT_DIR"

echo "ok"
