import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import { clearChangeRefCache } from "./changes";
import { resetReadCacheForTests } from "./cache";
import worker from "./worker";

const REPO = "gosuda/1";
const ENV = { GITHUB_REPO: REPO, GITHUB_TOKEN: "test-token", CACHE_TTL_SECONDS: "0" };
const WEBHOOK_SECRET = "webhook-test-secret";

type Json = Record<string, unknown>;

interface FakeCommit {
  message: string;
  parents: string[];
  date: string;
  author?: string;
}

interface Fixture {
  refs: Array<{ ref: string; sha: string }>;
  commits: Record<string, FakeCommit>;
  compares: Record<string, Json[] | number>;
  pulls: Json[] | number;
  // The pull request listing window; defaults to `pulls`. Lookups by number always use `pulls`.
  listing?: Json[];
  reviews: Record<number, Json[] | number>;
}

// Deterministic 40-hex object names; the tag is hex and padded with zeros.
const hex = (tag: string): string => tag.padEnd(40, "0");
const day = (n: number): string => `2026-10-${String(n).padStart(2, "0")}T10:00:00Z`;
const changeId = (tag: string): string => `I${tag.repeat(10)}`;

const A = changeId("aa01");
const B = changeId("bb02");
const C = changeId("cc03");
const D = changeId("dd04");
const E = changeId("ee05");
const F = changeId("ff06");

const TRUNK = hex("1000");
const TRUNK_X = hex("2000");
const TRUNK_Y = hex("2001");
const A1 = hex("a100");
const A2 = hex("a200");
const A3 = hex("a300");
const B1 = hex("b100");
const C1 = hex("c100");
const D1 = hex("d100");
const E0 = hex("e000");
const E1 = hex("e100");
const N1 = hex("9100");
const deepSha = (i: number): string => hex(`f${i.toString(16).padStart(3, "0")}`);

const OLD_HUNK = "@@ -10,3 +10,4 @@ func load() error {\n cfg := read()\n-return nil\n+if cfg == nil {\n+return errNoConfig\n+}\n return nil";
const REBASED_HUNK = "@@ -42,3 +46,4 @@ func load() error {\n cfg := read()\n-return nil\n+if cfg == nil {\n+return errNoConfig\n+}\n return nil";

function pullOf(number: number, headSha: string, headRef: string, state: "open" | "closed", login: string): Json {
  return {
    number,
    title: `PR ${number}`,
    html_url: `https://github.com/${REPO}/pull/${number}`,
    state,
    merged_at: state === "closed" ? day(9) : null,
    user: { login },
    head: { sha: headSha, ref: headRef },
    base: { ref: `base/${headRef.replace(/^review\//, "")}` },
  };
}

