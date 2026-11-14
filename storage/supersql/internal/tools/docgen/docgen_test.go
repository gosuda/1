package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// update rewrites the golden files under testdata/golden.
var update = flag.Bool("update", false, "rewrite the golden documents under testdata/golden")

const (
	fixtureRoot = "testdata/fixture"
	goldenRoot  = "testdata/golden"
)

// fixtureGenerator returns a generator rooted at the fixture module.
func fixtureGenerator(t *testing.T) *generator {
	t.Helper()
	root, err := filepath.Abs(fixtureRoot)
	if err != nil {
		t.Fatalf("fixture root: %v", err)
	}
	return newGenerator(root)
}

// repositoryGenerator returns a generator rooted at the module under test.
func repositoryGenerator(t *testing.T) *generator {
	t.Helper()
	root, err := resolveRoot(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("repository root: %v", err)
	}
	return newGenerator(root)
}

// TestRenderFixtureDocuments pins every renderer that can run over the fixture
// module. functions.md is absent here because it reads the compiled builtin
// catalog of the real module; TestWriteIsDeterministic covers it.
func TestRenderFixtureDocuments(t *testing.T) {
	g := fixtureGenerator(t)
	documents := map[string]func(*generator) ([]byte, error){
		"api.md":               renderAPI,
		"sql-compatibility.md": renderCompatibility,
		"sqlstates.md":         renderSQLStates,
		"configuration.md":     renderConfiguration,
		"storage-format.md":    renderStorageFormat,
	}
	for name, render := range documents {
		rendered, err := render(g)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		compareGolden(t, filepath.Join(goldenRoot, name), rendered)
	}
	readme, ok, err := renderReadme(g)
	if err != nil {
		t.Fatalf("readme: %v", err)
	}
	if !ok {
		t.Fatal("readme: example region not synchronized")
	}
	compareGolden(t, filepath.Join(goldenRoot, "readme.md"), readme)
}

// compareGolden writes the golden file under -update and compares otherwise.
func compareGolden(t *testing.T, path string, rendered []byte) {
	t.Helper()
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("golden directory: %v", err)
		}
		if err := os.WriteFile(path, rendered, 0o644); err != nil {
			t.Fatalf("golden %s: %v", path, err)
		}
		return
	}
	expected, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("golden %s: %v (run go test -run TestRenderFixtureDocuments -update ./internal/tools/docgen)", path, err)
	}
	if !bytes.Equal(expected, rendered) {
		t.Fatalf("%s differs from its golden file (run go test -run TestRenderFixtureDocuments -update ./internal/tools/docgen)\n%s", path, firstDifference(expected, rendered))
	}
}

// firstDifference reports the first differing line so a golden mismatch is
// actionable without dumping both documents.
func firstDifference(expected, actual []byte) string {
	expectedLines := strings.Split(string(expected), "\n")
	actualLines := strings.Split(string(actual), "\n")
	for index := range max(len(expectedLines), len(actualLines)) {
		var want, got string
		if index < len(expectedLines) {
			want = expectedLines[index]
		}
		if index < len(actualLines) {
			got = actualLines[index]
		}
		if want != got {
			return fmt.Sprintf("line %d:\n  want: %s\n  got:  %s", index+1, want, got)
		}
	}
	return "documents differ only in length"
}

// TestWriteIsDeterministic renders every document twice from the real module and
// requires byte-identical output, which is the property the checked-in
// docs/reference depends on.
func TestWriteIsDeterministic(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	for _, out := range []string{first, second} {
		if err := repositoryGenerator(t).write(out, out, io.Discard); err != nil {
			t.Fatalf("write %s: %v", out, err)
		}
	}
	for _, document := range documents() {
		one, err := os.ReadFile(filepath.Join(first, document.name))
		if err != nil {
			t.Fatalf("read %s: %v", document.name, err)
		}
		two, err := os.ReadFile(filepath.Join(second, document.name))
		if err != nil {
			t.Fatalf("read %s: %v", document.name, err)
		}
		if !bytes.Equal(one, two) {
			t.Fatalf("%s is not deterministic", document.name)
		}
		if !bytes.HasPrefix(one, []byte(generatedHeader)) {
			t.Fatalf("%s does not begin with the generated marker", document.name)
		}
		if len(one) == 0 {
			t.Fatalf("%s is empty", document.name)
		}
	}
}

