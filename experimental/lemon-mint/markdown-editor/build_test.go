package markdowneditor

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestEmbeddedBundleAndBuildInputsUpToDate(t *testing.T) {
	bundleFS := FS()
	if h := Hash(); len(h) != 12 {
		t.Fatalf("Hash() = %q, want 12-char hex digest", h)
	}
	for _, asset := range []string{
		"index.js",
		"index.css",
		"mermaid-asset.js",
		"THIRD_PARTY_LICENSES.txt",
		"BUILD-INPUTS.sha256",
	} {
		data, err := fs.ReadFile(bundleFS, asset)
		if err != nil {
			t.Fatalf("read embedded %s: %v", asset, err)
		}
		if len(data) == 0 {
			t.Fatalf("embedded %s is empty", asset)
		}
	}

	var rels []string
	for _, top := range []string{"package.json", "package-lock.json", "build.mjs"} {
		rels = append(rels, top)
	}
	err := filepath.WalkDir("src", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel := filepath.ToSlash(p)
		if !strings.HasSuffix(rel, ".test.js") {
			rels = append(rels, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk src: %v", err)
	}
	sort.Strings(rels)

	h := sha256.New()
	for _, rel := range rels {
		data, err := os.ReadFile(filepath.FromSlash(rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		data = bytes.ReplaceAll(data, []byte{'\r'}, nil)
		fmt.Fprintf(h, "%s\x00%d\x00", rel, len(data))
		h.Write(data)
		h.Write([]byte{'\n'})
	}
	want := hex.EncodeToString(h.Sum(nil))

	gotBytes, err := fs.ReadFile(bundleFS, "BUILD-INPUTS.sha256")
	if err != nil {
		t.Fatalf("read embedded BUILD-INPUTS.sha256: %v", err)
	}
	got := strings.TrimSpace(string(gotBytes))
	if got != want {
		t.Fatalf("dist/ is out of date with src/: BUILD-INPUTS.sha256 has %s, current sources hash to %s; run `npm run build`", got, want)
	}
}