function standardFixture(): Fixture {
  const commits: Record<string, FakeCommit> = {
    [TRUNK]: { message: "suda: trunk snapshot", parents: [], date: day(1) },
    [TRUNK_X]: { message: "root x", parents: [], date: day(1) },
    [TRUNK_Y]: { message: "root y", parents: [], date: day(1) },
    [A1]: { message: `ipc/hqq: fix header decode\n\nChange-Id: ${A}`, parents: [TRUNK], date: day(5), author: "Dalgona" },
    [A3]: { message: `wip header\n\nChange-Id: ${A}`, parents: [TRUNK], date: day(7), author: "Dalgona" },
    [A2]: { message: `ipc/hqq: fix header decode (rebased)\n\nChange-Id: ${A}`, parents: [A3], date: day(8), author: "Dalgona" },
    [B1]: { message: `docs: update README\n\nChange-Id: ${B}`, parents: [TRUNK], date: day(3), author: "Bob" },
    [C1]: { message: `sec-202610 harden decode\n\nChange-Id: ${C}`, parents: [TRUNK], date: day(2), author: "Carol" },
    [D1]: { message: `sec-202610 unrelated\n\nChange-Id: ${D}`, parents: [TRUNK], date: day(2) },
    [E0]: { message: `e: first try\n\nChange-Id: ${E}`, parents: [TRUNK], date: day(6) },
    [E1]: { message: `e: second try, two roots\n\nChange-Id: ${E}`, parents: [TRUNK_X, TRUNK_Y], date: day(10) },
    [N1]: { message: "no trailer here", parents: [TRUNK], date: day(4) },
  };
  // A 52-commit linear chain: its root is deeper than the walk allows.
  for (let i = 0; i <= 52; i++) {
    commits[deepSha(i)] = {
      message: `deep ${i}\n\nChange-Id: ${F}`,
      parents: i < 52 ? [deepSha(i + 1)] : [],
      date: day(4),
    };
  }

  return {
    refs: [
      { ref: "refs/suda/main", sha: TRUNK },
      { ref: "refs/suda/history", sha: TRUNK },
      { ref: "refs/ws/alice/fix-header", sha: B1 },
      { ref: `refs/ws/cl/${A}/${A1}`, sha: A1 },
      { ref: `refs/ws/cl/${A}/${A2}`, sha: A2 },
      { ref: `refs/ws/cl/${B}/${B1}`, sha: B1 },
      { ref: `refs/ws/cl/${C}/${C1}`, sha: C1 },
      { ref: `refs/ws/cl/sec/${D}/${D1}`, sha: D1 },
      { ref: `refs/ws/cl/not-a-change/${B1}`, sha: B1 },
      { ref: `refs/ws/cl/${B}/short`, sha: B1 },
      { ref: `refs/ws/cl/${E}/${E0}`, sha: E0 },
      { ref: `refs/ws/cl/${E}/${E1}`, sha: E1 },
      { ref: `refs/ws/cl/${F}/${deepSha(0)}`, sha: deepSha(0) },
    ],
    commits,
    compares: {
      [`${TRUNK}...${A1}`]: [
        { filename: "ipc/hqq/load.go", status: "modified", additions: 2, deletions: 1, patch: OLD_HUNK },
      ],
      [`${TRUNK}...${A2}`]: [
        { filename: "ipc/hqq/load.go", status: "modified", additions: 2, deletions: 1, patch: REBASED_HUNK },
      ],
      [`${TRUNK}...${B1}`]: [
        { filename: "docs/README.md", status: "modified", additions: 1, deletions: 1, patch: "@@ -1 +1 @@\n-a\n+b" },
        { filename: "bin/logo.png", status: "added", additions: 0, deletions: 0 },
      ],
      [`${TRUNK}...${E0}`]: [],
    },
    pulls: [
      pullOf(7, A2, "review/task-402", "open", "dalgona"),
      pullOf(9, B1, "review/task-b", "closed", "bob"),
      pullOf(11, D1, "review/sec-202610-x", "open", "mallory"),
      pullOf(12, N1, "review/task-no-trailer", "open", "eve"),
    ],
    reviews: {
      7: [
        { user: { login: "alice" }, state: "APPROVED", commit_id: A1, submitted_at: day(6) },
        { user: { login: "carol" }, state: "COMMENTED", commit_id: A2, submitted_at: day(9) },
        { user: { login: "bob" }, state: "CHANGES_REQUESTED", commit_id: A2, submitted_at: day(9) },
      ],
      9: 403,
    },
  };
}

// Fake gosuda/1 REST surface: the endpoints the change API reads.
function installFakeGithub(fixture: Fixture): string[] {
  const calls: string[] = [];
  const prefix = `/repos/${REPO}/`;
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    const url = new URL(String(input));
    const path = url.pathname.startsWith(prefix) ? url.pathname.slice(prefix.length) : url.pathname;
    calls.push(path + url.search);
    return fakeRespond(fixture, path);
  }) as typeof fetch;
  return calls;
}

