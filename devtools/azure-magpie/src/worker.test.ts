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
    expect(buildRedirectLocation("/", "gosuda/0", SAMPLE_REFS)).toBe(
      `https://github.com/gosuda/0/tree/${SAMPLE_REFS.mainSha}`,
    );
    expect(buildRedirectLocation("/ipc/hqq/HQQ", "gosuda/0", SAMPLE_REFS)).toBe(
      `https://github.com/gosuda/0/tree/${SAMPLE_REFS.mainSha}/ipc/hqq/HQQ`,
    );
    expect(buildRedirectLocation("/tree/web/gosuda", "gosuda/0", SAMPLE_REFS)).toBe(
      `https://github.com/gosuda/0/tree/${SAMPLE_REFS.mainSha}/web/gosuda`,
    );
  });

  test("routes blob and commit paths to refs/suda/main", () => {
    expect(buildRedirectLocation("/blob/ipc/hqq/HQQ/go.mod", "gosuda/0", SAMPLE_REFS)).toBe(
      `https://github.com/gosuda/0/blob/${SAMPLE_REFS.mainSha}/ipc/hqq/HQQ/go.mod`,
    );
    expect(buildRedirectLocation("/commit", "gosuda/0", SAMPLE_REFS)).toBe(
      `https://github.com/gosuda/0/commit/${SAMPLE_REFS.mainSha}`,
    );
  });

  test("routes history and blame paths to refs/suda/history", () => {
    expect(buildRedirectLocation("/history", "gosuda/0", SAMPLE_REFS)).toBe(
      `https://github.com/gosuda/0/commits/${SAMPLE_REFS.historySha}`,
    );
    expect(buildRedirectLocation("/history/net/ivnp", "gosuda/0", SAMPLE_REFS)).toBe(
      `https://github.com/gosuda/0/commits/${SAMPLE_REFS.historySha}/net/ivnp`,
    );
    expect(buildRedirectLocation("/blame/net/ivnp/go.mod", "gosuda/0", SAMPLE_REFS)).toBe(
      `https://github.com/gosuda/0/blame/${SAMPLE_REFS.historySha}/net/ivnp/go.mod`,
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
      GITHUB_REPO: "gosuda/0",
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
      const env = { GITHUB_REPO: "gosuda/0", GITHUB_TOKEN: "test-token", CACHE_TTL_SECONDS: "60" };

      const resRoot = await worker.fetch(new Request("https://0.gosuda.org/ipc/hqq/HQQ"), env);
      expect(resRoot.status).toBe(302);
      expect(resRoot.headers.get("location")).toBe(
        `https://github.com/gosuda/0/tree/${SAMPLE_REFS.mainSha}/ipc/hqq/HQQ`,
      );

      const resApi = await worker.fetch(new Request("https://0.gosuda.org/api/refs"), env);
      expect(resApi.status).toBe(200);
      const apiBody = (await resApi.json()) as {
        repo: string;
        "suda/main": string;
        workspaces: Array<{ label: string; sha: string }>;
      };
      expect(apiBody.repo).toBe("gosuda/0");
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
        `https://github.com/gosuda/0/compare/${SAMPLE_REFS.mainSha}..${wsSha}`,
      );
    } finally {
      globalThis.fetch = origFetch;
    }
  });

  test("rejects webhook requests with invalid HMAC signature and dispatches valid mirror pushes", async () => {
    const secret = "webhook-test-secret";
    const payload = JSON.stringify({
      ref: "refs/heads/main",
      repository: { full_name: "gosuda/HQQ", name: "HQQ", default_branch: "main" },
      head_commit: { message: "fix: external contributor commit" },
    });

    const badReq = new Request("https://0.gosuda.org/webhook", {
      method: "POST",
      headers: {
        "x-github-event": "push",
        "x-hub-signature-256": "sha256=deadbeef",
      },
      body: payload,
    });
    const badRes = await worker.fetch(badReq, { WEBHOOK_SECRET: secret, GITHUB_TOKEN: "tok" });
    expect(badRes.status).toBe(401);

    const key = await crypto.subtle.importKey(
      "raw",
      new TextEncoder().encode(secret),
      { name: "HMAC", hash: "SHA-256" },
      false,
      ["sign"],
    );
    const sigBuf = await crypto.subtle.sign("HMAC", key, new TextEncoder().encode(payload));
    const sigHex = Array.from(new Uint8Array(sigBuf))
      .map((b) => b.toString(16).padStart(2, "0"))
      .join("");

    let dispatchedUrl = "";
    let dispatchedBody = "";
    const origFetch = globalThis.fetch;
    globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
      dispatchedUrl = String(input);
      dispatchedBody = String(init?.body || "");
      return new Response(null, { status: 204 });
    }) as typeof fetch;

    try {
      const goodReq = new Request("https://0.gosuda.org/webhook", {
        method: "POST",
        headers: {
          "x-github-event": "push",
          "x-hub-signature-256": `sha256=${sigHex}`,
        },
        body: payload,
      });
      const goodRes = await worker.fetch(goodReq, {
        GITHUB_REPO: "gosuda/0",
        WEBHOOK_SECRET: secret,
        GITHUB_TOKEN: "tok",
      });
      expect(goodRes.status).toBe(200);
      expect(dispatchedUrl).toBe("https://api.github.com/repos/gosuda/azure-magpie/dispatches");
      expect(JSON.parse(dispatchedBody)).toEqual({
        event_type: "capy-inbound",
        client_payload: {
          workflow: "HQQ",
          ref: "refs/heads/main",
          mode: "ws",
        },
      });
    } finally {
      globalThis.fetch = origFetch;
    }
  });

  test("dispatches capy-inbound in ws mode for mirror pushes to a non-main default branch", async () => {
    const dispatched: unknown[] = [];
    const origFetch = globalThis.fetch;
    globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
      if (String(input) === "https://api.github.com/repos/gosuda/azure-magpie/dispatches") {
        dispatched.push(JSON.parse(String(init?.body)));
      }
      return new Response(null, { status: 204 });
    }) as typeof fetch;

    try {
      const res = await worker.fetch(
        await signedWebhookRequest("push", {
          ref: "refs/heads/master",
          repository: { full_name: "gosuda/portalite", name: "portalite", default_branch: "master" },
          head_commit: { message: "fix: external contributor commit" },
        }),
        { GITHUB_REPO: "gosuda/0", WEBHOOK_SECRET, GITHUB_TOKEN: "tok" },
      );
      expect(res.status).toBe(200);
      expect(dispatched).toEqual([
        {
          event_type: "capy-inbound",
          client_payload: { workflow: "portalite", ref: "refs/heads/master", mode: "ws" },
        },
      ]);
    } finally {
      globalThis.fetch = origFetch;
    }
  });

  test("ignores pushes to non-default branches without calling GitHub", async () => {
    const calls: string[] = [];
    const origFetch = globalThis.fetch;
    globalThis.fetch = (async (input: RequestInfo | URL) => {
      calls.push(String(input));
      return new Response(null, { status: 204 });
    }) as typeof fetch;

    try {
      const res = await worker.fetch(
        await signedWebhookRequest("push", {
          ref: "refs/heads/feature/external",
          repository: { full_name: "gosuda/HQQ", name: "HQQ", default_branch: "main" },
          head_commit: { message: "feat: external branch" },
        }),
        { GITHUB_REPO: "gosuda/0", WEBHOOK_SECRET, GITHUB_TOKEN: "tok" },
      );
      expect(res.status).toBe(200);
      expect(await res.json()).toEqual({ status: "ignored-event" });
      expect(calls).toEqual([]);
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
        { GITHUB_REPO: "gosuda/0", GITHUB_TOKEN: "test-token", CACHE_TTL_SECONDS: "60" },
      );
      expect(res.status).toBe(302);
      expect(res.headers.get("location")).toBe(
        `https://github.com/gosuda/0/tree/${SAMPLE_REFS.mainSha}/api/land`,
      );
      expect(calls.filter((c) => c.includes("/dispatches") || c.startsWith("POST"))).toEqual([]);
    } finally {
      globalThis.fetch = origFetch;
    }
  });
});
