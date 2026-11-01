---
name: suda-resolve-conflicts
description: >-
  Resolves rebase, merge-tree, and staged mirror-import conflicts when
  synchronizing a workspace with refs/suda/main in the gosuda/0 monorepo.
  Activate when git ws-sync stops on a conflict, when the lander rejects a
  landing request with a merge conflict, or when a staged refs/ws/capy/* import
  conflicts with the trunk.
---

# Conflict Resolution Procedure

This skill defines the procedure for resolving conflicts between a workspace branch, a staged mirror import, and `refs/suda/main`. The lander performs the final merge in memory and rejects a request that conflicts. It never resolves conflicts itself.

## 1. Local Rebase Conflict Resolution

When `git ws-sync` stops on a conflicting commit, inspect the unmerged paths, resolve the conflict markers, and continue the rebase.

1. List the unmerged file paths with `git diff --name-only --diff-filter=U`.
2. Edit each conflicting file to resolve the `<<<<<<<`, `=======`, and `>>>>>>>` markers.
3. Run the package test suite in the affected domain directory to verify the merged logic.
4. Stage the resolved files with `git add`.
5. Complete the rebase with `GIT_EDITOR=true git rebase --continue`.
6. Save the branch with `git ws-save`, then submit it with `git ws-land`.

```bash
git diff --name-only --diff-filter=U
# Edit conflicting files and run package tests
git add -A
GIT_EDITOR=true git rebase --continue
git ws-save
git ws-land
```

## 2. Lander-Reported Merge Conflicts

When the lander rejects a request with `merge conflict: <paths>`, the trunk changed the same lines after your branch started. The lander does not check out files, so the conflict exists only in the merge it computed. Reproduce it locally before resolving it.

1. Fetch the latest `origin/suda/main` snapshot.
2. Preview the conflict in memory. The output lists the conflicting paths:

   ```bash
   git fetch --filter=blob:none origin
   BASE=$(git rev-list --max-parents=0 HEAD)
   git merge-tree --write-tree --name-only --merge-base="${BASE}^{tree}" "origin/suda/main^{tree}" "HEAD^{tree}"
   ```

3. Run `git ws-sync` and resolve the conflicts as in Section 1.
4. Submit the rebased branch again with `git ws-save` and `git ws-land`. The rejected land ref was deleted, so the push creates a new request.

## 3. Resolving a Staged Mirror Import

A staged `refs/ws/capy/<wf>-main` or `refs/ws/capy/<wf>-pr-<N>` ref is an import of the mirror built on the baseline monorepo tree. Its root commit carries that baseline tree, and its tip carries the mirror's content for the prefix. A conflict means the trunk changed the same paths since the baseline.

1. Fetch the staged refs and switch to a workspace branch created from them:

   ```bash
   git fetch ws
   git switch -c ws/agent-dalgona/capy-HQQ-main refs/remotes/ws/ws/capy/HQQ-main
   ```

2. Run `git ws-sync`. The import is replayed onto the current trunk, so the conflicts appear locally.
3. Resolve each conflict so that both the trunk change and the mirror change are kept. Run the affected project's tests.
4. Save and land: `git ws-save` and `git ws-land`. You are the requester, so the `OWNERS` rules apply to you.

A mirror whose change is staged does not export until the change is incorporated. Resolving and landing the staged change is the step that unblocks it.

## 4. Diagnostics

- `git diff --name-only --diff-filter=U` lists unmerged paths during a rebase.
- `git log --oneline origin/suda/main -5 -- <path>` shows recent trunk changes to a conflicting path.
- `git log --oneline refs/remotes/ws/ws/capy/<wf>-main -- <path>` shows the staged change for the same path.
