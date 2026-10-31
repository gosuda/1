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
| `LANDER_APP_ID`, `LANDER_APP_KEY_FILE` | required | `suda-lander` GitHub App id and private key (PEM) |
| `LANDER_TRUNK_REPO`, `LANDER_USER_REPO` | `gosuda/0`, `gosuda/1` | Trunk and user repositories |
| `LANDER_STORE` | `/var/lib/lander/store.git` | Bare object store shared by all remotes |
| `LANDER_INTERVAL` | `15s` | Poll interval |
| `LANDER_DRYRUN` | unset | Decide and log, push nothing |
| `LANDER_SIGNING_KEY`, `LANDER_ALLOWED_SIGNERS` | unset | SSH key signing lander commits; when set, an unsigned `refs/suda/history` tip halts the queue |
| `LANDER_ACK_TIP` | unset | Accept that one unsigned history tip (after reviewing an out-of-band push) |
| `LANDER_PRESUBMIT` | `docker` | `docker` or `off` |
| `LANDER_PRESUBMIT_IMAGE`, `_TIMEOUT`, `_CPUS`, `_MEMORY` | `golang:1.27`, `15m`, `2`, `4g` | Sandbox settings |
| `LANDER_DOCKER`, `DOCKER_HOST` | `docker` | Docker CLI and daemon for the sandbox (use a dedicated daemon) |
| `LANDER_CACHE_DIR` | `/var/lib/lander/cache` | Dependency caches mounted read-only into the sandbox |
| `LANDER_LISTEN`, `LANDER_WAKE_SECRET` | `:8080`, unset | `/healthz`; `/wake` accepts an HMAC-SHA256 (`X-Hub-Signature-256`) signed POST and only shortens the wait |

## Security properties

- The lander never executes repository code. Presubmit prepares sources in the lander process and
  runs tests in containers with `--network none`, a read-only root, no credentials and no host env.
- Authorization comes only from GitHub-authenticated identities (activity API pusher, PR approvals
  bound to the exact tip, org team membership). Commit author and trailers are informational.
- Installation tokens are minted per repository and operation with narrowed permissions.

## Layout

The same layout builds in the control plane (`.github/scripts/lander`, `.github/scripts/capy`) and on
`refs/heads/lander` (`lander/`, `capy/`, `Dockerfile` at the root); `go.mod` uses `replace ../capy`.
Deployment: `deploy/deploy.sh`.
