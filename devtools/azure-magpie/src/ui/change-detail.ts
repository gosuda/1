// Change detail (/changes/<id> and /changes/<N>): patchsets, approvals, and the interdiff compare view.
import type { ApprovalInfo, ChangeDetailPayload, PatchsetInfo } from "../api-types";
import {
  collapseUnchanged,
  compareFiles,
  patchLines,
  patchOnly,
  type DiffLine,
  type FileInterdiff,
  type PatchFile,
} from "../interdiff";
import { loadChange } from "./data";
import {
  banner,
  byId,
  chip,
  commandRow,
  copyButton,
  emptyState,
  errorState,
  esc,
  exactTime,
  person,
  plural,
  shortSha,
  timeTag,
  type Tone,
} from "./dom";

const MAX_ROWS_PER_BLOCK = 2000;

// The change on screen; the compare selectors re-render against it without a new request.
let current: ChangeDetailPayload | null = null;
// Diff bodies are built when their block is opened, so long patches stay cheap to show.
const lazyBodies = new Map<string, () => string>();

export async function renderChangeDetail(param: string): Promise<string> {
  const result = await loadChange(param);
  if (!result.ok) {
    current = null;
    if (result.status === 404) {
      return emptyState(
        "No change found",
        `<p>Nothing matches <code>${esc(param)}</code> on gosuda/1. A change appears once <code>git ws-pr</code> has published a patchset for it.</p>
        <p class="muted">Embargoed work is never listed here.</p>
        <p><a href="/changes">Back to all changes</a></p>`,
      );
    }
    return `${errorState("Couldn't load this change.", result.message)}<p><a href="/changes">Back to all changes</a></p>`;
  }
  const data = result.value;
  current = data;
  const latest = data.patchsets[data.patchsets.length - 1];
  const defaultTo = latest.number;
  const defaultFrom = defaultTo > 1 ? defaultTo - 1 : "base";

  return `${crumbs(data)}
    ${header(data)}
    ${data.reviewUnavailable ? banner("info", data.reviewUnavailable, "Review status hidden.") : ""}
    ${landing(data)}
    ${patchsetsCard(data)}
    ${compareCard(data, String(defaultFrom), String(defaultTo))}
    ${cliCard(data, String(defaultFrom), String(defaultTo))}`;
}

function crumbs(data: ChangeDetailPayload): string {
  return `<nav class="crumbs" aria-label="Breadcrumb"><ol>
    <li><a href="/changes">Changes</a></li>
    <li aria-current="page"><code>${esc(data.id.slice(0, 9))}…</code></li>
  </ol></nav>`;
}

function header(data: ChangeDetailPayload): string {
  const first = data.patchsets[0];
  const review = data.review;
  const reviewLine = review
    ? `<a href="${esc(review.url)}" target="_blank" rel="noopener">Review #${review.number}<span class="visually-hidden"> on GitHub</span></a> ${chip(
        review.merged ? "merged" : review.state,
        review.merged ? "ok" : review.state === "open" ? "info" : "neutral",
      )}`
    : `<span class="muted">No review yet. Run <code>git ws-pr</code> to open one.</span>`;
  return `<header class="page-head">
    <div>
      <p class="eyebrow">Change · ${plural(data.patchsets.length, "patchset")}</p>
      <h1 tabindex="-1">${esc(data.subject)}</h1>
      <dl class="meta">
        <div><dt>Change-Id</dt><dd class="id-line"><code class="mono-wrap">${esc(data.id)}</code> ${copyButton(data.id, "Change-Id")}</dd></div>
        <div><dt>Review</dt><dd>${reviewLine}</dd></div>
        <div><dt>Author</dt><dd>${person(first.author.name, null)}</dd></div>
        <div><dt>Updated</dt><dd>${timeTag(data.updatedAt)}</dd></div>
      </dl>
    </div>
  </header>`;
}

