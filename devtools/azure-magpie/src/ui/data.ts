// Client-side data access: the /api/refs snapshot (trunk, workspaces, projects) and the change API.
import type { ChangeDetailPayload, ChangeListPayload } from "../api-types";

export interface WorkspaceItem {
  ref: string;
  label: string;
  sha: string;
}

export interface ProjectItem {
  prefix: string;
  mirror: string;
}

export interface RefsSnapshot {
  repo: string;
  mainSha: string;
  historySha: string;
  workspaces: WorkspaceItem[];
  projects: ProjectItem[];
}

// status is the HTTP status, or 0 when the request never completed.
export type Loaded<T> = { ok: true; value: T } | { ok: false; status: number; message: string };

const state: { refs: RefsSnapshot | null; handoffHead: string | null } = {
  refs: null,
  handoffHead: null,
};

export function currentRefs(): RefsSnapshot | null {
  return state.refs;
}

// Head reference chosen on another page (for example a workspace's "Use as head" button).
export function takeHandoffHead(): string | null {
  const value = state.handoffHead;
  state.handoffHead = null;
  return value;
}

export function handOffHead(ref: string): void {
  state.handoffHead = ref;
}

async function getJson<T>(url: string): Promise<Loaded<T>> {
  try {
    const resp = await fetch(url, { headers: { accept: "application/json" }, cache: "no-store" });
    const body: unknown = await resp.json().catch(() => null);
    if (!resp.ok) {
      const message =
        body && typeof body === "object" && "error" in body && typeof body.error === "string"
          ? body.error
          : `HTTP ${resp.status}`;
      return { ok: false, status: resp.status, message };
    }
    return { ok: true, value: body as T };
  } catch {
    return { ok: false, status: 0, message: "The network request failed. Check your connection and try again." };
  }
}

interface RawRefs {
  repo?: string;
  "suda/main"?: string;
  "suda/history"?: string;
  workspaces?: WorkspaceItem[];
  projects?: ProjectItem[];
}

export async function loadRefs(): Promise<Loaded<RefsSnapshot>> {
  const result = await getJson<RawRefs>("/api/refs");
  if (!result.ok) {
    return result;
  }
  const raw = result.value;
  const snapshot: RefsSnapshot = {
    repo: raw.repo || "gosuda/1",
    mainSha: raw["suda/main"] || "",
    historySha: raw["suda/history"] || "",
    workspaces: Array.isArray(raw.workspaces) ? raw.workspaces : [],
    projects: Array.isArray(raw.projects) ? raw.projects : [],
  };
  state.refs = snapshot;
  return { ok: true, value: snapshot };
}

export function loadChanges(): Promise<Loaded<ChangeListPayload>> {
  return getJson<ChangeListPayload>("/api/changes");
}

export function loadChange(param: string): Promise<Loaded<ChangeDetailPayload>> {
  return getJson<ChangeDetailPayload>(`/api/changes/${encodeURIComponent(param)}`);
}
