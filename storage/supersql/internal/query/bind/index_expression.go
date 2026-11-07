package bind

import (
	"context"
	"slices"
	"strconv"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

type IndexExpressionParameter struct {
	Column model.ColumnID
	Layout model.TypeLayout
}

// Parameters are private row bindings, never SQL-client parameters. Their order
// is declaration order, independent of physical row order or column spelling.
type BoundIndexExpression struct {
	Graph        *ir.Graph
	Root         ir.ScalarID
	Parameters   []IndexExpressionParameter
	Result       model.TypeLayout
	Builtin      builtin.ResolveOptions
	Dependencies []model.Dependency
	identities   []routineDefaultIdentity
}

type rowExpressionPurpose uint8

const (
	rowIndexKey rowExpressionPurpose = iota
	rowIndexPredicate
	rowStoredGenerated
	rowCheck
	rowDomainCheck
	rowGenerationTransform
	rowStatistics
)

const (
	generationTransformVersion uint32 = 1
	statisticsExpressionVersion uint32 = 1
)

func (p rowExpressionPurpose) encoding() (string, uint32) {
	switch p {
	case rowStoredGenerated:
		return "supersql.stored-generated", storedGeneratedVersion
	case rowCheck:
		return "supersql.check", checkExpressionVersion
	case rowDomainCheck:
		return "supersql.domain-check", domainCheckExpressionVersion
	case rowGenerationTransform:
		return "supersql.generation-transform", generationTransformVersion
	case rowStatistics:
		return "supersql.statistics-expression", statisticsExpressionVersion
	default:
		return "supersql.index-expression", indexExpressionSemanticVersion
	}
}

func (p rowExpressionPurpose) hasTarget() bool {
	return p == rowStoredGenerated || p == rowGenerationTransform || p == rowStatistics
}

func defineIndexExpression(ctx context.Context, ast *zpgparse.Node, table *model.Descriptor, columns []model.ColumnDescriptor, snapshot CatalogSnapshot, options Options, predicate bool) (*model.ExpressionDefinition, *BoundIndexExpression, error) {
	purpose := rowIndexKey
	if predicate {
		purpose = rowIndexPredicate
	}
	return defineRowExpression(ctx, ast, table, columns, snapshot, options, purpose, 0, nil, 0)
}

func defineRowExpression(ctx context.Context, ast *zpgparse.Node, table *model.Descriptor, columns []model.ColumnDescriptor, snapshot CatalogSnapshot, options Options, purpose rowExpressionPurpose, targetColumn model.ColumnID, target *model.TypeLayout, constraint model.ObjectID) (*model.ExpressionDefinition, *BoundIndexExpression, error) {
	b, err := newExpressionBinder(ctx, snapshot, options)
	if err != nil {
		return nil, nil, err
	}
	if ast == nil || table == nil {
		return nil, nil, bindError("42601", "missing row expression or table declaration", 0)
	}
	remaining, payload := options.Limits.MaxNodes, options.Limits.MaxBytes
	if err := b.checkAST(ast.ProtoReflect(), 0, &remaining, &payload); err != nil {
		return nil, nil, err
	}
	owned := proto.Clone(ast).(*zpgparse.Node)
	format, version := purpose.encoding()
	encoding := indexExpressionEncoding{Format: format, Version: version, PostgreSQL: PostgreSQLVersion, Table: table.ID(), TableVersion: table.Version(), SearchPath: slices.Clone(options.SearchPath), Collation: options.Collation, DefaultCollation: options.DefaultCollation, Builtin: options.Builtin, Predicate: purpose == rowIndexPredicate || purpose == rowCheck || purpose == rowDomainCheck, ConstraintID: constraint}
	if purpose.hasTarget() {
		ownedTarget, _, err := model.CopyTypeLayout(ctx, *target, options.Limits.MaxBytes)
		if err != nil {
			return nil, nil, err
		}
		encoding.Target = &ownedTarget
		encoding.TargetColumn = targetColumn
	}
	if err := rewriteIndexAST(ctx, owned.ProtoReflect(), func(node *zpgparse.Node) error {
		if param := node.GetParamRef(); param != nil {
			return bindError("42P02", "index expressions cannot contain parameters", int(param.Location)+1)
		}
		ref := node.GetColumnRef()
		if ref == nil {
			return nil
		}
		parts, err := names(ref.Fields)
		if err != nil {
			return err
		}
		if len(parts) < 1 || len(parts) > 2 {
			return bindError("42P01", "index expression references another relation", int(ref.Location)+1)
		}
		if len(parts) == 2 && parts[0] != table.Name().Name {
			return bindError("42P01", "index expression references another relation", int(ref.Location)+1)
		}
		col, err := publicIndexColumn(columns, parts[len(parts)-1])
		if err != nil {
			return err
		}
		if col.SystemIdentity {
			return bindError("42703", "column does not exist", int(ref.Location)+1)
		}
		generatedReference := col.ID == targetColumn || col.Generated != nil || col.GeneratedStored
		if purpose == rowStoredGenerated && generatedReference {
			return bindError("42P17", "generated expressions may reference only ordinary columns", int(ref.Location)+1)
		}
		index := slices.IndexFunc(encoding.Columns, func(p IndexExpressionParameter) bool { return p.Column == col.ID })
		if index < 0 {
			if uint64(len(encoding.Columns)) >= uint64(options.Limits.MaxOperands) || len(encoding.Columns) >= 1<<31-1 {
				return bindError("54000", "index expression column limit exceeded", 0)
			}
			encoding.Columns = append(encoding.Columns, IndexExpressionParameter{Column: col.ID, Layout: col.Layout})
			index = len(encoding.Columns) - 1
		}
		node.Node = &zpgparse.Node_ParamRef{ParamRef: &zpgparse.ParamRef{Number: int32(index + 1), Location: -1}}
		return nil
	}); err != nil {
		return nil, nil, err
	}
	var pending *model.Descriptor
	switch purpose {
	case rowStoredGenerated, rowCheck, rowDomainCheck:
		pending = table
	default:
		// Index and partition expressions may be bound against a relation that
		// is still being declared, as happens for a partial index predicate or
		// an expression partition key. The local descriptor is authoritative
		// until the declaration publishes it.
		if published, err := b.catalog.Descriptor(b.ctx, table.ID()); err != nil || published.Descriptor.Version() < table.Version() {
			pending = table
		}
	}
	binding, deps, err := bindParameterizedRow(ctx, owned, encoding, snapshot, options.Limits, pending, purpose, true, options.declaration, options.Temporal)
	if err != nil {
		return nil, nil, err
	}
	if len(binding.identities) != 0 {
		encoding.Identities = binding.identities
		binding, deps, err = bindParameterizedRow(ctx, owned, encoding, snapshot, options.Limits, pending, purpose, false, options.declaration, options.Temporal)
		if err != nil {
			return nil, nil, err
		}
	}
	encoding.Fingerprint, err = indexBindingFingerprint(ctx, binding.Graph)
	if err != nil {
		return nil, nil, err
	}
	encoding.AST, err = (proto.MarshalOptions{Deterministic: true}).Marshal(owned)
	if err != nil {
		return nil, nil, err
	}
	deps = appendIndexDependency(deps, model.Dependency{Object: table.ID(), Version: table.Version()})
	for _, col := range encoding.Columns {
		deps = appendIndexDependency(deps, model.Dependency{Object: table.ID(), Version: table.Version(), Column: col.Column})
	}
	encoded, err := encodeIndexExpression(ctx, encoding, options.Limits)
	if err != nil {
		return nil, nil, err
	}
	binding.Dependencies = slices.Clone(deps)
	return &model.ExpressionDefinition{SemanticVersion: encoding.Version, Encoding: encoded, Dependencies: deps}, binding, nil
}

// DefineStatisticsExpression freezes an immutable single-table expression and
// persists its resolved result layout for later ANALYZE execution.
func DefineStatisticsExpression(ctx context.Context, ast *zpgparse.Node, table *model.Descriptor, columns []model.ColumnDescriptor, snapshot CatalogSnapshot, options Options) (*model.ExpressionDefinition, *BoundIndexExpression, error) {
	_, inferred, err := defineRowExpression(ctx, ast, table, columns, snapshot, options, rowIndexKey, 0, nil, 0)
	if err != nil {
		return nil, nil, err
	}
	definition, binding, err := defineRowExpression(ctx, ast, table, columns, snapshot, options, rowStatistics, 0, &inferred.Result, 0)
	if err != nil {
		return nil, nil, err
	}
	sql, err := zpgparse.DeparseExpression(ast)
	if err != nil {
		return nil, nil, err
	}
	definition.SQL = sql
	return definition, binding, nil
}
func DefineGenerationTransform(ctx context.Context, ast *zpgparse.Node, table *model.Descriptor, columns []model.ColumnDescriptor, targetColumn model.ColumnID, target model.TypeLayout, snapshot CatalogSnapshot, options Options) (*model.ExpressionDefinition, *BoundIndexExpression, error) {
	return defineRowExpression(ctx, ast, table, columns, snapshot, options, rowGenerationTransform, targetColumn, &target, 0)
}

func redefineRowExpression(ctx context.Context, definition *model.ExpressionDefinition, table *model.Descriptor, columns []model.ColumnDescriptor, snapshot CatalogSnapshot, options Options, purpose rowExpressionPurpose, targetColumn model.ColumnID, target *model.TypeLayout, constraint model.ObjectID) (*model.ExpressionDefinition, *BoundIndexExpression, error) {
	format, version := purpose.encoding()
	if definition == nil || definition.SemanticVersion != version || definition.SQL != "" || table == nil {
		return nil, nil, missing("row expression semantic encoding", 0)
	}
	encoding, ast, err := decodeRowExpression(ctx, definition.Encoding, options.Limits, format, version)
	if err != nil {
		return nil, nil, err
	}
	if encoding.Table != table.ID() || encoding.TargetColumn != targetColumn || encoding.ConstraintID != constraint {
		return nil, nil, bindError("42804", "row expression identity changed", 0)
	}
	if (encoding.Target != nil) != purpose.hasTarget() {
		return nil, nil, bindError("42804", "row expression purpose changed", 0)
	}
	if err := rewriteIndexAST(ctx, ast.ProtoReflect(), func(node *zpgparse.Node) error {
		parameter := node.GetParamRef()
		if parameter == nil {
			return nil
		}
		position := int(parameter.Number) - 1
		if position < 0 || position >= len(encoding.Columns) {
			return bindError("42P02", "row expression parameter metadata changed", 0)
		}
		column := tableColumn(columns, encoding.Columns[position].Column)
		if column.ID == 0 {
			return bindError("42703", "row expression column does not exist", 0)
		}
		node.Node = columnReference(column.Name).Node
		return nil
	}); err != nil {
		return nil, nil, err
	}
	options.SearchPath = slices.Clone(encoding.SearchPath)
	options.Collation = encoding.Collation
	options.DefaultCollation = encoding.DefaultCollation
	options.Builtin = encoding.Builtin
	return defineRowExpression(ctx, ast, table, columns, snapshot, options, purpose, targetColumn, target, constraint)
}

func BindGenerationTransform(ctx context.Context, definition *model.ExpressionDefinition, targetColumn model.ColumnID, columns []storage.ColumnLayout, result model.TypeLayout, snapshot CatalogSnapshot, limits ir.Limits) (*BoundIndexExpression, error) {
	return bindRowExpression(ctx, definition, targetColumn, columns, result, snapshot, limits, rowGenerationTransform, 0)
}


// BindIndexExpression never reparses SQL. It rebinds the persisted parameterized
// AST and checks its resolved semantic fingerprint before exposing executable IR.
func BindIndexExpression(ctx context.Context, definition *model.ExpressionDefinition, columns []storage.ColumnLayout, result model.TypeLayout, snapshot CatalogSnapshot, limits ir.Limits) (*BoundIndexExpression, error) {
	return bindRowExpression(ctx, definition, 0, columns, result, snapshot, limits, rowIndexKey, 0)
}

// BindStatisticsExpression rebinds a persisted statistics expression using its
// frozen result layout and returns that layout with the executable expression.
func BindStatisticsExpression(ctx context.Context, definition *model.ExpressionDefinition, columns []storage.ColumnLayout, snapshot CatalogSnapshot, limits ir.Limits) (*BoundIndexExpression, model.TypeLayout, error) {
	format, version := rowStatistics.encoding()
	if definition == nil || definition.SemanticVersion != version {
		return nil, model.TypeLayout{}, missing("statistics expression semantic encoding", 0)
	}
	encoding, _, err := decodeRowExpression(ctx, definition.Encoding, limits, format, version)
	if err != nil {
		return nil, model.TypeLayout{}, err
	}
	if encoding.Target == nil || encoding.TargetColumn != 0 {
		return nil, model.TypeLayout{}, bindError("42804", "statistics expression result type is unavailable", 0)
	}
	binding, err := bindRowExpression(ctx, definition, 0, columns, *encoding.Target, snapshot, limits, rowStatistics, 0)
	if err != nil {
		return nil, model.TypeLayout{}, err
	}
	return binding, *encoding.Target, nil
}

func bindRowExpression(ctx context.Context, definition *model.ExpressionDefinition, targetColumn model.ColumnID, columns []storage.ColumnLayout, result model.TypeLayout, snapshot CatalogSnapshot, limits ir.Limits, purpose rowExpressionPurpose, constraint model.ObjectID) (*BoundIndexExpression, error) {
	format, version := purpose.encoding()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if definition == nil || definition.SemanticVersion != version || purpose != rowStatistics && definition.SQL != "" || snapshot == nil {
		return nil, missing("index expression semantic encoding", 0)
	}
	if uint64(len(columns)) > uint64(limits.MaxOperands) || uint64(len(definition.Dependencies)) > uint64(limits.MaxOperands) {
		return nil, bindError("54000", "index expression metadata limit exceeded", 0)
	}
	encoding, ast, err := decodeRowExpression(ctx, definition.Encoding, limits, format, version)
	if err != nil {
		return nil, err
	}
	if encoding.TargetColumn != targetColumn || (encoding.Target != nil) != purpose.hasTarget() || encoding.ConstraintID != constraint {
		return nil, bindError("42804", "row expression purpose or target changed", 0)
	}
	if (purpose == rowCheck || purpose == rowDomainCheck) && (constraint == 0 || !encoding.Predicate) {
		return nil, bindError("42804", "CHECK expression constraint identity or purpose changed", 0)
	}
	if purpose == rowIndexKey && encoding.Predicate {
		purpose = rowIndexPredicate
	}
	if encoding.Target != nil {
		if purpose != rowStatistics && encoding.TargetColumn == 0 || purpose == rowStatistics && encoding.TargetColumn != 0 || !sameType(*encoding.Target, result) || encoding.Predicate {
			return nil, bindError("42804", "row expression purpose or target changed", 0)
		}
	} else if encoding.TargetColumn != 0 {
		return nil, bindError("42804", "row expression purpose or target changed", 0)
	}
	table, err := snapshot.Descriptor(ctx, encoding.Table)
	if err != nil {
		return nil, err
	}
	if encoding.Target != nil && purpose == rowStoredGenerated {
		b, err := newExpressionBinder(ctx, snapshot, Options{Limits: limits, Collation: encoding.Collation, DefaultCollation: encoding.DefaultCollation, Builtin: encoding.Builtin, SearchPath: encoding.SearchPath})
		if err != nil {
			return nil, err
		}
		spec, err := b.descriptorSpec(table.Descriptor)
		if err != nil {
			return nil, err
		}
		if spec.Table == nil {
			return nil, bindError("42804", "generated target table metadata missing", 0)
		}
		targetMatches := slices.ContainsFunc(spec.Table.Columns, func(c model.ColumnDescriptor) bool {
			identity := c.ID == encoding.TargetColumn && c.State == model.StatePublic
			return identity && c.Generated != nil && sameType(c.Layout, result)
		})
		if !targetMatches {
			return nil, bindError("42804", "generated target identity or type changed", 0)
		}
	}
	if purpose == rowDomainCheck {
		spec, err := table.Descriptor.SpecWithin(ctx, descriptorLimits(limits))
		if err != nil {
			return nil, err
		}
		if table.Descriptor.Kind() != model.ObjectType || table.Descriptor.State() != model.StatePublic || table.Descriptor.Version() < encoding.TableVersion || spec.Type == nil || spec.Type.DeclaredKind() != model.TypeKindDomain || len(encoding.Columns) != 1 || !sameType(spec.Type.BaseLayout, encoding.Columns[0].Layout) {
			return nil, bindError("0A000", "domain CHECK type identity changed", 0)
		}
	} else if table.Descriptor.Kind() != model.ObjectTable || table.Descriptor.State() != model.StatePublic || table.Descriptor.Version() < encoding.TableVersion {
		return nil, bindError("0A000", "row expression relation identity changed", 0)
	}
	var expected []model.Dependency
	seen := make(map[model.Dependency]bool, len(definition.Dependencies))
	for _, dep := range definition.Dependencies {
		if dep.Object == 0 || dep.Version == 0 || seen[dep] {
			return nil, bindError("22023", "invalid index expression dependency", 0)
		}
		seen[dep] = true
		if dep.Object == encoding.Table {
			if dep.Version != encoding.TableVersion {
				return nil, bindError("22023", "index expression table dependency differs from encoding", 0)
			}
			if dep.Column != 0 && !slices.ContainsFunc(encoding.Columns, func(p IndexExpressionParameter) bool { return p.Column == dep.Column }) {
				return nil, bindError("22023", "undeclared index expression column dependency", 0)
			}
			continue
		}
		expected = append(expected, dep)
	}
	validator, err := newExpressionBinder(ctx, snapshot, Options{Limits: limits, Collation: encoding.Collation, DefaultCollation: encoding.DefaultCollation, Builtin: encoding.Builtin, SearchPath: encoding.SearchPath})
	if err != nil {
		return nil, err
	}
	if err := validator.validateDefaultDependencies(expected, encoding.Identities, 0); err != nil {
		return nil, err
	}
	expected = validator.dependencies
	if !seen[model.Dependency{Object: encoding.Table, Version: encoding.TableVersion}] {
		return nil, bindError("22023", "missing index expression table dependency", 0)
	}
	for _, parameter := range encoding.Columns {
		if !seen[model.Dependency{Object: encoding.Table, Version: encoding.TableVersion, Column: parameter.Column}] {
			return nil, bindError("22023", "missing index expression column dependency", 0)
		}
		found := false
		for _, column := range columns {
			if column.ID != parameter.Column {
				continue
			}
			if found || !sameType(parameter.Layout, model.TypeLayout{Type: column.Type, Children: column.Children}) {
				return nil, bindError("42804", "index expression column type changed", 0)
			}
			found = true
		}
		if !found {
			return nil, bindError("42703", "index expression column no longer exists", 0)
		}
	}
	binding, actual, err := bindParameterizedRow(ctx, ast, encoding, snapshot, limits, nil, purpose, false, nil, nil)
	if err != nil {
		return nil, err
	}
	if len(actual) != len(expected) {
		return nil, bindError("0A000", "index expression dependency resolution changed", 0)
	}
	for _, dep := range actual {
		if !slices.Contains(expected, dep) {
			return nil, bindError("0A000", "index expression dependency resolution changed", 0)
		}
	}
	fingerprint, err := indexBindingFingerprint(ctx, binding.Graph)
	if err != nil {
		return nil, err
	}
	if fingerprint != encoding.Fingerprint {
		return nil, bindError("0A000", "index expression semantic resolution changed", 0)
	}
	if !sameType(binding.Result, result) {
		return nil, bindError("42804", "index expression result type changed", 0)
	}
	binding.Dependencies = appendIndexDependency(actual, model.Dependency{Object: encoding.Table, Version: table.Descriptor.Version()})
	for _, parameter := range encoding.Columns {
		binding.Dependencies = appendIndexDependency(binding.Dependencies, model.Dependency{Object: encoding.Table, Version: table.Descriptor.Version(), Column: parameter.Column})
	}
	return binding, nil
}

func bindParameterizedRow(ctx context.Context, ast *zpgparse.Node, encoding indexExpressionEncoding, snapshot CatalogSnapshot, limits ir.Limits, pending *model.Descriptor, purpose rowExpressionPurpose, freeze bool, declaration *declarationScope, temporal model.TemporalRuntime) (*BoundIndexExpression, []model.Dependency, error) {
	b, err := newExpressionBinder(ctx, snapshot, Options{SearchPath: encoding.SearchPath, Collation: encoding.Collation, DefaultCollation: encoding.DefaultCollation, Builtin: encoding.Builtin, Limits: limits, declaration: declaration, Temporal: temporal})
	if err != nil {
		return nil, nil, err
	}
	remaining, payload := limits.MaxNodes, limits.MaxBytes
	if err := b.checkAST(ast.ProtoReflect(), 0, &remaining, &payload); err != nil {
		return nil, nil, err
	}
	if err := ValidateStatementFeatures(ctx, ast); err != nil {
		return nil, nil, err
	}
	used := make([]bool, len(encoding.Columns))
	if err := rewriteIndexAST(ctx, ast.ProtoReflect(), func(node *zpgparse.Node) error {
		if node.GetColumnRef() != nil {
			return bindError("22023", "persistent index expression contains an unresolved column", 0)
		}
		if p := node.GetParamRef(); p != nil {
			if p.Number <= 0 || int(p.Number) > len(encoding.Columns) {
				return bindError("42P02", "undeclared index expression parameter", 0)
			}
			used[p.Number-1] = true
		}
		return nil
	}); err != nil {
		return nil, nil, err
	}
	layoutBytes := limits.MaxBytes
	parameters := make([]IndexExpressionParameter, len(encoding.Columns))
	for i, parameter := range encoding.Columns {
		if parameter.Column == 0 || !used[i] || slices.ContainsFunc(parameters[:i], func(p IndexExpressionParameter) bool { return p.Column == parameter.Column }) {
			return nil, nil, bindError("22023", "invalid index expression column declaration", 0)
		}
		owned, bytes, err := model.CopyTypeLayout(ctx, parameter.Layout, layoutBytes)
		if err != nil {
			return nil, nil, err
		}
		layoutBytes -= bytes
		parameters[i] = IndexExpressionParameter{Column: parameter.Column, Layout: owned}
		b.params[uint32(i+1)] = owned
	}
	if freeze {
		b.rowRegclassLiterals = make(map[*zpgparse.Node]model.OID)
		b.rowTemporalLiterals = make(map[*zpgparse.Node]string)
	}
	if len(encoding.Identities) != 0 {
		dependencies := make([]model.Dependency, 0, len(encoding.Identities))
		for _, identity := range encoding.Identities {
			descriptor, err := b.declarationDescriptor(identity.Object)
			if err != nil {
				return nil, nil, err
			}
			dependencies = append(dependencies, model.Dependency{Object: identity.Object, Version: descriptor.Version()})
		}
		if err := b.validateDefaultDependencies(dependencies, encoding.Identities, 0); err != nil {
			return nil, nil, err
		}
	}
	s, err := b.newScope(nil)
	if err != nil {
		return nil, nil, err
	}
	s.allowAggregate, s.allowWindow = false, false
	e, err := b.scalar(ast, s)
	if err != nil {
		return nil, nil, err
	}
	if purpose.hasTarget() {
		e, err = b.coerceColumnAssignment(e, s, model.ColumnDescriptor{Layout: *encoding.Target})
	} else if purpose == rowIndexPredicate || purpose == rowCheck || purpose == rowDomainCheck {
		e, err = b.coerce(s, e, b.primitive(16), ir.ImplicitCast)
	} else {
		e, err = b.defaultType(s, e)
	}
	if err != nil {
		return nil, nil, err
	}
	if purpose != rowCheck && purpose != rowDomainCheck {
		if err := b.requireKeyCollation(e); err != nil {
			return nil, nil, err
		}
		if e.effects.Volatility != ir.Immutable {
			return nil, nil, bindError("42P17", "index and generated expressions must be IMMUTABLE", e.position)
		}
	}
	selfReference := slices.ContainsFunc(encoding.Identities, func(identity routineDefaultIdentity) bool { return identity.Object == encoding.Table })
	if freeze && pending != nil {
		for _, oid := range b.rowRegclassLiterals {
			selfReference = selfReference || oid == pending.OID()
		}
	}
	for _, dep := range b.dependencies {
		if dep.Object == encoding.Table && !selfReference {
			return nil, nil, missing("index routine with table dependencies", 0)
		}
	}
	if err := b.rowParameterDependencies(encoding, pending, purpose); err != nil {
		return nil, nil, err
	}
	slot, err := b.slot(s, e.typ, e.nullable, 0, 0)
	if err != nil {
		return nil, nil, err
	}
	rel, err := b.builder.AddRel(ctx, ir.RelSpec{Rel: ir.Rel{Kind: ir.Values, Scope: s.id, Rows: 1, Effects: e.effects}, Outputs: []model.SlotID{slot}, Expressions: []ir.ScalarID{e.id}})
	if err != nil {
		return nil, nil, err
	}
	graph, err := b.builder.Build(ctx, rel)
	if err != nil {
		return nil, nil, err
	}
	var identities []routineDefaultIdentity
	if freeze {
		_, identities, err = b.defaultIdentityDependencies(graph)
		if err != nil {
			return nil, nil, err
		}
		for node, oid := range b.rowRegclassLiterals {
			if !slices.ContainsFunc(identities, func(identity routineDefaultIdentity) bool { return identity.OID == oid }) {
				continue
			}
			node.Node = &zpgparse.Node_TypeCast{TypeCast: &zpgparse.TypeCast{
				Arg: &zpgparse.Node{Node: &zpgparse.Node_AConst{AConst: &zpgparse.A_Const{
					Val: &zpgparse.A_Const_Sval{Sval: &zpgparse.String{Sval: strconv.FormatUint(uint64(oid), 10)}}, Location: -1,
				}}},
				TypeName: &zpgparse.TypeName{Names: []*zpgparse.Node{
					{Node: &zpgparse.Node_String_{String_: &zpgparse.String{Sval: "pg_catalog"}}},
					{Node: &zpgparse.Node_String_{String_: &zpgparse.String{Sval: "regclass"}}},
				}, Typemod: -1, Location: -1},
				Location: -1,
			}}
		}
		for node, text := range b.rowTemporalLiterals {
			if err := ctx.Err(); err != nil {
				return nil, nil, err
			}
			node.GetAConst().GetSval().Sval = text
		}
	}
	evaluation := builtin.ResolveOptions{DefaultCollation: encoding.DefaultCollation, ExtraFloatDigits: 1, TimeZone: encoding.Builtin.TimeZone}
	return &BoundIndexExpression{Graph: graph, Root: e.id, Parameters: parameters, Result: e.typ, Builtin: evaluation, identities: identities}, b.dependencies, nil
}

func rewriteIndexAST(ctx context.Context, message protoreflect.Message, visit func(*zpgparse.Node) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(message.GetUnknown()) != 0 {
		return bindError("22023", "unknown index expression AST fields", 0)
	}
	if node, ok := message.Interface().(*zpgparse.Node); ok {
		if node.GetSubLink() != nil || node.GetSelectStmt() != nil {
			return bindError("42P17", "subqueries are not allowed in index expressions", 0)
		}
		if err := visit(node); err != nil {
			return err
		}
	}
	fields := message.Descriptor().Fields()
	for i := range fields.Len() {
		field := fields.Get(i)
		if field.Name() == "location" && field.Kind() == protoreflect.Int32Kind {
			message.Set(field, protoreflect.ValueOfInt32(-1))
			continue
		}
		if !message.Has(field) {
			continue
		}
		if field.Kind() != protoreflect.MessageKind {
			continue
		}
		if field.IsList() {
			list := message.Get(field).List()
			for j := range list.Len() {
				if err := rewriteIndexAST(ctx, list.Get(j).Message(), visit); err != nil {
					return err
				}
			}
		} else if err := rewriteIndexAST(ctx, message.Get(field).Message(), visit); err != nil {
			return err
		}
	}
	return nil
}

func (b *binder) rowParameterDependencies(encoding indexExpressionEncoding, pending *model.Descriptor, purpose rowExpressionPurpose) error {
	if len(encoding.Columns) == 0 {
		return nil
	}
	descriptor := pending
	if descriptor == nil {
		published, err := b.catalog.Descriptor(b.ctx, encoding.Table)
		if err != nil {
			return err
		}
		descriptor = published.Descriptor
	}
	spec, err := b.descriptorSpec(descriptor)
	if err != nil {
		return err
	}
	unpublished := pending == nil && spec.State != model.StatePublic
	types := make(map[model.TypeID]bool)
	if purpose == rowDomainCheck {
		if spec.Type == nil || spec.Type.DeclaredKind() != model.TypeKindDomain || unpublished || spec.Version < encoding.TableVersion || len(encoding.Columns) != 1 || encoding.Columns[0].Column != 1 || !sameType(spec.Type.BaseLayout, encoding.Columns[0].Layout) {
			return bindError("0A000", "domain CHECK type identity changed", 0)
		}
		collectIndexTypes(spec.Type.BaseLayout, types)
	} else {
		if spec.Table == nil || unpublished || spec.Version < encoding.TableVersion {
			return bindError("0A000", "row expression relation identity changed", 0)
		}
		for _, parameter := range encoding.Columns {
			found := false
			for _, column := range spec.Table.Columns {
				if column.ID == parameter.Column && column.SystemIdentity {
					return bindError("42703", "column does not exist", 0)
				}
				if column.ID == parameter.Column && column.State == model.StatePublic {
					generatedReference := column.ID == encoding.TargetColumn || column.Generated != nil
					if purpose == rowStoredGenerated && generatedReference {
						return bindError("42P17", "generated expressions may reference only ordinary columns", 0)
					}
					found = sameType(column.Layout, parameter.Layout)
					break
				}
			}
			if !found {
				return bindError("42804", "row expression column identity or type changed", 0)
			}
			collectIndexTypes(parameter.Layout, types)
		}
	}
	if len(types) == 0 {
		return nil
	}
	for _, dependency := range spec.Dependencies {
		if err := b.ctx.Err(); err != nil {
			return err
		}
		descriptor, err := b.declarationDescriptor(dependency.Object)
		if err != nil {
			return err
		}
		id := model.TypeID(descriptor.OID())
		if descriptor.Kind() != model.ObjectType || !types[id] {
			continue
		}
		if descriptor.Version() != dependency.Version || descriptor.State() != model.StatePublic {
			return bindError("0A000", "index expression column type dependency changed", 0)
		}
		b.dependency(descriptor, 0)
		delete(types, id)
	}
	if len(types) != 0 {
		return missing("index column type descriptor dependency", 0)
	}
	return nil
}

func collectIndexTypes(layout model.TypeLayout, types map[model.TypeID]bool) {
	if _, known := builtin.LookupType(layout.Type.ID); !known {
		types[layout.Type.ID] = true
	}
	for _, child := range layout.Children {
		collectIndexTypes(child, types)
	}
}
