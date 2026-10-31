# GoSuda Monorepo Design Specification

The `gosuda/0` repository consolidates `gosuda` projects into a single content-addressable store (CAS) hosted on GitHub. The control plane resides on `refs/heads/main`, while the unified `google3` source tree resides on `refs/suda/main`. Developers and coding agents operate on isolated workspaces using standard Git commands and automated server-side merge workflows.

## 1. Mounted Projects Explorer (`refs/suda/main`)

The unified source tree on `refs/suda/main` and the linear commit chain on `refs/suda/history` are served through [`magpie.gosuda.org`](https://magpie.gosuda.org). Use the [Diff Link Generator](https://magpie.gosuda.org/diff) to compare active `refs/ws/*` workspaces against `refs/suda/main`. Table 1 lists the root snapshot and mounted projects.

<!-- SUDA_EXPLORER_START -->
| Monorepo Path | Upstream Mirror | Snapshot Tree (`refs/suda/main`) | Commit History (`refs/suda/history`) |
| :--- | :--- | :--- | :--- |
| `//` (Unified Root Tree) | `gosuda/0` (`refs/suda/main`) | [Browse `//`](https://magpie.gosuda.org/tree) | [Commits `//`](https://magpie.gosuda.org/history) |
| `//web/gosuda` | `gosuda/website` | [Browse `//web/gosuda`](https://magpie.gosuda.org/tree/web/gosuda) | [Commits `//web/gosuda`](https://magpie.gosuda.org/history/web/gosuda) |
| `//web/securl` | `gosuda/securl` | [Browse `//web/securl`](https://magpie.gosuda.org/tree/web/securl) | [Commits `//web/securl`](https://magpie.gosuda.org/history/web/securl) |
| `//net/ivnp` | `gosuda/IVNP` | [Browse `//net/ivnp`](https://magpie.gosuda.org/tree/net/ivnp) | [Commits `//net/ivnp`](https://magpie.gosuda.org/history/net/ivnp) |
| `//net/portalite` | `gosuda/portalite` | [Browse `//net/portalite`](https://magpie.gosuda.org/tree/net/portalite) | [Commits `//net/portalite`](https://magpie.gosuda.org/history/net/portalite) |
| `//ipc/hqq/HQQ` | `gosuda/HQQ` | [Browse `//ipc/hqq/HQQ`](https://magpie.gosuda.org/tree/ipc/hqq/HQQ) | [Commits `//ipc/hqq/HQQ`](https://magpie.gosuda.org/history/ipc/hqq/HQQ) |
| `//ipc/hqq/hqq-sqlite3` | `gosuda/hqq-sqlite3` | [Browse `//ipc/hqq/hqq-sqlite3`](https://magpie.gosuda.org/tree/ipc/hqq/hqq-sqlite3) | [Commits `//ipc/hqq/hqq-sqlite3`](https://magpie.gosuda.org/history/ipc/hqq/hqq-sqlite3) |
| `//ipc/hqq/hqq-zstd` | `gosuda/hqq-zstd` | [Browse `//ipc/hqq/hqq-zstd`](https://magpie.gosuda.org/tree/ipc/hqq/hqq-zstd) | [Commits `//ipc/hqq/hqq-zstd`](https://magpie.gosuda.org/history/ipc/hqq/hqq-zstd) |
| `//devtools/azure-magpie` | `gosuda/azure-magpie` | [Browse `//devtools/azure-magpie`](https://magpie.gosuda.org/tree/devtools/azure-magpie) | [Commits `//devtools/azure-magpie`](https://magpie.gosuda.org/history/devtools/azure-magpie) |
| `//net/grt` | `gosuda/GRT` | [Browse `//net/grt`](https://magpie.gosuda.org/tree/net/grt) | [Commits `//net/grt`](https://magpie.gosuda.org/history/net/grt) |
<!-- SUDA_EXPLORER_END -->

## 2. Reference Namespace Map

The repository partitions control metadata, trunk snapshots, commit history, and active workspaces across distinct Git reference namespaces. Table 2 defines the ownership and visibility boundaries for each namespace.

| Reference Namespace | Owned Content | Permitted Writers | Default Clone Size |
| :--- | :--- | :--- | ---: |
| `refs/heads/main` | Control plane (`AGENTS.md`, `README.md`, `.gitconfig`, `capy.star`, `.agents/`, `.github/`, `docs/`) | Control-plane administrators | 24 KiB |
| `refs/suda/main` | Unified `google3` source tree (parentless snapshot commit) | Automated landing workflow (`land.sh`) | 0 KiB (fetched on demand) |
| `refs/suda/history` | Linear parent-linked commit chain sharing root trees with `refs/suda/main` | Automated landing workflow (`land.sh`) | 0 KiB (fetched on demand) |
| `refs/ws/<actor>/<id>` | Isolated work-in-progress workspace commits | Owning developer or coding agent | 0 KiB (ignored by clone) |
| `refs/heads/land/<id>` | Ephemeral merge-queue trigger | Pushed by caller; deleted by `land.sh` | 0 KiB (deleted on merge) |

## 3. Quickstart Procedure

Clone the control plane, attach the trunk reference namespace, and check out the specific domain directories required for your task.

> [!IMPORTANT]
> Always pass `--filter=blob:none` and `--sparse` when cloning or fetching `gosuda/0`. Omitting these flags downloads blobs for every directory in the monorepo.

1. Clone `gosuda/0` with blob filtering and sparse checkout enabled.
2. Include the repository `.gitconfig` file to register workspace aliases.
3. Fetch the trunk references (`refs/suda/*`) using `git suda-init`.
4. Configure sparse checkout for your target domain paths.
5. Create a local workspace branch from `origin/suda/main`.

```bash
git clone --filter=blob:none --sparse ssh://github-lmmt.com/gosuda/0
cd 0
git config include.path ../.gitconfig
git suda-init
git sparse-checkout set ipc/hqq web/gosuda
git ws-new engineer/fix-header
```

## 4. Core Mechanics and Invariants

Every change in `gosuda/0` obeys five structural invariants:

1. **Control-Plane Isolation**: `refs/heads/main` never contains project source directories (`web/`, `net/`, `ipc/`, `storage/`, `crypto/`, `ai/`, `devtools/`, `corp/`, `third_party/`, `experimental/`).
2. **Dual-Pointer Trunk**: Every landing updates `refs/suda/main` (an orphan commit for depth-1 fetches) and `refs/suda/history` (a parent-linked commit for `git blame`). Both commits reference the identical root tree SHA-1 hash.
3. **Four-Tier Visibility**: Public production domains (`//web/*`, `//net/*`, `//ipc/*`, `//storage/*`, `//crypto/*`, `//ai/*`, `//devtools/*`) never import `//corp/*` or `//experimental/*`. Vendor directories (`//third_party/*`) remain self-contained, `//corp/*` never imports `//experimental/*`, and `//experimental/<actor>/*` never imports `//corp/*`.
4. **Hierarchical `OWNERS` and Experimental Sandbox**: `capy check-owners` evaluates `OWNERS` files from the modified file directory up to `//OWNERS`, honoring `set noparent` and `per-file <glob>=...` rules against the commit author and `Reviewed-by:` trailers. Existing `OWNERS` files are read from `refs/suda/main` (`baseTree`) so a workspace commit cannot self-approve by modifying `OWNERS`. Paths under `//experimental/<actor>/*` bypass `OWNERS` review for rapid prototyping.
5. **In-Memory Atomic Landing**: Pushing to `refs/heads/land/<id>` and invoking `git ws-land` triggers `.github/scripts/land.sh` on `gosuda/azure-magpie`. The script merges trees via `git merge-tree --write-tree`, enforces `check-visibility` and `check-owners`, updates `refs/suda/main` and `refs/suda/history` atomically, exports modified public subtrees via `capy`, and deletes `refs/heads/land/<id>`.

## 5. Contribution, Review, and Experimental Workflows

1. **Direct Workspace Review and Landing (`refs/ws/*`)**:
   - Save work-in-progress to `refs/ws/<actor>/<id>` using `git ws-save`.
   - Inspect two-dot diffs and trigger one-click landing at [`https://magpie.gosuda.org/diff`](https://magpie.gosuda.org/diff), or run `git ws-land` from the terminal.
2. **Ephemeral GitHub Pull Request Review (`git ws-pr`)**:
   - Run `git ws-pr` to push the workspace base snapshot to `refs/heads/base/<id>` and the workspace tip to `refs/heads/review/<id>`, opening a GitHub Pull Request between the two branches.
   - Merging the PR on GitHub (or clicking **Land** on `magpie.gosuda.org`) dispatches `land.sh` and deletes both ephemeral PR branches.
3. **Experimental Sandbox and Graduation (`git ws-exp`, `git ws-graduate`)**:
   - Run `git ws-exp <actor> <project>` to create a sandboxed workspace under `//experimental/<actor>/<project>`.
   - Run `git ws-graduate experimental/<actor>/<project> <domain>/<project>` to promote an experimental package into a production domain with full `OWNERS` and visibility verification.
4. **External Standalone Mirror Process (`gosuda/<repo>`)**:
   - **Outbound Export (`land.sh`)**: Landing a change on `refs/suda/main` automatically exports modified project subtrees to `gosuda/<repo>` with `GitOrigin-RevId` provenance while preserving standalone `.github/workflows/`.
   - **Inbound PR Staging (`sync-inbound.sh ws`)**: Opening or updating a pull request on `gosuda/<repo>` imports the external PR tree into `refs/ws/capy/<repo>-pr-<num>` on `gosuda/0` for cross-monorepo inspection.
   - **Inbound Commit Landing (`sync-inbound.sh land`)**: Merging a PR or pushing a commit to `main` on `gosuda/<repo>` splices the updated subtree into `refs/suda/main` and advances `refs/suda/history` atomically.

## 6. Specification Index

Each specification document in `docs/` covers one subsystem boundary.

| Document | Scope Description |
| :--- | :--- |
| [architecture.md](./architecture.md) | Reference namespaces, `google3` directory taxonomy, visibility boundaries, and the in-memory `git merge-tree` landing algorithm. |
| [agents.md](./agents.md) | Pure-Git operating procedures for coding agents, `AGENTS.md` specification, `.gitconfig` aliases, and pre-execution safety hooks. |
| [tooling.md](./tooling.md) | Server-side scripts (`land.sh`, `sync-inbound.sh`), GitHub App configuration, and `//devtools/azure-magpie` routes. |
| [google3-and-copybara.md](./google3-and-copybara.md) | Organization audit across 125 repositories, `google3` parity matrix, two-tier CI execution, and the Starlark subtree synchronizer (`capy`). |
| [feasibility-review.md](./feasibility-review.md) | Empirical verification results on Git 2.54.0, corrected plumbing invocations, and GitHub platform constraints. |
| [hardening-plan.md](./hardening-plan.md) | Proposed migration to the two-repository topology (`gosuda/0` trunk, `gosuda/1` user repository), the self-hosted lander, and phased exit criteria for the 2026-10-30 review findings. |

## 7. Build and Verification Procedure

Run the verification suite for the `capy` Starlark synchronization engine before submitting control-plane changes:

```bash
go -C .github/scripts/capy test -v ./...
go -C .github/scripts/capy run ./cmd/capy list -config ../../../capy.star
```
