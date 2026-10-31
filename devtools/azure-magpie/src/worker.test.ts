import { describe, expect, test } from "bun:test";
import worker, { buildRedirectLocation, parseDiffSpec } from "./worker";

const SAMPLE_REFS = {
  mainSha: "7444de65a57c4604e430e202f6754c0a0408b7ed",
  historySha: "9999de65a57c4604e430e202f6754c0a0408b7ed",
  fetchedAtMs: Date.now(),
};

const WEBHOOK_SECRET = "webhook-test-secret";

async function signedWebhookRequest(event: string, payload: unknown): Promise<Request> {
  const body = JSON.stringify(payload);
  const key = await crypto.subtle.importKey(
    "raw",
    new TextEncoder().encode(WEBHOOK_SECRET),
    { name: "HMAC", hash: "SHA-256" },
    false,
    ["sign"],
  );
  const sigBuf = await crypto.subtle.sign("HMAC", key, new TextEncoder().encode(body));
  const sigHex = Array.from(new Uint8Array(sigBuf))
    .map((b) => b.toString(16).padStart(2, "0"))
    .join("");
  return new Request("https://0.gosuda.org/webhook", {
    method: "POST",
    headers: {
      "x-github-event": event,
      "x-hub-signature-256": `sha256=${sigHex}`,
    },
    body,
  });
}

describe("buildRedirectLocation", () => {
  test("routes root and subtree paths to refs/suda/main tree URLs", () => {
    expect(buildRedirectLocation("/", "gosuda/1", SAMPLE_REFS)).toBe(
      `https://github.com/gosuda/1/tree/${SAMPLE_REFS.mainSha}`,
    );
    expect(buildRedirectLocation("/ipc/hqq/HQQ", "gosuda/1", SAMPLE_REFS)).toBe(
      `https://github.com/gosuda/1/tree/${SAMPLE_REFS.mainSha}/ipc/hqq/HQQ`,
    );
    expect(buildRedirectLocation("/tree/web/gosuda", "gosuda/1", SAMPLE_REFS)).toBe(
      `https://github.com/gosuda/1/tree/${SAMPLE_REFS.mainSha}/web/gosuda`,
    );
  });

  test("routes blob and commit paths to refs/suda/main", () => {
    expect(buildRedirectLocation("/blob/ipc/hqq/HQQ/go.mod", "gosuda/1", SAMPLE_REFS)).toBe(
      `https://github.com/gosuda/1/blob/${SAMPLE_REFS.mainSha}/ipc/hqq/HQQ/go.mod`,
    );
    expect(buildRedirectLocation("/commit", "gosuda/1", SAMPLE_REFS)).toBe(
      `https://github.com/gosuda/1/commit/${SAMPLE_REFS.mainSha}`,
    );
  });

  test("routes history and blame paths to refs/suda/history", () => {
    expect(buildRedirectLocation("/history", "gosuda/1", SAMPLE_REFS)).toBe(
      `https://github.com/gosuda/1/commits/${SAMPLE_REFS.historySha}`,
    );
    expect(buildRedirectLocation("/history/net/ivnp", "gosuda/1", SAMPLE_REFS)).toBe(
      `https://github.com/gosuda/1/commits/${SAMPLE_REFS.historySha}/net/ivnp`,
    );
    expect(buildRedirectLocation("/blame/net/ivnp/go.mod", "gosuda/1", SAMPLE_REFS)).toBe(
      `https://github.com/gosuda/1/blame/${SAMPLE_REFS.historySha}/net/ivnp/go.mod`,
    );
  });
});

