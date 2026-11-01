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
- `refs/ws/cl/<change-id>/<tip>` stores an immutable patchset: a tip that `git ws-pr` sent for review. The lander fetches these refs to read the commit a reviewer approved after `refs/heads/review/<id>` moved on, and prunes all patchsets of a change 14 days after the newest one. Embargoed work uses `refs/ws/cl/sec/<change-id>/<tip>`, which magpie never lists and the lander keeps for the embargo TTL.
- `refs/ws/autosave/<actor>/<id>` stores the private working-state backup of `git ws-autosave`. It is never reviewed or landed.
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
4. A path is approved when the requester or an approver belongs to its owner set. Approvals count only when they are `APPROVED` on the exact tip being landed, or on another patchset of the same change that makes the identical patch against its own base snapshot (Section 4.5). A different diff, a whitespace-only edit, a mode change, or a patchset the lander cannot read never inherits an approval.
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
3. Restrict sparse checkout to the target directories: `git sparse-checkout set <domain>/<project>`. To fetch more projects later, run `git sparse-checkout add <domain>/<project>`; running `set` again replaces the list and drops `.agents` and `.github` unless you name them.
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
2. Reviewers approve the `review/<task-id>` tip. An approval counts for the exact commit that is landed and for a patch-identical patchset of the same change (Section 4.5).
3. Land the same tip either by merging the review pull request on GitHub or with `git ws-land`. A merge counts as a landing request by the merger for the head that was merged, and the approvals on that head count. The lander reads the merger from GitHub's activity record, never from commit metadata, and ignores a merge once `refs/heads/review/<task-id>` no longer holds the merged head. Merging is a request, not a result: if the lander rejects it (`OWNERS`, visibility, presubmit, conflict), the failure appears as a `suda/land` status on the merged head, the review branches are deleted, and a new `git ws-pr` is needed. After the change lands, the lander deletes `refs/heads/base/<task-id>` and `refs/heads/review/<task-id>`.

### 4.3 Experimental Sandbox and Graduation Flow (`git ws-exp`, `git ws-graduate`)

1. Start an experimental package: `git ws-exp <actor> <project>`. This adds `experimental/<actor>/<project>` to sparse checkout and creates branch `ws/<actor>/exp-<project>`. `<actor>` must be your GitHub login. Otherwise the path needs the normal `OWNERS` chain.
2. Iterate and land inside `//experimental/<actor>/<project>` with `git ws-land`.
3. To promote a package into a production domain, run `git ws-graduate experimental/<actor>/<project> <domain>/<project>`, commit the move, and land it with `git ws-pr` and `git ws-land`. The destination path needs `OWNERS` approval.

### 4.4 External Standalone Mirror Process (`gosuda/<repo>`)

External contributors and public CI runners interact with standalone `gosuda/<repo>` mirror repositories without cloning `gosuda/0`.

1. **Outbound export**: After a landing changes a mirror's prefix, the lander exports the prefix on top of the mirror's current tip. Excluded paths stay out of the mirror. The exported commit carries `GitOrigin-RevId: <refs/suda/history commit>`.
2. **Inbound pull request staging**: The lander stages each open pull request on a mirror as `refs/ws/capy/<wf>-pr-<N>` on `gosuda/1` and deletes the staged ref when the pull request closes. Staged changes never land automatically.
3. **Inbound push landing**: A push to a mirror branch (including a pull request merged on GitHub) lands automatically only when the pusher owns the prefix under the rules in Section 2.1. Each push since the last sync is judged by its own pusher, oldest first, so a later push by a bot cannot take over an earlier push by a member. If the activity API no longer lists every push, the pusher of the branch tip is judged for the whole range. Otherwise the lander stages it as `refs/ws/capy/<wf>-main`, and the mirror export for that workflow waits until the change is incorporated.
4. **Landing a staged change**: Fetch with `git fetch ws`, create a workspace branch from the staged ref (`git switch -c ws/<actor>/capy-<wf>-pr-<N> refs/remotes/ws/ws/capy/<wf>-pr-<N>`), then run `git ws-land`. You become the requester, so the `OWNERS` rules apply to you. If the lander rejects the request for an unknown base or a merge conflict, run `git ws-sync` and resolve the conflicts with the `suda-resolve-conflicts` skill.

### 4.5 Changes, Patchsets, and Autosave

The flow gives every workspace the identity and review semantics of a changelist.

