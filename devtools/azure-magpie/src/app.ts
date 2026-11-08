// Magpie UI entry: History API routing with real paths, theme and mobile menu, and delegated handlers.
// Pages are plain HTML strings (see src/ui); this file wires navigation and interaction only.
import { loadRefs, handOffHead, type RefsSnapshot } from "./ui/data";
import { announce, byId, copyText, errorState, shortSha } from "./ui/dom";
import { renderOverview, updateCliSnippet, updateSubtreeLinks, updateWorkspaceRows } from "./ui/overview";
import { filterChangeRows, renderChangesList } from "./ui/changes-list";
import { fillDiffBody, renderChangeDetail, setAllDiffBlocks, updateCompare } from "./ui/change-detail";
import { renderDiffLinks, resolveExactShas, setBase, setHead, updateDiffOutputs } from "./ui/diff-links";

export type ViewName = "overview" | "changes" | "change" | "diff" | "notfound";

export interface Route {
  view: ViewName;
  param: string;
}

const THEME_KEY = "magpie.theme";

export function resolveRoute(pathname: string): Route {
  const path = pathname.replace(/\/+$/, "") || "/";
  if (path === "/" || path === "/index.html") {
    return { view: "overview", param: "" };
  }
  if (path === "/changes") {
    return { view: "changes", param: "" };
  }
  if (path.startsWith("/changes/")) {
    let param = "";
    try {
      param = decodeURIComponent(path.slice("/changes/".length));
    } catch {
      param = path.slice("/changes/".length);
    }
    return { view: "change", param };
  }
  if (path === "/diff") {
    return { view: "diff", param: "" };
  }
  return { view: "notfound", param: "" };
}

const PAGES: Record<ViewName, (route: Route) => Promise<string>> = {
  overview: () => renderOverview(),
  changes: () => renderChangesList(),
  change: (route) => renderChangeDetail(route.param),
  diff: () => renderDiffLinks(),
  notfound: async () =>
    `<header class="page-head"><div><p class="eyebrow">Not found</p><h1 tabindex="-1">Page not found</h1><p class="lead">This page does not exist in Magpie. Start from the <a href="/">overview</a> or browse <a href="/changes">changes</a>.</p></div></header>`,
};

function titleFor(route: Route): string {
  switch (route.view) {
    case "overview":
      return "Overview · Magpie";
    case "changes":
      return "Changes · Magpie";
    case "change":
      if (/^[0-9]+$/.test(route.param)) {
        return `Review #${route.param} · Magpie`;
      }
      return `${route.param.slice(0, 9) || "Change"} · Magpie`;
    case "diff":
      return "Diff links · Magpie";
    case "notfound":
      return "Not found · Magpie";
  }
}

function syncNav(route: Route, hash: string): void {
  let active: string;
  if (route.view === "overview") {
    active = hash === "#workspaces" ? "workspaces" : hash === "#projects" ? "projects" : "overview";
  } else if (route.view === "change") {
    active = "changes";
  } else {
    active = route.view;
  }
  document.querySelectorAll<HTMLAnchorElement>("[data-nav]").forEach((link) => {
    if (link.dataset.nav === active) {
      link.setAttribute("aria-current", "page");
    } else {
      link.removeAttribute("aria-current");
    }
  });
}

function scrollToHash(hash: string): void {
  let id = hash.slice(1);
  try {
    id = decodeURIComponent(id);
  } catch {
    // Keep the raw fragment when it is not valid percent-encoding.
  }
  document.getElementById(id)?.scrollIntoView({ block: "start" });
}

let renderSerial = 0;
let renderedPath: string | null = null;

async function render(options: { focus: boolean; scroll: boolean }): Promise<void> {
  const serial = ++renderSerial;
  const route = resolveRoute(location.pathname);
  syncNav(route, location.hash);
  const view = byId<HTMLElement>("view");
  if (!view) {
    return;
  }
  view.setAttribute("aria-busy", "true");
  let html: string;
  try {
    html = await PAGES[route.view](route);
  } catch (err) {
    html = errorState("Something went wrong.", err instanceof Error ? err.message : "Unknown error.");
  }
  if (serial !== renderSerial) {
    return;
  }
  view.innerHTML = html;
  view.removeAttribute("aria-busy");
  document.title = titleFor(route);
  renderedPath = location.pathname;
  if (location.hash) {
    scrollToHash(location.hash);
  } else if (options.scroll) {
    window.scrollTo(0, 0);
  }
  if (options.focus) {
    document.querySelector<HTMLElement>("#view h1")?.focus({ preventScroll: true });
  }
}