function landing(data: ChangeDetailPayload): string {
  const reviewHint = data.review
    ? `Or merge review #${data.review.number} on GitHub. The lander reads the merge from GitHub and judges the approvals on the merged head.`
    : `Or merge the review on GitHub once it exists.`;
  return `<section class="card callout" aria-labelledby="landing-title">
    <h2 id="landing-title">Landing</h2>
    <p>An approval counts for the commit it was given on, and for a later patchset that is a clean rebase of that commit (the same patch on a newer base). Any other edit needs a new approval. The lander decides.</p>
    <p>From a workspace that has this change checked out, land the latest patchset with:</p>
    ${commandRow("git ws-sync && git ws-land", "landing commands")}
    <p class="muted">${esc(reviewHint)}</p>
  </section>`;
}

function approvalsOn(data: ChangeDetailPayload, number: number): ApprovalInfo[] {
  return data.approvals.filter((approval) => approval.patchset === number);
}

function changesRequestedOn(data: ChangeDetailPayload, patchset: PatchsetInfo): string[] {
  const approvers = new Set(data.approvals.map((approval) => approval.login));
  return data.reviews
    .filter((review) => review.state === "CHANGES_REQUESTED" && review.commitId === patchset.sha && !approvers.has(review.login))
    .map((review) => review.login);
}

function patchsetRow(data: ChangeDetailPayload, patchset: PatchsetInfo): string {
  const repo = `https://github.com/${data.repo}`;
  const approvals = approvalsOn(data, patchset.number);
  const carries = patchset.carriesTo.map((n) => `PS${n}`);
  const badges: string[] = approvals.map(
    (approval) => `<span class="badge badge-ok" title="Approved ${esc(exactTime(approval.submittedAt))}">✓ ${esc(approval.login)}</span>`,
  );
  if (approvals.length > 0 && carries.length > 0) {
    badges.push(
      `<span class="badge badge-carry" title="Estimate: this patchset's patch is identical to ${esc(carries.join(", "))}. The lander decides.">carries to ${esc(carries.join(", "))}</span>`,
    );
  }
  for (const login of changesRequestedOn(data, patchset)) {
    badges.push(`<span class="badge badge-warn">${esc(login)} requested changes</span>`);
  }
  const files = patchset.files;
  const additions = files ? files.reduce((sum, file) => sum + (file.additions ?? 0), 0) : 0;
  const deletions = files ? files.reduce((sum, file) => sum + (file.deletions ?? 0), 0) : 0;
  const errorNote = patchset.error ? `<p class="row-error">${esc(patchset.error)}</p>` : "";
  const compareLink =
    patchset.rootSha && !patchset.error
      ? `<a href="${repo}/compare/${esc(patchset.rootSha)}...${esc(patchset.sha)}" target="_blank" rel="noopener">Patch on GitHub<span class="visually-hidden"> for PS${patchset.number}</span></a>`
      : "";
  return `<tr>
    <td data-label="Patchset"><strong>PS${patchset.number}</strong></td>
    <td data-label="Commit">
      <div class="cell-stack">
        <span><code title="${esc(patchset.sha)}">${esc(shortSha(patchset.sha))}</code> ${copyButton(patchset.sha, `PS${patchset.number} commit`)}</span>
        <span class="row-sub">${esc(patchset.subject)}</span>
        ${errorNote}
      </div>
    </td>
    <td data-label="Base">${patchset.rootSha ? `<code title="${esc(patchset.rootSha)}">${esc(shortSha(patchset.rootSha))}</code>` : `<span class="muted">unknown</span>`}</td>
    <td data-label="Published">${timeTag(patchset.date)}</td>
    <td data-label="Files">${files ? `${files.length}${patchset.filesTruncated ? "+" : ""}` : `<span class="muted">—</span>`} <span class="cell-links">${compareLink}</span></td>
    <td data-label="+ / −" class="nums"><span class="add-count">+${additions}</span> <span class="del-count">−${deletions}</span></td>
    <td data-label="Approvals"><div class="badges">${badges.join("") || `<span class="muted">none yet</span>`}</div></td>
  </tr>`;
}

function unboundApprovals(data: ChangeDetailPayload): string {
  const loose = data.approvals.filter((approval) => approval.patchset === null);
  if (loose.length === 0) {
    return "";
  }
  const names = loose.map((approval) => `${approval.login} (${shortSha(approval.commitId)})`).join(", ");
  return `<p class="card-foot">Approvals on commits that are not published patchsets: ${esc(names)}.</p>`;
}

