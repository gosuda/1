# Azure-Winged Magpie (`gosuda/azure-magpie`)

`gosuda/azure-magpie` (`//devtools/azure-magpie` on `refs/suda/main`) provides the edge redirector, two-dot diff link generator, and webhook wake forwarder for the `gosuda/0` monorepo. It never lands, exports, or dispatches work: the self-hosted lander does that (`docs/hardening-plan.md`). `gosuda/azure-magpie` is an ordinary public mirror. Production traffic is served from Cloudflare Pages at [https://magpie.gosuda.org](https://magpie.gosuda.org).

## 1. HTTP Routes

`public/_worker.js` (compiled from `src/worker.ts`) resolves trunk refs (`refs/suda/main`, `refs/suda/history`) on `gosuda/0` and workspace refs (`refs/ws/*`, `refs/heads/land/*`, `refs/heads/review/*`) on the user repository `gosuda/1` (`USER_REPO`) via the GitHub REST API and caches trunk SHA resolutions at the edge. Static assets (`index.html`, `styles.css`, `app.js`) are served from `public/`.

| Route | Method | Behavior |
| :--- | :--- | :--- |
| `/` or `/diff` | `GET` | Serves the web interface for generating comparison links, inspecting active workspaces, and browsing subtrees |
| `/tree` or `/tree/<path>` | `GET` | Redirects (`302`) to `https://github.com/gosuda/0/tree/<suda-main-sha>/<path>` |
| `/blob/<path>` | `GET` | Redirects (`302`) to `https://github.com/gosuda/0/blob/<suda-main-sha>/<path>` |
| `/commit` or `/commit/<ref>` | `GET` | Resolves `<ref>` (defaults to `refs/suda/main`) and redirects (`302`) to `https://github.com/gosuda/0/commit/<sha>` |
| `/history` or `/history/<path>` | `GET` | Redirects (`302`) to `https://github.com/gosuda/0/commits/<suda-history-sha>/<path>` |
| `/blame/<path>` | `GET` | Redirects (`302`) to `https://github.com/gosuda/0/blame/<suda-history-sha>/<path>` |
| `/diff/ws/<actor>/<id>` | `GET` | Resolves `refs/suda/main` and `refs/ws/<actor>/<id>`, then redirects (`302`) to the two-dot GitHub comparison URL on `gosuda/1` (which carries a copy of the trunk refs) |
| `/diff/<base>..<head>` | `GET` | Resolves `<base>` and `<head>` to commit SHAs and redirects (`302`) to `https://github.com/gosuda/1/compare/<base-sha>..<head-sha>` (append `?format=json` for JSON metadata) |
| `/api/refs` | `GET` | Returns JSON containing `suda/main`, `suda/history`, active `refs/ws/*` and `refs/heads/land/*` tips, and mounted `google3` projects |
| `/refs.json` or `/sha` | `GET` | Returns JSON containing `repo`, `suda/main`, and `suda/history` commit SHAs |
| `/webhook` | `POST` | Validates `X-Hub-Signature-256` (`WEBHOOK_SECRET`), invalidates cached refs, and POSTs a wake signal to the lander (`LANDER_WAKE_URL`, signed with `LANDER_WAKE_SECRET`). The GitHub payload is not forwarded: the lander re-derives all work from repository state and also polls, so a missed wake only delays the next cycle |

## 2. Deployment and Secrets

- `.github/workflows/deploy.yml`: Runs `bun test`, builds `public/app.js` and `public/_worker.js`, syncs worker secrets, and deploys `public/` to Cloudflare Pages (`azure-magpie`).

Put the site behind Cloudflare Access and exempt only `/webhook`. The worker holds read-only GitHub access.

| Name | Kind | Purpose |
| :--- | :--- | :--- |
| `MAGPIE_APP_ID` | Secret or Variable | Numeric GitHub App ID used for read-only API calls |
| `MAGPIE_APP_PRIVATE_KEY` | Secret | RSA private key (PEM) used to sign GitHub App JWTs in Cloudflare Pages |
| `WEBHOOK_SECRET` | Secret | HMAC-SHA256 secret used by `/webhook` to verify `X-Hub-Signature-256` |
| `LANDER_WAKE_URL` | Secret or Variable | Lander `/wake` endpoint |
| `LANDER_WAKE_SECRET` | Secret | HMAC-SHA256 secret shared with the lander (`LANDER_WAKE_SECRET`) |
| `CLOUDFLARE_API_TOKEN` | Secret | Cloudflare API token with Pages Edit permission |
| `CLOUDFLARE_ACCOUNT_ID` | Variable | Target Cloudflare account ID for Pages deployment |

## 3. Local Development

Build and test require [Bun](https://bun.sh) v1.2+.

```bash
bun test
bun run build
npx wrangler pages deploy public --project-name=azure-magpie
```
