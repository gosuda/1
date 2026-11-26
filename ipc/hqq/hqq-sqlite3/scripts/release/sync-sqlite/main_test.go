package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"crypto/sha3"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseSQLiteVersion(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"3.54.1", 3540100},
		{"3.55.0", 3550000},
		{"3.54.1.2", 3540102},
		{"3.7.15", 3071500},
	}
	for _, tc := range cases {
		got, err := parseSQLiteVersion(tc.in)
		if err != nil {
			t.Fatalf("parseSQLiteVersion(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("parseSQLiteVersion(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestParseDownloadHTML(t *testing.T) {
	html := `<!-- Download product data for scripts to read
PRODUCT,VERSION,RELATIVE-URL,SIZE-IN-BYTES,SHA3-HASH
PRODUCT,3.55.0,2026/sqlite-src-3550000.zip,15000000,aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
PRODUCT,3.55.0,2026/sqlite-amalgamation-3550000.zip,2991234,92281c86e2957f9b9305890f9ac04204caa7b677cc868dc326d942689df28e30
-->`
	got, err := parseDownloadHTML([]byte(html))
	if err != nil {
		t.Fatalf("parseDownloadHTML: %v", err)
	}
	if got.Version != "3.55.0" || got.VersionNumber != 3550000 {
		t.Fatalf("got version %q (%d)", got.Version, got.VersionNumber)
	}
	if got.Archive != "sqlite-amalgamation-3550000.zip" {
		t.Fatalf("got archive %q", got.Archive)
	}
	if got.URL != "https://www.sqlite.org/2026/sqlite-amalgamation-3550000.zip" {
		t.Fatalf("got url %q", got.URL)
	}
	if got.Size != 2991234 {
		t.Fatalf("got size %d", got.Size)
	}
}

func TestVerifyAndApplyPinUpdates(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range map[string]string{
		"sqlite-amalgamation-3550000/sqlite3.c": "/* amalgamation */",
		"sqlite-amalgamation-3550000/sqlite3.h": "#define SQLITE_VERSION \"3.55.0\"\n#define SQLITE_VERSION_NUMBER 3550000\n",
	} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	zipData := buf.Bytes()
	sum3 := sha3.Sum256(zipData)
	sum256 := sha256.Sum256(zipData)

	up := upstreamRelease{
		Version:       "3.55.0",
		VersionNumber: 3550000,
		Archive:       "sqlite-amalgamation-3550000.zip",
		URL:           "https://www.sqlite.org/2026/sqlite-amalgamation-3550000.zip",
		Size:          int64(len(zipData)),
		SHA3_256:      hex.EncodeToString(sum3[:]),
	}
	gotSHA256, err := verifyAmalgamationZip(zipData, up)
	if err != nil {
		t.Fatalf("verifyAmalgamationZip: %v", err)
	}
	if gotSHA256 != hex.EncodeToString(sum256[:]) {
		t.Fatalf("sha256 mismatch: %s", gotSHA256)
	}

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "third_party", "sqlite"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "sqlite3"), 0o755); err != nil {
		t.Fatal(err)
	}
	hashesOrig := `package sqlite

// Version follows the v<WrapperMajor>.<WrapperMinor>.<NormalizedCVersion> scheme,
// where 3540100 corresponds to SQLITE_VERSION_NUMBER (SQLite 3.54.1).
const (
	Version             = "v0.3.3540100"
	SQLiteVersionNumber = 3540100
)
`
	if err := os.WriteFile(filepath.Join(root, "hashes.go"), []byte(hashesOrig), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("\tVersion             = \"v0.3.3540100\"\n\tSQLiteVersionNumber = 3540100\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sqlite3", "README.md"), []byte("Fetch the pinned SQLite amalgamation (`3.54.1`), build"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := applyPinUpdates(root, up, gotSHA256, "v0.3.3550000"); err != nil {
		t.Fatalf("applyPinUpdates: %v", err)
	}
	pin, err := readLocalPin(root)
	if err != nil {
		t.Fatalf("readLocalPin: %v", err)
	}
	if pin.Version != "3.55.0" || pin.VersionNumber != 3550000 || pin.WrapperPrefix != "v0.3." {
		t.Fatalf("unexpected pin after apply: %+v", pin)
	}
	if isUpdateAvailable(pin, up) {
		t.Fatal("expected isUpdateAvailable to be false after apply")
	}
	hb, _ := os.ReadFile(filepath.Join(root, "hashes.go"))
	if !strings.Contains(string(hb), `Version             = "v0.3.3550000"`) || !strings.Contains(string(hb), `SQLiteVersionNumber = 3550000`) {
		t.Fatalf("hashes.go not updated:\n%s", hb)
	}
}
