package procedural

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

type CompileOptions struct {
	Bind        bind.Options
	Limits      Limits
	CheckBodies bool
}
type Compiler struct {
	Limits Limits
}

func (frontend Compiler) Compile(ctx context.Context, statement *zpgparse.CreateFunctionStmt, metadata model.RoutineMetadata, snapshot bind.CatalogSnapshot, options bind.Options, checkBodies bool) (model.RoutineMetadata, error) {
	return CompileRoutine(ctx, statement, metadata, snapshot, CompileOptions{Bind: options, Limits: frontend.Limits, CheckBodies: checkBodies})
}
func (frontend Compiler) CompileInline(ctx context.Context, statement *zpgparse.DoStmt, snapshot bind.CatalogSnapshot, options bind.Options) (*Program, []model.Dependency, error) {
	if statement == nil || snapshot == nil {
		return nil, nil, sqlError("22023", "inline routine compilation requires a statement and catalog snapshot", nil)
	}
	limits := frontend.Limits
	if err := limits.Validate(); err != nil {
		return nil, nil, err
	}
	function, err := zpgparse.ParsePLpgSQLDO(statement)
	if err != nil {
		return nil, nil, err
	}
	metadata := model.RoutineMetadata{Language: "plpgsql", Procedure: true, Result: model.TypeLayout{Type: model.Type{ID: 2278, Storage: model.TypeAny, Modifier: -1}}}
	c := &compiler{ctx: ctx, snapshot: snapshot, options: CompileOptions{Bind: options, Limits: limits, CheckBodies: true}, metadata: metadata, parameterMap: make(map[string]int), parameterNumbers: make(map[int]int)}
	c.encoding = programEncoding{Format: programFormat, Language: metadata.Language, Procedure: true, Result: metadata.Result}
	if err := c.compilePLpgSQLFunction(function); err != nil {
		return nil, nil, err
	}
	definition, err := encodeProgram(ctx, c.encoding, limits)
	if err != nil {
		return nil, nil, err
	}
	slices.SortFunc(c.dependencies, func(a, b model.Dependency) int {
		if a.Object != b.Object {
			return int64Compare(uint64(a.Object), uint64(b.Object))
		}
		if a.Column != b.Column {
			return int64Compare(uint64(a.Column), uint64(b.Column))
		}
		return int64Compare(a.Version, b.Version)
	})
	dependencies := slices.Compact(c.dependencies)
	program, err := Decode(ctx, definition, limits)
	return program, slices.Clone(dependencies), err
}

type compiler struct {
	ctx              context.Context
	snapshot         bind.CatalogSnapshot
	options          CompileOptions
	metadata         model.RoutineMetadata
	encoding         programEncoding
	parameterMap     map[string]int
	parameterNumbers map[int]int
	dependencies     []model.Dependency
}

// CompileRoutine compiles every static SQL fragment before returning a durable,
// versioned body. The supplied metadata is copied into the result.
func CompileRoutine(ctx context.Context, statement *zpgparse.CreateFunctionStmt, metadata model.RoutineMetadata, snapshot bind.CatalogSnapshot, options CompileOptions) (model.RoutineMetadata, error) {
	if statement == nil || snapshot == nil {
		return model.RoutineMetadata{}, sqlError("22023", "routine compilation requires a statement and catalog snapshot", nil)
	}
	if err := options.Limits.Validate(); err != nil {
		return model.RoutineMetadata{}, err
	}
	if err := ctx.Err(); err != nil {
		return model.RoutineMetadata{}, err
	}
	c := &compiler{ctx: ctx, snapshot: snapshot, options: options, metadata: metadata, parameterMap: make(map[string]int), parameterNumbers: make(map[int]int)}
	c.encoding = programEncoding{Format: programFormat, Language: metadata.Language, Procedure: metadata.Procedure, Atomic: metadata.Atomic, ReturnsSet: metadata.ReturnsSet, Result: metadata.Result, Settings: slices.Clone(metadata.Settings)}
	input := 0
	for i, parameter := range metadata.Parameters {
		if parameter.Mode != "in" && parameter.Mode != "inout" && parameter.Mode != "variadic" {
			continue
		}
		input++
		c.parameterNumbers[input] = i + 1
		if parameter.Name != "" {
			c.parameterMap[parameter.Name] = i + 1
		}
	}
	var err error
	switch metadata.Language {
	case "plpgsql":
		err = c.compilePLpgSQL(statement)
	case "sql":
		err = c.compileSQL(statement)
	default:
		return model.RoutineMetadata{}, sqlError("0A000", "unsupported procedural language", nil)
	}
	if err != nil {
		return model.RoutineMetadata{}, err
	}
	definition, err := encodeProgram(ctx, c.encoding, options.Limits)
	if err != nil {
		return model.RoutineMetadata{}, err
	}
	if metadata.Atomic && statement.SqlBody != nil {
		sql, deparseErr := zpgparse.Deparse(&zpgparse.ParseResult{Stmts: []*zpgparse.RawStmt{{Stmt: statement.SqlBody}}})
		if deparseErr != nil {
			return model.RoutineMetadata{}, deparseErr
		}
		definition.SQL = sql
	}
	slices.SortFunc(c.dependencies, func(a, b model.Dependency) int {
		if a.Object != b.Object {
			return int64Compare(uint64(a.Object), uint64(b.Object))
		}
		if a.Column != b.Column {
			return int64Compare(uint64(a.Column), uint64(b.Column))
		}
		return int64Compare(a.Version, b.Version)
	})
	c.dependencies = slices.Compact(c.dependencies)
	definition.Dependencies = slices.Clone(c.dependencies)
	metadata.Body = definition
	return metadata, nil
}

