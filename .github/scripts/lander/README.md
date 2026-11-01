# lander

Single writer of `gosuda/0`. It lands workspace requests from `gosuda/1` onto the trunk
(`refs/suda/main`, `refs/suda/history`), keeps the control plane (`refs/heads/main`) and the
trunk overlay consistent, publishes `refs/heads/lander`, reconciles every mirror in `capy.star`,
stages external pull requests, and prunes stale workspace refs. Design: `docs/hardening-plan.md`.

The lander is level-triggered. A cycle fetches refs, compares trees, and does the outstanding work;
nothing depends on events arriving. Durable state lives only in refs (including
`refs/suda/sync/<workflow>` baselines) and commit statuses, so a restart resumes where it stopped.

## Commands

| Command | Purpose |
| :--- | :--- |
| `lander run` | Cycle forever; serves `/healthz` and an optional `/wake` |
| `lander once` | One cycle (use with `LANDER_DRYRUN=1` to observe decisions without pushing) |
| `lander check` | Verify tools and that the App can mint tokens for `gosuda/0` and `gosuda/1` |

## Configuration

| Variable | Default | Meaning |
| :--- | :--- | :--- |
| `LANDER_APP_ID` | required | `suda-lander` GitHub App id |
| `LANDER_APP_KEY_FILE` or `LANDER_APP_KEY` | required | App private key (PEM) as a file path, or the PEM content itself (literal `\n` is expanded) |
| `LANDER_TRUNK_REPO`, `LANDER_USER_REPO` | `gosuda/0`, `gosuda/1` | Trunk and user repositories |
| `LANDER_STORE` | `/var/lib/lander/data/store.git` | Bare object store shared by all remotes |
| `LANDER_INTERVAL` | `1m` | Poll interval for landing requests (a `/wake` ends the wait early) |
| `LANDER_MIRROR_INTERVAL` | `30m` | Interval of the mirror sweep (a cheap `ls-remote` plus one pull-request listing per mirror). A mirror is reconciled immediately when its project changed in the trunk or a webhook named it |
| `LANDER_MAGPIE_URL` | `https://magpie.gosuda.org` | Base of the magpie diff link in the review pull requests opened for staged mirror pull requests. Empty omits the link. |
| `LANDER_DRYRUN` | unset | Decide and log, push nothing |
| `LANDER_SIGNING_KEY_FILE` or `LANDER_SIGNING_KEY` | unset | SSH private key (path, or content) signing lander commits; when set, an unsigned `refs/suda/history` tip halts the queue. The allowed-signers entry is derived from the key's public half |
| `LANDER_ALLOWED_SIGNERS` | derived | Optional allowed-signers file overriding the derived one |
| `LANDER_EMBARGO_TEAM` | unset | `org/slug` of the team allowed to land embargoed changes (ids `sec-...`). Unset: they cannot be landed |
| `LANDER_EMBARGO_TTL` | `4320h` | Age after which embargoed workspace references are pruned (other workspaces: 14 days) |
| `LANDER_COMMITTER_NAME`, `LANDER_COMMITTER_EMAIL` | the App's bot identity | Identity on every commit the lander creates (author as well for commits it writes itself). Default: looked up from GitHub at start (`<slug>[bot]`, `<bot user id>+<slug>[bot]@users.noreply.github.com`), so commits show the App's logo; falls back to `Suda Lander <lander@gosuda.org>` when GitHub is unreachable |
| `LANDER_ACK_TIP` | unset | Accept that one unsigned history tip (after reviewing an out-of-band push) |
| `LANDER_PRESUBMIT` | `docker` | `docker` or `off` |
| `LANDER_PRESUBMIT_IMAGE`, `_TIMEOUT`, `_CPUS`, `_MEMORY` | `golang:1.27`, `15m`, `2`, `4g` | Sandbox settings |
| `LANDER_DOCKER`, `DOCKER_HOST` | `docker` | Docker CLI and daemon for the sandbox (use a dedicated daemon) |
| `LANDER_CACHE_DIR` | `/var/lib/lander/data/cache` | Dependency caches mounted read-only into the sandbox |
| `LANDER_LISTEN`, `LANDER_WAKE_SECRET` | `:8080`, unset | `/healthz`; `/wake` accepts an HMAC-SHA256 (`X-Hub-Signature-256`) signed POST and only shortens the wait |

## Merged review pull requests

Merging the review pull request `review/<id>` of a workspace or of a staged mirror pull request is a
landing request, like a push of `land/<id>`. The requester is the authenticated actor that set
`base/<id>` to the merge commit (activity API, `pr_merge`), the tip is the head the pull request was
merged at, and approvals of that head count. The lander ignores the merge when `review/<id>` no
longer holds that head or when `land/<id>` is pending. The review pair is deleted whatever the
outcome: a merged pull request cannot be reopened, and a rejected pair would be tried every cycle.

## Inbound mirror pushes

The pusher of a mirror commit comes from the repository activity API (`push`, `force_push`,
`branch_creation`, and `pr_merge` for a pull request merged in the web UI). The pushes that moved the
branch from the sync baseline to its tip are landed one by one, oldest first, each with its own
pusher as the requester, so a bot's push after a member's merge does not decide the member's change.
After each landed push whose mirror commit the trunk now exports exactly, the sync baseline moves to
that commit and the next push merges against it, so consecutive pushes that edit the same lines land.
A baseline left behind such a commit (a chain that stopped earlier) catches up at the start of the
next reconcile. The first push that fails a check stops the chain and the whole range is staged as
`refs/ws/capy/<workflow>-main`. When the API does not list every push of the range, or a commit of it
is missing from the store, the range is judged as one change by the pusher of the tip.

