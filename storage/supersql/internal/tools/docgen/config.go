package main

import (
	"fmt"
	"go/ast"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
)

// The embedder-facing configuration types and where their canonical default
// construction lives. Only the root package's own literals are treated as
// defaults; every other construction site is listed separately.
const (
	openOptionsFile    = "open.go"
	neosepiaConfigFile = "internal/dataplane/storage/neosepia/backend_config.go"
	descriptorFile     = "internal/model/descriptor.go"
	irLimitsFile       = "internal/query/ir/types.go"
)

// construction is one composite literal of a limits type found in source.
type construction struct {
	location string
	fields   []string
}

func renderConfiguration(g *generator) ([]byte, error) {
	writer := newMarkdownWriter()
	writer.heading(1, "Configuration reference")
	writer.paragraph("Every resource envelope an embedder can set, with the default construction observed in source. Defaults are read from the source literal of the documented constructor, never duplicated by hand.")

	if err := writeOpenOptions(g, writer); err != nil {
		return nil, err
	}
	if err := writeCompactionConfig(g, writer); err != nil {
		return nil, err
	}
	if err := writeDescriptorLimits(g, writer); err != nil {
		return nil, err
	}
	if err := writeIRLimits(g, writer); err != nil {
		return nil, err
	}
	writeCopyOptions(g, writer)
	writeMaintenanceConfig(g, writer)
	if err := writeInternalLimits(g, writer); err != nil {
		return nil, err
	}
	return writer.bytes(), nil
}

func writeOpenOptions(g *generator, writer *mdWriter) error {
	file, err := g.parse(openOptionsFile)
	if err != nil {
		return err
	}
	structure, ok := findStruct(file, "OpenOptions")
	if !ok {
		return fmt.Errorf("%s: OpenOptions not found", openOptionsFile)
	}
	writer.heading(2, "`supersql.OpenOptions`")
	if doc := docFor(file, "OpenOptions"); doc != "" {
		writer.paragraph(doc)
	}
	rows := make([][]string, 0, len(structure.Fields.List))
	for _, field := range g.structFields(structure) {
		rows = append(rows, []string{
			strings.Join(field.names, ", "),
			field.typ,
			openOptionDefault(strings.Join(field.names, ", ")),
			field.doc,
		})
	}
	writer.table([]string{"Field", "Type", "Default", "Description"}, rows)
	writer.paragraph("Open rejects an empty `Directory` with `ErrDirectoryRequired`, a nil or unavailable `Clock` with `ErrClockRequired`, and maps an incompatible directory layout to `*UpgradeRequired`. A nonzero `Compaction` replaces the whole backend envelope rather than merging field by field.")
	return nil
}

// openOptionDefault documents the zero-value behavior of each OpenOptions field.
func openOptionDefault(name string) string {
	switch name {
	case "Directory":
		return "none; required"
	case "Compaction":
		return "zero value selects `neosepia.DefaultLimits()`"
	case "Clock":
		return "none; required"
	case "HistoricalReadOnly":
		return "false"
	default:
		return "-"
	}
}

func writeCompactionConfig(g *generator, writer *mdWriter) error {
	file, err := g.parse(neosepiaConfigFile)
	if err != nil {
		return err
	}
	structure, ok := findStruct(file, "Limits")
	if !ok {
		return fmt.Errorf("%s: Limits not found", neosepiaConfigFile)
	}
	fields := g.structFields(structure)
	defaults := map[string]string{}
	if body, ok := findFuncBody(file, "DefaultLimits"); ok {
		if literal, ok := returnComposite(body, "Limits"); ok {
			defaults = g.fieldValues(literal)
		}
	}
	writer.heading(2, "`supersql.CompactionConfig`")
	writer.paragraph("`CompactionConfig` is a type alias of `neosepia.Limits`: the NeoSepia backend resource envelope covering resident handles, WAL, mutation and run preparation, read budgets and plan budgets. The zero value selects the defaults below; `neosepia.Open` rejects any zero bound or non-positive count, so a partially filled envelope is invalid rather than partially defaulted.")
	writer.paragraph(fmt.Sprintf("%d fields; defaults are the `return Limits{...}` literal of `DefaultLimits` in `%s`.", len(fields), neosepiaConfigFile))
	rows := make([][]string, 0, len(fields))
	missing := 0
	for _, field := range fields {
		name := strings.Join(field.names, ", ")
		value, ok := defaults[name]
		if !ok {
			missing++
			value = "-"
		}
		rows = append(rows, []string{name, field.typ, value})
	}
	writer.table([]string{"Field", "Type", "Default"}, rows)
	if missing > 0 {
		writer.paragraph(plural(missing, "field has no entry in `DefaultLimits`; its documented default is `-`.", "fields have no entry in `DefaultLimits`; their documented defaults are `-`."))
	}
	g.note("configuration: CompactionConfig %d fields, %d with defaults", len(fields), len(fields)-missing)
	return nil
}