func int64Compare(left, right uint64) int {
	if left < right {
		return -1
	}
	if left > right {
		return 1
	}
	return 0
}

func (c *compiler) compilePLpgSQL(statement *zpgparse.CreateFunctionStmt) error {
	function, err := zpgparse.ParsePLpgSQLFunction(statement)
	if err != nil {
		return err
	}
	return c.compilePLpgSQLFunction(function)
}

func (c *compiler) compilePLpgSQLFunction(function *zpgparse.PLpgSQLFunction) error {
	if len(function.Datums) > c.options.Limits.MaxDatums {
		return ErrLimit
	}
	c.encoding.Datums = make([]datum, len(function.Datums))
	c.encoding.NewDatum = function.NewVarno + 1
	c.encoding.OldDatum = function.OldVarno + 1
	var err error
	for i, source := range function.Datums {
		target := &c.encoding.Datums[i]
		target.Kind = datumKind(source.Kind + 1)
		target.Name, target.Constant, target.NotNull = source.Name, source.Constant, source.NotNull
		if source.Kind == zpgparse.PLpgSQLRecordField {
			target.Parent, target.Field = source.Parent+1, source.Name
		} else {
			var layout model.TypeLayout
			switch {
			case source.Kind == zpgparse.PLpgSQLRow:
				layout, err = c.rowDatumLayout(source, i)
				if err != nil {
					return err
				}
			case i < len(c.metadata.Parameters):
				// Routine argument datums carry the declared signature layout,
				// including pseudo-type markers the parser cannot name.
				layout = c.metadata.Parameters[i].Layout
			case source.Name == "$0":
				layout = c.metadata.Result
			default:
				layout, err = c.resolveDatumType(source.Type, i)
				if err != nil {
					return fmt.Errorf("resolve PL/pgSQL datum %q: %w", source.Name, err)
				}
			}
			target.Layout = layout
		}
		for _, field := range source.Fields {
			target.FieldNames = append(target.FieldNames, field.Name)
			target.Fields = append(target.Fields, field.Varno)
		}
		// The durable layout codec strips FieldNames; a declared composite's
		// field names must ride the datum so runtime field references and
		// fixed row shapes survive serialization.
		if len(target.FieldNames) == 0 && len(target.Layout.FieldNames) != 0 && len(target.Layout.FieldNames) == len(target.Layout.Children) {
			target.FieldNames = slices.Clone(target.Layout.FieldNames)
		}
	}
	// Expression defaults and cursor queries bind against datumLayouts, which
	// must be complete before any of them compile: a row datum created by a
	// later INTO/FOR target would otherwise present a zero layout and fail
	// the bind.
	for i, source := range function.Datums {
		target := &c.encoding.Datums[i]
		if source.Default != nil {
			target.Default, err = c.addExpression(source.Default)
			if err != nil {
				return err
			}
		}
		if source.CursorQuery != nil {
			target.CursorOptions = source.CursorOptions
			target.CursorQuery, err = c.addExpression(source.CursorQuery)
			if err != nil {
				return err
			}
			if source.CursorArgRow >= 0 {
				target.CursorArgRow = source.CursorArgRow + 1
			}
		}
	}
	for i, parameter := range c.metadata.Parameters {
		if i >= len(c.encoding.Datums) {
			return ErrInvalidProgram
		}
		switch parameter.Mode {
		case "in", "inout", "variadic":
			c.encoding.Inputs = append(c.encoding.Inputs, i)
		}
		if parameter.Mode == "out" || parameter.Mode == "inout" || parameter.Mode == "table" {
			c.encoding.Outputs = append(c.encoding.Outputs, i)
		}
	}
	statements, err := c.compileStatements(function.Action.Body, 0)
	if err != nil {
		return err
	}
	root := statement{Kind: stmtBlock, Body: statements}
	for _, handler := range function.Action.Exceptions {
		body, err := c.compileStatements(handler.Action, 1)
		if err != nil {
			return err
		}
		root.Exceptions = append(root.Exceptions, exceptionHandler{Conditions: slices.Clone(handler.Conditions), Body: body})
	}
	c.encoding.Statements = []statement{root}
	return nil
}

