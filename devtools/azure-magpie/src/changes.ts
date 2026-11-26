// Read model for changes: a change is the set of `refs/ws/cl/<change-id>/<tip-sha>` patchsets that
// share one Change-Id. Everything is read from the user repository (gosuda/1). Commits and compares
// are immutable by sha and cached for days; ref and pull request listings use short TTLs.
import type {
  ApprovalInfo,
  ChangeDetailPayload,
  ChangeListItem,
  ChangeListPayload,
  PatchsetInfo,
  PullRequestInfo,
  ReviewRecord,
} from "./api-types";
import { readThrough } from "./cache";
import type { Env } from "./env";
import { BudgetExhaustedError, GithubReadError, ReadBudget, githubGet, mapLimit } from "./github";
import { jsonResponse } from "./http";
import { type PatchFile, patchFingerprint } from "./interdiff";
import { isHiddenRef } from "./refs";

const CHANGE_ID_PATTERN = /^I[0-9a-f]{40}$/;
const CHANGE_REF_PATTERN = /^refs\/ws\/cl\/(I[0-9a-f]{40})\/([0-9a-f]{40})$/;
const CHANGE_ID_TRAILER = /^Change-Id:\s*(I[0-9a-f]{40})\s*$/gm;
const EMBARGOED_SUBJECT_PREFIX = "sec-";
const DECISIVE_REVIEW_STATES: Record<string, true> = { APPROVED: true, CHANGES_REQUESTED: true, DISMISSED: true };

const COMMIT_LOOKUP_LIMIT = 150;
const ROOT_WALK_DEPTH = 50;
const COMPARE_FILE_CAP = 300;
const LIST_CONCURRENCY = 4;
const HEAD_LOOKUP_LIMIT = 30;
const PULLS_PER_PAGE = 100;
const PULL_PAGE_LIMIT = 3;
const IMMUTABLE_TTL_SECONDS = 7 * 24 * 60 * 60;
const PULLS_TTL_SECONDS = 15;

interface ChangeRefEntry {
  changeId: string;
  sha: string;
  ref: string;
}

interface Stamp {
  name: string | null;
  email: string | null;
  date: string | null;
}

interface CommitInfo {
  sha: string;
  subject: string;
  message: string;
  parents: string[];
  author: Stamp;
  committer: Stamp;
}

interface CompareResult {
  files: PatchFile[];
  truncated: boolean;
}

interface PullSummary {
  number: number;
  title: string;
  url: string;
  state: "open" | "closed";
  merged: boolean;
  authorLogin: string | null;
  headSha: string;
  headRef: string;
}

interface PatchsetRecord {
  ref: string;
  sha: string;
  commit: CommitInfo;
}

interface ReviewLookup {
  unavailable: string | null;
  found: Map<string, PullSummary>;
}

export class ChangeNotFoundError extends Error {
  readonly reviewUnavailable: string | null;

  constructor(message: string, reviewUnavailable: string | null = null) {
    super(message);
    this.name = "ChangeNotFoundError";
    this.reviewUnavailable = reviewUnavailable;
  }
}

interface RawStamp {
  name?: string | null;
  email?: string | null;
  date?: string | null;
}

interface RawCommit {
  message?: string;
  parents?: Array<{ sha: string }>;
  author?: RawStamp | null;
  committer?: RawStamp | null;
}

interface RawFile {
  filename: string;
  previous_filename?: string | null;
  status: string;
  additions?: number;
  deletions?: number;
  patch?: string;
}

interface RawPull {
  number: number;
  title?: string;
  html_url: string;
  state: string;
  merged_at?: string | null;
  user?: { login?: string } | null;
  head: { sha: string; ref: string };
  base: { ref: string };
}

interface RawReview {
  user?: { login?: string } | null;
  state: string;
  commit_id: string;
  submitted_at?: string | null;
}

function messageOf(err: unknown): string {
  return err instanceof Error ? err.message : "unknown error";
}

function timeOf(iso: string | null | undefined): number {
  return Date.parse(iso ?? "") || 0;
}

function toStamp(raw: RawStamp | null | undefined): Stamp {
  return { name: raw?.name ?? null, email: raw?.email ?? null, date: raw?.date ?? null };
}

