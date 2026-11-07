// src/cache.ts
var MEMORY_LIMIT = 1000;
var CACHE_ORIGIN = "https://magpie-cache.invalid/v1/";
var memory = new Map;
function edgeCache() {
  const runtime = globalThis;
  return runtime.caches?.default ?? null;
}
async function readThrough(key, ttlSeconds, load) {
  const edge = edgeCache();
  if (edge) {
    const cacheUrl = `${CACHE_ORIGIN}${encodeURIComponent(key)}`;
    try {
      const hit = await edge.match(cacheUrl);
      if (hit) {
        return await hit.json();
      }
    } catch {}
    const value = await load();
    try {
      await edge.put(cacheUrl, new Response(JSON.stringify(value), {
        headers: {
          "content-type": "application/json; charset=utf-8",
          "cache-control": `public, max-age=${ttlSeconds}`
        }
      }));
    } catch {}
    return value;
  }
  const now = Date.now();
  const entry = memory.get(key);
  if (entry && entry.expiresAtMs > now) {
    return JSON.parse(entry.body);
  }
  memory.delete(key);
  const value = await load();
  if (memory.size >= MEMORY_LIMIT) {
    const oldest = memory.keys().next();
    if (!oldest.done) {
      memory.delete(oldest.value);
    }
  }
  memory.set(key, { body: JSON.stringify(value), expiresAtMs: now + ttlSeconds * 1000 });
  return value;
}

// src/env.ts
var USER_AGENT = "azure-winged-magpie-worker";
var DEFAULT_REPO = "gosuda/1";

// src/github-auth.ts
var cachedInstallationId = null;
var cachedInstallationToken = null;
async function buildGithubHeaders(env) {
  const token = await resolveGithubAuthToken(env);
  const headers = {
    accept: "application/vnd.github+json",
    "user-agent": USER_AGENT,
    "x-github-api-version": "2022-11-28"
  };
  if (token) {
    headers.authorization = `Bearer ${token}`;
  }
  return headers;
}
async function resolveGithubAuthToken(env) {
  if (env.GITHUB_TOKEN) {
    return env.GITHUB_TOKEN;
  }
  const appId = env.MAGPIE_APP_ID || env.CAPY_APP_ID || "";
  const privateKey = env.MAGPIE_APP_PRIVATE_KEY || env.CAPY_APP_PRIVATE_KEY || "";
  if (!appId || !privateKey) {
    return "";
  }
  const now = Date.now();
  if (cachedInstallationToken && cachedInstallationToken.expiresAtMs - now > 60000) {
    return cachedInstallationToken.token;
  }
  const jwt = await signGithubAppJwt(appId, privateKey);
  let installationId = env.MAGPIE_INSTALLATION_ID || env.CAPY_INSTALLATION_ID || cachedInstallationId || "";
  if (!installationId) {
    const repo = env.GITHUB_REPO || DEFAULT_REPO;
    const jwtHeaders = {
      accept: "application/vnd.github+json",
      authorization: `Bearer ${jwt}`,
      "user-agent": USER_AGENT,
      "x-github-api-version": "2022-11-28"
    };
    const instResp = await fetch(`https://api.github.com/repos/${repo}/installation`, {
      headers: jwtHeaders
    });
    if (instResp.ok) {
      const instData = await instResp.json();
      installationId = String(instData.id);
    } else {
      const listResp = await fetch("https://api.github.com/app/installations", {
        headers: jwtHeaders
      });
      if (!listResp.ok) {
        throw new Error(`GitHub App JWT auth failed (${listResp.status}): check MAGPIE_APP_ID and MAGPIE_APP_PRIVATE_KEY`);
      }
      const listData = await listResp.json();
      if (!Array.isArray(listData) || listData.length === 0) {
        throw new Error(`GitHub App is not installed on any organization or repository yet (install it on ${repo})`);
      }
      const owner = repo.split("/")[0]?.toLowerCase();
      const matched = listData.find((x) => x.account?.login?.toLowerCase() === owner) || listData[0];
      installationId = String(matched.id);
    }
    cachedInstallationId = installationId;
  }
  const resp = await fetch(`https://api.github.com/app/installations/${installationId}/access_tokens`, {
    method: "POST",
    headers: {
      accept: "application/vnd.github+json",
      authorization: `Bearer ${jwt}`,
      "user-agent": USER_AGENT,
      "x-github-api-version": "2022-11-28"
    }
  });
  if (!resp.ok) {
    throw new Error(`GitHub App token exchange failed with status ${resp.status}`);
  }
  const data = await resp.json();
  cachedInstallationToken = {
    token: data.token,
    expiresAtMs: Date.parse(data.expires_at)
  };
  return data.token;
}
async function signGithubAppJwt(appId, pemKey) {
  const nowSec = Math.floor(Date.now() / 1000);
  const header = base64UrlEncodeString(JSON.stringify({ alg: "RS256", typ: "JWT" }));
  const payload = base64UrlEncodeString(JSON.stringify({
    iat: nowSec - 60,
    exp: nowSec + 540,
    iss: appId
  }));
  const signingInput = `${header}.${payload}`;
  const keyData = pemToArrayBuffer(pemKey);
  const cryptoKey = await crypto.subtle.importKey("pkcs8", keyData, { name: "RSASSA-PKCS1-v1_5", hash: "SHA-256" }, false, ["sign"]);
  const sig = await crypto.subtle.sign("RSASSA-PKCS1-v1_5", cryptoKey, new TextEncoder().encode(signingInput));
  return `${signingInput}.${base64UrlEncodeBytes(new Uint8Array(sig))}`;
}
function pemToArrayBuffer(pem) {
  const isPkcs1 = pem.includes("BEGIN RSA PRIVATE KEY");
  const base64 = pem.replace(/-----BEGIN [A-Z0-9 ]+-----/g, "").replace(/-----END [A-Z0-9 ]+-----/g, "").replace(/\s+/g, "");
  const bin = atob(base64);
  const der = new Uint8Array(bin.length);
  for (let i = 0;i < bin.length; i++) {
    der[i] = bin.charCodeAt(i);
  }
  if (!isPkcs1) {
    return der.buffer;
  }
  return wrapPkcs1ToPkcs8(der).buffer;
}
function wrapPkcs1ToPkcs8(pkcs1) {
  const version = new Uint8Array([2, 1, 0]);
  const algId = new Uint8Array([
    48,
    13,
    6,
    9,
    42,
    134,
    72,
    134,
    247,
    13,
    1,
    1,
    1,
    5,
    0
  ]);
  const octetString = encodeAsn1Tag(4, pkcs1);
  const body = new Uint8Array(version.length + algId.length + octetString.length);
  body.set(version, 0);
  body.set(algId, version.length);
  body.set(octetString, version.length + algId.length);
  return encodeAsn1Tag(48, body);
}
function encodeAsn1Tag(tag, content) {
  const lenBytes = encodeAsn1Length(content.length);
  const out = new Uint8Array(1 + lenBytes.length + content.length);
  out[0] = tag;
  out.set(lenBytes, 1);
  out.set(content, 1 + lenBytes.length);
  return out;
}
function encodeAsn1Length(len) {
  if (len < 128) {
    return new Uint8Array([len]);
  }
  const bytes = [];
  let rem = len;
  while (rem > 0) {
    bytes.unshift(rem & 255);
    rem >>= 8;
  }
  return new Uint8Array([128 | bytes.length, ...bytes]);
}
function base64UrlEncodeString(input) {
  return base64UrlEncodeBytes(new TextEncoder().encode(input));
}
function base64UrlEncodeBytes(bytes) {
  let binary = "";
  for (let i = 0;i < bytes.length; i++) {
    binary += String.fromCharCode(bytes[i]);
  }
  return btoa(binary).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/g, "");
}

