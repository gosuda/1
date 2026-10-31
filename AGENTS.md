# gosuda/0 Monorepo Agent Rules

This file defines the mandatory operating rules for coding agents working in `gosuda/0` and its workspace repository `gosuda/1`. The control plane lives on `refs/heads/main` of `gosuda/0`, and the unified source tree lives on `refs/suda/main` of `gosuda/0`. Only the lander writes to either of them.

## 1. Reference Boundaries

Every Git reference namespace owns a fixed role in one of the two repositories. Obey these boundaries on every command.

In `gosuda/0` (`origin`, read-only for members):

- `refs/heads/main` stores only control-plane files (`AGENTS.md`, `README.md`, `.gitconfig`, `capy.star`, `.agents/`, `.github/`, `docs/`). Never commit project source directories to it. The lander keeps the overlay entries of `refs/suda/main` equal to this branch. An org owner may push `refs/heads/main` directly as a break-glass action; the next lander cycle records that push as an overlay-sync commit with a `Suda-Break-Glass:` trailer.
- `refs/suda/main` stores the unified `google3` source tree as a single parentless snapshot commit. Never push to it.
- `refs/suda/history` stores the linear, lander-signed commit chain for `git blame` and `git log`. Never push to it.
- `refs/suda/sync/<wf>` stores the sync baseline for each mirror, and `refs/heads/lander` stores the deployable lander tree. Only the lander writes them.

In `gosuda/1` (`ws`, where members push):

- `refs/ws/<actor>/<id>` stores isolated work-in-progress commits for an active task.
- `refs/heads/land/<id>` is a landing request. The lander deletes it once it processes the request, whether the request lands or is rejected.
- `refs/heads/base/<id>` and `refs/heads/review/<id>` are the review pair created by `git ws-pr`. The lander deletes both after the change lands.
- `refs/ws/capy/<wf>-main` and `refs/ws/capy/<wf>-pr-<N>` are external changes staged by the lander. A staged change lands only through a member's landing request.
- `refs/suda/main` and `refs/suda/history` are copies of the `gosuda/0` trunk refs. They exist only as a push negotiation base. Landing never trusts them: the base of every request must match a tree in the trunk history.

## 2. Directory Taxonomy, Visibility, and `OWNERS` Rules

The root tree of `refs/suda/main` organizes projects by domain and visibility tier. Table 1 lists the top-level directories, import boundaries, and `OWNERS` enforcement rules.

| Directory Prefix | Visibility Tier | Allowed Imports | Prohibited Imports | `OWNERS` Policy |
| :--- | :--- | :--- | :--- | :--- |
| `//web/*`, `//net/*`, `//ipc/*`, `//storage/*`, `//crypto/*`, `//ai/*`, `//devtools/*` | Public Production | Public Production, `//third_party/*` | `//corp/*`, `//experimental/*` | Enforced; owners named explicitly |
| `//corp/*` | Internal Private | Public Production, `//corp/*`, `//third_party/*` | `//experimental/*` | Enforced; owners named explicitly |
| `//third_party/*` | External Vendor | Self-contained or `//third_party/*` | All non-`third_party` domains | Enforced; owners named explicitly |
| `//experimental/<actor>/*` | Sandbox | Any non-corp package | `//corp/*` | `<actor>` (the requester login) may land; others need the `OWNERS` chain |

### 2.1 `OWNERS` and Visibility Verification

The lander runs `capy check-owners` and `capy check-visibility` against the trunk on every landing request. The rules below are the ones those checks apply.

