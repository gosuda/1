---
name: suda-resolve-conflicts
description: >-
  Resolves rebase and merge-tree conflicts when synchronizing a workspace with
  refs/suda/main in the gosuda/0 monorepo. Activate when git ws-sync or
  land.sh exits with status 1 due to overlapping edits.
---

# Conflict Resolution Procedure

This skill defines the procedure for resolving conflicts between a workspace branch and `refs/suda/main`.

## 1. Local Rebase Conflict Resolution

When `git ws-sync` stops on a conflicting commit, inspect the unmerged paths, resolve the conflict markers, and continue the rebase.

1. List all unmerged file paths using `git diff --name-only --diff-filter=U`.
2. Edit each conflicting file to resolve the `<<<<<<<`, `=======`, and `>>>>>>>` markers.
3. Run the package test suite in the affected domain directory to verify the merged logic.
4. Stage the resolved files with `git add`.
5. Complete the rebase with `GIT_EDITOR=true git rebase --continue`.
6. Push the updated branch with `git ws-save` or `git ws-land`.

```bash
git diff --name-only --diff-filter=U
# Edit conflicting files and run package tests
git add -A
GIT_EDITOR=true git rebase --continue
git ws-save
```

## 2. Inspecting Non-Checked-Out Conflicts

Preview conflicts in memory before running a rebase by invoking `git merge-tree --write-tree`.

1. Fetch the latest `origin/suda/main` snapshot.
2. Resolve the root commit of the current workspace branch.
3. Run `git merge-tree --write-tree` with `--merge-base` to list conflicting paths without modifying the working tree.

```bash
git fetch --filter=blob:none origin
BASE=$(git rev-list --max-parents=0 HEAD)
git merge-tree --write-tree --merge-base="${BASE}^{tree}" "origin/suda/main^{tree}" "HEAD^{tree}"
```
