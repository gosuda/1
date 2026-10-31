# gosuda/0

`gosuda/0` is the private system of record for the `gosuda` monorepo. `refs/heads/main` holds the control plane, and the unified `google3` source tree lives on `refs/suda/main`. Only the `suda-lander` GitHub App writes to this repository. Members do their work in `gosuda/1`, a second private repository, and submit landing requests there. A self-hosted lander, a single process on a dedicated server, lands those requests into `gosuda/0` and keeps the public mirrors in sync with the trunk.

Table 1 lists the repositories and their write authority.

| Repository | Visibility | Content | Writers |
| :--- | :--- | :--- | :--- |
| `gosuda/0` | Private | Control plane, trunk, sync baselines, lander deployment branch | `suda-lander` App; org owners for break-glass only |
| `gosuda/1` | Private | Workspaces, landing requests, review pull requests, staged external changes, trunk copy | Organization members; `suda-lander` App |
| `gosuda/<mirror>` | Public | Standalone exports of the projects listed in `capy.star` | `suda-lander` App; mirror maintainers for external pull requests |

Clients fetch the control plane and the trunk from `gosuda/0`, which is `origin` and is read-only for members. They push workspaces and landing requests to `gosuda/1`, which is the `ws` remote. Never fetch the control plane or the trunk from `gosuda/1`.

## Head Links

- **Unified Source Tree (`refs/suda/main`)**: [https://magpie.gosuda.org/tree](https://magpie.gosuda.org/tree)
- **Linear Commit History (`refs/suda/history`)**: [https://magpie.gosuda.org/history](https://magpie.gosuda.org/history)
- **Diff Link Generator & Workspace Studio**: [https://magpie.gosuda.org/diff](https://magpie.gosuda.org/diff)

## Quickstart

```bash
git clone --filter=blob:none --sparse ssh://github-lmmt.com/gosuda/0
cd 0
git config include.path ../.gitconfig
git suda-init
git sparse-checkout set ipc/hqq web/gosuda
git ws-new engineer/fix-header
```

`git suda-init` adds the `ws` remote, disables pushes to `origin`, and fetches `refs/suda/*` from `origin`. [docs/README.md](./docs/README.md) describes the full procedure.

## Contribution & Review Process

- **Direct Workspace (`refs/ws/<actor>/<id>` on `gosuda/1`)**: Create a workspace with `git ws-new`, save work-in-progress with `git ws-save`, inspect the two-dot diff at [`magpie.gosuda.org/diff`](https://magpie.gosuda.org/diff), and submit it with `git ws-land`. The lander lands or rejects the request and reports the result as a `suda/land` commit status on the tip in `gosuda/1`.
- **GitHub Pull Request Review (`git ws-pr`)**: `git ws-pr` pushes `refs/heads/base/<id>` and `refs/heads/review/<id>` to `gosuda/1` and opens a pull request there for discussion and approval. Merging that pull request lands nothing. Run `git ws-land` to land, and the lander deletes the pair after the change lands.
- **Hierarchical `OWNERS` & Experimental Sandbox (`git ws-exp`, `git ws-graduate`)**: The lander evaluates `OWNERS` files read from the trunk and resolves every import to enforce import visibility. A path under `experimental/<actor>/` is approved for the requester whose GitHub login equals `<actor>`.
- **External Mirror Pull Requests and Pushes (`gosuda/<repo>`)**: The lander stages each open mirror pull request as `refs/ws/capy/<wf>-pr-<N>` on `gosuda/1`. An inbound push to a mirror branch lands automatically only when the pusher owns the prefix; otherwise it is staged as `refs/ws/capy/<wf>-main`. A member lands a staged change with `git ws-land`.

## Documentation

- [Design Specification, Mounted Projects, and Quickstart (`docs/README.md`)](./docs/README.md)
- [Agent Operating Rules (`AGENTS.md`)](./AGENTS.md)
