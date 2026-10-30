# Azure-Winged Magpie (`gosuda/azure-magpie`)

`gosuda/azure-magpie` (`//devtools/azure-magpie` on `refs/suda/main`) provides the edge redirector, two-dot diff link generator, webhook dispatcher, and public GitHub Actions sync runner for the `gosuda/0` monorepo. Production traffic is served from Cloudflare Pages at [https://magpie.gosuda.org](https://magpie.gosuda.org).

## 1. HTTP Routes

`public/_worker.js` (compiled from `src/worker.ts`) resolves `refs/suda/main`, `refs/suda/history`, `refs/ws/*`, and `refs/heads/land/*` via the GitHub REST API and caches trunk SHA resolutions at the edge. Static assets (`index.html`, `styles.css`, `app.js`) are served from `public/`.

| Route | Method | Behavior |
| :--- | :--- | :--- |
| `/` or `/diff` | `GET` | Serves the web interface for generating comparison links, inspecting active workspaces, and browsing subtrees |
| `/tree` or `/tree/<path>` | `GET` | Redirects (`302`) to `https://github.com/gosuda/0/tree/<suda-main-sha>/<path>` |
| `/blob/<path>` | `GET` | Redirects (`302`) to `https://github.com/gosuda/0/blob/<suda-main-sha>/<path>` |
| `/commit` or `/commit/<ref>` | `GET` | Resolves `<ref>` (defaults to `refs/suda/main`) and redirects (`302`) to `https://github.com/gosuda/0/commit/<sha>` |
| `/history` or `/history/<path>` | `GET` | Redirects (`302`) to `https://github.com/gosuda/0/commits/<suda-history-sha>/<path>` |
| `/blame/<path>` | `GET` | Redirects (`302`) to `https://github.com/gosuda/0/blame/<suda-history-sha>/<path>` |
| `/diff/ws/<actor>/<id>` | `GET` | Resolves `refs/suda/main` and `refs/ws/<actor>/<id>`, then redirects (`302`) to the two-dot GitHub comparison URL |
| `/diff/<base>..<head>` | `GET` | Resolves `<base>` and `<head>` to commit SHAs and redirects (`302`) to `https://github.com/gosuda/0/compare/<base-sha>..<head-sha>` (append `?format=json` for JSON metadata) |
| `/api/refs` | `GET` | Returns JSON containing `suda/main`, `suda/history`, active `refs/ws/*` and `refs/heads/land/*` tips, and mounted `google3` projects |
| `/refs.json` or `/sha` | `GET` | Returns JSON containing `repo`, `suda/main`, and `suda/history` commit SHAs |
| `/api/land?ref=<land-ref>` | `GET` | Verifies `<land-ref>` on `gosuda/0` and dispatches `suda-land` on `gosuda/azure-magpie` |
| `/webhook` | `POST` | Validates `X-Hub-Signature-256` (`WEBHOOK_SECRET`), invalidates cached refs, and dispatches `suda-land` or `capy-inbound` on `gosuda/azure-magpie` |

## 2. Public Runner Workflows and Secrets

Because `gosuda/0` is a private repository without CI workflows, `gosuda/azure-magpie` executes both Cloudflare Pages deployments and monorepo synchronization on public GitHub Actions runners:

- `.github/workflows/deploy.yml`: Runs `bun test`, builds `public/app.js` and `public/_worker.js`, syncs worker secrets, and deploys `public/` to Cloudflare Pages (`azure-magpie`).
- `.github/workflows/suda-sync.yml`: Handles `suda-land` and `capy-inbound` repository dispatch events by minting an installation token from the `Azure-Winged Magpie` GitHub App, checking out the `gosuda/0` control plane, and executing `.github/scripts/land.sh` or `.github/scripts/sync-inbound.sh`.

| Name | Kind | Purpose |
| :--- | :--- | :--- |
| `MAGPIE_APP_ID` | Secret or Variable | Numeric GitHub App ID for `Azure-Winged Magpie` |
| `MAGPIE_APP_PRIVATE_KEY` | Secret | RSA private key (PEM) used to sign GitHub App JWTs in Actions and Cloudflare Pages |
| `WEBHOOK_SECRET` | Secret | HMAC-SHA256 secret used by `/webhook` to verify `X-Hub-Signature-256` |
| `CLOUDFLARE_API_TOKEN` | Secret | Cloudflare API token with Pages Edit permission |
| `CLOUDFLARE_ACCOUNT_ID` | Variable | Target Cloudflare account ID for Pages deployment |

## 3. Local Development

Build and test require [Bun](https://bun.sh) v1.2+.

```bash
bun test
bun run build
npx wrangler pages deploy public --project-name=azure-magpie
```
