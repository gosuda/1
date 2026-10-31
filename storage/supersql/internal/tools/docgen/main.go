// Command docgen renders the reference documentation of this repository into
// docs/reference (plus the README quick-start region) directly from the sources.
//
// It is deliberately stdlib-only: every document is derived from files on disk
// with go/parser, go/ast and go/doc, so the tool builds with the module's own
// toolchain and never needs type information or a `go list` subprocess. The one
// in-module import is the pinned builtin catalog that functions.md documents.
//
// Run `go run ./internal/tools/docgen` to refresh the output and
// `go run ./internal/tools/docgen -check` to fail on drift or on undocumented
// exported root symbols.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "docgen: "+err.Error())
		os.Exit(1)
	}
}

// document is one generated file under the output directory.
type document struct {
	name   string
	render func(*generator) ([]byte, error)
}

// documents lists the generated files in the order they are written and checked.
func documents() []document {
	return []document{
		{name: "api.md", render: renderAPI},
		{name: "sql-compatibility.md", render: renderCompatibility},
		{name: "functions.md", render: renderFunctions},
		{name: "sqlstates.md", render: renderSQLStates},
		{name: "configuration.md", render: renderConfiguration},
		{name: "storage-format.md", render: renderStorageFormat},
	}
}

func run(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("docgen", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	root := flags.String("root", "", "module root; defaults to the nearest ancestor directory holding go.mod")
	out := flags.String("out", "docs/reference", "output directory, relative to the module root")
	check := flags.Bool("check", false, "render in memory and fail when the checked-in output differs")
	if err := flags.Parse(args); err != nil {
		return err
	}
	resolved, err := resolveRoot(*root)
	if err != nil {
		return err
	}
	output := *out
	if !filepath.IsAbs(output) {
		output = filepath.Join(resolved, output)
	}
	generator := newGenerator(resolved)
	if *check {
		return generator.check(output, stdout)
	}
	return generator.write(output, *out, stdout)
}

// resolveRoot returns the absolute module root: the requested directory, or the
// nearest ancestor of the working directory that holds go.mod.
func resolveRoot(root string) (string, error) {
	if root != "" {
		absolute, err := filepath.Abs(root)
		if err != nil {
			return "", err
		}
		if _, err := os.Stat(filepath.Join(absolute, "go.mod")); err != nil {
			return "", fmt.Errorf("root %s has no go.mod: %w", root, err)
		}
		return absolute, nil
	}
	directory, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(directory, "go.mod")); err == nil {
			return directory, nil
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return "", errors.New("no go.mod found in the working directory or its ancestors")
		}
		directory = parent
	}
}

// generator parses repository sources once and shares them across renderers.
type generator struct {
	root         string
	fset         *token.FileSet
	parsed       map[string]*ast.File
	undocumented map[string]bool
	// renderedExamples records the examples already shown with a symbol so the
	// example list stays complete without repeating code.
	renderedExamples map[string]bool
	notes            []string
}

func newGenerator(root string) *generator {
	return &generator{
		root:             root,
		fset:             token.NewFileSet(),
		parsed:           map[string]*ast.File{},
		undocumented:     map[string]bool{},
		renderedExamples: map[string]bool{},
	}
}

// path turns a slash-separated root-relative path into an absolute path.
func (g *generator) path(rel string) string {
	return filepath.Join(g.root, filepath.FromSlash(rel))
}

// parse returns the parsed AST of a root-relative Go file, parsing it at most
// once per run so that repeated scans stay cheap and deterministic.
func (g *generator) parse(rel string) (*ast.File, error) {
	if file, ok := g.parsed[rel]; ok {
		return file, nil
	}
	file, err := parser.ParseFile(g.fset, g.path(rel), nil, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	g.parsed[rel] = file
	return file, nil
}

// parseIsolated parses a root-relative file without caching it. go/doc rewrites
// the AST it is given even under PreserveAST, so the documentation renderer
// takes a private copy and the other renderers keep the cached pristine tree.
func (g *generator) parseIsolated(rel string) (*ast.File, error) {
	return parser.ParseFile(g.fset, g.path(rel), nil, parser.ParseComments)
}

// position renders a source location as root-relative file:line, never as an
// absolute path, so generated documents are machine independent.
func (g *generator) position(pos token.Pos) string {
	location := g.fset.Position(pos)
	rel, err := filepath.Rel(g.root, location.Filename)
	if err != nil {
		return fmt.Sprintf("%s:%d", location.Filename, location.Line)
	}
	return fmt.Sprintf("%s:%d", filepath.ToSlash(rel), location.Line)
}

// note records one summary line that `run` prints for the operator.
func (g *generator) note(format string, args ...any) {
	g.notes = append(g.notes, fmt.Sprintf(format, args...))
}

// noteUndocumented records an exported symbol that lacks a doc comment. It is
// reported by -check, never silently dropped.
func (g *generator) noteUndocumented(symbol string) {
	g.undocumented[symbol] = true
}

func (g *generator) undocumentedSymbols() []string {
	symbols := make([]string, 0, len(g.undocumented))
	for symbol := range g.undocumented {
		symbols = append(symbols, symbol)
	}
	sort.Strings(symbols)
	return symbols
}

// dirFiles lists root-relative Go file names of one directory in lexical order.
func (g *generator) dirFiles(dir string, includeTests bool) ([]string, error) {
	entries, err := os.ReadDir(g.path(dir))
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		if !includeTests && strings.HasSuffix(name, "_test.go") {
			continue
		}
		if dir == "" {
			names = append(names, name)
			continue
		}
		names = append(names, dir+"/"+name)
	}
	sort.Strings(names)
	return names, nil
}

