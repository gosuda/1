interface WorkspaceItem {
  ref: string;
  label: string;
  sha: string;
}

interface ProjectItem {
  prefix: string;
  mirror: string;
}

interface ApiRefsPayload {
  repo: string;
  "suda/main": string;
  "suda/history": string;
  workspaces?: WorkspaceItem[];
  projects?: ProjectItem[];
}

const DEFAULT_PROJECTS: ProjectItem[] = [
  { prefix: "web/gosuda", mirror: "gosuda/website" },
  { prefix: "web/securl", mirror: "gosuda/securl" },
  { prefix: "net/ivnp", mirror: "gosuda/IVNP" },
  { prefix: "net/portalite", mirror: "gosuda/portalite" },
  { prefix: "ipc/hqq/HQQ", mirror: "gosuda/HQQ" },
  { prefix: "ipc/hqq/hqq-sqlite3", mirror: "gosuda/hqq-sqlite3" },
  { prefix: "ipc/hqq/hqq-zstd", mirror: "gosuda/hqq-zstd" },
  { prefix: "devtools/azure-magpie", mirror: "gosuda/azure-magpie" },
];

const state: {
  repo: string;
  mainSha: string;
  historySha: string;
  workspaces: WorkspaceItem[];
  projects: ProjectItem[];
} = {
  repo: "gosuda/0",
  mainSha: "",
  historySha: "",
  workspaces: [],
  projects: DEFAULT_PROJECTS,
};

function byId<T extends HTMLElement>(id: string): T {
  const el = document.getElementById(id);
  if (!el) {
    throw new Error(`Missing DOM element #${id}`);
  }
  return el as T;
}

function resolveKnownSha(refSpec: string): string {
  const clean = refSpec.trim().replace(/^\/+|\/+$/g, "");
  if ((clean === "suda/main" || clean === "refs/suda/main" || clean === "main") && state.mainSha) {
    return state.mainSha;
  }
  if (
    (clean === "suda/history" || clean === "refs/suda/history" || clean === "history") &&
    state.historySha
  ) {
    return state.historySha;
  }
  const found = state.workspaces.find(
    (w) => w.label === clean || w.ref === clean || w.ref === `refs/${clean}`,
  );
  if (found) {
    return found.sha;
  }
  return clean;
}

function updateDiffOutputs(): void {
  const baseInput = byId<HTMLInputElement>("base-ref");
  const headInput = byId<HTMLInputElement>("head-ref");
  const modeSelect = byId<HTMLSelectElement>("diff-mode");

  const baseRef = baseInput.value.trim() || "suda/main";
  const headRef = headInput.value.trim() || "ws/agent-dalgona/task-402";
  const op = modeSelect.value === "..." ? "..." : "..";

  const shortPath =
    baseRef === "suda/main" && op === ".."
      ? `/diff/${headRef}`
      : `/diff/${baseRef}${op}${headRef}`;
  const fullShortUrl = `${window.location.origin}${shortPath}`;

  const baseResolved = resolveKnownSha(baseRef);
  const headResolved = resolveKnownSha(headRef);
  const compareUrl = `https://github.com/${state.repo}/compare/${baseResolved}${op}${headResolved}`;

  byId<HTMLElement>("short-url-output").textContent = fullShortUrl;
  byId<HTMLElement>("github-url-output").textContent = compareUrl;
  byId<HTMLAnchorElement>("open-github-btn").href = shortPath;
}

async function resolveExactShasFromWorker(): Promise<void> {
  const baseRef = byId<HTMLInputElement>("base-ref").value.trim() || "suda/main";
  const headRef = byId<HTMLInputElement>("head-ref").value.trim();
  const op = byId<HTMLSelectElement>("diff-mode").value === "..." ? "..." : "..";
  if (!headRef) {
    return;
  }
  const btn = byId<HTMLButtonElement>("resolve-sha-btn");
  const prevText = btn.textContent;
  btn.textContent = "Resolving…";
  try {
    const resp = await fetch(`/diff/${baseRef}${op}${headRef}?format=json`);
    if (!resp.ok) {
      throw new Error(`HTTP ${resp.status}`);
    }
    const data = (await resp.json()) as { compareUrl?: string };
    if (data.compareUrl) {
      byId<HTMLElement>("github-url-output").textContent = data.compareUrl;
      byId<HTMLAnchorElement>("open-github-btn").href = data.compareUrl;
    }
  } catch {
    updateDiffOutputs();
  } finally {
    btn.textContent = prevText;
  }
}

