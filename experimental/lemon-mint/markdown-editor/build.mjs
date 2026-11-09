'use strict';

import { build } from "esbuild";
import { createHash } from "node:crypto";
import { mkdir, readdir, readFile, rm, writeFile } from "node:fs/promises";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const root = dirname(fileURLToPath(import.meta.url));
const outdir = join(root, "dist");

async function walk(dir) {
  const entries = await readdir(dir, { withFileTypes: true });
  const files = [];
  for (const e of entries) {
    const p = join(dir, e.name);
    if (e.isDirectory()) files.push(...(await walk(p)));
    else if (e.isFile()) files.push(p);
  }
  return files;
}

async function inputDigest() {
  const rels = ["package.json", "package-lock.json", "build.mjs"];
  for (const abs of await walk(join(root, "src"))) {
    const rel = abs.slice(root.length + 1).replaceAll("\\", "/");
    if (!rel.endsWith(".test.js")) rels.push(rel);
  }
  rels.sort();
  const h = createHash("sha256");
  for (const rel of rels) {
    const data = (await readFile(join(root, rel))).filter((b) => b !== 0x0d);
    h.update(rel);
    h.update("\0");
    h.update(String(data.length));
    h.update("\0");
    h.update(data);
    h.update("\n");
  }
  return h.digest("hex");
}

await rm(outdir, { recursive: true, force: true });
await mkdir(outdir, { recursive: true });

const common = {
  absWorkingDir: root,
  bundle: true,
  minify: true,
  target: ["es2022"],
  legalComments: "none",
  logLevel: "warning",
};

const jsResult = await build({
  ...common,
  entryPoints: ["src/index.js"],
  outfile: "dist/index.js",
  format: "esm",
  metafile: true,
  define: {
    FLATS_MERMAID_URL: JSON.stringify("./mermaid-asset.js"),
  },
});

const mermaidResult = await build({
  ...common,
  entryPoints: ["node_modules/mermaid/dist/mermaid.esm.min.mjs"],
  outfile: "dist/mermaid-asset.js",
  format: "esm",
  metafile: true,
});

await build({
  ...common,
  entryPoints: ["src/editor.css"],
  outfile: "dist/index.css",
});

// Collect third-party package licenses from every package esbuild pulled in.
const pkgDirs = new Set();
for (const meta of [jsResult.metafile, mermaidResult.metafile]) {
  for (const input of Object.keys(meta.inputs)) {
    const m = input.match(
      /^node_modules\/((?:@[^/]+\/[^/]+)|(?:[^/]+))\//,
    );
    if (m) pkgDirs.add(m[1]);
  }
}

const sections = [];
for (const name of [...pkgDirs].sort()) {
  const dir = join(root, "node_modules", name);
  const pkg = JSON.parse(await readFile(join(dir, "package.json"), "utf8"));
  let text = "";
  for (const cand of [
    "LICENSE",
    "LICENSE.md",
    "LICENSE.txt",
    "LICENCE",
    "license",
  ]) {
    try {
      text = await readFile(join(dir, cand), "utf8");
      break;
    } catch {
      // try next
    }
  }
  sections.push(
    `================================================================================\n` +
      `${pkg.name}@${pkg.version} (${pkg.license || "UNKNOWN"})\n` +
      `================================================================================\n` +
      `${text.trim()}\n`,
  );
}

await writeFile(
  join(outdir, "THIRD_PARTY_LICENSES.txt"),
  sections.join("\n"),
  "utf8",
);
await writeFile(
  join(outdir, "BUILD-INPUTS.sha256"),
  (await inputDigest()) + "\n",
  "utf8",
);
