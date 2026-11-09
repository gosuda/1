// Package markdowneditor provides the embedded static bundle and assets of the
// reusable single-surface live-preview Markdown editor extracted from Flats.
package markdowneditor

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"sync"
)

//go:embed dist/index.js dist/index.css dist/mermaid-asset.js dist/THIRD_PARTY_LICENSES.txt dist/BUILD-INPUTS.sha256
var rawDist embed.FS

var (
	distOnce sync.Once
	distSub  fs.FS
	distHash string
)

func initDist() {
	distOnce.Do(func() {
		sub, err := fs.Sub(rawDist, "dist")
		if err != nil {
			panic("markdowneditor: missing embedded dist: " + err.Error())
		}
		distSub = sub
		h := sha256.New()
		for _, name := range []string{"index.js", "index.css", "mermaid-asset.js"} {
			b, err := fs.ReadFile(sub, name)
			if err != nil {
				panic("markdowneditor: missing embedded asset " + name + ": " + err.Error())
			}
			_, _ = h.Write(b)
		}
		distHash = hex.EncodeToString(h.Sum(nil))[:12]
	})
}

// FS returns the embedded dist/ filesystem containing index.js, index.css,
// mermaid-asset.js, THIRD_PARTY_LICENSES.txt, and BUILD-INPUTS.sha256.
func FS() fs.FS {
	initDist()
	return distSub
}

// Hash returns a 12-character hex digest of the embedded JS and CSS bundles
// suitable for cache-busting query strings or ETag headers.
func Hash() string {
	initDist()
	return distHash
}
