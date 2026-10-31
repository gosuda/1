# GoSuda Monorepo Design Specification

The `gosuda/0` repository consolidates `gosuda` projects into a single content-addressable store (CAS). The control plane resides on `refs/heads/main`, and the unified `google3` source tree resides on `refs/suda/main`. Members work in the user repository `gosuda/1`, and the `suda-lander` service is the only writer of `gosuda/0`. Developers and coding agents operate on isolated workspaces with standard Git commands, and the lander lands their requests after identity, `OWNERS`, visibility, and presubmit checks.

## 1. Mounted Projects Explorer (`refs/suda/main`)

The unified source tree on `refs/suda/main` and the linear commit chain on `refs/suda/history` are served through [`magpie.gosuda.org`](https://magpie.gosuda.org). Use the [Diff Link Generator](https://magpie.gosuda.org/diff) to compare active `refs/ws/*` workspaces in `gosuda/1` against `refs/suda/main`. Table 1 lists the root snapshot and mounted projects.

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

The topology partitions control metadata, trunk snapshots, commit history, and active workspaces across two repositories and distinct Git reference namespaces. Table 2 defines the owner, the permitted writers, and whether clients fetch each namespace.

| Repository | Reference Namespace | Owned Content | Permitted Writers | Fetched by Clients |
| :--- | :--- | :--- | :--- | :--- |
| `gosuda/0` | `refs/heads/main` | Control plane (`AGENTS.md`, `README.md`, `.gitconfig`, `capy.star`, `.agents/`, `.github/`, `docs/`) | `suda-lander` (overlay landings and overlay sync); org owners (break-glass) | Yes, from `origin` |
| `gosuda/0` | `refs/suda/main` | Unified `google3` source tree (parentless snapshot commit) | `suda-lander` | Yes, from `origin`, on demand |
| `gosuda/0` | `refs/suda/history` | Linear, signed commit chain sharing root trees with `refs/suda/main` | `suda-lander` | Yes, from `origin`, on demand |
| `gosuda/0` | `refs/suda/sync/<wf>`, `refs/heads/lander` | Mirror sync baselines; deployable lander tree | `suda-lander` | No |
| `gosuda/1` | `refs/ws/<actor>/<id>` | Isolated work-in-progress workspace commits | Owning member | Pushed by `git ws-save` and `git ws-pr` |
| `gosuda/1` | `refs/heads/land/<id>` | Landing request | Owning member; deleted by the lander | Read by the lander |
| `gosuda/1` | `refs/heads/base/<id>`, `refs/heads/review/<id>` | Review pull request pair | Owning member (`git ws-pr`); deleted by the lander | Pushed by `git ws-pr` |
| `gosuda/1` | `refs/ws/capy/<wf>-main`, `refs/ws/capy/<wf>-pr-<N>` | Staged external changes | `suda-lander` | Yes, with `git fetch ws` |
| `gosuda/1` | `refs/suda/main`, `refs/suda/history` | Copies of the `gosuda/0` trunk refs (push negotiation base only) | `suda-lander` | Never |

## 3. Quickstart Procedure

Contributors: the step-by-step guide lives in `gosuda/1` ([README](https://github.com/gosuda/1#readme), [onboarding guide](https://github.com/gosuda/1/blob/main/docs/onboarding.md)); it is generated from `docs/user-repo/` in this repository. `.github/scripts/onboard.sh` performs the setup below in one command (`gh api -H 'Accept: application/vnd.github.raw' repos/gosuda/0/contents/.github/scripts/onboard.sh | bash -s -- -d gosuda-0 <dirs>`). The manual steps follow.

Clone the control plane from `gosuda/0`, attach the trunk namespace and the `ws` remote, and check out the domain directories required for your task.

> [!IMPORTANT]
> Always pass `--filter=blob:none` and `--sparse` when cloning or fetching `gosuda/0`. Omitting these flags downloads blobs for every directory in the monorepo.

1. Clone `gosuda/0` with blob filtering and sparse checkout enabled.
2. Include the repository `.gitconfig` file to register workspace aliases.
3. Run `git suda-init`. It registers the `ws` remote (`gosuda/1`), disables pushes to `origin`, fetches `refs/suda/*` from `origin`, and includes the aliases.
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

Every change in `gosuda/0` obeys six structural invariants:

1. **Control-Plane Isolation**: `refs/heads/main` never contains project source directories (`web/`, `net/`, `ipc/`, `storage/`, `crypto/`, `ai/`, `devtools/`, `corp/`, `third_party/`, `experimental/`).
2. **Overlay Invariant**: Every top-level entry of `refs/heads/main` appears unchanged in `refs/suda/main`. A landing that edits a control-plane path advances `refs/heads/main` in the same atomic push as the trunk.
3. **Dual-Pointer Trunk**: Every landing advances `refs/suda/main` (a parentless snapshot) and `refs/suda/history` (a parent-linked commit). Both commits reference the identical root tree SHA-1 hash.
4. **Four-Tier Visibility**: Public production domains (`//web/*`, `//net/*`, `//ipc/*`, `//storage/*`, `//crypto/*`, `//ai/*`, `//devtools/*`) never import `//corp/*` or `//experimental/*`. Vendor directories (`//third_party/*`) remain self-contained, `//corp/*` never imports `//experimental/*`, and `//experimental/<actor>/*` never imports `//corp/*`. The lander resolves imports to repository paths to enforce these rules.
5. **Hierarchical `OWNERS` and Authenticated Identity**: `capy check-owners` reads `OWNERS` files from the base trunk tree. Identities come only from GitHub: the pusher reported by the repository activity API, approvals bound to the exact tip, and organization team membership. Commit author and trailers never grant approval. A path under `//experimental/<actor>/*` is approved for a requester whose login is `<actor>`, without an `OWNERS` entry. Any other requester needs the `OWNERS` chain.
6. **In-Memory Atomic Landing**: The lander computes the merged tree with `git merge-tree --write-tree`, runs the visibility, `OWNERS`, and presubmit checks, and publishes the result in one `--atomic` push. Every updated reference has a lease, so a trunk ref that moved after evaluation makes the push fail instead of being overwritten.

## 5. Contribution, Review, and Experimental Workflows

1. **Direct Workspace Review and Landing (`refs/ws/*`)**:
   - Save work-in-progress to `refs/ws/<actor>/<id>` on `gosuda/1` with `git ws-save`.
   - Inspect two-dot diffs at [`https://magpie.gosuda.org/diff`](https://magpie.gosuda.org/diff) and submit with `git ws-land`.
2. **Ephemeral GitHub Pull Request Review (`git ws-pr`)**:
   - Run `git ws-pr` to push the workspace base commit to `refs/heads/base/<id>` and the workspace tip to `refs/heads/review/<id>` on `gosuda/1`, which opens a pull request between the two branches.
   - Approvals count only for the landed tip. Land with `git ws-land`; merging the pull request does not land anything.
3. **Experimental Sandbox and Graduation (`git ws-exp`, `git ws-graduate`)**:
   - Run `git ws-exp <actor> <project>` to create a sandboxed workspace under `//experimental/<actor>/<project>`. `<actor>` must be your GitHub login.
   - Run `git ws-graduate experimental/<actor>/<project> <domain>/<project>` to promote a package into a production domain. The destination needs `OWNERS` approval.
4. **External Standalone Mirror Process (`gosuda/<repo>`)**:
   - **Outbound export**: A landing that changes a mirrored prefix is exported to `gosuda/<repo>` on top of the mirror's tip, with `GitOrigin-RevId` provenance. Excluded paths stay out of the mirror.
   - **Inbound pull request staging**: Each open pull request on `gosuda/<repo>` is staged on `gosuda/1` as `refs/ws/capy/<wf>-pr-<N>` for review and landing by a member.
   - **Inbound push handling**: A push to a mirror branch lands automatically only when the pusher owns the prefix. Otherwise it is staged as `refs/ws/capy/<wf>-main`.

## 6. Specification Index

Table 3 lists each specification document in `docs/` and the subsystem boundary it covers.

| Document | Scope Description |
| :--- | :--- |
| [architecture.md](./architecture.md) | Repository topology, reference namespaces, `google3` directory taxonomy, visibility boundaries, the lander process, and the landing and mirror reconciliation algorithms. |
| [agents.md](./agents.md) | Pure-Git operating procedures for coding agents, the `AGENTS.md` specification, `.gitconfig` aliases, and the pre-execution safety hook. |
| [tooling.md](./tooling.md) | The lander's deployment and configuration, presubmit sandbox, the `capy` command set, the `suda-lander` App, and the `//devtools/azure-magpie` routes. |
| [google3-and-copybara.md](./google3-and-copybara.md) | Organization audit, `google3` parity matrix, two-tier CI execution, and the Starlark subtree synchronizer (`capy`). |
| [feasibility-review.md](./feasibility-review.md) | Empirical verification results on Git 2.54.0, plumbing corrections, and GitHub platform constraints. |
| [hardening-plan.md](./hardening-plan.md) | Trunk hardening and lander migration plan: the findings, decisions, and phased migration that produced this topology, with implementation status. |

## 7. Build and Verification Procedure

Run the verification suites for the `capy` Starlark synchronization engine and the lander before submitting control-plane changes:

```bash
go -C .github/scripts/capy test -v ./...
go -C .github/scripts/capy run ./cmd/capy list -config ../../../capy.star
go -C .github/scripts/lander test ./...
```