func writeDescriptorLimits(g *generator, writer *mdWriter) error {
	file, err := g.parse(descriptorFile)
	if err != nil {
		return err
	}
	structure, ok := findStruct(file, "DescriptorLimits")
	if !ok {
		return fmt.Errorf("%s: DescriptorLimits not found", descriptorFile)
	}
	writer.heading(2, "`model.DescriptorLimits`")
	writer.paragraph("Bounds one logical descriptor serialization: total bytes, item count and nesting depth. `Validate` rejects a zero byte bound, a non-positive item count, and a depth outside `(0, 64]` with `ErrDescriptorLimit`.")
	rows := make([][]string, 0)
	for _, field := range g.structFields(structure) {
		rows = append(rows, []string{strings.Join(field.names, ", "), field.typ, field.doc})
	}
	writer.table([]string{"Field", "Type", "Description"}, rows)
	constructions, err := collectConstructions(g, "model.DescriptorLimits", descriptorFile)
	if err != nil {
		return err
	}
	writer.paragraph("The root package fixes its own envelope when it opens a database; the table lists every construction site in non-test source.")
	writeConstructions(writer, constructions)
	return nil
}

func writeIRLimits(g *generator, writer *mdWriter) error {
	file, err := g.parse(irLimitsFile)
	if err != nil {
		return err
	}
	structure, ok := findStruct(file, "Limits")
	if !ok {
		return fmt.Errorf("%s: Limits not found", irLimitsFile)
	}
	writer.heading(2, "`ir.Limits`")
	writer.paragraph("Bounds one intermediate-representation graph built by the binder: node, operand and slot counts, nesting depth, total bytes and source bytes.")
	rows := make([][]string, 0)
	for _, field := range g.structFields(structure) {
		rows = append(rows, []string{strings.Join(field.names, ", "), field.typ, field.doc})
	}
	writer.table([]string{"Field", "Type", "Description"}, rows)
	constructions, err := collectConstructions(g, "ir.Limits", irLimitsFile)
	if err != nil {
		return err
	}
	writer.paragraph("The root package's `Catalog.irLimits` supplies the default envelope for statements it executes.")
	writeConstructions(writer, constructions)
	return nil
}

func writeCopyOptions(g *generator, writer *mdWriter) {
	file, err := g.parse(openOptionsFile)
	if err != nil {
		return
	}
	structure, ok := findStruct(file, "CopyOptions")
	if !ok {
		return
	}
	writer.heading(2, "`supersql.CopyOptions`")
	if doc := docFor(file, "CopyOptions"); doc != "" {
		writer.paragraph(doc)
	}
	rows := make([][]string, 0)
	for _, field := range g.structFields(structure) {
		rows = append(rows, []string{strings.Join(field.names, ", "), field.typ, field.doc})
	}
	writer.table([]string{"Field", "Type", "Description"}, rows)
}

// writeMaintenanceConfig documents the background maintenance envelope when the
// root package defines it. The type lands in a later change, so its absence is
// not an error: the section simply does not exist yet.
func writeMaintenanceConfig(g *generator, writer *mdWriter) {
	files, err := g.dirFiles("", false)
	if err != nil {
		return
	}
	for _, rel := range files {
		file, err := g.parse(rel)
		if err != nil {
			return
		}
		structure, ok := findStruct(file, "MaintenanceConfig")
		if !ok {
			continue
		}
		writer.heading(2, "`supersql.MaintenanceConfig`")
		if doc := docFor(file, "MaintenanceConfig"); doc != "" {
			writer.paragraph(doc)
		}
		rows := make([][]string, 0)
		for _, field := range g.structFields(structure) {
			rows = append(rows, []string{strings.Join(field.names, ", "), field.typ, field.doc})
		}
		writer.table([]string{"Field", "Type", "Description"}, rows)
		var constants [][]string
		for _, constant := range g.fileConstants(rel, file) {
			if !token.IsExported(constant.name) {
				continue
			}
			constants = append(constants, []string{constant.name, constant.value, g.position(constant.position)})
		}
		if len(constants) > 0 {
			writer.paragraph("Constants declared alongside the type:")
			writer.table([]string{"Constant", "Value", "Location"}, constants)
		}
		g.note("configuration: MaintenanceConfig documented from %s", rel)
		return
	}
	g.note("configuration: MaintenanceConfig absent from the root package; section omitted")
}