function fakeRespond(fx: Fixture, path: string): Response {
  const json = (body: unknown, status = 200): Response =>
    new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });
  const empty = (status: number): Response => new Response("{}", { status });

  if (path.startsWith("git/matching-refs/")) {
    const prefix = path.slice("git/matching-refs/".length);
    return json(
      fx.refs
        .filter((item) => item.ref.startsWith(`refs/${prefix}`))
        .map((item) => ({ ref: item.ref, object: { sha: item.sha, type: "commit" } })),
    );
  }
  if (path.startsWith("git/commits/")) {
    const sha = path.slice("git/commits/".length);
    const commit = fx.commits[sha];
    if (!commit) {
      return empty(404);
    }
    return json({
      sha,
      message: commit.message,
      parents: commit.parents.map((parent) => ({ sha: parent })),
      tree: { sha: "tree" },
      author: { name: commit.author ?? null, email: null, date: commit.date },
      committer: { name: commit.author ?? "committer", email: null, date: commit.date },
    });
  }
  if (path.startsWith("compare/")) {
    const entry = fx.compares[path.slice("compare/".length)];
    if (entry === undefined) {
      return empty(404);
    }
    return typeof entry === "number" ? empty(entry) : json({ status: "ahead", files: entry });
  }
  if (path === "pulls") {
    const listing = fx.listing ?? fx.pulls;
    return typeof listing === "number" ? empty(listing) : json(listing);
  }
  const single = /^pulls\/(\d+)$/.exec(path);
  if (single) {
    if (typeof fx.pulls === "number") {
      return empty(fx.pulls);
    }
    const pull = fx.pulls.find((item) => item.number === Number(single[1]));
    return pull ? json(pull) : empty(404);
  }
  const reviews = /^pulls\/(\d+)\/reviews$/.exec(path);
  if (reviews) {
    const entry = fx.reviews[Number(reviews[1])] ?? [];
    return typeof entry === "number" ? empty(entry) : json(entry);
  }
  if (path === "contents/capy.star") {
    return empty(404);
  }
  return empty(404);
}

async function get(path: string, env: Record<string, unknown> = ENV): Promise<Response> {
  return worker.fetch(new Request(`https://magpie.gosuda.org${path}`), env as never);
}

async function signedWebhook(event: string): Promise<Request> {
  const body = JSON.stringify({ zen: "ok" });
  const key = await crypto.subtle.importKey(
    "raw",
    new TextEncoder().encode(WEBHOOK_SECRET),
    { name: "HMAC", hash: "SHA-256" },
    false,
    ["sign"],
  );
  const sig = new Uint8Array(await crypto.subtle.sign("HMAC", key, new TextEncoder().encode(body)));
  const hexSig = Array.from(sig)
    .map((b) => b.toString(16).padStart(2, "0"))
    .join("");
  return new Request("https://magpie.gosuda.org/webhook", {
    method: "POST",
    headers: { "x-github-event": event, "x-hub-signature-256": `sha256=${hexSig}` },
    body,
  });
}

let restoreFetch: typeof fetch;

beforeEach(() => {
  restoreFetch = globalThis.fetch;
  resetReadCacheForTests();
  clearChangeRefCache();
});

afterEach(() => {
  globalThis.fetch = restoreFetch;
});