function patchsetsCard(data: ChangeDetailPayload): string {
  const rows = data.patchsets.map((patchset) => patchsetRow(data, patchset)).join("");
  return `<section class="card card-flush" aria-labelledby="patchsets-title">
    <header class="card-head pad">
      <div>
        <h2 id="patchsets-title">Patchsets</h2>
        <p class="card-lede">Each patchset is the commit range <code>root..tip</code> on the trunk snapshot it was based on, oldest first. Each approval is bound to the commit the reviewer approved.</p>
        <p class="card-lede"><strong>Approval carry-over</strong> is an estimate: a later patchset that is a clean rebase of an approved one (the same patch on a newer base) would keep that approval. Any other edit needs a new approval. The lander decides.</p>
      </div>
    </header>
    <div class="table-wrap">
      <table class="data-table patchset-table">
        <caption class="visually-hidden">Patchsets of ${esc(data.id)}, oldest first</caption>
        <thead>
          <tr>
            <th scope="col">PS</th>
            <th scope="col">Commit</th>
            <th scope="col">Base</th>
            <th scope="col">Published</th>
            <th scope="col">Files</th>
            <th scope="col">+ / −</th>
            <th scope="col">Approvals</th>
          </tr>
        </thead>
        <tbody>${rows}</tbody>
      </table>
    </div>
    ${unboundApprovals(data)}
  </section>`;
}

function optionsFor(data: ChangeDetailPayload, selected: string, includeBase: boolean): string {
  const base = includeBase
    ? `<option value="base"${selected === "base" ? " selected" : ""}>Base (trunk snapshot)</option>`
    : "";
  const patchsets = data.patchsets
    .map(
      (patchset) =>
        `<option value="${patchset.number}"${String(patchset.number) === selected ? " selected" : ""}>PS${patchset.number} · ${esc(shortSha(patchset.sha, 8))}</option>`,
    )
    .join("");
  return base + patchsets;
}

function compareCard(data: ChangeDetailPayload, from: string, to: string): string {
  return `<section class="card" id="compare" aria-labelledby="compare-title">
    <header class="card-head">
      <div>
        <h2 id="compare-title">Compare patchsets</h2>
        <p class="card-lede">See what changed between two patchsets. Choose <strong>Base</strong> to see one patchset's patch against the trunk snapshot it was based on.</p>
      </div>
    </header>
    <div class="compare-controls">
      <div class="field">
        <label for="cmp-from">From</label>
        <select id="cmp-from" data-compare="from">${optionsFor(data, from, true)}</select>
      </div>
      <div class="field">
        <label for="cmp-to">To</label>
        <select id="cmp-to" data-compare="to">${optionsFor(data, to, false)}</select>
      </div>
    </div>
    <div id="cmp-result" class="compare-result" aria-live="polite">${compareResult(data, from, to)}</div>
  </section>`;
}

function cliCard(data: ChangeDetailPayload, from: string, to: string): string {
  return `<section class="card" aria-labelledby="cli-title">
    <header class="card-head">
      <div>
        <h2 id="cli-title">From the command line</h2>
        <p class="card-lede">Commands for the patchsets selected above.</p>
      </div>
    </header>
    <div id="cmp-cli" class="stack">${cliCommands(data, from, to)}</div>
  </section>`;
}

