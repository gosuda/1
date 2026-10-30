# gosuda/0 Monorepo Agent Rules

This file defines the mandatory operating rules for coding agents working in `gosuda/0`. The repository separates the control plane on `refs/heads/main` from the unified source tree on `refs/suda/main`.

## 1. Reference Boundaries

Every Git reference namespace in `gosuda/0` owns a fixed role. Obey these boundaries on every command:

- `refs/heads/main` stores only control-plane files (`AGENTS.md`, `README.md`, `.gitconfig`, `capy.star`, `.agents/`, `.github/`, `docs/`). Never commit project source directories to `refs/heads/main`.
- `refs/suda/main` stores the unified `google3` source tree as a single parentless snapshot commit. Never push directly to `refs/suda/main`.
- `refs/suda/history` stores the linear parent-linked commit chain for `git blame` and `git log`. Never push directly to `refs/suda/history`.
- `refs/ws/<actor>/<id>` stores isolated work-in-progress commits for an active task or an imported external mirror PR (`refs/ws/capy/<repo>-pr-<num>`).
- `refs/heads/base/<id>` and `refs/heads/review/<id>` store ephemeral base and head pointers created by `git ws-pr` for GitHub Pull Request code review. Both branches are automatically deleted upon landing.
- `refs/heads/land/<id>` enqueues a workspace branch for automated merging by `.github/scripts/land.sh`.

## 2. Directory Taxonomy, Visibility, and `OWNERS` Rules

The root tree of `refs/suda/main` organizes projects by domain and visibility tier. Table 1 lists the top-level directories, import boundaries, and `OWNERS` enforcement rules.

| Directory Prefix | Visibility Tier | Allowed Imports | Prohibited Imports | `OWNERS` Policy |
| :--- | :--- | :--- | :--- | :--- |
| `//web/*`, `//net/*`, `//ipc/*`, `//storage/*`, `//crypto/*`, `//ai/*`, `//devtools/*` | Public Production | Public Production, `//third_party/*` | `//corp/*`, `//experimental/*` | Hierarchical `OWNERS` enforced |
| `//corp/*` | Internal Private | Public Production, `//corp/*`, `//third_party/*` | `//experimental/*` | Hierarchical `OWNERS` enforced |
| `//third_party/*` | External Vendor | Self-contained or `//third_party/*` | All non-`third_party` domains | Hierarchical `OWNERS` enforced |
| `//experimental/<actor>/*` | Sandbox | Any non-corp package | `//corp/*` | Sandbox auto-approved (`OWNERS` exempt) |

### 2.1 Hierarchical `OWNERS` Verification (`capy check-owners`)

1. `land.sh` evaluates `OWNERS` files from each modified file's directory up to `//OWNERS`.
2. Adding `set noparent` in a directory's `OWNERS` file stops inheritance from parent directories.
3. `per-file <glob>=@owner1,@owner2` restricts or grants approval on matching filenames.
4. Existing `OWNERS` files are always read from `refs/suda/main` (`baseTree`) so a commit cannot self-approve by modifying `OWNERS`.
5. When modifying a restricted directory, either the commit author or a `Reviewed-by: @owner` (or `Approved-by: @owner`) commit message trailer must match an authorized owner.

### 2.2 Project CI Workflows

Keep each project's `.github/workflows/` directory inside its own project folder (for example, `ipc/hqq/HQQ/.github/workflows/`). The `capy` synchronizer exports that folder to the root of the standalone public mirror repository, where GitHub Actions executes it on public runners.

## 3. Mounted Repository Directory Map

Table 2 maps the mounted repositories to their exact paths inside `refs/suda/main`.

| Upstream Repository | Monorepo Path in `refs/suda/main` | Go Module / Package Path |
| :--- | :--- | :--- |
| `gosuda/website` | `web/gosuda` | `gosuda.org/website` |
| `gosuda/securl` | `web/securl` | `securl.click/securl` |
| `gosuda/GRT` | `net/grt` | `gosuda.org/grt` |
| `gosuda/IVNP` | `net/ivnp` | `gosuda.org/ivnp` |
| `gosuda/portalite` | `net/portalite` | `gosuda.org/portalite` |
| `gosuda/HQQ` | `ipc/hqq/HQQ` | `gosuda.org/hqq` |
| `gosuda/hqq-sqlite3` | `ipc/hqq/hqq-sqlite3` | `gosuda.org/sqlite3` |
| `gosuda/hqq-zstd` | `ipc/hqq/hqq-zstd` | `gosuda.org/zstd` |
| `gosuda/azure-magpie` | `devtools/azure-magpie` | `azure-winged-magpie` |