1. The lander reads `OWNERS` files only from the base trunk tree, which is `refs/suda/main` before the request lands. A change cannot approve itself by adding or editing an `OWNERS` file. A new `OWNERS` file takes effect once it has landed, and a new directory inherits from its nearest existing ancestor.
2. For each changed path, the chain starts at the file's directory and walks toward the root. `set noparent` stops the walk. `per-file <glob>=@owner1,@owner2` adds owners for matching file names or repository paths. A mirror workflow that commits a generated file as a bot gets exactly that file this way (for example `per-file data.json.zstd=@github-actions[bot]` in `web/gosuda/zdata/OWNERS`, declared in the file's own directory); the bot must be the authenticated pusher, and the grant covers nothing else. `OWNERS` files are never exported to mirrors.
3. Owners are GitHub logins (`@login`) or teams (`@org/team`). A team owner matches when the GitHub API reports an active membership. The literal `*` matches nobody. The root `OWNERS` names explicit owners.
4. A path is approved when the requester or an approver belongs to its owner set. Approvals count only when they are `APPROVED` on the exact tip being landed.
5. `set require-review` in any `OWNERS` file of the chain requires an approver other than the requester. `.github/OWNERS` uses it for the control plane.
6. A path with no owners anywhere in its chain is rejected.
7. Commit author, committer, and `Reviewed-by:` or `Approved-by:` trailers are not identities. They never approve anything.
8. Visibility is checked by resolving references: Go import paths against `go.mod` module paths, Cargo `path` dependencies, `package.json` `file:` and `link:` dependencies, and relative JavaScript and TypeScript imports. Comments never count as references.

### 2.2 Project CI Workflows

Keep each project's `.github/workflows/` directory inside its project folder (for example, `ipc/hqq/HQQ/.github/workflows/`). The lander exports that folder to the root of the public mirror repository, where GitHub Actions runs it on public runners. Workflows on `gosuda/0` itself do not exist; landing runs in the lander's presubmit.

An edit made directly in a mirror's `.github/workflows/` is an inbound change. It lands only when the mirror pusher owns the prefix and is otherwise staged for a member.

### 2.3 Go Module Layout Rules

1. A committed `go.mod` never contains a local `replace` directive that leaves the module directory (`replace x => ../y`). The lander rejects a landing that adds one. Sibling modules resolve locally through the root `go.work`, which the lander never exports to a mirror.
2. A `require` of another monorepo module names a published version. Presubmit and the standalone mirror both build against it; the root `go.work` substitutes the local code during monorepo development and presubmit.
3. Add every new Go module (nested modules included) to the root `go.work` with `.github/scripts/go-work-add.sh <prefix>` in a separate commit. A module that does not compile in workspace mode stays out of `go.work` until it does; presubmit builds it with `GOWORK=off`. Typical causes are a pinned dependency whose API the local trunk code has moved past, and workspace-wide version unification (the highest `require` of any module wins), for example `net/zeroproxy`, whose `webtransport-go` needs an older `quic-go` than the workspace selects. A `go.work` `replace` may point an internal version that was never published at its local directory.
4. Import an external repository as an unmodified copy (`import-repo.sh`) so that the mirror tip equals the export of the trunk prefix and no export commit is needed. Remove a repository's prebuilt binaries and other large files in the source repository before importing it, and make go.mod and `go.work` cleanups in a second commit.
5. `//corp/*` projects are never mirrored: `import-repo.sh` and `create-repo.sh` skip the `capy.star` entry and the explorer row for `corp/` and `experimental/` prefixes.

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
| `gosuda/keyless_tls` | `crypto/keyless_tls` | `github.com/gosuda/keyless_tls` |
| `gosuda/x402-facilitator` | `crypto/x402-facilitator` | `github.com/gosuda/x402-facilitator` (nested modules `/cmd` and `/scheme/evm`) |
| `gosuda/portal-tunnel` | `net/portal-tunnel` | `github.com/gosuda/portal-tunnel/v2` |
| `gosuda/potly` | `devtools/potly` | `potly` |
| `gosuda/AutoIRC2P` | `net/autoirc2p` | `github.com/gosuda/AutoIRC2P` |
| `gosuda/portal-multiplatform-sdk` | `net/portal-multiplatform-sdk` | `github.com/gosuda/portal-multiplatform-sdk/native/bridge` |
| `gosuda/randflake` | `crypto/randflake` | `gosuda.org/randflake/v2` |
| `gosuda/jimin` | `ai/jimin` | `gosuda.org/jimin` |
| `gosuda/telemetry.gosuda.org` | `web/telemetry` | `telemetry.gosuda.org/telemetry` |
| `gosuda/rivulet` (private, not mirrored) | `corp/rivulet` | `github.com/gosuda/rivulet` |

## 4. Internal and External Contribution Processes