function renderWorkspacesTable(): void {
  const tbody = byId<HTMLTableSectionElement>("workspaces-tbody");
  const filter = byId<HTMLInputElement>("ws-filter").value.trim().toLowerCase();

  const filtered = state.workspaces.filter(
    (w) =>
      !filter ||
      w.ref.toLowerCase().includes(filter) ||
      w.label.toLowerCase().includes(filter) ||
      w.sha.toLowerCase().includes(filter),
  );

  if (filtered.length === 0) {
    tbody.innerHTML = `<tr><td colspan="4" class="empty-cell">No matching <code>refs/ws/*</code> or <code>refs/heads/land/*</code> branches found.</td></tr>`;
    return;
  }

  tbody.innerHTML = filtered
    .map((item) => {
      const ns = item.label.startsWith("land/") ? "land" : "ws";
      const shortSha = item.sha.slice(0, 10);
      const shortDiffPath = `/diff/${escapeHtml(item.label)}`;
      return `<tr>
        <td><span class="badge-ns">${ns}</span></td>
        <td><code>${escapeHtml(item.ref)}</code></td>
        <td><code>${escapeHtml(shortSha)}</code></td>
        <td>
          <div class="output-actions">
            <button type="button" class="btn btn-secondary" data-load-head="${escapeHtml(item.label)}">Use as Head</button>
            <a class="btn btn-primary" href="${shortDiffPath}" target="_blank" rel="noopener">Open Diff</a>
          </div>
        </td>
      </tr>`;
    })
    .join("");
}

function renderProjectsGrid(): void {
  const grid = byId<HTMLElement>("projects-grid");
  grid.innerHTML = state.projects
    .map((proj) => {
      const p = escapeHtml(proj.prefix);
      const m = escapeHtml(proj.mirror);
      return `<div class="project-card">
        <div>
          <div class="project-path">//${p}</div>
          <div class="project-mirror">${m}</div>
        </div>
        <div class="project-links">
          <a href="/tree/${p}" target="_blank" rel="noopener">Tree</a>
          <a href="/history/${p}" target="_blank" rel="noopener">History</a>
          <a href="https://github.com/${m}" target="_blank" rel="noopener">Mirror</a>
        </div>
      </div>`;
    })
    .join("");
}

function updateSubtreeJumpLinks(): void {
  const raw = byId<HTMLInputElement>("custom-path").value.trim().replace(/^\/+|\/+$/g, "");
  byId<HTMLAnchorElement>("jump-tree-btn").href = raw ? `/tree/${raw}` : "/tree";
  byId<HTMLAnchorElement>("jump-history-btn").href = raw ? `/history/${raw}` : "/history";
  byId<HTMLAnchorElement>("jump-blame-btn").href = raw ? `/blame/${raw}` : "/history";
}

function updateCliSnippet(): void {
  const actor = byId<HTMLInputElement>("cli-actor").value.trim() || "agent-dalgona";
  const task = byId<HTMLInputElement>("cli-task").value.trim() || "task-402";
  const paths = byId<HTMLInputElement>("cli-paths").value.trim() || "ipc/hqq/HQQ";
  const wsId = `${actor}/${task}`;

  const snippet = [
    `git config include.path ../.gitconfig`,
    `git suda-init`,
    `git sparse-checkout set ${paths}`,
    `git ws-new ${wsId}`,
    `# Edit and run tests inside ${paths}`,
    `git add -A && git commit -m "feat: update ${paths.split(/\s+/)[0]}"`,
    `git ws-save`,
    `git ws-diff   # or open ${window.location.origin}/diff/ws/${wsId}`,
    `git ws-sync && git ws-land`,
  ].join("\n");

  byId<HTMLElement>("cli-snippet-output").textContent = snippet;
}

async function fetchTrunkAndWorkspaces(): Promise<void> {
  try {
    const resp = await fetch("/api/refs", { headers: { accept: "application/json" } });
    if (!resp.ok) {
      throw new Error(`HTTP ${resp.status}`);
    }
    const data = (await resp.json()) as ApiRefsPayload;
    state.repo = data.repo || "gosuda/0";
    state.mainSha = data["suda/main"] || "";
    state.historySha = data["suda/history"] || "";
    if (Array.isArray(data.workspaces)) {
      state.workspaces = data.workspaces;
    }
    if (Array.isArray(data.projects) && data.projects.length > 0) {
      state.projects = data.projects;
    }
  } catch {
    // Keep client functional with unresolved ref names when running without backend token
  }

  const mainLink = byId<HTMLAnchorElement>("pill-main-link");
  const historyLink = byId<HTMLAnchorElement>("pill-history-link");
  if (state.mainSha) {
    mainLink.textContent = state.mainSha.slice(0, 10);
    mainLink.href = `https://github.com/${state.repo}/tree/${state.mainSha}`;
  } else {
    mainLink.textContent = "refs/suda/main";
  }
  if (state.historySha) {
    historyLink.textContent = state.historySha.slice(0, 10);
    historyLink.href = `https://github.com/${state.repo}/commits/${state.historySha}`;
  } else {
    historyLink.textContent = "refs/suda/history";
  }

  renderWorkspacesTable();
  renderProjectsGrid();
  updateDiffOutputs();
}

