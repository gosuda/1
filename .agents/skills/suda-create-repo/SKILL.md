---
name: suda-create-repo
description: >-
  Scaffolds a new project inside the gosuda/0 google3 directory hierarchy,
  registers its mirror mapping in capy.star and README.md, and optionally
  creates the standalone mirror repository on GitHub. Activate when creating
  a new project, library, or service in the monorepo.
---

# Project and Mirror Creation Procedure

This skill defines how to initialize a new project inside `refs/suda/main` and optionally create its standalone mirror repository on GitHub.

## 1. Scaffolding a Project in `refs/suda/main`

Create the initial `README.md`, `OWNERS`, and `go.mod` files inside `refs/suda/main` at the target `google3` prefix using `.github/scripts/create-repo.sh`.

> [!CAUTION]
> Place private internal services strictly under `corp/<name>` and set visibility to `private`. Placing internal services under public domain prefixes exposes them to outbound mirror synchronization.

1. Select the `google3` prefix (`<domain>/<name>`), target GitHub repository (`gosuda/<name>`), Go module path, and visibility (`public` or `private`).
2. Set `CREATE_GITHUB_MIRROR=1` if the standalone GitHub repository should be created and populated immediately via `gh repo create`.
3. Execute `.github/scripts/create-repo.sh`.
4. Commit and push the updated `capy.star` and `README.md` files on `refs/heads/main`.

```bash
CREATE_GITHUB_MIRROR=1 ./.github/scripts/create-repo.sh net/relaysrv gosuda/relaysrv gosuda.org/relaysrv public
git add capy.star README.md
git commit -m "feat(capy): register net/relaysrv project"
git push origin main
```

## 2. Checking Out the Newly Created Project Locally

Fetch the updated `refs/suda/main` snapshot and add the new project path to your sparse checkout.

1. Fetch the updated trunk snapshot via `git suda-init`.
2. Add the new domain path to sparse checkout.
3. Create a workspace branch to begin editing the new project.

```bash
git suda-init
git sparse-checkout add net/relaysrv
git ws-new agent-dalgona/init-relaysrv
```