Execute these steps with standard `git` commands or the aliases in `.gitconfig`. Members never push to `origin`; `git push` to `origin` fails by design.

> [!CAUTION]
> Never run `git pull` or an unconditional `git push --force` on a workspace branch. `git pull` fails against the parentless `refs/suda/main` snapshot, and `--force` without `--force-with-lease` overwrites concurrent updates. Use `git ws-sync` to rebase, then `git ws-save`, `git ws-pr`, or `git ws-land` to push with a lease.

### 4.1 Standard Monorepo Workspace Flow (`refs/ws/*`)

1. Enable the repository Git aliases: `git config include.path ../.gitconfig`.
2. Attach the trunk references and the `ws` remote: `git suda-init`.
3. Restrict sparse checkout to the target directories: `git sparse-checkout set <domain>/<project>`.
4. Create an isolated workspace branch from `origin/suda/main`: `git ws-new agent-<name>/<task-id>`.
5. Run the package test suite inside every modified project directory before saving.
6. Commit, then push work-in-progress to `refs/ws/agent-<name>/<task-id>` on `gosuda/1`: `git ws-save`.
7. Inspect the two-dot diff with `git ws-diff`, or at `https://magpie.gosuda.org/diff`.
8. Rebase onto the latest trunk snapshot with `git ws-sync`, then submit with `git ws-land`.
9. Read the result on the tip in `gosuda/1`: a `suda/land` status of `success`, or `failure` with the reason. The landing ref is deleted in both cases.

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

When a change needs line-by-line review comments or formal `OWNERS` sign-off on GitHub:

1. Run `git ws-pr` on the workspace branch. It pushes the workspace base commit to `refs/heads/base/<task-id>` and `HEAD` to `refs/heads/review/<task-id>` on `gosuda/1`, then opens a pull request between them that contains only the workspace delta.
2. Reviewers approve the `review/<task-id>` tip. An approval counts only for the exact commit that is landed.
3. Submit the same tip with `git ws-land`. Merging the pull request does not land the change. After the change lands, the lander deletes `refs/heads/base/<task-id>` and `refs/heads/review/<task-id>`, which closes the review pull request.

### 4.3 Experimental Sandbox and Graduation Flow (`git ws-exp`, `git ws-graduate`)

1. Start an experimental package: `git ws-exp <actor> <project>`. This adds `experimental/<actor>/<project>` to sparse checkout and creates branch `ws/<actor>/exp-<project>`. `<actor>` must be your GitHub login. Otherwise the path needs the normal `OWNERS` chain.
2. Iterate and land inside `//experimental/<actor>/<project>` with `git ws-land`.
3. To promote a package into a production domain, run `git ws-graduate experimental/<actor>/<project> <domain>/<project>`, commit the move, and land it with `git ws-pr` and `git ws-land`. The destination path needs `OWNERS` approval.

### 4.4 External Standalone Mirror Process (`gosuda/<repo>`)

External contributors and public CI runners interact with standalone `gosuda/<repo>` mirror repositories without cloning `gosuda/0`.

1. **Outbound export**: After a landing changes a mirror's prefix, the lander exports the prefix on top of the mirror's current tip. Excluded paths stay out of the mirror. The exported commit carries `GitOrigin-RevId: <refs/suda/history commit>`.
2. **Inbound pull request staging**: The lander stages each open pull request on a mirror as `refs/ws/capy/<wf>-pr-<N>` on `gosuda/1` and deletes the staged ref when the pull request closes. Staged changes never land automatically.
3. **Inbound push landing**: A push to a mirror branch lands automatically only when the pusher owns the prefix under the rules in Section 2.1. Otherwise the lander stages it as `refs/ws/capy/<wf>-main`, and the mirror export for that workflow waits until the change is incorporated.
4. **Landing a staged change**: Fetch with `git fetch ws`, create a workspace branch from the staged ref (`git switch -c ws/<actor>/capy-<wf>-pr-<N> refs/remotes/ws/ws/capy/<wf>-pr-<N>`), then run `git ws-land`. You become the requester, so the `OWNERS` rules apply to you. If the lander rejects the request for an unknown base or a merge conflict, run `git ws-sync` and resolve the conflicts with the `suda-resolve-conflicts` skill.