// rowDatumLayout builds the anonymous composite layout for a Row datum. Row
// datums gather earlier datums into an unnamed record: routine outputs, cursor
// argument rows, and multi-variable INTO/FOR targets. The row has no nameable
// type of its own, so its layout is the composite of the datums it references.
// index is the row datum's own position: its fields must all precede it.
func (c *compiler) rowDatumLayout(source *zpgparse.PLpgSQLDatum, index int) (model.TypeLayout, error) {
	layout := model.TypeLayout{
		Type:       model.Type{ID: 2249, Storage: model.TypeComposite, Modifier: -1},
		Children:   make([]model.TypeLayout, len(source.Fields)),
		FieldNames: make([]string, len(source.Fields)),
	}
	for j, field := range source.Fields {
		if field.Varno < 0 || field.Varno >= index {
			return model.TypeLayout{}, fmt.Errorf("resolve PL/pgSQL datum %q: %w", source.Name, ErrInvalidProgram)
		}
		if c.encoding.Datums[field.Varno].Kind == datumField {
			// RecordField datums resolve through their parent record at
			// runtime and carry no static layout; the row falls back to a
			// bare record, which the INTO/FOR target paths refine via
			// setRecordLayout.
			return model.TypeLayout{Type: model.Type{ID: 2249, Storage: model.TypeComposite, Modifier: -1}}, nil
		}
		layout.Children[j] = c.encoding.Datums[field.Varno].Layout
		layout.FieldNames[j] = field.Name
	}
	return layout, nil
}

// resolveDatumType resolves a declared variable type: a plain type name, a
// plpgsql variable reference (name%TYPE or record%rowtype), or a catalog
// column/rowtype reference (rel.col%TYPE, rel%rowtype). before bounds the
// datum name space to declarations already resolved — PostgreSQL declares
// variables in order, so a %TYPE reference cannot see later datums.
func (c *compiler) resolveDatumType(name string, before int) (model.TypeLayout, error) {
	if name == "record" {
		return model.TypeLayout{Type: model.Type{ID: 2249, Storage: model.TypeComposite, Modifier: -1}}, nil
	}
	if name == "refcursor" {
		return model.TypeLayout{Type: model.Type{ID: 1790, Storage: model.TypeString, Modifier: -1, Collation: c.options.Bind.DefaultCollation}}, nil
	}
	if ref, ok := strings.CutSuffix(name, "%rowtype"); ok {
		return c.datumRowType(ref, before)
	}
	if ref, ok := strings.CutSuffix(name, "%TYPE"); ok {
		return c.datumColumnType(ref, before)
	}
	return c.bindDatumType(name)
}

// bindDatumType resolves a type name by binding a synthetic cast; the bound
// statement's dependencies record the type the routine now depends on.
func (c *compiler) bindDatumType(name string) (model.TypeLayout, error) {
	query := "SELECT NULL::" + name
	return c.bindDatumColumn(query)
}

func (c *compiler) bindDatumColumn(query string) (model.TypeLayout, error) {
	parsed, err := zpgparse.Parse(query)
	if err != nil || len(parsed.Stmts) != 1 {
		return model.TypeLayout{}, errors.Join(err, ErrInvalidProgram)
	}
	// The statement is synthesized here and has no source extent in the caller's
	// text, so its own length is the bound. The builder rejects zero.
	options := c.options.Bind
	options.Limits.SourceBytes = uint32(len(query))
	bound, err := bind.Bind(c.ctx, parsed.Stmts[0], c.snapshot, options, nil)
	if err != nil {
		return model.TypeLayout{}, err
	}
	c.dependencies = append(c.dependencies, bound.Dependencies()...)
	columns := bound.Columns()
	if len(columns) != 1 {
		return model.TypeLayout{}, ErrInvalidProgram
	}
	return columns[0].Layout, nil
}

// datumByName finds the most recent declared datum with name among the first
// before datums, matching PostgreSQL's innermost-declaration shadowing.
func (c *compiler) datumByName(name string, before int) (model.TypeLayout, bool) {
	for i := before - 1; i >= 0; i-- {
		if datum := c.encoding.Datums[i]; datum.Name == name {
			return datum.Layout, true
		}
	}
	return model.TypeLayout{}, false
}

// datumFieldType walks a composite/record datum layout through field names;
// a bare record has no static fields, so field %TYPE references on it fail at
// compile time exactly as PostgreSQL reports.
func datumFieldType(base model.TypeLayout, fields []string) (model.TypeLayout, error) {
	layout := base
	for _, field := range fields {
		if layout.Type.Storage != model.TypeComposite || len(layout.Children) == 0 {
			return model.TypeLayout{}, sqlError("42703", fmt.Sprintf("record has no field %q", field), nil)
		}
		index := slices.Index(layout.FieldNames, field)
		if index < 0 || index >= len(layout.Children) {
			return model.TypeLayout{}, sqlError("42703", fmt.Sprintf("record has no field %q", field), nil)
		}
		layout = layout.Children[index]
	}
	return layout, nil
}

// datumReferenceLayout resolves parts[0] as a declared datum and walks any
// remaining parts as composite fields. A nil layout means parts[0] is not a
// declared datum, so the caller falls back to the catalog reference path.
func (c *compiler) datumReferenceLayout(parts []typeRefPart, before int) (model.TypeLayout, error) {
	base, ok := c.datumByName(parts[0].name, before)
	if !ok {
		return model.TypeLayout{}, nil
	}
	fields := make([]string, len(parts)-1)
	for i, part := range parts[1:] {
		fields[i] = part.name
	}
	return datumFieldType(base, fields)
}

