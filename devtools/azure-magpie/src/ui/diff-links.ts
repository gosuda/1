// Diff links (/diff): builds two-dot comparison links on gosuda/1 from trunk, workspace, and SHA references.
import { byId, esc, shortSha } from "./dom";
import { currentRefs, loadRefs, takeHandoffHead, type RefsSnapshot } from "./data";
import { workspaceTone } from "./overview";

interface DiffInputs {
  base: string;
  head: string;
  op: ".." | "...";
}

function readInputs(): DiffInputs {
  const base = byId<HTMLInputElement>("base-ref")?.value.trim() || "suda/main";
  const head = byId<HTMLInputElement>("head-ref")?.value.trim() || "";
  const op = byId<HTMLSelectElement>("diff-mode")?.value === "..." ? "..." : "..";
  return { base, head, op };
}

// Known names resolve from the snapshot already on the page; anything else is sent as typed.
function resolveKnown(refSpec: string, refs: RefsSnapshot | null): string {
  const clean = refSpec.trim().replace(/^\/+|\/+$/g, "");
  if (refs && (clean === "suda/main" || clean === "refs/suda/main" || clean === "main") && refs.mainSha) {
    return refs.mainSha;
  }
  if (refs && (clean === "suda/history" || clean === "refs/suda/history" || clean === "history") && refs.historySha) {
    return refs.historySha;
  }
  const found = refs?.workspaces.find((w) => w.label === clean || w.ref === clean || w.ref === `refs/${clean}`);
  return found ? found.sha : clean;
}

export function shortPathFor(inputs: DiffInputs): string {
  return inputs.base === "suda/main" && inputs.op === ".." ? `/diff/${inputs.head}` : `/diff/${inputs.base}${inputs.op}${inputs.head}`;
}

function setOutputs(inputs: DiffInputs): void {
  const refs = currentRefs();
  const repo = refs?.repo ?? "gosuda/1";
  const ready = inputs.head.length > 0;
  const shortUrl = ready ? `${window.location.origin}${shortPathFor(inputs)}` : "";
  const compareUrl = ready
    ? `https://github.com/${repo}/compare/${resolveKnown(inputs.base, refs)}${inputs.op}${resolveKnown(inputs.head, refs)}`
    : "";
  const markdown = ready ? `[Diff (${inputs.head})](${shortUrl})` : "";

  const shortOut = byId<HTMLElement>("short-url-output");
  const githubOut = byId<HTMLElement>("github-url-output");
  const open = byId<HTMLAnchorElement>("open-github-btn");
  const copyShort = byId<HTMLButtonElement>("copy-short-btn");
  const copyMd = byId<HTMLButtonElement>("copy-md-btn");
  const resolve = byId<HTMLButtonElement>("resolve-sha-btn");

  if (shortOut) {
    shortOut.textContent = ready ? shortUrl : "Enter a head reference to build a link.";
  }
  if (githubOut) {
    githubOut.textContent = ready ? compareUrl : "The GitHub compare URL appears here.";
  }
  if (open) {
    open.href = ready ? shortPathFor(inputs) : "/diff";
    open.setAttribute("aria-disabled", ready ? "false" : "true");
  }
  for (const [button, text] of [
    [copyShort, shortUrl],
    [copyMd, markdown],
  ] as const) {
    if (button) {
      button.disabled = !ready;
      button.setAttribute("data-copy", text);
    }
  }
  if (resolve) {
    resolve.disabled = !ready;
  }
}

// Resolves names to exact SHAs through the worker, so the compare link stops depending on branch moves.
export async function resolveExactShas(): Promise<string> {
  const inputs = readInputs();
  if (!inputs.head) {
    return "Enter a head reference first.";
  }
  const resp = await fetch(`/diff/${inputs.base}${inputs.op}${inputs.head}?format=json`, {
    headers: { accept: "application/json" },
    cache: "no-store",
  });
  if (!resp.ok) {
    throw new Error(`HTTP ${resp.status}`);
  }
  const data = (await resp.json()) as { compareUrl?: string };
  if (!data.compareUrl) {
    throw new Error("no compare URL in the response");
  }
  const githubOut = byId<HTMLElement>("github-url-output");
  const open = byId<HTMLAnchorElement>("open-github-btn");
  if (githubOut) {
    githubOut.textContent = data.compareUrl;
  }
  if (open) {
    open.href = data.compareUrl;
  }
  return "Resolved to exact commits.";
}

export function updateDiffOutputs(): void {
  setOutputs(readInputs());
}

export function setHead(ref: string): void {
  const head = byId<HTMLInputElement>("head-ref");
  if (head) {
    head.value = ref;
  }
  updateDiffOutputs();
}

export function setBase(ref: string): void {
  const base = byId<HTMLInputElement>("base-ref");
  if (base) {
    base.value = ref;
  }
  updateDiffOutputs();
}