1. **Change identity.** A change is identified by the `Change-Id: I<40 hex>` trailer of its tip commit. `git ws-save`, `git ws-pr`, and `git ws-land` add it once (`git ws-id` does so explicitly), reuse an existing one, and keep it across amends, rebases, and renamed branches.
2. **Patchsets.** `git ws-pr` pushes the tip to the immutable `refs/ws/cl/<change-id>/<tip>` next to the review branches. Run it again after `git ws-sync` or an amend to add a patchset to the same pull request. `git ws-ps` lists the patchsets of the current change, and `git ws-psdiff <a> <b>` shows the interdiff between two of them.
3. **Approval carry-over.** An approval is bound by GitHub to a commit. The lander counts it for the landing tip when the approved commit is the tip, or makes the same patch to its own root snapshot (`git patch-id --verbatim` of the full binary diff, so whitespace, modes, and binary content take part). The lander reads approvals from the pull request of `refs/heads/review/<id>` and of every other review branch whose tip carries the same `Change-Id`. After the trunk moves, `git ws-sync` followed by `git ws-land` therefore lands a clean rebase without a new review. A conflict resolved differently or any content edit needs a new approval, and `OWNERS` are still evaluated against the trunk at landing time.
4. **Numbers.** The landed commit keeps its `Change-Id` and records `Suda-Review: gosuda/1#<N>` when a review pull request existed. `git suda-cl <N|Change-Id>` finds the landed commit on `refs/suda/history`.
5. **Autosave.** `git ws-autosave start` backs up the working state (tracked files and untracked files that `.gitignore` does not exclude) to `refs/ws/autosave/<actor>/<id>` without touching the index, `HEAD`, or the branch. It is opt-in and never starts implicitly. Other machines restore it with `git ws-resume`. The refs in `gosuda/1` are readable by every member, so keep secrets out of the working tree as you would out of any commit.
6. **Instant workspaces.** `git ws-wt <actor>/<id>` adds a worktree on a new workspace branch rooted at `origin/suda/main`, with the same sparse directories, without a new clone.

`git suda-init` installs the workspace tools, `.gitconfig` and `.github/scripts/ws.sh`, from the checkout into the common Git directory (`suda.gitconfig`, `suda-ws.sh`). The aliases run that installed copy, so every branch and worktree of the clone uses the same version, including branches whose tree predates `ws.sh`. Run `git suda-init` again after the control plane changes `.gitconfig` or `ws.sh`. On a clone whose installed aliases predate `ws.sh`, run it twice: the first run installs the new aliases, and the second run installs `ws.sh`. On a checkout without `ws.sh`, `git suda-init` keeps the installed tools. To try an edited `ws.sh`, run `bash .github/scripts/ws.sh <subcommand>` directly.

### 4.6 Parallel Agents and Worktrees

Run parallel agents as one blobless clone per machine with one worktree per agent. Each worktree holds one change.

1. **One change, one worktree, one agent.** Create the worktree with `git ws-wt <actor>/<id> [<dir>]`. It shares objects, blobs fetched on demand, trunk refs, and aliases with the clone, and keeps its own index, `HEAD`, rebase state, sparse directories, and autosave state. Git refuses to check out one branch in two worktrees.
2. **Split concurrent work on one project into separate changes.** Give each change files that the others do not touch. The lander serializes landings. When one change conflicts with another that landed first, the later change runs `git ws-sync` and resolves the conflict.
3. **Change several projects atomically in one worktree.** List each project in the worktree's sparse directories. The change lands in a single landing.
4. **Never use `git stash`.** `refs/stash` is shared by every worktree, so one agent can pop another agent's work. Commit and `git ws-save` instead, or run `git ws-autosave once`. The command hook denies `git stash`.
5. **Run fetches through the workspace tools.** Worktrees share their remote-tracking refs, and two concurrent fetches fail with `cannot lock ref`. `ws-new`, `ws-sync`, `ws-exp`, `suda-log`, `suda-init`, and every `ws.sh` subcommand fetch under one lock in the common Git directory. Fetch with `git suda-ws fetch <args>`; the command hook rewrites a plain `git fetch` to this form. Pushes need no lock, because each workspace pushes its own refs.
6. **Do not change shared state.** Run `git config` only with `--worktree`. Never run `git gc --prune=now` or `git worktree prune`, and never delete another agent's branch.
7. **Use separate clones when there is an isolation boundary.** This covers agents in their own container, VM, or remote machine (a worktree's `.git` file holds the absolute path of the clone), agents that push with different GitHub identities (the lander's requester is the pusher), and agents that must not affect each other's Git state.