// datumColumnType resolves rel.col%TYPE and var%TYPE: the plpgsql name space
// shadows relations, so a leading datum name wins; otherwise the tail is a
// catalog column of the relation named by the head.
func (c *compiler) datumColumnType(ref string, before int) (model.TypeLayout, error) {
	parts, ok := splitTypeReference(ref)
	if !ok || len(parts) == 0 || len(parts) > 3 {
		return model.TypeLayout{}, sqlError("42601", fmt.Sprintf("invalid %%TYPE reference %q", ref), nil)
	}
	if len(parts) == 1 {
		if layout, found := c.datumByName(parts[0].name, before); found {
			return layout, nil
		}
		return model.TypeLayout{}, sqlError("42703", fmt.Sprintf("%q is not a known variable", parts[0].name), nil)
	}
	if layout, err := c.datumReferenceLayout(parts, before); err != nil {
		return model.TypeLayout{}, err
	} else if layout.Type.ID != 0 {
		return layout, nil
	}
	// rel.col%TYPE: the leading parts name a relation and the last a public
	// column. Resolving through the catalog records a column-scoped
	// dependency so altering or dropping the column invalidates the routine.
	relation, err := c.datumRelation(parts[:len(parts)-1])
	if err != nil {
		return model.TypeLayout{}, err
	}
	spec, err := relation.SpecWithin(c.ctx, c.descriptorLimits())
	if err != nil {
		return model.TypeLayout{}, err
	}
	var columns []model.ColumnDescriptor
	switch relation.Name().Kind {
	case model.ObjectTable:
		if spec.Table != nil {
			columns = spec.Table.Columns
		}
	case model.ObjectView, model.ObjectMaterializedView:
		if spec.View != nil {
			columns = spec.View.Columns
		}
	case model.ObjectCompositeRelation:
		if spec.CompositeRelation != nil {
			columns = spec.CompositeRelation.Columns
		}
	}
	column := parts[len(parts)-1].name
	for _, candidate := range columns {
		if candidate.SystemIdentity || candidate.State != model.StatePublic {
			continue
		}
		if candidate.Name == column {
			c.dependencies = append(c.dependencies,
				model.Dependency{Object: relation.ID(), Version: relation.Version(), Column: candidate.ID})
			return candidate.Layout, nil
		}
	}
	return model.TypeLayout{}, sqlError("42703", fmt.Sprintf("column %q does not exist", column), nil)
}

func (c *compiler) descriptorLimits() model.DescriptorLimits {
	return model.DescriptorLimits{
		MaxBytes: c.options.Bind.Limits.MaxBytes,
		MaxItems: int(c.options.Bind.Limits.MaxOperands),
		MaxDepth: int(min(c.options.Bind.Limits.MaxDepth, 64)),
	}
}

// datumRelation resolves the relation part of a %TYPE reference in
// search-path order over every row-carrying object kind, mirroring how
// PostgreSQL reads relation.column in a type reference.
func (c *compiler) datumRelation(parts []typeRefPart) (*model.Descriptor, error) {
	if len(parts) == 0 || len(parts) > 2 {
		return nil, sqlError("42P01", "relation does not exist", nil)
	}
	path := c.options.Bind.SearchPath
	if len(parts) == 2 {
		path = []string{parts[0].name}
	}
	name := parts[len(parts)-1].name
	for _, schema := range path {
		namespace, err := c.snapshot.Resolve(c.ctx, model.NamespaceName{Kind: model.ObjectSchema, Name: schema})
		if errors.Is(err, catalog.ErrUnknownObject) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, kind := range []model.ObjectKind{model.ObjectTable, model.ObjectView, model.ObjectMaterializedView, model.ObjectCompositeRelation} {
			published, err := c.snapshot.Resolve(c.ctx, model.NamespaceName{Parent: namespace.Descriptor.ID(), Kind: kind, Name: name})
			if errors.Is(err, catalog.ErrUnknownObject) {
				continue
			}
			if err != nil {
				return nil, err
			}
			if published.Descriptor.State() != model.StatePublic {
				continue
			}
			c.dependencies = append(c.dependencies, model.Dependency{Object: namespace.Descriptor.ID(), Version: namespace.Descriptor.Version()})
			return published.Descriptor, nil
		}
	}
	return nil, sqlError("42P01", fmt.Sprintf("relation %q does not exist", name), nil)
}

// datumRowType resolves rel%rowtype and composite datum references. A declared
// datum shadows the catalog name; otherwise the reference is a type name —
// PostgreSQL resolves table, view, and composite-type names identically here.
func (c *compiler) datumRowType(ref string, before int) (model.TypeLayout, error) {
	parts, ok := splitTypeReference(ref)
	if !ok || len(parts) == 0 || len(parts) > 3 {
		return model.TypeLayout{}, sqlError("42601", fmt.Sprintf("invalid %%rowtype reference %q", ref), nil)
	}
	if layout, err := c.datumReferenceLayout(parts, before); err != nil {
		return model.TypeLayout{}, err
	} else if layout.Type.ID != 0 {
		if layout.Type.Storage != model.TypeComposite {
			return model.TypeLayout{}, sqlError("42809", fmt.Sprintf("type %q is not composite", ref), nil)
		}
		return layout, nil
	}
	layout, err := c.bindDatumType(qualifiedName(parts))
	if err != nil {
		return model.TypeLayout{}, err
	}
	if layout.Type.Storage != model.TypeComposite {
		return model.TypeLayout{}, sqlError("42809", fmt.Sprintf("type %q is not composite", ref), nil)
	}
	return layout, nil
}