function toCommitInfo(sha: string, raw: RawCommit): CommitInfo {
  const message = raw.message ?? "";
  return {
    sha,
    subject: (message.split("\n", 1)[0] ?? "").replace(/\r$/, "").trim(),
    message,
    parents: (raw.parents ?? []).map((parent) => parent.sha),
    author: toStamp(raw.author),
    committer: toStamp(raw.committer),
  };
}

function toPatchFile(raw: RawFile): PatchFile {
  return {
    filename: raw.filename,
    previous_filename: raw.previous_filename ?? null,
    status: raw.status,
    additions: raw.additions ?? 0,
    deletions: raw.deletions ?? 0,
    // GitHub omits `patch` for binary and very large files; those stay null (unknown), never guessed.
    patch: typeof raw.patch === "string" ? raw.patch : null,
  };
}

function toPullSummary(raw: RawPull): PullSummary | null {
  if (isHiddenRef(raw.head.ref) || isHiddenRef(raw.base.ref)) {
    return null;
  }
  return {
    number: raw.number,
    title: raw.title ?? "",
    url: raw.html_url,
    state: raw.state === "open" ? "open" : "closed",
    merged: Boolean(raw.merged_at),
    authorLogin: raw.user?.login ?? null,
    headSha: raw.head.sha,
    headRef: raw.head.ref,
  };
}

function toPullInfo(pull: PullSummary): PullRequestInfo {
  return { number: pull.number, title: pull.title, url: pull.url, state: pull.state, merged: pull.merged };
}

function toReviewRecord(raw: RawReview): ReviewRecord {
  return {
    login: raw.user?.login ?? "ghost",
    state: raw.state,
    commitId: raw.commit_id,
    submittedAt: raw.submitted_at ?? null,
  };
}

// The Change-Id trailer names the change; the last one wins if a message repeats it.
function changeIdOf(commit: CommitInfo): string | null {
  let changeId: string | null = null;
  for (const match of commit.message.matchAll(CHANGE_ID_TRAILER)) {
    changeId = match[1];
  }
  return changeId;
}

function parseChangeRef(ref: string): ChangeRefEntry | null {
  if (isHiddenRef(ref)) {
    return null;
  }
  const match = CHANGE_REF_PATTERN.exec(ref);
  return match ? { changeId: match[1], sha: match[2], ref } : null;
}

// Reads that cost GitHub quota go through here: each uncached call spends one unit of the request
// budget. Cached values, and every call made under the same memo key, are free.
class Reader {
  readonly budget: ReadBudget;
  private readonly memo = new Map<string, Promise<unknown>>();

  constructor(
    private readonly env: Env,
    private readonly repo: string,
    limit: number,
  ) {
    this.budget = new ReadBudget(limit);
  }

  private once<T>(key: string, load: () => Promise<T>): Promise<T> {
    if (!this.memo.has(key)) {
      this.memo.set(key, load());
    }
    return this.memo.get(key) as Promise<T>;
  }

  commit(sha: string): Promise<CommitInfo> {
    return this.once(`commit ${sha}`, () =>
      readThrough(`commit:${this.repo}:${sha}`, IMMUTABLE_TTL_SECONDS, async () => {
        this.budget.spend();
        const raw = await githubGet<RawCommit>(this.env, this.repo, `git/commits/${sha}`);
        return toCommitInfo(sha, raw);
      }),
    );
  }

  compare(root: string, tip: string): Promise<CompareResult> {
    const range = `${root}...${tip}`;
    return this.once(`compare ${range}`, () =>
      readThrough(`compare:${this.repo}:${range}`, IMMUTABLE_TTL_SECONDS, async () => {
        this.budget.spend();
        const raw = await githubGet<{ files?: RawFile[] }>(this.env, this.repo, `compare/${range}`);
        const files = (raw.files ?? []).map(toPatchFile);
        return { files, truncated: files.length >= COMPARE_FILE_CAP };
      }),
    );
  }

