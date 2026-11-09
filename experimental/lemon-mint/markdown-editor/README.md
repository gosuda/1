# `@gosuda/markdown-editor` (`gosuda.org/experimental/lemon-mint/markdown-editor`)

A reusable single-surface live-preview Markdown editor extracted from Flats (`internal/apps/docs/_web`).

## Features

- **Single-surface live-preview editing** (`live.js`, `live-view.js`): Built on CodeMirror 6 (`@codemirror/view`, `@codemirror/state`, `@codemirror/lang-markdown`, `@lezer/markdown`). Markdown delimiters (`#`, `**`, `*`, `~~`, `` ` ``, `[text](url)`, `[ref]: url`, `>`, `-`, `---`, `![alt](src)`, GFM tables, and ` ```mermaid ` blocks) render inline and only reveal their raw syntax on the lines touched by the active cursor or selection while the editor is focused.
- **Linear-time Markdown inline parser** (`markdown-inline.js`): Replaces quadratic Lezer GFM inline scanners (`LinkEnd`, `Emphasis`, link destination/title scans, and unclosed inline HTML tags) with linear-time parsers and skips inline parsing on paragraphs longer than 20,000 characters.
- **Safe Markdown rendering** (`markdown.js`): Shared `markdown-it` configuration with raw HTML disabled, relative link resolution, and Mermaid diagram placeholders.
- **Interactive Mermaid diagrams** (`diagrams.js`, `diagram-preview.js`, `diagram-runtime.js`, `diagram-tools.js`, `diagram-export.js`, `wheel.js`): Lazy viewport rendering, theme-aware caching, on-request drawing mode for untrusted/large diagrams, SVG/PNG export, and an expanded pan/zoom viewer dialog.
- **Optional Yjs collaboration** (`remote-cursors.js`, `remote-batch.js`, `render-budget.js`): Optional `y-codemirror.next` binding, awareness remote carets with guest badges, adaptive remote update batching, and coalesced remote decoration refreshes.
- **Dual JS/ESM + Go embed packaging**: Use directly as an ES module (`src/index.js` or pre-bundled `dist/index.js` + `dist/index.css`) or serve from Go via `gosuda.org/experimental/lemon-mint/markdown-editor` (`FS()` and `Hash()`).

## Usage (JavaScript / ESM)

### High-level Widget (`createMarkdownEditor`)

```js
import { createMarkdownEditor } from "@gosuda/markdown-editor";
import "@gosuda/markdown-editor/style.css";

const editor = createMarkdownEditor(document.getElementById("app"), {
  value: "# Hello Markdown\n\nWrite with **live preview**.\n",
  mode: "edit", // "edit" | "view"
  readOnly: false,
  mermaidURL: "/assets/mermaid-asset.js",
  onChange(value) {
    console.log("Updated markdown:", value);
  },
});

// Switch between live-preview editing and rendered view:
editor.setMode("view");
editor.setMode("edit");
```

### Composable CodeMirror 6 Extensions (`createMarkdownExtensions`)

```js
import { EditorState } from "@codemirror/state";
import { EditorView } from "@codemirror/view";
import { createMarkdownExtensions } from "@gosuda/markdown-editor";

const { extensions } = createMarkdownExtensions({
  baseURL: window.location.href,
});

const view = new EditorView({
  state: EditorState.create({ doc: "# Custom EditorView\n", extensions }),
  parent: document.querySelector("#editor"),
});
```

## Usage (Go Embed)

```go
import (
	"net/http"

	markdowneditor "gosuda.org/experimental/lemon-mint/markdown-editor"
)

func registerAssets(mux *http.ServeMux) {
	mux.Handle("/assets/md/", http.StripPrefix("/assets/md/", http.FileServer(http.FS(markdowneditor.FS()))))
}
```

## Development

```bash
npm ci
npm run test:unit
npm run build
go test ./...
```
