// Overview (/): trunk state, recent changes, active workspaces, mounted projects, workspace commands.
import type { ChangeListItem, ChangeListPayload } from "../api-types";
import {
  banner,
  byId,
  chip,
  copyButton,
  emptyState,
  errorState,
  esc,
  shortSha,
  timeTag,
  type Tone,
} from "./dom";
import { currentRefs, loadChanges, loadRefs, type Loaded, type ProjectItem, type RefsSnapshot, type WorkspaceItem } from "./data";

const DEFAULT_PROJECTS: ProjectItem[] = [
  { prefix: "web/gosuda", mirror: "gosuda/website" },
  { prefix: "web/securl", mirror: "gosuda/securl" },
  { prefix: "net/grt", mirror: "gosuda/GRT" },
  { prefix: "net/ivnp", mirror: "gosuda/IVNP" },
  { prefix: "net/portalite", mirror: "gosuda/portalite" },
  { prefix: "ipc/hqq/HQQ", mirror: "gosuda/HQQ" },
  { prefix: "ipc/hqq/hqq-sqlite3", mirror: "gosuda/hqq-sqlite3" },
  { prefix: "ipc/hqq/hqq-zstd", mirror: "gosuda/hqq-zstd" },
  { prefix: "devtools/azure-magpie", mirror: "gosuda/azure-magpie" },
];

const RECENT_LIMIT = 5;

export function workspaceTone(label: string): { ns: string; tone: Tone } {
  if (label.startsWith("land/")) {
    return { ns: "land", tone: "warn" };
  }
  if (label.startsWith("review/")) {
    return { ns: "review", tone: "info" };
  }
  return { ns: "ws", tone: "accent" };
}

function statTile(label: string, value: string, note: string): string {
  return `<article class="stat">
    <p class="stat-label">${esc(label)}</p>
    <div class="stat-value">${value}</div>
    <p class="stat-note">${esc(note)}</p>
  </article>`;
}

function shaTile(label: string, sha: string | undefined, note: string): string {
  if (!sha) {
    return statTile(label, `<span class="muted">unavailable</span>`, note);
  }
  const value = `<code class="sha-lg" title="${esc(sha)}">${esc(shortSha(sha, 12))}</code> ${copyButton(sha, `${label} commit`)} <a class="link-external" href="https://github.com/${esc(currentRefs()?.repo ?? "gosuda/1")}/tree/${esc(sha)}" target="_blank" rel="noopener">View<span class="visually-hidden"> on GitHub</span></a>`;
  return statTile(label, value, note);
}

function changesTile(result: Loaded<ChangeListPayload>): string {
  if (!result.ok) {
    return statTile("Changes", `<span class="muted">unavailable</span>`, result.message);
  }
  const count = result.value.changes.length;
  const note = result.value.truncated
    ? "Partial: GitHub read limit reached. Open Changes to see more."
    : "Changes with a published patchset.";
  return statTile("Changes", `<span class="stat-number">${count}</span>`, note);
}

function workspacesTile(refs: RefsSnapshot | null): string {
  const count = refs ? refs.workspaces.length : null;
  return statTile(
    "Workspaces",
    count === null ? `<span class="muted">unavailable</span>` : `<span class="stat-number">${count}</span>`,
    "Branches under refs/ws/* in flight.",
  );
}

function recentChangesCard(result: Loaded<ChangeListPayload>): string {
  let body: string;
  if (!result.ok) {
    body = errorState("Couldn't load changes.", result.message);
  } else if (result.value.changes.length === 0) {
    body = emptyState(
      "No changes yet",
      `<p>A change appears here after <code>git ws-pr</code> publishes its first patchset. Run it on your workspace branch to start a review.</p>`,
    );
  } else {
    const items = result.value.changes.slice(0, RECENT_LIMIT).map(renderChangeLine).join("");
    body = `<ul class="row-list">${items}</ul>`;
  }
  return `<section class="card" aria-labelledby="recent-title">
    <header class="card-head">
      <div>
        <h2 id="recent-title">Recent changes</h2>
        <p class="card-lede">A <strong>change</strong> is one unit of work, identified by its <code>Change-Id</code> trailer. Each <code>git ws-pr</code> publishes a <strong>patchset</strong> (PS1, PS2, …) of that change.</p>
      </div>
    </header>
    ${body}
    <p class="card-foot"><a href="/changes">All changes</a></p>
  </section>`;
}

function renderChangeLine(change: ChangeListItem): string {
  const review = change.review ? ` · #${change.review.number} ${change.review.merged ? "merged" : change.review.state}` : "";
  return `<li class="row-item">
    <a class="row-title" href="/changes/${esc(change.id)}">${esc(change.subject)}</a>
    <p class="row-meta"><code>${esc(change.id.slice(0, 9))}</code> · PS${change.patchsetCount}${esc(review)} · ${timeTag(change.updatedAt)}</p>
  </li>`;
}

