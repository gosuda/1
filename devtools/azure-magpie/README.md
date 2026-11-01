# Azure-Winged Magpie (`gosuda/azure-magpie`)

`gosuda/azure-magpie` (`//devtools/azure-magpie` on `refs/suda/main`) is the web front of the `gosuda/0` monorepo flow: it shows changes and their patchsets, workspaces, trunk snapshots, and mounted projects, generates diff links, and forwards webhook wake signals to the lander. It links only to `gosuda/1` and never lands, exports, or dispatches work: the self-hosted lander does that (`docs/hardening-plan.md`). `gosuda/azure-magpie` is an ordinary public mirror. Production traffic is served from Cloudflare Pages at [https://magpie.gosuda.org](https://magpie.gosuda.org).

## 1. Pages

The UI is a single-page app with real paths (History API). Deep links load directly; back and forward work. Everything is self-contained: system font stacks, no CDN requests, light and dark themes (follows `prefers-color-scheme`, with a toggle remembered in `localStorage`).

| Path | Page |
| :--- | :--- |
| `/` | **Overview**: trunk snapshots (`suda/main`, `suda/history`), change and workspace counts, recent changes, active workspaces, mounted projects, and the workspace command builder. The nav items **Workspaces** (`/#workspaces`), **Projects** (`/#projects`), and the builder (`/#cli`) jump to sections of this page. |
| `/changes` | **Changes**: one row per change (subject, Change-Id, author, patchsets, review, last update). Filter in place. Embargoed changes are never listed. |
| `/changes/<change-id>` | **Change detail**: header with the full Change-Id and review link, the landing command (`git ws-sync && git ws-land`), the patchset table (PS1..n, commit, trunk base, published date, files, `+`/`−`, approvals and approval carry-over), and the **Compare** view. |
| `/changes/<N>` | The same detail for review pull request `#N` (its head commit's Change-Id). |
| `/diff` | **Diff links**: the two-dot link generator (base, head, comparison operator, short and GitHub links, Markdown, resolve exact SHAs) and a pick list of active workspaces. |

**Compare** (on a change detail page) selects two patchsets, or `Base` (the trunk snapshot a patchset sits on) against a patchset. `Base` shows that patchset's patch. Two patchsets are compared file by file with the interdiff rules below: a banner says whether the patch is identical (approvals on the earlier one carry over) or how many files changed. Changed files show a line-level diff of the two patch texts, like `git range-diff`. Diff bodies render when their block is opened.

The UI is built from `src/app.ts` (router, shell, delegated handlers) and `src/ui/*.ts` (pages). Pages build HTML strings; every dynamic value is escaped.

## 2. HTTP Routes

`public/_worker.js` (compiled from `src/worker.ts`) resolves trunk refs (`refs/suda/main`, `refs/suda/history`, copies the lander keeps current) and workspace refs (`refs/ws/*`, `refs/heads/land/*`, `refs/heads/review/*`) on the user repository `gosuda/1` (`GITHUB_REPO`) via the GitHub REST API and caches trunk SHA resolutions at the edge. Static assets (`index.html`, `styles.css`, `app.js`) are served from `public/` through the Pages `ASSETS` binding.

| Route | Method | Behavior |
| :--- | :--- | :--- |
| `/`, `/changes`, `/changes/<anything>` | `GET` | Serves the UI shell (`index.html` from `ASSETS`); the client router renders the page. Without an `ASSETS` binding, `/changes/...` answers `404`. |
| `/diff` | `GET` | Serves the diff link generator. `/diff/<spec>` redirects (see below). |
| `/tree` or `/tree/<path>` | `GET` | Redirects (`302`) to `https://github.com/gosuda/1/tree/<suda-main-sha>/<path>` |
| `/head` or `/head/<path>` | `GET` | Redirects (`302`) to `https://github.com/gosuda/1/tree/<suda-history-sha>/<path>`: the tree view at the head of `refs/suda/history` |
| `/blob/<path>` | `GET` | Redirects (`302`) to `https://github.com/gosuda/1/blob/<suda-main-sha>/<path>` |
| `/commit` or `/commit/<ref>` | `GET` | Resolves `<ref>` (defaults to `refs/suda/main`) and redirects (`302`) to `https://github.com/gosuda/1/commit/<sha>` |
| `/history` or `/history/<path>` | `GET` | Redirects (`302`) to `https://github.com/gosuda/1/commits/<suda-history-sha>/<path>` |
| `/blame/<path>` | `GET` | Redirects (`302`) to `https://github.com/gosuda/1/blame/<suda-history-sha>/<path>` |
| `/diff/ws/<actor>/<id>` | `GET` | Resolves `refs/suda/main` and `refs/ws/<actor>/<id>`, then redirects (`302`) to the two-dot GitHub comparison URL on `gosuda/1` (which carries a copy of the trunk refs) |
| `/diff/<base>..<head>` | `GET` | Resolves `<base>` and `<head>` to commit SHAs and redirects (`302`) to `https://github.com/gosuda/1/compare/<base-sha>..<head-sha>` (append `?format=json` for JSON metadata) |
| (hidden refs) | any | References with a path segment `sec` or starting `sec-` (embargoed security work, `docs/security-embargo.md`) are never listed and every route answers `404` for them without contacting GitHub |
| `/api/refs` | `GET` | Returns JSON containing `suda/main`, `suda/history`, active `refs/ws/*` (excluding patchsets under `refs/ws/cl/*`, which are changes) and `refs/heads/land/*` and `refs/heads/review/*` tips, and mounted `google3` projects |
| `/api/changes` | `GET` | Changes, newest first. See section 3. |
| `/api/changes/<change-id>` | `GET` | Detail of one change: patchsets, files, review, approvals. See section 3. |
| `/api/changes/<N>` | `GET` | Resolves review pull request `#N` to its head commit's Change-Id, then returns the detail above. `404` JSON when that cannot be resolved. |
| `/refs.json` or `/sha` | `GET` | Returns JSON containing `repo`, `suda/main`, and `suda/history` commit SHAs |
| `/webhook` | `POST` | Validates `X-Hub-Signature-256` (`WEBHOOK_SECRET`), invalidates cached refs and the change listing, and POSTs a wake signal to the lander (`LANDER_WAKE_URL`, signed with `LANDER_WAKE_SECRET`). The GitHub payload is not forwarded: the lander re-derives all work from repository state and also polls, so a missed wake only delays the next cycle |

## 3. Changes API

A **change** is every `refs/ws/cl/<change-id>/<tip-sha>` ref that shares one `Change-Id` (`I` + 40 lowercase hex). Each ref is one **patchset**; its patch is `root..tip`, where `root` is the single parentless trunk snapshot the patchset sits on. The API reads only `gosuda/1`.

**`GET /api/changes`** returns `{ repo, changes, truncated, omittedChanges, note, commitLookupLimit, reviewUnavailable }`. Each change has `id`, `subject` (of its newest patchset), `patchsetCount`, `updatedAt` (newest committer date), `author` (`name` from the first patchset; `login` from the review pull request's author when known), and `review` (`number`, `title`, `url`, `state` `open`/`closed`, `merged`) or `null`.

**`GET /api/changes/<change-id>`** returns `{ repo, id, subject, updatedAt, patchsets, review, reviews, approvals, reviewUnavailable }`:

- `patchsets`: oldest first by committer date, numbered `number` 1..n, each with `sha`, `ref`, `rootSha` (found by walking parents until the parentless commit; depth capped at 50), `date`, `subject`, `message`, `author`, `files` (from `compare/<root>...<tip>`: `filename`, `previous_filename`, `status`, `additions`, `deletions`, `patch`), `filesTruncated` (GitHub returns at most 300 files), `error`, and `carriesTo`. A patchset whose history is not single-rooted, or deeper than the limit, has `error` set and `files: null`; it is never dropped silently. `patch` is `null` for binary and very large files.
- `carriesTo`: later patchset numbers whose patch is identical (same file set, statuses, and hunks after ignoring hunk line numbers; whitespace is significant, like `git patch-id --verbatim`). A later patchset that is a clean rebase of an approved one (the same patch on a newer base) keeps that approval; any other edit needs a new approval. This is an estimate: the lander decides.
- `review`, `reviews` (`login`, `state`, `commitId`, `submittedAt`), and `approvals` (the latest decisive review per reviewer that is `APPROVED`, bound to the patchset whose sha it names, or `null`).

**Embargo.** Refs with an `sec` or `sec-*` segment are dropped before grouping. A change with any patchset whose subject starts with `sec-` is hidden from the list and answers `404` on detail, with the same message as an unknown change. Malformed ids answer `404` without any GitHub call.

**Degradation.** Pull request and review lookups need the GitHub App permission `pull_requests:read`. When GitHub refuses them (`403`/`404`), the API omits review data and says why in `reviewUnavailable`; changes and patchsets still load. Digit routes answer `404` JSON with the same reason.

**Listing window.** Review pull requests are matched among the 300 most recently updated pull requests of `gosuda/1` (open and closed; three pages of 100). A change whose review pull request falls outside that window shows no review and no approvals on the changes list and the detail. `/api/changes/<N>` still reads pull request `#N` directly, so it shows that review and its approvals on the change's detail.

**Caching and limits.** Commits and compares are immutable by sha: they are cached for seven days (Cache API when the runtime provides it, a bounded in-memory map otherwise). Ref listings use `CACHE_TTL_SECONDS` (default 5) and are dropped by the webhook; pull request lists are cached for 15 seconds. Each request may make at most 150 uncached GitHub reads for commits and compares. When the limit is reached, `/api/changes` returns the changes it could read with `truncated: true` and a `note`; a reload continues, because reads already made are cached.

## 4. Deployment and Secrets

- `.github/workflows/deploy.yml`: Runs `bun test`, builds `public/app.js` and `public/_worker.js`, syncs worker secrets, and deploys `public/` to Cloudflare Pages (`azure-magpie`).

Put the site behind Cloudflare Access and exempt only `/webhook`. The worker holds read-only GitHub access.

GitHub App permissions for the magpie App (installed on `gosuda/1`):

| Permission | Level | Used for |
| :--- | :--- | :--- |
| Contents | read (required) | Refs (`refs/suda/*`, `refs/ws/*`, `refs/heads/*`), commits, compares, `capy.star` |
| Pull requests | read (optional) | Review numbers and states on the changes list and detail, approvals, `/api/changes/<N>`. Without it the UI shows changes and patchsets, and says that review status is unavailable. |

| Name | Kind | Purpose |
| :--- | :--- | :--- |
| `MAGPIE_APP_ID` | Secret or Variable | Numeric GitHub App ID used for read-only API calls |
| `MAGPIE_APP_PRIVATE_KEY` | Secret | RSA private key (PEM) used to sign GitHub App JWTs in Cloudflare Pages |
| `WEBHOOK_SECRET` | Secret | HMAC-SHA256 secret used by `/webhook` to verify `X-Hub-Signature-256` |
| `LANDER_WAKE_URL` | Secret or Variable | Lander `/wake` endpoint |
| `LANDER_WAKE_SECRET` | Secret | HMAC-SHA256 secret shared with the lander (`LANDER_WAKE_SECRET`) |
| `CLOUDFLARE_API_TOKEN` | Secret | Cloudflare API token with Pages Edit permission |
| `CLOUDFLARE_ACCOUNT_ID` | Variable | Target Cloudflare account ID for Pages deployment |

## 5. Local Development

Build and test require [Bun](https://bun.sh) v1.2+.

```bash
bun test
bun run build
npx wrangler pages deploy public --project-name=azure-magpie
```

Sources: `src/worker.ts` (routes, refs, webhook), `src/changes.ts` (change API), `src/interdiff.ts` (pure patch comparison), `src/github.ts` and `src/github-auth.ts` (GitHub reads and App auth), `src/cache.ts` (read-through cache), `src/api-types.ts` (payload types shared with the UI), `src/app.ts` and `src/ui/*.ts` (UI). `public/app.js`, `public/_worker.js`, `public/index.html`, and `public/styles.css` are committed; `bun run build` regenerates the two bundles.
