// Pure patch comparison. A patchset's patch is `root..tip`, read from GitHub's compare API as one
// text patch per file. Two patchsets are compared file by file, the way `git patch-id --verbatim`
// treats them: whitespace is significant and hunk line numbers are ignored. The module runs in the
// worker (approval carry-over estimate) and in the browser (change detail compare view).

export interface PatchFile {
  filename: string;
  previous_filename?: string | null;
  status: string;
  additions?: number;
  deletions?: number;
  // Unified hunks as GitHub returns them. null: binary, too large, or otherwise not available.
  patch: string | null;
}

export type FileClassification = "identical" | "changed" | "added" | "dropped" | "unknown";

export interface DiffLine {
  op: "same" | "add" | "del";
  text: string;
  olderLine: number | null;
  newerLine: number | null;
}

export interface FileInterdiff {
  filename: string;
  classification: FileClassification;
  older: PatchFile | null;
  newer: PatchFile | null;
  // Diff of the two patch texts (like `git range-diff`); set only for `changed` files that fit the cap.
  lines: DiffLine[] | null;
  note: string | null;
}

export interface InterdiffResult {
  patchIdentical: boolean;
  files: FileInterdiff[];
  counts: Record<FileClassification, number>;
}

export type DisplayBlock = { kind: "lines"; lines: DiffLine[] } | { kind: "gap"; hidden: number };

export interface PatchLine {
  kind: "hunk" | "add" | "del" | "context" | "meta";
  text: string;
  line: number;
}

// Cap on the LCS table (cells) for a line-level diff of two patch texts.
export const MAX_DIFF_CELLS = 2_500_000;

const HUNK_HEADER = /^@@ -\d+(?:,\d+)? \+\d+(?:,\d+)? @@/;

// Hunk headers carry line numbers that shift on every rebase; the code they describe does not.
export function normalizeLine(line: string): string {
  return HUNK_HEADER.test(line) ? line.replace(HUNK_HEADER, "@@ @@") : line;
}

export function normalizePatch(patch: string): string {
  return patch.split("\n").map(normalizeLine).join("\n");
}

// Canonical text of a patchset's patch: equal fingerprints mean every file has the same status,
// rename source and normalized patch, and the file sets match. null when any patch is unavailable.
export function patchFingerprint(files: readonly PatchFile[]): string | null {
  const rows: Array<[string, string, string | null, string]> = [];
  for (const file of files) {
    if (file.patch === null) {
      return null;
    }
    rows.push([file.filename, file.status, file.previous_filename ?? null, normalizePatch(file.patch)]);
  }
  rows.sort((a, b) => (a[0] < b[0] ? -1 : a[0] > b[0] ? 1 : 0));
  return JSON.stringify(rows);
}

function indexByName(files: readonly PatchFile[]): Map<string, PatchFile> {
  const index = new Map<string, PatchFile>();
  for (const file of files) {
    index.set(file.filename, file);
  }
  return index;
}

function classifyPair(older: PatchFile, newer: PatchFile): FileClassification {
  if (older.patch === null || newer.patch === null) {
    return "unknown";
  }
  if (older.status !== newer.status || (older.previous_filename ?? null) !== (newer.previous_filename ?? null)) {
    return "changed";
  }
  return normalizePatch(older.patch) === normalizePatch(newer.patch) ? "identical" : "changed";
}

export function compareFiles(older: readonly PatchFile[], newer: readonly PatchFile[]): InterdiffResult {
  const olderByName = indexByName(older);
  const newerByName = indexByName(newer);
  const names = [...new Set([...olderByName.keys(), ...newerByName.keys()])].sort();
  const counts: Record<FileClassification, number> = {
    identical: 0,
    changed: 0,
    added: 0,
    dropped: 0,
    unknown: 0,
  };

  const files = names.map((filename): FileInterdiff => {
    const olderFile = olderByName.get(filename) ?? null;
    const newerFile = newerByName.get(filename) ?? null;
    let classification: FileClassification;
    if (olderFile && newerFile) {
      classification = classifyPair(olderFile, newerFile);
    } else if (newerFile) {
      classification = "added";
    } else {
      classification = "dropped";
    }
    counts[classification] += 1;

    let lines: DiffLine[] | null = null;
    let note: string | null = null;
    if (classification === "unknown") {
      note = "Patch text is not available for this file (binary, too large, or not returned by GitHub).";
    } else if (classification === "changed" && olderFile?.patch != null && newerFile?.patch != null) {
      const diff = diffPatches(olderFile.patch, newerFile.patch);
      if ("lines" in diff) {
        lines = diff.lines;
      } else {
        note = diff.tooLarge;
      }
    }
    return { filename, classification, older: olderFile, newer: newerFile, lines, note };
  });

  return {
    patchIdentical: files.every((file) => file.classification === "identical"),
    files,
    counts,
  };
}

type EditOp = { op: "same" | "add" | "del"; ai: number; bi: number };

