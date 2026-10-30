---
name: suda-workspace
description: >-
  Initializes, synchronizes, and submits isolated workspaces in the gosuda/0
  monorepo using standard Git commands, sparse checkout, and refs/suda/main.
  Activate when creating a new task branch, saving work-in-progress to refs/ws/*,
  or submitting a branch to refs/heads/land/*.
---

# Monorepo Workspace Procedure

This skill defines the standard operating procedure for creating, updating, and landing an isolated workspace in `gosuda/0`.

## 1. Workspace Initialization

Attach the hidden `refs/suda/*` trunk namespace and restrict the working directory to the required domain paths.

> [!IMPORTANT]
> Always configure sparse checkout before switching to `origin/suda/main`. Checking out `origin/suda/main` without sparse checkout materializes every project in the monorepo on disk.

1. Enable the repository Git aliases from `.gitconfig`.
2. Fetch `refs/suda/main` and `refs/suda/history` in blobless mode.
3. Configure sparse checkout for the target domain directories.
4. Create a new workspace branch rooted at `origin/suda/main`.

```bash
git config include.path ../.gitconfig
git suda-init
git sparse-checkout set ipc/hqq web/gosuda
git ws-new agent-dalgona/task-101
```

## 2. Saving Work and Submitting to the Landing Queue

Commit changes locally, push work-in-progress to `refs/ws/*`, rebase onto the latest trunk snapshot, and enqueue for landing.

> [!CAUTION]
> Never run `git pull` or `git push --force` without `--force-with-lease`. Running `git pull` fails against the parentless `refs/suda/main` commit, and unconditional force pushes overwrite concurrent updates.

1. Run the package test suite inside each modified domain directory.
2. Stage and commit all modified files with a `<domain>/<project>: <summary>` subject line.
3. Push the workspace branch to `refs/ws/<actor>/<id>` using `git ws-save`.
4. Rebase the workspace branch onto the latest `origin/suda/main` snapshot using `git ws-sync`.
5. Push the branch to `refs/heads/land/<id>` using `git ws-land`.

```bash
git add -A
git commit -m "ipc/hqq: validate frame length before allocation"
git ws-save
git ws-sync
git ws-land
```
