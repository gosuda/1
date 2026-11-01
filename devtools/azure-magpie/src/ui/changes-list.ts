// Changes (/changes): one row per change with its patchset count, review, and last update.
import type { ChangeListItem, ChangeListPayload, PullRequestInfo } from "../api-types";
import { loadChanges } from "./data";
import { banner, byId, chip, commandRow, copyButton, emptyState, errorState, esc, person, plural, timeTag, type Tone } from "./dom";

function reviewCell(review: PullRequestInfo | null, unavailable: string | null): string {
  if (!review) {
    return unavailable
      ? `<span class="muted" title="${esc(unavailable)}">Unknown</span>`
      : `<span class="muted">No review yet</span>`;
  }
  const tone: Tone = review.merged ? "ok" : review.state === "open" ? "info" : "neutral";
  const label = review.merged ? "merged" : review.state;
  return `<a href="${esc(review.url)}" target="_blank" rel="noopener">#${review.number}<span class="visually-hidden"> ${esc(review.title)} on GitHub</span></a> ${chip(label, tone)}`;
}

function changeRow(change: ChangeListItem, unavailable: string | null): string {
  const search = [change.subject, change.id, change.author.name, change.author.login, change.review ? `#${change.review.number}` : ""]
    .filter(Boolean)
    .join(" ")
    .toLowerCase();
  return `<tr data-search="${esc(search)}">
    <td data-label="Change"><a class="row-title" href="/changes/${esc(change.id)}">${esc(change.subject)}</a></td>
    <td data-label="Change-Id"><span class="mono" title="${esc(change.id)}">${esc(change.id.slice(0, 9))}…</span> ${copyButton(change.id, "Change-Id")}</td>
    <td data-label="Author">${person(change.author.name, change.author.login)}</td>
    <td data-label="Patchsets">${plural(change.patchsetCount, "patchset")}</td>
    <td data-label="Review">${reviewCell(change.review, unavailable)}</td>
    <td data-label="Updated">${timeTag(change.updatedAt)}</td>
  </tr>`;
}

function notices(payload: ChangeListPayload): string {
  const parts: string[] = [];
  if (payload.truncated && payload.note) {
    parts.push(banner("warn", payload.note, "Partial list."));
  } else if (payload.note) {
    parts.push(banner("info", payload.note));
  }
  if (payload.reviewUnavailable) {
    parts.push(banner("info", payload.reviewUnavailable, "Reviews hidden."));
  }
  return parts.join("");
}

function listBody(payload: ChangeListPayload): string {
  if (payload.changes.length === 0) {
    return `${notices(payload)}${emptyState(
      "No changes yet",
      `<p>A change appears here once <code>git ws-pr</code> publishes its first patchset from a workspace branch. The first run also opens a review on GitHub.</p>
      <p>Run this on your workspace branch:</p>${commandRow("git ws-pr", "git ws-pr")}`,
    )}`;
  }
  const rows = payload.changes.map((change) => changeRow(change, payload.reviewUnavailable)).join("");
  return `${notices(payload)}
    <div class="toolbar">
      <label class="visually-hidden" for="changes-filter">Filter changes</label>
      <input id="changes-filter" type="search" data-filter="changes" placeholder="Filter changes" aria-describedby="changes-count" autocomplete="off" spellcheck="false">
      <p class="toolbar-count" id="changes-count" aria-live="polite">${plural(payload.changes.length, "change")}</p>
    </div>
    <section class="card card-flush" aria-label="Changes">
      <div class="table-wrap">
        <table class="data-table changes-table">
          <caption class="visually-hidden">Changes, newest first</caption>
          <thead>
            <tr>
              <th scope="col">Change</th>
              <th scope="col">Change-Id</th>
              <th scope="col">Author</th>
              <th scope="col">Patchsets</th>
              <th scope="col">Review</th>
              <th scope="col">Updated</th>
            </tr>
          </thead>
          <tbody id="changes-body">${rows}</tbody>
        </table>
      </div>
    </section>`;
}

export async function renderChangesList(): Promise<string> {
  const result = await loadChanges();
  const body = result.ok
    ? listBody(result.value)
    : errorState("Couldn't load changes.", `${result.message} Check that magpie can read gosuda/1, then try again.`);
  return `<header class="page-head">
      <div>
        <p class="eyebrow">Changes</p>
        <h1 tabindex="-1">Changes</h1>
        <p class="lead">A <strong>change</strong> is one unit of work, identified by its <code>Change-Id</code> trailer. Each time you run <code>git ws-pr</code>, a new <strong>patchset</strong> is published: PS1 is the first version, PS2 the next, and so on.</p>
      </div>
      <div class="page-actions">
        <button type="button" class="btn btn-secondary" data-action="reload">Refresh</button>
      </div>
    </header>
    ${body}`;
}

// Client-side filter over the rendered rows; hidden rows keep their place for screen readers.
export function filterChangeRows(filter: string): void {
  const needle = filter.trim().toLowerCase();
  const rows = document.querySelectorAll<HTMLTableRowElement>("#changes-body tr[data-search]");
  let visible = 0;
  rows.forEach((row) => {
    const match = !needle || (row.dataset.search ?? "").includes(needle);
    row.hidden = !match;
    if (match) {
      visible += 1;
    }
  });
  const count = byId<HTMLElement>("changes-count");
  if (count) {
    count.textContent = needle ? `${visible} of ${rows.length} shown` : plural(rows.length, "change");
  }
}