  // Newest-updated pull requests (open and closed), a few pages at most. Pull requests on embargoed
  // branches are dropped before anything is cached or matched.
  pulls(): Promise<PullSummary[]> {
    return this.once("pulls", async () => {
      const all: PullSummary[] = [];
      for (let page = 1; page <= PULL_PAGE_LIMIT; page++) {
        const { count, items } = await readThrough(
          `pulls:${this.repo}:${page}`,
          PULLS_TTL_SECONDS,
          async () => {
            const raw = await githubGet<RawPull[]>(
              this.env,
              this.repo,
              `pulls?state=all&sort=updated&direction=desc&per_page=${PULLS_PER_PAGE}&page=${page}`,
            );
            const summaries = raw.flatMap((pull) => {
              const summary = toPullSummary(pull);
              return summary ? [summary] : [];
            });
            return { count: raw.length, items: summaries };
          },
        );
        all.push(...items);
        if (count < PULLS_PER_PAGE) {
          break;
        }
      }
      return all;
    });
  }

  pull(number: number): Promise<PullSummary | null> {
    return this.once(`pull ${number}`, async () => {
      this.budget.spend();
      return toPullSummary(await githubGet<RawPull>(this.env, this.repo, `pulls/${number}`));
    });
  }

  reviews(number: number): Promise<ReviewRecord[]> {
    return this.once(`reviews ${number}`, () =>
      readThrough(`reviews:${this.repo}:${number}`, PULLS_TTL_SECONDS, async () => {
        const raw = await githubGet<RawReview[]>(this.env, this.repo, `pulls/${number}/reviews?per_page=100`);
        return raw.map(toReviewRecord);
      }),
    );
  }
}

// Ref listing with a short in-memory TTL (CACHE_TTL_SECONDS, default 5). The webhook clears it.
let refListing: { repo: string; fetchedAtMs: number; entries: ChangeRefEntry[] } | null = null;

export function clearChangeRefCache(): void {
  refListing = null;
}

function refTtlMs(env: Env): number {
  const raw = env.CACHE_TTL_SECONDS;
  if (raw === undefined || raw.trim() === "") {
    return 5_000;
  }
  return Math.max(0, Number(raw) || 0) * 1000;
}

async function listChangeRefs(env: Env, repo: string): Promise<ChangeRefEntry[]> {
  const now = Date.now();
  if (refListing && refListing.repo === repo && now - refListing.fetchedAtMs < refTtlMs(env)) {
    return refListing.entries;
  }
  const raw = await githubGet<Array<{ ref?: string }>>(env, repo, "git/matching-refs/ws/cl/");
  const entries: ChangeRefEntry[] = [];
  for (const item of raw) {
    const entry = parseChangeRef(item.ref ?? "");
    if (entry) {
      entries.push(entry);
    }
  }
  refListing = { repo, fetchedAtMs: now, entries };
  return entries;
}

function groupByChange(entries: ChangeRefEntry[]): Map<string, ChangeRefEntry[]> {
  const groups = new Map<string, ChangeRefEntry[]>();
  for (const entry of entries) {
    const group = groups.get(entry.changeId);
    if (group) {
      group.push(entry);
    } else {
      groups.set(entry.changeId, [entry]);
    }
  }
  return groups;
}

// Oldest committer date first (the PS1..n order), sha as a stable tie-break.
function byPatchsetOrder(a: PatchsetRecord, b: PatchsetRecord): number {
  return (
    timeOf(a.commit.committer.date) - timeOf(b.commit.committer.date) ||
    (a.sha < b.sha ? -1 : a.sha > b.sha ? 1 : 0)
  );
}

async function readPatchsets(reader: Reader, refs: ChangeRefEntry[]): Promise<PatchsetRecord[]> {
  const commits = await Promise.all(refs.map((entry) => reader.commit(entry.sha)));
  return refs
    .map((entry, index) => ({ ref: entry.ref, sha: entry.sha, commit: commits[index] }))
    .sort(byPatchsetOrder);
}

function isEmbargoed(record: PatchsetRecord): boolean {
  return record.commit.subject.startsWith(EMBARGOED_SUBJECT_PREFIX);
}

