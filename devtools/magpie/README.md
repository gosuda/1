# Azure-Winged Magpie (`gosuda/magpie`)

Azure-Winged Magpie is the Scandinavian SaaS diff link generator, snapshot explorer, and edge webhook router for the `gosuda/0` CAS monorepo. The project compiles Vanilla TypeScript (`src/app.ts` and `src/worker.ts`) and Pure CSS (`public/styles.css`) into a Cloudflare Pages bundle (`public/`).

## 1. Architecture and Route Table

Cloudflare Pages serves the static web interface from `public/` while executing `public/_worker.js` at the edge to resolve `refs/suda/*` and `refs/ws/*`. Table 1 lists the HTTP routes handled by the application.

| Route | Method | Behavior |
| :--- | :--- | :--- |
| `/` or `/diff` | `GET` | Serves the Scandinavian SaaS Diff Link Generator and Snapshot Studio UI |
| `/api/refs` | `GET` | Returns `refs/suda/main`, `refs/suda/history`, active `refs/ws/*` branches, and mounted projects as JSON |
| `/diff/ws/<actor>/<id>` | `GET` | Redirects (`302`) to the two-dot GitHub comparison `https://github.com/gosuda/0/compare/<suda-main-sha>..<ws-sha>` |
| `/diff/<base>..<head>` | `GET` | Resolves both references to commit SHAs and redirects (`302`) to the GitHub comparison view |
| `/tree/<path>` | `GET` | Redirects (`302`) to `<path>` inside the latest `refs/suda/main` commit on GitHub |
| `/history/<path>` | `GET` | Redirects (`302`) to the commit log for `<path>` on `refs/suda/history` |
| `/blame/<path>` | `GET` | Redirects (`302`) to the file blame view for `<path>` on `refs/suda/history` |
| `/webhook` | `POST` | Verifies `X-Hub-Signature-256` and dispatches `capy-inbound` on `gosuda/0` |

## 2. Build and Test Procedure

Run the unit test suite and compile the browser and edge bundles using Bun.

1. Execute the unit test suite in `src/worker.test.ts`.
2. Compile `src/app.ts` and `src/worker.ts` into `public/app.js` and `public/_worker.js`.
3. Deploy the `public/` directory to Cloudflare Pages.

```bash
bun test
bun run build
npx wrangler pages deploy public --project-name=gosuda-magpie
```