function navigate(href: string, mode: "push" | "replace" = "push"): void {
  const url = new URL(href, location.href);
  setNavOpen(false);
  const target = `${url.pathname}${url.search}${url.hash}`;
  const current = `${location.pathname}${location.search}${location.hash}`;
  if (target !== current) {
    if (mode === "replace") {
      history.replaceState(null, "", target);
    } else {
      history.pushState(null, "", target);
    }
  }
  const samePage = url.pathname === location.pathname && url.search === location.search;
  if (samePage && url.hash) {
    syncNav(resolveRoute(url.pathname), url.hash);
    scrollToHash(url.hash);
    return;
  }
  void render({ focus: true, scroll: true });
}

function setNavOpen(open: boolean): void {
  byId<HTMLElement>("app")?.classList.toggle("nav-open", open);
  byId<HTMLButtonElement>("nav-toggle")?.setAttribute("aria-expanded", String(open));
}

function effectiveTheme(): "light" | "dark" {
  const explicit = document.documentElement.getAttribute("data-theme");
  if (explicit === "light" || explicit === "dark") {
    return explicit;
  }
  return window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
}

function paintThemeToggle(): void {
  const button = byId<HTMLButtonElement>("theme-toggle");
  if (!button) {
    return;
  }
  const next = effectiveTheme() === "dark" ? "light" : "dark";
  button.textContent = next === "dark" ? "Dark" : "Light";
  button.title = `Switch to ${next} theme`;
  button.setAttribute("aria-label", `Switch to ${next} theme`);
}

function toggleTheme(): void {
  const next = effectiveTheme() === "dark" ? "light" : "dark";
  document.documentElement.setAttribute("data-theme", next);
  try {
    localStorage.setItem(THEME_KEY, next);
  } catch {
    // Private mode: the choice lasts for this page only.
  }
  paintThemeToggle();
}

function paintTrunk(refs: RefsSnapshot | null): void {
  const main = byId<HTMLAnchorElement>("trunk-main-link");
  const mainCopy = byId<HTMLButtonElement>("trunk-main-copy");
  const history = byId<HTMLAnchorElement>("trunk-history-link");
  const historyCopy = byId<HTMLButtonElement>("trunk-history-copy");
  const repo = refs?.repo ?? "gosuda/1";
  if (main) {
    main.textContent = refs?.mainSha ? shortSha(refs.mainSha) : "unavailable";
    main.href = refs?.mainSha ? `https://github.com/${repo}/tree/${refs.mainSha}` : `https://github.com/${repo}`;
  }
  if (mainCopy) {
    mainCopy.disabled = !refs?.mainSha;
    mainCopy.setAttribute("data-copy", refs?.mainSha ?? "");
  }
  if (history) {
    history.textContent = refs?.historySha ? shortSha(refs.historySha) : "unavailable";
    history.href = refs?.historySha ? `https://github.com/${repo}/commits/${refs.historySha}` : `https://github.com/${repo}`;
  }
  if (historyCopy) {
    historyCopy.disabled = !refs?.historySha;
    historyCopy.setAttribute("data-copy", refs?.historySha ?? "");
  }
}

async function handleCopy(button: HTMLElement): Promise<void> {
  const text = button.getAttribute("data-copy") ?? "";
  if (!text) {
    return;
  }
  const ok = await copyText(text);
  const original = button.textContent;
  if (button.tagName === "BUTTON") {
    button.textContent = ok ? "Copied" : "Copy failed";
    window.setTimeout(() => {
      button.textContent = original;
    }, 1500);
  }
  announce(ok ? "Copied to clipboard." : "Could not copy. Select the text and copy it instead.");
}

function cliInputs(): { actor: string; task: string; paths: string } {
  return {
    actor: byId<HTMLInputElement>("cli-actor")?.value ?? "",
    task: byId<HTMLInputElement>("cli-task")?.value ?? "",
    paths: byId<HTMLInputElement>("cli-paths")?.value ?? "",
  };
}