// typeRefPart is one identifier in a %TYPE/%ROWTYPE reference. The quoted
// flag records how the source spelled it: builtin type aliases only resolve
// for unquoted names, so the flag must reach the synthesized SQL unchanged.
type typeRefPart struct {
	name   string
	quoted bool
}

// splitTypeReference parses a possibly-qualified identifier chain, honoring
// double-quoted parts so a delimited identifier containing a dot stays whole.
func splitTypeReference(ref string) ([]typeRefPart, bool) {
	var parts []typeRefPart
	for {
		var part strings.Builder
		quoted := false
		if len(ref) != 0 && ref[0] == '"' {
			quoted = true
			i := 1
			closed := false
			for i < len(ref) {
				if ref[i] != '"' {
					part.WriteByte(ref[i])
					i++
					continue
				}
				if i+1 < len(ref) && ref[i+1] == '"' {
					part.WriteByte('"')
					i += 2
					continue
				}
				closed = true
				i++
				break
			}
			if !closed {
				return nil, false
			}
			ref = ref[i:]
		} else if i := strings.IndexByte(ref, '.'); i < 0 {
			part.WriteString(ref)
			ref = ""
		} else {
			part.WriteString(ref[:i])
			ref = ref[i:]
		}
		if part.Len() == 0 {
			return nil, false
		}
		parts = append(parts, typeRefPart{name: part.String(), quoted: quoted})
		if ref == "" {
			return parts, true
		}
		if ref[0] != '.' || len(ref) == 1 {
			return nil, false
		}
		ref = ref[1:]
	}
}

func quoteIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// qualifiedName rebuilds a dotted reference for synthesized SQL, re-quoting
// only parts the source quoted: "t"."col" stays delimited while folded
// names stay bare so builtin aliases and folded catalog names resolve.
func qualifiedName(parts []typeRefPart) string {
	var b strings.Builder
	for i, part := range parts {
		if i != 0 {
			b.WriteByte('.')
		}
		if part.quoted {
			b.WriteString(quoteIdentifier(part.name))
		} else {
			b.WriteString(part.name)
		}
	}
	return b.String()
}

func (c *compiler) compileSQL(declaration *zpgparse.CreateFunctionStmt) error {
	c.encoding.Datums = make([]datum, len(c.metadata.Parameters))
	for i, parameter := range c.metadata.Parameters {
		c.encoding.Datums[i] = datum{Kind: datumScalar, Name: parameter.Name, Layout: parameter.Layout}
		if parameter.Mode == "in" || parameter.Mode == "inout" || parameter.Mode == "variadic" {
			c.encoding.Inputs = append(c.encoding.Inputs, i)
		}
		if parameter.Mode == "out" || parameter.Mode == "inout" || parameter.Mode == "table" {
			c.encoding.Outputs = append(c.encoding.Outputs, i)
		}
	}
	nodes, err := sqlBodyStatements(declaration)
	if err != nil {
		return err
	}
	for _, node := range nodes {
		clone, ok := proto.Clone(node).(*zpgparse.Node)
		if !ok || clone == nil {
			return ErrInvalidProgram
		}
		c.rewriteParameters(clone.ProtoReflect())
		if transaction := clone.GetTransactionStmt(); transaction != nil {
			if !c.metadata.Procedure || c.metadata.Atomic {
				return sqlError("2D000", "invalid transaction termination", nil)
			}
			switch transaction.Kind {
			case zpgparse.TransactionStmtKind_TRANS_STMT_COMMIT:
				c.encoding.Statements = append(c.encoding.Statements, statement{Kind: stmtTransaction, Chain: transaction.Chain})
				continue
			case zpgparse.TransactionStmtKind_TRANS_STMT_ROLLBACK:
				c.encoding.Statements = append(c.encoding.Statements, statement{Kind: stmtTransaction, Rollback: true, Chain: transaction.Chain})
				continue
			}
		}
		query, err := c.addNode(clone)
		if err != nil {
			return err
		}
		kind := stmtExecute
		if clone.GetCallStmt() != nil || clone.GetDoStmt() != nil {
			kind = stmtCall
		}
		c.encoding.Statements = append(c.encoding.Statements, statement{Kind: kind, Query: query})
	}
	if len(c.encoding.Statements) == 0 {
		return sqlError("42P13", "SQL routine body is empty", nil)
	}
	return nil
}