// TestCatalogCounts pins the catalog sizes the reference documents advertise.
func TestCatalogCounts(t *testing.T) {
	rendered, err := renderFunctions(repositoryGenerator(t))
	if err != nil {
		t.Fatalf("functions: %v", err)
	}
	for _, expected := range []string{
		fmt.Sprintf("| signatures | %d | %d |", expectedSignatures, expectedSignatures),
		fmt.Sprintf("| operators | %d | %d |", expectedOperators, expectedOperators),
		fmt.Sprintf("| casts | %d | %d |", expectedCasts, expectedCasts),
		fmt.Sprintf("| types | %d | %d |", expectedTypes, expectedTypes),
	} {
		if !bytes.Contains(rendered, []byte(expected)) {
			t.Fatalf("functions.md does not contain %q", expected)
		}
	}
}

// TestCheckCleanAndDrift proves that -check passes on a synchronized tree and
// fails once one byte of a document changes.
func TestCheckCleanAndDrift(t *testing.T) {
	root, out := copyFixture(t)
	if err := run([]string{"-root", root, "-out", out}, io.Discard); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := run([]string{"-root", root, "-out", out, "-check"}, io.Discard); err != nil {
		t.Fatalf("check on a synchronized tree: %v", err)
	}
	drifted := filepath.Join(out, "api.md")
	if err := os.WriteFile(drifted, []byte("mutated\n"), 0o644); err != nil {
		t.Fatalf("mutate: %v", err)
	}
	var output bytes.Buffer
	err := run([]string{"-root", root, "-out", out, "-check"}, &output)
	if err == nil {
		t.Fatal("check accepted a drifted document")
	}
	if !strings.Contains(output.String(), "api.md") {
		t.Fatalf("check did not name the drifted document: %s", output.String())
	}
}

// TestUndocumentedSymbolFails proves that -check refuses an exported symbol
// without a doc comment.
func TestUndocumentedSymbolFails(t *testing.T) {
	root, out := copyFixture(t)
	if err := run([]string{"-root", root, "-out", out}, io.Discard); err != nil {
		t.Fatalf("write: %v", err)
	}
	undocumented := "package fixture\n\nfunc UndocumentedFixture() {}\n"
	if err := os.WriteFile(filepath.Join(root, "undocumented.go"), []byte(undocumented), 0o644); err != nil {
		t.Fatalf("write undocumented file: %v", err)
	}
	var output bytes.Buffer
	err := run([]string{"-root", root, "-out", out, "-check"}, &output)
	if err == nil {
		t.Fatal("check accepted an undocumented exported symbol")
	}
	if !strings.Contains(output.String(), "UndocumentedFixture") {
		t.Fatalf("check did not name the undocumented symbol: %s", output.String())
	}
}

// TestReadmeRegionSkippedWithoutMarkers documents the graceful degradation: a
// README without the markers leaves the file untouched. The repository README
// carries the markers, so the case is pinned on a fixture copy with the markers
// removed.
func TestReadmeRegionSkippedWithoutMarkers(t *testing.T) {
	root, _ := copyFixture(t)
	path := filepath.Join(root, readmeName)
	text, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture README: %v", err)
	}
	stripped := strings.ReplaceAll(strings.ReplaceAll(string(text), readmeBegin, ""), readmeEnd, "")
	if err := os.WriteFile(path, []byte(stripped), 0o644); err != nil {
		t.Fatalf("strip markers: %v", err)
	}
	readme, ok, err := renderReadme(newGenerator(root))
	if err != nil {
		t.Fatalf("readme: %v", err)
	}
	if ok || readme != nil {
		t.Fatal("readme region was synchronized in a repository without markers")
	}
}

// TestExampleBodySkipsPackageClause pins the exact region the README shows.
func TestExampleBodySkipsPackageClause(t *testing.T) {
	source, err := os.ReadFile(filepath.Join(fixtureRoot, readmeExampleFile))
	if err != nil {
		t.Fatalf("example: %v", err)
	}
	body, ok := exampleBody(string(source))
	if !ok {
		t.Fatal("example body not found")
	}
	if strings.Contains(body, "package ") {
		t.Fatalf("example body still holds the package clause: %s", body)
	}
	if !strings.HasPrefix(body, "\nimport (") {
		t.Fatalf("example body does not start with the import block: %q", body)
	}
}

// copyFixture copies the fixture module into a temporary directory so a test can
// write into it without touching the checked-in tree. It returns the module root
// and an output directory beside it.
func copyFixture(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.CopyFS(root, os.DirFS(fixtureRoot)); err != nil {
		t.Fatalf("copy fixture: %v", err)
	}
	return root, filepath.Join(t.TempDir(), "reference")
}
