package txn

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/ordered"
)

// ValueExpression is an immutable, bound program. Inputs and runtime are borrowed
// and must not be mutated or retained; outputs transfer ownership. Evaluation
// allocations obey the engine/query resource policy supplied with the program.
type ValueExpression interface {
	Evaluate(context.Context, []model.Value, model.ExpressionRuntime) (model.Value, error)
}

// OwnedValueExpression reports retained program metadata. Other capabilities are
// borrowed and must be admitted by their external owner.
type OwnedValueExpression interface {
	ValueExpression
	OwnedBytes() uint64
}

type ColumnRule struct {
	Layout              storage.ColumnLayout
	Default             ValueExpression
	Generated           ValueExpression
	SystemIdentity      bool
	FillMissingOnUpdate bool
	Virtual             bool
	// Zero denotes an opaque/non-key column; positive positions are one-based.
	KeyPosition   uint32
	KeyDescending bool
	KeyNullsFirst bool
}

type CheckRule struct {
	Name       string
	Expression ValueExpression
}

// RowNormalizer owns descriptor metadata and borrows immutable expression
// capabilities. It completes and checks row images, not FK, trigger or index effects.
type RowNormalizer struct {
	target          storage.ObjectRef
	columns         []ColumnRule
	physicalColumns int
	checks          []CheckRule
	maxBytes        uint64
	metadataBytes   uint64
	retainedBytes   uint64
	identity        [24]byte
	hasIdentity     bool
	keyLayout       []storage.KeyColumn
	expressions     model.ExpressionRuntime
}