async function handleAction(button: HTMLElement, event: Event): Promise<void> {
  const action = button.dataset.action;
  switch (action) {
    case "reload":
    case "retry":
      event.preventDefault();
      void render({ focus: false, scroll: false });
      return;
    case "use-head":
      event.preventDefault();
      handOffHead(button.dataset.ref ?? "");
      navigate("/diff");
      return;
    case "pick-head":
      setHead(button.dataset.ref ?? "");
      announce(`Head set to ${button.dataset.ref ?? ""}.`);
      return;
    case "set-base":
      setBase(button.dataset.ref ?? "suda/main");
      return;
    case "expand-diffs":
      setAllDiffBlocks(true);
      return;
    case "collapse-diffs":
      setAllDiffBlocks(false);
      return;
    case "resolve": {
      event.preventDefault();
      const status = byId<HTMLElement>("diff-status");
      if (status) {
        status.textContent = "Resolving…";
      }
      try {
        const message = await resolveExactShas();
        if (status) {
          status.textContent = message;
        }
      } catch {
        updateDiffOutputs();
        if (status) {
          status.textContent = "Could not resolve the names. The links still work; they resolve when opened.";
        }
      }
      return;
    }
  }
}

function initDelegation(): void {
  document.addEventListener("click", (event) => {
    if (event.defaultPrevented || event.button !== 0) {
      return;
    }
    const target = event.target instanceof Element ? event.target : null;
    if (!target) {
      return;
    }
    const copyTarget = target.closest<HTMLElement>("[data-copy]");
    if (copyTarget) {
      event.preventDefault();
      void handleCopy(copyTarget);
      return;
    }
    const actionTarget = target.closest<HTMLElement>("[data-action]");
    if (actionTarget) {
      void handleAction(actionTarget, event);
      return;
    }
    const link = target.closest<HTMLAnchorElement>("a[href]");
    if (!link || link.target || link.hasAttribute("download")) {
      return;
    }
    if (event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) {
      return;
    }
    const url = new URL(link.href);
    if (url.origin !== location.origin) {
      return;
    }
    event.preventDefault();
    navigate(url.href);
  });

  document.addEventListener("input", (event) => {
    const field = event.target instanceof HTMLInputElement ? event.target : null;
    if (!field) {
      return;
    }
    if (field.dataset.filter === "changes") {
      filterChangeRows(field.value);
    } else if (field.dataset.filter === "workspaces") {
      updateWorkspaceRows(field.value);
    } else if (field.dataset.field === "subtree") {
      updateSubtreeLinks(field.value);
    } else if (field.dataset.field === "cli") {
      updateCliSnippet(cliInputs());
    } else if (field.id === "base-ref" || field.id === "head-ref") {
      updateDiffOutputs();
    }
  });

  document.addEventListener("change", (event) => {
    const field = event.target instanceof HTMLSelectElement ? event.target : null;
    if (!field) {
      return;
    }
    if (field.id === "diff-mode") {
      updateDiffOutputs();
    } else if (field.dataset.compare) {
      const from = byId<HTMLSelectElement>("cmp-from")?.value ?? "base";
      const to = byId<HTMLSelectElement>("cmp-to")?.value ?? "";
      updateCompare(from, to);
    }
  });

  // `toggle` does not bubble; the capture listener sees diff blocks as they open.
  document.addEventListener(
    "toggle",
    (event) => {
      const block = event.target instanceof HTMLDetailsElement ? event.target : null;
      if (block && block.classList.contains("diff-file") && block.open) {
        fillDiffBody(block);
      }
    },
    true,
  );

  window.addEventListener("popstate", () => {
    setNavOpen(false);
    if (location.pathname === renderedPath) {
      syncNav(resolveRoute(location.pathname), location.hash);
      if (location.hash) {
        scrollToHash(location.hash);
      }
      return;
    }
    void render({ focus: false, scroll: false });
  });

  window.addEventListener("keydown", (event) => {
    if (event.key === "Escape") {
      setNavOpen(false);
    }
  });
}

function initTheme(): void {
  byId<HTMLButtonElement>("theme-toggle")?.addEventListener("click", toggleTheme);
  window.matchMedia("(prefers-color-scheme: dark)").addEventListener("change", paintThemeToggle);
  paintThemeToggle();
}

function initNav(): void {
  byId<HTMLButtonElement>("nav-toggle")?.addEventListener("click", () => {
    const open = byId<HTMLElement>("app")?.classList.contains("nav-open") ?? false;
    setNavOpen(!open);
  });
}

async function boot(): Promise<void> {
  initTheme();
  initNav();
  initDelegation();
  const refs = await loadRefs();
  paintTrunk(refs.ok ? refs.value : null);
  await render({ focus: false, scroll: false });
}

if (typeof document !== "undefined") {
  void boot();
}
