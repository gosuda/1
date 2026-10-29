export interface AssetsFetcher {
  fetch(request: Request | string): Promise<Response>;
}

export interface Env {
  ASSETS?: AssetsFetcher;
  GITHUB_REPO?: string;
  GITHUB_TOKEN?: string;
  MAGPIE_APP_ID?: string;
  MAGPIE_INSTALLATION_ID?: string;
  MAGPIE_APP_PRIVATE_KEY?: string;
  CAPY_APP_ID?: string;
  CAPY_INSTALLATION_ID?: string;
  CAPY_APP_PRIVATE_KEY?: string;
  WEBHOOK_SECRET?: string;
  CACHE_TTL_SECONDS?: string;
}

export interface SudaRefs {
  mainSha: string;
  historySha: string;
  fetchedAtMs: number;
}

export interface DiffResolution {
  repo: string;
  baseRef: string;
  baseSha: string;
  headRef: string;
  headSha: string;
  operator: ".." | "...";
  compareUrl: string;
  shortPath: string;
}

interface CachedAppToken {
  token: string;
  expiresAtMs: number;
}

const USER_AGENT = "azure-winged-magpie-worker";

const STATIC_UI_PATHS = new Set(["/", "/index.html", "/styles.css", "/app.js", "/favicon.ico"]);

const MOUNTED_PROJECTS = [
  { prefix: "web/gosuda", mirror: "gosuda/website" },
  { prefix: "web/securl", mirror: "gosuda/securl" },
  { prefix: "net/ivnp", mirror: "gosuda/IVNP" },
  { prefix: "net/portalite", mirror: "gosuda/portalite" },
  { prefix: "ipc/hqq/HQQ", mirror: "gosuda/HQQ" },
  { prefix: "ipc/hqq/hqq-sqlite3", mirror: "gosuda/hqq-sqlite3" },
  { prefix: "ipc/hqq/hqq-zstd", mirror: "gosuda/hqq-zstd" },
  { prefix: "devtools/azure-magpie", mirror: "gosuda/azure-magpie" },
];

let cachedRefs: SudaRefs | null = null;
let cachedInstallationId: string | null = null;
let cachedInstallationToken: CachedAppToken | null = null;

export default {
  async fetch(request: Request, env: Env): Promise<Response> {
    const url = new URL(request.url);
    const repo = env.GITHUB_REPO || "gosuda/0";

    if (request.method === "POST" && url.pathname === "/webhook") {
      return handleGithubWebhook(request, env, repo);
    }

    if (request.method !== "GET" && request.method !== "HEAD") {
      return new Response("Method Not Allowed", { status: 405 });
    }

    if (env.ASSETS && STATIC_UI_PATHS.has(url.pathname)) {
      return env.ASSETS.fetch(request);
    }

    try {
      const refs = await resolveSudaRefs(env, repo);

      if (url.pathname === "/refs.json" || url.pathname === "/sha") {
        return jsonResponse({
          repo,
          "suda/main": refs.mainSha,
          "suda/history": refs.historySha,
        });
      }

      if (url.pathname === "/api/refs") {
        const workspaces = await listActiveWorkspaceRefs(env, repo);
        return jsonResponse({
          repo,
          "suda/main": refs.mainSha,
          "suda/history": refs.historySha,
          workspaces,
          projects: MOUNTED_PROJECTS,
        });
      }

      if (
        url.pathname === "/diff" ||
        url.pathname === "/diff/" ||
        url.pathname.startsWith("/diff/") ||
        url.pathname.startsWith("/compare/")
      ) {
        return handleDiffRequest(request, url, env, repo, refs);
      }

      if (url.pathname.startsWith("/commit/")) {
        const refSpec = decodeURIComponent(url.pathname.slice("/commit/".length));
        const sha = await resolveRefSha(env, repo, refSpec, refs);
        return redirectResponse(`https://github.com/${repo}/commit/${sha}`);
      }

      const targetUrl = buildRedirectLocation(url.pathname, repo, refs);
      return redirectResponse(targetUrl);
    } catch (err) {
      const message = err instanceof Error ? err.message : "upstream error";
      const status = message.includes("not found") ? 404 : 502;
      return jsonResponse({ error: message }, status);
    }
  },
};

