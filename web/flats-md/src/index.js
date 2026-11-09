'use strict';

import { EditorState, Compartment } from "@codemirror/state";
import { EditorView, keymap } from "@codemirror/view";
import {
  defaultKeymap,
  history,
  historyKeymap,
  indentWithTab,
} from "@codemirror/commands";
import {
  syntaxHighlighting,
  defaultHighlightStyle,
} from "@codemirror/language";
import * as Y from "yjs";
import { yCollab, yUndoManagerKeymap } from "y-codemirror.next";

import {
  editorMarkdown,
  linearInline,
  plainLongParagraphs,
  PLAIN_PARAGRAPH,
} from "./markdown-inline.js";
import { createMarkdown, isDiagram, linkRules } from "./markdown.js";
import {
  liveDecorations,
  activeLines,
  linkAt,
  tableBlocks,
  diagramBlocks,
  referenceDefinitions,
} from "./live.js";
import { livePreview } from "./live-view.js";
import {
  renderDiagrams,
  redrawDiagrams,
  updateDiagram,
  destroyDiagram,
  drawOnRequest,
  configureDiagrams,
} from "./diagrams.js";
import {
  diagramKeys,
  updatePreview,
  trackFocusExit,
  diagramClosures,
  closedDiagramCursor,
  setDiagramEditing,
  sourceAction,
} from "./diagram-preview.js";
import { createDiagramRuntime, tooLarge } from "./diagram-runtime.js";
import { diagramTools, uniqueSVG } from "./diagram-tools.js";
import { pngSize, pngBlob, snapshotSVG, saveBlob } from "./diagram-export.js";
import { createWheelPan } from "./wheel.js";
import { coalescedRefresh, REMOTE_RENDER_GAP_MS } from "./render-budget.js";
import { RemoteBatcher, renderedFrame, MAX_WAIT } from "./remote-batch.js";
import { remoteCursors, caretLabel } from "./remote-cursors.js";

export {
  editorMarkdown,
  linearInline,
  plainLongParagraphs,
  PLAIN_PARAGRAPH,
  createMarkdown,
  isDiagram,
  linkRules,
  liveDecorations,
  activeLines,
  linkAt,
  tableBlocks,
  diagramBlocks,
  referenceDefinitions,
  livePreview,
  renderDiagrams,
  redrawDiagrams,
  updateDiagram,
  destroyDiagram,
  drawOnRequest,
  configureDiagrams,
  diagramKeys,
  updatePreview,
  trackFocusExit,
  diagramClosures,
  closedDiagramCursor,
  setDiagramEditing,
  sourceAction,
  createDiagramRuntime,
  tooLarge,
  diagramTools,
  uniqueSVG,
  pngSize,
  pngBlob,
  snapshotSVG,
  saveBlob,
  createWheelPan,
  coalescedRefresh,
  REMOTE_RENDER_GAP_MS,
  RemoteBatcher,
  renderedFrame,
  MAX_WAIT,
  remoteCursors,
  caretLabel,
};

// defaultResolveURL resolves relative Markdown links and image URLs against the
// current page URL when `location.href` is available, or returns `url` as-is.
export function defaultResolveURL(url, baseHref) {
  const base =
    baseHref ||
    (typeof location !== "undefined" && location?.href ? location.href : "");
  if (!base) return String(url ?? "");
  try {
    return new URL(url, base).toString();
  } catch {
    return String(url ?? "");
  }
}

// createMarkdownExtensions builds the CodeMirror 6 extension array for the
// Flats live-preview Markdown editor. Callers that construct their own
// EditorState/EditorView can spread this array into `extensions`.
export function createMarkdownExtensions(options = {}) {
  const {
    resolveURL = (u) => defaultResolveURL(u, options.baseURL),
    markdown: customMarkdown,
    links: customLinks,
    onOpenLink,
    onChange,
    readOnly = false,
    readOnlyCompartment = new Compartment(),
    ytext = null,
    awareness = null,
    undoManager: customUndoManager,
    extensions: extraExtensions = [],
  } = options;

  const md = customMarkdown || createMarkdown();
  const links = customLinks || linkRules(md);
  const renderBlock = (src) => md.render(src);

  const collabExtensions = [];
  if (ytext) {
    const um =
      customUndoManager !== undefined
        ? customUndoManager
        : new Y.UndoManager(ytext);
    collabExtensions.push(yCollab(ytext, null, { undoManager: um }));
    if (awareness) {
      collabExtensions.push(remoteCursors(awareness));
    }
    collabExtensions.push(
      keymap.of([...defaultKeymap, ...yUndoManagerKeymap, indentWithTab]),
    );
  } else {
    collabExtensions.push(
      history(),
      keymap.of([...defaultKeymap, ...historyKeymap, indentWithTab]),
    );
  }

  const updateListeners = [];
  if (typeof onChange === "function") {
    updateListeners.push(
      EditorView.updateListener.of((update) => {
        if (update.docChanged) {
          onChange(update.state.doc.toString(), update);
        }
      }),
    );
  }

  const extensions = [
    syntaxHighlighting(defaultHighlightStyle, { fallback: true }),
    editorMarkdown(),
    livePreview(links, resolveURL, renderBlock, onOpenLink),
    EditorView.lineWrapping,
    ...collabExtensions,
    readOnlyCompartment.of([
      EditorState.readOnly.of(Boolean(readOnly)),
      EditorView.editable.of(!readOnly),
    ]),
    ...updateListeners,
    ...extraExtensions,
  ];

  return {
    extensions,
    markdown: md,
    links,
    readOnlyCompartment,
  };
}

