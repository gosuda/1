package bind

import (
	"context"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func partitionStrategy(strategy zpgparse.PartitionStrategy) (byte, string, error) {
	switch strategy {
	case zpgparse.PartitionStrategy_PARTITION_STRATEGY_HASH:
		return 'h', "hash", nil
	case zpgparse.PartitionStrategy_PARTITION_STRATEGY_LIST:
		return 'l', "btree", nil
	case zpgparse.PartitionStrategy_PARTITION_STRATEGY_RANGE:
		return 'r', "btree", nil
	default:
		return 0, "", bindError("42601", "invalid partition strategy", 0)
	}
}

func (b *binder) bindPartitionKeys(ctx context.Context, specification *zpgparse.PartitionSpec, table *model.Descriptor, columns []model.ColumnDescriptor, snapshot CatalogSnapshot, options CreateDeclarationOptions) (byte, []model.KeyElement, error) {
	if specification == nil {
		return 0, nil, nil
	}
	strategy, method, err := partitionStrategy(specification.Strategy)
	if err != nil {
		return 0, nil, err
	}
	if len(specification.PartParams) == 0 || len(specification.PartParams) > options.Descriptors.MaxItems {
		return 0, nil, model.ErrDescriptorLimit
	}
	if strategy == 'l' && len(specification.PartParams) != 1 {
		return 0, nil, bindError("0A000", "LIST partitioning requires exactly one key", int(specification.Location)+1)
	}
	keys := make([]model.KeyElement, 0, len(specification.PartParams))
	remaining := options.Descriptors.MaxBytes
	for _, node := range specification.PartParams {
		if err := ctx.Err(); err != nil {
			return 0, nil, err
		}
		element := node.GetPartitionElem()
		if element == nil || (element.Name == "") == (element.Expr == nil) {
			return 0, nil, bindError("42601", "invalid partition key", int(specification.Location)+1)
		}
		var key model.KeyElement
		var layout model.TypeLayout
		if element.Name != "" {
			column, err := publicIndexColumn(columns, element.Name)
			if err != nil {
				return 0, nil, err
			}
			key.Column, layout = column.ID, column.Layout
			b.dependency(table, column.ID)
			if len(element.Collation) != 0 {
				layout, err = b.indexColumnCollation(layout, element.Collation)
				if err != nil {
					return 0, nil, err
				}
			}
		} else {
			ast := element.Expr
			if len(element.Collation) != 0 {
				ast = &zpgparse.Node{Node: &zpgparse.Node_CollateClause{CollateClause: &zpgparse.CollateClause{Arg: ast, Collname: element.Collation, Location: -1}}}
			}
			definition, expression, err := defineIndexExpression(ctx, ast, table, columns, snapshot, options.Bind, false)
			if err != nil {
				return 0, nil, err
			}
			if err := admitIndexDefinition(definition, &remaining); err != nil {
				return 0, nil, err
			}
			key.Expression, layout = definition, expression.Result
		}
		if layout.Type.Storage == model.TypeAny || layout.Type.Storage == model.TypeVectorF32 {
			return 0, nil, missing("partition key physical type", int(element.Location)+1)
		}
		operatorType, err := b.indexOperatorClassType(layout)
		if err != nil {
			return 0, nil, err
		}
		if len(element.Opclass) != 0 {
			key.OperatorClass, err = partitionOperatorClass(method, operatorType, element.Opclass)
			if err != nil {
				return 0, nil, err
			}
		} else {
			information, ok := builtin.LookupDefaultOperatorClass(method, operatorType.ID)
			if !ok {
				return 0, nil, bindError("42704", "no default operator class for partition key type", int(element.Location)+1)
			}
			key.OperatorClass = information.ID
		}
		key.Type, key.Children, key.Collation = layout.Type, layout.Children, layout.Type.Collation
		keys = append(keys, key)
	}
	return strategy, keys, nil
}

func (b *binder) bindPartitionBound(ctx context.Context, raw *zpgparse.PartitionBoundSpec, parent *model.Descriptor, metadata *model.TableMetadata) (*model.PartitionBound, error) {
	if raw == nil || parent == nil || metadata == nil || metadata.PartitionStrategy == 0 || len(metadata.PartitionKeys) == 0 {
		return nil, bindError("42601", "partition bound requires a partitioned parent", 0)
	}
	definition, err := zpgparse.DeparsePartitionBound(raw)
	if err != nil {
		return nil, err
	}
	bound := &model.PartitionBound{Strategy: metadata.PartitionStrategy, Default: raw.IsDefault, Definition: definition}
	if raw.IsDefault {
		if metadata.PartitionStrategy == 'h' {
			return nil, bindError("0A000", "HASH partitioning does not support a default partition", int(raw.Location)+1)
		}
		return bound, nil
	}
	if len(raw.Strategy) != 1 || raw.Strategy[0] != metadata.PartitionStrategy {
		return nil, bindError("42P17", "partition bound strategy differs from parent", int(raw.Location)+1)
	}
	layouts := make([]model.TypeLayout, len(metadata.PartitionKeys))
	for i, key := range metadata.PartitionKeys {
		layouts[i] = model.TypeLayout{Type: key.Type, Children: key.Children}
	}
	switch metadata.PartitionStrategy {
	case 'h':
		if raw.Modulus <= 0 || raw.Remainder < 0 || raw.Remainder >= raw.Modulus || len(raw.Listdatums) != 0 || len(raw.Lowerdatums) != 0 || len(raw.Upperdatums) != 0 {
			return nil, bindError("22023", "invalid HASH partition bound", int(raw.Location)+1)
		}
		bound.Modulus, bound.Remainder = uint32(raw.Modulus), uint32(raw.Remainder)
	case 'l':
		if len(layouts) != 1 || len(raw.Listdatums) == 0 || len(raw.Lowerdatums) != 0 || len(raw.Upperdatums) != 0 {
			return nil, bindError("22023", "invalid LIST partition bound", int(raw.Location)+1)
		}
		bound.ListValues = make([]model.PartitionDatum, len(raw.Listdatums))
		for i, node := range raw.Listdatums {
			value, err := b.partitionBoundValue(ctx, node, layouts[0])
			if err != nil {
				return nil, err
			}
			bound.ListValues[i] = model.PartitionDatum{Kind: model.PartitionDatumValue, Layout: layouts[0], Value: value}
		}
	case 'r':
		if len(raw.Lowerdatums) != len(layouts) || len(raw.Upperdatums) != len(layouts) {
			return nil, bindError("22023", "RANGE partition bound arity differs from partition key", int(raw.Location)+1)
		}
		bound.Lower = make([]model.PartitionDatum, len(layouts))
		bound.Upper = make([]model.PartitionDatum, len(layouts))
		for i, node := range raw.Lowerdatums {
			datum, err := b.partitionRangeDatum(ctx, node, layouts[i])
			if err != nil {
				return nil, err
			}
			bound.Lower[i] = datum
		}
		for i, node := range raw.Upperdatums {
			datum, err := b.partitionRangeDatum(ctx, node, layouts[i])
			if err != nil {
				return nil, err
			}
			bound.Upper[i] = datum
		}
	default:
		return nil, bindError("42601", "invalid partition strategy", int(raw.Location)+1)
	}
	return bound, nil
}

func (b *binder) partitionRangeDatum(ctx context.Context, node *zpgparse.Node, layout model.TypeLayout) (model.PartitionDatum, error) {
	if node == nil {
		return model.PartitionDatum{}, bindError("42601", "invalid RANGE partition datum", 0)
	}
	position := 0
	if constant := node.GetAConst(); constant != nil {
		position = int(constant.Location) + 1
	}
	if ref := node.GetColumnRef(); ref != nil && len(ref.Fields) == 1 {
		name := ref.Fields[0].GetString_()
		if name != nil {
			position = int(ref.Location) + 1
			switch {
			case strings.EqualFold(name.Sval, "minvalue"):
				return model.PartitionDatum{Kind: model.PartitionDatumMin, Layout: layout}, nil
			case strings.EqualFold(name.Sval, "maxvalue"):
				return model.PartitionDatum{Kind: model.PartitionDatumMax, Layout: layout}, nil
			}
		}
	}
	value, err := b.partitionBoundValue(ctx, node, layout)
	if err == nil && value.IsNull {
		err = bindError("42P17", "cannot specify NULL in a range partition bound", position)
	}
	return model.PartitionDatum{Kind: model.PartitionDatumValue, Layout: layout, Value: value}, err
}

func (b *binder) partitionBoundValue(ctx context.Context, node *zpgparse.Node, layout model.TypeLayout) (model.Value, error) {
	if node == nil {
		return model.Value{}, bindError("42601", "missing partition bound value", 0)
	}
	scope, err := b.newScope(nil)
	if err != nil {
		return model.Value{}, err
	}
	value, err := b.scalar(node, scope)
	if err != nil {
		return model.Value{}, err
	}
	value, err = b.coerce(scope, value, layout, ir.ImplicitCast)
	if err != nil {
		return model.Value{}, err
	}
	if len(value.reads) != 0 || value.effects.Volatility != ir.Immutable {
		return model.Value{}, bindError("42P17", "partition bound expression is not an immutable constant", value.position)
	}
	// PostgreSQL coerces a bound and then folds it, so a literal whose declared
	// type differs from the partition key (an int4 constant bound to a bigint
	// key, for example) still reaches the catalog as a constant of the key's
	// type. Fold the immutable expression here to match that, and keep the
	// immutability and read-free requirements above so a genuinely non-constant
	// bound is still refused.
	if value.value.Type != layout.Type.Storage {
		folded, foldErr := b.foldConstantScalar(ctx, value.id, layout)
		if foldErr != nil {
			return model.Value{}, foldErr
		}
		value.value = folded
	}
	if value.value.Type != layout.Type.Storage {
		return model.Value{}, bindError("42P17", "partition bound expression is not an immutable constant", value.position)
	}
	return value.value.Clone(), ctx.Err()
}

// foldConstantScalar evaluates a read-free immutable scalar expression into a
// value of the expected storage. Only constant leaves and invocations whose
// resolved callable is itself immutable are evaluated, so a bound that depends
// on a slot, parameter, subquery, or volatile call yields no value and the
// caller reports the bound as non-constant.
func (b *binder) foldConstantScalar(ctx context.Context, id ir.ScalarID, layout model.TypeLayout) (model.Value, error) {
	if err := ctx.Err(); err != nil {
		return model.Value{}, err
	}
	limit := b.options.Limits.MaxBytes
	if limit == 0 {
		return model.Value{}, model.ErrValueBudget
	}
	node, ok := b.builder.Scalar(id)
	if !ok {
		return model.Value{}, bindError("42P17", "partition bound expression is not an immutable constant", 0)
	}
	if node.Kind == ir.Const {
		return b.builder.CloneConstant(ctx, id, limit)
	}
	if node.Kind != ir.Call && node.Kind != ir.Cast {
		return model.Value{}, model.ErrValueBudget
	}
	// Immutability is carried on the bound node, which already accounts for the
	// resolved function's volatility; the fold never re-derives it.
	if node.Effects.Volatility != ir.Immutable {
		return model.Value{}, model.ErrValueBudget
	}
	signature, ok := b.builder.Signature(node.Signature)
	if !ok {
		return model.Value{}, model.ErrValueBudget
	}
	arguments := make([]model.Value, node.OperandCount())
	var argumentLayouts [8]model.TypeLayout
	layouts := argumentLayouts[:0]
	for i := range arguments {
		operand, found := b.builder.Operand(id, uint32(i))
		if !found {
			return model.Value{}, model.ErrValueBudget
		}
		operandNode, found := b.builder.Scalar(operand)
		if !found {
			return model.Value{}, model.ErrValueBudget
		}
		operandLayout, found := b.builder.Layout(operandNode.Layout)
		if !found {
			return model.Value{}, model.ErrValueBudget
		}
		folded, foldErr := b.foldConstantScalar(ctx, operand, operandLayout)
		if foldErr != nil {
			return model.Value{}, foldErr
		}
		arguments[i] = folded
		layouts = append(layouts, operandLayout)
	}
	options := b.options.Builtin
	options.Collation, options.DefaultCollation = b.options.Collation, b.options.DefaultCollation
	options.ExtraFloatDigits = 1
	maximum := min(limit, uint64(1<<62))
	callable, err := builtin.ResolveTyped(ctx, signature.Function, layouts, layout, options, maximum)
	if err != nil {
		return model.Value{}, model.ErrValueBudget
	}
	remaining := maximum - callable.OwnedBytes()
	value, err := callable.Call(ctx, arguments, builtin.CallBudget{OutputBytes: int64(remaining), MemoryBytes: remaining, Temporal: b.options.Temporal})
	if err != nil {
		return model.Value{}, err
	}
	if value.Type != layout.Type.Storage {
		return model.Value{}, model.ErrValueBudget
	}
	return value, ctx.Err()
}

func partitionOperatorClass(method string, typ model.Type, nodes []*zpgparse.Node) (model.OID, error) {
	if len(nodes) == 0 || len(nodes) > 2 {
		return 0, bindError("42601", "operator class requires an optionally qualified name", 0)
	}
	parts, err := names(nodes)
	if err != nil {
		return 0, err
	}
	if len(parts) == 2 && parts[0] != "pg_catalog" {
		return 0, bindError("42704", "operator class does not exist for partition access method", 0)
	}
	class, found := builtin.LookupOperatorClassName(method, parts[len(parts)-1])
	if !found {
		return 0, bindError("42704", "operator class does not exist for partition access method", 0)
	}
	if err := builtin.ValidateOperatorClassInput(typ, class.ID); err != nil {
		return 0, err
	}
	return class.ID, nil
}

func validatePartitionUniqueIndexes(ctx context.Context, metadata *model.TableMetadata, indexes []*model.Descriptor, limits model.DescriptorLimits) error {
	if metadata == nil || metadata.PartitionStrategy == 0 {
		return nil
	}
	byID := make(map[model.ObjectID]*model.IndexMetadata, len(indexes))
	for _, descriptor := range indexes {
		spec, err := descriptor.SpecWithin(ctx, limits)
		if err != nil {
			return err
		}
		if spec.Index != nil {
			byID[descriptor.ID()] = spec.Index
		}
	}
	for _, constraint := range metadata.Constraints {
		if constraint.Kind != model.ConstraintPrimaryKey && constraint.Kind != model.ConstraintUnique {
			continue
		}
		index := byID[constraint.Index]
		if index == nil {
			return model.ErrInvalidDescriptor
		}
		for _, partitionKey := range metadata.PartitionKeys {
			found := false
			for _, indexKey := range index.Keys {
				if partitionKey.Column != 0 && partitionKey.Column == indexKey.Column {
					found = true
					break
				}
			}
			if !found {
				return bindError("0A000", "unique constraint on partitioned table must include all partitioning columns", 0)
			}
		}
	}
	return nil
}
