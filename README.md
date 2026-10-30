# gosuda/0

`gosuda/0` is the central content-addressable store (CAS) monorepo for `gosuda` projects. `refs/heads/main` stores only the control plane, while the unified `google3` source tree lives on `refs/suda/main`.

## Head Links

- **Unified Source Tree (`refs/suda/main`)**: [https://magpie.gosuda.org/tree](https://magpie.gosuda.org/tree)
- **Linear Commit History (`refs/suda/history`)**: [https://magpie.gosuda.org/history](https://magpie.gosuda.org/history)
- **Diff Link Generator & Workspace Studio**: [https://magpie.gosuda.org/diff](https://magpie.gosuda.org/diff)

## Contribution & Review Process

- **Direct Workspace (`refs/ws/<actor>/<id>`)**: Create a workspace with `git ws-new`, push WIP with `git ws-save`, inspect two-dot diffs at [`magpie.gosuda.org/diff`](https://magpie.gosuda.org/diff), and land via `git ws-land` or the web **Land** button.
- **GitHub Pull Request Review (`git ws-pr`)**: Push ephemeral `refs/heads/base/<id>` and `refs/heads/review/<id>` branches to open a line-by-line GitHub PR on `gosuda/0`. Merging the PR automatically lands the change into `refs/suda/main` and deletes both temporary branches.
- **Hierarchical `OWNERS` & Experimental Sandbox (`git ws-exp`, `git ws-graduate`)**: `land.sh` enforces `google3` `OWNERS` files (`set noparent`, `per-file`, `Reviewed-by:` trailers) and 4-tier import visibility. Prototyping under `//experimental/<actor>/*` bypasses `OWNERS` review until graduated.
- **External Mirror Pull Requests (`gosuda/<repo>`)**: Pull requests opened on standalone mirror repositories are imported into `refs/ws/capy/<repo>-pr-<num>`, and commits merged on mirror `main` branches are spliced back into `refs/suda/main` by `sync-inbound.sh`.

## Documentation

- [Design Specification, Mounted Projects, and Quickstart (`docs/README.md`)](./docs/README.md)
- [Agent Operating Rules (`AGENTS.md`)](./AGENTS.md)
