package main

import (
	"errors"
	"fmt"
	"go/ast"
	"go/doc"
	"io/fs"
	"os"
	"path"
	"strings"
)

// publicSubpackages lists the optional public subpackages documented in api.md
// after the root package, in presentation order. Subpackages absent from the
// module root (such as in the minimal fixture module) are skipped cleanly.
var publicSubpackages = []string{
	"pgx",
	"pgwire",
	"clock",
	"clock/local",
	"clock/gstime",
}

// renderAPI builds api.md: the exported surface of the root package (and any
// present public subpackages) as go/doc sees it, plus the examples that carry
// a `// Output:` comment.
//
// It also records every exported symbol without a doc comment in
// g.undocumented so that -check fails until the symbol is documented.
func renderAPI(g *generator) ([]byte, error) {
	pkg, err := loadDocPackage(g, "", g.modulePath())
	if err != nil {
		return nil, fmt.Errorf("documenting the root package: %w", err)
	}
	if pkg.Doc == "" {
		g.noteUndocumented("package " + pkg.Name)
	}
	writer := newMarkdownWriter()
	renderedExamples := map[string]bool{}
	writer.heading(1, "`"+pkg.Name+"` package reference")
	writer.paragraph(fmt.Sprintf("Import path `%s`.", g.modulePath()))
	if pkg.Doc != "" {
		writer.heading(2, "Package documentation")
		writer.paragraph(pkg.Doc)
	}
	writeValues(g, writer, "", "Constants", pkg.Consts, 2)
	writeValues(g, writer, "", "Variables", pkg.Vars, 2)
	if len(pkg.Funcs) > 0 {
		writer.heading(2, "Functions")
		for _, function := range pkg.Funcs {
			writeFunc(g, writer, "", renderedExamples, function, 3)
		}
	}
	writeTypes(g, writer, "", renderedExamples, pkg, 2)
	writeExamples(g, writer, renderedExamples, pkg)
	g.note("api: %d consts, %d vars, %d funcs, %d types", len(pkg.Consts), len(pkg.Vars), len(pkg.Funcs), len(pkg.Types))

	for _, subpkg := range publicSubpackages {
		if err := renderSubpackageAPI(g, writer, subpkg); err != nil {
			return nil, err
		}
	}
	return writer.bytes(), nil
}

// loadDocPackage parses all Go files in relDir and builds a go/doc.Package.
func loadDocPackage(g *generator, relDir, importPath string) (*doc.Package, error) {
	files, err := g.dirFiles(relDir, true)
	if err != nil {
		return nil, err
	}
	asts := make([]*ast.File, 0, len(files))
	for _, rel := range files {
		file, err := g.parseIsolated(rel)
		if err != nil {
			return nil, err
		}
		asts = append(asts, file)
	}
	// PreserveAST keeps the parsed files reusable by the other renderers.
	return doc.NewFromFiles(g.fset, asts, importPath, doc.PreserveAST)
}

// renderSubpackageAPI appends one public subpackage section when its directory
// exists under g.root. Missing directories are skipped so minimal fixture
// modules remain self-contained.
func renderSubpackageAPI(g *generator, writer *mdWriter, relDir string) error {
	info, err := os.Stat(g.path(relDir))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	if !info.IsDir() {
		return nil
	}
	files, err := g.dirFiles(relDir, true)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return nil
	}
	importPath := g.modulePath() + "/" + relDir
	pkg, err := loadDocPackage(g, relDir, importPath)
	if err != nil {
		return fmt.Errorf("documenting package %s: %w", relDir, err)
	}
	if pkg.Doc == "" {
		g.noteUndocumented(qualifySymbol(relDir, "package "+pkg.Name))
	}
	renderedExamples := map[string]bool{}
	writer.heading(2, "`"+relDir+"` package reference")
	if pkg.Name != path.Base(relDir) {
		writer.paragraph(fmt.Sprintf("Import path `%s` (package `%s`).", importPath, pkg.Name))
	} else {
		writer.paragraph(fmt.Sprintf("Import path `%s`.", importPath))
	}
	if pkg.Doc != "" {
		writer.heading(3, "Package documentation")
		writer.paragraph(pkg.Doc)
	}
	writeValues(g, writer, relDir, "Constants", pkg.Consts, 3)
	writeValues(g, writer, relDir, "Variables", pkg.Vars, 3)
	if len(pkg.Funcs) > 0 {
		writer.heading(3, "Functions")
		for _, function := range pkg.Funcs {
			writeFunc(g, writer, relDir, renderedExamples, function, 4)
		}
	}
	writeTypes(g, writer, relDir, renderedExamples, pkg, 3)
	writeSubpackageExamples(g, writer, renderedExamples, pkg, 3)
	g.note("api (%s): %d consts, %d vars, %d funcs, %d types", relDir, len(pkg.Consts), len(pkg.Vars), len(pkg.Funcs), len(pkg.Types))
	return nil
}