// NewRowNormalizer admits metadata and each normalization operation separately
// against maxBytes. Zero admits nothing. MissingValue is read-schema metadata,
// not an insert default. Defaults have no row input; generated programs receive
// the full logical row in increasing ColumnID order. Virtual generated values
// remain available to checks, indexes, triggers, and RETURNING, but Layout omits
// them from the persisted base-row payload.
func NewRowNormalizer(ctx context.Context, target storage.ObjectRef, columns []ColumnRule, maxBytes uint64, checks ...CheckRule) (*RowNormalizer, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := target.Validate(); err != nil {
		return nil, err
	}
	memory := overlayMemory{limit: maxBytes}
	if err := memory.add(1, uint64(unsafe.Sizeof(RowNormalizer{}))); err != nil {
		return nil, err
	}
	if err := memory.add(uint64(len(columns)), uint64(unsafe.Sizeof(ColumnRule{}))); err != nil {
		return nil, err
	}
	if err := memory.add(uint64(len(checks)), uint64(unsafe.Sizeof(CheckRule{}))); err != nil {
		return nil, err
	}
	n := &RowNormalizer{target: target, columns: make([]ColumnRule, len(columns)), maxBytes: maxBytes}
	for i, rule := range columns {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		column := rule.Layout
		if column.ID == 0 || (i > 0 && columns[i-1].Layout.ID >= column.ID) {
			return nil, storage.ErrInvalidLayout
		}
		if rule.FillMissingOnUpdate && rule.Default == nil {
			return nil, storage.ErrInvalidLayout
		}
		if rule.SystemIdentity {
			invalidLayout := column.Type.ID != 17 || column.Type.Storage != model.TypeBytes || column.Nullable
			invalidRule := rule.Default != nil || rule.Generated != nil || rule.Virtual || rule.KeyPosition != 1 || rule.FillMissingOnUpdate
			if invalidLayout || invalidRule {
				return nil, storage.ErrInvalidLayout
			}
		}
		if rule.Virtual && (rule.Generated == nil || rule.Default != nil) {
			return nil, storage.ErrInvalidLayout
		}
		if rule.Default != nil && rule.Generated != nil {
			return nil, fmt.Errorf("column %d has default and generated expressions: %w", column.ID, storage.ErrInvalidLayout)
		}
		layout, used, err := model.CopyTypeLayout(ctx, model.TypeLayout{Type: column.Type, Children: column.Children}, memory.limit-memory.used)
		if err != nil {
			return nil, normalizerCopyError(err)
		}
		memory.used += used
		column.Children = layout.Children
		column.MissingValue = model.Value{}
		if column.HasMissingValue {
			missing, used, err := model.CopyValue(ctx, rule.Layout.MissingValue, memory.limit-memory.used)
			if err != nil {
				return nil, normalizerCopyError(err)
			}
			memory.used += used
			if err := validateNormalizerValue(ctx, missing, column); err != nil {
				return nil, errors.Join(storage.ErrInvalidLayout, err)
			}
			column.MissingValue = missing
		}
		rule.Layout = column
		n.columns[i] = rule
		if !rule.Virtual {
			n.physicalColumns++
		}
	}
	n.checks = make([]CheckRule, len(checks))
	for i, check := range checks {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if check.Name == "" || nilValueExpression(check.Expression) {
			return nil, storage.ErrInvalidLayout
		}
		if err := memory.add(uint64(len(check.Name)), 1); err != nil {
			return nil, err
		}
		n.checks[i] = CheckRule{Name: strings.Clone(check.Name), Expression: check.Expression}
	}
	slices.SortFunc(n.checks, func(a, b CheckRule) int { return strings.Compare(a.Name, b.Name) })
	for i := 1; i < len(n.checks); i++ {
		if n.checks[i-1].Name == n.checks[i].Name {
			return nil, storage.ErrInvalidLayout
		}
	}
	keyCount := 0
	for _, rule := range n.columns {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if rule.KeyPosition != 0 {
			keyCount++
		} else if rule.KeyDescending || rule.KeyNullsFirst {
			return nil, storage.ErrInvalidLayout
		}
	}
	if err := memory.add(uint64(keyCount), uint64(unsafe.Sizeof(storage.KeyColumn{}))); err != nil {
		return nil, err
	}
	if keyCount != 0 {
		n.keyLayout = make([]storage.KeyColumn, keyCount)
	}
	for _, rule := range n.columns {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if rule.KeyPosition == 0 {
			continue
		}
		if uint64(rule.KeyPosition) > uint64(keyCount) {
			return nil, storage.ErrInvalidLayout
		}
		key := &n.keyLayout[rule.KeyPosition-1]
		if key.ColumnID != 0 {
			return nil, storage.ErrInvalidLayout
		}
		*key = storage.KeyColumn{ColumnID: rule.Layout.ID, Type: rule.Layout.Type, Children: rule.Layout.Children, Descending: rule.KeyDescending, NullsFirst: rule.KeyNullsFirst}
	}
	keyHeaders := uint64(keyCount) * uint64(unsafe.Sizeof(storage.KeyColumn{}))
	keyBytes, err := storage.KeyLayoutBytes(ctx, n.keyLayout, memory.limit-memory.used+keyHeaders)
	if err != nil {
		return nil, normalizerCopyError(err)
	}
	childBytes := keyBytes - keyHeaders
	memory.used += childBytes
	for i := range n.keyLayout {
		key := &n.keyLayout[i]
		layout, used, err := model.CopyPhysicalTypeLayout(ctx, model.TypeLayout{Type: key.Type, Children: key.Children}, childBytes)
		if err != nil {
			return nil, normalizerCopyError(err)
		}
		key.Children = layout.Children
		childBytes -= used
	}
	n.metadataBytes = memory.used
	n.retainedBytes = memory.used
	for _, rule := range n.columns {
		for _, expression := range [2]ValueExpression{rule.Default, rule.Generated} {
			if err := n.retainExpression(ctx, expression); err != nil {
				return nil, err
			}
		}
	}
	for _, check := range n.checks {
		if err := n.retainExpression(ctx, check.Expression); err != nil {
			return nil, err
		}
	}
	return n, nil
}

// RetainedBytes includes owned metadata and reported program ownership, not live
// heap measurement. Shared programs may be conservatively charged more than once.
func (n *RowNormalizer) RetainedBytes() uint64 { return n.retainedBytes }

func (n *RowNormalizer) retainExpression(ctx context.Context, expression ValueExpression) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if expression == nil {
		return nil
	}
	if nilValueExpression(expression) {
		return storage.ErrInvalidLayout
	}
	if owned, ok := expression.(OwnedValueExpression); ok {
		bytes := owned.OwnedBytes()
		if bytes > ^uint64(0)-n.retainedBytes {
			return errTransactionBudget
		}
		n.retainedBytes += bytes
	}
	return nil
}