function escapeHtml(input: string): string {
  return input
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;");
}

async function copyWithFeedback(btn: HTMLButtonElement, text: string): Promise<void> {
  const original = btn.textContent;
  await navigator.clipboard.writeText(text);
  btn.textContent = "Copied";
  setTimeout(() => {
    btn.textContent = original;
  }, 1400);
}

function initAriaValidationSync(): void {
  const syncAria = (target: EventTarget | null) => {
    if (target instanceof HTMLInputElement) {
      target.setAttribute("aria-invalid", target.matches(":user-invalid") ? "true" : "false");
    }
  };
  document.addEventListener("blur", (e) => syncAria(e.target), true);
  document.addEventListener("input", (e) => {
    if (e.target instanceof HTMLInputElement && e.target.hasAttribute("aria-invalid")) {
      syncAria(e.target);
    }
  });
}

function init(): void {
  initAriaValidationSync();
  renderProjectsGrid();
  updateSubtreeJumpLinks();
  updateCliSnippet();
  updateDiffOutputs();

  byId<HTMLInputElement>("base-ref").addEventListener("input", updateDiffOutputs);
  byId<HTMLInputElement>("head-ref").addEventListener("input", updateDiffOutputs);
  byId<HTMLSelectElement>("diff-mode").addEventListener("change", updateDiffOutputs);

  document.querySelectorAll<HTMLButtonElement>("[data-set-base]").forEach((btn) => {
    btn.addEventListener("click", () => {
      byId<HTMLInputElement>("base-ref").value = btn.dataset.setBase || "suda/main";
      updateDiffOutputs();
    });
  });

  byId<HTMLButtonElement>("copy-short-btn").addEventListener("click", (e) => {
    const text = byId<HTMLElement>("short-url-output").textContent || "";
    void copyWithFeedback(e.currentTarget as HTMLButtonElement, text);
  });

  byId<HTMLButtonElement>("copy-md-btn").addEventListener("click", (e) => {
    const head = byId<HTMLInputElement>("head-ref").value.trim() || "workspace";
    const url = byId<HTMLElement>("short-url-output").textContent || "";
    void copyWithFeedback(e.currentTarget as HTMLButtonElement, `[Diff (${head})](${url})`);
  });

  byId<HTMLButtonElement>("resolve-sha-btn").addEventListener("click", () => {
    void resolveExactShasFromWorker();
  });

  byId<HTMLTableSectionElement>("workspaces-tbody").addEventListener("click", (e) => {
    const target = (e.target as HTMLElement).closest<HTMLButtonElement>("[data-load-head]");
    if (target && target.dataset.loadHead) {
      byId<HTMLInputElement>("head-ref").value = target.dataset.loadHead;
      updateDiffOutputs();
      window.scrollTo({ top: 0, behavior: "smooth" });
    }
  });

  byId<HTMLInputElement>("ws-filter").addEventListener("input", renderWorkspacesTable);
  byId<HTMLButtonElement>("refresh-refs-btn").addEventListener("click", () => {
    void fetchTrunkAndWorkspaces();
  });

  byId<HTMLInputElement>("custom-path").addEventListener("input", updateSubtreeJumpLinks);
  byId<HTMLInputElement>("cli-actor").addEventListener("input", updateCliSnippet);
  byId<HTMLInputElement>("cli-task").addEventListener("input", updateCliSnippet);
  byId<HTMLInputElement>("cli-paths").addEventListener("input", updateCliSnippet);

  byId<HTMLButtonElement>("copy-cli-btn").addEventListener("click", (e) => {
    const text = byId<HTMLElement>("cli-snippet-output").textContent || "";
    void copyWithFeedback(e.currentTarget as HTMLButtonElement, text);
  });

  byId<HTMLButtonElement>("theme-toggle").addEventListener("click", () => {
    const root = document.documentElement;
    const current = root.getAttribute("data-theme") || "light";
    root.setAttribute("data-theme", current === "dark" ? "light" : "dark");
  });

  void fetchTrunkAndWorkspaces();
}

if (typeof document !== "undefined") {
  init();
}