A `pr_merge` step also carries approvals (`mirrorPullReview` in `sync.go`). The lander finds the pull
request whose merge commit is the step's commit, then takes the reviewers whose latest decisive review
approves exactly its head (fetched from `refs/pull/*/head`). The approvals count only when
`git patch-id --verbatim` of `before..merge` equals that of `merge-base(head, before)..head`, so a merge
that resolved conflicts or added content is judged by its merger alone. This makes `set require-review`
workable for projects that merge on the mirror: another owner's approval on the mirror pull request
lands the merge. A failed pull-request lookup counts as no approval. The landed commit records
`Suda-Mirror-Pull: <repo>#<N>`.

The authors of a counted approval come from the head branch (`headAuthors` in `sync.go`): the lander
reads the activity API of the head repository, a fork included, with a token of the mirror, and
follows the branch's updates back from the reviewed head until the branch held only commits the mirror
branch already had or did not exist. Every actor on that path is an author, so a reviewer's fix-up or a
force-pushed version makes its pusher an author too. With the authors known, `set require-review` needs
an owner approver who is not an author (`capy.Identity.Authors`), and the merger's own approval counts
when the merger pushed nothing: the GitHub flow where the reviewer merges. Authors never become owners
of the change; the merger or an approver still has to be one. When the path is not covered (activity
pruned, fork unreadable, head repository deleted), the merger is the only author, as before. Each
author is recorded as `Suda-Mirror-Author: <login>`.

## Organization admin bypass

A requester who is an active admin of the organization (`GET /orgs/{org}/memberships/{login}`,
`role: admin`, `state: active`, members:read) bypasses `OWNERS` approvals, matching the
`OrganizationAdmin` bypass of GitHub rulesets. The lander first runs `CheckOwners` normally and asks
for the role only when that fails. For an admin it re-runs the check with `Identity.Admin`, which
accepts every path whose `OWNERS` chain is not empty, including `set noparent` and
`set require-review` chains the admin is not on. A path with no `OWNERS` entry, visibility, `go.mod`,
and presubmit still reject. The bypass applies to workspace landings, merged review pull requests,
and inbound mirror pushes alike. The landed commit records `Suda-Review-Bypass: <login>`, and the log
has an `organization admin bypassed OWNERS` warning with the waived violations. A failed role lookup
fails the landing attempt and is retried.

## Security properties

- The lander never executes repository code. Presubmit prepares sources in the lander process and
  runs tests in containers with `--network none`, a read-only root, no credentials and no host env.
- Authorization comes only from GitHub-authenticated identities (activity API pusher, PR approvals
  bound to the exact tip or to a patch-identical patchset of the same change, org team membership,
  org admin role).
  Commit author and trailers are informational: a `Change-Id` only selects which pull requests are
  consulted, never what is approved.
- Installation tokens are minted per repository and operation with narrowed permissions.

## Changes, patchsets, and approvals

`git ws-pr` pushes each reviewed tip to `refs/ws/cl/<change-id>/<tip>`; the lander fetches these into
`refs/user/cl/*` (`patchset.go`). A GitHub approval is bound to a commit. It counts for a landing tip
when that commit is the tip, or when `git patch-id --verbatim` of its diff against its own root
snapshot equals the tip's (`git diff-tree --binary`, no external diff or textconv). A cheap
changed-path comparison runs first, at most 8 approvals of other commits are examined per request, and
a commit the store does not hold, with several roots, or that changes nothing never matches. Approvals are
read from the pull requests of `review/<id>` and of every other review branch whose tip carries the
same `Change-Id`. The landed commit gets `Suda-Review: gosuda/1#<N>` for the newest review pull
request. Patchsets of one change are pruned together, 14 days after the newest one.

## Presubmit Go modules

Presubmit tests Go modules in the trunk's workspace (the root `go.work` of `refs/suda/main`) against the
local code of their internal dependencies. The lander writes a `go.work` at the root of the staging
directory. It lists the closure's Go modules that the trunk's `go.work` lists in `use`, nested modules
included, under the highest `go` directive of the closure's `go.mod` files (then the trunk's, then `1.21`).
Replace directives of the trunk whose local targets lie in the closure are carried over. The trunk's own
`go.work` is never exported. The sandbox sets `GOWORK=/work/go.work`. The prepare step warms the module
cache once for the workspace: `go list -e -m -json all`, then one `go mod download` that names the external
modules, so internal modules are never fetched. Go modules that the trunk's `go.work` does not list are
built on their own: their commands run with `GOWORK=off`, and their `go mod download` runs per module with
`GOWORK=off`.

Convention: a committed `go.mod` never contains a local `replace`; the root `go.work` resolves sibling
modules; a `require` of an internal module names a published version.

## Layout

The same layout builds in the control plane (`.github/scripts/lander`, `.github/scripts/capy`) and on
`refs/heads/lander` (`lander/`, `capy/`, `Dockerfile` at the root); `go.mod` uses `replace ../capy`.
Deployment: `deploy/deploy.sh`.