func writeInternalLimits(g *generator, writer *mdWriter) error {
	type entry struct {
		name   string
		file   string
		fields int
	}
	var entries []entry
	err := g.walkSources(func(rel string, file *ast.File) error {
		for _, declaration := range file.Decls {
			gen, ok := declaration.(*ast.GenDecl)
			if !ok || gen.Tok != token.TYPE {
				continue
			}
			for _, spec := range gen.Specs {
				typeSpec, ok := spec.(*ast.TypeSpec)
				if !ok || !strings.HasSuffix(typeSpec.Name.Name, "Limits") {
					continue
				}
				structure, ok := typeSpec.Type.(*ast.StructType)
				if !ok {
					continue
				}
				entries = append(entries, entry{name: typeSpec.Name.Name, file: rel, fields: len(g.structFields(structure))})
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].file != entries[j].file {
			return entries[i].file < entries[j].file
		}
		return entries[i].name < entries[j].name
	})
	writer.heading(2, "Internal limit structs")
	writer.paragraph(plural(len(entries), "limit struct in non-test source is internal to a package and cannot be set by an embedder; it is listed so the full bound surface is visible.", "limit structs in non-test source are internal to a package and cannot be set by an embedder; they are listed so the full bound surface is visible."))
	rows := make([][]string, 0, len(entries))
	for _, item := range entries {
		rows = append(rows, []string{item.name, item.file, fmt.Sprint(item.fields)})
	}
	writer.table([]string{"Type", "File", "Fields"}, rows)
	g.note("configuration: %d internal limit structs", len(entries))
	return nil
}

// collectConstructions lists every composite literal of the named type in
// non-test source, in file:line order, with its field assignments. A literal
// written inside the declaring package uses the bare type name, so the bare
// form is accepted only in the declaring package's own directory.
func collectConstructions(g *generator, wanted, declarationFile string) ([]construction, error) {
	declarationDirectory := filepath.Dir(declarationFile)
	bare := lastSegment(wanted)
	var sites []construction
	err := g.walkSources(func(rel string, file *ast.File) error {
		ast.Inspect(file, func(node ast.Node) bool {
			literal, ok := node.(*ast.CompositeLit)
			if !ok {
				return true
			}
			name := exprName(literal.Type)
			if name != wanted && !(name == bare && filepath.Dir(rel) == declarationDirectory) {
				return true
			}
			fields := make([]string, 0, len(literal.Elts))
			for _, element := range literal.Elts {
				field, ok := element.(*ast.KeyValueExpr)
				if !ok {
					fields = append(fields, g.print(element))
					continue
				}
				key, ok := field.Key.(*ast.Ident)
				if !ok {
					fields = append(fields, g.print(element))
					continue
				}
				fields = append(fields, key.Name+"="+g.print(field.Value))
			}
			sort.Strings(fields)
			sites = append(sites, construction{location: g.position(literal.Pos()), fields: fields})
			return true
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(sites, func(i, j int) bool { return sites[i].location < sites[j].location })
	return sites, nil
}

// writeConstructions renders construction sites, or states that there are none.
func writeConstructions(writer *mdWriter, sites []construction) {
	if len(sites) == 0 {
		writer.paragraph("No construction site in non-test source.")
		return
	}
	rows := make([][]string, 0, len(sites))
	for _, site := range sites {
		rows = append(rows, []string{site.location, strings.Join(site.fields, " ")})
	}
	writer.table([]string{"Location", "Fields"}, rows)
}

// rootConstant is one named constant of the root package.
type rootConstant struct {
	name     string
	value    string
	location string
}

func collectRootConstants(g *generator) ([]rootConstant, error) {
	files, err := g.dirFiles("", false)
	if err != nil {
		return nil, err
	}
	var constants []rootConstant
	for _, rel := range files {
		file, err := g.parse(rel)
		if err != nil {
			return nil, err
		}
		for _, constant := range g.fileConstants(rel, file) {
			constants = append(constants, rootConstant{name: constant.name, value: constant.value, location: g.position(constant.position)})
		}
	}
	sort.Slice(constants, func(i, j int) bool { return constants[i].name < constants[j].name })
	return constants, nil
}

// docFor returns the doc comment of a top-level type declaration.
func docFor(file *ast.File, name string) string {
	for _, declaration := range file.Decls {
		gen, ok := declaration.(*ast.GenDecl)
		if !ok || gen.Tok != token.TYPE || gen.Doc == nil {
			continue
		}
		for _, spec := range gen.Specs {
			typeSpec, ok := spec.(*ast.TypeSpec)
			if !ok || typeSpec.Name.Name != name {
				continue
			}
			return strings.TrimSpace(gen.Doc.Text())
		}
	}
	return ""
}
