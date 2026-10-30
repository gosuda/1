---
name: suda-copybara
description: >-
  Registers new repositories in capy.star, validates google3 domain visibility
  boundaries, and exports or imports subtrees using the capy Starlark engine.
  Activate when onboarding a repository into gosuda/0 or synchronizing mirrors.
---

# Subtree Synchronization Procedure (`capy`)

This skill defines how to register a project in `capy.star`, verify `google3` import boundaries, and synchronize subtrees with standalone GitHub repositories.

## 1. Registering a Repository in `capy.star`

Add the repository mapping to the root `capy.star` file on `refs/heads/main` and verify the configuration with the `capy` CLI.

1. Append the `(name, monorepo_prefix, github_repo, strip_replaces)` tuple to `capy.star`.
2. Run `capy list` to verify that Starlark evaluation succeeds.
3. Run the `capy` test suite to confirm all transformation invariants pass.

```bash
go -C .github/scripts/capy run ./cmd/capy list -config ../../../capy.star
go -C .github/scripts/capy test -v ./...
```

## 2. Verifying Visibility and Exporting a Subtree

Validate import directionality and compute the transformed standalone root tree for a registered workflow.

1. Run `capy check-visibility` against the target root tree SHA-1.
2. Run `capy export-tree` to produce the standalone root tree SHA-1 for the target workflow.

```bash
TREE=$(git rev-parse refs/remotes/origin/suda/main^{tree})
go -C .github/scripts/capy run ./cmd/capy check-visibility -repo ../../.. -tree "$TREE"
go -C .github/scripts/capy run ./cmd/capy export-tree -repo ../../.. -config ../../../capy.star -workflow HQQ -tree "$TREE"
```