## 4. Internal and External Contribution Processes

Execute these steps using standard `git` commands or the aliases defined in `.gitconfig`.

> [!CAUTION]
> Never run `git pull` or an unconditional `git push --force` on a workspace branch. Running `git pull` fails against the parentless `refs/suda/main` snapshot, and `--force` without `--force-with-lease` overwrites concurrent updates. Use `git ws-sync` to rebase and `git ws-save`, `git ws-pr`, or `git ws-land` to push with a lease.

### 4.1 Standard Monorepo Workspace Flow (`refs/ws/*`)

1. Enable the repository Git aliases: `git config include.path ../.gitconfig`.
2. Attach and fetch the trunk references: `git suda-init`.
3. Restrict sparse checkout to the target directories: `git sparse-checkout set <domain>/<project>`.
4. Create an isolated workspace branch from `origin/suda/main`: `git ws-new agent-<name>/<task-id>`.
5. Run the package test suite inside every modified project directory before saving.
6. Stage, commit, and push work-in-progress to `refs/ws/agent-<name>/<task-id>`: `git ws-save`.
7. Inspect the two-dot diff via `git ws-diff` (or at `https://magpie.gosuda.org/diff`).
8. Rebase onto the latest `origin/suda/main` snapshot (`git ws-sync`) and land via `git ws-land` (or click **Land** in the Active Workspaces table on `https://magpie.gosuda.org/diff`).

```bash
git config include.path ../.gitconfig
git suda-init
git sparse-checkout set ipc/hqq web/gosuda
git ws-new agent-dalgona/task-402

# Edit files and run tests in the target directory
git add -A
git commit -m "ipc/hqq: validate slot index before header decode"
git ws-save
git ws-sync
git ws-land
```

### 4.2 Ephemeral GitHub Pull Request Review Flow (`git ws-pr`)

When a change requires line-by-line review comments or formal `OWNERS` sign-off on GitHub:

1. Run `git ws-pr` on your workspace branch. This pushes the workspace root snapshot to `refs/heads/base/<task-id>` and `HEAD` to `refs/heads/review/<task-id>`, opening a GitHub Pull Request on `gosuda/0` containing only your workspace delta.
2. Once approved (or with `Reviewed-by: @owner` appended to the commit/merge message), merge the PR on GitHub or click **Land** on `magpie.gosuda.org/diff`.
3. The `Azure-Winged Magpie` webhook automatically promotes the merged review tip to `refs/heads/land/<task-id>`, dispatches `land.sh`, and deletes `refs/heads/base/<task-id>` and `refs/heads/review/<task-id>`.

### 4.3 Experimental Sandbox and Graduation Flow (`git ws-exp`, `git ws-graduate`)

1. Start an experimental package without `OWNERS` gates: `git ws-exp <actor> <project>`. This adds `experimental/<actor>/<project>` to sparse-checkout and creates branch `ws/<actor>/exp-<project>`.
2. Iterate and land freely inside `//experimental/<actor>/<project>` using `git ws-land`.
3. To promote an experimental package into a production domain, run `git ws-graduate experimental/<actor>/<project> <domain>/<project>`, commit the move with required `OWNERS` approval (`Reviewed-by:`), and land via `git ws-pr` or `git ws-land`.

### 4.4 External Standalone Mirror Process (`gosuda/<repo>`)

External contributors and public CI runners interact with standalone `gosuda/<repo>` mirror repositories without cloning `gosuda/0`:

1. **Outbound Export (`land.sh`)**: Whenever a workspace lands on `refs/suda/main` and modifies a mapped project prefix, `capy` transforms the subtree, preserves the standalone mirror's `.github/workflows/`, appends `GitOrigin-RevId: <suda-main-sha>`, and pushes to `gosuda/<repo>`.
2. **Inbound Pull Request Staging (`sync-inbound.sh ws`)**: Opening or updating a pull request on `gosuda/<repo>` triggers the `Azure-Winged Magpie` webhook, which splices the external PR tree into `refs/ws/capy/<repo>-pr-<number>` on `gosuda/0` for cross-monorepo inspection and testing.
3. **Inbound Commit Landing (`sync-inbound.sh land`)**: Merging a PR or pushing a commit (without `GitOrigin-RevId`) to `main` on `gosuda/<repo>` triggers `sync-inbound.sh`, which splices the updated subtree onto `refs/suda/main` and advances `refs/suda/history` atomically.