export function workspaceRowsHtml(items: WorkspaceItem[], filter: string): string {
  const needle = filter.trim().toLowerCase();
  const matching = items.filter(
    (item) =>
      !needle ||
      item.ref.toLowerCase().includes(needle) ||
      item.label.toLowerCase().includes(needle) ||
      item.sha.toLowerCase().includes(needle),
  );
  if (items.length === 0) {
    return `<tr><td colspan="4">${emptyState(
      "No active workspaces",
      `<p>Start one on your machine with <code>git ws-new &lt;actor&gt;/&lt;task&gt;</code>, then publish it with <code>git ws-save</code>.</p>`,
    )}</td></tr>`;
  }
  if (matching.length === 0) {
    return `<tr><td colspan="4" class="empty-cell">No workspace matches “${esc(filter)}”.</td></tr>`;
  }
  return matching
    .map((item) => {
      const { ns, tone } = workspaceTone(item.label);
      return `<tr>
        <td data-label="Namespace">${chip(ns, tone)}</td>
        <td data-label="Reference"><code class="ref-code">${esc(item.ref)}</code></td>
        <td data-label="Tip"><span class="sha">${esc(shortSha(item.sha))}</span> ${copyButton(item.sha, `tip ${item.label}`)}</td>
        <td data-label="Actions" class="cell-actions">
          <button type="button" class="btn btn-secondary btn-sm" data-action="use-head" data-ref="${esc(item.label)}">Use as head</button>
          <a class="btn btn-ghost btn-sm" href="/diff/${esc(item.label)}" target="_blank" rel="noopener">Open diff<span class="visually-hidden"> in a new tab</span></a>
        </td>
      </tr>`;
    })
    .join("");
}

function workspacesCard(refs: RefsSnapshot | null): string {
  const rows = refs
    ? workspaceRowsHtml(refs.workspaces, "")
    : `<tr><td colspan="4">${errorState("Couldn't load workspaces.", "Reload the page to try again.")}</td></tr>`;
  return `<section class="card" id="workspaces" aria-labelledby="workspaces-title">
    <header class="card-head">
      <div>
        <h2 id="workspaces-title">Workspaces</h2>
        <p class="card-lede">Work in progress on gosuda/1, each on its own <code>refs/ws/*</code> branch. Use one as the head of a diff link, or open its diff against the trunk.</p>
      </div>
      <div class="card-tools">
        <label class="visually-hidden" for="ws-filter">Filter workspaces</label>
        <input id="ws-filter" type="search" data-filter="workspaces" placeholder="Filter by actor or task" autocomplete="off" spellcheck="false">
        <button type="button" class="btn btn-secondary" data-action="reload">Refresh</button>
      </div>
    </header>
    <div class="table-wrap">
      <table class="data-table">
        <caption class="visually-hidden">Active workspaces and landing requests</caption>
        <thead><tr><th scope="col">Namespace</th><th scope="col">Reference</th><th scope="col">Tip</th><th scope="col">Actions</th></tr></thead>
        <tbody id="ws-body">${rows}</tbody>
      </table>
    </div>
  </section>`;
}

export function updateWorkspaceRows(filter: string): void {
  const body = byId<HTMLTableSectionElement>("ws-body");
  const refs = currentRefs();
  if (body && refs) {
    body.innerHTML = workspaceRowsHtml(refs.workspaces, filter);
  }
}

function subtreeLinks(raw: string): { tree: string; history: string; blame: string } {
  const path = raw.trim().replace(/^\/+|\/+$/g, "");
  return {
    tree: path ? `/tree/${path}` : "/tree",
    history: path ? `/history/${path}` : "/history",
    blame: path ? `/blame/${path}` : "/history",
  };
}

export function updateSubtreeLinks(raw: string): void {
  const links = subtreeLinks(raw);
  byId<HTMLAnchorElement>("subtree-tree")?.setAttribute("href", links.tree);
  byId<HTMLAnchorElement>("subtree-history")?.setAttribute("href", links.history);
  byId<HTMLAnchorElement>("subtree-blame")?.setAttribute("href", links.blame);
}

function projectsCard(refs: RefsSnapshot | null): string {
  const projects = refs && refs.projects.length > 0 ? refs.projects : DEFAULT_PROJECTS;
  const cards = projects
    .map((project) => {
      const p = esc(project.prefix);
      const m = esc(project.mirror);
      return `<li class="project">
        <div>
          <p class="project-path"><code>//${p}</code></p>
          <p class="project-mirror muted">${m}</p>
        </div>
        <div class="project-links">
          <a href="/tree/${p}" target="_blank" rel="noopener">Tree</a>
          <a href="/history/${p}" target="_blank" rel="noopener">History</a>
          <a href="https://github.com/${m}" target="_blank" rel="noopener">Mirror<span class="visually-hidden"> on GitHub</span></a>
        </div>
      </li>`;
    })
    .join("");
  return `<section class="card" id="projects" aria-labelledby="projects-title">
    <header class="card-head">
      <div>
        <h2 id="projects-title">Projects</h2>
        <p class="card-lede">Projects mounted in the monorepo and the public mirror each one exports to. Jump to any subtree of the trunk snapshot or its history.</p>
      </div>
    </header>
    <div class="field">
      <label for="subtree-path">Subtree path</label>
      <p class="hint" id="subtree-hint">Example: ipc/hqq/HQQ or web/gosuda</p>
      <div class="inline-form">
        <input id="subtree-path" type="text" data-field="subtree" placeholder="ipc/hqq/HQQ" aria-describedby="subtree-hint" autocomplete="off" spellcheck="false">
        <a id="subtree-tree" class="btn btn-secondary" href="/tree" target="_blank" rel="noopener">Tree</a>
        <a id="subtree-history" class="btn btn-secondary" href="/history" target="_blank" rel="noopener">History</a>
        <a id="subtree-blame" class="btn btn-ghost" href="/history" target="_blank" rel="noopener">Blame</a>
      </div>
    </div>
    <ul class="project-grid">${cards}</ul>
  </section>`;
}

