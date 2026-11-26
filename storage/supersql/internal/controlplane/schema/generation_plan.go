package schema

import (
	"bytes"
	"context"
	"errors"
	"math"
	"slices"
	"sort"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type GenerationExpressions interface {
	ColumnExpressions
	CompileGenerationTransform(context.Context, *model.ExpressionDefinition, model.ColumnID, []storage.ColumnLayout, model.TypeLayout) (txn.ValueExpression, error)
}

type GenerationExpressionFactory func(context.Context, *catalog.CatalogSnapshot, JobDefinition) (GenerationExpressions, error)
type generationColumnsOnly struct{ ColumnExpressions }

func (generationColumnsOnly) CompileGenerationTransform(context.Context, *model.ExpressionDefinition, model.ColumnID, []storage.ColumnLayout, model.TypeLayout) (txn.ValueExpression, error) {
	return nil, errColumnExpressions
}

type generationColumn struct {
	declaration model.ColumnDescriptor
	source      int
	transform   txn.ValueExpression
	generated   txn.ValueExpression
	virtual     bool
}

// GenerationPlan owns the fixed source/target layouts and borrows immutable
// expression programs. It never consults the current catalog while expanding a
// write, so every statement uses the policy pinned by its schema intent.
type GenerationPlan struct {
	source             storage.ObjectRef
	target             storage.ObjectRef
	sourceTable        *model.Descriptor
	targetTable        *model.Descriptor
	sourceColumns      []storage.ColumnLayout
	sourceKeys         []storage.KeyColumn
	targetColumns      []generationColumn
	targetLayout       []storage.ColumnLayout
	targetStoredLayout []storage.ColumnLayout
	targetKeys         []storage.KeyColumn
	targetKeyColumn    []int
	targetPolicy       *txn.RowNormalizer
	targetIndexes      *IndexSet
	encoder            storage.KeyEncoder
	rowBytes           uint64
	workspaceBytes     uint64
}

func NewGenerationPlan(ctx context.Context, definition JobDefinition, encoder storage.KeyEncoder, expressions GenerationExpressions, indexExpressions IndexExpressions, limits IndexLimits) (*GenerationPlan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if definition.Operation != SchemaAddColumn && definition.Operation != SchemaDropColumn && definition.Operation != SchemaAlterColumnType && definition.Operation != SchemaRewritePrimaryKey && definition.Operation != SchemaAlterColumnExpression && definition.Operation != SchemaCreateIndexTransactional && definition.Operation != SchemaReindex {
		return nil, errJobDefinition
	}
	if mutationPolicyNil(encoder) || limits.MetadataBytes == 0 || limits.EntryBytes == 0 || limits.MaxColumns <= 0 || uint64(limits.MaxColumns) > math.MaxUint32 {
		return nil, errIndexPolicyOptions
	}
	sourceTarget, err := descriptorTarget(definition.SourceTable)
	if err != nil {
		return nil, err
	}
	targetTarget, err := descriptorTarget(definition.TargetTable)
	if err != nil {
		return nil, err
	}
	if sourceTarget.ObjectID != targetTarget.ObjectID || sourceTarget.Generation >= targetTarget.Generation {
		return nil, errJobDefinition
	}
	descriptorLimits := model.DescriptorLimits{MaxBytes: limits.MetadataBytes, MaxItems: int(min(limits.MetadataBytes, uint64(math.MaxInt))), MaxDepth: 64}
	sourceSpec, err := definition.SourceTable.SpecWithin(ctx, descriptorLimits)
	if err != nil {
		return nil, err
	}
	targetSpec, err := definition.TargetTable.SpecWithin(ctx, descriptorLimits)
	if err != nil {
		return nil, err
	}
	if sourceSpec.Table == nil || targetSpec.Table == nil || len(sourceSpec.Table.Columns) == 0 || len(targetSpec.Table.Columns) == 0 {
		return nil, errJobDefinition
	}
	slices.SortFunc(sourceSpec.Table.Columns, func(a, b model.ColumnDescriptor) int {
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	slices.SortFunc(targetSpec.Table.Columns, func(a, b model.ColumnDescriptor) int {
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	sourceColumns := slices.DeleteFunc(slices.Clone(sourceSpec.Table.Columns), func(column model.ColumnDescriptor) bool {
		return column.Generated != nil && !column.GeneratedStored
	})
	targetColumns := targetSpec.Table.Columns
	if len(sourceColumns) > limits.MaxColumns || len(targetColumns) > limits.MaxColumns {
		return nil, ErrJobLimit
	}
	plan := &GenerationPlan{
		source: sourceTarget, target: targetTarget, sourceTable: definition.SourceTable,
		targetTable: definition.TargetTable, encoder: encoder, rowBytes: limits.EntryBytes,
		sourceColumns: make([]storage.ColumnLayout, len(sourceColumns)),
		targetColumns: make([]generationColumn, len(targetColumns)),
		targetLayout:  make([]storage.ColumnLayout, len(targetColumns)),
	}
	for i, column := range sourceColumns {
		if column.State != model.StatePublic || column.ID == 0 || i > 0 && sourceColumns[i-1].ID == column.ID {
			return nil, errJobDefinition
		}
		plan.sourceColumns[i] = storage.ColumnLayout{ID: column.ID, Type: column.Layout.Type, Children: column.Layout.Children, Nullable: column.Nullable}
		if column.ConstantDefault != nil {
			plan.sourceColumns[i].HasMissingValue, plan.sourceColumns[i].MissingValue = true, *column.ConstantDefault
		}
	}
	transforms := make(map[model.ColumnID]*model.ExpressionDefinition, len(definition.Transforms))
	for _, transform := range definition.Transforms {
		transforms[transform.Column] = transform.Expression
	}
	for i, column := range targetColumns {
		if column.State != model.StatePublic || column.ID == 0 || i > 0 && targetColumns[i-1].ID == column.ID {
			return nil, errJobDefinition
		}
		layout := storage.ColumnLayout{ID: column.ID, Type: column.Layout.Type, Children: column.Layout.Children, Nullable: column.Nullable}
		if column.ConstantDefault != nil {
			layout.HasMissingValue, layout.MissingValue = true, *column.ConstantDefault
		}
		plan.targetLayout[i] = layout
		plan.targetColumns[i] = generationColumn{declaration: column, source: -1, virtual: column.Generated != nil && !column.GeneratedStored}
		position := sort.Search(len(sourceColumns), func(j int) bool { return sourceColumns[j].ID >= column.ID })
		if position < len(sourceColumns) && sourceColumns[position].ID == column.ID {
			plan.targetColumns[i].source = position
		}
		transform := transforms[column.ID]
		if transform != nil {
			if mutationPolicyNil(expressions) {
				return nil, errColumnExpressions
			}
			program, err := expressions.CompileGenerationTransform(ctx, transform, column.ID, plan.sourceColumns, column.Layout)
			if err != nil {
				return nil, err
			}
			if mutationPolicyNil(program) {
				return nil, errColumnExpressions
			}
			plan.targetColumns[i].transform = program
		}
		if column.Generated != nil {
			if mutationPolicyNil(expressions) {
				return nil, errColumnExpressions
			}
			program, err := expressions.CompileStoredGenerated(ctx, column.Generated, column.ID, plan.targetLayout, column.Layout)
			if err != nil {
				return nil, err
			}
			if mutationPolicyNil(program) {
				return nil, errColumnExpressions
			}
			plan.targetColumns[i].generated = program
		}
		if plan.targetColumns[i].source < 0 && plan.targetColumns[i].transform == nil && plan.targetColumns[i].generated == nil && column.ConstantDefault == nil && !column.Nullable && definition.Operation != SchemaAddColumn {
			return nil, errJobDefinition
		}
		if plan.targetColumns[i].source >= 0 && plan.targetColumns[i].transform == nil {
			source := sourceColumns[plan.targetColumns[i].source]
			if source.Layout.Type != column.Layout.Type || !storage.KeyLayoutsEqual([]storage.KeyColumn{{ColumnID: column.ID, Type: source.Layout.Type, Children: source.Layout.Children}}, []storage.KeyColumn{{ColumnID: column.ID, Type: column.Layout.Type, Children: column.Layout.Children}}) {
				return nil, errJobDefinition
			}
		}
	}
	plan.targetStoredLayout = make([]storage.ColumnLayout, 0, len(plan.targetLayout))
	for i, layout := range plan.targetLayout {
		if !plan.targetColumns[i].virtual {
			plan.targetStoredLayout = append(plan.targetStoredLayout, layout)
		}
	}
	columns, err := generationColumnsFactory(ctx, definition, expressions)
	if err != nil {
		return nil, err
	}
	sourcePolicy, err := NewTablePolicy(ctx, TablePolicyOptions{Descriptor: definition.SourceTable, Encoder: encoder, Expressions: columns, MetadataBytes: limits.MetadataBytes, RowBytes: limits.EntryBytes, MaxColumns: limits.MaxColumns})
	if err != nil {
		return nil, err
	}
	plan.sourceKeys = sourcePolicy.KeyLayout()
	plan.targetPolicy, err = NewTablePolicy(ctx, TablePolicyOptions{Descriptor: definition.TargetTable, Encoder: encoder, Expressions: columns, MetadataBytes: limits.MetadataBytes, RowBytes: limits.EntryBytes, MaxColumns: limits.MaxColumns})
	if err != nil {
		return nil, err
	}
	plan.targetKeys = plan.targetPolicy.KeyLayout()
	plan.targetKeyColumn = make([]int, len(plan.targetKeys))
	for i, key := range plan.targetKeys {
		position := sort.Search(len(plan.targetLayout), func(j int) bool { return plan.targetLayout[j].ID >= key.ColumnID })
		if position == len(plan.targetLayout) || plan.targetLayout[position].ID != key.ColumnID {
			return nil, errJobDefinition
		}
		plan.targetKeyColumn[i] = position
	}
	targetIndexes := definition.TargetIndexes
	indexTable := definition.TargetTable
	if len(definition.SourceIndexes) == len(definition.TargetIndexes) {
		targetIndexes = make([]*model.Descriptor, 0, len(definition.TargetIndexes))
		for i, target := range definition.TargetIndexes {
			if target.Physical().Generation > definition.SourceIndexes[i].Physical().Generation {
				targetIndexes = append(targetIndexes, target)
			}
		}
		if len(targetIndexes) != len(definition.TargetIndexes) {
			indexTableSpec := targetSpec
			indexTableSpec.Table.Indexes = make([]model.ObjectID, len(targetIndexes))
			for i, target := range targetIndexes {
				indexTableSpec.Table.Indexes[i] = target.ID()
			}
			indexTableSpec.Table.Constraints = slices.DeleteFunc(indexTableSpec.Table.Constraints, func(constraint model.ConstraintDescriptor) bool {
				switch constraint.Kind {
				case model.ConstraintPrimaryKey, model.ConstraintUnique, model.ConstraintExclude:
					return !slices.Contains(indexTableSpec.Table.Indexes, constraint.Index)
				default:
					return false
				}
			})
			indexTable, err = model.NewDescriptor(ctx, indexTableSpec, descriptorLimits)
			if err != nil {
				return nil, err
			}
		}
	}
	indexPlans := make([]*IndexPlan, len(targetIndexes))
	for i, descriptor := range targetIndexes {
		indexPlans[i], err = NewIndexPlan(ctx, indexTable, descriptor, encoder, indexExpressions, limits)
		if err != nil {
			return nil, err
		}
	}
	plan.targetIndexes, err = NewIndexSet(ctx, indexTable, indexPlans, limits)
	if err != nil {
		return nil, err
	}
	fixed := uint64(unsafe.Sizeof(GenerationPlan{})) + uint64(len(plan.sourceColumns)+len(plan.targetLayout)+len(plan.targetStoredLayout))*uint64(unsafe.Sizeof(storage.ColumnLayout{})) + uint64(len(plan.sourceKeys)+len(plan.targetKeys))*uint64(unsafe.Sizeof(storage.KeyColumn{})) + uint64(len(plan.targetColumns))*uint64(unsafe.Sizeof(generationColumn{}))
	if limits.EntryBytes > math.MaxUint64/4 || fixed > math.MaxUint64-4*limits.EntryBytes {
		return nil, ErrJobLimit
	}
	plan.workspaceBytes = fixed + 4*limits.EntryBytes
	if plan.targetIndexes.WorkspaceBytes() > math.MaxUint64-plan.workspaceBytes {
		return nil, ErrJobLimit
	}
	plan.workspaceBytes += plan.targetIndexes.WorkspaceBytes()
	maximum := limits.MetadataBytes
	if limits.EntryBytes > (math.MaxUint64-maximum)/8 {
		return nil, ErrJobLimit
	}
	maximum += 8 * limits.EntryBytes
	if plan.workspaceBytes > maximum {
		return nil, ErrJobLimit
	}
	return plan, nil
}

func generationColumnsFactory(ctx context.Context, definition JobDefinition, expressions GenerationExpressions) (ColumnExpressions, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if expressions == nil {
		spec, err := definition.TargetTable.Spec(ctx)
		if err != nil {
			return nil, err
		}
		for _, column := range spec.Table.Columns {
			if column.Default != nil || column.Generated != nil || column.IdentitySequence != 0 {
				return nil, errColumnExpressions
			}
		}
		return nil, nil
	}
	return expressions, nil
}

func (p *GenerationPlan) Source() storage.ObjectRef { return p.source }
func (p *GenerationPlan) Target() storage.ObjectRef { return p.target }
func (p *GenerationPlan) SourceColumns() []storage.ColumnLayout {
	if p == nil {
		return nil
	}
	return slices.Clone(p.sourceColumns)
}
func (p *GenerationPlan) SourceKeyLayout() []storage.KeyColumn {
	if p == nil {
		return nil
	}
	return slices.Clone(p.sourceKeys)
}
func (p *GenerationPlan) WorkspaceBytes() uint64 {
	if p == nil {
		return 0
	}
	return p.workspaceBytes
}
func (p *GenerationPlan) Layouts() []storage.ObjectLayout {
	if p == nil {
		return nil
	}
	result := []storage.ObjectLayout{p.targetPolicy.Layout()}
	result = append(result, p.targetIndexes.Layouts()...)
	return result
}
func (p *GenerationPlan) WithBaseResolver(base txn.MutationResolver) txn.MutationResolver {
	if p == nil {
		return base
	}
	return p.targetIndexes.WithBaseResolver(tablePolicyBinding{target: p.target, normalizer: p.targetPolicy, fallback: base})
}

func (p *GenerationPlan) ValidateDescriptor(ctx context.Context, table *model.Descriptor) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p == nil || table == nil || table.ID() != p.source.ObjectID || table.State() != model.StatePublic {
		return errJobDefinition
	}
	physical := table.Physical()
	if physical.Generation != p.source.Generation || physical.EncodingVersion != p.source.EncodingVersion || table.Version() < p.source.DescriptorVersion || table.Version() >= p.target.DescriptorVersion {
		return errMutationDescriptorChanged
	}
	return nil
}

func (p *GenerationPlan) transform(ctx context.Context, source *model.Row) (*model.Row, error) {
	return p.transformWithRuntime(ctx, source, model.ExpressionRuntime{})
}

func (p *GenerationPlan) transformWithRuntime(ctx context.Context, source *model.Row, runtime model.ExpressionRuntime) (*model.Row, error) {
	if source == nil {
		return nil, nil
	}
	if source.Deleted || len(source.Values) != len(p.sourceColumns) {
		return nil, storage.ErrInvalidLayout
	}
	remaining := p.rowBytes
	values := make([]model.Value, len(p.targetColumns))
	for i, column := range p.targetColumns {
		if column.generated != nil {
			values[i] = model.NullVal(column.declaration.Layout.Type.Storage)
			continue
		}
		var value model.Value
		var err error
		switch {
		case column.transform != nil:
			value, err = column.transform.Evaluate(ctx, source.Values, runtime)
		case column.source >= 0:
			value, _, err = model.CopyValue(ctx, source.Values[column.source], remaining)
		case column.declaration.ConstantDefault != nil:
			value, _, err = model.CopyValue(ctx, *column.declaration.ConstantDefault, remaining)
		case column.declaration.Nullable:
			value = model.NullVal(column.declaration.Layout.Type.Storage)
		default:
			return nil, errJobDefinition
		}
		if err != nil {
			return nil, err
		}
		used, err := model.ValueBytes(ctx, value, remaining)
		if err != nil {
			return nil, err
		}
		remaining -= used
		values[i] = value
	}
	for i, column := range p.targetColumns {
		if column.generated == nil {
			continue
		}
		value, err := column.generated.Evaluate(ctx, values, runtime)
		if err != nil {
			return nil, err
		}
		used, err := model.ValueBytes(ctx, value, remaining)
		if err != nil {
			return nil, err
		}
		remaining -= used
		values[i] = value
	}
	if err := p.targetPolicy.ValidateRow(ctx, &model.Row{Values: values}); err != nil {
		return nil, err
	}
	for i, value := range values {
		layout := model.TypeLayout{Type: p.targetLayout[i].Type, Children: p.targetLayout[i].Children}
		if err := storage.ValidateStoredValue(ctx, value, layout); err != nil {
			return nil, err
		}
	}
	keyValues := make([]model.Value, len(p.targetKeys))
	for i, position := range p.targetKeyColumn {
		keyValues[i] = values[position]
	}
	key, err := p.encoder.EncodePrefix(ctx, p.targetKeys, keyValues, remaining)
	if err != nil {
		return nil, err
	}
	return &model.Row{Key: key, Values: values}, nil
}

func (p *GenerationPlan) Expand(ctx context.Context, transaction txn.MutationApplier, old, current *model.Row) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p == nil || transaction == nil {
		return storage.ErrInvalidMutation
	}
	previous, err := p.transform(ctx, old)
	if err != nil {
		return err
	}
	next, err := p.transform(ctx, current)
	if err != nil {
		return err
	}
	return p.expand(ctx, transaction, previous, next)
}

func (p *GenerationPlan) expand(ctx context.Context, transaction txn.MutationApplier, previous, next *model.Row) error {
	if previous != nil && (next == nil || !bytes.Equal(previous.Key, next.Key)) {
		if err := p.targetIndexes.Expand(ctx, transaction, previous, nil); err != nil {
			return err
		}
		if err := transaction.ApplyMutations(ctx, []storage.Mutation{{Op: storage.MutationDelete, Target: p.target, Key: previous.Key}}); err != nil && !errors.Is(err, storage.ErrRowNotFound) {
			return err
		}
	}
	if next == nil {
		return nil
	}
	columnIDs := make([]model.ColumnID, 0, len(p.targetColumns))
	values := make([]model.Value, 0, len(p.targetColumns))
	for i, column := range p.targetColumns {
		if column.virtual {
			continue
		}
		columnIDs = append(columnIDs, column.declaration.ID)
		values = append(values, next.Values[i])
	}
	mutation := storage.Mutation{Op: storage.MutationUpsert, Target: p.target, Key: next.Key, ColumnIDs: columnIDs, Values: values}
	if err := transaction.ApplyMutations(ctx, []storage.Mutation{mutation}); err != nil {
		return err
	}
	if previous != nil && bytes.Equal(previous.Key, next.Key) {
		return p.targetIndexes.Expand(ctx, transaction, previous, next)
	}
	return p.targetIndexes.Expand(ctx, transaction, nil, next)
}