describe("GET /api/changes", () => {
  test("groups patchsets by change, hides embargoed and malformed refs, and orders newest first", async () => {
    installFakeGithub(standardFixture());
    const res = await get("/api/changes");
    expect(res.status).toBe(200);
    const body = (await res.json()) as {
      changes: Array<{
        id: string;
        subject: string;
        patchsetCount: number;
        updatedAt: string;
        author: { name: string | null; login: string | null };
        review: { number: number; state: string; merged: boolean; url: string } | null;
      }>;
      truncated: boolean;
      note: string | null;
      reviewUnavailable: string | null;
    };

    // C (embargoed subject) and D (sec namespace) are absent; E has the newest patchset.
    expect(body.changes.map((c) => c.id)).toEqual([E, A, F, B]);

    const a = body.changes.find((c) => c.id === A);
    expect(a?.subject).toBe("ipc/hqq: fix header decode (rebased)");
    expect(a?.patchsetCount).toBe(2);
    expect(a?.updatedAt).toBe(day(8));
    expect(a?.author).toEqual({ name: "Dalgona", login: "dalgona" });
    expect(a?.review).toEqual({
      number: 7,
      title: "PR 7",
      url: `https://github.com/${REPO}/pull/7`,
      state: "open",
      merged: false,
    });

    const b = body.changes.find((c) => c.id === B);
    expect(b?.review?.number).toBe(9);
    expect(b?.review?.state).toBe("closed");
    expect(b?.review?.merged).toBe(true);

    expect(body.changes.find((c) => c.id === F)?.review).toBeNull();
    expect(body.truncated).toBe(false);
    expect(body.note).toBeNull();
    expect(body.reviewUnavailable).toBeNull();
    expect(JSON.stringify(body)).not.toContain("sec-");
  });

  test("degrades to changes without review info when the App cannot read pull requests", async () => {
    const fixture = standardFixture();
    fixture.pulls = 403;
    installFakeGithub(fixture);
    const res = await get("/api/changes");
    expect(res.status).toBe(200);
    const body = (await res.json()) as {
      changes: Array<{ review: unknown }>;
      reviewUnavailable: string;
    };
    expect(body.changes.length).toBe(4);
    expect(body.changes.every((c) => c.review === null)).toBe(true);
    expect(body.reviewUnavailable).toContain("pull_requests:read");
  });

  test("matches a review branch that moved past its last patchset through the Change-Id trailer", async () => {
    const fixture = standardFixture();
    fixture.pulls = [pullOf(21, A3, "review/task-402-moved", "open", "dalgona")];
    installFakeGithub(fixture);
    const body = (await (await get("/api/changes")).json()) as {
      changes: Array<{ id: string; review: { number: number } | null }>;
    };
    expect(body.changes.find((c) => c.id === A)?.review?.number).toBe(21);
  });

  test("caps uncached commit reads per request and says so when the list is truncated", async () => {
    const fixture = standardFixture();
    fixture.refs = [{ ref: "refs/suda/main", sha: TRUNK }, { ref: "refs/suda/history", sha: TRUNK }];
    fixture.commits = {};
    fixture.pulls = [];
    for (let k = 0; k < 160; k++) {
      const id = `I${k.toString(16).padStart(40, "0")}`;
      const sha = hex(`c${k.toString(16).padStart(4, "0")}`);
      fixture.refs.push({ ref: `refs/ws/cl/${id}/${sha}`, sha });
      fixture.commits[sha] = { message: `bulk ${k}\n\nChange-Id: ${id}`, parents: [TRUNK], date: day(2) };
    }
    const calls = installFakeGithub(fixture);
    const body = (await (await get("/api/changes")).json()) as {
      changes: unknown[];
      truncated: boolean;
      omittedChanges: number;
      note: string;
      commitLookupLimit: number;
    };
    expect(body.commitLookupLimit).toBe(150);
    expect(body.truncated).toBe(true);
    expect(body.changes.length).toBe(150);
    expect(body.omittedChanges).toBe(10);
    expect(body.note).toContain("Stopped after 150 GitHub reads");
    expect(calls.filter((c) => c.startsWith("git/commits/")).length).toBe(150);
  });

  test("does not list refs/ws/cl patchsets as workspaces on /api/refs", async () => {
    installFakeGithub(standardFixture());
    const body = (await (await get("/api/refs")).json()) as { workspaces: Array<{ ref: string }> };
    expect(body.workspaces.map((w) => w.ref)).toEqual(["refs/ws/alice/fix-header"]);
  });
});