export interface CliInputs {
  actor: string;
  task: string;
  paths: string;
}

export function cliSnippet(inputs: CliInputs): string {
  const paths = inputs.paths.trim() || "ipc/hqq/HQQ";
  const wsId = `${inputs.actor.trim() || "agent-dalgona"}/${inputs.task.trim() || "task-402"}`;
  const firstPath = paths.split(/\s+/)[0];
  return [
    `git config include.path ../.gitconfig`,
    `git suda-init`,
    `git sparse-checkout set ${paths}`,
    `git ws-new ${wsId}`,
    `# Edit and run tests inside ${paths}`,
    `git add -A && git commit -m "feat: update ${firstPath}"`,
    `git ws-save      # publish the workspace`,
    `git ws-pr        # optional: open a review with patchset PS1`,
    `git ws-sync && git ws-land`,
  ].join("\n");
}

export function updateCliSnippet(inputs: CliInputs): void {
  const text = cliSnippet(inputs);
  const pre = byId<HTMLElement>("cli-snippet");
  const copy = byId<HTMLButtonElement>("cli-copy");
  if (pre) {
    pre.textContent = text;
  }
  copy?.setAttribute("data-copy", text);
}

function cliCard(): string {
  const initial: CliInputs = { actor: "agent-dalgona", task: "task-402", paths: "ipc/hqq/HQQ web/gosuda" };
  const text = cliSnippet(initial);
  return `<section class="card" id="cli" aria-labelledby="cli-title">
    <header class="card-head">
      <div>
        <h2 id="cli-title">Workspace commands</h2>
        <p class="card-lede">Copy-ready commands for one workspace, following the reference rules in <code>AGENTS.md</code>. Fill in the fields; the commands update as you type.</p>
      </div>
    </header>
    <div class="form-grid">
      <div class="field">
        <label for="cli-actor">Actor</label>
        <p class="hint" id="cli-actor-hint">Your GitHub login or an agent name, such as agent-dalgona.</p>
        <input id="cli-actor" type="text" data-field="cli" value="agent-dalgona" aria-describedby="cli-actor-hint" autocomplete="off" spellcheck="false">
      </div>
      <div class="field">
        <label for="cli-task">Task</label>
        <p class="hint" id="cli-task-hint">A short id, such as task-402 or fix-header.</p>
        <input id="cli-task" type="text" data-field="cli" value="task-402" aria-describedby="cli-task-hint" autocomplete="off" spellcheck="false">
      </div>
      <div class="field">
        <label for="cli-paths">Sparse checkout</label>
        <p class="hint" id="cli-paths-hint">Space-separated google3 paths.</p>
        <input id="cli-paths" type="text" data-field="cli" value="ipc/hqq/HQQ web/gosuda" aria-describedby="cli-paths-hint" autocomplete="off" spellcheck="false">
      </div>
    </div>
    <div class="snippet">
      <pre id="cli-snippet" class="code-block" tabindex="0" aria-label="Workspace commands">${esc(text)}</pre>
      <div class="snippet-actions">${copyButton(text, "workspace commands", "btn btn-secondary")}</div>
    </div>
  </section>`;
}

export async function renderOverview(): Promise<string> {
  const [refsResult, changesResult] = await Promise.all([loadRefs(), loadChanges()]);
  const refs = refsResult.ok ? refsResult.value : null;
  const refsNotice = refsResult.ok
    ? ""
    : banner("err", `Trunk and workspace state could not be loaded: ${refsResult.message}`, "Couldn't reach gosuda/1.");
  const trunkNote = "Parentless snapshot of the monorepo. Workspaces start from it.";

  return `
    <header class="page-head">
      <div>
        <p class="eyebrow">Overview</p>
        <h1 tabindex="-1">Overview</h1>
        <p class="lead">The state of gosuda/1 at a glance: the trunk snapshot, recent changes, and the workspaces in flight.</p>
      </div>
    </header>
    ${refsNotice}
    <section class="stat-grid" aria-label="At a glance">
      ${shaTile("suda/main", refs?.mainSha, trunkNote)}
      ${shaTile("suda/history", refs?.historySha, "Linear, lander-signed history for blame and log.")}
      ${changesTile(changesResult)}
      ${workspacesTile(refs)}
    </section>
    ${recentChangesCard(changesResult)}
    ${workspacesCard(refs)}
    ${projectsCard(refs)}
    ${cliCard()}
  `;
}
