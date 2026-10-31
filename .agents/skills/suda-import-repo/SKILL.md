---
name: suda-import-repo
description: >-
  Imports an existing gosuda GitHub repository into the gosuda/0 monorepo under
  its google3 domain path, registers it in capy.star, and lands the mirror
  registration. Also lands staged inbound mirror changes and pull requests from
  refs/ws/capy/* on gosuda/1. Activate when importing a repository or when a
  mirror change is staged for review.
---

# Repository and Pull Request Import Procedure

This skill defines how to import an existing repository into `refs/suda/main` and how to land mirror changes that the lander staged for review.

## 1. Importing a Repository

Mount an existing repository at its target `google3` domain path (`web/`, `net/`, `ipc/`, `storage/`, `crypto/`, `ai/`, `devtools/`, `corp/`, or `third_party/`). The repository must be one the `suda-lander` App can write to, because the lander exports to it after the import. In practice that means a repository in the `gosuda` organization.

> [!IMPORTANT]
> Public domains (`web/`, `net/`, `ipc/`, `storage/`, `crypto/`, `ai/`, `devtools/`) must not import `corp/` or `experimental/` packages. The lander's visibility check rejects a landing that does.

1. Choose the target path according to the repository domain and visibility tier.
2. Run `.github/scripts/import-repo.sh <prefix> <owner/repo> [branch]` from a workspace branch created with `git ws-new`. The branch defaults to `main`. The helper creates one workspace commit with the imported tree at the prefix, the `capy.star` entry, and the `docs/README.md` explorer row. It prints the next steps.
3. Confirm that the workflow is registered:

   ```bash
   go -C .github/scripts/capy run ./cmd/capy list -config ../../../capy.star
   ```

4. Save and land the commit:

   ```bash
   git ws-save
   git ws-sync
   git ws-land
   ```

5. Check the `suda/land` status as described in [suda-workspace](../suda-workspace/SKILL.md) §3.

```bash
git ws-new agent-dalgona/import-flats
./.github/scripts/import-repo.sh web/flats gosuda/flats main
git ws-save
git ws-sync
git ws-land
```

On its next reconcile, the lander compares the mirror with the export of the trunk. When the two trees are equal, it records the sync baseline for the workflow. When they differ and no baseline exists, it logs an error and leaves the mirror unchanged until the trees agree (see [architecture.md](../../../docs/architecture.md) §4.1).

## 2. Landing a Staged Inbound Change

The lander stages mirror changes on `gosuda/1` and never lands them automatically. Two kinds are staged:

- `refs/ws/capy/<wf>-pr-<N>`: an open pull request on the mirror, imported for review.
- `refs/ws/capy/<wf>-main`: a push to the mirror branch that you must land, either because the pusher does not own the prefix or because the change conflicts with the trunk.

Review the staged diff against its own root, which is the baseline. Then land it as a member:

```bash
git fetch ws
git switch -c ws/agent-dalgona/capy-HQQ-pr-42 refs/remotes/ws/ws/capy/HQQ-pr-42
git ws-land
```

You become the requester, so the `OWNERS` rules apply to you. Check the result as described in [suda-workspace](../suda-workspace/SKILL.md) §3.

If the lander rejects the staged change with `unknown base tree`, run `git ws-sync` on the staged branch. If it reports a merge conflict, follow [suda-resolve-conflicts](../suda-resolve-conflicts/SKILL.md). A mirror whose change is staged does not export until that change is incorporated, so landing the staged change is the step that unblocks the mirror.
