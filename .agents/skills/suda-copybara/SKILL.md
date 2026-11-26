---
name: suda-copybara
description: >-
  Registers mirror repositories in capy.star (including a non-default branch),
  validates google3 domain visibility boundaries, and checks subtree exports and
  imports with the capy engine. Activate when onboarding a repository into
  gosuda/0, changing a mirror mapping, or diagnosing a mirror that does not
  converge with the trunk.
---

# Subtree Synchronization Procedure (`capy`)

This skill defines how to register a mirror in `capy.star`, verify `google3` import boundaries, and check the subtree that the lander exports to and imports from a standalone GitHub repository. The lander reads `capy.star` from `refs/heads/main` on every cycle, so a registration takes effect when it lands.

## 1. Registering a Mirror in `capy.star`

Add the mapping to the root `capy.star` on the control plane. Each entry is a tuple:

```python
PILOT_REPOS = [
    ("website", "web/gosuda", "gosuda/website", False),
    ("portalite", "net/portalite", "gosuda/portalite", False, "master"),
]
```

- `name`: the workflow name, used in staged ref names (`refs/ws/capy/<name>-main`, `refs/ws/capy/<name>-pr-<N>`) and in `refs/suda/sync/<name>`.
- `monorepo prefix`: the `google3` path to export.
- `mirror repository`: `owner/repo` of the public mirror.
- `strip_replaces`: `True` adds the `gomod.strip_local_replaces()` transform. A workflow with a content transform is export-only (inbound changes are always staged).
- `branch` (optional fifth element): the mirror branch. It defaults to `main`. Set it only when the mirror's default branch differs, as `portalite` does with `master`.

Entries are evaluated by the loop in `capy.star`. Do not change the loop when you add an entry.

Procedure:

1. Append the tuple to `capy.star`. Use the fifth element only for a non-`main` branch.
2. Confirm that the Starlark evaluates and lists the workflow:

   ```bash
   go -C .github/scripts/capy run ./cmd/capy list -config ../../../capy.star
   ```

3. Run the `capy` test suite:

   ```bash
   go -C .github/scripts/capy test -v ./...
   ```

4. Submit the change as a workspace landing (see [suda-workspace](../suda-workspace/SKILL.md)). `capy.star` is a control-plane path, so the change also updates `refs/heads/main`.

## 2. Verifying Visibility and Computing the Export

Validate import directionality and compute the standalone root tree for a registered workflow.

1. Run `capy check-visibility` against the target root tree.
2. Run `capy export-tree` to compute the standalone tree for the workflow.

```bash
TREE=$(git rev-parse refs/remotes/origin/suda/main^{tree})
go -C .github/scripts/capy run ./cmd/capy check-visibility -repo ../../.. -tree "$TREE"
go -C .github/scripts/capy run ./cmd/capy export-tree -repo ../../.. -config ../../../capy.star -workflow HQQ -tree "$TREE"
```

`export-tree` applies the workflow's exclusions and transforms. Its output is the tree the lander compares with the mirror.

## 3. Checking Convergence

The lander keeps a mirror and its prefix in agreement on every cycle. To find out why a mirror does not converge:

1. Read the lander log for the workflow name. The messages to look for are `reconcile failed`, `mirror diverged and no sync baseline exists; manual baseline required`, `workflow is export-only (content transforms); mirror changes need manual import`, and `exported to mirror`.
2. Compare the trunk export with the mirror tip. Paths that match the workflow's exclusions are ignored by the lander, so a difference limited to those paths is expected:

   ```bash
   TREE=$(git rev-parse refs/remotes/origin/suda/main^{tree})
   EXPORT=$(go -C .github/scripts/capy run ./cmd/capy export-tree -repo ../../.. -config ../../../capy.star -workflow HQQ -tree "$TREE")
   git fetch --no-tags https://github.com/gosuda/HQQ refs/heads/main
   git diff-tree -r --name-status "$EXPORT" "FETCH_HEAD^{tree}"
   ```

3. A mirror change that the trunk lacks is staged as `refs/ws/capy/<name>-main` on `gosuda/1`. Land it with [suda-import-repo](../suda-import-repo/SKILL.md) §2.

## 4. Imports

Do not use `capy import-tree` to land a mirror change. It replaces the whole prefix with the mirror tree and does not keep export-excluded paths. The lander's inbound path keeps them and merges with the trunk, so an import through the lander is the only supported way to land a mirror change.
