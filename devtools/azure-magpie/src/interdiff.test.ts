import { describe, expect, test } from "bun:test";
import {
  collapseUnchanged,
  compareFiles,
  diffPatches,
  type DiffLine,
  normalizePatch,
  patchFingerprint,
  patchLines,
  patchOnly,
  type PatchFile,
} from "./interdiff";

const OLD_HUNK = `@@ -10,3 +10,4 @@ func load() error {
 	cfg := read()
-	return nil
+	if cfg == nil {
+		return errNoConfig
+	}
+	return nil
 }`;

// The same change after a rebase: the code is identical, the hunk starts 30 lines lower.
const REBASED_HUNK = `@@ -40,3 +44,4 @@ func load() error {
 	cfg := read()
-	return nil
+	if cfg == nil {
+		return errNoConfig
+	}
+	return nil
 }`;

function file(filename: string, patch: string | null, overrides: Partial<PatchFile> = {}): PatchFile {
  return { filename, status: "modified", additions: 1, deletions: 1, patch, ...overrides };
}

describe("normalizePatch", () => {
  test("ignores hunk line numbers but keeps the code and the function context", () => {
    expect(normalizePatch(OLD_HUNK)).toBe(normalizePatch(REBASED_HUNK));
    expect(normalizePatch(OLD_HUNK)).toContain("@@ @@ func load() error {");
  });

  test("keeps whitespace significant", () => {
    expect(normalizePatch("+return x")).not.toBe(normalizePatch("+return  x"));
  });
});

describe("compareFiles", () => {
  test("a rebase that only shifts line numbers is patch-identical", () => {
    const older = [file("ipc/hqq/load.go", OLD_HUNK)];
    const newer = [file("ipc/hqq/load.go", REBASED_HUNK)];
    const result = compareFiles(older, newer);
    expect(result.patchIdentical).toBe(true);
    expect(result.counts.identical).toBe(1);
    expect(result.files[0].classification).toBe("identical");
    expect(result.files[0].lines).toBeNull();
    expect(patchFingerprint(older)).toBe(patchFingerprint(newer));
  });

  test("a whitespace-only change is detected and shown line by line", () => {
    const older = [file("a.go", "@@ -1,2 +1,2 @@\n-x := 1\n+return x\n")];
    const newer = [file("a.go", "@@ -1,2 +1,2 @@\n-x := 1\n+return  x\n")];
    const result = compareFiles(older, newer);
    expect(result.patchIdentical).toBe(false);
    expect(result.files[0].classification).toBe("changed");
    const lines = result.files[0].lines ?? [];
    expect(lines.filter((line) => line.op === "del").map((line) => line.text)).toEqual(["+return x"]);
    expect(lines.filter((line) => line.op === "add").map((line) => line.text)).toEqual(["+return  x"]);
    expect(patchFingerprint(older)).not.toBe(patchFingerprint(newer));
  });

  test("classifies files only in the newer side as added and only in the older side as dropped", () => {
    const older = [file("gone.go", OLD_HUNK), file("same.go", OLD_HUNK)];
    const newer = [file("new.go", OLD_HUNK), file("same.go", REBASED_HUNK)];
    const result = compareFiles(older, newer);
    const byName = Object.fromEntries(result.files.map((f) => [f.filename, f.classification]));
    expect(byName).toEqual({ "gone.go": "dropped", "new.go": "added", "same.go": "identical" });
    expect(result.counts).toEqual({ identical: 1, changed: 0, added: 1, dropped: 1, unknown: 0 });
    expect(result.patchIdentical).toBe(false);
  });

  test("a side without patch text makes the file unknown, never identical", () => {
    const older = [file("bin.png", null, { status: "modified", additions: 0, deletions: 0 })];
    const newer = [file("bin.png", null, { status: "modified", additions: 0, deletions: 0 })];
    const result = compareFiles(older, newer);
    expect(result.files[0].classification).toBe("unknown");
    expect(result.files[0].note).toContain("not available");
    expect(result.patchIdentical).toBe(false);

    const mixed = compareFiles([file("x.go", OLD_HUNK)], [file("x.go", null)]);
    expect(mixed.files[0].classification).toBe("unknown");
    expect(mixed.patchIdentical).toBe(false);
  });

  test("a changed status or rename source is a change even when the hunks match", () => {
    const older = [file("a.go", OLD_HUNK, { status: "modified" })];
    const newer = [file("a.go", REBASED_HUNK, { status: "renamed", previous_filename: "b.go" })];
    const result = compareFiles(older, newer);
    expect(result.files[0].classification).toBe("changed");
    expect(result.patchIdentical).toBe(false);
  });

  test("an empty patchset compared with itself is identical", () => {
    expect(compareFiles([], []).patchIdentical).toBe(true);
  });
});

