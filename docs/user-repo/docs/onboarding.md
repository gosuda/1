# Onboarding guide

This guide takes you from nothing to a landed change. It applies to people and to coding agents.

## 1. Prerequisites

- A recent Git (2.30 or newer for partial clone and sparse checkout), SSH access to GitHub, and membership in the `gosuda` organization.
- [`gh`](https://cli.github.com) for `git ws-pr` and for the helper scripts (optional otherwise).

## 2. One-time setup

### The one-liner

With `gh` logged in (`gh auth login`), run this once. List the directories your task needs after the options:

```bash
gh api -H 'Accept: application/vnd.github.raw' repos/gosuda/0/contents/.github/scripts/onboard.sh \
  | bash -s -- -d gosuda-0 ipc/hqq web/gosuda
```

Options: `-d <dir>` target directory (default `gosuda-0`), `-p https` to clone over HTTPS instead of SSH.
The script clones `gosuda/0` **read-only** (blob-less and sparse), includes the trusted `.gitconfig`, runs
`git suda-init` (adds the `ws` remote, which is `gosuda/1`, and disables pushes to `origin`), and sets the
sparse directories. It refuses to guess: unknown directories, an existing non-clone target, or a dirty
clone stop it with a message.

The script comes from `gosuda/0`, where only the lander and org owners write. Do not run a copy of it from a
branch of `gosuda/1`.

### Re-running it

Run the same command with `-d` pointing at your existing clone to refresh the aliases from the current
control plane. It needs the clone on a clean `main` (your `ws/...` branches are never touched) and tells you
what to do otherwise. This also replaces the manual refresh below.

### Doing it by hand

The control plane carries the trusted `.gitconfig` aliases and the rules agents follow; that is why you
clone `gosuda/0` instead of this repository.

```bash
git clone --filter=blob:none --sparse git@github.com:gosuda/0.git
cd 0
git config include.path ../.gitconfig
git suda-init                              # adds the `ws` remote (gosuda/1) and disables pushes to `origin`
git sparse-checkout set ipc/hqq web/gosuda # only the directories your task needs
```

> [!WARNING]
> Always keep `--filter=blob:none --sparse`. Without them you download every project.
> After `git suda-init`, `origin` (`gosuda/0`) cannot be pushed to. That is intended.

### Already set up before the migration?

Your clone still has the old aliases. Re-run the one-liner with `-d` set to your clone, or refresh by hand:

```bash
git fetch origin refs/heads/main
git checkout FETCH_HEAD -- .gitconfig
git suda-init
```

## 3. Your first change

```bash
git ws-new alice/fix-header          # branch ws/alice/fix-header, rooted at the trunk snapshot
# edit files, run the project's tests
git add -A && git commit -m "ipc/hqq: validate slot index before header decode"
git ws-save                          # push work in progress to gosuda/1
git ws-sync                          # rebase onto the latest trunk (do this before landing)
git ws-land                          # submit for landing
```

Use your GitHub login as the actor segment (`alice`): you may land freely under `experimental/<login>/`.
Never use `git pull`; use `git ws-sync`.

### Watch the result

The lander reports on the **tip commit of your request** as the `suda/land` commit status in `gosuda/1`:

```bash
gh api repos/gosuda/1/commits/$(git rev-parse HEAD)/statuses --jq '.[] | "\(.context) \(.state) \(.description)"'
```

| Status description | Meaning | What to do |
| :--- | :--- | :--- |
| `landed as <sha>` | Done. The trunk contains your change. | Nothing. |
| `merge conflict: ...` | Your change overlaps newer trunk changes. | `git ws-sync`, resolve, commit, `git ws-land`. |
| `unknown base tree ...` | Your workspace root is not a recent trunk snapshot. | `git ws-sync`, then `git ws-land`. |
| `OWNERS: ...` | You are not an owner of a path you changed, and no owner approved this exact commit. | Ask an owner (see [Review](#4-review-and-ownership)). |
| `visibility: ...` | A public project imports `corp/` or `experimental/` code (or another tier rule). | Remove the import. |
| `presubmit: FAILED: <dir>` | Tests of an affected project failed. | Fix and resubmit. |

A request whose requester cannot be resolved yet simply waits and is retried; a rejected request is closed and
its `land/<id>` branch is deleted, while your `refs/ws/...` branch stays.

## 4. Review and ownership

`OWNERS` files decide who may change what. They are read from the current trunk, so a change cannot grant
itself access. Who counts as "you" comes only from GitHub: the account that pushed the landing branch. Commit
author names and `Reviewed-by:` lines are ignored.

To get an owner's approval:

```bash
git ws-pr                            # opens a pull request in gosuda/1 (base/<id> <- review/<id>)
```

An owner approves the pull request **on the exact commit you will land**. If you push another commit, ask for
a new approval. Then run `git ws-land` as usual. Changes under `.github/` additionally require an owner other
than yourself (`set require-review`).

## 5. Experiments, new projects, imports

| Goal | Command |
| :--- | :--- |
| Prototype under your own sandbox | `git ws-exp <login> <name>` creates `experimental/<login>/<name>` |
| Promote a prototype | `git ws-graduate experimental/<login>/<name> <domain>/<name>` (needs `OWNERS` approval) |
| Create a new project | `.github/scripts/create-repo.sh <prefix> <owner/repo> [go-module]` |
| Import an existing GitHub repository | `.github/scripts/import-repo.sh <prefix> <owner/repo> [branch]` |

Both scripts build a single commit in your workspace (project files, a `capy.star` entry, a row in the
`docs/README.md` project table) and print the next steps. They never touch `gosuda/0`. For a public mirror, an
org owner must create the repository and add it to the `suda-lander` GitHub App installation; until then
the lander reports `destination missing`.

The scripts are part of the trunk, so they are already in your workspace. They are intentionally not copied
into this repository: anyone in the organization can write here, so nothing you would execute is published
from here.

## 6. Changes that arrive from public mirrors

Public mirror repositories (for example `gosuda/HQQ`) are exports of the trunk.

- A change merged on a mirror's default branch by an **owner** of that project is imported by the lander
  automatically; the next export carries both sides.
- A pull request opened on a mirror, or a push by a non-owner, is **staged** in this repository as
  `refs/ws/capy/<project>-pr-<N>` or `refs/ws/capy/<project>-main`. It lands only when a member requests it:

```bash
git fetch ws                                       # staged refs appear as refs/remotes/ws/ws/capy/...
git push ws refs/remotes/ws/ws/capy/HQQ-pr-12:refs/heads/land/hqq-pr-12
```

Review it like any other change. You become the requester, so `OWNERS` applies to you.

Once the change has landed and been exported, the lander closes the original pull request on the mirror with a
comment (only if its head is exactly what landed; a pull request that got new commits stays open).

## 7. Security fixes under embargo

If you are fixing an undisclosed vulnerability, do **not** use ordinary branch names or land it. Follow
[`docs/security-embargo.md`](https://github.com/gosuda/0/blob/main/docs/security-embargo.md): use an opaque id
`sec-<YYYYMM>-<8 hex>`, a branch `<login>/sec/<id>`, and neutral commit text (`<id>: <neutral summary>`).
References with a `sec` or `sec-*` path segment are hidden from the public site, kept past the 14-day cleanup,
and can be landed only by the security response team. Never put the vulnerability, the project, or a CVE
number in a branch name, commit subject, pull request title, or status.

## 8. Rules that matter

1. Push only to the `ws` remote (`git ws-save`, `git ws-land`, `git ws-pr`). `origin` is read-only.
2. One task per workspace branch. Do not reuse a branch for unrelated work.
3. Never use `git push --force` without `--force-with-lease`, and never use `git pull` in a workspace.
4. Never put credentials in any ref: everyone in the organization can read `refs/ws/*`.
5. Workspace branches under `refs/ws/` are pruned after 14 days without a push.
6. Do not run scripts or aliases from branches of `gosuda/1` that you did not write.
7. Go modules: never commit a `replace x => ../y` in `go.mod` (the lander rejects it). Sibling modules resolve through
   the root `go.work`; add a new module with `.github/scripts/go-work-add.sh <prefix>`. A `require` of another
   monorepo module names a published version.

## 9. Troubleshooting

| Symptom | Cause and fix |
| :--- | :--- |
| `remote helper 'no-push' aborted session` | You pushed to `origin`. Push to `ws` (`git ws-save`, `git ws-land`). |
| `ws-new` fails with `Aborting` | Local changes would be overwritten by the switch. Commit or stash first. |
| Nothing happens after `git ws-land` | Check the status command above; a request waits at most about a minute (the lander also reacts to pushes). |
| Your push is rejected as non-fast-forward | Use the aliases: they push with `--force-with-lease` to your own branch. |

Questions: ask in the organization channel, and include the status description and your landing id.