// src/github.ts
class GithubReadError extends Error {
  status;
  constructor(status, path) {
    super(`GitHub API ${status} for ${path}`);
    this.name = "GithubReadError";
    this.status = status;
  }
}

class BudgetExhaustedError extends Error {
  constructor(limit) {
    super(`request read budget of ${limit} GitHub calls used up; reload to continue`);
    this.name = "BudgetExhaustedError";
  }
}

class ReadBudget {
  limit;
  remaining;
  constructor(limit) {
    this.limit = limit;
    this.remaining = limit;
  }
  spend() {
    if (this.remaining <= 0) {
      throw new BudgetExhaustedError(this.limit);
    }
    this.remaining -= 1;
  }
}
async function githubGet(env, repo, path) {
  const headers = await buildGithubHeaders(env);
  const resp = await fetch(`https://api.github.com/repos/${repo}/${path}`, { headers });
  if (!resp.ok) {
    throw new GithubReadError(resp.status, path);
  }
  return await resp.json();
}
async function mapLimit(items, limit, fn) {
  const out = new Array(items.length);
  let next = 0;
  const lanes = Array.from({ length: Math.min(limit, items.length) }, async () => {
    while (next < items.length) {
      const index = next++;
      out[index] = await fn(items[index]);
    }
  });
  await Promise.all(lanes);
  return out;
}

// src/http.ts
function redirectResponse(targetUrl) {
  return new Response(null, {
    status: 302,
    headers: {
      location: targetUrl,
      "cache-control": "no-store"
    }
  });
}
function jsonResponse(payload, status = 200) {
  return new Response(JSON.stringify(payload), {
    status,
    headers: {
      "content-type": "application/json; charset=utf-8",
      "cache-control": "no-store"
    }
  });
}
function encodeHtml(str) {
  return str.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;");
}

// src/interdiff.ts
var HUNK_HEADER = /^@@ -\d+(?:,\d+)? \+\d+(?:,\d+)? @@/;
function normalizeLine(line) {
  return HUNK_HEADER.test(line) ? line.replace(HUNK_HEADER, "@@ @@") : line;
}
function normalizePatch(patch) {
  return patch.split(`
`).map(normalizeLine).join(`
`);
}
function patchFingerprint(files) {
  const rows = [];
  for (const file of files) {
    if (file.patch === null) {
      return null;
    }
    rows.push([file.filename, file.status, file.previous_filename ?? null, normalizePatch(file.patch)]);
  }
  rows.sort((a, b) => a[0] < b[0] ? -1 : a[0] > b[0] ? 1 : 0);
  return JSON.stringify(rows);
}

