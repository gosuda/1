'use strict';

import { test } from "node:test";
import assert from "node:assert/strict";
import { EditorState } from "@codemirror/state";
import * as Y from "yjs";
import {
  defaultResolveURL,
  createMarkdownExtensions,
  createMarkdownEditor,
  editorMarkdown,
  createMarkdown,
  linkRules,
  liveDecorations,
  livePreview,
  configureDiagrams,
  RemoteBatcher,
  remoteCursors,
  caretLabel,
  createWheelPan,
} from "./index.js";

test("library entrypoint re-exports all core Markdown editor primitives", () => {
  assert.equal(typeof createMarkdownEditor, "function");
  assert.equal(typeof createMarkdownExtensions, "function");
  assert.equal(typeof defaultResolveURL, "function");
  assert.equal(typeof editorMarkdown, "function");
  assert.equal(typeof createMarkdown, "function");
  assert.equal(typeof linkRules, "function");
  assert.equal(typeof liveDecorations, "function");
  assert.equal(typeof livePreview, "function");
  assert.equal(typeof configureDiagrams, "function");
  assert.equal(typeof RemoteBatcher, "function");
  assert.equal(typeof remoteCursors, "function");
  assert.equal(typeof caretLabel, "function");
  assert.equal(typeof createWheelPan, "function");
});

test("defaultResolveURL resolves relative links against base URL and preserves invalid inputs", () => {
  assert.equal(
    defaultResolveURL("./guide.md", "https://gosuda.org/docs/index.md"),
    "https://gosuda.org/docs/guide.md",
  );
  assert.equal(
    defaultResolveURL("https://example.com/a", "https://gosuda.org/docs/"),
    "https://example.com/a",
  );
  assert.equal(defaultResolveURL("./local.md", ""), "./local.md");
});

test("createMarkdownExtensions produces a working CodeMirror 6 EditorState for standalone and Yjs docs", () => {
  const { extensions, markdown, links } = createMarkdownExtensions({
    baseURL: "https://gosuda.org/docs/readme.md",
    readOnly: false,
  });
  assert.ok(Array.isArray(extensions));
  const state = EditorState.create({
    doc: "# Heading\n\nSee [Guide](./guide.md) and **bold** text.\n",
    extensions,
  });
  assert.equal(state.doc.lines, 4);
  assert.equal(state.readOnly, false);

  const decos = liveDecorations(
    state,
    [{ from: 0, to: state.doc.length }],
    [],
    links,
  );
  assert.ok(decos.some((d) => d.kind === "line" && d.cls === "cm-h1"));
  assert.ok(decos.some((d) => d.kind === "mark" && d.cls === "cm-strong"));
  assert.ok(
    decos.some(
      (d) =>
        d.kind === "mark" &&
        d.cls === "cm-link" &&
        d.href === "./guide.md",
    ),
  );

  // Collaborative Yjs configuration also initializes cleanly.
  const ydoc = new Y.Doc();
  const ytext = ydoc.getText("markdown");
  ytext.insert(0, "Hello Yjs");
  const collab = createMarkdownExtensions({
    ytext,
    readOnly: true,
  });
  const ystate = EditorState.create({
    doc: ytext.toString(),
    extensions: collab.extensions,
  });
  assert.equal(ystate.readOnly, true);
});

test("createMarkdown renders safe HTML, rejects unsafe link schemes, and emits Mermaid placeholders", () => {
  const md = createMarkdown();
  const links = linkRules(md);
  const html = md.render(
    "# Title\n\n<script>alert(1)</script>\n\n[Sub](./sub.md)\n\n[Bad](javascript:alert(1))\n\n```mermaid\ngraph TD; A-->B\n```\n",
  );
  assert.ok(html.includes("<h1>Title</h1>"));
  assert.ok(!html.includes("<script>"));
  assert.ok(html.includes("&lt;script&gt;alert(1)&lt;/script&gt;"));
  assert.ok(html.includes('href="./sub.md"'));
  assert.ok(!html.includes('href="javascript:'));
  assert.equal(links.destination("./sub.md"), "./sub.md");
  assert.equal(links.destination("javascript:alert(1)"), "");
  assert.ok(html.includes('class="mermaid-diagram"'));
});