// Longest-common-subsequence edit script over interned lines. Common prefix and suffix are trimmed
// first, so the table covers only the region that actually changed.
function lcsOps(a: readonly string[], b: readonly string[], maxCells: number): EditOp[] | null {
  const ids = new Map<string, number>();
  const intern = (line: string): number => {
    let id = ids.get(line);
    if (id === undefined) {
      id = ids.size;
      ids.set(line, id);
    }
    return id;
  };
  const ia = Int32Array.from(a, intern);
  const ib = Int32Array.from(b, intern);
  const n = ia.length;
  const m = ib.length;

  let pre = 0;
  while (pre < n && pre < m && ia[pre] === ib[pre]) {
    pre++;
  }
  let suf = 0;
  while (suf < n - pre && suf < m - pre && ia[n - 1 - suf] === ib[m - 1 - suf]) {
    suf++;
  }
  const rows = n - pre - suf;
  const cols = m - pre - suf;
  if (rows * cols > maxCells) {
    return null;
  }

  const width = cols + 1;
  const table = new Uint32Array((rows + 1) * width);
  for (let i = rows - 1; i >= 0; i--) {
    for (let j = cols - 1; j >= 0; j--) {
      table[i * width + j] =
        ia[pre + i] === ib[pre + j]
          ? table[(i + 1) * width + j + 1] + 1
          : Math.max(table[(i + 1) * width + j], table[i * width + j + 1]);
    }
  }

  const ops: EditOp[] = [];
  for (let k = 0; k < pre; k++) {
    ops.push({ op: "same", ai: k, bi: k });
  }
  let i = 0;
  let j = 0;
  while (i < rows && j < cols) {
    if (ia[pre + i] === ib[pre + j]) {
      ops.push({ op: "same", ai: pre + i, bi: pre + j });
      i++;
      j++;
    } else if (table[(i + 1) * width + j] >= table[i * width + j + 1]) {
      ops.push({ op: "del", ai: pre + i, bi: -1 });
      i++;
    } else {
      ops.push({ op: "add", ai: -1, bi: pre + j });
      j++;
    }
  }
  while (i < rows) {
    ops.push({ op: "del", ai: pre + i, bi: -1 });
    i++;
  }
  while (j < cols) {
    ops.push({ op: "add", ai: -1, bi: pre + j });
    j++;
  }
  for (let k = 0; k < suf; k++) {
    ops.push({ op: "same", ai: n - suf + k, bi: m - suf + k });
  }
  return ops;
}

// Line-level diff of two patch texts. Hunk headers match when only their line numbers differ.
export function diffPatches(
  olderPatch: string,
  newerPatch: string,
  maxCells: number = MAX_DIFF_CELLS,
): { lines: DiffLine[] } | { tooLarge: string } {
  const olderLines = olderPatch.split("\n");
  const newerLines = newerPatch.split("\n");
  const ops = lcsOps(olderLines.map(normalizeLine), newerLines.map(normalizeLine), maxCells);
  if (ops === null) {
    return {
      tooLarge: `Too large for a line-by-line diff (${olderLines.length} and ${newerLines.length} lines). Compare the two patches on GitHub instead.`,
    };
  }
  const lines = ops.map(({ op, ai, bi }): DiffLine => {
    if (op === "del") {
      return { op, text: olderLines[ai], olderLine: ai + 1, newerLine: null };
    }
    if (op === "add") {
      return { op, text: newerLines[bi], olderLine: null, newerLine: bi + 1 };
    }
    return { op, text: newerLines[bi], olderLine: ai + 1, newerLine: bi + 1 };
  });
  return { lines };
}

// Keeps changed lines and `context` unchanged lines around them; the rest becomes gap markers.
export function collapseUnchanged(lines: readonly DiffLine[], context = 3): DisplayBlock[] {
  const keep = lines.map(() => false);
  lines.forEach((line, index) => {
    if (line.op === "same") {
      return;
    }
    const from = Math.max(0, index - context);
    const to = Math.min(lines.length - 1, index + context);
    for (let k = from; k <= to; k++) {
      keep[k] = true;
    }
  });

  const blocks: DisplayBlock[] = [];
  let index = 0;
  while (index < lines.length) {
    if (keep[index]) {
      const run: DiffLine[] = [];
      while (index < lines.length && keep[index]) {
        run.push(lines[index]);
        index++;
      }
      blocks.push({ kind: "lines", lines: run });
    } else {
      let hidden = 0;
      while (index < lines.length && !keep[index]) {
        hidden++;
        index++;
      }
      blocks.push({ kind: "gap", hidden });
    }
  }
  return blocks;
}

export interface PatchOnlyResult {
  files: PatchFile[];
  additions: number;
  deletions: number;
  // Files without patch text (binary, too large, or not returned by GitHub).
  unavailable: number;
}

// A patchset compared with the trunk base: its own patch, file by file, with totals. No interdiff.
export function patchOnly(files: readonly PatchFile[]): PatchOnlyResult {
  const sorted = [...files].sort((a, b) => (a.filename < b.filename ? -1 : a.filename > b.filename ? 1 : 0));
  return {
    files: sorted,
    additions: sorted.reduce((sum, file) => sum + (file.additions ?? 0), 0),
    deletions: sorted.reduce((sum, file) => sum + (file.deletions ?? 0), 0),
    unavailable: sorted.filter((file) => file.patch === null).length,
  };
}

// A single patch as display lines (used for a patchset shown against the trunk base).
export function patchLines(patch: string): PatchLine[] {
  return patch.split("\n").map((text, index): PatchLine => {
    let kind: PatchLine["kind"] = "context";
    if (text.startsWith("@@")) {
      kind = "hunk";
    } else if (text.startsWith("+")) {
      kind = "add";
    } else if (text.startsWith("-")) {
      kind = "del";
    } else if (text.startsWith("\\")) {
      kind = "meta";
    }
    return { kind, text, line: index + 1 };
  });
}