// src/refs.ts
function isHiddenRef(ref) {
  return ref.split("/").filter((seg) => seg !== "").some((seg) => seg === "sec" || seg.startsWith("sec-"));
}

// src/changes.ts
var CHANGE_ID_PATTERN = /^I[0-9a-f]{40}$/;
var CHANGE_REF_PATTERN = /^refs\/ws\/cl\/(I[0-9a-f]{40})\/([0-9a-f]{40})$/;
var CHANGE_ID_TRAILER = /^Change-Id:\s*(I[0-9a-f]{40})\s*$/gm;
var EMBARGOED_SUBJECT_PREFIX = "sec-";
var DECISIVE_REVIEW_STATES = { APPROVED: true, CHANGES_REQUESTED: true, DISMISSED: true };
var COMMIT_LOOKUP_LIMIT = 150;
var ROOT_WALK_DEPTH = 50;
var COMPARE_FILE_CAP = 300;
var LIST_CONCURRENCY = 4;
var HEAD_LOOKUP_LIMIT = 30;
var PULLS_PER_PAGE = 100;
var PULL_PAGE_LIMIT = 3;
var IMMUTABLE_TTL_SECONDS = 7 * 24 * 60 * 60;
var PULLS_TTL_SECONDS = 15;

class ChangeNotFoundError extends Error {
  reviewUnavailable;
  constructor(message, reviewUnavailable = null) {
    super(message);
    this.name = "ChangeNotFoundError";
    this.reviewUnavailable = reviewUnavailable;
  }
}
function messageOf(err) {
  return err instanceof Error ? err.message : "unknown error";
}
function timeOf(iso) {
  return Date.parse(iso ?? "") || 0;
}
function toStamp(raw) {
  return { name: raw?.name ?? null, email: raw?.email ?? null, date: raw?.date ?? null };
}
function toCommitInfo(sha, raw) {
  const message = raw.message ?? "";
  return {
    sha,
    subject: (message.split(`
`, 1)[0] ?? "").replace(/\r$/, "").trim(),
    message,
    parents: (raw.parents ?? []).map((parent) => parent.sha),
    author: toStamp(raw.author),
    committer: toStamp(raw.committer)
  };
}
function toPatchFile(raw) {
  return {
    filename: raw.filename,
    previous_filename: raw.previous_filename ?? null,
    status: raw.status,
    additions: raw.additions ?? 0,
    deletions: raw.deletions ?? 0,
    patch: typeof raw.patch === "string" ? raw.patch : null
  };
}
function toPullSummary(raw) {
  if (isHiddenRef(raw.head.ref) || isHiddenRef(raw.base.ref)) {
    return null;
  }
  return {
    number: raw.number,
    title: raw.title ?? "",
    url: raw.html_url,
    state: raw.state === "open" ? "open" : "closed",
    merged: Boolean(raw.merged_at),
    authorLogin: raw.user?.login ?? null,
    headSha: raw.head.sha,
    headRef: raw.head.ref
  };
}
function toPullInfo(pull) {
  return { number: pull.number, title: pull.title, url: pull.url, state: pull.state, merged: pull.merged };
}
function toReviewRecord(raw) {
  return {
    login: raw.user?.login ?? "ghost",
    state: raw.state,
    commitId: raw.commit_id,
    submittedAt: raw.submitted_at ?? null
  };
}
function changeIdOf(commit) {
  let changeId = null;
  for (const match of commit.message.matchAll(CHANGE_ID_TRAILER)) {
    changeId = match[1];
  }
  return changeId;
}
function parseChangeRef(ref) {
  if (isHiddenRef(ref)) {
    return null;
  }
  const match = CHANGE_REF_PATTERN.exec(ref);
  return match ? { changeId: match[1], sha: match[2], ref } : null;
}

