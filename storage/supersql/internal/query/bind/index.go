package bind

import (
	"context"
	"strconv"
	"strings"
	"unicode/utf8"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

// IndexExpressionCompiler admits actual executable kernels before an intent is
// durable. It borrows the binding only for the call and retains no mutable IR.
type IndexExpressionCompiler interface {
	CheckIndexExpression(context.Context, *BoundIndexExpression) error
}

type CreateIndexOptions struct {
	Bind               Options
	Descriptors        model.DescriptorLimits
	ObjectID           model.ObjectID
	OID                model.OID
	Physical           model.PhysicalIdentity
	Expressions        IndexExpressionCompiler
	PartitionHierarchy bool
}

// Existing is non-nil only for an explicit-name IF NOT EXISTS conflict; Index is
// then nil. No returned descriptor is modified or published by binding.
type BoundCreateIndex struct {
	Table       *model.Descriptor
	Index       *model.Descriptor
	Existing    *model.Descriptor
	Concurrent  bool
	IfNotExists bool
}

func BindCreateIndex(ctx context.Context, statement *zpgparse.IndexStmt, snapshot CatalogSnapshot, options CreateIndexOptions) (*BoundCreateIndex, error) {
	if err := options.Descriptors.Validate(); err != nil {
		return nil, err
	}
	b, err := newExpressionBinder(ctx, snapshot, options.Bind)
	if err != nil {
		return nil, err
	}
	if statement == nil || statement.Relation == nil || snapshot == nil {
		return nil, bindError("42601", "CREATE INDEX requires a table", 0)
	}
	remaining, payload := options.Bind.Limits.MaxNodes, options.Bind.Limits.MaxBytes
	if err := b.checkAST(statement.ProtoReflect(), 0, &remaining, &payload); err != nil {
		return nil, err
	}
	node := &zpgparse.Node{Node: &zpgparse.Node_IndexStmt{IndexStmt: statement}}
	if err := ValidateStatementFeatures(ctx, node); err != nil {
		return nil, err
	}
	if statement.Primary || statement.Isconstraint || statement.Deferrable || statement.Initdeferred || len(statement.ExcludeOpNames) != 0 {
		return nil, missing("constraint-backed CREATE INDEX", 0)
	}
	transformed := statement.Transformed || statement.IndexOid != 0 || statement.OldNumber != 0
	reusedStorage := statement.OldCreateSubid != 0 || statement.OldFirstRelfilelocatorSubid != 0
	derivedOptions := statement.ResetDefaultTblspc || statement.Idxcomment != ""
	if transformed || reusedStorage || derivedOptions {
		return nil, bindError("42601", "expected untransformed CREATE INDEX", 0)
	}
	rangeVar := statement.Relation
	if rangeVar.Catalogname != "" {
		return nil, bindError("0A000", "cross-database reference is not supported", int(rangeVar.Location)+1)
	}
	if rangeVar.Alias != nil {
		return nil, bindError("42601", "CREATE INDEX table cannot have an alias", int(rangeVar.Location)+1)
	}
	parts := []string{rangeVar.Relname}
	if rangeVar.Schemaname != "" {
		parts = []string{rangeVar.Schemaname, rangeVar.Relname}
	}
	table, _, err := b.resolve(parts, []model.ObjectKind{model.ObjectTable, model.ObjectView, model.ObjectMaterializedView}, "42P01", int(rangeVar.Location)+1)
	if err != nil {
		return nil, err
	}
	if table.Kind() != model.ObjectTable && table.Kind() != model.ObjectMaterializedView {
		return nil, bindError("42809", "cannot create an index on this relation", int(rangeVar.Location)+1)
	}
	spec, err := table.SpecWithin(ctx, options.Descriptors)
	if err != nil {
		return nil, err
	}
	if spec.Table == nil || len(spec.Table.PrimaryKey) == 0 {
		return nil, missing("CREATE INDEX without a physical primary key", 0)
	}
	if spec.Table.Partition != nil {
		return nil, missing("CREATE INDEX on locally partitioned storage", 0)
	}
	if spec.Table.PartitionStrategy != 0 && !options.PartitionHierarchy {
		return nil, missing("partitioned CREATE INDEX orchestration", 0)
	}
	result := &BoundCreateIndex{Table: table, Concurrent: statement.Concurrent, IfNotExists: statement.IfNotExists}
	if statement.Idxname != "" {
		existing, err := b.indexNameConflict(table.Name().Parent, statement.Idxname)
		if err != nil {
			return nil, err
		}
		if existing != nil {
			if !statement.IfNotExists {
				return nil, bindError("42P07", "relation already exists", 0)
			}
			result.Existing = existing
			return result, nil
		}
	}
	if len(statement.IndexParams) == 0 {
		return nil, bindError("42601", "index requires at least one key", 0)
	}
	if len(statement.IndexParams) > options.Descriptors.MaxItems || len(statement.IndexIncludingParams) > options.Descriptors.MaxItems-len(statement.IndexParams) {
		return nil, model.ErrDescriptorLimit
	}
	if statement.NullsNotDistinct && !statement.Unique {
		return nil, bindError("42601", "NULLS NOT DISTINCT requires a unique index", 0)
	}
	if spec.Table.Owner == 0 {
		return nil, model.ErrInvalidDescriptor
	}
	metadata := &model.IndexMetadata{Owner: spec.Table.Owner, Table: table.ID(), Unique: statement.Unique, NullsNotDistinct: statement.NullsNotDistinct}
	if statement.Concurrent {
		metadata.Invalid = true
		metadata.NotReady = true
	}
	dependencies := append([]model.Dependency(nil), b.dependencies...)
	definitionBytes := options.Descriptors.MaxBytes
	for _, raw := range statement.IndexParams {
		elem := raw.GetIndexElem()
		if elem == nil || (elem.Name == "") == (elem.Expr == nil) || elem.Indexcolname != "" {
			return nil, bindError("42601", "invalid index key", 0)
		}
		key := model.KeyElement{}
		switch elem.Ordering {
		case zpgparse.SortByDir_SORTBY_DEFAULT, zpgparse.SortByDir_SORTBY_ASC:
		case zpgparse.SortByDir_SORTBY_DESC:
			key.Descending = true
		default:
			return nil, missing("index ordering operator", 0)
		}
		key.NullsFirst = key.Descending
		switch elem.NullsOrdering {
		case zpgparse.SortByNulls_SORTBY_NULLS_DEFAULT:
		case zpgparse.SortByNulls_SORTBY_NULLS_FIRST:
			key.NullsFirst = true
		case zpgparse.SortByNulls_SORTBY_NULLS_LAST:
			key.NullsFirst = false
		default:
			return nil, bindError("42601", "invalid index NULL ordering", 0)
		}
		var layout model.TypeLayout
		if elem.Name != "" {
			col, err := publicIndexColumn(spec.Table.Columns, elem.Name)
			if err != nil {
				return nil, err
			}
			key.Column, layout = col.ID, col.Layout
			dependencies = appendIndexDependency(dependencies, model.Dependency{Object: table.ID(), Version: table.Version(), Column: col.ID})
			if len(elem.Collation) != 0 {
				layout, err = b.indexColumnCollation(layout, elem.Collation)
				if err != nil {
					return nil, err
				}
			}
		} else {
			ast := elem.Expr
			if len(elem.Collation) != 0 {
				ast = &zpgparse.Node{Node: &zpgparse.Node_CollateClause{CollateClause: &zpgparse.CollateClause{Arg: ast, Collname: elem.Collation, Location: -1}}}
			}
			definition, binding, err := defineIndexExpression(ctx, ast, table, spec.Table.Columns, snapshot, options.Bind, false)
			if err != nil {
				return nil, err
			}
			if err := admitIndexDefinition(definition, &definitionBytes); err != nil {
				return nil, err
			}
			if options.Expressions == nil {
				return nil, missing("immutable index expression compiler", 0)
			}
			if err := options.Expressions.CheckIndexExpression(ctx, binding); err != nil {
				return nil, err
			}
			key.Expression, layout = definition, binding.Result
			key.ExpressionName = clipIndexIdentifier(indexAttributeName(elem), 63)
			for _, dep := range definition.Dependencies {
				dependencies = appendIndexDependency(dependencies, dep)
			}
		}
		if layout.Type.Storage == model.TypeVectorF32 || layout.Type.Storage == model.TypeAny {
			return nil, missing("index key physical type", 0)
		}
		key.Type, key.Children, key.Collation = layout.Type, layout.Children, layout.Type.Collation
		if len(elem.Opclass) != 0 {
			operatorType, typeErr := b.indexOperatorClassType(layout)
			if typeErr != nil {
				return nil, typeErr
			}
			key.OperatorClass, err = indexOperatorClass(operatorType, elem.Opclass)
			if err != nil {
				return nil, err
			}
		}
		metadata.Keys = append(metadata.Keys, key)
	}
	for _, raw := range statement.IndexIncludingParams {
		elem := raw.GetIndexElem()
		if elem == nil {
			return nil, bindError("42601", "INCLUDE requires plain column names", 0)
		}
		notPlain := elem.Name == "" || elem.Expr != nil || elem.Indexcolname != ""
		customComparison := len(elem.Collation) != 0 || len(elem.Opclass) != 0 || len(elem.Opclassopts) != 0
		ordered := elem.Ordering != zpgparse.SortByDir_SORTBY_DEFAULT || elem.NullsOrdering != zpgparse.SortByNulls_SORTBY_NULLS_DEFAULT
		if notPlain || customComparison || ordered {
			return nil, bindError("42601", "INCLUDE requires plain column names", 0)
		}
		col, err := publicIndexColumn(spec.Table.Columns, elem.Name)
		if err != nil {
			return nil, err
		}
		metadata.Include = append(metadata.Include, col.ID)
		dependencies = appendIndexDependency(dependencies, model.Dependency{Object: table.ID(), Version: table.Version(), Column: col.ID})
	}
	if statement.WhereClause != nil {
		definition, binding, err := defineIndexExpression(ctx, statement.WhereClause, table, spec.Table.Columns, snapshot, options.Bind, true)
		if err != nil {
			return nil, err
		}
		if err := admitIndexDefinition(definition, &definitionBytes); err != nil {
			return nil, err
		}
		if options.Expressions == nil {
			return nil, missing("immutable index expression compiler", 0)
		}
		if err := options.Expressions.CheckIndexExpression(ctx, binding); err != nil {
			return nil, err
		}
		metadata.Predicate = definition
		for _, dep := range definition.Dependencies {
			dependencies = appendIndexDependency(dependencies, dep)
		}
	}
	name := statement.Idxname
	if name == "" {
		name, err = b.generatedIndexName(table.Name().Parent, table.Name().Name, statement.IndexParams, statement.IndexIncludingParams)
		if err != nil {
			return nil, err
		}
	}
	result.Index, err = model.NewDescriptor(ctx, model.DescriptorSpec{ID: options.ObjectID, OID: options.OID, Name: model.NamespaceName{Parent: table.Name().Parent, Kind: model.ObjectIndex, Name: name}, Version: 1, State: model.StateAbsent, Physical: options.Physical, Dependencies: dependencies, Index: metadata}, options.Descriptors)
	if err != nil {
		return nil, err
	}
	if result.Index.ID() == table.ID() {
		return nil, bindError("22023", "index and table identities must differ", 0)
	}
	return result, ctx.Err()
}

func publicIndexColumn(columns []model.ColumnDescriptor, name string) (model.ColumnDescriptor, error) {
	for _, column := range columns {
		if column.Name == name && column.State == model.StatePublic && !column.SystemIdentity {
			return column, nil
		}
	}
	return model.ColumnDescriptor{}, bindError("42703", "index column does not exist", 0)
}

func (b *binder) indexColumnCollation(layout model.TypeLayout, names []*zpgparse.Node) (model.TypeLayout, error) {
	s, err := b.newScope(nil)
	if err != nil {
		return layout, err
	}
	b.params[1] = layout
	arg := &zpgparse.Node{Node: &zpgparse.Node_ParamRef{ParamRef: &zpgparse.ParamRef{Number: 1, Location: -1}}}
	e, err := b.collate(&zpgparse.CollateClause{Arg: arg, Collname: names, Location: -1}, s)
	return e.typ, err
}

func (b *binder) indexNameConflict(parent model.ObjectID, name string) (*model.Descriptor, error) {
	if name == "" || len(name) > 63 || !utf8.ValidString(name) || strings.IndexByte(name, 0) >= 0 {
		return nil, bindError("42622", "invalid index identifier", 0)
	}
	for _, kind := range [...]model.ObjectKind{model.ObjectTable, model.ObjectIndex, model.ObjectSequence, model.ObjectView, model.ObjectMaterializedView, model.ObjectCompositeRelation} {
		p, err := b.catalog.Resolve(b.ctx, model.NamespaceName{Parent: parent, Kind: kind, Name: name})
		if isMissingObject(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return p.Descriptor, nil
	}
	return nil, nil
}

func (b *binder) generatedIndexName(parent model.ObjectID, table string, keys, included []*zpgparse.Node) (string, error) {
	columns := indexNameAddition(keys, included)
	for attempt := uint32(0); attempt < b.options.Limits.MaxNodes; attempt++ {
		label := "idx"
		if attempt != 0 {
			label += strconv.FormatUint(uint64(attempt), 10)
		}
		a, c := table, columns
		for len(a)+len(c)+len(label)+2 > 63 {
			if len(a) > len(c) {
				_, size := utf8.DecodeLastRuneInString(a)
				a = a[:len(a)-size]
			} else {
				_, size := utf8.DecodeLastRuneInString(c)
				c = c[:len(c)-size]
			}
		}
		name := a + "_" + c + "_" + label
		conflict, err := b.indexNameConflict(parent, name)
		if err != nil {
			return "", err
		}
		if conflict == nil {
			return name, nil
		}
	}
	return "", bindError("54000", "generated index name search limit exceeded", 0)
}

func appendIndexDependency(deps []model.Dependency, dep model.Dependency) []model.Dependency {
	for _, existing := range deps {
		if existing == dep {
			return deps
		}
	}
	return append(deps, dep)
}

func admitIndexDefinition(definition *model.ExpressionDefinition, remaining *uint64) error {
	cost := uint64(unsafe.Sizeof(*definition)) + uint64(len(definition.Encoding)) + uint64(len(definition.Dependencies))*uint64(unsafe.Sizeof(model.Dependency{}))
	if cost > *remaining {
		return model.ErrDescriptorLimit
	}
	*remaining -= cost
	return nil
}
func newExpressionBinder(ctx context.Context, snapshot CatalogSnapshot, options Options) (*binder, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if options.Collation == 0 || options.DefaultCollation == 0 || options.DefaultCollation == 100 {
		return nil, bindError("22023", "binding requires resolved collation settings", 0)
	}
	if err := ValidateBuiltinCollation(options.Collation, 0); err != nil {
		return nil, err
	}
	if err := ValidateBuiltinCollation(options.DefaultCollation, 0); err != nil {
		return nil, err
	}
	builder, err := ir.NewBuilder(options.Limits)
	if err != nil {
		return nil, err
	}
	if uint64(len(options.SearchPath)) > uint64(options.Limits.MaxOperands) {
		return nil, bindError("54000", "search path limit exceeded", 0)
	}
	remaining := options.Limits.MaxBytes
	for _, name := range options.SearchPath {
		if name == "" {
			return nil, bindError("22023", "empty search path entry", 0)
		}
		if uint64(len(name)) > remaining {
			return nil, bindError("54000", "search path byte limit exceeded", 0)
		}
		remaining -= uint64(len(name))
	}
	return &binder{ctx: ctx, catalog: snapshot, options: options, builder: builder, params: make(map[uint32]model.TypeLayout), layouts: make(map[model.Type]ir.LayoutID), views: make(map[model.ObjectID]bool), dependencySet: make(map[model.Dependency]struct{}), slotScopes: make(map[model.SlotID]ir.ScopeID), captures: make(map[ir.ScopeID][]model.SlotID)}, nil
}