describe("parseDiffSpec", () => {
  test("parses single workspace reference against suda/main using two-dot mode", () => {
    expect(parseDiffSpec("/diff/ws/agent-dalgona/task-402", new URLSearchParams())).toEqual({
      baseRef: "suda/main",
      headRef: "ws/agent-dalgona/task-402",
      operator: "..",
    });
  });

  test("parses explicit two-dot and three-dot comparison paths", () => {
    expect(
      parseDiffSpec("/diff/suda/main..ws/agent-dalgona/task-402", new URLSearchParams()),
    ).toEqual({
      baseRef: "suda/main",
      headRef: "ws/agent-dalgona/task-402",
      operator: "..",
    });
    expect(
      parseDiffSpec("/compare/7444de6...abcd123", new URLSearchParams()),
    ).toEqual({
      baseRef: "7444de6",
      headRef: "abcd123",
      operator: "...",
    });
  });

  test("returns null for /diff index when no head parameter is provided", () => {
    expect(parseDiffSpec("/diff", new URLSearchParams())).toBeNull();
    expect(
      parseDiffSpec(
        "/diff",
        new URLSearchParams("base=suda/main&head=ws/agent-a/task-1&mode=2dot"),
      ),
    ).toEqual({
      baseRef: "suda/main",
      headRef: "ws/agent-a/task-1",
      operator: "..",
    });
  });
});