export function buildRedirectLocation(pathname: string, repo: string, refs: SudaRefs): string {
  const base = `https://github.com/${repo}`;
  const trimmed = pathname.replace(/^\/+|\/+$/g, "");

  if (trimmed === "" || trimmed === "tree") {
    return `${base}/tree/${refs.mainSha}`;
  }
  if (trimmed.startsWith("tree/")) {
    return `${base}/tree/${refs.mainSha}/${trimmed.slice("tree/".length)}`;
  }
  if (trimmed.startsWith("blob/")) {
    return `${base}/blob/${refs.mainSha}/${trimmed.slice("blob/".length)}`;
  }
  if (trimmed === "commit") {
    return `${base}/commit/${refs.mainSha}`;
  }
  if (trimmed === "history") {
    return `${base}/commits/${refs.historySha}`;
  }
  if (trimmed.startsWith("history/")) {
    return `${base}/commits/${refs.historySha}/${trimmed.slice("history/".length)}`;
  }
  if (trimmed.startsWith("blame/")) {
    return `${base}/blame/${refs.historySha}/${trimmed.slice("blame/".length)}`;
  }
  return `${base}/tree/${refs.mainSha}/${trimmed}`;
}

export function parseDiffSpec(
  pathname: string,
  searchParams: URLSearchParams,
): { baseRef: string; headRef: string; operator: ".." | "..." } | null {
  const qHead = (searchParams.get("head") || "").trim();
  const qBase = (searchParams.get("base") || "suda/main").trim();
  const qMode = searchParams.get("mode") === "3dot" ? "..." : "..";

  const trimmed = pathname.replace(/^\/+|\/+$/g, "");
  if (trimmed === "diff" || trimmed === "compare") {
    if (!qHead) {
      return null;
    }
    return { baseRef: qBase || "suda/main", headRef: qHead, operator: qMode };
  }

  const rawSpec = trimmed.startsWith("diff/")
    ? trimmed.slice("diff/".length)
    : trimmed.startsWith("compare/")
      ? trimmed.slice("compare/".length)
      : "";

  if (!rawSpec) {
    return null;
  }

  if (rawSpec.includes("...")) {
    const idx = rawSpec.indexOf("...");
    const left = rawSpec.slice(0, idx) || "suda/main";
    const right = rawSpec.slice(idx + 3);
    if (!right) {
      return null;
    }
    return { baseRef: left, headRef: right, operator: "..." };
  }

  if (rawSpec.includes("..")) {
    const idx = rawSpec.indexOf("..");
    const left = rawSpec.slice(0, idx) || "suda/main";
    const right = rawSpec.slice(idx + 2);
    if (!right) {
      return null;
    }
    return { baseRef: left, headRef: right, operator: ".." };
  }

  return {
    baseRef: qBase || "suda/main",
    headRef: rawSpec,
    operator: qMode,
  };
}

async function handleDiffRequest(
  request: Request,
  url: URL,
  env: Env,
  repo: string,
  refs: SudaRefs,
): Promise<Response> {
  const spec = parseDiffSpec(url.pathname, url.searchParams);
  if (!spec) {
    if (env.ASSETS) {
      const indexUrl = new URL("/index.html", url.origin);
      return env.ASSETS.fetch(new Request(indexUrl.toString(), request));
    }
    const activeRefs = await listActiveWorkspaceRefs(env, repo);
    const html = renderFallbackDiffHtml(url.origin, repo, refs, activeRefs);
    return new Response(html, {
      status: 200,
      headers: {
        "content-type": "text/html; charset=utf-8",
        "cache-control": "no-store",
      },
    });
  }

  const baseSha = await resolveRefSha(env, repo, spec.baseRef, refs);
  const headSha = await resolveRefSha(env, repo, spec.headRef, refs);
  const compareUrl = `https://github.com/${repo}/compare/${baseSha}${spec.operator}${headSha}`;
  const shortPath =
    spec.baseRef === "suda/main" && spec.operator === ".."
      ? `/diff/${spec.headRef}`
      : `/diff/${spec.baseRef}${spec.operator}${spec.headRef}`;

  if (url.searchParams.get("format") === "json") {
    const payload: DiffResolution = {
      repo,
      baseRef: spec.baseRef,
      baseSha,
      headRef: spec.headRef,
      headSha,
      operator: spec.operator,
      compareUrl,
      shortPath,
    };
    return jsonResponse(payload);
  }

  return redirectResponse(compareUrl);
}