// Walks parents until the parentless commits are found. A patchset is `root..tip` only when exactly
// one such root exists, so anything else is an error for that patchset rather than a silent pick.
async function findRoot(reader: Reader, tip: CommitInfo): Promise<string> {
  const roots = new Set<string>();
  const seen = new Set<string>([tip.sha]);
  let frontier: CommitInfo[] = [tip];
  for (let depth = 0; frontier.length > 0; depth++) {
    const next: string[] = [];
    for (const commit of frontier) {
      if (commit.parents.length === 0) {
        roots.add(commit.sha);
        continue;
      }
      for (const parent of commit.parents) {
        if (!seen.has(parent)) {
          seen.add(parent);
          next.push(parent);
        }
      }
    }
    if (next.length === 0) {
      break;
    }
    if (depth + 1 > ROOT_WALK_DEPTH) {
      throw new Error(`history is deeper than ${ROOT_WALK_DEPTH} commits, so its root was not found`);
    }
    frontier = await Promise.all(next.map((sha) => reader.commit(sha)));
  }
  if (roots.size !== 1) {
    throw new Error(
      `history has ${roots.size} parentless roots; a patchset must sit on exactly one trunk snapshot`,
    );
  }
  return [...roots][0];
}

async function describePatchset(reader: Reader, record: PatchsetRecord, number: number): Promise<PatchsetInfo> {
  const { commit } = record;
  const info: PatchsetInfo = {
    number,
    sha: record.sha,
    ref: record.ref,
    rootSha: null,
    date: commit.committer.date ?? "",
    subject: commit.subject,
    message: commit.message,
    author: commit.author,
    files: null,
    filesTruncated: false,
    error: null,
    carriesTo: [],
  };
  try {
    info.rootSha = await findRoot(reader, commit);
    const compared = await reader.compare(info.rootSha, record.sha);
    info.files = compared.files;
    info.filesTruncated = compared.truncated;
  } catch (err) {
    info.error = messageOf(err);
  }
  return info;
}

// Estimate only: a later patchset with a patch identical to this one would keep its approvals if the
// lander judges the same patch-id. The lander decides; the UI labels this as an estimate.
function fillCarries(patchsets: PatchsetInfo[]): void {
  const prints = patchsets.map((patchset) => (patchset.files ? patchFingerprint(patchset.files) : null));
  for (let i = 0; i < patchsets.length; i++) {
    for (let j = i + 1; j < patchsets.length; j++) {
      if (prints[i] !== null && prints[i] === prints[j]) {
        patchsets[i].carriesTo.push(patchsets[j].number);
      }
    }
  }
}

// The latest decisive review per reviewer counts; an approval is an APPROVED state that is still
// the reviewer's latest decisive one. COMMENTED reviews never change it.
function effectiveApprovals(reviews: ReviewRecord[], numberOfSha: Map<string, number>): ApprovalInfo[] {
  const decisive = reviews
    .filter((review) => DECISIVE_REVIEW_STATES[review.state] === true)
    .sort((a, b) => timeOf(a.submittedAt) - timeOf(b.submittedAt));
  const latest = new Map<string, ReviewRecord>();
  for (const review of decisive) {
    latest.set(review.login, review);
  }
  return [...latest.values()]
    .filter((review) => review.state === "APPROVED")
    .map((review) => ({
      login: review.login,
      commitId: review.commitId,
      patchset: numberOfSha.get(review.commitId) ?? null,
      submittedAt: review.submittedAt,
    }));
}

export function reviewUnavailableReason(err: unknown): string {
  if (err instanceof GithubReadError) {
    if (err.status === 403) {
      return "Review status is unavailable: the magpie GitHub App cannot read pull requests on gosuda/1 (HTTP 403). Grant it pull_requests:read to show review numbers and approvals.";
    }
    if (err.status === 404) {
      return "Review status is unavailable: pull requests are not visible to the magpie GitHub App on gosuda/1 (HTTP 404). Grant it pull_requests:read to show them.";
    }
    return `Review status could not be read (GitHub returned HTTP ${err.status}).`;
  }
  return "Review status could not be read from GitHub right now.";
}

function preferredPull(candidate: PullSummary, current: PullSummary): boolean {
  if ((candidate.state === "open") !== (current.state === "open")) {
    return candidate.state === "open";
  }
  return candidate.number > current.number;
}

