import { cpSync, mkdirSync } from "node:fs";

mkdirSync("public", { recursive: true });

const appBuild = await Bun.build({
  entrypoints: ["./src/app.ts"],
  outdir: "./public",
  target: "browser",
  minify: true,
});

if (!appBuild.success) {
  console.error("Failed to build src/app.ts:", appBuild.logs);
  process.exit(1);
}

const workerBuild = await Bun.build({
  entrypoints: ["./src/worker.ts"],
  outdir: "./public",
  naming: "_worker.js",
  target: "browser",
  format: "esm",
  minify: false,
});

if (!workerBuild.success) {
  console.error("Failed to build src/worker.ts:", workerBuild.logs);
  process.exit(1);
}

console.log("Built public/app.js and public/_worker.js");
