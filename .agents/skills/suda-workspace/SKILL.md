---
name: suda-workspace
description: >-
  Initializes, synchronizes, and submits isolated workspaces in the gosuda/0
  monorepo using standard Git commands, sparse checkout, the gosuda/1 ws remote,
  and the lander landing queue. Activate when creating a new task branch, saving
  work-in-progress to gosuda/1, or submitting a branch for landing.
---

# Monorepo Workspace Procedure

This skill defines the standard operating procedure for creating, updating, and landing an isolated workspace. The control plane and the trunk come from `origin` (`gosuda/0`, read-only for members). Workspaces, landing requests, and review pull requests go to the `ws` remote (`gosuda/1`).

## 1. Workspace Initialization

Attach the trunk namespace and the `ws` remote, and restrict the working directory to the required domain paths.

> [!IMPORTANT]
> Always configure sparse checkout before switching to `origin/suda/main`. Checking out `origin/suda/main` without sparse checkout materializes every project in the monorepo on disk.

1. Enable the repository Git aliases from `.gitconfig`.
2. Run `git suda-init`. It adds the `ws` remote, disables pushes to `origin`, and fetches `refs/suda/main` and `refs/suda/history` in blobless mode.
3. Configure sparse checkout for the target domain directories. To add projects later, run `git sparse-checkout add <domain>/<project>`; running `set` again replaces the list and drops `.agents` and `.github` unless you name them.
4. Create a new workspace branch rooted at `origin/suda/main`.

```bash
git config include.path ../.gitconfig
git suda-init
git sparse-checkout set ipc/hqq web/gosuda
git ws-new agent-dalgona/task-101
```

## 2. Saving Work and Submitting to the Landing Queue

Commit changes locally, save work-in-progress to `gosuda/1`, rebase onto the latest trunk snapshot, and submit the landing request.

> [!CAUTION]
> Never run `git pull`, never push to `origin`, and never run `git push --force` without `--force-with-lease`. `git pull` fails against the parentless `refs/suda/main` commit, and unconditional force pushes overwrite concurrent updates. The hook in `.agents/scripts/guard-git.sh` denies these commands.

1. Run the package test suite inside each modified domain directory.
2. Stage and commit all modified files with a `<domain>/<project>: <summary>` subject line.
3. Save the branch to `refs/ws/<actor>/<id>` on `gosuda/1` with `git ws-save`.
4. Rebase the branch onto the latest `origin/suda/main` snapshot with `git ws-sync`.
5. Submit the tip with `git ws-land`. This pushes `HEAD` to `refs/heads/land/<id>` on `gosuda/1`.

`git ws-save`, `git ws-pr`, and `git ws-land` first add a `Change-Id: I<40 hex>` trailer to the tip commit when it has none (`git ws-id` does so explicitly). The id identifies the change across amends, rebases, and renames.

```bash
git add -A
git commit -m "ipc/hqq: validate frame length before allocation"
git ws-save
git ws-sync
git ws-land
```

## 3. Reading the Result

The lander processes the request on one of its next cycles, normally within a minute (immediately when the `gosuda/1` webhook wakes the lander). It reports the result as a `suda/land` status on the submitted commit in `gosuda/1`.

```bash
gh api "repos/gosuda/1/commits/$(git rev-parse HEAD)/statuses" \
  --jq '[.[] | select(.context == "suda/land")][0] | "\(.state): \(.description)"'
```

- `success`: the change landed, and its landing ref is gone from `gosuda/1`.
- `failure` with `unknown base tree`: run `git ws-sync`, then `git ws-save` and `git ws-land` again.
- `failure` with `merge conflict`: follow the `suda-resolve-conflicts` skill.
- `failure` with `visibility`, `OWNERS`, or `presubmit`: fix the reported path or test, then re-submit.
- No status yet: the request is waiting for the lander. If the requester identity is not yet visible, the lander retries on later cycles.

A rejected request never blocks other requests. The lander deletes the land ref of a rejected request, so fix the reason first and then submit again. Submitting an unchanged tip repeats the same rejection.

## 3a. Parallel Agents

When several agents work at once, each one works in its own worktree of the same clone, never in a shared working directory (`AGENTS.md` §4.6).

1. From any worktree of the clone, create the agent's worktree and branch. The new worktree gets the same sparse directories, and `.agents` and `.github` are added.

   ```bash
   git ws-wt agent-dalgona/task-403 ../task-403
   cd ../task-403
   git sparse-checkout add ipc/hqq    # optional: extra projects for this change
   ```

2. Give concurrent changes to the same project separate files when possible. A change that conflicts with an earlier landing is fixed with `git ws-sync`.
3. Do not use `git stash` (shared by all worktrees) or repository-wide `git config`. Fetch only through `git suda-ws fetch <args>` or the `ws-*` aliases, which serialize fetches across worktrees.
4. When the change has landed, remove the worktree with `git worktree remove ../task-403` from the agent that owns it.

## 4. Review Pull Request (Optional)

Use a review pull request when a change needs line-by-line comments or formal approval on GitHub.

1. Run `git ws-pr` on the workspace branch. It pushes the base commit to `refs/heads/base/<id>` and the tip to `refs/heads/review/<id>` on `gosuda/1`, then opens a pull request between them.
2. Reviewers approve the `review/<id>` tip. `git ws-pr` also pushes every tip as an immutable patchset `refs/ws/cl/<change-id>/<tip>`. An approval counts for the landed tip and for a patch-identical patchset of the same change: after the trunk moves, `git ws-sync` and `git ws-land` (or `git ws-pr` again for a new patchset) land a clean rebase without a new review. Any edit to the diff, even whitespace, needs a new approval. `git ws-ps` lists the patchsets and `git ws-psdiff <a> <b>` shows the interdiff; magpie shows both under `/changes/<change-id>`.
3. Land the tip by merging the review pull request or with `git ws-land`. A merge is a landing request by the merger for the merged head, and the lander reports the result as a `suda/land` status on that head. If it is rejected, the review branches are deleted, so fix the reason and open a new pull request with `git ws-pr`.