// walkSources visits every non-test Go file of the module outside testdata/ and
// hidden directories, in lexical order.
func (g *generator) walkSources(visit func(rel string, file *ast.File) error) error {
	return filepath.WalkDir(g.root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := entry.Name()
		if entry.IsDir() {
			if path == g.root {
				return nil
			}
			if name == "testdata" || strings.HasPrefix(name, ".") {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(g.root, path)
		if err != nil {
			return err
		}
		file, err := g.parse(filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		return visit(filepath.ToSlash(rel), file)
	})
}

// modulePath reads the module path from go.mod.
func (g *generator) modulePath() string {
	data, err := os.ReadFile(g.path("go.mod"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "module" {
			return fields[1]
		}
	}
	return ""
}

// print renders an AST node with the canonical Go formatting.
func (g *generator) print(node any) string {
	if node == nil {
		return ""
	}
	var buffer bytes.Buffer
	if err := printer.Fprint(&buffer, g.fset, node); err != nil {
		return fmt.Sprintf("/* unprintable %T: %v */", node, err)
	}
	return buffer.String()
}

func (g *generator) check(out string, stdout io.Writer) error {
	var drift []string
	for _, document := range documents() {
		rendered, err := document.render(g)
		if err != nil {
			return err
		}
		path := filepath.Join(out, document.name)
		existing, err := os.ReadFile(path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			drift = append(drift, document.name+": missing")
			continue
		case err != nil:
			return err
		}
		if !bytes.Equal(existing, rendered) {
			drift = append(drift, document.name+": content differs")
		}
	}
	readme, ok, err := renderReadme(g)
	if err != nil {
		return err
	}
	switch {
	case !ok:
		fmt.Fprintln(stdout, "README.md example region skipped: markers or example_readme_test.go missing")
	default:
		existing, err := os.ReadFile(g.path("README.md"))
		if err != nil {
			return err
		}
		if !bytes.Equal(existing, readme) {
			drift = append(drift, "README.md: example region differs")
		}
	}
	for _, finding := range drift {
		fmt.Fprintln(stdout, "docgen drift: "+finding)
	}
	undocumented := g.undocumentedSymbols()
	for _, symbol := range undocumented {
		fmt.Fprintln(stdout, "docgen undocumented: "+symbol)
	}
	if len(drift) > 0 || len(undocumented) > 0 {
		return fmt.Errorf("%d drifted files, %d undocumented exported symbols", len(drift), len(undocumented))
	}
	return nil
}

// write renders every document into out and prints the summary lines. display
// is the output directory as the caller wrote it, used only for reporting.
func (g *generator) write(out, display string, stdout io.Writer) error {
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	for _, document := range documents() {
		rendered, err := document.render(g)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(out, document.name), rendered, 0o644); err != nil {
			return err
		}
		g.note("wrote %s (%d bytes)", filepath.ToSlash(filepath.Join(display, document.name)), len(rendered))
	}
	readme, ok, err := renderReadme(g)
	if err != nil {
		return err
	}
	if !ok {
		g.note("README.md example region skipped: markers or example_readme_test.go missing")
	} else {
		if err := os.WriteFile(g.path("README.md"), readme, 0o644); err != nil {
			return err
		}
		g.note("updated README.md example region (%d bytes)", len(readme))
	}
	for _, note := range g.notes {
		fmt.Fprintln(stdout, note)
	}
	return nil
}