// createMarkdownEditor mounts a complete single-surface Markdown live-preview
// editor and rendered view inside `parent`.
export function createMarkdownEditor(parent, options = {}) {
  if (!parent || typeof parent.appendChild !== "function") {
    throw new TypeError("createMarkdownEditor requires a parent DOM element");
  }

  if (options.mermaidURL || options.loadMermaid) {
    configureDiagrams({
      mermaidURL: options.mermaidURL,
      loadMermaid: options.loadMermaid,
    });
  }
  if (options.drawDiagramsOnRequest) {
    drawOnRequest();
  }

  const doc = parent.ownerDocument || globalThis.document;
  const root = doc.createElement("div");
  root.className = "surface flats-md-surface";

  const editorElement = doc.createElement("div");
  editorElement.className = "md-editor-mount";

  const contentElement = doc.createElement("article");
  contentElement.className = "md-content";
  contentElement.hidden = true;

  root.appendChild(editorElement);
  root.appendChild(contentElement);
  parent.appendChild(root);

  let currentMode = options.mode === "view" ? "view" : "edit";
  let currentReadOnly = Boolean(options.readOnly);
  let destroyed = false;

  const ytext = options.ytext || null;
  const initialText = ytext
    ? ytext.toString()
    : String(options.value ?? options.doc ?? "");

  const readOnlyCompartment = new Compartment();
  const { extensions, markdown, links } = createMarkdownExtensions({
    ...options,
    readOnly: currentReadOnly,
    readOnlyCompartment,
    onChange: (text, update) => {
      if (currentMode === "view") {
        refreshPreview();
      }
      if (typeof options.onChange === "function") {
        options.onChange(text, update);
      }
    },
  });

  const state = EditorState.create({
    doc: initialText,
    extensions,
  });

  const view = new EditorView({
    state,
    parent: editorElement,
  });

  function renderHTML(source) {
    return markdown.render(String(source ?? ""));
  }

  function refreshPreview() {
    if (destroyed) return;
    contentElement.innerHTML = renderHTML(getValue());
    renderDiagrams(contentElement);
  }

  function getValue() {
    if (ytext) return ytext.toString();
    return view.state.doc.toString();
  }

  function setValue(nextValue) {
    const text = String(nextValue ?? "");
    if (ytext && ytext.doc) {
      ytext.doc.transact(() => {
        ytext.delete(0, ytext.length);
        if (text) ytext.insert(0, text);
      });
    } else {
      view.dispatch({
        changes: { from: 0, to: view.state.doc.length, insert: text },
      });
    }
    if (currentMode === "view") {
      refreshPreview();
    }
  }

  function getMode() {
    return currentMode;
  }

  function setMode(nextMode) {
    const mode = nextMode === "view" ? "view" : "edit";
    currentMode = mode;
    editorElement.hidden = mode !== "edit";
    contentElement.hidden = mode !== "view";
    if (mode === "view") {
      refreshPreview();
    } else {
      renderDiagrams(editorElement);
      view.requestMeasure();
    }
  }

  function isReadOnly() {
    return currentReadOnly;
  }

  function setReadOnly(nextReadOnly) {
    currentReadOnly = Boolean(nextReadOnly);
    view.dispatch({
      effects: readOnlyCompartment.reconfigure([
        EditorState.readOnly.of(currentReadOnly),
        EditorView.editable.of(!currentReadOnly),
      ]),
    });
  }

  function focus() {
    view.focus();
  }

  function destroy() {
    if (destroyed) return;
    destroyed = true;
    view.destroy();
    root.remove();
  }

  setMode(currentMode);

  return {
    view,
    markdown,
    links,
    root,
    editorElement,
    contentElement,
    getValue,
    setValue,
    getMode,
    setMode,
    isReadOnly,
    setReadOnly,
    renderHTML,
    refreshPreview,
    focus,
    destroy,
  };
}