describe("patchFingerprint", () => {
  test("is independent of file order and is null while any patch is unavailable", () => {
    const a = [file("a.go", OLD_HUNK), file("b.go", "@@ -1 +1 @@\n-a\n+b")];
    const b = [file("b.go", "@@ -9 +9 @@\n-a\n+b"), file("a.go", REBASED_HUNK)];
    expect(patchFingerprint(a)).toBe(patchFingerprint(b));
    expect(patchFingerprint([file("a.go", null)])).toBeNull();
  });
});

describe("diffPatches", () => {
  test("produces a line-level diff of two patch texts with gutters on both sides", () => {
    const result = diffPatches("@@ -1,3 +1,3 @@\n a\n-b\n+c\n d", "@@ -1,3 +1,3 @@\n a\n-b\n+x\n d");
    if (!("lines" in result)) {
      throw new Error("expected a diff");
    }
    expect(result.lines.map((line) => [line.op, line.text])).toEqual([
      ["same", "@@ -1,3 +1,3 @@"],
      ["same", " a"],
      ["same", "-b"],
      ["del", "+c"],
      ["add", "+x"],
      ["same", " d"],
    ]);
    const del = result.lines.find((line) => line.op === "del");
    const add = result.lines.find((line) => line.op === "add");
    expect(del?.olderLine).toBe(4);
    expect(del?.newerLine).toBeNull();
    expect(add?.newerLine).toBe(4);
    expect(add?.olderLine).toBeNull();
  });

  test("falls back to a message beyond the size cap", () => {
    const big = Array.from({ length: 50 }, (_, i) => `+line ${i}`).join("\n");
    const other = Array.from({ length: 50 }, (_, i) => `+other ${i}`).join("\n");
    const result = diffPatches(big, other, 100);
    expect("tooLarge" in result).toBe(true);
    if ("tooLarge" in result) {
      expect(result.tooLarge).toContain("GitHub");
    }
  });
});

describe("collapseUnchanged", () => {
  test("keeps changes with context and turns the rest into gap markers", () => {
    const lines: DiffLine[] = Array.from({ length: 20 }, (_, i): DiffLine => ({
      op: "same",
      text: ` ${i}`,
      olderLine: i + 1,
      newerLine: i + 1,
    }));
    lines[10] = { op: "add", text: "+x", olderLine: null, newerLine: 11 };
    const blocks = collapseUnchanged(lines, 2);
    expect(blocks.map((block) => block.kind)).toEqual(["gap", "lines", "gap"]);
    const gapHidden = blocks.flatMap((block) => (block.kind === "gap" ? [block.hidden] : []));
    expect(gapHidden).toEqual([8, 7]);
    const kept = blocks.flatMap((block) => (block.kind === "lines" ? block.lines : []));
    expect(kept).toHaveLength(5);
  });
});

describe("patchOnly", () => {
  test("shows a patchset's own patch against the trunk base, sorted, with totals", () => {
    const result = patchOnly([
      file("b.go", null, { additions: 2, deletions: 0 }),
      file("a.go", OLD_HUNK, { additions: 3, deletions: 1 }),
    ]);
    expect(result.files.map((f) => f.filename)).toEqual(["a.go", "b.go"]);
    expect(result.additions).toBe(5);
    expect(result.deletions).toBe(1);
    expect(result.unavailable).toBe(1);
  });
});

describe("patchLines", () => {
  test("classifies hunk, added, removed, context, and meta lines", () => {
    expect(patchLines("@@ -1 +1 @@\n-a\n+b\n c\n\\ No newline").map((line) => line.kind)).toEqual([
      "hunk",
      "del",
      "add",
      "context",
      "meta",
    ]);
  });
});
