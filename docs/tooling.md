# Server-Side Tooling and Pilot Repository Specification

The `gosuda/0` repository requires zero custom CLI binaries on developer workstations or agent containers. All merge-queue execution, policy verification, and subtree synchronization run inside GitHub Actions workflows stored on `refs/heads/main`.

## 1. Control-Plane Directory Layout

The `refs/heads/main` branch contains the agent rules, Git aliases, Starlark synchronization config, and CI workflow scripts. The entire branch occupies less than 20 KiB on disk.

```text
/ (refs/heads/main)
├── AGENTS.md
├── README.md
├── .gitconfig
├── capy.star
├── .agents/
│   ├── hooks.json
│   ├── scripts/
│   │   └── guard-git.sh
│   └── skills/
└── .github/
    ├── workflows/
    │   ├── import.yml
    │   ├── land.yml
    │   ├── sync-inbound.yml
    │   └── ws-gc.yml
    └── scripts/
        ├── create-repo.sh
        ├── import-pilot.sh
        ├── import-repo.sh
        ├── land.sh
        ├── sync-inbound.sh
        ├── ws-gc.sh
        └── capy/
```

## 2. Pilot Repository Measurements

The initial migration imports seven `gosuda` repositories into `refs/suda/main`. Table 1 lists the target path, Go module identifier, file count, and measured byte sizes from the GitHub Git Trees API.

| Upstream Repository | Target Path in `refs/suda/main` | Go Module Path | HEAD Files | Historical Disk (KB) | HEAD Uncompressed (KB) |
| :--- | :--- | :--- | ---: | ---: | ---: |
| `gosuda/website` | `//web/gosuda` | `gosuda.org/website` | 204 | 124,640 | 15,015 |
| `gosuda/IVNP` | `//net/ivnp` | `gosuda.org/ivnp` | 661 | 5,485 | 5,985 |
| `gosuda/HQQ` | `//ipc/hqq/HQQ` | `gosuda.org/hqq` | 594 | 3,095 | 6,848 |
| `gosuda/hqq-sqlite3` | `//ipc/hqq/hqq-sqlite3` | `gosuda.org/sqlite3` | 121 | 5,612 | 9,234 |
| `gosuda/hqq-zstd` | `//ipc/hqq/hqq-zstd` | `gosuda.org/zstd` | 54 | 3,140 | 4,750 |
| `gosuda/securl` | `//web/securl` | `securl.click/securl` | 153 | 1,014 | 710 |
| `gosuda/portalite` | `//net/portalite` | `gosuda.org/portalite` | 25 | 223 | 318 |
| **Total (7 Repositories)** | **Unified root tree** | **Multi-module** | **1,812** | **143,209** | **42,860** |

Importing only the `HEAD` tree of `gosuda/website` eliminates 109,625 KB of historical `zdata/data.json.zstd` revisions. The compressed Git pack for all seven repositories combined is approximately 18 to 20 MB, and a blobless tree fetch (`--filter=blob:none`) downloads 65 KB.

## 3. Initial Pilot Import Procedure (`import-pilot.sh`)

Assemble the initial `refs/suda/main` root tree from the seven upstream repositories using `git read-tree --prefix` without checking files out to the working directory.

> [!IMPORTANT]
> Run `import-pilot.sh` inside a temporary index (`GIT_INDEX_FILE`) so the script does not alter the control-plane index on `refs/heads/main`.

1. Allocate a temporary index file path and register an exit trap to remove it.
2. Initialize an empty Git index using `git read-tree --empty`.
3. Fetch each upstream repository at `--depth=1` and mount its `HEAD^{tree}` under its `google3` prefix.
4. Write the composite index to a root tree object using `git write-tree`.
5. Create the initial snapshot commit and push it atomically to `refs/suda/main` and `refs/suda/history`.

```bash
#!/usr/bin/env bash
set -euo pipefail

TMP_IDX=$(mktemp -u "${TMPDIR:-/tmp}/suda-idx.XXXXXX")
export GIT_INDEX_FILE="$TMP_IDX"
trap 'rm -f "$TMP_IDX"' EXIT

declare -A REPOS=(
  ["web/gosuda"]="https://github.com/gosuda/website.git"
  ["net/ivnp"]="https://github.com/gosuda/IVNP.git"
  ["ipc/hqq/HQQ"]="https://github.com/gosuda/HQQ.git"
  ["ipc/hqq/hqq-sqlite3"]="https://github.com/gosuda/hqq-sqlite3.git"
  ["ipc/hqq/hqq-zstd"]="https://github.com/gosuda/hqq-zstd.git"
  ["web/securl"]="https://github.com/gosuda/securl.git"
  ["net/portalite"]="https://github.com/gosuda/portalite.git"
)

git read-tree --empty
for prefix in "${!REPOS[@]}"; do
  git fetch --depth=1 "${REPOS[$prefix]}" HEAD
  tree=$(git rev-parse "FETCH_HEAD^{tree}")
  git read-tree --prefix="${prefix}/" "$tree"
done

ROOT_TREE=$(git write-tree)
SNAP=$(git commit-tree "$ROOT_TREE" -m "chore: initialize refs/suda/main with 7 pilot repositories")

git push --atomic origin \
  "${SNAP}:refs/suda/main" \
  "${SNAP}:refs/suda/history"
```

