---
name: suda-create-repo
description: >-
  Scaffolds a new project inside the gosuda/0 google3 directory hierarchy,
  registers its mirror mapping in capy.star and docs/README.md through a
  workspace commit, and lands it through the lander. Activate when creating a
  new project, library, or service in the monorepo.
---

# Project and Mirror Creation Procedure

This skill defines how to create a project inside `refs/suda/main` and register its public mirror. The helper script `.github/scripts/create-repo.sh` builds the change locally as one workspace commit. The lander lands it, the same way as any other change.

## 1. Choosing the Prefix and the Mirror

> [!CAUTION]
> Place private internal services strictly under `corp/<name>`. Only the public tiers (`web/`, `net/`, `ipc/`, `storage/`, `crypto/`, `ai/`, `devtools/`) are exported to public mirrors. A `corp/` project is never exported.

1. Select the `google3` prefix (`<domain>/<name>`) that matches the visibility tier.
2. Select the target mirror repository (`gosuda/<name>`) for a public project.
3. Select the Go module path (for example `gosuda.org/<name>`).
4. Ask an org owner to create the public mirror repository `gosuda/<name>` before the first export. Until it exists, the lander cannot export to it and logs a fetch failure for that mirror on every cycle.

## 2. Building the Workspace Commit

Run the helper from a workspace branch created with `git ws-new`. The helper creates the project subtree, the `capy.star` entry, and the `docs/README.md` explorer row in one commit. It prints the `git ws-save` and `git ws-land` commands to run next.

```bash
git ws-new agent-dalgona/init-relaysrv
./.github/scripts/create-repo.sh net/relaysrv gosuda/relaysrv gosuda.org/relaysrv
```

Then submit it:

```bash
git ws-save
git ws-sync
git ws-land
```

Check the result as described in [suda-workspace](../suda-workspace/SKILL.md) §3. Once it has landed, the new workflow appears in `capy list`.

```bash
go -C .github/scripts/capy run ./cmd/capy list -config ../../../capy.star
```

## 3. Checking Out the Newly Created Project

After the landing succeeds, fetch the updated trunk and add the new path to your sparse checkout.

```bash
git suda-init
git sparse-checkout add net/relaysrv
git ws-new agent-dalgona/work-relaysrv
```