describe("worker.fetch", () => {
  test("serves Cloudflare Pages static assets on / and /diff when ASSETS binding is present", async () => {
    const env = {
      GITHUB_REPO: "gosuda/1",
      ASSETS: {
        async fetch(req: Request | string) {
          const url = typeof req === "string" ? req : req.url;
          return new Response(`STATIC:${new URL(url).pathname}`, { status: 200 });
        },
      },
    };
    const rootRes = await worker.fetch(new Request("https://magpie.gosuda.org/"), env);
    expect(rootRes.status).toBe(200);
    expect(await rootRes.text()).toBe("STATIC:/");
  });

  test("redirects GET requests and serves /refs.json and /api/refs using GitHub matching-refs response", async () => {
    const wsSha = "1111222233334444555566667777888899990000";
    const origFetch = globalThis.fetch;
    globalThis.fetch = (async (input: RequestInfo | URL) => {
      const url = String(input);
      if (url.endsWith("/git/matching-refs/suda/")) {
        return new Response(
          JSON.stringify([
            { ref: "refs/suda/main", object: { sha: SAMPLE_REFS.mainSha } },
            { ref: "refs/suda/history", object: { sha: SAMPLE_REFS.historySha } },
          ]),
          { status: 200, headers: { "content-type": "application/json" } },
        );
      }
      if (url.endsWith("/git/ref/ws/agent-dalgona/task-402")) {
        return new Response(JSON.stringify({ object: { sha: wsSha } }), {
          status: 200,
          headers: { "content-type": "application/json" },
        });
      }
      if (url.endsWith("/git/matching-refs/ws/")) {
        return new Response(
          JSON.stringify([
            { ref: "refs/ws/agent-dalgona/task-402", object: { sha: wsSha } },
          ]),
          { status: 200, headers: { "content-type": "application/json" } },
        );
      }
      if (url.endsWith("/git/matching-refs/heads/land/")) {
        return new Response(JSON.stringify([]), {
          status: 200,
          headers: { "content-type": "application/json" },
        });
      }
      return new Response("not found", { status: 404 });
    }) as typeof fetch;

    try {
      const env = { GITHUB_REPO: "gosuda/1", GITHUB_TOKEN: "test-token", CACHE_TTL_SECONDS: "60" };

      const resRoot = await worker.fetch(new Request("https://0.gosuda.org/ipc/hqq/HQQ"), env);
      expect(resRoot.status).toBe(302);
      expect(resRoot.headers.get("location")).toBe(
        `https://github.com/gosuda/1/tree/${SAMPLE_REFS.mainSha}/ipc/hqq/HQQ`,
      );

      const resApi = await worker.fetch(new Request("https://0.gosuda.org/api/refs"), env);
      expect(resApi.status).toBe(200);
      const apiBody = (await resApi.json()) as {
        repo: string;
        "suda/main": string;
        workspaces: Array<{ label: string; sha: string }>;
      };
      expect(apiBody.repo).toBe("gosuda/1");
      expect(apiBody["suda/main"]).toBe(SAMPLE_REFS.mainSha);
      expect(apiBody.workspaces).toEqual([
        {
          ref: "refs/ws/agent-dalgona/task-402",
          label: "ws/agent-dalgona/task-402",
          sha: wsSha,
        },
      ]);

      const diffRedirect = await worker.fetch(
        new Request("https://0.gosuda.org/diff/ws/agent-dalgona/task-402"),
        env,
      );
      expect(diffRedirect.status).toBe(302);
      expect(diffRedirect.headers.get("location")).toBe(
        `https://github.com/gosuda/1/compare/${SAMPLE_REFS.mainSha}..${wsSha}`,
      );
    } finally {
      globalThis.fetch = origFetch;
    }
  });

  test("resolves trunk and workspace refs and builds every link on gosuda/1 without touching gosuda/0", async () => {
    const wsSha = "1111222233334444555566667777888899990000";
    const urls: string[] = [];
    const origFetch = globalThis.fetch;
    globalThis.fetch = (async (input: RequestInfo | URL) => {
      const url = String(input);
      urls.push(url);
      if (url.endsWith("/git/matching-refs/suda/")) {
        return new Response(
          JSON.stringify([
            { ref: "refs/suda/main", object: { sha: SAMPLE_REFS.mainSha } },
            { ref: "refs/suda/history", object: { sha: SAMPLE_REFS.historySha } },
          ]),
          { status: 200 },
        );
      }
      if (url.endsWith("/git/ref/heads/land/task-9")) {
        return new Response(JSON.stringify({ object: { sha: wsSha } }), { status: 200 });
      }
      return new Response("not found", { status: 404 });
    }) as typeof fetch;
    try {
      const env = { GITHUB_TOKEN: "t", CACHE_TTL_SECONDS: "0" }; // no GITHUB_REPO: the default applies
      const diff = await worker.fetch(new Request("https://0.gosuda.org/diff/land/task-9?format=json"), env);
      expect(diff.status).toBe(200);
      const body = (await diff.json()) as { compareUrl: string };
      expect(body.compareUrl).toBe(`https://github.com/gosuda/1/compare/${SAMPLE_REFS.mainSha}..${wsSha}`);

      const tree = await worker.fetch(new Request("https://0.gosuda.org/tree/ipc/hqq/HQQ"), env);
      expect(tree.headers.get("location")).toBe(`https://github.com/gosuda/1/tree/${SAMPLE_REFS.mainSha}/ipc/hqq/HQQ`);
      const hist = await worker.fetch(new Request("https://0.gosuda.org/history/net/grt"), env);
      expect(hist.headers.get("location")).toBe(`https://github.com/gosuda/1/commits/${SAMPLE_REFS.historySha}/net/grt`);

      expect(urls.length).toBeGreaterThan(0);
      expect(urls.filter((u) => u.includes("/repos/gosuda/0/"))).toEqual([]);
      expect(urls.some((u) => u.includes("/repos/gosuda/1/"))).toBe(true);
    } finally {
      globalThis.fetch = origFetch;
    }
  });

  test("rejects webhook requests with an invalid HMAC signature without waking the lander", async () => {
    const calls: string[] = [];
    const origFetch = globalThis.fetch;
    globalThis.fetch = (async (input: RequestInfo | URL) => {
      calls.push(String(input));
      return new Response(null, { status: 204 });
    }) as typeof fetch;
    try {
      const res = await worker.fetch(
        new Request("https://0.gosuda.org/webhook", {
          method: "POST",
          headers: { "x-github-event": "push", "x-hub-signature-256": "sha256=deadbeef" },
          body: "{}",
        }),
        { WEBHOOK_SECRET, LANDER_WAKE_URL: "https://lander.example/wake", LANDER_WAKE_SECRET: "wake" },
      );
      expect(res.status).toBe(401);
      expect(calls).toEqual([]);
    } finally {
      globalThis.fetch = origFetch;
    }
  });

  test("a valid webhook only wakes the lander with an HMAC-signed body and never dispatches work", async () => {
    const wakeSecret = "wake-secret";
    const seen: Array<{ url: string; body: string; sig: string }> = [];
    const origFetch = globalThis.fetch;
    globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
      seen.push({
        url: String(input),
        body: String(init?.body instanceof Uint8Array ? new TextDecoder().decode(init.body) : init?.body),
        sig: new Headers(init?.headers).get("x-hub-signature-256") || "",
      });
      return new Response(null, { status: 202 });
    }) as typeof fetch;
    try {
      const res = await worker.fetch(
        await signedWebhookRequest("push", {
          ref: "refs/heads/main",
          repository: { full_name: "gosuda/HQQ", name: "HQQ", default_branch: "main" },
          head_commit: { message: "GitOrigin-RevId: forged" },
        }),
        { WEBHOOK_SECRET, LANDER_WAKE_URL: "https://lander.example/wake", LANDER_WAKE_SECRET: wakeSecret },
      );
      expect(res.status).toBe(200);
      expect(await res.json()).toEqual({ status: "woken", code: 202 });
      expect(seen.length).toBe(1);
      expect(seen[0].url).toBe("https://lander.example/wake");
      // The payload (commit message, repository, ref) is not forwarded as input.
      expect(JSON.parse(seen[0].body)).toEqual({ event: "push" });
      const key = await crypto.subtle.importKey(
        "raw",
        new TextEncoder().encode(wakeSecret),
        { name: "HMAC", hash: "SHA-256" },
        false,
        ["sign"],
      );
      const mac = new Uint8Array(await crypto.subtle.sign("HMAC", key, new TextEncoder().encode(seen[0].body)));
      const hex = Array.from(mac)
        .map((b) => b.toString(16).padStart(2, "0"))
        .join("");
      expect(seen[0].sig).toBe(`sha256=${hex}`);
    } finally {
      globalThis.fetch = origFetch;
    }
  });

  test("without wake configuration the webhook only clears the cache, and a failing lander yields 502", async () => {
    const origFetch = globalThis.fetch;
    let calls = 0;
    globalThis.fetch = (async () => {
      calls++;
      throw new Error("lander down");
    }) as typeof fetch;
    try {
      const quiet = await worker.fetch(await signedWebhookRequest("push", { ref: "refs/heads/x" }), { WEBHOOK_SECRET });
      expect(quiet.status).toBe(200);
      expect(await quiet.json()).toEqual({ status: "cache-cleared" });
      expect(calls).toBe(0);

      const failing = await worker.fetch(await signedWebhookRequest("push", { ref: "refs/heads/x" }), {
        WEBHOOK_SECRET,
        LANDER_WAKE_URL: "https://lander.example/wake",
        LANDER_WAKE_SECRET: "wake",
      });
      expect(failing.status).toBe(502);
    } finally {
      globalThis.fetch = origFetch;
    }
  });

  test("GET /api/land never dispatches and falls through to the normal redirect", async () => {
    const calls: string[] = [];
    const origFetch = globalThis.fetch;
    globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      calls.push(`${init?.method ?? "GET"} ${url}`);
      if (url.endsWith("/git/matching-refs/suda/")) {
        return new Response(
          JSON.stringify([{ ref: "refs/suda/main", object: { sha: SAMPLE_REFS.mainSha } }]),
          { status: 200, headers: { "content-type": "application/json" } },
        );
      }
      return new Response("not found", { status: 404 });
    }) as typeof fetch;

    try {
      const res = await worker.fetch(
        new Request("https://0.gosuda.org/api/land?ref=refs/heads/land/task-402"),
        { GITHUB_REPO: "gosuda/1", GITHUB_TOKEN: "test-token", CACHE_TTL_SECONDS: "60" },
      );
      expect(res.status).toBe(302);
      expect(res.headers.get("location")).toBe(
        `https://github.com/gosuda/1/tree/${SAMPLE_REFS.mainSha}/api/land`,
      );
      expect(calls.filter((c) => c.includes("/dispatches") || c.startsWith("POST"))).toEqual([]);
    } finally {
      globalThis.fetch = origFetch;
    }
  });
});