class Reader {
  env;
  repo;
  budget;
  memo = new Map;
  constructor(env, repo, limit) {
    this.env = env;
    this.repo = repo;
    this.budget = new ReadBudget(limit);
  }
  once(key, load) {
    if (!this.memo.has(key)) {
      this.memo.set(key, load());
    }
    return this.memo.get(key);
  }
  commit(sha) {
    return this.once(`commit ${sha}`, () => readThrough(`commit:${this.repo}:${sha}`, IMMUTABLE_TTL_SECONDS, async () => {
      this.budget.spend();
      const raw = await githubGet(this.env, this.repo, `git/commits/${sha}`);
      return toCommitInfo(sha, raw);
    }));
  }
  compare(root, tip) {
    const range = `${root}...${tip}`;
    return this.once(`compare ${range}`, () => readThrough(`compare:${this.repo}:${range}`, IMMUTABLE_TTL_SECONDS, async () => {
      this.budget.spend();
      const raw = await githubGet(this.env, this.repo, `compare/${range}`);
      const files = (raw.files ?? []).map(toPatchFile);
      return { files, truncated: files.length >= COMPARE_FILE_CAP };
    }));
  }
  pulls() {
    return this.once("pulls", async () => {
      const all = [];
      for (let page = 1;page <= PULL_PAGE_LIMIT; page++) {
        const { count, items } = await readThrough(`pulls:${this.repo}:${page}`, PULLS_TTL_SECONDS, async () => {
          const raw = await githubGet(this.env, this.repo, `pulls?state=all&sort=updated&direction=desc&per_page=${PULLS_PER_PAGE}&page=${page}`);
          const summaries = raw.flatMap((pull) => {
            const summary = toPullSummary(pull);
            return summary ? [summary] : [];
          });
          return { count: raw.length, items: summaries };
        });
        all.push(...items);
        if (count < PULLS_PER_PAGE) {
          break;
        }
      }
      return all;
    });
  }
  pull(number) {
    return this.once(`pull ${number}`, async () => {
      this.budget.spend();
      return toPullSummary(await githubGet(this.env, this.repo, `pulls/${number}`));
    });
  }
  reviews(number) {
    return this.once(`reviews ${number}`, () => readThrough(`reviews:${this.repo}:${number}`, PULLS_TTL_SECONDS, async () => {
      const raw = await githubGet(this.env, this.repo, `pulls/${number}/reviews?per_page=100`);
      return raw.map(toReviewRecord);
    }));
  }
}
var refListing = null;
function clearChangeRefCache() {
  refListing = null;
}
function refTtlMs(env) {
  const raw = env.CACHE_TTL_SECONDS;
  if (raw === undefined || raw.trim() === "") {
    return 5000;
  }
  return Math.max(0, Number(raw) || 0) * 1000;
}
async function listChangeRefs(env, repo) {
  const now = Date.now();
  if (refListing && refListing.repo === repo && now - refListing.fetchedAtMs < refTtlMs(env)) {
    return refListing.entries;
  }
  const raw = await githubGet(env, repo, "git/matching-refs/ws/cl/");
  const entries = [];
  for (const item of raw) {
    const entry = parseChangeRef(item.ref ?? "");
    if (entry) {
      entries.push(entry);
    }
  }
  refListing = { repo, fetchedAtMs: now, entries };
  return entries;
}
function groupByChange(entries) {
  const groups = new Map;
  for (const entry of entries) {
    const group = groups.get(entry.changeId);
    if (group) {
      group.push(entry);
    } else {
      groups.set(entry.changeId, [entry]);
    }
  }
  return groups;
}
function byPatchsetOrder(a, b) {
  return timeOf(a.commit.committer.date) - timeOf(b.commit.committer.date) || (a.sha < b.sha ? -1 : a.sha > b.sha ? 1 : 0);
}
async function readPatchsets(reader, refs) {
  const commits = await Promise.all(refs.map((entry) => reader.commit(entry.sha)));
  return refs.map((entry, index) => ({ ref: entry.ref, sha: entry.sha, commit: commits[index] })).sort(byPatchsetOrder);
}
function isEmbargoed(record) {
  return record.commit.subject.startsWith(EMBARGOED_SUBJECT_PREFIX);
}
async function findRoot(reader, tip) {
  const roots = new Set;
  const seen = new Set([tip.sha]);
  let frontier = [tip];
  for (let depth = 0;frontier.length > 0; depth++) {
    const next = [];
    for (const commit of frontier) {
      if (commit.parents.length === 0) {
        roots.add(commit.sha);
        continue;
      }
      for (const parent of commit.parents) {
        if (!seen.has(parent)) {
          seen.add(parent);
          next.push(parent);
        }
      }
    }
    if (next.length === 0) {
      break;
    }
    if (depth + 1 > ROOT_WALK_DEPTH) {
      throw new Error(`history is deeper than ${ROOT_WALK_DEPTH} commits, so its root was not found`);
    }
    frontier = await Promise.all(next.map((sha) => reader.commit(sha)));
  }
  if (roots.size !== 1) {
    throw new Error(`history has ${roots.size} parentless roots; a patchset must sit on exactly one trunk snapshot`);
  }
  return [...roots][0];
}
async function describePatchset(reader, record, number) {
  const { commit } = record;
  const info = {
    number,
    sha: record.sha,
    ref: record.ref,
    rootSha: null,
    date: commit.committer.date ?? "",
    subject: commit.subject,
    message: commit.message,
    author: commit.author,
    files: null,
    filesTruncated: false,
    error: null,
    carriesTo: []
  };
  try {
    info.rootSha = await findRoot(reader, commit);
    const compared = await reader.compare(info.rootSha, record.sha);
    info.files = compared.files;
    info.filesTruncated = compared.truncated;
  } catch (err) {
    info.error = messageOf(err);
  }
  return info;
}
function fillCarries(patchsets) {
  const prints = patchsets.map((patchset) => patchset.files ? patchFingerprint(patchset.files) : null);
  for (let i = 0;i < patchsets.length; i++) {
    for (let j = i + 1;j < patchsets.length; j++) {
      if (prints[i] !== null && prints[i] === prints[j]) {
        patchsets[i].carriesTo.push(patchsets[j].number);
      }
    }
  }
}
function effectiveApprovals(reviews, numberOfSha) {
  const decisive = reviews.filter((review) => DECISIVE_REVIEW_STATES[review.state] === true).sort((a, b) => timeOf(a.submittedAt) - timeOf(b.submittedAt));
  const latest = new Map;
  for (const review of decisive) {
    latest.set(review.login, review);
  }
  return [...latest.values()].filter((review) => review.state === "APPROVED").map((review) => ({
    login: review.login,
    commitId: review.commitId,
    patchset: numberOfSha.get(review.commitId) ?? null,
    submittedAt: review.submittedAt
  }));
}
function reviewUnavailableReason(err) {
  if (err instanceof GithubReadError) {
    if (err.status === 403) {
      return "Review status is unavailable: the magpie GitHub App cannot read pull requests on gosuda/1 (HTTP 403). Grant it pull_requests:read to show review numbers and approvals.";
    }
    if (err.status === 404) {
      return "Review status is unavailable: pull requests are not visible to the magpie GitHub App on gosuda/1 (HTTP 404). Grant it pull_requests:read to show them.";
    }
    return `Review status could not be read (GitHub returned HTTP ${err.status}).`;
  }
  return "Review status could not be read from GitHub right now.";
}
function preferredPull(candidate, current) {
  if (candidate.state === "open" !== (current.state === "open")) {
    return candidate.state === "open";
  }
  return candidate.number > current.number;
}
async function findReviewPulls(reader, targets, pinned = null) {
  const found = new Map;
  if (pinned) {
    found.set(pinned.changeId, pinned.pull);
  }
  let pulls;
  try {
    pulls = await reader.pulls();
  } catch (err) {
    return { unavailable: pinned ? null : reviewUnavailableReason(err), found };
  }
  const changeOfSha = new Map;
  for (const [changeId, shas] of targets) {
    for (const sha of shas) {
      changeOfSha.set(sha, changeId);
    }
  }
  const offer = (changeId, pull) => {
    const current = found.get(changeId);
    if (!current || preferredPull(pull, current)) {
      found.set(changeId, pull);
    }
  };
  const branchHeads = [];
  for (const pull of pulls) {
    const changeId = changeOfSha.get(pull.headSha);
    if (changeId) {
      offer(changeId, pull);
    } else if (pull.headRef.startsWith("review/")) {
      branchHeads.push(pull);
    }
  }
  for (const pull of branchHeads.slice(0, HEAD_LOOKUP_LIMIT)) {
    try {
      const changeId = changeIdOf(await reader.commit(pull.headSha));
      if (changeId && targets.has(changeId)) {
        offer(changeId, pull);
      }
    } catch (err) {
      if (err instanceof BudgetExhaustedError) {
        break;
      }
    }
  }
  return { unavailable: null, found };
}
function listNote(truncated, omitted) {
  if (truncated) {
    return `Stopped after ${COMMIT_LOOKUP_LIMIT} GitHub reads for this request, so ${omitted} change${omitted === 1 ? " is" : "s are"} not shown yet. Reload to continue; reads already made are cached.`;
  }
  if (omitted > 0) {
    return `${omitted} change${omitted === 1 ? " is" : "s are"} not shown because GitHub could not be read for ${omitted === 1 ? "it" : "them"}.`;
  }
  return null;
}
async function listChanges(env, repo) {
  const reader = new Reader(env, repo, COMMIT_LOOKUP_LIMIT);
  const groups = [...groupByChange(await listChangeRefs(env, repo))].sort(([a], [b]) => a < b ? -1 : a > b ? 1 : 0);
  let omitted = 0;
  let truncated = false;
  const read = await mapLimit(groups, LIST_CONCURRENCY, async ([changeId, refs]) => {
    try {
      const patchsets = await readPatchsets(reader, refs);
      if (patchsets.some(isEmbargoed)) {
        return null;
      }
      return { changeId, patchsets };
    } catch (err) {
      omitted += 1;
      if (err instanceof BudgetExhaustedError) {
        truncated = true;
      }
      return null;
    }
  });
  const readable = read.flatMap((entry) => entry ? [entry] : []);
  const targets = new Map(readable.map(({ changeId, patchsets }) => [changeId, new Set(patchsets.map((p) => p.sha))]));
  const lookup = await findReviewPulls(reader, targets);
  const changes = readable.map(({ changeId, patchsets }) => {
    const first = patchsets[0];
    const newest = patchsets[patchsets.length - 1];
    const pull = lookup.found.get(changeId) ?? null;
    return {
      id: changeId,
      subject: newest.commit.subject,
      patchsetCount: patchsets.length,
      updatedAt: newest.commit.committer.date ?? "",
      author: { name: first.commit.author.name, login: pull?.authorLogin ?? null },
      review: pull ? toPullInfo(pull) : null
    };
  });
  changes.sort((a, b) => timeOf(b.updatedAt) - timeOf(a.updatedAt) || (a.id < b.id ? -1 : 1));
  return {
    repo,
    changes,
    truncated,
    omittedChanges: omitted,
    note: listNote(truncated, omitted),
    commitLookupLimit: COMMIT_LOOKUP_LIMIT,
    reviewUnavailable: lookup.unavailable
  };
}
async function getChangeDetail(env, repo, changeId, pinnedPull = null) {
  if (!CHANGE_ID_PATTERN.test(changeId)) {
    throw new ChangeNotFoundError(`${changeId} is not a change id`);
  }
  const reader = new Reader(env, repo, COMMIT_LOOKUP_LIMIT);
  const refs = (await listChangeRefs(env, repo)).filter((entry) => entry.changeId === changeId);
  const notFound = new ChangeNotFoundError(`No published patchsets for change ${changeId}.`);
  if (refs.length === 0) {
    throw notFound;
  }
  const records = await readPatchsets(reader, refs);
  if (records.some(isEmbargoed)) {
    throw notFound;
  }
  const patchsets = [];
  for (const [index, record] of records.entries()) {
    patchsets.push(await describePatchset(reader, record, index + 1));
  }
  fillCarries(patchsets);
  const targets = new Map([[changeId, new Set(records.map((r) => r.sha))]]);
  const lookup = await findReviewPulls(reader, targets, pinnedPull ? { changeId, pull: pinnedPull } : null);
  const pull = lookup.found.get(changeId) ?? null;
  let reviews = [];
  let reviewUnavailable = lookup.unavailable;
  if (pull) {
    try {
      reviews = await reader.reviews(pull.number);
    } catch (err) {
      reviewUnavailable = reviewUnavailableReason(err);
    }
  }
  const numberOfSha = new Map(patchsets.map((patchset) => [patchset.sha, patchset.number]));
  const newest = records[records.length - 1].commit;
  return {
    repo,
    id: changeId,
    subject: newest.subject,
    updatedAt: newest.committer.date ?? "",
    patchsets,
    review: pull ? toPullInfo(pull) : null,
    reviews,
    approvals: effectiveApprovals(reviews, numberOfSha),
    reviewUnavailable
  };
}
async function resolveChangeIdFromPull(env, repo, number) {
  const reader = new Reader(env, repo, COMMIT_LOOKUP_LIMIT);
  let pull;
  try {
    pull = await reader.pull(number);
  } catch (err) {
    const status = err instanceof GithubReadError ? ` (HTTP ${err.status})` : "";
    throw new ChangeNotFoundError(`Pull request #${number} could not be read${status}.`, reviewUnavailableReason(err));
  }
  if (!pull) {
    throw new ChangeNotFoundError(`Pull request #${number} is not available.`);
  }
  let changeId;
  try {
    changeId = changeIdOf(await reader.commit(pull.headSha));
  } catch (err) {
    throw new ChangeNotFoundError(`The head commit of pull request #${number} could not be read: ${messageOf(err)}`);
  }
  if (!changeId) {
    throw new ChangeNotFoundError(`Pull request #${number} has no Change-Id trailer on its head commit.`);
  }
  return { changeId, pull };
}
function changeErrorResponse(err) {
  if (err instanceof ChangeNotFoundError) {
    return jsonResponse(err.reviewUnavailable ? { error: err.message, reviewUnavailable: err.reviewUnavailable } : { error: err.message }, 404);
  }
  if (err instanceof BudgetExhaustedError) {
    return jsonResponse({ error: err.message }, 503);
  }
  if (err instanceof GithubReadError) {
    return jsonResponse({ error: `magpie could not read gosuda/1: ${err.message}` }, 502);
  }
  return jsonResponse({ error: messageOf(err) }, 500);
}
async function handleChangesApi(url, env, repo) {
  const path = url.pathname.replace(/\/+$/, "");
  try {
    if (path === "/api/changes") {
      return jsonResponse(await listChanges(env, repo));
    }
    const rest = path.slice("/api/changes/".length);
    if (/^[0-9]+$/.test(rest)) {
      const resolved = await resolveChangeIdFromPull(env, repo, Number(rest));
      return jsonResponse(await getChangeDetail(env, repo, resolved.changeId, resolved.pull));
    }
    return jsonResponse(await getChangeDetail(env, repo, rest));
  } catch (err) {
    return changeErrorResponse(err);
  }
}