describe("GET /api/changes/<change-id>", () => {
  test("returns numbered patchsets with roots, files, approvals, and carry-over estimates", async () => {
    installFakeGithub(standardFixture());
    const res = await get(`/api/changes/${A}`);
    expect(res.status).toBe(200);
    const body = (await res.json()) as {
      id: string;
      patchsets: Array<{
        number: number;
        sha: string;
        rootSha: string | null;
        date: string;
        message: string;
        files: Array<{ filename: string; previous_filename: string | null; patch: string | null }> | null;
        error: string | null;
        carriesTo: number[];
      }>;
      review: { number: number; state: string } | null;
      reviews: Array<{ login: string; state: string; commitId: string }>;
      approvals: Array<{ login: string; commitId: string; patchset: number | null }>;
      reviewUnavailable: string | null;
    };
    expect(body.id).toBe(A);
    expect(body.patchsets.map((p) => [p.number, p.sha])).toEqual([
      [1, A1],
      [2, A2],
    ]);
    // PS2 walks through the unpublished middle commit A3 to the trunk snapshot.
    expect(body.patchsets[1].rootSha).toBe(TRUNK);
    expect(body.patchsets[0].date).toBe(day(5));
    expect(body.patchsets[0].message).toContain("fix header decode");
    expect(body.patchsets[0].files?.[0]).toMatchObject({
      filename: "ipc/hqq/load.go",
      previous_filename: null,
      patch: OLD_HUNK,
    });
    expect(body.patchsets[0].carriesTo).toEqual([2]);
    expect(body.patchsets[1].carriesTo).toEqual([]);
    expect(body.review).toMatchObject({ number: 7, state: "open" });
    expect(body.reviews.length).toBe(3);
    // Alice approved PS1; Bob's later CHANGES_REQUESTED is not an approval; Carol only commented.
    expect(body.approvals).toEqual([
      expect.objectContaining({ login: "alice", commitId: A1, patchset: 1 }),
    ]);
    expect(body.reviewUnavailable).toBeNull();
  });

  test("reports a patchset with several parentless roots as an error, not as a silent pick", async () => {
    installFakeGithub(standardFixture());
    const body = (await (await get(`/api/changes/${E}`)).json()) as {
      patchsets: Array<{ number: number; error: string | null; files: unknown[] | null; rootSha: string | null }>;
    };
    expect(body.patchsets[0]).toMatchObject({ number: 1, error: null, rootSha: TRUNK });
    expect(body.patchsets[1].number).toBe(2);
    expect(body.patchsets[1].error).toContain("parentless roots");
    expect(body.patchsets[1].rootSha).toBeNull();
    expect(body.patchsets[1].files).toBeNull();
  });

  test("reports a history deeper than the walk limit as an error", async () => {
    installFakeGithub(standardFixture());
    const body = (await (await get(`/api/changes/${F}`)).json()) as {
      patchsets: Array<{ error: string | null }>;
    };
    expect(body.patchsets[0].error).toContain("deeper than 50 commits");
  });

  test("reports unavailable review data without dropping the patchsets when pull requests are forbidden", async () => {
    const fixture = standardFixture();
    fixture.pulls = 403;
    installFakeGithub(fixture);
    const body = (await (await get(`/api/changes/${A}`)).json()) as {
      patchsets: unknown[];
      review: unknown;
      reviews: unknown[];
      reviewUnavailable: string;
    };
    expect(body.patchsets.length).toBe(2);
    expect(body.review).toBeNull();
    expect(body.reviews).toEqual([]);
    expect(body.reviewUnavailable).toContain("pull_requests:read");
  });

  test("keeps the review but reports unavailable reviews when only reviews are forbidden", async () => {
    installFakeGithub(standardFixture());
    const body = (await (await get(`/api/changes/${B}`)).json()) as {
      review: { number: number } | null;
      reviews: unknown[];
      reviewUnavailable: string;
    };
    expect(body.review?.number).toBe(9);
    expect(body.reviews).toEqual([]);
    expect(body.reviewUnavailable).toContain("HTTP 403");
  });

  test("answers 404 for embargoed changes without revealing them", async () => {
    installFakeGithub(standardFixture());
    for (const id of [C, D]) {
      const res = await get(`/api/changes/${id}`);
      expect(res.status).toBe(404);
      const text = await res.text();
      expect(text).not.toContain("sec-");
      expect(text).not.toContain("harden");
    }
  });

  test("rejects malformed ids and embargo-looking names before contacting GitHub", async () => {
    const calls = installFakeGithub(standardFixture());
    for (const path of ["/api/changes/not-an-id", "/api/changes/sec-202610-0a1b2c3d", "/api/changes/I123/extra"]) {
      const res = await get(path);
      expect(res.status).toBe(404);
    }
    expect(calls).toEqual([]);
  });

  test("serves the same detail from cache on a second request without GitHub commit reads", async () => {
    const calls = installFakeGithub(standardFixture());
    const first = await get(`/api/changes/${A}`);
    expect(first.status).toBe(200);
    calls.length = 0;
    const second = await get(`/api/changes/${A}`);
    expect(second.status).toBe(200);
    expect(calls.filter((c) => c.startsWith("git/commits/") || c.startsWith("compare/"))).toEqual([]);
  });
});