func sqlBodyStatements(statement *zpgparse.CreateFunctionStmt) ([]*zpgparse.Node, error) {
	if statement.SqlBody != nil {
		return flattenSQLBody(statement.SqlBody), nil
	}
	body := ""
	for _, option := range statement.Options {
		definition := option.GetDefElem()
		if definition == nil || definition.Defname != "as" {
			continue
		}
		if list := definition.Arg.GetList(); list != nil {
			if len(list.Items) != 0 {
				body = list.Items[0].GetString_().GetSval()
			}
		} else if value := definition.Arg.GetString_(); value != nil {
			body = value.Sval
		}
	}
	if body == "" {
		return nil, sqlError("42P13", "routine body is missing", nil)
	}
	parsed, err := zpgparse.Parse(body)
	if err != nil {
		return nil, err
	}
	nodes := make([]*zpgparse.Node, len(parsed.Stmts))
	for i, raw := range parsed.Stmts {
		nodes[i] = raw.Stmt
	}
	return nodes, nil
}

func flattenSQLBody(node *zpgparse.Node) []*zpgparse.Node {
	if node == nil {
		return nil
	}
	if result := node.GetReturnStmt(); result != nil {
		selectNode := &zpgparse.SelectStmt{TargetList: []*zpgparse.Node{{Node: &zpgparse.Node_ResTarget{ResTarget: &zpgparse.ResTarget{Val: result.Returnval}}}}}
		return []*zpgparse.Node{{Node: &zpgparse.Node_SelectStmt{SelectStmt: selectNode}}}
	}
	list := node.GetList()
	if list == nil {
		return []*zpgparse.Node{node}
	}
	var result []*zpgparse.Node
	for _, item := range list.Items {
		if nested := item.GetList(); nested != nil {
			for _, child := range nested.Items {
				result = append(result, flattenSQLBody(child)...)
			}
			continue
		}
		result = append(result, flattenSQLBody(item)...)
	}
	return result
}

func (c *compiler) addExpression(expression *zpgparse.PLpgSQLExpression) (int, error) {
	if expression == nil || expression.SQL == nil || len(expression.SQL.Stmts) != 1 {
		return 0, ErrInvalidProgram
	}
	node := expression.SQL.Stmts[0].Stmt
	if assignment := node.GetPlassignStmt(); assignment != nil {
		node = &zpgparse.Node{Node: &zpgparse.Node_SelectStmt{SelectStmt: assignment.Val}}
	}
	return c.addNode(node)
}

func (c *compiler) addNode(node *zpgparse.Node) (int, error) {
	if len(c.encoding.Queries) >= c.options.Limits.MaxStatements {
		return 0, ErrLimit
	}
	if c.options.CheckBodies {
		// Routine body statements are parsed from the function definition rather
		// than from caller-supplied text, so their spans are not within the
		// caller's source. The routine's own byte budget is the bound; the IR
		// builder rejects a zero SourceBytes.
		//
		// Only query statements bind here. Utility commands, object DDL, and
		// statistics commands are executed by the session against the services
		// they need, so binding them as query statements would reject valid
		// PostgreSQL bodies such as CREATE TABLE inside DO.
		if bind.IsStatement(node) {
			options := c.options.Bind
			options.Limits.SourceBytes = uint32(min(c.options.Limits.MaxProgramBytes, uint64(math.MaxUint32)))
			// Routine bodies bind against signature layouts that may still be
			// declared pseudo-type markers; call sites resolve them per call.
			options.AdmitPseudoTypes()
			bound, err := bind.Bind(c.ctx, &zpgparse.RawStmt{Stmt: node}, c.snapshot, options, c.datumLayouts())
			if err != nil {
				return 0, err
			}
			c.dependencies = append(c.dependencies, bound.Dependencies()...)
		}
	}
	data, err := proto.Marshal(node)
	if err != nil || uint64(len(data)) > c.options.Limits.MaxProgramBytes {
		return 0, errors.Join(err, ErrLimit)
	}
	c.encoding.Queries = append(c.encoding.Queries, queryEncoding{Node: data})
	return len(c.encoding.Queries), nil
}

func (c *compiler) datumLayouts() []model.TypeLayout {
	layouts := make([]model.TypeLayout, len(c.encoding.Datums))
	for i := range layouts {
		layouts[i] = c.encoding.Datums[i].Layout
	}
	return layouts
}

func (c *compiler) rewriteParameters(message protoreflect.Message) {
	if message == nil || !message.IsValid() {
		return
	}
	if node, ok := message.Interface().(*zpgparse.Node); ok {
		if ref := node.GetParamRef(); ref != nil {
			number := c.parameterNumbers[int(ref.Number)]
			if number != 0 {
				ref.Number = int32(number)
			}
		}
		ref := node.GetColumnRef()
		if ref != nil && len(ref.Fields) == 1 {
			if name := ref.Fields[0].GetString_(); name != nil {
				if number := c.parameterMap[name.Sval]; number != 0 {
					node.Node = &zpgparse.Node_ParamRef{ParamRef: &zpgparse.ParamRef{Number: int32(number), Location: ref.Location}}
					return
				}
			}
		}
	}
	fields := message.Descriptor().Fields()
	for i := range fields.Len() {
		field := fields.Get(i)
		if field.IsMap() || (field.Kind() != protoreflect.MessageKind && field.Kind() != protoreflect.GroupKind) {
			continue
		}
		if field.IsList() {
			list := message.Get(field).List()
			for j := range list.Len() {
				c.rewriteParameters(list.Get(j).Message())
			}
		} else if message.Has(field) {
			c.rewriteParameters(message.Get(field).Message())
		}
	}
}

