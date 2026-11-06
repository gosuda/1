// DOM and formatting helpers shared by the UI pages. Pages build HTML strings; every dynamic value
// goes through esc(), and interaction is handled by delegated listeners in app.ts.

export type Tone = "neutral" | "accent" | "ok" | "warn" | "err" | "info";

export function esc(value: string | number | null | undefined): string {
  return String(value ?? "")
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;")
    .replace(/'/g, "&#39;");
}

export function byId<T extends HTMLElement>(id: string): T | null {
  return document.getElementById(id) as T | null;
}

export function shortSha(sha: string | null | undefined, length = 10): string {
  return (sha ?? "").slice(0, length);
}

export function plural(count: number, one: string, many = `${one}s`): string {
  return `${count} ${count === 1 ? one : many}`;
}

const RELATIVE = new Intl.RelativeTimeFormat("en", { numeric: "auto" });
const UNITS: Array<[Intl.RelativeTimeFormatUnit, number]> = [
  ["year", 365 * 24 * 3600],
  ["month", 30 * 24 * 3600],
  ["week", 7 * 24 * 3600],
  ["day", 24 * 3600],
  ["hour", 3600],
  ["minute", 60],
];

export function relativeTime(iso: string | null | undefined): string {
  const ms = Date.parse(iso ?? "");
  if (Number.isNaN(ms)) {
    return "unknown";
  }
  const seconds = Math.round((ms - Date.now()) / 1000);
  if (Math.abs(seconds) < 60) {
    return "just now";
  }
  for (const [unit, size] of UNITS) {
    if (Math.abs(seconds) >= size) {
      return RELATIVE.format(Math.round(seconds / size), unit);
    }
  }
  return RELATIVE.format(Math.round(seconds / 60), "minute");
}

export function exactTime(iso: string | null | undefined): string {
  const date = new Date(Date.parse(iso ?? ""));
  if (Number.isNaN(date.getTime())) {
    return "";
  }
  return date.toLocaleString("en-US", {
    year: "numeric",
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
    timeZoneName: "short",
  });
}

// Relative time in the page, exact time on hover and for assistive technology.
export function timeTag(iso: string | null | undefined): string {
  if (!iso) {
    return `<span class="muted">unknown</span>`;
  }
  return `<time datetime="${esc(iso)}" title="${esc(exactTime(iso))}">${esc(relativeTime(iso))}</time>`;
}

export function chip(label: string, tone: Tone = "neutral", extraClass = ""): string {
  return `<span class="chip chip-${tone}${extraClass ? ` ${extraClass}` : ""}">${esc(label)}</span>`;
}

export function initials(name: string | null | undefined): string {
  const words = (name ?? "").trim().split(/[\s._-]+/).filter(Boolean);
  const letters = words
    .slice(0, 2)
    .map((word) => word[0])
    .join("");
  return (letters || "?").toUpperCase();
}

export function person(name: string | null | undefined, login?: string | null): string {
  const label = name || login || "unknown";
  const handle = login && login !== name ? ` <span class="muted">@${esc(login)}</span>` : "";
  return `<span class="person"><span class="avatar" aria-hidden="true">${esc(initials(label))}</span><span>${esc(label)}${handle}</span></span>`;
}

export function copyButton(text: string, label: string, className = "btn btn-ghost btn-sm"): string {
  return `<button type="button" class="${className}" data-copy="${esc(text)}" aria-label="Copy ${esc(label)}">Copy</button>`;
}

// A command with a copy button: copy-ready, monospace, wraps on narrow screens.
export function commandRow(command: string, label = "command"): string {
  return `<div class="cmd"><code class="cmd-text">${esc(command)}</code>${copyButton(command, label, "btn btn-secondary btn-sm")}</div>`;
}

export function banner(tone: Tone, body: string, title?: string): string {
  const heading = title ? `<strong class="banner-title">${esc(title)}</strong> ` : "";
  return `<div class="banner banner-${tone}" role="${tone === "err" ? "alert" : "status"}">${heading}${body}</div>`;
}

export function emptyState(title: string, body: string, action = ""): string {
  return `<div class="empty"><h2 class="empty-title">${esc(title)}</h2><div class="empty-body">${body}</div>${action}</div>`;
}

export function loadingState(label: string): string {
  return `<div class="loading" role="status"><span class="spinner" aria-hidden="true"></span>${esc(label)}</div>`;
}

export function errorState(title: string, message: string): string {
  return `<div class="banner banner-err" role="alert"><strong class="banner-title">${esc(title)}</strong> ${esc(message)} <button type="button" class="btn btn-secondary btn-sm" data-action="retry">Try again</button></div>`;
}

export async function copyText(text: string): Promise<boolean> {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch {
    return false;
  }
}

export function announce(message: string): void {
  const region = byId<HTMLElement>("status");
  if (region) {
    region.textContent = message;
  }
}
