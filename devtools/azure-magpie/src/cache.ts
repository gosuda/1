// Read-through cache for GitHub responses. Commits and compares are immutable by sha, so they can
// live for days. The Cache API is used when the runtime provides it (Cloudflare Workers); otherwise
// a bounded in-memory map serves the same purpose (tests, other runtimes). Values are stored as JSON
// so every reader gets its own copy.

const MEMORY_LIMIT = 1_000;
const CACHE_ORIGIN = "https://magpie-cache.invalid/v1/";

interface MemoryEntry {
  body: string;
  expiresAtMs: number;
}

const memory = new Map<string, MemoryEntry>();

function edgeCache(): Cache | null {
  const runtime = globalThis as { caches?: { default?: Cache } };
  return runtime.caches?.default ?? null;
}

export async function readThrough<T>(
  key: string,
  ttlSeconds: number,
  load: () => Promise<T>,
): Promise<T> {
  const edge = edgeCache();
  if (edge) {
    const cacheUrl = `${CACHE_ORIGIN}${encodeURIComponent(key)}`;
    try {
      const hit = await edge.match(cacheUrl);
      if (hit) {
        return (await hit.json()) as T;
      }
    } catch {
      // A broken cache lookup only costs a GitHub read.
    }
    const value = await load();
    try {
      await edge.put(
        cacheUrl,
        new Response(JSON.stringify(value), {
          headers: {
            "content-type": "application/json; charset=utf-8",
            "cache-control": `public, max-age=${ttlSeconds}`,
          },
        }),
      );
    } catch {
      // Ignored: the value is still returned to the caller.
    }
    return value;
  }

  const now = Date.now();
  const entry = memory.get(key);
  if (entry && entry.expiresAtMs > now) {
    return JSON.parse(entry.body) as T;
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

// Tests use this to start from an empty cache.
export function resetReadCacheForTests(): void {
  memory.clear();
}