// src/worker.ts
var STATIC_UI_PATHS = new Set(["/", "/index.html", "/styles.css", "/app.js", "/favicon.ico"]);
var MOUNTED_PROJECTS = [
  { prefix: "web/gosuda", mirror: "gosuda/website" },
  { prefix: "web/securl", mirror: "gosuda/securl" },
  { prefix: "net/grt", mirror: "gosuda/GRT" },
  { prefix: "net/ivnp", mirror: "gosuda/IVNP" },
  { prefix: "net/portalite", mirror: "gosuda/portalite" },
  { prefix: "ipc/hqq/HQQ", mirror: "gosuda/HQQ" },
  { prefix: "ipc/hqq/hqq-sqlite3", mirror: "gosuda/hqq-sqlite3" },
  { prefix: "ipc/hqq/hqq-zstd", mirror: "gosuda/hqq-zstd" },
  { prefix: "devtools/azure-magpie", mirror: "gosuda/azure-magpie" }
];
var cachedRefs = null;
var cachedProjects = null;
var worker_default = {
  async fetch(request, env) {
    const url = new URL(request.url);
    const repo = env.GITHUB_REPO || DEFAULT_REPO;
    if (request.method === "POST" && url.pathname === "/webhook") {
      return handleGithubWebhook(request, env);
    }
    if (request.method !== "GET" && request.method !== "HEAD") {
      return new Response("Method Not Allowed", { status: 405 });
    }
    if (env.ASSETS && STATIC_UI_PATHS.has(url.pathname)) {
      return env.ASSETS.fetch(request);
    }
    if (isShellPath(url.pathname)) {
      return serveUiShell(request, url, env);
    }
    if (url.pathname === "/api/changes" || url.pathname.startsWith("/api/changes/")) {
      return handleChangesApi(url, env, repo);
    }
    try {
      const refs = await resolveSudaRefs(env, repo);
      if (url.pathname === "/refs.json" || url.pathname === "/sha") {
        return jsonResponse({
          repo,
          "suda/main": refs.mainSha,
          "suda/history": refs.historySha
        });
      }
      if (url.pathname === "/api/refs") {
        const [workspaces, projects] = await Promise.all([
          listActiveWorkspaceRefs(env, repo),
          resolveMountedProjects(env, repo)
        ]);
        return jsonResponse({
          repo,
          "suda/main": refs.mainSha,
          "suda/history": refs.historySha,
          workspaces,
          projects
        });
      }
      if (url.pathname === "/diff" || url.pathname === "/diff/" || url.pathname.startsWith("/diff/") || url.pathname.startsWith("/compare/")) {
        return await handleDiffRequest(request, url, env, repo, refs);
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
      const status = message.includes("not found") ? 404 : 500;
      return jsonResponse({ error: message }, status);
    }
  }
};
function isShellPath(pathname) {
  return pathname === "/changes" || pathname.startsWith("/changes/");
}
function serveUiShell(request, url, env) {
  if (!env.ASSETS) {
    return new Response("UI assets are not bound to this worker", { status: 404 });
  }
  return env.ASSETS.fetch(new Request(new URL("/index.html", url.origin).toString(), request));
}
function buildRedirectLocation(pathname, repo, refs) {
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
  if (trimmed === "head") {
    return `${base}/tree/${refs.historySha}`;
  }
  if (trimmed.startsWith("head/")) {
    return `${base}/tree/${refs.historySha}/${trimmed.slice("head/".length)}`;
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
function parseDiffSpec(pathname, searchParams) {
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
  const rawSpec = trimmed.startsWith("diff/") ? trimmed.slice("diff/".length) : trimmed.startsWith("compare/") ? trimmed.slice("compare/".length) : "";
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
    operator: qMode
  };
}
async function handleDiffRequest(request, url, env, repo, refs) {
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
        "cache-control": "no-store"
      }
    });
  }
  const baseSha = await resolveRefSha(env, repo, spec.baseRef, refs);
  const headSha = await resolveRefSha(env, repo, spec.headRef, refs);
  const compareUrl = `https://github.com/${repo}/compare/${baseSha}${spec.operator}${headSha}`;
  const shortPath = spec.baseRef === "suda/main" && spec.operator === ".." ? `/diff/${spec.headRef}` : `/diff/${spec.baseRef}${spec.operator}${spec.headRef}`;
  if (url.searchParams.get("format") === "json") {
    const payload = {
      repo,
      baseRef: spec.baseRef,
      baseSha,
      headRef: spec.headRef,
      headSha,
      operator: spec.operator,
      compareUrl,
      shortPath
    };
    return jsonResponse(payload);
  }
  return redirectResponse(compareUrl);
}
async function resolveRefSha(env, repo, refSpec, refs) {
  const cleaned = refSpec.replace(/^\/+|\/+$/g, "");
  if (isHiddenRef(cleaned)) {
    throw new Error(`ref '${refSpec}' not found`);
  }
  if (cleaned === "suda/main" || cleaned === "refs/suda/main" || cleaned === "main") {
    return refs.mainSha;
  }
  if (cleaned === "suda/history" || cleaned === "refs/suda/history" || cleaned === "history") {
    return refs.historySha;
  }
  if (/^[0-9a-f]{7,40}$/i.test(cleaned)) {
    return cleaned;
  }
  const candidates = [];
  if (cleaned.startsWith("refs/")) {
    candidates.push(cleaned.slice("refs/".length));
  } else if (cleaned.startsWith("ws/") || cleaned.startsWith("suda/") || cleaned.startsWith("heads/")) {
    candidates.push(cleaned);
  } else if (cleaned.startsWith("land/")) {
    candidates.push(`heads/${cleaned}`);
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
      const data = await resp.json();
      if (data.object?.sha) {
        return data.object.sha;
      }
    }
  }
  throw new Error(`ref '${refSpec}' not found`);
}
async function listActiveWorkspaceRefs(env, repo) {
  const headers = await buildGithubHeaders(env);
  const results = [];
  for (const prefix of ["ws/", "heads/review/", "heads/land/"]) {
    const resp = await fetch(`https://api.github.com/repos/${repo}/git/matching-refs/${prefix}`, {
      headers
    });
    if (!resp.ok) {
      continue;
    }
    const items = await resp.json();
    for (const item of items) {
      if (item.ref && item.object?.sha && !isHiddenRef(item.ref) && !item.ref.startsWith("refs/ws/cl/")) {
        const label = item.ref.replace(/^refs\/(heads\/)?/, "");
        results.push({ ref: item.ref, label, sha: item.object.sha });
      }
    }
  }
  return results;
}
async function resolveMountedProjects(env, repo) {
  const now = Date.now();
  if (cachedProjects && now - cachedProjects.fetchedAtMs < 60000) {
    return cachedProjects.items;
  }
  try {
    const headers = await buildGithubHeaders(env);
    headers.accept = "application/vnd.github.raw+json";
    const resp = await fetch(`https://api.github.com/repos/${repo}/contents/capy.star?ref=refs/suda/main`, { headers });
    if (resp.ok) {
      const text = await resp.text();
      const parsed = [];
      const tupleRe = /\(\s*"[^"]+"\s*,\s*"([^"]+)"\s*,\s*"([^"]+)"/g;
      for (const match of text.matchAll(tupleRe)) {
        if (match[1] && match[2] && match[1].includes("/") && match[2].includes("/")) {
          parsed.push({ prefix: match[1], mirror: match[2] });
        }
      }
      if (parsed.length > 0) {
        cachedProjects = { items: parsed, fetchedAtMs: now };
        return parsed;
      }
    }
  } catch {}
  return MOUNTED_PROJECTS;
}
function renderFallbackDiffHtml(origin, repo, refs, activeRefs) {
  const rows = activeRefs.map((item) => `<li><code>${encodeHtml(item.ref)}</code> (<a href="${encodeHtml(origin)}/diff/${encodeHtml(item.label)}">Diff vs suda/main</a>)</li>`).join("");
  return `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>${encodeHtml(repo)} Snapshot &amp; Diff Link Generator</title></head><body><h1>${encodeHtml(repo)} Snapshot &amp; Diff Link Generator</h1><p>suda/main: <code>${refs.mainSha}</code></p><ul>${rows}</ul></body></html>`;
}
async function resolveSudaRefs(env, repo) {
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
  const items = await resp.json();
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
async function handleGithubWebhook(request, env) {
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
  clearChangeRefCache();
  if (!env.LANDER_WAKE_URL || !env.LANDER_WAKE_SECRET) {
    return jsonResponse({ status: "cache-cleared" });
  }
  const wakeBody = new TextEncoder().encode(JSON.stringify({ event: request.headers.get("x-github-event") || "" }));
  const signature = await hmacSha256Hex(env.LANDER_WAKE_SECRET, wakeBody);
  try {
    const resp = await fetch(env.LANDER_WAKE_URL, {
      method: "POST",
      headers: {
        "content-type": "application/json",
        "user-agent": USER_AGENT,
        "x-hub-signature-256": `sha256=${signature}`
      },
      body: wakeBody,
      signal: AbortSignal.timeout(5000)
    });
    return jsonResponse({ status: "woken", code: resp.status }, resp.ok ? 200 : 502);
  } catch {
    return jsonResponse({ status: "wake-failed" }, 502);
  }
}
async function hmacSha256Hex(secret, data) {
  const key = await crypto.subtle.importKey("raw", new TextEncoder().encode(secret), { name: "HMAC", hash: "SHA-256" }, false, ["sign"]);
  const sig = new Uint8Array(await crypto.subtle.sign("HMAC", key, data));
  return Array.from(sig).map((b) => b.toString(16).padStart(2, "0")).join("");
}
async function verifyWebhookSignature(body, signatureHeader, secret) {
  if (!signatureHeader.startsWith("sha256=")) {
    return false;
  }
  const sigHex = signatureHeader.slice("sha256=".length);
  if (sigHex.length % 2 !== 0) {
    return false;
  }
  const sigBytes = new Uint8Array(sigHex.length / 2);
  for (let i = 0;i < sigHex.length; i += 2) {
    sigBytes[i / 2] = parseInt(sigHex.slice(i, i + 2), 16);
  }
  const key = await crypto.subtle.importKey("raw", new TextEncoder().encode(secret), { name: "HMAC", hash: "SHA-256" }, false, ["verify"]);
  return crypto.subtle.verify("HMAC", key, sigBytes, body);
}
export {
  buildRedirectLocation,
  worker_default as default,
  isHiddenRef,
  parseDiffSpec,
  resolveRefSha
};