export async function resolveRefSha(
  env: Env,
  repo: string,
  refSpec: string,
  refs: SudaRefs,
): Promise<string> {
  const cleaned = refSpec.replace(/^\/+|\/+$/g, "");
  if (cleaned === "suda/main" || cleaned === "refs/suda/main" || cleaned === "main") {
    return refs.mainSha;
  }
  if (cleaned === "suda/history" || cleaned === "refs/suda/history" || cleaned === "history") {
    return refs.historySha;
  }
  if (/^[0-9a-f]{7,40}$/i.test(cleaned)) {
    return cleaned;
  }

  const candidates: string[] = [];
  if (cleaned.startsWith("refs/")) {
    candidates.push(cleaned.slice("refs/".length));
  } else if (cleaned.startsWith("ws/") || cleaned.startsWith("suda/")) {
    candidates.push(cleaned);
  } else if (cleaned.startsWith("land/")) {
    candidates.push(`heads/${cleaned}`);
  } else if (cleaned.startsWith("heads/")) {
    candidates.push(cleaned);
  } else if (cleaned.startsWith("pr/")) {
    candidates.push(`pull/${cleaned.slice("pr/".length)}/head`);
  } else {
    candidates.push(`ws/${cleaned}`, `heads/land/${cleaned}`, `heads/${cleaned}`);
  }

  const headers = await buildGithubHeaders(env);
  for (const candidate of candidates) {
    const apiUrl = `https://api.github.com/repos/${repo}/git/ref/${candidate}`;
    const resp = await fetch(apiUrl, { headers });
    if (resp.ok) {
      const data = (await resp.json()) as { object?: { sha?: string } };
      if (data.object?.sha) {
        return data.object.sha;
      }
    }
  }

  throw new Error(`ref '${refSpec}' not found on ${repo}`);
}

async function listActiveWorkspaceRefs(
  env: Env,
  repo: string,
): Promise<Array<{ ref: string; label: string; sha: string }>> {
  const headers = await buildGithubHeaders(env);
  const results: Array<{ ref: string; label: string; sha: string }> = [];

  for (const prefix of ["ws/", "heads/land/"]) {
    const resp = await fetch(`https://api.github.com/repos/${repo}/git/matching-refs/${prefix}`, {
      headers,
    });
    if (!resp.ok) {
      continue;
    }
    const items = (await resp.json()) as Array<{ ref: string; object?: { sha?: string } }>;
    for (const item of items) {
      if (item.ref && item.object?.sha) {
        const label = item.ref.replace(/^refs\/(heads\/)?/, "");
        results.push({ ref: item.ref, label, sha: item.object.sha });
      }
    }
  }
  return results;
}

function renderFallbackDiffHtml(
  origin: string,
  repo: string,
  refs: SudaRefs,
  activeRefs: Array<{ ref: string; label: string; sha: string }>,
): string {
  const rows = activeRefs
    .map(
      (item) =>
        `<li><code>${encodeHtml(item.ref)}</code> (<a href="${encodeHtml(origin)}/diff/${encodeHtml(item.label)}">Diff vs suda/main</a>)</li>`,
    )
    .join("");
  return `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>${encodeHtml(repo)} Snapshot &amp; Diff Link Generator</title></head><body><h1>${encodeHtml(repo)} Snapshot &amp; Diff Link Generator</h1><p>suda/main: <code>${refs.mainSha}</code></p><ul>${rows}</ul></body></html>`;
}

function encodeHtml(str: string): string {
  return str
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;");
}

function redirectResponse(targetUrl: string): Response {
  return new Response(null, {
    status: 302,
    headers: {
      location: targetUrl,
      "cache-control": "no-store",
    },
  });
}

function jsonResponse(payload: unknown, status = 200): Response {
  return new Response(JSON.stringify(payload), {
    status,
    headers: {
      "content-type": "application/json; charset=utf-8",
      "cache-control": "no-store",
    },
  });
}

async function buildGithubHeaders(env: Env): Promise<Record<string, string>> {
  const token = await resolveGithubAuthToken(env);
  const headers: Record<string, string> = {
    accept: "application/vnd.github+json",
    "user-agent": USER_AGENT,
    "x-github-api-version": "2022-11-28",
  };
  if (token) {
    headers.authorization = `Bearer ${token}`;
  }
  return headers;
}

async function resolveSudaRefs(env: Env, repo: string): Promise<SudaRefs> {
  const ttlMs = (Number(env.CACHE_TTL_SECONDS) || 5) * 1000;
  const now = Date.now();
  if (cachedRefs && now - cachedRefs.fetchedAtMs < ttlMs) {
    return cachedRefs;
  }

  const headers = await buildGithubHeaders(env);
  const apiUrl = `https://api.github.com/repos/${repo}/git/matching-refs/suda/`;
  const resp = await fetch(apiUrl, { headers });
  if (!resp.ok) {
    throw new Error(`GitHub API ${resp.status}: unable to read refs/suda/* on ${repo}`);
  }

  const items = (await resp.json()) as Array<{ ref: string; object: { sha: string } }>;
  let mainSha = "";
  let historySha = "";
  for (const item of items) {
    if (item.ref === "refs/suda/main") {
      mainSha = item.object.sha;
    } else if (item.ref === "refs/suda/history") {
      historySha = item.object.sha;
    }
  }
  if (!mainSha) {
    throw new Error(`refs/suda/main not found on ${repo}`);
  }
  if (!historySha) {
    historySha = mainSha;
  }

  cachedRefs = { mainSha, historySha, fetchedAtMs: now };
  return cachedRefs;
}