// On the change's own branch, ws-psdiff takes patchset numbers (it compares two patchsets; a single
// patchset against its base has no branch shortcut). From any other clone, the patchsets are fetched
// under refs/remotes/ws/ws/cl/<id>/ and compared with range-diff over their roots.
function cliCommands(data: ChangeDetailPayload, from: string, to: string): string {
  const target = findPatchset(data, to);
  if (!target) {
    return "";
  }
  const origin = findPatchset(data, from);
  const local = origin ? `git ws-psdiff ${origin.number} ${target.number}` : null;
  const fetch = `git suda-ws fetch ws '+refs/ws/cl/${data.id}/*:refs/remotes/ws/ws/cl/${data.id}/*'`;
  let compare: string | null = null;
  if (origin && origin.rootSha && target.rootSha) {
    compare = `git range-diff ${origin.rootSha}..${origin.sha} ${target.rootSha}..${target.sha}`;
  } else if (!origin && target.rootSha) {
    compare = `git diff ${target.rootSha}..${target.sha}`;
  }
  const branch = local
    ? `<p class="cli-label">On this change's branch</p>
    ${commandRow(local, "ws-psdiff command")}`
    : "";
  const hint = local
    ? "Use the first when this change's branch is checked out; from any other clone, fetch the patchsets once and compare them."
    : "Fetch the patchsets once, then show this patchset's patch against the trunk snapshot it was based on.";
  return `${branch}
    <p class="cli-label">From any clone</p>
    ${commandRow(fetch, "fetch command")}
    ${compare ? commandRow(compare, compare.startsWith("git diff") ? "diff command" : "range-diff command") : ""}
    <p class="hint">${hint}</p>`;
}

function findPatchset(data: ChangeDetailPayload, value: string): PatchsetInfo | null {
  if (value === "base") {
    return null;
  }
  const number = Number(value);
  return data.patchsets.find((patchset) => patchset.number === number) ?? null;
}

function compareResult(data: ChangeDetailPayload, fromValue: string, toValue: string): string {
  lazyBodies.clear();
  const to = findPatchset(data, toValue);
  if (!to) {
    return banner("info", "Choose a patchset to compare.");
  }
  if (fromValue === "base") {
    return baseView(data, to);
  }
  const from = findPatchset(data, fromValue);
  if (!from) {
    return banner("info", "Choose a patchset to compare.");
  }
  if (from.number === to.number) {
    return banner("info", "Pick two different patchsets to see what changed between them.");
  }
  for (const side of [from, to]) {
    if (!side.files) {
      return banner("warn", `PS${side.number} can't be compared: ${side.error ?? "its patch is not available"}.`);
    }
  }
  return pairView(data, from, to);
}

interface Entry {
  name: string;
  label: string;
  tone: Tone;
  additions: number;
  deletions: number;
  note: string | null;
  open: boolean;
  body: () => string;
}

function statusTone(status: string): Tone {
  if (status === "added") {
    return "ok";
  }
  if (status === "removed") {
    return "err";
  }
  if (status === "renamed") {
    return "info";
  }
  return "neutral";
}

function baseView(data: ChangeDetailPayload, patchset: PatchsetInfo): string {
  const shown = patchOnly(patchset.files ?? []);
  const files = shown.files;
  const entries: Entry[] = files.map((file) => ({
    name: file.previous_filename ? `${file.previous_filename} → ${file.filename}` : file.filename,
    label: file.status,
    tone: statusTone(file.status),
    additions: file.additions ?? 0,
    deletions: file.deletions ?? 0,
    note: file.patch === null ? "Patch text is not available (binary or too large). Open it on GitHub." : null,
    open: files.length <= 3,
    body: () => patchBody(file),
  }));
  const baseLabel = patchset.rootSha ? `the trunk snapshot ${shortSha(patchset.rootSha)}` : "the trunk snapshot";
  const links = [
    patchset.rootSha
      ? `<a href="https://github.com/${esc(data.repo)}/compare/${esc(patchset.rootSha)}...${esc(patchset.sha)}" target="_blank" rel="noopener">Patch on GitHub (root…tip)</a>`
      : "",
  ];
  return `${banner(
    "info",
    `PS${patchset.number} against ${baseLabel}: ${plural(files.length, "file")}, <span class="add-count">+${shown.additions}</span> <span class="del-count">−${shown.deletions}</span>.`,
  )}
  ${linkRow(links)}
  ${renderEntries(entries)}`;
}

