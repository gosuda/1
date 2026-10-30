---
name: suda-import-repo
description: >-
  Imports an existing GitHub repository or an upstream pull request into the
  gosuda/0 monorepo under its google3 domain path, registers it in capy.star,
  and updates README.md explorer links. Activate when importing a repository
  or pulling external mirror commits into refs/suda/main or refs/ws/capy/*.
---

# Repository and Pull Request Import Procedure

This skill defines how to import an existing repository into `refs/suda/main` and how to pull inbound commits or pull requests from a standalone mirror repository.

## 1. Importing a Repository into `refs/suda/main`

Mount an existing GitHub repository at its target `google3` domain path (`web/`, `net/`, `ipc/`, `storage/`, `crypto/`, `ai/`, `devtools/`, `corp/`, `third_party/`) using `.github/scripts/import-repo.sh`.

> [!IMPORTANT]
> Verify that public domains (`web/`, `net/`, `ipc/`, `storage/`, `crypto/`, `ai/`, `devtools/`) do not import `corp/` or `experimental/` packages. The import script runs `capy check-visibility` and aborts if a boundary violation exists.

1. Choose the target `google3` path according to the repository domain and visibility tier.
2. Run `.github/scripts/import-repo.sh` with the target prefix, `owner/repo`, and source branch.
3. Verify that `capy list` includes the newly imported workflow.
4. Commit and push the updated `capy.star` and `README.md` files to `refs/heads/main`.

```bash
./.github/scripts/import-repo.sh web/flats gosuda/flats main
go -C .github/scripts/capy run ./cmd/capy list -config ../../../capy.star
git add capy.star README.md
git commit -m "chore(capy): register web/flats mirror workflow"
git push origin main
```

## 2. Importing an Inbound Pull Request or Branch (`sync-inbound.sh`)

Splice a branch or pull request from a standalone mirror repository into `refs/suda/main` and push it to `refs/ws/capy/*` or `refs/heads/land/*`.

1. Run `.github/scripts/sync-inbound.sh` with the workflow name, external reference (`refs/pull/<N>/head` or `refs/heads/main`), and target mode (`ws` or `land`).
2. Inspect the resulting workspace or landing queue status on `origin`.

```bash
# Import PR #42 from gosuda/HQQ into refs/ws/capy/HQQ-pull-42-head
./.github/scripts/sync-inbound.sh HQQ refs/pull/42/head ws

# Import and immediately enqueue external main branch for landing
./.github/scripts/sync-inbound.sh HQQ refs/heads/main land
```