async function resolveGithubAuthToken(env: Env): Promise<string> {
  if (env.GITHUB_TOKEN) {
    return env.GITHUB_TOKEN;
  }
  const appId = env.MAGPIE_APP_ID || env.CAPY_APP_ID || "";
  const privateKey = env.MAGPIE_APP_PRIVATE_KEY || env.CAPY_APP_PRIVATE_KEY || "";
  if (!appId || !privateKey) {
    return "";
  }
  const now = Date.now();
  if (cachedInstallationToken && cachedInstallationToken.expiresAtMs - now > 60_000) {
    return cachedInstallationToken.token;
  }

  const jwt = await signGithubAppJwt(appId, privateKey);
  let installationId =
    env.MAGPIE_INSTALLATION_ID || env.CAPY_INSTALLATION_ID || cachedInstallationId || "";

  if (!installationId) {
    const repo = env.GITHUB_REPO || "gosuda/0";
    const instResp = await fetch(`https://api.github.com/repos/${repo}/installation`, {
      headers: {
        accept: "application/vnd.github+json",
        authorization: `Bearer ${jwt}`,
        "user-agent": USER_AGENT,
        "x-github-api-version": "2022-11-28",
      },
    });
    if (!instResp.ok) {
      throw new Error(`GitHub App installation lookup failed with status ${instResp.status}`);
    }
    const instData = (await instResp.json()) as { id: number };
    installationId = String(instData.id);
    cachedInstallationId = installationId;
  }

  const resp = await fetch(
    `https://api.github.com/app/installations/${installationId}/access_tokens`,
    {
      method: "POST",
      headers: {
        accept: "application/vnd.github+json",
        authorization: `Bearer ${jwt}`,
        "user-agent": USER_AGENT,
        "x-github-api-version": "2022-11-28",
      },
    },
  );
  if (!resp.ok) {
    throw new Error(`GitHub App token exchange failed with status ${resp.status}`);
  }
  const data = (await resp.json()) as { token: string; expires_at: string };
  cachedInstallationToken = {
    token: data.token,
    expiresAtMs: Date.parse(data.expires_at),
  };
  return data.token;
}

async function handleGithubWebhook(request: Request, env: Env, monorepo: string): Promise<Response> {
  if (!env.WEBHOOK_SECRET) {
    return new Response("Webhook secret not configured", { status: 500 });
  }
  const sigHeader = request.headers.get("x-hub-signature-256") || "";
  const rawBody = new Uint8Array(await request.arrayBuffer());
  const valid = await verifyWebhookSignature(rawBody, sigHeader, env.WEBHOOK_SECRET);
  if (!valid) {
    return new Response("Invalid signature", { status: 401 });
  }

  cachedRefs = null;

  const event = request.headers.get("x-github-event") || "";
  const payload = JSON.parse(new TextDecoder().decode(rawBody)) as {
    ref?: string;
    repository?: { full_name?: string; name?: string };
    head_commit?: { message?: string };
    action?: string;
    number?: number;
  };

  const fullName = payload.repository?.full_name || "";
  const shortName = payload.repository?.name || "";
  if (!fullName || fullName === monorepo) {
    return jsonResponse({ status: "cache-cleared" });
  }

  let extRef = "";
  if (event === "push" && payload.ref === "refs/heads/main") {
    const msg = payload.head_commit?.message || "";
    if (msg.includes("GitOrigin-RevId:")) {
      return jsonResponse({ status: "ignored-echo" });
    }
    extRef = "refs/heads/main";
  } else if (
    event === "pull_request" &&
    (payload.action === "opened" || payload.action === "synchronize")
  ) {
    extRef = `refs/pull/${payload.number}/head`;
  } else {
    return jsonResponse({ status: "ignored-event" });
  }

  const token = await resolveGithubAuthToken(env);
  if (!token) {
    return new Response("Missing GitHub token for repository_dispatch", { status: 500 });
  }

  const dispatchResp = await fetch(`https://api.github.com/repos/${monorepo}/dispatches`, {
    method: "POST",
    headers: {
      accept: "application/vnd.github+json",
      authorization: `Bearer ${token}`,
      "content-type": "application/json",
      "user-agent": USER_AGENT,
    },
    body: JSON.stringify({
      event_type: "capy-inbound",
      client_payload: {
        workflow: shortName,
        ref: extRef,
        mode: event === "push" ? "land" : "ws",
      },
    }),
  });

  return jsonResponse(
    { status: "dispatched", code: dispatchResp.status },
    dispatchResp.ok ? 200 : 502,
  );
}