func qualifySymbol(pkgPrefix, name string) string {
	if pkgPrefix == "" {
		return name
	}
	return pkgPrefix + ": " + name
}

func writeValues(g *generator, writer *mdWriter, pkgPrefix, title string, values []*doc.Value, level int) {
	if len(values) == 0 {
		return
	}
	writer.heading(level, title)
	for _, value := range values {
		writeValue(g, writer, pkgPrefix, value, level+1)
	}
}

func writeValue(g *generator, writer *mdWriter, pkgPrefix string, value *doc.Value, level int) {
	// go/doc records the comment above the declaration, so a grouped var or const
	// block with per-name comments has an empty Value.Doc; check the specs too.
	if value.Doc == "" {
		for _, name := range value.Names {
			if !specDocumented(value.Decl, name) {
				g.noteUndocumented(qualifySymbol(pkgPrefix, name))
			}
		}
	}
	writer.heading(level, "`"+strings.Join(value.Names, ", ")+"`")
	if value.Doc != "" {
		writer.paragraph(value.Doc)
	}
	writer.code("go", g.print(value.Decl))
}

func writeFunc(g *generator, writer *mdWriter, pkgPrefix string, renderedExamples map[string]bool, function *doc.Func, level int) {
	if function.Doc == "" {
		g.noteUndocumented(qualifySymbol(pkgPrefix, symbolName(function)))
	}
	writer.heading(level, "`"+signatureLine(g, function)+"`")
	if function.Doc != "" {
		writer.paragraph(function.Doc)
	}
	for _, example := range function.Examples {
		writeExample(g, writer, renderedExamples, example, level+1)
	}
}

func writeTypes(g *generator, writer *mdWriter, pkgPrefix string, renderedExamples map[string]bool, pkg *doc.Package, level int) {
	if len(pkg.Types) == 0 {
		return
	}
	writer.heading(level, "Types")
	for _, typ := range pkg.Types {
		if typ.Doc == "" {
			g.noteUndocumented(qualifySymbol(pkgPrefix, typ.Name))
		}
		writer.heading(level+1, "`type "+typ.Name+"`")
		if typ.Doc != "" {
			writer.paragraph(typ.Doc)
		}
		writer.code("go", g.print(typ.Decl))
		for _, value := range typ.Consts {
			writeValue(g, writer, pkgPrefix, value, level+2)
		}
		for _, value := range typ.Vars {
			writeValue(g, writer, pkgPrefix, value, level+2)
		}
		for _, function := range typ.Funcs {
			writeFunc(g, writer, pkgPrefix, renderedExamples, function, level+2)
		}
		for _, method := range typ.Methods {
			writeFunc(g, writer, pkgPrefix, renderedExamples, method, level+2)
		}
		for _, example := range typ.Examples {
			writeExample(g, writer, renderedExamples, example, level+2)
		}
	}
}

// writeExamples renders the examples that assert output and are not already
// shown with a symbol: package-level examples, which no type or function owns.
func writeExamples(g *generator, writer *mdWriter, renderedExamples map[string]bool, pkg *doc.Package) {
	writer.heading(2, "Examples")
	total := 0
	seen := map[string]bool{}
	record := func(example *doc.Example) {
		if seen[example.Name] {
			return
		}
		seen[example.Name] = true
		total++
		if renderedExamples[example.Name] {
			return
		}
		writeExample(g, writer, renderedExamples, example, 3)
	}
	for _, example := range pkg.Examples {
		record(example)
	}
	for _, function := range pkg.Funcs {
		for _, example := range function.Examples {
			record(example)
		}
	}
	for _, typ := range pkg.Types {
		for _, example := range typ.Examples {
			record(example)
		}
		for _, function := range typ.Funcs {
			for _, example := range function.Examples {
				record(example)
			}
		}
		for _, method := range typ.Methods {
			for _, example := range method.Examples {
				record(example)
			}
		}
	}
	if total == 0 {
		writer.paragraph("No example in the root package carries a `// Output:` comment yet.")
	} else {
		writer.paragraph(plural(total, "example asserts output; it is shown with the symbol it documents.", "examples assert output; each one is shown with the symbol it documents."))
	}
	g.note("api: %d examples with output", total)
}

