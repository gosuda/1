import type { Env } from "./env";
import { buildGithubHeaders } from "./github-auth";

// A GitHub REST failure, carrying the HTTP status so callers can degrade by permission.
export class GithubReadError extends Error {
  readonly status: number;

  constructor(status: number, path: string) {
    super(`GitHub API ${status} for ${path}`);
    this.name = "GithubReadError";
    this.status = status;
  }
}

// Caps the uncached GitHub reads one request may make, so one page load cannot fan out unbounded.
export class BudgetExhaustedError extends Error {
  constructor(limit: number) {
    super(`request read budget of ${limit} GitHub calls used up; reload to continue`);
    this.name = "BudgetExhaustedError";
  }
}

export class ReadBudget {
  private remaining: number;

  constructor(readonly limit: number) {
    this.remaining = limit;
  }

  spend(): void {
    if (this.remaining <= 0) {
      throw new BudgetExhaustedError(this.limit);
    }
    this.remaining -= 1;
  }
}

export async function githubGet<T>(env: Env, repo: string, path: string): Promise<T> {
  const headers = await buildGithubHeaders(env);
  const resp = await fetch(`https://api.github.com/repos/${repo}/${path}`, { headers });
  if (!resp.ok) {
    throw new GithubReadError(resp.status, path);
  }
  return (await resp.json()) as T;
}

// Maps items with at most `limit` in flight; results keep the input order.
export async function mapLimit<T, R>(
  items: readonly T[],
  limit: number,
  fn: (item: T) => Promise<R>,
): Promise<R[]> {
  const out: R[] = new Array(items.length);
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