async function verifyWebhookSignature(
  body: Uint8Array,
  signatureHeader: string,
  secret: string,
): Promise<boolean> {
  if (!signatureHeader.startsWith("sha256=")) {
    return false;
  }
  const sigHex = signatureHeader.slice("sha256=".length);
  if (sigHex.length % 2 !== 0) {
    return false;
  }
  const sigBytes = new Uint8Array(sigHex.length / 2);
  for (let i = 0; i < sigHex.length; i += 2) {
    sigBytes[i / 2] = parseInt(sigHex.slice(i, i + 2), 16);
  }
  const key = await crypto.subtle.importKey(
    "raw",
    new TextEncoder().encode(secret),
    { name: "HMAC", hash: "SHA-256" },
    false,
    ["verify"],
  );
  return crypto.subtle.verify("HMAC", key, sigBytes, body);
}

async function signGithubAppJwt(appId: string, pemKey: string): Promise<string> {
  const nowSec = Math.floor(Date.now() / 1000);
  const header = base64UrlEncodeString(JSON.stringify({ alg: "RS256", typ: "JWT" }));
  const payload = base64UrlEncodeString(
    JSON.stringify({
      iat: nowSec - 60,
      exp: nowSec + 540,
      iss: appId,
    }),
  );
  const signingInput = `${header}.${payload}`;
  const keyData = pemToArrayBuffer(pemKey);
  const cryptoKey = await crypto.subtle.importKey(
    "pkcs8",
    keyData,
    { name: "RSASSA-PKCS1-v1_5", hash: "SHA-256" },
    false,
    ["sign"],
  );
  const sig = await crypto.subtle.sign(
    "RSASSA-PKCS1-v1_5",
    cryptoKey,
    new TextEncoder().encode(signingInput),
  );
  return `${signingInput}.${base64UrlEncodeBytes(new Uint8Array(sig))}`;
}

function pemToArrayBuffer(pem: string): ArrayBuffer {
  const isPkcs1 = pem.includes("BEGIN RSA PRIVATE KEY");
  const base64 = pem
    .replace(/-----BEGIN [A-Z0-9 ]+-----/g, "")
    .replace(/-----END [A-Z0-9 ]+-----/g, "")
    .replace(/\s+/g, "");
  const bin = atob(base64);
  const der = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) {
    der[i] = bin.charCodeAt(i);
  }
  if (!isPkcs1) {
    return der.buffer;
  }
  return wrapPkcs1ToPkcs8(der).buffer;
}

function wrapPkcs1ToPkcs8(pkcs1: Uint8Array): Uint8Array {
  const version = new Uint8Array([0x02, 0x01, 0x00]);
  const algId = new Uint8Array([
    0x30, 0x0d, 0x06, 0x09, 0x2a, 0x86, 0x48, 0x86, 0xf7, 0x0d, 0x01, 0x01, 0x01, 0x05, 0x00,
  ]);
  const octetString = encodeAsn1Tag(0x04, pkcs1);
  const body = new Uint8Array(version.length + algId.length + octetString.length);
  body.set(version, 0);
  body.set(algId, version.length);
  body.set(octetString, version.length + algId.length);
  return encodeAsn1Tag(0x30, body);
}

function encodeAsn1Tag(tag: number, content: Uint8Array): Uint8Array {
  const lenBytes = encodeAsn1Length(content.length);
  const out = new Uint8Array(1 + lenBytes.length + content.length);
  out[0] = tag;
  out.set(lenBytes, 1);
  out.set(content, 1 + lenBytes.length);
  return out;
}

function encodeAsn1Length(len: number): Uint8Array {
  if (len < 0x80) {
    return new Uint8Array([len]);
  }
  const bytes: number[] = [];
  let rem = len;
  while (rem > 0) {
    bytes.unshift(rem & 0xff);
    rem >>= 8;
  }
  return new Uint8Array([0x80 | bytes.length, ...bytes]);
}

function base64UrlEncodeString(input: string): string {
  return base64UrlEncodeBytes(new TextEncoder().encode(input));
}

function base64UrlEncodeBytes(bytes: Uint8Array): string {
  let binary = "";
  for (let i = 0; i < bytes.length; i++) {
    binary += String.fromCharCode(bytes[i]);
  }
  return btoa(binary).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/g, "");
}