function pairView(data: ChangeDetailPayload, from: PatchsetInfo, to: PatchsetInfo): string {
  const result = compareFiles(from.files ?? [], to.files ?? []);
  const { counts } = result;
  const fromApprovals = approvalsOn(data, from.number);
  const links = [
    `<a href="https://github.com/${esc(data.repo)}/compare/${esc(from.sha)}..${esc(to.sha)}" target="_blank" rel="noopener">Compare PS${from.number} → PS${to.number} on GitHub (two-dot)</a>`,
  ];

  let summary: string;
  if (result.patchIdentical) {
    const carry = fromApprovals.length > 0
      ? `Patch identical: approvals on PS${from.number} carry over to PS${to.number}.`
      : `Patch identical: PS${from.number} and PS${to.number} make the same change.`;
    summary = banner("ok", `${carry} <span class="muted">An estimate from comparing patch text; the lander decides.</span>`);
  } else {
    const changedFiles = result.files.filter((file) => file.classification !== "identical").length;
    const parts = [
      `${counts.changed} changed`,
      `${counts.added} added`,
      `${counts.dropped} dropped`,
      `${counts.identical} identical`,
    ];
    if (counts.unknown > 0) {
      parts.push(`${counts.unknown} without patch text`);
    }
    summary = banner(
      "info",
      `${plural(changedFiles, "file")} changed between PS${from.number} and PS${to.number}: ${parts.join(", ")}.`,
    );
  }

  const entries = result.files.map((file) => interdiffEntry(file));
  const legend = result.files.some((file) => file.lines !== null)
    ? `<p class="diff-legend">Legend: the outer marker (+ or −) shows what changed between the two patchsets; the sign inside a line is part of the patch itself.</p>`
    : "";
  return `${summary}${linkRow(links)}${legend}${renderEntries(entries)}`;
}

function interdiffEntry(file: FileInterdiff): Entry {
  const side = file.newer ?? file.older;
  const additions = side?.additions ?? 0;
  const deletions = side?.deletions ?? 0;
  const base = { name: file.filename, additions, deletions };
  switch (file.classification) {
    case "identical":
      return {
        ...base,
        label: "identical",
        tone: "ok",
        note: null,
        open: false,
        body: () => (file.newer?.patch ? patchBody(file.newer) : ""),
      };
    case "changed":
      return {
        ...base,
        label: "changed",
        tone: "warn",
        note: file.note,
        open: true,
        body: () => (file.lines ? diffOfDiffsBody(file.lines) : emptyBody(file.note ?? "No line-level diff.")),
      };
    case "added":
      return {
        ...base,
        label: "added",
        tone: "ok",
        note: "Only in the newer patchset.",
        open: false,
        body: () => (file.newer?.patch ? patchBody(file.newer) : emptyBody("No patch text.")),
      };
    case "dropped":
      return {
        ...base,
        label: "dropped",
        tone: "err",
        note: "Only in the older patchset.",
        open: false,
        body: () => (file.older?.patch ? patchBody(file.older) : emptyBody("No patch text.")),
      };
    case "unknown":
      return {
        ...base,
        label: "no patch text",
        tone: "neutral",
        note: file.note,
        open: false,
        body: () => emptyBody(file.note ?? "Patch text is not available."),
      };
  }
}

function linkRow(links: string[]): string {
  const present = links.filter(Boolean);
  return present.length > 0 ? `<p class="compare-links">${present.join(" · ")}</p>` : "";
}

function renderEntries(entries: Entry[]): string {
  if (entries.length === 0) {
    return banner("info", "No files to show.");
  }
  const index = entries
    .map(
      (entry, i) =>
        `<li><a href="#diff-${i}">${chip(entry.label, entry.tone)} <span class="mono-wrap">${esc(entry.name)}</span></a></li>`,
    )
    .join("");
  const blocks = entries
    .map((entry, i) => {
      const id = `diff-${i}`;
      const note = entry.note ? `<p class="diff-note muted">${esc(entry.note)}</p>` : "";
      if (entry.open) {
        // Built now: it is shown on load.
        return diffBlock(id, entry, entry.body(), note);
      }
      lazyBodies.set(id, entry.body);
      return diffBlock(id, entry, "", note);
    })
    .join("");
  return `<div class="compare-tools">
      <ul class="file-index" aria-label="Changed files">${index}</ul>
      <div class="compare-buttons">
        <button type="button" class="btn btn-secondary btn-sm" data-action="expand-diffs">Expand all</button>
        <button type="button" class="btn btn-secondary btn-sm" data-action="collapse-diffs">Collapse all</button>
      </div>
    </div>
    <div class="diff-blocks">${blocks}</div>`;
}

