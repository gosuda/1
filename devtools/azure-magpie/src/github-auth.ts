import { DEFAULT_REPO, USER_AGENT, type Env } from "./env";

interface CachedAppToken {
  token: string;
  expiresAtMs: number;
}

let cachedInstallationId: string | null = null;
let cachedInstallationToken: CachedAppToken | null = null;

export async function buildGithubHeaders(env: Env): Promise<Record<string, string>> {
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

export async function resolveGithubAuthToken(env: Env): Promise<string> {
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
    const repo = env.GITHUB_REPO || DEFAULT_REPO;
    const jwtHeaders = {
      accept: "application/vnd.github+json",
      authorization: `Bearer ${jwt}`,
      "user-agent": USER_AGENT,
      "x-github-api-version": "2022-11-28",
    };
    const instResp = await fetch(`https://api.github.com/repos/${repo}/installation`, {
      headers: jwtHeaders,
    });
    if (instResp.ok) {
      const instData = (await instResp.json()) as { id: number };
      installationId = String(instData.id);
    } else {
      const listResp = await fetch("https://api.github.com/app/installations", {
        headers: jwtHeaders,
      });
      if (!listResp.ok) {
        throw new Error(`GitHub App JWT auth failed (${listResp.status}): check MAGPIE_APP_ID and MAGPIE_APP_PRIVATE_KEY`);
      }
      const listData = (await listResp.json()) as Array<{ id: number; account?: { login?: string } }>;
      if (!Array.isArray(listData) || listData.length === 0) {
        throw new Error(`GitHub App is not installed on any organization or repository yet (install it on ${repo})`);
      }
      const owner = repo.split("/")[0]?.toLowerCase();
      const matched = listData.find((x) => x.account?.login?.toLowerCase() === owner) || listData[0];
      installationId = String(matched.id);
    }
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

export async function signGithubAppJwt(appId: string, pemKey: string): Promise<string> {
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