function pickList(refs: RefsSnapshot | null): string {
  if (!refs) {
    return `<p class="muted">Workspaces could not be loaded. Type a reference above instead.</p>`;
  }
  if (refs.workspaces.length === 0) {
    return `<p class="muted">No active workspaces right now. Type a reference above, such as a commit sha.</p>`;
  }
  const rows = refs.workspaces
    .map((item) => {
      const { ns } = workspaceTone(item.label);
      return `<li class="pick-row">
        <span class="pick-ns">${esc(ns)}</span>
        <code class="pick-ref">${esc(item.label)}</code>
        <span class="sha">${esc(shortSha(item.sha))}</span>
        <span class="pick-actions">
          <button type="button" class="btn btn-secondary btn-sm" data-action="pick-head" data-ref="${esc(item.label)}">Use as head</button>
          <a class="btn btn-ghost btn-sm" href="/diff/${esc(item.label)}" target="_blank" rel="noopener">Open diff<span class="visually-hidden"> in a new tab</span></a>
        </span>
      </li>`;
    })
    .join("");
  return `<ul class="pick-list">${rows}</ul>`;
}

export async function renderDiffLinks(): Promise<string> {
  const refsResult = await loadRefs();
  const refs = refsResult.ok ? refsResult.value : null;
  const handoff = takeHandoffHead();
  const head = handoff ?? "";
  const inputs: DiffInputs = { base: "suda/main", head, op: ".." };
  const short = head ? `${window.location.origin}${shortPathFor(inputs)}` : "";
  const compare = head ? `https://github.com/${refs?.repo ?? "gosuda/1"}/compare/${resolveKnown("suda/main", refs)}..${resolveKnown(head, refs)}` : "";
  const markdown = head ? `[Diff (${head})](${short})` : "";

  return `<header class="page-head">
    <div>
      <p class="eyebrow">Diff links</p>
      <h1 tabindex="-1">Diff links</h1>
      <p class="lead">Build a two-dot comparison on gosuda/1. Short links redirect through magpie to GitHub, so they work without a GitHub login. Names resolve when the link is followed; use <strong>Resolve exact SHAs</strong> to pin them.</p>
    </div>
  </header>
  ${refsResult.ok ? "" : `<div class="banner banner-warn" role="status"><strong class="banner-title">Trunk state is unavailable.</strong> Names can still be typed; the links resolve when opened.</div>`}
  <section class="card" aria-labelledby="generator-title">
    <header class="card-head">
      <div>
        <h2 id="generator-title">Link generator</h2>
        <p class="card-lede">Base is what you compare from; head is what you compare to. <code>suda/main</code> is the trunk snapshot.</p>
      </div>
      <div class="preset-row" role="group" aria-label="Base presets">
        <button type="button" class="chip-button" data-action="set-base" data-ref="suda/main">Base: suda/main</button>
        <button type="button" class="chip-button" data-action="set-base" data-ref="suda/history">Base: suda/history</button>
      </div>
    </header>
    <form id="diff-form" class="form-grid form-grid-generator" novalidate>
      <div class="field">
        <label for="base-ref">Base</label>
        <p class="hint" id="base-hint">suda/main, suda/history, ws/actor/task, or a commit sha</p>
        <input id="base-ref" name="base" type="text" value="suda/main" aria-describedby="base-hint" autocomplete="off" spellcheck="false">
      </div>
      <div class="field">
        <label for="head-ref">Head</label>
        <p class="hint" id="head-hint">A workspace such as ws/actor/task, a land/ or review/ branch, or a sha</p>
        <input id="head-ref" name="head" type="text" value="${esc(head)}" placeholder="ws/actor/task-id" aria-describedby="head-hint" autocomplete="off" spellcheck="false">
      </div>
      <div class="field">
        <label for="diff-mode">Comparison</label>
        <p class="hint" id="mode-hint">Two-dot is right for parentless trunk snapshots.</p>
        <select id="diff-mode" name="mode" aria-describedby="mode-hint">
          <option value="..">Two-dot (..): direct tree diff</option>
          <option value="...">Three-dot (...): from the merge base</option>
        </select>
      </div>
    </form>
    <div class="output-stack" aria-live="polite">
      <div class="output-row">
        <div class="output-meta">
          <span class="output-tag">Short link</span>
          <code id="short-url-output" class="output-code">${head ? esc(short) : "Enter a head reference to build a link."}</code>
        </div>
        <div class="output-actions">
          <button type="button" id="copy-short-btn" class="btn btn-secondary" data-copy="${esc(short)}"${head ? "" : " disabled"}>Copy short link</button>
          <button type="button" id="copy-md-btn" class="btn btn-ghost" data-copy="${esc(markdown)}"${head ? "" : " disabled"}>Copy Markdown</button>
        </div>
      </div>
      <div class="output-row">
        <div class="output-meta">
          <span class="output-tag">GitHub compare</span>
          <code id="github-url-output" class="output-code">${head ? esc(compare) : "The GitHub compare URL appears here."}</code>
        </div>
        <div class="output-actions">
          <button type="button" id="resolve-sha-btn" class="btn btn-secondary" data-action="resolve"${head ? "" : " disabled"}>Resolve exact SHAs</button>
          <a id="open-github-btn" class="btn btn-primary" href="${head ? esc(shortPathFor(inputs)) : "/diff"}" target="_blank" rel="noopener">Open diff on GitHub</a>
        </div>
      </div>
      <p id="diff-status" class="output-status" role="status"></p>
    </div>
  </section>
  <section class="card" aria-labelledby="pick-title">
    <header class="card-head">
      <div>
        <h2 id="pick-title">Pick a head</h2>
        <p class="card-lede">Active workspaces on gosuda/1. Use one as the head, or open its diff against the trunk.</p>
      </div>
    </header>
    ${pickList(refs)}
  </section>`;
}