func nilValueExpression(expression ValueExpression) bool {
	if expression == nil {
		return true
	}
	value := reflect.ValueOf(expression)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

// KeyLayout returns owned metadata in key order, which can differ from row order.
func (n *RowNormalizer) KeyLayout() []storage.KeyColumn {
	layout, _, err := storage.CopyKeyLayout(context.Background(), n.keyLayout, n.metadataBytes)
	if err != nil {
		panic(err) // Constructor-admitted immutable metadata cannot fail admission.
	}
	return layout
}

func normalizerCopyError(err error) error {
	if errors.Is(err, model.ErrValueBudget) {
		return errors.Join(ordered.ErrBudget, err)
	}
	return errors.Join(storage.ErrInvalidLayout, err)
}

func (n *RowNormalizer) physicalColumnCount() int {
	return n.physicalColumns
}

// Layout returns an independent physical-layout copy. Virtual generated columns
// are logical row fields and are intentionally absent from persisted base rows.
func (n *RowNormalizer) Layout() storage.ObjectLayout {
	layout := storage.ObjectLayout{Target: n.target, Columns: make([]storage.ColumnLayout, 0, n.physicalColumnCount())}
	for _, rule := range n.columns {
		if rule.Virtual {
			continue
		}
		column := rule.Layout
		column.Children = cloneNormalizerLayouts(column.Children)
		if column.HasMissingValue {
			value, _, err := model.CopyValue(context.Background(), column.MissingValue, n.maxBytes)
			if err != nil {
				panic(err) // Constructor-admitted immutable metadata cannot fail admission.
			}
			column.MissingValue = value
		}
		layout.Columns = append(layout.Columns, column)
	}
	return layout
}

func cloneNormalizerLayouts(source []model.TypeLayout) []model.TypeLayout {
	if len(source) == 0 {
		return nil
	}
	result := make([]model.TypeLayout, len(source))
	for i, layout := range source {
		result[i] = model.TypeLayout{Type: layout.Type, Children: cloneNormalizerLayouts(layout.Children)}
		if len(layout.FieldNames) != 0 {
			result[i].FieldNames = make([]string, len(layout.FieldNames))
			for j, name := range layout.FieldNames {
				result[i].FieldNames[j] = strings.Clone(name)
			}
		}
	}
	return result
}

// normalizeOwned transfers selected mutation/previous payloads on success without
// cloning them. Failure leaves both inputs unchanged. previous may be a complete
// logical row or a persisted physical row with virtual generated columns omitted;
// nil denotes absence. Inputs cannot be borrowed from a mutable snapshot cursor.
func (n *RowNormalizer) normalizeOwned(ctx context.Context, mutation storage.Mutation, previous *model.Row) (storage.Mutation, error) {
	if err := ctx.Err(); err != nil {
		return storage.Mutation{}, err
	}
	if mutation.Target != n.target {
		return storage.Mutation{}, storage.ErrInvalidTarget
	}
	used, err := mutationMemory(mutation, n.maxBytes)
	if err != nil {
		return storage.Mutation{}, err
	}
	memory := overlayMemory{limit: n.maxBytes, used: used}
	shape := mutation
	if mutation.Op >= storage.MutationInsert && mutation.Op <= storage.MutationReplace {
		if len(mutation.ColumnIDs) != len(mutation.Values) {
			return storage.Mutation{}, storage.ErrInvalidMutation
		}
		// The positions vector below validates IDs without a sorting allocation.
		shape.ColumnIDs, shape.Values = nil, nil
	}
	if err := shape.ValidateShape(); err != nil {
		return storage.Mutation{}, err
	}
	logicalPrevious := previous
	if previous != nil {
		physicalColumns := n.physicalColumnCount()
		if previous.Deleted || !bytes.Equal(previous.Key, mutation.Key) || len(previous.Values) != len(n.columns) && len(previous.Values) != physicalColumns {
			return storage.Mutation{}, storage.ErrInvalidLayout
		}
		if err := memory.add(uint64(cap(previous.Key)), 1); err != nil {
			return storage.Mutation{}, err
		}
		if err := memory.values(previous.Values, 0); err != nil {
			return storage.Mutation{}, err
		}
		if len(previous.Values) == physicalColumns && physicalColumns != len(n.columns) {
			if err := memory.add(uint64(len(n.columns)), uint64(unsafe.Sizeof(model.Value{}))); err != nil {
				return storage.Mutation{}, err
			}
			values := make([]model.Value, len(n.columns))
			physical := 0
			for i, rule := range n.columns {
				if rule.Virtual {
					values[i] = model.NullVal(rule.Layout.Type.Storage)
					continue
				}
				values[i] = previous.Values[physical]
				physical++
			}
			logicalPrevious = &model.Row{Key: previous.Key, Values: values}
		}
		for i, value := range logicalPrevious.Values {
			if logicalPrevious != previous && n.columns[i].Virtual {
				continue
			}
			if err := validateNormalizerValue(ctx, value, n.columns[i].Layout); err != nil {
				return storage.Mutation{}, errors.Join(storage.ErrInvalidLayout, err)
			}
		}
	}
	if mutation.Op == storage.MutationDelete || mutation.Op == storage.MutationDeleteRange {
		mutation.ChecksumCRC = 0
		return mutation, nil
	}
	return n.completeOwned(ctx, mutation, logicalPrevious, memory)
}

// completeOwned completes values independently of the encoded input key.
func (n *RowNormalizer) completeOwned(ctx context.Context, mutation storage.Mutation, previous *model.Row, memory overlayMemory) (storage.Mutation, error) {
	if len(mutation.ColumnIDs) != len(mutation.Values) {
		return storage.Mutation{}, storage.ErrInvalidMutation
	}
	// Positions also detect duplicate explicit IDs without a per-row hash table.
	if err := memory.add(uint64(len(n.columns)), uint64(unsafe.Sizeof(int(0)))+4+uint64(unsafe.Sizeof(model.Value{}))); err != nil {
		return storage.Mutation{}, err
	}
	positions := make([]int, len(n.columns))
	for i := range positions {
		positions[i] = -1
	}
	for i, id := range mutation.ColumnIDs {
		position := sort.Search(len(n.columns), func(j int) bool { return n.columns[j].Layout.ID >= id })
		if position == len(n.columns) || n.columns[position].Layout.ID != id {
			return storage.Mutation{}, &model.Error{Code: "42703", Message: fmt.Sprintf("unknown column %d", id), Cause: storage.ErrInvalidMutation}
		}
		if positions[position] >= 0 {
			return storage.Mutation{}, storage.ErrInvalidMutation
		}
		positions[position] = i
		column := n.columns[position]
		if column.Generated != nil {
			return storage.Mutation{}, &model.Error{Code: "428C9", Message: fmt.Sprintf("cannot supply generated column %d", id), Cause: storage.ErrInvalidMutation}
		}
		if err := validateNormalizerValue(ctx, mutation.Values[i], column.Layout); err != nil {
			return storage.Mutation{}, err
		}
	}
	if mutation.Op == storage.MutationUpdate && previous == nil {
		return storage.Mutation{}, storage.ErrRowNotFound
	}
	values := make([]model.Value, len(n.columns))
	columns := make([]model.ColumnID, len(n.columns))
	preserve := previous != nil && (mutation.Op == storage.MutationUpdate || mutation.Op == storage.MutationUpsert)
	for i, rule := range n.columns {
		if err := ctx.Err(); err != nil {
			return storage.Mutation{}, err
		}
		columns[i] = rule.Layout.ID
		if rule.Generated != nil {
			values[i] = model.NullVal(rule.Layout.Type.Storage)
			continue
		}
		switch {
		case positions[i] >= 0:
			values[i] = mutation.Values[positions[i]]
		case preserve && !(rule.FillMissingOnUpdate && previous.Values[i].IsNull && rule.Default != nil):
			values[i] = previous.Values[i]
		case rule.SystemIdentity && n.hasIdentity:
			if err := memory.add(24, 1); err != nil {
				return storage.Mutation{}, err
			}
			values[i] = model.BytesVal(slices.Clone(n.identity[:]))
		case rule.Default != nil:
			value, err := n.evaluate(ctx, rule.Default, nil, rule.Layout, &memory)
			if err != nil {
				return storage.Mutation{}, err
			}
			values[i] = value
		default:
			values[i] = model.NullVal(rule.Layout.Type.Storage)
		}
		if err := validateNormalizerValue(ctx, values[i], rule.Layout); err != nil {
			return storage.Mutation{}, err
		}
	}
	for i, rule := range n.columns {
		if rule.Generated == nil {
			continue
		}
		value, err := n.evaluate(ctx, rule.Generated, values, rule.Layout, &memory)
		if err != nil {
			return storage.Mutation{}, err
		}
		values[i] = value
	}
	if err := n.checkRow(ctx, values, &memory); err != nil {
		return storage.Mutation{}, err
	}
	if mutation.Op == storage.MutationInsert && previous != nil {
		return storage.Mutation{}, &model.Error{Code: "23505", Message: "row already exists", Cause: storage.ErrInvalidMutation}
	}
	mutation.Op = storage.MutationUpsert
	mutation.ColumnIDs = columns
	mutation.Values = values
	mutation.EndKey = nil
	mutation.ChecksumCRC = 0
	return mutation, nil
}
func (n *RowNormalizer) persistableMutation(mutation *storage.Mutation) error {
	if mutation == nil || mutation.Op != storage.MutationUpsert || len(mutation.ColumnIDs) != len(n.columns) || len(mutation.Values) != len(n.columns) {
		return storage.ErrInvalidMutation
	}
	if n.physicalColumnCount() == len(n.columns) {
		return nil
	}
	physical := 0
	for i, rule := range n.columns {
		if mutation.ColumnIDs[i] != rule.Layout.ID {
			return storage.ErrInvalidMutation
		}
		if rule.Virtual {
			continue
		}
		mutation.ColumnIDs[physical] = mutation.ColumnIDs[i]
		mutation.Values[physical] = mutation.Values[i]
		physical++
	}
	clear(mutation.ColumnIDs[physical:])
	clear(mutation.Values[physical:])
	mutation.ColumnIDs = mutation.ColumnIDs[:physical]
	mutation.Values = mutation.Values[:physical]
	return nil
}

// ValidateRow checks a complete logical row or persisted physical row against
// current generation, nullability, and CHECK constraints.
func (n *RowNormalizer) ValidateRow(ctx context.Context, row *model.Row) error {
	if n == nil || row == nil || row.Deleted {
		return storage.ErrInvalidLayout
	}
	physicalColumns := n.physicalColumnCount()
	if len(row.Values) != len(n.columns) && len(row.Values) != physicalColumns {
		return storage.ErrInvalidLayout
	}
	memory := overlayMemory{limit: n.maxBytes}
	if err := memory.values(row.Values, 0); err != nil {
		return err
	}
	values := row.Values
	if len(row.Values) == physicalColumns && physicalColumns != len(n.columns) {
		if err := memory.add(uint64(len(n.columns)), uint64(unsafe.Sizeof(model.Value{}))); err != nil {
			return err
		}
		values = make([]model.Value, len(n.columns))
		physical := 0
		for i, rule := range n.columns {
			if rule.Virtual {
				values[i] = model.NullVal(rule.Layout.Type.Storage)
				continue
			}
			values[i] = row.Values[physical]
			physical++
		}
		for i, rule := range n.columns {
			if !rule.Virtual {
				continue
			}
			value, err := n.evaluate(ctx, rule.Generated, values, rule.Layout, &memory)
			if err != nil {
				return err
			}
			values[i] = value
		}
	}
	for i, rule := range n.columns {
		if values[i].IsNull && !rule.Layout.Nullable {
			return &model.Error{Code: "23502", Message: fmt.Sprintf("column %d contains null values", rule.Layout.ID)}
		}
		if err := storage.ValidateStoredValue(ctx, values[i], model.TypeLayout{Type: rule.Layout.Type, Children: rule.Layout.Children}); err != nil {
			return err
		}
	}
	return n.checkRow(ctx, values, &memory)
}

func (n *RowNormalizer) checkRow(ctx context.Context, values []model.Value, memory *overlayMemory) error {
	layout := model.TypeLayout{Type: model.Type{ID: 16, Modifier: -1, Storage: model.TypeBoolean}}
	for _, check := range n.checks {
		if err := ctx.Err(); err != nil {
			return err
		}
		value, err := check.Expression.Evaluate(ctx, values, n.expressions)
		if err != nil {
			return fmt.Errorf("evaluate check constraint %q: %w", check.Name, err)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		scratch := *memory
		if err := scratch.values([]model.Value{value}, 0); err != nil {
			return err
		}
		if err := storage.ValidateStoredValue(ctx, value, layout); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return &model.Error{Code: "42804", Message: fmt.Sprintf("invalid result for check constraint %q", check.Name), Cause: err}
		}
		if !value.IsNull && value.IntVal != 0 && value.IntVal != 1 {
			return &model.Error{Code: "42804", Message: fmt.Sprintf("invalid boolean result for check constraint %q", check.Name), Cause: storage.ErrUnnormalizedBatch}
		}
		if !value.IsNull && value.IntVal == 0 {
			return &model.Error{Code: "23514", Message: fmt.Sprintf("row violates check constraint %q", check.Name), Cause: storage.ErrUnnormalizedBatch}
		}
	}
	return ctx.Err()
}

func (n *RowNormalizer) evaluate(ctx context.Context, expression ValueExpression, input []model.Value, column storage.ColumnLayout, memory *overlayMemory) (model.Value, error) {
	if err := ctx.Err(); err != nil {
		return model.Value{}, err
	}
	value, err := expression.Evaluate(ctx, input, n.expressions)
	if err != nil {
		return model.Value{}, fmt.Errorf("evaluate column %d: %w", column.ID, err)
	}
	if err := memory.values([]model.Value{value}, 0); err != nil {
		return model.Value{}, err
	}
	if err := validateNormalizerValue(ctx, value, column); err != nil {
		return model.Value{}, err
	}
	return value, nil
}

func (n *RowNormalizer) constraintDefaults(ctx context.Context, columns []model.ColumnID, runtime model.ExpressionRuntime, maxBytes uint64) ([]model.Value, error) {
	if n == nil || maxBytes == 0 {
		return nil, errTransactionBudget
	}
	memory := overlayMemory{limit: min(maxBytes, n.maxBytes)}
	if err := memory.add(uint64(len(columns)), uint64(unsafe.Sizeof(model.Value{}))); err != nil {
		return nil, err
	}
	values := make([]model.Value, len(columns))
	copy := *n
	copy.expressions = runtime
	for i, id := range columns {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		position := sort.Search(len(copy.columns), func(j int) bool { return copy.columns[j].Layout.ID >= id })
		if position == len(copy.columns) || copy.columns[position].Layout.ID != id || copy.columns[position].Generated != nil {
			return nil, &model.Error{Code: "42703", Message: fmt.Sprintf("unknown default column %d", id), Cause: storage.ErrInvalidMutation}
		}
		rule := copy.columns[position]
		if rule.Default == nil {
			values[i] = model.NullVal(rule.Layout.Type.Storage)
			if err := validateNormalizerValue(ctx, values[i], rule.Layout); err != nil {
				return nil, err
			}
			continue
		}
		value, err := copy.evaluate(ctx, rule.Default, nil, rule.Layout, &memory)
		if err != nil {
			return nil, err
		}
		values[i] = value
	}
	return values, ctx.Err()
}

func validateNormalizerValue(ctx context.Context, value model.Value, column storage.ColumnLayout) error {
	if err := storage.ValidateStoredValue(ctx, value, model.TypeLayout{Type: column.Type, Children: column.Children}); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &model.Error{Code: "42804", Message: fmt.Sprintf("invalid value for column %d", column.ID), Cause: err}
	}
	if value.IsNull && !column.Nullable {
		return &model.Error{Code: "23502", Message: fmt.Sprintf("null value in column %d", column.ID), Cause: storage.ErrUnnormalizedBatch}
	}
	return nil
}