// Matches pull requests to changes: a head at a published patchset tip, or a `review/*` head whose
// tip carries the change's Change-Id (the branch moved past its last patchset). Degrades to a reason
// when the App cannot read pull requests.
async function findReviewPulls(
  reader: Reader,
  targets: Map<string, Set<string>>,
  pinned: { changeId: string; pull: PullSummary } | null = null,
): Promise<ReviewLookup> {
  // A pull request already read by number is matched without consulting the listing window.
  const found = new Map<string, PullSummary>();
  if (pinned) {
    found.set(pinned.changeId, pinned.pull);
  }
  let pulls: PullSummary[];
  try {
    pulls = await reader.pulls();
  } catch (err) {
    return { unavailable: pinned ? null : reviewUnavailableReason(err), found };
  }

  const changeOfSha = new Map<string, string>();
  for (const [changeId, shas] of targets) {
    for (const sha of shas) {
      changeOfSha.set(sha, changeId);
    }
  }

  const offer = (changeId: string, pull: PullSummary): void => {
    const current = found.get(changeId);
    if (!current || preferredPull(pull, current)) {
      found.set(changeId, pull);
    }
  };
  const branchHeads: PullSummary[] = [];
  for (const pull of pulls) {
    const changeId = changeOfSha.get(pull.headSha);
    if (changeId) {
      offer(changeId, pull);
    } else if (pull.headRef.startsWith("review/")) {
      branchHeads.push(pull);
    }
  }

  for (const pull of branchHeads.slice(0, HEAD_LOOKUP_LIMIT)) {
    try {
      const changeId = changeIdOf(await reader.commit(pull.headSha));
      if (changeId && targets.has(changeId)) {
        offer(changeId, pull);
      }
    } catch (err) {
      if (err instanceof BudgetExhaustedError) {
        break;
      }
    }
  }
  return { unavailable: null, found };
}

function listNote(truncated: boolean, omitted: number): string | null {
  if (truncated) {
    return `Stopped after ${COMMIT_LOOKUP_LIMIT} GitHub reads for this request, so ${omitted} change${omitted === 1 ? " is" : "s are"} not shown yet. Reload to continue; reads already made are cached.`;
  }
  if (omitted > 0) {
    return `${omitted} change${omitted === 1 ? " is" : "s are"} not shown because GitHub could not be read for ${omitted === 1 ? "it" : "them"}.`;
  }
  return null;
}

export async function listChanges(env: Env, repo: string): Promise<ChangeListPayload> {
  const reader = new Reader(env, repo, COMMIT_LOOKUP_LIMIT);
  const groups = [...groupByChange(await listChangeRefs(env, repo))].sort(([a], [b]) =>
    a < b ? -1 : a > b ? 1 : 0,
  );

  let omitted = 0;
  let truncated = false;
  const read = await mapLimit(groups, LIST_CONCURRENCY, async ([changeId, refs]) => {
    try {
      const patchsets = await readPatchsets(reader, refs);
      // Embargoed by subject: hidden from the list without saying it exists.
      if (patchsets.some(isEmbargoed)) {
        return null;
      }
      return { changeId, patchsets };
    } catch (err) {
      omitted += 1;
      if (err instanceof BudgetExhaustedError) {
        truncated = true;
      }
      return null;
    }
  });
  const readable = read.flatMap((entry) => (entry ? [entry] : []));

  const targets = new Map<string, Set<string>>(
    readable.map(({ changeId, patchsets }) => [changeId, new Set(patchsets.map((p) => p.sha))]),
  );
  const lookup = await findReviewPulls(reader, targets);

  const changes: ChangeListItem[] = readable.map(({ changeId, patchsets }) => {
    const first = patchsets[0];
    const newest = patchsets[patchsets.length - 1];
    const pull = lookup.found.get(changeId) ?? null;
    return {
      id: changeId,
      subject: newest.commit.subject,
      patchsetCount: patchsets.length,
      updatedAt: newest.commit.committer.date ?? "",
      author: { name: first.commit.author.name, login: pull?.authorLogin ?? null },
      review: pull ? toPullInfo(pull) : null,
    };
  });
  changes.sort((a, b) => timeOf(b.updatedAt) - timeOf(a.updatedAt) || (a.id < b.id ? -1 : 1));

  return {
    repo,
    changes,
    truncated,
    omittedChanges: omitted,
    note: listNote(truncated, omitted),
    commitLookupLimit: COMMIT_LOOKUP_LIMIT,
    reviewUnavailable: lookup.unavailable,
  };
}