describe("GET /api/changes/<N>", () => {
  test("resolves a review pull request number to its change", async () => {
    installFakeGithub(standardFixture());
    const res = await get("/api/changes/7");
    expect(res.status).toBe(200);
    expect(((await res.json()) as { id: string }).id).toBe(A);
  });

  test("shows a review outside the listing window when reached by number, but not from the change id", async () => {
    const fixture = standardFixture();
    fixture.listing = [];
    installFakeGithub(fixture);
    const byNumber = (await (await get("/api/changes/7")).json()) as {
      review: { number: number } | null;
      approvals: unknown[];
    };
    expect(byNumber.review?.number).toBe(7);
    expect(byNumber.approvals.length).toBe(1);
    const byId = (await (await get(`/api/changes/${A}`)).json()) as { review: unknown; approvals: unknown[] };
    expect(byId.review).toBeNull();
    expect(byId.approvals).toEqual([]);
  });

  test("answers 404 JSON when the pull request is missing or has no Change-Id", async () => {
    installFakeGithub(standardFixture());
    const missing = await get("/api/changes/999");
    expect(missing.status).toBe(404);
    expect(((await missing.json()) as { error: string }).error).toContain("#999");

    const noTrailer = await get("/api/changes/12");
    expect(noTrailer.status).toBe(404);
    expect(((await noTrailer.json()) as { error: string }).error).toContain("no Change-Id trailer");
  });

  test("answers 404 JSON with the reason when pull requests cannot be read", async () => {
    const fixture = standardFixture();
    fixture.pulls = 403;
    installFakeGithub(fixture);
    const res = await get("/api/changes/7");
    expect(res.status).toBe(404);
    const body = (await res.json()) as { reviewUnavailable: string };
    expect(body.reviewUnavailable).toContain("pull_requests:read");
  });
});

describe("cache and webhook invalidation", () => {
  test("a valid webhook clears the change ref listing so the next list re-reads GitHub", async () => {
    const calls = installFakeGithub(standardFixture());
    const cachedEnv = { GITHUB_REPO: REPO, GITHUB_TOKEN: "test-token", CACHE_TTL_SECONDS: "60", WEBHOOK_SECRET };
    await get("/api/changes", cachedEnv);
    await get("/api/changes", cachedEnv);
    const listingCalls = () => calls.filter((c) => c.startsWith("git/matching-refs/ws/cl/")).length;
    expect(listingCalls()).toBe(1);

    const hook = await worker.fetch(await signedWebhook("push"), cachedEnv as never);
    expect(hook.status).toBe(200);
    await get("/api/changes", cachedEnv);
    expect(listingCalls()).toBe(2);
  });
});

describe("UI shell paths", () => {
  const assetEnv = (seen: string[]) => ({
    ...ENV,
    ASSETS: {
      async fetch(req: Request | string) {
        const url = typeof req === "string" ? req : req.url;
        seen.push(new URL(url).pathname);
        return new Response(`SHELL:${new URL(url).pathname}`, { status: 200 });
      },
    },
  });

  test("serves index.html for /changes and /changes/<anything>", async () => {
    const seen: string[] = [];
    for (const path of ["/changes", "/changes/", `/changes/${A}`, "/changes/7"]) {
      const res = await worker.fetch(new Request(`https://magpie.gosuda.org${path}`), assetEnv(seen) as never);
      expect(res.status).toBe(200);
      expect(await res.text()).toBe("SHELL:/index.html");
    }
    expect(seen.every((p) => p === "/index.html")).toBe(true);
  });

  test("keeps / and /diff on the static UI and never routes /api/changes to assets", async () => {
    const seen: string[] = [];
    installFakeGithub(standardFixture());
    const root = await worker.fetch(new Request("https://magpie.gosuda.org/"), assetEnv(seen) as never);
    expect(await root.text()).toBe("SHELL:/");
    const diffPage = await worker.fetch(new Request("https://magpie.gosuda.org/diff"), assetEnv(seen) as never);
    expect(await diffPage.text()).toBe("SHELL:/index.html");
    const api = await worker.fetch(new Request("https://magpie.gosuda.org/api/changes"), assetEnv(seen) as never);
    expect(api.headers.get("content-type")).toContain("application/json");
    expect(seen).toEqual(["/", "/index.html"]);
  });

  test("answers 404 for shell paths when no assets are bound", async () => {
    const res = await get("/changes");
    expect(res.status).toBe(404);
  });
});
