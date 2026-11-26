package main

import (
	"fmt"
	"go/ast"
	"go/doc"
	"strings"
)

// renderAPI builds api.md: the exported surface of the root package as go/doc
// sees it, plus the examples that carry a `// Output:` comment.
//
// It also records every exported symbol without a doc comment in
// g.undocumented so that -check fails until the symbol is documented.
func renderAPI(g *generator) ([]byte, error) {
	files, err := g.dirFiles("", true)
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
	pkg, err := doc.NewFromFiles(g.fset, asts, g.modulePath(), doc.PreserveAST)
	if err != nil {
		return nil, fmt.Errorf("documenting the root package: %w", err)
	}
	if pkg.Doc == "" {
		g.noteUndocumented("package " + pkg.Name)
	}
	writer := newMarkdownWriter()
	writer.heading(1, "`"+pkg.Name+"` package reference")
	writer.paragraph(fmt.Sprintf("Import path `%s`.", g.modulePath()))
	if pkg.Doc != "" {
		writer.heading(2, "Package documentation")
		writer.paragraph(pkg.Doc)
	}
	writeValues(g, writer, "Constants", pkg.Consts)
	writeValues(g, writer, "Variables", pkg.Vars)
	if len(pkg.Funcs) > 0 {
		writer.heading(2, "Functions")
		for _, function := range pkg.Funcs {
			writeFunc(g, writer, function, 3)
		}
	}
	writeTypes(g, writer, pkg)
	writeExamples(g, writer, pkg)
	g.note("api: %d consts, %d vars, %d funcs, %d types", len(pkg.Consts), len(pkg.Vars), len(pkg.Funcs), len(pkg.Types))
	return writer.bytes(), nil
}

func writeValues(g *generator, writer *mdWriter, title string, values []*doc.Value) {
	if len(values) == 0 {
		return
	}
	writer.heading(2, title)
	for _, value := range values {
		writeValue(g, writer, value, 3)
	}
}

func writeValue(g *generator, writer *mdWriter, value *doc.Value, level int) {
	// go/doc records the comment above the declaration, so a grouped var or const
	// block with per-name comments has an empty Value.Doc; check the specs too.
	if value.Doc == "" {
		for _, name := range value.Names {
			if !specDocumented(value.Decl, name) {
				g.noteUndocumented(name)
			}
		}
	}
	writer.heading(level, "`"+strings.Join(value.Names, ", ")+"`")
	if value.Doc != "" {
		writer.paragraph(value.Doc)
	}
	writer.code("go", g.print(value.Decl))
}

func writeFunc(g *generator, writer *mdWriter, function *doc.Func, level int) {
	if function.Doc == "" {
		g.noteUndocumented(symbolName(function))
	}
	writer.heading(level, "`"+signatureLine(g, function)+"`")
	if function.Doc != "" {
		writer.paragraph(function.Doc)
	}
	for _, example := range function.Examples {
		writeExample(g, writer, example, level+1)
	}
}

func writeTypes(g *generator, writer *mdWriter, pkg *doc.Package) {
	if len(pkg.Types) == 0 {
		return
	}
	writer.heading(2, "Types")
	for _, typ := range pkg.Types {
		if typ.Doc == "" {
			g.noteUndocumented(typ.Name)
		}
		writer.heading(3, "`type "+typ.Name+"`")
		if typ.Doc != "" {
			writer.paragraph(typ.Doc)
		}
		writer.code("go", g.print(typ.Decl))
		for _, value := range typ.Consts {
			writeValue(g, writer, value, 4)
		}
		for _, value := range typ.Vars {
			writeValue(g, writer, value, 4)
		}
		for _, function := range typ.Funcs {
			writeFunc(g, writer, function, 4)
		}
		for _, method := range typ.Methods {
			writeFunc(g, writer, method, 4)
		}
		for _, example := range typ.Examples {
			writeExample(g, writer, example, 4)
		}
	}
}

// writeExamples renders the examples that assert output and are not already
// shown with a symbol: package-level examples, which no type or function owns.
func writeExamples(g *generator, writer *mdWriter, pkg *doc.Package) {
	writer.heading(2, "Examples")
	total := 0
	seen := map[string]bool{}
	record := func(example *doc.Example) {
		if seen[example.Name] {
			return
		}
		seen[example.Name] = true
		total++
		if g.renderedExamples[example.Name] {
			return
		}
		writeExample(g, writer, example, 3)
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

// writeExample renders one example that asserts output; examples without an
// Output comment are reported in the summary but not inlined. Each example is
// rendered once: the first symbol it is associated with owns it.
func writeExample(g *generator, writer *mdWriter, example *doc.Example, level int) {
	if !hasOutput(example) || g.renderedExamples[example.Name] {
		return
	}
	g.renderedExamples[example.Name] = true
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