func (c *compiler) compileStatements(source []zpgparse.PLpgSQLStatement, depth int) ([]statement, error) {
	if depth > c.options.Limits.MaxDepth {
		return nil, ErrLimit
	}
	result := make([]statement, 0, len(source))
	for _, raw := range source {
		if err := c.ctx.Err(); err != nil {
			return nil, err
		}
		stmt, err := c.compileStatement(raw, depth)
		if err != nil {
			return nil, err
		}
		result = append(result, stmt)
	}
	return result, nil
}

func (c *compiler) compileStatement(raw zpgparse.PLpgSQLStatement, depth int) (statement, error) {
	var result statement
	var err error
	switch source := raw.(type) {
	case *zpgparse.PLpgSQLBlock:
		result = statement{Kind: stmtBlock, Line: source.Line, Label: source.Label}
		result.Body, err = c.compileStatements(source.Body, depth+1)
		for _, handler := range source.Exceptions {
			body, compileErr := c.compileStatements(handler.Action, depth+1)
			if compileErr != nil {
				return result, compileErr
			}
			result.Exceptions = append(result.Exceptions, exceptionHandler{Conditions: slices.Clone(handler.Conditions), Body: body})
		}
	case *zpgparse.PLpgSQLAssign:
		result = statement{Kind: stmtAssign, Line: source.Line, Target: source.Varno + 1}
		result.Query, err = c.addExpression(source.Expr)
	case *zpgparse.PLpgSQLIf:
		result = statement{Kind: stmtIf, Line: source.Line}
		result.Query, err = c.addExpression(source.Cond)
		if err == nil {
			result.Body, err = c.compileStatements(source.Then, depth+1)
		}
		for _, sourceBranch := range source.Elsifs {
			query, compileErr := c.addExpression(sourceBranch.Cond)
			if compileErr != nil {
				return result, compileErr
			}
			body, compileErr := c.compileStatements(sourceBranch.Body, depth+1)
			if compileErr != nil {
				return result, compileErr
			}
			result.Branches = append(result.Branches, branch{Query: query, Body: body})
		}
		if err == nil {
			result.Else, err = c.compileStatements(source.Else, depth+1)
		}
	case *zpgparse.PLpgSQLCase:
		result = statement{Kind: stmtCase, Line: source.Line, Target: source.Varno + 1, Strict: source.HaveElse}
		if source.Test != nil {
			result.Query, err = c.addExpression(source.Test)
		}
		for _, sourceBranch := range source.Whens {
			query, compileErr := c.addExpression(sourceBranch.Expr)
			if compileErr != nil {
				return result, compileErr
			}
			body, compileErr := c.compileStatements(sourceBranch.Body, depth+1)
			if compileErr != nil {
				return result, compileErr
			}
			result.Branches = append(result.Branches, branch{Query: query, Body: body})
		}
		if err == nil {
			result.Else, err = c.compileStatements(source.Else, depth+1)
		}
	case *zpgparse.PLpgSQLLoop:
		target := source.Varno
		if source.Var != nil {
			target = source.Var.Number
		}
		result = statement{Kind: stmtLoop, Line: source.Line, Label: source.Label, LoopKind: source.Kind, Target: target + 1, Reverse: source.Reverse, Slice: source.Slice}
		for _, expression := range []*zpgparse.PLpgSQLExpression{source.Cond, source.Lower, source.Upper, source.Step, source.Query, source.Argquery} {
			if expression == nil {
				result.Parameters = append(result.Parameters, 0)
				continue
			}
			query, compileErr := c.addExpression(expression)
			if compileErr != nil {
				return result, compileErr
			}
			result.Parameters = append(result.Parameters, query)
		}
		for _, parameter := range source.Params {
			query, compileErr := c.addExpression(parameter)
			if compileErr != nil {
				return result, compileErr
			}
			result.Using = append(result.Using, query)
		}
		result.Body, err = c.compileStatements(source.Body, depth+1)
	case *zpgparse.PLpgSQLExit:
		kind := stmtContinue
		if source.IsExit {
			kind = stmtExit
		}
		result = statement{Kind: kind, Line: source.Line, Label: source.Label}
		if source.Cond != nil {
			result.Query, err = c.addExpression(source.Cond)
		}
	case *zpgparse.PLpgSQLReturn:
		kind := stmtReturn
		if source.Kind == "return_next" {
			kind = stmtReturnNext
		} else if source.Kind == "return_query" {
			kind = stmtReturnQuery
		}
		result = statement{Kind: kind, Line: source.Line}
		if source.Retvarno >= 0 {
			result.Target = source.Retvarno + 1
		}
		if source.Expr != nil {
			result.Query, err = c.addExpression(source.Expr)
		} else if source.Query != nil {
			result.Query, err = c.addExpression(source.Query)
		} else if source.Dynquery != nil {
			result.Auxiliary, err = c.addExpression(source.Dynquery)
		}
		for _, parameter := range source.Params {
			query, compileErr := c.addExpression(parameter)
			if compileErr != nil {
				return result, compileErr
			}
			result.Parameters = append(result.Parameters, query)
		}
	case *zpgparse.PLpgSQLExecute:
		result = statement{Kind: stmtExecute, Line: source.Line, Target: datumNumber(source.Target), Strict: source.Strict, SetsFound: plpgsqlExecuteSetsFound(source)}
		if source.Dynamic {
			result.Auxiliary, err = c.addExpression(source.Query)
		} else {
			result.Query, err = c.addExpression(source.Query)
			if source.Query != nil && source.Query.SQL != nil && len(source.Query.SQL.Stmts) == 1 && source.Query.SQL.Stmts[0].Stmt.GetDoStmt() != nil {
				result.Kind = stmtCall
			}
		}
		for _, parameter := range source.Params {
			query, compileErr := c.addExpression(parameter)
			if compileErr != nil {
				return result, compileErr
			}
			result.Parameters = append(result.Parameters, query)
		}
	case *zpgparse.PLpgSQLPerform:
		result = statement{Kind: stmtPerform, Line: source.Line}
		result.Query, err = c.addExpression(source.Expr)
	case *zpgparse.PLpgSQLCall:
		result = statement{Kind: stmtCall, Line: source.Line, Target: datumNumber(source.Target)}
		result.Query, err = c.addExpression(source.Expr)
	case *zpgparse.PLpgSQLRaise:
		result = statement{Kind: stmtRaise, Line: source.Line, Level: source.Level, Condition: source.Condition}
		if source.Message != nil {
			result.Message = *source.Message
		}
		for _, parameter := range source.Params {
			query, compileErr := c.addExpression(parameter)
			if compileErr != nil {
				return result, compileErr
			}
			result.Parameters = append(result.Parameters, query)
		}
		for _, option := range source.Options {
			query, compileErr := c.addExpression(option.Expr)
			if compileErr != nil {
				return result, compileErr
			}
			result.Parameters = append(result.Parameters, query)
			result.OptionKinds = append(result.OptionKinds, option.Kind)
		}
	case *zpgparse.PLpgSQLAssert:
		result = statement{Kind: stmtAssert, Line: source.Line}
		result.Query, err = c.addExpression(source.Cond)
		if err == nil && source.Message != nil {
			result.Auxiliary, err = c.addExpression(source.Message)
		}
	case *zpgparse.PLpgSQLGetDiagnostics:
		result = statement{Kind: stmtGetDiagnostics, Line: source.Line, Stacked: source.Stacked}
		for _, item := range source.Items {
			result.Diagnostics = append(result.Diagnostics, diagnostic{Kind: item.Kind, Target: item.Target + 1})
		}
	case *zpgparse.PLpgSQLOpen:
		result = statement{Kind: stmtOpen, Line: source.Line, Target: source.Curvar + 1, CursorOptions: source.Options}
		if source.Args != nil {
			result.Argument, err = c.addExpression(source.Args)
		}
		if err == nil && source.Query != nil {
			result.Query, err = c.addExpression(source.Query)
		}
		if err == nil && source.Dynamic != nil {
			result.Auxiliary, err = c.addExpression(source.Dynamic)
		}
		if err == nil {
			for _, parameter := range source.Params {
				query, compileErr := c.addExpression(parameter)
				if compileErr != nil {
					return result, compileErr
				}
				result.Parameters = append(result.Parameters, query)
			}
		}
	case *zpgparse.PLpgSQLFetch:
		result = statement{Kind: stmtFetch, Line: source.Line, Target: datumNumber(source.Target), Cursor: source.Curvar + 1, Direction: source.Direction, Count: source.Count, Move: source.Move}
		if source.Expr != nil {
			result.Query, err = c.addExpression(source.Expr)
		}
	case *zpgparse.PLpgSQLClose:
		result = statement{Kind: stmtClose, Line: source.Line, Cursor: source.Curvar + 1}
	case *zpgparse.PLpgSQLTransaction:
		result = statement{Kind: stmtTransaction, Line: source.Line, Rollback: source.Rollback, Chain: source.Chain}
	default:
		return result, ErrInvalidProgram
	}
	return result, err
}

func datumNumber(value *zpgparse.PLpgSQLDatum) int {
	if value == nil {
		return 0
	}
	return value.Number + 1
}

func plpgsqlExecuteSetsFound(source *zpgparse.PLpgSQLExecute) bool {
	if source == nil || source.Dynamic {
		return false
	}
	if source.Into {
		return true
	}
	if source.Query == nil || source.Query.SQL == nil || len(source.Query.SQL.Stmts) != 1 {
		return false
	}
	node := source.Query.SQL.Stmts[0].Stmt
	return node.GetInsertStmt() != nil || node.GetUpdateStmt() != nil || node.GetDeleteStmt() != nil || node.GetMergeStmt() != nil
}