## 4. GitHub Actions Workflow Specifications

Three server-side workflows on `refs/heads/main` automate merging, external synchronization, and workspace garbage collection.

### 4.1 Automated Landing Workflow (`land.yml`)

Pushing a branch to `refs/heads/land/<id>` triggers `land.yml`. The workflow serializes merges across callers, updates `refs/suda/main` and `refs/suda/history`, exports modified public subtrees, and deletes the temporary landing branch.

```yaml
name: land
on:
  push:
    branches:
      - "land/**"

concurrency:
  group: land-suda-main
  cancel-in-progress: false

permissions:
  contents: write

jobs:
  land:
    runs-on: ubuntu-latest
    steps:
      - name: Checkout control plane
        uses: actions/checkout@v4
        with:
          ref: refs/heads/main
          filter: blob:none
      - name: Merge into refs/suda/main and export mirrors
        env:
          LAND_REF: ${{ github.ref }}
          MIRROR_TOKEN: ${{ secrets.SUDA_MIRROR_TOKEN }}
        run: ./.github/scripts/land.sh "$LAND_REF"
```

### 4.2 Stale Workspace Garbage Collection (`ws-gc.yml`)

The `ws-gc.yml` workflow runs once every 24 hours. It enumerates references under `refs/ws/*`, inspects the committer timestamp of each tip commit, and deletes references older than 14 days using `--force-with-lease`.

## 5. `Azure-Winged Magpie` GitHub App and Bidirectional Synchronization

Bidirectional synchronization between `gosuda/0` and standalone `gosuda/<repo>` mirrors authenticates through the `Azure-Winged Magpie` GitHub App. Table 2 specifies the required permissions, repository variables, and secrets.

| Configuration Item | Location | Value / Permission |
| :--- | :--- | :--- |
| Repository Permission: `Contents` | GitHub App settings | `Read and write` (pushes `refs/suda/*`, `refs/heads/main`, and mirror branches) |
| Repository Permission: `Pull requests` | GitHub App settings | `Read and write` (reads inbound PR refs and updates mirror PR status) |
| Repository Permission: `Actions` | GitHub App settings | `Write` (dispatches `capy-inbound` workflow runs on `gosuda/0`) |
| Repository Permission: `Metadata` | GitHub App settings | `Read-only` |
| Webhook Events | GitHub App settings | `Push`, `Pull request` (pointed at the Cloudflare Worker `/webhook` endpoint) |
| Variable `MAGPIE_APP_ID` | `gosuda/0` Actions variables | Numeric GitHub App ID |
| Secret `MAGPIE_APP_PRIVATE_KEY` | `gosuda/0` Actions secrets | PEM private key for `Azure-Winged Magpie` |
| Secret `WEBHOOK_SECRET` | `gosuda/azure-magpie` Actions secrets | HMAC secret shared with the GitHub App webhook |
| Secret `CLOUDFLARE_API_TOKEN` | `gosuda/azure-magpie` Actions secrets | Cloudflare API token with Cloudflare Pages Edit permission |
| Variable `CLOUDFLARE_ACCOUNT_ID` | `gosuda/azure-magpie` Actions variables | Cloudflare account identifier for automated Pages deployment |

## 6. Cloudflare Pages Snapshot Redirector and Diff Generator (`//devtools/azure-magpie`)

The `//devtools/azure-magpie` project (`gosuda/azure-magpie`) serves the Diff Link Generator UI and resolves `refs/suda/*` and `refs/ws/*` at the edge. The `capy` synchronizer exports `//devtools/azure-magpie/.github/workflows/` to the public `gosuda/azure-magpie` mirror, where GitHub Actions executes monorepo landing, mirror synchronization, and Cloudflare Pages deployments on public runners. Table 3 lists the supported routes.

| Request Path | Target Redirect Location or Response |
| :--- | :--- |
| `GET /` or `GET /<path>` | `302` to `https://github.com/gosuda/0/tree/<suda-main-sha>/<path>` |
| `GET /blob/<path>` | `302` to `https://github.com/gosuda/0/blob/<suda-main-sha>/<path>` |
| `GET /commit` or `GET /commit/<ref>` | `302` to `https://github.com/gosuda/0/commit/<resolved-sha>` |
| `GET /history/<path>` | `302` to `https://github.com/gosuda/0/commits/<suda-history-sha>/<path>` |
| `GET /blame/<path>` | `302` to `https://github.com/gosuda/0/blame/<suda-history-sha>/<path>` |
| `GET /diff` | `200` HTML Diff Link Generator UI listing active `refs/ws/*` and `refs/heads/land/*` branches |
| `GET /diff/ws/<actor>/<id>` | `302` to two-dot compare `https://github.com/gosuda/0/compare/<suda-main-sha>..<ws-sha>` |
| `GET /diff/<base>..<head>` | `302` to two-dot compare `https://github.com/gosuda/0/compare/<base-sha>..<head-sha>` |
| `GET /refs.json` or `GET /sha` | `200` JSON payload containing `suda/main` and `suda/history` commit hashes |
| `POST /webhook` | Verifies `X-Hub-Signature-256`, clears ref cache, and dispatches `capy-inbound` on `gosuda/0` |

