# DO NOT PUSH TO THIS REPOSITORY

**Browse the current monorepo tree: [magpie.gosuda.org/head](https://magpie.gosuda.org/head)** (read-only view at the head of `refs/suda/history`)

> [!CAUTION]
> **`gosuda/0` is the internal monorepo source of truth. Members never write here.**
> Every ref in this repository is written by the **lander** (a single automated service). A push from
> anyone else is either rejected or detected, and an unexpected `refs/suda/*` update **halts the landing
> queue for the whole organization** until an org owner investigates.

| Do | Do not |
| :--- | :--- |
| Read and clone this repository (`--filter=blob:none --sparse`) | Push **anything** to `gosuda/0`: branches, tags, `refs/suda/*`, `refs/ws/*` |
| Push workspaces and landing requests to **[`gosuda/1`](https://github.com/gosuda/1)** | Force-push, delete refs, or open pull requests here |
| Follow the onboarding guide in **[`gosuda/1`](https://github.com/gosuda/1#readme)** | Run `git pull` or `git push origin` from a workspace |

**Want to contribute?** Go to **[`gosuda/1`](https://github.com/gosuda/1)**. Everything a contributor needs (setup, workspace flow, review, landing, new projects) is documented there.

## What this repository is

`gosuda/0` is the system of record for the `gosuda` monorepo:

- `refs/heads/main`: the control plane (`AGENTS.md`, `.gitconfig`, `capy.star`, `.agents/`, `.github/`, `docs/`).
- `refs/suda/main` and `refs/suda/history`: the unified source tree (a parentless snapshot) and its linear history.
- `refs/suda/sync/<workflow>`: per-mirror synchronization baselines.
- `refs/heads/lander`: the deployable lander (Dockerfile, `lander/`, `capy/`), published by the lander itself.

Only the `suda-lander` GitHub App writes these refs. Org owners may use a documented break-glass push to
`refs/heads/main`; the lander detects it and synchronizes the trunk overlay.

## Head Links

- **Current Tree (head of `refs/suda/history`)**: [https://magpie.gosuda.org/head](https://magpie.gosuda.org/head)
- **Unified Source Tree (`refs/suda/main`)**: [https://magpie.gosuda.org/tree](https://magpie.gosuda.org/tree)
- **Linear Commit History (`refs/suda/history`)**: [https://magpie.gosuda.org/history](https://magpie.gosuda.org/history)
- **Diff Link Generator**: [https://magpie.gosuda.org/diff](https://magpie.gosuda.org/diff)

## Documentation

- [Design Specification and Mounted Projects (`docs/README.md`)](./docs/README.md)
- [Hardening plan and lander migration (`docs/hardening-plan.md`)](./docs/hardening-plan.md)
- [Agent Operating Rules (`AGENTS.md`)](./AGENTS.md)
- Lander service: [`.github/scripts/lander/README.md`](./.github/scripts/lander/README.md)