export async function getChangeDetail(
  env: Env,
  repo: string,
  changeId: string,
  pinnedPull: PullSummary | null = null,
): Promise<ChangeDetailPayload> {
  // Malformed ids are rejected before any GitHub call.
  if (!CHANGE_ID_PATTERN.test(changeId)) {
    throw new ChangeNotFoundError(`${changeId} is not a change id`);
  }
  const reader = new Reader(env, repo, COMMIT_LOOKUP_LIMIT);
  const refs = (await listChangeRefs(env, repo)).filter((entry) => entry.changeId === changeId);
  const notFound = new ChangeNotFoundError(`No published patchsets for change ${changeId}.`);
  if (refs.length === 0) {
    throw notFound;
  }
  const records = await readPatchsets(reader, refs);
  if (records.some(isEmbargoed)) {
    throw notFound;
  }

  const patchsets: PatchsetInfo[] = [];
  for (const [index, record] of records.entries()) {
    patchsets.push(await describePatchset(reader, record, index + 1));
  }
  fillCarries(patchsets);

  const targets = new Map([[changeId, new Set(records.map((r) => r.sha))]]);
  const lookup = await findReviewPulls(reader, targets, pinnedPull ? { changeId, pull: pinnedPull } : null);
  const pull = lookup.found.get(changeId) ?? null;
  let reviews: ReviewRecord[] = [];
  let reviewUnavailable = lookup.unavailable;
  if (pull) {
    try {
      reviews = await reader.reviews(pull.number);
    } catch (err) {
      reviewUnavailable = reviewUnavailableReason(err);
    }
  }

  const numberOfSha = new Map(patchsets.map((patchset) => [patchset.sha, patchset.number]));
  const newest = records[records.length - 1].commit;
  return {
    repo,
    id: changeId,
    subject: newest.subject,
    updatedAt: newest.committer.date ?? "",
    patchsets,
    review: pull ? toPullInfo(pull) : null,
    reviews,
    approvals: effectiveApprovals(reviews, numberOfSha),
    reviewUnavailable,
  };
}

// `/api/changes/<N>`: pull request #N -> head commit -> Change-Id -> the change. The pull request is
// read by number, so it resolves even when it is outside the newest-updated listing window.
export async function resolveChangeIdFromPull(
  env: Env,
  repo: string,
  number: number,
): Promise<{ changeId: string; pull: PullSummary }> {
  const reader = new Reader(env, repo, COMMIT_LOOKUP_LIMIT);
  let pull: PullSummary | null;
  try {
    pull = await reader.pull(number);
  } catch (err) {
    const status = err instanceof GithubReadError ? ` (HTTP ${err.status})` : "";
    throw new ChangeNotFoundError(
      `Pull request #${number} could not be read${status}.`,
      reviewUnavailableReason(err),
    );
  }
  if (!pull) {
    throw new ChangeNotFoundError(`Pull request #${number} is not available.`);
  }
  let changeId: string | null;
  try {
    changeId = changeIdOf(await reader.commit(pull.headSha));
  } catch (err) {
    throw new ChangeNotFoundError(`The head commit of pull request #${number} could not be read: ${messageOf(err)}`);
  }
  if (!changeId) {
    throw new ChangeNotFoundError(`Pull request #${number} has no Change-Id trailer on its head commit.`);
  }
  return { changeId, pull };
}

export function changeErrorResponse(err: unknown): Response {
  if (err instanceof ChangeNotFoundError) {
    return jsonResponse(
      err.reviewUnavailable ? { error: err.message, reviewUnavailable: err.reviewUnavailable } : { error: err.message },
      404,
    );
  }
  if (err instanceof BudgetExhaustedError) {
    return jsonResponse({ error: err.message }, 503);
  }
  if (err instanceof GithubReadError) {
    return jsonResponse({ error: `magpie could not read gosuda/1: ${err.message}` }, 502);
  }
  return jsonResponse({ error: messageOf(err) }, 500);
}

export async function handleChangesApi(url: URL, env: Env, repo: string): Promise<Response> {
  const path = url.pathname.replace(/\/+$/, "");
  try {
    if (path === "/api/changes") {
      return jsonResponse(await listChanges(env, repo));
    }
    const rest = path.slice("/api/changes/".length);
    if (/^[0-9]+$/.test(rest)) {
      const resolved = await resolveChangeIdFromPull(env, repo, Number(rest));
      return jsonResponse(await getChangeDetail(env, repo, resolved.changeId, resolved.pull));
    }
    return jsonResponse(await getChangeDetail(env, repo, rest));
  } catch (err) {
    return changeErrorResponse(err);
  }
}