// writeSubpackageExamples renders package-level examples that assert output for
// a subpackage, omitting the section when the subpackage defines no examples.
func writeSubpackageExamples(g *generator, writer *mdWriter, renderedExamples map[string]bool, pkg *doc.Package, level int) {
	var pending []*doc.Example
	seen := map[string]bool{}
	collect := func(example *doc.Example) {
		if seen[example.Name] || !hasOutput(example) {
			return
		}
		seen[example.Name] = true
		if !renderedExamples[example.Name] {
			pending = append(pending, example)
		}
	}
	for _, example := range pkg.Examples {
		collect(example)
	}
	for _, function := range pkg.Funcs {
		for _, example := range function.Examples {
			collect(example)
		}
	}
	for _, typ := range pkg.Types {
		for _, example := range typ.Examples {
			collect(example)
		}
		for _, function := range typ.Funcs {
			for _, example := range function.Examples {
				collect(example)
			}
		}
		for _, method := range typ.Methods {
			for _, example := range method.Examples {
				collect(example)
			}
		}
	}
	if len(pending) == 0 {
		return
	}
	writer.heading(level, "Examples")
	for _, example := range pending {
		writeExample(g, writer, renderedExamples, example, level+1)
	}
}

// writeExample renders one example that asserts output; examples without an
// Output comment are reported in the summary but not inlined. Each example is
// rendered once: the first symbol it is associated with owns it.
func writeExample(g *generator, writer *mdWriter, renderedExamples map[string]bool, example *doc.Example, level int) {
	if !hasOutput(example) || renderedExamples[example.Name] {
		return
	}
	renderedExamples[example.Name] = true
	// A package-level example has no symbol name of its own.
	title := "Package example"
	if example.Name != "" && example.Name != "Example" {
		title = "Example: `" + example.Name + "`"
	}
	writer.heading(level, title)
	if example.Doc != "" {
		writer.paragraph(example.Doc)
	}
	body := g.exampleBody(example)
	if example.Output != "" {
		body += "\n// Output:\n" + strings.TrimRight(example.Output, "\n")
	}
	writer.code("go", body)
}

// exampleBody renders the statements of one example without the enclosing
// function braces, which is how the quick start reads in a document.
func (g *generator) exampleBody(example *doc.Example) string {
	block, ok := example.Code.(*ast.BlockStmt)
	if !ok {
		return g.print(example.Code)
	}
	lines := make([]string, 0, len(block.List))
	for _, statement := range block.List {
		lines = append(lines, g.print(statement))
	}
	return strings.Join(lines, "\n")
}

// hasOutput reports whether an example asserts output. EmptyOutput means the
// example expects no output at all, which is still an assertion.
func hasOutput(example *doc.Example) bool {
	return example.Output != "" || example.EmptyOutput
}

// signatureLine renders one func or method as a single line: receiver, name and
// parameter/result lists, without the body.
func signatureLine(g *generator, function *doc.Func) string {
	if function.Decl == nil {
		return function.Name
	}
	copy := *function.Decl
	copy.Doc = nil
	copy.Body = nil
	return strings.TrimSpace(g.print(&copy))
}

// specDocumented reports whether one name of a var or const declaration carries
// a comment of its own.
func specDocumented(decl *ast.GenDecl, name string) bool {
	if decl == nil {
		return false
	}
	for _, spec := range decl.Specs {
		value, ok := spec.(*ast.ValueSpec)
		if !ok {
			continue
		}
		for _, identifier := range value.Names {
			if identifier.Name != name {
				continue
			}
			return value.Doc != nil || value.Comment != nil
		}
	}
	return false
}

// symbolName is the name used in the undocumented report.
func symbolName(function *doc.Func) string {
	if function.Recv != "" {
		return function.Recv + "." + function.Name
	}
	return function.Name
}
