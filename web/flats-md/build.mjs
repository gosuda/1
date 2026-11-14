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
  const rels = ["LICENSE", "package.json", "package-lock.json", "build.mjs"];
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
  platform: "browser",
  metafile: true,
  define: {
    FLATS_MERMAID_URL: JSON.stringify("./mermaid-asset.js"),
  },
});

const mermaidResult = await build({
  ...common,
  stdin: {
    contents: 'export { default } from "mermaid";',
    resolveDir: root,
  },
  outfile: "dist/mermaid-asset.js",
  format: "esm",
  platform: "browser",
  metafile: true,
});

await build({
  ...common,
  entryPoints: ["src/editor.css"],
  outfile: "dist/index.css",
});

// Collect third-party package licenses from every package esbuild bundled into
// dist/index.js and dist/mermaid-asset.js.
const packages = new Set();
for (const meta of [jsResult.metafile, mermaidResult.metafile]) {
  for (const input of Object.keys(meta.inputs)) {
    const m = input.match(/^node_modules\/((?:@[^/]+\/)?[^/]+)\//);
    if (m) packages.add(m[1]);
  }
}

let licenses =
  "Third-party software bundled into @gosuda/flats-md (gosuda.org/flats-md)\n\n";
for (const name of [...packages].sort()) {
  const dir = join(root, "node_modules", name);
  const pkg = JSON.parse(await readFile(join(dir, "package.json"), "utf8"));
  const files = (await readdir(dir)).filter((f) =>
    /^(license|licence|copying)([.-]|$)/i.test(f),
  );
  // A few packages (such as fastdom) carry their license as a README section.
  let readmeLicense = "";
  if (!files.length) {
    const readme = await readFile(join(dir, "README.md"), "utf8").catch(
      () => "",
    );
    const m = readme.match(/^##+ *Licen[cs]e *\n([\s\S]*?)(?=^#|(?![\s\S]))/m);
    if (!m || !/permission is hereby granted/i.test(m[1])) {
      throw new Error("Missing license text for " + name);
    }
    readmeLicense = m[1].trim() + "\n";
  }
  licenses += `===== ${name} ${pkg.version} (${pkg.license || "UNKNOWN"}) =====\n`;
  for (const f of files.sort()) {
    licenses += (await readFile(join(dir, f), "utf8")).trim() + "\n";
  }
  if (readmeLicense) licenses += readmeLicense;
  licenses += "\n";
}

await writeFile(join(outdir, "THIRD_PARTY_LICENSES.txt"), licenses, "utf8");
await writeFile(
  join(outdir, "BUILD-INPUTS.sha256"),
  (await inputDigest()) + "\n",
  "utf8",
);