function diffBlock(id: string, entry: Entry, body: string, note: string): string {
  return `<details class="diff-file" id="${id}" data-diff-id="${id}"${entry.open ? " open" : ""}>
    <summary class="diff-summary">
      ${chip(entry.label, entry.tone)}
      <code class="diff-name">${esc(entry.name)}</code>
      <span class="diff-counts"><span class="add-count">+${entry.additions}</span> <span class="del-count">−${entry.deletions}</span></span>
    </summary>
    ${note}
    <div class="diff-scroll" data-diff-body>${body}</div>
  </details>`;
}

function emptyBody(message: string): string {
  return `<p class="diff-empty muted">${esc(message)}</p>`;
}

const ROW_CLASS: Record<DiffLine["op"], string> = { same: "", add: "row-add", del: "row-del" };
const ROW_MARK: Record<DiffLine["op"], string> = {
  same: `<span class="visually-hidden">unchanged</span> `,
  add: `<span class="visually-hidden">added: </span>+`,
  del: `<span class="visually-hidden">removed: </span>−`,
};

function diffOfDiffsBody(lines: DiffLine[]): string {
  const rows = collapseUnchanged(lines, 3)
    .map((block) => {
      if (block.kind === "gap") {
        return `<tr class="row-gap"><td colspan="4">${block.hidden} unchanged ${block.hidden === 1 ? "line" : "lines"}</td></tr>`;
      }
      return block.lines
        .map(
          (line) => `<tr class="${ROW_CLASS[line.op]}">
            <td class="gutter">${line.olderLine ?? ""}</td>
            <td class="gutter">${line.newerLine ?? ""}</td>
            <td class="marker">${ROW_MARK[line.op]}</td>
            <td class="code">${esc(line.text)}</td>
          </tr>`,
        )
        .join("");
    })
    .join("");
  return `<table class="diff-table"><caption class="visually-hidden">Difference between the two patches</caption><tbody>${rows}</tbody></table>`;
}

function patchBody(file: PatchFile): string {
  if (file.patch === null) {
    return emptyBody("Patch text is not available (binary or too large). Open it on GitHub.");
  }
  const all = patchLines(file.patch);
  const shown = all.slice(0, MAX_ROWS_PER_BLOCK);
  const kindClass: Record<string, string> = { hunk: "row-hunk", add: "row-add", del: "row-del", meta: "row-meta", context: "" };
  const rows = shown
    .map(
      (line) => `<tr class="${kindClass[line.kind]}">
        <td class="gutter"></td>
        <td class="gutter">${line.line}</td>
        <td class="marker"></td>
        <td class="code">${esc(line.text)}</td>
      </tr>`,
    )
    .join("");
  const truncated =
    all.length > shown.length
      ? `<tr class="row-gap"><td colspan="4">Showing the first ${shown.length} of ${all.length} lines. Open the patch on GitHub for the rest.</td></tr>`
      : "";
  return `<table class="diff-table"><caption class="visually-hidden">Patch of ${esc(file.filename)}</caption><tbody>${rows}${truncated}</tbody></table>`;
}

// Recomputes the compare result and CLI commands for the selected pair. Called by the selects.
export function updateCompare(fromValue: string, toValue: string): void {
  if (!current) {
    return;
  }
  const result = byId<HTMLElement>("cmp-result");
  if (result) {
    result.innerHTML = compareResult(current, fromValue, toValue);
  }
  const cli = byId<HTMLElement>("cmp-cli");
  if (cli) {
    cli.innerHTML = cliCommands(current, fromValue, toValue);
  }
}

// Builds the body of a lazily rendered diff block the first time it is opened.
export function fillDiffBody(details: HTMLDetailsElement): void {
  const body = details.querySelector<HTMLElement>("[data-diff-body]");
  const id = details.dataset.diffId ?? "";
  const build = lazyBodies.get(id);
  if (!body || !build || body.childElementCount > 0) {
    return;
  }
  body.innerHTML = build();
  lazyBodies.delete(id);
}

export function setAllDiffBlocks(open: boolean): void {
  document.querySelectorAll<HTMLDetailsElement>("#cmp-result details.diff-file").forEach((details) => {
    details.open = open;
  });
}
