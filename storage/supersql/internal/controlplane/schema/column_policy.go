package schema

import (
	"cmp"
	"context"
	"errors"
	"math"
	"slices"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type IdentityDefaultSpec struct {
	Sequence model.ObjectID
	Table    model.ObjectID
	Column   model.ColumnID
	Result   model.TypeLayout
}

// ColumnExpressions borrows definitions during compilation. Returned programs own
// their metadata and must not retain a catalog snapshot or borrowed row values.
// Defaults receive nil; stored generation and CHECK receive the ColumnID-sorted row.
type ColumnExpressions interface {
	CompileColumnDefault(context.Context, *model.ExpressionDefinition, model.TypeLayout) (txn.ValueExpression, error)
	CompileStoredGenerated(context.Context, *model.ExpressionDefinition, model.ColumnID, []storage.ColumnLayout, model.TypeLayout) (txn.ValueExpression, error)
	CompileCheck(context.Context, *model.ExpressionDefinition, model.ObjectID, []storage.ColumnLayout) (txn.ValueExpression, error)
	CompileIdentityDefault(context.Context, IdentityDefaultSpec) (txn.ValueExpression, error)
}

// Descriptor copying and policy metadata are independently bounded by MetadataBytes.
// RowBytes bounds normalizer metadata and each completed row; compiler and encoder
// budgets remain independent. All limits must be positive.
type TablePolicyOptions struct {
	Descriptor    *model.Descriptor
	Encoder       storage.KeyEncoder
	Expressions   ColumnExpressions
	MetadataBytes uint64
	RowBytes      uint64
	MaxColumns    int
}

var (
	errTablePolicyOptions = &model.Error{Code: "22023", Message: "invalid table policy options", Cause: storage.ErrInvalidLayout}
	errTablePolicyLimit   = &model.Error{Code: "54000", Message: "table policy metadata exceeds budget", Cause: model.ErrValueBudget}
	errColumnExpressions  = &model.Error{Code: "55000", Message: "table policy requires a column expression compiler"}
)

// NewTablePolicy compiles once, without evaluating row expressions. It can
// serve as the base normalizer below index maintenance and sealed insert candidates.
func NewTablePolicy(ctx context.Context, options TablePolicyOptions) (*txn.RowNormalizer, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	invalidMetadata := options.MetadataBytes == 0 || options.MetadataBytes > math.MaxInt
	invalidRows := options.RowBytes == 0 || options.RowBytes > math.MaxInt
	invalidColumns := options.MaxColumns <= 0 || uint64(options.MaxColumns) > math.MaxUint32
	if invalidMetadata || invalidRows || invalidColumns {
		return nil, errTablePolicyLimit
	}
	descriptor := options.Descriptor
	if descriptor == nil || descriptor.Kind() != model.ObjectTable && descriptor.Kind() != model.ObjectMaterializedView || descriptor.State() != model.StatePublic || mutationPolicyNil(options.Encoder) {
		return nil, errTablePolicyOptions
	}
	spec, err := descriptor.SpecWithin(ctx, model.DescriptorLimits{MaxBytes: options.MetadataBytes, MaxItems: int(options.MetadataBytes), MaxDepth: 64})
	if err != nil {
		return nil, err
	}
	if spec.Table == nil || len(spec.Table.Columns) == 0 || len(spec.Table.PrimaryKey) == 0 {
		return nil, errTablePolicyOptions
	}
	table := spec.Table
	slices.SortFunc(table.Columns, func(a, b model.ColumnDescriptor) int { return cmp.Compare(a.ID, b.ID) })
	activeColumns := make([]model.ColumnDescriptor, 0, len(table.Columns))
	for i, column := range table.Columns {
		if column.ID == 0 || i > 0 && table.Columns[i-1].ID == column.ID {
			return nil, errTablePolicyOptions
		}
		switch column.State {
		case model.StatePublic, model.StateWriteOnly:
			activeColumns = append(activeColumns, column)
		case model.StateDeleteOnly, model.StateDropWriteOnly, model.StateDropDeleteOnly:
		default:
			return nil, errTablePolicyOptions
		}
	}
	table.Columns = activeColumns
	if len(table.Columns) == 0 || len(table.Columns) > options.MaxColumns || len(table.PrimaryKey) > options.MaxColumns {
		return nil, errTablePolicyLimit
	}
	remaining := options.MetadataBytes
	charge := func(count, size uint64) error {
		if size != 0 && count > remaining/size {
			return errTablePolicyLimit
		}
		remaining -= count * size
		return nil
	}
	if err := charge(1, uint64(unsafe.Sizeof(txn.RowNormalizer{}))); err != nil {
		return nil, err
	}
	// Admit both the construction layouts and the normalizer's retained copies.
	if err := charge(uint64(len(table.Columns)), 2*uint64(unsafe.Sizeof(txn.ColumnRule{}))+uint64(unsafe.Sizeof(storage.ColumnLayout{}))); err != nil {
		return nil, err
	}
	checkCount := 0
	for _, constraint := range table.Constraints {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		switch constraint.Kind {
		case model.ConstraintCheck:
			if constraint.Enforced {
				checkCount++
				if err := charge(uint64(len(constraint.Name)), 1); err != nil {
					return nil, err
				}
			}
		case model.ConstraintNotNull, model.ConstraintUnique, model.ConstraintPrimaryKey,
			model.ConstraintForeignKey, model.ConstraintExclude:
		}
	}
	if err := charge(uint64(checkCount), 2*uint64(unsafe.Sizeof(txn.CheckRule{}))); err != nil {
		return nil, err
	}
	columns := make([]storage.ColumnLayout, len(table.Columns))
	rules := make([]txn.ColumnRule, len(table.Columns))
	for i, column := range table.Columns {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		repeated := i > 0 && table.Columns[i-1].ID == column.ID
		if column.ID == 0 || repeated {
			return nil, errTablePolicyOptions
		}
		missingGeneration := column.GeneratedStored && column.Generated == nil
		hasDefault := column.Default != nil || column.ConstantDefault != nil
		generatedDefault := column.Generated != nil && hasDefault
		invalidIdentity := column.IdentitySequence != 0 && (hasDefault || column.Nullable || column.Generated != nil)
		if invalidIdentity || (column.IdentityAlways && column.IdentitySequence == 0) {
			return nil, errTablePolicyOptions
		}
		if missingGeneration || generatedDefault {
			return nil, errTablePolicyOptions
		}
		layout, used, err := model.CopyTypeLayout(ctx, column.Layout, remaining/2)
		if err != nil {
			return nil, err
		}
		remaining -= 2 * used
		columns[i] = storage.ColumnLayout{ID: column.ID, Type: layout.Type, Children: layout.Children, Nullable: column.Nullable}
		rules[i].SystemIdentity = column.SystemIdentity
		rules[i].Virtual = column.Generated != nil && !column.GeneratedStored
		if column.ConstantDefault != nil {
			if err := storage.ValidateStoredValue(ctx, *column.ConstantDefault, layout); err != nil {
				return nil, err
			}
			if err := charge(1, uint64(unsafe.Sizeof(columnConstant{}))); err != nil {
				return nil, err
			}
			value, used, err := model.CopyValue(ctx, *column.ConstantDefault, remaining)
			if err != nil {
				return nil, err
			}
			remaining -= used
			columns[i].HasMissingValue = true
			columns[i].MissingValue = value
			rules[i].Default = &columnConstant{value: value, resultBytes: options.RowBytes, ownedBytes: uint64(unsafe.Sizeof(columnConstant{})) + used}
		}
		rules[i].FillMissingOnUpdate = column.State == model.StateWriteOnly && rules[i].Default != nil
		rules[i].Layout = columns[i]
	}
	physical := descriptor.Physical()
	target := storage.ObjectRef{ObjectID: descriptor.ID(), Generation: physical.Generation, DescriptorVersion: descriptor.Version(), EncodingVersion: physical.EncodingVersion}
	persistedColumns := make([]storage.ColumnLayout, 0, len(columns))
	for i, column := range columns {
		if !rules[i].Virtual {
			persistedColumns = append(persistedColumns, column)
		}
	}
	if err := (storage.ObjectLayout{Target: target, Columns: persistedColumns}).Validate(ctx); err != nil {
		return nil, err
	}
	for i, key := range table.PrimaryKey {
		position, ok := slices.BinarySearchFunc(columns, key.Column, func(column storage.ColumnLayout, id model.ColumnID) int { return cmp.Compare(column.ID, id) })
		if !ok || key.Expression != nil {
			return nil, errTablePolicyOptions
		}
		wrongKeyType := columns[position].Nullable || columns[position].Type != key.Type
		wrongKeyIdentity := rules[position].KeyPosition != 0 || key.Collation != key.Type.Collation
		if wrongKeyType || wrongKeyIdentity {
			return nil, errTablePolicyOptions
		}
		unsupportedKey := key.Type.Storage == model.TypeVectorF32
		invalidCollation := key.Type.Storage != model.TypeString && key.Collation != 0
		if unsupportedKey || invalidCollation {
			return nil, errTablePolicyOptions
		}
		layout := model.TypeLayout{Type: key.Type, Children: columns[position].Children}
		if key.OperatorClass != 0 {
			encoder, ok := options.Encoder.(storage.IndexKeyEncoder)
			if !ok {
				return nil, &IndexKeyImplementationError{Type: key.Type, OperatorClass: key.OperatorClass}
			}
			if err := encoder.ValidateOperatorClass(ctx, layout, key.OperatorClass); err != nil {
				return nil, &IndexKeyImplementationError{Type: key.Type, OperatorClass: key.OperatorClass, Cause: err}
			}
		}
		keyColumn := storage.KeyColumn{ColumnID: key.Column, Type: key.Type, Children: layout.Children, Descending: key.Descending, NullsFirst: key.NullsFirst}
		keyBytes, err := storage.KeyLayoutBytes(ctx, []storage.KeyColumn{keyColumn}, remaining)
		if err != nil {
			return nil, err
		}
		remaining -= keyBytes
		encoded, err := options.Encoder.EncodePrefix(ctx, []storage.KeyColumn{keyColumn}, []model.Value{model.NullVal(key.Type.Storage)}, options.RowBytes)
		if err != nil {
			return nil, errors.Join(ctx.Err(), err)
		}
		if len(encoded) == 0 || uint64(cap(encoded)) > options.RowBytes {
			return nil, errTablePolicyOptions
		}
		rules[position].KeyPosition = uint32(i + 1)
		rules[position].KeyDescending = key.Descending
		rules[position].KeyNullsFirst = key.NullsFirst
	}
	for i, column := range table.Columns {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if column.Default == nil && column.Generated == nil && column.IdentitySequence == 0 {
			continue
		}
		if mutationPolicyNil(options.Expressions) {
			return nil, errColumnExpressions
		}
		var program txn.ValueExpression
		if column.IdentitySequence != 0 {
			program, err = options.Expressions.CompileIdentityDefault(ctx, IdentityDefaultSpec{Sequence: column.IdentitySequence, Table: descriptor.ID(), Column: column.ID, Result: column.Layout})
			rules[i].Default = program
		} else if column.Default != nil {
			program, err = options.Expressions.CompileColumnDefault(ctx, column.Default, column.Layout)
			rules[i].Default = program
		} else {
			program, err = options.Expressions.CompileStoredGenerated(ctx, column.Generated, column.ID, columns, column.Layout)
			rules[i].Generated = program
		}
		if err != nil {
			return nil, err
		}
		if mutationPolicyNil(program) {
			return nil, errColumnExpressions
		}
		rules[i].FillMissingOnUpdate = column.State == model.StateWriteOnly && rules[i].Default != nil
	}
	slices.SortFunc(table.Constraints, func(a, b model.ConstraintDescriptor) int { return cmp.Compare(a.Name, b.Name) })
	checks := make([]txn.CheckRule, 0, checkCount)
	for _, constraint := range table.Constraints {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if constraint.Kind != model.ConstraintCheck || !constraint.Enforced {
			continue
		}
		if mutationPolicyNil(options.Expressions) {
			return nil, errColumnExpressions
		}
		program, err := options.Expressions.CompileCheck(ctx, constraint.Expression, constraint.ID, columns)
		if err != nil {
			return nil, err
		}
		if mutationPolicyNil(program) {
			return nil, errColumnExpressions
		}
		checks = append(checks, txn.CheckRule{Name: constraint.Name, Expression: program})
	}
	return txn.NewRowNormalizer(ctx, target, rules, options.RowBytes, checks...)
}

type columnConstant struct {
	value       model.Value
	resultBytes uint64
	ownedBytes  uint64
}

func (c *columnConstant) OwnedBytes() uint64 { return c.ownedBytes }

func (c *columnConstant) Evaluate(ctx context.Context, row []model.Value, _ model.ExpressionRuntime) (model.Value, error) {
	if err := ctx.Err(); err != nil {
		return model.Value{}, err
	}
	if row != nil {
		return model.Value{}, errTablePolicyOptions
	}
	value, _, err := model.CopyValue(ctx, c.value, c.resultBytes)
	return value, err
}
